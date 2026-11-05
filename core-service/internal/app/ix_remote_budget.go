package app

import (
	"sync"
	"time"
)

// Optional remote previews/samples yield when downloads have filled their
// request windows, or Telegram has recently throttled them. Local recognition
// of already-kept previews can continue. This never releases held duplicates.
var ixRemoteChecksBusy = func(a *App) bool {
	p := a.ixPaceNow()
	if p.Premium10m > 0 || p.Floods10m > 0 {
		return true
	}
	for _, dc := range p.DCs {
		if dc.Waiting > 0 || dc.GateWaiting > 0 {
			return true
		}
	}
	return false
}

// On an account Telegram keeps speed-limited, "busy" is true for days. Yielding
// every time then means a check that could avoid a gigabyte never runs: the
// checks most worth their bytes starve exactly when bytes are scarcest. So
// under pressure each kind of check gets a bounded turn: a share of wall time
// in every window, started by the first check that asks for it. A check already
// under way may overrun its turn by that one check. Telegram's own FLOOD_WAIT
// is different: it is a penalty, and nothing optional runs for ten minutes.
//
// Samples (ix_copies_sample.go) cost megabytes and can spare a whole big file.
// Previews cost kilobytes each and feed the likeness checks, so they get a
// smaller turn.
var (
	ixRemoteBudgetOn     = true
	ixRemoteBudgetWindow = 10 * time.Minute
	ixRemoteBudgetSample = time.Minute
	ixRemoteBudgetView   = 30 * time.Second
	ixRemoteBudgetNow    = time.Now
)

// ixFloodedLately: Telegram told an account to wait (FLOOD_WAIT, not the speed
// limit) in the last ten minutes. Replaceable in tests.
var ixFloodedLately = func(a *App) bool { return a.ixPaceNow().Floods10m > 0 }

type ixRemoteTurns struct {
	mu     sync.Mutex
	starts map[string]time.Time
}

var ixRemoteTurnState sync.Map // *App -> *ixRemoteTurns

// ixRemoteTurn reports whether this kind of check may run now, in spite of the
// pressure.
func (a *App) ixRemoteTurn(kind string, share time.Duration) bool {
	if !ixRemoteBudgetOn {
		return false
	}
	v, _ := ixRemoteTurnState.LoadOrStore(a, &ixRemoteTurns{starts: map[string]time.Time{}})
	t := v.(*ixRemoteTurns)
	now := ixRemoteBudgetNow()
	t.mu.Lock()
	defer t.mu.Unlock()
	start, ok := t.starts[kind]
	if !ok || now.Sub(start) >= ixRemoteBudgetWindow {
		t.starts[kind], start = now, now
	}
	return now.Sub(start) < share
}

// ixSampleChecksBusy: samples of big videos have to wait.
func ixSampleChecksBusy(a *App) bool {
	return ixRemoteChecksBusy(a) && (ixFloodedLately(a) || !a.ixRemoteTurn("sample", ixRemoteBudgetSample))
}

// ixPreviewChecksBusy: previews of files not downloaded have to wait.
func ixPreviewChecksBusy(a *App) bool {
	return ixRemoteChecksBusy(a) && (ixFloodedLately(a) || !a.ixRemoteTurn("preview", ixRemoteBudgetView))
}
