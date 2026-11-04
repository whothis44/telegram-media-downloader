package app

import (
	"context"
	"strings"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/download"
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
`

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
		item.GroupID, item.MessageID, record.Dedup, record.Path, record.SHA256, item.Identity.Size, item.Identity.Key(), item.Name, item.Type, time.Now().UnixMilli())
}

// ixLinkedFrom joins each linked message without a catalogue entry of its
// own (l) to the saved file's entry (d): found by its SHA-256, or by its
// path when the hash is not known.
const ixLinkedFrom = `FROM ix_msg_link l JOIN downloads d ON d.id = COALESCE(
			(SELECT o.id FROM downloads o WHERE l.sha256<>'' AND o.file_hash=l.sha256 AND o.status='completed' AND o.file_path IS NOT NULL AND o.file_path<>'' ORDER BY o.id LIMIT 1),
			(SELECT o.id FROM downloads o WHERE o.file_path=l.of_path AND o.status='completed' ORDER BY o.id LIMIT 1))
		WHERE l.message_id>0 AND d.file_type IS NOT 'stories'
			AND NOT EXISTS (SELECT 1 FROM downloads x WHERE x.group_id=l.group_id AND x.message_id=l.message_id AND x.file_path IS NOT NULL AND x.file_path<>'' AND (x.status IS NULL OR x.status='completed'))`

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
			d.id, d.file_path, COALESCE(d.file_hash,''), COALESCE(d.file_size,0), COALESCE(d.file_type,''), COALESCE(CAST(d.created_at AS TEXT),''),
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
	err := a.db.Reader.QueryRowContext(ctx, `SELECT d.file_path `+ixLinkedFrom+` AND l.tg_file=? AND NOT (l.group_id=? AND l.message_id=?) LIMIT 1`,
		item.Identity.Key(), item.GroupID, item.MessageID).Scan(&path)
	return path, err == nil && path != ""
}
