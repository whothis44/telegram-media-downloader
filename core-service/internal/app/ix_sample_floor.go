package app

import (
	"context"
	"strings"
	"sync"
	"time"
)

// The check floor (ix.12.11). Big videos are checked first: a check of one
// of 200 MB or more (ixSampleMinSize) costs a few MB of parts against a large
// download it may spare. Once none of them waits for a check, smaller ones
// are: the floor steps down to 100, 50 and then 20 MB. Since 12.11 the parts
// a check reads are kept for the download (ix_part_store.go), so checking a
// video that is downloaded anyway costs almost nothing. Below 20 MB a check
// reads most of the file: downloading it is as cheap.
//
// "Waits" means a row a round would check now: due (ixSampleDueOf), with a
// message that can be read, and not a file whose check failed and waits for
// its time to try again (ixSampleFileWaits): a row that can never be checked
// must not hold the floor up.
var ixSampleFloorSteps = []int64{ixSampleMinSize, 100 << 20, 50 << 20, 20 << 20}

// ixSampleFloorFor is how long a floor found is used before it is looked at again.
var ixSampleFloorFor = time.Minute

type ixSampleFloorMemo struct {
	mu    sync.Mutex
	at    time.Time
	floor int64
}

var ixSampleFloors sync.Map // *App -> *ixSampleFloorMemo

// ixSampleFloor is the size from which videos are checked now.
func (a *App) ixSampleFloor(ctx context.Context) int64 {
	v, _ := ixSampleFloors.LoadOrStore(a, &ixSampleFloorMemo{})
	m := v.(*ixSampleFloorMemo)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.floor > 0 && time.Since(m.at) < ixSampleFloorFor {
		return m.floor
	}
	floor, ok := a.ixSampleFloorNow(ctx)
	if !ok {
		return ixSampleFloorSteps[0] // as before 12.11; not remembered
	}
	m.floor, m.at = floor, time.Now()
	return floor
}

// ixSampleFloorNow finds the floor: the first step with a video at or above
// it that waits for a check, else the last. ok is false when the queue could
// not be read.
func (a *App) ixSampleFloorNow(ctx context.Context) (int64, bool) {
	now := time.Now().UnixMilli()
	waits := map[string]bool{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT DISTINCT ident FROM ix_copy_sample WHERE ident<>'' AND result='failed'
		AND (final=1 OR at >= ? - MIN(86400000, 3600000*(1<<(MIN(MAX(tries,1),6)-1))))`, now)
	if err != nil {
		return 0, false
	}
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			waits[s] = true
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return 0, false
	}
	steps := ixSampleFloorSteps
	for _, step := range steps[:len(steps)-1] {
		found, err := a.ixSampleWaitsFrom(ctx, step, now, waits)
		if err != nil {
			return 0, false
		}
		if found {
			return step, true
		}
	}
	return steps[len(steps)-1], true
}

// ixSampleWaitsFrom: a video of step or more waits for a check.
func (a *App) ixSampleWaitsFrom(ctx context.Context, step, now int64, waits map[string]bool) (bool, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT w.identity, w.body `+ixSampleDue(false), step, now)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var identity string
		var body []byte
		if rows.Scan(&identity, &body) != nil {
			continue
		}
		// The queue writes kind, id and size apart with a NUL, the checks with a colon.
		if waits[strings.ReplaceAll(identity, "\x00", ":")] {
			continue
		}
		msg, ok := ixDecodeMessage(body)
		if !ok {
			continue
		}
		if _, ok := ixCopyFactsOf(msg); !ok {
			continue
		}
		return true, nil
	}
	return false, rows.Err()
}
