package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/backup"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/botnick/telegram-media-downloader/core-service/internal/version"
	"github.com/botnick/telegram-media-downloader/core-service/internal/ws"
)

// Nightly backup of the Telegram session(s) and the database, into a folder
// next to the index copy on the NAS ("<index folder> backups").
//
// The session files are copied exactly as they are, still encrypted. The key
// that opens them (secret.key in the app's data folder) is never copied
// there, so a restore also needs that key: keep a copy of the data folder
// somewhere safe as well. The database copy is a consistent snapshot taken
// while the app runs (VACUUM INTO, as the app's other backups do).
//
// Every copy is checked before it counts: sizes and SHA-256 hashes against
// the source, and SQLite's integrity check on the database copy itself. The
// last 7 nightly copies are kept; the copy made the day before Telegram's
// deletion date ("... final") is never rotated away.

const (
	ixBackupKey   = "ix_backup_last"
	ixBackupKeep  = 7
	ixBackupTries = 3 // a failed nightly backup is tried again an hour later, up to 3 times a day
)

// ixBackupStartDelay lets the app settle before the first check after a
// start (and keeps short-lived test apps from making backups).
var ixBackupStartDelay = 2 * time.Minute

var (
	ixBackupNameRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_\d{6}$`)
	ixBackupAnyRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_\d{6}( final)?$`)
)

// ixBackupFile is one file in a copy, with its path inside the copy.
type ixBackupFile struct {
	Path   string `json:"path"` // forward slashes: db.sqlite, sessions/native/<id>.enc
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type ixBackupStatus struct {
	Running     bool   `json:"running"`
	Reason      string `json:"reason,omitempty"` // nightly, final or manual
	StartedAt   int64  `json:"startedAt,omitempty"`
	FinishedAt  int64  `json:"finishedAt,omitempty"`
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	Bytes       int64  `json:"bytes,omitempty"` // the whole copy
	DBBytes     int64  `json:"dbBytes,omitempty"`
	Sessions    int    `json:"sessions,omitempty"` // session files copied
	Kept        int    `json:"kept,omitempty"`     // nightly copies in the folder
	Name        string `json:"name,omitempty"`     // the copy's folder name (its date and time)
	LastOKAt    int64  `json:"lastOkAt,omitempty"`
	LastOKBytes int64  `json:"lastOkBytes,omitempty"`
	// The nightly schedule: the day of the last nightly attempt, how many
	// attempts that day, when the last one started, and whether one worked.
	Day     string `json:"day,omitempty"`
	Tries   int    `json:"tries,omitempty"`
	TriedAt int64  `json:"triedAt,omitempty"`
	DayDone bool   `json:"dayDone,omitempty"`
}

var ixBackups sync.Map // *App -> *ixBackupBox

type ixBackupBox struct {
	mu     sync.Mutex
	st     ixBackupStatus
	loaded bool
}

func (a *App) ixBackupBox() *ixBackupBox {
	v, _ := ixBackups.LoadOrStore(a, &ixBackupBox{})
	box := v.(*ixBackupBox)
	box.mu.Lock()
	if !box.loaded {
		box.loaded = true
		var raw string
		if a.db.Reader.QueryRow(`SELECT value FROM kv WHERE key=?`, ixBackupKey).Scan(&raw) == nil {
			_ = json.Unmarshal([]byte(raw), &box.st)
			box.st.Running = false
		}
	}
	box.mu.Unlock()
	return box
}

func (a *App) ixBackupState() ixBackupStatus {
	box := a.ixBackupBox()
	box.mu.Lock()
	defer box.mu.Unlock()
	return box.st
}

// ixBackupUpdate changes the status and keeps it across restarts.
func (a *App) ixBackupUpdate(ctx context.Context, change func(*ixBackupStatus)) ixBackupStatus {
	box := a.ixBackupBox()
	box.mu.Lock()
	change(&box.st)
	st := box.st
	box.mu.Unlock()
	if data, err := json.Marshal(st); err == nil {
		_, _ = a.db.Writer.ExecContext(context.WithoutCancel(ctx), `INSERT INTO kv(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, ixBackupKey, string(data), time.Now().UnixMilli())
	}
	if a.hub != nil {
		a.hub.Broadcast(ws.Event{Type: "ix_backup", Flat: true, Payload: map[string]any{"running": st.Running, "ok": st.OK}, Roles: []string{"admin"}})
	}
	return st
}

// ixInside reports whether p is root or inside it.
func ixInside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ixBackupDir is the backups folder: next to the index copy, so both end up
// on the NAS ("X:\Telegram index" → "X:\Telegram index backups").
func (a *App) ixBackupDir(s ixSettings) (string, error) {
	index, err := a.ixExportDir(s)
	if err != nil {
		return "", err
	}
	parent, base := filepath.Dir(index), filepath.Base(index)
	if parent == index || base == "" || base == "." || base == string(filepath.Separator) {
		return "", errors.New("the index folder is the top of a drive or share; choose a folder for the index in the settings")
	}
	dir := filepath.Join(parent, base+" backups")
	if downloads, err := filepath.Abs(a.downloadsDir); err == nil && ixInside(downloads, dir) {
		return "", errors.New("the backups folder must not be inside the downloads folder")
	}
	return dir, nil
}

// ixBackup makes one checked copy now (reason: nightly, final or manual).
func (a *App) ixBackup(ctx context.Context, reason string, now time.Time) error {
	box := a.ixBackupBox()
	box.mu.Lock()
	if box.st.Running {
		box.mu.Unlock()
		return errors.New("a backup is already being made")
	}
	box.st.Running, box.st.Reason, box.st.StartedAt = true, reason, now.UnixMilli()
	box.mu.Unlock()
	res, err := a.ixBackupTo(ctx, reason, now)
	a.ixBackupUpdate(ctx, func(st *ixBackupStatus) {
		st.Running, st.FinishedAt, st.OK, st.Error = false, time.Now().UnixMilli(), err == nil, ""
		if reason == "nightly" {
			st.DayDone = err == nil
		}
		if err != nil {
			st.Error = err.Error()
			return
		}
		st.Bytes, st.DBBytes, st.Sessions, st.Kept, st.Name = res.bytes, res.dbBytes, res.sessions, res.kept, res.name
		st.LastOKAt, st.LastOKBytes = st.FinishedAt, res.bytes
	})
	return err
}

type ixBackupResult struct {
	name           string
	bytes, dbBytes int64
	sessions, kept int
}

func (a *App) ixBackupTo(ctx context.Context, reason string, now time.Time) (ixBackupResult, error) {
	var res ixBackupResult
	dir, err := a.ixBackupDir(a.ixLoadSettings(ctx))
	if err != nil {
		return res, err
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return res, fmt.Errorf("could not make the backups folder: %w", err)
	}
	res.name = now.Format("2006-01-02_150405")
	if reason == "final" {
		res.name += " final" // kept out of the nightly rotation
	}
	final := filepath.Join(dir, res.name)
	if _, err := os.Stat(final); err == nil {
		return res, errors.New("a copy made at this same second is already there")
	}
	work := final + ".partial"
	if err = os.RemoveAll(work); err != nil {
		return res, err
	}
	if err = os.MkdirAll(work, 0o755); err != nil {
		return res, err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(work)
		}
	}()
	var files []ixBackupFile

	// The database: a consistent snapshot in the data folder first (SQLite
	// is happiest on a local disk), then copied over and checked there.
	local := filepath.Join(a.dataDir, "backups", "archive-nightly")
	_ = os.RemoveAll(local)
	defer os.RemoveAll(local)
	snapCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	snap, err := backup.Snapshot(snapCtx, a.db.Reader, local, "archive")
	if err != nil {
		// A reading connection is enough for VACUUM INTO; fall back to the writer.
		if snap, err = backup.Snapshot(snapCtx, a.db.Writer, local, "archive"); err != nil {
			return res, fmt.Errorf("database snapshot: %w", err)
		}
	}
	dbCopy := filepath.Join(work, "db.sqlite")
	f, err := ixBackupCopy(snap.Path, dbCopy)
	if err != nil {
		return res, fmt.Errorf("database copy: %w", err)
	}
	if f.SHA256 != snap.SHA256 || f.Bytes != snap.Bytes {
		return res, errors.New("database copy: it does not match the snapshot")
	}
	if err = ixIntegrityCheck(snapCtx, dbCopy); err != nil {
		return res, fmt.Errorf("database copy: %w", err)
	}
	f.Path = "db.sqlite"
	files = append(files, f)
	res.dbBytes = f.Bytes
	_ = os.RemoveAll(local)

	// The session files, byte for byte (they stay encrypted).
	rels, err := a.ixSessionFiles()
	if err != nil {
		return res, err
	}
	for _, rel := range rels {
		f, err := ixBackupCopy(filepath.Join(a.dataDir, rel), filepath.Join(work, rel))
		if err != nil {
			return res, fmt.Errorf("session copy: %w", err)
		}
		f.Path = filepath.ToSlash(rel)
		files = append(files, f)
		res.sessions++
	}

	// What is in the copy, and how to put it back.
	manifest := map[string]any{
		"made":      now.UTC().Format(time.RFC3339),
		"reason":    reason,
		"app":       version.AppVersion,
		"files":     files,
		"integrity": "ok",
		"note":      "The session files are encrypted. Restoring them also needs secret.key from the app's data folder, which is never copied here.",
	}
	data, _ := json.MarshalIndent(manifest, "", "  ")
	for name, body := range map[string][]byte{"manifest.json": data, "README.txt": []byte(ixBackupReadme)} {
		if err = os.WriteFile(filepath.Join(work, name), body, 0o644); err != nil {
			return res, err
		}
	}
	for _, f := range files {
		res.bytes += f.Bytes
	}
	if err = os.Rename(work, final); err != nil {
		return res, fmt.Errorf("could not finish the copy: %w", err)
	}
	published = true
	res.kept = ixRotateBackups(dir, ixBackupKeep)
	return res, nil
}

const ixBackupReadme = `Backup of the Telegram archive app
==================================

db.sqlite   the app's database (messages, file list, settings, recognition)
sessions/   the Telegram session files, exactly as the app keeps them
session.enc (only on older installs) the first account's session
manifest.json  sizes and SHA-256 hashes of every file here

The session files are encrypted. The key that opens them is secret.key in
the app's data folder, and it is never copied here: keep a copy of the whole
data folder somewhere safe as well. Without that key the session files
cannot be used.

To restore: stop the app, put db.sqlite and the sessions folder (and
session.enc, if there is one) into the app's data folder, put secret.key
back next to them, and start the app. See docs/archive-after-deletion.md.
`

// ixSessionFiles lists the Telegram session files to copy, relative to the
// data folder: every saved account's native and imported session
// (NativePath, ImportPath), and anything else in the sessions folder (the
// legacy import marker). The secret key is never on the list.
func (a *App) ixSessionFiles() ([]string, error) {
	root, err := filepath.Abs(a.dataDir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" {
			return
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			return
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return
		}
		if strings.EqualFold(filepath.Base(rel), "secret.key") {
			return
		}
		seen[rel] = true
	}
	// SavedSessions refuses an ambiguous folder; its files are copied all the same.
	if saved, err := telegram.SavedSessions(root); err == nil {
		for _, s := range saved {
			add(s.NativePath)
			add(s.ImportPath)
		}
	}
	_ = filepath.WalkDir(filepath.Join(root, "sessions"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			add(p)
		}
		return nil
	})
	add(filepath.Join(root, "session.enc"))
	out := make([]string, 0, len(seen))
	for rel := range seen {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

// ixBackupCopy copies a file and checks the copy: same size and SHA-256 as
// the source, and the source unchanged while it was copied (a session file
// can be rewritten by the running app; then it is copied again).
func ixBackupCopy(src, dst string) (ixBackupFile, error) {
	var last error
	for try := 0; try < 3; try++ {
		f, err := ixBackupCopyOnce(src, dst)
		if err == nil {
			return f, nil
		}
		last = err
	}
	return ixBackupFile{}, last
}

func ixBackupCopyOnce(src, dst string) (ixBackupFile, error) {
	in, err := os.Open(src)
	if err != nil {
		return ixBackupFile{}, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return ixBackupFile{}, err
	}
	if !info.Mode().IsRegular() {
		return ixBackupFile{}, fmt.Errorf("%s is not a regular file", filepath.Base(src))
	}
	if err = os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return ixBackupFile{}, err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return ixBackupFile{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return ixBackupFile{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	gotSum, gotN, err := ixHashFile(dst)
	if err != nil {
		return ixBackupFile{}, err
	}
	if gotN != n || gotSum != sum {
		return ixBackupFile{}, fmt.Errorf("the copy of %s reads back differently", filepath.Base(src))
	}
	srcSum, srcN, err := ixHashFile(src)
	if err != nil {
		return ixBackupFile{}, err
	}
	if srcN != n || srcSum != sum {
		return ixBackupFile{}, fmt.Errorf("%s changed while it was copied", filepath.Base(src))
	}
	return ixBackupFile{Bytes: n, SHA256: sum}, nil
}

// ixSQLiteURI is a read-only SQLite address for a file, local or on a share
// (file:///X:/a, file:////nas/share/a, file:///home/a).
func ixSQLiteURI(path, query string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + (&url.URL{Path: p}).EscapedPath() + "?" + query
}

// ixIntegrityCheck runs SQLite's full integrity check on a database copy,
// opened read-only and without locks (it may be on a network share).
func ixIntegrityCheck(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", ixSQLiteURI(path, "mode=ro&immutable=1"))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return fmt.Errorf("integrity check could not run: %w", err)
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		results = append(results, line)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(results) != 1 || results[0] != "ok" {
		return fmt.Errorf("integrity check failed: %s", strings.Join(results[:min(len(results), 3)], "; "))
	}
	return nil
}

// ixRotateBackups keeps the newest nightly copies and removes the older
// ones, and anything left half-made by an interrupted backup. Only folders
// this app made (named by date and time, with a manifest) are touched.
func ixRotateBackups(dir string, keep int) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var nightly []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			continue
		}
		if strings.HasSuffix(name, ".partial") && ixBackupAnyRe.MatchString(strings.TrimSuffix(name, ".partial")) {
			_ = os.RemoveAll(filepath.Join(dir, name))
			continue
		}
		if ixBackupNameRe.MatchString(name) {
			if _, err := os.Stat(filepath.Join(dir, name, "manifest.json")); err == nil {
				nightly = append(nightly, name)
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(nightly)))
	for _, old := range nightly[min(len(nightly), keep):] {
		_ = os.RemoveAll(filepath.Join(dir, old))
	}
	return min(len(nightly), keep)
}

// ---------- schedule ----------

// ixRunBackups makes the nightly backup a minute after the index copy's
// time (after the index copy itself, when that is running). A backup missed
// while the app was off is made at the next start.
func (a *App) ixRunBackups(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(ixBackupStartDelay):
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if ixVdupBackground.Load() {
			a.ixBackupTick(ctx, time.Now())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) ixBackupTick(ctx context.Context, now time.Time) {
	s := a.ixLoadSettings(ctx)
	if s.BackupOff || now.Format("15:04") <= s.ExportTime || a.ixExportStatus().Running {
		return
	}
	st := a.ixBackupState()
	day := now.Format("2006-01-02")
	if st.Running || (st.Day == day && (st.DayDone || st.Tries >= ixBackupTries || now.UnixMilli()-st.TriedAt < time.Hour.Milliseconds())) {
		return
	}
	a.ixBackupUpdate(ctx, func(st *ixBackupStatus) {
		if st.Day != day {
			st.Day, st.Tries, st.DayDone = day, 0, false
		}
		st.Tries++
		st.TriedAt = now.UnixMilli()
	})
	if err := a.ixBackup(ctx, "nightly", now); err != nil && ctx.Err() == nil {
		a.ixLog("nightly backup: %v", err)
	}
}

// ---------- status, report, routes ----------

// ixBackupOverview is what the Overview and Settings show.
func (a *App) ixBackupOverview(ctx context.Context) map[string]any {
	s := a.ixLoadSettings(ctx)
	out := map[string]any{"enabled": !s.BackupOff, "status": a.ixBackupState(), "keep": ixBackupKeep}
	if dir, err := a.ixBackupDir(s); err == nil {
		out["dir"] = dir
	} else {
		out["dirError"] = err.Error()
	}
	if data, err := filepath.Abs(a.dataDir); err == nil {
		out["dataDir"] = data
	}
	return out
}

// ixBackupDiag is the report's line: numbers and times only.
func (a *App) ixBackupDiag(ctx context.Context, now time.Time, clean func(string) string) string {
	s := a.ixLoadSettings(ctx)
	st := a.ixBackupState()
	on := map[bool]string{true: "on", false: "off"}[!s.BackupOff]
	result := "not made yet"
	if st.FinishedAt > 0 {
		result = map[bool]string{true: "ok", false: "FAILED: " + clean(st.Error)}[st.OK]
	}
	line := fmt.Sprintf("backup %s · last %s (%s, %s) · last good %s, %d MB (database %d MB, %d session file(s)) · %d nightly copies kept · tries today %d",
		on, ixAgo(st.FinishedAt, now), st.Reason, result, ixAgo(st.LastOKAt, now), st.LastOKBytes>>20, st.DBBytes>>20, st.Sessions, st.Kept, map[bool]int{true: st.Tries, false: 0}[st.Day == now.Format("2006-01-02")])
	if st.Running {
		line += " · running now"
	}
	return line
}

// ixParseBackupSettings reads the backup switch from a settings change.
func ixParseBackupSettings(body map[string]any, s ixSettings) ixSettings {
	if v, ok := body["backupEnabled"].(bool); ok {
		s.BackupOff = !v
	}
	return s
}

func registerIxBackupRoutes(mux *http.ServeMux, a *App) {
	mux.Handle("POST /api/ix/backup", a.requireAdmin(http.HandlerFunc(a.handleIxBackup)))
}

func (a *App) handleIxBackup(w http.ResponseWriter, r *http.Request) {
	if a.ixBackupState().Running {
		writeJSONError(w, http.StatusConflict, "A backup is already being made")
		return
	}
	if !a.launchMaintenance(func() {
		if err := a.ixBackup(a.ctx, "manual", time.Now()); err != nil && a.ctx.Err() == nil {
			a.ixLog("backup: %v", err)
		}
	}) {
		writeJSONError(w, http.StatusServiceUnavailable, "Server is shutting down")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"success": true})
}
