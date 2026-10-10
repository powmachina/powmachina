#!/usr/bin/env python3
"""Produces the desktop wallet's icons from the PowMachina mark.

The mark is **polygonal** (the SVG paths are only `M`/`L`/`Z`), so it can be rendered exactly without
an SVG rasterizer — no librsvg or inkscape needed and the result is the same on every machine.

    python3 scripts/gui-icons.py gui/src-tauri/icons/powmachina.svg gui/src-tauri/icons

It makes PNGs in the required sizes, `icon.ico` (Windows) and `icon.icns` (macOS). The ICNS is
assembled by hand — it is an `icns` container plus `ic07..ic10` blocks with PNG data, nothing more.
"""
import re
import struct
import sys
from pathlib import Path

from PIL import Image, ImageDraw

# The mark's color in the app: light on dark. The icon is drawn on a transparent background, so it
# looks right on a dark and on a light panel alike.
INK = (250, 250, 250, 255)


def polygons(svg: str):
    """Extracts the polygons from the `d` paths. Only M/L/Z is supported — the mark needs no more."""
    out = []
    for d in re.findall(r'\sd="([^"]+)"', svg):
        pts, nums = [], re.findall(r"[-+]?\d*\.?\d+", d)
        if not re.fullmatch(r"[MLZmlz\s\d.+-]+", d):
            raise SystemExit(f"the path contains curves this script cannot handle: {d[:40]}…")
        for i in range(0, len(nums) - 1, 2):
            pts.append((float(nums[i]), float(nums[i + 1])))
        if len(pts) >= 3:
            out.append(pts)
    return out


def render(polys, box, size):
    w, h = box
    img = Image.new("RGBA", (size * 4, size * 4), (0, 0, 0, 0))
    dr = ImageDraw.Draw(img)
    # The mark is wider than it is tall — it fits in the middle of a square with a small margin.
    pad = 0.08
    scale = min(size * 4 * (1 - 2 * pad) / w, size * 4 * (1 - 2 * pad) / h)
    ox = (size * 4 - w * scale) / 2
    oy = (size * 4 - h * scale) / 2
    for p in polys:
        dr.polygon([(x * scale + ox, y * scale + oy) for x, y in p], fill=INK)
    # 4x supersampling: the mark's edges are slanted and would be jagged without it.
    return img.resize((size, size), Image.LANCZOS)


def icns(images):
    """Assembles an .icns from PNG blocks. The types are the ones macOS expects for 16-512 px."""
    types = {16: b"icp4", 32: b"icp5", 128: b"ic07", 256: b"ic08", 512: b"ic09"}
    blocks = b""
    for size, png in images.items():
        if size in types:
            blocks += types[size] + struct.pack(">I", len(png) + 8) + png
    return b"icns" + struct.pack(">I", len(blocks) + 8) + blocks


def main():
    svg_path, out_dir = Path(sys.argv[1]), Path(sys.argv[2])
    svg = svg_path.read_text()
    m = re.search(r'viewBox="([\d.\s-]+)"', svg)
    if not m:
        raise SystemExit("the SVG has no viewBox")
    _, _, w, h = (float(x) for x in m.group(1).split())
    polys = polygons(svg)
    if not polys:
        raise SystemExit("there are no paths in the SVG")

    out_dir.mkdir(parents=True, exist_ok=True)
    pngs = {}
    for size in (16, 32, 48, 64, 128, 256, 512, 1024):
        img = render(polys, (w, h), size)
        if size in (32, 128, 256, 512):
            name = {32: "32x32.png", 128: "128x128.png", 256: "128x128@2x.png",
                    512: "icon.png"}[size]
            img.save(out_dir / name)
        buf = out_dir / f".tmp-{size}.png"
        img.save(buf)
        pngs[size] = buf.read_bytes()
        buf.unlink()

    (out_dir / "icon.icns").write_bytes(icns(pngs))
    base = render(polys, (w, h), 256)
    base.save(out_dir / "icon.ico", sizes=[(16, 16), (32, 32), (48, 48), (64, 64), (256, 256)])
    print("done:", ", ".join(sorted(p.name for p in out_dir.iterdir() if p.suffix in (".png", ".ico", ".icns"))))


if __name__ == "__main__":
    main()
