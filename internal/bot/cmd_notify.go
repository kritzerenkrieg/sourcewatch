package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kritzerenkrieg/sourcewatch/internal/notify"
	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
	"github.com/kritzerenkrieg/sourcewatch/internal/store"
)

// The notify subcommands that make a notification, one per condition type.
var notifyTypes = []rules.Kind{rules.KindMap, rules.KindPlayer, rules.KindPlayerCount, rules.KindStatus, rules.KindTitle}

// runNotify handles /notify <subcommand>. Every type makes one notification with one condition. The id
// option adds the condition to an existing notification instead, and then all its conditions have to
// hold together (C4). list shows yours to you, and show shares them with the channel.
func runNotify(b *Bot, in *discordgo.Interaction, list []*discordgo.ApplicationCommandInteractionDataOption) string {
	a := who(in)
	if len(list) == 0 {
		return b.undeliverableNotice(a) + "Choose what to be notified about: `/notify map`, `player`, `playercount`, `status` or `title-query`. Or `list`, `show`, `settings`, `remove` and `reset`."
	}
	sub, o := list[0], toOpts(list[0].Options)
	if sub.Name == "show" { // seen by everyone, so nothing private goes in front of it
		return b.notifyList(a, true)
	}
	notice := b.undeliverableNotice(a)
	switch sub.Name {
	case "list":
		return notice + b.notifyList(a, false)
	case "settings":
		return notice + b.notifySettings(a, o)
	case "reset":
		return notice + b.resetPrompt(a)
	case "remove":
		return notice + b.notifyRemove(a, o)
	}
	for _, k := range notifyTypes {
		if string(k) == sub.Name {
			return notice + b.notifyCreate(a, k, o)
		}
	}
	return notice + "Unknown notification type. Try `/help`."
}

// undeliverableNotice tells the user, once, about notifications Discord refused to deliver: by DM
// (E7), or in their channel (D4).
func (b *Bot) undeliverableNotice(a actor) string {
	ns, err := b.deps.Store.TakeUndeliverable(a.UserID)
	if err != nil || len(ns) == 0 {
		return ""
	}
	var dms, channels []rules.Notification
	for _, n := range ns {
		if n.EffectiveDelivery() == rules.DeliveryDM {
			dms = append(dms, n)
		} else {
			channels = append(channels, n)
		}
	}
	var sb strings.Builder
	if len(dms) > 0 {
		sb.WriteString("**Some of your DM notifications could not be delivered** because Discord refused the DM:\n")
		for _, n := range dms {
			fmt.Fprintf(&sb, "- #%d %s\n", n.ID, describe(n))
		}
		sb.WriteString("Send me a DM first, or allow DMs from server members, and they will work again.\n")
	}
	if len(channels) > 0 {
		sb.WriteString("**Some of your notifications could not be posted in their channel**, because it is gone or I may not see it or post in it:\n")
		for _, n := range channels {
			where := ""
			if isNumeric(n.ChannelID) {
				where = fmt.Sprintf(", in <#%s>", n.ChannelID)
			}
			fmt.Fprintf(&sb, "- #%d %s%s\n", n.ID, describe(n), where)
		}
		sb.WriteString("Ask an admin to let me View Channel and Send Messages there, or move them with `/notify settings` and `delivery`.\n")
	}
	sb.WriteString("I switched them back on.\n\n")
	return sb.String()
}

// condition builds the condition from what the user gave: the map name, the user name, or the value.
func condition(kind rules.Kind, o opts) (rules.Condition, string) {
	c := rules.Condition{Kind: kind}
	given := o.str(valueOption(kind))
	switch kind {
	case rules.KindPlayerCount:
		op, n, _, err := rules.ParsePlayerCount(given)
		if err != nil {
			return c, fmt.Sprintf("`value` needs a number of players from 0 to %d, with an optional comparison, like `>=10` (at least 10), `<5` (fewer than 5), `=20` (exactly 20) or `!=3`. A plain number such as `10` means at least 10.", rules.MaxPlayerCount)
		}
		c.Op, c.N = op, n
	case rules.KindStatus:
		online, ok := parseStatus(given)
		if !ok {
			return c, "For `status`, set `value` to `online` or `offline`."
		}
		c.Online = online
	default:
		c.Text = given
		if c.Text == "" {
			return c, "Give the " + what(kind) + " in the `" + valueOption(kind) + "` option."
		}
	}
	if err := c.Validate(); err != nil {
		return c, "That is not a valid condition: " + err.Error() + "."
	}
	return c, ""
}

// what names the thing a type's value holds, for messages.
func what(kind rules.Kind) string {
	switch kind {
	case rules.KindMap:
		return "map name"
	case rules.KindPlayer:
		return "player's name"
	}
	return "word to look for in the server name"
}

func (b *Bot) notifyCreate(a actor, kind rules.Kind, o opts) string {
	cond, msg := condition(kind, o)
	if msg != "" {
		return msg
	}
	if msg := b.blacklistRefusal(a, cond); msg != "" {
		return msg
	}
	if id, ok := o.int("id"); ok {
		return b.notifyCombine(a, id, cond, o)
	}

	servers, err := b.deps.Store.ListenerServers(a.Scope)
	if err != nil {
		return b.fail("list servers", err)
	}
	if len(servers) == 0 {
		if a.InGuild {
			return "No game server is set for this Discord server yet. Someone with Manage Server needs to run `/server add` first."
		}
		return "You are not watching any game server yet. Add one with `/server add`."
	}
	// With a server named the notification is for that one. Without, it is for all of them, including
	// ones added later, as a single notification (E9).
	serverID, addr, hintServers := rules.AllServers, "", serverIDs(servers)
	if named := o.str("server"); named != "" {
		ref, ok := matchServer(servers, named)
		if !ok {
			return fmt.Sprintf("`%s` is not one of the game servers you watch.", clip(named))
		}
		serverID, addr, hintServers = ref.ID, ref.Addr, []int64{ref.ID}
	}
	count, err := b.deps.Store.CountUserNotifications(a.UserID)
	if err != nil {
		return b.fail("count notifications", err)
	}
	if count >= maxNotificationsPerUser {
		return fmt.Sprintf("You already have %d notifications. Delete one first with `/notify remove`.", maxNotificationsPerUser)
	}

	// A new notification uses the defaults: the channel it was made in, or a DM when made in a DM with
	// the bot (there is no channel there). An admin can change the defaults of a Discord server with
	// /admin settings, and /notify settings changes one notification afterwards.
	n := rules.Notification{UserID: a.UserID, Scope: a.Scope, ServerID: serverID, ChannelID: a.ChannelID, Conditions: []rules.Condition{cond}}
	n.Delivery = rules.DeliveryChannel
	gs := b.guildSettings(a)
	if !a.InGuild || gs.DefaultDelivery == rules.DeliveryDM || !gs.ChannelDelivery {
		n.Delivery = rules.DeliveryDM
	}
	if a.InGuild && gs.DefaultCooldownSec > 0 {
		n.CooldownSec = gs.DefaultCooldownSec
	}
	if a.InGuild && n.Delivery == rules.DeliveryDM { // a test DM first: this Discord server's default is DM
		if msg := b.checkDM(a); msg != "" {
			return msg
		}
	}
	if err := n.Validate(); err != nil {
		return "That is not a valid notification: " + err.Error() + "."
	}
	id, err := b.deps.Store.AddNotification(n)
	if err != nil {
		return b.fail("add notification", err)
	}
	n.ID = id
	return saved(n, addr, b.historyHint(hintServers, cond))
}

// checkDM sends a test DM before a DM notification is made (E7). It returns a message when
// the notification cannot be made.
func (b *Bot) checkDM(a actor) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := b.deps.DM.ProbeDM(ctx, a.UserID)
	switch {
	case errors.Is(err, notify.ErrDMClosed):
		return "I can't send you a DM. Send me a message first (open my profile and use Message), or allow direct messages from server members in this Discord server's privacy settings, then try again."
	case err != nil:
		b.log.Warn("probe dm", "err", err)
		return "I could not send you a test DM. Please try again later."
	}
	return ""
}

// notifyCombine adds a condition to a notification the user already has.
func (b *Bot) notifyCombine(a actor, id int64, cond rules.Condition, o opts) string {
	if _, given := o["server"]; given {
		return "A notification you add to keeps the game server it was made with. Leave `server` out, or make a new notification."
	}
	mine, err := b.deps.Store.UserNotifications(a.UserID, a.Scope)
	if err != nil {
		return b.fail("list notifications", err)
	}
	var n rules.Notification
	found := false
	for _, m := range mine {
		if m.ID == id {
			n, found = m, true
		}
	}
	if !found {
		return fmt.Sprintf("You have no notification with ID %d here. See yours with `/notify list`.", id)
	}
	if len(n.Conditions) >= rules.MaxConditions {
		return fmt.Sprintf("Notification %d already combines %d conditions, the most it can.", id, rules.MaxConditions)
	}
	for _, c := range n.Conditions {
		if c == cond {
			return fmt.Sprintf("Notification %d already has that condition.", id)
		}
	}
	n, err = b.deps.Store.AddCondition(id, a.UserID, cond)
	if err != nil {
		return b.fail("combine notification", err)
	}
	servers, _ := b.deps.Store.ListenerServers(a.Scope)
	addr, hintServers := "", serverIDs(servers) // a notification for all servers is about every one of them
	for _, s := range servers {
		if s.ID == n.ServerID {
			addr, hintServers = s.Addr, []int64{s.ID}
		}
	}
	return saved(n, addr, b.historyHint(hintServers, cond))
}

func saved(n rules.Notification, addr, hint string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ Notification saved as ID: %d\n", n.ID)
	where := "in this channel"
	if n.EffectiveDelivery() == rules.DeliveryDM {
		where = "by DM"
	}
	on := ""
	switch {
	case n.ServerID == rules.AllServers:
		on = " on all your game servers"
	case addr != "":
		on = " on " + code(addr)
	}
	if len(n.Conditions) == 1 {
		fmt.Fprintf(&sb, "You will be told %s when %s%s.\n", where, n.Conditions[0].Phrase(), on)
	} else {
		fmt.Fprintf(&sb, "You will be told %s when all of these are true%s:\n", where, on)
		for _, c := range n.Conditions {
			sb.WriteString("• " + c.Phrase() + "\n")
		}
	}
	fmt.Fprintf(&sb, "Cooldown: %s after it fires.\n", minsText(n.EffectiveCooldown()))
	sb.WriteString(hint)
	if len(n.Conditions) < rules.MaxConditions {
		fmt.Fprintf(&sb, "Add a condition with any `/notify` type and `id: %d`. Change where it arrives or its cooldown with `/notify settings id: %d`.", n.ID, n.ID)
	} else {
		fmt.Fprintf(&sb, "Change where it arrives or its cooldown with `/notify settings id: %d`.", n.ID)
	}
	return sb.String()
}

// minsText writes a cooldown like "60 mins".
func minsText(d time.Duration) string {
	if m := int(d / time.Minute); m != 1 {
		return fmt.Sprintf("%d mins", m)
	}
	return "1 min"
}

func (b *Bot) historyHint(serverIDs []int64, c rules.Condition) string {
	if c.Kind != rules.KindMap && c.Kind != rules.KindPlayer {
		return ""
	}
	seen := map[string]bool{}
	var known []string
	for _, id := range serverIDs {
		var names []string
		var err error
		if c.Kind == rules.KindMap {
			names, err = b.deps.Store.Maps(id)
		} else {
			names, err = b.deps.Store.PlayerNames(id)
		}
		if err != nil {
			return ""
		}
		for _, n := range names {
			if !seen[strings.ToLower(n)] {
				seen[strings.ToLower(n)] = true
				known = append(known, n)
			}
		}
	}
	if len(known) == 0 || seen[strings.ToLower(c.Text)] {
		return ""
	}
	msg := fmt.Sprintf("I have not seen %s %s on this server yet.", c.Kind, code(c.Text))
	if len(serverIDs) > 1 {
		msg = fmt.Sprintf("I have not seen %s %s on any of your servers yet.", c.Kind, code(c.Text))
	}
	if sugg := rules.Suggest(c.Text, known, 5); len(sugg) > 0 {
		msg += " Did you mean: " + strings.Join(quote(sugg), ", ") + "? Delete this one and make it again if so."
	}
	return msg + "\n"
}

func serverIDs(servers []store.ServerRef) []int64 {
	ids := make([]int64, len(servers))
	for i, s := range servers {
		ids[i] = s.ID
	}
	return ids
}

func (b *Bot) notifyList(a actor, public bool) string {
	ns, err := b.deps.Store.UserNotifications(a.UserID, a.Scope)
	if err != nil {
		return b.fail("list notifications", err)
	}
	if len(ns) == 0 {
		return "You have no notifications here yet. Make one with `/notify map`, `player`, `playercount`, `status` or `title-query`."
	}
	servers, _ := b.deps.Store.ListenerServers(a.Scope)
	byID := map[int64]store.ServerRef{}
	for _, s := range servers {
		byID[s.ID] = s
	}

	// Group by game server, in the order the servers were added. "All servers" comes first.
	order := []int64{}
	groups := map[int64][]rules.Notification{}
	for _, n := range ns {
		if _, ok := groups[n.ServerID]; !ok {
			if n.ServerID == rules.AllServers {
				order = append([]int64{rules.AllServers}, order...)
			} else {
				order = append(order, n.ServerID)
			}
		}
		groups[n.ServerID] = append(groups[n.ServerID], n)
	}

	// Discord limits a reply to 2000 characters, and 25 long notifications can pass that.
	// Lines are added until the limit; a server's header only goes in with its first line.
	var sb strings.Builder
	if public { // shared with the channel: say whose they are (a mention shows the name and pings no one)
		fmt.Fprintf(&sb, "**Notifications of <@%s>** (%d)\n", a.UserID, len(ns))
	} else {
		fmt.Fprintf(&sb, "**Your notifications** (%d)\n", len(ns))
	}
	const limit = 1750 // bytes, so it is safe in characters too, and leaves room for the footer
	shown := 0
fill:
	for _, id := range order {
		ref := byID[id]
		header := "Server removed"
		switch {
		case id == rules.AllServers:
			header = fmt.Sprintf("**All servers** (%d)", len(servers))
		case ref.Addr != "":
			header = code(ref.Addr)
			if st, _, err := b.deps.Store.ServerState(ref.ID); err == nil && st != nil {
				header = label(st.Title, ref.Addr)
			}
		}
		header += "\n"
		first := true
		for _, n := range groups[id] {
			line := fmt.Sprintf("`ID: %d` %s\n", n.ID, describe(n))
			need := len(line)
			if first {
				need += len(header)
			}
			if sb.Len()+need > limit {
				break fill
			}
			if first {
				sb.WriteString(header)
				first = false
			}
			sb.WriteString(line)
			shown++
		}
	}
	if shown < len(ns) {
		fmt.Fprintf(&sb, "… and %d more.\n", len(ns)-shown)
	}
	if !public {
		sb.WriteString("Delete one with `/notify remove`.")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (b *Bot) notifyRemove(a actor, o opts) string {
	id, ok := o.int("id")
	if !ok {
		return "Choose which notification to delete in the `id` option."
	}
	done, err := b.deps.Store.DeleteNotification(id, a.UserID, a.Scope)
	if err != nil {
		return b.fail("remove notification", err)
	}
	if !done {
		return fmt.Sprintf("You have no notification with ID %d here. See yours with `/notify list`.", id)
	}
	return fmt.Sprintf("✅ Deleted notification ID: %d.", id)
}

// targetServers is the game servers a command applies to: the one named, or all of them when no
// server was named (E9). A name that is not one of the user's servers gives a message.
func targetServers(servers []store.ServerRef, named string) ([]store.ServerRef, string) {
	if named == "" {
		return servers, ""
	}
	if r, ok := matchServer(servers, named); ok {
		return []store.ServerRef{r}, ""
	}
	return nil, fmt.Sprintf("`%s` is not one of the game servers you watch.", clip(named))
}

func parseStatus(v string) (online, ok bool) {
	switch strings.ToLower(v) {
	case "online", "up", "true":
		return true, true
	case "offline", "down", "false":
		return false, true
	}
	return false, false
}

func describe(n rules.Notification) string {
	parts := make([]string, len(n.Conditions))
	for i, c := range n.Conditions {
		parts[i] = c.Phrase()
	}
	via := "channel"
	if n.EffectiveDelivery() == rules.DeliveryDM {
		via = "DM"
	}
	s := strings.Join(parts, " **and** ") + " (via " + via + ", cooldown " + minsText(n.EffectiveCooldown())
	if n.Disabled {
		s += ", disabled"
	}
	return s + ")"
}

func quote(list []string) []string {
	out := make([]string, len(list))
	for i, v := range list {
		out[i] = code(v)
	}
	return out
}

// --- reset: delete all of a user's notifications here, after asking ---

const (
	customResetConfirm = "notify:reset:confirm"
	customResetCancel  = "notify:reset:cancel"
)

func isNotifySubcommand(in *discordgo.Interaction, name string) bool {
	data := in.ApplicationCommandData()
	return data.Name == "notify" && len(data.Options) > 0 && data.Options[0].Name == name
}

func (b *Bot) notificationCount(a actor) int {
	ns, err := b.deps.Store.UserNotifications(a.UserID, a.Scope)
	if err != nil {
		return 0
	}
	return len(ns)
}

func (b *Bot) resetPrompt(a actor) string {
	n := b.notificationCount(a)
	if n == 0 {
		return "You have no notifications here to delete."
	}
	where := "in this Discord server"
	if !a.InGuild {
		where = "in your DMs with me"
	}
	return fmt.Sprintf("This deletes all %d of your notifications %s. It cannot be undone. Delete them?", n, where)
}

func resetButtons(n int) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: fmt.Sprintf("Delete all %d", n), Style: discordgo.DangerButton, CustomID: customResetConfirm},
		discordgo.Button{Label: "Cancel", Style: discordgo.SecondaryButton, CustomID: customResetCancel},
	}}}
}

// component handles a button press. The buttons are on a reply only the person who ran the
// command can see, and the press is checked against whoever pressed it, so it can only delete
// the presser's own notifications.
func (b *Bot) component(r responder, in *discordgo.Interaction) {
	content, ok := b.componentReply(in)
	if !ok {
		return
	}
	err := r.InteractionRespond(in, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:         content,
			Components:      []discordgo.MessageComponent{}, // the buttons are used up
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
		},
	})
	if err != nil {
		b.log.Warn("respond to button", "err", err)
	}
}

func (b *Bot) componentReply(in *discordgo.Interaction) (string, bool) {
	a := who(in)
	custom := in.MessageComponentData().CustomID
	if custom != customResetConfirm && custom != customResetCancel {
		return "", false
	}
	// The buttons can be older than an admin turning sourcewatch off here (E15).
	if !b.guildSettings(a).Active {
		return disabledMessage, true
	}
	switch custom {
	case customResetConfirm:
		n, err := b.deps.Store.DeleteAllNotifications(a.UserID, a.Scope)
		if err != nil {
			return b.fail("reset notifications", err), true
		}
		if n == 0 {
			return "There was nothing left to delete.", true
		}
		return fmt.Sprintf("✅ Deleted %d notifications.", n), true
	case customResetCancel:
		return "Nothing was deleted.", true
	}
	return "", false
}

// --- settings: cooldown, on/off and delivery of one notification ---

// notifySettings shows or changes the settings of one notification, or of all of the user's
// notifications here when the id is "all" or left out. The game server cannot be changed, because every
// condition of the notification is about that server (E14).
func (b *Bot) notifySettings(a actor, o opts) string {
	target := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(o.str("id")), "#"))
	if target == "" || target == "all" { // without an id the settings are for all of them

		return b.settingsForAll(a, o)
	}
	id, err := strconv.ParseInt(target, 10, 64)
	if err != nil {
		return fmt.Sprintf("`id` is a notification's number, or `all`. `%s` is neither.", clip(o.str("id")))
	}
	n, addr, found := b.ownNotification(a, id)
	if !found {
		return fmt.Sprintf("You have no notification with ID %d here. See yours with `/notify list`.", id)
	}
	set, changed, msg := b.settingsFromOptions(a, o)
	if msg != "" {
		return msg
	}
	if !changed {
		return settingsText(n, addr, "**Settings of notification ID: %d**\n")
	}
	n, err = b.deps.Store.UpdateSettings(id, a.UserID, a.Scope, set)
	if err != nil {
		return b.fail("update settings", err)
	}
	return settingsText(n, addr, "✅ Settings saved for notification ID: %d\n")
}

// settingsForAll applies the settings to every notification the user has here.
func (b *Bot) settingsForAll(a actor, o opts) string {
	mine, err := b.deps.Store.UserNotifications(a.UserID, a.Scope)
	if err != nil {
		return b.fail("list notifications", err)
	}
	if len(mine) == 0 {
		return "You have no notifications here."
	}
	set, changed, msg := b.settingsFromOptions(a, o)
	if msg != "" {
		return msg
	}
	if !changed {
		off := 0
		for _, n := range mine {
			if n.Disabled {
				off++
			}
		}
		return fmt.Sprintf("You have %d notifications here: %d enabled, %d disabled.\nGive `cooldown`, `active` or `delivery` to change all of them at once, or add an `id` to change just one.", len(mine), len(mine)-off, off)
	}
	updated, err := b.deps.Store.UpdateAllSettings(a.UserID, a.Scope, set)
	if err != nil {
		return b.fail("update settings", err)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ Settings saved for all %d notifications.\n", len(updated))
	if set.Disabled != nil {
		if *set.Disabled {
			sb.WriteString("• Active: disabled (none of them will fire until you enable them again)\n")
		} else {
			sb.WriteString("• Active: enabled\n")
		}
	}
	if set.Delivery != nil {
		if *set.Delivery == rules.DeliveryDM {
			sb.WriteString("• Delivery: by DM\n")
		} else {
			fmt.Fprintf(&sb, "• Delivery: in <#%s>\n", a.ChannelID)
		}
	}
	if set.CooldownSec != nil {
		fmt.Fprintf(&sb, "• Cooldown: %s\n", minsText(time.Duration(*set.CooldownSec)*time.Second))
	}
	sb.WriteString("See them with `/notify list`.")
	return sb.String()
}

// settingsFromOptions reads the settings the user gave. changed is false when none were given.
// Choosing a DM sends a test DM first (E7).
func (b *Bot) settingsFromOptions(a actor, o opts) (set store.Settings, changed bool, msg string) {
	if m, ok := o.int("cooldown"); ok {
		sec := int(m) * 60
		set.CooldownSec = &sec
	}
	switch o.str("active") {
	case "":
	case "enabled", "disabled":
		off := o.str("active") == "disabled"
		set.Disabled = &off
	default:
		return set, false, "`active` is `enabled` or `disabled`."
	}
	switch d := o.str("delivery"); d {
	case "":
	case rules.DeliveryChannel:
		if !a.InGuild {
			return set, false, "There is no channel in a DM with me, so a notification here stays a DM."
		}
		if !b.guildSettings(a).ChannelDelivery {
			return set, false, "An admin turned channel delivery off in this Discord server, so notifications here go by DM."
		}
		set.Delivery, set.ChannelID = &d, &a.ChannelID
	case rules.DeliveryDM:
		if msg := b.checkDM(a); msg != "" {
			return set, false, msg
		}
		set.Delivery = &d
	default:
		return set, false, "`delivery` is `channel` or `dm`."
	}
	return set, set.CooldownSec != nil || set.Disabled != nil || set.Delivery != nil, ""
}

// ownNotification finds one of the user's notifications in this place, with its server address.
func (b *Bot) ownNotification(a actor, id int64) (rules.Notification, string, bool) {
	mine, err := b.deps.Store.UserNotifications(a.UserID, a.Scope)
	if err != nil {
		return rules.Notification{}, "", false
	}
	servers, _ := b.deps.Store.ListenerServers(a.Scope)
	for _, n := range mine {
		if n.ID != id {
			continue
		}
		for _, s := range servers {
			if s.ID == n.ServerID {
				return n, s.Addr, true
			}
		}
		return n, "", true
	}
	return rules.Notification{}, "", false
}

// settingsText lists how a notification behaves. header has one %d for the ID.
func settingsText(n rules.Notification, addr, header string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, header, n.ID)
	state := "enabled"
	if n.Disabled {
		state = "disabled (it will not fire until you enable it again)"
	}
	fmt.Fprintf(&sb, "• Active: %s\n", state)
	if n.EffectiveDelivery() == rules.DeliveryDM {
		sb.WriteString("• Delivery: by DM\n")
	} else if isNumeric(n.ChannelID) {
		fmt.Fprintf(&sb, "• Delivery: in <#%s>\n", n.ChannelID)
	} else {
		sb.WriteString("• Delivery: in a channel\n")
	}
	if n.CooldownSec <= 0 {
		fmt.Fprintf(&sb, "• Cooldown: %s (the default)\n", minsText(n.EffectiveCooldown()))
	} else {
		fmt.Fprintf(&sb, "• Cooldown: %s\n", minsText(n.EffectiveCooldown()))
	}
	switch {
	case n.ServerID == rules.AllServers:
		sb.WriteString("• Game server: all your game servers\n")
	case addr != "":
		fmt.Fprintf(&sb, "• Game server: %s\n", code(addr))
	}
	sb.WriteString("• When: ")
	parts := make([]string, len(n.Conditions))
	for i, c := range n.Conditions {
		parts[i] = c.Phrase()
	}
	sb.WriteString(strings.Join(parts, " **and** "))
	return sb.String()
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
