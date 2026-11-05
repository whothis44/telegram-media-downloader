package app

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/crashlog"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// ---------- faces (all invented) ----------

type ixFaceWorld struct {
	t   *testing.T
	a   *App
	ctx context.Context
	vec []float32
	n   float64
}

func (w *ixFaceWorld) face(key string, person int64) (int64, []int8) {
	w.t.Helper()
	q, _ := ixQuantise(w.vec)
	blob := make([]byte, len(q))
	for j, x := range q {
		blob[j] = byte(x)
	}
	w.n++
	x1 := 0.05 * w.n
	res, err := w.a.db.Writer.Exec(`INSERT INTO ix_faces(key,model,x1,y1,x2,y2,score,vec,person,at,frame,px,blur) VALUES(?,?,?,0.1,?,0.5,0.95,?,?,1,0,200,100)`,
		key, ixFaceModel, x1, x1+0.04, blob, person)
	if err != nil {
		w.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	box := w.a.ixMLBox()
	box.faces.mu.Lock()
	box.faces.addLocked(id, q, person, ixFaceInfo{key: key, score: 0.95, px: 200, blur: 100})
	box.faces.mu.Unlock()
	return id, q
}

func (w *ixFaceWorld) personOf(id int64) int64 {
	w.t.Helper()
	var p int64
	if err := w.a.db.Reader.QueryRow(`SELECT person FROM ix_faces WHERE id=?`, id).Scan(&p); err != nil {
		w.t.Fatal(err)
	}
	return p
}

func ixFaceTestWorld(t *testing.T) *ixFaceWorld {
	t.Helper()
	a, ctx := ixTestApp(t)
	if err := a.ixMLBox().faces.load(ctx, a.db.Reader, ixFaceModel); err != nil {
		t.Fatal(err)
	}
	if err := a.ixFaceMetaSet(ctx, "rules", ixFaceRulesVersion); err != nil {
		t.Fatal(err)
	}
	return &ixFaceWorld{t: t, a: a, ctx: ctx, vec: ixLQUnit(rand.New(rand.NewSource(1)), 512)}
}

func (w *ixFaceWorld) person() int64 {
	w.t.Helper()
	res, err := w.a.db.Writer.Exec(`INSERT INTO ix_people(name,created_at,updated_at) VALUES('',1,1)`)
	if err != nil {
		w.t.Fatal(err)
	}
	p, _ := res.LastInsertId()
	return p
}

// The crash of 2026-11-05 (fatal error: stack overflow, in a live archive): loose
// faces alike to a person they may not join, because they were taken out of them
// or that person already has a face in their picture. Each looked like it should go
// to that person first, each was looked at, and each sent the others round again
// for ever. A stack overflow ends the whole program, recovered or not.
func TestIxFacesThatMayNotJoinDoNotSendEachOtherRoundForEver(t *testing.T) {
	t.Run("taken out of the person", func(t *testing.T) {
		w := ixFaceTestWorld(t)
		p := w.person()
		w.face("z", p)
		a, va := w.face("a", 0)
		b, _ := w.face("b", 0)
		c, _ := w.face("c", 0)
		for _, id := range []int64{a, b, c} {
			if _, err := w.a.db.Writer.Exec(`INSERT INTO ix_face_reject(face,person,at) VALUES(?,?,1)`, id, p); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.a.ixGroupFace(w.ctx, a, va); err != nil {
			t.Fatal(err)
		}
		got := w.personOf(a)
		if got == 0 || got == p || w.personOf(b) != got || w.personOf(c) != got {
			t.Fatalf("person %d: a %d, b %d, c %d", p, got, w.personOf(b), w.personOf(c))
		}
	})
	t.Run("the person is already in their pictures", func(t *testing.T) {
		w := ixFaceTestWorld(t)
		p := w.person()
		for _, k := range []string{"a", "b", "c"} {
			w.face(k, p)
		}
		a, va := w.face("a", 0)
		b, _ := w.face("b", 0)
		c, _ := w.face("c", 0)
		if err := w.a.ixGroupFace(w.ctx, a, va); err != nil {
			t.Fatal(err)
		}
		got := w.personOf(a)
		if got == 0 || got == p || w.personOf(b) != got || w.personOf(c) != got {
			t.Fatalf("person %d: a %d, b %d, c %d", p, got, w.personOf(b), w.personOf(c))
		}
	})
}

// What the fix must not change: a look-alike that may join goes to the person, and
// no second person is made out of faces that belong to the first.
func TestIxFaceLookAlikeThatMayJoinStillGoesFirst(t *testing.T) {
	w := ixFaceTestWorld(t)
	p := w.person()
	w.face("z", p)
	a, va := w.face("a", 0)
	b, _ := w.face("b", 0)
	c, _ := w.face("c", 0)
	// a may not join, the others may.
	if _, err := w.a.db.Writer.Exec(`INSERT INTO ix_face_reject(face,person,at) VALUES(?,?,1)`, a, p); err != nil {
		t.Fatal(err)
	}
	if err := w.a.ixGroupFace(w.ctx, a, va); err != nil {
		t.Fatal(err)
	}
	if w.personOf(b) != p || w.personOf(c) != p {
		t.Fatalf("b %d, c %d, not person %d", w.personOf(b), w.personOf(c), p)
	}
	if w.personOf(a) == p {
		t.Fatal("a face taken out of a person went back to them")
	}
}

// ---------- sampling ----------

type ixSampleWorld struct {
	t      *testing.T
	a      *App
	ctx    context.Context
	reader *ixFakeReader
	frames int
}

func ixSampleTestWorld(t *testing.T) *ixSampleWorld {
	t.Helper()
	a, ctx := ixTestApp(t)
	savedFake, frameFake, perRun := ixSampleFake, ixSampleFrame, ixSamplesPerRun
	ixSampleFake = true
	ixSamplesPerRun = 1
	t.Cleanup(func() { ixSampleFake, ixSampleFrame, ixSamplesPerRun = savedFake, frameFake, perRun })
	hashAt := func(s int) uint64 { return uint64(s+1) * 0x9E3779B97F4A7C15 }
	var fp []ixVFrame
	for s := 0; s < 3000; s++ {
		fp = append(fp, ixVFrame{T: float32(s), H: hashAt(s), HF: ^hashAt(s)})
	}
	key := "h:" + ixSixtyFour("7")
	if err := a.ixVdupSave(ctx, key, "c/long.mp4", 3000, fp, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_type,file_path,status,file_hash) VALUES(?,90,'long.mp4','video','c/long.mp4','completed',?)`, "-1001", ixSixtyFour("7")); err != nil {
		t.Fatal(err)
	}
	w := &ixSampleWorld{t: t, a: a, ctx: ctx, reader: &ixFakeReader{files: map[int64][]byte{}}}
	ixTestUseReader(t, w.reader)
	ixSampleFrame = func(ctx context.Context, address string, at float64) (ixSampledFrame, bool, error) {
		w.frames++
		resp, err := http.Get(address)
		if err != nil {
			return ixSampledFrame{}, false, err
		}
		b := make([]byte, 16)
		_, _ = io.ReadFull(resp.Body, b)
		resp.Body.Close()
		if string(b) == "COPY-COPY-COPY-1" {
			return ixSampledFrame{T: at, H: hashAt(int(at)), HF: ^hashAt(int(at))}, true, nil
		}
		return ixSampledFrame{T: at, H: uint64(at*1000) ^ 0x5555, HF: 1}, true, nil
	}
	return w
}

func (w *ixSampleWorld) queue(msg int, doc int64, mark string) int64 {
	w.t.Helper()
	data := make([]byte, 2*telegram.PartSize)
	copy(data, mark)
	w.reader.files[doc] = data
	return ixTestQueue(w.t, w.a, ixTestVideo(msg, doc, 1<<30, 3000, 1280, 720, nil, 1700000000))
}

func (w *ixSampleWorld) result(id int64) (result, reason string, parts int) {
	w.t.Helper()
	_ = w.a.db.Reader.QueryRow(`SELECT result, reason, parts FROM ix_copy_sample WHERE work_id=?`, id).Scan(&result, &reason, &parts)
	return
}

func TestIxSamplingAsksOncePerTelegramFile(t *testing.T) {
	w := ixSampleTestWorld(t)
	first := w.queue(801, 900, "COPY-COPY-COPY-1")
	second := w.queue(802, 900, "COPY-COPY-COPY-1")
	third := w.queue(803, 900, "COPY-COPY-COPY-1")
	if err := w.a.ixSampleRound(w.ctx, false); err != nil {
		t.Fatal(err)
	}
	asked := w.reader.asked
	if asked == 0 {
		t.Fatal("nothing was read")
	}
	if err := w.a.ixSampleRound(w.ctx, false); err != nil {
		t.Fatal(err)
	}
	if w.reader.asked != asked {
		t.Fatalf("the same file was read again: %d parts, then %d", asked, w.reader.asked)
	}
	var fresh, reused int
	for _, id := range []int64{first, second, third} {
		result, reason, _ := w.result(id)
		if result != "confirmed" {
			t.Fatalf("row %d: %q", id, result)
		}
		if how, _ := ixTestHow(w.a, id); how != "sampled" || !ixTestPaused(t, w.a, id) {
			t.Fatalf("row %d is not held: %q", id, how)
		}
		if reason == ixSampleSameFile {
			reused++
		} else {
			fresh++
		}
	}
	if fresh != 1 || reused != 2 {
		t.Fatalf("%d checked, %d reused", fresh, reused)
	}
	tot := w.a.ixSampleTotalsNow(w.ctx)
	if tot.Checked != 1 || tot.Reused != 2 || tot.SizeBytes != 1<<30 || tot.Confirmed != 3 {
		t.Fatalf("totals %+v", tot)
	}
}

// Rows of a file whose check failed wait with it, and another file is not held up.
func TestIxSamplingWaitsForAFailedCheckOfTheSameFile(t *testing.T) {
	w := ixSampleTestWorld(t)
	failed := w.queue(801, 900, "COPY-COPY-COPY-1")
	twin := w.queue(802, 900, "COPY-COPY-COPY-1")
	other := w.queue(803, 901, "other-other-othe")
	if _, err := w.a.db.Writer.Exec(`INSERT INTO ix_copy_sample(work_id,ident,size,result,note,parts,samples,tries,at,reason,final) VALUES(?,?,?,'failed','',0,0,1,?,?,0)`,
		failed, "document:900:1073741824", 1<<30, time.Now().UnixMilli(), ixSampleWhyTimeout); err != nil {
		t.Fatal(err)
	}
	if err := w.a.ixSampleRound(w.ctx, false); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := w.result(twin); r != "" {
		t.Fatalf("the twin of a failed check was checked: %q", r)
	}
	if r, _, _ := w.result(other); r != "nomatch" {
		t.Fatalf("another file: %q", r)
	}
}

func TestIxSampleAnswerRules(t *testing.T) {
	w := ixSampleTestWorld(t)
	put := func(work int64, ident, result, note, of string, samples int) {
		t.Helper()
		if _, err := w.a.db.Writer.Exec(`INSERT INTO ix_copy_sample(work_id,ident,size,result,note,of_key,parts,samples,tries,at) VALUES(?,?,1,?,?,?,1,?,1,?)`,
			work, ident, result, note, of, samples, work); err != nil {
			t.Fatal(err)
		}
	}
	put(1, "f:1:1", "nomatch", "at most 4 of 8 sampled frames match a saved video", "h:near", 8)
	put(2, "f:2:1", "nomatch", "no saved video of a similar length has a fingerprint yet", "", 0)
	put(3, "f:3:1", "failed", "", "", 2)
	put(4, "f:4:1", "nomatch", "at most 1 of 8 sampled frames match a saved video", "h:a", 8)
	put(5, "f:4:1", "confirmed", "8 of 8", "h:b", 8)

	res, ok := w.a.ixSampleAnswerFor(w.ctx, "f:1:1", "")
	if !ok || res.Best != 4 || res.Of != "h:near" || res.Why != ixSampleSameFile || res.Parts != 0 {
		t.Fatalf("%+v %v", res, ok)
	}
	if _, ok := w.a.ixSampleAnswerFor(w.ctx, "f:1:1", "h:other"); ok {
		t.Fatal("a check that never compared with this row's like file answered for it")
	}
	if _, ok := w.a.ixSampleAnswerFor(w.ctx, "f:1:1", "h:near"); !ok {
		t.Fatal("it did compare with the closest one")
	}
	if _, ok := w.a.ixSampleAnswerFor(w.ctx, "f:2:1", ""); ok {
		t.Fatal("a check that compared nothing answered")
	}
	if _, ok := w.a.ixSampleAnswerFor(w.ctx, "f:3:1", ""); ok {
		t.Fatal("a failed check answered")
	}
	if _, ok := w.a.ixSampleAnswerFor(w.ctx, "", ""); ok {
		t.Fatal("no identity answered")
	}
	// Copy beats not a copy, whatever the like file.
	if res, ok := w.a.ixSampleAnswerFor(w.ctx, "f:4:1", "h:other"); !ok || res.Result != "confirmed" || res.Of != "h:b" {
		t.Fatalf("%+v %v", res, ok)
	}
}

func TestIxSampleStopsWhenMoreFramesCannotMatter(t *testing.T) {
	w := ixSampleTestWorld(t)
	// A copy: the rule needs 5 of 8.
	id := w.queue(801, 900, "COPY-COPY-COPY-1")
	if err := w.a.ixSampleRound(w.ctx, false); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := w.result(id); r != "confirmed" || w.frames != 5 {
		t.Fatalf("%q after %d frames", r, w.frames)
	}
	// Nothing alike at all.
	w.frames = 0
	id = w.queue(802, 901, "other-other-othe")
	if err := w.a.ixSampleRound(w.ctx, false); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := w.result(id); r != "nomatch" || w.frames != ixSampleMissAfter {
		t.Fatalf("%q after %d frames", r, w.frames)
	}
}

func TestIxSampleDecided(t *testing.T) {
	var saved []ixVFrame
	for s := 0; s < 100; s++ {
		saved = append(saved, ixVFrame{T: float32(s), H: uint64(s+1) * 0x9E3779B97F4A7C15, HF: 7})
	}
	cands := []ixSampleCand{{key: "k", frames: saved}}
	make2 := func(hits, misses int) []ixSampledFrame {
		var out []ixSampledFrame
		for i := 0; i < hits; i++ {
			out = append(out, ixSampledFrame{T: float64(i * 10), H: saved[i*10].H, HF: 1})
		}
		for i := 0; i < misses; i++ {
			out = append(out, ixSampledFrame{T: float64(i), H: 0x5555555555555555 ^ uint64(i), HF: 0xAAAAAAAAAAAAAAAA})
		}
		return out
	}
	for name, c := range map[string]struct {
		hits, misses, left int
		want               bool
	}{
		"too few to say anything":     {0, 2, 6, false},
		"a copy whatever else":        {5, 0, 3, true},
		"not yet a copy":              {4, 0, 4, false},
		"nothing in five":             {0, 5, 3, true},
		"nothing in four":             {0, 4, 4, false},
		"could still be partly":       {1, 5, 2, false},
		"can no longer be partly":     {1, 6, 1, true},
		"partly: goes on to the end":  {3, 3, 2, false},
		"two, with one left, is done": {2, 5, 0, true},
	} {
		if got := ixSampleDecided(make2(c.hits, c.misses), cands, c.left); got != c.want {
			t.Fatalf("%s: %v", name, got)
		}
	}
	// Footage over 3 points in a row meets one of the first ixSampleMissAfter.
	first := map[int]bool{}
	for _, p := range ixSampleOrder[:ixSampleMissAfter] {
		first[p] = true
	}
	for i := 0; i+2 < ixSamplePoints; i++ {
		if !first[i] && !first[i+1] && !first[i+2] {
			t.Fatalf("points %d to %d are all after the first frames", i, i+2)
		}
	}
	seen := map[int]bool{}
	for _, p := range ixSampleOrder {
		seen[p] = true
	}
	if len(seen) != ixSamplePoints {
		t.Fatalf("order %v", ixSampleOrder)
	}
}

// ---------- restarts in the report ----------

func TestIxRestartsInDiagnosticsNameTheCauseOnly(t *testing.T) {
	a, _ := ixTestApp(t)
	dir := filepath.Join(a.dataDir, CrashDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	line := fmt.Sprintf(`{"started":%q,"ended":%q,"version":"1","how":"crash","cause":"fatal error: stack overflow","at":"internal/app.x y.go:1","goroutines":9}`,
		now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339))
	old := fmt.Sprintf(`{"ended":%q,"how":"crash"}`, now.Add(-30*24*time.Hour).Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte(line+"\n"+old+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := crashlog.Read(dir); len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	text := strings.Join(a.ixRestartsDiag(now), "\n")
	for _, want := range []string{"last 7 days: 1 crashes", "fatal error: stack overflow", "internal/app.x y.go:1", "after 1h0m0s", "9 goroutines"} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q is not in:\n%s", want, text)
		}
	}
}
