package app

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// People made from faces: merging, "same person?" suggestions, and the
// user's decisions about them.
//
// One person often ends up as several (filters, lighting, webcams), so
// after grouping the people are compared by the middle of their faces (a
// quality-weighted mean embedding, as in Ente's centroid merge). The
// research notes give cosine distances (d = 1 - similarity): suggest a
// merge between maxDistance and maxDistance + 0.1, that is d 0.5-0.6 or
// similarity 0.4-0.5, and merge without asking only under about d 0.4.
// Here:
//   - similarity 0.4 up to ixFaceAutoMergeSim: "Same person?" is asked
//     (0.5-0.7 is also asked: closer than the research's band, but not
//     close enough to merge unasked);
//   - ixFaceAutoMergeSim (0.7, d 0.3) and above: merged by itself. Stricter
//     than the research's 0.6 because a wrong merge has no one-click undo,
//     while a missed one only costs one click on the suggestion.
//
// Never merged or suggested: two people seen together in one picture or
// frame, a pair a person said are different, hidden people. Never merged
// by itself: two people who both have a name.
const (
	ixFaceSuggestSim   = 0.4
	ixFaceAutoMergeSim = 0.7
	ixFaceSuggestMax   = 2000 // people compared (the ones in most files); pairs grow with the square
	ixFaceSuggestKeep  = 200  // suggestions kept
)

type ixPeopleBox struct {
	mu         sync.Mutex
	passStamp  string
	passAt     time.Time
	regrouping bool
	regroup    string // what grouping again is doing, for the People tab
}

var ixPeopleState sync.Map // *App -> *ixPeopleBox

func (a *App) ixPeopleBox() *ixPeopleBox {
	v, _ := ixPeopleState.LoadOrStore(a, &ixPeopleBox{})
	return v.(*ixPeopleBox)
}

// ixPersonResolve follows merges: a person merged into another is that
// other person now.
func (a *App) ixPersonResolve(ctx context.Context, person int64) int64 {
	for i := 0; i < 20; i++ {
		var into int64
		if a.db.Reader.QueryRowContext(ctx, `SELECT merged_into FROM ix_people WHERE id=?`, person).Scan(&into) != nil || into == 0 {
			break
		}
		person = into
	}
	return person
}

// ixFaceRejects lists the people a face was taken out of (as they are now).
func (a *App) ixFaceRejects(ctx context.Context, face int64) map[int64]bool {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT person FROM ix_face_reject WHERE face=?`, face)
	if err != nil {
		return nil
	}
	var ps []int64
	for rows.Next() {
		var p int64
		if rows.Scan(&p) == nil {
			ps = append(ps, p)
		}
	}
	rows.Close()
	if len(ps) == 0 {
		return nil
	}
	out := map[int64]bool{}
	for _, p := range ps {
		out[a.ixPersonResolve(ctx, p)] = true
	}
	return out
}

// ixMergePeopleLocked puts all of from's faces with into (the grouping lock
// is held). into keeps its name, or takes from's when it has none; the same
// for a chosen cover picture.
func (a *App) ixMergePeopleLocked(ctx context.Context, from, into int64) error {
	now := ixNowMs()
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE ix_faces SET person=? WHERE person=?`, []any{into, from}},
		{`UPDATE ix_people SET merged_into=?, updated_at=? WHERE id=?`, []any{into, now, from}},
		{`UPDATE ix_people SET name=(SELECT name FROM ix_people WHERE id=?), updated_at=? WHERE id=? AND name=''`, []any{from, now, into}},
		{`UPDATE ix_people SET cover=(SELECT cover FROM ix_people WHERE id=?) WHERE id=? AND cover=0`, []any{from, into}},
		{`DELETE FROM ix_person_pair WHERE state='ask' AND (a IN (?,?) OR b IN (?,?))`, []any{from, into, from, into}},
	} {
		if _, err := tx.ExecContext(ctx, q.sql, q.args...); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.ixMLBox().faces.repoint(from, into)
	return nil
}

// ixFacePeopleSoon runs the people pass when faces were grouped since the
// last one (at most every two minutes).
func (a *App) ixFacePeopleSoon(ctx context.Context) {
	pb := a.ixPeopleBox()
	var stamp string
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT (SELECT COUNT(*)||'/'||COALESCE(SUM(person),0)||'/'||COALESCE(MAX(id),0) FROM ix_faces WHERE person>0)||'/'||
		(SELECT COALESCE(MAX(updated_at),0) FROM ix_people)`).Scan(&stamp)
	pb.mu.Lock()
	due := stamp != pb.passStamp && time.Since(pb.passAt) > 2*time.Minute
	pb.mu.Unlock()
	if !due {
		return
	}
	if err := a.ixFacePeoplePass(ctx); err == nil {
		pb.mu.Lock()
		pb.passStamp, pb.passAt = stamp, time.Now()
		pb.mu.Unlock()
	}
}

type ixPersonMid struct {
	id    int64
	name  string
	mid   []float32       // quality-weighted mean of the faces, unit length
	shots map[int64]bool  // pictures and frames the person is seen in
	units map[string]bool // files
	faces []int32         // store indexes
}

// ixPersonMids works out the middle of each person's faces (people with no
// faces are left out). With the store's read lock held.
func (s *ixFaceStore) personMidsLocked(unit func(string) string) map[int64]*ixPersonMid {
	out := map[int64]*ixPersonMid{}
	d := s.dim
	for i, p := range s.person {
		if p <= 0 || s.norms[i] == 0 {
			continue
		}
		m := out[p]
		if m == nil {
			m = &ixPersonMid{id: p, mid: make([]float32, d), shots: map[int64]bool{}, units: map[string]bool{}}
			out[p] = m
		}
		w := float32(math.Max(0.05, float64(s.qual[i]))) / s.norms[i]
		for j, x := range s.vecs[i*d : (i+1)*d] {
			m.mid[j] += float32(x) * w
		}
		m.shots[s.shotLocked(int32(i))] = true
		m.units[unit(s.keys[s.key[i]])] = true
		m.faces = append(m.faces, int32(i))
	}
	for _, m := range out {
		var n float64
		for _, x := range m.mid {
			n += float64(x) * float64(x)
		}
		if n > 0 {
			inv := float32(1 / math.Sqrt(n))
			for j := range m.mid {
				m.mid[j] *= inv
			}
		}
	}
	return out
}

func ixSeenTogether(a, b map[int64]bool) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// ixFacePeoplePass compares people: very close ones are merged, close ones
// are suggested as "Same person?".
func (a *App) ixFacePeoplePass(ctx context.Context) error {
	box := a.ixMLBox()
	s := &box.faces
	if err := s.load(ctx, a.db.Reader, ixFaceModel); err != nil {
		return err
	}
	s.group.Lock()
	defer s.group.Unlock()
	type pinfo struct {
		name   string
		hidden bool
		into   int64
	}
	people := map[int64]pinfo{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT id, name, hidden, merged_into FROM ix_people`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var p pinfo
		if rows.Scan(&id, &p.name, &p.hidden, &p.into) == nil {
			people[id] = p
		}
	}
	rows.Close()
	resolve := func(id int64) int64 {
		for i := 0; i < 20 && people[id].into != 0; i++ {
			id = people[id].into
		}
		return id
	}
	no := a.ixPersonNoPairs(ctx, resolve)
	s.mu.RLock()
	mids := s.personMidsLocked(a.ixFaceUnitOf())
	covers := map[int64]int64{}
	for id, m := range mids {
		covers[id] = s.coverLocked(m)
	}
	s.mu.RUnlock()
	var list []*ixPersonMid
	for id, m := range mids {
		if p, ok := people[id]; ok && p.into == 0 && !p.hidden {
			m.name = p.name
			list = append(list, m)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if len(list[i].units) != len(list[j].units) {
			return len(list[i].units) > len(list[j].units)
		}
		return list[i].id < list[j].id
	})
	if len(list) > ixFaceSuggestMax {
		list = list[:ixFaceSuggestMax]
	}
	type pair struct {
		x, y *ixPersonMid
		sim  float32
	}
	var pairs []pair
	for i, x := range list {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for _, y := range list[i+1:] {
			sim := ixCosF(x.mid, y.mid)
			if sim < ixFaceSuggestSim || no[ixPairKey(x.id, y.id)] || ixSeenTogether(x.shots, y.shots) {
				continue
			}
			pairs = append(pairs, pair{x, y, sim})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].sim > pairs[j].sim })
	// Merge the very close ones, closest first; a person merged in this pass
	// is compared again next time, with their new faces.
	touched, gone := map[int64]bool{}, map[int64]bool{}
	var ask []pair
	for _, p := range pairs {
		auto := p.sim >= ixFaceAutoMergeSim && (p.x.name == "" || p.y.name == "")
		if !auto || touched[p.x.id] || touched[p.y.id] {
			if !gone[p.x.id] && !gone[p.y.id] && len(ask) < ixFaceSuggestKeep {
				ask = append(ask, p)
			}
			continue
		}
		from, into := p.y, p.x // into: the one with a name, else the one in more files
		if from.name != "" {
			from, into = into, from
		}
		if err := a.ixMergePeopleLocked(ctx, from.id, into.id); err != nil {
			return err
		}
		a.ixOrgLog("person", map[string]any{"person": from.id, "action": "merge", "into": into.id, "auto": true, "likeness": fmt.Sprintf("%.2f", p.sim)})
		touched[from.id], touched[into.id], gone[from.id] = true, true, true
	}
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM ix_person_pair WHERE state='ask'`); err != nil {
		return err
	}
	now := ixNowMs()
	for _, p := range ask {
		if gone[p.x.id] || gone[p.y.id] {
			continue
		}
		lo, hi := min(p.x.id, p.y.id), max(p.x.id, p.y.id)
		if _, err := tx.ExecContext(ctx, `INSERT INTO ix_person_pair(a,b,sim,state,at) VALUES(?,?,?,'ask',?) ON CONFLICT(a,b) DO NOTHING`, lo, hi, p.sim, now); err != nil {
			return err
		}
	}
	for id, face := range covers {
		if _, err := tx.ExecContext(ctx, `UPDATE ix_people SET auto_cover=? WHERE id=? AND auto_cover<>?`, face, id, face); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ixFaceCoverNear: the cover is the clearest of this many faces nearest
// the middle of a person's faces (the research notes' advice; Immich picks
// one at random).
const ixFaceCoverNear = 10

// coverLocked picks a person's cover face (store read lock held).
func (s *ixFaceStore) coverLocked(m *ixPersonMid) int64 {
	type near struct {
		i   int32
		sim float32
	}
	d := s.dim
	ns := make([]near, 0, len(m.faces))
	for _, i := range m.faces {
		var dot float32
		for j, x := range s.vecs[int(i)*d : int(i+1)*d] {
			dot += float32(x) * m.mid[j]
		}
		ns = append(ns, near{i, dot / s.norms[i]})
	}
	sort.Slice(ns, func(x, y int) bool { return ns[x].sim > ns[y].sim })
	best := ns[0]
	for _, n := range ns[:min(ixFaceCoverNear, len(ns))] {
		if s.qual[n.i] > s.qual[best.i] {
			best = n
		}
	}
	return s.ids[best.i]
}

func ixPairKey(x, y int64) [2]int64 { return [2]int64{min(x, y), max(x, y)} }

// ixPersonNoPairs lists the pairs a person said are different, as the
// people are now (after merges).
func (a *App) ixPersonNoPairs(ctx context.Context, resolve func(int64) int64) map[[2]int64]bool {
	out := map[[2]int64]bool{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT a, b FROM ix_person_pair WHERE state='no'`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var x, y int64
		if rows.Scan(&x, &y) == nil {
			out[ixPairKey(resolve(x), resolve(y))] = true
		}
	}
	return out
}

// ixJPair is a "Same person?" suggestion for the People tab.
type ixJPair struct {
	A      int64   `json:"a"`
	B      int64   `json:"b"`
	Sim    float32 `json:"sim"`
	AFaces []int64 `json:"aFaces"`
	BFaces []int64 `json:"bFaces"`
}

// ixPeopleSuggestions lists the suggestions between people of the list
// (shown, not hidden), with a few of the clearest faces of each.
func (a *App) ixPeopleSuggestions(ctx context.Context, list []ixPerson, limit int) []ixJPair {
	out := []ixJPair{}
	shown := map[int64]bool{}
	for _, p := range list {
		if !p.Hidden {
			shown[p.ID] = true
		}
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT a, b, sim FROM ix_person_pair WHERE state='ask' ORDER BY sim DESC LIMIT ?`, ixFaceSuggestKeep)
	if err != nil {
		return out
	}
	for rows.Next() {
		var p ixJPair
		if rows.Scan(&p.A, &p.B, &p.Sim) == nil && shown[p.A] && shown[p.B] && len(out) < limit {
			out = append(out, p)
		}
	}
	rows.Close()
	if len(out) == 0 {
		return out
	}
	no := a.ixPersonNoPairs(ctx, func(id int64) int64 { return a.ixPersonResolve(ctx, id) })
	need := map[int64]bool{}
	kept := out[:0]
	for _, p := range out {
		if !no[ixPairKey(p.A, p.B)] {
			kept = append(kept, p)
			need[p.A], need[p.B] = true, true
		}
	}
	out = kept
	best := a.ixPeopleBestFaces(ctx, need, 4)
	for i := range out {
		out[i].AFaces, out[i].BFaces = best[out[i].A], best[out[i].B]
	}
	return out
}

// ixPeopleBestFaces gives the n clearest faces of each of the people.
func (a *App) ixPeopleBestFaces(ctx context.Context, people map[int64]bool, n int) map[int64][]int64 {
	s := &a.ixMLBox().faces
	out := map[int64][]int64{}
	if s.load(ctx, a.db.Reader, ixFaceModel) != nil {
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx := map[int64][]int32{}
	for i, p := range s.person {
		if people[p] && s.norms[i] != 0 {
			idx[p] = append(idx[p], int32(i))
		}
	}
	for p, is := range idx {
		sort.Slice(is, func(x, y int) bool {
			if s.qual[is[x]] != s.qual[is[y]] {
				return s.qual[is[x]] > s.qual[is[y]]
			}
			return s.ids[is[x]] < s.ids[is[y]]
		})
		for _, i := range is[:min(n, len(is))] {
			out[p] = append(out[p], s.ids[i])
		}
	}
	return out
}
