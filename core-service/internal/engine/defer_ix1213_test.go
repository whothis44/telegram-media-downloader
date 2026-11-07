package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// ix.12.13: a download whose check decides the file waits goes back to the
// queue pending and paused, its attempt not spent, with the hold written in
// the same transaction; a failed hold leaves it pending and unpaused, and is
// claimed again (no loop: the app does not defer a row whose hold failed).
func TestIx1213DeferredDownloadIsHeldWithoutSpendingAnAttempt(t *testing.T) {
	for _, failHold := range []bool{false, true} {
		t.Run(fmt.Sprintf("failHold=%v", failHold), func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t)
			if _, err := s.writer.Exec(`CREATE TABLE held(work_id INTEGER PRIMARY KEY)`); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Enqueue(ctx, "one", Target{}, testMessage(1, 1, 1)); err != nil {
				t.Fatal(err)
			}
			factory := func(AccountConfig, *telegram.UpdateState, func(context.Context, tg.UpdatesClass) error, func(int64)) (Account, error) {
				return &readyAccount{}, nil
			}
			filter := func(context.Context, string, *tg.Message, tg.UpdatesClass) (Target, bool, error) {
				return Target{}, true, nil
			}
			var calls, failed atomic.Int32
			done := make(chan struct{}, 4)
			sink := func(_ context.Context, w *Work, _ *tg.Message, _ telegram.MediaDownloader) error {
				n := calls.Add(1)
				defer func() { done <- struct{}{} }()
				if n > 1 {
					return nil // the second claim (after a failed hold) downloads
				}
				id := w.ID
				return fmt.Errorf("ingest: %w", &telegram.DownloadDeferred{Reason: "copy",
					Hold: func(ctx context.Context, tx *sql.Tx) error {
						if _, err := tx.ExecContext(ctx, `INSERT INTO held(work_id) VALUES(?)`, id); err != nil {
							return err
						}
						if failHold {
							return errors.New("disk full")
						}
						return nil
					},
					Failed: func(error) { failed.Add(1) }})
			}
			c := New(s.writer, s.reader, t.TempDir(), factory, filter, sink, nil)
			if err := c.Start(ctx, ctx, []AccountConfig{{ID: "one"}}, 1, 1); err != nil {
				t.Fatal(err)
			}
			defer c.Stop(ctx)
			want := 1
			if failHold {
				want = 2
			}
			for i := 0; i < want; i++ {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatalf("no attempt %d", i+1)
				}
			}
			var status string
			var paused, attempts, held int
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if err := s.reader.QueryRow(`SELECT status,paused,attempts FROM tgdl_work`).Scan(&status, &paused, &attempts); err != nil {
					t.Fatal(err)
				}
				if status != "processing" {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			_ = s.reader.QueryRow(`SELECT COUNT(*) FROM held`).Scan(&held)
			if !failHold {
				if status != "pending" || paused != 1 || attempts != 0 || held != 1 || failed.Load() != 0 {
					t.Fatalf("deferred: status=%s paused=%d attempts=%d held=%d", status, paused, attempts, held)
				}
				// Paused: not claimed again.
				time.Sleep(300 * time.Millisecond)
				if calls.Load() != 1 {
					t.Fatalf("a held row was claimed again: %d", calls.Load())
				}
				return
			}
			if status != "completed" || held != 0 || failed.Load() != 1 || calls.Load() != 2 {
				t.Fatalf("hold failed: status=%s held=%d failed=%d calls=%d", status, held, failed.Load(), calls.Load())
			}
		})
	}
}

// A row that changed while it was checked (another claim generation) is not
// deferred: FinishDeferred changes nothing and says so.
func TestIx1213FinishDeferredLeavesAChangedRowAlone(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, _, err := s.Enqueue(ctx, "one", Target{}, testMessage(1, 1, 1)); err != nil {
		t.Fatal(err)
	}
	w, err := s.Claim(ctx, []string{"one"}, time.Now())
	if err != nil || w == nil {
		t.Fatalf("claim %v %v", w, err)
	}
	stale := *w
	stale.Generation++
	ran := false
	ok, err := s.FinishDeferred(ctx, &stale, func(context.Context, *sql.Tx) error { ran = true; return nil })
	if err != nil || ok || ran {
		t.Fatalf("stale claim deferred: ok=%v ran=%v %v", ok, ran, err)
	}
	// A restart while it is checked: the row is pending again, unpaused.
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	var paused int
	_ = s.reader.QueryRow(`SELECT status,paused FROM tgdl_work`).Scan(&status, &paused)
	if status != "pending" || paused != 0 {
		t.Fatalf("after a restart mid-check: %s paused=%d", status, paused)
	}
}
