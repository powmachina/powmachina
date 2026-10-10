package p2p

import (
	"net"
	"sync"
	"testing"
	"time"
)

// connPair returns two connections over one TCP pair.
func connPair(t *testing.T) (*Conn, *Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	magic := [4]byte{'P', 'W', 'M', 'R'}

	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	dialed, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	got := <-ch
	if got.err != nil {
		t.Fatal(got.err)
	}
	a, b := NewConn(dialed, magic), NewConn(got.c, magic)
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

// A ping has to arrive in a form the other side can pair, and a pong has to close it.
func TestPingPongRoundTrip(t *testing.T) {
	a, b := connPair(t)
	now := time.Now()

	if err := a.Ping(now); err != nil {
		t.Fatalf("the ping failed: %v", err)
	}
	msg, err := b.Recv()
	if err != nil {
		t.Fatalf("the ping did not arrive: %v", err)
	}
	if msg.Command != CmdPing {
		t.Fatalf("expected a ping, got %q", msg.Command)
	}
	if len(msg.Payload) != PingPayloadSize {
		t.Fatalf("the ping has %d B, expected %d", len(msg.Payload), PingPayloadSize)
	}

	// Until the answer arrives, a second ping is not sent — the time is measured from the first one.
	if err := a.Ping(now.Add(PingInterval)); err != nil {
		t.Fatalf("the second ping failed: %v", err)
	}
	if a.PingOverdue(now.Add(PingInterval)) {
		t.Fatal("the ping should not be overdue yet")
	}
	if !a.PingOverdue(now.Add(PingTimeout + time.Second)) {
		t.Fatal("an unanswered ping should have been reported after PingTimeout")
	}

	if err := a.Pong(msg.Payload, now.Add(time.Second)); err != nil {
		t.Fatalf("the pong was not paired: %v", err)
	}
	if a.PingOverdue(now.Add(2 * PingTimeout)) {
		t.Fatal("after the pong nothing is being waited for any more")
	}
	// Once it is closed, a new ping may be sent.
	if err := a.Ping(now.Add(PingInterval)); err != nil {
		t.Fatalf("a further ping failed: %v", err)
	}
	if _, err := b.Recv(); err != nil {
		t.Fatalf("a further ping did not arrive: %v", err)
	}
}

// A pong that belongs to nothing is an error — it is only sent on request.
func TestPongWithoutPingIsError(t *testing.T) {
	a, _ := connPair(t)
	now := time.Now()

	if err := a.Pong([]byte{1, 2, 3, 4, 5, 6, 7, 8}, now); err != ErrBadPong {
		t.Fatalf("an unsolicited pong should have been an error, got %v", err)
	}
	if err := a.Ping(now); err != nil {
		t.Fatal(err)
	}
	if err := a.Pong([]byte{9, 9, 9, 9, 9, 9, 9, 9}, now); err != ErrBadPong {
		t.Fatalf("a pong with a foreign nonce should have been an error, got %v", err)
	}
	if err := a.Pong([]byte{1, 2, 3}, now); err != ErrBadPong {
		t.Fatalf("a pong of the wrong length should have been an error, got %v", err)
	}
	// None of them may close the wait for a real answer.
	if !a.PingOverdue(now.Add(PingTimeout + time.Second)) {
		t.Fatal("a foreign pong closed the wait for our ping")
	}
}

// Several goroutines write into one connection (the ping loop, announcements, answers). The frames
// must not be interleaved — a peer would get one message's header with another's body.
func TestConcurrentSendKeepsFramesWhole(t *testing.T) {
	a, b := connPair(t)
	const senders, each = 8, 10

	payload := make([]byte, 512)
	for i := range payload {
		payload[i] = byte(i)
	}

	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				if err := a.Send(&Message{Command: CmdInv, Payload: payload}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}

	for i := 0; i < senders*each; i++ {
		msg, err := b.Recv()
		if err != nil {
			t.Fatalf("message %d cannot be read: %v", i, err)
		}
		if msg.Command != CmdInv || len(msg.Payload) != len(payload) {
			t.Fatalf("message %d is corrupted: %q, %d B", i, msg.Command, len(msg.Payload))
		}
		for k, v := range msg.Payload {
			if v != payload[k] {
				t.Fatalf("message %d has a mixed-up body at byte %d", i, k)
			}
		}
	}
	wg.Wait()
}
