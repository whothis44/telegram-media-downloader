package app

import (
	"context"
)

// ix.12.13 (the user's choice, 11-06): in ranked groups, videos only partly
// the same as a saved one (3-4 of 8 sampled frames, ix_copy_hold.how
// 'sampled-partial') download in their group's rank order instead of at the
// back. They are mostly footage the archive does not have, in the groups the
// user values most. Unranked groups keep the 12.5 rule.

// ixGroupRanked: the queue row's group has a rank.
func (a *App) ixGroupRanked(ctx context.Context, workID int64) bool {
	var n int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM tgdl_work w JOIN tgdl_group_rank r ON r.group_id=w.group_id WHERE w.id=?`, workID).Scan(&n)
	return n > 0
}

// ixReleasePartialRanked lets go the partly-same videos held in ranked
// groups: those held before 12.13 and those of a group ranked since. Run
// with every held step, so a group ranked later is covered within minutes;
// a let-go row is never held again (ixHoldWork).
func (a *App) ixReleasePartialRanked(ctx context.Context) (int, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT h.work_id FROM ix_held h JOIN ix_copy_hold c ON c.work_id=h.work_id
		JOIN tgdl_work w ON w.id=h.work_id JOIN tgdl_group_rank r ON r.group_id=w.group_id
		WHERE h.released_at=0 AND h.reason<>'' AND c.how='sampled-partial' AND w.status='pending' LIMIT 200`)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if err := a.ixLetGo(ctx, id); err != nil {
			return 0, err
		}
	}
	if len(ids) > 0 {
		a.ixLog("partly-same videos of ranked groups go back to their place in rank order: %d", len(ids))
		a.ixWake()
	}
	return len(ids), nil
}
