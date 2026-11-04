//go:build windows

package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

const ixDockerDesktopExe = `C:\Program Files\Docker\Docker\Docker Desktop.exe`

// ixDockerFallbacks: where Docker Desktop puts the docker program, for when
// it is not on the PATH yet (installed after the archive was started).
func ixDockerFallbacks() []string {
	return []string{`C:\Program Files\Docker\Docker\resources\bin\docker.exe`}
}

// ixStartDockerDesktop starts Docker Desktop and does not wait for it: it
// keeps running on its own after the archive stops.
func ixStartDockerDesktop() error {
	if _, err := os.Stat(ixDockerDesktopExe); err != nil {
		return errors.New(`Docker is not running, and Docker Desktop was not found in C:\Program Files\Docker. Start Docker yourself, then try again.`)
	}
	cmd := exec.Command(ixDockerDesktopExe)
	// Its own process group, away from the archive's console. (Programs the
	// archive runs in the background and waits for, such as docker and
	// ffmpeg, get no console window instead: procutil.HideWindow.)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000200 | 0x00000008} // CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("Docker is not running, and Docker Desktop could not be started: %v", err)
	}
	_ = cmd.Process.Release()
	return nil
}
