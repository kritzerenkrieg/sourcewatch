package poller

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

const OfflineAfter = 3

// Update is reported after every cycle. Transition is true when Online changed
// (including the first result for a server). Err is set when the cycle failed.
type Update struct {
	Addr       string
	Online     bool
	Transition bool
	State      State
	Err        error
}

type Poller struct {
	Dial     Dialer
	Interval time.Duration // 60 s (B3)
	Jitter   time.Duration // ±5 s (B3)
	OnUpdate func(Update)

	Now func() time.Time
}

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// New returns a Poller with the B3 values.
func New(onUpdate func(Update)) *Poller {
	return &Poller{Dial: DialA2S, Interval: 60 * time.Second, Jitter: 5 * time.Second, OnUpdate: onUpdate}
}

// Delay is the wait before the next cycle: interval ± jitter, with r in [0,1).
func Delay(interval, jitter time.Duration, r float64) time.Duration {
	return interval + time.Duration((r*2-1)*float64(jitter))
}

// Run polls every address in its own goroutine until ctx is cancelled.
func (p *Poller) Run(ctx context.Context, addrs []string) {
	var wg sync.WaitGroup
	for _, a := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.runServer(ctx, a, 0)
		}()
	}
	wg.Wait()
}

// runServer polls addr until ctx is cancelled. The first poll comes after first.
func (p *Poller) runServer(ctx context.Context, addr string, first time.Duration) {
	if !sleep(ctx, first) {
		return
	}
	failures, known, online := 0, false, false
	for {
		st, err := p.cycle(ctx, addr)
		if ctx.Err() != nil {
			return
		}
		u := Update{Addr: addr, State: st, Err: err}
		if err == nil {
			failures = 0
			u.Transition = !known || !online
			online, known = true, true
		} else {
			failures++
			if failures >= OfflineAfter && (!known || online) {
				u.Transition = true
				online, known = false, true
			}
		}
		u.Online = online
		p.OnUpdate(u)
		if !sleep(ctx, Delay(p.Interval, p.Jitter, rand.Float64())) {
			return
		}
	}
}

// sleep waits for d, and reports false when ctx is cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (p *Poller) cycle(ctx context.Context, addr string) (State, error) {
	c, err := p.Dial(addr)
	if err != nil {
		return State{}, err
	}
	defer c.Close()
	return Query(ctx, c)
}
