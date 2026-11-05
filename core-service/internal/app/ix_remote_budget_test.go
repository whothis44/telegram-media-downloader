package app

import (
	"context"
	"errors"
	"testing"
)

func TestIxActiveTelegramWaitIsNotAnUnexplainedStall(t *testing.T) {
	s := ixDlState{EngineRunning: true, Downloading: 2, QuietMin: 120, TelegramWaiting: 2}
	ixDlDecide(&s)
	if s.Verdict != ixDlSpeedLimited {
		t.Fatalf("active wait: %s", s.Verdict)
	}
	s.TelegramWaiting = 0
	ixDlDecide(&s)
	if s.Verdict != ixDlStalled {
		t.Fatalf("unexplained stall hidden: %s", s.Verdict)
	}
}

func TestIxRemoteChecksDeferWithoutRecordingFailure(t *testing.T) {
	a, ctx := ixTestApp(t)
	busy := true
	oldBusy, oldFake, oldReader := ixRemoteChecksBusy, ixSampleFake, ixOpenCopyReader
	ixRemoteChecksBusy = func(*App) bool { return busy }
	ixSampleFake = true
	opens := 0
	ixOpenCopyReader = func(*App, context.Context, string) (ixCopyReader, error) { opens++; return &ixFakeReader{}, nil }
	t.Cleanup(func() { ixRemoteChecksBusy, ixSampleFake, ixOpenCopyReader = oldBusy, oldFake, oldReader })
	s := a.ixLoadSettings(ctx)
	s.HoldLikely = true
	s.PreviewsAll = "on"
	if err := a.ixSaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	id := ixTestQueue(t, a, ixTestVideo(980, 980, 300<<20, 600, 1280, 720, nil, 1700000000))
	if err := a.ixSampleRound(ctx, false); err != nil {
		t.Fatal(err)
	}
	a.ixPreviewAllRound(ctx)
	if !errors.Is(a.ixPreviewAllSignalled(), errIxWaitSignal) {
		t.Fatal("preview requests did not yield")
	}
	var checks int
	if err := a.db.Reader.QueryRow(`SELECT count(*) FROM ix_copy_sample WHERE work_id=?`, id).Scan(&checks); err != nil {
		t.Fatal(err)
	}
	if opens != 0 || checks != 0 {
		t.Fatalf("deferred work opened %d readers and recorded %d failures", opens, checks)
	}
	busy = false
	if err := a.ixSampleRound(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Reader.QueryRow(`SELECT count(*) FROM ix_copy_sample WHERE work_id=?`, id).Scan(&checks); err != nil {
		t.Fatal(err)
	}
	if opens != 1 || checks != 1 {
		t.Fatalf("work did not resume: readers=%d checks=%d", opens, checks)
	}
}
