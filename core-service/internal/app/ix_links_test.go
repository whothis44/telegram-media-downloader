package app

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// ixLinkTransport is an invented account: its files' bytes, part hashes and
// ranges, and how many files it downloaded.
type ixLinkTransport struct {
	files     map[string][]byte
	downloads atomic.Int64
}

func (x *ixLinkTransport) DownloadMedia(_ context.Context, att telegram.Attachment, w io.Writer) error {
	x.downloads.Add(1)
	_, err := w.Write(x.files[att.Identity.ID])
	return err
}

func (x *ixLinkTransport) MediaFileHashes(_ context.Context, att telegram.Attachment, offset int64) ([]tg.FileHash, error) {
	data, ok := x.files[att.Identity.ID]
	if !ok {
		return nil, errors.New("FILE_ID_INVALID")
	}
	return ixFakeHashes(data, offset), nil
}

func (x *ixLinkTransport) MediaRange(_ context.Context, att telegram.Attachment, offset int64, limit int) ([]byte, error) {
	data := x.files[att.Identity.ID]
	return append([]byte(nil), data[offset:min(offset+int64(limit), int64(len(data)))]...), nil
}

// Every message stays tied to the saved file it shares, whichever way the
// copy was found and whether or not it got a catalogue entry of its own
// (download.skipDuplicates): in the media browser, its post and album, and
// the data export, with the saved file's SHA-256. Only the duplicate bytes
// go.
func TestIxCopiesStayLinkedToTheSavedFile(t *testing.T) {
	for _, skip := range []bool{false, true} {
		t.Run(fmt.Sprintf("skipDuplicates=%v", skip), func(t *testing.T) { ixCheckLinkedCopies(t, skip) })
	}
}

func ixCheckLinkedCopies(t *testing.T, skip bool) {
	a, ctx := ixTestApp(t)
	const channel = 9800
	group := strconv.FormatInt(-1000000000000-channel, 10)
	cfg, err := a.config.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg["groups"] = []any{map[string]any{"id": group, "name": "Invented Linked Chat", "enabled": true, "filters": map[string]any{"photos": true, "videos": true, "files": true}}}
	cfg["download"] = map[string]any{"skipDuplicates": skip}
	if err = a.config.Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	clip := ixRandom(11, 300<<10) // an invented clip
	sum := sha256.Sum256(clip)
	want := hex.EncodeToString(sum[:])
	size := int64(len(clip))
	tr := &ixLinkTransport{files: map[string][]byte{"7001": clip, "7002": clip, "7003": clip}}
	// The last upload is set to be downloaded for real, so it is found to
	// be a copy only after downloading.
	if _, err = a.db.Writer.Exec(`INSERT INTO ix_subst_block(identity) VALUES(?)`, telegram.MediaIdentity{Kind: "document", ID: "7003", Size: size}.Key()); err != nil {
		t.Fatal(err)
	}
	how := map[int]string{1: "", 2: "telegram-file", 3: "exact-reupload", 4: "after-download", 5: "telegram-file"}
	for _, x := range []struct {
		msg     int
		doc     int64
		grouped int64
	}{{1, 7001, 555}, {2, 7001, 555}, {3, 7002, 555}, {4, 7003, 555}, {5, 7003, 0}} {
		m := &tg.Message{ID: x.msg, Date: 1773489600 + x.msg, PeerID: &tg.PeerChannel{ChannelID: channel}, GroupedID: x.grouped,
			Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: x.doc, AccessHash: 1, Size: size, DCID: 2, MimeType: "video/mp4",
				Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: fmt.Sprintf("clip_%d.mp4", x.msg)}, &tg.DocumentAttributeVideo{W: 640, H: 360, Duration: 4}}}}}
		if x.grouped != 0 {
			m.SetGroupedID(x.grouped)
		}
		var buf bin.Buffer
		if err := m.Encode(&buf); err != nil {
			t.Fatal(err)
		}
		var decoded tg.Message
		if err := decoded.Decode(&buf); err != nil {
			t.Fatal(err)
		}
		row := ixRowFromMessage(&decoded)
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_messages(group_id,message_id,date,text,grouped_id,sender_id,media_type,media_name,media_size,media_key,tl,first_seen,seen_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,1,1)`, group, row.MessageID, row.Date, row.Text, row.GroupedID, row.SenderID, row.MediaType, row.MediaName, row.MediaSize, row.MediaKey, row.TL); err != nil {
			t.Fatal(err)
		}
		rec, err := a.ingestTelegram(ctx, &decoded, group, "Invented Linked Chat", "acc", tr)
		if err != nil {
			t.Fatalf("message %d: %v", x.msg, err)
		}
		if rec.Dedup != how[x.msg] {
			t.Fatalf("message %d found as %q, want %q (%+v)", x.msg, rec.Dedup, how[x.msg], rec)
		}
		if skip && x.msg > 1 && !rec.Skipped {
			t.Fatalf("message %d: an entry was made with skipDuplicates on", x.msg)
		}
	}
	// Bytes went over the wire for the original and the copy found after
	// downloading only; the forward of that copy was not downloaded either.
	if n := tr.downloads.Load(); n != 2 {
		t.Fatalf("downloads=%d, want 2", n)
	}
	files, _ := filepath.Glob(filepath.Join(a.downloadsDir, "*", "*", "*"))
	if len(files) != 1 {
		t.Fatalf("files on disk: %v", files)
	}
	var own int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM downloads WHERE group_id=?`, group).Scan(&own)
	if (skip && own != 1) || (!skip && own != 5) {
		t.Fatalf("catalogue entries=%d (skipDuplicates=%v)", own, skip)
	}

	// The media browser: every message shows the saved file.
	m, err := a.ixRebuildModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for msg := 1; msg <= 5; msg++ {
		rec := httptest.NewRecorder()
		a.handleIxBrowseItem(rec, httptest.NewRequest("GET", fmt.Sprintf("/api/ix/browse/item?group=%s&msg=%d", group, msg), nil))
		var body struct {
			Item struct {
				Key, Status, Src string
				Msg              int
			}
			Copies []struct{ Msg int }
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("browse item %d: %d %s", msg, rec.Code, rec.Body.String())
		}
		st := body.Item.Status
		if body.Item.Msg != msg || body.Item.Key != "h:"+want || body.Item.Src == "" || (st != "ok" && st != "subst") || len(body.Copies) != 5 {
			t.Fatalf("browse item %d: %s", msg, rec.Body.String())
		}
	}
	// The album holds all four, each with the saved file.
	album := m.ByMsg[group+":1"]
	for msg := 2; msg <= 4; msg++ {
		if m.ByMsg[group+":"+strconv.Itoa(msg)] != album {
			t.Fatalf("message %d is not in its album", msg)
		}
	}
	if items := m.Posts[album].Items; len(items) != 4 {
		t.Fatalf("album items: %d", len(items))
	} else {
		for _, ii := range items {
			if m.Items[ii].Key != "h:"+want {
				t.Fatalf("album item %+v", m.Items[ii])
			}
		}
	}

	// The data export: each message carries the file with its SHA-256, and
	// its post lists it.
	out := t.TempDir()
	count := 0
	if err = a.ixWriteData(ctx, out, filepath.Join(out, "none"), "/volume1/Invented", func(string) {}, &count); err != nil {
		t.Fatal(err)
	}
	gz, _ := filepath.Glob(filepath.Join(out, "data", "messages", "*.jsonl.gz"))
	if len(gz) != 1 {
		t.Fatalf("message files: %v", gz)
	}
	msgs := ixReadGzLines(t, gz[0])
	for msg := 1; msg <= 5; msg++ {
		line := msgs[msg]
		var row struct {
			FileStatus string `json:"file_status"`
			Files      []struct {
				SHA256 string `json:"sha256"`
				Linked string `json:"linked"`
				TgFile string `json:"telegram_file"`
			} `json:"files"`
		}
		if json.Unmarshal(line, &row) != nil || row.FileStatus != "downloaded" || len(row.Files) != 1 || row.Files[0].SHA256 != want {
			t.Fatalf("export message %d: %s", msg, line)
		}
		linkedWant := ""
		if skip && msg > 1 {
			linkedWant = how[msg]
		}
		if row.Files[0].Linked != linkedWant {
			t.Fatalf("export message %d linked %q, want %q", msg, row.Files[0].Linked, linkedWant)
		}
	}
	posts, err := os.ReadFile(filepath.Join(out, "data", "posts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	sc := bufio.NewScanner(bytes.NewReader(posts))
	for sc.Scan() {
		var p struct {
			Album    string `json:"album_id"`
			Messages []int  `json:"message_ids"`
			Files    []struct {
				Msg    int    `json:"message_id"`
				SHA256 string `json:"sha256"`
			} `json:"files"`
		}
		if json.Unmarshal(sc.Bytes(), &p) != nil || p.Album != "555" {
			continue
		}
		found = true
		if len(p.Messages) != 4 || len(p.Files) != 4 {
			t.Fatalf("album post: %s", sc.Bytes())
		}
		for _, f := range p.Files {
			if f.SHA256 != want {
				t.Fatalf("album post file: %s", sc.Bytes())
			}
		}
	}
	if !found {
		t.Fatalf("no album post: %s", posts)
	}

	// Copies found after downloading count as downloaded; forwards and
	// exact re-uploads do not.
	var before int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_msg_link w WHERE ` + ixSkippedBefore).Scan(&before)
	if before != 3 {
		t.Fatalf("skipped before downloading: %d, want 3", before)
	}
	if h := a.ixHeldStatus(ctx); h.Subst != 1 {
		t.Fatalf("held status: %+v", h)
	}
}

func ixReadGzLines(t *testing.T, path string) map[int][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int][]byte{}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var head struct {
			MessageID int `json:"message_id"`
		}
		if json.Unmarshal(sc.Bytes(), &head) == nil {
			out[head.MessageID] = append([]byte(nil), sc.Bytes()...)
		}
	}
	return out
}
