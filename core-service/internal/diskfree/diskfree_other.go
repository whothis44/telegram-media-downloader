//go:build !linux && !darwin && !windows

package diskfree

// Free is unknown here.
func Free(string) (int64, bool) { return 0, false }
