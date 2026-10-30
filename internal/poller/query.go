// Package poller queries Source servers over A2S and tracks their online state (B1-B4).
package poller

import (
	"context"
	"fmt"
	"time"
)

const (
	Timeout = 2 * time.Second
	Retries = 2
)

// Client queries one game server. The production one is our own A2S client (B8).
type Client interface {
	QueryInfo() (*Info, error)
	QueryPlayer() (*PlayerList, error)
	Close() error
}

// Dialer opens a client for one query cycle.
type Dialer func(addr string) (Client, error)

// DialA2S is the production Dialer. The client has no context or retries, so Query adds them.
func DialA2S(addr string) (Client, error) {
	c, err := dialA2S(addr, Timeout)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// State is one successful poll. Players excludes bots.
type State struct {
	Name        string
	Map         string
	Players     int
	MaxPlayers  int
	PlayerNames []string
	// NamesKnown is false when players were present but the A2S_PLAYER query failed.
	NamesKnown bool
}

// Query runs A2S_INFO, and A2S_PLAYER only when at least one non-bot player is present.
// Each query is tried 1+Retries times. A failed A2S_PLAYER does not fail the poll.
func Query(ctx context.Context, c Client) (State, error) {
	info, err := retry(ctx, c.QueryInfo)
	if err != nil {
		return State{}, err
	}
	humans := int(info.Players) - int(info.Bots)
	if humans < 0 {
		humans = 0
	}
	st := State{Name: info.Name, Map: info.Map, Players: humans, MaxPlayers: int(info.MaxPlayers), NamesKnown: true}
	if humans == 0 {
		return st, nil
	}
	pi, err := retry(ctx, c.QueryPlayer)
	if err != nil {
		st.NamesKnown = false
		return st, nil
	}
	st.PlayerNames = append(st.PlayerNames, pi.Names...)
	return st, nil
}

func retry[T any](ctx context.Context, f func() (T, error)) (v T, err error) {
	for i := 0; i <= Retries; i++ {
		if err = ctx.Err(); err != nil {
			return v, err
		}
		if v, err = safe(f); err == nil {
			return v, nil
		}
	}
	return v, err
}

// safe turns a panic in a client into an error, so one bad reply cannot stop the poller.
func safe[T any](f func() (T, error)) (v T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("malformed reply: %v", r)
		}
	}()
	return f()
}
