package telegram

import (
	"context"
	"io"
	"strings"
	"testing"
)

type partsWrapper struct{ MediaDownloader }

func (w partsWrapper) UnwrapTransport() MediaDownloader { return w.MediaDownloader }

// The download queue hands the library its progress-tracking wrapper around
// an *Account. The byte checks asked for a bare *GotdClient, so they never
// reached Telegram and no exact re-upload was found before downloading.
func TestPartSourceLooksThroughAccountAndWrappers(t *testing.T) {
	g := &GotdClient{}
	acc := &Account{GotdClient: g}
	for i, src := range []MediaDownloader{g, acc, partsWrapper{acc}, partsWrapper{partsWrapper{acc}}} {
		p, ok := PartSourceOf(src)
		if !ok || p == nil {
			t.Fatalf("source %d: no part source", i)
		}
	}
	plain := profileTransport(func(context.Context, Attachment, io.Writer) error { return nil })
	if _, ok := PartSourceOf(plain); ok {
		t.Fatal("a transport that cannot read parts was taken for one")
	}
	if _, ok := PartSourceOf(partsWrapper{nil}); ok {
		t.Fatal("an empty wrapper was taken for a part source")
	}
	// Through the wrapper the request is made (here it fails for want of a
	// connection), instead of being refused as unavailable.
	c := AttachmentClient{Source: partsWrapper{acc}}
	for name, call := range map[string]func() error{
		"hashes": func() error { _, err := c.FileHashes(context.Background(), 0); return err },
		"part":   func() error { _, err := c.ReadPart(context.Background(), 0); return err },
		"range":  func() error { _, err := c.ReadRange(context.Background(), 65536, 65536); return err },
	} {
		if err := call(); err == nil || strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := (AttachmentClient{Source: plain}).FileHashes(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("plain transport: %v", err)
	}
}

func TestValidRange(t *testing.T) {
	for _, c := range []struct {
		off   int64
		limit int
		ok    bool
	}{
		{0, 65536, true}, {65536 * 15, 65536, true}, {1 << 20, 1 << 20, true}, {4096, 4096, true},
		{4096, 65536, false}, {0, 3000, false}, {0, 2 << 20, false}, {0, 12288, false}, {-65536, 65536, false},
	} {
		if got := ValidRange(c.off, c.limit); got != c.ok {
			t.Fatalf("ValidRange(%d,%d)=%v", c.off, c.limit, got)
		}
	}
}
