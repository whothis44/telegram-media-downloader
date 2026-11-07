package app

import (
	"context"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// ix.12.8: files no server slot will take do not fill the queue limit.
//
// A backfill (a rescan's files pass) stops queueing while the queue holds its
// limit of files that can download (ixBigFilesMeasure). Seen live on 11-06
// (08:03 to 10:43): about 470 claimable files of one server waited for its 2
// slots for files of 20 MB or more, and they alone kept the queue at its
// limit, so the rescans queued nothing for hours: the files of the other
// servers, and the small files the deadline order was waiting for, never
// arrived. Those files cannot start before a slot of their size frees on
// their own server, however long the backfill waits, so they now wait apart,
// as files of 1 GB waiting for the big-file lane already do, and under the
// same bound (ixBigApartTimes the limit, the two together): the backfills
// still cannot queue without end.

// ixSlotBlockedNow counts the backlog of claimable files whose server's slots
// of their size are taken: per server and size, the claimable files beyond
// those its free slots would take. Files of a server not known yet (dc 0 or
// -1), temporary ones (ttl), live monitoring's, and files of 1 GB or more
// (they wait for the lane, counted apart already) are not counted; nor
// anything while the slots are unknown.
func (a *App) ixSlotBlockedNow(ctx context.Context) (int, error) {
	small, large := ixFileSlots(a)
	if small <= 0 || large <= 0 {
		return 0, nil
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT dc, file_size>=?, COALESCE(SUM(status='processing'),0),
		COALESCE(SUM(status='pending' AND paused=0 AND origin<>'live' AND ttl_seconds=0 AND file_size<?),0)
		FROM tgdl_work WHERE status IN ('pending','processing') AND dc>0 GROUP BY 1,2`, telegram.DownloadLargeFile, engine.BigFileSize)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	blocked := 0
	for rows.Next() {
		var dc, busy, claim int
		var big bool
		if err := rows.Scan(&dc, &big, &busy, &claim); err != nil {
			return 0, err
		}
		slots := small
		if big {
			slots = large
		}
		blocked += max(0, claim-max(0, slots-busy))
	}
	return blocked, rows.Err()
}
