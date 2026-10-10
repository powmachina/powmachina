package main

import (
	"net"
	"strconv"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// waitForApplied waits until the node has the UTXO set applied up to the given height.
func waitForApplied(t *testing.T, n *Node, want uint64, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		tip, err := n.store.Tip()
		if err == nil && tip.Height >= want {
			return
		}
		if time.Now().After(deadline) {
			h := uint64(0)
			if err == nil {
				h = tip.Height
			}
			_, hdr, _ := n.chain.Tip()
			t.Fatalf("the UTXO set is at height %d of %d (headers %d)", h, want, hdr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A chain longer than one getdata batch has to sync all the way through.
//
// If bodies were requested only once after a headers message, and at most MaxGetDataItems
// (128) of them, the node would stay with its headers ahead and its bodies behind forever. A
// test that never goes beyond a single batch cannot see that.
//
// The batch is shrunk to 3 here, so 12 blocks are enough to exercise a multi-round sync. The
// test with the real batch of 128 is right below.
func TestSyncsChainLongerThanGetDataBatch(t *testing.T) {
	syncLongerThanBatch(t, 12, 3)
}

// The same with the **real** batch (128) and a chain longer than it. It always runs — now that
// regtest is genuinely trivial (RegtestTargetBits ≈ 16 attempts), 158 blocks are mined in seconds.
func TestSyncsChainOverRealGetDataWindow(t *testing.T) {
	syncLongerThanBatch(t, p2p.MaxGetDataItems+30, p2p.MaxGetDataItems)
}

func syncLongerThanBatch(t *testing.T, height int, batch int) {
	t.Helper()
	miner, minerAddr, cleanup := listeningNode(t)
	defer cleanup()
	if err := miner.MineBlocks(height); err != nil {
		t.Fatalf("mining failed: %v", err)
	}

	follower := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer follower.Close()
	follower.bodies.batch = batch
	follower.addrs.Add(minerAddr, net.ParseIP("88.198.113.1"))
	if err := follower.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatalf("the connection failed: %v", err)
	}

	// The driver runs on its own; FillOutboundOnce asked for the headers, the rest is its work.
	stop := make(chan struct{})
	defer close(stop)
	follower.startLoop(stop, 20*time.Millisecond, func() { follower.SyncBodiesOnce(time.Now()) })

	waitForApplied(t, follower, uint64(height), 120*time.Second)

	_, got, gotID := follower.chain.Tip()
	_, want, wantID := miner.chain.Tip()
	if got != want || gotID != wantID {
		t.Fatalf("tip %d (%x) != %d (%x)", got, gotID[:6], want, wantID[:6])
	}
	if a, b := follower.coinCount(), miner.coinCount(); a != b {
		t.Fatalf("the UTXO sets do not match: %d vs %d outputs", a, b)
	}
	// The connection has to survive the sync — nothing in it may be a reason to disconnect.
	if c := follower.peers.Count(p2p.PeerOutboundFull); c != 1 {
		t.Fatalf("the connection fell apart during the sync (%d)", c)
	}
}

// A body out of order is stored and applied only when its turn comes. That is the whole point
// of separating `PutBlock` from applying: bodies are pulled in parallel, applied serially.
func TestOutOfOrderBodyWaitsForItsTurn(t *testing.T) {
	src := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer src.Close()
	if err := src.MineBlocks(3); err != nil {
		t.Fatal(err)
	}

	dst := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer dst.Close()

	// The headers (the manifest) first — without them a body has nothing to be verified against.
	var headers []*chain.Header
	for h := uint64(1); h <= 3; h++ {
		id, _ := src.chain.BlockIDAt(h)
		blk, err := src.store.Block(id)
		if err != nil {
			t.Fatal(err)
		}
		headers = append(headers, blk.Header)
	}
	if added, err := dst.acceptHeaders(p2p.NewPeer(p2p.Address{}, p2p.PeerInbound), headers); err != nil || added != 3 {
		t.Fatalf("headers: %d added, error %v", added, err)
	}

	body := func(h uint64) *chain.Block {
		id, _ := src.chain.BlockIDAt(h)
		b, err := src.store.Block(id)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	peer := p2p.NewPeer(p2p.Address{}, p2p.PeerInbound)

	// Bodies 3 and 2 arrive first — they must not be applied, but they have to be stored.
	for _, h := range []uint64{3, 2} {
		if err := dst.acceptBlock(peer, body(h)); err != nil {
			t.Fatalf("body %d: %v", h, err)
		}
	}
	tip, err := dst.store.Tip()
	if err != nil {
		t.Fatal(err)
	}
	if tip.Height != 0 {
		t.Fatalf("bodies out of order were applied, the UTXO set is at height %d", tip.Height)
	}
	for _, h := range []uint64{2, 3} {
		id, _ := dst.chain.BlockIDAt(h)
		if !dst.store.HasBody(id) {
			t.Fatalf("body %d was not stored", h)
		}
	}

	// The missing body 1 unlocks applying all three at once.
	if err := dst.acceptBlock(peer, body(1)); err != nil {
		t.Fatalf("body 1: %v", err)
	}
	tip, err = dst.store.Tip()
	if err != nil {
		t.Fatal(err)
	}
	if tip.Height != 3 {
		t.Fatalf("after the missing body the UTXO set should be at 3, it is at %d", tip.Height)
	}
	if a, b := dst.coinCount(), src.coinCount(); a != b {
		t.Fatalf("the UTXO sets do not match: %d vs %d", a, b)
	}
}

// A peer that does not answer a getdata must not stop the sync: the request is repeated after
// the timeout. Without that one silent peer is enough for the node to hang forever.
func TestBodySyncRetriesAfterTimeout(t *testing.T) {
	src := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer src.Close()
	if err := src.MineBlocks(2); err != nil {
		t.Fatal(err)
	}

	// A peer that gives headers but never bodies — it only counts how many times it was asked.
	ln, err := p2p.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	tcp := ln.Addr().(*net.TCPAddr)
	peerAddr := p2p.Address{IP: tcp.IP, Port: uint16(tcp.Port), LastSeen: time.Now()}

	asked := make(chan int, 16)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		conn := p2p.NewConn(c, chain.RegTest.Magic)
		ver := &p2p.VersionMsg{Protocol: p2p.ProtocolVersion, Nonce: 0xFEED, UserAgent: "/silent/"}
		if _, err := conn.Handshake(ver, 0xFEED, false); err != nil {
			return
		}
		for {
			msg, err := conn.Recv()
			if err != nil {
				return
			}
			switch msg.Command {
			case p2p.CmdGetHeaders:
				hs := src.chain.HeadersFrom(nil, p2p.MaxHeadersPerMessage)
				payload, err := p2p.EncodeHeaders(hs)
				if err != nil {
					return
				}
				_ = conn.Send(&p2p.Message{Command: p2p.CmdHeaders, Payload: payload})
			case p2p.CmdGetData:
				items, err := p2p.DecodeGetData(msg.Payload)
				if err != nil {
					return
				}
				asked <- len(items) // and send nothing — that is exactly the point here
			}
		}
	}()

	follower := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer follower.Close()
	follower.bodies.timeout = 100 * time.Millisecond
	follower.addrs.Add(peerAddr, net.ParseIP("88.198.113.1"))
	if err := follower.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatalf("the connection failed: %v", err)
	}

	stop := make(chan struct{})
	defer close(stop)
	follower.startLoop(stop, 20*time.Millisecond, func() { follower.SyncBodiesOnce(time.Now()) })

	// Two independent requests for the same body = the retry works.
	deadline := time.After(10 * time.Second)
	for rounds := 0; rounds < 2; {
		select {
		case <-asked:
			rounds++
		case <-deadline:
			t.Fatalf("the driver did not repeat the request (rounds: %d)", rounds)
		}
	}
}

// A header reorg has to drag the UTXO set with it. Without that the UTXO set stays on a dead
// branch and applying ends on "the block does not build on the tip" — forever, because nothing
// brings it back from there.
//
// The branches have to diverge **above the genesis**: consensus deliberately does not treat a
// fork right at the genesis as a fork (forkPointBoundedLocked), so no reorg would happen at all.
func TestReorgRevertsUTXOToActiveBranch(t *testing.T) {
	base, baseAddr, cleanup := listeningNode(t)
	defer cleanup()
	if err := base.MineBlocks(2); err != nil { // the common base of both branches
		t.Fatal(err)
	}

	// The heavier branch grows from the same base on another node.
	long, longAddr, cleanupLong := listeningNode(t)
	defer cleanupLong()
	if err := long.SyncFrom(net.JoinHostPort(baseAddr.IP.String(),
		strconv.Itoa(int(baseAddr.Port)))); err != nil {
		t.Fatalf("taking over the base failed: %v", err)
	}

	follower := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer follower.Close()
	stop := make(chan struct{})
	defer close(stop)
	follower.startLoop(stop, 20*time.Millisecond, func() { follower.SyncBodiesOnce(time.Now()) })

	// The follower goes to the shorter branch first.
	follower.addrs.Add(baseAddr, net.ParseIP("88.198.113.1"))
	if err := follower.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatal(err)
	}
	if err := base.MineBlocks(2); err != nil {
		t.Fatal(err)
	}
	waitForApplied(t, follower, 4, 40*time.Second)
	if _, _, id := follower.chain.Tip(); id != mustTipID(t, base) {
		t.Fatal("the follower is not on the shorter branch")
	}

	// The heavier branch: 5 blocks above the common base.
	if err := long.MineBlocks(5); err != nil {
		t.Fatal(err)
	}
	// The second connection goes to it. The address first has to be moved among the verified
	// ones (that is what the feeler does), otherwise the outbound selection does not see it —
	// it takes from the verified table while there is anything in it.
	follower.addrs.Add(longAddr, net.ParseIP("45.83.104.1"))
	if err := follower.FeelerOnce(); err != nil {
		t.Fatalf("the feeler to the heavier branch failed: %v", err)
	}
	if err := follower.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatal(err)
	}

	_, want, wantID := long.chain.Tip()
	waitForApplied(t, follower, want, 60*time.Second)

	_, h, id := follower.chain.Tip()
	if id != wantID || h != want {
		t.Fatalf("the follower did not switch to the heavier branch: height %d (expected %d)", h, want)
	}
	tip, err := follower.store.Tip()
	if err != nil {
		t.Fatal(err)
	}
	if tip.ID != id {
		t.Fatalf("the UTXO tip %x does not match the header tip %x", tip.ID[:6], id[:6])
	}
	if a, b := follower.coinCount(), long.coinCount(); a != b {
		t.Fatalf("the UTXO set does not match the heavier branch: %d vs %d", a, b)
	}
}

func mustTipID(t *testing.T, n *Node) [32]byte {
	t.Helper()
	_, _, id := n.chain.Tip()
	return id
}

// feedBranch moves the headers and bodies of a given range from one node to another without the network.
func feedBranch(t *testing.T, dst, src *Node, from, to uint64) {
	t.Helper()
	peer := p2p.NewPeer(p2p.Address{}, p2p.PeerInbound)
	var headers []*chain.Header
	var blocks []*chain.Block
	for h := from; h <= to; h++ {
		id, ok := src.chain.BlockIDAt(h)
		if !ok {
			t.Fatalf("the source does not have height %d", h)
		}
		b, err := src.store.Block(id)
		if err != nil {
			t.Fatalf("the source does not have body %d: %v", h, err)
		}
		headers = append(headers, b.Header)
		blocks = append(blocks, b)
	}
	if _, err := dst.acceptHeaders(peer, headers); err != nil {
		t.Fatalf("headers %d–%d: %v", from, to, err)
	}
	for _, b := range blocks {
		if err := dst.acceptBlock(peer, b); err != nil {
			t.Fatalf("body %d: %v", b.Header.Height, err)
		}
	}
}

// After a reorg and a restart the node has to come up on the **heavier branch** — not on a
// chain glued together from two branches.
//
// This is why the height index is written only by `SetActiveBranch` and not by `PutHeader`: a
// header by itself does not say whether it is on the active branch, and `Restore` assembles the
// chain from that index after a restart. When every accepted header wrote the index, one header
// from the losing branch was enough.
func TestRestartAfterReorgLoadsHeavierBranch(t *testing.T) {
	base, _, cleanup := listeningNode(t)
	defer cleanup()
	if err := base.MineBlocks(2); err != nil { // the common base
		t.Fatal(err)
	}

	short := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer short.Close()
	feedBranch(t, short, base, 1, 2)
	if err := short.MineBlocks(2); err != nil { // branch A: heights 3–4
		t.Fatal(err)
	}

	long := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer long.Close()
	feedBranch(t, long, base, 1, 2)
	if err := long.MineBlocks(5); err != nil { // branch B: heights 3–7
		t.Fatal(err)
	}

	dir := t.TempDir()
	node, _, err := OpenNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, dir)
	if err != nil {
		t.Fatal(err)
	}
	feedBranch(t, node, base, 1, 2)
	feedBranch(t, node, short, 3, 4)
	if _, h, id := node.chain.Tip(); h != 4 || id != mustTipID(t, short) {
		t.Fatalf("the node is not on branch A: height %d", h)
	}

	// The heavier branch switches both the active branch and the UTXO set.
	feedBranch(t, node, long, 3, 7)
	_, wantH, wantID := long.chain.Tip()
	if _, h, id := node.chain.Tip(); h != wantH || id != wantID {
		t.Fatalf("the node did not switch to branch B: height %d", h)
	}
	tip, err := node.store.Tip()
	if err != nil || tip.ID != wantID {
		t.Fatalf("the UTXO set is not on branch B: %+v (%v)", tip, err)
	}
	node.Close()

	// The restart: the chain is assembled from the height index.
	again, height, err := OpenNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, dir)
	if err != nil {
		t.Fatalf("the restart failed: %v", err)
	}
	defer again.Close()
	if height != wantH {
		t.Fatalf("height %d after the restart, expected %d", height, wantH)
	}
	_, h, id := again.chain.Tip()
	if h != wantH || id != wantID {
		t.Fatalf("the tip after the restart is %d (%x), expected %d (%x)", h, id[:6], wantH, wantID[:6])
	}
	if a, b := again.coinCount(), long.coinCount(); a != b {
		t.Fatalf("the UTXO set does not match after the restart: %d vs %d", a, b)
	}
}

// Headers from a branch that lost must not overwrite the manifest: after a restart the node has
// to come up on the heavier branch, not on a chain glued together from both.
//
// That is why the height index is written only by `SetActiveBranch` (called when the tip changes)
// and not by `PutHeader`. When every accepted header wrote it, the last header accepted decided
// what the node thinks about its own chain after a restart — and that is the branch an attacker
// picks.
func TestLosingBranchHeadersDoNotRewriteManifest(t *testing.T) {
	base, _, cleanup := listeningNode(t)
	defer cleanup()
	if err := base.MineBlocks(2); err != nil {
		t.Fatal(err)
	}

	long := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer long.Close()
	feedBranch(t, long, base, 1, 2)
	if err := long.MineBlocks(5); err != nil { // the heavier branch, heights 3–7
		t.Fatal(err)
	}

	short := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer short.Close()
	feedBranch(t, short, base, 1, 2)
	if err := short.MineBlocks(2); err != nil { // the weaker branch, heights 3–4
		t.Fatal(err)
	}

	dir := t.TempDir()
	node, _, err := OpenNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, dir)
	if err != nil {
		t.Fatal(err)
	}
	feedBranch(t, node, base, 1, 2)
	feedBranch(t, node, long, 3, 7)
	_, wantH, wantID := long.chain.Tip()

	// The weaker branch arrives second — the tip must not move…
	feedBranch(t, node, short, 3, 4)
	if _, h, id := node.chain.Tip(); h != wantH || id != wantID {
		t.Fatalf("the weaker branch switched the tip to %d (%x)", h, id[:6])
	}
	node.Close()

	// …and after a restart the node still has to stand on the heavier one.
	again, height, err := OpenNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, dir)
	if err != nil {
		t.Fatalf("the restart failed (does the manifest mix branches?): %v", err)
	}
	defer again.Close()
	_, h, id := again.chain.Tip()
	if height != wantH || h != wantH || id != wantID {
		t.Fatalf("the tip after the restart is %d (%x), expected %d (%x)", h, id[:6], wantH, wantID[:6])
	}
}

// A block has to reach a peer that is not connected to the miner directly either.
//
// This is an ordinary topology: a pool node mines, public nodes sit in front of it and hidden
// nodes hang behind them. If a node announced only its own mined block, the relay would be
// single-hop and the hidden nodes would stay a few heights below the tip.
func TestBlockRelayCrossesTwoHops(t *testing.T) {
	miner, minerAddr, cleanupA := listeningNode(t)
	defer cleanupA()

	// The middle node listens and is at the same time connected to the miner.
	middle := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer middle.Close()
	mln, err := p2p.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer mln.Close()
	go middle.Listen(mln)
	mtcp := mln.Addr().(*net.TCPAddr)
	middleAddr := p2p.Address{IP: mtcp.IP, Port: uint16(mtcp.Port), LastSeen: time.Now()}

	stopM := make(chan struct{})
	defer close(stopM)
	middle.startLoop(stopM, 20*time.Millisecond, func() { middle.SyncBodiesOnce(time.Now()) })
	middle.addrs.Add(minerAddr, net.ParseIP("88.198.113.1"))
	if err := middle.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatal(err)
	}

	// The tail only knows the middle node — it does not see the miner.
	tail := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	defer tail.Close()
	stopT := make(chan struct{})
	defer close(stopT)
	tail.startLoop(stopT, 20*time.Millisecond, func() { tail.SyncBodiesOnce(time.Now()) })
	tail.addrs.Add(middleAddr, net.ParseIP("45.83.104.1"))
	if err := tail.FillOutboundOnce(p2p.PeerOutboundFull); err != nil {
		t.Fatal(err)
	}

	if err := miner.MineBlocks(2); err != nil {
		t.Fatal(err)
	}
	waitForApplied(t, tail, 2, 30*time.Second)

	// And once more, so this is not only about the initial sync after connecting.
	if err := miner.MineBlocks(1); err != nil {
		t.Fatal(err)
	}
	waitForApplied(t, tail, 3, 30*time.Second)

	_, h, id := tail.chain.Tip()
	_, wantH, wantID := miner.chain.Tip()
	if h != wantH || id != wantID {
		t.Fatalf("the tail is at %d (%x), the miner at %d (%x)", h, id[:6], wantH, wantID[:6])
	}
}
