package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ix.12.13: a file that neither the GPU nor the CPU can open is a broken
// upload and must not count against the GPU; a file only the CPU reads does.
func TestIx1213BrokenFilesDoNotSwitchTheIndexToTheCPU(t *testing.T) {
	ixVdupBackground.Store(false)
	defer ixVdupBackground.Store(true)
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	saved, savedHave := ixFingerprintVideo, ixHaveFFmpeg
	defer func() { ixFingerprintVideo, ixHaveFFmpeg = saved, savedHave }()
	ixHaveFFmpeg = func() bool { return true }
	calls := map[string]int{}
	ixFingerprintVideo = func(_ context.Context, path, hw string) (float64, []ixVFrame, error) {
		name := filepath.Base(path)
		calls[name+"/"+map[bool]string{true: "gpu", false: "cpu"}[hw != ""]]++
		switch {
		case strings.HasPrefix(name, "broken"):
			return 0, nil, errors.New("Invalid data found when processing input")
		case strings.HasPrefix(name, "gpuonly") && hw != "":
			return 0, nil, errors.New("cuda: decoder failed")
		}
		return 10, []ixVFrame{{T: 1, H: 1, HF: 1}}, nil
	}
	if err = os.MkdirAll(filepath.Join(a.downloadsDir, "videos"), 0o755); err != nil {
		t.Fatal(err)
	}
	add := func(i int, name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(a.downloadsDir, "videos", name), []byte("v"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := a.db.Writer.Exec(`INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,status,file_hash) VALUES('-1000000000201','',?,?,1,'video',?,'completed',?)`,
			i+1, name, "videos/"+name, strings.Repeat(string(rune('a'+i)), 64)); err != nil {
			t.Fatal(err)
		}
	}
	step := func() {
		t.Helper()
		if _, err := a.ixVdupStep(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for i, n := range []string{"broken1.mp4", "broken2.mp4", "broken3.mp4", "broken4.mp4"} {
		add(i, n)
	}
	for i := 0; i < 4; i++ {
		step()
	}
	box := a.ixVdup()
	box.mu.Lock()
	gpuErrors, cpuOnly := box.gpuErrors, box.cpuOnly
	box.mu.Unlock()
	if gpuErrors != 0 || cpuOnly {
		t.Fatalf("broken files counted against the GPU: errors %d cpu only %v", gpuErrors, cpuOnly)
	}
	if calls["broken4.mp4/gpu"] != 1 || calls["broken4.mp4/cpu"] != 1 {
		t.Fatalf("the fourth broken file was not tried on the GPU then the CPU: %v", calls)
	}
	// Three files only the CPU reads: the GPU path is at fault, so the rest
	// of the run uses the CPU.
	for i, n := range []string{"gpuonly1.mp4", "gpuonly2.mp4", "gpuonly3.mp4", "fine.mp4"} {
		add(10+i, n)
	}
	for i := 0; i < 4; i++ {
		step()
	}
	box.mu.Lock()
	gpuErrors, cpuOnly = box.gpuErrors, box.cpuOnly
	box.mu.Unlock()
	if gpuErrors != 3 || !cpuOnly {
		t.Fatalf("GPU-only failures: errors %d cpu only %v", gpuErrors, cpuOnly)
	}
	if calls["fine.mp4/gpu"] != 0 || calls["fine.mp4/cpu"] != 1 {
		t.Fatalf("after three GPU failures the next file should go to the CPU only: %v", calls)
	}
}
