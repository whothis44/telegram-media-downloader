package app

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/seekbar"
)

// Near-duplicate videos: the same footage saved as different files
// (re-encoded, resized, letterboxed, mirrored, trimmed), which SHA-256
// cannot see.
//
// Each video is sampled on a fixed time grid: two frames a second for
// videos up to 30 s, one a second up to 20 minutes, one every two seconds
// beyond (the published norm is about one a second; key frames, used
// before ix.6, sit at different moments in every encode). Each frame has
// any flat border cut off, is shrunk to 32×32 grey and gets a 64-bit DCT
// hash (as pHash), and a second hash of its mirror image. Candidates come
// from an in-memory index of 16-bit slices of the hashes, also probing
// every slice one bit away. Two videos are compared by voting on the time
// offset between matching frames, then finding the longest run of matching
// frames along that offset. The run must last at least 5 s (or half of a
// short video); "same" means it covers 80% of both videos, "clip" 90% of
// the shorter one only, and "overlap" anything else they share.

const (
	ixVdupHamming     = 8    // bits that may differ between two frames of the same footage
	ixVdupShortHam    = 5    // the same, for videos with three samples or fewer
	ixVdupMinRatio    = 0.6  // share of samples inside a run that must match
	ixVdupMinRun      = 5.0  // seconds a shared run must last (or half a short video)
	ixVdupSameCover   = 0.8  // both videos covered this much: the same video
	ixVdupClipCover   = 0.9  // the shorter covered this much: a clip of the longer
	ixVdupMaxFrames   = 3600 // frames kept per video
	ixVdupIndexFrames = 256  // frames per video in the candidate index
	ixVdupFrameBytes  = 20
	ixVdupMaxDuration = 60 * time.Minute // per-video ffmpeg time limit
	ixVdupVersion     = 2
	ixVSide           = 96 // ffmpeg's frame size before borders are cut
)

type ixVFrame struct {
	T  float32
	H  uint64 // hash of the frame
	HF uint64 // hash of its mirror image
}

// ---------- perceptual hash ----------

var ixDCT = func() [32][32]float64 {
	var c [32][32]float64
	for k := 0; k < 32; k++ {
		for n := 0; n < 32; n++ {
			c[k][n] = math.Cos(math.Pi / 32 * (float64(n) + 0.5) * float64(k))
		}
	}
	return c
}()

// ixPHash hashes a 32×32 8-bit grey image. ok is false for a nearly flat
// frame (a fade, a black or white screen): its hash says nothing.
func ixPHash(px []byte) (uint64, bool) {
	if len(px) < 1024 {
		return 0, false
	}
	var mean, sq float64
	for _, p := range px[:1024] {
		mean += float64(p)
	}
	mean /= 1024
	for _, p := range px[:1024] {
		d := float64(p) - mean
		sq += d * d
	}
	if math.Sqrt(sq/1024) < 6 {
		return 0, false
	}
	// Rows, then the first 8 coefficients of each column.
	var rows [32][8]float64
	for y := 0; y < 32; y++ {
		for k := 0; k < 8; k++ {
			s := 0.0
			for x := 0; x < 32; x++ {
				s += float64(px[y*32+x]) * ixDCT[k][x]
			}
			rows[y][k] = s
		}
	}
	var coef [64]float64
	for k := 0; k < 8; k++ {
		for l := 0; l < 8; l++ {
			s := 0.0
			for y := 0; y < 32; y++ {
				s += rows[y][l] * ixDCT[k][y]
			}
			coef[k*8+l] = s
		}
	}
	sorted := append([]float64(nil), coef[1:]...)
	sort.Float64s(sorted)
	median := (sorted[31] + sorted[32]) / 2
	var h uint64
	for i, c := range coef {
		if c > median {
			h |= 1 << uint(i)
		}
	}
	return h, true
}

func ixPackFrames(frames []ixVFrame) []byte {
	out := make([]byte, len(frames)*ixVdupFrameBytes)
	for i, f := range frames {
		o := i * ixVdupFrameBytes
		binary.LittleEndian.PutUint32(out[o:], math.Float32bits(f.T))
		binary.LittleEndian.PutUint64(out[o+4:], f.H)
		binary.LittleEndian.PutUint64(out[o+12:], f.HF)
	}
	return out
}

func ixUnpackFrames(b []byte) []ixVFrame {
	n := len(b) / ixVdupFrameBytes
	out := make([]ixVFrame, n)
	for i := range out {
		o := i * ixVdupFrameBytes
		out[i] = ixVFrame{T: math.Float32frombits(binary.LittleEndian.Uint32(b[o:])), H: binary.LittleEndian.Uint64(b[o+4:]), HF: binary.LittleEndian.Uint64(b[o+12:])}
	}
	return out
}

// ixThin keeps at most max frames, evenly spread.
func ixThin(frames []ixVFrame, max int) []ixVFrame {
	if len(frames) <= max {
		return frames
	}
	out := make([]ixVFrame, 0, max)
	for i := 0; i < max; i++ {
		out = append(out, frames[i*len(frames)/max])
	}
	return out
}

// ---------- frames ----------

// ixTrimBorders finds the picture inside dark borders (letterbox and
// pillarbox bars) of a side×side grey frame: dark rows and columns whose
// pixels barely vary are cut from each edge, at most 35% per side. Flat
// areas that are not dark (a white wall, coloured bars) are picture.
func ixTrimBorders(px []byte, side int) (x0, y0, x1, y1 int) {
	x0, y0, x1, y1 = 0, 0, side, side
	flatRow := func(y, xa, xb int) bool {
		var sum, sq float64
		n := float64(xb - xa)
		for x := xa; x < xb; x++ {
			v := float64(px[y*side+x])
			sum += v
			sq += v * v
		}
		m := sum / n
		return m < 40 && sq/n-m*m < 16
	}
	flatCol := func(x, ya, yb int) bool {
		var sum, sq float64
		n := float64(yb - ya)
		for y := ya; y < yb; y++ {
			v := float64(px[y*side+x])
			sum += v
			sq += v * v
		}
		m := sum / n
		return m < 40 && sq/n-m*m < 16
	}
	limit := side * 35 / 100
	for y0 < limit && flatRow(y0, x0, x1) {
		y0++
	}
	for side-y1 < limit && flatRow(y1-1, x0, x1) {
		y1--
	}
	for x0 < limit && flatCol(x0, y0, y1) {
		x0++
	}
	for side-x1 < limit && flatCol(x1-1, y0, y1) {
		x1--
	}
	return
}

// ixResample32 shrinks the region [x0,x1)×[y0,y1) of a side×side frame to
// 32×32 by area averaging.
func ixResample32(px []byte, side, x0, y0, x1, y1 int) []byte {
	out := make([]byte, 1024)
	w, h := float64(x1-x0), float64(y1-y0)
	for i := 0; i < 32; i++ {
		sy0, sy1 := float64(y0)+float64(i)*h/32, float64(y0)+float64(i+1)*h/32
		for j := 0; j < 32; j++ {
			sx0, sx1 := float64(x0)+float64(j)*w/32, float64(x0)+float64(j+1)*w/32
			var sum, wt float64
			for y := int(sy0); y < int(math.Ceil(sy1)) && y < side; y++ {
				wy := math.Min(sy1, float64(y+1)) - math.Max(sy0, float64(y))
				if wy <= 0 {
					continue
				}
				for x := int(sx0); x < int(math.Ceil(sx1)) && x < side; x++ {
					wx := math.Min(sx1, float64(x+1)) - math.Max(sx0, float64(x))
					if wx <= 0 {
						continue
					}
					sum += float64(px[y*side+x]) * wx * wy
					wt += wx * wy
				}
			}
			if wt > 0 {
				out[i*32+j] = byte(math.Round(sum / wt))
			}
		}
	}
	return out
}

// ixFrameHashes hashes one side×side grey frame and its mirror image. ok is
// false for frames that say nothing (flat, or nearly all bits alike).
func ixFrameHashes(px []byte, side int) (h, hf uint64, ok bool) {
	if len(px) < side*side {
		return 0, 0, false
	}
	x0, y0, x1, y1 := ixTrimBorders(px, side)
	small := ixResample32(px, side, x0, y0, x1, y1)
	if h, ok = ixPHash(small); !ok {
		return 0, 0, false
	}
	if pop := bits.OnesCount64(h); pop < 8 || pop > 56 {
		return 0, 0, false
	}
	mirror := make([]byte, 1024)
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			mirror[y*32+x] = small[y*32+31-x]
		}
	}
	hf, _ = ixPHash(mirror)
	return h, hf, true
}

// ixGridStep is the sampling step, in half seconds, for a video's length.
func ixGridStep(duration float64) int {
	switch {
	case duration <= 30:
		return 1
	case duration <= 1200:
		return 2
	}
	return 4
}

// ixOnGrid keeps the frames (sampled every half second) that fall on the
// grid for the video's length, so copies of similar length are sampled at
// the same moments.
func ixOnGrid(frames []ixVFrame, duration float64) []ixVFrame {
	step := ixGridStep(duration)
	if step == 1 {
		return frames
	}
	out := frames[:0:0]
	for _, f := range frames {
		if int(math.Round(float64(f.T)*2))%step == 0 {
			out = append(out, f)
		}
	}
	return out
}

// ---------- comparing two videos ----------

type ixVMatch struct {
	Matched int
	Score   float64 // share of samples matching inside the shared run
	Offset  float64 // seconds to add to a's time to get b's
	Span    float64 // seconds of footage shared
	CoverA  float64 // share of a's length the run covers
	CoverB  float64
}

func ixVInterval(f []ixVFrame) float64 {
	if len(f) < 2 {
		return 1
	}
	var gaps []float64
	for i := 1; i < len(f); i++ {
		if d := float64(f[i].T - f[i-1].T); d > 0 {
			gaps = append(gaps, d)
		}
	}
	if len(gaps) == 0 {
		return 1
	}
	sort.Float64s(gaps)
	return gaps[len(gaps)/2]
}

func ixVLength(f []ixVFrame, dur float64) float64 {
	if dur > 0 {
		return dur
	}
	if len(f) == 0 {
		return 0
	}
	return float64(f[len(f)-1].T) + ixVInterval(f)
}

// ixVRun finds the longest run of x's samples matching y at one offset.
// mirror compares x's mirror-image hashes.
func ixVRun(x, y []ixVFrame, mirror bool) ixVMatch {
	tau := ixVdupHamming
	if len(x) <= 3 || len(y) <= 3 {
		tau = ixVdupShortHam
	}
	type pair struct {
		i   int
		off float64
	}
	var pairs []pair
	votes := map[int]int{}
	for i, fx := range x {
		hx := fx.H
		if mirror {
			hx = fx.HF
		}
		for _, fy := range y {
			if bits.OnesCount64(hx^fy.H) <= tau {
				off := float64(fy.T - fx.T)
				pairs = append(pairs, pair{i, off})
				votes[int(math.Round(off))]++
			}
		}
	}
	if len(pairs) == 0 {
		return ixVMatch{}
	}
	best, bestVotes := 0, -1
	for off := range votes {
		n := votes[off-1] + votes[off] + votes[off+1]
		if n > bestVotes || (n == bestVotes && off < best) {
			best, bestVotes = off, n
		}
	}
	matched := make([]bool, len(x))
	for _, p := range pairs {
		if math.Abs(p.off-float64(best)) <= 1.5 {
			matched[p.i] = true
		}
	}
	interval := ixVInterval(x)
	// A run may skip a stretch that hashes badly (noise, a flash): up to
	// three samples or four seconds, whichever is more.
	allowGap := max(3, int(4/interval))
	var top ixVMatch
	start, last, gap, hit := -1, -1, 0, 0
	closeRun := func() {
		if start < 0 || last < start {
			return
		}
		total := last - start + 1
		ratio := float64(hit) / float64(total)
		span := float64(x[last].T-x[start].T) + interval
		if ratio >= ixVdupMinRatio && hit >= min(2, len(x)) && span > top.Span {
			top = ixVMatch{Matched: hit, Score: ratio, Offset: float64(best), Span: span}
		}
	}
	for i := range x {
		if matched[i] {
			if start < 0 {
				start, hit = i, 0
			}
			last, gap = i, 0
			hit++
			continue
		}
		if start >= 0 {
			gap++
			if gap > allowGap {
				closeRun()
				start, last, gap, hit = -1, -1, 0, 0
			}
		}
	}
	closeRun()
	return top
}

// ixVCompare says whether two fingerprints share footage: "same" (the same
// video), "clip" (the shorter is part of the longer), "overlap" (they share
// a stretch) or "" (unrelated).
func ixVCompare(a, b []ixVFrame, durA, durB float64) (string, ixVMatch) {
	if len(a) == 0 || len(b) == 0 {
		return "", ixVMatch{}
	}
	lenA, lenB := ixVLength(a, durA), ixVLength(b, durB)
	// Walk the video with fewer samples; both orientations.
	x, y, swapped := a, b, false
	if len(b) < len(a) {
		x, y, swapped = b, a, true
	}
	m := ixVRun(x, y, false)
	if mm := ixVRun(x, y, true); mm.Span > m.Span {
		m = mm
	}
	if m.Matched == 0 {
		return "", m
	}
	if swapped {
		m.Offset = -m.Offset
	}
	short := math.Min(lenA, lenB)
	if m.Span < ixVdupMinRun && !(short < 2*ixVdupMinRun && m.Span >= 0.5*short) {
		return "", m
	}
	if lenA > 0 {
		m.CoverA = math.Min(1, m.Span/lenA)
	}
	if lenB > 0 {
		m.CoverB = math.Min(1, m.Span/lenB)
	}
	coverShort, coverLong := m.CoverA, m.CoverB
	if lenB < lenA {
		coverShort, coverLong = m.CoverB, m.CoverA
	}
	switch {
	case m.CoverA >= ixVdupSameCover && m.CoverB >= ixVdupSameCover:
		return "same", m
	case coverShort >= ixVdupClipCover && coverLong < ixVdupSameCover:
		return "clip", m
	}
	return "overlap", m
}

// ---------- the candidate index ----------

// ixVIndex finds videos sharing hash slices with a new one, in memory.
type ixVIndex struct {
	keys     []string
	byKey    map[string]int32
	postings [4][65536][]int32
}

func newIxVIndex() *ixVIndex { return &ixVIndex{byKey: map[string]int32{}} }

func ixIndexSample(frames []ixVFrame) []ixVFrame {
	return ixThin(frames, ixVdupIndexFrames)
}

func (x *ixVIndex) add(key string, frames []ixVFrame) {
	id, ok := x.byKey[key]
	if !ok {
		id = int32(len(x.keys))
		x.keys = append(x.keys, key)
		x.byKey[key] = id
	}
	seen := map[uint32]bool{}
	for _, f := range ixIndexSample(frames) {
		for band := 0; band < 4; band++ {
			v := uint16(f.H >> (16 * uint(band)))
			k := uint32(band)<<16 | uint32(v)
			if seen[k] {
				continue
			}
			seen[k] = true
			list := x.postings[band][v]
			if n := len(list); n == 0 || list[n-1] != id {
				x.postings[band][v] = append(list, id)
			}
		}
	}
}

// candidates lists indexed videos sharing slices (exactly or one bit away)
// with at least need distinct frames, most shared first. Slices common to
// over 1% of all videos (black frames, logos) are ignored.
func (x *ixVIndex) candidates(key string, frames []ixVFrame, max int) []string {
	sample := ixIndexSample(frames)
	if len(sample) == 0 || len(x.keys) == 0 {
		return nil
	}
	need := min(3, max1(len(sample)/2))
	stop := len(x.keys) / 100
	if stop < 100 {
		stop = 100
	}
	self, hasSelf := x.byKey[key]
	hits := map[int32]int{}
	last := map[int32]int{}
	for qi, f := range sample {
		for _, h := range []uint64{f.H, f.HF} {
			for band := 0; band < 4; band++ {
				v := uint16(h >> (16 * uint(band)))
				for probe := -1; probe < 16; probe++ {
					pv := v
					if probe >= 0 {
						pv ^= 1 << uint(probe)
					}
					list := x.postings[band][pv]
					if len(list) > stop {
						continue
					}
					for _, id := range list {
						if hasSelf && id == self {
							continue
						}
						if l, ok := last[id]; ok && l == qi {
							continue
						}
						last[id] = qi
						hits[id]++
					}
				}
			}
		}
	}
	type cand struct {
		id   int32
		hits int
	}
	var cs []cand
	for id, n := range hits {
		if n >= need {
			cs = append(cs, cand{id, n})
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		return cs[i].hits > cs[j].hits || (cs[i].hits == cs[j].hits && x.keys[cs[i].id] < x.keys[cs[j].id])
	})
	if len(cs) > max {
		cs = cs[:max]
	}
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = x.keys[c.id]
	}
	return out
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// ---------- ffmpeg ----------

var (
	ixPtsRe      = regexp.MustCompile(`pts_time:\s*(-?\d+(?:\.\d+)?)`)
	ixDurationRe = regexp.MustCompile(`Duration:\s*(\d+):(\d+):(\d+(?:\.\d+)?)`)
)

// ixFingerprintVideo is replaceable in tests.
var ixFingerprintVideo = ixFFmpegFingerprint

// ixFFmpegFingerprint decodes a video (on the GPU when hwaccel is set),
// takes two frames a second as 96×96 grey pictures, and hashes the ones on
// the grid for its length. Frames are read as ffmpeg makes them, so a long
// video is never held in memory.
func ixFFmpegFingerprint(ctx context.Context, path, hwaccel string) (float64, []ixVFrame, error) {
	bin := seekbar.FFmpegPath()
	if bin == "" {
		return 0, nil, ixErrNoFFmpeg
	}
	ctx, cancel := context.WithTimeout(ctx, ixVdupMaxDuration)
	defer cancel()
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "info"}
	if hwaccel != "" {
		args = append(args, "-hwaccel", hwaccel)
	}
	args = append(args, "-i", path, "-an", "-sn", "-dn",
		"-vf", fmt.Sprintf("fps=2,scale=%d:%d:flags=area,format=gray,showinfo", ixVSide, ixVSide), "-f", "rawvideo", "-pix_fmt", "gray", "pipe:1")
	cmd := exec.CommandContext(ctx, bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, nil, err
	}
	if err = cmd.Start(); err != nil {
		return 0, nil, err
	}
	var (
		pts      []float64
		duration float64
		lastLine string
		wg       sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if m := ixPtsRe.FindStringSubmatch(line); m != nil {
				if t, err := strconv.ParseFloat(m[1], 64); err == nil {
					pts = append(pts, t)
				}
				continue
			}
			if duration == 0 {
				if m := ixDurationRe.FindStringSubmatch(line); m != nil {
					h, _ := strconv.ParseFloat(m[1], 64)
					mi, _ := strconv.ParseFloat(m[2], 64)
					s, _ := strconv.ParseFloat(m[3], 64)
					duration = h*3600 + mi*60 + s
				}
			}
			if strings.TrimSpace(line) != "" {
				lastLine = line
			}
		}
	}()
	type hashed struct {
		h, hf uint64
		ok    bool
	}
	var hashes []hashed
	frame := make([]byte, ixVSide*ixVSide)
	r := bufio.NewReaderSize(stdout, 1<<20)
	for {
		if _, err := io.ReadFull(r, frame); err != nil {
			break
		}
		h, hf, ok := ixFrameHashes(frame, ixVSide)
		hashes = append(hashes, hashed{h, hf, ok})
	}
	_, _ = io.Copy(io.Discard, r)
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		return 0, nil, fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(lastLine))
	}
	n := min(len(hashes), len(pts))
	frames := make([]ixVFrame, 0, n)
	for i := 0; i < n; i++ {
		if hashes[i].ok {
			frames = append(frames, ixVFrame{T: float32(pts[i]), H: hashes[i].h, HF: hashes[i].hf})
		}
	}
	length := duration
	if length <= 0 && n > 0 {
		length = pts[n-1] + 0.5
	}
	return duration, ixThin(ixOnGrid(frames, length), ixVdupMaxFrames), nil
}

var ixErrNoFFmpeg = errors.New("ffmpeg is not installed")

// ---------- the background worker ----------

type ixVdupStatus struct {
	Running   bool   `json:"running"`
	Current   string `json:"current,omitempty"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
	Errors    int    `json:"errors"`
	Pairs     int    `json:"pairs"`
	HWAccel   string `json:"hwaccel"`
	LastError string `json:"lastError,omitempty"`
	FFmpeg    bool   `json:"ffmpeg"`
	Paused    bool   `json:"paused"`
}

var ixVdupState sync.Map // *App -> *ixVdupBox

type ixVdupBox struct {
	mu        sync.Mutex
	index     *ixVIndex // candidate index, built on first use
	st        ixVdupStatus
	cpuOnly   bool // the GPU decoder failed; use the CPU for the rest of this run
	gpuErrors int
}

func (a *App) ixVdup() *ixVdupBox {
	v, _ := ixVdupState.LoadOrStore(a, &ixVdupBox{})
	return v.(*ixVdupBox)
}

// ixVdupBackground lets tests drive the worker directly.
var ixVdupBackground atomic.Bool

// ixHaveFFmpeg is replaceable in tests.
var ixHaveFFmpeg = func() bool { return seekbar.FFmpegPath() != "" }

func init() { ixVdupBackground.Store(true) }

func (a *App) ixRunVdup(ctx context.Context) {
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		wait := 30 * time.Second
		if ixVdupBackground.Load() {
			worked, err := a.ixVdupStep(ctx)
			if err != nil && ctx.Err() == nil {
				a.ixLog("near-duplicate scan: %v", err)
			}
			if worked {
				wait = 100 * time.Millisecond
			}
		}
		timer.Reset(wait)
	}
}

// ixVdupHW picks the decoder for this video: the configured one, unless the
// GPU failed repeatedly in this run.
func (a *App) ixVdupHW(s ixSettings) string {
	box := a.ixVdup()
	box.mu.Lock()
	defer box.mu.Unlock()
	if box.cpuOnly {
		return ""
	}
	switch s.DupHWAccel {
	case "none":
		return ""
	case "", "cuda":
		return "cuda"
	}
	return allowedThumbAccel(s.DupHWAccel)
}

// ixVdupStep fingerprints one video and records its near-duplicates.
// worked is false when there was nothing to do (or it is paused).
func (a *App) ixVdupStep(ctx context.Context) (bool, error) {
	s := a.ixLoadSettings(ctx)
	box := a.ixVdup()
	box.mu.Lock()
	box.st.Paused = !s.DupEnabled
	box.mu.Unlock()
	if !s.DupEnabled || ixInQuietHours(ixSettings{QuietStart: s.DupQuietStart, QuietEnd: s.DupQuietEnd}, time.Now()) {
		a.ixVdupCount(ctx)
		return false, nil
	}
	if !ixHaveFFmpeg() {
		box.mu.Lock()
		box.st.FFmpeg, box.st.Running = false, false
		box.mu.Unlock()
		return false, nil
	}
	var id int64
	var rel, hash string
	err := a.db.Reader.QueryRowContext(ctx, `SELECT d.id, d.file_path, COALESCE(d.file_hash,'') FROM downloads d
		WHERE d.file_type='video' AND d.message_id>0 AND d.file_path IS NOT NULL AND d.file_path<>'' AND (d.status IS NULL OR d.status='completed')
		AND NOT EXISTS (SELECT 1 FROM ix_vfp v WHERE v.ver>=2 AND v.key = CASE WHEN length(d.file_hash)=64 THEN 'h:'||lower(d.file_hash) ELSE 'p:'||replace(d.file_path,char(92),'/') END)
		ORDER BY d.id LIMIT 1`).Scan(&id, &rel, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		box.mu.Lock()
		box.st.Running, box.st.Current, box.st.FFmpeg = false, "", true
		box.mu.Unlock()
		a.ixVdupCount(ctx)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	rel = ixNormPath(rel)
	key := ixFileKey(hash, rel)
	hw := a.ixVdupHW(s)
	box.mu.Lock()
	box.st.Running, box.st.Current, box.st.FFmpeg, box.st.HWAccel = true, rel, true, map[bool]string{true: hw, false: "cpu"}[hw != ""]
	box.mu.Unlock()
	full := filepath.Join(a.downloadsDir, filepath.FromSlash(rel))
	if rel == "" {
		return true, a.ixVdupSave(ctx, key, rel, 0, nil, "bad path")
	}
	if _, err := os.Stat(full); err != nil {
		// The NAS may be offline: try again later rather than marking it.
		if _, rootErr := os.Stat(a.downloadsDir); rootErr != nil {
			return false, nil
		}
		return true, a.ixVdupSave(ctx, key, rel, 0, nil, "file not found")
	}
	duration, frames, err := ixFingerprintVideo(ctx, full, hw)
	if err != nil && hw != "" && ctx.Err() == nil {
		// GPU decoding can fail on some files or drivers: the CPU always works.
		box.mu.Lock()
		box.gpuErrors++
		if box.gpuErrors >= 3 {
			box.cpuOnly = true
		}
		box.mu.Unlock()
		duration, frames, err = ixFingerprintVideo(ctx, full, "")
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if errors.Is(err, ixErrNoFFmpeg) {
		box.mu.Lock()
		box.st.FFmpeg = false
		box.mu.Unlock()
		return false, nil
	}
	if err != nil {
		box.mu.Lock()
		box.st.LastError = err.Error()
		box.mu.Unlock()
		return true, a.ixVdupSave(ctx, key, rel, 0, nil, err.Error())
	}
	if err = a.ixVdupSave(ctx, key, rel, duration, frames, ""); err != nil {
		return true, err
	}
	if err = a.ixVdupMatch(ctx, key, duration, frames); err != nil {
		return true, err
	}
	a.ixVdupCount(ctx)
	return true, nil
}

func (a *App) ixVdupSave(ctx context.Context, key, rel string, duration float64, frames []ixVFrame, failure string) error {
	status := "ok"
	if failure != "" {
		status = "error"
	}
	_, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_vfp(key,file_path,duration,frames,status,error,updated_at,ver) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET file_path=excluded.file_path, duration=excluded.duration, frames=excluded.frames, status=excluded.status, error=excluded.error, updated_at=excluded.updated_at, ver=excluded.ver`,
		key, rel, duration, ixPackFrames(frames), status, failure, time.Now().UnixMilli(), ixVdupVersion)
	return err
}

// ixVdupIndex builds the in-memory candidate index once per run.
func (a *App) ixVdupIndex(ctx context.Context) (*ixVIndex, error) {
	box := a.ixVdup()
	box.mu.Lock()
	idx := box.index
	box.mu.Unlock()
	if idx != nil {
		return idx, nil
	}
	idx = newIxVIndex()
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT key, frames FROM ix_vfp WHERE status='ok' AND ver=?`, ixVdupVersion)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key string
		var blob []byte
		if rows.Scan(&key, &blob) == nil {
			idx.add(key, ixUnpackFrames(blob))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	box.mu.Lock()
	box.index = idx
	box.mu.Unlock()
	return idx, nil
}

// ixVdupMatch compares a new fingerprint with the likely matches the index
// finds, records what it shares with them, and adds it to the index.
func (a *App) ixVdupMatch(ctx context.Context, key string, duration float64, frames []ixVFrame) error {
	if len(frames) == 0 {
		return nil
	}
	idx, err := a.ixVdupIndex(ctx)
	if err != nil {
		return err
	}
	box := a.ixVdup()
	box.mu.Lock()
	cands := idx.candidates(key, frames, 300)
	box.mu.Unlock()
	type found struct {
		other, kind string
		m           ixVMatch
	}
	var hits []found
	for _, other := range cands {
		var blob []byte
		var dur float64
		if err := a.db.Reader.QueryRowContext(ctx, `SELECT frames, duration FROM ix_vfp WHERE key=? AND status='ok'`, other).Scan(&blob, &dur); err != nil {
			continue
		}
		if kind, m := ixVCompare(frames, ixUnpackFrames(blob), duration, dur); kind != "" {
			hits = append(hits, found{other, kind, m})
		}
	}
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM ix_vdup WHERE key_a=? OR key_b=?`, key, key); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, h := range hits {
		ka, kb, ca, cb, off := key, h.other, h.m.CoverA, h.m.CoverB, h.m.Offset
		if kb < ka {
			ka, kb, ca, cb, off = kb, ka, cb, ca, -off
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO ix_vdup(key_a,key_b,kind,score,matched,offset_s,found_at,cover_a,cover_b) VALUES(?,?,?,?,?,?,?,?,?)`,
			ka, kb, h.kind, h.m.Score, h.m.Matched, off, now, ca, cb); err != nil {
			return err
		}
		if h.kind == "overlap" {
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO ix_msug(key_a,key_b,kind,distance,found_at) VALUES(?,?,'video',?,?)`, ka, kb, h.m.Score, now); err != nil {
				return err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	box.mu.Lock()
	idx.add(key, frames)
	box.mu.Unlock()
	return nil
}

func (a *App) ixVdupCount(ctx context.Context) {
	var total, done, errs, pairs int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(DISTINCT CASE WHEN length(file_hash)=64 THEN 'h:'||lower(file_hash) ELSE 'p:'||replace(file_path,char(92),'/') END)
		FROM downloads WHERE file_type='video' AND message_id>0 AND file_path IS NOT NULL AND file_path<>'' AND (status IS NULL OR status='completed')`).Scan(&total)
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(status='error'),0) FROM ix_vfp`).Scan(&done, &errs)
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_vdup`).Scan(&pairs)
	box := a.ixVdup()
	box.mu.Lock()
	box.st.Total, box.st.Done, box.st.Errors, box.st.Pairs = total, min(done, total), errs, pairs
	box.mu.Unlock()
}

func (a *App) ixVdupStatus() ixVdupStatus {
	box := a.ixVdup()
	box.mu.Lock()
	defer box.mu.Unlock()
	return box.st
}

// ---------- results ----------

type ixNear struct {
	Key  string
	Kind string // same, clip
}

// ixNearMap lists, per file key, the other files that are near-duplicates.
func (a *App) ixNearMap(ctx context.Context) map[string][]ixNear {
	out := map[string][]ixNear{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT key_a, key_b, kind FROM ix_vdup`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var ka, kb, kind string
		if rows.Scan(&ka, &kb, &kind) == nil {
			out[ka] = append(out[ka], ixNear{kb, kind})
			out[kb] = append(out[kb], ixNear{ka, kind})
		}
	}
	return out
}

// ixNearRoots groups "same video" matches into clusters (union-find): every
// key maps to its cluster's representative. Clips are not merged.
func ixNearRoots(near map[string][]ixNear) map[string]string {
	parent := map[string]string{}
	var find func(string) string
	find = func(k string) string {
		p, ok := parent[k]
		if !ok || p == k {
			parent[k] = k
			return k
		}
		r := find(p)
		parent[k] = r
		return r
	}
	keys := make([]string, 0, len(near))
	for k := range near {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, n := range near[k] {
			if n.Kind != "same" {
				continue
			}
			ra, rb := find(k), find(n.Key)
			if ra != rb {
				if rb < ra {
					ra, rb = rb, ra
				}
				parent[rb] = ra
			}
		}
	}
	out := map[string]string{}
	for _, k := range keys {
		out[k] = find(k)
	}
	return out
}

// ixNearPlaces finds where each near-duplicate file appears.
func (a *App) ixNearPlaces(ctx context.Context) map[string][]ixPlace {
	out := map[string][]ixPlace{}
	rows, err := a.db.Reader.QueryContext(ctx, `WITH k AS (
			SELECT CASE WHEN length(file_hash)=64 THEN 'h:'||lower(file_hash) ELSE 'p:'||replace(file_path,char(92),'/') END AS key, group_id, message_id
			FROM downloads WHERE file_type='video' AND message_id>0 AND file_path IS NOT NULL AND file_path<>'' AND (status IS NULL OR status='completed'))
		SELECT key, group_id, message_id FROM k WHERE key IN (SELECT key_a FROM ix_vdup UNION SELECT key_b FROM ix_vdup) ORDER BY key, group_id, message_id`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var p ixPlace
		if rows.Scan(&key, &p.GroupID, &p.MessageID) == nil {
			if strings.HasPrefix(key, "p:") {
				key = "p:" + ixNormPath(strings.TrimPrefix(key, "p:"))
			}
			out[key] = append(out[key], p)
		}
	}
	return out
}
