package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/seekbar"
)

// ixVGVideo makes a test video whose brightness tells the time: luma
// 16 + 10×t (limited range), so a frame says where it is. keyEvery is the
// keyframe distance in frames (25 a second).
func ixVGVideo(t *testing.T, dir string, secs, keyEvery int, size string) string {
	t.Helper()
	bin := seekbar.FFmpegPath()
	if bin == "" {
		t.Skip("ffmpeg is not installed")
	}
	out := filepath.Join(dir, fmt.Sprintf("t%d-g%d-%s.mp4", secs, keyEvery, size))
	slope := 200.0 / float64(secs)
	cmd := exec.Command(bin, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=black:s=%s:r=25:d=%d,format=yuv420p,geq=lum='16+%g*T':cb=128:cr=128", size, secs, slope),
		"-c:v", "mpeg4", "-q:v", "4", "-bf", "0", "-g", fmt.Sprint(keyEvery), out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot make a test video: %v %s", err, b)
	}
	return out
}

// ixVGBusyVideo makes a 60 s 1280×720 moving test picture: H.264 when
// ffmpeg has x264 (as most downloaded videos are), else MPEG-4.
func ixVGBusyVideo(t *testing.T, dir string, secs, keyEvery int) string {
	t.Helper()
	bin := seekbar.FFmpegPath()
	if bin == "" {
		t.Skip("ffmpeg is not installed")
	}
	out := filepath.Join(dir, fmt.Sprintf("busy-g%d.mp4", keyEvery))
	src := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=1280x720:rate=25:duration=%d", secs)}
	x264 := append(append([]string(nil), src...), "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", fmt.Sprint(keyEvery), out)
	if exec.Command(bin, x264...).Run() == nil {
		return out
	}
	mp4 := append(append([]string(nil), src...), "-c:v", "mpeg4", "-q:v", "4", "-g", fmt.Sprint(keyEvery), out)
	if b, err := exec.Command(bin, mp4...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot make a test video: %v %s", err, b)
	}
	return out
}

// ixVGTimeOf reads the time a test frame shows.
func ixVGTimeOf(t *testing.T, pic []byte, secs int) float64 {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(pic))
	if err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
	var sum, n float64
	switch p := img.(type) {
	case *image.YCbCr:
		for _, y := range p.Y {
			sum += float64(y)
			n++
		}
	case *image.Gray:
		for _, y := range p.Pix {
			sum += float64(y)
			n++
		}
	default:
		t.Fatalf("picture %T", img)
	}
	y := sum / n // full range in the JPEG
	return y * 219 / 255 / (200.0 / float64(secs))
}

func TestIxFFmpegFramesAtTheRightTimes(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	check := func(name string, got []ixGrabbed, wantMs []int64, secs int) {
		t.Helper()
		var at []int64
		for _, g := range got {
			at = append(at, g.AtMs)
			shown := ixVGTimeOf(t, g.Pic, secs)
			if math.Abs(shown-float64(g.AtMs)/1000) > 0.3 {
				t.Errorf("%s: frame said to be at %d ms shows %.2f s", name, g.AtMs, shown)
			}
		}
		if len(at) != len(wantMs) {
			t.Fatalf("%s: frames at %v, want %v", name, at, wantMs)
		}
		for i := range at {
			if d := at[i] - wantMs[i]; d < -60 || d > 60 {
				t.Fatalf("%s: frames at %v, want %v", name, at, wantMs)
			}
		}
	}
	// A keyframe every 2 s: each frame is its keyframe, within half the
	// distance to the next frame; nothing read again.
	short := ixVGVideo(t, dir, 20, 50, "160x120")
	use := &ixVFUse{}
	ctx = context.WithValue(ctx, ixVFUseKey{}, use)
	got, err := ixFFmpegFrames(ctx, short, ixEmbedFrames(20).reqs(), "")
	if err != nil {
		t.Fatal(err)
	}
	check("keyframes", got, []int64{2000, 6000, 12000, 16000}, 20)
	if use.accurate.Load() != 0 || use.runs.Load() != 1 {
		t.Fatal("read again exactly though the keyframes were near")
	}
	if w := ixJPEGWidth(t, got[0].Pic); w != 160 {
		t.Fatalf("width %d", w)
	}
	// A keyframe every 10 s: the three faces/text times (4, 10, 16 s) would
	// be 0, 10 and 10; 4 and 16 are read again exactly.
	long := ixVGVideo(t, dir, 20, 250, "160x120")
	got, err = ixFFmpegFrames(ctx, long, ixFullFrames(20).reqs(), "")
	if err != nil {
		t.Fatal(err)
	}
	check("exact", got, []int64{10000, 4000, 16000}, 20)
	if use.accurate.Load() != 1 || use.runs.Load() != 3 {
		t.Fatal("not read again exactly")
	}
	// Two sizes of one time come out of one input; a time past the end
	// gives nothing, quickly.
	start := time.Now()
	reqs := []ixFrameReq{{At: 5, Width: 64, Quality: 3, Slack: 1}, {At: 5, Width: 1280, Quality: 2, Slack: 1}, {At: 40, Width: 64, Quality: 3, Slack: 1}}
	got, err = ixFFmpegFrames(ctx, short, reqs, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || ixJPEGWidth(t, got[0].Pic) != 64 || ixJPEGWidth(t, got[1].Pic) != 160 || got[0].AtMs != 4000 {
		t.Fatalf("sizes: %d frames", len(got))
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("a time past the end took %v", d)
	}
}

func ixJPEGWidth(t *testing.T, pic []byte) int {
	t.Helper()
	c, err := jpeg.DecodeConfig(bytes.NewReader(pic))
	if err != nil {
		t.Fatal(err)
	}
	return c.Width
}

// The GPU decoder failing (no CUDA here, or a file it cannot do) falls
// back to the CPU; after three such failures the CPU is used from then on.
func TestIxFramesGPUFallsBackToCPU(t *testing.T) {
	short := ixVGVideo(t, t.TempDir(), 6, 25, "160x120")
	a, ctx := ix8App(t, map[string]any{"dupHwaccel": "cuda"})
	if hw := a.ixVFrameHW(ctx); hw != "cuda" {
		t.Fatalf("decoder %q", hw)
	}
	reqs := ixEmbedFrames(6).reqs()
	for i := 0; i < 3; i++ {
		got, err := a.ixGrabWithFallback(ctx, short, reqs, reqs, a.ixVFrameHW(ctx))
		if err != nil || len(got) != 3 {
			t.Fatalf("frames %d: %v", len(got), err)
		}
	}
	use := &a.ixVF().use
	if use.gpuFailed.Load() == 0 {
		t.Skip("the GPU decoder works here")
	}
	if !use.gpuOff.Load() || a.ixVFrameHW(ctx) != "" {
		t.Fatalf("GPU still used after %d failures", use.gpuFailed.Load())
	}
	if !strings.Contains(a.ixVFrameReport(), "gpu off") {
		t.Fatalf("report: %s", a.ixVFrameReport())
	}
	b, _ := ix8App(t, map[string]any{"dupHwaccel": "none"})
	if hw := b.ixVFrameHW(ctx); hw != "" {
		t.Fatalf("none: %q", hw)
	}
}

// When one run for all frames fails even on the CPU, each frame is read on
// its own, as before.
func TestIxFramesOneByOneWhenARunFails(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{"dupHwaccel": "none"})
	oldAll, oldOne := ixGrabFrames, ixGrabFrame
	t.Cleanup(func() { ixGrabFrames, ixGrabFrame = oldAll, oldOne })
	ixGrabFrames = func(context.Context, string, []ixFrameReq, string) ([]ixGrabbed, error) {
		return nil, errors.New("ffmpeg: exit status 1")
	}
	var asked []float64
	ixGrabFrame = func(_ context.Context, _ string, at float64, _, _ int) ([]byte, error) {
		asked = append(asked, at)
		return []byte("jpeg"), nil
	}
	own := ixFullFrames(10).reqs()
	extra := append(append([]ixFrameReq(nil), own...), ixEmbedFrames(10).reqs()...)
	got, err := a.ixGrabWithFallback(ctx, "x.mp4", extra, own, "")
	if err != nil || len(got) != 3 || fmt.Sprint(asked) != "[2 5 8]" || a.ixVF().use.oneByOne.Load() != 1 {
		t.Fatalf("frames %d %v asked %v", len(got), err, asked)
	}
}

// ixVGTasks is a round of the four tasks that read video frames, with
// what each has done.
func ixVGTasks(a *App, done map[string]map[string]bool) []*ixMLTask {
	var mu sync.Mutex
	var out []*ixMLTask
	for _, name := range []string{"clip", "faces", "text", "frames"} {
		name := name
		out = append(out, &ixMLTask{name: name, idle: name == "frames",
			wanted: func(it *ixMItem) bool { return it.Type == "video" },
			done: func(k string) bool {
				mu.Lock()
				defer mu.Unlock()
				return done[name][k]
			}})
	}
	return out
}

// One ffmpeg run per video serves clip, faces and text: clip reads the
// frames faces and text need too, those keep them until both are done.
func TestIxFramesAreSharedBetweenTasks(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{"dupHwaccel": "none"})
	m := ixVFModel([][3]string{{"v1", "video", "30"}, {"v2", "video", "30"}})
	ixVFFiles(t, a, m, nil)
	runs := map[string]int{}
	var mu sync.Mutex
	oldAll, oldHave := ixGrabFrames, ixHaveFFmpeg
	t.Cleanup(func() { ixGrabFrames, ixHaveFFmpeg = oldAll, oldHave })
	ixHaveFFmpeg = func() bool { return true }
	ixGrabFrames = func(_ context.Context, full string, reqs []ixFrameReq, _ string) ([]ixGrabbed, error) {
		mu.Lock()
		runs[filepath.Base(full)]++
		mu.Unlock()
		var out []ixGrabbed
		for _, r := range reqs {
			out = append(out, ixGrabbed{Req: r, AtMs: int64(r.At * 1000), Pic: bytes.Repeat([]byte{byte(r.Quality)}, 1000)})
		}
		return out, nil
	}
	done := map[string]map[string]bool{"clip": {}, "faces": {}, "text": {}, "frames": {}}
	a.ixVFRoundTasks(ixVGTasks(a, done))
	defer a.ixVFRoundTasks(nil)
	it := &m.Items[m.ByKey["v1"]]
	full := filepath.Join(a.downloadsDir, filepath.FromSlash(it.Path))
	as := func(task string) context.Context { return ixVFWithTask(ctx, task) }

	clip, err := a.ixVideoFramesOf(as("clip"), it, full, ixEmbedFrames(30))
	if err != nil || len(clip) != 6 || len(clip[0].Pic) != 1000 || clip[0].Pic[0] != 3 {
		t.Fatalf("clip: %d %v", len(clip), err)
	}
	done["clip"]["v1"], done["frames"]["v1"] = true, true
	faces, err := a.ixVideoFramesOf(as("faces"), it, full, ixFullFrames(30))
	if err != nil || len(faces) != 3 || faces[1].AtMs != 15000 || faces[0].Pic[0] != 2 {
		t.Fatalf("faces: %d %v", len(faces), err)
	}
	done["faces"]["v1"] = true
	if n, _ := a.ixVF().cache.size(); n != 1 {
		t.Fatal("frames dropped before text read them")
	}
	text, err := a.ixVideoFramesOf(as("text"), it, full, ixFullFrames(30))
	if err != nil || len(text) != 3 {
		t.Fatalf("text: %d %v", len(text), err)
	}
	if runs["v1.mp4"] != 1 {
		t.Fatalf("ffmpeg runs for one video: %d", runs["v1.mp4"])
	}
	if n, b := a.ixVF().cache.size(); n != 0 || b != 0 {
		t.Fatalf("frames kept after every task read them: %d, %d bytes", n, b)
	}
	use := &a.ixVF().use
	if use.hits.Load() != 2 || use.prefetched.Load() != 1 {
		t.Fatalf("reused %d, with others' frames %d", use.hits.Load(), use.prefetched.Load())
	}
	// Outside a round (tests, the copies check) nothing is kept.
	a.ixVFRoundTasks(nil)
	if _, err := a.ixVideoFramesOf(ctx, &m.Items[m.ByKey["v2"]], filepath.Join(a.downloadsDir, "v", "v2.mp4"), ixFullFrames(30)); err != nil {
		t.Fatal(err)
	}
	if n, _ := a.ixVF().cache.size(); n != 0 {
		t.Fatal("frames kept with nobody to read them")
	}
}

func TestIxFrameCacheIsBoundedAndTaskTakesHeldFirst(t *testing.T) {
	a, _ := ix8App(t, nil)
	oldMax := ixVFCacheMax
	t.Cleanup(func() { ixVFCacheMax = oldMax })
	ixVFCacheMax = 10_000
	c := &a.ixVF().cache
	pic := make([]byte, 3000)
	for i := 0; i < 5; i++ {
		f := ixVFFile{path: fmt.Sprintf("v%d", i), size: 1}
		r := []ixFrameReq{{At: 1, Width: 512, Quality: 3}}
		c.put(f, fmt.Sprintf("k%d", i), r, []ixGrabbed{{Req: r[0], Pic: pic}})
	}
	n, b := c.size()
	if b > ixVFCacheMax || n != 3 || fmt.Sprint(c.items()) != "[k2 k3 k4]" {
		t.Fatalf("held %d videos, %d bytes: %v", n, b, c.items())
	}
	// Used again: last to go.
	if _, ok := c.get(ixVFFile{path: "v2", size: 1}, []ixFrameReq{{At: 1, Width: 512, Quality: 3}}); !ok {
		t.Fatal("not held")
	}
	if _, ok := c.get(ixVFFile{path: "v2", size: 2}, []ixFrameReq{{At: 1, Width: 512, Quality: 3}}); ok {
		t.Fatal("a changed file's frames were given")
	}
	if fmt.Sprint(c.items()) != "[k3 k4 k2]" {
		t.Fatalf("order %v", c.items())
	}
	// Old frames go.
	oldFor := ixVFCacheFor
	t.Cleanup(func() { ixVFCacheFor = oldFor })
	ixVFCacheFor = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	if len(c.items()) != 0 {
		t.Fatal("old frames kept")
	}
	ixVFCacheFor = oldFor

	// A task takes the videos whose frames are held before the others.
	m := ixVFModel([][3]string{{"a", "video", "10"}, {"b", "video", "10"}, {"c", "video", "10"}, {"p", "photo", ""}})
	r := []ixFrameReq{{At: 1, Width: 1280, Quality: 2}}
	c.put(ixVFFile{path: "c"}, "c", r, nil)
	text := ixVGTasks(a, map[string]map[string]bool{"text": {}})[2]
	run := &ixMLRun{failed: map[string]bool{}, tried: map[string]bool{}}
	var order []string
	for {
		next := a.ixMLNext(text, m, run, 1)
		if len(next) == 0 {
			break
		}
		order = append(order, next[0].Key)
	}
	if fmt.Sprint(order) != "[c a b]" {
		t.Fatalf("order %v", order)
	}
}

// TestIxFramesOldAndNewOnALongVideo times a 60 s video's frames for clip,
// faces and text: before, one ffmpeg run per frame (12 + 3 + 3); now one
// run for all, and faces and text reuse it. CPU only.
func TestIxFramesOldAndNewOnALongVideo(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	dir := t.TempDir()
	for _, g := range []int{250, 25} { // x264's default keyframe distance, and a keyframe a second
		video := ixVGBusyVideo(t, dir, 60, g)
		a, ctx := ix8App(t, map[string]any{"dupHwaccel": "none"})
		it := &ixMItem{Key: "v", Type: "video", Dur: 60}
		embed, fullSet := ixEmbedFrames(60), ixFullFrames(60)

		start := time.Now()
		var oldFrames int
		for _, set := range []ixFrameSet{embed, fullSet, fullSet} { // clip, faces, text
			for _, at := range set.times {
				if pic, err := ixGrabFrame(ctx, video, at, set.width, set.quality); err == nil && len(pic) > 0 {
					oldFrames++
				}
			}
		}
		old := time.Since(start)

		a.ixVFRoundTasks(ixVGTasks(a, map[string]map[string]bool{"clip": {}, "faces": {}, "text": {}, "frames": {}}))
		start = time.Now()
		var newFrames int
		for _, task := range []string{"clip", "faces", "text"} {
			set := embed
			if task != "clip" {
				set = fullSet
			}
			fs, err := a.ixVideoFramesOf(ixVFWithTask(ctx, task), it, video, set)
			if err != nil {
				t.Fatal(err)
			}
			newFrames += len(fs)
		}
		now := time.Since(start)
		a.ixVFRoundTasks(nil)
		t.Logf("60 s 1280×720, keyframe every %d frames: one run per frame %v (%d frames), now %v (%d frames) · %s",
			g, old.Round(time.Millisecond), oldFrames, now.Round(time.Millisecond), newFrames, a.ixVFrameReport())
		if oldFrames != 18 || newFrames != 18 {
			t.Fatalf("frames: before %d, now %d", oldFrames, newFrames)
		}
		if now >= old {
			t.Errorf("not faster: %v vs %v", now, old)
		}
		_ = os.Remove(video)
	}
}
