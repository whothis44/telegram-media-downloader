package app

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
)

// Albums, lone files and chats that overlap: the tables and routes shared
// by ix_orphans.go (lone files placed in albums), ix_shoots.go (the same
// album posted in several chats) and ix_overlap.go (which chats repeat
// which).

const ixSchemaAlbums = `
CREATE TABLE IF NOT EXISTS ix_album_orphan ( -- a lone file a person said is not part of the album beside it
	post     TEXT PRIMARY KEY, -- <group>:<message> of the lone file
	decision TEXT NOT NULL,    -- out
	at       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ix_set_sug ( -- lone files that look like they belong with an album
	post     TEXT PRIMARY KEY, -- <group>:<message> of the lone file's post
	key      TEXT NOT NULL,    -- the lone file
	other    TEXT NOT NULL,    -- first post of the album it looks like
	score    REAL NOT NULL DEFAULT 0,
	margin   REAL NOT NULL DEFAULT 0,
	why      TEXT NOT NULL DEFAULT '',
	decision TEXT NOT NULL DEFAULT '', -- '' to review, yes, no
	found_at INTEGER NOT NULL DEFAULT 0,
	at       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ix_set_sug_seen ( -- lone files already compared, and against how many others
	key   TEXT PRIMARY KEY,
	model TEXT NOT NULL,
	size  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ix_album_pairs ( -- two albums a person said are, or are not, the same
	post_a   TEXT NOT NULL, -- first posts of the two albums, post_a < post_b
	post_b   TEXT NOT NULL,
	decision TEXT NOT NULL, -- same, different
	at       INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (post_a, post_b)
);
`

// ixEnsureAlbumSchema creates the tables above (called from ixEnsureSchema).
func ixEnsureAlbumSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, ixSchemaAlbums)
	return err
}

func registerIxAlbumRoutes(mux *http.ServeMux, a *App) {
	h := func(f http.HandlerFunc) http.Handler { return a.requireAdmin(f) }
	mux.Handle("POST /api/ix/browse/orphan", h(a.handleIxOrphanDecide))
	mux.Handle("GET /api/ix/browse/review/lone", h(a.handleIxLoneReview))
	mux.Handle("POST /api/ix/browse/review/lone", h(a.handleIxLoneDecide))
}

// ixPairRefs orders two post refs the way ix_album_pairs stores them.
func ixPairRefs(a, b string) (string, string) {
	if b < a {
		return b, a
	}
	return a, b
}

// ixLoadAlbumFixes adds a person's album decisions to the set corrections:
// albums said to be the same are tied; albums said to differ are never
// linked by look-alike pairs.
func (a *App) ixLoadAlbumFixes(ctx context.Context, f *ixSetFixes) {
	rows, err := a.db.Reader.QueryContext(ctx, `SELECT post_a, post_b, decision FROM ix_album_pairs`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var pa, pb, d string
		if rows.Scan(&pa, &pb, &d) != nil {
			continue
		}
		switch d {
		case "same":
			f.ties = append(f.ties, [2]string{pa, pb})
		case "different":
			f.apart = append(f.apart, [2]string{pa, pb})
		}
	}
}

// ixSplitPostRef splits "<group>:<message>" (the group may hold colons).
func ixSplitPostRef(ref string) (string, string, bool) {
	i := strings.LastIndexByte(ref, ':')
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}
