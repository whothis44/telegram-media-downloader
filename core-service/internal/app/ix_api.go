package app

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/botnick/telegram-media-downloader/core-service/internal/version"
)

func registerIxRoutes(mux *http.ServeMux, a *App) {
	mux.Handle("GET /ix", a.requireAdmin(http.HandlerFunc(a.handleIxPage)))
	mux.Handle("GET /ix/view/{path...}", a.requireAdmin(http.HandlerFunc(a.handleIxView)))
	mux.Handle("GET /api/ix/status", a.requireAdmin(http.HandlerFunc(a.handleIxStatus)))
	mux.Handle("GET /api/ix/search", a.requireAdmin(http.HandlerFunc(a.handleIxSearch)))
	mux.Handle("POST /api/ix/queue/add", a.requireAdmin(http.HandlerFunc(a.handleIxQueueAdd)))
	mux.Handle("POST /api/ix/queue/{action}", a.requireAdmin(http.HandlerFunc(a.handleIxQueueAction)))
	mux.Handle("POST /api/ix/settings", a.requireAdmin(http.HandlerFunc(a.handleIxSettings)))
	mux.Handle("POST /api/ix/export", a.requireAdmin(http.HandlerFunc(a.handleIxExport)))
	mux.Handle("POST /api/ix/downloads/retry", a.requireAdmin(http.HandlerFunc(a.handleIxDownloadsRetry)))
	mux.Handle("POST /api/ix/alerts/test", a.requireAdmin(http.HandlerFunc(a.handleIxAlertTest)))
	mux.Handle("POST /api/ix/groups/finished", a.requireAdmin(http.HandlerFunc(a.handleIxFinished)))
	mux.Handle("GET /api/ix/dups", a.requireAdmin(http.HandlerFunc(a.handleIxDups)))
	mux.Handle("POST /api/ix/dups/retry", a.requireAdmin(http.HandlerFunc(a.handleIxDupsRetry)))
	registerIxDockerRoutes(mux, a)
	registerIxBrowseRoutes(mux, a)
	registerIxBackupRoutes(mux, a)
}

type ixGroupStatus struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Category   string `json:"category"`
	Suggested  string `json:"suggested,omitempty"`
	Configured bool   `json:"configured"`
	Enabled    bool   `json:"enabled"`
	Messages   int64  `json:"messages"`
	Deleted    int64  `json:"deleted"`
	FirstDate  int64  `json:"firstDate,omitempty"`
	LastDate   int64  `json:"lastDate,omitempty"`
	Files      int64  `json:"files"`
	LastWalk   int64  `json:"lastWalk,omitempty"`
	Dir        string `json:"dir"`
	// Completeness: what Telegram last said the chat holds.
	TgCount   int64  `json:"tgCount,omitempty"`
	TgCountAt int64  `json:"tgCountAt,omitempty"`
	Comments  int64  `json:"comments,omitempty"` // comments kept on its posts
	About     string `json:"about,omitempty"`
	Members   int64  `json:"members,omitempty"`
	Linked    string `json:"linked,omitempty"` // its discussion group
	// Marked finished: every rescan shortcut leaves it alone.
	FinishedAt int64 `json:"finishedAt,omitempty"`
	// Message IDs: the newest kept, and how many below it are confirmed gone.
	MaxID    int64  `json:"maxId,omitempty"`
	Gone     int64  `json:"gone,omitempty"`
	Username string `json:"username,omitempty"`
	Invite   string `json:"invite,omitempty"`
	// A category suggested from what the group posts (from the media
	// browser's index, once it has been built).
	ContentSuggest string  `json:"contentSuggest,omitempty"`
	ContentShare   float64 `json:"contentShare,omitempty"`
}

func (a *App) handleIxStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg, err := a.config.Load(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type agg struct{ n, deleted, first, last int64 }
	msgs := map[string]agg{}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, COUNT(*), SUM(deleted_at>0), MIN(NULLIF(date,0)), MAX(date) FROM ix_messages GROUP BY group_id`); err == nil {
		for rows.Next() {
			var id string
			var v agg
			var first, last sql.NullInt64
			if rows.Scan(&id, &v.n, &v.deleted, &first, &last) == nil {
				v.first, v.last = first.Int64, last.Int64
				msgs[id] = v
			}
		}
		rows.Close()
	}
	files := map[string]int64{}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, COUNT(*) FROM downloads WHERE message_id>0 AND file_type IS NOT 'stories' AND (status IS NULL OR status='completed') GROUP BY group_id`); err == nil {
		for rows.Next() {
			var id string
			var n int64
			if rows.Scan(&id, &n) == nil {
				files[id] = n
			}
		}
		rows.Close()
	}
	walks := a.ixLatestWalks(ctx)
	type info struct {
		count, countAt, members, finished int64
		about, linked, username, invite   string
	}
	infos := map[string]info{}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, tg_count, tg_count_at, members, about, linked_group, finished_at, username, invite_link FROM ix_group_info`); err == nil {
		for rows.Next() {
			var id string
			var v info
			if rows.Scan(&id, &v.count, &v.countAt, &v.members, &v.about, &v.linked, &v.finished, &v.username, &v.invite) == nil {
				infos[id] = v
			}
		}
		rows.Close()
	}
	comments := map[string]int64{}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT substr(comment_on, 1, instr(comment_on, ':')-1), COUNT(*) FROM ix_messages WHERE comment_on<>'' GROUP BY 1`); err == nil {
		for rows.Next() {
			var id string
			var n int64
			if rows.Scan(&id, &n) == nil {
				comments[id] = n
			}
		}
		rows.Close()
	}
	cover := a.ixIDCoverage(ctx)
	var suggest map[string]ixCatSuggest
	if box := a.ixModelBox(); box != nil {
		box.mu.Lock()
		if box.model != nil {
			suggest = box.model.Suggest
		}
		box.mu.Unlock()
	}
	withInfo := func(g ixGroupStatus) ixGroupStatus {
		if sg, ok := suggest[g.ID]; ok {
			g.ContentSuggest, g.ContentShare = sg.Category, sg.Share
		}
		v := infos[g.ID]
		g.TgCount, g.TgCountAt, g.Members, g.About, g.Linked, g.Comments = v.count, v.countAt, v.members, v.about, v.linked, comments[g.ID]
		g.FinishedAt = v.finished
		g.Username, g.Invite = v.username, v.invite
		g.MaxID, g.Gone = cover[g.ID].MaxID, cover[g.ID].Gone
		return g
	}
	var groups []ixGroupStatus
	seen := map[string]bool{}
	for _, g := range configuredGroupList(cfg) {
		id := strings.TrimSpace(toString(g["id"]))
		if id == "" || seen[id] || toString(g["peerId"]) != "" {
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(toString(g["name"]))
		if name == "" {
			name = "Chat " + id
		}
		m := msgs[id]
		category := strings.TrimSpace(toString(g["category"]))
		groups = append(groups, withInfo(ixGroupStatus{ID: id, Name: name, Category: category, Suggested: ixSuggestCategory(name), Configured: true, Enabled: g["enabled"] == true,
			Messages: m.n, Deleted: m.deleted, FirstDate: m.first, LastDate: m.last, Files: files[id], LastWalk: walks[id], Dir: ixDirOf(id)}))
	}
	for id, m := range msgs {
		if !seen[id] {
			groups = append(groups, withInfo(ixGroupStatus{ID: id, Name: "Chat " + id, Messages: m.n, Deleted: m.deleted, FirstDate: m.first, LastDate: m.last, Files: files[id], LastWalk: walks[id], Dir: ixDirOf(id)}))
		}
	}
	queue, _ := a.ixLoadQueue(ctx)
	settings := a.ixLoadSettings(ctx)
	exportDir, exportErr := a.ixExportDir(settings)
	var fts int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name='ix_fts'`).Scan(&fts)
	out := map[string]any{
		"version":   version.AppVersion,
		"groups":    groups,
		"queue":     queue,
		"settings":  settings.public(),
		"account":   a.ixAccount(),
		"export":    a.ixExportStatus(),
		"exportDir": exportDir,
		"search":    map[bool]string{true: "full-text", false: "simple"}[fts > 0],
		"hold":      a.ixHold(ctx),
		"dups":      a.ixVdupStatus(),
		"downloads": a.ixDownloads(ctx),
		"freeze":    a.ixFreeze(ctx),
		"held":      a.ixHeldStatus(ctx),
		"fixity":    a.ixFixityStatus(ctx),
		"ml":        a.ixMLStatusNow(ctx),
		"mlStart":   a.ixMLStartStatusNow(),
		"deadline":  a.ixDeadlineEstimate(ctx, false),
		"pictures":  a.ixMediaStatus(),
		"backup":    a.ixBackupOverview(ctx),
		"readonly":  a.ixGoneState(),
	}
	if exportErr != nil {
		out["exportDirError"] = exportErr.Error()
	}
	a.ixVdupCount(ctx)
	out["dups"] = a.ixVdupStatus()
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleIxQueueAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Groups []struct {
			ID   any    `json:"id"`
			Name string `json:"name"`
		} `json:"groups"`
		EnsureFilters bool `json:"ensureFilters"`
		Start         bool `json:"start"`
		Priority      bool `json:"priority"` // before every other group, files included
		// Groups marked finished are skipped unless this is set.
		IncludeFinished bool `json:"includeFinished"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		return
	}
	if len(body.Groups) == 0 || len(body.Groups) > 5000 {
		writeJSONError(w, http.StatusBadRequest, "groups: 1 to 5000 groups")
		return
	}
	ctx := r.Context()
	cfg, err := a.config.Load(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	known := map[string]map[string]any{}
	for _, g := range configuredGroupList(cfg) {
		known[toString(g["id"])] = g
	}
	var added []ixQueueItem
	finished := a.ixFinishedGroups(ctx)
	skipped := 0
	for _, g := range body.Groups {
		id := strings.TrimSpace(toString(g.ID))
		if id == "" {
			continue
		}
		if finished[id] && !body.IncludeFinished {
			skipped++
			continue
		}
		name := ixCleanName(g.Name, id)
		if existing := known[id]; existing != nil && strings.TrimSpace(toString(existing["name"])) != "" {
			name = toString(existing["name"])
		}
		if body.EnsureFilters {
			// Photos, videos and files on, so a rescan also fetches every
			// document; created disabled when the app does not know the chat.
			patch := map[string]any{"filters": map[string]any{"photos": true, "videos": true, "files": true}}
			if known[id] == nil {
				patch["name"] = name
				patch["enabled"] = false
			}
			if code, out := ixCall(ctx, a.handleAPIGroupSave, http.MethodPut, "/api/groups/"+id, patch, map[string]string{"id": id}); code >= 300 {
				writeJSONError(w, http.StatusBadGateway, "could not update "+name+": "+toString(out["error"]))
				return
			}
		}
		added = append(added, ixQueueItem{GroupID: id, Name: name, State: "waiting", Phase: ixPhaseMessages, Priority: body.Priority, AddedAt: time.Now().UnixMilli()})
	}
	a.ixForgetGroups()
	st, err := a.ixQueueEdit(ctx, func(st *ixQueueState) error {
		for _, item := range added {
			if i := ixQueueIndex(st, item.GroupID); i >= 0 {
				switch st.Items[i].State {
				case "running", "waiting":
					// Already queued: keep its progress; it can only gain priority.
					st.Items[i].Priority = st.Items[i].Priority || item.Priority
				default:
					st.Items[i] = item // finished or failed: queue it again
				}
				continue
			}
			st.Items = append(st.Items, item)
		}
		if body.Start {
			st.Running = true
			st.Notice = ""
		}
		return nil
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "queue": st, "skippedFinished": skipped})
}

func (a *App) handleIxQueueAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	var body map[string]any
	if r.ContentLength != 0 {
		if err := decodeBody(w, r, &body); err != nil {
			return
		}
	}
	groupID := strings.TrimSpace(toString(body["groupId"]))
	ctx := r.Context()
	var cancelJob string
	st, err := a.ixQueueEdit(ctx, func(st *ixQueueState) error {
		switch action {
		case "start":
			st.Running = true
			st.Notice = ""
			for i := range st.Items {
				if st.Items[i].State == "waiting" {
					st.Items[i].NextAt = 0
				}
			}
		case "pause":
			st.Running = false
			st.Notice = "Paused. The group being rescanned finishes; no new group starts."
		case "clear-finished":
			kept := st.Items[:0]
			for _, item := range st.Items {
				if item.State != "done" && item.State != "failed" {
					kept = append(kept, item)
				}
			}
			st.Items = kept
		case "clear":
			kept := st.Items[:0]
			for _, item := range st.Items {
				if item.State == "running" {
					kept = append(kept, item)
				}
			}
			st.Items = kept
			st.Running = len(kept) > 0 && st.Running
		case "remove":
			if i := ixQueueIndex(st, groupID); i >= 0 {
				if st.Items[i].State == "running" {
					cancelJob = st.Items[i].JobID
				}
				st.Items = append(st.Items[:i], st.Items[i+1:]...)
			}
		case "retry":
			if i := ixQueueIndex(st, groupID); i >= 0 && st.Items[i].State != "running" {
				// Again from the start: messages first, then missing files.
				st.Items[i].State, st.Items[i].NextAt, st.Items[i].Error = "waiting", 0, ""
				st.Items[i].Phase, st.Items[i].Attempts = ixPhaseMessages, 0
				st.Items[i].ResumeFrom, st.Items[i].Carried, st.Items[i].PassStartedAt = 0, 0, 0
			}
		case "priority":
			if i := ixQueueIndex(st, groupID); i >= 0 {
				on, _ := body["priority"].(bool)
				st.Items[i].Priority = on
			}
		case "attempts":
			st.MaxAttempts = max(0, min(1000, int(number(body["maxAttempts"], 0))))
		default:
			return errBadIxAction
		}
		return nil
	})
	if err == errBadIxAction {
		writeJSONError(w, http.StatusNotFound, "unknown action")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cancelJob != "" {
		ixCall(ctx, a.handleHistoryCancel, http.MethodPost, "/api/history/"+cancelJob+"/cancel", nil, map[string]string{"jobId": cancelJob})
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "queue": st})
}

type ixError string

func (e ixError) Error() string { return string(e) }

const errBadIxAction = ixError("unknown action")

func (a *App) handleIxSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(w, r, &body); err != nil {
		return
	}
	ctx := r.Context()
	old := a.ixLoadSettings(ctx)
	s, err := ixParseSettings(body, old)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err = a.ixExportDir(s); err != nil && s.ExportDir != "" {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err = a.ixSaveSettings(ctx, s); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	telegram.SetPacing(s.DownloadPacing)
	if s.Pace != old.Pace {
		if err = a.ixApplyPace(ctx, s.Pace); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	a.ixWake()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "settings": s.public()})
}

func (a *App) handleIxExport(w http.ResponseWriter, r *http.Request) {
	if a.ixExportStatus().Running {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "The index is already being written"})
		return
	}
	if !a.launchMaintenance(func() {
		if err := a.ixExport(a.ctx); err != nil && a.ctx.Err() == nil {
			a.ixLog("index: %v", err)
		}
	}) {
		writeJSONError(w, http.StatusServiceUnavailable, "Server is shutting down")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"success": true})
}

// ---------- search ----------

type ixHit struct {
	GroupID   string `json:"groupId"`
	Group     string `json:"group"`
	Topic     string `json:"topic,omitempty"`
	MessageID int    `json:"messageId"`
	Date      int    `json:"date"`
	Snippet   string `json:"snippet"`
	Link      string `json:"link"`
	Deleted   bool   `json:"deleted,omitempty"`
}

// ixFTSQuery turns typed words into a safe FTS5 query: every word must
// appear (prefix match), "quoted phrases" stay phrases.
func ixFTSQuery(q string) string {
	var terms []string
	for len(q) > 0 {
		q = strings.TrimLeft(q, " \t\r\n")
		if q == "" {
			break
		}
		if q[0] == '"' {
			end := strings.IndexByte(q[1:], '"')
			var phrase string
			if end < 0 {
				phrase, q = q[1:], ""
			} else {
				phrase, q = q[1:end+1], q[end+2:]
			}
			if strings.TrimSpace(phrase) != "" {
				terms = append(terms, `"`+strings.ReplaceAll(phrase, `"`, `""`)+`"`)
			}
			continue
		}
		end := strings.IndexAny(q, " \t\r\n")
		word := q
		if end >= 0 {
			word, q = q[:end], q[end:]
		} else {
			q = ""
		}
		word = strings.ReplaceAll(word, `"`, "")
		if word != "" {
			terms = append(terms, `"`+word+`"*`)
		}
	}
	return strings.Join(terms, " ")
}

func (a *App) handleIxSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"hits": []ixHit{}, "total": 0})
		return
	}
	limit := max(1, min(200, int(number(r.URL.Query().Get("limit"), 50))))
	offset := max(0, int(number(r.URL.Query().Get("offset"), 0)))
	metas, err := a.ixGroups(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	byID := map[string]ixGroupMeta{}
	var scope []string
	group := r.URL.Query().Get("group")
	category := r.URL.Query().Get("category")
	for _, m := range metas {
		byID[m.ID] = m
		if (group == "" || m.ID == group) && (category == "" || m.Category == category) {
			scope = append(scope, m.ID)
		}
	}
	if len(scope) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"hits": []ixHit{}, "total": 0})
		return
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(scope)), ",")
	var fts int
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name='ix_fts'`).Scan(&fts)
	var rows *sql.Rows
	args := []any{}
	if fts > 0 {
		args = append(args, ixFTSQuery(q))
		for _, id := range scope {
			args = append(args, id)
		}
		args = append(args, limit+1, offset)
		rows, err = a.db.Reader.QueryContext(ctx, `SELECT m.group_id, m.message_id, m.date, m.topic_id, m.deleted_at, snippet(ix_fts, 0, char(2), char(3), '…', 16)
			FROM ix_fts JOIN ix_messages m ON m.rowid = ix_fts.rowid
			WHERE ix_fts MATCH ? AND m.group_id IN (`+marks+`) ORDER BY m.date DESC LIMIT ? OFFSET ?`, args...)
	} else {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		args = append(args, like)
		for _, id := range scope {
			args = append(args, id)
		}
		args = append(args, limit+1, offset)
		rows, err = a.db.Reader.QueryContext(ctx, `SELECT group_id, message_id, date, topic_id, deleted_at, substr(text, 1, 240)
			FROM ix_messages WHERE text LIKE ? ESCAPE '\' AND group_id IN (`+marks+`) ORDER BY date DESC LIMIT ? OFFSET ?`, args...)
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "search: "+err.Error())
		return
	}
	defer rows.Close()
	hits := []ixHit{}
	layouts := map[string]*ixGroup{}
	for rows.Next() {
		var h ixHit
		var topic int
		var deleted int64
		if err := rows.Scan(&h.GroupID, &h.MessageID, &h.Date, &topic, &deleted, &h.Snippet); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		h.Deleted = deleted > 0
		meta := byID[h.GroupID]
		h.Group = meta.Name
		g := layouts[h.GroupID]
		if g == nil {
			if g, err = a.ixLayoutOf(ctx, meta); err != nil {
				continue
			}
			layouts[h.GroupID] = g
		}
		if g.Forum {
			t := topic
			if t == 0 {
				t = 1
			}
			h.Topic = g.topicTitle(t)
		}
		page := g.PageOf[h.MessageID]
		if page == "" {
			page = "index.html"
		}
		h.Link = "/ix/view/" + g.Dir + "/" + page + "#m" + strconv.Itoa(h.MessageID)
		hits = append(hits, h)
	}
	more := len(hits) > limit
	if more {
		hits = hits[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits, "more": more, "mode": map[bool]string{true: "full-text", false: "simple"}[fts > 0]})
}

// handleIxFinished marks groups finished (or open again). A finished group
// is skipped when groups are added to the rescan queue.
func (a *App) handleIxFinished(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GroupIDs []any `json:"groupIds"`
		Finished bool  `json:"finished"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		return
	}
	if len(body.GroupIDs) == 0 || len(body.GroupIDs) > 5000 {
		writeJSONError(w, http.StatusBadRequest, "groupIds: 1 to 5000 groups")
		return
	}
	at := int64(0)
	if body.Finished {
		at = time.Now().UnixMilli()
	}
	ctx := r.Context()
	for _, raw := range body.GroupIDs {
		id := strings.TrimSpace(toString(raw))
		if id == "" {
			continue
		}
		if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_group_info(group_id,finished_at) VALUES(?,?)
			ON CONFLICT(group_id) DO UPDATE SET finished_at=excluded.finished_at`, id, at); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// ixFinishedGroups is the set of groups marked finished.
func (a *App) ixFinishedGroups(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id FROM ix_group_info WHERE finished_at>0`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}
