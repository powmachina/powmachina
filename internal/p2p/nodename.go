package p2p

import (
	"errors"
	"fmt"
	"strings"
)

// The node name an operator chooses at startup.
//
// It goes into the user agent in the handshake, so every peer sees it — it is a label in public,
// not a private field. Hence the restrictions: short, without the separator and without characters
// that would cause mischief in somebody else's log.
//
// Foreign names are already handled on decoding (VersionMsg.Decode only lets 0x20–0x7e through);
// this way a nonsensical name of our own cannot be produced either.

const (
	// MaxNodeNameBytes is the cap on the name's length. Together with the prefix it has to fit into
	// MaxUserAgentBytes.
	MaxNodeNameBytes = 24
	// AgentPrefix is the part of the user agent that identifies the software and the version.
	AgentPrefix = "/powmachinad:0.1/"
)

var ErrBadNodeName = errors.New("p2p: an invalid node name")

// ValidateNodeName verifies the name chosen by the operator. It returns the reason so it can be
// printed for a person — silently adjusting their choice would be worse than refusing it.
func ValidateNodeName(name string) error {
	switch {
	case name == "":
		return nil // no name is fine, nothing is appended
	case len(name) > MaxNodeNameBytes:
		return fmt.Errorf("%w: at most %d characters, it has %d",
			ErrBadNodeName, MaxNodeNameBytes, len(name))
	case strings.TrimSpace(name) != name:
		return fmt.Errorf("%w: it must not start or end with a space", ErrBadNodeName)
	}
	for _, r := range name {
		switch {
		case r == '/':
			return fmt.Errorf("%w: '/' separates the user agent's fields", ErrBadNodeName)
		case r < 0x20 || r > 0x7e:
			return fmt.Errorf("%w: printable ASCII characters only", ErrBadNodeName)
		}
	}
	return nil
}

// UserAgentFor assembles the user agent from the software version and the node name.
// An empty name gives the prefix alone.
func UserAgentFor(name string) string {
	if name == "" {
		return AgentPrefix
	}
	return AgentPrefix + name + "/"
}
