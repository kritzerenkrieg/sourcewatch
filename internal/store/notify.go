package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
)

// UserCooldown is the minimum gap between messages to one user (C2).
const UserCooldown = 5 * time.Second

// AddNotification validates and stores n with its conditions. It starts in PhaseNew, so
// creating it never fires it.
func (s *Store) AddNotification(n rules.Notification) (int64, error) {
	if err := n.Validate(); err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// A new notification takes the lowest ID that is free, so the ID of one that was deleted is used
	// again. (SQLite alone would only count up from the highest ID in use.)
	id, err := lowestFreeID(tx)
	if err != nil {
		return 0, err
	}
	// Nothing should be left under a free ID. If something is, it must not attach itself to the new notification.
	for _, q := range []string{"DELETE FROM conditions WHERE notification_id = ?", "DELETE FROM notification_state WHERE notification_id = ?"} {
		if _, err := tx.Exec(q, id); err != nil {
			return 0, err
		}
	}
	// The legacy single-condition columns still exist and are NOT NULL, so they are filled with blanks.
	_, err = tx.Exec(`INSERT INTO notifications(id, user_id, scope, server_id, kind, cooldown_sec, armed, phase, delivery, channel_id)
		VALUES (?,?,?,?,'',?,1,?,?,?)`, id, n.UserID, n.Scope, n.ServerID, n.CooldownSec, int(rules.PhaseNew), n.EffectiveDelivery(), n.ChannelID)
	if err != nil {
		return 0, err
	}
	for _, c := range n.Conditions {
		if err := insertCondition(tx, id, c); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// lowestFreeID is the smallest positive ID no notification has. Writes go through one connection, one
// transaction at a time, so nobody can take it before the insert that follows.
func lowestFreeID(tx execer) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT CASE
		WHEN NOT EXISTS (SELECT 1 FROM notifications WHERE id = 1) THEN 1
		ELSE (SELECT MIN(n.id + 1) FROM notifications n WHERE NOT EXISTS (SELECT 1 FROM notifications m WHERE m.id = n.id + 1))
	END`).Scan(&id)
	return id, err
}

func insertCondition(tx execer, notificationID int64, c rules.Condition) error {
	_, err := tx.Exec("INSERT INTO conditions(notification_id, kind, text, op, n, online) VALUES (?,?,?,?,?,?)",
		notificationID, string(c.Kind), c.Text, c.Op, c.N, b2i(c.Online))
	return err
}

// ErrNoNotification means the notification does not exist or is not the user's.
var ErrNoNotification = errors.New("no such notification")

// AddCondition adds a condition to one of the user's notifications. It starts again in
// PhaseNew, so it only fires when the new set of conditions becomes true. It returns the
// updated notification.
func (s *Store) AddCondition(id int64, userID string, c rules.Condition) (rules.Notification, error) {
	if err := c.Validate(); err != nil {
		return rules.Notification{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return rules.Notification{}, err
	}
	defer tx.Rollback()
	ns, err := queryNotifications(tx, "WHERE id = ? AND user_id = ?", id, userID)
	if err != nil {
		return rules.Notification{}, err
	}
	if len(ns) == 0 {
		return rules.Notification{}, ErrNoNotification
	}
	n := ns[0]
	n.Conditions = append(n.Conditions, c)
	if err := n.Validate(); err != nil {
		return rules.Notification{}, err
	}
	if err := insertCondition(tx, id, c); err != nil {
		return rules.Notification{}, err
	}
	if _, err := tx.Exec("UPDATE notifications SET phase = ? WHERE id = ?", int(rules.PhaseNew), id); err != nil {
		return rules.Notification{}, err
	}
	if err := resetState(tx, id); err != nil {
		return rules.Notification{}, err
	}
	n.Phase = rules.PhaseNew
	return n, tx.Commit()
}

const notificationCols = `id, user_id, scope, server_id, cooldown_sec, phase, last_fired, delivery, channel_id, undeliverable, disabled`

// notifications returns the notifications to check for a server: those made for it, and those made
// for all servers by a place that watches it. Disabled ones, ones stopped because their DM was
// refused, and ones from a Discord server where sourcewatch is turned off are left out.
func notifications(tx execer, serverID int64) ([]rules.Notification, error) {
	return queryNotifications(tx, `WHERE undeliverable = 0 AND disabled = 0
		AND (server_id = ? OR (server_id = 0 AND scope IN (SELECT scope FROM listeners WHERE server_id = ?)))
		AND scope NOT IN (SELECT scope FROM guild_settings WHERE active = 0)`, serverID, serverID)
}

// queryNotifications returns the notifications matching where (columns of notifications
// only), each with its conditions.
func queryNotifications(tx execer, where string, args ...any) ([]rules.Notification, error) {
	rows, err := tx.Query("SELECT "+notificationCols+" FROM notifications "+where+" ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	var out []rules.Notification
	index := map[int64]int{}
	for rows.Next() {
		var (
			n             rules.Notification
			phase         int
			undeliverable int
			disabled      int
			last          int64
		)
		if err := rows.Scan(&n.ID, &n.UserID, &n.Scope, &n.ServerID, &n.CooldownSec, &phase, &last,
			&n.Delivery, &n.ChannelID, &undeliverable, &disabled); err != nil {
			rows.Close()
			return nil, err
		}
		n.Phase, n.Undeliverable, n.Disabled = rules.Phase(phase), undeliverable == 1, disabled == 1
		if last != 0 {
			n.LastFired = time.UnixMilli(last)
		}
		index[n.ID] = len(out)
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(out) == 0 {
		return out, nil
	}

	crows, err := tx.Query(`SELECT c.notification_id, c.kind, c.text, c.op, c.n, c.online
		FROM conditions c WHERE c.notification_id IN (SELECT id FROM notifications `+where+`) ORDER BY c.id`, args...)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var (
			id     int64
			kind   string
			c      rules.Condition
			online int
		)
		if err := crows.Scan(&id, &kind, &c.Text, &c.Op, &c.N, &online); err != nil {
			return nil, err
		}
		c.Kind, c.Online = rules.Kind(kind), online == 1
		if i, ok := index[id]; ok {
			out[i].Conditions = append(out[i].Conditions, c)
		}
	}
	return out, crows.Err()
}

// Process is one poll result for one server. In a single transaction it stores the
// new state and history, evaluates the notifications that apply to the server against it, and
// saves each notification's phase. It returns the events to deliver. An event for a user who was
// messaged less than UserCooldown ago (C2) is held back: its notification is left as it was, so it
// fires at the next poll if its conditions still hold (C9).
//
// A notification for all servers is checked once for each server it covers, with its own phase and
// cooldown for that server. The settings of a Discord server apply: where channel delivery is off
// the event goes by DM, and a notification that tracks a blacklisted player is not checked.
func (s *Store) Process(serverID int64, cur rules.Snapshot, now time.Time) ([]rules.Event, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	_, cur, err = apply(tx, serverID, cur, now)
	if err != nil {
		return nil, err
	}
	ns, err := notifications(tx, serverID)
	if err != nil {
		return nil, err
	}
	settings := map[string]GuildSettings{}
	blacklists := map[string]map[string]bool{}
	var events []rules.Event
	for _, n := range ns {
		if !settingsLoaded(settings, n.Scope) {
			if settings[n.Scope], err = guildSettings(tx, n.Scope); err != nil {
				return nil, err
			}
			if blacklists[n.Scope], err = blacklistSet(tx, n.Scope); err != nil {
				return nil, err
			}
		}
		if tracksBlacklisted(n, blacklists[n.Scope]) {
			continue
		}
		if cur.Source == "gametracker" && !settings[n.Scope].GameTracker {
			continue // this Discord server only counts direct answers (E22): nothing changes for it
		}
		forAll := n.ServerID == rules.AllServers
		if forAll { // its phase and last firing for this server
			if n.Phase, n.LastFired, err = loadState(tx, n.ID, serverID); err != nil {
				return nil, err
			}
		}
		upd, ev := rules.Evaluate(cur, n, now)
		if ev != nil {
			ok, err := allowUser(tx, ev.UserID, now)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue // held back: nothing is saved, so it is still armed at the next poll (C9)
			}
		}
		if upd.Phase != n.Phase || !upd.LastFired.Equal(n.LastFired) {
			if forAll {
				err = saveState(tx, n.ID, serverID, upd)
			} else {
				_, err = tx.Exec("UPDATE notifications SET phase = ?, last_fired = ? WHERE id = ?", int(upd.Phase), ms(upd.LastFired), n.ID)
			}
			if err != nil {
				return nil, err
			}
		}
		if ev == nil {
			continue
		}
		ev.ServerID = serverID
		if ev.Delivery == rules.DeliveryChannel && !settings[n.Scope].ChannelDelivery && strings.HasPrefix(n.Scope, "guild:") {
			ev.Delivery = rules.DeliveryDM // an admin turned channel delivery off here
		}
		events = append(events, *ev)
	}
	return events, tx.Commit()
}

func settingsLoaded(m map[string]GuildSettings, scope string) bool { _, ok := m[scope]; return ok }

// tracksBlacklisted reports whether the notification has a player condition for a name on the blacklist.
func tracksBlacklisted(n rules.Notification, blacklist map[string]bool) bool {
	if len(blacklist) == 0 {
		return false
	}
	for _, c := range n.Conditions {
		if c.Kind == rules.KindPlayer && blacklist[blacklistKey(c.Text)] {
			return true
		}
	}
	return false
}

func loadState(tx execer, notificationID, serverID int64) (rules.Phase, time.Time, error) {
	var phase int
	var last int64
	err := tx.QueryRow("SELECT phase, last_fired FROM notification_state WHERE notification_id = ? AND server_id = ?", notificationID, serverID).Scan(&phase, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return rules.PhaseNew, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, err
	}
	var at time.Time
	if last != 0 {
		at = time.UnixMilli(last)
	}
	return rules.Phase(phase), at, nil
}

func saveState(tx execer, notificationID, serverID int64, n rules.Notification) error {
	_, err := tx.Exec(`INSERT INTO notification_state(notification_id, server_id, phase, last_fired) VALUES (?,?,?,?)
		ON CONFLICT(notification_id, server_id) DO UPDATE SET phase = excluded.phase, last_fired = excluded.last_fired`,
		notificationID, serverID, int(n.Phase), ms(n.LastFired))
	return err
}

// resetState makes a notification for all servers start over on every server.
func resetState(tx execer, notificationID int64) error {
	_, err := tx.Exec("DELETE FROM notification_state WHERE notification_id = ?", notificationID)
	return err
}

// startOver makes the notifications matching where (columns of notifications only) start over, on
// every server: they wait for their conditions to become true again (E14, E19).
func startOver(tx execer, where string, args ...any) error {
	if _, err := tx.Exec("DELETE FROM notification_state WHERE notification_id IN (SELECT id FROM notifications "+where+")", args...); err != nil {
		return err
	}
	_, err := tx.Exec("UPDATE notifications SET phase = ? "+where, append([]any{int(rules.PhaseNew)}, args...)...)
	return err
}

// allowUser reports whether a message to user may go out now, and if so records it.
func allowUser(tx execer, user string, now time.Time) (bool, error) {
	var last int64
	err := tx.QueryRow("SELECT last_sent FROM user_cooldowns WHERE user_id = ?", user).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil && now.Sub(time.UnixMilli(last)) < UserCooldown {
		return false, nil
	}
	_, err = tx.Exec(`INSERT INTO user_cooldowns(user_id, last_sent) VALUES (?,?)
		ON CONFLICT(user_id) DO UPDATE SET last_sent = excluded.last_sent`, user, ms(now))
	return err == nil, err
}

// MarkUndeliverable stops delivery of a notification that Discord refused to deliver, by DM (E7) or
// in its channel (D4).
// Its events are skipped until TakeUndeliverable is called for its user.
func (s *Store) MarkUndeliverable(id int64) error {
	_, err := s.db.Exec("UPDATE notifications SET undeliverable = 1 WHERE id = ?", id)
	return err
}

// TakeUndeliverable returns the user's undeliverable notifications so the bot can
// tell them (E7), and re-enables delivery for them. If the DMs are still refused,
// the next failed delivery marks them again. They start over, so a condition that
// became true while they were stopped does not fire late (E19).
func (s *Store) TakeUndeliverable(userID string) ([]rules.Notification, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ns, err := queryNotifications(tx, "WHERE user_id = ? AND undeliverable = 1", userID)
	if err != nil {
		return nil, err
	}
	if err := startOver(tx, "WHERE user_id = ? AND undeliverable = 1", userID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("UPDATE notifications SET undeliverable = 0 WHERE user_id = ? AND undeliverable = 1", userID); err != nil {
		return nil, err
	}
	return ns, tx.Commit()
}

// Settings are the parts of a notification that can be changed after it is made. A nil field
// is left as it is.
type Settings struct {
	CooldownSec *int
	Disabled    *bool
	Delivery    *string
	ChannelID   *string
}

// applySettings changes n in memory. Turning a disabled notification back on starts it over,
// so it does not fire for conditions that already hold. Changing the delivery lifts a stop
// caused by a refused DM, because the new place has not been tried yet.
func applySettings(n *rules.Notification, set Settings) (restarted bool) {
	if set.CooldownSec != nil {
		n.CooldownSec = *set.CooldownSec
	}
	if set.Delivery != nil {
		n.Delivery, n.Undeliverable = *set.Delivery, false
	}
	if set.ChannelID != nil {
		n.ChannelID = *set.ChannelID
	}
	if set.Disabled != nil {
		if n.Disabled && !*set.Disabled {
			n.Phase, restarted = rules.PhaseNew, true
		}
		n.Disabled = *set.Disabled
	}
	return restarted
}

func saveSettings(tx execer, n rules.Notification) error {
	_, err := tx.Exec(`UPDATE notifications SET cooldown_sec = ?, delivery = ?, channel_id = ?, disabled = ?, undeliverable = ?, phase = ? WHERE id = ?`,
		n.CooldownSec, n.EffectiveDelivery(), n.ChannelID, b2i(n.Disabled), b2i(n.Undeliverable), int(n.Phase), n.ID)
	return err
}

// UpdateSettings changes the settings of one of the user's notifications in scope and returns it.
func (s *Store) UpdateSettings(id int64, userID, scope string, set Settings) (rules.Notification, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return rules.Notification{}, err
	}
	defer tx.Rollback()
	ns, err := queryNotifications(tx, "WHERE id = ? AND user_id = ? AND scope = ?", id, userID, scope)
	if err != nil {
		return rules.Notification{}, err
	}
	if len(ns) == 0 {
		return rules.Notification{}, ErrNoNotification
	}
	n := ns[0]
	restarted := applySettings(&n, set)
	if err := n.Validate(); err != nil {
		return rules.Notification{}, err
	}
	if err := saveSettings(tx, n); err != nil {
		return rules.Notification{}, err
	}
	if restarted {
		if err := resetState(tx, n.ID); err != nil {
			return rules.Notification{}, err
		}
	}
	return n, tx.Commit()
}

// UpdateAllSettings changes the settings of every notification the user made in scope. If any
// of them would end up invalid, none is changed. It returns the notifications as they are now.
func (s *Store) UpdateAllSettings(userID, scope string, set Settings) ([]rules.Notification, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ns, err := queryNotifications(tx, "WHERE user_id = ? AND scope = ?", userID, scope)
	if err != nil {
		return nil, err
	}
	for i := range ns {
		restarted := applySettings(&ns[i], set)
		if err := ns[i].Validate(); err != nil {
			return nil, err
		}
		if err := saveSettings(tx, ns[i]); err != nil {
			return nil, err
		}
		if restarted {
			if err := resetState(tx, ns[i].ID); err != nil {
				return nil, err
			}
		}
	}
	return ns, tx.Commit()
}
