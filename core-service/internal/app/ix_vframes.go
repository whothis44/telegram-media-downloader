package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/procutil"
	"github.com/botnick/telegram-media-downloader/core-service/internal/seekbar"
)

// Videos for image recognition: frames about every 5 seconds, each looked
// at on its own. Each distinct frame's embedding is kept (ix_emb_frames),
// so a search by description can find a scene seen in one part of a video
// and say where it is. Their mean stays in ix_emb, as before, for sets,
// look-alikes and "more like this", where the video as a whole matters.

// Frames per video. Each frame is one request to the recognition service
// on the user's graphics card: about 10,000 videos × up to 12 frames is up
// to 120,000 pictures to look at once (3 per video before). Kept frames
// cost dim bytes each in memory when searched (768 with SigLIP2 ViT-B: about
// 90 MB for 120,000 frames).
const (
	ixVideoFrameEvery = 5.0  // seconds between frames
	ixVideoFramesMin  = 3    // for videos longer than ixVideoShort
	ixVideoFramesMax  = 12   // however long the video
	ixVideoShort      = 3.0  // seconds; shorter videos get one frame
	ixVideoFrameSame  = 0.98 // a frame this close to the last one kept is not kept
	ixVideoFramesPer  = 50   // steps of 2 videos per round when doing older videos again
)

// ixMLLookAlikeVideoSim: videos this alike by their mean embedding go to
// the look-alike review.
const ixMLLookAlikeVideoSim = 0.985

const ixSchemaVFrames = `
CREATE TABLE IF NOT EXISTS ix_emb_frames ( -- embeddings of distinct video frames (int8, like ix_emb)
	key   TEXT    NOT NULL,
	model TEXT    NOT NULL,
	t_ms  INTEGER NOT NULL, -- where the frame is in the video
	vec   BLOB    NOT NULL,
	PRIMARY KEY (key, t_ms)
);
`

func ixEnsureVideoFrames(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, ixSchemaVFrames)
	return err
}

// ---------- which frames ----------

// ixMLFrameTimes is where frames are taken for embedding, in seconds.
func ixMLFrameTimes(dur float64) []float64 {
	if dur <= 0 || math.IsNaN(dur) {
		return []float64{0} // length unknown: the first frame is always there
	}
	if dur <= ixVideoShort {
		return []float64{math.Min(1, dur/2)}
	}
	n := int(math.Round(dur / ixVideoFrameEvery))
	n = max(ixVideoFramesMin, min(ixVideoFramesMax, n))
	out := make([]float64, n)
	for i := range out {
		out[i] = (float64(i) + 0.5) * dur / float64(n)
	}
	return out
}

// ixVideoThreeTimes is where faces and text are read: a fifth, half and
// four fifths of a video, or one frame of a short one.
func ixVideoThreeTimes(dur float64) []float64 {
	if dur > ixVideoShort {
		return []float64{dur * 0.2, dur * 0.5, dur * 0.8}
	}
	return ixMLFrameTimes(dur)
}

// ixMLFrame is one frame of a video, as a JPEG.
type ixMLFrame struct {
	AtMs int64
	Pic  []byte
}

// ixGrabFrame reads one frame of a video, at a time in seconds, as a JPEG
// at most width pixels wide: an ffmpeg run of its own, seeking exactly. Only
// used when one run for all of a video's frames fails (ix_vgrab.go).
// Replaceable in tests.
var ixGrabFrame = func(ctx context.Context, full string, at float64, width, quality int) ([]byte, error) {
	bin := seekbar.FFmpegPath()
	if bin == "" {
		return nil, ixErrNoFFmpeg
	}
	fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(fctx, bin, "-hide_banner", "-nostdin", "-loglevel", "error", "-ss", fmt.Sprintf("%.2f", at), "-i", full,
		"-frames:v", "1", "-an", "-sn", "-dn", "-vf", fmt.Sprintf("scale='min(%d,iw)':-2", width), "-f", "image2pipe", "-vcodec", "mjpeg", "-q:v", fmt.Sprint(quality), "pipe:1")
	procutil.HideWindow(cmd)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if buf.Len() == 0 {
		return nil, errors.New("no frame")
	}
	return buf.Bytes(), nil
}

// ixMLVideoFrames is what is sent for a video: its frames, with their
// times.
func (a *App) ixMLVideoFrames(ctx context.Context, it *ixMItem) ([]ixMLFrame, error) {
	var out []ixMLFrame
	err := a.ixMLVideoFramesEach(ctx, it, func(f ixMLFrame) error {
		out = append(out, f)
		return nil
	})
	return out, err
}

func (a *App) ixMLVideoFramesEach(ctx context.Context, it *ixMItem, each func(ixMLFrame) error) error {
	if ixMLPicturesHook != nil { // tests: pictures without times
		pics, err := ixMLPicturesHook(a, ctx, it)
		if err != nil {
			return err
		}
		times := ixMLFrameTimes(it.Dur)
		for i, p := range pics {
			at := int64(i) * 1000
			if len(pics) == len(times) {
				at = int64(math.Round(times[i] * 1000))
			}
			if err := each(ixMLFrame{AtMs: at, Pic: p}); err != nil {
				return err
			}
		}
		return nil
	}
	full, err := a.ixMLVideoPath(it)
	if err != nil {
		return err
	}
	if !ixHaveFFmpeg() {
		return ixErrNoFFmpeg
	}
	// All frames in one ffmpeg run, or from the frames another task read
	// (ix_vgrab.go).
	frames, err := a.ixVideoFramesOf(ctx, it, full, ixEmbedFrames(it.Dur))
	if err != nil {
		return err
	}
	for _, f := range frames {
		if err := each(f); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) ixMLVideoPath(it *ixMItem) (string, error) {
	rel := ixNormPath(it.Path)
	if rel == "" {
		return "", errors.New("bad path")
	}
	full := filepath.Join(a.downloadsDir, filepath.FromSlash(rel))
	if _, err := os.Stat(full); err != nil {
		return "", errors.New("file not found")
	}
	return full, nil
}

// ---------- embedding a video ----------

// ixMLEmbedVideo looks at a video's frames, keeps the distinct ones, and
// saves them and their mean.
func (a *App) ixMLEmbedVideo(ctx context.Context, c *ixMLClient, it *ixMItem) error {
	// Frames are handed on as they are read (all of a video's come out of
	// one ffmpeg run, ix_vgrab.go).
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	frames := make(chan ixMLFrame, 2)
	var readErr error
	go func() {
		defer close(frames)
		readErr = a.ixMLVideoFramesEach(fctx, it, func(f ixMLFrame) error {
			select {
			case frames <- f:
				return nil
			case <-fctx.Done():
				return fctx.Err()
			}
		})
	}()
	fail := func(err error) error {
		cancel()
		for range frames {
		}
		return err
	}
	var sum, last []float32
	var kept []ixMLFrameVec
	for f := range frames {
		v, err := c.embed(ctx, f.Pic, "")
		if err != nil {
			return fail(ixMLFileErrOf(ctx, err))
		}
		if sum == nil {
			sum = make([]float32, len(v))
		}
		if len(v) != len(sum) {
			return fail(ixMLFileErr{"embeddings of different sizes"})
		}
		for i := range v {
			sum[i] += v[i]
		}
		// Static shots give many frames alike: one is enough to find them.
		if last != nil && ixCosF(last, v) > ixVideoFrameSame {
			continue
		}
		last = v
		kept = append(kept, ixMLFrameVec{at: f.AtMs, vec: v})
	}
	if readErr != nil {
		return a.ixMLReadErr(ctx, readErr)
	}
	if !ixNormalise(sum) {
		return ixMLFileErr{"an empty embedding"}
	}
	if err := a.ixSaveVideoFrames(ctx, c.model, it.Key, kept); err != nil {
		return err
	}
	if err := a.ixMLSave(ctx, c.model, it.Key, sum); err != nil {
		return err
	}
	a.ixMLMarkDone("frames", it.Key)
	a.ixMLLookAlikes(ctx, it.Key)
	return nil
}

type ixMLFrameVec struct {
	at  int64
	vec []float32
}

// ixSaveVideoFrames replaces a video's frames.
func (a *App) ixSaveVideoFrames(ctx context.Context, model, key string, frames []ixMLFrameVec) error {
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM ix_emb_frames WHERE key=?`, key); err != nil {
		return err
	}
	vf := ixVideoFrames{key: key}
	for _, f := range frames {
		q, n := ixQuantise(f.vec)
		blob := make([]byte, len(q))
		for i, x := range q {
			blob[i] = byte(x)
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO ix_emb_frames(key,model,t_ms,vec) VALUES(?,?,?,?)`, key, model, f.at, blob); err != nil {
			return err
		}
		vf.at = append(vf.at, int32(min(f.at, math.MaxInt32)))
		vf.vecs = append(vf.vecs, q...)
		vf.norms = append(vf.norms, n)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fs := a.ixFrameStore()
	fs.mu.Lock()
	if fs.loaded && fs.model == model {
		fs.putLocked(vf)
	}
	fs.mu.Unlock()
	return nil
}

// ixVideoFramesTask does older videos again, frame by frame: those whose
// mean was made from three frames, before frames were kept. It runs only
// when the other recognition work has nothing left in a round, so new
// pictures are never kept waiting.
func (a *App) ixVideoFramesTask(ctx context.Context, c *ixMLClient) *ixMLTask {
	a.ixMLDoneSet(ctx, "frames", c.model, `SELECT DISTINCT key FROM ix_emb_frames WHERE model=?`)
	box := a.ixMLBox()
	return &ixMLTask{
		name: "frames", model: c.model, errTable: "ix_ml_err", par: 2, per: ixVideoFramesPer, idle: true,
		wanted: func(it *ixMItem) bool { return it.Type == "video" && box.store.has(it.Key) },
		done:   a.ixMLIsDone("frames"),
		run:    func(ctx context.Context, it *ixMItem) error { return a.ixMLEmbedVideo(ctx, c, it) },
		test: func(ctx context.Context) error {
			_, err := c.embed(ctx, ixMLTestPicture(), "")
			return err
		},
		note: a.ixMLTaskNoter("frames"),
	}
}

// ixVideoFrameTimes lists where a video's distinct frames are, in
// milliseconds (for the viewer to jump to); empty for other files.
func (a *App) ixVideoFrameTimes(ctx context.Context, key string) []int64 {
	out := []int64{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT t_ms FROM ix_emb_frames WHERE key=? AND model=? ORDER BY t_ms`, key, ixMLModelOf(a.ixLoadSettings(ctx)))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t int64
		if rows.Scan(&t) == nil {
			out = append(out, t)
		}
	}
	return out
}

// ---------- searching frames ----------

// ixVideoFrames is one video's distinct frames, in memory.
type ixVideoFrames struct {
	key   string
	at    []int32 // ms
	vecs  []int8  // len(at) × dim
	norms []float32
}

// ixFrameStore holds the frames of every video, read from the database the
// first time a search needs them.
type ixFrameStore struct {
	mu     sync.RWMutex
	model  string
	dim    int
	loaded bool
	videos []ixVideoFrames
	index  map[string]int32
	frames int
}

var ixFrameState sync.Map // *App -> *ixFrameStore

func (a *App) ixFrameStore() *ixFrameStore {
	v, _ := ixFrameState.LoadOrStore(a, &ixFrameStore{index: map[string]int32{}})
	return v.(*ixFrameStore)
}

func (s *ixFrameStore) putLocked(vf ixVideoFrames) {
	if len(vf.at) == 0 {
		return
	}
	dim := len(vf.vecs) / len(vf.at)
	if s.dim == 0 {
		s.dim = dim
	}
	if dim != s.dim {
		return
	}
	if i, ok := s.index[vf.key]; ok {
		s.frames += len(vf.at) - len(s.videos[i].at)
		s.videos[i] = vf
		return
	}
	s.index[vf.key] = int32(len(s.videos))
	s.videos = append(s.videos, vf)
	s.frames += len(vf.at)
}

// load reads the frames of the current model from the database.
func (s *ixFrameStore) load(ctx context.Context, db *sql.DB, model string) error {
	s.mu.RLock()
	ok := s.loaded && s.model == model
	s.mu.RUnlock()
	if ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded && s.model == model {
		return nil
	}
	s.model, s.dim, s.videos, s.frames, s.loaded = model, 0, nil, 0, false
	s.index = map[string]int32{}
	rows, err := db.QueryContext(ctx, `SELECT key, t_ms, vec FROM ix_emb_frames WHERE model=? ORDER BY key, t_ms`, model)
	if err != nil {
		return err
	}
	defer rows.Close()
	var cur ixVideoFrames
	flush := func() {
		if cur.key != "" {
			s.putLocked(cur)
		}
	}
	for rows.Next() {
		var key string
		var at int64
		var blob []byte
		if rows.Scan(&key, &at, &blob) != nil || len(blob) == 0 {
			continue
		}
		if key != cur.key {
			flush()
			cur = ixVideoFrames{key: key}
		}
		if len(cur.at) > 0 && len(blob) != len(cur.vecs)/len(cur.at) {
			continue
		}
		var n float64
		for _, b := range blob {
			x := int8(b)
			cur.vecs = append(cur.vecs, x)
			n += float64(x) * float64(x)
		}
		cur.at = append(cur.at, int32(min(at, math.MaxInt32)))
		cur.norms = append(cur.norms, float32(math.Sqrt(n)))
	}
	flush()
	if err := rows.Err(); err != nil {
		return err
	}
	s.loaded = true
	return nil
}

// ixFrameHit is a video found by one of its frames.
type ixFrameHit struct {
	Key  string
	Sim  float32 // the cosine of its best frame
	AtMs int64   // where that frame is
}

// best scores every video by its closest frame to q and lists the k best.
func (s *ixFrameStore) best(q []float32, k int, keep func(string) bool) []ixFrameHit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dim == 0 || len(q) != s.dim || k <= 0 {
		return nil
	}
	qq, qn := ixQuantise(q)
	if qn == 0 {
		return nil
	}
	d := s.dim
	var hits []ixFrameHit
	worst := float32(-2)
	for vi := range s.videos {
		v := &s.videos[vi]
		bi, bc := -1, float32(-2)
		for fi := range v.at {
			if v.norms[fi] == 0 {
				continue
			}
			c := float32(ixDot8x4(qq, v.vecs[fi*d:(fi+1)*d])) / (qn * v.norms[fi])
			if c > bc {
				bi, bc = fi, c
			}
		}
		if bi < 0 || (len(hits) >= k && bc <= worst) {
			continue
		}
		if keep != nil && !keep(v.key) {
			continue
		}
		hits = append(hits, ixFrameHit{v.key, bc, int64(v.at[bi])})
		if len(hits) > 2*k {
			sort.Slice(hits, func(x, y int) bool { return hits[x].Sim > hits[y].Sim })
			hits = hits[:k]
			worst = hits[k-1].Sim
		}
	}
	sort.Slice(hits, func(x, y int) bool { return hits[x].Sim > hits[y].Sim })
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// ixDot8x4 is ixDot8 four at a time (about twice as fast).
func ixDot8x4(a, b []int8) int32 {
	var s0, s1, s2, s3 int32
	n := len(a) &^ 3
	b = b[:len(a)]
	for i := 0; i < n; i += 4 {
		s0 += int32(a[i]) * int32(b[i])
		s1 += int32(a[i+1]) * int32(b[i+1])
		s2 += int32(a[i+2]) * int32(b[i+2])
		s3 += int32(a[i+3]) * int32(b[i+3])
	}
	for i := n; i < len(a); i++ {
		s0 += int32(a[i]) * int32(b[i])
	}
	return s0 + s1 + s2 + s3
}

// ixVideoFrameHits finds the videos with a frame closest to q (a unit
// vector, such as the embedding of a description), best first, keeping
// only those keep accepts. Each video counts once, by its best frame.
//
// Not used by the search yet. To merge with picture hits (ixMLSearch, or
// box.store.nearest with the same q and keep): for each key take the
// larger of its picture Sim and its best frame Sim, then sort by it; both
// are cosines in the same space. A video's mean vector usually scores
// below its best frame, so a video found both ways should show the frame's
// score. Keep AtMs with the key so the viewer can seek to it
// (video.currentTime = AtMs/1000). Videos whose frames are not done yet
// are only in the picture hits, by their mean.
func (a *App) ixVideoFrameHits(ctx context.Context, q []float32, k int, keep func(key string) bool) []ixFrameHit {
	fs := a.ixFrameStore()
	if err := fs.load(ctx, a.db.Reader, ixMLModelOf(a.ixLoadSettings(ctx))); err != nil {
		return nil
	}
	return fs.best(q, k, keep)
}
