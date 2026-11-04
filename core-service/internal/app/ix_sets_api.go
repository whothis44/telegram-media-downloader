package app

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// The browser's media sets and its image-recognition search.

func registerIxSetRoutes(mux *http.ServeMux, a *App) {
	h := func(f http.HandlerFunc) http.Handler { return a.requireAdmin(f) }
	mux.Handle("GET /api/ix/browse/set", h(a.handleIxBrowseSet))
	mux.Handle("POST /api/ix/browse/set/post", h(a.handleIxSetPost))
	mux.Handle("POST /api/ix/browse/set/item", h(a.handleIxSetItem))
	mux.Handle("GET /api/ix/browse/similar", h(a.handleIxSimilar))
	mux.Handle("POST /api/ix/ml/check", h(a.handleIxMLCheck))
}

const ixSetFamiliesPage = 25

func ixNextPage(to, n int) int {
	if to >= n {
		return 0
	}
	return to
}

// setCard: the set's files that belong (shared ones first), with counts.
func (m *ixModel) setCard(si int32) ixJCard {
	s := &m.Sets[si]
	lead := &m.Posts[m.Families[s.Families[0]].Lead]
	c := ixJCard{Kind: "set", ID: int64(si), Date: s.First, Group: lead.Group, GroupName: m.GroupName[lead.Group], Msg: lead.Msg, Caption: lead.Caption,
		Posts: s.Posts, Chats: s.Chats, Family: m.Families[s.Families[0]].ID, Families: len(s.Families), Roles: ixSetCounts(s)}
	shown := 0
	for _, it := range s.Items {
		if it.Role != "core" && it.Role != "member" {
			continue
		}
		if shown >= ixCardItems {
			c.More++
			continue
		}
		j := m.jitem(it.Item)
		j.Role, j.Sim = it.Role, it.Sim
		c.Items = append(c.Items, j)
		shown++
	}
	return c
}

func (m *ixModel) setMatches(si int32, f ixFeedQuery, hits map[int32]bool) bool {
	for _, fi := range m.Sets[si].Families {
		for _, pi := range m.Families[fi].Posts {
			if m.postMatches(pi, f, hits) {
				return true
			}
		}
	}
	return false
}

// ixSetFeed lists the sets with a matching post.
func (m *ixModel) ixSetFeed(f ixFeedQuery, hits map[int32]bool) []int32 {
	var list []int32
	for si := range m.Sets {
		if m.setMatches(int32(si), f, hits) {
			list = append(list, int32(si))
		}
	}
	newest := f.Sort != "old"
	sort.SliceStable(list, func(i, j int) bool {
		a, b := &m.Sets[list[i]], &m.Sets[list[j]]
		if a.First != b.First {
			return a.First > b.First == newest
		}
		return (list[i] > list[j]) == newest
	})
	return list
}

func (a *App) handleIxBrowseSet(w http.ResponseWriter, r *http.Request) {
	m, err := a.ixModelNow(r.Context())
	if m == nil {
		writeJSONError(w, http.StatusInternalServerError, errString(err))
		return
	}
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil || id < 0 || id >= len(m.Sets) {
		writeJSONError(w, http.StatusNotFound, "no such set (the index may have been rebuilt; reload)")
		return
	}
	s := &m.Sets[id]
	card := m.setCard(int32(id))
	items := map[string][]ixJItem{}
	for _, it := range s.Items {
		j := m.jitem(it.Item)
		j.Role, j.Sim, j.Fixed = it.Role, it.Sim, it.Fixed
		if it.Other >= 0 {
			j.OtherSet = it.Other + 1 // 0 means none in JSON
		}
		items[it.Role] = append(items[it.Role], j)
	}
	type fam struct {
		Family int32     `json:"family"`
		Posts  []ixJPost `json:"posts"`
	}
	fams := []fam{}
	// Large sets list their posts a page at a time.
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	from = max(0, min(from, len(s.Families)))
	to := min(len(s.Families), from+ixSetFamiliesPage)
	for _, fi := range s.Families[from:to] {
		f := fam{Family: fi}
		for _, pi := range m.Families[fi].Posts {
			f.Posts = append(f.Posts, m.jpost(pi))
		}
		fams = append(fams, f)
	}
	writeJSON(w, http.StatusOK, map[string]any{"set": card, "anchor": s.Anchor, "items": items, "families": fams, "from": from, "next": ixNextPage(to, len(s.Families)), "totalFamilies": len(s.Families), "compared": a.ixEmbSim() != nil})
}

func (a *App) handleIxSetPost(w http.ResponseWriter, r *http.Request) {
	b := ixBody(r)
	if err := a.ixSetFixPost(r.Context(), toString(b["post"]), toString(b["mode"]), toString(b["other"])); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.ixModelSoon()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleIxSetItem(w http.ResponseWriter, r *http.Request) {
	b := ixBody(r)
	if err := a.ixSetFixItem(r.Context(), toString(b["anchor"]), toString(b["key"]), toString(b["decision"])); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.ixModelSoon()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleIxSimilar searches pictures and videos by a description (q) or by
// their likeness to a file (like), within the browser's chat filters.
func (a *App) handleIxSimilar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := a.ixModelNow(ctx)
	if m == nil {
		writeJSONError(w, http.StatusInternalServerError, errString(err))
		return
	}
	q := r.URL.Query()
	text, like, thing := strings.TrimSpace(q.Get("q")), strings.TrimSpace(q.Get("like")), strings.TrimSpace(q.Get("thing"))
	if text == "" && like == "" && thing == "" {
		writeJSONError(w, http.StatusBadRequest, "describe what to look for, or choose a file")
		return
	}
	if len(text) > 500 {
		text = text[:500]
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 600 {
		limit = 120
	}
	typ := q.Get("type")
	if typ != "photo" && typ != "video" {
		typ = ""
	}
	keep := ixMLFilter(m, q.Get("group"), q.Get("category"), typ)
	var hits []ixEmbHit
	cutoff := float32(-2)
	strong := -1
	switch {
	case thing != "":
		hits, err = a.ixThingHits(ctx, thing, limit, keep)
	case like != "":
		hits, err = a.ixMLSearch(ctx, m, text, like, limit, keep)
	default:
		// A description: only pictures that stand out, or the closest few
		// as weak matches when none do.
		var lk ixLooks
		lk, err = a.ixLooksFor(ctx, m, text, limit, keep)
		hits, cutoff, strong = lk.hits, lk.cutoff, lk.strong
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	cards := []ixJCard{}
	weak := 0
	for _, h := range hits {
		ii, ok := m.ByKey[h.Key]
		if !ok {
			continue
		}
		c := m.mediaCard(ii)
		c.Items[0].Sim = h.Sim
		if h.Sim < cutoff {
			c.Weak = true
			weak++
		}
		cards = append(cards, c)
	}
	total := len(cards)
	if strong >= 0 {
		total = strong
	}
	writeJSON(w, http.StatusOK, map[string]any{"cards": cards, "total": total, "weak": weak, "cutoff": cutoff, "next": 0, "built": m.Built.UnixMilli()})
}

func (a *App) handleIxMLCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c := a.ixMLClientNow(ctx)
	if c == nil {
		writeJSONError(w, http.StatusBadRequest, "image recognition is off: enter its address first")
		return
	}
	if err := c.ping(ctx); err != nil {
		a.ixMLNote(false, err.Error())
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	// Small requests load both models (the first time they are downloaded).
	if _, err := c.embed(ctx, nil, "a photo"); err != nil {
		a.ixMLNote(true, err.Error())
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	if _, err := c.embed(ctx, ixMLTestPicture(), ""); err != nil {
		a.ixMLNote(true, err.Error())
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	// Faces and text reading, when on, load their own models.
	st := a.ixLoadSettings(ctx)
	if st.MLFaces {
		if _, err := c.faces(ctx, ixMLTestPicture()); err != nil {
			a.ixMLTaskNoter("faces")(false, err.Error())
			writeJSONError(w, http.StatusBadGateway, "faces: "+err.Error())
			return
		}
		a.ixMLTaskNoter("faces")(true, "")
	}
	if st.MLText {
		if _, err := c.ocr(ctx, ixOCRModelOf(st), ixMLTestPicture()); err != nil {
			a.ixMLTaskNoter("text")(false, err.Error())
			writeJSONError(w, http.StatusBadGateway, "text reading: "+err.Error())
			return
		}
		a.ixMLTaskNoter("text")(true, "")
	}
	a.ixMLNote(true, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": a.ixMLStatusNow(ctx)})
}
