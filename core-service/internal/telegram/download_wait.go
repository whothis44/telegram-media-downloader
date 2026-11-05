package telegram

import (
	"context"
	"errors"
	"math"
)

type downloadWaitKey struct{}

// WithDownloadWait lets the queue exclude deliberate waits from the transfer's
// timeout budget. The callback and its resume function must be concurrency safe.
// Parent cancellation and the timeout on an actual network request still apply.
func WithDownloadWait(ctx context.Context, pause func() func()) context.Context {
	return context.WithValue(ctx, downloadWaitKey{}, pause)
}

func pauseDownloadWait(ctx context.Context) func() {
	if pause, ok := ctx.Value(downloadWaitKey{}).(func() func()); ok && pause != nil {
		return pause()
	}
	return func() {}
}

type downloadRankKey struct{}

type downloadRank struct {
	key   int64
	yield func() bool
}

// ErrDownloadYield is returned by a transfer still waiting for its file slot
// when better queued work exists. No bytes have moved: the queue takes the row
// back without counting an attempt.
var ErrDownloadYield = errors.New("download gave way to a higher-priority file")

// WithDownloadRank orders this transfer's waits for file and part slots: lower
// goes first. yield, optional, is asked now and then while the transfer waits
// for a file slot; true gives the slot wait up with ErrDownloadYield.
func WithDownloadRank(ctx context.Context, key int64, yield func() bool) context.Context {
	return context.WithValue(ctx, downloadRankKey{}, downloadRank{key: key, yield: yield})
}

// downloadRankOf is the transfer's rank; one without any goes last, in no
// order among others alike.
func downloadRankOf(ctx context.Context) downloadRank {
	if r, ok := ctx.Value(downloadRankKey{}).(downloadRank); ok {
		return r
	}
	return downloadRank{key: math.MaxInt64}
}

// rankWaiters counts who waits for a slot, by rank. Every waiter is woken when
// a slot frees and only the best rank may take it.
type rankWaiters map[int64]int

func (w rankWaiters) add(key int64) { w[key]++ }

func (w rankWaiters) remove(key int64) {
	if w[key] <= 1 {
		delete(w, key)
		return
	}
	w[key]--
}

// better reports whether someone with a better rank than key is waiting.
func (w rankWaiters) better(key int64) bool {
	for k := range w {
		if k < key {
			return true
		}
	}
	return false
}
