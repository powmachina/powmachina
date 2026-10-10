// mh-vectors — generator of the MachinaHash test vectors.
//
// The vectors are the algorithm's contract: the GPU kernel, the node and any other miner have
// to agree with them bit for bit. They may be regenerated ONLY on a deliberate change of the
// algorithm — never to make "the test pass".
//
//	mh-vectors [output file] [count]
//
// Lane C is computed in RandomX light mode (the cache, ~256 MiB). That is slower, but it has to
// give the same results as fast — internal/randomx/randomx_conformance_test.go verifies that.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"

	"github.com/zeebo/blake3"

	"powmachina/internal/mh"
)

func main() {
	path := "vectors/mh-test-vectors.txt"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	count := 1000
	if len(os.Args) > 2 {
		n, err := strconv.Atoi(os.Args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad count: %v\n", err)
			os.Exit(2)
		}
		count = n
	}

	v := mh.NewVerifier(0, mh.TestProfile)
	defer v.Close()
	rx, err := mh.NewRandomX(0, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "RandomX context (light, ~256 MiB): %v\n", err)
		os.Exit(1)
	}
	defer rx.Close()
	h, err := mh.NewHasherWithRandomX(rx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer h.Close()

	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	w := bufio.NewWriter(f)

	fmt.Fprintln(w, "# MachinaHash test vectors")
	fmt.Fprintln(w, "# lane C = MachinaHash-C (RandomX v2, PowMachina salt), epoch 0 key")
	fmt.Fprintln(w, "# lane G = MachinaHash-G, profile TestDag(epoch 0) — cache 1 MiB, dataset 8 MiB")
	fmt.Fprintln(w, "# format: <lane> <header_hex(80B)> <nonce_dec> <hash_hex(32B)>")

	for i := range count {
		lane := mh.LaneC
		if i%2 == 1 {
			lane = mh.LaneG
		}
		// A deterministic but irregular header.
		var header [mh.HeaderBytes]byte
		var iLE [8]byte
		binary.LittleEndian.PutUint64(iLE[:], uint64(i))
		hb := blake3.New()
		_, _ = hb.WriteString("mh-vector")
		_, _ = hb.Write(iLE[:])
		if _, err := hb.Digest().Read(header[:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		nonce := uint64(i) * 0x9E3779B97F4A7C15

		hash, err := h.Hash(v, &header, nonce, lane)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintf(w, "%s %s %d %s\n",
			lane, hex.EncodeToString(header[:]), nonce, hex.EncodeToString(hash[:]))
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "%d vectors → %s\n", count, path)
}
