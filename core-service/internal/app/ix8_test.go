package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

func ixTestFreeze(until int64) telegram.FreezeState { return telegram.FreezeState{Until: until} }

// ix8FakeML answers like Immich's machine-learning service for pictures
// (embedding, faces, text) and texts. A test picture is a real JPEG with
// "#<seed>" after it; the seed says what the picture holds:
// "faces=A,B;text=..." or a key for its embedding.
func ix8FakeML(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			_, _ = io.WriteString(w, "pong")
			return
		}
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var entries map[string]map[string]map[string]any
		_ = json.Unmarshal([]byte(r.FormValue("entries")), &entries)
		seed := r.FormValue("text")
		if f, _, err := r.FormFile("image"); err == nil {
			b, _ := io.ReadAll(f)
			if i := bytes.LastIndexByte(b, '#'); i >= 0 {
				seed = string(b[i+1:])
			} else {
				seed = "plain"
			}
		}
		parts := map[string]string{}
		for _, p := range strings.Split(seed, ";") {
			if k, v, ok := strings.Cut(p, "="); ok {
				parts[k] = v
			}
		}
		out := map[string]any{"imageWidth": 64, "imageHeight": 64}
		switch {
		case entries["facial-recognition"] != nil:
			// A face is "who[*likeness][:score[:size]]": who is one letter;
			// "A*0.45" is someone only 0.45 alike to A (and alike to other
			// "A*..." faces); score is the detection score (0.95), size the
			// box's short side in pixels (180) of a 640 px picture.
			out["imageWidth"], out["imageHeight"] = 640, 640
			faces := []any{}
			for i, spec := range strings.Split(parts["faces"], ",") {
				if spec == "" {
					continue
				}
				fields := strings.Split(spec, ":")
				who, score, size, like := fields[0], 0.95, 180.0, 1.0
				if len(fields) > 1 {
					score, _ = strconv.ParseFloat(fields[1], 64)
				}
				if len(fields) > 2 {
					size, _ = strconv.ParseFloat(fields[2], 64)
				}
				if n, l, ok := strings.Cut(who, "*"); ok {
					who = n
					like, _ = strconv.ParseFloat(l, 64)
				}
				v := make([]float32, 512)
				v[int(who[0])%512] = float32(like)
				v[(300+int(who[0]))%512] = float32(math.Sqrt(1 - like*like))
				v[(int(who[0])*7+i)%512] += 0.2 // a little difference between photos of one person
				raw, _ := json.Marshal(v)
				x1 := 8 + float64(i)*210
				faces = append(faces, map[string]any{"boundingBox": map[string]any{"x1": x1, "y1": 40, "x2": x1 + size, "y2": 40 + size*1.2}, "embedding": string(raw), "score": score})
			}
			out["facial-recognition"] = faces
		case entries["ocr"] != nil:
			text := []string{}
			if parts["text"] != "" {
				text = strings.Split(parts["text"], "|")
			}
			out["ocr"] = map[string]any{"text": text, "box": []float64{}, "boxScore": []float64{}, "textScore": []float64{}}
		default:
			s := strings.TrimPrefix(seed, "a photo of ")
			if k, ok := parts["key"]; ok {
				s = k
			}
			raw, _ := json.Marshal(ixSceneSim(s))
			out["clip"] = string(raw)
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
}

func ix8App(t *testing.T, settings map[string]any) (*App, context.Context) {
	t.Helper()
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	s, err := ixParseSettings(settings, a.ixLoadSettings(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ixSaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	return a, ctx
}

func TestIxFacesBecomePeopleAndCanBeCorrected(t *testing.T) {
	srv := ix8FakeML(t)
	defer srv.Close()
	a, ctx := ix8App(t, map[string]any{"mlUrl": srv.URL, "mlFaces": true, "mlText": true})
	m := ixSetScene()
	for i := range m.Items {
		m.Items[i].Path = "x/" + m.Items[i].Key + ".jpg"
	}
	m.Items[m.ByKey["s04"]].Path = "" // not downloaded yet
	who := map[string]string{"s00": "A", "s01": "A", "s02": "A,B", "s03": "B", "s05": "C", "x0": "D", "z0": "Q"}
	old := ixMLFullPictureHook
	defer func() { ixMLFullPictureHook = old }()
	ixMLFullPictureHook = func(_ *App, _ context.Context, it *ixMItem) ([][]byte, error) {
		seed := "faces=" + who[it.Key]
		if it.Key == "s01" {
			seed += ";text=FREE PARKING|Sunday only"
		}
		frame := append(ix10FacePicture(), []byte("#"+seed)...)
		if it.Key == "s02" { // a video: three frames, the same two people in each, a caption in one
			return [][]byte{frame, frame, append(ix10FacePicture(), []byte("#faces=A,B;text=free parking|LIVE")...)}, nil
		}
		return [][]byte{frame}, nil
	}
	c := a.ixMLClientNow(ctx)
	ft := a.ixFaceTask(ctx, c)
	a.ixMLRoundTask(ctx, ft, m, 200)
	people, err := a.ixPeople(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A appears in three files: a person. B twice, C and D once: not yet.
	if len(people) != 1 || people[0].Files != 3 || people[0].Faces != 3 {
		t.Fatalf("people: %+v", people)
	}
	pa := people[0].ID
	var ungrouped int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_faces WHERE person=0`).Scan(&ungrouped)
	if ungrouped != 5 { // B, B, C, D, Q
		t.Fatalf("ungrouped %d", ungrouped)
	}
	if _, err := os.Stat(filepath.Join(a.ixThumbDir(), ixFaceCropName(1))); err != nil {
		t.Fatalf("no face picture: %v", err)
	}
	// A third picture of B makes B a person, with the earlier two.
	m.Items[m.ByKey["s04"]].Path = "x/s04.jpg"
	who["s04"] = "B"
	a.ixMLRoundTask(ctx, ft, m, 200)
	people, _ = a.ixPeople(ctx)
	if len(people) != 2 {
		t.Fatalf("people after B: %+v", people)
	}
	var pb int64
	for _, p := range people {
		if p.ID != pa {
			pb = p.ID
		}
	}
	// Corrections: name, take a face out, merge.
	if err := a.ixPersonChange(ctx, pa, "rename", "Alice", 0, 0); err != nil {
		t.Fatal(err)
	}
	var faceOfB int64
	_ = a.db.Reader.QueryRow(`SELECT id FROM ix_faces WHERE person=? ORDER BY id LIMIT 1`, pb).Scan(&faceOfB)
	if err := a.ixPersonChange(ctx, pb, "remove-face", "", 0, faceOfB); err != nil {
		t.Fatal(err)
	}
	if err := a.ixPersonChange(ctx, pa, "remove-face", "", 0, faceOfB); err == nil {
		t.Fatal("removed a face from someone it does not belong to")
	}
	if err := a.ixPersonChange(ctx, pb, "merge", "", pa, 0); err != nil {
		t.Fatal(err)
	}
	people, _ = a.ixPeople(ctx)
	if len(people) != 1 || people[0].Name != "Alice" || people[0].Faces != 5 {
		t.Fatalf("after merge: %+v", people)
	}
	// A face taken out is never put back with that person by itself, nor
	// with the person they were merged into since.
	var person int64
	_ = a.db.Reader.QueryRow(`SELECT person FROM ix_faces WHERE id=?`, faceOfB).Scan(&person)
	if person != 0 {
		t.Fatalf("taken-out face: %d", person)
	}
	a.ixRegroupFaces(ctx, 100)
	_ = a.db.Reader.QueryRow(`SELECT person FROM ix_faces WHERE id=?`, faceOfB).Scan(&person)
	if person == pa || person == pb {
		t.Fatal("regrouping put a taken-out face back")
	}
	// The browser finds the people's posts, and each file's faces.
	a.ixLoadPeople(ctx, m)
	if len(m.PersonKeys[pa]) != 5 || !m.PersonKeys[pa]["s02"] {
		t.Fatalf("person keys: %v", m.PersonKeys)
	}
	f := ixFeedQuery{Person: pa}
	n := 0
	for pi := range m.Posts {
		if m.postMatches(int32(pi), f, nil) {
			n++
		}
	}
	if n != 3 { // A:1, B:1 and C:1 hold s00–s04
		t.Fatalf("posts with Alice: %d", n)
	}
	if fs := a.ixFacesOf(ctx, "s02"); len(fs) != 2 || fs[0].Name != "Alice" {
		t.Fatalf("faces of s02: %+v", fs)
	}
	// Text in pictures: read, and found by the search.
	a.ixMLRoundTask(ctx, a.ixOCRTask(ctx, c), m, 200)
	if text, _ := a.ixTextOf(ctx, "s01"); text != "FREE PARKING\nSunday only" {
		t.Fatalf("text: %q", text)
	}
	if text, _ := a.ixTextOf(ctx, "s02"); text != "free parking\nLIVE" {
		t.Fatalf("text of the video: %q", text)
	}
	hits := map[int32]bool{}
	a.ixSearchMediaText(ctx, m, "parking", hits)
	if len(hits) != 2 || !hits[0] || !hits[2] { // s01 and s02 are both in A:1 and C:1
		t.Fatalf("text search: %v", hits)
	}
	// Status and export.
	a.ixMLCount(ctx, m)
	st := a.ixMLStatusNow(ctx)
	if !st.Tasks["faces"].On || st.Tasks["faces"].Done == 0 || st.Tasks["text"].Done == 0 {
		t.Fatalf("status: %+v", st.Tasks)
	}
	files := map[string]*bytes.Buffer{}
	write := func(name string, f func(*json.Encoder) error) error {
		files[name] = &bytes.Buffer{}
		return f(json.NewEncoder(files[name]))
	}
	if err := a.ixWriteRecognised(ctx, m, write); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(files["people.jsonl"].String(), `"name":"Alice"`) || strings.Count(files["faces.jsonl"].String(), "\n") != 9 ||
		!strings.Contains(files["media_text.jsonl"].String(), `"kind":"picture"`) {
		t.Fatalf("export: %v", files)
	}
}

func TestIxThingsAndLookAlikes(t *testing.T) {
	srv := ix8FakeML(t)
	defer srv.Close()
	a, ctx := ix8App(t, map[string]any{"mlUrl": srv.URL, "mlThings": "x9\ns9\no5"})
	m := ixSetScene()
	for i := range m.Items {
		m.Items[i].Path = "x/" + m.Items[i].Key + ".jpg"
	}
	a.ixModelBox().model = m
	old := ixMLPicturesHook
	defer func() { ixMLPicturesHook = old }()
	ixMLPicturesHook = func(_ *App, _ context.Context, it *ixMItem) ([][]byte, error) {
		return [][]byte{append(ixMLTestPicture(), []byte("#key="+it.Key)...)}, nil
	}
	c := a.ixMLClientNow(ctx)
	_ = a.ixMLBox().store.load(ctx, a.db.Reader, c.model)
	a.ixMLRound(ctx, c, m, 200)
	things, err := a.ixThings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, th := range things {
		counts[th.Label] = th.Count
	}
	// x/y pictures share one scene (13 of them), s pictures another (18 of
	// 39: nearly half, yet still found), o5 is one picture of its own.
	if counts["x9"] != 13 || counts["s9"] != 18 || counts["o5"] > 2 {
		t.Fatalf("things: %v", counts)
	}
	hits, err := a.ixThingHits(ctx, "x9", 100, ixMLFilter(m, "E", "", ""))
	if err != nil || len(hits) != 7 { // E holds x0 and y1–y6
		t.Fatalf("thing hits in E: %d %v", len(hits), err)
	}
	// Pictures recognition finds near-identical go to the look-alike review.
	var pairs, oPairs int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_msug`).Scan(&pairs)
	// (Unrelated pictures of the fake service may happen to look alike
	// among themselves; never like the scenes.)
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_msug WHERE (substr(key_a,1,1) IN ('o','z')) <> (substr(key_b,1,1) IN ('o','z'))`).Scan(&oPairs)
	if pairs == 0 || oPairs != 0 {
		t.Fatalf("look-alike pairs %d, with unrelated pictures %d", pairs, oPairs)
	}
	// Switched off: none.
	_, _ = a.db.Writer.Exec(`DELETE FROM ix_msug`)
	s := a.ixLoadSettings(ctx)
	s.MLDupes = false
	_ = a.ixSaveSettings(ctx, s)
	a.ixMLLookAlikes(ctx, "s00")
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_msug`).Scan(&pairs)
	if pairs != 0 {
		t.Fatal("look-alikes queued while switched off")
	}
}

// ix8FakeWhisper answers like speaches (an OpenAI-compatible Whisper server).
func ix8FakeWhisper(t *testing.T, installs *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			w.WriteHeader(200)
		case strings.HasPrefix(r.URL.Path, "/v1/models/") && r.Method == http.MethodPost:
			*installs++
			if r.URL.Path != "/v1/models/Systran/faster-whisper-large-v3" {
				http.Error(w, "unknown model "+r.URL.Path, 404)
				return
			}
			w.WriteHeader(201)
		case r.URL.Path == "/v1/audio/transcriptions":
			if err := r.ParseMultipartForm(8 << 20); err != nil || r.FormValue("model") != "Systran/faster-whisper-large-v3" || r.FormValue("response_format") != "verbose_json" {
				http.Error(w, "bad request", 422)
				return
			}
			f, _, err := r.FormFile("file")
			if err != nil {
				http.Error(w, "no file", 422)
				return
			}
			b, _ := io.ReadAll(f)
			text := ""
			if i := bytes.LastIndexByte(b, '#'); i >= 0 {
				text = string(b[i+1:])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"task": "transcribe", "language": "fr", "duration": 3.5, "text": " " + text,
				"segments": []any{map[string]any{"start": 0.0, "end": 3.5, "text": " " + text}}})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestIxSpeechIsWrittenOutAndSearchable(t *testing.T) {
	installs := 0
	srv := ix8FakeWhisper(t, &installs)
	defer srv.Close()
	a, ctx := ix8App(t, map[string]any{"sttUrl": srv.URL})
	m := ixSetScene()
	for i := range m.Items {
		m.Items[i].Path = "x/" + m.Items[i].Key + ".ogg"
	}
	m.Items[m.ByKey["z0"]].Type, m.Items[m.ByKey["z0"]].Dur = "voice", 3.5
	m.Items[m.ByKey["z1"]].Type, m.Items[m.ByKey["z1"]].Dur = "video", 12
	m.Items[m.ByKey["x0"]].Type, m.Items[m.ByKey["x0"]].Dur = "video", 8
	old := ixSTTAudioHook
	defer func() { ixSTTAudioHook = old }()
	oldHave := ixHaveFFmpeg
	defer func() { ixHaveFFmpeg = oldHave }()
	ixHaveFFmpeg = func() bool { return true } // the test reads no real files
	ixSTTAudioHook = func(_ *App, _ context.Context, it *ixMItem) ([]byte, error) {
		if it.Key == "x0" {
			return nil, errIxNoAudio // a silent clip
		}
		return append(ixSilentWAV(1), []byte("#rendez-vous au parc à "+it.Key)...), nil
	}
	c := a.ixSTTClientNow(ctx)
	if c == nil || c.ping(ctx) != nil || c.install(ctx) != nil || installs != 1 {
		t.Fatal("speech service check")
	}
	a.ixMLRoundTask(ctx, a.ixSTTTask(ctx, c), m, 50)
	_, sp := a.ixTextOf(ctx, "z0")
	if sp == nil || sp.Language != "fr" || sp.Text != "rendez-vous au parc à z0" || len(sp.Segments) != 1 {
		t.Fatalf("speech: %+v", sp)
	}
	if _, sp := a.ixTextOf(ctx, "x0"); sp == nil || sp.Text != "" {
		t.Fatalf("silent clip: %+v", sp)
	}
	hits := map[int32]bool{}
	a.ixSearchMediaText(ctx, m, "parc à z1", hits)
	if len(hits) != 1 || !hits[5] {
		t.Fatalf("speech search: %v", hits)
	}
	// Photos are not sent.
	var n int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_stt`).Scan(&n)
	if n != 3 {
		t.Fatalf("written out: %d", n)
	}
	// Only local addresses, as for image recognition.
	if _, err := ixParseSettings(map[string]any{"sttUrl": "https://api.example.com"}, ixDefaultSettings()); err == nil {
		t.Fatal("a public speech service was accepted")
	}
	if _, err := ixParseSettings(map[string]any{"sttModel": "Systran/x/../y"}, ixDefaultSettings()); err == nil {
		t.Fatal("a bad speech model name was accepted")
	}
}

func TestIxDeadlinePutsSmallerFilesFirst(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{"deadlineOrder": "auto"})
	now := time.Now()
	add := func(msg int, size int64, status, origin string, at time.Time) int64 {
		res, err := a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at)
			VALUES('acc','-1001','G',?,1,?,'video','f',?,X'',?,?,?,?)`, msg, fmt.Sprint(msg), size, status, origin, at.UnixMilli(), at.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	small := add(1, 2<<20, "pending", "history", now)
	mid := add(2, 50<<20, "pending", "history", now)
	big := add(3, 900<<20, "pending", "history", now)
	live := add(4, 900<<20, "pending", "live", now)
	add(5, 100<<20, "completed", "history", now.Add(-2*time.Hour)) // 100 MB in the last day: 900 MB+ left needs days
	paused := func(id int64) bool {
		var p int
		_ = a.db.Reader.QueryRow(`SELECT paused FROM tgdl_work WHERE id=?`, id).Scan(&p)
		return p == 1
	}
	// No deletion date: auto leaves the order alone.
	if err := a.ixDeadlineStep(ctx); err != nil {
		t.Fatal(err)
	}
	if paused(mid) || paused(big) {
		t.Fatal("deferred without a deletion date")
	}
	a.ixSaveFreeze(ctx, ixFreezeInfo{FreezeState: ixTestFreeze(now.Add(48 * time.Hour).Unix())})
	d := a.ixDeadlineEstimate(ctx, true)
	if d.Finishes != "no" || !d.Active || d.LeftFiles != 4 || len(d.Loss) != 1 || !d.Loss[0].Private || d.Loss[0].Scanned {
		t.Fatalf("estimate: %+v", d)
	}
	_, _ = a.db.Writer.Exec(`INSERT INTO ix_walks(job_id,group_id,started_at,finished_at,processed,marked_deleted,state) VALUES('w',?,?,?,1,0,'done')`, "-1001", now.UnixMilli(), now.UnixMilli())
	if d := a.ixDeadlineEstimate(ctx, true); !d.Loss[0].Scanned {
		t.Fatalf("a chat with a finished rescan shows as not read: %+v", d.Loss)
	}
	if err := a.ixDeadlineStep(ctx); err != nil {
		t.Fatal(err)
	}
	if paused(small) || !paused(mid) || !paused(big) || paused(live) {
		t.Fatalf("band 0: small %v mid %v big %v live %v", paused(small), paused(mid), paused(big), paused(live))
	}
	if a.ixDeferredWaiting(ctx) != 2 {
		t.Fatal("deferred count")
	}
	// The small file done: the middle band goes, the big one still waits.
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='completed', updated_at=? WHERE id=?`, now.UnixMilli(), small)
	_ = a.ixDeadlineStep(ctx)
	if paused(mid) || !paused(big) {
		t.Fatal("band 1 not released alone")
	}
	// A file waiting to retry later does not hold the others back.
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET retry_at=? WHERE id=?`, now.Add(time.Hour).UnixMilli(), mid)
	_ = a.ixDeadlineStep(ctx)
	if paused(big) {
		t.Fatal("a file waiting to retry held the big one back")
	}
	// A new small file puts the big one back; switching off lets it go.
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET retry_at=0 WHERE id=?`, mid)
	add(6, 1<<20, "pending", "history", now)
	_ = a.ixDeadlineStep(ctx)
	if !paused(big) || !paused(mid) {
		t.Fatal("not set aside again for a smaller file")
	}
	s := a.ixLoadSettings(ctx)
	s.DeadlineOrder = "off"
	_ = a.ixSaveSettings(ctx, s)
	_ = a.ixDeadlineStep(ctx)
	if paused(big) || paused(mid) || a.ixDeferredWaiting(ctx) != 0 {
		t.Fatal("not released when switched off")
	}
	// A file a person paused is never let go by this.
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET paused=1 WHERE id=?`, live)
	s.DeadlineOrder = "on"
	_ = a.ixSaveSettings(ctx, s)
	_ = a.ixDeadlineStep(ctx)
	s.DeadlineOrder = "off"
	_ = a.ixSaveSettings(ctx, s)
	_ = a.ixDeadlineStep(ctx)
	if !paused(live) {
		t.Fatal("released a file someone else paused")
	}
	if al := ixDeadlineAlerts(d, now); len(al) != 1 || !strings.Contains(al[0].Message, "deleted in") {
		t.Fatalf("alert: %+v", al)
	}
}

func TestIxSetsCountTheSamePersonAsBelonging(t *testing.T) {
	m := ixSetScene()
	sim := func(a, b string) (float32, bool) { return ixCos(ixSceneSim(a), ixSceneSim(b)), true }
	// o1 (in A's post) looks like nothing in the set, but shows the person
	// in s00 and s03: it belongs.
	m.PersonKeys = map[int64]map[string]bool{7: {"s00": true, "s03": true, "o1": true}}
	ixAssignSets(m, ixSetFixes{}, ixSetLikeness(m, sim))
	roles := ixSetRoles(m, 1)
	if strings.Join(roles["loose"], ",") != "o2,o3,o4,o5,o6" {
		t.Fatalf("loose: %v", roles["loose"])
	}
	// Without embeddings, people alone decide what they can.
	m.PersonKeys = map[int64]map[string]bool{7: {"s00": true, "o1": true}}
	if ixSetLikeness(m, nil) == nil {
		t.Fatal("no likeness from people")
	}
	ixAssignSets(m, ixSetFixes{}, ixSetLikeness(m, nil))
	if roles := ixSetRoles(m, 1); len(roles["loose"]) != 0 {
		t.Fatalf("people alone marked files loose: %v", roles)
	}
}

func TestIxDeadlineRespectsPeopleAndOtherAccounts(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{"deadlineOrder": "on"})
	now := time.Now()
	add := func(acct string, msg int, size int64, status string) int64 {
		res, err := a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at)
			VALUES(?,'-1001','G',?,1,?,'video','f',?,X'',?,'history',?,?)`, acct, msg, fmt.Sprint(msg), size, status, now.UnixMilli(), now.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	paused := func(id int64) bool {
		var p int
		_ = a.db.Reader.QueryRow(`SELECT paused FROM tgdl_work WHERE id=?`, id).Scan(&p)
		return p == 1
	}
	add("main", 1, 5<<20, "completed")        // the working account
	stuck := add("gone", 2, 1<<20, "pending") // an account that is not running
	big := add("main", 3, 800<<20, "pending")
	mid := add("main", 4, 50<<20, "pending")
	_ = a.ixDeadlineStep(ctx)
	if paused(stuck) || paused(mid) || !paused(big) {
		t.Fatalf("a small file of an idle account held the working one back: big %v mid %v", paused(big), paused(mid))
	}
	// A person resumes the big file: it is never set aside again.
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET paused=0 WHERE id=?`, big)
	_ = a.ixDeadlineStep(ctx)
	_ = a.ixDeadlineStep(ctx)
	if paused(big) {
		t.Fatal("set a file aside again after a person resumed it")
	}
	// A set-aside file that becomes self-destructing goes at once.
	huge := add("main", 5, 900<<20, "pending")
	_ = a.ixDeadlineStep(ctx)
	if !paused(huge) {
		t.Fatal("huge not set aside")
	}
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET ttl_seconds=60 WHERE id=?`, huge)
	_ = a.ixDeadlineStep(ctx)
	if paused(huge) {
		t.Fatal("self-destructing media kept waiting")
	}
	// Nothing finished for half an hour: everything goes, for two hours.
	other := add("main", 6, 700<<20, "pending")
	_ = a.ixDeadlineStep(ctx)
	if !paused(other) {
		t.Fatal("other not set aside")
	}
	old := now.Add(-time.Hour).UnixMilli()
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET updated_at=? WHERE status='completed'`, old)
	_, _ = a.db.Writer.Exec(`UPDATE ix_deferred SET deferred_at=?`, old)
	_ = a.ixDeadlineStep(ctx)
	if paused(other) || !a.ixDeadlineSuspended(ctx, now) {
		t.Fatal("a stalled queue was not let go")
	}
}

func TestIxFacesNeverJoinAMergedPerson(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{})
	box := a.ixMLBox()
	_ = box.faces.load(ctx, a.db.Reader, ixFaceModel)
	now := ixNowMs()
	_, _ = a.db.Writer.Exec(`INSERT INTO ix_people(id,name,created_at,updated_at) VALUES(1,'',?,?),(2,'Q',?,?)`, now, now, now, now)
	_, _ = a.db.Writer.Exec(`INSERT INTO ix_faces(id,key,model,x1,y1,x2,y2,score,vec,person,at) VALUES(1,'k',?,0,0,1,1,0.9,zeroblob(4),1,?)`, ixFaceModel, now)
	if err := a.ixPersonChange(ctx, 1, "merge", "", 2, 0); err != nil {
		t.Fatal(err)
	}
	// A face grouped with person 1 a moment after the merge lands on 2.
	_, _ = a.db.Writer.Exec(`INSERT INTO ix_faces(id,key,model,x1,y1,x2,y2,score,vec,person,at) VALUES(2,'k2',?,0,0,1,1,0.9,zeroblob(4),0,?)`, ixFaceModel, now)
	if err := a.ixSetFacePerson(ctx, []int64{2}, 1); err != nil {
		t.Fatal(err)
	}
	var p int64
	_ = a.db.Reader.QueryRow(`SELECT person FROM ix_faces WHERE id=2`).Scan(&p)
	if p != 2 {
		t.Fatalf("face joined person %d", p)
	}
	// Hidden people do not tie sets together.
	m := ixSetScene()
	m.PersonKeys = map[int64]map[string]bool{2: {"s00": true, "o1": true}}
	m.PersonHidden = map[int64]bool{2: true}
	if ixSetLikeness(m, nil) != nil {
		t.Fatal("a hidden person counts for sets")
	}
}

func TestIxCopiesAreKnownBeforeDownload(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{})
	ixSeedIndex(t, a)
	// The saved shared.jpg is Telegram photo 555; message 4 (not downloaded)
	// is the same photo; two more messages forward photo 777, saved nowhere.
	_, _ = a.db.Writer.Exec(`UPDATE downloads SET telegram_media_kind='photo', telegram_media_id='555', telegram_media_size=1000 WHERE message_id=1`)
	_, _ = a.db.Writer.Exec(`UPDATE ix_messages SET media_key='photo:555:1000' WHERE message_id=4`)
	for _, r := range [][2]any{{"-1000000000102", 21}, {"-1000000000103", 31}} {
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_messages(group_id,message_id,date,text,media_type,media_key,seen_at) VALUES(?,?,1700001000,'',?,'photo:777:2048',1)`, r[0], r[1], "photo"); err != nil {
			t.Fatal(err)
		}
	}
	m, err := a.ixRebuildModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	item := func(g string, msg int) *ixMItem {
		pi := m.ByMsg[g+":"+fmt.Sprint(msg)]
		for _, ii := range m.Posts[pi].Items {
			if m.Items[ii].Msg == msg {
				return &m.Items[ii]
			}
		}
		t.Fatalf("no item %s:%d", g, msg)
		return nil
	}
	saved, copyOf := item("-1000000000101", 1), item("-1000000000101", 4)
	if copyOf.Status != "missing" || copyOf.Cluster != saved.Cluster {
		t.Fatalf("message 4 is not with the saved photo: %+v %+v", copyOf, saved)
	}
	a1, a2 := item("-1000000000102", 21), item("-1000000000103", 31)
	if a1.Cluster != a2.Cluster || a1.Cluster == saved.Cluster {
		t.Fatal("two forwards of one Telegram photo are not one set")
	}
	box := a.ixModelBox()
	box.mu.Lock()
	box.model = m
	box.mu.Unlock()
	d := a.ixDeadlineEstimate(ctx, false)
	// Still to get: photo 777 once (2048 bytes). Message 4 is free (saved
	// elsewhere), and the second 777 is a copy.
	if d.ToGetFiles != 1 || d.ToGetBytes != 2048 || d.Copies != 2 {
		t.Fatalf("estimate: %+v", d)
	}
}
