// Package engine owns account lifetimes and the durable Telegram work queue.
package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/rescue"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

type WorkStore struct {
	writer, reader *sql.DB
	emptyClaims    atomic.Int64 // claims that found nothing to take
}

var errFiltered = errors.New("queued message no longer matches download filters")
var ErrFiltered = errFiltered

func NewWorkStore(writer, reader *sql.DB) *WorkStore {
	return &WorkStore{writer: writer, reader: reader}
}

type Work struct {
	ID, MessageID, Generation                          int64
	AccountID, GroupID, GroupName, MediaType, FileName string
	FileSize                                           int64
	Attempts                                           int
	CreatedAt                                          int64
	ForceRefresh                                       bool
	Origin                                             string
	Identity                                           string
	// ClaimKey is the row's place in the claim order when it was claimed:
	// lower goes first (claimKey). Its waits for file and part slots are
	// ordered by it, and it yields to a better row while it only waits.
	ClaimKey int64
	body     []byte
}

func (w *Work) Message() (*tg.Message, error) {
	var message tg.Message
	if err := message.Decode(&bin.Buffer{Buf: w.body}); err != nil {
		return nil, fmt.Errorf("decode queued Telegram message: %w", err)
	}
	return &message, nil
}

// Enqueue commits before an update can advance its Telegram state. Identical
// updates from multiple accounts keep the original source account. A newer
// edit replaces the payload but keeps a running claim until it is released.
func (s *WorkStore) Enqueue(ctx context.Context, accountID string, target Target, message *tg.Message, updatePTS ...int) (int64, bool, error) {
	pts := 0
	if len(updatePTS) > 0 {
		pts = updatePTS[0]
	}
	return enqueueWork(ctx, s.writer, accountID, target, message, pts, "live")
}

type queueWriter interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func enqueueWork(ctx context.Context, db queueWriter, accountID string, target Target, message *tg.Message, pts int, origin string, repair ...bool) (int64, bool, error) {
	media, err := telegram.MessageAttachment(message)
	if err != nil {
		return 0, false, err
	}
	if accountID == "" {
		return 0, false, errors.New("queue account is required")
	}
	sourcePTS := int64(pts)
	if origin == "stories" {
		media.MessageID, err = telegram.StoryKey(message.ID)
		if err != nil {
			return 0, false, err
		}
		media.Type = "stories"
	}
	buffer := bin.Buffer{}
	if err = message.Encode(&buffer); err != nil {
		return 0, false, err
	}
	if target.ID == "" {
		target.ID = media.GroupID
	}
	if err := rescue.Observe(ctx, db, accountID, target.ID, message, origin); err != nil {
		return 0, false, err
	}
	if origin == "stories" {
		// Stories have no message PTS. Explicit reads serialize in the app and
		// enqueue in a transaction. Use a durable sequence independent of the
		// wall clock, including across account changes and process restarts.
		if err = db.QueryRowContext(ctx, `SELECT COALESCE((SELECT source_pts FROM tgdl_work WHERE group_id=? AND message_id=?),0)+1`, target.ID, media.MessageID).Scan(&sourcePTS); err != nil {
			return 0, false, err
		}
	}
	version := max(message.Date, message.EditDate)
	_, channel := message.PeerID.(*tg.PeerChannel)
	comparable := channel || origin == "stories"
	now := time.Now().UnixMilli()
	verifyExisting := len(repair) > 0 && repair[0] && origin != "live"
	var id int64
	err = db.QueryRowContext(ctx, `INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,source_pts,identity,media_type,file_name,file_size,body,created_at,updated_at,origin,ttl_seconds,sched_order,dc)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,COALESCE((SELECT sched_base FROM tgdl_group_rank WHERE group_id=?),`+unrankedOrderSQL+`),?)
 ON CONFLICT(group_id,message_id) DO UPDATE SET
 account_id=excluded.account_id,group_name=excluded.group_name,version=excluded.version,
 source_pts=CASE WHEN excluded.version=tgdl_work.version AND excluded.identity=tgdl_work.identity AND (? OR excluded.account_id=tgdl_work.account_id)
 THEN MAX(tgdl_work.source_pts,excluded.source_pts) ELSE excluded.source_pts END,identity=excluded.identity,
 media_type=excluded.media_type,file_name=excluded.file_name,file_size=excluded.file_size,body=excluded.body,dc=excluded.dc,
 ttl_seconds=MAX(tgdl_work.ttl_seconds,excluded.ttl_seconds),
 generation=tgdl_work.generation+1,attempts=0,retry_at=0,error=NULL,refresh_required=0,updated_at=excluded.updated_at,
 origin=CASE WHEN excluded.origin='url' THEN 'url' WHEN tgdl_work.origin<>'live' THEN tgdl_work.origin ELSE excluded.origin END,
 status=CASE WHEN tgdl_work.status='processing' THEN 'processing' ELSE 'pending' END
 WHERE excluded.version>tgdl_work.version OR (excluded.version=tgdl_work.version AND excluded.identity<>tgdl_work.identity
 AND excluded.source_pts>tgdl_work.source_pts AND (? OR excluded.account_id=tgdl_work.account_id))
 OR (excluded.origin<>'live' AND excluded.version=tgdl_work.version AND excluded.identity=tgdl_work.identity
 AND ((tgdl_work.origin='live' AND tgdl_work.status='pending')
 OR (excluded.origin='url' AND tgdl_work.origin<>'url' AND tgdl_work.status IN ('pending','processing'))))
 OR (? AND excluded.version=tgdl_work.version AND excluded.identity=tgdl_work.identity AND tgdl_work.status IN ('completed','skipped','failed'))
 RETURNING id`, accountID, target.ID, target.Name, media.MessageID, version, sourcePTS, media.Identity.Key(), media.Type, media.Name, media.Identity.Size, buffer.Buf, now, now, origin, telegram.MessageTTL(message), target.ID, max(media.DC, 0), comparable, comparable, verifyExisting).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// A repeated identity can still carry a newer ordering watermark. Keep
		// it without requeueing, otherwise an intermediate delayed edit could
		// replace a later observation of the current attachment.
		_, err = db.ExecContext(ctx, `UPDATE tgdl_work SET source_pts=MAX(source_pts,?)
 WHERE group_id=? AND message_id=? AND version=? AND identity=? AND (? OR account_id=?)`, sourcePTS, target.ID, media.MessageID, version, media.Identity.Key(), comparable, accountID)
		return 0, false, err
	}
	return id, err == nil, err
}

// claimPriority is the lane of a queue row: self-destructing media first (it
// may vanish before its turn), then explicit requests (a link someone pasted),
// then live downloads and the history of ranked groups, then the history of
// the other groups. Within a lane rows go by sched_order, then id. It matches
// idx_tgdl_work_priority and idx_tgdl_work_claim exactly (store/migrate.go).
//
// sched_order is what the app's scheduler keeps up to date (app/ix_sched.go):
// UnrankedOrder for a file no ranked group needs; lower for one a ranked group
// needs, whichever group's message brought it to the queue. With no group
// ranked the order is the one 2.x's lanes gave.
const claimPriority = `CASE WHEN ttl_seconds>0 THEN 0 WHEN origin NOT IN ('live','history') THEN 1 WHEN origin='history' AND sched_order>=` + unrankedOrderSQL + ` THEN 3 ELSE 2 END`

// UnrankedOrder is the sched_order of a file no ranked group needs. It is
// spelled out in store/engine_schema.sql and store/migrate.go.
const UnrankedOrder = 131072

const unrankedOrderSQL = `131072`

// The big-file lane. A file of BigFileSize or more can take hours under
// Telegram's limits, and every file of the account shares the pacer's
// window of parts in flight (telegram/pacer.go): with many of them at once,
// all of them crawl and so do the small files behind them. So at most
// BigFileLane() of them download at once and the other workers take smaller
// files. When nothing smaller can be claimed, twice as many may go, so that
// a queue holding only big files still keeps the window busy; never more,
// so that small files arriving later still find free workers. Rows of old
// versions are ordinary pending rows: the lane applies to them as to any.
// Self-destructing media is never held back (it may vanish). 0 switches
// the lane off.
//
// It is part of Claim's one statement (the writer is a single connection),
// so two workers cannot both take the last place, and it leaves the claim
// order (claimPriority, then id) as it is: a big file over the limit is
// only skipped. See claimSelect for how the statement finds its row.
const BigFileSize int64 = 1 << 30

// DefaultBigFileLane is how many big files download at once unless set.
const DefaultBigFileLane = 2

var bigFileLane atomic.Int64

func init() { bigFileLane.Store(DefaultBigFileLane) }

// SetBigFileLane sets how many files of BigFileSize or more may download at
// once (0: no limit).
func SetBigFileLane(n int) { bigFileLane.Store(int64(max(0, n))) }

// BigFileLane is how many files of BigFileSize or more may download at once
// (0: no limit).
func BigFileLane() int { return int(bigFileLane.Load()) }

// bigRowSQL is true for a row of the big-file lane. It is spelled as in
// idx_tgdl_work_claim (store/migrate.go), which SQLite only uses for the
// very same expression; TestClaimIndexMatchesTheLane checks the two agree.
var bigRowSQL = `(ttl_seconds=0 AND file_size>=` + strconv.FormatInt(BigFileSize, 10) + `)`

// A row's place in the claim order as one number, comparable across the two
// parts of the big-file lane: lane<<58 | sched_order<<40 | id. Ids stay far
// below 2^40, sched_order below 2^18.
const (
	claimKeyBase   int64 = 1 << 40
	claimOrderBase int64 = 1 << 40
	claimLaneBase  int64 = 1 << 58
)

// claimKey is that number in SQL.
var claimKey = claimPriority + `*` + strconv.FormatInt(claimLaneBase, 10) + `+sched_order*` + strconv.FormatInt(claimOrderBase, 10) + `+id`

// claimOrderSQL is the ORDER BY of a walk of the indexes.
const claimOrderSQL = claimPriority + `,sched_order,id`

// claimFree: a file is transferred by one worker at a time. While a row of
// the same Telegram file is being downloaded the others are left in the
// queue: taking one would only hold a worker on the identity lock in
// download.Library until the first is done, and then link to it. They are
// claimed afterwards, take no bytes, and credit their groups.
const claimFree = ` AND (identity='' OR NOT EXISTS (SELECT 1 FROM tgdl_work active WHERE active.status='processing' AND active.identity=tgdl_work.identity))`

// claimSelect is the SELECT giving the id Claim takes (NULL: none), with
// its arguments in order.
//
// Without the lane it walks idx_tgdl_work_priority in claim order. With
// the lane, the best row of each part of idx_tgdl_work_claim (pending rows
// split into big and the others) is found from the index, the big part
// only while the lane has room, and the better of the two is taken: the
// same row as one walk of the whole queue in claim order would give, but a
// queue full of big files waiting for the lane is not walked row by row
// on every claim that finds nothing (some 20 ms at 50,000 rows, on the
// only writer connection, for each idle worker's poll). The account is
// written +account_id so that a single account does not pick the index
// on accounts (idx_tgdl_work_pending) and sort every pending row.
func claimSelect(accounts []string, now time.Time, manualOnly bool) (string, []any) {
	return claimSelectAvoiding(accounts, now, manualOnly, nil)
}

// claimSelectAvoiding is claimSelect leaving out rows of the servers and
// sizes in full (ServerKey): their slots are all taken.
func claimSelectAvoiding(accounts []string, now time.Time, manualOnly bool, full []int) (string, []any) {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(accounts)), ",")
	avoid := avoidServersSQL(full)
	base := func(args []any) (string, []any) {
		for _, id := range accounts {
			args = append(args, id)
		}
		where := `status='pending' AND paused=0 AND +account_id IN (` + marks + `) AND retry_at<=?` + claimFree + avoid
		if manualOnly {
			where += ` AND origin<>'live'`
		}
		return where, append(args, now.UnixMilli())
	}
	const open = `(SELECT paused FROM tgdl_queue_state WHERE id=1)=0`
	n := BigFileLane()
	if n <= 0 {
		where, args := base(nil)
		return `SELECT id FROM tgdl_work WHERE ` + where + ` AND ` + open + ` ORDER BY ` + claimOrderSQL + ` LIMIT 1`, args
	}
	key := claimKey
	order := ` ORDER BY ` + claimOrderSQL + ` LIMIT 1`
	var args []any
	small, args := base(args)
	smallSQL := `SELECT ` + key + ` FROM tgdl_work WHERE ` + small + ` AND ` + bigRowSQL + `=0` + order
	exists, args := base(args)
	args = append(args, n, 2*n)
	big, args := base(args)
	// The lane's room is checked before the big part is looked at (a CASE,
	// not a WHERE term, which SQLite would test on every row it walks).
	bigSQL := `CASE WHEN (SELECT COUNT(*) FROM tgdl_work WHERE status='processing' AND ` + bigRowSQL + `)
	 < CASE WHEN EXISTS (SELECT 1 FROM tgdl_work WHERE ` + exists + ` AND ` + bigRowSQL + `=0) THEN ? ELSE ? END
	 THEN (SELECT ` + key + ` FROM tgdl_work WHERE ` + big + ` AND ` + bigRowSQL + `=1` + order + `) END`
	return `SELECT MIN(k) % ` + strconv.FormatInt(claimKeyBase, 10) + ` FROM (SELECT (` + smallSQL + `) AS k UNION ALL SELECT ` + bigSQL + `) WHERE ` + open, args
}

// ServerKey names a server's file slots of one size: dc*2, plus 1 for files
// of telegram.DownloadLargeFile or more.
func ServerKey(dc int, large bool) int {
	if large {
		return dc*2 + 1
	}
	return dc * 2
}

const serverKeySQL = `(dc*2+(file_size>=20971520))`

// avoidServersSQL is the claim condition leaving out the full servers.
// Rows of a server not known yet (dc 0) are never left out. The keys are
// numbers made here, not input.
func avoidServersSQL(full []int) string {
	if len(full) == 0 {
		return ""
	}
	keys := make([]string, 0, len(full))
	for _, k := range full {
		keys = append(keys, strconv.Itoa(k))
	}
	return ` AND ` + serverKeySQL + ` NOT IN (` + strings.Join(keys, ",") + `)`
}

// FullServers are the servers and sizes (ServerKey) whose files downloading
// now fill their slots: small and large slots per server, as Telegram's
// configuration gives them. Files on other servers are claimed instead of
// waiting for one of those (ix.12.4).
func (s *WorkStore) FullServers(ctx context.Context, small, large int) ([]int, error) {
	if small <= 0 || large <= 0 {
		return nil, nil
	}
	rows, err := s.reader.QueryContext(ctx, `SELECT dc, file_size>=20971520, COUNT(*) FROM tgdl_work WHERE status='processing' AND dc>0 GROUP BY 1,2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var full []int
	for rows.Next() {
		var dc, n int
		var big bool
		if err := rows.Scan(&dc, &big, &n); err != nil {
			return nil, err
		}
		if limit := map[bool]int{false: small, true: large}[big]; n >= limit {
			full = append(full, ServerKey(dc, big))
		}
	}
	return full, rows.Err()
}

// ClaimAvoiding is Claim leaving out the full servers (FullServers).
func (s *WorkStore) ClaimAvoiding(ctx context.Context, accounts []string, now time.Time, manualOnly bool, full []int) (*Work, error) {
	if len(accounts) == 0 {
		return nil, nil
	}
	pick, pickArgs := claimSelectAvoiding(accounts, now, manualOnly, full)
	return s.claim(ctx, now, pick, pickArgs)
}

// ClaimSmallerAvoiding is ClaimSmaller leaving out the full servers.
func (s *WorkStore) ClaimSmallerAvoiding(ctx context.Context, accounts []string, now time.Time, manualOnly bool, below int64, full []int) (*Work, error) {
	if len(accounts) == 0 {
		return nil, nil
	}
	where, args := smallerWhere(accounts, now, manualOnly, below)
	return s.claim(ctx, now, `SELECT id FROM tgdl_work WHERE `+where+avoidServersSQL(full)+` ORDER BY `+claimOrderSQL+` LIMIT 1`, args)
}

// FillServers records the server of queued rows written before ix.12.4
// (dc 0), a batch at a time, from their stored messages; rows whose message
// says none get -1 so they are not read again. claimable: only rows a worker
// could take now (not paused). It returns the rows done.
func (s *WorkStore) FillServers(ctx context.Context, batch int, claimable ...bool) (int, error) {
	only := ""
	if len(claimable) > 0 && claimable[0] {
		only = " AND paused=0"
	}
	rows, err := s.reader.QueryContext(ctx, `SELECT id, body FROM tgdl_work WHERE dc=0 AND status IN ('pending','processing') AND length(body)>0`+only+` LIMIT ?`, batch)
	if err != nil {
		return 0, err
	}
	type row struct {
		id int64
		dc int
	}
	var found []row
	for rows.Next() {
		var id int64
		var body []byte
		if err := rows.Scan(&id, &body); err != nil {
			rows.Close()
			return 0, err
		}
		dc := -1
		var msg tg.Message
		if msg.Decode(&bin.Buffer{Buf: body}) == nil {
			if media, err := telegram.MessageAttachment(&msg); err == nil && media.DC > 0 {
				dc = media.DC
			}
		}
		found = append(found, row{id, dc})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(found) == 0 {
		return 0, nil
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, r := range found {
		if _, err := tx.ExecContext(ctx, `UPDATE tgdl_work SET dc=? WHERE id=? AND dc=0`, r.dc, r.id); err != nil {
			return 0, err
		}
	}
	return len(found), tx.Commit()
}

// Claim changes pending -> processing in one SQLite statement. Only accounts
// whose authenticated Run callback is active can claim their own work.
func (s *WorkStore) Claim(ctx context.Context, accounts []string, now time.Time, manualOnly ...bool) (*Work, error) {
	if len(accounts) == 0 {
		return nil, nil
	}
	pick, pickArgs := claimSelect(accounts, now, len(manualOnly) > 0 && manualOnly[0])
	return s.claim(ctx, now, pick, pickArgs)
}

// smallerWhere is the claimable rows under below bytes, with their arguments.
func smallerWhere(accounts []string, now time.Time, manualOnly bool, below int64) (string, []any) {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(accounts)), ",")
	args := make([]any, 0, len(accounts)+2)
	for _, id := range accounts {
		args = append(args, id)
	}
	args = append(args, now.UnixMilli(), below)
	where := `status='pending' AND paused=0 AND +account_id IN (` + marks + `) AND retry_at<=? AND file_size<?` + claimFree
	if manualOnly {
		where += ` AND origin<>'live'`
	}
	return where + ` AND (SELECT paused FROM tgdl_queue_state WHERE id=1)=0`, args
}

// ClaimSmaller claims the best row under below bytes, in the claim order. A
// worker takes it after its large file gave up waiting for a slot to make room
// (telegram.ErrDownloadMakeRoom).
func (s *WorkStore) ClaimSmaller(ctx context.Context, accounts []string, now time.Time, manualOnly bool, below int64) (*Work, error) {
	if len(accounts) == 0 {
		return nil, nil
	}
	where, args := smallerWhere(accounts, now, manualOnly, below)
	return s.claim(ctx, now, `SELECT id FROM tgdl_work WHERE `+where+` ORDER BY `+claimOrderSQL+` LIMIT 1`, args)
}

// SmallerWaiting says whether a row under below bytes could be claimed now.
func (s *WorkStore) SmallerWaiting(ctx context.Context, accounts []string, now time.Time, manualOnly bool, below int64) (bool, error) {
	if len(accounts) == 0 {
		return false, nil
	}
	where, args := smallerWhere(accounts, now, manualOnly, below)
	var n int
	err := s.reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tgdl_work WHERE `+where+`)`, args...).Scan(&n)
	return n > 0, err
}

// ClaimExpiring claims only self-destructing media: what may vanish goes on even
// while an update waits for the downloads to finish.
func (s *WorkStore) ClaimExpiring(ctx context.Context, accounts []string, now time.Time, manualOnly bool) (*Work, error) {
	if len(accounts) == 0 {
		return nil, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(accounts)), ",")
	args := make([]any, 0, len(accounts)+1)
	for _, id := range accounts {
		args = append(args, id)
	}
	args = append(args, now.UnixMilli())
	where := `status='pending' AND paused=0 AND ttl_seconds>0 AND +account_id IN (` + marks + `) AND retry_at<=?` + claimFree
	if manualOnly {
		where += ` AND origin<>'live'`
	}
	pick := `SELECT id FROM tgdl_work WHERE ` + where + ` AND (SELECT paused FROM tgdl_queue_state WHERE id=1)=0 ORDER BY id LIMIT 1`
	return s.claim(ctx, now, pick, args)
}

func (s *WorkStore) claim(ctx context.Context, now time.Time, pick string, pickArgs []any) (*Work, error) {
	args := append([]any{now.UnixMilli()}, pickArgs...)
	row := s.writer.QueryRowContext(ctx, `UPDATE tgdl_work SET status='processing',claim_generation=generation,attempts=attempts+1,updated_at=?
	 WHERE id=(`+pick+`)
 RETURNING id,account_id,group_id,group_name,message_id,generation,media_type,file_name,file_size,body,attempts,created_at,refresh_required,origin,identity,`+claimKey, args...)
	w := new(Work)
	if err := row.Scan(&w.ID, &w.AccountID, &w.GroupID, &w.GroupName, &w.MessageID, &w.Generation, &w.MediaType, &w.FileName, &w.FileSize, &w.body, &w.Attempts, &w.CreatedAt, &w.ForceRefresh, &w.Origin, &w.Identity, &w.ClaimKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.emptyClaims.Add(1)
			return nil, nil
		}
		return nil, err
	}
	return w, nil
}

// Outranked reports whether a worker could claim a row that goes before key
// now. A worker whose file only waits for a file slot gives its place up then:
// nothing has been transferred yet, and it would hold a worker from better
// work for as long as the slots stay full.
func (s *WorkStore) Outranked(ctx context.Context, accounts []string, now time.Time, key int64, manualOnly bool) (bool, error) {
	if len(accounts) == 0 {
		return false, nil
	}
	pick, args := claimSelect(accounts, now, manualOnly)
	var best sql.NullInt64
	err := s.reader.QueryRowContext(ctx, `SELECT `+claimKey+` FROM tgdl_work WHERE id=(`+pick+`)`, args...).Scan(&best)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return best.Valid && best.Int64 < key, nil
}

// IdentitySaved reports whether the library holds this Telegram file. Such a
// row needs no fresh file reference: it takes no bytes, it only links to the
// saved file. A row whose file proves missing gets a second attempt, which
// refreshes as usual.
func (s *WorkStore) IdentitySaved(ctx context.Context, identity string) bool {
	parts := strings.Split(identity, "\x00")
	if len(parts) != 3 {
		return false
	}
	var saved bool
	if err := s.reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM downloads WHERE telegram_media_kind=? AND telegram_media_id=?)`, parts[0], parts[1]).Scan(&saved); err != nil {
		return false
	}
	return saved
}

// Finish cannot acknowledge an edit that arrived during an older claim. A
// shutdown cancellation preserves pending work instead of recording failure.
func (s *WorkStore) Finish(ctx context.Context, w *Work, cause error, cancelled bool, retryAt time.Time) error {
	status := "completed"
	var detail any
	if cause != nil {
		detail = cause.Error()
		status = "failed"
	}
	if errors.Is(cause, errFiltered) {
		status = "skipped"
	}
	if cancelled || !retryAt.IsZero() {
		status = "pending"
	}
	when := int64(0)
	if !retryAt.IsZero() {
		when = retryAt.UnixMilli()
	}
	_, err := s.writer.ExecContext(ctx, `UPDATE tgdl_work SET
 status=CASE WHEN generation<>? THEN 'pending' ELSE ? END,
 error=CASE WHEN generation<>? THEN NULL ELSE ? END,
 retry_at=CASE WHEN generation<>? THEN 0 ELSE ? END,
 body=CASE WHEN generation=? AND ? IN ('completed','skipped') THEN X'' ELSE body END,
 attempts=CASE WHEN generation=? AND ? THEN MAX(0,attempts-1) ELSE attempts END,
 claim_generation=NULL,updated_at=?
 WHERE id=? AND status='processing' AND claim_generation=?`, w.Generation, status, w.Generation, detail, w.Generation, when, w.Generation, status, w.Generation, cancelled, time.Now().UnixMilli(), w.ID, w.Generation)
	return err
}

// IsCurrent closes the interval between claiming work and registering its
// cancellation function. Later edits cancel through the active map.
func (s *WorkStore) IsCurrent(ctx context.Context, w *Work) (bool, error) {
	var current bool
	err := s.reader.QueryRowContext(ctx, `SELECT generation=? AND status='processing' AND claim_generation=? FROM tgdl_work WHERE id=?`, w.Generation, w.Generation, w.ID).Scan(&current)
	return current, err
}

// Refresh persists renewed file references. If Telegram now reports another
// attachment, replace only this claim's generation and let the next claim own
// the new media; an update arriving during the RPC always wins.
func (s *WorkStore) Refresh(ctx context.Context, w *Work, message *tg.Message) (bool, error) {
	media, err := telegram.MessageAttachment(message)
	if err != nil {
		return false, err
	}
	if w.Origin == "stories" {
		media.Type = "stories"
	}
	buf := bin.Buffer{}
	if err = message.Encode(&buf); err != nil {
		return false, err
	}
	var generation int64
	err = s.writer.QueryRowContext(ctx, `UPDATE tgdl_work SET
 generation=generation+CASE WHEN identity<>? THEN 1 ELSE 0 END,
 attempts=CASE WHEN identity<>? THEN 0 ELSE attempts END,
 version=MAX(version,?),identity=?,media_type=?,file_name=?,file_size=?,body=?,refresh_required=0,updated_at=?
 WHERE id=? AND generation=? AND claim_generation=? AND status='processing' RETURNING generation`,
		media.Identity.Key(), media.Identity.Key(), max(message.Date, message.EditDate), media.Identity.Key(), media.Type, media.Name, media.Identity.Size, buf.Buf, time.Now().UnixMilli(), w.ID, w.Generation, w.Generation).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return generation == w.Generation, err
}

// Recover runs under exclusive engine ownership before any worker starts.
func (s *WorkStore) Recover(ctx context.Context) error {
	_, err := s.writer.ExecContext(ctx, `UPDATE tgdl_work SET status='pending',claim_generation=NULL WHERE status='processing'`)
	return err
}

func (s *WorkStore) Counts(ctx context.Context) (map[string]int, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT status,count(*) FROM tgdl_work GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{"pending": 0, "processing": 0, "completed": 0, "failed": 0}
	for rows.Next() {
		var status string
		var count int
		if err = rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		out[status] = count
	}
	return out, rows.Err()
}

func (s *WorkStore) QueuePaused(ctx context.Context) (bool, error) {
	var paused bool
	err := s.reader.QueryRowContext(ctx, `SELECT paused<>0 FROM tgdl_queue_state WHERE id=1`).Scan(&paused)
	return paused, err
}

func (s *WorkStore) SetQueuePaused(ctx context.Context, paused bool) error {
	value := 0
	if paused {
		value = 1
	}
	now := time.Now().UnixMilli()
	if _, err := s.writer.ExecContext(ctx, `UPDATE tgdl_queue_state SET paused=?,updated_at=? WHERE id=1`, value, now); err != nil {
		return err
	}
	if !paused {
		// The Node queue's resume-all action also clears individual pauses. Keep
		// that durable behavior so a restart cannot leave a row unexpectedly
		// hidden after the operator resumed the whole queue.
		_, err := s.writer.ExecContext(ctx, `UPDATE tgdl_work SET paused=0,updated_at=? WHERE paused<>0 AND status IN ('pending','processing')`, now)
		return err
	}
	return nil
}

func (s *WorkStore) PausedCount(ctx context.Context) (int, error) {
	var count int
	err := s.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM tgdl_work WHERE paused<>0 AND status IN ('pending','processing')`).Scan(&count)
	return count, err
}

func (s *WorkStore) PauseJob(ctx context.Context, key string) (bool, error) {
	result, err := s.writer.ExecContext(ctx, `UPDATE tgdl_work SET paused=1,updated_at=? WHERE group_id||'_'||message_id=? AND status IN ('pending','processing')`, time.Now().UnixMilli(), key)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

func (s *WorkStore) ResumeJob(ctx context.Context, key string) (bool, error) {
	result, err := s.writer.ExecContext(ctx, `UPDATE tgdl_work SET paused=0,updated_at=? WHERE group_id||'_'||message_id=? AND paused<>0 AND status IN ('pending','processing')`, time.Now().UnixMilli(), key)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

func (s *WorkStore) CancelAllQueued(ctx context.Context) (int, error) {
	result, err := s.writer.ExecContext(ctx, `UPDATE tgdl_work SET status='skipped',paused=0,claim_generation=NULL,body=X'',error='cancelled by queue action',updated_at=? WHERE status='pending'`, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

func (s *WorkStore) CancelJob(ctx context.Context, key string) (bool, error) {
	result, err := s.writer.ExecContext(ctx, `UPDATE tgdl_work SET status='skipped',paused=0,claim_generation=NULL,body=X'',error='cancelled by queue action',updated_at=? WHERE group_id||'_'||message_id=? AND status IN ('pending','processing')`, time.Now().UnixMilli(), key)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

func (s *WorkStore) RetryJob(ctx context.Context, key string) (bool, error) {
	result, err := s.writer.ExecContext(ctx, `UPDATE tgdl_work SET status='pending',paused=0,claim_generation=NULL,attempts=0,retry_at=0,error=NULL,updated_at=? WHERE group_id||'_'||message_id=? AND status='failed' AND length(body)>0`, time.Now().UnixMilli(), key)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

func (s *WorkStore) RetryAll(ctx context.Context) (int, error) {
	result, err := s.writer.ExecContext(ctx, `UPDATE tgdl_work SET status='pending',paused=0,claim_generation=NULL,attempts=0,retry_at=0,error=NULL,updated_at=? WHERE status='failed' AND length(body)>0`, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// RequeueLost puts a message back in the queue whose download had counted
// as done but whose file was lost before it reached the library (a staged
// copy removed from the staging folder). The finished item gets message's
// body back and goes again; an item still waiting or running is left as it
// is; a removed one is queued anew. queued is false when the item was
// skipped (filtered out or cancelled since) and stays so, and when no
// download of the message is known any more (tgdl_message_generations has
// no row: its group was purged), which is never queued again.
func (s *WorkStore) RequeueLost(ctx context.Context, accountID string, target Target, message *tg.Message, origin string) (queued bool, err error) {
	media, err := telegram.MessageAttachment(message)
	if err != nil {
		return false, err
	}
	if target.ID == "" {
		target.ID = media.GroupID
	}
	if origin == "stories" {
		if media.MessageID, err = telegram.StoryKey(message.ID); err != nil {
			return false, err
		}
	}
	buffer := bin.Buffer{}
	if err = message.Encode(&buffer); err != nil {
		return false, err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var known bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tgdl_message_generations WHERE group_id=? AND message_id=?)`, target.ID, media.MessageID).Scan(&known); err != nil {
		return false, err
	}
	if !known {
		return false, nil
	}
	var status string
	err = tx.QueryRowContext(ctx, `UPDATE tgdl_work SET status=CASE WHEN status IN ('completed','failed') THEN 'pending' ELSE status END,
 body=CASE WHEN status IN ('completed','failed') THEN ? ELSE body END,
 generation=CASE WHEN status IN ('completed','failed') THEN generation+1 ELSE generation END,
 attempts=CASE WHEN status IN ('completed','failed') THEN 0 ELSE attempts END,
 retry_at=CASE WHEN status IN ('completed','failed') THEN 0 ELSE retry_at END,
 error=CASE WHEN status IN ('completed','failed') THEN NULL ELSE error END,
 paused=CASE WHEN status IN ('completed','failed') THEN 0 ELSE paused END,
 updated_at=? WHERE group_id=? AND message_id=? RETURNING status`, buffer.Buf, time.Now().UnixMilli(), target.ID, media.MessageID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		if accountID == "" {
			return false, errors.New("the queue item is gone and its account is unknown")
		}
		if _, _, err = enqueueWork(ctx, tx, accountID, target, message, 0, origin); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	return status != "skipped", tx.Commit()
}

func (s *WorkStore) DismissJob(ctx context.Context, key string) (bool, error) {
	result, err := s.writer.ExecContext(ctx, `DELETE FROM tgdl_work WHERE group_id||'_'||message_id=? AND status IN ('completed','failed','skipped')`, key)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

func (s *WorkStore) ClearFinished(ctx context.Context) error {
	if _, err := s.writer.ExecContext(ctx, `DELETE FROM tgdl_work WHERE status IN ('completed','failed','skipped')`); err != nil {
		return err
	}
	_, err := s.writer.ExecContext(ctx, `DELETE FROM kv WHERE key='queue_history'`)
	return err
}
