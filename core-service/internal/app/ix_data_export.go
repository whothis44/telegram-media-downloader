package app

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/botnick/telegram-media-downloader/core-service/internal/version"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// The data export is the archive in plain, documented files for other
// tools (a deduplicator, an album grouper, a search engine...): JSON Lines,
// one record per line, with every message's complete Telegram object kept
// both decoded (raw) and as the original bytes (tl_base64). It is written
// with the index, into its data folder; see ixDataReadme for the format.

const ixDataFormat = 5

type ixDataFile struct {
	DownloadID int64  `json:"download_id"`
	Path       string `json:"path"` // relative to the downloads folder, "/" separated
	Name       string `json:"name,omitempty"`
	Size       int64  `json:"size"`
	Type       string `json:"type,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	TgFile     string `json:"telegram_file,omitempty"` // kind:id:size, as media.telegram_file
	CreatedAt  string `json:"created_at,omitempty"`
	// Linked: the message has no catalogue entry of its own; the file is the
	// one saved for download_id, matched by how (telegram-file,
	// exact-reupload, after-download). Messages' files only.
	Linked string `json:"linked,omitempty"`
	ixDataFacts
}

// ixDataFacts describes a picture or video: what a browser shows before
// the file itself is opened.
type ixDataFacts struct {
	Mime      string  `json:"mime,omitempty"`
	Width     int     `json:"width,omitempty"`
	Height    int     `json:"height,omitempty"`
	DurationS float64 `json:"duration_s,omitempty"`
	// Telegram's "stripped" thumbnail: a tiny JPEG body without its standard
	// header (see core.telegram.org/api/files#stripped-thumbnails).
	ThumbStripped string `json:"thumb_stripped_base64,omitempty"`
}

func ixFactsOf(f telegram.MediaFacts) ixDataFacts {
	out := ixDataFacts{Mime: f.Mime, Width: f.Width, Height: f.Height, DurationS: float64(f.DurationMs) / 1000}
	if len(f.Thumb) > 0 {
		out.ThumbStripped = base64.StdEncoding.EncodeToString(f.Thumb)
	}
	return out
}

type ixDataEdit struct {
	EditDate   int    `json:"edit_date"`
	Text       string `json:"text"`
	RecordedAt int64  `json:"recorded_at"`
}

// ixDataRef points at a message of another chat.
type ixDataRef struct {
	GroupID   string `json:"group_id"`
	MessageID int    `json:"message_id"`
}

type ixDataMedia struct {
	Type   string `json:"type"`
	Name   string `json:"name,omitempty"`
	Size   int64  `json:"size,omitempty"`
	TgFile string `json:"telegram_file,omitempty"`
	ixDataFacts
}

type ixDataMessage struct {
	GroupID    string       `json:"group_id"`
	ChatID     string       `json:"chat_id"`
	MessageID  int          `json:"message_id"`
	Date       int          `json:"date"`
	DateUTC    string       `json:"date_utc,omitempty"`
	EditDate   int          `json:"edit_date,omitempty"`
	Text       string       `json:"text"`
	AlbumID    string       `json:"album_id,omitempty"`
	Sender     string       `json:"sender,omitempty"`
	PostAuthor string       `json:"post_author,omitempty"`
	FwdFrom    string       `json:"forwarded_from,omitempty"`
	FwdDate    int          `json:"forwarded_date,omitempty"`
	ReplyTo    int          `json:"reply_to,omitempty"`
	TopicID    int          `json:"topic_id,omitempty"`
	Views      int          `json:"views,omitempty"`
	Media      *ixDataMedia `json:"media"`
	FileStatus string       `json:"file_status"`
	Files      []ixDataFile `json:"files"`
	Edits      []ixDataEdit `json:"edits,omitempty"`
	DeletedAt  int64        `json:"deleted_at,omitempty"`
	FirstSeen  int64        `json:"first_seen"`
	LastSeen   int64        `json:"last_seen"`
	Service    string       `json:"service,omitempty"`  // a system notice: its TL action (messageActionPinMessage...)
	Comments   int          `json:"comments,omitempty"` // channel post: comments Telegram reports
	CommentOn  *ixDataRef   `json:"comment_on,omitempty"`
	Raw        any          `json:"raw,omitempty"`
	TLBase64   string       `json:"tl_base64,omitempty"`
}

// ixTL turns a decoded Telegram object into plain JSON values: each object
// gets "_" with its TL constructor name (message, messageMediaPhoto...),
// field names follow the TL schema's snake_case, bytes are base64, and
// empty or zero fields are left out.
func ixTL(v any) any { return ixTLValue(reflect.ValueOf(v), 0, false) }

type ixTLNamed interface{ TypeName() string }

var ixBinFields = reflect.TypeOf(bin.Fields(0))

// inline marks a struct held directly in its parent's field: Telegram's
// optional objects of that kind (fwd_from, replies, reactions...) are zero
// when absent, so one with nothing in it is left out.
func ixTLValue(v reflect.Value, depth int, inline bool) any {
	if depth > 64 || !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
		inline = false
	}
	name := ""
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		inline = false
		if n, ok := v.Interface().(ixTLNamed); ok {
			name = n.TypeName()
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		if name == "" && v.CanAddr() {
			if n, ok := v.Addr().Interface().(ixTLNamed); ok {
				name = n.TypeName()
			}
		}
		out := map[string]any{}
		if name != "" {
			out["_"] = name
		}
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() || f.Type == ixBinFields {
				continue
			}
			if val := ixTLValue(v.Field(i), depth+1, true); val != nil {
				out[ixSnake(f.Name)] = val
			}
		}
		if inline && len(out) <= 1 {
			return nil
		}
		return out
	case reflect.Slice, reflect.Array:
		if v.Len() == 0 {
			return nil
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			b := make([]byte, v.Len())
			reflect.Copy(reflect.ValueOf(b), v)
			return base64.StdEncoding.EncodeToString(b)
		}
		out := make([]any, 0, v.Len())
		for i := 0; i < v.Len(); i++ {
			out = append(out, ixTLValue(v.Index(i), depth+1, false))
		}
		return out
	case reflect.String:
		if v.String() == "" {
			return nil
		}
		return v.String()
	case reflect.Bool:
		if !v.Bool() {
			return nil
		}
		return true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.Int() == 0 {
			return nil
		}
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if v.Uint() == 0 {
			return nil
		}
		return v.Uint()
	case reflect.Float32, reflect.Float64:
		if v.Float() == 0 {
			return nil
		}
		return v.Float()
	}
	return nil
}

// ixSnake: PeerID → peer_id, TTLPeriod → ttl_period, GroupedID → grouped_id.
func ixSnake(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i, c := range r {
		if unicode.IsUpper(c) && i > 0 {
			prev := r[i-1]
			nextLower := i+1 < len(r) && unicode.IsLower(r[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(c))
	}
	return b.String()
}

// ixDataVersion changes whenever anything written for the group changes.
func (a *App) ixDataVersion(ctx context.Context, meta ixGroupMeta) string {
	var deleted, lastDeleted int64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(deleted_at),0) FROM ix_messages WHERE group_id=? AND deleted_at>0`, meta.ID).Scan(&deleted, &lastDeleted)
	var hashed int64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM downloads WHERE group_id=? AND file_hash IS NOT NULL AND file_hash<>''`, meta.ID).Scan(&hashed)
	v := fmt.Sprintf("f%d|%s|%d/%d|%d|%v", ixDataFormat, a.ixVersion(ctx, meta.ID), deleted, lastDeleted, hashed, normalizedGroupFilters(meta.Filters))
	var links, lastLink int64
	_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(at),0) FROM ix_msg_link WHERE group_id=?`, meta.ID).Scan(&links, &lastLink)
	if links > 0 { // messages sharing a saved file (ix_links.go)
		v += fmt.Sprintf("|l%d/%d", links, lastLink)
	}
	return v
}

// ixGroupFiles reads a group's downloaded files, by message.
func (a *App) ixGroupFiles(ctx context.Context, groupID string) (map[int][]ixDataFile, []ixDataFile, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT id,message_id,COALESCE(file_name,''),COALESCE(file_size,0),COALESCE(file_type,''),file_path,COALESCE(file_hash,''),
		COALESCE(telegram_media_kind,''),COALESCE(telegram_media_id,''),COALESCE(telegram_media_size,0),COALESCE(CAST(created_at AS TEXT),''),
		COALESCE(telegram_mime,''),COALESCE(media_width,0),COALESCE(media_height,0),COALESCE(media_duration_ms,0),telegram_thumb
		FROM downloads WHERE group_id=? AND message_id>0 AND file_type IS NOT 'stories' AND file_path IS NOT NULL AND file_path<>'' AND (status IS NULL OR status='completed') ORDER BY message_id,id`, groupID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	by := map[int][]ixDataFile{}
	var all []ixDataFile
	for rows.Next() {
		var f ixDataFile
		var msg int
		var kind, id string
		var size, durationMs int64
		var thumb []byte
		if err := rows.Scan(&f.DownloadID, &msg, &f.Name, &f.Size, &f.Type, &f.Path, &f.SHA256, &kind, &id, &size, &f.CreatedAt,
			&f.Mime, &f.Width, &f.Height, &durationMs, &thumb); err != nil {
			return nil, nil, err
		}
		f.DurationS = float64(durationMs) / 1000
		if len(thumb) > 0 {
			f.ThumbStripped = base64.StdEncoding.EncodeToString(thumb)
		}
		if f.Path = ixNormPath(f.Path); f.Path == "" {
			continue
		}
		if kind != "" && id != "" {
			f.TgFile = kind + ":" + id + ":" + strconv.FormatInt(size, 10)
		}
		by[msg] = append(by[msg], f)
		all = append(all, f)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	// Messages sharing a saved file without an entry of their own carry
	// that file (ix_links.go); files.jsonl lists each entry once.
	linked, err := a.ixLinkedFiles(ctx, groupID)
	if err != nil {
		return nil, nil, err
	}
	for _, l := range linked {
		if len(by[l.Msg]) > 0 {
			continue
		}
		f := ixDataFile{DownloadID: l.SavedID, Path: l.Path, Name: l.Name, Size: l.Size, Type: l.FileType, SHA256: l.Hash, TgFile: l.TgFile, CreatedAt: l.Created, Linked: l.How,
			ixDataFacts: ixDataFacts{Mime: l.Mime, Width: l.W, Height: l.H, DurationS: float64(l.DurMs) / 1000}}
		if len(l.Thumb) > 0 {
			f.ThumbStripped = base64.StdEncoding.EncodeToString(l.Thumb)
		}
		by[l.Msg] = append(by[l.Msg], f)
	}
	return by, all, nil
}

// ixWriteGroupMessages streams one group's messages to a gzip JSON Lines
// file, oldest first. It returns how many messages were written.
func (a *App) ixWriteGroupMessages(ctx context.Context, meta ixGroupMeta, files map[int][]ixDataFile, path string) (int, error) {
	edits := map[int][]ixDataEdit{}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT message_id,edit_date,text,recorded_at FROM ix_message_edits WHERE group_id=? ORDER BY message_id,recorded_at`, meta.ID); err == nil {
		for rows.Next() {
			var id int
			var e ixDataEdit
			if rows.Scan(&id, &e.EditDate, &e.Text, &e.RecordedAt) == nil {
				edits[id] = append(edits[id], e)
			}
		}
		rows.Close()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	out, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	zw := gzip.NewWriter(out)
	bw := bufio.NewWriterSize(zw, 1<<16)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT message_id,date,edit_date,text,grouped_id,sender_id,post_author,fwd_from,fwd_date,reply_to,topic_id,
		media_type,media_name,media_size,media_key,views,tl,first_seen,seen_at,deleted_at,service,comments,comment_on FROM ix_messages WHERE group_id=? ORDER BY message_id`, meta.ID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	written := map[int]bool{}
	n := 0
	chat := ixBare(meta.ID)
	for rows.Next() {
		var m ixDataMessage
		var mediaType, mediaName, mediaKey string
		var mediaSize int64
		var tl []byte
		var commentOn string
		if err := rows.Scan(&m.MessageID, &m.Date, &m.EditDate, &m.Text, &m.AlbumID, &m.Sender, &m.PostAuthor, &m.FwdFrom, &m.FwdDate, &m.ReplyTo, &m.TopicID,
			&mediaType, &mediaName, &mediaSize, &mediaKey, &m.Views, &tl, &m.FirstSeen, &m.LastSeen, &m.DeletedAt, &m.Service, &m.Comments, &commentOn); err != nil {
			return n, err
		}
		if i := strings.LastIndex(commentOn, ":"); i > 0 {
			if id, err := strconv.Atoi(commentOn[i+1:]); err == nil {
				m.CommentOn = &ixDataRef{GroupID: commentOn[:i], MessageID: id}
			}
		}
		m.GroupID, m.ChatID = meta.ID, chat
		if m.Date > 0 {
			m.DateUTC = time.Unix(int64(m.Date), 0).UTC().Format(time.RFC3339)
		}
		if mediaType != "" {
			m.Media = &ixDataMedia{Type: mediaType, Name: mediaName, Size: mediaSize, TgFile: mediaKey}
		}
		m.Files = files[m.MessageID]
		if m.Files == nil {
			m.Files = []ixDataFile{}
		}
		m.FileStatus = ixFileStatus(meta, mediaType, len(m.Files) > 0)
		m.Edits = edits[m.MessageID]
		if len(tl) > 0 {
			m.TLBase64 = base64.StdEncoding.EncodeToString(tl)
			if m.Service != "" {
				var msg tg.MessageService
				if msg.Decode(&bin.Buffer{Buf: tl}) == nil {
					m.Raw = ixTL(&msg)
				}
			} else {
				var msg tg.Message
				if msg.Decode(&bin.Buffer{Buf: tl}) == nil {
					m.Raw = ixTL(&msg)
					if att, err := telegram.MessageAttachment(&msg); err == nil && m.Media != nil {
						m.Media.ixDataFacts = ixFactsOf(att.Facts)
					}
				}
			}
		}
		if err := enc.Encode(m); err != nil {
			return n, err
		}
		written[m.MessageID] = true
		n++
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	// Files whose message was never captured: a stub record for each, so
	// every file is reachable from a message line.
	var orphans []int
	for id := range files {
		if !written[id] {
			orphans = append(orphans, id)
		}
	}
	sort.Ints(orphans)
	for _, id := range orphans {
		m := ixDataMessage{GroupID: meta.ID, ChatID: chat, MessageID: id, Files: files[id], FileStatus: "downloaded"}
		m.Media = &ixDataMedia{Type: m.Files[0].Type, Name: m.Files[0].Name, Size: m.Files[0].Size, TgFile: m.Files[0].TgFile, ixDataFacts: m.Files[0].ixDataFacts}
		if err := enc.Encode(m); err != nil {
			return n, err
		}
		n++
	}
	if err := bw.Flush(); err != nil {
		return n, err
	}
	if err := zw.Close(); err != nil {
		return n, err
	}
	return n, out.Close()
}

func ixFileStatus(meta ixGroupMeta, mediaType string, downloaded bool) string {
	switch {
	case downloaded:
		return "downloaded"
	case mediaType == "":
		return "no_media"
	case mediaType != "unknown" && ixWanted(meta.Filters, mediaType):
		return "missing"
	}
	return "not_wanted"
}

type ixDataGroup struct {
	GroupID    string         `json:"group_id"`
	ChatID     string         `json:"chat_id"`
	Name       string         `json:"name"`
	Category   string         `json:"category,omitempty"`
	Configured bool           `json:"configured"`
	Filters    map[string]any `json:"download_settings"`
	Messages   int            `json:"messages"`
	Files      int            `json:"files"`
	LastWalk   int64          `json:"last_full_rescan,omitempty"`
	File       string         `json:"messages_file"`
	// From Telegram's chat details, as last read.
	Title      string `json:"telegram_title,omitempty"`
	Kind       string `json:"type,omitempty"` // channel, supergroup, forum or group
	Forum      bool   `json:"forum,omitempty"`
	About      string `json:"about,omitempty"`
	Username   string `json:"username,omitempty"`
	Members    int    `json:"members,omitempty"`
	PinnedID   int    `json:"pinned_message_id,omitempty"`
	Linked     string `json:"discussion_group_id,omitempty"` // a channel's comments live there
	TgCount    int    `json:"telegram_message_count,omitempty"`
	TgCountAt  int64  `json:"telegram_message_count_at,omitempty"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	InfoBase64 string `json:"info_tl_base64,omitempty"` // messages.chatFull as Telegram sent it
	// How to find the chat again: its public link, or an invite link when
	// Telegram gave one, and its picture.
	Link   string `json:"link,omitempty"`
	Invite string `json:"invite_link,omitempty"`
	Photo  string `json:"photo,omitempty"`
	// Message numbers Telegram confirmed are gone (deleted before the
	// archive saw them), as ranges: "112-119".
	Gone []string `json:"message_ids_gone,omitempty"`
}

// ixWriteData writes the data folder into fresh (the export being built),
// reusing unchanged group files from the previous export in prev.
func (a *App) ixWriteData(ctx context.Context, fresh, prev, downloads string, progress func(string), count *int) error {
	dir := filepath.Join(fresh, "data")
	if err := os.MkdirAll(filepath.Join(dir, "messages"), 0o755); err != nil {
		return err
	}
	writeJSONL := func(name string, rows func(enc *json.Encoder) error) error {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		defer f.Close()
		bw := bufio.NewWriterSize(f, 1<<16)
		enc := json.NewEncoder(bw)
		enc.SetEscapeHTML(false)
		if err := rows(enc); err != nil {
			return err
		}
		if err := bw.Flush(); err != nil {
			return err
		}
		*count++
		return f.Close()
	}
	type groupState struct {
		Version  string `json:"version"`
		Messages int    `json:"messages"`
	}
	oldState := map[string]groupState{}
	if raw, err := os.ReadFile(filepath.Join(prev, "data", "state.json")); err == nil {
		_ = json.Unmarshal(raw, &oldState)
	}
	state := map[string]groupState{}
	metas, err := a.ixGroups(ctx)
	if err != nil {
		return err
	}
	walks := a.ixLatestWalks(ctx)
	var groups []ixDataGroup
	allFiles := map[string][]ixDataFile{}
	for i, meta := range metas {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		progress(fmt.Sprintf("Data export, group %d of %d: %s", i+1, len(metas), meta.Name))
		byMsg, files, err := a.ixGroupFiles(ctx, meta.ID)
		if err != nil {
			return err
		}
		allFiles[meta.ID] = files
		name := "messages/" + ixDirOf(meta.ID) + ".jsonl.gz"
		target := filepath.Join(dir, filepath.FromSlash(name))
		version := a.ixDataVersion(ctx, meta)
		n := -1
		if old, ok := oldState[meta.ID]; ok && old.Version == version {
			// Unchanged since the last export: reuse its file.
			if ixCopyFile(filepath.Join(prev, "data", filepath.FromSlash(name)), target) == nil {
				n = old.Messages
			}
		}
		if n < 0 {
			if n, err = a.ixWriteGroupMessages(ctx, meta, byMsg, target); err != nil {
				return fmt.Errorf("%s: %w", meta.Name, err)
			}
		}
		state[meta.ID] = groupState{version, n}
		*count++
		g := ixDataGroup{GroupID: meta.ID, ChatID: ixBare(meta.ID), Name: meta.Name, Category: meta.Category, Configured: meta.Configured,
			Filters: normalizedGroupFilters(meta.Filters), Messages: n, Files: len(files), LastWalk: walks[meta.ID], File: name}
		var info []byte
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT title, kind, about, username, members, pinned_id, linked_group, tg_count, tg_count_at, finished_at, tl, invite_link FROM ix_group_info WHERE group_id=?`, meta.ID).
			Scan(&g.Title, &g.Kind, &g.About, &g.Username, &g.Members, &g.PinnedID, &g.Linked, &g.TgCount, &g.TgCountAt, &g.FinishedAt, &info, &g.Invite)
		g.Forum = g.Kind == "forum"
		if g.Username != "" {
			g.Link = "https://t.me/" + g.Username
		}
		g.Gone = a.ixGoneRanges(ctx, meta.ID)
		if src := filepath.Join(a.dataDir, "photos", meta.ID+".jpg"); a.dataDir != "" {
			if _, err := os.Stat(src); err == nil {
				_ = os.MkdirAll(filepath.Join(dir, "photos"), 0o755)
				name := "photos/" + ixDirOf(meta.ID) + ".jpg"
				if ixCopyFile(src, filepath.Join(dir, filepath.FromSlash(name))) == nil {
					g.Photo = name
				}
			}
		}
		if len(info) > 0 {
			g.InfoBase64 = base64.StdEncoding.EncodeToString(info)
		}
		groups = append(groups, g)
	}
	progress("Data export: files, people, topics, duplicates…")
	if err := writeJSONL("groups.jsonl", func(enc *json.Encoder) error {
		for _, g := range groups {
			if err := enc.Encode(g); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := writeJSONL("files.jsonl", func(enc *json.Encoder) error {
		for _, g := range groups {
			for _, f := range allFiles[g.GroupID] {
				if err := enc.Encode(struct {
					GroupID string `json:"group_id"`
					ixDataFile
				}{g.GroupID, f}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := writeJSONL("peers.jsonl", func(enc *json.Encoder) error {
		rows, err := a.db.Reader.QueryContext(ctx, `SELECT peer_id,kind,name,username,updated_at FROM ix_peers ORDER BY peer_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p struct {
				PeerID    string `json:"peer_id"`
				Kind      string `json:"kind"`
				Name      string `json:"name"`
				Username  string `json:"username,omitempty"`
				UpdatedAt int64  `json:"updated_at"`
			}
			if err := rows.Scan(&p.PeerID, &p.Kind, &p.Name, &p.Username, &p.UpdatedAt); err != nil {
				return err
			}
			if err := enc.Encode(p); err != nil {
				return err
			}
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	if err := writeJSONL("topics.jsonl", func(enc *json.Encoder) error {
		rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id,topic_id,title FROM ix_topics ORDER BY group_id,topic_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t struct {
				GroupID string `json:"group_id"`
				TopicID int    `json:"topic_id"`
				Title   string `json:"title"`
			}
			if err := rows.Scan(&t.GroupID, &t.TopicID, &t.Title); err != nil {
				return err
			}
			if err := enc.Encode(t); err != nil {
				return err
			}
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	if err := writeJSONL("near_duplicates.jsonl", func(enc *json.Encoder) error {
		clusters, err := a.ixDupClusters(ctx)
		if err != nil {
			return err
		}
		places := a.ixNearPlaces(ctx)
		type place struct {
			GroupID   string `json:"group_id"`
			MessageID int    `json:"message_id"`
		}
		type member struct {
			DownloadID int64   `json:"download_id,omitempty"`
			Path       string  `json:"path"`
			SHA256     string  `json:"sha256,omitempty"`
			TgFile     string  `json:"telegram_file,omitempty"`
			Size       int64   `json:"size"`
			Duration   float64 `json:"duration_s"`
			Places     []place `json:"places"`
		}
		type pair struct {
			A       int     `json:"a"` // index into members
			B       int     `json:"b"`
			Kind    string  `json:"kind"` // same: the same video; clip: one is a part of the other; overlap: they share a stretch
			Score   float64 `json:"score"`
			OffsetS float64 `json:"offset_s"` // add to a time in a to get the same moment in b, in seconds
			CoverA  float64 `json:"cover_a"`  // share of a's length the shared footage covers
			CoverB  float64 `json:"cover_b"`
		}
		type keyed struct {
			a, b string
			p    pair
		}
		var allPairs []keyed
		rows, err := a.db.Reader.QueryContext(ctx, `SELECT key_a,key_b,kind,score,offset_s,cover_a,cover_b FROM ix_vdup ORDER BY key_a,key_b`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k keyed
			if rows.Scan(&k.a, &k.b, &k.p.Kind, &k.p.Score, &k.p.OffsetS, &k.p.CoverA, &k.p.CoverB) == nil {
				allPairs = append(allPairs, k)
			}
		}
		rows.Close()
		for i, c := range clusters {
			out := struct {
				Cluster int      `json:"cluster"`
				Kind    string   `json:"kind"`
				Members []member `json:"members"`
				Pairs   []pair   `json:"pairs"`
			}{Cluster: i + 1, Kind: c.Kind, Members: []member{}, Pairs: []pair{}}
			index := map[string]int{}
			for j, m := range c.Members {
				mm := member{DownloadID: m.DownloadID, Path: ixNormPath(m.Path), Size: m.Size, Duration: m.Duration, Places: []place{}}
				if m.DownloadID > 0 {
					_ = a.db.Reader.QueryRowContext(ctx, `SELECT CASE WHEN COALESCE(telegram_media_id,'')<>'' THEN telegram_media_kind||':'||telegram_media_id||':'||COALESCE(telegram_media_size,0) ELSE '' END FROM downloads WHERE id=?`, m.DownloadID).Scan(&mm.TgFile)
				}
				if strings.HasPrefix(m.Key, "h:") {
					mm.SHA256 = strings.TrimPrefix(m.Key, "h:")
				}
				for _, p := range places[m.Key] {
					mm.Places = append(mm.Places, place{p.GroupID, p.MessageID})
				}
				index[m.Key] = j
				out.Members = append(out.Members, mm)
			}
			for _, k := range allPairs {
				ia, okA := index[k.a]
				ib, okB := index[k.b]
				if okA && okB {
					p := k.p
					p.A, p.B = ia, ib
					out.Pairs = append(out.Pairs, p)
				}
			}
			if err := enc.Encode(out); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	progress("Data export: posts, repost families, links and wallets…")
	if err := a.ixWriteDataModel(ctx, writeJSONL); err != nil {
		return err
	}
	if err := a.ixWriteManifest(ctx, filepath.Join(dir, "manifest-sha256.txt")); err != nil {
		return err
	}
	*count++
	stateBytes, _ := json.MarshalIndent(state, "", " ")
	if err := os.WriteFile(filepath.Join(dir, "state.json"), stateBytes, 0o644); err != nil {
		return err
	}
	manifest := map[string]any{
		"format":         "telegram-archive",
		"format_version": ixDataFormat,
		"exported_at":    time.Now().UTC().Format(time.RFC3339),
		"app_version":    version.AppVersion,
		"downloads_root": downloads,
		"groups":         len(groups),
		"files": map[string]string{
			"groups": "groups.jsonl", "files": "files.jsonl", "peers": "peers.jsonl", "topics": "topics.jsonl",
			"near_duplicates": "near_duplicates.jsonl", "messages": "messages/<group folder>.jsonl.gz", "readme": "README.md",
			"posts": "posts.jsonl", "families": "families.jsonl", "media_clusters": "media_clusters.jsonl", "sets": "sets.jsonl", "covered": "covered.jsonl",
			"people": "people.jsonl", "faces": "faces.jsonl", "media_text": "media_text.jsonl",
			"entities": "entities.jsonl", "checksums": "manifest-sha256.txt",
		},
	}
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644); err != nil {
		return err
	}
	*count += 2
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(ixDataReadme), 0o644); err != nil {
		return err
	}
	*count++
	return nil
}

func ixCopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

const ixDataReadme = "# Telegram archive: data files\n\n" +
	"Everything the archive knows, in plain files for other tools. Written with the index (nightly, or with " +
	"\"Write the index to the NAS now\"). Format version 5; `manifest.json` says which version a folder holds.\n\n" +
	"All files are UTF-8 [JSON Lines](https://jsonlines.org/): one JSON object per line. Message files are gzip-compressed.\n\n" +
	"## Files\n\n" +
	"| File | One line per |\n|---|---|\n" +
	"| `manifest.json` | (one object) export time, app version, `downloads_root`: the downloads folder as the server saw it |\n" +
	"| `groups.jsonl` | chat: `group_id`, `chat_id`, `name`, `category`, `download_settings`, counts, `messages_file`; `link` (public t.me link), `invite_link` (when Telegram gave one), `photo` (the chat's picture, under `photos/`), `message_ids_gone` (message numbers Telegram confirmed are gone, as ranges like `112-119`); from Telegram's chat details: `telegram_title`, `type` (channel, supergroup, forum, group), `forum`, `about`, `username`, `members`, `pinned_message_id`, `discussion_group_id`, `telegram_message_count` (what Telegram says the chat holds; compare with `messages`), `info_tl_base64`; `finished_at` when marked finished |\n" +
	"| `messages/g_<chat_id>.jsonl.gz` | message of that chat, oldest first |\n" +
	"| `files.jsonl` | downloaded file: `group_id`, `download_id`, `path`, `size`, `type`, `sha256`, `telegram_file`, `created_at`, and when known `mime`, `width`, `height`, `duration_s`, `thumb_stripped_base64` |\n" +
	"| `peers.jsonl` | sender or source: `peer_id` (`user:<id>`, `channel:<id>`, `chat:<id>`), `name`, `username` |\n" +
	"| `topics.jsonl` | forum topic: `group_id`, `topic_id`, `title` |\n" +
	"| `near_duplicates.jsonl` | set of videos that share footage (re-encodes, clips): `kind` (`same`, `mixed`); `members` (largest first: `download_id`, `path`, `sha256`, `telegram_file`, `size`, `duration_s`, `places` = the messages it appears in); `pairs` (`a`, `b` = member indexes, `kind` `same`, `clip` or `overlap`, `score`, `offset_s` = seconds to add to a time in `a` to reach the same moment in `b`, `cover_a`/`cover_b` = share of each video's length the shared footage covers) |\n" +
	"| `posts.jsonl` | post (an album counts once): `post` (`<group_id>:<first message_id>`), `group_id`, `message_ids`, `album_id`, `date`, `original_date` (a forward's first date), `family`, `role` (`original`, `repost`, `adds`), `files` (`message_id`, `cluster`, `sha256` or `path`, `status`) |\n" +
	"| `families.jsonl` | repost family: one original post and every repost of it: `family`, `lead_post`, `first_date`, `chats`, `merged` (later posts added files), `posts` (`post`, `group_id`, `date`, `role`), `files` (the merged album in order: `cluster`, `best` file, `introduced_by` post) |\n" +
	"| `media_clusters.jsonl` | set of files that are the same picture or video (2 files or more): `cluster`, `kind`, `generic` (appears in many unrelated posts: a logo or sticker), `best` (the copy shown: most pixels, then most bytes), `members` (`group_id`, `message_id`, `sha256`, `path`, `status`, `matched_by`) |\n" +
	"| `sets.jsonl` | media set: posts by different people or chats that share pictures or videos although none holds them all (each family is one post and its reposts): `set`, `anchor_post` (its first post: names it across exports), `first_date`, `last_date`, `chats`, `families`, `posts`, `files` (`cluster`, `best` file, `role`: `core` = in two or more of its families, `member` = in one, `loose` = in one but unlike the core pictures (only when image recognition compared them), `elsewhere` = belongs to `other_set`; `in_families`, `likeness` = cosine to the closest core file, 0 when not compared; `placed_by_person`) |\n" +
	"| `people.jsonl` | person found by face grouping (only when faces are switched on): `person`, `name` (when given), `hidden`, `faces`, `files` |\n" +
	"| `faces.jsonl` | face found in a picture or in one of three frames of a video: `face`, `file` (the best copy of the file), `box` (x1, y1, x2, y2 as shares of width and height), `score`, `person` (absent: not grouped), `placed_by_person` (a person placed it; with no `person`: taken out of the person it was grouped with) |\n" +
	"| `media_text.jsonl` | words in a file: `file`, `kind` (`picture`: text read from it; `speech`: what is said, written out), `language`, `text`, `segments` (speech: `[start_s, end_s, text]`) |\n" +
	"| `covered.jsonl` | file not downloaded because the library has it: `group_id`, `message_id`, `how` (`telegram-hashes` or `sampled-parts`: the same bytes as `of_path`, checked on Telegram; `held`: a probable copy of `like` waiting at the back of the queue), `at` |\n" +
	"| `entities.jsonl` | link, @username, e-mail, phone, wallet or IBAN found in message text: `kind`, `value`, `count`, `chats`, `first`, `last`, `refs` (up to 200 messages: `group`, `msg`, `date`); wallets and IBANs pass their checksums |\n" +
	"| `manifest-sha256.txt` | `<sha256>  <path>` per stored file (the `sha256sum -c` format, run from the downloads folder) |\n\n" +
	"## IDs\n\n" +
	"- `group_id` is the downloader's marked chat ID (channels and supergroups: `-100` followed by `chat_id`). `chat_id` is Telegram's own ID.\n" +
	"- A message is identified by (`group_id`, `message_id`).\n" +
	"- `album_id` is Telegram's `grouped_id`: messages sharing it in a chat are one album (one post with several photos/videos). The caption is usually on one of them.\n" +
	"- `topic_id`: forum topic (0 = none; in forums, messages without a topic belong to General, topic 1).\n" +
	"- `reply_to`: the message replied to, in the same chat.\n" +
	"- `sender` and `forwarded_from` are `peer_id`s (see `peers.jsonl`); `forwarded_from` can also be a plain name, or `hidden`.\n\n" +
	"## Messages\n\n" +
	"`date`, `edit_date`, `forwarded_date`: Unix seconds (UTC). `first_seen`, `last_seen`, `deleted_at`, `recorded_at`: Unix milliseconds, when the archive saw it.\n\n" +
	"- `media`: what the message carries on Telegram (`type`: photo, video, document, audio, voice, gif, sticker...; `name`; `size` in bytes; `telegram_file`; `mime`, `width`, `height`, `duration_s`; `thumb_stripped_base64`: Telegram's tiny \"stripped\" preview, a JPEG body without its standard header, see core.telegram.org/api/files#stripped-thumbnails), or null.\n" +
	"- `file_status`: `downloaded` (see `files`), `missing` (wanted but not downloaded), `not_wanted` (switched off for the chat), `no_media`.\n" +
	"- `files`: the downloaded file(s), same fields as `files.jsonl`. A message whose file was already saved for another message and that has no entry of its own carries that file with `linked` set to how it was matched (`telegram-file`: the same Telegram file; `exact-reupload`: the same bytes checked on Telegram; `after-download`: the same bytes found once downloaded); `download_id` is then the saved file's entry.\n" +
	"- `edits`: earlier versions of the text, as they were seen.\n" +
	"- `deleted_at`: set when a full rescan no longer found the message on Telegram (it is kept here).\n" +
	"- `raw`: the complete Telegram message, decoded: every object has `_` (its TL constructor name, such as `message` or `messageMediaPhoto`) and its fields in the TL schema's snake_case; bytes are base64. Fields that are 0, false, empty or absent are left out: read a missing number as 0 and a missing flag as false (an entity at the start of the text has no `offset`). Formatting (`entities`), reactions, buttons and full media details are here.\n" +
	"- `tl_base64`: the same message as Telegram's original binary (TL) serialisation, for exact decoding with any MTProto library (gotd/td, Telethon...).\n" +
	"- A line with no `raw` and `date` 0 stands for a downloaded file whose message was never captured.\n" +
	"- `service`: set on system notices (pinned a message, changed the title or photo, created a topic, members joined...): the TL action name; `text` describes it, `raw._` is `messageService`.\n" +
	"- `comments`: on a channel post, how many comments Telegram reports. The comments themselves are messages of the channel's discussion group (`discussion_group_id` in `groups.jsonl`), in that group's own messages file, each with `comment_on` pointing at the channel post (`group_id`, `message_id`).\n\n" +
	"## Files on disk\n\n" +
	"`path` is relative to `downloads_root`, with `/` separators. `sha256` is the SHA-256 of the file's bytes when the app has computed it (it may be missing for some files; compute it yourself if needed).\n\n" +
	"## Duplicates\n\n" +
	"- Exactly the same file: equal `sha256`, or equal `telegram_file` (the same Telegram upload, for example forwarded to several chats; `kind:id:size`).\n" +
	"- Same video, different file (re-encoded, resized or cut): `near_duplicates.jsonl`.\n" +
	"- Same picture or video in any form, folded for viewing: `media_clusters.jsonl`; posts that repeat earlier ones: `families.jsonl`; posts that overlap in part: `sets.jsonl`. `family`, `cluster` and `set` numbers belong to one export.\n"

// ixGoneRanges lists a chat's message numbers confirmed gone, as ranges.
func (a *App) ixGoneRanges(ctx context.Context, groupID string) []string {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT message_id FROM ix_absent WHERE group_id=? ORDER BY message_id`, groupID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	start, prev := -1, -1
	flush := func() {
		if start < 0 {
			return
		}
		if start == prev {
			out = append(out, strconv.Itoa(start))
		} else {
			out = append(out, strconv.Itoa(start)+"-"+strconv.Itoa(prev))
		}
	}
	for rows.Next() {
		var id int
		if rows.Scan(&id) != nil {
			continue
		}
		if start >= 0 && id == prev+1 {
			prev = id
			continue
		}
		flush()
		start, prev = id, id
	}
	flush()
	return out
}
