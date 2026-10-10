package p2p

import (
	"testing"
	"time"
)

func sampleVersion() *VersionMsg {
	return &VersionMsg{
		Protocol: ProtocolVersion, Services: ServiceFullNode | ServiceFullDag,
		Timestamp: 1_700_000_000, Nonce: 0xabcdef, UserAgent: "/powmachinad:0.1/",
		StartHeight: 12345, Relay: true,
	}
}

func TestVersionRoundTrip(t *testing.T) {
	v := sampleVersion()
	raw, err := v.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeVersion(raw)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *v {
		t.Fatalf("the round-trip changed the version message:\n%+v\n%+v", got, v)
	}
}

// A connection to ourselves has to be detected — otherwise the node takes its own slot.
func TestSelfConnectionDetected(t *testing.T) {
	p := NewPeer(addr("5.9.100.1", 8333), PeerOutboundFull)
	v := sampleVersion()
	v.Nonce = 0x4242
	if err := p.CompleteHandshake(v, 0x4242); err != ErrSelfConnection {
		t.Fatalf("expected ErrSelfConnection, got %v", err)
	}
	if p.HandshakeDone() {
		t.Fatal("the handshake must not have been completed")
	}
}

func TestOldProtocolRejected(t *testing.T) {
	p := NewPeer(addr("5.9.100.1", 8333), PeerOutboundFull)
	v := sampleVersion()
	v.Protocol = MinProtocolVersion - 1
	if err := p.CompleteHandshake(v, 1); err != ErrOldProtocol {
		t.Fatalf("expected ErrOldProtocol, got %v", err)
	}
}

func TestVersionRejectsMalformed(t *testing.T) {
	good, _ := sampleVersion().Encode()

	cases := map[string][]byte{
		"empty":          {},
		"truncated":      good[:20],
		"leftover bytes": append(append([]byte{}, good...), 0xff),
		"user agent over the limit": func() []byte {
			v := sampleVersion()
			v.UserAgent = string(make([]byte, MaxUserAgentBytes+1))
			raw, err := v.Encode()
			if err != nil {
				return []byte{1} // Encode refused it correctly
			}
			return raw
		}(),
	}
	for name, raw := range cases {
		if _, err := DecodeVersion(raw); err == nil {
			t.Errorf("%s: a malformed version passed", name)
		}
	}

	// A binary user agent is forbidden — it is text, not room for data.
	v := sampleVersion()
	v.UserAgent = "ab"
	raw, _ := v.Encode()
	raw[29] = 0x01
	if _, err := DecodeVersion(raw); err != ErrBadVersionMsg {
		t.Errorf("a binary user agent should be refused, got %v", err)
	}

	// The relay byte has only two valid values.
	raw2, _ := sampleVersion().Encode()
	raw2[len(raw2)-1] = 2
	if _, err := DecodeVersion(raw2); err != ErrBadVersionMsg {
		t.Errorf("an invalid relay byte should be refused, got %v", err)
	}
}

// Flooding with addresses is the cheapest preparation for an eclipse — it has to be limited.
func TestAddrRateLimit(t *testing.T) {
	p := NewPeer(addr("5.9.100.1", 8333), PeerInbound)
	now := time.Now()

	if got := p.AllowAddrs(addrBurst, now); got != addrBurst {
		t.Fatalf("the first batch should pass in full, %d passed", got)
	}
	if got := p.AllowAddrs(500, now); got != 0 {
		t.Fatalf("right after exhaustion nothing should pass, %d passed", got)
	}
	// After an hour ~360 addresses are refilled.
	later := now.Add(time.Hour)
	got := p.AllowAddrs(1000, later)
	if got < 300 || got > 400 {
		t.Fatalf("after an hour I expect ~360 addresses, %d passed", got)
	}
}

// The handshake reads bytes from a foreign peer as the very first thing.
func FuzzDecodeVersion(f *testing.F) {
	raw, _ := sampleVersion().Encode()
	f.Add(raw)
	f.Add([]byte{})
	f.Add(make([]byte, 34))

	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := DecodeVersion(data)
		if err != nil {
			return
		}
		if len(v.UserAgent) > MaxUserAgentBytes {
			t.Fatalf("a user agent over the limit was accepted: %d", len(v.UserAgent))
		}
		back, err := v.Encode()
		if err != nil {
			t.Fatalf("an accepted message cannot be encoded back: %v", err)
		}
		if len(back) != len(data) {
			t.Fatalf("the round-trip changed the length: %d → %d", len(data), len(back))
		}
	})
}
