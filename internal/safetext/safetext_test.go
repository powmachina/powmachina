package safetext

import (
	"strings"
	"testing"
	"unicode"
)

// The characters under test are built from their numbers, so that this file holds none of them:
// some are invisible and one reorders the text of the line it stands in.
func ch(code rune) string { return string(code) }

// bs is a backslash, the start of an escape in the expected output.
const bs = `\`

// Ordinary text comes through untouched; anything a terminal would act on comes through as the
// characters that spell it.
func TestLineLeavesNothingATerminalActsOn(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"testnet", "testnet"},
		{"low difficulty share", "low difficulty share"},
		{"backslash " + bs + " stays", "backslash " + bs + " stays"},
		{"an accent " + ch(0xe9) + ", a dash " + ch(0x2014) + ", an arrow " + ch(0x2192),
			"an accent " + ch(0xe9) + ", a dash " + ch(0x2014) + ", an arrow " + ch(0x2192)},

		{"a" + ch(0x0a) + "b", "a" + bs + "nb"},
		{"a" + ch(0x0d) + ch(0x0a) + "BLOCK FOUND", "a" + bs + "r" + bs + "nBLOCK FOUND"},
		{ch(0x1b) + "[2J" + ch(0x1b) + "[H", bs + "x1b[2J" + bs + "x1b[H"},
		{ch(0x1b) + "]0;title" + ch(0x07), bs + "x1b]0;title" + bs + "a"},
		{"tab" + ch(0x09) + "here", "tab" + bs + "there"},
		{"nul" + ch(0x00), "nul" + bs + "x00"},
		{"del" + ch(0x7f), "del" + bs + "x7f"},
		{"c1 " + ch(0x9b) + "31m", "c1 " + bs + "u009b31m"},
		{"reordered " + ch(0x202e) + "gnp.exe", "reordered " + bs + "u202egnp.exe"},
		{"line" + ch(0x2028) + "separator", "line" + bs + "u2028separator"},
		{"zero" + ch(0x200b) + "width", "zero" + bs + "u200bwidth"},
		{"byte order " + ch(0xfeff) + " mark", "byte order " + bs + "ufeff mark"},
		{"private use " + ch(0xe000), "private use " + bs + "ue000"},
		{"non-breaking" + ch(0xa0) + "space", "non-breaking" + bs + "u00a0space"},
		{"not utf-8 " + string([]byte{0xff, 0xfe}), "not utf-8 " + bs + "xff" + bs + "xfe"},
		{"cut short " + string([]byte{0xe2, 0x82}), "cut short " + bs + "xe2" + bs + "x82"},
	}
	for _, c := range cases {
		got := Line(c.in, 1000)
		if got != c.want {
			t.Errorf("Line(%q) = %q, want %q", c.in, got, c.want)
		}
		for _, r := range got {
			if !unicode.IsPrint(r) {
				t.Errorf("Line(%q) still holds %U", c.in, r)
			}
		}
	}
}

// The other side also chooses how much it sends.
func TestLineIsCutAtTheCap(t *testing.T) {
	mark := ch(0x2026)
	got := Line(strings.Repeat("a", 5000), 200)
	if got != strings.Repeat("a", 200)+mark {
		t.Fatalf("5000 characters came out as %d bytes", len(got))
	}
	// A cap that falls inside an escape does not leave half of it.
	got = Line(strings.Repeat(ch(0x1b), 5000), 10)
	if !strings.HasSuffix(got, bs+"x1b"+mark) || len(got) > 10+4+len(mark) {
		t.Fatalf("escapes cut at the cap came out as %q", got)
	}
	if got := Line("short", 5); got != "short" {
		t.Fatalf("text exactly at the cap came out as %q", got)
	}
}
