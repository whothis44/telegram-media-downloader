package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/download"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// Invented data only. Fixes after the ix.10.2 review: a wrong "same bytes"
// is permanent loss, fetching re-uploads must really fetch them, the
// same-size wait must not hold unrelated files, and a file stays while a
// message still shares it.

// ixLookAlike is random bytes of size with the same header and the same
// silent stretches at the start, middle and end (an uncompressed WAV or a
// scan with white margins): equal where the old check looked, different
// elsewhere.
func ixLookAlike(seed int64, size int) []byte {
	b := ixRandom(seed, size)
	copy(b[:44], []byte("RIFF....WAVEfmt invented-header-of-44-bytes"))
	for _, r := range [][2]int{{40, 256 << 10}, {size/2 - 200<<10, size/2 + 200<<10}, {size - 256<<10, size}} {
		for i := r[0]; i < r[1]; i++ {
			b[i] = 0
		}
	}
	return b
}

func TestIxSubstUncompressedLookAlikesAreNotTheSame(t *testing.T) {
	const size = 3 << 20
	remote, local := ixLookAlike(1, size), ixLookAlike(2, size)
	f, err := os.CreateTemp(t.TempDir(), "saved")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(local); err != nil {
		t.Fatal(err)
	}
	for _, it := range []download.Item{
		{Type: "audio", Facts: telegram.MediaFacts{Mime: "audio/wav"}},
		{Type: "photo", Facts: telegram.MediaFacts{Mime: "image/bmp"}},
		{Type: "document", Facts: telegram.MediaFacts{Mime: "image/tiff"}},
		{Type: "document", Facts: telegram.MediaFacts{Mime: "application/octet-stream"}},
		{Type: "gif", Facts: telegram.MediaFacts{Mime: "image/gif"}},
	} {
		it.Identity = telegram.MediaIdentity{Kind: "document", ID: "1", Size: size}
		plan, ok := ixSubstPlanFor(it)
		if !ok || !plan.fullHashes || plan.sample != 0 {
			t.Fatalf("%s: plan %+v %v: only video may be judged on part of it", it.Facts.Mime, plan, ok)
		}
		for _, noHashes := range []bool{false, true} {
			remoteFile := &ixRangeRemote{data: remote, noHashes: noHashes}
			if how, same := newIxRemote(remoteFile, size).same(context.Background(), f, plan); same {
				t.Fatalf("%s noHashes=%v: different files judged the same bytes (%s)", it.Facts.Mime, noHashes, how)
			}
			if remoteFile.parts.Load()+remoteFile.reads.Load() != 0 {
				t.Fatalf("%s: parts were downloaded to compare", it.Facts.Mime)
			}
		}
	}
	// A video from 2 MiB is checked at the start, middle and end, and every
	// hash Telegram returned for those places is compared, not only the part
	// at each place.
	video := download.Item{Type: "video", Identity: telegram.MediaIdentity{Kind: "document", ID: "2", Size: size}, Facts: telegram.MediaFacts{Mime: "video/mp4"}}
	plan, _ := ixSubstPlanFor(video)
	if plan.fullHashes || plan.sample == 0 {
		t.Fatalf("video plan %+v", plan)
	}
	if _, same := newIxRemote(&ixRangeRemote{data: remote}, size).same(context.Background(), f, plan); same {
		t.Fatal("video: hashes past the checked part were ignored")
	}
	same, err := os.CreateTemp(t.TempDir(), "same")
	if err != nil {
		t.Fatal(err)
	}
	defer same.Close()
	if _, err := same.Write(remote); err != nil {
		t.Fatal(err)
	}
	if how, ok := newIxRemote(&ixRangeRemote{data: remote}, size).same(context.Background(), same, plan); !ok || how != "telegram-hashes" {
		t.Fatalf("video, same bytes: %q %v", how, ok)
	}
}

// ixLinkChat configures an invented chat for the link tests.
func ixLinkChat(t *testing.T, a *App, ctx context.Context, channel int64, skip bool) string {
	t.Helper()
	group := strconv.FormatInt(-1000000000000-channel, 10)
	cfg, err := a.config.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg["groups"] = []any{map[string]any{"id": group, "name": "Invented", "enabled": true, "filters": map[string]any{"photos": true, "videos": true, "files": true}}}
	cfg["download"] = map[string]any{"skipDuplicates": skip}
	if err = a.config.Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	return group
}

// ixLinkIngest ingests an invented video message of doc in channel.
func ixLinkIngest(t *testing.T, a *App, ctx context.Context, tr telegram.MediaDownloader, channel int64, group string, msg int, doc int64, size int64) download.Record {
	t.Helper()
	m := &tg.Message{ID: msg, Date: 1773489600 + msg, PeerID: &tg.PeerChannel{ChannelID: channel},
		Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: doc, AccessHash: 1, Size: size, DCID: 2, MimeType: "video/mp4",
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: fmt.Sprintf("c%d.mp4", msg)}, &tg.DocumentAttributeVideo{W: 640, H: 360, Duration: 4}}}}}
	var buf bin.Buffer
	if err := m.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	var d tg.Message
	if err := d.Decode(&buf); err != nil {
		t.Fatal(err)
	}
	rec, err := a.ingestTelegram(ctx, &d, group, "Invented", "acc", tr)
	if err != nil {
		t.Fatalf("message %d: %v", msg, err)
	}
	return rec
}

// "Fetch the real files of re-uploads" makes them download, even when a
// forward of a re-upload was matched to the saved file meanwhile (by its
// link, or by its own entry through the library's identity match).
func TestIxFetchSubstitutedDownloadsEvenAfterAForward(t *testing.T) {
	for _, skip := range []bool{true, false} {
		t.Run(fmt.Sprintf("skipDuplicates=%v", skip), func(t *testing.T) {
			a, ctx := ixTestApp(t)
			const channel = 9801
			group := ixLinkChat(t, a, ctx, channel, skip)
			clip := ixRandom(12, 300<<10)
			size := int64(len(clip))
			tr := &ixLinkTransport{files: map[string][]byte{"8001": clip, "8002": clip}}
			if r := ixLinkIngest(t, a, ctx, tr, channel, group, 1, 8001, size); r.Dedup != "" {
				t.Fatalf("original: %q", r.Dedup)
			}
			if r := ixLinkIngest(t, a, ctx, tr, channel, group, 2, 8002, size); r.Dedup != download.DedupReupload {
				t.Fatalf("re-upload: %q", r.Dedup)
			}
			if r := ixLinkIngest(t, a, ctx, tr, channel, group, 3, 8002, size); r.Dedup != download.DedupTelegramFile {
				t.Fatalf("forward: %q", r.Dedup)
			}
			n, _, err := a.ixFetchSubstituted(ctx)
			if err != nil || n != 1 {
				t.Fatalf("fetch substituted: %d %v", n, err)
			}
			// The forward lost its match too: it is queued again by the next pass.
			var left int
			_ = a.db.Reader.QueryRow(`SELECT (SELECT COUNT(*) FROM downloads WHERE message_id IN (2,3)) + (SELECT COUNT(*) FROM ix_msg_link WHERE message_id IN (2,3))`).Scan(&left)
			if left != 0 {
				t.Fatalf("messages 2 and 3 still matched to the saved file: %d rows", left)
			}
			before := tr.downloads.Load()
			r := ixLinkIngest(t, a, ctx, tr, channel, group, 2, 8002, size)
			if tr.downloads.Load() != before+1 || r.Dedup != download.DedupAfterDownload {
				t.Fatalf("after fetching, message 2 was not downloaded: %+v downloads %d->%d", r, before, tr.downloads.Load())
			}
			// Its bytes came: the forward is matched by identity again, without a download.
			if r := ixLinkIngest(t, a, ctx, tr, channel, group, 3, 8002, size); r.Dedup != download.DedupTelegramFile || tr.downloads.Load() != before+1 {
				t.Fatalf("forward after the real download: %+v downloads %d", r, tr.downloads.Load())
			}
			files, _ := filepath.Glob(filepath.Join(a.downloadsDir, "*", "*", "*"))
			if len(files) != 1 {
				t.Fatalf("files on disk: %v", files)
			}
		})
	}
}

// The same-size wait is only for copies the check can confirm: videos and
// photos of the same kind and MIME type, not numbered parts of a split
// archive, and not with the check off or a staging folder.
func TestIxSameSizeWaitOnlyForConfirmableCopies(t *testing.T) {
	a, ctx := ixTestApp(t)
	const size = 2000 << 20
	it := func(name, kind, mime string) download.Item {
		return download.Item{Name: name, Identity: telegram.MediaIdentity{Kind: kind, ID: "1", Size: size}, Facts: telegram.MediaFacts{Mime: mime}}
	}
	mp4, ok := a.ixSubstituteWait(ctx, it("trip.mp4", "document", "video/mp4"))
	if !ok {
		t.Fatal("a video does not wait")
	}
	if mkv, ok := a.ixSubstituteWait(ctx, it("trip.mkv", "document", "video/x-matroska")); !ok || mkv == mp4 {
		t.Fatalf("videos of another MIME type share the wait: %q %q", mkv, mp4)
	}
	if _, ok := a.ixSubstituteWait(ctx, it("scan.jpg", "photo", "image/jpeg")); !ok {
		t.Fatal("a photo does not wait")
	}
	for _, x := range []download.Item{
		it("backup.part01.rar", "document", "application/vnd.rar"),
		it("backup.7z.001", "document", "application/x-7z-compressed"),
		it("film.mp4.002", "document", "video/mp4"),
		it("film.part2.mp4", "document", "video/mp4"),
		it("dump.r00", "document", "application/octet-stream"),
		it("archive.zip", "document", "application/zip"),
		it("talk.wav", "document", "audio/wav"),
		it("scan.tiff", "document", "image/tiff"),
	} {
		if key, ok := a.ixSubstituteWait(ctx, x); ok {
			t.Fatalf("%s waits (%q)", x.Name, key)
		}
	}
	small := it("clip.mp4", "document", "video/mp4")
	small.Identity.Size = 100 << 10
	if _, ok := a.ixSubstituteWait(ctx, small); ok {
		t.Fatal("a file below the check's minimum waits")
	}
	s := a.ixLoadSettings(ctx)
	s.StageDir = t.TempDir()
	if err := a.ixSaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.ixSubstituteWait(ctx, it("trip.mp4", "document", "video/mp4")); ok {
		t.Fatal("waits with a staging folder, where the check cannot see the first file")
	}
	s.StageDir, s.SubstExact = "", false
	if err := a.ixSaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.ixSubstituteWait(ctx, it("trip.mp4", "document", "video/mp4")); ok {
		t.Fatal("waits with the check off")
	}
}

// Deleting, purging or replacing the original keeps the file while a
// message without an entry of its own still shares it, and that message
// still shows it.
func TestIxOriginalDeletedKeepsTheLinkedCopysFile(t *testing.T) {
	a, ctx := ixTestApp(t)
	const channel = 9802
	group := ixLinkChat(t, a, ctx, channel, true)
	clip := ixRandom(13, 300<<10)
	size := int64(len(clip))
	tr := &ixLinkTransport{files: map[string][]byte{"8101": clip}}
	orig := ixLinkIngest(t, a, ctx, tr, channel, group, 1, 8101, size)
	if fwd := ixLinkIngest(t, a, ctx, tr, channel, group, 2, 8101, size); !fwd.Skipped || fwd.Dedup != download.DedupTelegramFile {
		t.Fatalf("forward: %+v", fwd)
	}
	full := filepath.Join(a.downloadsDir, filepath.FromSlash(orig.Path))
	// The original's entry goes and its file is set for deletion, as the
	// gallery's delete, a purge or an edit replacing its file do.
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO tgdl_file_cleanup(path) SELECT file_path FROM downloads WHERE id=?`, orig.ID); err == nil {
		_, err = tx.Exec(`DELETE FROM downloads WHERE id=?`, orig.ID)
	}
	if err != nil || tx.Commit() != nil {
		t.Fatal(err)
	}
	if err := a.drainFileCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(full); err != nil {
		t.Fatalf("the shared file was deleted with the original: %v", err)
	}
	m, err := a.ixRebuildModel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pi, ok := m.ByMsg[group+":2"]
	if !ok {
		t.Fatal("the copy's post is gone")
	}
	found := false
	for _, ii := range m.Posts[pi].Items {
		if it := m.Items[ii]; it.Msg == 2 {
			found = it.Status == "ok" && it.Key == "h:"+orig.SHA256 && it.Path == orig.Path
		}
	}
	if !found {
		t.Fatal("the copy no longer shows its file")
	}
	// A purge of the copy's chat removes its link; nothing else keeps the
	// file, so it goes.
	p := purgeRecord{Key: "group:" + group, GroupID: group, FilesOnly: true, Phase: "queued", Status: dedupIdleStatus("groupPurge:" + group)}
	p.Status["running"], p.Status["startedAt"] = true, time.Now().UnixMilli()
	if err := a.runPurge(ctx, p, false); err != nil {
		t.Fatal(err)
	}
	var links int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM ix_msg_link`).Scan(&links)
	if links != 0 {
		t.Fatalf("links after purge: %d", links)
	}
	if _, err := os.Stat(full); !os.IsNotExist(err) {
		t.Fatalf("the file outlived every message that used it: %v", err)
	}
}

// A copy found after downloading into the staging folder is counted once,
// when it is found (the staged arrival only moves links).
func TestIxStagedCopyFoundAfterDownloadIsCounted(t *testing.T) {
	stage := t.TempDir()
	a, ctx := ix8App(t, map[string]any{"stageDir": stage})
	const channel = 9803
	group := ixLinkChat(t, a, ctx, channel, false)
	clip := ixRandom(14, 300<<10)
	size := int64(len(clip))
	tr := &ixLinkTransport{files: map[string][]byte{"8201": clip, "8202": clip}}
	if r := ixLinkIngest(t, a, ctx, tr, channel, group, 1, 8201, size); !r.Staged {
		t.Fatalf("not staged: %+v", r)
	}
	a.library.MoveStaged(ctx)
	if _, err := a.db.Writer.Exec(`INSERT INTO ix_subst_block(identity) VALUES(?)`, telegram.MediaIdentity{Kind: "document", ID: "8202", Size: size}.Key()); err != nil {
		t.Fatal(err)
	}
	r := ixLinkIngest(t, a, ctx, tr, channel, group, 2, 8202, size)
	if r.Dedup != download.DedupAfterDownload || tr.downloads.Load() != 2 {
		t.Fatalf("second: %+v downloads %d", r, tr.downloads.Load())
	}
	a.library.MoveStaged(ctx)
	if st := a.ixDedupStatus(ctx); st.After.Files != 1 || st.After.Bytes != size {
		t.Fatalf("after-download copies counted: %+v", st.After)
	}
	left, _ := filepath.Glob(filepath.Join(stage, download.StageFolder, "*", "*", "*"))
	if len(left) != 0 {
		t.Fatalf("staged copy kept: %v", left)
	}
}
