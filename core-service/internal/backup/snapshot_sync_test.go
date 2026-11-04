package backup

import (
	"os"
	"path/filepath"
	"testing"
)

// Windows refuses to flush a file opened read-only, so the snapshot is
// reopened for writing before it is flushed.
func TestSnapshotIsFlushedThroughAWritableHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.sqlite")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var flags int
	openForSync = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		flags = flag
		return os.OpenFile(name, flag, perm)
	}
	defer func() { openForSync = os.OpenFile }()
	if err := syncFile(path); err != nil {
		t.Fatal(err)
	}
	if flags&(os.O_WRONLY|os.O_RDWR) == 0 {
		t.Fatalf("the file was opened read-only for the flush (flags %#x)", flags)
	}
}
