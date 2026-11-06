package app

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ix.12.7: what the background checks read from the library (the NAS), hour
// by hour. They share the network with the staging folder's copies to the
// library: integrity checks read each file whole, and the video index's
// fingerprints decode each video whole (ffmpeg, two frames a second).

const (
	ixReadIntegrity = "integrity checks"
	ixReadVideoIdx  = "video fingerprints"
)

type ixLibReadHour struct {
	files int
	bytes int64
}

var ixLibReads struct {
	mu    sync.Mutex
	hours map[int64]map[string]*ixLibReadHour // hour start (unix s) -> kind
}

// ixNoteLibRead records one file read whole from the library.
func ixNoteLibRead(kind string, bytes int64) {
	start := time.Now().Truncate(time.Hour).Unix()
	ixLibReads.mu.Lock()
	defer ixLibReads.mu.Unlock()
	if ixLibReads.hours == nil {
		ixLibReads.hours = map[int64]map[string]*ixLibReadHour{}
	}
	for h := range ixLibReads.hours {
		if h < start-24*3600 {
			delete(ixLibReads.hours, h)
		}
	}
	if ixLibReads.hours[start] == nil {
		ixLibReads.hours[start] = map[string]*ixLibReadHour{}
	}
	r := ixLibReads.hours[start][kind]
	if r == nil {
		r = &ixLibReadHour{}
		ixLibReads.hours[start][kind] = r
	}
	r.files++
	r.bytes += bytes
}

// ixNoteLibReadPath records a file read whole, by its size on disk.
func ixNoteLibReadPath(kind, path string) {
	if fi, err := os.Stat(path); err == nil {
		ixNoteLibRead(kind, fi.Size())
	}
}

// ixLibReadsDiag is the report's line: each hour since start (at most a day,
// newest first), what each kind read.
func ixLibReadsDiag() string {
	ixLibReads.mu.Lock()
	defer ixLibReads.mu.Unlock()
	starts := make([]int64, 0, len(ixLibReads.hours))
	for h := range ixLibReads.hours {
		starts = append(starts, h)
	}
	if len(starts) == 0 {
		return "library reads by the background checks: none since start"
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] > starts[j] })
	var parts []string
	for _, h := range starts[:min(len(starts), 12)] {
		kinds := ixLibReads.hours[h]
		var ks []string
		for k := range kinds {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		var x []string
		for _, k := range ks {
			x = append(x, fmt.Sprintf("%s %d files %.0f MB", k, kinds[k].files, float64(kinds[k].bytes)/1e6))
		}
		parts = append(parts, time.Unix(h, 0).In(ixTZ).Format("15:04")+" "+strings.Join(x, ", "))
	}
	return "library reads by the background checks, by hour (they share the network with the staging copies): " + strings.Join(parts, " · ")
}
