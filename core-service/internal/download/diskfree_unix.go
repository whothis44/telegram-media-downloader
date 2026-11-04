//go:build linux || darwin

package download

import "golang.org/x/sys/unix"

// diskFree is the space free to this user on the disk holding dir.
func diskFree(dir string) (int64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
