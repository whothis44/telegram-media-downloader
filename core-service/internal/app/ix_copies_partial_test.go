package app

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// A big video with half its sampled frames in a saved video (another cut, a
// compilation with it in) is not a copy: it is still downloaded, but after
// everything else. Two matching frames say nothing, and do not move it.
func TestIxPartlyMatchingBigVideoGoesLast(t *testing.T) {
	a, ctx := ixTestApp(t)
	savedFake, frameFake := ixSampleFake, ixSampleFrame
	ixSampleFake = true
	defer func() { ixSampleFake, ixSampleFrame = savedFake, frameFake }()
	hashAt := func(s int) uint64 { return uint64(s+1) * 0x9E3779B97F4A7C15 }
	var fp []ixVFrame
	for s := 0; s < 3000; s++ {
		fp = append(fp, ixVFrame{T: float32(s), H: hashAt(s), HF: ^hashAt(s)})
	}
	if err := a.ixVdupSave(ctx, "h:"+ixSixtyFour("7"), "c/long.mp4", 3000, fp, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_type,file_path,status,file_hash) VALUES(?,90,'long.mp4','video','c/long.mp4','completed',?)`, ixTestGroup, ixSixtyFour("7")); err != nil {
		t.Fatal(err)
	}
	// Frames before this second are the saved video's, per marked file.
	sameUntil := map[string]float64{"HALF-HALF-HALF-1": 1500, "FEW--FEW--FEW--1": 600}
	ixSampleFrame = func(ctx context.Context, address string, at float64) (ixSampledFrame, bool, error) {
		resp, err := http.Get(address)
		if err != nil {
			return ixSampledFrame{}, false, err
		}
		b := make([]byte, 16)
		_, _ = io.ReadFull(resp.Body, b)
		resp.Body.Close()
		if at < sameUntil[string(b)] {
			return ixSampledFrame{T: at, H: hashAt(int(at)), HF: ^hashAt(int(at))}, true, nil
		}
		return ixSampledFrame{T: at, H: uint64(at*1000) ^ 0x5555, HF: 1}, true, nil
	}
	r := &ixFakeReader{files: map[int64][]byte{}}
	ixTestUseReader(t, r)
	queue := func(doc int64, mark string) int64 {
		data := make([]byte, 2*telegram.PartSize)
		copy(data, mark)
		r.files[doc] = data
		return ixTestQueue(t, a, ixTestVideo(int(doc), doc, 1536<<20, 3000, 1280, 720, nil, 1700000000))
	}
	half := queue(801, "HALF-HALF-HALF-1")
	few := queue(802, "FEW--FEW--FEW--1")
	if err := a.ixSampleRound(ctx, false); err != nil {
		t.Fatal(err)
	}
	var result string
	_ = a.db.Reader.QueryRow(`SELECT result FROM ix_copy_sample WHERE work_id=?`, half).Scan(&result)
	if result != "nomatch" {
		t.Fatalf("half: %q, not a copy", result)
	}
	if how, _ := ixTestHow(a, half); how != "sampled-partial" || !ixTestPaused(t, a, half) {
		t.Fatalf("half the footage saved: held %q", how)
	}
	var confirm string
	_ = a.db.Reader.QueryRow(`SELECT confirm FROM ix_copy_hold WHERE work_id=?`, half).Scan(&confirm)
	if confirm != "unconfirmed" {
		t.Fatalf("confirm %q", confirm)
	}
	if how, _ := ixTestHow(a, few); how != "" || ixTestPaused(t, a, few) {
		t.Fatalf("two matching frames moved it: %q", how)
	}
}
