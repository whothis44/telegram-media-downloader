package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func serverMessage(id int, dc int, size int64) *tg.Message {
	return &tg.Message{ID: id, Date: 1, PeerID: &tg.PeerChannel{ChannelID: 42}, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: int64(id), Size: size, DCID: dc}}}
}

func serverOf(t *testing.T, s *WorkStore, msg int) int {
	t.Helper()
	var dc int
	if err := s.reader.QueryRow(`SELECT dc FROM tgdl_work WHERE message_id=?`, msg).Scan(&dc); err != nil {
		t.Fatal(err)
	}
	return dc
}

// ix.12.4: no server holds more files than it has slots while another
// server has files queued. With the slots of server 1 taken, the next claim
// is the best file of another server, not a third file waiting on server 1.
func TestClaimsLeaveAFullServerForAnother(t *testing.T) {
	for _, lane := range []int{0, 2} {
		withLane(t, lane)
		s := testStore(t)
		ctx := context.Background()
		const mb = int64(1) << 20
		// Queue order: three large files on server 1, then server 4's.
		for i, m := range []struct{ dc int }{{1}, {1}, {1}, {4}, {4}} {
			if _, _, err := enqueueWork(ctx, s.writer, "one", Target{}, serverMessage(i+1, m.dc, 100*mb), 0, "history"); err != nil {
				t.Fatal(err)
			}
		}
		if serverOf(t, s, 1) != 1 || serverOf(t, s, 4) != 4 {
			t.Fatal("the queue did not record the servers")
		}
		var got []int64
		for i := 0; i < 4; i++ {
			full, err := s.FullServers(ctx, 5, 2)
			if err != nil {
				t.Fatal(err)
			}
			w, err := s.ClaimAvoiding(ctx, []string{"one"}, time.Now(), true, full)
			if err != nil || w == nil {
				t.Fatalf("lane %d claim %d: %+v %v", lane, i, w, err)
			}
			got = append(got, w.MessageID)
		}
		if got[0] != 1 || got[1] != 2 || got[2] != 4 || got[3] != 5 {
			t.Fatalf("lane %d: claimed %v, want 1 2 4 5 (message 3 waits for server 1)", lane, got)
		}
		full, _ := s.FullServers(ctx, 5, 2)
		if w, _ := s.ClaimAvoiding(ctx, []string{"one"}, time.Now(), true, full); w != nil {
			t.Fatalf("lane %d: claimed %+v with every server full", lane, w)
		}
		// A slot frees: message 3 goes.
		if _, err := s.writer.Exec(`UPDATE tgdl_work SET status='completed' WHERE message_id=1`); err != nil {
			t.Fatal(err)
		}
		full, _ = s.FullServers(ctx, 5, 2)
		if w, _ := s.ClaimAvoiding(ctx, []string{"one"}, time.Now(), true, full); w == nil || w.MessageID != 3 {
			t.Fatalf("lane %d: %+v, want message 3", lane, w)
		}
	}
}

// Small and large files have their own slots on a server.
func TestSmallFilesHaveTheirOwnSlots(t *testing.T) {
	withLane(t, 0)
	s := testStore(t)
	ctx := context.Background()
	const mb = int64(1) << 20
	for i, size := range []int64{100 * mb, 100 * mb, 100 * mb, 2 * mb} {
		if _, _, err := enqueueWork(ctx, s.writer, "one", Target{}, serverMessage(i+1, 1, size), 0, "history"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		full, _ := s.FullServers(ctx, 5, 2)
		if w, _ := s.ClaimAvoiding(ctx, []string{"one"}, time.Now(), true, full); w == nil {
			t.Fatal("no claim")
		}
	}
	full, _ := s.FullServers(ctx, 5, 2)
	w, _ := s.ClaimAvoiding(ctx, []string{"one"}, time.Now(), true, full)
	if w == nil || w.MessageID != 4 {
		t.Fatalf("%+v, want the small file 4", w)
	}
	full, _ = s.FullServers(ctx, 5, 2)
	if w, _ := s.ClaimSmallerAvoiding(ctx, []string{"one"}, time.Now(), true, 20*mb, full); w != nil {
		t.Fatalf("claimed %+v", w)
	}
}

// Rows queued before ix.12.4 get their server from their stored message; a
// row whose message says none is marked and never left out of claims.
func TestFillServersReadsOldRows(t *testing.T) {
	withLane(t, 0)
	s := testStore(t)
	ctx := context.Background()
	for i, dc := range []int{4, 5, 1} {
		var buf bin.Buffer
		if err := serverMessage(i+1, dc, 1<<20).Encode(&buf); err != nil {
			t.Fatal(err)
		}
		if _, err := s.writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at)
			VALUES('one','-1001','G',?,1,?,'video','f.mp4',1048576,?,'pending','history',1,1)`, i+1, "id"+string(rune('a'+i)), buf.Buf); err != nil {
			t.Fatal(err)
		}
	}
	addOldRow(t, s, 9, 1<<20, "history", 0) // a body that is no message
	for {
		n, err := s.FillServers(ctx, 2)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if serverOf(t, s, 1) != 4 || serverOf(t, s, 2) != 5 || serverOf(t, s, 3) != 1 || serverOf(t, s, 9) != -1 {
		t.Fatalf("servers %d %d %d %d", serverOf(t, s, 1), serverOf(t, s, 2), serverOf(t, s, 3), serverOf(t, s, 9))
	}
	full := []int{ServerKey(4, false), ServerKey(5, false), ServerKey(1, false)}
	w, err := s.ClaimAvoiding(ctx, []string{"one"}, time.Now(), true, full)
	if err != nil || w == nil || w.MessageID != 9 {
		t.Fatalf("%+v %v, want the row of no known server", w, err)
	}
}

// Before workers start, only the rows they could take are read: paused rows
// (held copies, files set aside) wait for the background fill.
func TestFillServersCanTakeClaimableRowsFirst(t *testing.T) {
	withLane(t, 0)
	s := testStore(t)
	ctx := context.Background()
	for i, paused := range []int{0, 1} {
		var buf bin.Buffer
		if err := serverMessage(i+1, 4, 1<<20).Encode(&buf); err != nil {
			t.Fatal(err)
		}
		if _, err := s.writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,paused,origin,created_at,updated_at)
			VALUES('one','-1001','G',?,1,?,'video','f.mp4',1048576,?,'pending',?,'history',1,1)`, i+1, "id"+string(rune('a'+i)), buf.Buf, paused); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.FillServers(ctx, 500, true); err != nil || n != 1 {
		t.Fatalf("claimable fill: %d %v", n, err)
	}
	if serverOf(t, s, 1) != 4 || serverOf(t, s, 2) != 0 {
		t.Fatalf("servers %d %d", serverOf(t, s, 1), serverOf(t, s, 2))
	}
	if n, _ := s.FillServers(ctx, 500, true); n != 0 {
		t.Fatal("claimable rows read twice")
	}
	if n, _ := s.FillServers(ctx, 500); n != 1 || serverOf(t, s, 2) != 4 {
		t.Fatal("the background fill did not take the paused row")
	}
}

type slotAccount struct{ connectingAccount }

func (slotAccount) FileSlots() (small, large int) { return 5, 2 }

// 25 workers claiming at the same moment, as at a start, still leave a
// server no more files than its slots (ix.12.5: they counted the same free
// slots and one server got 8 large files for 2 slots).
func TestClaimsAtOnceKeepTheServerCap(t *testing.T) {
	withLane(t, 0)
	s := testStore(t)
	ctx := context.Background()
	const mb = int64(1) << 20
	for i := 0; i < 40; i++ {
		dc := 1
		if i%4 == 3 {
			dc = 4
		}
		if _, _, err := enqueueWork(ctx, s.writer, "one", Target{}, serverMessage(i+1, dc, 100*mb), 0, "history"); err != nil {
			t.Fatal(err)
		}
	}
	run := &running{ctx: ctx, ids: []string{"one"}, accounts: map[string]Account{"one": &slotAccount{}}}
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := run.claimNext(s, false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	full, err := s.FullServers(ctx, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	var n1, n4 int
	_ = s.reader.QueryRow(`SELECT COUNT(*) FROM tgdl_work WHERE status='processing' AND dc=1`).Scan(&n1)
	_ = s.reader.QueryRow(`SELECT COUNT(*) FROM tgdl_work WHERE status='processing' AND dc=4`).Scan(&n4)
	if n1 != 2 || n4 != 2 || len(full) != 2 {
		t.Fatalf("server 1 has %d large files, server 4 %d (2 slots each); full %v", n1, n4, full)
	}
}
