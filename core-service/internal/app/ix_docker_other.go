//go:build !windows

package app

import (
	"errors"
	"os/exec"
)

func ixDockerFallbacks() []string { return nil }

func ixHideWindow(*exec.Cmd) {}

// ixStartDockerDesktop: only Docker Desktop on Windows is started by the
// archive; elsewhere Docker runs as a system service.
func ixStartDockerDesktop() error {
	return errors.New("Docker is not running. Start it (on Linux: sudo systemctl start docker), then try again.")
}
