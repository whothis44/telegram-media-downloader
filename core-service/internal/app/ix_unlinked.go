package app

import (
	"context"
	"fmt"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
)

// Queue rows marked done with nothing saved (ix.12.9, once).
//
// Before ix.10.2, with download.skipDuplicates on, a message whose file was
// judged a copy of a saved one (an exact re-upload, or the same bytes found
// after downloading) finished its queue row as completed but got no
// catalogue entry and no link to the saved file (ix_msg_link came with
// ix.10.2). The judgement also had a known flaw for some files: two
// same-size files matching at the three places it looked counted as the
// same, and then one was never downloaded. On one archive 11,085 such rows
// were left (93 GB, 55 chats), all from the day and a half before ix.10.2.
//
// They looked like files still needed (the media browser and Priorities
// counted them so, and the deadline figures too), but nothing queued them:
// a rescan leaves a completed row alone, so "Add them to the rescan" could
// not help. Each such row goes back in the queue now, with the message as
// recorded in the archive:
//   - held as a probable copy when a saved file has the same size (every one
//     on that archive had one): at the back of the queue with the other
//     probable copies, where the copy checks look at it, and downloaded
//     after everything else unless one confirms it; a copy found while
//     downloading is linked, not kept twice;
//   - as an ordinary download otherwise.
//
// Rows of messages deleted on Telegram, or not recorded in the archive, are
// left as they are (there is nothing to download them from). Rows whose
// file is saved or linked in any way, held, or waiting as a lower quality's
// original are not touched. It runs once (a kv flag), in batches.

const (
	ixUnlinkedFlag  = "ix_unlinked_repair_v1"
	ixUnlinkedBatch = 500
	ixUnlinkedHow   = "unlinked-copy"
)

// ixUnlinkedWhere selects the rows (tgdl_work w, ix_messages m).
const ixUnlinkedWhere = `w.status='completed' AND w.origin IN ('history','live') AND w.ttl_seconds=0
	AND length(m.tl)>0 AND m.deleted_at=0 AND m.media_key<>''
	AND NOT EXISTS (SELECT 1 FROM downloads d WHERE d.group_id=w.group_id AND d.message_id=w.message_id)
	AND NOT EXISTS (SELECT 1 FROM downloads d WHERE d.telegram_media_kind=substr(m.media_key,1,instr(m.media_key,':')-1)
		AND d.telegram_media_id=substr(substr(m.media_key,instr(m.media_key,':')+1),1,instr(substr(m.media_key,instr(m.media_key,':')+1),':')-1))
	AND NOT EXISTS (SELECT 1 FROM ix_subst s WHERE s.group_id=w.group_id AND s.message_id=w.message_id)
	AND NOT EXISTS (SELECT 1 FROM ix_msg_link l WHERE l.group_id=w.group_id AND l.message_id=w.message_id)
	AND NOT EXISTS (SELECT 1 FROM ix_held h WHERE h.work_id=w.id)
	AND NOT EXISTS (SELECT 1 FROM ix_lowq q WHERE q.group_id=w.group_id AND q.message_id=w.message_id)`

type ixUnlinkedResult struct {
	Held, Queued int
}

// ixRepairUnlinked runs the repair once.
func (a *App) ixRepairUnlinked(ctx context.Context) (ixUnlinkedResult, error) {
	var res ixUnlinkedResult
	var done int
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM kv WHERE key=?`, ixUnlinkedFlag).Scan(&done); err != nil || done > 0 {
		return res, err
	}
	type row struct {
		id, size int64
		group    string
		msg      int64
		tl       []byte
	}
	var lastID int64
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		rows, err := a.db.Reader.QueryContext(ctx, `SELECT w.id, w.group_id, w.message_id, w.file_size, m.tl FROM tgdl_work w
			JOIN ix_messages m ON m.group_id=w.group_id AND m.message_id=w.message_id
			WHERE w.id>? AND `+ixUnlinkedWhere+` ORDER BY w.id LIMIT ?`, lastID, ixUnlinkedBatch)
		if err != nil {
			return res, err
		}
		var batch []row
		for rows.Next() {
			var r row
			if rows.Scan(&r.id, &r.group, &r.msg, &r.size, &r.tl) == nil {
				batch = append(batch, r)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, err
		}
		if len(batch) == 0 {
			break
		}
		lastID = batch[len(batch)-1].id
		// The saved files of the same size, read before the write.
		type like struct {
			path, key string
			n         int
		}
		likes := make([]like, len(batch))
		for i, r := range batch {
			q, err := a.db.Reader.QueryContext(ctx, `SELECT file_path, COALESCE(file_hash,'') FROM downloads
				WHERE file_size=? AND status='completed' AND file_path IS NOT NULL AND file_path<>'' ORDER BY id LIMIT 2`, r.size)
			if err != nil {
				return res, err
			}
			for q.Next() {
				var path, hash string
				if q.Scan(&path, &hash) == nil {
					if likes[i].n == 0 {
						likes[i].path, likes[i].key = path, ixFileKey(hash, path)
					}
					likes[i].n++
				}
			}
			q.Close()
		}
		tx, err := a.db.Writer.BeginTx(ctx, nil)
		if err != nil {
			return res, err
		}
		now := time.Now().UnixMilli()
		for i, r := range batch {
			l := likes[i]
			paused := 0
			if l.n > 0 {
				paused = 1
			}
			out, err := tx.ExecContext(ctx, `UPDATE tgdl_work SET status='pending', paused=?, body=?, generation=generation+1, claim_generation=NULL,
				attempts=0, retry_at=0, error=NULL, refresh_required=0, sched_order=?, updated_at=? WHERE id=? AND status='completed'`,
				paused, r.tl, engine.UnrankedOrder, now, r.id)
			if err != nil {
				tx.Rollback()
				return res, err
			}
			if n, _ := out.RowsAffected(); n == 0 {
				continue
			}
			if l.n == 0 {
				res.Queued++
				continue
			}
			reason := "probably a copy of a saved file (the same size): marked done before ix.10.2 without a record of which"
			if l.n > 1 {
				reason = "probably a copy of a saved file (more than one saved file has its size): marked done before ix.10.2 without a record of which"
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO ix_held(work_id,group_id,message_id,like_path,like_key,reason,held_at,released_at) VALUES(?,?,?,?,?,?,?,0)`,
				r.id, r.group, r.msg, l.path, l.key, reason, now); err != nil {
				tx.Rollback()
				return res, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO ix_copy_hold(work_id,how,of_work,sim,confirm,confirm_note,checked_at) VALUES(?,?,0,0,'','',0)`, r.id, ixUnlinkedHow); err != nil {
				tx.Rollback()
				return res, err
			}
			res.Held++
		}
		if err := tx.Commit(); err != nil {
			return res, err
		}
	}
	if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO NOTHING`,
		ixUnlinkedFlag, fmt.Sprintf("held %d, queued %d", res.Held, res.Queued), time.Now().UnixMilli()); err != nil {
		return res, err
	}
	if res.Held+res.Queued > 0 {
		a.ixLog("queue rows marked done with nothing saved (before ix.10.2): %d back in the queue as probable copies (a saved file has the same size), %d as ordinary downloads", res.Held, res.Queued)
		a.ixSchedWake()
	}
	return res, nil
}

// ixUnlinkedOnce runs the repair from the schedule step; an error is logged
// and tried again at the next step.
func (a *App) ixUnlinkedOnce(ctx context.Context) {
	if _, err := a.ixRepairUnlinked(ctx); err != nil && ctx.Err() == nil {
		a.ixLog("repairing queue rows marked done with nothing saved: %v", err)
	}
}
