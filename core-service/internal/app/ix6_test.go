package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// ---------- pictures ----------

func ixTestScene(seed int64, w, h int) *image.RGBA {
	r := rand.New(rand.NewSource(seed))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	type blob struct {
		x, y, rad float64
		c         color.RGBA
	}
	var bl []blob
	for i := 0; i < 12; i++ {
		bl = append(bl, blob{r.Float64(), r.Float64(), 0.05 + r.Float64()*0.25, color.RGBA{uint8(r.Intn(255)), uint8(r.Intn(255)), uint8(r.Intn(255)), 255}})
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			fx, fy := float64(x)/float64(w), float64(y)/float64(h)
			c := color.RGBA{uint8(40 + 100*fx), uint8(60 + 80*fy), 90, 255}
			for _, b := range bl {
				if math.Hypot(fx-b.x, fy-b.y) < b.rad {
					c = b.c
				}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func ixTestJPEG(t *testing.T, img image.Image, q int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func ixTestResize(src *image.RGBA, w, h int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			out.Set(x, y, src.At(x*sw/w, y*sh/h))
		}
	}
	return out
}

func TestIxPDQMatchesCopiesNotOtherPictures(t *testing.T) {
	a := ixTestScene(1, 1280, 960)
	hash := func(data []byte) (ixPDQ, int, []byte) {
		p, err := ixReadPicture(data)
		if err != nil {
			t.Fatal(err)
		}
		h, q, buf := ixPDQHash(p.Luma, p.Rows, p.Cols)
		return h, q, ixLuma32(buf)
	}
	ha, qa, la := hash(ixTestJPEG(t, a, 92))
	hb, _, lb := hash(ixTestJPEG(t, a, 35))
	hc, _, lc := hash(ixTestJPEG(t, ixTestResize(a, 640, 480), 70))
	hd, _, ld := hash(ixTestJPEG(t, ixTestScene(2, 1280, 960), 92))
	if qa < ixPDQMinQual {
		t.Fatalf("quality %d", qa)
	}
	if d := ha.Distance(hb); d > ixPDQAuto {
		t.Fatalf("recompressed copy is %d bits away", d)
	}
	if d := ha.Distance(hc); d > ixPDQAuto {
		t.Fatalf("smaller copy is %d bits away", d)
	}
	if d := ha.Distance(hd); d <= ixPDQMatch {
		t.Fatalf("another picture is only %d bits away", d)
	}
	if ixSSIM(la, lb, 32) < ixSSIMSame || ixSSIM(la, lc, 32) < ixSSIMSame || ixSSIM(la, ld, 32) > 0.5 {
		t.Fatal("small grey copies do not compare as expected")
	}
	if v := ixPictureVerdict(25, 4.0/3, 4.0/3, la, lc); v != "verified" {
		t.Fatalf("close and alike: %q", v)
	}
	if v := ixPictureVerdict(25, 4.0/3, 16.0/9, la, lc); v != "suggest" {
		t.Fatalf("close, another shape: %q", v)
	}
	flat := image.NewGray(image.Rect(0, 0, 300, 300))
	for i := range flat.Pix {
		flat.Pix[i] = 128
	}
	if _, q, _ := hash(ixTestJPEG(t, flat, 90)); q >= ixPDQMinQual {
		t.Fatalf("a flat picture has quality %d", q)
	}
	b, ok := ixPDQFromBytes(ha.Bytes())
	if !ok || b != ha {
		t.Fatal("hash bytes round trip")
	}
}

func TestIxPictureIndexGroupsCopiesAndMakesThumbnails(t *testing.T) {
	ixVdupBackground.Store(false)
	defer ixVdupBackground.Store(true)
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	scene := ixTestScene(7, 1200, 900)
	pictures := map[string][]byte{
		"a.jpg": ixTestJPEG(t, scene, 92),
		"b.jpg": ixTestJPEG(t, ixTestResize(scene, 600, 450), 60), // the same, smaller and recompressed
		"c.jpg": ixTestJPEG(t, ixTestScene(8, 1200, 900), 92),     // another picture
	}
	if err := os.MkdirAll(filepath.Join(a.downloadsDir, "pics"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		if err := os.WriteFile(filepath.Join(a.downloadsDir, "pics", name), pictures[name], 0o644); err != nil {
			t.Fatal(err)
		}
		hash := any(nil)
		if name != "b.jpg" { // b has no checksum yet: the index adds it
			sum := sha256.Sum256(pictures[name])
			hash = hex.EncodeToString(sum[:])
		}
		if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,status,file_hash) VALUES('-1001',?,?,?,?,'photo',?,'completed',?)`,
			"", i+1, name, len(pictures[name]), "pics/"+name, hash); err != nil {
			t.Fatal(err)
		}
	}
	for n := 0; n < 10; n++ {
		worked, err := a.ixMediaStep(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	var bHash string
	if err := a.db.Reader.QueryRow(`SELECT file_hash FROM downloads WHERE file_path='pics/b.jpg'`).Scan(&bHash); err != nil || len(bHash) != 64 {
		t.Fatalf("checksum not filled in: %q %v", bHash, err)
	}
	clusters := map[string]string{}
	rows, _ := a.db.Reader.Query(`SELECT p.file_path, m.cluster, m.how FROM ix_mcl m JOIN ix_pdq p ON p.key=m.key`)
	for rows.Next() {
		var path, cluster, how string
		rows.Scan(&path, &cluster, &how)
		clusters[path] = cluster + " " + how
	}
	rows.Close()
	if len(clusters) != 3 || strings.Fields(clusters["pics/a.jpg"])[0] != strings.Fields(clusters["pics/b.jpg"])[0] || strings.Fields(clusters["pics/c.jpg"])[1] != "leader" {
		t.Fatalf("clusters: %v", clusters)
	}
	sum := sha256.Sum256(pictures["a.jpg"])
	if _, err := os.Stat(filepath.Join(a.ixThumbDir(), filepath.FromSlash(ixThumbName("h:"+hex.EncodeToString(sum[:]))))); err != nil {
		t.Fatalf("thumbnail: %v", err)
	}
	if st := a.ixMediaCountNow(ctx); st.Done != 3 || st.Total != 3 || st.Grouped != 1 {
		t.Fatalf("status: %+v", st)
	}
	// The browser folds the two copies into one tile, the larger shown.
	m, err := a.ixRebuildModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Clusters) != 2 {
		t.Fatalf("browser clusters: %d", len(m.Clusters))
	}
	for _, c := range m.Clusters {
		if len(c.Items) == 2 && m.Items[c.Canon].Path != "pics/a.jpg" {
			t.Fatalf("the copy shown is %s", m.Items[c.Canon].Path)
		}
	}
}

func (a *App) ixMediaCountNow(ctx context.Context) ixMediaStatus {
	a.ixMediaCount(ctx)
	return a.ixMediaStatus()
}

// ---------- things in text ----------

func TestIxEntitiesAreCheckedBeforeTheyCount(t *testing.T) {
	text := `Send to 1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2 (or 1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN3, a typo).
SegWit: bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq, ETH 0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed and 0x5aaeb6053F3E94C9b9A09f33669435E7Ef1BeAed.
IBAN GB82 WEST 1234 5698 7654 32 but not GB82 WEST 1234 5698 7654 33. Join https://t.me/SomeChannel or t.me/+AbCdEf123456, ping @Some_User, mail x.y@example.org, call +1 (613) 555-0142.`
	got := map[string]bool{}
	for _, e := range ixExtract(text) {
		got[e[0]+" "+e[1]] = true
	}
	for _, want := range []string{
		"btc 1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2", "btc bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq", "eth 0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed",
		"iban GB82WEST12345698765432", "telegram_link t.me/somechannel", "telegram_link t.me/+AbCdEf123456", "mention @some_user", "email x.y@example.org", "phone +16135550142",
	} {
		if !got[want] {
			t.Errorf("missing %s (got %v)", want, got)
		}
	}
	for k := range got {
		if strings.HasSuffix(k, "NVN3") || strings.HasSuffix(k, "7654 33") || strings.Contains(k, "GB82WEST12345698765433") {
			t.Errorf("a value failing its checksum was kept: %s", k)
		}
	}
	// The mixed-case address with a wrong capital fails EIP-55: only one ETH.
	n := 0
	for k := range got {
		if strings.HasPrefix(k, "eth ") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("eth addresses: %d", n)
	}
}

// ---------- exact re-uploads ----------

type ixFakeRemote struct {
	data     []byte
	noHashes bool
	parts    int
}

func (f *ixFakeRemote) FileHashes(_ context.Context, offset int64) ([]tg.FileHash, error) {
	if f.noHashes {
		return nil, errors.New("VOLUME_LOC_NOT_FOUND")
	}
	const part = 131072
	start := offset / part * part
	var out []tg.FileHash
	for o := start; o < int64(len(f.data)) && len(out) < 4; o += part {
		end := min(o+part, int64(len(f.data)))
		sum := sha256.Sum256(f.data[o:end])
		out = append(out, tg.FileHash{Offset: o, Limit: part, Hash: sum[:]})
	}
	return out, nil
}

func (f *ixFakeRemote) ReadPart(_ context.Context, offset int64) ([]byte, error) {
	f.parts++
	end := min(offset+telegram.PartSize, int64(len(f.data)))
	return append([]byte(nil), f.data[offset:end]...), nil
}

func TestIxSameBytesUsesTelegramHashesThenSamples(t *testing.T) {
	data := make([]byte, 5*1024*1024+333)
	rand.New(rand.NewSource(3)).Read(data)
	path := filepath.Join(t.TempDir(), "saved.mp4")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	defer f.Close()
	size := int64(len(data))
	if how, ok := ixSameBytes(context.Background(), &ixFakeRemote{data: data}, f, size); !ok || how != "telegram-hashes" {
		t.Fatalf("same bytes, hashes: %q %v", how, ok)
	}
	remote := &ixFakeRemote{data: data, noHashes: true}
	if how, ok := ixSameBytes(context.Background(), remote, f, size); !ok || how != "sampled-parts" || remote.parts != 3 {
		t.Fatalf("same bytes, samples: %q %v %d parts", how, ok, remote.parts)
	}
	other := append([]byte(nil), data...)
	other[size-10] ^= 0xff // differs near the end
	if _, ok := ixSameBytes(context.Background(), &ixFakeRemote{data: other}, f, size); ok {
		t.Fatal("different bytes matched by hashes")
	}
	if _, ok := ixSameBytes(context.Background(), &ixFakeRemote{data: other, noHashes: true}, f, size); ok {
		t.Fatal("different bytes matched by samples")
	}
	if ixInflateStripped([]byte{1, 40, 30, 0xAA}) == nil || ixInflateStripped([]byte{2, 1}) != nil {
		t.Fatal("stripped previews")
	}
}

// ---------- held-back copies ----------

func TestIxHeldCopiesAreReleasedWhenTheQueueEmpties(t *testing.T) {
	ixVdupBackground.Store(false)
	defer ixVdupBackground.Store(true)
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	now := time.Now().UnixMilli()
	for i, paused := range []int{1, 0} {
		if _, err := a.db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,paused,created_at,updated_at)
			VALUES('acc','-1001','',?,1,'document:1:1','video','v.mp4',9000000,X'00','pending',?,?,?)`, 10+i, paused, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_held(work_id,group_id,message_id,like_path,like_key,reason,held_at) VALUES(1,'-1001',10,'x.mp4','h:x','looks like a saved copy',?)`, now); err != nil {
		t.Fatal(err)
	}
	// The saved file it copies (a copy whose saved file is gone is let go).
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_type,file_path,status) VALUES('-1001',3,'x.mp4','video','x.mp4','completed')`); err != nil {
		t.Fatal(err)
	}
	if err := a.ixHeldStep(ctx); err != nil {
		t.Fatal(err)
	}
	var paused int
	_ = a.db.Reader.QueryRow(`SELECT paused FROM tgdl_work WHERE id=1`).Scan(&paused)
	if paused != 1 {
		t.Fatal("released while other files were waiting")
	}
	if st := a.ixHeldStatus(ctx); st.Held != 1 {
		t.Fatalf("status: %+v", st)
	}
	_, _ = a.db.Writer.Exec(`UPDATE tgdl_work SET status='done' WHERE id=2`)
	if err := a.ixHeldStep(ctx); err != nil {
		t.Fatal(err)
	}
	_ = a.db.Reader.QueryRow(`SELECT paused FROM tgdl_work WHERE id=1`).Scan(&paused)
	if paused != 0 {
		t.Fatal("not released once the queue was empty")
	}
	// Without Telegram's preview, a video is never held.
	if _, _, ok := a.ixLikelyCopy(ctx, telegram.MediaFacts{DurationMs: 21000, Width: 1280, Height: 720}, 9000000); ok {
		t.Fatal("held without a preview to compare")
	}
}

// ---------- gaps in message IDs ----------

type ixFakeByID struct {
	have  map[int]bool
	asked int
}

func (f *ixFakeByID) MessagesByID(_ telegram.Dialog, ids []int) (telegram.ByIDPage, error) {
	f.asked += len(ids)
	page := telegram.ByIDPage{Entities: &tg.Updates{}}
	for _, id := range ids {
		if f.have[id] {
			page.Messages = append(page.Messages, &tg.Message{ID: id, Date: 1773489600 + id, Message: "recovered", PeerID: &tg.PeerChannel{ChannelID: 42}})
		} else {
			page.Gone = append(page.Gone, id)
		}
	}
	return page, nil
}

func (f *ixFakeByID) Err() error { return nil }

func TestIxGapsAreFilledOrConfirmedGone(t *testing.T) {
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	saved := ixGapPause
	ixGapPause = 0
	defer func() { ixGapPause = saved }()
	group := "-1000000000042"
	for _, id := range []int{1, 2, 5, 9, 250} {
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_messages(group_id,message_id,date) VALUES(?,?,1)`, group, id); err != nil {
			t.Fatal(err)
		}
	}
	fake := &ixFakeByID{have: map[int]bool{7: true, 120: true}}
	if err := a.ixFillGaps(ctx, fake, telegram.Dialog{}, group); err != nil {
		t.Fatal(err)
	}
	if fake.asked != 245 {
		t.Fatalf("asked about %d IDs", fake.asked)
	}
	var kept, gone int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_messages WHERE group_id=?`, group).Scan(&kept)
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_absent WHERE group_id=?`, group).Scan(&gone)
	if kept != 7 || gone != 243 {
		t.Fatalf("kept %d, gone %d", kept, gone)
	}
	c := a.ixIDCoverage(ctx)[group]
	if c.MaxID != 250 || c.Kept != 7 || c.Gone != 243 {
		t.Fatalf("coverage: %+v", c)
	}
	fake.asked = 0
	if err := a.ixFillGaps(ctx, fake, telegram.Dialog{}, group); err != nil || fake.asked != 0 {
		t.Fatalf("second pass asked again: %d %v", fake.asked, err)
	}
	if r := a.ixGoneRanges(ctx, group); strings.Join(r, ",") != "3-4,6,8,10-119,121-249" {
		t.Fatalf("ranges: %v", r)
	}
	if got := ixFreezeFromAlerts(); got != "" {
		t.Fatal(got)
	}
}

func ixFreezeFromAlerts() string {
	al := ixFreezeAlerts(ixFreezeInfo{FreezeState: telegram.FreezeState{Until: time.Now().Add(48 * time.Hour).Unix(), AppealURL: "https://t.me/spambot"}}, time.Now())
	if len(al) != 1 || !strings.Contains(al[0].Message, "Do not log the account out") || !strings.Contains(al[0].Message, "1 day") && !strings.Contains(al[0].Message, "2 day") {
		return "freeze alert: " + al[0].Message
	}
	if len(ixFreezeAlerts(ixFreezeInfo{}, time.Now())) != 0 {
		return "an alert without a deletion date"
	}
	return ""
}

// ---------- integrity and moves ----------

func TestIxFixityAndPlannedMovesWithUndo(t *testing.T) {
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	write := func(rel string, body []byte) string {
		p := filepath.Join(a.downloadsDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		return hex.EncodeToString(sum[:])
	}
	same := []byte("the same video bytes")
	h1 := write("g1/v.mp4", same)
	write("g2/v-copy.mp4", same)
	h3 := write("g3/other.mp4", []byte("other"))
	for i, row := range [][3]string{{"g1/v.mp4", h1, "-1001"}, {"g2/v-copy.mp4", h1, "-1002"}, {"g3/other.mp4", h3, "-1003"}, {"g4/gone.mp4", h3, "-1004"}} {
		if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,status,file_hash,telegram_media_size) VALUES(?,'',?,'v',?,'video',?,'completed',?,?)`,
			row[2], i+1, len(same), row[0], row[1], len(same)); err != nil {
			t.Fatal(err)
		}
	}
	// Integrity: one file changed on disk, one missing.
	write("g3/other.mp4", []byte("tampered"))
	if n, err := a.ixFixityStep(ctx, 10); err != nil || n != 4 {
		t.Fatalf("checked %d: %v", n, err)
	}
	st := a.ixFixityStatus(ctx)
	if st.Changed != 1 || st.Missing != 1 || st.Checked != 4 {
		t.Fatalf("fixity: %+v", st)
	}
	if al := a.ixFixityProblems(ctx, time.Now()); len(al) != 1 {
		t.Fatalf("alerts: %v", al)
	}
	// Moves: the extra identical copy goes to _trash and its entry points at
	// the kept copy; undo puts both back.
	ops, err := a.ixPlanDuplicateCopies(ctx)
	if err != nil || len(ops) != 1 || ops[0].Src != "g2/v-copy.mp4" || ops[0].Repoint != "g1/v.mp4" {
		t.Fatalf("plan: %+v %v", ops, err)
	}
	id, err := a.ixSavePlan(ctx, "duplicate-copies", "test", ops)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ixApplyPlan(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.downloadsDir, "g2", "v-copy.mp4")); !os.IsNotExist(err) {
		t.Fatal("the copy is still in place")
	}
	if _, err := os.Stat(a.ixAbs(ops[0].Dst)); err != nil {
		t.Fatalf("not in _trash: %v", err)
	}
	var path string
	_ = a.db.Reader.QueryRow(`SELECT file_path FROM downloads WHERE group_id='-1002'`).Scan(&path)
	if path != "g1/v.mp4" {
		t.Fatalf("entry points at %q", path)
	}
	if err := a.ixUndoPlan(ctx, id); err != nil {
		t.Fatal(err)
	}
	_ = a.db.Reader.QueryRow(`SELECT file_path FROM downloads WHERE group_id='-1002'`).Scan(&path)
	if _, err := os.Stat(filepath.Join(a.downloadsDir, "g2", "v-copy.mp4")); err != nil || path != "g2/v-copy.mp4" {
		t.Fatalf("undo: %v %q", err, path)
	}
	p, _ := a.ixPlan(ctx, id, 10)
	if p.State != "undone" {
		t.Fatalf("plan state %s", p.State)
	}
	// A copy refuses a source whose checksum no longer matches.
	if err := ixCopyVerified(a.ixAbs("g3/other.mp4"), filepath.Join(t.TempDir(), "x"), h3); err == nil {
		t.Fatal("copied a changed file")
	}
}

// ---------- the browser's families and suggestions ----------

func TestIxFamiliesMergeOverlappingAlbumsAndSuggestCategories(t *testing.T) {
	m := &ixModel{GroupName: map[string]string{}, GroupCat: map[string]string{"A": "Sports", "B": "Sports", "C": ""}}
	addPost := func(group string, msg int, date int64, clusters ...int32) {
		pi := int32(len(m.Posts))
		p := ixMPost{Idx: pi, Group: group, Msg: msg, Date: date, Origin: date, Family: -1}
		for _, c := range clusters {
			for int(c) >= len(m.Clusters) {
				m.Clusters = append(m.Clusters, ixMCluster{Canon: -1})
			}
			ii := int32(len(m.Items))
			m.Items = append(m.Items, ixMItem{Group: group, Msg: msg, Key: "h:" + string(rune('a'+c)), Cluster: c, Post: pi, Type: "photo", Status: "ok"})
			m.Clusters[c].Items = append(m.Clusters[c].Items, ii)
			if m.Clusters[c].Canon < 0 {
				m.Clusters[c].Canon = ii
			}
			p.Items = append(p.Items, ii)
		}
		m.Posts = append(m.Posts, p)
	}
	addPost("A", 1, 100, 0, 1, 2, 3)    // an album
	addPost("B", 1, 200, 0, 1, 2, 3)    // reposted whole
	addPost("C", 1, 300, 1, 2, 3, 4, 5) // reposted with two new files
	addPost("C", 2, 400, 6)             // something new
	addPost("C", 3, 500, 0, 6, 7, 8)    // shares one file with each: a new post
	ixAssignFamilies(m)
	roles := []string{}
	for _, p := range m.Posts {
		roles = append(roles, p.Role)
	}
	if strings.Join(roles, ",") != "original,repost,adds,original,original" {
		t.Fatalf("roles: %v", roles)
	}
	f := m.Families[m.Posts[0].Family]
	if len(f.Posts) != 3 || len(f.Items) != 6 || !f.Merged || f.Chats != 3 {
		t.Fatalf("family: %+v", f)
	}
	// Category from content: C shares 4 of its 9 files with Sports chats.
	for i := 0; i < 3; i++ { // enough shared files to count
		addPost("A", 10+i, 600, int32(9+i))
		addPost("C", 10+i, 700, int32(9+i))
	}
	sg := ixContentSuggestions(m)
	if sg["C"].Category != "Sports" || sg["C"].Shared < 5 {
		t.Fatalf("suggestions: %+v", sg)
	}
}

// ---------- diagnostics ----------

func TestIxDiagnosticsConcealChatsFilesAndPeople(t *testing.T) {
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	cfg, _ := a.config.Load(ctx)
	cfg["groups"] = []any{map[string]any{"id": "-1001811239876", "name": "Secret Riders Club", "enabled": true}}
	if err := a.config.Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_peers(peer_id,kind,name,username,updated_at) VALUES('user:77','user','Jane Q Example','janeq',1)`); err != nil {
		t.Fatal(err)
	}
	a.ixLog("picture index: open D:\\Telegram\\Secret Riders Club\\images\\holiday-2026.jpg: access denied for -1001811239876")
	a.ixLog("comments on -1001811239876:105 from Jane Q Example via https://t.me/secretriders and @janeq_backup, file /volume1/Telegram/Secret Riders Club/videos/clip 1.mp4, hash 4a3b2c1d4a3b2c1d4a3b2c1d4a3b2c1d")
	out := a.ixDiagnostics(ctx)
	for _, secret := range []string{"Secret Riders", "1811239876", "holiday-2026", "Jane Q", "secretriders", "janeq", "volume1", "clip 1.mp4", "4a3b2c1d4a3b2c1d"} {
		if strings.Contains(out, secret) {
			t.Errorf("the report leaks %q:\n%s", secret, out)
		}
	}
	for _, kept := range []string{"G1", "access denied", "picture index", "[settings]", "<path>", "<link>", "<user>"} {
		if !strings.Contains(out, kept) {
			t.Errorf("the report lacks %q:\n%s", kept, out)
		}
	}
}

func TestIxPlansNeverTrashTheOnlyCopy(t *testing.T) {
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	body := []byte("only copy")
	sum := sha256.Sum256(body)
	h := hex.EncodeToString(sum[:])
	if err := os.MkdirAll(filepath.Join(a.downloadsDir, "g1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.downloadsDir, "g1", "v.mp4"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	// One file, catalogued under two spellings of its path.
	for i, p := range []string{"g1/v.mp4", "./g1/v.mp4", `g1\v.mp4`} {
		if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,status,file_hash) VALUES(?,'',1,'v',9,'video',?,'completed',?)`,
			"-100"+string(rune('1'+i)), p, h); err != nil {
			t.Fatal(err)
		}
	}
	ops, err := a.ixPlanDuplicateCopies(ctx)
	if err != nil || len(ops) != 0 {
		t.Fatalf("planned to move the only copy: %+v %v", ops, err)
	}
	// Even a plan written by hand cannot trash it.
	id, _ := a.ixSavePlan(ctx, "duplicate-copies", "bad", []ixMoveOp{{Op: "trash", Src: "g1/v.mp4", Dst: "_trash/x/g1/v.mp4", SHA256: h, Size: 9, Repoint: "./g1/v.mp4"}})
	if err := a.ixApplyPlan(ctx, id); err == nil {
		t.Fatal("trashed the only copy")
	}
	if _, err := os.Stat(filepath.Join(a.downloadsDir, "g1", "v.mp4")); err != nil {
		t.Fatalf("the file is gone: %v", err)
	}
	// A move whose catalogue entry cannot follow is put back.
	if err := os.WriteFile(filepath.Join(a.downloadsDir, "stray.mp4"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	id, _ = a.ixSavePlan(ctx, "collection", "stray", []ixMoveOp{{Op: "move", Src: "stray.mp4", Dst: "moved/stray.mp4", SHA256: h, Size: 9, Repoint: "moved/stray.mp4"}})
	if err := a.ixApplyPlan(ctx, id); err == nil {
		t.Fatal("moved a file no entry points at")
	}
	if _, err := os.Stat(filepath.Join(a.downloadsDir, "stray.mp4")); err != nil {
		t.Fatalf("not moved back: %v", err)
	}
	// An interrupted move (file moved, journal not updated) is finished on retry.
	if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,status,file_hash) VALUES('-1009','',1,'v',9,'video','stray.mp4','completed',?)`, h); err != nil {
		t.Fatal(err)
	}
	id, _ = a.ixSavePlan(ctx, "collection", "half", []ixMoveOp{{Op: "move", Src: "stray.mp4", Dst: "moved/stray.mp4", SHA256: h, Size: 9, Repoint: "moved/stray.mp4"}})
	if err := ixMoveVerified(a.ixAbs("stray.mp4"), a.ixAbs("moved/stray.mp4"), h); err != nil {
		t.Fatal(err)
	}
	_, _ = a.db.Writer.Exec(`UPDATE ix_moves SET state='running' WHERE plan_id=?`, id)
	_, _ = a.db.Writer.Exec(`UPDATE ix_move_plans SET state='failed' WHERE id=?`, id)
	if err := a.ixApplyPlan(ctx, id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	var path string
	_ = a.db.Reader.QueryRow(`SELECT file_path FROM downloads WHERE group_id='-1009'`).Scan(&path)
	if path != "moved/stray.mp4" {
		t.Fatalf("entry at %q", path)
	}
	if err := a.ixUndoPlan(ctx, id); err != nil {
		t.Fatal(err)
	}
	_ = a.db.Reader.QueryRow(`SELECT file_path FROM downloads WHERE group_id='-1009'`).Scan(&path)
	if _, err := os.Stat(a.ixAbs("stray.mp4")); err != nil || path != "stray.mp4" {
		t.Fatalf("undo: %v %q", err, path)
	}
}
