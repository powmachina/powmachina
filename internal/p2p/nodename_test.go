package p2p

import (
	"strings"
	"testing"
)

func TestNodeNameRules(t *testing.T) {
	ok := []string{"", "seed-fi-1", "the spare box", "node_42", "x"}
	for _, n := range ok {
		if err := ValidateNodeName(n); err != nil {
			t.Errorf("%q should have passed: %v", n, err)
		}
	}
	bad := map[string]string{
		strings.Repeat("a", MaxNodeNameBytes+1): "too long",
		" node":                                 "a space at the start",
		"node ":                                 "a space at the end",
		"a/b":                                   "a slash separates the agent's fields",
		"node\nfake":                            "a line break",
		"node\x1b[2J":                           "an escape sequence",
		"nøde":                                  "outside ASCII",
	}
	for n, why := range bad {
		if err := ValidateNodeName(n); err == nil {
			t.Errorf("%q should have failed (%s)", n, why)
		}
	}
}

// A user agent with a name has to fit into the limit, otherwise the handshake would not pass.
func TestUserAgentFitsLimit(t *testing.T) {
	long := strings.Repeat("W", MaxNodeNameBytes)
	if err := ValidateNodeName(long); err != nil {
		t.Fatal(err)
	}
	ua := UserAgentFor(long)
	if len(ua) > MaxUserAgentBytes {
		t.Fatalf("the user agent %q has %d B, the cap is %d", ua, len(ua), MaxUserAgentBytes)
	}
	if UserAgentFor("") != AgentPrefix {
		t.Fatalf("without a name the prefix alone should remain, it is %q", UserAgentFor(""))
	}
	// And it has to pass encoding too, not just a length test.
	v := &VersionMsg{Protocol: ProtocolVersion, UserAgent: ua}
	b, err := v.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeVersion(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UserAgent != ua {
		t.Fatalf("the user agent does not match: %q != %q", got.UserAgent, ua)
	}
}
