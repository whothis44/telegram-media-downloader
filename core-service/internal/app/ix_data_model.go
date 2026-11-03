package app

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
)

// The data export's view of posts, repost families and duplicate sets (from
// the browser's model), what was not downloaded because the library has it,
// things found in message text, and a checksum list for the stored files.

func ixPostRef(p *ixMPost) string { return p.Group + ":" + strconv.Itoa(p.Msg) }

type ixDataPostFile struct {
	MessageID int    `json:"message_id"`
	Cluster   int32  `json:"cluster"`
	SHA256    string `json:"sha256,omitempty"`
	Path      string `json:"path,omitempty"`
	Status    string `json:"status"` // ok, subst, held, missing
}

func ixDataFileOf(m *ixModel, ii int32) ixDataPostFile {
	it := &m.Items[ii]
	f := ixDataPostFile{MessageID: it.Msg, Cluster: it.Cluster, Path: it.Path, Status: it.Status}
	if strings.HasPrefix(it.Key, "h:") {
		f.SHA256 = it.Key[2:]
	}
	return f
}

func (a *App) ixWriteDataModel(ctx context.Context, writeJSONL func(string, func(*json.Encoder) error) error) error {
	m, err := a.ixRebuildModel(ctx)
	if err != nil {
		return err
	}
	// Posts in chat order.
	order := make([]int32, len(m.Posts))
	for i := range order {
		order[i] = int32(i)
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := &m.Posts[order[i]], &m.Posts[order[j]]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Msg < b.Msg
	})
	if err := writeJSONL("posts.jsonl", func(enc *json.Encoder) error {
		for _, pi := range order {
			p := &m.Posts[pi]
			row := struct {
				Post     string           `json:"post"`
				GroupID  string           `json:"group_id"`
				Messages []int            `json:"message_ids"`
				Album    string           `json:"album_id,omitempty"`
				Date     int64            `json:"date"`
				Original int64            `json:"original_date,omitempty"`
				Family   int32            `json:"family"`
				Role     string           `json:"role"`
				Files    []ixDataPostFile `json:"files"`
			}{Post: ixPostRef(p), GroupID: p.Group, Messages: p.Msgs, Album: p.Grouped, Date: p.Date, Family: p.Family, Role: p.Role, Files: []ixDataPostFile{}}
			if p.Origin != p.Date {
				row.Original = p.Origin
			}
			for _, ii := range p.Items {
				row.Files = append(row.Files, ixDataFileOf(m, ii))
			}
			if err := enc.Encode(row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := writeJSONL("families.jsonl", func(enc *json.Encoder) error {
		for fi := range m.Families {
			f := &m.Families[fi]
			type famPost struct {
				Post    string `json:"post"`
				GroupID string `json:"group_id"`
				Date    int64  `json:"date"`
				Role    string `json:"role"`
			}
			type famFile struct {
				Cluster    int32          `json:"cluster"`
				Best       ixDataPostFile `json:"best"`
				Introduced string         `json:"introduced_by"`
			}
			row := struct {
				Family int32     `json:"family"`
				Lead   string    `json:"lead_post"`
				First  int64     `json:"first_date"`
				Chats  int       `json:"chats"`
				Merged bool      `json:"merged,omitempty"`
				Posts  []famPost `json:"posts"`
				Files  []famFile `json:"files"`
			}{Family: int32(fi), Lead: ixPostRef(&m.Posts[f.Lead]), First: f.First, Chats: f.Chats, Merged: f.Merged}
			for _, pi := range f.Posts {
				p := &m.Posts[pi]
				row.Posts = append(row.Posts, famPost{ixPostRef(p), p.Group, p.Date, p.Role})
			}
			for k, ii := range f.Items {
				row.Files = append(row.Files, famFile{m.Items[ii].Cluster, ixDataFileOf(m, ii), ixPostRef(&m.Posts[f.Intro[k]])})
			}
			if err := enc.Encode(row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	how := map[string]string{}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT key, how FROM ix_mcl WHERE how IN ('auto','verified')`); err == nil {
		for rows.Next() {
			var k, h string
			if rows.Scan(&k, &h) == nil {
				how[k] = "picture_" + h
			}
		}
		rows.Close()
	}
	if err := writeJSONL("media_clusters.jsonl", func(enc *json.Encoder) error {
		for ci := range m.Clusters {
			c := &m.Clusters[ci]
			if len(c.Items) < 2 {
				continue
			}
			type member struct {
				GroupID   string `json:"group_id"`
				MessageID int    `json:"message_id"`
				SHA256    string `json:"sha256,omitempty"`
				Path      string `json:"path,omitempty"`
				Status    string `json:"status"`
				MatchedBy string `json:"matched_by"` // same_bytes, picture_auto, picture_verified, video_same, person, held_likely_copy
			}
			best := &m.Items[c.Canon]
			row := struct {
				Cluster int            `json:"cluster"`
				Kind    string         `json:"kind"`
				Generic bool           `json:"generic,omitempty"`
				Best    ixDataPostFile `json:"best"`
				Members []member       `json:"members"`
			}{Cluster: ci, Kind: c.Kind, Generic: c.Generic, Best: ixDataFileOf(m, c.Canon)}
			for _, ii := range c.Items {
				it := &m.Items[ii]
				x := member{GroupID: it.Group, MessageID: it.Msg, Path: it.Path, Status: it.Status}
				if strings.HasPrefix(it.Key, "h:") {
					x.SHA256 = it.Key[2:]
				}
				switch {
				case it.Status == "held":
					x.MatchedBy = "held_likely_copy"
				case it.Key == best.Key:
					x.MatchedBy = "same_bytes"
				case how[it.Key] != "":
					x.MatchedBy = how[it.Key]
				case how[best.Key] != "":
					x.MatchedBy = how[best.Key]
				case it.Type == "video":
					x.MatchedBy = "video_same"
				default:
					x.MatchedBy = "person"
				}
				row.Members = append(row.Members, x)
			}
			if err := enc.Encode(row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := writeJSONL("covered.jsonl", func(enc *json.Encoder) error {
		type row struct {
			GroupID   string `json:"group_id"`
			MessageID int    `json:"message_id"`
			How       string `json:"how"`
			OfPath    string `json:"of_path,omitempty"`
			Like      string `json:"like,omitempty"`
			At        int64  `json:"at"`
		}
		rows, err := a.db.Reader.QueryContext(ctx, `SELECT s.group_id, s.message_id, s.how, s.of_path, '', s.at FROM ix_subst s
				JOIN downloads d ON d.group_id=s.group_id AND d.message_id=s.message_id AND d.file_path=s.of_path
			UNION ALL SELECT group_id, message_id, 'held', '', like_key, held_at FROM ix_held WHERE released_at=0 AND reason<>''
			ORDER BY 1, 2`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.GroupID, &r.MessageID, &r.How, &r.OfPath, &r.Like, &r.At); err != nil {
				return err
			}
			r.OfPath = ixNormPath(r.OfPath)
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	list, err := a.ixEntities(ctx)
	if err != nil {
		return err
	}
	return writeJSONL("entities.jsonl", func(enc *json.Encoder) error {
		sorted := append([]*ixEntity(nil), list...)
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].Kind != sorted[j].Kind {
				return sorted[i].Kind < sorted[j].Kind
			}
			return sorted[i].Value < sorted[j].Value
		})
		for _, e := range sorted {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
		return nil
	})
}

// ixWriteManifest writes "<sha256>  <path>" for every stored file with a
// known checksum, sorted by path.
func (a *App) ixWriteManifest(ctx context.Context, path string) error {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT DISTINCT lower(file_hash), file_path FROM downloads WHERE length(file_hash)=64 AND file_path IS NOT NULL AND file_path<>'' AND (status IS NULL OR status='completed')`)
	if err != nil {
		return err
	}
	type line struct{ sum, path string }
	var lines []line
	seen := map[string]bool{}
	for rows.Next() {
		var l line
		if rows.Scan(&l.sum, &l.path) != nil {
			continue
		}
		l.path = ixNormPath(l.path)
		if l.path == "" || seen[l.path] {
			continue
		}
		seen[l.path] = true
		lines = append(lines, l)
	}
	rows.Close()
	sort.Slice(lines, func(i, j int) bool { return lines[i].path < lines[j].path })
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, l := range lines {
		_, _ = w.WriteString(l.sum + "  " + l.path + "\n")
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
