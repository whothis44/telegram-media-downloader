package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// ixGoneAccount is a fake Telegram account that counts every use, and whose
// session can be made to die the way a deleted account's does.
type ixGoneAccount struct {
	fixtureAccount
	calls *atomic.Int64
	die   chan struct{}
}

func (a *ixGoneAccount) Run(ctx context.Context, ready func()) error {
	a.calls.Add(1)
	ready()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.die:
		return errors.New("rpc error code 401: AUTH_KEY_UNREGISTERED")
	}
}

func (a *ixGoneAccount) RefreshMessage(ctx context.Context, m *tg.Message) (*telegram.RefreshedMessage, error) {
	a.calls.Add(1)
	return a.fixtureAccount.RefreshMessage(ctx, m)
}

func (a *ixGoneAccount) DownloadMedia(ctx context.Context, media telegram.Attachment, w io.Writer) error {
	a.calls.Add(1)
	return errors.New("invented: no downloads in this test")
}

// ixLockedBuffer collects the app's log output from several goroutines.
type ixLockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *ixLockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *ixLockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ixGoneFast shortens the confirmation for a test: the error on 3 items
// over 150 ms instead of 10 minutes.
func ixGoneFast(t *testing.T) {
	t.Helper()
	span, merge := ixGoneSpan, ixGoneEngineMerge
	ixGoneSpan, ixGoneEngineMerge = 150*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { ixGoneSpan, ixGoneEngineMerge = span, merge })
}

func TestIxAccountGoneStopsTelegramAndKeepsTheArchive(t *testing.T) {
	saved := monitorWatchdogEvery
	monitorWatchdogEvery = 20 * time.Millisecond // the watchdog tries to restart the engine as often as it can
	defer func() { monitorWatchdogEvery = saved }()
	delays := monitorRestartDelays
	monitorRestartDelays = []time.Duration{10 * time.Millisecond} // and restarts after an error at once
	defer func() { monitorRestartDelays = delays }()
	ixGoneFast(t)
	var connects, calls atomic.Int64
	var dead atomic.Bool // every new session dies at once, as a deleted account's does
	die := make(chan struct{})
	dead.Store(true)
	factory := func(_ engine.AccountConfig, _ *telegram.UpdateState, handler func(context.Context, tg.UpdatesClass) error, _ func(int64)) (engine.Account, error) {
		n := connects.Add(1)
		account := &ixGoneAccount{fixtureAccount: fixtureAccount{handle: handler}, calls: &calls, die: make(chan struct{})}
		switch {
		case n == 1:
			account.die = die // the first session works until the test kills it
		case dead.Load():
			close(account.die)
		}
		return account, nil
	}
	ctx := context.Background()
	out := &ixLockedBuffer{}
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir(), AccountFactory: factory, Output: out})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureMonitor(t, a)
	ixSeedIndex(t, a)
	if err := os.MkdirAll(a.downloadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if rec := monitorRequest(t, a, "/api/monitor/start"); rec.Code != 200 {
		t.Fatalf("start=%d %s", rec.Code, rec.Body.String())
	}
	if connects.Load() != 1 {
		t.Fatalf("connects=%d", connects.Load())
	}

	// Telegram says the session is gone. One engine error is not enough (no
	// live check can be made with the engine down): it must repeat, on
	// each restart of the engine, over the confirmation span.
	close(die)
	notYet := time.Now()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s; connects %d; log:\n%s", what, connects.Load(), out.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("read-only mode", a.ixIsGone)
	if took := time.Since(notYet); took < ixGoneSpan || connects.Load() < 3 {
		t.Fatalf("read-only after %v and %d sessions: not confirmed", took, connects.Load())
	}
	waitFor("the engine to stop", func() bool {
		st, _ := a.monitor.Status(ctx)
		return st["state"] == "error" || st["state"] == "stopped"
	})
	if st := a.ixGoneState(); st.Reason != "telegram" || st.Code != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("state: %+v", st)
	}
	time.Sleep(100 * time.Millisecond) // a restart begun just before ends
	usedConnects, usedCalls := connects.Load(), calls.Load()
	logAtGone := len(out.String())

	// Nothing reaches Telegram any more: not the watchdog (every 20 ms), not
	// a start by hand, not a backfill, a rescan or a freeze check.
	time.Sleep(300 * time.Millisecond)
	if rec := monitorRequest(t, a, "/api/monitor/start"); rec.Code == 200 {
		t.Fatalf("a start by hand should be refused: %s", rec.Body.String())
	}
	if err := a.startTelegramEngine(ctx, false); !errors.Is(err, errIxReadOnly) {
		t.Fatalf("engine start: %v", err)
	}
	if a.monitorShouldRestart() {
		t.Fatal("the watchdog should not restart a dead session")
	}
	if err := a.ixCheckFreeze(ctx); err == nil {
		t.Fatal("a freeze check cannot work without Telegram")
	}
	if _, err := a.ixQueueEdit(ctx, func(st *ixQueueState) error {
		st.Running = true
		st.Items = append(st.Items, ixQueueItem{GroupID: "-1000000000101", Name: "sports-1", State: "waiting", Phase: ixPhaseMessages})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ixBackgroundQueue.Store(false)
	defer ixBackgroundQueue.Store(true)
	deps := a.ixDeps()
	started := 0
	deps.start = func(context.Context, string, bool, int) (string, error) {
		started++
		return "", errors.New("must not start")
	}
	for i := 0; i < 3; i++ {
		if err := a.ixQueueStep(ctx, deps); err != nil {
			t.Fatal(err)
		}
	}
	if q, _ := a.ixLoadQueue(ctx); started != 0 || !strings.Contains(q.Notice, "read-only") {
		t.Fatalf("rescan started %d, notice %q", started, q.Notice)
	}
	a.ixGoneStep(ctx, time.Now()) // the loop's check, again: still stopped, nothing new
	time.Sleep(100 * time.Millisecond)
	if connects.Load() != usedConnects || calls.Load() != usedCalls {
		t.Fatalf("Telegram was used again: connects %d→%d, calls %d→%d", usedConnects, connects.Load(), usedCalls, calls.Load())
	}

	// The archive keeps working offline.
	token, err := a.sessions.Create(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/ix/status", "/ix/view/", "/api/ix/search?q=goal", "/api/ix/browse/info", "/api/ix/browse/feed", "/api/ix/browse/people", "/api/ix/diagnostics"} {
		rec := requestPurge(a, token, "GET", path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
		switch path {
		case "/api/ix/status":
			if !strings.Contains(rec.Body.String(), `"readonly":{"gone":true`) {
				t.Fatalf("status: %s", rec.Body.String())
			}
		case "/api/ix/search?q=goal":
			if !strings.Contains(rec.Body.String(), `"messageId":12`) {
				t.Fatalf("search: %s", rec.Body.String())
			}
		case "/api/ix/diagnostics":
			if !strings.Contains(rec.Body.String(), "read-only archive: yes") {
				t.Fatalf("report: %s", rec.Body.String())
			}
		}
	}
	if err := a.ixExport(ctx); err != nil {
		t.Fatalf("index copy: %v", err)
	}
	if err := a.ixBackup(ctx, "manual", time.Now()); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if connects.Load() != usedConnects || calls.Load() != usedCalls {
		t.Fatal("the offline features used Telegram")
	}
	// Said once in the archive log, and no restart loop in the server log.
	said := 0
	rt := a.ix()
	rt.mu.Lock()
	for _, l := range rt.recent {
		if strings.Contains(l.Text, "read-only now") {
			said++
		}
	}
	rt.mu.Unlock()
	if after := out.String()[logAtGone:]; said != 1 || strings.Count(after, "watchdog") > 0 || strings.Count(after, "Monitor restart") > 0 {
		t.Fatalf("log said it %d times; server log since:\n%s", said, after)
	}

	// "Try Telegram again" lets the (now working) account connect again.
	dead.Store(false)
	if rec := monitorRequest(t, a, "/api/ix/readonly/clear"); rec.Code != 200 || a.ixIsGone() {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	waitFor("the engine to start again", func() bool {
		st, _ := a.monitor.Status(ctx)
		return st["state"] == "running" && connects.Load() == usedConnects+1
	})
	if a.ixIsGone() {
		t.Fatal("the old error must not count after Try Telegram again")
	}
}

func ixGoneHistoryError(a *App, id, text string, at int64) {
	a.historyMu.Lock()
	defer a.historyMu.Unlock()
	a.historyJobs[id] = &historyJob{ID: id, State: "error", Error: &text, FinishedAt: &at}
}

func TestIxDeletionDateAloneKeepsTelegramGoing(t *testing.T) {
	a, ctx := ix8App(t, nil)
	now := time.Now()
	a.ixGoneStep(ctx, now) // no date, no errors: nothing
	if a.ixIsGone() {
		t.Fatal("gone without a reason")
	}
	// The date passed but Telegram still answers: keep using every hour.
	a.ixSaveFreeze(ctx, ixFreezeInfo{FreezeState: ixTestFreeze(now.Add(-time.Hour).Unix())})
	a.ixGoneStep(ctx, now)
	if a.ixIsGone() {
		t.Fatal("read-only from the date alone")
	}
	// Telegram says the account is gone. The engine is not running, so no
	// live check can be made: one error, or three close together, are not
	// enough; three over ten minutes are.
	gone := "rpc error code 401: USER_DEACTIVATED"
	ixGoneHistoryError(a, "ix-gone-1", gone, now.Add(-11*time.Minute).UnixMilli())
	a.ixGoneStep(ctx, now)
	if a.ixIsGone() {
		t.Fatal("read-only on a single error")
	}
	ixGoneHistoryError(a, "ix-gone-1", gone, now.Add(-2*time.Minute).UnixMilli())
	ixGoneHistoryError(a, "ix-gone-2", gone, now.Add(-time.Minute).UnixMilli())
	ixGoneHistoryError(a, "ix-gone-3", gone, now.UnixMilli())
	a.ixGoneStep(ctx, now)
	if a.ixIsGone() {
		t.Fatal("read-only on three errors within two minutes")
	}
	ixGoneHistoryError(a, "ix-gone-1", gone, now.Add(-11*time.Minute).UnixMilli())
	a.ixGoneStep(ctx, now)
	if st := a.ixGoneState(); !st.Gone || st.Reason != "telegram" || st.Code != "USER_DEACTIVATED" || a.ixHold(ctx) == "" {
		t.Fatalf("after Telegram's errors: %+v", st)
	}
	ixGoneBoxes.Delete(a)
	if !a.ixIsGone() {
		t.Fatal("not kept")
	}
	// Turned off by hand: older errors no longer count, fresh ones do.
	a.ixGoneClear(ctx)
	cleared := a.ixGoneState().ClearedAt
	a.ixGoneStep(ctx, time.Now())
	if a.ixIsGone() {
		t.Fatal("cleared, but gone again from old news")
	}
	fresh := "rpc error code 401: SESSION_REVOKED"
	for i, after := range []time.Duration{time.Second, 6 * time.Minute, 11 * time.Minute} {
		ixGoneHistoryError(a, "ix-gone-new-"+string(rune('a'+i)), fresh, cleared+after.Milliseconds())
	}
	a.ixGoneStep(ctx, time.UnixMilli(cleared).Add(12*time.Minute))
	if st := a.ixGoneState(); !st.Gone || st.Code != "SESSION_REVOKED" {
		t.Fatalf("fresh errors: %+v", st)
	}
	if !strings.Contains(a.ixGoneDiag(time.Now()), "Telegram said SESSION_REVOKED") {
		t.Fatal(a.ixGoneDiag(time.Now()))
	}
}

// ixGoneWorkError records a failed queue item with err, updated at at.
func ixGoneWorkError(t *testing.T, a *App, msg int, err string, at time.Time) {
	t.Helper()
	if _, e := a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at,error)
		VALUES('acc','-1009','G',?,1,'x','video','v.mp4',10,X'00','failed','live',?,?,?)
		ON CONFLICT(group_id,message_id) DO UPDATE SET updated_at=excluded.updated_at, error=excluded.error`, msg, at.UnixMilli(), at.UnixMilli(), err); e != nil {
		t.Fatal(e)
	}
}

// AUTH_KEY_UNREGISTERED on downloads (another data centre, after a
// reconnect) while the account's main connection answers: nothing stops,
// however often it repeats. When the main connection fails the same way,
// read-only mode starts at once.
func TestIxGoneNeedsTheMainConnectionToAgree(t *testing.T) {
	a, ctx := ix8App(t, nil)
	saved := ixGoneProbe
	t.Cleanup(func() { ixGoneProbe = saved })
	var probes atomic.Int64
	answer := error(nil)
	ixGoneProbe = func(*App, context.Context) error { probes.Add(1); return answer }
	now := time.Now()
	dcErr := "download: rpc error code 401: AUTH_KEY_UNREGISTERED"
	for i, ago := range []time.Duration{20 * time.Minute, 12 * time.Minute, 3 * time.Minute, time.Minute} {
		ixGoneWorkError(t, a, 100+i, dcErr, now.Add(-ago))
	}
	a.ixGoneStep(ctx, now)
	if a.ixIsGone() || probes.Load() != 1 {
		t.Fatalf("gone %v after %d live checks", a.ixIsGone(), probes.Load())
	}
	// The account status (banner, alert, rescans) ignores them too.
	errs := a.ixGoneFilter(ctx, []string{dcErr, "FROZEN_METHOD_INVALID"}, now.UnixMilli())
	if st := ixClassify(errs, now.UnixMilli()); st.Deactivated || !st.Frozen {
		t.Fatalf("account status: %+v", st)
	}
	// The same errors again: no new check, still nothing.
	a.ixGoneStep(ctx, now.Add(time.Minute))
	a.ixGoneStep(ctx, now.Add(10*time.Minute))
	if a.ixIsGone() || probes.Load() != 1 {
		t.Fatalf("gone %v after %d live checks", a.ixIsGone(), probes.Load())
	}
	// A new one, and the main connection still answers: still nothing.
	ixGoneWorkError(t, a, 200, dcErr, now.Add(11*time.Minute))
	a.ixGoneStep(ctx, now.Add(12*time.Minute))
	if a.ixIsGone() || probes.Load() != 2 {
		t.Fatalf("gone %v after %d live checks", a.ixIsGone(), probes.Load())
	}
	// Unanswered errors since the last answer still count for the status.
	ixGoneWorkError(t, a, 210, dcErr, now.Add(13*time.Minute))
	if st := ixClassify(a.ixGoneFilter(ctx, []string{dcErr}, now.Add(13*time.Minute).UnixMilli()), 0); !st.Deactivated {
		t.Fatal("a fresh error was dropped from the account status")
	}
	// The engine is down (no live check): one new error is not enough.
	answer = errIxNoProbe
	ixGoneWorkError(t, a, 201, dcErr, now.Add(20*time.Minute))
	a.ixGoneStep(ctx, now.Add(20*time.Minute))
	if a.ixIsGone() {
		t.Fatal("read-only on one error without a live check")
	}
	// The main connection fails the same way: read-only at once.
	answer = errors.New("rpc error code 401: AUTH_KEY_UNREGISTERED")
	a.ixGoneStep(ctx, now.Add(21*time.Minute))
	if st := a.ixGoneState(); !st.Gone || st.Code != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("confirmed on the main connection: %+v", st)
	}
}

// The live check through the engine: a running account whose main
// connection answers keeps the engine running despite download errors.
func TestIxGoneLiveCheckKeepsAWorkingEngineRunning(t *testing.T) {
	var freeze atomic.Int64
	factory := func(_ engine.AccountConfig, _ *telegram.UpdateState, handler func(context.Context, tg.UpdatesClass) error, _ func(int64)) (engine.Account, error) {
		return &ixAliveAccount{fixtureAccount: fixtureAccount{handle: handler}, freeze: &freeze}, nil
	}
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir(), AccountFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureMonitor(t, a)
	if err := os.MkdirAll(a.downloadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if rec := monitorRequest(t, a, "/api/monitor/start"); rec.Code != 200 {
		t.Fatalf("start=%d %s", rec.Code, rec.Body.String())
	}
	now := time.Now()
	for i := 0; i < 5; i++ {
		ixGoneWorkError(t, a, 300+i, "rpc error code 401: AUTH_KEY_UNREGISTERED", now.Add(-time.Duration(30-5*i)*time.Minute))
	}
	a.ixGoneStep(ctx, now)
	if a.ixIsGone() || freeze.Load() != 1 {
		t.Fatalf("gone %v, live checks %d", a.ixIsGone(), freeze.Load())
	}
	if st, _ := a.monitor.Status(ctx); st["state"] != "running" {
		t.Fatalf("engine %v", st["state"])
	}
}

type ixAliveAccount struct {
	fixtureAccount
	freeze *atomic.Int64
}

func (a *ixAliveAccount) RecoveryDialogs(context.Context, bool) ([]telegram.Dialog, error) {
	return nil, nil
}
func (a *ixAliveAccount) ProbeDialog(context.Context, telegram.Dialog) error { return nil }
func (a *ixAliveAccount) ResolveDialog(context.Context, string) (telegram.Dialog, error) {
	return telegram.Dialog{}, errors.New("invented: no chats in this test")
}

func (a *ixAliveAccount) HistoryPage(context.Context, telegram.Dialog, telegram.HistoryRequest) (telegram.HistoryPage, error) {
	return telegram.HistoryPage{}, nil
}

func (a *ixAliveAccount) FreezeState(context.Context) (telegram.FreezeState, error) {
	a.freeze.Add(1)
	return telegram.FreezeState{}, nil
}
