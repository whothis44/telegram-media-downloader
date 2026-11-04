package app

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"math/rand"
	"strings"
	"sync"
	"testing"
)

// Face grouping with quality, cannot-links, suggestions and covers. All
// pictures and faces here are invented: the fake service (ix8FakeML)
// reads what a picture "shows" from the text after it.

var ix10Pics struct {
	once        sync.Once
	sharp, flat []byte
}

// ix10FacePicture is a 640 px grey picture with fine detail (faces in it
// measure as sharp); ix10FlatPicture is one without (they measure as blurred).
func ix10FacePicture() []byte { ix10MakePictures(); return ix10Pics.sharp }
func ix10FlatPicture() []byte { ix10MakePictures(); return ix10Pics.flat }

func ix10MakePictures() {
	ix10Pics.once.Do(func() {
		r := rand.New(rand.NewSource(1))
		sharp, flat := image.NewGray(image.Rect(0, 0, 640, 640)), image.NewGray(image.Rect(0, 0, 640, 640))
		for i := range sharp.Pix {
			sharp.Pix[i] = uint8(r.Intn(256))
			flat.Pix[i] = 128
		}
		var a, b bytes.Buffer
		_ = jpeg.Encode(&a, sharp, &jpeg.Options{Quality: 90})
		_ = jpeg.Encode(&b, flat, &jpeg.Options{Quality: 90})
		// Full capacity: appending a seed always copies.
		ix10Pics.sharp, ix10Pics.flat = a.Bytes()[:a.Len():a.Len()], b.Bytes()[:b.Len():b.Len()]
	})
}

// ix10Scan scans the given files (key -> what the fake service sees in it;
// several strings are several video frames) as the faces task does.
func ix10Scan(t *testing.T, a *App, ctx context.Context, files map[string][]string, flat map[string]bool) {
	t.Helper()
	m := &ixModel{ByKey: map[string]int32{}}
	for k := range files {
		m.ByKey[k] = int32(len(m.Items))
		m.Items = append(m.Items, ixMItem{Key: k, Type: "photo", Path: "x/" + k + ".jpg", Cluster: int32(len(m.Clusters))})
		m.Clusters = append(m.Clusters, ixMCluster{Canon: int32(len(m.Items) - 1)})
	}
	old := ixMLFullPictureHook
	defer func() { ixMLFullPictureHook = old }()
	ixMLFullPictureHook = func(_ *App, _ context.Context, it *ixMItem) ([][]byte, error) {
		var out [][]byte
		for _, seed := range files[it.Key] {
			pic := ix10FacePicture()
			if flat[it.Key] {
				pic = ix10FlatPicture()
			}
			out = append(out, append(append([]byte(nil), pic...), []byte("#faces="+seed)...))
		}
		return out, nil
	}
	ft := a.ixFaceTask(ctx, a.ixMLClientNow(ctx))
	for _, it := range m.Items {
		it := it
		if err := ft.run(ctx, &it); err != nil {
			t.Fatalf("scan %s: %v", it.Key, err)
		}
	}
}

func ix10Faces(t *testing.T, a *App, key string) []struct{ ID, Person int64 } {
	t.Helper()
	var out []struct{ ID, Person int64 }
	rows, err := a.db.Reader.Query(`SELECT id, person FROM ix_faces WHERE key=? ORDER BY id`, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var f struct{ ID, Person int64 }
		_ = rows.Scan(&f.ID, &f.Person)
		out = append(out, f)
	}
	return out
}

func ix10People(t *testing.T, a *App, ctx context.Context) []ixPerson {
	t.Helper()
	p, err := a.ixPeople(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIxFaceQualityGate(t *testing.T) {
	srv := ix8FakeML(t)
	defer srv.Close()
	a, ctx := ix8App(t, map[string]any{"mlUrl": srv.URL, "mlFaces": true})
	// Sharpness: fine detail measures sharp, a flat picture blurred.
	img, _ := jpeg.Decode(bytes.NewReader(ix10FacePicture()))
	flat, _ := jpeg.Decode(bytes.NewReader(ix10FlatPicture()))
	if s := ixFaceSharpness(img, image.Rect(0, 0, 200, 240)); s < ixFaceCoreBlur {
		t.Fatalf("detail measured as blurred: %.1f", s)
	}
	if s := ixFaceSharpness(flat, image.Rect(0, 0, 200, 240)); s >= ixFaceJunkBlur {
		t.Fatalf("flat measured as sharp: %.1f", s)
	}
	if ixFaceTierOf(0.95, 180, 300) != ixFaceGood || ixFaceTierOf(0.75, 180, 300) != ixFaceWeak ||
		ixFaceTierOf(0.95, 60, 300) != ixFaceWeak || ixFaceTierOf(0.95, 180, 30) != ixFaceWeak ||
		ixFaceTierOf(0.95, 30, 300) != ixFaceJunk || ixFaceTierOf(0.95, 180, 5) != ixFaceJunk || ixFaceTierOf(0.95, 0, -1) != ixFaceGood {
		t.Fatal("face kinds")
	}
	// Three weak faces of F (low score, small, low score): no person.
	ix10Scan(t, a, ctx, map[string][]string{"f1": {"F:0.75"}, "f2": {"F:0.95:60"}, "f3": {"F:0.72"}}, nil)
	if p := ix10People(t, a, ctx); len(p) != 0 {
		t.Fatalf("weak faces started a person: %+v", p)
	}
	// Blurred faces are never grouped at all.
	ix10Scan(t, a, ctx, map[string][]string{"g1": {"G"}, "g2": {"G"}, "g3": {"G"}}, map[string]bool{"g1": true, "g2": true, "g3": true})
	if p := ix10People(t, a, ctx); len(p) != 0 {
		t.Fatalf("blurred faces started a person: %+v", p)
	}
	// Three good faces of F do, and the clearly alike weak ones join.
	ix10Scan(t, a, ctx, map[string][]string{"f4": {"F"}, "f5": {"F"}, "f6": {"F"}}, nil)
	p := ix10People(t, a, ctx)
	if len(p) != 1 || p[0].Files != 6 {
		t.Fatalf("F: %+v", p)
	}
	// A weak face only somewhat alike (0.57) does not join; a good one does.
	ix10Scan(t, a, ctx, map[string][]string{"f7": {"F*0.55:0.75"}, "f8": {"F*0.55"}}, nil)
	if f := ix10Faces(t, a, "f7"); f[0].Person != 0 {
		t.Fatal("a weak face joined at the usual likeness")
	}
	if f := ix10Faces(t, a, "f8"); f[0].Person != p[0].ID {
		t.Fatal("a good face did not join at the usual likeness")
	}
}

func TestIxFacesCountFilesNotFramesOrCopies(t *testing.T) {
	srv := ix8FakeML(t)
	defer srv.Close()
	a, ctx := ix8App(t, map[string]any{"mlUrl": srv.URL, "mlFaces": true})
	// h1 and h2 are copies of one picture (one cluster; h1 was its best
	// copy once), v1 is a video with H in all three frames.
	m := &ixModel{ByKey: map[string]int32{"h1": 0, "h2": 1, "v1": 2}, Items: []ixMItem{{Key: "h1", Cluster: 0}, {Key: "h2", Cluster: 0}, {Key: "v1", Cluster: 1}},
		Clusters: []ixMCluster{{Canon: 1, Items: []int32{0, 1}}, {Canon: 2, Items: []int32{2}}}}
	a.ixModelBox().model = m
	ix10Scan(t, a, ctx, map[string][]string{"h1": {"H"}, "h2": {"H"}, "v1": {"H", "H", "H"}}, nil)
	if len(ix10Faces(t, a, "v1")) != 1 {
		t.Fatal("one person in three frames is not one face")
	}
	if p := ix10People(t, a, ctx); len(p) != 0 {
		t.Fatalf("two files made a person: %+v", p)
	}
	ix10Scan(t, a, ctx, map[string][]string{"h3": {"H"}}, nil)
	p := ix10People(t, a, ctx)
	if len(p) != 1 || p[0].Faces != 4 || p[0].Files != 3 { // h1 and h2 are one file
		t.Fatalf("H: %+v", p)
	}
	// Faces kept only under the old best copy still find the file.
	_, _ = a.db.Writer.Exec(`DELETE FROM ix_faces WHERE key='h2'`)
	a.ixLoadPeople(ctx, m)
	if keys := m.PersonKeys[p[0].ID]; !keys["h1"] || !keys["h2"] {
		t.Fatalf("person keys: %v", keys)
	}
}

// ix10Vec is a unit embedding with the given parts (dimension -> weight).
func ix10Vec(parts map[int]float64) []byte {
	v := make([]float32, 512)
	var n float64
	for d, w := range parts {
		v[d] = float32(w)
		n += w * w
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(n))
	}
	q, _ := ixQuantise(v)
	b := make([]byte, len(q))
	for i, x := range q {
		b[i] = byte(x)
	}
	return b
}

// ix10Put stores people and their faces directly, as an older version
// (or earlier grouping) left them. Call before the faces are first loaded.
func ix10Put(t *testing.T, a *App, people map[int64]string, faces []ix10Face) {
	t.Helper()
	for id, name := range people {
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_people(id,name,created_at,updated_at) VALUES(?,?,1,1)`, id, name); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range faces {
		if f.score == 0 {
			f.score = 0.95
		}
		if f.px == 0 && f.blur == 0 {
			f.px, f.blur = 180, 300
		}
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_faces(id,key,model,x1,y1,x2,y2,score,vec,person,at,frame,px,blur) VALUES(?,?,?,0.1,0.1,0.4,0.5,?,?,?,1,0,?,?)`,
			f.id, f.key, ixFaceModel, f.score, ix10Vec(f.vec), f.person, f.px, f.blur); err != nil {
			t.Fatal(err)
		}
	}
}

type ix10Face struct {
	id     int64
	key    string
	person int64
	vec    map[int]float64
	score  float64
	px     float64
	blur   float64
}

func TestIxPeopleSuggestionsAndMerges(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{})
	q := func(o int, w float64) map[int]float64 { return map[int]float64{1: w, o: math.Sqrt(1 - w*w)} }
	var faces []ix10Face
	id := int64(0)
	add := func(person int64, vec map[int]float64, keys ...string) {
		for _, k := range keys {
			id++
			faces = append(faces, ix10Face{id: id, key: k, person: person, vec: vec})
		}
	}
	add(1, q(2, 1), "k1", "k2", "k3", "k4")                       // the same person as 2: merged unasked
	add(2, q(2, 0.75), "k5", "k6")                                // 0.75 alike to 1
	add(3, q(3, 0.45), "k7", "k8", "k9")                          // 0.45: "same person?"
	add(4, q(4, 0.75), "k1", "k10", "k11")                        // as alike as 2, but seen beside 1 in k1
	add(5, map[int]float64{9: 1}, "k12", "k13", "k14")            // Zed
	add(6, map[int]float64{9: 0.8, 10: 0.6}, "k15", "k16", "k17") // Zoe: alike, but both named
	ix10Put(t, a, map[int64]string{1: "", 2: "", 3: "", 4: "", 5: "Zed", 6: "Zoe"}, faces)
	if err := a.ixFacePeoplePass(ctx); err != nil {
		t.Fatal(err)
	}
	merged := func(id int64) int64 {
		var into int64
		_ = a.db.Reader.QueryRow(`SELECT merged_into FROM ix_people WHERE id=?`, id).Scan(&into)
		return into
	}
	if merged(2) != 1 || merged(4) != 0 || merged(5) != 0 || merged(6) != 0 {
		t.Fatalf("merged: 2→%d 4→%d 5→%d 6→%d", merged(2), merged(4), merged(5), merged(6))
	}
	pairs := func() string {
		var out []string
		for _, p := range a.ixPeopleSuggestions(ctx, ix10People(t, a, ctx), 12) {
			if len(p.AFaces) == 0 || len(p.BFaces) == 0 {
				t.Fatalf("suggestion without faces: %+v", p)
			}
			out = append(out, fmt.Sprintf("%d-%d", p.A, p.B))
		}
		return strings.Join(out, ",")
	}
	if got := pairs(); got != "5-6,1-3" {
		t.Fatalf("suggestions: %s", got)
	}
	// "Not the same" is remembered, also after the next pass.
	if err := a.ixPersonChange(ctx, 3, "not-same", "", 1, 0); err != nil {
		t.Fatal(err)
	}
	a.ixFacePeoplePass(ctx)
	if got := pairs(); got != "5-6" {
		t.Fatalf("after no: %s", got)
	}
	// ... and after 1 is merged into someone else.
	if err := a.ixPersonChange(ctx, 1, "merge", "", 4, 0); err != nil {
		t.Fatal(err)
	}
	a.ixFacePeoplePass(ctx)
	if got := pairs(); got != "5-6" {
		t.Fatalf("after merging 1: %s", got)
	}
	// "Same person": the usual merge; the suggestion goes.
	if err := a.ixPersonChange(ctx, 6, "merge", "", 5, 0); err != nil {
		t.Fatal(err)
	}
	if got := pairs(); got != "" {
		t.Fatalf("after yes: %s", got)
	}
}

func TestIxPersonCoverIsClearFaceOrChosen(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{})
	// Ten faces near the middle (face 2 the clearest of them), and an
	// even clearer one far from it, which is not the person's typical look.
	e := map[int]float64{1: 1}
	var faces []ix10Face
	for i := int64(1); i <= 10; i++ {
		faces = append(faces, ix10Face{id: i, key: fmt.Sprint("k", i), person: 1, vec: e, score: 0.85, px: 100, blur: 80})
	}
	faces[1] = ix10Face{id: 2, key: "k2", person: 1, vec: e, score: 0.97, px: 200, blur: 400}
	faces = append(faces, ix10Face{id: 11, key: "k11", person: 1, vec: map[int]float64{1: 0.8, 2: 0.6}, score: 0.99, px: 300, blur: 500})
	ix10Put(t, a, map[int64]string{1: ""}, faces)
	cover := func() int64 { return ix10People(t, a, ctx)[0].Cover }
	if err := a.ixFacePeoplePass(ctx); err != nil {
		t.Fatal(err)
	}
	if c := cover(); c != 2 {
		t.Fatalf("cover %d", c)
	}
	// A face chosen by a person wins, until it is taken out.
	if err := a.ixPersonChange(ctx, 1, "cover", "", 0, 3); err != nil {
		t.Fatal(err)
	}
	if c := cover(); c != 3 || a.ixPersonCover(ctx, 1) != 3 {
		t.Fatalf("chosen cover %d", c)
	}
	if err := a.ixPersonChange(ctx, 1, "cover", "", 0, 99); err == nil {
		t.Fatal("chose a face that is not the person's")
	}
	if err := a.ixPersonChange(ctx, 1, "remove-face", "", 0, 3); err != nil {
		t.Fatal(err)
	}
	if c := cover(); c != 2 {
		t.Fatalf("cover after taking the chosen face out: %d", c)
	}
}

func TestIxFaceRegroupKeepsDecisions(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{})
	e := func(d int) map[int]float64 { return map[int]float64{d: 1} }
	var faces []ix10Face
	put := func(id int64, key string, person int64, vec map[int]float64, score float64) {
		faces = append(faces, ix10Face{id: id, key: key, person: person, vec: vec, score: score, blur: -1}) // as stored before: not measured
	}
	for i := int64(1); i <= 4; i++ {
		put(i, fmt.Sprint("ka", i), 1, e(1), 0.95) // Ann (named)
	}
	put(5, "ka5", 1, e(1), 0.95)  // two faces of one picture in Ann
	put(6, "ka5", 1, e(1), 0.95)  //
	put(7, "kx", 1, e(5), 0.72)   // a poor face of someone else dragged into Ann
	put(8, "kb1", 2, e(2), 0.95)  // person 2 (no decisions)
	put(9, "kb2", 2, e(2), 0.95)  //
	put(10, "kb3", 2, e(2), 0.95) //
	put(11, "kj", 2, e(2), 0.95)  // blurred (its crop is flat)
	put(12, "kc1", 3, e(3), 0.95) // person 3, hidden
	put(13, "kc2", 3, e(3), 0.95)
	put(14, "kc3", 3, e(3), 0.95)
	put(15, "kd1", 4, e(4), 0.95) // person 4: two files only
	put(16, "kd2", 4, e(4), 0.95)
	put(17, "kr", -1, e(1), 0.95) // taken out under the old rule
	put(18, "krr", 0, e(2), 0.95) // taken out of person 2
	ix10Put(t, a, map[int64]string{1: "Ann", 2: "", 3: "", 4: "", 5: ""}, faces)
	for _, q := range []string{`UPDATE ix_people SET hidden=1 WHERE id=3`, `UPDATE ix_people SET merged_into=1 WHERE id=5`,
		`UPDATE ix_faces SET manual=1 WHERE id=17`, `INSERT INTO ix_face_reject(face,person) VALUES(18,2)`} {
		if _, err := a.db.Writer.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	sharp, _ := jpeg.Decode(bytes.NewReader(ix10FacePicture()))
	flat, _ := jpeg.Decode(bytes.NewReader(ix10FlatPicture()))
	box := ixFaceFound{X1: 0.1, Y1: 0.1, X2: 0.4, Y2: 0.5}
	a.ixWriteFaceCrop(1, sharp, box)
	a.ixWriteFaceCrop(11, flat, box)

	check := func(round string) {
		t.Helper()
		person := map[int64]int64{}
		rows, _ := a.db.Reader.Query(`SELECT id, person FROM ix_faces`)
		for rows.Next() {
			var id, p int64
			_ = rows.Scan(&id, &p)
			person[id] = p
		}
		rows.Close()
		want := map[int64]int64{1: 1, 2: 1, 3: 1, 4: 1, 7: 0, 8: 2, 9: 2, 10: 2, 11: 0, 12: 3, 13: 3, 14: 3, 15: 0, 16: 0, 17: -1, 18: 0}
		for id, p := range want {
			if person[id] != p {
				t.Fatalf("%s: face %d is with %d, not %d (%v)", round, id, person[id], p, person)
			}
		}
		if (person[5] == 1) == (person[6] == 1) || person[5]+person[6] != 1 {
			t.Fatalf("%s: faces of one picture: %d %d", round, person[5], person[6])
		}
		var name string
		var hidden bool
		_ = a.db.Reader.QueryRow(`SELECT name FROM ix_people WHERE id=1`).Scan(&name)
		_ = a.db.Reader.QueryRow(`SELECT hidden FROM ix_people WHERE id=3`).Scan(&hidden)
		if name != "Ann" || !hidden {
			t.Fatalf("%s: decisions lost: %q %v", round, name, hidden)
		}
		if a.ixFaceMetaGet(ctx, "rules") != ixFaceRulesVersion || a.ixFaceMetaGet(ctx, "regroup") != "" {
			t.Fatalf("%s: not finished", round)
		}
	}
	if err := a.ixFaceRegroupRun(ctx); err != nil {
		t.Fatal(err)
	}
	check("after the update")
	// Faces were measured from their crops (or left unknown without one).
	var px1, blur1, blur11, blur8 float64
	_ = a.db.Reader.QueryRow(`SELECT px, blur FROM ix_faces WHERE id=1`).Scan(&px1, &blur1)
	_ = a.db.Reader.QueryRow(`SELECT blur FROM ix_faces WHERE id=11`).Scan(&blur11)
	_ = a.db.Reader.QueryRow(`SELECT blur FROM ix_faces WHERE id=8`).Scan(&blur8)
	if px1 < ixFaceCorePx || blur1 < ixFaceCoreBlur || blur11 >= ixFaceJunkBlur || blur8 != ixFaceBlurUnknown {
		t.Fatalf("measured: %.0f %.0f %.0f %.0f", px1, blur1, blur11, blur8)
	}
	// Asked again from the People tab: the same.
	if err := a.ixFaceMetaSet(ctx, "regroup", "pending"); err != nil {
		t.Fatal(err)
	}
	if err := a.ixFaceRegroupRun(ctx); err != nil {
		t.Fatal(err)
	}
	check("asked again")
	// Nothing left to do: no background work is started.
	a.ixFaceRegroupKick(ctx)
	if a.ixPeopleBox().regrouping {
		t.Fatal("started with nothing to do")
	}
}

func TestIxFacesSeenTogetherAreTwoPeople(t *testing.T) {
	srv := ix8FakeML(t)
	defer srv.Close()
	a, ctx := ix8App(t, map[string]any{"mlUrl": srv.URL, "mlFaces": true})
	// b1 shows two very alike faces side by side (twins, or a mirror).
	ix10Scan(t, a, ctx, map[string][]string{"b1": {"L,L"}, "b2": {"L"}}, nil)
	if p := ix10People(t, a, ctx); len(p) != 0 {
		t.Fatalf("two faces of one picture counted as two sightings: %+v", p)
	}
	ix10Scan(t, a, ctx, map[string][]string{"b3": {"L"}}, nil)
	p := ix10People(t, a, ctx)
	if len(p) != 1 || p[0].Faces != 3 {
		t.Fatalf("L: %+v", p)
	}
	f := ix10Faces(t, a, "b1")
	if len(f) != 2 || (f[0].Person == p[0].ID) == (f[1].Person == p[0].ID) {
		t.Fatalf("faces of b1: %+v", f)
	}
	// Later, too: of two alike faces in one picture only one joins.
	ix10Scan(t, a, ctx, map[string][]string{"b4": {"L,L"}}, nil)
	if f := ix10Faces(t, a, "b4"); len(f) != 2 || f[0].Person != p[0].ID || f[1].Person != 0 {
		t.Fatalf("faces of b4: %+v", f)
	}
	// Regrouping does not change that.
	a.ixRegroupFaces(ctx, 100)
	if f := ix10Faces(t, a, "b4"); f[1].Person != 0 {
		t.Fatalf("regrouped into the person beside it: %+v", f)
	}
}
