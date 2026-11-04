package download

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// Local staging: download to a folder on this computer first, then copy to
// the library (often a NAS share), so a network hiccup on the share cannot
// fail a download that Telegram already sent.
//
// A staged file is written as a local .part, checked (size and SHA-256, as
// for a direct download) and kept with a journal row (tgdl_stage_moves).
// The mover then copies it to a .part next to its final place in the
// library, reads the copy back to check it, publishes it without ever
// replacing an existing file, and catalogues it. Only then is the local
// copy removed. Failed copies are retried with growing pauses for as long
// as it takes; the journal survives restarts. When anything does not add
// up, the local file is kept and reported, never deleted.
//
// With no staging folder set, nothing here runs and downloads go straight
// to the library exactly as before.

// Staging is the staging setting.
type Staging struct {
	Dir string // a local folder; empty: off
	Cap int64  // bytes the staged files may take; 0: DefaultStageCap
}

// DefaultStageCap is the local space staged files may take by default.
const DefaultStageCap int64 = 20 << 30

// StageFolder is the folder made inside the chosen one for staged files.
const StageFolder = "tgdl-staging"

const stageSchema = `
CREATE TABLE IF NOT EXISTS tgdl_stage_moves ( -- downloads waiting in the local staging folder
	path          TEXT PRIMARY KEY NOT NULL, -- final place, relative to the library
	stage_dir     TEXT NOT NULL,             -- the local folder holding it
	item          TEXT NOT NULL,
	generation    INTEGER NOT NULL,
	identity      TEXT NOT NULL,
	sha256        TEXT NOT NULL,
	size          INTEGER NOT NULL,
	state         TEXT NOT NULL DEFAULT 'waiting', -- waiting, or stuck: needs a person
	attempts      INTEGER NOT NULL DEFAULT 0,
	next_at       INTEGER NOT NULL DEFAULT 0,
	failing_since INTEGER NOT NULL DEFAULT 0,
	error         TEXT NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS tgdl_stage_moves_identity ON tgdl_stage_moves(identity);
CREATE TABLE IF NOT EXISTS tgdl_stage_followers ( -- other messages with the same file, catalogued when it arrives
	path       TEXT NOT NULL,
	group_id   TEXT NOT NULL,
	message_id INTEGER NOT NULL,
	item       TEXT NOT NULL,
	generation INTEGER NOT NULL,
	PRIMARY KEY (path, group_id, message_id)
);`

type stageState struct {
	known    atomic.Bool // the journal exists, so files may be waiting
	mu       sync.Mutex
	reserved int64 // staged downloads in progress
	signal   chan struct{}
	moving   sync.Mutex // one mover at a time
}

// stageDoubt is a problem a retry cannot fix: the local file is kept and
// the move waits for a person.
type stageDoubt struct{ msg string }

func (e stageDoubt) Error() string { return e.msg }

func (l *Library) stagingFor(ctx context.Context, item Item) Staging {
	if l.Staging == nil || item.NoStage {
		return Staging{}
	}
	st := l.Staging(ctx)
	st.Dir = strings.TrimSpace(st.Dir)
	if st.Cap <= 0 {
		st.Cap = DefaultStageCap
	}
	return st
}

func (l *Library) ensureStageTable(ctx context.Context) error {
	if l.stage.known.Load() {
		return nil
	}
	if _, err := l.writer.ExecContext(ctx, stageSchema); err != nil {
		return err
	}
	l.stage.known.Store(true)
	return nil
}

// stageWait is closed at the next change: a file moved, or room freed.
func (l *Library) stageWait() <-chan struct{} {
	l.stage.mu.Lock()
	defer l.stage.mu.Unlock()
	if l.stage.signal == nil {
		l.stage.signal = make(chan struct{})
	}
	return l.stage.signal
}

func (l *Library) stageSignal() {
	l.stage.mu.Lock()
	defer l.stage.mu.Unlock()
	if l.stage.signal != nil {
		close(l.stage.signal)
	}
	l.stage.signal = make(chan struct{})
}

func (l *Library) reserveStage(size int64) func() {
	l.stage.mu.Lock()
	l.stage.reserved += size
	l.stage.mu.Unlock()
	return func() {
		l.stage.mu.Lock()
		l.stage.reserved -= size
		l.stage.mu.Unlock()
		l.stageSignal()
	}
}

// stageUsed is the local space staged files take or will take.
func (l *Library) stageUsed(ctx context.Context) int64 {
	l.stage.mu.Lock()
	used := l.stage.reserved
	l.stage.mu.Unlock()
	if l.stage.known.Load() {
		var n int64
		_ = l.reader.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM tgdl_stage_moves`).Scan(&n)
		used += n
	}
	return used
}

// WaitStageRoom waits, when staging is on and the staged files already take
// the space allowed, until moves to the library make room for size bytes.
// One file is always let through so a file larger than the cap still goes.
func (l *Library) WaitStageRoom(ctx context.Context, size int64) error {
	for {
		st := l.stagingFor(ctx, Item{})
		if st.Dir == "" {
			return nil
		}
		used := l.stageUsed(ctx)
		if used == 0 || used+size <= st.Cap {
			return nil
		}
		wake := l.stageWait()
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func stageRootFor(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	root := filepath.Join(abs, StageFolder)
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", fmt.Errorf("staging folder: %w", err)
	}
	return root, nil
}

// ingestStaged downloads item into the staging folder. It runs inside
// Ingest, with the message and identity reserved.
func (l *Library) ingestStaged(ctx context.Context, item Item, client Client, st Staging) (Record, error) {
	if err := l.ensureStageTable(ctx); err != nil {
		return Record{}, err
	}
	dir, name, err := destinationName(item)
	if err != nil {
		return Record{}, err
	}
	relative := filepath.Join(dir, name)
	stageRoot, err := stageRootFor(st.Dir)
	if err != nil {
		return Record{}, err
	}
	release := l.reserveStage(item.Identity.Size)
	defer release()
	payload, err := json.Marshal(item)
	if err != nil {
		return Record{}, err
	}
	digest := sha256.New()
	m := NewManager(hashClient{client, digest}, telegram.NewDedupIndex())
	journaled := false
	m.prepared = func() error {
		// Written before the local file is published, so a restart finds it.
		_, err := l.writer.ExecContext(ctx, `INSERT INTO tgdl_stage_moves(path,stage_dir,item,generation,identity,sha256,size,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			filepath.ToSlash(relative), stageRoot, string(payload), item.generation, item.Identity.Key(), hex.EncodeToString(digest.Sum(nil)), item.Identity.Size, time.Now().UnixMilli())
		journaled = err == nil
		return err
	}
	root, err := os.OpenRoot(stageRoot)
	if err != nil {
		return Record{}, err
	}
	defer root.Close()
	if _, err = m.DownloadInRoot(ctx, item.Identity, root, relative); err != nil {
		if journaled {
			// Publication failed after the journal row: nothing is left locally.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, statErr := root.Stat(relative); errors.Is(statErr, fs.ErrNotExist) {
				_, _ = l.writer.ExecContext(cleanupCtx, `DELETE FROM tgdl_stage_moves WHERE path=?`, filepath.ToSlash(relative))
			}
		}
		return Record{}, err
	}
	sum := hex.EncodeToString(digest.Sum(nil))
	contentRelease, err := l.enter(ctx, "sha256:"+sum)
	if err != nil {
		l.stageSignal()
		return Record{Path: filepath.ToSlash(relative), SHA256: sum, Staged: true}, nil
	}
	defer contentRelease()
	// The same bytes already in the library: catalogue those and drop the
	// local copy, as a direct download does. A failed look-up only means
	// the file is moved like any other.
	if record, found, err := l.existing(ctx, item, `d.file_hash=?`, sum); err == nil && found {
		l.dropStaged(ctx, stageRoot, relative)
		return record, nil
	}
	if record, found, err := l.reuse(ctx, item, `d.file_hash=? AND d.file_size=?`, sum, item.Identity.Size); err == nil && found {
		l.dropStaged(ctx, stageRoot, relative)
		return record, nil
	}
	l.stageSignal()
	return Record{Path: filepath.ToSlash(relative), SHA256: sum, Staged: true}, nil
}

// dropStaged removes a staged file the library no longer needs (it holds
// the same bytes already, or a newer version replaced the message).
func (l *Library) dropStaged(ctx context.Context, stageRoot, relative string) {
	ctx = context.WithoutCancel(ctx)
	if err := os.Remove(filepath.Join(stageRoot, relative)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return
	}
	_, _ = l.writer.ExecContext(ctx, `DELETE FROM tgdl_stage_followers WHERE path=?`, filepath.ToSlash(relative))
	_, _ = l.writer.ExecContext(ctx, `DELETE FROM tgdl_stage_moves WHERE path=?`, filepath.ToSlash(relative))
	l.stageSignal()
}

// followStaged: when this item's file is already waiting in the staging
// folder for another message, this message is catalogued with it when it
// arrives instead of downloading it again.
func (l *Library) followStaged(ctx context.Context, item Item) (Record, bool, error) {
	if !l.stage.known.Load() {
		return Record{}, false, nil
	}
	var path, sum string
	err := l.reader.QueryRowContext(ctx, `SELECT path, sha256 FROM tgdl_stage_moves WHERE identity=? AND size=? AND state='waiting' ORDER BY created_at LIMIT 1`, item.Identity.Key(), item.Identity.Size).Scan(&path, &sum)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return Record{}, false, err
	}
	if _, err = l.writer.ExecContext(ctx, `INSERT INTO tgdl_stage_followers(path,group_id,message_id,item,generation) VALUES(?,?,?,?,?)
		ON CONFLICT(path,group_id,message_id) DO UPDATE SET item=excluded.item, generation=excluded.generation`, path, item.GroupID, item.MessageID, string(payload), item.generation); err != nil {
		return Record{}, false, err
	}
	return Record{Path: path, SHA256: sum, Reused: true, Staged: true}, true, nil
}

type stagedMove struct {
	path, stageDir, payload, sum string
	generation, size             int64
	attempts                     int
}

// RunStaging moves staged files to the library until ctx ends.
func (l *Library) RunStaging(ctx context.Context) {
	for ctx.Err() == nil {
		wake := l.stageWait()
		delay := 30 * time.Second
		if next := l.MoveStaged(ctx); next > 0 && next < delay {
			delay = next
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
		case <-wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// MoveStaged moves the staged files that are due, one after another, and
// says when the next one is due (0: none waiting).
func (l *Library) MoveStaged(ctx context.Context) time.Duration {
	if !l.stage.known.Load() {
		return 0
	}
	l.stage.moving.Lock()
	defer l.stage.moving.Unlock()
	now := time.Now().UnixMilli()
	rows, err := l.reader.QueryContext(ctx, `SELECT path,stage_dir,item,generation,sha256,size,attempts FROM tgdl_stage_moves WHERE state='waiting' AND next_at<=? ORDER BY created_at LIMIT 50`, now)
	if err != nil {
		return time.Minute
	}
	var due []stagedMove
	for rows.Next() {
		var mv stagedMove
		if rows.Scan(&mv.path, &mv.stageDir, &mv.payload, &mv.generation, &mv.sum, &mv.size, &mv.attempts) == nil {
			due = append(due, mv)
		}
	}
	rows.Close()
	for _, mv := range due {
		if ctx.Err() != nil {
			return 0
		}
		if err := l.moveStaged(ctx, mv); err != nil && ctx.Err() == nil {
			l.stageFailed(ctx, mv, err)
		}
	}
	var next sql.NullInt64
	_ = l.reader.QueryRowContext(ctx, `SELECT MIN(next_at) FROM tgdl_stage_moves WHERE state='waiting'`).Scan(&next)
	if !next.Valid {
		return 0
	}
	return max(time.Until(time.UnixMilli(next.Int64)), time.Second)
}

// stageBackoff is the pause before the next try: 5 s, doubling, at most 10 min.
func stageBackoff(attempts int) time.Duration {
	return min(5*time.Second<<min(attempts, 10), 10*time.Minute)
}

func (l *Library) stageFailed(ctx context.Context, mv stagedMove, cause error) {
	ctx = context.WithoutCancel(ctx)
	now := time.Now()
	var doubt stageDoubt
	if errors.As(cause, &doubt) {
		_, _ = l.writer.ExecContext(ctx, `UPDATE tgdl_stage_moves SET state='stuck', error=?, failing_since=CASE WHEN failing_since=0 THEN ? ELSE failing_since END WHERE path=?`, cause.Error(), now.UnixMilli(), mv.path)
		return
	}
	_, _ = l.writer.ExecContext(ctx, `UPDATE tgdl_stage_moves SET attempts=attempts+1, next_at=?, error=?, failing_since=CASE WHEN failing_since=0 THEN ? ELSE failing_since END WHERE path=?`,
		now.Add(stageBackoff(mv.attempts)).UnixMilli(), cause.Error(), now.UnixMilli(), mv.path)
}

func hashFile(ctx context.Context, f io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, &contextReader{ctx: ctx, r: f})
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// moveStaged copies one staged file to the library and catalogues it.
func (l *Library) moveStaged(ctx context.Context, mv stagedMove) error {
	var item Item
	if err := json.Unmarshal([]byte(mv.payload), &item); err != nil {
		return stageDoubt{"the staging journal entry cannot be read: " + err.Error()}
	}
	item.generation = mv.generation
	local := filepath.Join(mv.stageDir, filepath.FromSlash(mv.path))
	followers, err := l.stageFollowers(ctx, mv.path)
	if err != nil {
		return err
	}
	if !l.stageCurrent(ctx, item) && len(followers) == 0 {
		// A newer version of the message came meanwhile: nothing wants this file.
		l.dropStaged(ctx, mv.stageDir, filepath.FromSlash(mv.path))
		return nil
	}
	lf, err := os.Open(local)
	if errors.Is(err, fs.ErrNotExist) {
		// Already in the library (stopped after the copy was catalogued)?
		if l.stagedArrived(ctx, mv) {
			_, _ = l.writer.ExecContext(context.WithoutCancel(ctx), `DELETE FROM tgdl_stage_moves WHERE path=?`, mv.path)
			l.stageSignal()
			return nil
		}
		return stageDoubt{"the staged copy is missing from the staging folder"}
	}
	if err != nil {
		return stageDoubt{"the staged copy cannot be opened: " + err.Error()}
	}
	info, err := lf.Stat()
	if err != nil || info.Size() != mv.size {
		lf.Close()
		return stageDoubt{fmt.Sprintf("the staged copy has %d bytes, %d were downloaded", info.Size(), mv.size)}
	}
	relative := filepath.FromSlash(mv.path)
	dir, name := filepath.Dir(relative), filepath.Base(relative)
	if err := l.ensureDir(dir); err != nil {
		lf.Close()
		return fmt.Errorf("library folder: %w", err)
	}
	root, err := os.OpenRoot(l.root)
	if err != nil {
		lf.Close()
		return err
	}
	defer root.Close()
	parent, err := root.OpenRoot(dir)
	if err != nil {
		lf.Close()
		return err
	}
	defer parent.Close()
	part := partName(name)
	// Copy, hashing the local bytes as they are read.
	out, err := parent.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		lf.Close()
		return fmt.Errorf("write to the library: %w", err)
	}
	h := sha256.New()
	_, err = io.Copy(out, io.TeeReader(&contextReader{ctx: ctx, r: lf}, h))
	lf.Close()
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = parent.Remove(part)
		return fmt.Errorf("copy to the library: %w", err)
	}
	if hex.EncodeToString(h.Sum(nil)) != mv.sum {
		_ = parent.Remove(part)
		return stageDoubt{"the staged copy no longer matches what was downloaded; it is kept for inspection"}
	}
	// Read the copy back.
	if err := verifyCopy(ctx, parent, part, mv); err != nil {
		_ = parent.Remove(part)
		return err
	}
	if l.StageHold != nil {
		done, err := l.StageHold(ctx)
		if err != nil {
			_ = parent.Remove(part)
			return err
		}
		defer done()
	}
	dirFile, err := parent.Open(".")
	if err != nil {
		_ = parent.Remove(part)
		return err
	}
	err = publishExclusiveAt(dirFile, part, name)
	dirFile.Close()
	if errors.Is(err, os.ErrExist) {
		// Never replace a file in the library. The same bytes (an earlier
		// try that stopped after publishing) are used; other bytes get a
		// new unique name.
		same, checkErr := sameFile(ctx, parent, name, mv)
		if checkErr != nil {
			_ = parent.Remove(part)
			return checkErr
		}
		if same {
			_ = parent.Remove(part)
		} else {
			_, fresh, nameErr := destinationName(item)
			if nameErr != nil {
				_ = parent.Remove(part)
				return nameErr
			}
			if dirFile, err = parent.Open("."); err != nil {
				_ = parent.Remove(part)
				return err
			}
			err = publishExclusiveAt(dirFile, part, fresh)
			dirFile.Close()
			if err != nil {
				_ = parent.Remove(part)
				return fmt.Errorf("publish in the library: %w", err)
			}
			// The local copy and its journal entry follow the new name.
			newLocal := filepath.Join(mv.stageDir, dir, fresh)
			if err = os.Rename(local, newLocal); err != nil {
				return err
			}
			if err = l.renameStaged(ctx, mv, filepath.Join(dir, fresh)); err != nil {
				_ = os.Rename(newLocal, local)
				return err
			}
			local, relative = newLocal, filepath.Join(dir, fresh)
			mv.path = filepath.ToSlash(relative)
		}
	} else if err != nil {
		_ = parent.Remove(part)
		return fmt.Errorf("publish in the library: %w", err)
	}
	// Catalogue it. A failure here is retried; the next try finds the
	// published file with the same bytes and catalogues it.
	f, finfo, err := l.open(relative)
	if err != nil {
		return err
	}
	fingerprint, err := fileFingerprint(f, finfo)
	f.Close()
	if err != nil {
		return err
	}
	var records []stagedRecord
	media := candidate{path: relative, sha256: mv.sum, info: finfo, fingerprint: fingerprint}
	record, err := l.register(ctx, item, media, false)
	switch {
	case err == nil:
		records = append(records, stagedRecord{item, record})
	case errors.Is(err, ErrSuperseded):
	default:
		return err
	}
	for _, fw := range followers {
		if len(records) == 0 {
			// The first message was replaced meanwhile: the next one owns the file.
			rec, e := l.register(ctx, fw, media, false)
			if e == nil {
				records = append(records, stagedRecord{fw, rec})
			} else if !errors.Is(e, ErrSuperseded) {
				return e
			}
			continue
		}
		if rec, found, e := l.existing(ctx, fw, `d.file_path=?`, mv.path); e == nil && found {
			records = append(records, stagedRecord{fw, rec})
			continue
		}
		rec, found, e := l.reuse(ctx, fw, `d.file_path=?`, mv.path)
		if e != nil && !errors.Is(e, ErrSuperseded) {
			return e
		}
		if found {
			records = append(records, stagedRecord{fw, rec})
		}
	}
	// In the library: the local copy can go, then its journal entry.
	if err := os.Remove(local); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the staged copy: %w", err)
	}
	bg := context.WithoutCancel(ctx)
	_, _ = l.writer.ExecContext(bg, `DELETE FROM tgdl_stage_followers WHERE path=?`, mv.path)
	_, _ = l.writer.ExecContext(bg, `DELETE FROM tgdl_stage_moves WHERE path=?`, mv.path)
	l.stageSignal()
	if l.OnStaged != nil {
		for _, r := range records {
			l.OnStaged(bg, r.item, r.record)
		}
	}
	return nil
}

type stagedRecord struct {
	item   Item
	record Record
}

func verifyCopy(ctx context.Context, parent *os.Root, name string, mv stagedMove) error {
	f, err := parent.Open(name)
	if err != nil {
		return fmt.Errorf("read back the copy: %w", err)
	}
	sum, n, err := hashFile(ctx, f)
	f.Close()
	if err != nil {
		return fmt.Errorf("read back the copy: %w", err)
	}
	if n != mv.size || sum != mv.sum {
		return errors.New("the copy in the library did not read back the same; trying again")
	}
	return nil
}

// sameFile reports whether the library file name holds exactly mv's bytes.
func sameFile(ctx context.Context, parent *os.Root, name string, mv stagedMove) (bool, error) {
	f, err := parent.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != mv.size {
		return false, nil
	}
	sum, _, err := hashFile(ctx, f)
	if err != nil {
		return false, err
	}
	return sum == mv.sum, nil
}

func (l *Library) renameStaged(ctx context.Context, mv stagedMove, relative string) error {
	tx, err := l.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	to := filepath.ToSlash(relative)
	if _, err = tx.ExecContext(ctx, `UPDATE tgdl_stage_moves SET path=? WHERE path=?`, to, mv.path); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tgdl_stage_followers SET path=? WHERE path=?`, to, mv.path); err != nil {
		return err
	}
	return tx.Commit()
}

func (l *Library) stageCurrent(ctx context.Context, item Item) bool {
	var current int64
	if err := l.reader.QueryRowContext(ctx, `SELECT generation FROM tgdl_message_generations WHERE group_id=? AND message_id=?`, item.GroupID, item.MessageID).Scan(&current); err != nil {
		return true // unknown: move it anyway, registration decides
	}
	return current == item.generation
}

func (l *Library) stageFollowers(ctx context.Context, path string) ([]Item, error) {
	rows, err := l.reader.QueryContext(ctx, `SELECT item, generation FROM tgdl_stage_followers WHERE path=?`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var payload string
		var gen int64
		if err := rows.Scan(&payload, &gen); err != nil {
			return nil, err
		}
		var it Item
		if json.Unmarshal([]byte(payload), &it) == nil {
			it.generation = gen
			out = append(out, it)
		}
	}
	return out, rows.Err()
}

// stagedArrived reports whether mv's file is catalogued in the library
// with its bytes.
func (l *Library) stagedArrived(ctx context.Context, mv stagedMove) bool {
	var n int
	if l.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM downloads WHERE file_path=? AND lower(file_hash)=?`, mv.path, mv.sum).Scan(&n) != nil || n == 0 {
		return false
	}
	f, info, err := l.open(mv.path)
	if err != nil {
		return false
	}
	defer f.Close()
	if info.Size() != mv.size {
		return false
	}
	sum, _, err := hashFile(ctx, f)
	return err == nil && sum == mv.sum
}

// recoverStaging runs at start-up: a download stopped just before its local
// file was published is finished from its .part when the bytes check out,
// and leftover partial downloads in the staging folder are removed (the
// download queue fetches those again, as for direct downloads).
func (l *Library) recoverStaging(ctx context.Context) error {
	var exists int
	if err := l.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='tgdl_stage_moves'`).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		if err := l.ensureStageTable(ctx); err != nil {
			return err
		}
	}
	keep := map[string]bool{}
	roots := map[string]bool{}
	if exists > 0 {
		rows, err := l.reader.QueryContext(ctx, `SELECT path, stage_dir, sha256, size FROM tgdl_stage_moves`)
		if err != nil {
			return err
		}
		type entry struct {
			path, dir, sum string
			size           int64
		}
		var list []entry
		for rows.Next() {
			var e entry
			if rows.Scan(&e.path, &e.dir, &e.sum, &e.size) == nil {
				list = append(list, e)
			}
		}
		rows.Close()
		for _, e := range list {
			roots[e.dir] = true
			local := filepath.Join(e.dir, filepath.FromSlash(e.path))
			part := filepath.Join(filepath.Dir(local), partName(filepath.Base(local)))
			keep[part] = true
			if _, err := os.Stat(local); err == nil || !errors.Is(err, fs.ErrNotExist) {
				continue
			}
			f, err := os.Open(part)
			if err != nil {
				continue // the mover reports it
			}
			sum, n, err := hashFile(ctx, f)
			f.Close()
			if err == nil && n == e.size && sum == e.sum {
				_ = os.Rename(part, local)
			}
		}
	}
	if l.Staging != nil {
		if st := l.stagingFor(ctx, Item{}); st.Dir != "" {
			if abs, err := filepath.Abs(st.Dir); err == nil {
				roots[filepath.Join(abs, StageFolder)] = true
			}
		}
	}
	for root := range roots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".tgdl-") && strings.HasSuffix(name, ".part") && !keep[path] {
				_ = os.Remove(path)
			}
			return nil
		})
	}
	return nil
}

// StagingStatus is what the staging folder holds, for the dashboard and
// the diagnostics report.
type StagingStatus struct {
	Waiting      int      `json:"waiting"` // files waiting to be copied to the library
	Bytes        int64    `json:"bytes"`
	Failing      int      `json:"failing"` // of those, the last copy failed
	FailingSince int64    `json:"failingSince,omitempty"`
	Stuck        int      `json:"stuck"` // need a person; their local files are kept
	Errors       []string `json:"errors,omitempty"`
}

func (l *Library) StagingStatus(ctx context.Context) StagingStatus {
	st := StagingStatus{}
	if !l.stage.known.Load() {
		return st
	}
	_ = l.reader.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(size),0), COALESCE(SUM(error<>'' AND state='waiting'),0), COALESCE(SUM(state='stuck'),0), COALESCE(MIN(CASE WHEN failing_since>0 THEN failing_since END),0) FROM tgdl_stage_moves`).
		Scan(&st.Waiting, &st.Bytes, &st.Failing, &st.Stuck, &st.FailingSince)
	rows, err := l.reader.QueryContext(ctx, `SELECT error, COUNT(*) FROM tgdl_stage_moves WHERE error<>'' GROUP BY error ORDER BY 2 DESC LIMIT 5`)
	if err == nil {
		for rows.Next() {
			var e string
			var n int
			if rows.Scan(&e, &n) == nil {
				st.Errors = append(st.Errors, fmt.Sprintf("%d× %s", n, e))
			}
		}
		rows.Close()
	}
	return st
}
