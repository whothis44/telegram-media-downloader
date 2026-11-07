package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// ix.12.14: a rescan pass that stops making progress gives its lane back.
//
// On 2026-11-07 a messages pass had read one page (100 messages) and then
// nothing for 19 hours, through a restart (the restart resumed the same job,
// which stopped at the same place). Nothing noticed: the queue waits while a
// job is "running", the stall deadline of a backfill only covers a full
// download queue, and a cancel only flags the job, so a worker that never
// returns keeps it "running", and starting the group again hands back the
// same job. Four ranked groups waited behind it.
//
// Every backfill now beats while it works (each page, each wait of the
// download queue, each batch of the end-of-pass work). A pass with no beat
// for ixPassStallAfter is ended outright: its job is marked failed in memory
// and in the database (so a restart does not resume it, and the next start
// makes a new job), the goroutines are written to data/crashes (to find where
// it stopped), and the group tries once more from where it got to. A second
// stall in a row leaves it failed, so the groups behind it go on; "Run again"
// in the Rescan tab tries it once more.

// ixPassStallAfter is how long a pass may go without a beat.
var ixPassStallAfter = 30 * time.Minute

// ixStallDumps is the most goroutine dumps kept in data/crashes.
const ixStallDumps = 10

// ixBeat notes that the backfill of groupID did something just now.
func (a *App) ixBeat(groupID string) {
	if groupID == "" {
		return
	}
	rt := a.ix()
	rt.mu.Lock()
	if rt.beats == nil {
		rt.beats = map[string]int64{}
	}
	rt.beats[groupID] = time.Now().UnixMilli()
	rt.mu.Unlock()
}

// ixPassEnd notes whether the backfill of groupID has read every message and
// is in its end-of-pass work (comments, message gaps).
func (a *App) ixPassEnd(groupID string, at bool) {
	rt := a.ix()
	rt.mu.Lock()
	if rt.passEnd == nil {
		rt.passEnd = map[string]bool{}
	}
	if at {
		rt.passEnd[groupID] = true
	} else {
		delete(rt.passEnd, groupID)
	}
	rt.mu.Unlock()
}

// ixPassEnded says the backfill of groupID is in its end-of-pass work.
func (a *App) ixPassEnded(groupID string) bool {
	rt := a.ix()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.passEnd[groupID]
}

// ixPassStalled says whether the running pass jobID of groupID has gone
// ixPassStallAfter without a beat. A job first seen by this process counts
// from then: a job resumed after a restart beats once it works again.
func (a *App) ixPassStalled(_ context.Context, groupID, jobID string, now time.Time) bool {
	rt := a.ix()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.passSeen == nil {
		rt.passSeen = map[string]int64{}
	}
	seen, ok := rt.passSeen[jobID]
	if !ok {
		rt.passSeen[jobID] = now.UnixMilli()
		return false
	}
	last := max(seen, rt.beats[groupID])
	return now.UnixMilli()-last > ixPassStallAfter.Milliseconds()
}

// ixAbandonPass ends a stalled backfill job: failed, in memory and in the
// database, its worker told to stop. It returns a note for the log: whether
// a worker still held the job (blocked somewhere) or none did (it had ended
// and its end was not saved), and whether the goroutines were written. A
// worker that comes back later finds its job cancelled and stops.
func (a *App) ixAbandonPass(ctx context.Context, jobID, reason string) string {
	dumped := a.ixSaveStacks() != ""
	a.historyMu.Lock()
	defer a.historyMu.Unlock()
	rt := a.ix()
	rt.mu.Lock()
	delete(rt.passSeen, jobID)
	rt.mu.Unlock()
	note := "its worker had already ended"
	if a.historyCancel[jobID] != nil {
		note = "its worker was still running, blocked"
	}
	if dumped {
		note += "; what the app was doing is saved in data/crashes"
	}
	j := a.historyJobs[jobID]
	if j == nil || j.State != "running" {
		return note
	}
	c := *j
	now := time.Now().UnixMilli()
	why := reason
	c.State, c.Cancelled, c.FinishedAt, c.Error = "error", true, &now, &why
	// Bounded: the writer is one connection, and the queue and the backfills
	// wait on the locks held here.
	wctx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelWrite()
	if tx, err := a.db.Writer.BeginTx(wctx, nil); err == nil {
		if historySaveTx(wctx, tx, c) == nil {
			_ = tx.Commit()
		}
		_ = tx.Rollback()
	}
	// In memory even if the database refused: the queue and new starts go by
	// this copy; a restart would resume the job and stop it again.
	*j = c
	if cancel := a.historyCancel[jobID]; cancel != nil {
		cancel()
	}
	return note
}

// ixSaveStacks writes every goroutine's stack to data/crashes, to find where
// a stalled pass stopped. The file holds code locations and raw argument
// words, no message text; it stays on this computer and is not part of the
// diagnostics report.
func (a *App) ixSaveStacks() string {
	if a.dataDir == "" {
		return ""
	}
	dir := filepath.Join(a.dataDir, CrashDir)
	if old, _ := filepath.Glob(filepath.Join(dir, "stall-*.txt")); len(old) >= ixStallDumps {
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	buf := make([]byte, 8<<20)
	buf = buf[:runtime.Stack(buf, true)]
	name := filepath.Join(dir, "stall-"+time.Now().UTC().Format("20060102-150405")+".txt")
	if err := os.WriteFile(name, buf, 0o600); err != nil {
		return ""
	}
	return name
}
