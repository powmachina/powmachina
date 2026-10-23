package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log"
	"math"
	"net"
	"powmachina/internal/store"
	"powmachina/internal/stratum"
	"sync"
	"sync/atomic"
	"time"

	"powmachina/internal/address"
	"powmachina/internal/blocksync"
	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// Node ties together the header chain, the addrman and the peer manager: it listens, shakes
// hands, serves getheaders and syncs from a peer — exactly what a running network needs.
type Node struct {
	params  chain.NetParams
	chain   *chain.HeaderChain
	store   *store.Store
	mempool *chain.Mempool
	// orphans holds transactions whose parent has not arrived yet. Without it a child that
	// overtook its parent is silently dropped — and with chained payments that is a coin flip.
	orphans *chain.OrphanPool
	// datadir is the node directory; empty when running in memory. The mempool is dumped into it.
	datadir    string
	catchingUp bool // DAG auto mode: the node is catching up on history (guarded by mu)
	finality   *chain.FinalityState
	// signer is the node's finality key (finality.go); its blocks are its seats in the quorum.
	signer     *finalitySigner
	noAttest   bool // the node keeps its key in its blocks but does not sign (tests)
	seatMu     sync.Mutex
	seats      map[[32]byte]chain.Seat    // block id → its seat (guarded by seatMu)
	quorums    map[[32]byte]*chain.Quorum // block id → the quorum that finalizes it (guarded by seatMu)
	finalityMu sync.Mutex                 // one writer of the finality state file at a time
	addrs      *p2p.AddrMan
	peers      *p2p.Manager
	verifier   *mh.Verifier
	epochs     *epochCache
	settled    settledIndex     // what the recent blocks settled (settlement.go)
	shares     *chain.SharePool // the verified shares a block could still settle
	nonce      uint64
	agent      string
	nodeName   string
	minerKey   []byte     // where development mining (-mine) pays; the node has no keys otherwise
	genMu      sync.Mutex // generatetoaddress one at a time
	// applyMu serialises accepting a block (from a peer, submitblock, -mine) — one at a time.
	// It is an RWMutex and not a Mutex because of the **read** side: a mining template has to
	// see the tip and the mempool as one state. Applying a block moves the tip in
	// ApplyBlockDirect/ApplyStored and only then cleans the mempool in onBlockApplied, so a
	// template built in between gets the new tip with the old mempool — it carries a
	// transaction the new block has already confirmed, and the block the miner eventually
	// finds is thrown away with `invalid body: the input does not exist or is already spent`.
	// Template() therefore holds RLock, which makes the pair consistent. Nothing on that path
	// takes applyMu again, so it cannot deadlock.
	applyMu sync.RWMutex
	stratum *stratum.Server
	// blockSources are HTTP mirrors of block bodies (`-blocksource`). Accelerators, not authorities.
	blockSources []string

	// gate decides on incoming connections before the handshake (limits per machine, per
	// network, on unfinished handshakes and on connection rate). See internal/p2p/guard.go.
	gate *p2p.Gate
	// rejectLog rate-limits the log of rejected connections — a flood would otherwise fill the disk.
	rejectLog *p2p.TokenBucket
	// clockLog keeps the clock warning down to one line per 10 minutes.
	clockLog *p2p.TokenBucket
	rejected uint64
	// banListPath is the file with active bans for the firewall (`-banlist`). Empty = not written.
	banListPath  string
	banListDirty bool

	// bodies tracks which block bodies are in flight — see bodysync.go.
	bodies *bodySync
	// closing is closed by Close and the maintenance loops end on it; loops waits for them.
	closing   chan struct{}
	closeOnce sync.Once
	loops     sync.WaitGroup
	// clock collects how far our clock differs from the peers'. Consensus does not follow it
	// (that stays on the local clock), it is diagnostics — a machine with a shifted clock
	// otherwise cuts itself off the network silently and has no way to find out.
	clock p2p.ClockSkew
	// warmedEpoch is the epoch the node already started building ahead (under n.mu). Without
	// it the build would start over at every block before the boundary.
	warmedEpoch uint64
	// lastHeaderPoll is when the node last asked its peers for headers (under n.mu).
	lastHeaderPoll time.Time
	// lastBodyWarn is when the node last complained about a body nobody delivers (under n.mu).
	lastBodyWarn time.Time
	// verifyingHeaders counts the header batches being verified right now. A connection reads
	// one message at a time, so while its batch is being verified — over a minute for a full
	// message without the full DAG — no body can arrive on it, however willing the peer.
	verifyingHeaders atomic.Int32
	// reverted are the transactions of blocks the UTXO set rolled back in a reorg, and reorged
	// says that it did (both under applyMu). The mempool is brought in line with the new branch
	// once applying has caught up (settleMempool).
	reverted []*chain.Tx
	reorged  bool
	// bodyWaitID and bodyWaitSince say which body the UTXO set is waiting for and since when
	// (under n.mu) — a block whose body does not come is parked after bodyParkAfter.
	bodyWaitID    [32]byte
	bodyWaitSince time.Time
	// lastParkedAsk is when the bodies of parked blocks were last asked for (under n.mu).
	lastParkedAsk time.Time
	// lastAttestPoll is when it last asked for missing finality attestations (under n.mu).
	lastAttestPoll time.Time

	mu    sync.Mutex
	conns map[string]*p2p.Conn // connections something can be sent to
	// fullDag are the connections whose peer said in its handshake that it holds the full DAG:
	// the ones shares go to — a light node drops them (settlement.go).
	fullDag map[string]bool
	// shareLinks are the compact-share dictionaries of every connection (sharewire.go).
	shareLinks map[string]*shareLink
	// gossips are the connections that take part in address gossip (addrgossip.go).
	gossips map[string]bool
	// listenPort is the P2P port the node listens on, 0 when it does not; it announces it.
	listenPort atomic.Uint32
	// privatePeers are the -addnode peers on private addresses, set once by Bootstrap.
	privatePeers []p2p.Address
	// live are ALL open connections including those before the handshake. `conns` gets them
	// only after the handshake, so without this Close would wait for a foreign peer's
	// handshake (20 s).
	live map[*p2p.Conn]struct{}
}

// NewNode builds a node with **in-memory** storage. For tests and for short runs — after a
// restart the chain is downloaded again. A production node wants OpenNode with a datadir.
func NewNode(params chain.NetParams, genesis *chain.Header, profile mh.DagProfile) *Node {
	st, err := store.OpenMem()
	if err != nil {
		panic("the in-memory store cannot be opened: " + err.Error())
	}
	if err := st.InitGenesis(genesis); err != nil {
		panic("the genesis cannot be written: " + err.Error())
	}
	return newNode(params, genesis, profile, st)
}

// OpenNode opens a node over a datadir. This is the production path: the chain is stored on
// disk, so a restart means seconds rather than a resync.
//
// It also returns the height it continues at — the caller should print it so the operator
// sees that nothing was lost.
func OpenNode(params chain.NetParams, genesis *chain.Header, profile mh.DagProfile,
	datadir string) (*Node, uint64, error) {
	return OpenNodeWith(params, genesis, profile, datadir, store.Options{})
}

// OpenNodeWith is OpenNode with storage options — for instance with the transaction index on.
//
// When an index is turned on over a directory that does not have one, the chain is walked
// from scratch. Without that the index would cover only newer blocks and older transactions
// would silently "not exist".
func OpenNodeWith(params chain.NetParams, genesis *chain.Header, profile mh.DagProfile,
	datadir string, opt store.Options) (*Node, uint64, error) {
	if err := os.MkdirAll(datadir, 0o700); err != nil {
		return nil, 0, fmt.Errorf("datadir: %w", err)
	}
	st, err := store.OpenWith(filepath.Join(datadir, "chain"), opt)
	if err != nil {
		return nil, 0, err
	}
	n := newNode(params, genesis, profile, st)
	n.datadir = datadir
	if n.finality != nil {
		signer, err := loadFinalitySigner(datadir)
		if err != nil {
			st.Close()
			return nil, 0, err
		}
		n.signer = signer
	}

	// The genesis has to be in storage, otherwise the height index has nothing to stand on.
	// It also catches a datadir from another network — a reason to refuse to start, not to overwrite it.
	if err := st.InitGenesis(genesis); err != nil {
		st.Close()
		return nil, 0, err
	}

	// The marks on invalid branches have to be in the chain before headers start loading —
	// otherwise an invalid branch would be taken back on startup and the node would stall on
	// the same block as before.
	bad, err := st.InvalidIDs()
	if err != nil {
		st.Close()
		return nil, 0, fmt.Errorf("the list of invalid blocks cannot be read: %w", err)
	}
	for _, id := range bad {
		n.chain.MarkInvalidLoaded(id)
	}
	if len(bad) > 0 {
		log.Printf("invalid branches in the datadir: %d", len(bad))
	}

	height, err := st.Restore(n.chain)
	if err != nil {
		st.Close()
		return nil, 0, fmt.Errorf("restore from the datadir failed: %w", err)
	}
	// Blocks stored on disk but not applied yet — a crash between writing a body and applying
	// it is a normal state, not an error.
	if applied, err := st.ApplyStored(height+1000, store.ApplyHooks{
		Validate: func(b *chain.Block) error {
			if err := b.ValidateBlock(st, params); err != nil {
				return err
			}
			return n.checkSettlement(b)
		},
	}); err != nil {
		st.Close()
		return nil, 0, err
	} else if applied > 0 {
		log.Printf("applied %d more blocks from disk (without the network)", applied)
		if t, err := st.Tip(); err == nil {
			height = t.Height
		}
	}

	// The transaction index has to cover the whole chain, otherwise older transactions would
	// silently "not exist" — and that looks like a valid answer, not like an error.
	if opt.TxIndex && !st.TxIndexReady() {
		log.Printf("rebuilding the transaction index from scratch (up to height %d)…", height)
		t0 := time.Now()
		if err := st.ReindexTxs(func(h, tip uint64) {
			if h > 0 {
				log.Printf("  reindex %d / %d", h, tip)
			}
		}); err != nil {
			st.Close()
			return nil, 0, fmt.Errorf("transaction reindex: %w", err)
		}
		log.Printf("transaction index done in %s", time.Since(t0).Round(time.Millisecond))
	}
	if opt.AddrIndex && !st.AddrIndexReady() {
		log.Printf("rebuilding the address index from scratch (up to height %d)…", height)
		t0 := time.Now()
		if err := st.ReindexAddrs(func(h, tip uint64) {
			if h > 0 {
				log.Printf("  address reindex %d / %d", h, tip)
			}
		}); err != nil {
			st.Close()
			return nil, 0, fmt.Errorf("address reindex: %w", err)
		}
		log.Printf("address index done in %s", time.Since(t0).Round(time.Millisecond))
	}

	n.loadFinality()

	// The mempool comes last: transactions are checked against a finished UTXO set and the
	// height the node really stands on.
	n.loadMempool(height, time.Now())
	return n, height, nil
}

func newNode(params chain.NetParams, genesis *chain.Header, profile mh.DagProfile,
	st *store.Store) *Node {
	addrs := p2p.NewAddrMan(nil)
	// On regtest we run on a LAN and on loopback, where private addresses have to pass. Not on
	// the public network: a node knocking on 10.0.0.0/8 from a cloud server looks like an
	// internal network scanner to the provider and earns abuse reports (`-allow-private`
	// overrides this).
	regtest := params.Magic == chain.RegTest.Magic
	addrs.AllowPrivate = regtest
	// Spreading connections across network groups is a defence against an eclipse attack, but
	// on regtest it would mean several nodes on one machine never connect: 127.0.0.1 is a
	// single group, so the second outbound connection would have nowhere to go.
	mgrCfg := p2p.DefaultManagerConfig()
	mgrCfg.RequireDistinctGroups = !regtest
	nonce := uint64(time.Now().UnixNano())
	// Development mining without -mineaddress pays to a throwaway key that is discarded — the
	// node holds no keys (the wallet is a separate process).
	var throwaway [32]byte
	if _, err := rand.Read(throwaway[:]); err != nil {
		panic(err)
	}
	minerPk, err := mh.SchnorrPubkey(&throwaway)
	if err != nil {
		panic(err)
	}
	// Finality is a property of the network: without a window it is off and the chain runs on
	// plain PoW (the finality spec).
	var finality *chain.FinalityState
	if params.FinalityWindow > 0 {
		finality = chain.NewFinalityState()
	}
	n := &Node{
		params:   params,
		chain:    chain.NewHeaderChain(params, genesis, finality),
		finality: finality,
		store:    st,
		mempool:  chain.NewMempool(params),
		orphans:  chain.NewOrphanPool(),
		shares:   chain.NewSharePool(chain.DefaultSharePoolSize),
		conns:    make(map[string]*p2p.Conn),
		live:     make(map[*p2p.Conn]struct{}),
		minerKey: minerPk[:],
		addrs:    addrs,
		peers:    p2p.NewManager(mgrCfg, addrs, nonce),
		epochs:   newEpochCache(profile),
		bodies:   newBodySync(),
		closing:  make(chan struct{}),
		nonce:    nonce,
		agent:    p2p.UserAgentFor(""),
		gate:     p2p.NewGate(p2p.DefaultGateConfig(), nil, time.Now()),
		// one line about rejections per 10 s, with the count rejected in between
		rejectLog: p2p.NewTokenBucket(0.1, 1, time.Now()),
		clockLog:  p2p.NewTokenBucket(1.0/600, 1, time.Now()),
		seats:     make(map[[32]byte]chain.Seat),
		quorums:   make(map[[32]byte]*chain.Quorum),
	}
	n.peers.OnBan = n.markBanListDirty
	n.chain.SetVotes(n.shares.Votes)
	// A node in memory gets a key for its lifetime; OpenNode replaces it with the one in the datadir.
	if finality != nil {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			panic(err)
		}
		n.signer = newFinalitySigner(priv, "")
	}

	return n
}

// SetNodeName sets the label the node introduces itself to peers under. Called before the
// network starts — after the handshake a change would mean nothing.
func (n *Node) SetNodeName(name string) error {
	if err := p2p.ValidateNodeName(name); err != nil {
		return err
	}
	n.nodeName = name
	n.agent = p2p.UserAgentFor(name)
	return nil
}

// SetMinerAddress sets where development mining (-mine) pays.
func (n *Node) SetMinerAddress(addr string) error {
	version, prog, err := address.Decode(n.params.Bech32HRP, addr)
	if err != nil {
		return err
	}
	if version != chain.ScriptVersionBasic || len(prog) != chain.PubkeyBytes {
		return fmt.Errorf("address %s cannot be used for a reward", addr)
	}
	n.mu.Lock()
	n.minerKey = prog
	n.mu.Unlock()
	return nil
}

// Close stops the maintenance loops, waits for them and closes storage. It is idempotent.
//
// The order matters: if storage closed before the body driver's tick finished, Pebble would
// panic on "pebble: closed" — and under load during shutdown that is a common state, not a
// theoretical one.
func (n *Node) Close() {
	n.closeOnce.Do(func() {
		// Closing the channel and unregistering the connections both happen under n.mu, so
		// there is no gap between "the node is closing" and "a goroutine just registered".
		n.mu.Lock()
		close(n.closing)
		live := make([]*p2p.Conn, 0, len(n.live))
		for c := range n.live {
			live = append(live, c)
		}
		n.mu.Unlock()
		// The connections are closed so the read loops end on an error right away. Without
		// that Wait would sit on a peer with nothing to send until ReadTimeout (90 s).
		for _, c := range live {
			c.Close()
		}
		n.loops.Wait()
		// The mempool dump goes before storage closes but after the loops — otherwise we would
		// be dumping a pool somebody is still writing to.
		n.saveMempool()
		n.epochs.close()
		if n.store != nil {
			if err := n.store.Close(); err != nil {
				log.Printf("⚠ storage did not close cleanly: %v", err)
			}
		}
	})
}

// services are the services the node advertises in the handshake. ServiceFullDag only when it really holds the full DAG.
func (n *Node) services() uint64 {
	s := p2p.ServiceFullNode | p2p.ServiceBlockRelay
	if n.epochs.isFull() {
		s |= p2p.ServiceFullDag
	}
	return s
}

// SetDagMode sets whether the node holds the full DAG. In auto mode it holds one only while catching up on history.
func (n *Node) SetDagMode(mode dagMode) {
	n.epochs.setMode(mode, n.syncing)
}

// Thresholds for the DAG auto mode. Hysteresis: a node 2 h behind (a restart, a network
// outage) does not rebuild a gigabyte back and forth — the full DAG only pays off over a
// long history.
const (
	syncingAfter = 24 * time.Hour // tip older → catching up on history, full DAG
	syncedWithin = time.Hour      // tip younger → we are at the tip, light
)

// syncing says whether the node is catching up on history. It is decided by the time of our
// own tip, which a foreign peer cannot move backwards.
func (n *Node) syncing() bool {
	tip, _, _ := n.chain.Tip()
	age := time.Since(tip.Timestamp())
	n.mu.Lock()
	defer n.mu.Unlock()
	switch {
	case age > syncingAfter:
		n.catchingUp = true
	case age < syncedWithin:
		n.catchingUp = false
	}
	return n.catchingUp
}

// pow returns the verifier for a given height. It builds the epoch if it does not have it yet.
func (n *Node) pow(height uint64) (chain.PoWChecker, error) {
	return n.epochs.forHeight(height)
}

func (n *Node) version() *p2p.VersionMsg {
	_, height, _ := n.chain.Tip()
	return &p2p.VersionMsg{
		Protocol:    p2p.ProtocolVersion,
		Services:    n.services(),
		Timestamp:   time.Now().Unix(),
		Nonce:       n.nonce,
		UserAgent:   n.agent,
		StartHeight: uint32(height),
		Relay:       true,
	}
}

// Listen accepts connections until the listener is closed.
//
// Everything cheap is decided **right after Accept, before the handshake**: bans, per-machine
// and per-network limits, the number of unfinished handshakes and the connection rate. A
// handshake costs a thread, memory and up to 20 seconds — nobody who would fail anyway may get one.
func (n *Node) Listen(ln net.Listener) {
	// Only a node listening where the internet reaches it announces itself. One listening on a
	// private address (a hidden node that its siblings in a VPN connect to) would otherwise be
	// announced under the public address its peers see it from — where nothing answers, and which
	// is exactly what it is hidden to keep to itself. A closed deployment (-allow-private) is
	// private throughout, so there it does.
	if a, ok := ln.Addr().(*net.TCPAddr); ok && (a.IP.IsUnspecified() || p2p.Routable(a.IP) || n.addrs.AllowPrivate) {
		n.listenPort.Store(uint32(a.Port))
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		conn := p2p.NewConn(c, n.params.Magic)
		addr := conn.RemoteAddress()
		if n.peers.IsBanned(addr) {
			n.reject(addr, "banned")
			conn.Close()
			continue
		}
		ticket, why := n.gate.Admit(addr, time.Now())
		if ticket == nil {
			n.reject(addr, why)
			conn.Close()
			continue
		}
		if !n.goTracked(func() { n.serve(conn, ticket) }) {
			// The node is closing — nobody will serve a new connection any more.
			ticket.Release()
			conn.Close()
			return
		}
	}
}

// reject logs a rejected connection — at most one line per 10 seconds, with the count
// rejected in between. A connection flood therefore cannot fill the disk with logs.
func (n *Node) reject(addr p2p.Address, why string) {
	n.mu.Lock()
	n.rejected++
	count := n.rejected
	ok := n.rejectLog.Take(1, time.Now())
	if ok {
		n.rejected = 0
	}
	n.mu.Unlock()
	if ok {
		log.Printf("rejected a connection from %v: %s (rejected since the last report: %d)", addr.IP, why, count)
	}
}

func (n *Node) serve(conn *p2p.Conn, ticket *p2p.Ticket) {
	defer conn.Close()
	defer ticket.Release()
	addr := conn.RemoteAddress()
	if !n.addLive(conn) {
		return
	}
	defer n.removeLive(conn)

	remote, err := conn.Handshake(n.version(), n.nonce, false)
	if err != nil {
		// A connection to ourselves is spotted by the accepting side (it reads version
		// first). The caller only gets a closed connection, so learning our own address
		// has to happen here.
		if errors.Is(err, p2p.ErrSelfConnection) {
			if !n.addrs.IsLocal(addr) {
				log.Printf("address %v is our own — dropped from the addrman", addr.IP)
			}
			n.addrs.MarkLocal(addr)
		}
		n.reject(addr, "handshake failed: "+err.Error())
		return
	}
	peer := p2p.NewPeer(addr, p2p.PeerInbound)
	if err := peer.CompleteHandshake(remote, n.nonce); err != nil {
		return
	}
	n.notePeerClock(remote)
	if _, err := n.peers.Add(peer); err != nil {
		n.reject(addr, err.Error())
		return
	}
	ticket.Established()
	defer n.peers.Remove(addr)
	n.addConn(addr, conn, peer)
	defer n.removeConn(addr)
	log.Printf("peer %v connected (%s, height %d)", addr.IP, remote.UserAgent, remote.StartHeight)

	n.readLoop(conn, peer, p2p.InboundPeerLimits())
}

// readLoop reads messages from a peer while the connection lives. A peer sending more than an
// honest peer ever sends gets misbehaviour points and is disconnected.
func (n *Node) readLoop(conn *p2p.Conn, peer *p2p.Peer, limits p2p.PeerLimits) {
	meter := p2p.NewPeerMeter(limits, time.Now())
	for {
		msg, err := conn.Recv()
		if err != nil {
			return
		}
		if !meter.Allow(p2p.HeaderSize+len(msg.Payload), time.Now()) {
			n.peers.Misbehaving(peer, p2p.MisbehaviorFlood)
			n.reject(peer.Addr, "sending too many messages")
			return
		}
		if err := n.handle(conn, peer, msg); err != nil {
			log.Printf("peer %v: %v", peer.Addr.IP, err)
			return
		}
	}
}

func (n *Node) handle(conn *p2p.Conn, peer *p2p.Peer, msg *p2p.Message) error {
	switch msg.Command {
	case p2p.CmdPing:
		// The payload is sent back, so its length has to be kept in check — otherwise a peer
		// would send a ping with a megabyte of data and we would pay for the reply.
		if len(msg.Payload) != p2p.PingPayloadSize {
			n.peers.Misbehaving(peer, p2p.MisbehaviorBadMessage)
			return fmt.Errorf("ping has %d B, expected %d", len(msg.Payload), p2p.PingPayloadSize)
		}
		return conn.Send(&p2p.Message{Command: p2p.CmdPong, Payload: msg.Payload})

	case p2p.CmdPong:
		if err := conn.Pong(msg.Payload, time.Now()); err != nil {
			n.peers.Misbehaving(peer, p2p.MisbehaviorBadMessage)
			return err
		}
		return nil

	case p2p.CmdGetHeaders:
		locator, err := p2p.DecodeGetHeaders(msg.Payload)
		if err != nil {
			n.peers.Misbehaving(peer, p2p.MisbehaviorBadMessage)
			return err
		}
		payload, err := p2p.EncodeHeaders(n.chain.HeadersFrom(locator, p2p.MaxHeadersPerMessage))
		if err != nil {
			return err
		}
		return conn.Send(&p2p.Message{Command: p2p.CmdHeaders, Payload: payload})

	case p2p.CmdGetData:
		items, err := p2p.DecodeGetData(msg.Payload)
		if err != nil {
			n.peers.Misbehaving(peer, p2p.MisbehaviorBadMessage)
			return err
		}
		for _, it := range items {
			switch it.Type {
			case p2p.InvBlock:
				n.mu.Lock()
				b, err := n.store.Block(it.ID)
				ok := err == nil
				n.mu.Unlock()
				if !ok {
					continue
				}
				payload, err := p2p.EncodeBlock(b)
				if err != nil {
					return err
				}
				if err := conn.Send(&p2p.Message{Command: p2p.CmdBlock, Payload: payload}); err != nil {
					return err
				}
			case p2p.InvTx:
				e, ok := n.mempool.Get(it.ID)
				if !ok {
					continue
				}
				payload, err := p2p.EncodeTx(e.Tx)
				if err != nil {
					return err
				}
				if err := conn.Send(&p2p.Message{Command: p2p.CmdTx, Payload: payload}); err != nil {
					return err
				}
			}
		}
		return nil

	case p2p.CmdInv:
		items, err := p2p.DecodeInv(msg.Payload)
		if err != nil {
			n.peers.Misbehaving(peer, p2p.MisbehaviorBadMessage)
			return err
		}
		// We ask only for what we really do not have — and for a body only when we have a
		// verified header for it. Without one we have nothing to verify it against:
		// `acceptBlock` would drop it, so we would request megabytes of data and pay for
		// them for nothing.
		var want []p2p.InvItem
		needHeaders, needBodies := false, false
		for _, it := range items {
			switch it.Type {
			case p2p.InvTx:
				// A transaction from the orphan pool is not requested a second time: we have it
				// already, it is only missing a parent — and a second copy of the child will not
				// bring that.
				if _, have := n.mempool.Get(it.ID); !have && !n.orphans.Has(it.ID) {
					want = append(want, it)
				}
			case p2p.InvBlock:
				if n.store.HasBody(it.ID) {
					continue
				}
				if !n.chain.Has(it.ID) {
					// We do not know the announced block — it takes the same path as a sync:
					// headers first, bodies only after.
					needHeaders = true
					continue
				}
				needBodies = true
			}
			if len(want) >= p2p.MaxGetDataItems {
				break
			}
		}
		if needHeaders {
			if err := n.RequestHeaders(conn); err != nil {
				return err
			}
		}
		if needBodies {
			// We do not request bodies from here: the owner of those requests is the driver in
			// bodysync.go, which knows what is in flight and what has to be retried. Here we
			// only wake it up so an announced block does not wait for the next tick.
			n.SyncBodiesOnce(time.Now())
		}
		if len(want) == 0 {
			return nil
		}
		payload, err := p2p.EncodeGetData(want)
		if err != nil {
			return err
		}
		return conn.Send(&p2p.Message{Command: p2p.CmdGetData, Payload: payload})

	case p2p.CmdAttestation:
		att, err := p2p.DecodeAttestation(msg.Payload)
		if err != nil {
			n.peers.Misbehaving(peer, p2p.MisbehaviorBadMessage)
			return err
		}
		return n.acceptAttestation(att, addrOf(peer))

	case p2p.CmdGetAttest:
		// A peer asks for attestations it is missing (typically after a restart — finality
		// state lives in memory only and duplicates are deliberately not relayed).
		from, count, err := p2p.DecodeGetAttest(msg.Payload)
		if err != nil {
			n.peers.Misbehaving(peer, p2p.MisbehaviorBadMessage)
			return err
		}
		if n.finality == nil {
			return nil
		}
		// `from` and `count` are the peer's: an empty range and one that runs past the last height
		// are both answered with what there is, without the sum wrapping around.
		var list []chain.Attestation
		if count > 0 {
			to := from + uint64(count) - 1
			if to < from {
				to = math.MaxUint64
			}
			list = n.finality.InRange(from, to, p2p.MaxAttestPerMessage)
		}
		payload, err := p2p.EncodeAttestations(list)
		if err != nil {
			return err
		}
		return conn.Send(&p2p.Message{Command: p2p.CmdAttestations, Payload: payload})

	case p2p.CmdAttestations:
		list, err := p2p.DecodeAttestations(msg.Payload)
		if err != nil {
			n.peers.Misbehaving(peer, p2p.MisbehaviorTooManyItems)
			return err
		}
		for _, att := range list {
			// The same path as an attestation from the network: the signature is verified, a
			// duplicate is dropped and whatever is new is relayed on.
			_ = n.acceptAttestation(att, addrOf(peer))
		}
		return nil

	case p2p.CmdEquivocation:
		ev, err := p2p.DecodeEquivocation(msg.Payload)
		if err != nil {
			n.peers.Misbehaving(peer, p2p.MisbehaviorBadMessage)
			return err
		}
		n.acceptEquivocation(ev, addrOf(peer))
		return nil

	case p2p.CmdGetAddr:
		n.answerGetAddr(conn, peer)
		return nil

	case p2p.CmdAddr:
		list, err := p2p.DecodeAddr(msg.Payload)
		if err != nil {
			n.peers.Misbehaving(peer, p2p.MisbehaviorBadMessage)
			return err
		}
		n.acceptAddrs(peer, list)
		return nil

	case p2p.CmdShares:
		return n.receiveShares(peer, msg.Payload)

	case p2p.CmdTx:
		tx, err := p2p.DecodeTx(msg.Payload)
		if err != nil {
			n.peers.Misbehaving(peer, p2p.MisbehaviorBadMessage)
			return err
		}
		return n.acceptTx(peer, tx, addrOf(peer))

	case p2p.CmdBlock:
		blk, err := p2p.DecodeBlock(msg.Payload)
		if err != nil {
			n.peers.Misbehaving(peer, p2p.MisbehaviorBadMessage)
			return err
		}
		return n.acceptBlock(peer, blk)

	case p2p.CmdHeaders:
		headers, err := p2p.DecodeHeaders(msg.Payload)
		if err != nil {
			n.peers.Misbehaving(peer, p2p.MisbehaviorTooManyItems)
			return err
		}
		// How far the peer reaches is noted before anything else, and from every headers
		// message — including one that brings nothing new. This is the only place where the
		// number is refreshed after the handshake; without it `best_known` stays at the height
		// the peer had when it connected.
		for _, h := range headers {
			peer.NoteHeight(uint64(h.Height))
		}
		added, err := n.acceptHeaders(peer, headers)
		if err != nil {
			return err
		}
		if added == 0 {
			return nil // nothing new — the sync is done
		}
		// Bodies are pulled by the driver according to what is missing between the UTXO tip and
		// the header tip — not according to what arrived in this message. A message can carry
		// up to MaxHeadersPerMessage headers, but a single getdata only fits MaxGetDataItems
		// bodies; driven by the message, the sync would stall there forever.
		n.SyncBodiesOnce(time.Now())
		// The peer may have more headers; we keep asking while any keep coming.
		return n.RequestHeaders(conn)

	default:
		// Unknown messages are ignored — new protocol versions therefore do not knock old nodes over.
		return nil
	}
}

func (n *Node) acceptHeaders(peer *p2p.Peer, headers []*chain.Header) (int, error) {
	n.verifyingHeaders.Add(1)
	defer n.verifyingHeaders.Add(-1)
	added, tipMoved, invalidSeen := 0, false, 0
	for _, h := range headers {
		// The height in a header is whatever the peer wrote there. The epoch — a DAG of gigabytes
		// and a RandomX cache — is built only once the header links to a block we know, at the
		// height that follows it, and passes every other check that costs nothing.
		changed := false
		err := n.chain.CheckHeader(h)
		if err == nil {
			var pow chain.PoWChecker
			if pow, err = n.pow(uint64(h.Height)); err != nil {
				return added, err
			}
			changed, err = n.chain.AddHeader(h, pow)
		}
		switch {
		case err == nil:
			// The header is verified — store it. It reaches the height index only further
			// below, and only if it is on the active branch: the index is the manifest bodies
			// are pulled by and the chain is restored from after a restart.
			if perr := n.store.PutHeader(h); perr != nil {
				return added, perr
			}
			added++
			tipMoved = tipMoved || changed
		case err == chain.ErrDuplicate:
			continue
		case err == chain.ErrUnknownParent:
			// The peer sent headers out of order or from another branch — not a reason to ban.
			continue
		case errors.Is(err, chain.ErrTimeTooNew):
			// "Too far in the future" is measured against our clock. When ours is behind, every
			// honest peer sends such headers — charging them would cut a node with a slow clock
			// off the network. The header is not filed; it will be offered again.
			continue
		case errors.Is(err, chain.ErrForkTooDeep):
			// A branch that leaves ours deeper than we would ever reorganize. The peer may be on
			// another chain, or we may be: neither is something to charge it for — a node that
			// was fed a wrong history would otherwise ban the honest network one peer at a time.
			continue
		case errors.Is(err, chain.ErrInvalidAncestor):
			// We have already thrown away a branch whose body failed validation. The peer
			// cannot know our verdict (it has a different UTXO set), so we do not punish it
			// for this — we just ignore it, and this time for free.
			invalidSeen++
			continue
		default:
			// Invalid PoW after all the cheap checks = an instant ban: an honest peer cannot
			// send it and every such header costs us a PoW computation.
			points := p2p.MisbehaviorBadHeader
			if errors.Is(err, chain.ErrBadPoW) {
				points = p2p.MisbehaviorBadPoW
			}
			n.peers.Misbehaving(peer, points)
			return added, fmt.Errorf("invalid header at height %d: %w", h.Height, err)
		}
	}
	if tipMoved {
		// The height index is **not written** here. The only place that writes it is
		// ApplyReadyBodies under applyMu — if two places did it, two concurrent header
		// acceptances could overwrite the manifest with an older view of the chain and applying
		// would stop. The body driver is woken right after this function, so the index lags by at
		// most one round.
		//
		// Announce the new tip onwards. Without this the relay is **single-hop**: a block
		// reaches the miner's direct peers and stops there — a pool node and the public nodes
		// next to it grow while the hidden nodes behind them stand still.
		//
		// What is announced is a verified header, not a body: the receiver pulls the headers and
		// bodies itself from it, so 89 B travel instead of 256 kB.
		_, _, tip := n.chain.Tip()
		n.announce([]p2p.InvItem{{Type: p2p.InvBlock, ID: tip}}, addrOf(peer))
	}
	if added > 0 {
		_, height, id := n.chain.Tip()
		log.Printf("accepted %d headers, the tip is now %d (%x)", added, height, id[:6])
	}
	if invalidSeen > 0 {
		n.reject(peer.Addr, fmt.Sprintf("offers %d headers from an invalid branch", invalidSeen))
	}
	return added, nil
}

// RequestHeaders asks a peer for the headers after our tip.
//
// Called after every established outbound connection. Without it the node would connect but
// never learn that a longer chain exists.
func (n *Node) RequestHeaders(conn *p2p.Conn) error {
	payload, err := p2p.EncodeGetHeaders(n.chain.Locator())
	if err != nil {
		return err
	}
	return conn.Send(&p2p.Message{Command: p2p.CmdGetHeaders, Payload: payload})
}

// announceRaw sends a finished message to every peer except the source.
func (n *Node) announceRaw(cmd p2p.Command, payload []byte, except string) {
	n.mu.Lock()
	targets := make([]*p2p.Conn, 0, len(n.conns))
	for key, c := range n.conns {
		if key == except {
			continue
		}
		targets = append(targets, c)
	}
	n.mu.Unlock()
	for _, c := range targets {
		_ = c.Send(&p2p.Message{Command: cmd, Payload: payload})
	}
}

// acceptTx verifies a transaction, files it into the mempool and offers it to the other peers.
//
// An invalid transaction is not a reason to ban — it may be a conflict or something we have
// already seen. Only what cannot even be decoded is banned (the caller handles that).
func (n *Node) acceptTx(peer *p2p.Peer, tx *chain.Tx, from string) error {
	_, height, _ := n.chain.Tip()
	id := tx.TxID()
	if _, err := n.mempool.Accept(tx, n.store, height+1); err != nil {
		// A missing parent is the only rejection worth retrying: a child and its parent travel
		// the network independently, so overtaking is not an exception. The other rejections
		// (signature, fee, conflict) do not change on a second attempt.
		var miss *chain.MissingInputs
		if errors.As(err, &miss) {
			if parents := n.orphans.Add(tx, miss.Outpoints, time.Now()); len(parents) > 0 {
				log.Printf("transaction %x is waiting for %d parents (%d in the orphan pool)",
					id[:6], len(parents), n.orphans.Len())
				n.requestParents(parents, from)
			}
		}
		return nil
	}
	log.Printf("transaction %x accepted into the mempool (%d in the pool)", id[:6], n.mempool.Len())
	n.announce([]p2p.InvItem{{Type: p2p.InvTx, ID: id}}, from)
	// An accepted transaction is a parent for those that overtook it.
	n.resolveOrphans([][32]byte{id}, from)
	return nil
}

// maxBlockPushBytes is the cap up to which a block is pushed to peers whole instead of being
// announced. A block of the Sharepool carries its settlement body, ~45 kB at 3,000 shares, and at a
// 30 s block every round trip of an `inv` is a share of the orphan rate — so the usual block is
// pushed. The cap is here so that a block at its limit (chain.MaxBlockWireBytes) × every peer never
// becomes the default. Above it the peer gets an `inv` and pulls what it wants — which is also where
// compact blocks (the shares a peer already has, by id) would belong one day.
const maxBlockPushBytes = 128 << 10

// announceBlock hands a new block to the peers **whole**: one header message and the body
// right behind it.
//
// Why not just an `inv`: with an announcement the peer has to ask for the headers, verify
// them, and only then may it ask for the body — and the body request is driven by the sync
// driver, so it is a round trip plus up to one driver tick **per hop**. When one of those
// requests goes unanswered, the next attempt is a whole timeout away. On a three-hop path that
// chain of waits can hold one body minutes behind, and because nothing can be applied past a
// hole, the UTXO set of every node stops with it.
//
// The body is pushed unsolicited, so it must stand on its own: the header goes first in the
// same stream, the receiver verifies its PoW as it does for any header, and only then does the
// body verify against it (storeBody). A peer that does not want it drops it — the cost is one
// message.
func (n *Node) announceBlock(blk *chain.Block, except string) {
	raw := blk.Serialize()
	if len(raw) > maxBlockPushBytes {
		n.announce([]p2p.InvItem{{Type: p2p.InvBlock, ID: blk.Header.ID()}}, except)
		return
	}
	headers, err := p2p.EncodeHeaders([]*chain.Header{blk.Header})
	if err != nil {
		n.announce([]p2p.InvItem{{Type: p2p.InvBlock, ID: blk.Header.ID()}}, except)
		return
	}
	body, err := p2p.EncodeBlock(blk)
	if err != nil {
		n.announce([]p2p.InvItem{{Type: p2p.InvBlock, ID: blk.Header.ID()}}, except)
		return
	}
	n.mu.Lock()
	targets := make([]*p2p.Conn, 0, len(n.conns))
	for key, c := range n.conns {
		if key == except {
			continue
		}
		targets = append(targets, c)
	}
	n.mu.Unlock()
	for _, c := range targets {
		// The order matters: a body whose header the peer does not know yet is dropped
		// (storeBody), so the header has to be in the stream first.
		if err := c.Send(&p2p.Message{Command: p2p.CmdHeaders, Payload: headers}); err != nil {
			continue
		}
		_ = c.Send(&p2p.Message{Command: p2p.CmdBlock, Payload: body})
	}
}

// announce offers items to every peer except the one they came from.
func (n *Node) announce(items []p2p.InvItem, except string) {
	payload, err := p2p.EncodeInv(items)
	if err != nil {
		return
	}
	n.mu.Lock()
	targets := make([]*p2p.Conn, 0, len(n.conns))
	for key, c := range n.conns {
		if key == except {
			continue
		}
		targets = append(targets, c)
	}
	n.mu.Unlock()

	for _, c := range targets {
		_ = c.Send(&p2p.Message{Command: p2p.CmdInv, Payload: payload})
	}
}

func (n *Node) addConn(addr p2p.Address, c *p2p.Conn, peer *p2p.Peer) {
	n.mu.Lock()
	n.conns[addr.String()] = c
	if n.fullDag == nil {
		n.fullDag = make(map[string]bool)
		n.gossips = make(map[string]bool)
		n.shareLinks = make(map[string]*shareLink)
	}
	n.shareLinks[addr.String()] = newShareLink(c)
	if peer != nil && peer.Version != nil && peer.Version.Services&p2p.ServiceFullDag != 0 {
		n.fullDag[addr.String()] = true
	}
	if peer != nil && addrPeer(peer) {
		n.gossips[addr.String()] = true
	}
	n.mu.Unlock()
}

func (n *Node) removeConn(addr p2p.Address) {
	n.mu.Lock()
	delete(n.conns, addr.String())
	delete(n.fullDag, addr.String())
	delete(n.gossips, addr.String())
	delete(n.shareLinks, addr.String())
	n.mu.Unlock()
}

func addrOf(p *p2p.Peer) string { return p.Addr.String() }

// notePeerClock records the time from the handshake and says so now and then when the
// machine's clock is off.
//
// Why it is worth a line in the log: the allowed drift (NetParams.FutureDrift, 30 s) is measured
// against the **local** clock. A machine half a minute ahead produces blocks the network
// rejects; half a minute behind, it rejects foreign blocks as "too far in the future". From the
// outside that looks like a network outage.
func (n *Node) notePeerClock(remote *p2p.VersionMsg) {
	n.clock.Add(remote.Timestamp, time.Now())
	skew, off := n.clock.Off()
	if !off {
		return
	}
	n.mu.Lock()
	warn := n.clockLog.Take(1, time.Now())
	n.mu.Unlock()
	if warn {
		_, height, _ := n.chain.Tip()
		log.Printf("⚠ this machine's clock is %v off the network (median of the peers). "+
			"Above %v the network starts rejecting your blocks and you theirs — check NTP.",
			skew.Round(time.Second), n.params.FutureDrift(height+1))
	}
}

// ClockSkew is the clock offset from the network, for RPC.
func (n *Node) ClockSkew() (time.Duration, int) { return n.clock.Median() }

// acceptBlock takes a block body from the network: verifies it against the header manifest,
// stores it and applies everything whose turn has come.
//
// A body out of order is **neither applied nor dropped** — it is stored and waits until the
// UTXO set catches up below it (`ApplyReadyBodies`). Applied right away, the body of a new
// block announced over `inv` would end on "the block does not build on the tip", the handler
// would return an error and the read loop would hang up the connection. On every block.
func (n *Node) acceptBlock(peer *p2p.Peer, blk *chain.Block) error {
	if err := n.storeBody(peer, blk); err != nil {
		return err
	}
	return n.ApplyReadyBodies()
}

// submitblock errors — a miner tells from them whether to take a new template right away.
var (
	ErrBlockStale     = errors.New("the block does not build on the current tip (stale) — take a new template")
	ErrBlockDuplicate = errors.New("we already have the block")
)

// SubmitBlock accepts a block mined by a miner (RPC submitblock, and -mine too): it verifies
// the header including PoW and the lane rotation, the body, stores it, cleans the mempool and
// announces it to the peers. The block has to build on the current tip — a miner does not
// mine onto a side branch.
func (n *Node) SubmitBlock(blk *chain.Block) error {
	n.applyMu.Lock()
	defer n.applyMu.Unlock()
	h := blk.Header
	id := h.ID()
	if n.chain.Has(id) {
		return ErrBlockDuplicate
	}
	if _, tipHeight, tipID := n.chain.Tip(); h.PrevBlock != tipID || uint64(h.Height) != tipHeight+1 {
		return ErrBlockStale
	}
	// The body is verified before the header goes into the chain — a header without a valid
	// body would otherwise move the header tip to a block that will never have one.
	if err := blk.ValidateBlock(n.store, n.params); err != nil {
		return fmt.Errorf("invalid body: %w", err)
	}
	if err := n.checkSettlement(blk); err != nil {
		return fmt.Errorf("invalid settlement: %w", err)
	}
	pow, err := n.pow(uint64(h.Height))
	if err != nil {
		return err
	}
	if _, err := n.chain.AddHeader(h, pow); err != nil {
		return fmt.Errorf("invalid header: %w", err)
	}
	if err := n.store.PutBlock(blk); err != nil {
		return err
	}
	if _, err := n.store.ApplyBlockDirect(blk); err != nil {
		return err
	}
	// The announcement first (propagation is time-sensitive), then the same thing a block from
	// the network does.
	//
	// One function for both paths matters here: a mining node goes through ApplyBlockDirect and
	// not through ApplyStored, so everything hanging off `onBlockApplied` would otherwise
	// silently be missing on a mining node — the tip mark update, for one.
	n.announceBlock(blk, "")
	n.onBlockApplied(blk)
	return nil
}

// requestBodies asks for the bodies of blocks whose headers we already have verified.
func (n *Node) requestBodies(conn *p2p.Conn, peer *p2p.Peer, ids [][32]byte) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > p2p.MaxGetDataItems {
		ids = ids[:p2p.MaxGetDataItems]
	}
	payload, err := p2p.EncodeGetData(p2p.BlockItems(ids))
	if err != nil {
		return err
	}
	if err := conn.Send(&p2p.Message{Command: p2p.CmdGetData, Payload: payload}); err != nil {
		return err
	}
	for range ids {
		msg, err := conn.Recv()
		if err != nil {
			return err
		}
		if msg.Command != p2p.CmdBlock {
			continue
		}
		blk, err := p2p.DecodeBlock(msg.Payload)
		if err != nil {
			n.peers.Misbehaving(peer, p2p.MisbehaviorBadMessage)
			return err
		}
		if err := n.acceptBlock(peer, blk); err != nil {
			return err
		}
	}
	return nil
}

// SyncFrom connects to a peer and pulls the headers from it.
func (n *Node) SyncFrom(hostport string) error {
	conn, err := p2p.Dial(hostport, n.params.Magic)
	if err != nil {
		return err
	}
	defer conn.Close()

	remote, err := conn.Handshake(n.version(), n.nonce, true)
	if err != nil {
		return err
	}
	addr := conn.RemoteAddress()
	peer := p2p.NewPeer(addr, p2p.PeerOutboundFull)
	if err := peer.CompleteHandshake(remote, n.nonce); err != nil {
		return err
	}
	if _, err := n.peers.Add(peer); err != nil {
		return err
	}
	defer n.peers.Remove(addr)
	n.addConn(addr, conn, peer)
	defer n.removeConn(addr)
	n.addrs.Add(addr, addr.IP)
	n.addrs.Good(addr)
	log.Printf("connected to %s (%s, height %d)", hostport, remote.UserAgent, remote.StartHeight)

	// We pull headers while the peer keeps sending new ones.
	for round := 0; round < 1000; round++ {
		payload, err := p2p.EncodeGetHeaders(n.chain.Locator())
		if err != nil {
			return err
		}
		if err := conn.Send(&p2p.Message{Command: p2p.CmdGetHeaders, Payload: payload}); err != nil {
			return err
		}
		msg, err := conn.Recv()
		if err != nil {
			return err
		}
		if msg.Command != p2p.CmdHeaders {
			continue
		}
		headers, err := p2p.DecodeHeaders(msg.Payload)
		if err != nil {
			return err
		}
		if len(headers) == 0 {
			_, height, id := n.chain.Tip()
			log.Printf("synced: height %d, tip %x, UTXO %d outputs",
				height, id[:6], n.coinCount())
			return nil
		}
		if _, err := n.acceptHeaders(peer, headers); err != nil {
			return err
		}
		// Bodies are only requested after the headers are verified — those are now a verified
		// manifest, so bodies can be pulled from several sources in parallel (the sync design).
		_, tipHeight, _ := n.chain.Tip()
		err = n.bulkFetch(context.Background(), conn, peer.Addr.String(), tipHeight)
		if errors.Is(err, blocksync.ErrNoSources) {
			// Nothing to pull from in parallel — the classic one-at-a-time path.
			ids := make([][32]byte, 0, len(headers))
			for _, h := range headers {
				id := h.ID()
				if !n.store.HasBody(id) && n.chain.Has(id) {
					ids = append(ids, id)
				}
			}
			err = n.requestBodies(conn, peer, ids)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// MineBlocks mines n blocks (regtest and development only — real mining is a separate miner).
func (nd *Node) MineBlocks(count int) error {
	for i := 0; i < count; i++ {
		_, height, tipID := nd.chain.Tip()
		lanes := nd.chain.AllowedLanesNow(time.Now())
		if len(lanes) == 0 {
			return fmt.Errorf("no lane is allowed")
		}
		// Prefer the cheapest allowed lane. Lane C is RandomX and in light mode costs ~20 ms
		// per hash, so development mining would needlessly take minutes. The rotation rule
		// forces alternation anyway, so lane C gets exercised too.
		lane := lanes[0]
		for _, l := range lanes {
			if l == mh.LaneG {
				lane = l
				break
			}
		}
		bits, err := nd.chain.NextBitsFor(lane)
		if err != nil {
			return err
		}
		var extra [32]byte
		if _, err := rand.Read(extra[:]); err != nil {
			return err
		}
		selected := nd.mempool.Select(chain.MaxBlockBytes-chain.MaxCoinbaseBytes,
			chain.MaxBlockStorageMass-chain.CoinbaseStorageMassReserve)
		nd.mu.Lock()
		minerKey := nd.minerKey
		nd.mu.Unlock()
		payouts, err := chain.CreditPayouts(height+1, nd.store)
		if err != nil {
			return err
		}
		cb := chain.BuildCoinbase(height+1, payouts, extra, nd.params)
		blk := &chain.Block{
			Header: &chain.Header{Version: nd.params.BlockVersion(height + 1), PrevBlock: tipID, Bits: bits,
				Height: uint32(height + 1), Lane: lane},
			Txs: append([]*chain.Tx{cb}, selected...),
		}
		copy(blk.Share.Payout[:], minerKey)
		// The heaviest share a block can be: a block is the only share this miner publishes, so
		// it is worth the most units it can claim (the Sharepool spec §2.2).
		blk.Share.Weight = chain.MaxShareWeight
		if key := nd.FinalityKey(); key != nil {
			blk.Share.FinalityKey = *key
		}
		settlement, err := nd.templateSettlement(tipID, height)
		if err != nil {
			return err
		}
		blk.Settlement = *settlement
		chain.SealCoinbase(blk.Txs, nd.store.CreditsRoot())
		pow, err := nd.pow(height + 1)
		if err != nil {
			return err
		}
		blk.Seal()
		h := blk.Header
		// The clock, or the earliest time the chain allows when blocks come faster than it. The
		// bound is never above the parent's time, so mining hundreds of blocks within a second
		// on regtest does not walk the chain into the future and past the allowed drift.
		h.Time = uint32(time.Now().Unix())
		if low := nd.chain.MinTime(); h.Time < low {
			h.Time = low
		}

		mined := false
		for nonce := uint64(0); nonce < 5_000_000; nonce++ {
			h.Nonce = nonce
			ok, err := pow.VerifyPoW(h)
			if err != nil {
				return err
			}
			if ok {
				if err := nd.SubmitBlock(blk); err != nil {
					return fmt.Errorf("our own block did not pass: %w", err)
				}
				mined = true
				break
			}
		}
		if !mined {
			return fmt.Errorf("no nonce found for lane %s", lane)
		}
		_, newHeight, id := nd.chain.Tip()
		log.Printf("mined block %d in lane %s (%x), UTXO: %d outputs",
			newHeight, lane, id[:6], nd.coinCount())
	}
	return nil
}

// coinCount is the number of coins in the UTXO set. It only logs an error — this is a number for the log, not for consensus.
func (n *Node) coinCount() int {
	c, err := n.store.CoinCount()
	if err != nil {
		log.Printf("⚠ the UTXO set cannot be counted: %v", err)
		return -1
	}
	return c
}

// markBanListDirty is called after every ban. The write itself is done by writeBanListLoop —
// during a storm of bans, rewriting the file on each one would cost more than the ban itself.
func (n *Node) markBanListDirty() {
	n.mu.Lock()
	n.banListDirty = true
	n.mu.Unlock()
}

// writeBanListLoop writes the active bans into the file for the firewall (`-banlist`), at most
// once per 5 seconds, and once a minute even without a new ban — so expired bans disappear
// from the file.
//
// The write is atomic (temporary file + rename): the firewall never reads a half-written file.
// Format: one IPv4 address or IPv6 /64 network per line — straight for `nft add element`.
func (n *Node) writeBanListLoop(stop <-chan struct{}) {
	if n.banListPath == "" {
		return
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var sinceFull int
	write := func() {
		entries := n.peers.ActiveBans()
		tmp := n.banListPath + ".tmp"
		data := []byte(strings.Join(entries, "\n"))
		if len(entries) > 0 {
			data = append(data, '\n')
		}
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			log.Printf("⚠ the ban list cannot be written: %v", err)
			return
		}
		if err := os.Rename(tmp, n.banListPath); err != nil {
			log.Printf("⚠ the ban list cannot be moved into place: %v", err)
		}
	}
	write()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			sinceFull++
			n.mu.Lock()
			dirty := n.banListDirty
			n.banListDirty = false
			n.mu.Unlock()
			if dirty || sinceFull >= 12 {
				sinceFull = 0
				write()
			}
		}
	}
}

// SetBanListPath sets the file with bans for the firewall.
// AllowPrivateNet allows connecting to private ranges. Used for a closed deployment of your
// own; on the public network it is a path to an abuse report from the provider.
func (n *Node) AllowPrivateNet() { n.addrs.AllowPrivate = true }

func (n *Node) SetBanListPath(path string) { n.banListPath = path }
