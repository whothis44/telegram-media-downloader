package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
)

// The state seen live on 11-06 08:03, invented data: every claimable file is
// a ranked group's big file on DC1 or DC4, whose 2 large slots are full; the
// next band's files (private chats, 10-100 MB) are set aside for the smaller
// files a rescan should bring, but the rescan is held by the queue limit.
// Before 12.7 keep-busy counted the ranked files as work and let nothing go,
// so 4 files downloaded and every small slot and DC5 sat idle. Now the
// set-aside files fill the free slots: on their own server, of their size,
// held copies left alone, and nothing more for the full servers.
func TestIx127SetAsideFilesFillFreeServerSlots(t *testing.T) {
	ixTestWorkers(t, 25)
	ixTestLane(t, 0)
	old := ixFileSlots
	ixFileSlots = func(*App) (int, int) { return 5, 2 }
	t.Cleanup(func() { ixFileSlots = old })
	a, ctx := ix8App(t, map[string]any{"deadlineOrder": "on"})

	msg := 0
	row := func(dc int, size int64, ranked bool, status string) int64 {
		msg++
		group := "-1001"
		if ranked {
			group = "-1009" // a ranked group
		}
		id := ixOldQueueRow(t, a, group, msg, size)
		order := engine.UnrankedOrder
		if ranked {
			order = 1
		}
		if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET dc=?, sched_order=?, status=? WHERE id=?`, dc, order, status, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if err := a.ixSaveRanks(ctx, []string{"-1009"}, map[string]string{"-1009": "Ranked"}); err != nil {
		t.Fatal(err)
	}
	const mb = 1 << 20
	// Ranked big files: 2 downloading on DC1 and DC4 each, 520 waiting.
	for _, dc := range []int{1, 4} {
		row(dc, 50*mb, true, "processing")
		row(dc, 50*mb, true, "processing")
	}
	for i := 0; i < 500; i++ {
		row(1, 50*mb, true, "pending")
	}
	for i := 0; i < 20; i++ {
		row(4, 50*mb, true, "pending")
	}
	// The next band, unranked: small (10-20 MB) and large files.
	for i := 0; i < 8; i++ {
		row(1, 15*mb, false, "pending")
		row(2, 15*mb, false, "pending")
		row(5, 15*mb, false, "pending")
	}
	for i := 0; i < 4; i++ {
		row(1, 60*mb, false, "pending")
		row(5, 60*mb, false, "pending")
	}
	// A held copy on DC5 stays held.
	held := row(5, 12*mb, false, "pending")
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET paused=1 WHERE id=?`, held); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_held(work_id,group_id,message_id,reason,held_at) VALUES(?,'-1001',1,'like',1)`, held); err != nil {
		t.Fatal(err)
	}
	ixTestBackfill(t, a, ctx, "-1003") // a private chat still to read: band 0 to come
	// From 12.8 the files waiting for DC1's and DC4's large slots do not
	// count towards the queue limit (TestIx128...); a limit of 10 still holds
	// the rescan here with what remains, as the 520 did on 12.7.
	cfg, err := a.config.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ensureMap(cfg, "advanced")["history"] = map[string]any{"backpressureCap": 10}
	if err = a.config.Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if b := a.ixBigFilesMeasure(ctx); !b.CapPaused {
		t.Fatalf("the rescan should be held by the queue limit: %+v", b)
	}

	if err := a.ixDeadlineStep(ctx); err != nil {
		t.Fatal(err)
	}
	type key struct {
		dc  int
		big bool
	}
	let := map[key]int{}
	rows, err := a.db.Reader.Query(`SELECT dc, file_size>=20971520, COUNT(*) FROM tgdl_work WHERE status='pending' AND paused=0 AND sched_order>=? GROUP BY 1,2`, engine.UnrankedOrder)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k key
		var n int
		if err := rows.Scan(&k.dc, &k.big, &n); err != nil {
			t.Fatal(err)
		}
		let[k] = n
	}
	rows.Close()
	want := map[key]int{{1, false}: 5, {2, false}: 5, {5, false}: 5, {5, true}: 2}
	for k, n := range want {
		if let[k] != n {
			t.Errorf("DC%d large=%v: %d let go, want %d (all: %v)", k.dc, k.big, let[k], n, let)
		}
	}
	if let[key{1, true}] != 0 {
		t.Errorf("DC1's full large slots got %d more files", let[key{1, true}])
	}
	var paused int
	_ = a.db.Reader.QueryRow(`SELECT paused FROM tgdl_work WHERE id=?`, held).Scan(&paused)
	if paused != 1 {
		t.Error("a held copy was let go")
	}

	// The workers take exactly those: each free slot gets a file, the full
	// servers none.
	s := engine.NewWorkStore(a.db.Writer, a.db.Reader)
	got := map[key]int{}
	for i := 0; i < 40; i++ {
		full, err := s.FullServers(ctx, 5, 2)
		if err != nil {
			t.Fatal(err)
		}
		w, err := s.ClaimAvoiding(context.Background(), []string{"acc"}, time.Now(), true, full)
		if err != nil {
			t.Fatal(err)
		}
		if w == nil {
			break
		}
		var dc int
		_ = a.db.Reader.QueryRow(`SELECT dc FROM tgdl_work WHERE id=?`, w.ID).Scan(&dc)
		got[key{dc, w.FileSize >= 20<<20}]++
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("claimed on DC%d large=%v: %d, want %d (all: %v)", k.dc, k.big, got[k], n, got)
		}
	}
	if got[key{1, true}]+got[key{4, true}] != 0 {
		t.Errorf("a full server got a file: %v", got)
	}

	// The report says what happened.
	r := a.ixDiagnostics(ctx)
	if !strings.Contains(r, "set-aside files let go into free slots: 17 since start") || !strings.Contains(r, "DC5 small 5/5") {
		t.Fatalf("report: %s", r[strings.Index(r, "file slots"):][:400])
	}
}
