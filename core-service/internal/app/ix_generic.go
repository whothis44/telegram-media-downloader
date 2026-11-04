package app

// Generic files: a logo, sticker or promo card that turns up in many posts
// of many chats would weld unrelated posts together, so it is ignored for
// families and sets. But a popular album reposted by many variety chats
// crosses the same counts (more than 50 posts in more than 10 chats), and
// ignoring it would split the very reposts that belong together.
//
// The difference is what a file travels with. A logo sits beside
// different files each time; a file of a popular album comes with the same
// few siblings. So a file over those counts is generic only when at least
// half of the files posted beside it are different each time (distinct
// companions / all companions), or when it is small (under 200 pixels on
// its longer side), not a picture or video (stickers, documents), or in
// more than 200 posts (the old rule, kept for the extreme cases). A file
// always posted on its own cannot tie posts together and is never generic.

const (
	ixGenericHardPosts = 200 // in more posts than this: generic, whatever it travels with
	ixGenericSmall     = 200 // pixels on the longer side: smaller pictures are icons and logos
	ixGenericVaried    = 0.5 // share of companions that differ each time, from which a file is generic
)

// ixMarkGeneric sets each cluster's Generic flag (Posts, Chats, Kind and
// Canon must be known).
func ixMarkGeneric(m *ixModel) {
	for ci := range m.Clusters {
		c := &m.Clusters[ci]
		c.Generic = c.Posts > ixGenericPosts && c.Chats > ixGenericChats && ixLooksGeneric(m, int32(ci))
	}
}

func ixLooksGeneric(m *ixModel, ci int32) bool {
	c := &m.Clusters[ci]
	if c.Posts > ixGenericHardPosts || c.Kind == "other" {
		return true
	}
	if c.Canon >= 0 {
		if it := &m.Items[c.Canon]; it.W > 0 && it.H > 0 && max(it.W, it.H) < ixGenericSmall {
			return true
		}
	}
	posts := map[int32]bool{}
	distinct := map[int32]bool{}
	total := 0
	for _, ii := range c.Items {
		pi := m.Items[ii].Post
		if posts[pi] {
			continue
		}
		posts[pi] = true
		seen := map[int32]bool{ci: true}
		for _, jj := range m.Posts[pi].Items {
			if cj := m.Items[jj].Cluster; !seen[cj] {
				seen[cj] = true
				distinct[cj] = true
				total++
			}
		}
	}
	if total == 0 {
		return false
	}
	return float64(len(distinct)) >= ixGenericVaried*float64(total)
}
