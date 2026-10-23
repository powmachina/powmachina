// Package p2p is the node's network layer.
//
// Everything that arrives here comes from a foreign peer and has to be treated as hostile input.
// That is why every message has a hard size cap and every list a hard cap on the number of items —
// without those, a node can be taken down by memory before it even gets to validation.
package p2p

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

const (
	// HeaderSize is the size of a message frame: magic(4) + command(12) + length(4) + checksum(4).
	HeaderSize = 24

	// MaxMessageBytes is the payload cap. The largest legitimate message is a block with its
	// settlement body (chain.MaxBlockWireBytes, 352 kB), so 1 MiB is a comfortable margin and still
	// keeps memory under control.
	MaxMessageBytes = 1 << 20

	// Caps on the number of items in lists. Without them an `addr` or `headers` message is the
	// cheapest way to spend a node's memory.
	MaxHeadersPerMessage = 2000
	MaxAddrPerMessage    = 1000
	MaxInvPerMessage     = 5000
	// MaxAttestHeights is the cap on the range attestations can be asked for at once.
	// With a finality depth of 20 blocks, 64 heights is a threefold margin.
	MaxAttestHeights = 64
	// MaxAttestPerMessage is the cap on the number of attestations in one answer (64 heights × 10
	// keys) — a fixed cap, not trust in how many the peer sends. A quorum with more keys than that
	// is answered in part and the rest comes with the next request.
	MaxAttestPerMessage = 640
)

// Command is the message type. A fixed length of 12 B, ASCII, padded with zeros.
type Command string

const (
	CmdVersion    Command = "version"
	CmdVerack     Command = "verack"
	CmdPing       Command = "ping"
	CmdPong       Command = "pong"
	CmdGetAddr    Command = "getaddr"
	CmdAddr       Command = "addr"
	CmdInv        Command = "inv"
	CmdGetData    Command = "getdata"
	CmdGetHeaders Command = "getheaders"
	CmdHeaders    Command = "headers"
	CmdBlock      Command = "block"
	CmdTx         Command = "tx"
	// The finality messages carry the signer's key, not an index into a fixed quorum. They have
	// new names, so a node from before the change ignores them rather than reading them wrong.
	CmdAttestation  Command = "fattest"   // a finality attestation
	CmdEquivocation Command = "fequivoc"  // proof of double signing
	CmdGetAttest    Command = "fgetattst" // "send the attestations for these heights"
	CmdAttestations Command = "fattests"  // the answer: the attestations the peer has
)

var (
	ErrBadMagic     = errors.New("p2p: the message is from another network")
	ErrTooLarge     = errors.New("p2p: the message exceeds the allowed size")
	ErrBadChecksum  = errors.New("p2p: the checksum does not match")
	ErrBadCommand   = errors.New("p2p: an invalid message name")
	ErrTooManyItems = errors.New("p2p: the list has too many items")
)

// crcTable is Castagnoli — the checksum only serves to detect a corrupted frame.
// No security follows from it and none is meant to; the content is verified by validation.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Message is one message on the wire.
type Message struct {
	Command Command
	Payload []byte
}

// Encode writes a message together with its frame.
func Encode(w io.Writer, magic [4]byte, msg *Message) error {
	if len(msg.Payload) > MaxMessageBytes {
		return ErrTooLarge
	}
	cmd, err := encodeCommand(msg.Command)
	if err != nil {
		return err
	}
	var hdr [HeaderSize]byte
	copy(hdr[0:4], magic[:])
	copy(hdr[4:16], cmd[:])
	binary.LittleEndian.PutUint32(hdr[16:20], uint32(len(msg.Payload)))
	binary.LittleEndian.PutUint32(hdr[20:24], crc32.Checksum(msg.Payload, crcTable))

	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(msg.Payload)
	return err
}

// Decode reads one message. The size is checked **before** the payload is allocated, so a peer
// cannot force a large allocation with a number in the header alone.
func Decode(r io.Reader, magic [4]byte) (*Message, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	if [4]byte(hdr[0:4]) != magic {
		return nil, ErrBadMagic
	}
	length := binary.LittleEndian.Uint32(hdr[16:20])
	if length > MaxMessageBytes {
		return nil, ErrTooLarge
	}
	cmd, err := decodeCommand([12]byte(hdr[4:16]))
	if err != nil {
		return nil, err
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	if crc32.Checksum(payload, crcTable) != binary.LittleEndian.Uint32(hdr[20:24]) {
		return nil, ErrBadChecksum
	}
	return &Message{Command: cmd, Payload: payload}, nil
}

func encodeCommand(c Command) ([12]byte, error) {
	var out [12]byte
	if len(c) == 0 || len(c) > 12 {
		return out, ErrBadCommand
	}
	for i := 0; i < len(c); i++ {
		if c[i] < 0x20 || c[i] > 0x7e {
			return out, ErrBadCommand
		}
	}
	copy(out[:], c)
	return out, nil
}

func decodeCommand(raw [12]byte) (Command, error) {
	end := len(raw)
	for i, b := range raw {
		if b == 0 {
			end = i
			break
		}
	}
	if end == 0 {
		return "", ErrBadCommand
	}
	for i := 0; i < end; i++ {
		if raw[i] < 0x20 || raw[i] > 0x7e {
			return "", ErrBadCommand
		}
	}
	// After the first zero there may only be zeros — otherwise the same message would have more than one form.
	for i := end; i < len(raw); i++ {
		if raw[i] != 0 {
			return "", ErrBadCommand
		}
	}
	return Command(raw[:end]), nil
}

// CheckItemCount verifies the cap on the number of items before anything is allocated.
func CheckItemCount(n int, max int) error {
	if n < 0 || n > max {
		return fmt.Errorf("%w: %d (max %d)", ErrTooManyItems, n, max)
	}
	return nil
}
