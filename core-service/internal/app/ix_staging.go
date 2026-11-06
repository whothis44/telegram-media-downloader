package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/diskfree"
	"github.com/botnick/telegram-media-downloader/core-service/internal/download"
	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/botnick/telegram-media-downloader/core-service/internal/ws"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// The local staging folder (download/staging.go) as the app uses it: the
// setting, the hand-off when a staged file reaches the library, and what
// the dashboard and the diagnostics report show.

// ixWireStaging connects the library to the staging setting.
func (a *App) ixWireStaging() {
	a.library.Staging = func(ctx context.Context) download.Staging {
		s := a.ixLoadSettings(ctx)
		return download.Staging{Dir: s.StageDir, Cap: int64(s.StageCapGB) << 30}
	}
	a.library.StageHold = func(ctx context.Context) (func(), error) {
		a.mediaMu.RLock()
		if err := a.mediaWritable(ctx); err != nil {
			a.mediaMu.RUnlock()
			return nil, err
		}
		return a.mediaMu.RUnlock, nil
	}
	a.library.OnStaged = a.ixStagedArrived
	a.library.StageLost = a.ixStagedLost
}

// ixRequeue is what a staged download keeps to put its message back in the
// queue should its staged file be lost before it reaches the library: by
// then its queue item counts as done and no longer holds the message.
type ixRequeue struct {
	Account   string `json:"a"`
	Origin    string `json:"o"`
	Group     string `json:"g"`
	GroupName string `json:"n,omitempty"`
	Body      []byte `json:"m"` // the queued Telegram message (not a lower quality picked for it)
}

type ixRequeueKey struct{}

// ixWithRequeue notes the queue item being downloaded, for ixStageRequeue.
func ixWithRequeue(ctx context.Context, work *engine.Work, message *tg.Message) context.Context {
	if work == nil || message == nil {
		return ctx
	}
	var buf bin.Buffer
	if err := message.Encode(&buf); err != nil {
		return ctx
	}
	return context.WithValue(ctx, ixRequeueKey{}, ixRequeue{Account: work.AccountID, Origin: work.Origin, Group: work.GroupID, GroupName: work.GroupName, Body: buf.Buf})
}

// ixStageRequeue is item.Requeue for a download that may be staged.
func (a *App) ixStageRequeue(ctx context.Context) []byte {
	rq, ok := ctx.Value(ixRequeueKey{}).(ixRequeue)
	if !ok || a.ixLoadSettings(ctx).StageDir == "" {
		return nil
	}
	out, err := json.Marshal(rq)
	if err != nil {
		return nil
	}
	return out
}

// ixStagedLost puts a message back in the download queue whose staged file
// disappeared before it reached the library (download.Library.StageLost).
func (a *App) ixStagedLost(ctx context.Context, item download.Item) error {
	var rq ixRequeue
	if err := json.Unmarshal(item.Requeue, &rq); err != nil {
		return fmt.Errorf("read the queue item: %w", err)
	}
	var message tg.Message
	if err := message.Decode(&bin.Buffer{Buf: rq.Body}); err != nil {
		return fmt.Errorf("read the queued message: %w", err)
	}
	if a.monitor == nil {
		return errors.New("the download queue is not available")
	}
	queued, err := a.monitor.RequeueLost(ctx, rq.Account, engine.Target{ID: rq.Group, Name: rq.GroupName}, &message, rq.Origin)
	if err != nil {
		return err
	}
	if queued {
		a.ixLog("staging: the staged copy of message %d in %s was missing from the staging folder; it is queued again", item.MessageID, item.GroupID)
	} else {
		a.ixLog("staging: the staged copy of message %d in %s was missing from the staging folder; its queue item was skipped or its group purged since, so it is not queued again", item.MessageID, item.GroupID)
	}
	return nil
}

func (a *App) ixRunStaging(ctx context.Context) { a.library.RunStaging(ctx) }

// ixWaitStageRoom waits for room in the staging folder before a download;
// the time waited does not count against the download's time limit.
func (a *App) ixWaitStageRoom(ctx context.Context, size int64, transport telegram.MediaDownloader) error {
	if p, ok := transport.(interface{ PauseBudget() func() }); ok {
		resume := p.PauseBudget()
		defer resume()
	}
	return a.library.WaitStageRoom(ctx, size)
}

// ixStagedArrived finishes what ingestTelegram leaves for a staged file once
// it is in the library: the dashboard event and the background work.
func (a *App) ixStagedArrived(ctx context.Context, item download.Item, record download.Record) {
	key := item.GroupID + "_" + strconv.FormatInt(item.MessageID, 10)
	payload := map[string]any{"key": key, "groupId": item.GroupID, "groupName": item.GroupName, "messageId": item.MessageID, "fileName": item.Name, "filePath": record.Path, "fileSize": item.Identity.Size, "mediaType": item.Type, "deduped": record.Reused, "dedupedHow": record.Dedup, "addedAt": nil, "accountId": nil, "accountName": nil}
	if record.Reused {
		a.ixNoteLink(ctx, item, record) // the file's place in the library (it may have been renamed on arrival)
	}
	if record.Skipped {
		payload["skipped"] = true
		a.hub.Broadcast(ws.Event{Type: "download_complete", Payload: payload})
		return
	}
	a.hub.Broadcast(ws.Event{Type: "download_complete", Payload: payload})
	if item.Type == "video" {
		a.enqueueSeekbar(record.ID, false)
	}
	a.queueNSFW(record.ID, key)
	a.faces.enqueue(record.ID, false)
	if a.backups != nil {
		a.backups.Wake()
	}
	a.ixLowqArrived(ctx, item.GroupID, item.MessageID)
	if err := a.drainFileCleanup(ctx); err != nil {
		a.ixLog("staging: media cleanup remains pending: %v", err)
	}
}

// ixParseTransferSettings reads the lower-quality and staging settings.
func ixParseTransferSettings(body map[string]any, s *ixSettings) error {
	for key, dst := range map[string]*bool{"lowqFirst": &s.LowqFirst, "lowqKeep": &s.LowqKeep} {
		if v, ok := body[key].(bool); ok {
			*dst = v
		}
	}
	if v, ok := body["stageDir"].(string); ok {
		s.StageDir = strings.TrimSpace(v)
	}
	switch v := body["stageCapGb"].(type) {
	case float64:
		s.StageCapGB = int(v)
	case string:
		if strings.TrimSpace(v) == "" {
			s.StageCapGB = 20
		} else if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			s.StageCapGB = n
		} else {
			return errors.New("the staging space must be a number of GB")
		}
	}
	if s.StageCapGB < 1 || s.StageCapGB > 100000 {
		return errors.New("the staging space must be between 1 and 100000 GB")
	}
	return ixParseBigSettings(body, s)
}

// ixCheckStageDir checks a new staging folder: a full path to a folder that
// exists, that the app can write to, and that is not in the downloads folder.
func (a *App) ixCheckStageDir(dir string) error {
	if dir == "" {
		return nil
	}
	if !filepath.IsAbs(dir) {
		return errors.New("the staging folder must be a full path, such as D:\\Telegram staging")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return errors.New("the staging folder does not exist: create it first")
	}
	if !info.IsDir() {
		return errors.New("the staging folder is not a folder")
	}
	if downloads, err := filepath.Abs(a.downloadsDir); err == nil {
		if rel, err := filepath.Rel(downloads, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("the staging folder must not be the downloads folder or inside it")
		}
	}
	f, err := os.CreateTemp(dir, ".tgdl-probe-*")
	if err != nil {
		return fmt.Errorf("the app cannot write to the staging folder: %v", err)
	}
	name := f.Name()
	_, werr := f.Write([]byte("staging check"))
	cerr := f.Close()
	_ = os.Remove(name)
	if werr != nil || cerr != nil {
		return fmt.Errorf("the app cannot write to the staging folder: %v", errors.Join(werr, cerr))
	}
	return nil
}

type ixStagingStatus struct {
	download.StagingStatus
	On      bool   `json:"on"`
	CapGB   int    `json:"capGb"`
	Suggest string `json:"suggest"` // a folder to suggest: the app's data folder
	// This computer's fixed drives, for the folder picker (Windows; empty elsewhere).
	Drives []diskfree.Drive `json:"drives,omitempty"`
}

func (a *App) ixStagingStatus(ctx context.Context) ixStagingStatus {
	s := a.ixLoadSettings(ctx)
	st := ixStagingStatus{On: s.StageDir != "", CapGB: s.StageCapGB, Suggest: a.dataDir}
	if abs, err := filepath.Abs(a.dataDir); err == nil {
		st.Suggest = abs
	}
	if a.library != nil {
		st.StagingStatus = a.library.StagingStatus(ctx)
	}
	st.Drives = diskfree.Local()
	return st
}

// ixTransferDiag is the diagnostics report's lines on both.
func (a *App) ixTransferDiag(ctx context.Context, clean func(string) string) []string {
	s := a.ixLoadSettings(ctx)
	on := map[bool]string{true: "on", false: "off"}
	gb := func(b int64) string { return strconv.FormatFloat(float64(b)/1e9, 'f', 1, 64) + " GB" }
	lq := a.ixLowqStatus(ctx)
	out := []string{fmt.Sprintf("big videos: lower quality first %s (keep after upgrade %s) · %d big videos saved in a lower quality first (%s instead of %s); %d upgraded (the lower qualities of those cost %s extra) · %d originals still to come",
		on[s.LowqFirst], on[s.LowqKeep], lq.Saved, gb(lq.SavedBytes), gb(lq.OrigBytes), lq.Upgraded, gb(lq.ExtraBytes), lq.Waiting)}
	st := a.ixStagingStatus(ctx)
	line := fmt.Sprintf("local staging %s (up to %d GB) · %d files / %s waiting to be copied to the library", on[st.On], st.CapGB, st.Waiting, gb(st.Bytes))
	if st.Failing > 0 {
		line += fmt.Sprintf(" · %d failing, since %s", st.Failing, ixAgo(st.FailingSince, time.Now()))
	}
	if st.Stuck > 0 {
		line += fmt.Sprintf(" · %d (%s) need attention (their local copies are kept; they do not count against the space allowed)", st.Stuck, gb(st.StuckBytes))
	}
	if st.Full {
		line += fmt.Sprintf(" · downloads waiting for room since %s", ixAgo(st.FullSince, time.Now()))
	}
	out = append(out, line)
	for _, e := range st.Errors {
		out = append(out, "  "+clean(e))
	}
	return out
}

// ixStageFullAfter: downloads waiting this long for room in the staging
// folder raise an alert (a short wait while a big file is copied is normal).
const (
	ixStageFullAfter    = 10 * time.Minute
	ixStageFailingAfter = time.Hour
)

// ixStagingAlerts are the staging folder's alerts (ix_alerts.go).
func ixStagingAlerts(st ixStagingStatus, now time.Time) []ixAlert {
	var out []ixAlert
	gb := func(b int64) string { return strconv.FormatFloat(float64(b)/1e9, 'f', 1, 64) + " GB" }
	if st.Full && st.FullSince > 0 && now.Sub(time.UnixMilli(st.FullSince)) >= ixStageFullAfter {
		out = append(out, ixAlert{Key: "staging:full", Title: "Downloads are waiting for room in the staging folder",
			Message: fmt.Sprintf("Files waiting to be copied to the downloads folder take the space allowed for staging (%d GB): %d file(s), %s. Downloads have waited since %s. Usually the downloads folder (the NAS) cannot be reached: check it. Raising the staging space in Settings, or freeing space on the staging disk, lets downloads go on.",
				st.CapGB, st.Waiting, gb(st.Bytes), time.UnixMilli(st.FullSince).Format("2006-01-02 15:04"))})
	}
	if st.Failing > 0 && st.FailingSince > 0 && now.Sub(time.UnixMilli(st.FailingSince)) >= ixStageFailingAfter {
		out = append(out, ixAlert{Key: "staging:failing", Title: "Copies to the downloads folder are failing",
			Message: fmt.Sprintf("%d staged file(s) could not be copied to the downloads folder since %s; they are retried and the local copies are kept. Check that the downloads folder (the NAS) can be reached.",
				st.Failing, time.UnixMilli(st.FailingSince).Format("2006-01-02 15:04"))})
	}
	if st.Stuck > 0 {
		out = append(out, ixAlert{Key: "staging:stuck", Title: "Staged files need a look",
			Message: fmt.Sprintf("%d file(s) in the staging folder (%s) could not be copied to the downloads folder and need a person; their local copies are kept (see the diagnostics report).", st.Stuck, gb(st.StuckBytes))})
	}
	return out
}
