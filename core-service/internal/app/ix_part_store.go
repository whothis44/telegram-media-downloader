package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/diskfree"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// Parts kept from checks (ix.12.11). A check of a big video (ix_copies_sample.go)
// reads 10 to 32 parts of 1 MiB from Telegram. Most checked videos are not
// copies and are downloaded later, and those parts used to be fetched again
// (2.06 GB for 147 videos by 11-06). Now a check's parts are kept on disk,
// dataDir/ixparts/<SHA-256 of the Telegram identity>/<part index>, with a row
// each in ix_part_store, and a download of the same identity takes them
// (telegram/kept_parts.go): Telegram serves the same bytes for the same file.
// A part is read back only with the length and SHA-256 stored; anything
// doubtful is asked of Telegram as before.
//
// Space: at most ixPartsCap in all, nothing kept while the data disk has less
// than ixPartsFreeFloor free. A file's parts go once it is downloaded or no
// queue row of it is left, and the oldest go first over the cap, or while the
// disk is under the floor. The folder is not in the backups (they hold the
// database and the sessions).
//
// ix.12.12: the cap went from 4 GB to 100 GB. Checked videos download about a
// day after their check (on 11-06: a quarter within 2 hours, a quarter after 38
// hours), 240 checked ones still waited a median 55 hours, and checks kept
// about 120 MB an hour, so 4 GB held two days and the slowest lost their parts
// first. In practice the floor below limits the space now, not the cap. The floor went from 20 to 60 GB, above
// the 50 GB the staging folder keeps free when it runs past its own size
// (download/staging.go, DefaultStageKeepFree; a setting since ix.12.14): kept parts give way to staged
// downloads, and the sweep removes the oldest while the disk is under it.

const ixPartStoreSchema = `
CREATE TABLE IF NOT EXISTS ix_part_store ( -- 1 MiB parts a check read from Telegram, kept for the download (ix.12.11)
	ident  TEXT    NOT NULL, -- the file's Telegram identity (kind:id:size)
	part   INTEGER NOT NULL, -- offset / 1 MiB
	bytes  INTEGER NOT NULL,
	sha256 TEXT    NOT NULL,
	at     INTEGER NOT NULL,
	PRIMARY KEY (ident, part)
);
CREATE INDEX IF NOT EXISTS ix_part_store_at ON ix_part_store(at);
`

// Limits (variables for tests).
var (
	ixPartsCap        int64 = 100 << 30
	ixPartsFreeFloor  int64 = 60 << 30
	ixPartsFree             = diskfree.Free
	ixPartsSweepEvery       = 10 * time.Minute
)

// ixPartsFloor is the free space under which nothing is kept and the oldest
// parts go: ixPartsFreeFloor, or 10 GB above what the staging folder keeps
// free when that is more (ix.12.14: "Keep at least N GB free" may be set
// above 50 GB, and staged downloads need the room more than kept parts).
func (a *App) ixPartsFloor(ctx context.Context) int64 {
	floor := ixPartsFreeFloor
	if s := a.ixLoadSettings(ctx); s.StageDir != "" {
		floor = max(floor, int64(s.StageKeepFree()+10)<<30)
	}
	return floor
}

// ixPartsMu orders keeping, sweeping and trimming (one App in the program).
var ixPartsMu sync.Mutex

// ixPartsCounts are the report's figures since the start.
type ixPartsCounts struct {
	Kept, KeptBytes   atomic.Int64 // parts written
	LowDisk, Failed   atomic.Int64 // checks whose parts were not kept: too little free space, a write failed
	BadBytes, Missing atomic.Int64 // parts refused on reading: SHA-256 differs, file gone or unreadable
	Removed           atomic.Int64 // files whose parts went (downloaded, out of the queue, over the cap)
	ForSpace          atomic.Int64 // of those, removed because the disk was under the floor (ix.12.12)
	Downloads         atomic.Int64 // downloads that took kept parts (ix.12.12)
}

var ixPartsTotals sync.Map // *App -> *ixPartsCounts

func ixPartsCountsOf(a *App) *ixPartsCounts {
	v, _ := ixPartsTotals.LoadOrStore(a, &ixPartsCounts{})
	return v.(*ixPartsCounts)
}

func (a *App) ixPartsRoot() string { return filepath.Join(a.dataDir, "ixparts") }

func ixPartsDirName(ident string) string {
	sum := sha256.Sum256([]byte(ident))
	return hex.EncodeToString(sum[:])
}

// ixPartIdent is the identity as the checks write it (kind:id:size).
func ixPartIdent(id telegram.MediaIdentity) string {
	return id.Kind + ":" + id.ID + ":" + strconv.FormatInt(id.Size, 10)
}

// ixPartsKeep keeps the parts a check of ident read. A failed write keeps
// nothing of this check and changes nothing else.
func (a *App) ixPartsKeep(ctx context.Context, ident string, parts map[int64][]byte) {
	if a == nil || a.dataDir == "" || ident == "" || len(parts) == 0 {
		return
	}
	ixPartsMu.Lock()
	defer ixPartsMu.Unlock()
	have := map[int64]bool{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT part FROM ix_part_store WHERE ident=?`, ident)
	if err != nil {
		return
	}
	for rows.Next() {
		var p int64
		if rows.Scan(&p) == nil {
			have[p] = true
		}
	}
	rows.Close()
	var todo []int64
	var need int64
	for i, p := range parts {
		if !have[i] && i >= 0 && len(p) > 0 {
			todo = append(todo, i)
			need += int64(len(p))
		}
	}
	if len(todo) == 0 || need > ixPartsCap {
		return
	}
	t := ixPartsCountsOf(a)
	if free, ok := ixPartsFree(a.dataDir); !ok || free-need < a.ixPartsFloor(ctx) {
		t.LowDisk.Add(1)
		return
	}
	slices.Sort(todo)
	dir := filepath.Join(a.ixPartsRoot(), ixPartsDirName(ident))
	var written []string
	undo := func() {
		for _, p := range written {
			_ = os.Remove(p)
		}
		if len(have) == 0 {
			_ = os.Remove(dir) // only when empty
		}
		t.Failed.Add(1)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Failed.Add(1)
		return
	}
	sums := map[int64]string{}
	for _, i := range todo {
		path := filepath.Join(dir, strconv.FormatInt(i, 10))
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, parts[i], 0o600); err != nil {
			_ = os.Remove(tmp)
			undo()
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			undo()
			return
		}
		written = append(written, path)
		sum := sha256.Sum256(parts[i])
		sums[i] = hex.EncodeToString(sum[:])
	}
	now := ixNowMs()
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		undo()
		return
	}
	for _, i := range todo {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO ix_part_store(ident,part,bytes,sha256,at) VALUES(?,?,?,?,?)`, ident, i, len(parts[i]), sums[i], now); err != nil {
			_ = tx.Rollback()
			undo()
			return
		}
	}
	if err := tx.Commit(); err != nil {
		undo()
		return
	}
	t.Kept.Add(int64(len(todo)))
	t.KeptBytes.Add(need)
	a.ixPartsTrimLocked(ctx, ident)
}

// ixPartsTrimLocked removes the oldest files' parts while over the cap, never
// those of keep (just kept).
func (a *App) ixPartsTrimLocked(ctx context.Context, keep string) {
	var total int64
	if a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM ix_part_store`).Scan(&total) != nil || total <= ixPartsCap {
		return
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT ident, SUM(bytes) FROM ix_part_store GROUP BY ident ORDER BY MAX(at), ident`)
	if err != nil {
		return
	}
	type file struct {
		ident string
		bytes int64
	}
	var files []file
	for rows.Next() {
		var f file
		if rows.Scan(&f.ident, &f.bytes) == nil {
			files = append(files, f)
		}
	}
	rows.Close()
	for _, f := range files {
		if total <= ixPartsCap {
			break
		}
		if f.ident == keep {
			continue
		}
		if a.ixPartsRemoveLocked(ctx, f.ident) {
			total -= f.bytes
		}
	}
}

// ixPartsRemoveLocked removes one file's parts: the rows first, so that a
// download looking at the same time finds nothing rather than a part going.
func (a *App) ixPartsRemoveLocked(ctx context.Context, ident string) bool {
	if _, err := a.db.Writer.ExecContext(ctx, `DELETE FROM ix_part_store WHERE ident=?`, ident); err != nil {
		return false
	}
	_ = os.RemoveAll(filepath.Join(a.ixPartsRoot(), ixPartsDirName(ident)))
	ixPartsCountsOf(a).Removed.Add(1)
	return true
}

type ixKeptPart struct {
	bytes int
	sum   string
}

// ixKeptPartsFor is the downloads' hook (telegram.SetKeptParts): the parts kept
// for one file, read back only with the length and SHA-256 stored. It runs in
// a download, outside the background tasks' guard: a fault in it is caught
// here and the download asks Telegram as before.
func (a *App) ixKeptPartsFor(id telegram.MediaIdentity) (kept telegram.KeptParts) {
	defer func() {
		if v := recover(); v != nil {
			kept = nil
			a.ixLog("parts kept from checks: program error, downloads ask Telegram: %v", v)
		}
	}()
	if a == nil || a.db == nil || a.dataDir == "" || !id.Valid() {
		return nil
	}
	ident := ixPartIdent(id)
	// Parts a check in this download read (ix.12.13) come first, from memory.
	mem := ixMemPartsGet(a, ident)
	disk := a.ixKeptPartsOnDisk(id, ident)
	switch {
	case mem == nil:
		return disk
	case disk == nil:
		return func(index int) ([]byte, bool) {
			p, ok := mem[int64(index)]
			return p, ok && len(p) > 0
		}
	}
	return func(index int) ([]byte, bool) {
		if p, ok := mem[int64(index)]; ok && len(p) > 0 {
			return p, true
		}
		return disk(index)
	}
}

// ixKeptPartsOnDisk: the parts kept on disk for one file (ix.12.11).
func (a *App) ixKeptPartsOnDisk(id telegram.MediaIdentity, ident string) telegram.KeptParts {
	base := a.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, 10*time.Second)
	defer cancel()
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT part, bytes, sha256 FROM ix_part_store WHERE ident=?`, ident)
	if err != nil {
		return nil
	}
	parts := map[int]ixKeptPart{}
	for rows.Next() {
		var i int
		var p ixKeptPart
		if rows.Scan(&i, &p.bytes, &p.sum) == nil {
			parts[i] = p
		}
	}
	rows.Close()
	if len(parts) == 0 {
		return nil
	}
	dir := filepath.Join(a.ixPartsRoot(), ixPartsDirName(ident))
	t := ixPartsCountsOf(a)
	return func(index int) (data []byte, ok bool) {
		defer func() {
			if v := recover(); v != nil {
				data, ok = nil, false
			}
		}()
		p, found := parts[index]
		if !found {
			return nil, false
		}
		data, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(index)))
		if err != nil || len(data) != p.bytes {
			t.Missing.Add(1)
			return nil, false
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != p.sum {
			t.BadBytes.Add(1)
			return nil, false
		}
		return data, true
	}
}

// ixKeptPartsUsed logs a download that took parts kept from its check (ix.12.12):
// sizes only. It runs in the download; a fault in it is caught here.
func (a *App) ixKeptPartsUsed(id telegram.MediaIdentity, bytes int64) {
	defer func() { _ = recover() }()
	ixPartsCountsOf(a).Downloads.Add(1)
	a.ixLog("a download took %d MB of its %d MB file from parts kept from its check, not from Telegram", bytes>>20, id.Size>>20)
}

// ixPartsSweep removes the parts of files downloaded or no longer queued,
// folders no row names (a write cut short), and the oldest over the cap.
func (a *App) ixPartsSweep(ctx context.Context) error {
	ixPartsMu.Lock()
	defer ixPartsMu.Unlock()
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT DISTINCT ident FROM ix_part_store`)
	if err != nil {
		return err
	}
	var idents []string
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			idents = append(idents, s)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	queued := map[string]bool{}
	if len(idents) > 0 {
		// The queue writes kind, id and size apart with a NUL, the checks with a
		// colon: compared here, not in SQLite.
		rows, err = a.db.Reader.QueryContext(ctx, `SELECT identity FROM tgdl_work WHERE status IN ('pending','processing','failed')`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s string
			if rows.Scan(&s) == nil {
				queued[strings.ReplaceAll(s, "\x00", ":")] = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	names := map[string]bool{}
	for _, ident := range idents {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		gone := !queued[ident]
		if !gone {
			if kind, id, size, ok := ixSplitPartIdent(ident); ok {
				var one int
				err := a.db.Reader.QueryRowContext(ctx, `SELECT 1 FROM downloads WHERE telegram_media_kind=? AND telegram_media_id=? AND telegram_media_size=? AND status='completed' LIMIT 1`, kind, id, size).Scan(&one)
				gone = err == nil
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
		}
		if gone && a.ixPartsRemoveLocked(ctx, ident) {
			continue
		}
		names[ixPartsDirName(ident)] = true
	}
	if entries, err := os.ReadDir(a.ixPartsRoot()); err == nil {
		for _, e := range entries {
			if !names[e.Name()] {
				_ = os.RemoveAll(filepath.Join(a.ixPartsRoot(), e.Name()))
			}
		}
	}
	a.ixPartsTrimLocked(ctx, "")
	a.ixPartsFreeUpLocked(ctx)
	return nil
}

// ixPartsFreeUpLocked removes the oldest files' parts while the data disk
// has less than ixPartsFreeFloor free (ix.12.12): downloads need the room more.
func (a *App) ixPartsFreeUpLocked(ctx context.Context) {
	floor := a.ixPartsFloor(ctx)
	free, ok := ixPartsFree(a.dataDir)
	if !ok || free >= floor {
		return
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT ident, SUM(bytes) FROM ix_part_store GROUP BY ident ORDER BY MAX(at), ident`)
	if err != nil {
		return
	}
	type file struct {
		ident string
		bytes int64
	}
	var files []file
	for rows.Next() {
		var f file
		if rows.Scan(&f.ident, &f.bytes) == nil {
			files = append(files, f)
		}
	}
	rows.Close()
	for _, f := range files {
		if free >= floor || ctx.Err() != nil {
			return
		}
		if a.ixPartsRemoveLocked(ctx, f.ident) {
			free += f.bytes
			ixPartsCountsOf(a).ForSpace.Add(1)
		}
	}
}

func ixSplitPartIdent(ident string) (kind, id string, size int64, ok bool) {
	f := strings.Split(ident, ":")
	if len(f) != 3 {
		return "", "", 0, false
	}
	n, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return "", "", 0, false
	}
	return f[0], f[1], n, true
}

// ixRunParts gives downloads the kept parts and sweeps them now and then.
func (a *App) ixRunParts(ctx context.Context) {
	telegram.SetKeptParts(a.ixKeptPartsFor)
	telegram.SetKeptPartsUsed(a.ixKeptPartsUsed)
	defer telegram.SetKeptParts(nil)
	defer telegram.SetKeptPartsUsed(nil)
	wait := 3 * time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = ixPartsSweepEvery
		if err := a.ixPartsSweep(ctx); err != nil && ctx.Err() == nil {
			a.ixLog("parts kept from checks: %v", err)
		}
	}
}

// ixPartsDiag is the report's line on the kept parts: numbers only.
func (a *App) ixPartsDiag(ctx context.Context) string {
	var files, bytes int64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(DISTINCT ident), COALESCE(SUM(bytes),0) FROM ix_part_store`).Scan(&files, &bytes)
	reused, short := telegram.KeptPartStats()
	t := ixPartsCountsOf(a)
	free := "unknown"
	if n, ok := ixPartsFree(a.dataDir); ok {
		free = fmt.Sprintf("%d GB", n>>30)
	}
	return fmt.Sprintf("  parts kept from checks: reused by downloads %d MB since start, in %d downloads (refused, fetched again: %d wrong length, %d wrong bytes, %d unreadable) · kept now %d MB of %d files (at most %d MB; none kept under %d GB free, %s free now) · since start kept %d MB, not kept %d for disk space, %d failed writes, %d files removed (%d to free disk space)",
		reused>>20, t.Downloads.Load(), short, t.BadBytes.Load(), t.Missing.Load(), bytes>>20, files, ixPartsCap>>20, a.ixPartsFloor(ctx)>>30, free, t.KeptBytes.Load()>>20, t.LowDisk.Load(), t.Failed.Load(), t.Removed.Load(), t.ForSpace.Load())
}
