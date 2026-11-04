package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Space for files already downloaded: the clean-up plan that moves
// byte-identical extra copies to _trash also lists near-identical videos,
// the same video saved as different files (re-encoded, resized), as the
// video fingerprints find them: a "same" match (ix_vdup) that covers 90%
// of both (stricter than the 80% of "same") with lengths within 2%. Of
// each such video the best copy stays (the one a person chose to show,
// else the most pixels, then the longest, then the most bytes, then the
// earliest post); the others are listed with their sizes, to be moved to
// _trash only once a person has reviewed and applied the plan. Their
// catalogue entries then point at the kept copy, and the plan can be
// undone step by step like any other.

// ixNearOp is a plan step: move a near-identical copy to _trash.
const ixNearOp = "trash-near"

const ixNearCover = 0.9

type ixNearFile struct {
	key, stored, rel string // stored: the catalogue's spelling of the kept path
	hash             string
	f                ixCopyFacts
	info             os.FileInfo
}

// ixNearFileOf finds the copy kept on disk of a saved file key and its
// facts (false when it is not on disk).
func (a *App) ixNearFileOf(ctx context.Context, key string) (ixNearFile, bool) {
	q := `SELECT d.id, d.file_path, COALESCE(d.file_hash,''), COALESCE(d.file_size,0), COALESCE(d.media_width,0), COALESCE(d.media_height,0), COALESCE(d.media_duration_ms,0),
			COALESCE((SELECT MIN(CASE WHEN m.fwd_date>0 AND m.fwd_date<m.date THEN m.fwd_date ELSE m.date END) FROM ix_messages m WHERE m.group_id=d.group_id AND m.message_id=d.message_id),0)
		FROM downloads d WHERE d.file_path IS NOT NULL AND d.file_path<>'' AND (d.status IS NULL OR d.status='completed') AND `
	var arg string
	if h, ok := strings.CutPrefix(key, "h:"); ok {
		q, arg = q+`lower(d.file_hash)=? ORDER BY d.id`, h
	} else if p, ok := strings.CutPrefix(key, "p:"); ok {
		q, arg = q+`replace(d.file_path,char(92),'/')=? ORDER BY d.id`, p
	} else {
		return ixNearFile{}, false
	}
	rows, err := a.db.Reader.QueryContext(ctx, q, arg)
	if err != nil {
		return ixNearFile{}, false
	}
	defer rows.Close()
	var out ixNearFile
	found := false
	for rows.Next() {
		var id, size, date int64
		var path, hash string
		var w, h int
		var dur int64
		if rows.Scan(&id, &path, &hash, &size, &w, &h, &dur, &date) != nil {
			continue
		}
		if found {
			if date > 0 && (out.f.Date == 0 || date < out.f.Date) {
				out.f.Date = date // the earliest post of any of its copies
			}
			continue
		}
		rel := ixNormPath(path)
		info, err := os.Stat(a.ixAbs(rel))
		if rel == "" || err != nil || !info.Mode().IsRegular() {
			continue
		}
		found = true
		out = ixNearFile{key: key, stored: path, rel: rel, hash: strings.ToLower(hash), info: info,
			f: ixCopyFacts{ID: id, W: w, H: h, Dur: dur, Size: size, Date: date, Type: "video"}}
	}
	if found && out.f.Dur <= 0 {
		var d float64
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT duration FROM ix_vfp WHERE key=?`, key).Scan(&d)
		out.f.Dur = int64(d * 1000)
	}
	return out, found
}

// ixPlanNearCopies adds the near-identical video copies to a clean-up
// plan (ops already planned: byte-identical copies; their files are left
// to that part).
func (a *App) ixPlanNearCopies(ctx context.Context, ops []ixMoveOp) ([]ixMoveOp, error) {
	planned := map[string]bool{}
	for _, o := range ops {
		planned[ixPathKey(o.Src)] = true
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT key_a, key_b FROM ix_vdup WHERE kind='same' AND cover_a>=? AND cover_b>=? ORDER BY key_a, key_b`, ixNearCover, ixNearCover)
	if err != nil {
		return ops, err
	}
	near := map[string][]string{}
	for rows.Next() {
		var x, y string
		if rows.Scan(&x, &y) == nil && x != y {
			near[x] = append(near[x], y)
			near[y] = append(near[y], x)
		}
	}
	rows.Close()
	if len(near) == 0 {
		return ops, nil
	}
	chosen := map[string]bool{}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT key FROM ix_org_keeper`); err == nil {
		for rows.Next() {
			var k string
			if rows.Scan(&k) == nil {
				chosen[k] = true
			}
		}
		rows.Close()
	}
	files := map[string]ixNearFile{}
	var keys []string
	for k := range near {
		if f, ok := a.ixNearFileOf(ctx, k); ok && !planned[ixPathKey(f.rel)] {
			files[k] = f
			keys = append(keys, k)
		}
	}
	// Best copies first: each keeps its own direct matches.
	sort.Slice(keys, func(i, j int) bool {
		x, y := files[keys[i]], files[keys[j]]
		if chosen[x.key] != chosen[y.key] {
			return chosen[x.key]
		}
		if ixBetterCopy(x.f, y.f) != ixBetterCopy(y.f, x.f) {
			return ixBetterCopy(x.f, y.f)
		}
		return x.key < y.key
	})
	stamp := time.Now().In(ixTZ).Format("2006-01-02")
	done := map[string]bool{}
	for _, k := range keys {
		if done[k] {
			continue
		}
		done[k] = true
		keep := files[k]
		for _, other := range near[k] {
			t, ok := files[other]
			if !ok || done[other] || chosen[other] || !ixDurWithin(keep.f.Dur, t.f.Dur, 0.02) || os.SameFile(keep.info, t.info) {
				continue
			}
			done[other] = true
			sum := ""
			if strings.HasPrefix(t.key, "h:") {
				sum = t.key[2:]
			}
			ops = append(ops, ixMoveOp{Op: ixNearOp, Src: t.rel, Dst: "_trash/" + stamp + "/" + t.rel, SHA256: sum, Size: t.f.Size, Repoint: keep.stored})
			if len(ops) >= ixMaxPlanOps {
				return ops, nil
			}
		}
	}
	return ops, nil
}

// ixApplyNear carries out one near-copy step: the kept copy is checked,
// the copy moved to _trash (its SHA-256 checked) and its catalogue entries
// pointed at the kept copy, taking the kept copy's checksum and size.
func (a *App) ixApplyNear(ctx context.Context, plan int64, o ixMoveOp) error {
	keepRel := ixNormPath(o.Repoint)
	if keepRel == "" || ixPathKey(keepRel) == ixPathKey(o.Src) {
		return errors.New("the copy to keep is this same file")
	}
	ki, err := os.Stat(a.ixAbs(keepRel))
	if err != nil {
		return fmt.Errorf("the copy to keep is missing: %w", err)
	}
	if si, err := os.Stat(a.ixAbs(o.Src)); err == nil && os.SameFile(si, ki) {
		return errors.New("the copy to keep is this same file")
	}
	var keepHash string
	var keepSize int64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(file_hash,''), COALESCE(file_size,0) FROM downloads WHERE file_path=? ORDER BY id LIMIT 1`, o.Repoint).Scan(&keepHash, &keepSize)
	if len(keepHash) == 64 {
		got, _, err := ixHashFile(a.ixAbs(keepRel))
		if err != nil {
			return err
		}
		if !strings.EqualFold(got, keepHash) {
			return errors.New("the copy to keep no longer has the bytes recorded for it")
		}
	}
	moved := a.ixLanded(o)
	if !moved {
		if err := ixMoveVerified(a.ixAbs(o.Src), a.ixAbs(o.Dst), o.SHA256); err != nil {
			return err
		}
	}
	err = a.ixRepoint(ctx, plan, o)
	if err == nil {
		err = a.ixNearFacts(ctx, plan, o, keepHash, keepSize, o.Repoint)
	}
	if err != nil {
		if back := ixMoveVerified(a.ixAbs(o.Dst), a.ixAbs(o.Src), o.SHA256); back != nil {
			return fmt.Errorf("%v; moving it back also failed: %v (it is at %s)", err, back, o.Dst)
		}
	}
	return err
}

// ixNearFacts sets the checksum and size of the entries a step moved,
// those now at path.
func (a *App) ixNearFacts(ctx context.Context, plan int64, o ixMoveOp, hash string, size int64, path string) error {
	var ids string
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT rows FROM ix_moves WHERE plan_id=? AND seq=?`, plan, o.Seq).Scan(&ids); err != nil || ids == "" {
		return err
	}
	var list []string
	for _, id := range strings.Split(ids, ",") {
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			return fmt.Errorf("bad journal entry %q", id)
		}
		list = append(list, id)
	}
	_, err := a.db.Writer.ExecContext(ctx, `UPDATE downloads SET file_hash=?, file_size=? WHERE file_path=? AND id IN (`+strings.Join(list, ",")+`)`, hash, size, path)
	return err
}

// ixUndoNear puts a near copy back where it was, with its own checksum.
func (a *App) ixUndoNear(ctx context.Context, plan int64, o ixMoveOp) error {
	if err := ixMoveVerified(a.ixAbs(o.Dst), a.ixAbs(o.Src), o.SHA256); err != nil {
		return err
	}
	if err := a.ixUnrepoint(ctx, plan, o); err != nil {
		return err
	}
	var original string
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT src_stored FROM ix_moves WHERE plan_id=? AND seq=?`, plan, o.Seq).Scan(&original)
	if original == "" {
		original = o.Src
	}
	return a.ixNearFacts(ctx, plan, o, o.SHA256, o.Size, original)
}
