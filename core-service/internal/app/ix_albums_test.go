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
	// The overlap endpoint answers for every chat and for one.
	for _, q := range []string{"", "?group=" + ixAlbChat} {
		rec := httptest.NewRecorder()
		a.handleIxOverlap(rec, httptest.NewRequest("GET", "/api/ix/browse/overlap"+q, nil))
		var got map[string]any
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || (q == "" && got["cover"] == nil) || (q != "" && got["chat"] == nil) {
			t.Fatalf("overlap%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	a.handleIxOverlap(rec, httptest.NewRequest("GET", "/api/ix/browse/overlap?group=nope", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown chat: %d", rec.Code)
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

// ---------- the same album in several chats ----------

// ixShootScene: album A (10 files) in chat 1; B in chat 2 holds watermarked
// copies of A's files (other bytes, so other clusters); C in chat 3 is
// unrelated but one of its files was wrongly paired with one of A's; D in
// chat 4 is a re-encode of E in chat 5, with a single pair found. All
// invented.
func ixShootScene(r *rand.Rand) (*ixModel, map[string][]float32) {
	m := &ixModel{GroupName: map[string]string{}, GroupCat: map[string]string{}, ByKey: map[string]int32{}, ByMsg: map[string]int32{},
		PersonKeys: map[int64]map[string]bool{}, PersonHidden: map[int64]bool{}}
	vec := map[string][]float32{}
	add := func(group string, date int64, keys ...string) {
		pi := int32(len(m.Posts))
		p := ixMPost{Idx: pi, Group: group, Msg: 1, Date: date, Origin: date, Family: -1, Grouped: "x"}
		for _, k := range keys {
			cl := int32(len(m.Clusters))
			m.Clusters = append(m.Clusters, ixMCluster{Canon: int32(len(m.Items)), Kind: "photo", Posts: 1, Chats: 1})
			ii := int32(len(m.Items))
			m.Items = append(m.Items, ixMItem{Group: group, Msg: 1, Key: k, Cluster: cl, Post: pi, Type: "photo", Status: "ok"})
			m.Clusters[cl].Items = []int32{ii}
			m.ByKey[k] = ii
			p.Items = append(p.Items, ii)
		}
		m.Posts = append(m.Posts, p)
		m.ByMsg[ixPostRef(&p)] = pi
	}
	shoot := ixRandUnit(r, 64)
	var a, b, c, d, e []string
	for i := 0; i < 10; i++ {
		ka, kb, kc := fmt.Sprintf("a%d", i), fmt.Sprintf("b%d", i), fmt.Sprintf("c%d", i)
		vec[ka] = ixMix(64, 1.0, shoot, 0.6, ixRandUnit(r, 64))
		vec[kb] = ixMix(64, 1.0, vec[ka], 0.1, ixRandUnit(r, 64))
		vec[kc] = ixRandUnit(r, 64)
		a, b, c = append(a, ka), append(b, kb), append(c, kc)
	}
	other := ixRandUnit(r, 64)
	for i := 0; i < 4; i++ {
		kd, ke := fmt.Sprintf("d%d", i), fmt.Sprintf("e%d", i)
		vec[kd] = ixMix(64, 1.0, other, 0.6, ixRandUnit(r, 64))
		vec[ke] = ixMix(64, 1.0, vec[kd], 0.1, ixRandUnit(r, 64))
		d, e = append(d, kd), append(e, ke)
	}
	add("1", 100, a...)
	add("2", 200, b...)
	add("3", 300, c...)
	add("4", 400, d...)
	add("5", 500, e...)
	for i := 0; i < 8; i++ {
		m.Soft = append(m.Soft, ixSoftPair{A: a[i], B: b[i], Kind: "look"})
	}
	m.Soft = append(m.Soft, ixSoftPair{A: a[9], B: c[0], Kind: "photo"}, ixSoftPair{A: d[0], B: e[0], Kind: "photo"})
	ixAssignFamilies(m)
	return m, vec
}

func TestIxLookAlikeAlbumsShareASetAndAreOfferedAsOne(t *testing.T) {
	m, vec := ixShootScene(rand.New(rand.NewSource(4)))
	ixAssignSets(m, ixSetFixes{}, nil)
	fam := func(g string) int32 { return m.Posts[m.ByMsg[g+":1"]].Family }
	if s := m.FamSet[fam("1")]; s < 0 || s != m.FamSet[fam("2")] {
		t.Fatalf("A and B are not one set: %v", m.FamSet)
	}
	if m.FamSet[fam("3")] >= 0 {
		t.Fatalf("one wrong pair put C in a set: %v", m.FamSet)
	}
	// Without image recognition: only A+B (8 pairs) is offered.
	got := ixSameAlbums(m, nil, nil)
	if len(got) != 1 || got[0].A != fam("1") || got[0].B != fam("2") || len(got[0].Pairs) != 8 || got[0].Pending != 8 {
		t.Fatalf("offers: %+v", got)
	}
	// With it: D+E too (one pair, but all files alike), and A+B gains its
	// two unpaired files; the largest comes first.
	m.EmbSim = func(x, y string) (float32, bool) { return ixCos(vec[x], vec[y]), true }
	got = ixSameAlbums(m, nil, nil)
	if len(got) != 2 || got[0].A != fam("1") || len(got[0].Pairs) != 10 || got[1].A != fam("4") || got[1].Sim < ixAlbumSimBar {
		t.Fatalf("offers: %+v", got)
	}
	if !strings.Contains(got[1].Why, "alike overall") {
		t.Fatal(got[1].Why)
	}
	// Answered pairs are not offered again.
	ra, rb := ixPairRefs("4:1", "5:1")
	if got = ixSameAlbums(m, map[[2]string]bool{{ra, rb}: true}, nil); len(got) != 1 {
		t.Fatalf("answered pair offered: %+v", got)
	}
	// Different people in each rule a pair out; the same person in both
	// lowers the bar.
	m.PersonKeys[1] = map[string]bool{"d0": true}
	m.PersonKeys[2] = map[string]bool{"e0": true}
	if got = ixSameAlbums(m, nil, nil); len(got) != 1 {
		t.Fatalf("different people: %+v", got)
	}
	m.PersonKeys[2] = map[string]bool{"e0": true, "e1": true}
	m.PersonKeys[1]["e1"] = true
	if got = ixSameAlbums(m, nil, nil); len(got) != 2 || !got[1].Person {
		t.Fatalf("same person: %+v", got)
	}
	// Albums a person said differ are never linked by their pairs.
	ixAssignSets(m, ixSetFixes{apart: [][2]string{{"1:1", "2:1"}}}, nil)
	if s := m.FamSet[fam("1")]; s >= 0 && s == m.FamSet[fam("2")] {
		t.Fatal("A and B linked although said to differ")
	}
}

func TestIxSameAlbumsAnsweredInOneClick(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{})
	const g1, g2, g3 = "-1000000000401", "-1000000000402", "-1000000000403"
	ixAlbChats(t, a, g1, g2, g3)
	day := int64(1700000000)
	ka := ixAlbPost(t, a, g1, day, "A", 1, 2, 3, 4)
	kb := ixAlbPost(t, a, g2, day+600, "B", 1, 2, 3, 4)
	kc := ixAlbPost(t, a, g3, day+900, "C", 1, 2, 3)
	for i := 0; i < 4; i++ {
		x, y := ixPairRefs(ka[i], kb[i])
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_msug(key_a,key_b,kind,distance,found_at) VALUES(?,?,'photo',25,1)`, x, y); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		x, y := ixPairRefs(kb[i], kc[i])
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_msug(key_a,key_b,kind,distance,found_at) VALUES(?,?,'photo',27,1)`, x, y); err != nil {
			t.Fatal(err)
		}
	}
	m, err := a.ixRebuildModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.SameAlbums) != 2 || len(m.SameAlbums[0].Pairs) != 4 {
		t.Fatalf("offers: %+v", m.SameAlbums)
	}
	rec := httptest.NewRecorder()
	a.handleIxAlbumReview(rec, httptest.NewRequest("GET", "/api/ix/browse/review/albums", nil))
	var got struct {
		Albums []ixAlbumJ `json:"albums"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Albums) != 2 || got.Albums[0].Matched != 4 || got.Albums[0].AFiles != 4 {
		t.Fatalf("review: %v %s", err, rec.Body.String())
	}
	if _, err := a.ixAlbumDecide(ctx, m, g1+":1", g2+":1", "maybe"); err == nil {
		t.Fatal("bad answer accepted")
	}
	// Yes: four pairs become the same file, so B folds into A as a repost.
	n, err := a.ixAlbumDecide(ctx, m, g1+":1", g2+":1", "same")
	if err != nil || n != 4 {
		t.Fatalf("same: %d %v", n, err)
	}
	// No: B+C's pairs are marked different and the albums kept apart.
	if n, err = a.ixAlbumDecide(ctx, m, g2+":1", g3+":1", "different"); err != nil || n != 3 {
		t.Fatalf("different: %d %v", n, err)
	}
	var same, diff int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FILTER (WHERE decision='same'), COUNT(*) FILTER (WHERE decision='different') FROM ix_org_pairs`).Scan(&same, &diff)
	if same != 4 || diff != 3 {
		t.Fatalf("pair decisions: %d same, %d different", same, diff)
	}
	if m, err = a.ixRebuildModel(ctx); err != nil {
		t.Fatal(err)
	}
	pa, pb, pc := m.Posts[m.ByMsg[g1+":1"]], m.Posts[m.ByMsg[g2+":1"]], m.Posts[m.ByMsg[g3+":1"]]
	if pa.Family != pb.Family || pb.Role != "repost" {
		t.Fatalf("B is not a repost of A: %+v %+v", pa, pb)
	}
	if pc.Family == pa.Family || m.FamSet[pc.Family] >= 0 && m.FamSet[pc.Family] == m.FamSet[pa.Family] {
		t.Fatal("C was joined although said to differ")
	}
	if len(m.SameAlbums) != 0 {
		t.Fatalf("still offered: %+v", m.SameAlbums)
	}
}

// ---------- which chats repeat which ----------

// ixTestModel builds a model from posts given as chat, date and file keys;
// a key seen again is the same file.
type ixTestModel struct{ m *ixModel }

func newIxTestModel() *ixTestModel {
	return &ixTestModel{&ixModel{GroupName: map[string]string{}, GroupCat: map[string]string{}, ByKey: map[string]int32{}, ByMsg: map[string]int32{},
		PersonKeys: map[int64]map[string]bool{}, PersonHidden: map[int64]bool{}}}
}

func (x *ixTestModel) post(group string, date int64, keys ...string) int32 {
	m := x.m
	pi := int32(len(m.Posts))
	msg := len(m.Posts) + 1
	p := ixMPost{Idx: pi, Group: group, Msg: msg, Date: date, Origin: date, Family: -1}
	if len(keys) > 1 {
		p.Grouped = fmt.Sprint("g", msg)
	}
	for _, k := range keys {
		cl := int32(len(m.Clusters))
		if ii, ok := m.ByKey[k]; ok {
			cl = m.Items[ii].Cluster
		} else {
			m.Clusters = append(m.Clusters, ixMCluster{Canon: int32(len(m.Items)), Kind: "photo"})
		}
		ii := int32(len(m.Items))
		m.Items = append(m.Items, ixMItem{Group: group, Msg: msg, Key: k, Cluster: cl, Post: pi, Type: "photo", Status: "ok", W: 1080, H: 1920})
		m.Clusters[cl].Items = append(m.Clusters[cl].Items, ii)
		if _, ok := m.ByKey[k]; !ok {
			m.ByKey[k] = ii
		}
		p.Items = append(p.Items, ii)
	}
	m.Posts = append(m.Posts, p)
	m.ByMsg[ixPostRef(&p)] = pi
	return pi
}

// done counts each cluster's posts and chats, as the model build does.
func (x *ixTestModel) done() *ixModel {
	m := x.m
	for ci := range m.Clusters {
		c := &m.Clusters[ci]
		posts, chats := map[int32]bool{}, map[string]bool{}
		for _, ii := range c.Items {
			posts[m.Items[ii].Post] = true
			chats[m.Items[ii].Group] = true
		}
		c.Posts, c.Chats = len(posts), len(chats)
	}
	return m
}

func ixKeys(prefix string, from, to int) []string {
	var out []string
	for i := from; i < to; i++ {
		out = append(out, fmt.Sprintf("%s%d", prefix, i))
	}
	return out
}

func TestIxChatOverlapNamesMirrorsRepostersAndVarietyChats(t *testing.T) {
	x := newIxTestModel()
	// src posts 20 files first; mir reposts all of them; sub reposts 8 and
	// one of its own; three more sources a, b, c have 10 files each; var
	// takes 3 from each of src, a, b, c and adds one; own is mostly its own;
	// tiny has 3 files.
	for i := 0; i < 20; i += 5 {
		x.post("src", int64(100+i), ixKeys("s", i, i+5)...)
		x.post("mir", int64(1000+i), ixKeys("s", i, i+5)...)
	}
	x.post("sub", 2000, append(ixKeys("s", 0, 8), "sub0")...)
	for _, g := range []string{"a", "b", "c"} {
		x.post(g, 50, ixKeys(g, 0, 10)...)
	}
	x.post("var", 3000, append(append(append(append(ixKeys("s", 10, 13), ixKeys("a", 0, 3)...), ixKeys("b", 0, 3)...), ixKeys("c", 0, 3)...), "v0")...)
	x.post("own", 400, append(ixKeys("o", 0, 10), "a9")...)
	x.post("tiny", 500, "t0", "t1", "t2")
	m := x.done()
	o := ixComputeOverlap(m)
	class := func(g string) (string, string) {
		c, w := o.class(o.index[g])
		if w < 0 {
			return c, ""
		}
		return c, o.Groups[w]
	}
	for g, want := range map[string]string{"src": "mirror", "mir": "mirror", "var": "variety", "own": "unique", "tiny": "", "a": "unique"} {
		if c, _ := class(g); c != want {
			t.Errorf("%s: %q, want %q", g, c, want)
		}
	}
	if c, w := class("sub"); c != "reposter" || (w != "src" && w != "mir") {
		t.Errorf("sub: %s of %s", c, w)
	}
	if c, w := class("mir"); w != "src" {
		t.Errorf("mir mirrors %q (%s)", w, c)
	}
	// src posted everything mir has, first.
	p := o.pair(m, o.index["mir"], o.index["src"])
	if p.Shared != 20 || p.Share != 1 || p.TheyFirst != 1 || p.WeFirst != 0 {
		t.Fatalf("mir and src: %+v", p)
	}
	j := o.chat(m, o.index["var"])
	if j.Files != 13 || j.Unique != 1 || j.Top == nil || j.Top.Shared != 3 {
		t.Fatalf("var: %+v", j)
	}
	// What each chat adds: every distinct file once, the biggest chat first.
	if o.Cover[0].Adds != 20 || o.Cover[len(o.Cover)-1].Total != 20+1+30+1+10+3 {
		t.Fatalf("cover: %+v", o.Cover)
	}
	for _, s := range o.Cover {
		if s.Group == "mir" && s.Adds != 0 {
			t.Fatalf("a mirror adds %d", s.Adds)
		}
	}
	// "Not in other chats": posts with a file only that chat has.
	m.Overlap = o
	f := ixFeedQuery{Unique: true}
	for pi := range m.Posts {
		g := m.Posts[pi].Group
		want := g != "src" && g != "mir" // every other chat posted something of its own
		if got := m.postMatches(int32(pi), f, nil); got != want {
			t.Errorf("post in %s: unique=%v", g, got)
		}
	}
}

// ---------- generic files ----------

// Fifteen variety chats repost one popular album (whole, or six of its ten
// files) four times each, among albums of their own; a logo is added to
// one post of each of 60 unrelated albums; a small icon travels with the
// same two files 60 times.
func TestIxPopularAlbumsAreNotTakenForLogos(t *testing.T) {
	x := newIxTestModel()
	album := ixKeys("pop", 0, 10)
	x.post("orig", 1, album...)
	date := int64(10)
	for c := 0; c < 15; c++ {
		g := fmt.Sprint("variety", c)
		for k := 0; k < 4; k++ {
			date++
			if k == 3 {
				x.post(g, date, album[:6]...)
			} else {
				x.post(g, date, album...)
			}
			x.post(g, date, ixKeys(fmt.Sprintf("own%d-%d-", c, k), 0, 5)...)
		}
	}
	for i := 0; i < 60; i++ {
		x.post(fmt.Sprint("chat", i%15), date+int64(i), append(ixKeys(fmt.Sprintf("u%d-", i), 0, 4), "logo")...)
		x.post(fmt.Sprint("chat", i%15), date+int64(i), "icon", "w0", "w1")
	}
	m := x.done()
	icon := &m.Items[m.ByKey["icon"]]
	icon.W, icon.H = 96, 96
	ixMarkGeneric(m)
	cl := func(k string) *ixMCluster { return &m.Clusters[m.Items[m.ByKey[k]].Cluster] }
	if c := cl("pop0"); c.Posts != 61 || c.Chats != 16 {
		t.Fatalf("the album is not over the old counts: %+v", c)
	}
	for _, k := range album {
		if cl(k).Generic {
			t.Fatalf("%s of the popular album was taken for a logo", k)
		}
	}
	if !cl("logo").Generic || !cl("icon").Generic {
		t.Fatalf("logo %v icon %v", cl("logo").Generic, cl("icon").Generic)
	}
	if cl("w0").Generic {
		t.Fatal("a file always beside the icon is not a logo")
	}
	// So every repost of the album is one family; the logo joins nothing.
	ixAssignFamilies(m)
	f := m.Posts[0].Family
	n := 0
	for pi := range m.Posts {
		if m.Posts[pi].Family == f {
			n++
		}
	}
	if n != 61 {
		t.Fatalf("the album's family has %d posts, want 61", n)
	}
	logoFam := map[int32]bool{}
	for _, ii := range cl("logo").Items {
		logoFam[m.Posts[m.Items[ii].Post].Family] = true
	}
	if len(logoFam) != 60 {
		t.Fatalf("the logo joined posts: %d families", len(logoFam))
	}
	// The old rule would have split the reposts apart.
	for ci := range m.Clusters {
		c := &m.Clusters[ci]
		c.Generic = c.Posts > ixGenericPosts && c.Chats > ixGenericChats
	}
	if !cl("pop0").Generic {
		t.Fatal("the scene no longer shows the old rule's mistake")
	}
}

// ---------- the loose bar follows the model ----------

func TestIxLooseBarIsMeasuredForTheModelInUse(t *testing.T) {
	r := rand.New(rand.NewSource(8))
	vec := map[string][]float32{}
	var cores [][]string
	for si := 0; si < 40; si++ {
		shoot := ixRandUnit(r, 64)
		var keys []string
		for i := 0; i < 8; i++ {
			k := fmt.Sprintf("s%d-%d", si, i)
			vec[k] = ixMix(64, 1.0, shoot, 0.8, ixRandUnit(r, 64))
			keys = append(keys, k)
		}
		cores = append(cores, keys)
	}
	modelA := func(a, b string) (float32, bool) { return ixCos(vec[a], vec[b]), true }
	// Another model gives the same pictures higher numbers.
	modelB := func(a, b string) (float32, bool) { return 0.4 + 0.6*ixCos(vec[a], vec[b]), true }
	ta, tb := ixLooseThreshold(modelA, cores), ixLooseThreshold(modelB, cores)
	if ta <= ixLooseLow || ta >= ixLooseHigh || tb <= ta+0.05 {
		t.Fatalf("bars: %.3f and %.3f", ta, tb)
	}
	// A file of the shoot is above both bars, an unrelated one below both.
	member, stranger := ixMix(64, 1.0, ixMix(64, 1.0, vec["s0-0"], 1.0, vec["s0-1"]), 0.5, ixRandUnit(r, 64)), ixRandUnit(r, 64)
	best := func(v []float32, sim func(float32) float32) float32 {
		b := float32(-1)
		for _, k := range cores[0] {
			b = max(b, sim(ixCos(v, vec[k])))
		}
		return b
	}
	id, squeeze := func(x float32) float32 { return x }, func(x float32) float32 { return 0.4 + 0.6*x }
	if best(member, id) < ta || best(member, squeeze) < tb || best(stranger, id) >= ta || best(stranger, squeeze) >= tb {
		t.Fatalf("member %.3f/%.3f stranger %.3f/%.3f, bars %.3f/%.3f", best(member, id), best(member, squeeze), best(stranger, id), best(stranger, squeeze), ta, tb)
	}
	// Too little to measure: the fixed bar.
	if v := ixLooseThreshold(modelA, cores[:5]); v != ixSetLooseSim {
		t.Fatalf("few sets: %.3f", v)
	}
	if v := ixLooseThreshold(nil, cores); v != ixSetLooseSim {
		t.Fatalf("no model: %.3f", v)
	}
	// The sets use it.
	m := ixSetScene()
	m.EmbSim = modelA
	ixAssignSets(m, ixSetFixes{}, nil)
	if m.LooseSim != ixSetLooseSim {
		t.Fatalf("one set measured: %.3f", m.LooseSim)
	}
}
