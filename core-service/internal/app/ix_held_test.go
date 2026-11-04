package app

import (
	"testing"
	"time"
)

// ---------- 2. copies waiting together ----------

func TestIxQueuedCopiesWaitForTheBest(t *testing.T) {
	a, ctx := ixTestApp(t)
	sceneA := ixTestStripped(t, ixTestScene(11, 40, 23))
	sceneB := ixTestStripped(t, ixTestScene(12, 40, 23))
	sceneC := ixTestStripped(t, ixTestScene(13, 40, 23))
	v1 := ixTestVideo(1, 201, 50<<20, 60, 1280, 720, sceneA, 1700000000)
	v2 := ixTestVideo(2, 202, 80<<20, 60.2, 1920, 1080, sceneA, 1700000100) // the best
	v3 := ixTestVideo(3, 203, 40<<20, 59.8, 1280, 720, sceneA, 1700000200)
	v4 := ixTestVideo(4, 204, 30<<20, 60, 1280, 720, sceneB, 1700000300) // as long, another video
	id1, id2, id3, id4 := ixTestQueue(t, a, v1), ixTestQueue(t, a, v2), ixTestQueue(t, a, v3), ixTestQueue(t, a, v4)
	if err := a.ixHeldStep(ctx); err != nil {
		t.Fatal(err)
	}
	if ixTestPaused(t, a, id2) || ixTestPaused(t, a, id4) {
		t.Fatal("the best copy, or a unique video, was held")
	}
	for _, id := range []int64{id1, id3} {
		if how, of := ixTestHow(a, id); !ixTestPaused(t, a, id) || how != "queued" || of != id2 {
			t.Fatalf("item %d: held %v as %q of %d", id, ixTestPaused(t, a, id), how, of)
		}
	}
	// Held copies do not count towards a backfill's queue limit.
	if n := a.ixHeldWaiting(ctx); n != 2 {
		t.Fatalf("held waiting: %d", n)
	}
	// The best copy is saved: the others now wait as copies of that file.
	ixTestSave(t, a, v2, "c/v2.mp4", "bb22")
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='completed' WHERE id=?`, id2)
	if err := a.ixHeldStep(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{id1, id3} {
		var like string
		_ = a.db.Reader.QueryRow(`SELECT like_path FROM ix_held WHERE work_id=?`, id).Scan(&like)
		if how, _ := ixTestHow(a, id); how != "saved" || like != "c/v2.mp4" || !ixTestPaused(t, a, id) {
			t.Fatalf("item %d after the best was saved: %q %q", id, how, like)
		}
	}
	// A confirmed copy waits until everything else is done.
	_, _ = a.db.Writer.Exec(`UPDATE ix_copy_hold SET confirm='confirmed' WHERE work_id=?`, id1)
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='completed' WHERE id=?`, id4)
	retry := ixTestQueue(t, a, ixTestVideo(9, 299, 1<<20, 5, 640, 360, sceneC, 1700000900))
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET retry_at=? WHERE id=?`, time.Now().Add(time.Hour).UnixMilli(), retry)
	if err := a.ixHeldStep(ctx); err != nil {
		t.Fatal(err)
	}
	if ixTestPaused(t, a, id3) || !ixTestPaused(t, a, id1) {
		t.Fatalf("release: unconfirmed held %v, confirmed held %v", ixTestPaused(t, a, id3), ixTestPaused(t, a, id1))
	}
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='completed' WHERE id IN (?,?)`, retry, id3)
	if err := a.ixHeldStep(ctx); err != nil {
		t.Fatal(err)
	}
	if ixTestPaused(t, a, id1) {
		t.Fatal("the confirmed copy still waits with nothing else left")
	}
	if st := a.ixHeldStatus(ctx); st.Held != 0 {
		t.Fatalf("status: %+v", st)
	}
}

func TestIxHeldCopiesGoWhenTheirBestFailsForGood(t *testing.T) {
	a, ctx := ixTestApp(t)
	scene := ixTestStripped(t, ixTestScene(21, 40, 23))
	other := ixTestStripped(t, ixTestScene(22, 40, 23))
	best := ixTestQueue(t, a, ixTestVideo(1, 301, 90<<20, 120, 1920, 1080, scene, 1700000000))
	mid := ixTestQueue(t, a, ixTestVideo(2, 302, 60<<20, 120, 1280, 720, scene, 1700000100))
	low := ixTestQueue(t, a, ixTestVideo(3, 303, 30<<20, 120, 854, 480, scene, 1700000200))
	ixTestQueue(t, a, ixTestVideo(4, 304, 30<<20, 50, 854, 480, other, 1700000300))
	if err := a.ixHeldStep(ctx); err != nil {
		t.Fatal(err)
	}
	if !ixTestPaused(t, a, mid) || !ixTestPaused(t, a, low) {
		t.Fatal("lesser copies not held")
	}
	// Failed, but automatic retries will try again: they keep waiting.
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='failed' WHERE id=?`, best)
	if err := a.ixHeldStep(ctx); err != nil {
		t.Fatal(err)
	}
	if !ixTestPaused(t, a, mid) {
		t.Fatal("let go while the best copy may still be retried")
	}
	// Given up: the best of the waiting copies goes, the other waits for it.
	_, _ = a.db.Writer.Exec(`INSERT INTO ix_retry(work_key,rounds,last_at) VALUES(?,?,0)`, ixTestGroup+"_1", ixRetryRounds)
	if err := a.ixHeldStep(ctx); err != nil {
		t.Fatal(err)
	}
	if ixTestPaused(t, a, mid) {
		t.Fatal("the next best copy did not go")
	}
	if how, of := ixTestHow(a, low); how != "queued" || of != mid || !ixTestPaused(t, a, low) {
		t.Fatalf("the last copy: %q of %d", how, of)
	}
	// A person releasing everything gets every held file, confirmed or not.
	_, _ = a.db.Writer.Exec(`UPDATE ix_copy_hold SET confirm='confirmed' WHERE work_id=?`, low)
	if err := a.ixReleaseHeld(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if ixTestPaused(t, a, low) {
		t.Fatal("not released when asked")
	}
}

// Files queued before this version, already looked at by the old check
// (released_at -1), are checked again: against videos saved since, and
// against each other.
func TestIxFilesQueuedBeforeAreCheckedToo(t *testing.T) {
	a, ctx := ixTestApp(t)
	scene := ixTestStripped(t, ixTestScene(31, 40, 23))
	pair := ixTestStripped(t, ixTestScene(32, 40, 23))
	big := ixTestQueue(t, a, ixTestVideo(1, 401, 3<<30, 1800, 1920, 1080, scene, 1700000000)) // 3 GB
	p1 := ixTestQueue(t, a, ixTestVideo(2, 402, 900<<20, 600, 1280, 720, pair, 1700000100))
	p2 := ixTestQueue(t, a, ixTestVideo(3, 403, 700<<20, 600, 1280, 720, pair, 1700000200))
	for _, id := range []int64{big, p1, p2} {
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_held(work_id,group_id,message_id,reason,held_at,released_at) SELECT id, group_id, message_id, '', 1, -1 FROM tgdl_work WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	// A better copy of the 3 GB video was saved after it was queued.
	saved := ixTestVideo(9, 409, 3<<30+5<<20, 1800.2, 1920, 1080, scene, 1690000000)
	ixTestSave(t, a, saved, "c/long.mp4", "cc33")
	if err := a.ixHeldStep(ctx); err != nil {
		t.Fatal(err)
	}
	if how, _ := ixTestHow(a, big); how != "saved" || !ixTestPaused(t, a, big) {
		t.Fatalf("the 3 GB copy of a saved video: %q", how)
	}
	if how, of := ixTestHow(a, p2); how != "queued" || of != p1 || ixTestPaused(t, a, p1) {
		t.Fatalf("queued pair: %q of %d", how, of)
	}
}
