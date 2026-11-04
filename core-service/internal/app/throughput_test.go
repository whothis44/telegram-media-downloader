package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// All rows below are invented fixtures.
func throughputWork(t *testing.T, a *App, msg int, size int64, status string, at time.Time) {
	t.Helper()
	if _, err := a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at)
		VALUES('acc','-1009','Fixture',?,1,?,'video','f.mp4',?,X'',?,'history',?,?)`, msg, fmt.Sprint("doc:", msg), size, status, at.UnixMilli(), at.UnixMilli()); err != nil {
		t.Fatal(err)
	}
}

func TestThroughputRecentTotalsInMonitorStatus(t *testing.T) {
	a, srv := socketTestApp(t)
	now := time.Now()
	clock := now
	a.throughput.now = func() time.Time { return clock }

	throughputWork(t, a, 1, 1000, "completed", now.Add(-10*time.Minute))  // hour + day
	throughputWork(t, a, 2, 2000, "completed", now.Add(-50*time.Minute))  // hour + day
	throughputWork(t, a, 3, 4000, "completed", now.Add(-5*time.Hour))     // day
	throughputWork(t, a, 4, 8000, "completed", now.Add(-30*time.Hour))    // neither
	throughputWork(t, a, 5, 16000, "pending", now.Add(-time.Minute))      // not completed
	throughputWork(t, a, 6, 32000, "failed", now.Add(-time.Minute))       // not completed
	throughputWork(t, a, 7, 64000, "completed", now.Add(-20*time.Minute)) // a saved copy: not downloaded
	throughputWork(t, a, 8, 128000, "completed", now.Add(-3*time.Hour))   // a saved copy: not downloaded
	for _, msg := range []int{7, 8} {
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_subst(group_id,message_id,identity,size,of_path,how,at) VALUES('-1009',?,'x',1,'p','telegram-hashes',1)`, msg); err != nil {
			t.Fatal(err)
		}
	}

	get := func() map[string]any {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+"/api/monitor/status", nil)
		req.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: socketToken(t, a, "admin", time.Hour)})
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		tp, ok := body["throughput"].(map[string]any)
		if !ok {
			t.Fatalf("no throughput in %v", body)
		}
		return tp
	}
	totals := func(tp map[string]any, key string) (float64, float64) {
		t.Helper()
		m, ok := tp[key].(map[string]any)
		if !ok {
			t.Fatalf("%s = %v", key, tp[key])
		}
		return m["files"].(float64), m["bytes"].(float64)
	}

	tp := get()
	if tp["running"] != false || tp["speed"] != nil || tp["speedSource"] != nil {
		t.Fatalf("stopped engine reported a speed: %v", tp)
	}
	if f, b := totals(tp, "lastHour"); f != 2 || b != 3000 {
		t.Fatalf("lastHour = %v files, %v bytes", f, b)
	}
	if f, b := totals(tp, "last24h"); f != 3 || b != 7000 {
		t.Fatalf("last24h = %v files, %v bytes", f, b)
	}

	// Cached: a new completion shows only once the cache is stale.
	throughputWork(t, a, 9, 500, "completed", now.Add(-time.Minute))
	clock = now.Add(10 * time.Second)
	if f, _ := totals(get(), "lastHour"); f != 2 {
		t.Fatalf("cache ignored: lastHour files = %v", f)
	}
	clock = now.Add(throughputRecentTTL + time.Second)
	if f, b := totals(get(), "lastHour"); f != 3 || b != 3500 {
		t.Fatalf("stale cache kept: lastHour = %v files, %v bytes", f, b)
	}
}

func TestThroughputRecentQueryFailureKeepsLastFigures(t *testing.T) {
	a, _ := socketTestApp(t)
	now := time.Now()
	throughputWork(t, a, 1, 1000, "completed", now.Add(-time.Minute))
	ctx := context.Background()
	hour, day := a.throughputRecent(ctx, now)
	if hour == nil || day == nil || hour.Files != 1 || day.Bytes != 1000 {
		t.Fatalf("first figures %v %v", hour, day)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	h2, d2 := a.throughputRecent(canceled, now.Add(time.Hour))
	if h2 != hour || d2 != day {
		t.Fatalf("failed query replaced figures: %v %v", h2, d2)
	}

	fresh, _ := socketTestApp(t)
	if h, d := fresh.throughputRecent(canceled, now); h != nil || d != nil {
		t.Fatalf("failed first query invented figures: %v %v", h, d)
	}
	out := fresh.throughputPayload(canceled)
	raw, _ := json.Marshal(out)
	var back map[string]any
	_ = json.Unmarshal(raw, &back)
	if back["lastHour"] != nil || back["last24h"] != nil {
		t.Fatalf("expected null totals, got %s", raw)
	}
}

func TestThroughputCounterSpeed(t *testing.T) {
	var s throughputState
	t0 := time.Unix(1_700_000_000, 0)
	if _, ok := s.counterSpeed(t0, 0); ok {
		t.Fatal("one sample gave a speed")
	}
	if _, ok := s.counterSpeed(t0.Add(500*time.Millisecond), 100); ok {
		t.Fatal("half a second gave a speed")
	}
	if v, ok := s.counterSpeed(t0.Add(2*time.Second), 2000); !ok || v != 1000 {
		t.Fatalf("2 s: %v %v", v, ok)
	}
	// Every 3 s for a minute at 3000 B/s: the figure covers about the last
	// 10 s, so an earlier burst falls out of it.
	bytes := int64(2000)
	at := t0.Add(2 * time.Second)
	for i := 0; i < 20; i++ {
		at = at.Add(3 * time.Second)
		bytes += 9000
		v, ok := s.counterSpeed(at, bytes)
		if !ok || v != 3000 && i >= 4 {
			t.Fatalf("tick %d: %v %v", i, v, ok)
		}
	}
	if len(s.samples) > 6 {
		t.Fatalf("samples not pruned: %d", len(s.samples))
	}
	// Idle: the speed falls to zero once the window has passed.
	at = at.Add(12 * time.Second)
	if v, ok := s.counterSpeed(at, bytes); !ok || v != 0 {
		t.Fatalf("idle: %v %v", v, ok)
	}
	// Nobody asked for five minutes: no stale five-minute average.
	at = at.Add(5 * time.Minute)
	if _, ok := s.counterSpeed(at, bytes+3_000_000); ok {
		t.Fatal("stale baseline used")
	}
	if v, ok := s.counterSpeed(at.Add(3*time.Second), bytes+3_000_000+30); !ok || v != 10 {
		t.Fatalf("after gap: %v %v", v, ok)
	}
	// A counter that went back (new engine) starts over.
	if _, ok := s.counterSpeed(at.Add(6*time.Second), 5); ok {
		t.Fatal("counter reset gave a speed")
	}
}

func TestThroughputSpeedWhileEngineRuns(t *testing.T) {
	const size = 1_000_000
	made := make(chan *fixtureAccount, 1)
	factory := func(_ engine.AccountConfig, _ *telegram.UpdateState, handler func(context.Context, tg.UpdatesClass) error, _ func(int64)) (engine.Account, error) {
		account := &fixtureAccount{handle: handler, download: func(_ context.Context, _ telegram.Attachment, w io.Writer) error {
			_, err := w.Write(make([]byte, size))
			return err
		}}
		made <- account
		return account, nil
	}
	a, err := newConfiguredTestApp(context.Background(), Config{DataDir: t.TempDir(), AccountFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureMonitor(t, a)
	t0 := time.Now()
	clock := t0
	a.throughput.now = func() time.Time { return clock }
	ctx := context.Background()

	if tp := a.throughputPayload(ctx); tp["running"] != false || tp["speed"] != nil {
		t.Fatalf("before start: %v", tp)
	}
	if rec := monitorRequest(t, a, "/api/monitor/start"); rec.Code != 200 {
		t.Fatalf("start=%d %s", rec.Code, rec.Body.String())
	}
	account := <-made
	update := &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 10, Date: 100, PeerID: &tg.PeerChannel{ChannelID: 42},
		Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 123, Size: size, DCID: 2, Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "fixture.bin"}}}}}}}}
	if err = account.handle(ctx, update); err != nil {
		t.Fatal(err)
	}
	waitCompleted(t, a)
	if got := a.monitor.BytesReceived(); got != size {
		t.Fatalf("received %d", got)
	}
	clock = t0.Add(4 * time.Second)
	tp := a.throughputPayload(ctx)
	if tp["running"] != true || tp["speedSource"] != "counter" || tp["speed"] != int64(size/4) {
		t.Fatalf("running: %v", tp)
	}
	if rec := monitorRequest(t, a, "/api/monitor/stop"); rec.Code != 200 {
		t.Fatalf("stop=%d %s", rec.Code, rec.Body.String())
	}
	clock = t0.Add(8 * time.Second)
	if tp := a.throughputPayload(ctx); tp["running"] != false || tp["speed"] != nil {
		t.Fatalf("after stop: %v", tp)
	}
}
