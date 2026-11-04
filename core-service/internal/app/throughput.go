package app

import (
	"context"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// The dashboard status bar shows how fast files arrive right now and how
// much arrived lately. Both ride the monitor status payload (HTTP and the
// 3 s `monitor_status_push`), under "throughput", for admins only: a
// guest's status (HTTP, and its own copy of the push) has none
// (monitorStatusForGuests), and the status bar hides the chips then:
//
//	"throughput": {
//	  "running": true,                       // an engine run is in progress
//	  "speed": 2400000,                      // bytes a second, null when unknown or not running
//	  "speedSource": "pacer",                // pacer (last 10 s) or counter (bytes received)
//	  "lastHour": {"files": 36, "bytes": 1200000000},
//	  "last24h":  {"files": 410, "bytes": 18000000000}
//	}
//
// The recent totals count completed downloads in tgdl_work, leaving out
// files covered by a saved copy (ix_subst: nothing was downloaded for
// them), as the archive's Downloads page does. They are cached for
// throughputRecentTTL so the 3 s push does not scan the work table each
// time. A failed query keeps the last figures; with none yet, the totals
// are null rather than invented zeros.

const (
	throughputRecentTTL = 30 * time.Second
	// The counter speed spans about this much time: like the pacer's
	// 10 s figure, steady enough not to flicker between pushes.
	throughputWindow = 10 * time.Second
	// Samples older than this are dropped (one older sample stays as the
	// baseline when nothing newer exists).
	throughputKeep = time.Minute
)

type throughputTotals struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

type throughputSample struct {
	at    time.Time
	bytes int64
}

type throughputState struct {
	now func() time.Time // tests

	recentMu sync.Mutex
	recentAt time.Time
	hour     *throughputTotals
	day      *throughputTotals

	meterMu sync.Mutex
	samples []throughputSample
}

func (t *throughputState) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// counterSpeed records the engine's bytes-received counter and returns the
// rate over the last throughputWindow (or the longest span it has). ok is
// false until two samples at least a second apart exist.
func (t *throughputState) counterSpeed(now time.Time, received int64) (float64, bool) {
	t.meterMu.Lock()
	defer t.meterMu.Unlock()
	if n := len(t.samples); n > 0 && (received < t.samples[n-1].bytes || now.Before(t.samples[n-1].at)) {
		t.samples = t.samples[:0] // counter or clock went back: start over
	}
	if n := len(t.samples); n == 0 || now.Sub(t.samples[n-1].at) >= time.Second {
		t.samples = append(t.samples, throughputSample{at: now, bytes: received})
	}
	// The baseline is the newest sample at least a window old; older ones
	// go. A baseline from long ago (nobody asked for a while) would report
	// a stale average, so it goes too and the next call measures afresh.
	base := 0
	for i := range t.samples {
		if now.Sub(t.samples[i].at) >= throughputWindow {
			base = i
		}
	}
	if now.Sub(t.samples[base].at) > throughputKeep {
		base++
	}
	t.samples = append(t.samples[:0], t.samples[base:]...)
	if len(t.samples) == 0 {
		t.samples = append(t.samples, throughputSample{at: now, bytes: received})
	}
	first := t.samples[0]
	secs := now.Sub(first.at).Seconds()
	if secs < 1 {
		return 0, false
	}
	return float64(max(received-first.bytes, 0)) / secs, true
}

// throughputRecent returns the cached last-hour and last-24-hour totals, querying
// again once they are older than throughputRecentTTL.
func (a *App) throughputRecent(ctx context.Context, now time.Time) (hour, day *throughputTotals) {
	t := &a.throughput
	t.recentMu.Lock()
	defer t.recentMu.Unlock()
	if t.hour != nil && now.Sub(t.recentAt) < throughputRecentTTL && !now.Before(t.recentAt) {
		return t.hour, t.day
	}
	h, d, err := a.queryThroughputRecent(ctx, now)
	if err != nil {
		return t.hour, t.day // last figures, or null before the first success
	}
	t.hour, t.day, t.recentAt = &h, &d, now
	return t.hour, t.day
}

func (a *App) queryThroughputRecent(ctx context.Context, now time.Time) (hour, day throughputTotals, err error) {
	hourFrom := now.Add(-time.Hour).UnixMilli()
	dayFrom := now.Add(-24 * time.Hour).UnixMilli()
	err = a.db.Reader.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(w.updated_at>=?),0), COALESCE(SUM(CASE WHEN w.updated_at>=? THEN w.file_size ELSE 0 END),0),
		COUNT(*), COALESCE(SUM(w.file_size),0)
		FROM tgdl_work w WHERE w.status='completed' AND w.updated_at>=?
		AND NOT EXISTS (SELECT 1 FROM ix_subst s WHERE s.group_id=w.group_id AND s.message_id=w.message_id)`,
		hourFrom, hourFrom, dayFrom).Scan(&hour.Files, &hour.Bytes, &day.Files, &day.Bytes)
	return hour, day, err
}

// throughputPayload is the "throughput" object of the monitor status.
func (a *App) throughputPayload(ctx context.Context) map[string]any {
	now := a.throughput.clock()
	hour, day := a.throughputRecent(ctx, now)
	out := map[string]any{"running": false, "speed": nil, "speedSource": nil, "lastHour": hour, "last24h": day}
	if a.monitor == nil {
		return out
	}
	// Sample the counter on every call, running or not, so a run that just
	// started has a baseline.
	counter, counterOK := a.throughput.counterSpeed(now, a.monitor.BytesReceived())
	if _, _, running := a.monitor.Running(); !running {
		return out
	}
	out["running"] = true
	if telegram.Pacing() {
		paced, speed := false, 0.0
		for _, s := range a.monitor.Pace() {
			if len(s.DCs) > 0 {
				paced = true
			}
			speed += s.Speed
		}
		if paced {
			out["speed"], out["speedSource"] = int64(speed), "pacer"
			return out
		}
	}
	if counterOK {
		out["speed"], out["speedSource"] = int64(counter), "counter"
	}
	return out
}
