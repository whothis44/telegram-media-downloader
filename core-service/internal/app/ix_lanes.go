package app

import (
	"context"
	"time"
)

// ix.12.14: what the Rescan and Priorities tabs say about the two rescan
// lanes. Each lane runs one group at a time; the next is the first waiting
// group in the order ixQueueStep uses (ranked place, then ★, then the
// deletion triage). The pages used to say "messages: waiting" and "messages
// still being read" with no hint that a group waited behind another, or
// that the one ahead had stopped making progress (11-07: 19 hours).

// ixLaneHolder is the group running in a lane.
type ixLaneHolder struct {
	GroupID string `json:"groupId"`
	Name    string `json:"name"`
	Since   int64  `json:"since"`    // when its pass started (unix ms)
	LastWay int64  `json:"lastWork"` // its last sign of work seen by this process (unix ms)
	Place   int    `json:"place,omitempty"`
}

// ixLaneInfo is both lanes, and for each waiting group how many come before
// it in its lane.
type ixLaneInfo struct {
	Messages *ixLaneHolder `json:"messages,omitempty"`
	Files    *ixLaneHolder `json:"files,omitempty"`
	// Ahead: waiting groups that may start now -> groups before them in
	// their lane (0: next). Groups waiting to retry are left out.
	Ahead map[string]int `json:"ahead"`
	// Places: ranked groups -> their place in the ranking (1 = first).
	Places     map[string]int `json:"places"`
	StallAfter int64          `json:"stallAfter"` // ms without a sign of work before a pass is stopped
}

// ixLaneLess is the order in which waiting groups take a lane: ranked place
// first (unranked after every ranked group), then ★, then the triage.
func ixLaneLess(st *ixQueueState, places, triage map[string]int) func(i, j int) bool {
	place := func(groupID string) int {
		if p, ok := places[groupID]; ok {
			return p
		}
		return 1 << 30
	}
	return func(i, j int) bool {
		if a, b := place(st.Items[i].GroupID), place(st.Items[j].GroupID); a != b {
			return a < b
		}
		if st.Items[i].Priority != st.Items[j].Priority {
			return st.Items[i].Priority
		}
		return triage[st.Items[i].GroupID] < triage[st.Items[j].GroupID]
	}
}

// ixQueueLanes describes the lanes of st now.
func (a *App) ixQueueLanes(ctx context.Context, st ixQueueState) ixLaneInfo {
	info := ixLaneInfo{Ahead: map[string]int{}, Places: map[string]int{}, StallAfter: ixPassStallAfter.Milliseconds()}
	if ranks, err := a.ixRanks(ctx); err == nil {
		for i, g := range ranks {
			info.Places[g.GroupID] = i + 1
		}
	}
	bases := a.ixRankBases(ctx)
	var triage map[string]int
	if len(st.Items) > 0 {
		triage = a.ixTriage(ctx)
	}
	less := ixLaneLess(&st, bases, triage)
	rt := a.ix()
	rt.mu.Lock()
	for _, it := range st.Items {
		if it.State != "running" {
			continue
		}
		h := &ixLaneHolder{GroupID: it.GroupID, Name: it.Name, Since: it.StartedAt, Place: info.Places[it.GroupID]}
		h.LastWay = max(rt.beats[it.GroupID], rt.passSeen[it.JobID])
		if h.LastWay == 0 {
			h.LastWay = it.StartedAt
		}
		if it.Phase == ixPhaseFiles {
			info.Files = h
		} else {
			info.Messages = h
		}
	}
	rt.mu.Unlock()
	now := time.Now().UnixMilli()
	for i, it := range st.Items {
		if it.State != "waiting" || it.NextAt > now {
			continue
		}
		n := 0
		for j, other := range st.Items {
			if j == i || other.State != "waiting" || other.NextAt > now || (other.Phase == ixPhaseFiles) != (it.Phase == ixPhaseFiles) {
				continue
			}
			if less(j, i) {
				n++
			}
		}
		info.Ahead[it.GroupID] = n
	}
	return info
}
