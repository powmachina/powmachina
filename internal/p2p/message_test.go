package p2p

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

var testMagic = [4]byte{'P', 'W', 'M', '1'}

func TestMessageRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	msg := &Message{Command: CmdHeaders, Payload: []byte("some data")}
	if err := Encode(&buf, testMagic, msg); err != nil {
		t.Fatal(err)
	}
	got, err := Decode(&buf, testMagic)
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != msg.Command || !bytes.Equal(got.Payload, msg.Payload) {
		t.Fatalf("the round-trip changed the message: %+v", got)
	}
}

func TestMessageRejectsForeignNetwork(t *testing.T) {
	var buf bytes.Buffer
	Encode(&buf, [4]byte{'X', 'X', 'X', 'X'}, &Message{Command: CmdPing})
	if _, err := Decode(&buf, testMagic); err != ErrBadMagic {
		t.Fatalf("a message from another network should give ErrBadMagic, it gave %v", err)
	}
}

// A peer must not be able to force a large allocation just by writing a large number into the header.
func TestMessageRejectsOversizeBeforeAllocating(t *testing.T) {
	var hdr [HeaderSize]byte
	copy(hdr[0:4], testMagic[:])
	copy(hdr[4:16], "block")
	binary.LittleEndian.PutUint32(hdr[16:20], 0xffffffff) // 4 GB

	// There is nothing after the header — if the node allocated by the number, it would die here.
	if _, err := Decode(bytes.NewReader(hdr[:]), testMagic); err != ErrTooLarge {
		t.Fatalf("an absurd length should give ErrTooLarge, it gave %v", err)
	}
	if err := Encode(io.Discard, testMagic, &Message{Command: CmdBlock,
		Payload: make([]byte, MaxMessageBytes+1)}); err != ErrTooLarge {
		t.Fatalf("sending has a cap too, got %v", err)
	}
}

func TestMessageRejectsBadChecksum(t *testing.T) {
	var buf bytes.Buffer
	Encode(&buf, testMagic, &Message{Command: CmdPing, Payload: []byte("hello")})
	raw := buf.Bytes()
	raw[len(raw)-1] ^= 0xff
	if _, err := Decode(bytes.NewReader(raw), testMagic); err != ErrBadChecksum {
		t.Fatalf("a corrupted payload should give ErrBadChecksum, it gave %v", err)
	}
}

// A message name has to have exactly one valid form — otherwise the same message would have several.
func TestCommandValidation(t *testing.T) {
	bad := [][12]byte{
		{},                           // all zeros
		{'p', 'i', 'n', 'g', 0, 'x'}, // data after the terminating zero
		{'p', 'i', 0x01, 'g'},        // a control character
		{0xff, 0xfe},                 // outside ASCII
	}
	for i, raw := range bad {
		if _, err := decodeCommand(raw); err != ErrBadCommand {
			t.Errorf("case %d should have been refused, got %v", i, err)
		}
	}
	if _, err := encodeCommand(""); err != ErrBadCommand {
		t.Error("an empty name must not be encodable")
	}
	if _, err := encodeCommand("far-too-long-name"); err != ErrBadCommand {
		t.Error("a too long name must not be encodable")
	}
}

func TestTruncatedPayload(t *testing.T) {
	var buf bytes.Buffer
	Encode(&buf, testMagic, &Message{Command: CmdBlock, Payload: make([]byte, 100)})
	raw := buf.Bytes()[:HeaderSize+50]
	if _, err := Decode(bytes.NewReader(raw), testMagic); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a truncated payload should give ErrUnexpectedEOF, it gave %v", err)
	}
}

func TestItemCountLimits(t *testing.T) {
	if err := CheckItemCount(MaxHeadersPerMessage, MaxHeadersPerMessage); err != nil {
		t.Fatalf("exactly at the cap it should pass: %v", err)
	}
	if err := CheckItemCount(MaxHeadersPerMessage+1, MaxHeadersPerMessage); err == nil {
		t.Fatal("over the cap it has to fail")
	}
	if err := CheckItemCount(-1, MaxAddrPerMessage); err == nil {
		t.Fatal("a negative count has to fail")
	}
}

// The node reads frames from foreign peers — it must not crash on any byte.
func FuzzDecodeMessage(f *testing.F) {
	var buf bytes.Buffer
	Encode(&buf, testMagic, &Message{Command: CmdInv, Payload: []byte("x")})
	f.Add(buf.Bytes())
	f.Add([]byte{})
	f.Add(make([]byte, HeaderSize))

	f.Fuzz(func(t *testing.T, data []byte) {
		msg, err := Decode(bytes.NewReader(data), testMagic)
		if err != nil {
			return
		}
		if len(msg.Payload) > MaxMessageBytes {
			t.Fatalf("a payload over the limit was accepted: %d", len(msg.Payload))
		}
		if msg.Command == "" || len(msg.Command) > 12 {
			t.Fatalf("an invalid message name %q was accepted", msg.Command)
		}
		// What passed decoding has to encode back to the same bytes.
		var out bytes.Buffer
		if err := Encode(&out, testMagic, msg); err != nil {
			t.Fatalf("an accepted message cannot be encoded back: %v", err)
		}
		if !bytes.Equal(out.Bytes(), data[:HeaderSize+len(msg.Payload)]) {
			t.Fatalf("the round-trip changed the bytes of message %q", msg.Command)
		}
	})
}

// Inventory messages come from foreign peers and the node reads them first.
// An off-by-one in this decoder is enough to take the node down, hence the fuzz.
func FuzzDecodeInv(f *testing.F) {
	items := []InvItem{{Type: InvTx, ID: [32]byte{1}}, {Type: InvBlock, ID: [32]byte{2}}}
	raw, _ := EncodeInv(items)
	f.Add(raw)
	f.Add([]byte{})
	f.Add([]byte{1, 0})
	f.Add(make([]byte, 35))

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := DecodeInv(data)
		if err != nil {
			return
		}
		if len(got) > MaxInvPerMessage {
			t.Fatalf("%d items were accepted", len(got))
		}
		for _, it := range got {
			if it.Type != InvBlock && it.Type != InvTx {
				t.Fatalf("an unknown type %d was accepted", it.Type)
			}
		}
		back, err := EncodeInv(got)
		if err != nil {
			t.Fatalf("an accepted message cannot be encoded back: %v", err)
		}
		if !bytes.Equal(back, data) {
			t.Fatal("the round-trip changed the bytes of an inv message")
		}
	})
}

// Finality attestations travel the network and the node verifies them cryptographically — but first
// it has to read them safely.
func FuzzDecodeAttestation(f *testing.F) {
	f.Add(make([]byte, AttestationBytes))
	f.Add([]byte{})
	f.Add(make([]byte, EquivocationBytes))

	f.Fuzz(func(t *testing.T, data []byte) {
		att, err := DecodeAttestation(data)
		if err != nil {
			return
		}
		if len(att.Signature) != 64 {
			t.Fatalf("a signature of length %d was accepted", len(att.Signature))
		}
		back, err := EncodeAttestation(att)
		if err != nil {
			t.Fatalf("an accepted attestation cannot be encoded back: %v", err)
		}
		if !bytes.Equal(back, data) {
			t.Fatal("the round-trip changed the bytes of an attestation")
		}
	})
}
