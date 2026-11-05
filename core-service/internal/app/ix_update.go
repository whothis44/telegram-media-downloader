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

const (
	ixUpdateCheckEvery = time.Hour
	ixUpdateWatchEvery = 2 * time.Second
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
}

type ixUpdateBox struct {
	mu       sync.Mutex
	latest   string
	checkErr string
	checked  time.Time
	checking bool
	watching bool
	exiting  bool
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
	return fmt.Sprintf("update: running %s · latest release %s · waiting for downloads to finish %v (%d files, %d MB left) · launcher restarts the app %v · partial downloads kept for resuming %d (%d MB)", info.Current, latest, info.Draining, info.Files, info.BytesLeft>>20, info.Restarts, kept, keptBytes>>20)
}
