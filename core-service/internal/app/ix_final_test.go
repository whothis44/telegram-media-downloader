package app

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestIxFinalRunTheDayBeforeDeletion(t *testing.T) {
	a, ctx, dir := ixBackupFixture(t)
	if err := os.MkdirAll(a.downloadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 29, 1, 0, 0, 0, time.Local) // before the nightly time
	a.ixSaveFreeze(ctx, ixFreezeInfo{FreezeState: ixTestFreeze(now.Add(10 * time.Hour).Unix())})
	a.ixBackupTick(ctx, now)
	a.ixBackupTick(ctx, now.Add(time.Minute)) // once
	fr := a.ixFinalRunState(ctx)
	if fr.FinishedAt == 0 || fr.ExportError != "" || fr.BackupError != "" {
		t.Fatalf("final run: %+v", fr)
	}
	finals := 0
	for _, n := range ixBackupNames(t, dir) {
		if strings.HasSuffix(n, " final") {
			finals++
		}
	}
	if finals != 1 || a.ixExportStatus().FinishedAt == 0 {
		t.Fatalf("final copies %d, index %+v", finals, a.ixExportStatus())
	}
	// More than a day away, or past: no run.
	if !strings.Contains(a.ixFinalDiag(ctx, now), "day-before run: ") || a.ixFinalRunState(ctx).Starts != 1 {
		t.Fatal(a.ixFinalDiag(ctx, now))
	}
	if ov := a.ixFinalOverview(ctx, now); ov["window"] != true {
		t.Fatalf("banner: %v", ov)
	}
	if ixFinalWindow(now.Add(25*time.Hour).Unix(), now) || ixFinalWindow(now.Add(-time.Hour).Unix(), now) || !ixFinalWindow(now.Add(23*time.Hour).Unix(), now) {
		t.Fatal("window")
	}
}
