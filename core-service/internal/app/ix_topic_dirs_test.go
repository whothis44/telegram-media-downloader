package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// Forum topic folders (ix_topic_dirs.go), with invented chats and files.
func TestIxTopicFoldersNamesAndPlan(t *testing.T) {
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const g = "-1004242"
	for _, tp := range []struct {
		id    int
		title string
	}{{5, "Invented: clips"}, {6, "invented: clips"}, {7, ""}} {
		mustExec(t, a, `INSERT INTO ix_topics(group_id,topic_id,title) VALUES(?,?,?)`, g, tp.id, tp.title)
	}
	// Names: General has none; the first name stays after a rename; a clash
	// gets the number; an unknown name is "Topic <n>".
	if d, _ := a.ixTopicDir(ctx, g, 1); d != "" {
		t.Fatalf("General: %q", d)
	}
	if d, _ := a.ixTopicDir(ctx, g, 5); d != "Invented_ clips" {
		t.Fatalf("topic 5: %q", d)
	}
	mustExec(t, a, `UPDATE ix_topics SET title='Renamed' WHERE topic_id=5`)
	if d, _ := a.ixTopicDir(ctx, g, 5); d != "Invented_ clips" {
		t.Fatalf("renamed topic moved: %q", d)
	}
	if d, _ := a.ixTopicDir(ctx, g, 6); d != "invented_ clips (6)" {
		t.Fatalf("clash: %q", d)
	}
	if d, _ := a.ixTopicDir(ctx, g, 7); d != "Topic 7" {
		t.Fatalf("no name: %q", d)
	}

	// The plan: files saved before topic folders move into them.
	write := func(rel string, body []byte) string {
		p := filepath.Join(a.downloadsDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		s := sha256.Sum256(body)
		return hex.EncodeToString(s[:])
	}
	row := func(msg, topic int, rel, sum string, size int) {
		mustExec(t, a, `INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,status,file_hash) VALUES(?,'',?,'clip',?,'video',?,'completed',?)`, g, msg, size, rel, sum)
		mustExec(t, a, `INSERT INTO ix_messages(group_id,message_id,date,topic_id,media_type) VALUES(?,?,1,?,'video')`, g, msg, topic)
	}
	in5 := "Chat/videos/clip_10_0123456789abcdef0123456789abcdef.mp4"
	sum5 := write(in5, []byte("topic five"))
	row(10, 5, in5, sum5, 10)
	general := "Chat/videos/clip_11_0123456789abcdef0123456789abcdef.mp4"
	row(11, 0, general, write(general, []byte("general!!")), 9)
	shared := "Chat/videos/clip_12_0123456789abcdef0123456789abcdef.mp4" // two topics' messages share it
	sumS := write(shared, []byte("shared file"))
	row(12, 5, shared, sumS, 11)
	row(13, 6, shared, sumS, 11)
	// Other records of the moved file follow it.
	mustExec(t, a, `INSERT INTO ix_held(work_id,group_id,message_id,like_path,like_key,reason,held_at) VALUES(77,?,99,?,'h:x','looks like a saved copy',1)`, g, in5)
	mustExec(t, a, `INSERT INTO ix_fixity(path,size,sha256,checked_at,result) VALUES(?,10,?,1,'ok')`, in5, sum5)
	mustExec(t, a, `INSERT INTO ix_msg_link(group_id,message_id,how,of_path,size) VALUES(?,98,'telegram-file',?,10)`, g, in5)

	ops, err := a.ixPlanTopicFolders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := "Chat/Invented_ clips/videos/clip_10_0123456789abcdef0123456789abcdef.mp4"
	if len(ops) != 1 || ops[0].Src != in5 || ops[0].Dst != want || ops[0].Op != "move" || ops[0].Repoint != want {
		t.Fatalf("plan: %+v", ops)
	}
	id, err := a.ixSavePlan(ctx, ixTopicPlanKind, "topics", ops)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ixApplyPlan(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.downloadsDir, filepath.FromSlash(want))); err != nil {
		t.Fatal("not moved")
	}
	check := func(path string) {
		t.Helper()
		var dl, held, fix, link string
		_ = a.db.Reader.QueryRow(`SELECT file_path FROM downloads WHERE message_id=10`).Scan(&dl)
		_ = a.db.Reader.QueryRow(`SELECT like_path FROM ix_held WHERE work_id=77`).Scan(&held)
		_ = a.db.Reader.QueryRow(`SELECT path FROM ix_fixity`).Scan(&fix)
		_ = a.db.Reader.QueryRow(`SELECT of_path FROM ix_msg_link WHERE message_id=98`).Scan(&link)
		for _, got := range []string{dl, held, fix, link} {
			if got != path {
				t.Fatalf("want every record at %s: catalogue %s, held %s, integrity %s, link %s", path, dl, held, fix, link)
			}
		}
	}
	check(want)
	// Planned again: nothing left to move.
	if ops, _ := a.ixPlanTopicFolders(ctx); len(ops) != 0 {
		t.Fatalf("planned again: %+v", ops)
	}
	if err := a.ixUndoPlan(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.downloadsDir, filepath.FromSlash(in5))); err != nil {
		t.Fatal("not moved back")
	}
	check(in5)
}
