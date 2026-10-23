package chain

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"time"

	"powmachina/internal/mh"
)

// The settlement body: the shares a block includes (the Sharepool spec §3.1, §6).
//
// A share is 187 B in full, but almost all of it repeats: every share of a miner's template has the
// same payout, finality key, body root and lane, and its parent is one of the last few blocks. So
// a block carries three tables:
//
//	keys    payout ‖ finality key                                  64 B, once per miner
//	jobs    key ‖ lane ‖ threshold ‖ weight ‖ version ‖ body root  41 B, once per template
//	shares  job ‖ depth ‖ time ‖ nonce                             15 B
//
// and a share's header is rebuilt from them and from the chain: its parent is the block `depth`
// below the one that includes it, its height follows the parent, its bits are the lane's on that
// parent. What is rebuilt is exactly the header the miner hashed, so its PoW and its id are checked
// as if the share had come whole.
//
// The time is four bytes, not a two-byte delta from the block: a chain that stood still for hours
// would otherwise have shares it cannot express.
//
// A share builds on a block of the including block's own branch. A share that built on a sibling
// that lost is not paid: it would take the sibling's header in the body to check it, and the
// shares lost that way are the few seconds a fork lasts.
//
// The encoding is canonical: every key is used by a job and every job by a share, no key and no job
// appears twice, and the shares are in the order of their ids — which also makes a share included
// twice in one block impossible to miss.

const (
	// MaxSettlementShares is four blocks' worth of shares at 100 a second: room for the shares a
	// censoring block left out to be included by the next one (the Sharepool spec §3.1).
	MaxSettlementShares = 4 * 100 * 30
	// MaxSettlementBytes caps the settlement body on the wire, apart from MaxBlockBytes.
	MaxSettlementBytes = 256 * 1024

	settleKeyBytes   = 32 + 32
	settleJobBytes   = 2 + 1 + 1 + 1 + 4 + 32
	settleShareBytes = 2 + 1 + 4 + 8

	settlementRootTag = "PowMachina/settlement/v1"
	bodyRootTag       = "PowMachina/body/v1"
)

var (
	ErrSettlement        = errors.New("chain: the settlement body is malformed")
	ErrSettlementTooBig  = errors.New("chain: the settlement body is too large")
	ErrShareSettled      = errors.New("chain: the share is already settled on this branch")
	ErrShareOrder        = errors.New("chain: the shares are not in the order of their ids")
	ErrShareDepth        = errors.New("chain: the share builds on a block outside the expiry")
	ErrShareTimeTooNew   = errors.New("chain: the share is dated after the block that includes it")
	errSettlementMissing = errors.New("chain: no settlement view")
	// ErrSettlementUnchecked: the settlement could not be checked — the PoW could not be computed.
	// It says nothing about the block, which must not be marked invalid for it.
	ErrSettlementUnchecked = errors.New("chain: the settlement could not be checked")
)

// SettleKey is a miner in the settlement body.
type SettleKey struct {
	Payout      [32]byte
	FinalityKey [32]byte
}

// SettleJob is a template the shares were mined on.
type SettleJob struct {
	Key       uint16
	Lane      mh.Lane
	Threshold uint8
	Weight    uint8
	Version   uint32
	BodyRoot  [32]byte
}

// SettleShare is one share: which job, which parent (1 = the including block's parent), and what
// the miner chose.
type SettleShare struct {
	Job   uint16
	Depth uint8
	Time  uint32
	Nonce uint64
}

// Settlement is a block's settlement body. The zero value is an empty one.
type Settlement struct {
	Keys   []SettleKey
	Jobs   []SettleJob
	Shares []SettleShare
}

// Serialize writes the settlement body: three counts of two bytes, each followed by its table.
func (s *Settlement) Serialize() []byte {
	out := make([]byte, 0, 6+len(s.Keys)*settleKeyBytes+len(s.Jobs)*settleJobBytes+
		len(s.Shares)*settleShareBytes)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(s.Keys)))
	for _, k := range s.Keys {
		out = append(out, k.Payout[:]...)
		out = append(out, k.FinalityKey[:]...)
	}
	out = binary.LittleEndian.AppendUint16(out, uint16(len(s.Jobs)))
	for _, j := range s.Jobs {
		out = binary.LittleEndian.AppendUint16(out, j.Key)
		out = append(out, byte(j.Lane), j.Threshold, j.Weight)
		out = binary.LittleEndian.AppendUint32(out, j.Version)
		out = append(out, j.BodyRoot[:]...)
	}
	out = binary.LittleEndian.AppendUint16(out, uint16(len(s.Shares)))
	for _, sh := range s.Shares {
		out = binary.LittleEndian.AppendUint16(out, sh.Job)
		out = append(out, sh.Depth)
		out = binary.LittleEndian.AppendUint32(out, sh.Time)
		out = binary.LittleEndian.AppendUint64(out, sh.Nonce)
	}
	return out
}

// DeserializeSettlement reads a settlement body from untrusted bytes and returns how many it used.
// Every count is checked against what is left before anything is allocated.
func DeserializeSettlement(b []byte) (*Settlement, int, error) {
	pos := 0
	count := func(each int) (int, error) {
		if len(b)-pos < 2 {
			return 0, fmt.Errorf("%w: cut short", ErrSettlement)
		}
		n := int(binary.LittleEndian.Uint16(b[pos:]))
		pos += 2
		if n > MaxSettlementShares || n*each > len(b)-pos {
			return 0, fmt.Errorf("%w: a table of %d does not fit", ErrSettlement, n)
		}
		return n, nil
	}
	s := &Settlement{}
	n, err := count(settleKeyBytes)
	if err != nil {
		return nil, 0, err
	}
	s.Keys = make([]SettleKey, n)
	for i := range s.Keys {
		copy(s.Keys[i].Payout[:], b[pos:pos+32])
		copy(s.Keys[i].FinalityKey[:], b[pos+32:pos+64])
		pos += settleKeyBytes
	}
	if n, err = count(settleJobBytes); err != nil {
		return nil, 0, err
	}
	s.Jobs = make([]SettleJob, n)
	for i := range s.Jobs {
		j := &s.Jobs[i]
		j.Key = binary.LittleEndian.Uint16(b[pos:])
		j.Lane, j.Threshold, j.Weight = mh.Lane(b[pos+2]), b[pos+3], b[pos+4]
		j.Version = binary.LittleEndian.Uint32(b[pos+5:])
		copy(j.BodyRoot[:], b[pos+9:pos+41])
		pos += settleJobBytes
	}
	if n, err = count(settleShareBytes); err != nil {
		return nil, 0, err
	}
	s.Shares = make([]SettleShare, n)
	for i := range s.Shares {
		sh := &s.Shares[i]
		sh.Job = binary.LittleEndian.Uint16(b[pos:])
		sh.Depth = b[pos+2]
		sh.Time = binary.LittleEndian.Uint32(b[pos+3:])
		sh.Nonce = binary.LittleEndian.Uint64(b[pos+7:])
		pos += settleShareBytes
	}
	if pos > MaxSettlementBytes {
		return nil, 0, ErrSettlementTooBig
	}
	return s, pos, nil
}

// Root is what the block's body root commits to.
func (s *Settlement) Root() [32]byte {
	return mh.TaggedHash(settlementRootTag, s.Serialize())
}

// BodyRoot is the root of what a block carries: its transactions and its settlement body. The
// shares mined on a template commit to it, so a block cannot pay other shares than the ones its
// miner chose.
func BodyRoot(txs []*Tx, s *Settlement) [32]byte {
	txRoot := TxMerkleRoot(txs)
	setRoot := s.Root()
	var buf [64]byte
	copy(buf[:32], txRoot[:])
	copy(buf[32:], setRoot[:])
	return mh.TaggedHash(bodyRootTag, buf[:])
}

// CheckStructure verifies everything that needs no chain: the indices point somewhere, the
// values are in range and nothing is there that no share uses.
func (s *Settlement) CheckStructure() error {
	if len(s.Shares) > MaxSettlementShares || len(s.Jobs) > len(s.Shares) || len(s.Keys) > len(s.Jobs) {
		return fmt.Errorf("%w: %d keys, %d jobs, %d shares", ErrSettlement, len(s.Keys), len(s.Jobs), len(s.Shares))
	}
	keys := make(map[SettleKey]bool, len(s.Keys))
	for _, k := range s.Keys {
		if keys[k] {
			return fmt.Errorf("%w: a key appears twice", ErrSettlement)
		}
		keys[k] = true
	}
	jobs := make(map[SettleJob]bool, len(s.Jobs))
	for i, j := range s.Jobs {
		if int(j.Key) >= len(s.Keys) {
			return fmt.Errorf("%w: job %d names key %d of %d", ErrSettlement, i, j.Key, len(s.Keys))
		}
		if j.Lane > mh.LaneG {
			return fmt.Errorf("%w: job %d", ErrBadLane, i)
		}
		if j.Weight > MaxShareWeight {
			return fmt.Errorf("%w: job %d", ErrShareWeight, i)
		}
		if jobs[j] {
			return fmt.Errorf("%w: job %d appears twice", ErrSettlement, i)
		}
		jobs[j] = true
	}
	// The tables are in the order of first use: a share names a new job only as the next one, a
	// job a new key only as the next one. With that and every entry used, the same shares have
	// exactly one encoding.
	nextJob := 0
	for i, sh := range s.Shares {
		if int(sh.Job) > nextJob || int(sh.Job) >= len(s.Jobs) {
			return fmt.Errorf("%w: share %d names job %d, the next new one is %d", ErrSettlement, i, sh.Job, nextJob)
		}
		if int(sh.Job) == nextJob {
			nextJob++
		}
		if sh.Depth < 1 || sh.Depth > ShareExpiry {
			return fmt.Errorf("%w: share %d is %d deep", ErrShareDepth, i, sh.Depth)
		}
	}
	if nextJob != len(s.Jobs) {
		return fmt.Errorf("%w: job %d is used by no share", ErrSettlement, nextJob)
	}
	nextKey := 0
	for i, j := range s.Jobs {
		if int(j.Key) > nextKey {
			return fmt.Errorf("%w: job %d names key %d, the next new one is %d", ErrSettlement, i, j.Key, nextKey)
		}
		if int(j.Key) == nextKey {
			nextKey++
		}
	}
	if nextKey != len(s.Keys) {
		return fmt.Errorf("%w: key %d is used by no job", ErrSettlement, nextKey)
	}
	return nil
}

// SettlementView is what checking a settlement body needs from the chain: the blocks below the
// one being checked, as they are on its own branch.
type SettlementView struct {
	// anc[d] is the block d below (anc[1] the parent); anc[0] is unused.
	anc [ShareExpiry + 1]settleAncestor
	// Settled says whether a share was included by a block within ShareExpiry below. The caller
	// keeps that from the bodies it applied; the chain only has the headers.
	Settled func(id [32]byte) bool
}

type settleAncestor struct {
	ok      bool
	id      [32]byte
	height  uint64
	minTime uint32
	bits    [len(mh.Lanes)]uint32
}

// SettlementViewOn collects the view for a block whose parent is `parent`. It is taken under the
// lock once, so checking thousands of shares does not hold the chain.
func (hc *HeaderChain) SettlementViewOn(parent [32]byte, settled func([32]byte) bool) (*SettlementView, error) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	n, ok := hc.byID[parent]
	if !ok {
		return nil, ErrUnknownParent
	}
	v := &SettlementView{Settled: settled}
	for d := 1; d <= ShareExpiry && n != nil; d++ {
		a := &v.anc[d]
		a.ok, a.id, a.height, a.minTime = true, n.id, n.height, hc.minTimeLocked(n)
		for _, lane := range mh.Lanes {
			bits, err := hc.nextBitsLocked(n, lane)
			if err != nil {
				return nil, err
			}
			a.bits[lane] = bits
		}
		if n.id == hc.genesis {
			break
		}
		n = hc.byID[n.header.PrevBlock]
	}
	return v, nil
}

// header rebuilds share i's header as its miner hashed it.
func (s *Settlement) header(i int, v *SettlementView) (*Header, error) {
	sh := &s.Shares[i]
	a := &v.anc[sh.Depth]
	if !a.ok {
		return nil, fmt.Errorf("%w: share %d builds below the genesis", ErrShareDepth, i)
	}
	j := &s.Jobs[sh.Job]
	k := &s.Keys[j.Key]
	commit := ShareCommit{Payout: k.Payout, FinalityKey: k.FinalityKey, Threshold: j.Threshold, Weight: j.Weight}
	return &Header{
		Version: j.Version, PrevBlock: a.id, MerkleRoot: ShareRoot(j.BodyRoot, &commit),
		Time: sh.Time, Bits: a.bits[j.Lane], Height: uint32(a.height + 1), Nonce: sh.Nonce, Lane: j.Lane,
	}, nil
}

// ShareIDs returns the ids of the shares, in their order. It needs the view but no PoW: a caller
// that already applied the block uses it to remember what the block settled.
func (s *Settlement) ShareIDs(v *SettlementView) ([][32]byte, error) {
	if err := s.CheckStructure(); err != nil {
		return nil, err
	}
	out := make([][32]byte, len(s.Shares))
	for i := range s.Shares {
		h, err := s.header(i, v)
		if err != nil {
			return nil, err
		}
		out[i] = h.ID()
	}
	return out, nil
}

// SettledShare is a share a block pays for: who and how much.
type SettledShare struct {
	ID    [32]byte
	Key   SettleKey
	Units uint64
	Lane  mh.Lane
}

// CheckSettlement verifies a block's settlement body against its branch and returns what it
// settles. Each share has to be a share its miner really found on a block within ShareExpiry
// below this one: its PoW, its time, its bits; it must not be a block of this branch or a share
// one of the last blocks already settled; and the shares come in the order of their ids.
//
// The PoW is the expensive part — a full node pays 1.5–5 ms per share. `pow` is where a node
// answers from the shares it already verified when they were relayed. A nil `pow` takes the PoW
// on trust: a light node, for which a share costs 35–100 ms, checks everything else and leaves
// the PoW to the full nodes (the Sharepool spec §7).
func (b *Block) CheckSettlement(v *SettlementView, pow PoWHasher, params NetParams) ([]SettledShare, error) {
	if v == nil {
		return nil, errSettlementMissing
	}
	s := &b.Settlement
	if err := s.CheckStructure(); err != nil {
		return nil, err
	}
	own := b.Header.ID()
	latest := int64(b.Header.Time) + int64(params.FutureDrift(uint64(b.Header.Height))/time.Second)
	out := make([]SettledShare, 0, len(s.Shares))
	var prev [32]byte
	for i := range s.Shares {
		h, err := s.header(i, v)
		if err != nil {
			return nil, err
		}
		id := h.ID()
		if i > 0 && bytes.Compare(prev[:], id[:]) >= 0 {
			return nil, fmt.Errorf("%w: share %d", ErrShareOrder, i)
		}
		prev = id
		if id == own || v.isBlock(id) || (v.Settled != nil && v.Settled(id)) {
			return nil, fmt.Errorf("%w: share %d (%x)", ErrShareSettled, i, id[:6])
		}
		a := &v.anc[s.Shares[i].Depth]
		if h.Time < a.minTime {
			return nil, fmt.Errorf("%w: share %d", ErrTimeTooOld, i)
		}
		if int64(h.Time) > latest {
			return nil, fmt.Errorf("%w: share %d", ErrShareTimeTooNew, i)
		}
		j := &s.Jobs[s.Shares[i].Job]
		if pow != nil {
			hash, err := pow.PoWHash(h)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrSettlementUnchecked, err)
			}
			ok, err := MeetsShareTarget(hash, h.Bits, j.Weight)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("%w: share %d", ErrSharePoW, i)
			}
		}
		out = append(out, SettledShare{ID: id, Key: s.Keys[j.Key], Units: 1 << j.Weight, Lane: j.Lane})
	}
	return out, nil
}

// isBlock says whether an id is one of the blocks in the view: a block of the branch was already
// counted as the block it is.
func (v *SettlementView) isBlock(id [32]byte) bool {
	for d := 1; d < len(v.anc); d++ {
		if v.anc[d].ok && v.anc[d].id == id {
			return true
		}
	}
	return false
}

// NewSettlement builds the settlement body of a block at `height` that includes `shares`. They
// are put in the order of their ids, and the tables are filled in the order the shares need them,
// so the same shares always give the same bytes.
//
// It checks only what it needs to encode them: each share builds on a block within ShareExpiry
// below `height`. Whether they are valid there is CheckSettlement's job.
func NewSettlement(shares []*Share, height uint64) (*Settlement, error) {
	type withID struct {
		id [32]byte
		s  *Share
	}
	sorted := make([]withID, len(shares))
	for i, s := range shares {
		sorted[i] = withID{s.ID(), s}
	}
	slices.SortFunc(sorted, func(a, b withID) int { return bytes.Compare(a.id[:], b.id[:]) })

	out := &Settlement{}
	keys := map[SettleKey]uint16{}
	jobs := map[SettleJob]uint16{}
	for i, e := range sorted {
		if i > 0 && e.id == sorted[i-1].id {
			continue // the same share twice is one share
		}
		s := e.s
		if uint64(s.Header.Height) > height || height-uint64(s.Header.Height)+1 > ShareExpiry {
			return nil, fmt.Errorf("%w: a share at height %d in a block at %d", ErrShareDepth, s.Header.Height, height)
		}
		k := SettleKey{Payout: s.Commit.Payout, FinalityKey: s.Commit.FinalityKey}
		ki, ok := keys[k]
		if !ok {
			ki = uint16(len(out.Keys))
			keys[k] = ki
			out.Keys = append(out.Keys, k)
		}
		j := SettleJob{Key: ki, Lane: s.Header.Lane, Threshold: s.Commit.Threshold, Weight: s.Commit.Weight,
			Version: s.Header.Version, BodyRoot: s.BodyRoot}
		ji, ok := jobs[j]
		if !ok {
			ji = uint16(len(out.Jobs))
			jobs[j] = ji
			out.Jobs = append(out.Jobs, j)
		}
		out.Shares = append(out.Shares, SettleShare{Job: ji,
			Depth: uint8(height - uint64(s.Header.Height) + 1), Time: s.Header.Time, Nonce: s.Header.Nonce})
	}
	if len(out.Shares) > MaxSettlementShares {
		return nil, fmt.Errorf("%w: %d shares", ErrSettlementTooBig, len(out.Shares))
	}
	return out, nil
}

// Tally sums the shares a settlement body pays by payout program.
func (s *Settlement) Tally() map[[32]byte]ShareTally {
	out := make(map[[32]byte]ShareTally)
	for _, sh := range s.Shares {
		if int(sh.Job) >= len(s.Jobs) || int(s.Jobs[sh.Job].Key) >= len(s.Keys) {
			continue
		}
		job := s.Jobs[sh.Job]
		p := s.Keys[job.Key].Payout
		t := out[p]
		t.Shares++
		t.Units += uint64(1) << job.Weight
		out[p] = t
	}
	return out
}
