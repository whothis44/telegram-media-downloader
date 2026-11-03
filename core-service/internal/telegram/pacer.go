package telegram

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// The download pacer: how many file parts an account asks Telegram for at
// once, found by measuring rather than set by hand.
//
// Telegram limits file downloads per account by the number of requests in
// flight and over time; past the limit it answers FLOOD_WAIT and nothing
// moves for that long. Official clients stay well inside it (TDLib keeps
// about 2 MiB of parts in flight per datacenter on a free account, 16 MiB
// with Premium). Asking for more than the limit is slower, not faster.
//
// So each datacenter gets a window: the parts that may be in flight at once,
// shared by every download of the account. Every ten seconds the pacer looks
// at what came through:
//
//   - a FLOOD_WAIT cuts the window to 60% and holds it there for a minute,
//     and for ten minutes the window stays below the one that flooded;
//   - when downloads are waiting for room and nothing flooded, it tries one
//     part more, and keeps it only if the speed then rises by 5% or more;
//     a try that brings nothing is taken back and not tried again for two
//     minutes (the speed is set elsewhere: the line, the disk, Telegram);
//   - when nobody waits for room, the window stays as it is.
//
// The window learned for each datacenter is kept next to the session, so a
// restart starts from it rather than learning again.

const (
	paceInterval   = 10 * time.Second
	paceStart      = 4  // parts in flight on a datacenter not seen before (TDLib's free tier is 2 MiB)
	paceMin        = 1  // never fewer
	paceMax        = 32 // never more
	paceGain       = 1.05
	paceCut        = 0.6
	paceFloodHold  = time.Minute
	pacePlateau    = 2 * time.Minute
	paceCeiling    = 10 * time.Minute // after a flood, stay below the window it happened at this long
	paceHistoryLen = 30               // intervals kept for the 5-minute speed
)

type dcPace struct {
	dc       int
	window   int
	inflight int
	waiting  int
	wake     chan struct{}

	// This interval.
	bytes   int64
	parts   int
	floods  int
	waited  bool
	busyFor time.Duration // time with the window full

	// Learning.
	probing      bool
	probeBase    float64
	holdUntil    time.Time
	ceiling      int // the window a flood happened at
	ceilingUntil time.Time
	state        string

	speed   float64   // bytes a second, last interval
	history []float64 // last intervals' speeds
	floodAt []time.Time
	lastUse time.Time
}

type downloadPacer struct {
	mu      sync.Mutex
	dcs     map[int]*dcPace
	path    string // where windows are kept ("" = not kept)
	last    time.Time
	floodAt []time.Time // every flood of the account in the last day
	now     func() time.Time
	saved   map[int]int
}

func newDownloadPacer(path string) *downloadPacer {
	p := &downloadPacer{dcs: map[int]*dcPace{}, path: path, now: time.Now, saved: map[int]int{}}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			var kept map[string]int
			if json.Unmarshal(data, &kept) == nil {
				for k, v := range kept {
					if dc, err := strconv.Atoi(k); err == nil && v >= paceMin && v <= paceMax {
						p.saved[dc] = v
					}
				}
			}
		}
	}
	return p
}

func (p *downloadPacer) dcLocked(dc int) *dcPace {
	d := p.dcs[dc]
	if d == nil {
		w := paceStart
		if v, ok := p.saved[dc]; ok {
			w = v
		}
		d = &dcPace{dc: dc, window: w, wake: make(chan struct{}), state: "learning"}
		p.dcs[dc] = d
	}
	return d
}

func (d *dcPace) broadcast() {
	close(d.wake)
	d.wake = make(chan struct{})
}

// acquire waits for room to ask for one more part from dc.
func (p *downloadPacer) acquire(ctx context.Context, dc int) error {
	for {
		p.mu.Lock()
		d := p.dcLocked(dc)
		d.lastUse = p.now()
		if d.inflight < d.window {
			d.inflight++
			p.mu.Unlock()
			return nil
		}
		d.waited = true
		d.waiting++
		ch := d.wake
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			p.mu.Lock()
			d.waiting--
			p.mu.Unlock()
			return ctx.Err()
		case <-ch:
		}
		p.mu.Lock()
		d.waiting--
		p.mu.Unlock()
	}
}

// release frees the room taken by acquire; n is the bytes received.
func (p *downloadPacer) release(dc int, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.dcLocked(dc)
	d.inflight--
	d.bytes += int64(n)
	if n > 0 {
		d.parts++
	}
	d.broadcast()
}

// flood records a FLOOD_WAIT Telegram answered a part of dc with.
func (p *downloadPacer) flood(dc int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	d := p.dcLocked(dc)
	d.floods++
	d.floodAt = append(d.floodAt, now)
	p.floodAt = append(p.floodAt, now)
}

// tick adapts every window to what the last interval brought.
func (p *downloadPacer) tick() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	secs := paceInterval.Seconds()
	if !p.last.IsZero() {
		secs = math.Max(1, now.Sub(p.last).Seconds())
	}
	p.last = now
	changed := false
	for _, d := range p.dcs {
		d.speed = float64(d.bytes) / secs
		d.history = append(d.history, d.speed)
		if len(d.history) > paceHistoryLen {
			d.history = d.history[len(d.history)-paceHistoryLen:]
		}
		before := d.window
		switch {
		case d.floods > 0:
			d.ceiling, d.ceilingUntil = d.window, now.Add(paceCeiling)
			d.window = max(paceMin, int(math.Floor(float64(d.window)*paceCut)))
			d.probing = false
			d.holdUntil = now.Add(paceFloodHold)
			d.state = "slowed: Telegram asked to wait"
		case d.probing:
			// The interval after one part more: keep it only if it paid.
			d.probing = false
			if d.speed >= d.probeBase*paceGain {
				d.state = "faster with more"
			} else {
				d.window = max(paceMin, d.window-1)
				d.holdUntil = now.Add(pacePlateau)
				d.state = "at its best"
			}
		case now.Before(d.holdUntil):
			// Holding after a flood or a try that brought nothing.
		case d.waited && now.Before(d.ceilingUntil) && d.window+1 >= d.ceiling:
			d.state = "just below Telegram's limit"
		case d.waited && d.window < paceMax && d.bytes > 0:
			d.probing = true
			d.probeBase = d.speed
			d.window++
			d.state = "trying more"
		case !d.waited:
			if d.bytes == 0 {
				d.state = "idle"
			} else {
				d.state = "not limiting"
			}
		}
		if d.window != before {
			changed = true
			d.broadcast()
		}
		d.bytes, d.parts, d.floods, d.waited = 0, 0, 0, false
		d.floodAt = trimTimes(d.floodAt, now.Add(-time.Hour))
	}
	p.floodAt = trimTimes(p.floodAt, now.Add(-24*time.Hour))
	if changed {
		p.saveLocked()
	}
}

func trimTimes(ts []time.Time, after time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(after) {
		i++
	}
	return ts[i:]
}

func (p *downloadPacer) saveLocked() {
	if p.path == "" {
		return
	}
	kept := map[string]int{}
	for dc, d := range p.dcs {
		kept[strconv.Itoa(dc)] = d.window
		p.saved[dc] = d.window
	}
	data, _ := json.Marshal(kept)
	tmp := p.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, p.path)
	}
}

// run adapts the windows until ctx ends.
func (p *downloadPacer) run(ctx context.Context) {
	t := time.NewTicker(paceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick()
		}
	}
}

// DCPace is one datacenter's figures.
type DCPace struct {
	DC       int     `json:"dc"`
	Window   int     `json:"window"`   // parts that may be in flight
	InFlight int     `json:"inFlight"` // parts in flight now
	Waiting  int     `json:"waiting"`  // requests waiting for room
	Speed    float64 `json:"speed"`    // bytes a second, last 10 s
	Speed5m  float64 `json:"speed5m"`  // bytes a second, last 5 minutes
	Floods1h int     `json:"floods1h"`
	State    string  `json:"state"`
}

// PaceStats is the account's download pacing.
type PaceStats struct {
	Speed      float64  `json:"speed"`   // bytes a second, all datacenters, last 10 s
	Speed5m    float64  `json:"speed5m"` // last 5 minutes
	Window     int      `json:"window"`  // parts in flight allowed, all datacenters
	Floods1h   int      `json:"floods1h"`
	Floods24h  int      `json:"floods24h"`
	FloodsHour [24]int  `json:"floodsByHour"` // [0] is the current hour
	DCs        []DCPace `json:"dcs"`
}

func (p *downloadPacer) stats() PaceStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var s PaceStats
	for _, d := range p.dcs {
		if now.Sub(d.lastUse) > 30*time.Minute && d.inflight == 0 {
			continue
		}
		avg := 0.0
		for _, v := range d.history {
			avg += v
		}
		if len(d.history) > 0 {
			avg /= float64(len(d.history))
		}
		x := DCPace{DC: d.dc, Window: d.window, InFlight: d.inflight, Waiting: d.waiting, Speed: d.speed, Speed5m: avg, Floods1h: len(trimTimes(d.floodAt, now.Add(-time.Hour))), State: d.state}
		s.DCs = append(s.DCs, x)
		s.Speed += x.Speed
		s.Speed5m += x.Speed5m
		s.Window += x.Window
	}
	sort.Slice(s.DCs, func(i, j int) bool { return s.DCs[i].DC < s.DCs[j].DC })
	for _, t := range p.floodAt {
		age := now.Sub(t)
		if age < time.Hour {
			s.Floods1h++
		}
		if age < 24*time.Hour {
			s.Floods24h++
			s.FloodsHour[int(age/time.Hour)]++
		}
	}
	return s
}

// suggestedWorkers is how many files at once keep the windows full: a
// small file takes one part, and every file also spends time being written
// to disk, so three files for each part allowed, and two more.
func (p *downloadPacer) suggestedWorkers() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	now := p.now()
	for _, d := range p.dcs {
		if now.Sub(d.lastUse) < 10*time.Minute {
			total += d.window
		}
	}
	if total == 0 {
		return 0 // nothing learned yet
	}
	return 3*total + 2
}
