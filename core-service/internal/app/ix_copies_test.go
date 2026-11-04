package app

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"strconv"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// ---------- helpers: invented Telegram media, no Telegram involved ----------

// ixTestStripped makes a Telegram "stripped" preview of a small picture:
// the scan of a JPEG encoded with the standard tables, after its header.
func ixTestStripped(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 20}); err != nil {
		t.Fatal(err)
	}
	jp := buf.Bytes()
	sos := bytes.Index(jp, []byte{0xff, 0xda})
	if sos < 0 {
		t.Fatal("no scan in the JPEG")
	}
	seg := int(jp[sos+2])<<8 | int(jp[sos+3])
	b := img.Bounds()
	out := append([]byte{1, byte(b.Dy()), byte(b.Dx())}, jp[sos+2+seg:len(jp)-2]...)
	if _, ok := ixPreviewHash(out); !ok {
		t.Fatal("the invented preview does not decode")
	}
	return out
}

const ixTestChannel = 4242

var ixTestGroup = strconv.FormatInt(-1000000000000-ixTestChannel, 10)

// ixTestVideo is an invented video message.
func ixTestVideo(id int, docID, size int64, durSec float64, w, h int, stripped []byte, date int) *tg.Message {
	doc := &tg.Document{ID: docID, AccessHash: docID * 3, FileReference: []byte{9}, DCID: 2, Size: size, MimeType: "video/mp4",
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{W: w, H: h, Duration: durSec, SupportsStreaming: true}, &tg.DocumentAttributeFilename{FileName: "v" + strconv.Itoa(id) + ".mp4"}},
		Thumbs:     []tg.PhotoSizeClass{&tg.PhotoStrippedSize{Type: "i", Bytes: stripped}, &tg.PhotoSize{Type: "m", W: 320, H: 320 * h / w, Size: 9000}}}
	return &tg.Message{ID: id, Date: date, PeerID: &tg.PeerChannel{ChannelID: ixTestChannel}, Media: &tg.MessageMediaDocument{Document: doc}}
}

func ixTestTL(t *testing.T, m *tg.Message) []byte {
	t.Helper()
	var buf bin.Buffer
	if err := m.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Buf
}

// ixTestQueue puts a message in the download queue; its queue item ID.
func ixTestQueue(t *testing.T, a *App, m *tg.Message) int64 {
	t.Helper()
	att, err := telegram.MessageAttachment(m)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	res, err := a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,paused,created_at,updated_at,origin)
		VALUES('acc',?,'',?,1,?,?,?,?,?,'pending',0,?,?,'history')`, att.GroupID, m.ID, att.Identity.Key(), att.Type, att.Name, att.Identity.Size, ixTestTL(t, m), now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// ixTestArchive records a message as the archive does.
func ixTestArchive(t *testing.T, a *App, m *tg.Message) {
	t.Helper()
	att, _ := telegram.MessageAttachment(m)
	key := att.Identity.Kind + ":" + att.Identity.ID + ":" + strconv.FormatInt(att.Identity.Size, 10)
	if _, err := a.db.Writer.Exec(`INSERT OR REPLACE INTO ix_messages(group_id,message_id,date,media_type,media_name,media_size,media_key,tl) VALUES(?,?,?,?,?,?,?,?)`,
		att.GroupID, m.ID, m.Date, att.Type, att.Name, att.Identity.Size, key, ixTestTL(t, m)); err != nil {
		t.Fatal(err)
	}
}

// ixTestSave records a message's file as downloaded, at path.
func ixTestSave(t *testing.T, a *App, m *tg.Message, path, hash string) {
	t.Helper()
	att, _ := telegram.MessageAttachment(m)
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,status,file_hash,telegram_media_kind,telegram_media_id,telegram_media_size,
			telegram_mime,media_width,media_height,media_duration_ms,telegram_thumb) VALUES(?,'',?,?,?,?,?,'completed',?,?,?,?,?,?,?,?,?)`,
		att.GroupID, m.ID, att.Name, att.Identity.Size, att.Type, path, hash, att.Identity.Kind, att.Identity.ID, att.Identity.Size, att.Facts.Mime, att.Facts.Width, att.Facts.Height, att.Facts.DurationMs, att.Facts.Thumb); err != nil {
		t.Fatal(err)
	}
}

func ixTestApp(t *testing.T) (*App, context.Context) {
	t.Helper()
	ixVdupBackground.Store(false)
	t.Cleanup(func() { ixVdupBackground.Store(true) })
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a, ctx
}

func ixTestPaused(t *testing.T, a *App, id int64) bool {
	t.Helper()
	var p int
	if err := a.db.Reader.QueryRow(`SELECT paused FROM tgdl_work WHERE id=?`, id).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p != 0
}

func ixTestHow(a *App, id int64) (how string, of int64) {
	_ = a.db.Reader.QueryRow(`SELECT c.how, c.of_work FROM ix_copy_hold c JOIN ix_held h ON h.work_id=c.work_id WHERE c.work_id=? AND h.released_at=0 AND h.reason<>''`, id).Scan(&how, &of)
	return
}

// ---------- 1. measuring ----------

func TestIxCopiesAreMeasured(t *testing.T) {
	a, ctx := ixTestApp(t)
	sceneA := ixTestStripped(t, ixTestScene(1, 40, 23))
	sceneB := ixTestStripped(t, ixTestScene(2, 40, 23))
	sceneC := ixTestStripped(t, ixTestScene(3, 40, 23))
	saved := ixTestVideo(1, 101, 60<<20, 61, 1280, 720, sceneA, 1700000000)
	ixTestArchive(t, a, saved)
	ixTestSave(t, a, saved, "c/v1.mp4", "aa11")
	// Not downloaded: a forward of the saved file, a re-upload of it, two
	// uploads of another video and a unique one.
	fwd := ixTestVideo(2, 101, 60<<20, 61, 1280, 720, sceneA, 1700000100)
	reup := ixTestVideo(3, 102, 58<<20, 61.2, 1280, 720, sceneA, 1700000200)
	b1 := ixTestVideo(4, 103, 40<<20, 30, 1920, 1080, sceneB, 1700000300)
	b2 := ixTestVideo(5, 104, 20<<20, 30.3, 1280, 720, sceneB, 1700000400)
	uniq := ixTestVideo(6, 105, 33<<20, 90, 1280, 720, sceneC, 1700000500)
	for _, m := range []*tg.Message{fwd, reup, b1, b2, uniq} {
		ixTestArchive(t, a, m)
	}
	ixTestQueue(t, a, fwd)
	ixTestQueue(t, a, uniq)
	m, err := a.ixRebuildModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	k, err := a.ixCopiesSurvey(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if k.Missing.Files != 5 || k.Same.Files != 1 || k.Same.Bytes != 60<<20 {
		t.Fatalf("missing/same: %+v", k)
	}
	// The re-upload copies the saved video; b2 is the lesser of b1 and b2.
	if k.Likely.Files != 2 || k.Likely.Bytes != 58<<20+20<<20 {
		t.Fatalf("likely: %+v", k.Likely)
	}
	box := a.ixCopies()
	box.mu.Lock()
	box.known = k
	box.mu.Unlock()
	info := a.ixCopiesMeasure(ctx)
	if info.Queue.All.Files != 2 || info.Queue.Same.Files != 1 || info.Queue.Same.Bytes != 60<<20 || info.Queue.Held.Files != 0 {
		t.Fatalf("queue: %+v", info.Queue)
	}
	if got := a.ixCopiesNow(); got.Known.Likely.Files != 2 {
		t.Fatalf("kept: %+v", got.Known)
	}
	lines := a.ixCopiesDiag(ctx)
	if len(lines) != 4 || !bytes.Contains([]byte(lines[1]), []byte("probable copies by the video checks 2 / 78 MB")) ||
		!bytes.Contains([]byte(lines[3]), []byte("previews of files not downloaded (auto, off): 4 photos/videos · 320 px preview kept 0 · to fetch 4")) {
		t.Fatalf("diagnostics: %q", lines)
	}
}

func TestIxCopySettings(t *testing.T) {
	d := ixDefaultSettings()
	if d.ConfirmedGoEarly || d.CopyThumbsOff {
		t.Fatal("the recommended way is not the default")
	}
	s, err := ixParseSettings(map[string]any{"skipConfirmed": false, "copyThumbs": false}, d)
	if err != nil || !s.ConfirmedGoEarly || !s.CopyThumbsOff {
		t.Fatalf("%+v %v", s, err)
	}
	s, _ = ixParseSettings(map[string]any{"skipConfirmed": true, "copyThumbs": true}, s)
	if s.ConfirmedGoEarly || s.CopyThumbsOff {
		t.Fatal("turned back on")
	}
}
