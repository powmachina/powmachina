package stratum

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"powmachina/internal/mh"
	"powmachina/internal/work"
)

// Client is the miner's stratum client — it implements miner.Feed. On an outage it reconnects.
type Client struct {
	URL   string // stratum+tcp://host:port or stratum+ssl://host:port (or host:port)
	Login string // address[.worker]
	Pass  string
	Agent string
	Log   *log.Logger

	mu      sync.Mutex
	conn    net.Conn
	enc     *json.Encoder
	nextID  uint64
	pending map[uint64]chan *Response
}

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
		c.logf("stratum: %v — reconnecting in %v", err, backoff)
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
	if _, err := c.send("login", LoginParams{Login: c.Login, Pass: c.Pass, Agent: c.Agent, Lanes: ls}); err != nil {
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
		if msg.Method == "" && msg.ID != nil {
			resp := &Response{ID: *msg.ID, Result: msg.Result, Error: msg.Error}
			c.deliver(resp)
			if !loggedIn {
				if resp.Error != nil {
					return resp.Error
				}
				var res LoginResult
				_ = json.Unmarshal(resp.Result, &res)
				c.logf("stratum: connected to %s (%s)", c.URL, res.Network)
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
		case "pause":
			var p PauseParams
			if err := json.Unmarshal(msg.Params, &p); err == nil {
				if l, ok := ParseLane(p.Lane); ok {
					set(l, nil)
					c.logf("lane %s: paused by server: %s", p.Lane, p.Reason)
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
