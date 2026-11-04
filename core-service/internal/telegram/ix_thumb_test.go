package telegram

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"
)

func TestThumbAttachmentPicksThe320pxPreview(t *testing.T) {
	doc := &tg.Document{ID: 77, AccessHash: 5, FileReference: []byte{1}, DCID: 4, Size: 90 << 20, MimeType: "video/mp4",
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{W: 1280, H: 720, Duration: 61}},
		Thumbs:     []tg.PhotoSizeClass{&tg.PhotoStrippedSize{Type: "i", Bytes: []byte{1, 2, 3}}, &tg.PhotoSize{Type: "s", W: 90, H: 50, Size: 900}, &tg.PhotoSize{Type: "m", W: 320, H: 180, Size: 14000}}}
	msg := &tg.Message{ID: 9, PeerID: &tg.PeerChannel{ChannelID: 42}, Media: &tg.MessageMediaDocument{Document: doc}}
	a, w, h, ok := ThumbAttachment(msg)
	if !ok || w != 320 || h != 180 || a.DC != 4 || a.Identity.Size != 14000 {
		t.Fatalf("preview: %+v %dx%d %v", a.Identity, w, h, ok)
	}
	loc, isDoc := a.Location.(*tg.InputDocumentFileLocation)
	if !isDoc || loc.ThumbSize != "m" || loc.ID != 77 || loc.AccessHash != 5 {
		t.Fatalf("location: %#v", a.Location)
	}
	// The file's own identity is not the preview's: the library never takes one for the other.
	full, _ := MessageAttachment(msg)
	if full.Identity.Key() == a.Identity.Key() {
		t.Fatal("the preview has the file's identity")
	}
	photo := &tg.Photo{ID: 8, AccessHash: 3, DCID: 2, Sizes: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "x", W: 800, H: 600, Size: 70000}, &tg.PhotoSize{Type: "y", W: 1280, H: 960, Size: 200000}}}
	pm := &tg.Message{ID: 10, PeerID: &tg.PeerChannel{ChannelID: 42}, Media: &tg.MessageMediaPhoto{Photo: photo}}
	if a, w, _, ok := ThumbAttachment(pm); !ok || w != 800 || a.Location.(*tg.InputPhotoFileLocation).ThumbSize != "x" {
		t.Fatalf("photo without m: %v %d", ok, w)
	}
	bare := &tg.Message{ID: 11, PeerID: &tg.PeerChannel{ChannelID: 42}, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 1, DCID: 1, Size: 10, MimeType: "video/mp4"}}}
	if _, _, _, ok := ThumbAttachment(bare); ok {
		t.Fatal("a preview for a file that has none")
	}
}

// A video's preview comes from its own thumbnail sizes (the document's
// thumbs, asked for with thumb_size), never from its animated video
// previews: "m" (320 px) when it has one, else the small "s".
func TestThumbAttachmentUsesTheVideoThumbSizes(t *testing.T) {
	doc := &tg.Document{ID: 78, AccessHash: 6, FileReference: []byte{1}, DCID: 2, Size: 40 << 20, MimeType: "video/mp4",
		Attributes:  []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{W: 720, H: 1280, Duration: 20}},
		Thumbs:      []tg.PhotoSizeClass{&tg.PhotoStrippedSize{Type: "i", Bytes: []byte{1, 2, 3}}, &tg.PhotoSize{Type: "s", W: 51, H: 90, Size: 1500}},
		VideoThumbs: []tg.VideoSizeClass{&tg.VideoSize{Type: "u", W: 720, H: 1280, Size: 300000}}}
	msg := &tg.Message{ID: 12, PeerID: &tg.PeerChannel{ChannelID: 42}, Media: &tg.MessageMediaDocument{Document: doc}}
	a, w, h, ok := ThumbAttachment(msg)
	if !ok || w != 51 || h != 90 || a.Identity.Kind != "thumb-s" {
		t.Fatalf("preview: %+v %dx%d %v", a.Identity, w, h, ok)
	}
	if loc, isDoc := a.Location.(*tg.InputDocumentFileLocation); !isDoc || loc.ThumbSize != "s" || loc.ID != 78 {
		t.Fatalf("location: %#v", a.Location)
	}
	doc.Thumbs = append(doc.Thumbs, &tg.PhotoSize{Type: "m", W: 180, H: 320, Size: 18000})
	if a, w, _, ok := ThumbAttachment(msg); !ok || w != 180 || a.Location.(*tg.InputDocumentFileLocation).ThumbSize != "m" {
		t.Fatalf("with m: %v %d", ok, w)
	}
}

type onePart struct {
	data  []byte
	asked []tg.UploadGetFileRequest
}

func (p *onePart) UploadGetFile(_ context.Context, req *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
	p.asked = append(p.asked, *req)
	end := min(int(req.Offset)+req.Limit, len(p.data))
	return &tg.UploadFile{Bytes: p.data[req.Offset:end]}, nil
}

func TestAPreviewIsOneRequest(t *testing.T) {
	p := &onePart{data: make([]byte, 14000)}
	data, err := fetchPart(context.Background(), p, &tg.InputDocumentFileLocation{ID: 1, ThumbSize: "m"}, 0)
	if err != nil || len(data) != 14000 || len(p.asked) != 1 {
		t.Fatalf("%d bytes, %d requests, %v", len(data), len(p.asked), err)
	}
}
