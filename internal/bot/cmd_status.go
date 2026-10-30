package bot

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
	"github.com/kritzerenkrieg/sourcewatch/internal/store"
)

// runStatus is /status: every game server watched here, with who is online. It has no options and
// the reply is public, so everyone in the channel sees it. Each server gets a share of the reply,
// because Discord limits it to 2000 characters, and its player list is cut to fit.
func runStatus(b *Bot, in *discordgo.Interaction, _ []*discordgo.ApplicationCommandInteractionDataOption) string {
	a := who(in)
	servers, err := b.deps.Store.ListenerServers(a.Scope)
	if err != nil {
		return b.fail("list servers", err)
	}
	if len(servers) == 0 {
		return "No game server is being watched here yet. Add one with `/server add`."
	}
	budget := replyBudget / len(servers)
	if budget < minServerBudget {
		budget = minServerBudget
	}
	now := time.Now()
	hidden := map[string]bool{} // players an admin of this Discord server put on the blacklist
	if a.InGuild {
		names, _ := b.deps.Store.Blacklist(a.Scope)
		for _, n := range names {
			hidden[strings.ToLower(n)] = true
		}
	}
	direct := !a.InGuild || b.guildSettings(a).GameTracker // false: this Discord server turned GameTracker off (E22)
	var sb strings.Builder
	for _, srv := range servers {
		st, at, err := b.deps.Store.ServerState(srv.ID)
		if err != nil {
			return b.fail("read state", err)
		}
		if st != nil && st.Source == "gametracker" && !direct {
			// The data came from GameTracker, which this Discord server does not use.
			sb.WriteString(fmt.Sprintf("%s: **no direct answer**, and GameTracker is turned off in this Discord server.\n", code(srv.Addr)))
			continue
		}
		if st != nil && len(hidden) > 0 {
			view := *st
			view.PlayerNames = nil
			for _, n := range st.PlayerNames {
				if !hidden[strings.ToLower(strings.TrimSpace(n))] {
					view.PlayerNames = append(view.PlayerNames, n)
				}
			}
			if len(view.PlayerNames) == 0 && len(st.PlayerNames) > 0 {
				view.PlayerNames = []string{"(hidden)"} // every name here is blacklisted
			}
			st = &view
		}
		sb.WriteString(fitServer(func(maxNames int) string { return statusBlock(srv, st, at, now, maxNames) }, budget, maxNamesInView))
	}
	// A place can watch only a few servers, so the shares add up to less than the limit. This is
	// the net under that: a reply Discord would refuse is cut instead.
	if out := sb.String(); utf8.RuneCountInString(out) > 2000 {
		return string([]rune(out)[:1999]) + "…"
	}
	return sb.String()
}

const (
	replyBudget     = 1900 // under Discord's 2000 character limit
	minServerBudget = 150
	maxNamesInView  = 100
)

// fitServer renders a server's block with as many player names as fit in budget, trying
// at most maxNames. A block with no list that is still too long is cut.
func fitServer(render func(maxNames int) string, budget, maxNames int) string {
	for n := maxNames; n > 0; n-- {
		if out := render(n); utf8.RuneCountInString(out) <= budget {
			return out
		}
	}
	out := render(0)
	if r := []rune(out); len(r) > budget {
		return string(r[:budget-1]) + "…\n"
	}
	return out
}

// statusBlock is the status of one server, followed by who is on it.
func statusBlock(s store.ServerRef, st *rules.Snapshot, at, now time.Time, maxNames int) string {
	if st == nil {
		return fmt.Sprintf("%s: no data yet, the first poll is still pending.\n", code(s.Addr))
	}
	updated := "Updated " + ago(now.Sub(at))
	if st.Source == "gametracker" {
		// Not our own query: say where it came from and how old it is.
		updated = "Via GameTracker, scanned " + ago(now.Sub(st.DataAt))
	}
	switch {
	case st.Online:
		return fmt.Sprintf("%s: **online**, %d/%d players, map %s. %s.\n",
			label(st.Title, s.Addr), st.Players, st.MaxPlayers, code(st.Map), updated) + playerList(st, maxNames)
	case st.Title == "" && st.Map == "":
		// Never answered, so there is no name or map to show.
		return fmt.Sprintf("%s: **offline**, no answer yet. %s.\n", code(s.Addr), updated)
	}
	return fmt.Sprintf("%s: **offline**. Last known map %s. %s.\n", label(st.Title, s.Addr), code(st.Map), updated)
}

// playerList shows the names of the players on a server in a code block. Names come from
// other people, so they stay inside it: nothing in a code block is linked, expanded or
// formatted, and a backtick or line break in a name cannot end the block early.
func playerList(st *rules.Snapshot, maxNames int) string {
	if st.Players == 0 {
		return ""
	}
	if len(st.PlayerNames) == 0 {
		return "Players: names are not available right now.\n"
	}
	names := make([]string, 0, len(st.PlayerNames))
	for _, n := range st.PlayerNames {
		if n = cleanName(n); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return ""
	}
	if maxNames <= 0 {
		return fmt.Sprintf("Players: %d (the list is too long to show).\n", len(names))
	}
	shown, more := names, 0
	if len(names) > maxNames {
		shown, more = names[:maxNames], len(names)-maxNames
	}
	out := "```\n" + strings.Join(shown, ", ")
	if more > 0 {
		out += fmt.Sprintf("\n… and %d more", more)
	}
	return out + "\n```\n"
}

// cleanName makes a player name safe to put in a code block.
func cleanName(n string) string {
	n = strings.Map(func(r rune) rune {
		switch {
		case r == '`':
			return '\''
		case r < 32 || r == 127: // control characters, including line breaks
			return ' '
		}
		return r
	}, n)
	n = strings.Join(strings.Fields(n), " ")
	if r := []rune(n); len(r) > 32 {
		n = string(r[:31]) + "…"
	}
	return n
}

func ago(d time.Duration) string {
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%d s ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// label is the server name followed by its address. The name comes from the game server
// and often holds a Discord invite link, which Discord would turn into a link and a large
// preview card. Text inside a code span is neither linked nor expanded, so the name goes
// in one. With no name, only the address.
func label(title, addr string) string {
	if strings.TrimSpace(title) == "" {
		return code(addr)
	}
	return code(title) + " (" + code(addr) + ")"
}

// code wraps text from outside in an inline code span. A backtick cannot appear inside one.
func code(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "`-`"
	}
	return "`" + strings.ReplaceAll(clip(s), "`", "'") + "`"
}
