package engine

import "github.com/botnick/telegram-media-downloader/core-service/internal/telegram"

// UnwrapTransport names the account behind the progress tracking, so the
// archive's checks of a file's bytes on Telegram (part hashes, a few parts)
// reach it (telegram.PartSourceOf). Those small reads are not counted as
// the transfer's progress.
func (t trackedTransport) UnwrapTransport() telegram.MediaDownloader {
	return t.MediaDownloader
}

var _ telegram.TransportWrapper = trackedTransport{}
