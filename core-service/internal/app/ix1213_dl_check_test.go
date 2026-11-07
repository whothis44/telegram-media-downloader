package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/download"
	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// ixDlFakeClient is a download's client bound to one file.
type ixDlFakeClient struct {
	data  []byte
	reads int
}

func (c *ixDlFakeClient) Download(context.Context, telegram.MediaIdentity, io.Writer) error {
	return errors.New("not in this test")
}
func (c *ixDlFakeClient) FileHashes(context.Context, int64) ([]tg.FileHash, error) {
	return nil, errors.New("no hashes")
}
func (c *ixDlFakeClient) ReadPart(_ context.Context, off int64) ([]byte, error) {
	c.reads++
	end := min(off+telegram.PartSize, int64(len(c.data)))
	return c.data[off:end], nil
}

// ix.12.13: a big video is checked as the first step of its download: a
// copy is deferred (held at the back, in the engine's transaction), anything
// else downloads at once with the parts the check read; a second claim is
// not checked again; with the setting off nothing is checked.
func TestIx1213BigVideoIsCheckedInItsDownload(t *testing.T) {
	a, ctx := ixTestApp(t)
	savedFake, frameFake := ixSampleFake, ixSampleFrame
	ixSampleFake = true
	defer func() { ixSampleFake, ixSampleFrame = savedFake, frameFake }()
	hashAt := func(s int) uint64 { return uint64(s+1) * 0x9E3779B97F4A7C15 }
	var fp []ixVFrame
	for s := 0; s < 3000; s++ {
		fp = append(fp, ixVFrame{T: float32(s), H: hashAt(s), HF: ^hashAt(s)})
	}
	if err := a.ixVdupSave(ctx, "h:"+ixSixtyFour("7"), "c/long.mp4", 3000, fp, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_type,file_path,status,file_hash) VALUES(?,90,'long.mp4','video','c/long.mp4','completed',?)`, ixTestGroup, ixSixtyFour("7")); err != nil {
		t.Fatal(err)
	}
	ixSampleFrame = func(ctx context.Context, address string, at float64) (ixSampledFrame, bool, error) {
		resp, err := http.Get(address)
		if err != nil {
			return ixSampledFrame{}, false, err
		}
		b := make([]byte, 16)
		_, _ = io.ReadFull(resp.Body, b)
		resp.Body.Close()
		if string(b) == "COPY-COPY-COPY-1" {
			return ixSampledFrame{T: at, H: hashAt(int(at)), HF: ^hashAt(int(at))}, true, nil
		}
		return ixSampledFrame{T: at, H: uint64(at*1000) ^ 0x5555, HF: 1}, true, nil
	}
	type row struct {
		work   *engine.Work
		msg    *tg.Message
		item   download.Item
		client *ixDlFakeClient
	}
	queue := func(doc int64, mark string) row {
		t.Helper()
		data := make([]byte, 3*telegram.PartSize)
		copy(data, mark)
		msg := ixTestVideo(int(doc), doc, 1536<<20, 3000, 1280, 720, nil, 1700000000)
		id := ixTestQueue(t, a, msg)
		att, err := telegram.MessageAttachment(msg)
		if err != nil {
			t.Fatal(err)
		}
		w := &engine.Work{ID: id, GroupID: ixTestGroup, MessageID: doc, Origin: "history", Identity: att.Identity.Key()}
		return row{w, msg, download.Item{GroupID: ixTestGroup, MessageID: doc, Type: "video", Identity: att.Identity}, &ixDlFakeClient{data: data}}
	}
	check := func(r row) error {
		cctx, _ := ixWithDlCheck(ctx, r.work, r.msg)
		return a.ixDlCheck(cctx, r.item, r.client)
	}
	// A copy: deferred, with its hold.
	cp := queue(901, "COPY-COPY-COPY-1")
	err := check(cp)
	var d *telegram.DownloadDeferred
	if !errors.As(err, &d) || d.Hold == nil {
		t.Fatalf("a copy was not deferred: %v", err)
	}
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Hold(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if how, _ := ixTestHow(a, cp.work.ID); how != "sampled" {
		t.Fatalf("held as %q", how)
	}
	var confirm string
	_ = a.db.Reader.QueryRow(`SELECT confirm FROM ix_copy_hold WHERE work_id=?`, cp.work.ID).Scan(&confirm)
	if confirm != "confirmed" {
		t.Fatalf("confirm %q", confirm)
	}
	// Let go later (all else done): its next download is not checked again.
	if err = a.ixLetGo(ctx, cp.work.ID); err != nil {
		t.Fatal(err)
	}
	reads := cp.client.reads
	if err = check(cp); err != nil || cp.client.reads != reads {
		t.Fatalf("checked again after it was let go: %v reads %d→%d", err, reads, cp.client.reads)
	}
	// Not a copy: downloads now, its parts in memory for the transfer.
	nc := queue(902, "NONE-NONE-NONE-1")
	if err = check(nc); err != nil {
		t.Fatalf("not a copy: %v", err)
	}
	ident := ixPartIdent(nc.item.Identity)
	if ixMemPartsGet(a, ident) == nil {
		t.Fatal("the parts read are not in memory for the download")
	}
	kept := a.ixKeptPartsFor(nc.item.Identity)
	if p, ok := kept(0); kept == nil || !ok || string(p[:16]) != "NONE-NONE-NONE-1" {
		t.Fatal("the download does not get the parts the check read")
	}
	var result string
	_ = a.db.Reader.QueryRow(`SELECT result FROM ix_copy_sample WHERE work_id=?`, nc.work.ID).Scan(&result)
	if result != "nomatch" {
		t.Fatalf("recorded %q", result)
	}
	reads = nc.client.reads
	if err = check(nc); err != nil || nc.client.reads != reads {
		t.Fatalf("a checked video was checked again: %v", err)
	}
	ixMemPartsDrop(a, ident)
	// The switch: off, nothing is checked.
	st := a.ixLoadSettings(ctx)
	off := false
	st.CheckInDownload = &off
	if err = a.ixSaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	other := queue(903, "COPY-COPY-COPY-1")
	if err = check(other); err != nil || other.client.reads != 0 {
		t.Fatalf("checked with the setting off: %v reads %d", err, other.client.reads)
	}
	if line := a.ixDlCheckLine(ctx); line == "" {
		t.Fatal("no report line")
	} else {
		t.Log(line)
	}
}
