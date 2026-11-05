package app

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/seekbar"
	"github.com/gotd/td/tg"
)

// Sampling end to end, with real ffmpeg reading through the little range server
// as it does in production. The videos are generated here: invented, and small.
func TestIxSamplingWithRealFFmpeg(t *testing.T) {
	bin := seekbar.FFmpegPath()
	if bin == "" {
		t.Skip("no ffmpeg")
	}
	a, ctx := ixTestApp(t)
	dir := t.TempDir()
	const seconds = 120
	make := func(name, source string, args ...string) (string, []byte) {
		t.Helper()
		path := filepath.Join(dir, name)
		full := append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-f", "lavfi", "-i", source, "-t", "120", "-an", "-pix_fmt", "yuv420p", "-c:v", "libx264", "-preset", "ultrafast", "-movflags", "+faststart"}, args...)
		if out, err := exec.Command(bin, append(full, path)...).CombinedOutput(); err != nil {
			t.Skipf("this ffmpeg cannot make the test video: %v: %s", err, out)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return path, data
	}
	// Shots of 5 s, as footage has (a picture changing completely on every frame
	// does not match its own fingerprint at the 8 bits allowed: it reads as "no
	// match" and is downloaded, the safe way to be wrong). The copy is the same
	// picture, smaller and worse, as re-uploads are.
	original, _ := make("original.mp4", "testsrc2=size=640x360:rate=0.2,fps=15", "-g", "30", "-crf", "24")
	copyPath := filepath.Join(dir, "copy.mp4")
	if out, err := exec.Command(bin, "-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", original, "-an", "-vf", "scale=480:270", "-c:v", "libx264", "-preset", "ultrafast", "-crf", "32", "-g", "45", "-movflags", "+faststart", copyPath).CombinedOutput(); err != nil {
		t.Skipf("this ffmpeg cannot make the copy: %v: %s", err, out)
	}
	copied, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	_, other := make("other.mp4", "mandelbrot=size=640x360:rate=0.2,fps=15", "-g", "30", "-crf", "24")

	dur, frames, err := ixFFmpegFingerprint(ctx, original, "")
	if err != nil {
		t.Fatalf("fingerprint of the saved video: %v", err)
	}
	if len(frames) == 0 || dur < seconds-2 {
		t.Fatalf("fingerprint: %d frames, %.1f s", len(frames), dur)
	}
	key := "h:" + ixSixtyFour("a")
	if err := a.ixVdupSave(ctx, key, "c/original.mp4", dur, frames, ""); err != nil {
		t.Fatal(err)
	}

	fake := &ixFakeReader{files: map[int64][]byte{}}
	// Telegram sends a part only now and then, as on a speed-limited account.
	r := &ixThrottledReader{ixFakeReader: fake, delay: 30 * time.Millisecond, flood: map[int64]bool{}}
	check := func(doc int64, data []byte, w, h int) ixSampleResult {
		fake.files[doc] = data
		var msg *tg.Message = ixTestVideo(int(doc), doc, int64(len(data)), seconds, w, h, nil, 1700000000)
		f, ok := ixCopyFactsOf(msg)
		if !ok {
			t.Fatal("facts of the invented video")
		}
		return a.ixSampleCheck(ctx, r, msg, f, "")
	}
	if res := check(1, copied, 480, 270); res.Result != "confirmed" || res.Of != key {
		t.Fatalf("a re-encoded copy was not recognised: %+v", res)
	} else {
		t.Logf("copy of %d bytes: confirmed from %d parts, %d frames", len(copied), res.Parts, res.Samples)
	}
	if res := check(2, other, 640, 360); res.Result != "nomatch" {
		t.Fatalf("another video of the same length was taken for a copy: %+v", res)
	} else {
		t.Logf("other video: no match from %d parts, %d frames, best %d", res.Parts, res.Samples, res.Best)
	}
}

// On a speed-limited account "busy" is true for days. The check must still run
// in its turn, and stop when the turn is over, with nothing recorded as failed.
func TestIxSamplingRoundRunsInItsTurnUnderASpeedLimit(t *testing.T) {
	a, ctx := ixTestApp(t)
	clock := time.Unix(1_800_000_000, 0)
	oldBusy, oldNow, oldOn, oldFlood, oldFake, oldFrame := ixRemoteChecksBusy, ixRemoteBudgetNow, ixRemoteBudgetOn, ixFloodedLately, ixSampleFake, ixSampleFrame
	t.Cleanup(func() {
		ixRemoteChecksBusy, ixRemoteBudgetNow, ixRemoteBudgetOn, ixFloodedLately, ixSampleFake, ixSampleFrame = oldBusy, oldNow, oldOn, oldFlood, oldFake, oldFrame
	})
	ixRemoteBudgetOn, ixSampleFake = true, true
	ixRemoteChecksBusy = func(*App) bool { return true }
	ixFloodedLately = func(*App) bool { return false }
	ixRemoteBudgetNow = func() time.Time { return clock }
	var fp []ixVFrame
	hashAt := func(s int) uint64 { return uint64(s+1) * 0x9E3779B97F4A7C15 }
	for s := 0; s < 600; s++ {
		fp = append(fp, ixVFrame{T: float32(s), H: hashAt(s), HF: ^hashAt(s)})
	}
	if err := a.ixVdupSave(ctx, "h:"+ixSixtyFour("b"), "c/v.mp4", 600, fp, ""); err != nil {
		t.Fatal(err)
	}
	ixSampleFrame = func(ctx context.Context, address string, at float64) (ixSampledFrame, bool, error) {
		resp, err := http.Get(address)
		if err != nil {
			return ixSampledFrame{}, false, err
		}
		resp.Body.Close()
		return ixSampledFrame{T: at, H: hashAt(int(at)), HF: ^hashAt(int(at))}, true, nil
	}
	reader := &ixFakeReader{files: map[int64][]byte{981: make([]byte, 1<<20), 982: make([]byte, 1<<20)}}
	ixTestUseReader(t, reader)
	first := ixTestQueue(t, a, ixTestVideo(981, 981, 300<<20, 600, 1280, 720, nil, 1700000000))
	second := ixTestQueue(t, a, ixTestVideo(982, 982, 300<<20, 600, 1280, 720, nil, 1700000000))
	checked := func(id int64) int {
		var n int
		if err := a.db.Reader.QueryRow(`SELECT count(*) FROM ix_copy_sample WHERE work_id=? AND result<>'failed'`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	clock = clock.Add(ixRemoteBudgetSample + time.Second) // the turn others may have started has no effect yet
	if err := a.ixSampleRound(ctx, false); err != nil {
		t.Fatal(err)
	}
	if checked(first)+checked(second) == 0 {
		t.Fatal("no check ran in its turn, under a speed limit")
	}
	// After the turn: nothing more runs and nothing is recorded as failed.
	if _, err := a.db.Writer.Exec(`DELETE FROM ix_copy_sample`); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(ixRemoteBudgetSample + time.Second)
	if err := a.ixSampleRound(ctx, false); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := a.db.Reader.QueryRow(`SELECT count(*) FROM ix_copy_sample`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d checks ran after the turn ended", rows)
	}
	// In the next window it gets another turn.
	clock = clock.Add(ixRemoteBudgetWindow)
	if err := a.ixSampleRound(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Reader.QueryRow(`SELECT count(*) FROM ix_copy_sample`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Fatal("no turn in the next window")
	}
}
