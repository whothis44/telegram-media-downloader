package telegram

import (
	"context"
	"errors"
	"fmt"
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
	room  func() bool
}

// ErrDownloadYield is returned by a transfer still waiting for its file slot
// when better queued work exists. No bytes have moved: the queue takes the row
// back without counting an attempt.
var ErrDownloadYield = errors.New("download gave way to a higher-priority file")

// ErrDownloadMakeRoom is ErrDownloadYield from a file over DownloadLargeFile
// waiting for its slot while small-file slots are free and smaller files wait in
// the queue: the worker should take one of those instead.
var ErrDownloadMakeRoom = fmt.Errorf("%w, so that smaller files can use their free slots", ErrDownloadYield)

// DownloadLargeFile is where the slots of large files begin (see downloadFiles).
const DownloadLargeFile = downloadLargeFile

// WithDownloadRoom adds to the transfer's rank: room, asked now and then while a
// large file waits for its slot and small-file slots are free, says whether
// smaller files wait in the queue. True gives the wait up with ErrDownloadMakeRoom.
// Without it, every worker could be parked on large files waiting for their two
// slots while the small-file slots stayed empty.
func WithDownloadRoom(ctx context.Context, room func() bool) context.Context {
	r := downloadRankOf(ctx)
	r.room = room
	return context.WithValue(ctx, downloadRankKey{}, r)
}

// WithDownloadRank orders this transfer's waits for file and part slots: lower
// goes first. yield, optional, is asked now and then while the transfer waits
// for a file slot; true gives the slot wait up with ErrDownloadYield.
func WithDownloadRank(ctx context.Context, key int64, yield func() bool) context.Context {
	return context.WithValue(ctx, downloadRankKey{}, downloadRank{key: key, yield: yield})
}

// fileCheckRank is ahead of every queued transfer (their ranks are 0 or more).
const fileCheckRank = -1

// WithFileCheckRank is for the few parts a check reads instead of downloading a
// whole file, or a preview. Their total is limited by the checks' own turns, and
// unranked they went last: with the window down to one part under a speed limit,
// behind downloads that always wait, they got no part at all and timed out.
func WithFileCheckRank(ctx context.Context) context.Context {
	return WithDownloadRank(ctx, fileCheckRank, nil)
}

// DownloadRankKeyForTest is the rank key waits use for ctx.
func DownloadRankKeyForTest(ctx context.Context) int64 { return downloadRankOf(ctx).key }

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

// ShouldYieldForTest reports what a file waiting for its slot would be told.
func ShouldYieldForTest(ctx context.Context) bool {
	r := downloadRankOf(ctx)
	return r.yield != nil && r.yield()
}

type downloadOffsetKey struct{}

// WithDownloadOffset has DownloadMedia send the file from offset on (a multiple
// of PartSize). Only for a source AttachmentClient.CanResume found: another would
// ignore it and send the file from the start.
func WithDownloadOffset(ctx context.Context, offset int64) context.Context {
	return context.WithValue(ctx, downloadOffsetKey{}, offset)
}

func downloadOffsetOf(ctx context.Context) int64 {
	offset, _ := ctx.Value(downloadOffsetKey{}).(int64)
	return offset
}
