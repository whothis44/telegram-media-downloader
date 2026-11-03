package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// TestIxDataSample builds an invented archive (three chats, nobody real)
// and writes it with the real data exporter. The result is kept in the
// repository at docs/archive-data-sample, so a tool can be designed against
// it without any real data; this test fails when the exporter's output no
// longer matches it. IX_SAMPLE_OUT=<folder> writes a fresh copy there.
func TestIxDataSample(t *testing.T) {
	ixBackgroundQueue.Store(false)
	defer ixBackgroundQueue.Store(true)
	ixVdupBackground.Store(false)
	defer ixVdupBackground.Store(true)
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	const (
		club   = "-1001811230001" // channel
		forum  = "-1001811230002" // forum supergroup with topics
		makers = "-1001811230003" // files downloaded before the archive existed
		day    = 1773489600       // 2026-03-14 12:00 UTC
		seen   = int64(1780000000000)
	)
	cfg, err := a.config.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg["groups"] = []any{
		map[string]any{"id": club, "name": "Riverside Cycling Club", "enabled": true, "category": "Cycling", "filters": map[string]any{"photos": true, "videos": true, "files": true}},
		map[string]any{"id": forum, "name": "Trail Builders Forum", "enabled": true, "category": "Cycling", "filters": map[string]any{"photos": true, "videos": true, "files": true}},
		map[string]any{"id": makers, "name": "Ottawa Maker Space", "enabled": true, "category": "Makers", "filters": map[string]any{"photos": true, "videos": true, "files": false}},
	}
	if err = a.config.Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.db.Writer.Exec(q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	for _, p := range [][3]string{
		{"user:1001", "Maya Chen", "mayachen"},
		{"user:1002", "Liam Ortiz", ""},
		{"user:1003", "Priya Natarajan", "priya_n"},
		{"channel:1811230001", "Riverside Cycling Club", "riversidecc"},
		{"channel:1811239999", "Velo Weekly", "veloweekly"},
	} {
		kind := strings.SplitN(p[0], ":", 2)[0]
		exec(`INSERT INTO ix_peers(peer_id,kind,name,username,updated_at) VALUES(?,?,?,?,?)`, p[0], kind, p[1], p[2], seen)
	}
	exec(`INSERT INTO ix_topics(group_id,topic_id,title,title_date) VALUES(?,1,'General',?),(?,14,'Trail reports',?),(?,27,'Tools & repairs',?)`, forum, day, forum, day, forum, day)

	// Media, built as Telegram sends it.
	photo := func(id int64, size int) tg.MessageMediaClass {
		return &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: id, AccessHash: id * 7919, FileReference: []byte{1, 2, 3, 4}, Date: day, DCID: 2,
			Sizes: []tg.PhotoSizeClass{&tg.PhotoStrippedSize{Type: "i", Bytes: []byte{1, 40, 30}}, &tg.PhotoSize{Type: "m", W: 320, H: 240, Size: size / 9}, &tg.PhotoSize{Type: "y", W: 1280, H: 960, Size: size}}}}
	}
	document := func(id, size int64, mime string, attrs ...tg.DocumentAttributeClass) tg.MessageMediaClass {
		return &tg.MessageMediaDocument{Document: &tg.Document{ID: id, AccessHash: id * 7919, FileReference: []byte{5, 6, 7, 8}, Date: day, MimeType: mime, Size: size, DCID: 2, Attributes: attrs}}
	}
	video := func(id, size int64, seconds float64, name string) tg.MessageMediaClass {
		return document(id, size, "video/mp4", &tg.DocumentAttributeVideo{Duration: seconds, W: 1920, H: 1080, SupportsStreaming: true}, &tg.DocumentAttributeFilename{FileName: name})
	}
	type msg struct {
		group     string
		m         *tg.Message
		deleted   int64
		olderText []string // earlier versions, oldest first
	}
	user := func(id int64) tg.PeerClass { return &tg.PeerUser{UserID: id} }
	in := func(group string) *tg.PeerChannel {
		var id int64
		switch group {
		case club:
			id = 1811230001
		case forum:
			id = 1811230002
		}
		return &tg.PeerChannel{ChannelID: id}
	}
	post := func(group string, id, at int, text string) *tg.Message {
		return &tg.Message{ID: id, Date: day + at, Message: text, PeerID: in(group)}
	}
	var msgs []msg
	add := func(group string, m *tg.Message, mutate ...func(*msg)) {
		x := msg{group: group, m: m}
		for _, f := range mutate {
			f(&x)
		}
		msgs = append(msgs, x)
	}

	// Riverside Cycling Club: a channel with signed posts.
	m := post(club, 101, 0, "Spring Ride is on! Saturday 9:00 at the boathouse. Route and sign-up: riverside.example/spring")
	m.Entities = []tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 0, Length: 18}, &tg.MessageEntityTextURL{Offset: 66, Length: 25, URL: "https://riverside.example/spring"}}
	m.PostAuthor, m.Views = "Maya Chen", 1520
	add(club, m)
	for i, id := range []int{102, 103, 104} {
		m = post(club, id, 3600, "")
		if i == 0 {
			m.Message = "Photos from the ride: 64 riders, no flats (almost)."
		}
		m.GroupedID, m.Media, m.PostAuthor, m.Views = 3711000000000000001, photo(5100000000000000102+int64(i), 412000+int64(i)*9000), "Liam Ortiz", 1388
		add(club, m)
	}
	m = post(club, 105, 7200, "Ride recap video, 4K on the club drive.")
	m.Media, m.Views = video(5200000000000000105, 48_211_337, 94.5, "spring-ride-recap.mp4"), 1702
	add(club, m)
	m = post(club, 106, 9000, "Route map for Sunday (GPX inside).")
	m.Media, m.Views = document(5300000000000000106, 182_044, "application/gpx+xml", &tg.DocumentAttributeFilename{FileName: "sunday-route.gpx"}), 940
	add(club, m) // wanted, not downloaded yet: "missing"
	m = post(club, 107, 10800, "Correction: the boathouse lot is closed, park at Lansdowne.")
	m.EditDate = day + 11400
	add(club, m, func(x *msg) { x.olderText = []string{"Parking at the boathouse lot as usual."} })
	m = post(club, 108, 12600, "Tickets for the gala are sold out, sorry!")
	add(club, m, func(x *msg) { x.deleted = seen + 86_400_000 }) // gone from Telegram since
	m = post(club, 109, 14400, "")
	m.FwdFrom = tg.MessageFwdHeader{FromID: &tg.PeerChannel{ChannelID: 1811239999}, Date: day - 86400, ChannelPost: 812}
	m.Message = "Five tips for wet-weather braking."
	m.Media = photo(5100000000000000109, 268000)
	add(club, m)
	m = post(club, 110, 16200, "")
	m.Media = document(5400000000000000110, 23_018, "application/x-tgsticker", &tg.DocumentAttributeSticker{Alt: "👍", Stickerset: &tg.InputStickerSetEmpty{}})
	add(club, m) // stickers are off for this chat: "not_wanted"

	// Trail Builders Forum: topics, replies, people.
	// Topics 14 and 27 were opened by service messages (only their titles
	// are kept, in topics.jsonl); posts in a topic "reply" to its opener.
	m = post(forum, 5, 0, "Welcome! Pick a topic: Trail reports for conditions, Tools & repairs for wrenching.")
	m.FromID = user(1003)
	add(forum, m)
	m = post(forum, 201, 1800, "Ridge loop is dry, two trees down past the second bridge.")
	m.FromID, m.ReplyTo = user(1001), &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 14}
	add(forum, m)
	m = post(forum, 202, 2400, "Thanks! Bringing a saw on Sunday.")
	m.FromID, m.ReplyTo = user(1002), &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 201, ReplyToTopID: 14}
	add(forum, m)
	m = post(forum, 203, 3000, "Same recap the club posted, for anyone not in the channel.")
	m.FromID, m.ReplyTo = user(1001), &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 14}
	m.FwdFrom = tg.MessageFwdHeader{FromID: &tg.PeerChannel{ChannelID: 1811230001}, Date: day + 7200, ChannelPost: 105}
	m.Media = video(5200000000000000105, 48_211_337, 94.5, "spring-ride-recap.mp4") // the very same Telegram file
	add(forum, m)
	m = post(forum, 204, 3600, "Just the descent, trimmed.")
	m.FromID, m.ReplyTo = user(1002), &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 14}
	m.Media = video(5200000000000000204, 9_870_112, 21.0, "descent-clip.mp4")
	add(forum, m)
	m = post(forum, 301, 5400, "Voice note: how I bleed my brakes.")
	m.FromID, m.ReplyTo = user(1002), &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 27}
	m.Media = document(5500000000000000301, 61_440, "audio/ogg", &tg.DocumentAttributeAudio{Voice: true, Duration: 47})
	add(forum, m)
	m = post(forum, 302, 6000, "Torque spec sheet attached.")
	m.FromID, m.ReplyTo = user(1003), &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 27}
	m.Media = document(5300000000000000302, 1_204_331, "application/pdf", &tg.DocumentAttributeFilename{FileName: "torque-specs-2026.pdf"})
	add(forum, m)
	m = post(forum, 303, 6600, "Anyone selling a used truing stand?")
	m.FromID = user(1001)
	add(forum, m) // no topic mark: General

	media := map[string]telegram.Attachment{}
	for _, x := range msgs {
		// Through Telegram's own encoding, exactly as recorded live.
		var buf bin.Buffer
		if err := x.m.Encode(&buf); err != nil {
			t.Fatal(err)
		}
		var decoded tg.Message
		if err := decoded.Decode(&buf); err != nil {
			t.Fatal(err)
		}
		row := ixRowFromMessage(&decoded)
		exec(`INSERT INTO ix_messages(group_id,message_id,date,edit_date,text,grouped_id,sender_id,post_author,fwd_from,fwd_date,reply_to,topic_id,media_type,media_name,media_size,media_key,views,tl,first_seen,seen_at,deleted_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, x.group, row.MessageID, row.Date, row.EditDate, row.Text, row.GroupedID, row.SenderID, row.PostAuthor,
			row.FwdFrom, row.FwdDate, row.ReplyTo, row.TopicID, row.MediaType, row.MediaName, row.MediaSize, row.MediaKey, row.Views, row.TL, seen, seen, x.deleted)
		for i, older := range x.olderText {
			exec(`INSERT INTO ix_message_edits(group_id,message_id,edit_date,text,recorded_at) VALUES(?,?,?,?,?)`, x.group, row.MessageID, row.Date, older, seen+int64(i))
		}
		if att, err := telegram.MessageAttachment(&decoded); err == nil {
			media[x.group+"/"+ixItoa(int64(row.MessageID))] = att
		}
	}

	// Downloaded files. Identical Telegram files share one file on disk.
	hash := func(s string) string { h := sha256.Sum256([]byte("sample:" + s)); return hex.EncodeToString(h[:]) }
	file := func(group string, message int, path, kind string, size int64, at string) string {
		t.Helper()
		att := media[group+"/"+ixItoa(int64(message))]
		h := hash(path)
		var tk, tid any
		var tsize any
		if att.Identity.ID != "" {
			tk, tid, tsize = att.Identity.Kind, att.Identity.ID, att.Identity.Size
		}
		exec(`INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,status,created_at,file_hash,telegram_media_kind,telegram_media_id,telegram_media_size)
			VALUES(?,?,?,?,?,?,?,'completed',?,?,?,?,?)`, group, "", message, filepath.Base(path), size, kind, path, at, h, tk, tid, tsize)
		return h
	}
	file(club, 102, "Riverside Cycling Club/images/2026-03-14_102.jpg", "photo", 412000, "2026-03-14 13:01:07")
	file(club, 103, "Riverside Cycling Club/images/2026-03-14_103.jpg", "photo", 421000, "2026-03-14 13:01:09")
	file(club, 104, "Riverside Cycling Club/images/2026-03-14_104.jpg", "photo", 430000, "2026-03-14 13:01:12")
	recap := file(club, 105, "Riverside Cycling Club/videos/spring-ride-recap.mp4", "video", 48_211_337, "2026-03-14 14:02:44")
	file(club, 109, "Riverside Cycling Club/images/2026-03-14_109.jpg", "photo", 268000, "2026-03-14 16:00:31")
	exec(`INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,status,created_at,file_hash,telegram_media_kind,telegram_media_id,telegram_media_size)
		SELECT ?,'',203,file_name,file_size,file_type,file_path,'completed','2026-03-14 12:51:02',file_hash,telegram_media_kind,telegram_media_id,telegram_media_size FROM downloads WHERE group_id=? AND message_id=105`, forum, club)
	clip := file(forum, 204, "Trail Builders Forum/videos/descent-clip.mp4", "video", 9_870_112, "2026-03-14 13:00:40")
	file(forum, 301, "Trail Builders Forum/documents/voice_5500000000000000301.ogg", "document", 61_440, "2026-03-14 13:31:00")
	// Ottawa Maker Space: downloaded by the downloader before the archive
	// existed; its messages were never captured.
	file(makers, 57, "Ottawa Maker Space/images/laser-cutter-test.jpg", "photo", 198_220, "2025-12-02 18:12:00")
	reencode := file(makers, 58, "Ottawa Maker Space/videos/club-ride-720p.mp4", "video", 17_404_090, "2025-12-02 18:13:30")

	// Near-duplicate videos, as the fingerprinting finds them: a re-encoded
	// copy of the recap, and a clip cut from it.
	for _, v := range []struct {
		h, path string
		dur     float64
	}{{recap, "Riverside Cycling Club/videos/spring-ride-recap.mp4", 94.5}, {clip, "Trail Builders Forum/videos/descent-clip.mp4", 21.0}, {reencode, "Ottawa Maker Space/videos/club-ride-720p.mp4", 94.4}} {
		exec(`INSERT INTO ix_vfp(key,file_path,duration,frames,status,error,updated_at) VALUES(?,?,?,X'00',?,'',?)`, "h:"+v.h, v.path, v.dur, "ok", seen)
	}
	pair := func(a, b, kind string, score, offset float64) {
		if a > b {
			a, b = b, a
		}
		exec(`INSERT INTO ix_vdup(key_a,key_b,kind,score,matched,offset_s,found_at) VALUES(?,?,?,?,?,?,?)`, "h:"+a, "h:"+b, kind, score, 8, offset, seen)
	}
	pair(recap, reencode, "same", 0.94, 0.0)
	pair(recap, clip, "clip", 0.81, 61.5)

	exec(`INSERT INTO ix_walks(job_id,group_id,started_at,finished_at,processed,marked_deleted,state) VALUES('sample-1',?,?,?,10,1,'done'),('sample-2',?,?,?,13,0,'done')`,
		club, seen, seen+60_000, forum, seen, seen+90_000)

	out := t.TempDir()
	count := 0
	if err = a.ixWriteData(ctx, out, filepath.Join(out, "none"), "/volume1/Telegram", func(string) {}, &count); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(out, "data")
	_ = os.Remove(filepath.Join(data, "state.json")) // the exporter's own bookkeeping
	var manifest map[string]any
	raw, err := os.ReadFile(filepath.Join(data, "manifest.json"))
	if err != nil || json.Unmarshal(raw, &manifest) != nil {
		t.Fatalf("manifest: %v", err)
	}
	manifest["exported_at"] = "2026-11-03T05:00:00Z"
	raw, _ = json.MarshalIndent(manifest, "", "  ")
	if err = os.WriteFile(filepath.Join(data, "manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// Readable copies of the compressed message files.
	gz, _ := filepath.Glob(filepath.Join(data, "messages", "*.jsonl.gz"))
	for _, name := range gz {
		f, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := io.ReadAll(zr)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(strings.TrimSuffix(name, ".gz"), plain, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// What a tool needs is there.
	club101 := ixSampleLine(t, filepath.Join(data, "messages", "g_1811230001.jsonl"), 101)
	if rawMsg, _ := club101["raw"].(map[string]any); rawMsg == nil || len(rawMsg["entities"].([]any)) != 2 {
		t.Fatalf("formatting entities missing from raw: %v", club101)
	}
	forum203 := ixSampleLine(t, filepath.Join(data, "messages", "g_1811230002.jsonl"), 203)
	if forum203["topic_id"] != float64(14) || forum203["file_status"] != "downloaded" || forum203["forwarded_from"] != "channel:1811230001" {
		t.Fatalf("forum 203: %v", forum203)
	}
	if ixSampleLine(t, filepath.Join(data, "messages", "g_1811230002.jsonl"), 301)["media"].(map[string]any)["type"] != "voice" {
		t.Fatal("voice note")
	}

	files := map[string]bool{}
	_ = filepath.Walk(data, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(data, p)
			files[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	if dst := os.Getenv("IX_SAMPLE_OUT"); dst != "" {
		for rel := range files {
			body, err := os.ReadFile(filepath.Join(data, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dst, "data", filepath.FromSlash(rel))
			if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(target, body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("sample written to %s", dst)
	}
	// The copy kept in the repository must match what the exporter writes
	// now (compressed files and the manifest's version aside).
	committed := filepath.Join("..", "..", "..", "docs", "archive-data-sample", "data")
	if _, err := os.Stat(committed); err != nil {
		t.Logf("no committed sample at %s; skipping the comparison", committed)
		return
	}
	var names []string
	for rel := range files {
		if !strings.HasSuffix(rel, ".gz") && rel != "manifest.json" {
			names = append(names, rel)
		}
	}
	sort.Strings(names)
	for _, rel := range names {
		want, err := os.ReadFile(filepath.Join(committed, filepath.FromSlash(rel)))
		got, _ := os.ReadFile(filepath.Join(data, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
			t.Errorf("docs/archive-data-sample/data/%s is out of date: regenerate it (IX_SAMPLE_OUT) and commit it", rel)
		}
	}
}

func ixSampleLine(t *testing.T, path string, id int) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if m["message_id"] == float64(id) {
			return m
		}
	}
	t.Fatalf("%s: no message %d", path, id)
	return nil
}
