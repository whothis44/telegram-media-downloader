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
CREATE TABLE IF NOT EXISTS ix_retry (
	work_key TEXT PRIMARY KEY,
	rounds   INTEGER NOT NULL DEFAULT 0,
	last_at  INTEGER NOT NULL DEFAULT 0
);
`

func ixEnsureSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, ixSchema); err != nil {
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
