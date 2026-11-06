package app

import (
	"strings"
	"testing"
)

// ix.12.5: file names with characters outside letters and digits leaked in
// part ("same bytes as G1/videos/<part of the name><file>"), and message
// numbers under six digits were shown. Invented names only.
func TestIxRedactorHidesWholeNamesAndEveryMessageNumber(t *testing.T) {
	r := &ixRedactor{groups: map[string]string{"Holiday Group": "G1"}, names: []string{"Holiday Group"}}
	for _, tc := range []struct{ in, secret string }{
		{"not downloading G2 #451: same bytes as Holiday Group/videos/$Secret Name & co.mp4 (telegram-hashes)", "Secret"},
		{"not downloading G2 #451: same bytes as Holiday Group/videos/ชื่อลับ ทดสอบ.mkv (telegram-hashes)", "ชื่อลับ"},
		{"not downloading G2 #451: same bytes as Holiday Group/videos/Café ☕ Secret.v2.final.mp4 (telegram-hashes)", "Café"},
		{"not downloading G2 #451: same bytes as Holiday Group/photos/Secret!Plan, 2026.jpg (telegram-hashes)", "Plan"},
		{"held copy G3 #27581 goes: the file it waited for was saved", "27581"},
		{"checked G3 #1131 (300 MB): matched", "1131"},
		{"not downloading G2 #12: Secret Holiday Movie.mp4 (telegram-hashes)", "Movie"},
	} {
		got := r.clean(tc.in)
		if strings.Contains(got, tc.secret) || strings.Contains(got, "Holiday") {
			t.Errorf("%q\n  -> %q (still shows %q)", tc.in, got, tc.secret)
		}
		if !strings.Contains(got, "telegram-hashes") && strings.Contains(tc.in, "telegram-hashes") {
			t.Errorf("hid the reason too: %q", got)
		}
		if strings.HasPrefix(tc.in, "not downloading") && !strings.HasPrefix(got, "not downloading G2 #<n>:") {
			t.Errorf("hid the words before the name: %q", got)
		}
	}
}
