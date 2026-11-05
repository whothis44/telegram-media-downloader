package telegram

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/gotd/td/tg"
)

// Telegram's app configuration specifies separate per-DC file concurrency
// limits below/above 20 MB. These are file slots, not the pacer's part slots.
// Conservative local defaults apply when configuration is unavailable.
const downloadLargeFile = 20 * 1024 * 1024

type downloadFileKey struct {
	dc    int
	large bool
}

type downloadFiles struct {
	mu           sync.Mutex
	small, large int
	active       map[downloadFileKey]int
	waiting      map[downloadFileKey]rankWaiters
	wake         chan struct{}
}

// How often a transfer waiting for a file slot asks whether to give way.
var downloadYieldEvery = 15 * time.Second

func newDownloadFiles() *downloadFiles {
	return &downloadFiles{small: 4, large: 2, active: map[downloadFileKey]int{}, waiting: map[downloadFileKey]rankWaiters{}, wake: make(chan struct{})}
}

func (f *downloadFiles) changed() {
	close(f.wake)
	f.wake = make(chan struct{})
}

func (f *downloadFiles) configure(value tg.JSONValueClass) {
	obj, ok := value.(*tg.JSONObject)
	if !ok {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, entry := range obj.Value {
		n, ok := entry.Value.(*tg.JSONNumber)
		if !ok || math.IsNaN(n.Value) || math.IsInf(n.Value, 0) || n.Value < 1 || n.Value > 64 || math.Trunc(n.Value) != n.Value {
			continue
		}
		switch entry.Key {
		case "small_queue_max_active_operations_count":
			f.small = int(n.Value)
		case "large_queue_max_active_operations_count":
			f.large = int(n.Value)
		}
	}
	f.changed()
}

func (f *downloadFiles) acquire(ctx context.Context, dc int, size int64) (func(), error) {
	key := downloadFileKey{dc: dc, large: size >= downloadLargeFile}
	rank := downloadRankOf(ctx)
	registered := false
	defer func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if registered {
			f.waiting[key].remove(rank.key)
			f.changed() // the slot it was first in line for may be free
		}
	}()
	var ask <-chan time.Time
	if rank.yield != nil || rank.room != nil {
		tick := time.NewTicker(downloadYieldEvery)
		defer tick.Stop()
		ask = tick.C
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f.mu.Lock()
		limit := f.small
		if key.large {
			limit = f.large
		}
		waiters := f.waiting[key]
		if f.active[key] < limit && !waiters.better(rank.key) {
			f.active[key]++
			if registered {
				registered = false
				waiters.remove(rank.key)
			}
			f.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					f.active[key]--
					f.changed()
				})
			}, nil
		}
		if !registered {
			if waiters == nil {
				waiters = rankWaiters{}
				f.waiting[key] = waiters
			}
			waiters.add(rank.key)
			registered = true
		}
		ch := f.wake
		f.mu.Unlock()
		resume := pauseDownloadWait(ctx)
		select {
		case <-ctx.Done():
			resume()
			return nil, ctx.Err()
		case <-ch:
			resume()
		case <-ask:
			resume()
			if rank.yield != nil && rank.yield() {
				return nil, ErrDownloadYield
			}
			if key.large && rank.room != nil && f.smallFree(dc) && rank.room() {
				return nil, ErrDownloadMakeRoom
			}
		}
	}
}

// smallFree says whether a small file of dc would get a slot at once: fewer
// running and waiting than there are slots.
func (f *downloadFiles) smallFree(dc int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := downloadFileKey{dc: dc}
	n := f.active[key]
	for _, c := range f.waiting[key] {
		n += c
	}
	return n < f.small
}

func (f *downloadFiles) fillStats(s *PaceStats) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range s.DCs {
		d := &s.DCs[i]
		d.SmallFiles = f.active[downloadFileKey{dc: d.DC}]
		d.LargeFiles = f.active[downloadFileKey{dc: d.DC, large: true}]
		d.SmallFileLimit, d.LargeFileLimit = f.small, f.large
	}
}

func (c *GotdClient) loadDownloadLimits(ctx context.Context) {
	if c.files == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if result, err := c.API().HelpGetAppConfig(ctx, 0); err == nil {
		if cfg, ok := result.(*tg.HelpAppConfig); ok {
			c.files.configure(cfg.Config)
		}
	}
}
