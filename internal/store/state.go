package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
)

// ms stores a time as unix milliseconds, and the zero time as 0.
func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// LastState returns the last stored state of a server, or nil if it has none.
// PlayerNames are the players in open sessions.
func (s *Store) LastState(serverID int64) (*rules.Snapshot, error) {
	snap, _, err := lastState(s.db, serverID)
	return snap, err
}

func lastState(q execer, serverID int64) (*rules.Snapshot, time.Time, error) {
	var (
		sn            rules.Snapshot
		online, known int
		updated       int64
	)
	var dataAt int64
	err := q.QueryRow(`SELECT online, title, map, players, max_players, names_known, updated_at, source, data_at
		FROM server_state WHERE server_id = ?`, serverID).
		Scan(&online, &sn.Title, &sn.Map, &sn.Players, &sn.MaxPlayers, &known, &updated, &sn.Source, &dataAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	sn.Online, sn.NamesKnown = online == 1, known == 1
	if dataAt != 0 {
		sn.DataAt = time.UnixMilli(dataAt)
	}
	rows, err := q.Query(`SELECT p.name FROM sessions s JOIN players p ON p.id = s.player_id
		WHERE s.server_id = ? AND s.open = 1 ORDER BY p.name`, serverID)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, time.Time{}, err
		}
		sn.PlayerNames = append(sn.PlayerNames, n)
	}
	return &sn, time.UnixMilli(updated), rows.Err()
}

// apply stores cur as the server's state. It writes history rows only for values
// that changed, opens and closes player sessions by comparing names, and returns
// the previous state (nil on the first poll) and the state as stored, which keeps
// the last known title and map when the server is offline.
func apply(tx execer, serverID int64, cur rules.Snapshot, now time.Time) (prev *rules.Snapshot, stored rules.Snapshot, err error) {
	prev, prevAt, err := lastState(tx, serverID)
	if err != nil {
		return nil, cur, err
	}
	// An offline poll keeps the last known title, map and counts.
	if !cur.Online && prev != nil {
		cur.Title, cur.Map, cur.MaxPlayers = prev.Title, prev.Map, prev.MaxPlayers
	}
	if prev == nil || prev.Online != cur.Online {
		if _, err := tx.Exec("INSERT INTO status_history(server_id, online, at) VALUES (?,?,?)", serverID, b2i(cur.Online), ms(now)); err != nil {
			return nil, cur, err
		}
	}
	if cur.Online {
		if prev == nil || prev.Map != cur.Map {
			if _, err := tx.Exec("INSERT INTO map_history(server_id, map, at) VALUES (?,?,?)", serverID, cur.Map, ms(now)); err != nil {
				return nil, cur, err
			}
		}
		if prev == nil || prev.Title != cur.Title {
			if _, err := tx.Exec("INSERT INTO title_history(server_id, title, at) VALUES (?,?,?)", serverID, cur.Title, ms(now)); err != nil {
				return nil, cur, err
			}
		}
	}

	// Sessions. When the player list was not retrieved, leave them untouched.
	// Offline means nobody is there, so every open session closes.
	if !cur.Online || cur.NamesKnown {
		names := map[string]bool{}
		if cur.Online {
			for _, n := range cur.PlayerNames {
				if n != "" {
					names[n] = true
				}
			}
		}
		if err := syncSessions(tx, serverID, names, prevAt, now); err != nil {
			return nil, cur, err
		}
	} else if prev != nil {
		cur.PlayerNames, cur.NamesKnown = prev.PlayerNames, prev.NamesKnown
	}

	source, dataAt := cur.Source, cur.DataAt
	if source == "" {
		source = "a2s"
	}
	if dataAt.IsZero() {
		dataAt = now
	}
	_, err = tx.Exec(`INSERT INTO server_state(server_id, online, title, map, players, max_players, names_known, updated_at, source, data_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(server_id) DO UPDATE SET online=excluded.online, title=excluded.title, map=excluded.map,
			players=excluded.players, max_players=excluded.max_players, names_known=excluded.names_known, updated_at=excluded.updated_at,
			source=excluded.source, data_at=excluded.data_at`,
		serverID, b2i(cur.Online), cur.Title, cur.Map, cur.Players, cur.MaxPlayers, b2i(cur.NamesKnown || !cur.Online), ms(now), source, ms(dataAt))
	return prev, cur, err
}

// syncSessions closes sessions of players no longer present (last_seen = the previous
// poll, the last time they were known to be there) and opens sessions for new ones.
func syncSessions(tx execer, serverID int64, names map[string]bool, prevAt, now time.Time) error {
	rows, err := tx.Query(`SELECT s.id, p.name FROM sessions s JOIN players p ON p.id = s.player_id
		WHERE s.server_id = ? AND s.open = 1`, serverID)
	if err != nil {
		return err
	}
	type open struct {
		id   int64
		name string
	}
	var opens []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.name); err != nil {
			rows.Close()
			return err
		}
		opens = append(opens, o)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	still := map[string]bool{}
	for _, o := range opens {
		if names[o.name] {
			still[o.name] = true
			continue
		}
		if _, err := tx.Exec("UPDATE sessions SET open = 0, last_seen = ? WHERE id = ?", ms(prevAt), o.id); err != nil {
			return err
		}
	}
	for n := range names {
		if still[n] {
			continue
		}
		if _, err := tx.Exec("INSERT OR IGNORE INTO players(name) VALUES (?)", n); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO sessions(server_id, player_id, first_seen, last_seen, open)
			SELECT ?, id, ?, ?, 1 FROM players WHERE name = ?`, serverID, ms(now), ms(now), n); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO server_players(server_id, player_id)
			SELECT ?, id FROM players WHERE name = ?`, serverID, n); err != nil {
			return err
		}
	}
	return nil
}

// Apply stores cur for a server (see apply) in one transaction.
func (s *Store) Apply(serverID int64, cur rules.Snapshot, now time.Time) (*rules.Snapshot, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	prev, _, err := apply(tx, serverID, cur, now)
	if err != nil {
		return nil, err
	}
	return prev, tx.Commit()
}

// Maps returns every map the server has been seen on.
func (s *Store) Maps(serverID int64) ([]string, error) {
	return s.strings("SELECT DISTINCT map FROM map_history WHERE server_id = ?", serverID)
}

// PlayerNames returns every player name seen on the server. It reads server_players, which grows
// with the players, not with their visits.
func (s *Store) PlayerNames(serverID int64) ([]string, error) {
	return s.strings(`SELECT p.name FROM server_players sp JOIN players p ON p.id = sp.player_id WHERE sp.server_id = ?`, serverID)
}

func (s *Store) strings(q string, args ...any) ([]string, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
