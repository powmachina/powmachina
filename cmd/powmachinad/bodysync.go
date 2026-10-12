package main

import (
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"powmachina/internal/blocksync"
	"powmachina/internal/chain"
	"powmachina/internal/p2p"
	"powmachina/internal/store"
)

// Pulling block bodies in the background.
//
// # Why this has a driver of its own
//
// The header chain is a **verified manifest**: once we have it, we know the hash of every
// block we are missing (the sync design). Bodies may therefore be pulled in any order and from
// anybody — nothing can be slipped in, because every body is verified against the manifest.
// Applying them, by contrast, is **serial and validating**, and is done from disk
// (`store.ApplyStored`).
//
// The obvious design — request bodies once, right after a headers message arrives, at most
// `MaxGetDataItems` (128) per message — does not work. A single message carries up to
// `MaxHeadersPerMessage` (2 000) headers, so nobody would ever ask for the rest and the node
// would stay forever with its headers ahead and its bodies behind. Three rules this file
// stands on follow from it:
//
//  1. What gets downloaded is decided by **state** (what is missing between the UTXO tip and
//     the header tip), not by what just arrived in a message.
//  2. A request nobody answered is repeated after `bodyRequestTimeout` — one peer's silence
//     must not stop the sync.
//  3. A body out of order is **stored**, not applied. It is applied when its turn comes, and
//     then it is fully validated.

const (
	// bodySyncInterval is how often the driver looks at what is missing. With a block every
	// 120 s this is often enough for a new body to arrive at once, and rare enough to cost nothing.
	bodySyncInterval = 2 * time.Second
	// poolExpireInterval is how often old entries are thrown out of the mempool and the orphan
	// pool. Expiry is in hours (mempool) and tens of minutes (orphan), so anything more
	// frequent would just be work for nothing.
	poolExpireInterval = 5 * time.Minute
	// bodyRequestTimeout is how long until an undelivered body is requested again (from another peer too).
	bodyRequestTimeout = 20 * time.Second
	// bodyWindowBatches is how many batches may be in flight. It keeps memory and overhead in
	// check and is at the same time enough that the link does not idle between batches.
	bodyWindowBatches = 4
	// attestPollInterval is how often the node asks for missing finality attestations.
	// Finality state lives in memory only and duplicates are deliberately not relayed, so after
	// a restart it has no way to learn what the quorum already signed — it has to ask
	// (the finality spec).
	attestPollInterval = 20 * time.Second
	// headerPollInterval is how often the node asks its peers for headers when it has nothing
	// to pull. It is an insurance against a stall: the announcement of a new block can get lost
	// (the peer left, the message did not make it), and without this question the node would
	// stand still until somebody new connected. It costs one message per peer per half minute.
	headerPollInterval = 30 * time.Second
)

// bodySync tracks which bodies are in flight and since when.
type bodySync struct {
	// timeout is bodyRequestTimeout; the tests shorten it so they do not have to wait 20 s.
	timeout time.Duration
	// batch is how many bodies are requested from one peer at a time (MaxGetDataItems). The
	// tests shrink it so a multi-round sync can be exercised on a short chain.
	batch int

	mu       sync.Mutex
	inflight map[[32]byte]bodyRequest
	// The counters below are the only way to see this path from outside. A body that fails
	// verification is dropped without a word (storeBody), and a request that is never answered
	// looks exactly like one that was never sent — one missing body can stall the UTXO set of
	// every node, and without these there is nothing to say who had been asked for what.
	sent      uint64
	delivered uint64
	timeouts  uint64
	mismatch  uint64

	// src is who delivered a body that is stored and not applied yet. Applying decides what
	// becomes of it: a body that validates is handed on to everybody else, one that fails on its
	// witnesses costs the peer that sent it.
	src map[[32]byte]*p2p.Peer
}

// maxBodySources bounds src. Bodies of side branches are stored and never applied, so their
// entries would stay; at the cap the map starts over, and the worst that follows is a block
// announced back to the peer it came from.
const maxBodySources = 4096

type bodyRequest struct {
	sent time.Time
	from string // who was asked — so a retry is tried elsewhere
}

func newBodySync() *bodySync {
	return &bodySync{timeout: bodyRequestTimeout, batch: p2p.MaxGetDataItems,
		inflight: make(map[[32]byte]bodyRequest), src: make(map[[32]byte]*p2p.Peer)}
}

// deliveredBy remembers who a stored body came from.
func (b *bodySync) deliveredBy(id [32]byte, peer *p2p.Peer) {
	if peer == nil {
		return
	}
	b.mu.Lock()
	if len(b.src) >= maxBodySources {
		clear(b.src)
	}
	b.src[id] = peer
	b.mu.Unlock()
}

// source returns who delivered a body and forgets it. Nil for a body that was on disk at startup
// or came from a block mirror.
func (b *bodySync) source(id [32]byte) *p2p.Peer {
	b.mu.Lock()
	defer b.mu.Unlock()
	peer := b.src[id]
	delete(b.src, id)
	return peer
}

// take picks from the candidates those that may be requested from peer `from` now and records
// them as in flight. It skips those still being waited for; after the timeout it releases them
// again, but preferably to somebody else.
func (b *bodySync) take(cands [][32]byte, from string, now time.Time, max int) [][32]byte {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([][32]byte, 0, max)
	for _, id := range cands {
		if len(out) >= max {
			break
		}
		req, waiting := b.inflight[id]
		if waiting {
			if now.Sub(req.sent) < b.timeout {
				continue // still waiting
			}
			if req.from == from {
				// This peer already failed to answer once — let somebody else try. When there
				// is nobody else, retryAll releases the wait and it is asked of them again.
				continue
			}
		}
		b.inflight[id] = bodyRequest{sent: now, from: from}
		if waiting {
			b.timeouts++
		}
		b.sent++
		out = append(out, id)
	}
	return out
}

// oldest is how long the body that has been waited for longest has been in flight, and how
// many are waiting. It is what the operator sees in /metrics; a growing number there is the
// symptom that froze the wallets and the explorer.
func (b *bodySync) oldest(now time.Time) (time.Duration, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var age time.Duration
	for _, req := range b.inflight {
		if d := now.Sub(req.sent); d > age {
			age = d
		}
	}
	return age, len(b.inflight)
}

// counters returns the totals for /metrics: requests sent, bodies delivered, requests that ran
// into their timeout, and bodies that did not match the manifest.
func (b *bodySync) counters() (sent, delivered, timeouts, mismatch uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sent, b.delivered, b.timeouts, b.mismatch
}

// broadcast records that the given body has just been asked of every peer at once. It is kept
// out of `take` deliberately: `take` splits work between peers, this one duplicates it, and
// only for the single body that blocks everything else.
func (b *bodySync) broadcast(id [32]byte, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if req, waiting := b.inflight[id]; waiting && now.Sub(req.sent) >= b.timeout {
		b.timeouts++
	}
	b.inflight[id] = bodyRequest{sent: now, from: ""}
	b.sent++
}

// retryAll releases every wait that has passed its timeout — so bodies can be requested again
// even from the same peer when there is no other.
func (b *bodySync) retryAll(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, req := range b.inflight {
		if now.Sub(req.sent) >= b.timeout {
			delete(b.inflight, id)
			b.timeouts++
		}
	}
}

// window is the cap on bodies in flight — a multiple of the batch, so the link does not idle between batches.
func (b *bodySync) window() int { return bodyWindowBatches * b.batch }

// prune drops waits for bodies there is no longer any point to — the body arrived another way
// or its height left in a reorg. Without it the table would grow to the height of the chain.
func (b *bodySync) prune(keep [][32]byte) {
	set := make(map[[32]byte]bool, len(keep))
	for _, id := range keep {
		set[id] = true
	}
	// The lock goes **before** any read of the map. A `len(b.inflight) == 0` check ahead of the
	// lock would be a race: the driver runs both from its own loop and from the message handler
	// (woken after headers arrive), so two goroutines would read and write the map at once —
	// and that shows up as a crash on "assignment to entry in nil map" somewhere else entirely.
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.inflight) == 0 {
		return
	}
	for id := range b.inflight {
		if !set[id] {
			delete(b.inflight, id)
		}
	}
}

// forget drops the wait for a body we no longer want (an invalid branch).
func (b *bodySync) forget(id [32]byte) { b.done(id) }

func (b *bodySync) done(id [32]byte) {
	b.mu.Lock()
	if _, waiting := b.inflight[id]; waiting {
		b.delivered++
	}
	delete(b.inflight, id)
	b.mu.Unlock()
}

// badBody counts a body that did not match the manifest — a lying or broken source.
func (b *bodySync) badBody() {
	b.mu.Lock()
	b.mismatch++
	b.mu.Unlock()
}

func (b *bodySync) waiting() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.inflight)
}

// SyncBodiesOnce is one round of the driver: bring the UTXO set onto the active branch, apply
// what can be applied, and ask for what is missing.
func (n *Node) SyncBodiesOnce(now time.Time) {
	if err := n.ApplyReadyBodies(); err != nil {
		log.Printf("⚠ applying bodies failed: %v", err)
	}
	n.pollAttestations(now)

	_, headerTip, _ := n.chain.Tip()
	applied, err := n.appliedHeight()
	if err != nil {
		log.Printf("⚠ the storage tip cannot be read: %v", err)
		return
	}
	if applied >= headerTip {
		n.pollHeaders(now) // nothing to pull — ask whether there is a new tip
		return
	}

	cands := n.missingBodyIDs(applied+1, headerTip, n.bodies.window())
	n.bodies.prune(cands)
	if len(cands) == 0 {
		return
	}

	conns := n.connSnapshot()
	if len(conns) == 0 {
		return
	}
	if len(conns) == 1 {
		// A single peer: after the timeout it has to be asked again, otherwise the sync would stall.
		n.bodies.retryAll(now)
	}
	// The oldest missing body is asked of **every** peer at once, the rest are split between
	// them. One body is at most 256 kB and it verifies itself against the manifest, so a
	// duplicate costs only bandwidth — whereas waiting for a single peer costs a whole timeout,
	// and the block cannot be applied without it, nor can any block above it. That is how one
	// body can hold the UTXO set of every node minutes behind the headers while the blocks
	// above it have long arrived.
	head := cands[0]
	n.bodies.broadcast(head, now)
	if payload, err := p2p.EncodeGetData(p2p.BlockItems([][32]byte{head})); err == nil {
		for _, c := range conns {
			_ = c.Send(&p2p.Message{Command: p2p.CmdGetData, Payload: payload})
		}
	}
	for addr, c := range conns {
		want := n.bodies.take(cands, addr, now, n.bodies.batch)
		if len(want) == 0 {
			continue
		}
		payload, err := p2p.EncodeGetData(p2p.BlockItems(want))
		if err != nil {
			continue
		}
		if err := c.Send(&p2p.Message{Command: p2p.CmdGetData, Payload: payload}); err != nil {
			continue
		}
	}
	n.warnStuckBody(now, applied+1, len(conns))
}

// bodyStallWarn is how long a body may be missing before the node says so. It is above the
// request timeout on purpose: an ordinary retry is not worth a line in the log, a body that
// nobody delivers is.
const bodyStallWarn = 45 * time.Second

// warnStuckBody says out loud that the chain cannot move because one body is missing. Without
// it this state is invisible: the headers keep growing, the node answers everything, and only
// the wallets and the explorer stand still, because the UTXO set stops at the hole.
func (n *Node) warnStuckBody(now time.Time, height uint64, peers int) {
	age, waiting := n.bodies.oldest(now)
	if age < bodyStallWarn {
		return
	}
	n.mu.Lock()
	quiet := now.Sub(n.lastBodyWarn) < bodyStallWarn
	if !quiet {
		n.lastBodyWarn = now
	}
	n.mu.Unlock()
	if quiet {
		return
	}
	_, sent, timeouts, mismatch := n.bodies.counters()
	log.Printf("⚠ the body for height %d has been missing %s (%d in flight, %d peers, "+
		"delivered %d, timeouts %d, mismatched %d) — the UTXO set cannot move past it",
		height, age.Round(time.Second), waiting, peers, sent, timeouts, mismatch)
}

// pollAttestations requests attestations for heights that are already deep enough for
// finality but are not attested.
//
// Without it finality lags after every restart until the quorum signs the next height — with
// a depth of 20 blocks that is up to 40 minutes. It asks only for what is really missing, and
// for at most MaxAttestHeights heights at a time.
func (n *Node) pollAttestations(now time.Time) {
	if n.finality == nil {
		return
	}
	_, tip, _ := n.chain.Tip()
	if tip < chain.FinalityDepth {
		return
	}
	deepest := tip - chain.FinalityDepth
	final, _, _ := n.finality.Finalized()
	if final >= deepest {
		return // finality has caught up, nothing to ask for
	}

	n.mu.Lock()
	if now.Sub(n.lastAttestPoll) < attestPollInterval {
		n.mu.Unlock()
		return
	}
	n.lastAttestPoll = now
	n.mu.Unlock()

	from := final + 1
	count := deepest - from + 1
	if count > p2p.MaxAttestHeights {
		// We ask for the oldest missing heights — finality moves upwards from below.
		count = p2p.MaxAttestHeights
	}
	payload, err := p2p.EncodeGetAttest(from, uint16(count))
	if err != nil {
		return
	}
	for _, c := range n.connSnapshot() {
		_ = c.Send(&p2p.Message{Command: p2p.CmdGetAttest, Payload: payload})
	}
}

// pollHeaders asks every peer for headers once per headerPollInterval.
func (n *Node) pollHeaders(now time.Time) {
	n.mu.Lock()
	if now.Sub(n.lastHeaderPoll) < headerPollInterval {
		n.mu.Unlock()
		return
	}
	n.lastHeaderPoll = now
	n.mu.Unlock()

	for _, c := range n.connSnapshot() {
		_ = n.RequestHeaders(c)
	}
}

// ApplyReadyBodies applies the stored bodies in order — and first brings the UTXO set onto the
// active branch.
//
// Validation happens on apply, not on store: a body above the tip cannot be checked against a
// UTXO set that does not exist yet.
func (n *Node) ApplyReadyBodies() error {
	n.applyMu.Lock()
	defer n.applyMu.Unlock()

	// At most two rounds: when a body fails validation, the branch is marked invalid, the chain
	// switches to the heaviest valid one and applying continues there. More rounds are not
	// needed — each would mean another invalid branch, and those cannot be mined that fast.
	for round := 0; round < 8; round++ {
		// The height index has to describe the active branch; it is cheap (zero steps when they
		// agree) and after a crash in the middle of an invalidation it is the only thing that
		// brings the chain back into agreement.
		if err := n.indexActiveBranch(); err != nil {
			return err
		}
		if err := n.reconcileUTXOTip(); err != nil {
			return err
		}
		_, headerTip, _ := n.chain.Tip()
		var failed *chain.Block
		applied, err := n.store.ApplyStored(headerTip, store.ApplyHooks{
			Validate: func(b *chain.Block) error {
				if verr := b.ValidateBlock(n.store, n.params); verr != nil {
					failed = b
					return verr
				}
				return nil
			},
			Applied: func(b *chain.Block) {
				n.onBlockApplied(b)
				n.relayApplied(b)
			},
		})
		if applied > 0 {
			tip, _ := n.store.Tip()
			log.Printf("applied %d bodies, the UTXO set is at height %d (%d outputs)", applied, tip.Height, n.coinCount())
		}
		if err == nil {
			return nil
		}
		if failed == nil {
			return err // something other than validation failed (storage)
		}
		// The id of a block does not cover the witnesses. When everything the header commits to
		// is valid, the failure says something about this copy of the body and nothing about
		// the block: marking the block invalid would let anybody who relays it cut us off from
		// the chain by flipping a byte of a signature.
		if failed.ValidateCommitted(n.store, n.params) == nil {
			return n.dropBadCopyLocked(failed, err)
		}
		if ierr := n.invalidateBranchLocked(failed, err); ierr != nil {
			return ierr
		}
	}
	return nil
}

// invalidateBranchLocked marks a block invalid, switches the chain to the heaviest valid
// branch and throws the body away. Called with applyMu held.
//
// It does **not ban** the peer that delivered the body: the body matched the merkle root of a
// verified header, so it could not have known about the invalidity — that only shows up on
// validation against a UTXO set, which differs from node to node. The cost to whoever produced
// such a branch is a block of work; the cost to us is one downloaded body, and only once: the
// mark survives a restart.
func (n *Node) invalidateBranchLocked(blk *chain.Block, why error) error {
	id := blk.Header.ID()
	log.Printf("⚠ block %d (%x) failed validation: %v — the branch is invalid",
		blk.Header.Height, id[:6], why)

	newTip, changed, err := n.chain.Invalidate(id)
	if err != nil {
		return err
	}
	if err := n.store.MarkInvalid(id); err != nil {
		return err
	}
	if changed {
		_, h, _ := n.chain.Tip()
		log.Printf("the chain switched to the heaviest valid branch: height %d (%x)", h, newTip[:6])
	}
	n.bodies.forget(id)
	n.bodies.source(id)
	return nil
}

// dropBadCopyLocked throws away a body that failed validation on its witnesses and leaves the
// block alone, so the driver asks for the body again. Called with applyMu held.
//
// The peer that delivered it pays: a node relays a body only once it has validated it, so an
// honest peer does not send such a copy. The price is below a ban — the sender may be passing on
// what it was handed, or serving a body of a branch it never applied.
func (n *Node) dropBadCopyLocked(blk *chain.Block, why error) error {
	id := blk.Header.ID()
	from := "disk or a mirror"
	if peer := n.bodies.source(id); peer != nil {
		from = addrOf(peer)
		n.peers.Misbehaving(peer, p2p.MisbehaviorBadCopy)
	}
	n.bodies.badBody()
	log.Printf("⚠ the body of block %d (%x) from %s failed validation: %v — the copy is dropped, the block stays",
		blk.Header.Height, id[:6], from, why)
	return n.store.DropBody(id)
}

// relayApplied hands a block that has just been applied on to the other peers, whole, so the
// next hop does not have to ask for it.
//
// Only now, not when the body arrives: a body is checked against the header on arrival, and the
// header does not cover the witnesses. Relaying before validation would carry a tampered copy
// through the whole network ahead of the good one. And only near the tip: during a sync this
// would push the whole history to every peer, and there the pull driver is the right tool.
func (n *Node) relayApplied(blk *chain.Block) {
	peer := n.bodies.source(blk.Header.ID())
	if peer == nil {
		return // on disk since before the start, or mined here (SubmitBlock announces its own)
	}
	if _, headerTip, _ := n.chain.Tip(); uint64(blk.Header.Height)+2 < headerTip {
		return
	}
	except := addrOf(peer)
	n.goTracked(func() { n.announceBlock(blk, except) })
}

// reconcileUTXOTip brings the UTXO set back onto the branch that is active according to the
// headers.
//
// A header reorg switches the active branch (`HeaderChain.maybeReorgLocked`), but the UTXO set
// does not know about it on its own — without this, `ApplyStored` would hit "the block does not
// build on the tip" and the node would stall below the fork forever.
func (n *Node) reconcileUTXOTip() error {
	for {
		tip, err := n.store.Tip()
		if err != nil {
			return err
		}
		active, ok := n.chain.BlockIDAt(tip.Height)
		if ok && active == tip.ID {
			return nil
		}
		if tip.Height == 0 {
			return fmt.Errorf("the UTXO tip is at height 0 and does not match the genesis — a corrupted datadir")
		}
		if err := n.store.RevertTip(); err != nil {
			return fmt.Errorf("UTXO reorg: %w", err)
		}
		log.Printf("reorg: the UTXO set rolled back below height %d (block %x is no longer on the active branch)",
			tip.Height, tip.ID[:6])
	}
}

// appliedHeight is the height the UTXO set is finished up to.
func (n *Node) appliedHeight() (uint64, error) {
	tip, err := n.store.Tip()
	if err != nil {
		return 0, err
	}
	return tip.Height, nil
}

// indexActiveBranch records in storage which block sits at which height of the active branch.
//
// It goes top down until the index agrees with the chain — on a plain extension that is one
// entry, on a reorg as many as its depth (consensus bounds that from above with `ReorgLimit`).
// The write is a single atomic batch, so the index never describes two branches at once.
func (n *Node) indexActiveBranch() error {
	_, tipH, _ := n.chain.Tip()
	var entries []store.BranchEntry
	for h := tipH; h > 0; h-- {
		id, ok := n.chain.BlockIDAt(h)
		if !ok {
			break
		}
		if cur, err := n.store.BlockIDAt(h); err == nil && cur == id {
			break // from here down the index agrees
		}
		entries = append(entries, store.BranchEntry{Height: h, ID: id})
	}
	slices.Reverse(entries)
	return n.store.SetActiveBranch(entries)
}

// missingBodyIDs returns the ids of the active branch's blocks from `from` to `to` whose body we do not have.
func (n *Node) missingBodyIDs(from, to uint64, max int) [][32]byte {
	out := make([][32]byte, 0, min(max, int(to-from)+1))
	for h := from; h <= to && len(out) < max; h++ {
		id, ok := n.chain.BlockIDAt(h)
		if !ok {
			break // we have no headers beyond this, the manifest ends here
		}
		if n.store.HasBody(id) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// connSnapshot is a copy of the connection table — sending should happen without holding the lock.
func (n *Node) connSnapshot() map[string]*p2p.Conn {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make(map[string]*p2p.Conn, len(n.conns))
	for k, c := range n.conns {
		out[k] = c
	}
	return out
}

// storeBody verifies a body against the manifest and stores it. It does not apply it — that is
// done by ApplyReadyBodies in height order.
//
// The verification is exactly what blocksync does when downloading in parallel: the id has to
// match the header and the merkle root the transactions. Without the second check a source
// could send a correct header with completely different transactions.
func (n *Node) storeBody(peer *p2p.Peer, blk *chain.Block) error {
	id := blk.Header.ID()
	if !n.chain.Has(id) {
		// A body without a verified header cannot be verified by us, so it is dropped. That is
		// not a reason for misbehaviour points or a disconnect, though: between requesting the
		// body and its arrival the header can be lost in a reorg, and a peer that merely
		// answered our getdata is not at fault. The volume is kept in check by p2p.PeerMeter.
		return nil
	}
	n.bodies.done(id)
	if n.store.HasBody(id) {
		return nil
	}
	if err := blocksync.VerifyBody(blk, id); err != nil {
		// Loud on purpose: a body that does not match the manifest is either a lying peer or a
		// bug of ours, and neither should pass in silence.
		n.bodies.badBody()
		log.Printf("⚠ the body for height %d from %s does not match the manifest: %v",
			blk.Header.Height, addrOf(peer), err)
		n.peers.Misbehaving(peer, p2p.MisbehaviorInvalidBlock)
		return err
	}
	if err := n.store.PutBlock(blk); err != nil {
		return err
	}
	// Handing it onward waits for validation (relayApplied).
	n.bodies.deliveredBy(id, peer)
	return nil
}

// onBlockApplied is what belongs to a new tip and not in storage.
func (n *Node) onBlockApplied(b *chain.Block) {
	n.mempool.RemoveConfirmed(b)
	// Confirmed transactions are parents for those that overtook them — their outputs are now
	// in the UTXO set, so a child from the orphan pool will pass.
	parents := make([][32]byte, 0, len(b.Txs))
	for _, tx := range b.Txs {
		parents = append(parents, tx.TxID())
	}
	n.resolveOrphans(parents, "")
	n.warmNextEpoch(uint64(b.Header.Height))
	n.attestTip()
}
