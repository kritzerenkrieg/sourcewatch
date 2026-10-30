package rules

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func snap(title, mp string, players int, names ...string) Snapshot {
	return Snapshot{Online: true, Title: title, Map: mp, Players: players, MaxPlayers: 64, PlayerNames: names, NamesKnown: true}
}

var down = Snapshot{}

// notif builds a channel notification from conditions, in PhaseNew like a fresh one.
func notif(conds ...Condition) Notification {
	return Notification{ID: 1, UserID: "u", ServerID: 1, ChannelID: "c", Conditions: conds}
}

func mapIs(m string) Condition     { return Condition{Kind: KindMap, Text: m} }
func hasPlayer(p string) Condition { return Condition{Kind: KindPlayer, Text: p} }
func titleHas(t string) Condition  { return Condition{Kind: KindTitle, Text: t} }
func isOnline(b bool) Condition    { return Condition{Kind: KindStatus, Online: b} }
func players(op string, n int) Condition {
	return Condition{Kind: KindPlayerCount, Op: op, N: n}
}

// quick gives a notification a cooldown of 1 s, since run's steps are a minute apart.
func quick(n Notification) Notification { n.CooldownSec = 1; return n }

// run feeds states to a notification, one a minute, and returns at which steps it fired.
func run(n Notification, states ...Snapshot) (fired []int, last Notification) {
	for i, st := range states {
		var ev *Event
		n, ev = Evaluate(st, n, t0.Add(time.Duration(i)*time.Minute))
		if ev != nil {
			fired = append(fired, i)
		}
	}
	return fired, n
}

func TestFiresWhenTheConditionBecomesTrue(t *testing.T) {
	a, b := snap("T", "de_dust2", 1, "alice"), snap("T", "de_inferno", 1, "alice")
	for _, tc := range []struct {
		name   string
		n      Notification
		states []Snapshot
		want   []int
	}{
		{"map: changes to it, then not again while it stays", notif(mapIs("DE_INFERNO")), []Snapshot{a, a, b, b, b}, []int{2}},
		{"map: not for a different map", notif(mapIs("de_nuke")), []Snapshot{a, b, a, b}, nil},
		{"map: leaving and coming back fires again", notif(mapIs("de_inferno")), []Snapshot{a, b, a, b}, []int{1, 3}},
		{"map: already on it when created does not fire", notif(mapIs("de_dust2")), []Snapshot{a, a, a}, nil},
		{"player: joining", notif(hasPlayer("BOB")), []Snapshot{a, snap("T", "m", 2, "alice", "bob"), snap("T", "m", 2, "alice", "bob")}, []int{1}},
		{"player: exact name, not a part of one", notif(hasPlayer("bo")), []Snapshot{a, snap("T", "m", 2, "alice", "bob")}, nil},
		{"player: leaving and rejoining fires again", notif(hasPlayer("bob")), []Snapshot{a, snap("T", "m", 1, "bob"), a, snap("T", "m", 1, "bob")}, []int{1, 3}},
		{"title-query: the name gains the word, in any case", notif(titleHas("event")), []Snapshot{snap("Dust", "m", 1), snap("[EVENT]Dust", "m", 1), snap("[EVENT]Dust", "m", 1)}, []int{1}},
		{"title-query: never matches", notif(titleHas("zombie")), []Snapshot{a, a}, nil},
		{"status: goes offline", notif(isOnline(false)), []Snapshot{a, a, down, down}, []int{2}},
		{"status: back online", notif(isOnline(true)), []Snapshot{down, a, a}, []int{1}},
		{"status: offline when created does not fire for offline", notif(isOnline(false)), []Snapshot{down, down}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.n.CooldownSec = 1 // these test the conditions, not the quiet period, so it is short
			if got, _ := run(tc.n, tc.states...); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("fired at %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCreatingNeverFiresEvenWhenAlreadyTrue(t *testing.T) {
	n := notif(players(">=", 3))
	fired, n := run(n, snap("T", "m", 5), snap("T", "m", 6))
	if len(fired) != 0 || n.Phase != PhaseSpent {
		t.Fatalf("fired %v, phase %v: must wait for it to become true again", fired, n.Phase)
	}
	fired, _ = run(n, snap("T", "m", 1), snap("T", "m", 5)) // drops, then rises
	if !reflect.DeepEqual(fired, []int{1}) {
		t.Fatalf("fired %v", fired)
	}
}

func TestAServerThatIsDownMatchesOnlyStatus(t *testing.T) {
	up := snap("T", "de_dust2", 5, "alice")
	for name, c := range map[string]Condition{
		"map": mapIs("de_dust2"), "player": hasPlayer("alice"), "title": titleHas("T"), "playercount": players("<", 99),
	} {
		// Up, then down, then up again: the condition is false while down, so it fires on return.
		fired, _ := run(notif(c), down, up)
		if !reflect.DeepEqual(fired, []int{1}) {
			t.Errorf("%s: fired %v when coming back from down", name, fired)
		}
		fired, _ = run(notif(c), up, down)
		if len(fired) != 0 {
			t.Errorf("%s: fired %v when the server went down", name, fired)
		}
	}
}

func TestPlayerCountOperators(t *testing.T) {
	for _, tc := range []struct {
		op      string
		n, have int
		want    bool
	}{
		{">=", 10, 10, true}, {">=", 10, 9, false},
		{">", 10, 10, false}, {">", 10, 11, true},
		{"<=", 10, 10, true}, {"<=", 10, 11, false},
		{"<", 10, 9, true}, {"<", 10, 10, false},
		{"=", 10, 10, true}, {"=", 10, 11, false},
		{"!=", 10, 11, true}, {"!=", 10, 10, false},
	} {
		n := notif(players(tc.op, tc.n))
		n.Phase = PhaseArmed // as if it had been checked while false
		fired, _ := run(n, snap("T", "m", tc.have))
		if (len(fired) == 1) != tc.want {
			t.Errorf("%d %s %d: fired=%v, want %v", tc.have, tc.op, tc.n, len(fired) == 1, tc.want)
		}
	}
}

func TestPlayerCountFiresOnceThenRearmsWithCooldown(t *testing.T) {
	n := notif(players(">=", 50)) // default 60 minute cooldown
	n.Phase = PhaseArmed
	step := func(at time.Duration, count int) bool {
		var ev *Event
		n, ev = Evaluate(snap("t", "m", count), n, t0.Add(at))
		return ev != nil
	}
	min := time.Minute
	for _, s := range []struct {
		at    time.Duration
		count int
		want  bool
		why   string
	}{
		{0, 40, false, "below"},
		{1 * min, 50, true, "becomes true: fires"},
		{2 * min, 55, false, "sustained: does not repeat"},
		{3 * min, 40, false, "drops: rearms"},
		{10 * min, 52, false, "rises again inside 60 min: used up, no event"},
		{11 * min, 53, false, "still true: does not fire late"},
		{12 * min, 40, false, "drops: rearms"},
		{70 * min, 51, true, "rises after the cooldown: fires again"},
	} {
		if got := step(s.at, s.count); got != s.want {
			t.Fatalf("t+%v count=%d (%s): fired=%v, want %v", s.at, s.count, s.why, got, s.want)
		}
	}
}

func TestEveryNotificationHasA60MinuteCooldownUnlessSetOtherwise(t *testing.T) {
	if DefaultCooldown != 60*time.Minute {
		t.Fatalf("default cooldown %v, want 60 minutes", DefaultCooldown)
	}
	for name, c := range map[string]Condition{
		"map": mapIs("x"), "player": hasPlayer("x"), "playercount": players(">=", 5), "status": isOnline(true), "title-query": titleHas("x"),
	} {
		n := notif(c)
		if n.EffectiveCooldown() != DefaultCooldown {
			t.Errorf("%s: default cooldown %v", name, n.EffectiveCooldown())
		}
		n.CooldownSec = 600
		if n.EffectiveCooldown() != 10*time.Minute {
			t.Errorf("%s: explicit cooldown ignored: %v", name, n.EffectiveCooldown())
		}
	}
	// A notification with several conditions has one cooldown, not one per condition.
	if got := notif(mapIs("x"), players(">=", 5), hasPlayer("a")).EffectiveCooldown(); got != DefaultCooldown {
		t.Errorf("combined: %v", got)
	}
}

func TestCooldownIsCountedPerNotification(t *testing.T) {
	a, b := snap("T", "m1", 1), snap("T", "m2", 1)
	// m1 m2 m1 m2 m1 m2 at minutes 0..5. The first m2 fires, the others are inside 30 minutes.
	fired, _ := run(notif(mapIs("m2")), a, b, a, b, a, b)
	if !reflect.DeepEqual(fired, []int{1}) {
		t.Fatalf("default cooldown: fired %v", fired)
	}
	n := notif(mapIs("m2"))
	n.CooldownSec = 120 // 2 minutes: the m2 at minute 3 is inside it? no, 2 minutes later
	fired, _ = run(n, a, b, a, b, a, b)
	if !reflect.DeepEqual(fired, []int{1, 3, 5}) {
		t.Fatalf("2 minute cooldown: fired %v", fired)
	}
	// Two notifications for the same change each keep their own cooldown.
	x, y := notif(mapIs("m2")), notif(mapIs("m2"))
	x.CooldownSec, y.CooldownSec = 60, 3600
	fx, _ := run(x, a, b, a, b)
	fy, _ := run(y, a, b, a, b)
	if !reflect.DeepEqual(fx, []int{1, 3}) || !reflect.DeepEqual(fy, []int{1}) {
		t.Fatalf("x %v, y %v", fx, fy)
	}
}

// --- several conditions in one notification ---

func TestAllConditionsMustHoldTogether(t *testing.T) {
	n := notif(mapIs("de_dust2"), players(">=", 10))
	states := []Snapshot{
		snap("T", "de_inferno", 5),  // 0 neither
		snap("T", "de_dust2", 5),    // 1 map only
		snap("T", "de_dust2", 12),   // 2 both: fires
		snap("T", "de_dust2", 14),   // 3 still both: no repeat
		snap("T", "de_inferno", 14), // 4 players only: rearms
		snap("T", "de_dust2", 14),   // 5 both again, inside the 30 min cooldown: used up
		snap("T", "de_dust2", 3),    // 6 players drop: rearms
	}
	fired, n := run(n, states...)
	if !reflect.DeepEqual(fired, []int{2}) {
		t.Fatalf("fired at %v, want [2]", fired)
	}
	if n.Phase != PhaseArmed {
		t.Errorf("phase %v, want armed", n.Phase)
	}
	// After the cooldown it fires again.
	n, ev := Evaluate(snap("T", "de_dust2", 20), n, t0.Add(2*time.Hour))
	if ev == nil {
		t.Fatal("should fire again after the cooldown")
	}
}

func TestCombinedConditionsMixTypes(t *testing.T) {
	// A player on a map: alice is there first, and the map changes to the one wanted.
	n := notif(hasPlayer("alice"), mapIs("de_nuke"))
	fired, _ := run(n,
		snap("T", "de_dust2", 1, "alice"),
		snap("T", "de_nuke", 1, "alice"), // both now hold
	)
	if !reflect.DeepEqual(fired, []int{1}) {
		t.Fatalf("fired %v", fired)
	}
	// A status condition with a map: only while the server is up.
	n = notif(isOnline(true), titleHas("event"))
	fired, _ = run(n, down, snap("Event night", "m", 1), snap("Event night", "m", 1))
	if !reflect.DeepEqual(fired, []int{1}) {
		t.Fatalf("online and event: fired %v", fired)
	}
}

func TestUnknownPlayerListChangesNothing(t *testing.T) {
	blind := snap("T", "m", 1)
	blind.NamesKnown, blind.PlayerNames = false, nil
	n := notif(hasPlayer("bob"))
	fired, n := run(n, snap("T", "m", 1, "alice"), blind, snap("T", "m", 2, "alice", "bob"))
	if !reflect.DeepEqual(fired, []int{2}) {
		t.Fatalf("fired %v", fired)
	}
	// With another condition that is false, the list being unknown does not matter.
	n = notif(hasPlayer("bob"), mapIs("de_nuke"))
	n.Phase = PhaseSpent
	_, n = run(n, blind)
	if n.Phase != PhaseArmed {
		t.Errorf("a false condition decides even when another is unknown: %v", n.Phase)
	}
	// With the others true, it cannot tell, so the phase is kept.
	n = notif(hasPlayer("bob"), mapIs("m"))
	n.Phase = PhaseSpent
	_, n = run(n, blind)
	if n.Phase != PhaseSpent {
		t.Errorf("phase changed on an unknown level: %v", n.Phase)
	}
}

func TestValidate(t *testing.T) {
	good := []Notification{
		notif(mapIs("x")), notif(isOnline(false)), notif(players("<=", 3)),
		{UserID: "u", Delivery: DeliveryDM, Conditions: []Condition{isOnline(false)}},
		notif(mapIs("x"), players(">", 1), hasPlayer("a"), titleHas("t"), isOnline(true)),
	}
	bad := map[string]Notification{
		"no conditions":     notif(),
		"too many":          notif(mapIs("1"), mapIs("2"), mapIs("3"), mapIs("4"), mapIs("5"), mapIs("6")),
		"empty map":         notif(mapIs("")),
		"blank player":      notif(hasPlayer("  ")),
		"bad operator":      notif(players("=>", 1)),
		"negative count":    notif(players(">", -1)),
		"unknown type":      notif(Condition{Kind: "nope"}),
		"no channel":        {UserID: "u", Conditions: []Condition{isOnline(false)}},
		"bad delivery":      {UserID: "u", ChannelID: "c", Delivery: "pigeon", Conditions: []Condition{isOnline(false)}},
		"negative cooldown": {UserID: "u", ChannelID: "c", CooldownSec: -1, Conditions: []Condition{isOnline(false)}},
	}
	for i, n := range good {
		if err := n.Validate(); err != nil {
			t.Errorf("good %d: %v", i, err)
		}
	}
	for name, n := range bad {
		if n.Validate() == nil {
			t.Errorf("%s should be invalid", name)
		}
	}
}

func TestEventsAndMessages(t *testing.T) {
	cur := snap("Zombie | discord.gg/nide", "m2", 12, "alice")
	event := func(n Notification) *Event {
		n.Phase = PhaseArmed
		_, ev := Evaluate(cur, n, t0)
		return ev
	}
	// One condition: a short message with a fitting title.
	for _, tc := range []struct {
		c            Condition
		title, words string
	}{
		{mapIs("m2"), "Map change", "changed map to"},
		{hasPlayer("alice"), "Player joined", "joined"},
		{players(">=", 10), "Player count", "12/64"},
		{titleHas("zombie"), "Server name", "now matches"},
	} {
		ev := event(notif(tc.c))
		if ev == nil || ev.Title != tc.title || !strings.Contains(ev.Message, tc.words) {
			t.Errorf("%v: %+v", tc.c.Kind, ev)
		}
	}
	// Several conditions: one message that says all of them.
	ev := event(notif(mapIs("m2"), players(">=", 10)))
	if ev == nil || ev.Title != "All conditions met" || !strings.Contains(ev.Message, "the map is `m2`") ||
		!strings.Contains(ev.Message, " and the player count is >= 10") {
		t.Fatalf("combined: %+v", ev)
	}
	// The event carries where to deliver it.
	n := notif(mapIs("m2"))
	n.ChannelID = "c9"
	if ev := event(n); ev.Delivery != DeliveryChannel || ev.ChannelID != "c9" || ev.NotificationID != 1 {
		t.Errorf("delivery: %+v", ev)
	}
	n.Delivery = DeliveryDM
	if ev := event(n); ev.Delivery != DeliveryDM {
		t.Errorf("dm: %+v", ev)
	}
}

func TestServerNamesStayInCodeSpans(t *testing.T) {
	cur := snap("Zombie | discord.gg/nide", "m2", 1)
	for _, n := range []Notification{notif(mapIs("m2")), notif(mapIs("m2"), players(">=", 1))} {
		n.Phase = PhaseArmed
		_, ev := Evaluate(cur, n, t0)
		i := strings.Index(ev.Message, "discord.gg/nide")
		if i < 0 || strings.Count(ev.Message[:i], "`")%2 != 1 {
			t.Errorf("invite link outside a code span: %q", ev.Message)
		}
	}
	cur.Title = "a`b"
	n := notif(mapIs("m2"))
	n.Phase = PhaseArmed
	_, ev := Evaluate(cur, n, t0)
	if strings.Count(ev.Message, "`")%2 != 0 {
		t.Errorf("a backtick in a name broke the code span: %q", ev.Message)
	}
}

func TestPhrases(t *testing.T) {
	for c, want := range map[Condition]string{
		mapIs("de_dust2"):  "the map is `de_dust2`",
		hasPlayer("alice"): "`alice` is online",
		players(">=", 10):  "the player count is >= 10",
		isOnline(false):    "the server is offline",
		isOnline(true):     "the server is online",
		titleHas("event"):  "the server name contains `event`",
	} {
		if got := c.Phrase(); got != want {
			t.Errorf("%v: %q, want %q", c.Kind, got, want)
		}
	}
}

func TestSuggest(t *testing.T) {
	maps := []string{"de_inferno", "de_dust2", "de_dust", "cs_office", "ze_silent_hill_v3_0", "de_nuke"}
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"de_dust2", []string{"de_dust2", "de_dust"}}, // exact first, then a near miss
		{"DE_DUST", []string{"de_dust", "de_dust2"}},  // exact, then prefix
		{"dust", []string{"de_dust", "de_dust2"}},     // substring
		{"de_dsut2", []string{"de_dust2", "de_dust"}}, // swapped letters
		{"de_infrno", []string{"de_inferno"}},         // missing letter
		{"zzzz", nil},                                 // nothing similar
		{"", nil},
	} {
		if got := Suggest(tc.q, maps, 10); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Suggest(%q) = %v, want %v", tc.q, got, tc.want)
		}
	}
	if got := Suggest("a", []string{"a1", "a2", "a3"}, 2); len(got) != 2 {
		t.Errorf("limit not applied: %v", got)
	}
}

func TestParsePlayerCount(t *testing.T) {
	for _, tc := range []struct {
		in       string
		op       string
		n        int
		explicit bool
	}{
		{"10", ">=", 10, false}, // a plain number means at least
		{" 10 ", ">=", 10, false},
		{"0", ">=", 0, false},
		{">=10", ">=", 10, true},
		{">= 10", ">=", 10, true},
		{">10", ">", 10, true},
		{"<5", "<", 5, true},
		{"<=5", "<=", 5, true},
		{"=20", "=", 20, true},
		{"==20", "=", 20, true},
		{"!=3", "!=", 3, true},
		{"<>3", "!=", 3, true},
		{"=>10", ">=", 10, true}, // a common slip
		{"=<10", "<=", 10, true},
		{"≥10", ">=", 10, true},
		{"≤ 10", "<=", 10, true},
		{"≠3", "!=", 3, true},
		{"64", ">=", 64, false},
		{"<=64", "<=", 64, true},
	} {
		op, n, explicit, err := ParsePlayerCount(tc.in)
		if err != nil || op != tc.op || n != tc.n || explicit != tc.explicit {
			t.Errorf("%q: %q %d explicit=%v err=%v, want %q %d explicit=%v", tc.in, op, n, explicit, err, tc.op, tc.n, tc.explicit)
		}
		if err == nil && !ValidOp(op) {
			t.Errorf("%q gave an operator the rules do not know: %q", tc.in, op)
		}
	}
	for _, in := range []string{"", "  ", ">=", "many", ">=ten", "10 players", "-3", ">-3", "65", "1000", ">=65", ">=5000", "<<5", "=!5", "10.5", ">=1e3"} {
		if _, _, _, err := ParsePlayerCount(in); err == nil {
			t.Errorf("%q should be refused", in)
		}
	}
}

func TestPlayerCountsStopAtSixtyFour(t *testing.T) {
	if MaxPlayerCount != 64 {
		t.Fatalf("the most players a condition can name is %d, want 64", MaxPlayerCount)
	}
	if err := (Condition{Kind: KindPlayerCount, Op: ">=", N: 64}).Validate(); err != nil {
		t.Errorf("64 is allowed: %v", err)
	}
	if err := (Condition{Kind: KindPlayerCount, Op: ">=", N: 65}).Validate(); err == nil {
		t.Error("65 must be refused")
	}
}

func TestSeveralMapsAreOredAndSeveralPlayersAreAnded(t *testing.T) {
	at := func(m string, names ...string) Snapshot { return snap("T", m, len(names), names...) }
	for _, tc := range []struct {
		name   string
		n      Notification
		states []Snapshot
		want   []int
	}{
		{"maps: either one fires", quick(notif(mapIs("de_a"), mapIs("de_b"))),
			[]Snapshot{at("de_x"), at("de_a"), at("de_x"), at("de_b")}, []int{1, 3}},
		{"maps: going from one to the other stays true, so it does not fire again", notif(mapIs("de_a"), mapIs("de_b")),
			[]Snapshot{at("de_x"), at("de_a"), at("de_b"), at("de_a")}, []int{1}},
		{"maps: neither does not fire", notif(mapIs("de_a"), mapIs("de_b")),
			[]Snapshot{at("de_x"), at("de_y")}, nil},
		{"players: all must be online", notif(hasPlayer("al"), hasPlayer("bo")),
			[]Snapshot{at("m"), at("m", "al"), at("m", "bo"), at("m", "al", "bo")}, []int{3}},
		{"kinds are anded: (a or b) and both players", quick(notif(mapIs("de_a"), mapIs("de_b"), hasPlayer("al"), hasPlayer("bo"))),
			[]Snapshot{at("de_x", "al", "bo"), at("de_a", "al"), at("de_b", "al", "bo"), at("de_x", "al", "bo"), at("de_a", "al", "bo")}, []int{2, 4}},
		{"kinds are anded: the right map but one player is not enough", notif(mapIs("de_a"), mapIs("de_b"), hasPlayer("al"), hasPlayer("bo")),
			[]Snapshot{at("de_x"), at("de_a", "al"), at("de_b", "bo")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := run(tc.n, tc.states...); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("fired at %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPhrasesJoinTheMapsIntoOnePart(t *testing.T) {
	for _, tc := range []struct {
		n    Notification
		want []string
	}{
		{notif(mapIs("de_a")), []string{"the map is `de_a`"}},
		{notif(mapIs("de_a"), mapIs("de_b")), []string{"the map is one of `de_a`, `de_b`"}},
		{notif(hasPlayer("al"), mapIs("de_a"), hasPlayer("bo"), mapIs("de_b")),
			[]string{"`al` is online", "the map is one of `de_a`, `de_b`", "`bo` is online"}},
	} {
		if got := tc.n.Phrases(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Phrases() = %q, want %q", got, tc.want)
		}
	}
}

func TestSeveralMapsOnlyAreAnnouncedAsAMapChange(t *testing.T) {
	n := notif(mapIs("de_a"), mapIs("de_b"))
	n, _ = Evaluate(snap("T", "de_x", 1), n, t0)
	_, ev := Evaluate(snap("T", "de_b", 1), n, t0.Add(time.Minute))
	if ev == nil || ev.Title != "Map change" || !strings.Contains(ev.Message, "de_b") {
		t.Fatalf("event %+v, want a map change to de_b", ev)
	}
}
