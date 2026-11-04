//go:build linux || darwin

package diskfree

import "golang.org/x/sys/unix"

// Free is the space free to this user on the disk holding dir; ok is false
// when it cannot be known.
func Free(dir string) (int64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
