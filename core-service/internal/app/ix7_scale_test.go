package app

import (
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"
)

// ixScaleModel: groups chats posting albums, most of them reposts or
// partial reposts of what other chats posted.
func ixScaleModel(groups, items int, repeat float64, seed int64) *ixModel {
	r := rand.New(rand.NewSource(seed))
	m := &ixModel{GroupName: map[string]string{}, GroupCat: map[string]string{}, ByKey: map[string]int32{}, ByMsg: map[string]int32{}}
	var posted [][]int32 // earlier posts' clusters
	date := int64(0)
	for len(m.Items) < items {
		g := fmt.Sprintf("g%d", r.Intn(groups))
		n := 1 + r.Intn(10)
		var cl []int32
		if len(posted) > 0 && r.Float64() < repeat {
			src := posted[r.Intn(len(posted))]
			for _, c := range src {
				if r.Float64() < 0.6 && len(cl) < n {
					cl = append(cl, c)
				}
			}
		}
		for len(cl) < n {
			cl = append(cl, int32(len(m.Clusters)))
			m.Clusters = append(m.Clusters, ixMCluster{Canon: -1})
		}
		date++
		pi := int32(len(m.Posts))
		p := ixMPost{Idx: pi, Group: g, Msg: int(date), Date: date, Origin: date, Family: -1}
		for _, c := range cl {
			ii := int32(len(m.Items))
			k := fmt.Sprintf("h:%d", c)
			m.Items = append(m.Items, ixMItem{Group: g, Msg: int(date), Key: k, Cluster: c, Post: pi, Type: "photo", Status: "ok"})
			m.Clusters[c].Items = append(m.Clusters[c].Items, ii)
			if m.Clusters[c].Canon < 0 {
				m.Clusters[c].Canon = ii
				m.ByKey[k] = ii
			}
			p.Items = append(p.Items, ii)
		}
		m.Posts = append(m.Posts, p)
		posted = append(posted, cl)
	}
	for ci := range m.Clusters {
		chats := map[string]bool{}
		posts := map[int32]bool{}
		for _, ii := range m.Clusters[ci].Items {
			chats[m.Items[ii].Group] = true
			posts[m.Items[ii].Post] = true
		}
		c := &m.Clusters[ci]
		c.Posts, c.Chats = len(posts), len(chats)
		c.Generic = c.Posts > ixGenericPosts && c.Chats > ixGenericChats
	}
	return m
}

func TestIxScale(t *testing.T) {
	if os.Getenv("IX_SCALE") == "" {
		t.Skip("set IX_SCALE=1")
	}
	m := ixScaleModel(20, 100000, 0.7, 1)
	t0 := time.Now()
	ixAssignFamilies(m)
	t1 := time.Now()
	ixAssignSets(m, ixSetFixes{}, nil)
	t2 := time.Now()
	big, bi := 0, 0
	for i, s := range m.Sets {
		if len(s.Families) > big {
			big, bi = len(s.Families), i
		}
	}
	hist := map[int]int{}
	for _, s := range m.Sets {
		hist[len(s.Families)/5*5]++
	}
	t.Logf("size histogram (by 5): %v; biggest roles %v", hist, ixSetCounts(&m.Sets[bi]))
	if t2.Sub(t0) > 10*time.Second {
		t.Errorf("too slow: %v", t2.Sub(t0))
	}
	t.Logf("items %d clusters %d posts %d families %d sets %d (largest %d families); families %v, sets %v",
		len(m.Items), len(m.Clusters), len(m.Posts), len(m.Families), len(m.Sets), big, t1.Sub(t0), t2.Sub(t1))
}
