package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sourcewatch.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func online(title, mp string, names ...string) rules.Snapshot {
	return rules.Snapshot{Online: true, Title: title, Map: mp, Players: len(names), MaxPlayers: 64, PlayerNames: names, NamesKnown: true}
}

func count(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigrationCreatesSchemaOnEmptyDatabase(t *testing.T) {
	s, path := open(t)
	for _, table := range []string{"server_state", "players", "sessions", "map_history", "title_history", "status_history",
		"notifications", "conditions", "notification_state", "user_cooldowns", "listeners", "guild_settings", "guild_blacklist",
		"server_players"} {
		if count(t, s, table) != 0 {
			t.Errorf("%s not empty", table)
		}
	}
	// The one server row is the stand-in for "all servers".
	if got := count(t, s, "servers"); got != 1 {
		t.Errorf("servers has %d rows, want only the all-servers row", got)
	}
	var v int
	s.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != len(migrations) {
		t.Fatalf("user_version %d, want %d", v, len(migrations))
	}
	// Reopening applies nothing twice.
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
}

func TestLastStateSurvivesRestart(t *testing.T) {
	s, path := open(t)
	id, _ := s.UpsertServer("a:1")
	if _, err := s.Apply(id, online("Dust", "de_dust2", "alice", "bob"), t0); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	id2, _ := s2.UpsertServer("a:1")
	if id2 != id {
		t.Fatalf("server id changed: %d -> %d", id, id2)
	}
	got, err := s2.LastState(id)
	if err != nil || got == nil {
		t.Fatalf("got %v, %v", got, err)
	}
	if !got.DataAt.Equal(t0) {
		t.Fatalf("data time %v, want %v", got.DataAt, t0)
	}
	got.DataAt = time.Time{} // compared above; the zone differs after a reload
	want := online("Dust", "de_dust2", "alice", "bob")
	want.Source = "a2s"
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v, want %+v", *got, want)
	}
}

func TestUnchangedPollAddsNoHistory(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	st := online("Dust", "de_dust2", "alice", "bob")
	s.Apply(id, st, t0)
	tables := []string{"map_history", "title_history", "status_history", "sessions", "players"}
	before := map[string]int{}
	for _, tb := range tables {
		before[tb] = count(t, s, tb)
	}
	for i := 1; i <= 5; i++ {
		if _, err := s.Apply(id, st, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tb := range tables {
		if got := count(t, s, tb); got != before[tb] {
			t.Errorf("%s grew from %d to %d on unchanged polls", tb, before[tb], got)
		}
	}
}

func TestHistoryRecordsChanges(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	s.Apply(id, online("Dust", "de_dust2", "alice"), t0)
	s.Apply(id, online("[EVENT] Dust", "de_inferno", "alice", "bob"), t0.Add(time.Minute))
	s.Apply(id, online("[EVENT] Dust", "de_inferno", "bob"), t0.Add(2*time.Minute)) // alice leaves

	if got := count(t, s, "map_history"); got != 2 {
		t.Errorf("map_history %d, want 2", got)
	}
	if got := count(t, s, "title_history"); got != 2 {
		t.Errorf("title_history %d, want 2", got)
	}
	if got := count(t, s, "sessions"); got != 2 {
		t.Errorf("sessions %d, want 2 (one per player visit)", got)
	}
	var open int
	var lastSeen int64
	s.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE open = 1").Scan(&open)
	s.db.QueryRow(`SELECT s.last_seen FROM sessions s JOIN players p ON p.id = s.player_id WHERE p.name = 'alice'`).Scan(&lastSeen)
	if open != 1 {
		t.Errorf("open sessions %d, want 1", open)
	}
	if want := t0.Add(time.Minute).UnixMilli(); lastSeen != want {
		t.Errorf("alice last_seen %d, want %d (the last poll that saw her)", lastSeen, want)
	}
	maps, _ := s.Maps(id)
	names, _ := s.PlayerNames(id)
	sort.Strings(maps)
	sort.Strings(names)
	if !reflect.DeepEqual(maps, []string{"de_dust2", "de_inferno"}) || !reflect.DeepEqual(names, []string{"alice", "bob"}) {
		t.Errorf("maps %v names %v", maps, names)
	}
}

func TestRejoinCreatesNewSession(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	s.Apply(id, online("T", "m", "alice"), t0)
	s.Apply(id, online("T", "m"), t0.Add(time.Minute))
	s.Apply(id, online("T", "m", "alice"), t0.Add(2*time.Minute))
	if got := count(t, s, "sessions"); got != 2 {
		t.Fatalf("sessions %d, want 2", got)
	}
	if got := count(t, s, "players"); got != 1 {
		t.Fatalf("players %d, want 1", got)
	}
}

func TestUnknownPlayerListLeavesSessionsAlone(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	s.Apply(id, online("T", "m", "alice"), t0)
	blind := online("T", "m")
	blind.Players, blind.NamesKnown = 1, false
	s.Apply(id, blind, t0.Add(time.Minute))
	var open int
	s.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE open = 1").Scan(&open)
	if open != 1 {
		t.Fatalf("open sessions %d, want 1", open)
	}
}

func TestOfflineClosesSessionsAndKeepsTitle(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	s.Apply(id, online("Dust", "de_dust2", "alice"), t0)
	s.Apply(id, rules.Snapshot{}, t0.Add(time.Minute))
	got, _ := s.LastState(id)
	if got.Online || got.Title != "Dust" || got.Map != "de_dust2" || len(got.PlayerNames) != 0 {
		t.Fatalf("got %+v", got)
	}
	if n := count(t, s, "status_history"); n != 2 {
		t.Fatalf("status_history %d, want 2", n)
	}
	if n := count(t, s, "map_history"); n != 1 {
		t.Fatalf("map_history %d, want 1 (offline is not a map change)", n)
	}
}

func TestAnEventHeldBackByTheUserCooldownFiresAtTheNextPoll(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	onMap, _ := s.AddNotification(notif("u", id, rules.Notification{Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}}}))
	full, _ := s.AddNotification(notif("u", id, rules.Notification{Conditions: []rules.Condition{{Kind: rules.KindPlayerCount, Op: ">=", N: 2}}}))
	s.Process(id, online("T", "m1", "a"), t0)

	// The map change brings players in: both fire in the same poll, and the user gets one message.
	evs, _ := s.Process(id, online("T", "m2", "a", "b"), t0.Add(time.Minute))
	if len(evs) != 1 || evs[0].NotificationID != onMap {
		t.Fatalf("first poll: %+v, want only the map event", evs)
	}
	// The other was held back, not used up: it comes at the next poll, once.
	evs, _ = s.Process(id, online("T", "m2", "a", "b"), t0.Add(2*time.Minute))
	if len(evs) != 1 || evs[0].NotificationID != full {
		t.Fatalf("next poll: %+v, want the player count event", evs)
	}
	if evs, _ := s.Process(id, online("T", "m2", "a", "b"), t0.Add(3*time.Minute)); len(evs) != 0 {
		t.Fatalf("fired again: %+v", evs)
	}
}

func TestAnAllServersEventHeldBackOnOneServerFiresAtItsNextPoll(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.AddListener("guild:1", "a:1", "x", t0)
	b, _, _ := s.AddListener("guild:1", "b:2", "x", t0)
	allServers(t, s, "u", "guild:1", rules.Condition{Kind: rules.KindMap, Text: "m2"})
	s.Process(a.ID, online("A", "m1"), t0)
	s.Process(b.ID, online("B", "m1"), t0)

	// Both servers change map 2 s apart: the second is held back by the 5 s gap.
	if evs, _ := s.Process(a.ID, online("A", "m2"), t0.Add(time.Minute)); len(evs) != 1 {
		t.Fatalf("server A: %+v", evs)
	}
	if evs, _ := s.Process(b.ID, online("B", "m2"), t0.Add(time.Minute+2*time.Second)); len(evs) != 0 {
		t.Fatalf("server B inside 5 s: %+v", evs)
	}
	// Server B's next poll delivers it.
	evs, _ := s.Process(b.ID, online("B", "m2"), t0.Add(2*time.Minute+2*time.Second))
	if len(evs) != 1 || evs[0].ServerID != b.ID {
		t.Fatalf("server B next poll: %+v", evs)
	}
}

func TestNotificationsStartOverWhenSourcewatchIsTurnedBackOn(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.AddListener("guild:1", "a:1", "x", t0)
	m2 := rules.Condition{Kind: rules.KindMap, Text: "m2"}
	s.AddNotification(rules.Notification{UserID: "u1", Scope: "guild:1", ServerID: a.ID, ChannelID: "c", Conditions: []rules.Condition{m2}})
	allServers(t, s, "u2", "guild:1", m2)
	s.Process(a.ID, online("A", "m1"), t0) // both armed

	off, on := false, true
	s.UpdateGuildSettings("guild:1", GuildPatch{Active: &off})
	s.Process(a.ID, online("A", "m2"), t0.Add(time.Minute)) // skipped while off
	s.UpdateGuildSettings("guild:1", GuildPatch{Active: &on})
	if evs, _ := s.Process(a.ID, online("A", "m2"), t0.Add(2*time.Minute)); len(evs) != 0 {
		t.Fatalf("fired late for what became true while off: %+v", evs)
	}
	s.Process(a.ID, online("A", "m1"), t0.Add(3*time.Minute))
	if evs, _ := s.Process(a.ID, online("A", "m2"), t0.Add(4*time.Minute)); len(evs) != 2 {
		t.Fatalf("after the condition stopped and held again: %+v, want both", evs)
	}
	// Turning it on when it already is changes nothing.
	s.Process(a.ID, online("A", "m1"), t0.Add(5*time.Hour))
	s.UpdateGuildSettings("guild:1", GuildPatch{Active: &on})
	if evs, _ := s.Process(a.ID, online("A", "m2"), t0.Add(6*time.Hour)); len(evs) != 2 {
		t.Fatalf("armed notifications were reset: %+v", evs)
	}
}

func TestAnUndeliverableNotificationStartsOverWhenSwitchedBackOn(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	n, _ := s.AddNotification(notif("u", id, rules.Notification{Delivery: rules.DeliveryDM,
		Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}}}))
	s.Process(id, online("T", "m1"), t0) // armed
	s.MarkUndeliverable(n)
	s.Process(id, online("T", "m2"), t0.Add(time.Minute)) // skipped while undeliverable
	if ns, _ := s.TakeUndeliverable("u"); len(ns) != 1 {
		t.Fatalf("taken %+v", ns)
	}
	if evs, _ := s.Process(id, online("T", "m2"), t0.Add(2*time.Minute)); len(evs) != 0 {
		t.Fatalf("fired late for what became true while stopped: %+v", evs)
	}
	s.Process(id, online("T", "m1"), t0.Add(3*time.Minute))
	if evs, _ := s.Process(id, online("T", "m2"), t0.Add(4*time.Minute)); len(evs) != 1 {
		t.Fatalf("after the condition stopped and held again: %+v", evs)
	}
}

func notif(user string, server int64, n rules.Notification) rules.Notification {
	n.UserID, n.ServerID, n.ChannelID = user, server, "chan-1"
	return n
}

// Exit check for Phase C: a sequence of states gives exactly the expected events.
func TestProcessSequenceGivesExpectedEvents(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	cond := func(c rules.Condition) rules.Notification {
		return rules.Notification{Conditions: []rules.Condition{c}}
	}
	for _, n := range []rules.Notification{
		notif("u-map", id, cond(rules.Condition{Kind: rules.KindMap, Text: "de_inferno"})),
		notif("u-player", id, cond(rules.Condition{Kind: rules.KindPlayer, Text: "bob"})),
		notif("u-count", id, cond(rules.Condition{Kind: rules.KindPlayerCount, Op: ">=", N: 3})),
		notif("u-status", id, cond(rules.Condition{Kind: rules.KindStatus, Online: false})),
		notif("u-title", id, cond(rules.Condition{Kind: rules.KindTitle, Text: "event"})),
	} {
		if _, err := s.AddNotification(n); err != nil {
			t.Fatal(err)
		}
	}
	type step struct {
		cur  rules.Snapshot
		want []string // users that get an event, sorted
	}
	steps := []step{
		{online("Dust", "de_dust2", "alice"), nil},
		{online("Dust", "de_dust2", "alice"), nil},                                                // no change
		{online("[EVENT] Dust", "de_inferno", "alice", "bob"), u("u-map", "u-player", "u-title")}, // three changes at once
		{online("[EVENT] Dust", "de_inferno", "alice", "bob", "carl"), u("u-count")},              // reaches 3
		{online("[EVENT] Dust", "de_inferno", "alice", "bob", "carl", "dan"), nil},                // stays true
		{rules.Snapshot{}, u("u-status")},                                                         // offline
	}
	for i, st := range steps {
		evs, err := s.Process(id, st.cur, t0.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range evs {
			got = append(got, e.UserID)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, st.want) {
			t.Fatalf("step %d: users %v, want %v", i, got, st.want)
		}
	}
}

func u(users ...string) []string { return users }

func TestPlayerCountStateSurvivesRestart(t *testing.T) {
	s, path := open(t)
	id, _ := s.UpsertServer("a:1")
	s.AddNotification(notif("u", id, rules.Notification{Conditions: []rules.Condition{{Kind: rules.KindPlayerCount, Op: ">=", N: 2}}}))
	s.Process(id, online("T", "m", "a"), t0) // below the number: armed
	evs, _ := s.Process(id, online("T", "m", "a", "b"), t0.Add(time.Minute))
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	s.Close()
	s2, _ := Open(path)
	defer s2.Close()
	// Still true after a restart: must not fire again.
	evs, _ = s2.Process(id, online("T", "m", "a", "b"), t0.Add(2*time.Minute))
	if len(evs) != 0 {
		t.Fatalf("fired again after restart: %v", evs)
	}
}

func TestUserCooldownFiveSeconds(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	// Two notifications for the same user fire on the same poll; a third user is unaffected.
	// A 1 second cooldown, so this tests the 5 second gap between messages to one user, not the notification cooldown.
	s.AddNotification(notif("same", id, rules.Notification{CooldownSec: 1, Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}}}))
	s.AddNotification(notif("same", id, rules.Notification{CooldownSec: 1, Conditions: []rules.Condition{{Kind: rules.KindTitle, Text: "event"}}}))
	s.AddNotification(notif("other", id, rules.Notification{CooldownSec: 1, Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}}}))
	s.Process(id, online("T", "m1"), t0)

	evs, _ := s.Process(id, online("[event] T", "m2"), t0.Add(time.Second))
	got := map[string]int{}
	for _, e := range evs {
		got[e.UserID]++
	}
	if got["same"] != 1 || got["other"] != 1 {
		t.Fatalf("inside 5 s: %v, want one event each for same and other", got)
	}

	// 5 s later the same user is messaged again.
	s.Process(id, online("T", "m1"), t0.Add(3*time.Second))
	evs, _ = s.Process(id, online("T", "m2"), t0.Add(7*time.Second))
	delivered := false
	for _, e := range evs {
		delivered = delivered || e.UserID == "same"
	}
	if !delivered {
		t.Fatal("event after the 5 s cooldown should be delivered")
	}
}

func TestAddNotificationValidates(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	if _, err := s.AddNotification(notif("u", id, rules.Notification{Conditions: []rules.Condition{{Kind: rules.KindPlayerCount, Op: "=>"}}})); err == nil {
		t.Fatal("want error for a bad operator")
	}
}

func TestEventCarriesDeliveryFromStore(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	s.AddNotification(rules.Notification{UserID: "ch", ServerID: id, ChannelID: "c-7", Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}}})
	s.AddNotification(rules.Notification{UserID: "dm", ServerID: id, Delivery: rules.DeliveryDM, Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}}})
	s.Process(id, online("T", "m1"), t0)
	evs, err := s.Process(id, online("T", "m2"), t0.Add(time.Minute))
	if err != nil || len(evs) != 2 {
		t.Fatalf("%v, %v", evs, err)
	}
	for _, e := range evs {
		switch e.UserID {
		case "ch":
			if e.Delivery != rules.DeliveryChannel || e.ChannelID != "c-7" {
				t.Errorf("channel event: %+v", e)
			}
		case "dm":
			if e.Delivery != rules.DeliveryDM {
				t.Errorf("dm event: %+v", e)
			}
		}
	}
}

func TestUndeliverableSkippedThenTaken(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	nid, _ := s.AddNotification(rules.Notification{UserID: "u", ServerID: id, Delivery: rules.DeliveryDM, Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}}})
	s.Process(id, online("T", "m1"), t0)
	if err := s.MarkUndeliverable(nid); err != nil {
		t.Fatal(err)
	}
	evs, _ := s.Process(id, online("T", "m2"), t0.Add(time.Minute))
	if len(evs) != 0 {
		t.Fatalf("undeliverable notification fired: %v", evs)
	}

	got, err := s.TakeUndeliverable("u")
	if err != nil || len(got) != 1 || got[0].ID != nid || !got[0].Undeliverable {
		t.Fatalf("got %+v, %v", got, err)
	}
	if again, _ := s.TakeUndeliverable("u"); len(again) != 0 {
		t.Fatalf("second take returned %v", again)
	}
	// Delivery is re-enabled after the user was told.
	s.Process(id, online("T", "m1"), t0.Add(2*time.Minute))
	evs, _ = s.Process(id, online("T", "m2"), t0.Add(3*time.Minute))
	if len(evs) != 1 {
		t.Fatalf("re-enabled notification did not fire: %v", evs)
	}
}

func TestMigrationAddsDeliveryColumns(t *testing.T) {
	s, _ := open(t)
	if _, err := s.db.Exec("SELECT delivery, channel_id, undeliverable FROM notifications"); err != nil {
		t.Fatal(err)
	}
}

func TestListenersAddRemoveAndScopes(t *testing.T) {
	s, _ := open(t)
	ref, added, err := s.AddListener("guild:1", "a:1", "admin", t0)
	if err != nil || !added {
		t.Fatalf("%v %v", added, err)
	}
	if _, added, _ := s.AddListener("guild:1", "a:1", "admin", t0); added {
		t.Fatal("a second add of the same server must report added=false")
	}
	s.AddListener("guild:1", "b:2", "admin", t0.Add(time.Second))
	s.AddListener("user:9", "a:1", "9", t0)

	got, _ := s.ListenerServers("guild:1")
	if len(got) != 2 || got[0].Addr != "a:1" || got[1].Addr != "b:2" {
		t.Fatalf("guild servers %v", got)
	}
	if got, _ := s.ListenerServers("user:9"); len(got) != 1 {
		t.Fatalf("user servers %v", got)
	}
	// One address per listener row, so the poller's reference count matches.
	if addrs, _ := s.AllListenerAddrs(); len(addrs) != 3 {
		t.Fatalf("listener addrs %v", addrs)
	}

	s.AddNotification(rules.Notification{UserID: "u", Scope: "guild:1", ServerID: ref.ID, ChannelID: "c", Conditions: []rules.Condition{{Kind: rules.KindStatus}}})
	s.AddNotification(rules.Notification{UserID: "u", Scope: "user:9", ServerID: ref.ID, Delivery: rules.DeliveryDM, Conditions: []rules.Condition{{Kind: rules.KindStatus}}})
	removed, err := s.RemoveListener("guild:1", "a:1")
	if err != nil || !removed {
		t.Fatalf("%v %v", removed, err)
	}
	if again, _ := s.RemoveListener("guild:1", "a:1"); again {
		t.Fatal("removing twice must report false")
	}
	if none, _ := s.RemoveListener("guild:1", "unknown:1"); none {
		t.Fatal("unknown server must report false")
	}
	if ns, _ := s.UserNotifications("u", "guild:1"); len(ns) != 0 {
		t.Fatalf("guild notifications should go with the server: %v", ns)
	}
	if ns, _ := s.UserNotifications("u", "user:9"); len(ns) != 1 {
		t.Fatalf("other scope's notifications must stay: %v", ns)
	}
}

func TestNotificationOwnership(t *testing.T) {
	s, _ := open(t)
	ref, _, _ := s.AddListener("guild:1", "a:1", "x", t0)
	id, _ := s.AddNotification(rules.Notification{UserID: "u1", Scope: "guild:1", ServerID: ref.ID, ChannelID: "c", Conditions: []rules.Condition{{Kind: rules.KindStatus}}})
	if n, _ := s.CountUserNotifications("u1"); n != 1 {
		t.Fatalf("count %d", n)
	}
	if ok, _ := s.DeleteNotification(id, "u2", "guild:1"); ok {
		t.Fatal("deleted someone else's notification")
	}
	for _, scope := range []string{"guild:2", "user:u1"} {
		if ok, _ := s.DeleteNotification(id, "u1", scope); ok {
			t.Fatalf("deleted from %s a notification made in guild:1", scope)
		}
	}
	if ok, _ := s.DeleteNotification(id, "u1", "guild:1"); !ok {
		t.Fatal("owner could not delete")
	}
}

func TestServerStateReportsWhenStored(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	if st, _, _ := s.ServerState(id); st != nil {
		t.Fatal("no state yet")
	}
	s.Apply(id, online("T", "m"), t0)
	st, at, err := s.ServerState(id)
	if err != nil || st == nil || !at.Equal(t0) {
		t.Fatalf("%v %v %v", st, at, err)
	}
}

func TestStateRemembersSourceAndDataTime(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	s.Apply(id, online("T", "m"), t0)
	if st, _, _ := s.ServerState(id); st.Source != "a2s" || !st.DataAt.Equal(t0) {
		t.Fatalf("default source %+v", st)
	}
	scan := t0.Add(-7 * time.Minute)
	gt := online("T", "m")
	gt.Source, gt.DataAt = "gametracker", scan
	s.Apply(id, gt, t0.Add(time.Minute))
	st, at, _ := s.ServerState(id)
	if st.Source != "gametracker" || !st.DataAt.Equal(scan) || !at.Equal(t0.Add(time.Minute)) {
		t.Fatalf("got %+v at %v", st, at)
	}
}

// A cooldown on a map notification is kept in the database, so a restart does not reset it.
func TestCooldownOnAnyKindSurvivesRestart(t *testing.T) {
	s, path := open(t)
	id, _ := s.UpsertServer("a:1")
	s.AddNotification(rules.Notification{UserID: "u", ServerID: id, ChannelID: "c", CooldownSec: 600, Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}}})
	s.Process(id, online("T", "m1"), t0)
	if evs, _ := s.Process(id, online("T", "m2"), t0.Add(time.Minute)); len(evs) != 1 {
		t.Fatalf("first change: %v", evs)
	}
	s.Close()

	s2, _ := Open(path)
	defer s2.Close()
	s2.Process(id, online("T", "m1"), t0.Add(2*time.Minute))
	if evs, _ := s2.Process(id, online("T", "m2"), t0.Add(3*time.Minute)); len(evs) != 0 {
		t.Fatalf("inside the cooldown after a restart: %v", evs)
	}
	s2.Process(id, online("T", "m1"), t0.Add(12*time.Minute))
	if evs, _ := s2.Process(id, online("T", "m2"), t0.Add(13*time.Minute)); len(evs) != 1 {
		t.Fatalf("after the cooldown: %v", evs)
	}
}

// --- notifications with several conditions ---

func TestConditionsAreStoredAndLoadedInOrder(t *testing.T) {
	s, path := open(t)
	id, _ := s.UpsertServer("a:1")
	want := []rules.Condition{
		{Kind: rules.KindMap, Text: "de_dust2"},
		{Kind: rules.KindPlayerCount, Op: "<", N: 7},
		{Kind: rules.KindStatus, Online: true},
	}
	nid, err := s.AddNotification(rules.Notification{UserID: "u", Scope: "s", ServerID: id, ChannelID: "c", Conditions: want})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, _ := Open(path)
	defer s2.Close()
	got, _ := s2.UserNotifications("u", "s")
	if len(got) != 1 || got[0].ID != nid || !reflect.DeepEqual(got[0].Conditions, want) || got[0].Phase != rules.PhaseNew {
		t.Fatalf("got %+v", got)
	}
}

func TestAddConditionMakesItNeedBothConditions(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	nid, _ := s.AddNotification(rules.Notification{UserID: "u", ServerID: id, ChannelID: "c",
		Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}}})
	s.Process(id, online("T", "m1"), t0) // armed

	n, err := s.AddCondition(nid, "u", rules.Condition{Kind: rules.KindPlayerCount, Op: ">=", N: 3})
	if err != nil || len(n.Conditions) != 2 || n.Phase != rules.PhaseNew {
		t.Fatalf("%+v %v", n, err)
	}
	s.Process(id, online("T", "m1"), t0.Add(time.Minute)) // neither: armed again
	if evs, _ := s.Process(id, online("T", "m2", "a"), t0.Add(2*time.Minute)); len(evs) != 0 {
		t.Fatalf("the map alone must not fire it any more: %v", evs)
	}
	evs, _ := s.Process(id, online("T", "m2", "a", "b", "c"), t0.Add(3*time.Minute))
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "the map is `m2`") || !strings.Contains(evs[0].Message, "player count is >= 3") {
		t.Fatalf("both conditions became true together: %v", evs)
	}
	if evs, _ := s.Process(id, online("T", "m2", "a", "b", "c", "d"), t0.Add(4*time.Minute)); len(evs) != 0 {
		t.Fatalf("it fired again while both stayed true: %v", evs)
	}
}

// Adding a condition starts the notification over: if everything already holds, it waits for
// it to stop holding and become true again, instead of firing at once.
func TestAddConditionWhileEverythingHoldsDoesNotFire(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	nid, _ := s.AddNotification(rules.Notification{UserID: "u", ServerID: id, ChannelID: "c",
		Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}}})
	s.Process(id, online("T", "m1"), t0)
	if evs, _ := s.Process(id, online("T", "m2", "a", "b"), t0.Add(time.Minute)); len(evs) != 1 {
		t.Fatalf("setup: %v", evs)
	}
	s.AddCondition(nid, "u", rules.Condition{Kind: rules.KindPlayerCount, Op: ">=", N: 2}) // both already hold
	if evs, _ := s.Process(id, online("T", "m2", "a", "b"), t0.Add(2*time.Hour)); len(evs) != 0 {
		t.Fatalf("fired for conditions that already held: %v", evs)
	}
	s.Process(id, online("T", "m1", "a", "b"), t0.Add(3*time.Hour))
	if evs, _ := s.Process(id, online("T", "m2", "a", "b"), t0.Add(4*time.Hour)); len(evs) != 1 {
		t.Fatalf("after they stopped holding and became true again: %v", evs)
	}
}

func TestAddConditionChecks(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	nid, _ := s.AddNotification(rules.Notification{UserID: "u", ServerID: id, ChannelID: "c",
		Conditions: []rules.Condition{{Kind: rules.KindStatus}}})
	if _, err := s.AddCondition(nid, "someone-else", rules.Condition{Kind: rules.KindMap, Text: "m"}); !errors.Is(err, ErrNoNotification) {
		t.Errorf("another user's notification: %v", err)
	}
	if _, err := s.AddCondition(9999, "u", rules.Condition{Kind: rules.KindMap, Text: "m"}); !errors.Is(err, ErrNoNotification) {
		t.Errorf("missing notification: %v", err)
	}
	if _, err := s.AddCondition(nid, "u", rules.Condition{Kind: rules.KindMap}); err == nil {
		t.Error("an empty condition should be refused")
	}
	for i := 0; i < rules.MaxConditions-1; i++ {
		if _, err := s.AddCondition(nid, "u", rules.Condition{Kind: rules.KindMap, Text: fmt.Sprint("m", i)}); err != nil {
			t.Fatalf("condition %d: %v", i, err)
		}
	}
	if _, err := s.AddCondition(nid, "u", rules.Condition{Kind: rules.KindMap, Text: "one too many"}); err == nil {
		t.Error("more than the most conditions should be refused")
	}
}

func TestDeletingANotificationDeletesItsConditions(t *testing.T) {
	s, _ := open(t)
	id, _ := s.UpsertServer("a:1")
	nid, _ := s.AddNotification(rules.Notification{UserID: "u", ServerID: id, ChannelID: "c",
		Conditions: []rules.Condition{{Kind: rules.KindStatus}, {Kind: rules.KindMap, Text: "m"}}})
	if count(t, s, "conditions") != 2 {
		t.Fatal("setup")
	}
	s.DeleteNotification(nid, "u", "")
	if n := count(t, s, "conditions"); n != 0 {
		t.Fatalf("%d conditions left behind", n)
	}
}

// Notifications from before conditions had a table of their own keep working.
func TestMigrationKeepsExistingNotifications(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const beforeConditions = 4 // migrations 1-4 are the layout from before the conditions table
	for i := 0; i < beforeConditions; i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	db.Exec(fmt.Sprintf("PRAGMA user_version = %d", beforeConditions))
	db.Exec("INSERT INTO servers(addr) VALUES ('a:1')")
	db.Exec(`INSERT INTO notifications(user_id, scope, server_id, kind, text, op, n, online, cooldown_sec, armed, last_fired, delivery, channel_id)
		VALUES ('u','guild:1',1,'playercount','','>=',5,0,600,0,0,'channel','c'),
		       ('u','guild:1',1,'map','de_dust2','',0,0,0,1,0,'dm','')`)
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ns, err := s.UserNotifications("u", "guild:1")
	if err != nil || len(ns) != 2 {
		t.Fatalf("%v %v", ns, err)
	}
	if c := ns[0].Conditions; len(c) != 1 || c[0].Kind != rules.KindPlayerCount || c[0].Op != ">=" || c[0].N != 5 {
		t.Errorf("first condition %+v", c)
	}
	if ns[0].CooldownSec != 600 || ns[0].Phase != rules.PhaseSpent {
		t.Errorf("a playercount that was not armed stays waiting: %+v", ns[0])
	}
	if c := ns[1].Conditions; len(c) != 1 || c[0].Kind != rules.KindMap || c[0].Text != "de_dust2" {
		t.Errorf("second condition %+v", c)
	}
	if ns[1].Phase != rules.PhaseArmed || ns[1].Delivery != "dm" {
		t.Errorf("an armed one stays armed: %+v", ns[1])
	}
}

func TestPlayerNamesKeepsOneRowPerPlayerAndServer(t *testing.T) {
	s, _ := open(t)
	a, _ := s.UpsertServer("a:1")
	b, _ := s.UpsertServer("b:1")
	s.Process(a, online("A", "m", "alice", "bob"), t0)
	s.Process(a, online("A", "m", "bob"), t0.Add(time.Minute))
	s.Process(a, online("A", "m", "alice", "bob"), t0.Add(2*time.Minute)) // alice comes back: a new session
	s.Process(b, online("B", "m", "alice"), t0)
	if got := count(t, s, "sessions"); got != 4 {
		t.Fatalf("sessions %d, want 4", got)
	}
	if got := count(t, s, "server_players"); got != 3 {
		t.Errorf("server_players %d, want 3: a returning player adds no row", got)
	}
	for id, want := range map[int64][]string{a: {"alice", "bob"}, b: {"alice"}} {
		names, err := s.PlayerNames(id)
		sort.Strings(names)
		if err != nil || !reflect.DeepEqual(names, want) {
			t.Errorf("server %d: %v %v, want %v", id, names, err, want)
		}
	}
}

func TestMigrationFillsServerPlayersFromSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const before = 8 // migrations 1-8: everything up to the server_players table
	for i := 0; i < before; i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	db.Exec(fmt.Sprintf("PRAGMA user_version = %d", before))
	db.Exec("INSERT INTO servers(id, addr) VALUES (1, 'a:1'), (2, 'b:1')")
	db.Exec("INSERT INTO players(id, name) VALUES (1, 'alice'), (2, 'bob')")
	db.Exec(`INSERT INTO sessions(server_id, player_id, first_seen, last_seen, open)
		VALUES (1,1,0,0,0), (1,1,1,1,0), (1,2,0,0,1), (2,2,0,0,0)`)
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := count(t, s, "server_players"); got != 3 {
		t.Errorf("server_players %d, want 3", got)
	}
	names, _ := s.PlayerNames(1)
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"alice", "bob"}) {
		t.Errorf("names on server 1: %v", names)
	}
}

func TestDeleteAllNotificationsOnlyTouchesOneUserAndScope(t *testing.T) {
	s, _ := open(t)
	ref, _, _ := s.AddListener("guild:1", "a:1", "x", t0)
	add := func(user, scope string, conds ...rules.Condition) {
		if _, err := s.AddNotification(rules.Notification{UserID: user, Scope: scope, ServerID: ref.ID, ChannelID: "c", Conditions: conds}); err != nil {
			t.Fatal(err)
		}
	}
	add("u1", "guild:1", rules.Condition{Kind: rules.KindStatus}, rules.Condition{Kind: rules.KindMap, Text: "m"})
	add("u1", "guild:1", rules.Condition{Kind: rules.KindMap, Text: "m2"})
	add("u1", "guild:2", rules.Condition{Kind: rules.KindMap, Text: "m3"})
	add("u2", "guild:1", rules.Condition{Kind: rules.KindMap, Text: "m4"})

	n, err := s.DeleteAllNotifications("u1", "guild:1")
	if err != nil || n != 2 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	if got, _ := s.UserNotifications("u1", "guild:1"); len(got) != 0 {
		t.Errorf("left %v", got)
	}
	if got, _ := s.UserNotifications("u1", "guild:2"); len(got) != 1 {
		t.Errorf("another scope was touched: %v", got)
	}
	if got, _ := s.UserNotifications("u2", "guild:1"); len(got) != 1 {
		t.Errorf("another user was touched: %v", got)
	}
	if c := count(t, s, "conditions"); c != 2 {
		t.Errorf("%d conditions left, want the 2 that belong to the notifications that stayed", c)
	}
	if n, _ := s.DeleteAllNotifications("u1", "guild:1"); n != 0 {
		t.Errorf("a second reset deleted %d", n)
	}
}

// --- settings ---

func newMapNotification(t *testing.T, s *Store, user, scope string, serverID int64) int64 {
	t.Helper()
	id, err := s.AddNotification(rules.Notification{UserID: user, Scope: scope, ServerID: serverID, ChannelID: "c1",
		Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ptr[T any](v T) *T { return &v }

func TestUpdateSettingsChangesOnlyWhatIsGiven(t *testing.T) {
	s, path := open(t)
	sid, _ := s.UpsertServer("a:1")
	id := newMapNotification(t, s, "u", "guild:1", sid)

	n, err := s.UpdateSettings(id, "u", "guild:1", Settings{CooldownSec: ptr(600)})
	if err != nil || n.CooldownSec != 600 || n.Delivery != rules.DeliveryChannel || n.ChannelID != "c1" || n.Disabled {
		t.Fatalf("cooldown only: %+v %v", n, err)
	}
	n, err = s.UpdateSettings(id, "u", "guild:1", Settings{Delivery: ptr(rules.DeliveryDM)})
	if err != nil || n.Delivery != rules.DeliveryDM || n.CooldownSec != 600 {
		t.Fatalf("delivery only: %+v %v", n, err)
	}
	n, err = s.UpdateSettings(id, "u", "guild:1", Settings{Delivery: ptr(rules.DeliveryChannel), ChannelID: ptr("c9"), Disabled: ptr(true)})
	if err != nil || n.ChannelID != "c9" || !n.Disabled || n.EffectiveDelivery() != rules.DeliveryChannel {
		t.Fatalf("channel and disable: %+v %v", n, err)
	}
	// It is all kept after a restart.
	s.Close()
	s2, _ := Open(path)
	defer s2.Close()
	got, _ := s2.UserNotifications("u", "guild:1")
	if len(got) != 1 || got[0].CooldownSec != 600 || got[0].ChannelID != "c9" || !got[0].Disabled || got[0].EffectiveDelivery() != rules.DeliveryChannel {
		t.Fatalf("after restart %+v", got)
	}
}

func TestUpdateSettingsChecks(t *testing.T) {
	s, _ := open(t)
	sid, _ := s.UpsertServer("a:1")
	id := newMapNotification(t, s, "u", "guild:1", sid)
	set := Settings{CooldownSec: ptr(60)}
	if _, err := s.UpdateSettings(id, "other", "guild:1", set); !errors.Is(err, ErrNoNotification) {
		t.Errorf("another user: %v", err)
	}
	if _, err := s.UpdateSettings(id, "u", "guild:2", set); !errors.Is(err, ErrNoNotification) {
		t.Errorf("another place: %v", err)
	}
	if _, err := s.UpdateSettings(9999, "u", "guild:1", set); !errors.Is(err, ErrNoNotification) {
		t.Errorf("missing: %v", err)
	}
	if _, err := s.UpdateSettings(id, "u", "guild:1", Settings{CooldownSec: ptr(-5)}); err == nil {
		t.Error("a negative cooldown should be refused")
	}
	if _, err := s.UpdateSettings(id, "u", "guild:1", Settings{Delivery: ptr("pigeon")}); err == nil {
		t.Error("an unknown delivery should be refused")
	}
	if _, err := s.UpdateSettings(id, "u", "guild:1", Settings{Delivery: ptr(rules.DeliveryChannel), ChannelID: ptr("")}); err == nil {
		t.Error("channel delivery without a channel should be refused")
	}
	// A refused change leaves everything as it was.
	got, _ := s.UserNotifications("u", "guild:1")
	if got[0].CooldownSec != 0 || got[0].ChannelID != "c1" || got[0].Delivery != rules.DeliveryChannel {
		t.Fatalf("changed by a refused update: %+v", got[0])
	}
}

func TestADisabledNotificationNeverFiresAndRestartsWhenEnabled(t *testing.T) {
	s, _ := open(t)
	sid, _ := s.UpsertServer("a:1")
	id := newMapNotification(t, s, "u", "guild:1", sid)
	s.Process(sid, online("T", "m1"), t0) // armed

	if _, err := s.UpdateSettings(id, "u", "guild:1", Settings{Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if evs, _ := s.Process(sid, online("T", "m2"), t0.Add(time.Minute)); len(evs) != 0 {
		t.Fatalf("a disabled notification fired: %v", evs)
	}
	s.Process(sid, online("T", "m1"), t0.Add(2*time.Minute))
	// Enabled while the condition does not hold: it fires when the condition becomes true.
	if _, err := s.UpdateSettings(id, "u", "guild:1", Settings{Disabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if evs, _ := s.Process(sid, online("T", "m1"), t0.Add(3*time.Minute)); len(evs) != 0 {
		t.Fatalf("fired for a condition that does not hold: %v", evs)
	}
	if evs, _ := s.Process(sid, online("T", "m2"), t0.Add(4*time.Minute)); len(evs) != 1 {
		t.Fatalf("enabled and the condition became true: %v", evs)
	}

	// Enabled while the condition already holds: it waits for it to stop and become true again.
	s.UpdateSettings(id, "u", "guild:1", Settings{Disabled: ptr(true)})
	s.UpdateSettings(id, "u", "guild:1", Settings{Disabled: ptr(false)})
	if evs, _ := s.Process(sid, online("T", "m2"), t0.Add(2*time.Hour)); len(evs) != 0 {
		t.Fatalf("fired at once for a condition that already held: %v", evs)
	}
	s.Process(sid, online("T", "m1"), t0.Add(3*time.Hour))
	if evs, _ := s.Process(sid, online("T", "m2"), t0.Add(4*time.Hour)); len(evs) != 1 {
		t.Fatalf("after it stopped holding and held again: %v", evs)
	}
}

func TestChangingDeliveryLiftsAStopFromARefusedDM(t *testing.T) {
	s, _ := open(t)
	sid, _ := s.UpsertServer("a:1")
	id, _ := s.AddNotification(rules.Notification{UserID: "u", Scope: "guild:1", ServerID: sid, Delivery: rules.DeliveryDM,
		Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}}})
	s.Process(sid, online("T", "m1"), t0)
	s.MarkUndeliverable(id)
	if _, err := s.UpdateSettings(id, "u", "guild:1", Settings{Delivery: ptr(rules.DeliveryChannel), ChannelID: ptr("c1")}); err != nil {
		t.Fatal(err)
	}
	// It is no longer stopped, so there is nothing to tell the user about.
	if got, _ := s.TakeUndeliverable("u"); len(got) != 0 {
		t.Fatalf("still undeliverable: %v", got)
	}
	if evs, _ := s.Process(sid, online("T", "m2"), t0.Add(time.Minute)); len(evs) != 1 || evs[0].Delivery != rules.DeliveryChannel || evs[0].ChannelID != "c1" {
		t.Fatalf("events %+v", evs)
	}
}

func TestMigrationAddsTheDisabledColumn(t *testing.T) {
	s, _ := open(t)
	if _, err := s.db.Exec("SELECT disabled FROM notifications"); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateAllSettingsTouchesOnlyOneUsersNotificationsInOnePlace(t *testing.T) {
	s, _ := open(t)
	sid, _ := s.UpsertServer("a:1")
	a1 := newMapNotification(t, s, "u", "guild:1", sid)
	a2 := newMapNotification(t, s, "u", "guild:1", sid)
	other := newMapNotification(t, s, "u", "guild:2", sid)
	theirs := newMapNotification(t, s, "v", "guild:1", sid)
	s.UpdateSettings(a2, "u", "guild:1", Settings{Disabled: ptr(true)})

	ns, err := s.UpdateAllSettings("u", "guild:1", Settings{CooldownSec: ptr(300), Disabled: ptr(false)})
	if err != nil || len(ns) != 2 {
		t.Fatalf("%v %v", ns, err)
	}
	for _, id := range []int64{a1, a2} {
		got, _ := s.UserNotifications("u", "guild:1")
		for _, n := range got {
			if n.ID == id && (n.CooldownSec != 300 || n.Disabled) {
				t.Errorf("not changed: %+v", n)
			}
		}
	}
	if got, _ := s.UserNotifications("u", "guild:2"); got[0].ID != other || got[0].CooldownSec != 0 {
		t.Errorf("another place changed: %+v", got)
	}
	if got, _ := s.UserNotifications("v", "guild:1"); got[0].ID != theirs || got[0].CooldownSec != 0 {
		t.Errorf("another user changed: %+v", got)
	}
	// The one that was disabled starts over when it is turned on again, the other keeps its phase.
	got, _ := s.UserNotifications("u", "guild:1")
	if got[0].Phase != rules.PhaseNew && got[1].Phase != rules.PhaseNew {
		t.Errorf("re-enabled notification did not start over: %+v", got)
	}
}

func TestUpdateAllSettingsChangesNothingIfOneWouldBeInvalid(t *testing.T) {
	s, _ := open(t)
	sid, _ := s.UpsertServer("a:1")
	newMapNotification(t, s, "u", "guild:1", sid)
	newMapNotification(t, s, "u", "guild:1", sid)
	if _, err := s.UpdateAllSettings("u", "guild:1", Settings{CooldownSec: ptr(60), Delivery: ptr(rules.DeliveryChannel), ChannelID: ptr("")}); err == nil {
		t.Fatal("a channel delivery without a channel must be refused")
	}
	for _, n := range func() []rules.Notification { x, _ := s.UserNotifications("u", "guild:1"); return x }() {
		if n.CooldownSec != 0 || n.ChannelID != "c1" {
			t.Fatalf("changed by a refused update: %+v", n)
		}
	}
	if ns, err := s.UpdateAllSettings("nobody", "guild:1", Settings{CooldownSec: ptr(60)}); err != nil || len(ns) != 0 {
		t.Errorf("a user with no notifications: %v %v", ns, err)
	}
}

// --- notifications for all servers ---

func allServers(t *testing.T, s *Store, user, scope string, conds ...rules.Condition) int64 {
	t.Helper()
	id, err := s.AddNotification(rules.Notification{UserID: user, Scope: scope, ServerID: rules.AllServers, ChannelID: "c1", Conditions: conds})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAnAllServersNotificationKeepsItsStateForEachServer(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.AddListener("guild:1", "a:1", "x", t0)
	b, _, _ := s.AddListener("guild:1", "b:2", "x", t0)
	allServers(t, s, "u", "guild:1", rules.Condition{Kind: rules.KindMap, Text: "m2"})

	s.Process(a.ID, online("A", "m1"), t0)
	s.Process(b.ID, online("B", "m1"), t0)
	evs, _ := s.Process(a.ID, online("A", "m2"), t0.Add(time.Minute))
	if len(evs) != 1 || evs[0].ServerID != a.ID || !strings.Contains(evs[0].Message, "`A`") {
		t.Fatalf("server A: %+v", evs)
	}
	// Server A firing does not put server B in its cooldown, nor mark it as already true.
	evs, _ = s.Process(b.ID, online("B", "m2"), t0.Add(2*time.Minute))
	if len(evs) != 1 || evs[0].ServerID != b.ID || !strings.Contains(evs[0].Message, "`B`") {
		t.Fatalf("server B: %+v", evs)
	}
	// Each server keeps its own cooldown and phase.
	s.Process(a.ID, online("A", "m1"), t0.Add(3*time.Minute))
	if evs, _ := s.Process(a.ID, online("A", "m2"), t0.Add(4*time.Minute)); len(evs) != 0 {
		t.Fatalf("A is inside its own cooldown: %+v", evs)
	}
	if evs, _ := s.Process(a.ID, online("A", "m2"), t0.Add(4*time.Hour)); len(evs) != 0 {
		t.Fatalf("A stays true, so it does not fire again: %+v", evs)
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM notification_state").Scan(&n)
	if n != 2 {
		t.Errorf("%d state rows, want one for each server", n)
	}
}

func TestAnAllServersNotificationCoversServersAddedLaterButNotOtherPlaces(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.AddListener("guild:1", "a:1", "x", t0)
	other, _, _ := s.AddListener("guild:2", "z:9", "x", t0)
	id := allServers(t, s, "u", "guild:1", rules.Condition{Kind: rules.KindStatus, Online: false})

	// Another place's server never concerns it.
	s.Process(other.ID, online("Z", "m"), t0)
	if evs, _ := s.Process(other.ID, rules.Snapshot{}, t0.Add(time.Minute)); len(evs) != 0 {
		t.Fatalf("a server of another Discord server fired it: %+v", evs)
	}
	// A server added later is covered, and starts without firing for what already holds.
	c, _, _ := s.AddListener("guild:1", "c:3", "x", t0.Add(time.Hour))
	s.Process(c.ID, online("C", "m"), t0.Add(time.Hour))
	evs, _ := s.Process(c.ID, rules.Snapshot{}, t0.Add(2*time.Hour))
	if len(evs) != 1 || evs[0].ServerID != c.ID || evs[0].NotificationID != id {
		t.Fatalf("the later server: %+v", evs)
	}
	s.Process(a.ID, online("A", "m"), t0.Add(3*time.Hour))
	if evs, _ := s.Process(a.ID, rules.Snapshot{}, t0.Add(4*time.Hour)); len(evs) != 1 {
		t.Fatalf("the first server: %+v", evs)
	}
	// After the place stops watching a server it no longer applies, and its state is gone.
	s.RemoveListener("guild:1", "c:3")
	if evs, _ := s.Process(c.ID, rules.Snapshot{}, t0.Add(5*time.Hour)); len(evs) != 0 {
		t.Fatalf("a server that is no longer watched: %+v", evs)
	}
	var left int
	s.db.QueryRow("SELECT COUNT(*) FROM notification_state WHERE server_id = ?", c.ID).Scan(&left)
	if left != 0 {
		t.Errorf("%d state rows left for the removed server", left)
	}
	// The notification itself stays.
	if got, _ := s.UserNotifications("u", "guild:1"); len(got) != 1 || got[0].ServerID != rules.AllServers {
		t.Fatalf("%+v", got)
	}
}

func TestAddingAConditionOrEnablingRestartsEveryServer(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.AddListener("guild:1", "a:1", "x", t0)
	id := allServers(t, s, "u", "guild:1", rules.Condition{Kind: rules.KindMap, Text: "m2"})
	s.Process(a.ID, online("A", "m1"), t0)
	s.Process(a.ID, online("A", "m2"), t0.Add(time.Minute)) // fires
	s.AddCondition(id, "u", rules.Condition{Kind: rules.KindPlayerCount, Op: ">=", N: 1})
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM notification_state").Scan(&n)
	if n != 0 {
		t.Fatalf("state rows after adding a condition: %d", n)
	}
	s.UpdateSettings(id, "u", "guild:1", Settings{Disabled: ptr(true)})
	s.Process(a.ID, online("A", "m2", "x"), t0.Add(2*time.Hour))
	s.UpdateSettings(id, "u", "guild:1", Settings{Disabled: ptr(false)})
	if evs, _ := s.Process(a.ID, online("A", "m2", "x"), t0.Add(3*time.Hour)); len(evs) != 0 {
		t.Fatalf("fired at once for what already held: %+v", evs)
	}
	if ok, _ := s.DeleteNotification(id, "u", "guild:1"); !ok {
		t.Fatal("not deleted")
	}
	s.db.QueryRow("SELECT COUNT(*) FROM notification_state").Scan(&n)
	if n != 0 {
		t.Errorf("state rows left after deleting: %d", n)
	}
}

// --- guild settings ---

func TestGuildSettingsDefaultsAndUpdates(t *testing.T) {
	s, path := open(t)
	g, err := s.GuildSettings("guild:1")
	if err != nil || !g.ChannelDelivery || g.DefaultDelivery != "channel" || g.DefaultCooldownSec != 0 || !g.Active {
		t.Fatalf("defaults %+v %v", g, err)
	}
	g, err = s.UpdateGuildSettings("guild:1", GuildPatch{ChannelDelivery: ptr(false), DefaultCooldownSec: ptr(900)})
	if err != nil || g.ChannelDelivery || g.DefaultCooldownSec != 900 || g.DefaultDelivery != "channel" || !g.Active {
		t.Fatalf("after update %+v %v", g, err)
	}
	if _, err := s.UpdateGuildSettings("guild:1", GuildPatch{DefaultDelivery: ptr("dm"), Active: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateGuildSettings("guild:1", GuildPatch{DefaultDelivery: ptr("pigeon")}); err == nil {
		t.Error("an unknown delivery should be refused")
	}
	if _, err := s.UpdateGuildSettings("guild:1", GuildPatch{DefaultCooldownSec: ptr(-1)}); err == nil {
		t.Error("a negative cooldown should be refused")
	}
	// Kept after a restart, and another Discord server is untouched.
	s.Close()
	s2, _ := Open(path)
	defer s2.Close()
	g, _ = s2.GuildSettings("guild:1")
	if g.ChannelDelivery || g.DefaultDelivery != "dm" || g.DefaultCooldownSec != 900 || g.Active {
		t.Fatalf("after restart %+v", g)
	}
	if o, _ := s2.GuildSettings("guild:2"); !o.Active || !o.ChannelDelivery {
		t.Fatalf("another Discord server changed: %+v", o)
	}
}

func TestBlacklistIsCaseInsensitivePerDiscordServer(t *testing.T) {
	s, _ := open(t)
	if added, _ := s.AddBlacklist("guild:1", "Chujoy", "admin", t0); !added {
		t.Fatal("not added")
	}
	if added, _ := s.AddBlacklist("guild:1", "  chujoy ", "admin", t0); added {
		t.Error("the same name in another case is the same name")
	}
	s.AddBlacklist("guild:1", "alice", "admin", t0)
	if got, _ := s.Blacklist("guild:1"); !reflect.DeepEqual(got, []string{"alice", "Chujoy"}) {
		t.Fatalf("blacklist %v", got)
	}
	if ok, _ := s.IsBlacklisted("guild:1", "CHUJOY"); !ok {
		t.Error("matching ignores case")
	}
	if ok, _ := s.IsBlacklisted("guild:2", "chujoy"); ok {
		t.Error("another Discord server has its own blacklist")
	}
	if removed, _ := s.RemoveBlacklist("guild:1", "CHUJOY"); !removed {
		t.Error("not removed")
	}
	if removed, _ := s.RemoveBlacklist("guild:1", "chujoy"); removed {
		t.Error("removed twice")
	}
}

func TestGuildSettingsShapeWhatProcessDoes(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.AddListener("guild:1", "a:1", "x", t0)
	mk := func(conds ...rules.Condition) {
		s.AddNotification(rules.Notification{UserID: "u", Scope: "guild:1", ServerID: a.ID, ChannelID: "c1", CooldownSec: 1, Conditions: conds})
	}
	mk(rules.Condition{Kind: rules.KindMap, Text: "m2"})
	step := 0
	fire := func(mp string, names ...string) []rules.Event {
		step++
		evs, err := s.Process(a.ID, online("T", mp, names...), t0.Add(time.Duration(step)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
	fire("m1")

	// The bot turned off in this Discord server: nothing fires, and turning it on works again.
	s.UpdateGuildSettings("guild:1", GuildPatch{Active: ptr(false)})
	if evs := fire("m2"); len(evs) != 0 {
		t.Fatalf("fired although sourcewatch is turned off here: %+v", evs)
	}
	s.UpdateGuildSettings("guild:1", GuildPatch{Active: ptr(true)})
	fire("m1")
	if evs := fire("m2"); len(evs) != 1 || evs[0].Delivery != rules.DeliveryChannel {
		t.Fatalf("after turning it on: %+v", evs)
	}

	// Channel delivery off: the event goes by DM instead.
	s.UpdateGuildSettings("guild:1", GuildPatch{ChannelDelivery: ptr(false)})
	fire("m1")
	if evs := fire("m2"); len(evs) != 1 || evs[0].Delivery != rules.DeliveryDM {
		t.Fatalf("with channel delivery off: %+v", evs)
	}
	// Another Discord server's settings do not matter here.
	s.UpdateGuildSettings("guild:other", GuildPatch{Active: ptr(false), ChannelDelivery: ptr(false)})
	s.UpdateGuildSettings("guild:1", GuildPatch{ChannelDelivery: ptr(true)})
	fire("m1")
	if evs := fire("m2"); len(evs) != 1 || evs[0].Delivery != rules.DeliveryChannel {
		t.Fatalf("%+v", evs)
	}
}

func TestABlacklistedPlayerIsNotTracked(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.AddListener("guild:1", "a:1", "x", t0)
	s.AddNotification(rules.Notification{UserID: "u", Scope: "guild:1", ServerID: a.ID, ChannelID: "c1",
		Conditions: []rules.Condition{{Kind: rules.KindPlayer, Text: "Chujoy"}}})
	s.AddNotification(rules.Notification{UserID: "u", Scope: "guild:1", ServerID: a.ID, ChannelID: "c1",
		Conditions: []rules.Condition{{Kind: rules.KindPlayer, Text: "alice"}}})
	step := 0
	proc := func(names ...string) int {
		step++
		evs, _ := s.Process(a.ID, online("T", "m", names...), t0.Add(time.Duration(step)*time.Hour))
		return len(evs)
	}
	proc()
	s.AddBlacklist("guild:1", "chujoy", "admin", t0)
	if got := proc("Chujoy", "alice"); got != 1 {
		t.Fatalf("only alice may be tracked, got %d events", got)
	}
	proc()
	s.RemoveBlacklist("guild:1", "CHUJOY")
	if got := proc("Chujoy"); got != 1 {
		t.Fatalf("after the name left the blacklist: %d events", got)
	}
}

// --- notification IDs ---

func idOf(t *testing.T, s *Store, user string) int64 {
	t.Helper()
	sid, _ := s.UpsertServer("a:1")
	id, err := s.AddNotification(rules.Notification{UserID: user, Scope: "guild:1", ServerID: sid, ChannelID: "c",
		Conditions: []rules.Condition{{Kind: rules.KindStatus}}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestANewNotificationTakesTheLowestFreeID(t *testing.T) {
	s, _ := open(t)
	for want := int64(1); want <= 5; want++ {
		if got := idOf(t, s, "u"); got != want {
			t.Fatalf("new notification got ID %d, want %d", got, want)
		}
	}
	// Free the first one, and the next one uses it even though 2 to 5 are taken.
	if ok, _ := s.DeleteNotification(1, "u", "guild:1"); !ok {
		t.Fatal("not deleted")
	}
	if got := idOf(t, s, "u"); got != 1 {
		t.Fatalf("after deleting 1, the next ID is %d", got)
	}
	// Gaps are filled from the bottom, one at a time.
	s.DeleteNotification(2, "u", "guild:1")
	s.DeleteNotification(4, "u", "guild:1")
	for _, want := range []int64{2, 4, 6, 7} {
		if got := idOf(t, s, "u"); got != want {
			t.Fatalf("got ID %d, want %d", got, want)
		}
	}
}

func TestIDsAreSharedAcrossPeopleButNeverTouchSomeoneElses(t *testing.T) {
	s, _ := open(t)
	a1, a2 := idOf(t, s, "alice"), idOf(t, s, "alice")
	b1 := idOf(t, s, "bob")
	if a1 != 1 || a2 != 2 || b1 != 3 {
		t.Fatalf("%d %d %d", a1, a2, b1)
	}
	// Bob cannot delete or change Alice's, so the free ID can only come from a deletion of its owner.
	if ok, _ := s.DeleteNotification(a1, "bob", "guild:1"); ok {
		t.Fatal("bob deleted alice's notification")
	}
	s.DeleteNotification(a1, "alice", "guild:1")
	if got := idOf(t, s, "bob"); got != 1 {
		t.Fatalf("the freed ID goes to whoever makes the next one: got %d", got)
	}
	if _, err := s.UpdateSettings(1, "alice", "guild:1", Settings{CooldownSec: ptr(60)}); !errors.Is(err, ErrNoNotification) {
		t.Fatalf("alice can no longer change ID 1, it is bob's now: %v", err)
	}
}

func TestResetAndDeleteAllFreeTheirIDs(t *testing.T) {
	s, _ := open(t)
	idOf(t, s, "u")
	idOf(t, s, "u")
	idOf(t, s, "other")
	s.DeleteAllNotifications("u", "guild:1")
	if got := idOf(t, s, "u"); got != 1 {
		t.Fatalf("after a reset the first free ID is 1, got %d", got)
	}
	if got := idOf(t, s, "u"); got != 2 {
		t.Fatalf("got %d, want 2", got)
	}
}

// Anything left under a free ID must not attach to the notification that takes it.
func TestAReusedIDStartsClean(t *testing.T) {
	s, _ := open(t)
	sid, _ := s.UpsertServer("a:1")
	// Leftovers under ID 1, which no notification has: a condition and some per-server state.
	s.db.Exec("PRAGMA foreign_keys = OFF")
	s.db.Exec("INSERT INTO conditions(notification_id, kind, text) VALUES (1, 'map', 'ghost')")
	s.db.Exec("INSERT INTO notification_state(notification_id, server_id, phase, last_fired) VALUES (1, ?, 2, 5)", sid)
	s.db.Exec("PRAGMA foreign_keys = ON")

	id, err := s.AddNotification(rules.Notification{UserID: "u", Scope: "guild:1", ServerID: rules.AllServers, ChannelID: "c",
		Conditions: []rules.Condition{{Kind: rules.KindStatus, Online: false}}})
	if err != nil || id != 1 {
		t.Fatalf("%d %v", id, err)
	}
	got, _ := s.UserNotifications("u", "guild:1")
	if len(got) != 1 || len(got[0].Conditions) != 1 || got[0].Conditions[0].Kind != rules.KindStatus {
		t.Fatalf("a leftover condition attached itself: %+v", got)
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM notification_state").Scan(&n)
	if n != 0 {
		t.Fatalf("leftover state attached itself: %d rows", n)
	}
}

func TestADeletedNotificationLeavesNothingBehind(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.AddListener("guild:1", "a:1", "x", t0)
	id, _ := s.AddNotification(rules.Notification{UserID: "u", Scope: "guild:1", ServerID: rules.AllServers, ChannelID: "c",
		Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}, {Kind: rules.KindPlayerCount, Op: ">=", N: 1}}})
	s.Process(a.ID, online("A", "m1", "x"), t0)
	s.Process(a.ID, online("A", "m2", "x"), t0.Add(time.Minute))
	s.DeleteNotification(id, "u", "guild:1")
	for _, table := range []string{"notifications", "conditions", "notification_state"} {
		if n := count(t, s, table); n != 0 {
			t.Errorf("%s has %d rows left", table, n)
		}
	}
	// The next one is a fresh notification that has to wait for its conditions to become true.
	id2, _ := s.AddNotification(rules.Notification{UserID: "u", Scope: "guild:1", ServerID: rules.AllServers, ChannelID: "c",
		Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}}})
	if id2 != id {
		t.Fatalf("expected the ID to be reused: %d vs %d", id2, id)
	}
	if evs, _ := s.Process(a.ID, online("A", "m2", "x"), t0.Add(2*time.Hour)); len(evs) != 0 {
		t.Fatalf("the reused ID inherited the old state and fired: %+v", evs)
	}
}

func TestGameTrackerIsAllowedUnlessEveryPlaceWatchingTheServerTurnedItOff(t *testing.T) {
	s, _ := open(t)
	allowed := func(addr string) bool {
		ok, err := s.GameTrackerAllowed(addr)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !allowed("nobody:1") {
		t.Error("a server nobody watches (the config's) may use it")
	}
	s.AddListener("guild:1", "a:1", "x", t0)
	if !allowed("a:1") {
		t.Error("allowed by default")
	}
	s.UpdateGuildSettings("guild:1", GuildPatch{GameTracker: ptr(false)})
	if allowed("a:1") {
		t.Error("the only place watching it turned it off")
	}
	s.AddListener("guild:2", "a:1", "y", t0) // never set anything, so it is on
	if !allowed("a:1") {
		t.Error("another place watching it still wants it")
	}
	s.UpdateGuildSettings("guild:2", GuildPatch{GameTracker: ptr(false)})
	s.AddListener("dm:u", "a:1", "u", t0) // a DM has no settings, so it is on
	if !allowed("a:1") {
		t.Error("a DM watching it still wants it")
	}
}

func TestGameTrackerDataDoesNotCountInADiscordServerThatTurnedItOff(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.AddListener("guild:1", "a:1", "x", t0)
	s.AddListener("guild:2", "a:1", "y", t0)
	for _, scope := range []string{"guild:1", "guild:2"} {
		s.AddNotification(rules.Notification{UserID: scope, Scope: scope, ServerID: a.ID, ChannelID: "c", CooldownSec: 1,
			Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}}})
	}
	s.UpdateGuildSettings("guild:1", GuildPatch{GameTracker: ptr(false)})
	viaGT := func(mp string) rules.Snapshot {
		sn := online("T", mp)
		sn.Source = "gametracker"
		return sn
	}
	if _, err := s.Process(a.ID, viaGT("m1"), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	evs, err := s.Process(a.ID, viaGT("m2"), t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].UserID != "guild:2" {
		t.Fatalf("events %+v, want only guild:2's", evs)
	}
	// A direct answer counts everywhere.
	if _, err := s.Process(a.ID, online("T", "m1"), t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	evs, _ = s.Process(a.ID, online("T", "m2"), t0.Add(4*time.Minute))
	if len(evs) != 2 {
		t.Fatalf("events %+v, want both Discord servers' once direct answers are used", evs)
	}
}

func TestNotificationNumbersArePerPersonAndPlace(t *testing.T) {
	s, _ := open(t)
	a, _, _ := s.AddListener("guild:1", "a:1", "x", t0)
	mk := func(user, scope string, num int64) rules.Notification {
		t.Helper()
		n, err := s.AddNotificationAt(rules.Notification{UserID: user, Scope: scope, ServerID: a.ID, Delivery: rules.DeliveryDM,
			Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m"}}}, num)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := mk("u1", "guild:1", 0); n.Num != 1 {
		t.Fatalf("first num %d", n.Num)
	}
	if n := mk("u2", "guild:1", 0); n.Num != 1 {
		t.Fatalf("another person starts at 1 too, got %d", n.Num)
	}
	if n := mk("u1", "dm:u1", 0); n.Num != 1 {
		t.Fatalf("another place starts at 1 too, got %d", n.Num)
	}
	if n := mk("u1", "guild:1", 4); n.Num != 4 {
		t.Fatalf("asked for 4, got %d", n.Num)
	}
	if n := mk("u1", "guild:1", 0); n.Num != 2 {
		t.Fatalf("lowest free is 2, got %d", n.Num)
	}
	if _, err := s.AddNotificationAt(rules.Notification{UserID: "u1", Scope: "guild:1", ServerID: a.ID, Delivery: rules.DeliveryDM,
		Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m"}}}, 4); !errors.Is(err, ErrIDTaken) {
		t.Fatalf("taking u1's 4 twice: %v", err)
	}
}

func TestMigrationNumbersExistingNotificationsPerPerson(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	const before = 10 // migrations 1-10: everything up to the per-person numbers
	for i := 0; i < before; i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	db.Exec(fmt.Sprintf("PRAGMA user_version = %d", before))
	db.Exec("INSERT INTO servers(id, addr) VALUES (1, 'a:1')")
	for _, r := range []struct {
		id          int
		user, scope string
	}{{1, "u1", "g"}, {2, "u1", "g"}, {3, "u2", "g"}, {8, "u1", "g"}, {9, "u1", "dm"}} {
		if _, err := db.Exec(`INSERT INTO notifications(id, user_id, scope, server_id, kind, cooldown_sec, armed, phase, delivery, channel_id)
			VALUES (?, ?, ?, 1, '', 0, 1, 0, 'dm', '')`, r.id, r.user, r.scope); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := map[int64]int64{}
	rows, _ := s.db.Query("SELECT id, num FROM notifications")
	for rows.Next() {
		var id, num int64
		rows.Scan(&id, &num)
		got[id] = num
	}
	rows.Close()
	if want := map[int64]int64{1: 1, 2: 2, 3: 1, 8: 3, 9: 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("numbers %v, want %v", got, want)
	}
}
