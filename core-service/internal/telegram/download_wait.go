package telegram

import "context"

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
