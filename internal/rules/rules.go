// Package rules decides which /notify notifications fire for a change in server
// state. It has no storage or Discord code, so it is tested with plain values (C1, C4, C7).
package rules

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultCooldown is how long a notification stays quiet after it fires, unless it was made
// with another cooldown (C7, E12). It is counted per notification.
const DefaultCooldown = 60 * time.Minute

// Delivery options for a notification (E6).
const (
	DeliveryChannel = "channel" // the default
	DeliveryDM      = "dm"
)

type Kind string

const (
	KindMap         Kind = "map"
	KindPlayer      Kind = "player"
	KindPlayerCount Kind = "playercount"
	KindStatus      Kind = "status"
	KindTitle       Kind = "title-query"
)

// Snapshot is the state of one server after a poll. Players excludes bots.
// When NamesKnown is false the player list was not retrieved and is ignored.
type Snapshot struct {
	Online      bool
	Title       string
	Map         string
	Players     int
	MaxPlayers  int
	PlayerNames []string
	NamesKnown  bool

	// Source is "a2s" (also when empty) or "gametracker" (B5). DataAt is when the data was
	// taken: for GameTracker the time of its scan, which can be minutes old.
	Source string
	DataAt time.Time
}

// AllServers as a notification's ServerID means every game server the place it was made in
// watches, including ones added later. Its phase and cooldown are kept for each server separately.
const AllServers int64 = 0

// MaxConditions is how many conditions one notification can combine.
const MaxConditions = 5

// Condition is one thing that can be true or false about a server right now. Whether it is
// true is its level, and it only depends on the latest state.
//
//	map          Text = map name (not case-sensitive): the server is on that map.
//	player       Text = A2S name (not case-sensitive, exact): that player is on the server.
//	playercount  Op and N: the number of players compares as given.
//	status       Online = target: the server is up (true) or down (false).
//	title-query  Text = substring (not case-sensitive): the server name contains it.
type Condition struct {
	Kind   Kind
	Text   string
	Op     string
	N      int
	Online bool
}

// Phase is where a notification is in its cycle. A notification fires when all of its
// conditions become true together, then waits until they stop being true.
type Phase int

const (
	// PhaseNew: not checked yet since it was created or changed. The first check only
	// records whether the conditions already hold, so creating a notification never fires it.
	PhaseNew Phase = iota
	// PhaseArmed: the conditions do not all hold, so it fires when they do.
	PhaseArmed
	// PhaseSpent: it fired, or the conditions already held. It waits until they stop holding.
	PhaseSpent
)

// Notification is one notification: one or more conditions that all have to hold (C4).
// /notify makes it with one condition, and the id option adds more.
type Notification struct {
	// ID is the notification's number inside the bot, unique across everybody. Num is the number
	// its owner sees and types: each person's notifications in a place are numbered 1, 2, 3... (E18).
	ID         int64
	Num        int64
	UserID     string
	Scope      string // where it was created: "guild:<id>" or "user:<id>"
	ServerID   int64
	Conditions []Condition

	// Delivery is DeliveryChannel (default, also when empty) or DeliveryDM. ChannelID is
	// the channel /notify was run in, and is required for channel delivery.
	Delivery  string
	ChannelID string

	// Undeliverable is set when a DM was refused (E7). Such notifications are skipped
	// until the user is told.
	Undeliverable bool
	// Disabled notifications are not checked and never fire. The zero value is enabled.
	Disabled bool

	// CooldownSec is how long after firing the notification stays quiet. <= 0 means DefaultCooldown.
	CooldownSec int
	Phase       Phase
	LastFired   time.Time
}

// Event is a notification that fired.
type Event struct {
	NotificationID int64
	UserID         string
	ServerID       int64
	Title          string
	Message        string
	Delivery       string // DeliveryChannel or DeliveryDM
	ChannelID      string
}

var ops = map[string]func(a, b int) bool{
	">":  func(a, b int) bool { return a > b },
	">=": func(a, b int) bool { return a >= b },
	"<":  func(a, b int) bool { return a < b },
	"<=": func(a, b int) bool { return a <= b },
	"=":  func(a, b int) bool { return a == b },
	"!=": func(a, b int) bool { return a != b },
}

// ValidOp reports whether op is a supported playercount operator. The default is ">=" (C4).
func ValidOp(op string) bool { _, ok := ops[op]; return ok }

// comparisons maps what people type to the operator it means. Common slips such as "=>" are
// accepted too.
var comparisons = map[string]string{
	">=": ">=", "=>": ">=", "≥": ">=",
	"<=": "<=", "=<": "<=", "≤": "<=",
	"!=": "!=", "<>": "!=", "≠": "!=",
	">": ">", "<": "<", "=": "=", "==": "=",
}

// NormalizeComparison turns what was typed ("=>", "≥", "==") into an operator ValidOp accepts.
func NormalizeComparison(s string) (string, bool) {
	op, ok := comparisons[s]
	return op, ok
}

// MaxPlayerCount is the largest number of players a condition can name.
const MaxPlayerCount = 64

// ParsePlayerCount reads the value of a playercount condition: a number with an optional
// comparison in front, like ">=10", "<5", "=20" or "!=3". A plain number such as "10" means
// at least that many players, and explicit is false for it.
func ParsePlayerCount(s string) (op string, n int, explicit bool, err error) {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && strings.ContainsRune("<>=!", rune(s[end])) {
		end++
	}
	rest := s[end:]
	if end == 0 { // the unicode comparisons are several bytes long
		for _, sym := range []string{"≥", "≤", "≠"} {
			if strings.HasPrefix(s, sym) {
				end, rest = len(sym), s[len(sym):]
				break
			}
		}
	}
	op = ">="
	if end > 0 {
		var ok bool
		if op, ok = NormalizeComparison(s[:end]); !ok {
			return "", 0, false, fmt.Errorf("%q is not a comparison", s[:end])
		}
		explicit = true
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(rest))
	if convErr != nil {
		return "", 0, false, fmt.Errorf("%q is not a number of players", strings.TrimSpace(rest))
	}
	if n < 0 || n > MaxPlayerCount {
		return "", 0, false, fmt.Errorf("the number of players must be from 0 to %d", MaxPlayerCount)
	}
	return op, n, explicit, nil
}

// EffectiveDelivery is the delivery option with the default applied.
func (n Notification) EffectiveDelivery() string {
	if n.Delivery == "" {
		return DeliveryChannel
	}
	return n.Delivery
}

// Validate checks one condition.
func (c Condition) Validate() error {
	switch c.Kind {
	case KindMap, KindPlayer, KindTitle:
		if strings.TrimSpace(c.Text) == "" {
			return fmt.Errorf("%s needs a value", c.Kind)
		}
	case KindPlayerCount:
		if !ValidOp(c.Op) {
			return fmt.Errorf("unsupported playercount operator %q", c.Op)
		}
		if c.N < 0 || c.N > MaxPlayerCount {
			return fmt.Errorf("playercount value must be from 0 to %d", MaxPlayerCount)
		}
	case KindStatus:
	default:
		return fmt.Errorf("unknown notification type %q", c.Kind)
	}
	return nil
}

// Phrase says the condition in words, with outside text in code spans, for example
// "the map is `de_dust2`".
func (c Condition) Phrase() string {
	switch c.Kind {
	case KindMap:
		return "the map is " + span(c.Text)
	case KindPlayer:
		return span(c.Text) + " is online"
	case KindPlayerCount:
		return fmt.Sprintf("the player count is %s %d", c.Op, c.N)
	case KindStatus:
		if c.Online {
			return "the server is online"
		}
		return "the server is offline"
	case KindTitle:
		return "the server name contains " + span(c.Text)
	}
	return string(c.Kind)
}

// Validate checks a notification before it is stored.
func (n Notification) Validate() error {
	if n.CooldownSec < 0 {
		return fmt.Errorf("cooldown must not be negative")
	}
	switch n.EffectiveDelivery() {
	case DeliveryChannel:
		if n.ChannelID == "" {
			return fmt.Errorf("channel delivery needs a channel")
		}
	case DeliveryDM:
	default:
		return fmt.Errorf("unknown delivery %q", n.Delivery)
	}
	if len(n.Conditions) == 0 {
		return fmt.Errorf("a notification needs at least one condition")
	}
	if len(n.Conditions) > MaxConditions {
		return fmt.Errorf("a notification can combine at most %d conditions", MaxConditions)
	}
	for _, c := range n.Conditions {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// EffectiveCooldown is how long the notification stays quiet after firing.
func (n Notification) EffectiveCooldown() time.Duration {
	if n.CooldownSec > 0 {
		return time.Duration(n.CooldownSec) * time.Second
	}
	return DefaultCooldown
}

type level int

const (
	levelNo level = iota
	levelYes
	levelUnknown // cannot tell, for example the player list was not retrieved
)

// level says whether the condition holds for the given state. A server that is down has no
// map, players or name to match, so only a status condition can hold for it.
func (c Condition) level(cur Snapshot) level {
	yes := func(b bool) level {
		if b {
			return levelYes
		}
		return levelNo
	}
	if c.Kind == KindStatus {
		return yes(cur.Online == c.Online)
	}
	if !cur.Online {
		return levelNo
	}
	switch c.Kind {
	case KindMap:
		return yes(strings.EqualFold(cur.Map, c.Text))
	case KindTitle:
		return yes(containsFold(cur.Title, c.Text))
	case KindPlayer:
		if !cur.NamesKnown {
			return levelUnknown
		}
		return yes(hasName(cur.PlayerNames, c.Text))
	case KindPlayerCount:
		cmp, ok := ops[c.Op]
		return yes(ok && cmp(cur.Players, c.N))
	}
	return levelNo
}

// level is the level of all conditions together. Conditions of different kinds are ANDed, and so
// are several conditions of one kind, except map conditions, which are ORed: the map is any of them
// (E21). The result is no if any group is no, otherwise unknown if any is.
func (n Notification) level(cur Snapshot) level {
	out := levelYes
	mapLevel, hasMap := levelNo, false // the map group: yes if any map matches
	for _, c := range n.Conditions {
		l := c.level(cur)
		if c.Kind == KindMap {
			hasMap = true
			if l == levelYes || (l == levelUnknown && mapLevel == levelNo) {
				mapLevel = l
			}
			continue
		}
		switch l {
		case levelNo:
			return levelNo
		case levelUnknown:
			out = levelUnknown
		}
	}
	if hasMap {
		switch mapLevel {
		case levelNo:
			return levelNo
		case levelUnknown:
			out = levelUnknown
		}
	}
	return out
}

// Phrases describes the conditions as parts to join with "and": every part must hold. The map
// conditions are one part, since any of them will do.
func (n Notification) Phrases() []string {
	var out, maps []string
	mapAt := -1
	for _, c := range n.Conditions {
		if c.Kind != KindMap {
			out = append(out, c.Phrase())
			continue
		}
		if mapAt < 0 {
			mapAt = len(out)
			out = append(out, "")
		}
		maps = append(maps, span(c.Text))
	}
	switch {
	case len(maps) == 1:
		out[mapAt] = "the map is " + maps[0]
	case len(maps) > 1:
		out[mapAt] = "the map is one of " + strings.Join(maps, ", ")
	}
	return out
}

// Evaluate checks a notification against the latest state of its server. It fires when all
// its conditions become true together, once, and then waits until they stop being true
// (C7). Inside the cooldown after a firing that moment is used up without firing, so
// a condition that stays true never fires late. It returns the updated notification
// (Phase and LastFired) and the event, if any. The caller saves the updated notification.
func Evaluate(cur Snapshot, n Notification, now time.Time) (Notification, *Event) {
	switch n.level(cur) {
	case levelUnknown:
		return n, nil // cannot tell, so nothing changes
	case levelNo:
		n.Phase = PhaseArmed
		return n, nil
	}
	// All conditions hold.
	switch n.Phase {
	case PhaseNew:
		n.Phase = PhaseSpent // already true when first checked: wait for it to become true again
		return n, nil
	case PhaseSpent:
		return n, nil
	}
	n.Phase = PhaseSpent
	if cd := n.EffectiveCooldown(); cd > 0 && !n.LastFired.IsZero() && now.Sub(n.LastFired) < cd {
		return n, nil
	}
	n.LastFired = now
	return n, newEvent(cur, n)
}

func newEvent(cur Snapshot, n Notification) *Event {
	title, msg := "All conditions met", ""
	if phrases := n.Phrases(); len(phrases) == 1 {
		title, msg = single(cur, n.Conditions[0]) // one condition, or only maps: the map it changed to
	} else {
		msg = fmt.Sprintf("%s: %s.", span(cur.Title), strings.Join(phrases, " and "))
	}
	return &Event{NotificationID: n.ID, UserID: n.UserID, ServerID: n.ServerID, Title: title, Message: msg,
		Delivery: n.EffectiveDelivery(), ChannelID: n.ChannelID}
}

// single writes the title and text for a notification with one condition. Server names go
// in code spans: they often hold Discord invite links, which would otherwise become links
// and large preview cards.
func single(cur Snapshot, c Condition) (title, message string) {
	name := span(cur.Title)
	switch c.Kind {
	case KindMap:
		return "Map change", fmt.Sprintf("%s changed map to %s.", name, span(cur.Map))
	case KindStatus:
		if c.Online {
			return "Server status", fmt.Sprintf("%s is back online.", name)
		}
		return "Server status", fmt.Sprintf("%s went offline.", name)
	case KindTitle:
		return "Server name", fmt.Sprintf("%s now matches %s.", name, span(c.Text))
	case KindPlayer:
		return "Player joined", fmt.Sprintf("%s joined %s.", span(c.Text), name)
	}
	return "Player count", fmt.Sprintf("%s has %d/%d players.", name, cur.Players, cur.MaxPlayers)
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// hasName reports whether want is one of names: not case-sensitive, and ignoring spaces around a
// name, since some players have them in their in-game name (" SerbianEagle ").
func hasName(names []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, n := range names {
		if strings.EqualFold(strings.TrimSpace(n), want) {
			return true
		}
	}
	return false
}

// span puts text from outside in an inline code span. A backtick cannot appear inside one.
func span(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "`-`"
	}
	return "`" + strings.ReplaceAll(s, "`", "'") + "`"
}
