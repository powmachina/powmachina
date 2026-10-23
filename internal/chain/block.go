package chain

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// A block = the header + the share commitment + the settlement body + the transactions.
//
// The commitment (share.go) is what the header's merkle root is built from together with the
// transactions: the miner's payout, its finality key, its payout threshold and the weight of the
// share the block closes. A block is the share whose hash also met the block's target.

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
	ErrCoinbaseValue     = errors.New("chain: the coinbase does not pay what the credit state owes")
	ErrCreditRoot        = errors.New("chain: the coinbase commits to another credit state")
	ErrDevFeeMissing     = errors.New("chain: the dev fee output is missing or does not match")
	ErrInputsBelowOutput = errors.New("chain: the outputs exceed the inputs")
)

type Block struct {
	Header     *Header
	Share      ShareCommit
	Settlement Settlement
	Txs        []*Tx
}

// Serialize writes the block the way it travels the network.
func (b *Block) Serialize() []byte {
	hdr := b.Header.Serialize()
	commit := b.Share.Serialize()
	settlement := b.Settlement.Serialize()
	out := make([]byte, 0, len(hdr)+len(commit)+len(settlement)+4+len(b.Txs)*128)
	out = append(out, hdr[:]...)
	out = append(out, commit[:]...)
	out = append(out, settlement...)
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(b.Txs)))
	out = append(out, n[:]...)
	for _, tx := range b.Txs {
		out = append(out, tx.Serialize()...)
	}
	return out
}

// MaxBlockWireBytes is the most a block can take on the wire: the transactions' cap and the
// settlement body's.
const MaxBlockWireBytes = MaxBlockBytes + MaxSettlementBytes

// DeserializeBlock reads a block from a foreign peer — that is, untrusted bytes.
// The transaction count is checked before allocating and nothing may be left over at the end.
func DeserializeBlock(data []byte) (*Block, error) {
	if len(data) > MaxBlockWireBytes {
		return nil, ErrBlockTooBig
	}
	const fixed = HeaderBytes + ShareCommitBytes
	if len(data) < fixed+4 {
		return nil, errShort
	}
	hdr, err := DeserializeHeader(data[:HeaderBytes])
	if err != nil {
		return nil, err
	}
	commit, err := DeserializeShareCommit(data[HeaderBytes:fixed])
	if err != nil {
		return nil, err
	}
	settlement, used, err := DeserializeSettlement(data[fixed:])
	if err != nil {
		return nil, err
	}
	pos := fixed + used
	if len(data)-pos > MaxBlockBytes {
		return nil, ErrBlockTooBig
	}
	if len(data)-pos < 4 {
		return nil, errShort
	}
	count := binary.LittleEndian.Uint32(data[pos : pos+4])
	if count == 0 || count > MaxBlockTxs {
		return nil, ErrTxTooManyItems
	}
	b := &Block{Header: hdr, Share: commit, Settlement: *settlement, Txs: make([]*Tx, 0, count)}
	pos += 4
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
func (b *Block) ValidateBlock(u StateView, params NetParams) error {
	return b.validate(u, params, true)
}

// ValidateCommitted validates what the header commits to: the block with every witness taken out and
// no signature verified. The id of a block covers the transactions without their witnesses, so the
// same id can arrive with witnesses other than the miner's.
//
// It answers one question after ValidateBlock has failed: is the block bad, or only this copy of it? If
// the committed part is invalid, no witness can save the block. If it is valid, the failure came from
// the witnesses, and those the header says nothing about.
func (b *Block) ValidateCommitted(u StateView, params NetParams) error {
	bare := &Block{Header: b.Header, Share: b.Share, Settlement: b.Settlement, Txs: make([]*Tx, len(b.Txs))}
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

func (b *Block) validate(u StateView, params NetParams, signatures bool) error {
	// MaxBlockBytes is the cap on everything but the settlement body, which has its own.
	settlement := len(b.Settlement.Serialize())
	if settlement > MaxSettlementBytes {
		return ErrSettlementTooBig
	}
	if len(b.Serialize())-settlement > MaxBlockBytes {
		return ErrBlockTooBig
	}
	if err := b.Settlement.CheckStructure(); err != nil {
		return err
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
	rules := params.At(height)
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
		if err := tx.CheckRules(rules); err != nil {
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
			fee, values, err := b.checkInputs(tx, u, inBlock, spent, rules, signatures)
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

	if err := b.CheckRoot(); err != nil {
		return err
	}
	// That the commitment is there is a property of the block. Whether the witnesses match it is a
	// property of this copy — and ValidateCommitted has taken them out.
	want, err := b.witnessCommitment()
	if err != nil {
		return err
	}
	if signatures && params.Active(ForkWitnessCommitment, height) {
		if err := b.checkWitnessesAgainst(want); err != nil {
			return err
		}
	}
	return b.checkCoinbase(height, u, params)
}

// CheckRoot verifies that the header's merkle root commits to this block's share commitment and
// transactions. It needs nothing but the block, so it can run the moment a body arrives.
func (b *Block) CheckRoot() error {
	if b.Share.Weight > MaxShareWeight {
		return ErrShareWeight
	}
	if b.Header.MerkleRoot != ShareRoot(BodyRoot(b.Txs, &b.Settlement), &b.Share) {
		return ErrBadMerkle
	}
	return nil
}

// Seal writes the header's merkle root from the share commitment and the transactions. It is
// called once both are final.
func (b *Block) Seal() {
	b.Header.MerkleRoot = ShareRoot(BodyRoot(b.Txs, &b.Settlement), &b.Share)
}

// FinalityKey is the miner's finality key: the block gives it a seat in the quorum
// (the finality spec). A block without one still counts towards the quorum's weight.
func (b *Block) FinalityKey() ([32]byte, bool) {
	return b.Share.FinalityKey, b.Share.HasFinalityKey()
}

// checkInputs verifies one transaction's inputs: their existence, maturity, double spending and
// the conservation of value. It returns the fee.
func (b *Block) checkInputs(tx *Tx, u CoinView, inBlock map[Outpoint]Coin,
	spent map[Outpoint]bool, rules Rules, signatures bool) (int64, []int64, error) {

	height := rules.Height
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
		if err := VerifyInput(tx, i, coins, rules); err != nil {
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

// checkCoinbase verifies the coinbase output by output: the dev fee first, then exactly the
// payouts the credit state `v` the block builds on owes, then the commitment to the witnesses and
// to that state's root. A block pays its miner nothing directly (credits.go), so there is no
// amount left for anybody to choose.
func (b *Block) checkCoinbase(height uint64, v CreditView, params NetParams) error {
	cb := b.Txs[0]
	// The height in the coinbase (what BIP34 does in Bitcoin). Without it the uniqueness of the
	// coinbase txid rests only on a random extranonce — and two blocks with the same extranonce
	// and the same payouts have **the same txid**. Overwriting an unspent output is prevented by
	// `ErrCoinExists`, but once it is fully spent a duplicate txid is acceptable and the
	// transaction index then has two answers to the same question.
	//
	// The first 8 B of the extranonce field are therefore the height, the remaining 24 B stay the
	// miner's.
	if got := binary.LittleEndian.Uint64(cb.Inputs[0].PrevTxID[0:8]); got != height {
		return fmt.Errorf("%w: the coinbase reports height %d, the block is %d", ErrCoinbaseHeight, got, height)
	}
	outs := cb.Outputs
	if devFee := DevFee(height); devFee > 0 {
		// The dev fee has to be the **first** output and has to match exactly.
		if len(outs) == 0 || outs[0].Value != devFee || outs[0].ScriptVersion != ScriptVersionBasic ||
			!equalBytes(outs[0].Script, params.DevFeeScriptAt(height)) {
			return fmt.Errorf("%w: expected %d cogs first", ErrDevFeeMissing, devFee)
		}
		outs = outs[1:]
	}
	payouts, err := CreditPayouts(height, v)
	if err != nil {
		return err
	}
	if len(outs) != len(payouts)+1 {
		return fmt.Errorf("%w: %d outputs after the dev fee, the queue pays %d and the commitment",
			ErrCoinbaseValue, len(outs), len(payouts))
	}
	for i, want := range payouts {
		got := outs[i]
		if got.Value != want.Value || got.ScriptVersion != want.ScriptVersion || !equalBytes(got.Script, want.Script) {
			return fmt.Errorf("%w: payout %d is %d cogs, the queue owes %d to %x",
				ErrCoinbaseValue, i, got.Value, want.Value, want.Script[:6])
		}
	}
	_, creditRoot, err := b.coinbaseCommitment()
	if err != nil {
		return err
	}
	if want := v.CreditsRoot(); creditRoot != want {
		return fmt.Errorf("%w: %x, the state is %x", ErrCreditRoot, creditRoot[:6], want[:6])
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
// A block does not pay its miner: the reward goes into the pot and the miner is paid through
// credits (credits.go). The coinbase pays the dev fee and the `payouts` CreditPayouts names, and
// SealCoinbase adds the commitment. `extra` is the miner's free space (the extranonce).
func BuildCoinbase(height uint64, payouts []TxOut, extra [32]byte, params NetParams) *Tx {
	// The first 8 B are the height (the rule in checkCoinbase), the rest is the miner's extranonce.
	binary.LittleEndian.PutUint64(extra[0:8], height)
	tx := &Tx{Version: 1, Inputs: []TxIn{{PrevTxID: extra, PrevIndex: CoinbaseIndex}}}
	if devFee := DevFee(height); devFee > 0 {
		tx.Outputs = append(tx.Outputs, TxOut{Value: devFee, Script: params.DevFeeScriptAt(height)})
	}
	for _, o := range payouts {
		tx.Outputs = append(tx.Outputs, TxOut{Value: o.Value, ScriptVersion: o.ScriptVersion,
			Script: append([]byte(nil), o.Script...)})
	}
	return tx
}

// SealCoinbase finishes a block's transaction list: it writes the commitment — the witness root
// and `creditRoot`, the root of the credit state the block builds on — into the coinbase's data
// output (txs[0]). It has to be called once the list is final and before the merkle root is
// computed — the output is part of the coinbase's txid.
func SealCoinbase(txs []*Tx, creditRoot [32]byte) {
	if len(txs) == 0 {
		return
	}
	out := TxOut{ScriptVersion: ScriptVersionData, Script: coinbaseCommitmentScript(WitnessMerkleRoot(txs), creditRoot)}
	cb := txs[0]
	// A transaction may have one data output, so one that is already there is taken over.
	if n := len(cb.Outputs); n > 0 && cb.Outputs[n-1].ScriptVersion == ScriptVersionData {
		cb.Outputs[n-1] = out
		return
	}
	cb.Outputs = append(cb.Outputs, out)
}
