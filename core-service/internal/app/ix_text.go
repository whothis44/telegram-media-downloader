package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/seekbar"
)

// Words in the archive's media, made searchable: text read from pictures
// (Immich's text-reading model, in the same service as image recognition),
// and speech written out from voice notes, audio and videos (a Whisper
// server: an OpenAI-compatible speech-to-text service on this PC). Both are
// optional; like image recognition they only talk to addresses on this
// computer or the local network.

// ---------- text in pictures ----------

const ixOCRDefaultModel = "PP-OCRv5_mobile"

// ixOCRModels are the text-reading models Immich offers (by script).
var ixOCRModels = map[string]string{
	"PP-OCRv5_mobile":         "Chinese, English, Japanese (default)",
	"PP-OCRv5_server":         "Chinese, English, Japanese (larger, slower)",
	"LATIN__PP-OCRv5_mobile":  "Latin script: French, Spanish, German and others",
	"ESLAV__PP-OCRv5_mobile":  "Cyrillic: Russian, Ukrainian and others",
	"EN__PP-OCRv5_mobile":     "English",
	"EL__PP-OCRv5_mobile":     "Greek",
	"KOREAN__PP-OCRv5_mobile": "Korean",
	"TH__PP-OCRv5_mobile":     "Thai",
	"CH__PP-OCRv5_mobile":     "Chinese",
}

func ixOCRModelOf(s ixSettings) string {
	if _, ok := ixOCRModels[s.MLTextModel]; ok {
		return s.MLTextModel
	}
	return ixOCRDefaultModel
}

func (c *ixMLClient) ocr(ctx context.Context, model string, pic []byte) (string, error) {
	raw, err := c.predict(ctx, map[string]any{"ocr": map[string]any{
		"detection":   map[string]any{"modelName": model, "options": map[string]any{"minScore": 0.5, "maxResolution": 736}},
		"recognition": map[string]any{"modelName": model, "options": map[string]any{"minScore": 0.8}},
	}}, pic, "")
	if err != nil {
		return "", err
	}
	var resp struct {
		OCR *struct {
			Text []string `json:"text"`
		} `json:"ocr"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.OCR == nil {
		return "", errors.New("text reading: unreadable answer")
	}
	var lines []string
	for _, t := range resp.OCR.Text {
		if t = strings.TrimSpace(t); t != "" {
			lines = append(lines, t)
		}
	}
	return strings.Join(lines, "\n"), nil
}

func (a *App) ixOCRTask(ctx context.Context, c *ixMLClient) *ixMLTask {
	model := ixOCRModelOf(a.ixLoadSettings(ctx))
	a.ixMLDoneSet(ctx, "text", model, `SELECT key FROM ix_ocr WHERE model=?`)
	return &ixMLTask{
		name: "text", model: model, errTable: "ix_ml_err", par: 3,
		wanted: func(it *ixMItem) bool { return it.Type == "photo" || it.Type == "video" },
		done:   a.ixMLIsDone("text"),
		run: func(ctx context.Context, it *ixMItem) error {
			pics, err := ixMLFullPictureHook(a, ctx, it)
			if err != nil {
				return a.ixMLReadErr(ctx, err)
			}
			// A caption over a video shows in several frames: each line once.
			var lines []string
			seen := map[string]bool{}
			for _, pic := range pics {
				t, err := c.ocr(ctx, model, pic)
				if err != nil {
					return ixMLFileErrOf(ctx, err)
				}
				for _, l := range strings.Split(t, "\n") {
					if k := strings.ToLower(strings.TrimSpace(l)); k != "" && !seen[k] {
						seen[k] = true
						lines = append(lines, l)
					}
				}
			}
			text := strings.Join(lines, "\n")
			if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_ocr(key,model,text,at) VALUES(?,?,?,?)
				ON CONFLICT(key) DO UPDATE SET model=excluded.model, text=excluded.text, at=excluded.at`, it.Key, model, text, ixNowMs()); err != nil {
				return err
			}
			_, _ = a.db.Writer.ExecContext(ctx, `DELETE FROM ix_ml_err WHERE task='text' AND key=?`, it.Key)
			a.ixMLMarkDone("text", it.Key)
			return nil
		},
		test: func(ctx context.Context) error {
			_, err := c.ocr(ctx, model, ixMLTestPicture())
			return err
		},
		note: a.ixMLTaskNoter("text"),
	}
}

// ---------- speech to text ----------

const (
	ixSTTDefaultModel = "Systran/faster-whisper-large-v3"
	ixSTTMaxSeconds   = 90 * 60 // only the first 90 minutes of a recording are written out
	ixSTTVersion      = "0.8.3" // the speaches release the instructions use
)

var ixSTTModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,80}(/[A-Za-z0-9][A-Za-z0-9_.\-]{0,80})?$`)

type ixSTTClient struct {
	base  string
	model string
	http  *http.Client
}

func (a *App) ixSTTClientNow(ctx context.Context) *ixSTTClient {
	s := a.ixLoadSettings(ctx)
	if s.STTURL == "" {
		return nil
	}
	model := s.STTModel
	if model == "" {
		model = ixSTTDefaultModel
	}
	return &ixSTTClient{base: s.STTURL, model: model, http: ixSTTHTTP}
}

func (c *ixSTTClient) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("the speech-to-text service at %s does not answer (is its Docker container running?)", c.base)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the speech-to-text service at %s answered %d to its health check", c.base, resp.StatusCode)
	}
	return nil
}

// install asks the service to download its model (once; it says so when
// the model is already there).
func (c *ixSTTClient) install(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/models/"+(&url.URL{Path: c.model}).EscapedPath(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ixMLUnreachable{err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("the speech model %s could not be installed (%d): %s", c.model, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

type ixTranscript struct {
	Language string       `json:"language"`
	Text     string       `json:"text"`
	Segments [][3]any     `json:"segments"` // start s, end s, text
	raw      []ixSTTPiece // as the service sent them
}

type ixSTTPiece struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// ixSTTHTTP is ixMLHTTP's local-only connection without its overall time
// limit: each recording gets a limit by its length (transcribe).
var ixSTTHTTP = &http.Client{Transport: ixMLHTTP.Transport, CheckRedirect: ixMLHTTP.CheckRedirect}

// transcribe sends a recording (FLAC or WAV) to /v1/audio/transcriptions,
// streaming it rather than copying it. seconds is its length (0: unknown)
// and sets how long the service may take before the file counts as failed.
func (c *ixSTTClient) transcribe(ctx context.Context, audio []byte, seconds ...float64) (ixTranscript, error) {
	dur := float64(ixSTTMaxSeconds)
	if len(seconds) > 0 && seconds[0] > 0 {
		dur = min(seconds[0], dur)
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Minute+time.Duration(dur*1.5)*time.Second)
	defer cancel()
	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)
	go func() {
		part, err := w.CreateFormFile("file", "audio.flac")
		if err == nil {
			_, err = part.Write(audio)
		}
		for _, f := range [][2]string{{"model", c.model}, {"response_format", "verbose_json"}, {"vad_filter", "true"}} {
			if err == nil {
				err = w.WriteField(f[0], f[1])
			}
		}
		if err == nil {
			err = w.Close()
		}
		pw.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, c.base+"/v1/audio/transcriptions", pr)
	if err != nil {
		pr.Close()
		return ixTranscript{}, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		pr.Close()
		if ctx.Err() == nil && rctx.Err() != nil {
			// The service works but this recording took too long.
			return ixTranscript{}, fmt.Errorf("writing it out took longer than %s", (5*time.Minute + time.Duration(dur*1.5)*time.Second).Round(time.Minute))
		}
		return ixTranscript{}, ixMLUnreachable{err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return ixTranscript{}, ixMLUnreachable{err}
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		err := fmt.Errorf("speech to text answered %d: %s", resp.StatusCode, msg)
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusNotFound {
			return ixTranscript{}, ixMLUnreachable{err}
		}
		return ixTranscript{}, err
	}
	var out struct {
		Language string       `json:"language"`
		Text     string       `json:"text"`
		Segments []ixSTTPiece `json:"segments"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return ixTranscript{}, errors.New("speech to text: unreadable answer")
	}
	t := ixTranscript{Language: out.Language, Text: strings.TrimSpace(out.Text), raw: out.Segments}
	for _, sg := range out.Segments {
		t.Segments = append(t.Segments, [3]any{sg.Start, sg.End, strings.TrimSpace(sg.Text)})
	}
	return t, nil
}

// ixSTTAudioHook reads a file's sound as 16 kHz mono FLAC (errIxNoAudio
// when it has none). Replaceable in tests.
var ixSTTAudioHook = func(a *App, ctx context.Context, it *ixMItem) ([]byte, error) {
	return a.ixSTTAudio(ctx, it)
}

var errIxNoAudio = errors.New("no sound track")

func (a *App) ixSTTAudio(ctx context.Context, it *ixMItem) ([]byte, error) {
	rel := ixNormPath(it.Path)
	if rel == "" {
		return nil, errors.New("bad path")
	}
	full := filepath.Join(a.downloadsDir, filepath.FromSlash(rel))
	if _, err := os.Stat(full); err != nil {
		return nil, errors.New("file not found")
	}
	bin := seekbar.FFmpegPath()
	if bin == "" {
		return nil, ixErrNoFFmpeg
	}
	fctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(fctx, bin, "-hide_banner", "-nostdin", "-loglevel", "error", "-i", full, "-vn", "-sn", "-dn",
		"-ac", "1", "-ar", "16000", "-t", strconv.Itoa(ixSTTMaxSeconds), "-c:a", "flac", "-f", "flac", "pipe:1")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || out.Len() <= 128 {
		if strings.Contains(errOut.String(), "does not contain any stream") || strings.Contains(errOut.String(), "matches no streams") || out.Len() <= 128 {
			return nil, errIxNoAudio
		}
		return nil, fmt.Errorf("the sound could not be read: %s", strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

func ixSTTWanted(it *ixMItem) bool {
	switch it.Type {
	case "voice", "audio", "round", "video_note":
		return true // only the first 90 minutes are read
	case "video":
		return true // only the first 90 minutes are read
	}
	return false
}

func (a *App) ixSTTTask(ctx context.Context, c *ixSTTClient) *ixMLTask {
	a.ixMLDoneSet(ctx, "speech", c.model, `SELECT key FROM ix_stt WHERE model=?`)
	return &ixMLTask{
		name: "speech", model: c.model, errTable: "ix_ml_err",
		wanted: ixSTTWanted,
		done:   a.ixMLIsDone("speech"),
		run: func(ctx context.Context, it *ixMItem) error {
			wav, err := ixSTTAudioHook(a, ctx, it)
			var t ixTranscript
			switch {
			case errors.Is(err, errIxNoAudio):
				// Nothing to write out: done, with no text.
			case err != nil:
				return a.ixMLReadErr(ctx, err)
			default:
				if t, err = c.transcribe(ctx, wav, it.Dur); err != nil {
					return ixMLFileErrOf(ctx, err)
				}
			}
			segs, _ := json.Marshal(t.Segments)
			if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_stt(key,model,language,text,segments,at) VALUES(?,?,?,?,?,?)
				ON CONFLICT(key) DO UPDATE SET model=excluded.model, language=excluded.language, text=excluded.text, segments=excluded.segments, at=excluded.at`,
				it.Key, c.model, t.Language, t.Text, string(segs), ixNowMs()); err != nil {
				return err
			}
			_, _ = a.db.Writer.ExecContext(ctx, `DELETE FROM ix_ml_err WHERE task='speech' AND key=?`, it.Key)
			a.ixMLMarkDone("speech", it.Key)
			return nil
		},
		test: func(ctx context.Context) error {
			_, err := c.transcribe(ctx, ixSilentWAV(1))
			return err
		},
		note: a.ixMLTaskNoter("speech"),
	}
}

// ixSilentWAV is n seconds of silence, 16 kHz mono, to check the service.
func ixSilentWAV(n int) []byte {
	samples := 16000 * n
	var b bytes.Buffer
	le := func(v uint32, size int) {
		for i := 0; i < size; i++ {
			b.WriteByte(byte(v >> (8 * i)))
		}
	}
	b.WriteString("RIFF")
	le(uint32(36+2*samples), 4)
	b.WriteString("WAVEfmt ")
	le(16, 4)
	le(1, 2)
	le(1, 2)
	le(16000, 4)
	le(32000, 4)
	le(2, 2)
	le(16, 2)
	b.WriteString("data")
	le(uint32(2*samples), 4)
	b.Write(make([]byte, 2*samples))
	return b.Bytes()
}

// ixRunSTT writes out speech, one file at a time, behind everything else.
func (a *App) ixRunSTT(ctx context.Context) {
	wait := 90 * time.Second
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
		c := a.ixSTTClientNow(ctx)
		if c == nil || ixInQuietHours(ixSettings{QuietStart: s.DupQuietStart, QuietEnd: s.DupQuietEnd}, time.Now()) {
			continue
		}
		note := a.ixMLTaskNoter("speech")
		if err := c.ping(ctx); err != nil {
			note(false, err.Error())
			wait = 5 * time.Minute
			continue
		}
		m, err := a.ixModelNow(ctx)
		if err != nil || m == nil {
			continue
		}
		a.ixMLCount(ctx, m)
		wait = a.ixMLRoundTask(ctx, a.ixSTTTask(ctx, c), m, 50)
		a.ixMLCount(ctx, m)
	}
}

// ---------- reading it back ----------

// ixTextOf is what was read or heard in a file.
func (a *App) ixTextOf(ctx context.Context, key string) (ocr string, speech *ixTranscript) {
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT text FROM ix_ocr WHERE key=?`, key).Scan(&ocr)
	var t ixTranscript
	var segs string
	if a.db.Reader.QueryRowContext(ctx, `SELECT language, text, segments FROM ix_stt WHERE key=?`, key).Scan(&t.Language, &t.Text, &segs) == nil {
		_ = json.Unmarshal([]byte(segs), &t.Segments)
		speech = &t
	}
	return ocr, speech
}

// ixSearchMediaText finds the posts whose pictures show, or whose
// recordings say, the words searched for.
func (a *App) ixSearchMediaText(ctx context.Context, m *ixModel, q string, out map[int32]bool) {
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT key FROM ix_ocr WHERE text LIKE ? ESCAPE '\' UNION SELECT key FROM ix_stt WHERE text LIKE ? ESCAPE '\' LIMIT 20000`, like, like)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if rows.Scan(&k) != nil {
			continue
		}
		ii, ok := m.ByKey[k]
		if !ok {
			continue
		}
		for _, other := range m.Clusters[m.Items[ii].Cluster].Items {
			out[m.Items[other].Post] = true
		}
	}
}

// handleIxSTTCheck checks the speech service and installs its model.
func (a *App) handleIxSTTCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c := a.ixSTTClientNow(ctx)
	note := a.ixMLTaskNoter("speech")
	if c == nil {
		writeJSONError(w, http.StatusBadRequest, "speech to text is off: enter its address first")
		return
	}
	for _, step := range []func(context.Context) error{c.ping, c.install, func(ctx context.Context) error {
		_, err := c.transcribe(ctx, ixSilentWAV(1))
		return err
	}} {
		if err := step(ctx); err != nil {
			note(false, err.Error())
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	note(true, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleIxExtra is what recognition found in one file: faces, text read
// from it, speech written out.
func (a *App) handleIxExtra(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	key := r.URL.Query().Get("key")
	if m := a.ixModelLast(); m != nil {
		if ii, ok := m.ByKey[key]; ok {
			if canon := m.Clusters[m.Items[ii].Cluster].Canon; canon >= 0 && m.Items[canon].Key != "" {
				key = m.Items[canon].Key // looked at once per duplicate set, by its best copy
			}
		}
	}
	ocr, speech := a.ixTextOf(ctx, key)
	writeJSON(w, http.StatusOK, map[string]any{"faces": a.ixFacesOf(ctx, key), "text": ocr, "speech": speech})
}
