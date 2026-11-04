package telegram

import (
	"encoding/json"
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
	for _, d := range p.dcs {
		h.Bytes += d.bytes
		h.Parts += d.parts
		h.took += d.took
		h.answers += d.answers
		if d.bytes > 0 || d.inflight > 0 || d.waited {
			h.winSum += d.window
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

func (p *downloadPacer) hourWaitLocked(now time.Time, premium bool) {
	h := p.hourLocked(now)
	if premium {
		h.Premium++
	} else {
		h.Waits++
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
		out = append(out, p.hours[i])
	}
	return out
}
