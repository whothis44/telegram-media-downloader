package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
