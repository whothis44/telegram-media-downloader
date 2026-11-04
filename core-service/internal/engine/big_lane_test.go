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

func TestClaimWithLaneKeepsItsIndex(t *testing.T) {
	withLane(t, 2)
	s := testStore(t)
	where, args := claimFilter([]string{"one", "two"}, time.Now(), true)
	rows, err := s.reader.Query(`EXPLAIN QUERY PLAN SELECT id FROM tgdl_work WHERE `+where+` ORDER BY `+claimPriority+`,id LIMIT 1`, args...)
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
	joined := strings.Join(plan, "; ")
	if !strings.Contains(joined, "idx_tgdl_work_priority") || strings.Contains(joined, "TEMP B-TREE") || strings.Contains(joined, "SCAN tgdl_work") {
		t.Fatalf("claim with the big-file lane no longer comes from its index: %s", joined)
	}
}
