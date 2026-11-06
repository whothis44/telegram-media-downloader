package app

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// ix.12.11: the parts a check reads are kept for the download, within a cap
// and a free-space floor, and the check floor steps down once the big videos
// are checked. Invented data only.

func ix1211Space(t *testing.T, free int64) {
	t.Helper()
	savedFree, savedCap, savedFloor := ixPartsFree, ixPartsCap, ixPartsFreeFloor
	ixPartsFree = func(string) (int64, bool) { return free, true }
	t.Cleanup(func() { ixPartsFree, ixPartsCap, ixPartsFreeFloor = savedFree, savedCap, savedFloor })
}

func ix1211Data(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func ix1211Parts(data []byte, idx ...int64) map[int64][]byte {
	out := map[int64][]byte{}
	for _, i := range idx {
		start := i * telegram.PartSize
		out[i] = bytes.Clone(data[start:min(start+telegram.PartSize, int64(len(data)))])
	}
	return out
}

func ix1211Rows(t *testing.T, a *App, ident string) int {
	t.Helper()
	var n int
	if err := a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_part_store WHERE ident=?`, ident).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A check keeps the parts it read, whatever its answer; the download of the
// same identity finds exactly those bytes, another identity finds nothing.
func TestIx1211ACheckKeepsItsParts(t *testing.T) {
	a, ctx := ixTestApp(t)
	ix1211Space(t, 100<<30)
	savedFake, frameFake := ixSampleFake, ixSampleFrame
	ixSampleFake = true
	defer func() { ixSampleFake, ixSampleFrame = savedFake, frameFake }()
	var fp []ixVFrame
	for s := 0; s < 100; s++ {
		fp = append(fp, ixVFrame{T: float32(s), H: uint64(s+1) * 0x9E3779B97F4A7C15, HF: 7})
	}
	if err := a.ixVdupSave(ctx, "h:"+ixSixtyFour("7"), "c/saved.mp4", 100, fp, ""); err != nil {
		t.Fatal(err)
	}
	// ffmpeg reads the parts around the sample points: here part 0 and part 3.
	ixSampleFrame = func(ctx context.Context, address string, at float64) (ixSampledFrame, bool, error) {
		for _, off := range []int64{0, 3*telegram.PartSize + 5} {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
			req.Header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(off+15, 10))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return ixSampledFrame{}, false, err
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		return ixSampledFrame{T: at, H: uint64(at*1000) ^ 0x5555, HF: 1}, true, nil
	}
	data := ix1211Data(1, 4*telegram.PartSize+333)
	r := &ixFakeReader{files: map[int64][]byte{801: data}}
	msg := ixTestVideo(801, 801, int64(len(data)), 100, 1280, 720, nil, 1700000000)
	f, _ := ixCopyFactsOf(msg)
	res := a.ixSampleCheck(ctx, r, msg, f, "")
	if res.Result != "nomatch" {
		t.Fatalf("check: %+v", res)
	}
	att, _ := telegram.MessageAttachment(msg)
	if ix1211Rows(t, a, f.Ident) != 2 {
		t.Fatalf("kept %d parts, want 2 (parts 0 and 3)", ix1211Rows(t, a, f.Ident))
	}
	kept := a.ixKeptPartsFor(att.Identity)
	if kept == nil {
		t.Fatal("the download found no kept parts")
	}
	for i := 0; i < 5; i++ {
		got, ok := kept(i)
		want := (i == 0 || i == 3)
		if ok != want {
			t.Fatalf("part %d: kept %v, want %v", i, ok, want)
		}
		if ok && !bytes.Equal(got, data[int64(i)*telegram.PartSize:int64(i+1)*telegram.PartSize]) {
			t.Fatalf("part %d: other bytes", i)
		}
	}
	other := att.Identity
	other.ID = "802"
	if a.ixKeptPartsFor(other) != nil {
		t.Fatal("another file found these parts")
	}
	if line := a.ixPartsDiag(ctx); !strings.Contains(line, "kept now 2 MB of 1 files") {
		t.Fatalf("report: %s", line)
	}
}

// A kept part whose bytes or length changed on disk, or whose file is gone,
// is not given to the download.
func TestIx1211DoubtfulPartsAreRefused(t *testing.T) {
	a, ctx := ixTestApp(t)
	ix1211Space(t, 100<<30)
	id := telegram.MediaIdentity{Kind: "document", ID: "901", Size: 3*telegram.PartSize + 9}
	ident := ixPartIdent(id)
	data := ix1211Data(2, int(id.Size))
	a.ixPartsKeep(ctx, ident, ix1211Parts(data, 0, 1, 2, 3))
	if ix1211Rows(t, a, ident) != 4 {
		t.Fatal("not kept")
	}
	dir := filepath.Join(a.ixPartsRoot(), ixPartsDirName(ident))
	flipped := bytes.Clone(data[:telegram.PartSize])
	flipped[100] ^= 1
	_ = os.WriteFile(filepath.Join(dir, "0"), flipped, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "1"), data[telegram.PartSize:2*telegram.PartSize-1], 0o600)
	_ = os.Remove(filepath.Join(dir, "2"))
	kept := a.ixKeptPartsFor(id)
	for i := 0; i < 3; i++ {
		if _, ok := kept(i); ok {
			t.Fatalf("doubtful part %d was given", i)
		}
	}
	if got, ok := kept(3); !ok || !bytes.Equal(got, data[3*telegram.PartSize:]) {
		t.Fatal("the sound last part was refused")
	}
	c := ixPartsCountsOf(a)
	if c.BadBytes.Load() != 1 || c.Missing.Load() != 2 {
		t.Fatalf("refused: %d wrong bytes, %d unreadable", c.BadBytes.Load(), c.Missing.Load())
	}
}

// Nothing is kept while the data disk is under the free-space floor, and
// nothing over the cap; over the cap the oldest file's parts go first.
func TestIx1211CapAndFreeSpaceFloor(t *testing.T) {
	a, ctx := ixTestApp(t)
	ix1211Space(t, 20<<30+telegram.PartSize) // one part's room above the floor
	data := ix1211Data(3, 3*telegram.PartSize)
	a.ixPartsKeep(ctx, "document:1:3145728", ix1211Parts(data, 0, 1))
	if n := ix1211Rows(t, a, "document:1:3145728"); n != 0 {
		t.Fatalf("kept %d parts under the free-space floor", n)
	}
	if ixPartsCountsOf(a).LowDisk.Load() != 1 {
		t.Fatal("not counted")
	}
	if entries, _ := os.ReadDir(a.ixPartsRoot()); len(entries) != 0 {
		t.Fatal("a folder was left")
	}
	ixPartsFree = func(string) (int64, bool) { return 0, false }
	a.ixPartsKeep(ctx, "document:1:3145728", ix1211Parts(data, 0))
	if ix1211Rows(t, a, "document:1:3145728") != 0 {
		t.Fatal("kept with the free space unknown")
	}
	ixPartsFree = func(string) (int64, bool) { return 100 << 30, true }
	ixPartsCap = 5 * telegram.PartSize
	for i, ident := range []string{"document:1:3145728", "document:2:3145728", "document:3:3145728"} {
		a.ixPartsKeep(ctx, ident, ix1211Parts(data, 0, 1))
		if i < 2 {
			time.Sleep(3 * time.Millisecond)
		}
	}
	if ix1211Rows(t, a, "document:1:3145728") != 0 || ix1211Rows(t, a, "document:2:3145728") != 2 || ix1211Rows(t, a, "document:3:3145728") != 2 {
		t.Fatal("over the cap, the oldest did not go first")
	}
	if _, err := os.Stat(filepath.Join(a.ixPartsRoot(), ixPartsDirName("document:1:3145728"))); !os.IsNotExist(err) {
		t.Fatal("the oldest file's parts are still on disk")
	}
	// One check bigger than the cap keeps nothing.
	ixPartsCap = telegram.PartSize
	a.ixPartsKeep(ctx, "document:4:3145728", ix1211Parts(data, 0, 1))
	if ix1211Rows(t, a, "document:4:3145728") != 0 {
		t.Fatal("kept more than the cap")
	}
}

// The sweep removes a file's parts once it is downloaded or its queue row is
// gone, and folders no row names; a queued file keeps its parts.
func TestIx1211SweepRemovesWhatNoDownloadNeeds(t *testing.T) {
	a, ctx := ixTestApp(t)
	ix1211Space(t, 100<<30)
	data := ix1211Data(4, 2*telegram.PartSize)
	queued := ixTestVideo(1, 1001, int64(len(data)), 60, 640, 360, nil, 1700000000)
	done := ixTestVideo(2, 1002, int64(len(data)), 60, 640, 360, nil, 1700000000)
	saved := ixTestVideo(3, 1003, int64(len(data)), 60, 640, 360, nil, 1700000000)
	qID := ixTestQueue(t, a, queued)
	dID := ixTestQueue(t, a, done)
	ixTestQueue(t, a, saved)
	ident := func(m *tg.Message) string {
		f, _ := ixCopyFactsOf(m)
		return f.Ident
	}
	for _, m := range []*tg.Message{queued, done, saved} {
		a.ixPartsKeep(ctx, ident(m), ix1211Parts(data, 0))
	}
	a.ixPartsKeep(ctx, "document:1004:2097152", ix1211Parts(data, 0)) // no queue row at all
	_ = os.MkdirAll(filepath.Join(a.ixPartsRoot(), "leftover"), 0o700)
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='completed' WHERE id=?`, dID)
	att, _ := telegram.MessageAttachment(saved)
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_type,file_path,status,telegram_media_kind,telegram_media_id,telegram_media_size) VALUES(?,3,'v.mp4','video','c/v.mp4','completed',?,?,?)`,
		att.GroupID, att.Identity.Kind, att.Identity.ID, att.Identity.Size); err != nil {
		t.Fatal(err)
	}
	if err := a.ixPartsSweep(ctx); err != nil {
		t.Fatal(err)
	}
	if ix1211Rows(t, a, ident(queued)) != 1 {
		t.Fatal("a queued file lost its parts")
	}
	for _, gone := range []string{ident(done), ident(saved), "document:1004:2097152"} {
		if ix1211Rows(t, a, gone) != 0 {
			t.Fatalf("%s kept", gone)
		}
	}
	entries, _ := os.ReadDir(a.ixPartsRoot())
	if len(entries) != 1 || entries[0].Name() != ixPartsDirName(ident(queued)) {
		t.Fatalf("folders left: %d", len(entries))
	}
	_ = qID
}

// The floor steps down 200 → 100 → 50 → 20 MB, only when nothing at or above
// the current step waits; a row that cannot be checked holds nothing up.
func TestIx1211TheCheckFloorStepsDown(t *testing.T) {
	a, ctx := ixTestApp(t)
	saved := ixSampleFloorFor
	ixSampleFloorFor = 0
	defer func() { ixSampleFloorFor = saved }()
	q := func(id int, size int64) int64 {
		return ixTestQueue(t, a, ixTestVideo(id, int64(5000+id), size, 60, 640, 360, nil, 1700000000))
	}
	checked := func(work int64) {
		t.Helper()
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_copy_sample(work_id,ident,size,result,at) VALUES(?,'',0,'nomatch',1)`, work); err != nil {
			t.Fatal(err)
		}
	}
	floor := func(want int64) {
		t.Helper()
		if got := a.ixSampleFloor(ctx); got != want<<20 {
			t.Fatalf("floor %d MB, want %d MB", got>>20, want)
		}
	}
	floor(20) // nothing waits at all
	big := q(1, 250<<20)
	mid := q(2, 150<<20)
	small := q(3, 60<<20)
	tiny := q(4, 30<<20)
	q(5, 10<<20)
	floor(200)
	checked(big)
	floor(100)
	checked(mid)
	floor(50)
	checked(small)
	floor(20)
	checked(tiny)
	floor(20)
	// A big row whose file's check failed for good, and one whose message
	// cannot be read, are never checked: they do not hold the floor up.
	stuck := ixTestVideo(6, 6006, 400<<20, 60, 640, 360, nil, 1700000000)
	ixTestQueue(t, a, stuck)
	floor(200)
	f, _ := ixCopyFactsOf(stuck)
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_copy_sample(work_id,ident,size,result,at,reason,final,tries) VALUES(999999,?,0,'failed',1,'no-index',1,1)`, f.Ident); err != nil {
		t.Fatal(err)
	}
	floor(20)
	bad := q(7, 300<<20)
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET body=x'00' WHERE id=?`, bad)
	floor(20)
	// The report says the floor in use.
	if line := a.ixSampleRateDiag(ctx); !strings.Contains(line, "(videos of 20 MB or more: the floor in use)") {
		t.Fatalf("report: %s", line)
	}
	// Remembered for a while.
	ixSampleFloorFor = time.Hour
	_ = a.ixSampleFloor(ctx)
	q(8, 500<<20)
	floor(20)
}
