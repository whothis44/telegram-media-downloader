package app

import (
	"context"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func ixPattern(seed int, noise int) []byte {
	px := make([]byte, 1024)
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			v := ((x*(seed+3) + y*(seed*7+1)) * 37 / 5) % 256
			if (x/(4+seed%5)+y/(3+seed%4))%2 == 0 {
				v = 255 - v
			}
			if noise > 0 {
				v += ((x*13 + y*7 + seed) % (2*noise + 1)) - noise
			}
			px[y*32+x] = byte(min(255, max(0, v)))
		}
	}
	return px
}

func TestIxPerceptualHashAndMatching(t *testing.T) {
	h1, ok1 := ixPHash(ixPattern(1, 0))
	h1n, ok2 := ixPHash(ixPattern(1, 6))
	h2, ok3 := ixPHash(ixPattern(2, 0))
	if !ok1 || !ok2 || !ok3 {
		t.Fatal("patterns are not flat")
	}
	if d := bits.OnesCount64(h1 ^ h1n); d > ixVdupHamming {
		t.Fatalf("a slightly changed frame differs by %d bits", d)
	}
	if d := bits.OnesCount64(h1 ^ h2); d <= ixVdupHamming {
		t.Fatalf("different frames differ by only %d bits", d)
	}
	flat := make([]byte, 1024)
	for i := range flat {
		flat[i] = 40
	}
	if _, ok := ixPHash(flat); ok {
		t.Fatal("a flat frame must be ignored")
	}
	hashes := make([]uint64, 8)
	for i := range hashes {
		hashes[i], _ = ixPHash(ixPattern(i+10, 0))
	}
	seq := func(ts []float32, idx []int) []ixVFrame {
		out := []ixVFrame{}
		for i, n := range idx {
			out = append(out, ixVFrame{T: ts[i], H: hashes[n]})
		}
		return out
	}
	a := seq([]float32{0, 4, 8, 12, 16, 20}, []int{0, 1, 2, 3, 4, 5})
	reencoded := seq([]float32{0.4, 4.4, 8.4, 12.4, 20.4}, []int{0, 1, 2, 3, 5}) // one scene lost
	clip := seq([]float32{0, 4, 8}, []int{1, 2, 3})                              // starts 4 s in
	other := seq([]float32{0, 3, 6}, []int{6, 7, 6})
	shuffled := seq([]float32{0, 4, 8, 12}, []int{3, 0, 5, 1}) // same shots, different order
	if k, m := ixVKind(a, reencoded, 24, 24); k != "same" || m.Matched != 5 {
		t.Fatalf("re-encode: %q %+v", k, m)
	}
	if k, m := ixVKind(a, clip, 24, 12); k != "clip" || m.Offset != 4 {
		t.Fatalf("clip: %q %+v", k, m)
	}
	if k, _ := ixVKind(a, other, 24, 9); k != "" {
		t.Fatalf("unrelated: %q", k)
	}
	if k, m := ixVKind(a, shuffled, 24, 16); k != "" {
		t.Fatalf("shots in another order are not the same footage: %q %+v", k, m)
	}
	packed := ixUnpackFrames(ixPackFrames(a))
	if len(packed) != len(a) || packed[3] != a[3] {
		t.Fatal("pack")
	}
	roots := ixNearRoots(map[string][]ixNear{"h:a": {{"h:b", "same"}, {"h:c", "clip"}}, "h:b": {{"h:a", "same"}}, "h:c": {{"h:a", "clip"}}})
	if roots["h:a"] != roots["h:b"] || roots["h:c"] == roots["h:a"] {
		t.Fatalf("roots: %v", roots)
	}
}

func TestIxNearDuplicatesFoundIndexedAndListed(t *testing.T) {
	ixVdupBackground.Store(false)
	defer ixVdupBackground.Store(true)
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	hashes := make([]uint64, 10)
	for i := range hashes {
		hashes[i], _ = ixPHash(ixPattern(i+20, 0))
	}
	frames := func(start float32, idx ...int) []ixVFrame {
		var out []ixVFrame
		for i, n := range idx {
			out = append(out, ixVFrame{T: start + float32(i*4), H: hashes[n]})
		}
		return out
	}
	fake := map[string]struct {
		dur    float64
		frames []ixVFrame
	}{
		"a.mp4": {24, frames(0, 0, 1, 2, 3, 4, 5)},
		"b.mp4": {24, frames(0.3, 0, 1, 2, 3, 4, 5)},
		"c.mp4": {12, frames(0, 2, 3, 4)},
		"d.mp4": {12, frames(0, 7, 8, 9)},
	}
	saved, savedHave := ixFingerprintVideo, ixHaveFFmpeg
	defer func() { ixFingerprintVideo, ixHaveFFmpeg = saved, savedHave }()
	ixHaveFFmpeg = func() bool { return true }
	ixFingerprintVideo = func(_ context.Context, path, _ string) (float64, []ixVFrame, error) {
		f := fake[filepath.Base(path)]
		return f.dur, f.frames, nil
	}
	cfg, _ := a.config.Load(ctx)
	cfg["groups"] = []any{
		map[string]any{"id": "-1000000000201", "name": "clips-1", "enabled": true, "category": "Clips"},
		map[string]any{"id": "-1000000000202", "name": "clips-2", "enabled": true, "category": "Clips"},
	}
	if err = a.config.Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	for i, f := range []struct {
		group, name string
		size        int
	}{{"-1000000000201", "a.mp4", 5000}, {"-1000000000202", "b.mp4", 9000}, {"-1000000000202", "c.mp4", 2000}, {"-1000000000201", "d.mp4", 3000}} {
		rel := "videos/" + f.name
		if err = os.MkdirAll(filepath.Join(a.downloadsDir, "videos"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(a.downloadsDir, "videos", f.name), []byte("v"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err = a.db.Writer.Exec(`INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,status,file_hash) VALUES(?,?,?,?,?,'video',?,'completed',?)`,
			f.group, "", i+1, f.name, f.size, rel, strings.Repeat(string(rune('a'+i)), 64)); err != nil {
			t.Fatal(err)
		}
	}
	for n := 0; n < 10; n++ {
		worked, err := a.ixVdupStep(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	pairs := map[string]string{}
	rows, err := a.db.Reader.Query(`SELECT key_a, key_b, kind FROM ix_vdup`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var ka, kb, kind string
		rows.Scan(&ka, &kb, &kind)
		pairs[ka[2:3]+kb[2:3]] = kind
	}
	rows.Close()
	if pairs["ab"] != "same" || pairs["ac"] != "clip" || pairs["bc"] != "clip" || len(pairs) != 3 {
		t.Fatalf("pairs: %v", pairs)
	}
	if st := a.ixVdupStatus(); st.Done != 4 || st.Total != 4 || st.Pairs != 3 {
		t.Fatalf("status: %+v", st)
	}
	clusters, err := a.ixDupClusters(ctx)
	if err != nil || len(clusters) != 1 || len(clusters[0].Members) != 3 || clusters[0].Kind != "mixed" || clusters[0].Spare != 7000 || clusters[0].Members[0].Size != 9000 {
		t.Fatalf("clusters: %+v %v", clusters, err)
	}
	d, err := a.ixSite(ctx, ixSite{}, true)
	if err != nil {
		t.Fatal(err)
	}
	cat, _, err := a.ixViewFile(ctx, d, "c_clips/index.html")
	if err != nil || !strings.Contains(string(cat), "3 unique files") || !strings.Contains(string(cat), "1 near-duplicate video file(s) shown with their best version") {
		t.Fatalf("category: %v %s", err, cat)
	}
	var media string
	for name, body := range d.render.renderCategory(d.cats[0]) {
		if strings.HasPrefix(name, "media-") {
			media += string(body)
		}
	}
	if !strings.Contains(media, "2 versions of this video") {
		t.Fatalf("tiles: %s", media)
	}
	g := d.byID["-1000000000201"]
	page, _, err := a.ixViewFile(ctx, d, g.Dir+"/"+g.Pages[0].File)
	if err != nil || !strings.Contains(string(page), "near-duplicate") || !strings.Contains(string(page), "clips-2") {
		t.Fatalf("group page: %v %s", err, page)
	}
	csv, _, err := a.ixViewFile(ctx, d, "near_duplicates.csv")
	if err != nil || strings.Count(string(csv), "\r\n") != 4 {
		t.Fatalf("csv: %v %q", err, csv)
	}
}

// With the real ffmpeg (CI installs it): the same footage re-encoded at a
// lower size and quality, and a clip of it, are found; other footage is not.
func TestIxRealVideosNearDuplicates(t *testing.T) {
	if !ixHaveFFmpeg() {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("ffmpeg", append([]string{"-v", "error", "-y"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v %s", args, err, out)
		}
	}
	run("-f", "lavfi", "-i", "smptebars=s=320x180:d=4:r=25", "-f", "lavfi", "-i", "testsrc=s=320x180:d=4:r=25", "-f", "lavfi", "-i", "rgbtestsrc=s=320x180:d=4:r=25",
		"-f", "lavfi", "-i", "mandelbrot=s=320x180:r=25,trim=duration=4", "-f", "lavfi", "-i", "cellauto=s=320x180:r=25,trim=duration=4", "-f", "lavfi", "-i", "testsrc2=s=320x180:d=4:r=25",
		"-filter_complex", "[0][1][2][3][4][5]concat=n=6:v=1[v]", "-map", "[v]", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-crf", "23", "a.mp4")
	run("-i", "a.mp4", "-vf", "scale=212:120", "-c:v", "libx264", "-crf", "32", "-g", "120", "b.mp4")
	run("-ss", "4", "-i", "a.mp4", "-t", "10", "-vf", "scale=240:136", "-c:v", "libx264", "-crf", "28", "c.mp4")
	run("-f", "lavfi", "-i", "life=s=320x180:r=25:mold=10:ratio=0.1,trim=duration=8", "-f", "lavfi", "-i", "yuvtestsrc=s=320x180:d=8:r=25",
		"-filter_complex", "[0][1]concat=n=2:v=1[v]", "-map", "[v]", "-c:v", "libx264", "-pix_fmt", "yuv420p", "d.mp4")
	fp := map[string][]ixVFrame{}
	dur := map[string]float64{}
	for _, n := range []string{"a", "b", "c", "d"} {
		d, f, err := ixFFmpegFingerprint(context.Background(), filepath.Join(dir, n+".mp4"), "")
		if err != nil {
			t.Fatal(err)
		}
		fp[n], dur[n] = f, d
	}
	for _, c := range []struct{ x, y, want string }{{"a", "b", "same"}, {"a", "c", "clip"}, {"a", "d", ""}, {"c", "d", ""}} {
		if k, m := ixVKind(fp[c.x], fp[c.y], dur[c.x], dur[c.y]); k != c.want {
			t.Errorf("%s/%s: %q want %q (%+v, %d/%d frames)", c.x, c.y, k, c.want, m, len(fp[c.x]), len(fp[c.y]))
		}
	}
}
