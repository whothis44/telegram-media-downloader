package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// ixTestEmbed is an invented recognition vector: the picture's colours on
// an 8×8 grid, centred. Alike pictures (one shrunk) come out alike.
func ixTestEmbed(img image.Image) []float32 {
	b := img.Bounds()
	v := make([]float32, 0, 192)
	for gy := 0; gy < 8; gy++ {
		for gx := 0; gx < 8; gx++ {
			var r, g, bl, n float64
			for y := b.Min.Y + gy*b.Dy()/8; y < b.Min.Y+(gy+1)*b.Dy()/8; y++ {
				for x := b.Min.X + gx*b.Dx()/8; x < b.Min.X+(gx+1)*b.Dx()/8; x++ {
					cr, cg, cb, _ := img.At(x, y).RGBA()
					r, g, bl, n = r+float64(cr), g+float64(cg), bl+float64(cb), n+1
				}
			}
			v = append(v, float32(r/n), float32(g/n), float32(bl/n))
		}
	}
	var mean float32
	for _, x := range v {
		mean += x / float32(len(v))
	}
	for i := range v {
		v[i] -= mean
	}
	ixNormalise(v)
	return v
}

// ixSixtyFour is an invented SHA-256 (64 hex digits).
func ixSixtyFour(c string) string { return strings.Repeat(c, 64) }

func ixTestRecognition(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			_, _ = io.WriteString(w, "pong")
			return
		}
		f, _, err := r.FormFile("image")
		if err != nil {
			http.Error(w, "no picture", 422)
			return
		}
		data, _ := io.ReadAll(f)
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			http.Error(w, `{"detail":"cannot identify image file"}`, 422)
			return
		}
		raw, _ := json.Marshal(ixTestEmbed(img))
		out, _ := json.Marshal(map[string]any{"clip": string(raw)})
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ixFakeReader serves invented previews and file parts; nothing reaches
// Telegram.
type ixFakeReader struct {
	mu      sync.Mutex
	thumbs  map[int64][]byte // document ID -> its 320 px preview
	files   map[int64][]byte // document ID -> its bytes
	expired map[int64]bool   // the first request fails with an expired file reference
	asked   int
	parts   map[int64]int // document ID -> parts read
}

func (f *ixFakeReader) FilePart(a telegram.Attachment, offset int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	id, _ := strconv.ParseInt(a.Identity.ID, 10, 64)
	if f.expired[id] {
		delete(f.expired, id)
		return nil, errors.New("rpc error code 400: FILE_REFERENCE_EXPIRED")
	}
	if len(a.Identity.Kind) > 6 && a.Identity.Kind[:6] == "thumb-" {
		if d, ok := f.thumbs[id]; ok {
			return d, nil
		}
		return nil, errors.New("rpc error code 400: LOCATION_INVALID")
	}
	data := f.files[id]
	if offset%telegram.PartSize != 0 || offset >= int64(len(data)) {
		return nil, errors.New("rpc error code 400: OFFSET_INVALID")
	}
	if f.parts == nil {
		f.parts = map[int64]int{}
	}
	f.parts[id]++
	return data[offset:min(offset+telegram.PartSize, int64(len(data)))], nil
}

func (f *ixFakeReader) RefreshMessage(m *tg.Message) (*tg.Message, error) { return m, nil }
func (f *ixFakeReader) Close()                                            {}

func ixTestUseReader(t *testing.T, r *ixFakeReader) {
	saved, gap := ixOpenCopyReader, ixPreviewGap
	ixOpenCopyReader = func(*App, context.Context, string) (ixCopyReader, error) { return r, nil }
	ixPreviewGap = 0
	t.Cleanup(func() { ixOpenCopyReader, ixPreviewGap = saved, gap })
}

func ixTestRecognitionOn(t *testing.T, a *App, ctx context.Context) {
	srv := ixTestRecognition(t)
	s := a.ixLoadSettings(ctx)
	s.MLURL, s.MLModel = srv.URL, "test-model"
	if err := a.ixSaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
}

func TestIxPreviewsFindCopiesByRecognition(t *testing.T) {
	a, ctx := ixTestApp(t)
	ixTestRecognitionOn(t, a, ctx)
	sceneX, sceneY := ixTestScene(41, 1280, 720), ixTestScene(42, 1280, 720)
	// A saved video, looked at by recognition (from its frames).
	saved := ixTestVideo(1, 501, 300<<20, 120, 1280, 720, ixTestStripped(t, ixTestScene(43, 40, 23)), 1700000000)
	ixTestArchive(t, a, saved)
	ixTestSave(t, a, saved, "c/saved.mp4", ixSixtyFour("d"))
	if err := a.ixMLSave(ctx, "test-model", "h:"+ixSixtyFour("d"), ixTestEmbed(sceneX)); err != nil {
		t.Fatal(err)
	}
	// Queued: a re-encode of it whose stripped preview differs (the cheap
	// check misses it), and a different video as long.
	copyMsg := ixTestVideo(2, 502, 250<<20, 121.5, 1280, 720, ixTestStripped(t, ixTestScene(44, 40, 23)), 1700000100)
	other := ixTestVideo(3, 503, 260<<20, 120, 1280, 720, ixTestStripped(t, ixTestScene(45, 40, 23)), 1700000200)
	ixTestArchive(t, a, copyMsg)
	ixTestArchive(t, a, other)
	copyID, otherID := ixTestQueue(t, a, copyMsg), ixTestQueue(t, a, other)
	r := &ixFakeReader{thumbs: map[int64][]byte{502: ixTestJPEG(t, ixShrink(sceneX, 320), 80), 503: ixTestJPEG(t, ixShrink(sceneY, 320), 80)}, expired: map[int64]bool{502: true}}
	ixTestUseReader(t, r)
	if _, err := a.ixRebuildModel(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.ixPreviewRound(ctx); err != nil {
		t.Fatal(err)
	}
	if how, _ := ixTestHow(a, copyID); how != "recognition" || !ixTestPaused(t, a, copyID) {
		t.Fatalf("the re-encode: %q", how)
	}
	var sim float64
	_ = a.db.Reader.QueryRow(`SELECT sim FROM ix_copy_hold WHERE work_id=?`, copyID).Scan(&sim)
	if sim < ixCopySimDefault {
		t.Fatalf("likeness %.3f", sim)
	}
	if ixTestPaused(t, a, otherID) {
		t.Fatal("a different video was held")
	}
	if r.asked != 3 { // one expired reference asked again, two previews
		t.Fatalf("%d requests", r.asked)
	}
	// Each preview is fetched once; it is kept in the data folder.
	if err := a.ixPreviewRound(ctx); err != nil || r.asked != 3 {
		t.Fatalf("asked again: %d %v", r.asked, err)
	}
	ident := "document:503:" + strconv.Itoa(260<<20)
	if ixCopyPreviewURL(ident) == "" {
		t.Fatal("no preview address")
	}
	var rel string
	_ = a.db.Reader.QueryRow(`SELECT path FROM ix_copy_thumb WHERE ident=?`, ident).Scan(&rel)
	if _, err := os.Stat(filepath.Join(a.dataDir, "ixpreviews", filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
	m, err := a.ixRebuildModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ii := m.Posts[m.ByMsg[ixTestGroup+":3"]].Items[0]
	if j := m.jitem(ii); !j.PreviewOnly || j.Thumb != ixCopyPreviewURL(ident) {
		t.Fatalf("browser item: %+v", j)
	}
	// Two queued uploads alike only by their previews wait for the best.
	a2 := ixTestVideo(4, 504, 90<<20, 61, 1920, 1080, ixTestStripped(t, ixTestScene(46, 40, 23)), 1700000300)
	b2 := ixTestVideo(5, 505, 40<<20, 61.6, 1280, 720, ixTestStripped(t, ixTestScene(47, 40, 23)), 1700000400)
	ida, idb := ixTestQueue(t, a, a2), ixTestQueue(t, a, b2)
	sceneZ := ixTestScene(48, 1280, 720)
	r.thumbs[504], r.thumbs[505] = ixTestJPEG(t, ixShrink(sceneZ, 320), 85), ixTestJPEG(t, ixShrink(sceneZ, 320), 70)
	if err := a.ixPreviewRound(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.ixHeldStep(ctx); err != nil {
		t.Fatal(err)
	}
	if how, of := ixTestHow(a, idb); how != "queued" || of != ida || ixTestPaused(t, a, ida) {
		t.Fatalf("queued pair by previews: %q of %d", how, of)
	}
}

func TestIxCopyThresholdIsCalibratedOnTheArchive(t *testing.T) {
	a, ctx := ixTestApp(t)
	ixTestRecognitionOn(t, a, ctx)
	for i := 0; i < 8; i++ {
		img := ixTestScene(int64(60+i), 800, 600)
		rel := "c/p" + strconv.Itoa(i) + ".jpg"
		full := filepath.Join(a.downloadsDir, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, ixTestJPEG(t, img, 90), 0o644); err != nil {
			t.Fatal(err)
		}
		hash := "ee" + strconv.Itoa(i)
		if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_size,file_type,file_path,status,file_hash,media_width,media_height) VALUES(?,?,?,?,'photo',?,'completed',?,1600,1200)`,
			ixTestGroup, 10+i, "p.jpg", 900000, rel, hash); err != nil {
			t.Fatal(err)
		}
		if err := a.ixMLSave(ctx, "test-model", "p:"+rel, ixTestEmbed(img)); err != nil {
			t.Fatal(err)
		}
		_ = hash
	}
	c, err := a.ixCopyCalibrate(ctx, 40)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pictures != 8 || c.SameMin < 0.97 || c.Threshold < ixCopySimDefault || c.Threshold > 0.99 || c.DiffMax >= c.SameMin {
		t.Fatalf("calibration: %+v", c)
	}
	if got := a.ixCopyThreshold(ctx); got != c.Threshold {
		t.Fatalf("threshold in use %.3f", got)
	}
	if ixCalibThreshold(0.5) != ixCopySimDefault || ixCalibThreshold(0.97) != 0.98 || ixCalibThreshold(0.995) != 0.99 {
		t.Fatal("threshold bounds")
	}
}
