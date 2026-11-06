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
	}
	leaks := []string{"名前", "🎉", "Ünïcødé", "秘密", "Тема", "🎈", "secretchannel", "1234567890", "secretuser", "deadbeefdeadbeef"}
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
