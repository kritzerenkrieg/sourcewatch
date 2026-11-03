// Package app connects the pieces: poll results go through the store and rules, and
// the events that fire are delivered.
package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/kritzerenkrieg/sourcewatch/internal/notify"
	"github.com/kritzerenkrieg/sourcewatch/internal/poller"
	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
	"github.com/kritzerenkrieg/sourcewatch/internal/store"
)

type Pipeline struct {
	Store    *store.Store
	Notifier notify.Notifier // nil: events are logged and not delivered
	Log      *slog.Logger
	Ctx      context.Context

	mu  sync.Mutex
	ids map[string]int64
}

func (p *Pipeline) serverID(addr string) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id, ok := p.ids[addr]; ok {
		return id, nil
	}
	id, err := p.Store.UpsertServer(addr)
	if err != nil {
		return 0, err
	}
	if p.ids == nil {
		p.ids = map[string]int64{}
	}
	p.ids[addr] = id
	return id, nil
}

// Handle is the poller's OnUpdate. It stores the new state and delivers what fires.
// A failed cycle changes nothing until the poller declares the server offline.
func (p *Pipeline) Handle(u poller.Update) {
	var snap rules.Snapshot
	switch {
	case u.Err == nil:
		snap = rules.Snapshot{Online: true, Title: u.State.Name, Map: u.State.Map, Players: u.State.Players,
			MaxPlayers: u.State.MaxPlayers, PlayerNames: u.State.PlayerNames, NamesKnown: u.State.NamesKnown}
	case u.Transition && !u.Online:
		snap = rules.Snapshot{} // offline
		p.Log.Warn("server offline", "server", u.Addr, "err", u.Err)
	default:
		p.Log.Debug("poll failed", "server", u.Addr, "err", u.Err)
		return
	}
	id, err := p.serverID(u.Addr)
	if err != nil {
		p.Log.Error("look up server", "server", u.Addr, "err", err)
		return
	}
	events, err := p.Store.Process(id, snap, time.Now())
	if err != nil {
		p.Log.Error("process poll", "server", u.Addr, "err", err)
		return
	}
	if u.Transition {
		p.Log.Info("server state changed", "server", u.Addr, "online", u.Online)
	}
	if len(events) == 0 {
		return
	}
	p.Log.Info("notifications fired", "server", u.Addr, "count", len(events))
	if p.Notifier == nil {
		return
	}
	if err := notify.Deliver(p.Ctx, p.Notifier, p.Store, events); err != nil {
		p.Log.Error("deliver", "server", u.Addr, "err", err)
	}
}
