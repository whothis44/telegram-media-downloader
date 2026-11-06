package telegram

import (
	"encoding/json"
	"maps"
	"os"
	"time"
)

// An hour-by-hour record of how downloads went, for deciding what to change
// with numbers rather than impressions: what came through, how often and in
// which way Telegram asked to wait, how long it took per part, and whether
// the parts allowed in flight were what held things back. Numbers only; it
// is kept next to the session (".pace-hours.json") so a restart keeps it.

const paceHoursKept = 48

// PaceHour is one hour of downloads on the account, all datacenters.
type PaceHour struct {
	Start     int64   `json:"start"`     // unix seconds, start of the hour
	Bytes     int64   `json:"bytes"`     // file bytes received
	Parts     int     `json:"parts"`     // parts received
	Waits     int     `json:"waits"`     // FLOOD_WAITs on file parts
	Premium   int     `json:"premium"`   // FLOOD_PREMIUM_WAITs (speed limit without Premium)
	PartSecs  float64 `json:"partSecs"`  // average seconds Telegram took per part
	Window    float64 `json:"window"`    // average parts allowed in flight
	FullShare float64 `json:"fullShare"` // share of the hour downloads waited for room (0–1)
	Seconds   int     `json:"seconds"`   // seconds of the hour the pacer was measuring
	// Per Telegram server (datacenter): bytes received and speed-limit waits,
	// to tell whether the limit is per server (ix.12.3).
	DCBytes   map[int]int64 `json:"dcBytes,omitempty"`
	DCPremium map[int]int   `json:"dcPremium,omitempty"`
	// Per server (ix.12.4): the seconds Telegram asked for in its speed-limit
	// waits, summed, and the waits that came for the same file as that
	// server's wait before (a limit per file would show as most of them
	// when several files are downloading).
	DCPremiumSecs map[int]int `json:"dcPremiumSecs,omitempty"`
	DCPremiumSame map[int]int `json:"dcPremiumSame,omitempty"`
	// DCWaits: plain FLOOD_WAITs on file parts, per server (ix.12.5).
	DCWaits map[int]int `json:"dcWaits,omitempty"`
	// Per server, sampled every interval it was in use: parts in flight,
	// requests waiting for room, and the window, summed (DCSamples of them).
	// Their averages say whether the window, or too few parts asked for, is
	// what holds downloads back.
	DCSamples  map[int]int `json:"dcSamples,omitempty"`
	DCInflight map[int]int `json:"dcInflight,omitempty"`
	DCWaiting  map[int]int `json:"dcWaiting,omitempty"`
	DCWindow   map[int]int `json:"dcWindow,omitempty"`
	// Large-file slots (files of 20 MB or more), sampled likewise: in use,
	// files waiting for one, and how many Telegram's configuration allows.
	DCLarge        map[int]int `json:"dcLarge,omitempty"`
	DCLargeWaiting map[int]int `json:"dcLargeWaiting,omitempty"`
	DCLargeLimit   map[int]int `json:"dcLargeLimit,omitempty"`
	// Tries of a bigger or smaller window with answer times known, and where
	// judging by answer time would have decided otherwise than by bytes: a
	// climb bytes refused (speed perhaps missed), a drop bytes kept (the
	// window perhaps drifting down). ByAnswers: the hour was judged by answer time.
	TriesUp       int  `json:"triesUp,omitempty"`
	ClimbsRefused int  `json:"climbsRefused,omitempty"`
	TriesDown     int  `json:"triesDown,omitempty"`
	DropsKept     int  `json:"dropsKept,omitempty"`
	ByAnswers     bool `json:"byAnswers,omitempty"`

	took    time.Duration
	answers int
	winSum  int
	ticks   int
	full    int
}

func (p *downloadPacer) hourLocked(now time.Time) *PaceHour {
	start := now.Truncate(time.Hour).Unix()
	if n := len(p.hours); n > 0 && p.hours[n-1].Start == start {
		return &p.hours[n-1]
	}
	p.hours = append(p.hours, PaceHour{Start: start})
	if len(p.hours) > paceHoursKept {
		p.hours = p.hours[len(p.hours)-paceHoursKept:]
	}
	p.saveHoursLocked()
	return &p.hours[len(p.hours)-1]
}

// recordHourLocked adds one interval of every datacenter to its hour (called
// by tick before the interval's counters are reset).
func (p *downloadPacer) recordHourLocked(now time.Time, secs float64) {
	h := p.hourLocked(now)
	h.Seconds += int(secs)
	h.ticks++
	full := false
	h.ByAnswers = h.ByAnswers || paceByAnswers.Load()
	for _, d := range p.dcs {
		h.TriesUp += d.triesUp
		h.ClimbsRefused += d.climbsRefused
		h.TriesDown += d.triesDown
		h.DropsKept += d.dropsKept
		d.triesUp, d.climbsRefused, d.triesDown, d.dropsKept = 0, 0, 0, 0
		h.Bytes += d.bytes
		if d.bytes > 0 {
			if h.DCBytes == nil {
				h.DCBytes = map[int]int64{}
			}
			h.DCBytes[d.dc] += d.bytes
		}
		h.Parts += d.parts
		h.took += d.took
		h.answers += d.answers
		if d.bytes > 0 || d.inflight > 0 || d.waited {
			h.winSum += d.window
			if h.DCSamples == nil {
				h.DCSamples, h.DCInflight, h.DCWaiting, h.DCWindow = map[int]int{}, map[int]int{}, map[int]int{}, map[int]int{}
			}
			h.DCSamples[d.dc]++
			h.DCInflight[d.dc] += d.inflight
			h.DCWaiting[d.dc] += d.waiting
			h.DCWindow[d.dc] += d.window
			if p.slots != nil {
				large, waiting, limit := p.slots(d.dc)
				if h.DCLarge == nil {
					h.DCLarge, h.DCLargeWaiting, h.DCLargeLimit = map[int]int{}, map[int]int{}, map[int]int{}
				}
				h.DCLarge[d.dc] += large
				h.DCLargeWaiting[d.dc] += waiting
				h.DCLargeLimit[d.dc] += limit
			}
		}
		full = full || d.waited
	}
	if full {
		h.full++
	}
	h.derive()
	if h.ticks%30 == 0 { // every 5 minutes
		p.saveHoursLocked()
	}
}

func (h *PaceHour) derive() {
	if h.answers > 0 {
		h.PartSecs = (h.took / time.Duration(h.answers)).Seconds()
	}
	if h.ticks > 0 {
		h.Window = float64(h.winSum) / float64(h.ticks)
		h.FullShare = float64(h.full) / float64(h.ticks)
	}
}

func (p *downloadPacer) hourWaitLocked(now time.Time, dc int, premium bool, asked time.Duration, same bool) {
	h := p.hourLocked(now)
	if premium {
		h.Premium++
		if h.DCPremium == nil {
			h.DCPremium, h.DCPremiumSecs, h.DCPremiumSame = map[int]int{}, map[int]int{}, map[int]int{}
		}
		if h.DCPremiumSecs == nil {
			h.DCPremiumSecs, h.DCPremiumSame = map[int]int{}, map[int]int{}
		}
		h.DCPremium[dc]++
		h.DCPremiumSecs[dc] += int(asked.Round(time.Second).Seconds())
		if same {
			h.DCPremiumSame[dc]++
		}
	} else {
		h.Waits++
		if h.DCWaits == nil {
			h.DCWaits = map[int]int{}
		}
		h.DCWaits[dc]++
	}
}

func (p *downloadPacer) hoursPath() string {
	if p.path == "" {
		return ""
	}
	return p.path[:len(p.path)-len(".json")] + "-hours.json"
}

type paceHourFile struct {
	PaceHour
	Took    int64 `json:"tookMs"`
	Answers int   `json:"answers"`
	WinSum  int   `json:"winSum"`
	Ticks   int   `json:"ticks"`
	Full    int   `json:"full"`
}

func (p *downloadPacer) saveHoursLocked() {
	path := p.hoursPath()
	if path == "" {
		return
	}
	out := make([]paceHourFile, len(p.hours))
	for i, h := range p.hours {
		out[i] = paceHourFile{PaceHour: h, Took: h.took.Milliseconds(), Answers: h.answers, WinSum: h.winSum, Ticks: h.ticks, Full: h.full}
	}
	data, _ := json.Marshal(out)
	if os.WriteFile(path+".tmp", data, 0o600) == nil {
		_ = os.Rename(path+".tmp", path)
	}
}

func (p *downloadPacer) loadHours() {
	path := p.hoursPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var in []paceHourFile
	if json.Unmarshal(data, &in) != nil {
		return
	}
	for _, f := range in {
		h := f.PaceHour
		h.took, h.answers, h.winSum, h.ticks, h.full = time.Duration(f.Took)*time.Millisecond, f.Answers, f.WinSum, f.Ticks, f.Full
		h.derive()
		p.hours = append(p.hours, h)
	}
	if len(p.hours) > paceHoursKept {
		p.hours = p.hours[len(p.hours)-paceHoursKept:]
	}
}

// Hours is the record of the last 48 hours, newest first.
func (p *downloadPacer) hoursNewestFirst() []PaceHour {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PaceHour, 0, len(p.hours))
	for i := len(p.hours) - 1; i >= 0; i-- {
		h := p.hours[i]
		// The live hour's maps change under the lock.
		h.DCBytes, h.DCPremium = maps.Clone(h.DCBytes), maps.Clone(h.DCPremium)
		h.DCPremiumSecs, h.DCPremiumSame = maps.Clone(h.DCPremiumSecs), maps.Clone(h.DCPremiumSame)
		h.DCWaits = maps.Clone(h.DCWaits)
		h.DCSamples, h.DCInflight, h.DCWaiting, h.DCWindow = maps.Clone(h.DCSamples), maps.Clone(h.DCInflight), maps.Clone(h.DCWaiting), maps.Clone(h.DCWindow)
		h.DCLarge, h.DCLargeWaiting, h.DCLargeLimit = maps.Clone(h.DCLarge), maps.Clone(h.DCLargeWaiting), maps.Clone(h.DCLargeLimit)
		out = append(out, h)
	}
	return out
}
