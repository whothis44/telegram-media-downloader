package app

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
