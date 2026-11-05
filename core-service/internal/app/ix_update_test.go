package app

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/version"
)

func TestIxVersionNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"3.4.18-ix.11", "3.4.18-ix.10.2", true},
		{"3.4.18-ix.10.2", "3.4.18-ix.11", false},
		{"3.4.18-ix.11.1", "3.4.18-ix.11", true},
		{"3.4.18-ix.11", "3.4.18-ix.11.1", false},
		{"3.4.19", "3.4.18-ix.11", true},
		{"3.4.18-ix.10.2", "3.4.18-ix.10.2-regularfix", false}, // a local suffix does not count
		{"3.4.18-ix.10.2-regularfix", "3.4.18-ix.10.2", false},
		{"3.4.18-ix.9", "3.4.18-ix.10", false}, // numbers, not text
		{"", "3.4.18", false},
	} {
		if got := ixVersionNewer(c.a, c.b); got != c.want {
			t.Fatalf("%q newer than %q: %v", c.a, c.b, got)
		}
	}
}

func ixUpdateTestSetup(t *testing.T, latest string, restarts bool) (*App, context.Context, *atomic.Int32) {
	t.Helper()
	old := ixLatestRelease
	ixLatestRelease = func(context.Context) (string, error) { return latest, nil }
	t.Cleanup(func() { ixLatestRelease = old })
	if restarts {
		t.Setenv("TGDL_LAUNCHER_RESTARTS", "1")
	} else {
		t.Setenv("TGDL_LAUNCHER_RESTARTS", "")
	}
	a, ctx := ixTestApp(t)
	exits := new(atomic.Int32)
	a.requestExit = func() { exits.Add(1) }
	t.Cleanup(func() { a.monitor.SetDraining(false) })
	return a, ctx, exits
}

func ixUpdateAction(t *testing.T, a *App, ctx context.Context, action string) (int, map[string]any) {
	t.Helper()
	return ixCall(ctx, a.handleIxUpdateAction, http.MethodPost, "/api/ix/update", map[string]any{"action": action}, nil)
}

func ixWaitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// The page never waits for GitHub, and hears of the new version shortly after.
func TestIxUpdateIsNoticedInTheBackground(t *testing.T) {
	a, ctx, _ := ixUpdateTestSetup(t, "99.0.0", false)
	first := a.ixUpdateNow(ctx)
	if first.Current != version.AppVersion {
		t.Fatalf("%+v", first)
	}
	ixWaitFor(t, "the release to be noticed", func() bool { i := a.ixUpdateNow(ctx); return i.Newer && i.Latest == "99.0.0" })
}

// An older release than this build is not offered.
func TestIxUpdateDoesNotOfferAnOlderRelease(t *testing.T) {
	a, ctx, _ := ixUpdateTestSetup(t, "1.0.0", false)
	ixWaitFor(t, "the check", func() bool { return a.ixUpdateNow(ctx).Latest == "1.0.0" })
	if a.ixUpdateNow(ctx).Newer {
		t.Fatal("an older release was offered")
	}
}

// With a launcher that loops, it exits once the downloads are done: with none in
// progress, at once. Exiting happens once.
func TestIxUpdateRestartsWhenDrainedWithALauncher(t *testing.T) {
	old := ixUpdateWatchEvery
	_ = old
	a, ctx, exits := ixUpdateTestSetup(t, "99.0.0", true)
	code, out := ixUpdateAction(t, a, ctx, "start")
	if code != http.StatusOK || out["draining"] != true {
		t.Fatalf("%d %v", code, out)
	}
	ixWaitFor(t, "the exit", func() bool { return exits.Load() == 1 })
	if !a.ixUpdateNow(ctx).Exiting {
		t.Fatal("the page does not say it is restarting")
	}
	a.ixUpdateExit()
	if exits.Load() != 1 {
		t.Fatalf("%d exits", exits.Load())
	}
}

// Without one, nothing would bring the app back: it never exits, and says so.
func TestIxUpdateNeverExitsWithoutALauncher(t *testing.T) {
	a, ctx, exits := ixUpdateTestSetup(t, "99.0.0", false)
	if code, _ := ixUpdateAction(t, a, ctx, "start"); code != http.StatusOK {
		t.Fatalf("%d", code)
	}
	time.Sleep(ixUpdateWatchEvery + 500*time.Millisecond)
	if exits.Load() != 0 {
		t.Fatal("exited with nothing to bring it back")
	}
	info := a.ixUpdateNow(ctx)
	if !info.Draining || !info.Ready || info.Restarts {
		t.Fatalf("%+v", info)
	}
	if code, _ := ixUpdateAction(t, a, ctx, "now"); code != http.StatusConflict {
		t.Fatalf("restart now without a launcher: %d", code)
	}
	if exits.Load() != 0 {
		t.Fatal("exited with nothing to bring it back")
	}
}

// Cancelling goes back to downloading, and no restart follows.
func TestIxUpdateCancelGoesBackToDownloading(t *testing.T) {
	a, ctx, exits := ixUpdateTestSetup(t, "99.0.0", true)
	a.monitor.SetDraining(true)
	if code, out := ixUpdateAction(t, a, ctx, "cancel"); code != http.StatusOK || out["draining"] != false {
		t.Fatalf("%d %v", code, out)
	}
	if a.monitor.Draining() || exits.Load() != 0 {
		t.Fatalf("draining %v exits %d", a.monitor.Draining(), exits.Load())
	}
}

func TestIxUpdateNowExitsAtOnceWithALauncher(t *testing.T) {
	a, ctx, exits := ixUpdateTestSetup(t, "99.0.0", true)
	if code, _ := ixUpdateAction(t, a, ctx, "now"); code != http.StatusOK || exits.Load() != 1 {
		t.Fatalf("%d, %d exits", code, exits.Load())
	}
}

func TestIxUpdateBadActionIsRefused(t *testing.T) {
	a, ctx, exits := ixUpdateTestSetup(t, "99.0.0", true)
	if code, _ := ixUpdateAction(t, a, ctx, "explode"); code != http.StatusBadRequest || exits.Load() != 0 {
		t.Fatalf("%d", code)
	}
}

// While an update waits nothing moves, and that must not read as a stuck queue:
// the rules for that would let go held copies and set-aside files.
func TestIxDrainingDoesNotReadAsAStuckQueue(t *testing.T) {
	a, ctx, _ := ixUpdateTestSetup(t, "99.0.0", false)
	if a.ixQueuePaused(ctx) {
		t.Fatal("paused before anything")
	}
	a.monitor.SetDraining(true)
	if !a.ixQueuePaused(ctx) {
		t.Fatal("waiting for an update reads as a stuck queue")
	}
	// ... and it is not the queue's own pause, whose resume clears every item's.
	var paused int
	if err := a.db.Reader.QueryRow(`SELECT paused FROM tgdl_queue_state WHERE id=1`).Scan(&paused); err != nil || paused != 0 {
		t.Fatalf("paused=%d err=%v", paused, err)
	}
}
