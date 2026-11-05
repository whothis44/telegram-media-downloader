package app

import (
	"context"
	"database/sql"
	"strconv"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/download"
	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// A lower quality of very large videos first, when time is short.
//
// Telegram keeps smaller encodes of many videos (see telegram.AltQualities).
// When a deletion date is set and a large video (200 MB or more) would not
// be reached before it at the speed of the last day, the smallest
// alternative of at least 480p (or, without sizes, the smallest above a
// quarter of the bytes) is saved first as the message's file, if it is at
// most 40% of the original's bytes. The original stays queued as an
// upgrade that goes after everything else, held copies included, and
// before confirmed copies (ix_order.go). When it
// arrives and is verified, it replaces the lower quality (which is then
// removed, unless the setting to keep it is on).
//
// Never: for a probable or confirmed copy (those are left to ix_held.go and
// ix_subst), for self-destructing media or explicit requests, two
// alternatives of one video, or a quality already saved (that one is used).

// ixLowqMinSize is a variable so tests can use small files.
var ixLowqMinSize int64 = 200 << 20

const (
	ixLowqMinShort = 480  // pixels on the shorter side
	ixLowqMaxShare = 0.40 // of the original's bytes
)

const ixSchemaLowq = `
CREATE TABLE IF NOT EXISTS ix_lowq ( -- big videos saved in a lower quality first
	group_id    TEXT NOT NULL,
	message_id  INTEGER NOT NULL,
	work_id     INTEGER NOT NULL,
	orig_id     TEXT NOT NULL,              -- the original's Telegram document
	orig_size   INTEGER NOT NULL,
	alt_id      TEXT NOT NULL,              -- the lower quality's
	alt_size    INTEGER NOT NULL,
	alt_short   INTEGER NOT NULL DEFAULT 0, -- its shorter side, 0: unknown
	alt_cost    INTEGER NOT NULL DEFAULT 0, -- bytes downloaded for it (0: it was saved already)
	alt_path    TEXT NOT NULL DEFAULT '',
	keep        INTEGER NOT NULL DEFAULT 0, -- keep the lower quality after the upgrade
	state       TEXT NOT NULL DEFAULT 'saved', -- saved (original to come), released (original queued now), upgraded, kept, dropped
	saved_at    INTEGER NOT NULL DEFAULT 0,
	released_at INTEGER NOT NULL DEFAULT 0,
	upgraded_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (group_id, message_id)
);
CREATE INDEX IF NOT EXISTS ix_lowq_work ON ix_lowq(work_id);`

type ixLowqPlan struct {
	upgrade bool // this claim is the original, after its lower quality
	origID  string
	orig    int64
	alt     telegram.AltQuality
}

// ixIngestWork is the queue's sink for one item: it may save a lower
// quality first, waits for room in the staging folder, then ingests.
func (a *App) ixIngestWork(ctx context.Context, work *engine.Work, message *tg.Message, transport telegram.MediaDownloader) error {
	message, plan := a.ixLowqPick(ctx, work, message, transport)
	size := work.FileSize
	if plan != nil && !plan.upgrade {
		size = plan.alt.Doc.Size
	}
	if err := a.ixWaitStageRoom(ctx, size, transport); err != nil {
		return err
	}
	record, err := a.ingestTelegram(ctx, message, work.GroupID, work.GroupName, work.AccountID, transport)
	if err != nil {
		return err
	}
	if plan != nil && !plan.upgrade {
		return a.ixLowqSaved(ctx, work, plan, record)
	}
	if !record.Staged {
		a.ixLowqArrived(ctx, work.GroupID, work.MessageID)
	}
	return nil
}

// ixLowqPick returns the message to download: the original, or a copy
// naming the lower quality to save first, with its plan.
func (a *App) ixLowqPick(ctx context.Context, work *engine.Work, message *tg.Message, transport telegram.MediaDownloader) (*tg.Message, *ixLowqPlan) {
	if work == nil || message == nil || work.MediaType != "video" || work.FileSize < ixLowqMinSize {
		return message, nil
	}
	var state string
	err := a.db.Reader.QueryRowContext(ctx, `SELECT state FROM ix_lowq WHERE group_id=? AND message_id=?`, work.GroupID, work.MessageID).Scan(&state)
	if err == nil {
		if state == "saved" || state == "released" {
			// The upgrade: the original now, never another alternative.
			keep := 0
			if a.ixLoadSettings(ctx).LowqKeep {
				keep = 1
			}
			_, _ = a.db.Writer.ExecContext(ctx, `UPDATE ix_lowq SET keep=?, state='released', released_at=CASE WHEN released_at=0 THEN ? ELSE released_at END WHERE group_id=? AND message_id=?`, keep, time.Now().UnixMilli(), work.GroupID, work.MessageID)
			return message, &ixLowqPlan{upgrade: true}
		}
		return message, nil
	}
	if work.Origin != "history" && work.Origin != "live" || telegram.MessageTTL(message) > 0 {
		return message, nil
	}
	if !a.ixLoadSettings(ctx).LowqFirst {
		return message, nil
	}
	// A file a ranked group needs comes early in the order it goes in, and its
	// original would go after everything else: never for those. (ixLowqShort
	// judges by size alone, as if the ranking were not there.)
	var order int64
	if a.db.Reader.QueryRowContext(ctx, `SELECT sched_order FROM tgdl_work WHERE id=?`, work.ID).Scan(&order) == nil && order < engine.UnrankedOrder {
		return message, nil
	}
	orig, alts, ok := telegram.AltQualities(message)
	if !ok || len(alts) == 0 || orig.Size < ixLowqMinSize {
		return message, nil
	}
	att, err := telegram.MessageAttachment(message)
	if err != nil || att.Type != "video" {
		return message, nil
	}
	if !a.ixLowqShort(ctx, work, orig.Size) {
		return message, nil
	}
	if a.ixLowqIsCopy(ctx, work, att, transport) {
		return message, nil
	}
	// A quality already saved costs nothing: use it rather than another.
	q, found := a.ixLowqSavedAlt(ctx, alts)
	if !found {
		if q, found = telegram.PickAltQuality(orig.Size, alts, ixLowqMinShort, ixLowqMaxShare); !found {
			return message, nil
		}
	}
	return telegram.WithAltQuality(message, q, att.Name), &ixLowqPlan{origID: strconv.FormatInt(orig.ID, 10), orig: orig.Size, alt: q}
}

// ixLowqShort: the original would not be reached before the deletion date
// at the deadline estimate's speed (ixDeadlineRate). What goes before it is what the deadline
// order lets go first: the queued files smaller than it (held copies and
// other upgrades go after), plus the same share of the files the rescans
// have not queued yet.
func (a *App) ixLowqShort(ctx context.Context, work *engine.Work, size int64) bool {
	d := a.ixLowqDeadline(ctx)
	if !d.Active || d.Until <= 0 || d.Rate <= 0 || d.Finishes == "yes" {
		return false
	}
	var ahead int64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COALESCE(SUM(w.file_size),0) FROM tgdl_work w
		WHERE w.status IN ('pending','processing') AND w.id<>? AND (w.file_size<? OR (w.file_size=? AND w.id<?))
		AND NOT EXISTS (SELECT 1 FROM ix_held h WHERE h.work_id=w.id AND h.released_at=0 AND h.reason<>'')
		AND NOT EXISTS (SELECT 1 FROM ix_lowq q WHERE q.work_id=w.id AND q.state='saved')`, work.ID, size, size, work.ID).Scan(&ahead)
	if d.LeftBytes > 0 && d.ToGetBytes > d.LeftBytes {
		ahead += int64(float64(ahead) / float64(d.LeftBytes) * float64(d.ToGetBytes-d.LeftBytes))
	}
	reach := time.Now().Unix() + int64(float64(ahead+size)/d.Rate)
	return reach >= d.Until
}

var ixLowqEstimates sync.Map // *App -> ixLowqEstimate

type ixLowqEstimate struct {
	at time.Time
	d  ixDeadlineInfo
}

// ixLowqDeadline is the deadline estimate, measured at most once a minute.
func (a *App) ixLowqDeadline(ctx context.Context) ixDeadlineInfo {
	if v, ok := ixLowqEstimates.Load(a); ok {
		if e := v.(ixLowqEstimate); time.Since(e.at) < time.Minute {
			return e.d
		}
	}
	d := a.ixDeadlineEstimate(ctx, false)
	ixLowqEstimates.Store(a, ixLowqEstimate{time.Now(), d})
	return d
}

// ixLowqIsCopy: the video is a probable or confirmed copy of a saved file,
// or the original is saved already. Then neither quality is fetched here:
// the copy rules (ix_held.go) and the library decide.
func (a *App) ixLowqIsCopy(ctx context.Context, work *engine.Work, att telegram.Attachment, transport telegram.MediaDownloader) bool {
	var n int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM ix_held WHERE work_id=? AND reason<>'')
		+ (SELECT COUNT(*) FROM ix_subst WHERE group_id=? AND message_id=?)
		+ (SELECT COUNT(*) FROM downloads WHERE telegram_media_kind=? AND telegram_media_id=? AND status='completed' AND file_path IS NOT NULL AND file_path<>'')`,
		work.ID, work.GroupID, work.MessageID, att.Identity.Kind, att.Identity.ID).Scan(&n)
	if n > 0 {
		return true
	}
	if _, _, found := a.ixLikelyCopy(ctx, att.Facts, att.Identity.Size); found {
		return true
	}
	if transport != nil {
		item := download.Item{GroupID: work.GroupID, GroupName: work.GroupName, MessageID: work.MessageID, Name: att.Name, Type: att.Type, Identity: att.Identity, Facts: att.Facts}
		if _, _, same := a.ixSubstitute(ctx, item, telegram.AttachmentClient{Source: transport, Media: att}); same {
			return true
		}
	}
	return false
}

// ixLowqSavedAlt is the best alternative of the video already in the library.
func (a *App) ixLowqSavedAlt(ctx context.Context, alts []telegram.AltQuality) (telegram.AltQuality, bool) {
	var best telegram.AltQuality
	found := false
	for _, q := range alts {
		var n int
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM downloads WHERE telegram_media_kind='document' AND telegram_media_id=? AND status='completed' AND file_path IS NOT NULL AND file_path<>''`, strconv.FormatInt(q.Doc.ID, 10)).Scan(&n)
		if n > 0 && (!found || q.Doc.Size > best.Doc.Size) {
			best, found = q, true
		}
	}
	return best, found
}

// ixLowqSaved records the lower quality and puts the original back in the
// queue, paused until everything else is done. Bumping the generation
// makes the engine leave the item pending, with its message, instead of
// marking it done.
func (a *App) ixLowqSaved(ctx context.Context, work *engine.Work, plan *ixLowqPlan, record download.Record) error {
	cost := plan.alt.Doc.Size
	if record.Reused || record.Skipped {
		cost = 0
	}
	now := time.Now().UnixMilli()
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO ix_lowq(group_id,message_id,work_id,orig_id,orig_size,alt_id,alt_size,alt_short,alt_cost,alt_path,state,saved_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,'saved',?) ON CONFLICT(group_id,message_id) DO NOTHING`,
		work.GroupID, work.MessageID, work.ID, plan.origID, plan.orig, strconv.FormatInt(plan.alt.Doc.ID, 10), plan.alt.Doc.Size, plan.alt.Short(), cost, record.Path, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tgdl_work SET paused=1, generation=CASE WHEN generation=? THEN generation+1 ELSE generation END, updated_at=?
		WHERE id=? AND status='processing'`, work.Generation, now, work.ID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.ixLog("saved %s #%d in a lower quality first (%d MB instead of %d MB); the original follows after everything else", work.GroupID, work.MessageID, plan.alt.Doc.Size>>20, plan.orig>>20)
	return nil
}

// ixLowqArrived marks a message upgraded once its catalogue entry is the
// original. The library has then verified the original and queued the
// lower quality's file for removal (kept when keep is set).
func (a *App) ixLowqArrived(ctx context.Context, groupID string, messageID int64) {
	var origID string
	var keep int
	if a.db.Reader.QueryRowContext(ctx, `SELECT orig_id, keep FROM ix_lowq WHERE group_id=? AND message_id=? AND state IN ('saved','released','dropped')`, groupID, messageID).Scan(&origID, &keep) != nil {
		return
	}
	var have sql.NullString
	if a.db.Reader.QueryRowContext(ctx, `SELECT telegram_media_id FROM downloads WHERE group_id=? AND message_id=?`, groupID, messageID).Scan(&have) != nil || have.String != origID {
		return
	}
	state := "upgraded"
	if keep != 0 {
		state = "kept"
	}
	_, _ = a.db.Writer.ExecContext(ctx, `UPDATE ix_lowq SET state=?, upgraded_at=? WHERE group_id=? AND message_id=?`, state, time.Now().UnixMilli(), groupID, messageID)
}

// ixLowqKeptPaths are lower qualities to keep after their upgrade; the
// media cleanup leaves them alone.
func (a *App) ixLowqKeptPaths(ctx context.Context, tx *sql.Tx) []string {
	paths, err := queryPaths(ctx, tx, `SELECT alt_path FROM ix_lowq WHERE keep<>0 AND alt_path<>''`)
	if err != nil {
		return nil
	}
	return paths
}

func (a *App) ixRunLowq(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
		if !ixVdupBackground.Load() {
			continue
		}
		if err := a.ixLowqStep(ctx); err != nil && ctx.Err() == nil {
			a.ixLog("lower quality first: %v", err)
		}
	}
}

// ixLowqStep lets the originals go once every stage before them is done
// (ix_order.go): no file the engine could take now, none set aside by the
// deadline order, no held copy still to be released. Confirmed copies go
// after them.
func (a *App) ixLowqStep(ctx context.Context) error {
	if a.ixIsGone() {
		return nil // read-only: nothing downloads any more, so there is no order to keep
	}
	now := time.Now().UnixMilli()
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, message_id FROM ix_lowq WHERE state IN ('saved','released')`)
	if err != nil {
		return err
	}
	type key struct {
		group string
		msg   int64
	}
	var open []key
	for rows.Next() {
		var k key
		if rows.Scan(&k.group, &k.msg) == nil {
			open = append(open, k)
		}
	}
	rows.Close()
	for _, k := range open {
		a.ixLowqArrived(ctx, k.group, k.msg)
	}
	// The original was removed from the queue or cancelled: the lower quality stays.
	if _, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_lowq SET state='dropped' WHERE state IN ('saved','released')
		AND NOT EXISTS (SELECT 1 FROM tgdl_work w WHERE w.id=ix_lowq.work_id AND w.status<>'skipped')`); err != nil {
		return err
	}
	// A person resumed it: it goes now.
	if _, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_lowq SET state='released', released_at=? WHERE state='saved'
		AND EXISTS (SELECT 1 FROM tgdl_work w WHERE w.id=ix_lowq.work_id AND w.status='pending' AND w.paused=0)`, now); err != nil {
		return err
	}
	s := a.ixLoadSettings(ctx)
	limit := 10
	if s.LowqFirst {
		// After normal work, files set aside by the deadline order and
		// held copies; before confirmed copies (ix_order.go).
		o, err := a.ixOrderNow(ctx)
		if err != nil {
			return err
		}
		if !o.clearBefore(ixStageLowq) {
			return nil
		}
	} else {
		limit = -1 // switched off: every original goes now
	}
	return a.ixLowqRelease(ctx, limit)
}

// ixLowqRelease lets go up to n originals, smallest first (all when n < 0).
func (a *App) ixLowqRelease(ctx context.Context, n int) error {
	limit := ""
	if n >= 0 {
		limit = " LIMIT " + strconv.Itoa(n)
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT work_id FROM ix_lowq WHERE state='saved' ORDER BY orig_size, saved_at`+limit)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	now := time.Now().UnixMilli()
	for _, id := range ids {
		if _, err := a.db.Writer.ExecContext(ctx, `UPDATE tgdl_work SET paused=0 WHERE id=? AND status='pending' AND paused=1`, id); err != nil {
			return err
		}
		if _, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_lowq SET state='released', released_at=? WHERE work_id=? AND state='saved'`, now, id); err != nil {
			return err
		}
	}
	return nil
}

type ixLowqStatus struct {
	On         bool  `json:"on"`
	Saved      int64 `json:"saved"`      // big videos saved in a lower quality first
	SavedBytes int64 `json:"savedBytes"` // what those lower qualities took
	OrigBytes  int64 `json:"origBytes"`  // what their originals are
	Waiting    int64 `json:"waiting"`    // originals still to come
	Upgraded   int64 `json:"upgraded"`   // originals that came
	ExtraBytes int64 `json:"extraBytes"` // lower-quality bytes downloaded for videos whose original came too
	Kept       int64 `json:"kept"`       // of the upgraded, lower quality kept
}

func (a *App) ixLowqStatus(ctx context.Context) ixLowqStatus {
	st := ixLowqStatus{On: a.ixLoadSettings(ctx).LowqFirst}
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(alt_cost),0), COALESCE(SUM(orig_size),0),
		COALESCE(SUM(state IN ('saved','released')),0), COALESCE(SUM(state IN ('upgraded','kept')),0),
		COALESCE(SUM(CASE WHEN state IN ('upgraded','kept') THEN alt_cost ELSE 0 END),0), COALESCE(SUM(state='kept'),0)
		FROM ix_lowq WHERE state<>'dropped' OR alt_cost>0`).Scan(&st.Saved, &st.SavedBytes, &st.OrigBytes, &st.Waiting, &st.Upgraded, &st.ExtraBytes, &st.Kept)
	return st
}
