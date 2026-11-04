package telegram

import (
	"testing"
	"time"
)

// The last 10 minutes' waits, which the download state reads to tell
// "Telegram sets the pace" from "stalled".
func TestPacerCountsTheLastTenMinutesWaits(t *testing.T) {
	p := newDownloadPacer("")
	clock := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return clock }
	p.flood(4, true)  // a speed-limit wait, 25 minutes before the look
	p.flood(4, false) // an ordinary wait, as old
	clock = clock.Add(20 * time.Minute)
	p.flood(4, true)
	p.flood(2, true)
	p.flood(2, false)
	clock = clock.Add(5 * time.Minute)
	st := p.stats()
	if st.Premium10m != 2 || st.Floods10m != 1 || st.Premium1h != 3 || st.Floods1h != 2 {
		t.Fatalf("stats %+v", st)
	}
	clock = clock.Add(6 * time.Minute)
	if st = p.stats(); st.Premium10m != 0 || st.Floods10m != 0 || st.Premium1h != 3 {
		t.Fatalf("stats after 11 minutes %+v", st)
	}
}
