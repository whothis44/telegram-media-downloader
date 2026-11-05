package download

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/store"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// ix.12.3: checkpoints by time as well as by size, stopping right after a
// checkpoint when asked (an update, a pause), and resume in the staging folder.

func withStopNearEnd(t *testing.T, n int64) {
	t.Helper()
	old := partStopNearEnd
	partStopNearEnd = n
	t.Cleanup(func() { partStopNearEnd = old })
}

// The defaults the release ships with: 8 MiB, every 8 MiB or 30 s.
func TestResumeDefaults(t *testing.T) {
	if resumeMinSize != 8<<20 || partCheckpointEvery != 8<<20 || partCheckpointAfter != 30*time.Second || partStopNearEnd != 8<<20 {
		t.Fatalf("min %d, every %d, after %v, near the end %d", resumeMinSize, partCheckpointEvery, partCheckpointAfter, partStopNearEnd)
	}
}

// Under a speed limit a file gets few bytes per minute: the clock checkpoints
// it even when the size never comes round.
func TestResumeCheckpointsByTime(t *testing.T) {
	withCheckpoints(t)
	oldEvery, oldAfter := partCheckpointEvery, partCheckpointAfter
	partCheckpointEvery, partCheckpointAfter = 1<<40, time.Nanosecond
	t.Cleanup(func() { partCheckpointEvery, partCheckpointAfter = oldEvery, oldAfter })
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	if _, err := library.Ingest(context.Background(), resumeItem(resumeTestSize), &resumeClient{data: data, can: true, stopAfter: 3}); err == nil {
		t.Fatal("the attempt should have failed")
	}
	if offset, _ := resumeProgress(t, db); offset != 3<<20 {
		t.Fatalf("progress at %d, want every part boundary (3 MiB)", offset)
	}
	second := &resumeClient{data: data, can: true}
	rec, err := library.Ingest(context.Background(), resumeItem(resumeTestSize), second)
	if err != nil {
		t.Fatal(err)
	}
	if second.starts[0] != 3<<20 {
		t.Fatalf("started at %d", second.starts[0])
	}
	checkResumed(t, dir, db, rec, data)
}

// Asked to stop, a transfer records a checkpoint at the next part boundary and
// ends there with ErrDownloadStopped: not a failure, and nothing lost.
func TestResumeStopsRightAfterACheckpoint(t *testing.T) {
	withCheckpoints(t)
	withStopNearEnd(t, 2<<20)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	var asked atomic.Int64
	ctx := telegram.WithCheckpointStop(context.Background(), func() bool { return asked.Add(1) >= 3 })
	first := &resumeClient{data: data, can: true}
	_, err := library.Ingest(ctx, resumeItem(resumeTestSize), first)
	if !errors.Is(err, telegram.ErrDownloadStopped) {
		t.Fatalf("got %v, want ErrDownloadStopped", err)
	}
	// Three parts: one checkpoint by size at 2 MiB, one at 3 MiB to stop.
	offset, path := resumeProgress(t, db)
	if offset != 3<<20 {
		t.Fatalf("stopped with progress at %d, want 3 MiB", offset)
	}
	// The client counts a part once its write returns: the third, which
	// stopped it, is in the file but not counted.
	if first.sentParts.Load() != 2 {
		t.Fatalf("went on past the stop: %d parts", first.sentParts.Load())
	}
	if info, err := os.Stat(resumePartFile(dir, path)); err != nil || info.Size() != 3<<20 {
		t.Fatalf("the part: %v %v", info, err)
	}
	second := &resumeClient{data: data, can: true}
	rec, err := library.Ingest(context.Background(), resumeItem(resumeTestSize), second)
	if err != nil {
		t.Fatal(err)
	}
	if second.starts[0] != 3<<20 || second.sentParts.Load() != 7 {
		t.Fatalf("second attempt started at %d, fetched %d parts", second.starts[0], second.sentParts.Load())
	}
	checkResumed(t, dir, db, rec, data)
}

// Nearly finished, a file asked to stop finishes instead.
func TestResumeStopNearTheEndFinishes(t *testing.T) {
	withCheckpoints(t)
	withStopNearEnd(t, 4<<20)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	var stop atomic.Bool
	client := &resumeClient{data: data, can: true, onPart: func(sent int) {
		if sent == 7 {
			stop.Store(true) // 3 MiB left
		}
	}}
	ctx := telegram.WithCheckpointStop(context.Background(), stop.Load)
	rec, err := library.Ingest(ctx, resumeItem(resumeTestSize), client)
	if err != nil {
		t.Fatalf("a nearly finished file should finish: %v", err)
	}
	checkResumed(t, dir, db, rec, data)
}

// A file too small to keep its progress is never stopped: it finishes.
func TestResumeStopLeavesSmallFilesAlone(t *testing.T) {
	withCheckpoints(t)
	withStopNearEnd(t, 0)
	dir := t.TempDir()
	library, _ := openTestLibrary(t, dir)
	data := resumeBytes(3 << 20) // under resumeMinSize (4 MiB here)
	item := resumeItem(3 << 20)
	ctx := telegram.WithCheckpointStop(context.Background(), func() bool { return true })
	if _, err := library.Ingest(ctx, item, &resumeClient{data: data, can: true}); err != nil {
		t.Fatalf("a small file was stopped: %v", err)
	}
}

// When the record cannot be written, the transfer does not stop (it would lose
// everything since the last one): it goes on and tries at the next part.
func TestResumeDoesNotStopWithoutARecord(t *testing.T) {
	withCheckpoints(t)
	withStopNearEnd(t, 0)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	// The record's table is gone: every save fails.
	if _, err := db.Writer.Exec(`ALTER TABLE tgdl_part_progress RENAME TO tgdl_part_progress_away`); err != nil {
		t.Fatal(err)
	}
	ctx := telegram.WithCheckpointStop(context.Background(), func() bool { return true })
	client := &resumeClient{data: data, can: true}
	rec, err := library.Ingest(ctx, resumeItem(resumeTestSize), client)
	if err != nil {
		t.Fatalf("stopped without a record: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "downloads", rec.Path))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the file is not the original: %v", err)
	}
}

// ---------- the local staging folder ----------

func stageProgress(t *testing.T, db *store.DB) (offset int64, path, stageDir string) {
	t.Helper()
	if err := db.Reader.QueryRow(`SELECT offset,path,stage_dir FROM tgdl_part_progress`).Scan(&offset, &path, &stageDir); err != nil {
		return -1, "", ""
	}
	return offset, path, stageDir
}

func stagedPart(stageDir, path string) string {
	rel := filepath.FromSlash(path)
	return filepath.Join(stageDir, filepath.Dir(rel), partName(filepath.Base(rel)))
}

func addResumeWork(t *testing.T, db *store.DB, item Item, status string) {
	t.Helper()
	if _, err := db.Writer.Exec(`DELETE FROM tgdl_work`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,created_at,updated_at) VALUES('one',?,?,?,1,?,'video','v',?,X'00',?,1,1)`,
		item.GroupID, item.GroupName, item.MessageID, item.Identity.Key(), item.Identity.Size, status); err != nil {
		t.Fatal(err)
	}
}

// checkMoved: the staged file reached the library exact, with the whole file's
// hash, and nothing partial is left anywhere.
func checkMoved(t *testing.T, dir, stage string, l *Library, db *store.DB, want []byte) {
	t.Helper()
	if next := l.MoveStaged(context.Background()); next != 0 {
		t.Fatalf("the move is not done (next try in %v)", next)
	}
	var path, stored string
	if err := db.Reader.QueryRow(`SELECT file_path,file_hash FROM downloads WHERE telegram_media_id='42'`).Scan(&path, &stored); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "downloads", filepath.FromSlash(path)))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the library's file is not the original: %v", err)
	}
	if offset, _, _ := stageProgress(t, db); offset != -1 {
		t.Fatalf("progress left behind at %d", offset)
	}
	for _, root := range []string{filepath.Join(dir, "downloads"), stage} {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".part") {
				t.Fatalf("a partial file left behind: %s", p)
			}
			return nil
		})
	}
}

func TestStagedResumeCarriesOn(t *testing.T) {
	withCheckpoints(t)
	dir, stage := t.TempDir(), t.TempDir()
	library, db, _ := stagedLibrary(t, dir, stage)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	if _, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: true, stopAfter: 7}); err == nil {
		t.Fatal("the attempt should have failed")
	}
	offset, path, stageDir := stageProgress(t, db)
	if offset != 6<<20 {
		t.Fatalf("progress at %d", offset)
	}
	if want := filepath.Join(stage, StageFolder); stageDir != want {
		t.Fatalf("kept in %q, want %q", stageDir, want)
	}
	if _, err := os.Stat(stagedPart(stageDir, path)); err != nil {
		t.Fatalf("the staged part was not kept: %v", err)
	}
	if files := listFiles(t, filepath.Join(dir, "downloads")); len(files) != 0 {
		t.Fatalf("written to the library directly: %v", files)
	}
	second := &resumeClient{data: data, can: true}
	rec, err := library.Ingest(context.Background(), item, second)
	if err != nil || !rec.Staged {
		t.Fatalf("%+v %v", rec, err)
	}
	if second.starts[0] != 6<<20 || second.sentParts.Load() != 4 {
		t.Fatalf("second attempt started at %d, fetched %d parts", second.starts[0], second.sentParts.Load())
	}
	checkMoved(t, dir, stage, library, db, data)
}

// Stopping at a checkpoint works in the staging folder too.
func TestStagedResumeStopsAtACheckpoint(t *testing.T) {
	withCheckpoints(t)
	withStopNearEnd(t, 2<<20)
	dir, stage := t.TempDir(), t.TempDir()
	library, db, _ := stagedLibrary(t, dir, stage)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	var asked atomic.Int64
	ctx := telegram.WithCheckpointStop(context.Background(), func() bool { return asked.Add(1) >= 5 })
	if _, err := library.Ingest(ctx, item, &resumeClient{data: data, can: true}); !errors.Is(err, telegram.ErrDownloadStopped) {
		t.Fatalf("got %v", err)
	}
	if offset, _, _ := stageProgress(t, db); offset != 5<<20 {
		t.Fatalf("progress at %d", offset)
	}
	if stageRows(t, db) != 0 {
		t.Fatal("a stopped download was journaled to be moved")
	}
	second := &resumeClient{data: data, can: true}
	if _, err := library.Ingest(context.Background(), item, second); err != nil {
		t.Fatal(err)
	}
	if second.starts[0] != 5<<20 {
		t.Fatalf("started at %d", second.starts[0])
	}
	checkMoved(t, dir, stage, library, db, data)
}

// Recovery keeps a staged part its queue item still wants, and removes one
// nothing is coming back for, with its record.
func TestStagedRecoverKeepsWhatIsWanted(t *testing.T) {
	withCheckpoints(t)
	dir, stage := t.TempDir(), t.TempDir()
	library, db, _ := stagedLibrary(t, dir, stage)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	if _, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: true, stopAfter: 7}); err == nil {
		t.Fatal("the attempt should have failed")
	}
	_, path, stageDir := stageProgress(t, db)
	part := stagedPart(stageDir, path)
	// A partial file with no record, as a crash before any checkpoint leaves.
	stray := filepath.Join(filepath.Dir(part), partName("other_1_00.mp4"))
	if err := os.WriteFile(stray, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	addResumeWork(t, db, item, "pending")
	if err := library.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(part); err != nil {
		t.Fatalf("recovery removed a staged part the queue still wants: %v", err)
	}
	if _, err := os.Stat(stray); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a staged part with no record stayed: %v", err)
	}
	if offset, _, _ := stageProgress(t, db); offset != 6<<20 {
		t.Fatalf("progress at %d", offset)
	}
	addResumeWork(t, db, item, "skipped")
	if err := library.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(part); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an abandoned staged part stayed: %v", err)
	}
	if offset, _, _ := stageProgress(t, db); offset != -1 {
		t.Fatalf("progress at %d", offset)
	}
}

// A staging folder not there today (a disk unplugged) keeps its records: the
// parts may still be on it.
func TestStagedRecoverKeepsRecordsOfAMissingFolder(t *testing.T) {
	withCheckpoints(t)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	item := resumeItem(resumeTestSize)
	gone := filepath.Join(t.TempDir(), "unplugged", StageFolder)
	if _, err := db.Writer.Exec(`INSERT INTO tgdl_part_progress(group_id,message_id,identity,path,size,offset,sha256,updated_at,stage_dir) VALUES(?,?,?,?,?,?,?,?,?)`,
		item.GroupID, item.MessageID, item.Identity.Key(), "Invented/videos/video_7_00.mp4", item.Identity.Size, 2<<20, "00", 1, gone); err != nil {
		t.Fatal(err)
	}
	addResumeWork(t, db, item, "pending")
	if err := library.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if offset, _, _ := stageProgress(t, db); offset != 2<<20 {
		t.Fatalf("the record of a missing folder went: %d", offset)
	}
}

// Staging turned off: the staged part is not carried on in the library. That
// file starts over, and the staged part goes.
func TestStagingTurnedOffStartsOver(t *testing.T) {
	withCheckpoints(t)
	dir, stage := t.TempDir(), t.TempDir()
	library, db, _ := stagedLibrary(t, dir, stage)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	if _, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: true, stopAfter: 7}); err == nil {
		t.Fatal("the attempt should have failed")
	}
	_, path, stageDir := stageProgress(t, db)
	part := stagedPart(stageDir, path)
	library.Staging = func(context.Context) Staging { return Staging{} }
	second := &resumeClient{data: data, can: true}
	rec, err := library.Ingest(context.Background(), item, second)
	if err != nil || rec.Staged {
		t.Fatalf("%+v %v", rec, err)
	}
	if second.starts[0] != 0 {
		t.Fatalf("carried on a staged part in the library: started at %d", second.starts[0])
	}
	if _, err := os.Stat(part); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the staged part stayed: %v", err)
	}
	checkResumed(t, dir, db, rec, data)
}

// And the other way: a part kept in the library is not carried on in the
// staging folder.
func TestStagingTurnedOnStartsOver(t *testing.T) {
	withCheckpoints(t)
	dir, stage := t.TempDir(), t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	if _, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: true, stopAfter: 7}); err == nil {
		t.Fatal("the attempt should have failed")
	}
	_, path := resumeProgress(t, db)
	part := resumePartFile(dir, path)
	library.Staging = func(context.Context) Staging { return Staging{Dir: stage} }
	second := &resumeClient{data: data, can: true}
	if _, err := library.Ingest(context.Background(), item, second); err != nil {
		t.Fatal(err)
	}
	if second.starts[0] != 0 {
		t.Fatalf("started at %d", second.starts[0])
	}
	if _, err := os.Stat(part); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the library's part stayed: %v", err)
	}
	checkMoved(t, dir, stage, library, db, data)
}

// A real process killed while downloading to the staging folder, as for the
// library (TestResumeAfterARealCrash).
func TestStagedResumeAfterARealCrash(t *testing.T) {
	data := resumeBytes(resumeTestSize)
	for _, c := range []struct {
		name  string
		how   string
		part  int
		start int64
	}{
		{"mid-data, past a checkpoint", "part", 7, 6 << 20},
		{"mid-data, before any checkpoint", "part", 1, 0},
		{"bytes synced, record not written", "before-record", 2, 2 << 20},
	} {
		t.Run(c.name, func(t *testing.T) {
			withCheckpoints(t)
			dir, stage := t.TempDir(), t.TempDir()
			runResumeCrashIn(t, dir, stage, c.how, c.part)
			library, db, _ := stagedLibrary(t, dir, stage)
			if err := library.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			second := &resumeClient{data: data, can: true}
			rec, err := library.Ingest(context.Background(), resumeItem(resumeTestSize), second)
			if err != nil || !rec.Staged {
				t.Fatalf("%+v %v", rec, err)
			}
			if second.starts[0] != c.start {
				t.Fatalf("started at %d, want %d", second.starts[0], c.start)
			}
			if want := (int64(resumeTestSize) - c.start) >> 20; second.sentParts.Load() != want {
				t.Fatalf("fetched %d parts, want %d", second.sentParts.Load(), want)
			}
			checkMoved(t, dir, stage, library, db, data)
		})
	}
}

// Dying after the staged file is published but before its record is cleared.
func TestStagedResumeCrashAfterPublishing(t *testing.T) {
	withCheckpoints(t)
	dir, stage := t.TempDir(), t.TempDir()
	runResumeCrashIn(t, dir, stage, "published", 0)
	library, db, _ := stagedLibrary(t, dir, stage)
	if err := library.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if offset, _, _ := stageProgress(t, db); offset != -1 {
		t.Fatalf("a record stayed for a published file: %d", offset)
	}
	checkMoved(t, dir, stage, library, db, resumeBytes(resumeTestSize))
}
