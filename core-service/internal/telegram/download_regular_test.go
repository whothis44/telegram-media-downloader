package telegram

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

type regularParts struct{ asked map[int64]int }

func (p *regularParts) Close() error { return nil }
func (p *regularParts) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	r := input.(*tg.UploadGetFileRequest)
	p.asked[r.Offset]++
	// A later part repeatedly hits the regular-account limit. Earlier
	// successfully written bytes must not be fetched all over again.
	if r.Offset == downloadPartSize && p.asked[r.Offset] <= 6 {
		return &tgerr.Error{Code: 420, Type: "FLOOD_PREMIUM_WAIT", Argument: 65}
	}
	size := downloadPartSize
	if r.Offset > 0 {
		size = 17
	}
	output.(*tg.UploadFileBox).File = &tg.UploadFile{Bytes: bytes.Repeat([]byte{byte(r.Offset/downloadPartSize + 1)}, size)}
	return nil
}

func TestRegularWaitsPreserveTransferProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newDownloadPacer("")
		pool := &regularParts{asked: map[int64]int{}}
		inv := sharedInvoker{pool: pool, guard: newFloodGuard(), pacer: p, dc: 4}
		var paused, resumes int
		ctx := WithDownloadWait(context.Background(), func() func() {
			paused++
			return func() { resumes++ }
		})
		var out bytes.Buffer
		err := fetchParallel(ctx, tg.NewClient(inv), &tg.InputDocumentFileLocation{ID: 1}, downloadPartSize+17, 1, &out)
		if err != nil {
			t.Fatal(err)
		}
		if pool.asked[0] != 1 || pool.asked[downloadPartSize] != 7 || out.Len() != downloadPartSize+17 {
			t.Fatalf("lost/repeated data: requests=%v bytes=%d", pool.asked, out.Len())
		}
		if !bytes.Equal(out.Bytes()[:downloadPartSize], bytes.Repeat([]byte{1}, downloadPartSize)) || !bytes.Equal(out.Bytes()[downloadPartSize:], bytes.Repeat([]byte{2}, 17)) {
			t.Fatal("corrupt transfer")
		}
		if paused != 6 || resumes != paused {
			t.Fatalf("pause/resume %d/%d", paused, resumes)
		}
		st := p.stats()
		if st.Premium1h != 6 || st.Floods1h != 0 || st.DCs[0].GateWaitSecs < 390 || st.DCs[0].InFlight != 0 {
			t.Fatalf("stats: %+v", st)
		}
	})
}

func TestRegularWaitCancellationReleasesBudgetAndSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newDownloadPacer("")
		g := newFloodGuard()
		pool := &scriptedInvoker{errs: []error{&tgerr.Error{Code: 420, Type: "FLOOD_PREMIUM_WAIT", Argument: 3600}}}
		inv := sharedInvoker{pool: scriptedPool{pool}, guard: g, pacer: p, dc: 4}
		ctx, cancel := context.WithCancel(context.Background())
		var pauses atomic.Int32
		ctx = WithDownloadWait(ctx, func() func() { pauses.Add(1); return func() { pauses.Add(-1) } })
		done := make(chan error, 1)
		go func() { var box tg.UploadFileBox; done <- inv.Invoke(ctx, &tg.UploadGetFileRequest{}, &box) }()
		synctest.Wait()
		if pauses.Load() != 1 {
			t.Fatal("wait did not pause transfer budget")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
		if pauses.Load() != 0 || p.stats().DCs[0].InFlight != 0 || pool.calls != 1 {
			t.Fatal("cancellation leaked a slot/budget or retried")
		}
	})
}

func TestRegularWaitTakesPrecedenceOverSlowAnswers(t *testing.T) {
	p := newDownloadPacer("")
	clock := time.Unix(1800000000, 0)
	p.now = func() time.Time { return clock }
	d := p.dcLocked(4)
	d.window, d.trial, d.trialFrom = 8, 1, 7
	d.steady = []float64{100, 100, 100}
	p.answered(4, 2*time.Minute)
	p.flood(4, true)
	clock = clock.Add(paceInterval)
	p.tick()
	if d.window != 7 || d.trial != 0 || len(d.steady) != 0 {
		t.Fatalf("premium wait shrank stable window: %+v", d)
	}
	// A slow response in the recovery hold must not trigger another cut.
	p.answered(4, 2*time.Minute)
	clock = clock.Add(paceInterval)
	p.tick()
	if d.window != 7 {
		t.Fatal("recovery wait shrank window")
	}
}

func TestRegularAccountClampsOldSavedWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pace.json")
	if err := os.WriteFile(path, []byte(`{"4":32}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := newDownloadPacer(path).dcLocked(4).window; got != paceMax {
		t.Fatalf("window=%d", got)
	}
}

func TestRegularPacerDoesNotLearnDuringLongWait(t *testing.T) {
	p := newDownloadPacer("")
	clock := time.Unix(1800000000, 0)
	p.now = func() time.Time { return clock }
	d := p.dcLocked(4)
	d.lastUse = clock
	d.window = 6
	p.waitingOnTelegram(4, true)
	for range 120 {
		d.waited = true
		clock = clock.Add(paceInterval)
		p.tick()
		if d.window != 6 || d.trial != 0 {
			t.Fatal("learned a new window from a Telegram wait")
		}
	}
	p.waitingOnTelegram(4, false)
	if p.stats().DCs[0].GateWaiting != 0 {
		t.Fatal("wait count leaked")
	}
}

func TestRegularPacerPersistsOnlyStableWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pace.json")
	p := newDownloadPacer(path)
	clock := time.Unix(1800000000, 0)
	p.now = func() time.Time { return clock }
	d := p.dcLocked(4)
	d.window, d.trial, d.trialFrom = 5, 1, 4
	d.trialBase, d.trialLeft = 100, 1
	p.saveLocked()
	if got := newDownloadPacer(path).dcLocked(4).window; got != 4 {
		t.Fatalf("persisted unproven trial: %d", got)
	}
	d.bytes, d.waited = 1250, true
	clock = clock.Add(paceInterval)
	p.tick()
	if d.trial != 0 || d.window != 5 {
		t.Fatal("successful trial was not accepted")
	}
	if got := newDownloadPacer(path).dcLocked(4).window; got != 5 {
		t.Fatalf("accepted trial not persisted: %d", got)
	}
}

func fileConfig(small, large float64) tg.JSONValueClass {
	return &tg.JSONObject{Value: []tg.JSONObjectValue{
		{Key: "small_queue_max_active_operations_count", Value: &tg.JSONNumber{Value: small}},
		{Key: "large_queue_max_active_operations_count", Value: &tg.JSONNumber{Value: large}},
	}}
}

func TestDownloadFileLimitsSeparateDCsAndSizes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDownloadFiles()
		f.configure(fileConfig(1, 1))
		small, err := f.acquire(context.Background(), 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer small()
		large, err := f.acquire(context.Background(), 1, downloadLargeFile)
		if err != nil {
			t.Fatal(err)
		}
		defer large()
		other, err := f.acquire(context.Background(), 2, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer other()
		ctx, cancel := context.WithCancel(context.Background())
		var pauses atomic.Int32
		ctx = WithDownloadWait(ctx, func() func() { pauses.Add(1); return func() { pauses.Add(-1) } })
		done := make(chan error, 1)
		go func() {
			release, err := f.acquire(ctx, 1, 1)
			if release != nil {
				release()
			}
			done <- err
		}()
		synctest.Wait()
		if pauses.Load() != 1 {
			t.Fatal("same class did not wait")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel=%v", err)
		}
		if pauses.Load() != 0 {
			t.Fatal("wait budget leaked")
		}
		// Raising the advertised limit wakes existing waiters.
		go func() {
			release, err := f.acquire(context.Background(), 1, 1)
			if release != nil {
				release()
			}
			done <- err
		}()
		synctest.Wait()
		f.configure(fileConfig(2, 1))
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		f.configure(fileConfig(-1, 1.5))
		if f.small != 2 || f.large != 1 {
			t.Fatal("invalid config replaced valid limits")
		}
	})
}
