package chain

import (
	"sync"
	"time"
)

// The orphan pool — transactions whose parent is neither in the mempool nor in a block yet.
//
// Without it a transaction that overtook its parent is silently dropped. And overtaking is not an
// exception: a child and its parent travel the network independently, through different peers and
// paths of different lengths, so with every chained payment it is a matter of chance. The wallet
// meanwhile says "sent".
//
// The pool is **policy, not consensus** and it is deliberately small: it holds transactions that
// (still) cannot be verified, which makes it the one thing in the node carrying unverified data
// from strangers. Hence a cap on both the count and the bytes, a short expiry and evicting a random
// element — an attacker has no way to arrange for their own to be the one that stays.
const (
	// MaxOrphanTxs is the cap on the number of orphan transactions (like bitcoind).
	MaxOrphanTxs = 100
	// MaxOrphanBytes is the cap on the orphan pool's memory.
	MaxOrphanBytes = 4 << 20
	// OrphanExpiry is how long a parent is waited for. A relay crosses the network in seconds;
	// 20 minutes is already the case where the parent does not exist or nobody ever sent it.
	OrphanExpiry = 20 * time.Minute
)

type orphanEntry struct {
	tx    *Tx
	id    [32]byte
	size  int
	added time.Time
}

// OrphanPool is safe for concurrent use.
type OrphanPool struct {
	mu      sync.Mutex
	txs     map[[32]byte]*orphanEntry
	waiting map[[32]byte]map[[32]byte]bool // the parent's txid → the children waiting for it
	bytes   int
}

func NewOrphanPool() *OrphanPool {
	return &OrphanPool{
		txs:     make(map[[32]byte]*orphanEntry),
		waiting: make(map[[32]byte]map[[32]byte]bool),
	}
}

// Add files a transaction and returns the txids of the parents it is waiting for. An empty list
// means it was not filed (we already have it, or it did not fit).
//
// `missing` are the outpoints that were not found — the caller knows them from the verification, so
// they are not computed a second time here.
func (p *OrphanPool) Add(tx *Tx, missing []Outpoint, now time.Time) [][32]byte {
	if len(missing) == 0 || tx.IsCoinbase() {
		return nil
	}
	id := tx.TxID()
	size := len(tx.Serialize())
	if size > MaxOrphanBytes {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, dup := p.txs[id]; dup {
		return nil
	}
	p.expireLocked(now.Add(-OrphanExpiry))
	for len(p.txs) >= MaxOrphanTxs || p.bytes+size > MaxOrphanBytes {
		if !p.evictOneLocked(id) {
			return nil
		}
	}

	parents := make([][32]byte, 0, len(missing))
	seen := make(map[[32]byte]bool, len(missing))
	for _, op := range missing {
		if seen[op.TxID] {
			continue
		}
		seen[op.TxID] = true
		parents = append(parents, op.TxID)
		if p.waiting[op.TxID] == nil {
			p.waiting[op.TxID] = make(map[[32]byte]bool)
		}
		p.waiting[op.TxID][id] = true
	}
	p.txs[id] = &orphanEntry{tx: tx, id: id, size: size, added: now}
	p.bytes += size
	return parents
}

// TakeChildren picks out and removes the transactions that were waiting for this parent. It is
// called after every accepted transaction and after every block — that is when the parent appeared.
//
// They are removed even when the child is then not accepted: it no longer has to wait for the
// parent, and if it is invalid it has no business in the pool. A second attempt would cost the same
// and end the same way.
func (p *OrphanPool) TakeChildren(parent [32]byte) []*Tx {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := p.waiting[parent]
	if len(ids) == 0 {
		return nil
	}
	out := make([]*Tx, 0, len(ids))
	for id := range ids {
		if e, ok := p.txs[id]; ok {
			out = append(out, e.tx)
			p.removeLocked(id)
		}
	}
	return out
}

// ExpireBefore throws out the transactions filed before `cutoff`. It returns the number removed.
func (p *OrphanPool) ExpireBefore(cutoff time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.expireLocked(cutoff)
}

func (p *OrphanPool) expireLocked(cutoff time.Time) int {
	var old [][32]byte
	for id, e := range p.txs {
		if e.added.Before(cutoff) {
			old = append(old, id)
		}
	}
	for _, id := range old {
		p.removeLocked(id)
	}
	return len(old)
}

// evictOneLocked throws out one transaction to free space. It picks a **random** one (Go's map
// iteration order is random) rather than the oldest: with the oldest evicted, an attacker would
// only have to flood the pool with their own and foreign ones would never survive in it.
func (p *OrphanPool) evictOneLocked(except [32]byte) bool {
	for id := range p.txs {
		if id == except {
			continue
		}
		p.removeLocked(id)
		return true
	}
	return false
}

func (p *OrphanPool) removeLocked(id [32]byte) {
	e, ok := p.txs[id]
	if !ok {
		return
	}
	for _, in := range e.tx.Inputs {
		if set, ok := p.waiting[in.PrevTxID]; ok {
			delete(set, id)
			if len(set) == 0 {
				delete(p.waiting, in.PrevTxID)
			}
		}
	}
	p.bytes -= e.size
	delete(p.txs, id)
}

func (p *OrphanPool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.txs)
}

func (p *OrphanPool) Bytes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bytes
}

// Has says whether we already have a transaction in the pool — so it is not requested a second time from another peer.
func (p *OrphanPool) Has(id [32]byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.txs[id]
	return ok
}
