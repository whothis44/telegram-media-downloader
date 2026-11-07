package app

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ix.12.7: the [process] line, so the cause of a busy app can be found (seen
// 11-05 22:40: 134 % of one core and ~530 MB/s read, never explained). A
// sample a minute of the process's CPU time and bytes read and written, as
// the system counts them (on Windows every read and write of the process,
// files and network alike; elsewhere what the process asked to read and
// write, cache included), kept for the last hour.

type ixProcSample struct {
	at          time.Time
	cpu         time.Duration // user + system
	read, write uint64
}

const ixProcKeep = 61 // samples: an hour at one a minute

var ixProcRing struct {
	mu      sync.Mutex
	first   ixProcSample
	samples []ixProcSample
}

// ixProcRecord takes a sample (ixProcNow is per system).
func ixProcRecord() {
	s, ok := ixProcNow()
	if !ok {
		return
	}
	ixProcRing.mu.Lock()
	defer ixProcRing.mu.Unlock()
	if ixProcRing.first.at.IsZero() {
		ixProcRing.first = s
	}
	ixProcRing.samples = append(ixProcRing.samples, s)
	if n := len(ixProcRing.samples); n > ixProcKeep {
		ixProcRing.samples = append([]ixProcSample(nil), ixProcRing.samples[n-ixProcKeep:]...)
	}
}

// ixRunProcess samples once a minute.
func (a *App) ixRunProcess(ctx context.Context) {
	ixProcRecord()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ixProcRecord()
		}
	}
}

// ixProcRate is CPU (% of one core) and MB read and written per hour
// between two samples.
func ixProcRate(a, b ixProcSample) (cpu, readMBh, writeMBh float64) {
	d := b.at.Sub(a.at)
	if d <= 0 {
		return 0, 0, 0
	}
	h := d.Hours()
	cpu = 100 * float64(b.cpu-a.cpu) / float64(d)
	return cpu, float64(b.read-a.read) / (1 << 20) / h, float64(b.write-a.write) / (1 << 20) / h
}

// ixProcessDiag is the report's [process] lines.
func ixProcessDiag(now time.Time) []string {
	cur, ok := ixProcNow()
	if !ok {
		return []string{"not measured on this system"}
	}
	ixProcRing.mu.Lock()
	first := ixProcRing.first
	samples := append([]ixProcSample(nil), ixProcRing.samples...)
	ixProcRing.mu.Unlock()
	out := []string{fmt.Sprintf("cores %d · CPU time %s", runtime.NumCPU(), cur.cpu.Round(time.Second))}
	if len(samples) == 0 || first.at.IsZero() {
		return append(out, "no samples yet")
	}
	// The last hour: from the oldest sample kept.
	c, r, w := ixProcRate(samples[0], cur)
	out = append(out, fmt.Sprintf("last %s: CPU %.0f %% of one core · read %.0f MB/h (%.1f MB/s) · written %.0f MB/h", cur.at.Sub(samples[0].at).Round(time.Minute), c, r, r/3600, w))
	c, r, w = ixProcRate(first, cur)
	out = append(out, fmt.Sprintf("since sampling began %s ago: CPU %.0f %% · read %.0f MB/h · written %.0f MB/h", now.Sub(first.at).Round(time.Minute), c, r, w))
	// Ten-minute steps, newest first, to see when it was busy.
	var steps []string
	end := cur
	for i := len(samples) - 1; i >= 0 && len(steps) < 6; i-- {
		if end.at.Sub(samples[i].at) >= 10*time.Minute || i == 0 {
			c, r, _ := ixProcRate(samples[i], end)
			steps = append(steps, fmt.Sprintf("%s %.0f%%/%.0f MB/s", samples[i].at.In(ixTZ).Format("15:04"), c, r/3600))
			end = samples[i]
		}
	}
	if len(steps) > 0 {
		out = append(out, "by 10 minutes (start: CPU/read): "+strings.Join(steps, " · "))
	}
	return out
}
