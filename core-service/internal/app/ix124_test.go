package app

import (
	"strings"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

func TestIxDCWaitsSaysAskedSecondsAndSameFile(t *testing.T) {
	h := telegram.PaceHour{DCPremium: map[int]int{1: 4, 4: 2}, DCPremiumSecs: map[int]int{1: 58, 4: 6}, DCPremiumSame: map[int]int{1: 3}}
	want := "DC1 4, 14.5 s asked on average, 3 for the same file as the wait before; DC4 2, 3.0 s asked on average, 0 for the same file as the wait before"
	if got := ixDCWaits(h); got != want {
		t.Fatalf("got %q", got)
	}
	if got := ixDCWaits(telegram.PaceHour{DCPremium: map[int]int{1: 3}}); got != "" {
		t.Fatalf("an hour recorded before ix.12.4: %q", got)
	}
}

func TestProfilePhotoReasonIsSaidOnceAnHour(t *testing.T) {
	var c dialogPhotoCache
	now := time.Unix(1800000000, 0)
	if say, _ := c.sayUnavailable("no dialog", now); !say {
		t.Fatal("the first time is said")
	}
	for i := 0; i < 117; i++ {
		if say, _ := c.sayUnavailable("no dialog", now.Add(time.Minute)); say {
			t.Fatal("said again within the hour")
		}
	}
	if say, _ := c.sayUnavailable("another reason", now.Add(time.Minute)); !say {
		t.Fatal("another reason is said")
	}
	if say, more := c.sayUnavailable("no dialog", now.Add(time.Hour)); !say || more != 117 {
		t.Fatalf("after the hour: %v %d", say, more)
	}
}

func TestMemorySectionNamesFunctionsOnly(t *testing.T) {
	keep := make([][]byte, 0, 64)
	for i := 0; i < 64; i++ {
		keep = append(keep, make([]byte, 1<<20))
	}
	lines := ixMemoryDiag()
	if len(lines) == 0 || !strings.Contains(lines[0], "heap in use") || !strings.Contains(lines[0], "goroutines") {
		t.Fatalf("%q", lines)
	}
	if len(lines) > 1 && !strings.Contains(lines[1], "by function (estimated)") {
		t.Fatalf("%q", lines)
	}
	_ = keep
}
