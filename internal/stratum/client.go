package stratum

import (
	"bufio"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"powmachina/internal/finkey"
	"powmachina/internal/mh"
	"powmachina/internal/safetext"
	"powmachina/internal/work"
)

// Client is the miner's stratum client — it implements miner.Feed. On an outage it reconnects.
type Client struct {
	URL   string // stratum+tcp://host:port or stratum+ssl://host:port (or host:port)
	Login string // address[.worker]
	Pass  string
	Agent string
	Log   *log.Logger
	// Finality is the miner's own finality key (nil = the server's): the blocks the miner finds carry
	// it, and the miner signs when the server says the key has a seat (`attest`). The signer never
	// signs two blocks at one height, whatever the server asks.
	Finality *finkey.Signer

	mu      sync.Mutex
	conn    net.Conn
	enc     *json.Encoder
	nextID  uint64
	pending map[uint64]chan *Response
}

// maxServerText is how much of a text the server wrote the client keeps: the name of the network,
// the reason for a pause or for refusing a share. They are a few words; the cap is for a server
// that sends a few kilobytes instead.
const maxServerText = 200

// fromServer makes a text the server wrote fit to print. The miner shows these texts on a terminal
// and writes them to its log, and the server — or whoever sits on the wire of a connection without
// TLS — chooses every byte of them.
func fromServer(s string) string { return safetext.Line(s, maxServerText) }

func (c *Client) logf(format string, a ...any) {
	if c.Log != nil {
		c.Log.Printf(format, a...)
	}
}

func (c *Client) dial() (net.Conn, error) {
	u := c.URL
	useTLS := false
	switch {
	case strings.HasPrefix(u, "stratum+ssl://"), strings.HasPrefix(u, "stratum+tls://"):
		useTLS, u = true, u[strings.Index(u, "://")+3:]
	case strings.HasPrefix(u, "stratum+tcp://"):
		u = u[len("stratum+tcp://"):]
	}
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	if useTLS {
		host, _, _ := net.SplitHostPort(u)
		return tls.DialWithDialer(d, "tcp", u, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	}
	return d.Dial("tcp", u)
}

// Run implements miner.Feed: it connects, logs in and hands the jobs from the server to the miner.
func (c *Client) Run(lanes []mh.Lane, set func(mh.Lane, *work.Job), quit <-chan struct{}) error {
	backoff := time.Second
	for {
		err := c.session(lanes, set, quit)
		for _, l := range lanes {
			set(l, nil)
		}
		select {
		case <-quit:
			return nil
		default:
		}
		var se *Error
		if errors.As(err, &se) && se.Code == ErrCodeBadLogin {
			return fmt.Errorf("stratum login rejected: %s", se.Message)
		}
		// The error can quote what the server sent, a line that would not parse for one.
		c.logf("stratum: %s — reconnecting in %v", safetext.Line(fmt.Sprint(err), 2*maxServerText), backoff)
		select {
		case <-quit:
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *Client) session(lanes []mh.Lane, set func(mh.Lane, *work.Job), quit <-chan struct{}) error {
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer conn.Close()
	c.mu.Lock()
	c.conn, c.enc, c.pending = conn, json.NewEncoder(conn), map[uint64]chan *Response{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		for _, ch := range c.pending {
			close(ch)
		}
		c.conn, c.enc, c.pending = nil, nil, nil
		c.mu.Unlock()
	}()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-quit:
			conn.Close()
		case <-done:
		}
	}()

	ls := make([]string, len(lanes))
	for i, l := range lanes {
		ls[i] = l.String()
	}
	lp := LoginParams{Login: c.Login, Pass: c.Pass, Agent: c.Agent, Lanes: ls}
	if c.Finality != nil {
		lp.FinalityKey = hex.EncodeToString(c.Finality.Pub[:])
	}
	if _, err := c.send("login", lp); err != nil {
		return err
	}
	r := bufio.NewReaderSize(conn, MaxLine)
	var loggedIn bool
	keep := time.NewTicker(60 * time.Second)
	defer keep.Stop()
	go func() {
		for {
			select {
			case <-done:
				return
			case <-keep.C:
				_, _ = c.send("keepalived", struct{}{})
			}
		}
	}()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		line, err := r.ReadSlice('\n')
		if err != nil {
			return err
		}
		var msg struct {
			ID     *uint64         `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *Error          `json:"error"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			return fmt.Errorf("bad message from server: %w", err)
		}
		if msg.Error != nil {
			msg.Error.Message = fromServer(msg.Error.Message)
		}
		if msg.Method == "" && msg.ID != nil {
			resp := &Response{ID: *msg.ID, Result: msg.Result, Error: msg.Error}
			c.deliver(resp)
			if !loggedIn {
				if resp.Error != nil {
					return resp.Error
				}
				var res LoginResult
				_ = json.Unmarshal(resp.Result, &res)
				c.logf("stratum: connected to %s (%s)", c.URL, fromServer(res.Network))
				if res.Notice != "" {
					c.logf("UPDATE: %s", fromServer(res.Notice))
				}
				loggedIn = true
			}
			continue
		}
		switch msg.Method {
		case "job":
			var p JobParams
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				return err
			}
			j, err := JobFrom(&p)
			if err != nil {
				return err
			}
			set(j.Lane, j)
		case "attest":
			var p AttestParams
			if err := json.Unmarshal(msg.Params, &p); err == nil {
				go c.attest(p)
			}
		case "pause":
			var p PauseParams
			if err := json.Unmarshal(msg.Params, &p); err == nil {
				if l, ok := ParseLane(p.Lane); ok {
					set(l, nil)
					c.logf("lane %s: paused by server: %s", l, fromServer(p.Reason))
				}
			}
		}
	}
}

func (c *Client) send(method string, params any) (chan *Response, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enc == nil {
		return nil, errors.New("not connected")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan *Response, 1)
	c.pending[id] = ch
	_ = c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if err := c.enc.Encode(Request{ID: &id, Method: method, Params: raw}); err != nil {
		delete(c.pending, id)
		return nil, err
	}
	return ch, nil
}

func (c *Client) deliver(r *Response) {
	c.mu.Lock()
	ch := c.pending[r.ID]
	delete(c.pending, r.ID)
	c.mu.Unlock()
	if ch != nil {
		ch <- r
	}
}

// attest signs the block the server named and sends the signature back. A server that asks for a
// second block at a height the key already signed is told nothing: the signer refuses, and the log
// says so — an honest server never asks for that.
func (c *Client) attest(p AttestParams) {
	if c.Finality == nil {
		return
	}
	raw, err := hex.DecodeString(p.Block)
	if err != nil || len(raw) != 32 {
		return
	}
	var id [32]byte
	copy(id[:], raw)
	a, err := c.Finality.Sign(p.Height, id)
	if err != nil {
		c.logf("finality: not signing height %d (%x…): %v", p.Height, id[:6], err)
		return
	}
	ch, err := c.send("attestation", AttestationParams{Height: a.Height, Block: p.Block, Signature: hex.EncodeToString(a.Signature)})
	if err != nil {
		return
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return
		}
		if r.Error != nil {
			c.logf("finality: the server refused the signature of height %d: %s", p.Height, r.Error.Message)
			return
		}
		c.logf("finality: signed height %d (%x…)", p.Height, id[:6])
	case <-time.After(30 * time.Second):
	}
}

// ReportHashrate tells the server how fast a lane is hashing. Nothing waits for the answer: a server
// that does not know the method says so, and the miner mines on.
func (c *Client) ReportHashrate(lane mh.Lane, hps float64) {
	_, _ = c.send("hashrate", HashrateParams{Lane: lane.String(), Hashrate: hps})
}

// Submit implements miner.Feed.
func (c *Client) Submit(j *work.Job, nonce uint64) (bool, bool, error) {
	ch, err := c.send("submit", SubmitParams{JobID: j.ID, Nonce: FormatNonce(nonce)})
	if err != nil {
		return false, false, err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return false, false, errors.New("connection closed")
		}
		if r.Error != nil {
			if r.Error.Code != ErrCodeStale && r.Error.Code != ErrCodeRateLimit {
				c.logf("lane %s: share rejected: %s", j.Lane, r.Error.Message)
			}
			return false, false, nil
		}
		var res SubmitResult
		_ = json.Unmarshal(r.Result, &res)
		if res.Block {
			c.logf("BLOCK FOUND: lane %s, height %d", j.Lane, j.Height)
		}
		return true, res.Block, nil
	case <-time.After(30 * time.Second):
		return false, false, errors.New("submit timed out")
	}
}
