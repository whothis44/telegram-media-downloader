package app

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The browser's read model: every media post (an album counts once) with
// its files, each file's duplicate cluster, and repost families. It is
// built in memory from the archive tables and rebuilt when they change;
// nothing in it is ever written back, so it can always be thrown away.
//
// Clusters: files with the same SHA-256 are one file already; pictures the
// picture index grouped, videos found to be the same video, and pairs a
// person confirmed are joined. A held-back download joins the cluster of
// the saved file it probably copies until it arrives and is checked.
//
// Families: posts are taken oldest first (by the original date of a
// forward; files whose message was never captured come last). A post whose files were all shown before is a repost of the
// family that showed most of them. A post sharing at least half of its
// files (and at least two) with one family adds its new files to that
// family, so overlapping albums merge. Anything else starts a family.
// Clusters seen in more than 50 posts across more than 10 chats (logos,
// stickers, promo cards) are "generic" and ignored, so they cannot weld
// unrelated posts together.

const (
	ixGenericPosts  = 50
	ixGenericChats  = 10
	ixFamilyMaxSize = 60
	ixCaptionMax    = 400
)

type ixMItem struct {
	ID      int64  // the download's ID (0: not downloaded)
	Group   string // chat
	Msg     int
	Key     string // file key (h:sha or p:path); "" when not downloaded
	Cluster int32
	Type    string // photo, video, document, audio, voice, sticker...
	Path    string
	Name    string
	Size    int64
	W, H    int
	Dur     float64
	Status  string // ok, subst (same bytes as another upload), held (waiting: probably a copy), missing
	Like    string // held: the saved file's key
	Thumb   bool   // the picture index made a grid thumbnail
	Ident   string // not downloaded: Telegram's file identity (kind:id:size)
	Post    int32
	Placed  bool // a lone file placed in the album beside it (ix_orphans.go)
}

type ixMPost struct {
	Idx     int32
	Group   string
	Msg     int   // first message
	Msgs    []int // all messages of the album
	Grouped string
	Date    int64 // when posted in this chat
	Origin  int64 // when first posted (a forward's original date)
	Fwd     string
	Caption string
	Sender  string
	Topic   int
	Deleted bool
	Orphan  bool // a file whose message was never captured: dated by its download
	Items   []int32
	Family  int32
	Role    string // original, repost, adds
}

type ixMFamily struct {
	ID     int32
	Lead   int32   // the post that started it
	Posts  []int32 // oldest first
	Items  []int32 // canonical item of each cluster, in the order they appeared
	Intro  []int32 // the post that brought each of Items
	First  int64
	Last   int64
	Chats  int
	Merged bool // more than one post brought new files
}

type ixMCluster struct {
	Canon   int32 // best item: most pixels, then most bytes; or the keeper chosen
	Items   []int32
	Posts   int
	Chats   int
	Generic bool
	Kind    string // photo, video, other
}

type ixModel struct {
	Built        time.Time
	Took         time.Duration
	Stamp        string
	Items        []ixMItem
	Posts        []ixMPost
	Families     []ixMFamily
	Clusters     []ixMCluster
	ByMsg        map[string]int32 // "group:msg" -> post
	ByKey        map[string]int32 // file key -> an item with it
	ByID         map[int64]int32  // download ID -> item
	GroupName    map[string]string
	GroupCat     map[string]string
	Groups       []string
	Suggest      map[string]ixCatSuggest   // a category per chat, from what it posts
	Sets         []ixMSet                  // families that share pictures, oldest first
	PersonKeys   map[int64]map[string]bool // person -> files they appear in (best copies)
	PersonName   map[int64]string
	PersonHidden map[int64]bool // hidden people (a presenter, a cartoon) do not tie sets together
	FamSet       []int32        // family -> set (-1: none)
}

type ixModelBox struct {
	mu       sync.Mutex
	model    *ixModel
	building bool
	err      string
}

var ixModelState sync.Map // *App -> *ixModelBox

func (a *App) ixModelBox() *ixModelBox {
	v, _ := ixModelState.LoadOrStore(a, &ixModelBox{})
	return v.(*ixModelBox)
}

// ixModelLast is the last model built, without building one (nil before
// the first).
func (a *App) ixModelLast() *ixModel {
	box := a.ixModelBox()
	box.mu.Lock()
	defer box.mu.Unlock()
	return box.model
}

// ixModelNow returns the current model, building it the first time.
func (a *App) ixModelNow(ctx context.Context) (*ixModel, error) {
	box := a.ixModelBox()
	box.mu.Lock()
	m := box.model
	box.mu.Unlock()
	if m != nil {
		return m, nil
	}
	return a.ixRebuildModel(ctx)
}

func (a *App) ixRebuildModel(ctx context.Context) (*ixModel, error) {
	box := a.ixModelBox()
	box.mu.Lock()
	box.building = true
	box.mu.Unlock()
	m, err := a.ixBuildModel(ctx)
	box.mu.Lock()
	defer box.mu.Unlock()
	box.building = false
	if err != nil {
		box.err = err.Error()
		return box.model, err
	}
	box.err = ""
	box.model = m
	return m, nil
}

// ixModelStamp changes whenever anything the model is built from changes.
func (a *App) ixModelStamp(ctx context.Context) string {
	var parts []string
	for _, q := range []string{
		`SELECT COALESCE(MAX(rowid),0)||'/'||COUNT(*)||'/'||COALESCE(MAX(seen_at),0)||'/'||COALESCE(MAX(deleted_at),0) FROM ix_messages`,
		`SELECT COALESCE(MAX(id),0)||'/'||COUNT(*)||'/'||COALESCE(SUM(length(file_hash)),0) FROM downloads`,
		`SELECT COUNT(*)||'/'||COALESCE(MAX(updated_at),0) FROM ix_pdq`,
		`SELECT COUNT(*)||'/'||COALESCE(MAX(found_at),0) FROM ix_vdup`,
		`SELECT COUNT(*)||'/'||COALESCE(MAX(decided_at),0) FROM ix_org_pairs`,
		`SELECT COUNT(*) FROM ix_org_keeper`,
		`SELECT COUNT(*)||'/'||COALESCE(MAX(at),0) FROM ix_org_set_posts`,
		`SELECT COUNT(*)||'/'||COALESCE(MAX(at),0) FROM ix_org_set_items`,
		`SELECT COUNT(*)/500 FROM ix_emb`, // the sets use embeddings: rebuild every 500 new ones, not for each
		`SELECT COUNT(*)||'/'||COALESCE(SUM(person),0)||'/'||COALESCE(MAX(id),0) FROM ix_faces WHERE person<>0`,
		`SELECT COALESCE(MAX(updated_at),0)||'/'||COUNT(*) FROM ix_people`,
		`SELECT COUNT(*)||'/'||COALESCE(MAX(released_at),0)||'/'||COALESCE(MAX(held_at),0) FROM ix_held`,
	} {
		var s string
		_ = a.db.Reader.QueryRowContext(ctx, q).Scan(&s)
		parts = append(parts, s)
	}
	if cfg, err := a.config.Load(ctx); err == nil {
		for _, g := range configuredGroupList(cfg) {
			parts = append(parts, toString(g["id"])+"="+toString(g["name"])+"/"+ixCategoryOf(g))
		}
	}
	return strings.Join(parts, ";")
}

// ixRunBrowse keeps the model current: it is rebuilt a few minutes after
// the archive changes.
func (a *App) ixRunBrowse(ctx context.Context) {
	wait := 3 * time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 5 * time.Minute
		if !ixVdupBackground.Load() {
			continue
		}
		box := a.ixModelBox()
		box.mu.Lock()
		m := box.model
		box.mu.Unlock()
		if m != nil && a.ixModelStamp(ctx) == m.Stamp {
			continue
		}
		if _, err := a.ixRebuildModel(ctx); err != nil && ctx.Err() == nil {
			a.ixLog("browser index: %v", err)
		}
	}
}

type ixUnion struct{ parent map[string]string }

func (u *ixUnion) find(k string) string {
	p, ok := u.parent[k]
	if !ok || p == k {
		return k
	}
	r := u.find(p)
	u.parent[k] = r
	return r
}

func (u *ixUnion) union(a, b string) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if rb < ra {
		ra, rb = rb, ra
	}
	u.parent[rb] = ra
	u.parent[ra] = ra
}

func (a *App) ixBuildModel(ctx context.Context) (*ixModel, error) {
	start := time.Now()
	m := &ixModel{ByMsg: map[string]int32{}, ByKey: map[string]int32{}, ByID: map[int64]int32{}, GroupName: map[string]string{}, GroupCat: map[string]string{}}
	m.Stamp = a.ixModelStamp(ctx)
	metas, err := a.ixGroups(ctx)
	if err != nil {
		return nil, err
	}
	filters := map[string]map[string]any{}
	for _, g := range metas {
		m.GroupName[g.ID] = g.Name
		m.GroupCat[g.ID] = g.Category
		m.Groups = append(m.Groups, g.ID)
		filters[g.ID] = g.Filters
	}
	// Files.
	type fileRow struct {
		item    ixMItem
		created int64
	}
	files := map[string]*fileRow{}
	// A Telegram file (kind:id:size) is the same bytes wherever it is
	// forwarded, so files not downloaded yet are known as copies of saved
	// ones, or of each other, before any download.
	identKey := map[string]string{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT d.id, d.group_id, d.message_id, COALESCE(d.file_type,''), d.file_path, COALESCE(d.file_hash,''), COALESCE(d.file_size,0),
			COALESCE(d.media_width,0), COALESCE(d.media_height,0), COALESCE(d.media_duration_ms,0), COALESCE(d.file_name,''), COALESCE(CAST(d.created_at AS TEXT),''),
			COALESCE(p.width,0), COALESCE(p.height,0), s.of_path IS NOT NULL AND s.of_path=d.file_path, COALESCE(p.thumb,'')<>'',
			CASE WHEN COALESCE(d.telegram_media_id,'')<>'' THEN COALESCE(d.telegram_media_kind,'')||':'||d.telegram_media_id||':'||COALESCE(d.telegram_media_size,0) ELSE '' END
		FROM downloads d LEFT JOIN ix_pdq p ON p.key = CASE WHEN length(d.file_hash)=64 THEN 'h:'||lower(d.file_hash) ELSE 'p:'||replace(d.file_path,char(92),'/') END
		LEFT JOIN ix_subst s ON s.group_id=d.group_id AND s.message_id=d.message_id
		WHERE d.message_id>0 AND d.file_type IS NOT 'stories' AND d.file_path IS NOT NULL AND d.file_path<>'' AND (d.status IS NULL OR d.status='completed')`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f fileRow
		var created, hash string
		var dur int64
		var pw, ph int
		var subst bool
		var ident string
		if err := rows.Scan(&f.item.ID, &f.item.Group, &f.item.Msg, &f.item.Type, &f.item.Path, &hash, &f.item.Size, &f.item.W, &f.item.H, &dur, &f.item.Name, &created, &pw, &ph, &subst, &f.item.Thumb, &ident); err != nil {
			rows.Close()
			return nil, err
		}
		f.item.Path = ixNormPath(f.item.Path)
		if f.item.Path == "" {
			continue
		}
		f.item.Key = ixFileKey(hash, f.item.Path)
		if ident != "" {
			identKey[ident] = f.item.Key
		}
		if f.item.W == 0 && pw > 0 {
			f.item.W, f.item.H = pw, ph
		}
		f.item.Dur = float64(dur) / 1000
		f.item.Status = "ok"
		if subst {
			f.item.Status = "subst"
		}
		f.created = int64(ixParseCreated(created))
		files[f.item.Group+":"+strconv.Itoa(f.item.Msg)] = &f
	}
	rows.Close()
	held := map[string]string{}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT group_id, message_id, like_key FROM ix_held WHERE released_at=0 AND reason<>'' AND like_key<>''`); err == nil {
		for rows.Next() {
			var g, k string
			var msg int
			if rows.Scan(&g, &msg, &k) == nil {
				held[g+":"+strconv.Itoa(msg)] = k
			}
		}
		rows.Close()
	}
	// Messages with media, grouped into posts.
	type postKey struct{ group, grouped string }
	postOf := map[postKey]int32{}
	addItem := func(p *ixMPost, it ixMItem) {
		it.Post = p.Idx
		m.Items = append(m.Items, it)
		ii := int32(len(m.Items) - 1)
		p.Items = append(p.Items, ii)
		if it.Key != "" {
			if _, ok := m.ByKey[it.Key]; !ok {
				m.ByKey[it.Key] = ii
			}
		}
		if it.ID > 0 {
			m.ByID[it.ID] = ii
		}
	}
	rows, err = a.db.Reader.QueryContext(ctx, `SELECT group_id, message_id, date, grouped_id, fwd_from, fwd_date, substr(text,1,?), media_type, media_name, media_size, deleted_at, sender_id, topic_id, COALESCE(media_key,'')
		FROM ix_messages WHERE media_type<>'' AND service='' ORDER BY group_id, message_id`, ixCaptionMax)
	if err != nil {
		return nil, err
	}
	seenFile := map[string]bool{}
	for rows.Next() {
		var g, grouped, fwd, text, mtype, mname, sender, mkey string
		var msg, topic int
		var date, fwdDate, deleted, msize int64
		if err := rows.Scan(&g, &msg, &date, &grouped, &fwd, &fwdDate, &text, &mtype, &mname, &msize, &deleted, &sender, &topic, &mkey); err != nil {
			rows.Close()
			return nil, err
		}
		mk := g + ":" + strconv.Itoa(msg)
		f := files[mk]
		if f == nil && !ixWanted(filters[g], mtype) {
			continue // a kind of file this chat does not keep
		}
		pk := postKey{g, grouped}
		if grouped == "" {
			pk.grouped = "m" + strconv.Itoa(msg)
		}
		pi, ok := postOf[pk]
		if !ok {
			pi = int32(len(m.Posts))
			origin := date
			if fwdDate > 0 && fwdDate < date {
				origin = fwdDate
			}
			m.Posts = append(m.Posts, ixMPost{Idx: pi, Group: g, Msg: msg, Grouped: grouped, Date: date, Origin: origin, Fwd: fwd, Sender: sender, Topic: topic, Family: -1})
			postOf[pk] = pi
		}
		p := &m.Posts[pi]
		p.Msgs = append(p.Msgs, msg)
		if p.Caption == "" && strings.TrimSpace(text) != "" {
			p.Caption = text
		}
		if deleted > 0 {
			p.Deleted = true
		}
		m.ByMsg[mk] = pi
		if f != nil {
			seenFile[mk] = true
			addItem(p, f.item)
			continue
		}
		it := ixMItem{Group: g, Msg: msg, Type: ixFilterKeyType(mtype), Name: mname, Size: msize, Status: "missing", Ident: mkey}
		if it.Size == 0 && mkey != "" {
			if i := strings.LastIndexByte(mkey, ':'); i > 0 {
				it.Size, _ = strconv.ParseInt(mkey[i+1:], 10, 64) // Telegram's size, from the file's identity
			}
		}
		if k, ok := held[mk]; ok {
			it.Status, it.Like = "held", k
		}
		addItem(p, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Files whose messages were never captured stand as posts of their own.
	var orphan []*fileRow
	for k, f := range files {
		if !seenFile[k] {
			orphan = append(orphan, f)
		}
	}
	sort.Slice(orphan, func(i, j int) bool {
		if orphan[i].item.Group != orphan[j].item.Group {
			return orphan[i].item.Group < orphan[j].item.Group
		}
		return orphan[i].item.Msg < orphan[j].item.Msg
	})
	// Unless it sits inside an album of its chat (ix_orphans.go).
	placer := newIxOrphanPlacer(m, a.ixOrphanNear(ctx), a.ixOrphansOut(ctx))
	for _, f := range orphan {
		if pi := placer.place(f.item.Group, f.item.Msg, f.created); pi >= 0 {
			f.item.Placed = true
			addItem(&m.Posts[pi], f.item)
			ixPlaceInAlbum(m, pi, f.item.Msg)
			m.ByMsg[f.item.Group+":"+strconv.Itoa(f.item.Msg)] = pi
			continue
		}
		pi := int32(len(m.Posts))
		m.Posts = append(m.Posts, ixMPost{Idx: pi, Group: f.item.Group, Msg: f.item.Msg, Msgs: []int{f.item.Msg}, Date: f.created, Origin: f.created, Family: -1, Orphan: true})
		m.ByMsg[f.item.Group+":"+strconv.Itoa(f.item.Msg)] = pi
		addItem(&m.Posts[pi], f.item)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Clusters.
	u := &ixUnion{parent: map[string]string{}}
	for _, q := range []string{
		`SELECT key, cluster FROM ix_mcl WHERE how IN ('auto','verified') AND key<>cluster`,
		`SELECT key_a, key_b FROM ix_vdup WHERE kind='same'`,
		`SELECT key_a, key_b FROM ix_org_pairs WHERE decision='same'`,
	} {
		if rows, err := a.db.Reader.QueryContext(ctx, q); err == nil {
			for rows.Next() {
				var x, y string
				if rows.Scan(&x, &y) == nil {
					u.union(x, y)
				}
			}
			rows.Close()
		}
	}
	keepers := map[string]string{}
	if rows, err := a.db.Reader.QueryContext(ctx, `SELECT cluster, key FROM ix_org_keeper`); err == nil {
		for rows.Next() {
			var c, k string
			if rows.Scan(&c, &k) == nil {
				keepers[c] = k
			}
		}
		rows.Close()
	}
	clusterOf := map[string]int32{}
	for i := range m.Items {
		it := &m.Items[i]
		root := ""
		switch {
		case it.Key != "":
			root = u.find(it.Key)
		case it.Like != "":
			root = u.find(it.Like)
		case it.Ident != "" && identKey[it.Ident] != "":
			root = u.find(identKey[it.Ident]) // the same Telegram file is saved elsewhere
		case it.Ident != "":
			root = "t:" + it.Ident // copies of one Telegram file, none saved yet
		default:
			root = "m:" + it.Group + ":" + strconv.Itoa(it.Msg)
		}
		ci, ok := clusterOf[root]
		if !ok {
			ci = int32(len(m.Clusters))
			clusterOf[root] = ci
			m.Clusters = append(m.Clusters, ixMCluster{Canon: -1})
		}
		it.Cluster = ci
		m.Clusters[ci].Items = append(m.Clusters[ci].Items, int32(i))
	}
	keeperKey := map[int32]string{}
	for c, k := range keepers {
		if ci, ok := clusterOf[u.find(c)]; ok {
			keeperKey[ci] = k
		}
	}
	for ci := range m.Clusters {
		c := &m.Clusters[ci]
		posts, chats := map[int32]bool{}, map[string]bool{}
		for _, ii := range c.Items {
			it := &m.Items[ii]
			posts[it.Post] = true
			chats[it.Group] = true
			if c.Kind == "" || c.Kind == "other" {
				switch it.Type {
				case "photo", "video":
					c.Kind = it.Type
				default:
					c.Kind = "other"
				}
			}
			if c.Canon < 0 || ixBetter(it, &m.Items[c.Canon]) {
				c.Canon = ii
			}
		}
		if k := keeperKey[int32(ci)]; k != "" {
			for _, ii := range c.Items {
				if m.Items[ii].Key == k {
					c.Canon = ii
					break
				}
			}
		}
		c.Posts, c.Chats = len(posts), len(chats)
		c.Generic = c.Posts > ixGenericPosts && c.Chats > ixGenericChats
	}
	ixAssignFamilies(m)
	m.Suggest = ixContentSuggestions(m)
	a.ixLoadPeople(ctx, m)
	ixAssignSets(m, a.ixLoadSetFixes(ctx), ixSetLikeness(m, a.ixEmbSim()))
	m.Built = time.Now()
	m.Took = time.Since(start)
	return m, nil
}

// ixBetter: the copy to show of a set of duplicates. Downloaded beats
// waiting; then more pixels, then more bytes (less compression), then
// longer.
func ixBetter(a, b *ixMItem) bool {
	if (a.Key != "") != (b.Key != "") {
		return a.Key != ""
	}
	pa, pb := a.W*a.H, b.W*b.H
	if pa != pb {
		return pa > pb
	}
	if a.Size != b.Size {
		return a.Size > b.Size
	}
	if a.Dur != b.Dur {
		return a.Dur > b.Dur
	}
	return a.ID != 0 && (b.ID == 0 || a.ID < b.ID)
}

// ixFilterKeyType maps a message's media type to the file type names used
// by the downloader.
func ixFilterKeyType(mediaType string) string {
	switch mediaType {
	case "photo", "video", "audio", "voice", "sticker":
		return mediaType
	case "gif":
		return "video"
	}
	return "document"
}

// ixAssignFamilies groups posts into repost families (see the top of the
// file).
func ixAssignFamilies(m *ixModel) {
	order := make([]int32, len(m.Posts))
	for i := range order {
		order[i] = int32(i)
	}
	// Captured posts come before files known only from a download (no
	// caption, no date of their own), so a family's original is a real post
	// whenever there is one.
	sort.SliceStable(order, func(i, j int) bool {
		a, b := &m.Posts[order[i]], &m.Posts[order[j]]
		if a.Orphan != b.Orphan {
			return !a.Orphan
		}
		if a.Origin != b.Origin {
			return a.Origin < b.Origin
		}
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Msg < b.Msg
	})
	famOf := map[int32]int32{} // cluster -> family that first showed it
	inFam := map[int32]map[int32]bool{}
	for _, pi := range order {
		p := &m.Posts[pi]
		var set []int32
		seen := map[int32]bool{}
		for _, ii := range p.Items {
			c := m.Items[ii].Cluster
			if seen[c] || m.Clusters[c].Generic {
				continue
			}
			seen[c] = true
			set = append(set, c)
		}
		if len(set) == 0 {
			// Only generic files: a family of its own, never merged.
			for _, ii := range p.Items {
				c := m.Items[ii].Cluster
				if !seen[c] {
					seen[c] = true
					set = append(set, c)
				}
			}
			ixNewFamily(m, p, set, nil, famOf, inFam)
			continue
		}
		count := map[int32]int{}
		var fresh []int32
		for _, c := range set {
			if f, ok := famOf[c]; ok {
				count[f]++
			} else {
				fresh = append(fresh, c)
			}
		}
		best, bestN := int32(-1), 0
		for f, n := range count {
			if n > bestN || (n == bestN && f < best) {
				best, bestN = f, n
			}
		}
		shared := len(set) - len(fresh)
		switch {
		case len(fresh) == 0:
			ixJoinFamily(m, p, best, nil, "repost", famOf, inFam)
		case best >= 0 && bestN*2 >= len(set) && bestN >= min(2, len(set)) && shared > 0 &&
			len(m.Families[best].Items)+len(fresh) <= ixFamilyMaxSize:
			ixJoinFamily(m, p, best, fresh, "adds", famOf, inFam)
		default:
			ixNewFamily(m, p, set, fresh, famOf, inFam)
		}
	}
	for fi := range m.Families {
		f := &m.Families[fi]
		chats := map[string]bool{}
		for _, pi := range f.Posts {
			chats[m.Posts[pi].Group] = true
			if d := m.Posts[pi].Date; d > f.Last {
				f.Last = d
			}
		}
		f.Chats = len(chats)
	}
}

func ixNewFamily(m *ixModel, p *ixMPost, set, fresh []int32, famOf map[int32]int32, inFam map[int32]map[int32]bool) {
	fi := int32(len(m.Families))
	f := ixMFamily{ID: fi, Lead: p.Idx, Posts: []int32{p.Idx}, First: p.Origin}
	inFam[fi] = map[int32]bool{}
	for _, c := range set {
		inFam[fi][c] = true
		f.Items = append(f.Items, m.Clusters[c].Canon)
		f.Intro = append(f.Intro, p.Idx)
		if _, ok := famOf[c]; !ok {
			famOf[c] = fi
		}
	}
	_ = fresh
	m.Families = append(m.Families, f)
	p.Family, p.Role = fi, "original"
}

func ixJoinFamily(m *ixModel, p *ixMPost, fi int32, fresh []int32, role string, famOf map[int32]int32, inFam map[int32]map[int32]bool) {
	f := &m.Families[fi]
	f.Posts = append(f.Posts, p.Idx)
	for _, c := range fresh {
		if inFam[fi][c] {
			continue
		}
		inFam[fi][c] = true
		f.Items = append(f.Items, m.Clusters[c].Canon)
		f.Intro = append(f.Intro, p.Idx)
		famOf[c] = fi
		f.Merged = true
	}
	p.Family, p.Role = fi, role
}

// ixCatSuggest is a category suggested from what a chat posts: the
// category whose chats hold the most of its pictures and videos.
type ixCatSuggest struct {
	Category string
	Share    float64 // of the chat's files, the part also in that category
	Shared   int
}

// ixContentSuggestions suggests a category for each chat whose files are
// largely copies of files in the chats of one category (at least 5 files
// and a quarter of its own).
func ixContentSuggestions(m *ixModel) map[string]ixCatSuggest {
	byGroup := map[string]map[int32]bool{}
	groupsOf := map[int32]map[string]bool{}
	for i := range m.Items {
		it := &m.Items[i]
		if it.Key == "" || m.Clusters[it.Cluster].Generic {
			continue
		}
		if byGroup[it.Group] == nil {
			byGroup[it.Group] = map[int32]bool{}
		}
		byGroup[it.Group][it.Cluster] = true
		if groupsOf[it.Cluster] == nil {
			groupsOf[it.Cluster] = map[string]bool{}
		}
		groupsOf[it.Cluster][it.Group] = true
	}
	out := map[string]ixCatSuggest{}
	for g, clusters := range byGroup {
		count := map[string]int{}
		for c := range clusters {
			cats := map[string]bool{}
			for other := range groupsOf[c] {
				if cat := m.GroupCat[other]; other != g && cat != "" && cat != ixUncategorised {
					cats[cat] = true
				}
			}
			for cat := range cats {
				count[cat]++
			}
		}
		best, n := "", 0
		for cat, k := range count {
			if k > n || (k == n && cat < best) {
				best, n = cat, k
			}
		}
		share := float64(n) / float64(len(clusters))
		if n >= 5 && share >= 0.25 {
			out[g] = ixCatSuggest{best, share, n}
		}
	}
	return out
}
