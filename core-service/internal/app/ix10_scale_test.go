package app

import (
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"
)

// ixScaleArchive invents an archive shaped like the user's: 120 chats (30
// that post their own albums, 60 that repost from one or two of them, 30
// variety chats reposting from many), posts mostly albums (2 to 8 files)
// and single files, each distinct file posted about seven times, some
// reposts re-encoded (other bytes, with a look-alike pair waiting), every
// distinct picture with an embedding. All invented.
type ixScaleWorld struct {
	m     *ixModel
	store *ixEmbStore
	vec   map[string][]float32
	keys  map[int32]string // cluster -> its key
}

func ixScaleArchive(seed int64, posts, dim int, withVectors bool) *ixScaleWorld {
	r := rand.New(rand.NewSource(seed))
	w := &ixScaleWorld{store: &ixEmbStore{index: map[string]int32{}}, vec: map[string][]float32{}, keys: map[int32]string{}}
	x := newIxTestModel()
	w.m = x.m
	const sources, reposters, variety = 30, 60, 30
	chat := func(kind string, i int) string { return fmt.Sprintf("-100%s%03d", kind, i) }
	for i := 0; i < sources; i++ {
		w.m.Groups = append(w.m.Groups, chat("1", i))
	}
	for i := 0; i < reposters; i++ {
		w.m.Groups = append(w.m.Groups, chat("2", i))
	}
	for i := 0; i < variety; i++ {
		w.m.Groups = append(w.m.Groups, chat("3", i))
	}
	common := ixRandUnit(r, dim)
	creators := make([][]float32, sources)
	for i := range creators {
		creators[i] = ixRandUnit(r, dim)
	}
	type orig struct {
		keys []string
		src  int
	}
	var origs []orig
	date := int64(1600000000)
	newKey := func(src int, shoot []float32) string {
		k := fmt.Sprintf("h:%d", len(w.vec))
		if withVectors {
			v := ixMix(dim, 0.7, common, 0.4, creators[src], 0.5, shoot, 0.4, ixRandUnit(r, dim))
			w.vec[k] = v
			q, _ := ixQuantise(v)
			w.store.putLocked(k, q)
		} else {
			w.vec[k] = nil
		}
		return k
	}
	var soft []ixSoftPair
	for len(w.m.Posts) < posts {
		date += int64(r.Intn(600))
		if len(origs) < 30 || r.Float64() < 0.13 {
			// An original album or single file.
			src := r.Intn(sources)
			n := 1
			if r.Float64() < 0.5 {
				n = 2 + r.Intn(7)
			}
			var shoot []float32
			if withVectors {
				shoot = ixRandUnit(r, dim)
			}
			var keys []string
			for i := 0; i < n; i++ {
				keys = append(keys, newKey(src, shoot))
			}
			x.post(chat("1", src), date, keys...)
			origs = append(origs, orig{keys, src})
			continue
		}
		// A repost: popular posts more often (the square favours recent
		// and low indices alike, a rough long tail).
		o := origs[int(float64(len(origs))*r.Float64()*r.Float64())]
		var g string
		if r.Float64() < 0.55 {
			g = chat("2", (o.src*2+r.Intn(2))%reposters)
		} else {
			g = chat("3", r.Intn(variety))
		}
		keys := o.keys
		if len(keys) > 2 && r.Float64() < 0.3 {
			keys = keys[:2+r.Intn(len(keys)-2)] // part of the album
		}
		if r.Float64() < 0.08 {
			// Re-encoded with a watermark: other bytes, a look-alike pair each.
			var re []string
			for _, k := range keys {
				nk := fmt.Sprintf("h:%d", len(w.vec))
				if withVectors {
					v := ixMix(dim, 1.0, w.vec[k], 0.12, ixRandUnit(r, dim))
					w.vec[nk] = v
					q, _ := ixQuantise(v)
					w.store.putLocked(nk, q)
				} else {
					w.vec[nk] = nil
				}
				re = append(re, nk)
				soft = append(soft, ixSoftPair{A: k, B: nk, Kind: "look"})
			}
			keys = re
		}
		x.post(g, date, keys...)
	}
	m := x.done()
	m.Soft = soft
	for ci := range m.Clusters {
		w.keys[int32(ci)] = m.Items[m.Clusters[ci].Canon].Key
	}
	// Faces: each source's creator in a third of its files.
	for i := range m.Items {
		it := &m.Items[i]
		if it.Group[4] == '1' && r.Float64() < 0.33 {
			id := int64(it.Group[5]-'0')*100 + int64(it.Group[6]-'0')*10 + int64(it.Group[7]-'0') + 1
			if m.PersonKeys[id] == nil {
				m.PersonKeys[id] = map[string]bool{}
			}
			m.PersonKeys[id][it.Key] = true
		}
	}
	return w
}

// ixScaleRun times every stage of the model build after the tables are
// read, and the background lone-file matcher per file.
func ixScaleRun(t *testing.T, w *ixScaleWorld, lone int) time.Duration {
	m := w.m
	t0 := time.Now()
	ixMarkGeneric(m)
	t1 := time.Now()
	ixAssignFamilies(m)
	t2 := time.Now()
	m.Overlap = ixComputeOverlap(m)
	t3 := time.Now()
	if w.store.size() > 0 {
		m.EmbSim = w.store.sim
	}
	ixAssignSets(m, ixSetFixes{}, ixSetLikeness(m, m.EmbSim))
	t4 := time.Now()
	m.SameAlbums = ixSameAlbums(m, nil, nil)
	t5 := time.Now()
	total := t5.Sub(t0)
	generic := 0
	for i := range m.Clusters {
		if m.Clusters[i].Generic {
			generic++
		}
	}
	kinds := map[string]int{}
	for i := range m.Overlap.Groups {
		c, _ := m.Overlap.class(i)
		kinds[c]++
	}
	t.Logf("%d chats, %d posts, %d files, %d clusters (%d generic), %d families, %d sets, %d look-alike pairs, %d album pairs offered, loose bar %.3f",
		len(m.Overlap.Groups), len(m.Posts), len(m.Items), len(m.Clusters), generic, len(m.Families), len(m.Sets), len(m.Soft), len(m.SameAlbums), m.LooseSim)
	t.Logf("chat kinds %v", kinds)
	t.Logf("generic %v, families %v, overlap %v, sets %v, album pairs %v: %v in all", t1.Sub(t0), t2.Sub(t1), t3.Sub(t2), t4.Sub(t3), t5.Sub(t4), total)
	if lone > 0 && w.store.size() > 0 {
		x := newIxLoneIndex(m)
		var fams []int32
		for fi := range m.Families {
			if _, ok := x.lone(int32(fi)); ok {
				fams = append(fams, int32(fi))
			}
		}
		t6 := time.Now()
		n, made := 0, 0
		for _, fi := range fams {
			if n >= lone {
				break
			}
			ii, _ := x.lone(fi)
			if _, _, ok := ixLonePick(x.vote(w.store, x.fromFamily(fi, ii), ixLoneDefaults), ixLoneDefaults); ok {
				made++
			}
			n++
		}
		per := time.Since(t6) / time.Duration(max(1, n))
		t.Logf("lone files %d; matching %v each (one core), %d of %d offered an album: all of them in about %v on one core", len(fams), per, made, n, per*time.Duration(len(fams)))
	}
	return total
}

func TestIxAlbumsScaleSmall(t *testing.T) {
	w := ixScaleArchive(1, 4000, 64, true)
	if took := ixScaleRun(t, w, 50); took > 5*time.Second {
		t.Errorf("too slow: %v", took)
	}
}

func TestIxAlbumsScale(t *testing.T) {
	if os.Getenv("IX_SCALE") == "" {
		t.Skip("set IX_SCALE=1")
	}
	w := ixScaleArchive(1, 64000, 768, true)
	if took := ixScaleRun(t, w, 300); took > 4*time.Second {
		t.Errorf("too slow: %v", took)
	}
}

// TestIxAlbumsScaleBuild writes the invented archive to the database (in
// one album of five, a file is left without its message) and times the
// whole model build, tables read included.
func TestIxAlbumsScaleBuild(t *testing.T) {
	if os.Getenv("IX_SCALE") == "" {
		t.Skip("set IX_SCALE=1")
	}
	w := ixScaleArchive(2, 64000, 0, false)
	a, ctx := ix8App(t, map[string]any{})
	m := w.m
	var groups []any
	for _, g := range m.Groups {
		groups = append(groups, map[string]any{"id": g, "name": "chat " + g, "enabled": true})
	}
	cfg, err := a.config.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg["groups"] = groups
	if err = a.config.Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	tx, err := a.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	msgSt, err := tx.PrepareContext(ctx, `INSERT INTO ix_messages(group_id,message_id,date,text,grouped_id,media_type,seen_at) VALUES(?,?,?,'',?,'photo',1)`)
	if err != nil {
		t.Fatal(err)
	}
	dlSt, err := tx.PrepareContext(ctx, `INSERT INTO downloads(group_id,group_name,message_id,file_name,file_size,file_type,file_path,status,file_hash,created_at) VALUES(?,'',?,'f.jpg',1000,'photo',?,'completed',?,'2026-01-01 00:00:00')`)
	if err != nil {
		t.Fatal(err)
	}
	nextMsg := map[string]int{}
	orphans := 0
	for pi := range m.Posts {
		p := &m.Posts[pi]
		grouped := ""
		if len(p.Items) > 1 {
			grouped = fmt.Sprint(pi)
		}
		for k, ii := range p.Items {
			nextMsg[p.Group]++
			id := nextMsg[p.Group]
			it := &m.Items[ii]
			if _, err := dlSt.ExecContext(ctx, p.Group, id, fmt.Sprintf("%s/%d.jpg", p.Group, id), fmt.Sprintf("%064x", it.Cluster)); err != nil {
				t.Fatal(err)
			}
			if k == 1 && len(p.Items) > 2 && pi%5 == 0 {
				orphans++ // the album's second message was never captured
				continue
			}
			if _, err := msgSt.ExecContext(ctx, p.Group, id, p.Date, grouped); err != nil {
				t.Fatal(err)
			}
		}
		nextMsg[p.Group] += 1 + pi%3 // other messages between posts
	}
	// The re-encoded reposts' look-alike pairs, waiting for review.
	hashOf := func(k string) string { return fmt.Sprintf("h:%064x", m.Items[m.ByKey[k]].Cluster) }
	for _, p := range m.Soft {
		x, y := ixPairRefs(hashOf(p.A), hashOf(p.B))
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ix_msug(key_a,key_b,kind,distance,found_at) VALUES(?,?,'look',0.02,1)`, x, y); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d posts, %d files (%d without their message), %d look-alike pairs in %v", len(m.Posts), len(m.Items), orphans, len(m.Soft), time.Since(t0))
	var built *ixModel
	for i := 0; i < 2; i++ {
		if built, err = a.ixRebuildModel(ctx); err != nil {
			t.Fatal(err)
		}
		t.Logf("model build %d: %v", i+1, built.Took)
	}
	placed := 0
	for i := range built.Items {
		if built.Items[i].Placed {
			placed++
		}
	}
	kinds := map[string]int{}
	for i := range built.Overlap.Groups {
		c, _ := built.Overlap.class(i)
		kinds[c]++
	}
	t.Logf("%d posts, %d families, %d sets, %d album pairs offered, %d lone files placed in their album; chat kinds %v", len(built.Posts), len(built.Families), len(built.Sets), len(built.SameAlbums), placed, kinds)
	if len(built.Sets) == 0 || len(built.SameAlbums) == 0 {
		t.Error("the look-alike pairs linked nothing")
	}
	if placed < orphans*9/10 {
		t.Errorf("placed %d of %d", placed, orphans)
	}
	// Most of the build is reading the downloads and messages tables (as
	// before these changes; on a shared test machine 6-14 s here, the old
	// code included). The stages added for albums take under a second, so
	// this only guards against something going badly wrong.
	if built.Took > 30*time.Second {
		t.Errorf("model build too slow: %v", built.Took)
	}
}
