//go:build windows

package app

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var ixProcIoCounters = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessIoCounters")

// ixProcNow: CPU time (GetProcessTimes) and bytes read and written
// (GetProcessIoCounters: every read and write of the process, files and
// network alike).
func ixProcNow() (ixProcSample, bool) {
	h := windows.CurrentProcess()
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return ixProcSample{}, false
	}
	ticks := func(f windows.Filetime) time.Duration {
		return time.Duration(uint64(f.HighDateTime)<<32|uint64(f.LowDateTime)) * 100
	}
	s := ixProcSample{at: time.Now(), cpu: ticks(kernel) + ticks(user)}
	var io struct {
		ReadOps, WriteOps, OtherOps       uint64
		ReadBytes, WriteBytes, OtherBytes uint64
	}
	if ixProcIoCounters.Find() == nil {
		if r, _, _ := ixProcIoCounters.Call(uintptr(h), uintptr(unsafe.Pointer(&io))); r != 0 {
			s.read, s.write = io.ReadBytes, io.WriteBytes
		}
	}
	return s, true
}
