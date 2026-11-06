package app

import (
	"context"
	"errors"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// ix.12.7: copies left behind by files moved into the library by hand.
//
// On 11-06 part of the staging folder was moved to the library by hand while
// 12.5 ran. 12.5 took the moved files for lost and downloaded their messages
// again, under new names, so each moved copy stayed in the library as an
// exact duplicate the catalogue does not know (12.6 adopts such copies
// instead). The library names a file "<name>_<message>_<32 hex>.<ext>"
// (download.destinationName); a re-download differs from the hand-moved copy
// only in the 32 random hex digits. This plan finds library files the
// catalogue does not know whose name, message and size match a catalogued
// file in the same folder, and moves them to _trash. Nothing is read to make
// the plan: each step compares both files' SHA-256 before it moves anything,
// and leaves a file whose bytes differ (or that the catalogue has meanwhile
// taken up) where it is. Undo moves them back.

// ixUncatOp is the plan step for one such copy.
const ixUncatOp = "trash-uncatalogued"

var ixLibName = regexp.MustCompile(`^(.*_\d+_)([0-9a-f]{32})(\.[^./]*)?$`)

// ixPlanUncatalogued plans moving those copies to _trash.
func (a *App) ixPlanUncatalogued(ctx context.Context) ([]ixMoveOp, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT file_path, lower(COALESCE(file_hash,'')), COALESCE(file_size,0) FROM downloads
		WHERE file_path IS NOT NULL AND file_path<>'' AND (status IS NULL OR status='completed')`)
	if err != nil {
		return nil, err
	}
	type keeper struct {
		rel, sum string
		size     int64
	}
	known := map[string]bool{}   // every catalogued path (ixPathKey)
	keep := map[string]keeper{}  // folder/stem+ext+size -> a catalogued copy with its SHA-256
	folders := map[string]bool{} // folders holding catalogued files
	for rows.Next() {
		var stored, sum string
		var size int64
		if rows.Scan(&stored, &sum, &size) != nil {
			continue
		}
		rel := ixNormPath(stored)
		if rel == "" {
			continue
		}
		known[ixPathKey(rel)] = true
		dir, name := path.Dir(rel), path.Base(rel)
		m := ixLibName.FindStringSubmatch(name)
		if m == nil || len(sum) != 64 || size <= 0 {
			continue
		}
		folders[dir] = true
		k := dir + "/" + m[1] + m[3] + "\x00" + strconv.FormatInt(size, 10)
		if _, ok := keep[k]; !ok {
			keep[k] = keeper{rel, sum, size}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Files a download or the staging folder's mover is about to catalogue.
	for _, q := range []string{`SELECT path FROM tgdl_ingest_files`} {
		if r, err := a.db.Reader.QueryContext(ctx, q); err == nil {
			for r.Next() {
				var p string
				if r.Scan(&p) == nil {
					known[ixPathKey(ixNormPath(p))] = true
				}
			}
			r.Close()
		}
	}
	dirs := make([]string, 0, len(folders))
	for d := range folders {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	stamp := time.Now().In(ixTZ).Format("2006-01-02")
	var ops []ixMoveOp
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(a.ixAbs(dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			rel := dir + "/" + e.Name()
			if known[ixPathKey(rel)] {
				continue
			}
			m := ixLibName.FindStringSubmatch(e.Name())
			if m == nil {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			k, ok := keep[dir+"/"+m[1]+m[3]+"\x00"+strconv.FormatInt(info.Size(), 10)]
			if !ok {
				continue
			}
			ops = append(ops, ixMoveOp{Op: ixUncatOp, Src: rel, Dst: "_trash/" + stamp + "/" + rel, SHA256: k.sum, Size: info.Size(), Repoint: k.rel})
			if len(ops) >= ixMaxPlanOps {
				return ops, nil
			}
		}
	}
	return ops, nil
}

// ixApplyUncat carries out one step: the copy kept is there with the bytes
// recorded, this file is still not catalogued, and it has the same bytes
// (ixMoveVerified checks them before moving).
//
// A step whose files do not qualify any more is skipped (ixSkipped), and the
// plan goes on: each file is decided on its own.
func (a *App) ixApplyUncat(ctx context.Context, o ixMoveOp) error {
	if _, err := os.Stat(a.ixAbs(o.Src)); os.IsNotExist(err) {
		return ixSkipped{"no longer there"}
	}
	if err := a.ixCheckKeeper(o); err != nil {
		return ixSkipped{"left where it is: " + err.Error()}
	}
	ids, _, err := ixRowsAt(ctx, a.db.Reader, o.Src)
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		return ixSkipped{"the catalogue has this file now: left where it is"}
	}
	err = ixMoveVerified(a.ixAbs(o.Src), a.ixAbs(o.Dst), o.SHA256)
	var differs ixHashDiffers
	if errors.As(err, &differs) {
		return ixSkipped{"its bytes differ from the copy kept: left where it is"}
	}
	return err
}
