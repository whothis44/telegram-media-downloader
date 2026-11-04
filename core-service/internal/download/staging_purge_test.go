package download

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/store"
)

func purgeStagedTx(t *testing.T, db *store.DB, group string, all bool) []string {
	t.Helper()
	tx, err := db.Writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	files, err := PurgeStaged(context.Background(), tx, group, all)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return files
}

func countRows(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Reader.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A purge removed the group's downloads (no generations row is left) while
// the mover still had its staged file in hand: the file is neither
// catalogued nor, when it is missing, downloaded again.
func TestStagedFileOfAPurgedGroupIsNeitherCataloguedNorQueuedAgain(t *testing.T) {
	ctx := context.Background()
	for _, missing := range []bool{false, true} {
		name := "file present"
		if missing {
			name = "file missing"
		}
		t.Run(name, func(t *testing.T) {
			dir, stage := t.TempDir(), t.TempDir()
			l, db, arrived := stagedLibrary(t, dir, stage)
			lost := 0
			l.StageLost = func(context.Context, Item) error { lost++; return nil }
			content := []byte("purged while staged")
			item := stagedItem(1, "p", len(content))
			item.Requeue = []byte("queue item")
			var calls atomic.Int64
			rec, err := l.Ingest(ctx, item, bytesClient(&calls, content))
			if err != nil || !rec.Staged {
				t.Fatalf("ingest: %+v %v", rec, err)
			}
			if _, err := db.Writer.Exec(`DELETE FROM tgdl_message_generations`); err != nil {
				t.Fatal(err)
			}
			local := filepath.Join(stage, StageFolder, filepath.FromSlash(rec.Path))
			if missing {
				if err := os.Remove(local); err != nil {
					t.Fatal(err)
				}
			}
			l.MoveStaged(ctx)
			if lost != 0 {
				t.Fatal("a purged message was queued again")
			}
			if n := countRows(t, db, `SELECT COUNT(*) FROM downloads`); n != 0 || len(*arrived) != 0 {
				t.Fatalf("a purged message was catalogued: %d %d", n, len(*arrived))
			}
			if stageRows(t, db) != 0 || len(listFiles(t, filepath.Join(stage, StageFolder))) != 0 || len(listFiles(t, filepath.Join(dir, "downloads"))) != 0 {
				t.Fatalf("left behind: rows %d staging %v library %v", stageRows(t, db), listFiles(t, filepath.Join(stage, StageFolder)), listFiles(t, filepath.Join(dir, "downloads")))
			}
		})
	}
}

// PurgeStaged drops the purged group's staged files and followers; a file
// another group's message follows is handed to that message.
func TestPurgeStagedDropsTheGroupsFilesAndHandsOnSharedOnes(t *testing.T) {
	ctx := context.Background()
	dir, stage := t.TempDir(), t.TempDir()
	l, db, arrived := stagedLibrary(t, dir, stage)
	shared, own := []byte("shared between groups"), []byte("only the purged group")
	var calls atomic.Int64
	leader, err := l.Ingest(ctx, stagedItem(1, "s", len(shared)), bytesClient(&calls, shared))
	if err != nil {
		t.Fatal(err)
	}
	other := stagedItem(5, "s", len(shared))
	other.GroupID, other.GroupName = "2", "Two"
	if rec, err := l.Ingest(ctx, other, bytesClient(&calls, shared)); err != nil || !rec.Reused || calls.Load() != 1 {
		t.Fatalf("follower: %+v %v", rec, err)
	}
	sameGroup := stagedItem(3, "s", len(shared))
	if _, err := l.Ingest(ctx, sameGroup, bytesClient(&calls, shared)); err != nil {
		t.Fatal(err)
	}
	ownRec, err := l.Ingest(ctx, stagedItem(2, "o", len(own)), bytesClient(&calls, own))
	if err != nil {
		t.Fatal(err)
	}
	// A mover pass read the journal before the purge.
	stale := loadMoves(t, db)
	files := purgeStagedTx(t, db, "1", false)
	if _, err := db.Writer.Exec(`DELETE FROM tgdl_message_generations WHERE group_id='1'`); err != nil {
		t.Fatal(err)
	}
	ownLocal := filepath.Join(stage, StageFolder, filepath.FromSlash(ownRec.Path))
	if len(files) != 1 || files[0] != ownLocal {
		t.Fatalf("files to remove: %v", files)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM tgdl_stage_moves WHERE path=? AND json_extract(item,'$.GroupID')='2' AND json_extract(item,'$.MessageID')=5`, leader.Path); n != 1 || stageRows(t, db) != 1 {
		t.Fatalf("the shared file was not handed to the other group: %d of %d", n, stageRows(t, db))
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM tgdl_stage_followers`); n != 0 {
		t.Fatalf("followers left: %d", n)
	}
	// The stale pass neither drops the handed-on file nor queues anything.
	for _, mv := range stale {
		err := l.moveStaged(ctx, mv)
		switch mv.path {
		case leader.Path:
			if !errors.Is(err, errStageLater) {
				t.Fatalf("stale journal entry: %v", err)
			}
		default:
			if !errors.Is(err, errStageGone) {
				t.Fatalf("purged journal entry: %v", err)
			}
		}
	}
	if err := l.RemoveStagedFiles(files); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ownLocal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("purged staged file kept: %v", err)
	}
	moveDueNow(t, db)
	l.MoveStaged(ctx)
	var path string
	if err := db.Reader.QueryRow(`SELECT file_path FROM downloads WHERE group_id='2' AND message_id=5`).Scan(&path); err != nil {
		t.Fatalf("the other group's message was not catalogued: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "downloads", filepath.FromSlash(path))); err != nil || !bytes.Equal(data, shared) {
		t.Fatalf("catalogued file: %q %v", data, err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM downloads WHERE group_id='1'`); n != 0 || len(*arrived) != 1 || stageRows(t, db) != 0 {
		t.Fatalf("purged group catalogued %d, arrived %d, rows %d", n, len(*arrived), stageRows(t, db))
	}

	// Purging everything empties the journal.
	if _, err := l.Ingest(ctx, stagedItem(9, "z", 3), bytesClient(&calls, []byte("abc"))); err != nil {
		t.Fatal(err)
	}
	files = purgeStagedTx(t, db, "", true)
	if len(files) != 1 || stageRows(t, db) != 0 {
		t.Fatalf("purge all: %v, rows %d", files, stageRows(t, db))
	}
}

// A recovered group whose ID changes takes its staged files along.
func TestRekeyStagedFollowsANewGroupID(t *testing.T) {
	ctx := context.Background()
	dir, stage := t.TempDir(), t.TempDir()
	l, db, _ := stagedLibrary(t, dir, stage)
	content := []byte("recovered group")
	var calls atomic.Int64
	if _, err := l.Ingest(ctx, stagedItem(4, "r", len(content)), bytesClient(&calls, content)); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE tgdl_message_generations SET group_id='9' WHERE group_id='1'`); err != nil {
		t.Fatal(err)
	}
	if err := RekeyStaged(ctx, tx, "1", "9", "Nine"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	l.MoveStaged(ctx)
	if n := countRows(t, db, `SELECT COUNT(*) FROM downloads WHERE group_id='9' AND message_id=4`); n != 1 || stageRows(t, db) != 0 {
		t.Fatalf("catalogued %d, rows %d", n, stageRows(t, db))
	}
}

func loadMoves(t *testing.T, db *store.DB) []stagedMove {
	t.Helper()
	rows, err := db.Reader.Query(`SELECT path,stage_dir,item,generation,sha256,size,attempts,identity FROM tgdl_stage_moves ORDER BY path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []stagedMove
	for rows.Next() {
		var mv stagedMove
		if err := rows.Scan(&mv.path, &mv.stageDir, &mv.payload, &mv.generation, &mv.sum, &mv.size, &mv.attempts, &mv.identity); err != nil {
			t.Fatal(err)
		}
		out = append(out, mv)
	}
	return out
}
