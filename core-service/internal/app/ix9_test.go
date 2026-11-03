package app

import (
	"encoding/json"
	"testing"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

func TestIxDownloadPacingIsOnByDefaultAndCanBeSwitchedOff(t *testing.T) {
	s := ixDefaultSettings()
	if !s.DownloadPacing {
		t.Fatal("pacing should be on by default")
	}
	// Settings saved before ix.9 have no such key: pacing stays on.
	var old ixSettings = ixDefaultSettings()
	if err := json.Unmarshal([]byte(`{"exportEnabled":true}`), &old); err != nil || !old.DownloadPacing {
		t.Fatalf("old settings: %v %v", err, old.DownloadPacing)
	}
	off, err := ixParseSettings(map[string]any{"downloadPacing": false}, s)
	if err != nil || off.DownloadPacing {
		t.Fatalf("switching off: %v %v", err, off.DownloadPacing)
	}
	telegram.SetPacing(off.DownloadPacing)
	defer telegram.SetPacing(true)
	a := &App{}
	if p := a.ixPaceNow(); !p.Off {
		t.Fatal("the downloads card should say pacing is off")
	}
}
