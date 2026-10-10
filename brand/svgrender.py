#!/usr/bin/env python3
"""A minimal renderer for our logos: M/H/V/L/C/Z, absolute coordinates, even-odd, 4x SSAA."""
import re
from PIL import Image

TOK = re.compile(r"[MmHhVvLlCcZz]|-?\d*\.?\d+")


def flatten(d, steps=24):
    """Path -> a list of subpath polylines [(x,y), ...]."""
    toks = TOK.findall(d)
    subs, cur = [], []
    x = y = sx = sy = 0.0
    i = 0
    cmd = None
    while i < len(toks):
        t = toks[i]
        if t.isalpha():
            cmd = t
            i += 1
            if cmd in "Zz":
                if cur:
                    cur.append((sx, sy))
                    subs.append(cur)
                    cur = []
                x, y = sx, sy
            continue
        n = lambda k: float(toks[i + k])
        if cmd == "M":
            x, y = n(0), n(1); i += 2
            if cur:
                subs.append(cur)
            cur = [(x, y)]; sx, sy = x, y
            cmd = "L"
        elif cmd == "L":
            x, y = n(0), n(1); i += 2; cur.append((x, y))
        elif cmd == "H":
            x = n(0); i += 1; cur.append((x, y))
        elif cmd == "V":
            y = n(0); i += 1; cur.append((x, y))
        elif cmd == "C":
            x1, y1, x2, y2, x3, y3 = (n(k) for k in range(6)); i += 6
            for s in range(1, steps + 1):
                t_ = s / steps
                u = 1 - t_
                cur.append((u**3 * x + 3 * u * u * t_ * x1 + 3 * u * t_ * t_ * x2 + t_**3 * x3,
                            u**3 * y + 3 * u * u * t_ * y1 + 3 * u * t_ * t_ * y2 + t_**3 * y3))
            x, y = x3, y3
        else:
            raise ValueError(f"unsupported command {cmd!r}")
    if cur:
        subs.append(cur)
    return subs


def fill_evenodd(subs, size, ss=4):
    """Returns an (L) mask of size x size, 255 = inside."""
    N = size * ss
    img = Image.new("L", (N, N), 0)
    px = img.load()
    edges = []
    for sub in subs:
        pts = sub if sub[0] == sub[-1] else sub + [sub[0]]
        for (x0, y0), (x1, y1) in zip(pts, pts[1:]):
            if y0 != y1:
                edges.append((x0 * ss, y0 * ss, x1 * ss, y1 * ss))
    for row in range(N):
        yc = row + 0.5
        xs = []
        for x0, y0, x1, y1 in edges:
            if (y0 <= yc < y1) or (y1 <= yc < y0):
                xs.append(x0 + (yc - y0) * (x1 - x0) / (y1 - y0))
        xs.sort()
        for a, b in zip(xs[0::2], xs[1::2]):
            for col in range(max(0, int(a + 0.5)), min(N, int(b + 0.5))):
                px[col, row] = 255
    return img.resize((size, size), Image.LANCZOS)


def render(svg_text, size=512):
    """Draws our SVG shape (circle + path) onto an RGB image."""
    base = Image.new("RGB", (size, size), (255, 255, 255))
    k = size / 512.0
    for m in re.finditer(r"<circle[^>]*cx=\"([\d.]+)\"[^>]*cy=\"([\d.]+)\"[^>]*r=\"([\d.]+)\""
                         r"[^>]*fill=\"(#\w+)\"", svg_text):
        cx, cy, r = (float(m.group(j)) * k for j in (1, 2, 3))
        col = m.group(4)
        mask = Image.new("L", (size, size), 0)
        from PIL import ImageDraw
        ImageDraw.Draw(mask).ellipse([cx - r, cy - r, cx + r, cy + r], fill=255)
        base.paste(Image.new("RGB", (size, size), col), (0, 0), mask)
    for m in re.finditer(r"<path[^>]*?fill=\"(#\w+)\"[^>]*?d=\"(.*?)\"", svg_text, re.S):
        col, d = m.group(1), m.group(2)
        subs = [[(px_ * k, py_ * k) for px_, py_ in s] for s in flatten(d)]
        base.paste(Image.new("RGB", (size, size), col), (0, 0), fill_evenodd(subs, size))
    return base
