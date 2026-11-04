package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------- lone files placed in albums ----------

const ixAlbChat = "-1000000000201"

// ixAlbSeed: one chat with albums, a text message, a lone photo and files
// whose messages were never captured (orphans) next to them. All invented.
func ixAlbSeed(t *testing.T, a *App) {
	t.Helper()
	ctx := context.Background()
	cfg, err := a.config.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg["groups"] = []any{map[string]any{"id": ixAlbChat, "name": "albums", "enabled": true}}
	if err = a.config.Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	day := int64(1700000000)
	msg := func(id int, grouped, media string) {
		t.Helper()
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_messages(group_id,message_id,date,text,grouped_id,media_type,seen_at) VALUES(?,?,?,'',?,?,1)`, ixAlbChat, id, day+int64(id), grouped, media); err != nil {
			t.Fatal(err)
		}
	}
	file := func(id int, created string) {
		t.Helper()
		if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,status,file_hash,created_at) VALUES(?,?,?,?,?,?,?,'completed',?,?)`,
			ixAlbChat, "albums", id, fmt.Sprintf("f%d.jpg", id), 1000, "photo", fmt.Sprintf("albums/f%d.jpg", id), fmt.Sprintf("%064x", id), created); err != nil {
			t.Fatal(err)
		}
	}
	later := "2023-11-15 00:00:00"
	// Album A: 10-12, and orphan 13 after it (the next captured message is far).
	for _, id := range []int{10, 11, 12} {
		msg(id, "A", "photo")
		file(id, later)
	}
	file(13, later)
	// Album B: 30-31, a text message at 33: orphan 32 sits between them.
	msg(30, "B", "photo")
	msg(31, "B", "photo")
	file(30, later)
	file(31, later)
	msg(33, "", "")
	file(32, later)
	// Albums C (40-41) and D (44-45): orphan 42 between two albums.
	for _, x := range []struct {
		id int
		g  string
	}{{40, "C"}, {41, "C"}, {44, "D"}, {45, "D"}} {
		msg(x.id, x.g, "photo")
		file(x.id, later)
	}
	file(42, later)
	// Album E: 50-51, orphan 52 saved long before the album was posted.
	msg(50, "E", "photo")
	msg(51, "E", "photo")
	file(50, later)
	file(51, later)
	file(52, "2020-01-01 00:00:00")
	// Album F already holds 10 files: orphan 70 cannot be one more.
	for id := 60; id < 70; id++ {
		msg(id, "F", "photo")
		file(id, later)
	}
	file(70, later)
	// A lone photo at 80: orphan 81 next to it is not in an album.
	msg(80, "", "photo")
	file(80, later)
	file(81, later)
	// Album G: 90 and 93 captured, 91 and 92 not: both are placed.
	msg(90, "G", "photo")
	msg(93, "G", "photo")
	file(90, later)
	file(93, later)
	file(91, later)
	file(92, later)
}

func ixAlbPostOf(t *testing.T, m *ixModel, msg int) *ixMPost {
	t.Helper()
	pi, ok := m.ByMsg[ixAlbChat+":"+fmt.Sprint(msg)]
	if !ok {
		t.Fatalf("no post for %d", msg)
	}
	return &m.Posts[pi]
}

func TestIxLoneFilesArePlacedInTheAlbumBesideThem(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{})
	ixAlbSeed(t, a)
	m, err := a.ixRebuildModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	msgsOf := func(p *ixMPost) []int {
		var out []int
		for _, ii := range p.Items {
			out = append(out, m.Items[ii].Msg)
		}
		return out
	}
	placed := func(p *ixMPost) []int {
		var out []int
		for _, ii := range p.Items {
			if m.Items[ii].Placed {
				out = append(out, m.Items[ii].Msg)
			}
		}
		return out
	}
	// Placed: 13 in A; 91 and 92 in G, in message order.
	if p := ixAlbPostOf(t, m, 13); p.Grouped != "A" || fmt.Sprint(msgsOf(p)) != "[10 11 12 13]" || fmt.Sprint(placed(p)) != "[13]" {
		t.Fatalf("13: %+v %v", p, msgsOf(p))
	}
	if p := ixAlbPostOf(t, m, 91); p.Grouped != "G" || fmt.Sprint(msgsOf(p)) != "[90 91 92 93]" || fmt.Sprint(p.Msgs) != "[90 91 92 93]" {
		t.Fatalf("91: %+v %v", p, msgsOf(p))
	}
	// Left alone: a text message between (32), two albums (42), saved before
	// posted (52), a full album (70), not an album (81).
	for _, id := range []int{32, 42, 52, 70, 81} {
		if p := ixAlbPostOf(t, m, id); !p.Orphan || len(p.Items) != 1 {
			t.Fatalf("%d was placed: %+v", id, p)
		}
	}
	// The placed file is one family with its album, and is labelled.
	pa := ixAlbPostOf(t, m, 10)
	if len(m.Families[pa.Family].Items) != 4 {
		t.Fatalf("family of A: %+v", m.Families[pa.Family])
	}
	for _, ii := range pa.Items {
		j := m.jitem(ii)
		if j.Inferred != (j.Msg == 13) {
			t.Fatalf("label: %+v", j)
		}
	}
	// A person says 13 is not part of it: it stands alone; undo puts it back.
	if err := a.ixOrphanDecide(ctx, ixAlbChat, 13, "out"); err != nil {
		t.Fatal(err)
	}
	if a.ixOrphanDecide(ctx, ixAlbChat, 13, "maybe") == nil || a.ixOrphanDecide(ctx, "", 13, "out") == nil {
		t.Fatal("bad input accepted")
	}
	if m, err = a.ixRebuildModel(ctx); err != nil {
		t.Fatal(err)
	}
	if p := ixAlbPostOf(t, m, 13); !p.Orphan || len(ixAlbPostOf(t, m, 10).Items) != 3 {
		t.Fatalf("13 still placed: %+v", p)
	}
	if err := a.ixOrphanDecide(ctx, ixAlbChat, 13, "auto"); err != nil {
		t.Fatal(err)
	}
	if m, err = a.ixRebuildModel(ctx); err != nil {
		t.Fatal(err)
	}
	if p := ixAlbPostOf(t, m, 13); p.Orphan || p.Grouped != "A" {
		t.Fatalf("13 not placed again: %+v", p)
	}
}

// Without the database (as in the scale test) only media posts count as
// neighbours.
func TestIxOrphanPlacerUsesTheNearestNeighbours(t *testing.T) {
	m := &ixModel{ByMsg: map[string]int32{}}
	post := func(grouped string, date int64, msgs ...int) {
		pi := int32(len(m.Posts))
		m.Posts = append(m.Posts, ixMPost{Idx: pi, Group: "g", Grouped: grouped, Date: date, Msgs: msgs, Msg: msgs[0]})
		for _, x := range msgs {
			m.ByMsg["g:"+fmt.Sprint(x)] = pi
		}
	}
	post("A", 100, 1, 2, 3)
	post("", 100, 20)
	post("B", 100, 25, 26)
	o := newIxOrphanPlacer(m, nil, map[string]bool{"g:5": true})
	got := map[int]int32{}
	for _, msg := range []int{4, 5, 6, 12, 21, 27, 40} {
		got[msg] = o.place("g", msg, 0)
	}
	keys := []int{}
	for k := range got {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	// 4 joins A; 5 is "not part of it"; 6 then has A as its nearest captured
	// neighbour on one side and nothing within reach on the other: A; 12 is
	// beyond reach of A (more than 9 from 3); 21 sits between a lone post and
	// B; 27 joins B; 40 has no neighbour.
	want := map[int]int32{4: 0, 5: -1, 6: 0, 12: -1, 21: -1, 27: 2, 40: -1}
	for _, k := range keys {
		if got[k] != want[k] {
			t.Fatalf("%d: got %d want %d (all %v)", k, got[k], want[k], got)
		}
	}
}

// ixAlbPost adds a captured post with downloaded photos: one message per
// file, grouped when there are several.
func ixAlbPost(t *testing.T, a *App, group string, date int64, grouped string, msgs ...int) []string {
	t.Helper()
	var keys []string
	for _, id := range msgs {
		hash := fmt.Sprintf("%064x", int64(id)+int64(len(group))*1000000+int64(group[len(group)-1])*10000)
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_messages(group_id,message_id,date,text,grouped_id,media_type,seen_at) VALUES(?,?,?,'',?,'photo',1)`, group, id, date, grouped); err != nil {
			t.Fatal(err)
		}
		if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,status,file_hash,created_at) VALUES(?,?,?,?,?,?,?,'completed',?,'2023-11-15 00:00:00')`,
			group, "", id, fmt.Sprintf("f%d.jpg", id), 1000, "photo", fmt.Sprintf("c%s/f%d.jpg", group, id), hash); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, "h:"+hash)
	}
	return keys
}

func ixAlbChats(t *testing.T, a *App, ids ...string) {
	t.Helper()
	ctx := context.Background()
	cfg, err := a.config.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var groups []any
	for _, id := range ids {
		groups = append(groups, map[string]any{"id": id, "name": "chat " + id, "enabled": true})
	}
	cfg["groups"] = groups
	if err = a.config.Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestIxLoneFileIsSuggestedForItsAlbumAndCanBeAccepted(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{"mlUrl": "http://127.0.0.1:9"})
	const g1, g2 = "-1000000000301", "-1000000000302"
	ixAlbChats(t, a, g1, g2)
	day := int64(1700000000)
	album := ixAlbPost(t, a, g1, day, "A", 10, 11, 12)
	lone := ixAlbPost(t, a, g1, day+3600, "", 40)
	other := ixAlbPost(t, a, g2, day, "B", 10, 11, 12)
	far := ixAlbPost(t, a, g2, day+86400*30, "", 50)
	r := rand.New(rand.NewSource(1))
	shoot, elsewhere := ixRandUnit(r, 64), ixRandUnit(r, 64)
	for _, k := range append(append([]string{}, album...), lone...) {
		if err := a.ixMLSave(ctx, ixMLDefaultModel, k, ixMix(64, 1.0, shoot, 0.3, ixRandUnit(r, 64))); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range other {
		if err := a.ixMLSave(ctx, ixMLDefaultModel, k, ixMix(64, 1.0, elsewhere, 0.3, ixRandUnit(r, 64))); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.ixMLSave(ctx, ixMLDefaultModel, far[0], ixRandUnit(r, 64)); err != nil {
		t.Fatal(err)
	}
	m, err := a.ixRebuildModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a.ixLoneRun(ctx, m, 10*time.Second)
	var post, otherPost, why string
	if err := a.db.Reader.QueryRow(`SELECT post, other, why FROM ix_set_sug`).Scan(&post, &otherPost, &why); err != nil {
		t.Fatal(err)
	}
	if post != g1+":40" || otherPost != g1+":10" || !strings.Contains(why, "same chat") {
		t.Fatalf("suggestion: %s -> %s (%s)", post, otherPost, why)
	}
	var n int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_set_sug_seen`).Scan(&n)
	if n != 2 {
		t.Fatalf("compared %d lone files, want 2", n)
	}
	// Shown in the Review tab.
	rec := httptest.NewRecorder()
	a.handleIxLoneReview(rec, httptest.NewRequest("GET", "/api/ix/browse/review/lone", nil))
	var got struct {
		Lone []ixLoneJ `json:"lone"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Lone) != 1 || got.Lone[0].File.Key != lone[0] || got.Lone[0].AlbumFiles != 3 {
		t.Fatalf("review: %v %s", err, rec.Body.String())
	}
	if a.ixLoneDecide(ctx, "nope:1", "yes") == nil || a.ixLoneDecide(ctx, post, "maybe") == nil {
		t.Fatal("bad input accepted")
	}
	if err := a.ixLoneDecide(ctx, post, "yes"); err != nil {
		t.Fatal(err)
	}
	if m, err = a.ixRebuildModel(ctx); err != nil {
		t.Fatal(err)
	}
	fa, fb := m.Posts[m.ByMsg[g1+":40"]].Family, m.Posts[m.ByMsg[g1+":10"]].Family
	if m.FamSet[fa] < 0 || m.FamSet[fa] != m.FamSet[fb] {
		t.Fatalf("not one set: %v", m.FamSet)
	}
	if left := a.ixLonePending(ctx, m, 50); len(left) != 0 {
		t.Fatalf("still to review: %+v", left)
	}
}
