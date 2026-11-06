//go:build windows

package diskfree

import "golang.org/x/sys/windows"

// Free is the space free to this user on the disk holding dir; ok is false
// when it cannot be known.
func Free(dir string) (int64, bool) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, false
	}
	return int64(free), true
}

// Local lists this computer's fixed drives (not network shares, removable or
// optical drives) with their free and total space, for picking a local folder.
func Local() []Drive {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var out []Drive
	for i := 0; i < 26; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, err := windows.UTF16PtrFromString(root)
		if err != nil || windows.GetDriveType(p) != windows.DRIVE_FIXED {
			continue
		}
		var free, total, totalFree uint64
		if windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree) != nil {
			continue
		}
		out = append(out, Drive{Root: root, Free: int64(free), Total: int64(total)})
	}
	return out
}
