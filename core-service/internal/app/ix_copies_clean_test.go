package app

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestIxNearIdenticalVideosJoinTheCleanUpPlan(t *testing.T) {
	a, ctx := ixTestApp(t)
	put := func(rel, content string, msg, w, h int, dur int64) string {
		full := filepath.Join(a.downloadsDir, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(content))
		hash := hex.EncodeToString(sum[:])
		if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,message_id,file_name,file_size,file_type,file_path,status,file_hash,media_width,media_height,media_duration_ms) VALUES(?,?,?,?,'video',?,'completed',?,?,?,?)`,
			ixTestGroup, msg, filepath.Base(rel), len(content), rel, hash, w, h, dur); err != nil {
			t.Fatal(err)
		}
		return hash
	}
	best := put("c/best-1080.mp4", "the 1080p encode, the best copy....", 1, 1920, 1080, 120000)
	worse := put("c/worse-720.mp4", "the 720p re-encode", 2, 1280, 720, 120300)
	put("d/worse-720-again.mp4", "the 720p re-encode", 3, 1280, 720, 120300) // byte-identical to worse
	clip := put("c/clip.mp4", "only partly the same", 4, 1920, 1080, 119000)
	trimmed := put("c/trimmed.mp4", "the same footage, a minute shorter", 5, 1920, 1080, 60000)
	for _, p := range []struct {
		x, y   string
		ca, cb float64
	}{{best, worse, 0.97, 0.98}, {best, clip, 0.85, 0.86}, {best, trimmed, 0.95, 0.99}} {
		if _, err := a.db.Writer.Exec(`INSERT INTO ix_vdup(key_a,key_b,kind,cover_a,cover_b) VALUES(?,?,'same',?,?)`, "h:"+p.x, "h:"+p.y, p.ca, p.cb); err != nil {
			t.Fatal(err)
		}
	}
	ops, err := a.ixPlanDuplicateCopies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var near []ixMoveOp
	for _, o := range ops {
		if o.Op == ixNearOp {
			near = append(near, o)
		}
	}
	// The byte-identical copy goes first; of the near-identical, only the
	// 720p re-encode (the clip covers too little, the trimmed copy is not
	// as long).
	if len(ops) != 2 || ops[0].Op != "trash" || len(near) != 1 || near[0].Src != "c/worse-720.mp4" || near[0].Repoint != "c/best-1080.mp4" || near[0].Size != 18 {
		t.Fatalf("plan: %+v", ops)
	}
	id, err := a.ixSavePlan(ctx, "duplicate-copies", "test", ops)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing moved before the plan is applied.
	if _, err := os.Stat(filepath.Join(a.downloadsDir, "c", "worse-720.mp4")); err != nil {
		t.Fatal("moved before review")
	}
	if err := a.ixApplyPlan(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.downloadsDir, "c", "worse-720.mp4")); !os.IsNotExist(err) {
		t.Fatal("the near copy is still there")
	}
	var path, hash string
	_ = a.db.Reader.QueryRow(`SELECT file_path, file_hash FROM downloads WHERE message_id=2`).Scan(&path, &hash)
	if path != "c/best-1080.mp4" || hash != best {
		t.Fatalf("entry after the move: %s %s", path, hash)
	}
	_ = a.db.Reader.QueryRow(`SELECT file_path FROM downloads WHERE message_id=3`).Scan(&path)
	if path != "c/best-1080.mp4" {
		t.Fatalf("the byte-identical copy's entry: %s", path)
	}
	if err := a.ixUndoPlan(ctx, id); err != nil {
		t.Fatal(err)
	}
	_ = a.db.Reader.QueryRow(`SELECT file_path, file_hash FROM downloads WHERE message_id=2`).Scan(&path, &hash)
	if path != "c/worse-720.mp4" || hash != worse {
		t.Fatalf("entry after undo: %s %s", path, hash)
	}
	for _, rel := range []string{"c/worse-720.mp4", "d/worse-720-again.mp4", "c/best-1080.mp4"} {
		if _, err := os.Stat(filepath.Join(a.downloadsDir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s after undo: %v", rel, err)
		}
	}
}
