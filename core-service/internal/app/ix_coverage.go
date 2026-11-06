package app

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// What each group has, counted so that overlap does not inflate anything.
//
// A group's "files" are its distinct files (a file it posted twice counts
// once), each in the first of these that applies:
//
//	saved     in the library, and this group's message brought it
//	shared    in the library through another group's message: the same
//	          Telegram file, or the same bytes confirmed by every part's hash
//	probable  evidence only: a held copy, a look-alike of a saved file, or an
//	          exact re-upload checked on sampled parts (those can differ
//	          between the samples), so never counted as saved
//	needed    nothing yet
//
// Global totals count a file once, however many groups have it, and what a
// group gets from others is reported per group as coverage only. Files and
// bytes are separate figures: small pictures can make a group look nearly
// complete by count while its videos are missing. All of it is known only as
// far as the rescans have read: Scanned says so.

type ixCoverCount struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

func (c *ixCoverCount) add(size int64) {
	c.Files++
	c.Bytes += size
}

type ixGroupCoverage struct {
	GroupID  string       `json:"groupId"`
	Name     string       `json:"name"`
	Rank     int          `json:"rank"`  // 0: not ranked
	Place    int          `json:"place"` // goes before higher places (spread reorders them); 0: not ranked
	Scanned  bool         `json:"scanned"`
	Total    ixCoverCount `json:"total"`
	Saved    ixCoverCount `json:"saved"`
	Shared   ixCoverCount `json:"shared"`
	Probable ixCoverCount `json:"probable"`
	Needed   ixCoverCount `json:"needed"`
	// Of Needed, what a group going before this one needs too: it comes free
	// once that group has it.
	NeededAhead ixCoverCount `json:"neededAhead"`
	Left        int          `json:"left"`  // in the download queue, ready or downloading
	Apart       int          `json:"apart"` // in the queue, set aside for now
	Waiting     string       `json:"waiting"`
	// NotQueued: files are missing and nothing will queue them until the group
	// is in the rescan (the page offers to add it, ix.12.3).
	NotQueued bool `json:"notQueued"`
}

const ixNotQueued = "files missing but not queued: add it to the rescan"

type ixPrioritiesInfo struct {
	Policy   string            `json:"policy"`
	Known    bool              `json:"known"` // false until the browser index is first built
	Groups   []ixGroupCoverage `json:"groups"`
	Focus    string            `json:"focus"` // the group going first now
	Distinct struct {
		Saved    ixCoverCount `json:"saved"`    // each file once
		Probable ixCoverCount `json:"probable"` // each file once
		Needed   ixCoverCount `json:"needed"`   // each file once
	} `json:"distinct"`
	GroupRefs int     `json:"groupRefs"` // files counted per group, saved or shared: not more content, only more credit
	Rate      float64 `json:"rate"`      // bytes a second the estimates go by; 0: not known
	RateDay   float64 `json:"rateDay"`
	Rate3h    float64 `json:"rate3h"`
	Until     int64   `json:"until,omitempty"` // when the account is deleted (unix seconds)
}

// ixSubstConfirmed: an exact re-upload whose every part hash matched, rather
// than sampled regions (ixSubstPlan: photos and small files compare every part
// hash; videos from 2 MB compare the start, middle and end, or three samples).
func ixSubstConfirmed(mediaType string, size int64, how string) bool {
	if how != "telegram-hashes" {
		return false
	}
	return mediaType != "video" && mediaType != "gif" || size < ixSubstSampleFrom
}

const ixSubstSampleFrom = 2 << 20

// ixCoverageByGroup counts every group. known is false before the browser index
// is first built.
func (a *App) ixCoverageByGroup(ctx context.Context) (map[string]ixGroupCoverage, bool) {
	m := a.ixModelLast()
	if m == nil {
		return nil, false
	}
	saved, err := a.ixSavedIdentities(ctx)
	if err != nil {
		return nil, false
	}
	substHow := map[string]string{}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, message_id, how FROM ix_subst`); err == nil {
		for rows.Next() {
			var g, how string
			var msg int
			if rows.Scan(&g, &msg, &how) == nil {
				substHow[fmt.Sprintf("%s:%d", g, msg)] = how
			}
		}
		rows.Close()
	}
	return ixCoverage(m, saved, substHow), true
}

// ixCoverage is the count over the browser's model; saved is the library's
// Telegram files, substHow the check behind each exact re-upload.
func ixCoverage(m *ixModel, saved map[string]bool, substHow map[string]string) map[string]ixGroupCoverage {
	const (
		kindNeeded = iota
		kindProbable
		kindShared
		kindSaved
	)
	// The group whose download first holds each saved file.
	owner := map[string]string{}
	first := map[string]int64{}
	for i := range m.Items {
		it := &m.Items[i]
		if it.Key == "" || it.ID == 0 || it.Linked {
			continue
		}
		if at, ok := first[it.Key]; !ok || it.ID < at {
			first[it.Key], owner[it.Key] = it.ID, it.Group
		}
	}
	type entry struct {
		kind int
		size int64
	}
	byGroup := map[string]map[int32]entry{}
	for i := range m.Items {
		it := &m.Items[i]
		if int(it.Cluster) < 0 || int(it.Cluster) >= len(m.Clusters) {
			continue
		}
		kind := kindNeeded
		switch {
		case it.Status == "subst":
			if ixSubstConfirmed(it.Type, it.Size, substHow[fmt.Sprintf("%s:%d", it.Group, it.Msg)]) {
				kind = kindShared
			} else {
				kind = kindProbable
			}
		case it.Key != "":
			if o, ok := owner[it.Key]; ok && o == it.Group {
				kind = kindSaved
			} else {
				kind = kindShared
			}
		case it.Status == "held":
			kind = kindProbable
		default:
			// Not downloaded. Deleted on Telegram: it cannot be fetched, so it
			// is not something this group still needs.
			if int(it.Post) >= 0 && int(it.Post) < len(m.Posts) && m.Posts[it.Post].Deleted {
				continue
			}
			if id := ixIdentityOfMediaKey(it.Ident); id != "" && saved[ixIdentityFile(id)] {
				kind = kindShared
				break
			}
			for _, ii := range m.Clusters[it.Cluster].Items {
				if m.Items[ii].Key != "" {
					kind = kindProbable // a look-alike is saved: not the same as having it
					break
				}
			}
		}
		g := byGroup[it.Group]
		if g == nil {
			g = map[int32]entry{}
			byGroup[it.Group] = g
		}
		if e, ok := g[it.Cluster]; !ok || kind > e.kind {
			g[it.Cluster] = entry{kind, max(it.Size, e.size)}
		} else if it.Size > e.size {
			e.size = it.Size
			g[it.Cluster] = e
		}
	}
	out := make(map[string]ixGroupCoverage, len(byGroup))
	for group, files := range byGroup {
		c := ixGroupCoverage{GroupID: group}
		for _, e := range files {
			c.Total.add(e.size)
			switch e.kind {
			case kindSaved:
				c.Saved.add(e.size)
			case kindShared:
				c.Shared.add(e.size)
			case kindProbable:
				c.Probable.add(e.size)
			default:
				c.Needed.add(e.size)
			}
		}
		out[group] = c
	}
	return out
}

// ixNeededAhead fills NeededAhead for groups in the order they go: a needed
// file an earlier group needs too is not extra work for the later one.
func ixNeededAhead(m *ixModel, groups []ixGroupCoverage, saved map[string]bool) {
	if m != nil {
		ixApplyNeededAhead(ixNeedsByGroup(m, saved), groups)
	}
}

// ixNeedsByGroup is each group's needed files, by cluster.
func ixNeedsByGroup(m *ixModel, saved map[string]bool) map[string]map[int32]int64 {
	needs := map[string]map[int32]int64{}
	for i := range m.Items {
		it := &m.Items[i]
		if it.Key != "" || it.Status != "missing" || int(it.Cluster) < 0 || int(it.Cluster) >= len(m.Clusters) {
			continue
		}
		if int(it.Post) >= 0 && int(it.Post) < len(m.Posts) && m.Posts[it.Post].Deleted {
			continue
		}
		if id := ixIdentityOfMediaKey(it.Ident); id != "" && saved[ixIdentityFile(id)] {
			continue
		}
		g := needs[it.Group]
		if g == nil {
			g = map[int32]int64{}
			needs[it.Group] = g
		}
		g[it.Cluster] = max(g[it.Cluster], it.Size)
	}
	return needs
}

func ixApplyNeededAhead(needs map[string]map[int32]int64, groups []ixGroupCoverage) {
	ahead := map[int32]bool{}
	for i := range groups {
		for cl, size := range needs[groups[i].GroupID] {
			if ahead[cl] {
				groups[i].NeededAhead.add(size)
			}
			ahead[cl] = true
		}
	}
}

// ixCoverBase is the slow part of the report: counts over every file the archive
// knows, which do not depend on the ranks. It is kept for ixStatusCacheTTL, so
// that a change of the ranks answers at once and shows at once.
type ixCoverBase struct {
	cov      map[string]ixGroupCoverage
	known    bool
	needs    map[string]map[int32]int64
	distinct ixPrioritiesInfo // its Distinct
	estimate ixDeadlineInfo
}

func (a *App) ixCoverBaseNow(ctx context.Context) ixCoverBase {
	return a.ixCaches().coverBase.get(ixStatusCacheTTL, func() (ixCoverBase, bool) {
		var b ixCoverBase
		b.cov, b.known = a.ixCoverageByGroup(ctx)
		b.estimate = a.ixDeadlineEstimate(ctx, false)
		if m := a.ixModelLast(); m != nil {
			if saved, err := a.ixSavedIdentities(ctx); err == nil {
				b.needs = ixNeedsByGroup(m, saved)
				ixDistinctTotals(m, saved, &b.distinct)
			}
		}
		return b, ctx.Err() == nil
	})
}

// ixPrioritiesNow is the report.
func (a *App) ixPrioritiesNow(ctx context.Context) ixPrioritiesInfo {
	return a.ixPrioritiesMeasure(ctx)
}

func (a *App) ixPrioritiesMeasure(ctx context.Context) ixPrioritiesInfo {
	info := ixPrioritiesInfo{Policy: a.ixLoadSettings(ctx).GroupPolicy, Groups: []ixGroupCoverage{}}
	if info.Policy == "" {
		info.Policy = ixGroupPolicyOrder
	}
	ranks, _ := a.ixRanks(ctx)
	rank := map[string]ixRankedGroup{}
	places := append([]ixRankedGroup(nil), ranks...)
	for _, g := range ranks {
		rank[g.GroupID] = g
	}
	sortRankedByBase(places)
	place := map[string]int{}
	for i, g := range places {
		place[g.GroupID] = i + 1
	}
	base := a.ixCoverBaseNow(ctx)
	cov := base.cov
	info.Known = base.known
	left := a.ixQueueLeft(ctx)
	q, _ := a.ixLoadQueue(ctx)
	scanned := map[string]bool{}
	for id, at := range a.ixLatestWalks(ctx) {
		scanned[id] = at > 0
	}
	queueState := map[string]ixQueueItem{}
	for _, it := range q.Items {
		queueState[it.GroupID] = it
		if it.State == "done" {
			scanned[it.GroupID] = true
		} else {
			scanned[it.GroupID] = false
		}
	}
	names := map[string]string{}
	if metas, err := a.ixGroups(ctx); err == nil {
		for _, g := range metas {
			names[g.ID] = g.Name
		}
	}
	ids := map[string]bool{}
	for id := range cov {
		ids[id] = true
	}
	for _, g := range ranks {
		ids[g.GroupID] = true
	}
	for _, it := range q.Items {
		ids[it.GroupID] = true
	}
	for id := range ids {
		c := cov[id]
		c.GroupID = id
		c.Name = names[id]
		if c.Name == "" {
			c.Name = rank[id].Name
		}
		if c.Name == "" {
			c.Name = "Chat " + id
		}
		c.Rank, c.Place, c.Scanned = rank[id].Rank, place[id], scanned[id]
		c.Left, c.Apart = left[id].Left, left[id].Apart+left[id].Held
		info.Groups = append(info.Groups, c)
	}
	ixSortCoverage(info.Groups)
	ixApplyNeededAhead(base.needs, info.Groups)
	info.Distinct = base.distinct.Distinct
	focus := ""
	for i := range info.Groups {
		c := &info.Groups[i]
		c.Waiting = ixCoverageWaiting(c, queueState[c.GroupID], focus)
		c.NotQueued = c.Waiting == ixNotQueued
		if focus == "" && c.Place > 0 && (c.Left > 0 || c.Needed.Files > 0) {
			focus = c.Name
			info.Focus = focus
		}
		info.GroupRefs += c.Saved.Files + c.Shared.Files
	}
	d := base.estimate
	info.Rate, info.RateDay, info.Rate3h, info.Until = d.Rate, d.Rate24h, d.Rate3h, d.Until
	return info
}

func sortRankedByBase(g []ixRankedGroup) {
	sort.SliceStable(g, func(i, j int) bool { return g[i].Base < g[j].Base })
}

// ixSortCoverage: ranked groups in the order they go, then the others, largest
// need first.
func ixSortCoverage(g []ixGroupCoverage) {
	sort.SliceStable(g, func(i, j int) bool {
		a, b := &g[i], &g[j]
		switch {
		case (a.Place > 0) != (b.Place > 0):
			return a.Place > 0
		case a.Place != b.Place:
			return a.Place < b.Place
		case a.Needed.Bytes != b.Needed.Bytes:
			return a.Needed.Bytes > b.Needed.Bytes
		}
		return a.Name < b.Name
	})
}

// ixCoverageWaiting says in a few words why a group stands where it stands.
func ixCoverageWaiting(c *ixGroupCoverage, q ixQueueItem, focus string) string {
	if c.Needed.Files == 0 && c.Left == 0 && c.Apart == 0 {
		switch {
		case !c.Scanned && q.GroupID != "":
			return "nothing missing so far; messages still being read"
		case !c.Scanned:
			return "never rescanned: what it holds is not known"
		case c.Probable.Files > 0:
			return "nothing left but probable copies"
		}
		return "complete"
	}
	switch {
	case q.State == "running" && q.Phase == ixPhaseFiles && c.Left == 0:
		return "queueing its files"
	case q.State == "running" && q.Phase != ixPhaseFiles:
		return "reading its messages"
	case q.State == "waiting" && q.Phase == ixPhaseFiles && c.Left == 0:
		return "its files are not queued yet: waiting for the files lane"
	case q.State == "waiting" && c.Left == 0:
		return "waiting for the messages lane"
	case c.Left == 0 && c.Apart > 0:
		return "only files held back or set aside are queued"
	case c.Left == 0:
		return ixNotQueued
	case focus == "" && c.Place > 0:
		return "going first"
	case c.Place > 0:
		return "waiting for groups ranked above it"
	}
	return "not ranked: goes after every ranked group"
}

// ixDistinctTotals counts each file once across all groups.
func ixDistinctTotals(m *ixModel, saved map[string]bool, info *ixPrioritiesInfo) {
	for ci := range m.Clusters {
		c := &m.Clusters[ci]
		var size int64
		have, held, need := false, false, false
		for _, ii := range c.Items {
			it := &m.Items[ii]
			size = max(size, it.Size)
			switch {
			case it.Key != "":
				have = true
			case it.Status == "held":
				held = true
			default:
				if int(it.Post) >= 0 && int(it.Post) < len(m.Posts) && m.Posts[it.Post].Deleted {
					continue
				}
				if id := ixIdentityOfMediaKey(it.Ident); id != "" && saved[ixIdentityFile(id)] {
					have = true
				} else {
					need = true
				}
			}
		}
		switch {
		case have:
			info.Distinct.Saved.add(size)
		case held:
			info.Distinct.Probable.add(size)
		case need:
			info.Distinct.Needed.add(size)
		}
	}
}

// ---------- API ----------

func (a *App) handleIxPriorities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.ixPrioritiesNow(r.Context()))
}

// handleIxPrioritiesSet takes the whole ranked list (order), the policy, or
// both.
func (a *App) handleIxPrioritiesSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Order  *[]string         `json:"order"`
		Names  map[string]string `json:"names"`
		Policy *string           `json:"policy"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		return
	}
	ctx := r.Context()
	if body.Policy != nil {
		if *body.Policy != ixGroupPolicyOrder && *body.Policy != ixGroupPolicySpread {
			writeJSONError(w, http.StatusBadRequest, "policy: order or spread")
			return
		}
		st := a.ixLoadSettings(ctx)
		st.GroupPolicy = *body.Policy
		if err := a.ixSaveSettings(ctx, st); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		a.ixRankChanged()
	}
	if body.Order != nil {
		if len(*body.Order) > ixRankMax {
			writeJSONError(w, http.StatusBadRequest, "too many ranked groups")
			return
		}
		if err := a.ixSaveRanks(ctx, *body.Order, body.Names); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// The queue's stars follow the ranked list. A rescan step can hold the
		// queue for a while: nobody waits for that.
		on := map[string]bool{}
		for _, id := range *body.Order {
			on[strings.TrimSpace(id)] = true
		}
		a.ambientWG.Add(1)
		go func() {
			defer a.ambientWG.Done()
			_, _ = a.ixQueueEdit(a.ctx, func(st *ixQueueState) error {
				for i := range st.Items {
					st.Items[i].Priority = on[st.Items[i].GroupID]
				}
				return nil
			})
		}()
		a.ixRankChanged()
	}
	// The page has already moved the rows; it reads the figures separately.
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// ixPrioritiesDiag is the report's lines, with groups as placeholders.
func (a *App) ixPrioritiesDiag(ctx context.Context, group func(string) string) []string {
	info := a.ixPrioritiesMeasure(ctx)
	mb := func(c ixCoverCount) string { return fmt.Sprintf("%d / %d MB", c.Files, c.Bytes>>20) }
	var out []string
	ranked := 0
	for _, c := range info.Groups {
		if c.Place > 0 {
			ranked++
		}
	}
	out = append(out, fmt.Sprintf("policy %s · ranked groups %d · rows taking no transfer of their own %d · each file once: saved %s, probable %s, needed %s · per-group credit %d files",
		info.Policy, ranked, a.ixSharedWaiting(), mb(info.Distinct.Saved), mb(info.Distinct.Probable), mb(info.Distinct.Needed), info.GroupRefs))
	shown := 0
	for _, c := range info.Groups {
		if c.Place == 0 {
			continue
		}
		if shown++; shown > 40 {
			out = append(out, fmt.Sprintf("  ... and %d more", ranked-40))
			break
		}
		out = append(out, fmt.Sprintf("  place %d (rank %d) %s: read %v · saved %s · through others %s · probable %s · needed %s (of it, needed by a group ahead %s) · queue %d ready, %d apart · %s",
			c.Place, c.Rank, group(c.GroupID), c.Scanned, mb(c.Saved), mb(c.Shared), mb(c.Probable), mb(c.Needed), mb(c.NeededAhead), c.Left, c.Apart, c.Waiting))
	}
	return out
}
