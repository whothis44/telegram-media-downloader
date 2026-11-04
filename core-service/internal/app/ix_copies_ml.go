package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// Copies by image recognition. For a queued video (5 MB or more) or a
// large picture (2 MB or more) the archive has no exact match for,
// Telegram's small preview (320 px, a few KB, one request) is fetched and
// looked at by the image recognition service, then compared with the
// saved files' recognition vectors: a picture with saved pictures, a video
// with saved videos (the mean of three frames) of the same length to 2%.
// The same shape and no fewer pixels are required too. Alike enough
// (cosine at least the threshold, 0.95 unless calibrated higher on this
// archive) means a probable copy: held, never skipped.
//
// Previews are fetched one at a time, a few seconds apart, at most a few
// dozen per round, through the account's guarded download pools; a round
// stops at the first sign that Telegram wants the account to slow down.
// The biggest files go first (a preview costs one request; the file may
// cost thousands), then private chats. The previews are kept in the data
// folder (ixpreviews): they show in the browser for files that may never
// be downloaded.

const (
	ixPreviewMinPicture = 2 << 20
	ixPreviewsPerRound  = 30
	ixPreviewTries      = 3
	ixCopySimDefault    = 0.95
)

// ixPreviewGap is the pause between two preview requests.
var ixPreviewGap = 3 * time.Second

// ixCopyReader reads small parts of files through an account.
type ixCopyReader interface {
	FilePart(telegram.Attachment, int64) ([]byte, error)
	RefreshMessage(*tg.Message) (*tg.Message, error)
	Close()
}

// ixOpenCopyReader opens a reader on an account (replaceable in tests).
var ixOpenCopyReader = func(a *App, ctx context.Context, accountID string) (ixCopyReader, error) {
	if a.monitor == nil {
		return nil, errors.New("the Telegram engine is not running")
	}
	return a.monitor.OpenHistory(ctx, accountID)
}

// ixPreviewKnown: Telegram identities (kind:id:size) with a kept preview.
var ixPreviewKnown sync.Map

// ixCopyPreviewURL is the browser's address of a kept preview ("" when none).
func ixCopyPreviewURL(ident string) string {
	if ident == "" {
		return ""
	}
	if _, ok := ixPreviewKnown.Load(ident); !ok {
		return ""
	}
	return "/api/ix/copies/preview?ident=" + url.QueryEscape(ident)
}

func (a *App) ixPreviewDir() string { return filepath.Join(a.dataDir, "ixpreviews") }

func ixPreviewName(ident string) string {
	sum := sha256.Sum256([]byte(ident))
	h := hex.EncodeToString(sum[:16])
	return h[:2] + "/" + h + ".jpg"
}

func (a *App) ixLoadPreviewIdents(ctx context.Context) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT ident FROM ix_copy_thumb WHERE path<>''`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ixPreviewKnown.Store(id, true)
		}
	}
}

func (a *App) handleIxCopyPreview(w http.ResponseWriter, r *http.Request) {
	var rel string
	if a.db.Reader.QueryRowContext(r.Context(), `SELECT path FROM ix_copy_thumb WHERE ident=? AND path<>''`, r.URL.Query().Get("ident")).Scan(&rel) != nil || rel != ixNormPath(rel) || strings.Contains(rel, "..") {
		writeNotFound(w, r)
		return
	}
	data, err := os.ReadFile(filepath.Join(a.ixPreviewDir(), filepath.FromSlash(rel)))
	if err != nil {
		writeNotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

// ---------- the threshold ----------

// ixCopyCalib is a calibration on this archive's own pictures: each saved
// picture shrunk to 320 px (as Telegram's preview) against its own vector
// (same), and against the closest other picture (different).
type ixCopyCalib struct {
	Pictures  int     `json:"pictures"`
	SameMin   float64 `json:"sameMin"`
	SameP10   float64 `json:"sameP10"`
	DiffMax   float64 `json:"diffMax"`
	DiffP99   float64 `json:"diffP99"`
	Threshold float64 `json:"threshold"`
	Model     string  `json:"model"`
	At        int64   `json:"at"`
}

const ixCopyCalibKey = "ix_copy_calib"

func (a *App) ixCopyCalibLast(ctx context.Context) *ixCopyCalib {
	var raw string
	if a.db.Reader.QueryRowContext(ctx, `SELECT value FROM kv WHERE key=?`, ixCopyCalibKey).Scan(&raw) != nil {
		return nil
	}
	var c ixCopyCalib
	if json.Unmarshal([]byte(raw), &c) != nil || c.Pictures == 0 {
		return nil
	}
	return &c
}

// ixCopyThreshold: 0.95, or higher when the calibration of the current
// model says pictures that differ come closer than that.
func (a *App) ixCopyThreshold(ctx context.Context) float64 {
	c := a.ixCopyCalibLast(ctx)
	if c == nil || c.Model != ixMLModelOf(a.ixLoadSettings(ctx)) {
		return ixCopySimDefault
	}
	return c.Threshold
}

// ixCalibThreshold: at least 0.95, and above nearly every closest pair of
// different pictures; never above 0.99.
func ixCalibThreshold(diffP99 float64) float64 {
	return math.Min(0.99, math.Max(ixCopySimDefault, diffP99+0.01))
}

func ixPercentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(math.Round(p * float64(len(s)-1)))
	return s[max(0, min(len(s)-1, i))]
}

// ixShrink scales a picture down so its longer side is at most side, by
// averaging (as a 320 px preview is made).
func ixShrink(img image.Image, side int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= side && h <= side {
		return img
	}
	nw, nh := side, h*side/w
	if h > w {
		nw, nh = w*side/h, side
	}
	nw, nh = max(1, nw), max(1, nh)
	out := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0, y1 := b.Min.Y+y*h/nh, b.Min.Y+max((y+1)*h/nh, y*h/nh+1)
		for x := 0; x < nw; x++ {
			x0, x1 := b.Min.X+x*w/nw, b.Min.X+max((x+1)*w/nw, x*w/nw+1)
			var r, g, bl, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					cr, cg, cb, _ := img.At(xx, yy).RGBA()
					r, g, bl, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), n+1
				}
			}
			i := out.PixOffset(x, y)
			out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = uint8(r/n>>8), uint8(g/n>>8), uint8(bl/n>>8), 255
		}
	}
	return out
}

// ixCopyCalibrate measures the threshold on up to n saved pictures.
func (a *App) ixCopyCalibrate(ctx context.Context, n int) (*ixCopyCalib, error) {
	c := a.ixMLClientNow(ctx)
	if c == nil {
		return nil, errors.New("image recognition is not set up (Settings → Image recognition)")
	}
	m, err := a.ixModelNow(ctx)
	if m == nil {
		return nil, errors.Join(errors.New("the browser index is not ready"), err)
	}
	store := &a.ixMLBox().store
	if err := store.load(ctx, a.db.Reader, c.model); err != nil {
		return nil, err
	}
	var same, diff []float64
	for ci := range m.Clusters {
		if len(same) >= n || ctx.Err() != nil {
			break
		}
		canon := m.Clusters[ci].Canon
		if canon < 0 {
			continue
		}
		it := &m.Items[canon]
		if it.Type != "photo" || it.Key == "" || !store.has(it.Key) {
			continue
		}
		pics, err := a.ixMLPictures(ctx, it)
		if err != nil || len(pics) == 0 {
			continue
		}
		img, _, err := image.Decode(bytes.NewReader(pics[0]))
		if err != nil {
			continue
		}
		var buf bytes.Buffer
		if jpeg.Encode(&buf, ixShrink(img, 320), &jpeg.Options{Quality: 80}) != nil {
			continue
		}
		v, err := c.embed(ctx, buf.Bytes(), "")
		if err != nil || !ixNormalise(v) {
			continue
		}
		own := store.vector(it.Key)
		if own == nil {
			continue
		}
		same = append(same, ixCosine(v, own))
		cluster := it.Cluster
		hits := store.nearest(v, 1, func(k string) bool {
			j, ok := m.ByKey[k]
			return ok && m.Items[j].Type == "photo" && m.Items[j].Cluster != cluster
		}, it.Key)
		if len(hits) > 0 {
			diff = append(diff, float64(hits[0].Sim))
		}
	}
	if len(same) < 5 {
		return nil, errors.New("too few saved pictures have been looked at by image recognition yet")
	}
	r := &ixCopyCalib{Pictures: len(same), SameMin: ixPercentile(same, 0), SameP10: ixPercentile(same, 0.1), DiffMax: ixPercentile(diff, 1), DiffP99: ixPercentile(diff, 0.99), Model: c.model, At: time.Now().UnixMilli()}
	r.Threshold = ixCalibThreshold(r.DiffP99)
	data, _ := json.Marshal(r)
	if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, ixCopyCalibKey, string(data), r.At); err != nil {
		return nil, err
	}
	a.ixCopies().setThreshold(r.Threshold)
	return r, nil
}

func (a *App) handleIxCopyCalibrate(w http.ResponseWriter, r *http.Request) {
	c, err := a.ixCopyCalibrate(r.Context(), 40)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func ixCosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var d, na, nb float64
	for i := range a {
		d += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return d / math.Sqrt(na*nb)
}

// ---------- the threshold the checks use now ----------

func (b *ixCopiesBox) setThreshold(v float64) {
	b.mu.Lock()
	b.info.Threshold = v
	b.mu.Unlock()
}

func (a *App) ixCopyThresholdNow() float64 {
	box := a.ixCopies()
	box.mu.Lock()
	defer box.mu.Unlock()
	if box.info.Threshold == 0 {
		return ixCopySimDefault
	}
	return box.info.Threshold
}

func init() {
	ixSameQueuedHook = func(a *App, x, y ixCopyFacts) bool {
		if ixCheapSame(x, y) {
			return true
		}
		return x.Vec != nil && y.Vec != nil && ixSameShape(x, y) && ixDurWithin(x.Dur, y.Dur, 0.02) && ixCosine(x.Vec, y.Vec) >= a.ixCopyThresholdNow()
	}
	ixAttachVectorsHook = func(a *App, ctx context.Context, qs []*ixQueuedVideo) {
		model := ixMLModelOf(a.ixLoadSettings(ctx))
		for _, q := range qs {
			if q.f.Ident == "" {
				continue
			}
			var blob []byte
			if a.db.Reader.QueryRowContext(ctx, `SELECT vec FROM ix_copy_thumb WHERE ident=? AND model=? AND length(vec)>0`, q.f.Ident, model).Scan(&blob) == nil {
				q.f.Vec = ixUnpackVec(blob)
			}
		}
	}
	ixCopiesWorkHook = func(a *App, ctx context.Context) {
		if err := a.ixPreviewRound(ctx); err != nil && ctx.Err() == nil {
			a.ixLog("copy previews: %v", err)
		}
	}
}

func ixPackVec(v []float32) []byte {
	q, _ := ixQuantise(v)
	out := make([]byte, len(q))
	for i, x := range q {
		out[i] = byte(x)
	}
	return out
}

func ixUnpackVec(b []byte) []float32 {
	out := make([]float32, len(b))
	for i, x := range b {
		out[i] = float32(int8(x)) / 127
	}
	if !ixNormalise(out) {
		return nil
	}
	return out
}

// ---------- the round ----------

type ixPreviewJob struct {
	work    int64
	account string
	body    []byte
	f       ixCopyFacts
}

// ixPreviewCandidates are the queued files worth a preview, biggest first.
func (a *App) ixPreviewCandidates(ctx context.Context, model string, n int) ([]ixPreviewJob, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT w.id, w.account_id, w.body, w.file_size FROM tgdl_work w
		LEFT JOIN ix_group_info g ON g.group_id=w.group_id
		WHERE w.status='pending' AND w.ttl_seconds=0 AND length(w.body)>0
		AND ((w.media_type='video' AND w.file_size>=?) OR (w.media_type='photo' AND w.file_size>=?))
		AND w.id NOT IN (SELECT work_id FROM ix_held WHERE released_at=0 AND reason<>'')
		ORDER BY w.file_size>=524288000 DESC, COALESCE(g.username,'')='' DESC, w.file_size DESC, w.id LIMIT 2000`, ixHoldMinSize, ixPreviewMinPicture)
	if err != nil {
		return nil, err
	}
	var all []ixPreviewJob
	for rows.Next() {
		var j ixPreviewJob
		var size int64
		if rows.Scan(&j.work, &j.account, &j.body, &size) != nil {
			continue
		}
		all = append(all, j)
	}
	rows.Close()
	var out []ixPreviewJob
	for _, j := range all {
		if len(out) >= n {
			break
		}
		m, ok := ixDecodeMessage(j.body)
		if !ok {
			continue
		}
		f, ok := ixCopyFactsOf(m)
		if !ok {
			continue
		}
		f.ID = j.work
		var tries int
		var have bool
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT tries, (path<>'' AND model=? AND length(vec)>0) FROM ix_copy_thumb WHERE ident=?`, model, f.Ident).Scan(&tries, &have)
		if have || tries >= ixPreviewTries {
			continue
		}
		// An exact re-upload is checked by its bytes when it downloads.
		var same int
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM downloads WHERE status='completed' AND (file_size=? OR telegram_media_size=?) LIMIT 1`, f.Size, f.Size).Scan(&same)
		if same > 0 {
			continue
		}
		j.f = f
		out = append(out, j)
	}
	return out, nil
}

// ixPreviewRound fetches, looks at and compares the previews of a few
// queued files.
func (a *App) ixPreviewRound(ctx context.Context) error {
	s := a.ixLoadSettings(ctx)
	c := a.ixMLClientNow(ctx)
	if s.CopyThumbsOff || !s.HoldLikely || c == nil {
		return nil
	}
	a.ixCopies().setThreshold(a.ixCopyThreshold(ctx))
	jobs, err := a.ixPreviewCandidates(ctx, c.model, ixPreviewsPerRound)
	if err != nil || len(jobs) == 0 {
		return err
	}
	store := &a.ixMLBox().store
	if err := store.load(ctx, a.db.Reader, c.model); err != nil {
		return err
	}
	m := a.ixModelLast()
	readers := map[string]ixCopyReader{}
	defer func() {
		for _, r := range readers {
			r.Close()
		}
	}()
	for i, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if i > 0 && ixPreviewGap > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(ixPreviewGap):
			}
		}
		r := readers[j.account]
		if r == nil {
			if r, err = ixOpenCopyReader(a, ctx, j.account); err != nil {
				return err
			}
			readers[j.account] = r
		}
		data, w, h, err := ixFetchPreview(r, j.body)
		if err != nil {
			a.ixPreviewFailed(ctx, j.f, err.Error())
			if ixSlowDown(err) {
				return nil // Telegram wants the account to wait: next round
			}
			continue
		}
		v, err := c.embed(ctx, data, "")
		if err != nil || !ixNormalise(v) {
			if err == nil {
				err = errors.New("an empty recognition vector")
			}
			// Kept as a preview; looked at again next round.
			_ = a.ixPreviewSave(ctx, j.f, data, w, h, "", nil, err.Error())
			var unreachable ixMLUnreachable
			if errors.As(err, &unreachable) {
				return nil
			}
			continue
		}
		if err := a.ixPreviewSave(ctx, j.f, data, w, h, c.model, v, ""); err != nil {
			return err
		}
		if err := a.ixRecognitionCheck(ctx, m, store, j.f, v); err != nil {
			return err
		}
	}
	return nil
}

// ixSlowDown: Telegram asked the account to wait.
func ixSlowDown(err error) bool {
	s := err.Error()
	return strings.Contains(s, "FLOOD") || strings.Contains(s, "flood")
}

// ixFetchPreview reads a message's preview, asking for the message again
// once when its file reference has expired.
func ixFetchPreview(r ixCopyReader, body []byte) ([]byte, int, int, error) {
	msg, ok := ixDecodeMessage(body)
	if !ok {
		return nil, 0, 0, errors.New("the queued message cannot be read")
	}
	for try := 0; ; try++ {
		att, w, h, ok := telegram.ThumbAttachment(msg)
		if !ok {
			return nil, 0, 0, errIxNoPreview
		}
		data, err := r.FilePart(att, 0)
		if err != nil && try == 0 && strings.Contains(err.Error(), "FILE_REFERENCE") {
			fresh, ferr := r.RefreshMessage(msg)
			if ferr != nil {
				return nil, 0, 0, ferr
			}
			msg = fresh
			continue
		}
		if err != nil {
			return nil, 0, 0, err
		}
		if len(data) == 0 || len(data) > telegram.ThumbMaxBytes {
			return nil, 0, 0, errors.New("Telegram sent no usable preview")
		}
		if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
			return nil, 0, 0, errors.New("the preview is not a picture")
		}
		return data, w, h, nil
	}
}

var errIxNoPreview = errors.New("Telegram has no preview of this file")

func (a *App) ixPreviewFailed(ctx context.Context, f ixCopyFacts, why string) {
	tries := 1
	if why == errIxNoPreview.Error() {
		tries = ixPreviewTries
	}
	_, _ = a.db.Writer.ExecContext(ctx, `INSERT INTO ix_copy_thumb(ident,group_id,message_id,error,tries,at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(ident) DO UPDATE SET error=excluded.error, tries=ix_copy_thumb.tries+excluded.tries, at=excluded.at`, f.Ident, f.Group, f.Msg, why, tries, ixNowMs())
}

func (a *App) ixPreviewSave(ctx context.Context, f ixCopyFacts, data []byte, w, h int, model string, v []float32, why string) error {
	rel := ixPreviewName(f.Ident)
	full := filepath.Join(a.ixPreviewDir(), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return err
	}
	var blob []byte
	if v != nil {
		blob = ixPackVec(v)
	}
	tries := 0
	if why != "" {
		tries = 1
	}
	if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_copy_thumb(ident,group_id,message_id,path,w,h,bytes,model,vec,error,tries,at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(ident) DO UPDATE SET path=excluded.path, w=excluded.w, h=excluded.h, bytes=excluded.bytes, model=excluded.model, vec=excluded.vec, error=excluded.error,
		tries=ix_copy_thumb.tries+excluded.tries, at=excluded.at`, f.Ident, f.Group, f.Msg, rel, w, h, len(data), model, blob, why, tries, ixNowMs()); err != nil {
		return err
	}
	ixPreviewKnown.Store(f.Ident, true)
	return nil
}

// ixRecognitionCheck compares a queued file's preview with the saved
// files and holds it when one is alike enough and at least as good.
func (a *App) ixRecognitionCheck(ctx context.Context, m *ixModel, store *ixEmbStore, f ixCopyFacts, v []float32) error {
	if m == nil {
		return nil
	}
	thr := a.ixCopyThresholdNow()
	keep := func(k string) bool {
		j, ok := m.ByKey[k]
		if !ok {
			return false
		}
		it := &m.Items[j]
		saved := ixCopyFacts{W: it.W, H: it.H, Dur: int64(it.Dur * 1000)}
		if it.Type != f.Type || it.Path == "" || !ixSameShape(saved, f) || saved.pixels() < f.pixels() {
			return false
		}
		return f.Type != "video" || ixDurWithin(saved.Dur, f.Dur, 0.02)
	}
	hits := store.nearest(v, 3, keep, "")
	if len(hits) == 0 || float64(hits[0].Sim) < thr {
		return nil
	}
	it := &m.Items[m.ByKey[hits[0].Key]]
	ok, err := a.ixHoldWork(ctx, f, "recognition", it.Path, it.Key, 0, float64(hits[0].Sim), "looks like a saved copy (image recognition)")
	if ok {
		a.ixLog("holding %s #%d: its preview looks like a saved file (likeness %.3f)", f.Group, f.Msg, hits[0].Sim)
	}
	return err
}
