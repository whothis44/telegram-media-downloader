package app

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/bits"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// Copies: measuring how much of what is left to download is very probably
// something the archive already has (or will have), and the tables of the
// wider copy checks (ix_held.go has the holding itself):
//
//   - the same Telegram file as one saved or queued (a forward): never
//     downloaded twice, by the library;
//   - probable copies held at the back of the queue (ix_held): a video as
//     long as a saved one, of the same shape, with the same preview, or the
//     lesser copies of a group of such videos waiting in the queue together;
//   - copies found by image recognition on Telegram's 320 px preview of a
//     queued video or large picture (ix_copies_ml.go);
//   - "confirmed" copies: frames sampled across a big queued video (only
//     the parts around them read from Telegram) match a saved video's
//     fingerprint at one time offset (ix_copies_sample.go).
//
// Nothing is ever skipped for good: held files only wait, and every
// message is archived in full either way.

const ixCopiesSchema = `
CREATE TABLE IF NOT EXISTS ix_copy_hold ( -- more about held queue items (ix_held), by queue item
	work_id      INTEGER PRIMARY KEY,
	how          TEXT    NOT NULL DEFAULT '', -- saved, queued, recognition
	of_work      INTEGER NOT NULL DEFAULT 0,  -- queued: the queue item it is probably a lesser copy of
	sim          REAL    NOT NULL DEFAULT 0,  -- recognition: likeness of the previews (cosine)
	confirm      TEXT    NOT NULL DEFAULT '', -- '', confirmed, unconfirmed
	confirm_note TEXT    NOT NULL DEFAULT '',
	checked_at   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS ix_copy_hold_of ON ix_copy_hold(of_work) WHERE of_work<>0;
-- saved files by size: exact re-uploads are looked for by it (ix_held.go, ix_copies_ml.go)
CREATE INDEX IF NOT EXISTS ix_downloads_size ON downloads(file_size) WHERE status='completed';
CREATE INDEX IF NOT EXISTS ix_downloads_tg_size ON downloads(telegram_media_size) WHERE status='completed';
CREATE TABLE IF NOT EXISTS ix_copy_thumb ( -- Telegram's small previews of queued files
	ident      TEXT    PRIMARY KEY, -- the file's Telegram identity (kind:id:size)
	group_id   TEXT    NOT NULL DEFAULT '',
	message_id INTEGER NOT NULL DEFAULT 0,
	path       TEXT    NOT NULL DEFAULT '', -- under the data folder's ixpreviews
	w          INTEGER NOT NULL DEFAULT 0,
	h          INTEGER NOT NULL DEFAULT 0,
	bytes      INTEGER NOT NULL DEFAULT 0,
	model      TEXT    NOT NULL DEFAULT '',
	vec        BLOB,
	error      TEXT    NOT NULL DEFAULT '',
	tries      INTEGER NOT NULL DEFAULT 0,
	at         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS ix_copy_thumb_msg ON ix_copy_thumb(group_id, message_id);
CREATE TABLE IF NOT EXISTS ix_copy_sample ( -- big queued videos checked by frames sampled across the file
	work_id    INTEGER PRIMARY KEY,
	ident      TEXT    NOT NULL DEFAULT '',
	size       INTEGER NOT NULL DEFAULT 0,  -- the file's size
	result     TEXT    NOT NULL DEFAULT '', -- confirmed, nomatch, failed
	note       TEXT    NOT NULL DEFAULT '',
	of_key     TEXT    NOT NULL DEFAULT '', -- confirmed: the saved file
	offset_s   REAL    NOT NULL DEFAULT 0,
	parts      INTEGER NOT NULL DEFAULT 0,  -- 1 MiB parts read from Telegram, all tries
	samples    INTEGER NOT NULL DEFAULT 0,
	tries      INTEGER NOT NULL DEFAULT 0,
	at         INTEGER NOT NULL DEFAULT 0,
	reason     TEXT    NOT NULL DEFAULT '', -- failed: why, a category (ix_copies_sample.go)
	final      INTEGER NOT NULL DEFAULT 0   -- failed for a reason in the file itself: not tried again
);
CREATE INDEX IF NOT EXISTS ix_copy_sample_ident_x ON ix_copy_sample(ident);
`

func ixEnsureCopiesSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, ixCopiesSchema); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, ixPartStoreSchema); err != nil { // ix_part_store.go (ix.12.11)
		return err
	}
	// Checks made before the reasons were recorded are tried again.
	// frames, dur_ms (ix.12.10): the sampled frames of a check and the video's
	// length, to compare again when more saved videos have fingerprints.
	return ixAddColumns(ctx, db, "ix_copy_sample", [][2]string{{"reason", "TEXT NOT NULL DEFAULT ''"}, {"final", "INTEGER NOT NULL DEFAULT 0"},
		{"frames", "BLOB"}, {"dur_ms", "INTEGER NOT NULL DEFAULT 0"}})
}

// ixParseCopySettings reads the copy settings sent by the dashboard.
func ixParseCopySettings(body map[string]any, s *ixSettings) {
	if v, ok := body["skipConfirmed"].(bool); ok {
		s.ConfirmedGoEarly = !v
	}
	if v, ok := body["copyThumbs"].(bool); ok {
		s.CopyThumbsOff = !v
	}
	if v, ok := body["previewsAll"].(string); ok && (v == "auto" || v == "on" || v == "off") {
		s.PreviewsAll = v
	}
}

// ---------- the facts compared ----------

// ixCopyFacts are what a probable copy is judged on.
type ixCopyFacts struct {
	ID    int64 // queue item (0: not queued)
	Group string
	Msg   int
	Ident string // Telegram identity, kind:id:size
	Type  string // video, photo
	Dur   int64  // ms
	W, H  int
	Size  int64
	PH    uint64 // hash of Telegram's stripped preview
	HasPH bool
	Date  int64 // first posted (a forward's original date)
	Vec   []float32
}

func (f ixCopyFacts) pixels() int64 { return int64(f.W) * int64(f.H) }

// ixCopyFactsOf reads the facts of a message's file.
func ixCopyFactsOf(msg *tg.Message) (ixCopyFacts, bool) {
	att, err := telegram.MessageAttachment(msg)
	if err != nil {
		return ixCopyFacts{}, false
	}
	f := ixCopyFacts{Group: att.GroupID, Msg: int(att.MessageID), Type: att.Type, Dur: att.Facts.DurationMs, W: att.Facts.Width, H: att.Facts.Height, Size: att.Identity.Size,
		Ident: att.Identity.Kind + ":" + att.Identity.ID + ":" + strconv.FormatInt(att.Identity.Size, 10), Date: int64(msg.Date)}
	if fwd, ok := msg.GetFwdFrom(); ok && fwd.Date > 0 && int64(fwd.Date) < f.Date {
		f.Date = int64(fwd.Date)
	}
	f.PH, f.HasPH = ixPreviewHash(att.Facts.Thumb)
	return f, true
}

func ixDecodeMessage(tl []byte) (*tg.Message, bool) {
	if len(tl) == 0 {
		return nil, false
	}
	var msg tg.Message
	if msg.Decode(&bin.Buffer{Buf: tl}) != nil {
		return nil, false
	}
	return &msg, true
}

func ixSameShape(a, b ixCopyFacts) bool {
	if a.W <= 0 || a.H <= 0 || b.W <= 0 || b.H <= 0 {
		return false
	}
	x, y := float64(a.W)/float64(a.H), float64(b.W)/float64(b.H)
	return math.Abs(x-y)/x <= ixAspectSlack
}

// ixCheapSame: two videos as long (to half a second), of the same shape,
// with matching stripped previews. Nothing is downloaded to know this.
func ixCheapSame(a, b ixCopyFacts) bool {
	if a.Dur <= 0 || b.Dur <= 0 || !a.HasPH || !b.HasPH {
		return false
	}
	d := a.Dur - b.Dur
	return d >= -500 && d <= 500 && ixSameShape(a, b) && bits.OnesCount64(a.PH^b.PH) <= ixPreviewBits
}

// ixPreviewBits: bits that may differ between the stripped previews of
// one video uploaded twice.
const ixPreviewBits = 6

// ixBetterCopy says whether a is the copy to keep rather than b: most
// pixels, then the longest, then most bytes, then the earliest post.
func ixBetterCopy(a, b ixCopyFacts) bool {
	if pa, pb := a.pixels(), b.pixels(); pa != pb {
		return pa > pb
	}
	if a.Dur/1000 != b.Dur/1000 {
		return a.Dur > b.Dur
	}
	if a.Size != b.Size {
		return a.Size > b.Size
	}
	if a.Date != b.Date {
		return a.Date < b.Date
	}
	return a.ID < b.ID
}

// ixCoversSaved: a saved copy at least as good as f, by the rule of the
// first held copies: no fewer pixels and at least 90% of the bytes.
func ixCoversSaved(saved, f ixCopyFacts) bool {
	return saved.pixels() >= f.pixels() && float64(saved.Size) >= 0.9*float64(f.Size)
}

// ixGroupCopies joins files that are probably the same (same reports
// whether two are), and returns the groups of two or more, best first.
func ixGroupCopies(fs []ixCopyFacts, same func(a, b ixCopyFacts) bool, window int64) [][]int {
	idx := make([]int, len(fs))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(x, y int) bool { return fs[idx[x]].Dur < fs[idx[y]].Dur })
	parent := make([]int, len(fs))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for x := range idx {
		a := fs[idx[x]]
		for y := x + 1; y < len(idx); y++ {
			b := fs[idx[y]]
			if b.Dur-a.Dur > max(window, a.Dur/50) {
				break
			}
			if same(a, b) {
				ra, rb := find(idx[x]), find(idx[y])
				if ra != rb {
					parent[rb] = ra
				}
			}
		}
	}
	byRoot := map[int][]int{}
	for i := range fs {
		r := find(i)
		byRoot[r] = append(byRoot[r], i)
	}
	var out [][]int
	for _, g := range byRoot {
		if len(g) < 2 {
			continue
		}
		sort.Slice(g, func(x, y int) bool { return ixBetterCopy(fs[g[x]], fs[g[y]]) })
		out = append(out, g)
	}
	sort.Slice(out, func(x, y int) bool { return out[x][0] < out[y][0] })
	return out
}

// ---------- the estimate ----------

type ixCopyCount struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

func (c *ixCopyCount) add(size int64) { c.Files++; c.Bytes += size }

// ixCopiesQueue: the download queue.
type ixCopiesQueue struct {
	All       ixCopyCount `json:"all"`       // waiting or downloading
	Same      ixCopyCount `json:"same"`      // the same Telegram file is saved, or queued before it: never downloaded twice
	Held      ixCopyCount `json:"held"`      // held at the back of the queue, all reasons
	VsSaved   ixCopyCount `json:"vsSaved"`   // ...as a copy of a saved video
	VsQueued  ixCopyCount `json:"vsQueued"`  // ...as a lesser copy of one waiting in the queue
	Recog     ixCopyCount `json:"recog"`     // ...found by image recognition on the preview
	Confirmed ixCopyCount `json:"confirmed"` // ...and the first frames matched
}

// ixCopiesKnown: everything seen in messages and not downloaded.
type ixCopiesKnown struct {
	Measured bool        `json:"measured"`
	Missing  ixCopyCount `json:"missing"` // not downloaded
	Same     ixCopyCount `json:"same"`    // the same Telegram file as a saved file or another message (counted once)
	Held     ixCopyCount `json:"held"`
	Likely   ixCopyCount `json:"likely"` // videos the checks would hold: copies of saved ones, or lesser copies of each other
	At       int64       `json:"at,omitempty"`
}

type ixCopiesInfo struct {
	Queue     ixCopiesQueue  `json:"queue"`
	Known     ixCopiesKnown  `json:"known"`
	Previews  int64          `json:"previews"`  // Telegram previews kept for files not downloaded
	Threshold float64        `json:"threshold"` // likeness of previews that makes a probable copy
	Calib     *ixCopyCalib   `json:"calibration,omitempty"`
	Sampled   ixSampleTotals `json:"sampled"` // big videos checked by sampling (ix_copies_sample.go)
	At        int64          `json:"at,omitempty"`
}

type ixCopiesBox struct {
	mu    sync.Mutex
	info  ixCopiesInfo
	known ixCopiesKnown
	stamp string // the browser index the known part was measured on
}

var ixCopiesState sync.Map // *App -> *ixCopiesBox

func (a *App) ixCopies() *ixCopiesBox {
	v, _ := ixCopiesState.LoadOrStore(a, &ixCopiesBox{})
	return v.(*ixCopiesBox)
}

// ixCopiesNow is the last measurement (made in the background).
func (a *App) ixCopiesNow() ixCopiesInfo {
	box := a.ixCopies()
	box.mu.Lock()
	defer box.mu.Unlock()
	return box.info
}

// ixCopiesMeasure measures the queue now, with the last survey of the
// messages, and keeps the result.
func (a *App) ixCopiesMeasure(ctx context.Context) ixCopiesInfo {
	info := ixCopiesInfo{Threshold: a.ixCopyThreshold(ctx), Calib: a.ixCopyCalibLast(ctx), At: time.Now().UnixMilli()}
	info.Queue = a.ixCopiesQueueNow(ctx)
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_copy_thumb WHERE path<>''`).Scan(&info.Previews)
	info.Sampled = a.ixSampleTotalsNow(ctx)
	box := a.ixCopies()
	box.mu.Lock()
	info.Known = box.known
	box.info = info
	box.mu.Unlock()
	return info
}

func (a *App) ixCopiesQueueNow(ctx context.Context) ixCopiesQueue {
	var q ixCopiesQueue
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT w.id, w.identity, w.file_size, COALESCE(h.reason,''), COALESCE(h.released_at,-1), COALESCE(c.how,''), COALESCE(c.confirm,'')
		FROM tgdl_work w LEFT JOIN ix_held h ON h.work_id=w.id LEFT JOIN ix_copy_hold c ON c.work_id=w.id
		WHERE w.status IN ('pending','processing') ORDER BY w.id`)
	if err != nil {
		return q
	}
	type row struct {
		ident          string
		size, released int64
		reason, how    string
		confirm        string
	}
	var list []row
	for rows.Next() {
		var r row
		var id int64
		if rows.Scan(&id, &r.ident, &r.size, &r.reason, &r.released, &r.how, &r.confirm) == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	seen := map[string]bool{}
	for _, r := range list {
		q.All.add(r.size)
		if r.ident != "" {
			if seen[r.ident] || a.ixIdentSaved(ctx, r.ident) {
				q.Same.add(r.size)
				continue
			}
			seen[r.ident] = true
		}
		if r.reason == "" || r.released != 0 {
			continue
		}
		q.Held.add(r.size)
		switch r.how {
		case "queued":
			q.VsQueued.add(r.size)
		case "recognition":
			q.Recog.add(r.size)
		default:
			q.VsSaved.add(r.size)
		}
		if r.confirm == "confirmed" {
			q.Confirmed.add(r.size)
		}
	}
	return q
}

// ixIdentSaved: the file with this queue identity (kind\0id\0size) is saved.
func (a *App) ixIdentSaved(ctx context.Context, ident string) bool {
	parts := strings.Split(ident, "\x00")
	if len(parts) != 3 {
		return false
	}
	var n int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM downloads WHERE telegram_media_kind=? AND telegram_media_id=? AND status='completed' AND file_path IS NOT NULL AND file_path<>'' LIMIT 1`, parts[0], parts[1]).Scan(&n)
	return n > 0
}

// ixCopiesSurvey measures the files seen in messages and not downloaded:
// which are the same Telegram file as another, which are held, and which
// videos the checks would hold once queued. It reads every such video's
// archived record, so it runs in the background when the index changes.
func (a *App) ixCopiesSurvey(ctx context.Context, m *ixModel) (ixCopiesKnown, error) {
	k := ixCopiesKnown{Measured: true, At: time.Now().UnixMilli()}
	if m == nil {
		return k, nil
	}
	missingVideo := map[string]int64{} // group:msg -> size, videos not held and not the same file as another
	for ci := range m.Clusters {
		saved, first := false, true
		for _, ii := range m.Clusters[ci].Items {
			if m.Items[ii].Key != "" {
				saved = true
			}
		}
		for _, ii := range m.Clusters[ci].Items {
			it := &m.Items[ii]
			switch it.Status {
			case "held":
				k.Missing.add(it.Size)
				k.Held.add(it.Size)
			case "missing":
				k.Missing.add(it.Size)
				if saved || !first {
					k.Same.add(it.Size)
					continue
				}
				first = false
				if it.Type == "video" && it.Size >= ixHoldMinSize {
					missingVideo[it.Group+":"+strconv.Itoa(it.Msg)] = it.Size
				}
			}
		}
	}
	if len(missingVideo) == 0 {
		return k, nil
	}
	saved, err := a.ixSavedVideoFacts(ctx)
	if err != nil {
		return k, err
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, message_id, tl FROM ix_messages WHERE media_type='video' AND service=''`)
	if err != nil {
		return k, err
	}
	var queued []ixCopyFacts
	for rows.Next() {
		var g string
		var msg int
		var tl []byte
		if rows.Scan(&g, &msg, &tl) != nil {
			continue
		}
		if _, ok := missingVideo[g+":"+strconv.Itoa(msg)]; !ok {
			continue
		}
		mm, ok := ixDecodeMessage(tl)
		if !ok {
			continue
		}
		if f, ok := ixCopyFactsOf(mm); ok && f.Dur > 0 && f.HasPH {
			queued = append(queued, f)
		}
	}
	rows.Close()
	if ctx.Err() != nil {
		return k, ctx.Err()
	}
	var rest []ixCopyFacts
	for _, f := range queued {
		if ixSavedCopyOf(saved, f) >= 0 {
			k.Likely.add(f.Size)
			continue
		}
		rest = append(rest, f)
	}
	for _, g := range ixGroupCopies(rest, ixCheapSame, 500) {
		for _, i := range g[1:] {
			k.Likely.add(rest[i].Size)
		}
	}
	return k, nil
}

// ixSavedVideos: saved videos with what the cheap check needs, by length.
type ixSavedVideo struct {
	ixCopyFacts
	Path string
	Key  string
}

func (a *App) ixSavedVideoFacts(ctx context.Context) ([]ixSavedVideo, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT file_path, COALESCE(file_hash,''), media_width, media_height, COALESCE(file_size,0), media_duration_ms, telegram_thumb FROM downloads
		WHERE file_type='video' AND status='completed' AND file_path IS NOT NULL AND file_path<>'' AND media_duration_ms>0
		AND media_width IS NOT NULL AND media_height IS NOT NULL AND telegram_thumb IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ixSavedVideo
	for rows.Next() {
		var v ixSavedVideo
		var hash string
		var thumb []byte
		if rows.Scan(&v.Path, &hash, &v.W, &v.H, &v.Size, &v.Dur, &thumb) != nil {
			continue
		}
		if v.PH, v.HasPH = ixPreviewHash(thumb); !v.HasPH {
			continue
		}
		v.Type = "video"
		v.Key = ixFileKey(hash, ixNormPath(v.Path))
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dur < out[j].Dur })
	return out, rows.Err()
}

// ixSavedCopyOf finds a saved video f is very probably a copy of (and at
// least as good): its index, or -1.
func ixSavedCopyOf(saved []ixSavedVideo, f ixCopyFacts) int {
	i := sort.Search(len(saved), func(i int) bool { return saved[i].Dur >= f.Dur-500 })
	for ; i < len(saved) && saved[i].Dur <= f.Dur+500; i++ {
		if ixCheapSame(saved[i].ixCopyFacts, f) && ixCoversSaved(saved[i].ixCopyFacts, f) {
			return i
		}
	}
	return -1
}

// ---------- background ----------

// ixRunCopies keeps the measurement current and runs the wider checks:
// previews compared by recognition, and frames sampled across big videos.
func (a *App) ixRunCopies(ctx context.Context) {
	a.ixLoadPreviewIdents(ctx)
	wait := 90 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 2 * time.Minute
		if !ixVdupBackground.Load() {
			continue
		}
		a.ixCopiesRound(ctx)
	}
}

// ixCopiesRound is one pass of the background work.
func (a *App) ixCopiesRound(ctx context.Context) {
	if m := a.ixModelLast(); m != nil {
		box := a.ixCopies()
		box.mu.Lock()
		stale := box.stamp != m.Stamp || time.Since(time.UnixMilli(box.known.At)) > time.Hour
		box.mu.Unlock()
		if stale {
			if k, err := a.ixCopiesSurvey(ctx, m); err == nil {
				box.mu.Lock()
				box.known, box.stamp = k, m.Stamp
				box.mu.Unlock()
			} else if ctx.Err() == nil {
				a.ixLog("copies survey: %v", err)
			}
		}
	}
	a.ixCopiesWork(ctx)
	a.ixCopiesMeasure(ctx)
}

// ixCopiesWork is replaceable in tests; the real one runs the preview and
// first-part checks.
var ixCopiesWorkHook = func(a *App, ctx context.Context) {}

func (a *App) ixCopiesWork(ctx context.Context) { ixCopiesWorkHook(a, ctx) }

func registerIxCopiesRoutes(mux *http.ServeMux, a *App) {
	h := func(f http.HandlerFunc) http.Handler { return a.requireAdmin(f) }
	mux.Handle("GET /api/ix/copies", h(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.ixCopiesMeasure(r.Context()))
	}))
	mux.Handle("GET /api/ix/copies/preview", h(a.handleIxCopyPreview))
	mux.Handle("POST /api/ix/copies/calibrate", h(a.handleIxCopyCalibrate))
}

// ixCopiesDiag is the diagnostics report's part: numbers only.
func (a *App) ixCopiesDiag(ctx context.Context) []string {
	c := a.ixCopiesMeasure(ctx)
	mb := func(n int64) int64 { return n >> 20 }
	q, k := c.Queue, c.Known
	out := []string{fmt.Sprintf("  queue: %d files / %d MB · same Telegram file as one saved or queued %d / %d MB · held %d / %d MB (copies of saved %d, lesser copies of queued %d, by recognition %d, matched by sampled frames %d / %d MB)",
		q.All.Files, mb(q.All.Bytes), q.Same.Files, mb(q.Same.Bytes), q.Held.Files, mb(q.Held.Bytes), q.VsSaved.Files, q.VsQueued.Files, q.Recog.Files, q.Confirmed.Files, mb(q.Confirmed.Bytes))}
	if k.Measured {
		out = append(out, fmt.Sprintf("  seen in messages, not downloaded: %d files / %d MB · same Telegram file as another %d / %d MB · held %d / %d MB · probable copies by the video checks %d / %d MB",
			k.Missing.Files, mb(k.Missing.Bytes), k.Same.Files, mb(k.Same.Bytes), k.Held.Files, mb(k.Held.Bytes), k.Likely.Files, mb(k.Likely.Bytes)))
	} else {
		out = append(out, "  seen in messages, not downloaded: not measured yet")
	}
	s := c.Sampled
	failed := ""
	if s.Failed > 0 {
		failed = fmt.Sprintf(" (%s; %d to try again later)", s.failedText(), s.Retry)
	}
	out = append(out, fmt.Sprintf("  big videos sampled: checked %d big videos using %d MB instead of %.1f GB (%d more took the answer for the same file); %d matched by sampled frames; %d could not be checked%s · previews kept %d · likeness threshold %.3f",
		s.Checked, s.PartsMB, float64(s.SizeBytes)/(1<<30), s.Reused, s.Confirmed, s.Failed, failed, c.Previews, c.Threshold))
	out = append(out, a.ixSampleRateDiag(ctx))
	out = append(out, "  "+a.ixDlCheckLine(ctx)) // ix.12.13
	out = append(out, a.ixPartsDiag(ctx))        // ix_part_store.go (ix.12.11)
	out = append(out, a.ixPreviewAllDiag(ctx))
	return out
}
