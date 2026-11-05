package app

import "testing"

// A small invented model: each item is a chat's message of a file.
type ixCovFixture struct {
	group   string
	cluster int32
	key     string // saved file key ("": not downloaded)
	id      int64  // download id
	status  string
	ident   string
	size    int64
	msg     int
	typ     string
}

func ixCoverModel(items ...ixCovFixture) *ixModel {
	m := &ixModel{Posts: []ixMPost{{}}}
	top := int32(0)
	for _, i := range items {
		top = max(top, i.cluster)
	}
	m.Clusters = make([]ixMCluster, top+1)
	for n, i := range items {
		status := i.status
		if status == "" {
			status = "ok"
			if i.key == "" {
				status = "missing"
			}
		}
		typ := i.typ
		if typ == "" {
			typ = "video"
		}
		m.Items = append(m.Items, ixMItem{Group: i.group, Cluster: i.cluster, Key: i.key, ID: i.id, Status: status, Ident: i.ident, Size: i.size, Msg: i.msg, Type: typ})
		m.Clusters[i.cluster].Items = append(m.Clusters[i.cluster].Items, int32(n))
	}
	return m
}

// The handoff's example: sports-1 and sports-2 hold 100 each and 60 are the same
// Telegram files. Saving all of sports-1 covers those 60 in sports-2, leaving 40
// needed there, not 100; and the same 60 are not counted twice as content.
func TestIxCoverageSharedFilesCreditBothGroupsOnce(t *testing.T) {
	var items []ixCovFixture
	cluster := int32(0)
	saved := map[string]bool{}
	for i := 0; i < 100; i++ { // sports-1, all saved
		items = append(items, ixCovFixture{group: "s1", cluster: cluster, key: "k" + string(rune('A'+i%26)) + itoa(i), id: int64(i + 1), size: 10})
		saved["video\x00"+itoa(i)] = true
		if i < 60 { // the same Telegram file in sports-2, its row not done yet
			items = append(items, ixCovFixture{group: "s2", cluster: cluster, ident: "video:" + itoa(i) + ":10", size: 10})
		}
		cluster++
	}
	for i := 100; i < 140; i++ { // sports-2's own
		items = append(items, ixCovFixture{group: "s2", cluster: cluster, ident: "video:" + itoa(i) + ":10", size: 10})
		cluster++
	}
	m := ixCoverModel(items...)
	cov := ixCoverage(m, saved, nil)
	if c := cov["s1"]; c.Saved.Files != 100 || c.Needed.Files != 0 || c.Total.Files != 100 {
		t.Fatalf("s1 %+v", c)
	}
	c := cov["s2"]
	if c.Total.Files != 100 || c.Shared.Files != 60 || c.Needed.Files != 40 || c.Saved.Files != 0 {
		t.Fatalf("s2 %+v", c)
	}
	if c.Needed.Bytes != 400 || c.Shared.Bytes != 600 {
		t.Fatalf("bytes must be counted apart from files: %+v", c)
	}
	var info ixPrioritiesInfo
	ixDistinctTotals(m, saved, &info)
	if info.Distinct.Saved.Files != 100 || info.Distinct.Needed.Files != 40 {
		t.Fatalf("each file once: %+v", info.Distinct)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for ; i > 0; i /= 10 {
		s = string(rune('0'+i%10)) + s
	}
	return s
}

// Overlap is per file. A and B share a file, B and C share another: A and C
// share nothing, and nothing is inferred about a three-way intersection.
func TestIxCoveragePairwiseOverlapIsNotAThreeWayOne(t *testing.T) {
	saved := map[string]bool{"video\x001": true}
	m := ixCoverModel(
		ixCovFixture{group: "a", cluster: 0, key: "k1", id: 1, size: 10},
		ixCovFixture{group: "b", cluster: 0, ident: "video:1:10", size: 10},
		ixCovFixture{group: "b", cluster: 1, ident: "video:2:10", size: 10},
		ixCovFixture{group: "c", cluster: 1, ident: "video:2:10", size: 10},
		ixCovFixture{group: "c", cluster: 2, ident: "video:3:10", size: 10},
	)
	cov := ixCoverage(m, saved, nil)
	if c := cov["b"]; c.Shared.Files != 1 || c.Needed.Files != 1 {
		t.Fatalf("b %+v", c)
	}
	if c := cov["c"]; c.Shared.Files != 0 || c.Needed.Files != 2 {
		t.Fatalf("c: a file only a and b share must not cover c: %+v", c)
	}
	groups := []ixGroupCoverage{{GroupID: "b"}, {GroupID: "c"}}
	ixNeededAhead(m, groups, saved)
	if groups[0].NeededAhead.Files != 0 || groups[1].NeededAhead.Files != 1 {
		t.Fatalf("needed ahead %+v", groups)
	}
}

// Evidence stays evidence: a look-alike, a held copy and a re-upload compared on
// samples are never counted as saved, and equal size proves nothing.
func TestIxCoverageProbableIsNeverConfirmed(t *testing.T) {
	saved := map[string]bool{}
	substHow := map[string]string{
		"g:1": "sampled-parts",   // three samples agreed
		"g:2": "telegram-hashes", // a big video: start, middle and end hashes only
		"g:3": "telegram-hashes", // a photo: every part's hash
	}
	m := ixCoverModel(
		ixCovFixture{group: "o", cluster: 0, key: "k0", id: 1, size: 5 << 20},
		ixCovFixture{group: "g", cluster: 0, key: "k0", id: 2, status: "subst", msg: 1, size: 5 << 20},
		ixCovFixture{group: "o", cluster: 1, key: "k1", id: 3, size: 5 << 20},
		ixCovFixture{group: "g", cluster: 1, key: "k1", id: 4, status: "subst", msg: 2, size: 5 << 20},
		ixCovFixture{group: "o", cluster: 2, key: "k2", id: 5, size: 5 << 20, typ: "photo"},
		ixCovFixture{group: "g", cluster: 2, key: "k2", id: 6, status: "subst", msg: 3, size: 5 << 20, typ: "photo"},
		// Held as a probable copy of a saved file.
		ixCovFixture{group: "g", cluster: 3, status: "held", ident: "video:30:9", size: 9},
		// A look-alike of a saved file (same cluster), another Telegram file.
		ixCovFixture{group: "o", cluster: 4, key: "k4", id: 7, size: 9},
		ixCovFixture{group: "g", cluster: 4, ident: "video:40:9", size: 9},
	)
	c := ixCoverage(m, saved, substHow)["g"]
	if c.Probable.Files != 4 {
		t.Fatalf("probable %+v", c)
	}
	if c.Shared.Files != 1 {
		t.Fatalf("only the file compared on every part is confirmed: %+v", c)
	}
	if c.Saved.Files != 0 || c.Needed.Files != 0 {
		t.Fatalf("%+v", c)
	}
}

// A file deleted on Telegram cannot be fetched: it is not "still needed".
func TestIxCoverageLeavesOutWhatCannotBeFetched(t *testing.T) {
	m := ixCoverModel(ixCovFixture{group: "g", cluster: 0, ident: "video:1:9", size: 9})
	m.Posts = []ixMPost{{Deleted: true}}
	if c := ixCoverage(m, map[string]bool{}, nil)["g"]; c.Needed.Files != 0 || c.Total.Files != 0 {
		t.Fatalf("%+v", c)
	}
}

// The group that first saved a file has it as its own; a second message with its
// own catalogue entry counts as coming through the first group.
func TestIxCoverageOwnerIsTheFirstToSave(t *testing.T) {
	m := ixCoverModel(
		ixCovFixture{group: "late", cluster: 0, key: "k", id: 9, size: 7},
		ixCovFixture{group: "early", cluster: 0, key: "k", id: 2, size: 7},
	)
	cov := ixCoverage(m, map[string]bool{}, nil)
	if cov["early"].Saved.Files != 1 || cov["late"].Shared.Files != 1 || cov["late"].Saved.Files != 0 {
		t.Fatalf("%+v", cov)
	}
}

func TestIxIdentityOfMediaKey(t *testing.T) {
	for in, want := range map[string]string{
		"video:42:100": "video\x0042\x00100",
		"Video:42:100": "video\x0042\x00100",
		"":             "",
		"nonsense":     "",
		"a:b":          "",
	} {
		if got := ixIdentityOfMediaKey(in); got != want {
			t.Fatalf("%q -> %q", in, got)
		}
	}
	if ixIdentityFile("video\x0042\x00100") != "video\x0042" {
		t.Fatal("file of an identity")
	}
}

func TestIxSubstConfirmed(t *testing.T) {
	for _, c := range []struct {
		typ  string
		size int64
		how  string
		want bool
	}{
		{"photo", 5 << 20, "telegram-hashes", true},
		{"document", 900 << 20, "telegram-hashes", true},
		{"video", 1 << 20, "telegram-hashes", true}, // small: every part
		{"video", 5 << 20, "telegram-hashes", false},
		{"photo", 5 << 20, "sampled-parts", false},
		{"video", 5 << 20, "", false},
	} {
		if got := ixSubstConfirmed(c.typ, c.size, c.how); got != c.want {
			t.Fatalf("%+v: %v", c, got)
		}
	}
}
