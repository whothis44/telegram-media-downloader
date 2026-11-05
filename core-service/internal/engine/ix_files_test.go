package engine

import (
	"context"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

type deadlineSource struct {
	recoverySource
	left time.Duration
}

func (s *deadlineSource) FilePart(ctx context.Context, _ telegram.Attachment, _ int64) ([]byte, error) {
	if d, ok := ctx.Deadline(); ok {
		s.left = time.Until(d)
	}
	return []byte{1}, nil
}

// The limit on a part read also covers the waits Telegram asks for, which reach
// 65 s and more on a speed-limited account. It must outlast several of them, or
// duplicate checks only ever time out while nothing is wrong. A hung request is
// bounded separately, by the guard's own per-request timeout.
func TestFilePartOutlastsSpeedLimitWaits(t *testing.T) {
	source := &deadlineSource{}
	session := &MessageSession{ctx: context.Background(), source: source}
	if _, err := session.FilePart(telegram.Attachment{}, 0); err != nil {
		t.Fatal(err)
	}
	if source.left < 3*65*time.Second {
		t.Fatalf("a part read may only take %s", source.left)
	}
}

type rankSeeingSource struct {
	recoverySource
	rank int64
}

func (s *rankSeeingSource) FilePart(ctx context.Context, _ telegram.Attachment, _ int64) ([]byte, error) {
	s.rank = telegram.DownloadRankKeyForTest(ctx)
	return nil, nil
}

// A check's parts go ahead of queued downloads. Unranked they went last, and under
// a speed limit they never got a turn.
func TestFilePartsOfChecksGoAheadOfDownloads(t *testing.T) {
	source := &rankSeeingSource{}
	session := &MessageSession{ctx: context.Background(), source: source}
	if _, err := session.FilePart(telegram.Attachment{}, 0); err != nil {
		t.Fatal(err)
	}
	if source.rank >= 0 {
		t.Fatalf("rank %d: a queued download with rank 0 would go first", source.rank)
	}
}
