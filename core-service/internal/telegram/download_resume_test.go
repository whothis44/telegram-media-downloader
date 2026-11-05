package telegram

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"

	"github.com/gotd/td/tg"
)

// A file on an invented Telegram: it answers parts of 1 MiB, in any order, and
// remembers what it was asked for.
type fakeParts struct {
	data  []byte
	mu    sync.Mutex
	asked []int64
}

func (f *fakeParts) UploadGetFile(_ context.Context, r *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
	f.mu.Lock()
	f.asked = append(f.asked, r.Offset)
	f.mu.Unlock()
	if r.Offset%downloadPartSize != 0 || r.Limit != downloadPartSize {
		return nil, errors.New("LIMIT_INVALID")
	}
	if r.Offset >= int64(len(f.data)) {
		return &tg.UploadFile{Type: &tg.StorageFileUnknown{}}, nil
	}
	end := min(r.Offset+downloadPartSize, int64(len(f.data)))
	return &tg.UploadFile{Type: &tg.StorageFileUnknown{}, Bytes: f.data[r.Offset:end]}, nil
}

func TestFetchFromAnOffsetSendsExactlyTheRest(t *testing.T) {
	data := make([]byte, 7<<20+123)
	rand.New(rand.NewSource(7)).Read(data)
	for _, threads := range []int{1, 4} {
		for _, start := range []int64{0, 1 << 20, 3 << 20, 7 << 20} {
			parts := &fakeParts{data: data}
			var out bytes.Buffer
			if err := fetchParallelFrom(context.Background(), parts, &tg.InputDocumentFileLocation{}, int64(len(data)), start, threads, &out); err != nil {
				t.Fatalf("threads %d start %d: %v", threads, start, err)
			}
			if !bytes.Equal(out.Bytes(), data[start:]) {
				t.Fatalf("threads %d start %d: %d bytes, not the rest of the file", threads, start, out.Len())
			}
			for _, offset := range parts.asked {
				if offset < start {
					t.Fatalf("threads %d start %d: fetched %d again", threads, start, offset)
				}
			}
		}
	}
}

// A failure from an offset is reported, and what was written is an unbroken run of
// the file from that offset, stopping before the failed part. How long a run depends
// on what had arrived when the others were cancelled: none at all is right too.
func TestFetchFromAnOffsetKeepsOrderOnFailure(t *testing.T) {
	data := make([]byte, 8<<20)
	rand.New(rand.NewSource(8)).Read(data)
	bad := &failingParts{fakeParts: fakeParts{data: data}, failAt: 5 << 20}
	var out bytes.Buffer
	err := fetchParallelFrom(context.Background(), bad, &tg.InputDocumentFileLocation{}, int64(len(data)), 2<<20, 4, &out)
	if err == nil {
		t.Fatal("the failure was not reported")
	}
	if out.Len() > 3<<20 || out.Len()%downloadPartSize != 0 || !bytes.Equal(out.Bytes(), data[2<<20:2<<20+out.Len()]) {
		t.Fatalf("%d bytes written, not an unbroken run before the failure", out.Len())
	}
}

type failingParts struct {
	fakeParts
	failAt int64
}

func (f *failingParts) UploadGetFile(ctx context.Context, r *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
	if r.Offset == f.failAt {
		return nil, errors.New("FILE_REFERENCE_EXPIRED")
	}
	return f.fakeParts.UploadGetFile(ctx, r)
}

type plainSource struct{ gotCtx context.Context }

func (s *plainSource) DownloadMedia(ctx context.Context, _ Attachment, _ io.Writer) error {
	s.gotCtx = ctx
	return nil
}

type resumingSource struct {
	plainSource
	can bool
}

func (s *resumingSource) ResumesDownloads() bool { return s.can }

type wrapperSource struct{ MediaDownloader }

func (w wrapperSource) UnwrapTransport() MediaDownloader { return w.MediaDownloader }

// Only a source that says so resumes, seen through the wrappers, and the wrappers
// pass the offset on: a source that ignored it would send the file from the start
// and the two would be joined.
func TestCanResumeLooksThroughWrappers(t *testing.T) {
	yes := &resumingSource{can: true}
	for name, c := range map[string]struct {
		source MediaDownloader
		want   bool
	}{
		"direct":          {yes, true},
		"wrapped":         {wrapperSource{yes}, true},
		"wrapped twice":   {wrapperSource{wrapperSource{yes}}, true},
		"says no":         {wrapperSource{&resumingSource{can: false}}, false},
		"plain":           {&plainSource{}, false},
		"wrapped plain":   {wrapperSource{&plainSource{}}, false},
		"nothing":         {nil, false},
		"wrapped nothing": {wrapperSource{nil}, false},
	} {
		if got := (AttachmentClient{Source: c.source}).CanResume(); got != c.want {
			t.Fatalf("%s: %v", name, got)
		}
	}
	id := MediaIdentity{Kind: "video", ID: "1", Size: 9 << 20}
	client := AttachmentClient{Source: wrapperSource{yes}, Media: Attachment{Identity: id}}
	if err := client.DownloadFrom(context.Background(), id, 4<<20, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := downloadOffsetOf(yes.gotCtx); got != 4<<20 {
		t.Fatalf("the source saw offset %d", got)
	}
	if err := client.DownloadFrom(context.Background(), MediaIdentity{Kind: "video", ID: "2", Size: 9 << 20}, 4<<20, io.Discard); err == nil {
		t.Fatal("another file's offset was accepted")
	}
	if !(&GotdClient{}).ResumesDownloads() != verifyChunks {
		t.Fatal("per-chunk verification must rule resume out")
	}
}
