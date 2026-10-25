package store

import (
	"context"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"

	"powmachina/internal/chain"
)

// What a block's credit step costs as the credit state grows. It reads the entries it changes,
// the front of the payout queue and the tree's nodes on the changed paths, so it should barely
// move with the size of the state (chain.CreditStep). Run with
//
//	go test ./internal/store -run '^$' -bench CreditStep -benchtime 3x
func BenchmarkCreditStep(b *testing.B) {
	for _, n := range []int{10_000, 100_000, 1_000_000} {
		b.Run(fmt.Sprintf("keys=%d", n), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "db"))
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			const height = 1_000_000
			batch := s.db.NewBatch()
			for i := range n {
				var p [32]byte
				binary.LittleEndian.PutUint64(p[:], uint64(i)*0x9E3779B97F4A7C15)
				// Mostly waiting below the payout, a few ready: the shape of a live network.
				c := chain.Credit{Amount: 1_000_000, LastShare: height - uint64(i%2000), PaidAt: height - 100}
				if i%50 == 0 {
					c.Amount, c.PaidAt, c.ReadyAt = 10_000_000, height-3000, height-10
				}
				v := c.Serialize()
				if err := batch.Set(keyCredit(p), v[:], nil); err != nil {
					b.Fatal(err)
				}
			}
			if err := batch.Commit(pebble.Sync); err != nil {
				b.Fatal(err)
			}
			// The pot with the state's root, and the indexes built as an older directory gets them.
			state := map[[32]byte]chain.Credit{}
			if err := s.ForEachCredit(func(p [32]byte, c chain.Credit) error { state[p] = c; return nil }); err != nil {
				b.Fatal(err)
			}
			if err := s.db.Set(keyPot, encodePot(0, chain.CreditsRoot(0, state)), pebble.Sync); err != nil {
				b.Fatal(err)
			}
			if err := s.db.Delete(keyCreditIndexed, pebble.Sync); err != nil {
				b.Fatal(err)
			}
			if err := s.ensureCreditIndex(); err != nil {
				b.Fatal(err)
			}
			// Settle the writes, as on a node that has been running: no compaction under the timer.
			if err := s.db.Compact(context.Background(), []byte{0}, []byte{0xff, 0xff}, true); err != nil {
				b.Fatal(err)
			}
			// A block that settles 200 shares of 200 miners, a few of them new.
			commit := &chain.ShareCommit{Payout: [32]byte{0xAB}, Weight: 3}
			st := &chain.Settlement{}
			for i := range 200 {
				var p [32]byte
				binary.LittleEndian.PutUint64(p[:], uint64(i*37)*0x9E3779B97F4A7C15)
				if i%20 == 0 {
					p[31] = 0xEE
				}
				st.Keys = append(st.Keys, chain.SettleKey{Payout: p})
				st.Jobs = append(st.Jobs, chain.SettleJob{Key: uint16(i), Weight: 1})
				st.Shares = append(st.Shares, chain.SettleShare{Job: uint16(i)})
			}
			b.ResetTimer()
			for range b.N {
				if _, err := chain.CreditStep(height+1, commit, st, 0, s); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
