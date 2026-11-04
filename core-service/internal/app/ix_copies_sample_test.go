package app

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/seekbar"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

func TestIxRangesAreServedFromAlignedParts(t *testing.T) {
	data := make([]byte, 5*telegram.PartSize+1234)
	for i := range data {
		data[i] = byte(i * 7)
	}
	var offsets []int64
	f := &ixRangeFile{size: int64(len(data)), mime: "video/mp4", get: func(off int64) ([]byte, error) {
		if off%telegram.PartSize != 0 {
			t.Fatalf("unaligned part %d", off)
		}
		offsets = append(offsets, off)
		return data[off:min(off+telegram.PartSize, int64(len(data)))], nil
	}}
	address, stop, err := ixServeRemote(f)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	get := func(path, rng string) (int, []byte) {
		req, _ := http.NewRequest(http.MethodGet, path, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	// A range across a part boundary is answered to the end of its first
	// part; asked again from there, the next part follows. Each part is
	// read once.
	start := int64(telegram.PartSize - 10)
	code, b := get(address, "bytes="+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(start+19, 10))
	if code != http.StatusPartialContent || string(b) != string(data[start:start+10]) {
		t.Fatalf("range: %d, %d bytes", code, len(b))
	}
	code, b = get(address, "bytes="+strconv.FormatInt(start+10, 10)+"-"+strconv.FormatInt(start+19, 10))
	if code != http.StatusPartialContent || string(b) != string(data[start+10:start+20]) {
		t.Fatalf("next range: %d, %d bytes", code, len(b))
	}
	code, b = get(address, "bytes=-100")
	if code != http.StatusPartialContent || string(b) != string(data[len(data)-100:]) {
		t.Fatal("suffix range")
	}
	code, _ = get(address, "bytes="+strconv.FormatInt(start, 10)+"-")
	if code != http.StatusPartialContent {
		t.Fatal("open range")
	}
	if code, _ = get(strings.TrimSuffix(address, f.token)+"other", ""); code != http.StatusNotFound {
		t.Fatal("served without the file's token")
	}
	seen := map[int64]int{}
	for _, o := range offsets {
		seen[o]++
		if seen[o] > 1 {
			t.Fatalf("part %d read twice", o)
		}
	}
	// The cap stops a check that would read too much.
	saved := ixSampleMaxParts
	ixSampleMaxParts = len(offsets)
	defer func() { ixSampleMaxParts = saved }()
	if _, err := f.part(0); err != nil {
		t.Fatal("a kept part was refused")
	}
	if _, err := f.part(99); err != errIxSampleCap && len(offsets) < 6 {
		t.Fatalf("over the cap: %v", err)
	}
}

func ixTestFFmpeg(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command(seekbar.FFmpegPath(), append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
}

// Big videos are checked by frames sampled across the file, read through
// a fake part fetcher: a trimmed, resized re-encode is confirmed, another
// video is not, and only a fraction of the parts is read.
func TestIxBigCopiesAreConfirmedBySampling(t *testing.T) {
	if seekbar.FFmpegPath() == "" {
		t.Skip("ffmpeg is not installed")
	}
	a, ctx := ixTestApp(t)
	dir := t.TempDir()
	saved, trimmed, other := filepath.Join(dir, "saved.mp4"), filepath.Join(dir, "trimmed.mp4"), filepath.Join(dir, "other.mp4")
	ixTestFFmpeg(t, "-f", "lavfi", "-i", "mandelbrot=size=480x360:rate=10:maxiter=256", "-t", "90", "-c:v", "libx264", "-preset", "ultrafast", "-g", "20", "-qp", "0", "-pix_fmt", "yuv420p", saved)
	// Cut the first 7 seconds, smaller, re-encoded, index at the front.
	ixTestFFmpeg(t, "-ss", "7", "-i", saved, "-vf", "scale=320:240", "-c:v", "libx264", "-preset", "ultrafast", "-g", "25", "-qp", "0", "-pix_fmt", "yuv420p", "-movflags", "+faststart", trimmed)
	ixTestFFmpeg(t, "-f", "lavfi", "-i", "life=size=480x360:rate=10:mold=10:ratio=0.3", "-t", "83", "-c:v", "libx264", "-preset", "ultrafast", "-g", "20", "-qp", "0", "-pix_fmt", "yuv420p", other)
	dur, frames, err := ixFingerprintVideo(ctx, saved, "")
	if err != nil {
		t.Fatal(err)
	}
	key := "h:" + ixSixtyFour("5")
	if err := a.ixVdupSave(ctx, key, "c/saved.mp4", dur, frames, ""); err != nil {
		t.Fatal(err)
	}
	r := &ixFakeReader{files: map[int64][]byte{}}
	check := func(id int64, path string, sec float64) (ixSampleResult, int) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		r.files[id] = data
		msg := ixTestVideo(int(id), id, int64(len(data)), sec, 320, 240, nil, 1700000000)
		f, _ := ixCopyFactsOf(msg)
		return a.ixSampleCheck(ctx, r, msg, f, ""), (len(data) + telegram.PartSize - 1) / telegram.PartSize
	}
	res, total := check(601, trimmed, 83)
	if res.Result != "confirmed" || res.Of != key {
		t.Fatalf("trimmed copy: %+v", res)
	}
	if res.Offset < 4 || res.Offset > 10 {
		t.Fatalf("offset %.1f s for a copy missing its first 7 s", res.Offset)
	}
	if res.Parts != r.parts[601] || res.Parts*3 > total || res.Parts > ixSampleMaxParts {
		t.Fatalf("read %d of %d parts", res.Parts, total)
	}
	res, total = check(602, other, 83)
	if res.Result != "nomatch" || res.Parts*2 > total {
		t.Fatalf("another video: %+v (of %d parts)", res, total)
	}
	t.Logf("trimmed copy confirmed from %d of %d parts; another video read %d of %d", r.parts[601], (len(r.files[601])+telegram.PartSize-1)/telegram.PartSize, r.parts[602], total)
}

// The round takes the biggest queued videos first, records what each
// check cost, holds a big copy nothing else caught as confirmed, and
// confirms a held one; downloads under way are left alone.
func TestIxSamplingRoundTakesTheBiggestFirst(t *testing.T) {
	a, ctx := ixTestApp(t)
	savedFake, frameFake := ixSampleFake, ixSampleFrame
	ixSampleFake = true
	defer func() { ixSampleFake, ixSampleFrame = savedFake, frameFake }()
	// Invented fingerprints: the saved video's frame at second s has hash
	// s * a large odd number; a copy's frames are the same moments.
	hashAt := func(s int) uint64 { return uint64(s+1) * 0x9E3779B97F4A7C15 }
	var fp []ixVFrame
	for s := 0; s < 3000; s++ {
		fp = append(fp, ixVFrame{T: float32(s), H: hashAt(s), HF: ^hashAt(s)})
	}
	key := "h:" + ixSixtyFour("6")
	if err := a.ixVdupSave(ctx, key, "c/long.mp4", 3000, fp, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_type,file_path,status,file_hash) VALUES(?,90,'long.mp4','video','c/long.mp4','completed',?)`, ixTestGroup, ixSixtyFour("6")); err != nil {
		t.Fatal(err)
	}
	copyDoc := map[string]bool{}
	ixSampleFrame = func(ctx context.Context, address string, at float64) (ixSampledFrame, bool, error) {
		resp, err := http.Get(address) // reads the start of the file, as ffmpeg would
		if err != nil {
			return ixSampledFrame{}, false, err
		}
		b := make([]byte, 16)
		_, _ = io.ReadFull(resp.Body, b)
		resp.Body.Close()
		if copyDoc[string(b)] {
			return ixSampledFrame{T: at, H: hashAt(int(at)), HF: ^hashAt(int(at))}, true, nil
		}
		return ixSampledFrame{T: at, H: uint64(at*1000) ^ 0x5555, HF: 1}, true, nil
	}
	r := &ixFakeReader{files: map[int64][]byte{}}
	ixTestUseReader(t, r)
	queue := func(doc int64, size int64, mark string) int64 {
		data := make([]byte, 2*telegram.PartSize)
		copy(data, mark)
		r.files[doc] = data
		return ixTestQueue(t, a, ixTestVideo(int(doc), doc, size, 3000, 1280, 720, nil, 1700000000))
	}
	copyDoc["COPY-COPY-COPY-1"] = true
	small := queue(701, 300<<20, "small-small-smal")
	big := queue(702, 4<<30, "COPY-COPY-COPY-1")
	mid := queue(703, 1<<30, "other-other-othe")
	busy := queue(704, 8<<30, "COPY-COPY-COPY-1")
	tiny := queue(705, 150<<20, "COPY-COPY-COPY-1") // under 200 MB: not sampled
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='processing' WHERE id=?`, busy)
	// mid is held already as a probable copy of the saved video.
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET paused=1 WHERE id=?`, mid)
	_, _ = a.db.Writer.Exec(`INSERT INTO ix_held(work_id,group_id,message_id,like_path,like_key,reason,held_at) VALUES(?,?,703,'c/long.mp4',?,'looks like a saved copy',1)`, mid, ixTestGroup, key)
	_, _ = a.db.Writer.Exec(`INSERT INTO ix_copy_hold(work_id,how) VALUES(?,'saved')`, mid)
	ixSamplesPerRun = 2
	defer func() { ixSamplesPerRun = 3 }()
	if err := a.ixSampleRound(ctx, false); err != nil {
		t.Fatal(err)
	}
	var order []int64
	rows, _ := a.db.Reader.Query(`SELECT work_id FROM ix_copy_sample ORDER BY at, work_id`)
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		order = append(order, id)
	}
	rows.Close()
	if len(order) != 2 || order[0] == small || order[1] == small {
		t.Fatalf("checked first: %v (big %d, mid %d)", order, big, mid)
	}
	if how, _ := ixTestHow(a, big); how != "sampled" || !ixTestPaused(t, a, big) {
		t.Fatalf("the 4 GB copy: %q", how)
	}
	var confirm string
	_ = a.db.Reader.QueryRow(`SELECT confirm FROM ix_copy_hold WHERE work_id=?`, big).Scan(&confirm)
	if confirm != "confirmed" {
		t.Fatalf("confirm %q", confirm)
	}
	_ = a.db.Reader.QueryRow(`SELECT confirm FROM ix_copy_hold WHERE work_id=?`, mid).Scan(&confirm)
	if confirm != "unconfirmed" || !ixTestPaused(t, a, mid) {
		t.Fatalf("held, not confirmed: %q", confirm)
	}
	if err := a.ixSampleRound(ctx, false); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_copy_sample WHERE work_id IN (?,?)`, busy, tiny).Scan(&n)
	if n != 0 {
		t.Fatal("a download under way, or a small video, was checked")
	}
	if ixTestPaused(t, a, busy) {
		t.Fatal("a download under way was paused")
	}
	tot := a.ixSampleTotalsNow(ctx)
	if tot.Checked != 3 || tot.Confirmed != 1 || tot.PartsMB < 3 || tot.SizeBytes != 4<<30+1<<30+300<<20 {
		t.Fatalf("totals: %+v", tot)
	}
	if lines := a.ixCopiesDiag(ctx); !strings.Contains(strings.Join(lines, "\n"), "checked 3 big videos using") {
		t.Fatalf("diagnostics: %q", lines)
	}
	_ = tg.Message{}
}
