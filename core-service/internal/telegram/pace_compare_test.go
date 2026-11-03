package telegram

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// A rough model of Telegram serving one account's downloads, second by
// second, to compare the engine before ix.9 with the pacer. It is a model:
// Telegram does not publish its limits. Telegram answers FLOOD_WAIT_wait
// whenever more than limit parts are in flight, and then nothing moves for
// wait+1 seconds; otherwise each part in flight brings partRate bytes a
// second, up to line parts (the connection, disk or Telegram's per-request
// speed: past it, more parts do not go faster).
type teleModel struct {
	name  string
	limit int     // parts in flight past which Telegram floods
	line  int     // parts in flight past which it does not get faster
	wait  int     // seconds Telegram asks to wait
	noise float64 // each 10 s, the speed varies by up to this share
}

const simPartRate = 300_000.0 // bytes a second for one part in flight

// simOld is the engine before ix.9: files at once start at 10 and grow by 3
// every 5 s to 20 while the queue is long; a flood stops everything for the
// wait, drops files at once to 3 and halves the parts per file (4 at most,
// one back after about 8 files without a flood). An average file is 5 parts.
func simOld(m teleModel, seconds int) (bytes float64, floods int) {
	rnd := rand.New(rand.NewSource(11))
	factor := 1.0
	files, threads, cleanParts, stall := 10, 4, 0, 0
	for t := 0; t < seconds; t++ {
		if t%10 == 9 {
			factor = 1 + m.noise*(2*rnd.Float64()-1)
		}
		if stall > 0 {
			stall--
			continue
		}
		inflight := files * min(threads, 5)
		if inflight > m.limit {
			floods++
			stall = m.wait + 1
			files, threads, cleanParts = 3, max(1, threads/2), 0
			continue
		}
		bytes += float64(min(inflight, m.line)) * simPartRate * factor
		cleanParts += min(inflight, m.line)
		if cleanParts >= 40 && threads < 4 {
			threads++
			cleanParts = 0
		}
		if t%5 == 4 && files < 20 {
			files = min(20, files+3)
		}
	}
	return bytes, floods
}

// simNew is the pacer as built, driven by the same model.
func simNew(m teleModel, seconds int, start int) (bytes float64, floods int, finalWindow int) {
	rnd := rand.New(rand.NewSource(11))
	factor := 1.0
	p := newDownloadPacer("")
	clock := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return clock }
	p.mu.Lock()
	p.dcLocked(4).window = start
	p.mu.Unlock()
	stall := 0
	for t := 0; t < seconds; t++ {
		clock = clock.Add(time.Second)
		p.mu.Lock()
		d := p.dcLocked(4)
		w := d.window
		d.lastUse = clock
		d.waited = true // the queue is long
		switch {
		case stall > 0:
			stall--
		case w > m.limit:
			floods++
			d.floods++
			stall = m.wait + 1
		default:
			b := float64(min(w, m.line)) * simPartRate * factor
			d.bytes += int64(b)
			bytes += b
		}
		p.mu.Unlock()
		if t%10 == 9 {
			p.tick()
			factor = 1 + m.noise*(2*rnd.Float64()-1)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return bytes, floods, p.dcLocked(4).window
}

func TestPacerAgainstTheOldEngineInAModelOfTelegram(t *testing.T) {
	models := []teleModel{
		{"strict (Telegram allows 4)", 4, 40, 16, 0},
		{"like the report (allows 8)", 8, 40, 16, 0},
		{"allows 16", 16, 40, 16, 0},
		{"lenient (allows 60), line tops at 12", 60, 12, 16, 0},
		{"allows 8, long waits (60 s)", 8, 40, 60, 0},
		{"no limit in reach (allows 200, line 100)", 200, 100, 16, 0},
		{"allows 8, speed varies ±15%", 8, 40, 16, 0.15},
		{"line tops at 6, speed varies ±15%", 200, 6, 16, 0.15},
	}
	const hours = 6
	for _, m := range models {
		ob, of := simOld(m, hours*3600)
		nb, nf, w := simNew(m, hours*3600, paceStart)
		t.Logf("%-38s old %6.2f MB/s %4d waits | new %6.2f MB/s %4d waits (window %d) | %+.0f%%",
			m.name, ob/hours/3600/1e6, of, nb/hours/3600/1e6, nf, w, (nb/ob-1)*100)
		_ = nb
	}
	_ = fmt.Sprint()
	// No assertion: Telegram's real limits are not known, so this only shows
	// how each engine behaves under each guess. The pacer's real decisions
	// come from measured speed, not from this model.
}
