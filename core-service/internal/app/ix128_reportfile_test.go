package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The report saved to data/ixreports: latest.txt each time, one copy an
// hour, 48 kept, and the same placeholders as the dashboard's (invented
// names).
func TestIx128ReportSavedToTheDataFolder(t *testing.T) {
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cfg, _ := a.config.Load(ctx)
	cfg["groups"] = []any{map[string]any{"id": "-1001811239876", "name": "Secret Riders Club", "enabled": true}}
	if err := a.config.Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	a.ixLog("picture index: open D:\\Telegram\\Secret Riders Club\\images\\holiday-2026.jpg: access denied for -1001811239876")

	dir := filepath.Join(a.dataDir, ixReportDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// 50 older hours' copies are there already.
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, ixTZ)
	for i := 0; i < 50; i++ {
		if err := os.WriteFile(filepath.Join(dir, base.Add(time.Duration(i)*time.Hour).Format("2006-01-02_15")+".txt"), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 6, 10, 5, 0, 0, ixTZ)
	if err := a.ixSaveReport(ctx, now); err != nil {
		t.Fatal(err)
	}
	latest, err := os.ReadFile(filepath.Join(dir, "latest.txt"))
	if err != nil {
		t.Fatal(err)
	}
	out := string(latest)
	if !strings.Contains(out, "Archive diagnostics") || !strings.Contains(out, "access denied") {
		t.Fatalf("latest.txt is not the report:\n%s", out)
	}
	for _, secret := range []string{"Secret Riders", "1811239876", "holiday-2026"} {
		if strings.Contains(out, secret) {
			t.Errorf("the saved report leaks %q", secret)
		}
	}
	hour := filepath.Join(dir, "2026-11-06_10.txt")
	if b, err := os.ReadFile(hour); err != nil || string(b) != out {
		t.Fatalf("the hour's copy: %v", err)
	}
	// A later save in the same hour updates latest.txt only.
	if err := os.WriteFile(hour, []byte("first of the hour"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.ixSaveReport(ctx, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(hour); string(b) != "first of the hour" {
		t.Fatal("the hour's copy was replaced")
	}
	entries, _ := os.ReadDir(dir)
	var hours []string
	for _, e := range entries {
		if e.Name() != "latest.txt" {
			hours = append(hours, e.Name())
		}
	}
	if len(hours) != ixReportHours {
		t.Fatalf("%d hourly copies kept, want %d: %v", len(hours), ixReportHours, hours)
	}
	if hours[0] != base.Add(3*time.Hour).Format("2006-01-02_15")+".txt" {
		t.Fatalf("the oldest copies should go first: %v", hours[:3])
	}
}
