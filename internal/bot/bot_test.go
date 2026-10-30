package bot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kritzerenkrieg/sourcewatch/internal/notify"
	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
	"github.com/kritzerenkrieg/sourcewatch/internal/store"
)

type fakeWatcher struct{ added, removed []string }

func (w *fakeWatcher) Watch(a string)   { w.added = append(w.added, a) }
func (w *fakeWatcher) Unwatch(a string) { w.removed = append(w.removed, a) }

type fakeDM struct {
	err    error
	probed []string
}

func (d *fakeDM) ProbeDM(_ context.Context, user string) error {
	d.probed = append(d.probed, user)
	return d.err
}

type env struct {
	b  *Bot
	st *store.Store
	w  *fakeWatcher
	dm *fakeDM
}

func newEnv(t *testing.T) *env {
	t.Helper()
	lookupIP = func(host string) ([]net.IP, error) {
		switch host {
		case "localhost", "intranet.example":
			return []net.IP{net.ParseIP("10.0.0.5")}, nil
		case "v6only.example":
			return []net.IP{net.ParseIP("2606:2800:220:1::1")}, nil
		case "v6loopback.example":
			return []net.IP{net.ParseIP("::1")}, nil
		case "mixed.example":
			return []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("fd00::1")}, nil
		case "cgnat.example":
			return []net.IP{net.ParseIP("100.64.1.1")}, nil
		case "nxdomain.example":
			return nil, errors.New("no such host")
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	t.Cleanup(func() { lookupIP = net.LookupIP })
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, w: &fakeWatcher{}, dm: &fakeDM{}}
	e.b, err = New("test-token", "123", "456", slog.New(slog.NewTextHandler(io.Discard, nil)),
		Deps{Store: st, Watcher: e.w, DM: e.dm})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func str(name, v string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionString, Value: v}
}

func num(name string, v int) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionInteger, Value: float64(v)}
}

// guild builds /<command> run by a member of Discord server g1 in channel c1.
func guild(user string, perms int64, sub string, options ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.Interaction {
	in := dm(user, sub, options...)
	in.User = nil
	in.Member = &discordgo.Member{User: &discordgo.User{ID: user}, Permissions: perms}
	in.GuildID, in.ChannelID = "g1", "c1"
	return in
}

// dm builds /<command> run in a DM with the bot.
func dm(user, command string, options ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.Interaction {
	data := discordgo.ApplicationCommandInteractionData{Name: command, Options: options}
	return &discordgo.Interaction{Type: discordgo.InteractionApplicationCommand, Data: data, User: &discordgo.User{ID: user}, ChannelID: "dmchan"}
}

const manage = discordgo.PermissionManageGuild

func contains(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("reply %q does not contain %q", got, w)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	contains(t, newEnv(t).b.execute(dm("u1", "nope")), "Unknown command", "/help")
}

func TestNormalizeAddr(t *testing.T) {
	newEnv(t)
	for _, tc := range []struct {
		in, want, err string
	}{
		{"Example.COM:27016", "example.com:27016", ""},
		{"example.com", "example.com:27015", ""},
		{"  1.2.3.4:27015  ", "1.2.3.4:27015", ""},
		{"localhost:27015", "", "not a public address"},
		{"intranet.example:27015", "", "not a public address"},
		{"v6only.example:27015", "", "no IPv4 address"},
		{"v6loopback.example:27015", "", "not a public address"},
		{"mixed.example:27015", "", "not a public address"},
		{"cgnat.example:27015", "", "not a public address"},
		{"example.com:027016", "example.com:27016", ""},
		{"host:0", "", "not a valid port"},
		{"host:99999", "", "not a valid port"},
		{"bad host:27015", "", "not a valid host"},
		{"[::1]:27015", "", "not a valid host"},
		{"nxdomain.example:27015", "", "could not find"},
		{"", "", "give an address"},
	} {
		got, err := normalizeAddr(tc.in)
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%q: err %v, want %q", tc.in, err, tc.err)
		case tc.err == "" && tc.want != "" && (err != nil || got != tc.want):
			t.Errorf("%q: got %q, %v, want %q", tc.in, got, err, tc.want)
		}
	}
	// Real IP checks, without the stub.
	lookupIP = net.LookupIP
	for _, in := range []string{"127.0.0.1:27015", "192.168.1.5:27015", "10.0.0.1:27015", "169.254.1.1:27015", "172.16.0.1:27015", "0.0.0.0:27015", "100.64.0.1:27015", "198.18.0.1:27015"} {
		if _, err := normalizeAddr(in); err == nil {
			t.Errorf("%s should be refused", in)
		}
	}
}

func TestServerAddNeedsManageServerInAGuild(t *testing.T) {
	e := newEnv(t)
	got := e.b.execute(guild("u1", 0, "server", subcommand("add", str("address", "game.example:27015"))))
	contains(t, got, "Manage Server")
	if len(e.w.added) != 0 {
		t.Fatal("must not watch without permission")
	}
	got = e.b.execute(guild("u1", manage, "server", subcommand("add", str("address", "game.example:27015"))))
	contains(t, got, "Now watching", "game.example:27015")
	if len(e.w.added) != 1 || e.w.added[0] != "game.example:27015" {
		t.Fatalf("watcher %v", e.w.added)
	}
	contains(t, e.b.execute(guild("u1", manage, "server", subcommand("add", str("address", "game.example:27015")))), "Already watching")
	if len(e.w.added) != 1 {
		t.Fatal("a duplicate must not add a second listener")
	}
}

func TestServerAddInDMNeedsNoPermissionAndAllowsSeveral(t *testing.T) {
	e := newEnv(t)
	for _, a := range []string{"a.example:27015", "b.example:27016"} {
		contains(t, e.b.execute(dm("u1", "server", subcommand("add", str("address", a)))), "Now watching")
	}
	got, _ := e.st.ListenerServers("user:u1")
	if len(got) != 2 {
		t.Fatalf("servers %v", got)
	}
	contains(t, e.b.execute(dm("u1", "server", subcommand("remove", str("address", "a.example:27015")))), "Stopped watching")
	if left, _ := e.st.ListenerServers("user:u1"); len(left) != 1 || left[0].Addr != "b.example:27016" {
		t.Fatalf("the other server must stay: %v", left)
	}
	if len(e.w.removed) != 1 || e.w.removed[0] != "a.example:27015" {
		t.Fatalf("only the removed server may be unwatched: %v", e.w.removed)
	}
	// Another user's DM scope is separate.
	if got, _ := e.st.ListenerServers("user:u2"); len(got) != 0 {
		t.Fatalf("leaked to another user: %v", got)
	}
}

func TestServerAddRefusesPrivateAndLimit(t *testing.T) {
	e := newEnv(t)
	contains(t, e.b.execute(dm("u1", "server", subcommand("add", str("address", "localhost:27015")))), "not a public address")
	for i := 0; i < maxServersPerScope; i++ {
		e.b.execute(dm("u1", "server", subcommand("add", str("address", fmt.Sprintf("h%d.example:27015", i)))))
	}
	contains(t, e.b.execute(dm("u1", "server", subcommand("add", str("address", "extra.example:27015")))), "at most")
}

func TestServerRemoveStopsWatchingAndDropsItsNotifications(t *testing.T) {
	e := newEnv(t)
	e.b.execute(dm("u1", "server", subcommand("add", str("address", "a.example:27015"))))
	e.b.execute(dm("u1", "server", subcommand("add", str("address", "b.example:27015"))))
	e.b.execute(ndm("u1", "status", str("value", "offline"), str("server", "a.example:27015"))) // for a only
	e.b.execute(ndm("u1", "status", str("value", "online")))                                    // for all servers
	if ns, _ := e.st.UserNotifications("u1", "user:u1"); len(ns) != 2 {
		t.Fatalf("setup: %v", ns)
	}
	contains(t, e.b.execute(dm("u1", "server", subcommand("remove", str("address", "nope.example:27015")))), "not one of")
	contains(t, e.b.execute(dm("u1", "server", subcommand("remove", str("address", "a.example")))), "Stopped watching") // default port
	if len(e.w.removed) != 1 || e.w.removed[0] != "a.example:27015" {
		t.Fatalf("unwatched %v", e.w.removed)
	}
	// The one made for that server goes with it. The one for all servers stays, for the others.
	ns, _ := e.st.UserNotifications("u1", "user:u1")
	if len(ns) != 1 || ns[0].ServerID != rules.AllServers {
		t.Fatalf("left: %+v", ns)
	}
}
func TestServerRemoveNeedsManageServerInAGuild(t *testing.T) {
	e := newEnv(t)
	e.b.execute(guild("admin", manage, "server", subcommand("add", str("address", "a.example:27015"))))
	contains(t, e.b.execute(guild("u1", 0, "server", subcommand("remove", str("address", "a.example:27015")))), "Manage Server")
	if len(e.w.removed) != 0 {
		t.Fatal("removed without permission")
	}
}

func setupGuild(t *testing.T, e *env, addrs ...string) {
	t.Helper()
	for _, a := range addrs {
		contains(t, e.b.execute(guild("admin", manage, "server", subcommand("add", str("address", a)))), "Now watching")
	}
}

func mustServer(t *testing.T, e *env, scope string) store.ServerRef {
	t.Helper()
	list, err := e.st.ListenerServers(scope)
	if err != nil || len(list) == 0 {
		t.Fatalf("no server for %s: %v", scope, err)
	}
	return list[0]
}

func TestStatus(t *testing.T) {
	e := newEnv(t)
	contains(t, e.b.execute(guild("u1", 0, "status")), "No game server")
	setupGuild(t, e, "a.example:27015", "b.example:27015")
	ids := map[string]int64{}
	list, _ := e.st.ListenerServers("guild:g1")
	for _, s := range list {
		ids[s.Addr] = s.ID
	}
	contains(t, e.b.execute(guild("u1", 0, "status")), "no data yet")

	now := time.Now()
	e.st.Apply(ids["a.example:27015"], rules.Snapshot{Online: true, Title: "Alpha", Map: "de_dust2", Players: 12, MaxPlayers: 64, NamesKnown: true}, now)
	e.st.Apply(ids["b.example:27015"], rules.Snapshot{Online: true, Title: "Beta", Map: "cs_office", Players: 1, MaxPlayers: 32, NamesKnown: true}, now)
	e.st.Apply(ids["b.example:27015"], rules.Snapshot{}, now.Add(time.Second)) // goes offline

	got := e.b.execute(guild("u1", 0, "status"))
	contains(t, got, "Alpha", "online", "12/64", "de_dust2", "Beta", "offline", "cs_office")
	// /status takes nothing: it always shows every server, whatever else came along.
	for _, opts := range [][]opt{nil, {str("server", "a.example")}, {subcommand("anything")}} {
		both := e.b.execute(guild("u1", 0, "status", opts...))
		if !strings.Contains(both, "Alpha") || !strings.Contains(both, "Beta") {
			t.Errorf("not every server in %q", both)
		}
	}
}

func mustServer2(t *testing.T, e *env) store.ServerRef {
	t.Helper()
	l := mustServers(t, e)
	if len(l) < 2 {
		t.Fatalf("want two servers, have %d", len(l))
	}
	return l[1]
}

func mustServers(t *testing.T, e *env) []store.ServerRef {
	t.Helper()
	l, err := e.st.ListenerServers("guild:g1")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestStatusFormattingForUnnamedAndMarkdownServers(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015", "b.example:27015", "c.example:27015")
	list := mustServers(t, e)
	now := time.Now()
	e.st.Apply(list[0].ID, rules.Snapshot{}, now)                                                                        // never answered
	e.st.Apply(list[1].ID, rules.Snapshot{Online: true, Title: "*Fun* _Server_ ``x``", Map: "m", NamesKnown: true}, now) // markdown in the name
	// list[2] has no state at all.

	got := e.b.execute(guild("u1", 0, "status"))
	if strings.Contains(got, "****") {
		t.Errorf("empty bold markers: %q", got)
	}
	// The name sits in a code span, where formatting characters are plain text, and a
	// backtick in the name cannot end the span early.
	contains(t, got, "`a.example:27015`: **offline**, no answer yet", "`*Fun* _Server_ ''x''` (`b.example:27015`)", "no data yet")
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.Count(line, "`")%2 != 0 || strings.Count(line, "**")%2 != 0 {
			t.Errorf("unbalanced markers in %q", line)
		}
	}
}

// Game server names often contain Discord invite links. Discord links and expands them
// unless they are inside a code span.
func TestInviteLinksInNamesStayInsideCodeSpans(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	ref := mustServer(t, e, "guild:g1")
	e.st.Apply(ref.ID, rules.Snapshot{Online: true, Title: "Zombie Escape | discord.gg/nide", Map: "m", Players: 3, MaxPlayers: 65, NamesKnown: true}, time.Now())
	got := e.b.execute(guild("u1", 0, "status"))
	i := strings.Index(got, "discord.gg/nide")
	if i < 0 {
		t.Fatalf("name missing: %q", got)
	}
	if strings.Count(got[:i], "`")%2 != 1 {
		t.Errorf("invite link is outside a code span: %q", got)
	}
}

func TestStatusSaysWhenDataCameFromGameTracker(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	ref := mustServer(t, e, "guild:g1")
	snap := rules.Snapshot{Online: true, Title: "Zombie", Map: "m", Players: 3, MaxPlayers: 64, NamesKnown: true,
		Source: "gametracker", DataAt: time.Now().Add(-7 * time.Minute)}
	e.st.Apply(ref.ID, snap, time.Now())
	got := e.b.execute(guild("u1", 0, "status"))
	contains(t, got, "Zombie", "online", "3/64", "Via GameTracker, scanned 7 min ago")
	if strings.Contains(got, "Updated") {
		t.Errorf("should describe the data's age, not our poll: %q", got)
	}
	snap.Source, snap.DataAt = "a2s", time.Now()
	e.st.Apply(ref.ID, snap, time.Now())
	if got := e.b.execute(guild("u1", 0, "status")); strings.Contains(got, "GameTracker") {
		t.Errorf("A2S data labelled as GameTracker: %q", got)
	}
}

func TestRepliesAreEphemeralWithoutPreviewsOrPings(t *testing.T) {
	d := replyData("Zombie Escape | discord.gg/nide")
	if d.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Error("replies must be visible only to the person who ran the command")
	}
	if d.Flags&discordgo.MessageFlagsSuppressEmbeds == 0 {
		t.Error("link previews must be off, or invite links in server names become cards")
	}
	if d.AllowedMentions == nil || len(d.AllowedMentions.Parse) != 0 || len(d.AllowedMentions.Users) != 0 {
		t.Errorf("replies must not allow any ping: %+v", d.AllowedMentions)
	}
}

func applyOnline(e *env, id int64, title string, players int, names ...string) {
	e.st.Apply(id, rules.Snapshot{Online: true, Title: title, Map: "m", Players: players, MaxPlayers: 64, PlayerNames: names, NamesKnown: true}, time.Now())
}

func TestStatusListsThePlayersOfEveryServer(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	applyOnline(e, mustServer(t, e, "guild:g1").ID, "Alpha", 3, "alice", "bob", "carl")
	got := e.b.execute(guild("u1", 0, "status"))
	contains(t, got, "Alpha", "3/64 players", "```\nalice, bob, carl\n```")
}

func TestStatusShowsEachServerWithItsOwnPlayers(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015", "b.example:27015")
	ids := map[string]int64{}
	for _, s := range mustServers(t, e) {
		ids[s.Addr] = s.ID
	}
	applyOnline(e, ids["a.example:27015"], "Alpha", 1, "alice")
	applyOnline(e, ids["b.example:27015"], "Beta", 1, "bob")
	got := e.b.execute(guild("u1", 0, "status"))
	contains(t, got, "Alpha", "alice", "Beta", "bob")
	// Alice is listed under Alpha and Bob under Beta, in that order.
	if !(strings.Index(got, "Alpha") < strings.Index(got, "alice") && strings.Index(got, "alice") < strings.Index(got, "Beta") && strings.Index(got, "Beta") < strings.Index(got, "bob")) {
		t.Errorf("players under the wrong server: %q", got)
	}
	if strings.Contains(got, "Which game server") {
		t.Errorf("it never asks: %q", got)
	}
}

func TestPlayersViewEdgeCases(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	ref := mustServer(t, e, "guild:g1")
	view := func() string { return e.b.execute(guild("u1", 0, "status")) }

	applyOnline(e, ref.ID, "Alpha", 0)
	if got := view(); strings.Contains(got, "```") {
		t.Errorf("an empty server has no list: %q", got)
	}

	// Players are there but their names could not be read.
	e.st.Apply(ref.ID, rules.Snapshot{Online: true, Title: "Alpha", Map: "m", Players: 4, MaxPlayers: 64, NamesKnown: false}, time.Now())
	contains(t, view(), "4/64 players", "names are not available")

	// Names come from other people: a backtick, line break, invite link or huge name must
	// not break out of the block or change the layout.
	applyOnline(e, ref.ID, "Alpha", 3, "evil```\n@everyone", "join discord.gg/xyz", strings.Repeat("x", 100))
	got := view()
	if strings.Count(got, "```") != 2 {
		t.Errorf("a name broke the code block: %q", got)
	}
	contains(t, got, "evil''' @everyone", "discord.gg/xyz")
	if strings.Contains(got, strings.Repeat("x", 40)) {
		t.Errorf("long name not shortened: %q", got)
	}
	if i := strings.Index(got, "discord.gg/xyz"); strings.Count(got[:i], "```")%2 != 1 {
		t.Errorf("invite link outside the code block: %q", got)
	}

	// An offline server lists nobody.
	e.st.Apply(ref.ID, rules.Snapshot{}, time.Now())
	if got := view(); strings.Contains(got, "```") {
		t.Errorf("offline server lists players: %q", got)
	}
}
func TestStatusRepliesStayUnderDiscordsLimit(t *testing.T) {
	e := newEnv(t)
	var names []string
	for i := 0; i < 64; i++ {
		names = append(names, fmt.Sprintf("player_with_a_very_long_name_%02d", i))
	}
	setupGuild(t, e, "a.example:27015")
	applyOnline(e, mustServer(t, e, "guild:g1").ID, "Busy", 64, names...)

	// A busy server: the list is cut, says how many are left out, and the block is closed.
	got := e.b.execute(guild("u1", 0, "status"))
	if n := len([]rune(got)); n > 2000 {
		t.Fatalf("players view has %d characters", n)
	}
	contains(t, got, "and ", " more")
	if strings.Count(got, "```")%2 != 0 {
		t.Errorf("unbalanced code block: %q", got)
	}

	// Many servers with long names: the summary still fits, and every server still shows.
	for i := 0; i < 9; i++ { // more than the command allows, to test the limit of the reply
		if _, _, err := e.st.AddListener("guild:g1", fmt.Sprintf("host%d.example:27015", i), "admin", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range mustServers(t, e) {
		applyOnline(e, s.ID, strings.Repeat("N", 80), 64, names...)
	}
	got = e.b.execute(guild("u1", 0, "status"))
	if n := len([]rune(got)); n > 2000 {
		t.Fatalf("status for 10 servers has %d characters", n)
	}
	for _, s := range mustServers(t, e) {
		if !strings.Contains(got, s.Addr) {
			t.Errorf("server %s missing from the summary", s.Addr)
		}
	}
}

type fakeRegistrar struct {
	calls []regCall
	fail  map[string]error
}

type regCall struct {
	guild string
	n     int
}

func (f *fakeRegistrar) ApplicationCommandBulkOverwrite(app, guild string, cmds []*discordgo.ApplicationCommand, _ ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	f.calls = append(f.calls, regCall{guild, len(cmds)})
	return cmds, f.fail[guild]
}

func TestCommandsAreRegisteredGloballyAndOldGuildCopyRemoved(t *testing.T) {
	f := &fakeRegistrar{}
	n, err := registerCommands(f, "123", "456")
	if err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if n != 6 || len(f.calls) != 2 || f.calls[0] != (regCall{"", 6}) || f.calls[1] != (regCall{"456", 0}) {
		t.Fatalf("calls %+v: want one global registration of all commands, then the old server-only copy emptied", f.calls)
	}
	f = &fakeRegistrar{}
	registerCommands(f, "123", "")
	if len(f.calls) != 1 || f.calls[0].guild != "" {
		t.Fatalf("without a server to clean up there must be exactly one global call: %+v", f.calls)
	}
	f = &fakeRegistrar{fail: map[string]error{"": errors.New("boom")}}
	if _, err := registerCommands(f, "123", "456"); err == nil || len(f.calls) != 1 {
		t.Fatalf("a failed global registration must stop before the cleanup: %v %+v", err, f.calls)
	}
}

func TestEveryCommandWorksInServersAndDMs(t *testing.T) {
	for _, cmd := range commands() {
		if cmd.Contexts == nil {
			t.Fatalf("/%s: contexts must be explicit", cmd.Name)
		}
		has := map[discordgo.InteractionContextType]bool{}
		for _, c := range *cmd.Contexts {
			has[c] = true
		}
		if !has[discordgo.InteractionContextGuild] || !has[discordgo.InteractionContextBotDM] {
			t.Errorf("/%s must work in servers and in DMs with the bot: %v", cmd.Name, *cmd.Contexts)
		}
		if cmd.GuildID != "" {
			t.Errorf("/%s: a global command has no guild: %q", cmd.Name, cmd.GuildID)
		}
		if cmd.IntegrationTypes == nil || len(*cmd.IntegrationTypes) != 1 || (*cmd.IntegrationTypes)[0] != discordgo.ApplicationIntegrationGuildInstall {
			t.Errorf("/%s: integration types %v", cmd.Name, cmd.IntegrationTypes)
		}
	}
}

// --- answering: Discord wants a first response within 3 seconds ---

type fakeResponder struct {
	mu    sync.Mutex
	calls []string
	data  []*discordgo.InteractionResponseData
	edit  *discordgo.WebhookEdit
}

func (f *fakeResponder) InteractionRespond(_ *discordgo.Interaction, r *discordgo.InteractionResponse, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprint("respond:", r.Type))
	f.data = append(f.data, r.Data)
	return nil
}

func (f *fakeResponder) InteractionResponseEdit(_ *discordgo.Interaction, e *discordgo.WebhookEdit, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "edit")
	f.edit = e
	return &discordgo.Message{}, nil
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestQuickCommandsAreAnsweredDirectly(t *testing.T) {
	f := &fakeResponder{}
	answer(f, quietLog(), &discordgo.Interaction{}, false, func() reply { return reply{content: "hi"} })
	if len(f.calls) != 1 || f.calls[0] != fmt.Sprint("respond:", discordgo.InteractionResponseChannelMessageWithSource) {
		t.Fatalf("calls %v", f.calls)
	}
	d := f.data[0]
	if d.Content != "hi" || d.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Errorf("reply %+v", d)
	}
}

func TestSlowCommandsAreAcknowledgedThenFilledIn(t *testing.T) {
	old := deferAfter
	deferAfter = 20 * time.Millisecond
	defer func() { deferAfter = old }()

	f := &fakeResponder{}
	answer(f, quietLog(), &discordgo.Interaction{}, false, func() reply { time.Sleep(80 * time.Millisecond); return reply{content: "done"} })
	want := []string{fmt.Sprint("respond:", discordgo.InteractionResponseDeferredChannelMessageWithSource), "edit"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
	if f.data[0].Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Error("the acknowledgement must be private too, or the filled-in reply would be public")
	}
	if f.edit == nil || f.edit.Content == nil || *f.edit.Content != "done" {
		t.Fatalf("edit %+v", f.edit)
	}
	if f.edit.AllowedMentions == nil || len(f.edit.AllowedMentions.Parse) != 0 {
		t.Errorf("the filled-in reply must not allow pings: %+v", f.edit.AllowedMentions)
	}
}

func TestACrashingCommandIsAnsweredNotFatal(t *testing.T) {
	f := &fakeResponder{}
	answer(f, quietLog(), &discordgo.Interaction{}, false, func() reply { panic("boom") })
	if len(f.data) != 1 || !strings.Contains(f.data[0].Content, "went wrong") {
		t.Fatalf("data %+v", f.data)
	}
}

func TestAPanicInAutocompleteOrAButtonIsLoggedNotFatal(t *testing.T) {
	var logs strings.Builder
	b := &Bot{log: slog.New(slog.NewTextHandler(&logs, nil))} // no store, so every lookup panics
	o := str("map-name", "de")
	o.Focused = true
	ac := ndm("u1", "map", o)
	ac.Type = discordgo.InteractionApplicationCommandAutocomplete
	press := &discordgo.Interaction{Type: discordgo.InteractionMessageComponent, User: &discordgo.User{ID: "u1"},
		Data: discordgo.MessageComponentInteractionData{CustomID: customResetConfirm}}
	for _, in := range []*discordgo.Interaction{ac, press} {
		b.onInteraction(nil, &discordgo.InteractionCreate{Interaction: in}) // returns instead of crashing
	}
	if n := strings.Count(logs.String(), "interaction panicked"); n != 2 {
		t.Errorf("%d panics logged, want 2:\n%s", n, logs.String())
	}
}

// --- layout ---

// --- /notify: one subcommand per type, plus list and remove ---

// ndm and nguild build /notify <sub> ... run in a DM and in Discord server g1.
func ndm(user, sub string, options ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.Interaction {
	return dm(user, "notify", subcommand(sub, options...))
}

func nguild(user string, perms int64, sub string, options ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.Interaction {
	return guild(user, perms, "notify", subcommand(sub, options...))
}

func subcommand(name string, options ...*discordgo.ApplicationCommandInteractionDataOption) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionSubCommand, Options: options}
}

type opt = *discordgo.ApplicationCommandInteractionDataOption

// ids is the numbers the user sees for their notifications in the test Discord server (E18).
func ids(t *testing.T, e *env, user string) []int64 {
	t.Helper()
	ns, _ := e.st.UserNotifications(user, "guild:g1")
	var out []int64
	for _, n := range ns {
		out = append(out, n.Num)
	}
	return out
}

func TestCommandsAreTopLevelWithNotifyHoldingTheNotificationCommands(t *testing.T) {
	var names []string
	for _, c := range commands() {
		names = append(names, c.Name)
	}
	if want := []string{"server", "notify", "status", "admin", "help", "version"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("commands %v, want %v", names, want)
	}
	notify, _ := find("notify")
	var subs []string
	for _, o := range notify.options {
		if o.Type != discordgo.ApplicationCommandOptionSubCommand {
			t.Errorf("/notify %s is not a subcommand", o.Name)
		}
		subs = append(subs, o.Name)
	}
	if want := []string{"map", "player", "playercount", "status", "title-query", "list", "show", "remove", "settings", "reset"}; !reflect.DeepEqual(subs, want) {
		t.Fatalf("/notify subcommands %v, want %v", subs, want)
	}
}

// Discord rejects a command that breaks its limits, and the first sign would be a failed
// registration, so check them here, including the options inside subcommands.
func TestCommandsMeetDiscordLimits(t *testing.T) {
	var check func(path string, opts []*discordgo.ApplicationCommandOption)
	check = func(path string, opts []*discordgo.ApplicationCommandOption) {
		if len(opts) > 25 {
			t.Errorf("%s has too many options", path)
		}
		optional := false
		for _, o := range opts {
			p := path + " " + o.Name
			if len(o.Name) == 0 || len(o.Name) > 32 || o.Name != strings.ToLower(o.Name) || len(o.Description) == 0 || len(o.Description) > 100 {
				t.Errorf("%s breaks a name or description limit", p)
			}
			if o.Required && optional {
				t.Errorf("%s: required option after an optional one", p)
			}
			if !o.Required && o.Type != discordgo.ApplicationCommandOptionSubCommand {
				optional = true
			}
			if o.Autocomplete && len(o.Choices) > 0 {
				t.Errorf("%s has both choices and autocomplete", p)
			}
			for _, c := range o.Choices {
				if len(c.Name) == 0 || len(c.Name) > 100 {
					t.Errorf("%s choice %q breaks the name limit", p, c.Name)
				}
			}
			if len(o.Choices) > 25 {
				t.Errorf("%s has too many choices", p)
			}
			check(p, o.Options)
		}
	}
	seen := map[string]bool{}
	for _, c := range commands() {
		if seen[c.Name] || len(c.Name) > 32 || c.Name != strings.ToLower(c.Name) || len(c.Description) == 0 || len(c.Description) > 100 {
			t.Errorf("/%s breaks a limit or is a duplicate", c.Name)
		}
		seen[c.Name] = true
		check("/"+c.Name, c.Options)
	}
}

func TestVersionShowsTheBuildOrDev(t *testing.T) {
	e := newEnv(t)
	contains(t, e.b.execute(dm("u1", "version")), "`dev`", "go")
	e.b.deps.Version = "v1.2.3"
	contains(t, e.b.execute(dm("u1", "version")), "`v1.2.3`")
}

func TestHelpHasQuickStartAndStaysShort(t *testing.T) {
	got := newEnv(t).b.execute(dm("u1", "help"))
	contains(t, got, "Quick start", "`/server`", "`/notify", "`/status`", "Manage Server")
	for _, c := range commands() {
		contains(t, got, "`/"+c.Name+"`")
	}
	if n := len([]rune(got)); n > 1500 {
		t.Errorf("help is %d characters, too long to read at a glance", n)
	}
	if strings.Contains(got, "/sourcewatch") || strings.Contains(got, "/unnotify") {
		t.Errorf("old command names: %q", got)
	}
}

func TestNotifyEachTypeSavesOneNotificationAndSaysItsID(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	for _, tc := range []struct {
		sub  string
		opts []opt
		want string
	}{
		{"map", []opt{str("map-name", "de_dust2")}, "the map is `de_dust2`"},
		{"player", []opt{str("username", "alice")}, "`alice` is online"},
		{"playercount", []opt{str("value", "50")}, "the player count is >= 50"}, // a plain number means at least
		{"playercount", []opt{str("value", ">50")}, "the player count is > 50"},
		{"playercount", []opt{str("value", "<3")}, "the player count is < 3"},
		{"status", []opt{str("value", "offline")}, "the server is offline"},
		{"status", []opt{str("value", "ONLINE")}, "the server is online"},
		{"title-query", []opt{str("value", "event")}, "the server name contains `event`"},
	} {
		got := e.b.execute(nguild("u1", 0, tc.sub, tc.opts...))
		contains(t, got, "✅ Notification saved as ID: ", tc.want, "in this channel")
	}
	got := ids(t, e, "u1")
	if len(got) != 8 {
		t.Fatalf("stored %d, want one notification per command", len(got))
	}
	// The reply names the real ID.
	reply := e.b.execute(nguild("u1", 0, "map", str("map-name", "de_nuke")))
	contains(t, reply, fmt.Sprintf("Notification saved as ID: %d", ids(t, e, "u1")[8]))
	ns, _ := e.st.UserNotifications("u1", "guild:g1")
	if ns[0].ChannelID != "c1" || len(ns[0].Conditions) != 1 || ns[0].CooldownSec != 0 {
		t.Errorf("first: %+v", ns[0])
	}
	if plain := ns[2].Conditions[0]; plain.Op != ">=" || plain.N != 50 {
		t.Errorf("a plain number must mean at least: %+v", plain)
	}
	if pc := ns[4]; pc.Conditions[0].N != 3 || pc.Conditions[0].Op != "<" || pc.CooldownSec != 0 || pc.EffectiveDelivery() != "channel" {
		t.Errorf("playercount: %+v", pc)
	}
	// A new notification has the defaults. Cooldown and delivery are changed with /notify settings.
	contains(t, reply, "Cooldown: 60 mins", "/notify settings id: ")
}

func TestNotifyRejectsBadInput(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	run := func(sub string, o ...opt) string { return e.b.execute(nguild("u1", 0, sub, o...)) }
	contains(t, run("map"), "`map-name`")
	contains(t, run("player", str("username", "  ")), "`username`")
	contains(t, run("title-query"), "`value`")
	contains(t, run("status", str("value", "maybe")), "online")
	// A player count needs a number, and the error explains the format.
	for _, bad := range []string{"", "many", "-3", "65", "5000", ">=65", ">=", ">=ten", "<<5", "10 players"} {
		contains(t, run("playercount", str("value", bad)), "`value` needs a number of players from 0 to 64", "`>=10`", "plain number", "means at least 10")
	}
	contains(t, run("map", str("map-name", "x"), str("server", "other.example:27015")), "not one of")
	contains(t, e.b.execute(dm("u1", "notify")), "Choose what to be notified about")
	contains(t, e.b.execute(guild("u1", 0, "notify", subcommand("nope"))), "Unknown notification type")
	if got := ids(t, e, "u1"); len(got) != 0 {
		t.Fatalf("created despite errors: %v", got)
	}
}

func TestNotifyWithoutServers(t *testing.T) {
	e := newEnv(t)
	contains(t, e.b.execute(nguild("u1", 0, "status", str("value", "offline"))), "Manage Server", "`/server add`")
	contains(t, e.b.execute(ndm("u1", "status", str("value", "offline"))), "`/server add`")
}

func TestNotifyWithoutAServerIsOneNotificationForAllServers(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015", "b.example:27015")
	got := e.b.execute(nguild("u1", 0, "status", str("value", "offline")))
	contains(t, got, "Notification saved as ID:", "on all your game servers", "the server is offline", "Cooldown: 60 mins")
	if strings.Contains(got, "Which game server") || strings.Contains(got, "Notifications saved") {
		t.Errorf("one notification, no question: %q", got)
	}
	ns, _ := e.st.UserNotifications("u1", "guild:g1")
	if len(ns) != 1 || ns[0].ServerID != rules.AllServers {
		t.Fatalf("want one notification for all servers: %+v", ns)
	}

	// Naming a server makes one for just that server.
	one := e.b.execute(nguild("u1", 0, "map", str("map-name", "m"), str("server", "b.example:27015")))
	contains(t, one, "Notification saved as ID:", "b.example:27015")
	if ns, _ := e.st.UserNotifications("u1", "guild:g1"); len(ns) != 2 || ns[1].ServerID == rules.AllServers {
		t.Fatalf("%+v", ns)
	}
	// An unknown server is refused, and nothing is made.
	contains(t, e.b.execute(nguild("u1", 0, "map", str("map-name", "m"), str("server", "zzz.example:27015"))), "not one of")
	if ns, _ := e.st.UserNotifications("u1", "guild:g1"); len(ns) != 2 {
		t.Fatalf("a refused command made some: %d", len(ns))
	}
}

func TestAnAllServersNotificationFiresForServersAddedLater(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "status", str("value", "offline")))
	setupGuild(t, e, "b.example:27015") // added after
	var b store.ServerRef
	for _, s := range mustServers(t, e) {
		if s.Addr == "b.example:27015" {
			b = s
		}
	}
	e.st.Process(b.ID, rules.Snapshot{Online: true, Title: "B", Map: "m", NamesKnown: true}, time.Now())
	evs, _ := e.st.Process(b.ID, rules.Snapshot{}, time.Now().Add(time.Minute))
	if len(evs) != 1 || evs[0].ServerID != b.ID {
		t.Fatalf("the later server did not fire it: %+v", evs)
	}
}

func TestNotifyForAllServersHintsFromEveryServersHistory(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015", "b.example:27015")
	list := mustServers(t, e)
	e.st.Apply(list[0].ID, rules.Snapshot{Online: true, Title: "T", Map: "de_dust2", NamesKnown: true}, time.Now())
	e.st.Apply(list[1].ID, rules.Snapshot{Online: true, Title: "T", Map: "cs_office", NamesKnown: true}, time.Now())
	got := e.b.execute(nguild("u1", 0, "map", str("map-name", "de_dsut2")))
	contains(t, got, "not seen map", "any of your servers", "`de_dust2`")
	// A map seen on only one of them is not a surprise.
	if got := e.b.execute(nguild("u1", 0, "map", str("map-name", "cs_office"))); strings.Contains(got, "not seen") {
		t.Errorf("seen on the second server: %q", got)
	}
}
func TestNewNotificationsAreDeliveredInTheChannelAndSendNoDM(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	in := nguild("u1", 0, "status", str("value", "offline"))
	in.ChannelID = "4242"
	contains(t, e.b.execute(in), "saved as ID", "in this channel")
	ns, _ := e.st.UserNotifications("u1", "guild:g1")
	if ns[0].EffectiveDelivery() != "channel" || ns[0].ChannelID != "4242" || ns[0].CooldownSec != 0 {
		t.Fatalf("%+v", ns[0])
	}
	if len(e.dm.probed) != 0 {
		t.Fatalf("making a notification sent a DM: %v", e.dm.probed)
	}
	// To get it by DM, change the setting. That is where the test DM happens (E7).
	e.dm.err = notify.ErrDMClosed
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(ns[0].ID)), str("delivery", "dm"))), "can't send you a DM")
	e.dm.err = nil
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(ns[0].ID)), str("delivery", "dm"))), "Delivery: by DM")
}

func TestNotifyInADMIsDeliveredByDMWithoutATestDM(t *testing.T) {
	e := newEnv(t)
	e.b.execute(dm("u1", "server", subcommand("add", str("address", "a.example:27015"))))
	contains(t, e.b.execute(ndm("u1", "status", str("value", "offline"))), "saved as ID", "by DM")
	if len(e.dm.probed) != 0 {
		t.Fatalf("the person is already talking to the bot in a DM, so no test DM is needed: %v", e.dm.probed)
	}
}

func TestNotifyHintsAtSimilarNamesFromHistory(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.st.Apply(mustServer(t, e, "guild:g1").ID, rules.Snapshot{Online: true, Title: "T", Map: "de_dust2", NamesKnown: true, PlayerNames: []string{"alice"}}, time.Now())

	contains(t, e.b.execute(nguild("u1", 0, "map", str("map-name", "de_dsut2"))), "saved as ID", "not seen map", "`de_dust2`")
	if got := e.b.execute(nguild("u1", 0, "map", str("map-name", "de_dust2"))); strings.Contains(got, "not seen") {
		t.Errorf("exact match should not warn: %q", got)
	}
	contains(t, e.b.execute(nguild("u1", 0, "player", str("username", "alise"))), "not seen player", "`alice`")
}

func TestNotifyLimit(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	for i := 0; i < maxNotificationsPerUser; i++ {
		contains(t, e.b.execute(nguild("u1", 0, "title-query", str("value", fmt.Sprint("t", i)))), "saved as ID")
	}
	contains(t, e.b.execute(nguild("u1", 0, "title-query", str("value", "one more"))), "already have")
}

// --- the id option combines notifications ---

func TestIDAddsAConditionToAnExistingNotification(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "de_dust2")))
	id := ids(t, e, "u1")[0]

	got := e.b.execute(nguild("u1", 0, "playercount", str("value", ">=10"), num("id", int(id))))
	contains(t, got, fmt.Sprintf("Notification saved as ID: %d", id), "all of these are true", "• the map is `de_dust2`", "• the player count is >= 10")
	if len(ids(t, e, "u1")) != 1 {
		t.Fatalf("combining must not make a second notification: %v", ids(t, e, "u1"))
	}
	ns, _ := e.st.UserNotifications("u1", "guild:g1")
	if len(ns[0].Conditions) != 2 {
		t.Fatalf("conditions %+v", ns[0].Conditions)
	}
	contains(t, e.b.execute(nguild("u1", 0, "list")), "the map is `de_dust2` **and** the player count is >= 10")
}

func TestIDKeepsTheGameServerAndTheSettingsOfTheNotification(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015", "b.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m1"), str("server", "a.example:27015")))
	id := int(ids(t, e, "u1")[0])
	e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(id)), num("cooldown", 5)))

	// A condition added to a notification is about the same game server, so another one is refused.
	got := e.b.execute(nguild("u1", 0, "status", str("value", "online"), num("id", id), str("server", "b.example:27015")))
	contains(t, got, "keeps the game server it was made with", "`server`")
	if ns, _ := e.st.UserNotifications("u1", "guild:g1"); len(ns[0].Conditions) != 1 {
		t.Fatalf("conditions %+v", ns[0].Conditions)
	}
	// Without it the condition is added, and the server and the cooldown stay as they were.
	contains(t, e.b.execute(nguild("u1", 0, "status", str("value", "online"), num("id", id))), "a.example:27015", "Cooldown: 5 mins")
	ns, _ := e.st.UserNotifications("u1", "guild:g1")
	if len(ns[0].Conditions) != 2 || ns[0].CooldownSec != 300 {
		t.Fatalf("%+v", ns[0])
	}
}

func TestAnIDYouDoNotHaveIsUsedForANewNotification(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u2", 0, "map", str("map-name", "m1")))
	got := e.b.execute(nguild("u1", 0, "status", str("value", "online"), num("id", 5)))
	contains(t, got, "You had no notification with ID 5, so I made a new one", "Notification saved as ID: 5")
	if got := ids(t, e, "u1"); len(got) != 1 || got[0] != 5 {
		t.Fatalf("u1 has %v, want just ID 5", got)
	}
	// It is an ordinary notification now: the same ID adds a condition to it.
	contains(t, e.b.execute(nguild("u1", 0, "playercount", str("value", ">=3"), num("id", 5))), "saved as ID: 5", "and")
	// The next one without an ID takes the user's lowest free number.
	contains(t, e.b.execute(nguild("u1", 0, "map", str("map-name", "m9"))), "saved as ID: 1")
}

func TestEachPersonNumbersTheirOwnNotifications(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	for i := range 3 {
		e.b.execute(nguild("u2", 0, "map", str("map-name", fmt.Sprint("m", i))))
	}
	// u2 has 1, 2 and 3. u1's numbers are their own, so they also start at 1, and id:3 is free for u1.
	contains(t, e.b.execute(nguild("u1", 0, "map", str("map-name", "x"))), "saved as ID: 1")
	contains(t, e.b.execute(nguild("u1", 0, "map", str("map-name", "y"), num("id", 3))), "You had no notification with ID 3", "saved as ID: 3")
	if got := ids(t, e, "u1"); !reflect.DeepEqual(got, []int64{1, 3}) {
		t.Fatalf("u1 has %v, want [1 3]", got)
	}
	// Adding to u1's 3 does not touch u2's 3, and removing u1's 1 leaves u2's 1.
	contains(t, e.b.execute(nguild("u1", 0, "player", str("username", "Al"), num("id", 3))), "saved as ID: 3", "`Al` is online")
	contains(t, e.b.execute(nguild("u1", 0, "remove", num("id", 1))), "Deleted notification ID: 1")
	ns, _ := e.st.UserNotifications("u2", "guild:g1")
	if len(ns) != 3 || len(ns[2].Conditions) != 1 {
		t.Fatalf("u2's notifications changed: %+v", ns)
	}
	// The same numbers in a DM are separate again.
	contains(t, e.b.execute(ndm("u1", "map", str("map-name", "z"))), "You are not watching")
}

func TestSeveralMapsAndPlayersAreShownAsOrAndAnd(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "de_a")))
	id := int(ids(t, e, "u1")[0])
	got := e.b.execute(nguild("u1", 0, "map", str("map-name", "de_b"), num("id", id)))
	contains(t, got, "the map is one of `de_a`, `de_b`")
	e.b.execute(nguild("u1", 0, "player", str("username", "Al"), num("id", id)))
	e.b.execute(nguild("u1", 0, "player", str("username", "Bo"), num("id", id)))
	got = e.b.execute(nguild("u1", 0, "list"))
	contains(t, got, "the map is one of `de_a`, `de_b` **and** `Al` is online **and** `Bo` is online")
}

func TestIDChecks(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m1")))
	e.b.execute(nguild("u2", 0, "map", str("map-name", "m1")))
	mine, theirs := int(ids(t, e, "u1")[0]), int(ids(t, e, "u2")[0])

	_ = theirs // numbers are per person: u2's 1 is not u1's business (E18)
	contains(t, e.b.execute(nguild("u1", 0, "status", str("value", "online"), num("id", 0))), "a number from 1 up")
	contains(t, e.b.execute(nguild("u1", 0, "map", str("map-name", "m1"), num("id", mine))), "already has that condition")
	contains(t, e.b.execute(nguild("u1", 0, "map", str("map-name", ""), num("id", mine))), "`map-name`")
	for i := 1; i < rules.MaxConditions; i++ {
		contains(t, e.b.execute(nguild("u1", 0, "title-query", str("value", fmt.Sprint("w", i)), num("id", mine))), "saved as ID")
	}
	contains(t, e.b.execute(nguild("u1", 0, "title-query", str("value", "too many"), num("id", mine))), "already combines")
	if ns, _ := e.st.UserNotifications("u2", "guild:g1"); len(ns[0].Conditions) != 1 {
		t.Fatalf("someone else's notification changed: %+v", ns[0])
	}
}

func TestACombinedNotificationFiresOnlyWhenAllHold(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	srv := mustServer(t, e, "guild:g1")
	e.st.Apply(srv.ID, rules.Snapshot{Online: true, Title: "T", Map: "m1", Players: 1, MaxPlayers: 64, NamesKnown: true}, time.Now())
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m2")))
	id := int(ids(t, e, "u1")[0])
	e.b.execute(nguild("u1", 0, "playercount", str("value", ">=3"), num("id", id)))

	at := time.Now()
	step := func(mp string, players int) int {
		at = at.Add(time.Minute)
		evs, err := e.st.Process(srv.ID, rules.Snapshot{Online: true, Title: "T", Map: mp, Players: players, MaxPlayers: 64, NamesKnown: true}, at)
		if err != nil {
			t.Fatal(err)
		}
		return len(evs)
	}
	if step("m1", 1) != 0 || step("m2", 1) != 0 || step("m1", 5) != 0 {
		t.Fatal("fired with only one condition true")
	}
	if step("m2", 5) != 1 {
		t.Fatal("did not fire when both became true together")
	}
}

// --- list and remove ---

func TestNotifyListAndRemove(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	contains(t, e.b.execute(nguild("u1", 0, "list")), "no notifications")
	e.b.execute(nguild("u1", 0, "status", str("value", "offline")))
	e.b.execute(nguild("u2", 0, "status", str("value", "online")))
	got := e.b.execute(nguild("u1", 0, "list"))
	contains(t, got, "the server is offline", "**All servers**", "/notify remove")
	if strings.Contains(got, "the server is online") {
		t.Errorf("listed another user's notification: %q", got)
	}
	u1 := int(ids(t, e, "u1")[0])
	contains(t, e.b.execute(nguild("u1", 0, "remove", num("id", 7))), "no notification with ID 7")
	contains(t, e.b.execute(guild("u1", 0, "notify", subcommand("remove"))), "`id`")
	contains(t, e.b.execute(nguild("u1", 0, "remove", num("id", u1))), fmt.Sprintf("Deleted notification ID: %d", u1))
	contains(t, e.b.execute(nguild("u1", 0, "list")), "no notifications")
	if len(ids(t, e, "u2")) != 1 {
		t.Fatal("another user's notification was removed")
	}
}

func TestRemoveOnlyDeletesANotificationMadeHere(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "status", str("value", "offline")))
	id := int(ids(t, e, "u1")[0])

	// The same user in another Discord server, and in a DM with the bot.
	e.b.execute(guildServer(e, "g2", "z.example:27015"))
	elsewhere := nguild("u1", 0, "remove", num("id", id))
	elsewhere.GuildID = "g2"
	for _, in := range []*discordgo.Interaction{elsewhere, ndm("u1", "remove", num("id", id))} {
		contains(t, e.b.execute(in), fmt.Sprintf("no notification with ID %d here", id))
	}
	if len(ids(t, e, "u1")) != 1 {
		t.Fatal("removed a notification made in another place")
	}
	contains(t, e.b.execute(nguild("u1", 0, "remove", num("id", id))), fmt.Sprintf("Deleted notification ID: %d", id))
}

func TestListGroupsByServer(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015", "b.example:27015")
	add := func(sub, value, srv string) {
		contains(t, e.b.execute(nguild("u1", 0, sub, str(valueOption(rules.Kind(sub)), value), str("server", srv))), "saved as ID")
	}
	add("status", "offline", "a.example:27015")
	add("map", "de_dust2", "b.example:27015")
	add("title-query", "event", "a.example:27015")
	got := e.b.execute(nguild("u1", 0, "list"))
	contains(t, got, "**Your notifications** (3)", "`a.example:27015`", "`b.example:27015`", "the server is offline", "de_dust2")
	ia, ib := strings.Index(got, "`a.example:27015`"), strings.Index(got, "`b.example:27015`")
	if ia > ib || strings.Index(got, "the server is offline") > ib || strings.Index(got, "contains `event`") > ib {
		t.Errorf("not grouped by server: %q", got)
	}
	contains(t, got, "`ID: ")
}

func TestListStaysUnderDiscordsLimit(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	for i := 0; i < maxNotificationsPerUser; i++ {
		contains(t, e.b.execute(nguild("u1", 0, "title-query", str("value", strings.Repeat("w", 50)+fmt.Sprint(i)))), "saved as ID")
	}
	got := e.b.execute(nguild("u1", 0, "list"))
	if n := len([]rune(got)); n > 2000 {
		t.Fatalf("list has %d characters", n)
	}
	contains(t, got, "(25)")
}

func TestNotifyTellsAboutUndeliverableOnce(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "status", str("value", "offline")))
	nid := ids(t, e, "u1")[0]
	e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(nid)), str("delivery", "dm")))
	e.st.MarkUndeliverable(nid)
	got := e.b.execute(nguild("u1", 0, "list"))
	contains(t, got, "could not be delivered", "refused the DM", "the server is offline", "switched them back on")
	if strings.Contains(got, "their channel") {
		t.Errorf("a DM notification was reported as a channel one: %q", got)
	}
	if got := e.b.execute(nguild("u1", 0, "list")); strings.Contains(got, "could not be delivered") {
		t.Errorf("told twice: %q", got)
	}
}

func TestNotifyTellsAboutARefusedChannelOnce(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "de_dust2")))
	in := nguild("u1", 0, "title-query", str("value", "event"))
	in.ChannelID = "777"
	e.b.execute(in)
	for _, id := range ids(t, e, "u1") {
		e.st.MarkUndeliverable(id)
	}
	got := e.b.execute(nguild("u1", 0, "list"))
	contains(t, got, "could not be posted in their channel", "the map is `de_dust2`", "<#777>", "View Channel and Send Messages",
		"/notify settings", "switched them back on")
	if strings.Contains(got, "refused the DM") {
		t.Errorf("a channel notification was reported as a DM: %q", got)
	}
	if got := e.b.execute(nguild("u1", 0, "list")); strings.Contains(got, "their channel") {
		t.Errorf("told twice: %q", got)
	}
}

// --- autocomplete ---

func TestAutocomplete(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015", "b.example:27015")
	ref, _ := matchServer(mustServers(t, e), "a.example:27015")
	e.st.Apply(ref.ID, rules.Snapshot{Online: true, Title: "T", Map: "de_dust2", NamesKnown: true, PlayerNames: []string{"alice", "bob"}}, time.Now())

	// notifyFocus asks for suggestions for a /notify subcommand option.
	notifyFocus := func(sub, name, value string, others ...opt) []string {
		o := str(name, value)
		o.Focused = true
		in := nguild("u1", 0, sub, append(others, o)...)
		in.Type = discordgo.InteractionApplicationCommandAutocomplete
		var out []string
		for _, c := range e.b.autocomplete(in) {
			out = append(out, fmt.Sprint(c.Value))
		}
		return out
	}
	srvA := str("server", "a.example:27015")
	if got := notifyFocus("map", "server", "b."); len(got) != 1 || got[0] != "b.example:27015" {
		t.Errorf("server choices %v", got)
	}
	if got := notifyFocus("map", "map-name", "dsut", srvA); len(got) != 1 || got[0] != "de_dust2" {
		t.Errorf("map suggestions %v", got)
	}
	if got := notifyFocus("player", "username", "", srvA); len(got) != 2 {
		t.Errorf("player suggestions for empty input %v", got)
	}
	if got := notifyFocus("player", "username", "ali", srvA); len(got) != 1 || got[0] != "alice" {
		t.Errorf("player suggestions %v", got)
	}
	// With two servers and none chosen, names from all of them are suggested.
	e.st.Apply(mustServer2(t, e).ID, rules.Snapshot{Online: true, Title: "T", Map: "cs_office", NamesKnown: true}, time.Now())
	if got := notifyFocus("map", "map-name", "office"); len(got) != 1 || got[0] != "cs_office" {
		t.Errorf("suggestions from the other server: %v", got)
	}
	if got := notifyFocus("map", "map-name", "dust"); len(got) != 1 || got[0] != "de_dust2" {
		t.Errorf("suggestions from the first server: %v", got)
	}
	// A server that is named narrows it to that one.
	if got := notifyFocus("map", "map-name", "office", srvA); len(got) != 0 {
		t.Errorf("a map of the other server suggested for %v", got)
	}
	// A title-query value has no suggestions.
	if got := notifyFocus("title-query", "value", "x", srvA); got != nil {
		t.Errorf("title suggestions %v", got)
	}

	// The id option lists the user's own notifications, to combine with or to remove.
	e.b.execute(nguild("u1", 0, "status", str("value", "offline"), srvA))
	e.b.execute(nguild("u2", 0, "status", str("value", "online"), srvA))
	for _, sub := range []string{"map", "remove"} {
		got := notifyFocus(sub, "id", "")
		if len(got) != 1 || got[0] != fmt.Sprint(ids(t, e, "u1")[0]) {
			t.Errorf("/notify %s id choices %v", sub, got)
		}
	}
}

func TestAutocompleteSuggestsPlayerCountComparisons(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	applyOnline(e, mustServer(t, e, "guild:g1").ID, "T", 3)
	focus := func(typed string) []*discordgo.ApplicationCommandOptionChoice {
		o := &discordgo.ApplicationCommandInteractionDataOption{Name: "value", Type: discordgo.ApplicationCommandOptionString, Value: typed, Focused: true}
		in := nguild("u1", 0, "playercount", o)
		in.Type = discordgo.InteractionApplicationCommandAutocomplete
		return e.b.autocomplete(in)
	}
	got := focus("")
	if len(got) == 0 {
		t.Fatal("no suggestions")
	}
	last := got[len(got)-1]
	if last.Value != ">=64" || !strings.Contains(last.Name, "at least 64 players") || !strings.Contains(last.Name, "full") {
		t.Fatalf("a full server: %+v", last)
	}
	// Typing a plain number shows what each comparison would mean.
	var vals []string
	for _, c := range focus("10") {
		vals = append(vals, fmt.Sprint(c.Value))
	}
	if !reflect.DeepEqual(vals, []string{">=10", ">10", "<=10", "<10", "=10", "!=10"}) {
		t.Fatalf("typing 10: %v", vals)
	}
}

func TestCreationCommandsHaveOnlyWhatTheyNeed(t *testing.T) {
	notify, _ := find("notify")
	sub := func(name string) *discordgo.ApplicationCommandOption {
		for _, o := range notify.options {
			if o.Name == name {
				return o
			}
		}
		t.Fatalf("no /notify %s", name)
		return nil
	}
	optionNames := func(o *discordgo.ApplicationCommandOption) (names []string) {
		for _, x := range o.Options {
			names = append(names, x.Name)
		}
		return
	}
	// What each type is about is named for what it is.
	for kind, want := range map[string][]string{
		"map":         {"map-name", "id", "server"},
		"player":      {"username", "id", "server"},
		"playercount": {"value", "id", "server"},
		"status":      {"value", "id", "server"},
		"title-query": {"value", "id", "server"},
	} {
		got := optionNames(sub(kind))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("/notify %s options %v, want %v", kind, got, want)
		}
		first := sub(kind).Options[0]
		if !first.Required || sub(kind).Options[1].Required || sub(kind).Options[2].Required {
			t.Errorf("/notify %s: only the first option is required", kind)
		}
		// Delivery and cooldown live in /notify settings only.
		for _, o := range sub(kind).Options {
			if o.Name == "delivery" || o.Name == "cooldown" || o.Name == "operator" {
				t.Errorf("/notify %s still has %s", kind, o.Name)
			}
		}
	}
	// Status is just online or offline, in lowercase.
	status := sub("status").Options[0]
	var names, vals []string
	for _, c := range status.Choices {
		names, vals = append(names, c.Name), append(vals, fmt.Sprint(c.Value))
	}
	if !reflect.DeepEqual(names, []string{"online", "offline"}) || !reflect.DeepEqual(vals, []string{"online", "offline"}) {
		t.Errorf("status choices %v / %v", names, vals)
	}
	// A player count is text that explains the comparison.
	if v := sub("playercount").Options[0]; v.Type != discordgo.ApplicationCommandOptionString || !v.Autocomplete ||
		!strings.Contains(v.Description, ">=10") || !strings.Contains(v.Description, "plain number means at least") {
		t.Errorf("playercount value: %+v", v)
	}
	// /notify settings is where they are changed.
	var have []string
	for _, o := range sub("settings").Options {
		have = append(have, o.Name)
	}
	if !reflect.DeepEqual(have, []string{"id", "cooldown", "active", "delivery"}) {
		t.Errorf("/notify settings options %v", have)
	}
	for _, o := range sub("settings").Options {
		if o.Name == "delivery" {
			var vals []string
			for _, c := range o.Choices {
				vals = append(vals, fmt.Sprint(c.Value))
			}
			if !reflect.DeepEqual(vals, []string{"channel", "dm"}) {
				t.Errorf("delivery values %v", vals)
			}
		}
	}
}

func TestPlayerCountChoices(t *testing.T) {
	values := func(c []countChoice) (out []string) {
		for _, x := range c {
			out = append(out, x.value)
		}
		return
	}
	// Nothing typed: useful "at least" values for a 64 slot server, labelled in words.
	got := playerCountChoices("", 64)
	if !reflect.DeepEqual(values(got), []string{">=1", ">=5", ">=10", ">=16", ">=32", ">=48", ">=62", ">=64"}) {
		t.Errorf("64 slots: %v", values(got))
	}
	for _, c := range got {
		if c.value == ">=64" && !strings.Contains(c.name, "full") || c.value == ">=32" && !strings.Contains(c.name, "half") {
			t.Errorf("label %q", c.name)
		}
	}
	// A number alone: every comparison with it.
	if got := values(playerCountChoices("10", 64)); !reflect.DeepEqual(got, []string{">=10", ">10", "<=10", "<10", "=10", "!=10"}) {
		t.Errorf("typing 10: %v", got)
	}
	// A comparison typed: only that one, with the numbers that fit what is typed so far.
	if got := values(playerCountChoices("<", 64)); len(got) == 0 || got[0] != "<1" {
		t.Errorf("typing <: %v", got)
	}
	if got := values(playerCountChoices("<=3", 64)); !reflect.DeepEqual(got, []string{"<=3", "<=32"}) {
		t.Errorf("typing <=3: %v", got)
	}
	if got := values(playerCountChoices("=>1", 64)); len(got) == 0 || got[0] != ">=1" {
		t.Errorf("the => slip is understood: %v", got)
	}
	// Above the capacity, junk and unknown comparisons give nothing.
	for _, in := range []string{"99", ">=99", "many", "<<5", "!"} {
		if got := playerCountChoices(in, 64); got != nil {
			t.Errorf("%q: %v", in, values(got))
		}
	}
	// Without a known capacity there is a generic list.
	if got := values(playerCountChoices("", 0)); len(got) < 4 || got[0] != ">=1" {
		t.Errorf("unknown capacity: %v", got)
	}
	// Every suggested value is one the bot accepts.
	for _, in := range []string{"", "5", ">", "<=2", "!=1"} {
		for _, c := range playerCountChoices(in, 64) {
			if _, _, explicit, err := rules.ParsePlayerCount(c.value); err != nil || !explicit {
				t.Errorf("suggested %q (typed %q) is not accepted: %v", c.value, in, err)
			}
			if len(c.name) > 100 {
				t.Errorf("label too long: %q", c.name)
			}
		}
	}
}

// A plain number is read as "at least", so 20 and >=20 are the same notification.
func TestPlainPlayerCountMeansAtLeast(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	plain := e.b.execute(nguild("u1", 0, "playercount", str("value", "20")))
	explicit := e.b.execute(nguild("u1", 0, "playercount", str("value", ">=20")))
	contains(t, plain, "the player count is >= 20")
	for _, got := range []string{plain, explicit} {
		if strings.Contains(got, "Tip:") || strings.Contains(got, "more than") {
			t.Errorf("no tip is needed when a plain number means what people expect: %q", got)
		}
	}
	ns, _ := e.st.UserNotifications("u1", "guild:g1")
	if len(ns) != 2 || !reflect.DeepEqual(ns[0].Conditions, ns[1].Conditions) {
		t.Fatalf("20 and >=20 should be the same condition: %+v", ns)
	}
	// 64 is allowed and 65 is not.
	contains(t, e.b.execute(nguild("u1", 0, "playercount", str("value", "64"))), "the player count is >= 64")
	contains(t, e.b.execute(nguild("u1", 0, "playercount", str("value", "65"))), "from 0 to 64")
}

// --- limits and the default cooldown ---

func TestAPlaceCanWatchThreeGameServers(t *testing.T) {
	if maxServersPerScope != 3 {
		t.Fatalf("limit %d, want 3", maxServersPerScope)
	}
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		contains(t, e.b.execute(guild("admin", manage, "server", subcommand("add", str("address", fmt.Sprintf("h%d.example:27015", i))))), "Now watching")
	}
	contains(t, e.b.execute(guild("admin", manage, "server", subcommand("add", str("address", "h3.example:27015")))), "at most 3")
	// Another Discord server has its own three.
	other := guild("admin", manage, "server", subcommand("add", str("address", "h3.example:27015")))
	other.GuildID = "g2"
	contains(t, e.b.execute(other), "Now watching")
	// Removing one makes room.
	e.b.execute(guild("admin", manage, "server", subcommand("remove", str("address", "h0.example:27015"))))
	contains(t, e.b.execute(guild("admin", manage, "server", subcommand("add", str("address", "h3.example:27015")))), "Now watching")
}

func TestEveryTypeSaysItStaysQuietFor60MinutesByDefault(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	for _, tc := range []struct {
		sub  string
		opts []opt
	}{
		{"map", []opt{str("map-name", "m")}}, {"player", []opt{str("username", "p")}}, {"playercount", []opt{str("value", "5")}},
		{"status", []opt{str("value", "offline")}}, {"title-query", []opt{str("value", "w")}},
	} {
		contains(t, e.b.execute(nguild("u1", 0, tc.sub, tc.opts...)), "Cooldown: 60 mins")
	}
	contains(t, e.b.execute(nguild("u1", 0, "list")), "cooldown 60 mins")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m2")))
	id := fmt.Sprint(ids(t, e, "u1")[5])
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", id), num("cooldown", 5))), "Cooldown: 5 min")
	contains(t, e.b.execute(nguild("u1", 0, "list")), "cooldown 5 mins")
}

// --- /notify reset ---

func TestResetAsksFirstThenDeletesOnlyYourNotificationsHere(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	other := guild("admin", manage, "server", subcommand("add", str("address", "z.example:27015")))
	other.GuildID = "g2"
	e.b.execute(other)

	for i := 0; i < 3; i++ {
		e.b.execute(nguild("u1", 0, "title-query", str("value", fmt.Sprint("w", i))))
	}
	e.b.execute(nguild("u2", 0, "title-query", str("value", "theirs")))
	elsewhere := nguild("u1", 0, "title-query", str("value", "other server"))
	elsewhere.GuildID = "g2"
	e.b.execute(elsewhere)

	in := nguild("u1", 0, "reset")
	r := e.b.executeReply(in)
	contains(t, r.content, "deletes all 3 of your notifications in this Discord server", "cannot be undone")
	if len(r.components) != 1 {
		t.Fatalf("components %v", r.components)
	}
	row := r.components[0].(discordgo.ActionsRow)
	if len(row.Components) != 2 {
		t.Fatalf("buttons %v", row.Components)
	}
	confirm, cancel := row.Components[0].(discordgo.Button), row.Components[1].(discordgo.Button)
	if confirm.CustomID != customResetConfirm || confirm.Style != discordgo.DangerButton || !strings.Contains(confirm.Label, "3") ||
		cancel.CustomID != customResetCancel {
		t.Errorf("buttons %+v %+v", confirm, cancel)
	}
	if len(ids(t, e, "u1")) != 3 {
		t.Fatal("asking must not delete anything")
	}

	press := func(custom string, user string) (string, *fakeResponder) {
		in := guild(user, 0, "notify")
		in.Type = discordgo.InteractionMessageComponent
		in.Data = discordgo.MessageComponentInteractionData{CustomID: custom}
		f := &fakeResponder{}
		e.b.component(f, in)
		return custom, f
	}
	// Cancel keeps everything and uses up the buttons.
	_, f := press(customResetCancel, "u1")
	if len(ids(t, e, "u1")) != 3 || len(f.data) != 1 || f.data[0].Content != "Nothing was deleted." || len(f.data[0].Components) != 0 {
		t.Fatalf("cancel: %+v", f.data)
	}
	if f.calls[0] != fmt.Sprint("respond:", discordgo.InteractionResponseUpdateMessage) {
		t.Errorf("the buttons message must be updated in place: %v", f.calls)
	}
	// Confirm deletes this user's notifications in this place, and nobody else's.
	_, f = press(customResetConfirm, "u1")
	contains(t, f.data[0].Content, "Deleted 3 notifications")
	if len(ids(t, e, "u1")) != 0 {
		t.Fatalf("left %v", ids(t, e, "u1"))
	}
	if len(ids(t, e, "u2")) != 1 {
		t.Error("another user's notification was deleted")
	}
	if ns, _ := e.st.UserNotifications("u1", "guild:g2"); len(ns) != 1 {
		t.Error("a notification in another Discord server was deleted")
	}
	// A second press finds nothing, and an unknown button does nothing.
	_, f = press(customResetConfirm, "u1")
	contains(t, f.data[0].Content, "nothing left")
	if _, f = press("something:else", "u1"); len(f.calls) != 0 {
		t.Errorf("an unknown button was answered: %v", f.calls)
	}
}

func TestResetWithNothingToDeleteHasNoButtons(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	r := e.b.executeReply(nguild("u1", 0, "reset"))
	contains(t, r.content, "no notifications here to delete")
	if len(r.components) != 0 {
		t.Errorf("buttons for nothing: %v", r.components)
	}
	// Other commands never get buttons.
	if r := e.b.executeReply(nguild("u1", 0, "list")); len(r.components) != 0 {
		t.Errorf("list has buttons: %v", r.components)
	}
}

func TestResetInADMSaysSo(t *testing.T) {
	e := newEnv(t)
	e.b.execute(dm("u1", "server", subcommand("add", str("address", "a.example:27015"))))
	e.b.execute(ndm("u1", "status", str("value", "offline")))
	contains(t, e.b.execute(ndm("u1", "reset")), "deletes all 1 of your notifications in your DMs with me")
}

func TestResetDoesNothingWhileTurnedOff(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "title-query", str("value", "event")))
	if r := e.b.executeReply(nguild("u1", 0, "reset")); len(r.components) != 1 {
		t.Fatalf("the question needs its buttons: %+v", r.components)
	}
	e.b.execute(admin("a", manage, str("active", "disabled")))

	r := e.b.executeReply(nguild("u1", 0, "reset"))
	contains(t, r.content, "turned off in this Discord server")
	if len(r.components) != 0 {
		t.Errorf("buttons while turned off: %+v", r.components)
	}
	// A button from before an admin turned sourcewatch off deletes nothing.
	in := guild("u1", 0, "notify")
	in.Type = discordgo.InteractionMessageComponent
	in.Data = discordgo.MessageComponentInteractionData{CustomID: customResetConfirm}
	got, ok := e.b.componentReply(in)
	if !ok {
		t.Fatal("the press was not answered")
	}
	contains(t, got, "turned off in this Discord server")
	if len(ids(t, e, "u1")) != 1 {
		t.Fatal("deleted notifications while turned off")
	}
}

func TestButtonsAreSentWithTheReply(t *testing.T) {
	f := &fakeResponder{}
	btn := resetButtons(2)
	answer(f, quietLog(), &discordgo.Interaction{}, false, func() reply { return reply{content: "sure?", components: btn} })
	if len(f.data) != 1 || len(f.data[0].Components) != 1 || f.data[0].Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Fatalf("%+v", f.data)
	}
	// Also when the answer takes long and is filled in afterwards.
	old := deferAfter
	deferAfter = 10 * time.Millisecond
	defer func() { deferAfter = old }()
	f = &fakeResponder{}
	answer(f, quietLog(), &discordgo.Interaction{}, false, func() reply { time.Sleep(50 * time.Millisecond); return reply{content: "sure?", components: btn} })
	if f.edit == nil || f.edit.Components == nil || len(*f.edit.Components) != 1 {
		t.Fatalf("edit %+v", f.edit)
	}
}

// --- /notify settings ---

func TestSettingsWithOnlyAnIDShowsTheCurrentSettings(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	create := nguild("u1", 0, "map", str("map-name", "de_dust2"))
	create.ChannelID = "4242" // real channel IDs are numbers
	e.b.execute(create)
	id := int(ids(t, e, "u1")[0])
	got := e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(id))))
	contains(t, got, fmt.Sprintf("Settings of notification ID: %d", id), "Active: enabled", "Delivery: in <#4242>",
		"Cooldown: 60 mins (the default)", "all your game servers", "the map is `de_dust2`")
	if strings.Contains(got, "saved") {
		t.Errorf("looking must not change anything: %q", got)
	}
}

func TestSettingsChangeCooldownActiveAndDelivery(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "de_dust2")))
	id := int(ids(t, e, "u1")[0])
	get := func() (n struct {
		cooldown int
		disabled bool
		delivery string
		channel  string
	}) {
		ns, _ := e.st.UserNotifications("u1", "guild:g1")
		x := ns[0]
		n.cooldown, n.disabled, n.delivery, n.channel = x.CooldownSec, x.Disabled, x.EffectiveDelivery(), x.ChannelID
		return
	}

	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(id)), num("cooldown", 10))),
		fmt.Sprintf("Settings saved for notification ID: %d", id), "Cooldown: 10 min")
	if g := get(); g.cooldown != 600 || g.disabled || g.delivery != "channel" {
		t.Fatalf("after cooldown: %+v", g)
	}
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(id)), str("active", "disabled"))), "Active: disabled", "will not fire")
	if g := get(); !g.disabled || g.cooldown != 600 {
		t.Fatalf("after disabling: %+v", g)
	}
	contains(t, e.b.execute(nguild("u1", 0, "list")), "disabled")
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(id)), str("active", "enabled"))), "Active: enabled")
	if g := get(); g.disabled {
		t.Fatalf("after enabling: %+v", g)
	}

	// Several at once.
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(id)), num("cooldown", 45), str("delivery", "dm"), str("active", "disabled"))),
		"Cooldown: 45 min", "Delivery: by DM", "Active: disabled")
	if g := get(); g.cooldown != 2700 || g.delivery != "dm" || !g.disabled {
		t.Fatalf("after several: %+v", g)
	}
}

func TestSettingsDeliveryToDMChecksTheDMAndToChannelUsesThisChannel(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m")))
	id := int(ids(t, e, "u1")[0])

	e.dm.err = notify.ErrDMClosed
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(id)), str("delivery", "dm"))), "can't send you a DM")
	if ns, _ := e.st.UserNotifications("u1", "guild:g1"); ns[0].EffectiveDelivery() != "channel" {
		t.Fatal("changed to DM although the DM was refused")
	}
	e.dm.err = nil
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(id)), str("delivery", "dm"))), "Delivery: by DM")
	if len(e.dm.probed) != 2 {
		t.Fatalf("probed %v", e.dm.probed)
	}

	// Back to a channel: the channel where the command is run, which is not the first one.
	in := nguild("u1", 0, "settings", str("id", fmt.Sprint(id)), str("delivery", "channel"))
	in.ChannelID = "777"
	contains(t, e.b.execute(in), "Delivery: in <#777>")
	if ns, _ := e.st.UserNotifications("u1", "guild:g1"); ns[0].ChannelID != "777" || ns[0].EffectiveDelivery() != "channel" {
		t.Fatalf("%+v", ns[0])
	}
	before := len(e.dm.probed)
	e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(id)), num("cooldown", 5)))
	if len(e.dm.probed) != before {
		t.Fatal("changing the cooldown must not send a DM")
	}
}

func TestSettingsInADMCannotUseAChannel(t *testing.T) {
	e := newEnv(t)
	e.b.execute(dm("u1", "server", subcommand("add", str("address", "a.example:27015"))))
	e.b.execute(ndm("u1", "status", str("value", "offline")))
	ns, _ := e.st.UserNotifications("u1", "user:u1")
	id := int(ns[0].ID)
	contains(t, e.b.execute(ndm("u1", "settings", str("id", fmt.Sprint(id)), str("delivery", "channel"))), "no channel in a DM")
	contains(t, e.b.execute(ndm("u1", "settings", str("id", fmt.Sprint(id)), num("cooldown", 12))), "Cooldown: 12 min", "Delivery: by DM")
}

func TestSettingsOnlyForYourOwnNotificationsHere(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u2", 0, "map", str("map-name", "m")))
	theirs := int(ids(t, e, "u2")[0])
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(theirs)), num("cooldown", 1))), "no notification with ID")
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(99999)), str("active", "disabled"))), "no notification with ID")
	if ns, _ := e.st.UserNotifications("u2", "guild:g1"); ns[0].CooldownSec != 0 || ns[0].Disabled {
		t.Fatalf("someone else's notification changed: %+v", ns[0])
	}
	// Without an id the settings are for all of the user's own notifications, never someone else's:
	// u1 has none here (u2 made the one that exists), so there is nothing to change.
	contains(t, e.b.execute(guild("u1", 0, "notify", subcommand("settings"))), "You have no notifications here.")
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(theirs)), str("active", "maybe"))), "no notification") // not found comes first
}

func TestSettingsRejectsUnknownChoices(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m")))
	id := int(ids(t, e, "u1")[0])
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(id)), str("active", "maybe"))), "`enabled` or `disabled`")
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(id)), str("delivery", "pigeon"))), "`channel` or `dm`")
}

func TestThereAreNoDeliveryOrCooldownOptionsToRefuse(t *testing.T) {
	// They are not options of the creation commands any more, so an id can never come with them.
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m")))
	id := int(ids(t, e, "u1")[0])
	for i, extra := range []opt{str("delivery", "dm"), num("cooldown", 9)} {
		// Discord would not even offer these; if one did arrive it is simply ignored.
		contains(t, e.b.execute(nguild("u1", 0, "title-query", str("value", fmt.Sprint("w", i)), num("id", id), extra)), "saved as ID")
	}
	ns, _ := e.st.UserNotifications("u1", "guild:g1")
	if ns[0].CooldownSec != 0 || ns[0].EffectiveDelivery() != "channel" {
		t.Fatalf("an option that does not exist changed the notification: %+v", ns[0])
	}
}

func TestSettingsCommandDefinition(t *testing.T) {
	notifyCmd, _ := find("notify")
	var settings *discordgo.ApplicationCommandOption
	for _, o := range notifyCmd.options {
		if o.Name == "settings" {
			settings = o
		}
	}
	if settings == nil {
		t.Fatal("no /notify settings")
	}
	byName := map[string]*discordgo.ApplicationCommandOption{}
	for _, o := range settings.Options {
		byName[o.Name] = o
	}
	if byName["id"].Type != discordgo.ApplicationCommandOptionString || !strings.Contains(byName["id"].Description, "all") {
		t.Errorf("id is a number or the word all, so it is text: %+v", byName["id"])
	}
	if settings.Options[0].Name != "id" || byName["id"].Required || !byName["id"].Autocomplete || !strings.Contains(byName["id"].Description, "default") {
		t.Errorf("id comes first, is optional (it defaults to all), and has suggestions: %+v", settings.Options[0])
	}
	for _, name := range []string{"cooldown", "active", "delivery"} {
		if byName[name] == nil || byName[name].Required {
			t.Errorf("%s must exist and be optional: %+v", name, byName[name])
		}
	}
	if byName["cooldown"].Type != discordgo.ApplicationCommandOptionInteger || *byName["cooldown"].MinValue != 1 {
		t.Errorf("cooldown is minutes, at least 1: %+v", byName["cooldown"])
	}
	var active []string
	for _, c := range byName["active"].Choices {
		active = append(active, fmt.Sprint(c.Value))
	}
	if !reflect.DeepEqual(active, []string{"enabled", "disabled"}) {
		t.Errorf("active choices %v", active)
	}
	if _, ok := byName["server"]; ok {
		t.Error("the game server cannot be changed")
	}
}

func TestSettingsIDSuggestsYourNotifications(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m")))
	e.b.execute(nguild("u2", 0, "map", str("map-name", "other")))
	o := &discordgo.ApplicationCommandInteractionDataOption{Name: "id", Type: discordgo.ApplicationCommandOptionString, Value: "", Focused: true}
	in := nguild("u1", 0, "settings", o)
	in.Type = discordgo.InteractionApplicationCommandAutocomplete
	got := e.b.autocomplete(in)
	if len(got) != 2 || got[0].Value != "all" || !strings.Contains(got[0].Name, "every notification") ||
		got[1].Value != fmt.Sprint(ids(t, e, "u1")[0]) {
		t.Fatalf("choices %+v", got)
	}
	// With nothing to choose from there is no "all" either.
	o2 := &discordgo.ApplicationCommandInteractionDataOption{Name: "id", Type: discordgo.ApplicationCommandOptionString, Value: "", Focused: true}
	empty := nguild("nobody", 0, "settings", o2)
	empty.Type = discordgo.InteractionApplicationCommandAutocomplete
	if got := e.b.autocomplete(empty); len(got) != 0 {
		t.Fatalf("choices for a user with no notifications: %+v", got)
	}
}

// --- /notify settings id:all ---

func TestSettingsAllAppliesToEveryNotificationHere(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m1")))
	e.b.execute(nguild("u1", 0, "player", str("username", "p")))
	e.b.execute(nguild("u1", 0, "status", str("value", "offline")))
	e.b.execute(nguild("u2", 0, "map", str("map-name", "theirs")))
	other := nguild("u1", 0, "title-query", str("value", "elsewhere"))
	other.GuildID = "g2"
	e.b.execute(guildServer(e, "g2", "z.example:27015")) // a second Discord server
	e.b.execute(other)

	got := e.b.execute(nguild("u1", 0, "settings", str("id", "all"), str("active", "disabled"), num("cooldown", 7)))
	contains(t, got, "Settings saved for all 3 notifications", "Active: disabled", "Cooldown: 7 min")
	ns, _ := e.st.UserNotifications("u1", "guild:g1")
	if len(ns) != 3 {
		t.Fatalf("%d notifications", len(ns))
	}
	for _, n := range ns {
		if !n.Disabled || n.CooldownSec != 420 {
			t.Errorf("not changed: %+v", n)
		}
	}
	if theirs, _ := e.st.UserNotifications("u2", "guild:g1"); theirs[0].Disabled || theirs[0].CooldownSec != 0 {
		t.Errorf("another user's notification changed: %+v", theirs[0])
	}
	if elsewhere, _ := e.st.UserNotifications("u1", "guild:g2"); len(elsewhere) != 1 || elsewhere[0].Disabled {
		t.Errorf("a notification in another Discord server changed: %+v", elsewhere)
	}
	// Turn them all back on.
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", "ALL"), str("active", "enabled"))), "Active: enabled")
	for _, n := range func() []rules.Notification { x, _ := e.st.UserNotifications("u1", "guild:g1"); return x }() {
		if n.Disabled {
			t.Errorf("still disabled: %+v", n)
		}
	}
}

// guildServer adds a game server to another Discord server and returns the interaction used.
func guildServer(e *env, guildID, addr string) *discordgo.Interaction {
	in := guild("admin", manage, "server", subcommand("add", str("address", addr)))
	in.GuildID = guildID
	return in
}

func TestSettingsAllDeliveryChecksTheDMOnceAndUsesThisChannel(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m1")))
	e.b.execute(nguild("u1", 0, "status", str("value", "offline")))

	e.dm.err = notify.ErrDMClosed
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", "all"), str("delivery", "dm"))), "can't send you a DM")
	for _, n := range func() []rules.Notification { x, _ := e.st.UserNotifications("u1", "guild:g1"); return x }() {
		if n.EffectiveDelivery() != "channel" {
			t.Fatal("changed although the DM was refused")
		}
	}
	e.dm.err = nil
	before := len(e.dm.probed)
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", "all"), str("delivery", "dm"))), "Delivery: by DM")
	if len(e.dm.probed)-before != 1 {
		t.Fatalf("one test DM for the whole change, got %d", len(e.dm.probed)-before)
	}
	in := nguild("u1", 0, "settings", str("id", "all"), str("delivery", "channel"))
	in.ChannelID = "999"
	contains(t, e.b.execute(in), "Delivery: in <#999>")
	for _, n := range func() []rules.Notification { x, _ := e.st.UserNotifications("u1", "guild:g1"); return x }() {
		if n.ChannelID != "999" || n.EffectiveDelivery() != "channel" {
			t.Errorf("%+v", n)
		}
	}
}

func TestSettingsAllWithoutSettingsGivesASummary(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", "all"), str("active", "disabled"))), "no notifications here")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m1")))
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m2")))
	id := ids(t, e, "u1")[0]
	e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(id)), str("active", "disabled")))
	got := e.b.execute(nguild("u1", 0, "settings", str("id", "all")))
	contains(t, got, "2 notifications here: 1 enabled, 1 disabled", "cooldown", "active", "delivery")
	if strings.Contains(got, "saved") {
		t.Errorf("looking must not change anything: %q", got)
	}
}

func TestSettingsAllInADMRefusesAChannel(t *testing.T) {
	e := newEnv(t)
	e.b.execute(dm("u1", "server", subcommand("add", str("address", "a.example:27015"))))
	e.b.execute(ndm("u1", "status", str("value", "offline")))
	contains(t, e.b.execute(ndm("u1", "settings", str("id", "all"), str("delivery", "channel"))), "no channel in a DM")
	contains(t, e.b.execute(ndm("u1", "settings", str("id", "all"), num("cooldown", 3))), "all 1 notifications", "Cooldown: 3 min")
}

func TestSettingsIDAcceptsHashAndRejectsOtherText(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m")))
	id := ids(t, e, "u1")[0]
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprintf("#%d", id)), num("cooldown", 9))), "Cooldown: 9 min")
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", " "+fmt.Sprint(id)+" "), num("cooldown", 8))), "Cooldown: 8 min")
	for _, bad := range []string{"banana", "1.5", "-2x", "everything"} {
		contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", bad), num("cooldown", 1))), "a notification's number, or `all`")
	}
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", ""))), "1 notifications here")
	if ns, _ := e.st.UserNotifications("u1", "guild:g1"); ns[0].CooldownSec != 480 {
		t.Fatalf("a bad id changed something: %+v", ns[0])
	}
}

// --- /notify settings without an id means all ---

func TestSettingsWithoutAnIDAppliesToAll(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m1")))
	e.b.execute(nguild("u1", 0, "status", str("value", "offline")))
	e.b.execute(nguild("u2", 0, "map", str("map-name", "theirs")))

	contains(t, e.b.execute(nguild("u1", 0, "settings", str("active", "disabled"))),
		"Settings saved for all 2 notifications", "Active: disabled")
	for _, n := range func() []rules.Notification { x, _ := e.st.UserNotifications("u1", "guild:g1"); return x }() {
		if !n.Disabled {
			t.Errorf("not disabled: %+v", n)
		}
	}
	if theirs, _ := e.st.UserNotifications("u2", "guild:g1"); theirs[0].Disabled {
		t.Error("another user's notification was changed")
	}
	// The same as asking for all.
	contains(t, e.b.execute(nguild("u1", 0, "settings", num("cooldown", 15))), "all 2 notifications", "Cooldown: 15 min")
	// And with nothing at all it is the summary.
	got := e.b.execute(nguild("u1", 0, "settings"))
	contains(t, got, "2 notifications here: 0 enabled, 2 disabled", "add an `id` to change just one")
	// An id still picks one.
	id := ids(t, e, "u1")[0]
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(id)), str("active", "enabled"))), fmt.Sprintf("notification ID: %d", id))
	enabled := 0
	for _, n := range func() []rules.Notification { x, _ := e.st.UserNotifications("u1", "guild:g1"); return x }() {
		if !n.Disabled {
			enabled++
		}
	}
	if enabled != 1 {
		t.Errorf("%d enabled, want just the one that was picked", enabled)
	}
}

// --- /status all and /status players ---

func TestStatusIsOneCommandWithNoOptionsAndIsPublic(t *testing.T) {
	status, ok := find("status")
	if !ok {
		t.Fatal("no /status")
	}
	if len(status.options) != 0 {
		t.Errorf("/status takes no options and has no subcommands: %+v", status.options)
	}
	if !status.public {
		t.Error("/status is shown to everyone in the channel")
	}
	for _, c := range registry() {
		if c.name != "status" && c.public {
			t.Errorf("/%s must stay private to whoever ran it", c.name)
		}
	}
	for _, c := range commands() {
		if c.Name == "status" && len(c.Options) != 0 {
			t.Errorf("registered with options: %+v", c.Options)
		}
	}
}

func TestPublicRepliesAreNotEphemeralButStillSafe(t *testing.T) {
	pub := replyDataWith("Zombie | discord.gg/nide", nil, true)
	if pub.Flags&discordgo.MessageFlagsEphemeral != 0 {
		t.Error("a public reply must not be ephemeral")
	}
	if pub.Flags&discordgo.MessageFlagsSuppressEmbeds == 0 {
		t.Error("link previews stay off in a public reply, or invite links in names become cards for everyone")
	}
	if pub.AllowedMentions == nil || len(pub.AllowedMentions.Parse) != 0 || len(pub.AllowedMentions.Users) != 0 {
		t.Errorf("a public reply can reach many people, so it must not allow any ping: %+v", pub.AllowedMentions)
	}
	if priv := replyData("x"); priv.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Error("the default is private")
	}

	// Quick answer.
	f := &fakeResponder{}
	answer(f, quietLog(), &discordgo.Interaction{}, true, func() reply { return reply{content: "status"} })
	if len(f.data) != 1 || f.data[0].Flags&discordgo.MessageFlagsEphemeral != 0 {
		t.Fatalf("quick public answer: %+v", f.data)
	}
	// A slow one is public from the acknowledgement on, because that decides who sees the reply.
	old := deferAfter
	deferAfter = 10 * time.Millisecond
	defer func() { deferAfter = old }()
	f = &fakeResponder{}
	answer(f, quietLog(), &discordgo.Interaction{}, true, func() reply { time.Sleep(50 * time.Millisecond); return reply{content: "status"} })
	if len(f.data) != 1 || f.data[0].Flags&discordgo.MessageFlagsEphemeral != 0 || f.edit == nil {
		t.Fatalf("slow public answer: %+v / %+v", f.data, f.edit)
	}
}

func TestPlayersForAllServersStaysUnderDiscordsLimit(t *testing.T) {
	e := newEnv(t)
	var names []string
	for i := 0; i < 64; i++ {
		names = append(names, fmt.Sprintf("player_with_a_very_long_name_%02d", i))
	}
	setupGuild(t, e, "a.example:27015", "b.example:27015", "c.example:27015")
	for _, s := range mustServers(t, e) {
		applyOnline(e, s.ID, "Busy "+s.Addr, 64, names...)
	}
	got := e.b.execute(guild("u1", 0, "status"))
	if n := len([]rune(got)); n > 2000 {
		t.Fatalf("reply has %d characters", n)
	}
	for _, s := range mustServers(t, e) {
		if !strings.Contains(got, s.Addr) {
			t.Errorf("server %s missing", s.Addr)
		}
	}
	if strings.Count(got, "```")%2 != 0 {
		t.Errorf("unbalanced code block")
	}
}

// --- /server add and /server remove ---

func TestServerIsAddAndRemoveSubcommands(t *testing.T) {
	server, ok := find("server")
	if !ok {
		t.Fatal("no /server")
	}
	byName := map[string]*discordgo.ApplicationCommandOption{}
	var names []string
	for _, o := range server.options {
		if o.Type != discordgo.ApplicationCommandOptionSubCommand {
			t.Errorf("/server %s is not a subcommand", o.Name)
		}
		names = append(names, o.Name)
		byName[o.Name] = o
	}
	if !reflect.DeepEqual(names, []string{"add", "remove"}) {
		t.Fatalf("/server subcommands %v", names)
	}
	for _, name := range names {
		opts := byName[name].Options
		if len(opts) != 1 || opts[0].Name != "address" || !opts[0].Required || opts[0].Type != discordgo.ApplicationCommandOptionString {
			t.Errorf("/server %s takes one required address: %+v", name, opts)
		}
	}
	if byName["add"].Options[0].Autocomplete {
		t.Error("a new address cannot be suggested")
	}
	if !byName["remove"].Options[0].Autocomplete {
		t.Error("removing picks from the servers you watch, so it needs suggestions")
	}
	for _, c := range server.options {
		for _, o := range c.Options {
			if o.Name == "action" {
				t.Error("the old action option is still there")
			}
		}
	}
}

func TestServerWithoutASubcommandExplainsIt(t *testing.T) {
	e := newEnv(t)
	contains(t, e.b.execute(dm("u1", "server")), "`/server add`", "`/server remove`")
	contains(t, e.b.execute(dm("u1", "server", subcommand("nope"))), "`/server add`")
	// Without the permission nothing is explained or done.
	contains(t, e.b.execute(guild("u1", 0, "server")), "Manage Server")
}

func TestServerRemoveSuggestsTheServersYouWatch(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015", "b.example:27015")
	o := &discordgo.ApplicationCommandInteractionDataOption{Name: "address", Type: discordgo.ApplicationCommandOptionString, Value: "b.", Focused: true}
	in := guild("admin", manage, "server", subcommand("remove", o))
	in.Type = discordgo.InteractionApplicationCommandAutocomplete
	if got := e.b.autocomplete(in); len(got) != 1 || got[0].Value != "b.example:27015" {
		t.Fatalf("choices %+v", got)
	}
	// Adding gets no suggestions.
	add := guild("admin", manage, "server", subcommand("add", o))
	add.Type = discordgo.InteractionApplicationCommandAutocomplete
	if got := e.b.autocomplete(add); len(got) != 0 {
		t.Fatalf("suggestions for a new address: %+v", got)
	}
}

// --- wording of the list ---

func TestListSaysHowEachNotificationIsDeliveredAndItsCooldown(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m1")))
	e.b.execute(nguild("u1", 0, "status", str("value", "offline")))
	id := fmt.Sprint(ids(t, e, "u1")[1])
	e.b.execute(nguild("u1", 0, "settings", str("id", id), str("delivery", "dm"), num("cooldown", 5)))
	e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(ids(t, e, "u1")[0])), num("cooldown", 1)))
	got := e.b.execute(nguild("u1", 0, "list"))
	contains(t, got, "(via channel, cooldown 1 min)", "(via DM, cooldown 5 mins)")
	if strings.Contains(got, "quiet") || strings.Contains(got, "after firing") {
		t.Errorf("old wording: %q", got)
	}
	e.b.execute(nguild("u1", 0, "settings", str("id", id), str("active", "disabled")))
	contains(t, e.b.execute(nguild("u1", 0, "list")), "(via DM, cooldown 5 mins, disabled)")
}

func TestListShowsAllServersFirstAndOnce(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015", "b.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m1"), str("server", "b.example:27015")))
	e.b.execute(nguild("u1", 0, "status", str("value", "offline")))
	e.b.execute(nguild("u1", 0, "title-query", str("value", "event")))
	got := e.b.execute(nguild("u1", 0, "list"))
	contains(t, got, "**Your notifications** (3)", "**All servers** (2)", "**Server:** `b.example:27015`")
	if strings.Count(got, "All servers") != 1 {
		t.Errorf("the heading appears once: %q", got)
	}
	if strings.Index(got, "All servers") > strings.Index(got, "b.example:27015") {
		t.Errorf("All servers comes first: %q", got)
	}
	// Both notifications for all servers sit under that one heading, ahead of the server's own.
	if strings.Index(got, "the server is offline") > strings.Index(got, "b.example:27015") ||
		strings.Index(got, "contains `event`") > strings.Index(got, "b.example:27015") {
		t.Errorf("not grouped: %q", got)
	}
}

// --- /notify show ---

func TestShowIsAPublicListThatMentionsNoOneAndKeepsPrivateThingsOut(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "status", str("value", "offline"), str("server", "a.example:27015")))
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m")))
	e.st.MarkUndeliverable(ids(t, e, "u1")[0]) // something that would be told to the user privately

	show := nguild("u1", 0, "show")
	list := nguild("u1", 0, "list")
	if !isPublic(show) || isPublic(list) {
		t.Fatalf("show is public (%v) and list is private (%v)", isPublic(show), isPublic(list))
	}
	got := e.b.execute(show)
	contains(t, got, "**Notifications of <@u1>** (2)", "**All servers**", "the map is `m`", "the server is offline")
	if strings.Contains(got, "switched them back on") || strings.Contains(got, "Delete one with") {
		t.Errorf("a public message must not carry the owner's private notices: %q", got)
	}
	// The same content is what list shows to its owner, who is told about failures.
	private := e.b.execute(list)
	contains(t, private, "**Your notifications** (2)", "could not be posted in their channel", "switched them back on", "Delete one with")
	// Its reply is not ephemeral and cannot ping.
	d := replyDataWith(got, nil, isPublic(show))
	if d.Flags&discordgo.MessageFlagsEphemeral != 0 || len(d.AllowedMentions.Parse) != 0 || len(d.AllowedMentions.Users) != 0 {
		t.Errorf("%+v", d)
	}
	// Only show is public among the notify commands.
	for _, sub := range []string{"list", "remove", "settings", "reset", "map", "status"} {
		if isPublic(nguild("u1", 0, sub)) {
			t.Errorf("/notify %s must stay private", sub)
		}
	}
}

func TestShowWithNothingToShow(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	contains(t, e.b.execute(nguild("u1", 0, "show")), "no notifications")
}

// --- /admin settings ---

func admin(user string, perms int64, options ...opt) *discordgo.Interaction {
	return guild(user, perms, "admin", subcommand("settings", options...))
}

func TestAdminSettingsNeedsManageServerInADiscordServer(t *testing.T) {
	e := newEnv(t)
	contains(t, e.b.execute(admin("u1", 0)), "Manage Server")
	contains(t, e.b.execute(admin("u1", 0, str("active", "disabled"))), "Manage Server")
	if g, _ := e.st.GuildSettings("guild:g1"); !g.Active {
		t.Fatal("changed without permission")
	}
	contains(t, e.b.execute(dm("u1", "admin", subcommand("settings"))), "for Discord servers")
	contains(t, e.b.execute(admin("u1", manage)), "sourcewatch settings for this Discord server")
}

func TestAdminSettingsShowsTheDefaultsAndChangesEachSetting(t *testing.T) {
	e := newEnv(t)
	got := e.b.execute(admin("a", manage))
	contains(t, got, "Active: enabled", "Channel delivery: enabled", "Default delivery of new notifications: via channel",
		"Default cooldown of new notifications: 60 mins (the default)", "Blacklisted players: none", "`blacklist-add`")

	contains(t, e.b.execute(admin("a", manage, str("channel-delivery", "disabled"))), "Settings saved", "Channel delivery: **disabled**", "notifications go by DM", "via DM")
	contains(t, e.b.execute(admin("a", manage, str("channel-delivery", "enabled"), str("default-delivery", "dm"))), "Channel delivery: enabled", "via DM")
	contains(t, e.b.execute(admin("a", manage, str("default-delivery", "channel"), num("default-cooldown", 15))), "via channel", "Default cooldown of new notifications: 15 mins")
	g, _ := e.st.GuildSettings("guild:g1")
	if !g.ChannelDelivery || g.DefaultDelivery != "channel" || g.DefaultCooldownSec != 900 || !g.Active {
		t.Fatalf("%+v", g)
	}
	contains(t, e.b.execute(admin("a", manage, str("active", "disabled"))), "Active: **disabled**")
	// Wrong choices are refused and change nothing.
	contains(t, e.b.execute(admin("a", manage, str("active", "maybe"))), "`enabled` or `disabled`")
	contains(t, e.b.execute(admin("a", manage, str("channel-delivery", "yes"))), "`enabled` or `disabled`")
	contains(t, e.b.execute(admin("a", manage, str("default-delivery", "pigeon"))), "`channel` or `dm`")
	if g2, _ := e.st.GuildSettings("guild:g1"); g2.Active || !g2.ChannelDelivery || g2.DefaultDelivery != "channel" || g2.DefaultCooldownSec != 900 {
		t.Fatalf("a refused change altered the settings: %+v", g2)
	}
	// Another Discord server keeps its own.
	other := admin("a", manage)
	other.GuildID = "g2"
	contains(t, e.b.execute(other), "Active: enabled")
}

func TestTurningTheBotOffInADiscordServerBlocksEverythingButAdmin(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(admin("a", manage, str("active", "disabled")))
	for _, in := range []*discordgo.Interaction{
		nguild("u1", 0, "status", str("value", "offline")), nguild("u1", 0, "list"), nguild("u1", 0, "show"),
		guild("u1", 0, "status"), guild("u1", 0, "help"),
		guild("admin", manage, "server", subcommand("add", str("address", "x.example:27015"))),
	} {
		contains(t, e.b.execute(in), "turned off in this Discord server", "/admin settings")
	}
	if len(ids(t, e, "u1")) != 0 {
		t.Fatal("made a notification while turned off")
	}
	// Autocomplete says nothing either.
	o := str("map-name", "")
	o.Focused = true
	ac := nguild("u1", 0, "map", o)
	ac.Type = discordgo.InteractionApplicationCommandAutocomplete
	if got := e.b.autocomplete(ac); got != nil {
		t.Errorf("suggestions while turned off: %v", got)
	}
	// Admin settings still works, and turning it on brings everything back.
	contains(t, e.b.execute(admin("a", manage)), "Active: **disabled**")
	e.b.execute(admin("a", manage, str("active", "enabled")))
	contains(t, e.b.execute(nguild("u1", 0, "status", str("value", "offline"))), "saved as ID")
	// A DM with the bot is not a Discord server, so it is not affected.
	e.b.execute(admin("a", manage, str("active", "disabled")))
	contains(t, e.b.execute(dm("u1", "help")), "Quick start")
}

func TestDefaultDeliveryAndCooldownApplyToNewNotificationsOnly(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "before")))
	e.b.execute(admin("a", manage, str("default-delivery", "dm"), num("default-cooldown", 10)))
	e.dm.err = notify.ErrDMClosed
	contains(t, e.b.execute(nguild("u1", 0, "map", str("map-name", "after"))), "can't send you a DM") // a DM default means a test DM
	e.dm.err = nil
	got := e.b.execute(nguild("u1", 0, "map", str("map-name", "after")))
	contains(t, got, "by DM", "Cooldown: 10 mins")
	ns, _ := e.st.UserNotifications("u1", "guild:g1")
	if len(ns) != 2 || ns[0].EffectiveDelivery() != "channel" || ns[0].CooldownSec != 0 {
		t.Fatalf("the earlier one must not change: %+v", ns[0])
	}
	if ns[1].EffectiveDelivery() != "dm" || ns[1].CooldownSec != 600 {
		t.Fatalf("the new one uses the defaults: %+v", ns[1])
	}
	// A notification can still be changed one by one.
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", fmt.Sprint(ns[1].ID)), num("cooldown", 3), str("delivery", "channel"))), "Cooldown: 3 mins", "Delivery: in a channel")
}

func TestChannelDeliveryOffSendsNewNotificationsByDMAndRefusesChannel(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m1")))
	e.b.execute(admin("a", manage, str("channel-delivery", "disabled")))
	got := e.b.execute(nguild("u1", 0, "status", str("value", "offline")))
	contains(t, got, "by DM")
	if len(e.dm.probed) == 0 {
		t.Error("a DM notification needs a test DM")
	}
	id := fmt.Sprint(ids(t, e, "u1")[0]) // the old one still says channel
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", id), str("delivery", "channel"))), "turned channel delivery off")
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("delivery", "channel"))), "turned channel delivery off")
	// Turned on again, channel is possible.
	e.b.execute(admin("a", manage, str("channel-delivery", "enabled")))
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", id), str("delivery", "channel"))), "Delivery: in a channel")
}

func TestBlacklistedPlayersCannotBeTrackedOrListed(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	ref := mustServer(t, e, "guild:g1")
	applyOnline(e, ref.ID, "Alpha", 3, "Chujoy", "alice", "bob")
	e.b.execute(nguild("u1", 0, "player", str("username", "Chujoy"))) // made before the name was blacklisted

	got := e.b.execute(admin("a", manage, str("blacklist-add", "  chujoy ")))
	contains(t, got, "Settings saved", "Blacklisted players (1)", "`chujoy`", "nobody here can track that name")
	contains(t, e.b.execute(admin("a", manage, str("blacklist-add", "CHUJOY"))), "already on the blacklist")

	// Nobody can make a notification for the name, on its own or added to another one.
	contains(t, e.b.execute(nguild("u1", 0, "player", str("username", "Chujoy"))), "cannot be tracked in this Discord server", "blacklist")
	e.b.execute(nguild("u1", 0, "map", str("map-name", "m")))
	mapID := int(ids(t, e, "u1")[1])
	contains(t, e.b.execute(nguild("u1", 0, "player", str("username", "CHUJOY"), num("id", mapID))), "cannot be tracked")
	contains(t, e.b.execute(nguild("u1", 0, "player", str("username", "alice"))), "saved as ID") // others are fine
	if ns, _ := e.st.UserNotifications("u1", "guild:g1"); len(ns[1].Conditions) != 1 {
		t.Fatalf("the blacklisted name was added: %+v", ns[1].Conditions)
	}

	// The name does not show in /status, and is not a gap that gives it away.
	status := e.b.execute(guild("u1", 0, "status"))
	contains(t, status, "alice, bob", "3/64 players")
	if strings.Contains(strings.ToLower(status), "chujoy") {
		t.Errorf("a blacklisted name is shown: %q", status)
	}
	// If every name is blacklisted a placeholder says there are players.
	applyOnline(e, ref.ID, "Alpha", 1, "Chujoy")
	contains(t, e.b.execute(guild("u1", 0, "status")), "(hidden)")
	// Another Discord server sees the name, since the blacklist is per Discord server.
	otherStatus := guild("u1", 0, "status")
	otherStatus.GuildID = "g2"
	e.b.execute(guildServerCmd("g2", "a.example:27015"))
	if got := e.b.execute(otherStatus); !strings.Contains(got, "Chujoy") {
		t.Errorf("the blacklist leaked to another Discord server: %q", got)
	}

	// Taking it off lets it be tracked again.
	contains(t, e.b.execute(admin("a", manage, str("blacklist-remove", "Chujoy"))), "off the blacklist", "Blacklisted players: none")
	contains(t, e.b.execute(admin("a", manage, str("blacklist-remove", "Chujoy"))), "was not on the blacklist")
	contains(t, e.b.execute(nguild("u1", 0, "player", str("username", "Chujoy"))), "saved as ID")
}

func guildServerCmd(guildID, addr string) *discordgo.Interaction {
	in := guild("admin", manage, "server", subcommand("add", str("address", addr)))
	in.GuildID = guildID
	return in
}

func TestBlacklistRemoveSuggestsOnlyBlacklistedNamesToAdmins(t *testing.T) {
	e := newEnv(t)
	e.b.execute(admin("a", manage, str("blacklist-add", "Chujoy")))
	e.b.execute(admin("a", manage, str("blacklist-add", "alice")))
	o := &discordgo.ApplicationCommandInteractionDataOption{Name: "blacklist-remove", Type: discordgo.ApplicationCommandOptionString, Value: "chu", Focused: true}
	in := guild("a", manage, "admin", subcommand("settings", o))
	in.Type = discordgo.InteractionApplicationCommandAutocomplete
	if got := e.b.autocomplete(in); len(got) != 1 || got[0].Value != "Chujoy" {
		t.Fatalf("choices %+v", got)
	}
	// Someone without the permission gets nothing, so the list is not leaked.
	plain := guild("u1", 0, "admin", subcommand("settings", o))
	plain.Type = discordgo.InteractionApplicationCommandAutocomplete
	if got := e.b.autocomplete(plain); len(got) != 0 {
		t.Fatalf("leaked the blacklist: %+v", got)
	}
}

func TestAdminGameTrackerSettingAndStatus(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	contains(t, e.b.execute(guild("admin", manage, "admin", subcommand("settings"))), "GameTracker fallback: enabled")
	got := e.b.execute(guild("admin", manage, "admin", subcommand("settings", str("gametracker", "disabled"))))
	contains(t, got, "Settings saved", "GameTracker fallback: **disabled**")

	srvs, _ := e.st.ListenerServers("guild:g1")
	snap := rules.Snapshot{Online: true, Title: "Zombie", Map: "m", Players: 3, MaxPlayers: 64, NamesKnown: true, Source: "gametracker", DataAt: time.Now()}
	e.st.Apply(srvs[0].ID, snap, time.Now())
	contains(t, e.b.execute(guild("u1", 0, "status")), "no direct answer", "GameTracker is turned off")
	if strings.Contains(e.b.execute(guild("u1", 0, "status")), "Zombie") {
		t.Error("data from GameTracker is shown in a Discord server that turned it off")
	}
	// Direct data is shown as usual, and turning it on again shows GameTracker's.
	e.b.execute(guild("admin", manage, "admin", subcommand("settings", str("gametracker", "enabled"))))
	contains(t, e.b.execute(guild("u1", 0, "status")), "Zombie", "Via GameTracker")
}

func TestAdminCommandDefinition(t *testing.T) {
	admin, ok := find("admin")
	if !ok || len(admin.options) != 1 || admin.options[0].Name != "settings" || admin.options[0].Type != discordgo.ApplicationCommandOptionSubCommand {
		t.Fatalf("/admin has one subcommand, settings: %+v", admin.options)
	}
	var names []string
	for _, o := range admin.options[0].Options {
		if o.Required {
			t.Errorf("%s must be optional, so /admin settings alone shows the settings", o.Name)
		}
		names = append(names, o.Name)
	}
	want := []string{"channel-delivery", "default-delivery", "default-cooldown", "active", "gametracker", "blacklist-add", "blacklist-remove"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("options %v, want %v", names, want)
	}
	if admin.public {
		t.Error("admin replies are private")
	}
	help := newEnv(t).b.execute(dm("u1", "help"))
	contains(t, help, "`/admin`")
}

// --- IDs are reused ---

func TestAnIDYouRemoveIsGivenToTheNextNotification(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	for i := 1; i <= 3; i++ {
		contains(t, e.b.execute(nguild("u1", 0, "title-query", str("value", fmt.Sprint("w", i)))), fmt.Sprintf("Notification saved as ID: %d", i))
	}
	contains(t, e.b.execute(nguild("u1", 0, "remove", num("id", 1))), "Deleted notification ID: 1")
	// The next one takes 1 again, not 4.
	contains(t, e.b.execute(nguild("u1", 0, "map", str("map-name", "m"))), "Notification saved as ID: 1")
	contains(t, e.b.execute(nguild("u1", 0, "remove", num("id", 2))), "Deleted notification ID: 2")
	contains(t, e.b.execute(nguild("u1", 0, "status", str("value", "offline"))), "Notification saved as ID: 2")
	contains(t, e.b.execute(nguild("u1", 0, "status", str("value", "online"))), "Notification saved as ID: 4")
	// The reused ID is a new, separate notification that can be combined with and changed.
	contains(t, e.b.execute(nguild("u1", 0, "player", str("username", "alice"), num("id", 1))), "Notification saved as ID: 1", "all of these are true")
	contains(t, e.b.execute(nguild("u1", 0, "settings", str("id", "1"), num("cooldown", 5))), "Cooldown: 5 mins")
}

func TestResetStartsNumberingAtOneAgain(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	for i := 0; i < 3; i++ {
		e.b.execute(nguild("u1", 0, "title-query", str("value", fmt.Sprint("w", i))))
	}
	e.st.DeleteAllNotifications("u1", "guild:g1")
	contains(t, e.b.execute(nguild("u1", 0, "map", str("map-name", "m"))), "Notification saved as ID: 1")
}

func TestTheDefaultCooldownIsSixtyMinutes(t *testing.T) {
	e := newEnv(t)
	setupGuild(t, e, "a.example:27015")
	contains(t, e.b.execute(nguild("u1", 0, "map", str("map-name", "m"))), "Cooldown: 60 mins")
	contains(t, e.b.execute(nguild("u1", 0, "list")), "cooldown 60 mins")
	contains(t, e.b.execute(admin("a", manage)), "Default cooldown of new notifications: 60 mins (the default)")
	var desc string
	adminCmd, _ := find("admin")
	for _, o := range adminCmd.options[0].Options {
		if o.Name == "default-cooldown" {
			desc = o.Description
		}
	}
	if !strings.Contains(desc, "default: 60") {
		t.Errorf("the option says %q", desc)
	}
}
