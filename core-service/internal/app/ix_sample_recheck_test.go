package app

import (
	"context"
	"strings"
	"testing"
)

// Kept sampled frames compared again with newly fingerprinted saved videos
// (ix_sample_recheck.go), with invented frames: nothing reaches Telegram.
func TestIxSampleKeptFramesAreComparedAgain(t *testing.T) {
	a, ctx := ixTestApp(t)
	s := a.ixLoadSettings(ctx)
	s.HoldLikely = true
	if err := a.ixSaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	const mb = int64(1) << 20
	// The saved original, fingerprinted later than the check.
	orig := ixTestVideo(1, 801, 300*mb, 600, 1280, 720, nil, 100)
	ixTestSave(t, a, orig, "c/orig.mp4", ixSixtyFour("d"))
	queued := ixTestVideo(2, 802, 280*mb, 590, 1280, 720, nil, 200)
	other := ixTestVideo(3, 803, 290*mb, 590, 1280, 720, nil, 300)
	qid, oid := ixTestQueue(t, a, queued), ixTestQueue(t, a, other)

	// Eight frames sampled from the queued video: hashes far apart, and the
	// original holds the same frames 10 s later.
	var samples []ixSampledFrame
	var origFrames []ixVFrame
	for i := 0; i < 8; i++ {
		h := uint64(0x9e3779b97f4a7c15) * uint64(i+1)
		tm := 30 + float64(i)*70
		samples = append(samples, ixSampledFrame{T: tm, H: h, HF: ^h})
		origFrames = append(origFrames, ixVFrame{T: float32(tm + 10), H: h ^ 0x7, HF: ^h}) // 3 bits differ
	}
	var otherSamples []ixSampledFrame
	for i := 0; i < 8; i++ {
		h := uint64(0xc2b2ae3d27d4eb4f) * uint64(i+11)
		otherSamples = append(otherSamples, ixSampledFrame{T: float64(i) * 70, H: h, HF: ^h})
	}
	record := func(id int64, fs []ixSampledFrame) {
		t.Helper()
		msg := queued
		if id == oid {
			msg = other
		}
		f, _ := ixCopyFactsOf(msg)
		f.ID = id
		j := ixSampleJob{work: id, size: f.Size}
		res := ixSampleResult{Result: "nomatch", Note: "at most 0 of 8 sampled frames match a saved video", Samples: len(fs), Frames: fs, Parts: 9}
		if err := a.ixSampleRecord(ctx, j, f, res); err != nil {
			t.Fatal(err)
		}
	}
	record(qid, samples)
	record(oid, otherSamples)
	var kept int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_copy_sample WHERE length(frames)=? AND dur_ms>0`, 8*ixVdupFrameBytes).Scan(&kept)
	if kept != 2 {
		t.Fatalf("frames kept for %d checks", kept)
	}

	// The first round starts the clock: nothing older is compared.
	if err := a.ixSampleRecheck(ctx); err != nil {
		t.Fatal(err)
	}
	var mark string
	if a.db.Reader.QueryRow(`SELECT value FROM kv WHERE key=?`, ixRecheckFlag).Scan(&mark) != nil || mark == "" {
		t.Fatal("no starting point")
	}
	// Now the original gets its fingerprint, and a video of another length too.
	at := ixNowMs() + 1000
	mustExec(t, a, `INSERT INTO ix_vfp(key,file_path,duration,frames,status,error,updated_at) VALUES(?,?,?,?,'ok','',?)`, "h:"+ixSixtyFour("d"), "c/orig.mp4", 600.0, ixPackFrames(origFrames), at)
	mustExec(t, a, `INSERT INTO ix_vfp(key,file_path,duration,frames,status,error,updated_at) VALUES(?,?,?,?,'ok','',?)`, "h:"+ixSixtyFour("e"), "c/long.mp4", 2000.0, ixPackFrames(origFrames), at)
	if err := a.ixSampleRecheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	var result, note, how, confirm string
	if err := a.db.Reader.QueryRow(`SELECT s.result, s.note, c.how, c.confirm FROM ix_copy_sample s JOIN ix_copy_hold c ON c.work_id=s.work_id
		JOIN ix_held h ON h.work_id=s.work_id AND h.released_at=0 WHERE s.work_id=?`, qid).Scan(&result, &note, &how, &confirm); err != nil {
		t.Fatalf("the copy is not held: %v", err)
	}
	if result != "confirmed" || how != "sampled" || confirm != "confirmed" || !strings.Contains(note, "compared again") || !strings.Contains(note, "+10 s") {
		t.Fatalf("copy: %q %q %q %q", result, note, how, confirm)
	}
	var otherResult string
	var otherHeld int
	_ = a.db.Reader.QueryRow(`SELECT result, (SELECT COUNT(*) FROM ix_held WHERE work_id=?) FROM ix_copy_sample WHERE work_id=?`, oid, oid).Scan(&otherResult, &otherHeld)
	if otherResult != "nomatch" || otherHeld != 0 {
		t.Fatalf("the other video changed: %q held %d", otherResult, otherHeld)
	}
	tot := ixSampleRecheckTotalOf(a)
	if tot.Copies.Load() != 1 || tot.Compared.Load() != 2 {
		t.Fatalf("totals: copies %d compared %d (the 2000 s video is not compared)", tot.Copies.Load(), tot.Compared.Load())
	}
	// Nothing new: nothing compared again.
	if err := a.ixSampleRecheck(ctx); err != nil {
		t.Fatal(err)
	}
	if tot.Compared.Load() != 2 {
		t.Fatal("compared again without a new fingerprint")
	}
	if line := a.ixSampleRateDiag(ctx); !strings.Contains(line, "last hour 2 checks, 18 MB of parts") || !strings.Contains(line, "1 copies") {
		t.Fatalf("report line: %s", line)
	}
}
