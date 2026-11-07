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

// ix.12.10: a forum topic's file goes to <group>/<topic>/<type>; a copy moved
// in by hand where such files went before (<group>/<type>) is still adopted.
func TestATopicFileGoesToItsTopicFolder(t *testing.T) {
	item := stagedItem(7, "70", 20)
	plain, _, _ := destinationParts(item)
	item.Topic = "Invented: topic/name"
	dir, _, _ := destinationParts(item)
	group := filepath.Dir(plain)
	if want := filepath.Join(group, "Invented_ topic_name", filepath.Base(plain)); dir != want {
		t.Fatalf("topic folder %q, want %q", dir, want)
	}

	root := t.TempDir()
	l, _ := openTestLibrary(t, root)
	_, stem, ext := destinationParts(item)
	name := stem + strings.Repeat("ab", 16) + ext
	content := bytes.Repeat([]byte("m"), 20)
	if err := os.MkdirAll(filepath.Join(root, "downloads", plain), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "downloads", plain, name), content, 0600); err != nil {
		t.Fatal(err)
	}
	var asked []string
	l.Adopt = func(_ context.Context, _ Item, path string, _ Client) bool {
		asked = append(asked, path)
		return true
	}
	var calls atomic.Int64
	rec, err := l.Ingest(context.Background(), item, bytesClient(&calls, content))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.ToSlash(filepath.Join(plain, name))
	if len(asked) != 1 || asked[0] != want || calls.Load() != 0 || rec.Path != want {
		t.Fatalf("hand-moved copy in the old folder: asked %v, downloads %d, path %s", asked, calls.Load(), rec.Path)
	}

	// Without such a copy it downloads into the topic folder.
	other := stagedItem(8, "80", 20)
	other.Topic = "Invented topic"
	rec, err = l.Ingest(context.Background(), other, bytesClient(&calls, bytes.Repeat([]byte("n"), 20)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec.Path, filepath.ToSlash(filepath.Join(group, "Invented topic"))+"/") {
		t.Fatalf("downloaded to %s", rec.Path)
	}
}
