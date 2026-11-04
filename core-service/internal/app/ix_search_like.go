package app

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// "More like this" and Things, a page at a time: the best ~2000 files are
// found once and kept (per model, file or subject, chat filters and size
// of the store), then shown through the feed like a search, so every view
// and filter works and the count is real.

const (
	ixSimilarDepth = 2000 // files kept per "more like this" or subject
	ixSimilarKept  = 16   // searches kept
	ixLikeRelative = 0.6  // more like this: at least this share of the best likeness
)

type ixSimilarHits struct {
	hits   []ixEmbHit // one per duplicate set, best first
	cutoff float32    // below: a weak match
}

type ixSimilarCache struct {
	mu    sync.Mutex
	order []string // oldest first
	saved map[string]ixSimilarHits
}

var ixSimilarState sync.Map // *App -> *ixSimilarCache

func (a *App) ixSimilarBox() *ixSimilarCache {
	v, _ := ixSimilarState.LoadOrStore(a, &ixSimilarCache{saved: map[string]ixSimilarHits{}})
	return v.(*ixSimilarCache)
}

func (c *ixSimilarCache) get(key string) (ixSimilarHits, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.saved[key]
	return h, ok
}

func (c *ixSimilarCache) put(key string, h ixSimilarHits) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.saved[key]; !ok {
		c.order = append(c.order, key)
	}
	c.saved[key] = h
	for len(c.order) > ixSimilarKept {
		delete(c.saved, c.order[0])
		c.order = c.order[1:]
	}
}

// ixSimilarList finds (or recalls) the files most like a file, or showing
// a subject, within one chat, category or kind.
func (a *App) ixSimilarList(ctx context.Context, m *ixModel, like, thing, group, category, typ string) (ixSimilarHits, error) {
	c := a.ixMLClientNow(ctx)
	if c == nil {
		return ixSimilarHits{}, errors.New("image recognition is off (Archive → Settings)")
	}
	box := a.ixMLBox()
	if err := box.store.load(ctx, a.db.Reader, c.model); err != nil {
		return ixSimilarHits{}, err
	}
	var q []float32
	var thr float32
	what := ""
	if like != "" {
		// A copy of a file is searched by the set's best copy.
		if ii, ok := m.ByKey[like]; ok {
			if canon := m.Clusters[m.Items[ii].Cluster].Canon; canon >= 0 && m.Items[canon].Key != "" && box.store.has(m.Items[canon].Key) {
				like = m.Items[canon].Key
			}
		}
		if q = box.store.vector(like); q == nil {
			return ixSimilarHits{}, errors.New("this file has not been looked at by image recognition yet")
		}
		what = "like:" + like
	} else {
		list, err := a.ixThings(ctx)
		if err != nil {
			return ixSimilarHits{}, err
		}
		for _, t := range list {
			if strings.EqualFold(t.Label, thing) {
				q, thr, what = t.vec, t.thr, "thing:"+t.Label
			}
		}
		if q == nil {
			return ixSimilarHits{}, errors.New("no such subject in the list (Archive → Settings)")
		}
	}
	key := strings.Join([]string{c.model, what, strconv.Itoa(box.store.size()), group, category, typ, m.Built.String()}, "\x00")
	sc := a.ixSimilarBox()
	if h, ok := sc.get(key); ok {
		return h, nil
	}
	keep := ixSkipCluster(m, like, ixMLFilter(m, group, category, typ))
	ask := ixSimilarDepth + ixSimilarDepth/4
	var s ixEmbScan
	var hits []ixEmbHit
	for {
		s = box.store.scan(q, ixCutK, ask, keep, false)
		hits = ixUniqueHits(m, s.hits, ixSimilarDepth)
		if len(hits) >= ixSimilarDepth || len(s.hits) < ask {
			break
		}
		ask *= 2
	}
	out := ixSimilarHits{cutoff: thr}
	if like != "" {
		// Pictures that stand out from the rest, and not far below the best.
		out.cutoff = s.thr
		if len(hits) > 0 {
			out.cutoff = max(out.cutoff, ixLikeRelative*hits[0].Sim)
		}
		strong := 0
		for strong < len(hits) && hits[strong].Sim >= out.cutoff {
			strong++
		}
		out.hits = hits[:max(strong, min(len(hits), ixWeakFloor))]
	} else {
		// A subject: only the pictures that show it (as its count).
		for len(hits) > 0 && hits[len(hits)-1].Sim < thr {
			hits = hits[:len(hits)-1]
		}
		out.hits = hits
	}
	sc.put(key, out)
	return out, nil
}

// ixRankHits makes a ranking of likeness hits: a file's score is its
// likeness, a post's that of its best file.
func ixRankHits(m *ixModel, h ixSimilarHits) *ixRank {
	r := &ixRank{post: map[int32]float64{}, cluster: map[int32]float64{}, postWhy: map[int32]uint8{}, clusterWhy: map[int32]uint8{},
		sim: map[int32]float32{}, weakPost: map[int32]bool{}, weakSet: map[int32]bool{}, byScore: true, info: map[string]any{}}
	strongPost := map[int32]bool{}
	strong := 0
	for _, hit := range h.hits {
		ii, ok := m.ByKey[hit.Key]
		if !ok {
			continue
		}
		c := m.Items[ii].Cluster
		score := float64(hit.Sim) + 2 // above zero, whatever the likeness
		weak := hit.Sim < h.cutoff
		if !weak {
			strong++
		}
		r.cluster[c], r.sim[c], r.clusterWhy[c] = score, hit.Sim, ixWhyLooks
		if weak {
			r.weakSet[c] = true
		}
		for _, x := range m.Clusters[c].Items {
			pi := m.Items[x].Post
			r.post[pi] = max(r.post[pi], score)
			r.postWhy[pi] |= ixWhyLooks
			if !weak {
				strongPost[pi] = true
			}
		}
	}
	for pi := range r.post {
		if !strongPost[pi] {
			r.weakPost[pi] = true
		}
	}
	r.info["looks"], r.info["weak"], r.info["cutoff"], r.info["found"] = strong, len(r.cluster)-strong, h.cutoff, len(r.post)
	return r
}

// ixSimilarFeed answers /similar for like= or thing= as a page of the feed.
func (a *App) ixSimilarFeed(w http.ResponseWriter, r *http.Request, m *ixModel, like, thing string) {
	f := ixParseFeed(r)
	f.Q = "" // the likeness is the search
	typ := f.Type
	if typ != "photo" && typ != "video" {
		typ = ""
	}
	h, err := a.ixSimilarList(r.Context(), m, like, thing, f.Group, f.Category, typ)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	rank := ixRankHits(m, h)
	rank.info["match"] = "looks"
	a.ixWriteFeed(r.Context(), w, m, f, rank)
}
