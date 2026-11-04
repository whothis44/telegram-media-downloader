package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"
)

// Alerts: a message by email and/or to a web address (an ntfy topic, a
// Discord channel, or anything that accepts JSON) when something needs
// attention: the account frozen or logged out, a group that can no longer
// be read, downloads that stopped, the nightly index failing, or the rescan
// queue finishing. Alerts never contain message text; group names only when
// the setting allows it.

const (
	ixAlertEvery  = time.Minute
	ixAlertRepeat = 24 * time.Hour // a condition that persists is reported again after this
	ixStallAfter  = time.Hour      // no file finished for this long while files wait
)

type ixAlert struct {
	Key     string // what it is about; the same key is not sent again for ixAlertRepeat (once ever when Once)
	Once    bool
	Title   string
	Message string
}

// ixAlertConditions looks at the archive's state now.
func (a *App) ixAlertConditions(ctx context.Context, s ixSettings, now time.Time) []ixAlert {
	var out []ixAlert
	acct := a.ixAccount()
	if acct.Deactivated {
		out = append(out, ixAlert{Key: "account:deactivated", Title: "Telegram account logged out or deactivated",
			Message: "Telegram reports the account is logged out or deactivated (" + acct.LastError + "). Rescans are paused."})
	} else if acct.Frozen {
		out = append(out, ixAlert{Key: "account:frozen", Title: "Telegram account frozen",
			Message: "Telegram reports the account is frozen (" + acct.LastError + "). Rescans continue while history can still be read."})
	}
	out = append(out, ixFreezeAlerts(a.ixFreeze(ctx), now)...)
	out = append(out, ixDeadlineAlerts(a.ixDeadlineEstimate(ctx, false), now)...)
	for _, ev := range a.ixFixityProblems(ctx, now) {
		out = append(out, ev)
	}
	if st, err := a.ixLoadQueue(ctx); err == nil {
		var names []string
		var keys []string
		for _, it := range st.Items {
			if it.State == "failed" {
				keys = append(keys, it.GroupID)
				names = append(names, it.Name)
			}
		}
		if len(keys) > 0 {
			msg := fmt.Sprintf("%d group(s) in the rescan queue failed for good and need a look (Archive → Rescan).", len(keys))
			if s.AlertNames {
				msg += " " + strings.Join(names, ", ")
			}
			out = append(out, ixAlert{Key: "failed:" + strings.Join(keys, ","), Title: "Groups could not be rescanned", Message: msg})
		}
		if !st.Running && strings.HasPrefix(st.Notice, "Finished") && len(st.Items) > 0 {
			out = append(out, ixAlert{Key: fmt.Sprintf("finished:%d", st.UpdatedAt), Once: true, Title: "Rescan queue finished",
				Message: fmt.Sprintf("Every group in the rescan queue has been processed (%d group(s)).", len(st.Items))})
		}
	}
	d := a.ixDownloads(ctx)
	switch {
	case d.Waiting > 0 && !d.EngineRunning:
		out = append(out, ixAlert{Key: "downloads:engine", Title: "Downloads stopped",
			Message: fmt.Sprintf("%d file(s) are waiting, but the download engine is not running.", d.Waiting)})
	case d.Waiting > 0 && !d.QueuePaused && d.LastHour == 0 && (d.LastAt == 0 || now.Sub(time.UnixMilli(d.LastAt)) > ixStallAfter):
		out = append(out, ixAlert{Key: "downloads:stalled", Title: "Downloads stalled",
			Message: fmt.Sprintf("%d file(s) are waiting, but none has finished for over an hour.", d.Waiting)})
	}
	out = append(out, ixStagingAlerts(a.ixStagingStatus(ctx), now)...)
	if ex := a.ixExportStatus(); ex.Error != "" && ex.FinishedAt > 0 {
		out = append(out, ixAlert{Key: fmt.Sprintf("export:%d", ex.FinishedAt), Once: true, Title: "Writing the index failed",
			Message: "The index and data files could not be written to the NAS: " + ex.Error})
	}
	return out
}

func (a *App) ixRunAlerts(ctx context.Context) {
	ticker := time.NewTicker(ixAlertEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s := a.ixLoadSettings(ctx)
		if s.AlertWebhook == "" && s.AlertEmailTo == "" {
			continue
		}
		now := time.Now()
		for _, al := range a.ixAlertConditions(ctx, s, now) {
			var sent int64
			_ = a.db.Reader.QueryRowContext(ctx, `SELECT sent_at FROM ix_alerts WHERE key=?`, al.Key).Scan(&sent)
			if sent > 0 && (al.Once || now.Sub(time.UnixMilli(sent)) < ixAlertRepeat) {
				continue
			}
			if err := a.ixSendAlert(ctx, s, al); err != nil {
				a.ixLog("alert %q: %v", al.Title, err)
				continue
			}
			_, _ = a.db.Writer.ExecContext(ctx, `INSERT INTO ix_alerts(key,sent_at) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET sent_at=excluded.sent_at`, al.Key, now.UnixMilli())
		}
	}
}

// ixSendAlert delivers one alert to every configured destination; it fails
// only when none could be reached.
func (a *App) ixSendAlert(ctx context.Context, s ixSettings, al ixAlert) error {
	var errs []string
	delivered := false
	if s.AlertWebhook != "" {
		if err := ixPostAlert(ctx, s, al); err != nil {
			errs = append(errs, "web: "+err.Error())
		} else {
			delivered = true
		}
	}
	if s.AlertEmailTo != "" {
		if err := ixMailAlert(ctx, s, al); err != nil {
			errs = append(errs, "email: "+err.Error())
		} else {
			delivered = true
		}
	}
	if !delivered {
		if len(errs) == 0 {
			return errors.New("no alert destination is set")
		}
		return errors.New(strings.Join(errs, "; "))
	}
	if len(errs) > 0 {
		a.ixLog("alert %q: %s", al.Title, strings.Join(errs, "; "))
	}
	return nil
}

var ixAlertClient = &http.Client{Timeout: 20 * time.Second}

func ixPostAlert(ctx context.Context, s ixSettings, al ixAlert) error {
	var body []byte
	contentType := "application/json"
	switch s.AlertWebhookKind {
	case "discord":
		body, _ = json.Marshal(map[string]string{"content": "**" + al.Title + "**\n" + al.Message})
	case "json":
		body, _ = json.Marshal(map[string]any{"title": al.Title, "message": al.Message, "time": time.Now().UTC().Format(time.RFC3339), "source": "telegram archive"})
	default: // ntfy: the body is the message, the title a header
		body, contentType = []byte(al.Message), "text/plain; charset=utf-8"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.AlertWebhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if s.AlertWebhookKind == "" || s.AlertWebhookKind == "ntfy" {
		req.Header.Set("Title", al.Title)
		req.Header.Set("Tags", "warning")
	}
	resp, err := ixAlertClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("the web address answered %s", resp.Status)
	}
	return nil
}

func ixMailAlert(ctx context.Context, s ixSettings, al ixAlert) error {
	if s.AlertSMTPHost == "" {
		return errors.New("no mail server set")
	}
	port := s.AlertSMTPPort
	if port == "" {
		port = "587"
	}
	from := s.AlertEmailFrom
	if from == "" {
		from = s.AlertSMTPUser
	}
	if from == "" {
		from = s.AlertEmailTo
	}
	addr := net.JoinHostPort(s.AlertSMTPHost, port)
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	var err error
	if port == "465" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: s.AlertSMTPHost})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(time.Minute))
	c, err := smtp.NewClient(conn, s.AlertSMTPHost)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if port != "465" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err = c.StartTLS(&tls.Config{ServerName: s.AlertSMTPHost}); err != nil {
				return err
			}
		} else if s.AlertSMTPUser != "" {
			return errors.New("the mail server does not offer encryption; not sending the password")
		}
	}
	if s.AlertSMTPUser != "" {
		if err = c.Auth(smtp.PlainAuth("", s.AlertSMTPUser, s.AlertSMTPPass, s.AlertSMTPHost)); err != nil {
			return err
		}
	}
	if err = c.Mail(from); err != nil {
		return err
	}
	for _, to := range strings.Split(s.AlertEmailTo, ",") {
		if to = strings.TrimSpace(to); to != "" {
			if err = c.Rcpt(to); err != nil {
				return err
			}
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	msg := "From: " + from + "\r\nTo: " + s.AlertEmailTo + "\r\nSubject: Telegram archive: " + al.Title +
		"\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + al.Message + "\r\n"
	if _, err = w.Write([]byte(msg)); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func (a *App) handleIxAlertTest(w http.ResponseWriter, r *http.Request) {
	s := a.ixLoadSettings(r.Context())
	if s.AlertWebhook == "" && s.AlertEmailTo == "" {
		writeJSONError(w, http.StatusBadRequest, "Set a web address or an email address for alerts first, and save.")
		return
	}
	err := a.ixSendAlert(r.Context(), s, ixAlert{Key: "test", Title: "Test alert", Message: "Alerts from the Telegram archive reach you here."})
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}
