package app

// Files still to come per group, for the Overview's "Complete?" column: a
// group whose messages are all saved is not complete while files it keeps
// are still missing. Counted from the browser index (rebuilt about every
// minute), which already leaves out kinds of files a chat does not keep.

// ixGroupFiles is one group's files not saved yet.
type ixGroupFiles struct {
	ToGo int // missing: still to download (messages deleted on Telegram left out: they cannot be fetched)
	Held int // waiting as a probable copy of a saved file
}

// ixFilesToGo counts, per group, the files of its saved messages that the
// archive does not have yet. ok is false before the browser index is first
// built: then nothing is known.
func (a *App) ixFilesToGo() (out map[string]ixGroupFiles, ok bool) {
	m := a.ixModelLast()
	if m == nil {
		return nil, false
	}
	out = map[string]ixGroupFiles{}
	for i := range m.Items {
		it := &m.Items[i]
		if it.Status != "missing" && it.Status != "held" {
			continue
		}
		if int(it.Post) >= 0 && int(it.Post) < len(m.Posts) && m.Posts[it.Post].Deleted {
			continue
		}
		g := out[it.Group]
		// Held as a copy of a saved file. A lesser copy of a file only queued
		// still has to come: the group is not complete until it does.
		if it.Status == "held" && it.Like != "" {
			g.Held++
		} else {
			g.ToGo++
		}
		out[it.Group] = g
	}
	return out, true
}
