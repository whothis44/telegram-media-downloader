package download

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/store"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// noFreeSpaceKnown makes the staging disk's free space unknown for the test,
// so the space allowed for staging is kept to.
func noFreeSpaceKnown(t *testing.T) {
	t.Helper()
	setFreeSpace(func(string) (int64, bool) { return 0, false })
	t.Cleanup(func() { stageFreeHook.Store(nil) })
}

func setFreeSpace(f func(string) (int64, bool)) { stageFreeHook.Store(&f) }

// blockLibrary makes copies to the library fail (the NAS share is down):
// a file stands where the group's folder goes. It returns the unblock.
func blockLibrary(t *testing.T, dir string, group ...string) func() {
	t.Helper()
	blocker := filepath.Join(dir, "downloads", "One")
	if len(group) > 0 {
		blocker = filepath.Join(dir, "downloads", group[0])
	}
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Remove(blocker); err != nil {
			t.Fatal(err)
		}
	}
}

func failingClient(calls *atomic.Int64) Client {
	return clientFunc(func(context.Context, telegram.MediaIdentity, io.Writer) error {
		calls.Add(1)
		return errors.New("connection reset by peer")
	})
}

func moveDueNow(t *testing.T, db *store.DB) {
	t.Helper()
	if _, err := db.Writer.Exec(`UPDATE tgdl_stage_moves SET next_at=0`); err != nil {
		t.Fatal(err)
	}
}

// catalogued is the bytes of the file the catalogue names for message msg.
func catalogued(t *testing.T, db *store.DB, dir string, msg int64) []byte {
	t.Helper()
	var path string
	if err := db.Reader.QueryRow(`SELECT file_path FROM downloads WHERE group_id='1' AND message_id=?`, msg).Scan(&path); err != nil {
		t.Fatalf("message %d not catalogued: %v", msg, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "downloads", filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("message %d: catalogued file missing: %v", msg, err)
	}
	return data
}

// The reported scenario: the NAS is down, so a lower quality waits in the
// staging folder; the original is then downloaded (an upgrade, or a live
// edit) and that download fails. The staged file is the message's only
// copy: it must reach the library and the catalogue, not be deleted.
func TestStagedFileIsKeptWhenANewerDownloadOfItsMessageFails(t *testing.T) {
	ctx := context.Background()
	dir, stage := t.TempDir(), t.TempDir()
	l, db, arrived := stagedLibrary(t, dir, stage)
	unblock := blockLibrary(t, dir)
	lower := []byte("the lower quality, staged")
	var calls atomic.Int64
	rec, err := l.Ingest(ctx, stagedItem(1, "lq", len(lower)), bytesClient(&calls, lower))
	if err != nil || !rec.Staged {
		t.Fatalf("ingest: %+v %v", rec, err)
	}
	l.MoveStaged(ctx) // fails: the NAS is down
	if _, err := l.Ingest(ctx, stagedItem(1, "orig", 4096), failingClient(&calls)); err == nil {
		t.Fatal("the original's download should fail")
	}
	unblock()
	moveDueNow(t, db)
	l.MoveStaged(ctx)
	if got := catalogued(t, db, dir, 1); !bytes.Equal(got, lower) {
		t.Fatalf("catalogued %q", got)
	}
	local := filepath.Join(stage, StageFolder, filepath.FromSlash(rec.Path))
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatal("local copy kept after a good move")
	}
	if stageRows(t, db) != 0 || len(*arrived) != 1 {
		t.Fatalf("rows %d arrived %d", stageRows(t, db), len(*arrived))
	}
	// The original, downloaded later, still replaces it as usual.
	original := []byte("the original, finally")
	if _, err := l.Ingest(ctx, Item{GroupID: "1", GroupName: "One", MessageID: 1, Name: "clip.mp4", Type: "video", NoStage: true,
		Identity: telegram.MediaIdentity{Kind: "document", ID: "orig", Size: int64(len(original))}}, bytesClient(&calls, original)); err != nil {
		t.Fatal(err)
	}
	if got := catalogued(t, db, dir, 1); !bytes.Equal(got, original) {
		t.Fatalf("after the upgrade: %q", got)
	}
}

// A staged file is dropped once a newer version of its message is in the
// catalogue, and only then.
func TestStagedFileIsDroppedOnceANewerVersionIsCatalogued(t *testing.T) {
	ctx := context.Background()
	dir, stage := t.TempDir(), t.TempDir()
	l, db, arrived := stagedLibrary(t, dir, stage)
	unblock := blockLibrary(t, dir)
	old := []byte("before the edit")
	var calls atomic.Int64
	rec, err := l.Ingest(ctx, stagedItem(1, "a", len(old)), bytesClient(&calls, old))
	if err != nil {
		t.Fatal(err)
	}
	l.MoveStaged(ctx)
	unblock()
	edited := []byte("after the edit, straight to the library")
	item := stagedItem(1, "b", len(edited))
	item.NoStage = true
	if _, err := l.Ingest(ctx, item, bytesClient(&calls, edited)); err != nil {
		t.Fatal(err)
	}
	moveDueNow(t, db)
	l.MoveStaged(ctx)
	if got := catalogued(t, db, dir, 1); !bytes.Equal(got, edited) {
		t.Fatalf("catalogued %q", got)
	}
	if _, err := os.Stat(filepath.Join(stage, StageFolder, filepath.FromSlash(rec.Path))); !os.IsNotExist(err) {
		t.Fatal("the replaced staged file was kept")
	}
	if stageRows(t, db) != 0 || len(*arrived) != 0 || len(listFiles(t, filepath.Join(dir, "downloads"))) != 1 {
		t.Fatalf("rows %d arrived %d files %v", stageRows(t, db), len(*arrived), listFiles(t, filepath.Join(dir, "downloads")))
	}
}

// Two staged versions of one message: the older waits for the newer, which
// goes first; then the older is dropped.
func TestOlderStagedVersionWaitsForTheNewerOne(t *testing.T) {
	ctx := context.Background()
	dir, stage := t.TempDir(), t.TempDir()
	l, db, arrived := stagedLibrary(t, dir, stage)
	unblock := blockLibrary(t, dir)
	old, edited := []byte("first version"), []byte("second version!")
	var calls atomic.Int64
	if _, err := l.Ingest(ctx, stagedItem(1, "a", len(old)), bytesClient(&calls, old)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // created_at orders the moves
	if _, err := l.Ingest(ctx, stagedItem(1, "b", len(edited)), bytesClient(&calls, edited)); err != nil {
		t.Fatal(err)
	}
	unblock()
	l.MoveStaged(ctx)
	if got := catalogued(t, db, dir, 1); !bytes.Equal(got, edited) {
		t.Fatalf("catalogued %q", got)
	}
	if stageRows(t, db) != 1 {
		t.Fatal("the older version should still wait")
	}
	moveDueNow(t, db)
	l.MoveStaged(ctx)
	if got := catalogued(t, db, dir, 1); !bytes.Equal(got, edited) {
		t.Fatalf("catalogued %q after the older one", got)
	}
	if stageRows(t, db) != 0 || len(*arrived) != 1 || len(listFiles(t, filepath.Join(dir, "downloads"))) != 1 {
		t.Fatalf("rows %d arrived %d files %v", stageRows(t, db), len(*arrived), listFiles(t, filepath.Join(dir, "downloads")))
	}
	if files := listFiles(t, filepath.Join(stage, StageFolder)); len(files) != 0 {
		t.Fatalf("left in staging: %v", files)
	}
}

// While a download of the same message runs, the move waits for it (not an
// error), then goes.
func TestStagedMoveWaitsWhileItsMessageDownloads(t *testing.T) {
	ctx := context.Background()
	dir, stage := t.TempDir(), t.TempDir()
	l, db, arrived := stagedLibrary(t, dir, stage)
	content := []byte("waits its turn")
	var calls atomic.Int64
	if _, err := l.Ingest(ctx, stagedItem(1, "w", len(content)), bytesClient(&calls, content)); err != nil {
		t.Fatal(err)
	}
	release, err := l.enter(ctx, messageKey(stagedItem(1, "", 0)))
	if err != nil {
		t.Fatal(err)
	}
	l.MoveStaged(ctx)
	st := l.StagingStatus(ctx)
	if st.Waiting != 1 || st.Failing != 0 || st.Stuck != 0 || len(*arrived) != 0 {
		t.Fatalf("while busy: %+v", st)
	}
	release()
	moveDueNow(t, db)
	l.MoveStaged(ctx)
	if got := catalogued(t, db, dir, 1); !bytes.Equal(got, content) || stageRows(t, db) != 0 {
		t.Fatalf("not moved after the download ended: %q", got)
	}
}

// A staged file that disappeared (an antivirus, someone cleaning the
// folder) is lost: its messages' queue items are done already, so they are
// put back in the queue.
func TestLostStagedFileQueuesItsMessagesAgain(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		requeue bool
		hookErr error
	}{{"requeued", true, nil}, {"cannot be requeued", false, nil}, {"requeue fails", true, errors.New("database is locked")}} {
		t.Run(tc.name, func(t *testing.T) {
			dir, stage := t.TempDir(), t.TempDir()
			l, db, _ := stagedLibrary(t, dir, stage)
			var lost []Item
			l.StageLost = func(_ context.Context, it Item) error {
				if tc.hookErr != nil {
					return tc.hookErr
				}
				lost = append(lost, it)
				return nil
			}
			content := []byte("removed by an antivirus")
			first, second := stagedItem(1, "v", len(content)), stagedItem(2, "v", len(content))
			if tc.requeue {
				first.Requeue, second.Requeue = []byte("queue item 1"), []byte("queue item 2")
			}
			var calls atomic.Int64
			rec, err := l.Ingest(ctx, first, bytesClient(&calls, content))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := l.Ingest(ctx, second, bytesClient(&calls, content)); err != nil || calls.Load() != 1 {
				t.Fatalf("follower: %v", err)
			}
			if err := os.Remove(filepath.Join(stage, StageFolder, filepath.FromSlash(rec.Path))); err != nil {
				t.Fatal(err)
			}
			l.MoveStaged(ctx)
			st := l.StagingStatus(ctx)
			switch {
			case tc.hookErr != nil:
				if st.Waiting != 1 || st.Failing != 1 || st.Stuck != 0 {
					t.Fatalf("a failed requeue is retried: %+v", st)
				}
			case tc.requeue:
				if len(lost) != 2 || string(lost[0].Requeue) != "queue item 1" || string(lost[1].Requeue) != "queue item 2" {
					t.Fatalf("requeued %+v", lost)
				}
				if stageRows(t, db) != 0 || st.Stuck != 0 {
					t.Fatalf("journal left: %+v", st)
				}
			default:
				if len(lost) != 0 || st.Stuck != 1 {
					t.Fatalf("without what is needed to queue it again it needs a person: %+v %d", st, len(lost))
				}
			}
		})
	}
}

// Files that need a person stay on disk but do not count against the space
// allowed; while downloads wait for room, the status says so; and with
// ample free disk space downloads go on past the space allowed.
func TestStuckStagedFilesDoNotStopDownloads(t *testing.T) {
	ctx := context.Background()
	dir, stage := t.TempDir(), t.TempDir()
	l, db, _ := stagedLibrary(t, dir, stage)
	l.Staging = func(context.Context) Staging { return Staging{Dir: stage, Cap: 30} }
	noFreeSpaceKnown(t)
	var calls atomic.Int64
	rec, err := l.Ingest(ctx, stagedItem(1, "s", 20), bytesClient(&calls, bytes.Repeat([]byte("a"), 20)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, StageFolder, filepath.FromSlash(rec.Path)), bytes.Repeat([]byte("b"), 20), 0600); err != nil {
		t.Fatal(err)
	}
	l.MoveStaged(ctx)
	if st := l.StagingStatus(ctx); st.Stuck != 1 || st.StuckBytes != 20 || st.Waiting != 0 || st.Bytes != 0 {
		t.Fatalf("status: %+v", st)
	}
	quick, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := l.WaitStageRoom(quick, 20); err != nil {
		t.Fatalf("a stuck file stopped downloads: %v", err)
	}
	// A waiting file does count: the next download waits, and that shows.
	unblock := blockLibrary(t, dir, "Two")
	defer unblock()
	other := stagedItem(2, "t", 20)
	other.GroupName = "Two"
	if _, err := l.Ingest(ctx, other, bytesClient(&calls, bytes.Repeat([]byte("c"), 20))); err != nil {
		t.Fatal(err)
	}
	l.MoveStaged(ctx)
	waitCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- l.WaitStageRoom(waitCtx, 20) }()
	deadline := time.Now().Add(3 * time.Second)
	for !l.StagingStatus(ctx).Full {
		if time.Now().After(deadline) {
			t.Fatal("waiting for room is not reported")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := l.StagingStatus(ctx); st.FullSince == 0 {
		t.Fatalf("status: %+v", st)
	}
	// Plenty of free space on the staging disk: it goes on.
	setFreeSpace(func(string) (int64, bool) { return 200 << 30, true })
	l.stageSignal()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("still waiting with ample free disk space")
	}
	stop()
	if st := l.StagingStatus(ctx); st.Full || st.FullSince != 0 {
		t.Fatalf("still reported full: %+v", st)
	}
	// Free space that is not ample: it waits again.
	setFreeSpace(func(string) (int64, bool) { return 10 << 30, true })
	quick2, cancel2 := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel2()
	if err := l.WaitStageRoom(quick2, 20); err == nil {
		t.Fatal("went on with little free space")
	}
	_ = db
}
