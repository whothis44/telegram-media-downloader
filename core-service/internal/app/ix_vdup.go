package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
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
// (re-encoded, resized, trimmed, watermarked), which SHA-256 cannot see.
//
// Each video gets a visual fingerprint: ffmpeg decodes only its key frames
// (encoders put them at scene changes and every few seconds), each is shrunk
// to 32×32 grey and turned into a 64-bit perceptual hash (DCT, as pHash).
// Two videos are near-duplicates when many of their frames have close
// hashes at a consistent time offset. Candidates are found through 16-bit
// slices of the hashes, so a video is compared only with likely matches.

const (
	ixVdupHamming     = 10   // bits that may differ between two frames of the same footage
	ixVdupMinScore    = 0.6  // share of the shorter video's frames that must match
	ixVdupMaxFrames   = 600  // frames kept per video
	ixVdupCommonBand  = 2000 // a hash slice this common is ignored (black frames, logos)
	ixVdupSameLength  = 0.85 // duration ratio above which two matches are "the same video"
	ixVdupFrameBytes  = 12
	ixVdupMaxDuration = 30 * time.Minute // per-video ffmpeg time limit
)

type ixVFrame struct {
	T float32
	H uint64
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
		binary.LittleEndian.PutUint32(out[i*12:], math.Float32bits(f.T))
		binary.LittleEndian.PutUint64(out[i*12+4:], f.H)
	}
	return out
}

func ixUnpackFrames(b []byte) []ixVFrame {
	n := len(b) / ixVdupFrameBytes
	out := make([]ixVFrame, n)
	for i := range out {
		out[i] = ixVFrame{T: math.Float32frombits(binary.LittleEndian.Uint32(b[i*12:])), H: binary.LittleEndian.Uint64(b[i*12+4:])}
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

// ---------- comparing two videos ----------

type ixVMatch struct {
	Matched int
	Score   float64
	Offset  float64
}

// ixCompareFrames counts frames of the shorter list that have a close hash
// in the other at one consistent time offset (±1.5 s), so a match is the same
// footage in the same order, not a few look-alike shots.
func ixCompareFrames(a, b []ixVFrame, hamming int) ixVMatch {
	if len(a) == 0 || len(b) == 0 {
		return ixVMatch{}
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	type pair struct {
		i   int
		off float64
	}
	var pairs []pair
	votes := map[int]int{}
	for i, fa := range a {
		for _, fb := range b {
			if bits.OnesCount64(fa.H^fb.H) <= hamming {
				off := float64(fb.T - fa.T)
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
	seen := map[int]bool{}
	for _, p := range pairs {
		if math.Abs(p.off-float64(best)) <= 1.5 {
			seen[p.i] = true
		}
	}
	m := ixVMatch{Matched: len(seen), Offset: float64(best)}
	m.Score = float64(m.Matched) / float64(len(a))
	return m
}

// ixVKind says whether two fingerprints are the same video, one a clip of
// the other, or unrelated (""). Videos with one or two usable frames (very
// short or nearly still) must match much more closely.
func ixVKind(a, b []ixVFrame, durA, durB float64) (string, ixVMatch) {
	short := min(len(a), len(b))
	hamming, need := ixVdupHamming, 2
	if short < 3 {
		hamming, need = 4, short
	}
	m := ixCompareFrames(a, b, hamming)
	if m.Matched < need || m.Score < ixVdupMinScore {
		return "", m
	}
	lo, hi := math.Min(durA, durB), math.Max(durA, durB)
	if hi <= 0 || lo/hi >= ixVdupSameLength {
		return "same", m
	}
	return "clip", m
}

// ---------- ffmpeg ----------

var (
	ixPtsRe      = regexp.MustCompile(`pts_time:\s*(-?\d+(?:\.\d+)?)`)
	ixDurationRe = regexp.MustCompile(`Duration:\s*(\d+):(\d+):(\d+(?:\.\d+)?)`)
)

// ixFingerprintVideo is replaceable in tests.
var ixFingerprintVideo = ixFFmpegFingerprint

// ixFFmpegFingerprint decodes the key frames of one video (on the GPU when
// hwaccel is set) into 32×32 grey images and hashes them.
func ixFFmpegFingerprint(ctx context.Context, path, hwaccel string) (float64, []ixVFrame, error) {
	bin := seekbar.FFmpegPath()
	if bin == "" {
		return 0, nil, ixErrNoFFmpeg
	}
	ctx, cancel := context.WithTimeout(ctx, ixVdupMaxDuration)
	defer cancel()
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "info", "-skip_frame", "nokey"}
	if hwaccel != "" {
		args = append(args, "-hwaccel", hwaccel)
	}
	args = append(args, "-i", path, "-an", "-sn", "-dn", "-fps_mode", "passthrough",
		"-vf", "scale=32:32:flags=area,format=gray,showinfo", "-f", "rawvideo", "-pix_fmt", "gray", "pipe:1")
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:]
		}
		return 0, nil, fmt.Errorf("ffmpeg: %v: %s", err, msg)
	}
	stderr := errOut.String()
	var duration float64
	if m := ixDurationRe.FindStringSubmatch(stderr); m != nil {
		h, _ := strconv.ParseFloat(m[1], 64)
		mi, _ := strconv.ParseFloat(m[2], 64)
		s, _ := strconv.ParseFloat(m[3], 64)
		duration = h*3600 + mi*60 + s
	}
	var pts []float64
	for _, m := range ixPtsRe.FindAllStringSubmatch(stderr, -1) {
		if t, err := strconv.ParseFloat(m[1], 64); err == nil {
			pts = append(pts, t)
		}
	}
	raw := out.Bytes()
	n := min(len(raw)/1024, len(pts))
	frames := make([]ixVFrame, 0, n)
	for i := 0; i < n; i++ {
		if h, ok := ixPHash(raw[i*1024 : (i+1)*1024]); ok {
			frames = append(frames, ixVFrame{T: float32(pts[i]), H: h})
		}
	}
	return duration, ixThin(frames, ixVdupMaxFrames), nil
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
		AND NOT EXISTS (SELECT 1 FROM ix_vfp v WHERE v.key = CASE WHEN length(d.file_hash)=64 THEN 'h:'||lower(d.file_hash) ELSE 'p:'||replace(d.file_path,char(92),'/') END)
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
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	status := "ok"
	if failure != "" {
		status = "error"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ix_vfp(key,file_path,duration,frames,status,error,updated_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET file_path=excluded.file_path, duration=excluded.duration, frames=excluded.frames, status=excluded.status, error=excluded.error, updated_at=excluded.updated_at`,
		key, rel, duration, ixPackFrames(frames), status, failure, time.Now().UnixMilli()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM ix_vband WHERE key=?`, key); err != nil {
		return err
	}
	seen := map[[2]int]bool{}
	for _, f := range frames {
		for band := 0; band < 4; band++ {
			v := int((f.H >> (16 * uint(band))) & 0xffff)
			if seen[[2]int{band, v}] {
				continue
			}
			seen[[2]int{band, v}] = true
			if _, err = tx.ExecContext(ctx, `INSERT INTO ix_vband(band,v,key) VALUES(?,?,?)`, band, v, key); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ixVdupMatch compares a new fingerprint with the videos sharing hash slices.
func (a *App) ixVdupMatch(ctx context.Context, key string, duration float64, frames []ixVFrame) error {
	if len(frames) == 0 {
		return nil
	}
	votes := map[string]int{}
	seen := map[[2]int]bool{}
	for _, f := range frames {
		for band := 0; band < 4; band++ {
			v := int((f.H >> (16 * uint(band))) & 0xffff)
			if seen[[2]int{band, v}] {
				continue
			}
			seen[[2]int{band, v}] = true
			var n int
			if err := a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_vband WHERE band=? AND v=?`, band, v).Scan(&n); err != nil {
				return err
			}
			if n > ixVdupCommonBand {
				continue
			}
			rows, err := a.db.Reader.QueryContext(ctx, `SELECT key FROM ix_vband WHERE band=? AND v=? AND key<>?`, band, v, key)
			if err != nil {
				return err
			}
			for rows.Next() {
				var other string
				if rows.Scan(&other) == nil {
					votes[other]++
				}
			}
			rows.Close()
		}
	}
	need := 2
	if len(frames) <= 2 {
		need = 1
	}
	type cand struct {
		key   string
		votes int
	}
	var cands []cand
	for k, n := range votes {
		if n >= need {
			cands = append(cands, cand{k, n})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		return cands[i].votes > cands[j].votes || (cands[i].votes == cands[j].votes && cands[i].key < cands[j].key)
	})
	if len(cands) > 200 {
		cands = cands[:200]
	}
	type found struct {
		other, kind string
		m           ixVMatch
	}
	var hits []found
	for _, c := range cands {
		var blob []byte
		var dur float64
		if err := a.db.Reader.QueryRowContext(ctx, `SELECT frames, duration FROM ix_vfp WHERE key=? AND status='ok'`, c.key).Scan(&blob, &dur); err != nil {
			continue
		}
		if kind, m := ixVKind(frames, ixUnpackFrames(blob), duration, dur); kind != "" {
			hits = append(hits, found{c.key, kind, m})
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
	for _, h := range hits {
		ka, kb := key, h.other
		if kb < ka {
			ka, kb = kb, ka
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO ix_vdup(key_a,key_b,kind,score,matched,offset_s,found_at) VALUES(?,?,?,?,?,?,?)`,
			ka, kb, h.kind, h.m.Score, h.m.Matched, h.m.Offset, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
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
