package app

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed ixassets/browse.html
var ixBrowseHTML []byte

func registerIxBrowseRoutes(mux *http.ServeMux, a *App) {
	h := func(f http.HandlerFunc) http.Handler { return a.requireAdmin(f) }
	mux.Handle("GET /ix/browse", h(a.handleIxBrowsePage))
	mux.Handle("GET /api/ix/browse/info", h(a.handleIxBrowseInfo))
	mux.Handle("GET /api/ix/browse/feed", h(a.handleIxBrowseFeed))
	mux.Handle("GET /api/ix/browse/family", h(a.handleIxBrowseFamily))
	mux.Handle("GET /api/ix/browse/item", h(a.handleIxBrowseItem))
	mux.Handle("GET /api/ix/browse/chat", h(a.handleIxBrowseChat))
	mux.Handle("GET /api/ix/browse/review", h(a.handleIxBrowseReview))
	mux.Handle("POST /api/ix/browse/review", h(a.handleIxBrowseDecide))
	mux.Handle("POST /api/ix/browse/keeper", h(a.handleIxBrowseKeeper))
	mux.Handle("GET /api/ix/browse/collections", h(a.handleIxCollections))
	mux.Handle("POST /api/ix/browse/collections", h(a.handleIxCollectionCreate))
	mux.Handle("POST /api/ix/browse/collections/{id}", h(a.handleIxCollectionChange))
	mux.Handle("GET /api/ix/browse/entities", h(a.handleIxBrowseEntities))
	mux.Handle("GET /api/ix/browse/entity", h(a.handleIxBrowseEntity))
	mux.Handle("POST /api/ix/browse/rebuild", h(a.handleIxBrowseRebuild))
	mux.Handle("GET /api/ix/thumb", h(a.handleIxThumb))
	mux.Handle("GET /api/ix/preview", h(a.handleIxPreview))
	mux.Handle("GET /api/ix/moves", h(a.handleIxMoves))
	mux.Handle("GET /api/ix/moves/{id}", h(a.handleIxMove))
	mux.Handle("POST /api/ix/moves/plan", h(a.handleIxMovePlan))
	mux.Handle("POST /api/ix/moves/{id}/{action}", h(a.handleIxMoveAction))
	mux.Handle("POST /api/ix/held/release", h(a.handleIxHeldRelease))
	mux.Handle("POST /api/ix/subst/fetch", h(a.handleIxSubstFetch))
	mux.Handle("POST /api/ix/freeze/check", h(a.handleIxFreezeCheck))
	mux.Handle("GET /api/ix/diagnostics", h(a.handleIxDiagnostics))
	registerIxSetRoutes(mux, a)
	registerIxAlbumRoutes(mux, a)
	mux.Handle("GET /api/ix/browse/people", h(a.handleIxPeople))
	mux.Handle("GET /api/ix/browse/person", h(a.handleIxPerson))
	mux.Handle("POST /api/ix/browse/person/{id}", h(a.handleIxPersonChange))
	mux.Handle("GET /api/ix/face", h(a.handleIxFace))
	mux.Handle("GET /api/ix/browse/extra", h(a.handleIxExtra))
	mux.Handle("GET /api/ix/browse/things", h(a.handleIxThings))
	mux.Handle("POST /api/ix/stt/check", h(a.handleIxSTTCheck))
	mux.Handle("GET /api/ix/deadline", h(a.handleIxDeadline))
}

func (a *App) handleIxBrowsePage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(ixBrowseHTML)
}

// ---------- JSON shapes ----------

type ixJItem struct {
	ID       int64    `json:"id,omitempty"`
	Key      string   `json:"key,omitempty"`
	Type     string   `json:"type"`
	W        int      `json:"w,omitempty"`
	H        int      `json:"h,omitempty"`
	Dur      float64  `json:"dur,omitempty"`
	Size     int64    `json:"size,omitempty"`
	Status   string   `json:"status"`
	Group    string   `json:"group"`
	Msg      int      `json:"msg"`
	Name     string   `json:"name,omitempty"`
	Thumb    string   `json:"thumb"`
	Src      string   `json:"src,omitempty"`
	Copies   int      `json:"copies,omitempty"`   // files in its duplicate set
	New      bool     `json:"new,omitempty"`      // in a merged album: brought by a later post
	Like     *ixJItem `json:"like,omitempty"`     // held: the saved copy it probably is
	Role     string   `json:"role,omitempty"`     // in a media set: core, member, loose, elsewhere
	Sim      float32  `json:"sim,omitempty"`      // likeness (image recognition), 0 when not compared
	AtMs     int64    `json:"atMs,omitempty"`     // a video found by one of its frames: where (ms)
	Fixed    bool     `json:"fixed,omitempty"`    // a person placed it
	OtherSet int32    `json:"otherSet,omitempty"` // elsewhere: that set's ID + 1
	// Not downloaded, but Telegram's preview is kept (Thumb).
	PreviewOnly bool `json:"previewOnly,omitempty"`
	Inferred    bool `json:"inferred,omitempty"` // a lone file placed in the album beside it
}

type ixJPost struct {
	ID        int32     `json:"id"`
	Group     string    `json:"group"`
	GroupName string    `json:"groupName"`
	Msg       int       `json:"msg"`
	Msgs      []int     `json:"msgs,omitempty"`
	Date      int64     `json:"date"`
	Origin    int64     `json:"origin,omitempty"`
	Fwd       string    `json:"fwd,omitempty"`
	Caption   string    `json:"caption,omitempty"`
	Deleted   bool      `json:"deleted,omitempty"`
	Role      string    `json:"role"`
	Family    int32     `json:"family"`
	FamPosts  int       `json:"famPosts"`
	Items     []ixJItem `json:"items"`
}

type ixJCard struct {
	Kind      string         `json:"kind"` // family, post, media
	ID        int64          `json:"id"`
	Date      int64          `json:"date"`
	Group     string         `json:"group,omitempty"`
	GroupName string         `json:"groupName,omitempty"`
	Msg       int            `json:"msg,omitempty"`
	Caption   string         `json:"caption,omitempty"`
	Items     []ixJItem      `json:"items"`
	More      int            `json:"more,omitempty"`
	Posts     int            `json:"posts,omitempty"` // posts in its family
	Chats     int            `json:"chats,omitempty"`
	Merged    bool           `json:"merged,omitempty"`
	Role      string         `json:"role,omitempty"`
	Family    int32          `json:"family"`
	Deleted   bool           `json:"deleted,omitempty"`
	Families  int            `json:"families,omitempty"` // a media set's families
	Roles     map[string]int `json:"roles,omitempty"`
	Why       []string       `json:"why,omitempty"`  // a search: where it matched (caption, text, speech, name, looks)
	Weak      bool           `json:"weak,omitempty"` // a search: only a weak likeness, shown as nothing closer stands out
}

func ixPathURL(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "/files/" + strings.Join(parts, "/") + "?inline=1"
}

func (m *ixModel) jitem(ii int32) ixJItem {
	it := &m.Items[ii]
	j := ixJItem{ID: it.ID, Key: it.Key, Type: it.Type, W: it.W, H: it.H, Dur: it.Dur, Size: it.Size, Status: it.Status, Group: it.Group, Msg: it.Msg, Name: it.Name, Inferred: it.Placed}
	switch {
	case it.Thumb && it.Key != "":
		j.Thumb = "/api/ix/thumb?key=" + url.QueryEscape(it.Key)
	case it.ID > 0 && (it.Type == "photo" || it.Type == "video"):
		j.Thumb = "/api/thumbs/" + strconv.FormatInt(it.ID, 10)
	default:
		j.Thumb = "/api/ix/preview?group=" + url.QueryEscape(it.Group) + "&msg=" + strconv.Itoa(it.Msg)
		if u := ixCopyPreviewURL(it.Ident); u != "" && it.Key == "" {
			j.Thumb, j.PreviewOnly = u, true // Telegram's 320 px preview, kept (ix_copies_ml.go)
		}
	}
	if it.Path != "" {
		j.Src = ixPathURL(it.Path)
	}
	if c := int(it.Cluster); c >= 0 && c < len(m.Clusters) {
		j.Copies = len(m.Clusters[c].Items)
		if it.Status == "held" {
			if canon := m.Clusters[c].Canon; canon >= 0 && canon != ii && m.Items[canon].Key != "" {
				like := m.jitem(canon)
				like.Like = nil
				j.Like = &like
			}
		}
	}
	return j
}

func (m *ixModel) jpost(pi int32) ixJPost {
	p := &m.Posts[pi]
	j := ixJPost{ID: pi, Group: p.Group, GroupName: m.GroupName[p.Group], Msg: p.Msg, Msgs: p.Msgs, Date: p.Date, Fwd: p.Fwd, Caption: p.Caption, Deleted: p.Deleted, Role: p.Role, Family: p.Family}
	if p.Origin != p.Date {
		j.Origin = p.Origin
	}
	if p.Family >= 0 {
		j.FamPosts = len(m.Families[p.Family].Posts)
	}
	for _, ii := range p.Items {
		j.Items = append(j.Items, m.jitem(ii))
	}
	return j
}

const ixCardItems = 12

func (m *ixModel) familyCard(fi int32) ixJCard {
	f := &m.Families[fi]
	lead := &m.Posts[f.Lead]
	c := ixJCard{Kind: "family", ID: int64(fi), Date: f.First, Group: lead.Group, GroupName: m.GroupName[lead.Group], Msg: lead.Msg, Caption: lead.Caption,
		Posts: len(f.Posts), Chats: f.Chats, Merged: f.Merged, Family: fi, Deleted: lead.Deleted}
	for k, ii := range f.Items {
		if k >= ixCardItems {
			c.More = len(f.Items) - ixCardItems
			break
		}
		j := m.jitem(ii)
		j.New = f.Intro[k] != f.Lead
		c.Items = append(c.Items, j)
	}
	return c
}

func (m *ixModel) postCard(pi int32) ixJCard {
	p := &m.Posts[pi]
	c := ixJCard{Kind: "post", ID: int64(pi), Date: p.Date, Group: p.Group, GroupName: m.GroupName[p.Group], Msg: p.Msg, Caption: p.Caption, Role: p.Role, Family: p.Family, Deleted: p.Deleted}
	if p.Family >= 0 {
		c.Posts = len(m.Families[p.Family].Posts)
	}
	for k, ii := range p.Items {
		if k >= ixCardItems {
			c.More = len(p.Items) - ixCardItems
			break
		}
		c.Items = append(c.Items, m.jitem(ii))
	}
	return c
}

func (m *ixModel) mediaCard(ii int32) ixJCard {
	it := &m.Items[ii]
	p := &m.Posts[it.Post]
	c := ixJCard{Kind: "media", ID: int64(ii), Date: p.Date, Group: it.Group, GroupName: m.GroupName[it.Group], Msg: it.Msg, Caption: p.Caption, Family: p.Family, Role: p.Role}
	if p.Family >= 0 {
		c.Posts = len(m.Families[p.Family].Posts)
	}
	c.Items = []ixJItem{m.jitem(ii)}
	return c
}

// ---------- handlers ----------

func (a *App) handleIxBrowseInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := a.ixModelNow(ctx)
	if err != nil && m == nil {
		writeJSONError(w, http.StatusInternalServerError, "browser index: "+err.Error())
		return
	}
	box := a.ixModelBox()
	box.mu.Lock()
	building, lastErr := box.building, box.err
	box.mu.Unlock()
	type group struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Category string `json:"category"`
		Posts    int    `json:"posts"`
		Items    int    `json:"items"`
	}
	counts := map[string]*group{}
	for _, id := range m.Groups {
		counts[id] = &group{ID: id, Name: m.GroupName[id], Category: m.GroupCat[id]}
	}
	for i := range m.Posts {
		g := counts[m.Posts[i].Group]
		if g == nil {
			g = &group{ID: m.Posts[i].Group, Name: "Chat " + m.Posts[i].Group, Category: ixUncategorised}
			counts[g.ID] = g
		}
		g.Posts++
		g.Items += len(m.Posts[i].Items)
	}
	groups := []*group{}
	cats := map[string]bool{}
	for _, g := range counts {
		groups = append(groups, g)
		cats[g.Category] = true
	}
	sort.Slice(groups, func(i, j int) bool { return strings.ToLower(groups[i].Name) < strings.ToLower(groups[j].Name) })
	var dupPosts, merged, generic, held, missing int
	for i := range m.Families {
		if len(m.Families[i].Posts) > 1 {
			dupPosts += len(m.Families[i].Posts) - 1
		}
		if m.Families[i].Merged {
			merged++
		}
	}
	for i := range m.Clusters {
		if m.Clusters[i].Generic {
			generic++
		}
	}
	for i := range m.Items {
		switch m.Items[i].Status {
		case "held":
			held++
		case "missing":
			missing++
		}
	}
	colls, _ := a.ixCollections(ctx)
	var review int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_msug s WHERE NOT EXISTS (SELECT 1 FROM ix_org_pairs o WHERE o.key_a=s.key_a AND o.key_b=s.key_b)`).Scan(&review)
	writeJSON(w, http.StatusOK, map[string]any{
		"built": m.Built.UnixMilli(), "tookMs": m.Took.Milliseconds(), "building": building, "error": lastErr,
		"posts": len(m.Posts), "families": len(m.Families), "items": len(m.Items), "clusters": len(m.Clusters),
		"repostPosts": dupPosts, "mergedAlbums": merged, "generic": generic, "held": held, "missing": missing,
		"groups": groups, "categories": ixSortedKeys(cats), "collections": colls, "review": review,
		"media": a.ixMediaStatus(), "videos": a.ixVdupStatus(), "sets": len(m.Sets), "ml": a.ixMLStatusNow(ctx),
	})
}

type ixFeedQuery struct {
	View       string
	Group      string
	Category   string
	Type       string
	Sort       string
	Q          string
	Dups       bool
	Status     string
	Collection int64
	Person     []int64 // all of them in one post (person=1,7)
	Unique     bool    // only posts with a file no other chat has
	From, To   int64
	Cursor     int
	Limit      int
	Match      string           // a search: words, looks or both
	Rank       bool             // a search: best match first
	people     []map[int32]bool // each person's duplicate sets (ixPeopleSets)
}

func ixParseFeed(r *http.Request) ixFeedQuery {
	q := r.URL.Query()
	f := ixFeedQuery{View: q.Get("view"), Group: q.Get("group"), Category: q.Get("category"), Type: q.Get("type"), Sort: q.Get("sort"), Q: strings.TrimSpace(q.Get("q")),
		Dups: q.Get("dups") == "1", Status: q.Get("status"), Match: q.Get("match"), Rank: q.Get("rank") == "relevance", Unique: q.Get("unique") == "1"}
	f.Collection, _ = strconv.ParseInt(q.Get("collection"), 10, 64)
	for _, v := range strings.Split(q.Get("person"), ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && id > 0 && len(f.Person) < 4 {
			f.Person = append(f.Person, id)
		}
	}
	f.From, _ = strconv.ParseInt(q.Get("from"), 10, 64)
	f.To, _ = strconv.ParseInt(q.Get("to"), 10, 64)
	f.Cursor, _ = strconv.Atoi(q.Get("cursor"))
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 60
	}
	if f.View != "posts" && f.View != "media" && f.View != "sets" {
		f.View = "families"
	}
	return f
}

func (m *ixModel) postMatches(pi int32, f ixFeedQuery, hits map[int32]bool) bool {
	p := &m.Posts[pi]
	if f.Group != "" && p.Group != f.Group {
		return false
	}
	if f.Category != "" && m.GroupCat[p.Group] != f.Category {
		return false
	}
	if f.From > 0 && p.Date < f.From || f.To > 0 && p.Date > f.To {
		return false
	}
	if hits != nil && !hits[pi] {
		return false
	}
	if len(f.Person) > 0 && !m.postHasPeople(pi, f) {
		return false
	}
	if f.Dups && (p.Family < 0 || len(m.Families[p.Family].Posts) < 2) {
		return false
	}
	if f.Unique && !m.ixPostOnlyHere(pi) {
		return false
	}
	if f.Type == "album" && len(p.Items) < 2 {
		return false
	}
	if f.Type == "" || f.Type == "album" {
		if f.Status == "" {
			return true
		}
	}
	// One file must be of the kind and in the state asked for.
	for _, ii := range p.Items {
		if ixItemMatches(&m.Items[ii], f) {
			return true
		}
	}
	return false
}

// ixItemMatches: a file of the kind (photo, video, other) and state asked for.
func ixItemMatches(it *ixMItem, f ixFeedQuery) bool {
	switch f.Type {
	case "photo", "video":
		if it.Type != f.Type {
			return false
		}
	case "other":
		if it.Type == "photo" || it.Type == "video" {
			return false
		}
	}
	return f.Status == "" || it.Status == f.Status
}

func (a *App) handleIxBrowseFeed(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := a.ixModelNow(ctx)
	if m == nil {
		writeJSONError(w, http.StatusInternalServerError, "browser index: "+errString(err))
		return
	}
	f := ixParseFeed(r)
	var rank *ixRank
	if f.Q != "" {
		rank = a.ixHybridSearch(ctx, m, f) // words and looks, fused (ix_search_rank.go)
	}
	a.ixWriteFeed(ctx, w, m, f, rank)
}

// ixWriteFeed answers a page of the feed: the posts that match f (and the
// search, when rank is set), in the view asked for.
func (a *App) ixWriteFeed(ctx context.Context, w http.ResponseWriter, m *ixModel, f ixFeedQuery, rank *ixRank) {
	var hits map[int32]bool
	if rank != nil {
		hits = rank.hits()
	}
	if f.Collection > 0 {
		refs, err := a.ixCollectionRefs(ctx, f.Collection)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		in := map[int32]bool{}
		for _, ref := range refs {
			switch {
			case strings.HasPrefix(ref, "post:"):
				parts := strings.SplitN(ref, ":", 3)
				if pi, ok := m.ByMsg[parts[1]+":"+parts[2]]; ok && (hits == nil || hits[pi]) {
					in[pi] = true
				}
			case strings.HasPrefix(ref, "file:"):
				if ii, ok := m.ByKey[strings.TrimPrefix(ref, "file:")]; ok && (hits == nil || hits[m.Items[ii].Post]) {
					in[m.Items[ii].Post] = true
				}
			}
		}
		hits = in
	}
	f.people = m.ixPeopleSets(f.Person)
	cards, total, next := m.ixFeedPage(f, hits, rank)
	out := map[string]any{"cards": cards, "total": total, "next": next, "built": m.Built.UnixMilli()}
	if rank != nil {
		out["search"] = rank.info
	}
	writeJSON(w, http.StatusOK, out)
}

// ixFeedPage lists the matching cards of a view, one page from f.Cursor:
// by date, or best match first when rank says so.
func (m *ixModel) ixFeedPage(f ixFeedQuery, hits map[int32]bool, rank *ixRank) (cards []ixJCard, total, next int) {
	newest := f.Sort != "old"
	byScore := rank.ordered()
	page := func(n int, card func(int) ixJCard) {
		total = n
		for i := f.Cursor; i < n && len(cards) < f.Limit; i++ {
			cards = append(cards, card(i))
		}
	}
	switch f.View {
	case "sets":
		list := m.ixSetFeed(f, hits)
		if byScore {
			score := map[int32]float64{}
			for _, si := range list {
				for _, fi := range m.Sets[si].Families {
					for _, pi := range m.Families[fi].Posts {
						if s, ok := rank.post[pi]; ok && s > score[si] && m.postMatches(pi, f, hits) {
							score[si] = s
						}
					}
				}
			}
			sort.SliceStable(list, func(i, j int) bool { return score[list[i]] > score[list[j]] })
		}
		page(len(list), func(i int) ixJCard {
			c := m.setCard(list[i])
			var posts []int32
			for _, fi := range m.Sets[list[i]].Families {
				posts = append(posts, m.Families[fi].Posts...)
			}
			rank.decorate(m, &c, posts, nil)
			return c
		})
	case "posts":
		var list []int32
		for pi := range m.Posts {
			if m.postMatches(int32(pi), f, hits) {
				list = append(list, int32(pi))
			}
		}
		sort.SliceStable(list, func(i, j int) bool {
			if byScore {
				if x, y := rank.post[list[i]], rank.post[list[j]]; x != y {
					return x > y
				}
			}
			a, b := &m.Posts[list[i]], &m.Posts[list[j]]
			if a.Date != b.Date {
				return a.Date > b.Date == newest
			}
			return (list[i] > list[j]) == newest
		})
		page(len(list), func(i int) ixJCard {
			c := m.postCard(list[i])
			rank.decorate(m, &c, list[i:i+1], nil)
			return c
		})
	case "media":
		// One tile per duplicate set (its best copy), placed where it first appeared.
		type tile struct {
			item int32
			date int64
		}
		seen := map[int32]bool{}
		var list []tile
		for pi := range m.Posts {
			if !m.postMatches(int32(pi), f, hits) {
				continue
			}
			for _, ii := range m.Posts[pi].Items {
				it := &m.Items[ii]
				if !ixItemMatches(it, f) {
					continue
				}
				c := it.Cluster
				if seen[c] || len(f.Person) > 0 && !m.setHasAnyone(c, f) {
					continue
				}
				if rank != nil {
					if _, ok := rank.cluster[c]; !ok {
						continue // a search: only the files found
					}
				}
				seen[c] = true
				show := m.Clusters[c].Canon
				if f.Status != "" {
					show = ii // the copy in the state asked for, not the best one
				}
				list = append(list, tile{show, m.Posts[pi].Origin})
			}
		}
		sort.SliceStable(list, func(i, j int) bool {
			if byScore {
				x, y := rank.cluster[m.Items[list[i].item].Cluster], rank.cluster[m.Items[list[j].item].Cluster]
				if x != y {
					return x > y
				}
			}
			if list[i].date != list[j].date {
				return list[i].date > list[j].date == newest
			}
			return (list[i].item > list[j].item) == newest
		})
		page(len(list), func(i int) ixJCard {
			c := m.mediaCard(list[i].item)
			rank.decorate(m, &c, nil, []int32{m.Items[list[i].item].Cluster})
			return c
		})
	default:
		// Originals only: one card per family that has a matching post.
		seen := map[int32]bool{}
		score := map[int32]float64{}
		var list []int32
		for pi := range m.Posts {
			fam := m.Posts[pi].Family
			if fam < 0 || seen[fam] && !byScore || !m.postMatches(int32(pi), f, hits) {
				continue
			}
			if byScore {
				score[fam] = max(score[fam], rank.post[int32(pi)])
			}
			if !seen[fam] {
				seen[fam] = true
				list = append(list, fam)
			}
		}
		sort.SliceStable(list, func(i, j int) bool {
			if x, y := score[list[i]], score[list[j]]; x != y {
				return x > y
			}
			a, b := &m.Families[list[i]], &m.Families[list[j]]
			if a.First != b.First {
				return a.First > b.First == newest
			}
			return (list[i] > list[j]) == newest
		})
		page(len(list), func(i int) ixJCard {
			c := m.familyCard(list[i])
			rank.decorate(m, &c, m.Families[list[i]].Posts, nil)
			return c
		})
	}
	next = f.Cursor + len(cards)
	if next >= total {
		next = 0
	}
	if cards == nil {
		cards = []ixJCard{}
	}
	return cards, total, next
}

func errString(err error) string {
	if err == nil {
		return "not ready"
	}
	return err.Error()
}

func (a *App) handleIxBrowseFamily(w http.ResponseWriter, r *http.Request) {
	m, err := a.ixModelNow(r.Context())
	if m == nil {
		writeJSONError(w, http.StatusInternalServerError, errString(err))
		return
	}
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil || id < 0 || id >= len(m.Families) {
		writeJSONError(w, http.StatusNotFound, "no such family")
		return
	}
	f := &m.Families[id]
	card := m.familyCard(int32(id))
	card.Items, card.More = nil, 0
	for k, ii := range f.Items {
		j := m.jitem(ii)
		j.New = f.Intro[k] != f.Lead
		card.Items = append(card.Items, j)
	}
	posts := []ixJPost{}
	for _, pi := range f.Posts {
		posts = append(posts, m.jpost(pi))
	}
	intro := make([]int32, len(f.Intro))
	copy(intro, f.Intro)
	writeJSON(w, http.StatusOK, map[string]any{"family": card, "posts": posts, "intro": intro})
}

// handleIxBrowseItem lists every copy of a file (its duplicate set) and the
// videos it shares footage with.
func (a *App) handleIxBrowseItem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := a.ixModelNow(ctx)
	if m == nil {
		writeJSONError(w, http.StatusInternalServerError, errString(err))
		return
	}
	q := r.URL.Query()
	ii := int32(-1)
	if id, err := strconv.ParseInt(q.Get("id"), 10, 64); err == nil && id > 0 {
		if v, ok := m.ByID[id]; ok {
			ii = v
		}
	}
	if ii < 0 {
		if v, ok := m.ByKey[q.Get("key")]; ok {
			ii = v
		}
	}
	if ii < 0 {
		if pi, ok := m.ByMsg[q.Get("group")+":"+q.Get("msg")]; ok {
			for _, x := range m.Posts[pi].Items {
				if strconv.Itoa(m.Items[x].Msg) == q.Get("msg") {
					ii = x
				}
			}
		}
	}
	if ii < 0 {
		writeJSONError(w, http.StatusNotFound, "no such file")
		return
	}
	it := &m.Items[ii]
	c := &m.Clusters[it.Cluster]
	type copyJ struct {
		ixJItem
		GroupName string `json:"groupName"`
		Date      int64  `json:"date"`
		Canon     bool   `json:"canon,omitempty"`
		Family    int32  `json:"family"`
		Role      string `json:"role"`
	}
	var copies []copyJ
	for _, x := range c.Items {
		p := &m.Posts[m.Items[x].Post]
		copies = append(copies, copyJ{ixJItem: m.jitem(x), GroupName: m.GroupName[m.Items[x].Group], Date: p.Date, Canon: x == c.Canon, Family: p.Family, Role: p.Role})
	}
	sort.SliceStable(copies, func(i, j int) bool { return copies[i].Date < copies[j].Date })
	type related struct {
		Kind   string   `json:"kind"`
		Score  float64  `json:"score"`
		Offset float64  `json:"offsetS"`
		Item   *ixJItem `json:"item,omitempty"`
		Cover  float64  `json:"coverThis"`
		CoverO float64  `json:"coverOther"`
	}
	var rel []related
	if it.Key != "" {
		if rows, err := a.db.Reader.QueryContext(ctx, `SELECT key_a, key_b, kind, score, offset_s, cover_a, cover_b FROM ix_vdup WHERE (key_a=? OR key_b=?) AND kind<>'same'`, it.Key, it.Key); err == nil {
			for rows.Next() {
				var ka, kb, kind string
				var score, off, ca, cb float64
				if rows.Scan(&ka, &kb, &kind, &score, &off, &ca, &cb) != nil {
					continue
				}
				other, mine, theirs := kb, ca, cb
				if kb == it.Key {
					other, mine, theirs, off = ka, cb, ca, -off
				}
				x := related{Kind: kind, Score: score, Offset: off, Cover: mine, CoverO: theirs}
				if oi, ok := m.ByKey[other]; ok {
					j := m.jitem(m.Clusters[m.Items[oi].Cluster].Canon)
					x.Item = &j
				}
				rel = append(rel, x)
			}
			rows.Close()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": m.jitem(ii), "post": m.jpost(it.Post), "copies": copies, "generic": c.Generic, "related": rel})
}

// handleIxBrowseChat reads a chat as it was: messages in order, with their
// files, replies, forwards, edits, deletions, notices and comments.
func (a *App) handleIxBrowseChat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, _ := a.ixModelNow(ctx)
	q := r.URL.Query()
	group := q.Get("group")
	if group == "" {
		writeJSONError(w, http.StatusBadRequest, "group is required")
		return
	}
	limit := 80
	where, args, order := "group_id=?", []any{group}, "message_id DESC"
	if before, err := strconv.Atoi(q.Get("before")); err == nil && before > 0 {
		where += " AND message_id<?"
		args = append(args, before)
	} else if after, err := strconv.Atoi(q.Get("after")); err == nil && after > 0 {
		where += " AND message_id>?"
		args = append(args, after)
		order = "message_id ASC"
	} else if at, err := strconv.ParseInt(q.Get("date"), 10, 64); err == nil && at > 0 {
		where += " AND date>=?"
		args = append(args, at)
		order = "message_id ASC"
	}
	if topic, err := strconv.Atoi(q.Get("topic")); err == nil && topic > 0 {
		if topic == 1 {
			where += " AND topic_id IN (0,1)"
		} else {
			where += " AND topic_id=?"
			args = append(args, topic)
		}
	}
	args = append(args, limit)
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT message_id, date, edit_date, text, grouped_id, sender_id, post_author, fwd_from, fwd_date, reply_to, topic_id, media_type, media_name, deleted_at, service, comments, comment_on, views
		FROM ix_messages WHERE `+where+` ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type msgJ struct {
		ID        int       `json:"id"`
		Date      int64     `json:"date"`
		EditDate  int64     `json:"editDate,omitempty"`
		Text      string    `json:"text,omitempty"`
		Grouped   string    `json:"grouped,omitempty"`
		Sender    string    `json:"sender,omitempty"`
		Author    string    `json:"author,omitempty"`
		Fwd       string    `json:"fwd,omitempty"`
		FwdDate   int64     `json:"fwdDate,omitempty"`
		Reply     int       `json:"reply,omitempty"`
		Topic     int       `json:"topic,omitempty"`
		MediaType string    `json:"mediaType,omitempty"`
		MediaName string    `json:"mediaName,omitempty"`
		Deleted   int64     `json:"deleted,omitempty"`
		Service   string    `json:"service,omitempty"`
		Comments  int       `json:"comments,omitempty"`
		CommentOn string    `json:"commentOn,omitempty"`
		Views     int       `json:"views,omitempty"`
		Edits     int       `json:"edits,omitempty"`
		Items     []ixJItem `json:"items,omitempty"`
		Family    int32     `json:"family"`
		FamPosts  int       `json:"famPosts,omitempty"`
		Role      string    `json:"role,omitempty"`
	}
	var msgs []msgJ
	for rows.Next() {
		var x msgJ
		if rows.Scan(&x.ID, &x.Date, &x.EditDate, &x.Text, &x.Grouped, &x.Sender, &x.Author, &x.Fwd, &x.FwdDate, &x.Reply, &x.Topic, &x.MediaType, &x.MediaName, &x.Deleted, &x.Service, &x.Comments, &x.CommentOn, &x.Views) != nil {
			continue
		}
		x.Family = -1
		msgs = append(msgs, x)
	}
	rows.Close()
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
	names := map[string]string{}
	senders := map[string]bool{}
	for i := range msgs {
		x := &msgs[i]
		var n int
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_message_edits WHERE group_id=? AND message_id=?`, group, x.ID).Scan(&n)
		x.Edits = n
		for _, s := range []string{x.Sender, x.Fwd} {
			if s != "" {
				senders[s] = true
			}
		}
		if m == nil {
			continue
		}
		if pi, ok := m.ByMsg[group+":"+strconv.Itoa(x.ID)]; ok {
			p := &m.Posts[pi]
			x.Family, x.Role = p.Family, p.Role
			if p.Family >= 0 {
				x.FamPosts = len(m.Families[p.Family].Posts)
			}
			for _, ii := range p.Items {
				if m.Items[ii].Msg == x.ID {
					x.Items = append(x.Items, m.jitem(ii))
				}
			}
		}
	}
	for s := range senders {
		var name string
		if a.db.Reader.QueryRowContext(ctx, `SELECT name FROM ix_peers WHERE peer_id=?`, s).Scan(&name) == nil && name != "" {
			names[s] = name
		}
	}
	topics := map[string]string{}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT topic_id, title FROM ix_topics WHERE group_id=?`, group); err == nil {
		for rows.Next() {
			var id int
			var t string
			if rows.Scan(&id, &t) == nil {
				topics[strconv.Itoa(id)] = t
			}
		}
		rows.Close()
	}
	var info struct {
		Title    string `json:"title"`
		Kind     string `json:"kind"`
		About    string `json:"about"`
		Username string `json:"username"`
		Invite   string `json:"invite"`
		Members  int64  `json:"members"`
		Linked   string `json:"linked"`
	}
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT title, kind, about, username, invite_link, members, linked_group FROM ix_group_info WHERE group_id=?`, group).
		Scan(&info.Title, &info.Kind, &info.About, &info.Username, &info.Invite, &info.Members, &info.Linked)
	if msgs == nil {
		msgs = []msgJ{}
	}
	name := ""
	if m != nil {
		name = m.GroupName[group]
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": group, "name": name, "messages": msgs, "names": names, "topics": topics, "info": info})
}

func (a *App) handleIxBrowseReview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m, err := a.ixModelNow(ctx)
	if m == nil {
		writeJSONError(w, http.StatusInternalServerError, errString(err))
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT s.key_a, s.key_b, s.kind, s.distance FROM ix_msug s
		WHERE NOT EXISTS (SELECT 1 FROM ix_org_pairs o WHERE o.key_a=s.key_a AND o.key_b=s.key_b) ORDER BY CASE s.kind WHEN 'photo' THEN 0 WHEN 'video' THEN 1 ELSE 2 END, s.distance, s.found_at LIMIT 50 OFFSET ?`, max(0, offset))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	type pairJ struct {
		A        ixJItem `json:"a"`
		B        ixJItem `json:"b"`
		Kind     string  `json:"kind"`
		Distance float64 `json:"distance"`
		AWhere   string  `json:"aWhere"`
		BWhere   string  `json:"bWhere"`
		ADate    int64   `json:"aDate"`
		BDate    int64   `json:"bDate"`
	}
	pairs := []pairJ{}
	for rows.Next() {
		var ka, kb, kind string
		var d float64
		if rows.Scan(&ka, &kb, &kind, &d) != nil {
			continue
		}
		ia, okA := m.ByKey[ka]
		ib, okB := m.ByKey[kb]
		if !okA || !okB {
			continue
		}
		pa, pb := &m.Posts[m.Items[ia].Post], &m.Posts[m.Items[ib].Post]
		pairs = append(pairs, pairJ{A: m.jitem(ia), B: m.jitem(ib), Kind: kind, Distance: d, AWhere: m.GroupName[pa.Group], BWhere: m.GroupName[pb.Group], ADate: pa.Date, BDate: pb.Date})
	}
	writeJSON(w, http.StatusOK, map[string]any{"pairs": pairs})
}

func ixBody(r *http.Request) map[string]any {
	body := map[string]any{}
	_ = json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(&body)
	return body
}

func (a *App) handleIxBrowseDecide(w http.ResponseWriter, r *http.Request) {
	b := ixBody(r)
	if err := a.ixDecide(r.Context(), toString(b["a"]), toString(b["b"]), toString(b["decision"])); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if keep := toString(b["keep"]); keep != "" {
		_ = a.ixSetKeeper(r.Context(), toString(b["a"]), keep)
	}
	a.ixModelSoon()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleIxBrowseKeeper(w http.ResponseWriter, r *http.Request) {
	b := ixBody(r)
	if err := a.ixSetKeeper(r.Context(), toString(b["cluster"]), toString(b["key"])); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.ixModelSoon()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ixModelSoon rebuilds the model in the background after a change a person
// made, so the next view shows it.
func (a *App) ixModelSoon() {
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 10*time.Minute)
		defer cancel()
		_, _ = a.ixRebuildModel(ctx)
	}()
}

func (a *App) handleIxCollections(w http.ResponseWriter, r *http.Request) {
	list, err := a.ixCollections(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"collections": list})
}

func (a *App) handleIxCollectionCreate(w http.ResponseWriter, r *http.Request) {
	b := ixBody(r)
	id, err := a.ixCreateCollection(r.Context(), toString(b["name"]))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if refs := ixStrings(b["refs"]); len(refs) > 0 {
		if err := a.ixCollect(r.Context(), id, refs, false); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func ixStrings(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (a *App) handleIxCollectionChange(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad collection")
		return
	}
	b := ixBody(r)
	switch toString(b["action"]) {
	case "add":
		err = a.ixCollect(r.Context(), id, ixStrings(b["refs"]), false)
	case "remove":
		err = a.ixCollect(r.Context(), id, ixStrings(b["refs"]), true)
	case "rename":
		err = a.ixRenameCollection(r.Context(), id, toString(b["name"]))
	case "delete":
		err = a.ixDeleteCollection(r.Context(), id)
	default:
		err = errors.New("action must be add, remove, rename or delete")
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleIxBrowseEntities(w http.ResponseWriter, r *http.Request) {
	list, err := a.ixEntities(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	q := r.URL.Query()
	kind, needle := q.Get("kind"), strings.ToLower(strings.TrimSpace(q.Get("q")))
	offset, _ := strconv.Atoi(q.Get("offset"))
	type row struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
		Count int    `json:"count"`
		Chats int    `json:"chats"`
		First int64  `json:"first"`
		Last  int64  `json:"last"`
	}
	out := []row{}
	n := 0
	for _, e := range list {
		if kind != "" && e.Kind != kind || needle != "" && !strings.Contains(strings.ToLower(e.Value), needle) {
			continue
		}
		n++
		if n > offset && len(out) < 200 {
			out = append(out, row{e.Kind, e.Value, e.Count, e.Chats, e.First, e.Last})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entities": out, "total": n, "kinds": ixEntityCounts(list)})
}

func (a *App) handleIxBrowseEntity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := a.ixEntities(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	q := r.URL.Query()
	var e *ixEntity
	for _, x := range list {
		if x.Kind == q.Get("kind") && x.Value == q.Get("value") {
			e = x
			break
		}
	}
	if e == nil {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	m, _ := a.ixModelNow(ctx)
	type hit struct {
		Group     string `json:"group"`
		GroupName string `json:"groupName"`
		Msg       int    `json:"msg"`
		Date      int64  `json:"date"`
		Text      string `json:"text"`
	}
	hits := []hit{}
	for _, ref := range e.Refs {
		var text string
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT substr(text,1,600) FROM ix_messages WHERE group_id=? AND message_id=?`, ref.Group, ref.Msg).Scan(&text)
		name := "Chat " + ref.Group
		if m != nil && m.GroupName[ref.Group] != "" {
			name = m.GroupName[ref.Group]
		}
		hits = append(hits, hit{ref.Group, name, ref.Msg, ref.Date, text})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entity": e, "hits": hits})
}

func (a *App) handleIxBrowseRebuild(w http.ResponseWriter, r *http.Request) {
	m, err := a.ixRebuildModel(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"built": m.Built.UnixMilli(), "tookMs": m.Took.Milliseconds()})
}

// handleIxPreview serves Telegram's own tiny preview of a message's file,
// for files not downloaded (yet).
func (a *App) handleIxPreview(w http.ResponseWriter, r *http.Request) {
	msg, err := strconv.ParseInt(r.URL.Query().Get("msg"), 10, 64)
	if err != nil {
		writeNotFound(w, r)
		return
	}
	facts, ok := a.ixMessageFacts(r.Context(), r.URL.Query().Get("group"), msg)
	data := ixInflateStripped(facts.Thumb)
	if !ok || data == nil {
		writeNotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

// ---------- moves ----------

func (a *App) handleIxMoves(w http.ResponseWriter, r *http.Request) {
	plans, err := a.ixPlans(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": plans})
}

func (a *App) handleIxMove(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	p, err := a.ixPlan(r.Context(), id, 2000)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "no such plan")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *App) handleIxMovePlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	b := ixBody(r)
	kind := toString(b["kind"])
	var ops []ixMoveOp
	var err error
	title := ""
	switch kind {
	case "duplicate-copies":
		ops, err = a.ixPlanDuplicateCopies(ctx)
		title = "Move extra copies to _trash (byte-identical files, and near-identical videos: the best copy stays)"
	case "collection", "originals":
		m, merr := a.ixModelNow(ctx)
		if m == nil {
			writeJSONError(w, http.StatusInternalServerError, errString(merr))
			return
		}
		var items []int32
		if kind == "collection" {
			id := int64(number(b["collection"], 0))
			refs, cerr := a.ixCollectionRefs(ctx, id)
			if cerr != nil {
				writeJSONError(w, http.StatusBadRequest, cerr.Error())
				return
			}
			for _, ref := range refs {
				switch {
				case strings.HasPrefix(ref, "post:"):
					parts := strings.SplitN(ref, ":", 3)
					if pi, ok := m.ByMsg[parts[1]+":"+parts[2]]; ok {
						for _, ii := range m.Posts[pi].Items {
							items = append(items, m.Clusters[m.Items[ii].Cluster].Canon)
						}
					}
				case strings.HasPrefix(ref, "file:"):
					if ii, ok := m.ByKey[strings.TrimPrefix(ref, "file:")]; ok {
						items = append(items, m.Clusters[m.Items[ii].Cluster].Canon)
					}
				}
			}
			title = "Copy a collection to " + toString(b["folder"])
		} else {
			cat, group := toString(b["category"]), toString(b["group"])
			for fi := range m.Families {
				lead := &m.Posts[m.Families[fi].Lead]
				if group != "" && lead.Group != group || cat != "" && m.GroupCat[lead.Group] != cat {
					continue
				}
				items = append(items, m.Families[fi].Items...)
			}
			title = "Copy the originals of " + strings.TrimSpace(cat+" "+m.GroupName[group]) + " to " + toString(b["folder"])
		}
		seen := map[int32]bool{}
		var uniq []int32
		for _, ii := range items {
			if !seen[ii] {
				seen[ii] = true
				uniq = append(uniq, ii)
			}
		}
		move := b["move"] == true
		if move {
			title = strings.Replace(title, "Copy", "Move", 1)
		}
		ops, err = a.ixPlanCopy(ctx, m, uniq, toString(b["folder"]), move)
	default:
		err = errors.New("kind must be duplicate-copies, collection or originals")
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(ops) == 0 {
		writeJSONError(w, http.StatusBadRequest, "nothing to do")
		return
	}
	id, err := a.ixSavePlan(ctx, kind, title, ops)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, _ := a.ixPlan(ctx, id, 2000)
	writeJSON(w, http.StatusOK, p)
}

func (a *App) handleIxMoveAction(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad plan")
		return
	}
	action := r.PathValue("action")
	if action != "apply" && action != "undo" && action != "discard" {
		writeJSONError(w, http.StatusBadRequest, "action must be apply, undo or discard")
		return
	}
	if action == "discard" {
		res, err := a.db.Writer.ExecContext(r.Context(), `UPDATE ix_move_plans SET state='discarded' WHERE id=? AND state='planned'`, id)
		if n, _ := res.RowsAffected(); err != nil || n == 0 {
			writeJSONError(w, http.StatusBadRequest, "only a plan not yet applied can be discarded")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	go func() {
		ctx := a.ctx
		var err error
		if action == "apply" {
			err = a.ixApplyPlan(ctx, id)
		} else {
			err = a.ixUndoPlan(ctx, id)
		}
		if err != nil {
			a.ixLog("file plan %d %s: %v", id, action, err)
		}
		a.ixModelSoon()
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

// ---------- held-back and substituted files, freeze ----------

func (a *App) handleIxHeldRelease(w http.ResponseWriter, r *http.Request) {
	if err := a.ixReleaseHeld(r.Context(), 0); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.ixModelSoon()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleIxSubstFetch(w http.ResponseWriter, r *http.Request) {
	n, groups, err := a.ixFetchSubstituted(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": n, "groups": groups})
}

func (a *App) handleIxFreezeCheck(w http.ResponseWriter, r *http.Request) {
	if err := a.ixCheckFreeze(r.Context()); err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.ixFreeze(r.Context()))
}
