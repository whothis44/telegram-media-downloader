package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/download"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// Exact re-uploads, checked on Telegram before downloading (ix_held.go, 1).
//
// It runs when a queued file is about to be downloaded (library.Ingest,
// after the queue has claimed it and refreshed its message, so the file
// reference is fresh), for every origin, and only when a saved file has
// exactly its size. A file is never skipped on a guess: Telegram's part
// hashes, or parts downloaded from Telegram, must match the saved file.
//
// A wrong "same bytes" loses the file for good (it is never downloaded,
// and a message that had an entry is repointed and its own file deleted),
// so only compressed video, which changes throughout when it differs at
// all, is ever judged on part of its bytes. What is checked, by kind:
//   - Video (MIME video/*), from 256 KiB: Telegram's part hashes, a small
//     request. Below 2 MiB the hash of every part must match. From 2 MiB,
//     the hashes Telegram returns for the start, middle and end (every one
//     of them, not just the part at each place), or, when Telegram keeps
//     none, three 1 MiB parts (more over 256 MiB).
//   - Everything else (pictures, audio, documents, compressed photos), from
//     256 KiB: Telegram's hash of every part. Uncompressed audio and
//     pictures (WAV, BMP, TIFF scans) can be equal in long silent or white
//     stretches and differ elsewhere, and a document can be edited in place
//     without changing its length. Without hashes the file is downloaded.
//   - Compressed photos are checked only against a saved photo with the
//     same size, pixel size and stripped preview (Telegram re-encodes
//     photos, so a byte-identical one is rare otherwise).
//
// A file whose identity is set to be downloaded for real (ix_subst_block,
// "Fetch the real files") is never checked or matched by identity.
//
// From 256 KiB a video or photo also waits, at most 5 minutes, while
// another of exactly its size, kind and MIME type is downloading
// (ixSubstituteWait), so that a copy queued next to its original is
// checked against it instead of downloaded alongside.
//
// What each check found is counted per day (ix_dedup.go).

const (
	ixSubstMinSize      = 256 << 10 // smaller files are simply downloaded
	ixSubstSampleSize   = 2 << 20   // video from here may be judged on parts
	ixSubstHashChunk    = 131072    // upload.getFileHashes offsets are multiples of this
	ixSubstMaxHashCalls = 64        // per file: beyond that it is downloaded
)

type ixRemoteFile interface {
	FileHashes(ctx context.Context, offset int64) ([]tg.FileHash, error)
	ReadPart(ctx context.Context, offset int64) ([]byte, error)
}

// ixSubstPlan is how one file may be checked.
type ixSubstPlan struct {
	photo      bool
	fullHashes bool // every part's hash must match
	sample     int  // 0: hashes only; else the size of the samples read (video only)
}

// ixIsVideo: compressed video, by MIME type (Telegram gives every document
// one; a GIF picture, image/gif, is not video).
func ixIsVideo(item download.Item) bool {
	mime := strings.ToLower(strings.TrimSpace(item.Facts.Mime))
	return strings.HasPrefix(mime, "video/") || (mime == "" && item.Identity.Kind == "document" && item.Type == "video")
}

func ixSubstPlanFor(item download.Item) (ixSubstPlan, bool) {
	size := item.Identity.Size
	if size < ixSubstMinSize {
		return ixSubstPlan{}, false
	}
	if item.Identity.Kind == "photo" {
		return ixSubstPlan{photo: true, fullHashes: true}, true
	}
	if !ixIsVideo(item) || size < ixSubstSampleSize {
		return ixSubstPlan{fullHashes: true}, true
	}
	return ixSubstPlan{sample: telegram.PartSize}, true
}

// ixRemote is one file on Telegram while it is compared with saved files:
// what Telegram answered is kept for the next candidate.
type ixRemote struct {
	r        ixRemoteFile
	size     int64
	hashes   []tg.FileHash
	calls    int
	noHashes bool
	samples  map[int64][]byte
	sampled  int64 // bytes downloaded for samples
	failed   bool  // a sample could not be read
}

func newIxRemote(r ixRemoteFile, size int64) *ixRemote {
	return &ixRemote{r: r, size: size, samples: map[int64][]byte{}}
}

func (x *ixRemote) covering(off int64) (tg.FileHash, bool) {
	for _, h := range x.hashes {
		if h.Offset <= off && off < h.Offset+int64(h.Limit) {
			return h, true
		}
	}
	return tg.FileHash{}, false
}

// hashAt is Telegram's hash of the part holding off.
func (x *ixRemote) hashAt(ctx context.Context, off int64) (tg.FileHash, bool) {
	if h, ok := x.covering(off); ok {
		return h, true
	}
	if x.noHashes || x.calls >= ixSubstMaxHashCalls || ctx.Err() != nil {
		return tg.FileHash{}, false
	}
	x.calls++
	hs, err := x.r.FileHashes(ctx, off/ixSubstHashChunk*ixSubstHashChunk)
	if err != nil || len(hs) == 0 {
		x.noHashes = true
		return tg.FileHash{}, false
	}
	for _, h := range hs {
		if h.Limit > 0 && h.Offset >= 0 && h.Offset < x.size && len(h.Hash) == sha256.Size {
			x.hashes = append(x.hashes, h)
		}
	}
	return x.covering(off)
}

// byHashes compares local with Telegram's hashes: decided is false when
// Telegram did not answer for every place asked.
func (x *ixRemote) byHashes(ctx context.Context, local *os.File, full bool) (decided, same bool) {
	check := func(h tg.FileHash) (bool, error) {
		n := min(int64(h.Limit), x.size-h.Offset)
		buf := make([]byte, n)
		if _, err := local.ReadAt(buf, h.Offset); err != nil {
			return false, err
		}
		sum := sha256.Sum256(buf)
		return bytes.Equal(sum[:], h.Hash), nil
	}
	if full {
		for off := int64(0); off < x.size; {
			h, ok := x.hashAt(ctx, off)
			if !ok {
				return false, false
			}
			match, err := check(h)
			if err != nil {
				return false, false
			}
			if !match {
				return true, false
			}
			off = h.Offset + int64(h.Limit)
		}
		return true, true
	}
	// Ask at the start, middle and end, then compare every hash Telegram
	// returned, not only the part at each of those places.
	for _, off := range []int64{0, x.size / 2, x.size - 1} {
		if _, ok := x.hashAt(ctx, off); !ok {
			return false, false
		}
	}
	for _, h := range x.hashes {
		match, err := check(h)
		if err != nil {
			return false, false
		}
		if !match {
			return true, false
		}
	}
	return true, true
}

func (x *ixRemote) sampleOffsets(limit int) []int64 {
	l := int64(limit)
	last := (x.size - 1) / l * l
	offs := []int64{0, (x.size / 2) / l * l, last}
	if x.size > 256<<20 {
		for i := int64(1); i <= 5; i++ {
			offs = append(offs, (x.size*i/6)/l*l)
		}
	}
	seen := map[int64]bool{}
	out := offs[:0]
	for _, o := range offs {
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	return out
}

func (x *ixRemote) read(ctx context.Context, off int64, limit int) ([]byte, bool) {
	if data, ok := x.samples[off]; ok {
		return data, true
	}
	if limit != telegram.PartSize {
		return nil, false
	}
	data, err := x.r.ReadPart(ctx, off)
	if err != nil {
		x.failed = true
		return nil, false
	}
	x.sampled += int64(len(data))
	x.samples[off] = data
	return data, true
}

// bySamples compares local with parts downloaded from Telegram.
func (x *ixRemote) bySamples(ctx context.Context, local *os.File, limit int) (decided, same bool) {
	for _, off := range x.sampleOffsets(limit) {
		data, ok := x.read(ctx, off, limit)
		if !ok {
			return false, false
		}
		want := min(int64(limit), x.size-off)
		if int64(len(data)) != want {
			return true, false
		}
		buf := make([]byte, want)
		if _, err := local.ReadAt(buf, off); err != nil {
			return false, false
		}
		if !bytes.Equal(buf, data) {
			return true, false
		}
	}
	return true, true
}

// same compares local (as large as the remote file) with it: Telegram's
// hashes first, then samples where the plan allows. how names the check
// that decided ("" when none could).
func (x *ixRemote) same(ctx context.Context, local *os.File, plan ixSubstPlan) (how string, same bool) {
	if decided, ok := x.byHashes(ctx, local, plan.fullHashes); decided {
		return "telegram-hashes", ok
	}
	if plan.sample == 0 || ctx.Err() != nil {
		return "", false
	}
	how = "sampled-parts"
	if decided, ok := x.bySamples(ctx, local, plan.sample); decided {
		return how, ok
	}
	return "", false
}

// ixSameBytes compares a file on Telegram with a local file of the same
// size as for media from 2 MiB: Telegram's hashes, else sampled parts.
func ixSameBytes(ctx context.Context, remote ixRemoteFile, local *os.File, size int64) (string, bool) {
	how, ok := newIxRemote(remote, size).same(ctx, local, ixSubstPlan{sample: telegram.PartSize})
	if how == "" {
		return "", false
	}
	return how, ok
}

// ixSubstitute is the library's hook: the relative path of a saved file
// with exactly this item's bytes, how that was confirmed, or false.
func (a *App) ixSubstitute(ctx context.Context, item download.Item, client download.Client) (string, string, bool) {
	// Set to be downloaded for real ("Fetch the real files"): no check, and
	// no match by identity either.
	if a.ixSubstBlocked(ctx, item) {
		return "", "", false
	}
	// The same Telegram file as a message that shares a saved file without
	// an entry of its own (download.skipDuplicates): the library does not
	// know this identity, the link does (ix_links.go).
	if path, ok := a.ixLinkedIdentity(ctx, item); ok {
		return path, download.DedupTelegramFile, true
	}
	plan, ok := ixSubstPlanFor(item)
	if !ok || !a.ixLoadSettings(ctx).SubstExact {
		return "", "", false
	}
	remote, ok := client.(ixRemoteFile)
	if !ok {
		return "", "", false
	}
	// Asked already for this message a moment ago (the low-quality-first
	// check asks just before the library does): the same answer, without
	// asking Telegram or counting it again.
	memoKey := item.GroupID + "\x00" + strconv.FormatInt(item.MessageID, 10) + "\x00" + item.Identity.Key()
	if r, ok := a.ixSubstMemo().get(memoKey); ok {
		return r.path, r.how, r.ok
	}
	path, how, same, checked := a.ixSubstCheck(ctx, item, remote, plan)
	// Only a "same" is kept: a copy may finish downloading in between (the
	// library's same-size wait), and the next look must see it.
	if checked && same {
		a.ixSubstMemo().put(memoKey, ixSubstAnswer{path: path, how: how, ok: same})
	}
	return path, how, same
}

// ixSubstBlocked: the item's identity is set to be downloaded for real.
func (a *App) ixSubstBlocked(ctx context.Context, item download.Item) bool {
	var blocked int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_subst_block WHERE identity=?`, item.Identity.Key()).Scan(&blocked)
	return blocked > 0
}

type ixSubstAnswer struct {
	path, how string
	ok        bool
	at        time.Time
}

// ixSubstMemos keeps each App's recent answers for ten minutes.
type ixSubstMemos struct {
	mu sync.Mutex
	m  map[string]ixSubstAnswer
}

var ixSubstMemoBoxes sync.Map // *App -> *ixSubstMemos

func (a *App) ixSubstMemo() *ixSubstMemos {
	v, _ := ixSubstMemoBoxes.LoadOrStore(a, &ixSubstMemos{m: map[string]ixSubstAnswer{}})
	return v.(*ixSubstMemos)
}

const ixSubstMemoFor = 10 * time.Minute

func (s *ixSubstMemos) get(key string) (ixSubstAnswer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[key]
	if !ok || time.Since(r.at) > ixSubstMemoFor {
		return ixSubstAnswer{}, false
	}
	return r, true
}

func (s *ixSubstMemos) put(key string, r ixSubstAnswer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) >= 512 {
		for k, old := range s.m {
			if time.Since(old.at) > ixSubstMemoFor {
				delete(s.m, k)
			}
		}
	}
	if len(s.m) < 4096 {
		r.at = time.Now()
		s.m[key] = r
	}
}

// ixSubstCheck compares the item with the saved files of its size on
// Telegram; checked is false when there was nothing to compare with.
func (a *App) ixSubstCheck(ctx context.Context, item download.Item, remote ixRemoteFile, plan ixSubstPlan) (string, string, bool, bool) {
	paths := a.ixSubstCandidates(ctx, item, plan)
	if len(paths) == 0 {
		return "", "", false, false
	}
	size := item.Identity.Size
	x := newIxRemote(remote, size)
	checked := false
	defer func() {
		if !checked {
			return
		}
		bg := context.WithoutCancel(ctx)
		a.ixCountDedup(bg, ixDedupCheck, 1, x.sampled)
		if x.noHashes {
			a.ixCountDedup(bg, ixDedupCheckNoHash, 1, 0)
		}
		if x.failed {
			a.ixCountDedup(bg, ixDedupCheckFailed, 1, 0)
		}
	}()
	for _, stored := range paths {
		rel := ixNormPath(stored)
		if rel == "" {
			continue
		}
		f, err := os.Open(filepath.Join(a.downloadsDir, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		info, err := f.Stat()
		if err != nil || info.Size() != size {
			f.Close()
			continue
		}
		checked = true
		how, same := x.same(ctx, f, plan)
		f.Close()
		if same {
			a.ixLog("not downloading %s #%d: same bytes as %s (%s)", item.GroupID, item.MessageID, rel, how)
			return stored, how, true, true
		}
		if how == "" {
			break // Telegram cannot tell: the other candidates would not fare better
		}
	}
	return "", "", false, checked
}

// ixPartName: a numbered part of a split file (name.part01.rar, name.r00,
// name.z01, name.7z.001, name.001). Parts are all the same size and are
// not copies of each other.
var ixPartName = regexp.MustCompile(`(?i)(\.part\d+\.[a-z0-9]+|\.[rz]\d{2}|\.\d{3})$`)

// ixSubstituteWait is the library's same-size wait hook: the key under
// which this item waits for a download of exactly its size, kind and MIME
// type, or false. Only for what the check can confirm and a copy is likely
// for: videos and photos from 256 KiB, not numbered parts of split files,
// and only with the check on and no staging folder (a staged file is not
// in the library yet, so the check could not see it anyway).
func (a *App) ixSubstituteWait(ctx context.Context, item download.Item) (string, bool) {
	if item.Identity.Size < ixSubstMinSize || ixPartName.MatchString(strings.TrimSpace(item.Name)) {
		return "", false
	}
	mime := strings.ToLower(strings.TrimSpace(item.Facts.Mime))
	if item.Identity.Kind != "photo" && !ixIsVideo(item) {
		return "", false
	}
	s := a.ixLoadSettings(ctx)
	if !s.SubstExact || s.StageDir != "" {
		return "", false
	}
	return item.Identity.Kind + "\x00" + mime + "\x00" + strconv.FormatInt(item.Identity.Size, 10), true
}

// ixSubstCandidates: saved files of exactly the item's size (for a photo,
// also its pixel size and stripped preview), at most five.
func (a *App) ixSubstCandidates(ctx context.Context, item download.Item, plan ixSubstPlan) []string {
	size := item.Identity.Size
	var q string
	var args []any
	if plan.photo {
		if item.Facts.Width <= 0 || item.Facts.Height <= 0 || len(item.Facts.Thumb) == 0 {
			return nil
		}
		q = `SELECT DISTINCT file_path FROM downloads WHERE file_size=? AND media_width=? AND media_height=? AND telegram_thumb=? AND status='completed' AND file_path IS NOT NULL AND file_path<>'' AND NOT (group_id=? AND message_id=?) LIMIT 5`
		args = []any{size, item.Facts.Width, item.Facts.Height, item.Facts.Thumb, item.GroupID, item.MessageID}
	} else {
		// Two lookups rather than one OR: each uses its index (ix_downloads_size,
		// ix_downloads_tg_size) instead of reading the whole catalogue.
		q = `SELECT file_path FROM downloads WHERE file_size=? AND status='completed' AND file_path IS NOT NULL AND file_path<>'' AND NOT (group_id=? AND message_id=?)
		UNION SELECT file_path FROM downloads WHERE telegram_media_size=? AND status='completed' AND file_path IS NOT NULL AND file_path<>'' AND NOT (group_id=? AND message_id=?)
		LIMIT 5`
		args = []any{size, item.GroupID, item.MessageID, size, item.GroupID, item.MessageID}
	}
	rows, err := a.db.Reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			paths = append(paths, p)
		}
	}
	return paths
}
