package app

import (
	"context"
	"database/sql"
	"fmt"
	"image"
	"math"
	"strconv"
)

// Face quality. Self-filmed content (front camera, webcam, low light,
// motion blur, filters) gives many poor faces, and poor faces look a bit
// like everyone: they split one person into several and pull different
// people together. So, as in Immich, PhotoPrism and Ente, faces come in
// three kinds:
//   - good faces may start a person and join one at the usual likeness;
//   - weak faces may only join an existing person, and only when they are
//     clearly alike (a stricter likeness, as Ente does for poor faces);
//   - unusable faces (tiny or very blurry) are kept but never grouped.
//
// The numbers follow the research notes for this model (buffalo_l):
// detection score 0.8 is Ente's "minimum quality"; 80 px is the lower end
// of PhotoPrism's cluster size for a model that reads faces at 112 px;
// blur is the variance of the Laplacian on a 112 px grey face, with Ente's
// thresholds (soft 50, hard 10).
// The three "start a person" numbers are the usual gates; an archive of
// poorer faces gets lower ones, within limits (ix_faces_gates.go).
const (
	ixFaceCoreScore = 0.8  // detection score a face needs to start a person
	ixFaceCorePx    = 80.0 // short side of the face box, in pixels of the picture
	ixFaceCoreBlur  = 50.0 // sharpness (Laplacian variance) a face needs to start a person
	ixFaceJunkPx    = 40.0 // smaller than this: never grouped
	ixFaceJunkBlur  = 10.0 // blurrier than this: never grouped
	ixFaceWeakSim   = 0.6  // likeness a weak face needs to join (good faces: ixFaceSameSim)

	ixFaceBlurSide    = 112 // faces are measured at this size, like the model reads them
	ixFaceBlurUnknown = -2  // could not be measured (no picture); -1 in the table: not measured yet
)

// Kinds of face.
const (
	ixFaceGood int8 = iota
	ixFaceWeak
	ixFaceJunk
)

// ixFaceTierOf says what kind a face is under the usual (fixed) gates;
// the store uses the gates that follow the archive (ix_faces_gates.go). A
// size or sharpness that is not known (0 / below zero) does not count
// against the face: faces stored before these were measured are judged by
// their score alone.
func ixFaceTierOf(score, px, blur float64) int8 { return ixFaceGatesFixed.tierOf(score, px, blur) }

// ixFaceJunkSQL is ixFaceTierOf's "unusable" as SQL on ix_faces.
var ixFaceJunkSQL = fmt.Sprintf(`((px>0 AND px<%g) OR (blur>=0 AND blur<%g))`, ixFaceJunkPx, ixFaceJunkBlur)

// ixFaceQuality ranks faces for the cover picture: score × size × sharpness,
// each at most 1 (112 px and Ente's "very soft" blur 200 count as full
// marks; an unknown one counts half).
func ixFaceQuality(score, px, blur float64) float32 {
	size, sharp := 0.5, 0.5
	if px > 0 {
		size = math.Min(1, px/ixFaceBlurSide)
	}
	if blur >= 0 {
		sharp = math.Min(1, blur/200)
	}
	return float32(score * size * sharp)
}

// ixFacePairSim is the likeness two faces need to be the same person: the
// stricter one when either face is weak, so a poor face can neither join
// loosely nor let others join through it.
func ixFacePairSim(a, b int8) float32 {
	if a != ixFaceGood || b != ixFaceGood {
		return ixFaceWeakSim
	}
	return ixFaceSameSim
}

// ixFaceUnitOf maps a file key to what counts as one sighting for the
// "enough faces" rule and for the people's file counts: the file's cluster
// (a file and all its copies, including copies that were the best copy
// once), or the key itself when the browser has not seen it.
func (a *App) ixFaceUnitOf() func(string) string {
	m := a.ixModelLast()
	return func(k string) string {
		if m != nil {
			if ii, ok := m.ByKey[k]; ok && int(ii) < len(m.Items) {
				if c := m.Items[ii].Cluster; c >= 0 {
					return "c" + strconv.Itoa(int(c))
				}
			}
		}
		return k
	}
}

// ixFaceRect is a face's box in the picture.
func ixFaceRect(b image.Rectangle, f ixFaceFound) image.Rectangle {
	w, h := float64(b.Dx()), float64(b.Dy())
	return image.Rect(int(f.X1*w), int(f.Y1*h), int(math.Ceil(f.X2*w)), int(math.Ceil(f.Y2*h))).Add(b.Min).Intersect(b)
}

// ixFaceSharpness is the variance of the Laplacian of the face, made grey
// and 112 px square: low means blurred. Pure Go, about 12k samples.
func ixFaceSharpness(img image.Image, r image.Rectangle) float64 {
	r = r.Intersect(img.Bounds())
	if r.Dx() < 4 || r.Dy() < 4 {
		return ixFaceBlurUnknown
	}
	ycc, isYCC := img.(*image.YCbCr)
	gray, isGray := img.(*image.Gray)
	lum := func(x, y int) float64 {
		switch {
		case isYCC:
			return float64(ycc.Y[ycc.YOffset(x, y)])
		case isGray:
			return float64(gray.Pix[gray.PixOffset(x, y)])
		}
		cr, cg, cb, _ := img.At(x, y).RGBA()
		return (0.299*float64(cr) + 0.587*float64(cg) + 0.114*float64(cb)) / 257
	}
	const n = ixFaceBlurSide
	g := make([]float64, n*n)
	w, h := r.Dx(), r.Dy()
	sx, sy := float64(w)/n, float64(h)/n
	for ty := 0; ty < n; ty++ {
		for tx := 0; tx < n; tx++ {
			var v float64
			if sx <= 1 || sy <= 1 {
				// Smaller than 112 px: read between pixels (bilinear), so
				// enlarging adds no edges of its own.
				fx := math.Max(0, (float64(tx)+0.5)*sx-0.5)
				fy := math.Max(0, (float64(ty)+0.5)*sy-0.5)
				x0, y0 := int(fx), int(fy)
				x1, y1 := min(x0+1, w-1), min(y0+1, h-1)
				ax, ay := fx-float64(x0), fy-float64(y0)
				top := lum(r.Min.X+x0, r.Min.Y+y0)*(1-ax) + lum(r.Min.X+x1, r.Min.Y+y0)*ax
				bot := lum(r.Min.X+x0, r.Min.Y+y1)*(1-ax) + lum(r.Min.X+x1, r.Min.Y+y1)*ax
				v = top*(1-ay) + bot*ay
			} else {
				// Larger: the mean of a few points of its block.
				x0, y0 := r.Min.X+tx*w/n, r.Min.Y+ty*h/n
				x1, y1 := max(x0+1, r.Min.X+(tx+1)*w/n), max(y0+1, r.Min.Y+(ty+1)*h/n)
				stepX, stepY := max(1, (x1-x0)/3), max(1, (y1-y0)/3)
				c := 0
				for y := y0; y < y1; y += stepY {
					for x := x0; x < x1; x += stepX {
						v += lum(x, y)
						c++
					}
				}
				v /= float64(c)
			}
			g[ty*n+tx] = v
		}
	}
	var sum, sq float64
	cnt := 0
	for y := 1; y < n-1; y++ {
		for x := 1; x < n-1; x++ {
			l := g[(y-1)*n+x] + g[(y+1)*n+x] + g[y*n+x-1] + g[y*n+x+1] - 4*g[y*n+x]
			sum += l
			sq += l * l
			cnt++
		}
	}
	mean := sum / float64(cnt)
	return sq/float64(cnt) - mean*mean
}

// ixFaceMeasureCrop measures a face from its stored crop (for faces found
// before size and sharpness were kept). The crop is the face box with 40%
// margin, at most 160 px, so the face is its middle part and its size can
// be told up to about 114 px (enough for the 80 px rule).
func ixFaceMeasureCrop(img image.Image) (px, blur float64) {
	b := img.Bounds()
	side := float64(max(b.Dx(), b.Dy())) / 1.4
	cx, cy := float64(b.Min.X+b.Max.X)/2, float64(b.Min.Y+b.Max.Y)/2
	r := image.Rect(int(cx-side/2), int(cy-side/2), int(cx+side/2), int(cy+side/2))
	return side, ixFaceSharpness(img, r)
}

// ---------- schema ----------

// ixFaceSchema: faces a person took out of a person (never put back with
// them), pairs of people suggested as one or said to be different, and a
// little state for grouping again.
const ixFaceSchema = `
CREATE TABLE IF NOT EXISTS ix_face_reject (
	face   INTEGER NOT NULL,
	person INTEGER NOT NULL, -- as it was then; a person merged since is the one merged into
	at     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (face, person)
);
CREATE TABLE IF NOT EXISTS ix_person_pair (
	a     INTEGER NOT NULL, -- a < b
	b     INTEGER NOT NULL,
	sim   REAL    NOT NULL DEFAULT 0,
	state TEXT    NOT NULL DEFAULT 'ask', -- ask: "same person?"; no: a person said they are different
	at    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (a, b)
);
CREATE TABLE IF NOT EXISTS ix_face_prev (
	face   INTEGER PRIMARY KEY, -- while grouping again: the person a face was with
	person INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS ix_face_meta (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL DEFAULT ''
);
`

// ixEnsureFaceSchema adds the tables and columns of face quality, people
// suggestions and chosen covers.
func ixEnsureFaceSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, ixFaceSchema); err != nil {
		return err
	}
	if err := ixAddColumns(ctx, db, "ix_faces", [][2]string{
		{"frame", "INTEGER NOT NULL DEFAULT 0"}, // video frame the face is in (0 for a picture)
		{"px", "REAL NOT NULL DEFAULT 0"},       // short side of the box in pixels; 0: not known
		{"blur", "REAL NOT NULL DEFAULT -1"},    // sharpness; -1: not measured yet, -2: could not be
	}); err != nil {
		return err
	}
	return ixAddColumns(ctx, db, "ix_people", [][2]string{
		{"cover", "INTEGER NOT NULL DEFAULT 0"},      // face a person chose for the picture
		{"auto_cover", "INTEGER NOT NULL DEFAULT 0"}, // clearest face near the middle of the person's faces
	})
}

func (a *App) ixFaceMetaGet(ctx context.Context, k string) string {
	var v string
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT v FROM ix_face_meta WHERE k=?`, k).Scan(&v)
	return v
}

func (a *App) ixFaceMetaSet(ctx context.Context, k, v string) error {
	_, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_face_meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}
