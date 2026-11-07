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
	dcFiles map[int]int // files finished, per server (ix.12.7)
	files   int
	bytes   int64
	errs    map[string]int
	pace    telegram.PaceHour
	paced   bool
	accts   int
	winSum  float64
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
	if q, err := a.db.Reader.QueryContext(ctx, `SELECT w.updated_at/3600000*3600, w.dc, COUNT(*) FROM tgdl_work w
		WHERE w.status='completed' AND w.updated_at>=? AND w.dc>0 AND NOT `+ixSkippedBefore+` GROUP BY 1,2`, from.UnixMilli()); err == nil {
		for q.Next() {
			var start int64
			var dc, n int
			if q.Scan(&start, &dc, &n) == nil {
				addDCCount(&row(start).dcFiles, dc, n)
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
				for dc, b := range h.DCBytes {
					if r.pace.DCBytes == nil {
						r.pace.DCBytes = map[int]int64{}
					}
					r.pace.DCBytes[dc] += b
				}
				for dc, n := range h.DCPremium {
					if r.pace.DCPremium == nil {
						r.pace.DCPremium = map[int]int{}
					}
					r.pace.DCPremium[dc] += n
				}
				ixAddCounts(&r.pace.DCPremiumSecs, h.DCPremiumSecs)
				ixAddCounts(&r.pace.DCWaits, h.DCWaits)
				ixAddCounts(&r.pace.DCPremiumSame, h.DCPremiumSame)
				ixAddCounts(&r.pace.DCSamples, h.DCSamples)
				ixAddCounts(&r.pace.DCInflight, h.DCInflight)
				ixAddCounts(&r.pace.DCWaiting, h.DCWaiting)
				ixAddCounts(&r.pace.DCWindow, h.DCWindow)
				ixAddCounts(&r.pace.DCLarge, h.DCLarge)
				ixAddCounts(&r.pace.DCLargeWaiting, h.DCLargeWaiting)
				ixAddCounts(&r.pace.DCLargeLimit, h.DCLargeLimit)
				ixAddCounts(&r.pace.DCCutsWait, h.DCCutsWait)
				ixAddCounts(&r.pace.DCCutsSlow, h.DCCutsSlow)
				ixAddCounts(&r.pace.DCTriesUp, h.DCTriesUp)
				ixAddCounts(&r.pace.DCTriesDown, h.DCTriesDown)
				ixAddCounts(&r.pace.DCAtFloor, h.DCAtFloor)
				ixAddCounts(&r.pace.DCBelow, h.DCBelow)
				ixAddCounts(&r.pace.DCOkMs, h.DCOkMs) // ix.12.12: where each server's time went
				ixAddCounts(&r.pace.DCFailMs, h.DCFailMs)
				ixAddCounts(&r.pace.DCFails, h.DCFails)
				ixAddCounts(&r.pace.DCWaitedMs, h.DCWaitedMs)
				ixAddCounts(&r.pace.DCSleptMs, h.DCSleptMs)
				ixAddCounts(&r.pace.DCLimitMs, h.DCLimitMs)
				ixAddCounts(&r.pace.DCWaitSecs, h.DCWaitSecs)
				r.pace.Waits += h.Waits
				r.pace.Premium += h.Premium
				r.pace.Seconds = max(r.pace.Seconds, h.Seconds)
				r.winSum += h.Window
				// Seconds per part and the full share: the slowest account's.
				r.pace.PartSecs = max(r.pace.PartSecs, h.PartSecs)
				r.pace.TriesUp += h.TriesUp
				r.pace.ClimbsRefused += h.ClimbsRefused
				r.pace.TriesDown += h.TriesDown
				r.pace.DropsKept += h.DropsKept
				r.pace.ByAnswers = r.pace.ByAnswers || h.ByAnswers
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
	// Speed-limited hours, all servers and each one: whether the limit is per server.
	limited, limitedBytes, limitedDC := 0, int64(0), map[int]int64{}
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
			if len(r.pace.DCBytes) > 0 || len(r.pace.DCPremium) > 0 {
				line += " | per server: " + ixDCList(r.pace.DCBytes, r.pace.DCPremium)
			}
			if len(r.pace.DCSamples) > 0 {
				line += " | parts per server, on average: " + ixDCLoad(r.pace)
			}
			if w := ixDCWaits(r.pace); w != "" {
				line += " | speed-limit waits per server: " + w
			}
			if w := ixDCCounts(r.pace.DCWaits); w != "" {
				line += " | plain Telegram waits per server: " + w
			}
			if t := ixDCTimes(r.pace); t != "" {
				line += " | time per server, seconds summed over parts: " + t
			}
			if r.pace.TriesUp+r.pace.TriesDown > 0 {
				line += " | " + ixTriesLine(r.pace)
			}
			if s := ixDCServerLine(r.pace, r.dcFiles); s != "" {
				line += " | by server: " + s
			}
			if r.pace.Premium > 0 {
				limited++
				limitedBytes += r.pace.Bytes
				for dc, b := range r.pace.DCBytes {
					limitedDC[dc] += b
				}
			}
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
	head := []string{sum, "  what limits downloads (a guess from these numbers): " + ixLimitGuess(tot, fullSum, pacedHours, partSum, partHours)}
	if limited > 0 {
		// Bytes an hour in the hours with speed-limit waits. If the limit were per
		// server, more servers busy would give more in all; the hours' files differ
		// in size, so this is a hint, not a measurement.
		per := make(map[int]int64, len(limitedDC))
		for dc, b := range limitedDC {
			per[dc] = b / int64(limited)
		}
		head = append(head, fmt.Sprintf("  speed-limited hours: %d, %d MB an hour in all; per server, MB an hour: %s", limited, limitedBytes/int64(limited)>>20, ixDCList(per, nil)))
	}
	return append(head, out...)
}

func addDCCount(m *map[int]int, dc, n int) {
	if *m == nil {
		*m = map[int]int{}
	}
	(*m)[dc] += n
}

// ixDCServerLine is each server's hour (ix.12.7): files finished, the
// window's cuts (for a plain wait, for slow answers), tries up and down, and
// the share of the intervals in use it spent at the 2-part floor or below.
func ixDCServerLine(h telegram.PaceHour, files map[int]int) string {
	seen := map[int]bool{}
	for _, m := range []map[int]int{files, h.DCCutsWait, h.DCCutsSlow, h.DCTriesUp, h.DCTriesDown, h.DCSamples} {
		for dc := range m {
			seen[dc] = true
		}
	}
	dcs := make([]int, 0, len(seen))
	for dc := range seen {
		dcs = append(dcs, dc)
	}
	sort.Ints(dcs)
	var parts []string
	for _, dc := range dcs {
		x := fmt.Sprintf("DC%d %d files", dc, files[dc])
		if c := h.DCCutsWait[dc] + h.DCCutsSlow[dc]; c > 0 {
			x += fmt.Sprintf(", cuts %d (wait %d, slow %d)", c, h.DCCutsWait[dc], h.DCCutsSlow[dc])
		}
		if h.DCTriesUp[dc]+h.DCTriesDown[dc] > 0 {
			x += fmt.Sprintf(", tries up %d down %d", h.DCTriesUp[dc], h.DCTriesDown[dc])
		}
		if n := h.DCSamples[dc]; n > 0 {
			x += fmt.Sprintf(", at the 2-part floor %d%%, below it %d%%", 100*h.DCAtFloor[dc]/n, 100*h.DCBelow[dc]/n)
		}
		parts = append(parts, x)
	}
	return strings.Join(parts, "; ")
}

func ixAddCounts(dst *map[int]int, src map[int]int) {
	for k, v := range src {
		if *dst == nil {
			*dst = map[int]int{}
		}
		(*dst)[k] += v
	}
}

// ixTriesLine says how the pacer's tries were judged, and where judging by
// answer time would have decided otherwise than by bytes: the evidence for
// switching it on (Settings → Downloads) or leaving it off.
func ixTriesLine(h telegram.PaceHour) string {
	by := "bytes"
	if h.ByAnswers {
		by = "answer time"
	}
	return fmt.Sprintf("tries judged by %s: %d bigger (answer time would have kept %d that bytes refused), %d smaller (bytes kept %d that answer time would have refused)", by, h.TriesUp, h.ClimbsRefused, h.TriesDown, h.DropsKept)
}

// ixDCLoad is "DC4 1.2 in flight of 2.0 allowed, 3.1 waiting", by server: far
// fewer in flight than allowed means too few parts are asked for; as many as
// allowed with more waiting means the window holds them back.
func ixDCLoad(h telegram.PaceHour) string {
	dcs := make([]int, 0, len(h.DCSamples))
	for dc := range h.DCSamples {
		dcs = append(dcs, dc)
	}
	sort.Ints(dcs)
	parts := make([]string, 0, len(dcs))
	for _, dc := range dcs {
		n := float64(h.DCSamples[dc])
		if n == 0 {
			continue
		}
		x := fmt.Sprintf("DC%d %.1f in flight of %.1f allowed, %.1f waiting", dc, float64(h.DCInflight[dc])/n, float64(h.DCWindow[dc])/n, float64(h.DCWaiting[dc])/n)
		if h.DCLargeLimit[dc] > 0 {
			x += fmt.Sprintf(" (large files %.1f of %.0f, %.1f waiting for one)", float64(h.DCLarge[dc])/n, float64(h.DCLargeLimit[dc])/n, float64(h.DCLargeWaiting[dc])/n)
		}
		parts = append(parts, x)
	}
	return strings.Join(parts, ", ")
}

// ixDCTimes is where each server's parts spent their time in the hour
// (ix.12.12): in flight (sampled every 10 s), of that with Telegram (answered,
// and failed), in deliberate waits (of those speed-limit sleeps of single
// requests), held by the request rate limit, and what none of those covers;
// then the seconds plain FLOOD_WAITs asked for. Numbers only.
func ixDCTimes(h telegram.PaceHour) string {
	seen := map[int]bool{}
	for _, m := range []map[int]int{h.DCOkMs, h.DCFailMs, h.DCWaitedMs, h.DCLimitMs, h.DCWaitSecs} {
		for dc := range m {
			seen[dc] = true
		}
	}
	if len(seen) == 0 {
		return ""
	}
	dcs := make([]int, 0, len(seen))
	for dc := range seen {
		dcs = append(dcs, dc)
	}
	sort.Ints(dcs)
	s := func(ms int) int { return (ms + 500) / 1000 }
	parts := make([]string, 0, len(dcs))
	for _, dc := range dcs {
		inflight := h.DCInflight[dc] * 10 // samples every 10 s (telegram.paceInterval)
		ok, failed, waited, limit := s(h.DCOkMs[dc]), s(h.DCFailMs[dc]), s(h.DCWaitedMs[dc]), s(h.DCLimitMs[dc])
		x := fmt.Sprintf("DC%d in flight %d, answered %d, failed %d (%d requests), waits %d (speed-limit sleeps %d), rate limit %d, other %d",
			dc, inflight, ok, failed, h.DCFails[dc], waited, s(h.DCSleptMs[dc]), limit, max(0, inflight-ok-failed-waited-limit))
		if n := h.DCWaits[dc]; n > 0 {
			x += fmt.Sprintf(", %d plain waits asked %d", n, h.DCWaitSecs[dc])
		}
		parts = append(parts, x)
	}
	return strings.Join(parts, "; ")
}

// ixDCList is "DC1 820 MB (3 limit waits), DC4 10 MB", by server number.
func ixDCList(bytes map[int]int64, premium map[int]int) string {
	seen := map[int]bool{}
	var dcs []int
	for dc := range bytes {
		seen[dc] = true
		dcs = append(dcs, dc)
	}
	for dc := range premium {
		if !seen[dc] {
			dcs = append(dcs, dc)
		}
	}
	sort.Ints(dcs)
	parts := make([]string, len(dcs))
	for i, dc := range dcs {
		parts[i] = fmt.Sprintf("DC%d %d MB", dc, bytes[dc]>>20)
		if n := premium[dc]; n > 0 {
			parts[i] += fmt.Sprintf(" (%d limit waits)", n)
		}
	}
	return strings.Join(parts, ", ")
}

// ixDCWaits is each server's speed-limit waits of an hour: how long Telegram
// asked for on average, and how many came for the same file as that server's
// wait before (ix.12.4; hours recorded before have no asked seconds).
func ixDCWaits(h telegram.PaceHour) string {
	var dcs []int
	for dc := range h.DCPremiumSecs {
		if h.DCPremium[dc] > 0 {
			dcs = append(dcs, dc)
		}
	}
	sort.Ints(dcs)
	parts := make([]string, len(dcs))
	for i, dc := range dcs {
		n := h.DCPremium[dc]
		parts[i] = fmt.Sprintf("DC%d %d, %.1f s asked on average, %d for the same file as the wait before", dc, n, float64(h.DCPremiumSecs[dc])/float64(n), h.DCPremiumSame[dc])
	}
	return strings.Join(parts, "; ")
}

// ixDCCounts is "DC1 3, DC4 1" for counts per server.
func ixDCCounts(m map[int]int) string {
	var dcs []int
	for dc, n := range m {
		if n > 0 {
			dcs = append(dcs, dc)
		}
	}
	sort.Ints(dcs)
	parts := make([]string, len(dcs))
	for i, dc := range dcs {
		parts[i] = fmt.Sprintf("DC%d %d", dc, m[dc])
	}
	return strings.Join(parts, ", ")
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
