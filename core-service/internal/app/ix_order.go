package app

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
)

// What goes last, and in which order.
//
// Several things wait at the back of the download queue, paused so that the
// engine does not take them. They go in one order, a stage at a time:
//
//  0. normal work: whatever the engine can take now;
//  1. files set aside by the deadline order (ix_deferred), band by band,
//     private chats first (ix_deadline.go);
//  2. probable copies held back (ix_held), not confirmed (ix_held.go);
//  3. originals of big videos saved in a lower quality first, waiting to
//     replace it (ix_lowq state 'saved', ix_lowq.go);
//  4. confirmed copies (ix_copy_hold.confirm), which also wait for the
//     chats still to be read: they are certain copies, so they only go
//     once nothing new can come.
//
// A stage goes once every stage before it is done. "Done" leaves out what
// could not go anyway: items a person paused, items of accounts that are
// not downloading, items waiting to retry later, and live items while the
// engine runs without monitoring (a manual run takes none). Otherwise one
// of those would hold every later stage back for ever.
//
// An item belongs to one stage: a held copy that is also set aside counts
// as held (once let go it is set aside, and the deadline order lets it go).
// A lesser copy waiting for its better copy still in the queue follows that
// one and counts nowhere: it is checked once the better copy is saved.
//
// Held copies that are never let go automatically (the setting to hold
// them for ever) wait for a person and count nowhere either. With the
// setting that lets confirmed copies go early, they count as held copies.

const (
	ixStageWork      = iota // what the engine can take now
	ixStageDeferred         // set aside by the deadline order
	ixStageHeld             // probable copies held back
	ixStageLowq             // originals of videos saved in a lower quality first
	ixStageConfirmed        // confirmed copies
	ixStageCount
)

// ixOrder is how much each stage still has that could go.
type ixOrder struct {
	Waiting [ixStageCount]int
	Rescans int // chats still to be read (only confirmed copies wait for them)
}

// clearBefore: every stage before stage is done, so stage may go.
func (o ixOrder) clearBefore(stage int) bool {
	for s := 0; s < stage && s < ixStageCount; s++ {
		if o.Waiting[s] > 0 {
			return false
		}
	}
	return stage < ixStageConfirmed || o.Rescans == 0
}

// ixOrderNow measures the stages.
func (a *App) ixOrderNow(ctx context.Context) (ixOrder, error) {
	var o ixOrder
	s := a.ixLoadSettings(ctx)
	now := time.Now()
	// Stage of a held copy: none when held for ever, held, or confirmed.
	held := "2"
	confirmed := "4"
	switch {
	case s.HoldLikely && s.HoldForever:
		held, confirmed = "-1", "-1"
	case s.ConfirmedGoEarly:
		confirmed = "2"
	}
	args := []any{now.UnixMilli()}
	acct := ""
	if accounts := a.ixWorkingAccounts(ctx, now); len(accounts) > 0 {
		acct = " AND w.account_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(accounts)), ",") + ")"
		for _, id := range accounts {
			args = append(args, id)
		}
	}
	if _, observing, running := ixEngineRun(a); running && !observing {
		acct += " AND w.origin<>'live'" // a manual run does not take live items
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT stage, COUNT(*) FROM (SELECT CASE
			WHEN h.work_id IS NOT NULL AND COALESCE(c.confirm,'')='confirmed' THEN `+confirmed+`
			WHEN h.work_id IS NOT NULL THEN `+held+`
			WHEN EXISTS (SELECT 1 FROM ix_lowq q WHERE q.work_id=w.id AND q.state='saved') THEN 3
			WHEN EXISTS (SELECT 1 FROM ix_deferred x WHERE x.work_id=w.id AND x.released_at=0) THEN 1
			WHEN w.paused=0 THEN 0
			ELSE -1 END AS stage -- -1: paused by a person
		FROM tgdl_work w
		LEFT JOIN ix_held h ON h.work_id=w.id AND h.released_at=0 AND h.reason<>''
		LEFT JOIN ix_copy_hold c ON c.work_id=w.id
		WHERE w.status='pending' AND w.retry_at<=?`+acct+`
		AND NOT (h.work_id IS NOT NULL AND COALESCE(c.how,'')='queued'
			AND EXISTS (SELECT 1 FROM tgdl_work l WHERE l.id=c.of_work AND l.status IN ('pending','processing'))))
		WHERE stage>=0 GROUP BY stage`, args...)
	if err != nil {
		return o, err
	}
	for rows.Next() {
		var stage, n int
		if rows.Scan(&stage, &n) == nil && stage >= 0 && stage < ixStageCount {
			o.Waiting[stage] = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return o, err
	}
	// Chats still to be read: not when a person stopped the rescans, and
	// not a chat waiting to try again later.
	if q, err := a.ixLoadQueue(ctx); err == nil && q.Running {
		for _, it := range q.Items {
			if it.State == "running" || it.State == "waiting" && it.NextAt <= now.UnixMilli() {
				o.Rescans++
			}
		}
	}
	return o, nil
}

// ---------- progress ----------

// Rules that let things go when "nothing finished for half an hour" judge
// progress by bytes too: with only big files left, each can take hours
// under Telegram's limits while bytes keep arriving.

// ixBytesReceived is what the engine has received since it started
// (replaceable in tests).
var ixBytesReceived = func(a *App) int64 {
	if a.monitor == nil {
		return 0
	}
	return a.monitor.BytesReceived()
}

type ixProgressBox struct {
	mu    sync.Mutex
	seen  bool
	bytes int64
	at    time.Time // when the bytes were last seen to grow
}

var ixProgressState sync.Map // *App -> *ixProgressBox

func (a *App) ixProgressBox() *ixProgressBox {
	v, _ := ixProgressState.LoadOrStore(a, &ixProgressBox{})
	return v.(*ixProgressBox)
}

// ixLastProgress is when downloads last moved: a file finished, or bytes
// arrived since the last look (the checks look every two minutes). The
// first look counts as movement: after a restart (the app may have been
// off for hours) the downloads get half an hour to start before anything
// is judged stuck.
func (a *App) ixLastProgress(ctx context.Context) time.Time {
	box := a.ixProgressBox()
	n := ixBytesReceived(a)
	box.mu.Lock()
	if !box.seen || n != box.bytes {
		box.seen, box.bytes, box.at = true, n, time.Now()
	}
	at := box.at
	box.mu.Unlock()
	var last int64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(MAX(updated_at),0) FROM tgdl_work WHERE status='completed'`).Scan(&last)
	if last > 0 && time.UnixMilli(last).After(at) {
		at = time.UnixMilli(last)
	}
	return at
}

// ixBigApartTimes: big files waiting for their lane stay out of a
// backfill's count up to this many times its limit (ixWaitingApart).
const ixBigApartTimes = 2

// ixWaitingApart is how many backfill items wait at the back of the queue,
// paused: held copies, files set aside by the deadline order, and originals
// waiting to replace a lower quality. They do not count towards a
// backfill's queue limit: otherwise they fill it, no file that can download
// comes in behind them, and the backfill stops bringing new files.
//
// Files of 1 GB or more waiting for the big-file lane (engine.BigFileLane)
// do not count either, up to ixBigApartTimes the backfill's limit (cap):
// only a few of them download at once, so a queue filled with them (as
// queues of earlier versions are) would stop the backfill for days while
// the workers have nothing else to take. Beyond that they count, so that
// the backfills cannot queue big files without end.
//
// The two counts are kept for ixApartCacheTTL: a backfill at its limit asks
// every 250 ms.
func (a *App) ixWaitingApart(ctx context.Context, cap int) int {
	n := a.ixCaches().apart.get(ixApartCacheTTL, func() ([2]int, bool) { return a.ixWaitingApartNow(ctx) })
	return n[0] + min(n[1], ixBigApartTimes*max(0, cap))
}

// ixWaitingApartNow counts the paused items waiting apart, and the big
// files waiting for their lane.
func (a *App) ixWaitingApartNow(ctx context.Context) ([2]int, bool) {
	var apart, big int
	err := a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(SUM(w.paused=1),0), COALESCE(SUM(w.paused=0),0) FROM tgdl_work w WHERE w.status='pending' AND w.origin<>'live' AND (
		w.paused=1 AND (
		EXISTS (SELECT 1 FROM ix_held h WHERE h.work_id=w.id AND h.released_at=0 AND h.reason<>'')
		OR EXISTS (SELECT 1 FROM ix_deferred x WHERE x.work_id=w.id AND x.released_at=0)
		OR EXISTS (SELECT 1 FROM ix_lowq q WHERE q.work_id=w.id AND q.state='saved'))
		OR ? > 0 AND w.paused=0 AND w.ttl_seconds=0 AND w.file_size>=?)`, engine.BigFileLane(), engine.BigFileSize).Scan(&apart, &big)
	return [2]int{apart, big}, err == nil
}
