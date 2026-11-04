package app

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/ws"
)

func TestAmbientMonitorUsesHTTPProjectionAndStatsAreLive(t *testing.T) {
	a, srv := socketTestApp(t)
	token := socketToken(t, a, "admin", time.Hour)
	conn := socketDial(t, a, srv, token)
	guest := socketDial(t, a, srv, socketToken(t, a, "guest", time.Hour))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var event struct {
		Type    string
		Payload map[string]any
	}
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "monitor_status_push" {
		t.Fatal(event.Type)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/monitor/status", nil)
	req.AddCookie(&http.Cookie{Name: a.sessions.CookieName(), Value: token})
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var actual map[string]any
	if err = json.NewDecoder(res.Body).Decode(&actual); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(actual)
	got, _ := json.Marshal(event.Payload)
	if string(want) != string(got) {
		t.Fatalf("HTTP %s != WS %s", want, got)
	}
	requireSocketEvent(t, guest, "monitor_status_push")
	rescueInsert(t, a, 1, "one.bin", nil, nil, 0)
	if err = a.pushStats(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = conn.ReadJSON(&event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "stats_push" || event.Payload["totalFiles"] != float64(1) || event.Payload["totalSize"] != float64(4) {
		t.Fatal(event)
	}
	requireSocketEvent(t, guest, "stats_push")
}

func TestAmbientNoSubscribersNoQueriesAndSlowPushDoesNotOverlap(t *testing.T) {
	a, _ := socketTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int64
	go func() {
		defer close(done)
		a.runPushes(ctx, 5*time.Millisecond, func(ctx context.Context) error {
			if calls.Add(1) == 1 {
				close(entered)
			}
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("idle loop built a snapshot")
	}
	client := a.hub.Add("admin")
	defer a.hub.Remove(client)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no push for subscriber")
	}
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("overlapping snapshot builds")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("push loop did not join")
	}
	if calls.Load() != 1 {
		t.Fatalf("push loop restarted after cancellation: %d calls", calls.Load())
	}
}

func TestAmbientQueryFailureDoesNotPublishInventedCounters(t *testing.T) {
	a, _ := socketTestApp(t)
	c := a.hub.Add("guest")
	defer a.hub.Remove(c)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.pushStats(ctx); err == nil {
		t.Fatal("canceled stats succeeded")
	}
	if err := a.pushMonitorStatus(ctx); err == nil {
		t.Fatal("canceled monitor snapshot succeeded")
	}
	select {
	case event := <-c.Events():
		t.Fatalf("invented state: %+v", event)
	default:
	}
}

// The download throughput is the operator's: a guest's monitor status,
// over HTTP and over the websocket, has none.
func TestMonitorStatusThroughputIsForAdminsOnly(t *testing.T) {
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
	if _, ok := status(adminToken)["throughput"]; !ok {
		t.Fatal("no throughput for an admin")
	}
	g := status(guestToken)
	if _, ok := g["throughput"]; ok {
		t.Fatalf("throughput sent to a guest: %v", g)
	}
	if _, ok := g["state"]; !ok {
		t.Fatalf("the guest's status lost more than the throughput: %v", g)
	}
	admin, guest := a.hub.Add("admin"), a.hub.Add("guest")
	defer a.hub.Remove(admin)
	defer a.hub.Remove(guest)
	if err := a.pushMonitorStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	read := func(c interface{ Events() <-chan ws.Event }) []ws.Event {
		var out []ws.Event
		for {
			select {
			case e := <-c.Events():
				out = append(out, e)
			default:
				return out
			}
		}
	}
	ae, ge := read(admin), read(guest)
	if len(ae) != 1 || len(ge) != 1 || ae[0].Type != "monitor_status_push" || ge[0].Type != "monitor_status_push" {
		t.Fatalf("admin got %+v, guest got %+v", ae, ge)
	}
	if _, ok := ae[0].Payload.(map[string]any)["throughput"]; !ok {
		t.Fatal("no throughput in the admin's push")
	}
	if _, ok := ge[0].Payload.(map[string]any)["throughput"]; ok {
		t.Fatal("throughput in the guest's push")
	}
}
