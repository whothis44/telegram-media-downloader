package app

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"
)

// ---------- an invented archive with pictures ----------

// ixSynth: creators film themselves in a few rooms; each shoot becomes an
// album posted in one of the creator's chats, sometimes reposted in
// another. Every picture is a mix of what all self-filmed pictures share,
// the creator, the shoot and noise (low-quality front cameras), so pictures
// of one shoot are close (about 0.85), of one creator less so (about 0.6),
// and of anyone at all still somewhat alike (about 0.45). Some files stand
// alone for real: a shoot nobody else posted.
type ixSynth struct {
	m      *ixModel
	store  *ixEmbStore
	vec    map[string][]float32
	albums []int32 // families with 3 or more files
	alone  []int32 // families of a file that truly stands alone
}

type ixSynthOpts struct {
	Creators, Shoots, Chats int
	Noise                   float64 // noise weight, lowest (each picture adds up to 0.3 more)
	Dim                     int
	Lone                    float64 // share of shoots posted as a single file
	Faces                   float64 // share of pictures where the creator's face is found
}

func ixRandUnit(r *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	ixNormalise(v)
	return v
}

func ixMix(dim int, parts ...any) []float32 {
	out := make([]float32, dim)
	for i := 0; i < len(parts); i += 2 {
		w := float32(parts[i].(float64))
		for k, x := range parts[i+1].([]float32) {
			out[k] += w * x
		}
	}
	ixNormalise(out)
	return out
}

func ixSynthArchive(seed int64, o ixSynthOpts) *ixSynth {
	r := rand.New(rand.NewSource(seed))
	s := &ixSynth{m: &ixModel{GroupName: map[string]string{}, GroupCat: map[string]string{}, ByKey: map[string]int32{}, ByMsg: map[string]int32{},
		PersonKeys: map[int64]map[string]bool{}, PersonHidden: map[int64]bool{}}, store: &ixEmbStore{index: map[string]int32{}}, vec: map[string][]float32{}}
	m := s.m
	common := ixRandUnit(r, o.Dim)
	msg := 0
	post := func(group string, date int64, keys []string) int32 {
		msg += 20
		pi := int32(len(m.Posts))
		p := ixMPost{Idx: pi, Group: group, Msg: msg, Date: date, Origin: date, Family: -1}
		if len(keys) > 1 {
			p.Grouped = fmt.Sprint("g", msg)
		}
		for k, key := range keys {
			c, ok := m.ByKey[key]
			cl := int32(len(m.Clusters))
			if ok {
				cl = m.Items[c].Cluster
			} else {
				m.Clusters = append(m.Clusters, ixMCluster{Canon: -1, Kind: "photo"})
			}
			ii := int32(len(m.Items))
			m.Items = append(m.Items, ixMItem{Group: group, Msg: msg + k, Key: key, Cluster: cl, Post: pi, Type: "photo", Status: "ok"})
			m.Clusters[cl].Items = append(m.Clusters[cl].Items, ii)
			if m.Clusters[cl].Canon < 0 {
				m.Clusters[cl].Canon = ii
				m.ByKey[key] = ii
			}
			p.Items = append(p.Items, ii)
			p.Msgs = append(p.Msgs, msg+k)
		}
		m.Posts = append(m.Posts, p)
		m.ByMsg[ixPostRef(&p)] = pi
		return pi
	}
	type shootRef struct {
		lone bool
		post int32
	}
	var shoots []shootRef
	for c := 0; c < o.Creators; c++ {
		creator := ixRandUnit(r, o.Dim)
		chats := []string{fmt.Sprint("chat", r.Intn(o.Chats))}
		if r.Float64() < 0.5 {
			chats = append(chats, fmt.Sprint("chat", r.Intn(o.Chats)))
		}
		day := int64(1700000000 + r.Intn(300)*86400)
		person := int64(c + 1)
		m.PersonKeys[person] = map[string]bool{}
		for sh := 0; sh < o.Shoots; sh++ {
			day += int64(r.Intn(4)) * 86400 // often several shoots a day apart, or the same day
			shoot := ixRandUnit(r, o.Dim)
			sw := 0.35 + 0.25*r.Float64() // how much one shoot differs from the next
			n := 3 + r.Intn(8)
			lone := r.Float64() < o.Lone
			if lone {
				n = 1
			}
			var keys []string
			for i := 0; i < n; i++ {
				key := fmt.Sprintf("h:c%ds%di%d", c, sh, i)
				v := ixMix(o.Dim, 0.7, common, 0.4, creator, sw, shoot, o.Noise+0.3*r.Float64(), ixRandUnit(r, o.Dim))
				s.vec[key] = v
				q, _ := ixQuantise(v)
				s.store.putLocked(key, q)
				keys = append(keys, key)
				if r.Float64() < o.Faces {
					m.PersonKeys[person][key] = true
				}
			}
			pi := post(chats[r.Intn(len(chats))], day+int64(r.Intn(3600)), keys)
			shoots = append(shoots, shootRef{lone, pi})
			if !lone && r.Float64() < 0.2 {
				post(fmt.Sprint("chat", r.Intn(o.Chats)), day+86400*int64(1+r.Intn(30)), keys) // a repost elsewhere
			}
		}
	}
	for ci := range m.Clusters {
		cl := &m.Clusters[ci]
		posts, chats := map[int32]bool{}, map[string]bool{}
		for _, ii := range cl.Items {
			posts[m.Items[ii].Post] = true
			chats[m.Items[ii].Group] = true
		}
		cl.Posts, cl.Chats = len(posts), len(chats)
	}
	ixAssignFamilies(m)
	ixAssignSets(m, ixSetFixes{}, nil)
	for _, sh := range shoots {
		f := m.Posts[sh.post].Family
		if sh.lone {
			s.alone = append(s.alone, f)
		} else if len(m.Families[f].Items) >= 3 {
			s.albums = append(s.albums, f)
		}
	}
	return s
}

// ixLoneTrial: one file to match and the album it truly belongs to (-1:
// none).
type ixLoneTrial struct {
	votes []ixLoneVote
	want  int64
}

// ixLoneTrials hides one file of every album and matches it; it also
// matches every file that truly stands alone.
func ixLoneTrials(s *ixSynth, seed int64, p ixLoneParams) []ixLoneTrial {
	r := rand.New(rand.NewSource(seed))
	x := newIxLoneIndex(s.m)
	var out []ixLoneTrial
	for _, fi := range s.albums {
		f := &s.m.Families[fi]
		ii := f.Items[r.Intn(len(f.Items))]
		from := x.fromFamily(fi, ii)
		out = append(out, ixLoneTrial{x.vote(s.store, from, p), x.group(fi)})
	}
	for _, fi := range s.alone {
		ii, ok := x.lone(fi)
		if !ok {
			continue
		}
		out = append(out, ixLoneTrial{x.vote(s.store, x.fromFamily(fi, ii), p), -1})
	}
	return out
}

// ixLoneScore: of the suggestions made, the share that were right, and of
// the hidden files, the share put back.
func ixLoneScore(trials []ixLoneTrial, p ixLoneParams) (precision, recall float64, made int) {
	right, hidden := 0, 0
	for _, t := range trials {
		if t.want >= 0 {
			hidden++
		}
		win, _, ok := ixLonePick(t.votes, p)
		if !ok {
			continue
		}
		made++
		if win.Group == t.want {
			right++
		}
	}
	if made > 0 {
		precision = float64(right) / float64(made)
	}
	if hidden > 0 {
		recall = float64(right) / float64(hidden)
	}
	return
}

var ixSynthSeed = int64(0)

var ixSynthWorlds = []ixSynthOpts{
	{Creators: 60, Shoots: 10, Chats: 15, Noise: 0.30, Dim: 128, Lone: 0.1, Faces: 0.4},
	{Creators: 60, Shoots: 10, Chats: 15, Noise: 0.45, Dim: 128, Lone: 0.1, Faces: 0.4}, // poorer pictures
	{Creators: 25, Shoots: 25, Chats: 6, Noise: 0.35, Dim: 128, Lone: 0.1, Faces: 0},    // prolific creators, few chats, no faces
}

// TestIxLoneFileMatchingHoldOut checks the starting values: hiding one file
// of each album, most suggestions put it back where it came from, and files
// that truly stand alone are seldom given an album. IX_CALIBRATE=1 prints
// the precision and recall over a grid of bars and margins.
func TestIxLoneFileMatchingHoldOut(t *testing.T) {
	if v := os.Getenv("IX_SEED"); v != "" {
		fmt.Sscan(v, &ixSynthSeed)
	}
	var all []ixLoneTrial
	for wi, w := range ixSynthWorlds {
		s := ixSynthArchive(ixSynthSeed+int64(11+wi), w)
		trials := ixLoneTrials(s, int64(5+wi), ixLoneDefaults)
		pr, rc, made := ixLoneScore(trials, ixLoneDefaults)
		t.Logf("world %d: %d albums, %d alone; at the defaults %d suggestions, precision %.3f, recall %.3f", wi, len(s.albums), len(s.alone), made, pr, rc)
		if pr < 0.97 || rc < 0.75 {
			t.Errorf("world %d: precision %.3f recall %.3f", wi, pr, rc)
		}
		wrongAlone := 0
		for _, tr := range trials {
			if _, _, ok := ixLonePick(tr.votes, ixLoneDefaults); ok && tr.want < 0 {
				wrongAlone++
			}
		}
		if wrongAlone*20 > len(s.alone) {
			t.Errorf("world %d: %d of %d lone files were given an album", wi, wrongAlone, len(s.alone))
		}
		all = append(all, trials...)
	}
	if os.Getenv("IX_CALIBRATE") == "" {
		return
	}
	type cell struct{ min, margin, fit, pr, rc, alone float64 }
	var cells []cell
	for _, fit := range []float64{-1, 0, 0.03, 0.06, 0.1} {
		for mn := 0.3; mn <= 2.41; mn += 0.15 {
			for mg := 0.0; mg <= 1.21; mg += 0.15 {
				p := ixLoneDefaults
				p.Min, p.Margin, p.FitSlack = mn, mg, fit
				pr, rc, _ := ixLoneScore(all, p)
				wrong, n := 0, 0
				for _, tr := range all {
					if tr.want < 0 {
						n++
						if _, _, ok := ixLonePick(tr.votes, p); ok {
							wrong++
						}
					}
				}
				cells = append(cells, cell{mn, mg, fit, pr, rc, float64(wrong) / float64(n)})
			}
		}
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i].rc > cells[j].rc })
	n := 0
	for _, c := range cells {
		if c.pr >= 0.97 && c.alone <= 0.05 && n < 20 {
			t.Logf("min %.2f margin %.2f fit %.2f: precision %.3f recall %.3f, lone files given an album %.3f", c.min, c.margin, c.fit, c.pr, c.rc, c.alone)
			n++
		}
	}
}

func TestIxLoneVotesFollowTheEvidence(t *testing.T) {
	s := ixSynthArchive(3, ixSynthWorlds[0])
	x := newIxLoneIndex(s.m)
	fi := s.albums[0]
	f := &s.m.Families[fi]
	from := x.fromFamily(fi, f.Items[0])
	votes := x.vote(s.store, from, ixLoneDefaults)
	if len(votes) == 0 || votes[0].Group != x.group(fi) || !votes[0].SameChat || !votes[0].NearDate {
		t.Fatalf("votes: %+v", votes)
	}
	// Far from everything: no vote.
	odd := ixRandUnit(rand.New(rand.NewSource(99)), 128)
	q, _ := ixQuantise(odd)
	s.store.putLocked("h:odd", q)
	if v := x.vote(s.store, ixLoneFrom{Key: "h:odd", Own: -1}, ixLoneDefaults); len(v) != 0 {
		t.Fatalf("an unrelated picture got votes: %+v", v)
	}
	if _, _, ok := ixLonePick(nil, ixLoneDefaults); ok {
		t.Fatal("nothing to pick")
	}
	if w := ixLoneWhy(ixLoneVote{Hits: 3, SameChat: true, Person: true}); w != "looks like 3 of its files, same chat, same person" {
		t.Fatal(w)
	}
}
