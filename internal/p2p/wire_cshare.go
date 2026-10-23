package p2p

import (
	"encoding/binary"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
)

// The compact form of shares on the wire (the Sharepool spec §2.3).
//
// A share in full is 187 B, and most of it is the same for every share a miner finds on one
// template: the payout program and the finality key, the body root, the parent. So each side of a
// connection keeps two dictionaries the sender fills: **keys** (payout program + finality key) and
// **jobs** (a key, the lane, the threshold and everything of the header but the nonce and the
// time). Each entry is sent once and gets the next sequence number; a share then names its job by
// the low 16 bits of that number and carries only what differs — 14 B with its tag.
//
// A `shares` message is a list of items read in order: a key, a job or a share. The receiver keeps
// the last ShareDictWindow entries of each dictionary, and the sender defines an entry again once
// it is older than that. Every entry has to come with exactly the next number, so the two sides
// can never disagree about what a number means — a peer that breaks that is sending garbage.

// CmdShares carries shares in the compact form, in batches.
const CmdShares Command = "shares"

const (
	// ShareDictWindow is how many of the last keys and jobs a reference may reach: a key is 64 B,
	// a job 90 B, so a connection holds at most ~630 kB of them.
	ShareDictWindow = 4096
	// MaxShareItemsPerMessage caps the items of one message.
	MaxShareItemsPerMessage = 4000

	itemKey   = 0
	itemJob   = 1
	itemShare = 2

	shareKeyItemBytes   = 1 + 4 + 32 + 32
	shareJobItemBytes   = 1 + 4 + 4 + 1 + 1 + 4 + 4 + 4 + 4 + 32 + 32
	compactShareBytes   = 1 + 2 + 8 + 2 + 1
	shareTimeDeltaLimit = 1<<15 - 1
)

type shareKey struct {
	payout, finality [32]byte
}

// shareJob is everything of a share but its nonce, time and weight.
type shareJob struct {
	key       uint32 // the key's sequence number
	lane      uint8
	threshold uint8
	version   uint32
	bits      uint32
	height    uint32
	time      uint32 // the time the shares' deltas start from
	prev      [32]byte
	bodyRoot  [32]byte
}

// jobIdent is what makes two shares belong to one job, the key by value.
type jobIdent struct {
	key       shareKey
	lane      uint8
	threshold uint8
	version   uint32
	bits      uint32
	height    uint32
	prev      [32]byte
	bodyRoot  [32]byte
}

// fresh says whether entry `seq` is still held by the other side, `next` being the number the
// next entry gets.
func fresh(seq, next uint32) bool { return next-seq <= ShareDictWindow }

// ShareEncoder is the sending side of one connection.
type ShareEncoder struct {
	nextKey, nextJob uint32
	keys             map[shareKey]uint32
	jobs             map[jobIdent]encJob
}

type encJob struct {
	seq, key, time uint32
}

// NewShareEncoder starts both dictionaries empty.
func NewShareEncoder() *ShareEncoder {
	return &ShareEncoder{keys: map[shareKey]uint32{}, jobs: map[jobIdent]encJob{}}
}

// Encode packs shares into one `shares` payload, defining the keys and jobs the other side does not
// have. The caller keeps a batch under MaxShareItemsPerMessage / 3 shares, so it always fits.
func (e *ShareEncoder) Encode(shares []*chain.Share) []byte {
	out := make([]byte, 0, len(shares)*compactShareBytes)
	for _, s := range shares {
		k := shareKey{s.Commit.Payout, s.Commit.FinalityKey}
		ks, ok := e.keys[k]
		if !ok || !fresh(ks, e.nextKey) {
			ks = e.nextKey
			e.nextKey++
			e.keys[k] = ks
			out = append(out, itemKey)
			out = binary.LittleEndian.AppendUint32(out, ks)
			out = append(out, k.payout[:]...)
			out = append(out, k.finality[:]...)
		}
		h := &s.Header
		id := jobIdent{k, uint8(h.Lane), s.Commit.Threshold, h.Version, h.Bits, h.Height, h.PrevBlock, s.BodyRoot}
		j, ok := e.jobs[id]
		delta := int64(h.Time) - int64(j.time)
		if !ok || j.key != ks || !fresh(j.seq, e.nextJob) || delta < -shareTimeDeltaLimit || delta > shareTimeDeltaLimit {
			j = encJob{seq: e.nextJob, key: ks, time: h.Time}
			e.nextJob++
			e.jobs[id] = j
			delta = 0
			out = append(out, itemJob)
			out = binary.LittleEndian.AppendUint32(out, j.seq)
			out = binary.LittleEndian.AppendUint32(out, ks)
			out = append(out, uint8(h.Lane), s.Commit.Threshold)
			out = binary.LittleEndian.AppendUint32(out, h.Version)
			out = binary.LittleEndian.AppendUint32(out, h.Bits)
			out = binary.LittleEndian.AppendUint32(out, h.Height)
			out = binary.LittleEndian.AppendUint32(out, h.Time)
			out = append(out, h.PrevBlock[:]...)
			out = append(out, s.BodyRoot[:]...)
		}
		out = append(out, itemShare)
		out = binary.LittleEndian.AppendUint16(out, uint16(j.seq))
		out = binary.LittleEndian.AppendUint64(out, h.Nonce)
		out = binary.LittleEndian.AppendUint16(out, uint16(int16(delta)))
		out = append(out, s.Commit.Weight)
	}
	e.prune()
	return out
}

// prune forgets the entries the other side no longer holds, so the maps stay at the window.
func (e *ShareEncoder) prune() {
	if len(e.keys) > 2*ShareDictWindow {
		for k, seq := range e.keys {
			if !fresh(seq, e.nextKey) {
				delete(e.keys, k)
			}
		}
	}
	if len(e.jobs) > 2*ShareDictWindow {
		for id, j := range e.jobs {
			if !fresh(j.seq, e.nextJob) {
				delete(e.jobs, id)
			}
		}
	}
}

// ShareDecoder is the receiving side of one connection.
type ShareDecoder struct {
	nextKey, nextJob uint32
	keys             map[uint32]shareKey
	jobs             map[uint32]shareJob
}

// NewShareDecoder starts both dictionaries empty.
func NewShareDecoder() *ShareDecoder {
	return &ShareDecoder{keys: map[uint32]shareKey{}, jobs: map[uint32]shareJob{}}
}

// Decode reads a `shares` payload into whole shares. Their PoW is not checked here — that is the
// node's job. Any error means the peer sent what no honest encoder writes; the dictionaries may
// then be half updated, and the connection is not worth keeping.
func (d *ShareDecoder) Decode(payload []byte) ([]*chain.Share, error) {
	var out []*chain.Share
	items := 0
	for p := payload; len(p) > 0; items++ {
		if items == MaxShareItemsPerMessage {
			return nil, ErrTooManyItems
		}
		switch p[0] {
		case itemKey:
			if len(p) < shareKeyItemBytes || binary.LittleEndian.Uint32(p[1:]) != d.nextKey {
				return nil, ErrBadPayload
			}
			var k shareKey
			copy(k.payout[:], p[5:37])
			copy(k.finality[:], p[37:69])
			d.keys[d.nextKey] = k
			delete(d.keys, d.nextKey-ShareDictWindow)
			d.nextKey++
			p = p[shareKeyItemBytes:]
		case itemJob:
			if len(p) < shareJobItemBytes || binary.LittleEndian.Uint32(p[1:]) != d.nextJob {
				return nil, ErrBadPayload
			}
			j := shareJob{key: binary.LittleEndian.Uint32(p[5:]), lane: p[9], threshold: p[10],
				version: binary.LittleEndian.Uint32(p[11:]), bits: binary.LittleEndian.Uint32(p[15:]),
				height: binary.LittleEndian.Uint32(p[19:]), time: binary.LittleEndian.Uint32(p[23:])}
			copy(j.prev[:], p[27:59])
			copy(j.bodyRoot[:], p[59:91])
			if _, ok := d.keys[j.key]; !ok || j.lane > uint8(mh.LaneG) {
				return nil, ErrBadPayload
			}
			d.jobs[d.nextJob] = j
			delete(d.jobs, d.nextJob-ShareDictWindow)
			d.nextJob++
			p = p[shareJobItemBytes:]
		case itemShare:
			if len(p) < compactShareBytes || d.nextJob == 0 {
				return nil, ErrBadPayload
			}
			last := d.nextJob - 1
			seq := last - uint32(uint16(last)-binary.LittleEndian.Uint16(p[1:]))
			j, ok := d.jobs[seq]
			if !ok {
				return nil, ErrBadPayload
			}
			k, ok := d.keys[j.key]
			if !ok {
				return nil, ErrBadPayload
			}
			delta := int64(int16(binary.LittleEndian.Uint16(p[11:])))
			s := &chain.Share{BodyRoot: j.bodyRoot, Commit: chain.ShareCommit{Payout: k.payout,
				FinalityKey: k.finality, Threshold: j.threshold, Weight: p[13]}}
			s.Header = chain.Header{Version: j.version, PrevBlock: j.prev, Time: uint32(int64(j.time) + delta),
				Bits: j.bits, Height: j.height, Nonce: binary.LittleEndian.Uint64(p[3:]), Lane: mh.Lane(j.lane),
				MerkleRoot: chain.ShareRoot(j.bodyRoot, &s.Commit)}
			out = append(out, s)
			p = p[compactShareBytes:]
		default:
			return nil, ErrBadPayload
		}
	}
	return out, nil
}
