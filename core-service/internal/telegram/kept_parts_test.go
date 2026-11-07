package telegram

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"slices"
	"testing"

	"github.com/gotd/td/bin"
	gotd "github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// keptOf keeps the given parts of data, as a check would have (ix.12.11).
func keptOf(data []byte, size int64, indexes ...int) map[int][]byte {
	out := map[int][]byte{}
	for _, i := range indexes {
		start := int64(i) * downloadPartSize
		end := min(start+downloadPartSize, size, int64(len(data)))
		out[i] = bytes.Clone(data[start:end])
	}
	return out
}

func keptFunc(m map[int][]byte) KeptParts {
	return func(index int) ([]byte, bool) {
		p, ok := m[index]
		return p, ok
	}
}

func askedParts(f *fakeParts) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for _, off := range f.asked {
		out = append(out, int(off/downloadPartSize))
	}
	return out
}

// A download with kept parts at the start, the middle and the end equals a
// plain download byte for byte, and Telegram is never asked for a kept part.
func TestKeptPartsAreTakenAndNeverAskedFor(t *testing.T) {
	data := make([]byte, 9<<20+4321)
	rand.New(rand.NewSource(11)).Read(data)
	size := int64(len(data))
	for _, threads := range []int{1, 4} {
		for _, start := range []int64{0, 3 << 20, 9 << 20} {
			kept := keptOf(data, size, 0, 1, 4, 5, 9)
			parts := &fakeParts{data: data}
			before, _ := KeptPartStats()
			var out bytes.Buffer
			if err := fetchParallelKept(context.Background(), parts, &tg.InputDocumentFileLocation{}, size, start, threads, keptFunc(kept), &out); err != nil {
				t.Fatalf("threads %d start %d: %v", threads, start, err)
			}
			if !bytes.Equal(out.Bytes(), data[start:]) {
				t.Fatalf("threads %d start %d: %d bytes, not the file from the offset", threads, start, out.Len())
			}
			var reused int64
			for i, p := range kept {
				if int64(i)*downloadPartSize >= start {
					reused += int64(len(p))
				}
			}
			after, _ := KeptPartStats()
			if after-before != reused {
				t.Fatalf("threads %d start %d: %d bytes counted reused, want %d", threads, start, after-before, reused)
			}
			for _, i := range askedParts(parts) {
				if _, ok := kept[i]; ok {
					t.Fatalf("threads %d start %d: Telegram asked for kept part %d", threads, start, i)
				}
				if int64(i)*downloadPartSize < start {
					t.Fatalf("threads %d start %d: asked for part %d before the offset", threads, start, i)
				}
			}
		}
	}
}

// A kept part with wrong bytes is the app's to refuse (its SHA-256); one of
// the wrong length is refused here: both are asked of Telegram, and the file
// is still exact.
func TestKeptPartsOfTheWrongLengthAreFetched(t *testing.T) {
	data := make([]byte, 6<<20+99)
	rand.New(rand.NewSource(12)).Read(data)
	size := int64(len(data))
	for _, threads := range []int{1, 4} {
		kept := keptOf(data, size, 1, 3, 6)
		kept[1] = kept[1][:len(kept[1])-1]        // short
		kept[3] = append(bytes.Clone(kept[3]), 0) // long
		kept[6] = append(bytes.Clone(kept[6]), 1) // the last part, too long
		kept[2] = bytes.Repeat([]byte{7}, 100)    // short in the middle: must not end the file
		parts := &fakeParts{data: data}
		_, refusedBefore := KeptPartStats()
		var out bytes.Buffer
		if err := fetchParallelKept(context.Background(), parts, &tg.InputDocumentFileLocation{}, size, 0, threads, keptFunc(kept), &out); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out.Bytes(), data) {
			t.Fatalf("threads %d: the file differs from a plain download (%d bytes of %d)", threads, out.Len(), len(data))
		}
		asked := askedParts(parts)
		for _, i := range []int{1, 2, 3, 6} {
			if !slices.Contains(asked, i) {
				t.Fatalf("threads %d: part %d of the wrong length was not fetched: %v", threads, i, asked)
			}
		}
		if _, refused := KeptPartStats(); refused-refusedBefore != 4 {
			t.Fatalf("threads %d: %d refused, want 4", threads, refused-refusedBefore)
		}
	}
}

// A file Telegram reported too large: the kept parts (which a check took only
// with the length the reported size gives) are taken, the rest is read until
// Telegram's short part, and nothing is padded.
func TestKeptPartsWithAWrongReportedSize(t *testing.T) {
	data := make([]byte, 3<<20+10)
	rand.New(rand.NewSource(13)).Read(data)
	reported := int64(5 << 20)
	kept := keptOf(data, reported, 0, 1)
	for _, threads := range []int{1, 4} {
		parts := &fakeParts{data: data}
		var out bytes.Buffer
		if err := fetchParallelKept(context.Background(), parts, &tg.InputDocumentFileLocation{}, reported, 0, threads, keptFunc(kept), &out); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out.Bytes(), data) {
			t.Fatalf("threads %d: %d bytes, want the file's %d", threads, out.Len(), len(data))
		}
	}
}

// Through downloadMedia: the identity finds the kept parts, with one thread
// too (which otherwise uses the library's reader), and not with per-chunk
// verification.
func TestDownloadMediaTakesKeptPartsByIdentity(t *testing.T) {
	data := make([]byte, 4<<20+77)
	rand.New(rand.NewSource(14)).Read(data)
	size := int64(len(data))
	id := MediaIdentity{Kind: "document", ID: "4242", Size: size}
	kept := keptOf(data, size, 0, 2)
	var looked []MediaIdentity
	SetKeptParts(func(got MediaIdentity) KeptParts {
		looked = append(looked, got)
		if got.Key() != id.Key() {
			return nil
		}
		return keptFunc(kept)
	})
	defer SetKeptParts(nil)
	type use struct {
		id    MediaIdentity
		bytes int64
	}
	var used []use
	SetKeptPartsUsed(func(got MediaIdentity, n int64) { used = append(used, use{got, n}) })
	defer SetKeptPartsUsed(nil)
	for _, threads := range []int{1, 4} {
		used = nil
		pool := &keptTestPool{parts: fakeParts{data: data}}
		c := &downloadTestClient{}
		client := keptTestClient{downloadTestClient: c, pool: pool}
		var out bytes.Buffer
		a := Attachment{Identity: id, DC: 5, Location: &tg.InputDocumentFileLocation{ID: 4242}}
		if err := downloadMedia(context.Background(), client, a, &out, false, threads); err != nil {
			t.Fatalf("threads %d: %v", threads, err)
		}
		if !bytes.Equal(out.Bytes(), data) {
			t.Fatalf("threads %d: the file differs", threads)
		}
		for _, i := range askedParts(&pool.parts) {
			if i == 0 || i == 2 {
				t.Fatalf("threads %d: asked Telegram for kept part %d", threads, i)
			}
		}
		// Another identity of the same size takes nothing.
		pool = &keptTestPool{parts: fakeParts{data: data}}
		client.pool = pool
		other := a
		other.Identity.ID = "4343"
		out.Reset()
		if err := downloadMedia(context.Background(), client, other, &out, false, threads); err != nil || !bytes.Equal(out.Bytes(), data) {
			t.Fatalf("threads %d: other file: %v", threads, err)
		}
		if threads > 1 && len(askedParts(&pool.parts)) != 5 {
			t.Fatalf("threads %d: another file took kept parts: asked %v", threads, askedParts(&pool.parts))
		}
		// ix.12.12: one report for the file that took kept parts, with its bytes.
		if len(used) != 1 || used[0].id.Key() != id.Key() || used[0].bytes != 2*downloadPartSize {
			t.Fatalf("threads %d: reported %+v", threads, used)
		}
	}
	if len(looked) == 0 {
		t.Fatal("the kept parts were never looked up")
	}
}

type keptTestClient struct {
	*downloadTestClient
	pool *keptTestPool
}

func (c keptTestClient) Pool(int64) (gotd.CloseInvoker, error) { return c.pool, nil }

type keptTestPool struct{ parts fakeParts }

func (p *keptTestPool) Close() error { return nil }
func (p *keptTestPool) Invoke(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
	req, ok := in.(*tg.UploadGetFileRequest)
	if !ok {
		return errors.New("unexpected RPC")
	}
	res, err := p.parts.UploadGetFile(ctx, req)
	if err != nil {
		return err
	}
	out.(*tg.UploadFileBox).File = res
	return nil
}
