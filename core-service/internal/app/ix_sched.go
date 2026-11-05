package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
)

// Which group's files go first, and what a file shared between groups counts
// for.
//
// A person ranks groups (tgdl_group_rank); the ranking outlives rescans and
// their rows. It takes effect in every step of a file's way:
//
//   - discovery: ranked groups go first in both rescan lanes, and a ranked
//     group takes a lane from a lower one, which carries on later from where
//     it stopped (ixQueueStep);
//   - queue admission: rows a ranked group outranks do not fill its backfill's
//     queue limit (ixAdmissionCredit);
//   - download selection: every queue row carries sched_order, its place in
//     the claim order (engine.claimPriority). This step keeps it up to date;
//     the claim stays an index walk, so ranking costs a worker's poll nothing;
//   - the wait for a file or part slot goes by the same order, and a file
//     waiting for its slot gives way to better work (telegram.WithDownloadRank);
//   - the deadline order's set-asides apply only to files no ranked group
//     needs, and its size bands become the tie-break within a rank.
//
// A file needed by several groups is one piece of work: it goes at the best
// rank of any group that needs it, whichever group's message queued it and
// whether or not that group has queued it yet. The engine transfers it once
// (claimFree), and every message then links to the saved file. Probable copies
// held behind a better copy pass their group's rank to that copy. Rows of a
// file the library already has take no bytes: they go at once, so their groups
// are credited and they stop filling the queue. Nothing here releases a hold:
// a held file stays where the copies logic puts it (ix_order.go).
//
// Active transfers keep the place they were claimed with: they are never
// stopped to make a ranking look responsive, since without resume across
// attempts that would throw away the bytes they already fetched.
//
// Policies:
//
//	order   finish in order: rank 1 first, then rank 2 ...
//	spread  groups take turns: the group with the least coverage for its rank
//	        goes first, so no important group stays almost empty
const (
	ixGroupPolicyOrder  = "order"
	ixGroupPolicySpread = "spread"

	ixSchedEvery   = time.Minute
	ixSpreadEvery  = 10 * time.Minute // how often spread may change who goes first
	ixRankMax      = 16000            // keeps every order below engine.UnrankedOrder
	ixSchedBatch   = 500              // rows rewritten in one transaction
	ixPreemptAfter = 2 * time.Minute  // a rescan pass runs at least this long before giving way
	ixFreeOrder    = 0                // rows of a file already saved
)

// ixRankBase is where a position's files start in sched_order. Position 0 is
// the first rank; each position has room for the deadline bands, and the first
// slot is left to ixFreeOrder.
func ixRankBase(position int) int { return (position + 1) * ixBandCount }

type ixRankedGroup struct {
	GroupID string `json:"groupId"`
	Name    string `json:"name"`
	Rank    int    `json:"rank"` // 1 is first
	Base    int    `json:"-"`
}

type ixSchedBox struct {
	mu       sync.Mutex
	spreadAt time.Time
	wake     chan struct{}
	shared   atomic.Int64 // rows that take no transfer of their own (ixSharedWaiting)

	creditMu    sync.Mutex
	creditGroup string
	creditAt    time.Time
	creditN     int
}

var ixSchedState sync.Map // *App -> *ixSchedBox

func (a *App) ixSched() *ixSchedBox {
	v, _ := ixSchedState.LoadOrStore(a, &ixSchedBox{wake: make(chan struct{}, 1)})
	return v.(*ixSchedBox)
}

func (a *App) ixSchedWake() {
	select {
	case a.ixSched().wake <- struct{}{}:
	default:
	}
}

func (a *App) ixRunSched(ctx context.Context) {
	box := a.ixSched()
	for {
		if ixVdupBackground.Load() {
			if err := a.ixSchedStep(ctx); err != nil && ctx.Err() == nil {
				a.ixLog("group priorities: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(ixSchedEvery):
		case <-box.wake:
		}
	}
}

// ixRanks is the ranked groups, first rank first.
func (a *App) ixRanks(ctx context.Context) ([]ixRankedGroup, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, name, rank, sched_base FROM tgdl_group_rank ORDER BY rank, group_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ixRankedGroup
	for rows.Next() {
		var g ixRankedGroup
		if err := rows.Scan(&g.GroupID, &g.Name, &g.Rank, &g.Base); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ixRankBases is each ranked group's current place: lower goes first. Under
// spread it follows coverage, not the rank a person gave.
func (a *App) ixRankBases(ctx context.Context) map[string]int {
	ranks, err := a.ixRanks(ctx)
	if err != nil || len(ranks) == 0 {
		return nil
	}
	out := make(map[string]int, len(ranks))
	for _, g := range ranks {
		out[g.GroupID] = g.Base
	}
	return out
}

// ixSetRanks makes ids the whole ranked list, in that order. names may hold
// the groups' names; a group already ranked keeps the one it has otherwise.
func (a *App) ixSetRanks(ctx context.Context, ids []string, names map[string]string) error {
	if len(ids) > ixRankMax {
		return errors.New("too many ranked groups")
	}
	old, err := a.ixRanks(ctx)
	if err != nil {
		return err
	}
	had := map[string]string{}
	for _, g := range old {
		had[g.GroupID] = g.Name
	}
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM tgdl_group_rank`); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	seen := map[string]bool{}
	position := 0
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := names[id]
		if name == "" {
			name = had[id]
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO tgdl_group_rank(group_id,rank,sched_base,name,updated_at) VALUES(?,?,?,?,?)`, id, position+1, ixRankBase(position), name, now); err != nil {
			return err
		}
		position++
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.ixAfterRankChange(ctx)
	return nil
}

// ixRankSet puts a group on the ranked list (after the others) or takes it off.
func (a *App) ixRankSet(ctx context.Context, id, name string, on bool) error {
	ranks, err := a.ixRanks(ctx)
	if err != nil {
		return err
	}
	var ids []string
	found := false
	for _, g := range ranks {
		if g.GroupID == id {
			found = true
			if !on {
				continue
			}
		}
		ids = append(ids, g.GroupID)
	}
	if on && !found {
		ids = append(ids, id)
	}
	if on == found {
		return nil
	}
	return a.ixSetRanks(ctx, ids, map[string]string{id: name})
}

// ixAfterRankChange makes a change felt now: the rows at once (a person is
// watching), and the rescan lanes on their next step.
func (a *App) ixAfterRankChange(ctx context.Context) {
	box := a.ixSched()
	box.mu.Lock()
	box.spreadAt = time.Time{}
	box.mu.Unlock()
	a.ixCaches().priorities.clear()
	if err := a.ixSchedStep(ctx); err != nil && ctx.Err() == nil {
		a.ixLog("group priorities: %v", err)
	}
	a.ixWake()
}

// ixRanksFromStars ranks the groups starred in the rescan queue by an earlier
// version, once, in queue order.
func (a *App) ixRanksFromStars(ctx context.Context) error {
	const flag = "ix_rank_stars_done"
	var done int
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM kv WHERE key=?`, flag).Scan(&done); err != nil || done > 0 {
		return err
	}
	q, err := a.ixLoadQueue(ctx)
	if err != nil {
		return err
	}
	ranks, err := a.ixRanks(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(ranks))
	seen := map[string]bool{}
	for _, g := range ranks {
		ids = append(ids, g.GroupID)
		seen[g.GroupID] = true
	}
	names := map[string]string{}
	changed := false
	for _, it := range q.Items {
		if it.Priority && !seen[it.GroupID] {
			ids = append(ids, it.GroupID)
			names[it.GroupID] = it.Name
			seen[it.GroupID] = true
			changed = true
		}
	}
	now := time.Now().UnixMilli()
	if _, err = a.db.Writer.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at) VALUES(?,'1',?) ON CONFLICT(key) DO NOTHING`, flag, now); err != nil {
		return err
	}
	if changed {
		return a.ixSetRanks(ctx, ids, names)
	}
	return nil
}

// ixSchedRow is what the step needs to know of a pending queue row.
type ixSchedRow struct {
	id       int64
	group    string
	identity string
	order    int
	size     int64
	free     bool // no transfer of its own: paused or not, its file is saved
	blocked  bool // a person paused it: it asks for nothing
	held     bool // waits with the copies or for its better quality
	deferred bool
}

// ixSchedStep brings every pending row's sched_order up to date. It reads on
// the reader, decides in memory, and writes only the rows that changed, in
// short transactions: the writer is the one every download needs.
func (a *App) ixSchedStep(ctx context.Context) error {
	if a.ixIsGone() {
		return nil // read-only: nothing downloads any more
	}
	// Before the lock: a change of the ranks runs this step itself.
	if err := a.ixRanksFromStars(ctx); err != nil {
		return err
	}
	box := a.ixSched()
	box.mu.Lock()
	defer box.mu.Unlock()
	if err := a.ixSpreadUpdate(ctx, box); err != nil {
		return err
	}
	ranks, err := a.ixRanks(ctx)
	if err != nil {
		return err
	}
	base := make(map[string]int, len(ranks))
	for _, g := range ranks {
		base[g.GroupID] = g.Base
	}
	rows, err := a.ixSchedLoad(ctx)
	if err != nil {
		return err
	}
	saved, err := a.ixSavedIdentities(ctx)
	if err != nil {
		return err
	}
	public := a.ixPublicGroups(ctx)
	bands := a.ixDeadlineEstimate(ctx, false).Active

	// What each file is asked for by: the best rank of any row that wants it.
	demand := map[string]int{}
	want := func(identity string, b int) {
		if identity == "" || b <= 0 {
			return
		}
		if d, ok := demand[identity]; !ok || b < d {
			demand[identity] = b
		}
	}
	queued := make(map[string]bool, len(rows))
	byID := make(map[int64]*ixSchedRow, len(rows))
	seen := make(map[string]bool, len(rows))
	shared := int64(0)
	for i := range rows {
		r := &rows[i]
		byID[r.id] = r
		queued[r.identity] = true
		if !r.blocked {
			want(r.identity, base[r.group])
		}
		r.free = saved[ixIdentityFile(r.identity)]
		if !r.blocked && !r.held && !r.deferred {
			if r.free || (r.identity != "" && seen[r.identity]) {
				shared++
			}
			seen[r.identity] = true
		}
	}
	box.shared.Store(shared)
	// A ranked group's messages whose files are queued through another group,
	// before its own files pass has come to them. A same Telegram file only:
	// messages say nothing about look-alikes.
	for _, g := range ranks {
		if err := a.ixSchedMessageDemand(ctx, g, queued, want); err != nil {
			return err
		}
	}
	// Probable copies held behind a better copy: that copy is what their group
	// is waiting for. Twice, because the better copy may itself be shared.
	links, err := a.ixSchedHoldLinks(ctx)
	if err != nil {
		return err
	}
	for pass := 0; pass < 2; pass++ {
		for _, l := range links {
			from, to := byID[l[0]], byID[l[1]]
			if from == nil || to == nil || from.blocked {
				continue
			}
			if d, ok := demand[from.identity]; ok {
				want(to.identity, d)
			}
		}
	}
	blocked, err := a.ixSubstBlockedSet(ctx)
	if err != nil {
		return err
	}

	type change struct {
		id    int64
		order int
	}
	var changes []change
	for i := range rows {
		r := &rows[i]
		order := engine.UnrankedOrder
		switch d, ok := demand[r.identity]; {
		case r.free && !r.blocked && !r.held && !blocked[r.identity]:
			order = ixFreeOrder
		case ok:
			order = d
			if bands {
				order += ixBandOf(r.size)
				if public[r.group] {
					order += ixPublicLane
				}
			}
		}
		if order != r.order {
			changes = append(changes, change{r.id, order})
		}
	}
	for start := 0; start < len(changes); start += ixSchedBatch {
		if err := ctx.Err(); err != nil {
			return err
		}
		tx, err := a.db.Writer.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, c := range changes[start:min(start+ixSchedBatch, len(changes))] {
			if _, err := tx.ExecContext(ctx, `UPDATE tgdl_work SET sched_order=? WHERE id=? AND status='pending'`, c.order, c.id); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	// A ranked file, or one that takes no bytes, is no longer set aside. The
	// deadline order leaves such rows alone from here on (ixDeadlineStep).
	letGo := false
	for _, c := range changes {
		if r := byID[c.id]; r != nil && r.deferred && c.order < engine.UnrankedOrder {
			letGo = true
			break
		}
	}
	if letGo {
		if err := a.ixDeadlineLetGo(ctx, `w.sched_order<`+strconv.Itoa(engine.UnrankedOrder)); err != nil {
			return err
		}
	}
	if len(changes) > 0 {
		a.ixCaches().priorities.clear()
	}
	return nil
}

func (a *App) ixSchedLoad(ctx context.Context) ([]ixSchedRow, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT w.id, w.group_id, w.identity, w.sched_order, w.file_size, w.paused,
		h.work_id IS NOT NULL OR EXISTS (SELECT 1 FROM ix_lowq q WHERE q.work_id=w.id AND q.state='saved'),
		x.work_id IS NOT NULL
		FROM tgdl_work w
		LEFT JOIN ix_held h ON h.work_id=w.id AND h.released_at=0 AND h.reason<>''
		LEFT JOIN ix_deferred x ON x.work_id=w.id AND x.released_at=0
		WHERE w.status='pending' AND w.ttl_seconds=0
		ORDER BY w.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ixSchedRow
	for rows.Next() {
		var r ixSchedRow
		var paused bool
		if err := rows.Scan(&r.id, &r.group, &r.identity, &r.order, &r.size, &paused, &r.held, &r.deferred); err != nil {
			return nil, err
		}
		r.blocked = paused && !r.held && !r.deferred // paused, and not by the archive: a person did
		out = append(out, r)
	}
	return out, rows.Err()
}

// ixIdentityFile is a queue identity without its size: how the library knows a
// Telegram file (download.Library.existing).
func ixIdentityFile(identity string) string {
	if i := strings.LastIndexByte(identity, 0); i > 0 {
		return identity[:i]
	}
	return identity
}

// ixSavedIdentities is the Telegram files the library holds.
func (a *App) ixSavedIdentities(ctx context.Context) (map[string]bool, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT DISTINCT telegram_media_kind, telegram_media_id FROM downloads WHERE COALESCE(telegram_media_id,'')<>'' AND status='completed'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			return nil, err
		}
		out[strings.ToLower(kind)+"\x00"+id] = true
	}
	return out, rows.Err()
}

// ixSubstBlockedSet is the identities a person asked to fetch for real: their
// rows are real downloads even with the file saved.
func (a *App) ixSubstBlockedSet(ctx context.Context) (map[string]bool, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT identity FROM ix_subst_block`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (a *App) ixPublicGroups(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id FROM ix_group_info WHERE username<>''`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// ixIdentityOfMediaKey turns the archive's kind:id:size into a queue identity.
func ixIdentityOfMediaKey(key string) string {
	first := strings.IndexByte(key, ':')
	last := strings.LastIndexByte(key, ':')
	if first <= 0 || last <= first {
		return ""
	}
	return strings.ToLower(key[:first]) + "\x00" + key[first+1:last] + "\x00" + key[last+1:]
}

func (a *App) ixSchedMessageDemand(ctx context.Context, g ixRankedGroup, queued map[string]bool, want func(string, int)) error {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT DISTINCT media_key FROM ix_messages WHERE group_id=? AND media_key<>''`, g.GroupID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return err
		}
		if identity := ixIdentityOfMediaKey(key); queued[identity] {
			want(identity, g.Base)
		}
	}
	return rows.Err()
}

func (a *App) ixSchedHoldLinks(ctx context.Context) ([][2]int64, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT c.work_id, c.of_work FROM ix_copy_hold c
		JOIN ix_held h ON h.work_id=c.work_id AND h.released_at=0 AND h.reason<>''
		WHERE c.how='queued' AND c.of_work>0`)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out [][2]int64
	for rows.Next() {
		var l [2]int64
		if err := rows.Scan(&l[0], &l[1]); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ixSharedWaiting is how many queued rows take no transfer of their own: their
// file is saved, or another row of it comes first. They do not fill a
// backfill's queue limit, or the other groups' files would never be read.
func (a *App) ixSharedWaiting() int { return int(a.ixSched().shared.Load()) }

// ixAdmissionCredit is how many queued rows do not count towards the queue
// limit of a backfill of this group: those of files no group as high as this
// one needs. A queue full of lower-ranked files must not keep a ranked group's
// files from coming in. Kept for ixApartCacheTTL: a backfill at its limit asks
// every 250 ms.
func (a *App) ixAdmissionCredit(ctx context.Context, groupID string) int {
	box := a.ixSched()
	box.creditMu.Lock()
	defer box.creditMu.Unlock()
	if box.creditGroup == groupID && ixApartCacheTTL > 0 && time.Since(box.creditAt) < ixApartCacheTTL {
		return box.creditN
	}
	var base sql.NullInt64
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT sched_base FROM tgdl_group_rank WHERE group_id=?`, groupID).Scan(&base); err != nil || !base.Valid {
		box.creditGroup, box.creditAt, box.creditN = groupID, time.Now(), 0
		return 0
	}
	var n int
	// Waiting apart already leaves out what is paused and big files.
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM tgdl_work WHERE status='pending' AND origin<>'live' AND paused=0 AND file_size<? AND sched_order>=?`,
		engine.BigFileSize, base.Int64+ixBandCount).Scan(&n)
	box.creditGroup, box.creditAt, box.creditN = groupID, time.Now(), n
	return n
}

// ixSpreadUpdate, under the spread policy, reorders the groups' places by
// coverage for their rank: a group's weight is 1/rank, and the group with the
// least coverage per weight goes first. A group with nothing left to get goes
// last. It changes at most every ixSpreadEvery: each change rewrites rows and
// may make a rescan change groups. Under order, places follow the ranks.
func (a *App) ixSpreadUpdate(ctx context.Context, box *ixSchedBox) error {
	ranks, err := a.ixRanks(ctx)
	if err != nil || len(ranks) == 0 {
		return err
	}
	order := make([]ixRankedGroup, len(ranks))
	copy(order, ranks)
	if a.ixLoadSettings(ctx).GroupPolicy == ixGroupPolicySpread {
		if !box.spreadAt.IsZero() && time.Since(box.spreadAt) < ixSpreadEvery {
			return nil
		}
		cov, known := a.ixCoverageByGroup(ctx)
		if !known {
			return nil // the first index is still being built: keep the places
		}
		box.spreadAt = time.Now()
		score := make(map[string]float64, len(ranks))
		for _, g := range ranks {
			c := cov[g.GroupID]
			score[g.GroupID] = ixSpreadScore(c, g.Rank)
		}
		sort.SliceStable(order, func(i, j int) bool { return score[order[i].GroupID] < score[order[j].GroupID] })
	}
	return a.ixWriteBases(ctx, order)
}

// ixSpreadScore is coverage per weight; lower goes first.
func ixSpreadScore(c ixGroupCoverage, rank int) float64 {
	if c.Total.Bytes > 0 && c.Needed.Bytes == 0 {
		return 1e12 + float64(rank)
	}
	covered := 0.0 // nothing known of it yet: it needs reading before anything else
	if c.Total.Bytes > 0 {
		covered = float64(c.Saved.Bytes+c.Shared.Bytes) / float64(c.Total.Bytes)
	}
	return (covered + 0.01) * float64(rank)
}

func (a *App) ixWriteBases(ctx context.Context, order []ixRankedGroup) error {
	var tx *sql.Tx
	for position, g := range order {
		want := ixRankBase(position)
		if g.Base == want {
			continue
		}
		if tx == nil {
			var err error
			if tx, err = a.db.Writer.BeginTx(ctx, nil); err != nil {
				return err
			}
			defer tx.Rollback()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tgdl_group_rank SET sched_base=? WHERE group_id=?`, want, g.GroupID); err != nil {
			return err
		}
	}
	if tx != nil {
		return tx.Commit()
	}
	return nil
}

// ixCancelHistory stops a backfill, as the dashboard's button does.
func (a *App) ixCancelHistory(ctx context.Context, jobID string) {
	ixCall(ctx, a.handleHistoryCancel, http.MethodPost, "/api/history/"+jobID+"/cancel", nil, map[string]string{"jobId": jobID})
}
