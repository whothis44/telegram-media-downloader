package download

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// ix.12.6: a copy of a queued file already in the library under its own name
// (moved there by hand) is catalogued when Adopt confirms its bytes, instead
// of downloaded again; one Adopt refuses is left alone and the file
// downloads.
func TestAHandMovedCopyIsAdoptedNotDownloadedAgain(t *testing.T) {
	for _, accept := range []bool{true, false} {
		dir := t.TempDir()
		l, _ := openTestLibrary(t, dir)
		item := stagedItem(7, "70", 20)
		folder, stem, ext := destinationParts(item)
		name := stem + strings.Repeat("ab", 16) + ext
		content := bytes.Repeat([]byte("m"), 20)
		if err := os.MkdirAll(filepath.Join(dir, "downloads", folder), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "downloads", folder, name), content, 0600); err != nil {
			t.Fatal(err)
		}
		// A file of another message and one of another size are never candidates.
		_ = os.WriteFile(filepath.Join(dir, "downloads", folder, strings.Replace(stem, "_7_", "_8_", 1)+strings.Repeat("cd", 16)+ext), content, 0600)
		var asked []string
		l.Adopt = func(_ context.Context, _ Item, path string, _ Client) bool {
			asked = append(asked, path)
			return accept
		}
		var calls atomic.Int64
		rec, err := l.Ingest(context.Background(), item, bytesClient(&calls, content))
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.ToSlash(filepath.Join(folder, name))
		if len(asked) != 1 || asked[0] != want {
			t.Fatalf("accept %v: asked about %v, want only %s", accept, asked, want)
		}
		if accept && (calls.Load() != 0 || rec.Path != want) {
			t.Fatalf("not adopted: downloads %d, path %s", calls.Load(), rec.Path)
		}
		if !accept && (calls.Load() != 1 || rec.Path == want) {
			t.Fatalf("a refused copy was used: downloads %d, path %s", calls.Load(), rec.Path)
		}
	}
}

// A staged file moved to the library by hand, at its own path, is
// catalogued by the mover (size and SHA-256 from the journal), not counted as
// lost and queued again.
func TestAStagedFileMovedByHandIsCatalogued(t *testing.T) {
	dir, stage := t.TempDir(), t.TempDir()
	l, db, arrived := stagedLibrary(t, dir, stage)
	var lost atomic.Int64
	l.StageLost = func(context.Context, Item) error { lost.Add(1); return nil }
	content := []byte("staged then moved by hand")
	var calls atomic.Int64
	rec, err := l.Ingest(context.Background(), stagedItem(1, "9", len(content)), bytesClient(&calls, content))
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(stage, StageFolder, filepath.FromSlash(rec.Path))
	target := filepath.Join(dir, "downloads", filepath.FromSlash(rec.Path))
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(local, target); err != nil {
		t.Fatal(err)
	}
	l.MoveStaged(context.Background())
	if lost.Load() != 0 {
		t.Fatal("counted as lost and queued again")
	}
	if stageRows(t, db) != 0 {
		t.Fatal("the journal entry stayed")
	}
	var n int
	if err := db.Reader.QueryRow(`SELECT COUNT(*) FROM downloads WHERE file_path=? AND status='completed'`, rec.Path).Scan(&n); err != nil || n != 1 {
		t.Fatalf("not catalogued: %d %v", n, err)
	}
	if len(*arrived) != 1 {
		t.Fatalf("arrivals %d", len(*arrived))
	}
}
