package telegram

import (
	"context"
	"errors"
	"strconv"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// ChatInfo is what the archive keeps about a chat beyond its posts: its
// description, pinned message, size and, for a channel, the discussion group
// that holds the comments on its posts.
type ChatInfo struct {
	Title    string
	Kind     string // channel, supergroup, forum or group
	About    string
	Username string
	Members  int
	PinnedID int
	Invite   string  // an invite link, when Telegram includes one (the account made it or administers the chat)
	Linked   *Dialog // a channel's discussion group, when it has one
	TL       []byte  // messages.chatFull as Telegram sent it
	Users    []tg.UserClass
	Chats    []tg.ChatClass
}

// ChatInfo reads a chat's full details (channels.getFullChannel or
// messages.getFullChat).
func (a *Account) ChatInfo(ctx context.Context, dialog Dialog) (ChatInfo, error) {
	var info ChatInfo
	var full *tg.MessagesChatFull
	var err error
	switch peer := dialog.peer.(type) {
	case *tg.InputPeerChannel:
		full, err = a.API().ChannelsGetFullChannel(ctx, &tg.InputChannel{ChannelID: peer.ChannelID, AccessHash: peer.AccessHash})
		if err != nil {
			return info, err
		}
		cf, ok := full.FullChat.(*tg.ChannelFull)
		if !ok {
			return info, errors.New("unexpected Telegram channel details")
		}
		info.About = cf.About
		info.Members, _ = cf.GetParticipantsCount()
		info.PinnedID, _ = cf.GetPinnedMsgID()
		if inv, ok := cf.GetExportedInvite(); ok {
			if e, ok := inv.(*tg.ChatInviteExported); ok {
				info.Invite = e.Link
			}
		}
		linked, hasLinked := cf.GetLinkedChatID()
		for _, raw := range full.Chats {
			c, ok := raw.(*tg.Channel)
			if !ok {
				continue
			}
			switch {
			case c.ID == peer.ChannelID:
				info.Title, info.Username = c.Title, c.Username
				switch {
				case c.Forum:
					info.Kind = "forum"
				case c.Megagroup:
					info.Kind = "supergroup"
				default:
					info.Kind = "channel"
				}
			case hasLinked && c.ID == linked:
				hash, _ := c.GetAccessHash()
				info.Linked = &Dialog{peer: &tg.InputPeerChannel{ChannelID: c.ID, AccessHash: hash}, ID: strconv.FormatInt(-1000000000000-c.ID, 10), Name: c.Title, Username: c.Username, Type: "supergroup"}
			}
		}
	case *tg.InputPeerChat:
		full, err = a.API().MessagesGetFullChat(ctx, peer.ChatID)
		if err != nil {
			return info, err
		}
		cf, ok := full.FullChat.(*tg.ChatFull)
		if !ok {
			return info, errors.New("unexpected Telegram group details")
		}
		info.About = cf.About
		info.PinnedID, _ = cf.GetPinnedMsgID()
		if inv, ok := cf.GetExportedInvite(); ok {
			if e, ok := inv.(*tg.ChatInviteExported); ok {
				info.Invite = e.Link
			}
		}
		for _, raw := range full.Chats {
			if c, ok := raw.(*tg.Chat); ok && c.ID == peer.ChatID {
				info.Title, info.Members, info.Kind = c.Title, c.ParticipantsCount, "group"
			}
		}
	default:
		return info, errors.New("chat details are only kept for groups and channels")
	}
	var buf bin.Buffer
	if err = full.Encode(&buf); err == nil {
		info.TL = buf.Copy()
	}
	info.Users, info.Chats = full.Users, full.Chats
	return info, nil
}

// RepliesPage reads one page of the comments on a channel post (newest
// first, below offsetID); the comments live in the channel's discussion
// group.
func (a *Account) RepliesPage(ctx context.Context, dialog Dialog, msgID, offsetID, limit int) (HistoryPage, error) {
	var page HistoryPage
	if dialog.peer == nil {
		return page, errors.New("comments need an account-specific peer")
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	result, err := a.API().MessagesGetReplies(ctx, &tg.MessagesGetRepliesRequest{Peer: dialog.peer, MsgID: msgID, OffsetID: offsetID, Limit: limit})
	if err != nil {
		return page, err
	}
	raw, users, chats, err := historyMessages(result)
	if err != nil {
		return page, err
	}
	page.Entities = &tg.Updates{Users: users, Chats: chats}
	page.Count = historyCount(result)
	for _, m := range raw {
		if m == nil || m.GetID() <= 0 {
			continue
		}
		if offsetID > 0 && m.GetID() >= offsetID {
			continue
		}
		page.Messages = append(page.Messages, m)
		if page.NextOffset == 0 || m.GetID() < page.NextOffset {
			page.NextOffset = m.GetID()
		}
	}
	page.Done = len(raw) < limit || len(page.Messages) == 0
	return page, nil
}

// historyCount is the total Telegram reports for the whole history (or
// thread), not just this page.
func historyCount(result tg.MessagesMessagesClass) int {
	switch r := result.(type) {
	case *tg.MessagesChannelMessages:
		return r.Count
	case *tg.MessagesMessagesSlice:
		return r.Count
	case *tg.MessagesMessages:
		return len(r.Messages)
	}
	return 0
}
