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
	return b.notifyAs(who(in), list)
}

// runNotifyRole handles /notify-role: the same as /notify, but the notifications ping a role, are
// shared by the whole Discord server, and only people with Manage Server can use it (E25).
func runNotifyRole(b *Bot, in *discordgo.Interaction, list []*discordgo.ApplicationCommandInteractionDataOption) string {
	a, msg := roleActor(who(in))
	if msg != "" {
		return msg
	}
	return b.notifyAs(a, list)
}

// roleActor turns someone who ran /notify-role into the shared owner of the Discord server's role
// notifications. msg is set when they may not use it.
func roleActor(a actor) (actor, string) {
	if !a.InGuild {
		return a, "`/notify-role` is for Discord servers. Use it in a Discord server, or use `/notify` here."
	}
	if !a.CanManage {
		return a, "You need the Manage Server permission to use `/notify-role`."
	}
	a.UserID, a.Role = roleOwner(a.Scope), true
	return a, ""
}

// cmd is the slash command that made a notification, for messages.
func (a actor) cmd() string {
	if a.Role {
		return "/notify-role"
	}
	return "/notify"
}

func (b *Bot) notifyAs(a actor, list []*discordgo.ApplicationCommandInteractionDataOption) string {
	if len(list) == 0 {
		return b.undeliverableNotice(a) + "Choose what to be notified about: `" + a.cmd() + " map`, `player`, `playercount`, `status` or `title-query`. Or `list`, `show`, `settings`, `schedule`, `remove` and `reset`."
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
	case "schedule":
		return notice + b.notifySchedule(a, o)
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
			fmt.Fprintf(&sb, "- #%d %s\n", n.Num, describe(n))
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
			fmt.Fprintf(&sb, "- #%d %s%s\n", n.Num, describe(n), where)
		}
		sb.WriteString("Ask an admin to let me View Channel and Send Messages there, or move them with `" + a.cmd() + " settings` and `delivery`.\n")
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
	// IDs are the user's own numbers in this place (E18). The id of one of the user's notifications
	// adds the condition to it, and an id the user does not have makes a new notification with it (E20).
	role, msg := b.roleOption(a, o)
	if msg != "" {
		return msg
	}
	var wantID int64
	if id, ok := o.int("id"); ok {
		if _, _, found := b.ownNotification(a, id); found {
			return b.notifyCombine(a, id, cond, role, o)
		}
		if id < 1 {
			return "A notification's ID is a number from 1 up."
		}
		wantID = id
	}

	if a.Role && role == "" {
		return "Choose the role to ping in the `role` option."
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
	n := rules.Notification{UserID: a.UserID, Scope: a.Scope, ServerID: serverID, ChannelID: a.ChannelID, Conditions: []rules.Condition{cond}, RoleID: role}
	n.Delivery = rules.DeliveryChannel
	gs := b.guildSettings(a)
	if a.Role { // a role is pinged in a channel, whatever the default delivery is (E25)
		if !gs.ChannelDelivery {
			return roleChannelOff
		}
		if !gs.RoleMentions {
			return roleMentionsOff
		}
	} else if !a.InGuild || gs.DefaultDelivery == rules.DeliveryDM || !gs.ChannelDelivery {
		n.Delivery = rules.DeliveryDM
	}
	if a.InGuild && gs.DefaultCooldownSec > 0 {
		n.CooldownSec = gs.DefaultCooldownSec
	}
	if a.Role { // the server-level settings of /notify-role win (E26)
		if gs.RoleCooldownSec > 0 {
			n.CooldownSec = gs.RoleCooldownSec
		}
		if gs.RoleChannelID != "" {
			n.ChannelID = gs.RoleChannelID
		}
	}
	if a.InGuild && n.Delivery == rules.DeliveryDM { // a test DM first: this Discord server's default is DM
		if msg := b.checkDM(a); msg != "" {
			return msg
		}
	}
	if err := n.Validate(); err != nil {
		return "That is not a valid notification: " + err.Error() + "."
	}
	n, err = b.deps.Store.AddNotificationAt(n, wantID) // 0 takes the user's lowest free number
	if errors.Is(err, store.ErrIDTaken) {              // made at the same moment by another command of the user
		return fmt.Sprintf("You already have a notification with ID %d here. Try again.", wantID)
	}
	if err != nil {
		return b.fail("add notification", err)
	}
	out := saved(n, addr, b.historyHint(hintServers, cond))
	if wantID != 0 {
		out = fmt.Sprintf("You had no notification with ID %d, so I made a new one with that ID.\n", wantID) + out
	}
	return out
}

const roleMentionsOff = "Role mentions are turned off in this Discord server. Turn them on with `/notify-role settings active:Enabled`."

const roleChannelOff = "An admin turned channel delivery off in this Discord server, and a role can only be pinged in a channel."

// roleOption reads the role of /notify-role. It is empty when none was given. The @everyone role is
// refused, so nobody can ping the whole Discord server through a game server (D2).
func (b *Bot) roleOption(a actor, o opts) (role, msg string) {
	role = o.str("role")
	if role == "" {
		return "", ""
	}
	if !a.Role {
		return "", "`role` is only for `/notify-role`."
	}
	if strings.TrimPrefix(a.Scope, "guild:") == role { // the @everyone role. @here is not a role, and nothing can ping it (D2)
		return "", "`@everyone` and `@here` cannot be pinged. Choose another role."
	}
	return role, ""
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
func (b *Bot) notifyCombine(a actor, id int64, cond rules.Condition, role string, o opts) string {
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
		if m.Num == id {
			n, found = m, true
		}
	}
	if !found {
		return fmt.Sprintf("You have no notification with ID %d here. See yours with `/notify list`.", id)
	}
	if len(n.Conditions) >= rules.MaxConditions {
		return fmt.Sprintf("Notification %d already combines %d conditions, the most it can.", id, rules.MaxConditions)
	}
	if role != "" && role != n.RoleID {
		return fmt.Sprintf("Notification %d pings <@&%s>. Leave `role` out to add to it, or make a new notification with another ID.", id, n.RoleID)
	}
	for _, c := range n.Conditions {
		if c == cond {
			return fmt.Sprintf("Notification %d already has that condition.", id)
		}
	}
	n, err = b.deps.Store.AddCondition(n.ID, a.UserID, cond)
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
	fmt.Fprintf(&sb, "✅ Notification saved as ID: %d\n", n.Num)
	where, subject := "in this channel", "You will be told"
	if n.EffectiveDelivery() == rules.DeliveryDM {
		where = "by DM"
	}
	cmd := "/notify"
	if n.RoleID != "" {
		subject, cmd = "<@&"+n.RoleID+"> will be pinged", "/notify-role"
	}
	on := ""
	switch {
	case n.ServerID == rules.AllServers:
		on = " on all your game servers"
	case addr != "":
		on = " on " + code(addr)
	}
	if phrases := n.Phrases(); len(phrases) == 1 {
		fmt.Fprintf(&sb, "%s %s when %s%s.\n", subject, where, phrases[0], on)
	} else {
		fmt.Fprintf(&sb, "%s %s when all of these are true%s:\n", subject, where, on)
		for _, p := range phrases {
			sb.WriteString("• " + p + "\n")
		}
	}
	fmt.Fprintf(&sb, "Cooldown: %s after it fires.\n", minsText(n.EffectiveCooldown()))
	sb.WriteString(hint)
	if len(n.Conditions) < rules.MaxConditions {
		fmt.Fprintf(&sb, "Add a condition with any `%s` type and `id: %d`. Change where it arrives or its cooldown with `%s settings id: %d`.", cmd, n.Num, cmd, n.Num)
	} else {
		fmt.Fprintf(&sb, "Change where it arrives or its cooldown with `%s settings id: %d`.", cmd, n.Num)
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
			if key := strings.ToLower(strings.TrimSpace(n)); !seen[key] { // spaces around a name do not count
				seen[key] = true
				known = append(known, n)
			}
		}
	}
	if len(known) == 0 || seen[strings.ToLower(strings.TrimSpace(c.Text))] {
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
		return "You have no notifications here yet. Make one with `" + a.cmd() + " map`, `player`, `playercount`, `status` or `title-query`."
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
	switch {
	case a.Role:
		fmt.Fprintf(&sb, "**Role notifications of this Discord server** (%d)\n", len(ns))
	case public: // shared with the channel: say whose they are (a mention shows the name and pings no one)
		fmt.Fprintf(&sb, "**Notifications of <@%s>** (%d)\n", a.UserID, len(ns))
	default:
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
			header = "**Server:** " + code(ref.Addr)
			if st, _, err := b.deps.Store.ServerState(ref.ID); err == nil && st != nil {
				header = "**Server:** " + label(st.Title, ref.Addr)
			}
		}
		header += "\n"
		first := true
		for _, n := range groups[id] {
			line := fmt.Sprintf("`ID: %d` %s\n", n.Num, describe(n))
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
		sb.WriteString("Delete one with `" + a.cmd() + " remove`.")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (b *Bot) notifyRemove(a actor, o opts) string {
	id, ok := o.int("id")
	if !ok {
		return "Choose which notification to delete in the `id` option."
	}
	n, _, found := b.ownNotification(a, id)
	done := false
	if found {
		var err error
		if done, err = b.deps.Store.DeleteNotification(n.ID, a.UserID, a.Scope); err != nil {
			return b.fail("remove notification", err)
		}
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
	parts := n.Phrases()
	via := "channel"
	if n.EffectiveDelivery() == rules.DeliveryDM {
		via = "DM"
	}
	s := strings.Join(parts, " **and** ") + " (via " + via + ", cooldown " + minsText(n.EffectiveCooldown())
	if n.RoleID != "" {
		s += ", pings <@&" + n.RoleID + ">"
	}
	if n.Schedule != nil {
		s += ", " + n.Schedule.String()
	}
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
	// The same for /notify-role, so a press can never delete the other kind (E25).
	customRoleResetConfirm = "notify-role:reset:confirm"
	customRoleResetCancel  = "notify-role:reset:cancel"
)

func isNotifySubcommand(in *discordgo.Interaction, name string) bool {
	data := in.ApplicationCommandData()
	return (data.Name == "notify" || data.Name == "notify-role") && len(data.Options) > 0 && data.Options[0].Name == name
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
	if a.Role {
		return fmt.Sprintf("This deletes all %d role notifications of this Discord server, whoever made them. It cannot be undone. Delete them?", n)
	}
	where := "in this Discord server"
	if !a.InGuild {
		where = "in your DMs with me"
	}
	return fmt.Sprintf("This deletes all %d of your notifications %s. It cannot be undone. Delete them?", n, where)
}

func resetButtons(n int, role bool) []discordgo.MessageComponent {
	confirm, cancel := customResetConfirm, customResetCancel
	if role {
		confirm, cancel = customRoleResetConfirm, customRoleResetCancel
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: fmt.Sprintf("Delete all %d", n), Style: discordgo.DangerButton, CustomID: confirm},
		discordgo.Button{Label: "Cancel", Style: discordgo.SecondaryButton, CustomID: cancel},
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
	switch custom {
	case customResetConfirm, customResetCancel:
	case customRoleResetConfirm, customRoleResetCancel: // only people who may use /notify-role
		ra, msg := roleActor(a)
		if msg != "" {
			return msg, true
		}
		a = ra
		custom = strings.Replace(custom, "notify-role:", "notify:", 1)
	default:
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
		if a.Role {
			return b.roleServerSettings(a, o)
		}
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
	n, err = b.deps.Store.UpdateSettings(n.ID, a.UserID, a.Scope, set)
	if err != nil {
		return b.fail("update settings", err)
	}
	return settingsText(n, addr, "✅ Settings saved for notification ID: %d\n")
}

// roleServerSettings is /notify-role settings without an id: the same settings as /notify settings, but for the
// whole Discord server (E26). `cooldown` and `delivery:channel` are kept as what new role notifications get and
// are applied to the existing ones too. `active` is the switch for all role notifications: disabled, none fires
// and none can be made, and enabled again they start over (E19). Pinging stays off in a channel an admin
// turned channel delivery off in (E25).
func (b *Bot) roleServerSettings(a actor, o opts) string {
	on, onGiven, msg := enabledOption(o, "active")
	if msg != "" {
		return msg
	}
	rest := opts{}
	for k, v := range o {
		if k != "active" && k != "id" {
			rest[k] = v
		}
	}
	set, changed, msg := b.settingsFromOptions(a, rest)
	if msg != "" {
		return msg
	}
	if !changed && !onGiven {
		return b.roleServerSummary(a, "**Role notification settings for this Discord server**\n") +
			"\nChange them with `cooldown`, `active` and `delivery`, or add an `id` to change just one notification."
	}
	patch := store.GuildPatch{}
	if onGiven {
		patch.RoleMentions = &on
	}
	if set.CooldownSec != nil {
		patch.RoleCooldownSec = set.CooldownSec
	}
	if set.ChannelID != nil {
		patch.RoleChannelID = set.ChannelID
	}
	if changed {
		set.Disabled = nil // `active` here is the switch of the whole server
		if _, err := b.deps.Store.UpdateAllSettings(a.UserID, a.Scope, set); err != nil {
			return b.fail("update settings", err)
		}
	}
	if _, err := b.deps.Store.UpdateGuildSettings(a.Scope, patch); err != nil {
		return b.fail("update role settings", err)
	}
	return b.roleServerSummary(a, "✅ Role notification settings saved for this Discord server\n")
}

func (b *Bot) roleServerSummary(a actor, header string) string {
	g := b.guildSettings(a)
	var sb strings.Builder
	sb.WriteString(header)
	if g.RoleMentions {
		sb.WriteString("• Active: enabled\n")
	} else {
		sb.WriteString("• Active: **disabled** (no role notification fires or can be made until you enable them again)\n")
	}
	if g.RoleCooldownSec > 0 {
		fmt.Fprintf(&sb, "• Cooldown of new ones: %s\n", minsText(durationSec(g.RoleCooldownSec)))
	} else {
		fmt.Fprintf(&sb, "• Cooldown of new ones: %s (the default)\n", minsText(b.defaultCooldown(g)))
	}
	if isNumeric(g.RoleChannelID) {
		fmt.Fprintf(&sb, "• Channel of new ones: <#%s>\n", g.RoleChannelID)
	} else {
		sb.WriteString("• Channel of new ones: the channel where they are made\n")
	}
	if ns, err := b.deps.Store.UserNotifications(a.UserID, a.Scope); err == nil {
		fmt.Fprintf(&sb, "• Role notifications: %d", len(ns))
	}
	return sb.String()
}

func (b *Bot) defaultCooldown(g store.GuildSettings) time.Duration {
	if g.DefaultCooldownSec > 0 {
		return durationSec(g.DefaultCooldownSec)
	}
	return rules.DefaultCooldown
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
		if a.Role {
			return set, false, "A role is pinged in a channel, so role notifications cannot go by DM."
		}
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
		if n.Num != id {
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
	fmt.Fprintf(&sb, header, n.Num)
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
	if n.RoleID != "" {
		fmt.Fprintf(&sb, "• Pings: <@&%s>\n", n.RoleID)
	}
	if n.Schedule != nil {
		fmt.Fprintf(&sb, "• Schedule: %s\n", n.Schedule)
	}
	switch {
	case n.ServerID == rules.AllServers:
		sb.WriteString("• Game server: all your game servers\n")
	case addr != "":
		fmt.Fprintf(&sb, "• Game server: %s\n", code(addr))
	}
	sb.WriteString("• When: ")
	sb.WriteString(strings.Join(n.Phrases(), " **and** "))
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

// --- schedule: when a notification may fire (E24) ---

// notifySchedule sets the schedule of one notification (id), or of all of them without an id. A
// notification's own schedule overrides the one for all. Times are UTC; the reply also shows them as
// Discord timestamps, which each reader sees in their own timezone, so nobody has to convert.
func (b *Bot) notifySchedule(a actor, o opts) string {
	var num int64
	if id, ok := o.int("id"); ok {
		if _, _, found := b.ownNotification(a, id); !found {
			return fmt.Sprintf("There is no notification with ID %d here. See yours with `%s list`.", id, a.cmd())
		}
		num = id
	}
	target := "all of your notifications here (the ones with their own schedule keep it)"
	if a.Role {
		target = "all role notifications here (the ones with their own schedule keep it)"
	}
	if num > 0 {
		target = fmt.Sprintf("notification ID: %d", num)
	}
	if strings.EqualFold(o.str("time"), "off") {
		removed, err := b.deps.Store.ClearSchedule(a.UserID, a.Scope, num)
		switch {
		case err != nil:
			return b.fail("clear schedule", err)
		case !removed:
			return "There was no schedule set for " + target + "."
		}
		return "✅ Schedule removed for " + target + "."
	}
	sc, err := rules.ParseSchedule(o.str("time"), o.str("day"))
	if err != nil {
		return "That schedule does not work: " + err.Error() + "."
	}
	if err := b.deps.Store.SetSchedule(a.UserID, a.Scope, num, sc); err != nil {
		return b.fail("set schedule", err)
	}
	return scheduleSaved(sc, target, nowFunc())
}

func scheduleSaved(sc rules.Schedule, target string, now time.Time) string {
	midnight := now.UTC().Truncate(24 * time.Hour)
	at := func(min int) string {
		return fmt.Sprintf("<t:%d:t>", midnight.Add(time.Duration(min)*time.Minute).Unix())
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ Schedule saved for %s.\n", target)
	fmt.Fprintf(&sb, "• Days (UTC): %s\n", sc.DaysText())
	fmt.Fprintf(&sb, "• Time: %s-%s UTC, which is %s to %s in your timezone\n", rules.Clock(sc.StartMin), rules.Clock(sc.EndMin), at(sc.StartMin), at(sc.EndMin))
	sb.WriteString("It fires only inside this window. When the window opens it starts over, so it waits for its conditions to become true again.")
	return sb.String()
}
