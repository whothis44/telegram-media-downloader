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
// is being kept can be counted. Downloads to the local staging folder keep theirs
// too (from ix.12.3): stage_dir names that folder, and path is then relative to it.
// A part kept in one place is never carried on in another: when staging is turned
// on or off, or its folder changes, that file starts over.

type partRow struct {
	path, sum, stageDir string
	offset              int64
}

// canResume: this file is big enough to be worth it and the client can do it.
func canResume(client Client, size int64) bool {
	r, ok := client.(ResumeClient)
	return ok && size >= resumeMinSize && partCheckpointEvery > 0 && r.CanResume()
}

// partLoad finds the progress of this message's file kept in stageDir (empty:
// the library). An edit to another file has another identity, so it does not
// find this one's. A part kept somewhere else is removed with its record: this
// transfer starts over, and nothing would ever come back for it.
func (l *Library) partLoad(ctx context.Context, item Item, stageDir string) partRow {
	var row partRow
	var size int64
	err := l.reader.QueryRowContext(ctx, `SELECT path,offset,sha256,size,stage_dir FROM tgdl_part_progress WHERE group_id=? AND message_id=? AND identity=?`,
		item.GroupID, item.MessageID, item.Identity.Key()).Scan(&row.path, &row.offset, &row.sum, &size, &row.stageDir)
	if err != nil {
		return partRow{}
	}
	if size != item.Identity.Size || !filepath.IsLocal(filepath.FromSlash(row.path)) || row.stageDir != stageDir {
		l.partForget(ctx, item, row)
		return partRow{}
	}
	return row
}

// partOfAttempt: an earlier attempt kept a part of this item's file, in the
// library or a staging folder (ix.12.13: such a download is not checked
// again, it carries on).
func (l *Library) partOfAttempt(ctx context.Context, item Item) bool {
	var n int
	err := l.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM tgdl_part_progress WHERE group_id=? AND message_id=? AND identity=?`,
		item.GroupID, item.MessageID, item.Identity.Key()).Scan(&n)
	return err == nil && n > 0
}

// partForget removes a kept part and its record.
func (l *Library) partForget(ctx context.Context, item Item, row partRow) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if rel := filepath.FromSlash(row.path); filepath.IsLocal(rel) {
		dir := row.stageDir
		if dir == "" {
			dir = l.root
		}
		if root, err := os.OpenRoot(dir); err == nil {
			_ = root.Remove(filepath.Join(filepath.Dir(rel), partName(filepath.Base(rel))))
			root.Close()
		}
	}
	_, _ = l.writer.ExecContext(ctx, `DELETE FROM tgdl_part_progress WHERE group_id=? AND message_id=? AND identity=?`, item.GroupID, item.MessageID, item.Identity.Key())
}

func (l *Library) partResumeFor(item Item, path, stageDir string, row partRow, digest hash.Hash) *partResume {
	return &partResume{offset: row.offset, sum: row.sum, digest: digest,
		beforeSave: partBeforeSave,
		save: func(ctx context.Context, offset int64, sum string) error {
			_, err := l.writer.ExecContext(ctx, `INSERT INTO tgdl_part_progress(group_id,message_id,identity,path,size,offset,sha256,updated_at,stage_dir) VALUES(?,?,?,?,?,?,?,?,?)
				ON CONFLICT(group_id,message_id,identity) DO UPDATE SET path=excluded.path,size=excluded.size,offset=excluded.offset,sha256=excluded.sha256,updated_at=excluded.updated_at,stage_dir=excluded.stage_dir`,
				item.GroupID, item.MessageID, item.Identity.Key(), path, item.Identity.Size, offset, sum, time.Now().UnixMilli(), stageDir)
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

// partKept: the part of this published path in the library has progress
// recorded: recovery leaves it for the next attempt.
func (l *Library) partKept(ctx context.Context, path string) (bool, error) {
	var kept bool
	err := l.reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tgdl_part_progress WHERE path=? AND stage_dir='')`, filepath.ToSlash(path)).Scan(&kept)
	return kept, err
}

// partSweep removes what no download is coming back for: progress whose queue item
// is gone, finished or cancelled, and whose part is missing. It runs in Recover,
// before any transfer.
func (l *Library) partSweep(ctx context.Context, root *os.Root) error {
	rows, err := l.reader.QueryContext(ctx, `SELECT p.group_id,p.message_id,p.identity,p.path,p.stage_dir,
		EXISTS(SELECT 1 FROM tgdl_work w WHERE w.group_id=p.group_id AND w.message_id=p.message_id AND w.identity=p.identity AND w.status IN ('pending','processing'))
		FROM tgdl_part_progress p`)
	if err != nil {
		return err
	}
	type entry struct {
		group, identity, path, stageDir string
		message                         int64
		wanted                          bool
	}
	var all []entry
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.group, &e.message, &e.identity, &e.path, &e.stageDir, &e.wanted); err != nil {
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
		if e.stageDir != "" {
			if keep, err := l.partSweepStaged(e.stageDir, part, rel, e.wanted); err != nil || keep {
				continue // a staging folder not there today keeps its record
			}
		} else {
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
		}
		if _, err = l.writer.ExecContext(ctx, `DELETE FROM tgdl_part_progress WHERE group_id=? AND message_id=? AND identity=?`, e.group, e.message, e.identity); err != nil {
			return err
		}
	}
	return nil
}

// partSweepStaged is partSweep for a part in the staging folder dir: keep says it
// stays. A folder that cannot be opened (a disk not there today) gives an error,
// and the record stays: the part may still be on it.
func (l *Library) partSweepStaged(dir, part, rel string, wanted bool) (keep bool, err error) {
	stage, err := os.OpenRoot(dir)
	if err != nil {
		return false, err
	}
	defer stage.Close()
	if !filepath.IsLocal(rel) {
		return false, nil
	}
	if wanted {
		if _, statErr := stage.Stat(part); statErr == nil {
			return true, nil
		}
	}
	if err := stage.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return false, nil
}

// PartsKept counts the partial downloads being kept, and the bytes recorded.
func (l *Library) PartsKept(ctx context.Context) (files int, bytes int64) {
	_ = queryParts(ctx, l.reader, &files, &bytes)
	return files, bytes
}

func queryParts(ctx context.Context, db *sql.DB, files *int, bytes *int64) error {
	return db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(offset),0) FROM tgdl_part_progress`).Scan(files, bytes)
}
