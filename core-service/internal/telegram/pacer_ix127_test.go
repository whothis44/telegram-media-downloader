package telegram

import (
	"testing"
	"time"
)

// ix.12.7: the hour record says, per server, the window's cuts (for a plain
// wait, for slow answers) and how much of the time it sat at the 2-part floor
// or below; the flood guard keeps when each fallback began and ends.
func TestPacerHourCountsCutsAndFloorPerServer(t *testing.T) {
	p := newDownloadPacer("")
	clock := time.Unix(1_800_000_000, 0).Truncate(time.Hour).Add(time.Minute)
	p.now = func() time.Time { return clock }
	p.mu.Lock()
	p.dcLocked(4).window = 4
	p.dcLocked(2).window = paceLimitedFloor
	p.mu.Unlock()
	step := func(f func(d4, d2 *dcPace)) {
		p.mu.Lock()
		d4, d2 := p.dcLocked(4), p.dcLocked(2)
		d4.waited, d4.bytes, d4.lastUse = true, 1<<20, clock
		d2.waited, d2.bytes, d2.lastUse = true, 1<<20, clock
		if f != nil {
			f(d4, d2)
		}
		p.mu.Unlock()
		clock = clock.Add(paceInterval)
		p.tick()
	}
	step(func(d4, _ *dcPace) { d4.floods = 1 }) // DC4 cut: 4 -> 2
	step(nil)
	step(nil)
	h := p.hoursNewestFirst()[0]
	if h.DCCutsWait[4] != 1 || h.DCCutsWait[2] != 0 || h.DCCutsSlow[4] != 0 {
		t.Fatalf("cuts: wait %v slow %v", h.DCCutsWait, h.DCCutsSlow)
	}
	if h.DCSamples[2] != 3 || h.DCAtFloor[2] != 3 || h.DCBelow[2] != 0 {
		t.Fatalf("DC2 at the floor: samples %v floor %v below %v", h.DCSamples, h.DCAtFloor, h.DCBelow)
	}
	if h.DCAtFloor[4] != 2 { // after its cut
		t.Fatalf("DC4 at the floor %d of %d", h.DCAtFloor[4], h.DCSamples[4])
	}

	g := &floodGuard{}
	now := time.Now()
	g.mu.Lock()
	g.fallBackLocked(now, FallbackPlainWait)
	g.fallBackLocked(now.Add(time.Minute), FallbackPlainWait) // still on: not a new one
	g.mu.Unlock()
	ps := g.FallbackPeriods()
	if len(ps) != 1 || !ps[0].Start.Equal(now) || !ps[0].End.Equal(now.Add(premiumFallbackFor)) || ps[0].Why != FallbackPlainWait {
		t.Fatalf("fallback periods: %+v", ps)
	}
}
