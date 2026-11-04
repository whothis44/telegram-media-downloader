package download

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/store"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// stagedLibrary is a library whose staging folder is stage (empty: off).
func stagedLibrary(t *testing.T, dir, stage string) (*Library, *store.DB, *[]Record) {
	t.Helper()
	l, db := openTestLibrary(t, dir)
	l.Staging = func(context.Context) Staging { return Staging{Dir: stage} }
	arrived := &[]Record{}
	l.OnStaged = func(_ context.Context, _ Item, r Record) { *arrived = append(*arrived, r) }
	return l, db, arrived
}

func bytesClient(calls *atomic.Int64, content []byte) Client {
	return clientFunc(func(_ context.Context, _ telegram.MediaIdentity, w io.Writer) error {
		calls.Add(1)
		_, err := w.Write(content)
		return err
	})
}

func stagedItem(msg int64, id string, size int) Item {
	return Item{GroupID: "1", GroupName: "One", MessageID: msg, Name: "clip.mp4", Type: "video", Identity: telegram.MediaIdentity{Kind: "document", ID: id, Size: int64(size)}}
}

// listFiles is every file under dir, relative, sorted.
func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func stageRows(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.Reader.QueryRow(`SELECT COUNT(*) FROM tgdl_stage_moves`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An empty staging setting changes nothing: same files, same bytes, same
// catalogue, no journal, no staging folder.
func TestStagingOffIsExactlyAsBefore(t *testing.T) {
	content := []byte("the same video bytes")
	run := func(withHook bool) ([]string, []byte, []any) {
		dir := t.TempDir()
		l, db := openTestLibrary(t, dir)
		if withHook {
			l.Staging = func(context.Context) Staging { return Staging{} }
			l.OnStaged = func(context.Context, Item, Record) { t.Error("OnStaged called with staging off") }
		}
		var calls atomic.Int64
		rec, err := l.Ingest(context.Background(), stagedItem(1, "42", len(content)), bytesClient(&calls, content))
		if err != nil || rec.Staged || rec.ID == 0 {
			t.Fatalf("ingest: %+v %v", rec, err)
		}
		if withHook {
			var tables int
			_ = db.Reader.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'tgdl_stage%'`).Scan(&tables)
			if tables != 0 {
				t.Fatal("staging journal created with staging off")
			}
			if l.MoveStaged(context.Background()) != 0 {
				t.Fatal("mover found work with staging off")
			}
		}
		files := listFiles(t, filepath.Join(dir, "downloads"))
		data, _ := os.ReadFile(filepath.Join(dir, "downloads", filepath.FromSlash(rec.Path)))
		var row []any
		var name, typ, hash, kind, id string
		var size, tsize int64
		if err := db.Reader.QueryRow(`SELECT file_name,file_size,file_type,file_hash,telegram_media_kind,telegram_media_id,telegram_media_size FROM downloads`).Scan(&name, &size, &typ, &hash, &kind, &id, &tsize); err != nil {
			t.Fatal(err)
		}
		row = append(row, name, size, typ, hash, kind, id, tsize)
		// Only the random part of the file name differs between runs.
		for i, f := range files {
			files[i] = f[:strings.LastIndex(f, "_")]
		}
		return files, data, row
	}
	f1, d1, r1 := run(false)
	f2, d2, r2 := run(true)
	if strings.Join(f1, "|") != strings.Join(f2, "|") || !bytes.Equal(d1, d2) || !bytes.Equal(d1, content) {
		t.Fatalf("files differ: %v %v", f1, f2)
	}
	for i := range r1 {
		if r1[i] != r2[i] {
			t.Fatalf("catalogue differs: %v %v", r1, r2)
		}
	}
	for _, f := range f2 {
		if strings.Contains(f, StageFolder) || strings.HasSuffix(f, ".part") {
			t.Fatalf("stray file %s", f)
		}
	}
}

func TestStagedDownloadReachesTheLibraryAfterChecks(t *testing.T) {
	dir, stage := t.TempDir(), t.TempDir()
	l, db, arrived := stagedLibrary(t, dir, stage)
	content := []byte("staged video bytes")
	var calls atomic.Int64
	rec, err := l.Ingest(context.Background(), stagedItem(1, "42", len(content)), bytesClient(&calls, content))
	if err != nil || !rec.Staged || rec.ID != 0 {
		t.Fatalf("ingest: %+v %v", rec, err)
	}
	local := filepath.Join(stage, StageFolder, filepath.FromSlash(rec.Path))
	if data, err := os.ReadFile(local); err != nil || !bytes.Equal(data, content) {
		t.Fatalf("local copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "downloads", filepath.FromSlash(rec.Path))); !os.IsNotExist(err) {
		t.Fatal("written to the library before the move")
	}
	if st := l.StagingStatus(context.Background()); st.Waiting != 1 || st.Bytes != int64(len(content)) {
		t.Fatalf("status: %+v", st)
	}
	// A second message with the same file follows it: no second download.
	rec2, err := l.Ingest(context.Background(), stagedItem(2, "42", len(content)), bytesClient(&calls, content))
	if err != nil || !rec2.Staged || rec2.Path != rec.Path || calls.Load() != 1 {
		t.Fatalf("follower: %+v %v calls %d", rec2, err, calls.Load())
	}
	l.MoveStaged(context.Background())
	if data, err := os.ReadFile(filepath.Join(dir, "downloads", filepath.FromSlash(rec.Path))); err != nil || !bytes.Equal(data, content) {
		t.Fatalf("library copy: %v", err)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatal("local copy kept after a good move")
	}
	if stageRows(t, db) != 0 || len(*arrived) != 2 || (*arrived)[0].ID == 0 {
		t.Fatalf("after move: rows %d arrived %+v", stageRows(t, db), *arrived)
	}
	var n int
	_ = db.Reader.QueryRow(`SELECT COUNT(*) FROM downloads WHERE file_path=?`, rec.Path).Scan(&n)
	if n != 2 {
		t.Fatalf("catalogue entries: %d", n)
	}
	for _, f := range listFiles(t, filepath.Join(dir, "downloads")) {
		if strings.HasSuffix(f, ".part") {
			t.Fatalf("stray %s", f)
		}
	}
}

func TestStagedMoveRetriesWhileTheLibraryFailsAndKeepsTheLocalFile(t *testing.T) {
	dir, stage := t.TempDir(), t.TempDir()
	l, db, arrived := stagedLibrary(t, dir, stage)
	content := []byte("bytes for a flaky share")
	var calls atomic.Int64
	rec, err := l.Ingest(context.Background(), stagedItem(1, "7", len(content)), bytesClient(&calls, content))
	if err != nil {
		t.Fatal(err)
	}
	// The group folder cannot be made: a file stands in its place.
	blocker := filepath.Join(dir, "downloads", "One")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if next := l.MoveStaged(context.Background()); next <= 0 {
		t.Fatal("no retry planned")
	}
	st := l.StagingStatus(context.Background())
	if st.Waiting != 1 || st.Failing != 1 || st.Stuck != 0 || len(st.Errors) == 0 || len(*arrived) != 0 {
		t.Fatalf("status after a failure: %+v", st)
	}
	local := filepath.Join(stage, StageFolder, filepath.FromSlash(rec.Path))
	if _, err := os.Stat(local); err != nil {
		t.Fatal("local copy lost after a failed move")
	}
	// A restart: the move resumes.
	_ = os.Remove(blocker)
	l2, _ := openTestLibrary(t, dir)
	l2.Staging = l.Staging
	if err := l2.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _ = db.Writer.Exec(`UPDATE tgdl_stage_moves SET next_at=0`)
	l2.MoveStaged(context.Background())
	if data, err := os.ReadFile(filepath.Join(dir, "downloads", filepath.FromSlash(rec.Path))); err != nil || !bytes.Equal(data, content) {
		t.Fatalf("not moved after restart: %v", err)
	}
	if stageRows(t, db) != 0 || calls.Load() != 1 {
		t.Fatal("journal left or downloaded again")
	}
}

func TestStagedMoveNeverReplacesAFileInTheLibrary(t *testing.T) {
	ctx := context.Background()
	for _, same := range []bool{false, true} {
		dir, stage := t.TempDir(), t.TempDir()
		l, db, arrived := stagedLibrary(t, dir, stage)
		content := []byte("new download bytes")
		var calls atomic.Int64
		rec, err := l.Ingest(ctx, stagedItem(1, "9", len(content)), bytesClient(&calls, content))
		if err != nil {
			t.Fatal(err)
		}
		final := filepath.Join(dir, "downloads", filepath.FromSlash(rec.Path))
		existing := []byte("someone else's file")
		if same {
			existing = content
		}
		_ = os.MkdirAll(filepath.Dir(final), 0700)
		if err := os.WriteFile(final, existing, 0600); err != nil {
			t.Fatal(err)
		}
		l.MoveStaged(ctx)
		if data, _ := os.ReadFile(final); !bytes.Equal(data, existing) {
			t.Fatal("an existing file was replaced")
		}
		var path string
		if err := db.Reader.QueryRow(`SELECT file_path FROM downloads WHERE message_id=1`).Scan(&path); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(filepath.Join(dir, "downloads", filepath.FromSlash(path)))
		if !bytes.Equal(data, content) || len(*arrived) != 1 || stageRows(t, db) != 0 {
			t.Fatalf("same=%v: catalogued %s", same, path)
		}
		if same != (path == rec.Path) {
			t.Fatalf("same=%v: path %s, planned %s", same, path, rec.Path)
		}
		if files := listFiles(t, filepath.Join(dir, "downloads")); len(files) != map[bool]int{true: 1, false: 2}[same] {
			t.Fatalf("same=%v: files %v", same, files)
		}
	}
}

func TestStagedFileThatChangedLocallyIsKeptAndReported(t *testing.T) {
	dir, stage := t.TempDir(), t.TempDir()
	l, db, arrived := stagedLibrary(t, dir, stage)
	content := []byte("original bytes!")
	var calls atomic.Int64
	rec, err := l.Ingest(context.Background(), stagedItem(1, "5", len(content)), bytesClient(&calls, content))
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(stage, StageFolder, filepath.FromSlash(rec.Path))
	if err := os.WriteFile(local, []byte("damaged bytes!!"), 0600); err != nil {
		t.Fatal(err)
	}
	l.MoveStaged(context.Background())
	st := l.StagingStatus(context.Background())
	if st.Stuck != 1 || len(*arrived) != 0 || stageRows(t, db) != 1 {
		t.Fatalf("status: %+v", st)
	}
	if _, err := os.Stat(local); err != nil {
		t.Fatal("a doubtful local file was removed")
	}
	if files := listFiles(t, filepath.Join(dir, "downloads")); len(files) != 0 {
		t.Fatalf("something reached the library: %v", files)
	}
}

func TestStagingRoomWaitsForMoves(t *testing.T) {
	dir, stage := t.TempDir(), t.TempDir()
	l, _, _ := stagedLibrary(t, dir, stage)
	l.Staging = func(context.Context) Staging { return Staging{Dir: stage, Cap: 30} }
	noFreeSpaceKnown(t)
	var calls atomic.Int64
	if _, err := l.Ingest(context.Background(), stagedItem(1, "1", 20), bytesClient(&calls, bytes.Repeat([]byte("a"), 20))); err != nil {
		t.Fatal(err)
	}
	// 20 of 30 bytes used: a 5-byte file may start, a 20-byte one waits.
	if err := l.WaitStageRoom(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := l.WaitStageRoom(ctx, 20); err == nil {
		t.Fatal("did not wait for room")
	}
	done := make(chan error, 1)
	go func() { done <- l.WaitStageRoom(context.Background(), 20) }()
	time.Sleep(50 * time.Millisecond)
	l.MoveStaged(context.Background())
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("still waiting after the move")
	}
}

func TestStagingRecoveryFinishesAPublishedPartAndRemovesStrays(t *testing.T) {
	dir, stage := t.TempDir(), t.TempDir()
	l, db, _ := stagedLibrary(t, dir, stage)
	content := []byte("almost published")
	var calls atomic.Int64
	rec, err := l.Ingest(context.Background(), stagedItem(1, "3", len(content)), bytesClient(&calls, content))
	if err != nil {
		t.Fatal(err)
	}
	// Stopped between the journal entry and publication: only the .part is there.
	local := filepath.Join(stage, StageFolder, filepath.FromSlash(rec.Path))
	part := filepath.Join(filepath.Dir(local), partName(filepath.Base(local)))
	if err := os.Rename(local, part); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(filepath.Dir(local), partName("other.mp4"))
	_ = os.WriteFile(stray, []byte("half"), 0600)
	l2, _ := openTestLibrary(t, dir)
	l2.Staging = l.Staging
	if err := l2.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(local); err != nil {
		t.Fatal("the checked .part was not finished")
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("a stray partial download was left")
	}
	l2.MoveStaged(context.Background())
	if stageRows(t, db) != 0 {
		t.Fatal("not moved")
	}
}
