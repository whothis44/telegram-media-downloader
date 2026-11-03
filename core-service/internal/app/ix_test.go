package app

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

func TestIxRowFromMessageKeepsEveryDetail(t *testing.T) {
	m := &tg.Message{ID: 7, Date: 1700000000, Message: "hello", PeerID: &tg.PeerChannel{ChannelID: 42}}
	m.SetFromID(&tg.PeerUser{UserID: 5})
	m.SetEditDate(1700000100)
	m.SetGroupedID(1361234567890123456)
	m.SetPostAuthor("Ed")
	m.SetViews(12)
	fwd := tg.MessageFwdHeader{Date: 1600000000}
	fwd.SetFromName("Other channel")
	m.SetFwdFrom(fwd)
	m.SetReplyTo(&tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 12, ReplyToTopID: 10})
	m.Media = &tg.MessageMediaDocument{Document: &tg.Document{ID: 99, Size: 300, DCID: 2, MimeType: "audio/ogg", Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeAudio{Voice: true}}}}
	row := ixRowFromMessage(m)
	if row.Text != "hello" || row.Date != 1700000000 || row.EditDate != 1700000100 || row.GroupedID != "1361234567890123456" || row.SenderID != "user:5" ||
		row.PostAuthor != "Ed" || row.Views != 12 || row.FwdFrom != "Other channel" || row.FwdDate != 1600000000 {
		t.Fatalf("row=%+v", row)
	}
	if row.TopicID != 10 || row.ReplyTo != 12 {
		t.Fatalf("topic/reply: %d %d", row.TopicID, row.ReplyTo)
	}
	if row.MediaType != "voice" || row.MediaSize != 300 || row.MediaKey != "document:99:300" {
		t.Fatalf("media: %+v", row)
	}
	if len(row.TL) == 0 {
		t.Fatal("full Telegram record not kept")
	}
	// A message in a topic that is not a reply: its "reply" is the topic.
	inTopic := &tg.Message{ID: 8, PeerID: &tg.PeerChannel{ChannelID: 42}}
	inTopic.SetReplyTo(&tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 10})
	row = ixRowFromMessage(inTopic)
	if row.TopicID != 10 || row.ReplyTo != 0 || row.MediaType != "" {
		t.Fatalf("topic message: %+v", row)
	}
	// An ordinary reply keeps its target.
	reply := &tg.Message{ID: 9, PeerID: &tg.PeerChannel{ChannelID: 42}}
	reply.SetReplyTo(&tg.MessageReplyHeader{ReplyToMsgID: 3})
	if row = ixRowFromMessage(reply); row.ReplyTo != 3 || row.TopicID != 0 {
		t.Fatalf("reply: %+v", row)
	}
}

func TestIxTopicNamesFromServiceMessages(t *testing.T) {
	created := &tg.MessageService{ID: 10, Date: 5, PeerID: &tg.PeerChannel{ChannelID: 42}, Action: &tg.MessageActionTopicCreate{Title: "Match clips"}}
	if got, ok := ixTopicFromService(created); !ok || got != (ixTopic{ID: 10, Title: "Match clips", Date: 5}) {
		t.Fatalf("create: %+v %v", got, ok)
	}
	edit := &tg.MessageActionTopicEdit{}
	edit.SetTitle("Clips")
	renamed := &tg.MessageService{ID: 50, Date: 9, PeerID: &tg.PeerChannel{ChannelID: 42}, Action: edit}
	renamed.SetReplyTo(&tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 10})
	if got, ok := ixTopicFromService(renamed); !ok || got != (ixTopic{ID: 10, Title: "Clips", Date: 9}) {
		t.Fatalf("edit: %+v %v", got, ok)
	}
	closed := &tg.MessageService{ID: 51, Action: &tg.MessageActionTopicEdit{Closed: true}}
	if _, ok := ixTopicFromService(closed); ok {
		t.Fatal("an edit without a title is not a rename")
	}
}

func ixTestMessage(id int, text string) *tg.Message {
	m := &tg.Message{ID: id, Date: 1700000000 + id, Message: text, PeerID: &tg.PeerChannel{ChannelID: 42}}
	m.SetFromID(&tg.PeerUser{UserID: 5})
	return m
}

func TestIxRecordingKeepsEditsDeletionsAndNames(t *testing.T) {
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureMonitor(t, a)
	group := "-1000000000042"
	page := telegram.HistoryPage{Messages: []tg.MessageClass{ixTestMessage(7, "first"), ixTestMessage(6, "text only"), &tg.MessageEmpty{ID: 5},
		&tg.MessageService{ID: 4, Date: 3, PeerID: &tg.PeerChannel{ChannelID: 42}, Action: &tg.MessageActionTopicCreate{Title: "News"}}},
		Entities: &tg.Updates{Users: []tg.UserClass{&tg.User{ID: 5, FirstName: "Ann", LastName: "Lee", Username: "ann"}}, Chats: []tg.ChatClass{&tg.Channel{ID: 42, Title: "Test"}}}}
	if err = a.ixRecordHistoryPage(ctx, group, page); err != nil {
		t.Fatal(err)
	}
	var n int
	a.db.Reader.QueryRow(`SELECT count(*) FROM ix_messages WHERE group_id=?`, group).Scan(&n)
	if n != 2 {
		t.Fatalf("messages=%d", n)
	}
	var name, username, title string
	a.db.Reader.QueryRow(`SELECT name, username FROM ix_peers WHERE peer_id='user:5'`).Scan(&name, &username)
	a.db.Reader.QueryRow(`SELECT title FROM ix_topics WHERE group_id=? AND topic_id=4`, group).Scan(&title)
	if name != "Ann Lee" || username != "ann" || title != "News" {
		t.Fatalf("names: %q %q %q", name, username, title)
	}
	// An edit keeps the earlier text.
	edited := ixTestMessage(7, "second")
	edited.SetEditDate(1800000000)
	if err = a.ixRecordHistoryPage(ctx, group, telegram.HistoryPage{Messages: []tg.MessageClass{edited}}); err != nil {
		t.Fatal(err)
	}
	// A delayed older copy must not undo it.
	if err = a.ixRecordHistoryPage(ctx, group, telegram.HistoryPage{Messages: []tg.MessageClass{ixTestMessage(7, "first")}}); err != nil {
		t.Fatal(err)
	}
	var text, old string
	a.db.Reader.QueryRow(`SELECT text FROM ix_messages WHERE group_id=? AND message_id=7`, group).Scan(&text)
	a.db.Reader.QueryRow(`SELECT count(*), MAX(text) FROM ix_message_edits WHERE group_id=? AND message_id=7`, group).Scan(&n, &old)
	if text != "second" || n != 1 || old != "first" {
		t.Fatalf("edit: text=%q edits=%d old=%q", text, n, old)
	}
	// Deleted on Telegram: flagged, kept; seen again: no longer deleted.
	a.ixRecordUpdate(ctx, "one", &tg.UpdateDeleteChannelMessages{ChannelID: 42, Messages: []int{6}}, &tg.Updates{})
	var deleted int64
	a.db.Reader.QueryRow(`SELECT deleted_at FROM ix_messages WHERE group_id=? AND message_id=6`, group).Scan(&deleted)
	if deleted == 0 {
		t.Fatal("deletion not recorded")
	}
	a.ixRecordUpdate(ctx, "one", &tg.UpdateEditChannelMessage{Message: ixTestMessage(6, "text only")}, &tg.Updates{})
	a.db.Reader.QueryRow(`SELECT deleted_at FROM ix_messages WHERE group_id=? AND message_id=6`, group).Scan(&deleted)
	if deleted != 0 {
		t.Fatal("a message seen again stays marked deleted")
	}
	// Live messages: only the downloader's groups are recorded.
	a.ixRecordUpdate(ctx, "one", &tg.UpdateNewChannelMessage{Message: ixTestMessage(8, "live")}, &tg.Updates{})
	other := ixTestMessage(9, "private")
	other.PeerID = &tg.PeerChannel{ChannelID: 77}
	a.ixRecordUpdate(ctx, "one", &tg.UpdateNewChannelMessage{Message: other}, &tg.Updates{})
	a.db.Reader.QueryRow(`SELECT count(*) FROM ix_messages`).Scan(&n)
	if n != 3 {
		t.Fatalf("live recording: %d messages", n)
	}
}

func TestIxQueueRescansWholeGroupAndRecordsEveryMessage(t *testing.T) {
	var transfers atomic.Int64
	var minDates atomic.Int64
	media := func(id int) *tg.Message {
		m := updateFixture().Updates[0].(*tg.UpdateNewChannelMessage).Message.(*tg.Message)
		m.ID = id
		m.Message = fmt.Sprintf("caption %d", id)
		return m
	}
	page := func(_ context.Context, r telegram.HistoryRequest) (telegram.HistoryPage, error) {
		minDates.Store(int64(r.MinDate))
		switch r.OffsetID {
		case 0:
			return telegram.HistoryPage{Messages: []tg.MessageClass{ixTestMessage(30, "plain words here"), media(20)}, NextOffset: 20, Entities: &tg.Updates{}}, nil
		case 20:
			return telegram.HistoryPage{Messages: []tg.MessageClass{media(10)}, NextOffset: 10, Entities: &tg.Updates{}}, nil
		case 10:
			return telegram.HistoryPage{Done: true}, nil
		}
		return telegram.HistoryPage{}, fmt.Errorf("unexpected offset %d", r.OffsetID)
	}
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir(), AccountFactory: historyFactory(t, page, nil, &transfers)})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureDisabledHistory(t, a)
	group := "-1000000000042"
	// A message archived earlier that no longer exists on Telegram.
	if err = a.ixRecordHistoryPage(ctx, group, telegram.HistoryPage{Messages: []tg.MessageClass{ixTestMessage(25, "gone since")}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	res := historyHTTP(t, a, "POST", "/api/ix/queue/add", `{"groups":[{"id":"-1000000000042","name":"Test"}],"ensureFilters":true,"start":true}`)
	if res["success"] != true {
		t.Fatalf("add: %v", res)
	}
	deadline := time.Now().Add(10 * time.Second)
	var st ixQueueState
	sawMessagesPass := false
	for time.Now().Before(deadline) {
		if err = a.ixQueueStep(ctx, a.ixDeps()); err != nil {
			t.Fatal(err)
		}
		if st, err = a.ixLoadQueue(ctx); err != nil {
			t.Fatal(err)
		}
		if len(st.Items) == 1 && st.Items[0].Phase == ixPhaseFiles && st.Items[0].State == "waiting" {
			// Between the passes: the messages pass queued no download.
			sawMessagesPass = true
			var queued int
			a.db.Reader.QueryRow(`SELECT count(*) FROM tgdl_work`).Scan(&queued)
			if queued != 0 {
				t.Fatalf("the messages pass queued %d download(s)", queued)
			}
		}
		if len(st.Items) == 1 && st.Items[0].State == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawMessagesPass {
		t.Fatal("the group did not get a separate messages pass")
	}
	if len(st.Items) != 1 || st.Items[0].State != "done" || st.Items[0].Phase != ixPhaseFiles || st.Items[0].Messages != 3 || st.Items[0].Missing != 2 || st.Items[0].Processed != 3 {
		t.Fatalf("queue=%+v", st)
	}
	if minDates.Load() != 1 {
		t.Fatalf("the rescan must walk the whole history (minDate=1), got %d", minDates.Load())
	}
	var n int
	a.db.Reader.QueryRow(`SELECT count(*) FROM ix_messages WHERE group_id=? AND deleted_at=0`, group).Scan(&n)
	if n != 3 {
		t.Fatalf("recorded=%d", n)
	}
	var deleted int64
	a.db.Reader.QueryRow(`SELECT deleted_at FROM ix_messages WHERE group_id=? AND message_id=25`, group).Scan(&deleted)
	if deleted == 0 {
		t.Fatal("a message missing from the full walk was not flagged as deleted")
	}
	a.db.Reader.QueryRow(`SELECT count(*) FROM ix_walks WHERE group_id=? AND state='done'`, group).Scan(&n)
	if n != 2 { // the messages pass and the files pass
		t.Fatalf("walks=%d", n)
	}
	waitHistoryDownloads(t, a, 2)
	cfg, _ := a.config.Load(ctx)
	filters := cfg["groups"].([]any)[0].(map[string]any)["filters"].(map[string]any)
	if filters["files"] != true || filters["photos"] != true || filters["videos"] != true {
		t.Fatalf("filters=%v", filters)
	}
	// The dashboard view and search see the archive.
	token, err := a.sessions.Create(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	rec := requestPurge(a, token, "GET", "/ix/view/", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Test") {
		t.Fatalf("view: %d %s", rec.Code, rec.Body.String())
	}
	rec = requestPurge(a, token, "GET", "/api/ix/search?q=plain", "")
	var found struct {
		Hits []ixHit `json:"hits"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &found); err != nil || len(found.Hits) != 1 || found.Hits[0].MessageID != 30 || !strings.HasPrefix(found.Hits[0].Link, "/ix/view/g_42/") {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	rec = requestPurge(a, token, "GET", "/ix", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Archive") {
		t.Fatalf("page: %d", rec.Code)
	}
	guest, err := a.sessions.Create(ctx, "guest")
	if err != nil {
		t.Fatal(err)
	}
	if rec = requestPurge(a, guest, "GET", "/ix/view/", ""); rec.Code == 200 {
		t.Fatal("a guest can read the message archive")
	}
}

func TestIxQueueRetriesFailuresAtTheBackAndStopsOnPermanentErrors(t *testing.T) {
	ixBackgroundQueue.Store(false)
	defer ixBackgroundQueue.Store(true)
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	now := time.Unix(1_800_000_000, 0)
	jobs := map[string]historyJob{}
	started := []string{}
	hold := ""
	deps := ixQueueDeps{
		start: func(_ context.Context, id string, messagesOnly bool) (string, error) {
			if !messagesOnly {
				t.Fatalf("%s: the first pass must record messages only", id)
			}
			started = append(started, id)
			switch id {
			case "gone":
				return "", ixPermanentError{"This chat can't be reached"}
			case "flaky":
				if len(started) == 1 {
					return "", errors.New("network down")
				}
			}
			jobs["job-"+id] = historyJob{ID: "job-" + id, State: "running", GroupID: id}
			return "job-" + id, nil
		},
		job:     func(_ context.Context, id string) (historyJob, bool) { j, ok := jobs[id]; return j, ok },
		missing: func(context.Context, string) (int, error) { return 0, nil },
		finish:  func(context.Context, ixQueueItem, historyJob) error { return nil },
		hold:    func(context.Context) string { return hold },
		now:     func() time.Time { return now },
	}
	if _, err = a.ixQueueEdit(ctx, func(st *ixQueueState) error {
		st.Running = true
		for _, id := range []string{"flaky", "gone", "ok"} {
			st.Items = append(st.Items, ixQueueItem{GroupID: id, Name: id, State: "waiting"})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	step := func() ixQueueState {
		t.Helper()
		if err := a.ixQueueStep(ctx, deps); err != nil {
			t.Fatal(err)
		}
		st, err := a.ixLoadQueue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	st := step() // flaky fails: to the back, retry in a minute
	if st.Items[2].GroupID != "flaky" || st.Items[2].State != "waiting" || st.Items[2].Attempts != 1 || st.Items[2].NextAt != now.Add(time.Minute).UnixMilli() {
		t.Fatalf("retry: %+v", st.Items)
	}
	st = step() // gone: permanent
	if st.Items[0].GroupID != "gone" || st.Items[0].State != "failed" {
		t.Fatalf("permanent: %+v", st.Items)
	}
	hold = "quiet hours"
	if st = step(); st.Notice != "quiet hours" || len(started) != 2 {
		t.Fatalf("hold: %+v %v", st, started)
	}
	hold = ""
	st = step() // ok starts
	if st.Items[1].GroupID != "ok" || st.Items[1].State != "running" {
		t.Fatalf("start: %+v", st.Items)
	}
	j := jobs["job-ok"]
	j.State, j.Processed = "done", 9
	jobs["job-ok"] = j
	st = step() // ok done (nothing missing); flaky's retry is not due yet
	if st.Items[1].State != "done" || st.Items[1].Messages != 9 || !strings.Contains(st.Notice, "retry") {
		t.Fatalf("done: %+v", st)
	}
	now = now.Add(2 * time.Minute)
	st = step() // flaky is due: no waiting for the download queue
	if st.Items[2].State != "running" {
		t.Fatalf("retry start: %+v", st.Items)
	}
	j = jobs["job-flaky"]
	msg := "rpc error code 420: FLOOD_WAIT_30"
	j.State, j.Error = "error", &msg
	jobs["job-flaky"] = j
	st = step()
	if st.Items[2].State != "waiting" || st.Items[2].Attempts != 2 || st.Items[2].NextAt != now.Add(2*time.Minute).UnixMilli() || st.Items[2].Error != msg {
		t.Fatalf("backoff: %+v", st.Items[2])
	}
	if ixBackoff(10) != 15*time.Minute {
		t.Fatal("backoff must cap at 15 minutes")
	}
}

func TestIxQueueSavesEveryGroupsMessagesBeforeFetchingFiles(t *testing.T) {
	ixBackgroundQueue.Store(false)
	defer ixBackgroundQueue.Store(true)
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	now := time.Unix(1_800_000_000, 0)
	jobs := map[string]historyJob{}
	type start struct {
		id           string
		messagesOnly bool
	}
	var started []start
	missing := map[string]int{"a": 5, "b": 0}
	deps := ixQueueDeps{
		start: func(_ context.Context, id string, messagesOnly bool) (string, error) {
			started = append(started, start{id, messagesOnly})
			jid := fmt.Sprintf("job-%s-%d", id, len(started))
			jobs[jid] = historyJob{ID: jid, State: "running", GroupID: id}
			return jid, nil
		},
		job:     func(_ context.Context, id string) (historyJob, bool) { j, ok := jobs[id]; return j, ok },
		missing: func(_ context.Context, id string) (int, error) { return missing[id], nil },
		finish:  func(context.Context, ixQueueItem, historyJob) error { return nil },
		now:     func() time.Time { return now },
	}
	// "a" was queued by an earlier version (no phase).
	if _, err = a.ixQueueEdit(ctx, func(st *ixQueueState) error {
		st.Running = true
		st.Items = append(st.Items, ixQueueItem{GroupID: "a", Name: "a", State: "waiting"}, ixQueueItem{GroupID: "b", Name: "b", State: "waiting", Phase: ixPhaseMessages})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	step := func() ixQueueState {
		t.Helper()
		if err := a.ixQueueStep(ctx, deps); err != nil {
			t.Fatal(err)
		}
		st, err := a.ixLoadQueue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	finish := func(processed int) {
		last := started[len(started)-1]
		jid := fmt.Sprintf("job-%s-%d", last.id, len(started))
		j := jobs[jid]
		j.State, j.Processed, j.Downloaded = "done", processed, 1
		jobs[jid] = j
	}
	step() // a: messages
	finish(40)
	st := step() // a's messages saved, 5 files missing: files pass later; b's messages next
	if st.Items[0].Phase != ixPhaseFiles || st.Items[0].State != "waiting" || st.Items[0].Messages != 40 || st.Items[0].Missing != 5 || st.Items[0].MessagesAt == 0 {
		t.Fatalf("after a's messages: %+v", st.Items[0])
	}
	st = step()
	if st.Items[1].State != "running" || st.Items[0].State != "waiting" {
		t.Fatalf("b's messages must come before a's files: %+v", st.Items)
	}
	finish(7)
	st = step() // b: nothing missing, done after one pass
	if st.Items[1].State != "done" || st.Items[1].Messages != 7 {
		t.Fatalf("b: %+v", st.Items[1])
	}
	st = step() // now a's files
	if st.Items[0].State != "running" {
		t.Fatalf("a's files: %+v", st.Items[0])
	}
	finish(40)
	st = step()
	if st.Items[0].State != "done" || st.Items[0].Missing != 5 || st.Items[0].Downloaded != 1 {
		t.Fatalf("a done: %+v", st.Items[0])
	}
	want := []start{{"a", true}, {"b", true}, {"a", false}}
	if fmt.Sprint(started) != fmt.Sprint(want) {
		t.Fatalf("starts=%v want %v", started, want)
	}
}

// ixSeedIndex builds three groups: two in "Sports" sharing a file (one of
// them a forum with topics) and one uncategorised group whose messages were
// never captured.
func ixSeedIndex(t *testing.T, a *App) {
	t.Helper()
	ctx := context.Background()
	cfg, err := a.config.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg["groups"] = []any{
		map[string]any{"id": "-1000000000101", "name": "sports-1", "enabled": true, "category": "Sports"},
		map[string]any{"id": "-1000000000102", "name": "football-2", "enabled": true, "category": "Sports"},
		map[string]any{"id": "-1000000000103", "name": "News", "enabled": true},
	}
	if err = a.config.Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("ab", 32)
	for _, f := range []struct {
		group string
		msg   int
		path  string
		hash  string
		kind  string
	}{
		{"-1000000000101", 1, "sports-1/images/shared.jpg", hash, "photo"},
		{"-1000000000101", 3, "sports-1/images/a.jpg", strings.Repeat("cd", 32), "photo"},
		{"-1000000000102", 11, "sports-1/images/shared.jpg", hash, "photo"},
		{"-1000000000103", 99, "News/documents/report #1.pdf", "", "document"},
	} {
		if _, err = a.db.Writer.Exec(`INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,status,file_hash) VALUES(?,?,?,?,?,?,?,'completed',?)`,
			f.group, "", f.msg, filepath.Base(f.path), 1000, f.kind, f.path, f.hash); err != nil {
			t.Fatal(err)
		}
	}
	day := 1700000000
	rows := []struct {
		group                        string
		id, date, topic, reply       int
		text, grouped, media, sender string
		deleted                      int64
	}{
		{"-1000000000101", 1, day, 0, 0, "match photo https://example.com/a.", "", "photo", "user:5", 0},
		{"-1000000000101", 2, day + 60, 0, 1, "gone text", "", "", "user:5", 1},
		{"-1000000000101", 3, day + 120, 0, 0, "album caption", "77", "photo", "", 0},
		{"-1000000000101", 4, day + 120, 0, 0, "", "77", "photo", "", 0},
		{"-1000000000102", 11, day + 30, 10, 0, "clip", "", "photo", "user:6", 0},
		{"-1000000000102", 12, day + 40, 10, 11, "great goal", "", "", "user:6", 0},
		{"-1000000000102", 13, day + 50, 0, 0, "hello general", "", "", "user:6", 0},
	}
	for _, r := range rows {
		if _, err = a.db.Writer.Exec(`INSERT INTO ix_messages(group_id,message_id,date,text,grouped_id,sender_id,reply_to,topic_id,media_type,deleted_at,seen_at) VALUES(?,?,?,?,?,?,?,?,?,?,1)`,
			r.group, r.id, r.date, r.text, r.grouped, r.sender, r.reply, r.topic, r.media, r.deleted); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO ix_topics(group_id,topic_id,title,title_date) VALUES('-1000000000102',10,'Match clips',1)`,
		`INSERT INTO ix_message_edits(group_id,message_id,edit_date,text,recorded_at) VALUES('-1000000000101',1,0,'old caption',1)`,
		`INSERT INTO ix_peers(peer_id,kind,name,username,updated_at) VALUES('user:5','user','Ann Lee','ann',1)`,
	} {
		if _, err = a.db.Writer.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIxIndexPagesCategoriesTopicsAndSharedFiles(t *testing.T) {
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ixSeedIndex(t, a)
	d, err := a.ixSite(ctx, ixSite{}, true)
	if err != nil {
		t.Fatal(err)
	}
	get := func(rel string) string {
		t.Helper()
		body, _, err := a.ixViewFile(ctx, d, rel)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		return string(body)
	}
	root := get("index.html")
	if !strings.Contains(root, "3 group(s) in 2 categories") || strings.Index(root, ">Sports<") > strings.Index(root, ">Uncategorised<") {
		t.Fatalf("root: %s", root)
	}
	sports := get("c_sports/index.html")
	if !strings.Contains(sports, "<b>2 unique files</b>") || !strings.Contains(sports, "1 posted more than once") || !strings.Contains(sports, "3 files in posts") {
		t.Fatalf("category: %s", sports)
	}
	a1 := d.byID["-1000000000101"]
	page := get("g_101/" + a1.Pages[0].File)
	for _, want := range []string{"album of 2", "deleted on Telegram", "Edited — 1 earlier version", "old caption", "Ann Lee (@ann)", `href="https://example.com/a"`, "also in", "football-2 › Match clips #11", "/api/thumbs/", "Not downloaded yet: photo (message #4)", `reply to <a href="` + a1.Pages[0].File + `#m1">#1</a>`} {
		if !strings.Contains(page, want) {
			t.Errorf("group page lacks %q", want)
		}
	}
	forum := get("g_102/index.html")
	if !strings.Contains(forum, "<h2>Topics</h2>") || !strings.Contains(forum, "<b>Match clips</b>") || !strings.Contains(forum, "<b>General</b>") {
		t.Fatalf("forum: %s", forum)
	}
	b := d.byID["-1000000000102"]
	var topicPage string
	for _, pg := range b.Pages {
		if pg.Topic == 10 {
			topicPage = get("g_102/" + pg.File)
		}
	}
	if !strings.Contains(topicPage, "Topic: <b>Match clips</b>") || !strings.Contains(topicPage, "reply to <a") || strings.Contains(topicPage, "hello general") {
		t.Fatalf("topic page: %s", topicPage)
	}
	news := get("g_103/index.html")
	if !strings.Contains(news, "No messages captured yet") {
		t.Fatalf("uncaptured group: %s", news)
	}
	csv := get("g_101/posts.csv")
	if !strings.HasPrefix(csv, "\xef\xbb\xbfmessage_id,album_id,topic_id") || !strings.Contains(csv, "gone text") {
		t.Fatalf("csv: %s", csv)
	}
	if sums := get("g_101/sha256.txt"); !strings.Contains(sums, strings.Repeat("ab", 32)+"  sports-1/images/shared.jpg") {
		t.Fatalf("sha256: %s", sums)
	}

	// One message recorded from Telegram, with its full object.
	full := ixTestMessage(5, "with raw <tags> & entities")
	full.SetGroupedID(77)
	if err = a.ixRecordHistoryPage(ctx, "-1000000000101", telegram.HistoryPage{Messages: []tg.MessageClass{full}}); err != nil {
		t.Fatal(err)
	}

	// The static copy: written next to the downloads folder, linking to it.
	if err = os.MkdirAll(a.downloadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = a.ixExport(ctx); err != nil {
		t.Fatal(err)
	}
	dir := a.downloadsDir + " index"
	for _, f := range []string{"index.html", "style.css", "c_sports/index.html", "g_101/index.html", "g_102/index.html", "g_103/posts.csv"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("static copy lacks %s: %v", f, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "g_101", a1.Pages[0].File))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `src="../../downloads/sports-1/images/shared.jpg"`) {
		t.Fatalf("static links: %s", data)
	}
	if _, err := os.Stat(dir + ".new"); !os.IsNotExist(err) {
		t.Fatal("temporary folder left behind")
	}
	// Writing again replaces the copy.
	if err = a.ixExport(ctx); err != nil {
		t.Fatal(err)
	}
	if st := a.ixExportStatus(); st.Running || st.Error != "" || st.Files < 10 {
		t.Fatalf("status: %+v", st)
	}

	// The data files for other tools.
	readLines := func(name string) []map[string]any {
		t.Helper()
		f, err := os.Open(filepath.Join(dir, "data", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var r io.Reader = f
		if strings.HasSuffix(name, ".gz") {
			zr, err := gzip.NewReader(f)
			if err != nil {
				t.Fatal(err)
			}
			r = zr
		}
		var out []map[string]any
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Fatalf("%s: %v: %s", name, err, sc.Text())
			}
			out = append(out, m)
		}
		return out
	}
	var manifest map[string]any
	raw, err := os.ReadFile(filepath.Join(dir, "data", "manifest.json"))
	if err != nil || json.Unmarshal(raw, &manifest) != nil || manifest["format_version"] != float64(1) || manifest["downloads_root"] == "" {
		t.Fatalf("manifest: %s %v", raw, err)
	}
	if readme, err := os.ReadFile(filepath.Join(dir, "data", "README.md")); err != nil || !strings.Contains(string(readme), "album_id") {
		t.Fatal("data README missing")
	}
	if gs := readLines("groups.jsonl"); len(gs) != 3 || gs[0]["messages_file"] == "" {
		t.Fatalf("groups: %v", gs)
	}
	if fs := readLines("files.jsonl"); len(fs) != 4 {
		t.Fatalf("files: %v", fs)
	}
	msgs := map[float64]map[string]any{}
	for _, m := range readLines("messages/g_101.jsonl.gz") {
		msgs[m["message_id"].(float64)] = m
	}
	if len(msgs) != 5 {
		t.Fatalf("g_101 messages: %v", msgs)
	}
	m3, m4, m2, m5 := msgs[3], msgs[4], msgs[2], msgs[5]
	if m3["album_id"] != "77" || m4["album_id"] != "77" || m3["file_status"] != "downloaded" || m4["file_status"] != "missing" {
		t.Fatalf("album/status: %v / %v", m3, m4)
	}
	if files := m3["files"].([]any); len(files) != 1 || files[0].(map[string]any)["sha256"] != strings.Repeat("cd", 32) || files[0].(map[string]any)["path"] != "sports-1/images/a.jpg" {
		t.Fatalf("files of 3: %v", m3["files"])
	}
	if m2["deleted_at"] == nil || m2["reply_to"] != float64(1) {
		t.Fatalf("deleted reply: %v", m2)
	}
	r5, _ := m5["raw"].(map[string]any)
	if r5 == nil || r5["_"] != "message" || r5["message"] != "with raw <tags> & entities" || r5["grouped_id"] != float64(77) || m5["tl_base64"] == "" {
		t.Fatalf("raw message: %v", m5)
	}
	if peer, _ := r5["peer_id"].(map[string]any); peer == nil || peer["_"] != "peerChannel" || peer["channel_id"] != float64(42) {
		t.Fatalf("raw peer: %v", r5["peer_id"])
	}
	if m5["text"] != "with raw <tags> & entities" || m5["date_utc"] == "" {
		t.Fatalf("message 5: %v", m5)
	}
	news := readLines("messages/g_103.jsonl.gz")
	if len(news) != 1 || news[0]["message_id"] != float64(99) || news[0]["file_status"] != "downloaded" {
		t.Fatalf("file without a captured message: %v", news)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "state.json")); err != nil {
		t.Fatal("state.json missing")
	}
}

func TestIxSmallRules(t *testing.T) {
	if got := ixFTSQuery(`bank "wire transfer" x"y`); got != `"bank"* "wire transfer" "xy"*` {
		t.Fatalf("fts: %s", got)
	}
	s := ixSettings{QuietStart: "22:00", QuietEnd: "07:00"}
	for clock, want := range map[string]bool{"23:30": true, "03:00": true, "07:00": false, "12:00": false} {
		at, _ := time.Parse("15:04", clock)
		if ixInQuietHours(s, at) != want {
			t.Errorf("quiet %s", clock)
		}
	}
	st := ixClassify([]string{"rpc error code 420: FROZEN_METHOD_INVALID"}, 1)
	if !st.Frozen || st.Deactivated || st.LastError != "FROZEN_METHOD_INVALID" {
		t.Fatalf("classify: %+v", st)
	}
	if st = ixClassify([]string{"AUTH_KEY_UNREGISTERED"}, 1); !st.Deactivated {
		t.Fatal("deactivated")
	}
	if got := ixSuggestCategory("sports-3"); got != "sports" {
		t.Fatalf("suggest: %q", got)
	}
	for in, want := range map[string]string{"PeerID": "peer_id", "TTLPeriod": "ttl_period", "GroupedID": "grouped_id", "FwdFrom": "fwd_from", "Message": "message", "ReplyToMsgID": "reply_to_msg_id"} {
		if got := ixSnake(in); got != want {
			t.Errorf("snake %s: %s", in, got)
		}
	}
	if !ixNatLess("sports-2", "Sports-10") || ixNatLess("b", "a") {
		t.Fatal("natural order")
	}
	if got := ixFilesBase(filepath.Join("x", "Telegram index"), filepath.Join("x", "Telegram")); got != "../Telegram" {
		t.Fatalf("files base: %s", got)
	}
	if ixBare("-1001234567890") != "1234567890" || ixBare("-1000000000042") != "42" || ixBare("-55") != "55" || ixDirOf("-1001234567890") != "g_1234567890" {
		t.Fatal("ids")
	}
}

func TestIxMonitorRecordsLiveMessagesWithoutMedia(t *testing.T) {
	made := make(chan *fixtureAccount, 2)
	factory := func(_ engine.AccountConfig, _ *telegram.UpdateState, handler func(context.Context, tg.UpdatesClass) error, _ func(int64)) (engine.Account, error) {
		account := &fixtureAccount{handle: handler, download: func(_ context.Context, _ telegram.Attachment, w io.Writer) error {
			_, err := w.Write([]byte("live"))
			return err
		}}
		made <- account
		return account, nil
	}
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir(), AccountFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureMonitor(t, a)
	if rec := monitorRequest(t, a, "/api/monitor/start"); rec.Code != 200 {
		t.Fatalf("start=%d %s", rec.Code, rec.Body.String())
	}
	account := <-made
	text := ixTestMessage(50, "breaking news, no media")
	if err = account.handle(ctx, &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: text}}, Users: []tg.UserClass{&tg.User{ID: 5, FirstName: "Ann"}}}); err != nil {
		t.Fatal(err)
	}
	if err = account.handle(ctx, updateFixture()); err != nil {
		t.Fatal(err)
	}
	waitCompleted(t, a)
	var got, name string
	if err = a.db.Reader.QueryRow(`SELECT text FROM ix_messages WHERE group_id='-1000000000042' AND message_id=50`).Scan(&got); err != nil || got != "breaking news, no media" {
		t.Fatalf("live text message not recorded: %q %v", got, err)
	}
	a.db.Reader.QueryRow(`SELECT name FROM ix_peers WHERE peer_id='user:5'`).Scan(&name)
	var media int
	a.db.Reader.QueryRow(`SELECT count(*) FROM ix_messages WHERE group_id='-1000000000042' AND message_id=10 AND media_type<>''`).Scan(&media)
	if name != "Ann" || media != 1 {
		t.Fatalf("name=%q media=%d", name, media)
	}
	if err = account.handle(ctx, &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateDeleteChannelMessages{ChannelID: 42, Messages: []int{50}}}}); err != nil {
		t.Fatal(err)
	}
	var deleted int64
	a.db.Reader.QueryRow(`SELECT deleted_at FROM ix_messages WHERE group_id='-1000000000042' AND message_id=50`).Scan(&deleted)
	if deleted == 0 {
		t.Fatal("live deletion not recorded")
	}
}

func TestIxDownloadStatsExplainFailures(t *testing.T) {
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	now := time.Now().UnixMilli()
	for i, row := range []struct {
		status, err string
		updated     int64
	}{
		{"pending", "", now}, {"pending", "", now}, {"processing", "", now},
		{"completed", "", now - 60_000}, {"completed", "", now - 3*60*60*1000},
		{"failed", "FILE_REFERENCE_EXPIRED", now}, {"failed", "FILE_REFERENCE_EXPIRED", now}, {"failed", "disk full", now},
	} {
		var errText any
		if row.err != "" {
			errText = row.err
		}
		if _, err = a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,error,created_at,updated_at)
			VALUES('acc','-1000000000042','g',?,1,'id','photo','f.jpg',1,X'01',?,?,?,?)`, i+1, row.status, errText, row.updated, row.updated); err != nil {
			t.Fatal(err)
		}
	}
	// The "disk full" file has used up its automatic retries.
	if _, err = a.db.Writer.Exec(`INSERT INTO ix_retry(work_key,rounds,last_at) VALUES('-1000000000042_8',?,1)`, ixRetryRounds); err != nil {
		t.Fatal(err)
	}
	d := a.ixDownloads(ctx)
	if d.Waiting != 2 || d.Downloading != 1 || d.Failed != 3 || d.LastHour != 1 || d.GivenUp != 1 || d.LastAt != now-60_000 {
		t.Fatalf("stats=%+v", d)
	}
	if len(d.Errors) != 2 || d.Errors[0].Error != "FILE_REFERENCE_EXPIRED" || d.Errors[0].Count != 2 {
		t.Fatalf("errors=%+v", d.Errors)
	}
	if !a.ixLoadSettings(ctx).AutoRetry {
		t.Fatal("automatic retry must be on by default")
	}
}
