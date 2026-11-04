package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/ws"
)

// Read-only archive mode, for after the account is gone. When Telegram says
// the account is deleted or deactivated, or that the session is no longer
// valid (USER_DEACTIVATED, AUTH_KEY_UNREGISTERED, SESSION_REVOKED…; see
// ixClassify), or once the deletion date has passed, the app stops all
// Telegram work cleanly instead of retrying a dead session: the download
// engine is stopped and is not started again (the watchdog, the restart
// after an error, and every feature that opens a Telegram connection go
// through startTelegramEngine), rescans do not start, and freeze checks
// stop. Everything that works from the saved archive keeps going: the
// browser, search, recognition, faces, exports and backups.
//
// The state is kept in kv, so it holds across restarts. "Try Telegram
// again" (Overview) leaves the mode, for an appeal that was accepted; fresh
// errors put it back.

const ixGoneKey = "ix_gone"

// errIxReadOnly is what any attempt to connect to Telegram gets meanwhile.
var errIxReadOnly = errors.New("Telegram account gone: the archive is read-only now (Archive → Overview → Try Telegram again)")

// ixGoneStartDelay is the wait before the first check after a start.
var ixGoneStartDelay = time.Minute

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
	// The last engine error that said the account is gone, and when.
	engineErr string
	engineAt  int64
	wake      chan struct{}
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
// starts). One that says the account is gone switches to read-only mode
// right away, before anything can restart the engine, and the loop then
// stops the engine.
func (a *App) ixNoteEngine(err error) {
	if err == nil || a == nil || a.db == nil || !ixDeactivatedRe.MatchString(err.Error()) {
		return
	}
	now := time.Now()
	box := a.ixGoneBox()
	box.mu.Lock()
	box.engineErr, box.engineAt = err.Error(), now.UnixMilli()
	box.mu.Unlock()
	a.ixEnterGone(context.Background(), "telegram", ixDeactivatedRe.FindString(err.Error()), now)
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
	if fz := a.ixFreeze(ctx); fz.Until > 0 && now.Unix() >= fz.Until && st.ClearedAt < fz.Until*1000 {
		return "date", ""
	}
	// Only errors after "Try Telegram again" count.
	since := max(now.Add(-6*time.Hour).UnixMilli(), st.ClearedAt)
	if c := ixClassify(a.ixGoneErrors(ctx, since), now.UnixMilli()); c.Deactivated {
		return "telegram", c.LastError
	}
	return "", ""
}

// ixGoneErrors are the errors seen since a time: downloads, backfills, and
// the engine itself.
func (a *App) ixGoneErrors(ctx context.Context, since int64) []string {
	var out []string
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT error FROM tgdl_work WHERE error IS NOT NULL AND error<>'' AND updated_at>? ORDER BY updated_at DESC LIMIT 300`, since); err == nil {
		for rows.Next() {
			var e sql.NullString
			if rows.Scan(&e) == nil && e.Valid {
				out = append(out, e.String)
			}
		}
		rows.Close()
	}
	a.historyMu.Lock()
	for _, j := range a.historyJobs {
		if j.Error != nil && j.FinishedAt != nil && *j.FinishedAt > since {
			out = append(out, *j.Error)
		}
	}
	a.historyMu.Unlock()
	box := a.ixGoneBox()
	box.mu.Lock()
	if box.engineAt > since {
		out = append(out, box.engineErr)
	}
	box.mu.Unlock()
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
