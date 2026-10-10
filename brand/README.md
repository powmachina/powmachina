# PowMachina — the brand

The source of truth is the **SVGs** in this directory, and they are generated: run
`python3 build_brand.py` to redraw them, then `./export_png.sh` for the PNGs. Do not edit either by
hand.

Everything comes from one constant, `MARK` in `build_brand.py` — the four straight-edged paths of
the mark, on a 398×301 field. Change it there and every variant follows.

| file | use |
|---|---|
| `powm-mark.svg` | **the mark**, black, in its own field — documents, headers, print |
| `powm-mark-white.svg` | the same in white, for a dark background |
| `powm-mark-auto.svg` | follows the reader's colour scheme (see the caveat below) |
| `powm-mark-currentcolor.svg` | inherits the surrounding text colour — for inlining in HTML |
| `powm-square.svg` / `-white.svg` | the mark centred in a 512 square — avatars, app icons, coin listings |
| `powm-coin.svg` | a black disc with the mark cut out. Exchanges, the explorer, the favicon |
| `powm-coin-inv.svg` | a white disc with a black mark — for a dark background |
| `powm-coin-ring.svg` | the version with a ring — the website, print, large formats. **Not below 64 px** |
| `favicon.ico` | 16/32/48/64 |
| `png/` | generated, every variant at 32…2048 |

`powm-mark-auto.svg` only switches where the SVG is part of the page — inlined, or a CSS
background on the same document. Through `<img src>` the SVG is its own document and answers to the
reader's operating system rather than to the page, so a visitor on a light OS gets the black mark
on whatever ground the page happens to have. Pick `-white` or the plain one explicitly there.

Square versus the plain mark: the mark is 1.37:1. Anything that draws into a square box — an
exchange listing, a pool's coin column, an app icon — will crop or letterbox it, so give those
`powm-square*`.

## The rules

- Only `#000000` and `#FFFFFF`. No gradient, shadow, bevel or 3D.
- The mark is straight edges only, no curves. Every diagonal is 45°.
- The coins live on a 512 grid, centre `256,256`, disc `r=224`.
- The mark is a single path with `fill-rule="evenodd"`; the cut-outs in the coins come from the
  even-odd rule, never from a white shape laid on top.
- **Do not enlarge the mark inside the coins.** `0.827` on the plain coin leaves ~39 units from the
  rim (~2.4 px at 32 px) and `0.671` on the ring version clears the inner ring. The first coin let
  the mark touch the edge and it blurred shut at small sizes.

## The mark's grid

Ink bbox `x 18..379`, `y 18..282` — 361 × 264, centre `(198.5, 150)`, circumscribed radius 223.6.
Four subpaths, `M`/`L`/`Z` only, no curve commands anywhere.

## The renderer

`svgrender.py` is a hand-rolled rasteriser that only understands the 512 grid, so it cannot draw
the mark in its own viewBox. `export_png.sh` uses `rsvg-convert` instead. The file is kept for
reference; nothing imports it.
