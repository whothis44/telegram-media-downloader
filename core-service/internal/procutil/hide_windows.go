//go:build windows

package procutil

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW: the program gets no console.
const createNoWindow = 0x08000000

// HideWindow keeps a console window from flashing up when cmd runs. Call it
// on every command run in the background, before it starts.
func HideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
