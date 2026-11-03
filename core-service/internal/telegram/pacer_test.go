package telegram

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// paceSim drives a pacer against a simulated Telegram: a datacenter that
// moves partRate bytes per part a second up to cap parts (the line or the
// disk is the limit beyond), and answers FLOOD_WAIT whenever more than
// floodAt parts are in flight.
func paceSim(t *testing.T, cap, floodAt, ticks int) (windows []int, floods int) {
	t.Helper()
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
		d.bytes = int64(float64(min(w, cap)) * partRate * paceInterval.Seconds())
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
	w, floods := paceSim(t, 6, 20, 360) // an hour
	tail := w[60:]
	t.Logf("window %.1f, %d floods", mean(tail), floods)
	if m := mean(tail); m < 5.5 || m > 7.5 || floods != 0 {
		t.Fatalf("window settles at %.1f with %d floods: %v", m, floods, tail)
	}
}

func TestPacerStaysUnderTelegramsLimit(t *testing.T) {
	// Telegram floods past 8 parts in flight; the line could take 30.
	w, floods := paceSim(t, 30, 8, 360)
	tail := w[60:]
	t.Logf("window %.1f, %d floods", mean(tail), floods)
	if m := mean(tail); m < 5 || m > 8.5 {
		t.Fatalf("window settles at %.1f: %v", m, tail)
	}
	if floods > 8 { // about one an ten minutes at most
		t.Fatalf("%d floods in an hour", floods)
	}
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
