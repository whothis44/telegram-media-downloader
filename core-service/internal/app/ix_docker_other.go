//go:build !windows

package app

import "errors"

func ixDockerFallbacks() []string { return nil }

// ixStartDockerDesktop: only Docker Desktop on Windows is started by the
// archive; elsewhere Docker runs as a system service.
func ixStartDockerDesktop() error {
	return errors.New("Docker is not running. Start it (on Linux: sudo systemctl start docker), then try again.")
}
