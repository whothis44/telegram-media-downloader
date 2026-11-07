package app

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// ix.12.14: "Check for updates" asks GitHub now instead of within the hour,
// but at most once a minute (60 anonymous requests an hour per address,
// shared with the launcher), and answers with what it found.
func TestIx1214CheckForUpdatesNowIsThrottled(t *testing.T) {
	a, ctx, _ := ixUpdateTestSetup(t, "99.0.0", false)
	var calls atomic.Int32
	latest := atomic.Value{}
	latest.Store("99.0.0")
	ix1214Fresh(t, a)
	ixLatestRelease = func(context.Context) (string, error) { calls.Add(1); return latest.Load().(string), nil }

	code, out := ixUpdateAction(t, a, ctx, "check")
	if code != http.StatusOK || out["latest"] != "99.0.0" || out["newer"] != true || out["checkedAt"] == nil {
		t.Fatalf("first check: %d %v", code, out)
	}
	if calls.Load() != 1 {
		t.Fatalf("GitHub asked %d times", calls.Load())
	}
	// Again at once: no new request, the last answer shown.
	latest.Store("100.0.0")
	if _, out = ixUpdateAction(t, a, ctx, "check"); out["latest"] != "99.0.0" || calls.Load() != 1 {
		t.Fatalf("asked again within a minute: %d calls, %v", calls.Load(), out)
	}
	// The page's own look (every load) does not ask within the hour either.
	a.ixUpdateNow(ctx)
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("the page asked: %d calls", calls.Load())
	}
	// A minute later the button asks again.
	box := a.ixUpdates()
	box.mu.Lock()
	box.checked = time.Now().Add(-ixUpdateCheckMin - time.Second)
	box.mu.Unlock()
	if _, out = ixUpdateAction(t, a, ctx, "check"); out["latest"] != "100.0.0" || calls.Load() != 2 {
		t.Fatalf("not asked after a minute: %d calls, %v", calls.Load(), out)
	}
	// Bad actions are still refused.
	if code, _ := ixUpdateAction(t, a, ctx, "nonsense"); code != http.StatusBadRequest {
		t.Fatalf("nonsense: %d", code)
	}
}

// When GitHub does not answer in time, the button returns anyway.
func TestIx1214CheckForUpdatesDoesNotHang(t *testing.T) {
	a, ctx, _ := ixUpdateTestSetup(t, "99.0.0", false)
	ix1214Fresh(t, a)
	release := make(chan struct{})
	ixLatestRelease = func(ctx context.Context) (string, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "99.0.0", nil
	}
	old := ixUpdateCheckWait
	ixUpdateCheckWait = 200 * time.Millisecond
	defer func() { ixUpdateCheckWait = old }()
	start := time.Now()
	code, out := ixUpdateAction(t, a, ctx, "check")
	close(release)
	if code != http.StatusOK || time.Since(start) > 5*time.Second || out["checking"] != true {
		t.Fatalf("%d after %s: %v", code, time.Since(start), out)
	}
	box := a.ixUpdates()
	ixWaitFor(t, "the slow look to end", func() bool { box.mu.Lock(); defer box.mu.Unlock(); return !box.checking })
}

// ix1214Fresh waits out the look the test app made at start and forgets it.
func ix1214Fresh(t *testing.T, a *App) {
	t.Helper()
	box := a.ixUpdates()
	ixWaitFor(t, "the first look to end", func() bool { box.mu.Lock(); defer box.mu.Unlock(); return !box.checking })
	box.mu.Lock()
	box.checked = time.Time{}
	box.mu.Unlock()
}
