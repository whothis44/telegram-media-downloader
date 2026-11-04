package telegram

import (
	"strings"
	"testing"

	"github.com/gotd/td/tg"
)

func altVideo(id, size int64, w, h int) *tg.Document {
	attrs := []tg.DocumentAttributeClass{}
	if w > 0 {
		attrs = append(attrs, &tg.DocumentAttributeVideo{W: w, H: h, Duration: 60})
	}
	return &tg.Document{ID: id, AccessHash: 1, FileReference: []byte{1}, DCID: 2, Size: size, MimeType: "video/mp4", Attributes: attrs}
}

func altMessage(orig *tg.Document, alts ...tg.DocumentClass) *tg.Message {
	return &tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: 123}, Media: &tg.MessageMediaDocument{Document: orig, AltDocuments: alts}}
}

func TestAltQualitiesKeepOnlySmallerVideos(t *testing.T) {
	orig := altVideo(1, 1000, 1920, 1080)
	orig.Attributes = append(orig.Attributes, &tg.DocumentAttributeFilename{FileName: "clip.mp4"})
	playlist := &tg.Document{ID: 9, DCID: 2, Size: 10, MimeType: "application/x-mpegurl"}
	_, alts, ok := AltQualities(altMessage(orig, altVideo(2, 400, 1280, 720), altVideo(3, 1200, 3840, 2160), playlist, altVideo(1, 500, 1, 1), &tg.DocumentEmpty{}))
	if !ok || len(alts) != 1 || alts[0].Doc.ID != 2 || alts[0].Short() != 720 {
		t.Fatalf("alternatives: %+v", alts)
	}
	if _, _, ok := AltQualities(&tg.Message{ID: 1, Media: &tg.MessageMediaPhoto{}}); ok {
		t.Fatal("a photo has alternatives")
	}
}

func TestPickAltQualityFollowsTheRules(t *testing.T) {
	q := func(id, size int64, w, h int) AltQuality {
		return AltQuality{Doc: altVideo(id, size, w, h), Width: w, Height: h}
	}
	// The smallest of at least 480p, within 40% of the bytes.
	got, ok := PickAltQuality(1000, []AltQuality{q(2, 450, 1280, 720), q(3, 300, 854, 480), q(4, 150, 640, 360)}, 480, 0.4)
	if !ok || got.Doc.ID != 3 {
		t.Fatalf("picked %+v", got)
	}
	// Portrait videos: the shorter side counts.
	if got, ok := PickAltQuality(1000, []AltQuality{q(5, 300, 480, 854)}, 480, 0.4); !ok || got.Doc.ID != 5 {
		t.Fatal("portrait 480p refused")
	}
	// More than 40% saves too little.
	if _, ok := PickAltQuality(1000, []AltQuality{q(2, 450, 1280, 720)}, 480, 0.4); ok {
		t.Fatal("a 45% alternative was picked")
	}
	// Below 480p is not enough.
	if _, ok := PickAltQuality(1000, []AltQuality{q(4, 150, 640, 360)}, 480, 0.4); ok {
		t.Fatal("360p was picked")
	}
	// Without sizes: the smallest above a quarter of the bytes.
	got, ok = PickAltQuality(1000, []AltQuality{q(6, 200, 0, 0), q(7, 300, 0, 0), q(8, 380, 0, 0)}, 480, 0.4)
	if !ok || got.Doc.ID != 7 {
		t.Fatalf("without sizes picked %+v", got)
	}
}

func TestWithAltQualityNamesTheLowerQualityAndLeavesTheMessage(t *testing.T) {
	orig := altVideo(1, 1000, 1920, 1080)
	orig.Thumbs = []tg.PhotoSizeClass{&tg.PhotoStrippedSize{Type: "i", Bytes: []byte{1, 2, 3}}}
	orig.Attributes = append(orig.Attributes, &tg.DocumentAttributeFilename{FileName: "holiday.mp4"})
	alt := altVideo(3, 300, 854, 480)
	msg := altMessage(orig, alt)
	_, alts, _ := AltQualities(msg)
	low := WithAltQuality(msg, alts[0], "holiday.mp4")
	a, err := MessageAttachment(low)
	if err != nil {
		t.Fatal(err)
	}
	if a.Identity.ID != "3" || a.Identity.Size != 300 || a.Type != "video" || a.Name != "holiday (lower quality 480p).mp4" || a.Facts.Height != 480 || len(a.Facts.Thumb) == 0 {
		t.Fatalf("lower-quality attachment: %+v", a)
	}
	o, _ := MessageAttachment(msg)
	if o.Identity.ID != "1" || o.Name != "holiday.mp4" || len(msg.Media.(*tg.MessageMediaDocument).AltDocuments) != 1 {
		t.Fatalf("the message was changed: %+v", o)
	}
	for _, at := range alt.Attributes {
		if _, isName := at.(*tg.DocumentAttributeFilename); isName {
			t.Fatal("the alternative document was changed")
		}
	}
	if !strings.HasSuffix(a.Name, ".mp4") {
		t.Fatal(a.Name)
	}
}
