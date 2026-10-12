// Package safetext makes text written by another party fit to print.
//
// A miner prints what its pool tells it — the network's name, why a lane is paused, why a share was
// refused — and every client of a node prints the node's errors. That text goes to a terminal and to
// log files, and the other side chooses every byte of it: an escape sequence clears the screen or
// rewrites the lines above it, a newline starts a line that looks like the program's own.
package safetext

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Line returns s as text that stays on one line and does only what text does: everything that is
// not printable — control characters, escape sequences, line breaks, characters that reorder the
// text around them, bytes that are not UTF-8 — is written out as an escape, and what is past about
// max bytes is cut off and marked.
func Line(s string, max int) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if b.Len() >= max {
			b.WriteString("…")
			break
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case unicode.IsPrint(r):
			b.WriteRune(r)
		default:
			q := strconv.QuoteRune(r) // '\x1b', '\n', '‮'
			b.WriteString(q[1 : len(q)-1])
		}
		i += n
	}
	return b.String()
}
