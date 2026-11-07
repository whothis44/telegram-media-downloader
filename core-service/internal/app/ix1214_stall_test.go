package app

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ix.12.14: a messages pass that stops making progress (one page read, then
// nothing for 19 hours on 11-07, through a restart) gives its lane back: its
// job is ended, the group tries once more from where it got to with a new
// job, and a second stall leaves it failed so the groups behind it go on.
func TestIx1214StalledPassGivesItsLaneBack(t *testing.T) {
	ixBackgroundQueue.Store(false)
	defer ixBackgroundQueue.Store(true)
	a, ctx := ixTestApp(t)
	now := time.Unix(1_800_000_000, 0)
	jobs := map[string]historyJob{}
	var started []string
	var resumed []int
	var abandoned []string
	stalled := map[string]bool{}
	deps := ixQueueDeps{
		start: func(_ context.Context, id string, _ bool, resumeFrom int) (string, error) {
			started = append(started, id)
			resumed = append(resumed, resumeFrom)
			jid := fmt.Sprintf("job-%d", len(started))
			jobs[jid] = historyJob{ID: jid, State: "running", GroupID: id} // reads nothing: stalls
			return jid, nil
		},
		job:     func(_ context.Context, id string) (historyJob, bool) { j, ok := jobs[id]; return j, ok },
		cursor:  func(context.Context, string) int { return 4321 },
		missing: func(context.Context, string) (int, error) { return 0, nil },
		finish:  func(context.Context, ixQueueItem, historyJob) error { return nil },
		now:     func() time.Time { return now },
		stalled: func(_ context.Context, _ string, jobID string, _ time.Time) bool { return stalled[jobID] },
		abandon: func(_ context.Context, jobID, _ string) string {
			abandoned = append(abandoned, jobID)
			j := jobs[jobID]
			j.State = "error" // as ixAbandonPass leaves it
			jobs[jobID] = j
			return ""
		},
	}
	if _, err := a.ixQueueEdit(ctx, func(st *ixQueueState) error {
		st.Running = true
		st.Items = append(st.Items,
			ixQueueItem{GroupID: "stuck", Name: "stuck", State: "waiting", Phase: ixPhaseMessages},
			ixQueueItem{GroupID: "next", Name: "next", State: "waiting", Phase: ixPhaseMessages})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	step := func() {
		t.Helper()
		if err := a.ixQueueStep(ctx, deps); err != nil {
			t.Fatal(err)
		}
	}
	item := func(id string) ixQueueItem {
		t.Helper()
		st, err := a.ixLoadQueue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		i := ixQueueIndex(&st, id)
		if i < 0 {
			t.Fatalf("%s left the queue", id)
		}
		return st.Items[i]
	}
	step()
	if len(started) != 1 || started[0] != "stuck" {
		t.Fatalf("started %v", started)
	}
	// Running, with progress: left alone.
	step()
	if len(abandoned) != 0 || item("stuck").State != "running" {
		t.Fatalf("a working pass was stopped: %v", abandoned)
	}
	// No progress: its job is ended and it waits to try again from where it
	// got to; the lane goes to the next group meanwhile.
	stalled["job-1"] = true
	step()
	if len(abandoned) != 1 || abandoned[0] != "job-1" {
		t.Fatalf("abandoned %v", abandoned)
	}
	got := item("stuck")
	if got.State != "waiting" || got.Stalls != 1 || got.ResumeFrom != 4321 || got.JobID != "" || !strings.Contains(got.Error, "no progress") {
		t.Fatalf("after the first stall: %+v", got)
	}
	if len(started) != 2 || started[1] != "next" {
		t.Fatalf("the lane did not go to the next group: %v", started)
	}
	// The next group finishes; the stalled one gets a new job, resuming.
	j := jobs["job-2"]
	j.State = "done"
	jobs["job-2"] = j
	now = now.Add(time.Hour)
	step()
	step()
	if len(started) != 3 || started[2] != "stuck" || resumed[2] != 4321 {
		t.Fatalf("not tried again from where it got to: %v %v", started, resumed)
	}
	// Stalled again: failed, out of the lane, with a message.
	stalled["job-3"] = true
	step()
	got = item("stuck")
	if got.State != "failed" || got.Stalls != 2 || got.JobID != "" || !strings.Contains(got.Error, "twice") {
		t.Fatalf("after the second stall: %+v", got)
	}
	if len(abandoned) != 2 {
		t.Fatalf("abandoned %v", abandoned)
	}
	// "Run again" clears the count.
	if _, err := a.ixQueueEdit(ctx, func(st *ixQueueState) error {
		i := ixQueueIndex(st, "stuck")
		st.Items[i].State, st.Items[i].Stalls = "waiting", 0
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The watchdog's clock: a job first seen counts from then, a beat restarts
// it, and only a pass quiet for longer than ixPassStallAfter is stalled.
func TestIx1214PassStalledCountsFromTheLastBeat(t *testing.T) {
	a, ctx := ixTestApp(t)
	t0 := time.Now()
	if a.ixPassStalled(ctx, "g", "j", t0) {
		t.Fatal("stalled when first seen")
	}
	if a.ixPassStalled(ctx, "g", "j", t0.Add(ixPassStallAfter-time.Second)) {
		t.Fatal("stalled before the limit")
	}
	if !a.ixPassStalled(ctx, "g", "j", t0.Add(ixPassStallAfter+time.Second)) {
		t.Fatal("not stalled after the limit without a beat")
	}
	a.ixBeat("g")
	if a.ixPassStalled(ctx, "g", "j", time.Now().Add(ixPassStallAfter-time.Second)) {
		t.Fatal("stalled right after a beat")
	}
	if !a.ixPassStalled(ctx, "g", "j", time.Now().Add(ixPassStallAfter+time.Minute)) {
		t.Fatal("a beat kept it alive for ever")
	}
	// Another group's beat does not count.
	a.ixBeat("other")
	if !a.ixPassStalled(ctx, "g", "j", time.Now().Add(ixPassStallAfter+time.Minute)) {
		t.Fatal("another group's beat counted")
	}
}

// Ending a stalled job: failed in memory and in the database, its worker
// told to stop, the goroutines saved, and a new start no longer handed the
// same job.
func TestIx1214AbandonEndsTheJobForGood(t *testing.T) {
	a, ctx := ixTestApp(t)
	a.dataDir = t.TempDir()
	a.historyMu.Lock()
	if a.historyJobs == nil {
		a.historyJobs = map[string]*historyJob{}
	}
	if a.historyCancel == nil {
		a.historyCancel = map[string]context.CancelFunc{}
	}
	j := &historyJob{ID: "hj1", GroupID: "-1001234567890", State: "running", Processed: 100, MessagesOnly: true}
	a.historyJobs[j.ID] = j
	workerCtx, cancel := context.WithCancel(context.Background())
	a.historyCancel[j.ID] = cancel
	a.historyMu.Unlock()
	data, err := historyBytes(*j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO tgdl_history_jobs(id,group_id,state,payload) VALUES(?,?,?,?)`, j.ID, j.GroupID, j.State, string(data)); err != nil {
		t.Fatal(err)
	}
	note := a.ixAbandonPass(ctx, "hj1", "stopped: no progress for 30 minutes")
	if workerCtx.Err() == nil {
		t.Fatal("the worker was not told to stop")
	}
	a.historyMu.Lock()
	state, cancelled := a.historyJobs["hj1"].State, a.historyJobs["hj1"].Cancelled
	a.historyMu.Unlock()
	if state != "error" || !cancelled {
		t.Fatalf("in memory: %s cancelled=%v", state, cancelled)
	}
	var saved string
	if err := a.db.Reader.QueryRow(`SELECT state FROM tgdl_history_jobs WHERE id='hj1'`).Scan(&saved); err != nil || saved != "error" {
		t.Fatalf("in the database: %q %v (a restart would resume it)", saved, err)
	}
	// A restart resumes only running rows.
	var running int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM tgdl_history_jobs WHERE state='running'`).Scan(&running)
	if running != 0 {
		t.Fatalf("%d running rows left", running)
	}
	if !strings.Contains(note, "still running") || !strings.Contains(note, "data/crashes") {
		t.Fatalf("note: %q", note)
	}
	dumps, _ := filepath.Glob(filepath.Join(a.dataDir, CrashDir, "stall-*.txt"))
	if len(dumps) != 1 {
		t.Fatalf("goroutines not saved: %v", dumps)
	}
	if b, err := os.ReadFile(dumps[0]); err != nil || !strings.Contains(string(b), "goroutine") {
		t.Fatalf("dump: %v", err)
	}
	// A second call (the job already ended) changes nothing and does not fail.
	_ = a.ixAbandonPass(ctx, "hj1", "again")
	_ = a.ixAbandonPass(ctx, "missing", "none")
	var n sql.NullInt64
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM tgdl_history_jobs`).Scan(&n)
	if n.Int64 != 1 {
		t.Fatalf("rows: %d", n.Int64)
	}
}

// A messages pass that read every message and stalled in its end-of-pass
// work (comments, message gaps) counts as done: its files pass follows.
// One that reads something after a stall starts its count again.
func TestIx1214StallAfterTheLastMessageCountsAsDone(t *testing.T) {
	ixBackgroundQueue.Store(false)
	defer ixBackgroundQueue.Store(true)
	a, ctx := ixTestApp(t)
	now := time.Unix(1_800_000_000, 0)
	jobs := map[string]historyJob{}
	var started []string
	var finished []string
	stalled, ended := map[string]bool{}, map[string]bool{}
	deps := ixQueueDeps{
		start: func(_ context.Context, id string, _ bool, _ int) (string, error) {
			started = append(started, id)
			jid := fmt.Sprintf("job-%d", len(started))
			jobs[jid] = historyJob{ID: jid, State: "running", GroupID: id}
			return jid, nil
		},
		job:     func(_ context.Context, id string) (historyJob, bool) { j, ok := jobs[id]; return j, ok },
		cursor:  func(context.Context, string) int { return 10 },
		missing: func(context.Context, string) (int, error) { return 7, nil },
		finish: func(_ context.Context, it ixQueueItem, _ historyJob) error {
			finished = append(finished, it.GroupID)
			return nil
		},
		now:       func() time.Time { return now },
		stalled:   func(_ context.Context, _ string, jobID string, _ time.Time) bool { return stalled[jobID] },
		abandon:   func(context.Context, string, string) string { return "-" },
		passEnded: func(g string) bool { return ended[g] },
	}
	if _, err := a.ixQueueEdit(ctx, func(st *ixQueueState) error {
		st.Running = true
		st.Items = append(st.Items, ixQueueItem{GroupID: "g", Name: "g", State: "waiting", Phase: ixPhaseMessages, Stalls: 1})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.ixQueueStep(ctx, deps); err != nil {
		t.Fatal(err)
	}
	// It reads: the earlier stall no longer counts.
	j := jobs["job-1"]
	j.Processed = 300
	jobs["job-1"] = j
	if err := a.ixQueueStep(ctx, deps); err != nil {
		t.Fatal(err)
	}
	st, _ := a.ixLoadQueue(ctx)
	if it := st.Items[ixQueueIndex(&st, "g")]; it.Stalls != 0 {
		t.Fatalf("stall count kept after reading: %+v", it)
	}
	// Every message read, then no progress in the end-of-pass work.
	ended["g"], stalled["job-1"] = true, true
	if err := a.ixQueueStep(ctx, deps); err != nil {
		t.Fatal(err)
	}
	st, _ = a.ixLoadQueue(ctx)
	it := st.Items[ixQueueIndex(&st, "g")]
	if it.Phase != ixPhaseFiles || it.Missing != 7 || it.Messages != 300 || len(finished) != 1 {
		t.Fatalf("not counted as a finished messages pass: %+v finished=%v", it, finished)
	}
}
