package main

import (
	"sync"
	"time"

	"powmachina/internal/chain"
)

// A share can arrive before the block it builds on: a miner gets a new job the moment its node has
// the block, and the share it finds can overtake the block on the way to a third node. That node
// does not know the parent yet and used to drop the share — and nobody sends it again, so the
// miner lost it. Such shares wait here, briefly, for their block.
//
// The parent is unknown, so nothing about the share can be checked yet (not even its PoW, which
// needs the parent's difficulty): the room is small, a peer gets a part of it, and a share waits
// at most a minute. A block that does not come in a minute is not coming for this share anyway —
// it would expire eight blocks later.
const (
	maxOrphanShares     = 4096
	maxOrphanSharesPeer = 512
	orphanShareTTL      = time.Minute
)

type orphanShare struct {
	s            *chain.Share
	from, origin string
	at           time.Time
}

type shareOrphans struct {
	mu       sync.Mutex
	byParent map[[32]byte][]orphanShare
	perPeer  map[string]int
	n        int
}

// add files a share whose parent is unknown; false = no room.
func (o *shareOrphans) add(s *chain.Share, from, origin string, now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.byParent == nil {
		o.byParent, o.perPeer = map[[32]byte][]orphanShare{}, map[string]int{}
	}
	if o.n >= maxOrphanShares || o.perPeer[from] >= maxOrphanSharesPeer {
		o.expireLocked(now)
		if o.n >= maxOrphanShares || o.perPeer[from] >= maxOrphanSharesPeer {
			return false
		}
	}
	p := s.Header.PrevBlock
	o.byParent[p] = append(o.byParent[p], orphanShare{s: s, from: from, origin: origin, at: now})
	o.perPeer[from]++
	o.n++
	return true
}

// take hands back the shares waiting for a block that has now arrived.
func (o *shareOrphans) take(parent [32]byte, now time.Time) []orphanShare {
	o.mu.Lock()
	defer o.mu.Unlock()
	list := o.byParent[parent]
	if len(list) == 0 {
		return nil
	}
	delete(o.byParent, parent)
	out := list[:0]
	for _, e := range list {
		o.n--
		o.perPeer[e.from]--
		if o.perPeer[e.from] <= 0 {
			delete(o.perPeer, e.from)
		}
		if now.Sub(e.at) <= orphanShareTTL {
			out = append(out, e)
		}
	}
	return out
}

func (o *shareOrphans) expireLocked(now time.Time) {
	for p, list := range o.byParent {
		keep := list[:0]
		for _, e := range list {
			if now.Sub(e.at) <= orphanShareTTL {
				keep = append(keep, e)
				continue
			}
			o.n--
			o.perPeer[e.from]--
			if o.perPeer[e.from] <= 0 {
				delete(o.perPeer, e.from)
			}
		}
		if len(keep) == 0 {
			delete(o.byParent, p)
		} else {
			o.byParent[p] = keep
		}
	}
}

func (o *shareOrphans) len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.n
}

// retryOrphanShares takes the shares that waited for a block. Not under applyMu: taking a share can
// switch to a sibling, which applies blocks.
func (n *Node) retryOrphanShares(parent [32]byte) {
	for _, e := range n.shareOrphans.take(parent, time.Now()) {
		_ = n.acceptShare(e.s, e.from, e.origin)
	}
}
