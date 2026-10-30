package bot

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
)

// autocomplete answers Discord's live suggestions while a user types an option.
func (b *Bot) autocomplete(in *discordgo.Interaction) []*discordgo.ApplicationCommandOptionChoice {
	data := in.ApplicationCommandData()
	cmd, options := data.Name, data.Options
	sub := "" // the subcommand: for /notify it says what the value means, for /server add or remove
	if (cmd == "notify" || cmd == "server" || cmd == "admin") && len(options) > 0 {
		sub, options = options[0].Name, options[0].Options
	}
	o := toOpts(options)
	var focused *discordgo.ApplicationCommandInteractionDataOption
	for _, opt := range options {
		if opt.Focused {
			focused = opt
		}
	}
	if focused == nil {
		return nil
	}
	typed := ""
	switch v := focused.Value.(type) { // numbers can arrive as text while they are being typed
	case string:
		typed = v
	case float64:
		typed = strconv.Itoa(int(v))
	}
	a := who(in)
	if cmd != "admin" && !b.guildSettings(a).Active {
		return nil // sourcewatch is turned off here
	}
	servers, err := b.deps.Store.ListenerServers(a.Scope)
	if err != nil {
		return nil
	}

	var out []*discordgo.ApplicationCommandOptionChoice
	add := func(name string, value any) {
		if len(out) < rules.MaxSuggestions {
			out = append(out, &discordgo.ApplicationCommandOptionChoice{Name: clipName(name), Value: value})
		}
	}
	switch {
	case focused.Name == "server" || (cmd == "server" && sub == "remove" && focused.Name == "address"):
		for _, s := range servers {
			if strings.Contains(strings.ToLower(s.Addr), strings.ToLower(typed)) {
				add(s.Addr, s.Addr)
			}
		}
	case cmd == "notify" && focused.Name == valueOption(rules.Kind(sub)) && focused.Name != "":
		switch sub {
		case "playercount":
			// Suggest numbers that fit the biggest of the servers it applies to.
			capacity := 0
			targets, _ := targetServers(servers, o.str("server"))
			for _, srv := range targets {
				if st, _, _ := b.deps.Store.ServerState(srv.ID); st != nil && st.MaxPlayers > capacity {
					capacity = st.MaxPlayers
				}
			}
			for _, c := range playerCountChoices(typed, capacity) {
				add(c.name, c.value)
			}
		case "map", "player":
			// Names seen on any of the servers it applies to.
			targets, _ := targetServers(servers, o.str("server"))
			seen := map[string]bool{}
			var known []string
			for _, srv := range targets {
				var names []string
				if sub == "map" {
					names, _ = b.deps.Store.Maps(srv.ID)
				} else {
					names, _ = b.deps.Store.PlayerNames(srv.ID)
				}
				for _, n := range names {
					if !seen[strings.ToLower(n)] {
						seen[strings.ToLower(n)] = true
						known = append(known, n)
					}
				}
			}
			if strings.TrimSpace(typed) == "" {
				sort.Strings(known)
				if len(known) > 10 {
					known = known[:10]
				}
				for _, k := range known {
					add(k, k)
				}
			} else {
				for _, k := range rules.Suggest(typed, known, rules.MaxSuggestions) {
					add(k, k)
				}
			}
		}
	case cmd == "admin" && focused.Name == "blacklist-remove":
		if !a.InGuild || !a.CanManage {
			return nil
		}
		for _, n := range b.blacklistChoices(a, typed) {
			add(n, n)
		}
	case focused.Name == "id" && cmd == "notify":
		ns, _ := b.deps.Store.UserNotifications(a.UserID, a.Scope)
		if sub == "settings" { // this id is text: a number, or "all" for every notification
			if len(ns) > 0 {
				add(fmt.Sprintf("all - every notification you have here (%d)", len(ns)), "all")
			}
			for _, n := range ns {
				add(fmt.Sprintf("ID %d: %s", n.Num, plain(describe(n))), strconv.FormatInt(n.Num, 10))
			}
			break
		}
		for _, n := range ns {
			add(fmt.Sprintf("ID %d: %s", n.Num, plain(describe(n))), n.Num)
		}
	}
	return out
}

// clipName fits Discord's 100 character limit on a choice name.
func clipName(s string) string {
	if r := []rune(s); len(r) > 100 {
		return string(r[:99]) + "…"
	}
	return s
}

func plain(s string) string { return strings.ReplaceAll(s, "`", "") }

type countChoice struct{ name, value string }

var comparisonWords = map[string]string{">=": "at least", ">": "more than", "<=": "at most", "<": "fewer than", "=": "exactly", "!=": "anything but"}

// playerCountChoices suggests values for a player count: a comparison and a number, like
// ">=16" (the value is that text). capacity is the server's size, 0 when not known. A
// comparison typed so far is kept, and a number typed so far is offered with each
// comparison, so the format is easy to find.
func playerCountChoices(typed string, capacity int) []countChoice {
	typed = strings.TrimSpace(typed)
	i := 0
	for i < len(typed) && strings.ContainsRune("<>=!", rune(typed[i])) {
		i++
	}
	if i == 0 { // the single-character comparisons are several bytes long
		for _, sym := range []string{"≥", "≤", "≠"} {
			if strings.HasPrefix(typed, sym) {
				i = len(sym)
			}
		}
	}
	rawOp, digits := typed[:i], strings.TrimSpace(typed[i:])
	var typedN int
	if digits != "" {
		n, err := strconv.Atoi(digits)
		if err != nil || n < 0 || n > rules.MaxPlayerCount {
			return nil
		}
		typedN = n
	}
	ops := []string{">=", ">", "<=", "<", "=", "!="}
	switch {
	case rawOp != "":
		op, ok := rules.NormalizeComparison(rawOp)
		if !ok {
			return nil
		}
		ops = []string{op}
	case digits == "":
		ops = []string{">="} // browsing: at least is the usual wish
	}

	numbers := []int{1, 2, 5, 10, 16, 20, 32}
	if capacity > 0 {
		numbers = []int{1, 5, 10, capacity / 4, capacity / 2, capacity * 3 / 4, capacity - 2, capacity}
	}
	var out []countChoice
	seen := map[string]bool{}
	add := func(op string, n int) {
		if n < 0 || n > rules.MaxPlayerCount || (capacity > 0 && n > capacity) {
			return
		}
		value := fmt.Sprintf("%s%d", op, n)
		if seen[value] {
			return
		}
		seen[value] = true
		plural := "players"
		if n == 1 {
			plural = "player"
		}
		name := fmt.Sprintf("%s %d %s (%s)", comparisonWords[op], n, plural, value)
		switch {
		case capacity > 0 && n == capacity:
			name += " - full"
		case capacity > 0 && n == capacity/2:
			name += " - half full"
		}
		out = append(out, countChoice{name, value})
	}
	if digits != "" {
		for _, op := range ops {
			add(op, typedN)
		}
	}
	if digits == "" || rawOp != "" {
		for _, n := range numbers {
			if digits != "" && !strings.HasPrefix(strconv.Itoa(n), digits) {
				continue
			}
			for _, op := range ops {
				add(op, n)
			}
		}
	}
	return out
}
