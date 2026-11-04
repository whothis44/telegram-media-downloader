package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/download"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// Every message stays tied to its file, even when only the duplicate bytes
// were dropped.
//
// A message whose file the library already held (the same Telegram file,
// an exact re-upload, or the same bytes found after downloading) shares the
// saved file. Normally it gets its own catalogue entry pointing at it, but
// with download.skipDuplicates it gets none, and the archive then took the
// post's file for not downloaded. ix_msg_link keeps, for every such
// message, the saved file (path and SHA-256), the message's own Telegram
// file and how it was matched. The media browser, posts, albums and sets,
// and the data export read it for messages without an entry of their own.
//
// It also says which completed downloads cost no bytes (ixSkippedBefore):
// the same Telegram file or an exact re-upload. Copies found after
// downloading were downloaded and count as such.

const ixSchemaLinks = `
CREATE TABLE IF NOT EXISTS ix_msg_link ( -- messages sharing a saved file (ix_links.go)
	group_id   TEXT    NOT NULL,
	message_id INTEGER NOT NULL,
	how        TEXT    NOT NULL DEFAULT '', -- telegram-file, exact-reupload, after-download
	of_path    TEXT    NOT NULL DEFAULT '', -- the saved file
	sha256     TEXT    NOT NULL DEFAULT '',
	size       INTEGER NOT NULL DEFAULT 0,
	tg_file    TEXT    NOT NULL DEFAULT '', -- the message's own Telegram file, kind:id:size
	name       TEXT    NOT NULL DEFAULT '',
	type       TEXT    NOT NULL DEFAULT '',
	at         INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (group_id, message_id)
);
CREATE INDEX IF NOT EXISTS ix_msg_link_tg ON ix_msg_link(tg_file);
-- ix.10.2 test builds wrote the identity with NUL separators.
UPDATE ix_msg_link SET tg_file=replace(tg_file, char(0), ':') WHERE instr(tg_file, char(0))>0;
`

// ixLinkedKeptPaths are the files linked messages share (by path, or by
// SHA-256 through the verified-media cache): drainFileCleanup keeps them
// when the original's entry is deleted, purged or replaced by an edit.
func ixLinkedKeptPaths(ctx context.Context, tx *sql.Tx) []string {
	rows, err := tx.QueryContext(ctx, `SELECT of_path FROM ix_msg_link WHERE of_path<>''
		UNION SELECT v.path FROM tgdl_verified_media v WHERE v.sha256 IN (SELECT sha256 FROM ix_msg_link WHERE sha256<>'')`)
	if err != nil {
		return nil // no archive tables (the original application's database)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil && p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ixTgFile is a Telegram file as the export names it: kind:id:size.
func ixTgFile(id telegram.MediaIdentity) string {
	return id.Kind + ":" + id.ID + ":" + strconv.FormatInt(id.Size, 10)
}

// ixFetchForwardsToo, inside "Fetch the real files": the other messages
// matched by Telegram identity to the substituted file (identity, a
// MediaIdentity key) lose that match and are queued again by the next
// files pass. Only catalogue rows and links go; the saved file stays.
func ixFetchForwardsToo(ctx context.Context, tx *sql.Tx, identity, path string) error {
	parts := strings.Split(identity, "\x00")
	if len(parts) != 3 {
		return nil
	}
	size, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return nil
	}
	tgFile := ixTgFile(telegram.MediaIdentity{Kind: parts[0], ID: parts[1], Size: size})
	rows, err := tx.QueryContext(ctx, `SELECT group_id, message_id FROM downloads WHERE telegram_media_kind=? AND telegram_media_id=? AND telegram_media_size=? AND file_path=?
		UNION SELECT group_id, message_id FROM ix_msg_link WHERE tg_file=?`, parts[0], parts[1], size, path, tgFile)
	if err != nil {
		return err
	}
	type msg struct {
		group string
		id    int64
	}
	var msgs []msg
	for rows.Next() {
		var m msg
		if err := rows.Scan(&m.group, &m.id); err != nil {
			rows.Close()
			return err
		}
		msgs = append(msgs, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, m := range msgs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM downloads WHERE group_id=? AND message_id=? AND telegram_media_kind=? AND telegram_media_id=? AND file_path=?`, m.group, m.id, parts[0], parts[1], path); err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM ix_msg_link WHERE group_id=? AND message_id=?`,
			`DELETE FROM ix_subst WHERE group_id=? AND message_id=?`,
			`DELETE FROM tgdl_work WHERE group_id=? AND message_id=? AND status IN ('completed','skipped','failed')`,
		} {
			if _, err := tx.ExecContext(ctx, q, m.group, m.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// ixSkippedBefore is true for a completed queue item w whose bytes were
// not downloaded (it was covered by a saved file before downloading).
const ixSkippedBefore = `(EXISTS (SELECT 1 FROM ix_subst s WHERE s.group_id=w.group_id AND s.message_id=w.message_id)
	OR EXISTS (SELECT 1 FROM ix_msg_link k WHERE k.group_id=w.group_id AND k.message_id=w.message_id AND k.how IN ('telegram-file','exact-reupload')))`

// ixSubstValid is true for an exact re-upload s (ix_subst) that still
// points at the saved file: by its own catalogue entry, or, without one
// (download.skipDuplicates), by its link.
const ixSubstValid = `(EXISTS (SELECT 1 FROM downloads d WHERE d.group_id=s.group_id AND d.message_id=s.message_id AND d.file_path=s.of_path)
	OR (EXISTS (SELECT 1 FROM ix_msg_link k WHERE k.group_id=s.group_id AND k.message_id=s.message_id AND k.how='exact-reupload')
		AND NOT EXISTS (SELECT 1 FROM downloads x WHERE x.group_id=s.group_id AND x.message_id=s.message_id)))`

// ixNoteLink records (or, after a download of its own, forgets) the saved
// file a message shares.
func (a *App) ixNoteLink(ctx context.Context, item download.Item, record download.Record) {
	ctx = context.WithoutCancel(ctx)
	if !record.Reused || record.Dedup == "" || record.Path == "" {
		if !record.Reused {
			_, _ = a.db.Writer.ExecContext(ctx, `DELETE FROM ix_msg_link WHERE group_id=? AND message_id=?`, item.GroupID, item.MessageID)
		}
		return
	}
	_, _ = a.db.Writer.ExecContext(ctx, `INSERT INTO ix_msg_link(group_id,message_id,how,of_path,sha256,size,tg_file,name,type,at) VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(group_id,message_id) DO UPDATE SET how=excluded.how, of_path=excluded.of_path, sha256=excluded.sha256, size=excluded.size,
		tg_file=excluded.tg_file, name=excluded.name, type=excluded.type, at=excluded.at`,
		item.GroupID, item.MessageID, record.Dedup, record.Path, record.SHA256, item.Identity.Size, ixTgFile(item.Identity), item.Name, item.Type, time.Now().UnixMilli())
}

// ixLinkedFrom joins each linked message without a catalogue entry of its
// own (l) to the saved file's entry (d): found by its SHA-256, or by its
// path when the hash is not known. d is NULL when no entry is left (the
// original was deleted or purged): the file is then kept for the link
// (drainFileCleanup) and named by it (ixLinkedPath, ixLinkedHash).
const ixLinkedFrom = `FROM ix_msg_link l LEFT JOIN downloads d ON d.id = COALESCE(
			(SELECT o.id FROM downloads o WHERE l.sha256<>'' AND o.file_hash=l.sha256 AND o.status='completed' AND o.file_path IS NOT NULL AND o.file_path<>'' ORDER BY o.id LIMIT 1),
			(SELECT o.id FROM downloads o WHERE o.file_path=l.of_path AND o.status='completed' ORDER BY o.id LIMIT 1))
		WHERE l.message_id>0 AND d.file_type IS NOT 'stories'
			AND NOT EXISTS (SELECT 1 FROM downloads x WHERE x.group_id=l.group_id AND x.message_id=l.message_id AND x.file_path IS NOT NULL AND x.file_path<>'' AND (x.status IS NULL OR x.status='completed'))`

// The linked file's path and hash, with or without an entry (ixLinkedFrom).
const (
	ixLinkedPath = `COALESCE(d.file_path, l.of_path)`
	ixLinkedHash = `COALESCE(d.file_hash, l.sha256, '')`
)

// ixLinked is a message without a catalogue entry of its own, with the
// saved file it shares.
type ixLinked struct {
	Group  string
	Msg    int
	How    string
	Name   string // the message's own file name
	Type   string
	TgFile string // the message's own Telegram file
	// The saved file's catalogue entry.
	SavedID  int64
	Path     string
	Hash     string
	Size     int64
	FileType string
	Created  string
	Mime     string
	W, H     int
	DurMs    int64
	Thumb    []byte
}

// ixLinkedFiles lists the linked messages of a chat ("" for all) that have
// no catalogue entry of their own and whose saved file is still catalogued.
func (a *App) ixLinkedFiles(ctx context.Context, groupID string) ([]ixLinked, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT l.group_id, l.message_id, l.how, l.name, l.type, l.tg_file,
			COALESCE(d.id,0), `+ixLinkedPath+`, `+ixLinkedHash+`, COALESCE(d.file_size,l.size), COALESCE(d.file_type,l.type), COALESCE(CAST(d.created_at AS TEXT),''),
			COALESCE(d.telegram_mime,''), COALESCE(d.media_width,0), COALESCE(d.media_height,0), COALESCE(d.media_duration_ms,0), d.telegram_thumb
		`+ixLinkedFrom+` AND (?='' OR l.group_id=?) ORDER BY l.group_id, l.message_id`, groupID, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ixLinked
	for rows.Next() {
		var l ixLinked
		if err := rows.Scan(&l.Group, &l.Msg, &l.How, &l.Name, &l.Type, &l.TgFile, &l.SavedID, &l.Path, &l.Hash, &l.Size, &l.FileType, &l.Created, &l.Mime, &l.W, &l.H, &l.DurMs, &l.Thumb); err != nil {
			return nil, err
		}
		if l.Path = ixNormPath(l.Path); l.Path == "" {
			continue
		}
		if l.SavedID == 0 {
			// No entry is left for the file: it counts only while it is there.
			if info, err := os.Stat(filepath.Join(a.downloadsDir, filepath.FromSlash(l.Path))); err != nil || !info.Mode().IsRegular() {
				continue
			}
		}
		if l.Name == "" {
			l.Name = l.Path[strings.LastIndexByte(l.Path, '/')+1:]
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ixLinkedIdentity: the saved file of a linked message with this item's
// Telegram file, when another message has it.
func (a *App) ixLinkedIdentity(ctx context.Context, item download.Item) (string, bool) {
	var path string
	// The library catalogues on an entry's file, so only links with one count.
	err := a.db.Reader.QueryRowContext(ctx, `SELECT d.file_path `+ixLinkedFrom+` AND d.id IS NOT NULL AND l.tg_file=? AND NOT (l.group_id=? AND l.message_id=?) LIMIT 1`,
		ixTgFile(item.Identity), item.GroupID, item.MessageID).Scan(&path)
	return path, err == nil && path != ""
}
