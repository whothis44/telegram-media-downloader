package telegram

import (
	"context"
	"database/sql"
)

// DownloadDeferred (ix.12.13) is returned by a download that was not started
// because a check made just before it decided the file should wait (a big
// video found to be a probable copy). The queue takes the row back pending
// and paused, without counting an attempt, and Hold, run in the same
// transaction as that change, records why. If the row changed meanwhile the
// queue takes it back as after ErrDownloadYield and Hold is not run.
type DownloadDeferred struct {
	Reason string
	Hold   func(ctx context.Context, tx *sql.Tx) error
	// Failed (optional) is told when Hold returned an error and the row went
	// back to the queue unpaused instead, so the caller does not defer it
	// again in a loop.
	Failed func(error)
}

func (d *DownloadDeferred) Error() string { return "download deferred: " + d.Reason }
