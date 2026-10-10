package main

import (
	"errors"

	"powmachina/internal/p2p"
)

// errNodeClosing is returned by functions that have no business starting because the node is shutting down.
var errNodeClosing = errors.New("the node is shutting down")

// Tracking goroutines and connections so `Close` knows what to wait for.
//
// The reason is concrete: after `Close` the connection goroutines kept running and writing
// into closed storage and into a freed epoch cache. In CI that produced `pebble: closed` and
// `assignment to entry in nil map` — under load during shutdown that is a normal state, not a
// theoretical one. Waiting for the maintenance loops was enough, for the connection goroutines
// it was not: those are created for every incoming and outgoing connection.
//
// `Add` has to come **before** `go` and under the same lock `Close` closes `n.closing` under —
// otherwise a goroutine registers only after `Wait` has finished.

// goTracked starts `fn` as a goroutine that `Close` will wait for. It returns false when the
// node is already closing — then `fn` does not run at all and the caller has to clean up.
func (n *Node) goTracked(fn func()) bool {
	n.mu.Lock()
	select {
	case <-n.closing:
		n.mu.Unlock()
		return false
	default:
	}
	n.loops.Add(1)
	n.mu.Unlock()

	go func() {
		defer n.loops.Done()
		fn()
	}()
	return true
}

// addLive registers an open connection. It returns false when the node is closing — then the
// connection must not be served, because it would write into closed storage.
func (n *Node) addLive(c *p2p.Conn) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	select {
	case <-n.closing:
		return false
	default:
	}
	n.live[c] = struct{}{}
	return true
}

func (n *Node) removeLive(c *p2p.Conn) {
	n.mu.Lock()
	delete(n.live, c)
	n.mu.Unlock()
}
