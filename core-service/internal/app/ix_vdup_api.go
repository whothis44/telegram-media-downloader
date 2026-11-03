package app

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type ixDupMember struct {
	Key        string    `json:"key"`
	DownloadID int64     `json:"downloadId"`
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	Duration   float64   `json:"duration"`
	Places     []ixDupAt `json:"places"`
}

type ixDupAt struct {
	Group string `json:"group"`
	Label string `json:"label"`
	Link  string `json:"link"`
}

type ixDupCluster struct {
	Kind    string        `json:"kind"` // same: all the same video; mixed: includes clips
	Members []ixDupMember `json:"members"`
	Total   int64         `json:"totalSize"`
	Spare   int64         `json:"spareSize"` // bytes beyond the largest copy
}

// ixDupClusters groups near-duplicate pairs into sets of related videos,
// biggest potential saving first.
func (a *App) ixDupClusters(ctx context.Context) ([]ixDupCluster, error) {
	near := a.ixNearMap(ctx)
	if len(near) == 0 {
		return nil, nil
	}
	// Connected components over every pair (same and clip).
	seen := map[string]bool{}
	var comps [][]string
	keys := make([]string, 0, len(near))
	for k := range near {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if seen[k] {
			continue
		}
		var comp []string
		stack := []string{k}
		seen[k] = true
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			comp = append(comp, cur)
			for _, n := range near[cur] {
				if !seen[n.Key] {
					seen[n.Key] = true
					stack = append(stack, n.Key)
				}
			}
		}
		sort.Strings(comp)
		comps = append(comps, comp)
	}
	metas, err := a.ixGroups(ctx)
	if err != nil {
		return nil, err
	}
	layouts := map[string]*ixGroup{}
	layoutOf := func(id string) *ixGroup {
		if g, ok := layouts[id]; ok {
			return g
		}
		for _, m := range metas {
			if m.ID == id {
				g, err := a.ixLayoutOf(ctx, m)
				if err != nil {
					break
				}
				layouts[id] = g
				return g
			}
		}
		layouts[id] = nil
		return nil
	}
	places := a.ixNearPlaces(ctx)
	var out []ixDupCluster
	for _, comp := range comps {
		cl := ixDupCluster{Kind: "same"}
		var largest int64
		for _, k := range comp {
			for _, n := range near[k] {
				if n.Kind != "same" {
					cl.Kind = "mixed"
				}
			}
			m := ixDupMember{Key: k}
			_ = a.db.Reader.QueryRowContext(ctx, `SELECT duration, file_path FROM ix_vfp WHERE key=?`, k).Scan(&m.Duration, &m.Path)
			for _, p := range places[k] {
				g := layoutOf(p.GroupID)
				at := ixDupAt{Group: p.GroupID, Label: "chat " + p.GroupID + " #" + strconv.Itoa(p.MessageID)}
				if g != nil {
					at.Group = g.Name
					at.Label = g.Name + " #" + strconv.Itoa(p.MessageID)
					if msg := g.ByID[p.MessageID]; msg != nil {
						at.Label = g.placeName(msg) + " #" + strconv.Itoa(p.MessageID)
						for _, f := range msg.Files {
							if f.Key == k {
								m.DownloadID, m.Size = f.ID, f.Size
								if m.Path == "" {
									m.Path = f.Path
								}
							}
						}
					}
					page := g.PageOf[p.MessageID]
					if page == "" {
						page = "index.html"
					}
					at.Link = "/ix/view/" + g.Dir + "/" + page + "#m" + strconv.Itoa(p.MessageID)
				}
				m.Places = append(m.Places, at)
			}
			cl.Total += m.Size
			largest = max(largest, m.Size)
			cl.Members = append(cl.Members, m)
		}
		sort.Slice(cl.Members, func(i, j int) bool { return cl.Members[i].Size > cl.Members[j].Size })
		cl.Spare = cl.Total - largest
		out = append(out, cl)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Spare > out[j].Spare })
	return out, nil
}

func (a *App) handleIxDups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a.ixVdupCount(ctx)
	clusters, err := a.ixDupClusters(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	offset := max(0, int(number(r.URL.Query().Get("offset"), 0)))
	limit := max(1, min(200, int(number(r.URL.Query().Get("limit"), 50))))
	var spare int64
	for _, c := range clusters {
		spare += c.Spare
	}
	total := len(clusters)
	page := []ixDupCluster{}
	if offset < total {
		page = clusters[offset:min(total, offset+limit)]
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": a.ixVdupStatus(), "clusters": page, "total": total, "spareSize": spare})
}

func (a *App) handleIxDupsRetry(w http.ResponseWriter, r *http.Request) {
	res, err := a.db.Writer.ExecContext(r.Context(), `DELETE FROM ix_vfp WHERE status='error'`)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	box := a.ixVdup()
	box.mu.Lock()
	box.cpuOnly, box.gpuErrors, box.st.LastError = false, 0, ""
	box.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "retrying": n})
}

// ixDupCSV is near_duplicates.csv: one row per file in each set.
func ixDupCSV(clusters []ixDupCluster) []byte {
	rows := [][]string{{"set", "kind", "path_in_downloads_folder", "size_bytes", "duration_seconds", "posted_in"}}
	for i, c := range clusters {
		for _, m := range c.Members {
			var at []string
			for _, p := range m.Places {
				at = append(at, p.Label)
			}
			rows = append(rows, []string{strconv.Itoa(i + 1), c.Kind, m.Path, strconv.FormatInt(m.Size, 10), strconv.FormatFloat(m.Duration, 'f', 1, 64), strings.Join(at, " | ")})
		}
	}
	return ixCSVBytes(rows)
}
