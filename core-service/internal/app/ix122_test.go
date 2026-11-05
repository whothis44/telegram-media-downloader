package app

import (
	"strings"
	"testing"
	"time"
)

// A file a ranked group needs keeps its full quality: the lower-quality-first rule
// judged by size alone and would have sent the original behind everything else.
func TestIxLowerQualityLeavesRankedFilesAlone(t *testing.T) {
	a, ctx := lowqApp(t, map[string]any{"deadlineOrder": "auto"}, false)
	transport, got := lowqTransport()
	msg := lowqVideo(7)
	work := lowqWork(t, a, msg, 1000)
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET sched_order=? WHERE id=?`, 16, work.ID); err != nil {
		t.Fatal(err)
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
		t.Fatal("a ranked file was saved in a lower quality")
	}
	// (Unranked, the same file gets the lower quality first:
	// TestIxLowerQualityFirstThenTheOriginalReplacesIt.)
}

func TestQueueRowsOpenTheFileSavedForThem(t *testing.T) {
	a, ctx := ixTestApp(t)
	now := time.Now().UnixMilli()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.db.Writer.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// The same Telegram file was saved for message 1; message 2 linked to it.
	exec(`INSERT INTO downloads(group_id,message_id,file_name,file_type,file_path,status,telegram_media_kind,telegram_media_id) VALUES('-1001',1,'a.mp4','video','g/a.mp4','completed','document','77')`)
	exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at) VALUES('acc','-1001','G',2,1,?,'video','a.mp4',10,X'00','completed','history',?,?)`, "document\x0077\x0010", now, now)
	// Message 3 is an exact re-upload of a saved file.
	exec(`INSERT INTO ix_subst(group_id,message_id,identity,size,of_path,how,at) VALUES('-1001',3,'document:88:10',10,'g/b.mp4','telegram-hashes',1)`)
	snapshot := map[string]any{"recent": []map[string]any{
		{"status": "done", "groupId": "-1001", "messageId": int64(2)},
		{"status": "done", "groupId": "-1001", "messageId": int64(3)},
		{"status": "done", "groupId": "-1001", "messageId": int64(4)},
		{"status": "done", "groupId": "-1001", "messageId": int64(5), "filePath": "own.mp4"},
		{"status": "failed", "groupId": "-1001", "messageId": int64(3)},
	}}
	a.queueAddSavedPaths(ctx, snapshot)
	items := snapshot["recent"].([]map[string]any)
	want := []any{"g/a.mp4", "g/b.mp4", nil, "own.mp4", nil}
	for i, it := range items {
		if it["filePath"] != want[i] {
			t.Fatalf("row %d: %v, want %v", i, it["filePath"], want[i])
		}
	}
}

func TestIxFileNameTypedInFullComesFirst(t *testing.T) {
	m := &ixModel{}
	add := func(name string) {
		m.Posts = append(m.Posts, ixMPost{})
		m.Items = append(m.Items, ixMItem{Name: name, Post: int32(len(m.Posts) - 1)})
	}
	add("IMG_2034.MP4")
	add("IMG_2034_edit.mp4")
	add("holiday video.mp4")
	add("other.mp4")
	add("")
	got := func(q string) string {
		var out []string
		for _, p := range ixNamePins(m, q) {
			out = append(out, m.Items[p].Name)
		}
		return strings.Join(out, ",")
	}
	for q, want := range map[string]string{
		"img_2034.mp4":      "IMG_2034.MP4",
		"IMG_2034":          "IMG_2034.MP4,IMG_2034_edit.mp4",
		"holiday video.mp4": "holiday video.mp4",
		"holiday":           "",
		"im":                "",
		"video":             "",
		"other":             "", // a plain word: it ranks with the rest
		"other.mp4":         "other.mp4",
	} {
		if g := got(q); g != want {
			t.Fatalf("%q: %q, want %q", q, g, want)
		}
	}
}
