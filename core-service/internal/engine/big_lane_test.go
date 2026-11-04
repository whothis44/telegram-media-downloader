package engine

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// addOldRow inserts a pending row as versions before the big-file lane
// wrote them: no lane, no archive state, only the queue's own columns.
func addOldRow(t *testing.T, s *WorkStore, msg int, size int64, origin string, ttl int) int64 {
	t.Helper()
	res, err := s.writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,ttl_seconds,created_at,updated_at)
		VALUES('one','-1001','G',?,1,?,'video','f.mp4',?,X'00',?,?,?,1,1)`, msg, "id"+strconv.Itoa(msg), size, "pending", origin, ttl)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func withLane(t *testing.T, n int) {
	t.Helper()
	old := BigFileLane()
	SetBigFileLane(n)
	t.Cleanup(func() { SetBigFileLane(old) })
}

func claimIDs(t *testing.T, s *WorkStore, n int) []int64 {
	t.Helper()
	var out []int64
	for i := 0; i < n; i++ {
		w, err := s.Claim(context.Background(), []string{"one"}, time.Now(), true)
		if err != nil {
			t.Fatal(err)
		}
		if w == nil {
			break
		}
		out = append(out, w.MessageID)
	}
	return out
}

func TestBigFilesTakeAtMostTheirLane(t *testing.T) {
	withLane(t, 2)
	s := testStore(t)
	const gb = int64(1) << 30
	// A queue filled by an older version: big files first, small ones behind.
	for i := 1; i <= 6; i++ {
		addOldRow(t, s, i, gb+int64(i)<<28, "history", 0) // 1.25 to 2.5 GB
	}
	for i := 7; i <= 9; i++ {
		addOldRow(t, s, i, 5<<20, "history", 0)
	}
	got := claimIDs(t, s, 5)
	// Two big files, then the small ones, although the big ones came first.
	if len(got) != 5 || got[0] != 1 || got[1] != 2 || got[2] != 7 || got[3] != 8 || got[4] != 9 {
		t.Fatalf("claim order %v, want 1 2 7 8 9", got)
	}
	// Nothing smaller left: twice the lane, never more.
	got = claimIDs(t, s, 5)
	if len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("with only big files left: %v, want 3 4", got)
	}
	// A small file arriving now is taken; the big ones wait.
	addOldRow(t, s, 10, 1<<20, "history", 0)
	if got = claimIDs(t, s, 3); len(got) != 1 || got[0] != 10 {
		t.Fatalf("small file behind four big ones: %v", got)
	}
	// Self-destructing media is never held back.
	addOldRow(t, s, 11, 2*gb, "url", 30)
	if got = claimIDs(t, s, 3); len(got) != 1 || got[0] != 11 {
		t.Fatalf("self-destructing big file: %v", got)
	}
	// A big file finished: with nothing smaller waiting, another goes.
	if _, err := s.writer.Exec(`UPDATE tgdl_work SET status='completed' WHERE message_id IN (1,2)`); err != nil {
		t.Fatal(err)
	}
	if got = claimIDs(t, s, 3); len(got) != 2 || got[0] != 5 || got[1] != 6 {
		t.Fatalf("after two big files finished: %v", got)
	}
	// Switched off: no limit.
	addOldRow(t, s, 12, 2*gb, "history", 0)
	SetBigFileLane(0)
	if got = claimIDs(t, s, 3); len(got) != 1 || got[0] != 12 {
		t.Fatalf("lane off: %v", got)
	}
}

func TestBigFileLaneHoldsUnderConcurrentClaims(t *testing.T) {
	withLane(t, 2)
	s := testStore(t)
	for i := 1; i <= 20; i++ {
		addOldRow(t, s, i, 3<<30, "history", 0)
	}
	for i := 21; i <= 60; i++ {
		addOldRow(t, s, i, 1<<20, "history", 0)
	}
	var mu sync.Mutex
	big := 0
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, err := s.Claim(context.Background(), []string{"one"}, time.Now(), true)
			if err != nil {
				t.Error(err)
				return
			}
			if w != nil && w.FileSize >= BigFileSize {
				mu.Lock()
				big++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if big != 2 {
		t.Fatalf("25 workers at once took %d big files, want 2", big)
	}
}

func claimPlan(t *testing.T, s *WorkStore, accounts []string) string {
	t.Helper()
	pick, args := claimSelect(accounts, time.Now(), true)
	rows, err := s.reader.Query(`EXPLAIN QUERY PLAN `+pick, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var detail string
		if err := rows.Scan(new(int), new(int), new(int), &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	return strings.Join(plan, "; ")
}

// The claim comes from its index in claim order, for one account as for
// several: no sort of the pending rows, no scan of the table.
func TestClaimWithLaneKeepsItsIndex(t *testing.T) {
	s := testStore(t)
	for i := 1; i <= 300; i++ {
		addOldRow(t, s, i, int64(i%3)<<30, []string{"history", "url", "live"}[i%3], 0)
	}
	if _, err := s.writer.Exec(`ANALYZE`); err != nil { // the planner with statistics, as after PRAGMA optimize
		t.Fatal(err)
	}
	for _, lane := range []int{2, 0} {
		withLane(t, lane)
		index := "idx_tgdl_work_claim"
		if lane == 0 {
			index = "idx_tgdl_work_priority"
		}
		for _, accounts := range [][]string{{"one"}, {"one", "two"}} {
			plan := claimPlan(t, s, accounts)
			if !strings.Contains(plan, index) || strings.Contains(plan, "TEMP B-TREE") || strings.Contains(plan, "SCAN tgdl_work") {
				t.Fatalf("lane %d, %d account(s): the claim no longer comes from %s: %s", lane, len(accounts), index, plan)
			}
		}
	}
}

// The index is on the very expression the claim uses.
func TestClaimIndexMatchesTheLane(t *testing.T) {
	s := testStore(t)
	var def string
	if err := s.reader.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_tgdl_work_claim'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, bigRowSQL) || !strings.Contains(def, claimPriority) {
		t.Fatalf("idx_tgdl_work_claim %q does not use %q and %q", def, bigRowSQL, claimPriority)
	}
}

// A claim that finds nothing in a queue of big files waiting for the lane
// does not walk the queue: idle workers poll it.
func TestClaimFindingNothingIsCheap(t *testing.T) {
	withLane(t, 2)
	s := testStore(t)
	tx, err := s.writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 50000; i++ {
		if _, err := tx.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at)
			VALUES('one','-1001','G',?,1,?,'video','f.mp4',?,X'00',?,'history',1,1)`, i, "id"+strconv.Itoa(i), BigFileSize+int64(i), map[bool]string{true: "processing", false: "pending"}[i <= 4]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, accounts := range [][]string{{"one"}, {"one", "two"}} {
		start := time.Now()
		for i := 0; i < 20; i++ {
			w, err := s.Claim(context.Background(), accounts, time.Now(), true)
			if err != nil || w != nil {
				t.Fatalf("claimed %+v, %v", w, err)
			}
		}
		// Walking the 50,000 rows took some 16 ms a claim; generous for -race.
		per := time.Since(start) / 20
		t.Logf("%d account(s): %v a claim finding nothing", len(accounts), per)
		if per > 5*time.Millisecond {
			t.Fatalf("%d account(s): a claim finding nothing took %v", len(accounts), per)
		}
	}
	// A small file arriving is still found, before any waiting big file.
	addOldRow(t, s, 60001, 1<<20, "history", 0)
	w, err := s.Claim(context.Background(), []string{"one"}, time.Now(), true)
	if err != nil || w == nil || w.FileSize != 1<<20 {
		t.Fatalf("claimed %+v, %v", w, err)
	}
}
