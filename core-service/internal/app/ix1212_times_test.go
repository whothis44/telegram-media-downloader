package app

import (
	"strings"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// ix.12.12: the report's hourly line says where each server's time went, and
// a download that took kept parts is counted and logged with sizes only.
func TestIx1212ServerTimesAndKeptUseAreReported(t *testing.T) {
	h := telegram.PaceHour{
		DCInflight: map[int]int{1: 360, 4: 100},
		DCOkMs:     map[int]int{1: 900_000, 4: 980_000},
		DCFailMs:   map[int]int{1: 2_000},
		DCFails:    map[int]int{1: 4},
		DCWaitedMs: map[int]int{1: 1_500_000},
		DCLimitMs:  map[int]int{1: 1_000},
		DCWaits:    map[int]int{1: 9},
		DCWaitSecs: map[int]int{1: 135},
	}
	got := ixDCTimes(h)
	want := "DC1 in flight 3600, answered 900, failed 2 (4 requests), waits 1500 (speed-limit sleeps 0), rate limit 1, other 1197, 9 plain waits asked 135; DC4 in flight 1000, answered 980, failed 0 (0 requests), waits 0 (speed-limit sleeps 0), rate limit 0, other 20"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if ixDCTimes(telegram.PaceHour{}) != "" {
		t.Fatal("a line for an hour with no times")
	}
	a, ctx := ixTestApp(t)
	a.ixKeptPartsUsed(telegram.MediaIdentity{Kind: "document", ID: "1", Size: 300 << 20}, 12<<20)
	if line := a.ixPartsDiag(ctx); !strings.Contains(line, "in 1 downloads") {
		t.Fatalf("report: %s", line)
	}
}
