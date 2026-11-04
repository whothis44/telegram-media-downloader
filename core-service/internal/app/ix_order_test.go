package app

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// ixTestNoBytesFor makes the progress watch (ixLastProgress) see no bytes
// arrive for d.
func ixTestNoBytesFor(a *App, d time.Duration) {
	box := a.ixProgressBox()
	n := ixBytesReceived(a)
	box.mu.Lock()
	box.seen, box.bytes, box.at = true, n, time.Now().Add(-d)
	box.mu.Unlock()
}

// ixTestBytes replaces the engine's bytes-received counter with one the
// test moves.
func ixTestBytes(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	old := ixBytesReceived
	ixBytesReceived = func(*App) int64 { return n.Load() }
	t.Cleanup(func() { ixBytesReceived = old })
	return &n
}

// ixTestWork queues an invented item; its queue ID.
func ixTestWork(t *testing.T, a *App, acct, group string, msg int, media string, size int64, paused int) int64 {
	t.Helper()
	now := time.Now().UnixMilli()
	res, err := a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,paused,origin,created_at,updated_at)
		VALUES(?,?,'G',?,1,?,?,'f',?,X'','pending',?,'history',?,?)`, acct, group, msg, fmt.Sprintf("document:%d:%d", msg, size), media, size, paused, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// ixTestHold records an item as a held copy: of a saved file (of == 0) or
// waiting for a better copy in the queue.
func ixTestHold(t *testing.T, a *App, id int64, confirm string, of int64) {
	t.Helper()
	how, reason, like := "saved", "looks like a saved copy", "saved/a.mp4"
	if of != 0 {
		how, reason, like = "queued", "looks like a copy of a queued file", ""
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_held(work_id,group_id,message_id,like_path,like_key,reason,held_at,released_at)
		SELECT id,group_id,message_id,?,'',?,?,0 FROM tgdl_work WHERE id=?`, like, reason, time.Now().UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_copy_hold(work_id,how,of_work,confirm) VALUES(?,?,?,?)`, id, how, of, confirm); err != nil {
		t.Fatal(err)
	}
}

// ixTestLowq records an item as the original of a video saved in a lower
// quality first, waiting to replace it.
func ixTestLowq(t *testing.T, a *App, id int64) {
	t.Helper()
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_lowq(group_id,message_id,work_id,orig_id,orig_size,alt_id,alt_size,state,saved_at)
		SELECT group_id,message_id,id,'77'||id,file_size,'88'||id,1000,'saved',? FROM tgdl_work WHERE id=?`, time.Now().UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
}

func ixTestStatus(t *testing.T, a *App, id int64) string {
	t.Helper()
	var s string
	if err := a.db.Reader.QueryRow(`SELECT status FROM tgdl_work WHERE id=?`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// The things that wait at the back of the queue go in one order, a stage
// at a time, each once everything before it is done; items a person paused,
// items of an account that is not running and items waiting to retry hold
// nothing back; nothing waits for ever. Before, lower-quality originals and
// confirmed copies waited for each other (each counted the other as
// waiting), and confirmed copies waited for items a person had paused.
func TestIxWhatGoesLastGoesInOneOrder(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{"deadlineOrder": "on"}) // default settings otherwise: holding copies and lower quality first are on
	const g = "-1001"
	// The account at work, and the saved file the held copies copy.
	done := ixTestWork(t, a, "acc", g, 1, "photo", 1<<20, 0)
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='completed' WHERE id=?`, done)
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_type,file_path,status) VALUES(?,100,'a.mp4','video','saved/a.mp4','completed')`, g); err != nil {
		t.Fatal(err)
	}
	normal := ixTestWork(t, a, "acc", g, 2, "photo", 1<<20, 0)
	mid := ixTestWork(t, a, "acc", g, 3, "video", 50<<20, 0)  // set aside, band 1
	big := ixTestWork(t, a, "acc", g, 4, "video", 600<<20, 0) // set aside, band 3
	held := ixTestWork(t, a, "acc", g, 5, "video", 20<<20, 1)
	ixTestHold(t, a, held, "", 0)
	confirmed := ixTestWork(t, a, "acc", g, 6, "video", 30<<20, 1)
	ixTestHold(t, a, confirmed, "confirmed", 0)
	orig := ixTestWork(t, a, "acc", g, 7, "video", 300<<20, 1)
	ixTestLowq(t, a, orig)
	personPaused := ixTestWork(t, a, "acc", g, 8, "video", 10<<20, 1)
	leader := ixTestWork(t, a, "acc", g, 9, "video", 80<<20, 1) // paused by a person too
	follower := ixTestWork(t, a, "acc", g, 10, "video", 70<<20, 1)
	ixTestHold(t, a, follower, "", leader)
	otherAcct := ixTestWork(t, a, "other", g, 11, "photo", 1<<20, 0) // an account that is not running
	retry := ixTestWork(t, a, "acc", g, 12, "photo", 1<<20, 0)
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET retry_at=? WHERE id=?`, time.Now().Add(24*time.Hour).UnixMilli(), retry)

	watch := map[int64]string{normal: "normal", mid: "set aside 50 MB", big: "set aside 600 MB", held: "held copy",
		confirmed: "confirmed copy", orig: "lower-quality original", follower: "copy of a paused file"}
	released, finished := map[int64]int{}, map[int64]int{}
	steps := []func(context.Context) error{a.ixDeadlineStep, a.ixHeldStep, func(ctx context.Context) error { a.ixCopiesRound(ctx); return nil }, a.ixLowqStep}
	for step := 1; step <= 30; step++ {
		for _, f := range steps {
			if err := f(ctx); err != nil {
				t.Fatalf("step %d: %v", step, err)
			}
		}
		for id := range watch {
			if _, ok := released[id]; !ok && ixTestStatus(t, a, id) == "pending" && !ixTestPaused(t, a, id) {
				released[id] = step
			}
		}
		// The engine downloads everything of the running account it can take now.
		now := time.Now().UnixMilli()
		rows, err := a.db.Reader.Query(`SELECT id FROM tgdl_work WHERE status='pending' AND paused=0 AND account_id='acc' AND retry_at<=?`, now)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			finished[id] = step
			_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='completed', updated_at=? WHERE id=?`, now, id)
		}
	}
	for id, name := range watch {
		if finished[id] == 0 {
			t.Fatalf("%s never went (released at step %d)", name, released[id])
		}
	}
	// In order: normal work, set aside by band, held copies, originals,
	// confirmed copies; each once the one before it is downloaded.
	order := []int64{normal, mid, big, held, orig, confirmed}
	for i := 1; i < len(order); i++ {
		prev, cur := order[i-1], order[i]
		if released[cur] <= finished[prev] {
			t.Fatalf("%s went at step %d, before %s was done (step %d); released %v finished %v",
				watch[cur], released[cur], watch[prev], finished[prev], released, finished)
		}
	}
	// A copy waiting for a file a person paused does not wait for ever.
	if released[follower] != 1 {
		t.Fatalf("the copy of a paused file went at step %d", released[follower])
	}
	// What a person paused stays paused; the others are left alone.
	for _, id := range []int64{personPaused, leader} {
		if !ixTestPaused(t, a, id) || ixTestStatus(t, a, id) != "pending" {
			t.Fatal("a file a person paused was let go")
		}
	}
	if ixTestPaused(t, a, otherAcct) || ixTestPaused(t, a, retry) {
		t.Fatal("an item of another account, or one waiting to retry, was paused")
	}
	if o, err := a.ixOrderNow(ctx); err != nil || o.Waiting != [ixStageCount]int{} {
		t.Fatalf("still waiting: %+v %v", o, err)
	}
}

// Held for ever: held copies count nowhere, so the originals go; with the
// setting that lets confirmed copies go early, they go with the others.
func TestIxOrderFollowsTheHoldSettings(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{"holdForever": true})
	const g = "-1001"
	held := ixTestWork(t, a, "acc", g, 5, "video", 20<<20, 1)
	ixTestHold(t, a, held, "", 0)
	confirmed := ixTestWork(t, a, "acc", g, 6, "video", 30<<20, 1)
	ixTestHold(t, a, confirmed, "confirmed", 0)
	orig := ixTestWork(t, a, "acc", g, 7, "video", 300<<20, 1)
	ixTestLowq(t, a, orig)
	o, err := a.ixOrderNow(ctx)
	if err != nil || !o.clearBefore(ixStageLowq) {
		t.Fatalf("copies held for ever hold the originals back: %+v %v", o, err)
	}
	s := a.ixLoadSettings(ctx)
	s.HoldForever, s.ConfirmedGoEarly = false, true
	_ = a.ixSaveSettings(ctx, s)
	if o, _ = a.ixOrderNow(ctx); o.Waiting[ixStageHeld] != 2 || o.Waiting[ixStageConfirmed] != 0 || o.Waiting[ixStageLowq] != 1 {
		t.Fatalf("confirmed copies going early: %+v", o)
	}
	if err := a.ixHeldStep(ctx); err != nil {
		t.Fatal(err)
	}
	if ixTestPaused(t, a, held) || ixTestPaused(t, a, confirmed) || !ixTestPaused(t, a, orig) {
		t.Fatal("with confirmed copies going early, both copies go before the original")
	}
}

// "Nothing moved for half an hour" is judged by bytes too: with only big
// files left, each takes hours under Telegram's limits while bytes keep
// arriving, and that is not a stuck queue.
func TestIxStuckQueueRulesJudgeByBytes(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{"deadlineOrder": "on"})
	bytes := ixTestBytes(t)
	old := time.Now().Add(-2 * time.Hour).UnixMilli()
	done := ixTestWork(t, a, "acc", "-1001", 1, "photo", 1<<20, 0)
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='completed', updated_at=? WHERE id=?`, old, done) // the last file finished 2 hours ago
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_group_info(group_id,title,kind,username) VALUES('-1002','Public','channel','somechannel')`); err != nil {
		t.Fatal(err)
	}
	ixTestWork(t, a, "acc", "-1001", 2, "video", 900<<20, 0) // downloading for hours
	public := ixTestWork(t, a, "acc", "-1002", 3, "photo", 1<<20, 0)
	held := ixTestWork(t, a, "acc", "-1001", 4, "video", 20<<20, 1)
	ixTestHold(t, a, held, "", 0)
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_type,file_path,status) VALUES('-1001',100,'a.mp4','video','saved/a.mp4','completed')`); err != nil {
		t.Fatal(err)
	}
	if err := a.ixDeadlineStep(ctx); err != nil {
		t.Fatal(err)
	}
	if !ixTestPaused(t, a, public) {
		t.Fatal("the public chat's file was not set aside")
	}
	_, _ = a.db.Writer.Exec(`UPDATE ix_deferred SET deferred_at=?`, old)
	// Bytes keep arriving: nothing is let go.
	for i := 0; i < 3; i++ {
		ixTestNoBytesFor(a, time.Hour)
		bytes.Add(4 << 20)
		for _, f := range []func(context.Context) error{a.ixHeldStep, a.ixDeadlineStep} {
			if err := f(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if !ixTestPaused(t, a, held) || !ixTestPaused(t, a, public) || a.ixDeadlineSuspended(ctx, time.Now()) {
			t.Fatal("let go while bytes were arriving")
		}
	}
	// The queue paused by a person: not stuck either.
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_queue_state SET paused=1 WHERE id=1`)
	ixTestNoBytesFor(a, time.Hour)
	_ = a.ixHeldStep(ctx)
	_ = a.ixDeadlineStep(ctx)
	if !ixTestPaused(t, a, held) || !ixTestPaused(t, a, public) {
		t.Fatal("let go while a person had paused the queue")
	}
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_queue_state SET paused=0 WHERE id=1`)
	// No bytes for half an hour: stuck. Both let go.
	ixTestNoBytesFor(a, time.Hour)
	_ = a.ixHeldStep(ctx)
	_ = a.ixDeadlineStep(ctx)
	if ixTestPaused(t, a, held) || ixTestPaused(t, a, public) || !a.ixDeadlineSuspended(ctx, time.Now()) {
		t.Fatal("a stuck queue was not let go")
	}
	// The first look after a start counts as movement: a restart after
	// hours off does not let everything go at once.
	b := &App{db: a.db}
	if last := b.ixLastProgress(ctx); time.Since(last) > time.Minute {
		t.Fatalf("first look: %v", last)
	}
	ixProgressState.Delete(b)
}

// Backfills do not count what waits apart towards their queue limit: held
// copies, files set aside by the deadline order and originals waiting to
// replace a lower quality. Files a person paused and live items do count.
func TestIxBackfillCapLeavesOutWhatWaitsApart(t *testing.T) {
	a, ctx := ix8App(t, nil)
	const g = "-1009"
	held := ixTestWork(t, a, "acc", g, 1, "video", 20<<20, 1)
	ixTestHold(t, a, held, "", 0)
	set := ixTestWork(t, a, "acc", g, 2, "video", 900<<20, 1)
	_, _ = a.db.Writer.Exec(`INSERT INTO ix_deferred(work_id,band,deferred_at,released_at) VALUES(?,3,1,0)`, set)
	orig := ixTestWork(t, a, "acc", g, 3, "video", 300<<20, 1)
	ixTestLowq(t, a, orig)
	ixTestWork(t, a, "acc", g, 4, "video", 20<<20, 1) // paused by a person
	ixTestWork(t, a, "acc", g, 5, "photo", 1<<20, 0)  // waiting normally
	live := ixTestWork(t, a, "acc", g, 6, "video", 20<<20, 1)
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET origin='live' WHERE id=?`, live)
	ixTestHold(t, a, live, "", 0)
	released := ixTestWork(t, a, "acc", g, 7, "video", 300<<20, 0) // an original let go: it can download
	ixTestLowq(t, a, released)
	_, _ = a.db.Writer.Exec(`UPDATE ix_lowq SET state='released' WHERE work_id=?`, released)
	if n := a.ixWaitingApart(ctx); n != 3 {
		t.Fatalf("waiting apart: %d, want 3", n)
	}
}

// Through a backfill: a queue holding only items that wait apart does not
// stop the walk at its limit.
func TestHistoryCapIgnoresItemsWaitingApart(t *testing.T) {
	var transfers, pages atomic.Int64
	page := func(context.Context, telegram.HistoryRequest) (telegram.HistoryPage, error) {
		pages.Add(1)
		return historyMessages(10), nil
	}
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: t.TempDir(), AccountFactory: historyFactory(t, page, nil, &transfers)})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureDisabledHistory(t, a)
	cfg, err := a.config.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ensureMap(cfg, "advanced")["history"] = map[string]any{"backpressureCap": 1, "backpressureMaxWaitMs": 1}
	if err = a.config.Save(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Writer.Exec(`UPDATE tgdl_queue_state SET paused=1`); err != nil {
		t.Fatal(err)
	}
	// Another chat's set-aside file and lower-quality original fill the
	// limit, but cannot download now.
	set := ixTestWork(t, a, "acc", "-1009", 1, "video", 900<<20, 1)
	_, _ = a.db.Writer.Exec(`INSERT INTO ix_deferred(work_id,band,deferred_at,released_at) VALUES(?,3,1,0)`, set)
	ixTestLowq(t, a, ixTestWork(t, a, "acc", "-1009", 2, "video", 300<<20, 1))
	res := historyHTTP(t, a, "POST", "/api/history", `{"groupId":"-1000000000042"}`)
	j := historyFinished(t, a, res["jobId"].(string))
	// One page read and queued; then the walk's own file fills the limit.
	if j.Processed != 1 || pages.Load() != 1 || j.Error == nil || !strings.Contains(*j.Error, "stall deadline") {
		t.Fatalf("backfill job=%+v pages=%d", j, pages.Load())
	}
}

// A lesser copy whose better copy finished downloading but still sits in
// the local staging folder (not in the catalogue yet) waits; once the file
// is in the library it is checked against it.
func TestIxHeldCopyWaitsForAStagedBetterCopy(t *testing.T) {
	a, ctx := ixTestApp(t)
	scene := ixTestStripped(t, ixTestScene(21, 40, 23))
	v1 := ixTestVideo(1, 301, 50<<20, 60, 1280, 720, scene, 1700000000)
	v2 := ixTestVideo(2, 302, 80<<20, 60.2, 1920, 1080, scene, 1700000100) // the best
	id1, id2 := ixTestQueue(t, a, v1), ixTestQueue(t, a, v2)
	if err := a.ixHeldStep(ctx); err != nil {
		t.Fatal(err)
	}
	if how, of := ixTestHow(a, id1); how != "queued" || of != id2 {
		t.Fatalf("not held for the best: %q %d", how, of)
	}
	// Downloaded into the staging folder: completed, but no catalogue entry yet.
	if _, err := a.db.Writer.Exec(`CREATE TABLE IF NOT EXISTS tgdl_stage_moves (path TEXT PRIMARY KEY NOT NULL, stage_dir TEXT NOT NULL, item TEXT NOT NULL,
		generation INTEGER NOT NULL, identity TEXT NOT NULL, sha256 TEXT NOT NULL, size INTEGER NOT NULL, state TEXT NOT NULL DEFAULT 'waiting',
		attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL DEFAULT 0, failing_since INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO tgdl_stage_moves(path,stage_dir,item,generation,identity,sha256,size,created_at)
		SELECT 'c/v2.mp4','/stage','x',1,identity,'00',file_size,1 FROM tgdl_work WHERE id=?`, id2); err != nil {
		t.Fatal(err)
	}
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='completed' WHERE id=?`, id2)
	if err := a.ixHeldRecheck(ctx); err != nil {
		t.Fatal(err)
	}
	if how, _ := ixTestHow(a, id1); how != "queued" || !ixTestPaused(t, a, id1) {
		t.Fatalf("let go while its better copy was still being moved: %q", how)
	}
	// In the library now.
	_, _ = a.db.Writer.Exec(`DELETE FROM tgdl_stage_moves`)
	ixTestSave(t, a, v2, "c/v2.mp4", "cc33")
	if err := a.ixHeldRecheck(ctx); err != nil {
		t.Fatal(err)
	}
	if how, _ := ixTestHow(a, id1); how != "saved" || !ixTestPaused(t, a, id1) {
		t.Fatalf("not checked against the saved file: %q", how)
	}
}

// Sampling frames is for copies: originals waiting to replace a lower
// quality are not checked.
func TestIxSamplingLeavesOutLowerQualityOriginals(t *testing.T) {
	a, ctx := ixTestApp(t)
	scene := ixTestStripped(t, ixTestScene(31, 40, 23))
	plain := ixTestQueue(t, a, ixTestVideo(1, 401, 300<<20, 600, 1280, 720, scene, 1700000000))
	orig := ixTestQueue(t, a, ixTestVideo(2, 402, 400<<20, 900, 1280, 720, scene, 1700000100))
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET paused=1 WHERE id=?`, orig)
	ixTestLowq(t, a, orig)
	jobs, err := a.ixSampleJobs(ctx, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].work != plain {
		t.Fatalf("jobs: %+v", jobs)
	}
}

// Read-only (the account is gone): the copies, preview and sampling work
// and the queue order steps do nothing, quietly.
func TestIxReadOnlySkipsCopyWorkQuietly(t *testing.T) {
	a, ctx := ixTestApp(t)
	savedFake := ixSampleFake
	ixSampleFake = true
	opened := false
	oldOpen := ixOpenCopyReader
	ixOpenCopyReader = func(*App, context.Context, string) (ixCopyReader, error) {
		opened = true
		return nil, fmt.Errorf("no Telegram")
	}
	t.Cleanup(func() { ixSampleFake, ixOpenCopyReader = savedFake, oldOpen })
	scene := ixTestStripped(t, ixTestScene(41, 40, 23))
	big := ixTestQueue(t, a, ixTestVideo(1, 501, 300<<20, 600, 1280, 720, scene, 1700000000))
	held := ixTestWork(t, a, "acc", "-1001", 2, "video", 20<<20, 1)
	ixTestHold(t, a, held, "", 0)
	a.ixSetGone(ctx, ixGoneInfo{Gone: true, Reason: "telegram"})
	logLines := func() []ixLogLine {
		rt := a.ix()
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return append([]ixLogLine(nil), rt.recent...)
	}
	before := len(logLines())
	for i := 0; i < 3; i++ {
		a.ixCopiesRound(ctx)
		for _, f := range []func(context.Context) error{a.ixHeldStep, a.ixDeadlineStep, a.ixLowqStep} {
			if err := f(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	if opened {
		t.Fatal("tried to reach Telegram in read-only mode")
	}
	if lines := logLines(); len(lines) != before {
		t.Fatalf("log lines in read-only mode: %v", lines[before:])
	}
	if !ixTestPaused(t, a, held) || ixTestPaused(t, a, big) {
		t.Fatal("the queue was changed in read-only mode")
	}
}

// Looking for an exact re-upload by size uses the size indexes, not a
// read of the whole catalogue.
func TestIxSizeLookupsUseAnIndex(t *testing.T) {
	a, _ := ixTestApp(t)
	for _, q := range []string{
		`SELECT EXISTS (SELECT 1 FROM downloads WHERE file_size=? AND status='completed') OR EXISTS (SELECT 1 FROM downloads WHERE telegram_media_size=? AND status='completed')`,
		`SELECT file_path FROM downloads WHERE file_size=? AND status='completed' AND file_path IS NOT NULL AND file_path<>'' AND NOT (group_id=? AND message_id=?)
		UNION SELECT file_path FROM downloads WHERE telegram_media_size=? AND status='completed' AND file_path IS NOT NULL AND file_path<>'' AND NOT (group_id=? AND message_id=?) LIMIT 5`,
	} {
		args := make([]any, strings.Count(q, "?"))
		for i := range args {
			args[i] = 1
		}
		rows, err := a.db.Reader.Query(`EXPLAIN QUERY PLAN `+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, aux int
			var detail string
			_ = rows.Scan(&id, &parent, &aux, &detail)
			plan = append(plan, detail)
		}
		rows.Close()
		p := strings.Join(plan, "; ")
		if strings.Contains(p, "SCAN downloads") || !strings.Contains(p, "ix_downloads_size") || !strings.Contains(p, "ix_downloads_tg_size") {
			t.Fatalf("plan: %s", p)
		}
	}
}
