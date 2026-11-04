package app

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Racing the deletion date. When Telegram has set one, the archive
// estimates whether the download queue will finish before it at the rate
// actually achieved, and, when it will not (or when asked to always), lets
// smaller files go first: a large video waits until every smaller file of
// the backfill is done, so the most files are saved before the account
// goes. Files of private chats go before those of public ones (see
// ixPublicLane). Nothing is skipped, only put back: the deferred files are queue
// items set aside (as held copies are) and let go band by band. Live
// downloads, self-destructing media and held copies are not touched. Where
// this stands among the other things that wait at the back of the queue
// is set in ix_order.go: right after normal work.

// ixDeadlineBands are the size limits of the bands, smallest first.
var ixDeadlineBands = []int64{10 << 20, 100 << 20, 500 << 20}

func ixBandOf(size int64) int {
	for i, limit := range ixDeadlineBands {
		if size < limit {
			return i
		}
	}
	return len(ixDeadlineBands)
}

// ixPublicLane is added to the band of a file from a public chat (one with
// a username): every private chat's file goes before any public one's.
// What only this account can read is lost with it; a public chat can be read
// again from another account, and a public channel even without one
// (t.me/s/...).
const ixPublicLane = 4

// ixBandSQL is the band of a queue item of table (or alias) t in SQL: its
// size band, plus ixPublicLane for a public chat.
func ixBandSQL(t string) string {
	return `(CASE WHEN ` + t + `.file_size<10485760 THEN 0 WHEN ` + t + `.file_size<104857600 THEN 1 WHEN ` + t + `.file_size<524288000 THEN 2 ELSE 3 END
		+ CASE WHEN EXISTS (SELECT 1 FROM ix_group_info g WHERE g.group_id=` + t + `.group_id AND g.username<>'') THEN ` + strconv.Itoa(ixPublicLane) + ` ELSE 0 END)`
}

// ixBandCount is how many bands there are, both lanes.
const ixBandCount = 2 * ixPublicLane

type ixDeadlineLoss struct {
	GroupID string `json:"groupId"`
	Name    string `json:"name"`
	Private bool   `json:"private"` // no public username: cannot be read again from another account
	Files   int    `json:"files"`
	Bytes   int64  `json:"bytes"`
	Scanned bool   `json:"scanned"` // its messages have all been read
}

type ixDeadlineInfo struct {
	Mode       string           `json:"mode"`   // auto, on, off
	Active     bool             `json:"active"` // smaller files are going first now
	Until      int64            `json:"until,omitempty"`
	LeftFiles  int              `json:"leftFiles"` // in the download queue now
	LeftBytes  int64            `json:"leftBytes"`
	ToGetFiles int              `json:"toGetFiles"` // every distinct file not downloaded yet (copies counted once), queued or not
	ToGetBytes int64            `json:"toGetBytes"`
	Copies     int              `json:"copies"`   // not downloaded, but the same Telegram file is saved or queued elsewhere
	Rate24h    float64          `json:"rate24h"`  // bytes a second over the last day
	Rate1h     float64          `json:"rate1h"`   // over the last hour
	EtaSec     int64            `json:"etaSec"`   // at the day's rate; 0: unknown
	Finishes   string           `json:"finishes"` // yes, no, unknown
	Deferred   []int            `json:"deferred"` // items set aside, by band
	Unscanned  int              `json:"unscanned"`
	Loss       []ixDeadlineLoss `json:"loss"`
	BandLimits []int64          `json:"bandLimits"`
}

// ixDeadlineEstimate measures what is left and how fast it goes.
func (a *App) ixDeadlineEstimate(ctx context.Context, withLoss bool) ixDeadlineInfo {
	s := a.ixLoadSettings(ctx)
	d := ixDeadlineInfo{Mode: s.DeadlineOrder, Until: a.ixFreeze(ctx).Until, Deferred: make([]int, ixBandCount), BandLimits: ixDeadlineBands}
	if d.Mode == "" {
		d.Mode = "auto"
	}
	now := time.Now()
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(file_size),0) FROM tgdl_work WHERE status IN ('pending','processing')`).Scan(&d.LeftFiles, &d.LeftBytes)
	rate := func(since time.Duration) float64 {
		var bytes int64
		// Files covered by a copy already saved were not downloaded: they do
		// not count towards the speed.
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(SUM(w.file_size),0) FROM tgdl_work w
			WHERE w.status='completed' AND w.updated_at>=?
			AND NOT `+ixSkippedBefore, now.Add(-since).UnixMilli()).Scan(&bytes)
		return float64(bytes) / since.Seconds()
	}
	d.Rate24h, d.Rate1h = rate(24*time.Hour), rate(time.Hour)
	// The queue holds only what rescans have reached so far: what is left
	// is every file seen in messages and not downloaded, a copy counted
	// once (the browser's model knows which are copies).
	need := d.LeftBytes
	if m := a.ixModelLast(); m != nil {
		for ci := range m.Clusters {
			saved, wanted := false, false
			var size int64
			missing := 0
			for _, ii := range m.Clusters[ci].Items {
				it := &m.Items[ii]
				if it.Key != "" {
					saved = true
				}
				if it.Status == "missing" {
					wanted = true
					missing++
					size = max(size, it.Size)
				}
			}
			switch {
			case saved:
				d.Copies += missing
			case wanted:
				d.ToGetFiles++
				d.ToGetBytes += size
				d.Copies += missing - 1
			}
		}
		need = max(need, d.ToGetBytes)
	}
	d.Finishes = "unknown"
	if d.Rate24h > 0 {
		d.EtaSec = int64(float64(need) / d.Rate24h)
		if d.Until > 0 {
			d.Finishes = map[bool]string{true: "yes", false: "no"}[now.Unix()+d.EtaSec < d.Until]
		}
	}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT band, COUNT(*) FROM ix_deferred WHERE released_at=0 GROUP BY band`); err == nil {
		for rows.Next() {
			var b, n int
			if rows.Scan(&b, &n) == nil && b >= 0 && b < len(d.Deferred) {
				d.Deferred[b] = n
			}
		}
		rows.Close()
	}
	d.Active = ixDeadlineActive(d)
	// A chat counts as read once a full rescan finished (now or before),
	// unless it is waiting in the queue for another one.
	q, _ := a.ixLoadQueue(ctx)
	scanned := map[string]bool{}
	for id, at := range a.ixLatestWalks(ctx) {
		scanned[id] = at > 0
	}
	for _, it := range q.Items {
		if it.State == "done" {
			scanned[it.GroupID] = true
		} else {
			scanned[it.GroupID] = false
			d.Unscanned++
		}
	}
	if !withLoss {
		return d
	}
	names := map[string]string{}
	if metas, err := a.ixGroups(ctx); err == nil {
		for _, g := range metas {
			names[g.ID] = g.Name
		}
	}
	public := map[string]bool{}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id FROM ix_group_info WHERE username<>''`); err == nil {
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				public[id] = true
			}
		}
		rows.Close()
	}
	byGroup := map[string]*ixDeadlineLoss{}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, COUNT(*), COALESCE(SUM(file_size),0) FROM tgdl_work WHERE status IN ('pending','processing') GROUP BY group_id`); err == nil {
		for rows.Next() {
			var l ixDeadlineLoss
			if rows.Scan(&l.GroupID, &l.Files, &l.Bytes) == nil {
				byGroup[l.GroupID] = &l
			}
		}
		rows.Close()
	}
	for _, it := range q.Items {
		if it.State != "done" && byGroup[it.GroupID] == nil {
			byGroup[it.GroupID] = &ixDeadlineLoss{GroupID: it.GroupID}
		}
	}
	for id, l := range byGroup {
		l.Name = names[id]
		if l.Name == "" {
			l.Name = "Chat " + id
		}
		l.Private = !public[id]
		l.Scanned = scanned[id]
		d.Loss = append(d.Loss, *l)
	}
	sort.Slice(d.Loss, func(i, j int) bool {
		a, b := d.Loss[i], d.Loss[j]
		if a.Private != b.Private {
			return a.Private
		}
		if a.Scanned != b.Scanned {
			return !a.Scanned
		}
		if a.Bytes != b.Bytes {
			return a.Bytes > b.Bytes
		}
		return a.Name < b.Name
	})
	if len(d.Loss) > 300 {
		d.Loss = d.Loss[:300]
	}
	return d
}

// ixDeadlineActive: on always; auto when a deletion date is set and the
// queue would not finish before it (or the speed is not known yet).
func ixDeadlineActive(d ixDeadlineInfo) bool {
	switch d.Mode {
	case "on":
		return true
	case "off":
		return false
	}
	return d.Until > 0 && d.Finishes != "yes" && (d.LeftFiles > 0 || d.ToGetFiles > 0)
}

func (a *App) ixRunDeadline(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
		if !ixVdupBackground.Load() {
			continue
		}
		if err := a.ixDeadlineStep(ctx); err != nil && ctx.Err() == nil {
			a.ixLog("deadline order: %v", err)
		}
	}
}

// ixDeadlineStep sets aside the backfill items larger than the smallest
// band that still has work, and lets go those of that band.
func (a *App) ixDeadlineStep(ctx context.Context) error {
	if a.ixIsGone() {
		return nil // read-only: nothing downloads any more, so there is no order to keep
	}
	d := a.ixDeadlineEstimate(ctx, false)
	nowT := time.Now()
	now := nowT.UnixMilli()
	for _, q := range []string{
		// Items no longer waiting (done, failed, removed) are let go.
		`UPDATE ix_deferred SET released_at=? WHERE released_at=0
			AND NOT EXISTS (SELECT 1 FROM tgdl_work w WHERE w.id=ix_deferred.work_id AND w.status='pending')`,
		// An item a person resumed is theirs: never set aside again (-1).
		`UPDATE ix_deferred SET released_at=-1 WHERE released_at=0 AND ? > 0
			AND EXISTS (SELECT 1 FROM tgdl_work w WHERE w.id=ix_deferred.work_id AND w.status='pending' AND w.paused=0)`,
	} {
		if _, err := a.db.Writer.ExecContext(ctx, q, now); err != nil {
			return err
		}
	}
	// Items that became an explicit request or self-destructing media go
	// now; the others' bands follow their current size.
	if err := a.ixDeadlineLetGo(ctx, `(w.origin<>'history' OR w.ttl_seconds>0)`); err != nil {
		return err
	}
	if _, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_deferred SET band=(SELECT `+ixBandSQL("w")+` FROM tgdl_work w WHERE w.id=ix_deferred.work_id)
		WHERE released_at=0 AND EXISTS (SELECT 1 FROM tgdl_work w WHERE w.id=ix_deferred.work_id)`); err != nil {
		return err
	}
	if !d.Active || a.ixDeadlineSuspended(ctx, nowT) {
		return a.ixDeadlineRelease(ctx, -1)
	}
	// Downloads have not moved for half an hour (no file finished, no bytes
	// arrived) while files are set aside: the order may be what holds
	// things up. Let everything go and stand back for two hours. Big files
	// that take hours each still move, by bytes.
	if a.ixDeferredWaiting(ctx) > 0 && !a.ixQueuePaused(ctx) {
		var lastDone, firstSet int64
		if last := a.ixLastProgress(ctx); !last.IsZero() {
			lastDone = last.UnixMilli()
		}
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(MIN(deferred_at),0) FROM ix_deferred WHERE released_at=0`).Scan(&firstSet)
		if stall := 30 * time.Minute.Milliseconds(); now-lastDone > stall && firstSet > 0 && now-firstSet > stall {
			a.ixLog("deadline order: downloads have not moved for 30 minutes; letting every set-aside file go for 2 hours")
			_, _ = a.db.Writer.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at) VALUES('ix_deadline_suspend',?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`,
				strconv.FormatInt(nowT.Add(2*time.Hour).UnixMilli(), 10), now)
			return a.ixDeadlineRelease(ctx, -1)
		}
	}
	// The smallest band with work the downloader could take now: not items
	// waiting to retry, and only of accounts that are downloading (an item
	// of an account that is not running must not hold the others back).
	accounts := a.ixWorkingAccounts(ctx, nowT)
	acctClause, args := "", []any{now}
	if len(accounts) > 0 {
		acctClause = " AND account_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(accounts)), ",") + ")"
		for _, id := range accounts {
			args = append(args, id)
		}
	}
	var cur sql.NullInt64
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT MIN(b) FROM (
			SELECT `+ixBandSQL("tgdl_work")+` AS b FROM tgdl_work WHERE status='pending' AND paused=0 AND origin='history' AND ttl_seconds=0 AND retry_at<=?`+acctClause+`
			UNION ALL SELECT x.band FROM ix_deferred x JOIN tgdl_work w ON w.id=x.work_id WHERE x.released_at=0 AND w.status='pending')`, args...).Scan(&cur); err != nil {
		return err
	}
	if !cur.Valid {
		return nil
	}
	band := int(cur.Int64)
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO ix_deferred(work_id,band,deferred_at,released_at)
		SELECT id, `+ixBandSQL("tgdl_work")+`, ?, 0 FROM tgdl_work WHERE status='pending' AND paused=0 AND origin='history' AND ttl_seconds=0 AND `+ixBandSQL("tgdl_work")+`>?
		AND id NOT IN (SELECT work_id FROM ix_deferred WHERE released_at=-1)`, now, band); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tgdl_work SET paused=1 WHERE status='pending' AND paused=0 AND origin='history' AND ttl_seconds=0 AND `+ixBandSQL("tgdl_work")+`>?
		AND id IN (SELECT work_id FROM ix_deferred WHERE released_at=0)`, band); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return a.ixDeadlineRelease(ctx, band)
}

// ixEngineRun is the engine's run (engine.Controller.Running); replaceable
// in tests.
var ixEngineRun = func(a *App) (accounts []string, observing, running bool) {
	if a.monitor == nil {
		return nil, false, false
	}
	return a.monitor.Running()
}

// ixWorkingAccounts are the accounts that can download now: those the
// engine runs. With the engine stopped, a guess: the accounts that
// downloaded something in the last day or are downloading now (none known:
// no limit).
func (a *App) ixWorkingAccounts(ctx context.Context, now time.Time) []string {
	if ids, _, running := ixEngineRun(a); running && len(ids) > 0 {
		return ids
	}
	var out []string
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT DISTINCT account_id FROM tgdl_work WHERE status='processing' OR (status='completed' AND updated_at>=?)`, now.Add(-24*time.Hour).UnixMilli())
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

func (a *App) ixDeadlineSuspended(ctx context.Context, now time.Time) bool {
	var v string
	if a.db.Reader.QueryRowContext(ctx, `SELECT value FROM kv WHERE key='ix_deadline_suspend'`).Scan(&v) != nil {
		return false
	}
	until, _ := strconv.ParseInt(v, 10, 64)
	return now.UnixMilli() < until
}

// ixDeadlineLetGo unpauses set-aside items matching cond (on w) and marks
// them let go.
func (a *App) ixDeadlineLetGo(ctx context.Context, cond string) error {
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	if _, err = tx.ExecContext(ctx, `UPDATE tgdl_work SET paused=0 WHERE id IN (SELECT w.id FROM tgdl_work w JOIN ix_deferred x ON x.work_id=w.id
		WHERE x.released_at=0 AND w.status='pending' AND w.paused=1 AND `+cond+`)
		AND id NOT IN (SELECT work_id FROM ix_held WHERE released_at=0 AND reason<>'')`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ix_deferred SET released_at=? WHERE released_at=0 AND work_id IN (SELECT w.id FROM tgdl_work w WHERE `+cond+`)`, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ixDeadlineRelease lets go the set-aside items of bands up to band (all
// when band < 0).
func (a *App) ixDeadlineRelease(ctx context.Context, band int) error {
	limit := band
	if band < 0 {
		limit = ixBandCount - 1
	}
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE tgdl_work SET paused=0 WHERE status='pending' AND paused=1
		AND id IN (SELECT work_id FROM ix_deferred WHERE released_at=0 AND band<=?)
		AND id NOT IN (SELECT work_id FROM ix_held WHERE released_at=0 AND reason<>'')`, limit); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ix_deferred SET released_at=? WHERE released_at=0 AND band<=?`, time.Now().UnixMilli(), limit); err != nil {
		return err
	}
	return tx.Commit()
}

// ixDeferredWaiting is how many set-aside items still wait (held copies go
// after them).
func (a *App) ixDeferredWaiting(ctx context.Context) int {
	var n int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_deferred x JOIN tgdl_work w ON w.id=x.work_id WHERE x.released_at=0 AND w.status='pending'`).Scan(&n)
	return n
}

func (a *App) handleIxDeadline(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.ixDeadlineEstimate(r.Context(), true))
}

// ixDeadlineAlerts warns when the queue will not finish before deletion.
func ixDeadlineAlerts(d ixDeadlineInfo, now time.Time) []ixAlert {
	if d.Until == 0 || d.Finishes != "no" {
		return nil
	}
	days := float64(d.EtaSec) / 86400
	left := float64(d.Until-now.Unix()) / 86400
	var b strings.Builder
	b.WriteString("At the speed of the last day, the download queue needs about ")
	b.WriteString(strconv.FormatFloat(days, 'f', 1, 64))
	b.WriteString(" days; the account is deleted in ")
	b.WriteString(strconv.FormatFloat(left, 'f', 1, 64))
	b.WriteString(" days.")
	if ixDeadlineActive(d) {
		b.WriteString(" Files of private chats are going first, smaller ones before larger, so that the most of what only this account can read is saved.")
	}
	b.WriteString(" See Archive → Overview for what would be lost.")
	return []ixAlert{{Key: "deadline:" + now.In(ixTZ).Format("2006-01-02"), Once: true, Title: "The backup will not finish before deletion", Message: b.String()}}
}
