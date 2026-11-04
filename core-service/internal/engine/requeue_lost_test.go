package engine

import (
	"context"
	"testing"
	"time"
)

// A staged file lost after its queue item completed: the item goes back in
// the queue with its message; a waiting item is left alone; a removed one
// is queued anew; a skipped one stays skipped.
func TestRequeueLostPutsAFinishedMessageBackInTheQueue(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	msg := testMessage(20, 0, 20)
	if _, _, err := s.Enqueue(ctx, "one", Target{}, msg); err != nil {
		t.Fatal(err)
	}
	w, err := s.Claim(ctx, []string{"one"}, time.Now())
	if err != nil || w == nil {
		t.Fatalf("claim: %+v %v", w, err)
	}
	if err := s.Finish(ctx, w, nil, false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	var status string
	var body int
	if err := s.reader.QueryRow(`SELECT status, length(body) FROM tgdl_work WHERE message_id=20`).Scan(&status, &body); err != nil || status != "completed" || body != 0 {
		t.Fatalf("finished: %s %d %v", status, body, err)
	}
	if ok, err := s.RequeueLost(ctx, "one", Target{}, msg, "live"); err != nil || !ok {
		t.Fatalf("requeue: %t %v", ok, err)
	}
	again, err := s.Claim(ctx, []string{"one"}, time.Now())
	if err != nil || again == nil || again.MessageID != 20 || again.Generation <= w.Generation || again.Attempts != 1 {
		t.Fatalf("claim again: %+v %v", again, err)
	}
	if m, err := again.Message(); err != nil || m.ID != 20 {
		t.Fatalf("message: %v", err)
	}
	// Running: left alone (it will deliver the file).
	if ok, err := s.RequeueLost(ctx, "one", Target{}, msg, "live"); err != nil || !ok {
		t.Fatalf("while running: %t %v", ok, err)
	}
	if err := s.reader.QueryRow(`SELECT status FROM tgdl_work WHERE message_id=20`).Scan(&status); err != nil || status != "processing" {
		t.Fatalf("running item changed: %s %v", status, err)
	}
	// Removed from the queue list: queued anew.
	if _, err := s.writer.Exec(`DELETE FROM tgdl_work`); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RequeueLost(ctx, "one", Target{}, msg, "history"); err != nil || !ok {
		t.Fatalf("removed item: %t %v", ok, err)
	}
	var origin string
	if err := s.reader.QueryRow(`SELECT status, origin FROM tgdl_work WHERE message_id=20`).Scan(&status, &origin); err != nil || status != "pending" || origin != "history" {
		t.Fatalf("queued anew: %s %s %v", status, origin, err)
	}
	// Skipped since: stays so.
	if _, err := s.writer.Exec(`UPDATE tgdl_work SET status='skipped', body=X''`); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RequeueLost(ctx, "one", Target{}, msg, "live"); err != nil || ok {
		t.Fatalf("skipped: %t %v", ok, err)
	}
}
