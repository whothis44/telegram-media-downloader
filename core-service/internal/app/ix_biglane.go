package app

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// Big files (1 GB or more) and the queue.
//
// A file that big can take hours under Telegram's limits, and all files of
// an account share the download pacer's window. Three rules keep them from
// choking everything else, queues of earlier versions included (their big
// files are ordinary pending rows):
//
//   - the big-file lane (engine.BigFileLane, the setting "bigAtOnce"): at
//     most 2 of them download at once, twice that when nothing smaller can
//     be claimed;
//   - those waiting for the lane do not count towards a backfill's queue
//     limit (ixWaitingApart), up to twice that limit, so the backfill keeps
//     bringing smaller files and still cannot queue big ones without end;
//   - with the deadline order on, files the rescans have not queued yet
//     count as work of the smallest band (ixUnqueuedBand), so the big files
//     queued are set aside rather than filling the queue limit.

// ixApplyEngineSettings passes the archive settings the engine reads to it.
func ixApplyEngineSettings(s ixSettings) {
	telegram.SetPacing(s.DownloadPacing)
	engine.SetBigFileLane(s.BigAtOnce)
}

// ixParseBigSettings reads the big-file lane setting: a number from 0 (no
// limit) to 50, or empty for the default.
func ixParseBigSettings(body map[string]any, s *ixSettings) error {
	switch v := body["bigAtOnce"].(type) {
	case float64:
		s.BigAtOnce = int(v)
	case string:
		if v = strings.TrimSpace(v); v == "" {
			s.BigAtOnce = engine.DefaultBigFileLane
		} else if n, err := strconv.Atoi(v); err == nil {
			s.BigAtOnce = n
		} else {
			return errors.New("the number of big files at once must be a number")
		}
	}
	if s.BigAtOnce < 0 || s.BigAtOnce > 50 {
		return errors.New("the number of big files at once must be between 0 (no limit) and 50")
	}
	return nil
}

// ixBigFiles is what the Downloads card and the diagnostics report show.
type ixBigFiles struct {
	MinBytes    int64 `json:"minBytes"`    // what counts as big
	Limit       int   `json:"limit"`       // at most this many at once (0: no limit)
	Downloading int   `json:"downloading"` // big files downloading now
	Waiting     int   `json:"waiting"`     // waiting for the lane
	SetAside    int   `json:"setAside"`    // set aside by the deadline order
	Held        int   `json:"held"`        // held as probable copies, or originals after a lower quality
	// The backfills that queue files (a rescan's files pass, or one
	// started on the dashboard) wait while QueueCount, the queue items that
	// count towards their limit, is at QueueCap or more.
	Backfills  int  `json:"backfills"`
	QueueCount int  `json:"queueCount"`
	QueueCap   int  `json:"queueCap"`
	CapPaused  bool `json:"capPaused"`
}

func (a *App) ixBigFilesNow(ctx context.Context) ixBigFiles {
	b := ixBigFiles{MinBytes: engine.BigFileSize, Limit: engine.BigFileLane()}
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(SUM(st='run'),0), COALESCE(SUM(st='wait'),0), COALESCE(SUM(st='aside'),0), COALESCE(SUM(st='held'),0) FROM (
		SELECT CASE WHEN w.status='processing' THEN 'run'
			WHEN EXISTS (SELECT 1 FROM ix_held h WHERE h.work_id=w.id AND h.released_at=0 AND h.reason<>'')
				OR EXISTS (SELECT 1 FROM ix_lowq q WHERE q.work_id=w.id AND q.state='saved') THEN 'held'
			WHEN EXISTS (SELECT 1 FROM ix_deferred x WHERE x.work_id=w.id AND x.released_at=0) THEN 'aside'
			WHEN w.paused=0 THEN 'wait' ELSE '' END AS st
		FROM tgdl_work w WHERE w.status IN ('pending','processing') AND w.file_size>=?)`, engine.BigFileSize).
		Scan(&b.Downloading, &b.Waiting, &b.SetAside, &b.Held)
	a.historyMu.Lock()
	for _, j := range a.historyJobs {
		if j.State == "running" && !j.MessagesOnly && !j.Cancelled {
			b.Backfills++
		}
	}
	a.historyMu.Unlock()
	b.QueueCap = 500
	if cfg, err := a.config.Load(ctx); err == nil {
		b.QueueCap = historyBackpressureCap(cfg)
	}
	// As the backfill counts (history_runner.go): every queued item not of
	// live monitoring, less those that wait apart.
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM tgdl_work WHERE origin<>'live' AND status IN ('pending','processing')`).Scan(&b.QueueCount)
	b.QueueCount = max(0, b.QueueCount-a.ixWaitingApart(ctx, b.QueueCap))
	b.CapPaused = b.Backfills > 0 && b.QueueCount >= b.QueueCap
	return b
}
