package main

import (
	"errors"
	"sync"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/p2p"
)

// Shares travel in the compact form (p2p/wire_cshare.go): every connection has its own
// dictionaries, and the shares for a peer wait shareFlushDelay so that they go out together — a
// share is 14 B, a message frame 24 B.

const (
	// shareFlushDelay is how long a share waits for others to the same peer. A tenth of a second
	// per hop is nothing next to the 60 s of a lane's block.
	shareFlushDelay = 100 * time.Millisecond
	// shareBatchMax: a share takes at most three items (a key, a job, itself).
	shareBatchMax = p2p.MaxShareItemsPerMessage / 3
	// sharePendingMax is what a peer that does not keep up may have waiting; more is dropped. The
	// shares are not lost to the network, other peers carry them.
	sharePendingMax = 4 * shareBatchMax
)

// shareLink is the share state of one connection.
type shareLink struct {
	conn *p2p.Conn
	// dec is used by the connection's read loop alone.
	dec *p2p.ShareDecoder

	// send keeps the batches in the order they were encoded in: the dictionaries depend on it.
	send sync.Mutex
	mu   sync.Mutex
	enc  *p2p.ShareEncoder
	// pending are the shares waiting for the next batch; armed: a flush is scheduled.
	pending []*chain.Share
	armed   bool
}

func newShareLink(c *p2p.Conn) *shareLink {
	return &shareLink{conn: c, dec: p2p.NewShareDecoder(), enc: p2p.NewShareEncoder()}
}

func (l *shareLink) queue(s *chain.Share) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) >= sharePendingMax {
		return
	}
	l.pending = append(l.pending, s)
	if !l.armed {
		l.armed = true
		time.AfterFunc(shareFlushDelay, l.flush)
	}
}

func (l *shareLink) flush() {
	l.send.Lock()
	defer l.send.Unlock()
	l.mu.Lock()
	batch := l.pending
	if len(batch) > shareBatchMax {
		batch = batch[:shareBatchMax]
	}
	l.pending = l.pending[len(batch):]
	payload := l.enc.Encode(batch)
	if len(l.pending) > 0 {
		time.AfterFunc(shareFlushDelay, l.flush)
	} else {
		l.armed = false
	}
	l.mu.Unlock()
	if len(payload) > 0 {
		_ = l.conn.Send(&p2p.Message{Command: p2p.CmdShares, Payload: payload})
	}
}

// announceShare passes a verified share on to the peers that verify shares, except the one it
// came from.
func (n *Node) announceShare(s *chain.Share, except string) {
	n.mu.Lock()
	targets := make([]*shareLink, 0, len(n.fullDag))
	for key := range n.fullDag {
		if l, ok := n.shareLinks[key]; ok && key != except {
			targets = append(targets, l)
		}
	}
	n.mu.Unlock()
	for _, l := range targets {
		l.queue(s)
	}
}

// receiveShares reads a peer's `shares` message and takes every share in it.
func (n *Node) receiveShares(peer *p2p.Peer, payload []byte) error {
	from := addrOf(peer)
	n.mu.Lock()
	l := n.shareLinks[from]
	n.mu.Unlock()
	if l == nil {
		return nil // the connection is going away
	}
	shares, err := l.dec.Decode(payload)
	if err != nil {
		n.peers.Misbehaving(peer, p2p.MisbehaviorBadMessage)
		return err
	}
	for _, s := range shares {
		if err := n.acceptShare(s, from); errors.Is(err, errBadShare) {
			n.peers.Misbehaving(peer, p2p.MisbehaviorBadMessage)
			return nil
		}
	}
	return nil
}
