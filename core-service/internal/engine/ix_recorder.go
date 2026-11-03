package engine

import (
	"context"

	"github.com/gotd/td/tg"
)

// MessageRecorder sees every new, edited and deleted message update the
// monitor receives, with or without media, before any download filtering.
// It is used to keep a copy of message details (text, dates, senders,
// albums, topics). It must be quick and must not block the monitor: errors
// are the recorder's own business.
type MessageRecorder func(ctx context.Context, accountID string, update tg.UpdateClass, entities tg.UpdatesClass)

// SetMessageRecorder installs the recorder; nil removes it.
func (c *Controller) SetMessageRecorder(recorder MessageRecorder) {
	c.mu.Lock()
	c.recorder = recorder
	c.mu.Unlock()
}
