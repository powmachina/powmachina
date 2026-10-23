package p2p

import (
	"encoding/binary"
	"net"
	"time"
)

// Address gossip: `getaddr` asks a peer for addresses it knows, `addr` carries them.
//
// Every entry carries the services the address was last seen with. That is what lets a node
// holding the full DAG find others like it before it connects: shares go only between full-DAG
// nodes (settlement.go), so a full node behind light peers only would never hear a share.
// A claimed service is a hint for the choice, not trust — the handshake says what the peer is.
//
// An entry with the unspecified IP (0.0.0.0) and a port is a node announcing itself: "I listen on
// this port at the address you see me from". A node behind NAT does not know its public IP, but
// the peer it dialled does.

// addrEntryBytes: time u32 (unix seconds), services u64, IP (16 B, IPv4 mapped), port u16.
const addrEntryBytes = 4 + 8 + 16 + 2

// EncodeAddr packs addresses; onion addresses have no place in this form and are left out.
func EncodeAddr(list []Address) ([]byte, error) {
	out := make([]byte, 2, 2+len(list)*addrEntryBytes)
	n := 0
	for _, a := range list {
		if a.Onion != "" || a.IP == nil {
			continue
		}
		if n == MaxAddrPerMessage {
			return nil, ErrTooManyItems
		}
		var e [addrEntryBytes]byte
		binary.LittleEndian.PutUint32(e[0:4], uint32(a.LastSeen.Unix()))
		binary.LittleEndian.PutUint64(e[4:12], a.Services)
		copy(e[12:28], a.IP.To16())
		binary.LittleEndian.PutUint16(e[28:30], a.Port)
		out = append(out, e[:]...)
		n++
	}
	binary.LittleEndian.PutUint16(out[0:2], uint16(n))
	return out, nil
}

// DecodeAddr reads addresses from a foreign peer — that is, untrusted bytes. Whether an address is
// worth keeping is the addrman's business (Dialable); this only checks the form.
func DecodeAddr(payload []byte) ([]Address, error) {
	if len(payload) < 2 {
		return nil, ErrBadPayload
	}
	n := int(binary.LittleEndian.Uint16(payload[0:2]))
	if n > MaxAddrPerMessage {
		return nil, ErrTooManyItems
	}
	if len(payload) != 2+n*addrEntryBytes {
		return nil, ErrBadPayload
	}
	list := make([]Address, n)
	for i := range list {
		e := payload[2+i*addrEntryBytes : 2+(i+1)*addrEntryBytes]
		ip := make(net.IP, 16)
		copy(ip, e[12:28])
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		list[i] = Address{
			IP:       ip,
			Port:     binary.LittleEndian.Uint16(e[28:30]),
			Services: binary.LittleEndian.Uint64(e[4:12]),
			LastSeen: time.Unix(int64(binary.LittleEndian.Uint32(e[0:4])), 0),
		}
	}
	return list, nil
}
