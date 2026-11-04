package app

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestIxDeadlinePutsPrivateChatsFirst(t *testing.T) {
	a, ctx := ix8App(t, map[string]any{"deadlineOrder": "on"})
	now := time.Now()
	add := func(group string, msg int, size int64, status string) int64 {
		res, err := a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at)
			VALUES('acc',?,'G',?,1,?,'video','f',?,X'',?,'history',?,?)`, group, msg, group+fmt.Sprint(msg), size, status, now.UnixMilli(), now.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	paused := func(id int64) bool {
		var p int
		_ = a.db.Reader.QueryRow(`SELECT paused FROM tgdl_work WHERE id=?`, id).Scan(&p)
		return p == 1
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_group_info(group_id,title,kind,username) VALUES('-1002','Public','channel','somechannel')`); err != nil {
		t.Fatal(err)
	}
	add("-1001", 1, 1<<20, "completed")
	privBig := add("-1001", 2, 800<<20, "pending")
	pubSmall := add("-1002", 3, 1<<20, "pending")
	_ = a.ixDeadlineStep(ctx)
	if paused(privBig) || !paused(pubSmall) {
		t.Fatalf("a public chat's small file went before a private chat's large one: private big paused %v, public small paused %v", paused(privBig), paused(pubSmall))
	}
	if d := a.ixDeadlineEstimate(ctx, false); d.Deferred[ixPublicLane] != 1 {
		t.Fatalf("deferred by band: %v", d.Deferred)
	}
	// The private chat done: the public one goes.
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='completed' WHERE id=?`, privBig)
	_ = a.ixDeadlineStep(ctx)
	if paused(pubSmall) {
		t.Fatal("the public chat's file was not let go")
	}
}

func TestIxReportHasAnHourByHourRecordWithoutNames(t *testing.T) {
	a, ctx := ix8App(t, nil)
	now := time.Now()
	for i, e := range []string{"", "download Telegram media: FLOOD_PREMIUM_WAIT_30 SecretChatName", "write \\\\nas\\share\\Private Group\\clip.mp4.part: An unexpected network error occurred."} {
		status := "completed"
		if e != "" {
			status = "failed"
		}
		if _, err := a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at,error)
			VALUES('acc','-1001','Private Group',?,1,?,'video','clip.mp4',?,X'',?,'history',?,?,?)`, i+1, fmt.Sprint(i), 5<<20, status, now.UnixMilli(), now.UnixMilli(), e); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Join(a.ixDiagHours(ctx, now), "\n")
	for _, want := range []string{"last 24 h: 5 MB in 1 files", "speed limit 1", "disk or NAS 1", "what limits downloads"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("missing %q in:\n%s", want, lines)
		}
	}
	for _, leak := range []string{"Secret", "Private", "clip", "nas", "share"} {
		if strings.Contains(lines, leak) {
			t.Fatalf("report leaks %q:\n%s", leak, lines)
		}
	}
}

func TestIxVideosAreFoundByTheirBestFrame(t *testing.T) {
	m := &ixModel{ByKey: map[string]int32{"pic": 0, "vid": 1, "other": 2}, Items: []ixMItem{{Cluster: 0}, {Cluster: 1}, {Cluster: 2}}}
	hits := []ixEmbHit{{Key: "pic", Sim: 0.30}, {Key: "vid", Sim: 0.20}}
	frames := []ixFrameHit{{Key: "vid", Sim: 0.40, AtMs: 35000}, {Key: "other", Sim: 0.25, AtMs: 5000}}
	got := ixMergeFrameHits(m, hits, frames, 10)
	if len(got) != 3 || got[0].Key != "vid" || got[0].Sim != 0.40 || got[0].AtMs != 35000 || got[1].Key != "pic" || got[2].Key != "other" || got[2].AtMs != 5000 {
		t.Fatalf("merged: %+v", got)
	}
}

func TestIxReportHidesWindowsPathsWithSpaces(t *testing.T) {
	r := &ixRedactor{groups: map[string]string{}}
	for _, in := range []string{
		`write \\NAS\Media\Telegram Archive\Family Group Chat\videos\.tgdl-clip.part: An unexpected network error occurred.`,
		`open Z:\Telegram Archive\Ahmed and Sara\videos\x.mp4: The system cannot find the file specified.`,
		`backup to Z:/Telegram index backups/2026-11-03 final failed`,
	} {
		out := r.clean(in)
		for _, leak := range []string{"Archive", "Family", "Ahmed", "Sara", "videos", "backups", "Media"} {
			if strings.Contains(out, leak) {
				t.Fatalf("%q leaks %q: %s", in, leak, out)
			}
		}
		if !strings.Contains(out, "<path>") {
			t.Fatalf("no placeholder: %s", out)
		}
	}
	if out := r.clean(`open C:\data\x.db: access denied`); out != "open <path>: access denied" {
		t.Fatalf("the error text after the path should stay: %q", out)
	}
}
