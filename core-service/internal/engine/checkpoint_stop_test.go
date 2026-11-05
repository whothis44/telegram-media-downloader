package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// ix.12.3: an update waiting, or a person pausing a file, stops a download that
// keeps its progress right after its next checkpoint (download.mediaWriter). The
// sink below stands for that transfer: it moves bytes until it is asked to stop,
// then ends with telegram.ErrDownloadStopped, as the library does.

type stopRow struct {
	status         string
	attempts       int64
	paused         int64
	retryAt, calls int64
}

func readStopRow(t *testing.T, s *WorkStore, calls *atomic.Int64) stopRow {
	t.Helper()
	var r stopRow
	if err := s.reader.QueryRow(`SELECT status,attempts,paused,retry_at FROM tgdl_work`).Scan(&r.status, &r.attempts, &r.paused, &r.retryAt); err != nil {
		t.Fatal(err)
	}
	r.calls = calls.Load()
	return r
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// stoppingController runs one worker whose transfers move bytes until asked to
// stop. finish, when set, makes a transfer end well instead (its next attempt).
func stoppingController(t *testing.T, s *WorkStore) (*Controller, *atomic.Int64, *atomic.Bool, *atomic.Bool) {
	t.Helper()
	factory := func(AccountConfig, *telegram.UpdateState, func(context.Context, tg.UpdatesClass) error, func(int64)) (Account, error) {
		return &readyAccount{}, nil
	}
	filter := func(context.Context, string, *tg.Message, tg.UpdatesClass) (Target, bool, error) {
		return Target{}, true, nil
	}
	var calls atomic.Int64
	var moving, finish atomic.Bool
	sink := func(ctx context.Context, _ *Work, _ *tg.Message, _ telegram.MediaDownloader) error {
		calls.Add(1)
		if finish.Load() {
			return nil
		}
		stop := telegram.CheckpointStop(ctx)
		if stop == nil {
			t.Error("the transfer was given no way to stop at a checkpoint")
			return nil
		}
		moving.Store(true)
		defer moving.Store(false)
		for !stop() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
		return telegram.ErrDownloadStopped
	}
	c := New(s.writer, s.reader, t.TempDir(), factory, filter, sink, nil)
	ctx := context.Background()
	if err := c.Start(ctx, ctx, []AccountConfig{{ID: "one"}}, 1, 1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Stop(context.Background()) })
	return c, &calls, &moving, &finish
}

// The update's drain: a file moving bytes stops at its checkpoint, goes back to
// the queue with no attempt spent, and nothing new starts until the drain ends.
func TestDrainStopsAMovingFileAtItsCheckpoint(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, _, err := s.Enqueue(ctx, "one", Target{}, testMessage(1, 1, 1)); err != nil {
		t.Fatal(err)
	}
	c, calls, moving, finish := stoppingController(t, s)
	waitFor(t, "the transfer to start", moving.Load)
	c.SetDraining(true)
	waitFor(t, "the transfer to stop", func() bool { return !moving.Load() && readStopRow(t, s, calls).status == "pending" })
	waitFor(t, "the drain to see nothing in progress", func() bool {
		files, _, err := c.DrainStatus(ctx)
		return err == nil && files == 0
	})
	r := readStopRow(t, s, calls)
	if r.attempts != 0 || r.retryAt != 0 || r.paused != 0 {
		t.Fatalf("after stopping: %+v (want no attempt spent, no retry wait, not paused)", r)
	}
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("started again while draining: %d calls", calls.Load())
	}
	// Back to downloading: it carries on and finishes.
	finish.Store(true)
	c.SetDraining(false)
	waitFor(t, "the file to finish", func() bool { return readStopRow(t, s, calls).status == "completed" })
}

// Pausing a file in progress: it stops at its next checkpoint and stays paused;
// resumed, it carries on.
func TestPauseStopsAMovingFileAtItsCheckpoint(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, _, err := s.Enqueue(ctx, "one", Target{}, testMessage(1, 1, 1)); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := s.reader.QueryRow(`SELECT group_id||'_'||message_id FROM tgdl_work`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	c, calls, moving, finish := stoppingController(t, s)
	waitFor(t, "the transfer to start", moving.Load)
	if ok, err := c.PauseJob(ctx, key); err != nil || !ok {
		t.Fatalf("pause: %v %v", ok, err)
	}
	waitFor(t, "the transfer to stop", func() bool { return !moving.Load() && readStopRow(t, s, calls).status == "pending" })
	r := readStopRow(t, s, calls)
	if r.attempts != 0 || r.paused == 0 {
		t.Fatalf("after pausing: %+v", r)
	}
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("a paused file started again: %d calls", calls.Load())
	}
	finish.Store(true)
	if ok, err := c.ResumeJob(ctx, key); err != nil || !ok {
		t.Fatalf("resume: %v %v", ok, err)
	}
	waitFor(t, "the file to finish", func() bool { return readStopRow(t, s, calls).status == "completed" })
	if calls.Load() != 2 {
		t.Fatalf("%d calls", calls.Load())
	}
}

// Another file's pause, or one taken back before the checkpoint, does not stop
// this one.
func TestPauseOfAnotherFileLeavesThisOneAlone(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, _, err := s.Enqueue(ctx, "one", Target{}, testMessage(1, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Enqueue(ctx, "one", Target{}, testMessage(2, 1, 2)); err != nil {
		t.Fatal(err)
	}
	c, calls, moving, _ := stoppingController(t, s)
	waitFor(t, "the transfer to start", moving.Load)
	var running, other string
	if err := s.reader.QueryRow(`SELECT group_id||'_'||message_id FROM tgdl_work WHERE status='processing'`).Scan(&running); err != nil {
		t.Fatal(err)
	}
	if err := s.reader.QueryRow(`SELECT group_id||'_'||message_id FROM tgdl_work WHERE status='pending'`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PauseJob(ctx, other); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if !moving.Load() {
		t.Fatal("pausing another file stopped this one")
	}
	_ = running
	_ = calls
}

// ix.12.2's loop at the controller: a large file waiting for its slot makes room,
// and the same worker takes the best smaller file next, before the large file
// comes round again.
func TestMakeRoomClaimsASmallerFileNext(t *testing.T) {
	withLane(t, 0)
	ctx := context.Background()
	s := testStore(t)
	const mb = int64(1) << 20
	for id := 1; id <= 3; id++ {
		if _, _, err := s.Enqueue(ctx, "one", Target{}, testMessage(id, 1, int64(id))); err != nil {
			t.Fatal(err)
		}
	}
	// 1: large and first in the claim order; 2: large, second; 3: small, last.
	if _, err := s.writer.Exec(`UPDATE tgdl_work SET file_size=CASE message_id WHEN 1 THEN ? WHEN 2 THEN ? ELSE ? END, sched_order=CASE message_id WHEN 1 THEN 1 WHEN 2 THEN 2 ELSE 300 END`, 300*mb, 200*mb, 2*mb); err != nil {
		t.Fatal(err)
	}
	factory := func(AccountConfig, *telegram.UpdateState, func(context.Context, tg.UpdatesClass) error, func(int64)) (Account, error) {
		return &readyAccount{}, nil
	}
	filter := func(context.Context, string, *tg.Message, tg.UpdatesClass) (Target, bool, error) {
		return Target{}, true, nil
	}
	var mu sync.Mutex
	var order []int64
	madeRoom := false
	sink := func(_ context.Context, w *Work, _ *tg.Message, _ telegram.MediaDownloader) error {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, w.MessageID)
		if w.MessageID == 1 && !madeRoom {
			madeRoom = true
			return telegram.ErrDownloadMakeRoom
		}
		return nil
	}
	c := New(s.writer, s.reader, t.TempDir(), factory, filter, sink, nil)
	if err := c.Start(ctx, ctx, []AccountConfig{{ID: "one"}}, 1, 1); err != nil {
		t.Fatal(err)
	}
	defer c.Stop(ctx)
	waitFor(t, "every file to finish", func() bool {
		var left int
		_ = s.reader.QueryRow(`SELECT COUNT(*) FROM tgdl_work WHERE status<>'completed'`).Scan(&left)
		return left == 0
	})
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 4 || order[0] != 1 || order[1] != 3 || order[2] != 1 || order[3] != 2 {
		t.Fatalf("claimed in the order %v, want [1 3 1 2]: after making room, the small file", order)
	}
	var attempts int64
	if err := s.reader.QueryRow(`SELECT attempts FROM tgdl_work WHERE message_id=1`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("making room spent an attempt: %d", attempts)
	}
}
