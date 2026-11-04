package app

import (
	"context"
	"net/http"
	"strconv"
)

// Lone files placed in albums.
//
// A file whose message was never captured (an "orphan") is shown as a post
// of its own. In Telegram an album's messages have consecutive IDs and at
// most 10 files, so an orphan whose nearest captured neighbours in the same
// chat, on both sides that have one within 9 IDs, are messages of one album
// is almost certainly part of it. It is shown in that album with a small
// "probably part of this album" label; a person can say it is not, and it
// stands alone again.

const (
	ixOrphanReach = 9   // an album's messages lie within 9 IDs of each other
	ixAlbumMax    = 10  // files in one album at most
	ixOrphanEarly = 600 // seconds: a file cannot be saved before it was posted (allowing clock slack)
)

// ixMsgNear is a captured message near an orphan: its ID and album.
type ixMsgNear struct {
	ID      int
	Grouped string
}

// ixOrphanPlacer decides, for each orphan in turn, the album post it
// belongs to. near, when not nil, lists every captured message (with or
// without media) within reach of an ID; without it only the media posts of
// the model are looked at.
type ixOrphanPlacer struct {
	m     *ixModel
	near  func(group string, lo, hi int) []ixMsgNear
	out   map[string]bool // orphans a person said stand alone
	added map[string]bool // orphans already placed (not captured, so never a neighbour)
}

func newIxOrphanPlacer(m *ixModel, near func(string, int, int) []ixMsgNear, out map[string]bool) *ixOrphanPlacer {
	return &ixOrphanPlacer{m: m, near: near, out: out, added: map[string]bool{}}
}

// fromModel lists the media posts' messages within reach of msg.
func (o *ixOrphanPlacer) fromModel(group string, msg int) []ixMsgNear {
	var out []ixMsgNear
	for d := -ixOrphanReach; d <= ixOrphanReach; d++ {
		if d == 0 {
			continue
		}
		k := group + ":" + strconv.Itoa(msg+d)
		if o.added[k] {
			continue
		}
		if pi, ok := o.m.ByMsg[k]; ok && !o.m.Posts[pi].Orphan {
			out = append(out, ixMsgNear{msg + d, o.m.Posts[pi].Grouped})
		}
	}
	return out
}

// place returns the album post an orphan belongs to, or -1. saved is when
// the file was downloaded (0: unknown).
func (o *ixOrphanPlacer) place(group string, msg int, saved int64) int32 {
	ref := group + ":" + strconv.Itoa(msg)
	if o.out[ref] {
		return -1
	}
	pi := o.album(group, msg, o.fromModel(group, msg))
	if pi < 0 {
		return -1
	}
	// Messages without media (or filtered out) also break an album.
	if o.near != nil {
		var near []ixMsgNear
		for _, n := range o.near(group, msg-ixOrphanReach, msg+ixOrphanReach) {
			if n.ID != msg && !o.added[group+":"+strconv.Itoa(n.ID)] {
				near = append(near, n)
			}
		}
		if o.album(group, msg, near) != pi {
			return -1
		}
	}
	p := &o.m.Posts[pi]
	lo, hi := msg, msg
	for _, x := range p.Msgs {
		lo, hi = min(lo, x), max(hi, x)
	}
	if len(p.Msgs)+1 > ixAlbumMax || hi-lo > ixOrphanReach {
		return -1
	}
	if saved > 0 && p.Date > 0 && saved < p.Date-ixOrphanEarly {
		return -1
	}
	o.added[ref] = true
	return pi
}

// album: the album both nearest neighbours (or the only one) belong to.
func (o *ixOrphanPlacer) album(group string, msg int, near []ixMsgNear) int32 {
	var lo, hi *ixMsgNear
	for i := range near {
		n := &near[i]
		if n.ID < msg && msg-n.ID <= ixOrphanReach && (lo == nil || n.ID > lo.ID) {
			lo = n
		}
		if n.ID > msg && n.ID-msg <= ixOrphanReach && (hi == nil || n.ID < hi.ID) {
			hi = n
		}
	}
	if lo == nil && hi == nil {
		return -1
	}
	grouped := ""
	pi := int32(-1)
	for _, n := range []*ixMsgNear{lo, hi} {
		if n == nil {
			continue
		}
		if n.Grouped == "" || grouped != "" && n.Grouped != grouped {
			return -1
		}
		grouped = n.Grouped
		if x, ok := o.m.ByMsg[group+":"+strconv.Itoa(n.ID)]; ok && pi < 0 {
			pi = x
		}
	}
	if pi < 0 || o.m.Posts[pi].Orphan || o.m.Posts[pi].Grouped != grouped || o.m.Posts[pi].Group != group {
		return -1
	}
	return pi
}

// ixPlaceInAlbum adds a placed orphan's message to its album post, keeping
// the album's files in message order.
func ixPlaceInAlbum(m *ixModel, pi int32, msg int) {
	p := &m.Posts[pi]
	p.Msgs = append(p.Msgs, msg)
	for i := len(p.Msgs) - 1; i > 0 && p.Msgs[i] < p.Msgs[i-1]; i-- {
		p.Msgs[i], p.Msgs[i-1] = p.Msgs[i-1], p.Msgs[i]
	}
	for i := len(p.Items) - 1; i > 0 && m.Items[p.Items[i]].Msg < m.Items[p.Items[i-1]].Msg; i-- {
		p.Items[i], p.Items[i-1] = p.Items[i-1], p.Items[i]
	}
}

// ixOrphanNear reads the captured messages of a chat between two IDs
// (one prepared query, used for every orphan); done releases it.
func (a *App) ixOrphanNear(ctx context.Context) (near func(string, int, int) []ixMsgNear, done func()) {
	st, err := a.db.Reader.PrepareContext(ctx, `SELECT message_id, grouped_id FROM ix_messages WHERE group_id=? AND message_id BETWEEN ? AND ?`)
	if err != nil {
		return nil, func() {}
	}
	return func(group string, lo, hi int) []ixMsgNear {
		rows, err := st.QueryContext(ctx, group, lo, hi)
		if err != nil {
			return nil
		}
		defer rows.Close()
		var out []ixMsgNear
		for rows.Next() {
			var n ixMsgNear
			if rows.Scan(&n.ID, &n.Grouped) == nil {
				out = append(out, n)
			}
		}
		return out
	}, func() { st.Close() }
}

// ixOrphansOut lists the lone files a person said are not part of an album.
func (a *App) ixOrphansOut(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT post FROM ix_album_orphan WHERE decision='out'`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out[p] = true
		}
	}
	return out
}

// ixOrphanDecide records that a lone file is not part of the album it was
// placed in ("out"), or returns it to automatic placing ("auto").
func (a *App) ixOrphanDecide(ctx context.Context, group string, msg int, decision string) error {
	if group == "" || msg <= 0 {
		return errIxBadRef
	}
	ref := group + ":" + strconv.Itoa(msg)
	var err error
	switch decision {
	case "out":
		_, err = a.db.Writer.ExecContext(ctx, `INSERT INTO ix_album_orphan(post,decision,at) VALUES(?,'out',?) ON CONFLICT(post) DO UPDATE SET decision='out', at=excluded.at`, ref, ixNowMs())
	case "auto":
		_, err = a.db.Writer.ExecContext(ctx, `DELETE FROM ix_album_orphan WHERE post=?`, ref)
	default:
		return errIxBadMode
	}
	if err == nil {
		a.ixOrgLog("orphan_placed", map[string]any{"post": ref, "decision": decision})
	}
	return err
}

func (a *App) handleIxOrphanDecide(w http.ResponseWriter, r *http.Request) {
	b := ixBody(r)
	msg, _ := strconv.Atoi(toString(b["msg"]))
	if err := a.ixOrphanDecide(r.Context(), toString(b["group"]), msg, toString(b["decision"])); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.ixModelSoon()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
