package main

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/store"
)

// A node that goes back and forth between branches has to end up in exactly the state a node
// that synced only the winning branch from scratch has: the coins, the credit state with its
// indexes, the address and transaction indexes. And the set in memory, which reverts its own
// blocks, has to agree with both.
//
// The branches are random: transactions that spend coins created in the same block, shares, pays
// out of the Sharepool, forks off any block of any branch. The node on disk takes them in random
// order, sometimes a branch that loses, and is restarted now and then. After every step it is
// compared with a clean sync of its active chain. POWM_REORG_SEEDS runs more seeds than the default.
func TestReorgsMatchACleanSync(t *testing.T) {
	seeds := 2
	if testing.Short() || raceEnabled {
		seeds = 1
	}
	if s, err := strconv.Atoi(os.Getenv("POWM_REORG_SEEDS")); err == nil && s > 0 {
		seeds = s
	}
	for seed := range uint64(seeds) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { reorgScenario(t, seed) })
	}
}

// reorgKeys are the wallets the scenario pays between; each starts with a few coins.
type reorgKey struct {
	sk *[32]byte
	pk []byte
}

type ownedCoin struct {
	coin chain.Coin
	key  int
}

// reorgWorld is the tree of branches. Each branch is a node of its own that mines on its tip; a
// fork is a new node that took a branch up to some height.
type reorgWorld struct {
	t        *testing.T
	rng      *rand.Rand
	keys     []reorgKey
	funding  map[chain.Outpoint]ownedCoin
	branches []*Node
	// wallet is what the keys own after each block, by the block's id.
	wallet map[[32]byte]map[chain.Outpoint]ownedCoin
	txids  map[[32]byte]bool // every transaction mined anywhere
	nonce  uint64
	// paid counts the Sharepool's payouts in the blocks mined; inBlock the spends of an output of
	// the same block.
	paid, inBlock int
}

func newReorgWorld(t *testing.T, seed uint64) *reorgWorld {
	w := &reorgWorld{t: t, rng: rand.New(rand.NewPCG(seed, 0x5E0)), funding: map[chain.Outpoint]ownedCoin{},
		wallet: map[[32]byte]map[chain.Outpoint]ownedCoin{}, txids: map[[32]byte]bool{}}
	for k := range 6 {
		sk := new([32]byte)
		for i := range sk {
			sk[i] = byte(k*31 + i + 1)
		}
		pk, err := mh.SchnorrPubkey(sk)
		if err != nil {
			t.Fatal(err)
		}
		w.keys = append(w.keys, reorgKey{sk, pk[:]})
		for j := range 3 {
			op := chain.Outpoint{TxID: [32]byte{0xF0, byte(k), byte(j)}}
			w.funding[op] = ownedCoin{chain.Coin{Value: 10 * chain.POWM, Script: pk[:]}, k}
		}
	}
	w.wallet[chain.RegtestGenesis().ID()] = w.funding
	return w
}

// fund gives a node the coins every node of the scenario starts from.
func (w *reorgWorld) fund(n *Node) {
	for op, c := range w.funding {
		if err := n.store.AddCoin(op, c.coin); err != nil {
			w.t.Fatal(err)
		}
	}
}

// branch starts a new branch: a node that has `from` up to `height`, mining with its own key.
func (w *reorgWorld) branch(from *Node, height uint64) *Node {
	n := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	w.t.Cleanup(n.Close)
	w.fund(n)
	n.minerKey = bytes.Repeat([]byte{0xC0 + byte(len(w.branches))}, 32)
	if from != nil && height > 0 {
		feedBranch(w.t, n, from, 1, height)
	}
	w.branches = append(w.branches, n)
	return n
}

// mine adds a block to a branch: a few transactions, some of them spending an output of the one
// before in the same block, and a few shares.
func (w *reorgWorld) mine(n *Node) {
	t := w.t
	_, height, tipID := n.chain.Tip()
	owned := maps(w.wallet[tipID])
	var txs []*chain.Tx
	var last []chain.Outpoint
	for range w.rng.IntN(4) {
		var op chain.Outpoint
		if len(last) > 0 && w.rng.IntN(2) == 0 {
			op = last[w.rng.IntN(len(last))] // an output of this block: the chain inside one block
			w.inBlock++
		} else {
			ops := sortedOutpoints(owned)
			if len(ops) == 0 {
				break
			}
			op = ops[w.rng.IntN(len(ops))]
		}
		in := owned[op]
		delete(owned, op)
		value := in.coin.Value - 10_000
		outs := []chain.TxOut{}
		to := []int{w.rng.IntN(len(w.keys))}
		if w.rng.IntN(2) == 0 && value > 2*chain.POWM {
			to = append(to, w.rng.IntN(len(w.keys)))
		}
		for i, k := range to {
			v := value / int64(len(to))
			if i == 0 {
				v += value % int64(len(to))
			}
			outs = append(outs, chain.TxOut{Value: v, Script: w.keys[k].pk})
		}
		tx := &chain.Tx{Version: 1, Inputs: []chain.TxIn{{PrevTxID: op.TxID, PrevIndex: op.Index}}, Outputs: outs}
		sig, err := chain.SignInput(tx, 0, []chain.Coin{in.coin}, w.keys[in.key].sk, chain.RegTest.At(height+1))
		if err != nil {
			t.Fatal(err)
		}
		tx.Witness = []chain.Witness{{Items: [][]byte{sig}}}
		txs = append(txs, tx)
		txid := tx.TxID()
		w.txids[txid] = true
		last = last[:0]
		for i, out := range outs {
			o := chain.Outpoint{TxID: txid, Index: uint32(i)}
			owned[o] = ownedCoin{chain.Coin{Value: out.Value, Script: out.Script, Height: height + 1}, to[i]}
			last = append(last, o)
		}
	}
	var shares []*chain.Share
	for range w.rng.IntN(4) {
		w.nonce++
		shares = append(shares, shareOn(t, n, byte(1+w.rng.IntN(4)), w.nonce))
	}
	blk := mineBlockWith(t, n, txs, shares...)
	if err := n.SubmitBlock(blk); err != nil {
		t.Fatalf("a block of the scenario: %v", err)
	}
	w.txids[blk.Txs[0].TxID()] = true
	for _, out := range blk.Txs[0].Outputs {
		if out.ScriptVersion == chain.ScriptVersionBasic && len(out.Script) == 32 &&
			!bytes.Equal(out.Script, chain.RegTest.DevFeeScriptAt(height+1)) {
			w.paid++
		}
	}
	w.wallet[blk.Header.ID()] = owned
}

// feedAll gives `dst` what it does not have of `src`'s chain: the headers, then the bodies.
func feedAll(t *testing.T, dst, src *Node) {
	t.Helper()
	_, top, _ := src.chain.Tip()
	from := uint64(1)
	for ; from <= top; from++ {
		id, _ := src.chain.BlockIDAt(from)
		if !dst.chain.Has(id) || !dst.store.HasBody(id) {
			break
		}
	}
	if from <= top {
		feedBranch(t, dst, src, from, top)
	}
}

func maps(m map[chain.Outpoint]ownedCoin) map[chain.Outpoint]ownedCoin {
	out := make(map[chain.Outpoint]ownedCoin, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sortedOutpoints(m map[chain.Outpoint]ownedCoin) []chain.Outpoint {
	out := make([]chain.Outpoint, 0, len(m))
	for op := range m {
		out = append(out, op)
	}
	slices.SortFunc(out, func(a, b chain.Outpoint) int {
		if c := bytes.Compare(a.TxID[:], b.TxID[:]); c != 0 {
			return c
		}
		return int(a.Index) - int(b.Index)
	})
	return out
}

// memFollower is the set in memory, moved along the node's active chain by its own undo.
type memFollower struct {
	u     *chain.UTXOSet
	ids   [][32]byte // the blocks applied, by height from 1
	undos []*chain.BlockUndo
}

func (m *memFollower) follow(t *testing.T, n *Node) {
	t.Helper()
	_, top, _ := n.chain.Tip()
	var want [][32]byte
	for h := uint64(1); h <= top; h++ {
		id, _ := n.chain.BlockIDAt(h)
		want = append(want, id)
	}
	same := 0
	for same < len(m.ids) && same < len(want) && m.ids[same] == want[same] {
		same++
	}
	for len(m.ids) > same {
		m.u.Undo(m.undos[len(m.undos)-1])
		m.ids, m.undos = m.ids[:len(m.ids)-1], m.undos[:len(m.undos)-1]
	}
	for _, id := range want[same:] {
		b, err := n.store.Block(id)
		if err != nil {
			t.Fatal(err)
		}
		undo, err := m.u.Apply(b)
		if err != nil {
			t.Fatalf("the set in memory refused block %d: %v", b.Header.Height, err)
		}
		m.ids, m.undos = append(m.ids, id), append(m.undos, undo)
	}
}

var reorgIndexes = store.Options{TxIndex: true, AddrIndex: true}

func reorgScenario(t *testing.T, seed uint64) {
	w := newReorgWorld(t, seed)
	dir := t.TempDir()
	open := func() *Node {
		n, _, err := OpenNodeWith(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, dir, reorgIndexes)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	subject := open()
	defer func() { subject.Close() }()
	w.fund(subject)
	mem := &memFollower{u: chain.NewUTXOSet()}
	for op, c := range w.funding {
		mem.u.AddCoin(op, c.coin)
	}

	root := w.branch(nil, 0)
	for range 3 {
		w.mine(root)
	}
	steps, switches := 0, 0
	for step := range 40 {
		switch r := w.rng.IntN(10); {
		case r < 4: // a branch grows
			b := w.branches[w.rng.IntN(len(w.branches))]
			for range 1 + w.rng.IntN(3) {
				w.mine(b)
			}
		case r < 6: // a fork off some block of some branch, a few below its tip
			from := w.branches[w.rng.IntN(len(w.branches))]
			_, top, _ := from.chain.Tip()
			at := top - min(top-1, uint64(w.rng.IntN(5)))
			b := w.branch(from, at)
			for range 1 + w.rng.IntN(4) {
				w.mine(b)
			}
		case r < 7: // a restart
			subject.Close()
			subject = open()
			continue
		}
		before := mustTipID(t, subject)
		feedAll(t, subject, w.branches[w.rng.IntN(len(w.branches))])
		if mustTipID(t, subject) != before {
			switches++
		}
		mem.follow(t, subject)
		checkAgainstCleanSync(t, w, subject, mem, step)
		steps++
	}
	_, h, _ := subject.chain.Tip()
	t.Logf("seed %d: %d checks, %d tip changes, %d branches, height %d, %d transactions (%d spending an output of their block), %d payouts",
		seed, steps, switches, len(w.branches), h, len(w.txids), w.inBlock, w.paid)
	if switches < 3 || len(w.branches) < 3 {
		t.Fatalf("the scenario reorganised too little to say much: %d tip changes, %d branches", switches, len(w.branches))
	}
}

// checkAgainstCleanSync syncs the subject's active chain into a fresh node and compares the two
// and the set in memory.
func checkAgainstCleanSync(t *testing.T, w *reorgWorld, subject *Node, mem *memFollower, step int) {
	t.Helper()
	clean, _, err := OpenNodeWith(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, t.TempDir(), reorgIndexes)
	if err != nil {
		t.Fatal(err)
	}
	defer clean.Close()
	w.fund(clean)
	feedAll(t, clean, subject)

	fail := func(what string, args ...any) {
		t.Helper()
		_, h, _ := subject.chain.Tip()
		t.Fatalf("step %d, height %d: %s", step, h, fmt.Sprintf(what, args...))
	}
	a, b := subject.store, clean.store
	at, err1 := a.Tip()
	bt, err2 := b.Tip()
	if err1 != nil || err2 != nil || at != bt || at.ID != mustTipID(t, subject) {
		fail("the applied tips differ: %+v %+v (%v %v)", at, bt, err1, err2)
	}

	coins := func(each func(func(chain.Outpoint, chain.Coin) error) error) map[chain.Outpoint]chain.Coin {
		out := map[chain.Outpoint]chain.Coin{}
		if err := each(func(op chain.Outpoint, c chain.Coin) error { out[op] = c; return nil }); err != nil {
			t.Fatal(err)
		}
		return out
	}
	ac, bc, mc := coins(a.ForEachCoin), coins(b.ForEachCoin), coins(mem.u.ForEachCoin)
	if !reflect.DeepEqual(ac, bc) {
		fail("the coins differ from a clean sync: %d vs %d", len(ac), len(bc))
	}
	if !reflect.DeepEqual(ac, mc) {
		fail("the coins differ from the set in memory: %d vs %d", len(ac), len(mc))
	}

	// The credit state and every index of it, read the way the node reads them.
	type creditReader interface {
		Pot() int64
		CreditsRoot() [32]byte
		ForEachCredit(func([32]byte, chain.Credit) error) error
		PayoutQueue(uint64, int) ([]chain.QueueEntry, bool, error)
		ExpiredQueue(uint64, int) ([]chain.QueueEntry, bool, error)
		ExpiredDust(uint64) ([]chain.QueueEntry, error)
	}
	credits := func(r creditReader) []any {
		state := map[[32]byte]chain.Credit{}
		if err := r.ForEachCredit(func(p [32]byte, c chain.Credit) error { state[p] = c; return nil }); err != nil {
			t.Fatal(err)
		}
		q, _, err1 := r.PayoutQueue(^uint64(0), -1)
		e, _, err2 := r.ExpiredQueue(^uint64(0), -1)
		d, err3 := r.ExpiredDust(^uint64(0))
		if err := errors.Join(err1, err2, err3); err != nil {
			t.Fatal(err)
		}
		return []any{r.Pot(), r.CreditsRoot(), state, q, e, d}
	}
	ar, br, mr := credits(a), credits(b), credits(mem.u)
	if !reflect.DeepEqual(ar, br) {
		fail("the credit state differs from a clean sync")
	}
	if !reflect.DeepEqual(ar, mr) {
		fail("the credit state differs from the set in memory")
	}
	if want := chain.CreditsRoot(ar[0].(int64), ar[2].(map[[32]byte]chain.Credit)); ar[1] != want {
		fail("the stored credit root is not the root of the state")
	}
	for _, s := range []*store.Store{a, b} {
		n, err := s.CreditCount()
		r, err2 := s.RecentCredits(1 << 30)
		if err != nil || err2 != nil || n != len(ar[2].(map[[32]byte]chain.Credit)) || len(r) != n {
			fail("the credit count %d and the recent list %d do not match the state (%v %v)", n, len(r), err, err2)
		}
	}
	ar1, _ := a.RecentCredits(1 << 30)
	br1, _ := b.RecentCredits(1 << 30)
	if !reflect.DeepEqual(ar1, br1) {
		fail("the credits by last share differ from a clean sync")
	}

	// The address index: every key of the scenario, every payout program and the rich list.
	scripts := [][]byte{}
	for _, k := range w.keys {
		scripts = append(scripts, k.pk)
	}
	for p := range ar[2].(map[[32]byte]chain.Credit) {
		scripts = append(scripts, bytes.Clone(p[:]))
	}
	for _, br := range w.branches {
		scripts = append(scripts, br.minerKey)
	}
	for _, sc := range scripts {
		sk := store.ScriptKeyFor(chain.ScriptVersionBasic, sc)
		as, err1 := a.AddrSummary(sk)
		bs, err2 := b.AddrSummary(sk)
		ah, err3 := a.AddrHistory(sk, 0, 1<<20)
		bh, err4 := b.AddrHistory(sk, 0, 1<<20)
		if fmt.Sprint(err1) != fmt.Sprint(err2) || !reflect.DeepEqual(as, bs) || err3 != nil || err4 != nil ||
			!reflect.DeepEqual(ah, bh) {
			fail("the address index of %x differs from a clean sync", sc[:6])
		}
	}
	arl, err1 := a.RichList(1 << 20)
	brl, err2 := b.RichList(1 << 20)
	if err1 != nil || err2 != nil || !reflect.DeepEqual(arl, brl) {
		fail("the rich list differs from a clean sync (%v %v)", err1, err2)
	}

	// The transaction index: what the active chain has is found where it is, the rest is not found.
	for txid := range w.txids {
		al, err1 := a.TxLocation(txid)
		bl, err2 := b.TxLocation(txid)
		if al != bl || errors.Is(err1, store.ErrNotFound) != errors.Is(err2, store.ErrNotFound) {
			fail("transaction %x: %+v (%v) vs a clean sync %+v (%v)", txid[:6], al, err1, bl, err2)
		}
	}
}
