package telegram

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/tg"
)

// Cheap checks of a file's bytes on Telegram without downloading it: the
// SHA-256 of its parts (upload.getFileHashes), or a few parts or small
// ranges of it (upload.getFile).
//
// upload.getFileHashes takes any file location, a photo's included; whether
// Telegram answers is up to it (it keeps no hashes for some files, and some
// whole chats). The archive tries it first and falls back to reading parts.

// FilePartSource reads a file's part hashes and ranges straight from
// Telegram. *GotdClient has it, and so *Account, which embeds it.
type FilePartSource interface {
	MediaFileHashes(ctx context.Context, a Attachment, offset int64) ([]tg.FileHash, error)
	MediaRange(ctx context.Context, a Attachment, offset int64, limit int) ([]byte, error)
}

// TransportWrapper is a transport that adds something to another one (the
// download queue's progress tracking) and names the one it wraps.
type TransportWrapper interface {
	UnwrapTransport() MediaDownloader
}

// PartSourceOf finds the FilePartSource behind a transport, looking through
// wrappers. Until ix.10 the checks asked for a bare *GotdClient, while the
// download queue hands over its tracking wrapper around an *Account: the
// question never reached Telegram, so no exact re-upload was ever found
// before downloading.
func PartSourceOf(src MediaDownloader) (FilePartSource, bool) {
	for range 8 {
		if src == nil {
			return nil, false
		}
		if p, ok := src.(FilePartSource); ok {
			return p, true
		}
		w, ok := src.(TransportWrapper)
		if !ok {
			return nil, false
		}
		src = w.UnwrapTransport()
	}
	return nil, false
}

// MediaFileHashes asks Telegram for the SHA-256 hashes of the parts of the
// file starting at offset (upload.getFileHashes).
func (c *GotdClient) MediaFileHashes(ctx context.Context, a Attachment, offset int64) ([]tg.FileHash, error) {
	var out []tg.FileHash
	err := c.fileDC(ctx, a, func(api *tg.Client) error {
		var e error
		out, e = api.UploadGetFileHashes(ctx, &tg.UploadGetFileHashesRequest{Location: a.Location, Offset: offset})
		return e
	})
	return out, err
}

// ValidRange: limit divides 1 MiB and is a multiple of 4 KiB, and offset is
// a multiple of limit, so the range never crosses a 1 MiB boundary (what
// upload.getFile accepts).
func ValidRange(offset int64, limit int) bool {
	return limit >= 4096 && limit <= downloadPartSize && limit%4096 == 0 && downloadPartSize%limit == 0 && offset >= 0 && offset%int64(limit) == 0
}

// MediaRange downloads limit bytes of the file at offset (ValidRange); the
// file's last range is shorter.
func (c *GotdClient) MediaRange(ctx context.Context, a Attachment, offset int64, limit int) ([]byte, error) {
	if !ValidRange(offset, limit) {
		return nil, fmt.Errorf("invalid file range %d+%d", offset, limit)
	}
	var out []byte
	err := c.fileDC(ctx, a, func(api *tg.Client) error {
		for try := 0; ; try++ {
			res, e := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{Location: a.Location, Offset: offset, Limit: limit})
			if e == nil {
				file, ok := res.(*tg.UploadFile)
				if !ok {
					return fmt.Errorf("unexpected Telegram file response %T", res)
				}
				out = file.Bytes
				return nil
			}
			if try >= partTimeoutRetries || !partTimeout(ctx, e) {
				return e
			}
		}
	})
	return out, err
}

// FileHashes asks Telegram for the SHA-256 hashes of the parts of the file
// starting at offset. Telegram keeps no hashes for some files and answers
// with an error or nothing.
func (c AttachmentClient) FileHashes(ctx context.Context, offset int64) ([]tg.FileHash, error) {
	p, ok := PartSourceOf(c.Source)
	if !ok {
		return nil, errors.New("Telegram file hashes are unavailable")
	}
	return p.MediaFileHashes(ctx, c.Media, offset)
}

// ReadPart downloads the 1 MiB part of the file at offset (a multiple of
// 1 MiB); the last part is shorter.
func (c AttachmentClient) ReadPart(ctx context.Context, offset int64) ([]byte, error) {
	if offset%downloadPartSize != 0 {
		return nil, fmt.Errorf("part offset %d is not a multiple of %d", offset, downloadPartSize)
	}
	return c.ReadRange(ctx, offset, downloadPartSize)
}

// ReadRange downloads limit bytes of the file at offset (ValidRange).
func (c AttachmentClient) ReadRange(ctx context.Context, offset int64, limit int) ([]byte, error) {
	if !ValidRange(offset, limit) {
		return nil, fmt.Errorf("invalid file range %d+%d", offset, limit)
	}
	p, ok := PartSourceOf(c.Source)
	if !ok {
		return nil, errors.New("Telegram file parts are unavailable")
	}
	return p.MediaRange(ctx, c.Media, offset, limit)
}
