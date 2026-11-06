package app

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Comparing sampled frames again (ix.12.10). A check of a big queued video
// (ix_copies_sample.go) compares its sampled frames with the fingerprints of
// the saved videos of a similar length that exist at that moment. A video
// whose original is saved later was "no match" for good: nothing looked
// again, and checking it again would cost its parts once more. The sampled
// frames are now kept with the check (ix_copy_sample.frames, about 20 bytes
// a frame), and each time saved videos get their fingerprints (ix_vfp,
// updated_at), the queued videos still waiting with a "no match" are
// compared with them here: no request to Telegram, no byte downloaded.
//
// The rules are the check's: at least ixSampleMinHit frames, and
// ixSampleShare of those read, matching one saved video at one time offset
// make a probable copy; ixSamplePartialHit or more but fewer is partly the
// same footage. The result is recorded and acted on as a check's
// (ixSampleRecord): a copy is held at the back as one, a partial match waits
// at the back too. Checks made before this version kept no frames: they are
// not compared again.

const (
	ixRecheckFlag  = "ix_sample_recheck_vfp" // the last fingerprint time compared (ms)
	ixRecheckBatch = 50                      // new fingerprints per round
)

// ixPackSampled packs sampled frames as the fingerprints are (ixPackFrames).
func ixPackSampled(fs []ixSampledFrame) []byte {
	out := make([]byte, len(fs)*ixVdupFrameBytes)
	for i, f := range fs {
		o := i * ixVdupFrameBytes
		binary.LittleEndian.PutUint32(out[o:], math.Float32bits(float32(f.T)))
		binary.LittleEndian.PutUint64(out[o+4:], f.H)
		binary.LittleEndian.PutUint64(out[o+12:], f.HF)
	}
	return out
}

func ixUnpackSampled(b []byte) []ixSampledFrame {
	vs := ixUnpackFrames(b)
	out := make([]ixSampledFrame, len(vs))
	for i, v := range vs {
		out[i] = ixSampledFrame{T: float64(v.T), H: v.H, HF: v.HF}
	}
	return out
}

// ixSampleRecheckTotal counts the copies and partial matches found by
// comparing again, since the start (for the report).
type ixSampleRecheckTotal struct {
	Compared, Copies, Partial atomic.Int64
}

var ixSampleRechecks sync.Map // *App -> *ixSampleRecheckTotal

// ixSampleRecheck compares the kept frames of the queued "no match" videos
// with the fingerprints made since the last round.
func (a *App) ixSampleRecheck(ctx context.Context) error {
	if !a.ixLoadSettings(ctx).SampleBigOn() {
		return nil
	}
	var mark int64
	var v string
	if a.db.Reader.QueryRowContext(ctx, `SELECT value FROM kv WHERE key=?`, ixRecheckFlag).Scan(&v) != nil {
		// First round: fingerprints from now on (no check kept frames before).
		_, err := a.db.Writer.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO NOTHING`, ixRecheckFlag, strconv.FormatInt(ixNowMs(), 10), ixNowMs())
		return err
	}
	mark, _ = strconv.ParseInt(v, 10, 64)
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT key, duration, frames, updated_at FROM ix_vfp WHERE status='ok' AND updated_at>? ORDER BY updated_at LIMIT ?`, mark, ixRecheckBatch)
	if err != nil {
		return err
	}
	type fp struct {
		key    string
		dur    float64
		frames []ixVFrame
	}
	var fps []fp
	last := mark
	for rows.Next() {
		var f fp
		var blob []byte
		var at int64
		if rows.Scan(&f.key, &f.dur, &blob, &at) == nil {
			f.frames = ixUnpackFrames(blob)
			if len(f.frames) > 0 {
				fps = append(fps, f)
			}
			last = max(last, at)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if last == mark {
		return nil
	}
	// The queued videos waiting with a "no match" and their frames.
	type waiting struct {
		j      ixSampleJob
		dur    float64
		frames []ixSampledFrame
		best   int
	}
	var ws []waiting
	if len(fps) > 0 {
		q, err := a.db.Reader.QueryContext(ctx, `SELECT s.work_id, w.file_size, w.account_id, w.body, h.work_id IS NOT NULL, COALESCE(h.like_key,''), s.dur_ms, s.frames, s.note
			FROM ix_copy_sample s JOIN tgdl_work w ON w.id=s.work_id AND w.status='pending'
			LEFT JOIN ix_held h ON h.work_id=w.id AND h.released_at=0 AND h.reason<>''
			WHERE s.result='nomatch' AND length(s.frames)>0 AND s.dur_ms>0`)
		if err != nil {
			return err
		}
		for q.Next() {
			var w waiting
			var dur int64
			var blob []byte
			var note string
			if q.Scan(&w.j.work, &w.j.size, &w.j.account, &w.j.body, &w.j.held, &w.j.likeKey, &dur, &blob, &note) == nil {
				w.dur, w.frames = float64(dur)/1000, ixUnpackSampled(blob)
				_, _ = fmt.Sscanf(note, "at most %d of", &w.best)
				ws = append(ws, w)
			}
		}
		q.Close()
		if err := q.Err(); err != nil {
			return err
		}
	}
	t := ixSampleRecheckTotalOf(a)
	for _, w := range ws {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(w.frames) < ixSampleMinHit {
			continue
		}
		need := max(ixSampleMinHit, int(math.Ceil(ixSampleShare*float64(len(w.frames)))))
		best, bestOf, bestOff := 0, "", 0.0
		for _, f := range fps {
			if f.dur < w.dur*0.8 || f.dur > w.dur*1.2 {
				continue
			}
			t.Compared.Add(1)
			if hit, off := ixSampleMatch(w.frames, f.frames); hit > best {
				best, bestOf, bestOff = hit, f.key, off
			}
		}
		var res ixSampleResult
		switch {
		case best >= need:
			res = ixSampleResult{Result: "confirmed", Of: bestOf, Offset: bestOff, Samples: len(w.frames), Frames: w.frames,
				Note: fmt.Sprintf("%d of %d sampled frames match at %+.0f s (kept frames compared again with a newly fingerprinted saved video)", best, len(w.frames), bestOff)}
			t.Copies.Add(1)
		case best >= ixSamplePartialHit && w.best < ixSamplePartialHit:
			res = ixSampleResult{Result: "nomatch", Of: bestOf, Best: best, Samples: len(w.frames), Frames: w.frames,
				Note: fmt.Sprintf("at most %d of %d sampled frames match a saved video (kept frames compared again)", best, len(w.frames))}
			t.Partial.Add(1)
		default:
			continue
		}
		msg, ok := ixDecodeMessage(w.j.body)
		if !ok {
			continue
		}
		f, ok := ixCopyFactsOf(msg)
		if !ok {
			continue
		}
		f.ID = w.j.work
		if err := a.ixSampleRecord(ctx, w.j, f, res); err != nil {
			return err
		}
	}
	_, err = a.db.Writer.ExecContext(ctx, `UPDATE kv SET value=?, updated_at=? WHERE key=?`, strconv.FormatInt(last, 10), time.Now().UnixMilli(), ixRecheckFlag)
	return err
}

func ixSampleRecheckTotalOf(a *App) *ixSampleRecheckTotal {
	v, _ := ixSampleRechecks.LoadOrStore(a, &ixSampleRecheckTotal{})
	return v.(*ixSampleRecheckTotal)
}

// ixSampleRateDiag is the report's line on the pace of the checks (ix.12.10):
// checks made from Telegram's parts in the last hour and day, the parts they
// read, the copies and partial matches they found, how many wait for a
// check, and what comparing kept frames again found since the start.
func (a *App) ixSampleRateDiag(ctx context.Context) string {
	now := ixNowMs()
	window := func(since int64) string {
		var n, parts, copies, partial int64
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(parts),0), COALESCE(SUM(result='confirmed'),0), COALESCE(SUM(result='nomatch' AND (note LIKE 'at most 3 %' OR note LIKE 'at most 4 %')),0)
			FROM ix_copy_sample WHERE at>=? AND reason<>? AND parts>0`, since, ixSampleSameFile).Scan(&n, &parts, &copies, &partial)
		return fmt.Sprintf("%d checks, %d MB of parts, %d copies, %d partly the same", n, parts, copies, partial)
	}
	var due int64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) `+ixSampleDue(false), ixSampleMinSize, now).Scan(&due)
	t := ixSampleRecheckTotalOf(a)
	return fmt.Sprintf("  big-video checks: last hour %s · last 24 h %s · waiting for a check %d · turn while downloads are busy %s in %s · kept frames compared again since start: %d comparisons, %d copies, %d partly the same",
		window(now-3600_000), window(now-86400_000), due, ixRemoteBudgetSample, ixRemoteBudgetWindow, t.Compared.Load(), t.Copies.Load(), t.Partial.Load())
}
