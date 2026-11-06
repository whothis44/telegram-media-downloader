package app

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/telegram"
)

// ix.12.7: a file not downloaded whose duplicate set has a saved copy points
// at that copy, so the browser can open it; with no saved copy it points at
// nothing, and a saved file never points elsewhere.
func TestIx127MissingFileLinksToItsSavedCopy(t *testing.T) {
	m := &ixModel{
		Items: []ixMItem{
			{Group: "g1", Msg: 1, Type: "video", Status: "missing", Ident: "doc:1:100", Cluster: 0},
			{ID: 7, Group: "g2", Msg: 5, Type: "video", Status: "ok", Key: "h:aa", Path: "g2/a.mp4", Cluster: 0},
			{Group: "g1", Msg: 2, Type: "photo", Status: "missing", Ident: "photo:2:50", Cluster: 1},
			{Group: "g1", Msg: 3, Type: "video", Status: "held", Like: "h:aa", Cluster: 0},
		},
		Clusters: []ixMCluster{{Canon: 1, Items: []int32{0, 1, 3}}, {Canon: 2, Items: []int32{2}}},
	}
	j := m.jitem(0)
	if j.Like == nil || j.Like.Key != "h:aa" || j.Like.Group != "g2" || j.Like.Msg != 5 || j.Like.Src == "" {
		t.Fatalf("missing file: saved copy %+v, want h:aa in g2:5 with a source", j.Like)
	}
	if j.Like.Like != nil {
		t.Fatal("the saved copy points further")
	}
	if j := m.jitem(2); j.Like != nil {
		t.Fatalf("no copy is saved, but it points at %+v", j.Like)
	}
	if j := m.jitem(1); j.Like != nil {
		t.Fatal("a saved file points at another copy")
	}
	if j := m.jitem(3); j.Like == nil || j.Like.Key != "h:aa" {
		t.Fatal("a held file lost its likely copy")
	}
	// The best copy is not saved (a keeper chosen, or a bigger missing one):
	// another saved copy of the set is used.
	m.Clusters[0].Canon = 0
	if j := m.jitem(0); j.Like == nil || j.Like.Key != "h:aa" {
		t.Fatalf("best copy not saved: got %+v, want the saved one", j.Like)
	}
}

// ix.12.7: a forum chat lists its topics (stored names, General for unmarked
// messages, "Topic #id" for a name never seen), and the feed shows one topic.
func TestIx127FeedFiltersByForumTopic(t *testing.T) {
	a, ctx := ixTestApp(t)
	if _, err := a.db.Writer.ExecContext(ctx, `INSERT INTO ix_topics(group_id,topic_id,title,title_date) VALUES('f',7,'Invented Topic',1),('plain',9,'Unused',1)`); err != nil {
		t.Fatal(err)
	}
	m := &ixModel{GroupName: map[string]string{"f": "Forum", "plain": "Plain"}}
	add := func(g string, msg, topic int, orphan bool) {
		pi := int32(len(m.Posts))
		ii := int32(len(m.Items))
		m.Items = append(m.Items, ixMItem{Group: g, Msg: msg, Type: "photo", Status: "missing", Post: pi, Cluster: ii})
		m.Clusters = append(m.Clusters, ixMCluster{Canon: ii, Items: []int32{ii}})
		m.Posts = append(m.Posts, ixMPost{Idx: pi, Group: g, Msg: msg, Date: int64(msg), Origin: int64(msg), Topic: topic, Orphan: orphan, Family: -1, Items: []int32{ii}})
	}
	add("f", 1, 0, false) // General: no topic mark
	add("f", 2, 1, false) // General
	add("f", 3, 7, false)
	add("f", 4, 7, false)
	add("f", 5, 7, false)
	add("f", 6, 12, false) // a topic whose name was never seen
	add("f", 7, 0, true)   // message never captured: no known topic
	add("plain", 1, 0, false)
	add("plain", 2, 1, false)

	topics := a.ixBrowseTopics(ctx, m)
	if _, ok := topics["plain"]; ok {
		t.Fatalf("a chat without topics got a topic menu: %+v", topics["plain"])
	}
	got := fmt.Sprint(topics["f"])
	if want := "[{7 Invented Topic 3} {1 General 2} {12 Topic #12 1}]"; got != want {
		t.Fatalf("forum topics %s, want %s", got, want)
	}
	count := func(q string) int {
		r := httptest.NewRequest("GET", "/api/ix/browse/feed?view=posts&"+q, nil)
		f := ixParseFeed(r)
		n := 0
		for pi := range m.Posts {
			if m.postMatches(int32(pi), f, nil) {
				n++
			}
		}
		return n
	}
	for q, want := range map[string]int{"group=f": 7, "group=f&topic=7": 3, "group=f&topic=1": 2, "group=f&topic=12": 1, "group=f&topic=99": 0, "topic=7": 9} {
		if n := count(q); n != want {
			t.Errorf("%s: %d posts, want %d", q, n, want)
		}
	}
}

// ix.12.7: "speed-limited" says which waits: the speed limit
// (FLOOD_PREMIUM_WAIT), or only ordinary FLOOD_WAITs, which the label called
// "speed limit" too (seen 11-06: 0 speed-limit waits in the hour).
func TestIx127DownloadStateSaysWhichWaits(t *testing.T) {
	st := ixDlState{EngineRunning: true, Ready: 3, Downloading: 2, OtherWaits: 4, QuietMin: 1}
	ixDlDecide(&st)
	if st.Verdict != ixDlSpeedLimited || st.Waits != "plain" {
		t.Fatalf("plain waits: %s %q", st.Verdict, st.Waits)
	}
	if s, _ := ixDlAdvice(st); !strings.Contains(s, "not its speed limit") {
		t.Fatalf("advice: %s", s)
	}
	st.SpeedWaits = 1
	ixDlDecide(&st)
	if st.Waits != "speed-limit" {
		t.Fatalf("speed limit: %q", st.Waits)
	}
	if s, _ := ixDlAdvice(st); !strings.Contains(s, "speed limit sets the pace") {
		t.Fatalf("advice: %s", s)
	}
	st.SpeedWaits, st.OtherWaits = 0, 0
	ixDlDecide(&st)
	if st.Verdict != ixDlDownloading || st.Waits != "" {
		t.Fatalf("no waits: %s %q", st.Verdict, st.Waits)
	}
}

// ix.12.7: the [process] lines measure this process: CPU and bytes.
func TestIx127ProcessLine(t *testing.T) {
	ixProcRecord()
	buf := make([]byte, 1<<20)
	for i := 0; i < 20; i++ { // some CPU
		for j := range buf {
			buf[j] = byte(i + j)
		}
	}
	lines := strings.Join(ixProcessDiag(time.Now()), "\n")
	if !strings.Contains(lines, "CPU") || !strings.Contains(lines, "read") || strings.Contains(lines, "no samples yet") {
		t.Fatalf("process lines: %s", lines)
	}
	if s, ok := ixProcNow(); !ok || s.cpu <= 0 {
		t.Fatalf("no CPU time: %+v %v", s, ok)
	}
}

// ix.12.7: the hour line's per-server part.
func TestIx127HourLineByServer(t *testing.T) {
	h := telegram.PaceHour{DCCutsWait: map[int]int{4: 2}, DCCutsSlow: map[int]int{4: 1}, DCTriesUp: map[int]int{2: 3},
		DCSamples: map[int]int{2: 10, 4: 10}, DCAtFloor: map[int]int{2: 5}, DCBelow: map[int]int{4: 4}}
	got := ixDCServerLine(h, map[int]int{2: 7, 5: 1})
	want := "DC2 7 files, tries up 3 down 0, at the 2-part floor 50%, below it 0%; DC4 0 files, cuts 3 (wait 2, slow 1), at the 2-part floor 0%, below it 40%; DC5 1 files"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
