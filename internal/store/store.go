// Package store persists server state, history, notifications and cooldowns in SQLite (C3, C5).
package store

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// migrations run in order. PRAGMA user_version records how many have been applied.
var migrations = []string{`
CREATE TABLE servers (
	id   INTEGER PRIMARY KEY,
	addr TEXT NOT NULL UNIQUE
);
CREATE TABLE server_state (
	server_id   INTEGER PRIMARY KEY REFERENCES servers(id),
	online      INTEGER NOT NULL,
	title       TEXT NOT NULL,
	map         TEXT NOT NULL,
	players     INTEGER NOT NULL,
	max_players INTEGER NOT NULL,
	names_known INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL -- unix ms of the last poll
);
CREATE TABLE players (
	id   INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE
);
-- One row per visit. last_seen is the last poll that saw the player; while open=1 the
-- visit is still running and last_seen is not touched, so polls do not write per player.
CREATE TABLE sessions (
	id         INTEGER PRIMARY KEY,
	server_id  INTEGER NOT NULL REFERENCES servers(id),
	player_id  INTEGER NOT NULL REFERENCES players(id),
	first_seen INTEGER NOT NULL,
	last_seen  INTEGER NOT NULL,
	open       INTEGER NOT NULL
);
CREATE INDEX sessions_open ON sessions(server_id) WHERE open = 1;
CREATE INDEX sessions_player ON sessions(player_id, server_id);
-- History is written only when the value changes.
CREATE TABLE map_history    (id INTEGER PRIMARY KEY, server_id INTEGER NOT NULL REFERENCES servers(id), map    TEXT    NOT NULL, at INTEGER NOT NULL);
CREATE TABLE title_history  (id INTEGER PRIMARY KEY, server_id INTEGER NOT NULL REFERENCES servers(id), title  TEXT    NOT NULL, at INTEGER NOT NULL);
CREATE TABLE status_history (id INTEGER PRIMARY KEY, server_id INTEGER NOT NULL REFERENCES servers(id), online INTEGER NOT NULL, at INTEGER NOT NULL);
CREATE INDEX map_history_server ON map_history(server_id, map);
CREATE TABLE notifications (
	id           INTEGER PRIMARY KEY,
	user_id      TEXT    NOT NULL,
	server_id    INTEGER NOT NULL REFERENCES servers(id),
	kind         TEXT    NOT NULL,
	text         TEXT    NOT NULL DEFAULT '',
	op           TEXT    NOT NULL DEFAULT '',
	n            INTEGER NOT NULL DEFAULT 0,
	online       INTEGER NOT NULL DEFAULT 0,
	cooldown_sec INTEGER NOT NULL DEFAULT 0,
	armed        INTEGER NOT NULL DEFAULT 1,
	last_fired   INTEGER NOT NULL DEFAULT 0 -- unix ms, 0 = never
);
CREATE INDEX notifications_server ON notifications(server_id);
-- The 5 s minimum gap between messages to one user (C2).
CREATE TABLE user_cooldowns (
	user_id   TEXT PRIMARY KEY,
	last_sent INTEGER NOT NULL -- unix ms
);
`, `
ALTER TABLE notifications ADD COLUMN delivery TEXT NOT NULL DEFAULT 'channel';
ALTER TABLE notifications ADD COLUMN channel_id TEXT NOT NULL DEFAULT '';
ALTER TABLE notifications ADD COLUMN undeliverable INTEGER NOT NULL DEFAULT 0;
`, `
-- Who listens to which game server. scope is "guild:<id>" or "user:<id>" (a DM with the bot).
CREATE TABLE listeners (
	scope     TEXT    NOT NULL,
	server_id INTEGER NOT NULL REFERENCES servers(id),
	added_by  TEXT    NOT NULL,
	added_at  INTEGER NOT NULL,
	PRIMARY KEY (scope, server_id)
);
ALTER TABLE notifications ADD COLUMN scope TEXT NOT NULL DEFAULT '';
`, `
ALTER TABLE server_state ADD COLUMN source  TEXT    NOT NULL DEFAULT 'a2s';
ALTER TABLE server_state ADD COLUMN data_at INTEGER NOT NULL DEFAULT 0;
`, `
-- A notification holds one or more conditions that all have to hold. The old single-condition
-- columns of notifications (kind, text, op, n, online, armed) are no longer read.
CREATE TABLE conditions (
	id              INTEGER PRIMARY KEY,
	notification_id INTEGER NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
	kind            TEXT    NOT NULL,
	text            TEXT    NOT NULL DEFAULT '',
	op              TEXT    NOT NULL DEFAULT '',
	n               INTEGER NOT NULL DEFAULT 0,
	online          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX conditions_notification ON conditions(notification_id);
INSERT INTO conditions(notification_id, kind, text, op, n, online)
	SELECT id, kind, text, op, n, online FROM notifications;
-- phase: 0 new, 1 armed, 2 spent. Keep where an existing notification was.
ALTER TABLE notifications ADD COLUMN phase INTEGER NOT NULL DEFAULT 0;
UPDATE notifications SET phase = CASE WHEN armed = 1 THEN 1 ELSE 2 END;
`, `
ALTER TABLE notifications ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;
`, `
-- A notification for all servers has server_id 0, a row that stands for "every server". Its phase and
-- last firing are kept for each game server in notification_state.
INSERT OR IGNORE INTO servers(id, addr) VALUES (0, '*all servers*');
CREATE TABLE notification_state (
	notification_id INTEGER NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
	server_id       INTEGER NOT NULL REFERENCES servers(id),
	phase           INTEGER NOT NULL DEFAULT 0,
	last_fired      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (notification_id, server_id)
);
`, `
-- Settings an admin of a Discord server can change with /admin settings. scope is "guild:<id>".
CREATE TABLE guild_settings (
	scope               TEXT PRIMARY KEY,
	channel_delivery    INTEGER NOT NULL DEFAULT 1,
	default_delivery    TEXT    NOT NULL DEFAULT 'channel',
	default_cooldown_sec INTEGER NOT NULL DEFAULT 0,
	active              INTEGER NOT NULL DEFAULT 1
);
-- Player names nobody in that Discord server can track.
CREATE TABLE guild_blacklist (
	scope    TEXT    NOT NULL,
	name_key TEXT    NOT NULL, -- lower case, for matching
	name     TEXT    NOT NULL,
	added_by TEXT    NOT NULL,
	added_at INTEGER NOT NULL,
	PRIMARY KEY (scope, name_key)
);
`, `
-- The players seen on each server, so their names are found without reading every session.
CREATE TABLE server_players (
	server_id INTEGER NOT NULL REFERENCES servers(id),
	player_id INTEGER NOT NULL REFERENCES players(id),
	PRIMARY KEY (server_id, player_id)
) WITHOUT ROWID;
INSERT OR IGNORE INTO server_players(server_id, player_id) SELECT DISTINCT server_id, player_id FROM sessions;
`, `
-- Whether the GameTracker fallback may be used for the game servers a Discord server watches.
ALTER TABLE guild_settings ADD COLUMN gametracker INTEGER NOT NULL DEFAULT 1;
`}

type Store struct{ db *sql.DB }

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; keeps transactions simple
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		// PRAGMA does not take bound parameters.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// UpsertServer returns the id for addr, creating the row if needed.
func (s *Store) UpsertServer(addr string) (int64, error) {
	addr = strings.TrimSpace(addr)
	if _, err := s.db.Exec("INSERT OR IGNORE INTO servers(addr) VALUES (?)", addr); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRow("SELECT id FROM servers WHERE addr = ?", addr).Scan(&id)
	return id, err
}
