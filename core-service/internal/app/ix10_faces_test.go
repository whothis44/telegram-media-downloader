package app

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"math/rand"
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
	if len(p) != 1 || p[0].Faces != 4 {
		t.Fatalf("H: %+v", p)
	}
}
