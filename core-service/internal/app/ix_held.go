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
	if f.DurationMs <= 0 || f.Width <= 0 || f.Height <= 0 {
		return "", "", false
	}
	ph, okHash := ixPreviewHash(f.Thumb)
	if !okHash {
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
		if !ixVdupBackground.Load() {
			continue
		}
		if err := a.ixHeldStep(ctx); err != nil && ctx.Err() == nil {
			a.ixLog("held-back copies: %v", err)
		}
	}
}

// ixHeldStep holds new probable copies back, and lets them go once nothing
// else is waiting (or when holding is turned off).
func (a *App) ixHeldStep(ctx context.Context) error {
	s := a.ixLoadSettings(ctx)
	if s.HoldLikely {
		type work struct {
			id, msg, size int64
			group         string
		}
		rows, err := a.db.Reader.QueryContext(ctx, `SELECT w.id, w.group_id, w.message_id, w.file_size FROM tgdl_work w
			WHERE w.status='pending' AND w.paused=0 AND w.media_type='video' AND w.file_size>=? AND w.ttl_seconds=0
			AND NOT EXISTS (SELECT 1 FROM ix_held h WHERE h.work_id=w.id) ORDER BY w.id LIMIT 500`, ixHoldMinSize)
		if err != nil {
			return err
		}
		var ws []work
		for rows.Next() {
			var w work
			if rows.Scan(&w.id, &w.group, &w.msg, &w.size) == nil {
				ws = append(ws, w)
			}
		}
		rows.Close()
		for _, w := range ws {
			facts, ok := a.ixMessageFacts(ctx, w.group, w.msg)
			reason, likePath, likeKey := "", "", ""
			if ok {
				if p, k, found := a.ixLikelyCopy(ctx, facts, w.size); found {
					reason, likePath, likeKey = "looks like a saved copy", p, k
				}
			}
			tx, err := a.db.Writer.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			if reason != "" {
				res, err := tx.ExecContext(ctx, `UPDATE tgdl_work SET paused=1 WHERE id=? AND status='pending' AND paused=0`, w.id)
				if err != nil {
					tx.Rollback()
					return err
				}
				if n, _ := res.RowsAffected(); n == 0 {
					reason = ""
				}
			}
			released := int64(0)
			if reason == "" {
				released = -1 // looked at, not held
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO ix_held(work_id,group_id,message_id,like_path,like_key,reason,held_at,released_at) VALUES(?,?,?,?,?,?,?,?)`,
				w.id, w.group, w.msg, likePath, likeKey, reason, time.Now().UnixMilli(), released); err != nil {
				tx.Rollback()
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
		}
	}
	release := !s.HoldLikely
	if !release && !s.HoldForever {
		// Only files the engine could take now count: not ones waiting to
		// retry later. And when nothing has finished for half an hour while
		// copies are held, they go too rather than wait behind a stuck queue.
		var waiting int
		if err := a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM tgdl_work WHERE status='pending' AND paused=0 AND retry_at<=?`, time.Now().UnixMilli()).Scan(&waiting); err != nil {
			return err
		}
		release = waiting == 0
		if !release {
			var last int64
			_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(MAX(updated_at),0) FROM tgdl_work WHERE status='completed'`).Scan(&last)
			release = last > 0 && time.Since(time.UnixMilli(last)) > 30*time.Minute
		}
	}
	if !release {
		return nil
	}
	return a.ixReleaseHeld(ctx, 25)
}

// ixReleaseHeld unpauses up to n held items (all when n <= 0).
func (a *App) ixReleaseHeld(ctx context.Context, n int) error {
	limit := ""
	if n > 0 {
		limit = " LIMIT " + strconv.Itoa(n)
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT work_id FROM ix_held WHERE released_at=0 AND reason<>'' ORDER BY held_at`+limit)
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
	now := time.Now().UnixMilli()
	for _, id := range ids {
		if _, err := a.db.Writer.ExecContext(ctx, `UPDATE tgdl_work SET paused=0 WHERE id=? AND status='pending' AND paused=1`, id); err != nil {
			return err
		}
		if _, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_held SET released_at=? WHERE work_id=?`, now, id); err != nil {
			return err
		}
	}
	return nil
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
