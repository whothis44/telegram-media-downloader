package app

import (
	"context"
	"database/sql"
	"path"
	"sort"
	"strconv"
	"strings"
)

// One search box: words and looks, fused.
//
// A search runs every list that applies: captions and message text, text
// in pictures, speech, file names (words), and what pictures show (looks,
// past the cut-off). Each list is ranked by duplicate set first (a file and
// its copies count once; a caption and its reposts count once), then the
// lists are fused per post by reciprocal rank fusion:
// score = sum of weight / (ixRRFK + rank). A post's rank in a list is that
// of its best file. A caption far ahead of the next (bm25 at least 1.5
// times) is put first.

const (
	ixRRFK      = 20  // small: a few lists, the best of each matters
	ixPinRatio  = 1.5 // a caption this far ahead of the next is put first
	ixMatchBoth = "both"
)

// Where a search matched, as shown on its card.
const (
	ixWhyCaption uint8 = 1 << iota
	ixWhyText
	ixWhySpeech
	ixWhyName
	ixWhyLooks
)

var ixWhyNames = [...]string{"caption", "text", "speech", "name", "looks"}

func ixWhyList(w uint8) []string {
	var out []string
	for i, n := range ixWhyNames {
		if w&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return out
}

// ixList is one ranked list: ranks (1 = best) per post and per set.
type ixList struct {
	why     uint8
	weight  float64
	post    map[int32]int
	cluster map[int32]int
	n       int // sets ranked so far
}

func newIxList(why uint8, weight float64) *ixList {
	return &ixList{why: why, weight: weight, post: map[int32]int{}, cluster: map[int32]int{}}
}

// addCluster ranks the next set (once), and every post holding a copy.
func (l *ixList) addCluster(m *ixModel, c int32) {
	if _, ok := l.cluster[c]; ok {
		return
	}
	l.n++
	l.cluster[c] = l.n
	for _, ii := range m.Clusters[c].Items {
		if pi := m.Items[ii].Post; l.post[pi] == 0 || l.n < l.post[pi] {
			l.post[pi] = l.n
		}
	}
}

// addPost ranks a post at r, and its files with it.
func (l *ixList) addPost(m *ixModel, pi int32, r int) {
	if old := l.post[pi]; old != 0 && old <= r {
		return
	}
	l.post[pi] = r
	for _, ii := range m.Posts[pi].Items {
		if c := m.Items[ii].Cluster; l.cluster[c] == 0 || r < l.cluster[c] {
			l.cluster[c] = r
		}
	}
}

// ixRank is what a search found: a score per post and per set (higher is
// better; posts not found have none), and where each matched.
type ixRank struct {
	post       map[int32]float64
	cluster    map[int32]float64
	postWhy    map[int32]uint8
	clusterWhy map[int32]uint8
	sim        map[int32]float32 // set -> likeness to the description or the file
	weakPost   map[int32]bool    // found only by a weak likeness
	weakSet    map[int32]bool
	byScore    bool // best first (otherwise by date, as browsing)
	info       map[string]any
}

func (r *ixRank) ordered() bool { return r != nil && r.byScore }

func (r *ixRank) hits() map[int32]bool {
	out := make(map[int32]bool, len(r.post))
	for pi := range r.post {
		out[pi] = true
	}
	return out
}

// ixMatchMode: words, looks or both (both when recognition is on).
func ixMatchMode(want string, recognition bool) string {
	switch {
	case !recognition:
		return "words"
	case want == "words" || want == "looks":
		return want
	}
	return ixMatchBoth
}

// ixHybridSearch searches for f.Q (see the top of this file).
func (a *App) ixHybridSearch(ctx context.Context, m *ixModel, f ixFeedQuery) *ixRank {
	mode := ixMatchMode(f.Match, a.ixMLClientNow(ctx) != nil)
	info := map[string]any{"match": mode}
	var lists []*ixList
	var pinned []int32
	if mode != "looks" {
		capt, pin := a.ixCaptionList(ctx, m, f.Q)
		pinned = pin
		lists = append(lists, capt)
		for _, src := range []struct {
			table  string
			why    uint8
			weight float64
		}{{"ix_ocr", ixWhyText, 0.8}, {"ix_stt", ixWhySpeech, 0.6}} {
			l := newIxList(src.why, src.weight)
			for _, k := range a.ixMediaTextKeys(ctx, src.table, f.Q) {
				if ii, ok := m.ByKey[k]; ok {
					l.addCluster(m, m.Items[ii].Cluster)
				}
			}
			lists = append(lists, l)
		}
		lists = append(lists, ixNameList(m, f.Q))
	}
	var looks *ixList
	strong := 0
	var weak []int32
	sims := map[int32]float32{}
	if mode != "words" {
		typ := f.Type
		if typ != "photo" && typ != "video" {
			typ = ""
		}
		lk, err := a.ixLooksFor(ctx, m, f.Q, ixLooksDepth, ixMLFilter(m, f.Group, f.Category, typ))
		if err != nil {
			info["looksError"] = err.Error()
		} else {
			looks = newIxList(ixWhyLooks, 1)
			for _, h := range lk.hits {
				ii, ok := m.ByKey[h.Key]
				if !ok {
					continue
				}
				c := m.Items[ii].Cluster
				sims[c] = h.Sim
				if h.Sim >= lk.cutoff {
					looks.addCluster(m, c)
				} else {
					weak = append(weak, c)
				}
			}
			strong = looks.n
			info["looks"], info["cutoff"] = lk.strong, lk.cutoff
			lists = append(lists, looks)
		}
	}
	// Too few found: the closest pictures too, after the rest, as weak
	// matches.
	found := map[int32]bool{}
	for _, l := range lists {
		for pi := range l.post {
			found[pi] = true
		}
	}
	if looks != nil && len(found) < ixWeakFloor {
		for _, c := range weak {
			looks.addCluster(m, c)
		}
	}
	if looks != nil {
		info["weak"] = looks.n - strong
	}
	r := &ixRank{post: map[int32]float64{}, cluster: map[int32]float64{}, postWhy: map[int32]uint8{}, clusterWhy: map[int32]uint8{},
		sim: sims, weakPost: map[int32]bool{}, weakSet: map[int32]bool{}, byScore: f.Rank, info: info}
	strongPost := map[int32]bool{}
	for _, l := range lists {
		for pi, rk := range l.post {
			r.post[pi] += l.weight / float64(ixRRFK+rk)
			r.postWhy[pi] |= l.why
			if l != looks || rk <= strong {
				strongPost[pi] = true
			}
		}
		for c, rk := range l.cluster {
			r.cluster[c] += l.weight / float64(ixRRFK+rk)
			r.clusterWhy[c] |= l.why
			if l == looks && rk > strong {
				r.weakSet[c] = true
			}
		}
	}
	for pi := range r.post {
		if !strongPost[pi] {
			r.weakPost[pi] = true
		}
	}
	for c := range r.weakSet {
		if r.clusterWhy[c] != ixWhyLooks {
			delete(r.weakSet, c) // found by its words too
		}
	}
	for _, pi := range pinned {
		r.post[pi]++
		for _, ii := range m.Posts[pi].Items {
			r.cluster[m.Items[ii].Cluster]++
		}
	}
	info["pinned"] = len(pinned) > 0
	info["found"] = len(r.post)
	return r
}

// ixCaptionList ranks posts by their captions and message text (bm25),
// reposts of one post at one rank. It also says which posts to put first:
// those of the best caption when it is far ahead of the next.
func (a *App) ixCaptionList(ctx context.Context, m *ixModel, q string) (*ixList, []int32) {
	var l *ixList
	var unit map[int64]int
	var scores []float64 // bm25 of each unit's best message (lower is better)
	var first []int32    // the posts of the first unit
	collect := func(rows *sql.Rows) error {
		defer rows.Close()
		l, unit, scores, first = newIxList(ixWhyCaption, 1), map[int64]int{}, nil, nil
		for rows.Next() {
			var g string
			var msg int
			var score float64
			if rows.Scan(&g, &msg, &score) != nil {
				continue
			}
			pi, ok := m.ByMsg[g+":"+strconv.Itoa(msg)]
			if !ok {
				continue
			}
			u := int64(-1) - int64(pi)
			if fam := m.Posts[pi].Family; fam >= 0 {
				u = int64(fam)
			}
			rk, ok := unit[u]
			if !ok {
				rk = len(unit) + 1
				unit[u] = rk
				scores = append(scores, score)
			}
			if rk == 1 {
				first = append(first, pi)
			}
			l.addPost(m, pi, rk)
		}
		return rows.Err()
	}
	if fq := ixFTSQuery(q); fq != "" {
		rows, err := a.db.Reader.QueryContext(ctx, `SELECT m.group_id, m.message_id, bm25(ix_fts) FROM ix_fts JOIN ix_messages m ON m.rowid=ix_fts.rowid
			WHERE ix_fts MATCH ? ORDER BY bm25(ix_fts) LIMIT 20000`, fq)
		if err == nil && collect(rows) == nil {
			if len(scores) >= 2 && scores[1] < 0 && scores[0] <= ixPinRatio*scores[1] {
				return l, first
			}
			return l, nil
		}
	}
	// No full-text index: a plain scan, newest first.
	l = newIxList(ixWhyCaption, 1)
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, message_id, 0 FROM ix_messages WHERE text LIKE ? ESCAPE '\' ORDER BY date DESC LIMIT 20000`, like); err == nil {
		_ = collect(rows)
	}
	return l, nil
}

// ixNameList ranks files by their names: the whole name first, then names
// starting with the words, then names holding them.
func ixNameList(m *ixModel, q string) *ixList {
	l := newIxList(ixWhyName, 0.3)
	needle := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(q, `"`, "")))
	if needle == "" {
		return l
	}
	best := map[int32]int{}
	for i := range m.Items {
		name := m.Items[i].Name
		if len(name) < len(needle) {
			continue
		}
		low := strings.ToLower(name)
		if !strings.Contains(low, needle) {
			continue
		}
		grade := 2
		switch {
		case low == needle || strings.TrimSuffix(low, path.Ext(low)) == needle:
			grade = 0
		case strings.HasPrefix(low, needle):
			grade = 1
		}
		c := m.Items[i].Cluster
		if g, ok := best[c]; !ok || grade < g {
			best[c] = grade
		}
	}
	order := make([]int32, 0, len(best))
	for c := range best {
		order = append(order, c)
	}
	sort.Slice(order, func(i, j int) bool {
		if best[order[i]] != best[order[j]] {
			return best[order[i]] < best[order[j]]
		}
		return order[i] < order[j]
	})
	for _, c := range order {
		l.addCluster(m, c)
	}
	return l
}

// decorate marks a card with where it matched, whether only weakly, and
// each file's likeness. posts are the card's posts; sets its duplicate
// sets (for a single file).
func (r *ixRank) decorate(m *ixModel, c *ixJCard, posts []int32, sets []int32) {
	if r == nil {
		return
	}
	var why uint8
	weak, found := true, false
	for _, pi := range posts {
		if _, ok := r.post[pi]; ok {
			why |= r.postWhy[pi]
			weak = weak && r.weakPost[pi]
			found = true
		}
	}
	for _, s := range sets {
		if _, ok := r.cluster[s]; ok {
			why |= r.clusterWhy[s]
			weak = weak && r.weakSet[s]
			found = true
		}
	}
	c.Why = ixWhyList(why)
	c.Weak = found && weak
	for i := range c.Items {
		if k := c.Items[i].Key; k != "" {
			if ii, ok := m.ByKey[k]; ok {
				if s, ok := r.sim[m.Items[ii].Cluster]; ok {
					c.Items[i].Sim = s
				}
			}
		}
	}
}
