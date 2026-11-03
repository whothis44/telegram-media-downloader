package app

import (
	"context"
	"database/sql"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"net/http"
	"strings"
	"time"
)

// Downloads, seen from the archive: what is still missing, whether files
// are arriving, and failed downloads tried again later on their own.
//
// The download engine already retries a failed file a few times within
// minutes. Automatic retry here is the slower second chance: failed files
// are put back in the queue every few hours (a Telegram outage or a NAS
// hiccup usually passes), and given up after ixRetryRounds such rounds so
// a file that can never be fetched does not cost requests for ever.

const (
	ixRetryEvery  = 30 * time.Minute // how often failed downloads are looked at
	ixRetryAfter  = 3 * time.Hour    // a failure must be this old to be retried
	ixRetryRounds = 5                // automatic rounds before giving up on a file
	ixRetryBatch  = 500              // files put back per round, at most
)

// ixMissingFiles counts the files of a group's captured messages that the
// library does not have and that the group's download settings want,
// leaving out messages since deleted on Telegram (those cannot be fetched).
func (a *App) ixMissingFiles(ctx context.Context, groupID string) (int, error) {
	meta := ixGroupMeta{ID: groupID, Name: "Chat " + groupID}
	if all, err := a.ixGroups(ctx); err == nil {
		for _, g := range all {
			if g.ID == groupID {
				meta = g
				break
			}
		}
	}
	g, err := a.ixLoadGroup(ctx, meta, false)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range g.Missing {
		if m.Deleted == 0 {
			n++
		}
	}
	return n, nil
}

// ixRequeueFailed puts failed downloads back in the queue (one group, or
// every group when groupID is empty). With auto set, only failures older
// than ixRetryAfter that have not used up their automatic rounds are taken,
// and each one's round is counted.
func (a *App) ixRequeueFailed(ctx context.Context, groupID string, auto bool) (int, error) {
	if a.monitor == nil || a.monitor.RequireRunning() != nil {
		return 0, nil // nothing can download right now; try again later
	}
	now := time.Now().UnixMilli()
	q := `SELECT w.group_id||'_'||w.message_id FROM tgdl_work w`
	var args []any
	if auto {
		q += ` LEFT JOIN ix_retry r ON r.work_key=w.group_id||'_'||w.message_id
			WHERE w.status='failed' AND length(w.body)>0 AND w.updated_at<? AND COALESCE(r.rounds,0)<?`
		args = append(args, now-ixRetryAfter.Milliseconds(), ixRetryRounds)
	} else {
		q += ` WHERE w.status='failed' AND length(w.body)>0`
	}
	if groupID != "" {
		q += ` AND w.group_id=?`
		args = append(args, groupID)
	}
	q += ` ORDER BY w.updated_at LIMIT ?`
	args = append(args, ixRetryBatch)
	rows, err := a.db.Reader.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	var keys []string
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil {
			keys = append(keys, k)
		}
	}
	rows.Close()
	done := 0
	for _, k := range keys {
		ok, err := a.monitor.RetryJob(ctx, k)
		if err != nil {
			return done, err
		}
		if !ok {
			continue
		}
		done++
		if auto {
			_, _ = a.db.Writer.ExecContext(ctx, `INSERT INTO ix_retry(work_key,rounds,last_at) VALUES(?,1,?)
				ON CONFLICT(work_key) DO UPDATE SET rounds=rounds+1,last_at=excluded.last_at`, k, now)
		}
	}
	return done, nil
}

func (a *App) ixRunRetry(ctx context.Context) {
	ticker := time.NewTicker(ixRetryEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !a.ixLoadSettings(ctx).AutoRetry {
			continue
		}
		if n, err := a.ixRequeueFailed(ctx, "", true); err != nil && ctx.Err() == nil {
			a.ixLog("download retry: %v", err)
		} else if n > 0 {
			a.ixLog("download retry: put %d failed download(s) back in the queue", n)
		}
	}
}

type ixDownloadError struct {
	Error string `json:"error"`
	Count int    `json:"count"`
}

type ixDownloadStats struct {
	EngineRunning bool              `json:"engineRunning"`
	QueuePaused   bool              `json:"queuePaused"`
	Waiting       int               `json:"waiting"`
	Downloading   int               `json:"downloading"`
	Failed        int               `json:"failed"`
	GivenUp       int               `json:"givenUp"`  // failed, and automatic retries used up
	LastHour      int               `json:"lastHour"` // finished in the last hour
	LastAt        int64             `json:"lastAt,omitempty"`
	Errors        []ixDownloadError `json:"errors"`
	RetryRounds   int               `json:"retryRounds"`
	RetryAfterMin int               `json:"retryAfterMin"`
	Pace          ixPace            `json:"pace"`
	Days          []ixDownloadDay   `json:"days"` // the last 7 days, newest first
}

// ixPace is the download pacing of all running accounts together.
type ixPace struct {
	Off       bool    `json:"off"`     // pacing switched off in settings
	Speed     float64 `json:"speed"`   // bytes a second, last 10 s
	Speed5m   float64 `json:"speed5m"` // last 5 minutes
	Window    int     `json:"window"`  // file parts allowed in flight
	Floods1h  int     `json:"floods1h"`
	Floods24h int     `json:"floods24h"`
	// Premium waits: Telegram's speed limit for accounts without Premium.
	Premium1h  int        `json:"premium1h"`
	Premium24h int        `json:"premium24h"`
	DCs        []ixPaceDC `json:"dcs"`
}

type ixPaceDC struct {
	telegram.DCPace
	Account string `json:"account,omitempty"` // only with several accounts
}

type ixDownloadDay struct {
	Day   string `json:"day"` // YYYY-MM-DD, this computer's time
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

func (a *App) ixPaceNow() ixPace {
	p := ixPace{Off: !telegram.Pacing()}
	if a.monitor == nil {
		return p
	}
	all := a.monitor.Pace()
	for id, s := range all {
		p.Speed += s.Speed
		p.Speed5m += s.Speed5m
		p.Window += s.Window
		p.Floods1h += s.Floods1h
		p.Floods24h += s.Floods24h
		p.Premium1h += s.Premium1h
		p.Premium24h += s.Premium24h
		for _, d := range s.DCs {
			x := ixPaceDC{DCPace: d}
			if len(all) > 1 {
				x.Account = id
			}
			p.DCs = append(p.DCs, x)
		}
	}
	return p
}

// ixDownloadDays: files and bytes downloaded each of the last 7 days (files
// covered by a saved copy not counted: nothing was downloaded for them).
func (a *App) ixDownloadDays(ctx context.Context) []ixDownloadDay {
	now := time.Now().In(ixTZ)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, ixTZ).AddDate(0, 0, -6)
	byDay := map[string]*ixDownloadDay{}
	var out []ixDownloadDay
	for i := 0; i < 7; i++ {
		day := start.AddDate(0, 0, 6-i).Format("2006-01-02")
		out = append(out, ixDownloadDay{Day: day})
	}
	for i := range out {
		byDay[out[i].Day] = &out[i]
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT w.updated_at, w.file_size FROM tgdl_work w WHERE w.status='completed' AND w.updated_at>=?
		AND NOT EXISTS (SELECT 1 FROM ix_subst s WHERE s.group_id=w.group_id AND s.message_id=w.message_id)`, start.UnixMilli())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var at, size int64
		if rows.Scan(&at, &size) != nil {
			continue
		}
		if d := byDay[time.UnixMilli(at).In(ixTZ).Format("2006-01-02")]; d != nil {
			d.Files++
			d.Bytes += size
		}
	}
	return out
}

func (a *App) ixDownloads(ctx context.Context) ixDownloadStats {
	st := ixDownloadStats{Errors: []ixDownloadError{}, RetryRounds: ixRetryRounds, RetryAfterMin: int(ixRetryAfter / time.Minute)}
	st.EngineRunning = a.monitor != nil && a.monitor.RequireRunning() == nil
	st.Pace = a.ixPaceNow()
	st.Days = a.ixDownloadDays(ctx)
	var paused sql.NullInt64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT paused FROM tgdl_queue_state WHERE id=1`).Scan(&paused)
	st.QueuePaused = paused.Int64 != 0
	hour := time.Now().Add(-time.Hour).UnixMilli()
	var waiting, downloading, failed, recent, last sql.NullInt64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT SUM(status='pending'),SUM(status='processing'),SUM(status='failed'),
		SUM(status='completed' AND updated_at>?),MAX(CASE WHEN status='completed' THEN updated_at END) FROM tgdl_work`, hour).
		Scan(&waiting, &downloading, &failed, &recent, &last)
	st.Waiting, st.Downloading, st.Failed, st.LastHour, st.LastAt = int(waiting.Int64), int(downloading.Int64), int(failed.Int64), int(recent.Int64), last.Int64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM tgdl_work w JOIN ix_retry r ON r.work_key=w.group_id||'_'||w.message_id
		WHERE w.status='failed' AND r.rounds>=?`, ixRetryRounds).Scan(&st.GivenUp)
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT substr(COALESCE(error,''),1,240),COUNT(*) FROM tgdl_work WHERE status='failed' GROUP BY 1 ORDER BY 2 DESC LIMIT 6`); err == nil {
		for rows.Next() {
			var e ixDownloadError
			if rows.Scan(&e.Error, &e.Count) == nil {
				e.Error = strings.TrimSpace(e.Error)
				if e.Error == "" {
					e.Error = "(no reason given)"
				}
				st.Errors = append(st.Errors, e)
			}
		}
		rows.Close()
	}
	return st
}

// handleIxDownloadsRetry puts every failed download back in the queue now,
// including files whose automatic retries were used up (they get a fresh
// set of rounds).
func (a *App) handleIxDownloadsRetry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if a.monitor == nil || a.monitor.RequireRunning() != nil {
		writeJSONError(w, http.StatusConflict, "The download engine is not running, so nothing can be retried yet. Start the monitor on the dashboard first.")
		return
	}
	if _, err := a.db.Writer.ExecContext(ctx, `DELETE FROM ix_retry`); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	total := 0
	for {
		n, err := a.ixRequeueFailed(ctx, "", false)
		total += n
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if n < ixRetryBatch {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "retried": total})
}

// ixRequeueGroupFailed gives a group's failed downloads another go before
// its files pass: a walk on its own does not queue a file again once it has
// failed.
func (a *App) ixRequeueGroupFailed(ctx context.Context, groupID string) {
	for {
		n, err := a.ixRequeueFailed(ctx, groupID, false)
		if err != nil || n < ixRetryBatch {
			return
		}
	}
}
