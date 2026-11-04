package telegram

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
)

// Telegram keeps other qualities of many videos: a message's
// MessageMediaDocument.AltDocuments holds lower-resolution encodes of the
// same video (and sometimes streaming playlists, which are not videos and
// are ignored here).

// AltQuality is one alternative quality of a message's video.
type AltQuality struct {
	Doc    *tg.Document
	Width  int // 0 when Telegram does not say
	Height int
}

// Short is the shorter side in pixels, 0 when unknown.
func (q AltQuality) Short() int {
	if q.Width <= 0 || q.Height <= 0 {
		return 0
	}
	return min(q.Width, q.Height)
}

// AltQualities returns the message's video document and its alternative
// qualities that are videos smaller than it. ok is false when the message
// has no video document.
func AltQualities(message *tg.Message) (orig *tg.Document, alts []AltQuality, ok bool) {
	if message == nil {
		return nil, nil, false
	}
	media, isDoc := message.Media.(*tg.MessageMediaDocument)
	if !isDoc {
		return nil, nil, false
	}
	orig, ok = media.Document.(*tg.Document)
	if !ok || orig == nil {
		return nil, nil, false
	}
	for _, d := range media.AltDocuments {
		doc, isDoc := d.(*tg.Document)
		if !isDoc || doc == nil || doc.ID == orig.ID || doc.Size <= 0 || doc.Size >= orig.Size || doc.DCID <= 0 {
			continue
		}
		mime := strings.ToLower(strings.TrimSpace(strings.SplitN(doc.MimeType, ";", 2)[0]))
		if !strings.HasPrefix(mime, "video/") {
			continue
		}
		q := AltQuality{Doc: doc}
		for _, attr := range doc.Attributes {
			if v, isVideo := attr.(*tg.DocumentAttributeVideo); isVideo {
				q.Width, q.Height = v.W, v.H
			}
		}
		alts = append(alts, q)
	}
	return orig, alts, true
}

// PickAltQuality chooses the lower quality to save first: the smallest
// alternative whose shorter side is at least minShort pixels (when its size
// is given), or otherwise the smallest above a quarter of the original's
// bytes; never more than maxShare of the original's bytes. ok is false when
// no alternative fits.
func PickAltQuality(origSize int64, alts []AltQuality, minShort int, maxShare float64) (AltQuality, bool) {
	var best AltQuality
	found := false
	for _, q := range alts {
		size := q.Doc.Size
		if size >= origSize || float64(size) > maxShare*float64(origSize) {
			continue
		}
		if short := q.Short(); short > 0 {
			if short < minShort {
				continue
			}
		} else if size*4 <= origSize {
			continue
		}
		if !found || size < best.Doc.Size {
			best, found = q, true
		}
	}
	return best, found
}

// WithAltQuality is a copy of message whose media is the alternative
// document instead of the original, named after the original with a note,
// so the usual attachment and download path saves that quality. The
// message itself is not changed.
func WithAltQuality(message *tg.Message, q AltQuality, origName string) *tg.Message {
	media, _ := message.Media.(*tg.MessageMediaDocument)
	if media == nil || q.Doc == nil {
		return message
	}
	doc := *q.Doc
	label := "lower quality"
	if short := q.Short(); short > 0 {
		label += " " + strconv.Itoa(short) + "p"
	}
	ext := filepath.Ext(origName)
	if ext == "" || len(ext) > 6 {
		ext = ".mp4"
	}
	name := strings.TrimSuffix(origName, filepath.Ext(origName)) + " (" + label + ")" + ext
	attrs := make([]tg.DocumentAttributeClass, 0, len(doc.Attributes)+1)
	for _, a := range doc.Attributes {
		if _, isName := a.(*tg.DocumentAttributeFilename); !isName {
			attrs = append(attrs, a)
		}
	}
	doc.Attributes = append(attrs, &tg.DocumentAttributeFilename{FileName: name})
	if len(doc.Thumbs) == 0 {
		doc.Thumbs = documentThumbs(media.Document)
	}
	m := *message
	md := *media
	md.Document = &doc
	md.AltDocuments = nil
	m.Media = &md
	return &m
}

func documentThumbs(d tg.DocumentClass) []tg.PhotoSizeClass {
	if doc, ok := d.(*tg.Document); ok {
		return doc.Thumbs
	}
	return nil
}
