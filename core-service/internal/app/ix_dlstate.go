package app

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
)

// What downloads are doing, in one word and one sentence.
//
// The dashboard's queue figure counts only the files the engine could take
// now. Most of an archive's queue waits on purpose (held copies, files set
// aside by the deadline order, originals after a lower quality, big files
// waiting for their lane), and the rescan adds files a group at a time, so
// "0 queued" with files still finishing says nothing about whether all is
// well. ixDlState says it: one verdict, why the rest waits, and how far the
// rescan is. The dashboard status bar (admins only, like the throughput),
// the Archive overview and the diagnostics report all read it from here.
//
// The verdict is the first that applies:
//
//   - readonly: the archive is read-only (ixIsGone). Checked before paused:
//     a gone account stops the engine, and "start the engine" would be the
//     wrong advice then;
//   - paused: the download queue is paused, or no engine runs;
//   - stalled: work that goes by itself waits (ready, downloading, or set
//     aside other than by a person or held for ever), yet no bytes arrived
//     and no file finished for ixDlStallAfter;
//   - speed-limited: Telegram asked the account to wait in the last 10
//     minutes (its speed limit, FLOOD_PREMIUM_WAIT, or an ordinary wait)
//     and downloads still move;
//   - filling: nothing ready, at most ixDlFillingMax downloading, and the
//     rescan (or a backfill) is adding files, or waits to go on by itself;
//   - done: nothing ready, downloading, waiting to retry or set aside to go
//     by itself, and no rescan left to run. Files a person paused and
//     copies held for ever may still wait: they wait for a person;
//   - downloading: anything else.

const (
	ixDlStallAfter = 10 * time.Minute // no bytes, no finished file this long with work waiting: stalled
	ixDlFillingMax = 2                // downloading at most this many, nothing ready, rescan adding: filling
	// The deadline order's safety net (ixDeadlineStep): after this long
	// without movement it lets every set-aside file go for 2 hours. Its
	// check runs every ixDlNetTick.
	ixDlNetAfter = 30 * time.Minute
	ixDlNetTick  = 2 * time.Minute
)

// ixDlStateTTL: the status is measured at most this often (the status bar
// push asks every 3 s; one measure is a scan of the queue). Tests set 0.
var ixDlStateTTL = 5 * time.Second

// ixDlPace is the download pacing the verdict reads (replaced in tests).
var ixDlPace = func(a *App) ixPace { return a.ixPaceNow() }

// Verdicts.
const (
	ixDlPaused       = "paused"
	ixDlReadOnly     = "readonly"
	ixDlStalled      = "stalled"
	ixDlSpeedLimited = "speed-limited"
	ixDlFilling      = "filling"
	ixDlDownloading  = "downloading"
	ixDlDone         = "done"
)

// ixDlAside: why the files set aside wait. A file with several reasons
// counts once, under the first in this order.
type ixDlAside struct {
	Copies       int `json:"copies"`       // likely copies, held back (ix_held)
	Deadline     int `json:"deadline"`     // private chats and smaller files first (ix_deferred)
	LowerQuality int `json:"lowerQuality"` // originals waiting for a lower quality first (ix_lowq)
	BigLane      int `json:"bigLane"`      // big files waiting their turn (engine.BigFileLane)
	ByYou        int `json:"byYou"`        // paused by a person
}

type ixDlRescan struct {
	On        bool   `json:"on"`        // the rescan queue is switched on
	Adding    bool   `json:"adding"`    // a pass runs or is due soon, or a backfill queues files
	Held      bool   `json:"held"`      // on, but no group starts now (quiet hours, a safety pause)
	QueueFull bool   `json:"queueFull"` // a pass waits for room in the download queue
	Total     int    `json:"total"`     // groups in the rescan queue
	Done      int    `json:"done"`
	Failed    int    `json:"failed"`
	Left      int    `json:"left"`            // waiting or running
	Unread    int    `json:"unread"`          // of those, messages not all read yet
	Group     string `json:"group,omitempty"` // the group being read (the user's own pages only)
	Phase     string `json:"phase,omitempty"` // its pass: messages or files
	NotQueued int    `json:"notQueued"`       // files found missing in groups whose files pass is still to come
	groupID   string // for the diagnostics report's G<n>
}

type ixDlState struct {
	Verdict     string    `json:"verdict"`
	Advice      string    `json:"advice"` // one sentence: whether to do anything
	Act         bool      `json:"act"`    // the advice asks for something
	Ready       int       `json:"ready"`  // the engine could start these now
	Downloading int       `json:"downloading"`
	RetryLater  int       `json:"retryLater"` // failed for now, tried again later on their own
	SetAside    int       `json:"setAside"`   // sum of Aside
	Aside       ixDlAside `json:"aside"`
	ToDownload  int       `json:"toDownload"` // in the queue, plus found missing and not queued yet
	// What the verdict went by.
	EngineRunning   bool       `json:"engineRunning"`
	QueuePaused     bool       `json:"queuePaused"`
	ReadOnly        bool       `json:"readOnly"`
	QuietMin        int        `json:"quietMin"`        // minutes since bytes arrived or a file finished
	SpeedWaits      int        `json:"speedWaits"`      // Telegram's speed-limit waits, last 10 minutes
	TelegramWaiting int        `json:"telegramWaiting"` // requests currently waiting for Telegram
	OtherWaits      int        `json:"otherWaits"`      // its other waits, last 10 minutes
	SafetyNet       string     `json:"safetyNet"`       // stalled only: "soon" (set-aside files are let go then), "on" (they were), or ""
	SafetyNetMin    int        `json:"safetyNetMin"`    // soon: minutes until; on: minutes it lasts
	Rescan          ixDlRescan `json:"rescan"`
	heldForever     bool       // likely copies wait for a person
}

var ixDlBoxes sync.Map // *App -> *ixCached[ixDlState]

// ixDlState is what downloads are doing (cached for ixDlStateTTL).
func (a *App) ixDlState(ctx context.Context) ixDlState {
	v, _ := ixDlBoxes.LoadOrStore(a, &ixCached[ixDlState]{})
	return v.(*ixCached[ixDlState]).get(ixDlStateTTL, func() (ixDlState, bool) { return a.ixDlMeasure(ctx, time.Now()) })
}

func (a *App) ixDlMeasure(ctx context.Context, now time.Time) (ixDlState, bool) {
	var st ixDlState
	_, _, st.EngineRunning = ixEngineRun(a)
	st.QueuePaused = a.ixQueuePaused(ctx)
	st.ReadOnly = a.ixIsGone()
	s := a.ixLoadSettings(ctx)
	st.heldForever = s.HoldLikely && s.HoldForever
	if err := a.ixDlCount(ctx, now, &st); err != nil {
		return st, false
	}
	st.QuietMin = int(now.Sub(a.ixDlLastMove(ctx, now)) / time.Minute)
	p := ixDlPace(a)
	st.SpeedWaits, st.OtherWaits = p.Premium10m, p.Floods10m
	for _, dc := range p.DCs {
		st.TelegramWaiting += dc.GateWaiting
	}
	a.ixDlRescanNow(ctx, now, &st.Rescan)
	st.ToDownload = st.Ready + st.Downloading + st.RetryLater + st.SetAside + st.Rescan.NotQueued
	// Only worth a look when it can change the verdict or the advice.
	if st.Ready == 0 && st.Downloading <= ixDlFillingMax && st.Rescan.On && st.Rescan.Left > 0 {
		st.Rescan.QueueFull = a.ixBigFilesNow(ctx).CapPaused
	}
	ixDlDecide(&st)
	if st.Verdict == ixDlStalled {
		a.ixDlSafetyNet(ctx, now, &st)
	}
	st.Advice, st.Act = ixDlAdvice(st)
	return st, ctx.Err() == nil
}

// ixDlCount sorts the queue: one pass over the pending and downloading
// items, each counted once.
func (a *App) ixDlCount(ctx context.Context, now time.Time, st *ixDlState) error {
	lane := engine.BigFileLane()
	var run, bigRun, small, big int
	err := a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(SUM(st='run'),0), COALESCE(SUM(st='run' AND big),0), COALESCE(SUM(st='ready'),0), COALESCE(SUM(st='big'),0),
		COALESCE(SUM(st='retry'),0), COALESCE(SUM(st='held'),0), COALESCE(SUM(st='deferred'),0), COALESCE(SUM(st='lowq'),0), COALESCE(SUM(st='user'),0) FROM (
		SELECT CASE WHEN w.status='processing' THEN 'run'
			WHEN w.paused<>0 AND EXISTS (SELECT 1 FROM ix_held h WHERE h.work_id=w.id AND h.released_at=0 AND h.reason<>'') THEN 'held'
			WHEN w.paused<>0 AND EXISTS (SELECT 1 FROM ix_deferred x WHERE x.work_id=w.id AND x.released_at=0) THEN 'deferred'
			WHEN w.paused<>0 AND EXISTS (SELECT 1 FROM ix_lowq q WHERE q.work_id=w.id AND q.state='saved') THEN 'lowq'
			WHEN w.paused<>0 THEN 'user'
			WHEN w.retry_at>? THEN 'retry'
			WHEN ?>0 AND w.ttl_seconds=0 AND w.file_size>=? THEN 'big'
			ELSE 'ready' END AS st,
			(w.ttl_seconds=0 AND w.file_size>=?) AS big
		FROM tgdl_work w WHERE w.status IN ('pending','processing'))`, now.UnixMilli(), lane, engine.BigFileSize, engine.BigFileSize).
		Scan(&run, &bigRun, &small, &big, &st.RetryLater, &st.Aside.Copies, &st.Aside.Deadline, &st.Aside.LowerQuality, &st.Aside.ByYou)
	if err != nil {
		return err
	}
	// The lane lets lane big files download at once, twice that when
	// nothing smaller can be claimed: those it would take now are ready.
	free := lane - bigRun
	if small == 0 {
		free = 2*lane - bigRun
	}
	take := min(big, max(0, free))
	st.Downloading, st.Ready, st.Aside.BigLane = run, small+take, big-take
	st.SetAside = st.Aside.Copies + st.Aside.Deadline + st.Aside.LowerQuality + st.Aside.BigLane + st.Aside.ByYou
	return nil
}

// ixDlLastMove is when downloads last moved, as ixLastProgress judges it;
// the finished-file lookup is left out while bytes keep arriving.
func (a *App) ixDlLastMove(ctx context.Context, now time.Time) time.Time {
	box := a.ixProgressBox()
	n := ixBytesReceived(a)
	box.mu.Lock()
	if !box.seen || n != box.bytes {
		box.seen, box.bytes, box.at = true, n, now
	}
	at := box.at
	box.mu.Unlock()
	if now.Sub(at) < time.Minute {
		return at
	}
	return a.ixLastProgress(ctx)
}

func (a *App) ixDlRescanNow(ctx context.Context, now time.Time, r *ixDlRescan) {
	backfill := false
	a.historyMu.Lock()
	for _, j := range a.historyJobs {
		if j.State == "running" && !j.MessagesOnly && !j.Cancelled {
			backfill = true
		}
	}
	a.historyMu.Unlock()
	q, err := a.ixLoadQueue(ctx)
	if err != nil {
		r.Adding = backfill
		return
	}
	r.On = q.Running
	soon := now.Add(ixUnqueuedSoon).UnixMilli()
	due := false
	var cur *ixQueueItem
	for i := range q.Items {
		it := &q.Items[i]
		r.Total++
		switch it.State {
		case "done":
			r.Done++
		case "failed":
			r.Failed++
		case "running", "waiting":
			r.Left++
			if it.MessagesAt == 0 {
				r.Unread++
			}
			if it.Phase == ixPhaseFiles {
				n := it.Missing
				if it.State == "running" {
					n -= it.Downloaded
				}
				r.NotQueued += max(0, n)
			}
			if it.State == "running" {
				due = true
				if cur == nil || it.Phase == ixPhaseFiles && cur.Phase != ixPhaseFiles {
					cur = it
				}
			} else if it.NextAt <= soon {
				due = true
			}
		}
	}
	if cur != nil {
		r.Group, r.Phase, r.groupID = ixCleanName(cur.Name, cur.GroupID), cur.Phase, cur.GroupID
	}
	switch {
	case backfill || r.On && cur != nil:
		r.Adding = true
	case r.On && r.Left > 0 && a.ixHold(ctx) != "":
		r.Held = true // no group starts now; it goes on by itself
	case r.On && due:
		r.Adding = true
	}
}

// ixDlDecide sets the verdict.
func ixDlDecide(st *ixDlState) {
	a := st.Aside
	goes := st.Ready + st.Downloading + a.Deadline + a.LowerQuality + a.BigLane
	if !st.heldForever {
		goes += a.Copies
	}
	moving := time.Duration(st.QuietMin)*time.Minute < ixDlStallAfter
	r := st.Rescan
	switch {
	case st.ReadOnly:
		st.Verdict = ixDlReadOnly
	case st.QueuePaused || !st.EngineRunning:
		st.Verdict = ixDlPaused
	case st.TelegramWaiting > 0 && goes > 0:
		st.Verdict = ixDlSpeedLimited
	case goes > 0 && !moving:
		st.Verdict = ixDlStalled
	case st.SpeedWaits+st.OtherWaits > 0 && goes > 0:
		st.Verdict = ixDlSpeedLimited
	case st.Ready == 0 && st.Downloading <= ixDlFillingMax && (r.Adding || r.Held):
		st.Verdict = ixDlFilling
	case goes == 0 && st.RetryLater == 0 && r.Left == 0:
		st.Verdict = ixDlDone
	default:
		st.Verdict = ixDlDownloading
	}
}

// ixDlAdvice is the one sentence: whether to do anything. Plain words; it
// never names a chat. Scanning more groups is never suggested while the
// rescan has groups left.
func ixDlAdvice(st ixDlState) (string, bool) {
	n := func(v int) string { return strconv.Itoa(v) }
	plural := func(v int, one, many string) string {
		if v == 1 {
			return one
		}
		return many
	}
	r := st.Rescan
	switch st.Verdict {
	case ixDlReadOnly:
		return "Nothing to do — the archive is read-only now, so nothing more can download.", false
	case ixDlPaused:
		if st.QueuePaused {
			return "Paused — the download queue is paused. Resume it on the dashboard to go on.", true
		}
		return "Stopped — the download engine is not running. Start the monitor on the dashboard.", true
	case ixDlStalled:
		s := "Stalled — nothing has arrived for " + n(st.QuietMin) + " minutes. Send a diagnostics report (Archive → Settings)."
		switch st.SafetyNet {
		case "soon":
			s += " If nothing moves, the files set aside for private chats and smaller files first are let go in about " + n(st.SafetyNetMin) + " " + plural(st.SafetyNetMin, "minute", "minutes") + "."
		case "on":
			s += " The files set aside were already let go (for about " + n(st.SafetyNetMin) + " more " + plural(st.SafetyNetMin, "minute", "minutes") + "); that has not helped."
		}
		return s, true
	case ixDlSpeedLimited:
		return "Nothing to do — Telegram's speed limit sets the pace; more queued files would not help.", false
	case ixDlFilling:
		switch {
		case r.QueueFull && st.Aside.ByYou > 0:
			return "The rescan is waiting for room in the download queue, and " + n(st.Aside.ByYou) + " " + plural(st.Aside.ByYou, "file you paused fills", "files you paused fill") + " it. Resume or remove them on the queue page.", true
		case r.Held:
			return "Nothing to do — the rescan is waiting (quiet hours or a safety pause) and goes on by itself.", false
		}
		return "Nothing to do — the rescan is adding files group by group.", false
	case ixDlDone:
		s := "Nothing to do — everything queued is downloaded and the rescan is finished."
		if st.Aside.ByYou > 0 {
			s += " " + n(st.Aside.ByYou) + " " + plural(st.Aside.ByYou, "file you paused still waits", "files you paused still wait") + "."
		}
		if st.heldForever && st.Aside.Copies > 0 {
			s += " " + n(st.Aside.Copies) + " likely " + plural(st.Aside.Copies, "copy is", "copies are") + " held until you decide."
		}
		if st.Aside.ByYou == 0 && !(st.heldForever && st.Aside.Copies > 0) {
			s += " To fetch more, add groups on the Rescan tab."
		}
		return s, false
	}
	// Downloading. The queue running dry while the rescan is switched off
	// with groups left: that is the one time to start it.
	if st.Ready == 0 && st.Downloading <= ixDlFillingMax && r.Left > 0 && !r.On {
		return "The rescan is paused with " + n(r.Left) + " " + plural(r.Left, "group", "groups") + " left. Start it on the Rescan tab to queue more files.", true
	}
	if st.Ready == 0 && st.Downloading == 0 {
		if r.On && r.Left > 0 {
			return "Nothing to do — the rescan tries its remaining groups again later, by itself.", false
		}
		if st.RetryLater > 0 {
			return "Nothing to do — " + n(st.RetryLater) + " " + plural(st.RetryLater, "file waits", "files wait") + " to be tried again later.", false
		}
		return "Nothing to do — the files set aside go next, by themselves.", false
	}
	return "Nothing to do — files are downloading.", false
}

// ixDlSafetyNet: whether the deadline order's safety net (ixDeadlineStep)
// will let the set-aside files go, or already has.
func (a *App) ixDlSafetyNet(ctx context.Context, now time.Time, st *ixDlState) {
	ceilMin := func(d time.Duration) int { return int((d + time.Minute - 1) / time.Minute) }
	var v string
	if a.db.Reader.QueryRowContext(ctx, `SELECT value FROM kv WHERE key='ix_deadline_suspend'`).Scan(&v) == nil {
		if until, _ := strconv.ParseInt(v, 10, 64); until > now.UnixMilli() {
			st.SafetyNet, st.SafetyNetMin = "on", ceilMin(time.UnixMilli(until).Sub(now))
			return
		}
	}
	if st.Aside.Deadline == 0 {
		return
	}
	wait := ixDlNetAfter - time.Duration(st.QuietMin)*time.Minute
	var firstSet int64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(MIN(deferred_at),0) FROM ix_deferred WHERE released_at=0`).Scan(&firstSet)
	if firstSet > 0 {
		wait = max(wait, ixDlNetAfter-now.Sub(time.UnixMilli(firstSet)))
	}
	st.SafetyNet, st.SafetyNetMin = "soon", ceilMin(max(wait, 0)+ixDlNetTick)
}

// ixDlDiagLine is the diagnostics report's line: numbers only, a group
// named by its placeholder (group maps an ID to G<n>).
func (st ixDlState) diagLine(group func(string) string) string {
	a, r := st.Aside, st.Rescan
	cur := "none"
	if r.groupID != "" {
		cur = group(r.groupID) + " (" + map[bool]string{true: r.Phase, false: "pass"}[r.Phase != ""] + ")"
	}
	net := st.SafetyNet
	if net == "" {
		net = "-"
	} else {
		net += fmt.Sprintf(" %d min", st.SafetyNetMin)
	}
	return fmt.Sprintf("download state: %s · ready %d · downloading %d · retry later %d · set aside %d (copies %d, private/smaller first %d, lower quality %d, big-file lane %d, paused by a person %d) · quiet %d min · Telegram waits in 10 min: %d speed limit, %d other · safety net %s · rescan on %v, adding %v, held %v, queue full %v · groups %d done, %d failed, %d left (%d not read) of %d · reading %s · to download %d (%d not queued yet)",
		st.Verdict, st.Ready, st.Downloading, st.RetryLater, st.SetAside, a.Copies, a.Deadline, a.LowerQuality, a.BigLane, a.ByYou,
		st.QuietMin, st.SpeedWaits, st.OtherWaits, net, r.On, r.Adding, r.Held, r.QueueFull, r.Done, r.Failed, r.Left, r.Unread, r.Total, cur, st.ToDownload, r.NotQueued)
}
