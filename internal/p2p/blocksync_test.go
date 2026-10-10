package p2p

import (
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
)

// mineFullBlock mines a block with its body (the coinbase) and adds it to the chain and the UTXO set.
func mineFullBlock(t *testing.T, hc *chain.HeaderChain, u *chain.UTXOSet, lane mh.Lane,
	minerScript []byte, hasher *mh.Hasher, v *mh.Verifier) *chain.Block {
	t.Helper()
	params := chain.RegTest

	tip, height, tipID := hc.Tip()
	bits, err := hc.NextBitsFor(lane)
	if err != nil {
		t.Fatal(err)
	}
	var extra [32]byte
	extra[0] = byte(height + 1)
	cb := chain.BuildCoinbase(height+1, 0, minerScript, extra, params)

	blk := &chain.Block{
		Header: &chain.Header{Version: 1, PrevBlock: tipID, Bits: bits,
			Height: uint32(height + 1), Lane: lane, Time: tip.Time + 130},
		Txs: []*chain.Tx{cb},
	}
	if mtp := hc.MedianTimePast(); blk.Header.Time <= mtp {
		blk.Header.Time = mtp + 1
	}
	blk.Header.MerkleRoot = chain.TxMerkleRoot(blk.Txs)

	for nonce := uint64(0); nonce < 200_000; nonce++ {
		blk.Header.Nonce = nonce
		ok, err := blk.Header.CheckPoW(hasher, v)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			if _, err := hc.AddHeader(blk.Header, chain.PoWPair{Hasher: hasher, Verifier: v}); err != nil {
				t.Fatalf("our own block's header did not pass: %v", err)
			}
			if err := blk.ValidateBlock(u, params); err != nil {
				t.Fatalf("our own block's body did not pass: %v", err)
			}
			if _, err := u.Apply(blk); err != nil {
				t.Fatal(err)
			}
			return blk
		}
	}
	t.Fatal("no nonce found")
	return nil
}

// The whole chain: two nodes exchange headers, then block bodies, and the receiving side verifies
// everything and writes it into its own UTXO set.
func TestBlockBodiesSyncBetweenNodes(t *testing.T) {
	params := chain.RegTest
	verifier := mh.NewVerifier(0, mh.TestProfile)
	defer verifier.Close()
	hasher, closeHasher := testHasher(t)
	defer closeHasher()

	sk := new([32]byte)
	for i := range sk {
		sk[i] = byte(i + 1)
	}
	pk, err := mh.SchnorrPubkey(sk)
	if err != nil {
		t.Fatal(err)
	}

	// Node A: a chain with bodies.
	chainA := chain.NewHeaderChain(params, regtestGenesis(), nil)
	utxoA := chain.NewUTXOSet()
	blocks := map[[32]byte]*chain.Block{}
	for _, lane := range []mh.Lane{mh.LaneG, mh.LaneG, mh.LaneC} {
		b := mineFullBlock(t, chainA, utxoA, lane, pk[:], hasher, verifier)
		blocks[b.Header.ID()] = b
	}
	_, heightA, _ := chainA.Tip()

	// Node B: the genesis only.
	chainB := chain.NewHeaderChain(params, regtestGenesis(), nil)
	utxoB := chain.NewUTXOSet()

	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		conn := NewConn(c, params.Magic)
		defer conn.Close()
		ver := &VersionMsg{Protocol: ProtocolVersion, Timestamp: time.Now().Unix(),
			Nonce: 1, UserAgent: "/A/", StartHeight: uint32(heightA)}
		if _, err := conn.Handshake(ver, 1, false); err != nil {
			srvErr <- err
			return
		}
		for {
			msg, err := conn.Recv()
			if err != nil {
				srvErr <- nil
				return
			}
			switch msg.Command {
			case CmdGetHeaders:
				loc, err := DecodeGetHeaders(msg.Payload)
				if err != nil {
					srvErr <- err
					return
				}
				payload, err := EncodeHeaders(chainA.HeadersFrom(loc, MaxHeadersPerMessage))
				if err != nil {
					srvErr <- err
					return
				}
				if err := conn.Send(&Message{Command: CmdHeaders, Payload: payload}); err != nil {
					srvErr <- err
					return
				}
			case CmdGetData:
				items, err := DecodeGetData(msg.Payload)
				if err != nil {
					srvErr <- err
					return
				}
				for _, it := range items {
					b, ok := blocks[it.ID]
					if !ok {
						continue
					}
					payload, err := EncodeBlock(b)
					if err != nil {
						srvErr <- err
						return
					}
					if err := conn.Send(&Message{Command: CmdBlock, Payload: payload}); err != nil {
						srvErr <- err
						return
					}
				}
			}
		}
	}()

	conn, err := Dial(ln.Addr().String(), params.Magic)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ver := &VersionMsg{Protocol: ProtocolVersion, Timestamp: time.Now().Unix(),
		Nonce: 2, UserAgent: "/B/"}
	if _, err := conn.Handshake(ver, 2, true); err != nil {
		t.Fatal(err)
	}

	// 1) the headers
	locPayload, err := EncodeGetHeaders(chainB.Locator())
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(&Message{Command: CmdGetHeaders, Payload: locPayload}); err != nil {
		t.Fatal(err)
	}
	msg, err := conn.Recv()
	if err != nil {
		t.Fatal(err)
	}
	headers, err := DecodeHeaders(msg.Payload)
	if err != nil {
		t.Fatal(err)
	}
	wanted := make([][32]byte, 0, len(headers))
	for _, h := range headers {
		if _, err := chainB.AddHeader(h, chain.PoWPair{Hasher: hasher, Verifier: verifier}); err != nil {
			t.Fatalf("header %d did not pass: %v", h.Height, err)
		}
		wanted = append(wanted, h.ID())
	}

	// 2) the block bodies — only after the headers are verified
	getData, err := EncodeGetData(BlockItems(wanted))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(&Message{Command: CmdGetData, Payload: getData}); err != nil {
		t.Fatal(err)
	}
	for range wanted {
		msg, err := conn.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if msg.Command != CmdBlock {
			t.Fatalf("expected a block, got %q", msg.Command)
		}
		blk, err := DecodeBlock(msg.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if !chainB.Has(blk.Header.ID()) {
			t.Fatal("we received the body of a block whose header we do not have")
		}
		if err := blk.ValidateBlock(utxoB, params); err != nil {
			t.Fatalf("the body of block %d failed validation: %v", blk.Header.Height, err)
		}
		if _, err := utxoB.Apply(blk); err != nil {
			t.Fatal(err)
		}
	}

	if utxoA.Len() != utxoB.Len() {
		t.Fatalf("the UTXO sets differ: A has %d, B has %d", utxoA.Len(), utxoB.Len())
	}
	_, heightB, _ := chainB.Tip()
	if heightB != heightA {
		t.Fatalf("the heights differ: A=%d B=%d", heightA, heightB)
	}
	t.Logf("3 blocks transferred with their bodies; both nodes have %d unspent outputs",
		utxoB.Len())
}
