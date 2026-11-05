package download

import (
	"context"
	"database/sql"
	"errors"
	"hash"
	"os"
	"path/filepath"
	"time"
)

// Keeping a partial download across restarts (see partResume in manager.go). The
// records are in the database, not next to the files: nothing new appears in the
// library for its scans to meet, the commit is durable when it returns, and what
// is being kept can be counted. Only the library's own transfers keep progress; a
// download to the local staging folder starts over.

type partRow struct {
	path, sum string
	offset    int64
}

// canResume: this file is big enough to be worth it and the client can do it.
func canResume(client Client, size int64) bool {
	r, ok := client.(ResumeClient)
	return ok && size >= resumeMinSize && partCheckpointEvery > 0 && r.CanResume()
}

// partLoad finds the progress of this message's file. An edit to another file has
// another identity, so it does not find this one's.
func (l *Library) partLoad(ctx context.Context, item Item) partRow {
	var row partRow
	var size int64
	err := l.reader.QueryRowContext(ctx, `SELECT path,offset,sha256,size FROM tgdl_part_progress WHERE group_id=? AND message_id=? AND identity=?`,
		item.GroupID, item.MessageID, item.Identity.Key()).Scan(&row.path, &row.offset, &row.sum, &size)
	if err != nil || size != item.Identity.Size || !filepath.IsLocal(filepath.FromSlash(row.path)) {
		return partRow{}
	}
	return row
}

func (l *Library) partResumeFor(item Item, path string, row partRow, digest hash.Hash) *partResume {
	return &partResume{offset: row.offset, sum: row.sum, digest: digest,
		beforeSave: partBeforeSave,
		save: func(ctx context.Context, offset int64, sum string) error {
			_, err := l.writer.ExecContext(ctx, `INSERT INTO tgdl_part_progress(group_id,message_id,identity,path,size,offset,sha256,updated_at) VALUES(?,?,?,?,?,?,?,?)
				ON CONFLICT(group_id,message_id,identity) DO UPDATE SET path=excluded.path,size=excluded.size,offset=excluded.offset,sha256=excluded.sha256,updated_at=excluded.updated_at`,
				item.GroupID, item.MessageID, item.Identity.Key(), path, item.Identity.Size, offset, sum, time.Now().UnixMilli())
			return err
		},
		clear: func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_, _ = l.writer.ExecContext(ctx, `DELETE FROM tgdl_part_progress WHERE group_id=? AND message_id=? AND identity=?`, item.GroupID, item.MessageID, item.Identity.Key())
		},
	}
}

// partBeforeSave is for tests: between the sync and the record.
var partBeforeSave func()

// partKept: the part of this published path has progress recorded: recovery leaves
// it for the next attempt.
func (l *Library) partKept(ctx context.Context, path string) (bool, error) {
	var kept bool
	err := l.reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tgdl_part_progress WHERE path=?)`, filepath.ToSlash(path)).Scan(&kept)
	return kept, err
}

// partSweep removes what no download is coming back for: progress whose queue item
// is gone, finished or cancelled, and whose part is missing. It runs in Recover,
// before any transfer.
func (l *Library) partSweep(ctx context.Context, root *os.Root) error {
	rows, err := l.reader.QueryContext(ctx, `SELECT p.group_id,p.message_id,p.identity,p.path,
		EXISTS(SELECT 1 FROM tgdl_work w WHERE w.group_id=p.group_id AND w.message_id=p.message_id AND w.identity=p.identity AND w.status IN ('pending','processing'))
		FROM tgdl_part_progress p`)
	if err != nil {
		return err
	}
	type entry struct {
		group, identity, path string
		message               int64
		wanted                bool
	}
	var all []entry
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.group, &e.message, &e.identity, &e.path, &e.wanted); err != nil {
			rows.Close()
			return err
		}
		all = append(all, e)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, e := range all {
		rel := filepath.FromSlash(e.path)
		part := filepath.Join(filepath.Dir(rel), partName(filepath.Base(rel)))
		if e.wanted && filepath.IsLocal(rel) {
			if _, statErr := root.Stat(part); statErr == nil {
				continue
			}
		}
		if filepath.IsLocal(rel) {
			if err = root.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if _, err = l.writer.ExecContext(ctx, `DELETE FROM tgdl_part_progress WHERE group_id=? AND message_id=? AND identity=?`, e.group, e.message, e.identity); err != nil {
			return err
		}
	}
	return nil
}

// PartsKept counts the partial downloads being kept, and the bytes recorded.
func (l *Library) PartsKept(ctx context.Context) (files int, bytes int64) {
	_ = queryParts(ctx, l.reader, &files, &bytes)
	return files, bytes
}

func queryParts(ctx context.Context, db *sql.DB, files *int, bytes *int64) error {
	return db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(offset),0) FROM tgdl_part_progress`).Scan(files, bytes)
}
