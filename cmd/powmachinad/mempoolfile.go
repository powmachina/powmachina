package main

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/p2p"
)

// mempoolFile is the name of the mempool dump in the datadir.
const mempoolFile = "mempool.dat"

// saveMempool dumps the mempool into the datadir. A node without a datadir has nowhere to put
// it — and no reason to either, because it downloads the whole chain after a restart.
//
// The write goes through a temporary file and `rename`, so a crash halfway does not leave half
// a dump. An error is only logged: the mempool is not data a shutdown should stop for.
func (n *Node) saveMempool() {
	if n.datadir == "" || n.mempool.Len() == 0 {
		return
	}
	path := filepath.Join(n.datadir, mempoolFile)
	tmp, err := os.CreateTemp(n.datadir, mempoolFile+".*")
	if err != nil {
		log.Printf("⚠ the mempool was not dumped: %v", err)
		return
	}
	defer os.Remove(tmp.Name())

	count, err := n.mempool.Dump(tmp)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		log.Printf("⚠ the mempool was not dumped: %v", err)
		return
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		log.Printf("⚠ the mempool was not dumped: %v", err)
		return
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		log.Printf("⚠ the mempool was not dumped: %v", err)
		return
	}
	// Without an fsync of the directory the rename can disappear after a machine crash, even though the data is on disk.
	if dir, err := os.Open(n.datadir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	log.Printf("mempool dumped: %d transactions into %s", count, path)
}

// loadMempool reads the dump and deletes it. It is deleted even on an error: a corrupted or
// old file would be loaded at every startup and report the same thing every time.
//
// The transactions go through `Accept` like anything from the network — the file gets no privilege.
func (n *Node) loadMempool(height uint64, now time.Time) {
	if n.datadir == "" {
		return
	}
	path := filepath.Join(n.datadir, mempoolFile)
	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("⚠ the mempool dump cannot be opened: %v", err)
		}
		return
	}
	loaded, dropped, err := n.mempool.Load(f, n.store, height+1, now)
	_ = f.Close()
	_ = os.Remove(path)

	switch {
	case err != nil && loaded == 0:
		log.Printf("⚠ the mempool dump could not be read: %v", err)
	case err != nil:
		log.Printf("⚠ the mempool dump is incomplete (%d accepted): %v", loaded, err)
	case loaded > 0 || dropped > 0:
		log.Printf("mempool restored: %d transactions, %d dropped", loaded, dropped)
	}
}

// expirePools throws out of the mempool and the orphan pool whatever has been sitting there
// too long. It is called from the body driver (bodysync.go) — a loop of its own is not needed.
func (n *Node) expirePools(now time.Time) {
	if dropped := n.mempool.ExpireBefore(now.Add(-chain.MempoolExpiry)); dropped > 0 {
		log.Printf("mempool: %d transactions expired (they waited more than %s)", dropped,
			chain.MempoolExpiry)
	}
	n.orphans.ExpireBefore(now.Add(-chain.OrphanExpiry))
}

// resolveOrphans tries to file the transactions that were waiting for this parent. It handles
// chains too: an accepted child is a parent for the next one, so it keeps going while anything
// moves.
//
// It returns the number of accepted transactions.
func (n *Node) resolveOrphans(parents [][32]byte, from string) int {
	_, height, _ := n.chain.Tip()
	var accepted int
	queue := parents
	for len(queue) > 0 && accepted < chain.MaxOrphanTxs {
		parent := queue[0]
		queue = queue[1:]
		for _, tx := range n.orphans.TakeChildren(parent) {
			id := tx.TxID()
			if _, err := n.mempool.Accept(tx, n.store, height+1); err != nil {
				// The child is dropped here as well: it no longer has to wait for the parent
				// and a second attempt would end the same way. The one exception is another
				// missing parent.
				var miss *chain.MissingInputs
				if errors.As(err, &miss) {
					n.orphans.Add(tx, miss.Outpoints, time.Now())
				}
				continue
			}
			accepted++
			log.Printf("transaction %x accepted from the orphan pool (%d in the pool, %d waiting)",
				id[:6], n.mempool.Len(), n.orphans.Len())
			n.announce([]p2p.InvItem{{Type: p2p.InvTx, ID: id}}, from)
			queue = append(queue, id)
		}
	}
	return accepted
}

// requestParents asks for the missing parents from whoever sent the child. Without it an
// orphan would only get its parent if somebody announced it again — that is, by accident.
func (n *Node) requestParents(parents [][32]byte, from string) {
	if from == "" || len(parents) == 0 {
		return
	}
	n.mu.Lock()
	c := n.conns[from]
	n.mu.Unlock()
	if c == nil {
		return
	}
	// A cap on the count: a transaction with a hundred made-up inputs must not produce a
	// hundred requests. Only the first few make sense anyway — parents come together or not at all.
	items := make([]p2p.InvItem, 0, len(parents))
	for _, id := range parents {
		if _, have := n.mempool.Get(id); have {
			continue
		}
		items = append(items, p2p.InvItem{Type: p2p.InvTx, ID: id})
		if len(items) >= p2p.MaxGetDataItems {
			break
		}
	}
	if len(items) == 0 {
		return
	}
	payload, err := p2p.EncodeInv(items)
	if err != nil {
		return
	}
	_ = c.Send(&p2p.Message{Command: p2p.CmdGetData, Payload: payload})
}
