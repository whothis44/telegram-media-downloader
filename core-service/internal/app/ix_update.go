package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/version"
)

// Updating without throwing work away.
//
// A restart costs a download in progress its bytes since its last checkpoint (a small
// file, all of them: download/resume.go), so the update does not restart at once. It
// drains: no new file starts (self-destructing media still
// does: it may vanish while waiting), files that have only been waiting for a
// slot go back to the queue, and those already moving bytes finish. Then the
// server exits and the launcher updates it and starts it again.
//
// Exiting only brings the app back if a launcher loops around it, so it needs to
// say so (TGDL_LAUNCHER_RESTARTS=1, see start.example.bat). Without that the app
// never exits by itself: it says the downloads are done, and a person closes the
// window and starts it again.
//
// Draining is kept in memory on purpose: a restart for any other reason must not
// come back refusing to download.
//
// From ix.12.3 a file that keeps its progress (8 MiB or more) does not have to
// finish: it stops right after its next checkpoint (at most 30 s away) and carries
// on after the restart, so the drain takes seconds to minutes, not hours. Files
// too small to resume, and nearly finished ones, finish.
//
// "Install updates automatically" (AutoUpdate) starts the drain by itself when a
// newer release is out, but only with a launcher that says it loops: otherwise
// the app would stop downloading and wait for a person. Going back to downloading
// by hand leaves that version alone. An automatic drain that has not finished
// after ixAutoDrainLimit gives up and goes back to downloading, and tries again
// later, so it can never hold the downloads for long.

const (
	ixUpdateCheckEvery = time.Hour
	ixUpdateWatchEvery = 2 * time.Second
)

// The automatic update: how often it looks, how long a drain may take before it
// gives up, and how long it then waits to try again (variables for tests).
var (
	ixAutoUpdateEvery = time.Minute
	ixAutoDrainLimit  = 20 * time.Minute
	ixAutoRetryAfter  = time.Hour
)

// ixLatestRelease asks for the newest published version (replaceable in tests).
var ixLatestRelease = func(ctx context.Context) (string, error) {
	repo := strings.TrimSpace(os.Getenv("TGDL_RELEASE_REPO"))
	if repo == "" {
		repo = "whothis44/telegram-media-downloader"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "tgdl-ix-update")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub answered %d", resp.StatusCode)
	}
	var out struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return strings.TrimPrefix(out.Tag, "v"), nil
}

var ixVersionNumbers = regexp.MustCompile(`\d+`)

// ixVersionNewer: version a is newer than b, by their numbers in order, as
// ix-update.ps1 decides. 3.4.18-ix.11 is newer than 3.4.18-ix.10.2, and a local
// suffix such as -fix does not count.
func ixVersionNewer(a, b string) bool {
	x, y := ixVersionNumbers.FindAllString(a, -1), ixVersionNumbers.FindAllString(b, -1)
	for i := 0; i < len(x) || i < len(y); i++ {
		p, q := int64(-1), int64(-1)
		if i < len(x) {
			p, _ = strconv.ParseInt(x[i], 10, 64)
		}
		if i < len(y) {
			q, _ = strconv.ParseInt(y[i], 10, 64)
		}
		if p != q {
			return p > q
		}
	}
	return false
}

// ixUpdateInfo is what the page shows.
type ixUpdateInfo struct {
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	Newer     bool   `json:"newer"`
	CheckErr  string `json:"checkError,omitempty"`
	Draining  bool   `json:"draining"`
	Files     int    `json:"files"`     // downloads still in progress
	BytesLeft int64  `json:"bytesLeft"` // what they still have to fetch
	Ready     bool   `json:"ready"`     // nothing in progress: safe to restart
	Restarts  bool   `json:"restarts"`  // a launcher brings the app back after it exits
	Exiting   bool   `json:"exiting"`
	Auto      bool   `json:"auto"`    // Install updates automatically is on
	AutoNow   bool   `json:"autoNow"` // this drain was started automatically
	AutoNote  string `json:"autoNote,omitempty"`
}

type ixUpdateBox struct {
	mu       sync.Mutex
	latest   string
	checkErr string
	checked  time.Time
	checking bool
	watching bool
	exiting  bool
	// The automatic update: its drain began at autoSince (zero: none), a person
	// went back to downloading during one for declined, and it gave up at gaveUp.
	autoSince time.Time
	declined  string
	gaveUp    time.Time
	autoNote  string
}

var ixUpdateState sync.Map // *App -> *ixUpdateBox

func (a *App) ixUpdates() *ixUpdateBox {
	v, _ := ixUpdateState.LoadOrStore(a, &ixUpdateBox{})
	return v.(*ixUpdateBox)
}

func ixLauncherRestarts() bool { return strings.TrimSpace(os.Getenv("TGDL_LAUNCHER_RESTARTS")) == "1" }

// ixUpdateCheck has the newest release looked up in the background. The page
// never waits for GitHub.
func (a *App) ixUpdateCheck() {
	box := a.ixUpdates()
	box.mu.Lock()
	if box.checking || (!box.checked.IsZero() && time.Since(box.checked) < ixUpdateCheckEvery) {
		box.mu.Unlock()
		return
	}
	box.checking = true
	box.mu.Unlock()
	a.ambientWG.Add(1)
	go func() {
		defer a.ambientWG.Done()
		latest, err := ixLatestRelease(a.ctx)
		box.mu.Lock()
		defer box.mu.Unlock()
		box.checking, box.checked = false, time.Now()
		if err != nil {
			box.checkErr = err.Error()
			return
		}
		box.latest, box.checkErr = latest, ""
	}()
}

// ixUpdateNow is the state of the update, without waiting for anything slow.
func (a *App) ixUpdateNow(ctx context.Context) ixUpdateInfo {
	a.ixUpdateCheck()
	box := a.ixUpdates()
	box.mu.Lock()
	info := ixUpdateInfo{Current: version.AppVersion, Latest: box.latest, CheckErr: box.checkErr, Exiting: box.exiting, Restarts: ixLauncherRestarts()}
	box.mu.Unlock()
	info.Newer = info.Latest != "" && ixVersionNewer(info.Latest, info.Current)
	info.Auto = a.ixLoadSettings(ctx).AutoUpdate
	box.mu.Lock()
	info.AutoNow, info.AutoNote = !box.autoSince.IsZero(), box.autoNote
	box.mu.Unlock()
	if a.monitor != nil {
		info.Draining = a.monitor.Draining()
		if info.Draining {
			files, left, err := a.monitor.DrainStatus(ctx)
			if err == nil {
				info.Files, info.BytesLeft, info.Ready = files, left, files == 0
			}
		}
	}
	return info
}

func (a *App) handleIxUpdate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.ixUpdateNow(r.Context()))
}

// handleIxUpdateAction: start (wait for the downloads, then restart), now
// (restart at once), cancel (go back to downloading).
func (a *App) handleIxUpdateAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		return
	}
	if a.monitor == nil {
		writeJSONError(w, http.StatusConflict, "The downloader is not available")
		return
	}
	switch body.Action {
	case "start":
		a.monitor.SetDraining(true)
		a.ixUpdateWatch()
	case "cancel":
		box := a.ixUpdates()
		box.mu.Lock()
		if !box.autoSince.IsZero() {
			// Back to downloading by hand: this version is not tried again by itself.
			box.declined, box.autoSince = box.latest, time.Time{}
			box.autoNote = "You went back to downloading: version " + box.latest + " is not installed by itself. Use the button to install it."
		}
		box.mu.Unlock()
		a.monitor.SetDraining(false)
	case "now":
		if !ixLauncherRestarts() {
			writeJSONError(w, http.StatusConflict, "Nothing would start the app again: close the window and open start.bat yourself.")
			return
		}
		a.monitor.SetDraining(true)
		a.ixUpdateExit()
	default:
		writeJSONError(w, http.StatusBadRequest, "action: start, now or cancel")
		return
	}
	writeJSON(w, http.StatusOK, a.ixUpdateNow(r.Context()))
}

// ixUpdateWatch restarts once the downloads are done, if a launcher will bring
// the app back; otherwise the page says it is ready.
func (a *App) ixUpdateWatch() {
	box := a.ixUpdates()
	box.mu.Lock()
	if box.watching {
		box.mu.Unlock()
		return
	}
	box.watching = true
	box.mu.Unlock()
	a.ambientWG.Add(1)
	go func() {
		defer a.ambientWG.Done()
		defer func() {
			box.mu.Lock()
			box.watching = false
			box.mu.Unlock()
		}()
		tick := time.NewTicker(ixUpdateWatchEvery)
		defer tick.Stop()
		for a.monitor.Draining() {
			select {
			case <-a.ctx.Done():
				return
			case <-tick.C:
			}
			files, _, err := a.monitor.DrainStatus(a.ctx)
			if err == nil && files == 0 && a.monitor.Draining() && ixLauncherRestarts() {
				a.ixUpdateExit()
				return
			}
		}
	}()
}

// ixRunUpdates is the background task of the automatic update.
func (a *App) ixRunUpdates(ctx context.Context) {
	tick := time.NewTicker(ixAutoUpdateEvery)
	defer tick.Stop()
	for {
		a.ixAutoUpdateStep(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ixAutoUpdateStep starts the drain when it should, and gives an automatic one
// up when it takes too long.
func (a *App) ixAutoUpdateStep(ctx context.Context, now time.Time) {
	if a.monitor == nil {
		return
	}
	a.ixUpdateCheck()
	auto := a.ixLoadSettings(ctx).AutoUpdate
	if a.ixAutoDrainCheck(now) {
		return // one is under way
	}
	if !auto || !ixLauncherRestarts() || a.monitor.Draining() {
		return // off; nothing would bring the app back; or a person started one
	}
	box := a.ixUpdates()
	box.mu.Lock()
	start := !box.exiting && box.latest != "" && ixVersionNewer(box.latest, version.AppVersion) && box.latest != box.declined &&
		(box.gaveUp.IsZero() || now.Sub(box.gaveUp) >= ixAutoRetryAfter)
	if start {
		box.autoSince, box.autoNote = now, ""
	}
	latest := box.latest
	box.mu.Unlock()
	if !start {
		return
	}
	a.ixLog("automatic update to %s: waiting for the downloads in progress (large files stop at their next checkpoint)", latest)
	a.monitor.SetDraining(true)
	a.ixUpdateWatch()
}

// ixAutoDrainCheck looks after an automatic drain under way, and says whether
// there is one.
func (a *App) ixAutoDrainCheck(now time.Time) bool {
	box := a.ixUpdates()
	box.mu.Lock()
	defer box.mu.Unlock()
	if box.autoSince.IsZero() {
		return false
	}
	switch {
	case !a.monitor.Draining():
		box.autoSince = time.Time{} // ended elsewhere
		return false
	case !box.exiting && now.Sub(box.autoSince) >= ixAutoDrainLimit:
		a.monitor.SetDraining(false)
		box.autoSince, box.gaveUp = time.Time{}, now
		box.autoNote = "The automatic update waited " + ixAutoDrainLimit.String() + " for the downloads in progress and went back to downloading; it tries again later."
		a.ixLog("automatic update: downloads did not finish in %s; back to downloading, trying again later", ixAutoDrainLimit)
	}
	return true
}

// ixUpdateExit ends the server as Ctrl+C does.
func (a *App) ixUpdateExit() {
	box := a.ixUpdates()
	box.mu.Lock()
	defer box.mu.Unlock()
	if box.exiting {
		return
	}
	box.exiting = true
	a.ixLog("restarting for an update")
	if a.requestExit != nil {
		a.requestExit()
	}
}

// ixUpdateDiag is the report's line.
func (a *App) ixUpdateDiag(ctx context.Context) string {
	info := a.ixUpdateNow(ctx)
	latest := info.Latest
	if latest == "" {
		latest = "not known"
	}
	if info.CheckErr != "" {
		latest += " (last check failed)"
	}
	kept, keptBytes := 0, int64(0)
	if a.library != nil {
		kept, keptBytes = a.library.PartsKept(ctx)
	}
	return fmt.Sprintf("update: running %s · latest release %s · waiting for downloads to finish %v (%d files, %d MB left%s) · install automatically %v · launcher restarts the app %v · partial downloads kept for resuming %d (%d MB)", info.Current, latest, info.Draining, info.Files, info.BytesLeft>>20, map[bool]string{true: ", started automatically"}[info.AutoNow], info.Auto, info.Restarts, kept, keptBytes>>20)
}
