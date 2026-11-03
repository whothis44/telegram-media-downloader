package app

import (
	"context"
	"database/sql"
)

// The message archive ("ix") keeps what the downloader used to throw away:
// every message's text, dates, sender, album, reply and forum topic, plus
// sender and topic names. It lives in its own tables, created here rather
// than in the numbered store migrations, so the original application can
// still open the same database (it simply ignores these tables).
const ixSchema = `
CREATE TABLE IF NOT EXISTS ix_messages (
	group_id    TEXT    NOT NULL,
	message_id  INTEGER NOT NULL,
	date        INTEGER NOT NULL DEFAULT 0,
	edit_date   INTEGER NOT NULL DEFAULT 0,
	text        TEXT    NOT NULL DEFAULT '',
	grouped_id  TEXT    NOT NULL DEFAULT '',
	sender_id   TEXT    NOT NULL DEFAULT '',
	post_author TEXT    NOT NULL DEFAULT '',
	fwd_from    TEXT    NOT NULL DEFAULT '',
	fwd_date    INTEGER NOT NULL DEFAULT 0,
	reply_to    INTEGER NOT NULL DEFAULT 0,
	topic_id    INTEGER NOT NULL DEFAULT 0,
	media_type  TEXT    NOT NULL DEFAULT '',
	media_name  TEXT    NOT NULL DEFAULT '',
	media_size  INTEGER NOT NULL DEFAULT 0,
	media_key   TEXT    NOT NULL DEFAULT '',
	views       INTEGER NOT NULL DEFAULT 0,
	tl          BLOB,
	first_seen  INTEGER NOT NULL DEFAULT 0,
	seen_at     INTEGER NOT NULL DEFAULT 0,
	deleted_at  INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (group_id, message_id)
);
CREATE INDEX IF NOT EXISTS ix_messages_date ON ix_messages(group_id, date);
CREATE INDEX IF NOT EXISTS ix_messages_grouped ON ix_messages(group_id, grouped_id) WHERE grouped_id <> '';
CREATE TABLE IF NOT EXISTS ix_message_edits (
	group_id    TEXT    NOT NULL,
	message_id  INTEGER NOT NULL,
	edit_date   INTEGER NOT NULL DEFAULT 0,
	text        TEXT    NOT NULL DEFAULT '',
	recorded_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS ix_message_edits_msg ON ix_message_edits(group_id, message_id);
CREATE TABLE IF NOT EXISTS ix_peers (
	peer_id    TEXT PRIMARY KEY,
	kind       TEXT NOT NULL DEFAULT '',
	name       TEXT NOT NULL DEFAULT '',
	username   TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ix_topics (
	group_id   TEXT    NOT NULL,
	topic_id   INTEGER NOT NULL,
	title      TEXT    NOT NULL DEFAULT '',
	title_date INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (group_id, topic_id)
);
CREATE TABLE IF NOT EXISTS ix_walks (
	job_id      TEXT PRIMARY KEY,
	group_id    TEXT    NOT NULL,
	started_at  INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0,
	processed   INTEGER NOT NULL DEFAULT 0,
	marked_deleted INTEGER NOT NULL DEFAULT 0,
	state       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_walks_group ON ix_walks(group_id, finished_at);
CREATE TABLE IF NOT EXISTS ix_vfp (
	key        TEXT PRIMARY KEY,
	file_path  TEXT    NOT NULL DEFAULT '',
	duration   REAL    NOT NULL DEFAULT 0,
	frames     BLOB,
	status     TEXT    NOT NULL DEFAULT '',
	error      TEXT    NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ix_vband (
	band INTEGER NOT NULL,
	v    INTEGER NOT NULL,
	key  TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_vband_value ON ix_vband(band, v);
CREATE INDEX IF NOT EXISTS ix_vband_key ON ix_vband(key);
CREATE TABLE IF NOT EXISTS ix_vdup (
	key_a    TEXT    NOT NULL,
	key_b    TEXT    NOT NULL,
	kind     TEXT    NOT NULL DEFAULT '',
	score    REAL    NOT NULL DEFAULT 0,
	matched  INTEGER NOT NULL DEFAULT 0,
	offset_s REAL    NOT NULL DEFAULT 0,
	found_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (key_a, key_b)
);
CREATE TABLE IF NOT EXISTS ix_group_info (
	group_id     TEXT PRIMARY KEY,
	title        TEXT    NOT NULL DEFAULT '',
	kind         TEXT    NOT NULL DEFAULT '', -- channel, supergroup, forum or group
	about        TEXT    NOT NULL DEFAULT '',
	username     TEXT    NOT NULL DEFAULT '',
	members      INTEGER NOT NULL DEFAULT 0,
	pinned_id    INTEGER NOT NULL DEFAULT 0,
	linked_group TEXT    NOT NULL DEFAULT '',
	info_at      INTEGER NOT NULL DEFAULT 0,
	tl           BLOB,
	tg_count     INTEGER NOT NULL DEFAULT 0,
	tg_count_at  INTEGER NOT NULL DEFAULT 0,
	finished_at  INTEGER NOT NULL DEFAULT 0 -- marked finished: rescans leave it alone
);
CREATE TABLE IF NOT EXISTS ix_alerts (
	key     TEXT PRIMARY KEY,
	sent_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ix_retry (
	work_key TEXT PRIMARY KEY,
	rounds   INTEGER NOT NULL DEFAULT 0,
	last_at  INTEGER NOT NULL DEFAULT 0
);
`

// Tables added with the media index, the browser and the live-account
// checks (ix.6).
const ixSchema6 = `
CREATE TABLE IF NOT EXISTS ix_absent ( -- message IDs Telegram says are gone
	group_id   TEXT    NOT NULL,
	message_id INTEGER NOT NULL,
	checked_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (group_id, message_id)
);
CREATE TABLE IF NOT EXISTS ix_pdq ( -- one row per unique picture file
	key        TEXT PRIMARY KEY,
	file_path  TEXT    NOT NULL DEFAULT '',
	hash       BLOB,               -- PDQ, 32 bytes
	quality    INTEGER NOT NULL DEFAULT 0,
	width      INTEGER NOT NULL DEFAULT 0,
	height     INTEGER NOT NULL DEFAULT 0,
	luma       BLOB,               -- 32x32 grey, for checking close matches
	thumb      TEXT    NOT NULL DEFAULT '',
	status     TEXT    NOT NULL DEFAULT '',
	error      TEXT    NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ix_mcl ( -- picture clusters: each file and the leader it joined
	key      TEXT PRIMARY KEY,
	cluster  TEXT    NOT NULL,
	how      TEXT    NOT NULL DEFAULT '', -- leader, auto, verified
	distance INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS ix_mcl_cluster ON ix_mcl(cluster);
CREATE TABLE IF NOT EXISTS ix_msug ( -- close pairs a person should look at
	key_a    TEXT    NOT NULL,
	key_b    TEXT    NOT NULL,
	kind     TEXT    NOT NULL DEFAULT '', -- photo, video
	distance REAL    NOT NULL DEFAULT 0,
	found_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (key_a, key_b)
);
CREATE TABLE IF NOT EXISTS ix_subst ( -- files not downloaded because the library already has them
	group_id   TEXT    NOT NULL,
	message_id INTEGER NOT NULL,
	identity   TEXT    NOT NULL DEFAULT '',
	size       INTEGER NOT NULL DEFAULT 0,
	of_path    TEXT    NOT NULL DEFAULT '',
	how        TEXT    NOT NULL DEFAULT '', -- telegram-hashes, sampled-parts
	at         INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (group_id, message_id)
);
CREATE TABLE IF NOT EXISTS ix_subst_block ( -- identities to download for real
	identity TEXT PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS ix_held ( -- queued files held back as probable copies
	work_id    INTEGER PRIMARY KEY,
	group_id   TEXT    NOT NULL,
	message_id INTEGER NOT NULL,
	like_path  TEXT    NOT NULL DEFAULT '',
	like_key   TEXT    NOT NULL DEFAULT '',
	reason     TEXT    NOT NULL DEFAULT '',
	held_at    INTEGER NOT NULL DEFAULT 0,
	released_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS ix_held_msg ON ix_held(group_id, message_id);
CREATE TABLE IF NOT EXISTS ix_fixity ( -- the last integrity check of each stored file
	path       TEXT PRIMARY KEY,
	size       INTEGER NOT NULL DEFAULT 0,
	sha256     TEXT    NOT NULL DEFAULT '',
	checked_at INTEGER NOT NULL DEFAULT 0,
	result     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_fixity_checked ON ix_fixity(checked_at);
CREATE TABLE IF NOT EXISTS ix_fixity_events (
	at      INTEGER NOT NULL,
	path    TEXT    NOT NULL,
	result  TEXT    NOT NULL, -- ok, changed, missing, size, unreadable
	detail  TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ix_fixity_events_at ON ix_fixity_events(at);
CREATE TABLE IF NOT EXISTS ix_org_collections (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT    NOT NULL,
	created_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ix_org_items (
	collection_id INTEGER NOT NULL,
	ref           TEXT    NOT NULL, -- post:<group>:<message> or file:<key>
	added_at      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (collection_id, ref)
);
CREATE TABLE IF NOT EXISTS ix_org_pairs ( -- review decisions on two files
	key_a      TEXT NOT NULL,
	key_b      TEXT NOT NULL,
	decision   TEXT NOT NULL, -- same, different
	decided_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (key_a, key_b)
);
CREATE TABLE IF NOT EXISTS ix_org_keeper ( -- the copy to show for a set of duplicates
	cluster TEXT PRIMARY KEY,
	key     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS ix_move_plans (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	kind       TEXT    NOT NULL,
	title      TEXT    NOT NULL DEFAULT '',
	state      TEXT    NOT NULL DEFAULT 'planned', -- planned, applying, applied, undoing, undone, failed
	created_at INTEGER NOT NULL DEFAULT 0,
	applied_at INTEGER NOT NULL DEFAULT 0,
	undone_at  INTEGER NOT NULL DEFAULT 0,
	error      TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS ix_moves (
	plan_id INTEGER NOT NULL,
	seq     INTEGER NOT NULL,
	op      TEXT    NOT NULL, -- copy, move, trash
	src     TEXT    NOT NULL,
	dst     TEXT    NOT NULL,
	sha256  TEXT    NOT NULL DEFAULT '',
	size    INTEGER NOT NULL DEFAULT 0,
	repoint TEXT    NOT NULL DEFAULT '', -- downloads rows at src now point here
	state   TEXT    NOT NULL DEFAULT 'planned', -- planned, running, done, failed, undone
	error   TEXT    NOT NULL DEFAULT '',
	at      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (plan_id, seq)
);
`

// ixSchema7: sets (posts that share pictures although none holds them all)
// with a person's corrections, and image embeddings from an optional image
// recognition service.
const ixSchema7 = `
CREATE TABLE IF NOT EXISTS ix_org_set_posts ( -- a post taken out of sets, or tied to another
	post  TEXT PRIMARY KEY, -- <group>:<message>
	mode  TEXT NOT NULL,    -- alone, tie
	other TEXT NOT NULL DEFAULT '',
	at    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ix_org_set_items ( -- a file said to belong to a set, or not
	anchor   TEXT NOT NULL, -- the set's first post
	key      TEXT NOT NULL,
	decision TEXT NOT NULL, -- in, out
	at       INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (anchor, key)
);
CREATE TABLE IF NOT EXISTS ix_emb ( -- image embeddings (int8, L2-normalised before scaling)
	key   TEXT PRIMARY KEY,
	model TEXT NOT NULL,
	dim   INTEGER NOT NULL,
	vec   BLOB NOT NULL,
	at    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ix_emb_err (
	key   TEXT PRIMARY KEY,
	model TEXT NOT NULL,
	error TEXT NOT NULL DEFAULT '',
	tries INTEGER NOT NULL DEFAULT 0,
	at    INTEGER NOT NULL DEFAULT 0
);
`

// ixSchema8: faces and people, text read from pictures, speech written
// out, other image-recognition failures, and queue items put back for the
// deadline.
const ixSchema8 = `
CREATE TABLE IF NOT EXISTS ix_people (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT    NOT NULL DEFAULT '',
	hidden      INTEGER NOT NULL DEFAULT 0,
	merged_into INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL DEFAULT 0,
	updated_at  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ix_faces (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	key    TEXT    NOT NULL,
	model  TEXT    NOT NULL,
	x1 REAL NOT NULL, y1 REAL NOT NULL, x2 REAL NOT NULL, y2 REAL NOT NULL, -- share of the picture's width and height
	score  REAL    NOT NULL DEFAULT 0,
	vec    BLOB    NOT NULL,
	person INTEGER NOT NULL DEFAULT 0, -- 0: not grouped yet; -1: a person said it is nobody known
	manual INTEGER NOT NULL DEFAULT 0, -- placed by a person
	at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS ix_faces_key ON ix_faces(key);
CREATE INDEX IF NOT EXISTS ix_faces_person ON ix_faces(person);
CREATE TABLE IF NOT EXISTS ix_face_done (
	key   TEXT PRIMARY KEY,
	model TEXT NOT NULL,
	faces INTEGER NOT NULL DEFAULT 0,
	at    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ix_ocr (
	key   TEXT PRIMARY KEY,
	model TEXT NOT NULL,
	text  TEXT NOT NULL DEFAULT '',
	at    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ix_stt (
	key      TEXT PRIMARY KEY,
	model    TEXT NOT NULL,
	language TEXT NOT NULL DEFAULT '',
	text     TEXT NOT NULL DEFAULT '',
	segments TEXT NOT NULL DEFAULT '', -- JSON: [[start_s, end_s, text], ...]
	at       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ix_ml_err (
	task  TEXT NOT NULL,
	key   TEXT NOT NULL,
	model TEXT NOT NULL,
	error TEXT NOT NULL DEFAULT '',
	tries INTEGER NOT NULL DEFAULT 0,
	at    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (task, key)
);
CREATE TABLE IF NOT EXISTS ix_deferred ( -- queue items put back so smaller ones go first
	work_id     INTEGER PRIMARY KEY,
	band        INTEGER NOT NULL,
	deferred_at INTEGER NOT NULL DEFAULT 0,
	released_at INTEGER NOT NULL DEFAULT 0
);
`

// ixAddColumns adds the columns a table lacks.
func ixAddColumns(ctx context.Context, db *sql.DB, table string, cols [][2]string) error {
	for _, col := range cols {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, col[0]).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+col[0]+` `+col[1]); err != nil {
				return err
			}
		}
	}
	return nil
}

func ixEnsureSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, ixSchema); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, ixSchema6); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, ixSchema7); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, ixSchema8); err != nil {
		return err
	}
	if err := ixAddColumns(ctx, db, "ix_moves", [][2]string{{"rows", "TEXT NOT NULL DEFAULT ''"}, {"src_stored", "TEXT NOT NULL DEFAULT ''"}}); err != nil {
		return err
	}
	if err := ixAddColumns(ctx, db, "ix_group_info", [][2]string{{"invite_link", "TEXT NOT NULL DEFAULT ''"}}); err != nil {
		return err
	}
	if err := ixAddColumns(ctx, db, "ix_vdup", [][2]string{{"cover_a", "REAL NOT NULL DEFAULT 0"}, {"cover_b", "REAL NOT NULL DEFAULT 0"}}); err != nil {
		return err
	}
	// Video fingerprints before ix.6 sampled key frames only; they are all
	// made again on a fixed time grid, and their matches with them.
	var vfpVer int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('ix_vfp') WHERE name='ver'`).Scan(&vfpVer); err != nil {
		return err
	}
	if vfpVer == 0 {
		if _, err := db.ExecContext(ctx, `ALTER TABLE ix_vfp ADD COLUMN ver INTEGER NOT NULL DEFAULT 1; DELETE FROM ix_vfp; DELETE FROM ix_vdup; DELETE FROM ix_vband;`); err != nil {
			return err
		}
	}
	// Columns added after the first archive release.
	have := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('ix_messages')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			have[name] = true
		}
	}
	rows.Close()
	for _, col := range [][2]string{
		{"service", "TEXT NOT NULL DEFAULT ''"},    // service message: its action (pin, title change...)
		{"comments", "INTEGER NOT NULL DEFAULT 0"}, // channel post: comments Telegram reports
		{"comment_on", "TEXT NOT NULL DEFAULT ''"}, // comment: "<channel group id>:<post id>"
	} {
		if !have[col[0]] {
			if _, err := db.ExecContext(ctx, `ALTER TABLE ix_messages ADD COLUMN `+col[0]+` `+col[1]); err != nil {
				return err
			}
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS ix_messages_comment ON ix_messages(comment_on) WHERE comment_on <> ''`); err != nil {
		return err
	}
	ixEnsureSearch(ctx, db)
	return nil
}

const ixSearchTriggers = `
CREATE TRIGGER IF NOT EXISTS ix_fts_ai AFTER INSERT ON ix_messages BEGIN
	INSERT INTO ix_fts(rowid, text) VALUES (new.rowid, new.text);
END;
CREATE TRIGGER IF NOT EXISTS ix_fts_ad AFTER DELETE ON ix_messages BEGIN
	INSERT INTO ix_fts(ix_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
END;
CREATE TRIGGER IF NOT EXISTS ix_fts_au AFTER UPDATE OF text ON ix_messages BEGIN
	INSERT INTO ix_fts(ix_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
	INSERT INTO ix_fts(rowid, text) VALUES (new.rowid, new.text);
END;`

// ixEnsureSearch adds a full-text index of message text when the SQLite
// build supports FTS5. Without it, search falls back to a plain text scan.
func ixEnsureSearch(ctx context.Context, db *sql.DB) {
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name='ix_fts'`).Scan(&exists); err != nil {
		return
	}
	if exists == 0 {
		if _, err := db.ExecContext(ctx, `CREATE VIRTUAL TABLE ix_fts USING fts5(text, content='ix_messages', content_rowid='rowid', tokenize='unicode61 remove_diacritics 2')`); err != nil {
			return
		}
	}
	if _, err := db.ExecContext(ctx, ixSearchTriggers); err != nil {
		// Never leave a search table that is not kept up to date.
		_, _ = db.ExecContext(ctx, `DROP TRIGGER IF EXISTS ix_fts_ai; DROP TRIGGER IF EXISTS ix_fts_ad; DROP TRIGGER IF EXISTS ix_fts_au; DROP TABLE IF EXISTS ix_fts;`)
		return
	}
	if exists == 0 {
		_, _ = db.ExecContext(ctx, `INSERT INTO ix_fts(ix_fts) VALUES('rebuild')`)
	}
}
