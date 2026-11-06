package app

import (
	"context"
	"sync"
	"time"
)

// A rescan is "done" once it has read every message and queued every
// missing file; the files themselves may still be waiting to download. The
// rescan list shows what each group still has in the download queue, so a
// group only reads as done once its files are in. Files that wait for a
// person (paused by one, or likely copies held for ever) are counted
// apart: a group with only those left reads "done · N held back", not
// "downloading" for ever. Live downloads are not a rescan's: they do not
// count.

type ixGroupLeft struct {
	Left  int `json:"left"`  // waiting or downloading
	Apart int `json:"apart"` // set aside for now, to go by themselves: likely copies, larger files while time is short, originals after a lower quality
	Held  int `json:"held"`  // waiting for a person: paused by one, or likely copies held for ever
}

// ixQueueLeft counts each group's files still in the download queue
// (cached for ixStatusCacheTTL: the Archive page asks every few seconds).
func (a *App) ixQueueLeft(ctx context.Context) map[string]ixGroupLeft {
	return a.ixCaches().queueLeft.get(ixStatusCacheTTL, func() (map[string]ixGroupLeft, bool) {
		return a.ixQueueLeftNow(ctx)
	})
}

func (a *App) ixQueueLeftNow(ctx context.Context) (map[string]ixGroupLeft, bool) {
	s := a.ixLoadSettings(ctx)
	forever := s.HoldLikely && s.HoldForever
	out := map[string]ixGroupLeft{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, COALESCE(SUM(paused=0),0), COALESCE(SUM(paused<>0 AND goes),0), COALESCE(SUM(paused<>0 AND NOT goes),0) FROM (
		SELECT w.group_id, w.paused,
			EXISTS (SELECT 1 FROM ix_deferred x WHERE x.work_id=w.id AND x.released_at=0)
			OR EXISTS (SELECT 1 FROM ix_lowq q WHERE q.work_id=w.id AND q.state='saved')
			OR (? = 0 AND EXISTS (SELECT 1 FROM ix_held h WHERE h.work_id=w.id AND h.released_at=0 AND h.reason<>'')) AS goes
		FROM tgdl_work w WHERE w.status IN ('pending','processing') AND w.origin<>'live')
		GROUP BY group_id`, forever)
	if err != nil {
		return out, false
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var g ixGroupLeft
		if rows.Scan(&id, &g.Left, &g.Apart, &g.Held) == nil {
			out[id] = g
		}
	}
	return out, rows.Err() == nil
}

// ---------- short caches of status figures ----------

// The Archive page and the backfills ask for some figures often; each is a
// scan of the queue (tens of milliseconds at 50,000 items). They are kept
// a little while. Tests set both to 0 (main_test.go).
var (
	ixStatusCacheTTL = 30 * time.Second // what the pages show
	ixApartCacheTTL  = 3 * time.Second  // ixWaitingApart, which a backfill at its limit polls every 250 ms
)

// ixCached keeps one value for a while. A failed load is not kept.
type ixCached[T any] struct {
	mu  sync.Mutex
	at  time.Time
	val T
	set bool
}

func (c *ixCached[T]) get(ttl time.Duration, load func() (T, bool)) T {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set && ttl > 0 && time.Since(c.at) < ttl {
		return c.val
	}
	v, ok := load()
	if ok {
		c.val, c.at, c.set = v, time.Now(), true
	}
	return v
}

type ixCacheBox struct {
	queueLeft ixCached[map[string]ixGroupLeft]
	bigFiles  ixCached[ixBigFiles]
	apart     ixCached[[3]int]
	coverBase ixCached[ixCoverBase]
}

var ixCacheState sync.Map // *App -> *ixCacheBox

func (a *App) ixCaches() *ixCacheBox {
	v, _ := ixCacheState.LoadOrStore(a, &ixCacheBox{})
	return v.(*ixCacheBox)
}
