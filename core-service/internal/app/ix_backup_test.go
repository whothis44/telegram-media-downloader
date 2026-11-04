package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ixBackupFixture makes an archive with invented session files and a secret.
func ixBackupFixture(t *testing.T) (*App, context.Context, string) {
	t.Helper()
	nas := filepath.Join(t.TempDir(), "nas")
	a, ctx := ix8App(t, map[string]any{"exportDir": filepath.Join(nas, "index")})
	ixSeedIndex(t, a)
	for rel, body := range map[string]string{
		"sessions/one.enc":        "imported session, invented bytes",
		"sessions/native/one.enc": "native session \x00\x01\x02 invented",
		"secret.key":              "invented-secret-key",
	} {
		p := filepath.Join(a.dataDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return a, ctx, filepath.Join(nas, "index backups")
}

func ixBackupNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestIxBackupCopiesSessionsAndDatabaseAndKeepsSeven(t *testing.T) {
	a, ctx, dir := ixBackupFixture(t)
	t0 := time.Date(2026, 11, 3, 3, 31, 0, 0, time.Local)
	if err := a.ixBackup(ctx, "nightly", t0); err != nil {
		t.Fatal(err)
	}
	copyDir := filepath.Join(dir, "2026-11-03_033100")
	// The sessions are copied byte for byte; the secret never is.
	for _, rel := range []string{"sessions/one.enc", "sessions/native/one.enc"} {
		want, _ := os.ReadFile(filepath.Join(a.dataDir, filepath.FromSlash(rel)))
		got, err := os.ReadFile(filepath.Join(copyDir, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %v", rel, err)
		}
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.EqualFold(d.Name(), "secret.key") {
			t.Fatalf("the secret was copied: %s", p)
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("invented-secret-key")) {
			t.Fatalf("the secret's text is in %s", p)
		}
		return nil
	})
	// The database copy is whole and holds the archive.
	if err := ixIntegrityCheck(ctx, filepath.Join(copyDir, "db.sqlite")); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Files []ixBackupFile `json:"files"`
	}
	raw, err := os.ReadFile(filepath.Join(copyDir, "manifest.json"))
	if err != nil || json.Unmarshal(raw, &manifest) != nil || len(manifest.Files) != 3 {
		t.Fatalf("manifest: %v %s", err, raw)
	}
	for _, f := range manifest.Files {
		sum, n, err := ixHashFile(filepath.Join(copyDir, filepath.FromSlash(f.Path)))
		if err != nil || sum != f.SHA256 || n != f.Bytes {
			t.Fatalf("%s: hash %s/%s size %d/%d %v", f.Path, sum, f.SHA256, n, f.Bytes, err)
		}
	}
	st := a.ixBackupState()
	if !st.OK || st.Sessions != 2 || st.DBBytes == 0 || st.Bytes <= st.DBBytes || st.LastOKAt == 0 || st.Kept != 1 {
		t.Fatalf("status: %+v", st)
	}
	// Eight more nights, a final copy and a half-made one: seven nightly
	// copies stay, the final one stays, the half-made one goes.
	if err := os.MkdirAll(filepath.Join(dir, "2026-10-01_033100.partial"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.ixBackup(ctx, "final", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 8; i++ {
		if err := a.ixBackup(ctx, "nightly", t0.Add(time.Duration(i)*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	names := ixBackupNames(t, dir)
	finals, nightly := 0, 0
	for _, n := range names {
		switch {
		case strings.HasSuffix(n, " final"):
			finals++
		case ixBackupNameRe.MatchString(n):
			nightly++
		default:
			t.Fatalf("left over: %s", n)
		}
	}
	if nightly != 7 || finals != 1 || a.ixBackupState().Kept != 7 {
		t.Fatalf("kept %v", names)
	}
	for _, n := range names {
		if n == "2026-11-03_033100" || n == "2026-11-04_033100" {
			t.Fatalf("the oldest copies should be gone: %v", names)
		}
	}
	// The report has numbers and times, and no folder names.
	report := a.ixDiagnostics(ctx)
	if !strings.Contains(report, "backup on") || !strings.Contains(report, "7 nightly copies kept") || strings.Contains(report, dir) || strings.Contains(report, a.dataDir) {
		t.Fatalf("report: %s", report)
	}
	code, out := ixCall(ctx, a.handleIxStatus, http.MethodGet, "/api/ix/status", nil, nil)
	b, _ := out["backup"].(map[string]any)
	if code != 200 || b["enabled"] != true || b["dir"] != dir {
		t.Fatalf("status: %d %v", code, out["backup"])
	}
}

func TestIxBackupFailuresAreReportedAndChecked(t *testing.T) {
	a, ctx, _ := ixBackupFixture(t)
	if err := a.ixBackup(ctx, "nightly", time.Now()); err != nil {
		t.Fatal(err)
	}
	good := a.ixBackupState()
	// A folder inside the downloads folder is refused.
	s := a.ixLoadSettings(ctx)
	s.ExportDir = filepath.Join(a.downloadsDir, "index")
	if err := a.ixSaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := a.ixBackup(ctx, "manual", time.Now()); err == nil {
		t.Fatal("a backup inside the downloads folder should fail")
	}
	st := a.ixBackupState()
	if st.OK || st.Error == "" || st.LastOKAt != good.LastOKAt {
		t.Fatalf("failure: %+v", st)
	}
	if line := a.ixBackupDiag(ctx, time.Now(), func(s string) string { return "<cleaned>" }); !strings.Contains(line, "FAILED: <cleaned>") {
		t.Fatal(line)
	}
	// A damaged database copy fails the integrity check.
	bad := filepath.Join(t.TempDir(), "db.sqlite")
	if err := os.WriteFile(bad, []byte("not a database at all, invented"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ixIntegrityCheck(ctx, bad); err == nil {
		t.Fatal("a damaged copy should fail")
	}
	for in, want := range map[string]string{
		"/home/x/db copy.sqlite": "file:///home/x/db%20copy.sqlite?mode=ro",
	} {
		if got := ixSQLiteURI(in, "mode=ro"); got != want {
			t.Fatalf("%s: %s", in, got)
		}
	}
	// The switch is on by default and can be turned off.
	if ixDefaultSettings().BackupOff {
		t.Fatal("backups should be on by default")
	}
	off, err := ixParseSettings(map[string]any{"backupEnabled": false}, ixDefaultSettings())
	if err != nil || !off.BackupOff {
		t.Fatal("switching off")
	}
}

func TestIxBackupRunsNightlyAfterTheIndexCopy(t *testing.T) {
	a, ctx, dir := ixBackupFixture(t)
	day := time.Date(2026, 11, 5, 0, 0, 0, 0, time.Local)
	at := func(clock string) time.Time {
		tm, _ := time.ParseInLocation("15:04", clock, time.Local)
		return day.Add(time.Duration(tm.Hour())*time.Hour + time.Duration(tm.Minute())*time.Minute)
	}
	count := func() int {
		entries, _ := os.ReadDir(dir)
		return len(entries)
	}
	a.ixBackupTick(ctx, at("03:30")) // the index copy's minute: not yet
	if count() != 0 {
		t.Fatal("too early")
	}
	a.ixBackupTick(ctx, at("03:31"))
	a.ixBackupTick(ctx, at("09:00")) // once a day
	if count() != 1 || !a.ixBackupState().DayDone {
		t.Fatalf("copies %d, %+v", count(), a.ixBackupState())
	}
	day = day.Add(24 * time.Hour)
	a.ixBackupTick(ctx, at("03:31"))
	if count() != 2 {
		t.Fatalf("next night: %d", count())
	}
	// Turned off: nothing.
	day = day.Add(24 * time.Hour)
	s := a.ixLoadSettings(ctx)
	s.BackupOff = true
	_ = a.ixSaveSettings(ctx, s)
	a.ixBackupTick(ctx, at("03:31"))
	if count() != 2 {
		t.Fatal("made while turned off")
	}
	// A failing night is tried again an hour later, three times at most.
	s.BackupOff, s.ExportDir = false, filepath.Join(a.downloadsDir, "index")
	_ = a.ixSaveSettings(ctx, s)
	for _, clock := range []string{"03:31", "03:50", "04:32", "05:33", "06:34", "07:35"} {
		a.ixBackupTick(ctx, at(clock))
	}
	if st := a.ixBackupState(); st.Tries != 3 || st.OK || st.DayDone {
		t.Fatalf("retries: %+v", st)
	}
}

// SQLite's integrity check runs on the local snapshot in the data folder,
// never on the copy on the NAS (which is checked by its hash); a snapshot
// that fails the check makes no copy; a network path is refused outright.
func TestIxBackupChecksTheLocalSnapshotNotTheShare(t *testing.T) {
	a, ctx, dir := ixBackupFixture(t)
	old := ixIntegrityCheckHook
	t.Cleanup(func() { ixIntegrityCheckHook = old })
	var checked []string
	ixIntegrityCheckHook = func(ctx context.Context, path string) error {
		checked = append(checked, path)
		return ixIntegrityCheck(ctx, path)
	}
	if err := a.ixBackup(ctx, "manual", time.Date(2026, 11, 3, 4, 0, 0, 0, time.Local)); err != nil {
		t.Fatal(err)
	}
	if len(checked) != 1 || !ixInside(a.dataDir, checked[0]) || ixInside(dir, checked[0]) {
		t.Fatalf("checked %v (data folder %s, backups %s)", checked, a.dataDir, dir)
	}
	var manifest struct {
		Files []ixBackupFile `json:"files"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "2026-11-03_040000", "manifest.json"))
	if err != nil || json.Unmarshal(raw, &manifest) != nil {
		t.Fatal(err)
	}
	if sum, n, err := ixHashFile(filepath.Join(dir, "2026-11-03_040000", "db.sqlite")); err != nil || manifest.Files[0].Path != "db.sqlite" || sum != manifest.Files[0].SHA256 || n != manifest.Files[0].Bytes {
		t.Fatalf("the copy does not match its checked hash: %v", err)
	}
	// A snapshot that fails the check: no copy, the failure reported.
	ixIntegrityCheckHook = func(context.Context, string) error { return errors.New("integrity check failed: invented damage") }
	if err := a.ixBackup(ctx, "manual", time.Date(2026, 11, 3, 5, 0, 0, 0, time.Local)); err == nil || !strings.Contains(err.Error(), "invented damage") {
		t.Fatalf("a damaged snapshot: %v", err)
	}
	for _, name := range ixBackupNames(t, dir) {
		if strings.HasPrefix(name, "2026-11-03_0500") {
			t.Fatalf("a copy was left: %s", name)
		}
	}
	for _, p := range []string{`\\nas\share\index backups\db.sqlite`, `//nas/share/db.sqlite`, `\\?\UNC\nas\share\db.sqlite`} {
		if !ixNetworkPath(p) {
			t.Fatalf("%s is a network path", p)
		}
		if err := ixIntegrityCheck(ctx, p); err == nil || !strings.Contains(err.Error(), "network share") {
			t.Fatalf("%s: %v", p, err)
		}
	}
	for _, p := range []string{`C:\data\db.sqlite`, `\\?\C:\data\db.sqlite`, `/home/x/db.sqlite`, `X:\index\db.sqlite`} {
		if ixNetworkPath(p) {
			t.Fatalf("%s is not a network path", p)
		}
	}
}
