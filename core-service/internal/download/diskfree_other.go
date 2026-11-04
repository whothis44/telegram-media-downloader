//go:build !linux && !darwin && !windows

package download

// diskFree is unknown here: the space allowed for staging is kept to.
func diskFree(string) (int64, bool) { return 0, false }
