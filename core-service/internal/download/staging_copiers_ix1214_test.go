package download

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
)

// ix.12.14: several staged files are copied to the library at once, and
// every one arrives, catalogued, with its own bytes.
func TestIx1214StagedFilesAreCopiedSeveralAtOnce(t *testing.T) {
	ctx := context.Background()
	dir, stage := t.TempDir(), t.TempDir()
	l, db, arrived := stagedLibrary(t, dir, stage)
	var calls atomic.Int64
	const n = 12
	for i := 1; i <= n; i++ {
		content := bytes.Repeat([]byte{byte('a' + i)}, 100+i)
		if _, err := l.Ingest(ctx, stagedItem(int64(i), fmt.Sprintf("f%d", i), len(content)), bytesClient(&calls, content)); err != nil {
			t.Fatal(err)
		}
	}
	if stageRows(t, db) != n {
		t.Fatalf("staged %d", stageRows(t, db))
	}
	l.MoveStaged(ctx)
	if stageRows(t, db) != 0 || len(*arrived) != n {
		t.Fatalf("rows left %d, arrived %d", stageRows(t, db), len(*arrived))
	}
	for i := 1; i <= n; i++ {
		want := bytes.Repeat([]byte{byte('a' + i)}, 100+i)
		if got := catalogued(t, db, dir, int64(i)); !bytes.Equal(got, want) {
			t.Fatalf("message %d: %d bytes, want %d", i, len(got), len(want))
		}
	}
	if st := l.StagingStatus(ctx); st.MovedHour != n {
		t.Fatalf("moved last hour: %d", st.MovedHour)
	}
}

// Moves that share a file or a message go to one copier, in order.
func TestIx1214StageBatchesKeepSharedMovesTogether(t *testing.T) {
	mk := func(id string, group string, msg int64) stagedMove {
		b, _ := json.Marshal(Item{GroupID: group, MessageID: msg})
		return stagedMove{path: id, identity: id[:1], payload: string(b)}
	}
	// a1 and a2 share a file; b1 shares a message with a2; c1 and d1 stand alone.
	due := []stagedMove{mk("a1", "g", 1), mk("c1", "g", 3), mk("a2", "g", 2), mk("b1", "g", 2), mk("d1", "h", 1)}
	got := stageBatches(due)
	var shape []string
	for _, b := range got {
		s := ""
		for _, mv := range b {
			s += mv.path + " "
		}
		shape = append(shape, s)
	}
	want := []string{"a1 a2 b1 ", "c1 ", "d1 "}
	if fmt.Sprint(shape) != fmt.Sprint(want) {
		t.Fatalf("batches %q, want %q", shape, want)
	}
}
