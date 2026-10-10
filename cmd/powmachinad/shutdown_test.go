package main

import (
	"bytes"
	"errors"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"powmachina/internal/chain"
	"powmachina/internal/mh"
	"powmachina/internal/p2p"
)

// Close has to wait for the maintenance loops. If it closed storage earlier, the next tick of
// the body driver would panic on "pebble: closed" — and under load during shutdown that is a
// common state.
func TestCloseWaitsForLoops(t *testing.T) {
	dir := t.TempDir()
	node, _, err := OpenNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.MineBlocks(2); err != nil {
		t.Fatal(err)
	}

	// The driver runs so often that at closing time it is almost certainly mid-tick.
	stop := make(chan struct{})
	defer close(stop)
	for i := 0; i < 4; i++ {
		node.startLoop(stop, time.Millisecond, func() { node.SyncBodiesOnce(time.Now()) })
	}
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		node.Close() // must neither panic nor hang
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not finish — it did not wait for the loops or it deadlocked")
	}

	// A repeated Close has no consequences.
	node.Close()
}

// Close has to wait for the **connection** goroutines too, not only the maintenance loops.
// Those are created for every connection and reach into storage and into the epoch cache: a
// peer that just sent headers ends up in `pow()` → `epochs.forHeight`, and after
// `epochs.close()` that writes into a freed map — `panic: assignment to entry in nil map`.
func TestCloseWaitsForConnectionGoroutines(t *testing.T) {
	node, addr, cleanup := listeningNode(t)
	defer cleanup() // Close is idempotent, so a second close has no consequences
	conn, err := p2p.Dial(net.JoinHostPort(addr.IP.String(), strconv.Itoa(int(addr.Port))),
		chain.RegTest.Magic)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ver := &p2p.VersionMsg{Protocol: p2p.ProtocolVersion, Timestamp: time.Now().Unix(),
		Nonce: 0xAB01, UserAgent: "/test/"}
	if _, err := conn.Handshake(ver, 0xAB01, true); err != nil {
		t.Fatal(err)
	}
	// Wait until the connection is really registered — otherwise the test would be closing a
	// node that has no connection goroutine yet and would verify nothing.
	deadline := time.Now().Add(3 * time.Second)
	for {
		node.mu.Lock()
		live := len(node.live)
		node.mu.Unlock()
		if live > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the connection did not register")
		}
		time.Sleep(10 * time.Millisecond)
	}

	done := make(chan struct{})
	go func() {
		node.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// Without closing the connections, Close would wait for ReadTimeout (90 s).
		t.Fatal("Close did not finish — it did not wait for the connection goroutine or it hangs on a read")
	}

	node.mu.Lock()
	live := len(node.live)
	node.mu.Unlock()
	if live != 0 {
		t.Fatalf("%d tracked connections remained after Close", live)
	}
}

// After Close the epoch cache must not panic. It is a second layer on top of Close waiting for
// the connection goroutines: if a path were found that is not waited for, it should return an
// error rather than take the whole process down.
func TestEpochsAfterCloseReturnError(t *testing.T) {
	node := NewNode(chain.RegTest, chain.RegtestGenesis(), mh.TestProfile)
	node.Close()

	if _, err := node.epochs.forHeight(0); !errors.Is(err, errEpochsClosed) {
		t.Fatalf("after Close expected errEpochsClosed, got %v", err)
	}
	if _, err := node.pow(0); err == nil {
		t.Fatal("pow() after Close should return an error")
	}
	// Neither a new goroutine nor a new connection may register after Close — otherwise they
	// would race with Wait and the WaitGroup would be done before they start.
	if node.goTracked(func() {}) {
		t.Error("goTracked returned true after Close")
	}
}

// `powmachinad -regtest` with no further flags has to **keep running**, not exit. A node that
// ran to the end of the function when it had neither `-listen` nor `-rpc` would exit on exactly
// the command everybody types the first time (`powmachinad -testnet`), and that looks like a
// crash with no error message.
//
// The test runs the **real binary** as a process. Checking the same condition inside a test
// would be testing a copy of the logic — and a copy can be right while the binary is wrong.
func TestBinaryKeepsRunningWithoutListenerOrRPC(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "powmachinad")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("the build failed: %v\n%s", err, out)
	}

	// Regtest, so the test does not go onto the network: no DNS seeds, no outbound connections.
	cmd := exec.Command(bin, "-regtest", "-datadir", t.TempDir())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// If the node exited by itself, Wait finishes before the time is up.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("the node exited by itself (%v) — without -listen and -rpc it should keep running:\n%s", err, out.String())
	case <-time.After(5 * time.Second):
	}

	// And on SIGTERM it has to exit cleanly rather than hang.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the node did not exit on SIGTERM")
	}
	if !strings.Contains(out.String(), "shutting down") {
		t.Errorf("the shutdown line is missing from the log:\n%s", out.String())
	}
}
