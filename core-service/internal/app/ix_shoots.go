package app

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
)

// The same album posted in several chats, its files re-encoded, cropped or
// watermarked so they are not known as copies.
//
// Look-alike pairs still waiting for review (pictures close by their
// picture fingerprint or by image recognition, videos sharing footage) and
// videos found to be clips of one another link families in the sets at
// half the weight of a shared file, so such albums fall into one set
// before anyone reviews them (two pairs are needed, or one for a one-file
// post).
//
// Two albums that share 3 or more look-alike pairs (or all their files, for
// smaller ones), or that share at least one and whose files look alike as a
// whole (each file's closest match in the other album, averaged both ways,
// at 0.9 or more) become one review item: "Are these two albums the same?"
// A person seen in both counts for it; people seen in each but none in both
// rule it out (self-filmed content in similar rooms looks alike across
// creators). Yes marks every matching pair as the same file and keeps the
// two albums together; no marks the pairs as different and keeps the
// albums apart. Larger albums come first, since one answer settles more.

const (
	ixSoftShare    = 0.5  // a look-alike pair links families at this share of a shared file's weight
	ixSoftMaxHold  = 5    // a file in more families than this is not used for album pairs
	ixAlbumSimBar  = 0.9  // albums whose files match this well (both ways) are offered
	ixAlbumPairBar = 3    // or that share this many look-alike pairs
	ixAlbumSample  = 12   // files of each album compared
	ixAlbumSameFor = 0.03 // a person in both lowers the likeness needed by this much
)

// ixSoftPair: two files that may be the same, not yet decided.
type ixSoftPair struct {
	A, B string
	Kind string // photo, look, video (waiting for review), clip (a video found inside another)
}

// ixLoadSoftPairs reads the look-alike pairs nobody has decided, and the
// clip pairs.
func (a *App) ixLoadSoftPairs(ctx context.Context) []ixSoftPair {
	var out []ixSoftPair
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT s.key_a, s.key_b, s.kind FROM ix_msug s WHERE NOT EXISTS (SELECT 1 FROM ix_org_pairs o WHERE o.key_a=s.key_a AND o.key_b=s.key_b)
		UNION SELECT v.key_a, v.key_b, 'clip' FROM ix_vdup v WHERE v.kind='clip' AND NOT EXISTS (SELECT 1 FROM ix_org_pairs o WHERE o.key_a=v.key_a AND o.key_b=v.key_b)`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var p ixSoftPair
		if rows.Scan(&p.A, &p.B, &p.Kind) == nil {
			out = append(out, p)
		}
	}
	return out
}

// ixSoftClusters: the two clusters of a soft pair, when both are in the
// model, differ and are not generic.
func ixSoftClusters(m *ixModel, p ixSoftPair) (int32, int32, bool) {
	ia, okA := m.ByKey[p.A]
	ib, okB := m.ByKey[p.B]
	if !okA || !okB {
		return 0, 0, false
	}
	ca, cb := m.Items[ia].Cluster, m.Items[ib].Cluster
	if ca == cb || m.Clusters[ca].Generic || m.Clusters[cb].Generic {
		return 0, 0, false
	}
	return ca, cb, true
}

func ixFamPair(a, b int32) [2]int32 {
	if b < a {
		a, b = b, a
	}
	return [2]int32{a, b}
}

// ixAddSoftLinks adds the soft pairs' links to the sets' graph (see the top
// of the file).
func ixAddSoftLinks(m *ixModel, holders map[int32][]int32, alone []bool, apart map[[2]int32]bool, weights map[[2]int32]float64) {
	type link struct {
		w float64
		n int
	}
	soft := map[[2]int32]*link{}
	for _, p := range m.Soft {
		ca, cb, ok := ixSoftClusters(m, p)
		if !ok {
			continue
		}
		ha, hb := holders[ca], holders[cb]
		k := len(ha) + len(hb)
		if len(ha) == 0 || len(hb) == 0 || k > ixSetMaxShare {
			continue
		}
		w := ixSoftShare / math.Log2(1+float64(k))
		for _, fa := range ha {
			for _, fb := range hb {
				if fa == fb || alone[fa] || alone[fb] {
					continue
				}
				key := ixFamPair(fa, fb)
				if apart[key] {
					continue
				}
				l := soft[key]
				if l == nil {
					l = &link{}
					soft[key] = l
				}
				l.w += w
				l.n++
			}
		}
	}
	for key, l := range soft {
		need := min(2, len(m.Families[key[0]].Items), len(m.Families[key[1]].Items))
		if l.n >= need {
			weights[key] += l.w
		}
	}
}

// ---------- album pairs for review ----------

type ixAlbumSug struct {
	A, B    int32       // families
	Pairs   [][2]string // file pairs to mark as the same
	Pending int         // look-alike pairs waiting between them
	Sim     float32     // each file's closest match in the other, averaged both ways (0: not compared)
	Person  bool        // a person is seen in both
	Why     string
}

// ixSameAlbums lists the album pairs to offer for review (see the top of the
// file). decided holds the pairs of first posts a person already answered.
// only, when set, limits it to that one pair of families, offered or not.
func ixSameAlbums(m *ixModel, decided map[[2]string]bool, only *[2]int32) []ixAlbumSug {
	holders := map[int32][]int32{}
	for fi := range m.Families {
		seen := map[int32]bool{}
		for _, pi := range m.Families[fi].Posts {
			for _, ii := range m.Posts[pi].Items {
				if c := m.Items[ii].Cluster; !seen[c] {
					seen[c] = true
					holders[c] = append(holders[c], int32(fi))
				}
			}
		}
	}
	type cand struct {
		pairs [][2]string
		seen  map[[2]string]bool
	}
	cands := map[[2]int32]*cand{}
	for _, p := range m.Soft {
		ca, cb, ok := ixSoftClusters(m, p)
		if !ok || len(holders[ca]) > ixSoftMaxHold || len(holders[cb]) > ixSoftMaxHold {
			continue
		}
		for _, fa := range holders[ca] {
			for _, fb := range holders[cb] {
				key := ixFamPair(fa, fb)
				if fa == fb || only != nil && key != *only {
					continue
				}
				c := cands[key]
				if c == nil {
					c = &cand{seen: map[[2]string]bool{}}
					cands[key] = c
				}
				pk := [2]string{p.A, p.B}
				if !c.seen[pk] {
					c.seen[pk] = true
					c.pairs = append(c.pairs, pk)
				}
			}
		}
	}
	people := map[string][]int64{}
	for id, keys := range m.PersonKeys {
		if m.PersonHidden[id] {
			continue
		}
		for k := range keys {
			people[k] = append(people[k], id)
		}
	}
	keysOf := func(fi int32) []string {
		var out []string
		for _, ii := range m.Families[fi].Items {
			if it := &m.Items[ii]; it.Key != "" && (it.Type == "photo" || it.Type == "video") && len(out) < ixAlbumSample {
				out = append(out, it.Key)
			}
		}
		return out
	}
	personsOf := func(keys []string) map[int64]bool {
		out := map[int64]bool{}
		for _, k := range keys {
			for _, id := range people[k] {
				out[id] = true
			}
		}
		return out
	}
	var out []ixAlbumSug
	for key, c := range cands {
		fa, fb := &m.Families[key[0]], &m.Families[key[1]]
		ra, rb := ixPairRefs(ixPostRef(&m.Posts[fa.Lead]), ixPostRef(&m.Posts[fb.Lead]))
		if only == nil && decided[[2]string{ra, rb}] {
			continue
		}
		small := min(len(fa.Items), len(fb.Items))
		if small < 2 && only == nil {
			continue // a single file: the pair review covers it
		}
		s := ixAlbumSug{A: key[0], B: key[1], Pending: len(c.pairs), Pairs: c.pairs}
		ka, kb := keysOf(key[0]), keysOf(key[1])
		pa, pb := personsOf(ka), personsOf(kb)
		for id := range pa {
			s.Person = s.Person || pb[id]
		}
		veto := !s.Person && len(pa) > 0 && len(pb) > 0
		if m.EmbSim != nil {
			var mutual [][2]string
			s.Sim, mutual = ixChamfer(m.EmbSim, ka, kb)
			for _, pk := range mutual {
				if !c.seen[pk] && !c.seen[[2]string{pk[1], pk[0]}] {
					c.seen[pk] = true
					s.Pairs = append(s.Pairs, pk)
				}
			}
		}
		bar := float32(ixAlbumSimBar)
		if s.Person {
			bar -= ixAlbumSameFor
		}
		offer := !veto && (s.Pending >= min(ixAlbumPairBar, small) || s.Sim >= bar)
		if !offer && only == nil {
			continue
		}
		s.Why = ixAlbumWhy(s)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := &out[i], &out[j]
		if len(a.Pairs) != len(b.Pairs) {
			return len(a.Pairs) > len(b.Pairs)
		}
		sa := min(len(m.Families[a.A].Items), len(m.Families[a.B].Items))
		sb := min(len(m.Families[b.A].Items), len(m.Families[b.B].Items))
		if sa != sb {
			return sa > sb
		}
		if a.A != b.A {
			return a.A < b.A
		}
		return a.B < b.B
	})
	return out
}

// ixChamfer: each file's closest match in the other album, averaged both
// ways; and the pairs that are each other's closest and nearly identical.
func ixChamfer(sim func(a, b string) (float32, bool), ka, kb []string) (float32, [][2]string) {
	if len(ka) == 0 || len(kb) == 0 {
		return 0, nil
	}
	grid := make([][]float32, len(ka))
	for i, a := range ka {
		grid[i] = make([]float32, len(kb))
		for j, b := range kb {
			v, ok := sim(a, b)
			if !ok {
				v = -2
			}
			grid[i][j] = v
		}
	}
	bestA := make([]int, len(ka))
	bestB := make([]int, len(kb))
	for j := range bestB {
		bestB[j] = -1
	}
	var sumA, sumB float32
	var nA, nB int
	for i := range ka {
		bestA[i] = -1
		for j := range kb {
			if grid[i][j] > -2 && (bestA[i] < 0 || grid[i][j] > grid[i][bestA[i]]) {
				bestA[i] = j
			}
			if grid[i][j] > -2 && (bestB[j] < 0 || grid[i][j] > grid[bestB[j]][j]) {
				bestB[j] = i
			}
		}
		if bestA[i] >= 0 {
			sumA += grid[i][bestA[i]]
			nA++
		}
	}
	for j := range kb {
		if bestB[j] >= 0 {
			sumB += grid[bestB[j]][j]
			nB++
		}
	}
	if nA == 0 || nB == 0 {
		return 0, nil
	}
	var mutual [][2]string
	for i, j := range bestA {
		if j >= 0 && bestB[j] == i && grid[i][j] >= ixMLLookAlikeSim {
			mutual = append(mutual, [2]string{ka[i], kb[j]})
		}
	}
	return (sumA/float32(nA) + sumB/float32(nB)) / 2, mutual
}

func ixAlbumWhy(s ixAlbumSug) string {
	why := ""
	if s.Pending > 0 {
		why = fmt.Sprintf("%d look-alike pair(s) between them", s.Pending)
	}
	if s.Sim > 0 {
		if why != "" {
			why += ", "
		}
		why += fmt.Sprintf("files %d%% alike overall", int(math.Round(float64(s.Sim)*100)))
	}
	if s.Person {
		why += ", the same person in both"
	}
	return why
}

// ixAlbumDecided lists the album pairs a person answered.
func (a *App) ixAlbumDecided(ctx context.Context) map[[2]string]bool {
	out := map[[2]string]bool{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT post_a, post_b FROM ix_album_pairs`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var x, y string
		if rows.Scan(&x, &y) == nil {
			out[[2]string{x, y}] = true
		}
	}
	return out
}

// ---------- review ----------

type ixAlbumJ struct {
	A       ixJCard `json:"a"`
	B       ixJCard `json:"b"`
	AFiles  int     `json:"aFiles"`
	BFiles  int     `json:"bFiles"`
	Matched int     `json:"matched"`
	Why     string  `json:"why"`
}

func (a *App) handleIxAlbumReview(w http.ResponseWriter, r *http.Request) {
	m, err := a.ixModelNow(r.Context())
	if m == nil {
		writeJSONError(w, http.StatusInternalServerError, errString(err))
		return
	}
	decided := a.ixAlbumDecided(r.Context())
	out := []ixAlbumJ{}
	for _, s := range m.SameAlbums {
		if len(out) >= 50 {
			break
		}
		ra, rb := ixPairRefs(ixPostRef(&m.Posts[m.Families[s.A].Lead]), ixPostRef(&m.Posts[m.Families[s.B].Lead]))
		if decided[[2]string{ra, rb}] {
			continue // answered since the last rebuild
		}
		out = append(out, ixAlbumJ{A: m.familyCard(s.A), B: m.familyCard(s.B), AFiles: len(m.Families[s.A].Items), BFiles: len(m.Families[s.B].Items), Matched: len(s.Pairs), Why: s.Why})
	}
	writeJSON(w, http.StatusOK, map[string]any{"albums": out})
}

// ixAlbumDecide answers "are these two albums the same?" for the albums
// whose first posts are given.
func (a *App) ixAlbumDecide(ctx context.Context, m *ixModel, postA, postB, decision string) (int, error) {
	if decision != "same" && decision != "different" {
		return 0, errIxBadMode
	}
	pa, okA := m.ByMsg[postA]
	pb, okB := m.ByMsg[postB]
	if !okA || !okB || m.Posts[pa].Family < 0 || m.Posts[pb].Family < 0 {
		return 0, errIxBadRef
	}
	fa, fb := m.Posts[pa].Family, m.Posts[pb].Family
	if fa == fb {
		return 0, errIxBadRef
	}
	ra, rb := ixPairRefs(ixPostRef(&m.Posts[m.Families[fa].Lead]), ixPostRef(&m.Posts[m.Families[fb].Lead]))
	key := ixFamPair(fa, fb)
	var s ixAlbumSug
	for _, x := range m.SameAlbums {
		if x.A == key[0] && x.B == key[1] {
			s = x
			break
		}
	}
	if s.Pairs == nil {
		if list := ixSameAlbums(m, nil, &key); len(list) > 0 {
			s = list[0]
		}
	}
	n := 0
	for i, p := range s.Pairs {
		if decision == "different" && i >= s.Pending {
			break // only the pairs waiting for review; the closest matches were never asked
		}
		if err := a.ixDecide(ctx, p[0], p[1], decision); err != nil {
			return n, err
		}
		n++
	}
	if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_album_pairs(post_a,post_b,decision,at) VALUES(?,?,?,?) ON CONFLICT(post_a,post_b) DO UPDATE SET decision=excluded.decision, at=excluded.at`, ra, rb, decision, ixNowMs()); err != nil {
		return n, err
	}
	a.ixOrgLog("albums_decided", map[string]any{"a": ra, "b": rb, "decision": decision, "pairs": n})
	return n, nil
}

func (a *App) handleIxAlbumDecide(w http.ResponseWriter, r *http.Request) {
	m, err := a.ixModelNow(r.Context())
	if m == nil {
		writeJSONError(w, http.StatusInternalServerError, errString(err))
		return
	}
	b := ixBody(r)
	n, err := a.ixAlbumDecide(r.Context(), m, toString(b["a"]), toString(b["b"]), toString(b["decision"]))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.ixModelSoon()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pairs": n})
}
