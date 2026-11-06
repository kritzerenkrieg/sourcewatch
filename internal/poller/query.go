// Package poller queries Source servers over A2S and tracks their online state (B1-B4).
package poller

import (
	"context"
	"fmt"
	"strings"
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
	// NamesKnown is false when the A2S_PLAYER query failed. Players is then 0 and not known either.
	NamesKnown bool
}

// Query runs A2S_INFO and A2S_PLAYER. Each query is tried 1+Retries times. A failed A2S_PLAYER
// does not fail the poll, but leaves the players unknown.
//
// The player count comes from the player list, not from A2S_INFO: some servers report fake
// players there that are not in the list (B9). The list without empty names (players still
// connecting) and without SourceTV is counted, minus the bots A2S_INFO reports. SourceTV is
// itself a bot to the engine, so when it is listed it is taken to be one of those bots.
func Query(ctx context.Context, c Client) (State, error) {
	info, err := retry(ctx, c.QueryInfo)
	if err != nil {
		return State{}, err
	}
	st := State{Name: info.Name, Map: info.Map, MaxPlayers: int(info.MaxPlayers)}
	pi, err := retry(ctx, c.QueryPlayer)
	if err != nil {
		return st, nil
	}
	bots := int(info.Bots)
	for _, n := range pi.Names {
		switch {
		case strings.TrimSpace(n) == "":
		case info.SourceTV != "" && n == info.SourceTV:
			if bots > 0 {
				bots--
			}
		default:
			st.PlayerNames = append(st.PlayerNames, n)
		}
	}
	st.Players = max(len(st.PlayerNames)-bots, 0)
	st.NamesKnown = true
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
