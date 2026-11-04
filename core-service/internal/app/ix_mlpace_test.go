package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ixPaceMode is the window most often seen.
func ixPaceMode(ws []int) int {
	n := map[int]int{}
	best := 0
	for _, w := range ws {
		n[w]++
		if n[w] > n[best] {
			best = w
		}
	}
	return best
}

// The controller alone: a service whose speed grows with the files in
// flight up to a limit, then stays flat (with a little noise).
func TestIxMLPaceSettlesAtTheLimit(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for _, c := range []struct{ start, limit, want int }{{4, 6, 6}, {3, 3, 3}, {4, 1, 1}, {4, 40, ixMLParMax}, {1, 9, 9}} {
		p := &ixMLPace{stats: &ixMLStats{}, window: c.start}
		var seen []int
		for i := 0; i < 400; i++ {
			p.mu.Lock()
			speed := float64(min(p.window, c.limit)) * 10 * (1 + 0.04*(rng.Float64()-0.5))
			p.judgeLocked(speed)
			if i >= 200 {
				seen = append(seen, p.window)
			}
			p.mu.Unlock()
		}
		if got := ixPaceMode(seen); got != c.want {
			t.Fatalf("start %d, limit %d: settled at %d (%v)", c.start, c.limit, got, seen[:40])
		}
	}
	// An error of the service cuts at once, and takes back a try.
	p := &ixMLPace{stats: &ixMLStats{}, window: 10}
	p.trial, p.trialFrom, p.window = 1, 10, 11
	p.cutLocked()
	if p.window != 6 || p.trial != 0 || p.hold == 0 {
		t.Fatalf("after a failure: %d (trial %d, hold %d)", p.window, p.trial, p.hold)
	}
	p.window = 1
	p.cutLocked()
	if p.window != 1 {
		t.Fatalf("below the least: %d", p.window)
	}
	ctx := context.Background()
	if !ixMLPaceFault(ctx, ixMLUnreachable{fmt.Errorf("down")}) || !ixMLPaceFault(ctx, context.DeadlineExceeded) || ixMLPaceFault(ctx, ixMLFileErr{"bad picture"}) {
		t.Fatal("faults told apart wrongly")
	}
}

// ixPaceFakeML answers embeddings, at most limit at a time, each taking d:
// more requests than limit only wait.
func ixPaceFakeML(limit int, d time.Duration, peak *atomic.Int32) *httptest.Server {
	slots := make(chan struct{}, limit)
	var now atomic.Int32
	raw, _ := json.Marshal(ixVFVec("1:1"))
	answer, _ := json.Marshal(map[string]any{"clip": string(raw)})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			_, _ = io.WriteString(w, "pong")
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		n := now.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		slots <- struct{}{}
		time.Sleep(d)
		<-slots
		now.Add(-1)
		_, _ = w.Write(answer)
	}))
}

// The whole loop: files go through as many at a time as the pace allows,
// and the pace settles near what the service can do at once.
func TestIxMLParallelismSettlesNearTheService(t *testing.T) {
	const limit = 5
	old := ixMLPaceInterval
	ixMLPaceInterval = 150 * time.Millisecond
	defer func() { ixMLPaceInterval = old }()
	var peak atomic.Int32
	srv := ixPaceFakeML(limit, 25*time.Millisecond, &peak)
	defer srv.Close()
	a, ctx := ix8App(t, nil)
	files := make([][3]string, 6000)
	for i := range files {
		files[i] = [3]string{fmt.Sprintf("p%05d", i), "photo", ""}
	}
	m := ixVFModel(files)
	c := &ixMLClient{base: srv.URL, model: "m", http: ixMLHTTP}
	var mu sync.Mutex
	done := map[string]bool{}
	task := &ixMLTask{
		name: "test", model: "m", errTable: "ix_ml_err", par: 2,
		wanted: func(it *ixMItem) bool { return true },
		done: func(k string) bool {
			mu.Lock()
			defer mu.Unlock()
			return done[k]
		},
		run: func(ctx context.Context, it *ixMItem) error {
			_, err := c.embed(ctx, ixMLTestPicture(), "")
			if err == nil {
				mu.Lock()
				done[it.Key] = true
				mu.Unlock()
			}
			return err
		},
		test: func(context.Context) error { return nil },
		note: func(bool, string) {},
	}
	pace := a.ixMLPaceOf(task)
	var windows []int
	stop := make(chan struct{})
	sampled := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(sampled)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if time.Since(start) > 3*time.Second {
					windows = append(windows, pace.size())
				}
			}
		}
	}()
	for time.Since(start) < 6*time.Second {
		if a.ixMLRoundTask(ctx, task, m, 100) >= time.Minute {
			break // ran out of files
		}
	}
	close(stop)
	<-sampled
	got := ixPaceMode(windows)
	t.Logf("settled at %d at once (service: %d); peak in flight %d; %d files done", got, limit, peak.Load(), len(done))
	if got < limit-1 || got > limit+1 {
		t.Fatalf("settled at %d, the service does %d at once (%v)", got, limit, windows)
	}
	atOnce, timing := a.ixMLPaceReport()
	if !strings.Contains(atOnce, "test ") || !strings.Contains(timing, "server ") || strings.Contains(timing, "p0") {
		t.Fatalf("report: %q / %q", atOnce, timing)
	}
	if a.ixMLStatusNow(ctx).Par["test"] != pace.size() {
		t.Fatal("files at once not in the status")
	}
}

// A service that fails cuts the files in flight at once.
func TestIxMLParallelismBacksOffOnFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			_, _ = io.WriteString(w, "pong")
			return
		}
		http.Error(w, "out of memory", 500)
	}))
	defer srv.Close()
	a, ctx := ix8App(t, map[string]any{"mlUrl": srv.URL})
	m := ixVFModel([][3]string{{"p1", "photo", ""}, {"p2", "photo", ""}, {"p3", "photo", ""}, {"p4", "photo", ""}, {"p5", "photo", ""}})
	old := ixMLPicturesHook
	defer func() { ixMLPicturesHook = old }()
	ixMLPicturesHook = func(_ *App, _ context.Context, it *ixMItem) ([][]byte, error) {
		return [][]byte{ixMLTestPicture()}, nil
	}
	c := a.ixMLClientNow(ctx)
	_ = a.ixMLBox().store.load(ctx, a.db.Reader, c.model)
	if wait := a.ixMLRound(ctx, c, m, 10); wait < 5*time.Minute {
		t.Fatalf("wait %v", wait)
	}
	if got := a.ixMLPars()["clip"]; got != 2 {
		t.Fatalf("clip at once after a failure: %d", got)
	}
}
