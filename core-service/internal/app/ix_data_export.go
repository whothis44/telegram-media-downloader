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

	"github.com/botnick/telegram-media-downloader/core-service/internal/version"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// The data export is the archive in plain, documented files for other
// tools (a deduplicator, an album grouper, a search engine...): JSON Lines,
// one record per line, with every message's complete Telegram object kept
// both decoded (raw) and as the original bytes (tl_base64). It is written
// with the index, into its data folder; see ixDataReadme for the format.

const ixDataFormat = 1

type ixDataFile struct {
	DownloadID int64  `json:"download_id"`
	Path       string `json:"path"` // relative to the downloads folder, "/" separated
	Name       string `json:"name,omitempty"`
	Size       int64  `json:"size"`
	Type       string `json:"type,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	TgFile     string `json:"telegram_file,omitempty"` // kind:id:size, as media.telegram_file
	CreatedAt  string `json:"created_at,omitempty"`
}

type ixDataEdit struct {
	EditDate   int    `json:"edit_date"`
	Text       string `json:"text"`
	RecordedAt int64  `json:"recorded_at"`
}

type ixDataMedia struct {
	Type   string `json:"type"`
	Name   string `json:"name,omitempty"`
	Size   int64  `json:"size,omitempty"`
	TgFile string `json:"telegram_file,omitempty"`
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
	Raw        any          `json:"raw,omitempty"`
	TLBase64   string       `json:"tl_base64,omitempty"`
}

// ixTL turns a decoded Telegram object into plain JSON values: each object
// gets "_" with its TL constructor name (message, messageMediaPhoto...),
// field names follow the TL schema's snake_case, bytes are base64, and
// empty or zero fields are left out.
func ixTL(v any) any { return ixTLValue(reflect.ValueOf(v), 0) }

type ixTLNamed interface{ TypeName() string }

var ixBinFields = reflect.TypeOf(bin.Fields(0))

func ixTLValue(v reflect.Value, depth int) any {
	if depth > 64 || !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	name := ""
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
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
			if val := ixTLValue(v.Field(i), depth+1); val != nil {
				out[ixSnake(f.Name)] = val
			}
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
			out = append(out, ixTLValue(v.Index(i), depth+1))
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
	return fmt.Sprintf("f%d|%s|%d/%d|%d|%v", ixDataFormat, a.ixVersion(ctx, meta.ID), deleted, lastDeleted, hashed, normalizedGroupFilters(meta.Filters))
}

// ixGroupFiles reads a group's downloaded files, by message.
func (a *App) ixGroupFiles(ctx context.Context, groupID string) (map[int][]ixDataFile, []ixDataFile, error) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT id,message_id,COALESCE(file_name,''),COALESCE(file_size,0),COALESCE(file_type,''),file_path,COALESCE(file_hash,''),
		COALESCE(telegram_media_kind,''),COALESCE(telegram_media_id,''),COALESCE(telegram_media_size,0),COALESCE(CAST(created_at AS TEXT),'')
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
		var size int64
		if err := rows.Scan(&f.DownloadID, &msg, &f.Name, &f.Size, &f.Type, &f.Path, &f.SHA256, &kind, &id, &size, &f.CreatedAt); err != nil {
			return nil, nil, err
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
	return by, all, rows.Err()
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
		media_type,media_name,media_size,media_key,views,tl,first_seen,seen_at,deleted_at FROM ix_messages WHERE group_id=? ORDER BY message_id`, meta.ID)
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
		if err := rows.Scan(&m.MessageID, &m.Date, &m.EditDate, &m.Text, &m.AlbumID, &m.Sender, &m.PostAuthor, &m.FwdFrom, &m.FwdDate, &m.ReplyTo, &m.TopicID,
			&mediaType, &mediaName, &mediaSize, &mediaKey, &m.Views, &tl, &m.FirstSeen, &m.LastSeen, &m.DeletedAt); err != nil {
			return n, err
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
			var msg tg.Message
			if msg.Decode(&bin.Buffer{Buf: tl}) == nil {
				m.Raw = ixTL(&msg)
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
		m.Media = &ixDataMedia{Type: m.Files[0].Type, Name: m.Files[0].Name, Size: m.Files[0].Size, TgFile: m.Files[0].TgFile}
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
		for i, c := range clusters {
			type member struct {
				DownloadID int64   `json:"download_id"`
				Path       string  `json:"path"`
				Size       int64   `json:"size"`
				Duration   float64 `json:"duration_s"`
			}
			out := struct {
				Cluster int      `json:"cluster"`
				Kind    string   `json:"kind"`
				Members []member `json:"members"`
			}{Cluster: i + 1, Kind: c.Kind}
			for _, m := range c.Members {
				out.Members = append(out.Members, member{m.DownloadID, ixNormPath(m.Path), m.Size, m.Duration})
			}
			if err := enc.Encode(out); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
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
	"\"Write the index to the NAS now\"). Format version 1; `manifest.json` says which version a folder holds.\n\n" +
	"All files are UTF-8 [JSON Lines](https://jsonlines.org/): one JSON object per line. Message files are gzip-compressed.\n\n" +
	"## Files\n\n" +
	"| File | One line per |\n|---|---|\n" +
	"| `manifest.json` | (one object) export time, app version, `downloads_root`: the downloads folder as the server saw it |\n" +
	"| `groups.jsonl` | chat: `group_id`, `chat_id`, `name`, `category`, `download_settings`, counts, `messages_file` |\n" +
	"| `messages/g_<chat_id>.jsonl.gz` | message of that chat, oldest first |\n" +
	"| `files.jsonl` | downloaded file: `group_id`, `download_id`, `path`, `size`, `type`, `sha256`, `telegram_file`, `created_at` |\n" +
	"| `peers.jsonl` | sender or source: `peer_id` (`user:<id>`, `channel:<id>`, `chat:<id>`), `name`, `username` |\n" +
	"| `topics.jsonl` | forum topic: `group_id`, `topic_id`, `title` |\n" +
	"| `near_duplicates.jsonl` | set of videos that look the same (re-encodes, clips): `kind` (`same`, `mixed`), `members` |\n\n" +
	"## IDs\n\n" +
	"- `group_id` is the downloader's marked chat ID (channels and supergroups: `-100` followed by `chat_id`). `chat_id` is Telegram's own ID.\n" +
	"- A message is identified by (`group_id`, `message_id`).\n" +
	"- `album_id` is Telegram's `grouped_id`: messages sharing it in a chat are one album (one post with several photos/videos). The caption is usually on one of them.\n" +
	"- `topic_id`: forum topic (0 = none; in forums, messages without a topic belong to General, topic 1).\n" +
	"- `reply_to`: the message replied to, in the same chat.\n" +
	"- `sender` and `forwarded_from` are `peer_id`s (see `peers.jsonl`); `forwarded_from` can also be a plain name, or `hidden`.\n\n" +
	"## Messages\n\n" +
	"`date`, `edit_date`, `forwarded_date`: Unix seconds (UTC). `first_seen`, `last_seen`, `deleted_at`, `recorded_at`: Unix milliseconds, when the archive saw it.\n\n" +
	"- `media`: what the message carries on Telegram (`type`: photo, video, document, audio, voice, gif, sticker...; `name`; `size` in bytes; `telegram_file`), or null.\n" +
	"- `file_status`: `downloaded` (see `files`), `missing` (wanted but not downloaded), `not_wanted` (switched off for the chat), `no_media`.\n" +
	"- `files`: the downloaded file(s), same fields as `files.jsonl`.\n" +
	"- `edits`: earlier versions of the text, as they were seen.\n" +
	"- `deleted_at`: set when a full rescan no longer found the message on Telegram (it is kept here).\n" +
	"- `raw`: the complete Telegram message, decoded: every object has `_` (its TL constructor name, such as `message` or `messageMediaPhoto`) and its fields in the TL schema's snake_case; bytes are base64; empty fields are left out. Formatting (`entities`), reactions, buttons and full media details are here.\n" +
	"- `tl_base64`: the same message as Telegram's original binary (TL) serialisation, for exact decoding with any MTProto library (gotd/td, Telethon...).\n" +
	"- A line with no `raw` and `date` 0 stands for a downloaded file whose message was never captured.\n\n" +
	"## Files on disk\n\n" +
	"`path` is relative to `downloads_root`, with `/` separators. `sha256` is the SHA-256 of the file's bytes when the app has computed it (it may be missing for some files; compute it yourself if needed).\n\n" +
	"## Duplicates\n\n" +
	"- Exactly the same file: equal `sha256`, or equal `telegram_file` (the same Telegram upload, for example forwarded to several chats; `kind:id:size`).\n" +
	"- Same video, different file (re-encoded, resized or cut): `near_duplicates.jsonl`.\n"
