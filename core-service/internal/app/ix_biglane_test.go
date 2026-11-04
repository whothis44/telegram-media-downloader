package app

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
)

// ixOldQueueRow inserts a pending row as earlier versions (v7 and before)
// left them: only the queue's own columns, no archive state.
func ixOldQueueRow(t *testing.T, a *App, group string, msg int, size int64) int64 {
	t.Helper()
	at := time.Now().Add(-72 * time.Hour).UnixMilli()
	res, err := a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at)
		VALUES('acc',?,'G',?,1,?,'video','v.mp4',?,X'00','pending','history',?,?)`, group, msg, group+"/"+strconv.Itoa(msg), size, at, at)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func ixTestLane(t *testing.T, n int) {
	old := engine.BigFileLane()
	engine.SetBigFileLane(n)
	t.Cleanup(func() { engine.SetBigFileLane(old) })
}

func ixTestWorkers(t *testing.T, n int) {
	old := ixEngineWorkers
	ixEngineWorkers = func(*App, context.Context) int { return n }
	t.Cleanup(func() { ixEngineWorkers = old })
}

// ixTestBackfill puts a group in the rescan queue in its files pass, with
// its backfill running (it queues files, so the queue limit holds it back).
func ixTestBackfill(t *testing.T, a *App, ctx context.Context, group string) {
	t.Helper()
	if err := a.ixSaveQueue(ctx, ixQueueState{Running: true, Items: []ixQueueItem{{GroupID: group, Name: "B", State: "running", Phase: ixPhaseFiles, JobID: "j-" + group}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	a.historyMu.Lock()
	a.historyJobs["j-"+group] = &historyJob{ID: "j-" + group, State: "running", GroupID: group}
	a.historyMu.Unlock()
	t.Cleanup(func() {
		a.historyMu.Lock()
		delete(a.historyJobs, "j-"+group)
		a.historyMu.Unlock()
	})
}

func ixTestClaim(t *testing.T, a *App, n int) (big, small int) {
	t.Helper()
	s := engine.NewWorkStore(a.db.Writer, a.db.Reader)
	for i := 0; i < n; i++ {
		w, err := s.Claim(context.Background(), []string{"acc"}, time.Now(), true)
		if err != nil {
			t.Fatal(err)
		}
		if w == nil {
			break
		}
		if w.FileSize >= engine.BigFileSize {
			big++
		} else {
			small++
		}
	}
	return big, small
}

// The stall seen in production: the queue's limit (500) full of files of
// 1–2 GB queued by an earlier version, the backfill waiting for room that
// only those files can make, and the rescans' smaller files never queued.
// The deadline order alone must break it (the big-file lane is off here).
func TestIxDeadlineSetsBigFilesAsideForFilesNotQueuedYet(t *testing.T) {
	ixTestWorkers(t, 24)
	a, ctx := ix8App(t, map[string]any{"deadlineOrder": "on", "bigAtOnce": "0"})
	ixTestLane(t, 0)
	for i := 1; i <= 500; i++ {
		ixOldQueueRow(t, a, "-1001", i, 1<<30+int64(i%5)<<28) // 1 to 2 GB
	}
	ixTestBackfill(t, a, ctx, "-1002")
	b := a.ixBigFilesNow(ctx)
	if !b.CapPaused || b.QueueCount != 500 || b.Backfills != 1 || b.Waiting != 500 {
		t.Fatalf("before: %+v", b)
	}
	// Without files still to queue the order sees only the big band: it sets
	// nothing aside (how the stall came about).
	a.historyMu.Lock()
	a.historyJobs["j--1002"].State = "done"
	a.historyMu.Unlock()
	_ = a.ixSaveQueue(ctx, ixQueueState{Running: true, Items: []ixQueueItem{{GroupID: "-1002", State: "done", Phase: ixPhaseFiles}}}, time.Now())
	if err := a.ixDeadlineStep(ctx); err != nil {
		t.Fatal(err)
	}
	if n := a.ixDeferredWaiting(ctx); n != 0 {
		t.Fatalf("set aside %d with nothing smaller to come", n)
	}
	// With the rescan's files still to come, the big files are set aside;
	// half the workers may still take some, so downloads do not stop.
	ixTestBackfill(t, a, ctx, "-1002")
	if err := a.ixDeadlineStep(ctx); err != nil {
		t.Fatal(err)
	}
	b = a.ixBigFilesNow(ctx)
	if b.CapPaused || b.SetAside != 488 || b.Waiting != 12 || b.QueueCount != 12 {
		t.Fatalf("after the step: %+v", b)
	}
	// The backfill can queue again: the rescan's small files come in and
	// are claimed by the workers left free.
	for i := 1; i <= 40; i++ {
		ixOldQueueRow(t, a, "-1002", i, 3<<20)
	}
	if big, small := ixTestClaim(t, a, 24); big != 12 || small != 12 {
		t.Fatalf("24 workers took %d big and %d small files", big, small)
	}
	// The next step lets no more big files go while small ones wait.
	if err := a.ixDeadlineStep(ctx); err != nil {
		t.Fatal(err)
	}
	if b = a.ixBigFilesNow(ctx); b.Waiting != 0 || b.Downloading != 12 {
		t.Fatalf("second step: %+v", b)
	}
	if big, small := ixTestClaim(t, a, 50); big != 0 || small != 28 {
		t.Fatalf("then: %d big and %d small", big, small)
	}
	// The rescans done: the big files go, smallest first.
	a.historyMu.Lock()
	a.historyJobs["j--1002"].State = "done"
	a.historyMu.Unlock()
	_ = a.ixSaveQueue(ctx, ixQueueState{Running: true, Items: []ixQueueItem{{GroupID: "-1002", State: "done", Phase: ixPhaseFiles}}}, time.Now())
	if err := a.ixDeadlineStep(ctx); err != nil {
		t.Fatal(err)
	}
	if n := a.ixDeferredWaiting(ctx); n != 0 {
		t.Fatalf("still set aside after the rescans finished: %d", n)
	}
	// The diagnostics report has the numbers.
	if r := a.ixDiagnostics(ctx); !strings.Contains(r, "big files (1024 MB or more): 12 downloading") {
		t.Fatalf("report: %s", r)
	}
}

// The big-file lane on its own (the deadline order off): a queue of old
// big files no longer holds the backfill back, and the workers take the
// small files it brings while two big files download.
func TestIxBigFilesWaitingForTheirLaneDoNotFillTheQueue(t *testing.T) {
	ixTestLane(t, 2)
	a, ctx := ix8App(t, map[string]any{"deadlineOrder": "off"})
	for i := 1; i <= 500; i++ {
		ixOldQueueRow(t, a, "-1001", i, 1<<30+int64(i%5)<<28)
	}
	ixTestBackfill(t, a, ctx, "-1002")
	if b := a.ixBigFilesNow(ctx); b.CapPaused || b.QueueCount != 0 || b.Waiting != 500 || b.Limit != 2 {
		t.Fatalf("old big files fill the backfill's queue: %+v", b)
	}
	for i := 1; i <= 30; i++ {
		ixOldQueueRow(t, a, "-1002", i, 3<<20)
	}
	if big, small := ixTestClaim(t, a, 25); big != 2 || small != 23 {
		t.Fatalf("25 workers took %d big and %d small files", big, small)
	}
	b := a.ixBigFilesNow(ctx)
	if b.Downloading != 2 || b.Waiting != 498 || b.QueueCount != 25+7 {
		t.Fatalf("after claims: %+v", b)
	}
	// The setting: a number, empty for the default, 0 for no limit.
	s, err := ixParseSettings(map[string]any{"bigAtOnce": "3"}, a.ixLoadSettings(ctx))
	if err != nil || s.BigAtOnce != 3 {
		t.Fatalf("setting: %d %v", s.BigAtOnce, err)
	}
	if s, _ = ixParseSettings(map[string]any{"bigAtOnce": ""}, s); s.BigAtOnce != engine.DefaultBigFileLane {
		t.Fatalf("empty setting: %d", s.BigAtOnce)
	}
	if _, err = ixParseSettings(map[string]any{"bigAtOnce": "-1"}, s); err == nil {
		t.Fatal("a negative number was accepted")
	}
	if ixDefaultSettings().BigAtOnce != 2 {
		t.Fatal("default")
	}
	ixApplyEngineSettings(ixSettings{DownloadPacing: true, BigAtOnce: 0})
	if engine.BigFileLane() != 0 {
		t.Fatal("setting not applied")
	}
}

// Only public chats left to read: their files go after every private
// chat's, so the private chats' files queued are not set aside for them.
// A rescan queue held for good (the pause on freeze) counts only for the
// pass already running.
func TestIxUnqueuedFilesOfPublicChatsAndHeldQueues(t *testing.T) {
	ixTestWorkers(t, 24)
	a, ctx := ix8App(t, map[string]any{"deadlineOrder": "on", "pauseOnFreeze": true})
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_group_info(group_id,title,kind,username) VALUES('-1002','Public','channel','somechannel')`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 20; i++ {
		ixOldQueueRow(t, a, "-1001", i, 600<<20)
	}
	ixTestBackfill(t, a, ctx, "-1002")
	if band, ok := a.ixUnqueuedBand(ctx); !ok || band != ixPublicLane {
		t.Fatalf("band %d %v", band, ok)
	}
	if err := a.ixDeadlineStep(ctx); err != nil {
		t.Fatal(err)
	}
	if n := a.ixDeferredWaiting(ctx); n != 0 {
		t.Fatalf("private files set aside for a public chat's: %d", n)
	}
	// A private chat waiting for its files pass, the queue paused on freeze.
	_ = a.ixSaveQueue(ctx, ixQueueState{Running: true, Items: []ixQueueItem{{GroupID: "-1003", State: "waiting", Phase: ixPhaseFiles}}}, time.Now())
	a.historyMu.Lock()
	a.historyJobs["j--1002"].State = "done"
	a.historyMu.Unlock()
	if band, ok := a.ixUnqueuedBand(ctx); !ok || band != 0 {
		t.Fatalf("a waiting files pass did not count: %d %v", band, ok)
	}
	ixAccountState.Store(a, ixAccountStatus{Frozen: true})
	t.Cleanup(func() { ixAccountState.Delete(a) })
	if _, ok := a.ixUnqueuedBand(ctx); ok {
		t.Fatal("a files pass held by the pause on freeze counted")
	}
}

// Big files the lane holds back are not work the idle workers could take:
// with a dozen big explicit requests queued (the deadline order never sets
// those aside), set-aside small files must still go to the idle workers.
func TestIxKeepBusyCountsOnlyWhatTheLaneLetsGo(t *testing.T) {
	ixTestWorkers(t, 24)
	ixTestLane(t, 2)
	a, ctx := ix8App(t, map[string]any{"deadlineOrder": "on"})
	for i := 1; i <= 15; i++ {
		ixOldQueueRow(t, a, "-1003", i, 1<<30+int64(i)<<20)
	}
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET origin='url' WHERE group_id='-1003'`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 200; i++ {
		ixOldQueueRow(t, a, "-1001", i, 200<<20)
	}
	ixTestBackfill(t, a, ctx, "-1002") // a private chat's files still to come
	if err := a.ixDeadlineStep(ctx); err != nil {
		t.Fatal(err)
	}
	// Half the workers (12): 2 big files the lane lets go, 10 small ones
	// (the lane then lets 2 more big files go, nothing smaller being left).
	if n := a.ixDeferredWaiting(ctx); n != 190 {
		t.Fatalf("still set aside: %d (want 190)", n)
	}
	if big, small := ixTestClaim(t, a, 24); big != 4 || small != 10 {
		t.Fatalf("24 workers took %d big and %d small files", big, small)
	}
	// The next step lets no more go: half the workers have a file.
	if err := a.ixDeadlineStep(ctx); err != nil {
		t.Fatal(err)
	}
	if big, small := ixTestClaim(t, a, 24); big != 0 || small != 0 {
		t.Fatalf("then: %d big and %d small", big, small)
	}
}
