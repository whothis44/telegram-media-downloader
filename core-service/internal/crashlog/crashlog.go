// Package crashlog keeps what a crash leaves behind, and when the app stopped.
//
// A launcher that starts the app again hides a crash: the only trace is text in
// a window, which scrolls away. Go's fatal errors and unrecovered panics cannot
// be caught by the app, but the runtime can write its report to a file too. Each
// run points it at current.log and keeps a marker file, touched every minute, and
// the next start reads what they say:
//
//	current.log has text    it crashed: the report is kept as crash-<time>.log
//	only the marker is left it was killed or lost power, or its window was closed
//	neither                 it stopped cleanly
//
// None of this may stop the app: a failure here costs only the record.
package crashlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

const (
	currentName = "current.log"
	causeName   = "cause.txt" // the runtime's words of the last crash, kept by the supervisor (WriteCause)
	markerName  = "running"
	historyName = "history.jsonl"
	keepDumps   = 10
	keepHistory = 300
	maxParse    = 32 << 20
	modulePath  = "github.com/botnick/telegram-media-downloader/core-service/"

	// How a run ended.
	HowClean    = "clean"
	HowCrash    = "crash"
	HowNoReport = "no report"
)

// Beat is how often the marker is touched, so a run that ended without a report
// has an end time this close to the truth.
var Beat = time.Minute

// Run is one run of the app that has ended.
type Run struct {
	Started    time.Time `json:"started,omitzero"`
	Ended      time.Time `json:"ended"`
	Version    string    `json:"version,omitempty"`
	How        string    `json:"how"`
	Cause      string    `json:"cause,omitempty"`      // the runtime's words, never a panic's own message
	Top        string    `json:"top,omitempty"`        // the functions it was in, innermost first
	At         string    `json:"at,omitempty"`         // the first function of ours in the crashing goroutine
	Goroutines int       `json:"goroutines,omitempty"` // how many the report lists
	Dump       string    `json:"dump,omitempty"`       // the report's file name
}

type marker struct {
	Started time.Time `json:"started"`
	Version string    `json:"version"`
}

// Session is the record of the run under way. A nil Session does nothing.
type Session struct {
	dir     string
	started time.Time
	version string
	stop    chan struct{}
	done    chan struct{}
}

// Start settles what the last run left and starts recording this one. It returns
// the last run if that one did not stop cleanly. Only the process that owns the
// data folder may call it: a second one would take the running app for a dead one.
func Start(dir, version string) (*Session, *Run, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	now := time.Now()
	last, settleErr := settle(dir)
	body, err := json.Marshal(marker{Started: now, Version: version})
	if err != nil {
		return nil, last, err
	}
	if err := os.WriteFile(filepath.Join(dir, markerName), body, 0o644); err != nil {
		return nil, last, errors.Join(settleErr, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, currentName), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, last, errors.Join(settleErr, err)
	}
	defer f.Close() // the runtime keeps its own copy of the handle
	debug.SetTraceback("all")
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		return nil, last, errors.Join(settleErr, err)
	}
	s := &Session{dir: dir, started: now, version: version, stop: make(chan struct{}), done: make(chan struct{})}
	go s.beat()
	return s, last, settleErr
}

func (s *Session) beat() {
	defer close(s.done)
	t := time.NewTicker(Beat)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			_ = os.Chtimes(filepath.Join(s.dir, markerName), now, now)
		}
	}
}

// Finish records a clean stop. Call it last: a crash while shutting down still
// counts as one.
func (s *Session) Finish() {
	if s == nil {
		return
	}
	close(s.stop)
	<-s.done
	_ = addHistory(s.dir, Run{Started: s.started, Ended: time.Now(), Version: s.version, How: HowClean})
	_ = os.Remove(filepath.Join(s.dir, markerName))
}

// WriteCause keeps the runtime's own words of a crash (never a panic's
// message) for the next start's history line. The program's supervisor writes
// it (cmd/tgdl-server, TGDL_CRASH_CAPTURE): the runtime prints them to the
// window before it copies anything to the crash file.
func WriteCause(dir, cause string, at time.Time) error {
	body, err := json.Marshal(struct {
		At    time.Time `json:"at"`
		Cause string    `json:"cause"`
	}{at, cause})
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, causeName+".tmp")
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, causeName))
}

// keptCause is the cause the supervisor kept for a run that started at
// started (empty: none, or an older one). It is read once.
func keptCause(dir string, started time.Time) string {
	path := filepath.Join(dir, causeName)
	body, err := os.ReadFile(path)
	_ = os.Remove(path)
	if err != nil {
		return ""
	}
	var c struct {
		At    time.Time `json:"at"`
		Cause string    `json:"cause"`
	}
	if json.Unmarshal(body, &c) != nil || (!started.IsZero() && c.At.Before(started)) {
		return ""
	}
	return safe(c.Cause)
}

// settle turns what the last run left into a history line.
func settle(dir string) (*Run, error) {
	var run Run
	markerPath := filepath.Join(dir, markerName)
	hasMarker := false
	if body, err := os.ReadFile(markerPath); err == nil {
		hasMarker = true
		var m marker
		if json.Unmarshal(body, &m) == nil {
			run.Started, run.Version = m.Started, m.Version
		}
		if info, err := os.Stat(markerPath); err == nil {
			run.Ended = info.ModTime()
		}
	}
	currentPath := filepath.Join(dir, currentName)
	info, err := os.Stat(currentPath)
	switch {
	case err == nil && info.Size() > 0:
		run.How, run.Ended = HowCrash, info.ModTime()
		if f, err := os.Open(currentPath); err == nil {
			run.Cause, run.Top, run.At, run.Goroutines = Parse(io.LimitReader(f, maxParse))
			f.Close()
		}
		run.Dump = "crash-" + run.Ended.UTC().Format("20060102T150405Z") + ".log"
		if err := os.Rename(currentPath, filepath.Join(dir, run.Dump)); err != nil {
			run.Dump = ""
		}
	case hasMarker:
		run.How = HowNoReport
	default:
		_ = os.Remove(filepath.Join(dir, causeName))
		return nil, nil
	}
	if cause := keptCause(dir, run.Started); cause != "" {
		run.Cause = cause // the runtime's words, which the crash file lacks
	}
	if run.Ended.IsZero() {
		run.Ended = time.Now()
	}
	err = addHistory(dir, run)
	if rmErr := os.Remove(markerPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		err = errors.Join(err, rmErr)
	}
	pruneDumps(dir)
	return &run, err
}

func addHistory(dir string, run Run) error {
	line, err := json.Marshal(run)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, historyName)
	var lines []string
	if body, err := os.ReadFile(path); err == nil {
		lines = strings.Split(strings.TrimSpace(string(body)), "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
	}
	lines = append(lines, string(line))
	if len(lines) > keepHistory {
		lines = lines[len(lines)-keepHistory:]
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func pruneDumps(dir string) {
	names, err := filepath.Glob(filepath.Join(dir, "crash-*.log"))
	if err != nil || len(names) <= keepDumps {
		return
	}
	sort.Strings(names) // their names are their times
	for _, name := range names[:len(names)-keepDumps] {
		_ = os.Remove(name)
	}
}

// Read is the history, newest first.
func Read(dir string) []Run {
	body, err := os.ReadFile(filepath.Join(dir, historyName))
	if err != nil {
		return nil
	}
	var out []Run
	for _, line := range strings.Split(string(body), "\n") {
		var r Run
		if line != "" && json.Unmarshal([]byte(line), &r) == nil {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ended.After(out[j].Ended) })
	return out
}

// Started is when the run under way began, if it is recording.
func Started(dir string) (time.Time, bool) {
	body, err := os.ReadFile(filepath.Join(dir, markerName))
	if err != nil {
		return time.Time{}, false
	}
	var m marker
	if json.Unmarshal(body, &m) != nil {
		return time.Time{}, false
	}
	return m.Started, true
}

// Text is a run in one line, for the window and the diagnostics report.
func (r Run) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", r.How)
	if r.Version != "" {
		fmt.Fprintf(&b, " · %s", r.Version)
	}
	if !r.Started.IsZero() && !r.Ended.Before(r.Started) {
		d := r.Ended.Sub(r.Started)
		switch {
		case r.How == HowNoReport && d < Beat:
			// Its end is only known to within a beat: the marker is touched once
			// a minute, so "after 0s" would claim more than is known.
			b.WriteString(" · within its first minute")
		case r.How == HowNoReport:
			fmt.Fprintf(&b, " · after about %s", d.Round(time.Minute))
		case d < time.Minute:
			fmt.Fprintf(&b, " · after %s", d.Round(time.Second))
		default:
			fmt.Fprintf(&b, " · after %s", d.Round(time.Minute))
		}
	}
	if r.Cause != "" {
		fmt.Fprintf(&b, " · %s", r.Cause)
	}
	if r.Top != "" {
		fmt.Fprintf(&b, " · in %s", r.Top)
	}
	if r.At != "" {
		fmt.Fprintf(&b, " · at %s", r.At)
	}
	if r.Goroutines > 0 {
		fmt.Fprintf(&b, " · %d goroutines", r.Goroutines)
	}
	return b.String()
}

var (
	reGoroutine = regexp.MustCompile(`^goroutine \d+ .*\[`)
	reSafe      = regexp.MustCompile(`[^A-Za-z0-9 _:.,\[\]()=+/*-]`)
)

// Parse reads a crash report for its cause, the functions it was in, where in our
// code that was and how many goroutines it lists. A panic's message can hold
// anything, a file or chat name among them, so only the runtime's own errors are
// quoted. Go writes the words of a fatal error ("fatal error: ...") to the window
// only, before it starts on this file, so for those the functions have to tell.
func Parse(r io.Reader) (cause, top, at string, goroutines int) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var runtimeLine, fallback string
	var tops []string
	inFirst, seenFirst, stack := false, false, false
	prev := ""
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case reGoroutine.MatchString(line):
			goroutines++
			inFirst, seenFirst = !seenFirst, true
		case line == "runtime stack:":
			stack = true
		case cause == "" && strings.HasPrefix(line, "fatal error: "):
			cause = safe(line)
		case cause == "" && strings.HasPrefix(line, "panic: runtime error: "):
			cause = safe(line)
		case cause == "" && strings.HasPrefix(line, "panic: "):
			cause = "panic (its message is only in the report)"
		case cause == "" && strings.HasPrefix(line, "Exception 0x"):
			cause = safe(strings.Join(strings.Fields(line)[:2], " "))
		case runtimeLine == "" && strings.HasPrefix(line, "runtime: ") && !seenFirst:
			runtimeLine = safe(line)
		case (inFirst || stack) && strings.HasPrefix(line, "\t") && prev != "":
			fn := frameName(prev)
			if fn == "" {
				break
			}
			if len(tops) < 5 {
				tops = append(tops, safe(strings.TrimPrefix(fn, modulePath)))
			}
			where := fileOf(line)
			switch {
			case at != "":
			case strings.HasPrefix(fn, modulePath):
				at = safe(strings.TrimPrefix(fn, modulePath) + " " + where)
			case fallback == "" && strings.Contains(fn, "/") && strings.Contains(strings.SplitN(fn, "/", 2)[0], "."):
				fallback = safe(fn + " " + where)
			}
		}
		if line == "" {
			stack = false
		}
		prev = line
	}
	if at == "" {
		at = fallback
	}
	if runtimeLine != "" {
		if cause == "" {
			cause = runtimeLine
		} else {
			cause += " (" + runtimeLine + ")"
		}
	}
	if cause == "" && goroutines > 0 {
		cause = "fatal error (its words are only in the window)"
	}
	return cause, strings.Join(tops, " < "), at, goroutines
}

// frameName is a stack line's function without its arguments, or "" if it is no
// function line.
func frameName(line string) string {
	if line == "" || line[0] == '\t' || strings.HasPrefix(line, "created by ") || !strings.HasSuffix(line, ")") {
		return ""
	}
	depth := 0
	for i := len(line) - 1; i >= 0; i-- {
		switch line[i] {
		case ')':
			depth++
		case '(':
			depth--
			if depth == 0 {
				return line[:i]
			}
		}
	}
	return ""
}

// fileOf is a location line's file name and line number.
func fileOf(line string) string {
	line = strings.TrimSpace(line)
	if i := strings.IndexByte(line, ' '); i > 0 {
		line = line[:i]
	}
	if i := strings.LastIndexByte(line, '/'); i >= 0 {
		line = line[i+1:]
	}
	return line
}

func safe(s string) string {
	s = reSafe.ReplaceAllString(s, "")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
