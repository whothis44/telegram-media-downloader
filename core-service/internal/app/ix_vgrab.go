package app

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/procutil"
	"github.com/botnick/telegram-media-downloader/core-service/internal/seekbar"
)

// Frames out of videos for image recognition, all of a video's frames in
// one ffmpeg run.
//
// Before, every frame was its own ffmpeg run that seeked exactly: ffmpeg
// started, opened the file (over the NAS), went to the keyframe before the
// time and decoded every frame up to it. With phone videos (a keyframe a
// second) that is cheap; with videos re-encoded at x264's defaults (a
// keyframe every 8–10 s) it is up to 250 full-size frames decoded for each
// one kept, 12 times a video.
//
// Now one run has an input per time, each seeked to its keyframe only
// (-noaccurate_seek: one frame decoded), scaled by ffmpeg, written as small
// JPEGs. Where that keyframe is too far before the time asked (more than
// half the distance to the next frame; never less than a second), that time
// is read again exactly, in a second run of all such times. For one
// generated 60 s 1280×720 H.264 video, the 18 frames of clip, faces and
// text (TestIxFramesOldAndNewOnALongVideo, two CPU cores): with a keyframe
// every 10 s, 3.3 s one by one, 1.5 s now (6 times read again); with a
// keyframe a second, 1.6 s one by one, 0.3 s now.
//
// The decoder is the duplicate finder's (Settings → duplicates: cuda by
// default). A run the GPU fails is done again on the CPU; after a few such
// failures the CPU is used for the rest of the app's run.
//
// Faces and text read the same three frames of a video, a while apart (one
// task after the other). The frames grabbed are kept in memory for a while
// (ixVFCacheMax, oldest dropped first), and a task takes the videos whose
// frames are held before others: the second task runs no ffmpeg for those.
// When a video's pictures are looked at first (clip), the frames faces and
// text still need are read in the same run.

// ixFrameReq is a frame wanted: at a time (seconds), at most Width pixels
// wide, as a JPEG of this ffmpeg quality (2 best). Slack is how far before
// At its keyframe may be and still stand for it.
type ixFrameReq struct {
	At             float64
	Width, Quality int
	Slack          float64
}

// ixGrabbed is a frame read: AtMs is where it really is in the video.
type ixGrabbed struct {
	Req  ixFrameReq
	AtMs int64
	Pic  []byte
}

// ixFrameSet is the frames one kind of work reads of a video.
type ixFrameSet struct {
	name           string // "embed" or "full"
	times          []float64
	width, quality int
}

func (s ixFrameSet) reqs() []ixFrameReq {
	// Half the distance between frames: a keyframe nearer its own time than
	// the next frame's still stands for it.
	gap := math.Inf(1)
	ts := append([]float64(nil), s.times...)
	sort.Float64s(ts)
	for i := 1; i < len(ts); i++ {
		if g := ts[i] - ts[i-1]; g > 0 {
			gap = math.Min(gap, g)
		}
	}
	slack := 1.0
	if !math.IsInf(gap, 1) {
		slack = math.Max(slack, gap/2)
	}
	out := make([]ixFrameReq, len(s.times))
	for i, t := range s.times {
		out[i] = ixFrameReq{At: t, Width: s.width, Quality: s.quality, Slack: slack}
	}
	return out
}

// ixEmbedFrames: what a video's pictures are looked at by (clip, frames).
func ixEmbedFrames(dur float64) ixFrameSet {
	return ixFrameSet{"embed", ixMLFrameTimes(dur), 512, 3}
}

// ixFullFrames: what faces and text are read from.
func ixFullFrames(dur float64) ixFrameSet {
	return ixFrameSet{"full", ixVideoThreeTimes(dur), 1280, 2}
}

// ixFrameSetOf is the frames a task reads of a video.
func ixFrameSetOf(task string) string {
	switch task {
	case "clip", "frames":
		return "embed"
	case "faces", "text":
		return "full"
	}
	return ""
}

func ixFrameSetFor(name string, dur float64) ixFrameSet {
	if name == "full" {
		return ixFullFrames(dur)
	}
	return ixEmbedFrames(dur)
}

// ixGrabFrames reads frames of a video, all in one go, with this decoder
// ("" the CPU). A time that gives no frame is left out. Replaceable in tests.
var ixGrabFrames = ixFFmpegFrames

// ixFFmpegFrames: each time from its keyframe, then the times whose
// keyframe is too far before them exactly.
func ixFFmpegFrames(ctx context.Context, full string, reqs []ixFrameReq, hw string) ([]ixGrabbed, error) {
	got, err := ixFFmpegFramesRun(ctx, full, reqs, hw, false)
	if err != nil {
		return nil, err
	}
	var again []ixFrameReq
	var keep []ixGrabbed
	far := map[int]bool{}
	for i, g := range got {
		if g.Req.At-float64(g.AtMs)/1000 > g.Req.Slack+0.001 {
			far[i] = true
		}
	}
	seen := map[ixFrameReq]bool{}
	for i, g := range got {
		if !far[i] {
			keep = append(keep, g)
		} else if !seen[g.Req] {
			seen[g.Req] = true
			again = append(again, g.Req)
		}
	}
	if len(again) == 0 {
		return keep, nil
	}
	ixVFUseOf(ctx).accurate.Add(1)
	exact, err := ixFFmpegFramesRun(ctx, full, again, hw, true)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The keyframes are still frames of the video.
		for i, g := range got {
			if far[i] {
				keep = append(keep, g)
			}
		}
		return keep, nil
	}
	return append(keep, exact...), nil
}

var (
	ixShowinfoRe = regexp.MustCompile(`showinfo@o(\d+) @ [^\]]*\]\s*n:\s*\d+\s+pts:\s*-?\d+\s+pts_time:\s*(-?\d+(?:\.\d+)?)`)
	ixStartRe    = regexp.MustCompile(`Duration:[^,]*, start: (-?\d+(?:\.\d+)?)`)
)

// ixFFmpegFramesRun is one ffmpeg run: an input per time (seeked to its
// keyframe, or exactly), an output per frame wanted. Each output has its
// own filters, so one that gets no frame (a time past the end) does not
// keep ffmpeg decoding the others to their end.
func ixFFmpegFramesRun(ctx context.Context, full string, reqs []ixFrameReq, hw string, exact bool) ([]ixGrabbed, error) {
	bin := seekbar.FFmpegPath()
	if bin == "" {
		return nil, ixErrNoFFmpeg
	}
	if len(reqs) == 0 {
		return nil, nil
	}
	dir, err := os.MkdirTemp("", "ixframes-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	fctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-loglevel", "info", "-copyts", "-y"}
	var times []float64
	input := map[float64]int{}
	for _, r := range reqs {
		at := math.Max(0, math.Round(r.At*1000)/1000)
		if _, ok := input[at]; ok {
			continue
		}
		input[at] = len(times)
		times = append(times, at)
		if hw != "" {
			args = append(args, "-hwaccel", hw)
		}
		if exact {
			args = append(args, "-threads", "4")
		} else {
			// One frame is decoded: frame threads would only decode more.
			args = append(args, "-noaccurate_seek", "-threads", "1")
		}
		args = append(args, "-ss", strconv.FormatFloat(at, 'f', 3, 64), "-i", full)
	}
	type out struct {
		req  ixFrameReq
		file string
	}
	var outs []out
	done := map[[3]int]int{} // input, width, quality → output
	idx := make([]int, len(reqs))
	for i, r := range reqs {
		in := input[math.Max(0, math.Round(r.At*1000)/1000)]
		k := [3]int{in, r.Width, r.Quality}
		if j, ok := done[k]; ok {
			idx[i] = j
			continue
		}
		j := len(outs)
		done[k], idx[i] = j, j
		file := filepath.Join(dir, fmt.Sprintf("o%d.jpg", j))
		outs = append(outs, out{r, file})
		args = append(args, "-map", fmt.Sprintf("%d:V:0", in), "-frames:v", "1",
			"-vf", fmt.Sprintf("scale='min(%d,iw)':-2,showinfo@o%d", r.Width, j),
			"-c:v", "mjpeg", "-q:v", strconv.Itoa(r.Quality), "-f", "image2", "-update", "1", file)
	}
	cmd := exec.CommandContext(fctx, bin, args...)
	procutil.HideWindow(cmd)
	var stderr ixTailBuffer
	cmd.Stderr = &stderr
	st := ixVFUseOf(ctx)
	st.runs.Add(1)
	if hw != "" {
		st.gpuRuns.Add(1)
	}
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	log := stderr.String()
	if runErr != nil {
		last := strings.TrimSpace(log)
		if i := strings.LastIndexByte(last, '\n'); i >= 0 {
			last = last[i+1:]
		}
		return nil, fmt.Errorf("ffmpeg: %v: %s", runErr, last)
	}
	start := 0.0
	if m := ixStartRe.FindStringSubmatch(log); m != nil {
		start, _ = strconv.ParseFloat(m[1], 64)
	}
	pts := map[int]float64{}
	for _, m := range ixShowinfoRe.FindAllStringSubmatch(log, -1) {
		j, _ := strconv.Atoi(m[1])
		if t, err := strconv.ParseFloat(m[2], 64); err == nil {
			if _, ok := pts[j]; !ok {
				pts[j] = t - start
			}
		}
	}
	pics := make([][]byte, len(outs))
	for j, o := range outs {
		if b, err := os.ReadFile(o.file); err == nil && len(b) > 0 {
			pics[j] = b
		}
	}
	var res []ixGrabbed
	for i, r := range reqs {
		j := idx[i]
		if pics[j] == nil {
			continue
		}
		at := r.At
		if t, ok := pts[j]; ok {
			at = t
		}
		res = append(res, ixGrabbed{Req: r, AtMs: max(0, int64(math.Round(at*1000))), Pic: pics[j]})
		st.frames.Add(1)
	}
	return res, nil
}

// ixTailBuffer keeps the first 256 KiB of ffmpeg's messages (the inputs
// and frames are at the start; a broken file can print without end).
type ixTailBuffer struct{ b bytes.Buffer }

func (t *ixTailBuffer) Write(p []byte) (int, error) {
	if room := 256<<10 - t.b.Len(); room > 0 {
		t.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (t *ixTailBuffer) String() string { return t.b.String() }

// ---------- the decoder ----------

// ixVFUse counts what one app did: videos read, and how.
type ixVFUse struct {
	videos, ns, gpuFailed, gpuInRow, oneByOne atomic.Int64
	runs, gpuRuns, accurate, frames           atomic.Int64 // ffmpeg runs
	hits, misses, prefetched                  atomic.Int64
	gpuOff                                    atomic.Bool
}

type ixVFUseKey struct{}

// ixVFUseOf is where ffmpeg runs are counted (a throwaway outside an app).
func ixVFUseOf(ctx context.Context) *ixVFUse {
	if u, ok := ctx.Value(ixVFUseKey{}).(*ixVFUse); ok {
		return u
	}
	return &ixVFUse{}
}

// ixVFrameHW is the decoder for frames: the duplicate finder's, unless the
// GPU failed repeatedly in this run.
func (a *App) ixVFrameHW(ctx context.Context) string {
	if a.ixVF().use.gpuOff.Load() {
		return ""
	}
	s := a.ixLoadSettings(ctx)
	switch s.DupHWAccel {
	case "none":
		return ""
	case "", "cuda":
		return "cuda"
	}
	return allowedThumbAccel(s.DupHWAccel)
}

// ixGrabWithFallback: on the GPU, then on the CPU when that fails, then
// frame by frame (as before) when even one run of them all fails.
func (a *App) ixGrabWithFallback(ctx context.Context, full string, reqs, own []ixFrameReq, hw string) ([]ixGrabbed, error) {
	st := &a.ixVF().use
	ctx = context.WithValue(ctx, ixVFUseKey{}, st)
	start := time.Now()
	st.videos.Add(1)
	defer func() { st.ns.Add(int64(time.Since(start))) }()
	got, err := ixGrabFrames(ctx, full, reqs, hw)
	if err == nil || ctx.Err() != nil || errors.Is(err, ixErrNoFFmpeg) {
		if err == nil && hw != "" {
			st.gpuInRow.Store(0)
		}
		return got, err
	}
	if hw != "" {
		got, err = ixGrabFrames(ctx, full, reqs, "")
		if err == nil {
			// The CPU could: the GPU is at fault.
			st.gpuFailed.Add(1)
			if st.gpuInRow.Add(1) >= 3 || st.gpuFailed.Load() >= 10 {
				st.gpuOff.Store(true)
			}
			return got, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	// One frame at a time, the frames this task needs only.
	st.oneByOne.Add(1)
	var out []ixGrabbed
	for _, r := range own {
		pic, err := ixGrabFrame(ctx, full, r.At, r.Width, r.Quality)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ixErrNoFFmpeg) {
			return nil, err
		}
		if err == nil && len(pic) > 0 {
			out = append(out, ixGrabbed{Req: r, AtMs: int64(math.Round(r.At * 1000)), Pic: pic})
		}
	}
	return out, nil
}

// ---------- frames kept for the next task ----------

// ixVFCacheMax is how many bytes of frames are kept; ixVFCacheFor how long
// a video's frames are kept after their last use.
var (
	ixVFCacheMax int64 = 256 << 20
	ixVFCacheFor       = 20 * time.Minute
)

type ixVFFile struct {
	path      string
	size, mod int64
}

type ixVFReqKey struct {
	atMs           int64
	width, quality int
}

func ixVFKeyOf(r ixFrameReq) ixVFReqKey {
	return ixVFReqKey{int64(math.Round(r.At * 1000)), r.Width, r.Quality}
}

type ixVFEntry struct {
	file  ixVFFile
	item  string
	got   map[ixVFReqKey][]ixGrabbed // nil: asked, no frame
	bytes int64
	used  time.Time
}

type ixVFCache struct {
	mu     sync.Mutex
	bytes  int64
	lru    *list.List // of *ixVFEntry, oldest at the front
	byFile map[ixVFFile]*list.Element
	timer  *time.Timer
}

// ixVFRound is the tasks of the current round of image recognition.
type ixVFRound struct {
	mu    sync.Mutex
	tasks []*ixMLTask
}

type ixVFState struct {
	cache ixVFCache
	round ixVFRound
	use   ixVFUse
}

var ixVFStates sync.Map // *App -> *ixVFState

func (a *App) ixVF() *ixVFState {
	if v, ok := ixVFStates.Load(a); ok {
		return v.(*ixVFState)
	}
	v, _ := ixVFStates.LoadOrStore(a, &ixVFState{cache: ixVFCache{lru: list.New(), byFile: map[ixVFFile]*list.Element{}}})
	return v.(*ixVFState)
}

// get gives the frames of these requests when all of them were read
// before.
func (c *ixVFCache) get(f ixVFFile, reqs []ixFrameReq) ([]ixGrabbed, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(time.Now())
	el, ok := c.byFile[f]
	if !ok {
		return nil, false
	}
	e := el.Value.(*ixVFEntry)
	var out []ixGrabbed
	for _, r := range reqs {
		gs, ok := e.got[ixVFKeyOf(r)]
		if !ok {
			return nil, false
		}
		out = append(out, gs...)
	}
	e.used = time.Now()
	c.lru.MoveToBack(el)
	return out, true
}

// put keeps what was read for these requests.
func (c *ixVFCache) put(f ixVFFile, item string, reqs []ixFrameReq, got []ixGrabbed) {
	if ixVFCacheMax <= 0 {
		return
	}
	by := map[ixVFReqKey][]ixGrabbed{}
	for _, g := range got {
		k := ixVFKeyOf(g.Req)
		by[k] = append(by[k], g)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var e *ixVFEntry
	if el, ok := c.byFile[f]; ok {
		e = el.Value.(*ixVFEntry)
		c.lru.MoveToBack(el)
	} else {
		e = &ixVFEntry{file: f, got: map[ixVFReqKey][]ixGrabbed{}}
		c.byFile[f] = c.lru.PushBack(e)
	}
	e.item, e.used = item, time.Now()
	for _, r := range reqs {
		k := ixVFKeyOf(r)
		if _, ok := e.got[k]; ok {
			continue
		}
		gs := by[k]
		e.got[k] = gs
		for _, g := range gs {
			e.bytes += int64(len(g.Pic))
			c.bytes += int64(len(g.Pic))
		}
		e.bytes += 64
		c.bytes += 64
	}
	for c.bytes > ixVFCacheMax && c.lru.Len() > 0 {
		c.dropLocked(c.lru.Front())
	}
	if c.timer == nil && c.lru.Len() > 0 {
		c.timer = time.AfterFunc(ixVFCacheFor, c.sweep)
	}
}

func (c *ixVFCache) sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timer = nil
	c.expireLocked(time.Now())
	if c.lru.Len() > 0 {
		c.timer = time.AfterFunc(ixVFCacheFor, c.sweep)
	}
}

func (c *ixVFCache) expireLocked(now time.Time) {
	for c.lru.Len() > 0 {
		el := c.lru.Front()
		if now.Sub(el.Value.(*ixVFEntry).used) < ixVFCacheFor {
			return
		}
		c.dropLocked(el)
	}
}

func (c *ixVFCache) dropLocked(el *list.Element) {
	e := el.Value.(*ixVFEntry)
	c.lru.Remove(el)
	delete(c.byFile, e.file)
	c.bytes -= e.bytes
}

func (c *ixVFCache) drop(f ixVFFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byFile[f]; ok {
		c.dropLocked(el)
	}
}

// items lists the files whose frames are held, oldest first.
func (c *ixVFCache) items() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(time.Now())
	out := make([]string, 0, c.lru.Len())
	for el := c.lru.Front(); el != nil; el = el.Next() {
		if k := el.Value.(*ixVFEntry).item; k != "" {
			out = append(out, k)
		}
	}
	return out
}

func (c *ixVFCache) size() (int, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len(), c.bytes
}

// ixVFRoundTasks records the tasks of a round (ixMLRoundAll), so frames
// read for one task can be read and kept for the others.
func (a *App) ixVFRoundTasks(tasks []*ixMLTask) {
	r := &a.ixVF().round
	r.mu.Lock()
	r.tasks = tasks
	r.mu.Unlock()
}

type ixVFTaskKey struct{}

// ixVFWithTask marks the work as done for this task.
func ixVFWithTask(ctx context.Context, task string) context.Context {
	return context.WithValue(ctx, ixVFTaskKey{}, task)
}

func ixVFTaskOf(ctx context.Context) string {
	s, _ := ctx.Value(ixVFTaskKey{}).(string)
	return s
}

// ixVFNeeds says whether a task of this round other than the current one
// still has to read these frames of the file. idleToo counts the tasks that
// run only when the others have nothing left (they come much later).
func (a *App) ixVFNeeds(ctx context.Context, it *ixMItem, set string, idleToo bool) bool {
	cur := ixVFTaskOf(ctx)
	r := &a.ixVF().round
	r.mu.Lock()
	tasks := r.tasks
	r.mu.Unlock()
	for _, t := range tasks {
		if t.name == cur || ixFrameSetOf(t.name) != set || (t.idle && !idleToo) {
			continue
		}
		if t.wanted(it) && !t.done(it.Key) {
			return true
		}
	}
	return false
}

// ixVFCachedFirst lists the files whose frames are held that this task
// still has to do, oldest first (ixMLNext takes them before others).
func (a *App) ixVFCachedFirst(t *ixMLTask) []string {
	if ixFrameSetOf(t.name) == "" {
		return nil
	}
	return a.ixVF().cache.items()
}

// ixVideoFramesOf reads a video's frames of a set: from the frames kept,
// or from the file, with the frames the other tasks will want of it.
func (a *App) ixVideoFramesOf(ctx context.Context, it *ixMItem, full string, set ixFrameSet) ([]ixMLFrame, error) {
	fi, err := os.Stat(full)
	if err != nil {
		return nil, errors.New("file not found")
	}
	file := ixVFFile{full, fi.Size(), fi.ModTime().UnixNano()}
	cache := &a.ixVF().cache
	st := &a.ixVF().use
	frames := func(gs []ixGrabbed) []ixMLFrame {
		out := make([]ixMLFrame, 0, len(gs))
		for _, g := range gs {
			out = append(out, ixMLFrame{AtMs: g.AtMs, Pic: g.Pic})
		}
		return out
	}
	release := func() {
		// Nobody else wants this file's frames: they need not be kept.
		if !a.ixVFNeeds(ctx, it, "embed", true) && !a.ixVFNeeds(ctx, it, "full", true) {
			cache.drop(file)
		}
	}
	read := func(own []ixFrameReq) ([]ixGrabbed, error) {
		if gs, ok := cache.get(file, own); ok {
			st.hits.Add(1)
			release()
			return gs, nil
		}
		st.misses.Add(1)
		reqs := own
		keep := a.ixVFNeeds(ctx, it, set.name, true)
		// The frames the other tasks of this round will read next.
		for _, other := range []string{"embed", "full"} {
			if other != set.name && a.ixVFNeeds(ctx, it, other, false) {
				reqs = append(append([]ixFrameReq(nil), reqs...), ixFrameSetFor(other, it.Dur).reqs()...)
				st.prefetched.Add(1)
				keep = true
			}
		}
		clock := ixMLClock(ctx, ixStFFmpeg)
		gs, err := a.ixGrabWithFallback(ctx, full, reqs, own, a.ixVFrameHW(ctx))
		clock()
		if err != nil {
			return nil, err
		}
		if keep {
			cache.put(file, it.Key, reqs, gs)
		}
		var mine []ixGrabbed
		ownKey := map[ixVFReqKey]bool{}
		for _, r := range own {
			ownKey[ixVFKeyOf(r)] = true
		}
		for _, g := range gs {
			if ownKey[ixVFKeyOf(g.Req)] {
				mine = append(mine, g)
			}
		}
		return mine, nil
	}
	own := set.reqs()
	gs, err := read(own)
	if err != nil {
		return nil, err
	}
	if len(gs) == 0 {
		// A clip shorter than it says, or a wrong length: the very first
		// frame is always there.
		triedStart := false
		for _, r := range own {
			triedStart = triedStart || r.At == 0
		}
		if !triedStart {
			if gs, err = read([]ixFrameReq{{At: 0, Width: set.width, Quality: set.quality, Slack: 1}}); err != nil {
				return nil, err
			}
		}
	}
	if len(gs) == 0 {
		return nil, errors.New("no frame could be read from the video")
	}
	return frames(gs), nil
}

// ixVFrameReport is the diagnostics line of frames out of videos.
func (a *App) ixVFrameReport() string {
	use := &a.ixVF().use
	videos := use.videos.Load()
	per := func(v int64) float64 {
		if videos == 0 {
			return 0
		}
		return float64(v) / float64(videos)
	}
	n, b := a.ixVF().cache.size()
	dec := "cpu"
	if use.gpuOff.Load() {
		dec = "cpu (gpu off after failures)"
	} else if use.gpuRuns.Load() > 0 {
		dec = "gpu"
	}
	return fmt.Sprintf("videos read %d (%.0f ms, %.1f ffmpeg runs, %.1f frames each; decoder %s; gpu failed→cpu %d; exact re-reads %d; frame by frame %d) · frames kept: %d videos, %d MB; reused %d times (%d read with the frames of other tasks)",
		videos, per(use.ns.Load())/1e6, per(use.runs.Load()), per(use.frames.Load()), dec, use.gpuFailed.Load(), use.accurate.Load(), use.oneByOne.Load(),
		n, b>>20, use.hits.Load(), use.prefetched.Load())
}
