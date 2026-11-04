package app

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// ixScaleVec is a fixed random unit vector for a text (768 dimensions).
func ixScaleVec(text string) []float32 {
	h := fnv.New64a()
	_, _ = io.WriteString(h, strings.TrimPrefix(text, "a photo of "))
	r := rand.New(rand.NewSource(int64(h.Sum64())))
	v := make([]float32, 768)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	ixNormalise(v)
	return v
}

// BenchmarkIxEmbScan: one pass over 120k pictures (768 dimensions), the
// cut-off and the 200 best, against the plain nearest.
func BenchmarkIxEmbScan(b *testing.B) {
	var s ixEmbStore
	s.index = map[string]int32{}
	for i := 0; i < 120000; i++ {
		q, _ := ixQuantise(ixScaleVec(fmt.Sprint(i)))
		s.putLocked(fmt.Sprint(i), q)
	}
	q := ixScaleVec("cat")
	b.Run("scan", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			s.scan(q, ixCutK, ixLooksDepth, nil, true)
		}
	})
	b.Run("nearest", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			s.nearest(q, ixLooksDepth, nil, "")
		}
	})
}

// TestIxSearchScale times one search box at the archive's size and more:
// ~184k files in ~120 chats, 120k embedded pictures (768 dimensions),
// a caption per post, 15k pictures with text in them.
func TestIxSearchScale(t *testing.T) {
	if os.Getenv("IX_SCALE") == "" {
		t.Skip("set IX_SCALE=1")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			_, _ = io.WriteString(w, "pong")
			return
		}
		_ = r.ParseMultipartForm(1 << 20)
		raw, _ := json.Marshal(ixScaleVec(r.FormValue("text")))
		_ = json.NewEncoder(w).Encode(map[string]any{"clip": string(raw)})
	}))
	defer srv.Close()
	a, ctx := ix8App(t, map[string]any{"mlUrl": srv.URL})
	t0 := time.Now()
	m := ixScaleModel(120, 184000, 0.6, 1)
	ixAssignFamilies(m)
	ixAssignSets(m, ixSetFixes{}, nil)
	r := rand.New(rand.NewSource(2))
	words := make([]string, 3000)
	for i := range words {
		words[i] = fmt.Sprintf("w%d", i)
	}
	text := func(n int) string {
		var b []string
		for i := 0; i < n; i++ {
			b = append(b, words[r.Intn(len(words))])
		}
		if r.Intn(50) == 0 {
			b = append(b, "cat")
		}
		return strings.Join(b, " ")
	}
	for i := range m.Items {
		m.Items[i].Name = fmt.Sprintf("IMG_%d.jpg", i)
		if r.Intn(400) == 0 {
			m.Items[i].Name = fmt.Sprintf("cat_%d.jpg", i)
		}
	}
	for i := range m.Posts {
		m.ByMsg[ixPostRef(&m.Posts[i])] = int32(i)
	}
	a.ixModelBox().model = m
	tx, err := a.db.Writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range m.Posts {
		p := &m.Posts[i]
		if _, err := tx.Exec(`INSERT INTO ix_messages(group_id, message_id, date, text, media_type) VALUES (?, ?, ?, ?, 'photo')`, p.Group, p.Msg, p.Date, text(5+r.Intn(15))); err != nil {
			t.Fatal(err)
		}
	}
	for c := 0; c < 15000 && c < len(m.Clusters); c++ {
		k := m.Items[m.Clusters[c*len(m.Clusters)/15000].Canon].Key
		if _, err := tx.Exec(`INSERT OR IGNORE INTO ix_ocr(key, model, text) VALUES (?, 'm', ?)`, k, text(3+r.Intn(10))); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ixEnsureMediaSearch(ctx, a.db.Writer)
	// 120k pictures; 60 of them show a cat.
	c := a.ixMLClientNow(ctx)
	s := &a.ixMLBox().store
	s.mu.Lock()
	s.model, s.loaded, s.index = c.model, true, map[string]int32{}
	cat := ixScaleVec("cat")
	for i := 0; i < 120000; i++ {
		key := fmt.Sprintf("h:%d", i)
		v := ixScaleVec(key)
		if i%2000 == 7 {
			for d := range v {
				v[d] = 0.3*cat[d] + 0.7*v[d]
			}
			ixNormalise(v)
		}
		q, _ := ixQuantise(v)
		s.putLocked(key, q)
	}
	s.mu.Unlock()
	t.Logf("set up in %v: %d files, %d posts, %d families, %d sets, %d embedded", time.Since(t0).Round(time.Millisecond), len(m.Items), len(m.Posts), len(m.Families), len(m.Sets), s.size())

	for _, view := range []string{"families", "posts", "media", "sets"} {
		for _, match := range []string{"both", "words", "looks"} {
			for _, q := range []string{"cat", "w12", "w7 w8"} {
				start := time.Now()
				res := ixGetJSON(t, a.handleIxBrowseFeed, "/api/ix/browse/feed", url.Values{"q": {q}, "view": {view}, "rank": {"relevance"}, "match": {match}, "limit": {"48"}})
				took := time.Since(start)
				if view == "families" || q == "cat" {
					t.Logf("%-8s %-5s %-6q %4d found, %v", view, match, q, int(res["total"].(float64)), took.Round(time.Millisecond))
				}
				if took > time.Second {
					t.Errorf("%s %s %q took %v", view, match, q, took)
				}
			}
		}
	}
	start := time.Now()
	lk, err := a.ixLooksFor(ctx, m, "cat", ixLooksDepth, ixMLFilter(m, "", "", ""))
	t.Logf("looks alone: %v, %d stand out (cut-off %.3f) %v", time.Since(start).Round(time.Millisecond), lk.strong, lk.cutoff, err)
	// The planted cat pictures the browser knows come first.
	planted := 0
	for i := 7; i < 120000; i += 2000 {
		if _, ok := m.ByKey[fmt.Sprintf("h:%d", i)]; ok {
			planted++
		}
	}
	for i, h := range lk.hits[:min(planted, len(lk.hits))] {
		var n int
		if _, err := fmt.Sscanf(h.Key, "h:%d", &n); err != nil || n%2000 != 7 {
			t.Fatalf("hit %d is %s (%.3f), not a cat", i, h.Key, h.Sim)
		}
	}
	if planted == 0 || lk.strong < planted {
		t.Errorf("cat pictures standing out: %d of %d", lk.strong, planted)
	}
}
