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
// Source says where State came from, and DataAt when that data was taken: the poll
// time for A2S, the scan time for GameTracker.
type Update struct {
	Addr       string
	Online     bool
	Transition bool
	State      State
	Err        error
	Source     string
	DataAt     time.Time
}

type Poller struct {
	Dial     Dialer
	Interval time.Duration // 60 s (B3)
	Jitter   time.Duration // ±5 s (B3)
	OnUpdate func(Update)

	// Fallback, when set, is asked for a server that A2S has failed OfflineAfter
	// times in a row, before that server is declared offline (B5).
	Fallback Fallback
	// UseFallback, when set, is asked before each use of the fallback for a server, and says whether
	// it may be used for it (E22). Nil means always.
	UseFallback   func(addr string) bool
	FallbackEvery time.Duration // default FallbackEvery
	MaxDataAge    time.Duration // default MaxGameTrackerAge
	Now           func() time.Time
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

// fbState is the fallback bookkeeping for one server.
type fbState struct {
	lastFetch time.Time
	lastScan  time.Time
	lastOK    bool // the last fetch said the server is alive, with fresh data
}

type fbOutcome int

const (
	fbDown    fbOutcome = iota // no usable data: the server is offline
	fbNothing                  // alive, but nothing new since the last update
	fbApplied                  // alive, with a newer scan
)

// runServer polls addr until ctx is cancelled. The first poll comes after first.
func (p *Poller) runServer(ctx context.Context, addr string, first time.Duration) {
	if !sleep(ctx, first) {
		return
	}
	failures, known, online := 0, false, false
	var fb fbState
	for {
		st, err := p.cycle(ctx, addr)
		if ctx.Err() != nil {
			return
		}
		u := Update{Addr: addr, State: st, Err: err, Source: SourceA2S, DataAt: p.now()}
		emit := true
		if err == nil {
			failures = 0
			fb = fbState{} // A2S works again, so the fallback starts over if it is ever needed
			u.Transition = !known || !online
			online, known = true, true
		} else {
			failures++
			outcome := fbDown
			var res GTResult
			if p.Fallback != nil && failures >= OfflineAfter && (p.UseFallback == nil || p.UseFallback(addr)) {
				res, outcome = p.tryFallback(ctx, addr, &fb)
			}
			switch outcome {
			case fbApplied:
				u.State, u.Err, u.Source, u.DataAt = res.State, nil, SourceGameTracker, res.ScannedAt
				u.Transition = !known || !online
				online, known = true, true
			case fbNothing:
				emit = false
			default:
				if failures >= OfflineAfter && (!known || online) {
					u.Transition = true
					online, known = false, true
				}
			}
		}
		u.Online = online
		if emit {
			p.OnUpdate(u)
		}
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

// tryFallback asks the fallback about a server A2S cannot reach, at most once per
// FallbackEvery. Data is used only when the page says Alive, its scan is recent
// enough, and it is newer than the one already used.
func (p *Poller) tryFallback(ctx context.Context, addr string, fb *fbState) (GTResult, fbOutcome) {
	every, maxAge := p.FallbackEvery, p.MaxDataAge
	if every <= 0 {
		every = FallbackEvery
	}
	if maxAge <= 0 {
		maxAge = MaxGameTrackerAge
	}
	now := p.now()
	if !fb.lastFetch.IsZero() && now.Sub(fb.lastFetch) < every {
		if fb.lastOK {
			return GTResult{}, fbNothing
		}
		return GTResult{}, fbDown
	}
	fb.lastFetch = now
	res, err := p.Fallback.Fetch(ctx, addr, now)
	if err != nil || !res.Alive || res.Age > maxAge {
		fb.lastOK = false
		return GTResult{}, fbDown
	}
	fb.lastOK = true
	// The same scan shows up in every fetch until GameTracker scans again. The page
	// gives the age in whole seconds, so allow for that rounding.
	if !fb.lastScan.IsZero() && !res.ScannedAt.After(fb.lastScan.Add(5*time.Second)) {
		return GTResult{}, fbNothing
	}
	fb.lastScan = res.ScannedAt
	return res, fbApplied
}

func (p *Poller) cycle(ctx context.Context, addr string) (State, error) {
	c, err := p.Dial(addr)
	if err != nil {
		return State{}, err
	}
	defer c.Close()
	return Query(ctx, c)
}
