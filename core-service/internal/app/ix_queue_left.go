package app

import "context"

// A rescan is "done" once it has read every message and queued every
// missing file; the files themselves may still be waiting to download. The
// rescan list shows what each group still has in the download queue, so a
// group only reads as done once its files are in.

type ixGroupLeft struct {
	Left  int `json:"left"`  // waiting or downloading
	Apart int `json:"apart"` // set aside for now: likely copies, larger files while time is short, or paused by a person
}

// ixQueueLeft counts each group's files still in the download queue.
func (a *App) ixQueueLeft(ctx context.Context) map[string]ixGroupLeft {
	out := map[string]ixGroupLeft{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, paused, COUNT(*) FROM tgdl_work WHERE status IN ('pending','processing') GROUP BY group_id, paused`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var paused, n int
		if rows.Scan(&id, &paused, &n) != nil {
			continue
		}
		g := out[id]
		if paused != 0 {
			g.Apart += n
		} else {
			g.Left += n
		}
		out[id] = g
	}
	return out
}
