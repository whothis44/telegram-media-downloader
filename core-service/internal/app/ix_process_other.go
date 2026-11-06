//go:build !windows

package app

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ixProcNow: CPU time (getrusage) and, where /proc has them, the bytes the
// process asked to read and write (rchar, wchar: cache included).
func ixProcNow() (ixProcSample, bool) {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return ixProcSample{}, false
	}
	tv := func(t syscall.Timeval) time.Duration {
		return time.Duration(t.Sec)*time.Second + time.Duration(t.Usec)*time.Microsecond
	}
	s := ixProcSample{at: time.Now(), cpu: tv(ru.Utime) + tv(ru.Stime)}
	if f, err := os.Open("/proc/self/io"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), ":")
			if !ok {
				continue
			}
			n, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			switch k {
			case "rchar":
				s.read = n
			case "wchar":
				s.write = n
			}
		}
		f.Close()
	}
	return s, true
}
