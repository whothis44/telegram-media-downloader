package crashlog

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const secret = "secret-group-name"

// The app as a real process: it starts recording, then ends in the way the test
// asked for.
func init() {
	dir, how := os.Getenv("CRASHLOG_DIR"), os.Getenv("CRASHLOG_HOW")
	if dir == "" {
		return
	}
	s, _, err := Start(dir, "9.9.9")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(9)
	}
	switch how {
	case "clean":
		s.Finish()
		os.Exit(0)
	case "fatal":
		crashFatal()
	case "panic":
		go crashPanic()
	case "nil":
		go crashNil()
	case "hang":
		fmt.Println("ready")
	}
	time.Sleep(time.Minute)
	os.Exit(8)
}

func crashFatal() {
	var mu sync.Mutex
	mu.Unlock()
}

func crashPanic() { panic(secret) }

func crashNil() {
	var p *int
	fmt.Println(*p)
}

func helper(dir, how string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "CRASHLOG_DIR="+dir, "CRASHLOG_HOW="+how)
	return cmd
}

func TestARealFatalErrorIsKeptAndNamed(t *testing.T) {
	dir := t.TempDir()
	if err := helper(dir, "fatal").Run(); err == nil {
		t.Fatal("the helper should have crashed")
	}
	before := time.Now()
	run, err := settle(dir)
	if err != nil || run == nil {
		t.Fatalf("%v %v", run, err)
	}
	// Go writes the words of a fatal error to the window only: the functions tell.
	if run.How != HowCrash || !strings.Contains(run.Cause, "fatal error") || !strings.Contains(run.Top, "internal/sync.fatal") || !strings.Contains(run.At, "internal/crashlog.crashFatal") {
		t.Fatalf("%+v", run)
	}
	if run.Version != "9.9.9" || run.Started.IsZero() || run.Ended.After(before) || run.Goroutines < 1 {
		t.Fatalf("%+v", run)
	}
	body, err := os.ReadFile(filepath.Join(dir, run.Dump))
	if err != nil || !strings.Contains(string(body), "crashFatal") {
		t.Fatalf("the report: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, markerName)); !os.IsNotExist(err) {
		t.Fatal("the marker stayed")
	}
	again, err := settle(dir)
	if again != nil || err != nil {
		t.Fatalf("settled twice: %+v %v", again, err)
	}
	if got := Read(dir); len(got) != 1 || got[0].How != HowCrash {
		t.Fatalf("%+v", got)
	}
}

// A panic's message can name a private file or chat: the report keeps it, the line
// that goes into diagnostics does not.
func TestAPanicsOwnMessageStaysOutOfTheSummary(t *testing.T) {
	dir := t.TempDir()
	if err := helper(dir, "panic").Run(); err == nil {
		t.Fatal("the helper should have crashed")
	}
	run, err := settle(dir)
	if err != nil || run == nil || run.How != HowCrash {
		t.Fatalf("%+v %v", run, err)
	}
	if strings.Contains(run.Text(), secret) || !strings.Contains(run.Cause, "panic") {
		t.Fatalf("%q", run.Text())
	}
	body, _ := os.ReadFile(filepath.Join(dir, run.Dump))
	if !strings.Contains(string(body), secret) {
		t.Fatal("the report should keep the whole message")
	}
	history, _ := os.ReadFile(filepath.Join(dir, historyName))
	if strings.Contains(string(history), secret) {
		t.Fatal("the message reached the history")
	}
}

func TestARuntimePanicIsNamed(t *testing.T) {
	dir := t.TempDir()
	if err := helper(dir, "nil").Run(); err == nil {
		t.Fatal("the helper should have crashed")
	}
	run, _ := settle(dir)
	if run == nil || !strings.Contains(run.Cause, "invalid memory address") {
		t.Fatalf("%+v", run)
	}
}

func TestACleanStopIsNotACrash(t *testing.T) {
	dir := t.TempDir()
	if err := helper(dir, "clean").Run(); err != nil {
		t.Fatal(err)
	}
	if run, err := settle(dir); run != nil || err != nil {
		t.Fatalf("%+v %v", run, err)
	}
	if got := Read(dir); len(got) != 1 || got[0].How != HowClean || got[0].Started.IsZero() {
		t.Fatalf("%+v", got)
	}
}

func TestAKilledRunLeavesNoReportButIsCounted(t *testing.T) {
	dir := t.TempDir()
	cmd := helper(dir, "hang")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := out.Read(buf); err != nil {
		t.Fatal(err)
	}
	if _, ok := Started(dir); !ok {
		t.Fatal("the run under way is not on record")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	run, err := settle(dir)
	if err != nil || run == nil || run.How != HowNoReport || run.Dump != "" || run.Ended.IsZero() || run.Started.IsZero() {
		t.Fatalf("%+v %v", run, err)
	}
}

// Parsed from what Go prints, with the frames of a fatal error on Windows.
func TestParseFindsOurFunction(t *testing.T) {
	report := "runtime: VirtualAlloc of 1048576 bytes failed with errno=1455\n" +
		"fatal error: runtime: cannot allocate memory\n\nruntime stack:\n" +
		"runtime.throw({0x7ff, 0x1})\n\truntime/panic.go:1094 +0x4b fp=0x1 sp=0x2 pc=0x3\n\n" +
		"goroutine 7 gp=0x1 m=3 mp=0x2 [running]:\n" +
		"runtime.mallocgc(0x10, 0x0, 0x1)\n\truntime/malloc.go:1 +0x1 fp=0x1 sp=0x2 pc=0x3\n" +
		"github.com/gotd/td/bin.(*Buffer).Expand(...)\n\tgithub.com/gotd/td@v0.163.0/bin/buffer.go:2 +0x1\n" +
		modulePath + "internal/app.(*App).ixSomething(0xc000, {0x1, 0x2})\n\t" + modulePath + "internal/app/ix_something.go:42 +0x25 fp=0x1 sp=0x2 pc=0x3\n" +
		"created by " + modulePath + "internal/app.other in goroutine 1\n\t" + modulePath + "internal/app/other.go:1 +0x1\n\n" +
		"goroutine 8 gp=0x2 m=nil [select]:\n" +
		modulePath + "internal/app.notThisOne()\n\t" + modulePath + "internal/app/no.go:9 +0x1\n"
	cause, top, at, n := Parse(strings.NewReader(report))
	if !strings.HasPrefix(top, "runtime.throw < runtime.mallocgc") {
		t.Fatalf("top %q", top)
	}
	if !strings.Contains(cause, "cannot allocate memory") || !strings.Contains(cause, "errno=1455") {
		t.Fatalf("cause %q", cause)
	}
	if at != "internal/app.(*App).ixSomething ix_something.go:42" || n != 2 {
		t.Fatalf("at %q, %d goroutines", at, n)
	}
	// No function of ours: the first that is not the standard library's.
	_, _, at, _ = Parse(strings.NewReader("fatal error: x\n\ngoroutine 1 [running]:\nruntime.a()\n\truntime/a.go:1 +0x1\nsync.b()\n\tsync/b.go:2 +0x1\ngithub.com/gotd/td/c.D(0x1)\n\tgithub.com/gotd/td@v1/c/d.go:3 +0x1\n"))
	if at != "github.com/gotd/td/c.D d.go:3" {
		t.Fatalf("fallback %q", at)
	}
}

func TestOldReportsAndHistoryAreBounded(t *testing.T) {
	dir := t.TempDir()
	for i := range keepDumps + 4 {
		name := fmt.Sprintf("crash-202611%02dT000000Z.log", i+1)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pruneDumps(dir)
	names, _ := filepath.Glob(filepath.Join(dir, "crash-*.log"))
	if len(names) != keepDumps || !strings.Contains(names[0], "20261105") {
		t.Fatalf("%v", names)
	}
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := range keepHistory + 5 {
		if err := addHistory(dir, Run{Ended: base.Add(time.Duration(i) * time.Minute), How: HowClean}); err != nil {
			t.Fatal(err)
		}
	}
	got := Read(dir)
	if len(got) != keepHistory || !got[0].Ended.Equal(base.Add((keepHistory+4)*time.Minute)) {
		t.Fatalf("%d, newest %v", len(got), got[0].Ended)
	}
}

// A folder it cannot use is an error for the caller to report, nothing more.
func TestStartOnAnUnusableFolderFailsPolitely(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, _, err := Start(filepath.Join(file, "sub"), "1")
	if err == nil || s != nil {
		t.Fatalf("%v %v", s, err)
	}
	s.Finish() // nil is fine
	if Read(dir) != nil {
		t.Fatal("no history yet")
	}
	if _, ok := Started(dir); ok {
		t.Fatal("not running")
	}
}
