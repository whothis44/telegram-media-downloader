package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http/httptrace"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// How many files each kind of recognition work has in flight at once,
// found by measuring rather than set by hand (the same ideas as the
// download pacer, internal/telegram/pacer.go).
//
// A task starts at its par (clip 4, faces 3, text 3, speech 1, frames 2).
// Every interval of about 30 s while it has more files waiting than it
// runs, its speed is measured: answers from the service per second (one
// answer is one picture or frame looked at, or one recording written out,
// so a 12-frame video counts for its work, not as one file). Then:
//
//   - after two intervals at a window, one more file at once is tried for
//     three intervals. Going from w to w+1 can at best bring 1/w more; the
//     try is kept when it brings at least half of that;
//   - a try that brought nothing is taken back and held for a few
//     intervals, and the next try is one fewer, so a lucky measurement is
//     taken back later rather than built on. Fewer is kept when it loses
//     less than half of what it could;
//   - an error of the service itself, or a timeout, cuts the window to 60%
//     at once and holds it there (once: files already in flight that fail
//     the same way do not cut it again).
//
// Intervals in which the task ran out of files are not counted.

const (
	ixMLParMin        = 1
	ixMLParMax        = 16
	ixMLPaceGainShare = 0.5 // a try is kept when it brings at least half of what it could
	ixMLPaceTrial     = 3   // intervals a try is measured over
	ixMLPaceBase      = 2   // intervals measured at a window before trying another
	ixMLPaceHold      = 4   // intervals to stay after a try that brought nothing
	ixMLPaceCut       = 0.6 // on an error of the service or a timeout
)

// ixMLPaceInterval is how long one measurement lasts. Replaceable in tests.
var ixMLPaceInterval = 30 * time.Second

// ---------- where the time goes ----------

// Stages of a file, timed cheaply: summed per task, shown per file in the
// diagnostics report.
const (
	ixStRead   = iota // reading the file or its thumbnail (disk or NAS)
	ixStFFmpeg        // frames or sound out of videos
	ixStJPEG          // decoding pictures and making face crops
	ixStUpload        // connecting and sending the request
	ixStServer        // waiting for the service to answer (its own work)
	ixStAnswer        // reading the answer
	ixStFile          // the whole file, start to end
	ixStages
)

var ixStageNames = [ixStages]string{"read", "ffmpeg", "jpeg", "upload", "server", "answer", "whole"}

type ixMLStats struct {
	ns      [ixStages]atomic.Int64
	files   atomic.Int64 // files finished
	answers atomic.Int64 // answers received from the service
}

type ixMLStatsKey struct{}

func ixMLWithStats(ctx context.Context, s *ixMLStats) context.Context {
	return context.WithValue(ctx, ixMLStatsKey{}, s)
}

func ixMLStatsOf(ctx context.Context) *ixMLStats {
	s, _ := ctx.Value(ixMLStatsKey{}).(*ixMLStats)
	return s
}

// ixMLClock starts timing a stage of the file being done; call what it
// returns at the end. Nothing is timed outside the recognition work.
func ixMLClock(ctx context.Context, stage int) func() {
	s := ixMLStatsOf(ctx)
	if s == nil {
		return func() {}
	}
	start := time.Now()
	return func() { s.ns[stage].Add(int64(time.Since(start))) }
}

// ixMLTraced times a request to the service: sending it, then waiting for
// the first byte of the answer.
func ixMLTraced(ctx context.Context) context.Context {
	s := ixMLStatsOf(ctx)
	if s == nil {
		return ctx
	}
	start := time.Now()
	var wrote atomic.Int64
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) {
			now := time.Now()
			wrote.Store(now.UnixNano())
			s.ns[ixStUpload].Add(int64(now.Sub(start)))
		},
		GotFirstResponseByte: func() {
			if w := wrote.Load(); w > 0 {
				s.ns[ixStServer].Add(time.Now().UnixNano() - w)
			}
			s.answers.Add(1)
		},
	})
}

// ---------- the pace of one task ----------

type ixMLPace struct {
	mu       sync.Mutex
	stats    *ixMLStats
	window   int
	inflight int

	// This interval.
	start   time.Time
	answers int64 // stats.answers at its start
	starved bool  // the task ran out of files

	// Learning.
	steady      []float64 // speeds of the last full intervals at this window
	trial       int       // the step being tried (negative: fewer), 0 when none
	trialFrom   int
	trialBase   float64
	trialLeft   int
	trialSpeeds []float64
	nextDown    bool // the last try of more brought nothing: try fewer next
	hold        int  // intervals left to stay as it is
	state       string
	speed       float64 // answers a second, last full interval
	lastCut     time.Time
}

type ixMLPaces struct {
	mu sync.Mutex
	m  map[string]*ixMLPace
}

var ixMLPaceState sync.Map // *App -> *ixMLPaces

// ixMLPaceOf is the pace of a task, kept while the program runs.
func (a *App) ixMLPaceOf(t *ixMLTask) *ixMLPace {
	v, _ := ixMLPaceState.LoadOrStore(a, &ixMLPaces{m: map[string]*ixMLPace{}})
	ps := v.(*ixMLPaces)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p := ps.m[t.name]
	if p == nil {
		p = &ixMLPace{stats: &ixMLStats{}, window: max(ixMLParMin, min(ixMLParMax, t.par)), state: "measuring"}
		ps.m[t.name] = p
	}
	return p
}

func (p *ixMLPace) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.window
}

// begin starts measuring (a round of the task starts).
func (p *ixMLPace) begin(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.start, p.answers, p.starved = now, p.stats.answers.Load(), false
}

func (p *ixMLPace) launched(d int) {
	p.mu.Lock()
	p.inflight += d
	p.mu.Unlock()
}

// starve: the task had fewer files to do than it could run.
func (p *ixMLPace) starve() {
	p.mu.Lock()
	p.starved = true
	p.mu.Unlock()
}

// ixMLPaceFault tells an error of the service, or a timeout, from a file's
// own failure.
func ixMLPaceFault(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var down ixMLUnreachable
	var ne net.Error
	return errors.As(err, &down) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// finished: a file started at started ended, with err (nil when it
// worked). Failures of files started before the last cut were already cut
// for.
func (p *ixMLPace) finished(ctx context.Context, started, now time.Time, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inflight--
	if ixMLPaceFault(ctx, err) {
		if started.Before(p.lastCut) {
			return
		}
		p.cutLocked()
		p.lastCut = now
		p.start, p.answers, p.starved = now, p.stats.answers.Load(), false
		return
	}
	secs := now.Sub(p.start).Seconds()
	if secs < ixMLPaceInterval.Seconds() {
		return
	}
	got := p.stats.answers.Load()
	full := !p.starved
	speed := float64(got-p.answers) / secs
	p.start, p.answers, p.starved = now, got, false
	if full {
		p.judgeLocked(speed)
	}
}

// cutLocked backs off at once.
func (p *ixMLPace) cutLocked() {
	from := p.window
	if p.trial != 0 {
		from = p.trialFrom
	}
	p.window = max(ixMLParMin, int(math.Floor(float64(from)*ixMLPaceCut)))
	p.trial, p.nextDown, p.steady = 0, false, nil
	p.hold = ixMLPaceHold
	p.state = "slowed: the service failed or timed out"
}

// judgeLocked adapts the window to one full interval's speed.
func (p *ixMLPace) judgeLocked(speed float64) {
	p.speed = speed
	switch {
	case p.trial != 0:
		p.trialSpeeds = append(p.trialSpeeds, speed)
		if p.trialLeft--; p.trialLeft > 0 {
			return
		}
		p.judgeTrialLocked()
	case p.hold > 0:
		p.hold--
	default:
		p.steady = lastOf(append(p.steady, speed), ixMLPaceTrial)
		if len(p.steady) < ixMLPaceBase {
			p.state = "measuring"
			return
		}
		step := 0
		switch {
		case p.nextDown && p.window > ixMLParMin:
			step = -1
		case p.window < ixMLParMax:
			step = 1
		default:
			p.state = "at the most it may send"
		}
		if step != 0 {
			p.trial, p.trialFrom, p.trialBase = step, p.window, meanOf(p.steady)
			p.trialLeft, p.trialSpeeds = ixMLPaceTrial, nil
			p.window += step
			p.state = map[bool]string{true: "trying more", false: "trying fewer"}[step > 0]
		}
	}
}

func (p *ixMLPace) judgeTrialLocked() {
	step, from := p.trial, p.trialFrom
	p.trial = 0
	speed := meanOf(p.trialSpeeds)
	could := math.Abs(float64(step)) / float64(max(1, from))
	change := 0.0
	if p.trialBase > 0 {
		change = speed/p.trialBase - 1
	}
	switch {
	case step > 0 && change >= ixMLPaceGainShare*could:
		p.steady = lastOf(p.trialSpeeds, ixMLPaceTrial)
		p.nextDown = false
		p.state = "faster with more"
	case step > 0:
		p.window, p.hold, p.nextDown = from, ixMLPaceHold, true
		p.state = "at its best"
	case -change < ixMLPaceGainShare*could:
		// As fast with fewer: keep fewer, and see whether fewer still do.
		p.steady = lastOf(p.trialSpeeds, ixMLPaceTrial)
		p.state = "as fast with fewer"
	default:
		p.window, p.hold, p.nextDown = from, ixMLPaceHold, false
		p.state = "at its best"
	}
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

// ---------- reporting ----------

// ixMLPars is how many files each task may have in flight now.
func (a *App) ixMLPars() map[string]int {
	out := map[string]int{}
	v, ok := ixMLPaceState.Load(a)
	if !ok {
		return out
	}
	ps := v.(*ixMLPaces)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for name, p := range ps.m {
		out[name] = p.size()
	}
	return out
}

// ixMLPaceReport is two lines for the diagnostics report: files at once
// per task, and where the time of a file goes (milliseconds per file, since
// the program started). Numbers only.
func (a *App) ixMLPaceReport() (atOnce, timing string) {
	v, ok := ixMLPaceState.Load(a)
	if !ok {
		return "not started yet", "nothing done yet"
	}
	ps := v.(*ixMLPaces)
	ps.mu.Lock()
	names := make([]string, 0, len(ps.m))
	for name := range ps.m {
		names = append(names, name)
	}
	sort.Strings(names)
	paces := make([]*ixMLPace, len(names))
	for i, name := range names {
		paces[i] = ps.m[name]
	}
	ps.mu.Unlock()
	var once, times []string
	for i, p := range paces {
		p.mu.Lock()
		once = append(once, fmt.Sprintf("%s %d (%s, %.1f answers/s)", names[i], p.window, p.state, p.speed))
		p.mu.Unlock()
		files := p.stats.files.Load()
		if files == 0 {
			continue
		}
		parts := []string{fmt.Sprintf("%s %d files:", names[i], files)}
		for st := 0; st < ixStages; st++ {
			parts = append(parts, fmt.Sprintf("%s %.0f", ixStageNames[st], float64(p.stats.ns[st].Load())/float64(files)/1e6))
		}
		times = append(times, strings.Join(parts, " "))
	}
	if len(times) == 0 {
		times = []string{"nothing done yet"}
	}
	return strings.Join(once, " · "), strings.Join(times, " | ")
}
