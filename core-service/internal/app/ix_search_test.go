package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
)

// ---------- fixtures ----------

// ixSceneCopy adds a post in another chat holding a copy of a file (its own
// key, the same duplicate set), as a review "same" or a better download
// leaves behind, and assigns the families again.
func ixSceneCopy(m *ixModel, group string, msg int, date int64, key, of string) {
	c := m.Items[m.ByKey[of]].Cluster
	pi := int32(len(m.Posts))
	ii := int32(len(m.Items))
	m.Items = append(m.Items, ixMItem{Group: group, Msg: msg, Key: key, Cluster: c, Post: pi, Type: "photo", Status: "ok", Path: "x/" + key + ".jpg"})
	m.Clusters[c].Items = append(m.Clusters[c].Items, ii)
	m.ByKey[key] = ii
	m.Posts = append(m.Posts, ixMPost{Idx: pi, Group: group, Msg: msg, Msgs: []int{msg}, Date: date, Origin: date, Family: -1, Items: []int32{ii}})
	m.ByMsg[ixPostRef(&m.Posts[pi])] = pi
	ixReassignFamilies(m)
}

func ixReassignFamilies(m *ixModel) {
	m.Families = nil
	for i := range m.Posts {
		m.Posts[i].Family, m.Posts[i].Role = -1, ""
	}
	ixAssignFamilies(m)
}

// ixTestStore puts the scene's embeddings (ixSceneSim of each key, or of
// another key: same[key] = the key whose picture it is) into the store,
// as image recognition would have, for the client's model.
func ixTestStore(t *testing.T, a *App, ctx context.Context, keys []string, same map[string]string) {
	t.Helper()
	c := a.ixMLClientNow(ctx)
	if c == nil {
		t.Fatal("no recognition client")
	}
	s := &a.ixMLBox().store
	s.mu.Lock()
	defer s.mu.Unlock()
	s.model, s.loaded, s.dim, s.keys, s.vecs, s.norms, s.index = c.model, true, 0, nil, nil, nil, map[string]int32{}
	for _, k := range keys {
		src := k
		if o, ok := same[k]; ok {
			src = o
		}
		q, _ := ixQuantise(ixSceneSim(src))
		s.putLocked(k, q)
	}
}

func ixSceneKeys(m *ixModel) []string {
	var keys []string
	seen := map[string]bool{}
	for i := range m.Items {
		if k := m.Items[i].Key; k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys
}

// ixGetJSON calls a handler as the browser would.
func ixGetJSON(t *testing.T, h http.HandlerFunc, path string, q url.Values) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, path+"?"+q.Encode(), nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("%s?%s: %d %v", path, q.Encode(), rec.Code, out)
	}
	return out
}

func ixCardKeys(res map[string]any) []string {
	var out []string
	cards, _ := res["cards"].([]any)
	for _, c := range cards {
		items, _ := c.(map[string]any)["items"].([]any)
		if len(items) > 0 {
			k, _ := items[0].(map[string]any)["key"].(string)
			out = append(out, k)
		}
	}
	return out
}

// ---------- bugs ----------

func TestIxStatusFilterWorksWithTheTypeFilter(t *testing.T) {
	m := ixSetScene()
	// B:1 has a picture not downloaded; F:1 a video not downloaded.
	m.Items[m.Posts[1].Items[9]].Status = "missing"
	z1 := m.ByKey["z1"]
	m.Items[z1].Type, m.Items[z1].Status = "video", "missing"
	count := func(f ixFeedQuery) []string {
		var out []string
		for pi := range m.Posts {
			if m.postMatches(int32(pi), f, nil) {
				out = append(out, m.Posts[pi].Group)
			}
		}
		return out
	}
	if got := count(ixFeedQuery{Type: "photo", Status: "missing"}); len(got) != 1 || got[0] != "B" {
		t.Fatalf("missing pictures: %v", got)
	}
	if got := count(ixFeedQuery{Type: "video", Status: "missing"}); len(got) != 1 || got[0] != "F" {
		t.Fatalf("missing videos: %v", got)
	}
	if got := count(ixFeedQuery{Type: "album", Status: "missing"}); len(got) != 2 {
		t.Fatalf("albums with a missing file: %v", got)
	}
	if got := count(ixFeedQuery{Type: "photo"}); len(got) != 6 {
		t.Fatalf("posts with pictures: %v", got)
	}
	// The media grid shows only the files in that state.
	a, ctx := ix8App(t, nil)
	_ = ctx
	a.ixModelBox().model = m
	res := ixGetJSON(t, a.handleIxBrowseFeed, "/api/ix/browse/feed", url.Values{"view": {"media"}, "type": {"photo"}, "status": {"missing"}})
	if keys := ixCardKeys(res); len(keys) != 1 || keys[0] != m.Items[m.Posts[1].Items[9]].Key {
		t.Fatalf("media grid: %v", keys)
	}
}

func TestIxCopiesOfOneFileAreFoundOnce(t *testing.T) {
	srv := ix8FakeML(t)
	defer srv.Close()
	a, ctx := ix8App(t, map[string]any{"mlUrl": srv.URL, "mlThings": "s9\nx9"})
	m := ixSetScene()
	for i := range m.Items {
		m.Items[i].Path = "x/" + m.Items[i].Key + ".jpg"
	}
	// s00 was reposted in G; both copies were embedded (the old best copy
	// before a better one came).
	ixSceneCopy(m, "G", 1, 500, "s00b", "s00")
	a.ixModelBox().model = m
	ixTestStore(t, a, ctx, ixSceneKeys(m), map[string]string{"s00b": "s00"})
	clusterOf := func(k string) int32 { return m.Items[m.ByKey[k]].Cluster }
	// More like this: the file itself (either copy) is not a result, and
	// no set comes twice.
	for _, like := range []string{"s00", "s00b", "s01"} {
		hits, err := a.ixMLSearch(ctx, m, "", like, 40, ixMLFilter(m, "", "", ""))
		if err != nil {
			t.Fatal(err)
		}
		seen := map[int32]bool{}
		for _, h := range hits {
			c := clusterOf(h.Key)
			if c == clusterOf(like) {
				t.Fatalf("like %s found its own set: %+v", like, h)
			}
			if seen[c] {
				t.Fatalf("like %s: set %d twice: %+v", like, c, hits)
			}
			seen[c] = true
		}
	}
	// Search by description: 18 pictures of the scene, each once.
	hits, err := a.ixMLSearch(ctx, m, "s99", "", 40, ixMLFilter(m, "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	ns := 0
	for _, h := range hits {
		if h.Key[0] == 's' {
			ns++
		}
	}
	if ns != 18 {
		t.Fatalf("scene pictures found: %d (%+v)", ns, hits)
	}
	// Things count the set once.
	things, err := a.ixThings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range things {
		if th.Label == "s9" && th.Count != 18 || th.Label == "x9" && th.Count != 13 {
			t.Fatalf("things: %+v", things)
		}
	}
	th, err := a.ixThingHits(ctx, "s9", 100, nil)
	if err != nil || len(th) != 18 {
		t.Fatalf("thing hits: %d %v", len(th), err)
	}
	// People: a face found in both copies is one file.
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_people(id, name) VALUES (1, 'Ann')`); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"s00", "s00b", "s01"} {
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_faces(key, model, x1, y1, x2, y2, vec, person) VALUES (?, 'f', 0, 0, 1, 1, x'00', 1)`, k); err != nil {
			t.Fatal(err)
		}
	}
	people, err := a.ixPeople(ctx)
	if err != nil || len(people) != 1 || people[0].Files != 2 {
		t.Fatalf("people: %+v %v", people, err)
	}
}

func TestIxSetRoutesNeedAnAdmin(t *testing.T) {
	a, _ := ix8App(t, nil)
	mux := http.NewServeMux()
	registerIxSetRoutes(mux, a)
	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/ix/browse/similar?q=cat", nil),
		httptest.NewRequest(http.MethodGet, "/api/ix/browse/set?id=0", nil),
		httptest.NewRequest(http.MethodPost, "/api/ix/ml/check", nil),
		httptest.NewRequest(http.MethodPost, "/api/ix/browse/set/post", nil),
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without a session: %d", r.Method, r.URL.Path, rec.Code)
		}
	}
}

// ---------- one search box ----------

// ixWordsScene gives the scene words: captions, text in pictures, speech
// and a file name, all mentioning a xylophone (the fake recognition places
// "xylophone" with the x/y pictures, as it does every word starting with x).
func ixWordsScene(t *testing.T, a *App, ctx context.Context, m *ixModel) {
	t.Helper()
	for _, row := range [][2]string{{"A", "xylophone concert"}, {"C", "xylophone"}, {"F", "nothing to see"}} {
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_messages(group_id, message_id, date, text, media_type) VALUES (?, 1, 100, ?, 'photo')`, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range [][3]string{{"ix_ocr", "z0", "XYLOPHONE LESSONS"}, {"ix_stt", "o5", "playing the xylophone"}} {
		if _, err := a.db.Writer.Exec(`INSERT INTO `+row[0]+`(key, model, text) VALUES (?, 'm', ?)`, row[1], row[2]); err != nil {
			t.Fatal(err)
		}
		a.ixIndexMediaText(ctx, row[0], row[1], row[2])
	}
	m.Items[m.ByKey["s10"]].Name = "Xylophone.jpg"
}

type ixFoundCard struct {
	group string
	why   string
	weak  bool
	kind  string
	key   string
	sim   float64
}

func ixFound(res map[string]any) []ixFoundCard {
	var out []ixFoundCard
	for _, c := range res["cards"].([]any) {
		cm := c.(map[string]any)
		fc := ixFoundCard{kind: cm["kind"].(string), weak: cm["weak"] == true}
		fc.group, _ = cm["group"].(string)
		if w, ok := cm["why"].([]any); ok {
			for i, x := range w {
				if i > 0 {
					fc.why += "+"
				}
				fc.why += x.(string)
			}
		}
		if items, ok := cm["items"].([]any); ok && len(items) > 0 {
			it := items[0].(map[string]any)
			fc.key, _ = it["key"].(string)
			fc.sim, _ = it["sim"].(float64)
		}
		out = append(out, fc)
	}
	return out
}

func TestIxOneSearchBoxFindsWordsAndLooks(t *testing.T) {
	a, ctx, m, _ := ixLooksScene(t, nil)
	// A repost of x0 in G: found once per original, in every post holding it.
	ixSceneCopy(m, "G", 1, 500, "x0b", "x0")
	ixAssignSets(m, ixSetFixes{}, nil)
	ixTestStore(t, a, ctx, ixSceneKeys(m), map[string]string{"x0b": "x0"})
	ixWordsScene(t, a, ctx, m)
	search := func(view, match string, extra ...string) map[string]any {
		q := url.Values{"q": {"xylophone"}, "view": {view}, "rank": {"relevance"}, "match": {match}}
		for i := 0; i+1 < len(extra); i += 2 {
			q.Set(extra[i], extra[i+1])
		}
		return ixGetJSON(t, a.handleIxBrowseFeed, "/api/ix/browse/feed", q)
	}
	why := func(cards []ixFoundCard) map[string]string {
		out := map[string]string{}
		for _, c := range cards {
			out[c.group] = c.why
		}
		return out
	}
	// Both: every way of matching, each post once, best first.
	res := search("posts", "both")
	cards := ixFound(res)
	got := why(cards)
	want := map[string]string{"A": "caption", "B": "name", "C": "caption+speech", "D": "looks", "E": "looks", "F": "text", "G": "looks"}
	if len(cards) != len(want) {
		t.Fatalf("posts found: %+v", cards)
	}
	for g, w := range want {
		if got[g] != w {
			t.Fatalf("why %s: %q, want %q (%v)", g, got[g], w, got)
		}
	}
	if cards[0].group != "C" {
		t.Fatalf("best first: %+v", cards)
	}
	s := res["search"].(map[string]any)
	if s["match"] != "both" || s["looks"].(float64) != 13 || s["pinned"] != false {
		t.Fatalf("search info: %v", s)
	}
	// Originals: the repost folds into its original.
	if res := search("families", "both"); res["total"].(float64) != 6 {
		t.Fatalf("originals found: %v", res["total"])
	}
	// The media grid: only the files found, with their likeness.
	res = search("media", "both")
	tiles := ixFound(res)
	byKey := map[string]ixFoundCard{}
	for _, c := range tiles {
		if _, dup := byKey[c.key]; dup {
			t.Fatalf("a file twice: %s", c.key)
		}
		byKey[c.key] = c
	}
	if c := byKey["x3"]; c.why != "looks" || c.sim <= 0 {
		t.Fatalf("x3 tile: %+v", c)
	}
	if _, ok := byKey["x0b"]; ok {
		t.Fatal("the copy of x0 has a tile of its own")
	}
	if c := byKey["o5"]; c.why != "caption+speech" {
		t.Fatalf("o5 tile: %+v", c)
	}
	if _, ok := byKey["z1"]; ok {
		t.Fatal("z1 is shown: only z0 has the words")
	}
	// Words only, looks only.
	if got := why(ixFound(search("posts", "words"))); len(got) != 4 || got["D"] != "" {
		t.Fatalf("words: %v", got)
	}
	if got := why(ixFound(search("posts", "looks"))); len(got) != 3 || got["D"] != "looks" || got["A"] != "" {
		t.Fatalf("looks: %v", got)
	}
	// Every filter still applies: one chat, kind, date.
	if got := why(ixFound(search("posts", "both", "group", "E"))); len(got) != 1 || got["E"] != "looks" {
		t.Fatalf("in E: %v", got)
	}
	if got := ixFound(search("posts", "both", "type", "video")); len(got) != 0 {
		t.Fatalf("videos: %+v", got)
	}
	if got := why(ixFound(search("posts", "both", "from", "250", "to", "450"))); len(got) != 2 || got["C"] == "" || got["F"] == "" {
		t.Fatalf("dated 250-450: %v", got)
	}
	// Sets and newest first work too.
	if res := search("sets", "both"); res["total"].(float64) != 2 {
		t.Fatalf("sets found: %v", res["total"])
	}
	res = ixGetJSON(t, a.handleIxBrowseFeed, "/api/ix/browse/feed", url.Values{"q": {"xylophone"}, "view": {"posts"}})
	if cards := ixFound(res); len(cards) != 7 || cards[0].group != "G" {
		t.Fatalf("newest first: %+v", cards)
	}
}

func TestIxSearchPutsAFarBetterCaptionFirst(t *testing.T) {
	a, ctx, m, _ := ixLooksScene(t, nil)
	_ = ctx
	for _, row := range [][2]string{
		{"F", "zebra zebra zebra"},
		{"A", "a zebra among the many horses, donkeys and cows grazing in a wide field near the river on a sunny day"},
		{"B", "the zebra seen once from the road past the farm with the old barn, the silos and the tractors"},
	} {
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_messages(group_id, message_id, date, text, media_type) VALUES (?, 1, 100, ?, 'photo')`, row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	res := ixGetJSON(t, a.handleIxBrowseFeed, "/api/ix/browse/feed", url.Values{"q": {"zebra"}, "view": {"posts"}, "rank": {"relevance"}, "match": {"words"}})
	cards := ixFound(res)
	if res["search"].(map[string]any)["pinned"] != true || len(cards) != 3 || cards[0].group != "F" {
		t.Fatalf("pinned: %v %+v", res["search"], cards)
	}
	// Two captions alike: nothing is put first.
	if _, err := a.db.Writer.Exec(`UPDATE ix_messages SET text='zebra zebra zebra!' WHERE group_id='A'`); err != nil {
		t.Fatal(err)
	}
	res = ixGetJSON(t, a.handleIxBrowseFeed, "/api/ix/browse/feed", url.Values{"q": {"zebra"}, "view": {"posts"}, "rank": {"relevance"}, "match": {"words"}})
	if res["search"].(map[string]any)["pinned"] != false {
		t.Fatalf("pinned between equals: %v", res["search"])
	}
	_ = m
}

func TestIxTextInPicturesAndSpeechIsIndexed(t *testing.T) {
	a, ctx := ix8App(t, nil)
	m := ixSetScene()
	// Rows saved before the index existed are indexed once.
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_ocr(key, model, text) VALUES ('s01', 'm', 'Old Harbour Road'), ('s02', 'm', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_stt(key, model, text) VALUES ('z0', 'm', 'meet me at the harbour')`); err != nil {
		t.Fatal(err)
	}
	ixEnsureMediaSearch(ctx, a.db.Writer)
	ixEnsureMediaSearch(ctx, a.db.Writer) // twice: nothing indexed twice
	var n int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_ocr_fts`).Scan(&n)
	if n != 1 {
		t.Fatalf("indexed pictures: %d", n)
	}
	if keys := a.ixMediaTextKeys(ctx, "ix_ocr", "harbour"); len(keys) != 1 || keys[0] != "s01" {
		t.Fatalf("picture text: %v", keys)
	}
	if keys := a.ixMediaTextKeys(ctx, "ix_stt", "harb"); len(keys) != 1 || keys[0] != "z0" {
		t.Fatalf("speech (a word begun): %v", keys)
	}
	// Read again with other words: the old ones are gone from the index.
	if _, err := a.db.Writer.Exec(`UPDATE ix_ocr SET text='New Bridge' WHERE key='s01'`); err != nil {
		t.Fatal(err)
	}
	a.ixIndexMediaText(ctx, "ix_ocr", "s01", "New Bridge")
	if keys := a.ixMediaTextKeys(ctx, "ix_ocr", "harbour"); len(keys) != 0 {
		t.Fatalf("old words still found: %v", keys)
	}
	hits := map[int32]bool{}
	a.ixSearchMediaText(ctx, m, "bridge", hits)
	if len(hits) != 2 || !hits[0] || !hits[2] { // s01 is in A:1 and C:1
		t.Fatalf("posts: %v", hits)
	}
	// Words that are not a full-text query still search.
	if keys := a.ixMediaTextKeys(ctx, "ix_ocr", `"`); keys != nil {
		t.Fatalf("a lone quote: %v", keys)
	}
}

// ---------- more like this, Things: pages ----------

func TestIxMoreLikeThisAndThingsArePaged(t *testing.T) {
	a, ctx, m, _ := ixLooksScene(t, map[string]any{"mlThings": "x9\ns9"})
	similar := func(v url.Values) map[string]any {
		return ixGetJSON(t, a.handleIxSimilar, "/api/ix/browse/similar", v)
	}
	// Twelve pictures like x0 (x1–x6, y1–y6), five at a time, best first,
	// none twice, never x0 itself.
	seen := map[string]bool{}
	last := 2.0
	cursor := "0"
	pages := 0
	for {
		res := similar(url.Values{"like": {"x0"}, "view": {"media"}, "limit": {"5"}, "cursor": {cursor}})
		if res["total"].(float64) != 12 {
			t.Fatalf("total %v", res["total"])
		}
		for _, c := range ixFound(res) {
			if c.key == "x0" || seen[c.key] || c.key[0] != 'x' && c.key[0] != 'y' || c.sim > last || c.why != "looks" || c.weak {
				t.Fatalf("page %d: %+v", pages, c)
			}
			seen[c.key], last = true, c.sim
		}
		pages++
		next := int(res["next"].(float64))
		if next == 0 {
			break
		}
		cursor = strconv.Itoa(next)
	}
	if pages != 3 || len(seen) != 12 {
		t.Fatalf("pages %d, files %d", pages, len(seen))
	}
	// Found once, kept for the next pages.
	if n := len(a.ixSimilarBox().saved); n != 1 {
		t.Fatalf("kept searches: %d", n)
	}
	// Every view and filter: originals, one chat, dates.
	if res := similar(url.Values{"like": {"x0"}, "view": {"families"}}); res["total"].(float64) != 2 {
		t.Fatalf("originals like x0: %v", res["total"])
	}
	if got := ixFound(similar(url.Values{"like": {"x0"}, "view": {"posts"}, "group": {"E"}})); len(got) != 1 || got[0].group != "E" {
		t.Fatalf("in E: %+v", got)
	}
	if got := ixFound(similar(url.Values{"like": {"x0"}, "view": {"posts"}, "from": {"55"}})); len(got) != 1 || got[0].group != "E" {
		t.Fatalf("from 55: %+v", got)
	}
	// A subject: its pictures, as many as its count.
	res := similar(url.Values{"thing": {"x9"}, "view": {"media"}, "limit": {"100"}})
	if res["total"].(float64) != 13 || len(ixFound(res)) != 13 {
		t.Fatalf("x9: %v", res["total"])
	}
	if res := similar(url.Values{"thing": {"s9"}, "view": {"posts"}}); res["total"].(float64) != 3 {
		t.Fatalf("posts showing s9: %v", res["total"])
	}
	// The store grows: searched again.
	s := &a.ixMLBox().store
	s.mu.Lock()
	q, _ := ixQuantise(ixSceneSim("x0"))
	s.putLocked("new", q)
	s.mu.Unlock()
	similar(url.Values{"like": {"x0"}, "view": {"media"}})
	if n := len(a.ixSimilarBox().saved); n != 5 { // all, in E, x9, s9, all again
		t.Fatalf("kept searches after the store grew: %d", n)
	}
	if _, err := a.ixSimilarList(ctx, m, "nope", "", "", "", ""); err == nil {
		t.Fatal("a file never looked at was searched")
	}
}

// ---------- the cut-off ----------

// ix8CountingML is ix8FakeML, counting the requests it answers.
func ix8CountingML(t *testing.T, calls *int) *httptest.Server {
	inner := ix8FakeML(t)
	t.Cleanup(inner.Close)
	h := inner.Config.Handler
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/predict" {
			mu.Lock()
			*calls++
			mu.Unlock()
		}
		h.ServeHTTP(w, r)
	}))
}

// ixLooksScene: the scene, embedded, with recognition on.
func ixLooksScene(t *testing.T, settings map[string]any) (*App, context.Context, *ixModel, *int) {
	calls := new(int)
	srv := ix8CountingML(t, calls)
	t.Cleanup(srv.Close)
	if settings == nil {
		settings = map[string]any{}
	}
	settings["mlUrl"] = srv.URL
	a, ctx := ix8App(t, settings)
	m := ixSetScene()
	for i := range m.Items {
		m.Items[i].Path = "x/" + m.Items[i].Key + ".jpg"
	}
	a.ixModelBox().model = m
	ixTestStore(t, a, ctx, ixSceneKeys(m), nil)
	return a, ctx, m, calls
}

func TestIxDescriptionSearchHasACutOff(t *testing.T) {
	a, ctx, m, calls := ixLooksScene(t, nil)
	res := ixGetJSON(t, a.handleIxSimilar, "/api/ix/browse/similar", url.Values{"q": {"x9"}, "limit": {"240"}})
	keys := ixCardKeys(res)
	if res["total"].(float64) != 13 || len(keys) != 13 || res["weak"].(float64) != 0 {
		t.Fatalf("x9: total %v weak %v cards %v", res["total"], res["weak"], keys)
	}
	for _, k := range keys {
		if k[0] != 'x' && k[0] != 'y' {
			t.Fatalf("x9 found %s", k)
		}
	}
	if c := res["cutoff"].(float64); c <= 0 || c >= 1 {
		t.Fatalf("cutoff %v", c)
	}
	// The description is embedded once (as typed and as "a photo of ..."),
	// then remembered.
	if *calls != 2 {
		t.Fatalf("embedding requests: %d", *calls)
	}
	ixGetJSON(t, a.handleIxSimilar, "/api/ix/browse/similar", url.Values{"q": {"x9"}})
	if *calls != 2 {
		t.Fatalf("embedding requests after the same search: %d", *calls)
	}
	if _, err := a.ixQueryVector(ctx, a.ixMLClientNow(ctx), "one two three four five six"); err != nil || *calls != 3 {
		t.Fatalf("a long description: %d requests, %v", *calls, err)
	}
	// Something no picture shows: the closest twelve, all marked weak.
	found := false
	for i := 0; i < 200 && !found; i++ {
		q := fmt.Sprintf("q%d", i)
		lk, err := a.ixLooksFor(ctx, m, q, 240, ixMLFilter(m, "", "", ""))
		if err != nil {
			t.Fatal(err)
		}
		if lk.strong != 0 {
			continue
		}
		found = true
		res := ixGetJSON(t, a.handleIxSimilar, "/api/ix/browse/similar", url.Values{"q": {q}})
		cards := res["cards"].([]any)
		if res["total"].(float64) != 0 || len(cards) != ixWeakFloor || res["weak"].(float64) != ixWeakFloor {
			t.Fatalf("%s: total %v weak %v cards %d", q, res["total"], res["weak"], len(cards))
		}
		for _, c := range cards {
			if c.(map[string]any)["weak"] != true {
				t.Fatalf("%s: a card not marked weak", q)
			}
		}
	}
	if !found {
		t.Fatal("no description without a match in the scene")
	}
	// Within one chat: only its pictures.
	res = ixGetJSON(t, a.handleIxSimilar, "/api/ix/browse/similar", url.Values{"q": {"x9"}, "group": {"E"}})
	if res["total"].(float64) != 7 {
		t.Fatalf("x9 in E: %v", res["total"])
	}
}
