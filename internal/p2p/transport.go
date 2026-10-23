package p2p

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"time"
)

// The TCP layer. Every read and write has a deadline — without one, a peer that connects and sends
// nothing keeps a slot forever (the cheapest availability attack there is).

const (
	// DialTimeout is the time allowed to establish a connection.
	DialTimeout = 10 * time.Second
	// ReadTimeout is the time the next message has to arrive within (ping/pong keeps it alive).
	ReadTimeout = 90 * time.Second
	// WriteTimeout is the time allowed to send a message.
	WriteTimeout = 30 * time.Second
	// PingInterval is how often the node sends a ping. It is sent even on a connection with traffic
	// — it is 32 bytes and it does not measure idleness but whether the peer still answers.
	//
	// It has to be considerably less than ReadTimeout: with a block every 30 s a connection is idle
	// most of the time, so without a ping every connection dies on the read deadline and the nodes
	// reconnect forever.
	PingInterval = 30 * time.Second
	// PingTimeout is the time a pong has to arrive within. A peer that answers other messages but
	// not a ping is as useless as a dead one — it just looks different.
	PingTimeout = 90 * time.Second
	// PingPayloadSize is the length of both the ping and the pong message: a nonce, little-endian.
	PingPayloadSize = 8
)

// ErrBadPong is a pong that does not belong to any ping of ours.
var ErrBadPong = errors.New("p2p: a pong with an unknown nonce")

// Conn is a connection to one peer.
type Conn struct {
	c     net.Conn
	magic [4]byte

	// wmu keeps writes one at a time. Several goroutines write into one connection (the ping loop,
	// the announcement of a new block, answers from the read loop) and two frames must not be
	// interleaved — the peer would get one message's header with another's body and drop the connection.
	wmu sync.Mutex

	// Keepalive: the nonce of the last ping sent and the time it left. While the nonce is not zero we
	// are waiting for an answer and no further ping is sent — otherwise there would be no way to
	// measure how long the peer has been silent.
	pmu       sync.Mutex
	pingNonce uint64
	pingSent  time.Time
}

func NewConn(c net.Conn, magic [4]byte) *Conn {
	return &Conn{c: c, magic: magic}
}

// Dial opens a connection to a peer.
func Dial(hostport string, magic [4]byte) (*Conn, error) {
	c, err := net.DialTimeout("tcp", hostport, DialTimeout)
	if err != nil {
		return nil, err
	}
	return NewConn(c, magic), nil
}

// Listen opens a listening socket.
func Listen(hostport string) (net.Listener, error) {
	return net.Listen("tcp", hostport)
}

func (c *Conn) Send(msg *Message) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.c.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		return err
	}
	return Encode(c.c, c.magic, msg)
}

// PingMessage records a new nonce and returns the message to send. When the previous ping is still
// being waited for, it returns nil — the time is measured from the first unanswered ping, not from
// the last one.
//
// Recording the time is separated from sending on purpose: on a stuck connection sending can take up
// to WriteTimeout, so the caller does it outside its own loop. If the time were recorded there, a
// silent peer would push its deadline back every time and it would never expire.
func (c *Conn) PingMessage(now time.Time) (*Message, error) {
	var b [PingPayloadSize]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	nonce := binary.LittleEndian.Uint64(b[:])
	if nonce == 0 {
		nonce = 1 // zero means "we are waiting for nothing"
		binary.LittleEndian.PutUint64(b[:], nonce)
	}

	c.pmu.Lock()
	defer c.pmu.Unlock()
	if c.pingNonce != 0 {
		return nil, nil
	}
	c.pingNonce = nonce
	c.pingSent = now
	return &Message{Command: CmdPing, Payload: b[:]}, nil
}

// Ping records and immediately sends a ping. A caller that must not wait on a socket write should
// use PingMessage and send the message itself.
func (c *Conn) Ping(now time.Time) error {
	msg, err := c.PingMessage(now)
	if err != nil || msg == nil {
		return err
	}
	if err := c.Send(msg); err != nil {
		c.ForgetPing()
		return err
	}
	return nil
}

// ForgetPing cancels the wait for an answer. It is used when a ping could not be sent — otherwise
// the connection would expire over something the peer never received.
func (c *Conn) ForgetPing() {
	c.pmu.Lock()
	c.pingNonce, c.pingSent = 0, time.Time{}
	c.pmu.Unlock()
}

// Pong pairs an answer with our ping. A pong that belongs to nothing is an error: it is only sent on
// request, so an honest peer does not send one on its own.
func (c *Conn) Pong(payload []byte, now time.Time) error {
	if len(payload) != PingPayloadSize {
		return ErrBadPong
	}
	nonce := binary.LittleEndian.Uint64(payload)
	c.pmu.Lock()
	defer c.pmu.Unlock()
	if nonce == 0 || nonce != c.pingNonce {
		return ErrBadPong
	}
	c.pingNonce, c.pingSent = 0, time.Time{}
	return nil
}

// PingOverdue says that no answer to a ping arrived within PingTimeout. The caller should hang the
// connection up — the read loop then ends on an error and cleans up after itself.
func (c *Conn) PingOverdue(now time.Time) bool {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	return c.pingNonce != 0 && now.Sub(c.pingSent) > PingTimeout
}

func (c *Conn) Recv() (*Message, error) {
	if err := c.c.SetReadDeadline(time.Now().Add(ReadTimeout)); err != nil {
		return nil, err
	}
	return Decode(c.c, c.magic)
}

func (c *Conn) Close() error { return c.c.Close() }

// RemoteAddress returns the peer's address in the form the addrman works with.
func (c *Conn) RemoteAddress() Address {
	if tcp, ok := c.c.RemoteAddr().(*net.TCPAddr); ok {
		return Address{IP: tcp.IP, Port: uint16(tcp.Port), LastSeen: time.Now()}
	}
	return Address{LastSeen: time.Now()}
}

// Handshake exchanges version and verack. The initiator sends version first.
//
// The order is fixed: whoever calls introduces themselves first. Thanks to that the node does not
// have to allocate anything before it knows who it is talking to.
func (c *Conn) Handshake(local *VersionMsg, ourNonce uint64, initiator bool) (*VersionMsg, error) {
	deadline := time.Now().Add(HandshakeTimeout)
	if err := c.c.SetDeadline(deadline); err != nil {
		return nil, err
	}
	defer c.c.SetDeadline(time.Time{})

	payload, err := local.Encode()
	if err != nil {
		return nil, err
	}
	sendVersion := func() error { return Encode(c.c, c.magic, &Message{Command: CmdVersion, Payload: payload}) }

	if initiator {
		if err := sendVersion(); err != nil {
			return nil, err
		}
	}

	msg, err := Decode(c.c, c.magic)
	if err != nil {
		return nil, err
	}
	if msg.Command != CmdVersion {
		return nil, ErrBadVersionMsg
	}
	remote, err := DecodeVersion(msg.Payload)
	if err != nil {
		return nil, err
	}
	if remote.Nonce == ourNonce {
		return nil, ErrSelfConnection
	}
	if remote.Protocol < MinProtocolVersion {
		return nil, ErrOldProtocol
	}

	if !initiator {
		if err := sendVersion(); err != nil {
			return nil, err
		}
	}
	if err := Encode(c.c, c.magic, &Message{Command: CmdVerack}); err != nil {
		return nil, err
	}
	ack, err := Decode(c.c, c.magic)
	if err != nil {
		return nil, err
	}
	if ack.Command != CmdVerack {
		return nil, ErrBadVersionMsg
	}
	return remote, nil
}
