package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/download"
	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/seekbar"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// ix.12.13 (the user's decision, 11-06): every big video is checked by
// sampled frames as the first step of its own download, not by a separate
// background job that tries to keep ahead. The download worker claims the
// video; the library's Check hook (after the exact re-upload check, before
// staging and the transfer) reads the sample parts through the download's own
// connection and compares the frames with the saved videos' fingerprints:
//   - a probable copy (or, outside ranked groups, a partly-same video) goes
//     back to the queue held at the back (telegram.DownloadDeferred: pending,
//     paused, ix_held and ix_copy_hold written in the same transaction, no
//     attempt spent);
//   - anything else downloads at once, taking the parts the check read (kept
//     on disk as in 12.11, and in memory for this attempt either way).
//
// Any failure of the check (no ffmpeg, no index, the part cap, a timeout, a
// panic) means "download now", never an error. A download that kept a part
// of an earlier attempt is not checked (the library skips the hook). The
// dashboard setting checkInDownload (on by default) turns it off: downloads
// then behave exactly as in 12.12. With it on, the background checks are
// kept only for videos already held (to confirm or release them).

// ixDlCheckKey carries the queue row being downloaded to the library's hook.
type ixDlCheckKey struct{}

type ixDlCheckRow struct {
	work    *engine.Work
	message *tg.Message
	ident   string // set when the check left parts in memory for this attempt
}

// ixWithDlCheck notes the row for the hook (ixIngestWork, only when the
// message downloaded is the queued one, not a lower quality).
func ixWithDlCheck(ctx context.Context, work *engine.Work, message *tg.Message) (context.Context, *ixDlCheckRow) {
	if work == nil || message == nil {
		return ctx, nil
	}
	row := &ixDlCheckRow{work: work, message: message}
	return context.WithValue(ctx, ixDlCheckKey{}, row), row
}

// ixDlCheckHardLimit bounds one check in a download, waits for parts
// included (variable for tests).
var ixDlCheckHardLimit = 10 * time.Minute

// ixDlCheckMax checks run at once in downloads; another waits at most
// ixDlCheckWaitMax for its turn, then downloads unchecked.
const ixDlCheckMax = 6

var (
	ixDlCheckSlots   = make(chan struct{}, ixDlCheckMax)
	ixDlCheckWaitMax = 2 * time.Minute
)

// Why a big video downloaded without a check (counted for the report).
const (
	ixDlSkipChecked  = "checked before"
	ixDlSkipLetGo    = "let go before"
	ixDlSkipNoFFmpeg = "no ffmpeg"
	ixDlSkipNoReader = "no part reader"
	ixDlSkipFacts    = "no duration"
	ixDlSkipHoldFail = "hold failed"
	ixDlSkipPanic    = "program error"
	ixDlSkipLowq     = "lower quality first"
	ixDlSkipBusy     = "checks busy"
)

// ixDlCheckStats: since the start, for the report.
type ixDlCheckStats struct {
	Checked, Copies, Partial, NoMatch, Failed, Held atomic.Int64
	Millis                                          atomic.Int64 // time the checks added, in all
	PartsMB                                         atomic.Int64
	mu                                              sync.Mutex
	failedBy, skipped                               map[string]int64
	holdFailed                                      map[int64]bool // rows whose deferral failed: not deferred again this run
}

var ixDlChecks sync.Map // *App -> *ixDlCheckStats

func ixDlChecksOf(a *App) *ixDlCheckStats {
	v, _ := ixDlChecks.LoadOrStore(a, &ixDlCheckStats{failedBy: map[string]int64{}, skipped: map[string]int64{}, holdFailed: map[int64]bool{}})
	return v.(*ixDlCheckStats)
}

func (t *ixDlCheckStats) skip(why string) {
	t.mu.Lock()
	t.skipped[why]++
	t.mu.Unlock()
}

func (t *ixDlCheckStats) fail(why string) {
	t.Failed.Add(1)
	t.mu.Lock()
	t.failedBy[why]++
	t.mu.Unlock()
}

func ixCountText(m map[string]int64) string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// ixDlCheckLine is the report's line.
func (a *App) ixDlCheckLine(ctx context.Context) string {
	t := ixDlChecksOf(a)
	t.mu.Lock()
	failed, skipped := ixCountText(t.failedBy), ixCountText(t.skipped)
	t.mu.Unlock()
	n := t.Checked.Load()
	avg := 0.0
	if n > 0 {
		avg = float64(t.Millis.Load()) / 1000 / float64(n)
	}
	on := "on"
	if !a.ixLoadSettings(ctx).CheckInDownloadOn() {
		on = "off"
	}
	return fmt.Sprintf("big videos checked in their download (%s) since start: %d (held at the back %d: copies %d, partly the same %d; no match %d; check failed, downloaded anyway %d: %s) · %d MB of parts · %.1f s added a check on average · downloaded without a check: %s",
		on, n, t.Held.Load(), t.Copies.Load(), t.Partial.Load(), t.NoMatch.Load(), t.Failed.Load(), failed, t.PartsMB.Load(), avg, skipped)
}

// ixDlCheck is the library's Check hook. It returns nil (download now) or a
// telegram.DownloadDeferred; nothing else.
func (a *App) ixDlCheck(ctx context.Context, item download.Item, client download.Client) (out error) {
	t := ixDlChecksOf(a)
	defer func() {
		if v := recover(); v != nil {
			out = nil
			t.skip(ixDlSkipPanic)
			a.ixLog("checking a big video in its download: program error, it downloads: %v", v)
		}
	}()
	row, _ := ctx.Value(ixDlCheckKey{}).(*ixDlCheckRow)
	if row == nil || row.work == nil || row.message == nil || item.Type != "video" || a.ixIsGone() {
		return nil
	}
	work := row.work
	if work.Origin != "history" && work.Origin != "live" || item.TTLSeconds > 0 {
		return nil
	}
	s := a.ixLoadSettings(ctx)
	if !s.CheckInDownloadOn() || !s.SampleBigOn() || !s.HoldLikely {
		return nil
	}
	if item.Identity.Size < a.ixSampleFloor(ctx) {
		return nil
	}
	// The queued row itself, not another file (a lower quality, ix_lowq.go).
	if item.Identity.Key() != work.Identity {
		t.skip(ixDlSkipLowq)
		return nil
	}
	t.mu.Lock()
	failedHold := t.holdFailed[work.ID]
	t.mu.Unlock()
	if failedHold {
		t.skip(ixDlSkipHoldFail)
		return nil
	}
	// Let go before (by a person, or by the rules), or held now (a person
	// resumed it): it downloads; a row is never held twice (ixHoldWork).
	// Rows only marked "looked at" by the held step (reason '', released_at
	// -1: every pending big video gets one) are checked.
	var heldRows int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_held WHERE work_id=? AND (released_at>0 OR (released_at=0 AND reason<>''))`, work.ID).Scan(&heldRows)
	if heldRows > 0 {
		t.skip(ixDlSkipLetGo)
		return nil
	}
	f, ok := ixCopyFactsOf(row.message)
	if !ok || f.Dur <= 0 {
		t.skip(ixDlSkipFacts)
		return nil
	}
	f.ID = work.ID
	j := ixSampleJob{work: work.ID, size: item.Identity.Size, account: work.AccountID}
	// Checked before (a background check, or this download's own check
	// before a restart): its answer, without reading anything again.
	// A check that failed for a reason that may pass gets one more try, here.
	var res ixSampleResult
	var result, note, of string
	var best, final, tries int
	recorded := a.db.Reader.QueryRowContext(ctx, `SELECT result, note, of_key, final, tries FROM ix_copy_sample WHERE work_id=?`, work.ID).Scan(&result, &note, &of, &final, &tries) == nil
	fresh := false
	if recorded && !(result == "failed" && final == 0 && tries < 2) {
		// The recorded answer, without reading again: a copy, or a partial
		// match, is deferred (its deferral may not have committed before a
		// restart or a cancel); anything else downloads.
		if result == "nomatch" {
			_, _ = fmt.Sscanf(note, "at most %d of", &best)
		}
		if result != "confirmed" && !(result == "nomatch" && best >= ixSamplePartialHit && of != "") {
			t.skip(ixDlSkipChecked)
			return nil
		}
		res = ixSampleResult{Result: result, Note: note, Of: of, Best: best}
	} else {
		fresh = true
		if seekbar.FFmpegPath() == "" && !ixSampleFake {
			t.skip(ixDlSkipNoFFmpeg)
			return nil
		}
		remote, ok := client.(ixRemoteFile)
		if !ok {
			t.skip(ixDlSkipNoReader)
			return nil
		}
		// The check's time is not the transfer's: the attempt's budget waits.
		if p, ok := client.(interface{ PauseBudget() func() }); ok {
			defer p.PauseBudget()()
		}
		// At most ixDlCheckMax checks at once (a queue of big videos claimed
		// together right after a start must not start dozens of ffmpeg
		// processes); a video that waits too long for one downloads.
		wait := time.NewTimer(ixDlCheckWaitMax)
		select {
		case ixDlCheckSlots <- struct{}{}:
			wait.Stop()
		case <-wait.C:
			t.skip(ixDlSkipBusy)
			return nil
		case <-ctx.Done():
			wait.Stop()
			return nil
		}
		defer func() { <-ixDlCheckSlots }()
		checkCtx, cancel := context.WithTimeout(ctx, ixDlCheckHardLimit)
		defer cancel()
		// Its parts go ahead of the queued transfers, as the background
		// checks' do (engine.MessageSession.FilePart).
		reader := &ixDlReader{ctx: telegram.WithFileCheckRank(checkCtx), remote: remote}
		var mem map[int64][]byte
		checkCtx = context.WithValue(checkCtx, ixSamplePartsSinkKey{}, func(parts map[int64][]byte) { mem = parts })
		started := time.Now()
		res = a.ixSampleCheck(checkCtx, reader, row.message, f, "")
		t.Checked.Add(1)
		t.Millis.Add(time.Since(started).Milliseconds())
		t.PartsMB.Add(int64(res.Parts))
		// In memory only what the disk store did not keep (below its
		// free-space floor, or a failed write): those stay for the whole
		// transfer.
		if len(mem) > 0 {
			ident := ixPartIdent(item.Identity)
			var onDisk int
			_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_part_store WHERE ident=?`, ident).Scan(&onDisk)
			if onDisk < len(mem) {
				ixMemPartsPut(a, ident, mem)
				row.ident = ident
			}
		}
		if ctx.Err() != nil {
			return nil // the download itself was stopped: it says so
		}
		if err := a.ixSampleRecord(ctx, j, f, res); err != nil {
			a.ixLog("checking a big video in its download: could not record the result, it downloads: %v", err)
			return nil
		}
		best = res.Best
	}
	how, reason, confirm := "", "", ""
	switch {
	case res.Result == "confirmed":
		how, reason, confirm = "sampled", "the same footage as a saved video (sampled frames)", "confirmed"
	case res.Result == "nomatch" && best >= ixSamplePartialHit && res.Of != "" && !s.HoldForever && !a.ixGroupRanked(ctx, work.ID):
		how, reason, confirm = "sampled-partial", "partly the same footage as a saved video (sampled frames)", "unconfirmed"
	case res.Result == "failed":
		t.fail(res.Why)
		return nil
	default:
		if fresh {
			t.NoMatch.Add(1)
		}
		return nil
	}
	path := a.ixStoredPathOfKey(ctx, res.Of)
	if path == "" {
		if fresh {
			t.NoMatch.Add(1)
		}
		return nil
	}
	if fresh && how == "sampled" {
		t.Copies.Add(1)
	} else if fresh {
		t.Partial.Add(1)
	}
	id, group, msg, likeKey, note2 := work.ID, work.GroupID, work.MessageID, res.Of, res.Note
	return &telegram.DownloadDeferred{
		Reason: reason,
		Hold: func(ctx context.Context, tx *sql.Tx) error {
			now := time.Now().UnixMilli()
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO ix_held(work_id,group_id,message_id,like_path,like_key,reason,held_at,released_at) VALUES(?,?,?,?,?,?,?,0)`,
				id, group, msg, path, likeKey, reason, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO ix_copy_hold(work_id,how,of_work,sim,confirm,confirm_note,checked_at) VALUES(?,?,0,0,?,?,?)`,
				id, how, confirm, note2, now); err != nil {
				return err
			}
			t.Held.Add(1) // counted when the hold is written; its commit follows in the engine
			return nil
		},
		Failed: func(err error) {
			t.mu.Lock()
			t.holdFailed[id] = true
			t.mu.Unlock()
			a.ixLog("checking a big video in its download: could not hold it at the back, it downloads: %v", err)
		},
	}
}

// ixDlReader reads the parts of the file being downloaded, through the
// download's own client (bound to that file).
type ixDlReader struct {
	ctx    context.Context
	remote ixRemoteFile
}

func (r *ixDlReader) FilePart(_ telegram.Attachment, offset int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Minute)
	defer cancel()
	return r.remote.ReadPart(ctx, offset)
}

func (r *ixDlReader) RefreshMessage(*tg.Message) (*tg.Message, error) {
	// The download renewed the reference when it was claimed if needed; a
	// reference that fails here fails the check, and the file downloads.
	return nil, errors.New("not renewed during a download's check")
}

func (r *ixDlReader) Close() {}

// ixSamplePartsSinkKey: ixSampleCheck hands the parts it read to this
// function as well as keeping them on disk (ix.12.13).
type ixSamplePartsSinkKey struct{}

// Parts a check in a download read, kept in memory for that download's
// transfer whatever the disk store did (below its free-space floor it keeps
// nothing). Dropped when the ingest returns.
var ixMemParts sync.Map // *App -> *sync.Map (ident -> map[int64][]byte)

func ixMemPartsOf(a *App) *sync.Map {
	v, _ := ixMemParts.LoadOrStore(a, &sync.Map{})
	return v.(*sync.Map)
}

func ixMemPartsPut(a *App, ident string, parts map[int64][]byte) {
	ixMemPartsOf(a).Store(ident, parts)
}

func ixMemPartsDrop(a *App, ident string) {
	if ident != "" {
		ixMemPartsOf(a).Delete(ident)
	}
}

func ixMemPartsGet(a *App, ident string) map[int64][]byte {
	if v, ok := ixMemPartsOf(a).Load(ident); ok {
		return v.(map[int64][]byte)
	}
	return nil
}
