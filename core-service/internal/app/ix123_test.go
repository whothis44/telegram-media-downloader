package app

import "testing"

// ix.12.3: the report's per-server figures, by server number.
func TestIxDCList(t *testing.T) {
	got := ixDCList(map[int]int64{4: 10 << 20, 1: 820 << 20}, map[int]int{1: 3, 5: 2})
	if want := "DC1 820 MB (3 limit waits), DC4 10 MB, DC5 0 MB (2 limit waits)"; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	if got := ixDCList(nil, nil); got != "" {
		t.Fatalf("%q", got)
	}
}

// A lesser copy of a file still queued is held with no saved look-alike
// (like_key ''). It is held all the same: a probable copy in the coverage, not
// a file still needed; and the group is not complete until it comes (ix.12.3).
func TestIxLesserCopyOfAQueuedFileIsHeld(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{})
	ixSeedIndex(t, a)
	const g = "-1000000000102"
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_messages(group_id,message_id,date,text,media_type,media_key,seen_at) VALUES(?,21,1700001000,'','video','video:888:4096',1)`, g); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_held(work_id,group_id,message_id,like_path,like_key,reason,held_at,released_at) VALUES(9001,?,21,'','','a lesser copy of a queued file',1,0)`, g); err != nil {
		t.Fatal(err)
	}
	m, err := a.ixRebuildModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pi := m.ByMsg[g+":21"]
	var it *ixMItem
	for _, ii := range m.Posts[pi].Items {
		if m.Items[ii].Msg == 21 {
			it = &m.Items[ii]
		}
	}
	if it == nil || it.Status != "held" || it.Like != "" {
		t.Fatalf("%+v", it)
	}
	if c := ixCoverage(m, map[string]bool{}, nil)[g]; c.Probable.Files != 1 || c.Needed.Files != 0 {
		t.Fatalf("coverage %+v", c)
	}
	box := a.ixModelBox()
	box.mu.Lock()
	box.model = m
	box.mu.Unlock()
	if togo, _ := a.ixFilesToGo(); togo[g].ToGo != 1 || togo[g].Held != 0 {
		t.Fatalf("files to go %+v", togo[g])
	}
}
