package main

import (
	"strings"
	"testing"
	"time"

	"powmachina/internal/geoip"
)

// The map counts a country per share by where it came in from; a private or local origin is
// unknown, and the window forgets what is older than geoMinutes.
func TestShareGeoWindow(t *testing.T) {
	db, err := geoip.Read(strings.NewReader("5.0.0.0,5.255.255.255,DE\n37.0.0.0,37.255.255.255,FI\n"))
	if err != nil {
		t.Fatal(err)
	}
	g := newShareGeo(db)
	at := time.Unix(1_800_000_000, 0)
	g.now = func() time.Time { return at }
	g.note("5.9.1.2:17696")
	g.note("37.1.2.3")
	g.note("37.1.2.3")
	g.note("10.0.0.5:17696")
	g.note("")
	w, seen, unknown, _ := g.snapshot()
	if w["DE"] != 1 || w["FI"] != 2 || unknown != 2 || seen["FI"] != 2 {
		t.Fatalf("window %v seen %v unknown %d", w, seen, unknown)
	}
	at = at.Add((geoMinutes + 1) * time.Minute)
	g.note("5.9.1.2")
	w, seen, unknown, _ = g.snapshot()
	if len(w) != 1 || w["DE"] != 1 || unknown != 0 || seen["DE"] != 2 {
		t.Fatalf("after the window: %v seen %v unknown %d", w, seen, unknown)
	}
}

// A share relayed by one of the network's own nodes is not counted: it entered somewhere else.
func TestShareGeoIgnoresOwnNodes(t *testing.T) {
	db, err := geoip.Read(strings.NewReader("5.0.0.0,5.255.255.255,DE\n37.0.0.0,37.255.255.255,FI\n"))
	if err != nil {
		t.Fatal(err)
	}
	ignore, err := parsePrefixes(" 37.1.2.3, 5.9.0.0/16 ,")
	if err != nil || len(ignore) != 2 {
		t.Fatalf("parse: %v %v", ignore, err)
	}
	if _, err := parsePrefixes("nope"); err == nil {
		t.Fatal("a word was taken for an address")
	}
	g := newShareGeo(db)
	g.ignore = ignore
	g.note("37.1.2.3:17696")
	g.note("[::ffff:5.9.1.2]:17696")
	g.note("5.10.1.2")
	g.note("37.1.2.4")
	w, _, unknown, _ := g.snapshot()
	if w["DE"] != 1 || w["FI"] != 1 || unknown != 0 {
		t.Fatalf("window %v unknown %d", w, unknown)
	}
}
