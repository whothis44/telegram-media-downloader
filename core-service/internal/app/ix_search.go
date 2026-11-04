package app

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Search in the browser: one box for words (captions, text in pictures,
// speech, file names) and for what pictures show (image recognition), with
// copies of one file counted once.

// ---------- words in pictures and speech, indexed ----------

// Text read from pictures and speech written out get full-text indexes
// (ranked, no full scans). A row's rowid is that of its ix_ocr / ix_stt
// row; they are filled where those are saved (ix_text.go), and from the
// rows already there the first time.
const ixSchemaSearch = `
CREATE VIRTUAL TABLE IF NOT EXISTS ix_ocr_fts USING fts5(text, tokenize='unicode61 remove_diacritics 2');
CREATE VIRTUAL TABLE IF NOT EXISTS ix_stt_fts USING fts5(text, tokenize='unicode61 remove_diacritics 2');
`

// ixEnsureMediaSearch adds the indexes when SQLite has FTS5 (without it,
// search falls back to a plain text scan) and indexes rows not in them yet.
func ixEnsureMediaSearch(ctx context.Context, db *sql.DB) {
	if _, err := db.ExecContext(ctx, ixSchemaSearch); err != nil {
		return
	}
	for _, t := range []string{"ix_ocr", "ix_stt"} {
		_, _ = db.ExecContext(ctx, `INSERT INTO `+t+`_fts(rowid, text) SELECT rowid, text FROM `+t+` WHERE text<>'' AND rowid NOT IN (SELECT rowid FROM `+t+`_fts)`)
	}
}

// ixIndexMediaText puts a file's text (table ix_ocr or ix_stt, just saved)
// in its full-text index, in place of what was there.
func (a *App) ixIndexMediaText(ctx context.Context, table, key, text string) {
	if table != "ix_ocr" && table != "ix_stt" {
		return
	}
	var rowid int64
	if a.db.Writer.QueryRowContext(ctx, `SELECT rowid FROM `+table+` WHERE key=?`, key).Scan(&rowid) != nil {
		return
	}
	if _, err := a.db.Writer.ExecContext(ctx, `DELETE FROM `+table+`_fts WHERE rowid=?`, rowid); err != nil {
		return // no index (SQLite without FTS5)
	}
	if strings.TrimSpace(text) != "" {
		_, _ = a.db.Writer.ExecContext(ctx, `INSERT INTO `+table+`_fts(rowid, text) VALUES(?,?)`, rowid, text)
	}
}

// ixMediaTextKeys lists the files whose pictures show (ix_ocr) or whose
// recordings say (ix_stt) the words searched for, best first.
func (a *App) ixMediaTextKeys(ctx context.Context, table, q string) []string {
	var keys []string
	collect := func(rows *sql.Rows) error {
		defer rows.Close()
		keys = nil
		for rows.Next() {
			var k string
			if rows.Scan(&k) == nil {
				keys = append(keys, k)
			}
		}
		return rows.Err()
	}
	if fq := ixFTSQuery(q); fq != "" {
		rows, err := a.db.Reader.QueryContext(ctx, `SELECT t.key FROM `+table+`_fts f JOIN `+table+` t ON t.rowid=f.rowid WHERE `+table+`_fts MATCH ? ORDER BY bm25(`+table+`_fts) LIMIT 20000`, fq)
		if err == nil && collect(rows) == nil {
			return keys
		}
	}
	// No index: a plain scan, unranked.
	keys = nil
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT key FROM `+table+` WHERE text LIKE ? ESCAPE '\' LIMIT 20000`, like); err == nil {
		_ = collect(rows)
	}
	return keys
}

// ---------- one pass over the embeddings ----------

// ixEmbScan is what one pass over the store finds for a query.
type ixEmbScan struct {
	thr   float32    // likeness at which a file stands out (outliers' rule)
	top   float32    // the best likeness of all
	hits  []ixEmbHit // the best files kept, best first
	above []string   // every file at or above thr (only when asked for)
}

// scan compares q with every stored file once: the cut-off (outliers with k
// deviations), up to n best files that keep accepts, and, when wantAbove,
// every file past the cut-off.
func (s *ixEmbStore) scan(q []float32, k float64, n int, keep func(string) bool, wantAbove bool) ixEmbScan {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := ixEmbScan{thr: 2}
	if s.dim == 0 || len(q) != s.dim {
		return out
	}
	qq, qn := ixQuantise(q)
	if qn == 0 {
		return out
	}
	d := s.dim
	sims := make([]float32, len(s.keys))
	// A few cores share the comparisons (each a part of the store).
	workers := max(1, min(runtime.GOMAXPROCS(0), 4, len(s.keys)/8192))
	part := (len(s.keys) + workers - 1) / workers
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo, hi := w*part, min(len(s.keys), (w+1)*part)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := lo; i < hi; i++ {
				if s.norms[i] == 0 {
					sims[i] = -2
					continue
				}
				sims[i] = float32(ixDot8(qq, s.vecs[i*d:(i+1)*d])) / (qn * s.norms[i])
			}
		}()
	}
	wg.Wait()
	all := make([]float64, 0, len(s.keys))
	out.top = -2
	for _, c := range sims {
		if c > -2 {
			all = append(all, float64(c))
			out.top = max(out.top, c)
		}
	}
	if len(all) >= 20 {
		out.thr = float32(ixOutlierThr(all, k))
	}
	if wantAbove {
		for i, c := range sims {
			if c >= out.thr {
				out.above = append(out.above, s.keys[i])
			}
		}
	}
	if n <= 0 {
		return out
	}
	// The n best: keep is asked only of files that could still make it.
	worst := float32(-2)
	for i, c := range sims {
		if c <= -2 || len(out.hits) >= n && c <= worst {
			continue
		}
		if keep != nil && !keep(s.keys[i]) {
			continue
		}
		out.hits = append(out.hits, ixEmbHit{s.keys[i], c})
		if len(out.hits) > 2*n {
			sort.Slice(out.hits, func(x, y int) bool { return out.hits[x].Sim > out.hits[y].Sim })
			out.hits = out.hits[:n]
			worst = out.hits[n-1].Sim
		}
	}
	sort.Slice(out.hits, func(x, y int) bool { return out.hits[x].Sim > out.hits[y].Sim })
	if len(out.hits) > n {
		out.hits = out.hits[:n]
	}
	return out
}

// ---------- what pictures show, for a description ----------

const (
	ixCutK       = 3   // a picture matches a description this many deviations above the rest (Things use 4)
	ixWeakFloor  = 12  // shown even when fewer stand out, as weak matches
	ixLooksDepth = 200 // pictures taken into the ranking
)

// ixQueryWait: how long a search waits for a description's embedding.
var ixQueryWait = 15 * time.Second

// ixQueryVecs caches the embeddings of typed descriptions (model|text).
type ixQueryVecs struct {
	mu   sync.Mutex
	vecs map[string][]float32
}

var ixQueryState sync.Map // *App -> *ixQueryVecs

func (a *App) ixQueryBox() *ixQueryVecs {
	v, _ := ixQueryState.LoadOrStore(a, &ixQueryVecs{vecs: map[string][]float32{}})
	return v.(*ixQueryVecs)
}

// ixQueryVector embeds a description. A short one (five words or fewer)
// is also embedded as "a photo of ...", and the two are averaged: short
// queries match better that way.
func (a *App) ixQueryVector(ctx context.Context, c *ixMLClient, text string) ([]float32, error) {
	text = strings.Join(strings.Fields(text), " ")
	key := c.model + "|" + text
	box := a.ixQueryBox()
	box.mu.Lock()
	v := box.vecs[key]
	box.mu.Unlock()
	if v != nil {
		return v, nil
	}
	v, err := c.embed(ctx, nil, text)
	if err != nil {
		return nil, err
	}
	if len(strings.Fields(text)) <= 5 {
		w, err := c.embed(ctx, nil, "a photo of "+text)
		if err != nil {
			return nil, err
		}
		if len(w) == len(v) {
			sum := make([]float32, len(v))
			for i := range v {
				sum[i] = v[i] + w[i]
			}
			if ixNormalise(sum) {
				v = sum
			}
		}
	}
	box.mu.Lock()
	if len(box.vecs) >= 500 {
		box.vecs = map[string][]float32{} // a small cache: start again
	}
	box.vecs[key] = v
	box.mu.Unlock()
	return v, nil
}

// ixLooks: the pictures closest to a description, best first. The first
// strong of them stand out from the archive (see outliers); the rest, when
// fewer than ixWeakFloor stand out, are the closest anyway, as weak matches.
type ixLooks struct {
	hits   []ixEmbHit
	strong int
	cutoff float32
}

func (a *App) ixLooksFor(ctx context.Context, m *ixModel, text string, n int, keep func(string) bool) (ixLooks, error) {
	c := a.ixMLClientNow(ctx)
	if c == nil {
		return ixLooks{}, errors.New("image recognition is off (Archive → Settings)")
	}
	box := a.ixMLBox()
	if err := box.store.load(ctx, a.db.Reader, c.model); err != nil {
		return ixLooks{}, err
	}
	if box.store.size() == 0 {
		return ixLooks{cutoff: 2}, nil // nothing looked at yet: no need to ask
	}
	// A search does not wait long for the service (unreachable, or loading
	// its model): the words are shown without it.
	qctx, cancel := context.WithTimeout(ctx, ixQueryWait)
	defer cancel()
	q, err := a.ixQueryVector(qctx, c, text)
	if err != nil {
		if ctx.Err() == nil && qctx.Err() != nil {
			err = errors.New("the image recognition service did not answer in time (it may be starting); try again in a moment")
		}
		return ixLooks{}, err
	}
	want := max(n, ixWeakFloor)
	ask := want + want/4 + 8
	var sc ixEmbScan
	var hits []ixEmbHit
	for {
		sc = box.store.scan(q, ixCutK, ask, keep, true)
		hits = ixUniqueHits(m, sc.hits, want)
		if len(hits) >= want || len(sc.hits) < ask {
			break
		}
		ask *= 2
	}
	out := ixLooks{cutoff: sc.thr}
	shown := 0
	for _, h := range hits {
		if h.Sim < sc.thr {
			break
		}
		shown++
	}
	// All that stand out, not only those shown.
	var above []string
	for _, k := range sc.above {
		if keep == nil || keep(k) {
			above = append(above, k)
		}
	}
	out.strong = max(shown, ixDistinctFiles(m, above))
	out.hits = hits[:max(min(shown, n), min(len(hits), ixWeakFloor))]
	return out, nil
}

// ---------- one hit per duplicate set ----------

// Embeddings are never removed when a duplicate set's best copy changes
// (sets merged after a review, a keeper chosen, a better copy downloaded),
// so one set can have several. Only the best-ranked of them counts.

// ixUniqueHits keeps the first hit of each duplicate set, at most k.
func ixUniqueHits(m *ixModel, hits []ixEmbHit, k int) []ixEmbHit {
	seen := map[int32]bool{}
	out := make([]ixEmbHit, 0, min(k, len(hits)))
	for _, h := range hits {
		if ii, ok := m.ByKey[h.Key]; ok {
			c := m.Items[ii].Cluster
			if seen[c] {
				continue
			}
			seen[c] = true
		}
		out = append(out, h)
		if len(out) >= k {
			break
		}
	}
	return out
}

// ixNearestUnique is nearest with one hit per duplicate set: it asks for a
// few more than k, and more again when copies took their places.
func ixNearestUnique(m *ixModel, s *ixEmbStore, q []float32, k int, keep func(string) bool) []ixEmbHit {
	n := k + k/4 + 8
	for {
		raw := s.nearest(q, n, keep, "")
		out := ixUniqueHits(m, raw, k)
		if len(out) >= k || len(raw) < n {
			return out
		}
		n *= 2
	}
}

// ixSkipCluster leaves out every copy of a file (for "more like this": the
// file itself is not a result, whichever copy was embedded).
func ixSkipCluster(m *ixModel, key string, keep func(string) bool) func(string) bool {
	if key == "" {
		return keep
	}
	ii, ok := m.ByKey[key]
	if !ok {
		return func(k string) bool { return k != key && (keep == nil || keep(k)) }
	}
	c := m.Items[ii].Cluster
	return func(k string) bool {
		if k == key {
			return false
		}
		if jj, ok := m.ByKey[k]; ok && m.Items[jj].Cluster == c {
			return false
		}
		return keep == nil || keep(k)
	}
}

// ixDistinctFiles counts the duplicate sets among keys (a key the browser
// does not know counts as a file of its own).
func ixDistinctFiles(m *ixModel, keys []string) int {
	if m == nil {
		return len(keys)
	}
	seen := map[int32]bool{}
	n := 0
	for _, k := range keys {
		if ii, ok := m.ByKey[k]; ok {
			if c := m.Items[ii].Cluster; !seen[c] {
				seen[c] = true
				n++
			}
			continue
		}
		n++
	}
	return n
}

// ixPeopleFiles recounts each person's files as duplicate sets: faces are
// kept for earlier best copies too, so one picture can be counted twice.
func (a *App) ixPeopleFiles(ctx context.Context, m *ixModel, list []ixPerson) {
	if m == nil || len(list) == 0 {
		return
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT DISTINCT person, key FROM ix_faces WHERE person>0`)
	if err != nil {
		return
	}
	keys := map[int64][]string{}
	for rows.Next() {
		var id int64
		var k string
		if rows.Scan(&id, &k) == nil {
			keys[id] = append(keys[id], k)
		}
	}
	rows.Close()
	for i := range list {
		if ks, ok := keys[list[i].ID]; ok {
			list[i].Files = ixDistinctFiles(m, ks)
		}
	}
}
