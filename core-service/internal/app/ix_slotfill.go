package app

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// ix.12.7: the deadline order's keep-busy counts by server slots.
//
// Each Telegram server downloads a few files of each size at once (5 under
// 20 MB, 2 larger, from Telegram's configuration), and the workers skip a
// server whose slots of a size are full (engine.FullServers). Seen live on
// 11-06 08:03: every claimable file was a ranked group's big file on DC1 or
// DC4, so 6 files downloaded, no small slot was used and DC5 sat idle, while
// 11,000 files of the next band were set aside. The keep-busy step counted
// those 750 big files as work the workers could take, so it let nothing go.
// Now a claimable file counts only while its server has a free slot of its
// size, and set-aside files are let go into the slots nothing else would
// fill: on that server, of that size, smallest band first. They take nothing
// from the files going first (a file can only download from its own server),
// and those not taken are set aside again at the next step.

// ixFileSlots is the engine's file slots per server (small, large); 0, 0
// when unknown. Replaceable in tests.
var ixFileSlots = func(a *App) (small, large int) {
	if a.monitor == nil {
		return 0, 0
	}
	return a.monitor.FileSlots()
}

// ixSlotRoom is what the claimable files leave of each server's slots.
type ixSlotRoom struct {
	Free     map[int]int // engine.ServerKey -> slots no claimable file would take
	Ready    int         // claimable files that would start now
	Busy     int         // files downloading
	LaneLeft int         // places left in the big-file lane (-1: no lane)
}

// ixSlotRoomNow measures ixSlotRoom for the accounts of acctClause (args:
// now, then the accounts, as ixDeadlineStep's). Files of a server not known
// yet (dc 0 or -1) are never kept off a server (engine.avoidServersSQL), so
// they count as ready.
func (a *App) ixSlotRoomNow(ctx context.Context, acctClause string, args []any, small, large int) (ixSlotRoom, error) {
	room := ixSlotRoom{Free: map[int]int{}, LaneLeft: -1}
	if lane := engine.BigFileLane(); lane > 0 {
		var running int
		if err := a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM tgdl_work WHERE status='processing' AND file_size>=? AND ttl_seconds=0`, engine.BigFileSize).Scan(&running); err != nil {
			return room, err
		}
		room.LaneLeft = max(0, lane-running)
	}
	q := `SELECT dc, file_size>=?, COALESCE(SUM(status='processing'),0),
		COALESCE(SUM(status='pending' AND paused=0 AND retry_at<=? AND (file_size<? OR ttl_seconds>0)),0),
		COALESCE(SUM(status='pending' AND paused=0 AND retry_at<=? AND file_size>=? AND ttl_seconds=0),0)
		FROM tgdl_work WHERE status IN ('pending','processing')` + acctClause + ` GROUP BY 1,2 ORDER BY 1,2`
	qargs := append([]any{telegram.DownloadLargeFile, args[0], engine.BigFileSize, args[0], engine.BigFileSize}, args[1:]...)
	rows, err := a.db.Reader.QueryContext(ctx, q, qargs...)
	if err != nil {
		return room, err
	}
	defer rows.Close()
	lane := func(n int) int { // big files the lane lets start
		if room.LaneLeft < 0 {
			return n
		}
		n = min(n, room.LaneLeft)
		room.LaneLeft -= n
		return n
	}
	for rows.Next() {
		var dc, busy, claim, bigClaim int
		var big bool
		if err := rows.Scan(&dc, &big, &busy, &claim, &bigClaim); err != nil {
			return room, err
		}
		room.Busy += busy
		if dc <= 0 {
			room.Ready += claim + lane(bigClaim)
			continue
		}
		slots := small
		if big {
			slots = large
		}
		free := max(0, slots-busy)
		take := min(claim, free)
		free -= take
		bigTake := lane(min(bigClaim, free))
		free -= bigTake
		room.Ready += take + bigTake
		room.Free[engine.ServerKey(dc, big)] = free
	}
	return room, rows.Err()
}

// ixSlotFillPick chooses up to need set-aside files to let go into the free
// slots of room: per server and size no more than its free slots, then the
// smallest band first across servers (band, size, id: the keep-busy order).
// Held copies stay held, and big files (engine.BigFileSize) only while the
// big-file lane has a place.
func (a *App) ixSlotFillPick(ctx context.Context, acctClause string, args []any, room ixSlotRoom, need int) ([]int64, error) {
	if need <= 0 {
		return nil, nil
	}
	type cand struct {
		id         int64
		band       int
		size       int64
		key, isBig int
	}
	keys := make([]int, 0, len(room.Free))
	for k, n := range room.Free {
		if n > 0 {
			keys = append(keys, k)
		}
	}
	sort.Ints(keys)
	var all []cand
	acct := strings.ReplaceAll(acctClause, "account_id", "w.account_id")
	for _, k := range keys {
		dc, big := k/2, k%2 == 1
		q := `SELECT w.id, x.band, w.file_size, w.file_size>=? AND w.ttl_seconds=0 FROM ix_deferred x JOIN tgdl_work w ON w.id=x.work_id
			WHERE x.released_at=0 AND w.status='pending' AND w.paused=1 AND w.retry_at<=?` + acct + `
			AND w.dc=? AND (w.file_size>=?)=?
			AND w.id NOT IN (SELECT work_id FROM ix_held WHERE released_at=0 AND reason<>'')
			ORDER BY x.band, w.file_size, w.id LIMIT ` + strconv.Itoa(min(room.Free[k], need))
		qargs := append([]any{engine.BigFileSize, args[0]}, args[1:]...)
		qargs = append(qargs, dc, telegram.DownloadLargeFile, big)
		rows, err := a.db.Reader.QueryContext(ctx, q, qargs...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			c := cand{key: k}
			if err := rows.Scan(&c.id, &c.band, &c.size, &c.isBig); err != nil {
				rows.Close()
				return nil, err
			}
			all = append(all, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].band != all[j].band {
			return all[i].band < all[j].band
		}
		if all[i].size != all[j].size {
			return all[i].size < all[j].size
		}
		return all[i].id < all[j].id
	})
	left := room.Free
	laneLeft := room.LaneLeft
	var ids []int64
	for _, c := range all {
		if len(ids) >= need {
			break
		}
		if left[c.key] <= 0 {
			continue
		}
		if c.isBig == 1 && laneLeft >= 0 {
			if laneLeft == 0 {
				continue
			}
			laneLeft--
		}
		left[c.key]--
		ids = append(ids, c.id)
	}
	return ids, nil
}

// ixSlotFillStats counts the set-aside files let go to keep workers busy,
// for the diagnostics report.
var ixSlotFillStats struct {
	mu    sync.Mutex
	total int
	last  time.Time
}

func (a *App) ixSlotFillNote(n int) {
	ixSlotFillStats.mu.Lock()
	ixSlotFillStats.total += n
	ixSlotFillStats.last = time.Now()
	ixSlotFillStats.mu.Unlock()
}

// ixSlotFillSeen: files let go into free slots since the start, and when last.
func ixSlotFillSeen() (int, time.Time) {
	ixSlotFillStats.mu.Lock()
	defer ixSlotFillStats.mu.Unlock()
	return ixSlotFillStats.total, ixSlotFillStats.last
}

// ixSlotDiag writes the report's line on server slots: per server and size,
// the files downloading of the slots there are, those that could start, and
// those set aside; then the set-aside files let go into free slots.
func (a *App) ixSlotDiag(ctx context.Context, line func(string, ...any), now time.Time) {
	small, large := ixFileSlots(a)
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT w.dc, w.file_size>=?, COALESCE(SUM(w.status='processing'),0),
		COALESCE(SUM(w.status='pending' AND w.paused=0 AND w.retry_at<=?),0),
		COALESCE(SUM(w.status='pending' AND x.work_id IS NOT NULL),0)
		FROM tgdl_work w LEFT JOIN (SELECT DISTINCT work_id FROM ix_deferred WHERE released_at=0) x ON x.work_id=w.id
		WHERE w.status IN ('pending','processing') AND w.dc>0 GROUP BY 1,2 ORDER BY 1,2`, telegram.DownloadLargeFile, now.UnixMilli())
	if err != nil {
		return
	}
	var parts []string
	for rows.Next() {
		var dc, busy, ready, aside int
		var big bool
		if rows.Scan(&dc, &big, &busy, &ready, &aside) != nil {
			break
		}
		size, slots := "small", small
		if big {
			size, slots = "large", large
		}
		parts = append(parts, fmt.Sprintf("DC%d %s %d/%d (%d could start, %d set aside)", dc, size, busy, slots, ready, aside))
	}
	rows.Close()
	n, last := ixSlotFillSeen()
	lastMs := int64(0)
	if !last.IsZero() {
		lastMs = last.UnixMilli()
	}
	line("file slots by server (downloading/slots): %s · set-aside files let go into free slots: %d since start, last %s", strings.Join(parts, " · "), n, ixAgo(lastMs, now))
}
