package app

import (
	"context"
	"errors"
	"image"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// Previews of everything not downloaded (ix_previews_all.go), with
// invented messages and a fake reader: nothing reaches Telegram.

// ixAllReader serves invented 320 px previews and records what was asked.
type ixAllReader struct {
	mu        sync.Mutex
	preview   []byte
	asked     []int64 // file IDs, in order
	at        []time.Time
	sizes     []string // the preview size asked for
	expired   map[int64]bool
	fail      map[int64]error // the next request for it fails so
	refreshed int
	onAsk     func(n int) // after the nth request (1-based)
}

func (r *ixAllReader) FilePart(a telegram.Attachment, _ int64) ([]byte, error) {
	r.mu.Lock()
	id, _ := strconv.ParseInt(a.Identity.ID, 10, 64)
	r.asked = append(r.asked, id)
	r.at = append(r.at, time.Now())
	switch l := a.Location.(type) {
	case *tg.InputDocumentFileLocation:
		r.sizes = append(r.sizes, "document:"+l.ThumbSize)
	case *tg.InputPhotoFileLocation:
		r.sizes = append(r.sizes, "photo:"+l.ThumbSize)
	}
	n, hook := len(r.asked), r.onAsk
	var err error
	if r.expired[id] {
		delete(r.expired, id)
		err = errors.New("rpc error code 400: FILE_REFERENCE_EXPIRED")
	} else if e := r.fail[id]; e != nil {
		delete(r.fail, id)
		err = e
	}
	r.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	if err != nil {
		return nil, err
	}
	return r.preview, nil
}

func (r *ixAllReader) RefreshMessage(m *tg.Message) (*tg.Message, error) {
	r.mu.Lock()
	r.refreshed++
	r.mu.Unlock()
	return m, nil
}

func (r *ixAllReader) Close() {}

func ixAllUse(t *testing.T, r *ixAllReader, gap time.Duration) *[2]int {
	t.Helper()
	saved, savedGap, savedSig := ixOpenCopyReader, ixPreviewAllGap, ixPreviewAllSignal
	sig := &[2]int{}
	var mu sync.Mutex
	ixOpenCopyReader = func(*App, context.Context, string) (ixCopyReader, error) { return r, nil }
	ixPreviewAllGap = gap
	ixPreviewAllSignal = func(*App) (int, int) { mu.Lock(); defer mu.Unlock(); return sig[0], sig[1] }
	t.Cleanup(func() { ixOpenCopyReader, ixPreviewAllGap, ixPreviewAllSignal = saved, savedGap, savedSig })
	return sig
}

// ixAllDeadline sets a deletion date (the pass is on by default then).
func ixAllDeadline(t *testing.T, a *App, ctx context.Context) {
	t.Helper()
	a.ixSaveFreeze(ctx, ixFreezeInfo{FreezeState: telegram.FreezeState{Until: time.Now().Add(27 * 24 * time.Hour).Unix()}})
}

// ixTestPhoto is an invented photo message in channel ch.
func ixTestPhoto(id int, ch int64, photoID int64, date int) *tg.Message {
	p := &tg.Photo{ID: photoID, AccessHash: photoID * 7, FileReference: []byte{3}, DCID: 2, Date: date,
		Sizes: []tg.PhotoSizeClass{&tg.PhotoStrippedSize{Type: "i", Bytes: []byte{1, 30, 40}}, &tg.PhotoSize{Type: "m", W: 320, H: 240, Size: 20000}, &tg.PhotoSize{Type: "y", W: 1280, H: 960, Size: 3 << 20}}}
	return &tg.Message{ID: id, Date: date, PeerID: &tg.PeerChannel{ChannelID: ch}, Media: &tg.MessageMediaPhoto{Photo: p}}
}

func ixTestIn(m *tg.Message, ch int64) *tg.Message {
	c := *m
	c.PeerID = &tg.PeerChannel{ChannelID: ch}
	return &c
}

const ixAllPublic = 4343

func ixAllPublicGroup(t *testing.T, a *App) {
	t.Helper()
	g := strconv.FormatInt(-1000000000000-ixAllPublic, 10)
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_group_info(group_id,title,username) VALUES(?,?,?)`, g, "Invented public channel", "invented_public"); err != nil {
		t.Fatal(err)
	}
}

func ixAllIdent(m *tg.Message) string {
	f, _ := ixCopyFactsOf(m)
	return f.Ident
}

func TestIxPreviewsAllOrderAndSkips(t *testing.T) {
	a, ctx := ixTestApp(t)
	ixAllDeadline(t, a, ctx)
	ixAllPublicGroup(t, a)
	preview := ixTestJPEG(t, ixShrink(ixTestScene(61, 640, 480), 320), 80)
	stripped := ixTestStripped(t, ixTestScene(62, 40, 23))
	// A private chat (ixTestChannel has no public username) and a public one.
	vidA := ixTestVideo(1, 701, 80<<20, 30, 1280, 720, stripped, 100) // queued
	picB := ixTestPhoto(2, ixTestChannel, 702, 300)                   // its reference expired
	vidC := ixTestVideo(3, 703, 20<<20, 12, 1280, 720, stripped, 200) // downloaded
	fwdC := ixTestVideo(4, 703, 20<<20, 12, 1280, 720, stripped, 400) // the same file as C: saved
	picK := ixTestPhoto(5, ixTestChannel, 709, 350)                   // its preview is kept already
	picH := ixTestPhoto(6, ixTestChannel, 708, 500)                   // deleted on Telegram
	pubP1 := ixTestIn(ixTestVideo(7, 711, 1<<20, 5, 640, 360, stripped, 600), ixAllPublic)
	pubP2 := ixTestPhoto(8, ixAllPublic, 712, 50)
	pubDup := ixTestIn(ixTestVideo(9, 701, 80<<20, 30, 1280, 720, stripped, 700), ixAllPublic) // a forward of A
	for _, m := range []*tg.Message{vidA, picB, vidC, fwdC, picK, picH, pubP1, pubP2, pubDup} {
		ixTestArchive(t, a, m)
	}
	ixTestQueue(t, a, vidA)
	ixTestSave(t, a, vidC, "c/v3.mp4", ixSixtyFour("e"))
	if _, err := a.db.Writer.Exec(`UPDATE ix_messages SET deleted_at=1 WHERE message_id=6`); err != nil {
		t.Fatal(err)
	}
	fk, _ := ixCopyFactsOf(picK)
	if err := a.ixPreviewSave(ctx, fk, preview, 320, 240, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	r := &ixAllReader{preview: preview, expired: map[int64]bool{702: true}}
	ixAllUse(t, r, 0)

	a.ixPreviewAllRound(ctx)
	want := []int64{702, 702, 701, 708, 711, 712} // private newest first (B asked again after a fresh copy), deleted last, then public
	if len(r.asked) != len(want) {
		t.Fatalf("asked %v, want %v", r.asked, want)
	}
	for i := range want {
		if r.asked[i] != want[i] {
			t.Fatalf("asked %v, want %v", r.asked, want)
		}
	}
	if r.refreshed != 1 {
		t.Fatalf("message read again %d times", r.refreshed)
	}
	if r.sizes[2] != "document:m" || r.sizes[0] != "photo:m" {
		t.Fatalf("preview sizes asked: %v", r.sizes)
	}
	for _, m := range []*tg.Message{vidA, picB, picH, pubP1, pubP2} {
		if ixCopyPreviewURL(ixAllIdent(m)) == "" {
			t.Fatalf("no preview kept for message %d", m.ID)
		}
	}
	if ixCopyPreviewURL(ixAllIdent(vidC)) != "" {
		t.Fatal("a preview for a downloaded file")
	}
	// The browser shows it for the file not downloaded.
	m, err := a.ixRebuildModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ii := m.Posts[m.ByMsg[ixTestGroup+":2"]].Items[0]
	if j := m.jitem(ii); !j.PreviewOnly || j.Thumb != ixCopyPreviewURL(ixAllIdent(picB)) {
		t.Fatalf("browser item: %+v", j)
	}
	p := a.ixPreviewAllNow()
	if !p.On || p.Mode != "auto" || p.Known != 6 || p.Fetched != 6 || p.Remaining != 0 || p.FetchedRun != 5 {
		t.Fatalf("counts: %+v", p)
	}
	// Nothing is fetched twice.
	a.ixPreviewAllRound(ctx)
	if len(r.asked) != len(want) {
		t.Fatalf("asked again: %v", r.asked)
	}
	if line := a.ixPreviewAllDiag(ctx); line == "" {
		t.Fatal("no diagnostics line")
	}
}

func TestIxPreviewsAllSettingAndReadOnly(t *testing.T) {
	a, ctx := ixTestApp(t)
	ixTestArchive(t, a, ixTestPhoto(1, ixTestChannel, 801, 100))
	r := &ixAllReader{preview: ixTestJPEG(t, image.NewRGBA(image.Rect(0, 0, 32, 24)), 80)}
	ixAllUse(t, r, 0)
	// No deletion date: auto is off.
	a.ixPreviewAllRound(ctx)
	if len(r.asked) != 0 || a.ixPreviewAllNow().On {
		t.Fatalf("asked without a deletion date: %v", r.asked)
	}
	// Off stays off with one.
	ixAllDeadline(t, a, ctx)
	s := a.ixLoadSettings(ctx)
	ixParseCopySettings(map[string]any{"previewsAll": "off"}, &s)
	if err := a.ixSaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	a.ixPreviewAllRound(ctx)
	if len(r.asked) != 0 {
		t.Fatal("asked while off")
	}
	// A read-only archive asks nothing.
	s.PreviewsAll = "on"
	if err := a.ixSaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	a.ixSetGone(ctx, ixGoneInfo{Gone: true, Reason: "telegram"})
	a.ixPreviewAllRound(ctx)
	if len(r.asked) != 0 {
		t.Fatal("asked while read-only")
	}
	a.ixSetGone(ctx, ixGoneInfo{})
	// Without the engine: quietly later, nothing marked as failed.
	ixOpenCopyReader = func(*App, context.Context, string) (ixCopyReader, error) {
		return nil, errors.New("the Telegram engine is not running")
	}
	a.ixPreviewAllRound(ctx)
	var rows int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_copy_thumb`).Scan(&rows)
	if rows != 0 || a.ixPreviewAllNow().Why == "" {
		t.Fatalf("engine missing: %d rows, %+v", rows, a.ixPreviewAllNow())
	}
	ixOpenCopyReader = func(*App, context.Context, string) (ixCopyReader, error) { return r, nil }
	a.ixPreviewAllRound(ctx)
	if len(r.asked) != 1 || a.ixPreviewAllNow().Remaining != 0 {
		t.Fatalf("on: %v %+v", r.asked, a.ixPreviewAllNow())
	}
}

func TestIxPreviewsAllRateAndWaits(t *testing.T) {
	a, ctx := ixTestApp(t)
	ixAllDeadline(t, a, ctx)
	for i := 1; i <= 6; i++ {
		ixTestArchive(t, a, ixTestPhoto(i, ixTestChannel, int64(900+i), 1000-i))
	}
	r := &ixAllReader{preview: ixTestJPEG(t, image.NewRGBA(image.Rect(0, 0, 32, 24)), 80)}
	gap := 40 * time.Millisecond
	sig := ixAllUse(t, r, gap)
	// The downloads are told to wait after the second request: the pass stops.
	r.onAsk = func(n int) {
		if n == 2 {
			sig[0]++
		}
	}
	if d := a.ixPreviewAllRound(ctx); d < ixPreviewAllFloodPause-time.Second {
		t.Fatalf("paused for %v", d)
	}
	if len(r.asked) != 2 || r.at[1].Sub(r.at[0]) < gap-5*time.Millisecond {
		t.Fatalf("asked %v at %v", r.asked, r.at)
	}
	if p := a.ixPreviewAllNow(); p.PausedUntil < time.Now().Add(4*time.Minute).UnixMilli() || p.Remaining != 4 {
		t.Fatalf("pause: %+v", p)
	}
	a.ixPreviewAllRound(ctx)
	if len(r.asked) != 2 {
		t.Fatal("asked while paused")
	}
	// Telegram's speed limit on its own request: a pause, twice the gap, no try counted.
	box := a.ixPreviewAllBox()
	box.mu.Lock()
	box.pauseUntil = time.Time{}
	box.mu.Unlock()
	r.onAsk = nil
	r.fail = map[int64]error{903: errors.New("rpc error code 420: FLOOD_PREMIUM_WAIT_30")}
	if d := a.ixPreviewAllRound(ctx); d < ixPreviewAllPremiumMin-time.Second {
		t.Fatalf("speed limit paused for %v", d)
	}
	if len(r.asked) != 3 || a.ixPreviewAllGapNow() != 2*gap {
		t.Fatalf("speed limit: asked %v, gap %v", r.asked, a.ixPreviewAllGapNow())
	}
	var tries int
	_ = a.db.Reader.QueryRow(`SELECT COALESCE(SUM(tries),0) FROM ix_copy_thumb WHERE path=''`).Scan(&tries)
	if tries != 0 {
		t.Fatalf("a wait counted as a failed try: %d", tries)
	}
	// The downloads meet the speed limit: slower again, but it goes on.
	box.mu.Lock()
	box.pauseUntil = time.Time{}
	box.mu.Unlock()
	r.onAsk = func(n int) {
		if n == 4 {
			sig[1]++
		}
	}
	a.ixPreviewAllRound(ctx)
	if len(r.asked) != 7 {
		t.Fatalf("asked %v", r.asked)
	}
	if d := r.at[6].Sub(r.at[5]); d < 4*gap-5*time.Millisecond {
		t.Fatalf("not slower after the speed limit: %v", d)
	}
	if a.ixPreviewAllNow().Remaining != 0 {
		t.Fatalf("left: %+v", a.ixPreviewAllNow())
	}
	// A FLOOD_WAIT longer than the usual pause is waited out in full.
	if d := a.ixPreviewAllPause(errors.New("rpc error code 420: FLOOD_WAIT (900)")); d != 900*time.Second {
		t.Fatalf("flood wait pause %v", d)
	}
	// A file Telegram has no preview of is not asked for again.
	box.mu.Lock()
	box.pauseUntil = time.Time{}
	box.mu.Unlock()
	bare := &tg.Message{ID: 20, Date: 5, PeerID: &tg.PeerChannel{ChannelID: ixTestChannel}, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 990, DCID: 2, Size: 9 << 20, MimeType: "video/mp4"}}}
	ixTestArchive(t, a, bare)
	a.ixPreviewAllRound(ctx)
	a.ixPreviewAllRound(ctx)
	if p := a.ixPreviewAllNow(); len(r.asked) != 7 || p.Unavailable != 1 || p.Remaining != 0 {
		t.Fatalf("no preview: %v %+v", r.asked, p)
	}
}

// The copy check looks at a preview the pass kept without asking Telegram.
func TestIxCopyCheckReusesKeptPreviews(t *testing.T) {
	a, ctx := ixTestApp(t)
	ixAllDeadline(t, a, ctx)
	ixTestRecognitionOn(t, a, ctx)
	scene := ixTestScene(71, 1280, 720)
	v := ixTestVideo(1, 1501, 200<<20, 60, 1280, 720, ixTestStripped(t, ixTestScene(72, 40, 23)), 1700000000)
	ixTestArchive(t, a, v)
	ixTestQueue(t, a, v)
	r := &ixAllReader{preview: ixTestJPEG(t, ixShrink(scene, 320), 80)}
	ixAllUse(t, r, 0)
	saved := ixPreviewGap
	ixPreviewGap = 0
	t.Cleanup(func() { ixPreviewGap = saved })
	a.ixPreviewAllRound(ctx)
	if len(r.asked) != 1 {
		t.Fatalf("asked %v", r.asked)
	}
	if _, err := a.ixRebuildModel(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.ixPreviewRound(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.asked) != 1 {
		t.Fatalf("the copy check asked Telegram again: %v", r.asked)
	}
	var model string
	var n int
	_ = a.db.Reader.QueryRow(`SELECT model, length(vec) FROM ix_copy_thumb WHERE ident=?`, ixAllIdent(v)).Scan(&model, &n)
	if model != "test-model" || n == 0 {
		t.Fatalf("not looked at: %q %d", model, n)
	}
}

// An account that cannot be opened (removed, or not running) no longer
// ends every round on the same chat: the running account reads it. A
// chat Telegram refuses as a whole is asked once, then left alone.
func TestIxPreviewsAllAccountFallbackAndChatWideErrors(t *testing.T) {
	a, ctx := ixTestApp(t)
	ixAllDeadline(t, a, ctx)
	const other = 4244
	picA1, picA2 := ixTestPhoto(1, ixTestChannel, 1101, 500), ixTestPhoto(2, ixTestChannel, 1102, 400)
	var picsB []*tg.Message
	for i := 0; i < 3; i++ {
		picsB = append(picsB, ixTestPhoto(10+i, other, int64(1201+i), 900-100*i))
	}
	for _, m := range append([]*tg.Message{picA1, picA2}, picsB...) {
		ixTestArchive(t, a, m)
	}
	ixTestQueue(t, a, picA1)
	if _, err := a.db.Writer.Exec(`UPDATE tgdl_work SET account_id='removed'`); err != nil {
		t.Fatal(err)
	}
	r := &ixAllReader{preview: ixTestJPEG(t, image.NewRGBA(image.Rect(0, 0, 32, 24)), 80),
		fail: map[int64]error{1201: errors.New("rpc error code 406: CHANNEL_PRIVATE")}}
	ixAllUse(t, r, 0)
	var opens []string
	ixOpenCopyReader = func(_ *App, _ context.Context, account string) (ixCopyReader, error) {
		opens = append(opens, account)
		if account == "removed" {
			return nil, errors.New("account removed cannot resolve Telegram messages")
		}
		return r, nil
	}
	a.ixPreviewAllRound(ctx)
	if len(r.asked) != 3 || r.asked[0] != 1201 || r.asked[1] != 1101 || r.asked[2] != 1102 {
		t.Fatalf("asked %v (opened %v)", r.asked, opens)
	}
	if len(opens) != 2 || opens[0] != "" && opens[1] != "" {
		t.Fatalf("opened %v", opens)
	}
	if p := a.ixPreviewAllNow(); p.ChatsLeft != 1 || p.Why != "" {
		t.Fatalf("after the round: %+v", p)
	}
	// The refused chat is left alone in the next rounds.
	a.ixPreviewAllRound(ctx)
	if len(r.asked) != 3 {
		t.Fatalf("asked the refused chat again: %v", r.asked)
	}
	if line := a.ixPreviewAllDiag(ctx); !strings.Contains(line, "chats left alone for now 1") {
		t.Fatalf("diagnostics: %s", line)
	}
	// Once the time is up, it is tried again.
	box := a.ixPreviewAllBox()
	box.mu.Lock()
	for g := range box.skipChats {
		box.skipChats[g] = time.Now().Add(-time.Second)
	}
	box.mu.Unlock()
	a.ixPreviewAllRound(ctx)
	if len(r.asked) != 5 {
		t.Fatalf("after the time: %v", r.asked)
	}
}
