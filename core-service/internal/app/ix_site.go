package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Assembling the index: the dashboard view (/ix/view/…) and the static copy.

type ixSiteData struct {
	render *ixRender
	groups []*ixGroup // layouts (no text)
	byID   map[string]*ixGroup
	byDir  map[string]*ixGroup
	cats   []*ixCategory
	walks  map[string]int64
}

func (a *App) ixPeers(ctx context.Context) map[string]ixPeerName {
	out := map[string]ixPeerName{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT peer_id, kind, name, username FROM ix_peers`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var p ixPeerName
		if rows.Scan(&id, &p.Kind, &p.Name, &p.Username) == nil {
			out[id] = p
		}
	}
	return out
}

func (a *App) ixEdits(ctx context.Context) func(string, []int) map[int][]ixEdit {
	return func(groupID string, ids []int) map[int][]ixEdit {
		out := map[int][]ixEdit{}
		for start := 0; start < len(ids); start += 400 {
			chunk := ids[start:min(len(ids), start+400)]
			marks := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
			args := []any{groupID}
			for _, id := range chunk {
				args = append(args, id)
			}
			rows, err := a.db.Reader.QueryContext(ctx, `SELECT message_id, edit_date, text FROM ix_message_edits WHERE group_id=? AND message_id IN (`+marks+`) ORDER BY message_id, recorded_at`, args...)
			if err != nil {
				continue
			}
			for rows.Next() {
				var id int
				var e ixEdit
				if rows.Scan(&id, &e.EditDate, &e.Text) == nil {
					out[id] = append(out[id], e)
				}
			}
			rows.Close()
		}
		return out
	}
}

func (a *App) ixSite(ctx context.Context, site ixSite, dashboard bool) (*ixSiteData, error) {
	metas, err := a.ixGroups(ctx)
	if err != nil {
		return nil, err
	}
	shared, err := a.ixShared(ctx)
	if err != nil {
		return nil, err
	}
	near := a.ixNearMap(ctx)
	d := &ixSiteData{byID: map[string]*ixGroup{}, byDir: map[string]*ixGroup{}, walks: a.ixLatestWalks(ctx)}
	for _, meta := range metas {
		g, err := a.ixLayoutOf(ctx, meta)
		if err != nil {
			return nil, err
		}
		ixCountShared(g, shared)
		ixCountNear(g, near)
		d.groups = append(d.groups, g)
		d.byID[g.ID] = g
		if _, taken := d.byDir[g.Dir]; !taken {
			d.byDir[g.Dir] = g
		}
	}
	d.cats = ixCategories(d.groups, ixNearRoots(near))
	d.render = &ixRender{
		site:      site,
		shared:    shared,
		layoutOf:  func(id string) *ixGroup { return d.byID[id] },
		peers:     a.ixPeers(ctx),
		edits:     a.ixEdits(ctx),
		generated: time.Now(),
		near:      near,
		nearAt:    a.ixNearPlaces(ctx),
		dashboard: dashboard,
	}
	return d, nil
}

// ixFullGroup loads one group with text for its pages.
func (a *App) ixFullGroup(ctx context.Context, d *ixSiteData, layout *ixGroup) (*ixGroup, error) {
	g, err := a.ixLoadGroup(ctx, layout.ixGroupMeta, true)
	if err != nil {
		return nil, err
	}
	ixCountShared(g, d.render.shared)
	ixCountNear(g, d.render.near)
	return g, nil
}

// handleIxView serves the index inside the dashboard.
func (a *App) handleIxView(w http.ResponseWriter, r *http.Request) {
	rel := strings.Trim(r.PathValue("path"), "/")
	if rel == "" {
		rel = "index.html"
	}
	if rel == "style.css" {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write([]byte(ixCSS))
		return
	}
	d, err := a.ixSite(r.Context(), ixSite{}, true)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "index: "+err.Error())
		return
	}
	body, ctype, err := a.ixViewFile(r.Context(), d, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeNotFound(w, r)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "index: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-store")
	if strings.HasSuffix(rel, ".csv") || strings.HasSuffix(rel, ".txt") {
		name := strings.ReplaceAll(rel, "/", "_")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
	_, _ = w.Write(body)
}

func ixContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".csv"):
		return "text/csv; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	}
	return "text/plain; charset=utf-8"
}

func (a *App) ixViewFile(ctx context.Context, d *ixSiteData, rel string) ([]byte, string, error) {
	if rel == "index.html" {
		return d.render.renderRoot(d.cats), ixContentType(rel), nil
	}
	if rel == "near_duplicates.csv" {
		clusters, err := a.ixDupClusters(ctx)
		if err != nil {
			return nil, "", err
		}
		return ixDupCSV(clusters), ixContentType(rel), nil
	}
	dir, file, ok := strings.Cut(rel, "/")
	if !ok || file == "" || strings.Contains(file, "/") {
		return nil, "", os.ErrNotExist
	}
	if strings.HasPrefix(dir, "c_") {
		for _, c := range d.cats {
			if "c_"+c.Slug == dir {
				pages := d.render.renderCategory(c)
				if body, ok := pages[file]; ok {
					return body, ixContentType(file), nil
				}
			}
		}
		return nil, "", os.ErrNotExist
	}
	layout := d.byDir[dir]
	if layout == nil {
		return nil, "", os.ErrNotExist
	}
	g, err := a.ixFullGroup(ctx, d, layout)
	if err != nil {
		return nil, "", err
	}
	if file == "index.html" {
		return d.render.renderGroupIndex(g, d.walks[g.ID]), ixContentType(file), nil
	}
	if strings.HasSuffix(file, ".csv") || file == "sha256.txt" {
		if body, ok := d.render.groupFiles(g)[file]; ok {
			return body, ixContentType(file), nil
		}
		return nil, "", os.ErrNotExist
	}
	for i, pg := range g.Pages {
		if pg.File == file {
			return d.render.renderGroupPage(g, i), ixContentType(file), nil
		}
	}
	return nil, "", os.ErrNotExist
}

// ---------- the static copy ----------

type ixExportStatus struct {
	Running    bool   `json:"running"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
	Progress   string `json:"progress,omitempty"`
	Error      string `json:"error,omitempty"`
	Dir        string `json:"dir,omitempty"`
	Files      int    `json:"files,omitempty"`
	LastDay    string `json:"lastDay,omitempty"`
}

var ixExports sync.Map // *App -> *ixExportBox

type ixExportBox struct {
	mu sync.Mutex
	st ixExportStatus
}

func (a *App) ixExportBox() *ixExportBox {
	v, _ := ixExports.LoadOrStore(a, &ixExportBox{})
	return v.(*ixExportBox)
}

func (a *App) ixExportStatus() ixExportStatus {
	box := a.ixExportBox()
	box.mu.Lock()
	defer box.mu.Unlock()
	return box.st
}

// ixExportDir is where the static copy goes: the configured folder, or a
// folder next to the downloads folder (X:\Telegram → X:\Telegram index).
// Never inside the downloads folder: the downloader's "Re-index" would take
// the pages for downloads.
func (a *App) ixExportDir(s ixSettings) (string, error) {
	downloads, err := filepath.Abs(a.downloadsDir)
	if err != nil {
		return "", err
	}
	if s.ExportDir != "" {
		dir, err := filepath.Abs(s.ExportDir)
		if err != nil {
			return "", err
		}
		if rel, err := filepath.Rel(downloads, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", errors.New("the index folder must not be inside the downloads folder")
		}
		return dir, nil
	}
	parent := filepath.Dir(downloads)
	base := filepath.Base(downloads)
	if parent == downloads || base == "" || base == "." || base == string(filepath.Separator) {
		return "", errors.New("the downloads folder is the top of a drive or share; choose a folder for the index in the settings")
	}
	return filepath.Join(parent, base+" index"), nil
}

// ixFilesBase is the link from the index folder to the downloads folder.
func ixFilesBase(indexDir, downloads string) string {
	if rel, err := filepath.Rel(indexDir, downloads); err == nil {
		parts := strings.Split(filepath.ToSlash(rel), "/")
		for i, p := range parts {
			if p != ".." && p != "." {
				parts[i] = strings.ReplaceAll(ixEncodePath(p), "%2F", "/")
			}
		}
		return strings.Join(parts, "/")
	}
	slash := filepath.ToSlash(downloads)
	if strings.HasPrefix(slash, "//") {
		return "file:" + ixEncodePath(slash)
	}
	return "file:///" + ixEncodePath(strings.TrimPrefix(slash, "/"))
}

// ixExport writes the whole index into "<dir>.new" and swaps it in.
func (a *App) ixExport(ctx context.Context) error {
	box := a.ixExportBox()
	box.mu.Lock()
	if box.st.Running {
		box.mu.Unlock()
		return errors.New("the index is already being written")
	}
	box.st = ixExportStatus{Running: true, StartedAt: time.Now().UnixMilli(), LastDay: box.st.LastDay}
	box.mu.Unlock()
	progress := func(msg string) {
		box.mu.Lock()
		box.st.Progress = msg
		box.mu.Unlock()
	}
	var files int
	dir, err := a.ixExportTo(ctx, progress, &files)
	box.mu.Lock()
	box.st.Running, box.st.FinishedAt, box.st.Dir, box.st.Files, box.st.Progress = false, time.Now().UnixMilli(), dir, files, ""
	box.st.Error = ""
	if err != nil {
		box.st.Error = err.Error()
	}
	box.mu.Unlock()
	return err
}

func (a *App) ixExportTo(ctx context.Context, progress func(string), count *int) (string, error) {
	s := a.ixLoadSettings(ctx)
	target, err := a.ixExportDir(s)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(a.downloadsDir); err != nil {
		return target, fmt.Errorf("the downloads folder is not reachable: %w", err)
	}
	downloads, _ := filepath.Abs(a.downloadsDir)
	fresh, old := target+".new", target+".old"
	if err = os.RemoveAll(fresh); err != nil {
		return target, err
	}
	write := func(name string, data []byte) error {
		p := filepath.Join(fresh, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		*count++
		return os.WriteFile(p, data, 0o644)
	}
	progress("Reading the archive…")
	d, err := a.ixSite(ctx, ixSite{static: true, filesBase: ixFilesBase(target, downloads)}, false)
	if err != nil {
		return target, err
	}
	if err = write("style.css", []byte(ixCSS)); err != nil {
		return target, err
	}
	if err = write("index.html", d.render.renderRoot(d.cats)); err != nil {
		return target, err
	}
	if clusters, err := a.ixDupClusters(ctx); err == nil && len(clusters) > 0 {
		if err = write("near_duplicates.csv", ixDupCSV(clusters)); err != nil {
			return target, err
		}
	}
	for _, c := range d.cats {
		for name, body := range d.render.renderCategory(c) {
			if err = write("c_"+c.Slug+"/"+name, body); err != nil {
				return target, err
			}
		}
	}
	for i, layout := range d.groups {
		if ctx.Err() != nil {
			return target, ctx.Err()
		}
		if d.byDir[layout.Dir] != layout {
			continue // two chats with the same folder name: the first wins
		}
		progress(fmt.Sprintf("Group %d of %d: %s", i+1, len(d.groups), layout.Name))
		g, err := a.ixFullGroup(ctx, d, layout)
		if err != nil {
			return target, err
		}
		if err = write(g.Dir+"/index.html", d.render.renderGroupIndex(g, d.walks[g.ID])); err != nil {
			return target, err
		}
		for name, body := range d.render.groupFiles(g) {
			if err = write(g.Dir+"/"+name, body); err != nil {
				return target, err
			}
		}
		for i := range g.Pages {
			if err = write(g.Dir+"/"+g.Pages[i].File, d.render.renderGroupPage(g, i)); err != nil {
				return target, err
			}
		}
	}
	if err = a.ixWriteData(ctx, fresh, target, downloads, progress, count); err != nil {
		return target, fmt.Errorf("data files: %w", err)
	}
	progress("Swapping in the new index…")
	if err = os.RemoveAll(old); err != nil {
		return target, err
	}
	if _, err = os.Stat(target); err == nil {
		if err = os.Rename(target, old); err != nil {
			// In use (a window open on it): copy over it instead.
			if err = ixCopyTree(fresh, target); err != nil {
				return target, err
			}
			return target, os.RemoveAll(fresh)
		}
	}
	if err = os.Rename(fresh, target); err != nil {
		if _, statErr := os.Stat(old); statErr == nil {
			_ = os.Rename(old, target)
		}
		return target, err
	}
	_ = os.RemoveAll(old)
	return target, nil
}

func ixCopyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, data, 0o644)
	})
}

// ixRunExports writes the static index once a day at the configured time.
func (a *App) ixRunExports(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		s := a.ixLoadSettings(ctx)
		now := time.Now()
		day := now.Format("2006-01-02")
		box := a.ixExportBox()
		box.mu.Lock()
		due := s.ExportEnabled && now.Format("15:04") == s.ExportTime && box.st.LastDay != day && !box.st.Running
		if due {
			box.st.LastDay = day
		}
		box.mu.Unlock()
		if due {
			if err := a.ixExport(ctx); err != nil && ctx.Err() == nil {
				a.ixLog("nightly index: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func ixItoa(n int64) string { return strconv.FormatInt(n, 10) }
