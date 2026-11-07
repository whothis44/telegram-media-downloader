package app

import (
	"strings"
	"testing"
)

// ix.12.14: "Keep at least N GB free on this disk" replaces "Space it may
// use". Settings saved before keep what they did: an old space up to 50 GB
// stopped staging at about 50 GB free, a larger one at that much free.
func TestIx1214StageKeepFreeUpgradesTheOldSpace(t *testing.T) {
	for _, c := range []struct{ cap, keep, want int }{
		{20, 0, 50},   // the default before: stops at 50 GB free, as before
		{1, 0, 50},    // any space up to 50
		{50, 0, 50},   // the edge
		{250, 0, 250}, // a larger space: staging stopped at 250 GB free
		{250, 80, 80}, // set in 12.14: the new setting wins
		{20, 120, 120},
	} {
		s := ixDefaultSettings()
		s.StageCapGB, s.StageKeepFreeGB = c.cap, c.keep
		if got := s.StageKeepFree(); got != c.want {
			t.Errorf("space %d GB, keep %d GB: keeps %d GB free, want %d", c.cap, c.keep, got, c.want)
		}
	}
	if s := ixDefaultSettings(); s.StageKeepFree() != 50 {
		t.Fatalf("default keeps %d GB free", s.StageKeepFree())
	}
}

func TestIx1214StageKeepFreeIsParsed(t *testing.T) {
	s := ixDefaultSettings()
	p, err := ixParseSettings(map[string]any{"stageKeepFreeGb": "120"}, s)
	if err != nil || p.StageKeepFreeGB != 120 || p.StageKeepFree() != 120 {
		t.Fatalf("parsed %d (%v)", p.StageKeepFreeGB, err)
	}
	if p, err = ixParseSettings(map[string]any{"stageKeepFreeGb": float64(75)}, s); err != nil || p.StageKeepFree() != 75 {
		t.Fatalf("number: %d (%v)", p.StageKeepFreeGB, err)
	}
	if p, err = ixParseSettings(map[string]any{"stageKeepFreeGb": " "}, s); err != nil || p.StageKeepFree() != 50 {
		t.Fatalf("empty: %d (%v)", p.StageKeepFreeGB, err)
	}
	for _, bad := range []any{"lots", "0", "-5", "19", "100001", float64(0), float64(19)} {
		if _, err := ixParseSettings(map[string]any{"stageKeepFreeGb": bad}, s); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	// Saving other settings leaves an upgraded value as it was.
	s.StageCapGB = 250
	if p, err = ixParseSettings(map[string]any{"autoRetry": false}, s); err != nil || p.StageKeepFree() != 250 {
		t.Fatalf("other settings changed it: %d (%v)", p.StageKeepFree(), err)
	}
}

func TestIx1214StagingLineSaysWhatItKeepsFree(t *testing.T) {
	stage := t.TempDir()
	a, ctx := ix8App(t, map[string]any{"stageDir": stage, "stageKeepFreeGb": "20"})
	st := a.ixStagingStatus(ctx)
	if !st.On || st.KeepFreeGB != 20 || st.KeepFree != 20<<30 {
		t.Fatalf("status: %+v", st)
	}
	var line string
	for _, l := range a.ixTransferDiag(ctx, func(s string) string { return s }) {
		if strings.HasPrefix(l, "local staging") {
			line = l
		}
	}
	if !strings.HasPrefix(line, "local staging on (keeps 20 GB free on its disk)") {
		t.Fatalf("line: %s", line)
	}
	if st.RoomKnown && !strings.Contains(line, "more now") {
		t.Fatalf("room not shown: %s", line)
	}
}

// The kept parts give way above what staging keeps free (ix.12.14).
func TestIx1214PartsFloorFollowsStaging(t *testing.T) {
	a, ctx := ix8App(t, nil)
	if got := a.ixPartsFloor(ctx); got != ixPartsFreeFloor {
		t.Fatalf("staging off: %d GB", got>>30)
	}
	a2, ctx2 := ix8App(t, map[string]any{"stageDir": t.TempDir(), "stageKeepFreeGb": "120"})
	if got := a2.ixPartsFloor(ctx2); got != 130<<30 {
		t.Fatalf("keep 120: %d GB", got>>30)
	}
	a3, ctx3 := ix8App(t, map[string]any{"stageDir": t.TempDir(), "stageKeepFreeGb": "20"})
	if got := a3.ixPartsFloor(ctx3); got != ixPartsFreeFloor {
		t.Fatalf("keep 20: %d GB", got>>30)
	}
}
