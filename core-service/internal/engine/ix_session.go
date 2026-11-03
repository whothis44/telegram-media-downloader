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

type freezeSource interface {
	FreezeState(context.Context) (telegram.FreezeState, error)
}

type byIDSource interface {
	MessagesByID(context.Context, telegram.Dialog, []int) (telegram.ByIDPage, error)
}

// FreezeState reads whether Telegram has frozen the account, and when it
// will be deleted.
func (s *MessageSession) FreezeState() (telegram.FreezeState, error) {
	if err := s.ctx.Err(); err != nil {
		return telegram.FreezeState{}, err
	}
	source, ok := s.source.(freezeSource)
	if !ok {
		return telegram.FreezeState{}, errors.New("account cannot read its freeze state")
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	return source.FreezeState(ctx)
}

// MessagesByID reads up to 100 messages of a channel or supergroup by ID.
func (s *MessageSession) MessagesByID(dialog telegram.Dialog, ids []int) (telegram.ByIDPage, error) {
	if err := s.ctx.Err(); err != nil {
		return telegram.ByIDPage{}, err
	}
	source, ok := s.source.(byIDSource)
	if !ok {
		return telegram.ByIDPage{}, errors.New("account cannot read messages by ID")
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	return source.MessagesByID(ctx, dialog, ids)
}
