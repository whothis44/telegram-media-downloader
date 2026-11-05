package app

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/crashlog"
)

// CrashDir is the data folder's folder for crash reports and the run history.
const CrashDir = "crashes"

// ixRestartsDiag says how the app's runs ended: a launcher that starts it again
// would otherwise hide a crash. The lines hold the runtime's words and function
// names only (crashlog.Parse), so they need no placeholders; keep it that way.
func (a *App) ixRestartsDiag(now time.Time) []string {
	dir := filepath.Join(a.dataDir, CrashDir)
	var out []string
	if at, ok := crashlog.Started(dir); ok {
		out = append(out, fmt.Sprintf("this run: up %s", now.Sub(at).Round(time.Minute)))
	} else {
		out = append(out, "this run: not on record")
	}
	counts := map[string]int{}
	var recent []crashlog.Run
	for _, r := range crashlog.Read(dir) {
		if now.Sub(r.Ended) > 7*24*time.Hour {
			continue
		}
		counts[r.How]++
		if r.How != crashlog.HowClean && len(recent) < 8 {
			recent = append(recent, r)
		}
	}
	out = append(out, fmt.Sprintf("last 7 days: %d crashes, %d ended with no report (window closed, killed or power), %d stopped cleanly",
		counts[crashlog.HowCrash], counts[crashlog.HowNoReport], counts[crashlog.HowClean]))
	for _, r := range recent {
		out = append(out, fmt.Sprintf("  %s %s", r.Ended.UTC().Format("01-02 15:04"), r.Text()))
	}
	return out
}
