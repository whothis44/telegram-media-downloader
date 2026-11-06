package telegram

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestFileSlotGoesToTheBestRankWaiting(t *testing.T) {
	f := newDownloadFiles()
	f.large = 1
	ctx := context.Background()
	release, err := f.acquire(ctx, 1, downloadLargeFile)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []int64
	var wg sync.WaitGroup
	started := make(chan struct{}, 2)
	for _, key := range []int64{50, 10} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started <- struct{}{}
			rel, err := f.acquire(WithDownloadRank(ctx, key, nil), 1, downloadLargeFile)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			got = append(got, key)
			mu.Unlock()
			rel()
		}()
		<-started
		for {
			f.mu.Lock()
			n := f.waiting[downloadFileKey{dc: 1, large: true}][key]
			f.mu.Unlock()
			if n == 1 {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	release()
	wg.Wait()
	if len(got) != 2 || got[0] != 10 || got[1] != 50 {
		t.Fatalf("order %v, want the better rank first", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.waiting[downloadFileKey{dc: 1, large: true}]); n != 0 {
		t.Fatalf("%d waiters left behind", n)
	}
}

// Another size class or datacenter has its own line.
func TestFileSlotRankDoesNotCrossClasses(t *testing.T) {
	f := newDownloadFiles()
	f.large = 1
	ctx := context.Background()
	hold, _ := f.acquire(ctx, 1, downloadLargeFile)
	defer hold()
	waiting, cancel := context.WithCancel(WithDownloadRank(ctx, 1, nil))
	defer cancel()
	go f.acquire(waiting, 1, downloadLargeFile)
	time.Sleep(10 * time.Millisecond)
	done := make(chan error, 2)
	go func() {
		rel, err := f.acquire(WithDownloadRank(ctx, 99, nil), 1, 1024)
		if rel != nil {
			rel()
		}
		done <- err
	}()
	go func() {
		rel, err := f.acquire(WithDownloadRank(ctx, 99, nil), 2, downloadLargeFile)
		if rel != nil {
			rel()
		}
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a free slot of another class waited behind a better rank")
		}
	}
}

func TestFileSlotWaitGivesWayWhenAsked(t *testing.T) {
	old := downloadYieldEvery
	downloadYieldEvery = 5 * time.Millisecond
	defer func() { downloadYieldEvery = old }()
	f := newDownloadFiles()
	f.large = 1
	ctx := context.Background()
	hold, _ := f.acquire(ctx, 1, downloadLargeFile)
	defer hold()
	asks := 0
	_, err := f.acquire(WithDownloadRank(ctx, 5, func() bool { asks++; return asks >= 2 }), 1, downloadLargeFile)
	if !errors.Is(err, ErrDownloadYield) {
		t.Fatalf("err=%v, want it to give way", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.waiting[downloadFileKey{dc: 1, large: true}]); n != 0 {
		t.Fatalf("%d waiters left behind", n)
	}
	if f.active[downloadFileKey{dc: 1, large: true}] != 1 {
		t.Fatal("giving way changed the slots in use")
	}
}

// A better waiter that leaves must not strand a worse one behind it.
func TestFileSlotWakesTheNextWhenABetterWaiterLeaves(t *testing.T) {
	f := newDownloadFiles()
	f.large = 1
	ctx := context.Background()
	hold, _ := f.acquire(ctx, 1, downloadLargeFile)
	better, cancel := context.WithCancel(WithDownloadRank(ctx, 1, nil))
	betterDone := make(chan struct{})
	go func() {
		// The better waiter may take the freed slot before it is cancelled:
		// then it gives the slot back, as every caller does.
		if rel, err := f.acquire(better, 1, downloadLargeFile); err == nil && rel != nil {
			rel()
		}
		close(betterDone)
	}()
	for {
		f.mu.Lock()
		n := f.waiting[downloadFileKey{dc: 1, large: true}][1]
		f.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	worseDone := make(chan error, 1)
	go func() {
		rel, err := f.acquire(WithDownloadRank(ctx, 9, nil), 1, downloadLargeFile)
		if rel != nil {
			rel()
		}
		worseDone <- err
	}()
	time.Sleep(10 * time.Millisecond)
	hold() // free, but the better waiter is first in line
	cancel()
	<-betterDone
	select {
	case err := <-worseDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the worse waiter was stranded")
	}
}

func TestPartRoomGoesToTheBestRankWaiting(t *testing.T) {
	p := newDownloadPacer("")
	ctx := context.Background()
	p.mu.Lock()
	p.dcLocked(1).window = 1
	p.mu.Unlock()
	if err := p.acquire(ctx, 1); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []int64
	var wg sync.WaitGroup
	for _, key := range []int64{7, 3} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.acquire(WithDownloadRank(ctx, key, nil), 1); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			got = append(got, key)
			mu.Unlock()
			p.release(1, 0)
		}()
		for {
			p.mu.Lock()
			n := p.dcLocked(1).ranks[key]
			p.mu.Unlock()
			if n == 1 {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	p.release(1, 0)
	wg.Wait()
	if len(got) != 2 || got[0] != 3 {
		t.Fatalf("order %v, want rank 3 first", got)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if d := p.dcLocked(1); d.waiting != 0 || len(d.ranks) != 0 {
		t.Fatalf("waiting=%d ranks=%v left behind", d.waiting, d.ranks)
	}
}

func TestPartWaitCancelledLeavesNothingBehind(t *testing.T) {
	p := newDownloadPacer("")
	bg := context.Background()
	p.mu.Lock()
	p.dcLocked(1).window = 1
	p.mu.Unlock()
	if err := p.acquire(bg, 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(WithDownloadRank(bg, 2, nil))
	done := make(chan error, 1)
	go func() { done <- p.acquire(ctx, 1) }()
	for {
		p.mu.Lock()
		n := p.dcLocked(1).waiting
		p.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if d := p.dcLocked(1); d.waiting != 0 || len(d.ranks) != 0 {
		t.Fatalf("waiting=%d ranks=%v left behind", d.waiting, d.ranks)
	}
}

// A large file parked on its slot makes room when small-file slots are free and
// smaller files wait; not when those slots are taken, or when nothing smaller waits.
func TestLargeFileWaitMakesRoomForSmallFiles(t *testing.T) {
	old := downloadYieldEvery
	downloadYieldEvery = 5 * time.Millisecond
	defer func() { downloadYieldEvery = old }()
	f := newDownloadFiles()
	f.large, f.small = 1, 2
	ctx := context.Background()
	hold, _ := f.acquire(ctx, 1, downloadLargeFile)
	defer hold()

	_, err := f.acquire(WithDownloadRoom(WithDownloadRank(ctx, 5, nil), func() bool { return true }), 1, downloadLargeFile)
	if !errors.Is(err, ErrDownloadMakeRoom) || !errors.Is(err, ErrDownloadYield) {
		t.Fatalf("err=%v, want it to make room (a kind of giving way)", err)
	}

	// Nothing smaller waits: it keeps waiting.
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel()
	if _, err := f.acquire(WithDownloadRoom(WithDownloadRank(waitCtx, 5, nil), func() bool { return false }), 1, downloadLargeFile); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want it still waiting", err)
	}

	// The small-file slots are taken: room would help nobody.
	s1, _ := f.acquire(ctx, 1, 1<<20)
	s2, _ := f.acquire(ctx, 1, 1<<20)
	defer s1()
	defer s2()
	asked := false
	waitCtx2, cancel2 := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel2()
	if _, err := f.acquire(WithDownloadRoom(WithDownloadRank(waitCtx2, 5, nil), func() bool { asked = true; return true }), 1, downloadLargeFile); !errors.Is(err, context.DeadlineExceeded) || asked {
		t.Fatalf("err=%v asked=%v, want it still waiting without asking", err, asked)
	}

	// A small file never makes room.
	f2 := newDownloadFiles()
	f2.small = 1
	h, _ := f2.acquire(ctx, 1, 1<<20)
	defer h()
	waitCtx3, cancel3 := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel3()
	if _, err := f2.acquire(WithDownloadRoom(WithDownloadRank(waitCtx3, 5, nil), func() bool { return true }), 1, 1<<20); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, a small file made room", err)
	}
}
