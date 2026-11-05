package telegram

import (
	"context"
	"io"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// paceSim drives a pacer against a simulated Telegram: a datacenter that
// moves partRate bytes per part a second up to cap parts (the line or the
// disk is the limit beyond), and answers FLOOD_WAIT whenever more than
// floodAt parts are in flight. noise (0.15 = ±15%) varies each interval's
// speed at random, as a real line does.
var paceSimSeed int64 = 7

func paceSim(t *testing.T, cap, floodAt, ticks int, noise float64) (windows []int, floods int) {
	t.Helper()
	rnd := rand.New(rand.NewSource(paceSimSeed))
	p := newDownloadPacer("")
	clock := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return clock }
	const dc = 4
	const partRate = 400_000.0
	for i := 0; i < ticks; i++ {
		p.mu.Lock()
		d := p.dcLocked(dc)
		w := d.window
		d.waited = true // always more to download than room
		d.bytes = int64(float64(min(w, cap)) * partRate * paceInterval.Seconds() * (1 + noise*(2*rnd.Float64()-1)))
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
	return windows, floods
}

func mean(xs []int) float64 {
	s := 0
	for _, x := range xs {
		s += x
	}
	return float64(s) / float64(len(xs))
}

func TestPacerFindsTheSpeedLimitWithoutFlooding(t *testing.T) {
	// The line tops out at 6 parts; Telegram would allow 20.
	w, floods := paceSim(t, 6, 20, 360, 0) // an hour
	tail := w[60:]
	t.Logf("window %.1f, %d floods", mean(tail), floods)
	if m := mean(tail); m < 5.5 || m > 7.5 || floods != 0 {
		t.Fatalf("window settles at %.1f with %d floods: %v", m, floods, tail)
	}
}

func TestPacerStaysUnderTelegramsLimit(t *testing.T) {
	// Telegram floods past 8 parts in flight; the line could take 30.
	w, floods := paceSim(t, 30, 8, 360, 0)
	tail := w[60:]
	t.Logf("window %.1f, %d floods", mean(tail), floods)
	if m := mean(tail); m < 5 || m > 8.5 {
		t.Fatalf("window settles at %.1f: %v", m, tail)
	}
	if floods > 8 { // about one an ten minutes at most
		t.Fatalf("%d floods in an hour", floods)
	}
}

func TestPacerIsNotFooledByANoisyLine(t *testing.T) {
	// The line tops out at 6 parts, but each interval's speed varies by
	// ±15%: lucky intervals must not ratchet the window up.
	defer func() { paceSimSeed = 7 }()
	for seed := int64(1); seed <= 5; seed++ {
		paceSimSeed = seed
		w, floods := paceSim(t, 6, 200, 2160, 0.15) // six hours
		tail := w[180:]
		hi := 0
		for _, x := range tail {
			hi = max(hi, x)
		}
		t.Logf("window %.1f (at most %d), %d floods", mean(tail), hi, floods)
		if m := mean(tail); m < 4.5 || m > 9 || hi > 14 {
			t.Fatalf("window settles at %.1f, at most %d", m, hi)
		}
	}
}

func TestPacerFindsTheLimitOnANoisyLine(t *testing.T) {
	w, floods := paceSim(t, 30, 8, 2160, 0.15)
	tail := w[180:]
	t.Logf("window %.1f, %d floods", mean(tail), floods)
	if m := mean(tail); m < 4.5 || m > 8.5 || floods > 6*8 {
		t.Fatalf("window settles at %.1f with %d floods", m, floods)
	}
}

func TestPacerCutsOnceForAFloodOfAnswers(t *testing.T) {
	p := newDownloadPacer("")
	clock := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return clock }
	p.mu.Lock()
	p.dcLocked(4).window = 20
	p.mu.Unlock()
	// Parts asked for before the cut keep coming back with FLOOD_WAIT for
	// a few intervals: one cut, not one a interval.
	for i := 0; i < 4; i++ {
		p.mu.Lock()
		d := p.dcLocked(4)
		d.waited, d.floods, d.lastUse = true, 3, clock
		p.mu.Unlock()
		clock = clock.Add(paceInterval)
		p.tick()
	}
	p.mu.Lock()
	d := p.dcLocked(4)
	w, ceiling := d.window, d.ceiling
	p.mu.Unlock()
	if w != 12 || ceiling != 20 {
		t.Fatalf("window %d ceiling %d after a burst of floods, want 12 under 20", w, ceiling)
	}
	// A flood at a small window still leaves room to work under it.
	p.mu.Lock()
	d.window = 2
	d.lastCut = time.Time{}
	d.floods = 1
	p.mu.Unlock()
	p.tick()
	p.mu.Lock()
	defer p.mu.Unlock()
	if d.window != 1 || d.ceiling != paceCeilingMin {
		t.Fatalf("window %d ceiling %d", d.window, d.ceiling)
	}
}

func TestPacerSuggestsWorkersOnlyWhileItsWindowsAreFull(t *testing.T) {
	p := newDownloadPacer("")
	clock := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return clock }
	p.mu.Lock()
	d := p.dcLocked(2)
	d.lastUse, d.bytes = clock, 1<<20
	p.mu.Unlock()
	clock = clock.Add(paceInterval)
	p.tick()
	if n := p.suggestedWorkers(); n != 0 {
		t.Fatalf("windows never full, suggested %d", n)
	}
	p.mu.Lock()
	d.waited, d.lastUse = true, clock
	p.mu.Unlock()
	clock = clock.Add(paceInterval)
	p.tick()
	if n := p.suggestedWorkers(); n != 3*paceStart+2 {
		t.Fatalf("full window, suggested %d", n)
	}
	SetPacing(false)
	defer SetPacing(true)
	if n := p.suggestedWorkers(); n != 0 {
		t.Fatalf("pacing off, suggested %d", n)
	}
}

func TestPacerRunsOnce(t *testing.T) {
	p := newDownloadPacer("")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	second := make(chan struct{})
	go func() { p.run(ctx); close(second) }()
	select {
	case <-second: // returned at once: already running
	case <-time.After(time.Second):
		t.Fatal("a second run loop started")
	}
	cancel()
	<-done
}

func TestPacerWindowsLimitPartsInFlight(t *testing.T) {
	p := newDownloadPacer("")
	p.mu.Lock()
	p.dcLocked(2).window = 2
	p.mu.Unlock()
	ctx := context.Background()
	if p.acquire(ctx, 2) != nil || p.acquire(ctx, 2) != nil {
		t.Fatal("first two")
	}
	got := make(chan error, 1)
	go func() { got <- p.acquire(ctx, 2) }()
	select {
	case <-got:
		t.Fatal("a third part went past a window of 2")
	case <-time.After(50 * time.Millisecond):
	}
	p.release(2, 1<<20)
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	go func() { got <- p.acquire(cancelled, 2) }()
	cancel()
	if err := <-got; err == nil {
		t.Fatal("waiting was not cancelled")
	}
	// Other datacenters have their own room.
	if p.acquire(ctx, 5) != nil {
		t.Fatal("dc 5")
	}
	st := p.stats()
	if len(st.DCs) != 2 || st.DCs[0].InFlight != 2 || st.DCs[0].Waiting != 0 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestPacerRemembersWhatItLearned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.pace.json")
	p := newDownloadPacer(path)
	p.mu.Lock()
	d := p.dcLocked(4)
	d.window = 4 // persistence is independent of the startup default
	d.waited, d.bytes, d.floods = true, 1<<20, 1
	p.mu.Unlock()
	p.tick() // a flood: 4 → 2
	q := newDownloadPacer(path)
	q.mu.Lock()
	defer q.mu.Unlock()
	if w := q.dcLocked(4).window; w != 2 {
		t.Fatalf("window after restart: %d", w)
	}
	if w := q.dcLocked(1).window; w != paceStart {
		t.Fatalf("new datacenter: %d", w)
	}
}

type partPool struct {
	mu     sync.Mutex
	flood  int
	served int
	main   bool
}

func (p *partPool) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := input.(*tg.UploadGetFileRequest); !ok {
		p.main = true
		return nil
	}
	if p.flood > 0 {
		p.flood--
		return floodErr(1)
	}
	p.served++
	output.(*tg.UploadFileBox).File = &tg.UploadFile{Bytes: make([]byte, 1000)}
	return nil
}
func (p *partPool) Close() error { return nil }

func TestPacedPartsCountBytesAndFloodsOnTheFilesGate(t *testing.T) {
	withFastFlood(t)
	g := newFloodGuard()
	p := newDownloadPacer("")
	pool := &partPool{flood: 1}
	inv := sharedInvoker{pool: pool, guard: g, pacer: p, dc: 4}
	var box tg.UploadFileBox
	if err := inv.Invoke(context.Background(), &tg.UploadGetFileRequest{Limit: 1 << 20}, &box); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	d := p.dcLocked(4)
	bytes, floods, inflight := d.bytes, d.floods, d.inflight
	p.mu.Unlock()
	if bytes != 1000 || floods != 1 || inflight != 0 || pool.served != 1 {
		t.Fatalf("bytes %d floods %d inflight %d served %d", bytes, floods, inflight, pool.served)
	}
	// The flood closed the files gate, not the account's.
	if !g.FloodWaitUntil().IsZero() {
		t.Fatal("a file flood closed the account gate")
	}
	// Other requests on the pool are not paced.
	if err := inv.Invoke(context.Background(), &tg.HelpGetConfigRequest{}, nil); err != nil || !pool.main {
		t.Fatal("other request")
	}
}

// lostOnce fails its first part as if the connection had dropped.
type lostOnce struct {
	partPool
	lost  bool
	pacer *downloadPacer
	held  int
}

func (p *lostOnce) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	p.mu.Lock()
	if !p.lost {
		p.lost = true
		p.mu.Unlock()
		return io.EOF
	}
	p.mu.Unlock()
	p.pacer.mu.Lock()
	p.held = p.pacer.dcLocked(4).inflight
	p.pacer.mu.Unlock()
	return p.partPool.Invoke(ctx, input, output)
}

func TestPacedPartGivesItsRoomBackWhileItsConnectionIsReplaced(t *testing.T) {
	old := connectionRetryDelay
	connectionRetryDelay = 30 * time.Millisecond
	defer func() { connectionRetryDelay = old }()
	p := newDownloadPacer("")
	p.mu.Lock()
	p.dcLocked(4).window = 1
	p.mu.Unlock()
	pool := &lostOnce{pacer: p}
	inv := sharedInvoker{pool: pool, guard: newFloodGuard(), pacer: p, dc: 4}
	done := make(chan error, 1)
	go func() {
		var box tg.UploadFileBox
		done <- inv.Invoke(context.Background(), &tg.UploadGetFileRequest{Limit: 1 << 20}, &box)
	}()
	// While the lost part backs off, another part may use the window.
	time.Sleep(10 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.acquire(ctx, 4); err != nil {
		t.Fatal("the window stayed taken while the connection was replaced")
	}
	p.release(4, 0)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if pool.held != 1 {
		t.Fatalf("the retried part was not counted in flight: %d", pool.held)
	}
}

func TestUnpacedPartsGoAsBefore(t *testing.T) {
	withFastFlood(t)
	SetPacing(false)
	defer SetPacing(true)
	g := newFloodGuard()
	p := newDownloadPacer("")
	pool := &partPool{flood: 1}
	inv := sharedInvoker{pool: pool, guard: g, pacer: p, dc: 4}
	var box tg.UploadFileBox
	if err := inv.Invoke(context.Background(), &tg.UploadGetFileRequest{Limit: 1 << 20}, &box); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	n := len(p.dcs)
	p.mu.Unlock()
	if n != 0 || pool.served != 1 {
		t.Fatalf("paced while off: %d datacenters, %d served", n, pool.served)
	}
	// As in ix.8, the flood closed the account's gate, not the files gate.
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.until.IsZero() || !g.filesUntil.IsZero() {
		t.Fatal("a file flood did not close the account gate")
	}
}

func TestPremiumWaitsKeepTheWindowAndPauseTries(t *testing.T) {
	p := newDownloadPacer("")
	clock := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return clock }
	p.mu.Lock()
	d := p.dcLocked(4)
	d.window, d.trial, d.trialFrom, d.trialLeft = 9, 1, 8, 2
	d.waited, d.lastUse = true, clock
	p.mu.Unlock()
	p.flood(4, true)
	clock = clock.Add(paceInterval)
	p.tick()
	p.mu.Lock()
	w, trial, ceiling, hold := d.window, d.trial, d.ceiling, d.holdUntil
	p.mu.Unlock()
	if w != 8 || trial != 0 || ceiling != 0 || !hold.After(clock) {
		t.Fatalf("window %d trial %d ceiling %d hold %v", w, trial, ceiling, hold)
	}
	st := p.stats()
	if st.Premium1h != 1 || st.Floods1h != 0 || st.DCs[0].Premium1h != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestFloodGuardTellsPremiumWaitsApart(t *testing.T) {
	withFastFlood(t)
	g := newFloodGuard()
	var kinds []bool
	next := &scriptedInvoker{errs: []error{floodErr(1), &tgerr.Error{Code: 420, Type: "FLOOD_PREMIUM_WAIT", Argument: 1}}}
	if err := g.invokeFiles(context.Background(), next, nil, nil, partHooks{flood: func(_ time.Duration, premium bool) { kinds = append(kinds, premium) }}); err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 2 || kinds[0] || !kinds[1] {
		t.Fatalf("kinds %v", kinds)
	}
}

func TestPacerKeepsSlowAnswersUnderEightSeconds(t *testing.T) {
	p := newDownloadPacer("")
	clock := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return clock }
	p.mu.Lock()
	d := p.dcLocked(4)
	d.window, d.waited, d.lastUse, d.bytes = 16, true, clock, 1<<20
	p.mu.Unlock()
	// Parts take 32 s each: at this speed 4 in flight would take 8 s.
	for i := 0; i < 4; i++ {
		p.answered(4, 32*time.Second)
	}
	clock = clock.Add(paceInterval)
	p.tick()
	p.mu.Lock()
	w, ceiling := d.window, d.ceiling
	p.mu.Unlock()
	if w != 4 || ceiling != 0 {
		t.Fatalf("window %d ceiling %d, want 4 and no ceiling", w, ceiling)
	}
	if st := p.stats(); st.DCs[0].PartSecs != 32 {
		t.Fatalf("part time %v", st.DCs[0].PartSecs)
	}
	// Quick answers change nothing.
	p.mu.Lock()
	d.lastCut = time.Time{}
	d.holdUntil = time.Time{}
	p.mu.Unlock()
	p.answered(4, time.Second)
	clock = clock.Add(paceInterval)
	p.tick()
	p.mu.Lock()
	defer p.mu.Unlock()
	if d.window != 4 {
		t.Fatalf("window %d after quick answers", d.window)
	}
}

type silentPool struct{ calls int }

func (p *silentPool) Invoke(ctx context.Context, _ bin.Encoder, output bin.Decoder) error {
	p.calls++
	if p.calls == 1 {
		<-ctx.Done() // never answered
		return ctx.Err()
	}
	output.(*tg.UploadFileBox).File = &tg.UploadFile{Bytes: make([]byte, 10)}
	return nil
}
func (p *silentPool) Close() error { return nil }

func TestAPartTelegramNeverAnswersIsAskedForAgain(t *testing.T) {
	old := filePartTimeout
	filePartTimeout = 30 * time.Millisecond
	defer func() { filePartTimeout = old }()
	p := newDownloadPacer("")
	pool := &silentPool{}
	inv := sharedInvoker{pool: pool, guard: newFloodGuard(), pacer: p, dc: 4}
	// The library's reader asks again after a timeout, as fetchPart does.
	data, err := fetchPart(context.Background(), tg.NewClient(inv), &tg.InputDocumentFileLocation{}, 0)
	if err != nil || len(data) != 10 || pool.calls != 2 {
		t.Fatalf("data %d err %v calls %d", len(data), err, pool.calls)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if d := p.dcLocked(4); d.inflight != 0 || d.answers != 2 || d.took < filePartTimeout {
		t.Fatalf("inflight %d answers %d", d.inflight, d.answers)
	}
}

func TestPacerKeepsAnHourByHourRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.pace.json")
	p := newDownloadPacer(path)
	clock := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return clock }
	for i := 0; i < 360; i++ { // an hour, then into the next
		p.mu.Lock()
		d := p.dcLocked(4)
		d.bytes, d.parts, d.waited, d.lastUse = 1<<20, 1, i%2 == 0, clock
		p.mu.Unlock()
		p.answered(4, 2*time.Second)
		if i == 10 {
			p.flood(4, true)
			p.flood(4, false)
		}
		clock = clock.Add(paceInterval)
		p.tick()
	}
	hs := p.hoursNewestFirst()
	if len(hs) != 2 {
		t.Fatalf("hours %d", len(hs))
	}
	h := hs[1]
	if h.Bytes < 350<<20 || h.Premium != 1 || h.Waits != 1 || h.PartSecs != 2 || h.FullShare < 0.45 || h.FullShare > 0.55 || h.Window < 1 {
		t.Fatalf("hour %+v", h)
	}
	// Kept across a restart.
	p.mu.Lock()
	p.saveHoursLocked()
	p.mu.Unlock()
	q := newDownloadPacer(path)
	if got := q.hoursNewestFirst(); len(got) != 2 || got[1].Premium != 1 || got[1].PartSecs != 2 {
		t.Fatalf("reloaded %+v", got)
	}
}
