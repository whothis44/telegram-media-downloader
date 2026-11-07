package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// Work that needs the live account, done while it can still be done: the
// account's deletion date (Telegram's app configuration says when a frozen
// account will be deleted), and closing gaps in each chat's message IDs so
// that every number between the first and last message is either kept or
// confirmed gone.

const ixFreezeKey = "ix_freeze"

type ixFreezeInfo struct {
	telegram.FreezeState
	CheckedAt int64  `json:"checkedAt,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ixFreezeEvery is how often the freeze state is read.
var ixFreezeEvery = 6 * time.Hour

func (a *App) ixFreeze(ctx context.Context) ixFreezeInfo {
	var out ixFreezeInfo
	var raw string
	if err := a.db.Reader.QueryRowContext(ctx, `SELECT value FROM kv WHERE key=?`, ixFreezeKey).Scan(&raw); err == nil {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (a *App) ixSaveFreeze(ctx context.Context, f ixFreezeInfo) {
	data, _ := json.Marshal(f)
	_, _ = a.db.Writer.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, ixFreezeKey, string(data), time.Now().UnixMilli())
}

// ixCheckFreeze reads the freeze state through the running account.
func (a *App) ixCheckFreeze(ctx context.Context) error {
	if a.monitor == nil {
		return errors.New("the Telegram engine is not running")
	}
	session, err := a.monitor.OpenHistory(ctx, "")
	if err != nil {
		return err
	}
	defer session.Close()
	st, err := session.FreezeState()
	if err != nil {
		return err
	}
	a.ixSaveFreeze(ctx, ixFreezeInfo{FreezeState: st, CheckedAt: time.Now().UnixMilli()})
	return nil
}

func (a *App) ixRunFreeze(ctx context.Context) {
	wait := 2 * time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = ixFreezeEvery
		if a.ixIsGone() {
			wait = 15 * time.Minute // read-only archive: nothing to ask Telegram
			continue
		}
		if err := a.ixCheckFreeze(ctx); err != nil && ctx.Err() == nil {
			old := a.ixFreeze(ctx)
			if time.Since(time.UnixMilli(old.CheckedAt)) > ixFreezeEvery {
				old.Error = err.Error()
				a.ixSaveFreeze(ctx, old)
			}
			wait = 15 * time.Minute // the engine may still be starting
		}
	}
}

// ixTriage ranks groups for the rescan queue once Telegram has set a
// deletion date: private chats (no public username) first, since a public
// channel can still be read from another account later.
func (a *App) ixTriage(ctx context.Context) map[string]int {
	if a.ixFreeze(ctx).Until == 0 {
		return nil
	}
	out := map[string]int{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id FROM ix_group_info WHERE username<>''`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = 1
		}
	}
	return out
}

// ---------- the end of a messages pass ----------

// ixMessagesPassEnd runs after a group's history has been read: the
// comments on its posts, then the gaps in its message IDs.
func (a *App) ixMessagesPassEnd(ctx context.Context, session *engine.MessageSession, dialog telegram.Dialog, groupID string) error {
	if err := a.ixSaveComments(ctx, session, dialog, groupID); err != nil {
		return err
	}
	if err := a.ixFillGaps(ctx, session, dialog, groupID); err != nil && ctx.Err() == nil && session.Err() == nil {
		a.ixLog("message gaps in %s: %v", groupID, err)
	}
	return ctx.Err()
}

// ixGapLimit is the most IDs one pass asks about (100 per request).
var ixGapLimit = 20000

// ixGapPause spaces the requests out.
var ixFloodWaitRe = regexp.MustCompile(`FLOOD_WAIT[_ (]+(\d+)`)
var ixGapPause = time.Second

// ixGapIDs lists the IDs between 1 and the newest kept message that are
// neither kept nor known to be gone, newest first.
func (a *App) ixGapIDs(ctx context.Context, groupID string, limit int) ([]int, int, error) {
	have := map[int]bool{}
	maxID := 0
	for _, q := range []string{`SELECT message_id FROM ix_messages WHERE group_id=?`, `SELECT message_id FROM ix_absent WHERE group_id=?`} {
		rows, err := a.db.Reader.QueryContext(ctx, q, groupID)
		if err != nil {
			return nil, 0, err
		}
		for rows.Next() {
			var id int
			if rows.Scan(&id) == nil {
				have[id] = true
				if id > maxID && !strings.Contains(q, "ix_absent") {
					maxID = id
				}
			}
		}
		rows.Close()
	}
	var out []int
	for id := maxID - 1; id >= 1 && len(out) < limit; id-- {
		if !have[id] {
			out = append(out, id)
		}
	}
	return out, maxID, nil
}

// ixFillGaps asks Telegram about every missing message ID of a channel or
// supergroup: messages it still has are kept like any other, the rest are
// recorded as gone. Basic groups number messages per account, so they have
// no gaps to close.
// ixByIDReader is what gap filling needs from a history session.
type ixByIDReader interface {
	MessagesByID(telegram.Dialog, []int) (telegram.ByIDPage, error)
	Err() error
}

func (a *App) ixFillGaps(ctx context.Context, session ixByIDReader, dialog telegram.Dialog, groupID string) error {
	if !strings.HasPrefix(groupID, "-100") {
		return nil
	}
	ids, _, err := a.ixGapIDs(ctx, groupID, ixGapLimit)
	if err != nil || len(ids) == 0 {
		return err
	}
	found, gone := 0, 0
	for start := 0; start < len(ids); start += 100 {
		a.ixBeat(groupID)
		batch := ids[start:min(start+100, len(ids))]
		page, err := session.MessagesByID(dialog, batch)
		for try := 0; err != nil && try < len(historyPageRetries) && ctx.Err() == nil && session.Err() == nil; try++ {
			wait := historyPageRetries[try]
			if d, ok := tgerr.AsFloodWait(err); ok {
				if d > 5*time.Minute {
					break // a long flood wait: leave the rest for the next pass
				}
				wait = d + time.Second
			} else if m := ixFloodWaitRe.FindStringSubmatch(err.Error()); m != nil {
				n, _ := strconv.Atoi(m[1])
				if n > 300 {
					break
				}
				wait = time.Duration(n+1) * time.Second
			} else if !historyConnectionLost(err) {
				break
			}
			a.ixBeat(groupID) // a Telegram wait of up to 5 min is work, for the stall watchdog
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			a.ixBeat(groupID)
			page, err = session.MessagesByID(dialog, batch)
		}
		if err != nil {
			return err
		}
		if len(page.Messages) > 0 {
			byGroup := map[string][]tg.MessageClass{}
			for _, m := range page.Messages {
				if g := ixMessageGroup(m); g == groupID {
					byGroup[g] = append(byGroup[g], m)
				}
			}
			for g, ms := range byGroup {
				if err := a.ixRecordHistoryPage(ctx, g, telegram.HistoryPage{Messages: ms, Entities: page.Entities}); err != nil {
					return err
				}
				found += len(ms)
			}
		}
		// IDs Telegram neither returned nor marked empty are gone too.
		returned := map[int]bool{}
		for _, m := range page.Messages {
			returned[m.GetID()] = true
		}
		now := time.Now().UnixMilli()
		tx, err := a.db.Writer.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, id := range batch {
			if returned[id] {
				continue
			}
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO ix_absent(group_id,message_id,checked_at) VALUES(?,?,?)`, groupID, id, now); err != nil {
				tx.Rollback()
				return err
			}
			gone++
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ixGapPause):
		}
	}
	a.ixLog("message gaps in %s: %d checked, %d recovered, %d gone", groupID, len(ids), found, gone)
	return nil
}

// ixIDCoverage says, for each group, how much of its message-ID range is
// accounted for: the newest ID, the IDs kept, the IDs confirmed gone.
type ixIDCover struct {
	MaxID, Kept, Gone int64
}

func (a *App) ixIDCoverage(ctx context.Context) map[string]ixIDCover {
	out := map[string]ixIDCover{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, MAX(message_id), COUNT(*) FROM ix_messages WHERE group_id LIKE '-100%' GROUP BY group_id`)
	if err != nil {
		return out
	}
	for rows.Next() {
		var id string
		var c ixIDCover
		if rows.Scan(&id, &c.MaxID, &c.Kept) == nil {
			out[id] = c
		}
	}
	rows.Close()
	rows, err = a.db.Reader.QueryContext(ctx, `SELECT a.group_id, COUNT(*) FROM ix_absent a WHERE a.message_id < (SELECT MAX(message_id) FROM ix_messages m WHERE m.group_id=a.group_id) GROUP BY a.group_id`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int64
		if rows.Scan(&id, &n) == nil {
			c := out[id]
			c.Gone = n
			out[id] = c
		}
	}
	return out
}

// ixFreezeAlerts are the alerts about the account's deletion date.
func ixFreezeAlerts(f ixFreezeInfo, now time.Time) []ixAlert {
	if f.Until == 0 {
		return nil
	}
	until := time.Unix(f.Until, 0)
	days := int(until.Sub(now).Hours() / 24)
	msg := fmt.Sprintf("Telegram will delete this account on %s (%d day(s) from now) unless an appeal is accepted.", until.In(ixTZ).Format("Mon Jan 2 2006, 15:04"), max(days, 0))
	if f.AppealURL != "" {
		msg += " Appeal: " + f.AppealURL
	}
	msg += " Do not log the account out: a frozen account cannot sign in again."
	return []ixAlert{{Key: "freeze:" + strconv.FormatInt(f.Until, 10), Title: "Telegram account will be deleted", Message: msg}}
}

// ixSortedKeys is a helper for stable output.
func ixSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
