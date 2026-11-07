package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// The forecast to the deletion date (ix.12.9): what in the download queue
// would finish before Telegram deletes the account, at two speeds, by stage,
// by size and by group, so a person can still raise a group's rank in time.
//
// It walks the queue in about the order it goes (ix_order.go): what is
// downloading now, then normal work in claim order (ranked groups by their
// place, then the others), then the files the deadline order set aside, band
// by band (private chats first), then probable copies held back, originals
// waiting to replace a lower quality, and confirmed copies. A Telegram file
// queued twice costs its bytes once. Per-server file slots and the big-file
// lane change the real order a little, files a rescan has not queued yet are
// not in it (their total is shown apart), and the deletion date is a day,
// not an hour: an estimate, not a promise. Only groups added to the archive
// count: a group added later makes the queue longer (its files go ahead of
// the held copies, and ahead of other groups if it is ranked).
//
// The two speeds come from the pacer's hours (what the network brought,
// telegram/pacer_hours.go), not from finished files (those are counted when
// staging has copied a file to the library, later than it arrived):
//   - the last 24 hours, bytes over the seconds measured;
//   - the speed-limited hours of the kept 48 (more than
//     ixForecastLimitedWaits FLOOD_PREMIUM_WAITs): what to expect if
//     Telegram's speed limit stays on until the deletion date.

const (
	ixForecastLimitedWaits = 50   // speed-limit waits in an hour that make it a limited hour
	ixForecastMinSeconds   = 1800 // seconds an hour must have been measured to count
	ixForecastMinHours     = 3    // limited hours needed for that speed
)

// ixForecastHours are the pacer's hours of the running accounts, summed by
// hour (replaceable in tests).
var ixForecastHours = func(a *App) []telegram.PaceHour {
	if a.monitor == nil {
		return nil
	}
	by := map[int64]*telegram.PaceHour{}
	for _, s := range a.monitor.Pace() {
		for _, h := range s.Hours {
			t := by[h.Start]
			if t == nil {
				t = &telegram.PaceHour{Start: h.Start}
				by[h.Start] = t
			}
			t.Bytes += h.Bytes
			t.Premium += h.Premium
			t.Seconds = max(t.Seconds, h.Seconds)
		}
	}
	out := make([]telegram.PaceHour, 0, len(by))
	for _, h := range by {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// Stages of the forecast, in the order they go.
const (
	ixFcNow       = iota // downloading now
	ixFcRanked           // normal work of ranked groups (and live, self-destructing, manual)
	ixFcOther            // normal work of the other groups
	ixFcDeferred         // set aside by the deadline order
	ixFcHeld             // probable copies held back
	ixFcLowq             // originals waiting to replace a lower quality
	ixFcConfirmed        // confirmed copies
	ixFcStages
)

var ixFcStageNames = [ixFcStages]string{
	"downloading now", "ranked groups and other files first", "other groups' files ready",
	"set aside (smaller files and private chats first)", "probable copies held back",
	"originals of videos saved in a lower quality", "confirmed copies",
}

// ixFcBandLimits are the size bands shown (the deadline order's, and 1 GB).
var ixFcBandLimits = []int64{10 << 20, 100 << 20, 500 << 20, 1 << 30}
var ixFcBandNames = []string{"under 10 MB", "10-100 MB", "100-500 MB", "500 MB-1 GB", "1 GB or more"}

func ixFcBand(size int64) int {
	for i, l := range ixFcBandLimits {
		if size < l {
			return i
		}
	}
	return len(ixFcBandLimits)
}

// ixForecastRate is one of the two speeds.
type ixForecastRate struct {
	Name     string  `json:"name"`     // "last24h" or "limited"
	Rate     float64 `json:"rate"`     // bytes a second; 0: not known
	Hours    int     `json:"hours"`    // hours behind it
	Budget   int64   `json:"budget"`   // bytes that fit before the deletion date
	Finishes bool    `json:"finishes"` // the whole queue fits, and the files not queued yet
	Spare    int64   `json:"spare"`    // bytes left after all that (0 when it does not fit)
}

// ixForecastRow is a part of the queue and what of it finishes at each speed.
type ixForecastRow struct {
	Name      string   `json:"name"`
	GroupID   string   `json:"groupId,omitempty"`
	Place     int      `json:"place,omitempty"` // rank place, 0: unranked
	Private   bool     `json:"private,omitempty"`
	Files     int      `json:"files"`
	Bytes     int64    `json:"bytes"` // a file queued twice counted once
	DoneFiles [2]int   `json:"doneFiles"`
	DoneBytes [2]int64 `json:"doneBytes"`
}

func (r *ixForecastRow) add(size int64, done [2]bool) {
	r.Files++
	r.Bytes += size
	for i, d := range done {
		if d {
			r.DoneFiles[i]++
			r.DoneBytes[i] += size
		}
	}
}

type ixForecast struct {
	Until    int64             `json:"until"`
	LeftSec  int64             `json:"leftSec"`
	Rates    [2]ixForecastRate `json:"rates"`
	Total    ixForecastRow     `json:"total"`
	Stages   []ixForecastRow   `json:"stages"`
	Bands    []ixForecastRow   `json:"bands"`
	Groups   []ixForecastRow   `json:"groups"` // ranked by place, then the others with the most left over under the limit
	Paused   int               `json:"paused"` // paused by a person, or held for ever: not in the forecast
	Unqueued int               `json:"unqueued"`
	UnqBytes int64             `json:"unqueuedBytes"` // about: not in the queue yet
	UnqFit   [2]int64          `json:"unqueuedFit"`   // of them, bytes that fit at each speed (counted before the held copies)
}

// ixForecastRates measures the two speeds from the pacer's hours.
func ixForecastRates(hours []telegram.PaceHour, now time.Time) [2]ixForecastRate {
	out := [2]ixForecastRate{{Name: "last24h"}, {Name: "limited"}}
	var dayBytes, limBytes int64
	var daySecs, limSecs int
	from := now.Add(-24 * time.Hour).Unix()
	for _, h := range hours {
		if h.Start >= from && h.Seconds > 0 {
			dayBytes += h.Bytes
			daySecs += h.Seconds
			out[0].Hours++
		}
		if h.Premium > ixForecastLimitedWaits && h.Seconds >= ixForecastMinSeconds {
			limBytes += h.Bytes
			limSecs += h.Seconds
			out[1].Hours++
		}
	}
	if daySecs >= 3600 {
		out[0].Rate = float64(dayBytes) / float64(daySecs)
	}
	if out[1].Hours >= ixForecastMinHours && limSecs > 0 {
		out[1].Rate = float64(limBytes) / float64(limSecs)
	}
	return out
}

// ixForecastItem is one queue row as the forecast sees it.
type ixForecastItem struct {
	id, sched, prio int64
	stage, band     int
	group, identity string
	size            int64
}

// ixForecastNow builds the forecast; nil without a deletion date.
func (a *App) ixForecastNow(ctx context.Context, d ixDeadlineInfo) *ixForecast {
	if d.Until <= 0 {
		return nil
	}
	now := time.Now()
	f := &ixForecast{Until: d.Until, LeftSec: max(0, d.Until-now.Unix())}
	f.Rates = ixForecastRates(ixForecastHours(a), now)
	for i := range f.Rates {
		f.Rates[i].Budget = int64(f.Rates[i].Rate * float64(f.LeftSec))
	}
	s := a.ixLoadSettings(ctx)
	held, confirmed := ixFcHeld, ixFcConfirmed
	switch {
	case s.HoldLikely && s.HoldForever:
		held, confirmed = -1, -1 // wait for a person
	case s.ConfirmedGoEarly:
		confirmed = ixFcHeld
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT w.id, w.group_id, COALESCE(w.file_size,0), COALESCE(w.identity,''), w.sched_order,
		CASE WHEN w.ttl_seconds>0 THEN 0 WHEN w.origin NOT IN ('live','history') THEN 1 WHEN w.origin='history' AND w.sched_order>=? THEN 3 ELSE 2 END,
		CASE WHEN w.status='processing' THEN 0
			WHEN h.work_id IS NOT NULL AND COALESCE(c.confirm,'')='confirmed' THEN 6
			WHEN h.work_id IS NOT NULL THEN 4
			WHEN EXISTS (SELECT 1 FROM ix_lowq q WHERE q.work_id=w.id AND q.state='saved') THEN 5
			WHEN x.work_id IS NOT NULL THEN 3
			WHEN w.paused=0 THEN CASE WHEN w.sched_order<? OR w.origin<>'history' OR w.ttl_seconds>0 THEN 1 ELSE 2 END
			ELSE -1 END,
		COALESCE(x.band,0)
		FROM tgdl_work w
		LEFT JOIN ix_held h ON h.work_id=w.id AND h.released_at=0 AND h.reason<>''
		LEFT JOIN ix_copy_hold c ON c.work_id=w.id
		LEFT JOIN ix_deferred x ON x.work_id=w.id AND x.released_at=0
		WHERE w.status IN ('pending','processing')`, engine.UnrankedOrder, engine.UnrankedOrder)
	if err != nil {
		return nil
	}
	var items []ixForecastItem
	for rows.Next() {
		var it ixForecastItem
		if rows.Scan(&it.id, &it.group, &it.size, &it.identity, &it.sched, &it.prio, &it.stage, &it.band) != nil {
			continue
		}
		switch it.stage {
		case ixFcHeld:
			it.stage = held
		case ixFcConfirmed:
			it.stage = confirmed
		}
		if it.stage < 0 {
			f.Paused++
			continue
		}
		items = append(items, it)
	}
	rows.Close()
	sort.Slice(items, func(i, j int) bool {
		x, y := items[i], items[j]
		if x.stage != y.stage {
			return x.stage < y.stage
		}
		if x.stage == ixFcDeferred && x.band != y.band {
			return x.band < y.band
		}
		if x.prio != y.prio {
			return x.prio < y.prio
		}
		if x.sched != y.sched {
			return x.sched < y.sched
		}
		return x.id < y.id
	})
	// Names, places and public chats, for the groups.
	place := map[string]int{}
	if ranks, err := a.ixRanks(ctx); err == nil {
		sortRankedByBase(ranks)
		for i, g := range ranks {
			place[g.GroupID] = i + 1
		}
	}
	names := map[string]string{}
	if metas, err := a.ixGroups(ctx); err == nil {
		for _, g := range metas {
			names[g.ID] = g.Name
		}
	}
	public := map[string]bool{}
	if q, err := a.db.Reader.QueryContext(ctx, `SELECT group_id FROM ix_group_info WHERE username<>''`); err == nil {
		for q.Next() {
			var id string
			if q.Scan(&id) == nil {
				public[id] = true
			}
		}
		q.Close()
	}
	f.Stages = make([]ixForecastRow, ixFcStages)
	for i := range f.Stages {
		f.Stages[i].Name = ixFcStageNames[i]
	}
	f.Bands = make([]ixForecastRow, len(ixFcBandNames))
	for i := range f.Bands {
		f.Bands[i].Name = ixFcBandNames[i]
	}
	groups := map[string]*ixForecastRow{}
	// What each row costs: a Telegram file queued twice comes once.
	costs := make([]int64, len(items))
	seen := map[string]bool{}
	var queued int64
	for i, it := range items {
		costs[i] = it.size
		if it.identity != "" {
			if seen[it.identity] {
				costs[i] = 0 // the same Telegram file: it comes with the first
			}
			seen[it.identity] = true
		}
		queued += costs[i]
	}
	// Files no rescan has queued yet: every distinct file still to download,
	// less the distinct files in the queue (about). Once queued they go with
	// normal work or are set aside, so ahead of the held copies: the walk
	// counts them there.
	if d.ToGetFiles > 0 {
		f.Unqueued = max(0, d.ToGetFiles-len(seen)-ixFcNoIdentity(items))
		f.UnqBytes = max(0, d.ToGetBytes-queued)
	}
	var cum int64
	unqDone := false
	unqueued := func() {
		if unqDone {
			return
		}
		unqDone = true
		for i, r := range f.Rates {
			if r.Rate > 0 {
				f.UnqFit[i] = min(f.UnqBytes, max(0, r.Budget-cum))
			}
		}
		cum += f.UnqBytes
	}
	for i, it := range items {
		if it.stage >= ixFcHeld {
			unqueued()
		}
		cost := costs[i]
		cum += cost
		var done [2]bool
		for i, r := range f.Rates {
			done[i] = r.Rate > 0 && cum <= r.Budget
		}
		f.Total.add(cost, done)
		f.Stages[it.stage].add(cost, done)
		f.Bands[ixFcBand(it.size)].add(cost, done)
		g := groups[it.group]
		if g == nil {
			g = &ixForecastRow{GroupID: it.group, Place: place[it.group], Private: !public[it.group], Name: names[it.group]}
			if g.Name == "" {
				g.Name = "Chat " + it.group
			}
			groups[it.group] = g
		}
		g.add(cost, done)
	}
	unqueued()
	for i := range f.Rates {
		r := &f.Rates[i]
		r.Finishes = r.Rate > 0 && cum <= r.Budget
		if r.Finishes {
			r.Spare = r.Budget - cum
		}
	}
	for _, g := range groups {
		f.Groups = append(f.Groups, *g)
	}
	sort.Slice(f.Groups, func(i, j int) bool {
		x, y := f.Groups[i], f.Groups[j]
		if (x.Place > 0) != (y.Place > 0) {
			return x.Place > 0
		}
		if x.Place != y.Place {
			return x.Place < y.Place
		}
		lx, ly := x.Bytes-x.DoneBytes[1], y.Bytes-y.DoneBytes[1]
		if lx != ly {
			return lx > ly
		}
		return x.GroupID < y.GroupID
	})
	if len(f.Groups) > 200 {
		f.Groups = f.Groups[:200]
	}
	return f
}

func ixFcNoIdentity(items []ixForecastItem) int {
	n := 0
	for _, it := range items {
		if it.identity == "" {
			n++
		}
	}
	return n
}

// ixForecastDiag is the report's lines, with groups as placeholders.
func ixForecastDiag(f *ixForecast, group func(string) string) []string {
	if f == nil {
		return []string{"forecast to the deletion date: no deletion date set"}
	}
	gb := func(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/1e9) }
	label := [2]string{"at the last 24 h speed", "if the speed limit stays on"}
	out := []string{fmt.Sprintf("forecast to the deletion date (%s, %.1f days left; groups added so far only; the queue in about the order it goes; an estimate): queue %d files / %s",
		time.Unix(f.Until, 0).UTC().Format("2006-01-02"), float64(f.LeftSec)/86400, f.Total.Files, gb(f.Total.Bytes))}
	for i, r := range f.Rates {
		if r.Rate <= 0 {
			why := "not enough hours measured"
			if i == 1 {
				why = fmt.Sprintf("fewer than %d speed-limited hours in the pacer's record", ixForecastMinHours)
			}
			out = append(out, fmt.Sprintf("  %s: speed not known (%s)", label[i], why))
			continue
		}
		l := fmt.Sprintf("  %s (%.1f GB/h, from %d hours): %s fit · finishes %d files / %s of the queue", label[i], r.Rate*3600/1e9, r.Hours, gb(r.Budget), f.Total.DoneFiles[i], gb(f.Total.DoneBytes[i]))
		if r.Finishes {
			l += fmt.Sprintf(" · everything fits, with the files not queued yet; room for about %s more", gb(r.Spare))
		}
		out = append(out, l)
	}
	part := func(rows []ixForecastRow) string {
		var s []string
		for _, r := range rows {
			if r.Files == 0 {
				continue
			}
			s = append(s, fmt.Sprintf("%s %d / %s → %d / %s, %d / %s", r.Name, r.Files, gb(r.Bytes), r.DoneFiles[0], gb(r.DoneBytes[0]), r.DoneFiles[1], gb(r.DoneBytes[1])))
		}
		return strings.Join(s, " | ")
	}
	out = append(out, "  by stage (in the queue → finishes at the 24 h speed, under the limit): "+part(f.Stages))
	out = append(out, "  by size: "+part(f.Bands))
	var ranked, others []string
	for _, g := range f.Groups {
		s := fmt.Sprintf("%s %d / %s → %d, %d", group(g.GroupID), g.Files, gb(g.Bytes), g.DoneFiles[0], g.DoneFiles[1])
		if g.Place > 0 {
			ranked = append(ranked, fmt.Sprintf("place %d %s", g.Place, s))
		} else if len(others) < 15 {
			others = append(others, s)
		}
	}
	if len(ranked) > 0 {
		out = append(out, "  ranked groups (files / size in the queue → files finishing at the 24 h speed, under the limit): "+strings.Join(ranked, " · "))
	}
	if len(others) > 0 {
		out = append(out, "  other groups, most left over under the limit first: "+strings.Join(others, " · "))
	}
	tail := fmt.Sprintf("  not queued yet: about %d files / %s, counted before the held copies (a rescan adds them) → fits %s at the 24 h speed, %s under the limit", f.Unqueued, gb(f.UnqBytes), gb(f.UnqFit[0]), gb(f.UnqFit[1]))
	if f.Paused > 0 {
		tail += fmt.Sprintf(" · not counted: %d paused by a person or held for ever", f.Paused)
	}
	return append(out, tail)
}
