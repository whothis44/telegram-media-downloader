package download

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// Item describes one fresh Telegram message. The stable media identity is
// independent of its message, group and filename; each message retains its own
// catalog row even when several messages reuse the same physical file.
type Item struct {
	GroupID    string
	GroupName  string
	MessageID  int64
	Name       string
	Type       string
	Identity   telegram.MediaIdentity
	Facts      telegram.MediaFacts
	TTLSeconds int // self-destruct timer of the source media, 0 when none
	// Topic: the folder of the message's forum topic inside the group's
	// folder ("": none, or the General topic). It is the topic's name when
	// its first file was saved (ix.12.10), already a safe path component.
	Topic string `json:",omitempty"`
	// SkipDuplicates: media already in the library under another message is
	// not catalogued again. Reposts and forwards of one photo or clip
	// otherwise each became an entry pointing at the same file.
	SkipDuplicates bool
	// NoStage: write straight to the library even when a staging folder is set.
	NoStage bool `json:"-"`
	// Requeue (optional) is what the caller needs to queue the message again
	// (StageLost). It is kept with a staged file only.
	Requeue    []byte `json:",omitempty"`
	generation int64
}

type Record struct {
	ID     int64
	Path   string
	SHA256 string
	Reused bool
	// Skipped: the media was already catalogued for another message, so no
	// entry was made for this one (Item.SkipDuplicates). ID is the existing
	// entry.
	Skipped bool
	// Staged: the file is complete and verified in the local staging folder
	// and is copied to the library later; ID is 0 until then (OnStaged).
	Staged bool
	// Dedup says how a Reused record was found (Dedup* below), and DedupHow
	// how an exact re-upload was confirmed (Substitute's answer).
	Dedup, DedupHow string
}

// How a message was found to share a file already in the library.
const (
	// DedupTelegramFile: the same Telegram file (a forward, a repost, or
	// the message again). Nothing was downloaded.
	DedupTelegramFile = "telegram-file"
	// DedupReupload: a new upload of the same bytes, confirmed on Telegram
	// before downloading (Substitute). Nothing was downloaded.
	DedupReupload = "exact-reupload"
	// DedupAfterDownload: the downloaded bytes were already in the library.
	// The downloaded copy was removed; the bytes were spent.
	DedupAfterDownload = "after-download"
)

// tagged marks how a record was found when it reuses a file.
func tagged(r Record, how string) Record {
	if r.Reused && r.Dedup == "" {
		r.Dedup = how
	}
	return r
}

// Library uses the existing downloads table as its durable identity index.
// Only active transfers have in-memory entries, bounded by the worker limit.
// It shares the app's SQLite writer with deletion and cleanup.
type Library struct {
	// Substitute (an archive option) may name a saved file that holds this
	// item's exact bytes, checked on Telegram without a full download. The
	// item is then catalogued like a forward of that file. how names the
	// check that confirmed it (Record.DedupHow); DedupTelegramFile when it
	// is the same Telegram file, known by a message without an entry.
	Substitute func(context.Context, Item, Client) (path, how string, ok bool)
	// Adopt (ix.12.6), when set, says whether a file already in the library
	// under this message's own name (put there by hand, or from a staging
	// folder emptied by hand) holds exactly the item's bytes, as checked on
	// Telegram. Such a file is catalogued instead of downloaded again.
	Adopt func(ctx context.Context, item Item, path string, client Client) bool
	// SubstituteWait (with Substitute) names the key under which an item
	// waits, at most SubstituteWaitMax, while another item with the same
	// key is downloading, so that Substitute can find that one saved (the
	// app keys by kind, MIME type and exact size, for what it can confirm).
	// After the wait the item downloads as usual.
	SubstituteWait func(context.Context, Item) (string, bool)
	// Check (optional, ix.12.13) runs after Substitute found no saved copy
	// and before the transfer (staged or not), unless a part of an earlier
	// attempt is kept (the download then carries on). An error it returns
	// ends this ingest with that error: the app's sampled check of a big
	// video returns telegram.DownloadDeferred for a probable copy.
	Check             func(context.Context, Item, Client) error
	SubstituteWaitMax time.Duration // 0: 5 minutes
	// Blocked (optional): the item's identity is to be downloaded for real
	// (the archive's "Fetch the real files"): it is not matched by its
	// Telegram identity to a saved file, nor follows a staged one.
	Blocked func(context.Context, Item) bool
	// Staging (optional) names a local folder downloads are written to
	// first, then copied to the library (staging.go). OnStaged is told when
	// such a file is in the library; StageHold is held while it is added.
	// StageLost puts a message back in the download queue when its staged
	// file disappeared before it reached the library.
	Staging   func(context.Context) Staging
	OnStaged  func(context.Context, Item, Record)
	StageHold func(context.Context) (func(), error)
	StageLost func(context.Context, Item) error
	stage     stageState

	writer, reader *sql.DB
	root           string
	listings       folderListings // folder listings for adoption (ix.12.6)
	dataDir        string
	mu             sync.Mutex
	flights        map[string]chan struct{}
	slots          chan struct{}
}

func NewLibrary(writer, reader *sql.DB, root string, workers int) (*Library, error) {
	configured, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return NewLibraryIn(writer, reader, filepath.Dir(configured), root, workers)
}

// NewLibraryIn keeps derived data (thumbs, seekbar) under dataDir even when
// the media root lives on another disk (TGDL_DOWNLOADS_DIR).
func NewLibraryIn(writer, reader *sql.DB, dataDir, root string, workers int) (*Library, error) {
	if writer == nil || reader == nil || root == "" || workers < 1 || workers > 64 {
		return nil, errors.New("invalid media library configuration")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Library{writer: writer, reader: reader, root: root, dataDir: dataDir, flights: make(map[string]chan struct{}), slots: make(chan struct{}, workers)}, nil
}

// enter coalesces identity/content work, including wait cancellation. It does
// not retain completed IDs; completed media is looked up in SQLite next time.
func (l *Library) enter(ctx context.Context, key string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l.mu.Lock()
		if done, ok := l.flights[key]; ok {
			l.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				continue
			}
		}
		done := make(chan struct{})
		l.flights[key] = done
		l.mu.Unlock()
		return func() { l.mu.Lock(); delete(l.flights, key); close(done); l.mu.Unlock() }, nil
	}
}

// tryEnter is enter without waiting: ok is false while someone else holds key.
func (l *Library) tryEnter(key string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, busy := l.flights[key]; busy {
		return nil, false
	}
	done := make(chan struct{})
	l.flights[key] = done
	return func() { l.mu.Lock(); delete(l.flights, key); close(done); l.mu.Unlock() }, true
}

func (l *Library) substituteWait(ctx context.Context, item Item) (string, bool) {
	if l.Substitute == nil || l.SubstituteWait == nil {
		return "", false
	}
	return l.SubstituteWait(ctx, item)
}

// enterWaiting is enter for at most max, with the client's attempt time
// limit stopped while it waits (clients that have one: PauseBudget). After
// max it gives up: release is nil and err is nil, and the caller goes on
// without the key.
func (l *Library) enterWaiting(ctx context.Context, key string, max time.Duration, client Client) (func(), error) {
	if release, ok := l.tryEnter(key); ok {
		return release, nil
	}
	if p, ok := client.(interface{ PauseBudget() func() }); ok {
		resume := p.PauseBudget()
		defer resume()
	}
	waitCtx, cancel := context.WithTimeout(ctx, max)
	defer cancel()
	release, err := l.enter(waitCtx, key)
	if err != nil && ctx.Err() == nil {
		return nil, nil // waited long enough
	}
	return release, err
}

func (l *Library) blocked(ctx context.Context, item Item) bool {
	return l.Blocked != nil && l.Blocked(ctx, item)
}

func messageKey(item Item) string {
	return "message:" + item.GroupID + ":" + strconv.FormatInt(item.MessageID, 10)
}

func (l *Library) Ingest(ctx context.Context, item Item, client Client) (Record, error) {
	item.GroupID = strings.TrimSpace(item.GroupID)
	item.Identity.Kind = strings.ToLower(strings.TrimSpace(item.Identity.Kind))
	item.Identity.ID = strings.TrimSpace(item.Identity.ID)
	if !item.Identity.Valid() || item.GroupID == "" || item.MessageID <= 0 {
		return Record{}, ErrInvalidIdentity
	}
	select {
	case l.slots <- struct{}{}:
		defer func() { <-l.slots }()
	case <-ctx.Done():
		return Record{}, ctx.Err()
	}
	messageRelease, err := l.enter(ctx, messageKey(item))
	if err != nil {
		return Record{}, err
	}
	defer messageRelease()
	if err := l.writer.QueryRowContext(ctx, `INSERT INTO tgdl_message_generations(group_id,message_id,generation) VALUES(?,?,1)
      ON CONFLICT(group_id,message_id) DO UPDATE SET generation=generation+1 RETURNING generation`, item.GroupID, item.MessageID).Scan(&item.generation); err != nil {
		return Record{}, err
	}
	release, err := l.enter(ctx, "identity:"+item.Identity.Key())
	if err != nil {
		return Record{}, err
	}
	defer release()
	if !l.blocked(ctx, item) {
		if record, found, err := l.existing(ctx, item, `d.telegram_media_kind=? AND d.telegram_media_id=?`, item.Identity.Kind, item.Identity.ID); err != nil || found {
			return tagged(record, DedupTelegramFile), err
		}
		if record, found, err := l.reuse(ctx, item, `d.telegram_media_kind=? AND d.telegram_media_id=? AND (d.telegram_media_size=? OR (d.telegram_media_size IS NULL AND d.file_size=?))`, item.Identity.Kind, item.Identity.ID, item.Identity.Size, item.Identity.Size); err != nil || found {
			return tagged(record, DedupTelegramFile), err
		}
		if record, found, err := l.followStaged(ctx, item); err != nil || found {
			return tagged(record, DedupTelegramFile), err
		}
	}
	if client == nil {
		return Record{}, errors.New("Telegram download client is unavailable")
	}
	if !l.blocked(ctx, item) {
		if record, found, err := l.adoptOrphan(ctx, item, client); err != nil || found {
			return record, err
		}
	}
	if key, ok := l.substituteWait(ctx, item); ok {
		max := l.SubstituteWaitMax
		if max <= 0 {
			max = 5 * time.Minute
		}
		sizeRelease, err := l.enterWaiting(ctx, "same-size:"+key, max, client)
		if err != nil {
			return Record{}, err
		}
		if sizeRelease != nil {
			defer sizeRelease()
		}
	}
	if l.Substitute != nil {
		if path, how, ok := l.Substitute(ctx, item, client); ok {
			// With SkipDuplicates no entry is made, as for any other copy.
			record, found, err := l.existing(ctx, item, `d.file_path=?`, path)
			if err == nil && !found {
				record, found, err = l.reuse(ctx, item, `d.file_path=?`, path)
			}
			if err != nil || found {
				// how is DedupTelegramFile when Substitute knows this Telegram
				// file under a message that has no entry of its own.
				kind := DedupReupload
				if how == DedupTelegramFile {
					kind = DedupTelegramFile
				}
				record = tagged(record, kind)
				if record.Dedup == kind {
					record.DedupHow = how
				}
				return record, err
			}
		}
	}
	if l.Check != nil && !l.partOfAttempt(ctx, item) {
		if err := l.Check(ctx, item, client); err != nil {
			return Record{}, err
		}
	}
	if st := l.stagingFor(ctx, item); st.Dir != "" {
		record, err := l.ingestStaged(ctx, item, client, st)
		return tagged(record, DedupAfterDownload), err
	}
	// A part kept by an earlier attempt goes on under its own name.
	resuming := canResume(client, item.Identity.Size)
	var progress partRow
	if resuming {
		progress = l.partLoad(ctx, item, "")
	}
	relative := filepath.FromSlash(progress.path)
	if progress.path == "" {
		relative, err = l.destination(item)
	} else {
		err = l.ensureDir(filepath.Dir(relative))
	}
	if err != nil {
		return Record{}, err
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return Record{}, err
	}
	if _, err = l.writer.ExecContext(ctx, `INSERT INTO tgdl_ingest_files(path,item,generation) VALUES(?,?,?)`, filepath.ToSlash(relative), string(payload), item.generation); err != nil {
		return Record{}, err
	}
	digest := sha256.New()
	// The per-transfer handoff owns exactly one identity; the durable library
	// owns all cross-message coalescing and does not retain this temporary map.
	m := NewManager(hashClient{client, digest}, telegram.NewDedupIndex())
	if resuming {
		m.resume = l.partResumeFor(item, filepath.ToSlash(relative), "", progress, digest)
	}
	m.prepared = func() error {
		_, err := l.writer.ExecContext(ctx, `UPDATE tgdl_ingest_files SET sha256=? WHERE path=?`, hex.EncodeToString(digest.Sum(nil)), filepath.ToSlash(relative))
		return err
	}
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return Record{}, err
	}
	defer root.Close()
	path, err := m.DownloadInRoot(ctx, item.Identity, root, relative)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, cleanupErr := l.writer.ExecContext(cleanupCtx, `DELETE FROM tgdl_ingest_files WHERE path=?`, filepath.ToSlash(relative))
		return Record{}, errors.Join(err, cleanupErr)
	}
	f, info, err := l.open(relative)
	if err != nil {
		return Record{}, err
	}
	fingerprint, stampErr := fileFingerprint(f, info)
	f.Close()
	if stampErr != nil {
		return Record{}, stampErr
	}
	sum := hex.EncodeToString(digest.Sum(nil))
	contentRelease, err := l.enter(ctx, "sha256:"+sum)
	if err != nil {
		return Record{}, err
	}
	defer contentRelease()
	// The bytes were downloaded and are already in the library: the
	// catalogue points at the saved copy and the downloaded one goes.
	if record, found, err := l.existing(ctx, item, `d.file_hash=?`, sum); err != nil {
		return Record{}, err
	} else if found {
		record = tagged(record, DedupAfterDownload)
		if err := root.Remove(path); err != nil {
			return record, fmt.Errorf("remove redundant downloaded file: %w", err)
		}
		if _, err := l.writer.ExecContext(ctx, `DELETE FROM tgdl_ingest_files WHERE path=?`, filepath.ToSlash(path)); err != nil {
			return record, err
		}
		return record, nil
	}
	if record, found, err := l.reuse(ctx, item, `d.file_hash=? AND d.file_size=?`, sum, item.Identity.Size); err != nil {
		return Record{}, err
	} else if found {
		record = tagged(record, DedupAfterDownload)
		// No catalog entry has ever named our unique destination. Deleting
		// only this transfer's file cannot remove another message's media.
		if err := root.Remove(path); err != nil {
			return record, fmt.Errorf("remove redundant downloaded file: %w", err)
		}
		if _, err := l.writer.ExecContext(ctx, `DELETE FROM tgdl_ingest_files WHERE path=?`, filepath.ToSlash(path)); err != nil {
			return record, err
		}
		return record, nil
	}
	record, err := l.register(ctx, item, candidate{path: relative, sha256: sum, info: info, fingerprint: fingerprint}, false)
	// A failed catalog commit intentionally leaves the completed file in place
	// for recovery rather than claiming success or deleting downloaded data.
	if err != nil {
		return Record{}, fmt.Errorf("catalog registration failed for %s: %w", relative, err)
	}
	return record, nil
}

type hashClient struct {
	Client
	digest hash.Hash
}

func (c hashClient) Download(ctx context.Context, id telegram.MediaIdentity, w io.Writer) error {
	return c.Client.Download(ctx, id, io.MultiWriter(w, c.digest))
}

// CanResume and DownloadFrom pass on to the client, which the library has already
// asked (canResume).
func (c hashClient) CanResume() bool { return canResume(c.Client, resumeMinSize) }

func (c hashClient) DownloadFrom(ctx context.Context, id telegram.MediaIdentity, offset int64, w io.Writer) error {
	return c.Client.(ResumeClient).DownloadFrom(ctx, id, offset, io.MultiWriter(w, c.digest))
}

// SafeComponent is value made safe as one folder or file name.
func SafeComponent(value string) string { return safeComponent(value) }

func safeComponent(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, value)
	value = strings.Trim(value, " .")
	for len(value) > 120 {
		_, n := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-n]
	}
	if value == "" {
		value = "media"
	}
	return value
}

func (l *Library) destination(item Item) (string, error) {
	dir, name, err := destinationName(item)
	if err != nil {
		return "", err
	}
	if err := l.ensureDir(dir); err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// destinationParts is where the item's files go and how their names begin
// and end: stem + 32 random hex digits + ext (destinationName).
func destinationParts(item Item) (dir, stem, ext string) {
	group := item.GroupName
	if strings.TrimSpace(group) == "" {
		group = item.GroupID
	}
	folder := "documents"
	switch item.Type {
	case "photo":
		folder = "images"
	case "video":
		folder = "videos"
	case "audio":
		folder = "audio"
	case "sticker":
		folder = "stickers"
	case "gif":
		folder = "gifs"
	}
	dir = filepath.Join(safeComponent(group), folder)
	if strings.TrimSpace(item.Topic) != "" {
		dir = filepath.Join(safeComponent(group), safeComponent(item.Topic), folder)
	}
	name := safeComponent(item.Name)
	ext = filepath.Ext(name)
	stem = strings.TrimSuffix(name, ext) + "_" + strconv.FormatInt(item.MessageID, 10) + "_"
	return dir, stem, ext
}

// orphansFor lists files in the item's library folder under its own name
// (stem, 32 hex digits, ext) and of its exact size that the catalogue does
// not know: copies of it that reached the library by hand (ix.12.6).
func (l *Library) orphansFor(ctx context.Context, item Item) []string {
	dir, stem, ext := destinationParts(item)
	dirs := []string{dir}
	if item.Topic != "" {
		// A forum topic's file moved in by hand may sit in the group's own
		// type folder, where files went before topic folders (ix.12.10).
		plain := item
		plain.Topic = ""
		legacy, _, _ := destinationParts(plain)
		dirs = append(dirs, legacy)
	}
	var out []string
	for _, dir := range dirs {
		out = append(out, l.orphansIn(ctx, item, dir, stem, ext)...)
	}
	return out
}

func (l *Library) orphansIn(ctx context.Context, item Item, dir, stem, ext string) []string {
	var out []string
	for _, e := range l.folderListing(dir) {
		name := e.name
		if !strings.HasPrefix(name, stem) || !strings.HasSuffix(name, ext) || len(name) != len(stem)+32+len(ext) || e.size != item.Identity.Size {
			continue
		}
		if _, err := hex.DecodeString(name[len(stem) : len(stem)+32]); err != nil {
			continue
		}
		rel := filepath.ToSlash(filepath.Join(dir, name))
		var n int
		if l.reader.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM downloads WHERE file_path=?)+(SELECT COUNT(*) FROM tgdl_ingest_files WHERE path=?)`, rel, rel).Scan(&n) != nil || n > 0 {
			continue
		}
		out = append(out, rel)
	}
	return out
}

type folderEntry struct {
	name string
	size int64
}

type folderListings struct {
	mu sync.Mutex
	at map[string]time.Time
	of map[string][]folderEntry
}

// folderListingFor is how long a folder's listing is used before it is read
// again: one listing per folder at most this often, not one per download.
var folderListingFor = 10 * time.Minute

// folderListing is the regular files of a library folder with their sizes,
// read at most every folderListingFor.
func (l *Library) folderListing(dir string) []folderEntry {
	c := &l.listings
	c.mu.Lock()
	defer c.mu.Unlock()
	if at, ok := c.at[dir]; ok && time.Since(at) < folderListingFor {
		return c.of[dir]
	}
	if c.at == nil {
		c.at, c.of = map[string]time.Time{}, map[string][]folderEntry{}
	}
	var out []folderEntry
	if entries, err := os.ReadDir(filepath.Join(l.root, dir)); err == nil {
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			if info, err := e.Info(); err == nil {
				out = append(out, folderEntry{e.Name(), info.Size()})
			}
		}
	}
	c.at[dir], c.of[dir] = time.Now(), out
	return out
}

// adoptOrphan catalogues a copy of the item already in the library, when
// Adopt confirms its bytes, instead of downloading the item again.
func (l *Library) adoptOrphan(ctx context.Context, item Item, client Client) (Record, bool, error) {
	if l.Adopt == nil {
		return Record{}, false, nil
	}
	for _, path := range l.orphansFor(ctx, item) {
		if !l.Adopt(ctx, item, path, client) {
			continue
		}
		f, info, err := l.open(path)
		if err != nil {
			continue
		}
		fingerprint, err := fileFingerprint(f, info)
		if err != nil {
			f.Close()
			continue
		}
		sum, n, err := hashFile(ctx, f)
		f.Close()
		if err != nil || n != item.Identity.Size {
			continue
		}
		record, err := l.register(ctx, item, candidate{path: path, sha256: sum, info: info, fingerprint: fingerprint}, false)
		if errors.Is(err, errCandidateChanged) {
			continue
		}
		return record, err == nil, err
	}
	return Record{}, false, nil
}

// destinationName is a new unique name for the item, without touching disk.
func destinationName(item Item) (dir, name string, err error) {
	dir, stem, ext := destinationParts(item)
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", "", err
	}
	return dir, stem + hex.EncodeToString(random) + ext, nil
}

// ensureDir creates dir under the library root and refuses symlinks on it.
func (l *Library) ensureDir(dir string) error {
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// No user-controlled directory symlink may be used by the transfer path.
	parts := strings.Split(dir, string(filepath.Separator))
	prefix := ""
	for _, part := range parts {
		prefix = filepath.Join(prefix, part)
		info, err := root.Lstat(prefix)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("media destination contains a symlink")
		}
	}
	return nil
}

type candidate struct {
	path, sha256, fingerprint string
	info                      os.FileInfo
}

var ErrSuperseded = errors.New("Telegram message was superseded by a later ingestion")

var errCandidateChanged = errors.New("media candidate changed before registration")

func (l *Library) open(name string) (*os.File, os.FileInfo, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if !filepath.IsLocal(name) || filepath.Clean(name) == "." {
		return nil, nil, os.ErrPermission
	}
	f, err := os.OpenInRoot(l.root, name)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err == nil {
			err = os.ErrPermission
		}
		return nil, nil, err
	}
	return f, info, nil
}

func sameMedia(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// existing finds, when item.SkipDuplicates is set, an entry of another
// message whose media matches `where` and whose file is still on disk. A
// message that already has its own entry is never skipped: that is a repair
// or an edit, and it goes through the normal path.
func (l *Library) existing(ctx context.Context, item Item, where string, args ...any) (Record, bool, error) {
	if !item.SkipDuplicates {
		return Record{}, false, nil
	}
	var own bool
	if err := l.reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM downloads WHERE group_id=? AND message_id=?)`, item.GroupID, item.MessageID).Scan(&own); err != nil || own {
		return Record{}, false, err
	}
	rows, err := l.reader.QueryContext(ctx, `SELECT d.id,d.file_path,COALESCE(d.file_hash,'') FROM downloads d
		WHERE d.status='completed' AND d.file_path IS NOT NULL AND d.file_path<>'' AND (`+where+`) ORDER BY d.id LIMIT 20`, args...)
	if err != nil {
		return Record{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var path, sum string
		if err := rows.Scan(&id, &path, &sum); err != nil {
			return Record{}, false, err
		}
		f, info, err := l.open(path)
		if err != nil {
			continue
		}
		f.Close()
		if info.Size() <= 0 {
			continue
		}
		return Record{ID: id, Path: filepath.ToSlash(path), SHA256: sum, Reused: true, Skipped: true}, true, nil
	}
	return Record{}, false, rows.Err()
}

func (l *Library) reuse(ctx context.Context, item Item, where string, args ...any) (Record, bool, error) {
	// The identity index eliminates filename scans. A verified size/mtime hash
	// cache avoids rereading multi-gigabyte media for every forwarded message.
	rows, err := l.reader.QueryContext(ctx, `WITH candidates AS (
        SELECT DISTINCT d.file_path FROM downloads d
        WHERE d.status='completed' AND d.file_path IS NOT NULL AND (`+where+`)
    ) SELECT d.file_path,NULL,v.size,v.mtime_ns,v.sha256,v.fingerprint,
        (SELECT min(lower(o.file_hash)) FROM downloads o WHERE o.file_path=d.file_path AND o.file_hash<>''),
        (SELECT max(lower(o.file_hash)) FROM downloads o WHERE o.file_path=d.file_path AND o.file_hash<>'')
        FROM candidates d LEFT JOIN tgdl_verified_media v ON v.path=d.file_path`, args...)
	if err != nil {
		return Record{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var storedHash, cachedHash, cachedFingerprint, minHash, maxHash sql.NullString
		var size, mtime sql.NullInt64
		if err := rows.Scan(&path, &storedHash, &size, &mtime, &cachedHash, &cachedFingerprint, &minHash, &maxHash); err != nil {
			return Record{}, false, err
		}
		f, info, err := l.open(path)
		if err != nil {
			// Unreadable, non-regular and root-escaping historical entries are
			// never dedup evidence. Another valid candidate can still win.
			continue
		}
		if info.Size() != item.Identity.Size {
			f.Close()
			continue
		}
		fingerprint, err := fileFingerprint(f, info)
		if err != nil {
			f.Close()
			return Record{}, false, err
		}
		sum := cachedHash.String
		if fingerprint == "" || fingerprint != cachedFingerprint.String || !cachedHash.Valid || !size.Valid || !mtime.Valid || size.Int64 != info.Size() || mtime.Int64 != info.ModTime().UnixNano() {
			h := sha256.New()
			buf := make([]byte, 1<<20)
			for {
				if err = ctx.Err(); err != nil {
					break
				}
				var n int
				n, err = f.Read(buf)
				if n > 0 {
					_, _ = h.Write(buf[:n])
				}
				if err != nil {
					break
				}
			}
			if err != nil && err != io.EOF {
				f.Close()
				return Record{}, false, err
			}
			after, err := f.Stat()
			if err != nil {
				f.Close()
				return Record{}, false, err
			}
			afterStamp, stampErr := fileFingerprint(f, after)
			if stampErr != nil {
				f.Close()
				return Record{}, false, stampErr
			}
			if !sameMedia(info, after) || fingerprint != afterStamp {
				f.Close()
				continue
			}
			sum = hex.EncodeToString(h.Sum(nil))
		}
		f.Close()
		if (storedHash.Valid && storedHash.String != "" && !strings.EqualFold(storedHash.String, sum)) || (cachedHash.Valid && cachedHash.String != "" && !strings.EqualFold(cachedHash.String, sum)) {
			continue
		}
		if (minHash.Valid && minHash.String != sum) || (maxHash.Valid && maxHash.String != sum) {
			continue
		}
		record, err := l.register(ctx, item, candidate{path: path, sha256: sum, info: info, fingerprint: fingerprint}, true)
		if errors.Is(err, errCandidateChanged) {
			continue
		}
		return record, err == nil, err
	}
	return Record{}, false, rows.Err()
}

func (l *Library) register(ctx context.Context, item Item, media candidate, reused bool) (Record, error) {
	tx, err := l.writer.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback()
	// Acquire the write lock before inspecting disk: the deletion outbox uses
	// this same lock, so a retained path cannot be unlinked before commit.
	if _, err := tx.ExecContext(ctx, `UPDATE tgdl_file_cleanup SET path=path WHERE 0`); err != nil {
		return Record{}, err
	}
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM tgdl_message_generations WHERE group_id=? AND message_id=?`, item.GroupID, item.MessageID).Scan(&generation); err != nil {
		return Record{}, err
	}
	if generation != item.generation {
		return Record{}, ErrSuperseded
	}
	// Which version is in the catalogue: a staged file of an older one is
	// dropped only once a newer one is (staging.go).
	if _, err := tx.ExecContext(ctx, `UPDATE tgdl_message_generations SET registered=? WHERE group_id=? AND message_id=?`, item.generation, item.GroupID, item.MessageID); err != nil {
		return Record{}, err
	}
	f, info, err := l.open(media.path)
	if err != nil {
		if os.IsNotExist(err) {
			return Record{}, errCandidateChanged
		}
		return Record{}, err
	}
	fingerprint, stampErr := fileFingerprint(f, info)
	f.Close()
	if stampErr != nil {
		return Record{}, stampErr
	}
	if !sameMedia(media.info, info) || (media.fingerprint != "" && media.fingerprint != fingerprint) {
		return Record{}, errCandidateChanged
	}
	var previousPath, previousHash sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT file_path,file_hash FROM downloads WHERE group_id=? AND message_id=?`, item.GroupID, item.MessageID).Scan(&previousPath, &previousHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Record{}, err
	}
	inserted := errors.Is(err, sql.ErrNoRows)
	var id int64
	facts := item.Facts
	err = tx.QueryRowContext(ctx, `INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,file_hash,telegram_media_kind,telegram_media_id,telegram_media_size,
		telegram_mime,media_width,media_height,media_duration_ms,forward_origin,telegram_thumb,ttl_seconds,status)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'completed') ON CONFLICT(group_id,message_id) DO UPDATE SET
		group_name=excluded.group_name,file_name=excluded.file_name,file_size=excluded.file_size,file_type=excluded.file_type,
		file_path=excluded.file_path,file_hash=excluded.file_hash,telegram_media_kind=excluded.telegram_media_kind,
		telegram_media_id=excluded.telegram_media_id,telegram_media_size=excluded.telegram_media_size,
		telegram_mime=excluded.telegram_mime,media_width=excluded.media_width,media_height=excluded.media_height,
		media_duration_ms=excluded.media_duration_ms,forward_origin=excluded.forward_origin,telegram_thumb=excluded.telegram_thumb,
		ttl_seconds=COALESCE(excluded.ttl_seconds,ttl_seconds),status='completed' RETURNING id`,
		item.GroupID, item.GroupName, item.MessageID, item.Name, item.Identity.Size, item.Type, filepath.ToSlash(media.path), media.sha256, item.Identity.Kind, item.Identity.ID, item.Identity.Size,
		nullString(facts.Mime), nullInt(int64(facts.Width)), nullInt(int64(facts.Height)), nullInt(facts.DurationMs), nullString(facts.Origin), nullBytes(facts.Thumb), nullInt(int64(item.TTLSeconds))).Scan(&id)
	if err != nil {
		return Record{}, err
	}
	// A source-delete receipt can arrive before or during the byte transfer.
	// Apply its state in the same transaction as catalog publication/recovery.
	if _, err := tx.ExecContext(ctx, `UPDATE downloads SET
      pending_until=CASE WHEN rescued_at IS NOT NULL OR (SELECT rescued_at FROM tgdl_rescue_messages WHERE group_id=? AND message_id=?) IS NOT NULL THEN NULL
        WHEN ? THEN (SELECT pending_until FROM tgdl_rescue_messages WHERE group_id=? AND message_id=?) ELSE pending_until END,
      rescued_at=COALESCE(rescued_at,(SELECT rescued_at FROM tgdl_rescue_messages WHERE group_id=? AND message_id=?))
      WHERE id=? AND EXISTS(SELECT 1 FROM tgdl_rescue_messages WHERE group_id=? AND message_id=?)`,
		item.GroupID, item.MessageID, inserted, item.GroupID, item.MessageID, item.GroupID, item.MessageID, id, item.GroupID, item.MessageID); err != nil {
		return Record{}, err
	}
	if previousPath.Valid && (!previousHash.Valid || !strings.EqualFold(previousHash.String, media.sha256)) {
		if err := l.InvalidateDerived(ctx, tx, id); err != nil {
			return Record{}, err
		}
	}
	if previousPath.Valid && previousPath.String != "" && previousPath.String != media.path {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tgdl_file_cleanup(path) VALUES(?)`, previousPath.String); err != nil {
			return Record{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tgdl_verified_media(path,size,mtime_ns,sha256,fingerprint) VALUES(?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET size=excluded.size,mtime_ns=excluded.mtime_ns,sha256=excluded.sha256,fingerprint=excluded.fingerprint`, filepath.ToSlash(media.path), info.Size(), info.ModTime().UnixNano(), media.sha256, fingerprint); err != nil {
		return Record{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tgdl_ingest_files WHERE path=?`, filepath.ToSlash(media.path)); err != nil {
		return Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, err
	}
	return Record{ID: id, Path: filepath.ToSlash(media.path), SHA256: media.sha256, Reused: reused}, l.CleanDerived(ctx)
}

// Unknown facts stay NULL so "not reported" never matches as zero.
func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullInt(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

func nullBytes(v []byte) any {
	if len(v) == 0 {
		return nil
	}
	return v
}
