package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kritzerenkrieg/sourcewatch/internal/notify"
	"github.com/kritzerenkrieg/sourcewatch/internal/poller"
	"github.com/kritzerenkrieg/sourcewatch/internal/rules"
	"github.com/kritzerenkrieg/sourcewatch/internal/store"
)

type recorder struct {
	mu   sync.Mutex
	sent []notify.Target
	err  error
}

func (r *recorder) Send(_ context.Context, t notify.Target, title, msg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, t)
	return r.err
}

func newPipeline(t *testing.T, n notify.Notifier) (*Pipeline, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Pipeline{Store: st, Notifier: n, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Ctx: context.Background()}, st
}

func ok(mp string, names ...string) poller.Update {
	return poller.Update{Addr: "a:1", Online: true, State: poller.State{Name: "T", Map: mp, Players: len(names), PlayerNames: names, NamesKnown: true}}
}

func TestPollsFlowToDelivery(t *testing.T) {
	rec := &recorder{}
	p, st := newPipeline(t, rec)
	id, _ := st.UpsertServer("a:1")
	st.AddNotification(rules.Notification{UserID: "111", ServerID: id, ChannelID: "222", Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "de_inferno"}}})

	p.Handle(ok("de_dust2")) // first poll: nothing fires
	p.Handle(ok("de_dust2"))
	if len(rec.sent) != 0 {
		t.Fatalf("sent early: %v", rec.sent)
	}
	p.Handle(ok("de_inferno"))
	if len(rec.sent) != 1 || rec.sent[0].UserID != "111" || rec.sent[0].ChannelID != "222" || rec.sent[0].Delivery != rules.DeliveryChannel {
		t.Fatalf("sent %+v", rec.sent)
	}
}

func TestFailedCyclesOnlyCountOnceOffline(t *testing.T) {
	rec := &recorder{}
	p, st := newPipeline(t, rec)
	id, _ := st.UpsertServer("a:1")
	st.AddNotification(rules.Notification{UserID: "111", ServerID: id, ChannelID: "222", Conditions: []rules.Condition{{Kind: rules.KindStatus, Online: false}}})
	p.Handle(ok("m"))

	fail := poller.Update{Addr: "a:1", Online: true, Err: errors.New("timeout")}
	p.Handle(fail) // within the tolerance: nothing
	p.Handle(fail)
	if len(rec.sent) != 0 {
		t.Fatalf("fired before the server was declared offline: %v", rec.sent)
	}
	p.Handle(poller.Update{Addr: "a:1", Online: false, Transition: true, Err: errors.New("timeout")})
	if len(rec.sent) != 1 {
		t.Fatalf("offline notification not delivered: %v", rec.sent)
	}
	last, _ := st.LastState(id)
	if last.Online || last.Title != "T" {
		t.Fatalf("stored state %+v", last)
	}
}

func TestClosedDMMarksNotificationUndeliverable(t *testing.T) {
	rec := &recorder{err: notify.ErrDMClosed}
	p, st := newPipeline(t, rec)
	id, _ := st.UpsertServer("a:1")
	st.AddNotification(rules.Notification{UserID: "111", ServerID: id, Delivery: rules.DeliveryDM, Conditions: []rules.Condition{{Kind: rules.KindMap, Text: "m2"}}})
	p.Handle(ok("m1"))
	p.Handle(ok("m2"))
	got, _ := st.TakeUndeliverable("111")
	if len(got) != 1 {
		t.Fatalf("undeliverable %v", got)
	}
}

func TestNoNotifierStillStoresState(t *testing.T) {
	p, st := newPipeline(t, nil)
	p.Handle(ok("m1", "alice"))
	id, _ := st.UpsertServer("a:1")
	if got, _ := st.LastState(id); got == nil || got.Map != "m1" {
		t.Fatalf("state %+v", got)
	}
}
