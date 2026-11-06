package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/app"
	"github.com/botnick/telegram-media-downloader/core-service/internal/config"
	"github.com/botnick/telegram-media-downloader/core-service/internal/crashlog"
)

// Keeping the words of a crash (TGDL_CRASH_CAPTURE=1, ix.12.3).
//
// When the Go runtime dies it prints "fatal error: ..." to the window first and
// only then marks itself as dying and copies what follows to the crash file, so
// the crash file (crashlog) names the functions but not the cause. Nothing in a
// process can keep what it writes to the window after it dies. So the server
// runs as a child of a small supervisor in the same program: the supervisor
// copies the child's error output to the window as before and to a bounded
// window.log in the crash folder, and when the child ends badly it keeps the
// runtime's own words (never a panic's message, which could name a file) in
// cause.txt, which the next start's run history uses.
//
// It is opt-in, and it never stops the app from running: if the child cannot be
// started, the server runs in this process as it always did. Ctrl+C reaches both
// (the window sends it to every program in it); the supervisor ignores it and
// waits for the child, whose exit code it passes on, so the launcher's loop and
// the update's restart work as before.

const (
	superviseEnv  = "TGDL_CRASH_CAPTURE"
	supervisedEnv = "TGDL_SUPERVISED"
	windowLogName = "window.log"
	windowLogMax  = 4 << 20 // then it becomes window.log.1, and a new one starts
	windowTailMax = 64 << 10
)

// superviseWanted: server mode, asked for, and not already the child.
func superviseWanted(args []string) bool {
	return len(args) == 0 && os.Getenv(superviseEnv) == "1" && os.Getenv(supervisedEnv) != "1"
}

// superviseStart starts the child (replaceable in tests).
var superviseStart = func(stderr io.Writer) (*exec.Cmd, io.ReadCloser, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), supervisedEnv+"=1")
	cmd.Stdin, cmd.Stdout = os.Stdin, os.Stdout
	pipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return cmd, pipe, nil
}

// supervise runs the server as a child and keeps its error output. ok false:
// the child could not be started, and the caller runs the server itself.
func supervise(stderr io.Writer) (code int, ok bool) {
	cfg, err := config.FromFullEnv(os.Getenv)
	if err != nil {
		return 0, false // the server itself reports it
	}
	dir := filepath.Join(cfg.DataDir, app.CrashDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, false
	}
	interrupts := make(chan os.Signal, 4)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM) // the child shuts down; this one waits
	defer signal.Stop(interrupts)
	cmd, pipe, err := superviseStart(stderr)
	if err != nil {
		_, _ = io.WriteString(stderr, "crash capture: could not start the server as a child ("+err.Error()+"); running it directly\n")
		return 0, false
	}
	tail := copyWindow(pipe, stderr, dir)
	err = cmd.Wait()
	code = 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		code = 1
	}
	if code != 0 {
		if cause := runtimeCause(tail); cause != "" {
			_ = crashlog.WriteCause(dir, cause, time.Now())
		}
	}
	return code, true
}

// copyWindow copies the child's error output to the window and to window.log
// until the child closes it, and returns the last part of it.
func copyWindow(pipe io.Reader, stderr io.Writer, dir string) []byte {
	path := filepath.Join(dir, windowLogName)
	var file *os.File
	var size int64
	open := func() {
		if info, err := os.Stat(path); err == nil && info.Size() >= windowLogMax {
			_ = os.Rename(path, path+".1")
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			file = nil
			return
		}
		file = f
		size = 0
		if info, err := f.Stat(); err == nil {
			size = info.Size()
		}
	}
	open()
	defer func() {
		if file != nil {
			file.Close()
		}
	}()
	var tail []byte
	buf := make([]byte, 32<<10)
	for {
		n, err := pipe.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			_, _ = stderr.Write(chunk) // the window first, as before
			if file != nil {
				if w, werr := file.Write(chunk); werr == nil {
					size += int64(w)
				}
				if size >= windowLogMax {
					file.Close()
					open()
				}
			}
			tail = append(tail, chunk...)
			if len(tail) > windowTailMax {
				tail = append([]byte(nil), tail[len(tail)-windowTailMax:]...)
			}
		}
		if err != nil {
			return tail
		}
	}
}

// runtimeCause is the runtime's own words of a crash in out: its last "fatal
// error:" line and the "runtime:" lines just before it (such as "goroutine stack
// exceeds 1000000000-byte limit"). A panic's message is the program's, which can
// hold a name or a path, so only that it was a panic is kept.
func runtimeCause(out []byte) string {
	lines := strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(line, "fatal error: "):
			cause := line
			if i > 0 {
				if prev := strings.TrimSpace(lines[i-1]); strings.HasPrefix(prev, "runtime: ") {
					cause = prev + " - " + cause
				}
			}
			return safeCause(cause)
		case strings.HasPrefix(line, "panic: "):
			return "panic (its message is not kept)"
		}
	}
	return ""
}

// safeCause keeps a cause short and to printable text.
func safeCause(s string) string {
	s = string(bytes.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, []byte(s)))
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
