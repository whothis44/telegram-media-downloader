package app

import (
	"strings"
	"testing"
	"time"
)

// ix.12.14: the lanes as the pages show them: who holds each, and how many
// waiting groups come before each one in its lane, in the queue's own order
// (ranked place first, unranked after; groups waiting to retry left out).
func TestIx1214QueueLanesSayWhoIsAhead(t *testing.T) {
	a, ctx := ixTestApp(t)
	if err := a.ixSetRanks(ctx, []string{"r1", "r2", "r3"}, map[string]string{"r1": "R1", "r2": "R2", "r3": "R3"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	st := ixQueueState{Running: true, Items: []ixQueueItem{
		{GroupID: "u1", Name: "U1", State: "waiting", Phase: ixPhaseMessages},
		{GroupID: "r3", Name: "R3", State: "waiting", Phase: ixPhaseMessages},
		{GroupID: "r1", Name: "R1", State: "running", Phase: ixPhaseMessages, StartedAt: now - 1000, JobID: "j1"},
		{GroupID: "r2", Name: "R2", State: "waiting", Phase: ixPhaseMessages},
		{GroupID: "f", Name: "F", State: "waiting", Phase: ixPhaseFiles},
		{GroupID: "later", Name: "L", State: "waiting", Phase: ixPhaseMessages, NextAt: now + 60000},
	}}
	a.ixBeat("r1")
	lanes := a.ixQueueLanes(ctx, st)
	if lanes.Messages == nil || lanes.Messages.GroupID != "r1" || lanes.Messages.Place != 1 || lanes.Messages.LastWay < now-1000 || lanes.Files != nil {
		t.Fatalf("holders: %+v %+v", lanes.Messages, lanes.Files)
	}
	want := map[string]int{"r2": 0, "r3": 1, "u1": 2, "f": 0}
	for g, n := range want {
		if got, ok := lanes.Ahead[g]; !ok || got != n {
			t.Errorf("%s: %d ahead (%v), want %d", g, got, ok, n)
		}
	}
	if _, ok := lanes.Ahead["later"]; ok {
		t.Error("a group waiting to retry was counted")
	}
	if lanes.Places["r3"] != 3 || lanes.Places["u1"] != 0 {
		t.Fatalf("places %v", lanes.Places)
	}
	// What Priorities says (no group name: it is in the report too).
	if s := ixLaneWait(st.Items[3], lanes); s != "next in the messages lane, after the group in it now" {
		t.Fatalf("r2: %q", s)
	}
	if s := ixLaneWait(st.Items[0], lanes); s != "waiting for the messages lane: 2 groups before it" {
		t.Fatalf("u1: %q", s)
	}
	if s := ixLaneWait(st.Items[5], lanes); !strings.Contains(s, "again") {
		t.Fatalf("later: %q", s)
	}
	c := &ixGroupCoverage{}
	if s := ixCoverageWaiting(c, st.Items[1], "", lanes); s != "nothing missing so far: waiting for the messages lane: 1 group before it" {
		t.Fatalf("coverage r3: %q", s)
	}
	if s := ixCoverageWaiting(c, st.Items[2], "", lanes); !strings.HasPrefix(s, "reading its messages now") {
		t.Fatalf("coverage r1: %q", s)
	}
}
