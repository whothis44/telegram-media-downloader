package app

import (
	"context"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Lone files that look like they belong with an album.
//
// A file posted on its own (an original with one file, in no set) is
// compared by image recognition with everything else. Its closest files
// vote for the album (or set) they are in: each vote counts by how much
// closer than "loose" the file is, at most 4 votes per album, and the
// album's total grows when it was posted in the same chat, within 3 days,
// or shows the same person. An album is suggested only when its total
// clears a bar and beats the runner-up by a margin (as PhotoPrism does for
// faces), so a file that looks a bit like many albums is left alone. The
// suggestion goes to the Review tab ("Does this file belong with this
// album?"); yes ties the two posts into one set, no is remembered.
//
// One more check keeps a file from the creator's other shoots out: its
// closest file in the album must be about as close as the album's own
// files are to each other (within FitSlack of their median).
//
// The bar, margin and slack are starting values, chosen by hiding one file
// of each album in invented archives of self-filmed shoots and checking how
// often the matcher puts it back where it came from (ix_lone_test.go, run
// with IX_CALIBRATE=1 for the whole grid). At these values over 99% of its
// suggestions were right, 78-92% of hidden files were put back (fewer with
// poorer pictures), and about 2% of files that truly stand alone were
// offered an album. They are worth checking on a real archive.

// ixVecIndex is what matching needs from the embedding store.
type ixVecIndex interface {
	vector(key string) []float32
	nearest(q []float32, k int, keep func(string) bool, skip string) []ixEmbHit
	sim(a, b string) (float32, bool)
}

type ixLoneParams struct {
	K        int     // closest files looked at
	PerAlbum int     // votes counted per album at most
	Min      float64 // the winner's total must reach this
	Margin   float64 // and beat the runner-up by this much
	SameChat float64 // bonus share: posted in the same chat
	NearDate float64 // bonus share: posted within Days of each other
	Person   float64 // bonus share: the same person is in both
	Days     int64
	FitSlack float64 // the file must be about as close to the album as its own files are to each other (<0: not checked)
}

var ixLoneDefaults = ixLoneParams{K: 12, PerAlbum: 4, Min: 0.6, Margin: 0.3, SameChat: 0.25, NearDate: 0.15, Person: 0.5, Days: 3, FitSlack: 0.06}

// ixLoneIndex holds the lookups matching needs, built once per model.
type ixLoneIndex struct {
	m       *ixModel
	fams    [][]int32          // cluster -> albums holding it
	persons map[string][]int64 // file key -> people (not hidden) seen in it
	floor   float32
}

func newIxLoneIndex(m *ixModel) *ixLoneIndex {
	x := &ixLoneIndex{m: m, fams: make([][]int32, len(m.Clusters)), persons: map[string][]int64{}, floor: m.LooseSim}
	if x.floor <= 0 || x.floor >= 1 {
		x.floor = ixSetLooseSim
	}
	for fi := range m.Families {
		if !x.album(int32(fi)) {
			continue
		}
		seen := map[int32]bool{}
		for _, pi := range m.Families[fi].Posts {
			for _, ii := range m.Posts[pi].Items {
				if c := m.Items[ii].Cluster; !seen[c] {
					seen[c] = true
					x.fams[c] = append(x.fams[c], int32(fi))
				}
			}
		}
	}
	for id, keys := range m.PersonKeys {
		if m.PersonHidden[id] {
			continue
		}
		for k := range keys {
			x.persons[k] = append(x.persons[k], id)
		}
	}
	return x
}

// album: a family a lone file can be suggested for (two or more files, or
// in a set).
func (x *ixLoneIndex) album(fi int32) bool {
	return len(x.m.Families[fi].Items) >= 2 || int(fi) < len(x.m.FamSet) && x.m.FamSet[fi] >= 0
}

// group names what a vote is for: the set a family is in, or the family.
func (x *ixLoneIndex) group(fi int32) int64 {
	if int(fi) < len(x.m.FamSet) && x.m.FamSet[fi] >= 0 {
		return 1<<32 | int64(x.m.FamSet[fi])
	}
	return int64(fi)
}

// lone reports whether a family is a lone file that can be matched, and
// its file.
func (x *ixLoneIndex) lone(fi int32) (int32, bool) {
	f := &x.m.Families[fi]
	if len(f.Items) != 1 || int(fi) < len(x.m.FamSet) && x.m.FamSet[fi] >= 0 {
		return -1, false
	}
	it := &x.m.Items[f.Items[0]]
	if it.Key == "" || it.Type != "photo" && it.Type != "video" {
		return -1, false
	}
	return f.Items[0], true
}

type ixLoneVote struct {
	Group    int64
	Family   int32 // family of its closest file: the post to tie to
	Score    float64
	Hits     int
	Best     float32
	SameChat bool
	NearDate bool
	Person   bool
	Fit      float32 // how alike the album's own files are (median of each one's closest other; 0: unknown)
}

// ixLoneFrom is where a file to match was posted.
type ixLoneFrom struct {
	Key   string
	Own   int32 // its cluster: never a vote
	Chats map[string]bool
	Date  int64
}

// vote ranks the albums a file looks like, best first.
func (x *ixLoneIndex) vote(idx ixVecIndex, from ixLoneFrom, p ixLoneParams) []ixLoneVote {
	m := x.m
	v := idx.vector(from.Key)
	if v == nil {
		return nil
	}
	hits := idx.nearest(v, 2*p.K, func(k string) bool {
		ii, ok := m.ByKey[k]
		return ok && m.Items[ii].Cluster != from.Own && len(x.fams[m.Items[ii].Cluster]) > 0
	}, from.Key)
	mine := x.persons[from.Key]
	votes := map[int64]*ixLoneVote{}
	seen := map[int32]bool{}
	n := 0
	for _, h := range hits {
		ii := m.ByKey[h.Key]
		c := m.Items[ii].Cluster
		if seen[c] {
			continue // an older copy's vector
		}
		seen[c] = true
		if n++; n > p.K {
			break
		}
		if h.Sim < x.floor {
			continue
		}
		strength := float64(h.Sim-x.floor) / float64(1-x.floor)
		person := false
		for _, a := range mine {
			for _, b := range x.persons[h.Key] {
				person = person || a == b
			}
		}
		chat, near := false, false
		for _, ci := range m.Clusters[c].Items {
			it := &m.Items[ci]
			chat = chat || from.Chats[it.Group]
			d := m.Posts[it.Post].Date - from.Date
			near = near || from.Date > 0 && d <= p.Days*86400 && d >= -p.Days*86400
		}
		counted := map[int64]bool{}
		for _, fi := range x.fams[c] {
			g := x.group(fi)
			if counted[g] {
				continue
			}
			counted[g] = true
			vt := votes[g]
			if vt == nil {
				vt = &ixLoneVote{Group: g, Family: fi, Best: h.Sim}
				votes[g] = vt
			}
			if vt.Hits >= p.PerAlbum {
				continue
			}
			vt.Hits++
			vt.Score += strength
			vt.SameChat = vt.SameChat || chat
			vt.NearDate = vt.NearDate || near
			vt.Person = vt.Person || person
		}
	}
	out := make([]ixLoneVote, 0, len(votes))
	for _, vt := range votes {
		bonus := 1.0
		if vt.SameChat {
			bonus += p.SameChat
		}
		if vt.NearDate {
			bonus += p.NearDate
		}
		if vt.Person {
			bonus += p.Person
		}
		vt.Score *= bonus
		out = append(out, *vt)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Group < out[j].Group
	})
	if len(out) > 0 {
		out[0].Fit = x.fit(idx, out[0].Family, from.Own)
	}
	return out
}

// fit: how alike an album's own files are, the median of each file's
// closest other file (leaving out one cluster, the file being matched).
func (x *ixLoneIndex) fit(idx ixVecIndex, fi int32, skip int32) float32 {
	var keys []string
	for _, ii := range x.m.Families[fi].Items {
		if it := &x.m.Items[ii]; it.Cluster != skip && it.Key != "" && len(keys) < 12 {
			keys = append(keys, it.Key)
		}
	}
	var best []float32
	for i, a := range keys {
		b, ok := float32(-2), false
		for j, k := range keys {
			if i != j {
				if v, has := idx.sim(a, k); has && v > b {
					b, ok = v, true
				}
			}
		}
		if ok {
			best = append(best, b)
		}
	}
	if len(best) < 2 {
		return 0
	}
	sort.Slice(best, func(i, j int) bool { return best[i] < best[j] })
	return best[len(best)/2]
}

// ixLonePick: the album to suggest, if one clearly wins.
func ixLonePick(votes []ixLoneVote, p ixLoneParams) (ixLoneVote, float64, bool) {
	if len(votes) == 0 {
		return ixLoneVote{}, 0, false
	}
	win, second := votes[0], 0.0
	if len(votes) > 1 {
		second = votes[1].Score
	}
	margin := win.Score - second
	fits := p.FitSlack < 0 || win.Fit == 0 || float64(win.Best) >= float64(win.Fit)-p.FitSlack
	return win, margin, win.Hits >= 2 && win.Score >= p.Min && margin >= p.Margin && fits
}

// ixLoneWhy says, in plain words, why an album was suggested.
func ixLoneWhy(v ixLoneVote) string {
	parts := []string{"looks like " + strconv.Itoa(v.Hits) + " of its files"}
	if v.SameChat {
		parts = append(parts, "same chat")
	}
	if v.NearDate {
		parts = append(parts, "posted within 3 days")
	}
	if v.Person {
		parts = append(parts, "same person")
	}
	return strings.Join(parts, ", ")
}

// ixLoneFromFamily describes a lone family's file for matching.
func (x *ixLoneIndex) fromFamily(fi int32, ii int32) ixLoneFrom {
	m := x.m
	f := &m.Families[fi]
	from := ixLoneFrom{Key: m.Items[ii].Key, Own: m.Items[ii].Cluster, Chats: map[string]bool{}, Date: m.Posts[f.Lead].Date}
	for _, pi := range f.Posts {
		from.Chats[m.Posts[pi].Group] = true
	}
	return from
}

// ---------- background matching ----------

var ixLoneRunning sync.Map // *App -> bool

// ixLoneRun matches lone files not matched yet (or not since the store grew
// by a fifth), for up to budget, and stores what it finds.
func (a *App) ixLoneRun(ctx context.Context, m *ixModel, budget time.Duration) {
	if m == nil {
		return
	}
	if _, busy := ixLoneRunning.LoadOrStore(a, true); busy {
		return
	}
	defer ixLoneRunning.Delete(a)
	if a.ixEmbSim() == nil {
		return // image recognition off, or nothing looked at yet
	}
	store := &a.ixMLBox().store
	store.mu.RLock()
	model := store.model
	store.mu.RUnlock()
	size := store.size()
	type seenRow struct {
		model string
		size  int
	}
	seen := map[string]seenRow{}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT key, model, size FROM ix_set_sug_seen`); err == nil {
		for rows.Next() {
			var k string
			var r seenRow
			if rows.Scan(&k, &r.model, &r.size) == nil {
				seen[k] = r
			}
		}
		rows.Close()
	}
	x := newIxLoneIndex(m)
	type job struct {
		fam  int32
		item int32
	}
	var jobs []job
	for fi := range m.Families {
		ii, ok := x.lone(int32(fi))
		if !ok || !store.has(m.Items[ii].Key) {
			continue
		}
		if r, ok := seen[m.Items[ii].Key]; ok && r.model == model && size*5 <= r.size*6 {
			continue
		}
		jobs = append(jobs, job{int32(fi), ii})
	}
	if len(jobs) == 0 {
		return
	}
	type result struct {
		job
		vote   ixLoneVote
		margin float64
		ok     bool
	}
	deadline := time.Now().Add(budget)
	results := make([]result, len(jobs))
	done := make([]bool, len(jobs))
	var next int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < max(1, runtime.NumCPU()/2); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil && time.Now().Before(deadline) {
				mu.Lock()
				i := next
				next++
				mu.Unlock()
				if i >= len(jobs) {
					return
				}
				j := jobs[i]
				v, margin, ok := ixLonePick(x.vote(store, x.fromFamily(j.fam, j.item), ixLoneDefaults), ixLoneDefaults)
				results[i] = result{j, v, margin, ok}
				done[i] = true
			}
		}()
	}
	wg.Wait()
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	now := ixNowMs()
	for i, r := range results {
		if !done[i] {
			continue
		}
		post := ixPostRef(&m.Posts[m.Families[r.fam].Lead])
		key := m.Items[r.item].Key
		if r.ok {
			other := ixPostRef(&m.Posts[m.Families[r.vote.Family].Lead])
			_, err = tx.ExecContext(ctx, `INSERT INTO ix_set_sug(post,key,other,score,margin,why,decision,found_at) VALUES(?,?,?,?,?,?,'',?)
				ON CONFLICT(post) DO UPDATE SET key=excluded.key, other=excluded.other, score=excluded.score, margin=excluded.margin, why=excluded.why, found_at=excluded.found_at,
					decision=CASE WHEN ix_set_sug.other<>excluded.other THEN '' ELSE ix_set_sug.decision END`, post, key, other, r.vote.Score, r.margin, ixLoneWhy(r.vote), now)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM ix_set_sug WHERE post=? AND decision=''`, post)
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO ix_set_sug_seen(key,model,size) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET model=excluded.model, size=excluded.size`, key, model, size)
		}
		if err != nil {
			return
		}
	}
	_ = tx.Commit()
}

// ---------- review ----------

type ixLoneJ struct {
	Post       string  `json:"post"`
	Other      string  `json:"other"`
	File       ixJItem `json:"file"`
	Where      string  `json:"where"`
	Date       int64   `json:"date"`
	Album      ixJCard `json:"album"`
	Why        string  `json:"why"`
	Score      float64 `json:"score"`
	AlbumFiles int     `json:"albumFiles"`
}

// ixLonePending lists the suggestions still to review that hold in the
// current model, best first.
func (a *App) ixLonePending(ctx context.Context, m *ixModel, limit int) []ixLoneJ {
	out := []ixLoneJ{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT post, other, why, score FROM ix_set_sug WHERE decision='' ORDER BY score DESC, post LIMIT 2000`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() && len(out) < limit {
		var s ixLoneJ
		if rows.Scan(&s.Post, &s.Other, &s.Why, &s.Score) != nil {
			continue
		}
		pi, ok := m.ByMsg[s.Post]
		oi, ok2 := m.ByMsg[s.Other]
		if !ok || !ok2 {
			continue
		}
		fa, fb := m.Posts[pi].Family, m.Posts[oi].Family
		if fa < 0 || fb < 0 || fa == fb || len(m.Families[fa].Items) != 1 {
			continue // already merged, or no longer alone
		}
		if sa, sb := m.FamSet[fa], m.FamSet[fb]; sa >= 0 && sa == sb {
			continue // already in that set
		}
		s.File = m.jitem(m.Families[fa].Items[0])
		s.Where, s.Date = m.GroupName[m.Posts[pi].Group], m.Posts[pi].Date
		s.Album = m.familyCard(fb)
		s.AlbumFiles = len(m.Families[fb].Items)
		out = append(out, s)
	}
	return out
}

func (a *App) handleIxLoneReview(w http.ResponseWriter, r *http.Request) {
	m, err := a.ixModelNow(r.Context())
	if m == nil {
		writeJSONError(w, http.StatusInternalServerError, errString(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lone": a.ixLonePending(r.Context(), m, 50)})
}

// ixLoneDecide: yes puts the lone file's post in the album's set (the same
// tie as "Put in the same set as…"); no is remembered.
func (a *App) ixLoneDecide(ctx context.Context, post, decision string) error {
	var other string
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT other FROM ix_set_sug WHERE post=?`, post).Scan(&other); err != nil {
		return errIxBadRef
	}
	switch decision {
	case "yes":
		if err := a.ixSetFixPost(ctx, post, "tie", other); err != nil {
			return err
		}
	case "no":
	default:
		return errIxBadMode
	}
	_, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_set_sug SET decision=?, at=? WHERE post=?`, decision, ixNowMs(), post)
	if err == nil {
		a.ixOrgLog("lone_decided", map[string]any{"post": post, "album": other, "decision": decision})
	}
	return err
}

func (a *App) handleIxLoneDecide(w http.ResponseWriter, r *http.Request) {
	b := ixBody(r)
	if err := a.ixLoneDecide(r.Context(), toString(b["post"]), toString(b["decision"])); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.ixModelSoon()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
