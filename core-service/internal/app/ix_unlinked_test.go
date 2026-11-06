package app

import (
	"strings"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/gotd/td/tg"
)

// Queue rows marked done with nothing saved (ix_unlinked.go), invented data.
func TestIxUnlinkedRowsGoBackToTheQueue(t *testing.T) {
	a, ctx := ixTestApp(t)
	const mb = int64(1) << 20
	saved := ixTestVideo(1, 501, 40*mb, 30, 1280, 720, nil, 100) // a saved file
	copyOf := ixTestVideo(2, 502, 40*mb, 30, 1280, 720, nil, 200) // same size, another Telegram file: done, nothing saved
	alone := ixTestVideo(3, 503, 41*mb, 30, 1280, 720, nil, 300)  // no saved file of its size
	gone := ixTestVideo(4, 504, 40*mb, 30, 1280, 720, nil, 400)   // its message deleted on Telegram
	linked := ixTestVideo(5, 505, 40*mb, 30, 1280, 720, nil, 500) // linked to the saved file (ix.10.2 and later)
	same := ixTestVideo(6, 501, 40*mb, 30, 1280, 720, nil, 600)   // the same Telegram file as the saved one
	normal := ixTestVideo(7, 507, 42*mb, 30, 1280, 720, nil, 700) // done and saved
	for _, m := range []*tg.Message{saved, copyOf, alone, gone, linked, same, normal} {
		ixTestArchive(t, a, m)
	}
	ixTestSave(t, a, saved, "c/v1.mp4", ixSixtyFour("a"))
	ixTestSave(t, a, normal, "c/v7.mp4", ixSixtyFour("b"))
	ids := map[int]int64{}
	for _, m := range []*tg.Message{copyOf, alone, gone, linked, same, normal} {
		ids[m.ID] = ixTestQueue(t, a, m)
		mustExec(t, a, `UPDATE tgdl_work SET status='completed', body=x'', attempts=1 WHERE id=?`, ids[m.ID])
	}
	mustExec(t, a, `UPDATE ix_messages SET deleted_at=1 WHERE message_id=4`)
	mustExec(t, a, `INSERT INTO ix_msg_link(group_id,message_id,how,of_path,sha256,size,tg_file,name,type,at) VALUES(?,5,'exact-reupload','c/v1.mp4','',?,'','v5.mp4','video',1)`, ixTestGroup, 40*mb)

	res, err := a.ixRepairUnlinked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Held != 1 || res.Queued != 1 {
		t.Fatalf("result %+v", res)
	}
	type st struct {
		status      string
		paused, gen int
		body        int
		order       int
	}
	get := func(id int64) st {
		var s st
		if err := a.db.Reader.QueryRow(`SELECT status, paused, generation, length(body), sched_order FROM tgdl_work WHERE id=?`, id).Scan(&s.status, &s.paused, &s.gen, &s.body, &s.order); err != nil {
			t.Fatal(err)
		}
		return s
	}
	// The probable copy: back, held, with the saved file it looks like.
	if s := get(ids[2]); s.status != "pending" || s.paused != 1 || s.body == 0 || s.order != engine.UnrankedOrder {
		t.Fatalf("copy: %+v", s)
	}
	var likePath, likeKey, reason, how string
	if err := a.db.Reader.QueryRow(`SELECT h.like_path, h.like_key, h.reason, c.how FROM ix_held h JOIN ix_copy_hold c ON c.work_id=h.work_id WHERE h.work_id=? AND h.released_at=0`, ids[2]).
		Scan(&likePath, &likeKey, &reason, &how); err != nil {
		t.Fatal(err)
	}
	if likePath != "c/v1.mp4" || likeKey != "h:"+ixSixtyFour("a") || how != ixUnlinkedHow || !strings.Contains(reason, "probably a copy") {
		t.Fatalf("hold: %q %q %q %q", likePath, likeKey, how, reason)
	}
	// The body is the archived message, readable by the engine and the checks.
	var body []byte
	_ = a.db.Reader.QueryRow(`SELECT body FROM tgdl_work WHERE id=?`, ids[2]).Scan(&body)
	if m, ok := ixDecodeMessage(body); !ok || m.ID != 2 {
		t.Fatal("body is not the message")
	}
	// No saved file of its size: an ordinary download.
	if s := get(ids[3]); s.status != "pending" || s.paused != 0 {
		t.Fatalf("alone: %+v", s)
	}
	var heldAlone int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_held WHERE work_id=?`, ids[3]).Scan(&heldAlone)
	if heldAlone != 0 {
		t.Fatal("held without a look-alike")
	}
	// Left alone: deleted, linked, the same Telegram file saved, saved.
	for _, m := range []int{4, 5, 6, 7} {
		if s := get(ids[m]); s.status != "completed" {
			t.Fatalf("message %d touched: %+v", m, s)
		}
	}
	// It counts at the back with the probable copies.
	o, err := a.ixOrderNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if o.Waiting[ixStageHeld] != 1 || o.Waiting[ixStageWork] != 1 {
		t.Fatalf("order: %+v", o.Waiting)
	}
	// Once only.
	mustExec(t, a, `UPDATE tgdl_work SET status='completed', paused=0 WHERE id=?`, ids[3])
	if res, err := a.ixRepairUnlinked(ctx); err != nil || res.Held+res.Queued != 0 {
		t.Fatalf("ran again: %+v %v", res, err)
	}
}
