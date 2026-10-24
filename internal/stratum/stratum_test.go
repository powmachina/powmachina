package stratum

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"powmachina/internal/address"
	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/work"
)

// fakeNode: a regtest chain in memory, the PoW computed with the same code as the node (light).
type fakeNode struct {
	mu     sync.Mutex
	tip    [32]byte
	height uint64
	blocks []*chain.Block
	v      *mh.Verifier
	h      *mh.Hasher
	shares []*chain.Share
	// finality: the node's key in templates, the block a key should sign, the signatures taken
	nodeKey string
	due     map[[32]byte]dueAttest
	atts    []chain.Attestation
}

type dueAttest struct {
	height uint64
	block  [32]byte
}

func newFakeNode(t *testing.T) *fakeNode {
	rx, err := mh.NewRandomX(0, false)
	if err != nil {
		t.Skipf("RandomX: %v", err)
	}
	h, err := mh.NewHasherWithRandomX(rx)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeNode{tip: chain.RegtestGenesis().ID(), v: mh.NewLightVerifier(0, mh.TestProfile), h: h}
}

func (f *fakeNode) Params() chain.NetParams { return chain.RegTest }
func (f *fakeNode) TipID() [32]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tip
}
func (f *fakeNode) WorkTemplate(lane mh.Lane) (*work.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &work.Template{Height: f.height + 1, Lane: lane.String(), Bits: fmt.Sprintf("%08x", chain.RegtestTargetBits),
		PrevBlock: hex.EncodeToString(f.tip[:]), MinTime: 1_700_000_001, CreditRoot: strings.Repeat("00", 32),
		FinalityKey: f.nodeKey}, nil
}
func (f *fakeNode) PoWHash(h *chain.Header) ([32]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pow := h.PoWInput()
	return f.h.Hash(f.v, &pow, h.Nonce, h.Lane)
}
func (f *fakeNode) SubmitBlock(b *chain.Block) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b.Header.PrevBlock != f.tip {
		return fmt.Errorf("stale")
	}
	f.blocks = append(f.blocks, b)
	f.tip, f.height = b.Header.ID(), uint64(b.Header.Height)
	return nil
}

func (f *fakeNode) SubmitStratumShare(s *chain.Share, ip string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shares = append(f.shares, s)
	return false, nil
}

func (f *fakeNode) AttestDue(key [32]byte) (uint64, [32]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.due[key]
	return d.height, d.block, ok
}

func (f *fakeNode) SubmitAttestation(a chain.Attestation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.atts = append(f.atts, a)
	return nil
}

// A device's job carries the heaviest Sharepool weight its stratum target still meets: every share
// it sends is a Sharepool share, worth as much as it can be.
func TestDeviceWeightFollowsItsTarget(t *testing.T) {
	bits := uint32(0x1d00ffff)
	tpl := &work.Template{Bits: fmt.Sprintf("%08x", bits)}
	for w := uint8(0); w <= chain.MaxShareWeight; w++ {
		st, _ := chain.ShareTarget(bits, w)
		if got := deviceWeight(tpl, st); got != w {
			t.Fatalf("a device exactly at the target of weight %d got weight %d", w, got)
		}
	}
	block, _ := chain.CompactToTarget(bits)
	if got := deviceWeight(tpl, block); got != chain.MaxShareWeight {
		t.Fatalf("a device at the block's target got weight %d", got)
	}
	var easiest [32]byte
	for i := range easiest {
		easiest[i] = 0xff
	}
	if got := deviceWeight(tpl, easiest); got != 0 {
		t.Fatalf("a device on an easier target than any share got weight %d", got)
	}
}

func startServer(t *testing.T, f *fakeNode, opt ServerOptions) (*Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if opt.Log == nil {
		opt.Log = log.New(io.Discard, "", 0)
	}
	s := NewServer(f, opt)
	go s.Serve(ln)
	t.Cleanup(s.Close)
	return s, ln.Addr().String()
}

func testAddr(seed byte) string {
	k := make([]byte, 32)
	k[0] = seed
	a, _ := address.Encode(chain.RegTest.Bech32HRP, chain.ScriptVersionBasic, k)
	return a
}

// raw: a hand-written client for testing the protocol.
type raw struct {
	c net.Conn
	r *bufio.Reader
}

func dialRaw(t *testing.T, addr string) *raw {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &raw{c, bufio.NewReader(c)}
}

func (r *raw) send(id uint64, method string, params any) {
	p, _ := json.Marshal(params)
	b, _ := json.Marshal(Request{ID: &id, Method: method, Params: p})
	r.c.Write(append(b, '\n'))
}

func (r *raw) read(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	r.c.SetReadDeadline(time.Now().Add(20 * time.Second))
	line, err := r.r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(line, &m)
	return m
}

// resp reads the next answer (it skips the `job`/`pause` notifications).
func (r *raw) resp(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	for {
		m := r.read(t)
		if m["id"] != nil {
			return m
		}
	}
}

func TestLoginRequiresAddressOfThisNetwork(t *testing.T) {
	f := newFakeNode(t)
	_, addr := startServer(t, f, ServerOptions{})
	r := dialRaw(t, addr)
	main, _ := address.Encode(chain.MainNet.Bech32HRP, 0, make([]byte, 32))
	r.send(1, "login", LoginParams{Login: main + ".rig", Lanes: []string{"C"}})
	m := r.read(t)
	var e Error
	json.Unmarshal(m["error"], &e)
	if e.Code != ErrCodeBadLogin {
		t.Fatalf("a mainnet address on regtest: %s", m["error"])
	}
}

// The whole flow: logging in, a job, a mined share = a block (regtest), a duplicate, an old job, a weak
// share.
func TestSubmitFlowAndRejections(t *testing.T) {
	f := newFakeNode(t)
	_, addr := startServer(t, f, ServerOptions{})
	r := dialRaw(t, addr)
	r.send(1, "login", LoginParams{Login: testAddr(9) + ".rig1", Agent: "test", Lanes: []string{"G"}})
	if m := r.read(t); m["error"] != nil && string(m["error"]) != "null" {
		t.Fatalf("login: %s", m["error"])
	}
	m := r.read(t)
	var jp JobParams
	json.Unmarshal(m["params"], &jp)
	job, err := JobFrom(&jp)
	if err != nil {
		t.Fatal(err)
	}
	// Find a nonce below the target and one above it.
	var good, bad uint64
	haveGood, haveBad := false, false
	for n := uint64(0); !(haveGood && haveBad); n++ {
		out, _ := f.h.Hash(f.v, &job.PoW, n, mh.LaneG)
		if mh.MeetsTarget(&out, &job.Target) {
			if !haveGood {
				good, haveGood = n, true
			}
		} else if !haveBad {
			bad, haveBad = n, true
		}
	}
	r.send(2, "submit", SubmitParams{JobID: job.ID, Nonce: FormatNonce(bad)})
	if m := r.resp(t); !strings.Contains(string(m["error"]), fmt.Sprint(ErrCodeLowDiff)) {
		t.Fatalf("a weak share: %s", m["error"])
	}
	r.send(3, "submit", SubmitParams{JobID: job.ID, Nonce: FormatNonce(good)})
	m = r.resp(t)
	var res SubmitResult
	json.Unmarshal(m["result"], &res)
	if res.Status != "OK" || !res.Block {
		t.Fatalf("share = block: %s %s", m["result"], m["error"])
	}
	if len(f.blocks) != 1 {
		t.Fatal("the block did not reach the node")
	}
	// the block's share is credited to the address from the login
	_, prog, _ := address.Decode(chain.RegTest.Bech32HRP, testAddr(9))
	if string(f.blocks[0].Share.Payout[:]) != string(prog) {
		t.Fatal("the block's share does not credit the address from the login")
	}
	r.send(4, "submit", SubmitParams{JobID: job.ID, Nonce: FormatNonce(good)})
	if m := r.resp(t); !strings.Contains(string(m["error"]), fmt.Sprint(ErrCodeDuplicate)) {
		t.Fatalf("a duplicate share: %s", m["error"])
	}
	r.send(5, "submit", SubmitParams{JobID: "Gffffff", Nonce: FormatNonce(good)})
	m = r.resp(t)
	if !strings.Contains(string(m["error"]), fmt.Sprint(ErrCodeStale)) {
		t.Fatalf("an old job: %s", m["error"])
	}
}

func TestLongLineDisconnects(t *testing.T) {
	f := newFakeNode(t)
	_, addr := startServer(t, f, ServerOptions{})
	r := dialRaw(t, addr)
	r.c.Write([]byte(strings.Repeat("x", MaxLine+100)))
	r.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := r.r.ReadByte(); err == nil {
		t.Fatal("the server did not close the connection after an over-long line")
	}
}

// A fixed payout address (a farm): the login is only the device's name.
func TestFixedPayout(t *testing.T) {
	f := newFakeNode(t)
	_, prog, _ := address.Decode(chain.RegTest.Bech32HRP, testAddr(3))
	s, addr := startServer(t, f, ServerOptions{Payout: prog})
	r := dialRaw(t, addr)
	r.send(1, "login", LoginParams{Login: "rig-7", Lanes: []string{"C", "G"}})
	if m := r.read(t); m["error"] != nil && string(m["error"]) != "null" {
		t.Fatalf("a login by worker name with a fixed address: %s", m["error"])
	}
	time.Sleep(200 * time.Millisecond)
	st := s.Stats()
	if len(st) != 1 || st[0].Worker != "rig-7" || len(st[0].Lanes) != 2 {
		t.Fatalf("the statistics: %+v", st)
	}
}
