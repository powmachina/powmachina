package geoip

import (
	"net/netip"
	"strings"
	"testing"
)

const sample = `1.0.0.0,1.0.0.255,AU
5.0.0.0,5.255.255.255,DE
2.0.0.0,2.0.0.255,ZZ
2a01:4f8::,2a01:4f8:ffff:ffff:ffff:ffff:ffff:ffff,DE
`

func TestCountry(t *testing.T) {
	db, err := Read(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]string{
		"1.0.0.7": "AU", "5.9.1.2": "DE", "2a01:4f8:1:2::3": "DE", "::ffff:5.1.1.1": "DE",
		"1.0.1.0": "", "0.9.9.9": "", "2.0.0.1": "", "2001:db8::1": "",
	} {
		got, ok := db.Country(netip.MustParseAddr(ip))
		if got != want || ok != (want != "") {
			t.Errorf("%s: %q %v, want %q", ip, got, ok, want)
		}
	}
	if _, err := Read(strings.NewReader("1.0.0.0,AU\n")); err == nil {
		t.Error("a broken line was taken")
	}
}
