package telegram

import (
	"context"
	"fmt"
	"strconv"

	"github.com/gotd/td/tg"
)

// Small reads for the archive's copy checks: Telegram's 320 px preview of a
// photo or video (a few KB, one request), and single parts of a file (the
// start of a video), both through the account's shared, guarded download
// pools like any download.

// ThumbSizes are the preview sizes asked for, best first: "m" is 320 px,
// "x" 800 px (for files that have no "m"), "s" 100 px as a last resort.
var ThumbSizes = []string{"m", "x", "s"}

// ThumbMaxBytes caps a preview: anything larger is not a preview.
const ThumbMaxBytes = 512 << 10

// ThumbAttachment is the attachment of a message's small preview picture,
// with its size in pixels. ok is false when the message has none.
func ThumbAttachment(message *tg.Message) (a Attachment, w, h int, ok bool) {
	media, err := MessageAttachment(message)
	if err != nil {
		return Attachment{}, 0, 0, false
	}
	switch m := message.Media.(type) {
	case *tg.MessageMediaDocument:
		if doc, isDoc := m.Document.(*tg.Document); isDoc {
			kind, size, sw, sh, found := pickThumb(doc.Thumbs)
			if !found {
				return Attachment{}, 0, 0, false
			}
			media.Location = &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference, ThumbSize: kind}
			media.Identity = MediaIdentity{Kind: "thumb-" + kind, ID: strconv.FormatInt(doc.ID, 10), Size: size}
			return media, sw, sh, true
		}
	case *tg.MessageMediaPhoto:
		if photo, isPhoto := m.Photo.(*tg.Photo); isPhoto {
			kind, size, sw, sh, found := pickThumb(photo.Sizes)
			if !found {
				return Attachment{}, 0, 0, false
			}
			media.Location = &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: kind}
			media.Identity = MediaIdentity{Kind: "thumb-" + kind, ID: strconv.FormatInt(photo.ID, 10), Size: size}
			return media, sw, sh, true
		}
	}
	return Attachment{}, 0, 0, false
}

// pickThumb finds the best preview size among a file's sizes.
func pickThumb(sizes []tg.PhotoSizeClass) (kind string, size int64, w, h int, ok bool) {
	for _, want := range ThumbSizes {
		for _, s := range sizes {
			switch v := s.(type) {
			case *tg.PhotoSize:
				if v.Type == want && v.Size > 0 && v.Size <= ThumbMaxBytes {
					return v.Type, int64(v.Size), v.W, v.H, true
				}
			case *tg.PhotoSizeProgressive:
				if v.Type == want && len(v.Sizes) > 0 {
					n := v.Sizes[len(v.Sizes)-1]
					if n > 0 && n <= ThumbMaxBytes {
						return v.Type, int64(n), v.W, v.H, true
					}
				}
			}
		}
	}
	return "", 0, 0, 0, false
}

// FilePart reads the part of a file (or preview) at offset, a multiple of
// 1 MiB; a preview is a single part. It goes through the account's flood
// guard and, with pacing on, the datacenter's download window.
func (c *GotdClient) FilePart(ctx context.Context, a Attachment, offset int64) ([]byte, error) {
	if offset%downloadPartSize != 0 {
		return nil, fmt.Errorf("part offset %d is not a multiple of %d", offset, downloadPartSize)
	}
	var out []byte
	err := c.fileDC(ctx, a, func(api *tg.Client) error {
		var e error
		out, e = fetchPart(ctx, api, a.Location, offset)
		return e
	})
	return out, err
}
