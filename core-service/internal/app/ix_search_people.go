package app

import (
	"sort"
	"strings"
)

// People as a browser filter: posts where every person chosen appears
// (person=1,7: both in one post), in any view, with a search or not.

// ixPeopleSets lists, for each person, the duplicate sets they appear in.
// A face found on any copy counts, so a post is not missed while a new
// best copy waits to be looked at for faces.
func (m *ixModel) ixPeopleSets(ids []int64) []map[int32]bool {
	out := make([]map[int32]bool, len(ids))
	for i, id := range ids {
		set := map[int32]bool{}
		for k := range m.PersonKeys[id] {
			if ii, ok := m.ByKey[k]; ok {
				set[m.Items[ii].Cluster] = true
			}
		}
		out[i] = set
	}
	return out
}

// postHasPeople: every person chosen appears in one of the post's files.
func (m *ixModel) postHasPeople(pi int32, f ixFeedQuery) bool {
	people := f.people
	if len(people) != len(f.Person) {
		people = m.ixPeopleSets(f.Person) // not worked out beforehand
	}
	for _, set := range people {
		found := false
		for _, ii := range m.Posts[pi].Items {
			if set[m.Items[ii].Cluster] {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// setHasAnyone: a file (duplicate set) shows one of the people chosen.
func (m *ixModel) setHasAnyone(c int32, f ixFeedQuery) bool {
	people := f.people
	if len(people) != len(f.Person) {
		people = m.ixPeopleSets(f.Person)
	}
	for _, set := range people {
		if set[c] {
			return true
		}
	}
	return false
}

// ixPeopleTop: for the sidebar, the people not hidden whose name holds q,
// most files first, at most top (0: all); and how many there are.
func ixPeopleTop(list []ixPerson, q string, top int) ([]ixPerson, int) {
	q = strings.ToLower(strings.TrimSpace(q))
	out := []ixPerson{}
	for _, p := range list {
		if !p.Hidden && (q == "" || strings.Contains(strings.ToLower(p.Name), q)) {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Files > out[j].Files })
	n := len(out)
	if top > 0 && len(out) > top {
		out = out[:top]
	}
	return out, n
}
