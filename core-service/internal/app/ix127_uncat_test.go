package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// ix.12.7: copies moved into the library by hand and downloaded again (11-06)
// stay as uncatalogued duplicates beside the re-download. The plan finds
// them by name, message and size, moves to _trash only those whose bytes
// match, leaves the rest, and undo puts them back. Invented files.
func TestIx127UncataloguedCopiesPlan(t *testing.T) {
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
	const hexA, hexB, hexC, hexD = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210", "00112233445566778899aabbccddeeff", "ffeeddccbbaa99887766554433221100"
	body := []byte("invented video bytes")
	// The re-download (catalogued) and the hand-moved copy beside it.
	sum := write("G/videos/clip_10_"+hexA+".mp4", body)
	write("G/videos/clip_10_"+hexB+".mp4", body)
	// Same name and size, other bytes: must stay.
	other := []byte("invented video BYTES")
	write("G/videos/clip_11_"+hexA+".mp4", []byte("invented video zzzzz"))
	write("G/videos/clip_11_"+hexC+".mp4", other)
	// Another message's file, uncatalogued, no catalogued twin: not touched.
	write("G/videos/clip_12_"+hexD+".mp4", body)
	// A name of another shape: not touched.
	write("G/videos/notes.txt", body)
	for i, row := range []struct {
		path, sum string
		msg       int
	}{{"G/videos/clip_10_" + hexA + ".mp4", sum, 10}, {"G/videos/clip_11_" + hexA + ".mp4", hex.EncodeToString(func() []byte { s := sha256.Sum256([]byte("invented video zzzzz")); return s[:] }()), 11}} {
		if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,status,file_hash,telegram_media_size) VALUES('-1001','',?,'clip',?,'video',?,'completed',?,?)`,
			row.msg, len(body), row.path, row.sum, len(body)); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
	}
	ops, err := a.ixPlanUncatalogued(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 {
		t.Fatalf("planned %d steps, want 2 (the copy of 10, and the look-alike of 11 whose bytes differ): %+v", len(ops), ops)
	}
	id, err := a.ixSavePlan(ctx, "uncatalogued-copies", "test", ops)
	if err != nil {
		t.Fatal(err)
	}
	// The file whose bytes differ is skipped; the plan goes on and is applied.
	if err := a.ixApplyPlan(ctx, id); err != nil {
		t.Fatal(err)
	}
	p, err := a.ixPlan(ctx, id, 0)
	if err != nil || p.State != "applied" {
		t.Fatalf("plan state %q: %v", p.State, err)
	}
	states := map[string]int{}
	for _, o := range p.List {
		states[o.State]++
	}
	if states["done"] != 1 || states["skipped"] != 1 {
		t.Fatalf("steps: %v", states)
	}
	gone := func(rel string) bool {
		_, err := os.Stat(filepath.Join(a.downloadsDir, filepath.FromSlash(rel)))
		return os.IsNotExist(err)
	}
	if !gone("G/videos/clip_10_" + hexB + ".mp4") {
		t.Fatal("the hand-moved copy is still in place")
	}
	if gone("G/videos/clip_11_"+hexC+".mp4") || gone("G/videos/clip_12_"+hexD+".mp4") || gone("G/videos/notes.txt") || gone("G/videos/clip_10_"+hexA+".mp4") {
		t.Fatal("a file that is not an exact copy was moved")
	}
	var n int
	_ = a.db.Reader.QueryRow(`SELECT COUNT(*) FROM downloads WHERE file_path=?`, "G/videos/clip_10_"+hexA+".mp4").Scan(&n)
	if n != 1 {
		t.Fatalf("the catalogue changed: %d entries at the kept copy", n)
	}
	if err := a.ixUndoPlan(ctx, id); err != nil {
		t.Fatal(err)
	}
	if gone("G/videos/clip_10_" + hexB + ".mp4") {
		t.Fatal("undo did not put the copy back")
	}
}
