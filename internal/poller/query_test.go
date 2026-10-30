package poller

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

var header = []byte{0xFF, 0xFF, 0xFF, 0xFF}

func cstr(b *bytes.Buffer, s string) { b.WriteString(s); b.WriteByte(0) }

func infoReply(players, bots byte) []byte {
	var b bytes.Buffer
	b.Write(header)
	b.WriteByte(0x49)
	b.WriteByte(17)
	cstr(&b, "Test Server")
	cstr(&b, "de_dust2")
	cstr(&b, "cstrike")
	cstr(&b, "Counter-Strike")
	b.Write([]byte{0x0A, 0x00}) // app id 10
	b.Write([]byte{players, 24, bots, 'd', 'l', 0, 1})
	cstr(&b, "1.0")
	return b.Bytes()
}

func playerReply(names ...string) []byte {
	var b bytes.Buffer
	b.Write(header)
	b.WriteByte(0x44)
	b.WriteByte(byte(len(names)))
	for i, n := range names {
		b.WriteByte(byte(i))
		cstr(&b, n)
		binary.Write(&b, binary.LittleEndian, int32(5))
		binary.Write(&b, binary.LittleEndian, math.Float32bits(60))
	}
	return b.Bytes()
}

var challenge = append(append([]byte{}, header...), 0x41, 1, 2, 3, 4)

// fakeServer answers A2S requests over UDP. A request without the challenge gets
// a challenge reply, and a request carrying it gets the real reply. info and
// players are the real replies; queries counts packets received.
func fakeServer(t *testing.T, info, players []byte) (addr string, queries *atomic.Int32) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	queries = new(atomic.Int32)
	go func() {
		buf := make([]byte, 1400)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			queries.Add(1)
			req := buf[:n]
			var reply []byte
			switch req[4] {
			case 0x54: // A2S_INFO: "Source Engine Query\0" then the challenge
				if n == 5+len("Source Engine Query")+1 {
					reply = challenge
				} else {
					reply = info
				}
			case 0x55: // A2S_PLAYER
				if bytes.Equal(req[5:], []byte{0xFF, 0xFF, 0xFF, 0xFF}) {
					reply = challenge
				} else {
					reply = players
				}
			}
			pc.WriteTo(reply, from)
		}
	}()
	return pc.LocalAddr().String(), queries
}

func dial(t *testing.T, addr string) Client {
	t.Helper()
	c, err := dialA2S(addr, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestQueryHandshakeAndValidReply(t *testing.T) {
	addr, _ := fakeServer(t, infoReply(3, 1), playerReply("alice", "bob", "bot"))
	st, err := Query(context.Background(), dial(t, addr))
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "Test Server" || st.Map != "de_dust2" || st.Players != 2 || st.MaxPlayers != 24 {
		t.Fatalf("bad state %+v", st)
	}
	if len(st.PlayerNames) != 3 || !st.NamesKnown {
		t.Fatalf("bad names %+v", st)
	}
}

func TestQuerySkipsPlayersWhenOnlyBots(t *testing.T) {
	addr, queries := fakeServer(t, infoReply(2, 2), playerReply("bot1", "bot2"))
	st, err := Query(context.Background(), dial(t, addr))
	if err != nil || st.Players != 0 {
		t.Fatalf("got %+v, %v", st, err)
	}
	if n := queries.Load(); n != 2 { // info request + info request with challenge
		t.Fatalf("expected 2 packets (no A2S_PLAYER), got %d", n)
	}
}

func TestQueryMalformedReply(t *testing.T) {
	addr, queries := fakeServer(t, append(header, 0x49, 17, 'x'), nil) // truncated info
	_, err := Query(context.Background(), dial(t, addr))
	if err == nil {
		t.Fatal("want error for malformed reply")
	}
	if n := queries.Load(); n != 2*(1+Retries) {
		t.Fatalf("expected %d packets over %d attempts, got %d", 2*(1+Retries), 1+Retries, n)
	}
}

// fake is a scripted Client for retry and poller tests.
type fake struct {
	infoErrs int // number of QueryInfo calls that fail first
	calls    atomic.Int32
	players  atomic.Int32
	info     Info
}

func (f *fake) QueryInfo() (*Info, error) {
	if int(f.calls.Add(1)) <= f.infoErrs {
		return nil, context.DeadlineExceeded
	}
	return &f.info, nil
}
func (f *fake) QueryPlayer() (*PlayerList, error) {
	f.players.Add(1)
	return &PlayerList{}, nil
}
func (f *fake) Close() error { return nil }

func TestQueryRetries(t *testing.T) {
	f := &fake{infoErrs: Retries}
	if _, err := Query(context.Background(), f); err != nil {
		t.Fatalf("want success on last retry: %v", err)
	}
	f = &fake{infoErrs: Retries + 1}
	if _, err := Query(context.Background(), f); err == nil {
		t.Fatal("want error after retries are exhausted")
	}
	if n := f.calls.Load(); n != 1+Retries {
		t.Fatalf("want %d attempts, got %d", 1+Retries, n)
	}
}

func TestQueryTimeout(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0") // never answers
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	start := time.Now()
	_, err = Query(context.Background(), dial(t, pc.LocalAddr().String()))
	if err == nil {
		t.Fatal("want timeout error")
	}
	if d := time.Since(start); d < 3*150*time.Millisecond {
		t.Fatalf("returned after %v, want at least 3 timeouts", d)
	}
}

func TestQueryContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fake{}
	if _, err := Query(ctx, f); err == nil || f.calls.Load() != 0 {
		t.Fatalf("want immediate cancel, err=%v calls=%d", err, f.calls.Load())
	}
}

// stubbornServer answers every request with a new challenge extra times before the real reply,
// like a server behind an anti-DDoS filter (B8). Replies of more than splitAt bytes are split.
func stubbornServer(t *testing.T, extra int, info, players []byte, splitAt int) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1400)
		seen := map[byte]int{}
		for {
			_, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			kind := buf[4]
			seen[kind]++
			if seen[kind] <= 1+extra { // the plain request and the extra rounds get a fresh challenge
				pc.WriteTo(append(append([]byte{}, header...), 0x41, byte(seen[kind]), 2, 3, 4), from)
				continue
			}
			reply := info
			if kind == 0x55 {
				reply = players
			}
			if splitAt <= 0 || len(reply) <= splitAt {
				pc.WriteTo(reply, from)
				continue
			}
			var parts [][]byte
			for b := reply; len(b) > 0; {
				k := min(splitAt, len(b))
				parts = append(parts, b[:k])
				b = b[k:]
			}
			for i := len(parts) - 1; i >= 0; i-- { // out of order on purpose
				var p bytes.Buffer
				binary.Write(&p, binary.LittleEndian, int32(-2))
				binary.Write(&p, binary.LittleEndian, int32(77))
				p.Write([]byte{byte(len(parts)), byte(i)})
				binary.Write(&p, binary.LittleEndian, uint16(1248))
				p.Write(parts[i])
				pc.WriteTo(p.Bytes(), from)
			}
		}
	}()
	return pc.LocalAddr().String()
}

func TestQueryAnswersSeveralChallengesInARow(t *testing.T) {
	addr := stubbornServer(t, 3, infoReply(2, 0), playerReply("alice", "bob"), 0)
	c, err := dialA2S(addr, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	st, err := Query(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "Test Server" || st.Players != 2 || len(st.PlayerNames) != 2 || !st.NamesKnown {
		t.Fatalf("state %+v", st)
	}
}

func TestQueryGivesUpAfterTooManyChallenges(t *testing.T) {
	addr := stubbornServer(t, 100, infoReply(0, 0), nil, 0)
	c, err := dialA2S(addr, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.QueryInfo(); !errors.Is(err, ErrTooManyChallenges) {
		t.Fatalf("err %v, want ErrTooManyChallenges", err)
	}
}

func TestQueryJoinsASplitPlayerList(t *testing.T) {
	var names []string
	for i := range 64 {
		names = append(names, fmt.Sprintf("player-with-a-long-name-%02d", i))
	}
	addr := stubbornServer(t, 1, infoReply(64, 0), playerReply(names...), 500)
	c, err := dialA2S(addr, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pl, err := c.QueryPlayer()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pl.Names, names) {
		t.Fatalf("got %d names, first %q", len(pl.Names), pl.Names[:min(3, len(pl.Names))])
	}
}

func TestParseRefusesTruncatedReplies(t *testing.T) {
	full := infoReply(3, 1)[5:]                                                         // after the header and type byte
	need := 1 + len("Test Server\x00de_dust2\x00cstrike\x00Counter-Strike\x00") + 2 + 3 // up to the bot count
	for i := range need {
		if _, err := parseInfo(full[:i]); err == nil {
			t.Fatalf("parseInfo accepted %d of the %d bytes it needs", i, need)
		}
	}
	if in, err := parseInfo(full[:need]); err != nil || in.Players != 3 || in.Bots != 1 {
		t.Fatalf("parseInfo(%d bytes) = %+v, %v", need, in, err)
	}
	pl := playerReply("alice", "bob")[5:]
	if _, err := parsePlayers(pl[:len(pl)-3]); err == nil {
		t.Fatal("parsePlayers accepted a cut reply")
	}
}
