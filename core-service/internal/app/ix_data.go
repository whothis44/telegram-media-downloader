package app

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file loads what the index shows: the archived messages of a group
// matched (by message ID) to the files the downloader saved, sorted into
// posts (albums count once), topics and pages.

const (
	ixPostsPerPage  = 300
	ixTilesPerPage  = 200
	ixUncategorised = "Uncategorised"
)

var ixTZ = func() *time.Location {
	if loc, err := time.LoadLocation("America/Toronto"); err == nil {
		return loc
	}
	return time.Local
}()

type ixGroupMeta struct {
	ID         string
	Name       string
	Category   string
	Filters    map[string]any
	Configured bool
}

type ixFile struct {
	ID        int64
	MessageID int
	Name      string
	Type      string
	Path      string
	Hash      string
	Size      int64
	Key       string
	Created   int
}

type ixMsg struct {
	ID        int
	Date      int
	EditDate  int
	Text      string
	Grouped   string
	Sender    string
	Author    string
	Fwd       string
	FwdDate   int
	Reply     int
	Topic     int // as recorded: 0 = no topic mark
	TopicID   int // in forum groups: Topic, or 1 (General)
	MediaType string
	MediaName string
	MediaSize int64
	Deleted   int64
	Edits     int
	Files     []*ixFile
	Status    string // downloaded, missing, skipped, none
	Captured  bool   // false: known only from a downloaded file
}

type ixMismatch struct {
	Msg  *ixMsg
	File *ixFile
}

type ixPost struct {
	First *ixMsg
	Msgs  []*ixMsg
	Topic int
	Date  int
}

type ixPage struct {
	Key   string // YYYY-MM or "undated"
	Topic int
	Part  int
	Parts int
	File  string
	Posts []*ixPost
}

type ixCounts struct {
	Messages, Captured, Posts, Albums, WithText, WithMedia, Downloaded, Files, Missing, Skipped, TextOnly, Extra, SizeMismatch, Deleted, Edited, Shared, Topics int
}

type ixGroup struct {
	ixGroupMeta
	Dir      string
	Msgs     []*ixMsg
	ByID     map[int]*ixMsg
	Posts    []*ixPost
	Pages    []*ixPage
	PageOf   map[int]string
	Forum    bool
	Topics   map[int]string
	Counts   ixCounts
	Missing  []*ixMsg
	Extra    []*ixFile
	Mismatch []ixMismatch
	First    int
	Last     int
}

func ixBare(id string) string {
	if strings.HasPrefix(id, "-100") && len(id) > 4 {
		return id[4:]
	}
	return strings.TrimPrefix(id, "-")
}

var ixDirRe = regexp.MustCompile(`[^0-9A-Za-z_-]+`)

func ixDirOf(id string) string {
	return "g_" + ixDirRe.ReplaceAllString(ixBare(id), "_")
}

var ixSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

func ixSlug(name string) string {
	s := strings.Trim(ixSlugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "category"
	}
	return s
}

func ixCategoryOf(group map[string]any) string {
	if c := strings.TrimSpace(toString(group["category"])); c != "" {
		return c
	}
	return ixUncategorised
}

var ixSuggestRe = regexp.MustCompile(`^(.*?)[\s._-]*\d+$`)

// ixSuggestCategory suggests a category from names like "sports-3".
func ixSuggestCategory(name string) string {
	m := ixSuggestRe.FindStringSubmatch(strings.TrimSpace(name))
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func ixFilterKey(mediaType string) string {
	switch mediaType {
	case "photo":
		return "photos"
	case "video":
		return "videos"
	case "voice":
		return "voice"
	case "audio":
		return "audio"
	case "gif":
		return "gifs"
	case "sticker":
		return "stickers"
	}
	return "files"
}

func ixWanted(filters map[string]any, mediaType string) bool {
	key := ixFilterKey(mediaType)
	f := normalizedGroupFilters(filters)
	if key == "stickers" {
		return f[key] == true
	}
	return f[key] != false
}

// ixGroups lists the downloader's groups, plus any chat that has files or
// archived messages but has since been removed from the list.
func (a *App) ixGroups(ctx context.Context) ([]ixGroupMeta, error) {
	cfg, err := a.config.Load(ctx)
	if err != nil {
		return nil, err
	}
	var out []ixGroupMeta
	seen := map[string]bool{}
	for _, g := range configuredGroupList(cfg) {
		id := strings.TrimSpace(toString(g["id"]))
		if id == "" || seen[id] || g["peerId"] != nil && toString(g["peerId"]) != "" {
			continue
		}
		seen[id] = true
		filters, _ := g["filters"].(map[string]any)
		name := strings.TrimSpace(toString(g["name"]))
		if name == "" {
			name = "Chat " + id
		}
		out = append(out, ixGroupMeta{ID: id, Name: name, Category: ixCategoryOf(g), Filters: filters, Configured: true})
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, MAX(name) FROM (
		SELECT group_id, COALESCE(group_name,'') AS name FROM downloads WHERE message_id>0 AND file_type IS NOT 'stories' GROUP BY group_id
		UNION ALL SELECT group_id, '' FROM ix_messages GROUP BY group_id) GROUP BY group_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		if seen[id] || strings.HasPrefix(id, "unknown:") {
			continue
		}
		seen[id] = true
		if strings.TrimSpace(name) == "" {
			name = "Chat " + id
		}
		out = append(out, ixGroupMeta{ID: id, Name: name, Category: ixUncategorised})
	}
	return out, rows.Err()
}

func ixNormPath(p string) string {
	s := strings.ReplaceAll(p, `\`, "/")
	if s == "" || strings.HasPrefix(s, "/") || (len(s) > 1 && s[1] == ':') || strings.ContainsRune(s, 0) {
		return ""
	}
	parts := []string{}
	for _, part := range strings.Split(s, "/") {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return ""
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "/")
}

func ixFileKey(hash, path string) string {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if len(hash) == 64 {
		return "h:" + hash
	}
	return "p:" + path
}

// ixParseCreated reads downloads.created_at ("YYYY-MM-DD HH:MM:SS", UTC).
func ixParseCreated(s string) int {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05.000"} {
		if t, err := time.Parse(layout, s); err == nil {
			return int(t.Unix())
		}
	}
	return 0
}

// ixLoadGroup reads one group. withText=false skips message text (used for
// page layouts and statistics).
func (a *App) ixLoadGroup(ctx context.Context, meta ixGroupMeta, withText bool) (*ixGroup, error) {
	g := &ixGroup{ixGroupMeta: meta, Dir: ixDirOf(meta.ID), ByID: map[int]*ixMsg{}, PageOf: map[int]string{}, Topics: map[int]string{}}
	text := "''"
	if withText {
		text = "text"
	}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT message_id,date,edit_date,`+text+`,grouped_id,sender_id,post_author,fwd_from,fwd_date,reply_to,topic_id,media_type,media_name,media_size,deleted_at
		FROM ix_messages WHERE group_id=? ORDER BY message_id`, meta.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		m := &ixMsg{Captured: true}
		if err := rows.Scan(&m.ID, &m.Date, &m.EditDate, &m.Text, &m.Grouped, &m.Sender, &m.Author, &m.Fwd, &m.FwdDate, &m.Reply, &m.Topic, &m.MediaType, &m.MediaName, &m.MediaSize, &m.Deleted); err != nil {
			rows.Close()
			return nil, err
		}
		g.Msgs = append(g.Msgs, m)
		g.ByID[m.ID] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if edits, err := a.db.Reader.QueryContext(ctx, `SELECT message_id, COUNT(*) FROM ix_message_edits WHERE group_id=? GROUP BY message_id`, meta.ID); err == nil {
		for edits.Next() {
			var id, n int
			if edits.Scan(&id, &n) == nil {
				if m := g.ByID[id]; m != nil {
					m.Edits = n
				}
			}
		}
		edits.Close()
	}
	if topics, err := a.db.Reader.QueryContext(ctx, `SELECT topic_id, title FROM ix_topics WHERE group_id=?`, meta.ID); err == nil {
		for topics.Next() {
			var id int
			var title string
			if topics.Scan(&id, &title) == nil && strings.TrimSpace(title) != "" {
				g.Topics[id] = strings.TrimSpace(title)
			}
		}
		topics.Close()
	}
	files, err := a.db.Reader.QueryContext(ctx, `SELECT id,message_id,COALESCE(file_name,''),COALESCE(file_size,0),COALESCE(file_type,''),file_path,COALESCE(file_hash,''),COALESCE(CAST(created_at AS TEXT),'')
		FROM downloads WHERE group_id=? AND message_id>0 AND file_type IS NOT 'stories' AND file_path IS NOT NULL AND file_path<>'' AND (status IS NULL OR status='completed') ORDER BY message_id`, meta.ID)
	if err != nil {
		return nil, err
	}
	captured := len(g.Msgs) > 0
	for files.Next() {
		f := &ixFile{}
		var created string
		if err := files.Scan(&f.ID, &f.MessageID, &f.Name, &f.Size, &f.Type, &f.Path, &f.Hash, &created); err != nil {
			files.Close()
			return nil, err
		}
		f.Path = ixNormPath(f.Path)
		if f.Path == "" {
			continue
		}
		f.Created = ixParseCreated(created)
		f.Key = ixFileKey(f.Hash, f.Path)
		if f.Name == "" {
			f.Name = f.Path[strings.LastIndex(f.Path, "/")+1:]
		}
		m := g.ByID[f.MessageID]
		if m == nil {
			if captured {
				g.Extra = append(g.Extra, f)
				continue
			}
			// Messages not captured yet: show the file on its own.
			m = &ixMsg{ID: f.MessageID, Date: f.Created, MediaType: f.Type}
			g.Msgs = append(g.Msgs, m)
			g.ByID[m.ID] = m
		}
		m.Files = append(m.Files, f)
	}
	files.Close()
	if err := files.Err(); err != nil {
		return nil, err
	}
	sort.Slice(g.Msgs, func(i, j int) bool { return g.Msgs[i].ID < g.Msgs[j].ID })
	g.analyse()
	g.layout()
	return g, nil
}

func (g *ixGroup) analyse() {
	c := &g.Counts
	for _, m := range g.Msgs {
		if m.Topic > 0 {
			g.Forum = true
			break
		}
	}
	byAlbum := map[string]*ixPost{}
	topicsSeen := map[int]bool{}
	for _, m := range g.Msgs {
		if g.Forum {
			m.TopicID = m.Topic
			if m.TopicID == 0 {
				m.TopicID = 1
			}
			topicsSeen[m.TopicID] = true
		}
		c.Messages++
		if m.Captured {
			c.Captured++
		}
		if m.Text != "" {
			c.WithText++
		}
		if m.Deleted > 0 {
			c.Deleted++
		}
		if m.Edits > 0 {
			c.Edited++
		}
		hasMedia := m.MediaType != ""
		if hasMedia {
			c.WithMedia++
		}
		switch {
		case len(m.Files) > 0:
			m.Status = "downloaded"
			c.Downloaded++
			c.Files += len(m.Files)
			if m.MediaSize > 0 && m.MediaType != "photo" {
				for _, f := range m.Files {
					if f.Size > 0 && f.Size != m.MediaSize {
						c.SizeMismatch++
						g.Mismatch = append(g.Mismatch, ixMismatch{m, f})
					}
				}
			}
		case hasMedia && m.MediaType != "unknown" && ixWanted(g.Filters, m.MediaType):
			m.Status = "missing"
			c.Missing++
			g.Missing = append(g.Missing, m)
		case hasMedia:
			m.Status = "skipped"
			c.Skipped++
		default:
			m.Status = "none"
			c.TextOnly++
		}
		var post *ixPost
		if m.Grouped != "" {
			post = byAlbum[m.Grouped]
		}
		if post == nil {
			post = &ixPost{First: m, Topic: m.TopicID, Date: m.Date}
			g.Posts = append(g.Posts, post)
			if m.Grouped != "" {
				byAlbum[m.Grouped] = post
			}
		}
		post.Msgs = append(post.Msgs, m)
		if m.Date > 0 && (g.First == 0 || m.Date < g.First) {
			g.First = m.Date
		}
		if m.Date > g.Last {
			g.Last = m.Date
		}
	}
	c.Extra = len(g.Extra)
	c.Posts = len(g.Posts)
	for _, p := range g.Posts {
		if p.First.Grouped != "" {
			c.Albums++
		}
	}
	c.Topics = len(topicsSeen)
}

func ixMonth(ts int) string {
	if ts <= 0 {
		return "undated"
	}
	return time.Unix(int64(ts), 0).In(ixTZ).Format("2006-01")
}

func ixLocal(ts int) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(int64(ts), 0).In(ixTZ).Format("2006-01-02 15:04:05")
}

func ixUTC(ts int) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(int64(ts), 0).UTC().Format(time.RFC3339)
}

// layout splits posts into pages: per month, per topic and month in forum
// groups, at most ixPostsPerPage posts each.
func (g *ixGroup) layout() {
	type bucketKey struct {
		topic int
		month string
	}
	buckets := map[bucketKey][]*ixPost{}
	var keys []bucketKey
	for _, p := range g.Posts {
		k := bucketKey{p.Topic, ixMonth(p.Date)}
		if _, ok := buckets[k]; !ok {
			keys = append(keys, k)
		}
		buckets[k] = append(buckets[k], p)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].topic != keys[j].topic {
			return keys[i].topic < keys[j].topic
		}
		return keys[i].month < keys[j].month
	})
	g.Pages = nil
	for _, k := range keys {
		list := buckets[k]
		stem := k.month
		if g.Forum {
			stem = fmt.Sprintf("t%d-%s", k.topic, k.month)
		}
		n := (len(list) + ixPostsPerPage - 1) / ixPostsPerPage
		for i := 0; i < n; i++ {
			file := stem + ".html"
			if n > 1 {
				file = fmt.Sprintf("%s-%d.html", stem, i+1)
			}
			end := min(len(list), (i+1)*ixPostsPerPage)
			pg := &ixPage{Key: k.month, Topic: k.topic, Part: i + 1, Parts: n, File: file, Posts: list[i*ixPostsPerPage : end]}
			g.Pages = append(g.Pages, pg)
			for _, p := range pg.Posts {
				for _, m := range p.Msgs {
					g.PageOf[m.ID] = file
				}
			}
		}
	}
}

func (g *ixGroup) topicTitle(id int) string {
	if t := g.Topics[id]; t != "" {
		return t
	}
	if id == 1 {
		return "General"
	}
	return "Topic #" + strconv.Itoa(id)
}

func (g *ixGroup) placeName(m *ixMsg) string {
	if g.Forum {
		return g.Name + " › " + g.topicTitle(m.TopicID)
	}
	return g.Name
}

// ---------- shared files across groups ----------

type ixPlace struct {
	GroupID   string
	MessageID int
}

// ixShared lists, for every file stored more than once (in different posts
// or groups), every place it appears.
func (a *App) ixShared(ctx context.Context) (map[string][]ixPlace, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `WITH k AS (
			SELECT CASE WHEN length(file_hash)=64 THEN 'h:'||lower(file_hash) ELSE 'p:'||replace(file_path,char(92),'/') END AS key, group_id, message_id
			FROM downloads WHERE message_id>0 AND file_type IS NOT 'stories' AND file_path IS NOT NULL AND file_path<>'' AND (status IS NULL OR status='completed'))
		SELECT key, group_id, message_id FROM k WHERE key IN (SELECT key FROM k GROUP BY key HAVING COUNT(*)>1) ORDER BY key, group_id, message_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]ixPlace{}
	for rows.Next() {
		var key string
		var p ixPlace
		if err := rows.Scan(&key, &p.GroupID, &p.MessageID); err != nil {
			return nil, err
		}
		if strings.HasPrefix(key, "p:") {
			key = "p:" + ixNormPath(strings.TrimPrefix(key, "p:"))
		}
		out[key] = append(out[key], p)
	}
	return out, rows.Err()
}

// ---------- caches for the dashboard view ----------

// ixLayoutCache keeps page layouts (no text) of groups for links between
// groups, rebuilt when a group's data changes.
type ixLayoutCache struct {
	mu      sync.Mutex
	entries map[string]ixLayoutEntry
}

type ixLayoutEntry struct {
	version string
	group   *ixGroup
}

var ixLayouts sync.Map // *App -> *ixLayoutCache

func (a *App) ixVersion(ctx context.Context, groupID string) string {
	var n, maxSeen, files, maxFile int64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(seen_at),0) FROM ix_messages WHERE group_id=?`, groupID).Scan(&n, &maxSeen)
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(id),0) FROM downloads WHERE group_id=?`, groupID).Scan(&files, &maxFile)
	var edits int64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_message_edits WHERE group_id=?`, groupID).Scan(&edits)
	return fmt.Sprintf("%d/%d/%d/%d/%d", n, maxSeen, files, maxFile, edits)
}

// ixLayoutOf returns a group's statistics and page layout (no text).
func (a *App) ixLayoutOf(ctx context.Context, meta ixGroupMeta) (*ixGroup, error) {
	v, _ := ixLayouts.LoadOrStore(a, &ixLayoutCache{entries: map[string]ixLayoutEntry{}})
	cache := v.(*ixLayoutCache)
	version := a.ixVersion(ctx, meta.ID) + "|" + meta.Name + "|" + meta.Category + fmt.Sprint(normalizedGroupFilters(meta.Filters))
	cache.mu.Lock()
	entry, ok := cache.entries[meta.ID]
	cache.mu.Unlock()
	if ok && entry.version == version {
		return entry.group, nil
	}
	g, err := a.ixLoadGroup(ctx, meta, false)
	if err != nil {
		return nil, err
	}
	cache.mu.Lock()
	cache.entries[meta.ID] = ixLayoutEntry{version: version, group: g}
	cache.mu.Unlock()
	return g, nil
}

// ixLatestWalk is the end of the last complete rescan of a group.
func (a *App) ixLatestWalks(ctx context.Context) map[string]int64 {
	out := map[string]int64{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, MAX(finished_at) FROM ix_walks WHERE state='done' GROUP BY group_id`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var at sql.NullInt64
		if rows.Scan(&id, &at) == nil && at.Valid {
			out[id] = at.Int64
		}
	}
	return out
}

// ixCountShared counts a group's files that also appear in another group.
func ixCountShared(g *ixGroup, shared map[string][]ixPlace) {
	n := 0
	for _, m := range g.Msgs {
		for _, f := range m.Files {
			for _, p := range shared[f.Key] {
				if p.GroupID != g.ID {
					n++
					break
				}
			}
		}
	}
	g.Counts.Shared = n
}
