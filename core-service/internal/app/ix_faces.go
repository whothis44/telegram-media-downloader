package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/seekbar"
)

// Faces and people (optional, off unless switched on). Immich's face model
// finds the faces in each distinct picture (and in three frames of each
// video) and describes each by an embedding; faces whose embeddings are
// close are the same person. They are grouped the way Immich groups them:
// a face joins the person of its closest grouped look-alike, and a new
// person is made once enough faces look alike. A person can then be named,
// merged with another, hidden, and a face taken out of a person.

const (
	ixFaceModel    = "buffalo_l"
	ixFaceMinScore = 0.7 // detection confidence
	ixFaceSameSim  = 0.5 // cosine at or above which two faces look alike (Immich: max distance 0.5)
	ixFaceMinFaces = 3   // look-alike faces needed to make a new person
)

// ---------- client ----------

type ixFaceFound struct {
	X1, Y1, X2, Y2 float64 // share of the picture's width and height
	Px             float64 // short side of the box, in pixels of the picture
	Score          float64
	Vec            []float32
}

func (c *ixMLClient) faces(ctx context.Context, pic []byte) ([]ixFaceFound, error) {
	raw, err := c.predict(ctx, map[string]any{"facial-recognition": map[string]any{
		"detection":   map[string]any{"modelName": ixFaceModel, "options": map[string]any{"minScore": ixFaceMinScore}},
		"recognition": map[string]any{"modelName": ixFaceModel, "options": map[string]any{}},
	}}, pic, "")
	if err != nil {
		return nil, err
	}
	return ixParseFaces(raw)
}

func ixParseFaces(raw []byte) ([]ixFaceFound, error) {
	var resp struct {
		Faces []struct {
			Box struct {
				X1, Y1, X2, Y2 float64
			} `json:"boundingBox"`
			Embedding json.RawMessage `json:"embedding"`
			Score     float64         `json:"score"`
		} `json:"facial-recognition"`
		W float64 `json:"imageWidth"`
		H float64 `json:"imageHeight"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("faces: unreadable answer: %v", err)
	}
	if resp.W <= 0 || resp.H <= 0 {
		return nil, errors.New("faces: the answer has no picture size")
	}
	var out []ixFaceFound
	for _, f := range resp.Faces {
		v, err := ixMLParse([]byte(`{"clip":` + string(f.Embedding) + `}`))
		if err != nil {
			return nil, fmt.Errorf("faces: %v", err)
		}
		clamp := func(x float64) float64 { return math.Max(0, math.Min(1, x)) }
		px := math.Max(0, math.Min(math.Min(f.Box.X2, resp.W)-math.Max(f.Box.X1, 0), math.Min(f.Box.Y2, resp.H)-math.Max(f.Box.Y1, 0)))
		out = append(out, ixFaceFound{X1: clamp(f.Box.X1 / resp.W), Y1: clamp(f.Box.Y1 / resp.H), X2: clamp(f.Box.X2 / resp.W), Y2: clamp(f.Box.Y2 / resp.H), Px: px, Score: f.Score, Vec: v})
	}
	return out, nil
}

// ---------- the faces in memory ----------

type ixFaceStore struct {
	group  sync.Mutex // grouping and people changes, one at a time
	mu     sync.RWMutex
	model  string
	dim    int
	ids    []int64
	index  map[int64]int32
	vecs   []int8
	norms  []float32
	person []int64
	key    []int32   // the face's file, an index into keys
	frame  []int16   // the video frame it is in (0 for a picture)
	tier   []int8    // good, weak or unusable (ixFaceTierOf)
	qual   []float32 // for the cover picture (ixFaceQuality)
	keys   []string
	keyIdx map[string]int32
	shots  map[int64][]int32 // one picture or video frame -> its faces
	loaded bool
}

// ixFaceInfo is what is known of a face besides its embedding.
type ixFaceInfo struct {
	key             string
	frame           int
	score, px, blur float64
}

func (s *ixFaceStore) load(ctx context.Context, db *sql.DB, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded && s.model == model {
		return nil
	}
	s.model, s.dim, s.ids, s.vecs, s.norms, s.person = model, 0, nil, nil, nil, nil
	s.key, s.frame, s.tier, s.qual, s.keys = nil, nil, nil, nil, nil
	s.index, s.keyIdx, s.shots = map[int64]int32{}, map[string]int32{}, map[int64][]int32{}
	rows, err := db.QueryContext(ctx, `SELECT id, vec, person, key, frame, score, px, blur FROM ix_faces WHERE model=? ORDER BY id`, model)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, person int64
		var blob []byte
		var fi ixFaceInfo
		if rows.Scan(&id, &blob, &person, &fi.key, &fi.frame, &fi.score, &fi.px, &fi.blur) != nil {
			continue
		}
		s.addLocked(id, ixBytesToInt8(blob), person, fi)
	}
	s.loaded = true
	return rows.Err()
}

func ixBytesToInt8(b []byte) []int8 {
	v := make([]int8, len(b))
	for i, x := range b {
		v[i] = int8(x)
	}
	return v
}

func (s *ixFaceStore) addLocked(id int64, v []int8, person int64, fi ixFaceInfo) {
	if s.dim == 0 {
		s.dim = len(v)
	}
	if len(v) != s.dim {
		return
	}
	if s.index == nil {
		s.index, s.keyIdx, s.shots = map[int64]int32{}, map[string]int32{}, map[int64][]int32{}
	}
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	k, ok := s.keyIdx[fi.key]
	if !ok {
		k = int32(len(s.keys))
		s.keys = append(s.keys, fi.key)
		s.keyIdx[fi.key] = k
	}
	i := int32(len(s.ids))
	s.index[id] = i
	s.ids = append(s.ids, id)
	s.vecs = append(s.vecs, v...)
	s.norms = append(s.norms, float32(math.Sqrt(n)))
	s.person = append(s.person, person)
	s.key = append(s.key, k)
	s.frame = append(s.frame, int16(fi.frame))
	s.tier = append(s.tier, ixFaceTierOf(fi.score, fi.px, fi.blur))
	s.qual = append(s.qual, ixFaceQuality(fi.score, fi.px, fi.blur))
	sh := ixFaceShot(k, int16(fi.frame))
	s.shots[sh] = append(s.shots[sh], i)
}

// ixFaceShot names one picture, or one frame of a video.
func ixFaceShot(key int32, frame int16) int64 { return int64(key)<<16 | int64(uint16(frame)) }

func (s *ixFaceStore) shotLocked(i int32) int64 { return ixFaceShot(s.key[i], s.frame[i]) }

// inShotLocked says whether person has a face (other than skip) in the
// picture or frame: two faces seen together are never the same person.
func (s *ixFaceStore) inShotLocked(person, shot, skip int64) bool {
	for _, i := range s.shots[shot] {
		if s.person[i] == person && s.ids[i] != skip {
			return true
		}
	}
	return false
}

// dropLocked forgets a face deleted from the table.
func (s *ixFaceStore) dropLocked(id int64) {
	if i, ok := s.index[id]; ok {
		s.norms[i], s.person[i], s.tier[i] = 0, 0, ixFaceJunk
	}
}

// setInfoLocked updates what a face was measured as.
func (s *ixFaceStore) setInfoLocked(id int64, score, px, blur float64) {
	if i, ok := s.index[id]; ok {
		s.tier[i], s.qual[i] = ixFaceTierOf(score, px, blur), ixFaceQuality(score, px, blur)
	}
}

type ixFaceNear struct {
	id     int64
	person int64
	sim    float32
	idx    int32
}

// near lists the faces at least min alike to v.
func (s *ixFaceStore) nearLocked(v []int8, min float32, skip int64) []ixFaceNear {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	qn := float32(math.Sqrt(n))
	if qn == 0 || s.dim != len(v) {
		return nil
	}
	var out []ixFaceNear
	d := s.dim
	for i, id := range s.ids {
		if id == skip || s.norms[i] == 0 {
			continue
		}
		c := float32(ixDot8(v, s.vecs[i*d:(i+1)*d])) / (qn * s.norms[i])
		if c >= min {
			out = append(out, ixFaceNear{id, s.person[i], c, int32(i)})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].sim > out[b].sim })
	return out
}

func (s *ixFaceStore) setPersonLocked(id, person int64) {
	if i, ok := s.index[id]; ok {
		s.person[i] = person
	}
}

func (s *ixFaceStore) repoint(from, to int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.person {
		if p == from {
			s.person[i] = to
		}
	}
}

func ixCosF(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var d float32
	for i := range a {
		d += a[i] * b[i]
	}
	return d // both are unit vectors
}

// ---------- the task ----------

func (a *App) ixFaceTask(ctx context.Context, c *ixMLClient) *ixMLTask {
	a.ixMLDoneSet(ctx, "faces", ixFaceModel, `SELECT key FROM ix_face_done WHERE model=?`)
	return &ixMLTask{
		name: "faces", model: ixFaceModel, errTable: "ix_ml_err", par: 3,
		wanted: func(it *ixMItem) bool { return it.Type == "photo" || it.Type == "video" },
		done:   a.ixMLIsDone("faces"),
		run:    func(ctx context.Context, it *ixMItem) error { return a.ixFaceItem(ctx, c, it) },
		test: func(ctx context.Context) error {
			_, err := c.faces(ctx, ixMLTestPicture())
			return err
		},
		note: a.ixMLTaskNoter("faces"),
		finish: func(ctx context.Context) {
			a.ixRegroupFaces(ctx, 300)
			a.ixFacePeopleSoon(ctx)
		},
	}
}

// ixMLFullPictureHook is what a file is read from for faces and text: a
// photo itself, or three frames of a video (at a fifth, half and four
// fifths: self-filmed clips often have the face, or the caption, only part
// of the time). Replaceable in tests.
var ixMLFullPictureHook = func(a *App, ctx context.Context, it *ixMItem) ([][]byte, error) {
	return a.ixMLFullPicture(ctx, it)
}

func (a *App) ixMLFullPicture(ctx context.Context, it *ixMItem) ([][]byte, error) {
	rel := ixNormPath(it.Path)
	if rel == "" {
		return nil, errors.New("bad path")
	}
	full := filepath.Join(a.downloadsDir, filepath.FromSlash(rel))
	if it.Type == "photo" {
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, errors.New("file not found")
		}
		if len(data) > ixMLMaxImageMB<<20 {
			return nil, errors.New("too large")
		}
		return [][]byte{data}, nil
	}
	if _, err := os.Stat(full); err != nil {
		return nil, errors.New("file not found")
	}
	bin := seekbar.FFmpegPath()
	if bin == "" {
		return nil, ixErrNoFFmpeg
	}
	times := []float64{1}
	if it.Dur > 3 {
		times = []float64{it.Dur * 0.2, it.Dur * 0.5, it.Dur * 0.8}
	}
	var out [][]byte
	for _, at := range times {
		fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		cmd := exec.CommandContext(fctx, bin, "-hide_banner", "-nostdin", "-loglevel", "error", "-ss", fmt.Sprintf("%.2f", at), "-i", full,
			"-frames:v", "1", "-an", "-sn", "-dn", "-vf", "scale='min(1280,iw)':-2", "-f", "image2pipe", "-vcodec", "mjpeg", "-q:v", "2", "pipe:1")
		var buf bytes.Buffer
		cmd.Stdout = &buf
		err := cmd.Run()
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil && buf.Len() > 0 {
			out = append(out, buf.Bytes())
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no frame could be read from the video")
	}
	return out, nil
}

func (a *App) ixFaceItem(ctx context.Context, c *ixMLClient, it *ixMItem) error {
	pics, err := ixMLFullPictureHook(a, ctx, it)
	if err != nil {
		return a.ixMLReadErr(ctx, err)
	}
	// Faces of all frames; one person seen in several frames is kept once,
	// by their best face. Two faces in one frame are always kept as two.
	type kept struct {
		f     ixFaceFound
		img   image.Image
		frame int
		blur  float64
		q     float32
	}
	var found []kept
	for fi, pic := range pics {
		fs, err := c.faces(ctx, pic)
		if err != nil {
			return ixMLFileErrOf(ctx, err)
		}
		if len(fs) == 0 {
			continue
		}
		img, _, _ := image.Decode(bytes.NewReader(pic))
	next:
		for _, f := range fs {
			blur := float64(ixFaceBlurUnknown)
			if img != nil {
				blur = ixFaceSharpness(img, ixFaceRect(img.Bounds(), f))
			}
			k := kept{f, img, fi, blur, ixFaceQuality(f.Score, f.Px, blur)}
			for i := range found {
				if found[i].frame != fi && ixCosF(found[i].f.Vec, f.Vec) >= ixFaceSameSim {
					if k.q > found[i].q {
						found[i] = k
					}
					continue next
				}
			}
			found = append(found, k)
		}
	}
	box := a.ixMLBox()
	if err := box.faces.load(ctx, a.db.Reader, ixFaceModel); err != nil {
		return err
	}
	// The file's faces and its "done" mark are written together, so a file
	// stopped part-way is done again from scratch, never twice.
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var gone []int64
	if rows, err := tx.QueryContext(ctx, `SELECT id FROM ix_faces WHERE key=? AND model=? AND manual=0 AND person=0`, it.Key, ixFaceModel); err == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				gone = append(gone, id)
			}
		}
		rows.Close()
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ix_faces WHERE key=? AND model=? AND manual=0 AND person=0`, it.Key, ixFaceModel); err != nil {
		return err
	}
	ids := make([]int64, len(found))
	qs := make([][]int8, len(found))
	for i, k := range found {
		f := k.f
		q, _ := ixQuantise(f.Vec)
		blob := make([]byte, len(q))
		for j, x := range q {
			blob[j] = byte(x)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO ix_faces(key,model,x1,y1,x2,y2,score,vec,person,at,frame,px,blur) VALUES(?,?,?,?,?,?,?,?,0,?,?,?,?)`,
			it.Key, ixFaceModel, f.X1, f.Y1, f.X2, f.Y2, f.Score, blob, ixNowMs(), k.frame, f.Px, k.blur)
		if err != nil {
			return err
		}
		ids[i], _ = res.LastInsertId()
		qs[i] = q
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ix_face_done(key,model,faces,at) VALUES(?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET model=excluded.model, faces=excluded.faces, at=excluded.at`, it.Key, ixFaceModel, len(found), ixNowMs()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	box.faces.mu.Lock()
	for _, id := range gone {
		box.faces.dropLocked(id)
	}
	for i, k := range found {
		box.faces.addLocked(ids[i], qs[i], 0, ixFaceInfo{key: it.Key, frame: k.frame, score: k.f.Score, px: k.f.Px, blur: k.blur})
	}
	box.faces.mu.Unlock()
	for i, k := range found {
		if k.img != nil {
			a.ixWriteFaceCrop(ids[i], k.img, k.f)
		}
	}
	// Grouped once all the file's faces are known.
	for i := range found {
		if err := a.ixGroupFace(ctx, ids[i], qs[i]); err != nil {
			return err
		}
	}
	_, _ = a.db.Writer.ExecContext(ctx, `DELETE FROM ix_ml_err WHERE task='faces' AND key=?`, it.Key)
	a.ixMLMarkDone("faces", it.Key)
	return nil
}

// ixGroupFace puts a face with a person: the person of its closest grouped
// look-alike, or a new person when enough faces look alike.
func (a *App) ixGroupFace(ctx context.Context, id int64, v []int8) error {
	box := a.ixMLBox()
	// Grouping and a person's changes (merge, taking a face out) take turns,
	// so a face never joins a person merged away a moment before.
	box.faces.group.Lock()
	defer box.faces.group.Unlock()
	return a.ixGroupFaceLocked(ctx, id, v, a.ixFaceUnitOf(), nil)
}

// ixGroupFaceLocked does ixGroupFace with the grouping lock held. The rules:
//   - unusable faces are never grouped;
//   - a face joins the person of its closest grouped look-alike, at the
//     likeness the pair needs (stricter when either face is weak);
//   - otherwise a good face starts a new person when good look-alikes not
//     yet grouped are seen in at least ixFaceMinFaces different files
//     (copies of one file, and frames of one video, count once). It takes
//     those faces with it, and the weak ones that are clearly alike.
//
// reuse, when set, may give an existing empty person for the new one.
func (a *App) ixGroupFaceLocked(ctx context.Context, id int64, v []int8, unit func(string) string, reuse func([]int64) int64) error {
	box := a.ixMLBox()
	s := &box.faces
	rejected := a.ixFaceRejects(ctx, id) // people a person took this face out of
	s.mu.RLock()
	me, ok := s.index[id]
	if !ok || s.person[me] != 0 || s.tier[me] == ixFaceJunk {
		s.mu.RUnlock()
		return nil
	}
	tier := s.tier[me]
	myShot := s.shotLocked(me)
	type cand struct {
		ixFaceNear
		tier int8
		unit string
	}
	var grouped, loose []cand
	shots := map[int64]bool{myShot: true} // cannot-link: one face per picture or frame
	for _, n := range s.nearLocked(v, ixFaceSameSim, id) {
		t := s.tier[n.idx]
		if t == ixFaceJunk || n.sim < ixFacePairSim(tier, t) {
			continue
		}
		c := cand{n, t, ""}
		switch {
		case n.person > 0:
			if !rejected[n.person] && !s.inShotLocked(n.person, myShot, id) {
				grouped = append(grouped, c)
			}
		case n.person == 0:
			if sh := s.shotLocked(n.idx); !shots[sh] {
				shots[sh] = true
				c.unit = unit(s.keys[s.key[n.idx]])
				loose = append(loose, c)
			}
		}
	}
	myUnit := unit(s.keys[s.key[me]])
	s.mu.RUnlock()
	if len(grouped) > 0 {
		return a.ixSetFacePerson(ctx, []int64{id}, grouped[0].person)
	}
	if tier != ixFaceGood {
		return nil // weak faces only join
	}
	units := map[string]bool{myUnit: true}
	take := []int64{id}
	for _, n := range loose {
		if n.tier == ixFaceGood {
			units[n.unit] = true
		}
		take = append(take, n.id)
	}
	if len(units) < ixFaceMinFaces {
		return nil
	}
	var person int64
	if reuse != nil {
		person = reuse(take)
	}
	if person == 0 {
		now := ixNowMs()
		res, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_people(name,created_at,updated_at) VALUES('',?,?)`, now, now)
		if err != nil {
			return err
		}
		person, _ = res.LastInsertId()
	}
	return a.ixSetFacePerson(ctx, take, person)
}

// ixRegroupFaces gives ungrouped faces another chance: a person may have
// been made near them since. A few hundred per round, in turn.
func (a *App) ixRegroupFaces(ctx context.Context, n int) {
	box := a.ixMLBox()
	if box.faces.load(ctx, a.db.Reader, ixFaceModel) != nil {
		return
	}
	box.mu.Lock()
	after := box.regroupAfter
	box.mu.Unlock()
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT id FROM ix_faces WHERE person=0 AND manual=0 AND id>? ORDER BY id LIMIT ?`, after, n)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	last := int64(0)
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		last = id
		box.faces.mu.RLock()
		i, ok := box.faces.index[id]
		var v []int8
		if ok {
			v = append([]int8(nil), box.faces.vecs[int(i)*box.faces.dim:int(i+1)*box.faces.dim]...)
		}
		box.faces.mu.RUnlock()
		if v != nil {
			_ = a.ixGroupFace(ctx, id, v)
		}
	}
	box.mu.Lock()
	box.regroupAfter = last // 0 after the last: start over
	box.mu.Unlock()
}

func (a *App) ixSetFacePerson(ctx context.Context, ids []int64, person int64) error {
	box := a.ixMLBox()
	// A person merged into another is that other person now.
	person = a.ixPersonResolve(ctx, person)
	for _, id := range ids {
		if _, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_faces SET person=? WHERE id=? AND manual=0`, person, id); err != nil {
			return err
		}
		box.faces.mu.Lock()
		box.faces.setPersonLocked(id, person)
		box.faces.mu.Unlock()
	}
	return nil
}

func ixFaceCropName(id int64) string {
	return filepath.Join("faces", fmt.Sprintf("%02x", id%256), strconv.FormatInt(id, 10)+".jpg")
}

// ixWriteFaceCrop keeps a small picture of a face, with some margin.
func (a *App) ixWriteFaceCrop(id int64, img image.Image, f ixFaceFound) {
	b := img.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())
	cx, cy := (f.X1+f.X2)/2*w, (f.Y1+f.Y2)/2*h
	side := math.Max((f.X2-f.X1)*w, (f.Y2-f.Y1)*h) * 1.4
	r := image.Rect(int(cx-side/2), int(cy-side/2), int(cx+side/2), int(cy+side/2)).Add(b.Min).Intersect(b)
	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok || r.Empty() {
		return
	}
	thumb, err := ixMakeThumb(sub.SubImage(r), 160)
	if err != nil {
		return
	}
	path := filepath.Join(a.ixThumbDir(), ixFaceCropName(id))
	if os.MkdirAll(filepath.Dir(path), 0o755) == nil {
		_ = os.WriteFile(path, thumb, 0o644)
	}
}

// ---------- people ----------

type ixPerson struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Hidden bool   `json:"hidden,omitempty"`
	Faces  int    `json:"faces"`
	Files  int    `json:"files"`
	Cover  int64  `json:"cover"` // the clearest face
}

func (a *App) ixPeople(ctx context.Context) ([]ixPerson, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT p.id, p.name, p.hidden, COUNT(f.id), COUNT(DISTINCT f.key),
			(SELECT f2.id FROM ix_faces f2 WHERE f2.person=p.id ORDER BY f2.score DESC, f2.id LIMIT 1)
		FROM ix_people p JOIN ix_faces f ON f.person=p.id WHERE p.merged_into=0 GROUP BY p.id ORDER BY (p.name='') , COUNT(DISTINCT f.key) DESC, p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ixPerson{}
	for rows.Next() {
		var p ixPerson
		var cover sql.NullInt64
		if rows.Scan(&p.ID, &p.Name, &p.Hidden, &p.Faces, &p.Files, &cover) == nil {
			p.Cover = cover.Int64
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// ixPersonChange: rename, hide, show, merge (into another person), say two
// people are not the same, or take a face out (it is never put with this
// person again).
func (a *App) ixPersonChange(ctx context.Context, id int64, action, name string, other, face int64) error {
	box := a.ixMLBox()
	box.faces.group.Lock()
	defer box.faces.group.Unlock()
	var exists int
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_people WHERE id=? AND merged_into=0`, id).Scan(&exists); err != nil || exists == 0 {
		return errors.New("no such person")
	}
	now := ixNowMs()
	var err error
	switch action {
	case "rename":
		name = strings.TrimSpace(name)
		if len(name) > 120 {
			return errors.New("a name is at most 120 characters")
		}
		_, err = a.db.Writer.ExecContext(ctx, `UPDATE ix_people SET name=?, updated_at=? WHERE id=?`, name, now, id)
	case "hide", "show":
		_, err = a.db.Writer.ExecContext(ctx, `UPDATE ix_people SET hidden=?, updated_at=? WHERE id=?`, action == "hide", now, id)
	case "merge":
		if other == id {
			return errors.New("choose another person")
		}
		if err := a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_people WHERE id=? AND merged_into=0`, other).Scan(&exists); err != nil || exists == 0 {
			return errors.New("no such person to merge into")
		}
		err = a.ixMergePeopleLocked(ctx, id, other)
	case "not-same":
		// "Same person?" answered no: never suggested (or merged by
		// itself) again, also after either is merged with someone else.
		if other == id || other <= 0 {
			return errors.New("choose another person")
		}
		lo, hi := min(id, other), max(id, other)
		_, err = a.db.Writer.ExecContext(ctx, `INSERT INTO ix_person_pair(a,b,state,at) VALUES(?,?,'no',?)
			ON CONFLICT(a,b) DO UPDATE SET state='no', at=excluded.at`, lo, hi, now)
	case "remove-face":
		// As in Ente: the face is never put with this person again (nor with
		// anyone this person is merged with), but may go to someone else.
		var res sql.Result
		res, err = a.db.Writer.ExecContext(ctx, `UPDATE ix_faces SET person=0 WHERE id=? AND person=?`, face, id)
		if err == nil {
			if n, _ := res.RowsAffected(); n == 0 {
				return errors.New("that face is not this person's")
			}
			_, err = a.db.Writer.ExecContext(ctx, `INSERT OR IGNORE INTO ix_face_reject(face,person,at) VALUES(?,?,?)`, face, id, now)
			box.faces.mu.Lock()
			box.faces.setPersonLocked(face, 0)
			box.faces.mu.Unlock()
		}
	default:
		return errIxBadMode
	}
	if err == nil {
		a.ixOrgLog("person", map[string]any{"person": id, "action": action, "name": name, "into": other, "face": face})
	}
	return err
}

// ixFacesOf lists the faces in a file, with their people.
type ixJFace struct {
	ID     int64      `json:"id"`
	Box    [4]float64 `json:"box"`
	Person int64      `json:"person"`
	Name   string     `json:"name,omitempty"`
}

func (a *App) ixFacesOf(ctx context.Context, key string) []ixJFace {
	out := []ixJFace{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT f.id, f.x1, f.y1, f.x2, f.y2, CASE WHEN f.person>0 THEN f.person ELSE 0 END, COALESCE(p.name,'')
		FROM ix_faces f LEFT JOIN ix_people p ON p.id=f.person AND p.hidden=0 WHERE f.key=? ORDER BY f.x1`, key)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var f ixJFace
		if rows.Scan(&f.ID, &f.Box[0], &f.Box[1], &f.Box[2], &f.Box[3], &f.Person, &f.Name) == nil {
			out = append(out, f)
		}
	}
	return out
}

// ixLoadPeople adds who appears in which file to the browser's model.
func (a *App) ixLoadPeople(ctx context.Context, m *ixModel) {
	m.PersonKeys = map[int64]map[string]bool{}
	m.PersonName = map[int64]string{}
	m.PersonHidden = map[int64]bool{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT DISTINCT f.person, f.key, p.name, p.hidden FROM ix_faces f JOIN ix_people p ON p.id=f.person WHERE f.person>0 AND p.merged_into=0`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var key, name string
		var hidden bool
		if rows.Scan(&id, &key, &name, &hidden) != nil {
			continue
		}
		m.PersonHidden[id] = hidden
		if m.PersonKeys[id] == nil {
			m.PersonKeys[id] = map[string]bool{}
		}
		m.PersonKeys[id][key] = true
		m.PersonName[id] = name
	}
}

// ---------- handlers ----------

func (a *App) handleIxPeople(w http.ResponseWriter, r *http.Request) {
	list, err := a.ixPeople(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Faces too small or blurred to group are counted on their own.
	var loose, unusable int
	_ = a.db.Reader.QueryRowContext(r.Context(), `SELECT COALESCE(SUM(NOT `+ixFaceJunkSQL+`),0), COALESCE(SUM(`+ixFaceJunkSQL+`),0) FROM ix_faces WHERE person=0`).Scan(&loose, &unusable)
	writeJSON(w, http.StatusOK, map[string]any{"people": list, "ungrouped": loose, "unusable": unusable,
		"suggest": a.ixPeopleSuggestions(r.Context(), list, 12), "status": a.ixMLStatusNow(r.Context()).Tasks["faces"]})
}

func (a *App) handleIxPerson(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	m, err := a.ixModelNow(ctx)
	if m == nil {
		writeJSONError(w, http.StatusInternalServerError, errString(err))
		return
	}
	var p ixPerson
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT id, name, hidden FROM ix_people WHERE id=? AND merged_into=0`, id).Scan(&p.ID, &p.Name, &p.Hidden); err != nil {
		writeJSONError(w, http.StatusNotFound, "no such person (it may have been merged; reload)")
		return
	}
	type face struct {
		ID  int64  `json:"id"`
		Key string `json:"key"`
	}
	faces := []face{}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT id, key FROM ix_faces WHERE person=? ORDER BY score DESC, id LIMIT 400`, id); err == nil {
		for rows.Next() {
			var f face
			if rows.Scan(&f.ID, &f.Key) == nil {
				faces = append(faces, f)
			}
		}
		rows.Close()
	}
	cards := []ixJCard{}
	seen := map[int32]bool{}
	type hit struct {
		item int32
		date int64
	}
	var hits []hit
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT DISTINCT key FROM ix_faces WHERE person=?`, id); err == nil {
		for rows.Next() {
			var k string
			if rows.Scan(&k) == nil {
				if ii, ok := m.ByKey[k]; ok && !seen[m.Items[ii].Cluster] {
					seen[m.Items[ii].Cluster] = true
					hits = append(hits, hit{ii, m.Posts[m.Items[ii].Post].Date})
				}
			}
		}
		rows.Close()
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].date > hits[j].date })
	for _, h := range hits {
		cards = append(cards, m.mediaCard(h.item))
		if len(cards) >= 600 {
			break
		}
	}
	p.Files = len(hits)
	p.Faces = len(faces)
	writeJSON(w, http.StatusOK, map[string]any{"person": p, "faces": faces, "cards": cards})
}

func (a *App) handleIxPersonChange(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	b := ixBody(r)
	num := func(k string) int64 {
		switch v := b[k].(type) {
		case float64:
			return int64(v)
		case string:
			n, _ := strconv.ParseInt(v, 10, 64)
			return n
		}
		return 0
	}
	if err := a.ixPersonChange(r.Context(), id, toString(b["action"]), toString(b["name"]), num("into"), num("face")); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.ixModelSoon()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleIxFace(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(filepath.Join(a.ixThumbDir(), ixFaceCropName(id)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

var _ = jpeg.Encode // crops are JPEG (ixMakeThumb)
