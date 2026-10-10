package wallet

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"powmachina/internal/chain"
)

// Balance is the wallet's state.
type Balance struct {
	Spendable int64 // confirmed (≥ minConf), matured, unreserved
	Immature  int64 // a coinbase before maturity
	Pending   int64 // coins reserved by our unconfirmed transaction
	Incoming  int64 // our own unconfirmed change — spendable right away (bitcoind: "trusted")
}

// Total is everything that belongs to the wallet or will come back to it.
func (b Balance) Total() int64 { return b.Spendable + b.Immature + b.Incoming }

// Available is what can be sent right now: the confirmed coins + our own unconfirmed change.
func (b Balance) Available() int64 { return b.Spendable + b.Incoming }

// Balance computes the balance. `minConf` is the minimum number of confirmations (1 = right after a block).
func (w *Wallet) Balance(minConf uint64) Balance {
	w.mu.Lock()
	defer w.mu.Unlock()
	var b Balance
	for _, c := range w.coins {
		switch {
		case c.spent():
		case c.pending():
			b.Pending += c.Value
		case c.Height == 0:
			b.Incoming += c.Value
		case !w.matureLocked(c):
			b.Immature += c.Value
		case w.confLocked(c.Height) >= max(minConf, 1):
			b.Spendable += c.Value
		}
	}
	return b
}

func (w *Wallet) confLocked(height uint64) uint64 {
	if height == 0 || height > w.height {
		return 0
	}
	return w.height - height + 1
}

// UnspentCoin is a coin for listing (listunspent).
type UnspentCoin struct {
	Coin
	Address       string
	Label         string
	Confirmations uint64
	Spendable     bool
}

// Unspent returns the unspent coins with confirmations in a range. It leaves out immature block rewards
// (like bitcoind's listunspent) — whoever put them into a transaction would have the node refuse it.
func (w *Wallet) Unspent(minConf, maxConf uint64) []UnspentCoin {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []UnspentCoin
	for _, c := range w.coins {
		if c.spent() || !w.matureLocked(c) {
			continue
		}
		conf := w.confLocked(c.Height)
		if conf < minConf || conf > maxConf {
			continue
		}
		a := w.encode(c.PubKey)
		out = append(out, UnspentCoin{Coin: *c, Address: a, Label: w.labels[a], Confirmations: conf,
			Spendable: !c.pending() && w.matureLocked(c)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Height != out[j].Height {
			return out[i].Height > out[j].Height
		}
		return out[i].Address < out[j].Address
	})
	return out
}

// TxView is a transaction with its confirmations, for listing.
type TxView struct {
	Tx
	Confirmations uint64
}

// Transactions returns the history from the newest. The unconfirmed ones come first.
func (w *Wallet) Transactions() []TxView {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]TxView, 0, len(w.txs))
	for _, t := range w.txs {
		out = append(out, TxView{Tx: *t, Confirmations: w.confLocked(t.Height)})
	}
	sort.Slice(out, func(i, j int) bool {
		hi, hj := out[i].Height, out[j].Height
		if hi == 0 || hj == 0 {
			if hi != hj {
				return hi == 0
			}
			return out[i].Time > out[j].Time
		}
		if hi != hj {
			return hi > hj
		}
		return string(out[i].ID[:]) < string(out[j].ID[:])
	})
	return out
}

// Transaction returns one transaction of the wallet.
func (w *Wallet) Transaction(id [32]byte) (TxView, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.txs[id]
	if !ok {
		return TxView{}, false
	}
	return TxView{Tx: *t, Confirmations: w.confLocked(t.Height)}, true
}

// Status is the sync state.
type Status struct {
	Synced    bool
	Height    uint64
	Tip       [32]byte
	Birth     uint64
	Addresses uint32
	Imported  int
}

func (w *Wallet) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return Status{Synced: w.synced, Height: w.height, Tip: w.tip, Birth: w.birth,
		Addresses: w.nextRecv, Imported: len(w.imported)}
}

// BlockAt returns the ID of the block at a given height if the wallet remembers it (listsinceblock).
func (w *Wallet) BlockAt(h uint64) ([32]byte, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := len(w.recent) - 1; i >= 0; i-- {
		if w.recent[i].Height == h {
			return w.recent[i].ID, true
		}
	}
	return [32]byte{}, false
}

// HeightOf returns a block's height by its ID if the wallet remembers it.
func (w *Wallet) HeightOf(id [32]byte) (uint64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range w.recent {
		if r.ID == id {
			return r.Height, true
		}
	}
	return 0, false
}

// ------------------------------------------------------------------ sums

// FormatAmount shows an amount in POWM with eight decimal places — in integers, without a float.
func FormatAmount(cogs int64) string {
	sign := ""
	if cogs < 0 {
		sign, cogs = "-", -cogs
	}
	return fmt.Sprintf("%s%d.%08d", sign, cogs/chain.POWM, cogs%chain.POWM)
}

var ErrBadAmount = errors.New("wallet: invalid amount (POWM, up to 8 decimals)")

// ParseAmount reads an amount in POWM ("1", "0.5", "12.34567890") into cogs. Exactly, without a float:
// the amount 0.1 is 10 000 000 cogs, not 9 999 999.
func ParseAmount(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		return 0, ErrBadAmount
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if hasDot && (frac == "" || len(frac) > 8) {
		return 0, ErrBadAmount
	}
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return 0, ErrBadAmount
		}
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > chain.MaxMoney/chain.POWM {
		return 0, ErrBadAmount
	}
	f := int64(0)
	if frac != "" {
		frac += strings.Repeat("0", 8-len(frac))
		if f, err = strconv.ParseInt(frac, 10, 64); err != nil {
			return 0, ErrBadAmount
		}
	}
	v := w*chain.POWM + f
	if v > chain.MaxMoney {
		return 0, ErrBadAmount
	}
	return v, nil
}
