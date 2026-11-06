package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/ws"
)

// Settings and account safety for the archive tools.

const ixSettingsKey = "ix_settings"

// ixSafetyAppliesSettings: the safety loop re-applies the engine's settings once a
// minute. They are global, so in tests that hold them to a value it would overwrite
// them part-way through a long run.
var ixSafetyAppliesSettings = true

type ixSettings struct {
	ExportEnabled bool   `json:"exportEnabled"` // write the static index every night
	ExportTime    string `json:"exportTime"`    // HH:MM, local time of the server
	ExportDir     string `json:"exportDir"`     // empty: next to the downloads folder
	// BackupOff turns off the nightly backup of the Telegram session(s) and
	// the database next to the index copy (ix_backup.go); on by default.
	BackupOff     bool   `json:"backupOff"`
	PauseOnFreeze bool   `json:"pauseOnFreeze"` // stop starting rescans when Telegram reports the account frozen
	QuietStart    string `json:"quietStart"`    // HH:MM; no new rescans start between QuietStart and QuietEnd
	QuietEnd      string `json:"quietEnd"`
	Pace          string `json:"pace"`          // "normal" or "gentle"
	DupEnabled    bool   `json:"dupEnabled"`    // look for near-duplicate videos in the background
	DupHWAccel    string `json:"dupHwaccel"`    // "cuda" (default), "none", or another ffmpeg backend
	DupQuietStart string `json:"dupQuietStart"` // HH:MM; no fingerprinting between start and end
	DupQuietEnd   string `json:"dupQuietEnd"`
	AutoRetry     bool   `json:"autoRetry"`     // put failed downloads back in the queue every few hours
	FixityEnabled bool   `json:"fixityEnabled"` // re-read every stored file about once a month
	SubstExact    bool   `json:"substExact"`    // same-size re-uploads: check bytes on Telegram instead of downloading
	HoldLikely    bool   `json:"holdLikely"`    // probable copies wait until nothing else is queued
	HoldForever   bool   `json:"holdForever"`   // ...and are never released automatically
	MLURL         string `json:"mlUrl"`         // image recognition (Immich machine learning); empty: off
	MLModel       string `json:"mlModel"`       // its model; empty: ixMLDefaultModel
	MLFaces       bool   `json:"mlFaces"`       // find faces and group them into people (off unless turned on)
	MLTextModel   string `json:"mlTextModel"`   // text-reading model (by script); empty: ixOCRDefaultModel
	MLText        bool   `json:"mlText"`        // read text in pictures
	MLDupes       bool   `json:"mlDupes"`       // near-identical pictures by recognition go to review
	MLThings      string `json:"mlThings"`      // subjects to tag, one per line; empty: ixDefaultThings
	STTURL        string `json:"sttUrl"`        // speech to text (an OpenAI-compatible Whisper server); empty: off
	STTModel      string `json:"sttModel"`      // its model; empty: ixSTTDefaultModel
	// The Docker containers of the two services (see ix_docker.go), and
	// whether the archive starts them when it starts.
	MLContainer   string `json:"mlContainer"`
	STTContainer  string `json:"sttContainer"`
	MLAutoStart   bool   `json:"mlAutoStart"`
	DeadlineOrder string `json:"deadlineOrder"` // auto (when the backup would not finish before deletion), on, off
	GroupPolicy   string `json:"groupPolicy"`   // what the group ranking means: order (default), spread (ix_sched.go)
	// DownloadPacing lets the download pacer set how many file parts are in
	// flight (ix.9); off goes back to how ix.8 downloaded.
	DownloadPacing bool `json:"downloadPacing"`
	// PaceByAnswers judges the pacer's tries by how fast Telegram answers
	// (ix.12.3); off unless set. Off, the report still says where it would
	// have decided otherwise.
	PaceByAnswers bool `json:"paceByAnswers"`
	// Big videos and the NAS (ix_lowq.go, ix_staging.go).
	LowqFirst  bool   `json:"lowqFirst"`  // save a lower quality of very large videos first when time is short
	LowqKeep   bool   `json:"lowqKeep"`   // keep that lower quality after the original arrives
	StageDir   string `json:"stageDir"`   // local folder downloads go to before the library; empty: off
	StageCapGB int    `json:"stageCapGb"` // space staged files may take
	// BigAtOnce is how many files of 1 GB or more download at once
	// (engine.BigFileLane; 0: no limit).
	BigAtOnce int `json:"bigAtOnce"`
	// Alerts (see ix_alerts.go). The SMTP password is never sent back to
	// the dashboard; AlertSMTPPassSet says whether one is saved.
	AlertWebhook     string `json:"alertWebhook"`
	AlertWebhookKind string `json:"alertWebhookKind"` // ntfy, discord or json
	AlertSMTPHost    string `json:"alertSmtpHost"`
	AlertSMTPPort    string `json:"alertSmtpPort"`
	AlertSMTPUser    string `json:"alertSmtpUser"`
	AlertSMTPPass    string `json:"alertSmtpPass,omitempty"`
	AlertSMTPPassSet bool   `json:"alertSmtpPassSet"`
	AlertEmailFrom   string `json:"alertEmailFrom"`
	AlertEmailTo     string `json:"alertEmailTo"`
	AlertNames       bool   `json:"alertNames"` // name the groups in alerts
	// Copies (ix_copies.go). Both off by default means the recommended way.
	ConfirmedGoEarly bool `json:"confirmedGoEarly"` // confirmed copies may go once the queue is empty, like other held copies
	CopyThumbsOff    bool `json:"copyThumbsOff"`    // no Telegram previews fetched to compare by recognition
	// PreviewsAll fetches Telegram's 320 px preview of every photo and video
	// not downloaded (ix_previews_all.go): auto (while a deletion date is
	// set), on, off.
	PreviewsAll string `json:"previewsAll"`
	// AutoUpdate starts the update's drain by itself when a newer release is
	// out (ix_update.go). It needs a launcher that says it loops; off unless set.
	AutoUpdate bool `json:"autoUpdate"`
}

// public is the settings as the dashboard may see them.
func (s ixSettings) public() ixSettings {
	s.AlertSMTPPassSet = s.AlertSMTPPass != ""
	s.AlertSMTPPass = ""
	return s
}

func ixDefaultSettings() ixSettings {
	return ixSettings{ExportEnabled: true, ExportTime: "03:30", Pace: "normal", DupEnabled: true, DupHWAccel: "cuda", AutoRetry: true, FixityEnabled: true, SubstExact: true, HoldLikely: true, MLText: true, MLDupes: true, MLContainer: ixMLDefaultContainer, STTContainer: ixSTTDefaultContainer, DeadlineOrder: "auto", DownloadPacing: true, LowqFirst: true, StageCapGB: 20, BigAtOnce: engine.DefaultBigFileLane, AlertWebhookKind: "ntfy", AlertSMTPPort: "587", PreviewsAll: "auto"}
}

func (a *App) ixLoadSettings(ctx context.Context) ixSettings {
	s := ixDefaultSettings()
	var raw string
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT value FROM kv WHERE key=?`, ixSettingsKey).Scan(&raw); err == nil {
		_ = json.Unmarshal([]byte(raw), &s)
	}
	if !ixValidClock(s.ExportTime) {
		s.ExportTime = "03:30"
	}
	if s.Pace != "gentle" {
		s.Pace = "normal"
	}
	return s
}

func (a *App) ixSaveSettings(ctx context.Context, s ixSettings) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = a.db.Writer.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, ixSettingsKey, string(data), time.Now().UnixMilli())
	return err
}

var ixClockRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

func ixValidClock(s string) bool { return ixClockRe.MatchString(s) }

// ixInQuietHours reports whether now (server local time) is inside the
// window; a window may cross midnight (22:00–07:00).
func ixInQuietHours(s ixSettings, now time.Time) bool {
	if !ixValidClock(s.QuietStart) || !ixValidClock(s.QuietEnd) || s.QuietStart == s.QuietEnd {
		return false
	}
	clock := now.Format("15:04")
	if s.QuietStart < s.QuietEnd {
		return clock >= s.QuietStart && clock < s.QuietEnd
	}
	return clock >= s.QuietStart || clock < s.QuietEnd
}

// ixApplyPace sets the backfill engine's own pacing knobs. Gentle pauses
// 2 seconds every 50 messages and a minute every 500, and keeps the
// download queue short.
func (a *App) ixApplyPace(ctx context.Context, pace string) error {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	cfg, err := a.config.Load(ctx)
	if err != nil {
		return err
	}
	advanced := ensureMap(cfg, "advanced")
	history := ensureMap(advanced, "history")
	if pace == "gentle" {
		history["shortBreakEveryN"] = 50
		history["longBreakEveryN"] = 500
		history["backpressureCap"] = 200
	} else {
		history["shortBreakEveryN"] = 100
		history["longBreakEveryN"] = 1000
		history["backpressureCap"] = 500
	}
	if err = a.config.Save(ctx, cfg); err != nil {
		return err
	}
	if a.hub != nil {
		a.hub.Broadcast(ws.Event{Type: "config_updated", Flat: true})
	}
	return nil
}

// ---------- account status ----------

type ixAccountStatus struct {
	Frozen      bool   `json:"frozen"`
	Deactivated bool   `json:"deactivated"`
	FloodWait   bool   `json:"floodWait"`
	LastError   string `json:"lastError,omitempty"`
	SeenAt      int64  `json:"seenAt,omitempty"`
	CheckedAt   int64  `json:"checkedAt,omitempty"`
}

var ixAccountState sync.Map // *App -> ixAccountStatus

var (
	ixFrozenRe      = regexp.MustCompile(`FROZEN_[A-Z_]+`)
	ixDeactivatedRe = regexp.MustCompile(`USER_DEACTIVATED(_BAN)?|AUTH_KEY_UNREGISTERED|SESSION_REVOKED|PHONE_NUMBER_BANNED|AUTH_KEY_DUPLICATED`)
	ixFloodRe       = regexp.MustCompile(`FLOOD_WAIT_?\d*|FLOOD_PREMIUM_WAIT|PEER_FLOOD`)
)

// ixClassify reads Telegram error codes out of recent error messages.
func ixClassify(errorsSeen []string, now int64) ixAccountStatus {
	st := ixAccountStatus{CheckedAt: now}
	for _, e := range errorsSeen {
		switch {
		case ixDeactivatedRe.MatchString(e):
			st.Deactivated = true
			st.LastError = ixDeactivatedRe.FindString(e)
		case ixFrozenRe.MatchString(e):
			st.Frozen = true
			if !st.Deactivated {
				st.LastError = ixFrozenRe.FindString(e)
			}
		case ixFloodRe.MatchString(e):
			st.FloodWait = true
			if st.LastError == "" {
				st.LastError = ixFloodRe.FindString(e)
			}
		}
	}
	if st.LastError != "" {
		st.SeenAt = now
	}
	return st
}

func (a *App) ixRecentErrors(ctx context.Context, since int64) []string {
	var out []string
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT error FROM tgdl_work WHERE error IS NOT NULL AND error<>'' AND updated_at>? ORDER BY updated_at DESC LIMIT 300`, since)
	if err == nil {
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
		if j.Error != nil && (j.FinishedAt == nil || *j.FinishedAt > since) {
			out = append(out, *j.Error)
		}
	}
	a.historyMu.Unlock()
	if st, err := a.ixLoadQueue(ctx); err == nil {
		for _, item := range st.Items {
			if item.Error != "" {
				out = append(out, item.Error)
			}
		}
	}
	return out
}

func (a *App) ixAccount() ixAccountStatus {
	if v, ok := ixAccountState.Load(a); ok {
		return v.(ixAccountStatus)
	}
	return ixAccountStatus{}
}

func (a *App) ixRunSafety(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if ixSafetyAppliesSettings {
			ixApplyEngineSettings(a.ixLoadSettings(ctx))
		}
		now := time.Now().UnixMilli()
		st := ixClassify(a.ixGoneFilter(ctx, a.ixRecentErrors(ctx, now-6*60*60*1000), now), now)
		old := a.ixAccount()
		ixAccountState.Store(a, st)
		if (st.Frozen != old.Frozen || st.Deactivated != old.Deactivated) && a.hub != nil {
			a.hub.Broadcast(ws.Event{Type: "ix_account", Flat: true, Payload: map[string]any{"frozen": st.Frozen, "deactivated": st.Deactivated}, Roles: []string{"admin"}})
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ixHold explains why no new rescan may start right now ("" = go ahead).
func (a *App) ixHold(ctx context.Context) string {
	if why := a.ixGoneHold(); why != "" {
		return why
	}
	acct := a.ixAccount()
	if acct.Deactivated {
		return "Paused: Telegram reports this account is logged out or deactivated (" + acct.LastError + ")."
	}
	s := a.ixLoadSettings(ctx)
	if acct.Frozen && s.PauseOnFreeze {
		return "Paused: Telegram reports this account is frozen (" + acct.LastError + "), and the setting to pause on a freeze is on."
	}
	if ixInQuietHours(s, time.Now()) {
		return "Quiet hours (" + s.QuietStart + "–" + s.QuietEnd + "): the next group starts after " + s.QuietEnd + "."
	}
	return ""
}

func ixParseSettings(body map[string]any, s ixSettings) (ixSettings, error) {
	s = ixParseBackupSettings(body, s)
	if v, ok := body["exportEnabled"].(bool); ok {
		s.ExportEnabled = v
	}
	if v, ok := body["pauseOnFreeze"].(bool); ok {
		s.PauseOnFreeze = v
	}
	if v, ok := body["dupEnabled"].(bool); ok {
		s.DupEnabled = v
	}
	if v, ok := body["autoRetry"].(bool); ok {
		s.AutoRetry = v
	}
	for key, dst := range map[string]*bool{"fixityEnabled": &s.FixityEnabled, "substExact": &s.SubstExact, "holdLikely": &s.HoldLikely, "holdForever": &s.HoldForever,
		"mlFaces": &s.MLFaces, "mlText": &s.MLText, "mlDupes": &s.MLDupes, "downloadPacing": &s.DownloadPacing, "mlAutoStart": &s.MLAutoStart, "autoUpdate": &s.AutoUpdate, "paceByAnswers": &s.PaceByAnswers} {
		if v, ok := body[key].(bool); ok {
			*dst = v
		}
	}
	if v, ok := body["alertNames"].(bool); ok {
		s.AlertNames = v
	}
	for key, dst := range map[string]*string{"alertWebhook": &s.AlertWebhook, "alertSmtpHost": &s.AlertSMTPHost, "alertSmtpPort": &s.AlertSMTPPort,
		"alertSmtpUser": &s.AlertSMTPUser, "alertEmailFrom": &s.AlertEmailFrom, "alertEmailTo": &s.AlertEmailTo} {
		if v, ok := body[key].(string); ok {
			*dst = strings.TrimSpace(v)
		}
	}
	if v, ok := body["alertSmtpPass"].(string); ok && v != "" {
		s.AlertSMTPPass = v // empty: keep the saved password
	}
	if v, ok := body["alertSmtpPassClear"].(bool); ok && v {
		s.AlertSMTPPass = ""
	}
	if v, ok := body["mlUrl"].(string); ok {
		u, err := ixMLCheckURL(v)
		if err != nil {
			return s, err
		}
		s.MLURL = u
	}
	if v, ok := body["mlModel"].(string); ok {
		v = strings.TrimSpace(v)
		if v != "" && !ixMLModelRe.MatchString(v) {
			return s, errors.New("the model name may hold letters, digits, dots, dashes and underscores only")
		}
		s.MLModel = v
	}
	if v, ok := body["mlTextModel"].(string); ok {
		if _, known := ixOCRModels[v]; v != "" && !known {
			return s, errors.New("unknown text-reading model")
		}
		s.MLTextModel = v
	}
	if v, ok := body["mlThings"].(string); ok {
		var lines []string
		for _, l := range strings.Split(v, "\n") {
			if l = strings.TrimSpace(l); l != "" && len(l) <= 80 && len(lines) < 200 {
				lines = append(lines, l)
			}
		}
		s.MLThings = strings.Join(lines, "\n")
	}
	if v, ok := body["sttUrl"].(string); ok {
		u, err := ixMLCheckURL(v)
		if err != nil {
			return s, errors.New(strings.Replace(err.Error(), "image recognition", "speech to text", 1))
		}
		s.STTURL = u
	}
	if v, ok := body["sttModel"].(string); ok {
		v = strings.TrimSpace(v)
		if v != "" && !ixSTTModelRe.MatchString(v) {
			return s, errors.New("the speech model name may hold letters, digits, dots, dashes, underscores and one slash")
		}
		s.STTModel = v
	}
	for _, c := range []struct {
		key, def string
		dst      *string
	}{{"mlContainer", ixMLDefaultContainer, &s.MLContainer}, {"sttContainer", ixSTTDefaultContainer, &s.STTContainer}} {
		if v, ok := body[c.key].(string); ok {
			if v = strings.TrimSpace(v); v == "" {
				v = c.def
			}
			if !ixContainerRe.MatchString(v) {
				return s, errors.New("a Docker container name may hold letters, digits, dots, dashes and underscores only (up to 64), starting with a letter or digit")
			}
			*c.dst = v
		}
	}
	if v, ok := body["deadlineOrder"].(string); ok {
		if v != "auto" && v != "on" && v != "off" {
			return s, errors.New("deadlineOrder must be auto, on or off")
		}
		s.DeadlineOrder = v
	}
	if v, ok := body["alertWebhookKind"].(string); ok {
		if v != "ntfy" && v != "discord" && v != "json" {
			return s, errors.New("alertWebhookKind must be ntfy, discord or json")
		}
		s.AlertWebhookKind = v
	}
	if s.AlertWebhook != "" && !strings.HasPrefix(s.AlertWebhook, "https://") && !strings.HasPrefix(s.AlertWebhook, "http://") {
		return s, errors.New("the alert web address must start with https://")
	}
	if s.AlertSMTPPort != "" {
		if n, err := strconv.Atoi(s.AlertSMTPPort); err != nil || n <= 0 || n > 65535 {
			return s, errors.New("the mail server port must be a number")
		}
	}
	if v, ok := body["dupHwaccel"].(string); ok {
		v = strings.ToLower(strings.TrimSpace(v))
		if v != "none" && v != "cuda" && allowedThumbAccel(v) == "" {
			return s, errors.New("dupHwaccel must be cuda, none, d3d11va, dxva2, qsv or vaapi")
		}
		s.DupHWAccel = v
	}
	if v, ok := body["exportTime"].(string); ok {
		if !ixValidClock(v) {
			return s, errors.New("exportTime must be HH:MM")
		}
		s.ExportTime = v
	}
	for key, dst := range map[string]*string{"quietStart": &s.QuietStart, "quietEnd": &s.QuietEnd, "dupQuietStart": &s.DupQuietStart, "dupQuietEnd": &s.DupQuietEnd} {
		if v, ok := body[key].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" && !ixValidClock(v) {
				return s, errors.New(key + " must be HH:MM or empty")
			}
			*dst = v
		}
	}
	if v, ok := body["exportDir"].(string); ok {
		s.ExportDir = strings.TrimSpace(v)
	}
	if v, ok := body["pace"].(string); ok {
		if v != "gentle" && v != "normal" {
			return s, errors.New("pace must be normal or gentle")
		}
		s.Pace = v
	}
	ixParseCopySettings(body, &s)
	if err := ixParseTransferSettings(body, &s); err != nil {
		return s, err
	}
	return s, nil
}
