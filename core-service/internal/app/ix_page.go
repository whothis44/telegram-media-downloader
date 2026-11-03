package app

import (
	_ "embed"
	"net/http"
)

//go:embed ixassets/ix.html
var ixPageHTML []byte

// handleIxPage serves the archive tools page (rescan queue, categories,
// search, settings). It is one self-contained file, never cached, so an
// update is visible on the next visit.
func (a *App) handleIxPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(ixPageHTML)
}
