package app

// The dashboard's queue tells the queue items the archive set aside itself
// from those a person paused. Both are tgdl_work.paused=1; the archive's
// own are, by queue item:
//
//   - copy:     a probable copy of a saved (or queued) file, held (ix_held)
//   - lowq:     an original waiting to replace the lower quality saved
//     first (ix_lowq, state 'saved')
//   - deadline: a larger file put back so private chats and smaller files
//     go first (ix_deferred, released_at=0)
//
// A person resuming one of these makes it theirs: the archive never sets it
// aside again. "Resume all" therefore leaves them waiting unless asked to
// include them.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	queueHoldCopy     = "copy"
	queueHoldLowq     = "lowq"
	queueHoldDeadline = "deadline"
)

// queueHoldReasonSQL is the archive's reason for holding the paused queue
// item ref (empty when a person paused it): one lookup by queue item in each
// table (ix_held and ix_deferred by primary key, ix_lowq by ix_lowq_work).
// The order follows the order they wait in (ix_order.go).
func queueHoldReasonSQL(ref string) string {
	return `CASE
		WHEN EXISTS (SELECT 1 FROM ix_held h WHERE h.work_id=` + ref + `.id AND h.released_at=0 AND h.reason<>'') THEN '` + queueHoldCopy + `'
		WHEN EXISTS (SELECT 1 FROM ix_lowq q WHERE q.work_id=` + ref + `.id AND q.state='saved') THEN '` + queueHoldLowq + `'
		WHEN EXISTS (SELECT 1 FROM ix_deferred x WHERE x.work_id=` + ref + `.id AND x.released_at=0) THEN '` + queueHoldDeadline + `'
		ELSE '' END`
}

// queuePausedLive matches the queue items that are paused and still to
// download (the same rows as the engine's PausedCount).
const queuePausedLive = `paused<>0 AND status IN ('pending','processing')`

// queueAddHolds marks the snapshot's paused items the archive set aside
// (archiveHold: its reason) and counts both kinds over the whole queue, not
// only the part the snapshot lists. On a read error the snapshot stays as
// the engine made it.
func (a *App) queueAddHolds(ctx context.Context, snapshot map[string]any) {
	want := map[string]map[string]any{}
	for _, list := range []string{"active", "queued"} {
		items, _ := snapshot[list].([]map[string]any)
		for _, item := range items {
			if item["status"] == "paused" {
				if key, _ := item["key"].(string); key != "" {
					want[key] = item
				}
			}
		}
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT w.group_id, w.message_id, `+queueHoldReasonSQL("w")+`
		FROM tgdl_work w WHERE `+queuePausedLive)
	if err != nil {
		return
	}
	defer rows.Close()
	var held, person int
	for rows.Next() {
		var group, reason string
		var msg int64
		if rows.Scan(&group, &msg, &reason) != nil {
			return
		}
		if reason == "" {
			person++
			continue
		}
		held++
		if item := want[group+"_"+strconv.FormatInt(msg, 10)]; item != nil {
			item["archiveHold"] = reason
		}
	}
	if rows.Err() != nil {
		return
	}
	snapshot["archiveHeldCount"] = held
	snapshot["personPausedCount"] = person
	// ix.12.14: the files ready to download in the whole queue; the snapshot
	// lists the next 1,000 of them, so the page can say "921 of 4,832".
	var ready int
	if a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM tgdl_work WHERE status='pending' AND paused=0`).Scan(&ready) == nil {
		snapshot["queuedCount"] = ready
	}
}

// queueKeyParts splits a queue key (group_message) at its last underscore.
func queueKeyParts(key string) (string, int64, bool) {
	i := strings.LastIndexByte(key, '_')
	if i <= 0 {
		return "", 0, false
	}
	msg, err := strconv.ParseInt(key[i+1:], 10, 64)
	return key[:i], msg, err == nil
}

// queueHoldReason is the archive's reason for holding the queue item key,
// empty when it is not paused by the archive.
func (a *App) queueHoldReason(ctx context.Context, key string) (string, error) {
	group, msg, ok := queueKeyParts(key)
	if !ok {
		return "", nil
	}
	var reason string
	err := a.db.Reader.QueryRowContext(ctx, `SELECT `+queueHoldReasonSQL("w")+` FROM tgdl_work w
		WHERE w.group_id=? AND w.message_id=? AND w.paused<>0 AND w.status IN ('pending','processing')`, group, msg).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return reason, err
}

// queueResumeAllKeepHeld lifts the queue-wide pause and every pause a
// person made, and leaves the items the archive set aside waiting. The
// workers look for work every second, so nothing needs waking.
func (a *App) queueResumeAllKeepHeld(ctx context.Context) (resumed int64, kept int, err error) {
	if err = a.monitor.RequireRunning(); err != nil {
		return 0, 0, err
	}
	return a.queueLiftPausesKeepHeld(ctx)
}

// queueLiftPausesKeepHeld is queueResumeAllKeepHeld's change to the queue.
func (a *App) queueLiftPausesKeepHeld(ctx context.Context) (resumed int64, kept int, err error) {
	now := time.Now().UnixMilli()
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE tgdl_queue_state SET paused=0,updated_at=? WHERE id=1`, now); err != nil {
		return 0, 0, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE tgdl_work SET paused=0,updated_at=? WHERE `+queuePausedLive+`
		AND `+queueHoldReasonSQL("tgdl_work")+`=''`, now)
	if err != nil {
		return 0, 0, err
	}
	if resumed, err = res.RowsAffected(); err != nil {
		return 0, 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM tgdl_work WHERE `+queuePausedLive).Scan(&kept)
	return resumed, kept, nil
}

// decodeOptionalBody reads a JSON body that may be left out.
func decodeOptionalBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid JSON body")
		return false
	}
	if strings.TrimSpace(string(body)) == "" {
		return true
	}
	if err = json.Unmarshal(body, dst); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Invalid JSON body")
		return false
	}
	return true
}
