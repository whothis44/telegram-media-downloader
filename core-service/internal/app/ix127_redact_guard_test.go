package app

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ix.12.7: the guard for the shared diagnostics report. Every a.ixLog format
// string in the app (found in the source, so a new log line is covered the
// day it is written) is filled with invented names that are hard to hide:
// other scripts, emoji, $&,; dots and spaces, long numbers, in chat names,
// Windows and relative paths, bare file names, error texts, links and IDs.
// The report's redactor must leave none of them. Counts (%d) stay, except a
// message number after '#'.
func TestIx127EveryLogLineIsRedacted(t *testing.T) {
	formats := ix127LogFormats(t)
	if len(formats) < 50 {
		t.Fatalf("found only %d ixLog format strings: the search is broken", len(formats))
	}
	const chat = "Ünïcødé 名前 $&,; 🎉 a.b Chat"
	const topic = "Тема 秘密 #7 🎈 x.y"
	values := []string{
		chat,
		topic,
		`E:\T\tg-dl\library\` + chat + `\sub dir\clip 01.final.mp4`,
		`\\nas\share\` + chat + `\photo (2).jpg`,
		chat + `/sub dir/clip.final.mp4`,
		`data/tgdl-staging/` + chat + `/名前 🎉 $&,;.mp4`,
		`名前 🎉 $&,; a.b.mp4`,
		`open E:\T\tg-dl\library\` + chat + `\a b.mp4: The system cannot find the file specified.`,
		`rename data/tgdl-staging/` + chat + `/x.mkv data/lib/` + chat + `/x.mkv: access denied`,
		`https://t.me/secretchannel/123456`,
		`-1001234567890`,
		`@secretuser_name`,
		`deadbeefdeadbeefdeadbeefdeadbeef00`,
		// ix.12.13: generated names carry Telegram's document and photo IDs
		// after '_', and a path may follow a placeholder put in place of a
		// chat name.
		`G1/` + chat + `/videos/document_1234567890123456789_1234_ab12cd34ef.m4v`,
		`G1/<name>/videos/document_1234567890123456789_1234_ab12cd34ef.m4v`,
		`document_5555555555555555555.mp4`,
		`photo_4444444444444444444.jpg`,
		`document_3333333333333333333.bin`,
		`秘密 clip.3gp`,
		`名前 old.flv`,
		`movie 名前.wmv`,
		// ix.12.14: a chat or topic name inside a file or folder name, with
		// other words around it (the 12.13 report leaked the words before it).
		`G1/videos/Zorblat Words ` + chat + `_005_Quixfen Words_2021.mp4`,
		`G1/videos/_iCaps` + chat + `.mp4`,
		`data/tgdl-staging/G1/` + topic + ` Snarvik/clip.mp4`,
		`E:\T\tg-dl\library\G1\Plinthar ` + chat + ` Dravel\folder`,
		`open E:\T\tg-dl\library\Merovak ` + topic + `\a.mp4: access denied`,
	}
	leaks := []string{"名前", "🎉", "Ünïcødé", "秘密", "Тема", "🎈", "secretchannel", "1234567890", "secretuser", "deadbeefdeadbeef",
		"1234567890123456789", "5555555555", "4444444444", "3333333333", "ab12cd34ef",
		"Zorblat", "Quixfen", "iCaps", "Snarvik", "Plinthar", "Dravel", "Merovak"}
	r := &ixRedactor{groups: map[string]string{}}
	for _, n := range []string{chat, topic} {
		r.groups[n] = "<name>"
		r.names = append(r.names, n)
	}
	r.groups["-1001234567890"] = "G1"
	verb := regexp.MustCompile(`%[-+# 0]*[0-9*]*(?:\.[0-9*]+)?[a-zA-Z%]`)
	checked := 0
	defer func() {
		t.Logf("%d log formats, %d filled lines carried a name to hide", len(formats), checked)
		if checked < 200 {
			t.Errorf("only %d filled lines carried a name: the guard checks too little", checked)
		}
	}()
	for _, f := range formats {
		for _, v := range values {
			var args []any
			for _, m := range verb.FindAllString(f.text, -1) {
				switch m[len(m)-1] {
				case '%':
				case 'd', 'x', 'X', 'o', 'b', 'c', 'U':
					args = append(args, 98765)
				case 'f', 'g', 'e':
					args = append(args, 1.5)
				case 't':
					args = append(args, true)
				default: // s, v, q
					args = append(args, ix127Err(v))
				}
			}
			raw := fmt.Sprintf(f.text, args...)
			if strings.Contains(raw, "%!") {
				t.Errorf("%s: %q: the guard filled it wrongly: %s", f.at, f.text, raw)
			}
			// The guard itself: a line printing a value must carry its marks
			// before redaction, or the check below proves nothing.
			hasLeak := false
			for _, l := range leaks {
				hasLeak = hasLeak || strings.Contains(raw, l)
			}
			if hasLeak {
				checked++
			} else if strings.ContainsAny(v, "名秘") && len(args) > 0 && strings.ContainsAny(f.text, "sv") && verb.MatchString(strings.ReplaceAll(f.text, "%d", "")) {
				for _, a := range args {
					if _, ok := a.(ix127Err); ok {
						t.Errorf("%s: %q printed %q without its marks: %s", f.at, f.text, v, raw)
						break
					}
				}
			}
			line := r.clean(raw)
			for _, l := range leaks {
				if strings.Contains(line, l) {
					t.Errorf("%s: %q with %q leaks %q:\n  %s", f.at, f.text, v, l, line)
				}
			}
			if strings.Contains(line, "#98765") {
				t.Errorf("%s: %q shows a message number: %s", f.at, f.text, line)
			}
		}
	}
}

// ix127Err is a value printed with %v: an error, as most of them are.
type ix127Err string

func (e ix127Err) Error() string { return string(e) }

type ix127Format struct{ text, at string }

// ix127LogFormats finds the format string of every a.ixLog call in the
// package's own (non-test) files.
func ix127LogFormats(t *testing.T) []ix127Format {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []ix127Format
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ixLog" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s: ixLog with a format that is not a constant string: the guard cannot check it", fset.Position(call.Pos()))
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, ix127Format{text, fset.Position(call.Pos()).String()})
			return true
		})
	}
	return out
}

// ix.12.13: the line that leaked in the 18:00-20:00 reports of 11-06, and
// counts that must still read.
func TestIx1213GeneratedNamesAreHidden(t *testing.T) {
	r := &ixRedactor{groups: map[string]string{"Some Chat": "G2"}, names: []string{"Some Chat"}}
	cases := map[string]string{
		"not downloading -1001234567890 #55: same bytes as Some Chat/videos/document_1234567890123456789_1234_ab12cd34ef.m4v (hashes)": "not downloading G2 #<n>: same bytes as <path> (hashes)",
		"same bytes as G1/<name>/videos/document_1234567890123456789_1234_ab12cd34ef.m4v (hashes)":                                     "same bytes as <path> (hashes)",
		"open document_5555555555555555555.mp4: denied":                                                                                "<file>: denied",
		"took 37 files / 338 MB, 12 of 20 parts, deadline 2026-11-30":                                                                  "took 37 files / 338 MB, 12 of 20 parts, deadline 2026-11-30",
		"read 1234567 bytes": "read <n> bytes",
	}
	for in, want := range cases {
		if got := r.clean(in); got != want {
			t.Errorf("clean(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// ix.12.14: the line that leaked in the first 12.13 reports of 11-07: a file
// name holding a chat name. The path and file rules stopped at the '<' of the
// placeholder and left the words before it.
func TestIx1214NamesInsidePathsAreHiddenWhole(t *testing.T) {
	r := &ixRedactor{groups: map[string]string{"Some Chat": "G2", "Wombat Club": "<name>", "-1001234567890": "G3", "-4012345678": "G4"}, names: []string{"Wombat Club", "Some Chat"}}
	cases := map[string]string{
		"not downloading G2 #55: same bytes as Some Chat/videos/Zorblat Words Wombat Club_005.mp4 (hashes)": "not downloading G2 #<n>: same bytes as <path> (hashes)",
		"same bytes as G2/videos/_iCapsWombat Club.mp4 (size and sampled hashes)":                           "same bytes as <path> (size and sampled hashes)",
		`open E:\T\lib\Plinthar Wombat Club Dravel\x.mp4: access denied`:                                    "open <path>: access denied",
		`stat E:\T\lib\Plinthar Wombat Club Dravel: not found`:                                              "stat <path>: not found",
		"saved: Quixfen Wombat Club old.flv":                                                                "saved: <file>",
		"group Wombat Club: 3 files, 12 MB":                                                                 "group <name>: 3 files, 12 MB",
		"link https://t.me/somewhere/12 and mail a@b.example, then Wombat Club":                             "link <link> and mail <email>, then <name>",
		// The sampled check's line: group ID, message number, and a note
		// that may name the saved copy.
		"checked -1001234567890 #5537 (812 MB) from 10 MB of parts: copy (same as Some Chat/videos/Zorblat Wombat Club.mp4)": "checked G3 #<n> (812 MB) from 10 MB of parts: copy (same as <path>)",
		"checked 1234567890 #12 (300 MB) from 9 MB of parts: no match (-)":                                                   "checked <n> #<n> (300 MB) from 9 MB of parts: no match (-)",
		"checked -4012345678 #7 (250 MB) from 9 MB of parts: partly the same (3 of 8)":                                       "checked G4 #<n> (250 MB) from 9 MB of parts: partly the same (3 of 8)",
		"staging: the staged copy of message #4321 in -1001234567890 was missing":                                            "staging: the staged copy of message #<n> in G3 was missing",
	}
	for in, want := range cases {
		if got := r.clean(in); got != want {
			t.Errorf("clean(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}
