package app

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/version"
)

// A diagnostics report that can be shared safely: counts, states, settings
// and error messages, with every chat, file, person and link replaced by a
// placeholder. Made only when asked for; nothing is collected meanwhile
// beyond the archive's last 300 log lines, kept in memory.

type ixRedactor struct {
	groups map[string]string // group ID or name -> G<n>
	names  []string          // group names, longest first
}

var (
	ixReGroupID = regexp.MustCompile(`-100\d{6,}|-\d{6,}`)
	ixReURL     = regexp.MustCompile(`(?i)\b(?:https?|ftp)://\S+|\bt\.me/\S+|\bwww\.\S+`)
	// A Windows path (drive letter or \\server\share, either slash) runs to
	// ": " (how Windows errors go on), a quote, or the end: folder names
	// may hold spaces.
	ixReWinPath = regexp.MustCompile(`(?i)(?:\b[A-Z]:[\\/]|\\\\)[^"'<>|\r\n]*?(?:: |["'<>|\r\n]|$)`)
	ixReUnix    = regexp.MustCompile(`(?:^|[\s"'(=])((?:[\w.\-()\[\] ]+/)+[\w.\-()\[\] ]*\.[A-Za-z0-9]{1,5})`)
	ixReFile    = regexp.MustCompile(`[\w\-() ]+\.(?i:jpe?g|png|gif|webp|mp4|mkv|mov|avi|webm|mp3|ogg|oga|m4a|pdf|zip|rar|7z|docx?|xlsx?|txt|apk|exe|heic|tgs)\b`)
	ixReAt      = regexp.MustCompile(`@[A-Za-z][A-Za-z0-9_]{3,}`)
	ixReEmailD  = regexp.MustCompile(`[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}`)
	ixReLongNum = regexp.MustCompile(`\b\d{6,}\b`)
	ixReHex     = regexp.MustCompile(`\b[0-9a-fA-F]{24,}\b`)
)

func (r *ixRedactor) group(id string) string {
	if t, ok := r.groups[id]; ok {
		return t
	}
	t := "G" + strconv.Itoa(len(r.groups)+1)
	r.groups[id] = t
	return t
}

// clean conceals everything identifying in one line of text.
func (r *ixRedactor) clean(s string) string {
	for _, name := range r.names {
		if len(name) >= 3 {
			s = strings.ReplaceAll(s, name, r.groups[name])
		}
	}
	s = ixReURL.ReplaceAllString(s, "<link>")
	s = ixReEmailD.ReplaceAllString(s, "<email>")
	s = ixReWinPath.ReplaceAllStringFunc(s, func(m string) string {
		end := ""
		switch {
		case strings.HasSuffix(m, ": "):
			end = ": "
		case len(m) > 0 && strings.ContainsRune("\"'<>|\r\n", rune(m[len(m)-1])):
			end = m[len(m)-1:]
		}
		return "<path>" + end
	})
	s = ixReUnix.ReplaceAllStringFunc(s, func(m string) string {
		lead := ""
		if len(m) > 0 && strings.ContainsRune(" \"'(=", rune(m[0])) {
			lead = m[:1]
		}
		return lead + "<path>"
	})
	s = ixReFile.ReplaceAllString(s, "<file>")
	s = ixReGroupID.ReplaceAllStringFunc(s, r.group)
	s = ixReAt.ReplaceAllString(s, "<user>")
	s = ixReHex.ReplaceAllString(s, "<hash>")
	s = ixReLongNum.ReplaceAllString(s, "<n>")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

func (a *App) ixDiagnostics(ctx context.Context) string {
	r := &ixRedactor{groups: map[string]string{}}
	metas, _ := a.ixGroups(ctx)
	for _, m := range metas {
		t := r.group(m.ID)
		if n := strings.TrimSpace(m.Name); n != "" {
			if _, ok := r.groups[n]; !ok {
				r.groups[n] = t
				r.names = append(r.names, n)
			}
		}
	}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT title FROM ix_group_info WHERE title<>'' UNION SELECT name FROM ix_peers WHERE name<>''`); err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				if _, ok := r.groups[n]; !ok {
					r.groups[n] = "<name>"
					r.names = append(r.names, n)
				}
			}
		}
		rows.Close()
	}
	sort.Slice(r.names, func(i, j int) bool { return len(r.names[i]) > len(r.names[j]) })
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	now := time.Now()
	line("Archive diagnostics (privacy-safe: chats, files, people and links are replaced by placeholders)")
	line("app %s · %s/%s · %s", version.AppVersion, runtime.GOOS, runtime.GOARCH, now.UTC().Format(time.RFC3339))
	s := a.ixLoadSettings(ctx)
	on := func(v bool) string { return map[bool]string{true: "on", false: "off"}[v] }
	set := func(v string) string { return map[bool]string{true: "set", false: "not set"}[v != ""] }
	line("\n[settings]")
	line("export %s at %s · pace %s · quiet %q–%q · pause on freeze %s · auto retry %s · download pacing %s · big files at once %d", on(s.ExportEnabled), s.ExportTime, s.Pace, s.QuietStart, s.QuietEnd, on(s.PauseOnFreeze), on(s.AutoRetry), on(s.DownloadPacing), s.BigAtOnce)
	line("duplicates %s (decoder %s, quiet %q–%q) · fixity %s · exact re-uploads %s · hold copies %s (forever %s)", on(s.DupEnabled), s.DupHWAccel, s.DupQuietStart, s.DupQuietEnd, on(s.FixityEnabled), on(s.SubstExact), on(s.HoldLikely), on(s.HoldForever))
	line("alerts: web address %s (%s) · email %s · names in alerts %s", set(s.AlertWebhook), s.AlertWebhookKind, set(s.AlertEmailTo), on(s.AlertNames))
	line("recognition services: auto start %s", on(s.MLAutoStart))
	line("\n[account]")
	acct := a.ixAccount()
	// Two different things (ix.12.3 says which is which): Telegram's own freeze
	// dates for the account, and whether recent errors were refusals because of it.
	fz := a.ixFreeze(ctx)
	if fz.Until > 0 {
		line("freeze: Telegram reports the account frozen since %s, deletion %s (%d days left)", time.Unix(fz.Since, 0).UTC().Format("2006-01-02"), time.Unix(fz.Until, 0).UTC().Format("2006-01-02"), int(time.Unix(fz.Until, 0).Sub(now).Hours()/24))
	} else {
		line("freeze: none reported (checked %s)%s", ixAgo(fz.CheckedAt, now), map[bool]string{true: " · last check failed: " + r.clean(fz.Error), false: ""}[fz.Error != ""])
	}
	line("recent errors: refused as frozen %v · deactivated %v · flood wait %v · last error %s (a frozen account can still read, so this stays false while nothing is refused)", acct.Frozen, acct.Deactivated, acct.FloodWait, r.clean(acct.LastError))
	line("hold: %s", r.clean(a.ixHold(ctx)))
	line("\n[restarts]")
	for _, l := range a.ixRestartsDiag(now) {
		line("%s", l)
	}
	line("\n[memory]")
	for _, l := range ixMemoryDiag() {
		line("%s", l)
	}
	line("")
	line("%s", r.clean(a.ixGoneDiag(now)))
	line("\n[rescan queue]")
	if st, err := a.ixLoadQueue(ctx); err == nil {
		count := map[string]int{}
		for _, it := range st.Items {
			count[it.State+"/"+it.Phase]++
		}
		line("running %v · %d groups · %v · notice: %s", st.Running, len(st.Items), count, r.clean(st.Notice))
		for _, it := range st.Items {
			if it.Error != "" || it.State == "running" {
				line("  %s %s/%s attempts %d resumes %s messages %d missing %d: %s", r.group(it.GroupID), it.State, it.Phase, it.Attempts, ixYesNo(it.ResumeFrom > 0), it.Messages, it.Missing, r.clean(it.Error))
			}
		}
	}
	line("\n[downloads]")
	d := a.ixDownloads(ctx)
	line("%s", a.ixDlState(ctx).diagLine(r.group)) // ix_dlstate.go
	line("engine running %v · queue paused %v · downloading %d · waiting %d · failed %d (given up %d) · last hour %d · last finished %s", d.EngineRunning, d.QueuePaused, d.Downloading, d.Waiting, d.Failed, d.GivenUp, d.LastHour, ixAgo(d.LastAt, now))
	mb := func(b float64) float64 { return b / 1e6 }
	if d.Pace.Off {
		line("download pacing is off (Settings): downloads go as in ix.8")
	}
	// The 5-minute figure first: "now" counts whole 1 MiB parts of the last few
	// seconds, so a few parts read as the same speed on every server.
	line("speed %.2f MB/s over 5 min, %.2f MB/s now · parts in flight allowed %d · Telegram waits: %d in the last hour, %d in the last day · speed-limit waits (no Premium): %d in the last hour, %d in the last day", mb(d.Pace.Speed5m), mb(d.Pace.Speed), d.Pace.Window, d.Pace.Floods1h, d.Pace.Floods24h, d.Pace.Premium1h, d.Pace.Premium24h)
	if !d.Pace.Off {
		mode := "each holds only the request that got it (ix.12.4)"
		if !d.Pace.PremiumPerRequest && !d.Pace.PremiumShared.IsZero() {
			mode = "every file waits for each (as before ix.12.4) until " + d.Pace.PremiumShared.In(ixTZ).Format("15:04")
		}
		fb := ixKindList(d.Pace.PremiumFallbacks)
		if fb == "" {
			fb = "none"
		}
		line("speed-limit waits: %s · back to every file waiting, since start: %s", mode, fb)
	}
	for _, dc := range d.Pace.DCs {
		line("  DC%d: window %d, in flight %d, waiting %d, %.2f MB/s over 5 min (now %.2f), %.1f s a part, waits last hour %d (speed limit %d) · %s", dc.DC, dc.Window, dc.InFlight, dc.Waiting, mb(dc.Speed5m), mb(dc.Speed), dc.PartSecs, dc.Floods1h, dc.Premium1h, dc.State)
		line("    Telegram wait time since start: %.1f request-seconds (summed across requests, separate from network response time)", dc.GateWaitSecs)
		line("    waiting for Telegram now %d requests; file slots: small %d/%d, large %d/%d (20 MB boundary)", dc.GateWaiting, dc.SmallFiles, dc.SmallFileLimit, dc.LargeFiles, dc.LargeFileLimit)
	}
	bf := d.Big
	line("big files (%d MB or more): %d downloading (at most %d at once, 0 = no limit) · %d waiting · %d set aside · %d held · backfills queueing files %d · queue %d of %d · backfill paused by the queue limit %v",
		bf.MinBytes>>20, bf.Downloading, bf.Limit, bf.Waiting, bf.SetAside, bf.Held, bf.Backfills, bf.QueueCount, bf.QueueCap, bf.CapPaused)
	var days []string
	for _, day := range d.Days {
		days = append(days, fmt.Sprintf("%s %d files %d MB", day.Day[5:], day.Files, day.Bytes>>20))
	}
	line("per day: %s", strings.Join(days, " | "))
	line("by hour:")
	for _, l := range a.ixDiagHours(ctx, now) {
		line("%s", l)
	}
	for _, e := range d.Errors {
		line("  %d× %s", e.Count, r.clean(e.Error))
	}
	h := a.ixHeldStatus(ctx)
	line("copies: %d exact re-uploads not downloaded (%d MB) · %d held · %d released", h.Subst, h.SavedMB, h.Held, h.Released)
	for _, l := range a.ixDedupDiag(ctx) { // ix_dedup.go
		line("%s", l)
	}
	for _, l := range a.ixCopiesDiag(ctx) {
		line("%s", l)
	}
	for _, l := range a.ixTransferDiag(ctx, r.clean) {
		line("%s", l)
	}
	line("%s", a.ixUpdateDiag(ctx))
	line("\n[group priorities]")
	for _, l := range a.ixPrioritiesDiag(ctx, r.group) { // ix_coverage.go
		line("%s", l)
	}
	line("\n[background work]")
	pic := a.ixMediaStatus()
	line("pictures %d/%d (errors %d, grouped %d, to review %d, running %v)", pic.Done, pic.Total, pic.Errors, pic.Grouped, pic.Suggested, pic.Running)
	vd := a.ixVdupStatus()
	line("videos %d/%d (errors %d, pairs %d, decoder %s, ffmpeg %v, running %v) last error: %s", vd.Done, vd.Total, vd.Errors, vd.Pairs, vd.HWAccel, vd.FFmpeg, vd.Running, r.clean(vd.LastError))
	for _, q := range []struct{ label, sql string }{
		{"picture index errors", `SELECT error, COUNT(*) FROM ix_pdq WHERE status='error' GROUP BY error ORDER BY 2 DESC LIMIT 10`},
		{"video index errors", `SELECT error, COUNT(*) FROM ix_vfp WHERE status='error' GROUP BY error ORDER BY 2 DESC LIMIT 10`},
		{"integrity results", `SELECT result, COUNT(*) FROM ix_fixity GROUP BY result ORDER BY 2 DESC`},
		{"image recognition errors", `SELECT error, COUNT(*) FROM ix_emb_err GROUP BY error ORDER BY 2 DESC LIMIT 8`},
		{"faces/text/speech errors", `SELECT task||': '||error, COUNT(*) FROM ix_ml_err GROUP BY 1 ORDER BY 2 DESC LIMIT 8`},
		{"file plans", `SELECT state||' '||kind, COUNT(*) FROM ix_move_plans GROUP BY 1`},
	} {
		if rows, err := a.db.Reader.QueryContext(ctx, q.sql); err == nil {
			var parts []string
			for rows.Next() {
				var k string
				var n int
				if rows.Scan(&k, &n) == nil {
					parts = append(parts, fmt.Sprintf("%d× %s", n, r.clean(k)))
				}
			}
			rows.Close()
			if len(parts) > 0 {
				line("%s: %s", q.label, strings.Join(parts, " | "))
			}
		}
	}
	ml := a.ixMLStatusNow(ctx)
	line("image recognition %s (model %s, Immich %s) · reachable %v · looked at %d · to go %d · unreadable %d · running %v · last error: %s", on(ml.Enabled), ml.Model, ml.Version, ml.Reachable, ml.Embedded, ml.Pending, ml.Failed, ml.Running, r.clean(ml.LastError))
	for _, name := range []string{"faces", "text", "frames"} {
		t := ml.Tasks[name]
		line("  %s %s · done %d · to go %d · unreadable %d · last error: %s", name, on(t.On), t.Done, t.Pending, t.Failed, r.clean(t.LastError))
	}
	atOnce, timing := a.ixMLPaceReport()
	line("  files at once: %s", atOnce)
	line("  ms per file: %s", timing)
	line("  video frames: %s", a.ixVFrameReport())
	line("speech to text %s · reachable %v · done %d · to go %d · unreadable %d · last error: %s", on(ml.Speech.On), ml.Speech.Reachable, ml.Speech.Done, ml.Speech.Pending, ml.Speech.Failed, r.clean(ml.Speech.LastError))
	var people, faces, ungrouped int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM ix_people WHERE merged_into=0), COUNT(*), COALESCE(SUM(person=0),0) FROM ix_faces`).Scan(&people, &faces, &ungrouped)
	line("people %d · faces %d (%d not grouped)", people, faces, ungrouped)
	if faces > 0 {
		for _, l := range ixFaceLooseLines(a.ixFaceLooseNow(ctx, time.Minute, time.Minute)) {
			line("%s", l)
		}
	}
	dl := a.ixDeadlineEstimate(ctx, false)
	line("still to download: %d distinct files / %d MB (+%d copies that come free)", dl.ToGetFiles, dl.ToGetBytes>>20, dl.Copies)
	limit := ""
	if dl.Premium1h > 0 {
		limit = fmt.Sprintf(", speed limit on: %d FLOOD_PREMIUM_WAIT in the last hour", dl.Premium1h)
	}
	line("deadline order %s (active %v) · queued %d files / %d MB · speed %.1f MB/h (day), %.1f MB/h (3 h), %.1f MB/h (hour) · estimate at %s speed (%.1f MB/h%s) · finishes %s · set aside by band %v · groups not fully read %d",
		dl.Mode, dl.Active, dl.LeftFiles, dl.LeftBytes>>20, dl.Rate24h*3600/1e6, dl.Rate3h*3600/1e6, dl.Rate1h*3600/1e6,
		map[string]string{"day": "the day's", "3h": "the last 3 h"}[dl.RateUsed], dl.Rate*3600/1e6, limit, dl.Finishes, dl.Deferred, dl.Unscanned)
	fx := a.ixFixityStatus(ctx)
	line("integrity: %d files, %d checked this cycle, %d changed, %d missing, %d size differs, last %s", fx.Files, fx.Checked, fx.Changed, fx.Missing, fx.SizeOff, ixAgo(fx.LastCheck, now))
	box := a.ixModelBox()
	box.mu.Lock()
	if m := box.model; m != nil {
		biggest := 0
		for i := range m.Sets {
			biggest = max(biggest, len(m.Sets[i].Families))
		}
		saved := 0
		for i := range m.Items {
			if m.Items[i].Key != "" {
				saved++
			}
		}
		line("browser index: %d posts, %d originals, %d files (%d saved), %d distinct files, %d media sets (largest %d posts), built %s in %s", len(m.Posts), len(m.Families), len(m.Items), saved, len(m.Clusters), len(m.Sets), biggest, ixAgo(m.Built.UnixMilli(), now), m.Took.Round(time.Millisecond))
	} else {
		line("browser index: not built yet")
	}
	if box.err != "" {
		line("browser index error: %s", r.clean(box.err))
	}
	box.mu.Unlock()
	ex := a.ixExportStatus()
	line("export: last %s · error: %s", ixAgo(ex.FinishedAt, now), r.clean(ex.Error))
	line("%s", a.ixBackupDiag(ctx, now, r.clean))
	line("\n[messages]")
	var msgs, groups, gone int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT group_id) FROM ix_messages`).Scan(&msgs, &groups)
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_absent`).Scan(&gone)
	line("%d messages in %d chats · %d message numbers confirmed gone", msgs, groups, gone)
	line("\n[recent archive log]")
	rt := a.ix()
	rt.mu.Lock()
	recent := append([]ixLogLine(nil), rt.recent...)
	rt.mu.Unlock()
	if len(recent) > 120 {
		recent = recent[len(recent)-120:]
	}
	for _, l := range recent {
		line("%s %s", l.At.UTC().Format("01-02 15:04:05"), r.clean(l.Text))
	}
	line("\n[recent errors]")
	counts := map[string]int{}
	for _, e := range a.ixRecentErrors(ctx, now.Add(-24*time.Hour).UnixMilli()) {
		counts[r.clean(e)]++
	}
	type kv struct {
		k string
		n int
	}
	var list []kv
	for k, n := range counts {
		list = append(list, kv{k, n})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n || (list[i].n == list[j].n && list[i].k < list[j].k) })
	for i, x := range list {
		if i == 30 {
			break
		}
		line("  %d× %s", x.n, x.k)
	}
	return b.String()
}

func ixAgo(ms int64, now time.Time) string {
	if ms <= 0 {
		return "never"
	}
	d := now.Sub(time.UnixMilli(ms)).Round(time.Minute)
	return d.String() + " ago"
}

func (a *App) handleIxDiagnostics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(a.ixDiagnostics(r.Context())))
}

// ixYesNo: a message number would say where in a chat the rescan is; the
// report only says whether it resumes.
func ixYesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
