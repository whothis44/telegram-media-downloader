package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/jpeg"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/download"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// Saving downloads on copies, without losing anything.
//
// The downloader already never downloads the same Telegram file twice (a
// forward points at the copy it has). Two more cases are handled here:
//
//  1. Exact re-uploads: someone uploaded the same bytes again as a new file.
//     When a queued file is exactly as large as one already saved, Telegram
//     is asked for the SHA-256 of parts of it (upload.getFileHashes) and
//     those are compared with the saved file; when Telegram keeps no hashes,
//     three or more 1 MiB parts are downloaded and compared instead. If all
//     agree, the message points at the saved file, as for a forward, and the
//     substitution is recorded (ix_subst). On by default.
//  2. Probable copies: a queued video as long as one already saved (to half
//     a second), the same shape, no larger in pixels or bytes, with a
//     matching preview picture. These are not skipped: they are held at the
//     back of the queue (paused) and released once nothing else is waiting,
//     so unique files come first while time is short (ix_held). On by
//     default; releasing can be turned off.
//  3. Copies waiting together (ix.10): videos alike by the same check that
//     are all still in the queue. Only the best (most pixels, then the
//     longest, then most bytes, then the earliest post) goes first; the
//     others wait for it (ix_copy_hold.of_work). Once it is saved they are
//     checked against the saved file, as in 2; if it fails for good, the
//     best of them goes instead. With image recognition, previews alike
//     count too (ix_copies_ml.go), and frames sampled across a big video
//     can confirm the copy (ix_copies_sample.go): confirmed copies wait
//     until everything else is done unless the setting says otherwise.
//
// Held copies do not count towards a backfill's queue limit (ixHeldWaiting):
// before ix.10 they filled it, so the backfill stopped bringing new files,
// the queue emptied of everything else and the copies were let go.
//
// Every message is archived in full either way (ix_messages keeps
// Telegram's complete record, with the file's identity), so any of these
// files can still be fetched later.

const (
	ixSubstMinSize = 2 << 20 // smaller files are simply downloaded
	ixHoldMinSize  = 5 << 20
)

var ixStrippedHeader = func() []byte {
	b, _ := hex.DecodeString("ffd8ffe000104a46494600010100000100010000ffdb004300281c1e231e19282321232d2b28303c64413c37373c7b585d4964918099968f808c8aa0b4e6c3a0aadaad8a8cc8ffcbdaeef5ffffff9bc1fffffffaffe6fdfff8ffdb0043012b2d2d3c353c76414176f8a58ca5f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8f8ffc00011080000000003012200021101031101ffc4001f0000010501010101010100000000000000000102030405060708090a0bffc400b5100002010303020403050504040000017d01020300041105122131410613516107227114328191a1082342b1c11552d1f02433627282090a161718191a25262728292a3435363738393a434445464748494a535455565758595a636465666768696a737475767778797a838485868788898a92939495969798999aa2a3a4a5a6a7a8a9aab2b3b4b5b6b7b8b9bac2c3c4c5c6c7c8c9cad2d3d4d5d6d7d8d9dae1e2e3e4e5e6e7e8e9eaf1f2f3f4f5f6f7f8f9faffc4001f0100030101010101010101010000000000000102030405060708090a0bffc400b51100020102040403040705040400010277000102031104052131061241510761711322328108144291a1b1c109233352f0156272d10a162434e125f11718191a262728292a35363738393a434445464748494a535455565758595a636465666768696a737475767778797a82838485868788898a92939495969798999aa2a3a4a5a6a7a8a9aab2b3b4b5b6b7b8b9bac2c3c4c5c6c7c8c9cad2d3d4d5d6d7d8d9dae2e3e4e5e6e7e8e9eaf2f3f4f5f6f7f8f9faffda000c03010002110311003f00")
	return b
}()

// ixInflateStripped turns Telegram's "stripped" preview (a JPEG without its
// standard header) back into a JPEG.
func ixInflateStripped(b []byte) []byte {
	if len(b) < 3 || b[0] != 1 || len(ixStrippedHeader) != 623 {
		return nil
	}
	out := append([]byte(nil), ixStrippedHeader...)
	out[164], out[166] = b[1], b[2]
	out = append(out, b[3:]...)
	return append(out, 0xff, 0xd9)
}

// ixPreviewHash is a 64-bit hash of a stripped preview (0, false when there
// is none or it says nothing).
func ixPreviewHash(stripped []byte) (uint64, bool) {
	data := ixInflateStripped(stripped)
	if data == nil {
		return 0, false
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, false
	}
	b := img.Bounds()
	if b.Dx() < 4 || b.Dy() < 4 {
		return 0, false
	}
	px := make([]byte, 1024)
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			c := image.Point{b.Min.X + x*b.Dx()/32, b.Min.Y + y*b.Dy()/32}
			r, g, bl, _ := img.At(c.X, c.Y).RGBA()
			px[y*32+x] = byte((0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)) / 257)
		}
	}
	return ixPHash(px)
}

// ---------- 1. exact re-uploads ----------

type ixRemoteFile interface {
	FileHashes(ctx context.Context, offset int64) ([]tg.FileHash, error)
	ReadPart(ctx context.Context, offset int64) ([]byte, error)
}

// ixSubstitute is the library's hook: the relative path of a saved file
// with exactly this item's bytes, or false.
func (a *App) ixSubstitute(ctx context.Context, item download.Item, client download.Client) (string, bool) {
	size := item.Identity.Size
	if size < ixSubstMinSize {
		return "", false
	}
	// Sampled checks suit media files, which change throughout when re-encoded.
	// A document can be edited in place without changing its length, so
	// documents are always downloaded.
	mime := strings.ToLower(item.Facts.Mime)
	switch {
	case item.Type == "video" || item.Type == "audio" || item.Type == "voice" || item.Type == "photo":
	case strings.HasPrefix(mime, "video/") || strings.HasPrefix(mime, "audio/") || strings.HasPrefix(mime, "image/"):
	default:
		return "", false
	}
	if !a.ixLoadSettings(ctx).SubstExact {
		return "", false
	}
	remote, ok := client.(ixRemoteFile)
	if !ok {
		return "", false
	}
	var blocked int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_subst_block WHERE identity=?`, item.Identity.Key()).Scan(&blocked)
	if blocked > 0 {
		return "", false
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT DISTINCT file_path FROM downloads WHERE status='completed' AND file_path IS NOT NULL AND file_path<>''
		AND (file_size=? OR telegram_media_size=?) AND NOT (group_id=? AND message_id=?) LIMIT 5`, size, size, item.GroupID, item.MessageID)
	if err != nil {
		return "", false
	}
	var paths []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			paths = append(paths, p)
		}
	}
	rows.Close()
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
		how, same := ixSameBytes(ctx, remote, f, size)
		f.Close()
		if !same {
			continue
		}
		_, _ = a.db.Writer.ExecContext(ctx, `INSERT INTO ix_subst(group_id,message_id,identity,size,of_path,how,at) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(group_id,message_id) DO UPDATE SET identity=excluded.identity, size=excluded.size, of_path=excluded.of_path, how=excluded.how, at=excluded.at`,
			item.GroupID, item.MessageID, item.Identity.Key(), size, stored, how, time.Now().UnixMilli())
		a.ixLog("not downloading %s #%d: same bytes as %s (%s)", item.GroupID, item.MessageID, rel, how)
		return stored, true
	}
	return "", false
}

// ixSameBytes compares a file on Telegram with a local file of the same
// size, by Telegram's part hashes when it has them, by sampled parts
// otherwise. how names the method used.
func ixSameBytes(ctx context.Context, remote ixRemoteFile, local *os.File, size int64) (string, bool) {
	part := int64(telegram.PartSize)
	last := (size - 1) / part * part
	offsets := []int64{0, (size / 2) / part * part, last}
	if size > 256<<20 {
		for i := int64(1); i <= 5; i++ {
			offsets = append(offsets, (size*i/6)/part*part)
		}
	}
	// Telegram's own hashes: nothing to download.
	compared, hashesOK := 0, true
	for _, off := range []int64{0, (size / 2) / 131072 * 131072, (size - 1) / 131072 * 131072} {
		hs, err := remote.FileHashes(ctx, off)
		if err != nil || len(hs) == 0 {
			hashesOK = false
			break
		}
		for _, h := range hs {
			if h.Limit <= 0 || h.Offset < 0 || h.Offset >= size {
				continue
			}
			n := min(int64(h.Limit), size-h.Offset)
			buf := make([]byte, n)
			if _, err := local.ReadAt(buf, h.Offset); err != nil {
				return "", false
			}
			sum := sha256.Sum256(buf)
			if !bytes.Equal(sum[:], h.Hash) {
				return "telegram-hashes", false
			}
			compared++
		}
	}
	if hashesOK && compared >= 3 {
		return "telegram-hashes", true
	}
	if ctx.Err() != nil {
		return "", false
	}
	seen := map[int64]bool{}
	for _, off := range offsets {
		if seen[off] {
			continue
		}
		seen[off] = true
		data, err := remote.ReadPart(ctx, off)
		if err != nil {
			return "", false
		}
		want := min(part, size-off)
		if int64(len(data)) != want {
			return "sampled-parts", false
		}
		buf := make([]byte, want)
		if _, err := local.ReadAt(buf, off); err != nil {
			return "", false
		}
		if !bytes.Equal(buf, data) {
			return "sampled-parts", false
		}
	}
	return "sampled-parts", true
}

// ---------- 2. probable copies, held back ----------

type ixHeldStatus struct {
	Held     int64 `json:"held"`     // waiting now
	Released int64 `json:"released"` // let go once the queue was empty
	Subst    int64 `json:"subst"`    // exact re-uploads not downloaded
	SavedMB  int64 `json:"savedMb"`
}

func (a *App) ixHeldStatus(ctx context.Context) ixHeldStatus {
	var st ixHeldStatus
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(SUM(h.released_at=0 AND w.status='pending'),0), COALESCE(SUM(h.released_at>0),0)
		FROM ix_held h LEFT JOIN tgdl_work w ON w.id=h.work_id`).Scan(&st.Held, &st.Released)
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(s.size),0)/1048576 FROM ix_subst s JOIN downloads d ON d.group_id=s.group_id AND d.message_id=s.message_id AND d.file_path=s.of_path`).Scan(&st.Subst, &st.SavedMB)
	return st
}

// ixMessageFacts reads a message's media facts from its archived record.
func (a *App) ixMessageFacts(ctx context.Context, groupID string, messageID int64) (telegram.MediaFacts, bool) {
	var tl []byte
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT tl FROM ix_messages WHERE group_id=? AND message_id=? AND service=''`, groupID, messageID).Scan(&tl); err != nil || len(tl) == 0 {
		return telegram.MediaFacts{}, false
	}
	var msg tg.Message
	if msg.Decode(&bin.Buffer{Buf: tl}) != nil {
		return telegram.MediaFacts{}, false
	}
	att, err := telegram.MessageAttachment(&msg)
	if err != nil {
		return telegram.MediaFacts{}, false
	}
	return att.Facts, true
}

// ixLikelyCopy finds a saved video this one is very probably a copy of,
// and at least as good: same length to half a second, same shape, no
// fewer pixels, at least 90% of the bytes, and a matching preview.
func (a *App) ixLikelyCopy(ctx context.Context, f telegram.MediaFacts, size int64) (path, key string, ok bool) {
	ph, okHash := ixPreviewHash(f.Thumb)
	return a.ixLikelyCopyFacts(ctx, ixCopyFacts{Dur: f.DurationMs, W: f.Width, H: f.Height, Size: size, PH: ph, HasPH: okHash})
}

func (a *App) ixLikelyCopyFacts(ctx context.Context, q ixCopyFacts) (path, key string, ok bool) {
	f := telegram.MediaFacts{DurationMs: q.Dur, Width: q.W, Height: q.H}
	size, ph := q.Size, q.PH
	if f.DurationMs <= 0 || f.Width <= 0 || f.Height <= 0 || !q.HasPH {
		return "", "", false
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT file_path, COALESCE(file_hash,''), media_width, media_height, COALESCE(file_size,0), telegram_thumb FROM downloads
		WHERE file_type='video' AND status='completed' AND file_path IS NOT NULL AND file_path<>'' AND media_duration_ms BETWEEN ? AND ?
		AND media_width IS NOT NULL AND media_height IS NOT NULL AND telegram_thumb IS NOT NULL LIMIT 200`, f.DurationMs-500, f.DurationMs+500)
	if err != nil {
		return "", "", false
	}
	defer rows.Close()
	aspect := float64(f.Width) / float64(f.Height)
	for rows.Next() {
		var p, h string
		var w, hh, sz int64
		var thumb []byte
		if rows.Scan(&p, &h, &w, &hh, &sz, &thumb) != nil || hh == 0 {
			continue
		}
		if math.Abs(float64(w)/float64(hh)-aspect)/aspect > ixAspectSlack {
			continue
		}
		if w*hh < int64(f.Width)*int64(f.Height) || float64(sz) < 0.9*float64(size) {
			continue
		}
		th, ok := ixPreviewHash(thumb)
		if !ok || bits.OnesCount64(th^ph) > 6 {
			continue
		}
		return p, ixFileKey(h, ixNormPath(p)), true
	}
	return "", "", false
}

func (a *App) ixRunHeld(ctx context.Context) {
	// The first pass soon after start covers the whole queue as it is.
	wait := 30 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 2 * time.Minute
		if !ixVdupBackground.Load() {
			continue
		}
		if err := a.ixHeldStep(ctx); err != nil && ctx.Err() == nil {
			a.ixLog("held-back copies: %v", err)
		}
	}
}

// ixHeldStep holds new probable copies back, checks the held ones again,
// and lets them go once nothing else is waiting (or when holding is
// turned off).
func (a *App) ixHeldStep(ctx context.Context) error {
	s := a.ixLoadSettings(ctx)
	// Held copies first (one whose better copy was saved meanwhile is
	// checked against that file), then the new files.
	if err := a.ixHeldRecheck(ctx); err != nil {
		return err
	}
	if s.HoldLikely {
		if err := a.ixHeldTriage(ctx); err != nil {
			return err
		}
	}
	if !s.HoldLikely {
		return a.ixReleaseSome(ctx, 25, true)
	}
	if s.HoldForever {
		return nil
	}
	// Only files the engine could take now count: not ones waiting to
	// retry later. And when nothing has finished for half an hour while
	// copies are held, they go too rather than wait behind a stuck queue.
	var waiting int
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM tgdl_work WHERE status='pending' AND paused=0 AND retry_at<=?`, time.Now().UnixMilli()).Scan(&waiting); err != nil {
		return err
	}
	// Files set aside so that smaller ones go first (deadline order)
	// still count as waiting: held copies go after them.
	waiting += a.ixDeferredWaiting(ctx)
	release := waiting == 0
	if !release {
		var last int64
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(MAX(updated_at),0) FROM tgdl_work WHERE status='completed'`).Scan(&last)
		release = last > 0 && time.Since(time.UnixMilli(last)) > 30*time.Minute
	}
	if !release {
		return nil
	}
	// Just before they would go, big held videos not checked yet are
	// checked by sampling (ix_copies_sample.go): a confirmed copy stays.
	ixBeforeReleaseHook(a, ctx)
	return a.ixReleaseSome(ctx, 25, s.ConfirmedGoEarly || a.ixOnlyConfirmedLeft(ctx))
}

// ixBeforeReleaseHook runs before held copies are let go.
var ixBeforeReleaseHook = func(a *App, ctx context.Context) {}

// ixQueuedVideo is a video in the download queue, as the checks see it.
type ixQueuedVideo struct {
	f        ixCopyFacts
	status   string
	paused   bool
	deferred bool   // set aside by the deadline order
	held     bool   // held now
	released int64  // ix_held.released_at: -1 looked at, >0 let go
	how      string // why it is held
	of       int64  // held as a lesser copy of this queue item
	known    bool   // has an ix_held row
}

func (q *ixQueuedVideo) holdable() bool {
	return q.status == "pending" && !q.held && q.released <= 0 && (!q.paused || q.deferred)
}

// ixQueuedVideos lists the videos waiting in the queue (or downloading)
// that are large enough to be worth holding, with their facts.
func (a *App) ixQueuedVideos(ctx context.Context) ([]*ixQueuedVideo, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT w.id, w.group_id, w.message_id, w.file_size, w.body, w.status, w.paused,
			EXISTS (SELECT 1 FROM ix_deferred x WHERE x.work_id=w.id AND x.released_at=0),
			h.work_id IS NOT NULL, COALESCE(h.reason,''), COALESCE(h.released_at,0), COALESCE(c.how,''), COALESCE(c.of_work,0)
		FROM tgdl_work w LEFT JOIN ix_held h ON h.work_id=w.id LEFT JOIN ix_copy_hold c ON c.work_id=w.id
		WHERE w.status IN ('pending','processing') AND w.media_type='video' AND w.file_size>=? AND w.ttl_seconds=0 ORDER BY w.file_size DESC, w.id LIMIT 20000`, ixHoldMinSize)
	if err != nil {
		return nil, err
	}
	var out []*ixQueuedVideo
	for rows.Next() {
		q := &ixQueuedVideo{}
		var group, reason string
		var msg, size int64
		var body []byte
		if rows.Scan(&q.f.ID, &group, &msg, &size, &body, &q.status, &q.paused, &q.deferred, &q.known, &reason, &q.released, &q.how, &q.of) != nil {
			continue
		}
		q.held = q.known && reason != "" && q.released == 0
		if q.held && q.how == "" {
			q.how = "saved" // held before ix.10
		}
		id := q.f.ID
		if m, ok := ixDecodeMessage(body); ok {
			if f, ok := ixCopyFactsOf(m); ok {
				q.f = f
			}
		}
		q.f.ID, q.f.Group, q.f.Msg, q.f.Size, q.f.Type = id, group, int(msg), size, "video"
		out = append(out, q)
	}
	rows.Close()
	for _, q := range out {
		if q.f.Ident == "" { // no queued record: the archived one
			if facts, ok := a.ixMessageFacts(ctx, q.f.Group, int64(q.f.Msg)); ok {
				q.f.Dur, q.f.W, q.f.H = facts.DurationMs, facts.Width, facts.Height
				q.f.PH, q.f.HasPH = ixPreviewHash(facts.Thumb)
			}
		}
	}
	a.ixAttachVectors(ctx, out)
	return out, rows.Err()
}

// ixAttachVectorsHook adds the recognition vectors of the queued files'
// Telegram previews, when they were looked at (ix_copies_ml.go).
var ixAttachVectorsHook = func(a *App, ctx context.Context, qs []*ixQueuedVideo) {}

func (a *App) ixAttachVectors(ctx context.Context, qs []*ixQueuedVideo) { ixAttachVectorsHook(a, ctx, qs) }

// ixSameQueuedHook: two queued videos are very probably the same: the
// cheap check, or (with image recognition, ix_copies_ml.go) previews
// alike, as long to 2% and the same shape.
var ixSameQueuedHook = func(a *App, x, y ixCopyFacts) bool { return ixCheapSame(x, y) }

// ixHeldTriage holds new probable copies: videos like a saved one, and,
// of videos alike that wait in the queue together, all but the best.
func (a *App) ixHeldTriage(ctx context.Context) error {
	qs, err := a.ixQueuedVideos(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	// 1. Copies of saved videos. Every waiting file is checked each round,
	// largest first, so files queued before this check existed (or before
	// their copy was saved) are covered too.
	saved, err := a.ixSavedVideoFacts(ctx)
	if err != nil {
		return err
	}
	var group []ixCopyFacts
	var members []*ixQueuedVideo
	for _, q := range qs {
		if q.holdable() {
			if i := ixSavedCopyOf(saved, q.f); i >= 0 {
				ok, err := a.ixHoldWork(ctx, q.f, "saved", saved[i].Path, saved[i].Key, 0, 0, "looks like a saved copy")
				if err != nil {
					return err
				}
				if ok {
					q.held, q.how, q.known = true, "saved", true
					continue
				}
			}
		}
		if q.held && q.how != "queued" {
			continue // held as a copy of a saved file
		}
		if q.f.Dur > 0 && (q.f.HasPH || q.f.Vec != nil) && q.f.W > 0 && q.f.H > 0 {
			group = append(group, q.f)
			members = append(members, q)
		}
	}
	// 2. Videos alike waiting together: the best goes first.
	same := func(x, y ixCopyFacts) bool { return ixSameQueuedHook(a, x, y) }
	for _, g := range ixGroupCopies(group, same, 500) {
		best := members[g[0]]
		if best.held {
			// Its better copy is not waiting any more (saved, failed or
			// gone): the check of held copies decides what happens.
			continue
		}
		for _, i := range g[1:] {
			q := members[i]
			switch {
			case q.held && q.of != best.f.ID:
				if _, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_copy_hold SET of_work=? WHERE work_id=?`, best.f.ID, q.f.ID); err != nil {
					return err
				}
			case q.holdable():
				sim := 0.0 // likeness of the previews, when recognition looked at both
				if !ixCheapSame(q.f, best.f) {
					sim = ixCosine(q.f.Vec, best.f.Vec)
				}
				ok, err := a.ixHoldWork(ctx, q.f, "queued", "", "", best.f.ID, sim, "looks like a copy of a queued file")
				if err != nil {
					return err
				}
				if ok {
					q.held, q.known = true, true
				}
			}
		}
	}
	// 3. The others are marked looked at.
	for _, q := range qs {
		if q.known || q.status != "pending" {
			continue
		}
		if _, err := a.db.Writer.ExecContext(ctx, `INSERT OR IGNORE INTO ix_held(work_id,group_id,message_id,reason,held_at,released_at) VALUES(?,?,?,'',?,-1)`, q.f.ID, q.f.Group, q.f.Msg, now); err != nil {
			return err
		}
	}
	return nil
}

// ixHoldWork holds a queue item back as a probable copy (false when it was
// taken, paused by a person, or let go before).
func (a *App) ixHoldWork(ctx context.Context, f ixCopyFacts, how, likePath, likeKey string, ofWork int64, sim float64, reason string) (bool, error) {
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// Paused by the deadline order is fine (it stays paused); paused by a
	// person, or taken, is not.
	res, err := tx.ExecContext(ctx, `UPDATE tgdl_work SET paused=1 WHERE id=? AND status='pending' AND (paused=0 OR id IN (SELECT work_id FROM ix_deferred WHERE released_at=0))
		AND id NOT IN (SELECT work_id FROM ix_held WHERE released_at>0)`, f.ID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	now := time.Now().UnixMilli()
	if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO ix_held(work_id,group_id,message_id,like_path,like_key,reason,held_at,released_at) VALUES(?,?,?,?,?,?,?,0)`,
		f.ID, f.Group, f.Msg, likePath, likeKey, reason, now); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO ix_copy_hold(work_id,how,of_work,sim,confirm,confirm_note,checked_at) VALUES(?,?,?,?,'','',0)`, f.ID, how, ofWork, sim); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ixUnhold lets a held item go and forgets it was looked at, so the checks
// see it again as new.
func (a *App) ixUnhold(ctx context.Context, workID int64) error {
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE tgdl_work SET paused=0 WHERE id=? AND status='pending' AND paused=1
		AND id NOT IN (SELECT work_id FROM ix_deferred WHERE released_at=0)`, workID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM ix_held WHERE work_id=?`, workID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM ix_copy_hold WHERE work_id=?`, workID); err != nil {
		return err
	}
	return tx.Commit()
}

// ixHeldRecheck looks at the held items again: a lesser copy whose better
// copy has now been saved is checked against that file; when the better
// copy failed for good (or was cancelled), the best of the copies waiting
// for it goes instead; a copy of a saved file no longer saved goes.
func (a *App) ixHeldRecheck(ctx context.Context) error {
	now := time.Now().UnixMilli()
	// Resumed by a person (or "resume all" on the dashboard): theirs now.
	if _, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_held SET released_at=? WHERE released_at=0 AND reason<>''
		AND EXISTS (SELECT 1 FROM tgdl_work w WHERE w.id=ix_held.work_id AND w.status='pending' AND w.paused=0)`, now); err != nil {
		return err
	}
	type held struct {
		id, of, msg int64
		group, how  string
		likePath    string
		leader      string // the better copy's queue state, or gone
		gaveUp      bool
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT h.work_id, h.group_id, h.message_id, COALESCE(c.how,'saved'), COALESCE(c.of_work,0), h.like_path,
			COALESCE(l.status,'gone'), COALESCE(l.status='failed' AND (length(l.body)=0 OR COALESCE(r.rounds,0)>=?),0)
		FROM ix_held h JOIN tgdl_work w ON w.id=h.work_id AND w.status='pending'
		LEFT JOIN ix_copy_hold c ON c.work_id=h.work_id
		LEFT JOIN tgdl_work l ON l.id=c.of_work
		LEFT JOIN ix_retry r ON r.work_key=l.group_id||'_'||l.message_id
		WHERE h.released_at=0 AND h.reason<>''`, ixRetryRounds)
	if err != nil {
		return err
	}
	var hs []held
	for rows.Next() {
		var h held
		if rows.Scan(&h.id, &h.group, &h.msg, &h.how, &h.of, &h.likePath, &h.leader, &h.gaveUp) == nil {
			hs = append(hs, h)
		}
	}
	rows.Close()
	autoRetry := a.ixLoadSettings(ctx).AutoRetry
	orphans := map[int64][]int64{} // a better copy that will not come -> the copies waiting for it
	for _, h := range hs {
		switch {
		case h.how != "queued":
			if h.likePath == "" {
				continue
			}
			var n int
			_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM downloads WHERE file_path=? AND (status IS NULL OR status='completed')`, h.likePath).Scan(&n)
			if n == 0 {
				a.ixLog("held copy %s #%d goes: the saved file it copies is no longer in the archive", h.group, h.msg)
				if err := a.ixLetGo(ctx, h.id); err != nil {
					return err
				}
			}
		case h.leader == "completed":
			if err := a.ixRecheckAgainstSaved(ctx, h.id, h.of); err != nil {
				return err
			}
		case h.leader == "gone" || h.leader == "skipped" || h.leader == "failed" && (h.gaveUp || !autoRetry):
			orphans[h.of] = append(orphans[h.of], h.id)
		}
	}
	for _, of := range ixSortedInts(orphans) {
		if err := a.ixPromote(ctx, orphans[of]); err != nil {
			return err
		}
	}
	return nil
}

func ixSortedInts[V any](m map[int64]V) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ixRecheckAgainstSaved: the better copy of a held item was saved; the
// held item now waits as a copy of that file, if it still looks like one.
func (a *App) ixRecheckAgainstSaved(ctx context.Context, workID, leaderID int64) error {
	var group string
	var msg int64
	var body []byte
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT group_id, message_id, body FROM tgdl_work WHERE id=?`, workID).Scan(&group, &msg, &body); err != nil {
		return nil
	}
	var lg string
	var lm int64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT group_id, message_id FROM tgdl_work WHERE id=?`, leaderID).Scan(&lg, &lm)
	var path, hash string
	var w, h, size, dur int64
	var thumb []byte
	err := a.db.Reader.QueryRowContext(ctx, `SELECT file_path, COALESCE(file_hash,''), COALESCE(media_width,0), COALESCE(media_height,0), COALESCE(file_size,0), COALESCE(media_duration_ms,0), telegram_thumb
		FROM downloads WHERE group_id=? AND message_id=? AND status='completed' AND file_path IS NOT NULL AND file_path<>'' ORDER BY id DESC LIMIT 1`, lg, lm).Scan(&path, &hash, &w, &h, &size, &dur, &thumb)
	keep := false
	if err == nil {
		saved := ixCopyFacts{W: int(w), H: int(h), Size: size, Dur: dur, Type: "video"}
		saved.PH, saved.HasPH = ixPreviewHash(thumb)
		if m, ok := ixDecodeMessage(body); ok {
			if f, ok := ixCopyFactsOf(m); ok {
				switch {
				case ixCoversSaved(saved, f) && ixCheapSame(saved, f):
					keep = true
				case saved.pixels() >= f.pixels() && ixSameShape(saved, f) && ixDurWithin(saved.Dur, f.Dur, 0.02) && a.ixHeldBySimilarity(ctx, workID):
					keep = true // the previews were alike by recognition
				}
			}
		}
	}
	if !keep {
		a.ixLog("held copy %s #%d goes: the file it waited for was saved and does not look the same", group, msg)
		return a.ixLetGo(ctx, workID)
	}
	key := ixFileKey(hash, ixNormPath(path))
	if _, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_held SET like_path=?, like_key=?, reason='looks like a saved copy' WHERE work_id=?`, path, key, workID); err != nil {
		return err
	}
	_, err = a.db.Writer.ExecContext(ctx, `UPDATE ix_copy_hold SET how='saved', of_work=0 WHERE work_id=?`, workID)
	return err
}

// ixDurWithin: two lengths (ms) differ by at most share of the longer
// (and never less than half a second).
func ixDurWithin(a, b int64, share float64) bool {
	if a <= 0 || b <= 0 {
		return false
	}
	return math.Abs(float64(a-b)) <= math.Max(500, share*float64(max(a, b)))
}

// ixHeldBySimilarity: held because recognition found the previews alike.
func (a *App) ixHeldBySimilarity(ctx context.Context, workID int64) bool {
	var sim float64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT sim FROM ix_copy_hold WHERE work_id=?`, workID).Scan(&sim)
	return sim > 0
}

// ixPromote: the better copy these waited for will not come; the best of
// them goes now and the others wait for it instead.
func (a *App) ixPromote(ctx context.Context, ids []int64) error {
	var fs []ixCopyFacts
	for _, id := range ids {
		var body []byte
		var size int64
		if a.db.Reader.QueryRowContext(ctx, `SELECT body, file_size FROM tgdl_work WHERE id=?`, id).Scan(&body, &size) != nil {
			continue
		}
		f := ixCopyFacts{Size: size}
		if m, ok := ixDecodeMessage(body); ok {
			if g, ok := ixCopyFactsOf(m); ok {
				f = g
			}
		}
		f.ID = id
		fs = append(fs, f)
	}
	if len(fs) == 0 {
		return nil
	}
	sort.Slice(fs, func(i, j int) bool { return ixBetterCopy(fs[i], fs[j]) })
	a.ixLog("the better copy of %d held file(s) will not download: the best of them goes now", len(fs))
	if err := a.ixUnhold(ctx, fs[0].ID); err != nil {
		return err
	}
	for _, f := range fs[1:] {
		if _, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_copy_hold SET of_work=? WHERE work_id=?`, fs[0].ID, f.ID); err != nil {
			return err
		}
	}
	return nil
}

// ixLetGo releases one held item for good.
func (a *App) ixLetGo(ctx context.Context, workID int64) error {
	if _, err := a.db.Writer.ExecContext(ctx, `UPDATE tgdl_work SET paused=0 WHERE id=? AND status='pending' AND paused=1
		AND id NOT IN (SELECT work_id FROM ix_deferred WHERE released_at=0)`, workID); err != nil {
		return err
	}
	_, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_held SET released_at=? WHERE work_id=?`, time.Now().UnixMilli(), workID)
	return err
}

// ixOnlyConfirmedLeft: nothing is left to do but confirmed copies: no
// other file waiting (held, set aside or retrying) and every rescan done.
func (a *App) ixOnlyConfirmedLeft(ctx context.Context) bool {
	var other int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM tgdl_work w WHERE w.status IN ('pending','processing')
		AND w.id NOT IN (SELECT h.work_id FROM ix_held h JOIN ix_copy_hold c ON c.work_id=h.work_id WHERE h.released_at=0 AND h.reason<>'' AND c.confirm='confirmed')`).Scan(&other)
	if other > 0 {
		return false
	}
	q, err := a.ixLoadQueue(ctx)
	if err != nil {
		return false
	}
	for _, it := range q.Items {
		if it.State == "waiting" || it.State == "running" {
			return false
		}
	}
	return true
}

// ixReleaseHeld unpauses up to n held items (all when n <= 0), confirmed
// copies included: a person asked.
func (a *App) ixReleaseHeld(ctx context.Context, n int) error {
	return a.ixReleaseSome(ctx, n, true)
}

// ixReleaseSome lets up to n held items go (all when n <= 0): copies not
// confirmed first, oldest first. When only some go, lesser copies whose
// better copy is still waiting or downloading stay (they are checked
// against it once it is saved), and confirmed copies go only with
// confirmed.
func (a *App) ixReleaseSome(ctx context.Context, n int, confirmed bool) error {
	limit, cond := "", ""
	if n > 0 {
		limit = " LIMIT " + strconv.Itoa(n)
		cond = ` AND NOT (COALESCE(c.how,'')='queued' AND EXISTS (SELECT 1 FROM tgdl_work l WHERE l.id=c.of_work AND l.status IN ('pending','processing')))`
	}
	if !confirmed {
		cond += ` AND COALESCE(c.confirm,'')<>'confirmed'`
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT h.work_id FROM ix_held h LEFT JOIN ix_copy_hold c ON c.work_id=h.work_id
		WHERE h.released_at=0 AND h.reason<>''`+cond+` ORDER BY COALESCE(c.confirm,'')='confirmed', h.held_at`+limit)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if err := a.ixLetGo(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// ixHeldWaiting is how many held copies wait in the queue for backfills.
// They do not count towards a backfill's queue limit: otherwise the copies
// fill the queue, no unique file can come in behind them, and the queue
// empties of everything else, which lets the copies go.
func (a *App) ixHeldWaiting(ctx context.Context) int {
	var n int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_held h JOIN tgdl_work w ON w.id=h.work_id
		WHERE h.released_at=0 AND h.reason<>'' AND w.status='pending' AND w.paused=1 AND w.origin<>'live'`).Scan(&n)
	return n
}

// ixFetchSubstituted makes the substituted files download for real: their
// identities are never substituted again, their catalogue entries are
// removed, and the next files pass of their groups downloads them.
func (a *App) ixFetchSubstituted(ctx context.Context) (int, []string, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT s.group_id, s.message_id, s.identity, s.of_path FROM ix_subst s
		JOIN downloads d ON d.group_id=s.group_id AND d.message_id=s.message_id AND d.file_path=s.of_path`)
	if err != nil {
		return 0, nil, err
	}
	type sub struct {
		group, identity, path string
		msg                   int64
	}
	var subs []sub
	for rows.Next() {
		var x sub
		if rows.Scan(&x.group, &x.msg, &x.identity, &x.path) == nil {
			subs = append(subs, x)
		}
	}
	rows.Close()
	groups := map[string]bool{}
	for _, x := range subs {
		tx, err := a.db.Writer.BeginTx(ctx, nil)
		if err != nil {
			return 0, nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO ix_subst_block(identity) VALUES(?)`, x.identity)
		if err == nil {
			// Only the catalogue row goes: the saved file it pointed at stays.
			_, err = tx.ExecContext(ctx, `DELETE FROM downloads WHERE group_id=? AND message_id=? AND file_path=?`, x.group, x.msg, x.path)
		}
		if err == nil {
			// So the next files pass queues it again.
			_, err = tx.ExecContext(ctx, `DELETE FROM tgdl_work WHERE group_id=? AND message_id=? AND status IN ('completed','skipped','failed')`, x.group, x.msg)
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM ix_subst WHERE group_id=? AND message_id=?`, x.group, x.msg)
		}
		if err != nil {
			tx.Rollback()
			return 0, nil, err
		}
		if err = tx.Commit(); err != nil {
			return 0, nil, err
		}
		groups[x.group] = true
	}
	return len(subs), ixSortedKeys(groups), nil
}
