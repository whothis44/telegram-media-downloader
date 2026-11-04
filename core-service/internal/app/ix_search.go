package app

import (
	"context"
	"sort"
)

// Search in the browser: one box for words (captions, text in pictures,
// speech, file names) and for what pictures show (image recognition), with
// copies of one file counted once.

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
	all := make([]float64, 0, len(s.keys))
	out.top = -2
	for i := range s.keys {
		if s.norms[i] == 0 {
			sims[i] = -2
			continue
		}
		c := float32(ixDot8(qq, s.vecs[i*d:(i+1)*d])) / (qn * s.norms[i])
		sims[i] = c
		all = append(all, float64(c))
		out.top = max(out.top, c)
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
