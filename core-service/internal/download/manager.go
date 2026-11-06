// Package download owns the bounded file handoff from Telegram to the local
// library. Identity reservation happens before network I/O, and publication
// publishes a sibling .part file without overwriting an existing destination.
package download

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

var ErrDuplicate = errors.New("telegram media identity is already queued or downloaded")
var ErrInvalidIdentity = errors.New("invalid Telegram media identity")

type Client interface {
	Download(context.Context, telegram.MediaIdentity, io.Writer) error
}

// ResumeClient can send a file from an offset. CanResume may say no: a client
// that cannot do it must start from the beginning, never append.
type ResumeClient interface {
	Client
	CanResume() bool
	DownloadFrom(context.Context, telegram.MediaIdentity, int64, io.Writer) error
}

// Files at least this big keep their progress, every this many bytes or this
// often, whichever comes first. A checkpoint syncs the file: on the NAS it took
// 6 to 8 ms, which cost nothing measurable at 8 MiB against unlimited downloading
// (2 to 5% when writing flat out at 23 MB/s, about 13% at 2 MiB). Under
// Telegram's speed limit a file gets 8 MiB only every few minutes, so the clock
// sets how much a restart can lose then.
var (
	resumeMinSize       int64 = 8 << 20
	partCheckpointEvery int64 = 8 << 20
	partCheckpointAfter       = 30 * time.Second
)

// A file asked to stop with no more than this left finishes instead: stopping
// would cost a new request and a read of everything kept, to save very little.
var partStopNearEnd int64 = 8 << 20

// partAfterPublish is for tests: between publishing a file and clearing its record.
var partAfterPublish func()

var errPartTooLong = errors.New("Telegram media exceeds declared size")

// partResume is what a transfer needs to carry on across restarts. Parts arrive
// in order, so progress is one offset and the SHA-256 of everything before it.
//
// The order is what makes it safe: the bytes are synced before the offset is
// recorded, so a record never claims more than is on disk. Coming back, the bytes
// are read and compared with the recorded hash before anything is reused, and on
// any difference the file starts from the beginning. Nothing is trusted that was
// not checked, because the file's hash is what the library believes afterwards.
type partResume struct {
	offset int64  // from an earlier attempt (0: none)
	sum    string // of the bytes before offset
	save   func(context.Context, int64, string) error
	clear  func(context.Context)
	digest hash.Hash // the caller's hash of the whole file, restored past the bytes kept
	// beforeSave is called between syncing the bytes and recording them (tests).
	beforeSave func()
}

type Manager struct {
	Client Client
	Index  *telegram.DedupIndex
	resume *partResume
	// prepared records durable recovery metadata after sync, before publication.
	prepared func() error
}

func NewManager(client Client, index *telegram.DedupIndex) *Manager {
	if index == nil {
		index = telegram.NewDedupIndex()
	}
	return &Manager{Client: client, Index: index}
}

func (m *Manager) Download(ctx context.Context, identity telegram.MediaIdentity, finalPath string) (string, error) {
	if m == nil || m.Client == nil || m.Index == nil {
		return "", errors.New("download manager is not configured")
	}
	if !identity.Valid() {
		return "", ErrInvalidIdentity
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !m.Index.Reserve(identity) {
		return "", ErrDuplicate
	}
	return m.DownloadReserved(ctx, identity, finalPath)
}

// DownloadReserved publishes an identity that was reserved by the live
// Telegram monitor before enqueueing. It is the handoff that prevents a live
// update and a history catch-up from both starting network I/O.
func (m *Manager) DownloadReserved(ctx context.Context, identity telegram.MediaIdentity, finalPath string) (string, error) {
	if m == nil || m.Client == nil || m.Index == nil {
		return "", errors.New("download manager is not configured")
	}
	if !identity.Valid() {
		return "", ErrInvalidIdentity
	}
	if !m.Index.Claim(identity) {
		return "", ErrDuplicate
	}
	return m.downloadReserved(ctx, identity, finalPath)
}

func (m *Manager) downloadReserved(ctx context.Context, identity telegram.MediaIdentity, finalPath string) (string, error) {
	committed := false
	defer func() {
		if !committed {
			m.Index.AbortClaim(identity)
		}
	}()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	finalPath = filepath.Clean(finalPath)
	if finalPath == "." || finalPath == string(filepath.Separator) {
		return "", errors.New("invalid final path")
	}
	dir := filepath.Dir(finalPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create media directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := m.transfer(ctx, identity, root, filepath.Base(finalPath)); err != nil {
		return "", err
	}
	committed = true
	m.Index.Complete(identity)
	return finalPath, nil
}

// DownloadInRoot keeps creation, cleanup and publication relative to open
// directory handles. The supplied root must outlive the call.
func (m *Manager) DownloadInRoot(ctx context.Context, identity telegram.MediaIdentity, root *os.Root, relative string) (string, error) {
	if m == nil || m.Client == nil || m.Index == nil || root == nil {
		return "", errors.New("download manager is not configured")
	}
	if !identity.Valid() {
		return "", ErrInvalidIdentity
	}
	if !filepath.IsLocal(relative) || filepath.Clean(relative) == "." {
		return "", os.ErrPermission
	}
	if !m.Index.Reserve(identity) || !m.Index.Claim(identity) {
		return "", ErrDuplicate
	}
	committed := false
	defer func() {
		if !committed {
			m.Index.AbortClaim(identity)
		}
	}()
	dir := filepath.Dir(relative)
	if err := root.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	parent, err := root.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer parent.Close()
	if err = m.transfer(ctx, identity, parent, filepath.Base(relative)); err != nil {
		return "", err
	}
	committed = true
	m.Index.Complete(identity)
	return relative, nil
}

// reopenPart checks a part kept by an earlier attempt and readies it to carry on.
// A nil file means there is nothing usable: the transfer starts from the beginning.
// An error means it could not tell (the attempt was cancelled, say): the part stays.
func (m *Manager) reopenPart(ctx context.Context, root *os.Root, tmpName string, identity telegram.MediaIdentity, running hash.Hash) (*os.File, int64, error) {
	r := m.resume
	if r.offset <= 0 || r.offset >= identity.Size {
		return nil, 0, nil
	}
	tmp, err := root.OpenFile(tmpName, os.O_RDWR, 0600)
	if err != nil {
		return nil, 0, nil // gone
	}
	info, err := tmp.Stat()
	if err != nil || info.Size() < r.offset {
		tmp.Close()
		return nil, 0, nil // shorter than recorded: a sync that did not hold
	}
	if _, err := io.CopyN(running, &contextReader{ctx: ctx, r: tmp}, r.offset); err != nil {
		tmp.Close()
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, nil
	}
	if hex.EncodeToString(running.Sum(nil)) != r.sum {
		tmp.Close()
		return nil, 0, nil
	}
	// The whole file's hash goes on from here, without reading the bytes again.
	state, stateErr := running.(encoding.BinaryMarshaler).MarshalBinary()
	if stateErr == nil {
		stateErr = r.digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(state)
	}
	if stateErr != nil || tmp.Truncate(r.offset) != nil {
		tmp.Close()
		return nil, 0, nil
	}
	if _, err := tmp.Seek(r.offset, io.SeekStart); err != nil {
		tmp.Close()
		return nil, 0, nil
	}
	return tmp, r.offset, nil
}

func (m *Manager) transfer(ctx context.Context, identity telegram.MediaIdentity, root *os.Root, finalName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tmpName := partName(finalName)
	var tmp *os.File
	var start int64
	var running hash.Hash
	if m.resume != nil {
		running = sha256.New()
		var err error
		if tmp, start, err = m.reopenPart(ctx, root, tmpName, identity, running); err != nil {
			return err
		}
		if tmp == nil && m.resume.offset > 0 {
			// Not usable: as if it had never been.
			m.resume.clear(context.WithoutCancel(ctx))
			_ = root.Remove(tmpName)
			running.Reset()
		}
	}
	if tmp == nil {
		var err error
		tmp, err = root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("create partial file: %w", err)
		}
	}
	// With a checkpoint recorded the part stays for the next attempt, whatever
	// stopped this one. Without one it has nothing worth keeping.
	keep := start > 0
	defer func() {
		tmp.Close()
		if !keep {
			root.Remove(tmpName)
		}
	}()
	writer := &mediaWriter{ctx: ctx, dst: tmp, remaining: identity.Size - start, sum: running, written: start, saved: start, savedAt: time.Now()}
	if running != nil {
		writer.stop = telegram.CheckpointStop(ctx)
		writer.checkpoint = func(offset int64) (bool, error) {
			if err := tmp.Sync(); err != nil {
				return false, fmt.Errorf("sync partial file: %w", err)
			}
			if m.resume.beforeSave != nil {
				m.resume.beforeSave()
			}
			// A record that cannot be written only costs the progress since the
			// last one: not a reason to fail a good download.
			if err := m.resume.save(ctx, offset, hex.EncodeToString(running.Sum(nil))); err != nil {
				return false, nil
			}
			keep = true
			return true, nil
		}
	}
	var downloadErr error
	if start > 0 {
		downloadErr = m.Client.(ResumeClient).DownloadFrom(ctx, identity, start, writer)
	} else {
		downloadErr = m.Client.Download(ctx, identity, writer)
	}
	discard := func() {
		keep = false
		if m.resume != nil {
			m.resume.clear(context.WithoutCancel(ctx))
		}
	}
	if writer.stopped && keep {
		// Asked to stop, and stopped right after recording a checkpoint: the
		// part stays for the next attempt, whatever the client made of it.
		return telegram.ErrDownloadStopped
	}
	if downloadErr != nil {
		if errors.Is(downloadErr, errPartTooLong) {
			discard()
		}
		return fmt.Errorf("download Telegram media: %w", downloadErr)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if writer.remaining != 0 {
		discard()
		return fmt.Errorf("Telegram media size mismatch: expected %d, received %d", identity.Size, identity.Size-writer.remaining)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync partial file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close partial file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.prepared != nil {
		if err := m.prepared(); err != nil {
			return err
		}
	}
	// The OS performs an exclusive rename: a competing file must never be
	// replaced, even if it appeared after the initial existence check.
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := publishExclusiveAt(dir, tmpName, finalName); err != nil {
		if errors.Is(err, os.ErrExist) {
			discard()
			return fmt.Errorf("final file already exists: %w", ErrDuplicate)
		}
		return fmt.Errorf("publish media file: %w", err)
	}
	keep = false
	if m.resume != nil {
		if partAfterPublish != nil {
			partAfterPublish()
		}
		m.resume.clear(context.WithoutCancel(ctx))
	}
	return nil
}

type mediaWriter struct {
	ctx       context.Context
	dst       io.Writer
	remaining int64
	// With sum set, progress is recorded on part boundaries, where Telegram can
	// be asked to carry on. checkpoint says whether the record was written.
	sum            hash.Hash
	written, saved int64
	savedAt        time.Time
	checkpoint     func(int64) (bool, error)
	recorded       bool // saved is in the record (not only where the attempt began)
	// stop, optional, asks at each part boundary whether to end there: the
	// writer then records a checkpoint at once and ends the transfer.
	stop    func() bool
	stopped bool
}

// errStopAtCheckpoint ends the client's transfer; transfer reports it as
// telegram.ErrDownloadStopped.
var errStopAtCheckpoint = errors.New("stopped after a checkpoint")

func (w *mediaWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.remaining {
		return 0, errPartTooLong
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	if w.sum != nil {
		w.sum.Write(p[:n])
		w.written += int64(n)
		if err == nil && w.remaining > 0 && w.written%telegram.PartSize == 0 {
			stopping := w.stop != nil && w.remaining > partStopNearEnd && w.stop()
			due := w.written-w.saved >= partCheckpointEvery ||
				(w.written > w.saved && partCheckpointAfter > 0 && time.Since(w.savedAt) >= partCheckpointAfter)
			if due || (stopping && w.written > w.saved) {
				w.saved, w.savedAt = w.written, time.Now()
				recorded, err := w.checkpoint(w.written)
				if err != nil {
					return n, err
				}
				w.recorded = recorded
			}
			// Only a recorded checkpoint lets it stop: without one it goes on,
			// and tries again at the next part.
			if stopping && w.recorded && w.saved == w.written {
				w.stopped = true
				return n, errStopAtCheckpoint
			}
		}
	}
	return n, err
}

func partName(final string) string { return ".tgdl-" + final + ".part" }
