package engine

import (
	"context"
	"errors"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// Archive additions to a history session: a chat's own details, and the
// comments on a channel's posts.

type chatInfoSource interface {
	ChatInfo(context.Context, telegram.Dialog) (telegram.ChatInfo, error)
}

type repliesSource interface {
	RepliesPage(context.Context, telegram.Dialog, int, int, int) (telegram.HistoryPage, error)
}

// ChatInfo reads the chat's description, pinned message, size and linked
// discussion group.
func (s *MessageSession) ChatInfo(dialog telegram.Dialog) (telegram.ChatInfo, error) {
	if err := s.ctx.Err(); err != nil {
		return telegram.ChatInfo{}, err
	}
	source, ok := s.source.(chatInfoSource)
	if !ok {
		return telegram.ChatInfo{}, errors.New("account cannot read chat details")
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	return source.ChatInfo(ctx, dialog)
}

// Replies reads one page of the comments on a channel post, below offsetID.
func (s *MessageSession) Replies(dialog telegram.Dialog, msgID, offsetID int) (telegram.HistoryPage, error) {
	if err := s.ctx.Err(); err != nil {
		return telegram.HistoryPage{}, err
	}
	source, ok := s.source.(repliesSource)
	if !ok {
		return telegram.HistoryPage{}, errors.New("account cannot read comments")
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	return source.RepliesPage(ctx, dialog, msgID, offsetID, 100)
}
