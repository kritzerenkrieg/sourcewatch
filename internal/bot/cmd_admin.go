package bot

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
	"github.com/kritzerenkrieg/sourcewatch/internal/store"
)

// nowFunc is the clock, a variable so tests can fix it.
var nowFunc = time.Now

func durationSec(sec int) time.Duration { return time.Duration(sec) * time.Second }

const disabledMessage = "sourcewatch is turned off in this Discord server. Someone with Manage Server can turn it on again with `/admin settings`."

// guildSettings are the settings of the Discord server the command was run in. Outside a Discord
// server, in a DM with the bot, there are none, so the defaults apply.
func (b *Bot) guildSettings(a actor) store.GuildSettings {
	g := store.GuildSettings{ChannelDelivery: true, DefaultDelivery: rules.DeliveryChannel, Active: true}
	if !a.InGuild {
		return g
	}
	got, err := b.deps.Store.GuildSettings(a.Scope)
	if err != nil {
		b.log.Warn("read guild settings", "err", err)
		return g
	}
	return got
}

// blacklistRefusal says why a player condition cannot be made, when the player is on the blacklist of
// the Discord server. It is empty for anything else.
func (b *Bot) blacklistRefusal(a actor, c rules.Condition) string {
	if !a.InGuild || c.Kind != rules.KindPlayer {
		return ""
	}
	if on, _ := b.deps.Store.IsBlacklisted(a.Scope, c.Text); on {
		return fmt.Sprintf("%s cannot be tracked in this Discord server: an admin put that name on the blacklist.", code(c.Text))
	}
	return ""
}

// runAdmin handles /admin settings, which only people with Manage Server can use, in a Discord server.
func runAdmin(b *Bot, in *discordgo.Interaction, list []*discordgo.ApplicationCommandInteractionDataOption) string {
	a := who(in)
	if !a.InGuild {
		return "`/admin settings` is for Discord servers. Use it in a Discord server."
	}
	if !a.CanManage {
		return "You need the Manage Server permission to change this Discord server's settings."
	}
	if len(list) == 0 || list[0].Name != "settings" {
		return "Use `/admin settings`."
	}
	return b.adminSettings(a, toOpts(list[0].Options))
}

// enabledOption reads an "enabled" or "disabled" option. ok is false if it was not given.
func enabledOption(o opts, name string) (value, ok bool, msg string) {
	switch o.str(name) {
	case "":
		return false, false, ""
	case "enabled":
		return true, true, ""
	case "disabled":
		return false, true, ""
	}
	return false, false, fmt.Sprintf("`%s` is `enabled` or `disabled`.", name)
}

func (b *Bot) adminSettings(a actor, o opts) string {
	var patch store.GuildPatch
	changed := false
	for _, name := range []string{"channel-delivery", "active", "gametracker"} {
		v, given, msg := enabledOption(o, name)
		if msg != "" {
			return msg
		}
		if !given {
			continue
		}
		changed = true
		switch name {
		case "active":
			patch.Active = &v
		case "gametracker":
			patch.GameTracker = &v
		default:
			patch.ChannelDelivery = &v
		}
	}
	switch d := o.str("default-delivery"); d {
	case "":
	case rules.DeliveryChannel, rules.DeliveryDM:
		patch.DefaultDelivery, changed = &d, true
	default:
		return "`default-delivery` is `channel` or `dm`."
	}
	if m, ok := o.int("default-cooldown"); ok {
		sec := int(m) * 60
		patch.DefaultCooldownSec, changed = &sec, true
	}

	var notes []string
	if name := cleanBlacklistName(o.str("blacklist-add")); name != "" {
		changed = true
		added, err := b.deps.Store.AddBlacklist(a.Scope, name, a.UserID, nowFunc())
		if err != nil {
			return b.fail("add to blacklist", err)
		}
		if added {
			notes = append(notes, fmt.Sprintf("%s is on the blacklist: nobody here can track that name, notifications for it stop firing, and `/status` leaves it out.", code(name)))
		} else {
			notes = append(notes, fmt.Sprintf("%s was already on the blacklist.", code(name)))
		}
	}
	if name := cleanBlacklistName(o.str("blacklist-remove")); name != "" {
		changed = true
		removed, err := b.deps.Store.RemoveBlacklist(a.Scope, name)
		if err != nil {
			return b.fail("remove from blacklist", err)
		}
		if removed {
			notes = append(notes, fmt.Sprintf("%s is off the blacklist.", code(name)))
		} else {
			notes = append(notes, fmt.Sprintf("%s was not on the blacklist.", code(name)))
		}
	}
	if changed {
		if _, err := b.deps.Store.UpdateGuildSettings(a.Scope, patch); err != nil {
			return b.fail("save guild settings", err)
		}
	}

	g, err := b.deps.Store.GuildSettings(a.Scope)
	if err != nil {
		return b.fail("read guild settings", err)
	}
	names, _ := b.deps.Store.Blacklist(a.Scope)
	var sb strings.Builder
	if changed {
		sb.WriteString("✅ Settings saved for this Discord server\n")
	} else {
		sb.WriteString("**sourcewatch settings for this Discord server**\n")
	}
	sb.WriteString(adminSummary(g, names))
	for _, n := range notes {
		sb.WriteString("\n" + n)
	}
	if !changed {
		sb.WriteString("\nChange them with `channel-delivery`, `default-delivery`, `default-cooldown`, `active`, `gametracker`, `blacklist-add` and `blacklist-remove`.")
	}
	return sb.String()
}

func cleanBlacklistName(n string) string {
	n = strings.Join(strings.Fields(n), " ")
	if r := []rune(n); len(r) > 64 {
		n = string(r[:64])
	}
	return n
}

// adminSummary lists the settings of a Discord server in words.
func adminSummary(g store.GuildSettings, blacklist []string) string {
	var sb strings.Builder
	if g.Active {
		sb.WriteString("• Active: enabled\n")
	} else {
		sb.WriteString("• Active: **disabled** (sourcewatch does nothing in this Discord server, except `/admin settings`)\n")
	}
	if g.ChannelDelivery {
		sb.WriteString("• Channel delivery: enabled\n")
	} else {
		sb.WriteString("• Channel delivery: **disabled** (notifications go by DM)\n")
	}
	if g.GameTracker {
		sb.WriteString("• GameTracker fallback: enabled\n")
	} else {
		sb.WriteString("• GameTracker fallback: **disabled** (only direct answers from the game servers count here)\n")
	}
	via := "channel"
	if g.DefaultDelivery == rules.DeliveryDM || !g.ChannelDelivery {
		via = "DM"
	}
	fmt.Fprintf(&sb, "• Default delivery of new notifications: via %s\n", via)
	if g.DefaultCooldownSec > 0 {
		fmt.Fprintf(&sb, "• Default cooldown of new notifications: %s\n", minsText(durationSec(g.DefaultCooldownSec)))
	} else {
		fmt.Fprintf(&sb, "• Default cooldown of new notifications: %s (the default)\n", minsText(rules.DefaultCooldown))
	}
	if len(blacklist) == 0 {
		sb.WriteString("• Blacklisted players: none")
		return sb.String()
	}
	shown := blacklist
	more := 0
	if len(shown) > 15 {
		shown, more = shown[:15], len(blacklist)-15
	}
	fmt.Fprintf(&sb, "• Blacklisted players (%d): %s", len(blacklist), strings.Join(quote(shown), ", "))
	if more > 0 {
		fmt.Fprintf(&sb, " … and %d more", more)
	}
	return sb.String()
}

// blacklistChoices suggests blacklisted names that match what was typed.
func (b *Bot) blacklistChoices(a actor, typed string) []string {
	names, _ := b.deps.Store.Blacklist(a.Scope)
	sort.Strings(names)
	var out []string
	for _, n := range names {
		if strings.Contains(strings.ToLower(n), strings.ToLower(strings.TrimSpace(typed))) {
			out = append(out, n)
		}
	}
	return out
}
