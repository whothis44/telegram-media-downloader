package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/ws"
)

// Read-only archive mode, for after the account is gone. When Telegram says
// the account is deleted or deactivated, or that the session is no longer
// valid (USER_DEACTIVATED, AUTH_KEY_UNREGISTERED, SESSION_REVOKED…; see
// ixClassify), the app stops all
// Telegram work cleanly instead of retrying a dead session: the download
// engine is stopped and is not started again (the watchdog, the restart
// after an error, and every feature that opens a Telegram connection go
// through startTelegramEngine), rescans do not start, and freeze checks
// stop. Everything that works from the saved archive keeps going: the
// browser, search, recognition, faces, exports and backups.
//
// One error is not enough: gotd connections to another data centre (file
// downloads) can answer AUTH_KEY_UNREGISTERED for a while after a reconnect
// or an authorization export, while the account is fine. So before the mode
// starts, a cheap call that needs a logged-in session, on the account's main
// connection (users.getUsers for the account itself; see
// telegram.Account.SessionCheck), must fail the same way; while the engine
// is running and that call works, the errors are ignored. (The freeze
// check, help.getAppConfig, proves nothing here: Telegram answers it on a
// connection that is not logged in.) A frozen account is not gone: it may
// still make that call, and a FROZEN_* answer comes only to a logged-in
// session, so it counts as working. When no such call can be
// made (the engine is down, as when the main connection itself failed), the
// error must have repeated: on at least ixGoneItems different items (queue
// items, backfills, engine runs) over at least ixGoneSpan.
//
// The state is kept in kv, so it holds across restarts. "Try Telegram
// again" (Overview) leaves the mode, for an appeal that was accepted; fresh
// errors put it back.

const ixGoneKey = "ix_gone"

// errIxReadOnly is what any attempt to connect to Telegram gets meanwhile.
var errIxReadOnly = errors.New("Telegram account gone: the archive is read-only now (Archive → Overview → Try Telegram again)")

// ixGoneStartDelay is the wait before the first check after a start.
var ixGoneStartDelay = time.Minute

var (
	// Without a live check, read-only mode needs the error on this many
	// different items, the first and the last this far apart.
	ixGoneItems = 3
	ixGoneSpan  = 10 * time.Minute
	// ixGoneCheckEvery spaces the live checks on the main connection out.
	ixGoneCheckEvery = 5 * time.Minute
	// ixGoneEngineMerge: engine errors this close together are one failure
	// (a failed start can be reported twice).
	ixGoneEngineMerge = time.Second
)

// ixGoneProbe asks Telegram on the account's main connection whether the
// session still works, with a call only a logged-in session can make. nil:
// it works; an error ixDeactivatedRe matches: it is gone; errIxNoProbe: no
// call could be made, or the answer says neither.
var ixGoneProbe = func(a *App, ctx context.Context) error {
	if a.monitor == nil || a.monitor.RequireRunning() != nil {
		return errIxNoProbe
	}
	session, err := a.monitor.OpenHistory(ctx, "")
	if err == nil {
		err = session.SessionCheck()
		session.Close()
	}
	return ixGoneProbeAnswer(err)
}

// ixGoneProbeAnswer reads the answer to the live check.
func ixGoneProbeAnswer(err error) error {
	switch {
	case err == nil:
		return nil
	case ixDeactivatedRe.MatchString(err.Error()):
		return err
	case ixFrozenRe.MatchString(err.Error()):
		return nil // frozen, but logged in: the account is there
	}
	return fmt.Errorf("%w: %v", errIxNoProbe, err)
}

var errIxNoProbe = errors.New("no live check of the Telegram session could be made")

// ixGoneSign is one error that says the account may be gone.
type ixGoneSign struct {
	key  string // the item it was seen on
	at   int64  // Unix ms
	code string
}

type ixGoneInfo struct {
	Gone      bool   `json:"gone"`
	Since     int64  `json:"since,omitempty"`
	Reason    string `json:"reason,omitempty"` // "telegram" (an error said so) or "date" (the deletion date passed)
	Code      string `json:"code,omitempty"`   // the Telegram error seen
	ClearedAt int64  `json:"clearedAt,omitempty"`
}

var ixGoneBoxes sync.Map // *App -> *ixGoneBox

type ixGoneBox struct {
	mu     sync.Mutex
	st     ixGoneInfo
	loaded bool
	// Engine errors that said the account may be gone (newest last).
	engine []ixGoneSign
	// The last live check on the main connection: when, and when it last
	// showed the session working (errors before that do not count).
	checkedAt, aliveAt int64
	checkAlive         bool
	wake               chan struct{}
}

func (a *App) ixGoneBox() *ixGoneBox {
	v, _ := ixGoneBoxes.LoadOrStore(a, &ixGoneBox{wake: make(chan struct{}, 1)})
	box := v.(*ixGoneBox)
	box.mu.Lock()
	if !box.loaded && a.db != nil {
		box.loaded = true
		var raw string
		if a.db.Reader.QueryRow(`SELECT value FROM kv WHERE key=?`, ixGoneKey).Scan(&raw) == nil {
			_ = json.Unmarshal([]byte(raw), &box.st)
		}
	}
	box.mu.Unlock()
	return box
}

func (a *App) ixGoneState() ixGoneInfo {
	box := a.ixGoneBox()
	box.mu.Lock()
	defer box.mu.Unlock()
	return box.st
}

// ixIsGone: the archive is read-only; nothing may talk to Telegram.
func (a *App) ixIsGone() bool {
	if a == nil || a.db == nil {
		return false
	}
	return a.ixGoneState().Gone
}

func (a *App) ixSetGone(ctx context.Context, st ixGoneInfo) {
	box := a.ixGoneBox()
	box.mu.Lock()
	box.st = st
	box.mu.Unlock()
	a.ixKeepGone(ctx, st)
}

// ixEnterGone switches to read-only mode (once) and says so in the log.
func (a *App) ixEnterGone(ctx context.Context, reason, code string, now time.Time) {
	box := a.ixGoneBox()
	box.mu.Lock()
	if box.st.Gone {
		box.mu.Unlock()
		return
	}
	box.st.Gone, box.st.Since, box.st.Reason, box.st.Code = true, now.UnixMilli(), reason, code
	st := box.st
	box.mu.Unlock()
	a.ixKeepGone(ctx, st)
	if reason == "date" {
		a.ixLog("the deletion date has passed: Telegram work stopped, the archive is read-only now")
	} else {
		a.ixLog("Telegram account gone (%s): Telegram work stopped, the archive is read-only now", code)
	}
}

// ixKeepGone saves the state across restarts and tells the dashboard.
func (a *App) ixKeepGone(ctx context.Context, st ixGoneInfo) {
	if data, err := json.Marshal(st); err == nil {
		_, _ = a.db.Writer.ExecContext(context.WithoutCancel(ctx), `INSERT INTO kv(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, ixGoneKey, string(data), time.Now().UnixMilli())
	}
	if a.hub != nil {
		a.hub.Broadcast(ws.Event{Type: "ix_readonly", Flat: true, Payload: map[string]any{"gone": st.Gone}, Roles: []string{"admin"}})
	}
}

// ixNoteEngine is told about every engine error (monitorEvent, and failed
// starts). One that says the account may be gone is kept as a sign, and the
// loop looks at once: the engine is down, so the error must repeat (each
// restart of the engine tries the main connection again).
func (a *App) ixNoteEngine(err error) {
	if err == nil || a == nil || a.db == nil || !ixDeactivatedRe.MatchString(err.Error()) {
		return
	}
	now := time.Now().UnixMilli()
	box := a.ixGoneBox()
	box.mu.Lock()
	if n := len(box.engine); n > 0 && now-box.engine[n-1].at < ixGoneEngineMerge.Milliseconds() {
		box.engine[n-1].at = now
	} else {
		box.engine = append(box.engine, ixGoneSign{key: "engine:" + strconv.FormatInt(now, 10), at: now, code: ixDeactivatedRe.FindString(err.Error())})
		if len(box.engine) > 50 {
			box.engine = box.engine[len(box.engine)-50:]
		}
	}
	box.mu.Unlock()
	select {
	case box.wake <- struct{}{}:
	default:
	}
}

// ixGoneHold is the rescan queue's reason to wait ("" = not read-only).
func (a *App) ixGoneHold() string {
	if !a.ixIsGone() {
		return ""
	}
	return "Telegram account gone: the archive is read-only now. No rescan starts."
}

func (a *App) ixRunGone(ctx context.Context) {
	box := a.ixGoneBox()
	wait := ixGoneStartDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-box.wake:
		}
		wait = time.Minute
		a.ixGoneStep(ctx, time.Now())
	}
}

// ixGoneStep enters read-only mode when the account is gone, and keeps the
// engine stopped while it lasts.
func (a *App) ixGoneStep(ctx context.Context, now time.Time) {
	if st := a.ixGoneState(); !st.Gone {
		reason, code := a.ixGoneWhy(ctx, st, now)
		if reason == "" {
			return
		}
		a.ixEnterGone(ctx, reason, code, now)
	}
	a.ixStopTelegram(ctx)
}

// ixGoneWhy says whether the account is gone: "date" or "telegram" (with
// the error code), or "" while it may still work.
func (a *App) ixGoneWhy(ctx context.Context, st ixGoneInfo, now time.Time) (string, string) {
	// The deletion date alone does not stop anything: Telegram may delete
	// later than it said, and every hour it still answers is worth using.
	// Its own errors decide, confirmed (see the top of this file).
	box := a.ixGoneBox()
	box.mu.Lock()
	alive := box.aliveAt
	box.mu.Unlock()
	// Only errors after "Try Telegram again", and after the session last
	// answered on its main connection, count.
	since := max(now.Add(-6*time.Hour).UnixMilli(), st.ClearedAt, alive)
	signs := a.ixGoneSigns(ctx, since)
	if len(signs) == 0 {
		return "", ""
	}
	switch code, alive := a.ixGoneCheck(ctx, now); {
	case code != "":
		return "telegram", code
	case alive:
		return "", ""
	}
	if code := ixGoneRepeated(signs); code != "" {
		return "telegram", code
	}
	return "", ""
}

// ixGoneCheck makes the live check on the main connection (at most every
// ixGoneCheckEvery; in between the last answer stands): code is the error
// that confirms the account is gone, alive that the session answered.
// Neither: no check could be made.
func (a *App) ixGoneCheck(ctx context.Context, now time.Time) (code string, alive bool) {
	box := a.ixGoneBox()
	box.mu.Lock()
	recent := box.checkedAt > 0 && now.UnixMilli()-box.checkedAt < ixGoneCheckEvery.Milliseconds()
	wasAlive := box.checkAlive
	box.mu.Unlock()
	if recent {
		return "", wasAlive
	}
	probe, cancel := context.WithTimeout(ctx, time.Minute)
	err := ixGoneProbe(a, probe)
	cancel()
	box.mu.Lock()
	defer box.mu.Unlock()
	box.checkedAt, box.checkAlive = now.UnixMilli(), err == nil
	switch {
	case err == nil:
		box.aliveAt = now.UnixMilli()
		a.ixLog("Telegram errors said the account may be gone, but its main connection still answers: Telegram work goes on")
		return "", true
	case !errors.Is(err, errIxNoProbe) && ixDeactivatedRe.MatchString(err.Error()):
		return ixDeactivatedRe.FindString(err.Error()), false
	}
	box.checkedAt = 0 // nothing learned: try again at the next look
	return "", false
}

// ixGoneRepeated is the error code when the signs show the error on at
// least ixGoneItems different items over at least ixGoneSpan ("" if not).
func ixGoneRepeated(signs []ixGoneSign) string {
	items := map[string]bool{}
	var first, last ixGoneSign
	for i, s := range signs {
		items[s.key] = true
		if i == 0 || s.at < first.at {
			first = s
		}
		if i == 0 || s.at >= last.at {
			last = s
		}
	}
	if len(items) >= ixGoneItems && last.at-first.at >= ixGoneSpan.Milliseconds() {
		return last.code
	}
	return ""
}

// ixGoneSigns are the errors since a time that say the account may be
// gone, each with the item it was seen on: downloads, backfills, and the
// engine itself.
func (a *App) ixGoneSigns(ctx context.Context, since int64) []ixGoneSign {
	var out []ixGoneSign
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id||'_'||message_id, updated_at, error FROM tgdl_work WHERE error IS NOT NULL AND error<>'' AND updated_at>? ORDER BY updated_at DESC LIMIT 300`, since); err == nil {
		for rows.Next() {
			var key string
			var at int64
			var e sql.NullString
			if rows.Scan(&key, &at, &e) == nil && e.Valid && ixDeactivatedRe.MatchString(e.String) {
				out = append(out, ixGoneSign{key: "work:" + key, at: at, code: ixDeactivatedRe.FindString(e.String)})
			}
		}
		rows.Close()
	}
	a.historyMu.Lock()
	for _, j := range a.historyJobs {
		if j.Error != nil && j.FinishedAt != nil && *j.FinishedAt > since && ixDeactivatedRe.MatchString(*j.Error) {
			out = append(out, ixGoneSign{key: "history:" + j.ID, at: *j.FinishedAt, code: ixDeactivatedRe.FindString(*j.Error)})
		}
	}
	a.historyMu.Unlock()
	box := a.ixGoneBox()
	box.mu.Lock()
	for _, s := range box.engine {
		if s.at > since {
			out = append(out, s)
		}
	}
	box.mu.Unlock()
	return out
}

// ixGoneFilter drops the errors that say the account is gone from errs
// (for the account status: banner, alert, rescans paused) when the main
// connection has answered since the last of them: those came from another
// data centre, not from a dead account.
func (a *App) ixGoneFilter(ctx context.Context, errs []string, now int64) []string {
	if a.ixIsGone() {
		return errs
	}
	box := a.ixGoneBox()
	box.mu.Lock()
	alive := box.aliveAt
	box.mu.Unlock()
	if alive == 0 || len(a.ixGoneSigns(ctx, max(alive, now-6*60*60*1000))) > 0 {
		return errs
	}
	out := errs[:0:0]
	for _, e := range errs {
		if !ixDeactivatedRe.MatchString(e) {
			out = append(out, e)
		}
	}
	return out
}

// ixStopTelegram stops the engine: downloads, live updates and every open
// Telegram session (backfills end with it). Stopping a stopped engine does
// nothing, so this is cheap to repeat.
func (a *App) ixStopTelegram(ctx context.Context) {
	if a.monitor == nil {
		return
	}
	stop, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := a.monitor.Stop(stop); err != nil && ctx.Err() == nil {
		a.ixLog("read-only archive: the Telegram engine did not stop yet: %v", err)
	}
}

// ixGoneClear leaves read-only mode by hand. Monitoring starts again if it
// is meant to run; if the account is really gone, the next error puts the
// archive back in read-only mode.
func (a *App) ixGoneClear(ctx context.Context) {
	box := a.ixGoneBox()
	box.mu.Lock()
	box.engine, box.checkedAt, box.checkAlive = nil, 0, false
	box.mu.Unlock()
	a.ixSetGone(ctx, ixGoneInfo{ClearedAt: time.Now().UnixMilli()})
	a.ixLog("read-only archive turned off by hand: Telegram work may start again")
	a.ixWake()
	a.launchMaintenance(func() {
		a.monitorOp.Lock()
		defer a.monitorOp.Unlock()
		if a.monitorShouldRestart() {
			if err := a.startMonitor(a.ctx); err != nil && a.ctx.Err() == nil {
				a.ixLog("monitor start after leaving read-only mode: %v", err)
			}
		}
	})
}

func (a *App) handleIxGoneClear(w http.ResponseWriter, r *http.Request) {
	a.ixGoneClear(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "gone": a.ixGoneState()})
}

// ixGoneDiag is the report's line: states and times only.
func (a *App) ixGoneDiag(now time.Time) string {
	st := a.ixGoneState()
	if !st.Gone {
		if st.ClearedAt > 0 {
			return "read-only archive: no (turned off by hand " + ixAgo(st.ClearedAt, now) + ")"
		}
		return "read-only archive: no"
	}
	why := "the deletion date passed"
	if st.Reason == "telegram" {
		why = "Telegram said " + st.Code
	}
	return "read-only archive: yes, since " + ixAgo(st.Since, now) + " (" + why + ")"
}
