package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Starting the optional recognition services: the image-recognition and
// speech-to-text Docker containers on this PC (docs/image-recognition.md).
// Docker is only ever run with fixed arguments and checked container names,
// never through a shell.

const (
	ixMLDefaultContainer  = "immich-ml"
	ixSTTDefaultContainer = "speaches"
	ixMLAutoStartEvery    = 30 * time.Minute
)

// ixContainerRe: a Docker container name (it can't start with a dash, so
// it is never read as an option).
var ixContainerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// How long to wait for Docker Desktop to start, how often to ask, and how
// long to wait for a started service to answer (tests shorten them).
var (
	ixDockerEngineWait = 3 * time.Minute
	ixDockerPoll       = 3 * time.Second
	ixDockerReadyWait  = 90 * time.Second
	ixMLAutoStartDelay = 20 * time.Second
)

// ixDockerRunner runs the docker program; tests replace it.
type ixDockerRunner interface {
	// find returns where the docker program is.
	find() (string, error)
	// run runs docker with the given arguments and returns what it printed.
	run(ctx context.Context, bin string, args ...string) (string, error)
	// startDesktop starts Docker Desktop (Windows) without waiting for it.
	startDesktop() error
}

var ixDocker ixDockerRunner = ixExecDocker{}

type ixExecDocker struct{}

func (ixExecDocker) find() (string, error) {
	if p, err := exec.LookPath("docker"); err == nil {
		return p, nil
	}
	for _, p := range ixDockerFallbacks() {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("the docker program was not found on this PC")
}

func (ixExecDocker) run(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	ixHideWindow(cmd)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (ixExecDocker) startDesktop() error { return ixStartDockerDesktop() }

// ---------- progress ----------

// ixMLStartResult: what happened to one container.
type ixMLStartResult struct {
	Name    string `json:"name"`
	Service string `json:"service"` // image recognition, speech to text
	State   string `json:"state"`   // running, started, missing, error
	Message string `json:"message,omitempty"`
}

type ixMLStartStatus struct {
	Running    bool              `json:"running"`
	Auto       bool              `json:"auto,omitempty"` // started by the "when the archive starts" setting
	Step       string            `json:"step,omitempty"`
	Error      string            `json:"error,omitempty"`
	StartedAt  int64             `json:"startedAt,omitempty"`
	FinishedAt int64             `json:"finishedAt,omitempty"`
	Containers []ixMLStartResult `json:"containers"`
}

type ixMLStartBox struct {
	mu sync.Mutex
	st ixMLStartStatus
}

var ixMLStartState sync.Map // *App -> *ixMLStartBox

func (a *App) ixMLStartBox() *ixMLStartBox {
	v, _ := ixMLStartState.LoadOrStore(a, &ixMLStartBox{})
	return v.(*ixMLStartBox)
}

func (a *App) ixMLStartStatusNow() ixMLStartStatus {
	box := a.ixMLStartBox()
	box.mu.Lock()
	defer box.mu.Unlock()
	st := box.st
	st.Containers = append([]ixMLStartResult{}, box.st.Containers...)
	return st
}

// ixMLStartClaim marks a start as under way; false when one already is.
func (a *App) ixMLStartClaim(auto bool) bool {
	box := a.ixMLStartBox()
	box.mu.Lock()
	defer box.mu.Unlock()
	if box.st.Running {
		return false
	}
	box.st = ixMLStartStatus{Running: true, Auto: auto, Step: "Looking for Docker…", StartedAt: time.Now().UnixMilli(), Containers: []ixMLStartResult{}}
	return true
}

func (a *App) ixMLStartStep(step string) {
	box := a.ixMLStartBox()
	box.mu.Lock()
	box.st.Step = step
	box.mu.Unlock()
}

// ---------- the start itself ----------

type ixWantedContainer struct{ name, service string }

// ixMLWantedContainers: the containers of the services that have an address.
func ixMLWantedContainers(s ixSettings) []ixWantedContainer {
	pick := func(name, def string) string {
		if !ixContainerRe.MatchString(name) {
			return def
		}
		return name
	}
	var out []ixWantedContainer
	if s.MLURL != "" {
		out = append(out, ixWantedContainer{pick(s.MLContainer, ixMLDefaultContainer), "image recognition"})
	}
	if s.STTURL != "" {
		out = append(out, ixWantedContainer{pick(s.STTContainer, ixSTTDefaultContainer), "speech to text"})
	}
	return out
}

// ixMLStartRun starts Docker and the containers (after ixMLStartClaim).
func (a *App) ixMLStartRun(ctx context.Context) {
	s := a.ixLoadSettings(ctx)
	results, err := a.ixMLStartSequence(ctx, s)
	box := a.ixMLStartBox()
	box.mu.Lock()
	box.st.Running = false
	box.st.Step = ""
	box.st.FinishedAt = time.Now().UnixMilli()
	box.st.Containers = results
	if err != nil {
		box.st.Error = err.Error()
	}
	auto := box.st.Auto
	box.mu.Unlock()
	// The log names the states only, never what docker printed.
	var parts []string
	for _, r := range results {
		parts = append(parts, r.Name+" "+r.State)
	}
	if err != nil {
		parts = append(parts, "Docker not ready")
	}
	how := "button"
	if auto {
		how = "automatic"
	}
	a.ixLog("recognition services start (%s): %s", how, strings.Join(parts, ", "))
}

func (a *App) ixMLStartSequence(ctx context.Context, s ixSettings) ([]ixMLStartResult, error) {
	wanted := ixMLWantedContainers(s)
	if len(wanted) == 0 {
		return nil, errors.New("Neither image recognition nor speech to text has an address yet: enter one and save first.")
	}
	d := ixDocker
	bin, err := d.find()
	if err != nil {
		return nil, errors.New("Docker is not installed on this PC, or the docker program can't be found. To install it, see docs/image-recognition.md.")
	}
	a.ixMLStartStep("Checking Docker…")
	if !ixDockerEngineUp(ctx, d, bin) {
		a.ixMLStartStep("Starting Docker…")
		if err := d.startDesktop(); err != nil {
			return nil, err
		}
		a.ixMLStartStep("Starting Docker… (this can take a minute or two)")
		deadline := time.Now().Add(ixDockerEngineWait)
		for !ixDockerEngineUp(ctx, d, bin) {
			if time.Now().After(deadline) {
				return nil, errors.New("Docker Desktop did not finish starting within 3 minutes. Wait until it says Engine running, then try again.")
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(ixDockerPoll):
			}
		}
	}
	a.ixMLStartStep("Starting the containers…")
	var out []ixMLStartResult
	for _, w := range wanted {
		out = append(out, ixDockerStartContainer(ctx, d, bin, w))
	}
	// Shown while the services are given time to answer.
	box := a.ixMLStartBox()
	box.mu.Lock()
	box.st.Containers = append([]ixMLStartResult{}, out...)
	box.mu.Unlock()
	a.ixMLStartWaitReady(ctx, s, out)
	return out, nil
}

// ixDockerEngineUp asks Docker whether its engine is running.
func ixDockerEngineUp(ctx context.Context, d ixDockerRunner, bin string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := d.run(ctx, bin, "info", "--format", "{{.ServerVersion}}")
	return err == nil && out != ""
}

func ixDockerStartContainer(ctx context.Context, d ixDockerRunner, bin string, w ixWantedContainer) ixMLStartResult {
	res := ixMLStartResult{Name: w.name, Service: w.service}
	if !ixContainerRe.MatchString(w.name) { // never reached: names are checked when saved
		res.State, res.Message = "error", "the container name is not valid"
		return res
	}
	ictx, cancel := context.WithTimeout(ctx, 20*time.Second)
	out, err := d.run(ictx, bin, "inspect", "-f", "{{.State.Running}}", w.name)
	cancel()
	switch {
	case err != nil && strings.Contains(strings.ToLower(out), "no such"):
		res.State = "missing"
		res.Message = "There is no container named " + w.name + " on this PC. To install it, see docs/image-recognition.md."
		return res
	case err != nil:
		res.State, res.Message = "error", "Docker could not look it up: "+ixDockerFirstLine(out, err)
		return res
	case out == "true":
		res.State, res.Message = "running", "It was already running."
		return res
	}
	sctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	out, err = d.run(sctx, bin, "start", w.name)
	cancel()
	if err != nil {
		res.State, res.Message = "error", "Docker could not start it: "+ixDockerFirstLine(out, err)
		return res
	}
	res.State, res.Message = "started", "Started."
	return res
}

// ixDockerFirstLine: the first line of what docker printed, kept short.
func ixDockerFirstLine(out string, err error) string {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if line = strings.TrimSpace(line); line == "" {
		line = err.Error()
	}
	if len(line) > 200 {
		line = line[:200] + "…"
	}
	return line
}

// ixMLStartWaitReady waits for the services whose containers run to answer,
// and records it, so the connection status in Settings is current.
func (a *App) ixMLStartWaitReady(ctx context.Context, s ixSettings, results []ixMLStartResult) {
	if ixDockerReadyWait <= 0 {
		return
	}
	pings := map[int]func(context.Context) error{}
	for i, r := range results {
		if r.State != "running" && r.State != "started" {
			continue
		}
		switch r.Service {
		case "image recognition":
			if c := a.ixMLClientNow(ctx); c != nil {
				pings[i] = func(ctx context.Context) error {
					err := c.ping(ctx)
					if err == nil {
						a.ixMLNote(true, "")
						a.ixMLWake()
					}
					return err
				}
			}
		case "speech to text":
			if c := a.ixSTTClientNow(ctx); c != nil {
				pings[i] = func(ctx context.Context) error {
					err := c.ping(ctx)
					if err == nil {
						a.ixMLTaskNoter("speech")(true, "")
					}
					return err
				}
			}
		}
	}
	if len(pings) == 0 {
		return
	}
	a.ixMLStartStep("Waiting for the services to answer…")
	deadline := time.Now().Add(ixDockerReadyWait)
	for len(pings) > 0 {
		for i, ping := range pings {
			if ping(ctx) == nil {
				results[i].Message += " It answers."
				delete(pings, i)
			}
		}
		if len(pings) == 0 || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(ixDockerPoll):
		}
	}
	for i := range pings {
		results[i].Message += " It does not answer yet: it may still be loading. Check the connection in a minute."
	}
}

// ---------- start with the archive ----------

// ixRunMLAutoStart starts the services once when the archive starts, then
// again at most every 30 minutes while one of them does not answer, when
// the setting is on.
func (a *App) ixRunMLAutoStart(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(ixMLAutoStartDelay):
	}
	var last time.Time
	first := true
	for {
		a.ixMLAutoStartStep(ctx, first, &last)
		first = false
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// ixMLAutoStartStep runs the start when it is due; true when it ran.
func (a *App) ixMLAutoStartStep(ctx context.Context, first bool, last *time.Time) bool {
	s := a.ixLoadSettings(ctx)
	if !s.MLAutoStart || len(ixMLWantedContainers(s)) == 0 {
		return false
	}
	if !first && (time.Since(*last) < ixMLAutoStartEvery || !a.ixMLServicesDown(ctx, s)) {
		return false
	}
	if !a.ixMLStartClaim(true) {
		return false
	}
	*last = time.Now()
	a.ixMLStartRun(ctx)
	return true
}

// ixMLServicesDown: a service with an address was checked and did not answer.
func (a *App) ixMLServicesDown(ctx context.Context, s ixSettings) bool {
	st := a.ixMLStatusNow(ctx)
	if s.MLURL != "" && st.CheckedAt != 0 && !st.Reachable {
		return true
	}
	return s.STTURL != "" && !st.Speech.Reachable && st.Speech.LastError != ""
}

// ---------- routes ----------

func registerIxDockerRoutes(mux *http.ServeMux, a *App) {
	mux.Handle("POST /api/ix/ml/start", a.requireAdmin(http.HandlerFunc(a.handleIxMLStart)))
	mux.Handle("GET /api/ix/ml/start", a.requireAdmin(http.HandlerFunc(a.handleIxMLStartStatus)))
}

// handleIxMLStart starts Docker and the containers in the background and
// answers at once; GET /api/ix/ml/start follows it.
func (a *App) handleIxMLStart(w http.ResponseWriter, r *http.Request) {
	if len(ixMLWantedContainers(a.ixLoadSettings(r.Context()))) == 0 {
		writeJSONError(w, http.StatusBadRequest, "Neither image recognition nor speech to text has an address yet: enter one and save first.")
		return
	}
	if !a.ixMLStartClaim(false) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "already": true, "status": a.ixMLStartStatusNow()})
		return
	}
	if !a.launchMaintenance(func() { a.ixMLStartRun(a.ctx) }) {
		box := a.ixMLStartBox()
		box.mu.Lock()
		box.st.Running, box.st.Step, box.st.Error = false, "", "The archive is shutting down."
		box.mu.Unlock()
		writeJSONError(w, http.StatusServiceUnavailable, "The archive is shutting down.")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "status": a.ixMLStartStatusNow()})
}

func (a *App) handleIxMLStartStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.ixMLStartStatusNow())
}
