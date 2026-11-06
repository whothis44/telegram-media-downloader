package app

import (
	"testing"
)

// The queue held at its limit on 11-06 08:03-10:43, invented data: claimable
// files of 20 MB or more on one server, whose 2 large slots are taken, fill
// the backfill's limit, so the rescan queues nothing. From 12.8 they wait
// apart (as big files waiting for their lane do), under the same bound of
// twice the limit; files whose server has a free slot still count.
func TestIx128FilesWaitingForTheirServerDoNotFillTheQueueLimit(t *testing.T) {
	ixTestLane(t, 2)
	old := ixFileSlots
	ixFileSlots = func(*App) (int, int) { return 5, 2 }
	t.Cleanup(func() { ixFileSlots = old })
	a, ctx := ix8App(t, map[string]any{"deadlineOrder": "off"})
	cfg, err := a.config.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ensureMap(cfg, "advanced")["history"] = map[string]any{"backpressureCap": 20}
	if err = a.config.Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	const mb = 1 << 20
	msg := 0
	row := func(dc int, size int64, status string) {
		msg++
		id := ixOldQueueRow(t, a, "-1001", msg, size)
		if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET dc=?, status=? WHERE id=?`, dc, status, id); err != nil {
			t.Fatal(err)
		}
	}
	// DC1: both large slots taken, 30 large files waiting for them.
	row(1, 50*mb, "processing")
	row(1, 50*mb, "processing")
	for i := 0; i < 30; i++ {
		row(1, 50*mb, "pending")
	}
	// DC2: 3 small files, with 5 small slots free; DC4: one large file, 1 of
	// its 2 large slots free; a file of a server not known yet.
	for i := 0; i < 3; i++ {
		row(2, 5*mb, "pending")
	}
	row(4, 50*mb, "processing")
	row(4, 50*mb, "pending")
	row(0, 50*mb, "pending")
	ixTestBackfill(t, a, ctx, "-1002")

	// Before 12.8: 38 queued, limit 20, the rescan held. Now the 30 DC1
	// files wait apart: 3 + 1 + 1 waiting and 3 downloading count.
	b := a.ixBigFilesMeasure(ctx)
	if b.SlotBlocked != 30 || b.QueueCount != 8 || b.CapPaused {
		t.Fatalf("30 files waiting for DC1's large slots, limit 20: %+v", b)
	}

	// Bounded: with 45 more on DC1 (75 waiting, the bound is 40), 35 count.
	for i := 0; i < 45; i++ {
		row(1, 50*mb, "pending")
	}
	if b = a.ixBigFilesMeasure(ctx); b.SlotBlocked != 75 || b.QueueCount != 8+35 || !b.CapPaused {
		t.Fatalf("75 waiting for DC1, bound 40: %+v", b)
	}

	// Slots unknown (the engine not started): nothing is left out.
	ixFileSlots = func(*App) (int, int) { return 0, 0 }
	if b = a.ixBigFilesMeasure(ctx); b.SlotBlocked != 0 || b.QueueCount != 83 {
		t.Fatalf("slots unknown: %+v", b)
	}
}
