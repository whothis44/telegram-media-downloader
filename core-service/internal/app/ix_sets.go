package app

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
)

var (
	errIxBadRef  = errors.New("not a post or file of the archive")
	errIxBadMode = errors.New("unknown choice")
)

func ixNowMs() int64 { return time.Now().UnixMilli() }

// Media sets: media that belong together although no single post holds
// them all. Several people post overlapping selections of the same photos
// and clips (say three posts of ten, eighteen distinct pictures between
// them, five shared); families (one post and its reposts) cannot join them,
// since each shares only a few files with the others.
//
// Families are linked by the files they share, each shared file weighing
// more the rarer it is (a picture in 2 families says much, one in 40 says
// little; generic files say nothing). The linked families are split into
// sets by community detection (Louvain modularity), so a busy hub cannot
// pull unrelated families into one set. Within a set every file gets a
// role:
//
//	core     in two or more of the set's families
//	member   in one of them, and not more strongly tied anywhere else
//	elsewhere in one of them, but in more families of another set
//	loose    in one of them, and, when pictures can be compared (image
//	         recognition switched on), unlike the set's core
//
// A person's corrections are kept by post and file: a post can be taken
// out of its set or tied to another post, and a file marked as belonging
// or not.

const (
	ixSetMaxShare = 50   // a file in more families than this links nothing
	ixSetTieW     = 10.0 // weight of a tie a person made
	ixSetLooseSim = 0.62 // cosine below which a one-off file is unlike the set's core
)

type ixSetItem struct {
	Item  int32   // canonical item of the file's duplicate set
	Role  string  // core, member, elsewhere, loose
	In    int     // the set's families holding it
	Other int32   // elsewhere: the set it belongs to (-1 none)
	Sim   float32 // closeness to the core (0: not compared)
	Fixed bool    // a person said so
}

type ixMSet struct {
	ID       int32
	Families []int32 // oldest first
	Posts    int
	Chats    int
	First    int64
	Last     int64
	Items    []ixSetItem
	Anchor   string // the lead post of its first family: names it across rebuilds
}

// ixSetFixes are a person's corrections.
type ixSetFixes struct {
	alone map[string]bool              // post ref -> taken out of any set
	ties  [][2]string                  // post refs tied together
	item  map[string]map[string]string // set anchor -> file key -> in/out
	apart [][2]string                  // post refs whose albums a person said differ: look-alikes never link them
}

func (a *App) ixLoadSetFixes(ctx context.Context) ixSetFixes {
	f := ixSetFixes{alone: map[string]bool{}, item: map[string]map[string]string{}}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT post, mode, other FROM ix_org_set_posts`); err == nil {
		for rows.Next() {
			var post, mode, other string
			if rows.Scan(&post, &mode, &other) != nil {
				continue
			}
			switch mode {
			case "alone":
				f.alone[post] = true
			case "tie":
				f.ties = append(f.ties, [2]string{post, other})
			}
		}
		rows.Close()
	}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT anchor, key, decision FROM ix_org_set_items`); err == nil {
		for rows.Next() {
			var anchor, key, decision string
			if rows.Scan(&anchor, &key, &decision) != nil {
				continue
			}
			if f.item[anchor] == nil {
				f.item[anchor] = map[string]string{}
			}
			f.item[anchor][key] = decision
		}
		rows.Close()
	}
	a.ixLoadAlbumFixes(ctx, &f)
	return f
}

// ---------- Louvain ----------

type ixGEdge struct {
	to int32
	w  float64
}

// ixLouvain finds communities in a weighted undirected graph (adjacency
// lists may hold self loops); it returns each node's community, numbered
// from 0.
func ixLouvain(adj [][]ixGEdge) []int32 {
	n := len(adj)
	node := make([]int32, n) // original node -> current community
	for i := range node {
		node[i] = int32(i)
	}
	cur := adj
	for level := 0; level < 12; level++ {
		comm, moved := ixLouvainPass(cur)
		if !moved {
			break
		}
		// Renumber and fold communities into nodes.
		ids := map[int32]int32{}
		for _, c := range comm {
			if _, ok := ids[c]; !ok {
				ids[c] = int32(len(ids))
			}
		}
		for i := range node {
			node[i] = ids[comm[node[i]]]
		}
		next := make([]map[int32]float64, len(ids))
		for i := range next {
			next[i] = map[int32]float64{}
		}
		// A self loop is stored once; an edge between two nodes is stored in
		// both their lists, so when both fall in one community each half
		// becomes part of its self loop.
		for i, edges := range cur {
			ci := ids[comm[i]]
			for _, e := range edges {
				cj := ids[comm[e.to]]
				switch {
				case int32(i) == e.to:
					next[ci][ci] += e.w
				case ci == cj:
					next[ci][ci] += e.w / 2
				default:
					next[ci][cj] += e.w
				}
			}
		}
		folded := make([][]ixGEdge, len(ids))
		for i, m := range next {
			for j, w := range m {
				folded[i] = append(folded[i], ixGEdge{j, w})
			}
			sort.Slice(folded[i], func(a, b int) bool { return folded[i][a].to < folded[i][b].to })
		}
		if len(folded) == len(cur) {
			break
		}
		cur = folded
	}
	return node
}

// ixLouvainPass moves nodes between communities while modularity improves.
func ixLouvainPass(adj [][]ixGEdge) ([]int32, bool) {
	n := len(adj)
	comm := make([]int32, n)
	deg := make([]float64, n)
	tot := make([]float64, n)
	m2 := 0.0
	for i, edges := range adj {
		comm[i] = int32(i)
		for _, e := range edges {
			if int(e.to) == i {
				deg[i] += 2 * e.w
			} else {
				deg[i] += e.w
			}
		}
		tot[i] = deg[i]
		m2 += deg[i]
	}
	if m2 == 0 {
		return comm, false
	}
	movedAny := false
	links := map[int32]float64{}
	for pass := 0; pass < 30; pass++ {
		moved := false
		for i := 0; i < n; i++ {
			if len(adj[i]) == 0 {
				continue
			}
			for k := range links {
				delete(links, k)
			}
			for _, e := range adj[i] {
				if int(e.to) != i {
					links[comm[e.to]] += e.w
				}
			}
			old := comm[i]
			tot[old] -= deg[i]
			best, bestGain := old, links[old]-tot[old]*deg[i]/m2
			for c, w := range links {
				if g := w - tot[c]*deg[i]/m2; g > bestGain+1e-12 || (math.Abs(g-bestGain) <= 1e-12 && c < best) {
					best, bestGain = c, g
				}
			}
			comm[i] = best
			tot[best] += deg[i]
			if best != old {
				moved, movedAny = true, true
			}
		}
		if !moved {
			break
		}
	}
	return comm, movedAny
}

// ixSetMaxFamilies: a community larger than this (long chains of partial
// reposts make them) is split again, inside itself, until its parts are
// tight or cannot be split.
const ixSetMaxFamilies = 30

// ixSplitLarge splits a large community by running Louvain on its own
// links only (a smaller graph has a finer resolution).
func ixSplitLarge(adj [][]ixGEdge, fams []int32, depth int) [][]int32 {
	if len(fams) <= ixSetMaxFamilies || depth >= 6 {
		return [][]int32{fams}
	}
	local := make(map[int32]int32, len(fams))
	for i, f := range fams {
		local[f] = int32(i)
	}
	sub := make([][]ixGEdge, len(fams))
	for i, f := range fams {
		for _, e := range adj[f] {
			if j, ok := local[e.to]; ok {
				sub[i] = append(sub[i], ixGEdge{j, e.w})
			}
		}
	}
	comm := ixLouvain(sub)
	byComm := map[int32][]int32{}
	var order []int32
	for i, c := range comm {
		if _, ok := byComm[c]; !ok {
			order = append(order, c)
		}
		byComm[c] = append(byComm[c], fams[i])
	}
	if len(byComm) < 2 {
		return [][]int32{fams}
	}
	var out [][]int32
	for _, c := range order {
		out = append(out, ixSplitLarge(adj, byComm[c], depth+1)...)
	}
	return out
}

// ---------- building sets ----------

// ixAssignSets groups the model's families into sets. sim, when not
// nil, says how alike two files look (cosine of their image embeddings; ok
// false when either has none).
func ixAssignSets(m *ixModel, fixes ixSetFixes, sim func(a, b string) (float32, bool)) {
	nf := len(m.Families)
	m.LooseSim = ixSetLooseSim
	famClusters := make([][]int32, nf)
	holders := map[int32][]int32{} // cluster -> families
	for fi := range m.Families {
		seen := map[int32]bool{}
		for _, pi := range m.Families[fi].Posts {
			for _, ii := range m.Posts[pi].Items {
				c := m.Items[ii].Cluster
				if seen[c] || m.Clusters[c].Generic {
					continue
				}
				seen[c] = true
				famClusters[fi] = append(famClusters[fi], c)
				holders[c] = append(holders[c], int32(fi))
			}
		}
	}
	alone := make([]bool, nf)
	famOfPost := map[string]int32{}
	for fi := range m.Families {
		for _, pi := range m.Families[fi].Posts {
			ref := ixPostRef(&m.Posts[pi])
			famOfPost[ref] = int32(fi)
			if fixes.alone[ref] {
				alone[fi] = true
			}
		}
	}
	weights := map[[2]int32]float64{}
	for _, fams := range holders {
		k := len(fams)
		if k < 2 || k > ixSetMaxShare {
			continue
		}
		w := 1 / math.Log2(1+float64(k))
		for x := 0; x < k; x++ {
			for y := x + 1; y < k; y++ {
				a, b := fams[x], fams[y]
				if alone[a] || alone[b] {
					continue
				}
				if b < a {
					a, b = b, a
				}
				weights[[2]int32{a, b}] += w
			}
		}
	}
	for _, t := range fixes.ties {
		a, okA := famOfPost[t[0]]
		b, okB := famOfPost[t[1]]
		if okA && okB && a != b && !alone[a] && !alone[b] {
			if b < a {
				a, b = b, a
			}
			weights[[2]int32{a, b}] += ixSetTieW
		}
	}
	adj := make([][]ixGEdge, nf)
	keys := make([][2]int32, 0, len(weights))
	for k := range weights {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		w := weights[k]
		adj[k[0]] = append(adj[k[0]], ixGEdge{k[1], w})
		adj[k[1]] = append(adj[k[1]], ixGEdge{k[0], w})
	}
	comm := ixLouvain(adj)
	members := map[int32][]int32{}
	for fi, c := range comm {
		if len(adj[fi]) > 0 {
			members[c] = append(members[c], int32(fi))
		}
	}
	var groups [][]int32
	var parts [][]int32
	for _, fams := range members {
		parts = append(parts, ixSplitLarge(adj, fams, 0)...)
	}
	for _, fams := range parts {
		if len(fams) >= 2 {
			sort.Slice(fams, func(i, j int) bool {
				a, b := &m.Families[fams[i]], &m.Families[fams[j]]
				if a.First != b.First {
					return a.First < b.First
				}
				return fams[i] < fams[j]
			})
			groups = append(groups, fams)
		}
	}
	sort.Slice(groups, func(i, j int) bool {
		a, b := &m.Families[groups[i][0]], &m.Families[groups[j][0]]
		if a.First != b.First {
			return a.First < b.First
		}
		return groups[i][0] < groups[j][0]
	})
	m.FamSet = make([]int32, nf)
	for i := range m.FamSet {
		m.FamSet[i] = -1
	}
	m.Sets = m.Sets[:0]
	for si, fams := range groups {
		for _, fi := range fams {
			m.FamSet[fi] = int32(si)
		}
		m.Sets = append(m.Sets, ixMSet{ID: int32(si), Families: fams})
	}
	// Each file's families per set, to place one-offs.
	inSet := func(c int32) map[int32]int {
		out := map[int32]int{}
		for _, fi := range holders[c] {
			if s := m.FamSet[fi]; s >= 0 {
				out[s]++
			}
		}
		return out
	}
	for si := range m.Sets {
		s := &m.Sets[si]
		chats, posts := map[string]bool{}, 0
		order := []int32{}
		seen := map[int32]bool{}
		for _, fi := range s.Families {
			f := &m.Families[fi]
			posts += len(f.Posts)
			for _, pi := range f.Posts {
				chats[m.Posts[pi].Group] = true
				if d := m.Posts[pi].Date; d > s.Last {
					s.Last = d
				}
			}
			if s.First == 0 || f.First < s.First {
				s.First = f.First
			}
			for _, c := range famClusters[fi] {
				if !seen[c] {
					seen[c] = true
					order = append(order, c)
				}
			}
		}
		s.Posts, s.Chats = posts, len(chats)
		s.Anchor = ixPostRef(&m.Posts[m.Families[s.Families[0]].Lead])
		// A person's placements follow the set: they were made under the
		// set's first post at the time, which stays one of its posts even
		// when an older post joins it later.
		fixed := map[string]string{}
		for _, fi := range s.Families {
			for _, pi := range m.Families[fi].Posts {
				for k, v := range fixes.item[ixPostRef(&m.Posts[pi])] {
					fixed[k] = v
				}
			}
		}
		for k, v := range fixes.item[s.Anchor] {
			fixed[k] = v // the current first post wins
		}
		var core []string
		for _, c := range order {
			counts := inSet(c)
			it := ixSetItem{Item: m.Clusters[c].Canon, In: counts[int32(si)], Other: -1}
			if it.In >= 2 {
				it.Role = "core"
				if k := m.Items[it.Item].Key; k != "" {
					core = append(core, k)
				}
			} else {
				it.Role = "member"
				for other, n := range counts {
					if other != int32(si) && n > it.In && (it.Other < 0 || n > counts[it.Other]) {
						it.Role, it.Other = "elsewhere", other
					}
				}
			}
			s.Items = append(s.Items, it)
		}
		// One-offs unlike every core file are loose, when pictures can be
		// compared.
		if sim != nil && len(core) > 0 {
			for i := range s.Items {
				it := &s.Items[i]
				key := m.Items[it.Item].Key
				if it.Role != "member" || key == "" {
					continue
				}
				best, compared := float32(-1), false
				for _, ck := range core {
					if v, ok := sim(key, ck); ok {
						compared = true
						if v > best {
							best = v
						}
					}
				}
				if compared {
					it.Sim = best
					if best < ixSetLooseSim {
						it.Role = "loose"
					}
				}
			}
		}
		for i := range s.Items {
			it := &s.Items[i]
			switch fixed[m.Items[it.Item].Key] {
			case "in":
				if it.Role != "core" {
					it.Role = "member"
				}
				it.Fixed = true
			case "out":
				it.Role, it.Fixed = "loose", true
			}
		}
		rank := map[string]int{"core": 0, "member": 1, "loose": 2, "elsewhere": 3}
		sort.SliceStable(s.Items, func(i, j int) bool { return rank[s.Items[i].Role] < rank[s.Items[j].Role] })
	}
}

// ixSetLikeness compares two files for the sets: files showing the same
// person belong together (for self-filmed content this says far more than
// what the pictures generally show); otherwise the picture embeddings
// decide. nil when neither is available.
func ixSetLikeness(m *ixModel, emb func(string, string) (float32, bool)) func(string, string) (float32, bool) {
	people := map[string][]int64{}
	for id, keys := range m.PersonKeys {
		if m.PersonHidden[id] {
			continue
		}
		for k := range keys {
			people[k] = append(people[k], id)
		}
	}
	if len(people) == 0 {
		return emb
	}
	return func(a, b string) (float32, bool) {
		for _, p := range people[a] {
			for _, q := range people[b] {
				if p == q {
					return 1, true
				}
			}
		}
		if emb != nil {
			return emb(a, b)
		}
		return 0, false
	}
}

// ixSetCounts counts a set's files by role.
func ixSetCounts(s *ixMSet) map[string]int {
	out := map[string]int{}
	for _, it := range s.Items {
		out[it.Role]++
	}
	return out
}

// ixSetFix records a correction: a post taken out of sets ("alone"),
// tied to another post ("tie"), returned to normal ("auto"), or a file
// marked as in or out of a set.
func (a *App) ixSetFixPost(ctx context.Context, post, mode, other string) error {
	if !ixValidRef("post:" + post) {
		return errIxBadRef
	}
	var err error
	switch mode {
	case "auto":
		_, err = a.db.Writer.ExecContext(ctx, `DELETE FROM ix_org_set_posts WHERE post=?`, post)
	case "alone":
		_, err = a.db.Writer.ExecContext(ctx, `INSERT INTO ix_org_set_posts(post,mode,other,at) VALUES(?,?,'',?) ON CONFLICT(post) DO UPDATE SET mode='alone', other='', at=excluded.at`, post, mode, ixNowMs())
	case "tie":
		if !ixValidRef("post:" + other) {
			return errIxBadRef
		}
		_, err = a.db.Writer.ExecContext(ctx, `INSERT INTO ix_org_set_posts(post,mode,other,at) VALUES(?,?,?,?) ON CONFLICT(post) DO UPDATE SET mode='tie', other=excluded.other, at=excluded.at`, post, mode, other, ixNowMs())
	default:
		return errIxBadMode
	}
	if err == nil {
		a.ixOrgLog("set_post", map[string]any{"post": post, "mode": mode, "other": other})
	}
	return err
}

func (a *App) ixSetFixItem(ctx context.Context, anchor, key, decision string) error {
	if !ixValidRef("post:"+anchor) || strings.TrimSpace(key) == "" {
		return errIxBadRef
	}
	var err error
	switch decision {
	case "auto":
		_, err = a.db.Writer.ExecContext(ctx, `DELETE FROM ix_org_set_items WHERE anchor=? AND key=?`, anchor, key)
	case "in", "out":
		_, err = a.db.Writer.ExecContext(ctx, `INSERT INTO ix_org_set_items(anchor,key,decision,at) VALUES(?,?,?,?) ON CONFLICT(anchor,key) DO UPDATE SET decision=excluded.decision, at=excluded.at`, anchor, key, decision, ixNowMs())
	default:
		return errIxBadMode
	}
	if err == nil {
		a.ixOrgLog("set_item", map[string]any{"set": anchor, "file": key, "decision": decision})
	}
	return err
}
