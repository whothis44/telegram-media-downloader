package telegram

import (
	"sync/atomic"
)

// Kept parts (ix.12.11). A check of a big video (the app's sampled check)
// reads some 1 MiB parts of a file from Telegram; the app keeps them on disk,
// keyed by the file's identity. Telegram serves the same bytes for the same
// identity, so a later download of it takes those parts instead of asking
// for them again. The app checks a part against the SHA-256 it stored; the
// download checks its length. A part that fails either is asked of Telegram
// as before. Kept parts take no pacer slot: they are not network bytes.

// KeptParts returns part index (offset index*PartSize) of one file, or false.
type KeptParts func(index int) ([]byte, bool)

// KeptPartsSource finds the kept parts of a file, nil when there are none.
type KeptPartsSource func(id MediaIdentity) KeptParts

var keptPartsSource atomic.Pointer[KeptPartsSource]

// SetKeptParts sets where downloads look for kept parts (nil: nowhere).
func SetKeptParts(source KeptPartsSource) {
	if source == nil {
		keptPartsSource.Store(nil)
		return
	}
	keptPartsSource.Store(&source)
}

func keptPartsFor(id MediaIdentity) KeptParts {
	source := keptPartsSource.Load()
	if source == nil || *source == nil {
		return nil
	}
	return (*source)(id)
}

var keptPartBytes, keptPartRefused atomic.Int64

// KeptPartsUsed is told, after a download that took kept parts finished,
// how many bytes of the file came from them (ix.12.12: one log line each).
type KeptPartsUsed func(id MediaIdentity, bytes int64)

var keptPartsUsed atomic.Pointer[KeptPartsUsed]

// SetKeptPartsUsed sets who hears of downloads that took kept parts (nil: nobody).
func SetKeptPartsUsed(f KeptPartsUsed) {
	if f == nil {
		keptPartsUsed.Store(nil)
		return
	}
	keptPartsUsed.Store(&f)
}

// countingKept counts into used the bytes kept hands out with the length
// keptPart accepts. It is called from the download's own loop only.
func countingKept(kept KeptParts, size int64, used *int64) KeptParts {
	if kept == nil {
		return nil
	}
	return func(index int) ([]byte, bool) {
		data, ok := kept(index)
		if ok && int64(len(data)) == min(int64(downloadPartSize), size-int64(index)*downloadPartSize) {
			*used += int64(len(data))
		}
		return data, ok
	}
}

// reportKeptUsed tells the hook of a finished download's kept bytes.
func reportKeptUsed(id MediaIdentity, used int64) {
	if used <= 0 {
		return
	}
	if f := keptPartsUsed.Load(); f != nil && *f != nil {
		(*f)(id, used)
	}
}

// KeptPartStats: bytes of kept parts written in place of a fetch since the
// start, and kept parts refused for their length (fetched from Telegram).
func KeptPartStats() (bytes, refused int64) {
	return keptPartBytes.Load(), keptPartRefused.Load()
}

// keptPart is part index of a file of size, when kept with the right length:
// a full part, or what is left of the file for its last part.
func keptPart(kept KeptParts, index int, size int64) ([]byte, bool) {
	if kept == nil {
		return nil, false
	}
	data, ok := kept(index)
	if !ok {
		return nil, false
	}
	want := min(int64(downloadPartSize), size-int64(index)*downloadPartSize)
	if want <= 0 || int64(len(data)) != want {
		keptPartRefused.Add(1)
		return nil, false
	}
	return data, true
}
