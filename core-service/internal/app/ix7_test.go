package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- Louvain ----------

func TestIxLouvainSplitsTwoTightGroupsJoinedByOneLink(t *testing.T) {
	adj := make([][]ixGEdge, 8)
	link := func(a, b int32, w float64) {
		adj[a] = append(adj[a], ixGEdge{b, w})
		adj[b] = append(adj[b], ixGEdge{a, w})
	}
	for _, g := range [][]int32{{0, 1, 2, 3}, {4, 5, 6, 7}} {
		for i := range g {
			for j := i + 1; j < len(g); j++ {
				link(g[i], g[j], 1)
			}
		}
	}
	link(3, 4, 0.3)
	c := ixLouvain(adj)
	for i := 1; i < 4; i++ {
		if c[i] != c[0] || c[i+4] != c[4] {
			t.Fatalf("communities: %v", c)
		}
	}
	if c[0] == c[4] {
		t.Fatalf("the two groups were joined: %v", c)
	}
	// Two nodes alone, and a pair.
	adj = make([][]ixGEdge, 4)
	adj[1] = []ixGEdge{{2, 1}}
	adj[2] = []ixGEdge{{1, 1}}
	c = ixLouvain(adj)
	if c[1] != c[2] || c[0] == c[1] || c[3] == c[1] {
		t.Fatalf("pair: %v", c)
	}
}

// ---------- the three-posters example ----------

// ixSetScene: three people each post ten files. Eighteen distinct files
// belong to the set; five of them are posted twice (shared); seven of the
// thirty are something else: one belongs to another set (two other posts
// share it), six are unrelated. Each pair of posts shares too little to be
// the same album.
func ixSetScene() *ixModel {
	m := &ixModel{GroupName: map[string]string{}, GroupCat: map[string]string{}}
	clusters := map[string]int32{}
	addPost := func(group string, msg int, date int64, keys ...string) {
		pi := int32(len(m.Posts))
		p := ixMPost{Idx: pi, Group: group, Msg: msg, Date: date, Origin: date, Family: -1}
		for _, k := range keys {
			c, ok := clusters[k]
			if !ok {
				c = int32(len(m.Clusters))
				clusters[k] = c
				m.Clusters = append(m.Clusters, ixMCluster{Canon: -1})
			}
			ii := int32(len(m.Items))
			m.Items = append(m.Items, ixMItem{Group: group, Msg: msg, Key: k, Cluster: c, Post: pi, Type: "photo", Status: "ok"})
			m.Clusters[c].Items = append(m.Clusters[c].Items, ii)
			if m.Clusters[c].Canon < 0 {
				m.Clusters[c].Canon = ii
				m.ByKey[k] = ii
			}
			p.Items = append(p.Items, ii)
		}
		m.Posts = append(m.Posts, p)
	}
	m.ByKey = map[string]int32{}
	m.ByMsg = map[string]int32{}
	s := func(i int) string { return fmt.Sprintf("s%02d", i) }
	o := func(i int) string { return fmt.Sprintf("o%d", i) }
	addPost("A", 1, 100, s(0), s(1), s(2), s(5), s(6), s(7), s(8), s(9), o(0), o(1))
	addPost("B", 1, 200, s(0), s(3), s(4), s(10), s(11), s(12), s(13), s(14), o(2), o(3))
	addPost("C", 1, 300, s(1), s(2), s(3), s(4), s(15), s(16), s(17), o(4), o(5), o(6))
	// Another set: two posts sharing o0 and x0.
	addPost("D", 1, 50, o(0), "x0", "x1", "x2", "x3", "x4", "x5", "x6")
	addPost("E", 1, 60, o(0), "x0", "y1", "y2", "y3", "y4", "y5", "y6")
	// Something alone.
	addPost("F", 1, 400, "z0", "z1")
	for i := range m.Posts {
		m.ByMsg[ixPostRef(&m.Posts[i])] = int32(i)
	}
	ixAssignFamilies(m)
	return m
}

// ixSceneSim: the set's files show one scene, the x/y files another, the
// unrelated ones each something of their own.
func ixSceneSim(key string) []float32 {
	v := make([]float32, 16)
	seed := 0
	for _, r := range key {
		seed = (seed*31 + int(r)) % 1000003
	}
	switch key[0] {
	case 's':
		v[0] = 1
	case 'x', 'y':
		v[1] = 1
	default:
		v[2+seed%14] = 1
	}
	for i := range v {
		v[i] += float32((seed*(i+7))%13) / 100
	}
	ixNormalise(v)
	return v
}

func ixCos(a, b []float32) float32 {
	var d float32
	for i := range a {
		d += a[i] * b[i]
	}
	return d
}

func ixSetRoles(m *ixModel, si int) map[string][]string {
	out := map[string][]string{}
	for _, it := range m.Sets[si].Items {
		out[it.Role] = append(out[it.Role], m.Items[it.Item].Key)
	}
	for _, v := range out {
		sort.Strings(v)
	}
	return out
}

func TestIxSetsFindTheThreePostersSet(t *testing.T) {
	m := ixSetScene()
	if len(m.Families) != 6 {
		t.Fatalf("families: %d", len(m.Families))
	}
	ixAssignSets(m, ixSetFixes{}, nil)
	if len(m.Sets) != 2 {
		t.Fatalf("sets: %+v", m.Sets)
	}
	// Oldest first: D+E (50), then A+B+C (100).
	if len(m.Sets[0].Families) != 2 || len(m.Sets[1].Families) != 3 {
		t.Fatalf("set sizes: %d %d", len(m.Sets[0].Families), len(m.Sets[1].Families))
	}
	set := m.Sets[1]
	if set.Posts != 3 || set.Chats != 3 || set.Anchor != "A:1" {
		t.Fatalf("set: %+v", set)
	}
	roles := ixSetRoles(m, 1)
	if strings.Join(roles["core"], ",") != "s00,s01,s02,s03,s04" {
		t.Fatalf("core: %v", roles["core"])
	}
	if strings.Join(roles["elsewhere"], ",") != "o0" {
		t.Fatalf("elsewhere: %v", roles["elsewhere"])
	}
	// Without pictures to compare, the six unrelated files cannot be told
	// from the thirteen that belong: all are members, unverified.
	if len(roles["member"]) != 13+6 || len(roles["loose"]) != 0 {
		t.Fatalf("members: %v loose: %v", roles["member"], roles["loose"])
	}
	if m.FamSet[m.Posts[5].Family] != -1 {
		t.Fatal("a post sharing nothing is in a set")
	}

	// With image recognition, the unrelated ones are told apart.
	sim := func(a, b string) (float32, bool) { return ixCos(ixSceneSim(a), ixSceneSim(b)), true }
	ixAssignSets(m, ixSetFixes{}, sim)
	roles = ixSetRoles(m, 1)
	if len(roles["member"]) != 13 || strings.Join(roles["loose"], ",") != "o1,o2,o3,o4,o5,o6" {
		t.Fatalf("members: %v loose: %v", roles["member"], roles["loose"])
	}
	for _, k := range roles["member"] {
		if k[0] != 's' {
			t.Fatalf("member %s", k)
		}
	}
	c := ixSetCounts(&m.Sets[1])
	if c["core"] != 5 || c["member"] != 13 || c["loose"] != 6 || c["elsewhere"] != 1 {
		t.Fatalf("counts: %v", c)
	}
}

func TestIxSetCorrectionsAreKept(t *testing.T) {
	m := ixSetScene()
	fixes := ixSetFixes{alone: map[string]bool{"C:1": true}, item: map[string]map[string]string{"A:1": {"o1": "out", "o2": "in"}}}
	ixAssignSets(m, fixes, nil)
	if len(m.Sets) != 2 || len(m.Sets[1].Families) != 2 {
		t.Fatalf("C was not taken out: %+v", m.Sets)
	}
	roles := ixSetRoles(m, 1)
	if strings.Join(roles["core"], ",") != "s00" || strings.Join(roles["loose"], ",") != "o1" {
		t.Fatalf("roles: %v", roles)
	}
	for _, it := range m.Sets[1].Items {
		if k := m.Items[it.Item].Key; (k == "o1" || k == "o2") != it.Fixed {
			t.Fatalf("%s fixed=%v", k, it.Fixed)
		}
	}
	// A tie brings a post that shares nothing into a set.
	fixes = ixSetFixes{ties: [][2]string{{"F:1", "A:1"}}}
	ixAssignSets(m, fixes, nil)
	if s := m.FamSet[m.Posts[5].Family]; s < 0 || s != m.FamSet[m.Posts[0].Family] {
		t.Fatalf("tie: %v", m.FamSet)
	}
}

func TestIxSetFixesAreStored(t *testing.T) {
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.ixSetFixPost(ctx, "-1001:5", "alone", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.ixSetFixPost(ctx, "-1001:6", "tie", "-1002:9"); err != nil {
		t.Fatal(err)
	}
	if err := a.ixSetFixItem(ctx, "-1001:5", "h:abc", "out"); err != nil {
		t.Fatal(err)
	}
	if a.ixSetFixPost(ctx, "nonsense", "alone", "") == nil || a.ixSetFixItem(ctx, "-1001:5", "h:abc", "maybe") == nil {
		t.Fatal("bad input accepted")
	}
	f := a.ixLoadSetFixes(ctx)
	if !f.alone["-1001:5"] || len(f.ties) != 1 || f.ties[0] != [2]string{"-1001:6", "-1002:9"} || f.item["-1001:5"]["h:abc"] != "out" {
		t.Fatalf("fixes: %+v", f)
	}
	if err := a.ixSetFixPost(ctx, "-1001:5", "auto", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.ixSetFixItem(ctx, "-1001:5", "h:abc", "auto"); err != nil {
		t.Fatal(err)
	}
	f = a.ixLoadSetFixes(ctx)
	if f.alone["-1001:5"] || len(f.item["-1001:5"]) != 0 {
		t.Fatalf("not undone: %+v", f)
	}
}

// ---------- image recognition ----------

func TestIxMLOnlyTalksToTheLocalNetwork(t *testing.T) {
	for _, ok := range []string{"http://127.0.0.1:3003", "http://localhost:3003/", "http://192.168.1.20:3003", "http://10.0.0.5:3003", "http://nas:3003", "http://ugreen.local:3003", "http://[::1]:3003", ""} {
		if _, err := ixMLCheckURL(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://8.8.8.8:3003", "https://example.com", "ftp://127.0.0.1", "http://134744072:3003", "http://0x08080808:3003", "http://[fe80::1%25eth0]:3003", "http://user:pw@127.0.0.1:3003", "http://127.0.0.1:3003/predict", "127.0.0.1:3003"} {
		if _, err := ixMLCheckURL(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if _, err := ixParseSettings(map[string]any{"mlUrl": "http://203.0.113.9:3003"}, ixDefaultSettings()); err == nil {
		t.Error("a public address was saved")
	}
	if _, err := ixParseSettings(map[string]any{"mlModel": "x; rm -rf"}, ixDefaultSettings()); err == nil {
		t.Error("a bad model name was saved")
	}
}

var ixFakeMLMode string // "422" or "500": every picture fails so

var ixFakeMLMu sync.Mutex

// ixSetFakeMLMode: the fake service may still be answering a request the
// round gave up on.
func ixSetFakeMLMode(m string) {
	ixFakeMLMu.Lock()
	ixFakeMLMode = m
	ixFakeMLMu.Unlock()
}

// ixFakeML answers like Immich's machine-learning service: the embedding of
// an image is ixSceneSim of the text the test put in it, of a text the same.
func ixFakeML(t *testing.T, calls *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ping":
			_, _ = io.WriteString(w, "pong")
			return
		case "/predict":
		default:
			http.NotFound(w, r)
			return
		}
		ixFakeMLMu.Lock()
		*calls++
		ixFakeMLMu.Unlock()
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var entries map[string]map[string]map[string]any
		if err := json.Unmarshal([]byte(r.FormValue("entries")), &entries); err != nil {
			http.Error(w, "entries", 422)
			return
		}
		var seed string
		if f, _, err := r.FormFile("image"); err == nil {
			b, _ := io.ReadAll(f)
			seed = string(b)
			if entries["clip"]["visual"]["modelName"] != "test-model" {
				http.Error(w, "model", 422)
				return
			}
		} else {
			seed = r.FormValue("text")
			if entries["clip"]["textual"]["modelName"] != "test-model" {
				http.Error(w, "model", 422)
				return
			}
		}
		ixFakeMLMu.Lock()
		mode := ixFakeMLMode
		ixFakeMLMu.Unlock()
		if mode != "" && r.FormValue("text") == "" {
			code := map[string]int{"422": 422, "500": 500}[mode]
			http.Error(w, `{"detail":"model failed"}`, code)
			return
		}
		if seed == "broken" {
			http.Error(w, `{"detail":"cannot identify image file"}`, 422)
			return
		}
		v := ixSceneSim(seed)
		for i := range v {
			v[i] *= 3 // not normalised, as some models answer
		}
		raw, _ := json.Marshal(v)
		out, _ := json.Marshal(map[string]any{"clip": string(raw), "imageHeight": 10, "imageWidth": 10})
		_, _ = w.Write(out)
	}))
}

func TestIxMLEmbedsSearchesAndFindsLookAlikes(t *testing.T) {
	ctx := context.Background()
	calls := 0
	srv := ixFakeML(t, &calls)
	defer srv.Close()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	s, err := ixParseSettings(map[string]any{"mlUrl": srv.URL, "mlModel": "test-model"}, a.ixLoadSettings(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ixSaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	m := ixSetScene()
	for i := range m.Items {
		m.Items[i].Path = "x/" + m.Items[i].Key + ".jpg"
	}
	m.Items[m.ByKey["z1"]].Type = "document" // not a picture: skipped
	old := ixMLPicturesHook
	defer func() { ixMLPicturesHook = old }()
	ixMLPicturesHook = func(_ *App, _ context.Context, it *ixMItem) ([][]byte, error) {
		if it.Key == "z0" {
			return [][]byte{[]byte("broken")}, nil
		}
		return [][]byte{[]byte(it.Key)}, nil
	}
	c := a.ixMLClientNow(ctx)
	if c == nil || c.ping(ctx) != nil {
		t.Fatal("no client")
	}
	if err := a.ixMLBox().store.load(ctx, a.db.Reader, c.model); err != nil {
		t.Fatal(err)
	}
	a.ixMLRound(ctx, c, m, 200)
	a.ixMLCount(ctx, m)
	st := a.ixMLStatusNow(ctx)
	wanted := len(m.Clusters) - 1 // z1 is a document
	if st.Embedded != wanted-1 || st.Pending != 1 || st.LastError != "" {
		t.Fatalf("status after one pass: %+v (clusters %d)", st, len(m.Clusters))
	}
	// The broken picture is tried three times, then left.
	for pass := 0; pass < 3; pass++ {
		a.ixMLRound(ctx, c, m, 10)
	}
	a.ixMLCount(ctx, m)
	if st := a.ixMLStatusNow(ctx); st.Failed != 1 || st.Pending != 0 {
		t.Fatalf("status: %+v", st)
	}
	// Kept across restarts: a fresh store reads the same.
	var fresh ixEmbStore
	if err := fresh.load(ctx, a.db.Reader, "test-model"); err != nil || fresh.size() != wanted-1 {
		t.Fatalf("stored: %d %v", fresh.size(), err)
	}
	if v, ok := fresh.sim("s00", "s05"); !ok || math.Abs(float64(v-ixCos(ixSceneSim("s00"), ixSceneSim("s05")))) > 0.02 {
		t.Fatalf("sim %v %v", v, ok)
	}
	// Search by description: the fake places the text "s99" with the set.
	hits, err := a.ixMLSearch(ctx, m, "s99", "", 18, ixMLFilter(m, "", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 18 {
		t.Fatalf("hits: %d", len(hits))
	}
	for _, h := range hits {
		if h.Key[0] != 's' {
			t.Fatalf("description search found %s: %+v", h.Key, hits)
		}
	}
	// More like this, within one chat.
	hits, err = a.ixMLSearch(ctx, m, "", "x0", 5, ixMLFilter(m, "E", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || (hits[0].Key[0] != 'y' && hits[0].Key[0] != 'x') {
		t.Fatalf("like x0 in E: %+v", hits)
	}
	for _, h := range hits {
		if h.Key == "x0" {
			t.Fatal("the file itself was listed")
		}
		ii := m.ByKey[h.Key]
		inE := false
		for _, other := range m.Clusters[m.Items[ii].Cluster].Items {
			inE = inE || m.Items[other].Group == "E"
		}
		if !inE {
			t.Fatalf("%s is not in E", h.Key)
		}
	}
	// The sets use the stored embeddings.
	sim := a.ixEmbSim()
	if sim == nil {
		t.Fatal("no similarity")
	}
	ixAssignSets(m, ixSetFixes{}, sim)
	roles := ixSetRoles(m, 1)
	if len(roles["loose"]) != 6 || len(roles["member"]) != 13 {
		t.Fatalf("roles with stored embeddings: %v", roles)
	}
	// The service going away is noticed, not counted against files.
	srv.Close()
	if err := c.ping(ctx); err == nil {
		t.Fatal("ping after close")
	}
	if _, err := c.embed(ctx, []byte("s00"), ""); err == nil {
		t.Fatal("embed after close")
	} else if _, ok := err.(ixMLUnreachable); !ok {
		t.Fatalf("not marked unreachable: %T %v", err, err)
	}
}

func TestIxMLParsesBothAnswerShapes(t *testing.T) {
	for _, raw := range []string{`{"clip":"[3, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0]"}`, `{"clip":[3,4,0,0,0,0,0,0,0,0,0,0,0,0,0,0]}`} {
		v, err := ixMLParse([]byte(raw))
		if err != nil || math.Abs(float64(v[0])-0.6) > 1e-6 || math.Abs(float64(v[1])-0.8) > 1e-6 {
			t.Fatalf("%s: %v %v", raw, v, err)
		}
	}
	for _, raw := range []string{`{}`, `{"clip":"[]"}`, `{"clip":"nope"}`, `{"clip":"[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]"}`} {
		if _, err := ixMLParse([]byte(raw)); err == nil {
			t.Fatalf("%s accepted", raw)
		}
	}
}

func TestIxMLNeverConnectsOutsideTheLocalNetwork(t *testing.T) {
	// The address is checked when connecting too, whatever a name resolves
	// to; redirects are not followed.
	_, err := ixMLHTTP.Get("http://203.0.113.7:3003/ping")
	if err == nil || !strings.Contains(err.Error(), "not on this computer") {
		t.Fatalf("connected to a public address: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://203.0.113.7/steal", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := &ixMLClient{base: srv.URL, model: "m", http: ixMLHTTP}
	if _, err := c.embed(context.Background(), []byte("picture"), ""); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect: %v", err)
	}
}

func TestIxMLBlamesTheServiceNotTheFiles(t *testing.T) {
	ctx := context.Background()
	calls := 0
	srv := ixFakeML(t, &calls)
	defer srv.Close()
	defer ixSetFakeMLMode("")
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	s, _ := ixParseSettings(map[string]any{"mlUrl": srv.URL, "mlModel": "test-model"}, a.ixLoadSettings(ctx))
	_ = a.ixSaveSettings(ctx, s)
	m := ixSetScene()
	for i := range m.Items {
		m.Items[i].Path = "x/" + m.Items[i].Key + ".jpg"
	}
	m.Items[m.ByKey["x0"]].Type = "video"
	old := ixMLPicturesHook
	defer func() { ixMLPicturesHook = old }()
	ixMLPicturesHook = func(_ *App, _ context.Context, it *ixMItem) ([][]byte, error) { return [][]byte{[]byte(it.Key)}, nil }
	c := a.ixMLClientNow(ctx)
	_ = a.ixMLBox().store.load(ctx, a.db.Reader, c.model)
	errRows := func() int {
		var n int
		_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_emb_err`).Scan(&n)
		return n
	}
	// The picture model fails for every picture: nothing is held against
	// the files, and the round stops.
	ixSetFakeMLMode("422")
	ixFakeMLMu.Lock()
	before := calls
	ixFakeMLMu.Unlock()
	if wait := a.ixMLRound(ctx, c, m, 500); wait < 10*time.Minute {
		t.Fatalf("wait %v", wait)
	}
	ixFakeMLMu.Lock()
	made := calls - before
	ixFakeMLMu.Unlock()
	if n := errRows(); n != 0 || made > ixMLStreak+4 {
		t.Fatalf("recorded %d failures after %d calls", n, made)
	}
	if st := a.ixMLStatusNow(ctx); !strings.Contains(st.LastError, "in a row") {
		t.Fatalf("status: %+v", st)
	}
	// A server error is the service's fault too.
	ixSetFakeMLMode("500")
	if wait := a.ixMLRound(ctx, c, m, 500); wait < 5*time.Minute || errRows() != 0 {
		t.Fatalf("500: wait %v, %d failures", wait, errRows())
	}
	if st := a.ixMLStatusNow(ctx); st.Reachable {
		t.Fatalf("still reachable: %+v", st)
	}
	// Without ffmpeg, videos wait; pictures go ahead.
	ixSetFakeMLMode("")
	a.ixMLRound(ctx, c, m, 500)
	if errRows() != 0 {
		t.Fatal("failures recorded")
	}
	if !a.ixMLBox().store.has("s00") {
		t.Fatal("pictures not embedded")
	}
}

func TestIxLouvainKeepsARingOfCliquesApart(t *testing.T) {
	// Six cliques of seven, each joined to the next by one edge: modularity
	// keeps them apart (merging two would lower it). Folding levels must
	// keep the graph's weight for this to hold.
	const k, n = 7, 6
	adj := make([][]ixGEdge, k*n)
	link := func(a, b int) {
		adj[a] = append(adj[a], ixGEdge{int32(b), 1})
		adj[b] = append(adj[b], ixGEdge{int32(a), 1})
	}
	for c := 0; c < n; c++ {
		for i := 0; i < k; i++ {
			for j := i + 1; j < k; j++ {
				link(c*k+i, c*k+j)
			}
		}
		link(c*k, ((c+1)%n)*k+1)
	}
	comm := ixLouvain(adj)
	seen := map[int32]bool{}
	for c := 0; c < n; c++ {
		for i := 1; i < k; i++ {
			if comm[c*k+i] != comm[c*k] {
				t.Fatalf("clique %d split: %v", c, comm)
			}
		}
		if seen[comm[c*k]] {
			t.Fatalf("cliques merged: %v", comm)
		}
		seen[comm[c*k]] = true
	}
}
