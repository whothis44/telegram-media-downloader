package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/store"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// Invented bytes, seeded so that another process can make the same ones.
func resumeBytes(size int) []byte {
	out := make([]byte, size)
	rand.New(rand.NewSource(99)).Read(out)
	return out
}

const resumeTestSize = 10 << 20 // parts of 1 MiB, a checkpoint every 2 MiB

// resumeClient sends parts of 1 MiB, as Telegram does, and remembers where it was
// asked to start. stopAfter ends the attempt after that many parts.
type resumeClient struct {
	data       []byte
	can        bool
	stopAfter  int
	failWith   error
	starts     []int64
	onPart     func(sent int)
	sentParts  atomic.Int64
	extraBytes int // sends this much past the declared size
	chunk      int // writes in pieces of this size, as a throttled writer does
}

func (c *resumeClient) CanResume() bool { return c.can }

func (c *resumeClient) Download(ctx context.Context, id telegram.MediaIdentity, w io.Writer) error {
	return c.DownloadFrom(ctx, id, 0, w)
}

func (c *resumeClient) DownloadFrom(ctx context.Context, _ telegram.MediaIdentity, offset int64, w io.Writer) error {
	c.starts = append(c.starts, offset)
	if offset%telegram.PartSize != 0 {
		return fmt.Errorf("offset %d is not on a part", offset)
	}
	data := c.data[offset:]
	if c.extraBytes > 0 {
		data = append(append([]byte(nil), data...), make([]byte, c.extraBytes)...)
	}
	sent := 0
	for len(data) > 0 {
		if c.stopAfter > 0 && sent == c.stopAfter {
			if c.failWith != nil {
				return c.failWith
			}
			return errors.New("the connection dropped (invented)")
		}
		n := min(telegram.PartSize, len(data))
		for piece := data[:n]; len(piece) > 0; {
			size := len(piece)
			if c.chunk > 0 {
				size = min(c.chunk, size)
			}
			if _, err := w.Write(piece[:size]); err != nil {
				return err
			}
			piece = piece[size:]
		}
		data = data[n:]
		sent++
		c.sentParts.Add(1)
		if c.onPart != nil {
			c.onPart(sent)
		}
	}
	return ctx.Err()
}

func resumeItem(size int64) Item {
	return Item{GroupID: "-100", GroupName: "Invented", MessageID: 7, Name: "video.mp4", Type: "video",
		Identity: telegram.MediaIdentity{Kind: "video", ID: "42", Size: size}}
}

func withCheckpoints(t *testing.T) {
	t.Helper()
	oldMin, oldEvery := resumeMinSize, partCheckpointEvery
	resumeMinSize, partCheckpointEvery = 4<<20, 2<<20
	t.Cleanup(func() { resumeMinSize, partCheckpointEvery = oldMin, oldEvery })
}

// resumeProgress is the row of the test's file (offset -1: none).
func resumeProgress(t *testing.T, db *store.DB) (int64, string) {
	t.Helper()
	var offset int64
	var path string
	err := db.Reader.QueryRow(`SELECT offset,path FROM tgdl_part_progress`).Scan(&offset, &path)
	if err != nil {
		return -1, ""
	}
	return offset, path
}

func resumePartFile(dir, path string) string {
	rel := filepath.FromSlash(path)
	return filepath.Join(dir, "downloads", filepath.Dir(rel), partName(filepath.Base(rel)))
}

func checkResumed(t *testing.T, dir string, db *store.DB, rec Record, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, "downloads", rec.Path))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the saved file is not the original: %d bytes, want %d", len(got), len(want))
	}
	var stored string
	if err := db.Reader.QueryRow(`SELECT file_hash FROM downloads WHERE file_path=?`, rec.Path).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(want)
	if stored != hex.EncodeToString(sum[:]) {
		t.Fatalf("the catalogue's hash %s is not the whole file's", stored)
	}
	if offset, _ := resumeProgress(t, db); offset != -1 {
		t.Fatalf("progress left behind at %d", offset)
	}
	filepath.WalkDir(filepath.Join(dir, "downloads"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".part") {
			t.Fatalf("a partial file left behind: %s", p)
		}
		return nil
	})
}

// The whole point: the second attempt starts at the checkpoint, fetches nothing
// before it, and the file comes out exact, hash included.
func TestResumeCarriesOnFromTheCheckpoint(t *testing.T) {
	withCheckpoints(t)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	first := &resumeClient{data: data, can: true, stopAfter: 7}
	if _, err := library.Ingest(context.Background(), item, first); err == nil {
		t.Fatal("the attempt should have failed")
	}
	offset, path := resumeProgress(t, db)
	if offset != 6<<20 {
		t.Fatalf("after 7 parts, progress at %d, want the last checkpoint (6 MiB)", offset)
	}
	info, err := os.Stat(resumePartFile(dir, path))
	if err != nil {
		t.Fatalf("the partial file was not kept: %v", err)
	}
	if info.Size() != 7<<20 {
		t.Fatalf("partial file %d bytes", info.Size())
	}
	second := &resumeClient{data: data, can: true}
	rec, err := library.Ingest(context.Background(), item, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.starts) != 1 || second.starts[0] != 6<<20 {
		t.Fatalf("second attempt started at %v, want 6 MiB", second.starts)
	}
	if got := second.sentParts.Load(); got != 4 {
		t.Fatalf("second attempt fetched %d parts, want 4", got)
	}
	checkResumed(t, dir, db, rec, data)
}

// Nothing was kept because nothing was recorded yet: it starts over, cleanly.
func TestResumeBeforeTheFirstCheckpointStartsOver(t *testing.T) {
	withCheckpoints(t)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	if _, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: true, stopAfter: 1}); err == nil {
		t.Fatal("the attempt should have failed")
	}
	if offset, _ := resumeProgress(t, db); offset != -1 {
		t.Fatalf("progress at %d", offset)
	}
	second := &resumeClient{data: data, can: true}
	rec, err := library.Ingest(context.Background(), item, second)
	if err != nil {
		t.Fatal(err)
	}
	if second.starts[0] != 0 {
		t.Fatalf("started at %d", second.starts[0])
	}
	checkResumed(t, dir, db, rec, data)
}

// The bytes on disk are not what was recorded: not trusted. It starts from the
// beginning, and the file comes out exact. Equal length: only the hash can tell.
func TestResumeRefusesACorruptedPrefix(t *testing.T) {
	for name, damage := range map[string]func(*testing.T, string){
		"a flipped byte": func(t *testing.T, part string) {
			f, err := os.OpenFile(part, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteAt([]byte{0xFF, 0x00, 0xFF}, 3<<20); err != nil {
				t.Fatal(err)
			}
		},
		"shorter than recorded": func(t *testing.T, part string) {
			if err := os.Truncate(part, 1<<20); err != nil {
				t.Fatal(err)
			}
		},
		"gone": func(t *testing.T, part string) {
			if err := os.Remove(part); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			withCheckpoints(t)
			dir := t.TempDir()
			library, db := openTestLibrary(t, dir)
			data := resumeBytes(resumeTestSize)
			item := resumeItem(resumeTestSize)
			if _, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: true, stopAfter: 7}); err == nil {
				t.Fatal("the attempt should have failed")
			}
			_, path := resumeProgress(t, db)
			damage(t, resumePartFile(dir, path))
			second := &resumeClient{data: data, can: true}
			rec, err := library.Ingest(context.Background(), item, second)
			if err != nil {
				t.Fatal(err)
			}
			if second.starts[0] != 0 {
				t.Fatalf("it used damaged bytes: started at %d", second.starts[0])
			}
			checkResumed(t, dir, db, rec, data)
		})
	}
}

// The tail past the last record may be anything: it is cut, never reused.
func TestResumeCutsWhatWasNeverRecorded(t *testing.T) {
	withCheckpoints(t)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	if _, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: true, stopAfter: 7}); err == nil {
		t.Fatal("the attempt should have failed")
	}
	_, path := resumeProgress(t, db)
	f, err := os.OpenFile(resumePartFile(dir, path), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xAA}, 1<<20), 6<<20); err != nil { // garbage past the checkpoint
		t.Fatal(err)
	}
	f.Close()
	second := &resumeClient{data: data, can: true}
	rec, err := library.Ingest(context.Background(), item, second)
	if err != nil {
		t.Fatal(err)
	}
	if second.starts[0] != 6<<20 {
		t.Fatalf("started at %d", second.starts[0])
	}
	checkResumed(t, dir, db, rec, data)
}

// A part longer than the file (an interrupted write, a filesystem's habit): what is
// past the record is cut, so the file is not left longer than it should be.
func TestResumeCutsAPartLongerThanTheFile(t *testing.T) {
	withCheckpoints(t)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	if _, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: true, stopAfter: 7}); err == nil {
		t.Fatal("the attempt should have failed")
	}
	_, path := resumeProgress(t, db)
	if err := os.Truncate(resumePartFile(dir, path), resumeTestSize+5<<20); err != nil {
		t.Fatal(err)
	}
	rec, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: true})
	if err != nil {
		t.Fatal(err)
	}
	checkResumed(t, dir, db, rec, data)
}

// Interrupted again before the next checkpoint: what was already kept stays kept.
func TestResumeSurvivesASecondInterruption(t *testing.T) {
	withCheckpoints(t)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	if _, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: true, stopAfter: 7}); err == nil {
		t.Fatal("the attempt should have failed")
	}
	if _, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: true, stopAfter: 1}); err == nil {
		t.Fatal("the second attempt should have failed")
	}
	third := &resumeClient{data: data, can: true}
	rec, err := library.Ingest(context.Background(), item, third)
	if err != nil {
		t.Fatal(err)
	}
	if third.starts[0] != 6<<20 {
		t.Fatalf("third attempt started at %d: the second lost what the first kept", third.starts[0])
	}
	checkResumed(t, dir, db, rec, data)
}

// A checkpoint is only ever on a part boundary, however the bytes are written:
// Telegram can only be asked to carry on from there.
func TestResumeCheckpointsOnlyOnPartBoundaries(t *testing.T) {
	withCheckpoints(t)
	partCheckpointEvery = 3 << 19 // 1.5 MiB: not a whole number of parts
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	seen := map[int64]bool{}
	first := &resumeClient{data: data, can: true, chunk: 300 << 10, stopAfter: 7, onPart: func(int) {
		if offset, _ := resumeProgress(t, db); offset >= 0 {
			seen[offset] = true
		}
	}}
	if _, err := library.Ingest(context.Background(), item, first); err == nil {
		t.Fatal("the attempt should have failed")
	}
	if len(seen) == 0 {
		t.Fatal("no checkpoint at all")
	}
	for offset := range seen {
		if offset%telegram.PartSize != 0 {
			t.Fatalf("a checkpoint at %d, off a part boundary", offset)
		}
	}
	second := &resumeClient{data: data, can: true, chunk: 300 << 10}
	rec, err := library.Ingest(context.Background(), item, second)
	if err != nil {
		t.Fatal(err)
	}
	checkResumed(t, dir, db, rec, data)
}

// The finished file is not a checkpoint: a record of it could only ever send the
// next attempt back to the beginning.
func TestResumeDoesNotCheckpointTheEnd(t *testing.T) {
	withCheckpoints(t)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize) // a multiple of the checkpoint distance
	var last int64
	client := &resumeClient{data: data, can: true, onPart: func(int) {
		if offset, _ := resumeProgress(t, db); offset >= 0 {
			last = offset
		}
	}}
	if _, err := library.Ingest(context.Background(), resumeItem(resumeTestSize), client); err != nil {
		t.Fatal(err)
	}
	if last != 8<<20 {
		t.Fatalf("the last checkpoint was at %d, want 8 MiB", last)
	}
}

// A client that cannot send from an offset never has its progress used.
func TestResumeNeverAppendsWithAClientThatCannot(t *testing.T) {
	withCheckpoints(t)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	if _, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: false, stopAfter: 7}); err == nil {
		t.Fatal("the attempt should have failed")
	}
	if offset, _ := resumeProgress(t, db); offset != -1 {
		t.Fatalf("progress kept for a client that cannot resume: %d", offset)
	}
	// Plain clients, with no such method at all, are as before.
	plain := clientFunc(func(_ context.Context, _ telegram.MediaIdentity, w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	rec, err := library.Ingest(context.Background(), item, plain)
	if err != nil {
		t.Fatal(err)
	}
	checkResumed(t, dir, db, rec, data)
}

// Small files take the old way: nothing is recorded for them.
func TestResumeLeavesSmallFilesAlone(t *testing.T) {
	withCheckpoints(t)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(3 << 20)
	item := resumeItem(3 << 20)
	if _, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: true, stopAfter: 2}); err == nil {
		t.Fatal("the attempt should have failed")
	}
	if offset, _ := resumeProgress(t, db); offset != -1 {
		t.Fatalf("progress kept for a small file: %d", offset)
	}
}

// A message edited to another file has another identity: the old part is not it.
func TestResumeDoesNotMistakeAnotherFile(t *testing.T) {
	withCheckpoints(t)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	if _, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: true, stopAfter: 7}); err == nil {
		t.Fatal("the attempt should have failed")
	}
	other := make([]byte, resumeTestSize)
	copy(other, "another file of the same size")
	edited := item
	edited.Identity.ID = "43"
	second := &resumeClient{data: other, can: true}
	rec, err := library.Ingest(context.Background(), edited, second)
	if err != nil {
		t.Fatal(err)
	}
	if second.starts[0] != 0 {
		t.Fatalf("started at %d", second.starts[0])
	}
	got, err := os.ReadFile(filepath.Join(dir, "downloads", rec.Path))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, other) {
		t.Fatal("the edited file took the old file's bytes")
	}
	_ = db
}

// Telegram sending more than it declared is not progress to keep.
func TestResumeDiscardsWhatGrewPastTheDeclaredSize(t *testing.T) {
	withCheckpoints(t)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	if _, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: true, extraBytes: 1 << 20}); err == nil {
		t.Fatal("too many bytes should fail")
	}
	if offset, _ := resumeProgress(t, db); offset != -1 {
		t.Fatalf("progress kept: %d", offset)
	}
}

// A cancelled attempt (the app shutting down) keeps its progress.
func TestResumeKeepsProgressWhenCancelled(t *testing.T) {
	withCheckpoints(t)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	ctx, cancel := context.WithCancel(context.Background())
	first := &resumeClient{data: data, can: true, onPart: func(sent int) {
		if sent == 5 {
			cancel()
		}
	}}
	if _, err := library.Ingest(ctx, item, first); err == nil {
		t.Fatal("the attempt should have been cancelled")
	}
	if offset, _ := resumeProgress(t, db); offset != 4<<20 {
		t.Fatalf("progress at %d", offset)
	}
	second := &resumeClient{data: data, can: true}
	rec, err := library.Ingest(context.Background(), item, second)
	if err != nil {
		t.Fatal(err)
	}
	if second.starts[0] != 4<<20 {
		t.Fatalf("started at %d", second.starts[0])
	}
	checkResumed(t, dir, db, rec, data)
}

// Recovery after a restart: what a queue item still wants stays, and what nothing
// is coming back for goes.
func TestRecoverKeepsWhatIsWantedAndSweepsTheRest(t *testing.T) {
	withCheckpoints(t)
	dir := t.TempDir()
	library, db := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	item := resumeItem(resumeTestSize)
	if _, err := library.Ingest(context.Background(), item, &resumeClient{data: data, can: true, stopAfter: 7}); err == nil {
		t.Fatal("the attempt should have failed")
	}
	_, path := resumeProgress(t, db)
	part := resumePartFile(dir, path)
	addWork := func(status string) {
		if _, err := db.Writer.Exec(`DELETE FROM tgdl_work`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,created_at,updated_at) VALUES('one',?,?,?,1,?,'video','v',?,X'00',?,1,1)`,
			item.GroupID, item.GroupName, item.MessageID, item.Identity.Key(), item.Identity.Size, status); err != nil {
			t.Fatal(err)
		}
	}
	addWork("pending")
	if err := library.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(part); err != nil {
		t.Fatalf("recovery removed a part the queue still wants: %v", err)
	}
	if offset, _ := resumeProgress(t, db); offset != 6<<20 {
		t.Fatalf("progress at %d", offset)
	}
	addWork("skipped") // cancelled by a person
	if err := library.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(part); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an abandoned part stayed: %v", err)
	}
	if offset, _ := resumeProgress(t, db); offset != -1 {
		t.Fatalf("progress at %d", offset)
	}
}

func TestPartsKeptCounts(t *testing.T) {
	withCheckpoints(t)
	dir := t.TempDir()
	library, _ := openTestLibrary(t, dir)
	data := resumeBytes(resumeTestSize)
	if _, err := library.Ingest(context.Background(), resumeItem(resumeTestSize), &resumeClient{data: data, can: true, stopAfter: 7}); err == nil {
		t.Fatal("the attempt should have failed")
	}
	if files, bytes := library.PartsKept(context.Background()); files != 1 || bytes != 6<<20 {
		t.Fatalf("%d files, %d bytes", files, bytes)
	}
}

// ---------- a real crash ----------

// The helper below runs in its own process and dies at a chosen moment, with no
// deferred cleanup or graceful close: nothing runs after os.Exit, as after a power
// cut or the crash this fork had. Only the page cache survives that, not the disk's
// guarantee; the point here is the order of the steps and what recovery does.
func TestResumeHelperProcess(t *testing.T) {
	dir := os.Getenv("TGDL_RESUME_HELPER_DIR")
	if dir == "" {
		t.Skip("run by the crash tests")
	}
	oldMin, oldEvery := resumeMinSize, partCheckpointEvery
	resumeMinSize, partCheckpointEvery = 4<<20, 2<<20
	defer func() { resumeMinSize, partCheckpointEvery = oldMin, oldEvery }()
	db, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	library, err := NewLibrary(db.Writer, db.Reader, filepath.Join(dir, "downloads"), 4)
	if err != nil {
		t.Fatal(err)
	}
	item := resumeItem(resumeTestSize)
	if _, err := db.Writer.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,created_at,updated_at) VALUES('one',?,?,?,1,?,'video','v',?,X'00','processing',1,1)`,
		item.GroupID, item.GroupName, item.MessageID, item.Identity.Key(), item.Identity.Size); err != nil {
		t.Fatal(err)
	}
	client := &resumeClient{data: resumeBytes(resumeTestSize), can: true}
	dieAtPart, _ := strconv.Atoi(os.Getenv("TGDL_RESUME_DIE_PART"))
	switch os.Getenv("TGDL_RESUME_DIE") {
	case "part": // in the middle of the data
		client.onPart = func(sent int) {
			if sent == dieAtPart {
				os.Exit(42)
			}
		}
	case "before-record": // the bytes are synced, the record is not written
		saves := 0
		partBeforeSave = func() {
			if saves++; saves == dieAtPart {
				os.Exit(42)
			}
		}
	case "published": // the file is in place, its progress record is not cleared
		partAfterPublish = func() { os.Exit(42) }
	}
	_, err = library.Ingest(context.Background(), item, client)
	fmt.Println("finished without dying:", err)
	os.Exit(0)
}

func runResumeCrash(t *testing.T, dir, how string, part int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestResumeHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "TGDL_RESUME_HELPER_DIR="+dir, "TGDL_RESUME_DIE="+how, "TGDL_RESUME_DIE_PART="+strconv.Itoa(part))
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 42 {
		t.Fatalf("the helper did not die where it should: %v\n%s", err, out)
	}
}

func TestResumeAfterARealCrash(t *testing.T) {
	data := resumeBytes(resumeTestSize)
	for _, c := range []struct {
		name  string
		how   string
		part  int
		start int64 // where the next attempt must start
	}{
		{"mid-data, past a checkpoint", "part", 7, 6 << 20},
		{"mid-data, before any checkpoint", "part", 1, 0},
		{"bytes synced, record not written", "before-record", 2, 2 << 20},
		{"first record not written", "before-record", 1, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			withCheckpoints(t)
			dir := t.TempDir()
			runResumeCrash(t, dir, c.how, c.part)
			// A new start, as the app does it: recovery, then the queue again.
			library, db := openTestLibrary(t, dir)
			if err := library.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			second := &resumeClient{data: data, can: true}
			rec, err := library.Ingest(context.Background(), resumeItem(resumeTestSize), second)
			if err != nil {
				t.Fatal(err)
			}
			if second.starts[0] != c.start {
				t.Fatalf("started at %d, want %d", second.starts[0], c.start)
			}
			if want := (int64(resumeTestSize) - c.start) >> 20; second.sentParts.Load() != want {
				t.Fatalf("fetched %d parts, want %d", second.sentParts.Load(), want)
			}
			got, err := os.ReadFile(filepath.Join(dir, "downloads", rec.Path))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, data) {
				t.Fatal("the file is not the original")
			}
			var stored string
			if err := db.Reader.QueryRow(`SELECT file_hash FROM downloads WHERE file_path=?`, rec.Path).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			if stored != hex.EncodeToString(sum[:]) {
				t.Fatal("the catalogue's hash is not the whole file's")
			}
			if offset, _ := resumeProgress(t, db); offset != -1 {
				t.Fatalf("progress left behind: %d", offset)
			}
			var leftovers []string
			filepath.WalkDir(filepath.Join(dir, "downloads"), func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() && strings.HasSuffix(p, ".part") {
					leftovers = append(leftovers, p)
				}
				return nil
			})
			// Recovery removes a part with no record: its random name would never
			// be used again.
			if len(leftovers) != 0 {
				t.Fatalf("partial files left behind: %v", leftovers)
			}
		})
	}
}

// Dying after the file is published but before its record is cleared: recovery
// finishes it from the journal and the record does not keep a ghost of it.
func TestResumeCrashAfterPublishing(t *testing.T) {
	withCheckpoints(t)
	dir := t.TempDir()
	runResumeCrash(t, dir, "published", 0)
	library, db := openTestLibrary(t, dir)
	if err := library.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if offset, _ := resumeProgress(t, db); offset != -1 {
		t.Fatalf("a record stayed for a published file: %d", offset)
	}
	var files int
	if err := db.Reader.QueryRow(`SELECT COUNT(*) FROM downloads WHERE telegram_media_id='42'`).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if files != 1 {
		t.Fatalf("%d catalogue entries after recovery", files)
	}
	// And the queue item, trying again, costs nothing.
	second := &resumeClient{data: resumeBytes(resumeTestSize), can: true}
	if _, err := library.Ingest(context.Background(), resumeItem(resumeTestSize), second); err != nil {
		t.Fatal(err)
	}
	if second.sentParts.Load() != 0 {
		t.Fatalf("fetched %d parts of a file already saved", second.sentParts.Load())
	}
}
