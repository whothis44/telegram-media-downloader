package app

import (
	"context"
	"sort"
	"strconv"
	"strings"
)

// ixJTopic is one forum topic of a chat, for the browser's topic filter.
type ixJTopic struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Posts int    `json:"posts"`
}

// ixPostTopic: the forum topic a post is in, 1 (General) when its message
// carries no topic mark.
func ixPostTopic(p *ixMPost) int {
	if p.Topic <= 0 {
		return 1
	}
	return p.Topic
}

// ixBrowseTopics lists the topics of each forum chat (one with a message in a
// topic other than General), with their posts, most posts first. Names come
// from the topics' own service messages (ix_topics); a topic whose name was
// never seen is "Topic #id", as in the chat pages (ix_data.go topicTitle).
func (a *App) ixBrowseTopics(ctx context.Context, m *ixModel) map[string][]ixJTopic {
	counts := map[string]map[int]int{}
	forum := map[string]bool{}
	for i := range m.Posts {
		p := &m.Posts[i]
		if p.Orphan {
			continue
		}
		if p.Topic > 1 {
			forum[p.Group] = true
		}
		if counts[p.Group] == nil {
			counts[p.Group] = map[int]int{}
		}
		counts[p.Group][ixPostTopic(p)]++
	}
	if len(forum) == 0 {
		return nil
	}
	names := map[string]map[int]string{}
	if a != nil && a.db != nil {
		if rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, topic_id, title FROM ix_topics`); err == nil {
			for rows.Next() {
				var g, title string
				var id int
				if rows.Scan(&g, &id, &title) == nil && forum[g] && strings.TrimSpace(title) != "" {
					if names[g] == nil {
						names[g] = map[int]string{}
					}
					names[g][id] = strings.TrimSpace(title)
				}
			}
			rows.Close()
		}
	}
	out := map[string][]ixJTopic{}
	for g := range forum {
		var list []ixJTopic
		for id, n := range counts[g] {
			name := names[g][id]
			switch {
			case name != "":
			case id == 1:
				name = "General"
			default:
				name = "Topic #" + strconv.Itoa(id)
			}
			list = append(list, ixJTopic{ID: id, Name: name, Posts: n})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Posts != list[j].Posts {
				return list[i].Posts > list[j].Posts
			}
			return list[i].ID < list[j].ID
		})
		out[g] = list
	}
	return out
}
