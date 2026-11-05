package telegram

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ix.12.3: tries judged by how fast Telegram answers a part, with fallbacks.
//
// paceSimDemand is a datacenter that answers each part in partLat while at
// most capParts are in flight; past that, parts queue and each takes
// proportionally longer (the line or Telegram is the limit, so more parts in
// flight bring nothing). Telegram answers FLOOD_WAIT past floodAt. Demand
// varies: files start and finish, so in each interval only a share between
// 1-demandNoise and 1 of the window is in use, though downloads waited for
// room at some point. withAnswers false gives the pacer no answer times, as
// if every try were judged by bytes a second.
func paceSimDemand(seed int64, capParts, floodAt, ticks int, demandNoise float64, withAnswers bool) (windows []int, floods int, bytes float64) {
	rnd := rand.New(rand.NewSource(seed))
	p := newDownloadPacer("")
	clock := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return clock }
	const dc = 4
	const partLat = 0.25 // seconds
	const part = 1 << 20
	for i := 0; i < ticks; i++ {
		p.mu.Lock()
		d := p.dcLocked(dc)
		w := d.window
		used := float64(w) * (1 - demandNoise*rnd.Float64())
		lat := partLat
		if used > float64(capParts) {
			lat = partLat * used / float64(capParts)
		}
		parts := used / lat * paceInterval.Seconds()
		d.waited = true
		d.bytes = int64(parts * part)
		bytes += parts * part
		if withAnswers {
			d.answers = int(parts)
			d.took = time.Duration(parts * lat * float64(time.Second))
		}
		d.lastUse = clock
		if w > floodAt {
			d.floods++
			floods++
		}
		p.mu.Unlock()
		clock = clock.Add(paceInterval)
		p.tick()
		windows = append(windows, w)
	}
	return windows, floods, bytes
}

// The line takes 8 parts; demand varies by up to 40% from one interval to the
// next. Judged by answer time, the window finds the line's 8, and does no
// worse than judged by bytes.
func TestPacerJudgedByAnswersHoldsUnderVaryingDemand(t *testing.T) {
	for seed := int64(1); seed <= 5; seed++ {
		w, floods, b := paceSimDemand(seed, 8, 200, 2160, 0.4, true) // six hours
		ow, _, ob := paceSimDemand(seed, 8, 200, 2160, 0.4, false)
		m, om := mean(w[180:]), mean(ow[180:])
		t.Logf("seed %d: by answers window %.1f (%.1f MB/s), by bytes %.1f (%.1f MB/s), %d floods", seed, m, b/6/3600/1e6, om, ob/6/3600/1e6, floods)
		if m < 6.5 || floods != 0 {
			t.Fatalf("by answers the window settles at %.1f with %d floods", m, floods)
		}
		if b < ob*0.97 {
			t.Fatalf("judging by answers brought less: %.1f against %.1f MB/s", b/6/3600/1e6, ob/6/3600/1e6)
		}
	}
}

// The line tops out at 4 parts: more only make every part wait longer, so the
// window must not climb far past it.
func TestPacerJudgedByAnswersStopsAtTheLine(t *testing.T) {
	for seed := int64(1); seed <= 5; seed++ {
		w, _, _ := paceSimDemand(seed, 4, 200, 2160, 0.3, true)
		m := mean(w[180:])
		t.Logf("seed %d: window %.1f", seed, m)
		if m < 3 || m > 6 {
			t.Fatalf("window settles at %.1f with a line of 4", m)
		}
	}
}

// Telegram floods past 5 parts. The fallback (two cuts in an hour: judged by
// bytes for two hours, under the window that flooded) keeps the waits down:
// without it this model gave 36 floods in six hours.
func TestPacerJudgedByAnswersStaysUnderAFloodLimit(t *testing.T) {
	for seed := int64(1); seed <= 5; seed++ {
		w, floods, _ := paceSimDemand(seed, 30, 5, 2160, 0.3, true)
		m := mean(w[180:])
		t.Logf("seed %d: window %.1f, %d floods in six hours", seed, m, floods)
		if m > 5.5 || floods > 24 {
			t.Fatalf("window %.1f, %d floods", m, floods)
		}
	}
}

// Two cuts for Telegram's waits within an hour: judged by bytes for two
// hours, and kept under the window that flooded all that time.
func TestPacerFallsBackAfterTwoCutsInAnHour(t *testing.T) {
	p := newDownloadPacer("")
	clock := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return clock }
	flood := func() {
		p.mu.Lock()
		d := p.dcLocked(4)
		d.window, d.floods, d.waited, d.lastUse = 6, 1, true, clock
		p.mu.Unlock()
		clock = clock.Add(paceInterval)
		p.tick()
	}
	flood()
	p.mu.Lock()
	if !p.dcLocked(4).bytesUntil.IsZero() {
		t.Fatal("fell back after one cut")
	}
	p.mu.Unlock()
	clock = clock.Add(5 * time.Minute)
	flood()
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.dcLocked(4)
	if until := d.bytesUntil.Sub(clock); until < paceFallbackFor-paceInterval || until > paceFallbackFor {
		t.Fatalf("judged by bytes for %v after two cuts", until)
	}
	if d.ceiling != 6 || d.ceilingUntil.Before(d.bytesUntil) {
		t.Fatalf("ceiling %d until %v, bytes until %v", d.ceiling, d.ceilingUntil, d.bytesUntil)
	}
}

// Answer time says a bigger window is faster, but the bytes drop (Telegram
// slowing the account down some other way): five minutes after the climb it
// is taken back, and judged by bytes for two hours.
func TestPacerTakesBackAClimbTheBytesDoNotBearOut(t *testing.T) {
	p := newDownloadPacer("")
	clock := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return clock }
	p.mu.Lock()
	p.dcLocked(4).window = 3
	p.mu.Unlock()
	took := map[int]bool{}
	for i := 0; i < 360; i++ { // an hour
		p.mu.Lock()
		d := p.dcLocked(4)
		w := d.window
		took[w] = true
		parts := float64(w) / 0.25 * paceInterval.Seconds() // answers stay as fast
		b := 3.0 / 0.25 * paceInterval.Seconds() * (1 << 20)
		if w > 3 {
			b *= 0.7 // but past 3 parts the bytes drop
		}
		d.waited, d.lastUse = true, clock
		d.bytes, d.answers, d.took = int64(b), int(parts), time.Duration(parts*0.25*float64(time.Second))
		p.mu.Unlock()
		clock = clock.Add(paceInterval)
		p.tick()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.dcLocked(4)
	if !took[4] {
		t.Fatal("never climbed: the test does not test the check")
	}
	if d.bytesUntil.IsZero() || d.window > 3 {
		t.Fatalf("window %d, judged by bytes until %v (state %q)", d.window, d.bytesUntil, d.state)
	}
}

// From one part, where the live windows had drifted, it climbs back.
func TestPacerClimbsBackFromOnePart(t *testing.T) {
	p := newDownloadPacer("")
	clock := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return clock }
	p.mu.Lock()
	p.dcLocked(4).window = 1
	p.mu.Unlock()
	for i := 0; i < 90; i++ { // fifteen minutes
		p.mu.Lock()
		d := p.dcLocked(4)
		parts := float64(d.window) / 0.25 * paceInterval.Seconds()
		d.waited, d.lastUse = true, clock
		d.bytes, d.answers, d.took = int64(parts*(1<<20)), int(parts), time.Duration(parts*0.25*float64(time.Second))
		p.mu.Unlock()
		clock = clock.Add(paceInterval)
		p.tick()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if w := p.dcLocked(4).window; w < 6 {
		t.Fatalf("after fifteen minutes from one part, the window is %d", w)
	}
}

// A window saved below the start comes back at the start.
func TestPacerStartsNoLowerThanItsStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.pace.json")
	if err := os.WriteFile(path, []byte(`{"1":1,"4":1,"5":6}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newDownloadPacer(path)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dcLocked(1).window != paceStart || p.dcLocked(4).window != paceStart || p.dcLocked(5).window != 6 {
		t.Fatalf("windows %d %d %d", p.dcLocked(1).window, p.dcLocked(4).window, p.dcLocked(5).window)
	}
}

// The hour-by-hour record samples each server's large-file slots.
func TestPacerHoursRecordLargeFileSlots(t *testing.T) {
	p := newDownloadPacer("")
	clock := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return clock }
	p.slots = func(dc int) (int, int, int) { return 2, 5, 2 }
	for i := 0; i < 3; i++ {
		p.mu.Lock()
		d := p.dcLocked(4)
		d.bytes, d.waited, d.lastUse = 1<<20, true, clock
		p.mu.Unlock()
		clock = clock.Add(paceInterval)
		p.tick()
	}
	h := p.hoursNewestFirst()[0]
	if h.DCSamples[4] != 3 || h.DCLarge[4] != 6 || h.DCLargeWaiting[4] != 15 || h.DCLargeLimit[4] != 6 {
		t.Fatalf("%v %v %v %v", h.DCSamples, h.DCLarge, h.DCLargeWaiting, h.DCLargeLimit)
	}
}
