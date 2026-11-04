package download

import (
	"context"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// Every way a message is found to share a saved file says which one it was,
// and a duplicate found only after downloading never keeps its bytes.
func TestLibraryNamesHowEachDuplicateWasFound(t *testing.T) {
	for _, skip := range []bool{false, true} {
		dir := t.TempDir()
		library, db := openTestLibrary(t, dir)
		var calls atomic.Int64
		client := clientFunc(func(_ context.Context, _ telegram.MediaIdentity, w io.Writer) error {
			calls.Add(1)
			_, err := w.Write([]byte("invented clip bytes"))
			return err
		})
		item := func(group string, msg int64, id string) Item {
			return Item{GroupID: group, MessageID: msg, Name: "clip.mp4", Type: "video", Identity: telegram.MediaIdentity{Kind: "document", ID: id, Size: 19}, SkipDuplicates: skip}
		}
		first, err := library.Ingest(context.Background(), item("-1001", 1, "501"), client)
		if err != nil || first.Reused || first.Dedup != "" {
			t.Fatalf("skip=%v first: %+v %v", skip, first, err)
		}

		// A forward of the same Telegram file: nothing downloaded.
		fwd, err := library.Ingest(context.Background(), item("-1002", 7, "501"), client)
		if err != nil || !fwd.Reused || fwd.Dedup != DedupTelegramFile || fwd.Path != first.Path || calls.Load() != 1 {
			t.Fatalf("skip=%v forward: %+v %v calls=%d", skip, fwd, err, calls.Load())
		}

		// A new upload of the same bytes, confirmed before downloading.
		var asked atomic.Int64
		library.Substitute = func(_ context.Context, it Item, _ Client) (string, string, bool) {
			asked.Add(1)
			if it.Identity.ID == "502" {
				return first.Path, "telegram-hashes", true
			}
			return "", "", false
		}
		sub, err := library.Ingest(context.Background(), item("-1001", 2, "502"), client)
		if err != nil || !sub.Reused || sub.Dedup != DedupReupload || sub.DedupHow != "telegram-hashes" || sub.Path != first.Path || calls.Load() != 1 {
			t.Fatalf("skip=%v re-upload: %+v %v calls=%d", skip, sub, err, calls.Load())
		}

		// The same bytes, found only once downloaded: the downloaded copy
		// is removed and the catalogue points at the saved file.
		after, err := library.Ingest(context.Background(), item("-1001", 3, "503"), client)
		if err != nil || !after.Reused || after.Dedup != DedupAfterDownload || after.Path != first.Path || calls.Load() != 2 || asked.Load() != 2 {
			t.Fatalf("skip=%v after download: %+v %v calls=%d asked=%d", skip, after, err, calls.Load(), asked.Load())
		}
		files, _ := filepath.Glob(filepath.Join(dir, "downloads", "*", "*", "*"))
		if len(files) != 1 {
			t.Fatalf("skip=%v files on disk: %v", skip, files)
		}
		var paths, pending int
		if err := db.Reader.QueryRow(`SELECT COUNT(DISTINCT file_path) FROM downloads`).Scan(&paths); err != nil || paths != 1 {
			t.Fatalf("skip=%v catalogue paths=%d %v", skip, paths, err)
		}
		if err := db.Reader.QueryRow(`SELECT COUNT(*) FROM tgdl_ingest_files`).Scan(&pending); err != nil || pending != 0 {
			t.Fatalf("skip=%v ingest journal=%d %v", skip, pending, err)
		}

		// A new file is not a duplicate.
		fresh, err := library.Ingest(context.Background(), Item{GroupID: "-1001", MessageID: 4, Name: "other.mp4", Type: "video", Identity: telegram.MediaIdentity{Kind: "document", ID: "504", Size: 4}, SkipDuplicates: skip}, fakeClient{data: []byte("new!")})
		if err != nil || fresh.Reused || fresh.Dedup != "" {
			t.Fatalf("skip=%v fresh: %+v %v", skip, fresh, err)
		}
	}
}
