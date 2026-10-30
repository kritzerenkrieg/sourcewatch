// Package config loads settings from environment variables or a 600 JSON file (A2).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

type Config struct {
	DiscordToken string `json:"discord_token"`
	WebhookURL   string `json:"webhook_url"`
	// ApplicationID is the Discord application ID. It is not a secret.
	ApplicationID string `json:"application_id"`
	// GuildID, when set, is a Discord server whose old server-only copy of the commands is
	// removed at startup. The commands are always registered globally.
	GuildID string `json:"guild_id"`
	// Database is the SQLite file. Defaults to sourcewatch.db in the working directory.
	Database string `json:"database"`
	// DisableGameTracker turns off the GameTracker fallback (B5).
	DisableGameTracker bool `json:"disable_gametracker"`
	// Servers are A2S addresses, "host:port" (B1).
	Servers []string `json:"servers"`
}

// Load reads the file named by SOURCEWATCH_CONFIG (which must have mode 0600), then
// lets SOURCEWATCH_DISCORD_TOKEN, SOURCEWATCH_WEBHOOK_URL and SOURCEWATCH_SERVERS (comma-separated)
// override its values.
func Load() (Config, error) {
	var c Config
	if path := env("CONFIG"); path != "" {
		fi, err := os.Stat(path)
		if err != nil {
			return c, err
		}
		if fi.Mode().Perm() != 0o600 {
			return c, fmt.Errorf("config %s has mode %o, want 600", path, fi.Mode().Perm())
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return c, err
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if v := env("DISCORD_TOKEN"); v != "" {
		c.DiscordToken = v
	}
	if v := env("WEBHOOK_URL"); v != "" {
		c.WebhookURL = v
	}
	if v := env("SERVERS"); v != "" {
		c.Servers = strings.Split(v, ",")
	}
	if v := env("DATABASE"); v != "" {
		c.Database = v
	}
	c.Database = chooseDatabase(c.Database)
	c.Servers = cleanServers(c.Servers)
	if len(c.Servers) == 0 && c.DiscordToken == "" {
		return c, errors.New("no servers configured and no discord token")
	}
	return c, nil
}

// cleanServers trims each address and drops empty ones, so "a:27015, b:27015," is two servers.
// The poller dials an address exactly as it is written.
func cleanServers(list []string) []string {
	var out []string
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// LogValue redacts secrets so a Config can be logged safely.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("discord_token_set", c.DiscordToken != ""),
		slog.Bool("webhook_url_set", c.WebhookURL != ""),
		slog.Int("servers", len(c.Servers)),
	)
}

const (
	defaultDatabase = "sourcewatch.db"
	// legacyDatabase is what the file was called before the project was renamed.
	legacyDatabase = "pawnz.db"
)

// env reads a setting from the environment: SOURCEWATCH_<name>, or PAWNZ_<name>, the name used
// before the project was renamed, so an install that still sets those keeps working.
func env(name string) string {
	if v := os.Getenv("SOURCEWATCH_" + name); v != "" {
		return v
	}
	return os.Getenv("PAWNZ_" + name)
}

// chooseDatabase picks the database file when none is configured: sourcewatch.db, unless there is
// only an old pawnz.db in the working directory, which is then kept in use so a renamed install
// does not start over with an empty database.
func chooseDatabase(configured string) string {
	if configured != "" {
		return configured
	}
	if _, err := os.Stat(defaultDatabase); err == nil {
		return defaultDatabase
	}
	if _, err := os.Stat(legacyDatabase); err == nil {
		return legacyDatabase
	}
	return defaultDatabase
}
