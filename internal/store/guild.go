package store

import (
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

// GuildSettings are what an admin of a Discord server sets with /admin settings. They apply to
// the notifications made in that Discord server.
type GuildSettings struct {
	// ChannelDelivery is false when notifications may not be posted in channels: they go by DM.
	ChannelDelivery bool
	// DefaultDelivery is how a new notification is delivered: "channel" or "dm".
	DefaultDelivery string
	// DefaultCooldownSec is the cooldown of a new notification. 0 means the usual default.
	DefaultCooldownSec int
	// Active is false when sourcewatch is turned off in the whole Discord server.
	Active bool
	// GameTracker is false when the GameTracker fallback may not be used for the game servers this
	// Discord server watches: only direct queries count here (E22).
	GameTracker bool
}

func defaultGuildSettings() GuildSettings {
	return GuildSettings{ChannelDelivery: true, DefaultDelivery: "channel", Active: true, GameTracker: true}
}

func guildSettings(q execer, scope string) (GuildSettings, error) {
	g := defaultGuildSettings()
	var ch, active, gt int
	err := q.QueryRow(`SELECT channel_delivery, default_delivery, default_cooldown_sec, active, gametracker FROM guild_settings WHERE scope = ?`, scope).
		Scan(&ch, &g.DefaultDelivery, &g.DefaultCooldownSec, &active, &gt)
	if errors.Is(err, sql.ErrNoRows) {
		return g, nil
	}
	if err != nil {
		return g, err
	}
	g.ChannelDelivery, g.Active, g.GameTracker = ch == 1, active == 1, gt == 1
	return g, nil
}

// GuildSettings returns the settings of a Discord server, with the defaults for what was never set.
func (s *Store) GuildSettings(scope string) (GuildSettings, error) { return guildSettings(s.db, scope) }

// GuildPatch changes some guild settings. A nil field is left as it is.
type GuildPatch struct {
	ChannelDelivery    *bool
	DefaultDelivery    *string
	DefaultCooldownSec *int
	Active             *bool
	GameTracker        *bool
}

// UpdateGuildSettings applies the patch and returns the settings as they are now.
func (s *Store) UpdateGuildSettings(scope string, p GuildPatch) (GuildSettings, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return GuildSettings{}, err
	}
	defer tx.Rollback()
	g, err := guildSettings(tx, scope)
	if err != nil {
		return g, err
	}
	if p.ChannelDelivery != nil {
		g.ChannelDelivery = *p.ChannelDelivery
	}
	if p.DefaultDelivery != nil {
		if *p.DefaultDelivery != "channel" && *p.DefaultDelivery != "dm" {
			return g, errors.New("default delivery is channel or dm")
		}
		g.DefaultDelivery = *p.DefaultDelivery
	}
	if p.DefaultCooldownSec != nil {
		if *p.DefaultCooldownSec < 0 {
			return g, errors.New("cooldown must not be negative")
		}
		g.DefaultCooldownSec = *p.DefaultCooldownSec
	}
	if p.Active != nil {
		// Turned back on, the notifications made here start over, so a condition that became
		// true while sourcewatch was off does not fire late (E19).
		if *p.Active && !g.Active {
			if err := startOver(tx, "WHERE scope = ?", scope); err != nil {
				return g, err
			}
		}
		g.Active = *p.Active
	}
	if p.GameTracker != nil {
		g.GameTracker = *p.GameTracker
	}
	_, err = tx.Exec(`INSERT INTO guild_settings(scope, channel_delivery, default_delivery, default_cooldown_sec, active, gametracker) VALUES (?,?,?,?,?,?)
		ON CONFLICT(scope) DO UPDATE SET channel_delivery=excluded.channel_delivery, default_delivery=excluded.default_delivery,
			default_cooldown_sec=excluded.default_cooldown_sec, active=excluded.active, gametracker=excluded.gametracker`,
		scope, b2i(g.ChannelDelivery), g.DefaultDelivery, g.DefaultCooldownSec, b2i(g.Active), b2i(g.GameTracker))
	if err != nil {
		return g, err
	}
	return g, tx.Commit()
}

func blacklistKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// blacklistSet returns the lower-case names that nobody in the Discord server can track.
func blacklistSet(q execer, scope string) (map[string]bool, error) {
	rows, err := q.Query("SELECT name_key FROM guild_blacklist WHERE scope = ?", scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	set := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		set[k] = true
	}
	return set, rows.Err()
}

// Blacklist returns the names on a Discord server's blacklist, sorted, as they were written.
func (s *Store) Blacklist(scope string) ([]string, error) {
	rows, err := s.db.Query("SELECT name FROM guild_blacklist WHERE scope = ?", scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out, rows.Err()
}

// IsBlacklisted reports whether the name (not case-sensitive) is on the Discord server's blacklist.
func (s *Store) IsBlacklisted(scope, name string) (bool, error) {
	set, err := blacklistSet(s.db, scope)
	return set[blacklistKey(name)], err
}

// AddBlacklist adds a name. added is false if it was already there.
func (s *Store) AddBlacklist(scope, name, by string, now time.Time) (added bool, err error) {
	name = strings.TrimSpace(name)
	res, err := s.db.Exec("INSERT OR IGNORE INTO guild_blacklist(scope, name_key, name, added_by, added_at) VALUES (?,?,?,?,?)",
		scope, blacklistKey(name), name, by, ms(now))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RemoveBlacklist removes a name. removed is false if it was not there.
func (s *Store) RemoveBlacklist(scope, name string) (removed bool, err error) {
	res, err := s.db.Exec("DELETE FROM guild_blacklist WHERE scope = ? AND name_key = ?", scope, blacklistKey(name))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// GameTrackerAllowed reports whether the GameTracker fallback may be used for a game server: unless
// every place that watches it has turned the fallback off. A server nobody watches, one that is only
// in the config, may use it.
func (s *Store) GameTrackerAllowed(addr string) (bool, error) {
	var total, allowing int
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(COALESCE(g.gametracker, 1)), 0)
		FROM listeners l JOIN servers v ON v.id = l.server_id LEFT JOIN guild_settings g ON g.scope = l.scope
		WHERE v.addr = ?`, addr).Scan(&total, &allowing)
	return total == 0 || allowing > 0, err
}
