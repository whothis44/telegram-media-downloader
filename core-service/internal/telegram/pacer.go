package telegram

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
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
//     and for ten minutes the window stays below the one that flooded. Floods
//     in the minute after a cut answer requests sent before it and are not
//     cut for again;
//   - when downloads keep waiting for room and nothing flooded, it tries a
//     different window for three intervals and compares their average speed
//     with the intervals before. A bigger window (one part more, or a quarter
//     more while tries keep paying in full and Telegram has never asked to
//     wait) is kept only if it brings at least half of what it could; a
//     smaller one is kept if it loses less than half of what it could. After
//     a bigger window brought nothing, the next try is a smaller one, so a
//     lucky measurement is taken back later rather than built on;
//   - when Telegram answers parts slowly (more than 8 s a part on average),
//     the window keeps only as many parts as it answers within 8 s at that
//     speed: Telegram may slow an account down instead of asking it to
//     wait, and more parts then only wait longer, until they time out;
//   - a FLOOD_PREMIUM_WAIT (Telegram's speed limit for accounts without
//     Premium) is waited out without changing the window;
//   - when nobody waits for room, the window stays as it is.
//
// The window learned for each datacenter is kept next to the session, so a
// restart starts from it rather than learning again.
//
// Pacing can be switched off (SetPacing): downloads then go as before ix.9,
// each file with its own share of parts and every flood closing the
// account's gate.

const (
	paceInterval   = 10 * time.Second
	paceStart      = 4   // parts in flight on a datacenter not seen before (TDLib's free tier is 2 MiB)
	paceMin        = 1   // never fewer
	paceMax        = 32  // never more: twice Telegram Desktop's 16 MiB; about 100 drew -429 on every socket (MadelineProto)
	paceGainShare  = 0.5 // a try is kept when it brings at least half the speed it could (step/window)
	paceCut        = 0.6
	paceFloodHold  = time.Minute
	pacePlateau    = 2 * time.Minute
	paceCeiling    = 10 * time.Minute // after a flood, stay below the window it happened at this long
	paceCeilingMin = 3                // the window under a flood's ceiling may still be 2
	paceTrial      = 3                // intervals a try is measured over
	paceBase       = 2                // intervals measured at a window before trying another
	paceHistoryLen = 30               // intervals kept for the 5-minute speed
	paceFullRecent = 2 * time.Minute  // windows full this recently say how many files are enough
	paceSlow       = 8 * time.Second  // parts answered slower than this on average: fewer in flight
	paceSlowMin    = 2                // the fewest a slow answer cuts to
)

var pacingOff atomic.Bool

// SetPacing switches the download pacer on or off for every account.
func SetPacing(on bool) { pacingOff.Store(!on) }

// Pacing reports whether downloads are paced.
func Pacing() bool { return !pacingOff.Load() }

type dcPace struct {
	dc       int
	window   int
	inflight int
	waiting  int
	wake     chan struct{}

	// This interval.
	bytes    int64
	parts    int
	floods   int
	premium  int // FLOOD_PREMIUM_WAITs
	waited   bool
	took     time.Duration // answered parts' time, summed
	answers  int
	lastTook time.Duration // average of the last interval with answers

	// Learning.
	steady       []float64 // speeds of the last full intervals at this window
	trial        int       // the step being tried (negative: fewer parts), 0 when none
	trialFrom    int
	trialBase    float64
	trialLeft    int
	trialSpeeds  []float64
	trialFull    bool
	nextDown     bool // the last bigger window brought nothing: try a smaller one next
	growFast     bool // the last try paid in full: try a bigger step next
	holdUntil    time.Time
	lastCut      time.Time
	ceiling      int // the window a flood happened at
	ceilingUntil time.Time
	fullAt       time.Time // the last interval downloads waited for room
	state        string

	speed     float64   // bytes a second, last interval
	history   []float64 // last intervals' speeds
	floodAt   []time.Time
	premiumAt []time.Time
	lastUse   time.Time
}

type downloadPacer struct {
	mu        sync.Mutex
	dcs       map[int]*dcPace
	path      string // where windows are kept ("" = not kept)
	last      time.Time
	floodAt   []time.Time // every flood of the account in the last day
	premiumAt []time.Time // every FLOOD_PREMIUM_WAIT in the last day
	now       func() time.Time
	saved     map[int]int
	running   atomic.Bool
	hours     []PaceHour // the last 48 hours (pacer_hours.go)
}

func newDownloadPacer(path string) *downloadPacer {
	p := &downloadPacer{dcs: map[int]*dcPace{}, path: path, now: time.Now, saved: map[int]int{}}
	p.loadHours()
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

// answered records how long Telegram took to answer a part of dc.
func (p *downloadPacer) answered(dc int, took time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.dcLocked(dc)
	d.took += took
	d.answers++
}

// flood records a wait Telegram answered a part of dc with. A
// FLOOD_PREMIUM_WAIT (premium) is its speed limit for accounts without
// Premium after tens of gigabytes: fewer parts in flight would not avoid it
// (the official apps do not cut for it either), so it does not cut the
// window; it pauses the tries, whose speeds it would distort.
func (p *downloadPacer) flood(dc int, premium bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	d := p.dcLocked(dc)
	p.hourWaitLocked(now, premium)
	if premium {
		d.premium++
		d.premiumAt = append(d.premiumAt, now)
		p.premiumAt = append(p.premiumAt, now)
		return
	}
	d.floods++
	d.floodAt = append(d.floodAt, now)
	p.floodAt = append(p.floodAt, now)
}

func meanOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func lastOf(xs []float64, n int) []float64 {
	if len(xs) > n {
		xs = xs[len(xs)-n:]
	}
	return append([]float64(nil), xs...)
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
	p.recordHourLocked(now, secs)
	for _, d := range p.dcs {
		d.speed = float64(d.bytes) / secs
		d.history = lastOf(append(d.history, d.speed), paceHistoryLen)
		if d.waited {
			d.fullAt = now
		}
		if d.answers > 0 {
			d.lastTook = d.took / time.Duration(d.answers)
		}
		before := d.window
		switch {
		case d.floods > 0 && now.Sub(d.lastCut) >= paceFloodHold:
			d.ceiling, d.ceilingUntil = max(paceCeilingMin, d.window), now.Add(paceCeiling)
			d.window = max(paceMin, int(math.Floor(float64(d.window)*paceCut)))
			d.trial, d.growFast, d.nextDown, d.steady = 0, false, false, nil
			d.holdUntil, d.lastCut = now.Add(paceFloodHold), now
			d.state = "slowed: Telegram asked to wait"
		case d.floods > 0:
			// Answers to parts asked for before the last cut.
		case d.answers > 0 && d.took/time.Duration(d.answers) > paceSlow && d.window > paceSlowMin && now.Sub(d.lastCut) >= paceFloodHold:
			// Telegram answers slowly (it may slow an account down rather
			// than ask it to wait). Parts in flight are speed × time per
			// part, so keep only as many as are answered within paceSlow at
			// this speed: no slower, and nothing left waiting long enough to
			// time out.
			lat := d.took / time.Duration(d.answers)
			if d.trial != 0 {
				d.window, d.trial = d.trialFrom, 0
			}
			d.window = max(paceSlowMin, int(float64(d.window)*paceSlow.Seconds()/lat.Seconds()))
			d.growFast, d.nextDown, d.steady = false, false, nil
			d.holdUntil, d.lastCut = now.Add(paceFloodHold), now
			d.state = "slowed: Telegram answers slowly"
		case d.premium > 0:
			// Telegram's speed limit for free accounts: keep the window,
			// take back a try (its speed says nothing now) and try nothing
			// while it lasts.
			if d.trial != 0 {
				d.window, d.trial = d.trialFrom, 0
			}
			d.steady = nil
			if until := now.Add(paceFloodHold); until.After(d.holdUntil) {
				d.holdUntil = until
			}
			d.state = "slowed: Telegram's speed limit for accounts without Premium"
		case d.trial != 0:
			d.trialSpeeds = append(d.trialSpeeds, d.speed)
			d.trialFull = d.trialFull || d.waited
			if d.trialLeft--; d.trialLeft > 0 {
				break
			}
			d.judgeTrial(now)
		case now.Before(d.holdUntil):
			// Holding after a flood or a try that brought nothing.
		case !d.waited:
			if d.bytes == 0 {
				d.state = "idle"
			} else {
				d.state = "not limiting"
			}
		default:
			d.steady = lastOf(append(d.steady, d.speed), paceTrial)
			if len(d.steady) < paceBase {
				d.state = "measuring"
				break
			}
			step := 0
			switch {
			case d.nextDown && d.window > paceMin:
				step = -1
			case now.Before(d.ceilingUntil) && d.window+1 >= d.ceiling:
				d.state = "just below Telegram's limit"
			case d.window >= paceMax:
				d.state = "at the most it may ask for"
			default:
				step = 1
				if d.growFast && d.ceiling == 0 {
					step = max(1, d.window/4)
				}
				if now.Before(d.ceilingUntil) {
					step = min(step, d.ceiling-1-d.window)
				}
				step = min(step, paceMax-d.window)
			}
			if step != 0 {
				d.trial, d.trialFrom, d.trialBase = step, d.window, meanOf(d.steady)
				d.trialLeft, d.trialSpeeds, d.trialFull = paceTrial, nil, false
				d.window += step
				if step > 0 {
					d.state = "trying more"
				} else {
					d.state = "trying fewer"
				}
			}
		}
		if d.window != before {
			changed = true
			d.broadcast()
		}
		d.bytes, d.parts, d.floods, d.premium, d.waited = 0, 0, 0, 0, false
		d.took, d.answers = 0, 0
		d.floodAt = trimTimes(d.floodAt, now.Add(-time.Hour))
		d.premiumAt = trimTimes(d.premiumAt, now.Add(-time.Hour))
	}
	p.floodAt = trimTimes(p.floodAt, now.Add(-24*time.Hour))
	p.premiumAt = trimTimes(p.premiumAt, now.Add(-24*time.Hour))
	if changed {
		p.saveLocked()
	}
}

// judgeTrial keeps or takes back the window tried over the last intervals.
// Going from w to w+s parts could at best change the speed by s/w; a try
// counts when it brings (or, for fewer parts, loses) at least half that.
func (d *dcPace) judgeTrial(now time.Time) {
	step, from := d.trial, d.trialFrom
	d.trial = 0
	speed := meanOf(d.trialSpeeds)
	could := math.Abs(float64(step)) / float64(max(1, from))
	change := 0.0
	if d.trialBase > 0 {
		change = speed/d.trialBase - 1
	}
	switch {
	case !d.trialFull:
		// Nobody waited for room: the try says nothing.
		d.window, d.state = from, "not limiting"
	case step > 0 && change >= paceGainShare*could:
		d.steady = lastOf(d.trialSpeeds, paceTrial)
		d.growFast = change >= 0.8*could
		d.nextDown = false
		d.state = "faster with more"
	case step > 0:
		d.window = from
		d.holdUntil = now.Add(pacePlateau)
		d.growFast, d.nextDown = false, true
		d.state = "at its best"
	case -change < paceGainShare*could:
		// As fast with fewer: keep fewer, and see whether fewer still do.
		d.steady = lastOf(d.trialSpeeds, paceTrial)
		d.growFast = false
		d.state = "as fast with fewer"
	default:
		d.window = from
		d.holdUntil = now.Add(pacePlateau)
		d.nextDown = false
		d.state = "at its best"
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
	if !p.running.CompareAndSwap(false, true) {
		return // already adapting
	}
	defer p.running.Store(false)
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
	DC        int     `json:"dc"`
	Window    int     `json:"window"`   // parts that may be in flight
	InFlight  int     `json:"inFlight"` // parts in flight now
	Waiting   int     `json:"waiting"`  // requests waiting for room
	Speed     float64 `json:"speed"`    // bytes a second, last 10 s
	Speed5m   float64 `json:"speed5m"`  // bytes a second, last 5 minutes
	Floods1h  int     `json:"floods1h"`
	Premium1h int     `json:"premium1h"` // FLOOD_PREMIUM_WAITs in the last hour
	PartSecs  float64 `json:"partSecs"`  // seconds Telegram took per part, last interval
	State     string  `json:"state"`
}

// PaceStats is the account's download pacing.
type PaceStats struct {
	Off       bool    `json:"off"`     // pacing is switched off
	Speed     float64 `json:"speed"`   // bytes a second, all datacenters, last 10 s
	Speed5m   float64 `json:"speed5m"` // last 5 minutes
	Window    int     `json:"window"`  // parts in flight allowed, all datacenters
	Floods1h  int     `json:"floods1h"`
	Floods24h int     `json:"floods24h"`
	// Premium waits: Telegram's speed limit for accounts without Premium.
	Premium1h  int        `json:"premium1h"`
	Premium24h int        `json:"premium24h"`
	Premium10m int        `json:"premium10m"` // the last 10 minutes: is a limit what sets the pace now
	Floods10m  int        `json:"floods10m"`
	FloodsHour [24]int    `json:"floodsByHour"` // [0] is the current hour
	Hours      []PaceHour `json:"hours"`        // the last 48 hours, newest first
	DCs        []DCPace   `json:"dcs"`
}

func (p *downloadPacer) stats() PaceStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	s := PaceStats{Off: !Pacing()}
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
		x := DCPace{PartSecs: d.lastTook.Seconds(), DC: d.dc, Window: d.window, InFlight: d.inflight, Waiting: d.waiting, Speed: d.speed, Speed5m: avg, Floods1h: len(trimTimes(d.floodAt, now.Add(-time.Hour))), Premium1h: len(trimTimes(d.premiumAt, now.Add(-time.Hour))), State: d.state}
		s.DCs = append(s.DCs, x)
		s.Speed += x.Speed
		s.Speed5m += x.Speed5m
		s.Window += x.Window
	}
	sort.Slice(s.DCs, func(i, j int) bool { return s.DCs[i].DC < s.DCs[j].DC })
	for _, t := range p.premiumAt {
		if age := now.Sub(t); age < time.Hour {
			s.Premium1h++
		}
		if now.Sub(t) < 10*time.Minute {
			s.Premium10m++
		}
		if now.Sub(t) < 24*time.Hour {
			s.Premium24h++
		}
	}
	for _, t := range p.floodAt {
		age := now.Sub(t)
		if age < time.Hour {
			s.Floods1h++
		}
		if age < 10*time.Minute {
			s.Floods10m++
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
//
// It is 0 (no suggestion) while no window has been full lately: then the
// windows are not what limits the account, and more files may help.
func (p *downloadPacer) suggestedWorkers() int {
	if !Pacing() {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	total, full := 0, false
	now := p.now()
	for _, d := range p.dcs {
		if now.Sub(d.lastUse) < 10*time.Minute {
			total += d.window
			full = full || now.Sub(d.fullAt) < paceFullRecent
		}
	}
	if total == 0 || !full {
		return 0
	}
	return 3*total + 2
}
