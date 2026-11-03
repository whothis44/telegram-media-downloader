package app

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The index pages. The same code renders them for the dashboard (served by
// the app, files through /files/) and for the static copy written next to
// the downloads folder (files linked relatively, opens without the app).

type ixSite struct {
	static    bool
	filesBase string // static only: from the index folder to the downloads folder, e.g. "../Telegram"
}

func ixEncodePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func (s ixSite) fileHref(f *ixFile, depth int) string {
	if s.static {
		return strings.Repeat("../", depth) + s.filesBase + "/" + ixEncodePath(f.Path)
	}
	return "/files/" + ixEncodePath(f.Path) + "?inline=1"
}

func (s ixSite) thumbSrc(f *ixFile, depth int) string {
	if s.static {
		return s.fileHref(f, depth)
	}
	return "/api/thumbs/" + strconv.FormatInt(f.ID, 10)
}

type ixEdit struct {
	EditDate int
	Text     string
}

type ixPeerName struct {
	Name, Username, Kind string
}

// ixRender holds what pages need beyond one group.
type ixRender struct {
	site      ixSite
	shared    map[string][]ixPlace
	layoutOf  func(groupID string) *ixGroup // other groups' layouts, for links
	catOf     func(groupID string) string   // category name of a group
	peers     map[string]ixPeerName
	edits     func(groupID string, ids []int) map[int][]ixEdit
	generated time.Time
	dashboard bool
}

var ixEsc = html.EscapeString

var ixURLRe = regexp.MustCompile(`https?://[^\s<>"']+`)

func ixLinkify(text string) string {
	var b strings.Builder
	last := 0
	for _, loc := range ixURLRe.FindAllStringIndex(text, -1) {
		u := text[loc[0]:loc[1]]
		trimmed := strings.TrimRight(u, ".,;:!?)]")
		b.WriteString(ixEsc(text[last:loc[0]]))
		b.WriteString(`<a href="` + ixEsc(trimmed) + `" rel="noopener noreferrer" target="_blank">` + ixEsc(trimmed) + `</a>`)
		last = loc[0] + len(trimmed)
	}
	b.WriteString(ixEsc(text[last:]))
	return b.String()
}

func ixSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func ixNum(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + ixNum(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

var ixTypeWord = map[string]string{"photo": "photo", "video": "video", "voice": "voice message", "audio": "audio file", "gif": "GIF", "sticker": "sticker", "document": "file", "unknown": "attachment"}

func ixWord(t string) string {
	if w := ixTypeWord[t]; w != "" {
		return w
	}
	return "file"
}

const ixCSS = `:root{--bg:#fff;--fg:#1f2328;--muted:#5d6670;--line:#d9dde3;--soft:#f3f5f7;--accent:#2481cc;--ok:#1a7f37;--warn:#9a6700;--bad:#cf222e}
@media (prefers-color-scheme:dark){:root{--bg:#17212b;--fg:#e7edf3;--muted:#9aa7b4;--line:#2b3a49;--soft:#1f2c3a;--accent:#5eaaf0;--ok:#4cc26a;--warn:#e3b341;--bad:#ff7b72}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
main{max-width:1060px;margin:0 auto;padding:16px}a{color:var(--accent)}h1{font-size:22px;margin:8px 0 4px}h2{font-size:17px;margin:22px 0 8px}
.muted{color:var(--muted);font-size:13px}.warn{color:var(--warn)}.bad{color:var(--bad)}.ok{color:var(--ok)}
table{border-collapse:collapse;width:100%;font-size:14px}th,td{border-bottom:1px solid var(--line);padding:6px 8px;text-align:left;vertical-align:top}th{color:var(--muted);font-weight:600}td.n{text-align:right;font-variant-numeric:tabular-nums}
.nav{display:flex;gap:12px;flex-wrap:wrap;margin:8px 0 16px;font-size:14px}.months{display:flex;flex-wrap:wrap;gap:6px}.months a{padding:3px 8px;border:1px solid var(--line);border-radius:6px;text-decoration:none;font-size:13px}
.cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(220px,1fr));gap:10px;margin:8px 0 6px}.card{border:1px solid var(--line);border-radius:10px;padding:10px 12px;text-decoration:none;color:inherit;display:block}.card b{font-size:15px;color:var(--accent)}
.post{border:1px solid var(--line);border-radius:10px;padding:10px 12px;margin:0 0 12px;background:var(--bg)}.post header{font-size:13px;color:var(--muted);margin-bottom:6px}.post.deleted{border-style:dashed}
.text{white-space:pre-wrap;word-wrap:break-word;margin:4px 0 8px}.media{display:flex;flex-wrap:wrap;gap:8px}.item{display:flex;flex-direction:column;gap:3px;max-width:100%}
.media img{max-height:240px;max-width:100%;border-radius:6px;display:block;background:var(--soft)}.media video{max-height:280px;max-width:100%;border-radius:6px;background:#000}
.file{display:inline-block;padding:6px 10px;border:1px solid var(--line);border-radius:8px;background:var(--soft);text-decoration:none}
.note{font-size:13px;padding:4px 8px;border-radius:6px;background:var(--soft)}.note.missing{color:var(--bad)}.note.skipped{color:var(--muted)}
.badge{display:inline-block;font-size:11.5px;font-weight:600;padding:1px 7px;border-radius:999px;border:1px solid var(--accent);color:var(--accent);background:var(--bg)}.badge.bad{border-color:var(--bad);color:var(--bad)}.badge.warn{border-color:var(--warn);color:var(--warn)}.also{font-size:12px;color:var(--muted)}
details.edits{font-size:13px;color:var(--muted);margin-top:6px}details.edits pre{white-space:pre-wrap;font:inherit;margin:4px 0;padding:6px 8px;background:var(--soft);border-radius:6px}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(170px,1fr));gap:10px}.tile{border:1px solid var(--line);border-radius:8px;overflow:hidden;display:flex;flex-direction:column}
.tile .thumb{display:block;height:130px;background:var(--soft)}.tile img{width:100%;height:130px;object-fit:cover;display:block}.tile .ph{height:130px;display:flex;align-items:center;justify-content:center;text-align:center;padding:8px;font-size:13px;color:var(--muted);word-break:break-word}
.tile .cap{font-size:12px;padding:6px 8px;color:var(--muted);display:flex;flex-direction:column;gap:2px}`

func (r *ixRender) page(title string, depth int, body string) []byte {
	css := strings.Repeat("../", depth) + "style.css"
	top := ""
	if r.dashboard {
		top = `<div class="nav"><a href="/">← Dashboard</a><a href="/ix">Archive tools and search</a></div>`
	}
	return []byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` + ixEsc(title) + `</title><link rel="stylesheet" href="` + css + `"></head><body><main>` + top + body + `</main></body></html>`)
}

func ixCSVBytes(rows [][]string) []byte {
	var b bytes.Buffer
	b.WriteString("\xef\xbb\xbf")
	w := csv.NewWriter(&b)
	w.UseCRLF = true
	_ = w.WriteAll(rows)
	return b.Bytes()
}

func (r *ixRender) senderLabel(g *ixGroup, m *ixMsg) string {
	if m.Sender == "" || m.Sender == "channel:"+ixBare(g.ID) {
		return "Channel post"
	}
	if p, ok := r.peers[m.Sender]; ok && (p.Name != "" || p.Username != "") {
		label := p.Name
		if p.Username != "" {
			if label == "" {
				label = "@" + p.Username
			} else {
				label += " (@" + p.Username + ")"
			}
		}
		return label
	}
	kind, id, _ := strings.Cut(m.Sender, ":")
	switch kind {
	case "user":
		return "User " + id
	case "channel":
		return "Channel " + id
	case "chat":
		return "Group " + id
	}
	return m.Sender
}

func (r *ixRender) fwdLabel(m *ixMsg) string {
	if m.Fwd == "" {
		return ""
	}
	if p, ok := r.peers[m.Fwd]; ok && p.Name != "" {
		return p.Name
	}
	if m.Fwd == "hidden" {
		return ""
	}
	return m.Fwd
}

// others lists the other places a file appears.
func (r *ixRender) others(f *ixFile, g *ixGroup, m *ixMsg) []ixPlace {
	var out []ixPlace
	for _, p := range r.shared[f.Key] {
		if p.GroupID == g.ID && p.MessageID == m.ID {
			continue
		}
		out = append(out, p)
	}
	return out
}

// placeLink is a link (from a page one folder deep) to a message elsewhere.
func (r *ixRender) placeLink(p ixPlace, from *ixGroup) (label, href string) {
	og := from
	if p.GroupID != from.ID {
		og = r.layoutOf(p.GroupID)
	}
	if og == nil {
		return "chat " + p.GroupID + " #" + strconv.Itoa(p.MessageID), ""
	}
	page := og.PageOf[p.MessageID]
	if page == "" {
		page = "index.html"
	}
	label = "#" + strconv.Itoa(p.MessageID)
	if og != from {
		label = og.Name + " " + label
		if m := og.ByID[p.MessageID]; m != nil && og.Forum {
			label = og.placeName(m) + " #" + strconv.Itoa(p.MessageID)
		}
	} else if m := og.ByID[p.MessageID]; m != nil && og.Forum {
		label = og.topicTitle(m.TopicID) + " " + label
	}
	return label, "../" + og.Dir + "/" + page + "#m" + strconv.Itoa(p.MessageID)
}

func (r *ixRender) alsoHTML(f *ixFile, g *ixGroup, m *ixMsg) string {
	others := r.others(f, g, m)
	if len(others) == 0 {
		return ""
	}
	var parts []string
	for i, p := range others {
		if i == 3 {
			break
		}
		label, href := r.placeLink(p, g)
		if href == "" {
			parts = append(parts, ixEsc(label))
		} else {
			parts = append(parts, `<a href="`+ixEsc(href)+`">`+ixEsc(label)+`</a>`)
		}
	}
	more := ""
	if len(others) > 3 {
		more = fmt.Sprintf(" +%d more", len(others)-3)
	}
	return `<span class="also"><span class="badge">also in</span> ` + strings.Join(parts, ", ") + more + `</span>`
}

func (r *ixRender) renderPost(g *ixGroup, p *ixPost, edits map[int][]ixEdit) string {
	m := p.First
	var bits []string
	if m.Date > 0 {
		bits = append(bits, ixEsc(ixLocal(m.Date)))
	} else {
		bits = append(bits, "date unknown")
	}
	if m.Captured {
		bits = append(bits, ixEsc(r.senderLabel(g, m)))
	}
	if m.Author != "" {
		bits = append(bits, "Author: "+ixEsc(m.Author))
	}
	if len(p.Msgs) > 1 {
		bits = append(bits, fmt.Sprintf("album of %d", len(p.Msgs)))
	}
	if m.Fwd != "" {
		if name := r.fwdLabel(m); name != "" {
			bits = append(bits, "forwarded from "+ixEsc(name))
		} else {
			bits = append(bits, "forwarded")
		}
	}
	if m.Reply > 0 {
		if page := g.PageOf[m.Reply]; page != "" {
			bits = append(bits, fmt.Sprintf(`reply to <a href="%s#m%d">#%d</a>`, ixEsc(page), m.Reply, m.Reply))
		} else {
			bits = append(bits, fmt.Sprintf("reply to #%d (not archived)", m.Reply))
		}
	}
	bits = append(bits, fmt.Sprintf(`<a href="#m%d">#%d</a>`, m.ID, m.ID))
	deleted := false
	for _, x := range p.Msgs {
		if x.Deleted > 0 {
			deleted = true
		}
	}
	if deleted {
		bits = append(bits, `<span class="badge bad">deleted on Telegram</span>`)
	}
	if !m.Captured {
		bits = append(bits, `<span class="badge warn">message text not captured yet</span>`)
	}
	var anchors strings.Builder
	for _, x := range p.Msgs[1:] {
		fmt.Fprintf(&anchors, `<span id="m%d"></span>`, x.ID)
	}
	var texts []string
	var media strings.Builder
	var editHTML strings.Builder
	for _, x := range p.Msgs {
		if x.Text != "" {
			texts = append(texts, x.Text)
		}
		for _, f := range x.Files {
			h := ixEsc(r.site.fileHref(f, 1))
			var el string
			switch f.Type {
			case "photo":
				el = `<a href="` + h + `" target="_blank"><img loading="lazy" decoding="async" src="` + ixEsc(r.site.thumbSrc(f, 1)) + `" alt="` + ixEsc(f.Name) + `"></a>`
			case "video":
				poster := ""
				if !r.site.static {
					poster = ` poster="` + ixEsc(r.site.thumbSrc(f, 1)) + `"`
				}
				el = `<video preload="none" controls` + poster + ` src="` + h + `"></video><span class="muted"><a href="` + h + `" target="_blank">` + ixEsc(f.Name) + `</a> · ` + ixSize(f.Size) + `</span>`
			default:
				el = `<a class="file" href="` + h + `" target="_blank">` + ixEsc(f.Name) + ` · ` + ixSize(f.Size) + `</a>`
			}
			media.WriteString(`<div class="item">` + el + r.alsoHTML(f, g, x) + `</div>`)
		}
		switch x.Status {
		case "missing":
			fmt.Fprintf(&media, `<span class="note missing">Not downloaded yet: %s (message #%d)</span>`, ixEsc(ixWord(x.MediaType)), x.ID)
		case "skipped":
			fmt.Fprintf(&media, `<span class="note skipped">%s not downloaded (turned off for this group)</span>`, ixEsc(ixWord(x.MediaType)))
		}
		if list := edits[x.ID]; len(list) > 0 {
			fmt.Fprintf(&editHTML, `<details class="edits"><summary>Edited — %d earlier version(s)</summary>`, len(list))
			for _, e := range list {
				when := "before the first recorded edit"
				if e.EditDate > 0 {
					when = "until " + ixLocal(e.EditDate)
				}
				fmt.Fprintf(&editHTML, `<div>%s</div><pre>%s</pre>`, ixEsc(when), ixEsc(e.Text))
			}
			editHTML.WriteString(`</details>`)
		}
	}
	class := "post"
	if deleted {
		class += " deleted"
	}
	out := fmt.Sprintf(`<article class="%s" id="m%d">%s<header>%s</header>`, class, m.ID, anchors.String(), strings.Join(bits, " · "))
	if len(texts) > 0 {
		out += `<div class="text">` + ixLinkify(strings.Join(texts, "\n\n")) + `</div>`
	}
	if media.Len() > 0 {
		out += `<div class="media">` + media.String() + `</div>`
	}
	return out + editHTML.String() + `</article>`
}

func ixPagesIn(g *ixGroup, topic int) []*ixPage {
	var out []*ixPage
	for _, pg := range g.Pages {
		if pg.Topic == topic {
			out = append(out, pg)
		}
	}
	return out
}

func ixMonthLinks(pages []*ixPage) string {
	var b strings.Builder
	for _, pg := range pages {
		label := pg.Key
		if pg.Parts > 1 {
			label += fmt.Sprintf(" (%d)", pg.Part)
		}
		fmt.Fprintf(&b, `<a href="%s">%s</a>`, ixEsc(pg.File), ixEsc(label))
	}
	return b.String()
}

// renderGroupPage renders one post page of a group.
func (r *ixRender) renderGroupPage(g *ixGroup, idx int) []byte {
	pg := g.Pages[idx]
	cat := ixCategoryName(g)
	var prev, next *ixPage
	if idx > 0 && g.Pages[idx-1].Topic == pg.Topic {
		prev = g.Pages[idx-1]
	}
	if idx+1 < len(g.Pages) && g.Pages[idx+1].Topic == pg.Topic {
		next = g.Pages[idx+1]
	}
	label := pg.Key
	if pg.Parts > 1 {
		label += fmt.Sprintf(" (part %d of %d)", pg.Part, pg.Parts)
	}
	if g.Forum {
		label = g.topicTitle(pg.Topic) + " — " + label
	}
	nav := `<div class="nav"><a href="../index.html">All groups</a><a href="../c_` + ixSlug(cat) + `/index.html">` + ixEsc(cat) + `</a><a href="index.html">` + ixEsc(g.Name) + `</a>`
	if prev != nil {
		nav += `<a href="` + ixEsc(prev.File) + `">← Older</a>`
	}
	if next != nil {
		nav += `<a href="` + ixEsc(next.File) + `">Newer →</a>`
	}
	nav += `</div>`
	var ids []int
	for _, p := range pg.Posts {
		for _, m := range p.Msgs {
			if m.Edits > 0 {
				ids = append(ids, m.ID)
			}
		}
	}
	var edits map[int][]ixEdit
	if len(ids) > 0 && r.edits != nil {
		edits = r.edits(g.ID, ids)
	}
	var b strings.Builder
	b.WriteString(nav + `<h1>` + ixEsc(g.Name+" — "+label) + `</h1><p class="muted">`)
	if g.Forum {
		b.WriteString(`Topic: <b>` + ixEsc(g.topicTitle(pg.Topic)) + `</b> · `)
	}
	fmt.Fprintf(&b, `%d posts, oldest first. Times are Toronto time.</p>`, len(pg.Posts))
	for _, p := range pg.Posts {
		b.WriteString(r.renderPost(g, p, edits))
	}
	b.WriteString(nav)
	return r.page(g.Name+" — "+label, 1, b.String())
}

func ixCategoryName(g *ixGroup) string {
	if g.Category == "" {
		return ixUncategorised
	}
	return g.Category
}

func ixPct(c ixCounts) string {
	if c.Downloaded+c.Missing == 0 {
		return "100%"
	}
	return fmt.Sprintf("%.1f%%", float64(int(1000*float64(c.Downloaded)/float64(c.Downloaded+c.Missing)))/10)
}

func (r *ixRender) renderGroupIndex(g *ixGroup, lastWalk int64) []byte {
	c := g.Counts
	cat := ixCategoryName(g)
	nav := `<div class="nav"><a href="../index.html">All groups</a><a href="../c_` + ixSlug(cat) + `/index.html">` + ixEsc(cat) + `</a></div>`
	var b strings.Builder
	b.WriteString(nav + `<h1>` + ixEsc(g.Name) + `</h1>`)
	fmt.Fprintf(&b, `<p class="muted">Category: %s · Chat ID %s · %s to %s (Toronto time)`, ixEsc(cat), ixEsc(g.ID), ixEsc(ixLocal(g.First)), ixEsc(ixLocal(g.Last)))
	if lastWalk > 0 {
		fmt.Fprintf(&b, ` · last full rescan %s`, ixEsc(ixLocal(int(lastWalk/1000))))
	}
	b.WriteString(`</p>`)
	if c.Captured == 0 {
		b.WriteString(`<p class="warn"><b>No messages captured yet for this group.</b> The files are listed, but text, dates and senders appear after the group is rescanned (Archive → Rescan queue).</p>`)
	}
	row := func(label string, n int, class string) {
		fmt.Fprintf(&b, `<tr><th>%s</th><td class="n %s">%s</td></tr>`, label, class, ixNum(n))
	}
	b.WriteString(`<table>`)
	row("Messages", c.Messages, "")
	row("…with text, dates and senders captured", c.Captured, "")
	row("Posts (albums count once)", c.Posts, "")
	row("Messages with an attachment", c.WithMedia, "")
	row("…downloaded", c.Downloaded, "ok")
	row("…missing (should have been downloaded)", c.Missing, map[bool]string{true: "bad", false: ""}[c.Missing > 0])
	row("…turned off for this group", c.Skipped, "")
	row("Text-only messages", c.TextOnly, "")
	row("Deleted on Telegram (copy kept)", c.Deleted, map[bool]string{true: "warn", false: ""}[c.Deleted > 0])
	row("Edited (earlier versions kept)", c.Edited, "")
	row("Files also posted elsewhere", c.Shared, "")
	row("Files with no archived message", c.Extra, map[bool]string{true: "warn", false: ""}[c.Extra > 0])
	row("Size different from Telegram's", c.SizeMismatch, map[bool]string{true: "warn", false: ""}[c.SizeMismatch > 0])
	b.WriteString(`</table><p class="muted">Spreadsheets: <a href="posts.csv">posts.csv</a> · <a href="files.csv">files.csv</a> · <a href="missing.csv">missing.csv</a> · <a href="extra.csv">extra.csv</a> · <a href="size_mismatch.csv">size_mismatch.csv</a> · <a href="sha256.txt">sha256.txt</a> (SHA-256 of every file, for checking them later)</p>`)
	if g.Forum {
		b.WriteString(`<h2>Topics</h2><p class="muted">This group is split into topics. Each topic has its own pages, by month.</p><table><tr><th>Topic</th><th>Posts</th><th>Files</th><th>Missing</th><th>Months</th></tr>`)
		var ids []int
		seen := map[int]bool{}
		for _, pg := range g.Pages {
			if !seen[pg.Topic] {
				seen[pg.Topic] = true
				ids = append(ids, pg.Topic)
			}
		}
		for _, id := range ids {
			posts, files, miss := 0, 0, 0
			for _, p := range g.Posts {
				if p.Topic != id {
					continue
				}
				posts++
				for _, m := range p.Msgs {
					files += len(m.Files)
					if m.Status == "missing" {
						miss++
					}
				}
			}
			cls := ""
			if miss > 0 {
				cls = "bad"
			}
			fmt.Fprintf(&b, `<tr><td><b>%s</b></td><td class="n">%s</td><td class="n">%s</td><td class="n %s">%s</td><td><div class="months">%s</div></td></tr>`, ixEsc(g.topicTitle(id)), ixNum(posts), ixNum(files), cls, ixNum(miss), ixMonthLinks(ixPagesIn(g, id)))
		}
		b.WriteString(`</table>`)
	} else {
		links := ixMonthLinks(g.Pages)
		if links == "" {
			links = `<span class="muted">No messages.</span>`
		}
		b.WriteString(`<h2>Months</h2><div class="months">` + links + `</div>`)
	}
	if len(g.Missing) > 0 {
		b.WriteString(`<h2>Missing files</h2><table><tr><th>Message</th><th>Date</th><th>Type</th><th>Name</th></tr>`)
		for i, m := range g.Missing {
			if i == 200 {
				break
			}
			typ := ixWord(m.MediaType)
			if g.Forum {
				typ += " · " + g.topicTitle(m.TopicID)
			}
			fmt.Fprintf(&b, `<tr><td><a href="%s#m%d">#%d</a></td><td>%s</td><td>%s</td><td>%s</td></tr>`, ixEsc(g.PageOf[m.ID]), m.ID, m.ID, ixEsc(ixLocal(m.Date)), ixEsc(typ), ixEsc(m.MediaName))
		}
		b.WriteString(`</table>`)
		if len(g.Missing) > 200 {
			fmt.Fprintf(&b, `<p class="muted">Showing 200 of %d; all are in missing.csv.</p>`, len(g.Missing))
		}
	}
	return r.page(g.Name, 1, b.String())
}

func (r *ixRender) topicCol(g *ixGroup, m *ixMsg) string {
	if g.Forum {
		return g.topicTitle(m.TopicID)
	}
	return ""
}

// groupFiles returns the spreadsheets and checksum list of a group.
func (r *ixRender) groupFiles(g *ixGroup) map[string][]byte {
	out := map[string][]byte{}
	posts := [][]string{{"message_id", "album_id", "topic_id", "topic", "date_utc", "date_toronto", "sender", "author", "forwarded_from", "reply_to", "attachment", "status", "deleted_on_telegram", "edited_versions", "files", "text"}}
	for _, m := range g.Msgs {
		var paths []string
		for _, f := range m.Files {
			paths = append(paths, f.Path)
		}
		deleted := ""
		if m.Deleted > 0 {
			deleted = time.UnixMilli(m.Deleted).In(ixTZ).Format("2006-01-02 15:04:05")
		}
		topicID := ""
		if m.TopicID > 0 {
			topicID = strconv.Itoa(m.TopicID)
		}
		reply := ""
		if m.Reply > 0 {
			reply = strconv.Itoa(m.Reply)
		}
		sender := ""
		if m.Captured {
			sender = r.senderLabel(g, m)
		}
		attachment := ""
		if m.MediaType != "" {
			attachment = ixWord(m.MediaType)
		}
		posts = append(posts, []string{strconv.Itoa(m.ID), m.Grouped, topicID, r.topicCol(g, m), ixUTC(m.Date), ixLocal(m.Date), sender, m.Author, r.fwdLabel(m), reply, attachment, m.Status, deleted, strconv.Itoa(m.Edits), strings.Join(paths, " | "), m.Text})
	}
	out["posts.csv"] = ixCSVBytes(posts)
	files := [][]string{{"message_id", "album_id", "topic", "date_toronto", "file_type", "file_name", "path_in_downloads_folder", "size_bytes", "sha256", "also_posted_in"}}
	var sums strings.Builder
	for _, m := range g.Msgs {
		for _, f := range m.Files {
			var also []string
			for _, p := range r.others(f, g, m) {
				label, _ := r.placeLink(p, g)
				also = append(also, label)
			}
			files = append(files, []string{strconv.Itoa(m.ID), m.Grouped, r.topicCol(g, m), ixLocal(m.Date), f.Type, f.Name, f.Path, strconv.FormatInt(f.Size, 10), strings.ToLower(f.Hash), strings.Join(also, " | ")})
			if len(f.Hash) == 64 {
				fmt.Fprintf(&sums, "%s  %s\n", strings.ToLower(f.Hash), f.Path)
			}
		}
	}
	out["files.csv"] = ixCSVBytes(files)
	out["sha256.txt"] = []byte(sums.String())
	missing := [][]string{{"message_id", "album_id", "topic", "date_toronto", "attachment", "name_on_telegram", "size_on_telegram"}}
	for _, m := range g.Missing {
		missing = append(missing, []string{strconv.Itoa(m.ID), m.Grouped, r.topicCol(g, m), ixLocal(m.Date), ixWord(m.MediaType), m.MediaName, strconv.FormatInt(m.MediaSize, 10)})
	}
	out["missing.csv"] = ixCSVBytes(missing)
	extra := [][]string{{"message_id", "file_type", "path_in_downloads_folder", "size_bytes", "sha256"}}
	for _, f := range g.Extra {
		extra = append(extra, []string{strconv.Itoa(f.MessageID), f.Type, f.Path, strconv.FormatInt(f.Size, 10), strings.ToLower(f.Hash)})
	}
	out["extra.csv"] = ixCSVBytes(extra)
	mism := [][]string{{"message_id", "path_in_downloads_folder", "size_on_disk", "size_on_telegram"}}
	for _, pair := range g.Mismatch {
		mism = append(mism, []string{strconv.Itoa(pair.Msg.ID), pair.File.Path, strconv.FormatInt(pair.File.Size, 10), strconv.FormatInt(pair.Msg.MediaSize, 10)})
	}
	out["size_mismatch.csv"] = ixCSVBytes(mism)
	return out
}

// ---------- categories ----------

type ixCategory struct {
	Name    string
	Slug    string
	Groups  []*ixGroup
	Posts   int
	Files   int
	Missing int
	Unique  int
	SharedU int
}

type ixTile struct {
	f      *ixFile
	date   int
	places []ixPlace
	first  ixPlace
}

func ixCategories(groups []*ixGroup) []*ixCategory {
	byName := map[string]*ixCategory{}
	var cats []*ixCategory
	for _, g := range groups {
		name := ixCategoryName(g)
		c := byName[name]
		if c == nil {
			c = &ixCategory{Name: name}
			byName[name] = c
			cats = append(cats, c)
		}
		c.Groups = append(c.Groups, g)
		c.Posts += g.Counts.Posts
		c.Files += g.Counts.Files
		c.Missing += g.Counts.Missing
	}
	sort.Slice(cats, func(i, j int) bool {
		ui, uj := cats[i].Name == ixUncategorised, cats[j].Name == ixUncategorised
		if ui != uj {
			return uj
		}
		return strings.ToLower(cats[i].Name) < strings.ToLower(cats[j].Name)
	})
	used := map[string]bool{}
	for _, c := range cats {
		slug := ixSlug(c.Name)
		base := slug
		for n := 2; used[slug]; n++ {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		used[slug] = true
		c.Slug = slug
		sort.Slice(c.Groups, func(i, j int) bool {
			return ixNatLess(c.Groups[i].Name, c.Groups[j].Name)
		})
		c.tiles() // fills Unique and SharedU
	}
	return cats
}

// ixNatLess sorts names case-insensitively with numbers in number order
// (sports-2 before sports-10).
func ixNatLess(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	for a != "" && b != "" {
		da, db := ixLeadingDigits(a), ixLeadingDigits(b)
		if da != "" && db != "" {
			na, _ := strconv.Atoi(da)
			nb, _ := strconv.Atoi(db)
			if na != nb {
				return na < nb
			}
			a, b = a[len(da):], b[len(db):]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func ixLeadingDigits(s string) string {
	i := 0
	for i < len(s) && i < 9 && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

// tiles is every file of the category once, oldest first.
func (c *ixCategory) tiles() []*ixTile {
	byKey := map[string]*ixTile{}
	var list []*ixTile
	for _, g := range c.Groups {
		for _, m := range g.Msgs {
			for _, f := range m.Files {
				t := byKey[f.Key]
				place := ixPlace{GroupID: g.ID, MessageID: m.ID}
				if t == nil {
					t = &ixTile{f: f, date: m.Date, first: place}
					byKey[f.Key] = t
					list = append(list, t)
				}
				t.places = append(t.places, place)
				if m.Date > 0 && (t.date == 0 || m.Date < t.date) {
					t.date, t.first = m.Date, place
				}
			}
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].date != list[j].date {
			return list[i].date < list[j].date
		}
		return list[i].first.MessageID < list[j].first.MessageID
	})
	c.Unique = len(list)
	c.SharedU = 0
	for _, t := range list {
		if len(t.places) > 1 {
			c.SharedU++
		}
	}
	return list
}

type ixTilePage struct {
	Key, File   string
	Part, Parts int
	Tiles       []*ixTile
}

func (c *ixCategory) tilePages() []*ixTilePage {
	list := c.tiles()
	months := map[string][]*ixTile{}
	var keys []string
	for _, t := range list {
		k := ixMonth(t.date)
		if _, ok := months[k]; !ok {
			keys = append(keys, k)
		}
		months[k] = append(months[k], t)
	}
	sort.Strings(keys)
	var pages []*ixTilePage
	for _, k := range keys {
		l := months[k]
		n := (len(l) + ixTilesPerPage - 1) / ixTilesPerPage
		for i := 0; i < n; i++ {
			file := "media-" + k + ".html"
			if n > 1 {
				file = fmt.Sprintf("media-%s-%d.html", k, i+1)
			}
			pages = append(pages, &ixTilePage{Key: k, File: file, Part: i + 1, Parts: n, Tiles: l[i*ixTilesPerPage : min(len(l), (i+1)*ixTilesPerPage)]})
		}
	}
	return pages
}

func (r *ixRender) groupRow(g *ixGroup, depth string) string {
	c := g.Counts
	topics := ""
	if g.Forum {
		topics = fmt.Sprintf(` <span class="muted">· %d topics</span>`, c.Topics)
	}
	captured := ixNum(c.Captured)
	if c.Captured == 0 {
		captured = `<span class="warn">not yet</span>`
	}
	missCls := "ok"
	if c.Missing > 0 {
		missCls = "bad"
	}
	dates := ""
	if g.First > 0 {
		dates = ixLocal(g.First)[:10] + " – " + ixLocal(g.Last)[:10]
	}
	return fmt.Sprintf(`<tr><td><a href="%s%s/index.html">%s</a>%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n %s">%s</td><td class="n">%s</td><td class="n">%s</td><td class="n">%s</td><td class="muted">%s</td></tr>`,
		depth, g.Dir, ixEsc(g.Name), topics, ixNum(c.Messages), captured, ixNum(c.Files), missCls, ixNum(c.Missing), ixPct(c), ixNum(c.Shared), ixNum(c.Deleted), ixEsc(dates))
}

const ixGroupHead = `<table><tr><th>Group</th><th>Messages</th><th>Captured</th><th>Files</th><th>Missing</th><th>Complete</th><th>Also posted elsewhere</th><th>Deleted</th><th>Dates</th></tr>`

func (r *ixRender) renderCategory(c *ixCategory) map[string][]byte {
	out := map[string][]byte{}
	pages := c.tilePages()
	for i, pg := range pages {
		nav := `<div class="nav"><a href="../index.html">All groups</a><a href="index.html">` + ixEsc(c.Name) + `</a>`
		if i > 0 {
			nav += `<a href="` + pages[i-1].File + `">← Older</a>`
		}
		if i+1 < len(pages) {
			nav += `<a href="` + pages[i+1].File + `">Newer →</a>`
		}
		nav += `</div>`
		label := pg.Key
		if pg.Parts > 1 {
			label += fmt.Sprintf(" (part %d of %d)", pg.Part, pg.Parts)
		}
		var b strings.Builder
		b.WriteString(nav + `<h1>` + ixEsc(c.Name) + ` — media first posted ` + ixEsc(label) + `</h1><p class="muted">Each file once. Files posted more than once show every place they appeared.</p><div class="grid">`)
		for _, t := range pg.Tiles {
			f := t.f
			h := ixEsc(r.site.fileHref(f, 1))
			var thumb string
			switch {
			case f.Type == "photo" || (f.Type == "video" && !r.site.static):
				thumb = `<a class="thumb" href="` + h + `" target="_blank"><img loading="lazy" decoding="async" src="` + ixEsc(r.site.thumbSrc(f, 1)) + `" alt="` + ixEsc(f.Name) + `"></a>`
			default:
				icon := ""
				if f.Type == "video" {
					icon = "▶ "
				}
				thumb = `<a class="thumb" href="` + h + `" target="_blank"><div class="ph">` + icon + ixEsc(f.Name) + `<br>` + ixSize(f.Size) + `</div></a>`
			}
			var where []string
			for i, p := range t.places {
				if i == 3 {
					break
				}
				og := r.layoutOf(p.GroupID)
				label := "#" + strconv.Itoa(p.MessageID)
				href := ""
				if og != nil {
					m := og.ByID[p.MessageID]
					name := og.Name
					if m != nil {
						name = og.placeName(m)
					}
					label = name + " #" + strconv.Itoa(p.MessageID)
					page := og.PageOf[p.MessageID]
					if page == "" {
						page = "index.html"
					}
					href = "../" + og.Dir + "/" + page + "#m" + strconv.Itoa(p.MessageID)
				}
				if href != "" {
					where = append(where, `<a href="`+ixEsc(href)+`">`+ixEsc(label)+`</a>`)
				} else {
					where = append(where, ixEsc(label))
				}
			}
			more := ""
			if len(t.places) > 3 {
				more = fmt.Sprintf("<br>+%d more", len(t.places)-3)
			}
			badge := ""
			if len(t.places) > 1 {
				badge = fmt.Sprintf(`<span><span class="badge">%d posts</span></span>`, len(t.places))
			}
			date := ""
			if t.date > 0 {
				date = ixLocal(t.date)[:10]
			}
			b.WriteString(`<div class="tile">` + thumb + `<div class="cap">` + badge + `<span>` + strings.Join(where, "<br>") + more + `</span><span>` + ixEsc(date) + `</span></div></div>`)
		}
		b.WriteString(`</div>` + nav)
		out[pg.File] = r.page(c.Name+" — media "+label, 1, b.String())
	}
	var rows strings.Builder
	for _, g := range c.Groups {
		rows.WriteString(r.groupRow(g, "../"))
	}
	var months strings.Builder
	for _, pg := range pages {
		label := pg.Key
		if pg.Parts > 1 {
			label += fmt.Sprintf(" (%d)", pg.Part)
		}
		fmt.Fprintf(&months, `<a href="%s">%s</a>`, pg.File, ixEsc(label))
	}
	if months.Len() == 0 {
		months.WriteString(`<span class="muted">No files yet.</span>`)
	}
	body := `<div class="nav"><a href="../index.html">All groups</a></div><h1>` + ixEsc(c.Name) + `</h1>` +
		fmt.Sprintf(`<p class="muted">%d group(s) · %s files in posts · <b>%s unique files</b> · %s posted more than once</p>`, len(c.Groups), ixNum(c.Files), ixNum(c.Unique), ixNum(c.SharedU)) +
		ixGroupHead + rows.String() + `</table><h2>All media, each file once</h2><div class="months">` + months.String() + `</div>`
	out["index.html"] = r.page(c.Name, 1, body)
	return out
}

func (r *ixRender) renderRoot(cats []*ixCategory) []byte {
	groups := 0
	var cards, sections strings.Builder
	for _, c := range cats {
		groups += len(c.Groups)
		missCls := "ok"
		if c.Missing > 0 {
			missCls = "bad"
		}
		fmt.Fprintf(&cards, `<a class="card" href="c_%s/index.html"><b>%s</b><div class="muted">%d group(s) · %s posts</div><div>%s unique files · <span class="%s">%s missing</span></div><div class="muted">%s posted more than once</div></a>`,
			c.Slug, ixEsc(c.Name), len(c.Groups), ixNum(c.Posts), ixNum(c.Unique), missCls, ixNum(c.Missing), ixNum(c.SharedU))
		var rows strings.Builder
		for _, g := range c.Groups {
			rows.WriteString(r.groupRow(g, ""))
		}
		fmt.Fprintf(&sections, `<h2>%s</h2><p class="muted"><a href="c_%s/index.html">All media in this category, each file once →</a></p>%s%s</table>`, ixEsc(c.Name), c.Slug, ixGroupHead, rows.String())
	}
	plural := "categories"
	if len(cats) == 1 {
		plural = "category"
	}
	body := `<h1>Telegram backup index</h1>` +
		fmt.Sprintf(`<p class="muted">Updated %s (Toronto time) · %d group(s) in %d %s · Files stay where the downloader saved them; these pages only link to them.</p>`, ixEsc(r.generated.In(ixTZ).Format("2006-01-02 15:04")), groups, len(cats), plural) +
		`<div class="cards">` + cards.String() + `</div>` + sections.String()
	return r.page("Telegram backup index", 0, body)
}
