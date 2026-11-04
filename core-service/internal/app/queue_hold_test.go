package app

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The queue snapshot tells the items the archive set aside (with why) from
// those a person paused, and counts the two apart; "resume all" without
// includeArchive lifts only the pauses people made.
func TestQueueSnapshotTellsArchiveHoldsFromPauses(t *testing.T) {
	a, ctx := ixTestApp(t)
	const g = "-1009000000001" // invented chat
	deferred := ixTestWork(t, a, "acc", g, 11, "video", 600<<20, 1)
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_deferred(work_id,band,deferred_at,released_at) VALUES(?,3,1,0)`, deferred); err != nil {
		t.Fatal(err)
	}
	copyOf := ixTestWork(t, a, "acc", g, 12, "video", 20<<20, 1)
	ixTestHold(t, a, copyOf, "", 0)
	orig := ixTestWork(t, a, "acc", g, 13, "video", 300<<20, 1)
	ixTestLowq(t, a, orig)
	person := ixTestWork(t, a, "acc", g, 14, "photo", 1<<20, 1)
	// Let go by the archive earlier, paused by a person since: theirs.
	released := ixTestWork(t, a, "acc", g, 15, "photo", 1<<20, 1)
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_deferred(work_id,band,deferred_at,released_at) VALUES(?,3,1,5)`, released); err != nil {
		t.Fatal(err)
	}
	running := ixTestWork(t, a, "acc", g, 16, "photo", 1<<20, 0)
	_ = running

	token, err := a.sessions.Create(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	rec := requestPurge(a, token, http.MethodGet, "/api/queue/snapshot", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot: %d %s", rec.Code, rec.Body.String())
	}
	var snap struct {
		Queued []struct {
			Key         string  `json:"key"`
			Status      string  `json:"status"`
			ArchiveHold *string `json:"archiveHold"`
		} `json:"queued"`
		ArchiveHeldCount  int `json:"archiveHeldCount"`
		PersonPausedCount int `json:"personPausedCount"`
		PausedCount       int `json:"pausedCount"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{g + "_11": "deadline", g + "_12": "copy", g + "_13": "lowq", g + "_14": "", g + "_15": "", g + "_16": ""}
	if len(snap.Queued) != len(want) {
		t.Fatalf("queued: %+v", snap.Queued)
	}
	for _, item := range snap.Queued {
		got := ""
		if item.ArchiveHold != nil {
			got = *item.ArchiveHold
		}
		if got != want[item.Key] {
			t.Errorf("%s: archiveHold %q, want %q", item.Key, got, want[item.Key])
		}
		if item.Key == g+"_16" && item.Status != "queued" || item.Key != g+"_16" && item.Status != "paused" {
			t.Errorf("%s: status %q", item.Key, item.Status)
		}
	}
	if snap.ArchiveHeldCount != 3 || snap.PersonPausedCount != 2 || snap.PausedCount != 5 {
		t.Fatalf("counts: archive %d person %d paused %d", snap.ArchiveHeldCount, snap.PersonPausedCount, snap.PausedCount)
	}

	// One item at a time (the batch's resume check).
	for key, reason := range want {
		got, err := a.queueHoldReason(ctx, key)
		if err != nil || got != reason {
			t.Errorf("queueHoldReason(%s) = %q, %v; want %q", key, got, err, reason)
		}
	}

	// Resume all, keeping what the archive set aside.
	if _, err = a.db.Writer.Exec(`UPDATE tgdl_queue_state SET paused=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	resumed, kept, err := a.queueLiftPausesKeepHeld(ctx)
	if err != nil || resumed != 2 || kept != 3 {
		t.Fatalf("resume all: resumed %d kept %d %v", resumed, kept, err)
	}
	for id, paused := range map[int64]bool{deferred: true, copyOf: true, orig: true, person: false, released: false} {
		if ixTestPaused(t, a, id) != paused {
			t.Errorf("item %d: paused %v, want %v", id, !paused, paused)
		}
	}
	var global int
	if err = a.db.Reader.QueryRow(`SELECT paused FROM tgdl_queue_state WHERE id=1`).Scan(&global); err != nil || global != 0 {
		t.Fatalf("queue-wide pause %d %v", global, err)
	}
}

func TestQueueKeyParts(t *testing.T) {
	for key, want := range map[string]struct {
		group string
		msg   int64
		ok    bool
	}{
		"-1001000000001_101": {"-1001000000001", 101, true},
		"a_b_7":              {"a_b", 7, true},
		"_7":                 {"", 0, false},
		"nounderscore":       {"", 0, false},
		"g_x":                {"g", 0, false},
	} {
		g, m, ok := queueKeyParts(key)
		if ok != want.ok || ok && (g != want.group || m != want.msg) {
			t.Errorf("%q: %q %d %v", key, g, m, ok)
		}
	}
}
