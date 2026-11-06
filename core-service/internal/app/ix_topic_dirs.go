package app

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/download"
)

// Forum topic folders (ix.12.10, the user's choice on 11-06). A file of a
// forum group's topic goes to <group>/<topic>/<type>/ instead of
// <group>/<type>/; the General topic (and every chat without topics) stays
// in <group>/<type>/. A topic's folder keeps the name the topic had when
// its first file was saved (ix_topic_dirs): a topic renamed later moves
// nothing. Two topics of one group with the same name get the topic's
// number added to the second. A topic whose name the archive does not know
// yet is "Topic <number>".
//
// Files downloaded before go to their topic's folder with an Organise plan
// ("Plan: move forum files into topic folders"): listed in full first, each
// file's SHA-256 checked before it moves, the catalogue and the archive's
// other records of the file pointed at its new place, and undone in one
// click. It needs no account: it works from the archive's messages
// (ix_messages.topic_id) and topic names (ix_topics).

const ixTopicDirsSchema = `
CREATE TABLE IF NOT EXISTS ix_topic_dirs ( -- the folder of each forum topic, named at its first file
	group_id TEXT    NOT NULL,
	topic_id INTEGER NOT NULL,
	dir      TEXT    NOT NULL,
	at       INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (group_id, topic_id)
);`

// ixTopicGeneral: a forum's General topic has number 1; messages in it
// carry none (0).
func ixTopicGeneral(topic int) bool { return topic <= 1 }

// ixTopicDir is the folder of a forum topic's files inside the group's
// folder ("" for General or no topic), named once.
func (a *App) ixTopicDir(ctx context.Context, group string, topic int) (string, error) {
	if ixTopicGeneral(topic) || group == "" {
		return "", nil
	}
	var dir string
	err := a.db.Reader.QueryRowContext(ctx, `SELECT dir FROM ix_topic_dirs WHERE group_id=? AND topic_id=?`, group, topic).Scan(&dir)
	if err == nil {
		return dir, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var title string
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT title FROM ix_topics WHERE group_id=? AND topic_id=?`, group, topic).Scan(&title)
	name := "Topic " + strconv.Itoa(topic)
	if t := strings.TrimSpace(title); t != "" {
		name = download.SafeComponent(t)
	}
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	// Another topic of the group has this folder already: add the number.
	var taken int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_topic_dirs WHERE group_id=? AND lower(dir)=lower(?) AND topic_id<>?`, group, name, topic).Scan(&taken); err != nil {
		return "", err
	}
	if taken > 0 {
		name = download.SafeComponent(name + " (" + strconv.Itoa(topic) + ")")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ix_topic_dirs(group_id,topic_id,dir,at) VALUES(?,?,?,?) ON CONFLICT(group_id,topic_id) DO NOTHING`, group, topic, name, time.Now().UnixMilli()); err != nil {
		return "", err
	}
	if err := tx.QueryRowContext(ctx, `SELECT dir FROM ix_topic_dirs WHERE group_id=? AND topic_id=?`, group, topic).Scan(&dir); err != nil {
		return "", err
	}
	return dir, tx.Commit()
}

// ixTypeFolders are the library's type folders (download.destinationParts).
var ixTypeFolders = map[string]bool{"images": true, "videos": true, "documents": true, "audio": true, "stickers": true, "gifs": true}

// ixTopicLegacyDir is, for a topic folder's type folder
// (<group>/<topic>/<type>), the group's own type folder (<group>/<type>),
// where its files went before: a copy moved in by hand may still be there
// (ix_uncatalogued.go). "" for any other folder.
func ixTopicLegacyDir(dir string) string {
	parts := strings.Split(dir, "/")
	if len(parts) != 3 || !ixTypeFolders[parts[2]] {
		return ""
	}
	return parts[0] + "/" + parts[2]
}

// ixTopicPlanKind names the Organise plan.
const ixTopicPlanKind = "topic-folders"

// ixPlanTopicFolders plans moving the saved files of forum topics from
// <group>/<type>/ to <group>/<topic>/<type>/. A file shared by messages of
// different topics or chats, a file without a checksum, and a file not in
// the usual place are left where they are.
func (a *App) ixPlanTopicFolders(ctx context.Context) ([]ixMoveOp, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT d.file_path, lower(COALESCE(d.file_hash,'')), COALESCE(d.file_size,0), d.group_id, COALESCE(m.topic_id,0)
		FROM downloads d LEFT JOIN ix_messages m ON m.group_id=d.group_id AND m.message_id=d.message_id
		WHERE d.file_path IS NOT NULL AND d.file_path<>'' AND (d.status IS NULL OR d.status='completed')
		AND d.group_id IN (SELECT group_id FROM ix_topics)`)
	if err != nil {
		return nil, err
	}
	type file struct {
		rel, sum, group string
		size            int64
		topic           int
		mixed           bool
	}
	byPath := map[string]*file{}
	var order []string
	for rows.Next() {
		var stored, sum, group string
		var size int64
		var topic int
		if rows.Scan(&stored, &sum, &size, &group, &topic) != nil {
			continue
		}
		rel := ixNormPath(stored)
		if rel == "" {
			continue
		}
		k := ixPathKey(rel)
		f := byPath[k]
		if f == nil {
			f = &file{rel: rel, sum: sum, group: group, size: size, topic: topic}
			byPath[k] = f
			order = append(order, k)
			continue
		}
		if f.group != group || f.topic != topic {
			f.mixed = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var ops []ixMoveOp
	for _, k := range order {
		f := byPath[k]
		if f.mixed || ixTopicGeneral(f.topic) || len(f.sum) != 64 || f.size <= 0 {
			continue
		}
		parts := strings.Split(f.rel, "/")
		if len(parts) != 3 || !ixTypeFolders[parts[1]] {
			continue // already in a topic folder, or not where the library puts files
		}
		dir, err := a.ixTopicDir(ctx, f.group, f.topic)
		if err != nil {
			return nil, err
		}
		if dir == "" {
			continue
		}
		dst := parts[0] + "/" + dir + "/" + parts[1] + "/" + parts[2]
		ops = append(ops, ixMoveOp{Op: "move", Src: f.rel, Dst: dst, SHA256: f.sum, Size: f.size, Repoint: dst})
		if len(ops) >= ixMaxPlanOps {
			break
		}
	}
	return ops, nil
}
