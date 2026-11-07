package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/app"
	"github.com/botnick/telegram-media-downloader/core-service/internal/crashlog"
)

// The child of the crash tests. As the server: records its run, then dies of a
// real stack overflow, the crash the live app had (or panics). As the next
// start (TGDL_SUPERVISE_HELPER_NEXT): prints what its history says of the run
// before. Both in a process of their own: the crash log keeps its file open in
// the process that starts it.
func TestSuperviseHelperProcess(t *testing.T) {
	dir := os.Getenv("TGDL_SUPERVISE_HELPER_DIR")
	if dir == "" {
		t.Skip("run by the supervisor tests")
	}
	crashes := filepath.Join(dir, app.CrashDir)
	if os.Getenv("TGDL_SUPERVISE_HELPER_NEXT") == "1" {
		_, last, err := crashlog.Start(crashes, "test")
		if err != nil || last == nil {
			_, _ = io.WriteString(os.Stdout, "RESULT none\n")
			os.Exit(0)
		}
		_, _ = io.WriteString(os.Stdout, "RESULT "+last.How+"|"+last.Cause+"|"+last.Text()+"\n")
		os.Exit(0)
	}
	if _, _, err := crashlog.Start(crashes, "test"); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(os.Stderr, "an ordinary log line\n")
	if os.Getenv("TGDL_SUPERVISE_HELPER_PANIC") == "1" {
		panic("Private Group Name/secret.mp4") // a panic's message is never kept
	}
	debug.SetMaxStack(1 << 20)
	var deeper func(int) int
	deeper = func(n int) int { return deeper(n+1) + 1 }
	deeper(0)
}

func helperCmd(dir string, extra ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestSuperviseHelperProcess$")
	cmd.Env = append(append(os.Environ(), "TGDL_SUPERVISE_HELPER_DIR="+dir, supervisedEnv+"=1"), extra...)
	return cmd
}

func superviseWithHelper(t *testing.T, extra ...string) (dir string, code int, window string) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("TGDL_DATA_DIR", dir)
	t.Setenv("TGDL_DOWNLOADS_DIR", filepath.Join(dir, "downloads"))
	old := superviseStart
	superviseStart = func(io.Writer) (*exec.Cmd, io.ReadCloser, error) {
		cmd := helperCmd(dir, extra...)
		pipe, err := cmd.StderrPipe()
		if err != nil {
			return nil, nil, err
		}
		return cmd, pipe, cmd.Start()
	}
	t.Cleanup(func() { superviseStart = old })
	var shown bytes.Buffer
	code, ok := supervise(&shown)
	if !ok {
		t.Fatal("the child was not started")
	}
	body, err := os.ReadFile(filepath.Join(dir, app.CrashDir, windowLogName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, shown.Bytes()) {
		t.Fatal("window.log is not what the window showed")
	}
	return dir, code, string(body)
}

// nextStart is what the next start's history says of the run before:
// how|cause|text, or none.
func nextStart(t *testing.T, dir string) string {
	t.Helper()
	out, _ := helperCmd(dir, "TGDL_SUPERVISE_HELPER_NEXT=1").Output()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "RESULT ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "RESULT "))
		}
	}
	t.Fatalf("no result: %s", out)
	return ""
}

func TestSuperviseKeepsTheRuntimesWordsOfACrash(t *testing.T) {
	dir, code, window := superviseWithHelper(t)
	if code == 0 {
		t.Fatal("the crash's exit code was not passed on")
	}
	if !strings.Contains(window, "an ordinary log line") || !strings.Contains(window, "fatal error: stack overflow") {
		t.Fatalf("window.log: %.400s", window)
	}
	// The next start's history names the cause, which the crash file lacks.
	got := nextStart(t, dir)
	if parts := strings.SplitN(got, "|", 3); len(parts) != 3 || parts[0] != crashlog.HowCrash || !strings.Contains(parts[1], "stack overflow") {
		t.Fatalf("the last run: %s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, app.CrashDir, "cause.txt")); !os.IsNotExist(err) {
		t.Fatalf("cause.txt stayed: %v", err)
	}
}

func TestSuperviseNeverKeepsAPanicsMessage(t *testing.T) {
	dir, code, _ := superviseWithHelper(t, "TGDL_SUPERVISE_HELPER_PANIC=1")
	if code == 0 {
		t.Fatal("the panic's exit code was not passed on")
	}
	got := nextStart(t, dir)
	if got == "none" || strings.Contains(got, "secret") || strings.Contains(got, "Private") {
		t.Fatalf("the last run: %s", got)
	}
}

// A child that cannot be started: the server runs in this process instead.
func TestSuperviseFallsBackWhenTheChildCannotStart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TGDL_DATA_DIR", dir)
	t.Setenv("TGDL_DOWNLOADS_DIR", filepath.Join(dir, "downloads"))
	old := superviseStart
	superviseStart = func(io.Writer) (*exec.Cmd, io.ReadCloser, error) { return nil, nil, os.ErrPermission }
	t.Cleanup(func() { superviseStart = old })
	var shown bytes.Buffer
	if _, ok := supervise(&shown); ok {
		t.Fatal("reported a child that never started")
	}
	if !strings.Contains(shown.String(), "running it directly") {
		t.Fatalf("%q", shown.String())
	}
}

func TestSuperviseOnlyWhenAskedAndNotTwice(t *testing.T) {
	t.Setenv(superviseEnv, "")
	if superviseWanted(nil) {
		t.Fatal("on without being asked")
	}
	t.Setenv(superviseEnv, "1")
	if !superviseWanted(nil) || superviseWanted([]string{"version"}) {
		t.Fatal("server mode only")
	}
	t.Setenv(supervisedEnv, "1")
	if superviseWanted(nil) {
		t.Fatal("the child would start another child")
	}
}

func TestRuntimeCause(t *testing.T) {
	for _, c := range []struct{ out, want string }{
		{"log\nruntime: goroutine stack exceeds 1000000000-byte limit\r\nfatal error: stack overflow\r\n\nruntime stack:\n", "runtime: goroutine stack exceeds 1000000000-byte limit - fatal error: stack overflow"},
		{"fatal error: concurrent map writes\n\ngoroutine 1", "fatal error: concurrent map writes"},
		{"panic: open D:\\Secret Group\\x.mp4: denied\n\ngoroutine 1", "panic (its message is not kept)"},
		{"nothing wrong\n", ""},
	} {
		if got := runtimeCause([]byte(c.out)); got != c.want {
			t.Fatalf("%q: %q, want %q", c.out, got, c.want)
		}
	}
}
