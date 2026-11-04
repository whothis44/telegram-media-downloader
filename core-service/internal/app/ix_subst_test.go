package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/download"
	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// Invented data only: random bytes, made-up chat and message numbers.

func ixFakeHashes(data []byte, offset int64) []tg.FileHash {
	const part = 131072
	var out []tg.FileHash
	for o := offset / part * part; o < int64(len(data)) && len(out) < 8; o += part {
		sum := sha256.Sum256(data[o:min(o+part, int64(len(data)))])
		out = append(out, tg.FileHash{Offset: o, Limit: part, Hash: sum[:]})
	}
	return out
}

// ixRangeRemote is a file on Telegram for ixSubstitute: hashes (unless
// noHashes), 1 MiB parts and small ranges, each counted.
type ixRangeRemote struct {
	data                 []byte
	noHashes             bool
	hashes, parts, reads atomic.Int64
	readBytes            atomic.Int64
}

func (r *ixRangeRemote) Download(context.Context, telegram.MediaIdentity, io.Writer) error {
	return errors.New("not downloaded in this test")
}

func (r *ixRangeRemote) FileHashes(_ context.Context, offset int64) ([]tg.FileHash, error) {
	r.hashes.Add(1)
	if r.noHashes {
		return nil, errors.New("FILE_HASHES_UNAVAILABLE")
	}
	return ixFakeHashes(r.data, offset), nil
}

func (r *ixRangeRemote) ReadPart(_ context.Context, offset int64) ([]byte, error) {
	r.parts.Add(1)
	out := r.data[offset:min(offset+telegram.PartSize, int64(len(r.data)))]
	r.readBytes.Add(int64(len(out)))
	return append([]byte(nil), out...), nil
}

func (r *ixRangeRemote) ReadRange(_ context.Context, offset int64, limit int) ([]byte, error) {
	if !telegram.ValidRange(offset, limit) {
		return nil, errors.New("LIMIT_INVALID")
	}
	r.reads.Add(1)
	out := r.data[offset:min(offset+int64(limit), int64(len(r.data)))]
	r.readBytes.Add(int64(len(out)))
	return append([]byte(nil), out...), nil
}

func ixRandom(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// ixSaveFixture puts a saved file in the library and the catalogue.
func ixSaveFixture(t *testing.T, a *App, msg int64, rel, typ string, data []byte, w, h int, thumb []byte) {
	t.Helper()
	full := filepath.Join(a.downloadsDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var thumbArg any
	if len(thumb) > 0 {
		thumbArg = thumb
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_size,file_type,file_path,telegram_media_kind,telegram_media_id,telegram_media_size,media_width,media_height,telegram_thumb,status)
		VALUES('-1009900',?,?,?,?,?,?,?,?,NULLIF(?,0),NULLIF(?,0),?,'completed')`, msg, filepath.Base(rel), len(data), typ, rel, map[bool]string{true: "photo", false: "document"}[typ == "photo"], 900+msg, len(data), w, h, thumbArg); err != nil {
		t.Fatal(err)
	}
}

func ixDedupCounter(t *testing.T, a *App, how string) (files, bytes int64) {
	t.Helper()
	_ = a.db.Reader.QueryRow(`SELECT COALESCE(SUM(files),0), COALESCE(SUM(bytes),0) FROM ix_dedup_daily WHERE how=?`, how).Scan(&files, &bytes)
	return
}

func TestIxSubstituteChecksBySizeAndKind(t *testing.T) {
	a, ctx := ixTestApp(t)
	thumb := []byte{1, 40, 30, 0x5a, 0xa5, 0x11}
	small := ixRandom(1, 400<<10)      // a short clip, below 2 MiB
	doc := ixRandom(2, 600<<10)        // an archive
	photo := ixRandom(3, 1200<<10)     // a large compressed photo
	smallPhoto := ixRandom(4, 600<<10) // a photo below 1 MiB
	ixSaveFixture(t, a, 1, "Invented Chat/videos/clip_1.mp4", "video", small, 640, 360, nil)
	ixSaveFixture(t, a, 2, "Invented Chat/documents/pack_2.zip", "document", doc, 0, 0, nil)
	ixSaveFixture(t, a, 3, "Invented Chat/images/photo_3.jpg", "photo", photo, 2560, 1920, thumb)
	ixSaveFixture(t, a, 4, "Invented Chat/images/photo_4.jpg", "photo", smallPhoto, 1280, 960, thumb)

	item := func(msg int64, typ, mime string, size int, w, h int, th []byte) download.Item {
		kind := "document"
		if typ == "photo" && mime == "image/jpeg" {
			kind = "photo"
		}
		return download.Item{GroupID: "-1009901", MessageID: msg, Name: "x", Type: typ, Identity: telegram.MediaIdentity{Kind: kind, ID: "77" + string(rune('0'+msg%10)), Size: int64(size)}, Facts: telegram.MediaFacts{Mime: mime, Width: w, Height: h, Thumb: th}}
	}
	run := func(it download.Item, r *ixRangeRemote) (string, string, bool) {
		return a.ixSubstitute(ctx, it, r)
	}

	// A short clip, same bytes: one small hash request, nothing downloaded.
	r := &ixRangeRemote{data: small}
	if path, how, ok := run(item(11, "video", "video/mp4", len(small), 640, 360, nil), r); !ok || how != "telegram-hashes" || path != "Invented Chat/videos/clip_1.mp4" || r.parts.Load()+r.reads.Load() != 0 {
		t.Fatalf("short clip: %q %q %v parts=%d", path, how, ok, r.parts.Load())
	}
	// Without Telegram's hashes a short clip is downloaded: sampling it would cost as much.
	r = &ixRangeRemote{data: small, noHashes: true}
	if _, _, ok := run(item(12, "video", "video/mp4", len(small), 640, 360, nil), r); ok || r.parts.Load()+r.reads.Load() != 0 {
		t.Fatalf("short clip without hashes: %v parts=%d ranges=%d", ok, r.parts.Load(), r.reads.Load())
	}
	// Below 256 KiB nothing is asked.
	tiny := ixRandom(5, 200<<10)
	ixSaveFixture(t, a, 5, "Invented Chat/videos/tiny_5.mp4", "video", tiny, 0, 0, nil)
	r = &ixRangeRemote{data: tiny}
	if _, _, ok := run(item(13, "video", "video/mp4", len(tiny), 0, 0, nil), r); ok || r.hashes.Load() != 0 {
		t.Fatalf("tiny: %v hashes=%d", ok, r.hashes.Load())
	}

	// A document edited in place (same length) where three spot checks
	// would not look: every part's hash is compared, so it is downloaded.
	edited := append([]byte(nil), doc...)
	edited[140000] ^= 0xff
	r = &ixRangeRemote{data: edited}
	if _, _, ok := run(item(14, "document", "application/zip", len(doc), 0, 0, nil), r); ok {
		t.Fatal("edited document taken for the saved one")
	}
	r = &ixRangeRemote{data: doc}
	if _, how, ok := run(item(15, "document", "application/zip", len(doc), 0, 0, nil), r); !ok || how != "telegram-hashes" {
		t.Fatalf("same document: %q %v", how, ok)
	}
	// Without hashes a document is never sampled.
	r = &ixRangeRemote{data: doc, noHashes: true}
	if _, _, ok := run(item(16, "document", "application/zip", len(doc), 0, 0, nil), r); ok || r.parts.Load()+r.reads.Load() != 0 {
		t.Fatalf("document without hashes: %v", ok)
	}

	// A large photo matching a saved one in size, pixel size and preview:
	// without hashes, four 64 KiB ranges (not the whole photo) decide.
	r = &ixRangeRemote{data: photo, noHashes: true}
	if path, how, ok := run(item(17, "photo", "image/jpeg", len(photo), 2560, 1920, thumb), r); !ok || how != "sampled-ranges" || path != "Invented Chat/images/photo_3.jpg" || r.parts.Load() != 0 || r.readBytes.Load() > 256<<10 {
		t.Fatalf("photo: %q %q %v parts=%d read=%d", path, how, ok, r.parts.Load(), r.readBytes.Load())
	}
	// Different bytes in a sampled range: downloaded.
	other := append([]byte(nil), photo...)
	other[len(other)-5] ^= 0xff
	r = &ixRangeRemote{data: other, noHashes: true}
	if _, _, ok := run(item(18, "photo", "image/jpeg", len(photo), 2560, 1920, thumb), r); ok {
		t.Fatal("different photo bytes matched")
	}
	// Same size but another preview or pixel size: not asked at all.
	for _, it := range []download.Item{
		item(19, "photo", "image/jpeg", len(photo), 2560, 1920, []byte{1, 40, 30, 0x00}),
		item(20, "photo", "image/jpeg", len(photo), 1920, 2560, thumb),
		item(21, "photo", "image/jpeg", len(photo), 2560, 1920, nil),
	} {
		r = &ixRangeRemote{data: photo}
		if _, _, ok := run(it, r); ok || r.hashes.Load() != 0 {
			t.Fatalf("photo %d: %v hashes=%d", it.MessageID, ok, r.hashes.Load())
		}
	}
	// A photo below 1 MiB: Telegram's hashes only.
	r = &ixRangeRemote{data: smallPhoto}
	if _, how, ok := run(item(22, "photo", "image/jpeg", len(smallPhoto), 1280, 960, thumb), r); !ok || how != "telegram-hashes" {
		t.Fatalf("small photo: %q %v", how, ok)
	}
	r = &ixRangeRemote{data: smallPhoto, noHashes: true}
	if _, _, ok := run(item(23, "photo", "image/jpeg", len(smallPhoto), 1280, 960, thumb), r); ok || r.reads.Load()+r.parts.Load() != 0 {
		t.Fatalf("small photo without hashes: %v", ok)
	}

	// What the checks did is counted: every item with a same-size saved
	// file was checked (9); samples were downloaded for photos 17 and 18 only.
	checks, sampled := ixDedupCounter(t, a, ixDedupCheck)
	noHash, _ := ixDedupCounter(t, a, ixDedupCheckNoHash)
	if checks != 9 || noHash != 5 || sampled <= 0 || sampled > 2*(256<<10) {
		t.Fatalf("counters: checks=%d noHash=%d sampled=%d", checks, noHash, sampled)
	}
	// Nothing is recorded as substituted until the library has catalogued it.
	var subst int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_subst`).Scan(&subst)
	if subst != 0 {
		t.Fatalf("ix_subst rows before catalogue: %d", subst)
	}
}

// ixPartsAccount is an account whose files Telegram keeps part hashes for.
type ixPartsAccount struct {
	fixtureAccount
	files     map[string][]byte // by document id
	downloads atomic.Int64
}

func (a *ixPartsAccount) MediaFileHashes(_ context.Context, att telegram.Attachment, offset int64) ([]tg.FileHash, error) {
	data, ok := a.files[att.Identity.ID]
	if !ok {
		return nil, errors.New("FILE_ID_INVALID")
	}
	return ixFakeHashes(data, offset), nil
}

func (a *ixPartsAccount) MediaRange(_ context.Context, att telegram.Attachment, offset int64, limit int) ([]byte, error) {
	data := a.files[att.Identity.ID]
	return append([]byte(nil), data[offset:min(offset+int64(limit), int64(len(data)))]...), nil
}

func ixClipUpdate(msg, doc int, size int64) *tg.Updates {
	return &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: msg, Date: 100 + msg, PeerID: &tg.PeerChannel{ChannelID: 42},
		Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: int64(doc), Size: size, DCID: 2, MimeType: "video/mp4", Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "clip.mp4"}}}}}}}}
}

func ixWaitCompleted(t *testing.T, a *App, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var done int
		_ = a.db.Reader.QueryRow(`SELECT count(*) FROM tgdl_work WHERE status='completed'`).Scan(&done)
		if done >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%d downloads did not finish", n)
}

// Through the real download queue: the queue hands the library its
// progress wrapper around the account, and the exact re-upload check must
// still reach Telegram (it never did: 0 re-uploads found in ~6,100
// downloads). Also counts each duplicate by how it was found.
func TestIxQueueSkipsExactReuploadBeforeDownloading(t *testing.T) {
	original := ixRandom(7, 3<<20+777)
	edited := ixRandom(8, len(original)) // same length, other bytes (a re-encode)
	size := int64(len(original))
	made := make(chan *ixPartsAccount, 1)
	factory := func(_ engine.AccountConfig, _ *telegram.UpdateState, handler func(context.Context, tg.UpdatesClass) error, _ func(int64)) (engine.Account, error) {
		acc := &ixPartsAccount{files: map[string][]byte{"5001": original, "5002": original, "5003": edited, "5004": original}}
		acc.handle = handler
		acc.download = func(_ context.Context, att telegram.Attachment, w io.Writer) error {
			acc.downloads.Add(1)
			_, err := w.Write(acc.files[att.Identity.ID])
			return err
		}
		made <- acc
		return acc, nil
	}
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: t.TempDir(), AccountFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureMonitor(t, a)
	// The last clip is set to be downloaded for real (Fetch the real files),
	// so it is found to be a copy only after downloading.
	if _, err = a.db.Writer.Exec(`INSERT INTO ix_subst_block(identity) VALUES(?)`, telegram.MediaIdentity{Kind: "document", ID: "5004", Size: size}.Key()); err != nil {
		t.Fatal(err)
	}
	if rec := monitorRequest(t, a, "/api/monitor/start"); rec.Code != 200 {
		t.Fatalf("start=%d %s", rec.Code, rec.Body.String())
	}
	acc := <-made
	for i, doc := range []int{5001, 5002, 5003, 5004} {
		if err = acc.handle(context.Background(), ixClipUpdate(10+i, doc, size)); err != nil {
			t.Fatal(err)
		}
		ixWaitCompleted(t, a, i+1)
	}
	if n := acc.downloads.Load(); n != 3 {
		t.Fatalf("downloads=%d, want 3 (the exact re-upload is not downloaded)", n)
	}
	var first, second, how string
	_ = a.db.Reader.QueryRow(`SELECT file_path FROM downloads WHERE message_id=10`).Scan(&first)
	_ = a.db.Reader.QueryRow(`SELECT file_path FROM downloads WHERE message_id=11`).Scan(&second)
	if first == "" || second != first {
		t.Fatalf("re-upload catalogued at %q, saved file %q", second, first)
	}
	if err = a.db.Reader.QueryRow(`SELECT how FROM ix_subst WHERE message_id=11`).Scan(&how); err != nil || how != "telegram-hashes" {
		t.Fatalf("ix_subst: %q %v", how, err)
	}
	var substOther int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_subst WHERE message_id<>11`).Scan(&substOther)
	if substOther != 0 {
		t.Fatalf("other messages recorded as re-uploads: %d", substOther)
	}
	// The copy found after downloading points at the saved file; its own download is gone.
	var fourth string
	_ = a.db.Reader.QueryRow(`SELECT file_path FROM downloads WHERE message_id=13`).Scan(&fourth)
	if fourth != first {
		t.Fatalf("after-download copy at %q, saved %q", fourth, first)
	}
	files, _ := filepath.Glob(filepath.Join(a.downloadsDir, "*", "*", "*"))
	if len(files) != 2 {
		t.Fatalf("files on disk: %d, want 2", len(files))
	}
	st := a.ixDedupStatus(context.Background())
	if st.Reupload.Files != 1 || st.Reupload.Bytes != size || st.After.Files != 1 || st.After.Bytes != size || st.TelegramFile.Files != 0 || st.Checks != 2 {
		t.Fatalf("dedup status: %+v", st)
	}
	if h := a.ixHeldStatus(context.Background()); h.Subst != 1 || h.SavedMB != 3 {
		t.Fatalf("held status: %+v", h)
	}
	diag := strings.Join(a.ixDedupDiag(context.Background()), "\n")
	if !strings.Contains(diag, "exact re-upload check 1 (3 MB)") || !strings.Contains(diag, "after download, same bytes 1 (3 MB downloaded") {
		t.Fatalf("diagnostics: %s", diag)
	}
	// A forward of the saved clip: no download, counted as the same Telegram file.
	if err = acc.handle(context.Background(), ixClipUpdate(20, 5001, size)); err != nil {
		t.Fatal(err)
	}
	ixWaitCompleted(t, a, 5)
	if st = a.ixDedupStatus(context.Background()); st.TelegramFile.Files != 1 || acc.downloads.Load() != 3 {
		t.Fatalf("forward: %+v downloads=%d", st, acc.downloads.Load())
	}
}
