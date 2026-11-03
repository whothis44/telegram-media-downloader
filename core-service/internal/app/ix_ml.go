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
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/seekbar"
)

// Image recognition, optional and entirely on the user's own machines: the
// machine-learning service of Immich (a self-hosted photo library) runs in
// Docker on the PC, on its graphics card, and turns each picture into an
// embedding, a list of numbers placing it by what it shows. Pictures with
// close embeddings show the same scene, people or subject even when they
// are not copies; a sentence in any language the model knows is placed the
// same way, so the archive can be searched by description.
//
// The service answers POST /predict (multipart: entries = the task as JSON,
// and an image or a text) with {"clip": "[0.01, ...]"}; GET /ping answers
// "pong". This is Immich's internal API, so the version used is pinned in
// the instructions (ixMLImmichVersion). Only addresses on this computer or
// the local network are accepted, so no picture can be sent anywhere else.

const (
	ixMLImmichVersion = "v2.7.5"
	ixMLDefaultModel  = "ViT-B-16-SigLIP2__webli"
	ixMLMaxTries      = 3
	ixMLMaxImageMB    = 40
)

var ixMLModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,99}$`)

// ixMLCheckURL accepts http(s) addresses on this computer or the local
// network only.
func ixMLCheckURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return "", errors.New("the image recognition address must look like http://127.0.0.1:3003")
	}
	host := u.Hostname()
	if strings.Contains(host, "%") || !ixLocalHost(host) {
		return "", errors.New("the image recognition service must be on this computer or the local network (127.0.0.1, a 192.168.x.x / 10.x.x.x address, or a local name), so no picture leaves your network")
	}
	return u.Scheme + "://" + u.Host, nil
}

func ixLocalHost(host string) bool {
	h := strings.ToLower(host)
	if h == "localhost" || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".lan") || strings.HasSuffix(h, ".home.arpa") || strings.HasSuffix(h, ".internal") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	// A bare machine name (no dots) is resolved on the local network; it
	// must start with a letter (134744072 or 0x08080808 are addresses).
	// The address actually connected to is checked again (ixMLHTTP).
	return ixMachineNameRe.MatchString(h)
}

var ixMachineNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// ---------- client ----------

type ixMLClient struct {
	base  string
	model string
	http  *http.Client
}

func (a *App) ixMLClientNow(ctx context.Context) *ixMLClient {
	s := a.ixLoadSettings(ctx)
	if s.MLURL == "" {
		return nil
	}
	model := s.MLModel
	if model == "" {
		model = ixMLDefaultModel
	}
	// The first request may wait while the service downloads the model.
	return &ixMLClient{base: s.MLURL, model: model, http: ixMLHTTP}
}

// ixMLHTTP talks to the service directly: never through a proxy set for
// the system, and never following a redirect, so a picture can only reach
// the address that was checked.
var ixMLHTTP = &http.Client{
	Timeout: 10 * time.Minute,
	Transport: &http.Transport{Proxy: nil, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second,
		// A local name could resolve to a public address: check the address
		// actually connected to.
		DialContext: (&net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
				return fmt.Errorf("the image recognition address resolves to %s, which is not on this computer or the local network", host)
			}
			return nil
		}}).DialContext},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("the image recognition service answered with a redirect; refusing to follow it")
	},
}

func (c *ixMLClient) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/ping", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("the image recognition service at %s does not answer (is its Docker container running?)", c.base)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "pong") {
		return fmt.Errorf("%s does not look like Immich's machine-learning service (GET /ping answered %d)", c.base, resp.StatusCode)
	}
	return nil
}

// embed asks for the embedding of a picture (image) or a text.
func (c *ixMLClient) embed(ctx context.Context, image []byte, text string) ([]float32, error) {
	kind := "visual"
	if image == nil {
		kind = "textual"
	}
	raw, err := c.predict(ctx, map[string]any{"clip": map[string]any{kind: map[string]any{"modelName": c.model, "options": map[string]any{}}}}, image, text)
	if err != nil {
		return nil, err
	}
	return ixMLParse(raw)
}

// predict sends one request to /predict: the tasks as JSON, and a picture
// or a text.
func (c *ixMLClient) predict(ctx context.Context, task map[string]any, image []byte, text string) ([]byte, error) {
	entries, _ := json.Marshal(task)
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("entries", string(entries))
	if image != nil {
		part, err := w.CreateFormFile("image", "image")
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(image); err != nil {
			return nil, err
		}
	} else {
		_ = w.WriteField("text", text)
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/predict", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, ixMLUnreachable{err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, ixMLUnreachable{err}
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		var e struct {
			Detail any `json:"detail"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Detail != nil {
			msg = fmt.Sprint(e.Detail)
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		err := fmt.Errorf("image recognition answered %d: %s", resp.StatusCode, msg)
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusNotFound {
			return nil, ixMLUnreachable{err} // the service or its model, not this picture
		}
		return nil, err
	}
	return raw, nil
}

// ixMLUnreachable marks errors of the service itself (not of one picture).
type ixMLUnreachable struct{ err error }

func (e ixMLUnreachable) Error() string { return e.err.Error() }
func (e ixMLUnreachable) Unwrap() error { return e.err }

// ixMLParse reads {"clip": "[...]"} (the vector serialised as a JSON string,
// or, in some versions, an array) and normalises it to length 1.
func ixMLParse(raw []byte) ([]float32, error) {
	var resp struct {
		Clip json.RawMessage `json:"clip"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("image recognition: unreadable answer: %v", err)
	}
	if len(resp.Clip) == 0 {
		return nil, errors.New("image recognition: the answer has no embedding")
	}
	inner := []byte(resp.Clip)
	var s string
	if json.Unmarshal(inner, &s) == nil {
		inner = []byte(s)
	}
	var v []float32
	if err := json.Unmarshal(inner, &v); err != nil || len(v) < 16 || len(v) > 8192 {
		return nil, errors.New("image recognition: the embedding is not a list of numbers")
	}
	if !ixNormalise(v) {
		return nil, errors.New("image recognition: an empty embedding")
	}
	return v, nil
}

func ixNormalise(v []float32) bool {
	var n float64
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return false
		}
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return false
	}
	inv := float32(1 / math.Sqrt(n))
	for i := range v {
		v[i] *= inv
	}
	return true
}

// ---------- store ----------

// Embeddings are kept as int8 (a unit vector times 127): a quarter of the
// memory of float32, and cosine within about 0.005 of the exact one.

type ixEmbStore struct {
	mu     sync.RWMutex
	model  string
	dim    int
	keys   []string
	index  map[string]int32
	vecs   []int8
	norms  []float32
	loaded bool
}

type ixMLBox struct {
	store        ixEmbStore
	faces        ixFaceStore
	mu           sync.Mutex
	st           ixMLStatus
	after        map[string]int32           // task -> cluster index reached in this pass
	notes        map[string]ixMLTaskNote    // task -> whether its service answered, its last error
	done         map[string]map[string]bool // task -> files looked at (faces, text, speech)
	doneModel    map[string]string
	regroupAfter int64 // the last ungrouped face given another chance
}

type ixMLStatus struct {
	Enabled   bool                      `json:"enabled"`
	URL       string                    `json:"url,omitempty"`
	Model     string                    `json:"model,omitempty"`
	Reachable bool                      `json:"reachable"`
	Running   bool                      `json:"running"`
	Embedded  int                       `json:"embedded"`
	Pending   int                       `json:"pending"`
	Failed    int                       `json:"failed"`
	LastError string                    `json:"lastError,omitempty"`
	CheckedAt int64                     `json:"checkedAt,omitempty"`
	Version   string                    `json:"immichVersion"`
	Tasks     map[string]ixMLTaskStatus `json:"tasks"`
	Speech    ixMLTaskStatus            `json:"speech"`
}

type ixMLTaskNote struct {
	reachable bool
	msg       string
}

// ixMLTaskStatus: progress of one kind of work (faces, text, speech).
type ixMLTaskStatus struct {
	On        bool   `json:"on"`
	Done      int    `json:"done"`
	Pending   int    `json:"pending"`
	Failed    int    `json:"failed"`
	Reachable bool   `json:"reachable"`
	LastError string `json:"lastError,omitempty"`
}

var ixMLState sync.Map // *App -> *ixMLBox

func (a *App) ixMLBox() *ixMLBox {
	v, _ := ixMLState.LoadOrStore(a, &ixMLBox{store: ixEmbStore{index: map[string]int32{}}, faces: ixFaceStore{index: map[int64]int32{}},
		after: map[string]int32{}, notes: map[string]ixMLTaskNote{}, done: map[string]map[string]bool{}, doneModel: map[string]string{}})
	return v.(*ixMLBox)
}

func ixQuantise(v []float32) ([]int8, float32) {
	q := make([]int8, len(v))
	var n float64
	for i, x := range v {
		r := math.Round(float64(x) * 127)
		r = math.Max(-127, math.Min(127, r))
		q[i] = int8(r)
		n += r * r
	}
	return q, float32(math.Sqrt(n))
}

// load reads the embeddings of the current model from the database.
func (s *ixEmbStore) load(ctx context.Context, db *sql.DB, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded && s.model == model {
		return nil
	}
	s.model, s.dim, s.keys, s.vecs, s.norms = model, 0, nil, nil, nil
	s.index = map[string]int32{}
	rows, err := db.QueryContext(ctx, `SELECT key, dim, vec FROM ix_emb WHERE model=? ORDER BY key`, model)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var dim int
		var blob []byte
		if rows.Scan(&key, &dim, &blob) != nil || len(blob) != dim {
			continue
		}
		v := make([]int8, dim)
		for i, b := range blob {
			v[i] = int8(b)
		}
		s.putLocked(key, v)
	}
	s.loaded = true
	return rows.Err()
}

func (s *ixEmbStore) putLocked(key string, v []int8) {
	if s.dim == 0 {
		s.dim = len(v)
	}
	if len(v) != s.dim {
		return
	}
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if i, ok := s.index[key]; ok {
		copy(s.vecs[int(i)*s.dim:], v)
		s.norms[i] = float32(math.Sqrt(n))
		return
	}
	s.index[key] = int32(len(s.keys))
	s.keys = append(s.keys, key)
	s.vecs = append(s.vecs, v...)
	s.norms = append(s.norms, float32(math.Sqrt(n)))
}

func (s *ixEmbStore) has(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.index[key]
	return ok
}

func (s *ixEmbStore) size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys)
}

func ixDot8(a, b []int8) int32 {
	var sum int32
	for i := range a {
		sum += int32(a[i]) * int32(b[i])
	}
	return sum
}

// sim is the cosine of two stored files (ok false when either is missing).
func (s *ixEmbStore) sim(a, b string) (float32, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i, okA := s.index[a]
	j, okB := s.index[b]
	if !okA || !okB {
		return 0, false
	}
	d := s.dim
	den := s.norms[i] * s.norms[j]
	if den == 0 {
		return 0, false
	}
	return float32(ixDot8(s.vecs[int(i)*d:int(i+1)*d], s.vecs[int(j)*d:int(j+1)*d])) / den, true
}

type ixEmbHit struct {
	Key string
	Sim float32
}

// nearest lists the stored files closest to q (a unit vector), best first.
func (s *ixEmbStore) nearest(q []float32, k int, keep func(string) bool, skip string) []ixEmbHit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.dim == 0 || len(q) != s.dim {
		return nil
	}
	qq, qn := ixQuantise(q)
	if qn == 0 {
		return nil
	}
	var hits []ixEmbHit
	worst := float32(-2)
	d := s.dim
	for i, key := range s.keys {
		if key == skip || s.norms[i] == 0 {
			continue
		}
		c := float32(ixDot8(qq, s.vecs[i*d:(i+1)*d])) / (qn * s.norms[i])
		if len(hits) >= k && c <= worst {
			continue
		}
		if keep != nil && !keep(key) {
			continue
		}
		hits = append(hits, ixEmbHit{key, c})
		if len(hits) > 2*k {
			sort.Slice(hits, func(x, y int) bool { return hits[x].Sim > hits[y].Sim })
			hits = hits[:k]
			worst = hits[k-1].Sim
		}
	}
	sort.Slice(hits, func(x, y int) bool { return hits[x].Sim > hits[y].Sim })
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

func (s *ixEmbStore) vector(key string) []float32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i, ok := s.index[key]
	if !ok || s.norms[i] == 0 {
		return nil
	}
	out := make([]float32, s.dim)
	for k, x := range s.vecs[int(i)*s.dim : int(i+1)*s.dim] {
		out[k] = float32(x) / s.norms[i]
	}
	return out
}

// ixEmbSim compares two files by their embeddings, for the sets; nil when
// image recognition is off or nothing is embedded yet.
func (a *App) ixEmbSim() func(string, string) (float32, bool) {
	box := a.ixMLBox()
	s := a.ixLoadSettings(context.Background())
	if s.MLURL == "" {
		return nil
	}
	model := s.MLModel
	if model == "" {
		model = ixMLDefaultModel
	}
	if err := box.store.load(context.Background(), a.db.Reader, model); err != nil || box.store.size() == 0 {
		return nil
	}
	return box.store.sim
}

// ---------- worker ----------

func (a *App) ixRunML(ctx context.Context) {
	wait := 45 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = time.Minute
		if !ixVdupBackground.Load() {
			continue
		}
		s := a.ixLoadSettings(ctx)
		c := a.ixMLClientNow(ctx)
		box := a.ixMLBox()
		box.mu.Lock()
		box.st.Enabled = c != nil
		box.mu.Unlock()
		if c == nil || ixInQuietHours(ixSettings{QuietStart: s.DupQuietStart, QuietEnd: s.DupQuietEnd}, time.Now()) {
			continue
		}
		if err := c.ping(ctx); err != nil {
			a.ixMLNote(false, err.Error())
			wait = 5 * time.Minute
			continue
		}
		if err := box.store.load(ctx, a.db.Reader, c.model); err != nil {
			a.ixMLNote(true, err.Error())
			continue
		}
		m, err := a.ixModelNow(ctx)
		if err != nil || m == nil {
			continue
		}
		// Each kind of work takes its turn: what a picture shows first (it
		// powers search and sets), then faces, then text.
		wait = time.Minute
		for _, t := range a.ixMLTasks(ctx, c, s) {
			w := a.ixMLRoundTask(ctx, t, m, 500)
			if w > time.Minute { // the service is down or failing: stop here
				wait = w
				break
			}
			if w < wait {
				wait = w
			}
		}
		a.ixMLCount(ctx, m)
	}
}

// ixMLTask is one kind of work done once per distinct file.
type ixMLTask struct {
	name     string
	model    string
	errTable string // where single files' failures are kept
	wanted   func(it *ixMItem) bool
	done     func(key string) bool
	run      func(ctx context.Context, it *ixMItem) error
	test     func(ctx context.Context) error // does the service work at all
	note     func(reachable bool, msg string)
	finish   func(ctx context.Context) // after a round that did something
}

// ixMLTasks lists the image-recognition work that is switched on.
func (a *App) ixMLTasks(ctx context.Context, c *ixMLClient, s ixSettings) []*ixMLTask {
	out := []*ixMLTask{a.ixClipTask(c)}
	if s.MLFaces {
		out = append(out, a.ixFaceTask(ctx, c))
	}
	if s.MLText {
		out = append(out, a.ixOCRTask(ctx, c))
	}
	return out
}

func (a *App) ixClipTask(c *ixMLClient) *ixMLTask {
	box := a.ixMLBox()
	return &ixMLTask{
		name: "clip", model: c.model, errTable: "ix_emb_err",
		wanted: func(it *ixMItem) bool { return it.Type == "photo" || it.Type == "video" },
		done:   box.store.has,
		run:    func(ctx context.Context, it *ixMItem) error { return a.ixMLEmbedItem(ctx, c, it) },
		test: func(ctx context.Context) error {
			_, err := c.embed(ctx, ixMLTestPicture(), "")
			return err
		},
		note: a.ixMLNote,
	}
}

// ixMLDoneSet is the set of files a task has looked at, read once per
// model and kept up to date as files are done.
func (a *App) ixMLDoneSet(ctx context.Context, task, model, query string) map[string]bool {
	box := a.ixMLBox()
	box.mu.Lock()
	defer box.mu.Unlock()
	if box.doneModel[task] == model && box.done[task] != nil {
		return box.done[task]
	}
	set := map[string]bool{}
	if rows, err := a.db.Reader.QueryContext(ctx, query, model); err == nil {
		for rows.Next() {
			var k string
			if rows.Scan(&k) == nil {
				set[k] = true
			}
		}
		rows.Close()
	}
	box.done[task], box.doneModel[task] = set, model
	return set
}

func (a *App) ixMLMarkDone(task, key string) {
	box := a.ixMLBox()
	box.mu.Lock()
	if box.done[task] != nil {
		box.done[task][key] = true
	}
	box.mu.Unlock()
}

func (a *App) ixMLIsDone(task string) func(string) bool {
	return func(key string) bool {
		box := a.ixMLBox()
		box.mu.Lock()
		defer box.mu.Unlock()
		return box.done[task][key]
	}
}

// ixMLRound embeds up to n files (tests and the connection check use it).
func (a *App) ixMLRound(ctx context.Context, c *ixMLClient, m *ixModel, n int) time.Duration {
	return a.ixMLRoundTask(ctx, a.ixClipTask(c), m, n)
}

// ixMLRoundTask does up to n files of one task and says when to try again.
func (a *App) ixMLRoundTask(ctx context.Context, t *ixMLTask, m *ixModel, n int) time.Duration {
	t.note(true, "")
	r := &ixMLRun{failed: a.ixMLFailedKeys(ctx, t), tried: map[string]bool{}, noFFmpeg: seekbar.FFmpegPath() == ""}
	defer func() {
		// Failures of single files are recorded once the service is known
		// to work: a file worked in this round, or a test request does.
		if len(r.pending) > 0 && !r.ok {
			if err := t.test(ctx); err != nil {
				r.pending = nil
				return
			}
		}
		a.ixMLFlush(ctx, t, r)
		if r.ok && t.finish != nil {
			t.finish(ctx)
		}
	}()
	for i := 0; i < n && ctx.Err() == nil; i++ {
		worked, err := a.ixMLStep(ctx, t, m, r)
		var down ixMLUnreachable
		switch {
		case errors.Is(err, errIxMLOffline):
			return 5 * time.Minute // the NAS is offline
		case errors.As(err, &down):
			r.pending = nil // not the files' fault
			t.note(false, err.Error())
			return 5 * time.Minute
		case errors.Is(err, errIxMLStreak):
			// Many files in a row failed and none worked: the service or
			// its model is at fault, not the files.
			msg := fmt.Sprintf("%d files in a row could not be looked at (%s); trying again in 30 minutes", len(r.pending), r.last)
			r.pending = nil
			t.note(true, msg)
			return 30 * time.Minute
		case err != nil:
			a.ixLog("image recognition (%s): %v", t.name, err)
			return time.Minute
		case !worked:
			return time.Minute
		}
	}
	return 2 * time.Second
}

// ixMLTestPicture is a small grey gradient, to check the picture model.
func ixMLTestPicture() []byte {
	img := image.NewGray(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Pix[y*64+x] = uint8(x*2 + y*2)
		}
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85})
	return buf.Bytes()
}

var (
	errIxMLOffline = errors.New("the downloads folder is offline")
	errIxMLStreak  = errors.New("too many failures in a row")
)

// ixMLStreak is how many files in a row may fail, with none working, before
// the service rather than the files is blamed.
const ixMLStreak = 8

// ixMLRun is one round's bookkeeping. A file's failure is recorded only
// once another file has worked (or at the end of the round), so a broken
// service or model never marks good files unreadable.
type ixMLRun struct {
	failed   map[string]bool
	tried    map[string]bool
	noFFmpeg bool
	pending  [][2]string // key, error
	last     string
	ok       bool // a file worked in this round
}

func (a *App) ixMLFlush(ctx context.Context, t *ixMLTask, r *ixMLRun) {
	for _, f := range r.pending {
		_ = a.ixMLFail(ctx, t, f[0], f[1])
	}
	r.pending = nil
}

// ixMLTaskNoter records how a task's service answered.
func (a *App) ixMLTaskNoter(task string) func(bool, string) {
	return func(reachable bool, msg string) {
		box := a.ixMLBox()
		box.mu.Lock()
		box.notes[task] = ixMLTaskNote{reachable, msg}
		box.mu.Unlock()
	}
}

func (a *App) ixMLNote(reachable bool, msg string) {
	box := a.ixMLBox()
	box.mu.Lock()
	defer box.mu.Unlock()
	box.notes["clip"] = ixMLTaskNote{reachable, msg}
	box.st.Reachable = reachable
	box.st.LastError = msg
	box.st.CheckedAt = time.Now().UnixMilli()
}

// ixMLWanted: a duplicate set gets looked at once, by its best copy, when it
// is downloaded.
func ixMLWanted(m *ixModel, c int) (int32, bool) {
	cl := &m.Clusters[c]
	if cl.Canon < 0 || int(cl.Canon) >= len(m.Items) {
		return 0, false
	}
	it := &m.Items[cl.Canon]
	if it.Key == "" || it.Path == "" {
		return 0, false
	}
	return cl.Canon, true
}

// ixMLStep does the next file of a task; false when none is left.
func (a *App) ixMLStep(ctx context.Context, t *ixMLTask, m *ixModel, r *ixMLRun) (bool, error) {
	box := a.ixMLBox()
	box.mu.Lock()
	start := box.after[t.name]
	box.mu.Unlock()
	if int(start) > len(m.Clusters) {
		start = 0 // the model was rebuilt smaller
	}
	for ci := int(start); ci < len(m.Clusters); ci++ {
		ii, ok := ixMLWanted(m, ci)
		if !ok {
			continue
		}
		it := &m.Items[ii]
		if !t.wanted(it) || t.done(it.Key) || r.failed[it.Key] || r.tried[it.Key] || (it.Type != "photo" && r.noFFmpeg) {
			continue
		}
		r.tried[it.Key] = true // once per round
		box.mu.Lock()
		box.after[t.name] = int32(ci + 1)
		box.st.Running = true
		box.mu.Unlock()
		err := t.run(ctx, it)
		box.mu.Lock()
		box.st.Running = false
		box.mu.Unlock()
		var bad ixMLFileErr
		if errors.As(err, &bad) {
			r.pending = append(r.pending, [2]string{it.Key, bad.msg})
			r.last = bad.msg
			if !r.ok && len(r.pending) >= ixMLStreak {
				return true, errIxMLStreak
			}
			if r.ok {
				a.ixMLFlush(ctx, t, r)
			}
			return true, nil
		}
		if err == nil {
			r.ok = true
			a.ixMLFlush(ctx, t, r)
		}
		return true, err
	}
	box.mu.Lock()
	box.after[t.name] = 0
	box.mu.Unlock()
	if start > 0 {
		return a.ixMLStep(ctx, t, m, r) // once more from the start
	}
	return false, nil
}

// ixMLFileErr: this file could not be looked at (the service works).
type ixMLFileErr struct{ msg string }

func (e ixMLFileErr) Error() string { return e.msg }

func (a *App) ixMLFailedKeys(ctx context.Context, t *ixMLTask) map[string]bool {
	out := map[string]bool{}
	q, args := `SELECT key FROM ix_emb_err WHERE model=? AND tries>=?`, []any{t.model, ixMLMaxTries}
	if t.errTable != "ix_emb_err" {
		q, args = `SELECT key FROM ix_ml_err WHERE task=? AND model=? AND tries>=?`, []any{t.name, t.model, ixMLMaxTries}
	}
	rows, err := a.db.Reader.QueryContext(ctx, q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil {
			out[k] = true
		}
	}
	return out
}

// ixMLFileErrOf turns an error of the service about one file into a file
// failure, and keeps the service's own failures as they are.
func ixMLFileErrOf(ctx context.Context, err error) error {
	var down ixMLUnreachable
	if err == nil || errors.As(err, &down) || ctx.Err() != nil {
		return err
	}
	return ixMLFileErr{err.Error()}
}

// ixMLReadErr is a file that could not be read for a task.
func (a *App) ixMLReadErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, rootErr := os.Stat(a.downloadsDir); rootErr != nil {
		return errIxMLOffline
	}
	return ixMLFileErr{err.Error()}
}

func (a *App) ixMLEmbedItem(ctx context.Context, c *ixMLClient, it *ixMItem) error {
	pics, err := a.ixMLPictures(ctx, it)
	if err != nil {
		return a.ixMLReadErr(ctx, err)
	}
	var sum []float32
	for _, pic := range pics {
		v, err := c.embed(ctx, pic, "")
		if err != nil {
			return ixMLFileErrOf(ctx, err)
		}
		if sum == nil {
			sum = make([]float32, len(v))
		}
		if len(v) != len(sum) {
			return ixMLFileErr{"embeddings of different sizes"}
		}
		for i := range v {
			sum[i] += v[i]
		}
	}
	if !ixNormalise(sum) {
		return ixMLFileErr{"an empty embedding"}
	}
	if err := a.ixMLSave(ctx, c.model, it.Key, sum); err != nil {
		return err
	}
	if it.Type == "photo" {
		a.ixMLLookAlikes(ctx, it.Key)
	}
	return nil
}

func (a *App) ixMLSave(ctx context.Context, model, key string, v []float32) error {
	q, _ := ixQuantise(v)
	blob := make([]byte, len(q))
	for i, x := range q {
		blob[i] = byte(x)
	}
	if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_emb(key,model,dim,vec,at) VALUES(?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET model=excluded.model, dim=excluded.dim, vec=excluded.vec, at=excluded.at`, key, model, len(q), blob, ixNowMs()); err != nil {
		return err
	}
	_, _ = a.db.Writer.ExecContext(ctx, `DELETE FROM ix_emb_err WHERE key=?`, key)
	box := a.ixMLBox()
	box.store.mu.Lock()
	if box.store.model == model {
		box.store.putLocked(key, q)
	}
	box.store.mu.Unlock()
	return nil
}

func (a *App) ixMLFail(ctx context.Context, t *ixMLTask, key, why string) error {
	if len(why) > 300 {
		why = why[:300]
	}
	if t.errTable == "ix_emb_err" {
		_, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_emb_err(key,model,error,tries,at) VALUES(?,?,?,1,?)
			ON CONFLICT(key) DO UPDATE SET model=excluded.model, error=excluded.error, tries=CASE WHEN ix_emb_err.model=excluded.model THEN ix_emb_err.tries+1 ELSE 1 END, at=excluded.at`, key, t.model, why, ixNowMs())
		return err
	}
	_, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_ml_err(task,key,model,error,tries,at) VALUES(?,?,?,?,1,?)
		ON CONFLICT(task,key) DO UPDATE SET model=excluded.model, error=excluded.error, tries=CASE WHEN ix_ml_err.model=excluded.model THEN ix_ml_err.tries+1 ELSE 1 END, at=excluded.at`, t.name, key, t.model, why, ixNowMs())
	return err
}

// ixMLLookAlikeSim: pictures this alike by recognition, not already known
// as copies, go to the look-alike review (nothing is merged by itself).
const ixMLLookAlikeSim = 0.975

// ixMLLookAlikes queues the closest other pictures of a newly looked-at
// picture for review.
func (a *App) ixMLLookAlikes(ctx context.Context, key string) {
	if !a.ixLoadSettings(ctx).MLDupes {
		return
	}
	m := a.ixModelLast() // the last model built; nil before the first
	if m == nil {
		return
	}
	ii, ok := m.ByKey[key]
	if !ok {
		return
	}
	cluster := m.Items[ii].Cluster
	box := a.ixMLBox()
	v := box.store.vector(key)
	if v == nil {
		return
	}
	hits := box.store.nearest(v, 3, func(k string) bool {
		j, ok := m.ByKey[k]
		return ok && m.Items[j].Type == "photo" && m.Items[j].Cluster != cluster
	}, key)
	for _, h := range hits {
		if h.Sim < ixMLLookAlikeSim {
			break
		}
		ka, kb := key, h.Key
		if kb < ka {
			ka, kb = kb, ka
		}
		_, _ = a.db.Writer.ExecContext(ctx, `INSERT OR IGNORE INTO ix_msug(key_a,key_b,kind,distance,found_at) VALUES(?,?,'look',?,?)`, ka, kb, 1-float64(h.Sim), ixNowMs())
	}
}

// ixMLPictures is what is sent for a file: a photo's grid thumbnail (or the
// file itself), or three frames of a video, at a fifth, half and four
// fifths of its length.
var ixMLPicturesHook = func(a *App, ctx context.Context, it *ixMItem) ([][]byte, error) {
	return a.ixMLPicturesFromDisk(ctx, it)
}

func (a *App) ixMLPictures(ctx context.Context, it *ixMItem) ([][]byte, error) {
	return ixMLPicturesHook(a, ctx, it)
}

func (a *App) ixMLPicturesFromDisk(ctx context.Context, it *ixMItem) ([][]byte, error) {
	if it.Type == "photo" {
		if it.Thumb {
			if data, err := os.ReadFile(filepath.Join(a.ixThumbDir(), filepath.FromSlash(ixThumbName(it.Key)))); err == nil && len(data) > 0 {
				return [][]byte{data}, nil
			}
		}
	}
	rel := ixNormPath(it.Path)
	if rel == "" {
		return nil, errors.New("bad path")
	}
	full := filepath.Join(a.downloadsDir, filepath.FromSlash(rel))
	if it.Type == "photo" {
		f, err := os.Open(full)
		if err != nil {
			return nil, errors.New("file not found")
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, ixMLMaxImageMB<<20+1))
		if err != nil {
			return nil, err
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
	for _, t := range times {
		fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		cmd := exec.CommandContext(fctx, bin, "-hide_banner", "-nostdin", "-loglevel", "error", "-ss", fmt.Sprintf("%.2f", t), "-i", full,
			"-frames:v", "1", "-an", "-sn", "-dn", "-vf", "scale='min(512,iw)':-2", "-f", "image2pipe", "-vcodec", "mjpeg", "-q:v", "3", "pipe:1")
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

// ixMLCount updates the counts shown in the status.
func (a *App) ixMLCount(ctx context.Context, m *ixModel) {
	s := a.ixLoadSettings(ctx)
	box := a.ixMLBox()
	c := a.ixMLClientNow(ctx)
	if c == nil {
		c = &ixMLClient{model: ixMLModelOf(s)}
	}
	tasks := map[string]*ixMLTask{"clip": a.ixClipTask(c), "faces": a.ixFaceTask(ctx, c), "text": a.ixOCRTask(ctx, c)}
	if sc := a.ixSTTClientNow(ctx); sc != nil {
		tasks["speech"] = a.ixSTTTask(ctx, sc)
	}
	on := map[string]bool{"clip": true, "faces": s.MLFaces, "text": s.MLText, "speech": s.STTURL != ""}
	out := map[string]ixMLTaskStatus{}
	for name, t := range tasks {
		failed := a.ixMLFailedKeys(ctx, t)
		st := ixMLTaskStatus{On: on[name], Failed: len(failed)}
		if m != nil {
			for ci := range m.Clusters {
				ii, ok := ixMLWanted(m, ci)
				if !ok || !t.wanted(&m.Items[ii]) {
					continue
				}
				switch k := m.Items[ii].Key; {
				case t.done(k):
					st.Done++
				case !failed[k]:
					st.Pending++
				}
			}
		}
		out[name] = st
	}
	box.mu.Lock()
	clip := out["clip"]
	box.st.Embedded, box.st.Pending, box.st.Failed = clip.Done, clip.Pending, clip.Failed
	delete(out, "clip")
	box.st.Speech = out["speech"]
	delete(out, "speech")
	box.st.Tasks = out
	box.mu.Unlock()
}

func ixMLModelOf(s ixSettings) string {
	if s.MLModel == "" {
		return ixMLDefaultModel
	}
	return s.MLModel
}

func (a *App) ixMLStatusNow(ctx context.Context) ixMLStatus {
	s := a.ixLoadSettings(ctx)
	box := a.ixMLBox()
	box.mu.Lock()
	st := box.st
	st.Tasks = map[string]ixMLTaskStatus{}
	for k, v := range box.st.Tasks {
		n := box.notes[k]
		v.Reachable, v.LastError = n.reachable, n.msg
		st.Tasks[k] = v
	}
	n := box.notes["speech"]
	st.Speech.Reachable, st.Speech.LastError = n.reachable, n.msg
	notes := map[string]ixMLTaskNote{}
	for k, v := range box.notes {
		notes[k] = v
	}
	box.mu.Unlock()
	st.Enabled = s.MLURL != ""
	st.URL = s.MLURL
	st.Model = ixMLModelOf(s)
	st.Version = ixMLImmichVersion
	for k, on := range map[string]bool{"faces": s.MLFaces, "text": s.MLText} {
		t := st.Tasks[k]
		t.On = on && st.Enabled
		if n, ok := notes[k]; ok {
			t.Reachable, t.LastError = n.reachable, n.msg
		}
		st.Tasks[k] = t
	}
	st.Speech.On = s.STTURL != ""
	return st
}

// ---------- search ----------

// ixMLFilter keeps the files that have a copy in the chats the browser
// shows (one chat, one category, photos or videos).
func ixMLFilter(m *ixModel, group, category, typ string) func(string) bool {
	if group == "" && category == "" && typ == "" {
		return func(key string) bool { _, ok := m.ByKey[key]; return ok }
	}
	return func(key string) bool {
		ii, ok := m.ByKey[key]
		if !ok {
			return false
		}
		if typ != "" && m.Items[ii].Type != typ {
			return false
		}
		c := m.Items[ii].Cluster
		for _, other := range m.Clusters[c].Items {
			g := m.Items[other].Group
			if (group == "" || g == group) && (category == "" || m.GroupCat[g] == category) {
				return true
			}
		}
		return false
	}
}

// ixMLSearch finds the files closest to a description, or to a file.
func (a *App) ixMLSearch(ctx context.Context, m *ixModel, text, like string, limit int, keep func(string) bool) ([]ixEmbHit, error) {
	c := a.ixMLClientNow(ctx)
	if c == nil {
		return nil, errors.New("image recognition is off (Archive → Settings)")
	}
	box := a.ixMLBox()
	if err := box.store.load(ctx, a.db.Reader, c.model); err != nil {
		return nil, err
	}
	var q []float32
	if like != "" {
		// A copy of a file is searched by the set's best copy.
		if ii, ok := m.ByKey[like]; ok {
			if canon := m.Clusters[m.Items[ii].Cluster].Canon; canon >= 0 {
				like = m.Items[canon].Key
			}
		}
		q = box.store.vector(like)
		if q == nil {
			return nil, errors.New("this file has not been looked at by image recognition yet")
		}
	} else {
		v, err := c.embed(ctx, nil, text)
		if err != nil {
			return nil, err
		}
		q = v
	}
	return box.store.nearest(q, limit, keep, like), nil
}
