package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/kritzerenkrieg/sourcewatch/internal/app"
	"github.com/kritzerenkrieg/sourcewatch/internal/bot"
	"github.com/kritzerenkrieg/sourcewatch/internal/config"
	"github.com/kritzerenkrieg/sourcewatch/internal/notify"
	"github.com/kritzerenkrieg/sourcewatch/internal/poller"
	"github.com/kritzerenkrieg/sourcewatch/internal/store"
)

// version is set when a release is built: go build -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("sourcewatch starting", "version", version)
	if err := run(log); err != nil {
		log.Error("sourcewatch stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log.Info("config loaded", "config", cfg, "database", cfg.Database)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer st.Close()

	pipe := &app.Pipeline{Store: st, Log: log, Ctx: ctx}
	var discord *notify.Discord
	if cfg.DiscordToken != "" {
		discord, err = notify.NewDiscord(cfg.DiscordToken)
		if err != nil {
			return err
		}
		pipe.Notifier = discord
	}

	p := poller.New(pipe.Handle)
	// Servers added from Discord are checked for a public address on every poll, with their host
	// names looked up at most every 10 minutes. The ones in the config file are the operator's own
	// and are trusted.
	dns := &poller.LookupCache{Lookup: net.LookupIP}
	p.Dial = poller.PublicDialer(poller.DialA2S, dns.LookupIP, cfg.Servers)
	if !cfg.DisableGameTracker {
		p.Fallback = poller.NewGameTracker() // used only when A2S gets no answer (B5)
	}
	mgr := poller.NewManager(ctx, p)
	// Servers people asked the bot to watch come back after a restart. Their first polls, and those
	// of the config file's servers, are spread over the first minute.
	addrs, err := st.AllListenerAddrs()
	if err != nil {
		return err
	}
	mgr.WatchAll(append(append([]string{}, cfg.Servers...), addrs...))

	var botErr error
	if cfg.DiscordToken != "" {
		b, err := bot.New(cfg.DiscordToken, cfg.ApplicationID, cfg.GuildID, log, bot.Deps{Store: st, Watcher: mgr, DM: discord})
		if err != nil {
			return err
		}
		botErr = b.Run(ctx) // blocks until ctx is done or the connection fails
		stop()
	} else {
		log.Warn("no discord token: polling and storing only")
		<-ctx.Done()
	}
	mgr.Wait()
	return botErr
}
