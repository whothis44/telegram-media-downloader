package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// ixRuntime is the archive's in-memory state for one App.
type ixRuntime struct {
	mu        sync.Mutex
	groups    map[string]bool // configured group IDs, refreshed every ixGroupsTTL
	groupsAt  time.Time
	wake      chan struct{}
	lastError time.Time
	queueMu   sync.Mutex       // serialises queue state changes
	recent    []ixLogLine      // the last archive log lines, for the diagnostics report
	beats     map[string]int64 // group ID -> its backfill's last sign of work (unix ms; ix_stall.go)
	passSeen  map[string]int64 // backfill job ID -> first seen running by this process (unix ms)
	passEnd   map[string]bool  // group ID -> its backfill read every message and is in its end-of-pass work
}

type ixLogLine struct {
	At   time.Time
	Text string
}

const ixRecentLines = 300

const ixGroupsTTL = 30 * time.Second

var ixRuntimes sync.Map // *App -> *ixRuntime

func (a *App) ix() *ixRuntime {
	if v, ok := ixRuntimes.Load(a); ok {
		return v.(*ixRuntime)
	}
	v, _ := ixRuntimes.LoadOrStore(a, &ixRuntime{wake: make(chan struct{}, 1)})
	return v.(*ixRuntime)
}

// ixLog reports archive problems at most once a minute, so a broken disk
// cannot flood the log from the monitor path.
func (a *App) ixLog(format string, args ...any) {
	rt := a.ix()
	rt.mu.Lock()
	rt.recent = append(rt.recent, ixLogLine{time.Now(), fmt.Sprintf(format, args...)})
	if len(rt.recent) > ixRecentLines {
		rt.recent = append([]ixLogLine(nil), rt.recent[len(rt.recent)-ixRecentLines:]...)
	}
	quiet := time.Since(rt.lastError) < time.Minute
	if !quiet {
		rt.lastError = time.Now()
	}
	rt.mu.Unlock()
	if !quiet && a.output != nil {
		fmt.Fprintf(a.output, "Message archive: "+format+"\n", args...)
	}
}

// ixRow is one message as stored in ix_messages.
type ixRow struct {
	MessageID  int
	Date       int
	EditDate   int
	Text       string
	GroupedID  string
	SenderID   string
	PostAuthor string
	FwdFrom    string
	FwdDate    int
	ReplyTo    int
	TopicID    int
	MediaType  string
	MediaName  string
	MediaSize  int64
	MediaKey   string
	Views      int
	TL         []byte
	Service    string // a service message's action: messageActionPinMessage...
	Comments   int    // comments on a channel post
}

type ixPeer struct {
	ID, Kind, Name, Username string
}

type ixTopic struct {
	ID    int
	Title string
	Date  int
}

// ixMarkedChatID is the downloader's ID for a chat: -100… for channels and
// supergroups, -… for basic groups, the plain ID for users.
func ixMarkedChatID(peer tg.PeerClass) string {
	switch p := peer.(type) {
	case *tg.PeerChannel:
		return strconv.FormatInt(-1000000000000-p.ChannelID, 10)
	case *tg.PeerChat:
		return strconv.FormatInt(-p.ChatID, 10)
	case *tg.PeerUser:
		return strconv.FormatInt(p.UserID, 10)
	}
	return ""
}

// ixPeerKey names a sender: user:…, channel:… or chat:… (bare Telegram IDs).
func ixPeerKey(peer tg.PeerClass) string {
	switch p := peer.(type) {
	case *tg.PeerUser:
		return "user:" + strconv.FormatInt(p.UserID, 10)
	case *tg.PeerChannel:
		return "channel:" + strconv.FormatInt(p.ChannelID, 10)
	case *tg.PeerChat:
		return "chat:" + strconv.FormatInt(p.ChatID, 10)
	}
	return ""
}

func ixRowFromMessage(m *tg.Message) ixRow {
	row := ixRow{MessageID: m.ID, Date: m.Date, Text: m.Message, TopicID: telegram.MessageTopicID(m)}
	if v, ok := m.GetEditDate(); ok {
		row.EditDate = v
	}
	if v, ok := m.GetGroupedID(); ok && v != 0 {
		row.GroupedID = strconv.FormatInt(v, 10)
	}
	if v, ok := m.GetFromID(); ok {
		row.SenderID = ixPeerKey(v)
	}
	if v, ok := m.GetPostAuthor(); ok {
		row.PostAuthor = v
	}
	if v, ok := m.GetViews(); ok {
		row.Views = v
	}
	if replies, ok := m.GetReplies(); ok && replies.Comments {
		row.Comments = replies.Replies
	}
	if fwd, ok := m.GetFwdFrom(); ok {
		row.FwdDate = fwd.Date
		if name, ok := fwd.GetFromName(); ok && name != "" {
			row.FwdFrom = name
		} else if from, ok := fwd.GetFromID(); ok {
			row.FwdFrom = ixPeerKey(from)
		}
		if row.FwdFrom == "" {
			row.FwdFrom = "hidden"
		}
	}
	if header, ok := m.ReplyTo.(*tg.MessageReplyHeader); ok {
		// In a forum, a message in a topic "replies" to the topic's first
		// message; only a reply inside the topic also names the topic.
		if !header.ForumTopic || header.ReplyToTopID != 0 {
			row.ReplyTo = header.ReplyToMsgID
		}
	}
	if media, err := telegram.MessageAttachment(m); err == nil {
		row.MediaType, row.MediaName, row.MediaSize = media.Type, media.Name, media.Identity.Size
		if media.Voice {
			row.MediaType = "voice"
		}
		row.MediaKey = media.Identity.Kind + ":" + media.Identity.ID + ":" + strconv.FormatInt(media.Identity.Size, 10)
	} else if !errors.Is(err, telegram.ErrNoMedia) {
		row.MediaType = "unknown"
	}
	var buf bin.Buffer
	if err := m.Encode(&buf); err == nil {
		row.TL = buf.Copy()
	}
	return row
}

// ixRowFromService keeps a service message (pin, title or photo change,
// members joining...) as a line of the chat with a plain description.
func ixRowFromService(m *tg.MessageService) ixRow {
	row := ixRow{MessageID: m.ID, Date: m.Date, TopicID: ixServiceTopic(m), Text: ixServiceText(m.Action)}
	if m.Action != nil {
		row.Service = m.Action.TypeName()
	}
	if v, ok := m.GetFromID(); ok {
		row.SenderID = ixPeerKey(v)
	}
	if header, ok := m.ReplyTo.(*tg.MessageReplyHeader); ok && (!header.ForumTopic || header.ReplyToTopID != 0) {
		row.ReplyTo = header.ReplyToMsgID // a pin points at the pinned message
	}
	var buf bin.Buffer
	if err := m.Encode(&buf); err == nil {
		row.TL = buf.Copy()
	}
	return row
}

func ixServiceTopic(m *tg.MessageService) int {
	header, ok := m.ReplyTo.(*tg.MessageReplyHeader)
	if !ok || !header.ForumTopic {
		return 0
	}
	if header.ReplyToTopID != 0 {
		return header.ReplyToTopID
	}
	return header.ReplyToMsgID
}

// ixServiceText describes a service message in a few words.
func ixServiceText(action tg.MessageActionClass) string {
	switch a := action.(type) {
	case *tg.MessageActionPinMessage:
		return "pinned a message"
	case *tg.MessageActionChatEditTitle:
		return "changed the title to “" + a.Title + "”"
	case *tg.MessageActionChatEditPhoto:
		return "changed the photo"
	case *tg.MessageActionChatDeletePhoto:
		return "removed the photo"
	case *tg.MessageActionChatCreate:
		return "created the group “" + a.Title + "”"
	case *tg.MessageActionChannelCreate:
		return "created the channel “" + a.Title + "”"
	case *tg.MessageActionChatAddUser:
		if len(a.Users) == 1 {
			return "added a member"
		}
		return "added " + strconv.Itoa(len(a.Users)) + " members"
	case *tg.MessageActionChatJoinedByLink, *tg.MessageActionChatJoinedByRequest:
		return "joined"
	case *tg.MessageActionChatDeleteUser:
		return "removed a member"
	case *tg.MessageActionChatMigrateTo:
		return "upgraded to a supergroup"
	case *tg.MessageActionChannelMigrateFrom:
		return "moved here from the old group “" + a.Title + "”"
	case *tg.MessageActionTopicCreate:
		return "created the topic “" + a.Title + "”"
	case *tg.MessageActionTopicEdit:
		if title, ok := a.GetTitle(); ok && title != "" {
			return "renamed the topic to “" + title + "”"
		}
		if closed, ok := a.GetClosed(); ok {
			if closed {
				return "closed the topic"
			}
			return "reopened the topic"
		}
		return "edited the topic"
	case *tg.MessageActionGroupCall:
		return "voice chat"
	case *tg.MessageActionHistoryClear:
		return "cleared the history"
	case nil:
		return "service message"
	}
	name := strings.TrimPrefix(action.TypeName(), "messageAction")
	return "service message: " + name
}

// ixTopicFromService reads a forum topic's name from the service message
// that created or renamed it.
func ixTopicFromService(s *tg.MessageService) (ixTopic, bool) {
	switch action := s.Action.(type) {
	case *tg.MessageActionTopicCreate:
		if action.Title != "" {
			return ixTopic{ID: s.ID, Title: action.Title, Date: s.Date}, true
		}
	case *tg.MessageActionTopicEdit:
		title, ok := action.GetTitle()
		if !ok || title == "" {
			return ixTopic{}, false
		}
		header, _ := s.ReplyTo.(*tg.MessageReplyHeader)
		if header == nil {
			return ixTopic{}, false
		}
		id := header.ReplyToTopID
		if id == 0 {
			id = header.ReplyToMsgID
		}
		if id > 0 {
			return ixTopic{ID: id, Title: title, Date: s.Date}, true
		}
	}
	return ixTopic{}, false
}

func ixPeersFrom(users []tg.UserClass, chats []tg.ChatClass) []ixPeer {
	var out []ixPeer
	for _, raw := range users {
		u, ok := raw.(*tg.User)
		if !ok {
			continue
		}
		name := strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))
		if u.Deleted && name == "" {
			name = "Deleted account"
		}
		out = append(out, ixPeer{ID: "user:" + strconv.FormatInt(u.ID, 10), Kind: "user", Name: name, Username: u.Username})
	}
	for _, raw := range chats {
		switch c := raw.(type) {
		case *tg.Channel:
			out = append(out, ixPeer{ID: "channel:" + strconv.FormatInt(c.ID, 10), Kind: "channel", Name: c.Title, Username: c.Username})
		case *tg.Chat:
			out = append(out, ixPeer{ID: "chat:" + strconv.FormatInt(c.ID, 10), Kind: "chat", Name: c.Title})
		case *tg.ChannelForbidden:
			out = append(out, ixPeer{ID: "channel:" + strconv.FormatInt(c.ID, 10), Kind: "channel", Name: c.Title})
		case *tg.ChatForbidden:
			out = append(out, ixPeer{ID: "chat:" + strconv.FormatInt(c.ID, 10), Kind: "chat", Name: c.Title})
		}
	}
	return out
}

func ixEntities(u tg.UpdatesClass) ([]tg.UserClass, []tg.ChatClass) {
	switch v := u.(type) {
	case *tg.Updates:
		return v.Users, v.Chats
	case *tg.UpdatesCombined:
		return v.Users, v.Chats
	}
	return nil, nil
}

// ixWriteTx stores messages, topic names and sender names for one group.
// A message seen again is no longer considered deleted; a changed text is
// kept in ix_message_edits before it is replaced.
func ixWriteTx(ctx context.Context, tx *sql.Tx, groupID string, rows []ixRow, topics []ixTopic, peers []ixPeer, now int64) error {
	for _, r := range rows {
		var oldText string
		var oldEdit int
		err := tx.QueryRowContext(ctx, `SELECT text, edit_date FROM ix_messages WHERE group_id=? AND message_id=?`, groupID, r.MessageID).Scan(&oldText, &oldEdit)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case r.EditDate < oldEdit:
			// An older copy of the message (a delayed update): keep the newer text.
			if _, err = tx.ExecContext(ctx, `UPDATE ix_messages SET seen_at=?, deleted_at=0 WHERE group_id=? AND message_id=?`, now, groupID, r.MessageID); err != nil {
				return err
			}
			continue
		case oldText != r.Text:
			if _, err = tx.ExecContext(ctx, `INSERT INTO ix_message_edits(group_id,message_id,edit_date,text,recorded_at) VALUES(?,?,?,?,?)`, groupID, r.MessageID, oldEdit, oldText, now); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO ix_messages(group_id,message_id,date,edit_date,text,grouped_id,sender_id,post_author,fwd_from,fwd_date,reply_to,topic_id,media_type,media_name,media_size,media_key,views,tl,first_seen,seen_at,deleted_at,service,comments)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?)
			ON CONFLICT(group_id,message_id) DO UPDATE SET date=excluded.date, edit_date=excluded.edit_date, text=excluded.text, grouped_id=excluded.grouped_id,
				sender_id=excluded.sender_id, post_author=excluded.post_author, fwd_from=excluded.fwd_from, fwd_date=excluded.fwd_date, reply_to=excluded.reply_to,
				topic_id=excluded.topic_id, media_type=excluded.media_type, media_name=excluded.media_name, media_size=excluded.media_size, media_key=excluded.media_key,
				views=MAX(ix_messages.views, excluded.views), tl=excluded.tl, seen_at=excluded.seen_at, deleted_at=0,
				service=excluded.service, comments=MAX(ix_messages.comments, excluded.comments)`,
			groupID, r.MessageID, r.Date, r.EditDate, r.Text, r.GroupedID, r.SenderID, r.PostAuthor, r.FwdFrom, r.FwdDate, r.ReplyTo, r.TopicID,
			r.MediaType, r.MediaName, r.MediaSize, r.MediaKey, r.Views, r.TL, now, now, r.Service, r.Comments)
		if err != nil {
			return err
		}
	}
	for _, t := range topics {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ix_topics(group_id,topic_id,title,title_date) VALUES(?,?,?,?)
			ON CONFLICT(group_id,topic_id) DO UPDATE SET title=excluded.title, title_date=excluded.title_date WHERE excluded.title_date >= ix_topics.title_date`,
			groupID, t.ID, t.Title, t.Date); err != nil {
			return err
		}
	}
	for _, p := range peers {
		if p.ID == "" || (p.Name == "" && p.Username == "") {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ix_peers(peer_id,kind,name,username,updated_at) VALUES(?,?,?,?,?)
			ON CONFLICT(peer_id) DO UPDATE SET kind=excluded.kind, name=CASE WHEN excluded.name<>'' THEN excluded.name ELSE ix_peers.name END,
				username=excluded.username, updated_at=excluded.updated_at`,
			p.ID, p.Kind, p.Name, p.Username, now); err != nil {
			return err
		}
	}
	return nil
}

// ixSplit turns raw Telegram messages into rows and topic names.
func ixSplit(messages []tg.MessageClass) ([]ixRow, []ixTopic) {
	var rows []ixRow
	var topics []ixTopic
	for _, raw := range messages {
		switch m := raw.(type) {
		case *tg.Message:
			if m.ID > 0 {
				rows = append(rows, ixRowFromMessage(m))
			}
		case *tg.MessageService:
			if t, ok := ixTopicFromService(m); ok {
				topics = append(topics, t)
			}
			if m.ID > 0 {
				rows = append(rows, ixRowFromService(m))
			}
		}
	}
	return rows, topics
}

// ixRecordHistoryPage stores one page of a backfill before its messages are
// queued, so every message read is kept even when it has no media.
func (a *App) ixRecordHistoryPage(ctx context.Context, groupID string, page telegram.HistoryPage) error {
	if groupID != "" && page.Count > 0 {
		// How many messages Telegram says the chat has: the completeness check.
		if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_group_info(group_id,tg_count,tg_count_at) VALUES(?,?,?)
			ON CONFLICT(group_id) DO UPDATE SET tg_count=excluded.tg_count, tg_count_at=excluded.tg_count_at`, groupID, page.Count, time.Now().UnixMilli()); err != nil {
			return fmt.Errorf("record message count: %w", err)
		}
	}
	if groupID == "" || len(page.Messages) == 0 {
		return nil
	}
	rows, topics := ixSplit(page.Messages)
	var peers []ixPeer
	if page.Entities != nil {
		peers = ixPeersFrom(page.Entities.Users, page.Entities.Chats)
	}
	if len(rows) == 0 && len(topics) == 0 && len(peers) == 0 {
		return nil
	}
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = ixWriteTx(ctx, tx, groupID, rows, topics, peers, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("record messages: %w", err)
	}
	return tx.Commit()
}

// ixConfigured reports whether a chat is one of the downloader's groups.
// Only those are recorded: other chats the account sees stay private.
func (a *App) ixConfigured(ctx context.Context, groupID string) bool {
	rt := a.ix()
	rt.mu.Lock()
	fresh := rt.groups != nil && time.Since(rt.groupsAt) < ixGroupsTTL
	known := rt.groups[groupID]
	rt.mu.Unlock()
	if fresh {
		return known
	}
	cfg, err := a.config.Load(ctx)
	if err != nil {
		return false
	}
	groups := map[string]bool{}
	for _, g := range configuredGroupList(cfg) {
		if id := strings.TrimSpace(toString(g["id"])); id != "" && g["suspended"] != true {
			groups[id] = true
		}
	}
	rt.mu.Lock()
	rt.groups, rt.groupsAt = groups, time.Now()
	rt.mu.Unlock()
	return groups[groupID]
}

// ixForgetGroups makes the next update reload the group list.
func (a *App) ixForgetGroups() {
	rt := a.ix()
	rt.mu.Lock()
	rt.groups = nil
	rt.mu.Unlock()
}

// ixRecordUpdate is the monitor's MessageRecorder.
func (a *App) ixRecordUpdate(parent context.Context, _ string, update tg.UpdateClass, entities tg.UpdatesClass) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	now := time.Now().UnixMilli()
	switch u := update.(type) {
	case *tg.UpdateDeleteChannelMessages:
		group := strconv.FormatInt(-1000000000000-u.ChannelID, 10)
		if err := a.ixMarkDeleted(ctx, group, u.Messages, now); err != nil {
			a.ixLog("could not mark deleted messages: %v", err)
		}
		return
	case *tg.UpdateDeleteMessages:
		// Basic groups and private chats share one numbering per account,
		// and Telegram does not say which chat these were in.
		if err := a.ixMarkDeleted(ctx, "", u.Messages, now); err != nil {
			a.ixLog("could not mark deleted messages: %v", err)
		}
		return
	}
	var raw tg.MessageClass
	switch u := update.(type) {
	case *tg.UpdateNewMessage:
		raw = u.Message
	case *tg.UpdateNewChannelMessage:
		raw = u.Message
	case *tg.UpdateEditMessage:
		raw = u.Message
	case *tg.UpdateEditChannelMessage:
		raw = u.Message
	default:
		return
	}
	var peer tg.PeerClass
	switch m := raw.(type) {
	case *tg.Message:
		peer = m.PeerID
	case *tg.MessageService:
		peer = m.PeerID
	default:
		return
	}
	group := ixMarkedChatID(peer)
	if group == "" || !a.ixConfigured(ctx, group) {
		return
	}
	rows, topics := ixSplit([]tg.MessageClass{raw})
	users, chats := ixEntities(entities)
	peers := ixPeersFrom(users, chats)
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		a.ixLog("could not record a message: %v", err)
		return
	}
	defer tx.Rollback()
	if err = ixWriteTx(ctx, tx, group, rows, topics, peers, now); err == nil {
		err = tx.Commit()
	}
	if err != nil {
		a.ixLog("could not record a message: %v", err)
	}
}

// ixMarkDeleted flags messages deleted on Telegram. The copy is kept.
// An empty group means "any basic group or private chat" (see above).
func (a *App) ixMarkDeleted(ctx context.Context, groupID string, ids []int, now int64) error {
	if len(ids) == 0 {
		return nil
	}
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		args := []any{now}
		marks := make([]string, 0, end-start)
		for _, id := range ids[start:end] {
			marks = append(marks, "?")
			args = append(args, id)
		}
		query := `UPDATE ix_messages SET deleted_at=? WHERE deleted_at=0 AND message_id IN (` + strings.Join(marks, ",") + `)`
		if groupID != "" {
			query += ` AND group_id=?`
			args = append(args, groupID)
		} else {
			query += ` AND group_id NOT LIKE '-100%'`
		}
		if _, err := a.db.Writer.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}
