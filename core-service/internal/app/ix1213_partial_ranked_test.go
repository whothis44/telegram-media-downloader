package app

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// ix.12.13: in a ranked group a partly matching video keeps its place in the
// rank order; held ones go back once their group is ranked. Unranked groups
// and probable copies keep the 12.5 rule.
func TestIx1213PartlyMatchingVideosOfRankedGroupsKeepTheirPlace(t *testing.T) {
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
	half := queue(811, "HALF-HALF-HALF-1")
	if err := a.ixSampleRound(ctx, false); err != nil {
		t.Fatal(err)
	}
	if how, _ := ixTestHow(a, half); how != "sampled-partial" || !ixTestPaused(t, a, half) {
		t.Fatalf("unranked group: held %q", how)
	}
	if n, err := a.ixReleasePartialRanked(ctx); err != nil || n != 0 || !ixTestPaused(t, a, half) {
		t.Fatalf("unranked group let go: %d %v", n, err)
	}
	// A probable copy in the same group, held as one.
	copyRow := ixTestWork(t, a, "", ixTestGroup, 9001, "video", 300<<20, 0)
	if ok, err := a.ixHoldWork(ctx, ixCopyFacts{ID: copyRow, Group: ixTestGroup, Msg: 9001}, "saved", "c/long.mp4", "", 0, 0, "looks like a saved copy"); err != nil || !ok {
		t.Fatalf("hold: %v %v", ok, err)
	}
	if err := a.ixSaveRanks(ctx, []string{ixTestGroup}, map[string]string{ixTestGroup: "g"}); err != nil {
		t.Fatal(err)
	}
	if n, err := a.ixReleasePartialRanked(ctx); err != nil || n != 1 {
		t.Fatalf("ranked group: let go %d %v", n, err)
	}
	if ixTestPaused(t, a, half) {
		t.Fatal("the partly matching video of a ranked group is still held")
	}
	if !ixTestPaused(t, a, copyRow) {
		t.Fatal("a probable copy of a ranked group was let go")
	}
	if n, _ := a.ixReleasePartialRanked(ctx); n != 0 {
		t.Fatalf("let go twice: %d", n)
	}
	// A new one in the ranked group is not held at all.
	half2 := queue(812, "HALF-HALF-HALF-1")
	if err := a.ixSampleRound(ctx, false); err != nil {
		t.Fatal(err)
	}
	var result string
	_ = a.db.Reader.QueryRow(`SELECT result FROM ix_copy_sample WHERE work_id=?`, half2).Scan(&result)
	if result != "nomatch" {
		t.Fatalf("not checked: %q", result)
	}
	if how, _ := ixTestHow(a, half2); how != "" || ixTestPaused(t, a, half2) {
		t.Fatalf("held in a ranked group: %q", how)
	}
	// The held step releases them on its own (a group ranked later).
	half3 := queue(813, "HALF-HALF-HALF-1")
	if _, err := a.db.Writer.Exec(`DELETE FROM tgdl_group_rank`); err != nil {
		t.Fatal(err)
	}
	if err := a.ixSampleRound(ctx, false); err != nil {
		t.Fatal(err)
	}
	if !ixTestPaused(t, a, half3) {
		t.Fatal("unranked again: not held")
	}
	if err := a.ixSaveRanks(ctx, []string{ixTestGroup}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.ixHeldStep(ctx); err != nil {
		t.Fatal(err)
	}
	if ixTestPaused(t, a, half3) {
		t.Fatal("the held step did not let it go")
	}
}
