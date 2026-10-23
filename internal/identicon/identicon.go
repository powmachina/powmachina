// Package identicon draws a miner's icon from its payout program: a 5×5 grid, mirrored like
// GitHub's, in one colour. The same program always gives the same icon, so nothing is stored or
// sent — the explorer and the wallet draw it from what the chain already carries.
//
// It is a picture, not an identity: two programs can look alike, and the icon proves nothing.
package identicon

import (
	"crypto/sha256"
	"fmt"
	"math/bits"
	"strings"
)

const (
	grid = 5
	tag  = "PowMachina/identicon/v1"
	// minCells is the fewest of the 15 drawn cells an icon has (but for a hash with too few bits).
	minCells = 6
)

// Icon is what an icon is drawn from.
type Icon struct {
	// Cells are the filled cells, row by row.
	Cells [grid][grid]bool
	// Hue is 0–359, Saturation and Lightness in percent.
	Hue, Saturation, Lightness int
}

// For derives the icon of a payout program.
func For(program []byte) Icon {
	h := sha256.Sum256(append([]byte(tag), program...))
	var ic Icon
	// The left three columns from 15 bits, mirrored onto the right two. A sparse one (a dash, a
	// dot) gets more bits ORed in until at least minCells of the 15 are on: at 16 px a lonely line
	// does not read as a picture.
	b := uint32(h[0]) | uint32(h[1])<<8
	for i := 6; i < 14 && bits.OnesCount32(b&0x7fff) < minCells; i += 2 {
		b |= uint32(h[i]) | uint32(h[i+1])<<8
	}
	if b&0x7fff == 0 {
		b = 0x2492 // the middle column, should the hash ever leave nothing
	}
	for row := range grid {
		for col := range 3 {
			if b&(1<<(row*3+col)) != 0 {
				ic.Cells[row][col] = true
				ic.Cells[row][grid-1-col] = true
			}
		}
	}
	ic.Hue = int(uint16(h[2])|uint16(h[3])<<8) % 360
	ic.Saturation = 50 + int(h[4])%25
	ic.Lightness = 45 + int(h[5])%15
	return ic
}

// Color is the icon's colour as CSS.
func (ic Icon) Color() string {
	// The comma form: it is also what an SVG fill attribute takes in every browser.
	return fmt.Sprintf("hsl(%d, %d%%, %d%%)", ic.Hue, ic.Saturation, ic.Lightness)
}

// SVG is the icon as an inline SVG `px` pixels wide, one path in one colour (~300 B).
func (ic Icon) SVG(px int) string {
	var d strings.Builder
	for row := range grid {
		for col := range grid {
			if ic.Cells[row][col] {
				fmt.Fprintf(&d, "M%d %dh1v1h-1z", col, row)
			}
		}
	}
	return fmt.Sprintf(`<svg class="identicon" viewBox="-0.5 -0.5 6 6" width="%d" height="%d" `+
		`shape-rendering="crispEdges" aria-hidden="true"><path fill="%s" d="%s"/></svg>`,
		px, px, ic.Color(), d.String())
}
