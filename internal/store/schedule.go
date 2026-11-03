package store

import (
	"database/sql"
	"errors"

	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
)

// scheduleKey says whose schedule it is: an owner's notification in a place. Num 0 is the schedule
// for all of the owner's notifications there (E24).
type scheduleKey struct {
	userID, scope string
	num           int64
}

// attachSchedules gives each notification its own schedule, or else the one for all of its owner's
// notifications in that place. The table is small, so it is read in one go.
func attachSchedules(tx execer, ns []rules.Notification) error {
	rows, err := tx.Query("SELECT user_id, scope, num, start_min, end_min, days FROM schedules")
	if err != nil {
		return err
	}
	defer rows.Close()
	all := map[scheduleKey]rules.Schedule{}
	for rows.Next() {
		var (
			k    scheduleKey
			s    rules.Schedule
			days string
		)
		if err := rows.Scan(&k.userID, &k.scope, &k.num, &s.StartMin, &s.EndMin, &days); err != nil {
			return err
		}
		for i := 0; i < 7 && i < len(days); i++ {
			s.Days[i] = days[i] == '1'
		}
		all[k] = s
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range ns {
		n := &ns[i]
		s, ok := all[scheduleKey{n.UserID, n.Scope, n.Num}]
		if !ok {
			s, ok = all[scheduleKey{n.UserID, n.Scope, 0}]
		}
		if ok {
			sc := s
			n.Schedule = &sc
		}
	}
	return nil
}

// SetSchedule sets the schedule of one notification (num), or of all of the owner's notifications in
// the place (num 0). A notification's own schedule overrides the one for all.
func (s *Store) SetSchedule(userID, scope string, num int64, sc rules.Schedule) error {
	_, err := s.db.Exec(`INSERT INTO schedules(user_id, scope, num, start_min, end_min, days) VALUES (?,?,?,?,?,?)
		ON CONFLICT(user_id, scope, num) DO UPDATE SET start_min = excluded.start_min, end_min = excluded.end_min, days = excluded.days`,
		userID, scope, num, sc.StartMin, sc.EndMin, sc.Mask())
	return err
}

// ClearSchedule removes the schedule set for num (0 for all). It reports whether there was one.
func (s *Store) ClearSchedule(userID, scope string, num int64) (bool, error) {
	res, err := s.db.Exec("DELETE FROM schedules WHERE user_id = ? AND scope = ? AND num = ?", userID, scope, num)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Schedule returns the schedule set for exactly num (0 for all), or nil if none.
func (s *Store) Schedule(userID, scope string, num int64) (*rules.Schedule, error) {
	var (
		sc   rules.Schedule
		days string
	)
	err := s.db.QueryRow("SELECT start_min, end_min, days FROM schedules WHERE user_id = ? AND scope = ? AND num = ?",
		userID, scope, num).Scan(&sc.StartMin, &sc.EndMin, &days)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for i := 0; i < 7 && i < len(days); i++ {
		sc.Days[i] = days[i] == '1'
	}
	return &sc, nil
}
