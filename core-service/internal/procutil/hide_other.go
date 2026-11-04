//go:build !windows

package procutil

import "os/exec"

// HideWindow keeps a console window from flashing up when cmd runs (on
// Windows; elsewhere there is none). Call it on every command run in the
// background, before it starts.
func HideWindow(*exec.Cmd) {}
