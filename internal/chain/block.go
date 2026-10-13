package chain

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// A block = the header + the transactions.

const MaxBlockTxs = 20000

var (
	ErrBlockTooBig    = errors.New("chain: the block exceeds the allowed size")
	ErrCoinbaseTooBig = errors.New("chain: the coinbase exceeds the allowed size")
	// ErrStorageMass: the block creates too many tiny outputs (the fees-and-dust note).
	ErrStorageMass       = errors.New("chain: the block exceeds the storage mass cap")
	ErrNoCoinbase        = errors.New("chain: the first transaction is not a coinbase")
	ErrExtraCoinbase     = errors.New("chain: only the first transaction may be a coinbase")
	ErrBadMerkle         = errors.New("chain: the merkle root does not match the header")
	ErrDuplicateTx       = errors.New("chain: the block contains the same transaction twice")
	ErrCoinbaseValue     = errors.New("chain: the coinbase takes more than it may")
	ErrDevFeeMissing     = errors.New("chain: the dev fee output is missing or does not match")
	ErrInputsBelowOutput = errors.New("chain: the outputs exceed the inputs")
)

type Block struct {
	Header *Header
	Txs    []*Tx
}

// Serialize writes the block the way it travels the network.
func (b *Block) Serialize() []byte {
	hdr := b.Header.Serialize()
	out := make([]byte, 0, len(hdr)+2+len(b.Txs)*128)
	out = append(out, hdr[:]...)
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(b.Txs)))
	out = append(out, n[:]...)
	for _, tx := range b.Txs {
		out = append(out, tx.Serialize()...)
	}
	return out
}

// DeserializeBlock reads a block from a foreign peer — that is, untrusted bytes.
// The transaction count is checked before allocating and nothing may be left over at the end.
func DeserializeBlock(data []byte) (*Block, error) {
	if len(data) > MaxBlockBytes {
		return nil, ErrBlockTooBig
	}
	if len(data) < HeaderBytes+4 {
		return nil, errShort
	}
	hdr, err := DeserializeHeader(data[:HeaderBytes])
	if err != nil {
		return nil, err
	}
	count := binary.LittleEndian.Uint32(data[HeaderBytes : HeaderBytes+4])
	if count == 0 || count > MaxBlockTxs {
		return nil, ErrTxTooManyItems
	}
	b := &Block{Header: hdr, Txs: make([]*Tx, 0, count)}
	pos := HeaderBytes + 4
	for i := uint32(0); i < count; i++ {
		tx, used, err := DeserializeTx(data[pos:])
		if err != nil {
			return nil, fmt.Errorf("transaction %d: %w", i, err)
		}
		b.Txs = append(b.Txs, tx)
		pos += used
	}
	if pos != len(data) {
		return nil, ErrTxNotCanonical // leftover bytes = more than one form of the same block
	}
	return b, nil
}

// ValidateBlock verifies a block's body against the UTXO set.
//
// The header (PoW, difficulty, time, lane rotation) is verified in HeaderChain.AddHeader — this is
// about the transactions, the values and the coinbase.
func (b *Block) ValidateBlock(u CoinView, params NetParams) error {
	return b.validate(u, params, true)
}

// ValidateCommitted validates what the header commits to: the block with every witness taken out and
// no signature verified. The id of a block covers the transactions without their witnesses, so the
// same id can arrive with witnesses other than the miner's.
//
// It answers one question after ValidateBlock has failed: is the block bad, or only this copy of it? If
// the committed part is invalid, no witness can save the block. If it is valid, the failure came from
// the witnesses, and those the header says nothing about.
func (b *Block) ValidateCommitted(u CoinView, params NetParams) error {
	bare := &Block{Header: b.Header, Txs: make([]*Tx, len(b.Txs))}
	for i, tx := range b.Txs {
		if tx == nil {
			return ErrNoCoinbase
		}
		c := *tx
		c.Witness = nil
		bare.Txs[i] = &c
	}
	return bare.validate(u, params, false)
}

func (b *Block) validate(u CoinView, params NetParams, signatures bool) error {
	raw := b.Serialize()
	if len(raw) > MaxBlockBytes {
		return ErrBlockTooBig
	}
	if len(b.Txs) == 0 {
		return ErrNoCoinbase
	}
	if !b.Txs[0].IsCoinbase() {
		return ErrNoCoinbase
	}
	if len(b.Txs[0].Serialize()) > MaxCoinbaseBytes {
		return ErrCoinbaseTooBig
	}

	height := uint64(b.Header.Height)
	seenTx := make(map[[32]byte]bool, len(b.Txs))
	// Outputs created in this block — a transaction may spend an output of a previous one in the same block.
	inBlock := make(map[Outpoint]Coin)
	spent := make(map[Outpoint]bool)
	var totalFees int64
	var mass uint64

	for i, tx := range b.Txs {
		if err := tx.CheckSanity(); err != nil {
			return fmt.Errorf("transaction %d: %w", i, err)
		}
		if i > 0 && tx.IsCoinbase() {
			return ErrExtraCoinbase
		}
		txid := tx.TxID()
		if seenTx[txid] {
			return ErrDuplicateTx
		}
		seenTx[txid] = true

		var inValues []int64
		if i > 0 {
			fee, values, err := b.checkInputs(tx, u, inBlock, spent, height, signatures)
			if err != nil {
				return fmt.Errorf("transaction %d: %w", i, err)
			}
			inValues = values
			totalFees += fee
			if totalFees < 0 || totalFees > MaxMoney {
				return ErrTxBadValue
			}
		}
		// Storage mass is summed across the whole block including the coinbase — otherwise tiny
		// outputs could be smuggled through the coinbase (which may be up to 64 kB).
		if mass = satAdd(mass, TxStorageMass(inValues, tx)); mass > MaxBlockStorageMass {
			return fmt.Errorf("%w (transaction %d)", ErrStorageMass, i)
		}
		for j, out := range tx.Outputs {
			if out.ScriptVersion == ScriptVersionData {
				continue
			}
			inBlock[Outpoint{TxID: txid, Index: uint32(j)}] = Coin{
				Value: out.Value, ScriptVersion: out.ScriptVersion, Script: out.Script,
				Height: height, Coinbase: i == 0,
			}
		}
	}

	if b.Header.MerkleRoot != TxMerkleRoot(b.Txs) {
		return ErrBadMerkle
	}
	if params.Active(ForkWitnessCommitment, height) {
		// That the commitment is there is a property of the block. Whether the witnesses match it
		// is a property of this copy — and ValidateCommitted has taken them out.
		want, err := b.witnessCommitment()
		if err != nil {
			return err
		}
		if signatures {
			if err := b.checkWitnessesAgainst(want); err != nil {
				return err
			}
		}
	}
	return b.checkCoinbase(height, totalFees, params)
}

// checkInputs verifies one transaction's inputs: their existence, maturity, double spending and
// the conservation of value. It returns the fee.
func (b *Block) checkInputs(tx *Tx, u CoinView, inBlock map[Outpoint]Coin,
	spent map[Outpoint]bool, height uint64, signatures bool) (int64, []int64, error) {

	var inSum int64
	// The coins are all collected first — the sighash commits to the amounts and the scripts of
	// every input, so a signature cannot be verified before we know them.
	coins := make([]Coin, 0, len(tx.Inputs))
	for _, in := range tx.Inputs {
		op := Outpoint{TxID: in.PrevTxID, Index: in.PrevIndex}
		if spent[op] {
			return 0, nil, ErrCoinMissing // a double spend inside the block
		}
		coin, ok := inBlock[op]
		if !ok {
			coin, ok = u.Get(op)
		}
		if !ok {
			return 0, nil, ErrCoinMissing
		}
		if coin.Coinbase && height < coin.Height+CoinbaseMaturity {
			return 0, nil, ErrCoinImmature
		}
		spent[op] = true
		coins = append(coins, coin)
		inSum += coin.Value
		if inSum < 0 || inSum > MaxMoney {
			return 0, nil, ErrTxBadValue
		}
	}

	// The signatures are verified last — they are the most expensive and there is no point
	// computing them for a transaction that fails a cheaper check.
	for i := range tx.Inputs {
		if !signatures {
			break
		}
		if err := VerifyInput(tx, i, coins); err != nil {
			return 0, nil, fmt.Errorf("input %d: %w", i, err)
		}
	}

	var outSum int64
	for _, o := range tx.Outputs {
		outSum += o.Value
	}
	if outSum > inSum {
		return 0, nil, ErrInputsBelowOutput
	}
	values := make([]int64, len(coins))
	for i, c := range coins {
		values[i] = c.Value
	}
	return inSum - outSum, values, nil
}

// checkCoinbase verifies that the miner did not take more than theirs and that the dev fee is in place.
func (b *Block) checkCoinbase(height uint64, fees int64, params NetParams) error {
	cb := b.Txs[0]
	// The height in the coinbase (what BIP34 does in Bitcoin). Without it the uniqueness of the
	// coinbase txid rests only on a random extranonce — and two blocks with the same extranonce,
	// the same address and the same reward have **the same txid**. Overwriting an unspent output is
	// prevented by `ErrCoinExists`, but once it is fully spent a duplicate txid is acceptable and
	// the transaction index then has two answers to the same question.
	//
	// The first 8 B of the extranonce field are therefore the height, the remaining 24 B stay the
	// miner's.
	if got := binary.LittleEndian.Uint64(cb.Inputs[0].PrevTxID[0:8]); got != height {
		return fmt.Errorf("%w: the coinbase reports height %d, the block is %d", ErrCoinbaseHeight, got, height)
	}

	var total int64
	for _, o := range cb.Outputs {
		total += o.Value
	}
	allowed := Subsidy(height) + fees
	if total > allowed {
		return fmt.Errorf("%w: takes %d, may take %d", ErrCoinbaseValue, total, allowed)
	}

	devFee := DevFee(height)
	if devFee == 0 {
		return nil
	}
	// The dev fee has to be the **first** output and has to match exactly — not "at least that much".
	if len(cb.Outputs) == 0 {
		return ErrDevFeeMissing
	}
	first := cb.Outputs[0]
	if first.Value != devFee || first.ScriptVersion != ScriptVersionBasic ||
		!equalBytes(first.Script, params.DevFeeScript) {
		return fmt.Errorf("%w: got %d cogs, expected %d", ErrDevFeeMissing, first.Value, devFee)
	}
	return nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// BuildCoinbase assembles a valid coinbase transaction. Both the miner and a pool use it for a
// template, so the dev fee rules live in one place.
//
// `extra` is the miner's free space (the extranonce), `minerScript` is their payout script.
func BuildCoinbase(height uint64, fees int64, minerScript []byte, extra [32]byte,
	params NetParams) *Tx {

	// The first 8 B are the height (the rule in checkCoinbase), the rest is the miner's extranonce.
	binary.LittleEndian.PutUint64(extra[0:8], height)
	tx := &Tx{Version: 1, Inputs: []TxIn{{PrevTxID: extra, PrevIndex: CoinbaseIndex}}}
	devFee := DevFee(height)
	if devFee > 0 {
		tx.Outputs = append(tx.Outputs, TxOut{Value: devFee, Script: params.DevFeeScript})
	}
	minerValue := Subsidy(height) - devFee + fees
	if minerValue > 0 {
		tx.Outputs = append(tx.Outputs, TxOut{Value: minerValue, Script: minerScript})
	}
	if len(tx.Outputs) == 0 {
		// The genesis and blocks with a zero reward need at least one output.
		tx.Outputs = append(tx.Outputs, TxOut{Value: 0, ScriptVersion: ScriptVersionData})
	}
	return tx
}

// SealCoinbase finishes a block's transaction list: where the rule holds, it writes the witness
// commitment into the coinbase (txs[0]). It has to be called once the list is final and before
// the merkle root is computed — the commitment is an output, so it changes the coinbase's txid.
func SealCoinbase(txs []*Tx, height uint64, params NetParams) {
	if len(txs) == 0 || !params.Active(ForkWitnessCommitment, height) {
		return
	}
	cb := txs[0]
	out := TxOut{ScriptVersion: ScriptVersionData, Script: witnessCommitmentScript(WitnessMerkleRoot(txs))}
	// A transaction may have one data output, so one that is already there is taken over.
	if n := len(cb.Outputs); n > 0 && cb.Outputs[n-1].ScriptVersion == ScriptVersionData {
		cb.Outputs[n-1] = out
		return
	}
	cb.Outputs = append(cb.Outputs, out)
}
