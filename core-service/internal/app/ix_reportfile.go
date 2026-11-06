package app

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ix.12.8: the diagnostics report, saved to the data folder.
//
// The report (ixDiagnostics: chats, files, people and links replaced by
// placeholders) is on the dashboard behind its login. Saved as a file every
// ixReportEvery it can be read on this computer without the login, and the
// copy kept each hour for ixReportHours shows how a day went, not only the
// moment it is asked for. Nothing is sent anywhere.
const (
	ixReportDir   = "ixreports"
	ixReportHours = 48
)

var ixReportEvery = 10 * time.Minute

func (a *App) ixRunReportFile(ctx context.Context) {
	if a.dataDir == "" {
		return
	}
	// Not at once: the first minutes after a start say little.
	t := time.NewTimer(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := a.ixSaveReport(ctx, time.Now()); err != nil && ctx.Err() == nil {
			a.ixLog("could not save the diagnostics report: %v", err)
		}
		t.Reset(ixReportEvery)
	}
}

// ixSaveReport writes latest.txt, and the hour's copy (YYYY-MM-DD_HH.txt,
// local time) when the hour has none yet; copies beyond ixReportHours go.
func (a *App) ixSaveReport(ctx context.Context, now time.Time) error {
	dir := filepath.Join(a.dataDir, ixReportDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	text := []byte(a.ixDiagnostics(ctx))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := ixWriteFileAtomic(filepath.Join(dir, "latest.txt"), text); err != nil {
		return err
	}
	hour := filepath.Join(dir, now.In(ixTZ).Format("2006-01-02_15")+".txt")
	if _, err := os.Stat(hour); os.IsNotExist(err) {
		if err := ixWriteFileAtomic(hour, text); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var hours []string
	for _, e := range entries {
		if n := e.Name(); e.Type().IsRegular() && n != "latest.txt" && strings.HasSuffix(n, ".txt") && len(n) == len("2006-01-02_15.txt") {
			hours = append(hours, n)
		}
	}
	sort.Strings(hours)
	for len(hours) > ixReportHours {
		_ = os.Remove(filepath.Join(dir, hours[0]))
		hours = hours[1:]
	}
	return nil
}

func ixWriteFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
