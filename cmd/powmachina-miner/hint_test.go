package main

import (
	"net"
	"strings"
	"testing"

	"powmachina/internal/chain"
	"powmachina/internal/nodeclient"
)

// A first solo start usually meets a node without its RPC. The error then has to say how the
// node is started, with the port of the network the miner was started for.
func TestUnreachableNodeSaysHowToStartOne(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	_, err = nodeclient.New("http://"+addr, "").Network()
	if err == nil {
		t.Fatal("a closed port answered")
	}
	hint := nodeHint(err, chain.TestNet)
	if !strings.Contains(hint, "powmachinad -testnet -rpc 127.0.0.1:17697") {
		t.Fatalf("the hint for testnet: %q", hint)
	}
	if hint := nodeHint(err, chain.MainNet); !strings.Contains(hint, "powmachinad -rpc 127.0.0.1:7697") {
		t.Fatalf("the hint for mainnet: %q", hint)
	}

	// A node that answered and said something else is not a node that is missing.
	if hint := nodeHint(&net.OpError{Op: "read"}, chain.TestNet); hint != "" {
		t.Fatalf("a hint for an error that is not a refused connection: %q", hint)
	}
}
