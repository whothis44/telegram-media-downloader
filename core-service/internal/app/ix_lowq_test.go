package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// lowqVideo is an invented 1000-byte 1080p video with three alternatives:
// 720p at 45% (saves too little), 480p at 30%, 360p at 15% (too small).
func lowqVideo(msgID int) *tg.Message {
	doc := func(id, size int64, w, h int) *tg.Document {
		return &tg.Document{ID: id, AccessHash: 1, FileReference: []byte{1}, DCID: 2, Size: size, MimeType: "video/mp4",
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{W: w, H: h, Duration: 60}}}
	}
	orig := doc(1, 1000, 1920, 1080)
	orig.Attributes = append(orig.Attributes, &tg.DocumentAttributeFilename{FileName: "clip.mp4"})
	return &tg.Message{ID: msgID, Date: 1700000000, PeerID: &tg.PeerChannel{ChannelID: 123},
		Media: &tg.MessageMediaDocument{Document: orig, AltDocuments: []tg.DocumentClass{doc(2, 450, 1280, 720), doc(3, 300, 854, 480), doc(4, 150, 640, 360)}}}
}

const lowqGroup = "-1000000000123"

// lowqWork queues msg as a claimed backfill item, as the engine would.
func lowqWork(t *testing.T, a *App, msg *tg.Message, size int64) *engine.Work {
	t.Helper()
	var buf bin.Buffer
	if err := msg.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	res, err := a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at,generation,claim_generation)
		VALUES('acc',?,'G',?,1,'x','video','clip.mp4',?,?,'processing','history',?,?,1,1)`, lowqGroup, msg.ID, size, buf.Buf, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return &engine.Work{ID: id, MessageID: int64(msg.ID), Generation: 1, AccountID: "acc", GroupID: lowqGroup, GroupName: "G", MediaType: "video", FileName: "clip.mp4", FileSize: size, Origin: "history"}
}

// lowqTransport sends invented bytes, different for each Telegram file,
// and notes which files were asked for.
func lowqTransport() (telegram.MediaDownloader, func() []string) {
	var mu sync.Mutex
	var got []string
	return mediaTransport(func(_ context.Context, m telegram.Attachment, w io.Writer) error {
			mu.Lock()
			got = append(got, m.Identity.ID)
			mu.Unlock()
			b := make([]byte, m.Identity.Size)
			for i := range b {
				b[i] = m.Identity.ID[0] + byte(i%7)
			}
			_, err := w.Write(b)
			return err
		}), func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), got...)
		}
}

// lowqApp is an app with a deletion date an hour away and a speed of 100
// bytes a day, so no large video is reached in time.
func lowqApp(t *testing.T, settings map[string]any, fast bool) (*App, context.Context) {
	t.Helper()
	old := ixLowqMinSize
	ixLowqMinSize = 500
	t.Cleanup(func() { ixLowqMinSize = old })
	a, ctx := ix8App(t, settings)
	now := time.Now()
	a.ixSaveFreeze(ctx, ixFreezeInfo{FreezeState: ixTestFreeze(now.Add(time.Hour).Unix())})
	done := int64(100)
	if fast {
		done = 1 << 40
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at)
		VALUES('acc','-1009','G',900,1,'y','photo','p.jpg',?,X'','completed','history',?,?)`, done, now.Add(-time.Hour).UnixMilli(), now.Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	return a, ctx
}

func lowqRow(t *testing.T, a *App, msg int) (state string, cost int64, path string) {
	t.Helper()
	if err := a.db.Reader.QueryRow(`SELECT state, alt_cost, alt_path FROM ix_lowq WHERE group_id=? AND message_id=?`, lowqGroup, msg).Scan(&state, &cost, &path); err != nil {
		t.Fatalf("lower-quality row %d: %v", msg, err)
	}
	return
}

func lowqCatalogued(t *testing.T, a *App, msg int) (id, name string) {
	t.Helper()
	if err := a.db.Reader.QueryRow(`SELECT telegram_media_id, file_name FROM downloads WHERE group_id=? AND message_id=?`, lowqGroup, msg).Scan(&id, &name); err != nil {
		t.Fatal(err)
	}
	return
}

func TestIxLowerQualityFirstThenTheOriginalReplacesIt(t *testing.T) {
	a, ctx := lowqApp(t, map[string]any{"deadlineOrder": "auto"}, false)
	store := engine.NewWorkStore(a.db.Writer, a.db.Reader)
	transport, got := lowqTransport()
	msg := lowqVideo(7)
	work := lowqWork(t, a, msg, 1000)
	if err := a.ixIngestWork(ctx, work, msg, transport); err != nil {
		t.Fatal(err)
	}
	if g := got(); len(g) != 1 || g[0] != "3" {
		t.Fatalf("downloaded %v, want the 480p alternative only", g)
	}
	if id, name := lowqCatalogued(t, a, 7); id != "3" || !strings.Contains(name, "lower quality 480p") {
		t.Fatalf("the message's file: %s %q", id, name)
	}
	state, cost, lowPath := lowqRow(t, a, 7)
	if state != "saved" || cost != 300 || lowPath == "" {
		t.Fatalf("row: %s %d %q", state, cost, lowPath)
	}
	// The engine finishing the claim leaves the original queued and paused.
	if err := store.Finish(ctx, work, nil, false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	var status string
	var paused, body int
	_ = a.db.Reader.QueryRow(`SELECT status, paused, length(body) FROM tgdl_work WHERE id=?`, work.ID).Scan(&status, &paused, &body)
	if status != "pending" || paused != 1 || body == 0 {
		t.Fatalf("original not kept queued: %s paused %d body %d", status, paused, body)
	}
	// Another message with the same video: the saved quality is used, nothing is downloaded.
	msg8 := lowqVideo(8)
	work8 := lowqWork(t, a, msg8, 1000)
	if err := a.ixIngestWork(ctx, work8, msg8, transport); err != nil {
		t.Fatal(err)
	}
	_ = store.Finish(ctx, work8, nil, false, time.Time{})
	if g := got(); len(g) != 1 {
		t.Fatalf("a saved quality was downloaded again: %v", g)
	}
	if _, cost8, _ := lowqRow(t, a, 8); cost8 != 0 {
		t.Fatalf("a reused quality counted %d bytes", cost8)
	}
	st := a.ixLowqStatus(ctx)
	if st.Saved != 2 || st.SavedBytes != 300 || st.OrigBytes != 2000 || st.Waiting != 2 || st.Upgraded != 0 {
		t.Fatalf("status: %+v", st)
	}
	// Something else waits: the originals stay back.
	_, _ = a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at)
		VALUES('acc','-1009','G',901,1,'z','photo','q.jpg',10,X'00','pending','history',0,0)`)
	if err := a.ixLowqStep(ctx); err != nil {
		t.Fatal(err)
	}
	_ = a.db.Reader.QueryRow(`SELECT paused FROM tgdl_work WHERE id=?`, work.ID).Scan(&paused)
	if paused != 1 {
		t.Fatal("the original went before other files")
	}
	// A held copy still to be released also goes first.
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='completed' WHERE message_id=901`)
	_, _ = a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,paused,origin,created_at,updated_at)
		VALUES('acc','-1009','G',902,1,'h','video','h.mp4',10,X'00','pending',1,'history',0,0)`)
	_, _ = a.db.Writer.Exec(`INSERT INTO ix_held(work_id,group_id,message_id,like_path,like_key,reason,held_at,released_at) SELECT id,group_id,message_id,'','','looks like a saved copy',1,0 FROM tgdl_work WHERE message_id=902`)
	_ = a.ixLowqStep(ctx)
	_ = a.db.Reader.QueryRow(`SELECT paused FROM tgdl_work WHERE id=?`, work.ID).Scan(&paused)
	if paused != 1 {
		t.Fatal("the original went before a held copy")
	}
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='completed' WHERE message_id=902`)
	if err := a.ixLowqStep(ctx); err != nil {
		t.Fatal(err)
	}
	_ = a.db.Reader.QueryRow(`SELECT paused FROM tgdl_work WHERE id=?`, work.ID).Scan(&paused)
	if state, _, _ := lowqRow(t, a, 7); paused != 0 || state != "released" {
		t.Fatalf("not released once everything else was done: paused %d state %s", paused, state)
	}
	// The upgrade: the original, which replaces the lower quality.
	for _, w := range []*engine.Work{work, work8} {
		_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='processing', claim_generation=generation WHERE id=?`, w.ID)
		w.Generation = 2
		if err := a.ixIngestWork(ctx, w, lowqVideo(int(w.MessageID)), transport); err != nil {
			t.Fatal(err)
		}
	}
	if g := got(); len(g) != 2 || g[1] != "1" {
		t.Fatalf("downloads: %v, want the original once", g)
	}
	for _, m := range []int{7, 8} {
		if id, name := lowqCatalogued(t, a, m); id != "1" || name != "clip.mp4" {
			t.Fatalf("message %d file: %s %q", m, id, name)
		}
		if state, _, _ := lowqRow(t, a, m); state != "upgraded" {
			t.Fatalf("message %d state %s", m, state)
		}
	}
	if _, err := os.Stat(filepath.Join(a.downloadsDir, filepath.FromSlash(lowPath))); !os.IsNotExist(err) {
		t.Fatal("the lower quality was kept after the upgrade")
	}
	st = a.ixLowqStatus(ctx)
	if st.Upgraded != 2 || st.ExtraBytes != 300 || st.Waiting != 0 {
		t.Fatalf("status after upgrade: %+v", st)
	}
	if rep := a.ixDiagnostics(ctx); !strings.Contains(rep, "2 big videos saved in a lower quality first") || !strings.Contains(rep, "2 upgraded") {
		t.Fatalf("diagnostics: %s", rep)
	}
}

func TestIxLowerQualityKeptWhenAsked(t *testing.T) {
	a, ctx := lowqApp(t, map[string]any{"deadlineOrder": "auto", "lowqKeep": true}, false)
	transport, _ := lowqTransport()
	msg := lowqVideo(7)
	work := lowqWork(t, a, msg, 1000)
	if err := a.ixIngestWork(ctx, work, msg, transport); err != nil {
		t.Fatal(err)
	}
	_ = engine.NewWorkStore(a.db.Writer, a.db.Reader).Finish(ctx, work, nil, false, time.Time{})
	_, _, lowPath := lowqRow(t, a, 7)
	_ = a.ixLowqStep(ctx)
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='processing', claim_generation=generation WHERE id=?`, work.ID)
	work.Generation = 2
	if err := a.ixIngestWork(ctx, work, msg, transport); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := lowqRow(t, a, 7); state != "kept" {
		t.Fatalf("state %s", state)
	}
	if _, err := os.Stat(filepath.Join(a.downloadsDir, filepath.FromSlash(lowPath))); err != nil {
		t.Fatal("the lower quality was removed although it should be kept")
	}
	// Later cleanups leave it alone too.
	_ = a.drainFileCleanup(ctx)
	if _, err := os.Stat(filepath.Join(a.downloadsDir, filepath.FromSlash(lowPath))); err != nil {
		t.Fatal("a later cleanup removed the kept lower quality")
	}
}

func TestIxOriginalOnlyWhenTimeAllowsOrForCopies(t *testing.T) {
	cases := []struct {
		name     string
		fast     bool
		settings map[string]any
		prepare  func(a *App, w *engine.Work, msg *tg.Message)
	}{
		{name: "the queue finishes in time", fast: true},
		{name: "switched off", settings: map[string]any{"lowqFirst": false}},
		{name: "deadline order off", settings: map[string]any{"deadlineOrder": "off"}},
		{name: "a probable copy held back", prepare: func(a *App, w *engine.Work, _ *tg.Message) {
			_, _ = a.db.Writer.Exec(`INSERT INTO ix_held(work_id,group_id,message_id,like_path,like_key,reason,held_at,released_at) VALUES(?,?,?,'','','looks like a saved copy',1,1)`, w.ID, w.GroupID, w.MessageID)
		}},
		{name: "a confirmed copy", prepare: func(a *App, w *engine.Work, _ *tg.Message) {
			_, _ = a.db.Writer.Exec(`INSERT INTO ix_subst(group_id,message_id,identity,size,of_path,how,at) VALUES(?,?,'x',1000,'p','telegram-hashes',1)`, w.GroupID, w.MessageID)
		}},
		{name: "only a 45% alternative", prepare: func(_ *App, _ *engine.Work, msg *tg.Message) {
			md := msg.Media.(*tg.MessageMediaDocument)
			md.AltDocuments = md.AltDocuments[:1]
		}},
		{name: "self-destructing", prepare: func(_ *App, _ *engine.Work, msg *tg.Message) {
			msg.Media.(*tg.MessageMediaDocument).SetTTLSeconds(30)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			settings := map[string]any{"deadlineOrder": "auto"}
			for k, v := range c.settings {
				settings[k] = v
			}
			a, ctx := lowqApp(t, settings, c.fast)
			transport, got := lowqTransport()
			msg := lowqVideo(7)
			work := lowqWork(t, a, msg, 1000)
			if c.prepare != nil {
				c.prepare(a, work, msg)
			}
			if err := a.ixIngestWork(ctx, work, msg, transport); err != nil {
				t.Fatal(err)
			}
			if g := got(); len(g) != 1 || g[0] != "1" {
				t.Fatalf("downloaded %v, want the original", g)
			}
			var n int
			_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_lowq`).Scan(&n)
			if n != 0 {
				t.Fatal("recorded a lower quality")
			}
		})
	}
}

func TestIxStagingSettingIsCheckedAndOffByDefault(t *testing.T) {
	a, ctx := ix8App(t, nil)
	s := a.ixLoadSettings(ctx)
	if s.StageDir != "" || s.StageCapGB != 20 || !s.LowqFirst || s.LowqKeep {
		t.Fatalf("defaults: %+v", s)
	}
	if err := a.ixCheckStageDir("relative/folder"); err == nil {
		t.Fatal("a relative folder was accepted")
	}
	if err := a.ixCheckStageDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing folder was accepted")
	}
	inside := filepath.Join(a.downloadsDir, "stage")
	_ = os.MkdirAll(inside, 0700)
	if err := a.ixCheckStageDir(inside); err == nil {
		t.Fatal("a folder inside the downloads folder was accepted")
	}
	good := t.TempDir()
	if err := a.ixCheckStageDir(good); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(good); len(entries) != 0 {
		t.Fatal("the write check left a file")
	}
	if _, err := ixParseSettings(map[string]any{"stageCapGb": "lots"}, s); err == nil {
		t.Fatal("a bad size was accepted")
	}
	p, err := ixParseSettings(map[string]any{"stageDir": " " + good + " ", "stageCapGb": "50"}, s)
	if err != nil || p.StageDir != good || p.StageCapGB != 50 {
		t.Fatalf("parsed %+v %v", p, err)
	}
}

func TestIxStagedDownloadArrivesThroughTheApp(t *testing.T) {
	stage := t.TempDir()
	a, ctx := ix8App(t, map[string]any{"stageDir": stage})
	transport, _ := lowqTransport()
	msg := &tg.Message{ID: 10, PeerID: &tg.PeerChannel{ChannelID: 123}, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 77, Size: 64, DCID: 2, AccessHash: 1, FileReference: []byte{1},
		MimeType: "video/mp4", Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "a.mp4"}, &tg.DocumentAttributeVideo{W: 10, H: 10}}}}}
	rec, err := a.IngestTelegram(ctx, msg, "G", transport)
	if err != nil || !rec.Staged {
		t.Fatalf("not staged: %+v %v", rec, err)
	}
	// Waiting to be copied, unless the app's own mover was quicker.
	if st := a.ixStagingStatus(ctx); !st.On || st.Waiting != 1 {
		var moved int
		_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM downloads WHERE file_path=?`, rec.Path).Scan(&moved)
		if !st.On || st.Waiting != 0 || moved != 1 {
			t.Fatalf("status: %+v", st)
		}
	}
	// The app's own mover may have looked first, while the download still
	// held its locks, and put the move off for a while (errStageLater): bring
	// it forward and move it here.
	var n int
	for try := 0; try < 3 && n != 1; try++ {
		_, _ = a.db.Writer.Exec(`UPDATE tgdl_stage_moves SET next_at=0 WHERE state='waiting'`)
		a.library.MoveStaged(ctx)
		_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM downloads WHERE file_path=?`, rec.Path).Scan(&n)
	}
	if n != 1 {
		t.Fatal("not catalogued after the move")
	}
	if _, err := os.Stat(filepath.Join(a.downloadsDir, filepath.FromSlash(rec.Path))); err != nil {
		t.Fatal(err)
	}
	if rep := a.ixDiagnostics(ctx); !strings.Contains(rep, "local staging on") {
		t.Fatalf("diagnostics: %s", rep)
	}
}
