package app

import (
	"container/heap"
	"net/http"
	"sort"
)

// Which chats repeat which.
//
// Every chat is taken as the set of distinct files it posted (copies,
// re-encodes and confirmed look-alikes fold into one; logos and stickers,
// "generic" files, and files whose identity is unknown are left out). For
// each pair of chats the model counts the files both posted, and which of
// them posted each first. From that:
//
//	share of A also in B   files in both / files of A (A in B)
//	unique to A            files of A in no other chat
//	X had it first         of the files both posted, the part X posted earlier
//
// and a chat is called (starting cut-offs, from organising.md C):
//
//	mirror     shares 80% or more of everything with one chat, both ways
//	reposter   80% or more of it is in one chat, under half of that chat in it
//	variety    under half of it is in any one chat, but under 20% is its own
//	unique     half or more of it is in no other chat
//	mixed      anything else (or under 5 files: not called)
//
// "What each chat adds" ranks chats by greedy cover: take the chat with the
// most files, then repeatedly the one adding most files not yet seen.

const (
	ixOverlapMinFiles = 5  // fewer files: a chat is not called anything
	ixOverlapMaxChats = 50 // a file in more chats counts for each, but not for pairs
)

type ixOverlap struct {
	Groups  []string
	index   map[string]int
	Files   []int     // distinct files per chat
	Unique  []int     // of them, in no other chat
	First   []int     // of the shared ones, those this chat posted before every other
	Shared  [][]int32 // [a][b]: files in both
	Earlier [][]int32 // [a][b]: files in both that a posted first
	only    []bool    // cluster -> in one chat only
	Cover   []ixCoverStep
}

type ixCoverStep struct {
	Group string `json:"group"`
	Adds  int    `json:"adds"`  // files no chat before it in the list has
	Files int    `json:"files"` // its files
	Total int    `json:"total"` // files seen so far, it included
}

// ixComputeOverlap counts files shared between chats (see the top of the
// file). It runs in the model build: one pass over the clusters, plus the
// pairs of chats each shared file is in.
func ixComputeOverlap(m *ixModel) *ixOverlap {
	o := &ixOverlap{index: map[string]int{}, only: make([]bool, len(m.Clusters))}
	add := func(g string) {
		if _, ok := o.index[g]; !ok {
			o.index[g] = len(o.Groups)
			o.Groups = append(o.Groups, g)
		}
	}
	for _, g := range m.Groups {
		add(g)
	}
	for i := range m.Posts {
		add(m.Posts[i].Group)
	}
	n := len(o.Groups)
	o.Files, o.Unique, o.First = make([]int, n), make([]int, n), make([]int, n)
	o.Shared, o.Earlier = make([][]int32, n), make([][]int32, n)
	for i := range o.Shared {
		o.Shared[i], o.Earlier[i] = make([]int32, n), make([]int32, n)
	}
	lists := make([][]int32, n) // chat -> its clusters, for the cover
	type seen struct {
		chat int
		date int64
	}
	var buf []seen
	for ci := range m.Clusters {
		c := &m.Clusters[ci]
		if c.Generic {
			continue
		}
		buf = buf[:0]
		known := false
		for _, ii := range c.Items {
			it := &m.Items[ii]
			known = known || it.Key != "" || it.Ident != "" || it.Like != ""
			ch, d := o.index[it.Group], m.Posts[it.Post].Date
			found := false
			for k := range buf {
				if buf[k].chat == ch {
					buf[k].date = min(buf[k].date, d)
					found = true
					break
				}
			}
			if !found {
				buf = append(buf, seen{ch, d})
			}
		}
		if !known {
			continue
		}
		for _, s := range buf {
			o.Files[s.chat]++
			lists[s.chat] = append(lists[s.chat], int32(ci))
		}
		if len(buf) == 1 {
			o.Unique[buf[0].chat]++
			o.only[ci] = true
			continue
		}
		first, tie := buf[0], false
		for _, s := range buf[1:] {
			switch {
			case s.date < first.date:
				first, tie = s, false
			case s.date == first.date:
				tie = true
			}
		}
		if !tie {
			o.First[first.chat]++
		}
		if len(buf) > ixOverlapMaxChats {
			continue
		}
		for x := 0; x < len(buf); x++ {
			for y := x + 1; y < len(buf); y++ {
				a, b := buf[x], buf[y]
				o.Shared[a.chat][b.chat]++
				o.Shared[b.chat][a.chat]++
				switch {
				case a.date < b.date:
					o.Earlier[a.chat][b.chat]++
				case b.date < a.date:
					o.Earlier[b.chat][a.chat]++
				}
			}
		}
	}
	o.Cover = ixGreedyCover(o.Groups, lists, len(m.Clusters))
	return o
}

// ---------- greedy cover ----------

type ixCoverItem struct {
	chat, gain int
}
type ixCoverHeap []ixCoverItem

func (h ixCoverHeap) Len() int { return len(h) }
func (h ixCoverHeap) Less(i, j int) bool {
	if h[i].gain != h[j].gain {
		return h[i].gain > h[j].gain
	}
	return h[i].chat < h[j].chat
}
func (h ixCoverHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *ixCoverHeap) Push(x any)   { *h = append(*h, x.(ixCoverItem)) }
func (h *ixCoverHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// ixGreedyCover orders chats by the files each adds to those before it. A
// chat's gain only falls as others are taken, so a stale gain is
// recomputed only when it reaches the top.
func ixGreedyCover(groups []string, lists [][]int32, clusters int) []ixCoverStep {
	covered := make([]bool, clusters)
	h := &ixCoverHeap{}
	for i, l := range lists {
		if len(l) > 0 {
			*h = append(*h, ixCoverItem{i, len(l)})
		}
	}
	heap.Init(h)
	var out []ixCoverStep
	total := 0
	for h.Len() > 0 {
		top := heap.Pop(h).(ixCoverItem)
		gain := 0
		for _, c := range lists[top.chat] {
			if !covered[c] {
				gain++
			}
		}
		if h.Len() > 0 && gain < (*h)[0].gain {
			heap.Push(h, ixCoverItem{top.chat, gain})
			continue
		}
		for _, c := range lists[top.chat] {
			covered[c] = true
		}
		total += gain
		out = append(out, ixCoverStep{Group: groups[top.chat], Adds: gain, Files: len(lists[top.chat]), Total: total})
	}
	return out
}

// ---------- reading it ----------

// class names what kind of chat a is (see the top of the file), and the
// chat it mirrors or reposts (-1: none).
func (o *ixOverlap) class(a int) (string, int) {
	files := o.Files[a]
	if files < ixOverlapMinFiles {
		return "", -1
	}
	unique := float64(o.Unique[a]) / float64(files)
	best, bestC := -1, 0.0
	mirror, bestJ := -1, 0.0
	for b := range o.Groups {
		if b == a || o.Files[b] == 0 {
			continue
		}
		s := float64(o.Shared[a][b])
		if c := s / float64(files); c > bestC {
			best, bestC = b, c
		}
		if j := s / float64(files+o.Files[b]-int(o.Shared[a][b])); j > bestJ {
			mirror, bestJ = b, j
		}
	}
	switch {
	case bestJ >= 0.8:
		return "mirror", mirror
	case bestC >= 0.8 && float64(o.Shared[best][a])/float64(o.Files[best]) < 0.5:
		return "reposter", best
	case bestC < 0.5 && unique < 0.2:
		return "variety", -1
	case unique >= 0.5:
		return "unique", -1
	}
	return "mixed", -1
}

// ixPostOnlyHere: the post has a file no other chat has posted.
func (m *ixModel) ixPostOnlyHere(pi int32) bool {
	for _, ii := range m.Posts[pi].Items {
		c := m.Items[ii].Cluster
		if m.Overlap != nil {
			if int(c) < len(m.Overlap.only) && m.Overlap.only[c] {
				return true
			}
		} else if cl := &m.Clusters[c]; cl.Chats == 1 && !cl.Generic {
			return true
		}
	}
	return false
}

type ixOverlapChatJ struct {
	Group     string          `json:"group"`
	Name      string          `json:"name"`
	Files     int             `json:"files"`
	Unique    int             `json:"unique"`
	Elsewhere float64         `json:"elsewhere"` // share of its files also in another chat
	FirstOf   float64         `json:"firstOf"`   // of its shared files, the part it posted first
	Class     string          `json:"class,omitempty"`
	With      string          `json:"with,omitempty"`
	WithName  string          `json:"withName,omitempty"`
	Top       *ixOverlapPairJ `json:"top,omitempty"` // the chat holding most of its files
}

type ixOverlapPairJ struct {
	Group      string  `json:"group"`
	Name       string  `json:"name"`
	Shared     int     `json:"shared"`
	Share      float64 `json:"share"`      // of this chat's files, the part also there
	TheirShare float64 `json:"theirShare"` // of that chat's files, the part also here
	TheyFirst  float64 `json:"theyFirst"`  // of the shared files, the part that chat posted first
	WeFirst    float64 `json:"weFirst"`    // ... and the part this chat posted first
}

func (o *ixOverlap) pair(m *ixModel, a, b int) ixOverlapPairJ {
	s := int(o.Shared[a][b])
	j := ixOverlapPairJ{Group: o.Groups[b], Name: m.GroupName[o.Groups[b]], Shared: s}
	if o.Files[a] > 0 {
		j.Share = float64(s) / float64(o.Files[a])
	}
	if o.Files[b] > 0 {
		j.TheirShare = float64(s) / float64(o.Files[b])
	}
	if s > 0 {
		j.TheyFirst = float64(o.Earlier[b][a]) / float64(s)
		j.WeFirst = float64(o.Earlier[a][b]) / float64(s)
	}
	return j
}

func (o *ixOverlap) chat(m *ixModel, a int) ixOverlapChatJ {
	g := o.Groups[a]
	j := ixOverlapChatJ{Group: g, Name: m.GroupName[g], Files: o.Files[a], Unique: o.Unique[a]}
	if j.Name == "" {
		j.Name = "Chat " + g
	}
	if shared := o.Files[a] - o.Unique[a]; o.Files[a] > 0 {
		j.Elsewhere = float64(shared) / float64(o.Files[a])
		if shared > 0 {
			j.FirstOf = float64(o.First[a]) / float64(shared)
		}
	}
	cls, with := o.class(a)
	j.Class = cls
	if with >= 0 {
		j.With, j.WithName = o.Groups[with], m.GroupName[o.Groups[with]]
	}
	best := -1
	for b := range o.Groups {
		if b != a && o.Shared[a][b] > 0 && (best < 0 || o.Shared[a][b] > o.Shared[a][best]) {
			best = b
		}
	}
	if best >= 0 {
		p := o.pair(m, a, best)
		j.Top = &p
	}
	return j
}

// handleIxOverlap: with group=, how that chat overlaps the others (most
// shared first); without, every chat in brief and what each adds.
func (a *App) handleIxOverlap(w http.ResponseWriter, r *http.Request) {
	m, err := a.ixModelNow(r.Context())
	if m == nil || m.Overlap == nil {
		writeJSONError(w, http.StatusInternalServerError, errString(err))
		return
	}
	o := m.Overlap
	if g := r.URL.Query().Get("group"); g != "" {
		ai, ok := o.index[g]
		if !ok {
			writeJSONError(w, http.StatusNotFound, "no such chat")
			return
		}
		var with []ixOverlapPairJ
		for b := range o.Groups {
			if b != ai && o.Shared[ai][b] > 0 {
				with = append(with, o.pair(m, ai, b))
			}
		}
		sort.Slice(with, func(i, j int) bool {
			if with[i].Shared != with[j].Shared {
				return with[i].Shared > with[j].Shared
			}
			return with[i].Group < with[j].Group
		})
		if len(with) > 20 {
			with = with[:20]
		}
		if with == nil {
			with = []ixOverlapPairJ{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"chat": o.chat(m, ai), "with": with})
		return
	}
	chats := make([]ixOverlapChatJ, 0, len(o.Groups))
	for i := range o.Groups {
		if o.Files[i] > 0 {
			chats = append(chats, o.chat(m, i))
		}
	}
	type stepJ struct {
		ixCoverStep
		Name string `json:"name"`
	}
	cover := make([]stepJ, 0, len(o.Cover))
	for _, s := range o.Cover {
		cover = append(cover, stepJ{s, m.GroupName[s.Group]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": chats, "cover": cover})
}
