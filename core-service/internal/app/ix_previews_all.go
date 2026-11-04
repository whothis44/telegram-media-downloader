package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// Sharper previews for everything not downloaded. Without them, a file not
// downloaded shows in the browser as Telegram's tiny stripped preview
// (about 40 px, blurry). Telegram also keeps a 320 px preview of every
// photo and video ("m", some 20 KB, one request); this pass fetches it for
// every photo and video known from the archived messages whose file is not
// saved (queued or not: held, set aside, or not reached yet), and keeps it
// with the copy check's previews (ix_copies_ml.go, dataDir/ixpreviews), so
// the browser shows it ("preview only") and the copy check looks at it
// without asking Telegram again. It needs no image recognition.
//
// It is gentle: one request at a time, at least ixPreviewAllGap apart,
// through the account's guarded download pools like the copy check. A
// FLOOD_WAIT, on its own requests or on the downloads', pauses it (five
// minutes, or as long as Telegram asked). Telegram's speed limit for
// accounts without Premium (FLOOD_PREMIUM_WAIT) is about volume: each one
// doubles the time between requests (up to ixPreviewAllGapMax), and a
// round without one brings it back; one on its own request also pauses it
// as long as Telegram asked (a minute at least). A read-only archive asks
// Telegram nothing; without the engine it waits quietly.
//
// Order: private chats first (no public username), then the newest
// messages; messages deleted on Telegram after the rest. A file posted in
// several messages is fetched once.
//
// A chat's previews are read through the account that queues its files,
// or the running account when that one cannot be opened (removed, or not
// running). An answer about the whole chat (CHANNEL_PRIVATE, FROZEN_*,
// CHAT_*) leaves the chat's other files alone for ixPreviewAllChatSkip
// rather than asking for each of them; one chat never ends the round.

const (
	ixPreviewAllBatch      = 100 // files per round, then the order is read again
	ixPreviewAllFloodPause = 5 * time.Minute
	ixPreviewAllPremiumMin = time.Minute
	ixPreviewAllGapMax     = 16 // times ixPreviewAllGap, after speed-limit waits
	ixPreviewAllChatSkip   = 6 * time.Hour
)

var (
	// ixPreviewAllGap is the least time between two requests of the pass
	// (a message read again counts as one).
	ixPreviewAllGap = 1500 * time.Millisecond
	// ixPreviewAllIdle is the wait when there is nothing to do.
	ixPreviewAllIdle = 10 * time.Minute
)

// ixPreviewAllSignal reports Telegram's waits on the running accounts'
// downloads in the last hour: plain waits, and the speed limit for accounts
// without Premium (replaceable in tests).
var ixPreviewAllSignal = func(a *App) (floods, premium int) {
	p := a.ixPaceNow()
	return p.Floods1h, p.Premium1h
}

// ixPreviewAllInfo is what the Overview and the diagnostics report show
// (numbers only).
type ixPreviewAllInfo struct {
	Mode        string `json:"mode"` // auto (while a deletion date is set), on, off
	On          bool   `json:"on"`
	Measured    bool   `json:"measured"`
	Known       int64  `json:"known"`       // photos and videos in messages, not downloaded (a file counted once)
	Fetched     int64  `json:"fetched"`     // of them, with a 320 px preview kept
	Remaining   int64  `json:"remaining"`   // still to fetch
	Unavailable int64  `json:"unavailable"` // Telegram has none, or it failed a few times
	FetchedRun  int64  `json:"fetchedRun"`  // fetched by this pass since the archive started
	ChatsLeft   int    `json:"chatsLeft"`   // chats left alone for now: Telegram refused the whole chat
	PausedUntil int64  `json:"pausedUntil,omitempty"`
	Why         string `json:"why,omitempty"` // why it is not fetching now
	At          int64  `json:"at,omitempty"`
}

type ixPreviewAllBox struct {
	mu          sync.Mutex
	info        ixPreviewAllInfo
	pauseUntil  time.Time
	slower      int  // the gap is ixPreviewAllGap times this (0: 1), after speed-limit waits
	premiumSeen bool // a speed-limit wait during this round
	sigSeen     bool
	lastFloods  int
	lastPremium int
	skipChats   map[string]time.Time // chats left alone until then (an answer about the whole chat)
}

var ixPreviewAllState sync.Map // *App -> *ixPreviewAllBox

func (a *App) ixPreviewAllBox() *ixPreviewAllBox {
	v, _ := ixPreviewAllState.LoadOrStore(a, &ixPreviewAllBox{})
	return v.(*ixPreviewAllBox)
}

// ixPreviewAllNow is the last measurement (made in the background).
func (a *App) ixPreviewAllNow() ixPreviewAllInfo {
	b := a.ixPreviewAllBox()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.info
}

func ixPreviewAllModeOf(s ixSettings) string {
	switch s.PreviewsAll {
	case "on", "off":
		return s.PreviewsAll
	}
	return "auto"
}

// ixPreviewAllOn: on, or auto while Telegram has set a deletion date.
func (a *App) ixPreviewAllOn(ctx context.Context) (string, bool) {
	mode := ixPreviewAllModeOf(a.ixLoadSettings(ctx))
	switch mode {
	case "on":
		return mode, true
	case "off":
		return mode, false
	}
	return mode, a.ixFreeze(ctx).Until > 0
}

// ixPreviewAllWhere selects the photos and videos of archived messages
// whose file is not saved: neither from this message nor (the same
// Telegram file) from another.
const ixPreviewAllWhere = `m.media_type IN ('photo','video','gif') AND m.service='' AND m.media_key<>'' AND length(m.tl)>0
	AND NOT EXISTS (SELECT 1 FROM downloads d WHERE d.group_id=m.group_id AND d.message_id=m.message_id
		AND d.file_path IS NOT NULL AND d.file_path<>'' AND (d.status IS NULL OR d.status='completed'))
	AND NOT EXISTS (SELECT 1 FROM downloads d WHERE d.telegram_media_kind=substr(m.media_key,1,instr(m.media_key,':')-1)
		AND d.telegram_media_id=substr(substr(m.media_key,instr(m.media_key,':')+1),1,instr(substr(m.media_key,instr(m.media_key,':')+1),':')-1)
		AND d.file_path IS NOT NULL AND d.file_path<>'' AND (d.status IS NULL OR d.status='completed'))`

// ixPreviewAllMeasure counts what is left and keeps the result.
func (a *App) ixPreviewAllMeasure(ctx context.Context) ixPreviewAllInfo {
	var known, fetched, unavailable int64
	err := a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(t.path<>''),0), COALESCE(SUM(t.path='' AND t.tries>=?),0)
		FROM (SELECT DISTINCT m.media_key AS ident FROM ix_messages m WHERE `+ixPreviewAllWhere+`) c
		LEFT JOIN ix_copy_thumb t ON t.ident=c.ident`, ixPreviewTries).Scan(&known, &fetched, &unavailable)
	mode, on := a.ixPreviewAllOn(ctx)
	b := a.ixPreviewAllBox()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.info.Mode, b.info.On = mode, on
	if err == nil {
		b.info.Measured = true
		b.info.Known, b.info.Fetched, b.info.Unavailable = known, fetched, unavailable
		b.info.Remaining = max(0, known-fetched-unavailable)
		b.info.At = time.Now().UnixMilli()
	}
	b.info.ChatsLeft = len(b.skippedLocked(time.Now()))
	b.info.PausedUntil = 0
	if time.Now().Before(b.pauseUntil) {
		b.info.PausedUntil = b.pauseUntil.UnixMilli()
	}
	return b.info
}

func (a *App) ixPreviewAllDiag(ctx context.Context) string {
	p := a.ixPreviewAllMeasure(ctx)
	line := fmt.Sprintf("  previews of files not downloaded (%s, %s): %d photos/videos · 320 px preview kept %d · to fetch %d · none available %d · fetched since start %d",
		p.Mode, map[bool]string{true: "on", false: "off"}[p.On], p.Known, p.Fetched, p.Remaining, p.Unavailable, p.FetchedRun)
	if p.ChatsLeft > 0 {
		line += fmt.Sprintf(" · chats left alone for now %d", p.ChatsLeft)
	}
	if p.Why != "" {
		line += " · " + p.Why
	}
	return line
}

// skippedLocked are the chats left alone now (expired ones are dropped).
func (b *ixPreviewAllBox) skippedLocked(now time.Time) []any {
	var out []any
	for g, until := range b.skipChats {
		if now.Before(until) {
			out = append(out, g)
		} else {
			delete(b.skipChats, g)
		}
	}
	return out
}

// ixPreviewAllSkipChat leaves a chat alone for ixPreviewAllChatSkip.
func (a *App) ixPreviewAllSkipChat(group string) {
	b := a.ixPreviewAllBox()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.skipChats == nil {
		b.skipChats = map[string]time.Time{}
	}
	b.skipChats[group] = time.Now().Add(ixPreviewAllChatSkip)
}

// ixPreviewChatWide: Telegram's answer is about the whole chat (or the
// account in it), not this file: asking for the chat's other files would
// get the same.
func ixPreviewChatWide(err error) bool {
	s := err.Error()
	return strings.Contains(s, "CHANNEL_PRIVATE") || strings.Contains(s, "CHANNEL_INVALID") || strings.Contains(s, "FROZEN_") || strings.Contains(s, "CHAT_")
}

func (a *App) ixPreviewAllSetWhy(why string) {
	b := a.ixPreviewAllBox()
	b.mu.Lock()
	b.info.Why = why
	b.mu.Unlock()
}

// ---------- the pass ----------

type ixPreviewAllJob struct {
	group   string
	msg     int
	ident   string
	body    []byte
	account string
}

// ixPreviewAllJobs lists up to n files without a kept preview, in order,
// leaving out the chats left alone for now.
func (a *App) ixPreviewAllJobs(ctx context.Context, n int) ([]ixPreviewAllJob, error) {
	b := a.ixPreviewAllBox()
	b.mu.Lock()
	skip := b.skippedLocked(time.Now())
	b.mu.Unlock()
	notIn := ""
	if len(skip) > 0 {
		notIn = ` AND m.group_id NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(skip)), ",") + `)`
	}
	args := append(append([]any{}, skip...), ixPreviewTries, n*2)
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT m.group_id, m.message_id, m.media_key, m.tl FROM ix_messages m
		LEFT JOIN ix_group_info g ON g.group_id=m.group_id
		WHERE `+ixPreviewAllWhere+notIn+`
		AND NOT EXISTS (SELECT 1 FROM ix_copy_thumb t WHERE t.ident=m.media_key AND (t.path<>'' OR t.tries>=?))
		ORDER BY COALESCE(g.username,'')='' DESC, m.deleted_at=0 DESC, m.date DESC, m.group_id, m.message_id DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	var out []ixPreviewAllJob
	seen := map[string]bool{}
	for rows.Next() {
		var j ixPreviewAllJob
		if rows.Scan(&j.group, &j.msg, &j.ident, &j.body) != nil || seen[j.ident] {
			continue
		}
		seen[j.ident] = true
		if len(out) < n {
			out = append(out, j)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The account that queues this chat's files reads its previews too
	// ("": the running account).
	accounts := map[string]string{}
	for i := range out {
		g := out[i].group
		acc, ok := accounts[g]
		if !ok {
			_ = a.db.Reader.QueryRowContext(ctx, `SELECT account_id FROM tgdl_work WHERE group_id=? ORDER BY id DESC LIMIT 1`, g).Scan(&acc)
			accounts[g] = acc
		}
		out[i].account = acc
	}
	return out, nil
}

// ixPreviewPacer keeps the pass's requests apart.
type ixPreviewPacer struct {
	gap  func() time.Duration
	last time.Time
}

func (p *ixPreviewPacer) wait(ctx context.Context) error {
	if gap := p.gap(); gap > 0 && !p.last.IsZero() {
		if d := time.Until(p.last.Add(gap)); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}
	}
	p.last = time.Now()
	return ctx.Err()
}

// ixPreviewAllGapNow is the time between two requests now.
func (a *App) ixPreviewAllGapNow() time.Duration {
	b := a.ixPreviewAllBox()
	b.mu.Lock()
	defer b.mu.Unlock()
	return ixPreviewAllGap * time.Duration(max(1, b.slower))
}

// ixPacedReader is a reader whose every request waits its turn.
type ixPacedReader struct {
	ixCopyReader
	ctx   context.Context
	pacer *ixPreviewPacer
	check func() error // a sign to stop before asking
}

func (r *ixPacedReader) before() error {
	if r.check != nil {
		if err := r.check(); err != nil {
			return err
		}
	}
	if err := r.pacer.wait(r.ctx); err != nil {
		return err
	}
	if r.check != nil {
		return r.check()
	}
	return nil
}

func (r *ixPacedReader) FilePart(att telegram.Attachment, offset int64) ([]byte, error) {
	if err := r.before(); err != nil {
		return nil, err
	}
	return r.ixCopyReader.FilePart(att, offset)
}

func (r *ixPacedReader) RefreshMessage(m *tg.Message) (*tg.Message, error) {
	if err := r.before(); err != nil {
		return nil, err
	}
	return r.ixCopyReader.RefreshMessage(m)
}

// errIxWaitSignal: Telegram just asked the downloads to wait; the pass
// stops.
var errIxWaitSignal = errors.New("Telegram asked the downloads to wait (FLOOD_WAIT)")

var ixFloodSecsRe = regexp.MustCompile(`FLOOD_(?:PREMIUM_)?WAIT_?\s*\(?(\d+)`)

// slowerLocked doubles the time between requests (a speed-limit wait).
func (b *ixPreviewAllBox) slowerLocked() {
	b.slower = min(max(1, b.slower)*2, ixPreviewAllGapMax)
	b.premiumSeen = true
}

// ixPreviewAllPause pauses the pass after Telegram asked it, or the
// downloads, to wait, and says for how long.
func (a *App) ixPreviewAllPause(err error) time.Duration {
	s := err.Error()
	premium := strings.Contains(s, "FLOOD_PREMIUM_WAIT")
	d := ixPreviewAllFloodPause
	if premium {
		d = ixPreviewAllPremiumMin
	}
	if m := ixFloodSecsRe.FindStringSubmatch(s); m != nil {
		if n, _ := strconv.Atoi(m[1]); time.Duration(n)*time.Second > d {
			d = time.Duration(n) * time.Second
		}
	}
	b := a.ixPreviewAllBox()
	b.mu.Lock()
	defer b.mu.Unlock()
	if premium {
		b.slowerLocked()
		b.info.Why = "paused: Telegram's speed limit for accounts without Premium"
	} else {
		b.info.Why = "paused: Telegram asked the account to wait"
	}
	b.pauseUntil = time.Now().Add(d)
	b.info.PausedUntil = b.pauseUntil.UnixMilli()
	return d
}

// ixPreviewAllSignalled looks at the downloads' waits since the last look:
// a new FLOOD_WAIT stops the pass (errIxWaitSignal); a new speed-limit wait
// slows it down.
func (a *App) ixPreviewAllSignalled() error {
	floods, premium := ixPreviewAllSignal(a)
	b := a.ixPreviewAllBox()
	b.mu.Lock()
	defer b.mu.Unlock()
	first := !b.sigSeen
	moreFloods, morePremium := floods > b.lastFloods, premium > b.lastPremium
	b.sigSeen, b.lastFloods, b.lastPremium = true, floods, premium
	if first {
		return nil // waits from before the pass looked are not news
	}
	if morePremium {
		b.slowerLocked()
	}
	if moreFloods {
		return errIxWaitSignal
	}
	return nil
}

// ixPreviewFinal: errors no further try would change.
func ixPreviewFinal(err error) bool {
	s := err.Error()
	return errors.Is(err, errIxNoPreview) || s == errIxNoPreview.Error() || strings.Contains(s, "the message is gone") || strings.Contains(s, "cannot be read") ||
		strings.Contains(s, "MESSAGE_ID_INVALID") || strings.Contains(s, "CHANNEL_PRIVATE") || strings.Contains(s, "CHANNEL_INVALID")
}

func (a *App) ixPreviewAllFailed(ctx context.Context, j ixPreviewAllJob, err error) {
	tries := 1
	if ixPreviewFinal(err) {
		tries = ixPreviewTries
	}
	_, _ = a.db.Writer.ExecContext(ctx, `INSERT INTO ix_copy_thumb(ident,group_id,message_id,error,tries,at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(ident) DO UPDATE SET error=excluded.error, tries=ix_copy_thumb.tries+excluded.tries, at=excluded.at WHERE ix_copy_thumb.path=''`,
		j.ident, j.group, j.msg, err.Error(), tries, ixNowMs())
}

// ixPreviewAllKeep keeps a fetched preview; one the copy check kept in the
// meantime (perhaps with its recognition vector) is left as it is.
func (a *App) ixPreviewAllKeep(ctx context.Context, j ixPreviewAllJob, data []byte, w, h int) error {
	f := ixCopyFacts{Ident: j.ident, Group: j.group, Msg: j.msg}
	var kept bool
	if a.db.Reader.QueryRowContext(ctx, `SELECT path<>'' FROM ix_copy_thumb WHERE ident=?`, j.ident).Scan(&kept) == nil && kept {
		return nil
	}
	return a.ixPreviewSave(ctx, f, data, w, h, "", nil, "")
}

// ixPreviewAllRound fetches the previews of up to ixPreviewAllBatch files
// and says how long to wait before the next round.
func (a *App) ixPreviewAllRound(ctx context.Context) time.Duration {
	defer a.ixPreviewAllMeasure(ctx)
	if a.ixIsGone() {
		a.ixPreviewAllSetWhy("read-only archive: nothing is asked of Telegram")
		return 15 * time.Minute
	}
	if _, on := a.ixPreviewAllOn(ctx); !on {
		a.ixPreviewAllSetWhy("")
		return 5 * time.Minute
	}
	b := a.ixPreviewAllBox()
	b.mu.Lock()
	until := b.pauseUntil
	b.mu.Unlock()
	if d := time.Until(until); d > 0 {
		return d
	}
	jobs, err := a.ixPreviewAllJobs(ctx, ixPreviewAllBatch)
	if err != nil {
		if ctx.Err() == nil {
			a.ixLog("previews of files not downloaded: %v", err)
		}
		return 5 * time.Minute
	}
	if len(jobs) == 0 {
		a.ixPreviewAllSetWhy("")
		return ixPreviewAllIdle
	}
	if err := a.ixPreviewAllSignalled(); err != nil {
		return a.ixPreviewAllPause(err)
	}
	b.mu.Lock()
	b.premiumSeen = false
	b.mu.Unlock()
	pacer := &ixPreviewPacer{gap: a.ixPreviewAllGapNow}
	readers := map[string]ixCopyReader{} // by the account asked for
	var opened []ixCopyReader
	defer func() {
		for _, r := range opened {
			r.Close()
		}
	}()
	// open is the reader for an account: that account's, or the running
	// account's when it cannot be opened; nil when neither can.
	open := func(account string) ixCopyReader {
		if r, ok := readers[account]; ok {
			return r
		}
		raw, err := ixOpenCopyReader(a, ctx, account)
		if err != nil && account != "" {
			r := readers[""]
			if _, tried := readers[""]; !tried {
				if raw, err = ixOpenCopyReader(a, ctx, ""); err == nil {
					r = &ixPacedReader{ixCopyReader: raw, ctx: ctx, pacer: pacer, check: a.ixPreviewAllSignalled}
					opened = append(opened, r)
				}
				readers[""] = r
			}
			readers[account] = r
			return r
		}
		var r ixCopyReader
		if err == nil {
			r = &ixPacedReader{ixCopyReader: raw, ctx: ctx, pacer: pacer, check: a.ixPreviewAllSignalled}
			opened = append(opened, r)
		}
		readers[account] = r
		return r
	}
	a.ixPreviewAllSetWhy("")
	leftAlone := map[string]bool{} // chats left for this round
	for _, j := range jobs {
		if ctx.Err() != nil || a.ixIsGone() {
			return time.Minute
		}
		if leftAlone[j.group] {
			continue
		}
		r := open(j.account)
		if r == nil {
			leftAlone[j.group] = true // no account can read it now: the next round tries again
			continue
		}
		data, w, h, err := ixFetchPreview(r, j.body)
		if err != nil {
			switch {
			case ctx.Err() != nil || errors.Is(err, context.Canceled):
				return time.Minute
			case errors.Is(err, errIxWaitSignal) || ixSlowDown(err):
				return a.ixPreviewAllPause(err)
			}
			a.ixPreviewAllFailed(ctx, j, err)
			if ixPreviewChatWide(err) {
				a.ixPreviewAllSkipChat(j.group)
				leftAlone[j.group] = true
			}
			continue
		}
		if err := a.ixPreviewAllKeep(ctx, j, data, w, h); err != nil {
			if ctx.Err() == nil {
				a.ixLog("keeping a preview: %v", err)
			}
			return 5 * time.Minute
		}
		b.mu.Lock()
		b.info.FetchedRun++
		b.mu.Unlock()
	}
	if len(opened) == 0 {
		a.ixPreviewAllSetWhy("waiting for the Telegram engine")
		return 5 * time.Minute // quietly: the engine is stopped or starting
	}
	// A whole round without a speed-limit wait: back to the usual pace.
	b.mu.Lock()
	if !b.premiumSeen {
		b.slower = 0
	}
	b.mu.Unlock()
	return a.ixPreviewAllGapNow()
}

// ixRunPreviewsAll runs the pass in the background.
func (a *App) ixRunPreviewsAll(ctx context.Context) {
	wait := 2 * time.Minute
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if !ixVdupBackground.Load() {
			wait = 2 * time.Minute
			continue
		}
		wait = max(a.ixPreviewAllRound(ctx), time.Second)
	}
}
