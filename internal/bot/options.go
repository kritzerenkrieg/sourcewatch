package bot

import (
	"github.com/bwmarrin/discordgo"
	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
)

func choices(values ...string) []*discordgo.ApplicationCommandOptionChoice {
	out := make([]*discordgo.ApplicationCommandOptionChoice, len(values))
	for i, v := range values {
		out[i] = &discordgo.ApplicationCommandOptionChoice{Name: v, Value: v}
	}
	return out
}

// labeled makes a choice whose label is what people read and whose value is what the bot gets.
func labeled(pairs ...[2]string) []*discordgo.ApplicationCommandOptionChoice {
	out := make([]*discordgo.ApplicationCommandOptionChoice, len(pairs))
	for i, p := range pairs {
		out[i] = &discordgo.ApplicationCommandOptionChoice{Name: p[0], Value: p[1]}
	}
	return out
}

func f64(v float64) *float64 { return &v }

// serverOptions are the subcommands of /server: add and remove, each with the game server's address.
func serverOptions() []*discordgo.ApplicationCommandOption {
	return []*discordgo.ApplicationCommandOption{
		{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "add", Description: "Start watching a game server",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "address", Description: "Server address, like 1.2.3.4:27015 or play.example.com", Required: true},
			}},
		{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "remove", Description: "Stop watching a game server",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "address", Description: "The game server to stop watching", Required: true, Autocomplete: true},
			}},
	}
}

// notifyOptions are the subcommands of /notify: one per condition type, then list and remove. With
// role set they are the ones of /notify-role (E25): the same, and the type commands also take a role.
func notifyOptions(role bool) []*discordgo.ApplicationCommandOption {
	sub := func(name, desc string, opts ...*discordgo.ApplicationCommandOption) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionSubCommand, Name: name, Description: desc, Options: opts}
	}
	return []*discordgo.ApplicationCommandOption{
		sub("map", "When the server is on a map (several maps in one notification: any of them, OR)", createOptions(role, rules.KindMap)...),
		sub("player", "When a player is on the server (several players in one notification: all of them, AND)", createOptions(role, rules.KindPlayer)...),
		sub("playercount", "When the number of players reaches a number", createOptions(role, rules.KindPlayerCount)...),
		sub("status", "When the server goes online or offline", createOptions(role, rules.KindStatus)...),
		sub("title-query", "When the server name contains a word", createOptions(role, rules.KindTitle)...),
		sub("list", "Show your notifications"),
		sub("show", "Share your notifications with everyone in this channel"),
		sub("remove", "Delete one of your notifications",
			&discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionInteger, Name: "id", Description: "Which notification to delete", Required: true, Autocomplete: true}),
		sub("settings", "Change or see how one notification behaves", settingsOptions(role)...),
		sub("schedule", "Only let notifications fire at certain times (UTC)",
			&discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString, Name: "time", Description: "HH:MM-HH:MM in UTC, like 18:00-23:30. off removes the schedule", Required: true},
			&discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString, Name: "day", Description: "Mon to Sun as 7 digits, 1 = on: 0000011 is Sat and Sun (default 1111111)", Autocomplete: true},
			&discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionInteger, Name: "id", Description: "One notification's ID. Leave out for all of them (its own schedule wins)", MinValue: f64(1), Autocomplete: true}),
		sub("reset", "Delete all your notifications here (asks first)"),
	}
}

// settingsOptions are the options of the settings subcommand. They are the same for /notify and
// /notify-role; for /notify-role, without an id they are the settings of the whole Discord server (E26).
func settingsOptions(role bool) []*discordgo.ApplicationCommandOption {
	idText := "A notification's ID number, or all (the default) for every notification you have here"
	if role {
		idText = "A notification's ID number. Leave out for the settings of the whole Discord server"
	}
	out := []*discordgo.ApplicationCommandOption{
		{Type: discordgo.ApplicationCommandOptionString, Name: "id", Description: idText, Autocomplete: true},
		{Type: discordgo.ApplicationCommandOptionInteger, Name: "cooldown", Description: "Minutes to stay quiet after it fires", MinValue: f64(1), MaxValue: 10080},
		{Type: discordgo.ApplicationCommandOptionString, Name: "active", Description: "Turn the notification on or off",
			Choices: labeled([2]string{"Enabled", "enabled"}, [2]string{"Disabled", "disabled"})},
		{Type: discordgo.ApplicationCommandOptionString, Name: "delivery", Description: "Where it arrives: the channel you run this in, or your DMs",
			Choices: labeled([2]string{"In this channel", "channel"}, [2]string{"By DM", "dm"})},
	}
	return out
}

// valueOption is the name of the option that holds what a notification of this type is about.
func valueOption(kind rules.Kind) string {
	switch kind {
	case rules.KindMap:
		return "map-name"
	case rules.KindPlayer:
		return "username"
	}
	return "value"
}

// createOptions are the options of the subcommand that makes a notification of this type: what
// it is about (required), and the id to add it to, and the game server. Where it is delivered and
// the cooldown are not here: a new notification uses the defaults, and /notify settings changes them.
func createOptions(role bool, kind rules.Kind) []*discordgo.ApplicationCommandOption {
	str := discordgo.ApplicationCommandOptionString
	var value *discordgo.ApplicationCommandOption
	switch kind {
	case rules.KindMap:
		value = &discordgo.ApplicationCommandOption{Type: str, Name: valueOption(kind), Description: "Map name, like de_dust2. Several maps in one notification (id) mean any of them (OR)", Required: true, Autocomplete: true}
	case rules.KindPlayer:
		value = &discordgo.ApplicationCommandOption{Type: str, Name: valueOption(kind), Description: "Player name. Several players in one notification (id) must all be online (AND)", Required: true, Autocomplete: true}
	case rules.KindPlayerCount:
		value = &discordgo.ApplicationCommandOption{Type: str, Name: valueOption(kind), Description: "Players (0-64), with a comparison: >=10, <5, =20, !=3. A plain number means at least", Required: true, Autocomplete: true}
	case rules.KindStatus:
		value = &discordgo.ApplicationCommandOption{Type: str, Name: valueOption(kind), Description: "online or offline", Required: true,
			Choices: labeled([2]string{"online", "online"}, [2]string{"offline", "offline"})}
	default:
		value = &discordgo.ApplicationCommandOption{Type: str, Name: valueOption(kind), Description: "Word or phrase in the server name", Required: true}
	}
	out := []*discordgo.ApplicationCommandOption{value}
	if role {
		out = append(out, &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "The role to ping", Required: true})
	}
	return append(out,
		&discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionInteger, Name: "id", Description: "Add this to one of your notifications (its ID). An ID you do not have makes a new one", MinValue: f64(1), Autocomplete: true},
		&discordgo.ApplicationCommandOption{Type: str, Name: "server", Description: "Which game server (leave out for all of them)", Autocomplete: true})
}

// adminOptions are the subcommands of /admin: for now, settings, whose options are all optional.
func adminOptions() []*discordgo.ApplicationCommandOption {
	str := discordgo.ApplicationCommandOptionString
	enabled := labeled([2]string{"enabled", "enabled"}, [2]string{"disabled", "disabled"})
	return []*discordgo.ApplicationCommandOption{
		{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "settings", Description: "See or change the settings of this Discord server",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: str, Name: "channel-delivery", Description: "Allow notifications to be posted in channels (enabled by default)", Choices: enabled},
				{Type: str, Name: "default-delivery", Description: "How new notifications are delivered (default: channel)",
					Choices: labeled([2]string{"channel", "channel"}, [2]string{"dm", "dm"})},
				{Type: discordgo.ApplicationCommandOptionInteger, Name: "default-cooldown", Description: "Cooldown of new notifications, in minutes (default: 60)", MinValue: f64(1), MaxValue: 10080},
				{Type: str, Name: "active", Description: "Turn sourcewatch on or off in this whole Discord server (enabled by default)", Choices: enabled},
				{Type: str, Name: "blacklist-add", Description: "A player name that nobody here can track"},
				{Type: str, Name: "blacklist-remove", Description: "Take a player name off the blacklist", Autocomplete: true},
			}},
	}
}
