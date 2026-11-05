package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// A row as the app's scheduler left it: invented chats and files.
func addRankedRow(t *testing.T, s *WorkStore, group string, msg int, identity string, size int64, order int) int64 {
	t.Helper()
	res, err := s.writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,sched_order,created_at,updated_at)
		VALUES('one',?,'G',?,1,?,'video','f.mp4',?,X'00','pending','history',?,1,1)`, group, msg, identity, size, order)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func claimOne(t *testing.T, s *WorkStore) *Work {
	t.Helper()
	w, err := s.Claim(context.Background(), []string{"one"}, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestRankedRowsAreClaimedBeforeOlderUnrankedOnes(t *testing.T) {
	s := testStore(t)
	for _, lane := range []int{2, 0} { // with and without the big-file lane
		withLane(t, lane)
		if _, err := s.writer.Exec(`DELETE FROM tgdl_work`); err != nil {
			t.Fatal(err)
		}
		addRankedRow(t, s, "low", 1, "a", 10, UnrankedOrder)
		addRankedRow(t, s, "low", 2, "b", 10, UnrankedOrder)
		second := addRankedRow(t, s, "two", 3, "c", 10, 16)
		first := addRankedRow(t, s, "one", 4, "d", 10, 8)
		var got []int64
		for w := claimOne(t, s); w != nil; w = claimOne(t, s) {
			got = append(got, w.ID)
		}
		if len(got) != 4 || got[0] != first || got[1] != second {
			t.Fatalf("lane %d: claim order %v, want %d then %d first", lane, got, first, second)
		}
	}
}

// A ranked big file goes ahead of lower-ranked small files, within the lane's
// limit; when the lane is full, smaller files of any rank still use the workers.
func TestRankedBigFileGoesFirstWithinTheLane(t *testing.T) {
	s := testStore(t)
	withLane(t, 1)
	for i := 1; i <= 3; i++ {
		addRankedRow(t, s, "low", i, "small"+strconv.Itoa(i), 1<<20, UnrankedOrder)
	}
	big := addRankedRow(t, s, "top", 10, "big", BigFileSize*2, 8)
	big2 := addRankedRow(t, s, "top", 11, "big2", BigFileSize*2, 8)
	if w := claimOne(t, s); w == nil || w.ID != big {
		t.Fatalf("first claim %+v, want the ranked big file", w)
	}
	w := claimOne(t, s)
	if w == nil || w.ID == big2 || w.FileSize >= BigFileSize {
		t.Fatalf("second claim %+v, want a small file: the lane is full", w)
	}
}

// Self-destructing media and explicit requests keep their place ahead of any
// rank; with no group ranked, live goes before history as it always did.
func TestLanesStayAheadOfRanks(t *testing.T) {
	s := testStore(t)
	withLane(t, 0)
	ranked := addRankedRow(t, s, "top", 1, "a", 10, 8)
	history := addOldRow(t, s, 2, 10, "history", 0)
	live := addOldRow(t, s, 3, 10, "live", 0)
	explicit := addOldRow(t, s, 4, 10, "url", 0)
	ttl := addOldRow(t, s, 5, 10, "history", 60)
	want := []int64{ttl, explicit, ranked, live, history}
	for i, id := range want {
		w, err := s.Claim(context.Background(), []string{"one"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if w == nil || w.ID != id {
			t.Fatalf("claim %d = %+v, want id %d", i, w, id)
		}
	}
}

// One transfer per Telegram file: while a row downloads, its twin in another
// group stays in the queue instead of holding a worker, then is claimed.
func TestClaimLeavesATwinOfWhatIsDownloading(t *testing.T) {
	s := testStore(t)
	for _, lane := range []int{2, 0} {
		withLane(t, lane)
		if _, err := s.writer.Exec(`DELETE FROM tgdl_work`); err != nil {
			t.Fatal(err)
		}
		shared := "video\x00777\x00100"
		a := addRankedRow(t, s, "high", 1, shared, 100, 8)
		b := addRankedRow(t, s, "low", 2, shared, 100, 8)
		c := addRankedRow(t, s, "low", 3, "video\x00888\x00100", 100, UnrankedOrder)
		w1 := claimOne(t, s)
		if w1 == nil || w1.ID != a {
			t.Fatalf("lane %d: first claim %+v", lane, w1)
		}
		w2 := claimOne(t, s)
		if w2 == nil || w2.ID != c {
			t.Fatalf("lane %d: second claim %+v, want the other file, not the twin", lane, w2)
		}
		if w := claimOne(t, s); w != nil {
			t.Fatalf("lane %d: claimed the twin %d while its file downloads", lane, w.ID)
		}
		if err := s.Finish(context.Background(), w1, nil, false, time.Time{}); err != nil {
			t.Fatal(err)
		}
		if w := claimOne(t, s); w == nil || w.ID != b {
			t.Fatalf("lane %d: the twin was not claimed once the file was done: %+v", lane, w)
		}
	}
}

func TestOutrankedSeesBetterWorkOnly(t *testing.T) {
	s := testStore(t)
	withLane(t, 2)
	ctx := context.Background()
	addRankedRow(t, s, "low", 1, "a", 10, UnrankedOrder)
	w := claimOne(t, s)
	better, err := s.Outranked(ctx, []string{"one"}, time.Now(), w.ClaimKey, true)
	if err != nil || better {
		t.Fatalf("empty queue: better=%v err=%v", better, err)
	}
	addRankedRow(t, s, "low", 2, "b", 10, UnrankedOrder) // same order, later id
	if better, err = s.Outranked(ctx, []string{"one"}, time.Now(), w.ClaimKey, true); err != nil || better {
		t.Fatalf("a later equal row is not better: better=%v err=%v", better, err)
	}
	addRankedRow(t, s, "top", 3, "c", 10, 8)
	if better, err = s.Outranked(ctx, []string{"one"}, time.Now(), w.ClaimKey, true); err != nil || !better {
		t.Fatalf("a ranked row is better: better=%v err=%v", better, err)
	}
}

// A row entering the queue takes its group's place at once, without waiting
// for the scheduler's next step.
func TestEnqueueTakesTheGroupsRank(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.writer.Exec(`INSERT INTO tgdl_group_rank(group_id,rank,sched_base,name,updated_at) VALUES('-1009',1,8,'top',1)`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := enqueueWork(ctx, s.writer, "one", Target{ID: "-1009", Name: "top"}, testMessage(5, 0, 1), 0, "history"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := enqueueWork(ctx, s.writer, "one", Target{ID: "-1008", Name: "other"}, testMessage(6, 0, 2), 0, "history"); err != nil {
		t.Fatal(err)
	}
	var ranked, other int
	if err := s.reader.QueryRow(`SELECT sched_order FROM tgdl_work WHERE group_id='-1009'`).Scan(&ranked); err != nil {
		t.Fatal(err)
	}
	if err := s.reader.QueryRow(`SELECT sched_order FROM tgdl_work WHERE group_id='-1008'`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if ranked != 8 || other != UnrankedOrder {
		t.Fatalf("sched_order ranked=%d other=%d", ranked, other)
	}
}

// The claim stays an index walk with the rank in it, at any queue size.
func TestRankedClaimKeepsItsIndex(t *testing.T) {
	s := testStore(t)
	for i := 1; i <= 300; i++ {
		addRankedRow(t, s, "g"+strconv.Itoa(i%7), i, "id"+strconv.Itoa(i), int64(i%3)<<30, 8*(i%5+1))
	}
	if _, err := s.writer.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	withLane(t, 2)
	plan := claimPlan(t, s, []string{"one"})
	if !strings.Contains(plan, "idx_tgdl_work_claim") || strings.Contains(plan, "TEMP B-TREE") || strings.Contains(plan, "SCAN tgdl_work") {
		t.Fatalf("the ranked claim left its index: %s", plan)
	}
}

func TestIdentitySavedLooksAtTheLibrary(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_size,file_type,file_path,status,telegram_media_kind,telegram_media_id,telegram_media_size) VALUES('-1',1,'a',5,'video','a.mp4','completed','video','42',5)`); err != nil {
		t.Fatal(err)
	}
	if !s.IdentitySaved(ctx, "video\x0042\x005") {
		t.Fatal("saved file not seen")
	}
	if s.IdentitySaved(ctx, "video\x0043\x005") || s.IdentitySaved(ctx, "") {
		t.Fatal("unknown file seen as saved")
	}
}

// A file waiting for its slot gives way to better work: the row goes back to
// the queue as it was, no attempt spent, and the worker takes the better one.
func TestWorkerYieldReturnsTheRowUntouched(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, _, err := s.Enqueue(ctx, "one", Target{}, testMessage(1, 1, 1)); err != nil {
		t.Fatal(err)
	}
	factory := func(AccountConfig, *telegram.UpdateState, func(context.Context, tg.UpdatesClass) error, func(int64)) (Account, error) {
		return &readyAccount{}, nil
	}
	filter := func(context.Context, string, *tg.Message, tg.UpdatesClass) (Target, bool, error) {
		return Target{}, true, nil
	}
	var mu sync.Mutex
	var seen []int
	sink := func(_ context.Context, w *Work, m *tg.Message, _ telegram.MediaDownloader) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, m.ID)
		if len(seen) == 1 {
			// Better work arrives while this one waits for a slot.
			if _, err := s.writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,sched_order,created_at,updated_at)
				SELECT account_id,group_id,group_name,2,version,'other',media_type,file_name,file_size,body,'pending','history',8,created_at,updated_at FROM tgdl_work WHERE id=?`, w.ID); err != nil {
				t.Error(err)
			}
			return fmt.Errorf("download: %w", telegram.ErrDownloadYield)
		}
		return nil
	}
	c := New(s.writer, s.reader, t.TempDir(), factory, filter, sink, nil)
	if err := c.Start(ctx, ctx, []AccountConfig{{ID: "one"}}, 1, 1); err != nil {
		t.Fatal(err)
	}
	defer c.Stop(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var left int
		if err := s.reader.QueryRow(`SELECT COUNT(*) FROM tgdl_work WHERE status<>'completed'`).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if left == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen[0] != 1 {
		t.Fatalf("worker saw %v, want 1, then the better row, then 1 again", seen)
	}
	var attempts int
	var failure sql.NullString
	if err := s.reader.QueryRow(`SELECT attempts,error FROM tgdl_work WHERE message_id=1`).Scan(&attempts, &failure); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || failure.Valid {
		t.Fatalf("attempts=%d error=%v: giving way must cost nothing", attempts, failure)
	}
}

type refreshCounter struct {
	readyAccount
	refreshes atomic.Int64
}

func (a *refreshCounter) RefreshMessage(ctx context.Context, m *tg.Message) (*telegram.RefreshedMessage, error) {
	a.refreshes.Add(1)
	return a.connectingAccount.RefreshMessage(ctx, m)
}

// A row of a file already saved costs Telegram nothing; any other old row, or a
// second attempt at a saved one (its file may have proved missing), refreshes.
func TestSavedFileNeedsNoRefresh(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	for _, c := range []struct {
		msg, media, attempts int
		want                 int64
	}{
		{1, 1, 0, 0}, // saved below
		{2, 2, 0, 1}, // not saved
		{3, 1, 1, 1}, // saved, but a retry
	} {
		if _, _, err := s.Enqueue(ctx, "one", Target{}, testMessage(c.msg, 1, int64(c.media))); err != nil {
			t.Fatal(err)
		}
		if _, err := s.writer.Exec(`UPDATE tgdl_work SET created_at=1,attempts=? WHERE message_id=?`, c.attempts, c.msg); err != nil {
			t.Fatal(err)
		}
		var identity string
		if err := s.reader.QueryRow(`SELECT identity FROM tgdl_work WHERE message_id=?`, c.msg).Scan(&identity); err != nil {
			t.Fatal(err)
		}
		if c.media == 1 && c.msg == 1 {
			parts := strings.Split(identity, "\x00")
			if _, err := s.writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_size,file_type,file_path,status,telegram_media_kind,telegram_media_id,telegram_media_size) VALUES('x',9,'a',4,'document','a','completed',?,?,4)`, parts[0], parts[1]); err != nil {
				t.Fatal(err)
			}
		}
		account := &refreshCounter{}
		factory := func(AccountConfig, *telegram.UpdateState, func(context.Context, tg.UpdatesClass) error, func(int64)) (Account, error) {
			return account, nil
		}
		filter := func(context.Context, string, *tg.Message, tg.UpdatesClass) (Target, bool, error) {
			return Target{}, true, nil
		}
		sink := func(context.Context, *Work, *tg.Message, telegram.MediaDownloader) error { return nil }
		ctrl := New(s.writer, s.reader, t.TempDir(), factory, filter, sink, nil)
		if err := ctrl.Start(ctx, ctx, []AccountConfig{{ID: "one"}}, 1, 1); err != nil {
			t.Fatal(err)
		}
		done := false
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var status string
			if err := s.reader.QueryRow(`SELECT status FROM tgdl_work WHERE message_id=?`, c.msg).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status == "completed" {
				done = true
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err := ctrl.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		if !done {
			t.Fatalf("message %d never finished", c.msg)
		}
		if got := account.refreshes.Load(); got != c.want {
			t.Fatalf("message %d: %d refreshes, want %d", c.msg, got, c.want)
		}
	}
}

// While an update waits no new file starts, except what may vanish. Going back
// brings the rest, and the queue's own pause is not involved.
func TestDrainingHoldsNewFilesAndLetsExpiringOnesGo(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	for _, id := range []int{1, 2} {
		if _, _, err := s.Enqueue(ctx, "one", Target{}, testMessage(id, 1, int64(id))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.writer.Exec(`UPDATE tgdl_work SET ttl_seconds=60 WHERE message_id=2`); err != nil {
		t.Fatal(err)
	}
	var waiting, expiring int64
	if err := s.reader.QueryRow(`SELECT id FROM tgdl_work WHERE message_id=1`).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	if err := s.reader.QueryRow(`SELECT id FROM tgdl_work WHERE message_id=2`).Scan(&expiring); err != nil {
		t.Fatal(err)
	}
	factory := func(AccountConfig, *telegram.UpdateState, func(context.Context, tg.UpdatesClass) error, func(int64)) (Account, error) {
		return &readyAccount{}, nil
	}
	filter := func(context.Context, string, *tg.Message, tg.UpdatesClass) (Target, bool, error) {
		return Target{}, true, nil
	}
	var mu sync.Mutex
	var seen []int64
	sink := func(_ context.Context, w *Work, _ *tg.Message, _ telegram.MediaDownloader) error {
		mu.Lock()
		seen = append(seen, w.ID)
		mu.Unlock()
		return nil
	}
	c := New(s.writer, s.reader, t.TempDir(), factory, filter, sink, nil)
	c.SetDraining(true)
	if err := c.Start(ctx, ctx, []AccountConfig{{ID: "one"}}, 1, 1); err != nil {
		t.Fatal(err)
	}
	defer c.Stop(ctx)
	got := func() []int64 {
		mu.Lock()
		defer mu.Unlock()
		return append([]int64(nil), seen...)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(got()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if g := got(); len(g) != 1 || g[0] != expiring {
		t.Fatalf("while draining the worker took %v, want only %d", g, expiring)
	}
	if files, left, err := c.DrainStatus(ctx); err != nil || files != 0 || left != 0 {
		t.Fatalf("drain status %d %d %v", files, left, err)
	}
	c.SetDraining(false)
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(got()) < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if g := got(); len(g) != 2 || g[1] != waiting {
		t.Fatalf("after going back the worker took %v", g)
	}
}

// A file still waiting for its slot goes back to the queue when an update waits.
func TestDrainingMakesWaitingFilesGiveWay(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, _, err := s.Enqueue(ctx, "one", Target{}, testMessage(1, 1, 1)); err != nil {
		t.Fatal(err)
	}
	factory := func(AccountConfig, *telegram.UpdateState, func(context.Context, tg.UpdatesClass) error, func(int64)) (Account, error) {
		return &readyAccount{}, nil
	}
	filter := func(context.Context, string, *tg.Message, tg.UpdatesClass) (Target, bool, error) {
		return Target{}, true, nil
	}
	var c *Controller
	asked := make(chan bool, 1)
	sink := func(wctx context.Context, _ *Work, _ *tg.Message, _ telegram.MediaDownloader) error {
		c.SetDraining(true)
		asked <- telegram.ShouldYieldForTest(wctx)
		return telegram.ErrDownloadYield
	}
	c = New(s.writer, s.reader, t.TempDir(), factory, filter, sink, nil)
	if err := c.Start(ctx, ctx, []AccountConfig{{ID: "one"}}, 1, 1); err != nil {
		t.Fatal(err)
	}
	defer c.Stop(ctx)
	select {
	case yes := <-asked:
		if !yes {
			t.Fatal("a file waiting for its slot does not give way to an update")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no attempt")
	}
	deadline := time.Now().Add(5 * time.Second)
	var status string
	var attempts int
	for time.Now().Before(deadline) {
		if err := s.reader.QueryRow(`SELECT status,attempts FROM tgdl_work`).Scan(&status, &attempts); err != nil {
			t.Fatal(err)
		}
		if status == "pending" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if status != "pending" || attempts != 0 {
		t.Fatalf("status=%s attempts=%d", status, attempts)
	}
}
