package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// A synthetic world of self-filmed, low-quality faces (all invented: random
// identities, random vectors, made-up sizes and sharpness). Each person is
// a random direction; a face is its person's direction plus noise, and the
// poorer the face (blurred, small), the less of the person is in it and the
// more of a direction that all poor faces share: poor faces look a bit like
// everyone, which is what makes them dangerous. Two pairs of people are
// look-alikes (siblings). Sizes and sharpness spread like front-camera
// footage in low light: half the faces under 80 px and under sharpness 26
// (p10/p50/p90: 50/79/119 px, sharpness 9/28/80). Two faces of one person
// are 0.43-0.73 alike (half under 0.57), siblings up to about 0.54, others
// up to about 0.49: the likeness thresholds alone cannot keep everyone
// apart, so the rules have to.

type ixLQFace struct {
	who             int
	key             string
	score, px, blur float64
	vec             []float32
}

type ixLQWorld struct {
	faces []ixLQFace
	files []string // in the order they are looked at
	byKey map[string][]int
}

func ixLQUnit(r *rand.Rand, d int) []float32 {
	v := make([]float32, d)
	var n float64
	for i := range v {
		x := r.NormFloat64()
		v[i] = float32(x)
		n += x * x
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(n))
	}
	return v
}

func ixLQMix(parts ...any) []float32 {
	var out []float32
	for i := 0; i < len(parts); i += 2 {
		w, v := parts[i].(float64), parts[i+1].([]float32)
		if out == nil {
			out = make([]float32, len(v))
		}
		for j := range v {
			out[j] += float32(w) * v[j]
		}
	}
	var n float64
	for _, x := range out {
		n += float64(x) * float64(x)
	}
	for j := range out {
		out[j] /= float32(math.Sqrt(n))
	}
	return out
}

func ixLQMake(seed int64) *ixLQWorld {
	r := rand.New(rand.NewSource(seed))
	const d = 512
	// Six people seen often, fourteen now and then, sixty strangers seen
	// once or twice (who should stay loose).
	sizes := []int{40, 30, 25, 20, 15, 12}
	for i := 0; i < 14; i++ {
		sizes = append(sizes, 4+r.Intn(7))
	}
	for i := 0; i < 60; i++ {
		sizes = append(sizes, 1+r.Intn(2))
	}
	ids := make([][]float32, len(sizes))
	for i := range ids {
		ids[i] = ixLQUnit(r, d)
	}
	ids[1] = ixLQMix(0.35, ids[0], math.Sqrt(1-0.35*0.35), ids[1]) // siblings, 0.35 alike
	ids[3] = ixLQMix(0.45, ids[2], math.Sqrt(1-0.45*0.45), ids[3]) // closer siblings, 0.45
	ids[7] = ixLQMix(0.45, ids[6], math.Sqrt(1-0.45*0.45), ids[7])
	poor := ixLQUnit(r, d)
	w := &ixLQWorld{byKey: map[string][]int{}}
	face := func(who int, key string) {
		px := math.Max(30, math.Min(320, math.Exp(math.Log(80)+0.35*r.NormFloat64())))
		blur := math.Exp(math.Log(26) + 0.9*r.NormFloat64())
		score := math.Max(0.7, math.Min(0.99, 0.93-math.Abs(0.1*r.NormFloat64())))
		s := math.Max(0, math.Min(1, math.Log(blur/10)/math.Log(20)))
		z := math.Max(0, math.Min(1, (px-40)/80))
		q := 0.5*s + 0.5*z
		c := 0.5 + 0.35*r.Float64()
		v := ixLQMix(0.4+0.6*q, ids[who], 0.7-0.6*q, poor, c, ixLQUnit(r, d))
		w.byKey[key] = append(w.byKey[key], len(w.faces))
		w.faces = append(w.faces, ixLQFace{who: who, key: key, score: score, px: px, blur: blur, vec: v})
	}
	for who, n := range sizes {
		for i := 0; i < n; i++ {
			face(who, fmt.Sprintf("p%d-%d", who, i))
		}
	}
	for i := 0; i < 30; i++ { // two people in one picture
		a, b := r.Intn(20), r.Intn(20)
		if a == b {
			b = (a + 1) % 20
		}
		k := fmt.Sprintf("m%d", i)
		face(a, k)
		face(b, k)
	}
	for k := range w.byKey {
		w.files = append(w.files, k)
	}
	sort.Strings(w.files)
	r.Shuffle(len(w.files), func(i, j int) { w.files[i], w.files[j] = w.files[j], w.files[i] })
	return w
}

// ixLQScan looks at the world's files one at a time as the faces task does
// (store the file's faces, group them), then as its rounds end (loose faces
// again, people compared). It says each stored face's person.
func ixLQScan(t *testing.T, a *App, ctx context.Context, w *ixLQWorld) map[int64]int {
	t.Helper()
	box := a.ixMLBox()
	if err := box.faces.load(ctx, a.db.Reader, ixFaceModel); err != nil {
		t.Fatal(err)
	}
	if err := a.ixFaceMetaSet(ctx, "rules", ixFaceRulesVersion); err != nil {
		t.Fatal(err)
	}
	who := map[int64]int{}
	for _, k := range w.files {
		var ids []int64
		var qs [][]int8
		for fi, n := range w.byKey[k] {
			f := w.faces[n]
			q, _ := ixQuantise(f.vec)
			blob := make([]byte, len(q))
			for j, x := range q {
				blob[j] = byte(x)
			}
			x1 := 0.05 + 0.5*float64(fi)
			res, err := a.db.Writer.Exec(`INSERT INTO ix_faces(key,model,x1,y1,x2,y2,score,vec,person,at,frame,px,blur) VALUES(?,?,?,0.1,?,0.5,?,?,0,1,0,?,?)`,
				k, ixFaceModel, x1, x1+0.4, f.score, blob, f.px, f.blur)
			if err != nil {
				t.Fatal(err)
			}
			id, _ := res.LastInsertId()
			box.faces.mu.Lock()
			box.faces.addLocked(id, q, 0, ixFaceInfo{key: k, score: f.score, px: f.px, blur: f.blur})
			box.faces.mu.Unlock()
			ids, qs = append(ids, id), append(qs, q)
			who[id] = f.who
		}
		for i := range ids {
			if err := a.ixGroupFace(ctx, ids[i], qs[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < 2*len(w.faces)/300+2; i++ {
		a.ixRegroupFaces(ctx, 300)
	}
	if err := a.ixFacePeoplePass(ctx); err != nil {
		t.Fatal(err)
	}
	return who
}

type ixLQResult struct {
	usable, grouped, wrong, people, split int
	often, oftenGrouped                   int // usable faces of people seen in 3 files or more
}

// ixLQJudge counts usable faces, grouped ones, faces with a person who is
// mostly someone else (wrong), people, and people beyond one per person
// (split).
func ixLQJudge(t *testing.T, a *App, w *ixLQWorld, who map[int64]int) ixLQResult {
	t.Helper()
	var res ixLQResult
	files := map[int]map[string]bool{}
	for _, f := range w.faces {
		if files[f.who] == nil {
			files[f.who] = map[string]bool{}
		}
		files[f.who][f.key] = true
	}
	byPerson := map[int64]map[int]int{}
	rows, err := a.db.Reader.Query(`SELECT f.id, CASE WHEN f.person>0 THEN COALESCE(NULLIF(p.merged_into,0), f.person) ELSE 0 END, f.score, f.px, f.blur FROM ix_faces f LEFT JOIN ix_people p ON p.id=f.person`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, person int64
		var score, px, blur float64
		_ = rows.Scan(&id, &person, &score, &px, &blur)
		usable := ixFaceTierOf(score, px, blur) != ixFaceJunk
		often := usable && len(files[who[id]]) >= 3
		if usable {
			res.usable++
		}
		if often {
			res.often++
		}
		if person > 0 {
			res.grouped++
			if often {
				res.oftenGrouped++
			}
			if byPerson[person] == nil {
				byPerson[person] = map[int]int{}
			}
			byPerson[person][who[id]]++
		}
	}
	rows.Close()
	perWho := map[int]int{}
	for _, c := range byPerson {
		best, total := -1, 0
		for w, n := range c {
			total += n
			if best < 0 || n > c[best] {
				best = w
			}
		}
		res.wrong += total - c[best]
		perWho[best]++
		res.people++
	}
	for _, n := range perWho {
		res.split += n - 1
	}
	return res
}

// In the low-quality world most faces of people seen in three files or
// more end up with a person, nobody gets a face of someone else, and the
// strangers seen once or twice stay loose. Under the usual fixed gates
// (what the archive did before) most faces stayed loose.
func TestIxFaceLowQualityWorldGroups(t *testing.T) {
	for seed := int64(1); seed <= 3; seed++ {
		w := ixLQMake(seed)
		a, ctx := ix8App(t, map[string]any{})
		r := ixLQJudge(t, a, w, ixLQScan(t, a, ctx, w))
		g := a.ixMLBox().faces.gatesNow()
		share := float64(r.oftenGrouped) / float64(r.often)
		t.Logf("seed %d: gates %.2f/%.0f px/%.0f · usable %d grouped %d · seen often %d grouped %.0f%% · people %d", seed, g.Score, g.Px, g.Blur, r.usable, r.grouped, r.often, 100*share, r.people)
		if g.Px != ixFaceGatePxFloor || g.Blur != ixFaceGateBlurFloor || g.Score < ixFaceGateScoreLow {
			t.Fatalf("seed %d: gates %+v", seed, g)
		}
		if r.wrong != 0 || r.split != 0 {
			t.Fatalf("seed %d: %d faces with someone else, %d people split", seed, r.wrong, r.split)
		}
		if share < 0.75 || r.people < 16 || r.people > 20 {
			t.Fatalf("seed %d: %.0f%% grouped, %d people (20 seen often)", seed, 100*share, r.people)
		}
		// Before: fixed gates.
		old := ixFaceGateMin
		ixFaceGateMin = 1 << 30
		b, bctx := ix8App(t, map[string]any{})
		rb := ixLQJudge(t, b, w, ixLQScan(t, b, bctx, w))
		ixFaceGateMin = old
		t.Logf("seed %d, fixed gates: seen often grouped %.0f%% · people %d · wrong %d", seed, 100*float64(rb.oftenGrouped)/float64(rb.often), rb.people, rb.wrong)
		if float64(rb.oftenGrouped)/float64(rb.often) > share-0.3 {
			t.Fatalf("seed %d: fixed gates did as well", seed)
		}
	}
}

// Stored faces grouped under the old rules are grouped again under the new
// ones (the rules version changed), and end as well as when found anew.
func TestIxFaceLowQualityWorldRegroup(t *testing.T) {
	w := ixLQMake(4)
	a, ctx := ix8App(t, map[string]any{})
	var faces []ix10Face
	who := map[int64]int{}
	for i, f := range w.faces {
		q, _ := ixQuantise(f.vec)
		v := map[int]float64{}
		for d, x := range q {
			v[d] = float64(x)
		}
		id := int64(i + 1)
		who[id] = f.who
		faces = append(faces, ix10Face{id: id, key: f.key, vec: v, score: f.score, px: f.px, blur: f.blur})
	}
	ix10Put(t, a, nil, faces)
	if err := a.ixFaceMetaSet(ctx, "rules", "ix10-1"); err != nil {
		t.Fatal(err)
	}
	if err := a.ixFaceRegroupRun(ctx); err != nil {
		t.Fatal(err)
	}
	r := ixLQJudge(t, a, w, who)
	share := float64(r.oftenGrouped) / float64(r.often)
	t.Logf("regrouped: seen often %d grouped %.0f%% · people %d", r.often, 100*share, r.people)
	if a.ixFaceMetaGet(ctx, "rules") != ixFaceRulesVersion || r.wrong != 0 || r.split != 0 || share < 0.75 {
		t.Fatalf("regrouped: rules %q, %d wrong, %d split, %.0f%% grouped", a.ixFaceMetaGet(ctx, "rules"), r.wrong, r.split, 100*share)
	}
}

func TestIxFaceGatesFollowTheArchive(t *testing.T) {
	spread := func(n int, lo, hi float64) []float32 {
		out := make([]float32, n)
		for i := range out {
			out[i] = float32(lo + (hi-lo)*float64(i)/float64(n-1))
		}
		return out
	}
	// Clear faces: the usual gates.
	if g := ixFaceGatesFrom(spread(300, 0.85, 0.99), spread(300, 100, 400), spread(300, 80, 900)); g != ixFaceGatesFixed {
		t.Fatalf("clear archive: %+v", g)
	}
	// Too few faces to tell: the usual gates.
	if g := ixFaceGatesFrom(spread(150, 0.7, 0.9), spread(150, 40, 90), spread(150, 5, 60)); g != ixFaceGatesFixed {
		t.Fatalf("few faces: %+v", g)
	}
	// Poor faces: the 20th percentile, within limits.
	if g := ixFaceGatesFrom(spread(500, 0.7, 0.95), spread(500, 50, 150), spread(500, 0, 200)); g != (ixFaceGates{Score: 0.75, Px: 69, Blur: 39}) {
		t.Fatalf("poor archive: %+v", g)
	}
	if g := ixFaceGatesFrom(spread(500, 0.7, 0.8), spread(500, 20, 60), spread(500, 0, 40)); g != (ixFaceGates{Score: 0.75, Px: 64, Blur: 15}) {
		t.Fatalf("very poor archive: %+v", g)
	}
	// Unknown sizes and sharpness are left out.
	if g := ixFaceGatesFrom(spread(300, 0.85, 0.99), make([]float32, 300), spread(300, -2, -1)); g != ixFaceGatesFixed {
		t.Fatalf("unknown: %+v", g)
	}
}

// Why faces are loose, in numbers only.
func TestIxFaceLooseBreakdown(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{})
	e := func(d int) map[int]float64 { return map[int]float64{d: 1} }
	like := func(w float64) map[int]float64 { return map[int]float64{1: w, 20: math.Sqrt(1 - w*w)} }
	ix10Put(t, a, map[int64]string{1: ""}, []ix10Face{
		{id: 1, key: "k1", person: 1, vec: e(1)}, // a person
		{id: 2, key: "k2", person: 1, vec: e(1)},
		{id: 3, key: "k3", person: 1, vec: e(1)},
		{id: 4, key: "k4", vec: e(1), px: 30, blur: 300},                            // small
		{id: 5, key: "k5", vec: e(1), px: 180, blur: 5},                             // blurry
		{id: 6, key: "k6", vec: like(0.55), score: 0.75},                            // weak (score), 0.55 alike to the person
		{id: 7, key: "k7", vec: e(7), px: 60, blur: 30},                             // weak (size, sharpness), unlike anyone
		{id: 8, key: "k8", vec: e(8)},                                               // good, alone
		{id: 9, key: "k9", vec: e(9)},                                               // good, one look-alike in another file
		{id: 10, key: "k10", vec: e(9)},                                             //
		{id: 11, key: "k11", vec: like(0.9)},                                        // good, alike to the person
		{id: 12, key: "k12", vec: map[int]float64{9: 0.55, 30: 0.835}, score: 0.72}, // weak, 0.55 alike to 9 and 10: too little to count
	})
	l := a.ixFaceLooseNow(ctx, time.Minute, 10*time.Second)
	if l == nil {
		t.Fatal("not worked out")
	}
	want := ixFaceLoose{Faces: 12, Loose: 9, Small: 1, Blurry: 1, Weak: 3, WeakScore: 2, WeakSize: 1, WeakSharp: 1, WeakNear: 1, Good: 4, Alone: 1, Few: 2, Ready: 1}
	got := *l
	got.Score, got.Px, got.Blur, got.Gates = [3]float64{}, [3]float64{}, [3]float64{}, ixFaceGates{}
	if got != want {
		t.Fatalf("breakdown\n got %+v\nwant %+v", got, want)
	}
	if l.Gates != ixFaceGatesFixed || l.Px != [3]float64{60, 180, 180} || l.Blur != [3]float64{30, 300, 300} || l.Score != [3]float64{0.75, 0.95, 0.95} {
		t.Fatalf("spread %+v %+v %+v gates %+v", l.Score, l.Px, l.Blur, l.Gates)
	}
	// The report has it, as numbers only.
	rep := a.ixDiagnostics(ctx)
	for _, s := range []string{
		"people 1 · faces 12 (9 not grouped)",
		"  not grouped 9: unusable 2 (small 1, blurry 1) · weak 3 (score 2, size 1, sharpness 1; 1 only 0.5-0.6 alike to a person) · good 4 (alone 1, look-alikes in <3 files 2, could group now 1)",
		"  all 12 faces p10/p50/p90: score 0.75/0.95/0.95 · box 60/180/180 px · sharpness 30/300/300 · start gates now: score 0.80, box 80 px, sharpness 50",
	} {
		if !strings.Contains(rep, s+"\n") {
			t.Fatalf("report lacks %q:\n%s", s, rep)
		}
	}
	// And the People tab.
	rec := httptest.NewRecorder()
	a.handleIxPeople(rec, httptest.NewRequest("GET", "/api/ix/browse/people", nil))
	var body struct {
		Why *ixFaceLoose `json:"why"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Why == nil || body.Why.Few != 2 || body.Why.Gates.Blur != 50 {
		t.Fatalf("people tab: %v %s", err, rec.Body.String())
	}
}
