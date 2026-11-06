package app

import (
	"context"
	"testing"
	"time"
)

// ix.12.3: Install updates automatically.

func ixAutoSetup(t *testing.T, latest string, restarts, on bool) (*App, context.Context, func() int32) {
	t.Helper()
	a, ctx, exits := ixUpdateTestSetup(t, latest, restarts)
	s := a.ixLoadSettings(ctx)
	s.AutoUpdate = on
	if err := a.ixSaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	ixWaitFor(t, "the release to be noticed", func() bool { return a.ixUpdateNow(ctx).Latest == latest })
	return a, ctx, exits.Load
}

// Off by default: saved settings from before keep updates by hand.
func TestIxAutoUpdateIsOffByDefault(t *testing.T) {
	if ixDefaultSettings().AutoUpdate {
		t.Fatal("on by default")
	}
	s, err := ixParseSettings(map[string]any{"autoUpdate": true}, ixDefaultSettings())
	if err != nil || !s.AutoUpdate {
		t.Fatalf("%+v %v", s.AutoUpdate, err)
	}
}

// On, with a launcher that loops: a newer release starts the drain by itself,
// and with nothing in progress the app exits for the launcher to update it.
func TestIxAutoUpdateStartsTheDrainByItself(t *testing.T) {
	a, ctx, exits := ixAutoSetup(t, "99.0.0", true, true)
	a.ixAutoUpdateStep(ctx, time.Now())
	if !a.monitor.Draining() {
		t.Fatal("no drain started")
	}
	info := a.ixUpdateNow(ctx)
	if !info.AutoNow || !info.Auto {
		t.Fatalf("%+v", info)
	}
	ixWaitFor(t, "the exit", func() bool { return exits() == 1 })
}

// Without a launcher that loops, it never starts: the app would stop
// downloading and wait for a person.
func TestIxAutoUpdateNeedsALauncher(t *testing.T) {
	a, ctx, exits := ixAutoSetup(t, "99.0.0", false, true)
	a.ixAutoUpdateStep(ctx, time.Now())
	if a.monitor.Draining() || exits() != 0 {
		t.Fatal("drained with nothing to bring the app back")
	}
}

// Off, or no newer release: nothing happens.
func TestIxAutoUpdateLeavesItAloneWhenOffOrCurrent(t *testing.T) {
	a, ctx, _ := ixAutoSetup(t, "99.0.0", true, false)
	a.ixAutoUpdateStep(ctx, time.Now())
	if a.monitor.Draining() {
		t.Fatal("drained with the setting off")
	}
	b, ctx2, _ := ixAutoSetup(t, "1.0.0", true, true)
	b.ixAutoUpdateStep(ctx2, time.Now())
	if b.monitor.Draining() {
		t.Fatal("drained for an older release")
	}
}

// Going back to downloading by hand leaves that version alone.
func TestIxAutoUpdateRespectsGoingBack(t *testing.T) {
	a, ctx, _ := ixAutoSetup(t, "99.0.0", true, true)
	// Hold the watch back so it does not exit: as if files were in progress.
	box := a.ixUpdates()
	box.mu.Lock()
	box.watching = true
	box.mu.Unlock()
	t.Cleanup(func() { box.mu.Lock(); box.watching = false; box.mu.Unlock() })
	a.ixAutoUpdateStep(ctx, time.Now())
	if !a.monitor.Draining() {
		t.Fatal("no drain")
	}
	if code, _ := ixUpdateAction(t, a, ctx, "cancel"); code != 200 {
		t.Fatalf("%d", code)
	}
	if a.monitor.Draining() {
		t.Fatal("still draining")
	}
	a.ixAutoUpdateStep(ctx, time.Now().Add(2*time.Hour))
	if a.monitor.Draining() {
		t.Fatal("started again for the version a person went back from")
	}
	if a.ixUpdateNow(ctx).AutoNote == "" {
		t.Fatal("the page does not say why")
	}
}

// An automatic drain that takes too long goes back to downloading, and tries
// again later.
func TestIxAutoUpdateGivesUpAndTriesLater(t *testing.T) {
	a, ctx, _ := ixAutoSetup(t, "99.0.0", true, true)
	start := time.Now()
	// Hold the watch back so it does not exit: as if files were in progress.
	box := a.ixUpdates()
	box.mu.Lock()
	box.watching = true
	box.mu.Unlock()
	a.ixAutoUpdateStep(ctx, start)
	if !a.monitor.Draining() {
		t.Fatal("no drain")
	}
	a.ixAutoUpdateStep(ctx, start.Add(ixAutoDrainLimit-time.Second))
	if !a.monitor.Draining() {
		t.Fatal("gave up too early")
	}
	a.ixAutoUpdateStep(ctx, start.Add(ixAutoDrainLimit))
	if a.monitor.Draining() {
		t.Fatal("still holding the downloads after the limit")
	}
	if a.ixUpdateNow(ctx).AutoNote == "" {
		t.Fatal("the page does not say it gave up")
	}
	a.ixAutoUpdateStep(ctx, start.Add(ixAutoDrainLimit+ixAutoRetryAfter/2))
	if a.monitor.Draining() {
		t.Fatal("tried again too soon")
	}
	a.ixAutoUpdateStep(ctx, start.Add(ixAutoDrainLimit+ixAutoRetryAfter))
	if !a.monitor.Draining() {
		t.Fatal("did not try again")
	}
	box.mu.Lock()
	box.watching = false
	box.mu.Unlock()
}
