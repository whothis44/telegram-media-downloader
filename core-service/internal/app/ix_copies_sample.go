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
	"sync/atomic"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/procutil"
	"github.com/botnick/telegram-media-downloader/core-service/internal/seekbar"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// Confirming big copies without downloading them. For a queued video of
// 200 MB or more (less once none of those waits: ix_sample_floor.go), frames
// are sampled across the whole file: the file on
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
	// A big video no other check held with at least this many sampled
	// frames matching one saved video (but too few for a copy) is partly
	// footage the archive has: it goes to the back of the queue.
	ixSamplePartialHit = 3
	ixSampleSlack      = 2.5 // seconds the offsets of matching frames may differ
)

// ixSampleSameFile: the answer came from an earlier check of the same Telegram file
// (ix_copy_sample.reason), and nothing was read.
const ixSampleSameFile = "same-file"

// ixSampleOrder visits the sample points spread out, so that footage shared with a
// saved video over 3 points or more in a row meets one of the first 5.
var ixSampleOrder = [ixSamplePoints]int{0, 7, 3, 5, 1, 6, 2, 4}

// ixSampleMissAfter frames matching nothing at all end a check.
const ixSampleMissAfter = 5

// Limits of one check (variables for tests).
var (
	ixSampleMaxParts     = 32
	ixSampleTimeout      = 60 * time.Second // not counting the waits for parts
	ixSampleHardTimeout  = 10 * time.Minute // in all: a part read alone may take 5 (engine.filePartWaitLimit)
	ixSampleClockTick    = 250 * time.Millisecond
	ixSamplesPerRun      = 3
	ixSampleReusePerRun  = 200
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
	Of      string  // confirmed: the saved file's key; nomatch: the closest one
	Best    int     // nomatch: the most sampled frames that matched one saved video
	Offset  float64 // seconds to add to the queued video's time
	Parts   int
	Samples int
	Frames  []ixSampledFrame // confirmed, nomatch: the sampled frames, kept to compare again (ix.12.10)
}

// ixSampleDecided says whether more frames could not change the answer: already a
// copy whatever the others show, or too little alike to become a copy or partly
// the same footage. Each frame costs parts of a file under a limit.
func ixSampleDecided(samples []ixSampledFrame, cands []ixSampleCand, left int) bool {
	if len(samples) < ixSampleMinHit {
		return false
	}
	best := 0
	for _, c := range cands {
		hit, _ := ixSampleMatch(samples, c.frames)
		best = max(best, hit)
	}
	switch {
	case best >= max(ixSampleMinHit, int(math.Ceil(ixSampleShare*ixSamplePoints))):
		return true
	case best == 0 && len(samples) >= ixSampleMissAfter:
		return true
	}
	return best+left < min(ixSampleMinHit, ixSamplePartialHit)
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
	outer := ctx
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
	// Whatever the result, the parts read are kept for the download (ix.12.11,
	// ix_part_store.go), once the server below has stopped.
	defer func() {
		if outer.Err() == nil {
			a.ixPartsKeep(outer, ixPartIdent(att.Identity), file.partsNow())
		}
	}()
	address, stop, err := ixServeRemote(file)
	if err != nil {
		return ixSampleResult{Result: "failed", Why: ixSampleWhyFFmpeg, Note: err.Error()}
	}
	defer stop()
	// The check's clock: its own time (not the waits for parts) and the
	// time in all.
	started := time.Now()
	tick, own, all := ixSampleClockTick, ixSampleTimeout, ixSampleHardTimeout
	clockDone, clockGone := make(chan struct{}), make(chan struct{})
	defer func() { close(clockDone); <-clockGone }()
	go func() {
		defer close(clockGone)
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-clockDone:
				return
			case <-ctx.Done():
				return
			case now := <-t.C:
				switch spent := now.Sub(started); {
				case spent > all:
					cancel(errIxSampleHardLimit)
					return
				case spent-file.waitedNow(now) > own:
					cancel(errIxSampleTimeout)
					return
				}
			}
		}
	}()
	dur := float64(f.Dur) / 1000
	var samples []ixSampledFrame
	var lastErr error
	for n, i := range ixSampleOrder {
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
			if ixSampleDecided(samples, cands, ixSamplePoints-n-1) {
				break
			}
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
			// A file request can time out without canceling the check.
			cause := context.Cause(ctx)
			if cause == nil {
				cause = why
			}
			res.Note = "could not check without downloading: " + cause.Error()
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
	best, bestOf := 0, ""
	for _, c := range cands {
		hit, off := ixSampleMatch(samples, c.frames)
		if hit > best {
			best, bestOf = hit, c.key
		}
		if hit >= need {
			res.Frames = samples
			res.Result, res.Of, res.Offset = "confirmed", c.key, off
			res.Note = fmt.Sprintf("%d of %d sampled frames match at %+.0f s", hit, len(samples), off)
			return res
		}
	}
	res.Result, res.Note = "nomatch", fmt.Sprintf("at most %d of %d sampled frames match a saved video", best, len(samples))
	res.Best, res.Of, res.Frames = best, bestOf, samples
	return res
}

// partsNow is a copy of the parts read so far.
func (f *ixRangeFile) partsNow() map[int64][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[int64][]byte, len(f.parts))
	for i, p := range f.parts {
		out[i] = p
	}
	return out
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
// after the first failure, doubling up to a day), those a ranked group needs
// first (ix_sched.go), then largest first. Originals of
// videos saved in a lower quality first are not copies (ix_lowq.go): they
// are left out.
//
// Which rows: ixSampleAll, ixSampleHeld (held ones only, ranked groups first,
// then the largest: what a check can bring back is biggest there) or
// ixSampleNotHeld.
func (a *App) ixSampleJobs(ctx context.Context, which int, n int) ([]ixSampleJob, error) {
	order := ` ORDER BY w.sched_order, w.file_size DESC, w.id`
	if which == ixSampleHeld {
		order = ` ORDER BY w.sched_order>=` + strconv.Itoa(engine.UnrankedOrder) + `, w.file_size DESC, w.id`
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT w.id, w.file_size, w.account_id, w.body, h.work_id IS NOT NULL, COALESCE(h.like_key,'') `+ixSampleDueOf(which)+
		order+` LIMIT ?`, a.ixSampleFloor(ctx), time.Now().UnixMilli(), n)
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

// Which rows ixSampleJobs takes.
const (
	ixSampleAll = iota
	ixSampleHeld
	ixSampleNotHeld
)

// ixSampleHeldEvery: one check in this many goes to a held video (ix.12.5),
// the others to the queue in download order, whose checks spare bytes now.
const ixSampleHeldEvery = 3

// ixSampleTurns lists the rows of the next checks: from the queue in
// download order, with every ixSampleHeldEvery-th from the held videos. When
// one list is empty the other fills its turns.
func (a *App) ixSampleTurns(ctx context.Context, n int) ([]ixSampleJob, error) {
	front, err := a.ixSampleJobs(ctx, ixSampleNotHeld, n)
	if err != nil {
		return nil, err
	}
	held, err := a.ixSampleJobs(ctx, ixSampleHeld, n)
	if err != nil {
		return nil, err
	}
	out := make([]ixSampleJob, 0, len(front)+len(held))
	seq := a.ixSampleSeqOf().Load()
	for len(front) > 0 || len(held) > 0 {
		take := &front
		if (seq%ixSampleHeldEvery == ixSampleHeldEvery-1 && len(held) > 0) || len(front) == 0 {
			take = &held
		}
		out = append(out, (*take)[0])
		*take = (*take)[1:]
		seq++
	}
	return out, nil
}

// ixSampleDue is the rows that may be checked now, as the end of a query.
func ixSampleDue(heldOnly bool) string {
	if heldOnly {
		return ixSampleDueOf(ixSampleHeld)
	}
	return ixSampleDueOf(ixSampleAll)
}

func ixSampleDueOf(which int) string {
	cond := ""
	switch which {
	case ixSampleHeld:
		cond = " AND h.work_id IS NOT NULL"
	case ixSampleNotHeld:
		cond = " AND h.work_id IS NULL"
	}
	return `FROM tgdl_work w LEFT JOIN ix_held h ON h.work_id=w.id AND h.released_at=0 AND h.reason<>''
		LEFT JOIN ix_copy_sample s ON s.work_id=w.id
		WHERE w.status='pending' AND w.media_type='video' AND w.file_size>=? AND w.ttl_seconds=0 AND length(w.body)>0
		AND (s.work_id IS NULL OR (s.result='failed' AND s.final=0 AND s.at < ? - MIN(86400000, 3600000*(1<<(MIN(MAX(s.tries,1),6)-1)))))
		AND w.id NOT IN (SELECT work_id FROM ix_lowq WHERE state IN ('saved','released'))` + cond
}

// ixSampleAnswerFor is the answer an earlier check of the same Telegram file gave.
// The file is the same, and a copy stays a copy. "Not a copy" is passed on only
// when that check compared with the file this row is held as a copy of, or the row
// is not held. Checks that compared nothing or failed say nothing.
func (a *App) ixSampleAnswerFor(ctx context.Context, ident, likeKey string) (ixSampleResult, bool) {
	if ident == "" {
		return ixSampleResult{}, false
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT s.result, s.note, s.of_key, s.offset_s, s.samples, COALESCE(h.like_key,'')
		FROM ix_copy_sample s LEFT JOIN ix_held h ON h.work_id=s.work_id
		WHERE s.ident=? AND s.result IN ('confirmed','nomatch') AND s.samples>0
		ORDER BY s.result='confirmed' DESC, s.at DESC`, ident)
	if err != nil {
		return ixSampleResult{}, false
	}
	defer rows.Close()
	for rows.Next() {
		res := ixSampleResult{Why: ixSampleSameFile}
		var like string
		if rows.Scan(&res.Result, &res.Note, &res.Of, &res.Offset, &res.Samples, &like) != nil {
			continue
		}
		if res.Result == "nomatch" {
			if likeKey != "" && like != likeKey && res.Of != likeKey {
				continue
			}
			_, _ = fmt.Sscanf(res.Note, "at most %d of", &res.Best)
		}
		return res, true
	}
	return ixSampleResult{}, false
}

// ixSampleFileWaits says whether a check of this Telegram file failed and it is
// not time to try again (or never will be): another row of it would fail alike.
func (a *App) ixSampleFileWaits(ctx context.Context, ident string) bool {
	if ident == "" {
		return false
	}
	var n int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_copy_sample WHERE ident=? AND result='failed'
		AND (final=1 OR at >= ? - MIN(86400000, 3600000*(1<<(MIN(MAX(tries,1),6)-1))))`, ident, time.Now().UnixMilli()).Scan(&n)
	return n > 0
}

// ixSampleReuse gives rows the answer already known for their Telegram file. It
// reads nothing from Telegram, so it does not wait for a check's turn.
func (a *App) ixSampleReuse(ctx context.Context, heldOnly bool) error {
	known := map[string]bool{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT DISTINCT ident FROM ix_copy_sample WHERE ident<>'' AND result IN ('confirmed','nomatch') AND samples>0`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var ident string
		if rows.Scan(&ident) == nil {
			known[ident] = true
		}
	}
	rows.Close()
	if len(known) == 0 {
		return nil
	}
	rows, err = a.db.Reader.QueryContext(ctx, `SELECT w.id, w.identity `+ixSampleDue(heldOnly)+`
		ORDER BY w.sched_order, w.file_size DESC, w.id`, a.ixSampleFloor(ctx), time.Now().UnixMilli())
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var identity string
		// The queue writes kind, id and size apart with a NUL, the checks with a colon.
		if rows.Scan(&id, &identity) == nil && known[strings.ReplaceAll(identity, "\x00", ":")] {
			ids = append(ids, id)
		}
	}
	rows.Close()
	done := 0
	for _, id := range ids {
		if done >= ixSampleReusePerRun || ctx.Err() != nil {
			break
		}
		var j ixSampleJob
		if a.db.Reader.QueryRowContext(ctx, `SELECT w.id, w.file_size, w.account_id, w.body, h.work_id IS NOT NULL, COALESCE(h.like_key,'')
			FROM tgdl_work w LEFT JOIN ix_held h ON h.work_id=w.id AND h.released_at=0 AND h.reason<>'' WHERE w.id=?`, id).
			Scan(&j.work, &j.size, &j.account, &j.body, &j.held, &j.likeKey) != nil {
			continue
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
		res, ok := a.ixSampleAnswerFor(ctx, f.Ident, j.likeKey)
		if !ok {
			continue
		}
		if err := a.ixSampleRecord(ctx, j, f, res); err != nil {
			return err
		}
		done++
	}
	if done > 0 {
		a.ixLog("big videos: %d took the answer of an earlier check of the same file", done)
	}
	return nil
}

// ixSampleRound checks a few big queued videos (heldOnly: only held ones,
// as just before they would be let go).
func (a *App) ixSampleRound(ctx context.Context, heldOnly bool) error {
	if a.ixIsGone() {
		return nil // read-only: nothing may talk to Telegram, and that is no error
	}
	s := a.ixLoadSettings(ctx)
	if !s.SampleBigOn() {
		return nil
	}
	if err := a.ixSampleReuse(ctx, heldOnly); err != nil {
		return err
	}
	if ixSampleChecksBusy(a) {
		return nil
	}
	if seekbar.FFmpegPath() == "" && !ixSampleFake {
		return nil
	}
	// More rows than checks: rows of one file are passed over without a check.
	var jobs []ixSampleJob
	var err error
	if heldOnly {
		jobs, err = a.ixSampleJobs(ctx, ixSampleHeld, ixSamplesPerRun*8)
	} else {
		jobs, err = a.ixSampleTurns(ctx, ixSamplesPerRun*8)
	}
	if err != nil || len(jobs) == 0 {
		return err
	}
	checks := 0
	readers := map[string]ixCopyReader{}
	defer func() {
		for _, r := range readers {
			r.Close()
		}
	}()
	for _, j := range jobs {
		if checks >= ixSamplesPerRun {
			return nil
		}
		if ixSampleChecksBusy(a) {
			return nil
		}
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
		if res, ok := a.ixSampleAnswerFor(ctx, f.Ident, j.likeKey); ok {
			if err := a.ixSampleRecord(ctx, j, f, res); err != nil {
				return err
			}
			continue
		}
		if a.ixSampleFileWaits(ctx, f.Ident) {
			continue
		}
		checks++
		a.ixSampleSeqOf().Add(1)
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

// ixSampleSeqOf counts the checks made (ixSampleTurns), per App.
func (a *App) ixSampleSeqOf() *atomic.Int64 {
	v, _ := ixSampleSeqs.LoadOrStore(a, new(atomic.Int64))
	return v.(*atomic.Int64)
}

var ixSampleSeqs sync.Map // *App -> *atomic.Int64

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
	if len(res.Frames) > 0 {
		// Kept so a "no match" can be compared again, here, when more saved
		// videos have their fingerprints (ix_sample_recheck.go).
		if _, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_copy_sample SET frames=?, dur_ms=? WHERE work_id=?`, ixPackSampled(res.Frames), f.Dur, j.work); err != nil {
			return err
		}
	}
	how := res.Result
	if how == "confirmed" {
		how = "matched by sampled frames" // probable, not proven
	}
	if res.Result == "nomatch" && res.Best >= ixSamplePartialHit {
		// Not a copy, but partly the same footage: held at the back below
		// (ix.12.9: the line said only "nomatch").
		how = "partly the same footage, not a copy: at the back of the queue while likely copies are held"
	}
	if res.Result == "failed" {
		how += " [" + res.Why + map[bool]string{true: ", final", false: ", tried again later"}[final] + "]"
	}
	if res.Why != ixSampleSameFile {
		a.ixLog("checked %s #%d (%d MB) from %d MB of parts: %s (%s)", f.Group, f.Msg, j.size>>20, res.Parts, how, res.Note)
	}
	if j.held {
		switch {
		case res.Result == "confirmed":
			// A partial match found to be a copy after all (compared again,
			// ix.12.10) is a probable copy now, not a partial match.
			_, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_copy_hold SET confirm='confirmed', how=CASE WHEN how='sampled-partial' THEN 'sampled' ELSE how END, confirm_note=?, checked_at=? WHERE work_id=?`, res.Note, ixNowMs(), j.work)
			return err
		case res.Result == "nomatch" && res.Best >= ixSamplePartialHit:
			// Partly the same footage: it stays at the back, but as a
			// partial match, not a probable copy (ix.12.5).
			_, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_copy_hold SET how='sampled-partial', confirm='unconfirmed', confirm_note=?, checked_at=? WHERE work_id=?`, res.Note, ixNowMs(), j.work)
			return err
		case res.Result == "nomatch":
			// Its sampled frames match no saved video: the look-alike
			// evidence it was held on is outweighed; it goes back to its
			// place (ix.12.5). A failed check changes nothing.
			if _, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_copy_hold SET confirm='unconfirmed', confirm_note=?, checked_at=? WHERE work_id=?`, res.Note, ixNowMs(), j.work); err != nil {
				return err
			}
			if err := a.ixLetGo(ctx, j.work); err != nil {
				return err
			}
			a.ixLog("held %s #%d goes back to its place: its sampled frames match no saved video (%s)", f.Group, f.Msg, res.Note)
		}
		return nil
	}
	if !a.ixLoadSettings(ctx).HoldLikely {
		return nil // recorded; with holding off nothing moves
	}
	if res.Result == "nomatch" && res.Best >= ixSamplePartialHit && res.Of != "" && !a.ixLoadSettings(ctx).HoldForever {
		// (Held copies kept for ever never download; a partly matching video
		// is not a copy, so then it is not held at all.)
		// Part of it is footage the archive has (another cut, a compilation
		// with it in): not a copy, so it is still downloaded, but after
		// everything else, like the probable copies.
		path := a.ixStoredPathOfKey(ctx, res.Of)
		if path == "" {
			return nil
		}
		ok, err := a.ixHoldWork(ctx, f, "sampled-partial", path, res.Of, 0, 0, "partly the same footage as a saved video (sampled frames)")
		if err != nil || !ok {
			return err
		}
		_, err = a.db.Writer.ExecContext(ctx, `UPDATE ix_copy_hold SET confirm='unconfirmed', confirm_note=?, checked_at=? WHERE work_id=?`, res.Note, ixNowMs(), j.work)
		return err
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
	Retry     int64 `json:"retry"`  // of the failed, those tried again later
	Reused    int64 `json:"reused"` // answered from an earlier check of the same file (not in Checked or SizeBytes)
	// FailedBy counts the failed by why (ixSampleWhy*; "" for checks made
	// before the reasons were recorded).
	FailedBy map[string]int64 `json:"failedBy"`
}

func (a *App) ixSampleTotalsNow(ctx context.Context) ixSampleTotals {
	t := ixSampleTotals{FailedBy: map[string]int64{}}
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(SUM(reason<>?),0), COALESCE(SUM(parts),0), COALESCE(SUM(CASE WHEN reason<>? THEN size ELSE 0 END),0), COALESCE(SUM(result='confirmed'),0), COALESCE(SUM(result='failed'),0),
		COALESCE(SUM(result='failed' AND final=0),0), COALESCE(SUM(reason=?),0) FROM ix_copy_sample`, ixSampleSameFile, ixSampleSameFile, ixSampleSameFile).
		Scan(&t.Checked, &t.PartsMB, &t.SizeBytes, &t.Confirmed, &t.Failed, &t.Retry, &t.Reused)
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
