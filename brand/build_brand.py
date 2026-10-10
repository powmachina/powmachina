#!/usr/bin/env python3
"""PowMachina brand: one mark, every variant, plus the PNG exports.

The mark is four straight-edged paths on a 398x301 field, no curves. This file only places it:
every variant below is derived from that one constant.
"""
import os, re

OUT = os.path.dirname(os.path.abspath(__file__))

# The mark as the site draws it. Four subpaths, M/L/Z only — no curve commands anywhere.
# Ink bbox x 18..379, y 18..282 (361 x 264), centre (198.5, 150), circumscribed radius 223.6.
MARK_VB = "0 0 398 301"
MARK = (
    "M201 18 L18 18 L57 63 L183 63 L196 76 L54 219 L53 280 L258 77 Z "
    "M152 86 L54 86 L54 180 L53 181 L54 184 L53 185 Z "
    "M342 86 L285 86 L85 280 L142 280 Z "
    "M379 86 L175 281 L237 280 L330 190 L331 281 L378 282 Z"
)

NUM = re.compile(r"-?\d*\.?\d+")


def fmt(v):
    return str(int(v)) if abs(v - round(v)) < 1e-9 else f"{v:g}"


def transform(d, s, cx=198.5, cy=150.0):
    """Scales by s around the mark's own centre and recentres on (256,256) of a 512 grid."""
    out, i = [], 0
    toks = re.findall(r"[MHVLCZ]|-?\d*\.?\d+", d)
    cmd = None
    while i < len(toks):
        t = toks[i]
        if t.isalpha():
            cmd, i = t, i + 1
            out.append(t)
            continue
        n = int({"M": 2, "L": 2, "C": 6, "H": 1, "V": 1}[cmd])
        vals = [float(x) for x in toks[i:i + n]]
        i += n
        if cmd == "H":
            out.append(fmt(256 + (vals[0] - cx) * s))
        elif cmd == "V":
            out.append(fmt(256 + (vals[0] - cy) * s))
        else:
            for j, v in enumerate(vals):
                out.append(fmt(256 + (v - (cx if j % 2 == 0 else cy)) * s))
        if cmd == "M":
            cmd = "L"
    txt = " ".join(out)
    return re.sub(r"\s+([MHVLCZ])", r" \1", txt)


def circle_path(r, cx=256, cy=256):
    k = r * 0.5522847498
    return (f"M{fmt(cx)} {fmt(cy - r)} C{fmt(cx + k)} {fmt(cy - r)} {fmt(cx + r)} {fmt(cy - k)} "
            f"{fmt(cx + r)} {fmt(cy)} C{fmt(cx + r)} {fmt(cy + k)} {fmt(cx + k)} {fmt(cy + r)} "
            f"{fmt(cx)} {fmt(cy + r)} C{fmt(cx - k)} {fmt(cy + r)} {fmt(cx - r)} {fmt(cy + k)} "
            f"{fmt(cx - r)} {fmt(cy)} C{fmt(cx - r)} {fmt(cy - k)} {fmt(cx - k)} {fmt(cy - r)} "
            f"{fmt(cx)} {fmt(cy - r)} Z")


def svg(body, title, viewbox="0 0 512 512"):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="{viewbox}" '
            f'role="img" aria-label="{title}">\n{body}\n</svg>\n')


def one_path(d, fill):
    return f'  <path fill="{fill}" fill-rule="evenodd" d="{d}" />'


FILES = {}

# 1. the mark on its own, in its own field — a tight bbox is right for a standalone mark, and
#    squeezing a 1.37:1 shape into a square would only add side margin.
FILES["powm-mark.svg"] = svg(one_path(MARK, "#000000"), "PowMachina", MARK_VB)
FILES["powm-mark-white.svg"] = svg(one_path(MARK, "#FFFFFF"), "PowMachina", MARK_VB)

# 1b. the mark that follows the reader, for a favicon or anywhere both grounds occur. Only works
#     where the SVG is part of the page; through <img> it answers to the OS, not to the page.
FILES["powm-mark-auto.svg"] = (
    f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="{MARK_VB}" role="img" aria-label="PowMachina">\n'
    "  <style>path{fill:#000000}@media (prefers-color-scheme:dark){path{fill:#FFFFFF}}</style>\n"
    f'  <path fill-rule="evenodd" d="{MARK}" />\n</svg>\n')

# 1c. inherits the surrounding text colour — for inlining in HTML.
FILES["powm-mark-currentcolor.svg"] = svg(one_path(MARK, "currentColor"), "PowMachina", MARK_VB)

# 2. the coin: a black disc with the mark cut out (even-odd). 0.827 leaves ~39 u. from the rim,
#    which is ~2.4 px at 32 px — the first coin let the mark touch the edge and it blurred shut.
M_COIN = transform(MARK, 0.827)
FILES["powm-coin.svg"] = svg(one_path(circle_path(224) + " " + M_COIN, "#000000"), "PowMachina")

# 3. the coin inverted: a white disc with a black mark, for a dark background.
FILES["powm-coin-inv.svg"] = svg(
    '  <circle cx="256" cy="256" r="224" fill="#FFFFFF"/>\n' + one_path(M_COIN, "#000000"),
    "PowMachina")

# 4. the coin with a ring — one path, even-odd alternating:
#    212..224 the black rim · 188..212 the white ring (24 u. = 1.5 px at 32 px) · the inside black
#    · the mark white.
M_RING = transform(MARK, 0.671)
FILES["powm-coin-ring.svg"] = svg(
    one_path(" ".join([circle_path(224), circle_path(212), circle_path(188), M_RING]), "#000000"),
    "PowMachina")

# 5. the mark centred in a square, for avatars, app icons and coin listings — the exchanges and
#    the pool draw into a square box and would otherwise crop a 1.37:1 mark.
M_SQ = transform(MARK, 1.108)
FILES["powm-square.svg"] = svg(one_path(M_SQ, "#000000"), "PowMachina")
FILES["powm-square-white.svg"] = svg(one_path(M_SQ, "#FFFFFF"), "PowMachina")

if __name__ == "__main__":
    os.makedirs(OUT, exist_ok=True)
    for name, txt in FILES.items():
        open(os.path.join(OUT, name), "w").write(txt)
    print("SVG:", ", ".join(sorted(FILES)))
