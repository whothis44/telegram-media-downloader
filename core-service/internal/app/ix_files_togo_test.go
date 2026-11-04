package app

import "testing"

// A group whose messages are all in is not complete while files of them are
// still to come; files of messages deleted on Telegram cannot come and do
// not count, and likely copies held aside are counted apart.
func TestIxFilesToGoPerGroup(t *testing.T) {
	a, _ := ix8App(t, nil)
	if _, ok := a.ixFilesToGo(); ok {
		t.Fatal("known before the browser index was built")
	}
	m := &ixModel{
		Posts: []ixMPost{{Idx: 0, Group: "-1001"}, {Idx: 1, Group: "-1001", Deleted: true}, {Idx: 2, Group: "-1002"}},
		Items: []ixMItem{
			{Group: "-1001", Status: "ok", Post: 0},
			{Group: "-1001", Status: "missing", Post: 0},
			{Group: "-1001", Status: "missing", Post: 0},
			{Group: "-1001", Status: "held", Post: 0},
			{Group: "-1001", Status: "missing", Post: 1}, // deleted on Telegram
			{Group: "-1002", Status: "subst", Post: 2},
			{Group: "-1002", Status: "ok", Post: 2},
		},
	}
	box := a.ixModelBox()
	box.mu.Lock()
	box.model = m
	box.mu.Unlock()
	got, ok := a.ixFilesToGo()
	if !ok {
		t.Fatal("not known with an index")
	}
	if g := got["-1001"]; g.ToGo != 2 || g.Held != 1 {
		t.Fatalf("-1001: %+v", g)
	}
	if g := got["-1002"]; g.ToGo != 0 || g.Held != 0 {
		t.Fatalf("-1002: %+v", g)
	}
}
