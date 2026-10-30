package poller

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// Our own A2S client (B8). It replaces go-a2s, which answers only one challenge: some servers
// behind an anti-DDoS filter send a new challenge in reply to the first one, sometimes several
// times, and go-a2s then failed with "Unsupported protocol header". This one keeps answering
// challenges, up to MaxChallenges, on the same socket.

// MaxChallenges is how many challenges one query answers before it gives up.
const MaxChallenges = 5

const (
	a2sInfoRequest   = 'T'
	a2sInfoReply     = 'I'
	a2sPlayerRequest = 'U'
	a2sPlayerReply   = 'D'
	a2sChallenge     = 'A'

	singlePacket = -1
	splitPacket  = -2
	maxPacket    = 1400 // what a Source server sends at most per UDP packet
	maxSplits    = 32   // a reply of more parts than this is refused
)

var (
	// ErrTooManyChallenges means the server kept answering with a new challenge.
	ErrTooManyChallenges = errors.New("a2s: too many challenges")
	// ErrCompressed means a split reply is compressed, which only very old engines do.
	ErrCompressed = errors.New("a2s: compressed reply is not supported")
)

// Info is the part of an A2S_INFO reply the poller uses.
type Info struct {
	Name, Map, Folder, Game string
	AppID                   uint16
	Players, MaxPlayers     uint8
	Bots                    uint8
}

// PlayerList is an A2S_PLAYER reply: the names, in the server's order.
type PlayerList struct {
	Names []string
}

// a2sClient queries one server over one UDP socket.
type a2sClient struct {
	conn    net.Conn
	timeout time.Duration
	buf     []byte
}

func dialA2S(addr string, timeout time.Duration) (*a2sClient, error) {
	conn, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return nil, err
	}
	return &a2sClient{conn: conn, timeout: timeout, buf: make([]byte, 65536)}, nil
}

func (c *a2sClient) Close() error { return c.conn.Close() }

// QueryInfo asks for the server's name, map and player count.
func (c *a2sClient) QueryInfo() (*Info, error) {
	req := append([]byte{0xFF, 0xFF, 0xFF, 0xFF, a2sInfoRequest}, "Source Engine Query\x00"...)
	data, err := c.query(req, a2sInfoReply, func(ch []byte) []byte {
		return append(append([]byte{}, req...), ch...) // the challenge goes after the request
	})
	if err != nil {
		return nil, err
	}
	return parseInfo(data)
}

// QueryPlayer asks for the names of the players on the server.
func (c *a2sClient) QueryPlayer() (*PlayerList, error) {
	req := []byte{0xFF, 0xFF, 0xFF, 0xFF, a2sPlayerRequest, 0xFF, 0xFF, 0xFF, 0xFF}
	data, err := c.query(req, a2sPlayerReply, func(ch []byte) []byte {
		return append([]byte{0xFF, 0xFF, 0xFF, 0xFF, a2sPlayerRequest}, ch...) // the challenge replaces -1
	})
	if err != nil {
		return nil, err
	}
	return parsePlayers(data)
}

// query sends req and answers challenges until the reply of type want arrives. It returns the
// reply's payload after the type byte.
func (c *a2sClient) query(req []byte, want byte, withChallenge func([]byte) []byte) ([]byte, error) {
	for range MaxChallenges + 1 {
		if err := c.send(req); err != nil {
			return nil, err
		}
		data, err := c.receive()
		if err != nil {
			return nil, err
		}
		if len(data) < 1 {
			return nil, errors.New("a2s: empty reply")
		}
		switch data[0] {
		case want:
			return data[1:], nil
		case a2sChallenge:
			if len(data) < 5 {
				return nil, errors.New("a2s: short challenge")
			}
			req = withChallenge(data[1:5])
		default:
			return nil, fmt.Errorf("a2s: unexpected reply type 0x%02x", data[0])
		}
	}
	return nil, ErrTooManyChallenges
}

func (c *a2sClient) send(b []byte) error {
	c.conn.SetWriteDeadline(time.Now().Add(c.timeout))
	_, err := c.conn.Write(b)
	return err
}

// receive reads one reply, putting a split one back together, and returns it without the -1 header.
func (c *a2sClient) receive() ([]byte, error) {
	pkt, err := c.read()
	if err != nil {
		return nil, err
	}
	switch int32(binary.LittleEndian.Uint32(pkt)) {
	case singlePacket:
		return pkt[4:], nil
	case splitPacket:
		whole, err := c.joinSplit(pkt)
		if err != nil {
			return nil, err
		}
		if len(whole) < 4 || int32(binary.LittleEndian.Uint32(whole)) != singlePacket {
			return nil, errors.New("a2s: bad header in split reply")
		}
		return whole[4:], nil
	}
	return nil, errors.New("a2s: bad packet header")
}

func (c *a2sClient) read() ([]byte, error) {
	c.conn.SetReadDeadline(time.Now().Add(c.timeout))
	n, err := c.conn.Read(c.buf)
	if err != nil {
		return nil, err
	}
	if n < 5 {
		return nil, errors.New("a2s: short packet")
	}
	return append([]byte{}, c.buf[:n]...), nil
}

// joinSplit collects the parts of a split reply (the Source format: id, total, number, size)
// and joins them in order. first is the part already read.
func (c *a2sClient) joinSplit(first []byte) ([]byte, error) {
	var id int32
	var total int
	parts := map[int][]byte{}
	add := func(pkt []byte) error {
		if len(pkt) < 12 {
			return errors.New("a2s: short split packet")
		}
		pid := int32(binary.LittleEndian.Uint32(pkt[4:]))
		if uint32(pid)&0x80000000 != 0 {
			return ErrCompressed
		}
		if total == 0 {
			id, total = pid, int(pkt[8])
			if total < 1 || total > maxSplits {
				return fmt.Errorf("a2s: split reply of %d parts", total)
			}
		}
		if pid != id {
			return nil // a part of another reply, from an earlier try: ignore it
		}
		num := int(pkt[9])
		if num >= total {
			return errors.New("a2s: split part out of range")
		}
		parts[num] = pkt[12:] // after id, total, number and the 2-byte size
		return nil
	}
	if err := add(first); err != nil {
		return nil, err
	}
	for len(parts) < total {
		pkt, err := c.read()
		if err != nil {
			return nil, err
		}
		if int32(binary.LittleEndian.Uint32(pkt)) != splitPacket {
			continue
		}
		if err := add(pkt); err != nil {
			return nil, err
		}
	}
	var whole bytes.Buffer
	for i := range total {
		whole.Write(parts[i])
	}
	return whole.Bytes(), nil
}

// reader reads A2S fields. Reading past the end sets err instead of panicking.
type reader struct {
	b   []byte
	err error
}

func (r *reader) byte() byte {
	if r.err != nil || len(r.b) < 1 {
		r.fail()
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

func (r *reader) uint16() uint16 {
	if r.err != nil || len(r.b) < 2 {
		r.fail()
		return 0
	}
	v := binary.LittleEndian.Uint16(r.b)
	r.b = r.b[2:]
	return v
}

func (r *reader) skip(n int) {
	if r.err != nil || len(r.b) < n {
		r.fail()
		return
	}
	r.b = r.b[n:]
}

func (r *reader) string() string {
	if r.err != nil {
		return ""
	}
	i := bytes.IndexByte(r.b, 0)
	if i < 0 {
		r.fail()
		return ""
	}
	s := string(r.b[:i])
	r.b = r.b[i+1:]
	return s
}

func (r *reader) fail() {
	if r.err == nil {
		r.err = errors.New("a2s: truncated reply")
	}
}

// parseInfo reads an A2S_INFO reply after its type byte. Only the fields up to the bot count
// are needed; the rest (server type, OS, VAC, version and the extra data) is not read.
func parseInfo(b []byte) (*Info, error) {
	r := &reader{b: b}
	r.byte() // protocol
	in := &Info{Name: r.string(), Map: r.string(), Folder: r.string(), Game: r.string()}
	in.AppID = r.uint16()
	in.Players, in.MaxPlayers, in.Bots = r.byte(), r.byte(), r.byte()
	if r.err != nil {
		return nil, r.err
	}
	return in, nil
}

// parsePlayers reads an A2S_PLAYER reply after its type byte: a count, then for each player an
// index, the name, the score (4 bytes) and the time on the server (4 bytes). The Ship's extra
// fields come after all players and are not read.
func parsePlayers(b []byte) (*PlayerList, error) {
	r := &reader{b: b}
	n := int(r.byte())
	pl := &PlayerList{}
	for range n {
		r.byte() // index
		name := r.string()
		r.skip(8) // score and duration
		if r.err != nil {
			return nil, r.err
		}
		pl.Names = append(pl.Names, name)
	}
	if r.err != nil {
		return nil, r.err
	}
	return pl, nil
}
