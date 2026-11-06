package app

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"strings"
)

// ixMemoryDiag is the report's [memory] section (ix.12.4): the heap, what
// the process reserved from the system, goroutines, and the functions holding
// the most heap now. Function names and bytes only: never data. The
// per-function figures come from Go's sampled heap profile (one sample about
// every 512 KB allocated), scaled as pprof scales them, so they are
// estimates.
func ixMemoryDiag() []string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	mb := func(b uint64) float64 { return float64(b) / (1 << 20) }
	out := []string{fmt.Sprintf("heap in use %.0f MB (objects %.0f MB) · reserved from the system %.0f MB · goroutines %d · garbage collections %d",
		mb(m.HeapInuse), mb(m.HeapAlloc), mb(m.Sys), runtime.NumGoroutine(), m.NumGC)}
	top := ixHeapTop(10)
	if len(top) == 0 {
		return out
	}
	parts := make([]string, len(top))
	for i, f := range top {
		parts[i] = fmt.Sprintf("%s %.0f MB", f.name, f.bytes/(1<<20))
	}
	return append(out, "most heap held, by function (estimated): "+strings.Join(parts, " · "))
}

type ixHeapFunc struct {
	name  string
	bytes float64
}

// ixHeapTop sums the sampled heap in use by the first function outside the
// runtime in each stack, largest first.
func ixHeapTop(n int) []ixHeapFunc {
	rate := runtime.MemProfileRate
	if rate <= 0 {
		return nil
	}
	var records []runtime.MemProfileRecord
	k, _ := runtime.MemProfile(nil, true)
	for {
		records = make([]runtime.MemProfileRecord, k+50)
		got, ok := runtime.MemProfile(records, true)
		if ok {
			records = records[:got]
			break
		}
		k = got
	}
	by := map[string]float64{}
	for _, r := range records {
		objects, bytes := r.InUseObjects(), r.InUseBytes()
		if objects <= 0 || bytes <= 0 {
			continue
		}
		// pprof's scaling of a sampled heap record.
		avg := float64(bytes) / float64(objects)
		scale := 1 / (1 - math.Exp(-avg/float64(rate)))
		by[ixHeapFrame(r.Stack())] += float64(bytes) * scale
	}
	out := make([]ixHeapFunc, 0, len(by))
	for name, b := range by {
		out = append(out, ixHeapFunc{name, b})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].bytes > out[j].bytes })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// ixHeapFrame is the first function of a stack outside the runtime, shortened
// to package.function.
func ixHeapFrame(stack []uintptr) string {
	frames := runtime.CallersFrames(stack)
	first := ""
	for {
		f, more := frames.Next()
		name := f.Function
		if first == "" {
			first = name
		}
		if name != "" && !strings.HasPrefix(name, "runtime.") && !strings.HasPrefix(name, "internal/") {
			return ixShortFunc(name)
		}
		if !more {
			break
		}
	}
	if first == "" {
		return "unknown"
	}
	return ixShortFunc(first)
}

func ixShortFunc(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}
