package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/download"
)

// Duplicates by when they were found, counted per day: before downloading
// (the same Telegram file, or an exact re-upload confirmed on Telegram) or
// only after downloading (the same bytes were already saved: the
// downloaded copy is removed, but the bytes were spent, which counts
// against a volume-throttled account). Also what the checks on Telegram
// cost and how often Telegram could not answer them (ix_subst.go).

const ixSchemaDedup = `
CREATE TABLE IF NOT EXISTS ix_dedup_daily ( -- duplicates by how they were found, per day (ix_dedup.go)
	day   TEXT    NOT NULL, -- YYYY-MM-DD in the archive's time zone
	how   TEXT    NOT NULL,
	files INTEGER NOT NULL DEFAULT 0,
	bytes INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (day, how)
);
`

const (
	ixDedupTelegramFile = download.DedupTelegramFile  // bytes: not downloaded
	ixDedupReupload     = download.DedupReupload      // bytes: not downloaded
	ixDedupAfter        = download.DedupAfterDownload // bytes: downloaded for nothing
	ixDedupCheck        = "check"                     // files checked on Telegram; bytes: downloaded as samples
	ixDedupCheckNoHash  = "check-nohash"              // Telegram kept no part hashes
	ixDedupCheckFailed  = "check-failed"              // parts could not be read
)

func (a *App) ixCountDedup(ctx context.Context, how string, files, bytes int64) {
	day := time.Now().In(ixTZ).Format("2006-01-02")
	_, _ = a.db.Writer.ExecContext(ctx, `INSERT INTO ix_dedup_daily(day,how,files,bytes) VALUES(?,?,?,?)
		ON CONFLICT(day,how) DO UPDATE SET files=files+excluded.files, bytes=bytes+excluded.bytes`, day, how, files, bytes)
}

// ixNoteDedup counts a catalogued message that shares a saved file, links
// the message to that file (ix_links.go), and records an exact re-upload
// (ix_subst) once the library has catalogued it on the saved file.
func (a *App) ixNoteDedup(ctx context.Context, item download.Item, record download.Record) {
	a.ixNoteLink(ctx, item, record)
	if !record.Reused || record.Dedup == download.DedupAfterDownload {
		// Its bytes came from Telegram: an identity set to be downloaded
		// for real ("Fetch the real files") has been, and may be matched by
		// identity again.
		var blocked int
		if a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_subst_block WHERE identity=?`, item.Identity.Key()).Scan(&blocked) == nil && blocked > 0 {
			_, _ = a.db.Writer.ExecContext(context.WithoutCancel(ctx), `DELETE FROM ix_subst_block WHERE identity=?`, item.Identity.Key())
		}
	}
	if !record.Reused || record.Dedup == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	a.ixCountDedup(ctx, record.Dedup, 1, item.Identity.Size)
	if record.Dedup == download.DedupReupload {
		_, _ = a.db.Writer.ExecContext(ctx, `INSERT INTO ix_subst(group_id,message_id,identity,size,of_path,how,at) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(group_id,message_id) DO UPDATE SET identity=excluded.identity, size=excluded.size, of_path=excluded.of_path, how=excluded.how, at=excluded.at`,
			item.GroupID, item.MessageID, item.Identity.Key(), item.Identity.Size, record.Path, record.DedupHow, time.Now().UnixMilli())
	}
}

type ixDedupCount struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
	MB    int64 `json:"mb"`
}

type ixDedupDay struct {
	Day          string       `json:"day"`
	TelegramFile ixDedupCount `json:"telegramFile"`
	Reupload     ixDedupCount `json:"reupload"`
	After        ixDedupCount `json:"after"`
}

type ixDedupStatus struct {
	// The last 7 days.
	TelegramFile ixDedupCount `json:"telegramFile"` // before downloading: the same Telegram file
	Reupload     ixDedupCount `json:"reupload"`     // before downloading: exact re-upload check
	After        ixDedupCount `json:"after"`        // after downloading: same bytes (MB wasted)
	Checks       int64        `json:"checks"`       // files compared on Telegram
	SampledMB    int64        `json:"sampledMb"`    // downloaded as samples for that
	NoHashes     int64        `json:"noHashes"`     // Telegram kept no part hashes
	CheckFailed  int64        `json:"checkFailed"`  // parts could not be read
	Days         []ixDedupDay `json:"days"`         // newest first
	Since        string       `json:"since"`        // first day counted
}

func (a *App) ixDedupStatus(ctx context.Context) ixDedupStatus {
	now := time.Now().In(ixTZ)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, ixTZ).AddDate(0, 0, -6)
	st := ixDedupStatus{Days: []ixDedupDay{}}
	byDay := map[string]*ixDedupDay{}
	for i := 0; i < 7; i++ {
		st.Days = append(st.Days, ixDedupDay{Day: start.AddDate(0, 0, 6-i).Format("2006-01-02")})
	}
	for i := range st.Days {
		byDay[st.Days[i].Day] = &st.Days[i]
	}
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(MIN(day),'') FROM ix_dedup_daily`).Scan(&st.Since)
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT day, how, files, bytes FROM ix_dedup_daily WHERE day>=?`, start.Format("2006-01-02"))
	if err != nil {
		return st
	}
	defer rows.Close()
	add := func(c *ixDedupCount, files, bytes int64) {
		c.Files += files
		c.Bytes += bytes
		c.MB = c.Bytes >> 20
	}
	var sampled int64
	for rows.Next() {
		var day, how string
		var files, bytes int64
		if rows.Scan(&day, &how, &files, &bytes) != nil {
			continue
		}
		d := byDay[day]
		switch how {
		case ixDedupTelegramFile:
			add(&st.TelegramFile, files, bytes)
			if d != nil {
				add(&d.TelegramFile, files, bytes)
			}
		case ixDedupReupload:
			add(&st.Reupload, files, bytes)
			if d != nil {
				add(&d.Reupload, files, bytes)
			}
		case ixDedupAfter:
			add(&st.After, files, bytes)
			if d != nil {
				add(&d.After, files, bytes)
			}
		case ixDedupCheck:
			st.Checks += files
			sampled += bytes
			st.SampledMB = sampled >> 20
		case ixDedupCheckNoHash:
			st.NoHashes += files
		case ixDedupCheckFailed:
			st.CheckFailed += files
		}
	}
	return st
}

// ixDedupDiag: the diagnostics report's lines (numbers only).
func (a *App) ixDedupDiag(ctx context.Context) []string {
	s := a.ixDedupStatus(ctx)
	since := s.Since
	if since == "" {
		since = "not yet"
	}
	out := []string{fmt.Sprintf("duplicates, last 7 days (counted since %s): before download, same Telegram file %d (%d MB) · before download, exact re-upload check %d (%d MB) · after download, same bytes %d (%d MB downloaded, copies removed)",
		since, s.TelegramFile.Files, s.TelegramFile.MB, s.Reupload.Files, s.Reupload.MB, s.After.Files, s.After.MB)}
	out = append(out, fmt.Sprintf("  re-upload checks on Telegram %d (no part hashes %d, parts unreadable %d, %d MB sampled)", s.Checks, s.NoHashes, s.CheckFailed, s.SampledMB))
	var days []string
	for _, d := range s.Days {
		if d.TelegramFile.Files+d.Reupload.Files+d.After.Files == 0 {
			continue
		}
		days = append(days, fmt.Sprintf("%s %d/%d/%d (%d/%d/%d MB)", d.Day[5:], d.TelegramFile.Files, d.Reupload.Files, d.After.Files, d.TelegramFile.MB, d.Reupload.MB, d.After.MB))
	}
	if len(days) > 0 {
		out = append(out, "  per day, same file/re-upload/after download: "+strings.Join(days, " | "))
	}
	return out
}
