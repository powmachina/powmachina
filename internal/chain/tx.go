package chain

import (
	"encoding/binary"
	"errors"

	"powmachina/internal/mh"
)

// The transaction format.
//
// The design has no scriptSig from the start: signatures live in the witness, which **does not
// enter the txid**. Malleability is therefore excluded by construction, not by a patch (SPEC §3).
//
// The serialization is **canonical**: every transaction has exactly one valid form. Freedom in the
// encoding is a source of implementations diverging, and with consensus divergence is fatal.

const (
	MaxTxInputs    = 1000
	MaxTxOutputs   = 1000
	MaxScriptBytes = 10 * 1024
	// MaxWitnessItems and MaxWitnessItemBytes are deliberately looser than in Bitcoin (520 B), so
	// a covenant script and a zk proof fit (SPEC §2.4). Ballast is blocked by price, not by format.
	MaxWitnessItems     = 16
	MaxWitnessItemBytes = 10 * 1024
	MaxWitnessPerInput  = 16 * 1024

	// CoinbaseIndex marks a coinbase input.
	CoinbaseIndex uint32 = 0xffffffff

	// ScriptVersionBasic is the basic script set.
	ScriptVersionBasic uint8 = 0
	// ScriptVersionMaxReserved — versions 1..15 are reserved for future script types (the soft-fork
	// path, SPEC §2.4). Higher values other than the data version are invalid.
	ScriptVersionMaxReserved uint8 = 15
	// ScriptVersionData is an unspendable data output (our equivalent of OP_RETURN).
	ScriptVersionData uint8 = 0xff
	// MaxDataScriptBytes keeps data outputs at the level of a hash or an anchor, not storage.
	MaxDataScriptBytes = 80
)

var (
	ErrTxNoInputs     = errors.New("chain: a transaction with no inputs")
	ErrTxNoOutputs    = errors.New("chain: a transaction with no outputs")
	ErrTxTooManyItems = errors.New("chain: the transaction exceeds the item count limits")
	ErrTxBadScript    = errors.New("chain: an invalid or oversized script")
	ErrTxBadValue     = errors.New("chain: an invalid output value")
	ErrTxNotCanonical = errors.New("chain: the transaction is not in canonical form")
	// ErrCoinbaseHeight: the coinbase does not carry its block's height. It is what BIP34
	// introduced in Bitcoin — without it a coinbase txid is not unique by construction.
	ErrCoinbaseHeight = errors.New("chain: the coinbase does not carry the block height")
	ErrTxBadVersion   = errors.New("chain: unknown script version")
)

// TxIn is an input. In a coinbase, PrevTxID is free space for the miner (the extranonce) and
// PrevIndex has the value CoinbaseIndex.
type TxIn struct {
	PrevTxID  [32]byte
	PrevIndex uint32
	Sequence  uint32
}

// TxOut is an output.
type TxOut struct {
	Value         int64 // in cogs
	ScriptVersion uint8
	Script        []byte
}

// Witness carries the signatures for one input. It does not enter the txid.
type Witness struct {
	Items [][]byte
}

type Tx struct {
	Version  uint32
	Inputs   []TxIn
	Outputs  []TxOut
	LockTime uint32
	Witness  []Witness // the length has to match the number of inputs (or be zero)
}

// IsCoinbase says whether this is a coinbase transaction.
func (t *Tx) IsCoinbase() bool {
	return len(t.Inputs) == 1 && t.Inputs[0].PrevIndex == CoinbaseIndex
}

// SerializeNoWitness is the form the txid is computed from.
func (t *Tx) SerializeNoWitness() []byte {
	out := make([]byte, 0, 64+len(t.Inputs)*40+len(t.Outputs)*48)
	var b [8]byte
	binary.LittleEndian.PutUint32(b[:4], t.Version)
	out = append(out, b[:4]...)

	binary.LittleEndian.PutUint16(b[:2], uint16(len(t.Inputs)))
	out = append(out, b[:2]...)
	for _, in := range t.Inputs {
		out = append(out, in.PrevTxID[:]...)
		binary.LittleEndian.PutUint32(b[:4], in.PrevIndex)
		out = append(out, b[:4]...)
		binary.LittleEndian.PutUint32(b[:4], in.Sequence)
		out = append(out, b[:4]...)
	}

	binary.LittleEndian.PutUint16(b[:2], uint16(len(t.Outputs)))
	out = append(out, b[:2]...)
	for _, o := range t.Outputs {
		binary.LittleEndian.PutUint64(b[:], uint64(o.Value))
		out = append(out, b[:]...)
		out = append(out, o.ScriptVersion)
		binary.LittleEndian.PutUint16(b[:2], uint16(len(o.Script)))
		out = append(out, b[:2]...)
		out = append(out, o.Script...)
	}

	binary.LittleEndian.PutUint32(b[:4], t.LockTime)
	return append(out, b[:4]...)
}

// SerializeWitness is the serialization of the signatures. An empty witness has zero length.
func (t *Tx) SerializeWitness() []byte {
	if len(t.Witness) == 0 {
		return nil
	}
	out := make([]byte, 0, 64)
	var b [2]byte
	for _, w := range t.Witness {
		out = append(out, byte(len(w.Items)))
		for _, item := range w.Items {
			binary.LittleEndian.PutUint16(b[:], uint16(len(item)))
			out = append(out, b[:]...)
			out = append(out, item...)
		}
	}
	return out
}

// Serialize is the whole transaction including the witness (as it travels the network and sits in a block).
func (t *Tx) Serialize() []byte {
	body := t.SerializeNoWitness()
	wit := t.SerializeWitness()
	out := make([]byte, 0, len(body)+len(wit)+4)
	out = append(out, body...)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(len(wit)))
	out = append(out, b[:]...)
	return append(out, wit...)
}

// TxID is the transaction's identity — **without the witness**, so signatures cannot change it.
func (t *Tx) TxID() [32]byte {
	return mh.TaggedHash("PowMachina/txid/v0", t.SerializeNoWitness())
}

// WTxID covers the signatures too; the commitment to it goes into the coinbase.
func (t *Tx) WTxID() [32]byte {
	return mh.TaggedHash("PowMachina/wtxid/v0", t.Serialize())
}

// CheckSanity verifies what can be verified without knowing the UTXO set.
func (t *Tx) CheckSanity() error {
	if len(t.Inputs) == 0 {
		return ErrTxNoInputs
	}
	if len(t.Outputs) == 0 {
		return ErrTxNoOutputs
	}
	if len(t.Inputs) > MaxTxInputs || len(t.Outputs) > MaxTxOutputs {
		return ErrTxTooManyItems
	}
	if len(t.Witness) != 0 && len(t.Witness) != len(t.Inputs) {
		return ErrTxNotCanonical
	}

	dataOutputs := 0
	var total int64
	for _, o := range t.Outputs {
		if o.Value < 0 {
			return ErrTxBadValue
		}
		// An overflow check: the sum of the outputs must not exceed the emission cap.
		total += o.Value
		if total < 0 || total > MaxMoney {
			return ErrTxBadValue
		}
		switch {
		case o.ScriptVersion == ScriptVersionData:
			dataOutputs++
			if o.Value != 0 || len(o.Script) > MaxDataScriptBytes {
				return ErrTxBadScript
			}
		case o.ScriptVersion <= ScriptVersionMaxReserved:
			if len(o.Script) == 0 || len(o.Script) > MaxScriptBytes {
				return ErrTxBadScript
			}
		default:
			return ErrTxBadVersion
		}
	}
	if dataOutputs > 1 {
		return ErrTxBadScript // at most one data output per transaction
	}

	for _, w := range t.Witness {
		if len(w.Items) > MaxWitnessItems {
			return ErrTxTooManyItems
		}
		size := 0
		for _, item := range w.Items {
			if len(item) > MaxWitnessItemBytes {
				return ErrTxBadScript
			}
			size += len(item)
		}
		if size > MaxWitnessPerInput {
			return ErrTxBadScript
		}
	}

	if t.IsCoinbase() {
		return nil
	}
	// An ordinary transaction must have neither a coinbase input nor the same outpoint twice.
	seen := make(map[Outpoint]bool, len(t.Inputs))
	for _, in := range t.Inputs {
		if in.PrevIndex == CoinbaseIndex {
			return ErrTxNotCanonical
		}
		op := Outpoint{TxID: in.PrevTxID, Index: in.PrevIndex}
		if seen[op] {
			return ErrTxNotCanonical // a double spend inside one transaction
		}
		seen[op] = true
	}
	return nil
}

// DeserializeTx reads a transaction from a foreign peer. Every count is checked before allocating
// and at the end the whole input has to be consumed exactly — otherwise the same transaction would
// have more than one form.
func DeserializeTx(data []byte) (*Tx, int, error) {
	r := &reader{buf: data}
	t := &Tx{}
	var err error

	if t.Version, err = r.u32(); err != nil {
		return nil, 0, err
	}
	inCount, err := r.u16()
	if err != nil {
		return nil, 0, err
	}
	if int(inCount) > MaxTxInputs {
		return nil, 0, ErrTxTooManyItems
	}
	t.Inputs = make([]TxIn, inCount)
	for i := range t.Inputs {
		if err := r.bytes32(&t.Inputs[i].PrevTxID); err != nil {
			return nil, 0, err
		}
		if t.Inputs[i].PrevIndex, err = r.u32(); err != nil {
			return nil, 0, err
		}
		if t.Inputs[i].Sequence, err = r.u32(); err != nil {
			return nil, 0, err
		}
	}

	outCount, err := r.u16()
	if err != nil {
		return nil, 0, err
	}
	if int(outCount) > MaxTxOutputs {
		return nil, 0, ErrTxTooManyItems
	}
	t.Outputs = make([]TxOut, outCount)
	for i := range t.Outputs {
		v, err := r.u64()
		if err != nil {
			return nil, 0, err
		}
		if v > uint64(MaxMoney) {
			return nil, 0, ErrTxBadValue
		}
		t.Outputs[i].Value = int64(v)
		if t.Outputs[i].ScriptVersion, err = r.u8(); err != nil {
			return nil, 0, err
		}
		scriptLen, err := r.u16()
		if err != nil {
			return nil, 0, err
		}
		if int(scriptLen) > MaxScriptBytes {
			return nil, 0, ErrTxBadScript
		}
		if t.Outputs[i].Script, err = r.take(int(scriptLen)); err != nil {
			return nil, 0, err
		}
	}

	if t.LockTime, err = r.u32(); err != nil {
		return nil, 0, err
	}
	witLen, err := r.u32()
	if err != nil {
		return nil, 0, err
	}
	if witLen > uint32(MaxWitnessPerInput*MaxTxInputs) {
		return nil, 0, ErrTxTooManyItems
	}
	witBytes, err := r.take(int(witLen))
	if err != nil {
		return nil, 0, err
	}
	if witLen > 0 {
		wr := &reader{buf: witBytes}
		t.Witness = make([]Witness, len(t.Inputs))
		for i := range t.Witness {
			n, err := wr.u8()
			if err != nil {
				return nil, 0, err
			}
			if int(n) > MaxWitnessItems {
				return nil, 0, ErrTxTooManyItems
			}
			t.Witness[i].Items = make([][]byte, n)
			for j := range t.Witness[i].Items {
				l, err := wr.u16()
				if err != nil {
					return nil, 0, err
				}
				if int(l) > MaxWitnessItemBytes {
					return nil, 0, ErrTxBadScript
				}
				if t.Witness[i].Items[j], err = wr.take(int(l)); err != nil {
					return nil, 0, err
				}
			}
		}
		if wr.left() != 0 {
			return nil, 0, ErrTxNotCanonical
		}
	}
	return t, r.pos, nil
}

// reader is a helper for strict reading — it never reads past the end of the buffer.
type reader struct {
	buf []byte
	pos int
}

var errShort = errors.New("chain: the data is shorter than the format requires")

func (r *reader) take(n int) ([]byte, error) {
	if n < 0 || r.pos+n > len(r.buf) {
		return nil, errShort
	}
	out := r.buf[r.pos : r.pos+n]
	r.pos += n
	return out, nil
}

func (r *reader) u8() (uint8, error) {
	b, err := r.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *reader) u16() (uint16, error) {
	b, err := r.take(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

func (r *reader) u32() (uint32, error) {
	b, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (r *reader) u64() (uint64, error) {
	b, err := r.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

func (r *reader) bytes32(dst *[32]byte) error {
	b, err := r.take(32)
	if err != nil {
		return err
	}
	copy(dst[:], b)
	return nil
}

func (r *reader) left() int { return len(r.buf) - r.pos }
