package telegram

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// Parts that take 200 ms; the first part is refused once with a plain
// FLOOD_WAIT of 3 s.
type timedParts struct{ asked map[int64]int }

func (p *timedParts) Close() error { return nil }
func (p *timedParts) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	r := input.(*tg.UploadGetFileRequest)
	p.asked[r.Offset]++
	time.Sleep(200 * time.Millisecond)
	if r.Offset == 0 && p.asked[r.Offset] == 1 {
		return &tgerr.Error{Code: 420, Type: "FLOOD_WAIT", Argument: 3}
	}
	size := downloadPartSize
	if r.Offset > 0 {
		size = 17
	}
	output.(*tg.UploadFileBox).File = &tg.UploadFile{Bytes: bytes.Repeat([]byte{1}, size)}
	return nil
}

// ix.12.12: the hour-by-hour record says where each server's time went:
// answered, failed, waiting (the gate closed for the plain wait), and the
// seconds the plain wait asked for. Counted once per interval.
func TestPacerRecordsWhereEachServersTimeWent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newDownloadPacer("")
		pool := &timedParts{asked: map[int64]int{}}
		inv := sharedInvoker{pool: pool, guard: newFloodGuard(), pacer: p, dc: 4}
		var out bytes.Buffer
		if err := fetchParallel(context.Background(), tg.NewClient(inv), &tg.InputDocumentFileLocation{ID: 1}, downloadPartSize+17, 1, &out); err != nil {
			t.Fatal(err)
		}
		if out.Len() != downloadPartSize+17 {
			t.Fatalf("%d bytes", out.Len())
		}
		p.tick()
		p.tick() // a second interval adds nothing more
		hs := p.hoursNewestFirst()
		if len(hs) == 0 {
			t.Fatal("no hour recorded")
		}
		h := hs[0]
		if h.DCOkMs[4] < 400 || h.DCOkMs[4] > 600 {
			t.Fatalf("answered %d ms, want about 400 (two parts of 200 ms)", h.DCOkMs[4])
		}
		if h.DCFails[4] != 1 || h.DCFailMs[4] < 200 || h.DCFailMs[4] > 300 {
			t.Fatalf("failed %d requests, %d ms", h.DCFails[4], h.DCFailMs[4])
		}
		if h.DCWaitedMs[4] < 3000 || h.DCWaitSecs[4] != 3 || h.DCWaits[4] != 1 {
			t.Fatalf("waited %d ms, asked %d s in %d waits", h.DCWaitedMs[4], h.DCWaitSecs[4], h.DCWaits[4])
		}
		if h.DCSleptMs[4] != 0 {
			t.Fatalf("speed-limit sleeps %d ms without a speed-limit wait", h.DCSleptMs[4])
		}
	})
}
