package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
)

// rguild builds /notify-role <sub> run by a member of Discord server g1.
func rguild(user string, perms int64, sub string, options ...opt) *discordgo.Interaction {
	return guild(user, perms, "notify-role", subcommand(sub, options...))
}

func TestScheduleForAllAndForOneNotification(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	nowFunc = func() time.Time { return time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { nowFunc = time.Now })
	e.b.execute(nguild("u1", 0, "map", str("map-name", "de_dust2")))
	e.b.execute(nguild("u1", 0, "map", str("map-name", "de_nuke")))

	got := e.b.execute(nguild("u1", 0, "schedule", str("time", "20:00-23:30"), str("day", "0000011")))
	// 20:00 UTC on 2026-10-27 is 1793131200, shown as a Discord timestamp for each reader's own timezone.
	contains(t, got, "Schedule saved for all of your notifications", "Sat, Sun", "20:00-23:30 UTC", "<t:1793131200:t>", "<t:1793143800:t>")
	ns, _ := e.st.UserNotifications("u1", "guild:g1")
	for _, n := range ns {
		if n.Schedule == nil || n.Schedule.Mask() != "0000011" {
			t.Fatalf("the schedule for all applies to every notification: %+v", n.Schedule)
		}
	}

	// One notification's own schedule overrides the one for all.
	contains(t, e.b.execute(nguild("u1", 0, "schedule", str("time", "08:00-09:00"), num("id", 2))), "notification ID: 2", "08:00-09:00 UTC", "every day")
	ns, _ = e.st.UserNotifications("u1", "guild:g1")
	if ns[0].Schedule.StartMin != 20*60 || ns[1].Schedule.StartMin != 8*60 {
		t.Fatalf("own schedule must win for ID 2 only: %+v %+v", ns[0].Schedule, ns[1].Schedule)
	}
	contains(t, e.b.execute(nguild("u1", 0, "list")), "Sat, Sun 20:00-23:30 UTC", "every day 08:00-09:00 UTC")
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", "2"))), "Schedule: every day 08:00-09:00 UTC")

	// An id that does not exist is reported, and nothing is saved.
	contains(t, e.b.execute(nguild("u1", 0, "schedule", str("time", "01:00-02:00"), num("id", 9))), "no notification with ID 9")
	if sc, _ := e.st.Schedule("u1", "guild:g1", 9); sc != nil {
		t.Fatal("saved a schedule for a missing notification")
	}
	// Someone else's numbers are not yours.
	contains(t, e.b.execute(nguild("u2", 0, "schedule", str("time", "01:00-02:00"), num("id", 1))), "no notification with ID 1")

	// off removes it.
	contains(t, e.b.execute(nguild("u1", 0, "schedule", str("time", "off"), num("id", 2))), "Schedule removed", "notification ID: 2")
	contains(t, e.b.execute(nguild("u1", 0, "schedule", str("time", "OFF"))), "Schedule removed", "all of your")
	contains(t, e.b.execute(nguild("u1", 0, "schedule", str("time", "off"))), "no schedule set")
}

func TestScheduleRefusesBadInput(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "de_dust2")))
	for _, tc := range []struct {
		time, day, want string
	}{
		{"20:00", "", "HH:MM-HH:MM"}, {"25:00-26:00", "", "not a time"}, {"10:00-10:00", "", "same time"},
		{"10:00-11:00", "11", "seven digits"}, {"10:00-11:00", "0000000", "at least one 1"}, {"10:00-11:00", "abcdefg", "only holds 0 and 1"},
	} {
		got := e.b.execute(nguild("u1", 0, "schedule", str("time", tc.time), str("day", tc.day)))
		contains(t, got, "does not work", tc.want)
	}
	if sc, _ := e.st.Schedule("u1", "guild:g1", 0); sc != nil {
		t.Fatal("a bad schedule was saved")
	}
}

func TestNotifyRoleNeedsManageServerAndAGuild(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	contains(t, e.b.execute(rguild("u1", 0, "map", str("map-name", "x"), str("role", "777"))), "Manage Server")
	contains(t, e.b.execute(rguild("u1", 0, "list")), "Manage Server")
	contains(t, e.b.execute(dm("u1", "notify-role", subcommand("map", str("map-name", "x"), str("role", "777")))), "Discord servers")
	if got, _ := e.st.UserNotifications(roleOwner("guild:g1"), "guild:g1"); len(got) != 0 {
		t.Fatal("a refused command saved something")
	}
}

func TestNotifyRoleIsSharedByTheServerAndSeparateFromPersonalOnes(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	got := e.b.execute(rguild("m1", manage, "map", str("map-name", "de_dust2"), str("role", "777")))
	contains(t, got, "✅ Notification saved as ID: 1", "<@&777> will be pinged in this channel", "the map is `de_dust2`", "/notify-role settings id: 1")
	// Another manager continues the same numbering, and can combine into it.
	contains(t, e.b.execute(rguild("m2", manage, "map", str("map-name", "de_nuke"), str("role", "888"))), "saved as ID: 2")
	contains(t, e.b.execute(rguild("m2", manage, "playercount", str("value", ">=10"), num("id", 1))), "all of these are true", "<@&777>")
	ns, _ := e.st.UserNotifications(roleOwner("guild:g1"), "guild:g1")
	if len(ns) != 2 || ns[0].RoleID != "777" || len(ns[0].Conditions) != 2 || ns[0].Delivery != rules.DeliveryChannel || ns[0].ChannelID != "c1" {
		t.Fatalf("%+v", ns)
	}
	// Personal notifications are separate: the same manager's own list is empty, and numbering starts at 1.
	contains(t, e.b.execute(nguild("m1", manage, "list")), "no notifications")
	contains(t, e.b.execute(nguild("m1", manage, "map", str("map-name", "de_inferno"))), "saved as ID: 1")
	list := e.b.execute(rguild("m2", manage, "list"))
	contains(t, list, "Role notifications of this Discord server", "pings <@&777>", "pings <@&888>")
	if strings.Contains(list, "de_inferno") {
		t.Fatalf("a personal notification showed in the role list: %s", list)
	}
	// Any manager can change or remove it.
	contains(t, e.b.execute(rguild("m2", manage, "remove", num("id", 1))), "Deleted notification ID: 1")
	// Another Discord server does not share them.
	if other, _ := e.st.UserNotifications(roleOwner("guild:g2"), "guild:g2"); len(other) != 0 {
		t.Fatal(other)
	}
}

func TestNotifyRoleRules(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	contains(t, e.b.execute(rguild("m1", manage, "map", str("map-name", "x"))), "Choose the role")
	contains(t, e.b.execute(rguild("m1", manage, "map", str("map-name", "x"), str("role", "g1"))), "`@everyone` and `@here`")
	e.b.execute(rguild("m1", manage, "map", str("map-name", "x"), str("role", "777")))
	contains(t, e.b.execute(rguild("m1", manage, "status", str("value", "online"), str("role", "888"), num("id", 1))), "pings <@&777>")
	contains(t, e.b.execute(rguild("m1", manage, "status", str("value", "online"), str("role", "777"), num("id", 1))), "all of these are true")
	// A role is pinged in a channel: no DMs, and not where an admin turned channels off.
	contains(t, e.b.execute(rguild("m1", manage, "settings", str("id", "1"), str("delivery", "dm"))), "cannot go by DM")
	if len(e.dm.probed) != 0 {
		t.Fatal("a role notification sent a test DM")
	}
	contains(t, e.b.execute(guild("m1", manage, "admin", subcommand("settings", str("default-delivery", "dm")))), "saved")
	contains(t, e.b.execute(rguild("m1", manage, "map", str("map-name", "y"), str("role", "777"))), "pinged in this channel")
	e.b.execute(guild("m1", manage, "admin", subcommand("settings", str("channel-delivery", "disabled"))))
	contains(t, e.b.execute(rguild("m1", manage, "map", str("map-name", "z"), str("role", "777"))), "only be pinged in a channel")
	// The personal command has no role option.
	contains(t, e.b.execute(nguild("u1", 0, "map", str("map-name", "q"), str("role", "777"))), "only for `/notify-role`")
	// A schedule works for them too.
	contains(t, e.b.execute(rguild("m1", manage, "schedule", str("time", "18:00-20:00"), num("id", 1))), "Schedule saved")
}

func TestNotifyRoleResetUsesItsOwnButtons(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(rguild("m1", manage, "map", str("map-name", "x"), str("role", "777")))
	e.b.execute(nguild("m1", manage, "map", str("map-name", "y")))
	r := e.b.executeReply(rguild("m1", manage, "reset"))
	contains(t, r.content, "1 role notifications")
	row := r.components[0].(discordgo.ActionsRow)
	confirm := row.Components[0].(discordgo.Button).CustomID
	if confirm != customRoleResetConfirm {
		t.Fatalf("%q", confirm)
	}
	press := func(user string, perms int64, custom string) string {
		in := guild(user, perms, "x")
		in.Type = discordgo.InteractionMessageComponent
		in.Data = discordgo.MessageComponentInteractionData{CustomID: custom}
		got, _ := e.b.componentReply(in)
		return got
	}
	contains(t, press("u9", 0, confirm), "Manage Server")
	if n, _ := e.st.UserNotifications(roleOwner("guild:g1"), "guild:g1"); len(n) != 1 {
		t.Fatal("someone without Manage Server deleted role notifications")
	}
	contains(t, press("m1", manage, confirm), "Deleted 1 notifications")
	if n, _ := e.st.UserNotifications(roleOwner("guild:g1"), "guild:g1"); len(n) != 0 {
		t.Fatal("not deleted")
	}
	if n, _ := e.st.UserNotifications("m1", "guild:g1"); len(n) != 1 {
		t.Fatal("the role reset deleted a personal notification")
	}
}

func TestRoleIsARequiredOptionOfEveryTypeCommand(t *testing.T) {
	cmd, _ := find("notify-role")
	for _, sub := range cmd.options {
		switch sub.Name {
		case "map", "player", "playercount", "status", "title-query":
			var role *discordgo.ApplicationCommandOption
			for _, o := range sub.Options {
				if o.Name == "role" {
					role = o
				}
			}
			if role == nil || !role.Required || role.Type != discordgo.ApplicationCommandOptionRole {
				t.Errorf("/notify-role %s must have a required role option", sub.Name)
			}
		}
	}
	personal, _ := find("notify")
	for _, sub := range personal.options {
		for _, o := range sub.Options {
			if o.Name == "role" {
				t.Errorf("/notify %s must not have %s", sub.Name, o.Name)
			}
		}
	}
}

func TestRoleSettingsWithoutAnIDAreServerLevel(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(rguild("m1", manage, "map", str("map-name", "x"), str("role", "777")))
	e.b.execute(rguild("m1", manage, "map", str("map-name", "y"), str("role", "777")))
	contains(t, e.b.execute(rguild("m1", manage, "settings")), "Role notification settings for this Discord server",
		"Active: enabled", "Cooldown of new ones: 60 mins (the default)", "where they are made", "Role notifications: 2")
	contains(t, e.b.execute(rguild("m1", 0, "settings", num("cooldown", 5))), "Manage Server")

	// cooldown and delivery are kept for new ones and applied to the existing ones.
	set := rguild("m1", manage, "settings", num("cooldown", 5), str("delivery", "channel"))
	set.ChannelID = "111"
	contains(t, e.b.execute(set), "settings saved", "Cooldown of new ones: 5 mins", "<#111>")
	ns, _ := e.st.UserNotifications(roleOwner("guild:g1"), "guild:g1")
	if ns[0].CooldownSec != 300 || ns[1].CooldownSec != 300 {
		t.Fatalf("existing ones follow: %+v", ns)
	}
	in := rguild("m1", manage, "map", str("map-name", "z"), str("role", "777"))
	in.ChannelID = "999" // made elsewhere, but the server-level channel wins
	e.b.execute(in)
	ns, _ = e.st.UserNotifications(roleOwner("guild:g1"), "guild:g1")
	if len(ns) != 3 || ns[2].CooldownSec != 300 || ns[2].ChannelID != "111" || ns[0].ChannelID != "111" {
		t.Fatalf("new ones follow: %+v", ns[2])
	}
	// With an id it is just that notification.
	contains(t, e.b.execute(rguild("m1", manage, "settings", str("id", "1"), num("cooldown", 9))), "9 mins")
	ns, _ = e.st.UserNotifications(roleOwner("guild:g1"), "guild:g1")
	if ns[0].CooldownSec != 540 || ns[1].CooldownSec != 300 {
		t.Fatalf("%+v", ns)
	}
	if g, _ := e.st.GuildSettings("guild:g1"); g.RoleCooldownSec != 300 {
		t.Fatal("an id must not change the server-level cooldown")
	}
	contains(t, e.b.execute(rguild("m1", manage, "settings", str("delivery", "dm"))), "cannot go by DM")
}

func TestRoleSettingsActiveIsTheSwitchOfAllRoleNotifications(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(rguild("m1", manage, "map", str("map-name", "x"), str("role", "777")))
	contains(t, e.b.execute(rguild("m1", manage, "settings", str("active", "disabled"))), "Active: **disabled**")
	ns, _ := e.st.UserNotifications(roleOwner("guild:g1"), "guild:g1")
	if ns[0].Disabled {
		t.Fatal("the server switch does not touch each notification's own switch")
	}
	contains(t, e.b.execute(rguild("m1", manage, "map", str("map-name", "y"), str("role", "777"))), "Role mentions are turned off")
	contains(t, e.b.execute(nguild("u1", 0, "map", str("map-name", "y"))), "saved as ID: 1") // personal ones are unaffected
	contains(t, e.b.execute(rguild("m1", manage, "settings", str("active", "maybe"))), "enabled")
	contains(t, e.b.execute(rguild("m1", manage, "settings", str("active", "enabled"))), "Active: enabled")
	contains(t, e.b.execute(rguild("m1", manage, "map", str("map-name", "y"), str("role", "777"))), "saved as ID: 2")
	// With an id, active is that one notification's own switch.
	contains(t, e.b.execute(rguild("m1", manage, "settings", str("id", "1"), str("active", "disabled"))), "disabled")
	ns, _ = e.st.UserNotifications(roleOwner("guild:g1"), "guild:g1")
	if !ns[0].Disabled || ns[1].Disabled {
		t.Fatalf("%+v", ns)
	}
	if g, _ := e.st.GuildSettings("guild:g1"); !g.RoleMentions {
		t.Fatal("an id must not turn the whole server off")
	}
}
