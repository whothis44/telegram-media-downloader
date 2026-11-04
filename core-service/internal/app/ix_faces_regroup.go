package app

import (
	"bytes"
	"context"
	"fmt"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Grouping again: the current rules (quality, cannot-links, files not
// frames) applied to the faces already stored. It runs once by itself
// after an update that changes the rules, and when asked in the People
// tab. What people decided is kept:
//   - people with a name, hidden people, people others were merged into
//     and people with a chosen picture stay, with their faces that pass
//     the rules (good faces, and weak ones clearly alike to them; one face
//     per picture or frame);
//   - faces taken out stay out, and merged people stay merged;
//   - "not the same" answers are kept.
// All other faces are grouped again from scratch, in three passes (good
// faces, good faces again, weak faces), a few hundred at a time. A person
// made again mostly from one earlier person gets that person's number, so
// links and answers about them still hold.
//
// State (ix_face_meta "regroup"): "" nothing to do, "pending" to start,
// "group:<pass>:<last face id>" under way. Stopping part-way is safe: it
// goes on from there next time.

const ixFaceRulesVersion = "ix10-1" // change to group all stored faces again

// ixFaceKeptSQL picks the people a person made decisions about.
const ixFaceKeptSQL = `merged_into=0 AND (name<>'' OR hidden=1 OR cover>0 OR id IN (SELECT merged_into FROM ix_people WHERE merged_into>0))`

const ixFaceRegroupChunk = 500

// ixFaceRegroupKick starts grouping again (or measuring old faces) in the
// background when there is something to do.
func (a *App) ixFaceRegroupKick(ctx context.Context) {
	pb := a.ixPeopleBox()
	pb.mu.Lock()
	running := pb.regrouping
	pb.mu.Unlock()
	if running {
		return
	}
	var faces, unmeasured int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(blur=-1),0) FROM ix_faces`).Scan(&faces, &unmeasured)
	rules, state := a.ixFaceMetaGet(ctx, "rules"), a.ixFaceMetaGet(ctx, "regroup")
	if faces == 0 && state == "" {
		// Nothing stored yet: the current rules apply from the start.
		if rules != ixFaceRulesVersion {
			_ = a.ixFaceMetaSet(ctx, "rules", ixFaceRulesVersion)
		}
		return
	}
	if rules == ixFaceRulesVersion && state == "" && unmeasured == 0 {
		return
	}
	pb.mu.Lock()
	if pb.regrouping {
		pb.mu.Unlock()
		return
	}
	pb.regrouping = true
	pb.mu.Unlock()
	base := a.ctx
	if base == nil {
		base = context.Background()
	}
	go func() {
		defer func() {
			pb.mu.Lock()
			pb.regrouping, pb.regroup = false, ""
			pb.mu.Unlock()
		}()
		_ = a.ixFaceRegroupRun(base)
	}()
}

// ixFaceRegroupAsk starts grouping all faces again (People tab).
func (a *App) ixFaceRegroupAsk(ctx context.Context) error {
	if err := a.ixFaceMetaSet(ctx, "regroup", "pending"); err != nil {
		return err
	}
	a.ixFaceRegroupKick(ctx)
	return nil
}

func (a *App) ixFaceRegroupNote(note string) {
	pb := a.ixPeopleBox()
	pb.mu.Lock()
	pb.regroup = note
	pb.mu.Unlock()
}

// ixFaceRegroupStatus says what grouping again is doing ("" when idle).
func (a *App) ixFaceRegroupStatus(ctx context.Context) string {
	pb := a.ixPeopleBox()
	pb.mu.Lock()
	note, running := pb.regroup, pb.regrouping
	pb.mu.Unlock()
	if running && note != "" {
		return note
	}
	if a.ixFaceMetaGet(ctx, "regroup") != "" {
		return "Grouping faces again with the current rules: waiting to go on."
	}
	return ""
}

// ixFaceMetaSwap sets k to v only while it is still old (so a new request
// made meanwhile is not overwritten).
func (a *App) ixFaceMetaSwap(ctx context.Context, k, old, v string) error {
	_, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_face_meta SET v=? WHERE k=? AND v=?`, v, k, old)
	return err
}

// ixFaceRegroupRun does the work of grouping again, up to the end.
func (a *App) ixFaceRegroupRun(ctx context.Context) error {
	s := &a.ixMLBox().faces
	if err := s.load(ctx, a.db.Reader, ixFaceModel); err != nil {
		return err
	}
	if err := a.ixFaceBackfill(ctx); err != nil {
		return err
	}
	if a.ixFaceMetaGet(ctx, "rules") != ixFaceRulesVersion && a.ixFaceMetaGet(ctx, "regroup") == "" {
		if err := a.ixFaceMetaSet(ctx, "regroup", "pending"); err != nil {
			return err
		}
	}
	for ctx.Err() == nil {
		st := a.ixFaceMetaGet(ctx, "regroup")
		switch {
		case st == "":
			return nil
		case st == "pending":
			a.ixFaceRegroupNote("Grouping faces again with the current rules: starting.")
			if err := a.ixFaceRegroupReset(ctx); err != nil {
				return err
			}
		case strings.HasPrefix(st, "group:"):
			var pass int
			var after int64
			_, _ = fmt.Sscanf(st, "group:%d:%d", &pass, &after)
			if pass >= 3 {
				a.ixFaceRegroupNote("Grouping faces again with the current rules: comparing people.")
				if err := a.ixFacePeoplePass(ctx); err != nil {
					return err
				}
				if _, err := a.db.Writer.ExecContext(ctx, `DELETE FROM ix_face_prev`); err != nil {
					return err
				}
				if err := a.ixFaceMetaSet(ctx, "rules", ixFaceRulesVersion); err != nil {
					return err
				}
				// (The browser notices the new people by itself: its model
				// stamp includes the faces.)
				if err := a.ixFaceMetaSwap(ctx, "regroup", st, ""); err != nil {
					return err
				}
				continue
			}
			a.ixFaceRegroupNote(fmt.Sprintf("Grouping faces again with the current rules: step %d of 3.", pass+1))
			next, err := a.ixFaceRegroupStep(ctx, pass, after)
			if err != nil {
				return err
			}
			if next == 0 {
				pass++
			}
			if err := a.ixFaceMetaSwap(ctx, "regroup", st, fmt.Sprintf("group:%d:%d", pass, next)); err != nil {
				return err
			}
		default:
			if err := a.ixFaceMetaSet(ctx, "regroup", "pending"); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// ixFaceBackfill measures size and sharpness of faces stored before they
// were measured, from their crops (no crop: left unknown, which does not
// count against them).
func (a *App) ixFaceBackfill(ctx context.Context) error {
	s := &a.ixMLBox().faces
	for ctx.Err() == nil {
		var left int
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_faces WHERE blur=-1`).Scan(&left)
		if left == 0 {
			return nil
		}
		a.ixFaceRegroupNote(fmt.Sprintf("Measuring the faces found before: %d left.", left))
		rows, err := a.db.Reader.QueryContext(ctx, `SELECT id, score FROM ix_faces WHERE blur=-1 ORDER BY id LIMIT ?`, ixFaceRegroupChunk)
		if err != nil {
			return err
		}
		type face struct {
			id    int64
			score float64
		}
		var faces []face
		for rows.Next() {
			var f face
			if rows.Scan(&f.id, &f.score) == nil {
				faces = append(faces, f)
			}
		}
		rows.Close()
		if len(faces) == 0 {
			return nil
		}
		for _, f := range faces {
			px, blur := 0.0, float64(ixFaceBlurUnknown)
			if data, err := os.ReadFile(filepath.Join(a.ixThumbDir(), ixFaceCropName(f.id))); err == nil {
				if img, err := jpeg.Decode(bytes.NewReader(data)); err == nil {
					px, blur = ixFaceMeasureCrop(img)
				}
			}
			if _, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_faces SET px=?, blur=? WHERE id=?`, px, blur, f.id); err != nil {
				return err
			}
			s.mu.Lock()
			s.setInfoLocked(f.id, f.score, px, blur)
			s.mu.Unlock()
		}
	}
	return ctx.Err()
}

// ixFaceRegroupReset keeps what people decided and lets go of the rest.
func (a *App) ixFaceRegroupReset(ctx context.Context) error {
	s := &a.ixMLBox().faces
	s.group.Lock()
	defer s.group.Unlock()
	kept := map[int64]bool{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT id FROM ix_people WHERE `+ixFaceKeptSQL)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			kept[id] = true
		}
	}
	rows.Close()
	s.mu.RLock()
	release := s.regroupReleaseLocked(kept)
	s.mu.RUnlock()
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM ix_face_prev`,
		`INSERT INTO ix_face_prev(face,person) SELECT id, person FROM ix_faces WHERE person>0 AND manual=0`,
		`UPDATE ix_faces SET person=0 WHERE person>0 AND manual=0 AND person NOT IN (SELECT id FROM ix_people WHERE ` + ixFaceKeptSQL + `)`,
		`DELETE FROM ix_person_pair WHERE state='ask'`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	for _, id := range release {
		if _, err := tx.ExecContext(ctx, `UPDATE ix_faces SET person=0 WHERE id=? AND manual=0`, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ix_face_meta SET v='group:0:0' WHERE k='regroup' AND v='pending'`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	for i, p := range s.person {
		if p > 0 && !kept[p] {
			s.person[i] = 0
		}
	}
	for _, id := range release {
		s.setPersonLocked(id, 0)
	}
	s.mu.Unlock()
	return nil
}

// regroupReleaseLocked lists the faces of kept people that do not pass the
// rules: unusable faces; a second face in one picture or frame (the one
// further from the person's middle goes); weak faces not clearly alike to
// any of the person's good faces. A person with no good face keeps their
// usable faces (one per picture or frame).
func (s *ixFaceStore) regroupReleaseLocked(kept map[int64]bool) []int64 {
	byPerson := map[int64][]int32{}
	for i, p := range s.person {
		if kept[p] && s.norms[i] != 0 {
			byPerson[p] = append(byPerson[p], int32(i))
		}
	}
	d := s.dim
	vec := func(i int32) []int8 { return s.vecs[int(i)*d : int(i+1)*d] }
	cos := func(i, j int32) float32 { return float32(ixDot8(vec(i), vec(j))) / (s.norms[i] * s.norms[j]) }
	var release []int64
	for _, faces := range byPerson {
		var good, weak []int32
		for _, i := range faces {
			switch s.tier[i] {
			case ixFaceGood:
				good = append(good, i)
			case ixFaceWeak:
				weak = append(weak, i)
			default:
				release = append(release, s.ids[i])
			}
		}
		base := good
		if len(base) == 0 {
			base = weak
		}
		// Middle of the faces the person is judged by.
		mid := make([]float32, d)
		for _, i := range base {
			w := 1 / s.norms[i]
			for j, x := range vec(i) {
				mid[j] += float32(x) * w
			}
		}
		toMid := map[int32]float32{}
		for _, i := range faces {
			var dot float32
			for j, x := range vec(i) {
				dot += float32(x) * mid[j]
			}
			toMid[i] = dot / s.norms[i]
		}
		byMid := func(l []int32) {
			sort.Slice(l, func(x, y int) bool { return toMid[l[x]] > toMid[l[y]] })
		}
		used := map[int64]bool{}
		keep := func(i int32) bool {
			if sh := s.shotLocked(i); !used[sh] {
				used[sh] = true
				return true
			}
			return false
		}
		byMid(good)
		for _, i := range good {
			if !keep(i) {
				release = append(release, s.ids[i])
			}
		}
		if len(good) == 0 {
			byMid(weak)
			for _, i := range weak {
				if !keep(i) {
					release = append(release, s.ids[i])
				}
			}
			continue
		}
		// Weak faces are compared with the clearest few hundred good ones.
		ref := append([]int32(nil), good...)
		sort.Slice(ref, func(x, y int) bool { return s.qual[ref[x]] > s.qual[ref[y]] })
		ref = ref[:min(len(ref), 500)]
		type cand struct {
			i   int32
			sim float32
		}
		var cands []cand
		for _, i := range weak {
			best := float32(math.Inf(-1))
			for _, j := range ref {
				best = max(best, cos(i, j))
			}
			cands = append(cands, cand{i, best})
		}
		sort.Slice(cands, func(x, y int) bool { return cands[x].sim > cands[y].sim })
		for _, c := range cands {
			if c.sim < ixFaceWeakSim || !keep(c.i) {
				release = append(release, s.ids[c.i])
			}
		}
	}
	return release
}

// ixFaceRegroupStep groups up to a chunk of loose faces after the given
// one: good faces in passes 0 and 1, weak ones in pass 2. It says the last
// face looked at (0: the pass is over).
func (a *App) ixFaceRegroupStep(ctx context.Context, pass int, after int64) (int64, error) {
	s := &a.ixMLBox().faces
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT id FROM ix_faces WHERE person=0 AND manual=0 AND id>? ORDER BY id LIMIT ?`, after, ixFaceRegroupChunk)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return 0, nil
	}
	unit := a.ixFaceUnitOf()
	reuse := a.ixFaceReuse(ctx)
	for _, id := range ids {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		v, tier, ok := s.vecOf(id)
		if !ok || (pass < 2) != (tier == ixFaceGood) {
			continue
		}
		s.group.Lock()
		_, err := a.ixGroupFaceLocked(ctx, id, v, unit, reuse) // (passes keep their order: no retries here)
		s.group.Unlock()
		if err != nil {
			return 0, err
		}
	}
	return ids[len(ids)-1], nil
}

// ixFaceReuse gives a person made again the number of the earlier person
// most of its faces were with, when that person is now empty, was not
// merged away, and none of the faces was taken out of them (those stay
// loose instead).
func (a *App) ixFaceReuse(ctx context.Context) func([]int64) (int64, []int64) {
	return func(take []int64) (int64, []int64) {
		counts := map[int64]int{}
		for _, f := range take {
			var p int64
			if a.db.Reader.QueryRowContext(ctx, `SELECT person FROM ix_face_prev WHERE face=?`, f).Scan(&p) == nil && p > 0 {
				counts[p]++
			}
		}
		var best int64
		for p, n := range counts {
			if n > counts[best] || n == counts[best] && p < best {
				best = p
			}
		}
		if best == 0 || counts[best]*2 <= len(take) {
			return 0, take
		}
		var into, faces int
		if a.db.Reader.QueryRowContext(ctx, `SELECT merged_into, (SELECT COUNT(*) FROM ix_faces WHERE person=?) FROM ix_people WHERE id=?`, best, best).Scan(&into, &faces) != nil || into != 0 || faces != 0 {
			return 0, take
		}
		var out []int64
		for _, f := range take {
			if !a.ixFaceRejects(ctx, f)[best] {
				out = append(out, f)
			}
		}
		if len(out) == 0 {
			return 0, take
		}
		return best, out
	}
}
