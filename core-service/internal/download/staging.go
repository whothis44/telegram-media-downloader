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

	"github.com/botnick/telegram-media-downloader/core-service/internal/diskfree"
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
	waiters  int   // downloads waiting for room now
	// Downloads sent straight to the library because staging was full
	// (ix.12.5), and when the last one went.
	bypassed   int
	bypassedAt int64
	fullSince  int64 // when they started waiting (Unix ms), 0: none waits
	signal     chan struct{}
	moving     sync.Mutex // one mover at a time
}

// stageFreeReserve is the free space the staging disk keeps when downloads
// go on past the space allowed (see WaitStageRoom).
const stageFreeReserve int64 = 50 << 30

// stageFreeHook replaces diskfree.Free in tests.
var stageFreeHook atomic.Pointer[func(string) (int64, bool)]

// stageFreeSpace is the free space on the disk holding dir.
func stageFreeSpace(dir string) (int64, bool) {
	if f := stageFreeHook.Load(); f != nil {
		return (*f)(dir)
	}
	return diskfree.Free(dir)
}

// errStageLater: the move waits for another download of the same message
// or file to finish. Not a failure; it is tried again shortly.
var errStageLater = errors.New("waiting for another download of the same message")

// errStageGone: the journal entry went while the move ran (its group was
// purged). Nothing is left to do: the purge removes the local file.
var errStageGone = errors.New("the staged file was removed from the journal")

// stageDoubt is a problem a retry cannot fix: the local file is kept and
// the move waits for a person.
type stageDoubt struct{ msg string }

func (e stageDoubt) Error() string { return e.msg }

func (l *Library) stagingFor(ctx context.Context, item Item) Staging {
	if l.Staging == nil || item.NoStage || ctx.Value(noStageKey{}) != nil {
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

// stageUsed is the local space staged files take or will take. Files that
// need a person (stuck) stay on disk but do not count: they would otherwise
// stop every download until someone looks (StagingStatus reports them).
func (l *Library) stageUsed(ctx context.Context) int64 {
	l.stage.mu.Lock()
	used := l.stage.reserved
	l.stage.mu.Unlock()
	if l.stage.known.Load() {
		var n int64
		_ = l.reader.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM tgdl_stage_moves WHERE state='waiting'`).Scan(&n)
		used += n
	}
	return used
}

// stageRoomOnDisk: past the space allowed, a download still goes while the
// staging disk keeps plenty free after it (as much again as the space
// allowed, and at least stageFreeReserve).
func stageRoomOnDisk(st Staging, size int64) bool {
	free, ok := stageFreeSpace(st.Dir)
	return ok && free-size >= max(st.Cap, stageFreeReserve)
}

func (l *Library) stageWaiting(delta int) {
	l.stage.mu.Lock()
	defer l.stage.mu.Unlock()
	l.stage.waiters += delta
	switch {
	case l.stage.waiters <= 0:
		l.stage.waiters, l.stage.fullSince = 0, 0
	case l.stage.fullSince == 0:
		l.stage.fullSince = time.Now().UnixMilli()
	}
}

type noStageKey struct{}

// WithoutStaging has the download go straight to the library, staging on or
// not (ix.12.5: when the staging folder is full).
func WithoutStaging(ctx context.Context) context.Context {
	return context.WithValue(ctx, noStageKey{}, true)
}

// StagingSkipped says whether ctx sends downloads straight to the library.
func StagingSkipped(ctx context.Context) bool { return ctx.Value(noStageKey{}) != nil }

// StageRoom says whether a download of size bytes may go to the staging
// folder now: staging is off (true: nothing to wait for), or the staged
// files leave room (the rules of WaitStageRoom).
func (l *Library) StageRoom(ctx context.Context, size int64) bool {
	st := l.stagingFor(ctx, Item{})
	if st.Dir == "" {
		return true
	}
	used := l.stageUsed(ctx)
	return used == 0 || used+size <= st.Cap || stageRoomOnDisk(st, size)
}

// StageBypass records a download sent straight to the library because the
// staging folder was full (StagingStatus: Bypassed, BypassedAt).
func (l *Library) StageBypass() {
	l.stage.mu.Lock()
	defer l.stage.mu.Unlock()
	l.stage.bypassed++
	l.stage.bypassedAt = time.Now().UnixMilli()
}

// WaitStageRoom waits, when staging is on and the staged files already take
// the space allowed, until moves to the library make room for size bytes.
// One file is always let through so a file larger than the cap still goes,
// and downloads go on while the disk has plenty of free space. While any
// download waits, StagingStatus says so (Full), for the dashboard and alerts.
func (l *Library) WaitStageRoom(ctx context.Context, size int64) error {
	waiting := false
	defer func() {
		if waiting {
			l.stageWaiting(-1)
		}
	}()
	for {
		st := l.stagingFor(ctx, Item{})
		if st.Dir == "" {
			return nil
		}
		used := l.stageUsed(ctx)
		if used == 0 || used+size <= st.Cap || stageRoomOnDisk(st, size) {
			return nil
		}
		if !waiting {
			waiting = true
			l.stageWaiting(1)
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
	stageRoot, err := stageRootFor(st.Dir)
	if err != nil {
		return Record{}, err
	}
	// A part kept by an earlier attempt in this folder goes on under its own
	// name, as in the library (resume.go).
	resuming := canResume(client, item.Identity.Size)
	var progress partRow
	if resuming {
		progress = l.partLoad(ctx, item, stageRoot)
	}
	relative := filepath.FromSlash(progress.path)
	if progress.path == "" {
		dir, name, err := destinationName(item)
		if err != nil {
			return Record{}, err
		}
		relative = filepath.Join(dir, name)
	}
	release := l.reserveStage(item.Identity.Size)
	defer release()
	payload, err := json.Marshal(item)
	if err != nil {
		return Record{}, err
	}
	digest := sha256.New()
	m := NewManager(hashClient{client, digest}, telegram.NewDedupIndex())
	if resuming {
		m.resume = l.partResumeFor(item, filepath.ToSlash(relative), stageRoot, progress, digest)
	}
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
// the same bytes already, or a newer version of the message is catalogued).
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
	var path, sum, dir string
	err := l.reader.QueryRowContext(ctx, `SELECT path, sha256, stage_dir FROM tgdl_stage_moves WHERE identity=? AND size=? AND state='waiting' ORDER BY created_at LIMIT 1`, item.Identity.Key(), item.Identity.Size).Scan(&path, &sum, &dir)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	// A staged file found missing is being queued again (stagedLost): this
	// message is downloaded on its own instead of waiting for it.
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(path))); errors.Is(err, fs.ErrNotExist) {
		return Record{}, false, nil
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
	path, stageDir, payload, sum, identity string
	generation, size                       int64
	attempts                               int
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
	rows, err := l.reader.QueryContext(ctx, `SELECT path,stage_dir,item,generation,sha256,size,attempts,identity FROM tgdl_stage_moves WHERE state='waiting' AND next_at<=? ORDER BY created_at LIMIT 50`, now)
	if err != nil {
		return time.Minute
	}
	var due []stagedMove
	for rows.Next() {
		var mv stagedMove
		if rows.Scan(&mv.path, &mv.stageDir, &mv.payload, &mv.generation, &mv.sum, &mv.size, &mv.attempts, &mv.identity) == nil {
			due = append(due, mv)
		}
	}
	rows.Close()
	for _, mv := range due {
		if ctx.Err() != nil {
			return 0
		}
		err := l.moveStaged(ctx, mv)
		switch {
		case err == nil || ctx.Err() != nil:
		case errors.Is(err, errStageGone):
		case errors.Is(err, errStageLater):
			_, _ = l.writer.ExecContext(context.WithoutCancel(ctx), `UPDATE tgdl_stage_moves SET next_at=? WHERE path=?`, time.Now().Add(stageLaterPause).UnixMilli(), mv.path)
		default:
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
	// What becomes of the file is decided with the locks of its file and its
	// messages held (no download of them runs meanwhile), once before the
	// copy and again just before it is catalogued. The locks are not held
	// during the copy, and StageHold is never waited for while they are: a
	// download holds the app's media lock while it waits for its message.
	locks := stageLocks{l: l, held: map[string]func(){}}
	defer locks.release()
	d, err := l.stageDecide(ctx, &locks, item, mv)
	if err != nil {
		return err
	}
	if !d.wanted {
		// A newer version of each message is in the catalogue: nothing wants this file.
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
		// Moved to the library by hand, at its own path (ix.12.6): the
		// journal's size and SHA-256 say it is the same file.
		if l.stagedInLibrary(ctx, mv) {
			if record, err := l.register(ctx, item, l.stagedCandidate(mv), false); err == nil {
				_, _ = l.writer.ExecContext(context.WithoutCancel(ctx), `DELETE FROM tgdl_stage_moves WHERE path=?`, mv.path)
				if l.OnStaged != nil {
					l.OnStaged(context.WithoutCancel(ctx), item, record)
				}
				l.stageSignal()
				return nil
			}
		}
		// Removed by someone or something else: its messages go back in the queue.
		return l.stagedLost(ctx, mv, append([]Item{item}, d.followers...), append([]stageFate{d.fate}, d.fates...))
	}
	locks.release()
	if err != nil {
		return stageDoubt{"the staged copy cannot be opened: " + err.Error()}
	}
	info, err := lf.Stat()
	if err != nil {
		lf.Close()
		return fmt.Errorf("the staged copy cannot be read: %w", err)
	}
	if info.Size() != mv.size {
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
	// Decide again: a download of one of its messages may have run during
	// the copy. The locks are held from here until it is catalogued.
	if d, err = l.stageDecide(ctx, &locks, item, mv); err != nil || !d.wanted {
		_ = parent.Remove(part)
		if err == nil {
			l.dropStaged(ctx, mv.stageDir, filepath.FromSlash(mv.path))
		}
		return err
	}
	fate, followers, fates := d.fate, d.followers, d.fates
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
	if fate != stageReplaced {
		if fate == stageAdopt {
			if err := l.adoptStaged(ctx, &item); err != nil {
				return err
			}
		}
		record, err := l.register(ctx, item, media, false)
		switch {
		case err == nil:
			records = append(records, stagedRecord{item, record})
		case errors.Is(err, ErrSuperseded):
		default:
			return err
		}
	}
	for i, fw := range followers {
		switch fates[i] {
		case stageReplaced, stageLater:
			continue // a newer version of that message is catalogued, or comes
		case stageAdopt:
			if err := l.adoptStaged(ctx, &fw); err != nil {
				return err
			}
		}
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
		// Followers waited for the same Telegram file (followStaged).
		if rec, found, e := l.existing(ctx, fw, `d.file_path=?`, mv.path); e == nil && found {
			records = append(records, stagedRecord{fw, tagged(rec, DedupTelegramFile)})
			continue
		}
		rec, found, e := l.reuse(ctx, fw, `d.file_path=?`, mv.path)
		if e != nil && !errors.Is(e, ErrSuperseded) {
			return e
		}
		if found {
			records = append(records, stagedRecord{fw, tagged(rec, DedupTelegramFile)})
		}
	}
	if len(records) == 0 {
		// Some message wants this file (checked above, with their locks
		// held), so nothing may be removed before one has it catalogued.
		return errors.New("the staged file was not catalogued; trying again")
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

// stageLaterPause is the wait before a move that waited for another
// download is tried again.
const stageLaterPause = 30 * time.Second

// stageLocks are the identity and message locks a move holds, taken
// without waiting.
type stageLocks struct {
	l    *Library
	held map[string]func()
}

func (s *stageLocks) take(key string) bool {
	if _, ok := s.held[key]; ok {
		return true
	}
	release, ok := s.l.tryEnter(key)
	if ok {
		s.held[key] = release
	}
	return ok
}

func (s *stageLocks) release() {
	for key, release := range s.held {
		release()
		delete(s.held, key)
	}
}

// stageDecision is what becomes of a staged file: fate for its first
// message, fates for the others (followers), and whether any still wants it.
type stageDecision struct {
	fate      stageFate
	followers []Item
	fates     []stageFate
	wanted    bool
}

// stageDecide takes the locks of the staged file and of all its messages
// without waiting (errStageLater when one is busy) and decides, with them
// held, what becomes of the file for each message.
func (l *Library) stageDecide(ctx context.Context, locks *stageLocks, item Item, mv stagedMove) (stageDecision, error) {
	var d stageDecision
	if !locks.take("identity:"+mv.identity) || !locks.take(messageKey(item)) {
		return d, errStageLater
	}
	// The journal entry as it is now: a purge may have removed it, or handed
	// the file to another message, since the mover read it.
	var payload string
	var generation int64
	err := l.reader.QueryRowContext(ctx, `SELECT item, generation FROM tgdl_stage_moves WHERE path=?`, mv.path).Scan(&payload, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return d, errStageGone
	}
	if err != nil {
		return d, err
	}
	if payload != mv.payload || generation != mv.generation {
		return d, errStageLater // read again on the next pass
	}
	followers, err := l.stageFollowers(ctx, mv.path)
	if err != nil {
		return d, err
	}
	for _, fw := range followers {
		if !locks.take(messageKey(fw)) {
			return d, errStageLater
		}
	}
	if d.fate, err = l.stageFateOf(ctx, item, mv.path); err != nil {
		return d, err
	}
	if d.fate == stageLater {
		return d, errStageLater
	}
	d.followers, d.fates = followers, make([]stageFate, len(followers))
	d.wanted = d.fate != stageReplaced
	for i, fw := range followers {
		if d.fates[i], err = l.stageFateOf(ctx, fw, mv.path); err != nil {
			return d, err
		}
		d.wanted = d.wanted || d.fates[i] == stageCurrent || d.fates[i] == stageAdopt
	}
	return d, nil
}

// stageFate is what becomes of a staged file for one of its messages, when
// that message may have been downloaded again since.
type stageFate int

const (
	stageCurrent  stageFate = iota // the message's latest download
	stageReplaced                  // a newer version is catalogued, or the message was purged: not needed for it
	stageLater                     // a newer version waits in the staging folder: that one goes first
	stageAdopt                     // newer downloads failed or stopped: this is its only copy, kept
)

// stageFateOf decides, with the message's lock held, what becomes of the
// staged file at path for item. A newer download that failed never costs
// the file: it is dropped only once a newer version is in the catalogue.
func (l *Library) stageFateOf(ctx context.Context, item Item, path string) (stageFate, error) {
	var current, registered int64
	err := l.reader.QueryRowContext(ctx, `SELECT generation, registered FROM tgdl_message_generations WHERE group_id=? AND message_id=?`, item.GroupID, item.MessageID).Scan(&current, &registered)
	if errors.Is(err, sql.ErrNoRows) {
		// No download of the message is known: its group was purged (every
		// download writes the row first). Nothing wants the file for it, and
		// it is never queued again.
		return stageReplaced, nil
	}
	if err != nil {
		return 0, err
	}
	switch {
	case current == item.generation:
		return stageCurrent, nil
	case registered > item.generation:
		return stageReplaced, nil
	}
	var newer bool
	err = l.reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tgdl_stage_moves WHERE state='waiting' AND path<>? AND generation>? AND json_extract(item,'$.GroupID')=? AND json_extract(item,'$.MessageID')=?)
		OR EXISTS(SELECT 1 FROM tgdl_stage_followers f JOIN tgdl_stage_moves m ON m.path=f.path WHERE m.state='waiting' AND f.path<>? AND f.generation>? AND f.group_id=? AND f.message_id=?)`,
		path, item.generation, item.GroupID, item.MessageID, path, item.generation, item.GroupID, item.MessageID).Scan(&newer)
	if err != nil {
		return 0, err
	}
	if newer {
		return stageLater, nil
	}
	return stageAdopt, nil
}

// adoptStaged makes item its message's latest version again, so that its
// staged file is catalogued. The message's lock is held.
func (l *Library) adoptStaged(ctx context.Context, item *Item) error {
	return l.writer.QueryRowContext(ctx, `UPDATE tgdl_message_generations SET generation=generation+1 WHERE group_id=? AND message_id=? RETURNING generation`, item.GroupID, item.MessageID).Scan(&item.generation)
}

// stagedLost handles a staged file that is gone from the staging folder
// before it reached the library (an antivirus, someone cleaning up): its
// download had already counted as done, so each message that still wants it
// is put back in the download queue, and the journal entry goes.
func (l *Library) stagedLost(ctx context.Context, mv stagedMove, items []Item, fates []stageFate) error {
	if l.StageLost == nil {
		return stageDoubt{"the staged copy is missing from the staging folder"}
	}
	for i, it := range items {
		if (fates[i] == stageCurrent || fates[i] == stageAdopt) && len(it.Requeue) == 0 {
			return stageDoubt{"the staged copy is missing from the staging folder, and its message cannot be queued again on its own: download it again"}
		}
	}
	// The other messages first, each forgotten once it is queued again, the
	// first message last: when one fails, the retry queues none twice.
	bg := context.WithoutCancel(ctx)
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if fates[i] == stageCurrent || fates[i] == stageAdopt {
			if err := l.StageLost(ctx, it); err != nil {
				return fmt.Errorf("the staged copy is missing from the staging folder; queueing its message again failed: %w", err)
			}
		}
		if i > 0 {
			if _, err := l.writer.ExecContext(bg, `DELETE FROM tgdl_stage_followers WHERE path=? AND group_id=? AND message_id=?`, mv.path, it.GroupID, it.MessageID); err != nil {
				return err
			}
		}
	}
	_, _ = l.writer.ExecContext(bg, `DELETE FROM tgdl_stage_followers WHERE path=?`, mv.path)
	_, _ = l.writer.ExecContext(bg, `DELETE FROM tgdl_stage_moves WHERE path=?`, mv.path)
	l.stageSignal()
	return nil
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
// stagedInLibrary: the library holds mv's file at mv's path with the
// journal's size and SHA-256 (moved there by hand).
func (l *Library) stagedInLibrary(ctx context.Context, mv stagedMove) bool {
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

// stagedCandidate is mv's file in the library, for register.
func (l *Library) stagedCandidate(mv stagedMove) candidate {
	c := candidate{path: mv.path, sha256: mv.sum}
	if f, info, err := l.open(mv.path); err == nil {
		c.info = info
		c.fingerprint, _ = fileFingerprint(f, info)
		f.Close()
	}
	return c
}

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
// download queue fetches those again, as for direct downloads), except those
// with progress recorded (resume.go), which the next attempt checks and carries
// on. partSweep has already removed the records nothing comes back for.
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
	if kept, err := l.reader.QueryContext(ctx, `SELECT stage_dir, path FROM tgdl_part_progress WHERE stage_dir<>''`); err == nil {
		for kept.Next() {
			var dir, path string
			if kept.Scan(&dir, &path) == nil && filepath.IsLocal(filepath.FromSlash(path)) {
				local := filepath.Join(dir, filepath.FromSlash(path))
				keep[filepath.Join(filepath.Dir(local), partName(filepath.Base(local)))] = true
				roots[dir] = true
			}
		}
		kept.Close()
	} else {
		return err // without the list, a kept part could be removed
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

// stagingTableIn reports whether the staging journal exists (staging used).
func stagingTableIn(ctx context.Context, tx *sql.Tx) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='tgdl_stage_moves'`).Scan(&n)
	return n > 0, err
}

// PurgeStaged removes from the staging journal, inside the purge's
// transaction, the staged files of groupID (all: of every group), so that
// none is catalogued or downloaded again after the purge. A file another
// group's message waits for (a follower) is handed to that message instead.
// It returns the local files to remove once the transaction is committed
// (RemoveStagedFiles).
func PurgeStaged(ctx context.Context, tx *sql.Tx, groupID string, all bool) ([]string, error) {
	if ok, err := stagingTableIn(ctx, tx); err != nil || !ok {
		return nil, err
	}
	where, args := `json_extract(item,'$.GroupID')=?`, []any{groupID}
	followers := `DELETE FROM tgdl_stage_followers WHERE group_id=?`
	if all {
		where, args, followers = "1=1", nil, `DELETE FROM tgdl_stage_followers`
	}
	if _, err := tx.ExecContext(ctx, followers, args...); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT path, stage_dir FROM tgdl_stage_moves WHERE `+where+` ORDER BY path`, args...)
	if err != nil {
		return nil, err
	}
	type staged struct{ path, dir string }
	var list []staged
	for rows.Next() {
		var s staged
		if err := rows.Scan(&s.path, &s.dir); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, s := range list {
		var item, group string
		var generation, message int64
		err := tx.QueryRowContext(ctx, `SELECT item, generation, group_id, message_id FROM tgdl_stage_followers WHERE path=? ORDER BY rowid LIMIT 1`, s.path).Scan(&item, &generation, &group, &message)
		switch {
		case err == nil:
			if _, err := tx.ExecContext(ctx, `UPDATE tgdl_stage_moves SET item=?, generation=? WHERE path=?`, item, generation, s.path); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM tgdl_stage_followers WHERE path=? AND group_id=? AND message_id=?`, s.path, group, message); err != nil {
				return nil, err
			}
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tgdl_stage_moves WHERE path=?`, s.path); err != nil {
			return nil, err
		}
		if rel := filepath.FromSlash(s.path); filepath.IsLocal(rel) && filepath.IsAbs(s.dir) {
			files = append(files, filepath.Join(s.dir, rel))
		}
	}
	return files, nil
}

// RemoveStagedFiles removes local staged files PurgeStaged let go (a file
// already gone is fine) and lets waiting downloads use the room freed.
func (l *Library) RemoveStagedFiles(files []string) error {
	var errs []error
	for _, f := range files {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	l.stageSignal()
	return errors.Join(errs...)
}

// RekeyStaged follows a group whose ID changed (a recovered group, see
// app/recovery_resolve.go) in the staging journal, inside its transaction.
func RekeyStaged(ctx context.Context, tx *sql.Tx, from, to, name string) error {
	if ok, err := stagingTableIn(ctx, tx); err != nil || !ok {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tgdl_stage_moves SET item=json_set(item,'$.GroupID',?,'$.GroupName',?) WHERE json_extract(item,'$.GroupID')=?`, to, name, from); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE tgdl_stage_followers SET group_id=?, item=json_set(item,'$.GroupID',?,'$.GroupName',?) WHERE group_id=?`, to, to, name, from)
	return err
}

// StagingStatus is what the staging folder holds, for the dashboard and
// the diagnostics report.
type StagingStatus struct {
	Waiting      int      `json:"waiting"` // files waiting to be copied to the library
	Bytes        int64    `json:"bytes"`
	Failing      int      `json:"failing"` // of those, the last copy failed
	FailingSince int64    `json:"failingSince,omitempty"`
	Stuck        int      `json:"stuck"` // need a person; their local files are kept
	StuckBytes   int64    `json:"stuckBytes,omitempty"`
	Full         bool     `json:"full"`                // downloads are waiting for room in the staging folder
	Bypassed     int      `json:"bypassed"`            // downloads sent straight to the library since start: staging was full (ix.12.5)
	BypassedAt   int64    `json:"bypassedAt"`          // the last of them (unix ms)
	FullSince    int64    `json:"fullSince,omitempty"` // since when (Unix ms)
	Errors       []string `json:"errors,omitempty"`
}

func (l *Library) StagingStatus(ctx context.Context) StagingStatus {
	st := StagingStatus{}
	l.stage.mu.Lock()
	st.Full, st.FullSince = l.stage.waiters > 0, l.stage.fullSince
	st.Bypassed, st.BypassedAt = l.stage.bypassed, l.stage.bypassedAt
	l.stage.mu.Unlock()
	if !l.stage.known.Load() {
		return st
	}
	_ = l.reader.QueryRowContext(ctx, `SELECT COALESCE(SUM(state='waiting'),0), COALESCE(SUM(CASE WHEN state='waiting' THEN size ELSE 0 END),0), COALESCE(SUM(error<>'' AND state='waiting'),0),
		COALESCE(SUM(state='stuck'),0), COALESCE(SUM(CASE WHEN state='stuck' THEN size ELSE 0 END),0), COALESCE(MIN(CASE WHEN failing_since>0 THEN failing_since END),0) FROM tgdl_stage_moves`).
		Scan(&st.Waiting, &st.Bytes, &st.Failing, &st.Stuck, &st.StuckBytes, &st.FailingSince)
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
