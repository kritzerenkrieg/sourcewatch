// Package notify delivers fired notifications to Discord over REST (D1, D2, E6, E7).
// It needs no gateway connection.
package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
)

var (
	// ErrDMClosed means Discord refused the DM (error 50007) (E7).
	ErrDMClosed = errors.New("discord refused the DM")
	// ErrChannelRefused means Discord refused a channel message: the channel is gone, or the bot may
	// not see it or post in it (errors 10003, 50001 and 50013) (D4).
	ErrChannelRefused = errors.New("discord refused the channel message")
	// ErrUnauthorized means Discord rejected the bot token. Retrying cannot help.
	ErrUnauthorized = errors.New("discord rejected the bot token")
)

// maxContent is Discord's message length limit.
const maxContent = 2000

// Target says where a message goes. UserID is the Discord user it is for.
type Target struct {
	Delivery  string // rules.DeliveryChannel (also when empty) or rules.DeliveryDM
	ChannelID string // for channel delivery
	UserID    string
	// UserIDs, for channel delivery, is everyone the one message is for. They are all mentioned,
	// and nobody else can be. When empty, only UserID is.
	UserIDs []string
}

// MaxMentions is how many people one channel message mentions. A longer group is sent as
// several messages, so none gets near Discord's 2000 character limit.
const MaxMentions = 40

func (t Target) users() []string {
	if len(t.UserIDs) > 0 {
		return t.UserIDs
	}
	return []string{t.UserID}
}

// Notifier sends a message to a target.
type Notifier interface {
	Send(ctx context.Context, t Target, title, message string) error
}

// Discord is the Notifier for Discord. Rate limits (429, retry_after) are waited out by discordgo.
type Discord struct{ s *discordgo.Session }

// NewDiscord returns a Discord notifier for the bot token. It never opens the gateway.
func NewDiscord(token string) (*Discord, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	return &Discord{s: s}, nil
}

// Send mentions the user by ID in channel delivery. Every message carries
// allowed_mentions with an empty parse list and at most that one user, so names in the
// title or message (game names are not trusted) cannot ping anyone (D2).
func (d *Discord) Send(ctx context.Context, t Target, title, message string) error {
	users := t.users()
	for _, u := range users {
		if !isSnowflake(u) {
			return fmt.Errorf("invalid user id %q", u)
		}
	}
	allowed := &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
	body := "**" + title + "**\n" + message
	channel := t.ChannelID
	switch t.Delivery {
	case rules.DeliveryDM:
		dm, err := d.s.UserChannelCreate(users[0], discordgo.WithContext(ctx))
		if err != nil {
			return mapErr(err)
		}
		channel = dm.ID
	case rules.DeliveryChannel, "":
		if !isSnowflake(channel) {
			return fmt.Errorf("invalid channel id %q", channel)
		}
		mentions := make([]string, len(users))
		for i, u := range users {
			mentions[i] = "<@" + u + ">"
		}
		body = strings.Join(mentions, " ") + " " + body
		allowed.Users = users
	default:
		return fmt.Errorf("unknown delivery %q", t.Delivery)
	}
	_, err := d.s.ChannelMessageSendComplex(channel, &discordgo.MessageSend{
		Content:         truncate(body, maxContent),
		AllowedMentions: allowed,
		// Server names often hold invite links. Without this Discord expands them into cards.
		Flags: discordgo.MessageFlagsSuppressEmbeds,
	}, discordgo.WithContext(ctx))
	return mapErr(err)
}

// ProbeDM sends a short DM to see whether the user can be reached (E7). It returns
// ErrDMClosed when Discord refuses it.
func (d *Discord) ProbeDM(ctx context.Context, userID string) error {
	return d.Send(ctx, Target{Delivery: rules.DeliveryDM, UserID: userID},
		"sourcewatch", "Notifications for you will arrive here.")
}

// Marker records that a notification could not be delivered.
type Marker interface{ MarkUndeliverable(id int64) error }

// Deliver sends the events. People who get the same message in the same channel are mentioned
// together in one message, like "@Kritz @Player2 @Player3 Map change …", instead of one message each.
// DMs are always one person each. A refused DM (E7), or a refused channel message (D4), marks the
// notifications it was for undeliverable, and delivery goes on with the rest. A rejected token stops
// everything. Other errors are collected and returned after all messages were tried.
func Deliver(ctx context.Context, n Notifier, m Marker, events []rules.Event) error {
	var errs []error
	for _, g := range groupEvents(events) {
		err := n.Send(ctx, g.target, g.title, g.message)
		switch {
		case err == nil:
		case errors.Is(err, ErrDMClosed), errors.Is(err, ErrChannelRefused):
			for _, id := range g.notificationIDs {
				if merr := m.MarkUndeliverable(id); merr != nil {
					errs = append(errs, merr)
				}
			}
		case errors.Is(err, ErrUnauthorized):
			return errors.Join(append(errs, err)...)
		default:
			errs = append(errs, fmt.Errorf("notifications %v: %w", g.notificationIDs, err))
		}
	}
	return errors.Join(errs...)
}

type message struct {
	target          Target
	title, message  string
	notificationIDs []int64
}

// groupEvents merges channel events with the same channel, title and text into one message each, in
// the order they first appeared. Each person is mentioned once, and a group larger than MaxMentions is
// split. Events for DMs are never merged.
func groupEvents(events []rules.Event) []message {
	var out []message
	index := map[string]int{} // the group being filled, by channel and text
	for _, ev := range events {
		t := Target{Delivery: ev.Delivery, ChannelID: ev.ChannelID, UserID: ev.UserID}
		if ev.Delivery == rules.DeliveryDM {
			out = append(out, message{target: t, title: ev.Title, message: ev.Message, notificationIDs: []int64{ev.NotificationID}})
			continue
		}
		key := ev.ChannelID + "\x00" + ev.Title + "\x00" + ev.Message
		i, ok := index[key]
		if ok && len(out[i].target.UserIDs) >= MaxMentions {
			ok = false // that message is full, so start the next one
		}
		if !ok {
			index[key] = len(out)
			out = append(out, message{target: t, title: ev.Title, message: ev.Message})
			i = len(out) - 1
		}
		g := &out[i]
		g.notificationIDs = append(g.notificationIDs, ev.NotificationID)
		if !contains(g.target.UserIDs, ev.UserID) {
			g.target.UserIDs = append(g.target.UserIDs, ev.UserID)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var re *discordgo.RESTError
	if errors.As(err, &re) {
		if re.Message != nil {
			switch re.Message.Code {
			case discordgo.ErrCodeCannotSendMessagesToThisUser:
				return fmt.Errorf("%w: %v", ErrDMClosed, err)
			case discordgo.ErrCodeUnknownChannel, discordgo.ErrCodeMissingAccess, discordgo.ErrCodeMissingPermissions:
				return fmt.Errorf("%w: %v", ErrChannelRefused, err)
			}
		}
		if re.Response != nil && re.Response.StatusCode == 401 {
			return fmt.Errorf("%w: %v", ErrUnauthorized, err)
		}
	}
	return err
}

// isSnowflake reports whether s looks like a Discord ID, so it is safe to put in a mention.
func isSnowflake(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
