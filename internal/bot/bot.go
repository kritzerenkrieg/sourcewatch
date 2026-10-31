// Package bot is the Discord side of sourcewatch: the gateway connection and the slash
// commands (E1, E8).
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
	"github.com/kritzerenkrieg/sourcewatch/internal/store"
)

// botCmd is one slash command. The registry drives registration with Discord and the
// /help text, so a command cannot be registered without being listed.
type botCmd struct {
	name        string
	description string
	// public replies are shown to everyone in the channel. All others are private to whoever ran the command.
	public  bool
	options []*discordgo.ApplicationCommandOption
	run     func(b *Bot, in *discordgo.Interaction, opts []*discordgo.ApplicationCommandInteractionDataOption) string
}

// registry lists every slash command, in the order /help shows them.
func registry() []botCmd {
	return []botCmd{
		{name: "server", description: "Add or remove a game server to watch (needs Manage Server in a server)", options: serverOptions(), run: runServer},
		{name: "notify", description: "Get a notification when something happens on a game server", options: notifyOptions(), run: runNotify},
		{name: "status", description: "Show what your game servers are doing and who is online, to everyone here", public: true, run: runStatus},
		{name: "admin", description: "Settings for this Discord server (needs Manage Server)", options: adminOptions(), run: runAdmin},
		{name: "help", description: "How to use sourcewatch", run: runHelp},
		{name: "version", description: "Show which version of sourcewatch is running", run: runVersion},
	}
}

// Watcher starts and stops polling a game server. *poller.Manager implements it.
type Watcher interface {
	Watch(addr string)
	Unwatch(addr string)
}

// DMProber sends a test DM (E7). *notify.Discord implements it.
type DMProber interface {
	ProbeDM(ctx context.Context, userID string) error
}

// Deps are what the commands work on.
type Deps struct {
	Store   *store.Store
	Watcher Watcher
	DM      DMProber
	// Version is the build of the bot, set when a release is built. Empty means a plain build.
	Version string
}

type Bot struct {
	s       *discordgo.Session
	log     *slog.Logger
	appID   string
	guildID string
	deps    Deps
}

// New creates a bot for the token. appID may be empty, in which case the bot's own
// user ID is used. guildID, when set, names a Discord server whose old server-only copy of
// the commands is removed at startup (the commands themselves are global).
func New(token, appID, guildID string, log *slog.Logger, deps Deps) (*Bot, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.LogLevel = discordgo.LogWarning
	s.Identify.Intents = discordgo.IntentsGuilds // slash commands need no privileged intents
	return &Bot{s: s, log: log, appID: appID, guildID: guildID, deps: deps}, nil
}

// commandRegistrar is the part of a Discord session that registers commands.
type commandRegistrar interface {
	ApplicationCommandBulkOverwrite(appID, guildID string, commands []*discordgo.ApplicationCommand, options ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error)
}

// registerCommands registers the commands globally, so they work in every Discord server the
// bot is in and in DMs with the bot. The list replaces whatever was registered before,
// including the old /sourcewatch command. A command registered for one Discord server only never
// shows in a DM, and would show twice next to the global one, so when staleGuildID is set
// that server's own copies are removed. It returns the number of global commands.
func registerCommands(r commandRegistrar, appID, staleGuildID string) (int, error) {
	cmds, err := r.ApplicationCommandBulkOverwrite(appID, "", commands())
	if err != nil {
		return 0, fmt.Errorf("register commands: %w", err)
	}
	if staleGuildID != "" {
		if _, err := r.ApplicationCommandBulkOverwrite(appID, staleGuildID, []*discordgo.ApplicationCommand{}); err != nil {
			return len(cmds), fmt.Errorf("remove server-only commands: %w", err)
		}
	}
	return len(cmds), nil
}

// commands builds the application commands from the registry. Each can be used in Discord
// servers and in DMs with the bot, and the app installs to servers.
func commands() []*discordgo.ApplicationCommand {
	out := make([]*discordgo.ApplicationCommand, 0, len(registry()))
	for _, c := range registry() {
		contexts := []discordgo.InteractionContextType{discordgo.InteractionContextGuild, discordgo.InteractionContextBotDM}
		install := []discordgo.ApplicationIntegrationType{discordgo.ApplicationIntegrationGuildInstall}
		out = append(out, &discordgo.ApplicationCommand{
			Name:             c.name,
			Description:      c.description,
			Options:          c.options,
			Contexts:         &contexts,
			IntegrationTypes: &install,
		})
	}
	return out
}

// find returns the registered command with this name.
func find(name string) (botCmd, bool) {
	for _, c := range registry() {
		if c.name == name {
			return c, true
		}
	}
	return botCmd{}, false
}

// Run connects to the gateway, registers the commands and blocks until ctx is done.
func (b *Bot) Run(ctx context.Context) error {
	b.s.AddHandler(b.onInteraction)
	if err := b.s.Open(); err != nil {
		return fmt.Errorf("open gateway: %w", err)
	}
	defer b.s.Close()

	appID := b.appID
	if appID == "" {
		appID = b.s.State.User.ID
	}
	n, err := registerCommands(b.s, appID, b.guildID)
	if err != nil {
		return err
	}
	b.log.Info("bot online", "user", b.s.State.User.Username, "commands", n, "scope", "global", "cleaned_guild", b.guildID)
	<-ctx.Done()
	return nil
}

func (b *Bot) onInteraction(s *discordgo.Session, ic *discordgo.InteractionCreate) {
	// discordgo runs each handler in its own goroutine and does not recover, so a panic in
	// autocomplete or a button would stop the whole service. Commands also recover in answer,
	// which replies with an apology.
	defer func() {
		if p := recover(); p != nil {
			b.log.Error("interaction panicked", "type", ic.Type, "panic", p)
		}
	}()
	if ic.Type == discordgo.InteractionApplicationCommandAutocomplete {
		if _, ok := find(ic.ApplicationCommandData().Name); !ok {
			return
		}
		err := s.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionApplicationCommandAutocompleteResult,
			Data: &discordgo.InteractionResponseData{Choices: b.autocomplete(ic.Interaction)},
		})
		if err != nil {
			b.log.Warn("respond to autocomplete", "err", err)
		}
		return
	}
	if ic.Type == discordgo.InteractionMessageComponent {
		b.component(s, ic.Interaction)
		return
	}
	if ic.Type != discordgo.InteractionApplicationCommand {
		return
	}
	if _, ok := find(ic.ApplicationCommandData().Name); !ok {
		return
	}
	answer(s, b.log, ic.Interaction, isPublic(ic.Interaction), func() reply { return b.executeReply(ic.Interaction) })
}

// isPublic reports whether the reply to this command is shown to everyone in the channel: /status, and
// /notify show.
func isPublic(in *discordgo.Interaction) bool {
	data := in.ApplicationCommandData()
	if c, ok := find(data.Name); ok && c.public {
		return true
	}
	return isNotifySubcommand(in, "show")
}

// reply is what a command answers: text, and sometimes buttons.
type reply struct {
	content    string
	components []discordgo.MessageComponent
}

// executeReply runs a command and also returns the buttons it needs. execute gives only the text.
func (b *Bot) executeReply(in *discordgo.Interaction) reply {
	r := reply{content: b.execute(in)}
	// The reset question gets its buttons, unless sourcewatch is turned off here and the reply
	// says so instead (E15).
	if a := who(in); isNotifySubcommand(in, "reset") && b.guildSettings(a).Active {
		if n := b.notificationCount(a); n > 0 {
			r.components = resetButtons(n)
		}
	}
	return r
}

// responder is the part of a Discord session used to answer an interaction.
type responder interface {
	InteractionRespond(i *discordgo.Interaction, r *discordgo.InteractionResponse, o ...discordgo.RequestOption) error
	InteractionResponseEdit(i *discordgo.Interaction, e *discordgo.WebhookEdit, o ...discordgo.RequestOption) (*discordgo.Message, error)
}

// deferAfter is how long a command may run before the bot shows "thinking…". Discord wants a
// first response within 3 seconds, and a command like /notify with a DM can take longer.
var deferAfter = 2 * time.Second

// answer runs a command and replies. A quick command is answered directly. A slow one
// first gets an acknowledgement and is then filled in. A crash inside a command becomes an
// apology and cannot take the bot down.
func answer(r responder, log *slog.Logger, in *discordgo.Interaction, public bool, run func() reply) {
	done := make(chan reply, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Error("command panicked", "panic", p)
				done <- reply{content: "Something went wrong on my side. Please try again."}
			}
		}()
		done <- run()
	}()

	timer := time.NewTimer(deferAfter)
	defer timer.Stop()
	select {
	case rep := <-done:
		err := r.InteractionRespond(in, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: replyDataWith(rep.content, rep.components, public),
		})
		if err != nil {
			log.Warn("respond to interaction", "err", err)
		}
	case <-timer.C:
		ack := &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Flags: visibility(public)},
		}
		if err := r.InteractionRespond(in, ack); err != nil {
			log.Warn("acknowledge interaction", "err", err)
			return
		}
		rep := <-done
		d := replyDataWith(rep.content, rep.components, public)
		edit := &discordgo.WebhookEdit{Content: &d.Content, AllowedMentions: d.AllowedMentions}
		if len(rep.components) > 0 {
			edit.Components = &rep.components
		}
		if _, err := r.InteractionResponseEdit(in, edit); err != nil {
			log.Warn("edit interaction reply", "err", err)
		}
	}
}

// execute runs the command named in the interaction and returns the reply text.
func (b *Bot) execute(in *discordgo.Interaction) string {
	data := in.ApplicationCommandData()
	c, ok := find(data.Name)
	if !ok {
		return fmt.Sprintf("Unknown command %q. Try /help.", data.Name)
	}
	// An admin can turn sourcewatch off for a whole Discord server. Only /admin still works there.
	if data.Name != "admin" && !b.guildSettings(who(in)).Active {
		return disabledMessage
	}
	return c.run(b, in, data.Options)
}

func runHelp(_ *Bot, _ *discordgo.Interaction, _ []*discordgo.ApplicationCommandInteractionDataOption) string {
	var sb strings.Builder
	sb.WriteString("**sourcewatch** tells you when something happens on a Source game server: a map change, a player joining, a full server, or a server going offline.\n\n")
	sb.WriteString("**Quick start**\n")
	sb.WriteString("1. `/server add` to watch a game server (in a Discord server you need Manage Server)\n")
	sb.WriteString("2. `/notify map`, `player`, `playercount`, `status` or `title-query` to choose what you want to hear about\n")
	sb.WriteString("3. `/status` to show your servers and who is online (everyone in the channel sees it)\n")
	sb.WriteString("Manage notifications with `/notify list`, `show`, `settings`, `remove` and `reset`. Admins use `/admin settings`.\n\n")
	sb.WriteString("**Combine conditions**\n")
	fmt.Fprintf(&sb, "Add `id:` to any `/notify` type to add it to that notification (up to %d). All must hold, but for several maps any one is enough.\n", rules.MaxConditions)
	sb.WriteString("Example: `/notify map map-name:de_dust2` saves ID 1, then `/notify playercount value:>=10 id:1`\n\n")
	sb.WriteString("**Notification settings** (`/notify settings id:1`, or no `id` for all)\n")
	sb.WriteString("`delivery` - in this channel or by DM\n")
	sb.WriteString("`cooldown` - minutes of quiet after it fires\n")
	sb.WriteString("`active` - enabled or disabled\n\n")
	sb.WriteString("**Commands**\n")
	for _, c := range registry() {
		fmt.Fprintf(&sb, "`/%s` - %s\n", c.name, c.description)
	}
	sb.WriteString("\nNotifications arrive in the channel where you made them, or by DM if you choose that.")
	return sb.String()
}

// runVersion is /version: the build of the bot that is running.
func runVersion(b *Bot, _ *discordgo.Interaction, _ []*discordgo.ApplicationCommandInteractionDataOption) string {
	v := b.deps.Version
	if v == "" {
		v = "dev"
	}
	return fmt.Sprintf("**sourcewatch** `%s` (%s)", v, runtime.Version())
}

// visibility is the message flags every reply has: link previews are off, and unless the command
// is public only the person who ran it sees the reply. Link previews are off because game server
// names often hold Discord invite links, which Discord would otherwise expand into a large card.
func visibility(public bool) discordgo.MessageFlags {
	if public {
		return discordgo.MessageFlagsSuppressEmbeds
	}
	return discordgo.MessageFlagsEphemeral | discordgo.MessageFlagsSuppressEmbeds
}

// replyData is how a private reply is sent. Replies can include names from game servers, so
// nothing may ping (D2).
func replyData(content string) *discordgo.InteractionResponseData {
	return replyDataWith(content, nil, false)
}

func replyDataWith(content string, components []discordgo.MessageComponent, public bool) *discordgo.InteractionResponseData {
	return &discordgo.InteractionResponseData{
		Content:         content,
		Components:      components,
		Flags:           visibility(public),
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}
}
