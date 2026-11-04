package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/botnick/telegram-media-downloader/core-service/internal/ws"
)

// The rescan queue walks whole groups with the backfill engine. Each group
// gets two passes, in two lanes that run side by side, one group each: the
// messages pass reads and records every message and queues no downloads, so
// it is never held back by the download queue; the files pass walks the
// group again once its messages are saved and queues the files the library
// does not have yet (groups with nothing missing skip it). Groups marked as
// priority go first in each lane. The queue runs inside the server, so it
// continues with no browser open, and survives restarts (its state is in
// the kv table).

const (
	ixQueueKey  = "ix_queue"
	ixQueueTick = 10 * time.Second
	ixRetryBase = time.Minute
	ixRetryMax  = 15 * time.Minute
	ixLostJob   = 3 * time.Minute
)

// Passes of a queued group.
const (
	ixPhaseMessages = "messages" // record every message, download nothing
	ixPhaseFiles    = "files"    // walk again and queue missing files
)

type ixQueueItem struct {
	GroupID    string `json:"groupId"`
	Name       string `json:"name"`
	State      string `json:"state"` // waiting, running, done, failed
	Attempts   int    `json:"attempts"`
	NextAt     int64  `json:"nextAt,omitempty"`
	JobID      string `json:"jobId,omitempty"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
	Processed  int    `json:"processed"`
	Downloaded int    `json:"downloaded"`
	Error      string `json:"error,omitempty"`
	AddedAt    int64  `json:"addedAt,omitempty"`
	// Phase is the pass to run next, or running ("" in queues saved by
	// earlier versions: one pass that also queues downloads).
	Phase      string `json:"phase,omitempty"`
	MessagesAt int64  `json:"messagesAt,omitempty"` // when the messages pass finished
	Messages   int    `json:"messages,omitempty"`   // messages read by that pass
	Missing    int    `json:"missing,omitempty"`    // files still missing after it
	// Priority groups get both passes before any other group starts.
	Priority bool `json:"priority,omitempty"`
	// A pass that stopped part-way carries on below ResumeFrom (the oldest
	// message it reached); Carried counts what earlier attempts read, and
	// PassStartedAt is when the pass first started.
	ResumeFrom    int   `json:"resumeFrom,omitempty"`
	Carried       int   `json:"carried,omitempty"`
	PassStartedAt int64 `json:"passStartedAt,omitempty"`
}

type ixQueueState struct {
	Running     bool          `json:"running"`
	Items       []ixQueueItem `json:"items"`
	Notice      string        `json:"notice,omitempty"`
	MaxAttempts int           `json:"maxAttempts"` // 0 = keep trying
	UpdatedAt   int64         `json:"updatedAt"`
}

// ixQueueDeps are the backfill operations the queue uses; tests replace them.
type ixQueueDeps struct {
	start   func(ctx context.Context, groupID string, messagesOnly bool, resumeFrom int) (jobID string, err error)
	cursor  func(ctx context.Context, jobID string) int // oldest message a backfill reached (0: unknown)
	job     func(ctx context.Context, jobID string) (historyJob, bool)
	missing func(ctx context.Context, groupID string) (int, error) // files the library still lacks
	finish  func(ctx context.Context, item ixQueueItem, job historyJob) error
	hold    func(ctx context.Context) string // non-empty: do not start new groups (safety pause, quiet hours)
	now     func() time.Time
	// triage ranks groups when the account is to be deleted: lower goes
	// first (private chats before public ones). nil or empty: queue order.
	triage func(ctx context.Context) map[string]int
}

func (a *App) ixDeps() ixQueueDeps {
	return ixQueueDeps{
		start:   a.ixStartHistory,
		job:     a.ixHistoryJob,
		cursor:  a.ixHistoryCursor,
		missing: a.ixMissingFiles,
		finish:  a.ixFinishWalk,
		hold:    a.ixHold,
		now:     time.Now,
		triage:  a.ixTriage,
	}
}

func (a *App) ixStart() {
	telegram.SetPacing(a.ixLoadSettings(a.ctx).DownloadPacing)
	a.ambientWG.Add(1)
	go func() { defer a.ambientWG.Done(); a.ixRunQueue(a.ctx) }()
	a.ambientWG.Add(1)
	go func() { defer a.ambientWG.Done(); a.ixRunExports(a.ctx) }()
	a.ambientWG.Add(1)
	go func() { defer a.ambientWG.Done(); a.ixRunSafety(a.ctx) }()
	a.ambientWG.Add(1)
	go func() { defer a.ambientWG.Done(); a.ixRunVdup(a.ctx) }()
	a.ambientWG.Add(1)
	go func() { defer a.ambientWG.Done(); a.ixRunRetry(a.ctx) }()
	a.ambientWG.Add(1)
	go func() { defer a.ambientWG.Done(); a.ixRunAlerts(a.ctx) }()
	for _, run := range []func(context.Context){a.ixRunFreeze, a.ixRunMedia, a.ixRunFixity, a.ixRunBrowse, a.ixRunHeld, a.ixRunML, a.ixRunSTT, a.ixRunDeadline, a.ixRunBackups} {
		a.ambientWG.Add(1)
		go func() { defer a.ambientWG.Done(); run(a.ctx) }()
	}
}

func (a *App) ixWake() {
	select {
	case a.ix().wake <- struct{}{}:
	default:
	}
}

// ixBackgroundQueue lets tests drive the queue step by step.
var ixBackgroundQueue atomic.Bool

func init() { ixBackgroundQueue.Store(true) }

func (a *App) ixRunQueue(ctx context.Context) {
	ticker := time.NewTicker(ixQueueTick)
	defer ticker.Stop()
	deps := a.ixDeps()
	for ctx.Err() == nil {
		if ixBackgroundQueue.Load() {
			if err := a.ixQueueStep(ctx, deps); err != nil && ctx.Err() == nil {
				a.ixLog("rescan queue: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-a.ix().wake:
		}
	}
}

func (a *App) ixLoadQueue(ctx context.Context) (ixQueueState, error) {
	var raw string
	err := a.db.Reader.QueryRowContext(ctx, `SELECT value FROM kv WHERE key=?`, ixQueueKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ixQueueState{Items: []ixQueueItem{}}, nil
	}
	if err != nil {
		return ixQueueState{}, err
	}
	var st ixQueueState
	if err = json.Unmarshal([]byte(raw), &st); err != nil {
		return ixQueueState{}, err
	}
	if st.Items == nil {
		st.Items = []ixQueueItem{}
	}
	for i := range st.Items {
		// Groups queued by an earlier version start with the messages pass.
		if st.Items[i].State == "waiting" && st.Items[i].Phase == "" {
			st.Items[i].Phase = ixPhaseMessages
		}
	}
	return st, nil
}

func (a *App) ixSaveQueue(ctx context.Context, st ixQueueState, now time.Time) error {
	st.UpdatedAt = now.UnixMilli()
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = a.db.Writer.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, ixQueueKey, string(data), st.UpdatedAt)
	if err == nil && a.hub != nil {
		a.hub.Broadcast(ws.Event{Type: "ix_queue", Flat: true, Payload: map[string]any{"updatedAt": st.UpdatedAt}, Roles: []string{"admin"}})
	}
	return err
}

func ixBackoff(attempts int) time.Duration {
	d := ixRetryBase
	for i := 1; i < attempts && d < ixRetryMax; i++ {
		d *= 2
	}
	return min(d, ixRetryMax)
}

// ixRetryLater puts a failed group at the back of the queue to try again
// after a growing pause, or gives up when a retry limit is set and reached.
func ixRetryLater(st *ixQueueState, i int, now time.Time, reason string) {
	item := st.Items[i]
	item.Attempts++
	item.Error = reason
	item.JobID = ""
	if st.MaxAttempts > 0 && item.Attempts >= st.MaxAttempts {
		item.State = "failed"
		item.FinishedAt = now.UnixMilli()
		st.Items[i] = item
		return
	}
	item.State = "waiting"
	item.NextAt = now.Add(ixBackoff(item.Attempts)).UnixMilli()
	st.Items = append(append(st.Items[:i:i], st.Items[i+1:]...), item)
}

// ixQueueStep advances the queue. It is called every few seconds and after
// every change made through the API. Two lanes run side by side, one group
// each: messages passes, and files passes (a group's files pass can start as
// soon as its messages are saved), so saving messages never waits for
// downloads and downloads never wait for the other groups' messages.
func (a *App) ixQueueStep(ctx context.Context, deps ixQueueDeps) error {
	rt := a.ix()
	rt.queueMu.Lock()
	defer rt.queueMu.Unlock()
	st, err := a.ixLoadQueue(ctx)
	if err != nil || !st.Running {
		return err
	}
	now := deps.now()
	before, _ := json.Marshal(st)
	save := func() error {
		after, _ := json.Marshal(st)
		if bytes.Equal(before, after) {
			return nil
		}
		return a.ixSaveQueue(ctx, st, now)
	}
	var running []string
	for _, item := range st.Items {
		if item.State == "running" {
			running = append(running, item.GroupID)
		}
	}
	for _, groupID := range running {
		if err := a.ixQueueTrack(ctx, deps, &st, groupID, now); err != nil {
			return err
		}
	}
	// Lanes in use, and the next group for each free lane: prioritised groups
	// first, queue order otherwise. A group waiting to retry holds no one up.
	busy := map[bool]bool{}
	for _, item := range st.Items {
		if item.State == "running" {
			busy[item.Phase == ixPhaseFiles] = true
		}
	}
	next := map[bool]int{false: -1, true: -1}
	var ranks map[string]int
	if deps.triage != nil {
		ranks = deps.triage(ctx)
	}
	better := func(i, j int) bool {
		if st.Items[i].Priority != st.Items[j].Priority {
			return st.Items[i].Priority
		}
		return ranks[st.Items[i].GroupID] < ranks[st.Items[j].GroupID]
	}
	remaining := 0
	for i, item := range st.Items {
		if item.State != "waiting" {
			continue
		}
		remaining++
		if item.NextAt > now.UnixMilli() {
			continue
		}
		lane := item.Phase == ixPhaseFiles
		if j := next[lane]; j < 0 || better(i, j) {
			next[lane] = i
		}
	}
	if remaining == 0 {
		if len(busy) == 0 {
			st.Running = false
			st.Notice = "Finished: every group in the queue has been processed."
		}
		return save()
	}
	var startable []int
	for _, lane := range []bool{false, true} { // messages lane first
		if !busy[lane] && next[lane] >= 0 {
			startable = append(startable, next[lane])
		}
	}
	if len(startable) == 0 {
		if len(busy) == 0 {
			st.Notice = "Waiting to retry groups that failed."
		}
		return save()
	}
	if deps.hold != nil {
		if why := deps.hold(ctx); why != "" {
			st.Notice = why
			return save()
		}
	}
	st.Notice = ""
	// Indexes move when a failed start goes to the back: start by group.
	var groups []string
	for _, i := range startable {
		groups = append(groups, st.Items[i].GroupID)
	}
	for _, groupID := range groups {
		i := ixQueueIndex(&st, groupID)
		if i < 0 {
			continue
		}
		item := &st.Items[i]
		if item.Phase == "" {
			item.Phase = ixPhaseMessages
		}
		if item.ResumeFrom == 0 || item.PassStartedAt == 0 {
			item.ResumeFrom, item.Carried, item.PassStartedAt = 0, 0, now.UnixMilli()
		}
		jobID, err := deps.start(ctx, item.GroupID, item.Phase == ixPhaseMessages, item.ResumeFrom)
		if err != nil {
			var permanent ixPermanentError
			if errors.As(err, &permanent) {
				item.State, item.Error, item.FinishedAt = "failed", err.Error(), now.UnixMilli()
				continue
			}
			ixRetryLater(&st, i, now, err.Error())
			continue
		}
		item.State, item.JobID, item.StartedAt, item.Error = "running", jobID, now.UnixMilli(), ""
		item.Processed, item.Downloaded, item.NextAt = 0, 0, 0
	}
	return save()
}

// ixQueueTrack follows one running group's backfill: progress, finishing a
// pass (messages → files when files are missing), or a retry.
func (a *App) ixQueueTrack(ctx context.Context, deps ixQueueDeps, st *ixQueueState, groupID string, now time.Time) error {
	i := ixQueueIndex(st, groupID)
	if i < 0 {
		return nil
	}
	item := &st.Items[i]
	job, ok := deps.job(ctx, item.JobID)
	// Where a stopped backfill got to, so the retry carries on from there.
	keepPlace := func(processed int) {
		if deps.cursor == nil {
			return
		}
		if c := deps.cursor(ctx, item.JobID); c > 0 && (item.ResumeFrom == 0 || c < item.ResumeFrom) {
			item.ResumeFrom = c
			item.Carried += processed
		}
	}
	if !ok {
		if now.Sub(time.UnixMilli(item.StartedAt)) > ixLostJob {
			keepPlace(item.Processed)
			ixRetryLater(st, i, now, "the backfill disappeared (the server may have restarted)")
		}
		return nil
	}
	item.Processed, item.Downloaded = job.Processed, job.Downloaded
	switch job.State {
	case "running":
		return nil
	case "done":
		if job.Cancelled {
			keepPlace(job.Processed)
			ixRetryLater(st, i, now, "the backfill was cancelled")
			return nil
		}
		if deps.finish != nil {
			if err := deps.finish(ctx, *item, job); err != nil {
				ixRetryLater(st, i, now, "could not finish: "+err.Error())
				return nil
			}
		}
		read := item.Carried + job.Processed
		item.ResumeFrom, item.Carried, item.PassStartedAt = 0, 0, 0
		if item.Phase == ixPhaseMessages {
			item.MessagesAt, item.Messages = now.UnixMilli(), read
			missing := 0
			if deps.missing != nil {
				n, err := deps.missing(ctx, item.GroupID)
				if err != nil {
					ixRetryLater(st, i, now, "could not count missing files: "+err.Error())
					return nil
				}
				missing = n
			}
			item.Missing, item.JobID, item.Error = missing, "", ""
			if missing > 0 {
				item.Phase, item.State, item.Attempts, item.NextAt = ixPhaseFiles, "waiting", 0, 0
				item.Processed, item.Downloaded = 0, 0
				return nil
			}
		}
		item.State, item.FinishedAt, item.Error, item.JobID = "done", now.UnixMilli(), "", ""
	default:
		reason := "the backfill stopped (" + job.State + ")"
		if job.Error != nil && *job.Error != "" {
			reason = *job.Error
		}
		keepPlace(job.Processed)
		ixRetryLater(st, i, now, reason)
	}
	return nil
}

// ixCall runs one of the server's own HTTP handlers in-process, exactly as
// the dashboard would call it, so the queue goes through the same checks.
func ixCall(ctx context.Context, handler http.HandlerFunc, method, path string, body any, values map[string]string) (int, map[string]any) {
	var reader *bytes.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, reader).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	for k, v := range values {
		r.SetPathValue(k, v)
	}
	w := httptest.NewRecorder()
	handler(w, r)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// ixStartHistory starts a whole-history backfill. A start date of 1 (the
// beginning of time) makes the backfill engine walk every message from the
// newest down, using Telegram's normal paging; files already in the library
// are recognised and not downloaded again. messagesOnly records the
// messages without queueing any download.
func (a *App) ixStartHistory(ctx context.Context, groupID string, messagesOnly bool, resumeFrom int) (string, error) {
	if !messagesOnly && resumeFrom == 0 {
		a.ixRequeueGroupFailed(ctx, groupID)
	}
	body := map[string]any{"groupId": groupID, "limit": 0, "mode": "pull-older", "minDate": 1, "messagesOnly": messagesOnly}
	if resumeFrom > 0 {
		body["resumeFrom"] = resumeFrom
	}
	code, out := ixCall(ctx, a.handleHistoryStart, http.MethodPost, "/api/history", body, nil)
	if id := toString(out["jobId"]); id != "" && (code == http.StatusOK || toString(out["code"]) == "ALREADY_RUNNING") {
		return id, nil
	}
	msg := toString(out["error"])
	if msg == "" {
		msg = http.StatusText(code)
	}
	if toString(out["code"]) == "CHAT_UNREACHABLE" {
		// Left, banned or deleted: retrying cannot help.
		return "", ixPermanentError{msg}
	}
	return "", errors.New(msg)
}

type ixPermanentError struct{ msg string }

func (e ixPermanentError) Error() string { return e.msg }

// ixHistoryJob finds a backfill by ID, live or finished.
func (a *App) ixHistoryJob(ctx context.Context, id string) (historyJob, bool) {
	if id == "" {
		return historyJob{}, false
	}
	a.historyMu.Lock()
	if j := a.historyJobs[id]; j != nil {
		found := *j
		a.historyMu.Unlock()
		return found, true
	}
	a.historyMu.Unlock()
	var payload string
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT payload FROM tgdl_history_jobs WHERE id=?`, id).Scan(&payload); err == nil {
		var record historyRecord
		if json.Unmarshal([]byte(payload), &record) == nil && record.Job.ID == id {
			return record.Job, true
		}
	}
	var raw string
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT value FROM kv WHERE key='history_jobs'`).Scan(&raw); err == nil {
		var list []historyJob
		if json.Unmarshal([]byte(raw), &list) == nil {
			for _, j := range list {
				if j.ID == id {
					return j, true
				}
			}
		}
	}
	return historyJob{}, false
}

// ixFinishWalk records a completed whole-group walk. Every message that
// still exists was read during the walk, so archived messages not seen since
// it started were deleted on Telegram; they are flagged (and kept).
func (a *App) ixFinishWalk(ctx context.Context, item ixQueueItem, job historyJob) error {
	group := job.GroupID
	if group == "" {
		group = item.GroupID
	}
	now := time.Now().UnixMilli()
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var marked int64
	started := job.StartedAt
	if item.PassStartedAt > 0 && item.PassStartedAt < started {
		started = item.PassStartedAt // the pass was carried on over several backfills
	}
	if job.Limit == nil && job.MinDate > 0 && job.MinDate <= 1 && started > 0 {
		res, err := tx.ExecContext(ctx, `UPDATE ix_messages SET deleted_at=? WHERE group_id=? AND deleted_at=0 AND seen_at<?`, now, group, started)
		if err != nil {
			return err
		}
		marked, _ = res.RowsAffected()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ix_walks(job_id,group_id,started_at,finished_at,processed,marked_deleted,state) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(job_id) DO UPDATE SET finished_at=excluded.finished_at, processed=excluded.processed, marked_deleted=excluded.marked_deleted, state=excluded.state`,
		job.ID, group, started, now, job.Processed, marked, "done"); err != nil {
		return err
	}
	return tx.Commit()
}

// ixQueueEdit applies a change made from the dashboard.
func (a *App) ixQueueEdit(ctx context.Context, edit func(st *ixQueueState) error) (ixQueueState, error) {
	rt := a.ix()
	rt.queueMu.Lock()
	st, err := a.ixLoadQueue(ctx)
	if err == nil {
		err = edit(&st)
	}
	if err == nil {
		err = a.ixSaveQueue(ctx, st, time.Now())
	}
	rt.queueMu.Unlock()
	if err == nil {
		a.ixWake()
	}
	return st, err
}

func ixQueueIndex(st *ixQueueState, groupID string) int {
	for i, item := range st.Items {
		if item.GroupID == groupID {
			return i
		}
	}
	return -1
}

func ixCleanName(name, id string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return id
	}
	return name
}

// ixHistoryCursor is the oldest message a backfill has reached: where a
// walk continues if it has to be started again (0: not known).
func (a *App) ixHistoryCursor(ctx context.Context, id string) int {
	if id == "" {
		return 0
	}
	a.historyMu.Lock()
	if j := a.historyJobs[id]; j != nil {
		c := j.cursor
		a.historyMu.Unlock()
		return c
	}
	a.historyMu.Unlock()
	var payload string
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT payload FROM tgdl_history_jobs WHERE id=?`, id).Scan(&payload); err == nil {
		var record historyRecord
		if json.Unmarshal([]byte(payload), &record) == nil && record.Job.ID == id {
			return record.Cursor
		}
	}
	return 0
}
