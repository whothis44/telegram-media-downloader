package procutil

import (
	"os/exec"
	"testing"
)

// HideWindow can be applied to any command (twice too) and the command
// still runs.
func TestHideWindowLeavesTheCommandRunnable(t *testing.T) {
	bin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no program to run")
	}
	cmd := exec.Command(bin, "version")
	HideWindow(cmd)
	HideWindow(cmd)
	if out, err := cmd.Output(); err != nil || len(out) == 0 {
		t.Fatalf("run: %q %v", out, err)
	}
}
