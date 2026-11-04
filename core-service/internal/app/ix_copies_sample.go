package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"net"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/procutil"
	"github.com/botnick/telegram-media-downloader/core-service/internal/seekbar"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// Confirming big copies without downloading them. For a queued video of
// 200 MB or more, frames are sampled across the whole file: the file on
// Telegram is served to ffmpeg by a tiny web server on this computer
// (127.0.0.1, a random port and a random path for each check), which
// answers ffmpeg's byte ranges with 1 MiB parts read from Telegram on
// demand (aligned, never crossing a part boundary, through the same
// guarded and paced pools as downloads) and kept for the check. ffmpeg
// reads the file's index (an MP4's moov at the front or the end, an MKV's
// cues) and jumps to the key frame before each of 8 points between 5% and
// 95% of the length, so only the parts around them are read.
//
// Each sampled frame gets the hash the video fingerprints use (ix_vfp),
// and is looked for anywhere in the saved videos' fingerprints: the one it
// is held as a copy of, and saved videos 20% shorter to 20% longer. Most
// sampled frames matching at one time offset (a trimmed copy, a new intro
// or a re-encode keeps a single offset) means a confirmed copy. A check
// that would need more than 32 MiB or a minute stops, and the file stays a
// probable copy (or an ordinary download): "could not check without
// downloading".
//
// The minute does not count the time spent waiting for Telegram's parts:
// under its speed limit for accounts without Premium (FLOOD_PREMIUM_WAIT)
// one part can take most of a minute, and a check timed on the clock alone
// failed on every file. A check still stops after ixSampleHardTimeout in
// all. Each failure records why (ix_copy_sample.reason, a category, never
// a name): a passing one (a flood wait, the time, a file reference, a
// Telegram or connection error) is tried again later, after 1 hour, then
// twice as long each time up to a day; only one in the file itself (no
// index ffmpeg can find, no seeking, unreadable data, too few frames, more
// parts than its share) is final.
//
// Downloads already under way are never checked or stopped: the engine
// cannot resume a part-downloaded file, so stopping one would throw away
// what it has fetched.

const (
	ixSampleMinSize = 200 << 20
	ixSamplePoints  = 8
	ixSampleMinHit  = 3   // sampled frames that must match
	ixSampleShare   = 0.6 // ...and this share of those that say anything
	ixSampleSlack   = 2.5 // seconds the offsets of matching frames may differ
)

// Limits of one check (variables for tests).
var (
	ixSampleMaxParts     = 32
	ixSampleTimeout      = 60 * time.Second // not counting the waits for parts
	ixSampleHardTimeout  = 4 * time.Minute  // in all
	ixSampleClockTick    = 250 * time.Millisecond
	ixSamplesPerRun      = 3
	errIxSampleTimeout   = errors.New("the check took too long")
	errIxSampleHardLimit = errors.New("the check took too long, waits for Telegram included")
)

// Why a check failed (ix_copy_sample.reason). The final ones are in the
// file itself: trying again would fail the same way.
const (
	ixSampleWhyFlood     = "flood"       // Telegram asked to wait
	ixSampleWhyTimeout   = "timeout"     // the check's time ran out
	ixSampleWhyReference = "reference"   // the file reference could not be renewed
	ixSampleWhyTelegram  = "telegram"    // another error reading a part
	ixSampleWhyIO        = "ffmpeg-io"   // ffmpeg lost the connection
	ixSampleWhyFFmpeg    = "ffmpeg"      // another ffmpeg error
	ixSampleWhyNoFFmpeg  = "no-ffmpeg"   // ffmpeg is not installed
	ixSampleWhyNoIndex   = "no-index"    // final: no index ffmpeg can find (moov, cues)
	ixSampleWhyNoSeek    = "cannot-seek" // final
	ixSampleWhyBadData   = "bad-data"    // final: ffmpeg cannot read the data
	ixSampleWhyFrames    = "few-frames"  // final: too few frames could be read
	ixSampleWhyParts     = "parts-cap"   // final: needs more parts than its share
	ixSampleWhyMessage   = "message"     // final: the queued message cannot be read
)

func ixSampleFinal(why string) bool {
	switch why {
	case ixSampleWhyNoIndex, ixSampleWhyNoSeek, ixSampleWhyBadData, ixSampleWhyFrames, ixSampleWhyParts, ixSampleWhyMessage:
		return true
	}
	return false
}

// ixSampleWhyOf is the category of a failed check: a part that could not
// be read first (when a part fails, ffmpeg's own error only follows from
// it), then the time, then ffmpeg's last error.
func ixSampleWhyOf(part, timeCause, ffmpegErr error, samples int) string {
	if part != nil {
		s := part.Error()
		switch {
		case errors.Is(part, errIxSampleCap):
			return ixSampleWhyParts
		case ixSlowDown(part):
			return ixSampleWhyFlood
		case strings.Contains(s, "FILE_REFERENCE"):
			return ixSampleWhyReference
		case errors.Is(part, context.DeadlineExceeded) || errors.Is(part, context.Canceled) || timeCause != nil:
			return ixSampleWhyTimeout
		}
		return ixSampleWhyTelegram
	}
	if timeCause != nil {
		return ixSampleWhyTimeout
	}
	if ffmpegErr != nil {
		s := strings.ToLower(ffmpegErr.Error())
		switch {
		case errors.Is(ffmpegErr, ixErrNoFFmpeg):
			return ixSampleWhyNoFFmpeg
		case strings.Contains(s, "moov atom not found") || strings.Contains(s, "no index") || strings.Contains(s, "cues"):
			return ixSampleWhyNoIndex
		case strings.Contains(s, "seek"):
			return ixSampleWhyNoSeek
		case strings.Contains(s, "connection") || strings.Contains(s, "input/output error") || strings.Contains(s, "end of file") || strings.Contains(s, "timed out") || strings.Contains(s, "server returned"):
			return ixSampleWhyIO
		case strings.Contains(s, "invalid data") || strings.Contains(s, "no frame"):
			return ixSampleWhyBadData
		}
		if samples < ixSampleMinHit {
			return ixSampleWhyFFmpeg
		}
	}
	return ixSampleWhyFrames
}

var errIxSampleCap = errors.New("could not check without downloading: the file needs more than its share of parts")

// ixRangeFile serves a remote file's bytes by HTTP ranges, from 1 MiB
// parts read on demand.
type ixRangeFile struct {
	size    int64
	mime    string
	token   string
	get     func(offset int64) ([]byte, error)
	mu      sync.Mutex
	parts   map[int64][]byte
	fetched int
	failed  error

	clock    sync.Mutex
	waited   time.Duration // spent waiting for Telegram's parts
	fetching time.Time     // a part asked for since then (zero: none)
}

// waitedNow is the time spent waiting for parts, the one asked for now
// included.
func (f *ixRangeFile) waitedNow(now time.Time) time.Duration {
	f.clock.Lock()
	defer f.clock.Unlock()
	w := f.waited
	if !f.fetching.IsZero() {
		w += now.Sub(f.fetching)
	}
	return w
}

// fetch reads a part from Telegram, timing the wait.
func (f *ixRangeFile) fetch(off int64) ([]byte, error) {
	f.clock.Lock()
	f.fetching = time.Now()
	f.clock.Unlock()
	defer func() {
		f.clock.Lock()
		f.waited += time.Since(f.fetching)
		f.fetching = time.Time{}
		f.clock.Unlock()
	}()
	return f.get(off)
}

func (f *ixRangeFile) part(index int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.parts[index]; ok {
		return p, nil
	}
	if f.failed != nil {
		return nil, f.failed
	}
	if f.fetched >= ixSampleMaxParts {
		f.failed = errIxSampleCap
		return nil, f.failed
	}
	off := index * telegram.PartSize
	data, err := f.fetch(off)
	if err != nil {
		f.failed = err
		return nil, err
	}
	want := min(int64(telegram.PartSize), f.size-off)
	if int64(len(data)) != want {
		f.failed = fmt.Errorf("Telegram sent %d bytes for a part of %d", len(data), want)
		return nil, f.failed
	}
	f.fetched++
	f.parts[index] = data
	return data, nil
}

func (f *ixRangeFile) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/"+f.token {
		http.NotFound(w, r)
		return
	}
	start, end := int64(0), f.size-1
	status := http.StatusOK
	if spec := r.Header.Get("Range"); spec != "" {
		a, b, ok := ixParseRange(spec, f.size)
		if !ok {
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(f.size, 10))
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start, end, status = a, b, http.StatusPartialContent
	}
	// At most to the end of the part holding the start: the connection's
	// buffers would otherwise make the server fetch parts ffmpeg never
	// reads. ffmpeg asks again from where the answer stopped.
	if status == http.StatusPartialContent || r.Header.Get("Range") == "" && f.size > telegram.PartSize {
		end = min(end, (start/telegram.PartSize+1)*telegram.PartSize-1)
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, f.size))
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", f.mime)
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	for pos := start; pos <= end; {
		idx := pos / telegram.PartSize
		p, err := f.part(idx)
		if err != nil {
			return // the connection is cut: ffmpeg sees a read error
		}
		from := pos - idx*telegram.PartSize
		to := min(int64(len(p)), end-idx*telegram.PartSize+1)
		if _, err := w.Write(p[from:to]); err != nil {
			return // ffmpeg went elsewhere
		}
		pos = idx*telegram.PartSize + to
	}
}

// ixParseRange reads one "bytes=a-b", "bytes=a-" or "bytes=-n" range.
func ixParseRange(spec string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(spec), "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, false
	}
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		return max(0, size-n), size - 1, true
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if b != "" {
		if end, err = strconv.ParseInt(b, 10, 64); err != nil || end < start {
			return 0, 0, false
		}
		end = min(end, size-1)
	}
	return start, end, true
}

// ixServeRemote starts the little web server for one file: its address,
// and a function that stops it.
func ixServeRemote(f *ixRangeFile) (string, func(), error) {
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return "", nil, err
	}
	f.token = hex.EncodeToString(tok)
	if f.parts == nil {
		f.parts = map[int64][]byte{}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: f, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String() + "/" + f.token, func() { _ = srv.Close() }, nil
}

// ixSampledFrame is one frame read from the remote file.
type ixSampledFrame struct {
	T     float64
	H, HF uint64
}

// ixSampleFrame reads the key frame at or before t seconds of the video at
// address (replaceable in tests).
var ixSampleFrame = func(ctx context.Context, address string, t float64) (ixSampledFrame, bool, error) {
	bin := seekbar.FFmpegPath()
	if bin == "" {
		return ixSampledFrame{}, false, ixErrNoFFmpeg
	}
	// Fixed arguments, no shell. -noaccurate_seek takes the key frame
	// itself (fewer bytes); -copyts and showinfo say which moment it is.
	// ffmpeg's reads never give up before the check does (-rw_timeout; by
	// default it has none, and the little server answers as slowly as
	// Telegram sends the parts).
	cmd := exec.CommandContext(ctx, bin, "-hide_banner", "-nostdin", "-loglevel", "info", "-reconnect", "0",
		"-rw_timeout", strconv.FormatInt(ixSampleHardTimeout.Microseconds(), 10),
		"-noaccurate_seek", "-copyts", "-ss", strconv.FormatFloat(t, 'f', 2, 64), "-i", address,
		"-frames:v", "1", "-an", "-sn", "-dn", "-vf", fmt.Sprintf("scale=%d:%d:flags=area,format=gray,showinfo", ixVSide, ixVSide),
		"-f", "rawvideo", "-pix_fmt", "gray", "pipe:1")
	procutil.HideWindow(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ixSampledFrame{}, false, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return ixSampledFrame{}, false, err
	}
	if err := cmd.Start(); err != nil {
		return ixSampledFrame{}, false, err
	}
	pts, last := -1.0, ""
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			if m := ixPtsRe.FindStringSubmatch(line); m != nil && pts < 0 {
				pts, _ = strconv.ParseFloat(m[1], 64)
			} else if strings.TrimSpace(line) != "" {
				last = line
			}
		}
	}()
	frame := make([]byte, ixVSide*ixVSide)
	_, readErr := io.ReadFull(stdout, frame)
	_, _ = io.Copy(io.Discard, stdout)
	wg.Wait()
	waitErr := cmd.Wait()
	if readErr != nil {
		if waitErr != nil {
			return ixSampledFrame{}, false, fmt.Errorf("ffmpeg: %v: %s", waitErr, strings.TrimSpace(last))
		}
		return ixSampledFrame{}, false, errors.New("ffmpeg read no frame")
	}
	h, hf, ok := ixFrameHashes(frame, ixVSide)
	if pts < 0 {
		pts = t
	}
	return ixSampledFrame{T: pts, H: h, HF: hf}, ok, nil
}

// ixSampleMatch: the share of sampled frames found in a saved fingerprint
// at one time offset. hit is how many matched at the best offset, valid
// how many frames said anything.
func ixSampleMatch(samples []ixSampledFrame, saved []ixVFrame) (hit int, offset float64) {
	type cand struct{ off float64 }
	var offs [][]float64 // per sample: offsets where it matches
	for _, s := range samples {
		var o []float64
		for _, f := range saved {
			if bits.OnesCount64(s.H^f.H) <= ixVdupHamming || bits.OnesCount64(s.HF^f.H) <= ixVdupHamming {
				o = append(o, float64(f.T)-s.T)
			}
		}
		offs = append(offs, o)
	}
	// Each matching offset proposes itself; the one most samples agree
	// with (within the slack) wins.
	for _, o := range offs {
		for _, x := range o {
			n := 0
			for _, other := range offs {
				for _, y := range other {
					if math.Abs(x-y) <= ixSampleSlack {
						n++
						break
					}
				}
			}
			if n > hit {
				hit, offset = n, x
			}
		}
	}
	return hit, offset
}

// ixSampleResult is what a check found.
type ixSampleResult struct {
	Result  string // confirmed, nomatch, failed
	Why     string // failed: ixSampleWhy*
	Note    string
	Of      string  // confirmed: the saved file's key
	Offset  float64 // seconds to add to the queued video's time
	Parts   int
	Samples int
}

// ixSampleCheck samples a queued video and compares it with saved ones.
func (a *App) ixSampleCheck(ctx context.Context, r ixCopyReader, msg *tg.Message, f ixCopyFacts, likeKey string) ixSampleResult {
	att, err := telegram.MessageAttachment(msg)
	if err != nil || f.Dur <= 0 {
		return ixSampleResult{Result: "failed", Why: ixSampleWhyMessage, Note: "the queued message cannot be read"}
	}
	cands := a.ixSampleCandidates(ctx, f, likeKey)
	if len(cands) == 0 {
		return ixSampleResult{Result: "nomatch", Note: "no saved video of a similar length has a fingerprint yet"}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var refreshed bool
	file := &ixRangeFile{size: att.Identity.Size, mime: att.Facts.Mime, get: func(off int64) ([]byte, error) {
		data, err := r.FilePart(att, off)
		if err != nil && !refreshed && strings.Contains(err.Error(), "FILE_REFERENCE") {
			refreshed = true
			if fresh, ferr := r.RefreshMessage(msg); ferr == nil {
				if na, aerr := telegram.MessageAttachment(fresh); aerr == nil && na.Identity.Key() == att.Identity.Key() {
					att = na
					data, err = r.FilePart(att, off)
				}
			}
		}
		return data, err
	}}
	if file.mime == "" {
		file.mime = "video/mp4"
	}
	address, stop, err := ixServeRemote(file)
	if err != nil {
		return ixSampleResult{Result: "failed", Why: ixSampleWhyFFmpeg, Note: err.Error()}
	}
	defer stop()
	// The check's clock: its own time (not the waits for parts) and the
	// time in all.
	started := time.Now()
	clockDone := make(chan struct{})
	defer close(clockDone)
	go func() {
		t := time.NewTicker(ixSampleClockTick)
		defer t.Stop()
		for {
			select {
			case <-clockDone:
				return
			case <-ctx.Done():
				return
			case now := <-t.C:
				switch all := now.Sub(started); {
				case all > ixSampleHardTimeout:
					cancel(errIxSampleHardLimit)
					return
				case all-file.waitedNow(now) > ixSampleTimeout:
					cancel(errIxSampleTimeout)
					return
				}
			}
		}
	}()
	dur := float64(f.Dur) / 1000
	var samples []ixSampledFrame
	var lastErr error
	for i := 0; i < ixSamplePoints; i++ {
		t := dur * (0.05 + 0.9*float64(i)/float64(ixSamplePoints-1))
		s, ok, err := ixSampleFrame(ctx, address, t)
		if err != nil {
			lastErr = err
			if errors.Is(err, ixErrNoFFmpeg) || ctx.Err() != nil || file.failedNow() != nil {
				break
			}
			continue
		}
		if ok {
			samples = append(samples, s)
		}
	}
	res := ixSampleResult{Parts: file.fetchedNow(), Samples: len(samples)}
	if why := file.failedNow(); why != nil || ctx.Err() != nil || len(samples) < ixSampleMinHit {
		res.Result = "failed"
		res.Why = ixSampleWhyOf(why, context.Cause(ctx), lastErr, len(samples))
		switch {
		case res.Why == ixSampleWhyFlood:
			res.Note = "Telegram asked to wait: " + why.Error()
		case res.Why == ixSampleWhyTimeout:
			res.Note = "could not check without downloading: " + context.Cause(ctx).Error()
			if why != nil && context.Cause(ctx) == nil {
				res.Note = "could not check without downloading: " + why.Error()
			}
		case why != nil:
			res.Note = "could not check without downloading: " + why.Error()
		case lastErr != nil:
			res.Note = "could not check without downloading: " + lastErr.Error()
		default:
			res.Note = "too few frames could be read"
		}
		return res
	}
	need := max(ixSampleMinHit, int(math.Ceil(ixSampleShare*float64(len(samples)))))
	best := 0
	for _, c := range cands {
		hit, off := ixSampleMatch(samples, c.frames)
		if hit > best {
			best = hit
		}
		if hit >= need {
			res.Result, res.Of, res.Offset = "confirmed", c.key, off
			res.Note = fmt.Sprintf("%d of %d sampled frames match at %+.0f s", hit, len(samples), off)
			return res
		}
	}
	res.Result, res.Note = "nomatch", fmt.Sprintf("at most %d of %d sampled frames match a saved video", best, len(samples))
	return res
}

func (f *ixRangeFile) fetchedNow() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetched
}

func (f *ixRangeFile) failedNow() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failed
}

type ixSampleCand struct {
	key    string
	frames []ixVFrame
}

// ixSampleCandidates are the saved fingerprints to compare with: the file
// it is held as a copy of first, then those 20% shorter to 20% longer,
// closest in length first.
func (a *App) ixSampleCandidates(ctx context.Context, f ixCopyFacts, likeKey string) []ixSampleCand {
	var out []ixSampleCand
	seen := map[string]bool{}
	add := func(rows *sql.Rows, err error) {
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			var blob []byte
			if rows.Scan(&key, &blob) == nil && !seen[key] {
				seen[key] = true
				if fr := ixUnpackFrames(blob); len(fr) > 0 {
					out = append(out, ixSampleCand{key, fr})
				}
			}
		}
	}
	if likeKey != "" {
		add(a.db.Reader.QueryContext(ctx, `SELECT key, frames FROM ix_vfp WHERE key=? AND status='ok'`, likeKey))
	}
	d := float64(f.Dur) / 1000
	add(a.db.Reader.QueryContext(ctx, `SELECT key, frames FROM ix_vfp WHERE status='ok' AND duration BETWEEN ? AND ? ORDER BY abs(duration-?) LIMIT 300`, d*0.8, d*1.2, d))
	return out
}

// ---------- the round ----------

type ixSampleJob struct {
	work, size int64
	account    string
	body       []byte
	held       bool
	likeKey    string
}

// ixSampleJobs: queued videos of 200 MB or more not checked yet (or whose
// check failed for a reason that may pass, once its wait is over: 1 hour
// after the first failure, doubling up to a day), largest first. Originals of
// videos saved in a lower quality first are not copies (ix_lowq.go): they
// are left out.
func (a *App) ixSampleJobs(ctx context.Context, heldOnly bool, n int) ([]ixSampleJob, error) {
	cond := ""
	if heldOnly {
		cond = " AND h.work_id IS NOT NULL"
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT w.id, w.file_size, w.account_id, w.body, h.work_id IS NOT NULL, COALESCE(h.like_key,'')
		FROM tgdl_work w LEFT JOIN ix_held h ON h.work_id=w.id AND h.released_at=0 AND h.reason<>''
		LEFT JOIN ix_copy_sample s ON s.work_id=w.id
		WHERE w.status='pending' AND w.media_type='video' AND w.file_size>=? AND w.ttl_seconds=0 AND length(w.body)>0
		AND (s.work_id IS NULL OR (s.result='failed' AND s.final=0 AND s.at < ? - MIN(86400000, 3600000*(1<<(MIN(MAX(s.tries,1),6)-1)))))
		AND w.id NOT IN (SELECT work_id FROM ix_lowq WHERE state IN ('saved','released'))`+cond+`
		ORDER BY w.file_size DESC, w.id LIMIT ?`, ixSampleMinSize, time.Now().UnixMilli(), n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ixSampleJob
	for rows.Next() {
		var j ixSampleJob
		if rows.Scan(&j.work, &j.size, &j.account, &j.body, &j.held, &j.likeKey) == nil {
			out = append(out, j)
		}
	}
	return out, rows.Err()
}

// ixSampleRound checks a few big queued videos (heldOnly: only held ones,
// as just before they would be let go).
func (a *App) ixSampleRound(ctx context.Context, heldOnly bool) error {
	if a.ixIsGone() {
		return nil // read-only: nothing may talk to Telegram, and that is no error
	}
	s := a.ixLoadSettings(ctx)
	if !s.HoldLikely || seekbar.FFmpegPath() == "" && !ixSampleFake {
		return nil
	}
	jobs, err := a.ixSampleJobs(ctx, heldOnly, ixSamplesPerRun)
	if err != nil || len(jobs) == 0 {
		return err
	}
	readers := map[string]ixCopyReader{}
	defer func() {
		for _, r := range readers {
			r.Close()
		}
	}()
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg, ok := ixDecodeMessage(j.body)
		if !ok {
			continue
		}
		f, ok := ixCopyFactsOf(msg)
		if !ok {
			continue
		}
		f.ID = j.work
		r := readers[j.account]
		if r == nil {
			if r, err = ixOpenCopyReader(a, ctx, j.account); err != nil {
				return err
			}
			readers[j.account] = r
		}
		res := a.ixSampleCheck(ctx, r, msg, f, j.likeKey)
		if err := a.ixSampleRecord(ctx, j, f, res); err != nil {
			return err
		}
		if res.Result == "failed" && res.Why == ixSampleWhyFlood {
			return nil // Telegram wants the account to wait: the next round
		}
	}
	return nil
}

// ixSampleFake lets tests run without ffmpeg (with ixSampleFrame replaced).
var ixSampleFake = false

func (a *App) ixSampleRecord(ctx context.Context, j ixSampleJob, f ixCopyFacts, res ixSampleResult) error {
	final := res.Result == "failed" && ixSampleFinal(res.Why)
	if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_copy_sample(work_id,ident,size,result,note,of_key,offset_s,parts,samples,tries,at,reason,final) VALUES(?,?,?,?,?,?,?,?,?,1,?,?,?)
		ON CONFLICT(work_id) DO UPDATE SET ident=excluded.ident, size=excluded.size, result=excluded.result, note=excluded.note, of_key=excluded.of_key, offset_s=excluded.offset_s,
		parts=ix_copy_sample.parts+excluded.parts, samples=excluded.samples, tries=ix_copy_sample.tries+1, at=excluded.at, reason=excluded.reason, final=excluded.final`,
		j.work, f.Ident, j.size, res.Result, res.Note, res.Of, res.Offset, res.Parts, res.Samples, ixNowMs(), res.Why, final); err != nil {
		return err
	}
	how := res.Result
	if res.Result == "failed" {
		how += " [" + res.Why + map[bool]string{true: ", final", false: ", tried again later"}[final] + "]"
	}
	a.ixLog("checked %s #%d (%d MB) from %d MB of parts: %s (%s)", f.Group, f.Msg, j.size>>20, res.Parts, how, res.Note)
	if j.held {
		switch res.Result {
		case "confirmed", "nomatch":
			c := map[string]string{"confirmed": "confirmed", "nomatch": "unconfirmed"}[res.Result]
			_, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_copy_hold SET confirm=?, confirm_note=?, checked_at=? WHERE work_id=?`, c, res.Note, ixNowMs(), j.work)
			return err
		}
		return nil
	}
	if res.Result != "confirmed" {
		return nil
	}
	// A big video no other check caught: held, already confirmed.
	path := a.ixStoredPathOfKey(ctx, res.Of)
	if path == "" {
		return nil
	}
	ok, err := a.ixHoldWork(ctx, f, "sampled", path, res.Of, 0, 0, "the same footage as a saved video (sampled frames)")
	if err != nil || !ok {
		return err
	}
	_, err = a.db.Writer.ExecContext(ctx, `UPDATE ix_copy_hold SET confirm='confirmed', confirm_note=?, checked_at=? WHERE work_id=?`, res.Note, ixNowMs(), j.work)
	return err
}

// ixSampleTotals: what the checks cost and found.
type ixSampleTotals struct {
	Checked   int64 `json:"checked"`
	PartsMB   int64 `json:"partsMb"`
	SizeBytes int64 `json:"sizeBytes"`
	Confirmed int64 `json:"confirmed"`
	Failed    int64 `json:"failed"`
	Retry     int64 `json:"retry"` // of the failed, those tried again later
	// FailedBy counts the failed by why (ixSampleWhy*; "" for checks made
	// before the reasons were recorded).
	FailedBy map[string]int64 `json:"failedBy"`
}

func (a *App) ixSampleTotalsNow(ctx context.Context) ixSampleTotals {
	t := ixSampleTotals{FailedBy: map[string]int64{}}
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(parts),0), COALESCE(SUM(size),0), COALESCE(SUM(result='confirmed'),0), COALESCE(SUM(result='failed'),0),
		COALESCE(SUM(result='failed' AND final=0),0) FROM ix_copy_sample`).
		Scan(&t.Checked, &t.PartsMB, &t.SizeBytes, &t.Confirmed, &t.Failed, &t.Retry)
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT reason, COUNT(*) FROM ix_copy_sample WHERE result='failed' GROUP BY reason`); err == nil {
		for rows.Next() {
			var why string
			var n int64
			if rows.Scan(&why, &n) == nil {
				t.FailedBy[why] = n
			}
		}
		rows.Close()
	}
	return t
}

// ixSampleFailedText is FailedBy for the diagnostics report: "timeout 5,
// flood 3" (categories only).
func (t ixSampleTotals) failedText() string {
	var keys []string
	for k := range t.FailedBy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		name := k
		if name == "" {
			name = "unrecorded"
		}
		parts = append(parts, fmt.Sprintf("%s %d", name, t.FailedBy[k]))
	}
	return strings.Join(parts, ", ")
}

// ixStoredPathOfKey is the catalogue's path of a saved file key.
func (a *App) ixStoredPathOfKey(ctx context.Context, key string) string {
	var path string
	if h, ok := strings.CutPrefix(key, "h:"); ok {
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT file_path FROM downloads WHERE lower(file_hash)=? AND file_path IS NOT NULL AND file_path<>'' AND (status IS NULL OR status='completed') ORDER BY id LIMIT 1`, h).Scan(&path)
	} else if p, ok := strings.CutPrefix(key, "p:"); ok {
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT file_path FROM downloads WHERE replace(file_path,char(92),'/')=? AND (status IS NULL OR status='completed') ORDER BY id LIMIT 1`, p).Scan(&path)
	}
	return path
}
