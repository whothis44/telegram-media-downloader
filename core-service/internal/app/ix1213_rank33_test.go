package app

import (
	"context"
	"testing"
	"time"
)

// ix.12.13 (Task 6): a ranked basic group (ID without -100), disabled in the
// config, whose messages were read but whose files pass never ran. The
// Priorities page offers "Add it to the rescan"; it must stick.
func TestIx1213RankedBasicGroupAddedToTheRescanSticks(t *testing.T) {
	ixBackgroundQueue.Store(false)
	defer ixBackgroundQueue.Store(true)
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const gid = "-4567890123"
	cfg, _ := a.config.Load(ctx)
	cfg["groups"] = []any{map[string]any{"id": int64(-4567890123), "name": "basic", "enabled": false,
		"filters": map[string]any{"photos": true, "videos": true, "files": true}}}
	if err = a.config.Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 20; i++ {
		if _, err = a.db.Writer.Exec(`INSERT INTO ix_messages(group_id,message_id,date,media_type,media_size,media_key) VALUES(?,?,?,?,?,?)`,
			gid, i, 1700000000+i, "video", 5<<20, "document:"+string(rune('a'+i))+":5242880"); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UnixMilli()
	if _, err = a.db.Writer.Exec(`INSERT INTO ix_walks(job_id,group_id,started_at,finished_at,processed,marked_deleted,state) VALUES('old',?,?,?,20,0,'done')`, gid, now-1000, now); err != nil {
		t.Fatal(err)
	}
	if err = a.ixSaveRanks(ctx, []string{gid}, map[string]string{gid: "basic"}); err != nil {
		t.Fatal(err)
	}
	if n, err := a.ixMissingFiles(ctx, gid); err != nil || n != 20 {
		t.Fatalf("missing %d %v", n, err)
	}
	find := func() ixGroupCoverage {
		a.ixCaches().coverBase = ixCached[ixCoverBase]{}
		for _, g := range a.ixPrioritiesNow(ctx).Groups {
			if g.GroupID == gid {
				return g
			}
		}
		t.Fatal("group not in the report")
		return ixGroupCoverage{}
	}
	before := find()
	t.Logf("before: place %d needed %d waiting %q notQueued %v", before.Place, before.Needed.Files, before.Waiting, before.NotQueued)
	res := historyHTTP(t, a, "POST", "/api/ix/queue/add", `{"groups":[{"id":"`+gid+`","name":"basic"}],"ensureFilters":true,"start":false,"priority":false}`)
	if res["success"] != true {
		t.Fatalf("add: %v", res)
	}
	st, _ := a.ixLoadQueue(ctx)
	if ixQueueIndex(&st, gid) < 0 {
		t.Fatalf("not queued: %+v (skipped finished %v)", st, res["skippedFinished"])
	}
	after := find()
	t.Logf("after: waiting %q notQueued %v", after.Waiting, after.NotQueued)
	if after.NotQueued {
		t.Fatalf("still offered: %+v", after)
	}
	// Read fully once before: queued again, it is not "not fully read".
	if !after.InRescan {
		t.Fatalf("not marked as in the rescan: %+v", after)
	}
	if !after.Scanned {
		t.Fatalf("shown as not fully read: %+v", after)
	}
}
