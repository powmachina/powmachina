package store

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"

	"powmachina/internal/chain"
)

// What a block's credit step costs as the credit state grows: it reads every entry, plans the
// payouts over all of them and hashes the whole tree (chain.CreditStep). Run with
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
			commit := &chain.ShareCommit{Payout: [32]byte{0xAB}, Weight: 3}
			b.ResetTimer()
			for range b.N {
				if _, err := chain.CreditStep(height+1, commit, nil, 0, s); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
