package app

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/ws"
)

// Every chat, file and number below is invented.

func init() { ixDlStateTTL = 0 }

func ixTestDefer(t *testing.T, a *App, id int64, at time.Time) {
	t.Helper()
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_deferred(work_id,band,deferred_at,released_at) VALUES(?,1,?,0)`, id, at.UnixMilli()); err != nil {
		t.Fatal(err)
	}
}

// ixTestQuietQueue keeps the background rescan loop off the invented
// rescan queue (it would start or reset its groups).
func ixTestQuietQueue(t *testing.T) {
	t.Helper()
	ixBackgroundQueue.Store(false)
	t.Cleanup(func() { ixBackgroundQueue.Store(true) })
}

func ixTestPace(t *testing.T, premium, floods int) {
	t.Helper()
	old := ixDlPace
	ixDlPace = func(*App) ixPace { return ixPace{Premium10m: premium, Floods10m: floods} }
	t.Cleanup(func() { ixDlPace = old })
}

// The words the user reads: plain, no Telegram error names or internal terms.
func ixTestPlain(t *testing.T, s string) {
	t.Helper()
	for _, bad := range []string{"FLOOD", "band", "ix_", "deferred", "backfill", "G1"} {
		if strings.Contains(s, bad) {
			t.Fatalf("advice uses %q: %s", bad, s)
		}
	}
	if s == "" {
		t.Fatal("no advice")
	}
}

// Each verdict, the order they are chosen in, and the sentence that goes
// with each.
func TestIxDlStateVerdicts(t *testing.T) {
	base := func() ixDlState { return ixDlState{EngineRunning: true, Ready: 40, Downloading: 6} }
	cases := []struct {
		name    string
		edit    func(*ixDlState)
		verdict string
		act     bool
		advice  string
	}{
		{"normal", func(*ixDlState) {}, ixDlDownloading, false, "files are downloading"},
		{"queue paused", func(s *ixDlState) { s.QueuePaused = true; s.QuietMin = 60 }, ixDlPaused, true, "queue is paused"},
		{"engine stopped", func(s *ixDlState) { s.EngineRunning = false; s.QuietMin = 60 }, ixDlPaused, true, "Start the monitor"},
		{"read-only before stopped", func(s *ixDlState) { s.ReadOnly, s.EngineRunning = true, false }, ixDlReadOnly, false, "read-only"},
		{"stalled", func(s *ixDlState) { s.QuietMin = 12 }, ixDlStalled, true, "send a diagnostics report"},
		{"stalled with only set-aside work", func(s *ixDlState) { s.Ready, s.Downloading, s.Aside.Deadline, s.QuietMin = 0, 0, 30, 15 }, ixDlStalled, true, "nothing has arrived for 15 minutes"},
		{"stalled beats speed limit", func(s *ixDlState) { s.QuietMin, s.SpeedWaits = 11, 3 }, ixDlStalled, true, "Stalled"},
		{"quiet but nothing that goes by itself", func(s *ixDlState) { s.Ready, s.Downloading, s.Aside.ByYou, s.QuietMin = 0, 0, 7, 600 }, ixDlDone, false, "7 files you paused still wait"},
		{"speed limited", func(s *ixDlState) { s.SpeedWaits = 4; s.QuietMin = 3 }, ixDlSpeedLimited, false, "Telegram's speed limit sets the pace"},
		{"other waits count too", func(s *ixDlState) { s.OtherWaits = 1 }, ixDlSpeedLimited, false, "more queued files would not help"},
		{"speed limit beats filling", func(s *ixDlState) { s.Ready, s.Downloading, s.SpeedWaits, s.Rescan.Adding = 0, 2, 5, true }, ixDlSpeedLimited, false, "speed limit"},
		{"filling", func(s *ixDlState) {
			s.Ready, s.Downloading, s.Aside.Deadline = 0, 2, 900
			s.Rescan = ixDlRescan{On: true, Adding: true, Left: 3, Total: 9}
		}, ixDlFilling, false, "the rescan is adding files group by group"},
		{"filling while the rescan waits for quiet hours", func(s *ixDlState) {
			s.Ready, s.Downloading = 0, 1
			s.Rescan = ixDlRescan{On: true, Held: true, Left: 3}
		}, ixDlFilling, false, "goes on by itself"},
		{"filling, but paused files fill the queue", func(s *ixDlState) {
			s.Ready, s.Downloading, s.Aside.ByYou = 0, 0, 500
			s.Rescan = ixDlRescan{On: true, Adding: true, QueueFull: true, Left: 3}
		}, ixDlFilling, true, "500 files you paused fill it"},
		{"three downloading is not filling", func(s *ixDlState) {
			s.Ready, s.Downloading = 0, 3
			s.Rescan = ixDlRescan{On: true, Adding: true, Left: 3}
		}, ixDlDownloading, false, "files are downloading"},
		{"done", func(s *ixDlState) { s.Ready, s.Downloading, s.QuietMin = 0, 0, 90 }, ixDlDone, false, "add groups on the Rescan tab"},
		{"done with copies held for ever", func(s *ixDlState) {
			s.Ready, s.Downloading, s.Aside.Copies, s.heldForever = 0, 0, 12, true
		}, ixDlDone, false, "12 likely copies are held until you decide"},
		{"copies held for a while are not done", func(s *ixDlState) { s.Ready, s.Downloading, s.Aside.Copies = 0, 0, 12 }, ixDlDownloading, false, "go next, by themselves"},
		{"rescan paused by the user", func(s *ixDlState) {
			s.Ready, s.Downloading = 0, 0
			s.Rescan = ixDlRescan{Left: 4, Total: 10}
		}, ixDlDownloading, true, "Start it on the Rescan tab"},
		{"retry later", func(s *ixDlState) { s.Ready, s.Downloading, s.RetryLater = 0, 0, 2 }, ixDlDownloading, false, "2 files wait to be tried again"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := base()
			c.edit(&st)
			ixDlDecide(&st)
			if st.Verdict != c.verdict {
				t.Fatalf("verdict %q, want %q", st.Verdict, c.verdict)
			}
			advice, act := ixDlAdvice(st)
			if act != c.act || !strings.Contains(strings.ToLower(advice), strings.ToLower(c.advice)) {
				t.Fatalf("advice %q (act %v), want %q (act %v)", advice, act, c.advice, c.act)
			}
			ixTestPlain(t, advice)
			// Scanning more is never suggested while the rescan has groups left.
			if st.Rescan.On && st.Rescan.Left > 0 && strings.Contains(advice, "Rescan tab") {
				t.Fatalf("suggests scanning while the rescan runs: %s", advice)
			}
		})
	}
	// The safety net, in words.
	st := base()
	st.QuietMin, st.Verdict, st.SafetyNet, st.SafetyNetMin = 14, ixDlStalled, "soon", 18
	if s, _ := ixDlAdvice(st); !strings.Contains(s, "let go in about 18 minutes") {
		t.Fatal(s)
	}
	st.SafetyNet, st.SafetyNetMin = "on", 95
	if s, _ := ixDlAdvice(st); !strings.Contains(s, "already let go (for about 95 more minutes)") {
		t.Fatal(s)
	}
}

// The reasons files wait, from the queue itself: each file once, under its
// first reason; big files the lane would take now count as ready.
func TestIxDlStateSplitsTheQueueOnce(t *testing.T) {
	a, ctx := ix8App(t, nil)
	ixTestEngine(t, []string{"acc"}, true, true)
	ixTestBytes(t)
	ixTestLane(t, 2)
	now := time.Now()
	g := "-1007001"
	mk := func(msg int, size int64, paused int) int64 {
		return ixTestWork(t, a, "acc", g, msg, "video", size, paused)
	}
	big := engine.BigFileSize

	mk(1, 1<<20, 0) // ready
	mk(2, 2<<20, 0) // ready
	run := mk(3, 3<<20, 0)
	bigRun := mk(4, big+1, 0)
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET status='processing' WHERE id IN (?,?)`, run, bigRun); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // the lane has room for one more of these
		mk(10+i, big+int64(i), 0)
	}
	retry := mk(20, 1<<20, 0)
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET retry_at=? WHERE id=?`, now.Add(time.Hour).UnixMilli(), retry); err != nil {
		t.Fatal(err)
	}
	// Held and set aside: counts once, as a copy.
	both := mk(30, 5<<20, 1)
	ixTestHold(t, a, both, "", 0)
	ixTestDefer(t, a, both, now)
	copyOnly := mk(31, 5<<20, 1)
	ixTestHold(t, a, copyOnly, "", 0)
	// Set aside and a lower-quality original: counts once, set aside.
	deferLowq := mk(32, 50<<20, 1)
	ixTestDefer(t, a, deferLowq, now)
	ixTestLowq(t, a, deferLowq)
	ixTestDefer(t, a, mk(33, 60<<20, 1), now)
	ixTestLowq(t, a, mk(34, 70<<20, 1))
	mk(35, 1<<20, 1) // paused by a person
	mk(36, 1<<20, 1)
	// Let go (paused=0) though its rows remain: the engine can take it.
	freed := mk(37, 1<<20, 0)
	ixTestDefer(t, a, freed, now)
	// Finished and failed items are not in the queue.
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET status='completed' WHERE id=?`, mk(40, 1<<20, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET status='failed' WHERE id=?`, mk(41, 1<<20, 0)); err != nil {
		t.Fatal(err)
	}

	st := a.ixDlState(ctx)
	want := ixDlAside{Copies: 2, Deadline: 2, LowerQuality: 1, BigLane: 2, ByYou: 2}
	if st.Aside != want {
		t.Fatalf("aside %+v, want %+v", st.Aside, want)
	}
	// 1, 2, 37, and one big file the lane takes now.
	if st.Ready != 4 || st.Downloading != 2 || st.RetryLater != 1 || st.SetAside != 9 {
		t.Fatalf("ready %d downloading %d retry %d set aside %d", st.Ready, st.Downloading, st.RetryLater, st.SetAside)
	}
	if total := st.Ready + st.Downloading + st.RetryLater + st.SetAside; total != 16 || st.ToDownload != 16 {
		t.Fatalf("counted %d, to download %d; the queue holds 16", total, st.ToDownload)
	}
	if st.Verdict != ixDlDownloading {
		t.Fatalf("verdict %q", st.Verdict)
	}

	// With nothing smaller to take, the lane takes twice as many.
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET paused=1 WHERE status='pending' AND paused=0 AND file_size<?`, big); err != nil {
		t.Fatal(err)
	}
	st = a.ixDlState(ctx)
	if st.Ready != 3 || st.Aside.BigLane != 0 {
		t.Fatalf("ready %d, big lane %d", st.Ready, st.Aside.BigLane)
	}
	// No lane limit: no big file waits for it.
	ixTestLane(t, 0)
	if st = a.ixDlState(ctx); st.Aside.BigLane != 0 {
		t.Fatalf("big lane %d with no limit", st.Aside.BigLane)
	}
}

// The verdicts from a whole app: the queue, the progress watch, Telegram's
// waits and the rescan queue as they are stored.
func TestIxDlStateFromTheApp(t *testing.T) {
	ixTestQuietQueue(t)
	a, ctx := ix8App(t, nil)
	ixTestBytes(t)
	ixTestPace(t, 0, 0)
	g := "-1007002"
	verdict := func(want string) ixDlState {
		t.Helper()
		st := a.ixDlState(ctx)
		if st.Verdict != want {
			t.Fatalf("verdict %q, want %q (%+v)", st.Verdict, want, st)
		}
		ixTestPlain(t, st.Advice)
		return st
	}

	// Nothing queued, no rescan, no engine: stopped.
	ixTestEngine(t, nil, false, false)
	verdict(ixDlPaused)
	ixTestEngine(t, []string{"acc"}, true, true)
	verdict(ixDlDone)

	// Files ready, bytes arriving.
	ixTestNoBytesFor(a, 0)
	for i := 1; i <= 5; i++ {
		ixTestWork(t, a, "acc", g, i, "photo", 1<<20, 0)
	}
	verdict(ixDlDownloading)
	ixTestPace(t, 3, 0)
	verdict(ixDlSpeedLimited)
	ixTestPace(t, 0, 0)

	// The queue paused by a person.
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_queue_state SET paused=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	verdict(ixDlPaused)
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_queue_state SET paused=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	// Stalled: nothing moved for 12 minutes, with files set aside by the
	// deadline order for 20. The net needs 30 minutes of both, and its
	// check runs every 2: due in about 18 + 2.
	ixTestDefer(t, a, ixTestWork(t, a, "acc", g, 50, "video", 900<<20, 1), time.Now().Add(-20*time.Minute))
	ixTestNoBytesFor(a, 12*time.Minute)
	st := verdict(ixDlStalled)
	if st.SafetyNet != "soon" || st.SafetyNetMin < 19 || st.SafetyNetMin > 21 || !st.Act {
		t.Fatalf("safety net %q in %d min", st.SafetyNet, st.SafetyNetMin)
	}
	// It kicked in: set-aside files go for two hours.
	if _, err := a.db.Writer.Exec(`INSERT INTO kv(key,value,updated_at) VALUES('ix_deadline_suspend',?,0)`, time.Now().Add(90*time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if st = verdict(ixDlStalled); st.SafetyNet != "on" || st.SafetyNetMin < 89 || st.SafetyNetMin > 90 {
		t.Fatalf("safety net %q for %d min", st.SafetyNet, st.SafetyNetMin)
	}
	ixTestNoBytesFor(a, 0)

	// Filling: nothing ready, one downloading, a files pass running.
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET status='completed' WHERE message_id BETWEEN 2 AND 5`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET status='processing' WHERE message_id=1`); err != nil {
		t.Fatal(err)
	}
	if err := a.ixSaveQueue(ctx, ixQueueState{Running: true, Items: []ixQueueItem{
		{GroupID: "-1007003", Name: "Invented One", State: "done", Phase: ixPhaseFiles},
		{GroupID: "-1007004", Name: "Invented Two", State: "running", Phase: ixPhaseMessages},
		{GroupID: "-1007005", Name: "Invented Three", State: "running", Phase: ixPhaseFiles, MessagesAt: 1, Missing: 300, Downloaded: 120},
		{GroupID: "-1007006", Name: "Invented Four", State: "waiting", Phase: ixPhaseFiles, MessagesAt: 1, Missing: 80},
		{GroupID: "-1007007", Name: "Invented Five", State: "waiting", Phase: ixPhaseMessages},
		{GroupID: "-1007008", Name: "Invented Six", State: "failed", Phase: ixPhaseMessages},
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	st = verdict(ixDlFilling)
	r := st.Rescan
	if !r.On || !r.Adding || r.Total != 6 || r.Done != 1 || r.Failed != 1 || r.Left != 4 || r.Unread != 2 || r.NotQueued != 260 {
		t.Fatalf("rescan %+v", r)
	}
	if r.Group != "Invented Three" || r.Phase != ixPhaseFiles {
		t.Fatalf("reading %q (%s): the files pass is the one that adds files", r.Group, r.Phase)
	}
	if st.ToDownload != 1+1+260 {
		t.Fatalf("to download %d", st.ToDownload)
	}
	if strings.Contains(st.Advice, "Rescan tab") {
		t.Fatal(st.Advice)
	}

	// The rescan paused with groups left, nothing ready: start it.
	if err := a.ixSaveQueue(ctx, ixQueueState{Running: false, Items: []ixQueueItem{{GroupID: "-1007004", State: "waiting", Phase: ixPhaseMessages}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if st = verdict(ixDlDownloading); !st.Act || !strings.Contains(st.Advice, "Rescan tab") {
		t.Fatalf("advice %q", st.Advice)
	}

	// Read-only.
	box := a.ixGoneBox()
	box.mu.Lock()
	box.st = ixGoneInfo{Gone: true, Reason: "date"}
	box.mu.Unlock()
	verdict(ixDlReadOnly)
}

// The status is the operator's: a guest's monitor status, over HTTP and
// over the websocket, has none (it names the group being read).
func TestIxDlStateIsForAdminsOnly(t *testing.T) {
	a, srv := socketTestApp(t)
	adminToken, guestToken := socketToken(t, a, "admin", time.Hour), socketToken(t, a, "guest", time.Hour)
	status := func(token string) map[string]any {
		req, _ := http.NewRequest("GET", srv.URL+"/api/monitor/status", nil)
		req.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: token})
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&out) != nil {
			t.Fatalf("status %d", res.StatusCode)
		}
		return out
	}
	ds, ok := status(adminToken)["downloadState"].(map[string]any)
	if !ok || ds["verdict"] == "" || ds["aside"] == nil || ds["advice"] == "" {
		t.Fatalf("admin's download state: %v", ds)
	}
	g := status(guestToken)
	if _, ok := g["downloadState"]; ok {
		t.Fatalf("download state sent to a guest: %v", g)
	}
	if _, ok := g["state"]; !ok {
		t.Fatalf("the guest's status lost more than it should: %v", g)
	}
	admin, guest := a.hub.Add("admin"), a.hub.Add("guest")
	defer a.hub.Remove(admin)
	defer a.hub.Remove(guest)
	if err := a.pushMonitorStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := func(c interface{ Events() <-chan ws.Event }) map[string]any {
		select {
		case e := <-c.Events():
			return e.Payload.(map[string]any)
		default:
			t.Fatal("no push")
			return nil
		}
	}
	if _, ok := first(admin)["downloadState"]; !ok {
		t.Fatal("no download state in the admin's push")
	}
	if _, ok := first(guest)["downloadState"]; ok {
		t.Fatal("download state in the guest's push")
	}
}

// The diagnostics report's line holds numbers only: the group being read
// is a placeholder, never its name or ID. The Archive page (the user's own)
// shows the name.
func TestIxDlStateDiagnosticsLineHasNoNames(t *testing.T) {
	ixTestQuietQueue(t)
	a, ctx := ix8App(t, nil)
	ixTestEngine(t, []string{"acc"}, true, true)
	ixTestBytes(t)
	ixTestPace(t, 2, 1)
	ixTestNoBytesFor(a, 0)
	name, id := "Invented Private Chat Name", "-1009876543"
	ixTestWork(t, a, "acc", id, 1, "photo", 1<<20, 0)
	if err := a.ixSaveQueue(ctx, ixQueueState{Running: true, Items: []ixQueueItem{{GroupID: id, Name: name, State: "running", Phase: ixPhaseFiles, MessagesAt: 1, Missing: 40}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := a.ixDlState(ctx).Rescan.Group; got != name {
		t.Fatalf("the Archive page's group %q", got)
	}
	var line string
	for _, l := range strings.Split(a.ixDiagnostics(ctx), "\n") {
		if strings.HasPrefix(l, "download state: ") {
			line = l
		}
	}
	if line == "" {
		t.Fatal("no download state line in the report")
	}
	if strings.Contains(line, "Invented") || strings.Contains(line, "Private") || strings.Contains(line, "9876543") {
		t.Fatalf("the report names the chat: %s", line)
	}
	if !regexp.MustCompile(`reading G\d+ \(files\)`).MatchString(line) || !strings.Contains(line, "speed-limited") || !strings.Contains(line, "2 speed limit, 1 other") {
		t.Fatalf("line: %s", line)
	}
	// Apart from the placeholder, the vocabulary is fixed.
	words := regexp.MustCompile(`[A-Za-z][A-Za-z-]*`).FindAllString(regexp.MustCompile(`G\d+`).ReplaceAllString(line, ""), -1)
	allowed := map[string]bool{}
	for _, w := range strings.Fields("download state speed-limited ready downloading retry later set aside copies private smaller first lower quality big-file lane paused by a person quiet min Telegram waits in speed limit other safety net rescan on adding held queue full true false groups done failed left not read of reading files to queued yet") {
		allowed[w] = true
	}
	for _, w := range words {
		if !allowed[w] {
			t.Fatalf("unexpected word %q in %s", w, line)
		}
	}
}
