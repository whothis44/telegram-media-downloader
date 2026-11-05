package engine

import (
	"context"
	"errors"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
	"github.com/gotd/td/tg"
)

// Archive additions: small file reads (a preview, the first part of a
// video) for the copy checks, through the session's account, and a fresh
// copy of a message when its file reference has expired.

type filePartSource interface {
	FilePart(context.Context, telegram.Attachment, int64) ([]byte, error)
}

type messageRefresher interface {
	RefreshMessage(context.Context, *tg.Message) (*telegram.RefreshedMessage, error)
}

// filePartWaitLimit bounds one read of a file part, Telegram's waits included.
// A speed-limited account is asked to wait 65 s and more at a time, so a tighter
// limit fails checks that are doing nothing wrong (and each failed check waits an
// hour or more to try again). A request that hangs is cut off sooner, by the
// guard's per-request timeout (telegram.filePartTimeout).
const filePartWaitLimit = 5 * time.Minute

// FilePart reads one 1 MiB part of a file (or a whole preview) at offset.
func (s *MessageSession) FilePart(a telegram.Attachment, offset int64) ([]byte, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	source, ok := s.source.(filePartSource)
	if !ok {
		return nil, errors.New("account cannot read file parts")
	}
	ctx, cancel := context.WithTimeout(telegram.WithFileCheckRank(s.ctx), filePartWaitLimit)
	defer cancel()
	return source.FilePart(ctx, a, offset)
}

// RefreshMessage reads a message again (new file references).
func (s *MessageSession) RefreshMessage(message *tg.Message) (*tg.Message, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	source, ok := s.source.(messageRefresher)
	if !ok {
		return nil, errors.New("account cannot refresh messages")
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	fresh, err := source.RefreshMessage(ctx, message)
	if err != nil {
		return nil, err
	}
	if fresh == nil || fresh.Message == nil {
		return nil, errors.New("the message is gone")
	}
	return fresh.Message, nil
}
