package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"

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
// What is checked, by kind:
//   - Media (video, audio, GIF, image files, by type or MIME), from 256 KiB:
//     Telegram's part hashes, a small request. Below 2 MiB every part's hash
//     must match and nothing else is tried (sampling would cost about as
//     much as the file). From 2 MiB, hashes at the start, middle and end,
//     or, when Telegram keeps none, three 1 MiB parts (more over 256 MiB).
//   - Other documents, from 256 KiB: Telegram's hash of every part. A
//     document can be edited in place without changing its length, so
//     samples are not enough; with no hashes it is downloaded.
//   - Compressed photos: Telegram re-encodes photos on upload, so the same
//     picture posted again is mostly a forward (the same Telegram file, not
//     downloaded at all) or different bytes. A byte-identical re-upload is
//     likely only when a saved photo has the same size, the same pixel size
//     and the very same stripped preview. Only then: from 256 KiB Telegram's
//     hash of every part, and from 1 MiB, when it has none, four 64 KiB
//     ranges (at most a quarter of the photo).
//
// From 256 KiB a file also waits while another of exactly its size is
// downloading (download.Library.SubstituteFrom), so that a copy queued next
// to its original is checked against it instead of downloaded alongside.
//
// What each check found is counted per day (ix_dedup.go).

const (
	ixSubstMinSize         = 256 << 10 // smaller files are simply downloaded
	ixSubstSampleSize      = 2 << 20   // media from here may be sampled in 1 MiB parts
	ixSubstPhotoSampleSize = 1 << 20   // photos from here may be sampled in small ranges
	ixSubstPhotoRange      = 64 << 10
	ixSubstHashChunk       = 131072 // upload.getFileHashes offsets are multiples of this
	ixSubstMaxHashCalls    = 64     // per file: beyond that it is downloaded
)

type ixRemoteFile interface {
	FileHashes(ctx context.Context, offset int64) ([]tg.FileHash, error)
	ReadPart(ctx context.Context, offset int64) ([]byte, error)
}

// ixRangeReader reads less than a 1 MiB part (telegram.ValidRange).
type ixRangeReader interface {
	ReadRange(ctx context.Context, offset int64, limit int) ([]byte, error)
}

// ixSubstPlan is how one file may be checked.
type ixSubstPlan struct {
	photo      bool
	fullHashes bool // every part's hash must match
	sample     int  // 0: hashes only; else the size of the samples read
}

func ixSubstPlanFor(item download.Item) (ixSubstPlan, bool) {
	size := item.Identity.Size
	if size < ixSubstMinSize {
		return ixSubstPlan{}, false
	}
	if item.Identity.Kind == "photo" {
		p := ixSubstPlan{photo: true, fullHashes: true}
		if size >= ixSubstPhotoSampleSize {
			p.sample = ixSubstPhotoRange
		}
		return p, true
	}
	mime := strings.ToLower(item.Facts.Mime)
	switch {
	case item.Type == "video" || item.Type == "audio" || item.Type == "voice" || item.Type == "photo" || item.Type == "gif":
	case strings.HasPrefix(mime, "video/") || strings.HasPrefix(mime, "audio/") || strings.HasPrefix(mime, "image/"):
	default:
		return ixSubstPlan{fullHashes: true}, true
	}
	if size < ixSubstSampleSize {
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
	for _, off := range []int64{0, x.size / 2, x.size - 1} {
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
	}
	return true, true
}

func (x *ixRemote) sampleOffsets(limit int) []int64 {
	l := int64(limit)
	last := (x.size - 1) / l * l
	var offs []int64
	if limit == telegram.PartSize {
		offs = []int64{0, (x.size / 2) / l * l, last}
		if x.size > 256<<20 {
			for i := int64(1); i <= 5; i++ {
				offs = append(offs, (x.size*i/6)/l*l)
			}
		}
	} else {
		offs = []int64{0, (x.size / 3) / l * l, (x.size * 2 / 3) / l * l, last}
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
	var data []byte
	var err error
	if limit == telegram.PartSize {
		data, err = x.r.ReadPart(ctx, off)
	} else if rr, ok := x.r.(ixRangeReader); ok {
		data, err = rr.ReadRange(ctx, off, limit)
	} else {
		return nil, false
	}
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
	if plan.sample != telegram.PartSize {
		how = "sampled-ranges"
	}
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
	plan, ok := ixSubstPlanFor(item)
	if !ok || !a.ixLoadSettings(ctx).SubstExact {
		return "", "", false
	}
	remote, ok := client.(ixRemoteFile)
	if !ok {
		return "", "", false
	}
	var blocked int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_subst_block WHERE identity=?`, item.Identity.Key()).Scan(&blocked)
	if blocked > 0 {
		return "", "", false
	}
	paths := a.ixSubstCandidates(ctx, item, plan)
	if len(paths) == 0 {
		return "", "", false
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
			return stored, how, true
		}
		if how == "" {
			break // Telegram cannot tell: the other candidates would not fare better
		}
	}
	return "", "", false
}

// ixSubstituteFrom: the smallest file checked, 0 when the check is off
// (the library then lets same-size files download at the same time).
func (a *App) ixSubstituteFrom(ctx context.Context) int64 {
	if !a.ixLoadSettings(ctx).SubstExact {
		return 0
	}
	return ixSubstMinSize
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
