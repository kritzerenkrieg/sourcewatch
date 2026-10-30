package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
)

// ServerRef is a game server as stored.
type ServerRef struct {
	ID   int64
	Addr string
}

// AddListener makes scope listen to the server at addr. added is false if it already did.
func (s *Store) AddListener(scope, addr, by string, now time.Time) (ref ServerRef, added bool, err error) {
	id, err := s.UpsertServer(addr)
	if err != nil {
		return ServerRef{}, false, err
	}
	res, err := s.db.Exec("INSERT OR IGNORE INTO listeners(scope, server_id, added_by, added_at) VALUES (?,?,?,?)", scope, id, by, ms(now))
	if err != nil {
		return ServerRef{}, false, err
	}
	n, _ := res.RowsAffected()
	return ServerRef{ID: id, Addr: addr}, n == 1, nil
}

// RemoveListener stops scope listening to addr and deletes the notifications it
// created for that server. removed is false if scope was not listening.
func (s *Store) RemoveListener(scope, addr string) (removed bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRow("SELECT id FROM servers WHERE addr = ?", addr).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	res, err := tx.Exec("DELETE FROM listeners WHERE scope = ? AND server_id = ?", scope, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	// What a notification for all servers had kept about this server goes too.
	if _, err := tx.Exec("DELETE FROM notification_state WHERE server_id = ? AND notification_id IN (SELECT id FROM notifications WHERE scope = ?)", id, scope); err != nil {
		return false, err
	}
	if _, err := tx.Exec("DELETE FROM notification_state WHERE notification_id IN (SELECT id FROM notifications WHERE scope = ? AND server_id = ?)", scope, id); err != nil {
		return false, err
	}
	if _, err := tx.Exec("DELETE FROM conditions WHERE notification_id IN (SELECT id FROM notifications WHERE scope = ? AND server_id = ?)", scope, id); err != nil {
		return false, err
	}
	if _, err := tx.Exec("DELETE FROM notifications WHERE scope = ? AND server_id = ?", scope, id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ListenerServers returns the game servers scope listens to, oldest first.
func (s *Store) ListenerServers(scope string) ([]ServerRef, error) {
	rows, err := s.db.Query(`SELECT s.id, s.addr FROM listeners l JOIN servers s ON s.id = l.server_id
		WHERE l.scope = ? ORDER BY l.added_at, s.id`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServerRef
	for rows.Next() {
		var r ServerRef
		if err := rows.Scan(&r.ID, &r.Addr); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AllListenerAddrs returns the server address of every listener row, so the poller
// can be set up again at startup. An address appears once per listening scope.
func (s *Store) AllListenerAddrs() ([]string, error) {
	return s.strings("SELECT s.addr FROM listeners l JOIN servers s ON s.id = l.server_id ORDER BY l.rowid")
}

// ServerState returns the last stored state and when it was stored. nil if none yet.
func (s *Store) ServerState(serverID int64) (*rules.Snapshot, time.Time, error) {
	return lastState(s.db, serverID)
}

// UserNotifications returns the user's notifications created in scope.
func (s *Store) UserNotifications(userID, scope string) ([]rules.Notification, error) {
	return queryNotifications(s.db, "WHERE user_id = ? AND scope = ?", userID, scope)
}

// CountUserNotifications is the number of notifications the user has anywhere.
func (s *Store) CountUserNotifications(userID string) (n int, err error) {
	err = s.db.QueryRow("SELECT COUNT(*) FROM notifications WHERE user_id = ?", userID).Scan(&n)
	return
}

// DeleteNotification removes one of the user's notifications made in scope. false if there is
// no such notification there.
func (s *Store) DeleteNotification(id int64, userID, scope string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM notification_state WHERE notification_id IN (SELECT id FROM notifications WHERE id = ? AND user_id = ? AND scope = ?)", id, userID, scope); err != nil {
		return false, err
	}
	if _, err := tx.Exec("DELETE FROM conditions WHERE notification_id IN (SELECT id FROM notifications WHERE id = ? AND user_id = ? AND scope = ?)", id, userID, scope); err != nil {
		return false, err
	}
	res, err := tx.Exec("DELETE FROM notifications WHERE id = ? AND user_id = ? AND scope = ?", id, userID, scope)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, tx.Commit()
}

// DeleteAllNotifications removes every notification the user made in scope and returns how many.
func (s *Store) DeleteAllNotifications(userID, scope string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM notification_state WHERE notification_id IN (SELECT id FROM notifications WHERE user_id = ? AND scope = ?)", userID, scope); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("DELETE FROM conditions WHERE notification_id IN (SELECT id FROM notifications WHERE user_id = ? AND scope = ?)", userID, scope); err != nil {
		return 0, err
	}
	res, err := tx.Exec("DELETE FROM notifications WHERE user_id = ? AND scope = ?", userID, scope)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), tx.Commit()
}
