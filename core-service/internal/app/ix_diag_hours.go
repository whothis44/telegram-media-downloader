package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// The report's hour-by-hour download record: enough to tell which limit the
// account meets (Telegram's waits, its speed limit for accounts without
// Premium, slow answers, or nothing on Telegram's side) before changing
// anything. Numbers and categories only: no chat, file or error text.

// ixErrKind sorts an error message into a category.
func ixErrKind(e string) string {
	switch {
	case strings.Contains(e, "FLOOD_PREMIUM_WAIT"):
		return "speed limit"
	case strings.Contains(e, "FLOOD_WAIT"):
		return "wait"
	case strings.Contains(e, "FILE_REFERENCE"):
		return "file reference"
	case strings.Contains(e, "did not answer") || strings.Contains(e, "deadline exceeded") || strings.Contains(e, "timeout"):
		return "timeout"
	case strings.Contains(e, "forcibly closed") || strings.Contains(e, "connection reset") || strings.Contains(e, "EOF") || strings.Contains(e, "broken pipe"):
		return "connection"
	case strings.Contains(e, "write ") || strings.Contains(e, "network error") || strings.Contains(e, "no space") || strings.Contains(e, "not enough space"):
		return "disk or NAS"
	case strings.Contains(e, "context canceled"):
		return "stopped"
	}
	return "other"
}

type ixHourRow struct {
	files  int
	bytes  int64
	errs   map[string]int
	pace   telegram.PaceHour
	paced  bool
	accts  int
	winSum float64
}

// ixDiagHours lists the last 24 hours, newest first, and a summary.
func (a *App) ixDiagHours(ctx context.Context, now time.Time) []string {
	from := now.Add(-24 * time.Hour).Truncate(time.Hour)
	rows := map[int64]*ixHourRow{}
	row := func(start int64) *ixHourRow {
		if rows[start] == nil {
			rows[start] = &ixHourRow{errs: map[string]int{}}
		}
		return rows[start]
	}
	if q, err := a.db.Reader.QueryContext(ctx, `SELECT w.updated_at/3600000*3600, COUNT(*), COALESCE(SUM(w.file_size),0) FROM tgdl_work w
		WHERE w.status='completed' AND w.updated_at>=?
		AND NOT `+ixSkippedBefore+`
		GROUP BY 1`, from.UnixMilli()); err == nil {
		for q.Next() {
			var start int64
			var n int
			var b int64
			if q.Scan(&start, &n, &b) == nil {
				r := row(start)
				r.files, r.bytes = n, b
			}
		}
		q.Close()
	}
	if q, err := a.db.Reader.QueryContext(ctx, `SELECT updated_at/3600000*3600, error FROM tgdl_work WHERE error IS NOT NULL AND error<>'' AND updated_at>=?`, from.UnixMilli()); err == nil {
		for q.Next() {
			var start int64
			var e string
			if q.Scan(&start, &e) == nil {
				row(start).errs[ixErrKind(e)]++
			}
		}
		q.Close()
	}
	if a.monitor != nil {
		for _, s := range a.monitor.Pace() {
			for _, h := range s.Hours {
				if h.Start < from.Unix() {
					continue
				}
				r := row(h.Start)
				r.paced = true
				r.accts++
				r.pace.Bytes += h.Bytes
				r.pace.Parts += h.Parts
				r.pace.Waits += h.Waits
				r.pace.Premium += h.Premium
				r.pace.Seconds = max(r.pace.Seconds, h.Seconds)
				r.winSum += h.Window
				// Seconds per part and the full share: the slowest account's.
				r.pace.PartSecs = max(r.pace.PartSecs, h.PartSecs)
				r.pace.FullShare = max(r.pace.FullShare, h.FullShare)
			}
		}
	}
	starts := make([]int64, 0, len(rows))
	for s := range rows {
		starts = append(starts, s)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] > starts[j] })
	var out []string
	var tot ixHourRow
	tot.errs = map[string]int{}
	var fullSum, partSum float64
	var pacedHours, partHours int
	for _, s := range starts {
		r := rows[s]
		tot.files += r.files
		tot.bytes += r.bytes
		tot.pace.Waits += r.pace.Waits
		tot.pace.Premium += r.pace.Premium
		for k, v := range r.errs {
			tot.errs[k] += v
		}
		line := fmt.Sprintf("  %s  %5d MB %4d files", time.Unix(s, 0).In(ixTZ).Format("15:04"), r.bytes>>20, r.files)
		if r.paced {
			pacedHours++
			fullSum += r.pace.FullShare
			if r.pace.PartSecs > 0 {
				partSum += r.pace.PartSecs
				partHours++
			}
			line += fmt.Sprintf(" | waits %d, speed limit %d | %.1f s a part, %.1f parts allowed, full %d%% of the time", r.pace.Waits, r.pace.Premium, r.pace.PartSecs, r.winSum, int(r.pace.FullShare*100+0.5))
		}
		if len(r.errs) > 0 {
			line += " | errors: " + ixKindList(r.errs)
		}
		out = append(out, line)
	}
	if len(starts) == 0 {
		return []string{"  nothing downloaded in the last 24 hours"}
	}
	sum := fmt.Sprintf("last 24 h: %d MB in %d files (%.2f MB/s on average) · Telegram waits %d · speed-limit waits %d", tot.bytes>>20, tot.files, float64(tot.bytes)/86400/1e6, tot.pace.Waits, tot.pace.Premium)
	if pacedHours > 0 {
		sum += fmt.Sprintf(" · parts allowed were all in use %d%% of the time", int(fullSum/float64(pacedHours)*100+0.5))
		if partHours > 0 {
			sum += fmt.Sprintf(" · %.1f s a part on average", partSum/float64(partHours))
		}
	}
	if len(tot.errs) > 0 {
		sum += " · errors: " + ixKindList(tot.errs)
	}
	return append([]string{sum, "  what limits downloads (a guess from these numbers): " + ixLimitGuess(tot, fullSum, pacedHours, partSum, partHours)}, out...)
}

func ixKindList(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] || (m[keys[i]] == m[keys[j]] && keys[i] < keys[j]) })
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

// ixLimitGuess names the most likely limit, plainly labelled as a guess.
func ixLimitGuess(t ixHourRow, fullSum float64, pacedHours int, partSum float64, partHours int) string {
	switch {
	case pacedHours == 0:
		return "not known yet (download pacing off or no record)"
	case t.pace.Premium > 0 && t.pace.Premium >= t.pace.Waits:
		return "Telegram's speed limit for accounts without Premium (it does not go away with fewer parts at once)"
	case t.pace.Waits > 6*pacedHours:
		return "Telegram's limit on parts at once (many waits)"
	case partHours > 0 && partSum/float64(partHours) > 8:
		return "Telegram answers slowly (it may be slowing the account down without asking it to wait)"
	case fullSum/float64(pacedHours) < 0.3:
		return "not Telegram: the parts allowed were mostly not all in use (too few files at once, held or set-aside files, the disk or NAS, or the queue)"
	}
	return "no clear limit: Telegram answers without waits and the parts allowed are in use"
}
