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
	if err := writeJSONL("sets.jsonl", func(enc *json.Encoder) error {
		for si := range m.Sets {
			st := &m.Sets[si]
			type setFile struct {
				Cluster  int32          `json:"cluster"`
				Best     ixDataPostFile `json:"best"`
				Role     string         `json:"role"` // core, member, loose, elsewhere
				Families int            `json:"in_families"`
				Likeness float32        `json:"likeness,omitempty"`
				OtherSet *int32         `json:"other_set,omitempty"`
				Person   bool           `json:"placed_by_person,omitempty"`
			}
			row := struct {
				Set      int32     `json:"set"`
				Anchor   string    `json:"anchor_post"`
				First    int64     `json:"first_date"`
				Last     int64     `json:"last_date"`
				Chats    int       `json:"chats"`
				Families []int32   `json:"families"`
				Posts    []string  `json:"posts"`
				Files    []setFile `json:"files"`
			}{Set: int32(si), Anchor: st.Anchor, First: st.First, Last: st.Last, Chats: st.Chats, Families: st.Families, Posts: []string{}, Files: []setFile{}}
			for _, fi := range st.Families {
				for _, pi := range m.Families[fi].Posts {
					row.Posts = append(row.Posts, ixPostRef(&m.Posts[pi]))
				}
			}
			for _, it := range st.Items {
				f := setFile{Cluster: m.Items[it.Item].Cluster, Best: ixDataFileOf(m, it.Item), Role: it.Role, Families: it.In, Likeness: it.Sim, Person: it.Fixed}
				if it.Other >= 0 {
					other := it.Other
					f.OtherSet = &other
				}
				row.Files = append(row.Files, f)
			}
			if err := enc.Encode(row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := a.ixWriteRecognised(ctx, m, writeJSONL); err != nil {
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

// ixWriteRecognised writes what recognition found: people and their faces,
// text read from pictures and speech written out. Files are named by their
// best copy (sha256 or path).
func (a *App) ixWriteRecognised(ctx context.Context, m *ixModel, writeJSONL func(string, func(*json.Encoder) error) error) error {
	fileOf := func(key string) (ixDataPostFile, bool) {
		ii, ok := m.ByKey[key]
		if !ok {
			return ixDataPostFile{}, false
		}
		return ixDataFileOf(m, m.Clusters[m.Items[ii].Cluster].Canon), true
	}
	if err := writeJSONL("people.jsonl", func(enc *json.Encoder) error {
		rows, err := a.db.Reader.QueryContext(ctx, `SELECT p.id, p.name, p.hidden, COUNT(f.id), COUNT(DISTINCT f.key) FROM ix_people p JOIN ix_faces f ON f.person=p.id
			WHERE p.merged_into=0 GROUP BY p.id ORDER BY p.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r struct {
				Person int64  `json:"person"`
				Name   string `json:"name,omitempty"`
				Hidden bool   `json:"hidden,omitempty"`
				Faces  int    `json:"faces"`
				Files  int    `json:"files"`
			}
			if err := rows.Scan(&r.Person, &r.Name, &r.Hidden, &r.Faces, &r.Files); err != nil {
				return err
			}
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	if err := writeJSONL("faces.jsonl", func(enc *json.Encoder) error {
		rows, err := a.db.Reader.QueryContext(ctx, `SELECT id, key, x1, y1, x2, y2, score, CASE WHEN person>0 THEN person ELSE 0 END, manual FROM ix_faces ORDER BY key, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			var r struct {
				Face   int64          `json:"face"`
				File   ixDataPostFile `json:"file"`
				Box    [4]float64     `json:"box"` // x1, y1, x2, y2 as shares of the picture's width and height
				Score  float64        `json:"score"`
				Person int64          `json:"person,omitempty"`
				Manual bool           `json:"placed_by_person,omitempty"`
			}
			if err := rows.Scan(&r.Face, &key, &r.Box[0], &r.Box[1], &r.Box[2], &r.Box[3], &r.Score, &r.Person, &r.Manual); err != nil {
				return err
			}
			f, ok := fileOf(key)
			if !ok {
				continue
			}
			r.File = f
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	return writeJSONL("media_text.jsonl", func(enc *json.Encoder) error {
		rows, err := a.db.Reader.QueryContext(ctx, `SELECT key, 'picture', '', text, '' FROM ix_ocr WHERE text<>''
			UNION ALL SELECT key, 'speech', language, text, segments FROM ix_stt WHERE text<>'' ORDER BY 1, 2`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key, segs string
			var r struct {
				File     ixDataPostFile  `json:"file"`
				Kind     string          `json:"kind"` // picture (text read from it) or speech (written out)
				Language string          `json:"language,omitempty"`
				Text     string          `json:"text"`
				Segments json.RawMessage `json:"segments,omitempty"`
			}
			if err := rows.Scan(&key, &r.Kind, &r.Language, &r.Text, &segs); err != nil {
				return err
			}
			f, ok := fileOf(key)
			if !ok {
				continue
			}
			r.File = f
			if segs != "" && segs != "null" {
				r.Segments = json.RawMessage(segs)
			}
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}
