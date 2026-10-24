package stratum

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"powmachina/internal/address"
	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/work"
)

// Backend is the node the stratum server serves.
type Backend interface {
	Params() chain.NetParams
	WorkTemplate(lane mh.Lane) (*work.Template, error)
	TipID() [32]byte
	// PoWHash computes a header's PoW hash (verifying a share).
	PoWHash(h *chain.Header) ([32]byte, error)
	SubmitBlock(b *chain.Block) error
	// SubmitStratumShare passes a device's Sharepool share to the node (the Sharepool spec §2),
	// with the device's IP address — the node counts the country for the SharePool map.
	SubmitStratumShare(s *chain.Share, ip string) (bool, error)
	// AttestDue says which block a finality key should sign now: the block at the finality depth
	// below the tip, when the key has a seat in its quorum. ok = false: nothing to sign.
	AttestDue(key [32]byte) (height uint64, block [32]byte, ok bool)
	// SubmitAttestation takes a miner's signature of a block (verified, filed, relayed).
	SubmitAttestation(a chain.Attestation) error
}

// ServerOptions are the server's settings.
type ServerOptions struct {
	// Payout: when it is set, every block pays here and the login is only a device name (a single-owner
	// farm). Otherwise the address from the login is paid — whoever finds a block gets it.
	Payout      []byte
	MaxConns    int           // 0 = 1024
	MaxPerIP    int           // 0 = 64
	ShareTarget time.Duration // the target interval of shares per connection and lane; 0 = 15 s
	Log         *log.Logger
}

// Server is the node's stratum server.
type Server struct {
	b   Backend
	opt ServerOptions

	// ln is under mu: Serve sets it, Close closes it, and those are two goroutines.
	mu       sync.Mutex
	ln       net.Listener
	conns    map[*sconn]bool
	perIP    map[string]int
	tip      [32]byte
	seq      atomic.Uint64
	closed   chan struct{}
	closeOne sync.Once
	kick     chan struct{}
}

// NewServer creates the server.
func NewServer(b Backend, opt ServerOptions) *Server {
	if opt.MaxConns <= 0 {
		opt.MaxConns = 1024
	}
	if opt.MaxPerIP <= 0 {
		opt.MaxPerIP = 64
	}
	if opt.ShareTarget <= 0 {
		opt.ShareTarget = 15 * time.Second
	}
	if opt.Log == nil {
		opt.Log = log.Default()
	}
	return &Server{b: b, opt: opt, conns: map[*sconn]bool{}, perIP: map[string]int{}, closed: make(chan struct{}),
		kick: make(chan struct{}, 1)}
}

// Serve accepts connections until the listener is closed.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	go s.watch()
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return nil
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
		ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())
		s.mu.Lock()
		if len(s.conns) >= s.opt.MaxConns || s.perIP[ip] >= s.opt.MaxPerIP {
			s.mu.Unlock()
			c.Close()
			continue
		}
		sc := newSconn(s, c, ip)
		s.conns[sc] = true
		s.perIP[ip]++
		s.mu.Unlock()
		go sc.serve()
	}
}

// Close stops the server. It is idempotent and safe from several goroutines — both the signal handler
// and the test cleanup call it.
func (s *Server) Close() {
	s.closeOne.Do(func() {
		close(s.closed)
		s.mu.Lock()
		if s.ln != nil {
			s.ln.Close()
		}
		for c := range s.conns {
			c.conn.Close()
		}
		s.mu.Unlock()
	})
}

func (s *Server) drop(c *sconn) {
	s.mu.Lock()
	if s.conns[c] {
		delete(s.conns, c)
		if s.perIP[c.ip]--; s.perIP[c.ip] <= 0 {
			delete(s.perIP, c.ip)
		}
	}
	s.mu.Unlock()
}

func (s *Server) all() []*sconn {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*sconn, 0, len(s.conns))
	for c := range s.conns {
		out = append(out, c)
	}
	return out
}

// TipChanged tells the server the node has a new tip, so the devices get new jobs now and not at
// the next check. Every moment a device hashes on the old tip is a block that comes out stale.
func (s *Server) TipChanged() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// watch sends new jobs on a new tip (told by TipChanged, and checked every 500 ms for a backend
// that does not tell) and every 30 s (new transactions in the mempool).
func (s *Server) watch() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
		case <-s.kick:
		}
		tip := s.b.TipID()
		s.mu.Lock()
		changed := tip != s.tip
		s.tip = tip
		s.mu.Unlock()
		if changed || time.Since(last) > 30*time.Second {
			last = time.Now()
			for _, c := range s.all() {
				c.pushJobs()
			}
		}
		if changed {
			for _, c := range s.all() {
				c.askAttest()
			}
		}
	}
}

// ------------------------------------------------------------------ the statistics

// WorkerStat is a device connected to stratum.
type WorkerStat struct {
	Address   string   `json:"address"`
	Worker    string   `json:"worker"`
	Remote    string   `json:"remote"`
	Agent     string   `json:"agent"`
	Since     int64    `json:"since"`
	Lanes     []string `json:"lanes"`
	Accepted  uint64   `json:"accepted"`
	Rejected  uint64   `json:"rejected"`
	Blocks    uint64   `json:"blocks"`
	HashrateC float64  `json:"hashrate_c"` // an estimate from the shares, H/s (for display only)
	HashrateG float64  `json:"hashrate_g"`
}

// Stats returns the connected devices.
func (s *Server) Stats() []WorkerStat {
	var out []WorkerStat
	for _, c := range s.all() {
		out = append(out, c.stat())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since < out[j].Since })
	return out
}

// ------------------------------------------------------------------ the connection

var two256 = new(big.Int).Lsh(big.NewInt(1), 256)

type laneState struct {
	target *big.Int // the share target (larger = easier)
	jobs   []*issued
	shares int
	since  time.Time
	work   *big.Int // the sum of the expected hashes from the accepted shares (a hashrate estimate)
	workAt time.Time
	paused bool
}

// issued is a job a connection was given, with the nonces the connection has sent for it.
//
// The nonces live exactly as long as the job: while the job can be submitted to, every share sent
// for it is remembered, and once the job is dropped a share for it is stale whatever its nonce. So
// there is no moment at which a share already counted can be counted again. The set needs no cap of
// its own — a nonce is recorded only after the pace limit let the share through, and a job is
// replaced within minutes.
type issued struct {
	*work.Job
	seen map[uint64]struct{}
}

type sconn struct {
	s    *Server
	conn net.Conn
	ip   string
	enc  *json.Encoder
	wmu  sync.Mutex

	mu       sync.Mutex
	loggedIn bool
	payout   []byte
	addr     string
	worker   string
	agent    string
	since    time.Time
	lanes    map[mh.Lane]*laneState
	accepted uint64
	rejected uint64
	blocks   uint64
	invalid  int
	tokens   float64
	tokensAt time.Time
	// finKey is the miner's own finality key from the login (nil = the node's key);
	// asked is the last height the miner was asked to sign.
	finKey *[32]byte
	asked  uint64
}

func newSconn(s *Server, c net.Conn, ip string) *sconn {
	return &sconn{s: s, conn: c, ip: ip, enc: json.NewEncoder(c), since: time.Now(),
		lanes: map[mh.Lane]*laneState{}, tokens: 40, tokensAt: time.Now()}
}

func (c *sconn) write(v any) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if err := c.enc.Encode(v); err != nil {
		c.conn.Close()
	}
}

func (c *sconn) reply(id uint64, result any, e *Error) {
	r := Response{ID: id, Error: e}
	if e == nil {
		raw, _ := json.Marshal(result)
		r.Result = raw
	}
	c.write(r)
}

func (c *sconn) notify(method string, params any) {
	raw, _ := json.Marshal(params)
	c.write(Request{Method: method, Params: raw})
}

func (c *sconn) serve() {
	defer c.s.drop(c)
	defer c.conn.Close()
	r := bufio.NewReaderSize(c.conn, MaxLine)
	deadline := 30 * time.Second // na login
	for {
		_ = c.conn.SetReadDeadline(time.Now().Add(deadline))
		line, err := r.ReadSlice('\n')
		if err != nil {
			return // including a line longer than MaxLine (bufio.ErrBufferFull)
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil || req.ID == nil {
			return
		}
		switch req.Method {
		case "login":
			if e := c.login(req.Params); e != nil {
				c.reply(*req.ID, nil, e)
				return
			}
			c.reply(*req.ID, LoginResult{ID: c.sessionID(), Network: c.s.b.Params().Name}, nil)
			c.pushJobs()
			c.askAttest()
			deadline = 10 * time.Minute
		case "submit":
			res, e := c.submit(req.Params)
			c.reply(*req.ID, res, e)
			c.mu.Lock()
			bad := c.invalid > 50
			c.mu.Unlock()
			if bad {
				c.s.opt.Log.Printf("stratum: %s disconnected — too many invalid shares", c.ip)
				return
			}
		case "attestation":
			e := c.attestation(req.Params)
			if e != nil {
				c.reply(*req.ID, nil, e)
			} else {
				c.reply(*req.ID, map[string]string{"status": "OK"}, nil)
			}
		case "keepalived":
			c.reply(*req.ID, map[string]string{"status": "KEEPALIVED"}, nil)
		case "hashrate":
			// What the miner says about itself. The node has its own estimate from the shares
			// (getstratuminfo) and keeps to it; the claim is acknowledged so the miner is not told
			// the method is unknown.
			c.reply(*req.ID, map[string]string{"status": "OK"}, nil)
		default:
			c.reply(*req.ID, nil, &Error{ErrCodeInvalid, "unknown method " + req.Method})
		}
	}
}

func (c *sconn) sessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (c *sconn) login(raw json.RawMessage) *Error {
	var p LoginParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return &Error{ErrCodeInvalid, "bad login params"}
	}
	params := c.s.b.Params()
	login, worker, _ := strings.Cut(p.Login, ".")
	var payout []byte
	if c.s.opt.Payout != nil {
		payout = c.s.opt.Payout
		if worker == "" {
			worker = login // with a fixed address the login is just the worker's name
		}
	} else {
		v, prog, err := address.Decode(params.Bech32HRP, login)
		if err != nil || v != chain.ScriptVersionBasic || len(prog) != chain.PubkeyBytes {
			return &Error{ErrCodeBadLogin, fmt.Sprintf("login must be a %s address (%s…), optionally .worker", params.Name, params.Bech32HRP+"1")}
		}
		payout = prog
	}
	if len(worker) > 32 {
		worker = worker[:32]
	}
	var finKey *[32]byte
	if p.FinalityKey != "" {
		raw, err := hex.DecodeString(p.FinalityKey)
		if err != nil || len(raw) != 32 {
			return &Error{ErrCodeInvalid, "finality_key must be 32 bytes hex"}
		}
		finKey = new([32]byte)
		copy(finKey[:], raw)
	}
	lanes := map[mh.Lane]*laneState{}
	for _, l := range p.Lanes {
		if id, ok := ParseLane(l); ok {
			lanes[id] = &laneState{since: time.Now(), work: new(big.Int), workAt: time.Now()}
		}
	}
	if len(lanes) == 0 {
		return &Error{ErrCodeInvalid, `lanes must contain "C" and/or "G"`}
	}
	c.mu.Lock()
	c.loggedIn, c.payout, c.addr, c.worker, c.lanes, c.finKey = true, payout, login, worker, lanes, finKey
	if len(p.Agent) > 64 {
		p.Agent = p.Agent[:64]
	}
	c.agent = p.Agent
	c.mu.Unlock()
	return nil
}

// pushJobs produces and sends a new job for each of the client's lanes.
func (c *sconn) pushJobs() {
	c.mu.Lock()
	if !c.loggedIn {
		c.mu.Unlock()
		return
	}
	ids := make([]mh.Lane, 0, len(c.lanes))
	for id := range c.lanes {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	for _, id := range ids {
		c.pushJob(id)
	}
}

func (c *sconn) pushJob(id mh.Lane) {
	t, err := c.s.b.WorkTemplate(id)
	var block [32]byte
	var easiest *big.Int
	if err == nil {
		var bits uint32
		if bits, err = work.ParseBits(t.Bits); err == nil {
			block, err = chain.CompactToTarget(bits)
		}
		if err == nil {
			easiest = easiestTarget(t, bits, c.s.b.Params())
		}
	}
	c.mu.Lock()
	st := c.lanes[id]
	payout := c.payout
	finKey := c.finKey
	blockN := new(big.Int).SetBytes(block[:])
	if err == nil {
		if st.target == nil {
			// The start: 256× easier than a block (capped at the easiest target). Vardiff evens it out within minutes.
			st.target = new(big.Int).Lsh(blockN, 8)
		}
		st.target = clampTarget(st.target, blockN, easiest)
	}
	var target [32]byte
	if st.target != nil {
		st.target.FillBytes(target[:])
	}
	c.mu.Unlock()
	var j *work.Job
	if err == nil {
		// The miner's own key replaces the node's in the share commitment — on a network with
		// finality only (the node's template carries a key then).
		if finKey != nil && t.FinalityKey != "" {
			own := *t
			own.FinalityKey = hex.EncodeToString(finKey[:])
			t = &own
		}
		j, err = work.BuildShares(t, payout, deviceWeight(t, target), c.s.b.Params())
	}
	c.mu.Lock()
	if errors.Is(err, work.ErrTipNotApplied) && len(st.jobs) > 0 {
		// The new tip's body is on its way: the device keeps the job on the block below (the last
		// one the node verified) and gets a new one when the tip is applied.
		c.mu.Unlock()
		return
	}
	if err != nil {
		wasPaused := st.paused
		st.paused = true
		st.jobs = nil
		c.mu.Unlock()
		if !wasPaused {
			c.notify("pause", PauseParams{Lane: id.String(), Reason: err.Error()})
		}
		return
	}
	st.paused = false
	j.ID = fmt.Sprintf("%s%x", id, c.s.seq.Add(1))
	j.Target = target
	st.jobs = append(st.jobs, &issued{Job: j, seen: map[uint64]struct{}{}})
	if len(st.jobs) > 8 {
		st.jobs = st.jobs[len(st.jobs)-8:]
	}
	c.mu.Unlock()
	c.notify("job", JobParamsFrom(j))
}

// askAttest asks a miner with its own finality key to sign the block at the finality depth, once
// per height, when the key has a seat in that block's quorum.
func (c *sconn) askAttest() {
	c.mu.Lock()
	key := c.finKey
	c.mu.Unlock()
	if key == nil {
		return
	}
	h, id, ok := c.s.b.AttestDue(*key)
	if !ok {
		return
	}
	c.mu.Lock()
	if h <= c.asked {
		c.mu.Unlock()
		return
	}
	c.asked = h
	c.mu.Unlock()
	c.notify("attest", AttestParams{Height: h, Block: hex.EncodeToString(id[:])})
}

// attestation takes the miner's signature. It has to be by the key the miner logged in with: the
// stratum port is not a way into the network for anybody's attestations.
func (c *sconn) attestation(raw json.RawMessage) *Error {
	var p AttestationParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return c.badAttestation("bad attestation params")
	}
	c.mu.Lock()
	key := c.finKey
	c.mu.Unlock()
	if key == nil {
		return c.badAttestation("log in with a finality_key first")
	}
	id, err1 := hex.DecodeString(p.Block)
	sig, err2 := hex.DecodeString(p.Signature)
	if err1 != nil || err2 != nil || len(id) != 32 || len(sig) != 64 {
		return c.badAttestation("block must be 32 bytes and signature 64 bytes hex")
	}
	a := chain.Attestation{Height: p.Height, Key: *key, Signature: sig}
	copy(a.BlockID[:], id)
	if err := a.Verify(); err != nil {
		return c.badAttestation("the signature does not verify")
	}
	if err := c.s.b.SubmitAttestation(a); err != nil {
		return &Error{ErrCodeInvalid, err.Error()}
	}
	return nil
}

// badAttestation counts a malformed attestation towards the disconnect, not towards the shares.
func (c *sconn) badAttestation(msg string) *Error {
	c.mu.Lock()
	c.invalid++
	c.mu.Unlock()
	return &Error{ErrCodeInvalid, msg}
}

// deviceWeight is the Sharepool weight of a device's job: the heaviest whose target every share at
// the device's stratum `target` still meets, so that each of them is a Sharepool share and counts
// for as much as it can. A device on an easier target than the lightest share gets weight 0, and
// only its shares that meet it go on.
func deviceWeight(t *work.Template, target [32]byte) uint8 {
	bits, err := work.ParseBits(t.Bits)
	if err != nil {
		return chain.MaxShareWeight
	}
	for w := chain.MaxShareWeight; w > 0; w-- {
		if st, err := chain.ShareTarget(bits, uint8(w)); err == nil && mh.MeetsTarget(&target, &st) {
			return uint8(w)
		}
	}
	return 0
}

// easiestTarget is the easiest share target a device may get. On a node that takes shares it is
// the lightest Sharepool share (weight 0, SharesPerLaneBlock times easier than the block): a stratum
// share any easier would be paid by nothing, and one any harder is not needed. A node that takes
// no shares keeps the network's easiest block target. The difference shows on a chain whose
// difficulty sits near its floor — a testnet, a young mainnet: there that floor is only a few times
// easier than a block, and a small CPU would find a share every few minutes instead of every 15 s.
func easiestTarget(t *work.Template, bits uint32, p chain.NetParams) *big.Int {
	if t.TakesShares {
		if st, err := chain.ShareTarget(bits, 0); err == nil {
			return new(big.Int).SetBytes(st[:])
		}
	}
	if limit, err := chain.CompactToTarget(p.PowLimitBits); err == nil {
		return new(big.Int).SetBytes(limit[:])
	}
	return new(big.Int).Sub(two256, big.NewInt(1))
}

// clampTarget keeps the share target between the block's target (never harder) and the easiest
// target (easiestTarget).
func clampTarget(t, block, easiest *big.Int) *big.Int {
	out := new(big.Int).Set(t)
	if easiest != nil && out.Cmp(easiest) > 0 {
		out.Set(easiest)
	}
	if out.Cmp(block) < 0 {
		out.Set(block)
	}
	return out
}

func (c *sconn) submit(raw json.RawMessage) (*SubmitResult, *Error) {
	var p SubmitParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, c.bad(ErrCodeInvalid, "bad submit params")
	}
	nonce, err := ParseNonce(p.Nonce)
	if err != nil {
		return nil, c.bad(ErrCodeInvalid, err.Error())
	}
	c.mu.Lock()
	if !c.loggedIn {
		c.mu.Unlock()
		return nil, &Error{ErrCodeNotLoggedIn, "login first"}
	}
	// The pace: verifying a share costs the node a PoW computation (RandomX light ~20 ms) — at most 4 per
	// second on average (2 lanes × 2), 40 at once. Vardiff keeps the ordinary pace at ~1 per 15 s per lane.
	now := time.Now()
	c.tokens = minF(40, c.tokens+now.Sub(c.tokensAt).Seconds()*4)
	c.tokensAt = now
	if c.tokens < 1 {
		// It is not an invalid share — a fast honest miner at a low network difficulty hits this too.
		// A disconnect is only risked for invalid shares.
		c.rejected++
		c.mu.Unlock()
		return nil, &Error{ErrCodeRateLimit, "too many shares — difficulty will rise"}
	}
	c.tokens--
	var job *issued
	var st *laneState
	for _, s := range c.lanes {
		for _, j := range s.jobs {
			if j.ID == p.JobID {
				job, st = j, s
			}
		}
	}
	if job == nil {
		c.rejected++
		c.mu.Unlock()
		return nil, &Error{ErrCodeStale, "stale job"}
	}
	// The nonce is compared as a number: the same nonce can be written in more than one way (the hex
	// digits in either case) and it is one share however it is written.
	if _, dup := job.seen[nonce]; dup {
		c.invalid++
		c.rejected++
		c.mu.Unlock()
		return nil, &Error{ErrCodeDuplicate, "duplicate share"}
	}
	job.seen[nonce] = struct{}{}
	c.mu.Unlock()

	h := job.Header
	h.Nonce = nonce
	hash, err := c.s.b.PoWHash(&h)
	if err != nil {
		return nil, &Error{ErrCodeStale, err.Error()}
	}
	if !mh.MeetsTarget(&hash, &job.Target) {
		c.mu.Lock()
		c.invalid++
		c.rejected++
		c.mu.Unlock()
		return nil, &Error{ErrCodeLowDiff, "low difficulty share"}
	}
	isBlock := false
	// A job for a lane the rotation rule blocks (NoBlock) has no block to find: a hash at the
	// block's target is a share like any other.
	if !job.NoBlock && mh.MeetsTarget(&hash, &job.BlockTarget) {
		if err := c.s.b.SubmitBlock(job.Block(nonce)); err != nil {
			c.s.opt.Log.Printf("stratum: block from %s/%s rejected by node: %v", c.addr, c.worker, err)
		} else {
			isBlock = true
			c.s.opt.Log.Printf("stratum: BLOCK FOUND by %s (%s), lane %s, height %d", c.worker, c.addr, job.Lane, job.Height)
		}
	}
	// A block is its own share; any other that meets the job's Sharepool target goes to the node.
	if !isBlock && job.Shares && mh.MeetsTarget(&hash, &job.ShareTarget) {
		if _, err := c.s.b.SubmitStratumShare(job.ShareAt(nonce), c.ip); err != nil {
			c.s.opt.Log.Printf("stratum: share from %s/%s refused by node: %v", c.addr, c.worker, err)
		}
	}
	c.mu.Lock()
	c.accepted++
	if isBlock {
		c.blocks++
	}
	st.shares++
	st.work.Add(st.work, new(big.Int).Div(two256, new(big.Int).Add(new(big.Int).SetBytes(job.Target[:]), big.NewInt(1))))
	retarget := c.vardiffLocked(st)
	c.mu.Unlock()
	if retarget {
		c.pushJob(job.Lane)
	}
	return &SubmitResult{Status: "OK", Block: isBlock}, nil
}

// vardiffLocked adjusts the share target so that ~1 share arrives per ShareTarget. After 30 s or 20
// shares, in a step of at most ×4 / ÷4.
func (c *sconn) vardiffLocked(st *laneState) bool {
	el := time.Since(st.since)
	if el < 30*time.Second && st.shares < 20 {
		return false
	}
	want := c.s.opt.ShareTarget.Seconds()
	// the factor = the actual interval / the desired interval (>1 = the shares come slowly → an easier target)
	actual := el.Seconds() / float64(max(st.shares, 1)) // only for steering the difficulty, not consensus
	f := actual / want
	f = maxF(0.25, minF(4, f))
	num := big.NewInt(int64(f * 1000))
	st.target = new(big.Int).Div(new(big.Int).Mul(st.target, num), big.NewInt(1000))
	st.shares, st.since = 0, time.Now()
	return f < 0.9 || f > 1.1
}

func (c *sconn) bad(code int, msg string) *Error {
	c.mu.Lock()
	c.invalid++
	c.rejected++
	c.mu.Unlock()
	return &Error{code, msg}
}

func (c *sconn) stat() WorkerStat {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := WorkerStat{Address: c.addr, Worker: c.worker, Remote: c.ip, Agent: c.agent, Since: c.since.Unix(),
		Accepted: c.accepted, Rejected: c.rejected, Blocks: c.blocks}
	for id, st := range c.lanes {
		w.Lanes = append(w.Lanes, id.String())
		secs := time.Since(st.workAt).Seconds()
		if secs > 0 {
			hs, _ := new(big.Float).Quo(new(big.Float).SetInt(st.work), big.NewFloat(secs)).Float64()
			if id == mh.LaneC {
				w.HashrateC = hs
			} else {
				w.HashrateG = hs
			}
		}
	}
	sort.Strings(w.Lanes)
	return w
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
