package poller

import (
	"context"
	"sync"
	"time"
)

// Manager polls game servers that people are listening to, added and removed at
// runtime (E3, E4). A server is polled while at least one listener watches it.
type Manager struct {
	p   *Poller
	ctx context.Context
	wg  sync.WaitGroup

	mu sync.Mutex
	w  map[string]*watch
}

type watch struct {
	n      int
	cancel context.CancelFunc
}

func NewManager(ctx context.Context, p *Poller) *Manager {
	return &Manager{p: p, ctx: ctx, w: map[string]*watch{}}
}

// Watch adds a listener for addr and starts polling it at once if it was not polled yet.
func (m *Manager) Watch(addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.add(addr) {
		m.start(addr, 0)
	}
}

// WatchAll adds a listener for each address, as Watch does. It is for the servers known at
// startup: the ones it starts are not all polled at once, but have their first polls spread
// evenly over one interval, so they are not queried, stored and checked for notifications in
// the same moment.
func (m *Manager) WatchAll(addrs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var started []string
	for _, a := range addrs {
		if m.add(a) {
			started = append(started, a)
		}
	}
	for i, a := range started {
		m.start(a, time.Duration(i)*m.p.Interval/time.Duration(len(started)))
	}
}

// add counts a listener for addr and reports whether addr is new, so that it needs starting.
func (m *Manager) add(addr string) bool {
	if w, ok := m.w[addr]; ok {
		w.n++
		return false
	}
	m.w[addr] = &watch{n: 1}
	return true
}

// start polls addr in its own goroutine, the first time after first.
func (m *Manager) start(addr string, first time.Duration) {
	ctx, cancel := context.WithCancel(m.ctx)
	m.w[addr].cancel = cancel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.p.runServer(ctx, addr, first)
	}()
}

// Unwatch removes a listener for addr, and stops polling it after the last one.
func (m *Manager) Unwatch(addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.w[addr]
	if !ok {
		return
	}
	if w.n--; w.n <= 0 {
		w.cancel()
		delete(m.w, addr)
	}
}

// Watching reports whether addr is being polled.
func (m *Manager) Watching(addr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.w[addr]
	return ok
}

// Wait blocks until every polling goroutine has returned. Cancel the context first.
func (m *Manager) Wait() { m.wg.Wait() }
