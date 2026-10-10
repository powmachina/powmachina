package mh

import (
	"encoding/binary"
	"math/bits"

	"github.com/zeebo/blake3"
)

// B-core — the bandwidth-bound core (the GPU domain).
//
// 64 mutually independent 256 B accesses into the DAG per round: a GPU coalesces and parallelizes
// them, a CPU is limited by its number of memory channels. The RANDMATH sequence changes every epoch,
// so a hardwired datapath has nothing to fix in place.

// xof is a BLAKE3 hasher that gets recycled. Allocating one for every DAG item would be most of the
// time with millions of items.
type xof struct{ h *blake3.Hasher }

func newXof() *xof { return &xof{h: blake3.New()} }

// fill fills out with the BLAKE3 XOF stream over domain ‖ parts.
func (x *xof) fill(out []byte, domain []byte, parts ...[]byte) {
	x.h.Reset()
	_, _ = x.h.Write(domain)
	for _, p := range parts {
		_, _ = x.h.Write(p)
	}
	d := x.h.Digest()
	if _, err := d.Read(out); err != nil {
		// blake3.Digest.Read never returns an error; if it started to, that is a programming bug.
		panic("mh: BLAKE3 XOF failed: " + err.Error())
	}
}

// sum32 is a 32-byte BLAKE3 over domain ‖ parts.
func (x *xof) sum32(domain []byte, parts ...[]byte) [32]byte {
	var out [32]byte
	x.fill(out[:], domain, parts...)
	return out
}

// kiss99 is the generator ProgPoW also uses to derive its sequence of operations.
type kiss99 struct{ z, w, jsr, jcong uint32 }

func newKiss99(seed *[32]byte) kiss99 {
	g := func(o int) uint32 { return binary.LittleEndian.Uint32(seed[o : o+4]) }
	return kiss99{z: g(0) | 1, w: g(4) | 1, jsr: g(8) | 1, jcong: g(12) | 1}
}

func (k *kiss99) next() uint32 {
	k.z = 36969*(k.z&65535) + (k.z >> 16)
	k.w = 18000*(k.w&65535) + (k.w >> 16)
	mwc := (k.z << 16) + k.w
	k.jsr ^= k.jsr << 17
	k.jsr ^= k.jsr >> 13
	k.jsr ^= k.jsr << 5
	k.jcong = 69069*k.jcong + 1234567
	return (mwc ^ k.jcong) + k.jsr
}

type mathOp struct{ kind, src, dst uint8 }

// RandMath is the sequence of operations valid for one epoch.
type RandMath struct {
	epoch uint64
	ops   [RandMathOps]mathOp
}

// NewRandMath derives the sequence for a given epoch.
func NewRandMath(epoch uint64) *RandMath {
	var epochLE [8]byte
	binary.LittleEndian.PutUint64(epochLE[:], epoch)
	seed := newXof().sum32(domainEpoch, epochLE[:])

	rng := newKiss99(&seed)
	m := &RandMath{epoch: epoch}
	for i := range m.ops {
		r := rng.next()
		m.ops[i] = mathOp{
			kind: uint8(r % 9),
			src:  uint8(rng.next() % MixWords),
			dst:  uint8(rng.next() % MixWords),
		}
	}
	return m
}

// Epoch returns the epoch the sequence is valid for.
func (m *RandMath) Epoch() uint64 { return m.epoch }

// OpsRaw is the sequence as raw triples (kind, src, dst) — for the GPU kernel, which receives it as a
// constant. It is deliberately not derived a second time in CUDA: KISS99 in two implementations would
// be exactly the one-constant difference that splits a network.
func (m *RandMath) OpsRaw() [RandMathOps][3]uint8 {
	var out [RandMathOps][3]uint8
	for i, op := range m.ops {
		out[i] = [3]uint8{op.kind, op.src, op.dst}
	}
	return out
}

func (m *RandMath) apply(mix *[MixWords]uint32, k int) {
	for i := range m.ops {
		op := &m.ops[i]
		d := (int(op.dst) + k*13 + i) % MixWords
		s := (int(op.src) + k*7 + i) % MixWords
		a, b := mix[d], mix[s]
		switch op.kind {
		case 0:
			mix[d] = a + b
		case 1:
			mix[d] = a * (b | 1)
		case 2:
			mix[d] = a ^ b
		case 3:
			mix[d] = bits.RotateLeft32(a, int(b&31))
		case 4:
			mix[d] = bits.RotateLeft32(a, -int(b&31))
		case 5:
			mix[d] = a + uint32(bits.LeadingZeros32(b))
		case 6:
			mix[d] = a ^ uint32(bits.OnesCount32(b))
		case 7:
			mix[d] = min(a, b) + max(a, b)>>3
		default:
			mix[d] = uint32((uint64(a) * uint64(b)) >> 32)
		}
	}
}

func mixToBytes(mix *[MixWords]uint32, out *[MixWords * 4]byte) {
	for i, w := range mix {
		binary.LittleEndian.PutUint32(out[i*4:], w)
	}
}

func bytesToMix(in *[MixWords * 4]byte, mix *[MixWords]uint32) {
	for i := range mix {
		mix[i] = binary.LittleEndian.Uint32(in[i*4:])
	}
}

// bcoreState is one thread's workspace. It is kept outside the loop so B-core
// does not allocate per hash.
type bcoreState struct {
	x     *xof
	mix   [MixWords]uint32
	item  [DagItemWords]uint32
	bytes [MixWords * 4]byte
}

func newBcoreState() *bcoreState { return &bcoreState{x: newXof()} }

// run computes one B segment: rounds rounds of accesses independent DAG accesses each.
func (s *bcoreState) run(seed *[32]byte, rounds uint32, accesses int, dag DagAccess, math *RandMath) [32]byte {
	s.x.fill(s.bytes[:], domainBcore, seed[:])
	bytesToMix(&s.bytes, &s.mix)

	items := uint32(dag.ItemCount())
	for r := uint32(0); r < rounds; r++ {
		for k := range accesses {
			// The indices within a round are mutually independent → parallelizable,
			// bandwidth-bound.
			idx := fnv1a(s.mix[(k*7)%MixWords]^uint32(k), r) % items
			dag.ItemInto(int(idx), &s.item)
			for w := range s.mix {
				s.mix[w] = fnv1a(s.mix[w], s.item[w])
			}
			math.apply(&s.mix, k)
		}
		// compress round
		mixToBytes(&s.mix, &s.bytes)
		s.x.fill(s.bytes[:], domainBcore, []byte("round"), s.bytes[:])
		bytesToMix(&s.bytes, &s.mix)
	}

	mixToBytes(&s.mix, &s.bytes)
	var tail [8]byte
	binary.LittleEndian.PutUint32(tail[0:], rounds)
	binary.LittleEndian.PutUint32(tail[4:], uint32(accesses))
	return s.x.sum32(domainBcore, []byte("out"), s.bytes[:], tail[:])
}
