package download

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// ix.12.14: the staging folder keeps a free space on its disk instead of
// keeping to a size. A new download is staged while the disk keeps that much
// free after it, whatever the staged files take, and goes straight to the
// library otherwise; the old size only applies when the free space cannot be
// read.
func TestIx1214StagingKeepsFreeSpaceOnItsDisk(t *testing.T) {
	ctx := context.Background()
	dir, stage := t.TempDir(), t.TempDir()
	l, _, _ := stagedLibrary(t, dir, stage)
	l.Staging = func(context.Context) Staging { return Staging{Dir: stage, KeepFree: 50 << 30, Cap: 30} }
	free := atomic.Int64{}
	free.Store(200 << 30)
	setFreeSpace(func(string) (int64, bool) { return free.Load(), true })
	t.Cleanup(func() { stageFreeHook.Store(nil) })

	var calls atomic.Int64
	if _, err := l.Ingest(ctx, stagedItem(1, "1", 20), bytesClient(&calls, bytes.Repeat([]byte("a"), 20))); err != nil {
		t.Fatal(err)
	}
	// 20 bytes staged, past the old size of 30 with a 20-byte file: the
	// free space decides, and 200 GB leave far more than 50.
	if !l.StageRoom(ctx, 20) {
		t.Fatal("a file was refused with 200 GB free and 50 to keep")
	}
	if !l.StageRoom(ctx, 150<<30) || l.StageRoom(ctx, 150<<30+1) {
		t.Fatal("room: up to exactly 150 GB fits with 200 GB free and 50 to keep")
	}
	st := l.StagingStatus(ctx)
	if !st.RoomKnown || st.Room != 150<<30 || st.KeepFree != 50<<30 {
		t.Fatalf("status room: %+v", st)
	}

	// Down to 50 GB free: new downloads go straight to the library, even
	// with nothing staged (the size used to let one through).
	free.Store(50 << 30)
	if l.StageRoom(ctx, 1) {
		t.Fatal("a file was staged with the disk at the free space it keeps")
	}
	if st := l.StagingStatus(ctx); st.Room != 0 || !st.RoomKnown {
		t.Fatalf("status room at the limit: %+v", st)
	}
	free.Store(40 << 30)
	if st := l.StagingStatus(ctx); st.Room != 0 {
		t.Fatalf("room below the limit must be 0, got %d", st.Room)
	}

	// A download whose bytes are staged waits for copies while one waits...
	quick, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if err := l.WaitStageRoom(quick, 20); err == nil {
		t.Fatal("a staged download went on with the disk below the free space to keep")
	}
	// ...and goes on once the disk has room again.
	done := make(chan error, 1)
	go func() { done <- l.WaitStageRoom(ctx, 20) }()
	time.Sleep(30 * time.Millisecond)
	free.Store(60 << 30)
	l.stageSignal()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("still waiting with room on the disk")
	}

	// With no staged file left to copy, waiting cannot make room: it goes on.
	free.Store(1 << 30)
	l.MoveStaged(ctx)
	if st := l.StagingStatus(ctx); st.Waiting != 0 {
		t.Fatalf("not moved: %+v", st)
	}
	quick2, cancel2 := context.WithTimeout(ctx, time.Second)
	defer cancel2()
	if err := l.WaitStageRoom(quick2, 20); err != nil {
		t.Fatalf("a staged download waited with nothing to copy: %v", err)
	}
	if l.StageRoom(ctx, 1) {
		t.Fatal("a new download was staged with 1 GB free")
	}
}

// With no free space to read, the staged files keep to the size, as before.
func TestIx1214StagingWithoutFreeSpaceKeepsToTheSize(t *testing.T) {
	ctx := context.Background()
	dir, stage := t.TempDir(), t.TempDir()
	l, _, _ := stagedLibrary(t, dir, stage)
	l.Staging = func(context.Context) Staging { return Staging{Dir: stage, Cap: 30} }
	noFreeSpaceKnown(t)
	if !l.StageRoom(ctx, 1000) {
		t.Fatal("one file always goes through")
	}
	var calls atomic.Int64
	if _, err := l.Ingest(ctx, stagedItem(1, "1", 20), bytesClient(&calls, bytes.Repeat([]byte("a"), 20))); err != nil {
		t.Fatal(err)
	}
	if !l.StageRoom(ctx, 10) || l.StageRoom(ctx, 11) {
		t.Fatal("room: 10 more bytes fit in 30")
	}
	if st := l.StagingStatus(ctx); st.RoomKnown || st.KeepFree != DefaultStageKeepFree {
		t.Fatalf("status: %+v", st)
	}
}
