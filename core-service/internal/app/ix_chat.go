package app

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/engine"
	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// A chat's own details, and the comments on a channel's posts, kept with
// its messages during the archive's messages pass.

// ixCommentsPause spaces the comment requests out.
var ixCommentsPause = time.Second

// ixRecordChatInfo keeps the chat's description, pinned message, size and
// linked discussion group. Best effort: a chat whose details cannot be read
// is still rescanned.
func (a *App) ixRecordChatInfo(ctx context.Context, session *engine.MessageSession, dialog telegram.Dialog, groupID string) {
	if groupID == "" {
		return
	}
	info, err := session.ChatInfo(dialog)
	if err != nil {
		if ctx.Err() == nil {
			a.ixLog("chat details for %s: %v", groupID, err)
		}
		return
	}
	linked := ""
	if info.Linked != nil {
		linked = info.Linked.ID
	}
	now := time.Now().UnixMilli()
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO ix_group_info(group_id,title,kind,about,username,members,pinned_id,linked_group,info_at,tl,invite_link) VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(group_id) DO UPDATE SET title=excluded.title, kind=excluded.kind, about=excluded.about, username=excluded.username, members=excluded.members,
			pinned_id=excluded.pinned_id, linked_group=excluded.linked_group, info_at=excluded.info_at, tl=excluded.tl,
			invite_link=CASE WHEN excluded.invite_link<>'' THEN excluded.invite_link ELSE invite_link END`,
		groupID, info.Title, info.Kind, info.About, info.Username, info.Members, info.PinnedID, linked, now, info.TL, info.Invite); err != nil {
		a.ixLog("chat details for %s: %v", groupID, err)
		return
	}
	if err = ixWriteTx(ctx, tx, groupID, nil, nil, ixPeersFrom(info.Users, info.Chats), now); err != nil {
		return
	}
	_ = tx.Commit()
}

// ixSaveComments reads the comments on every post of a channel that has
// more comments on Telegram than are kept. Comments are stored as messages
// of the discussion group they live in, each marked with the post it
// belongs to. A comment request that fails is logged and left for the next
// rescan; it does not fail the messages pass.
func (a *App) ixSaveComments(ctx context.Context, session *engine.MessageSession, dialog telegram.Dialog, groupID string) error {
	type post struct{ id, comments int }
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT message_id, comments FROM ix_messages WHERE group_id=? AND comments>0 AND deleted_at=0 ORDER BY message_id DESC`, groupID)
	if err != nil {
		return err
	}
	var posts []post
	for rows.Next() {
		var p post
		if rows.Scan(&p.id, &p.comments) == nil {
			posts = append(posts, p)
		}
	}
	rows.Close()
	saved := 0
	for _, p := range posts {
		on := groupID + ":" + strconv.Itoa(p.id)
		var have int
		_ = a.db.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM ix_messages WHERE comment_on=?`, on).Scan(&have)
		if have >= p.comments {
			continue
		}
		offset := 0
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			page, err := session.Replies(dialog, p.id, offset)
			for try := 0; err != nil && try < len(historyPageRetries) && ctx.Err() == nil && session.Err() == nil && historyConnectionLost(err); try++ {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(historyPageRetries[try]):
				}
				page, err = session.Replies(dialog, p.id, offset)
			}
			if err != nil {
				if ctx.Err() != nil || session.Err() != nil {
					return err
				}
				a.ixLog("comments on %s: %v", on, err)
				if strings.Contains(err.Error(), "FLOOD_WAIT") || strings.Contains(err.Error(), "FROZEN") {
					return nil // try the rest at the next rescan
				}
				break
			}
			byGroup := map[string][]tg.MessageClass{}
			for _, raw := range page.Messages {
				if g := ixMessageGroup(raw); g != "" {
					byGroup[g] = append(byGroup[g], raw)
				}
			}
			for discussion, messages := range byGroup {
				if err := a.ixRecordHistoryPage(ctx, discussion, telegram.HistoryPage{Messages: messages, Entities: page.Entities}); err != nil {
					return err
				}
				ids := make([]any, 0, len(messages)+2)
				ids = append(ids, on, discussion)
				marks := make([]string, 0, len(messages))
				for _, m := range messages {
					ids = append(ids, m.GetID())
					marks = append(marks, "?")
				}
				if _, err := a.db.Writer.ExecContext(ctx, `UPDATE ix_messages SET comment_on=? WHERE group_id=? AND message_id IN (`+strings.Join(marks, ",")+`)`, ids...); err != nil {
					return err
				}
				saved += len(messages)
			}
			if page.Done || page.NextOffset == 0 || page.NextOffset == offset {
				break
			}
			offset = page.NextOffset
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(ixCommentsPause):
			}
		}
	}
	if saved > 0 {
		a.ixLog("comments: kept %d for %s", saved, groupID)
	}
	return nil
}

// ixMessageGroup is the downloader's ID of the chat a message belongs to.
func ixMessageGroup(m tg.MessageClass) string {
	var peer tg.PeerClass
	switch v := m.(type) {
	case *tg.Message:
		peer = v.PeerID
	case *tg.MessageService:
		peer = v.PeerID
	}
	switch p := peer.(type) {
	case *tg.PeerChannel:
		return strconv.FormatInt(-1000000000000-p.ChannelID, 10)
	case *tg.PeerChat:
		return strconv.FormatInt(-p.ChatID, 10)
	}
	return ""
}
