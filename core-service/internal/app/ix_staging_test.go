package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/download"
	"github.com/gotd/td/tg"
)

// A staged file removed from the staging folder (an antivirus, someone
// cleaning up) after its queue item completed: the item is queued again
// with its message, so the file is downloaded again.
func TestIxLostStagedFileIsQueuedAgain(t *testing.T) {
	stage := t.TempDir()
	a, ctx := ix8App(t, map[string]any{"stageDir": stage})
	transport, got := lowqTransport()
	msg := &tg.Message{ID: 10, Date: 1700000000, PeerID: &tg.PeerChannel{ChannelID: 123}, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 77, Size: 64, DCID: 2, AccessHash: 1, FileReference: []byte{1},
		MimeType: "video/mp4", Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "a.mp4"}, &tg.DocumentAttributeVideo{W: 10, H: 10}}}}}
	work := lowqWork(t, a, msg, 64)
	// The downloads folder cannot be written (the NAS is down), so the file
	// waits in the staging folder whatever the background mover does.
	if err := os.WriteFile(filepath.Join(a.downloadsDir, "G"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.ixIngestWork(ixWithRequeue(ctx, work, msg), work, msg, transport); err != nil {
		t.Fatal(err)
	}
	// The engine finishes the item: done, its message dropped.
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET status='completed', body=X'', claim_generation=NULL WHERE id=?`, work.ID); err != nil {
		t.Fatal(err)
	}
	var path, item string
	if err := a.db.Reader.QueryRow(`SELECT path, item FROM tgdl_stage_moves`).Scan(&path, &item); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(item, `"Requeue"`) {
		t.Fatal("the staged download keeps nothing to queue its message again")
	}
	// The background mover may hold the file open for a moment while it
	// tries (and fails) to copy it; Windows refuses to remove an open file.
	staged := filepath.Join(stage, download.StageFolder, filepath.FromSlash(path))
	for deadline := time.Now().Add(2 * time.Second); ; {
		err := os.Remove(staged)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_stage_moves SET next_at=0`); err != nil {
		t.Fatal(err)
	}
	a.library.MoveStaged(ctx)
	var status string
	var body int
	var generation int64
	if err := a.db.Reader.QueryRow(`SELECT status, length(body), generation FROM tgdl_work WHERE id=?`, work.ID).Scan(&status, &body, &generation); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || body == 0 || generation <= work.Generation {
		t.Fatalf("queue item: %s body %d generation %d", status, body, generation)
	}
	if st := a.ixStagingStatus(ctx); st.Waiting != 0 || st.Stuck != 0 {
		t.Fatalf("status: %+v", st)
	}
	if len(got()) != 1 {
		t.Fatalf("downloads: %v", got())
	}
}

func TestIxStagingAlerts(t *testing.T) {
	now := time.Now()
	st := ixStagingStatus{CapGB: 20}
	if al := ixStagingAlerts(st, now); len(al) != 0 {
		t.Fatalf("alerts with nothing wrong: %+v", al)
	}
	st.Full, st.FullSince, st.Waiting, st.Bytes = true, now.Add(-2*time.Minute).UnixMilli(), 3, 21<<30
	if al := ixStagingAlerts(st, now); len(al) != 0 {
		t.Fatalf("a short wait raised an alert: %+v", al)
	}
	st.FullSince = now.Add(-ixStageFullAfter - time.Minute).UnixMilli()
	st.Stuck, st.StuckBytes = 2, 5<<30
	st.Failing, st.FailingSince = 3, now.Add(-2*time.Hour).UnixMilli()
	keys := map[string]bool{}
	for _, al := range ixStagingAlerts(st, now) {
		keys[al.Key] = true
	}
	if !keys["staging:full"] || !keys["staging:stuck"] || !keys["staging:failing"] || len(keys) != 3 {
		t.Fatalf("alerts: %v", keys)
	}
}

// A group purged while its download waits in the staging folder: the staged
// file and its journal entry go with it, and the mover neither catalogues
// it nor downloads it again.
func TestIxPurgeRemovesTheGroupsStagedFiles(t *testing.T) {
	stage := t.TempDir()
	a, ctx := ix8App(t, map[string]any{"stageDir": stage})
	transport, got := lowqTransport()
	msg := &tg.Message{ID: 11, Date: 1700000000, PeerID: &tg.PeerChannel{ChannelID: 123}, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 78, Size: 64, DCID: 2, AccessHash: 1, FileReference: []byte{1},
		MimeType: "video/mp4", Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "b.mp4"}, &tg.DocumentAttributeVideo{W: 10, H: 10}}}}}
	work := lowqWork(t, a, msg, 64)
	if err := os.WriteFile(filepath.Join(a.downloadsDir, "G"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.ixIngestWork(ixWithRequeue(ctx, work, msg), work, msg, transport); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := a.db.Reader.QueryRow(`SELECT path FROM tgdl_stage_moves`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(stage, download.StageFolder, filepath.FromSlash(path))
	if _, err := os.Stat(local); err != nil {
		t.Fatal(err)
	}
	token, err := a.sessions.Create(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if w := requestPurge(a, token, "POST", "/api/groups/"+lowqGroup+"/delete-files", "{}"); w.Code != 200 {
		t.Fatalf("purge: %d %s", w.Code, w.Body.String())
	}
	a.purgeWG.Wait()
	assertPurgeDone(t, a, "group:"+lowqGroup)
	for _, table := range []string{"tgdl_stage_moves", "tgdl_stage_followers", "tgdl_work", "tgdl_message_generations"} {
		var n int
		if err := a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s after the purge: %d %v", table, n, err)
		}
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("staged file kept after the purge: %v", err)
	}
	if err := os.Remove(filepath.Join(a.downloadsDir, "G")); err != nil {
		t.Fatal(err)
	}
	a.library.MoveStaged(ctx)
	var n int
	if err := a.db.Reader.QueryRow(`SELECT (SELECT COUNT(*) FROM downloads)+(SELECT COUNT(*) FROM tgdl_work)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("purged download came back: %d %v", n, err)
	}
	if len(got()) != 1 {
		t.Fatalf("downloads from Telegram: %v", got())
	}
}

// The mover found a staged file missing after its group was purged (its
// journal entry still read): the message is not queued again.
func TestIxLostStagedFileOfAPurgedGroupIsNotQueuedAgain(t *testing.T) {
	stage := t.TempDir()
	a, ctx := ix8App(t, map[string]any{"stageDir": stage})
	transport, _ := lowqTransport()
	msg := &tg.Message{ID: 12, Date: 1700000000, PeerID: &tg.PeerChannel{ChannelID: 123}, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 79, Size: 64, DCID: 2, AccessHash: 1, FileReference: []byte{1},
		MimeType: "video/mp4", Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "c.mp4"}, &tg.DocumentAttributeVideo{W: 10, H: 10}}}}}
	work := lowqWork(t, a, msg, 64)
	if err := os.WriteFile(filepath.Join(a.downloadsDir, "G"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.ixIngestWork(ixWithRequeue(ctx, work, msg), work, msg, transport); err != nil {
		t.Fatal(err)
	}
	var item string
	if err := a.db.Reader.QueryRow(`SELECT item FROM tgdl_stage_moves`).Scan(&item); err != nil {
		t.Fatal(err)
	}
	var it download.Item
	if err := json.Unmarshal([]byte(item), &it); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`DELETE FROM tgdl_work; DELETE FROM tgdl_message_generations`); err != nil {
		t.Fatal(err)
	}
	if err := a.ixStagedLost(ctx, it); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := a.db.Reader.QueryRow(`SELECT COUNT(*) FROM tgdl_work`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a purged message was queued again: %d %v", n, err)
	}
}
