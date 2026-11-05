package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
)

// All of it invented: chats, files and sizes.

func ixSchedIdent(n int, size int64) string {
	return fmt.Sprintf("video\x00%d\x00%d", n, size)
}

func ixSchedRowAdd(t *testing.T, a *App, group string, msg int, identity string, size int64) int64 {
	t.Helper()
	res, err := a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at)
		VALUES('one',?,?,?,1,?,'video','f.mp4',?,X'00','pending','history',1,1)`, group, group, msg, identity, size)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func ixSchedClaim(t *testing.T, a *App) *engine.Work {
	t.Helper()
	w, err := engine.NewWorkStore(a.db.Writer, a.db.Reader).Claim(context.Background(), []string{"one"}, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func ixSchedOrder(t *testing.T, a *App, id int64) int {
	t.Helper()
	var n int
	if err := a.db.Reader.QueryRow(`SELECT sched_order FROM tgdl_work WHERE id=?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func ixSchedStepOK(t *testing.T, a *App, ctx context.Context) {
	t.Helper()
	if err := a.ixSchedStep(ctx); err != nil {
		t.Fatal(err)
	}
}

// Promoted after its files were queued: the very next claim reflects it.
func TestIxRankPromoteAfterFilesAreQueued(t *testing.T) {
	a, ctx := ixTestApp(t)
	early := ixSchedRowAdd(t, a, "low", 1, ixSchedIdent(1, 10), 10)
	ixSchedRowAdd(t, a, "low", 2, ixSchedIdent(2, 10), 10)
	late := ixSchedRowAdd(t, a, "top", 3, ixSchedIdent(3, 10), 10)
	ixSchedStepOK(t, a, ctx)
	if w := ixSchedClaim(t, a); w == nil || w.ID != early {
		t.Fatalf("with nothing ranked the queue goes in order: %+v", w)
	}
	if err := a.ixSetRanks(ctx, []string{"top"}, nil); err != nil {
		t.Fatal(err)
	}
	if w := ixSchedClaim(t, a); w == nil || w.ID != late {
		t.Fatalf("after ranking, next claim %+v, want %d", w, late)
	}
}

// Finish in order: a big file of rank 1 goes before small files of rank 2 and
// of no rank, and the deadline order does not set it aside for them.
func TestIxRankBigFileIsNotBlockedBySmallerLowerFiles(t *testing.T) {
	a, ctx := ixTestApp(t)
	if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at) VALUES('ix_settings','{"deadlineOrder":"on"}',1) ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		ixSchedRowAdd(t, a, "low", i, ixSchedIdent(i, 1<<20), 1<<20)
	}
	big := ixSchedRowAdd(t, a, "top", 50, ixSchedIdent(50, 900<<20), 900<<20)
	if err := a.ixSetRanks(ctx, []string{"top"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.ixDeadlineStep(ctx); err != nil {
		t.Fatal(err)
	}
	if ixTestPaused(t, a, big) {
		t.Fatal("the deadline order set a ranked group's file aside")
	}
	if w := ixSchedClaim(t, a); w == nil || w.ID != big {
		t.Fatalf("next claim %+v, want the ranked big file", w)
	}
}

// A file set aside before its group was ranked comes back.
func TestIxRankReleasesFilesSetAside(t *testing.T) {
	a, ctx := ixTestApp(t)
	if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at) VALUES('ix_settings','{"deadlineOrder":"on"}',1) ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		t.Fatal(err)
	}
	ixSchedRowAdd(t, a, "low", 1, ixSchedIdent(1, 1<<20), 1<<20)
	big := ixSchedRowAdd(t, a, "top", 2, ixSchedIdent(2, 900<<20), 900<<20)
	if err := a.ixDeadlineStep(ctx); err != nil {
		t.Fatal(err)
	}
	if !ixTestPaused(t, a, big) {
		t.Fatal("setup: the big file should be set aside")
	}
	if err := a.ixSetRanks(ctx, []string{"top"}, nil); err != nil {
		t.Fatal(err)
	}
	if ixTestPaused(t, a, big) {
		t.Fatal("ranking its group did not bring it back")
	}
	if err := a.ixDeadlineStep(ctx); err != nil {
		t.Fatal(err)
	}
	if ixTestPaused(t, a, big) {
		t.Fatal("the deadline order set it aside again")
	}
}

// Within a rank, while time is short, the size bands are the tie-break.
func TestIxRankBandsBreakTiesWithinARank(t *testing.T) {
	a, ctx := ixTestApp(t)
	if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at) VALUES('ix_settings','{"deadlineOrder":"on"}',1) ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		t.Fatal(err)
	}
	big := ixSchedRowAdd(t, a, "top", 1, ixSchedIdent(1, 900<<20), 900<<20)
	small := ixSchedRowAdd(t, a, "top", 2, ixSchedIdent(2, 1<<20), 1<<20)
	if err := a.ixSetRanks(ctx, []string{"top"}, nil); err != nil {
		t.Fatal(err)
	}
	if ixSchedOrder(t, a, small) >= ixSchedOrder(t, a, big) {
		t.Fatalf("small %d, big %d", ixSchedOrder(t, a, small), ixSchedOrder(t, a, big))
	}
	// ... and never across ranks.
	if ixSchedOrder(t, a, big) >= ixRankBase(1) {
		t.Fatalf("a band reached the next rank: %d", ixSchedOrder(t, a, big))
	}
}

// Queued through a low group, wanted by a high one: promoted, one transfer.
func TestIxRankSharedFileInheritsAndDownloadsOnce(t *testing.T) {
	a, ctx := ixTestApp(t)
	shared := ixSchedIdent(7, 100)
	ixSchedRowAdd(t, a, "other", 1, ixSchedIdent(1, 100), 100)
	viaLow := ixSchedRowAdd(t, a, "low", 2, shared, 100)
	viaTop := ixSchedRowAdd(t, a, "top", 3, shared, 100)
	if err := a.ixSetRanks(ctx, []string{"top", "low"}, nil); err != nil {
		t.Fatal(err)
	}
	if ixSchedOrder(t, a, viaLow) != ixRankBase(0) || ixSchedOrder(t, a, viaTop) != ixRankBase(0) {
		t.Fatalf("orders low=%d top=%d, want both at rank 1's", ixSchedOrder(t, a, viaLow), ixSchedOrder(t, a, viaTop))
	}
	first := ixSchedClaim(t, a)
	if first == nil || first.ID != viaLow {
		t.Fatalf("first claim %+v, want the oldest row of the shared file", first)
	}
	second := ixSchedClaim(t, a)
	if second == nil || second.ID == viaTop {
		t.Fatalf("second claim %+v: the twin must wait for the transfer", second)
	}
}

// A ranked group's message not queued yet still asks for the file: its files
// pass has not come to it, but another group's row of the same file goes up.
func TestIxRankMessageDemandPromotesQueuedFile(t *testing.T) {
	a, ctx := ixTestApp(t)
	row := ixSchedRowAdd(t, a, "low", 1, ixSchedIdent(9, 100), 100)
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_messages(group_id,message_id,media_key) VALUES('top',5,'video:9:100')`); err != nil {
		t.Fatal(err)
	}
	if err := a.ixSetRanks(ctx, []string{"top"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := ixSchedOrder(t, a, row); got != ixRankBase(0) {
		t.Fatalf("order %d, want rank 1's", got)
	}
}

// A file already saved takes no bytes: its rows go at once, even set aside, and
// do not fill the queue limit.
func TestIxSavedFileRowsGoAtOnce(t *testing.T) {
	a, ctx := ixTestApp(t)
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_size,file_type,file_path,status,telegram_media_kind,telegram_media_id,telegram_media_size) VALUES('other',1,'a',5,'video','a.mp4','completed','video','42',5)`); err != nil {
		t.Fatal(err)
	}
	ixSchedRowAdd(t, a, "low", 1, ixSchedIdent(1, 5), 5)
	free := ixSchedRowAdd(t, a, "low", 2, ixSchedIdent(42, 5), 5)
	ixSchedStepOK(t, a, ctx)
	if got := ixSchedOrder(t, a, free); got != ixFreeOrder {
		t.Fatalf("order %d", got)
	}
	if w := ixSchedClaim(t, a); w == nil || w.ID != free {
		t.Fatalf("next claim %+v, want the row that takes no bytes", w)
	}
	if a.ixSharedWaiting() != 1 {
		t.Fatalf("shared rows %d", a.ixSharedWaiting())
	}
}

// A person asked for the real file ("Fetch the real files"): it is a download.
func TestIxBlockedIdentityIsNotFree(t *testing.T) {
	a, ctx := ixTestApp(t)
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_size,file_type,file_path,status,telegram_media_kind,telegram_media_id,telegram_media_size) VALUES('other',1,'a',5,'video','a.mp4','completed','video','42',5)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_subst_block(identity) VALUES(?)`, ixSchedIdent(42, 5)); err != nil {
		t.Fatal(err)
	}
	row := ixSchedRowAdd(t, a, "low", 2, ixSchedIdent(42, 5), 5)
	ixSchedStepOK(t, a, ctx)
	if got := ixSchedOrder(t, a, row); got != engine.UnrankedOrder {
		t.Fatalf("order %d", got)
	}
}

// A person's pause stands: the row stays paused, and it asks for nothing.
func TestIxRankKeepsAPersonsPause(t *testing.T) {
	a, ctx := ixTestApp(t)
	shared := ixSchedIdent(7, 100)
	paused := ixSchedRowAdd(t, a, "top", 1, shared, 100)
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET paused=1 WHERE id=?`, paused); err != nil {
		t.Fatal(err)
	}
	other := ixSchedRowAdd(t, a, "low", 2, shared, 100)
	if err := a.ixSetRanks(ctx, []string{"top"}, nil); err != nil {
		t.Fatal(err)
	}
	if !ixTestPaused(t, a, paused) {
		t.Fatal("ranking undid a person's pause")
	}
	if got := ixSchedOrder(t, a, other); got != engine.UnrankedOrder {
		t.Fatalf("a paused row promoted its twin: %d", got)
	}
}

// Promoting a group does not release its probable copies, but the better copy
// they wait for, queued through any group, goes at their rank.
func TestIxRankHeldCopyStaysAndPromotesItsBetterCopy(t *testing.T) {
	a, ctx := ixTestApp(t)
	better := ixSchedRowAdd(t, a, "low", 1, ixSchedIdent(1, 500), 500)
	held := ixSchedRowAdd(t, a, "top", 2, ixSchedIdent(2, 400), 400)
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET paused=1 WHERE id=?`, held); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_held(work_id,group_id,message_id,reason,released_at) VALUES(?,'top',2,'probable copy',0)`, held); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_copy_hold(work_id,how,of_work) VALUES(?,'queued',?)`, held, better); err != nil {
		t.Fatal(err)
	}
	if err := a.ixSetRanks(ctx, []string{"top"}, nil); err != nil {
		t.Fatal(err)
	}
	if !ixTestPaused(t, a, held) {
		t.Fatal("ranking its group let a probable copy go")
	}
	if got := ixSchedOrder(t, a, better); got != ixRankBase(0) {
		t.Fatalf("the better copy's order %d, want rank 1's", got)
	}
}

// The ranks survive the queue row, a clear of the queue, and new rows of the
// group take them again.
func TestIxRankOutlivesRescanRows(t *testing.T) {
	a, ctx := ixTestApp(t)
	if _, err := a.ixQueueEdit(ctx, func(st *ixQueueState) error {
		st.Items = append(st.Items, ixQueueItem{GroupID: "top", Name: "Top", State: "waiting", Phase: ixPhaseMessages})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code, out := ixCall(ctx, a.handleIxQueueAction, http.MethodPost, "/api/ix/queue/priority", map[string]any{"groupId": "top", "priority": true}, map[string]string{"action": "priority"}); code != http.StatusOK {
		t.Fatalf("star: %d %v", code, out)
	}
	if code, out := ixCall(ctx, a.handleIxQueueAction, http.MethodPost, "/api/ix/queue/clear", map[string]any{}, map[string]string{"action": "clear"}); code != http.StatusOK {
		t.Fatalf("clear: %d %v", code, out)
	}
	ranks, err := a.ixRanks(ctx)
	if err != nil || len(ranks) != 1 || ranks[0].GroupID != "top" || ranks[0].Name != "Top" || ranks[0].Rank != 1 {
		t.Fatalf("ranks %+v err %v", ranks, err)
	}
	row := ixSchedRowAdd(t, a, "top", 1, ixSchedIdent(1, 10), 10)
	ixSchedStepOK(t, a, ctx)
	if got := ixSchedOrder(t, a, row); got != ixRankBase(0) {
		t.Fatalf("order %d", got)
	}
}

// Groups starred by an earlier version join the ranking once, in queue order.
func TestIxRanksComeFromOldStarsOnce(t *testing.T) {
	a, ctx := ixTestApp(t)
	if _, err := a.ixQueueEdit(ctx, func(st *ixQueueState) error {
		st.Items = append(st.Items,
			ixQueueItem{GroupID: "plain", Name: "plain", State: "waiting"},
			ixQueueItem{GroupID: "s1", Name: "one", State: "waiting", Priority: true},
			ixQueueItem{GroupID: "s2", Name: "two", State: "done", Priority: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ixSchedStepOK(t, a, ctx)
	ranks, _ := a.ixRanks(ctx)
	if len(ranks) != 2 || ranks[0].GroupID != "s1" || ranks[1].GroupID != "s2" {
		t.Fatalf("ranks %+v", ranks)
	}
	if err := a.ixSetRanks(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	ixSchedStepOK(t, a, ctx)
	if ranks, _ = a.ixRanks(ctx); len(ranks) != 0 {
		t.Fatalf("taken off the ranking, the stars came back: %+v", ranks)
	}
}

// A queue full of lower-ranked files does not hold a ranked group's back.
func TestIxRankAdmissionIgnoresOutrankedRows(t *testing.T) {
	a, ctx := ixTestApp(t)
	for i := 1; i <= 20; i++ {
		ixSchedRowAdd(t, a, "low", i, ixSchedIdent(i, 10), 10)
	}
	if err := a.ixSetRanks(ctx, []string{"top", "low"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := a.ixAdmissionCredit(ctx, "top"); got != 20 {
		t.Fatalf("credit for the higher group %d, want 20", got)
	}
	if got := a.ixAdmissionCredit(ctx, "low"); got != 0 {
		t.Fatalf("credit for the same group %d, want 0", got)
	}
	if got := a.ixAdmissionCredit(ctx, "unranked"); got != 0 {
		t.Fatalf("credit for an unranked group %d, want 0", got)
	}
}

// Shared rows do not fill the queue limit either.
func TestIxSharedRowsAreCounted(t *testing.T) {
	a, ctx := ixTestApp(t)
	for i := 1; i <= 4; i++ {
		ixSchedRowAdd(t, a, fmt.Sprintf("g%d", i), 1, ixSchedIdent(1, 10), 10) // one file, four rows
	}
	ixSchedRowAdd(t, a, "g9", 1, ixSchedIdent(2, 10), 10)
	ixSchedStepOK(t, a, ctx)
	if got := a.ixSharedWaiting(); got != 3 {
		t.Fatalf("shared rows %d, want 3", got)
	}
}

// A blocked top rank does not stop the rest, and is taken up again.
func TestIxRankBlockedGroupYieldsAndIsRevisited(t *testing.T) {
	a, ctx := ixTestApp(t)
	top := ixSchedRowAdd(t, a, "top", 1, ixSchedIdent(1, 10), 10)
	low := ixSchedRowAdd(t, a, "low", 2, ixSchedIdent(2, 10), 10)
	low2 := ixSchedRowAdd(t, a, "low", 3, ixSchedIdent(3, 10), 10)
	if err := a.ixSetRanks(ctx, []string{"top"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET retry_at=? WHERE id=?`, time.Now().Add(time.Hour).UnixMilli(), top); err != nil {
		t.Fatal(err)
	}
	if w := ixSchedClaim(t, a); w == nil || w.ID != low {
		t.Fatalf("with rank 1 waiting to retry, next claim %+v", w)
	}
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET retry_at=0 WHERE id=?`, top); err != nil {
		t.Fatal(err)
	}
	if w := ixSchedClaim(t, a); w == nil || w.ID != top {
		t.Fatalf("rank 1 not revisited: %+v", w)
	}
	if w := ixSchedClaim(t, a); w == nil || w.ID != low2 {
		t.Fatalf("then the rest: %+v", w)
	}
}

// Nothing changes, nothing is written: the step is cheap to run every minute.
func TestIxSchedStepWritesOnlyWhatChanged(t *testing.T) {
	a, ctx := ixTestApp(t)
	const rows = 5000
	tx, err := a.db.Writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= rows; i++ {
		if _, err := tx.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at)
			VALUES('one',?,'g',?,1,?,'video','f.mp4',10,X'00','pending','history',1,1)`, fmt.Sprintf("g%d", i%10), i, ixSchedIdent(i%3000, 10)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := a.ixSetRanks(ctx, []string{"g1", "g2"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`CREATE TRIGGER ix_test_no_writes BEFORE UPDATE OF sched_order ON tgdl_work BEGIN SELECT RAISE(ABORT,'rewrote a row'); END`); err != nil {
		t.Fatal(err)
	}
	ixSchedStepOK(t, a, ctx)
}

// The rescan lanes: a higher-ranked group takes the lane from a lower one,
// which carries on where it stopped, at no attempt's cost.
func TestIxRankTakesTheLaneAndTheLowerGroupResumes(t *testing.T) {
	ixBackgroundQueue.Store(false)
	defer ixBackgroundQueue.Store(true)
	a, ctx := ixTestApp(t)
	now := time.Unix(1_800_000_000, 0)
	jobs := map[string]historyJob{}
	cursor := map[string]int{}
	var started []string
	var resumed []int
	var cancelled []string
	places := map[string]int{}
	deps := ixQueueDeps{
		start: func(_ context.Context, id string, _ bool, resumeFrom int) (string, error) {
			started = append(started, id)
			resumed = append(resumed, resumeFrom)
			jid := fmt.Sprintf("job-%d", len(started))
			jobs[jid] = historyJob{ID: jid, State: "running", GroupID: id}
			cursor[jid] = 500
			return jid, nil
		},
		job:     func(_ context.Context, id string) (historyJob, bool) { j, ok := jobs[id]; return j, ok },
		cursor:  func(_ context.Context, id string) int { return cursor[id] },
		missing: func(context.Context, string) (int, error) { return 0, nil },
		finish:  func(context.Context, ixQueueItem, historyJob) error { return nil },
		now:     func() time.Time { return now },
		ranks:   func(context.Context) map[string]int { return places },
		cancel: func(_ context.Context, id string) {
			cancelled = append(cancelled, id)
			j := jobs[id]
			j.State, j.Cancelled, j.Processed = "done", true, 40
			jobs[id] = j
		},
	}
	if _, err := a.ixQueueEdit(ctx, func(st *ixQueueState) error {
		st.Running = true
		st.Items = append(st.Items, ixQueueItem{GroupID: "low", Name: "low", State: "waiting", Phase: ixPhaseMessages})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.ixQueueStep(ctx, deps); err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 || started[0] != "low" {
		t.Fatalf("started %v", started)
	}
	places["top"] = ixRankBase(0)
	if _, err := a.ixQueueEdit(ctx, func(st *ixQueueState) error {
		st.Items = append(st.Items, ixQueueItem{GroupID: "top", Name: "top", State: "waiting", Phase: ixPhaseMessages})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Not at once: a pass that just started keeps its lane for a while.
	if err := a.ixQueueStep(ctx, deps); err != nil {
		t.Fatal(err)
	}
	if len(cancelled) != 0 {
		t.Fatalf("cancelled too soon: %v", cancelled)
	}
	now = now.Add(ixPreemptAfter + time.Second)
	if err := a.ixQueueStep(ctx, deps); err != nil {
		t.Fatal(err)
	}
	if len(cancelled) != 1 {
		t.Fatalf("the lower pass was not stopped: %v", cancelled)
	}
	if err := a.ixQueueStep(ctx, deps); err != nil {
		t.Fatal(err)
	}
	if len(started) != 2 || started[1] != "top" {
		t.Fatalf("started %v, want top next", started)
	}
	st, _ := a.ixLoadQueue(ctx)
	low := st.Items[ixQueueIndex(&st, "low")]
	if low.State != "waiting" || low.Attempts != 0 || low.NextAt != 0 || low.ResumeFrom != 500 || low.Carried != 40 || low.Error != "" || low.Preempted {
		t.Fatalf("lower group after giving way: %+v", low)
	}
	// It carries on from there once the lane is free, and is not stopped again.
	for id, j := range jobs {
		if j.State == "running" {
			j.State, j.Processed = "done", 5
			jobs[id] = j
		}
	}
	if err := a.ixQueueStep(ctx, deps); err != nil {
		t.Fatal(err)
	}
	if len(started) != 3 || started[2] != "low" || resumed[2] != 500 {
		t.Fatalf("started %v resumed %v", started, resumed)
	}
}

// An equal or lower group never takes a lane; quiet hours stop it too.
func TestIxRankDoesNotTakeTheLaneWithoutReason(t *testing.T) {
	ixBackgroundQueue.Store(false)
	defer ixBackgroundQueue.Store(true)
	a, ctx := ixTestApp(t)
	now := time.Unix(1_800_000_000, 0)
	jobs := map[string]historyJob{}
	cancels := 0
	hold := ""
	places := map[string]int{"run": ixRankBase(0), "wait": ixRankBase(1)}
	n := 0
	deps := ixQueueDeps{
		start: func(_ context.Context, id string, _ bool, _ int) (string, error) {
			n++
			jid := fmt.Sprintf("job-%d", n)
			jobs[jid] = historyJob{ID: jid, State: "running", GroupID: id}
			return jid, nil
		},
		job:    func(_ context.Context, id string) (historyJob, bool) { j, ok := jobs[id]; return j, ok },
		now:    func() time.Time { return now },
		ranks:  func(context.Context) map[string]int { return places },
		hold:   func(context.Context) string { return hold },
		cancel: func(context.Context, string) { cancels++ },
	}
	if _, err := a.ixQueueEdit(ctx, func(st *ixQueueState) error {
		st.Running = true
		st.Items = append(st.Items,
			ixQueueItem{GroupID: "run", Name: "run", State: "waiting", Phase: ixPhaseMessages},
			ixQueueItem{GroupID: "wait", Name: "wait", State: "waiting", Phase: ixPhaseMessages})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.ixQueueStep(ctx, deps); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := a.ixQueueStep(ctx, deps); err != nil {
		t.Fatal(err)
	}
	if cancels != 0 {
		t.Fatal("a lower group took a lane")
	}
	places["wait"] = 1 // now it is above
	hold = "quiet hours"
	if err := a.ixQueueStep(ctx, deps); err != nil {
		t.Fatal(err)
	}
	if cancels != 0 {
		t.Fatal("a pass was stopped when nothing may start")
	}
	hold = ""
	if err := a.ixQueueStep(ctx, deps); err != nil {
		t.Fatal(err)
	}
	if cancels != 1 {
		t.Fatalf("cancels %d, want 1", cancels)
	}
}

func TestIxSpreadGoesByCoverageForRank(t *testing.T) {
	cov := func(saved, shared, total int64) ixGroupCoverage {
		return ixGroupCoverage{Total: ixCoverCount{Bytes: total}, Saved: ixCoverCount{Bytes: saved}, Shared: ixCoverCount{Bytes: shared}, Needed: ixCoverCount{Bytes: total - saved - shared}}
	}
	mostlyDone1 := ixSpreadScore(cov(80, 10, 100), 1)
	empty2 := ixSpreadScore(cov(5, 0, 100), 2)
	if empty2 >= mostlyDone1 {
		t.Fatalf("an almost empty rank 2 (%f) should go before a nearly done rank 1 (%f)", empty2, mostlyDone1)
	}
	if same1, same2 := ixSpreadScore(cov(40, 0, 100), 1), ixSpreadScore(cov(40, 0, 100), 2); same1 >= same2 {
		t.Fatalf("at the same coverage the higher rank goes first: %f %f", same1, same2)
	}
	if done := ixSpreadScore(cov(100, 0, 100), 1); done <= empty2 {
		t.Fatal("a complete group goes last")
	}
	if unknown := ixSpreadScore(ixGroupCoverage{}, 3); unknown >= mostlyDone1 {
		t.Fatal("a group not read yet needs reading")
	}
}

// Spread reorders where groups go; the ranks a person gave stay as they are.
func TestIxSpreadChangesPlacesNotRanks(t *testing.T) {
	a, ctx := ixTestApp(t)
	if err := a.ixSetRanks(ctx, []string{"one", "two"}, nil); err != nil {
		t.Fatal(err)
	}
	box := a.ixSched()
	order := []ixRankedGroup{{GroupID: "two", Base: ixRankBase(1)}, {GroupID: "one", Base: ixRankBase(0)}}
	if err := a.ixWriteBases(ctx, order); err != nil {
		t.Fatal(err)
	}
	_ = box
	ranks, _ := a.ixRanks(ctx)
	if ranks[0].GroupID != "one" || ranks[0].Rank != 1 || ranks[0].Base != ixRankBase(1) || ranks[1].Base != ixRankBase(0) {
		t.Fatalf("%+v", ranks)
	}
	// Back to order: places follow the ranks again.
	if err := a.ixSpreadUpdate(ctx, box); err != nil {
		t.Fatal(err)
	}
	ranks, _ = a.ixRanks(ctx)
	if ranks[0].Base != ixRankBase(0) || ranks[1].Base != ixRankBase(1) {
		t.Fatalf("%+v", ranks)
	}
}

// ---------- the API ----------

func TestIxPrioritiesAPI(t *testing.T) {
	a, ctx := ixTestApp(t)
	code, out := ixCall(ctx, a.handleIxPrioritiesSet, http.MethodPost, "/api/ix/priorities",
		map[string]any{"order": []string{"b", "a", "b"}, "names": map[string]string{"a": "Alpha", "b": "Beta"}, "policy": "spread"}, nil)
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, out)
	}
	ranks, _ := a.ixRanks(ctx)
	if len(ranks) != 2 || ranks[0].GroupID != "b" || ranks[0].Name != "Beta" || ranks[1].GroupID != "a" {
		t.Fatalf("%+v", ranks)
	}
	if a.ixLoadSettings(ctx).GroupPolicy != ixGroupPolicySpread {
		t.Fatal("policy not saved")
	}
	if code, _ := ixCall(ctx, a.handleIxPrioritiesSet, http.MethodPost, "/api/ix/priorities", map[string]any{"policy": "random"}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad policy: %d", code)
	}
	code, out = ixCall(ctx, a.handleIxPriorities, http.MethodGet, "/api/ix/priorities", nil, nil)
	if code != http.StatusOK || out["policy"] != "spread" {
		t.Fatalf("%d %v", code, out)
	}
	found := false
	for _, g := range out["groups"].([]any) {
		found = found || g.(map[string]any)["name"] == "Beta"
	}
	if !found {
		t.Fatalf("a ranked group with no files yet is missing: %v", out)
	}
}

// ---------- the checks' budget ----------

func TestIxBudgetGivesChecksABoundedTurnUnderPressure(t *testing.T) {
	a, _ := ixTestApp(t)
	clock := time.Unix(1_800_000_000, 0)
	oldBusy, oldNow, oldOn, oldFlood := ixRemoteChecksBusy, ixRemoteBudgetNow, ixRemoteBudgetOn, ixFloodedLately
	t.Cleanup(func() {
		ixRemoteChecksBusy, ixRemoteBudgetNow, ixRemoteBudgetOn, ixFloodedLately = oldBusy, oldNow, oldOn, oldFlood
	})
	ixRemoteBudgetOn = true
	busy, flooded := true, false
	ixRemoteChecksBusy = func(*App) bool { return busy }
	ixFloodedLately = func(*App) bool { return flooded }
	ixRemoteBudgetNow = func() time.Time { return clock }

	// A speed-limited account: "busy" all day, and the checks still get a turn.
	if ixSampleChecksBusy(a) {
		t.Fatal("no turn for samples under a long speed limit")
	}
	clock = clock.Add(ixRemoteBudgetSample + time.Second)
	if !ixSampleChecksBusy(a) {
		t.Fatal("the turn did not end")
	}
	open, total := 0, 0
	for i := 0; i < 600; i++ { // a day and a bit, in steps of ten seconds: bounded share
		clock = clock.Add(10 * time.Second)
		total++
		if !ixSampleChecksBusy(a) {
			open++
		}
	}
	if share := float64(open) / float64(total); share <= 0 || share > 0.15 {
		t.Fatalf("samples ran %.0f%% of the time", share*100)
	}
	// Previews have their own, smaller turn.
	previewClock := clock.Add(ixRemoteBudgetWindow)
	clock = previewClock
	if ixPreviewChecksBusy(a) {
		t.Fatal("no turn for previews")
	}
	clock = clock.Add(ixRemoteBudgetView + time.Second)
	if !ixPreviewChecksBusy(a) {
		t.Fatal("the previews' turn did not end")
	}
	// Telegram's FLOOD_WAIT is a penalty: nothing optional.
	flooded = true
	clock = clock.Add(2 * ixRemoteBudgetWindow)
	if !ixSampleChecksBusy(a) || !ixPreviewChecksBusy(a) {
		t.Fatal("checks ran during a flood wait")
	}
	// Nothing pressing: no limit at all.
	busy, flooded = false, false
	if ixSampleChecksBusy(a) || ixPreviewChecksBusy(a) {
		t.Fatal("limited with no pressure")
	}
}

// ---------- a simulation of what the ranking is for ----------

// Scarce capacity, overlapping groups: how much of the top group is saved, and
// how many distinct files, with and without a ranking. Synthetic, and says
// nothing of a live account's speed: it checks the mechanics, through the real
// scheduler step and the real claim.
func TestIxRankSimulationScarceCapacity(t *testing.T) {
	type outcome struct {
		topFiles, distinct, transfers int
		topLast                       int64
	}
	run := func(ranked bool) outcome {
		a, ctx := ixTestApp(t)
		store := engine.NewWorkStore(a.db.Writer, a.db.Reader)
		// "many": 300 files queued first. "top": 100 files, 60 of them the same
		// Telegram files as 60 of "many"'s; every file 10 MB, plus one 1.5 GB
		// recording only "top" has.
		const mb = int64(1 << 20)
		for i := 1; i <= 300; i++ {
			ixSchedRowAdd(t, a, "many", i, ixSchedIdent(i, 10*mb), 10*mb)
		}
		for i := 1; i <= 100; i++ {
			file := 240 + i // 241..300 are shared; 301..340 are its own
			ixSchedRowAdd(t, a, "top", i, ixSchedIdent(file, 10*mb), 10*mb)
		}
		bigOne := ixSchedRowAdd(t, a, "top", 500, ixSchedIdent(9999, 1536*mb), 1536*mb)
		if ranked {
			if err := a.ixSetRanks(ctx, []string{"top"}, nil); err != nil {
				t.Fatal(err)
			}
		} else {
			ixSchedStepOK(t, a, ctx)
		}
		// Capacity for 2.5 GB: well under what is queued.
		capacity := int64(2560) * mb
		saved := map[string]bool{}
		var out outcome
		for capacity > 0 {
			w, err := store.Claim(ctx, []string{"one"}, time.Now(), true)
			if err != nil {
				t.Fatal(err)
			}
			if w == nil {
				break
			}
			if !saved[w.Identity] {
				if w.FileSize > capacity {
					break // ran out part-way: not saved
				}
				capacity -= w.FileSize
				saved[w.Identity] = true
				out.transfers++
				if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_size,file_type,file_path,status,telegram_media_kind,telegram_media_id,telegram_media_size) VALUES(?,?,?,?,'video',?,'completed','video',?,?)`,
					w.GroupID, w.MessageID, w.FileName, w.FileSize, fmt.Sprintf("%s-%d.mp4", w.GroupID, w.MessageID), strings.Split(w.Identity, "\x00")[1], w.FileSize); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Finish(ctx, w, nil, false, time.Time{}); err != nil {
				t.Fatal(err)
			}
			if w.ID == bigOne {
				out.topLast = 1
			}
		}
		if err := a.db.Reader.QueryRow(`SELECT COUNT(*) FROM tgdl_work WHERE group_id='top' AND status='completed'`).Scan(&out.topFiles); err != nil {
			t.Fatal(err)
		}
		out.distinct = len(saved)
		return out
	}
	plain := run(false)
	ranked := run(true)
	if plain.topFiles >= 101 || plain.topLast != 0 {
		t.Fatalf("setup: without a ranking the top group should not finish: %+v", plain)
	}
	if ranked.topFiles != 101 || ranked.topLast != 1 {
		t.Fatalf("with a ranking the top group should finish, recording included: %+v", ranked)
	}
	// Credit is not content: distinct saved equals transfers, whichever way.
	if ranked.distinct != ranked.transfers || plain.distinct != plain.transfers {
		t.Fatalf("a file transferred twice: %+v %+v", ranked, plain)
	}
}

// The diagnostics report is shared to get help: names stay out of it.
func TestIxPrioritiesDiagUsesPlaceholders(t *testing.T) {
	a, ctx := ixTestApp(t)
	if err := a.ixSetRanks(ctx, []string{"-100555"}, map[string]string{"-100555": "SecretChatName"}); err != nil {
		t.Fatal(err)
	}
	lines := a.ixPrioritiesDiag(ctx, func(string) string { return "G1" })
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "SecretChatName") || strings.Contains(joined, "-100555") {
		t.Fatalf("a name or id in the report: %s", joined)
	}
	if !strings.Contains(joined, "place 1 (rank 1) G1") {
		t.Fatalf("the ranked group is missing: %s", joined)
	}
}

// The cost at the size of a real queue, with a sixth of it shared between
// groups: go test ./internal/app -run xxx -bench IxSchedAtQueueSize -benchtime 5x
func BenchmarkIxSchedAtQueueSize(b *testing.B) {
	ixVdupBackground.Store(false)
	defer ixVdupBackground.Store(true)
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: b.TempDir()})
	if err != nil {
		b.Fatal(err)
	}
	defer a.Close()
	const rows = 50000
	tx, err := a.db.Writer.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= rows; i++ {
		file := i
		if i%6 == 0 {
			file = i - 3
		}
		if _, err := tx.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at)
			VALUES('one',?,'g',?,1,?,'video','f.mp4',?,X'00','pending','history',1,1)`, fmt.Sprintf("g%d", i%48), i, ixSchedIdent(file, 10), int64(i%7)<<26); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	started := time.Now()
	if err := a.ixSetRanks(ctx, []string{"g1", "g2", "g3", "g4", "g5", "g6", "g7", "g8"}, nil); err != nil {
		b.Fatal(err)
	}
	b.Logf("first ranking, rewriting rows: %s", time.Since(started))
	store := engine.NewWorkStore(a.db.Writer, a.db.Reader)
	started = time.Now()
	for i := 0; i < 100; i++ {
		w, err := store.Claim(ctx, []string{"one"}, time.Now(), true)
		if err != nil || w == nil {
			b.Fatal(w, err)
		}
		if err := store.Finish(ctx, w, nil, false, time.Time{}); err != nil {
			b.Fatal(err)
		}
	}
	b.Logf("a claim and its finish: %s", time.Since(started)/100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := a.ixSchedStep(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// A ranked group's small files do not make the deadline order set aside the
// bigger files of everyone else: workers a ranked group leaves free stay useful.
func TestIxRankedWorkDoesNotDecideTheBandOfTheOthers(t *testing.T) {
	a, ctx := ixTestApp(t)
	if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at) VALUES('ix_settings','{"deadlineOrder":"on"}',1) ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
		t.Fatal(err)
	}
	ixSchedRowAdd(t, a, "top", 1, ixSchedIdent(1, 1<<20), 1<<20)
	other := ixSchedRowAdd(t, a, "low", 2, ixSchedIdent(2, 50<<20), 50<<20)
	if err := a.ixSetRanks(ctx, []string{"top"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.ixDeadlineStep(ctx); err != nil {
		t.Fatal(err)
	}
	if ixTestPaused(t, a, other) {
		t.Fatal("an unranked file was set aside for a ranked group's smaller one")
	}
}

// The fault that crashed 10.x: a nil pointer in a background task. It must cost
// that task a restart, not every download; and a task that ends by itself stays
// ended.
func TestIxBackgroundTaskCrashDoesNotEndTheProcess(t *testing.T) {
	a, _ := ixTestApp(t)
	old := ixPanicRestart
	ixPanicRestart = time.Millisecond
	defer func() { ixPanicRestart = old }()
	runs := make(chan int, 8)
	n := 0
	a.ixBackground("invented task", func(context.Context) {
		n++
		runs <- n
		if n < 3 {
			var cause error
			_ = cause.Error() // as ixSampleCheck did
		}
	})
	for want := 1; want <= 3; want++ {
		select {
		case got := <-runs:
			if got != want {
				t.Fatalf("run %d, want %d", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("not restarted after run %d", want-1)
		}
	}
	select {
	case got := <-runs:
		t.Fatalf("a task that ended by itself ran again: %d", got)
	case <-time.After(50 * time.Millisecond):
	}
	found := false
	rt := a.ix()
	rt.mu.Lock()
	for _, l := range rt.recent {
		found = found || strings.Contains(l.Text, "program error in invented task")
	}
	rt.mu.Unlock()
	if !found {
		t.Fatal("the fault was not reported")
	}
}

// A saved file that has since gone missing covers nothing: its rows are real
// downloads again, and no group is credited with it.
func TestIxMissingRepresentativeCoversNothing(t *testing.T) {
	a, ctx := ixTestApp(t)
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_size,file_type,file_path,status,telegram_media_kind,telegram_media_id,telegram_media_size) VALUES('other',1,'a',5,'video','c/a.mp4','completed','video','42',5)`); err != nil {
		t.Fatal(err)
	}
	row := ixSchedRowAdd(t, a, "low", 2, ixSchedIdent(42, 5), 5)
	ixSchedStepOK(t, a, ctx)
	if got := ixSchedOrder(t, a, row); got != ixFreeOrder {
		t.Fatalf("setup: order %d", got)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_fixity(path,result) VALUES('c/a.mp4','missing')`); err != nil {
		t.Fatal(err)
	}
	ixSchedStepOK(t, a, ctx)
	if got := ixSchedOrder(t, a, row); got != engine.UnrankedOrder {
		t.Fatalf("order %d: a row of a missing file still expects no download", got)
	}
	saved, err := a.ixSavedIdentities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m := ixCovFixtureModel()
	if c := ixCoverage(m, saved, nil)["low"]; c.Shared.Files != 0 || c.Needed.Files != 1 {
		t.Fatalf("credited with a missing file: %+v", c)
	}
}

func ixCovFixtureModel() *ixModel {
	return ixCoverModel(ixCovFixture{group: "low", cluster: 0, ident: "video:42:5", size: 5})
}
