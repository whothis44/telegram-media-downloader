package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ixFakeDocker stands in for the docker program: no real docker is run.
type ixFakeDocker struct {
	mu         sync.Mutex
	engineUp   bool
	upAfter    int               // engine checks after Docker Desktop starts until it is up
	containers map[string]string // name -> "true" (running) or "false"
	desktop    int               // times Docker Desktop was started
	calls      [][]string
	noDocker   bool
}

func (f *ixFakeDocker) find() (string, error) {
	if f.noDocker {
		return "", errors.New("not found")
	}
	return "/fake/docker", nil
}

func (f *ixFakeDocker) startDesktop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.desktop++
	return nil
}

func (f *ixFakeDocker) run(_ context.Context, bin string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{bin}, args...))
	exit := errors.New("exit status 1")
	switch args[0] {
	case "info":
		if !f.engineUp && f.desktop > 0 {
			if f.upAfter--; f.upAfter <= 0 {
				f.engineUp = true
			}
		}
		if !f.engineUp {
			return "failed to connect to the docker API at npipe:////./pipe/dockerDesktopLinuxEngine", exit
		}
		return "28.3.2", nil
	case "inspect":
		name := args[len(args)-1]
		st, ok := f.containers[name]
		if !ok {
			return "Error: No such object: " + name, exit
		}
		return st, nil
	case "start":
		name := args[len(args)-1]
		if _, ok := f.containers[name]; !ok {
			return "Error response from daemon: No such container: " + name, exit
		}
		f.containers[name] = "true"
		return name, nil
	}
	return "unknown command", exit
}

func (f *ixFakeDocker) ran(cmd string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Join(c[1:], " ") == cmd {
			return true
		}
	}
	return false
}

func ixUseFakeDocker(t *testing.T, f *ixFakeDocker) {
	t.Helper()
	old, poll, ready := ixDocker, ixDockerPoll, ixDockerReadyWait
	ixDocker, ixDockerPoll, ixDockerReadyWait = f, time.Millisecond, 0
	t.Cleanup(func() { ixDocker, ixDockerPoll, ixDockerReadyWait = old, poll, ready })
}

func ixStartNow(t *testing.T, a *App, ctx context.Context) ixMLStartStatus {
	t.Helper()
	if !a.ixMLStartClaim(false) {
		t.Fatal("a start was already under way")
	}
	a.ixMLStartRun(ctx)
	st := a.ixMLStartStatusNow()
	if st.Running || st.FinishedAt == 0 {
		t.Fatalf("the start did not finish: %+v", st)
	}
	return st
}

func TestIxDockerStartsDesktopThenTheStoppedContainer(t *testing.T) {
	f := &ixFakeDocker{upAfter: 3, containers: map[string]string{"immich-ml": "false", "speaches": "true"}}
	ixUseFakeDocker(t, f)
	a, ctx := ix8App(t, map[string]any{"mlUrl": "http://127.0.0.1:3003", "sttUrl": "http://127.0.0.1:8000"})
	st := ixStartNow(t, a, ctx)
	if st.Error != "" {
		t.Fatalf("error: %s", st.Error)
	}
	if f.desktop != 1 {
		t.Fatalf("Docker Desktop started %d times, want 1", f.desktop)
	}
	if len(st.Containers) != 2 || st.Containers[0].Name != "immich-ml" || st.Containers[0].State != "started" || st.Containers[1].Name != "speaches" || st.Containers[1].State != "running" {
		t.Fatalf("containers: %+v", st.Containers)
	}
	if !f.ran("start immich-ml") || f.ran("start speaches") {
		t.Fatalf("calls: %v", f.calls)
	}
	// Only fixed arguments and the container names reach docker.
	allowed := map[string]bool{"info": true, "--format": true, "{{.ServerVersion}}": true, "inspect": true, "-f": true, "{{.State.Running}}": true, "start": true, "immich-ml": true, "speaches": true}
	for _, c := range f.calls {
		for _, arg := range c[1:] {
			if !allowed[arg] {
				t.Fatalf("unexpected docker argument %q in %v", arg, c)
			}
		}
	}
}

func TestIxDockerMissingContainerIsNotInstalled(t *testing.T) {
	f := &ixFakeDocker{engineUp: true, containers: map[string]string{}}
	ixUseFakeDocker(t, f)
	a, ctx := ix8App(t, map[string]any{"mlUrl": "http://127.0.0.1:3003"})
	st := ixStartNow(t, a, ctx)
	if f.desktop != 0 {
		t.Fatal("Docker Desktop was started while the engine ran")
	}
	if len(st.Containers) != 1 || st.Containers[0].State != "missing" || !strings.Contains(st.Containers[0].Message, "docs/image-recognition.md") {
		t.Fatalf("containers: %+v", st.Containers)
	}
	if f.ran("start immich-ml") {
		t.Fatal("a missing container was started")
	}
	// No speech-to-text address: its container is left alone.
	if f.ran("inspect -f {{.State.Running}} speaches") {
		t.Fatal("speaches was looked up without an address")
	}
}

func TestIxDockerNotInstalledOrNothingToStart(t *testing.T) {
	f := &ixFakeDocker{noDocker: true}
	ixUseFakeDocker(t, f)
	a, ctx := ix8App(t, map[string]any{"mlUrl": "http://127.0.0.1:3003"})
	if st := ixStartNow(t, a, ctx); !strings.Contains(st.Error, "Docker is not installed") {
		t.Fatalf("error: %q", st.Error)
	}
	b, ctx2 := ix8App(t, map[string]any{})
	if st := ixStartNow(t, b, ctx2); !strings.Contains(st.Error, "address") || len(f.calls) != 0 {
		t.Fatalf("error: %q, calls %v", st.Error, f.calls)
	}
}

func TestIxDockerContainerNamesAreChecked(t *testing.T) {
	base := ixDefaultSettings()
	if base.MLContainer != "immich-ml" || base.STTContainer != "speaches" || base.MLAutoStart {
		t.Fatalf("defaults: %q %q auto %v", base.MLContainer, base.STTContainer, base.MLAutoStart)
	}
	for _, bad := range []string{"-rm", "--privileged", "immich ml", "a;b", "$(calc)", "a&b", "a/b", `a\b`, ".hidden", strings.Repeat("a", 65), "naïve"} {
		if _, err := ixParseSettings(map[string]any{"mlContainer": bad}, base); err == nil {
			t.Fatalf("mlContainer %q was accepted", bad)
		}
		if _, err := ixParseSettings(map[string]any{"sttContainer": bad}, base); err == nil {
			t.Fatalf("sttContainer %q was accepted", bad)
		}
	}
	s, err := ixParseSettings(map[string]any{"mlContainer": " my-ml.1 ", "sttContainer": "", "mlAutoStart": true}, base)
	if err != nil {
		t.Fatal(err)
	}
	if s.MLContainer != "my-ml.1" || s.STTContainer != "speaches" || !s.MLAutoStart {
		t.Fatalf("parsed: %q %q auto %v", s.MLContainer, s.STTContainer, s.MLAutoStart)
	}
	// A bad name saved some other way is never passed to docker.
	s.MLURL, s.MLContainer = "http://127.0.0.1:3003", "-rm"
	if w := ixMLWantedContainers(s); len(w) != 1 || w[0].name != "immich-ml" {
		t.Fatalf("wanted: %+v", w)
	}
}

func TestIxDockerAutoStart(t *testing.T) {
	f := &ixFakeDocker{engineUp: true, containers: map[string]string{"immich-ml": "true"}}
	ixUseFakeDocker(t, f)
	a, ctx := ix8App(t, map[string]any{"mlUrl": "http://127.0.0.1:3003"})
	var last time.Time
	// Off (the default): nothing happens, not even when the archive starts.
	if a.ixMLAutoStartStep(ctx, true, &last) || a.ixMLAutoStartStep(ctx, false, &last) || len(f.calls) != 0 {
		t.Fatalf("auto start off ran docker: %v", f.calls)
	}
	s, _ := ixParseSettings(map[string]any{"mlAutoStart": true}, a.ixLoadSettings(ctx))
	if err := a.ixSaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if !a.ixMLAutoStartStep(ctx, true, &last) || len(f.calls) == 0 {
		t.Fatal("auto start on did not run when the archive started")
	}
	if st := a.ixMLStartStatusNow(); !st.Auto || st.Containers[0].State != "running" {
		t.Fatalf("status: %+v", st)
	}
	// Later: only while the service does not answer, and at most every 30 minutes.
	if a.ixMLAutoStartStep(ctx, false, &last) {
		t.Fatal("ran again while nothing was known to be down")
	}
	a.ixMLNote(false, "the image recognition service does not answer")
	if a.ixMLAutoStartStep(ctx, false, &last) {
		t.Fatal("ran again within 30 minutes")
	}
	last = time.Now().Add(-31 * time.Minute)
	if !a.ixMLAutoStartStep(ctx, false, &last) {
		t.Fatal("did not run again after 30 minutes with the service down")
	}
	a.ixMLNote(true, "")
	last = time.Now().Add(-31 * time.Minute)
	if a.ixMLAutoStartStep(ctx, false, &last) {
		t.Fatal("ran while the service answers")
	}
}

func TestIxDockerRouteIsAdminOnly(t *testing.T) {
	f := &ixFakeDocker{engineUp: true, containers: map[string]string{}}
	ixUseFakeDocker(t, f)
	a, ctx := ix8App(t, map[string]any{"mlUrl": "http://127.0.0.1:3003"})
	get := func(token string) int {
		r := httptest.NewRequest("GET", "/api/ix/ml/start", nil)
		r.AddCookie(&http.Cookie{Name: "tg_dl_session", Value: token})
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w.Code
	}
	guest, err := a.sessions.Create(ctx, "guest")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := a.sessions.Create(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if code := get(guest); code != http.StatusForbidden {
		t.Fatalf("guest: %d", code)
	}
	if code := get(admin); code != http.StatusOK {
		t.Fatalf("admin: %d", code)
	}
}
