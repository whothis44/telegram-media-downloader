package app

import (
	"fmt"
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
