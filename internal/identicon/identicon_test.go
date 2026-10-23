package identicon

import (
	"strings"
	"testing"
)

// The icon depends on the program alone, is mirrored, never empty, and two programs differ.
func TestIconIsStableMirroredAndDistinct(t *testing.T) {
	a, b := make([]byte, 32), make([]byte, 32)
	b[31] = 1
	if For(a) != For(a) {
		t.Fatal("the same program drew two icons")
	}
	if For(a) == For(b) {
		t.Fatal("two programs drew the same icon")
	}
	for i := range 2000 {
		p := []byte{byte(i), byte(i >> 8)}
		ic := For(p)
		left := 0
		for row := range grid {
			for col := range grid {
				if ic.Cells[row][col] != ic.Cells[row][grid-1-col] {
					t.Fatalf("program %d: the icon is not mirrored", i)
				}
				if col < 3 && ic.Cells[row][col] {
					left++
				}
			}
		}
		if left < minCells {
			t.Fatalf("program %d: a sparse icon, %d of 15 cells", i, left)
		}
		if ic.Hue < 0 || ic.Hue > 359 || ic.Saturation < 50 || ic.Saturation > 74 || ic.Lightness < 45 || ic.Lightness > 59 {
			t.Fatalf("program %d: colour out of range %+v", i, ic)
		}
	}
	svg := For(a).SVG(16)
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") || len(svg) > 600 {
		t.Fatalf("an unexpected SVG (%d B): %s", len(svg), svg)
	}
}
