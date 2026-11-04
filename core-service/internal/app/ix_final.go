package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// The day before Telegram's deletion date: as soon as the date is less than
// 24 hours away, one more index copy and backup are made at once (once), and
// the Overview reminds the user to keep the app running and to copy the
// app's data folder somewhere safe. The backup made then is named
// "<date and time> final" and is never rotated away.

const ixFinalKey = "ix_final_run"

type ixFinalRun struct {
	Until       int64  `json:"until"` // the deletion date it was made for (Unix seconds)
	StartedAt   int64  `json:"startedAt,omitempty"`
	FinishedAt  int64  `json:"finishedAt,omitempty"`
	ExportError string `json:"exportError,omitempty"`
	BackupError string `json:"backupError,omitempty"`
	Starts      int    `json:"starts,omitempty"` // a run cut short by a restart is started once more
}

// ixFinalWindow: Telegram's deletion date is less than a day away.
func ixFinalWindow(until int64, now time.Time) bool {
	return until > 0 && now.Unix() < until && until-now.Unix() < 24*60*60
}

func (a *App) ixFinalRunState(ctx context.Context) ixFinalRun {
	var fr ixFinalRun
	var raw string
	if a.db.Reader.QueryRowContext(ctx, `SELECT value FROM kv WHERE key=?`, ixFinalKey).Scan(&raw) == nil {
		_ = json.Unmarshal([]byte(raw), &fr)
	}
	return fr
}

func (a *App) ixSaveFinalRun(ctx context.Context, fr ixFinalRun) {
	data, _ := json.Marshal(fr)
	_, _ = a.db.Writer.ExecContext(context.WithoutCancel(ctx), `INSERT INTO kv(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, ixFinalKey, string(data), time.Now().UnixMilli())
}

// ixFinalStep writes one more index copy and backup, once, as soon as the
// deletion date is less than a day away.
func (a *App) ixFinalStep(ctx context.Context, now time.Time) {
	fz := a.ixFreeze(ctx)
	if !ixFinalWindow(fz.Until, now) {
		return
	}
	fr := a.ixFinalRunState(ctx)
	if fr.Until == fz.Until && (fr.FinishedAt > 0 || fr.Starts >= 2) {
		return
	}
	if fr.Until != fz.Until {
		fr = ixFinalRun{Until: fz.Until}
	}
	fr.StartedAt, fr.Starts = now.UnixMilli(), fr.Starts+1
	a.ixSaveFinalRun(ctx, fr)
	a.ixLog("the deletion date is less than a day away: writing a last index copy and backup")
	wait := func(busy func() bool) {
		for busy() && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
	wait(func() bool { return a.ixExportStatus().Running })
	if err := a.ixExport(ctx); err != nil {
		fr.ExportError = err.Error()
		a.ixLog("last index copy: %v", err)
	}
	wait(func() bool { return a.ixBackupState().Running })
	if err := a.ixBackup(ctx, "final", time.Now()); err != nil {
		fr.BackupError = err.Error()
		a.ixLog("last backup: %v", err)
	}
	fr.FinishedAt = time.Now().UnixMilli()
	a.ixSaveFinalRun(ctx, fr)
}

// ixFinalOverview is what the Overview banner needs.
func (a *App) ixFinalOverview(ctx context.Context, now time.Time) map[string]any {
	fz := a.ixFreeze(ctx)
	fr := a.ixFinalRunState(ctx)
	if fr.Until != fz.Until {
		fr = ixFinalRun{}
	}
	return map[string]any{"window": ixFinalWindow(fz.Until, now), "run": fr}
}

// ixFinalDiag is the report's part about it.
func (a *App) ixFinalDiag(ctx context.Context, now time.Time) string {
	fz := a.ixFreeze(ctx)
	ok := map[bool]string{true: "ok", false: "failed"}
	if fr := a.ixFinalRunState(ctx); fr.Until != 0 && fr.Until == fz.Until {
		if fr.FinishedAt == 0 {
			return fmt.Sprintf("day-before run: started %s, not finished", ixAgo(fr.StartedAt, now))
		}
		return fmt.Sprintf("day-before run: %s (index %s, backup %s)", ixAgo(fr.FinishedAt, now), ok[fr.ExportError == ""], ok[fr.BackupError == ""])
	}
	if ixFinalWindow(fz.Until, now) {
		return "day-before run: due"
	}
	return "day-before run: not due"
}
