package poller

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/rumblefrog/go-a2s"
)

func TestDelayWithinJitter(t *testing.T) {
	for _, r := range []float64{0, 0.25, 0.5, 0.999999} {
		d := Delay(60*time.Second, 5*time.Second, r)
		if d < 55*time.Second || d > 65*time.Second {
			t.Fatalf("r=%v: %v outside 60s ±5s", r, d)
		}
	}
	if Delay(60*time.Second, 5*time.Second, 0) != 55*time.Second {
		t.Fatal("r=0 should give the minimum")
	}
}

func TestNewUsesB3Values(t *testing.T) {
	p := New(nil)
	if p.Interval != 60*time.Second || p.Jitter != 5*time.Second {
		t.Fatalf("got %v ±%v", p.Interval, p.Jitter)
	}
}

type collector struct {
	mu sync.Mutex
	us []Update
}

func (c *collector) add(u Update) { c.mu.Lock(); c.us = append(c.us, u); c.mu.Unlock() }
func (c *collector) get(addr string) (out []Update) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, u := range c.us {
		if u.Addr == addr {
			out = append(out, u)
		}
	}
	return
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestRunPollsBothServersAndStops(t *testing.T) {
	before := runtime.NumGoroutine()
	var col collector
	p := &Poller{
		Dial: func(addr string) (Client, error) {
			return &fake{info: a2s.ServerInfo{Name: addr, Map: "m", MaxPlayers: 8}}, nil
		},
		Interval: 10 * time.Millisecond, Jitter: 2 * time.Millisecond, OnUpdate: col.add,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx, []string{"a:1", "b:2"}); close(done) }()

	waitFor(t, func() bool { return len(col.get("a:1")) >= 2 && len(col.get("b:2")) >= 2 })
	if u := col.get("a:1")[0]; !u.Online || !u.Transition || u.State.Name != "a:1" {
		t.Fatalf("first update %+v", u)
	}
	if col.get("a:1")[1].Transition {
		t.Fatal("second update should not be a transition")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	waitFor(t, func() bool { return runtime.NumGoroutine() <= before })
}

func TestOfflineAfterThreeFailedCyclesThenBackOnline(t *testing.T) {
	var mu sync.Mutex
	failing := false
	var col collector
	p := &Poller{
		Dial: func(string) (Client, error) {
			mu.Lock()
			defer mu.Unlock()
			if failing {
				return nil, errors.New("down")
			}
			return &fake{}, nil
		},
		Interval: 5 * time.Millisecond, OnUpdate: col.add,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx, []string{"a:1"})

	waitFor(t, func() bool { return len(col.get("a:1")) >= 1 })
	mu.Lock()
	failing = true
	mu.Unlock()
	waitFor(t, func() bool {
		for _, u := range col.get("a:1") {
			if !u.Online {
				return true
			}
		}
		return false
	})
	var fails []Update
	for _, u := range col.get("a:1") {
		if u.Err != nil {
			fails = append(fails, u)
		}
	}
	if len(fails) < OfflineAfter {
		t.Fatalf("offline reported after only %d failures", len(fails))
	}
	for i, u := range fails[:OfflineAfter-1] {
		if !u.Online || u.Transition {
			t.Fatalf("failure %d should still be online: %+v", i+1, u)
		}
	}
	if u := fails[OfflineAfter-1]; u.Online || !u.Transition {
		t.Fatalf("failure %d should go offline: %+v", OfflineAfter, u)
	}

	mu.Lock()
	failing = false
	mu.Unlock()
	waitFor(t, func() bool {
		seenOffline := false
		for _, u := range col.get("a:1") {
			if !u.Online {
				seenOffline = true
			} else if seenOffline && u.Transition {
				return true
			}
		}
		return false
	})
}

func TestManagerPollsOnlyWhileListened(t *testing.T) {
	var col collector
	p := &Poller{
		Dial:     func(string) (Client, error) { return &fake{}, nil },
		Interval: 5 * time.Millisecond, OnUpdate: col.add,
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(ctx, p)

	m.Watch("a:1")
	m.Watch("a:1") // a second listener on the same server
	waitFor(t, func() bool { return len(col.get("a:1")) >= 2 })

	m.Unwatch("a:1")
	if !m.Watching("a:1") {
		t.Fatal("one listener is left, so the server must still be polled")
	}
	m.Unwatch("a:1")
	if m.Watching("a:1") {
		t.Fatal("last listener removed, polling must stop")
	}
	time.Sleep(30 * time.Millisecond) // let an in-flight cycle finish
	n := len(col.get("a:1"))
	time.Sleep(60 * time.Millisecond)
	if got := len(col.get("a:1")); got != n {
		t.Fatalf("still polled after removal: %d -> %d updates", n, got)
	}

	m.Unwatch("never-added:1") // harmless
	m.Watch("b:2")
	waitFor(t, func() bool { return len(col.get("b:2")) >= 1 })
	cancel()
	m.Wait()
}

// firstPolls records when each server is first dialed.
type firstPolls struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (f *firstPolls) dial(addr string) (Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.at[addr]; !ok {
		f.at[addr] = time.Now()
	}
	return &fake{}, nil
}

func (f *firstPolls) get(addr string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.at[addr]
	return t, ok
}

func TestWatchAllSpreadsTheFirstPollsOverTheInterval(t *testing.T) {
	// With an hour between polls, the second and third servers wait 20 and 40 minutes.
	f := &firstPolls{at: map[string]time.Time{}}
	p := &Poller{Dial: f.dial, Interval: time.Hour, OnUpdate: func(Update) {}}
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(ctx, p)
	m.WatchAll([]string{"a:1", "b:2", "a:1", "c:3"})
	waitFor(t, func() bool { _, ok := f.get("a:1"); return ok })
	time.Sleep(50 * time.Millisecond)
	for _, a := range []string{"b:2", "c:3"} {
		if _, ok := f.get(a); ok {
			t.Errorf("%s was polled at once", a)
		}
		if !m.Watching(a) {
			t.Errorf("%s is not watched while it waits for its first poll", a)
		}
	}
	// Listeners are counted as with Watch: a:1 was given twice.
	m.Unwatch("a:1")
	if !m.Watching("a:1") {
		t.Fatal("one listener of a:1 is left")
	}
	m.Unwatch("a:1")
	m.Unwatch("c:3") // stops a server still waiting for its first poll
	if m.Watching("a:1") || m.Watching("c:3") {
		t.Fatal("still watched after the last listener left")
	}
	cancel()
	done := make(chan struct{})
	go func() { m.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a server waiting for its first poll did not stop")
	}

	// With a short interval the first polls come in order, a third of it apart.
	f = &firstPolls{at: map[string]time.Time{}}
	p = &Poller{Dial: f.dial, Interval: 600 * time.Millisecond, OnUpdate: func(Update) {}}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	m = NewManager(ctx, p)
	m.WatchAll([]string{"a:1", "b:2", "c:3"})
	waitFor(t, func() bool { _, ok := f.get("c:3"); return ok })
	ta, _ := f.get("a:1")
	tb, _ := f.get("b:2")
	tc, _ := f.get("c:3")
	if gap := tb.Sub(ta); gap < 150*time.Millisecond {
		t.Errorf("b:2 polled %v after a:1, want about 200ms", gap)
	}
	if gap := tc.Sub(tb); gap < 150*time.Millisecond {
		t.Errorf("c:3 polled %v after b:2, want about 200ms", gap)
	}
}
