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

func TestIxAccountGoneStopsTelegramAndKeepsTheArchive(t *testing.T) {
	saved := monitorWatchdogEvery
	monitorWatchdogEvery = 20 * time.Millisecond // the watchdog tries to restart the engine as often as it can
	defer func() { monitorWatchdogEvery = saved }()
	var connects, calls atomic.Int64
	die := make(chan struct{})
	factory := func(_ engine.AccountConfig, _ *telegram.UpdateState, handler func(context.Context, tg.UpdatesClass) error, _ func(int64)) (engine.Account, error) {
		n := connects.Add(1)
		account := &ixGoneAccount{fixtureAccount: fixtureAccount{handle: handler}, calls: &calls, die: make(chan struct{})}
		if n == 1 {
			account.die = die // the first session dies; a later one (after "Try Telegram again") works
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

	// Telegram says the session is gone.
	close(die)
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("read-only mode", a.ixIsGone)
	waitFor("the engine to stop", func() bool {
		st, _ := a.monitor.Status(ctx)
		return st["state"] == "error" || st["state"] == "stopped"
	})
	if st := a.ixGoneState(); st.Reason != "telegram" || st.Code != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("state: %+v", st)
	}
	usedConnects, usedCalls := connects.Load(), calls.Load()

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
	deps.start = func(context.Context, string, bool, int) (string, error) { started++; return "", errors.New("must not start") }
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
	if said != 1 || strings.Count(out.String(), "watchdog") > 0 || strings.Count(out.String(), "Monitor restart") > 0 {
		t.Fatalf("log said it %d times; server log:\n%s", said, out.String())
	}

	// "Try Telegram again" lets the (now working) account connect again.
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

func TestIxDeletionDatePassedMakesTheArchiveReadOnly(t *testing.T) {
	a, ctx := ix8App(t, nil)
	now := time.Now()
	a.ixGoneStep(ctx, now) // no date, no errors: nothing
	if a.ixIsGone() {
		t.Fatal("gone without a reason")
	}
	a.ixSaveFreeze(ctx, ixFreezeInfo{FreezeState: ixTestFreeze(now.Add(-time.Hour).Unix())})
	a.ixGoneStep(ctx, now.Add(-2*time.Hour))
	if a.ixIsGone() {
		t.Fatal("gone before the date")
	}
	a.ixGoneStep(ctx, now)
	if st := a.ixGoneState(); !st.Gone || st.Reason != "date" || a.ixHold(ctx) == "" {
		t.Fatalf("after the date: %+v", st)
	}
	// Kept across a restart of the app (read back from kv).
	ixGoneBoxes.Delete(a)
	if !a.ixIsGone() {
		t.Fatal("not kept")
	}
	// Turned off by hand: the date no longer counts, older errors neither,
	// a fresh one does.
	old := "rpc error code 401: USER_DEACTIVATED"
	done := time.Now().UnixMilli() - 1000
	a.historyMu.Lock()
	a.historyJobs["ix-gone-old"] = &historyJob{ID: "ix-gone-old", State: "error", Error: &old, FinishedAt: &done}
	a.historyMu.Unlock()
	a.ixGoneClear(ctx)
	a.ixGoneStep(ctx, time.Now())
	if a.ixIsGone() {
		t.Fatal("cleared, but gone again from old news")
	}
	fresh := "rpc error code 401: SESSION_REVOKED"
	later := time.Now().UnixMilli() + 1000
	a.historyMu.Lock()
	a.historyJobs["ix-gone-new"] = &historyJob{ID: "ix-gone-new", State: "error", Error: &fresh, FinishedAt: &later}
	a.historyMu.Unlock()
	a.ixGoneStep(ctx, time.Now())
	if st := a.ixGoneState(); !st.Gone || st.Code != "SESSION_REVOKED" {
		t.Fatalf("fresh error: %+v", st)
	}
	if !strings.Contains(a.ixGoneDiag(time.Now()), "Telegram said SESSION_REVOKED") {
		t.Fatal(a.ixGoneDiag(time.Now()))
	}
}
