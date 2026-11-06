package app

import (
	"encoding/json"
	"testing"
)

// ix.12.5: evidence the hold rules did not use. A held video whose sampled
// frames match no saved video goes back to its place; a partial match stays
// at the back as one; a failed check changes nothing; a hold with no record
// of why is forgotten.
func TestIxHeldVideosFollowTheirSampledEvidence(t *testing.T) {
	a, ctx := ixTestApp(t)
	hold := func(doc int, how string) int64 {
		id := ixTestQueue(t, a, ixTestVideo(doc, int64(doc), 300<<20, 600, 1280, 720, nil, 1700000000))
		mustExec(t, a, `UPDATE tgdl_work SET paused=1 WHERE id=?`, id)
		mustExec(t, a, `INSERT INTO ix_held(work_id,group_id,message_id,like_key,reason,held_at) VALUES(?,?,?,'h:k','looks like a saved copy',1)`, id, ixTestGroup, doc)
		if how != "" {
			mustExec(t, a, `INSERT INTO ix_copy_hold(work_id,how) VALUES(?,?)`, id, how)
		}
		return id
	}
	sample := func(id int64, result, note string, samples int) {
		mustExec(t, a, `INSERT INTO ix_copy_sample(work_id,ident,size,result,note,samples,at) VALUES(?,'d:x',1,?,?,?,1)`, id, result, note, samples)
	}
	none := hold(901, "saved")
	sample(none, "nomatch", "at most 1 of 8 sampled frames match a saved video", 8)
	part := hold(902, "saved")
	sample(part, "nomatch", "at most 4 of 8 sampled frames match a saved video", 8)
	failed := hold(903, "saved")
	sample(failed, "failed", "the check took too long", 0)
	matched := hold(904, "saved")
	sample(matched, "confirmed", "5 of 5 sampled frames match at -0 s", 5)
	unknown := hold(905, "")

	if err := a.ixHeldOutweighed(ctx); err != nil {
		t.Fatal(err)
	}
	if ixTestPaused(t, a, none) {
		t.Fatal("a held video matching no saved video stayed held")
	}
	var released int64
	_ = a.db.Reader.QueryRow(`SELECT released_at FROM ix_held WHERE work_id=?`, none).Scan(&released)
	if released == 0 {
		t.Fatal("not let go for good")
	}
	if !ixTestPaused(t, a, part) {
		t.Fatal("a partial match left the back of the queue")
	}
	if how, _ := ixTestHow(a, part); how != "sampled-partial" {
		t.Fatalf("partial match how=%q", how)
	}
	if !ixTestPaused(t, a, failed) || !ixTestPaused(t, a, matched) {
		t.Fatal("a failed check or a match changed the hold")
	}
	if ixTestPaused(t, a, unknown) {
		t.Fatal("a hold with no record of why stayed")
	}
	var n int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_held WHERE work_id=?`, unknown).Scan(&n)
	if n != 0 {
		t.Fatal("a hold with no record of why was not forgotten")
	}
	// Once is enough: a second pass changes nothing.
	if err := a.ixHeldOutweighed(ctx); err != nil {
		t.Fatal(err)
	}
	if !ixTestPaused(t, a, part) || !ixTestPaused(t, a, failed) {
		t.Fatal("a second pass moved files")
	}
}

// One check in three goes to a held video (ranked groups first, then the
// largest); the others go to the queue in download order.
func TestIxSampleTurnsGiveHeldVideosOneInThree(t *testing.T) {
	a, ctx := ixTestApp(t)
	var front, held []int64
	for i := 0; i < 4; i++ {
		front = append(front, ixTestQueue(t, a, ixTestVideo(910+i, int64(910+i), 300<<20, 600, 1280, 720, nil, 1700000000)))
	}
	for i := 0; i < 2; i++ {
		id := ixTestQueue(t, a, ixTestVideo(920+i, int64(920+i), int64(300+100*i)<<20, 600, 1280, 720, nil, 1700000000))
		mustExec(t, a, `UPDATE tgdl_work SET paused=1 WHERE id=?`, id)
		mustExec(t, a, `INSERT INTO ix_held(work_id,group_id,message_id,reason,held_at) VALUES(?,?,?,'looks like a saved copy',1)`, id, ixTestGroup, 920+i)
		held = append(held, id)
	}
	jobs, err := a.ixSampleTurns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, j := range jobs {
		got = append(got, j.work)
	}
	if len(got) != 6 || !got2held(got[2], held[1]) || !got2held(got[5], held[0]) {
		t.Fatalf("turns %v (held %v): want a held video third and sixth, the larger first", got, held)
	}
	for _, i := range []int{0, 1, 3, 4} {
		if jobs[i].held {
			t.Fatalf("turn %d was a held video", i)
		}
	}
}

func got2held(a, b int64) bool { return a == b }

// Sampling has its own switch; an old setting follows holdLikely.
func TestIxSampleBigFollowsHoldLikelyUntilSet(t *testing.T) {
	s := ixDefaultSettings()
	if err := json.Unmarshal([]byte(`{"holdLikely":true}`), &s); err != nil {
		t.Fatal(err)
	}
	if !s.SampleBigOn() {
		t.Fatal("an old setting with holding on should sample")
	}
	s.HoldLikely = false
	if s.SampleBigOn() {
		t.Fatal("an old setting with holding off should not sample")
	}
	s, err := ixParseSettings(map[string]any{"sampleBig": true}, s)
	if err != nil || !s.SampleBigOn() || s.HoldLikely {
		t.Fatalf("set on its own: %v %v", s.SampleBigOn(), err)
	}
	data, _ := json.Marshal(s)
	var back ixSettings
	_ = json.Unmarshal(data, &back)
	if !back.SampleBigOn() {
		t.Fatal("not kept")
	}
}

func mustExec(t *testing.T, a *App, q string, args ...any) {
	t.Helper()
	if _, err := a.db.Writer.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}
