package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ixVFVec turns "3:1,5:0.2" into a unit vector of 32 numbers.
func ixVFVec(spec string) []float32 {
	v := make([]float32, 32)
	for _, p := range strings.Split(spec, ",") {
		i, w, ok := strings.Cut(p, ":")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(i)
		f, _ := strconv.ParseFloat(w, 64)
		v[n%32] += float32(f)
	}
	if !ixNormalise(v) {
		v[31] = 1
	}
	return v
}

// ixVFFakeML answers embeddings: a picture ending in "#<spec>", or a text
// "<spec>", gets ixVFVec(spec). It counts the pictures it was sent.
func ixVFFakeML(t *testing.T, pictures *int) *httptest.Server {
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			_, _ = io.WriteString(w, "pong")
			return
		}
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		spec := r.FormValue("text")
		if f, _, err := r.FormFile("image"); err == nil {
			b, _ := io.ReadAll(f)
			spec = "30:1"
			if i := bytes.LastIndexByte(b, '#'); i >= 0 {
				spec = string(b[i+1:])
			}
			mu.Lock()
			*pictures++
			mu.Unlock()
		}
		raw, _ := json.Marshal(ixVFVec(spec))
		_ = json.NewEncoder(w).Encode(map[string]any{"clip": string(raw)})
	}))
}

// ixVFModel: one chat with the given files (key → type), each its own
// duplicate set, all downloaded.
func ixVFModel(files [][3]string) *ixModel {
	m := &ixModel{GroupName: map[string]string{"G": "G"}, GroupCat: map[string]string{}, ByKey: map[string]int32{}, ByMsg: map[string]int32{}}
	for i, f := range files {
		ii := int32(len(m.Items))
		dur, _ := strconv.ParseFloat(f[2], 64)
		ext := ".jpg"
		if f[1] == "video" {
			ext = ".mp4"
		}
		m.Items = append(m.Items, ixMItem{ID: int64(i + 1), Group: "G", Msg: i + 1, Key: f[0], Cluster: int32(i), Post: int32(i), Type: f[1], Dur: dur, Path: "v/" + f[0] + ext, Status: "ok"})
		m.Clusters = append(m.Clusters, ixMCluster{Canon: ii, Items: []int32{ii}})
		m.ByKey[f[0]] = ii
		m.Posts = append(m.Posts, ixMPost{Idx: int32(i), Group: "G", Msg: i + 1, Date: int64(i), Origin: int64(i), Family: -1, Items: []int32{ii}})
	}
	for i := range m.Posts {
		m.ByMsg[ixPostRef(&m.Posts[i])] = int32(i)
	}
	return m
}

// ixVFFiles writes the model's files: photos hold their spec; videos are
// empty (their frames come from ixGrabFrame).
func ixVFFiles(t *testing.T, a *App, m *ixModel, photo map[string]string) {
	t.Helper()
	for _, it := range m.Items {
		full := filepath.Join(a.downloadsDir, filepath.FromSlash(it.Path))
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		var body []byte
		if it.Type == "photo" {
			body = append(ixMLTestPicture(), []byte("#"+photo[it.Key])...)
		}
		if err := os.WriteFile(full, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ixVFGrab fakes ffmpeg: real[file] is the true length of each video and
// scene(file, t) what is seen at t. A time past the end gives no frame.
// Every time asked is recorded.
func ixVFGrab(t *testing.T, real map[string]float64, scene func(file string, at float64) string) *[]string {
	var mu sync.Mutex
	asked := &[]string{}
	old, oldHave := ixGrabFrame, ixHaveFFmpeg
	t.Cleanup(func() { ixGrabFrame, ixHaveFFmpeg = old, oldHave })
	ixHaveFFmpeg = func() bool { return true }
	ixGrabFrame = func(_ context.Context, full string, at float64, _, _ int) ([]byte, error) {
		file := strings.TrimSuffix(filepath.Base(full), filepath.Ext(full))
		mu.Lock()
		*asked = append(*asked, fmt.Sprintf("%s@%g", file, at))
		mu.Unlock()
		if at >= real[file] {
			return nil, fmt.Errorf("exit status 1")
		}
		return append(ixMLTestPicture(), []byte("#"+scene(file, at))...), nil
	}
	return asked
}

func TestIxVideoFrameTimes(t *testing.T) {
	for _, c := range []struct {
		dur   float64
		n     int
		first float64
	}{{0, 1, 0}, {0.4, 1, 0.2}, {2, 1, 1}, {3, 1, 1}, {4, 3, 4.0 / 6}, {20, 4, 2.5}, {60, 12, 2.5}, {3600, 12, 150}} {
		got := ixMLFrameTimes(c.dur)
		if len(got) != c.n || math.Abs(got[0]-c.first) > 1e-9 || got[len(got)-1] >= math.Max(c.dur, 0.001) {
			t.Fatalf("%v s: %v", c.dur, got)
		}
	}
	if got := ixVideoThreeTimes(10); len(got) != 3 || got[0] != 2 {
		t.Fatalf("faces and text times: %v", got)
	}
	if got := ixVideoThreeTimes(0.5); len(got) != 1 || got[0] != 0.25 {
		t.Fatalf("short clip, faces and text: %v", got)
	}
}

func TestIxShortVideosGetAFrame(t *testing.T) {
	pictures := 0
	srv := ixVFFakeML(t, &pictures)
	defer srv.Close()
	a, ctx := ix8App(t, map[string]any{"mlUrl": srv.URL})
	// gif: 0.4 s, length known. nolen: length unknown. lies: says 2 s but
	// is 0.3 s. broken: no frame anywhere.
	m := ixVFModel([][3]string{{"gif", "video", "0.4"}, {"nolen", "video", "0"}, {"lies", "video", "2"}, {"broken", "video", "10"}})
	ixVFFiles(t, a, m, nil)
	asked := ixVFGrab(t, map[string]float64{"gif": 0.4, "nolen": 0.5, "lies": 0.3, "broken": 0}, func(string, float64) string { return "1:1" })
	c := a.ixMLClientNow(ctx)
	_ = a.ixMLBox().store.load(ctx, a.db.Reader, c.model)
	a.ixMLRound(ctx, c, m, 50)
	for _, k := range []string{"gif", "nolen", "lies"} {
		if !a.ixMLBox().store.has(k) {
			t.Fatalf("%s not looked at; asked %v", k, *asked)
		}
	}
	if a.ixMLBox().store.has("broken") {
		t.Fatal("a video without frames was looked at")
	}
	want := map[string]bool{"gif@0.2": true, "nolen@0": true, "lies@1": true, "lies@0": true, "broken@1.6666666666666667": true, "broken@5": true, "broken@0": true}
	got := map[string]bool{}
	for _, s := range *asked {
		got[s] = true
	}
	for s := range want {
		if !got[s] {
			t.Fatalf("not asked for %s: %v", s, *asked)
		}
	}
	if got["nolen@1"] || got["gif@1"] || len(*asked) != 1+1+2+4 {
		t.Fatalf("asked: %v", *asked)
	}
	var why string
	_ = a.db.Reader.QueryRow(`SELECT error FROM ix_emb_err WHERE key='broken'`).Scan(&why)
	if why != "no frame could be read from the video" {
		t.Fatalf("error: %q", why)
	}
	if ts := a.ixVideoFrameTimes(ctx, "lies"); len(ts) != 1 || ts[0] != 0 {
		t.Fatalf("frame times of the first-frame fallback: %v", ts)
	}
	// Faces and text read the same way.
	if pics, err := a.ixMLFullPicture(ctx, &m.Items[m.ByKey["gif"]]); err != nil || len(pics) != 1 {
		t.Fatalf("faces frames of a short clip: %d %v", len(pics), err)
	}
}

func TestIxVideoFramesAreKeptAndSearched(t *testing.T) {
	pictures := 0
	srv := ixVFFakeML(t, &pictures)
	defer srv.Close()
	a, ctx := ix8App(t, map[string]any{"mlUrl": srv.URL})
	// trip: 30 s, six frames: a beach twice (one kept), then a street, a
	// dog for one frame, a car twice (both kept, they differ enough).
	m := ixVFModel([][3]string{{"trip", "video", "30"}, {"p1", "photo", ""}, {"p2", "photo", ""}})
	ixVFFiles(t, a, m, map[string]string{"p1": "1:1", "p2": "4:1,9:0.4"})
	trip := map[float64]string{2.5: "1:1", 7.5: "1:1", 12.5: "2:1", 17.5: "3:1", 22.5: "5:1", 27.5: "5:1,6:0.5"}
	ixVFGrab(t, map[string]float64{"trip": 30}, func(_ string, at float64) string { return trip[at] })
	c := a.ixMLClientNow(ctx)
	_ = a.ixMLBox().store.load(ctx, a.db.Reader, c.model)
	a.ixMLRound(ctx, c, m, 50)
	if pictures != 8 {
		t.Fatalf("pictures sent: %d", pictures)
	}
	if ts := a.ixVideoFrameTimes(ctx, "trip"); fmt.Sprint(ts) != "[2500 12500 17500 22500 27500]" {
		t.Fatalf("frames kept: %v", ts)
	}
	// The mean is kept for sets and "more like this".
	mean := a.ixMLBox().store.vector("trip")
	if mean == nil {
		t.Fatal("no mean")
	}
	dog := ixVFVec("3:1")
	byMean := ixCos(mean, dog)
	// By its best frame, the dog is found, with where it is.
	hits := a.ixVideoFrameHits(ctx, dog, 5, nil)
	if len(hits) != 1 || hits[0].Key != "trip" || hits[0].AtMs != 17500 || hits[0].Sim < 0.99 || byMean > 0.5 {
		t.Fatalf("frame hits %+v (mean %.2f)", hits, byMean)
	}
	if hits := a.ixVideoFrameHits(ctx, dog, 5, func(string) bool { return false }); len(hits) != 0 {
		t.Fatalf("filtered: %+v", hits)
	}
	// Read back from the database the same.
	ixFrameState.Delete(a)
	hits = a.ixVideoFrameHits(ctx, ixVFVec("5:1,6:0.5"), 5, nil)
	if len(hits) != 1 || hits[0].AtMs != 27500 || a.ixFrameStore().frames != 5 {
		t.Fatalf("after loading: %+v (%d frames)", hits, a.ixFrameStore().frames)
	}
	// The viewer is told where the frames are.
	rec := httptest.NewRecorder()
	a.handleIxExtra(rec, httptest.NewRequest("GET", "/api/ix/browse/extra?key=trip", nil))
	var extra struct{ Frames []int64 }
	_ = json.Unmarshal(rec.Body.Bytes(), &extra)
	if len(extra.Frames) != 5 || extra.Frames[2] != 17500 {
		t.Fatalf("extra: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	a.handleIxExtra(rec, httptest.NewRequest("GET", "/api/ix/browse/extra?key=p1", nil))
	if !strings.Contains(rec.Body.String(), `"frames":[]`) {
		t.Fatalf("extra of a photo: %s", rec.Body.String())
	}
	// Doing the video again replaces its frames, in memory too.
	trip[17.5] = "2:1"
	_, _ = a.db.Writer.Exec(`DELETE FROM ix_emb WHERE key='trip'`)
	a.ixMLBox().store = ixEmbStore{index: map[string]int32{}}
	_ = a.ixMLBox().store.load(ctx, a.db.Reader, c.model)
	a.ixMLRound(ctx, c, m, 50)
	if hits := a.ixVideoFrameHits(ctx, dog, 5, nil); len(hits) == 1 && hits[0].Sim > 0.5 {
		t.Fatalf("old frame still found: %+v", hits)
	}
	if fs := a.ixFrameStore(); fs.frames != 4 || len(fs.videos) != 1 {
		t.Fatalf("frames in memory: %d in %d videos", fs.frames, len(fs.videos))
	}
}

func TestIxOlderVideosAreDoneAgainLast(t *testing.T) {
	pictures := 0
	srv := ixVFFakeML(t, &pictures)
	defer srv.Close()
	a, ctx := ix8App(t, map[string]any{"mlUrl": srv.URL})
	m := ixVFModel([][3]string{{"old", "video", "20"}, {"p1", "photo", ""}, {"p2", "photo", ""}, {"p3", "photo", ""}})
	ixVFFiles(t, a, m, map[string]string{"p1": "1:1", "p2": "2:1", "p3": "3:1"})
	ixVFGrab(t, map[string]float64{"old": 20}, func(_ string, at float64) string { return fmt.Sprintf("%d:1", 10+int(at)) })
	c := a.ixMLClientNow(ctx)
	// "old" was looked at before frames were kept: a mean only.
	if err := a.ixMLSave(ctx, c.model, "old", ixVFVec("7:1")); err != nil {
		t.Fatal(err)
	}
	box := a.ixMLBox()
	_ = box.store.load(ctx, a.db.Reader, c.model)
	clip := a.ixClipTask(c)
	clip.per, clip.par = 1, 1 // pictures still to do after this round
	frames := a.ixVideoFramesTask(ctx, c)
	a.ixMLRoundAll(ctx, []*ixMLTask{clip, frames}, m)
	if !box.store.has("p1") || box.store.has("p2") || len(a.ixVideoFrameTimes(ctx, "old")) != 0 {
		t.Fatal("older videos went before new pictures")
	}
	a.ixMLCount(ctx, m)
	if st := a.ixMLStatusNow(ctx).Tasks["frames"]; !st.On || st.Pending != 1 || st.Done != 0 {
		t.Fatalf("status before: %+v", st)
	}
	clip.per = 0
	a.ixMLRoundAll(ctx, []*ixMLTask{clip, frames}, m)
	a.ixMLRoundAll(ctx, []*ixMLTask{clip, frames}, m)
	if ts := a.ixVideoFrameTimes(ctx, "old"); len(ts) != 4 {
		t.Fatalf("older video not done again: %v", ts)
	}
	if sim := ixCos(box.store.vector("old"), ixVFVec("7:1")); sim > 0.5 {
		t.Fatalf("mean not replaced: %.2f", sim)
	}
	a.ixMLCount(ctx, m)
	if st := a.ixMLStatusNow(ctx).Tasks["frames"]; st.Pending != 0 || st.Done != 1 {
		t.Fatalf("status after: %+v", st)
	}
	// The real task list ends with it.
	if ts := a.ixMLTasks(ctx, c, a.ixLoadSettings(ctx)); ts[len(ts)-1].name != "frames" || !ts[len(ts)-1].idle {
		t.Fatal("frames task not last")
	}
}

func TestIxVideoLookAlikes(t *testing.T) {
	pictures := 0
	srv := ixVFFakeML(t, &pictures)
	defer srv.Close()
	a, ctx := ix8App(t, map[string]any{"mlUrl": srv.URL, "mlDupes": true})
	// v1 and v2: the same footage, one with a logo; v3 something else; p1
	// a photo of the same scene (never paired with a video).
	m := ixVFModel([][3]string{{"v1", "video", "12"}, {"v2", "video", "12"}, {"v3", "video", "12"}, {"p1", "photo", ""}})
	ixVFFiles(t, a, m, map[string]string{"p1": "1:1"})
	a.ixModelBox().model = m
	ixVFGrab(t, map[string]float64{"v1": 12, "v2": 12, "v3": 12}, func(file string, at float64) string {
		switch file {
		case "v1":
			return fmt.Sprintf("1:1,%d:0.2", 2+int(at))
		case "v2":
			return fmt.Sprintf("1:1,%d:0.2,20:0.05", 2+int(at))
		}
		return fmt.Sprintf("9:1,%d:0.5", 2+int(at))
	})
	c := a.ixMLClientNow(ctx)
	_ = a.ixMLBox().store.load(ctx, a.db.Reader, c.model)
	a.ixMLRound(ctx, c, m, 50)
	rows, _ := a.db.Reader.Query(`SELECT key_a||'-'||key_b||':'||kind FROM ix_msug ORDER BY 1`)
	var pairs []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		pairs = append(pairs, s)
	}
	rows.Close()
	if fmt.Sprint(pairs) != "[v1-v2:look]" {
		t.Fatalf("look-alike pairs: %v", pairs)
	}
	// The review shows them, as videos.
	rec := httptest.NewRecorder()
	a.handleIxBrowseReview(rec, httptest.NewRequest("GET", "/api/ix/browse/review", nil))
	if !strings.Contains(rec.Body.String(), `"kind":"look"`) || strings.Count(rec.Body.String(), `"type":"video"`) != 2 {
		t.Fatalf("review: %s", rec.Body.String())
	}
}

// TestIxVideoFrameHitsScale: 50,000 frames (5,000 videos × 10) of 768
// numbers, the size of SigLIP2 ViT-B embeddings.
func TestIxVideoFrameHitsScale(t *testing.T) {
	const videos, per, dim = 5000, 10, 768
	rng := rand.New(rand.NewSource(1))
	unit := func() []float32 {
		v := make([]float32, dim)
		for i := range v {
			v[i] = float32(rng.NormFloat64())
		}
		ixNormalise(v)
		return v
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fs := &ixFrameStore{index: map[string]int32{}, loaded: true}
	target := unit()
	for vi := 0; vi < videos; vi++ {
		vf := ixVideoFrames{key: fmt.Sprintf("h:%05d", vi)}
		for f := 0; f < per; f++ {
			v := unit()
			if vi == 1234 && f == 7 {
				v = target
			}
			q, n := ixQuantise(v)
			vf.at = append(vf.at, int32(f*5000))
			vf.vecs = append(vf.vecs, q...)
			vf.norms = append(vf.norms, n)
		}
		fs.putLocked(vf)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	t.Logf("%d frames: %.1f MB in memory", fs.frames, float64(after.HeapAlloc-before.HeapAlloc)/1e6)
	keep := func(k string) bool { return k != "h:00001" }
	_ = fs.best(target, 50, keep) // warm up
	const runs = 5
	start := time.Now()
	var hits []ixFrameHit
	for i := 0; i < runs; i++ {
		hits = fs.best(target, 50, keep)
	}
	per1 := time.Since(start) / runs
	t.Logf("one search: %v", per1)
	if len(hits) != 50 || hits[0].Key != "h:01234" || hits[0].AtMs != 35000 || hits[0].Sim < 0.99 {
		t.Fatalf("best: %+v", hits[0])
	}
	if !ixRaceOn && per1 > 100*time.Millisecond {
		t.Fatalf("too slow: %v", per1)
	}
}
