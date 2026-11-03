package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Fixity: every stored file is read again and its SHA-256 compared with the
// one recorded when it was saved, a slice of the library every few minutes
// so that the whole library is checked about once a month (the NDSA levels
// of digital preservation ask for regular checks with their outcomes kept).
// A changed or missing file raises an alert. The same pass compares each
// file's size with the size Telegram gave for it.

const (
	ixFixityCycle = 30 * 24 * time.Hour // the whole library is read once per cycle
	ixFixityTick  = 10 * time.Minute
)

type ixFixityStatus struct {
	Files     int64 `json:"files"`
	Checked   int64 `json:"checked"` // within the current cycle
	Changed   int64 `json:"changed"`
	Missing   int64 `json:"missing"`
	SizeOff   int64 `json:"sizeOff"`
	LastCheck int64 `json:"lastCheck,omitempty"`
}

func (a *App) ixRunFixity(ctx context.Context) {
	wait := 5 * time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = ixFixityTick
		if !ixVdupBackground.Load() {
			continue
		}
		s := a.ixLoadSettings(ctx)
		if !s.FixityEnabled || ixInQuietHours(ixSettings{QuietStart: s.DupQuietStart, QuietEnd: s.DupQuietEnd}, time.Now()) {
			continue
		}
		if _, err := a.ixFixityStep(ctx, 0); err != nil && ctx.Err() == nil {
			a.ixLog("integrity check: %v", err)
		}
	}
}

// ixFixityStep checks the files whose last check is oldest; batch 0 sizes
// it so the library is covered once per cycle.
func (a *App) ixFixityStep(ctx context.Context, batch int) (int, error) {
	if _, err := os.Stat(a.downloadsDir); err != nil {
		return 0, nil // the NAS is offline
	}
	if batch <= 0 {
		var total int64
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(DISTINCT file_path) FROM downloads WHERE file_path IS NOT NULL AND file_path<>'' AND (status IS NULL OR status='completed')`).Scan(&total)
		batch = int(max(20, total*int64(ixFixityTick)/int64(ixFixityCycle)+1))
	}
	type item struct {
		path, hash string
		tgSize     int64
		substitute bool
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT d.file_path, COALESCE(MAX(lower(d.file_hash)),''),
			COALESCE(MAX(CASE WHEN s.group_id IS NULL THEN d.telegram_media_size END),0), MAX(s.group_id IS NOT NULL)
		FROM downloads d LEFT JOIN ix_fixity f ON f.path=d.file_path
		LEFT JOIN ix_subst s ON s.group_id=d.group_id AND s.message_id=d.message_id
		WHERE d.file_path IS NOT NULL AND d.file_path<>'' AND (d.status IS NULL OR d.status='completed')
		GROUP BY d.file_path ORDER BY COALESCE(MAX(f.checked_at),0), d.file_path LIMIT ?`, batch)
	if err != nil {
		return 0, err
	}
	var items []item
	for rows.Next() {
		var it item
		var sub int
		if rows.Scan(&it.path, &it.hash, &it.tgSize, &sub) == nil {
			it.substitute = sub != 0
			items = append(items, it)
		}
	}
	rows.Close()
	done := 0
	for _, it := range items {
		if ctx.Err() != nil {
			return done, ctx.Err()
		}
		result, detail, size, sum := a.ixFixityCheck(ctx, it.path, it.hash, it.tgSize)
		if result == "" {
			return done, nil // the NAS went away mid-run
		}
		now := time.Now().UnixMilli()
		var prev string
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT result FROM ix_fixity WHERE path=?`, it.path).Scan(&prev)
		if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_fixity(path,size,sha256,checked_at,result) VALUES(?,?,?,?,?)
			ON CONFLICT(path) DO UPDATE SET size=excluded.size, sha256=excluded.sha256, checked_at=excluded.checked_at, result=excluded.result`, it.path, size, sum, now, result); err != nil {
			return done, err
		}
		if result != "ok" || (prev != "" && prev != "ok") {
			if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_fixity_events(at,path,result,detail) VALUES(?,?,?,?)`, now, it.path, result, detail); err != nil {
				return done, err
			}
		}
		if it.hash == "" && sum != "" && result == "ok" {
			_, _ = a.db.Writer.ExecContext(ctx, `UPDATE downloads SET file_hash=? WHERE file_path=? AND (file_hash IS NULL OR file_hash='')`, sum, it.path)
		}
		done++
	}
	return done, nil
}

// ixFixityCheck reads one file: ok, changed, missing, size or unreadable
// ("" when the downloads folder itself is unreachable).
func (a *App) ixFixityCheck(ctx context.Context, stored, want string, tgSize int64) (result, detail string, size int64, sum string) {
	rel := ixNormPath(stored)
	if rel == "" {
		return "unreadable", "bad path", 0, ""
	}
	f, err := os.Open(filepath.Join(a.downloadsDir, filepath.FromSlash(rel)))
	if err != nil {
		if _, rootErr := os.Stat(a.downloadsDir); rootErr != nil {
			return "", "", 0, ""
		}
		if errors.Is(err, os.ErrNotExist) {
			return "missing", "", 0, ""
		}
		return "unreadable", err.Error(), 0, ""
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		if ctx.Err() != nil {
			return "", "", 0, ""
		}
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			size += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "unreadable", err.Error(), size, ""
		}
	}
	sum = hex.EncodeToString(h.Sum(nil))
	switch {
	case want != "" && !strings.EqualFold(want, sum):
		return "changed", "recorded " + want + ", now " + sum, size, sum
	case tgSize > 0 && size != tgSize:
		return "size", fmt.Sprintf("Telegram size %d, file %d", tgSize, size), size, sum
	}
	return "ok", "", size, sum
}

func (a *App) ixFixityStatus(ctx context.Context) ixFixityStatus {
	var st ixFixityStatus
	since := time.Now().Add(-ixFixityCycle).UnixMilli()
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(DISTINCT file_path) FROM downloads WHERE file_path IS NOT NULL AND file_path<>'' AND (status IS NULL OR status='completed')`).Scan(&st.Files)
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(SUM(checked_at>=?),0), COALESCE(SUM(result='changed'),0), COALESCE(SUM(result='missing'),0), COALESCE(SUM(result='size'),0), COALESCE(MAX(checked_at),0) FROM ix_fixity`, since).
		Scan(&st.Checked, &st.Changed, &st.Missing, &st.SizeOff, &st.LastCheck)
	return st
}

// ixFixityProblems raises an alert, once a day, when stored files changed
// or disappeared.
func (a *App) ixFixityProblems(ctx context.Context, now time.Time) []ixAlert {
	var changed, missing int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(SUM(result='changed'),0), COALESCE(SUM(result='missing'),0) FROM ix_fixity`).Scan(&changed, &missing)
	if changed+missing == 0 {
		return nil
	}
	return []ixAlert{{Key: "fixity:" + now.In(ixTZ).Format("2006-01-02"), Once: true, Title: "Stored files changed or missing",
		Message: fmt.Sprintf("The integrity check found %d file(s) whose contents changed and %d missing (Archive → Overview). Restore them from a backup copy.", changed, missing)}}
}
