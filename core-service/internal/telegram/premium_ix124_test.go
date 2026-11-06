package telegram

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gotd/td/tgerr"
)

func premiumErr(seconds int) error {
	return &tgerr.Error{Code: 420, Type: "FLOOD_PREMIUM_WAIT", Argument: seconds}
}

// ix.12.4: a speed-limit wait holds only the request that got it. Another
// file's part goes at once, and the account is not reported as waiting, so
// the engine may start files meanwhile.
func TestPremiumWaitHoldsOnlyItsOwnRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newFloodGuard()
		slow := &scriptedInvoker{errs: []error{premiumErr(10)}}
		var waits, waited int
		hooks := partHooks{waiting: func(on bool) {
			if on {
				waits++
			}
		}, waited: func(time.Duration) { waited++ }}
		started := time.Now()
		done := make(chan error, 1)
		go func() { done <- g.invokeFiles(context.Background(), slow, nil, nil, hooks) }()
		synctest.Wait()
		if !g.FilesWaitUntil().IsZero() || !g.FloodWaitUntil().IsZero() || g.SpeedLimitedOnly() {
			t.Fatal("a speed-limit wait closed a shared gate")
		}
		other := &scriptedInvoker{}
		if err := g.invokeFiles(context.Background(), other, nil, nil, partHooks{}); err != nil || other.calls != 1 || time.Since(started) != 0 {
			t.Fatalf("another file's part waited: err=%v calls=%d after %s", err, other.calls, time.Since(started))
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if slow.calls != 2 || time.Since(started) < 10*time.Second || waits != 1 || waited != 1 {
			t.Fatalf("calls=%d after %s waits=%d waited=%d", slow.calls, time.Since(started), waits, waited)
		}
		if per, _, fb := g.PremiumMode(); !per || len(fb) != 0 {
			t.Fatalf("mode per-request=%v fallbacks=%v", per, fb)
		}
	})
}

func TestPremiumWaitCancelsAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newFloodGuard()
		next := &scriptedInvoker{errs: []error{premiumErr(3600)}}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- g.invokeFiles(ctx, next, nil, nil, partHooks{}) }()
		synctest.Wait()
		cancel()
		if err := <-done; err != context.Canceled || next.calls != 1 {
			t.Fatalf("err=%v calls=%d", err, next.calls)
		}
	})
}

// A plain FLOOD_WAIT on a file part goes back to the shared gate for two
// hours: then a speed-limit wait stops every file again, as in ix.12.3.
func TestPlainFileWaitFallsBackToTheSharedGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newFloodGuard()
		if err := g.invokeFiles(context.Background(), &scriptedInvoker{errs: []error{floodErr(1)}}, nil, nil, partHooks{}); err != nil {
			t.Fatal(err)
		}
		per, until, fb := g.PremiumMode()
		if per || until.Sub(time.Now()) < time.Hour || fb[FallbackPlainWait] != 1 {
			t.Fatalf("mode per-request=%v until=%s fallbacks=%v", per, until, fb)
		}
		time.Sleep(time.Second) // past the plain wait's gate
		done := make(chan error, 1)
		go func() {
			done <- g.invokeFiles(context.Background(), &scriptedInvoker{errs: []error{premiumErr(5)}}, nil, nil, partHooks{})
		}()
		synctest.Wait()
		if g.FilesWaitUntil().IsZero() || !g.SpeedLimitedOnly() {
			t.Fatal("the fallback did not close the shared gate")
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		time.Sleep(premiumFallbackFor)
		if per, _, _ := g.PremiumMode(); !per {
			t.Fatal("the fallback did not end")
		}
	})
}

// When most file requests are refused, the shared gate comes back too.
func TestMostlyRefusedPartsFallBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		min := premiumRecentMin
		premiumRecentMin = 4
		t.Cleanup(func() { premiumRecentMin = min })
		g := newFloodGuard()
		g.partAnswered(time.Now())
		for i := 0; i < 3; i++ {
			g.partPremium(time.Now())
		}
		if per, _, _ := g.PremiumMode(); !per {
			t.Fatal("fell back before enough waits")
		}
		g.partPremium(time.Now())
		if per, _, fb := g.PremiumMode(); per || fb[FallbackRefused] != 1 {
			t.Fatalf("no fallback: %v", fb)
		}
	})
}

func TestMostlyAnsweredPartsKeepPerRequestWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		min := premiumRecentMin
		premiumRecentMin = 4
		t.Cleanup(func() { premiumRecentMin = min })
		g := newFloodGuard()
		for i := 0; i < 6; i++ {
			g.partAnswered(time.Now())
			g.partPremium(time.Now())
		}
		if per, _, _ := g.PremiumMode(); !per {
			t.Fatal("one wait per part answered is the usual speed limit, not refusal")
		}
	})
}

// While the speed limit is on, a server keeps at least 2 parts in flight
// (Telegram's own apps keep 2 MiB per server without Premium); not after a
// recent plain FLOOD_WAIT.
func TestSpeedLimitKeepsTwoPartsPerServer(t *testing.T) {
	p := newDownloadPacer("")
	clock := time.Unix(1800000000, 0)
	p.now = func() time.Time { return clock }
	d := p.dcLocked(1)
	d.window = 1
	p.flood(1, true)
	clock = clock.Add(paceInterval)
	p.tick()
	if d.window != paceLimitedFloor {
		t.Fatalf("window=%d", d.window)
	}

	e := p.dcLocked(2)
	e.window, e.lastCut = 1, clock.Add(-time.Minute)
	p.flood(2, true)
	clock = clock.Add(paceInterval)
	p.tick()
	if e.window != 1 {
		t.Fatalf("raised right after a plain flood: %d", e.window)
	}

	f := p.dcLocked(4)
	f.window = 5
	p.flood(4, true)
	clock = clock.Add(paceInterval)
	p.tick()
	if f.window != 5 {
		t.Fatalf("a bigger window changed: %d", f.window)
	}
}

func TestHourRecordsAskedSecondsAndSameFile(t *testing.T) {
	p := newDownloadPacer("")
	clock := time.Unix(1800000000, 0)
	p.now = func() time.Time { return clock }
	p.floodFor(1, true, 14*time.Second, 77)
	p.floodFor(1, true, 15*time.Second, 77)
	p.floodFor(1, true, 15*time.Second, 78)
	p.floodFor(4, true, 3*time.Second, 0)
	p.floodFor(4, true, 3*time.Second, 0)
	h := p.hoursNewestFirst()[0]
	if h.Premium != 5 || h.DCPremium[1] != 3 || h.DCPremiumSecs[1] != 44 || h.DCPremiumSame[1] != 1 || h.DCPremiumSecs[4] != 6 || h.DCPremiumSame[4] != 0 {
		t.Fatalf("hour: %+v", h)
	}
}
