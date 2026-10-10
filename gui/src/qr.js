// QR codes for addresses. Byte mode, error correction level M, versions 1–10 (up to 213 bytes) —
// an address is 64–65 characters and lands on version 5.
//
// Why our own and not a library: the wallet has no dependencies to audit or update, and a QR
// code is a closed standard (ISO/IEC 18004) that does not move. The structure follows Project
// Nayuki's QR Code generator (MIT), reduced to the one mode and level a wallet needs.
//
// A wrong QR code would not send coins to a wrong place — an address carries its own checksum
// and a wallet refuses one that fails it — but it would send nobody anywhere, so
// gui/test/qr.test.js checks every code against fixed vectors that an independent decoder read
// back correctly.
"use strict";

const QR = (() => {
  const ECC_PER_BLOCK = [0, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26];   // level M, by version
  const NUM_BLOCKS = [0, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5];
  const MAX_VERSION = 10;

  function rawModules(ver) {
    let n = (16 * ver + 128) * ver + 64;
    if (ver >= 2) {
      const a = Math.floor(ver / 7) + 2;
      n -= (25 * a - 10) * a - 55;
      if (ver >= 7) n -= 36;
    }
    return n;
  }
  const dataCodewords = (ver) => Math.floor(rawModules(ver) / 8) - ECC_PER_BLOCK[ver] * NUM_BLOCKS[ver];

  // GF(2^8) over x^8 + x^4 + x^3 + x^2 + 1.
  function mul(x, y) {
    let z = 0;
    for (let i = 7; i >= 0; i--) {
      z = (z << 1) ^ ((z >>> 7) * 0x11d);
      z ^= ((y >>> i) & 1) * x;
    }
    return z;
  }
  function rsDivisor(degree) {
    const r = new Array(degree).fill(0);
    r[degree - 1] = 1;
    let root = 1;
    for (let i = 0; i < degree; i++) {
      for (let j = 0; j < degree; j++) {
        r[j] = mul(r[j], root);
        if (j + 1 < degree) r[j] ^= r[j + 1];
      }
      root = mul(root, 2);
    }
    return r;
  }
  function rsRemainder(data, divisor) {
    const r = divisor.map(() => 0);
    for (const b of data) {
      const f = b ^ r.shift();
      r.push(0);
      divisor.forEach((c, i) => { r[i] ^= mul(c, f); });
    }
    return r;
  }

  function codewords(bytes, ver) {
    const bits = [];
    const put = (v, n) => { for (let i = n - 1; i >= 0; i--) bits.push((v >>> i) & 1); };
    put(4, 4);                                   // byte mode
    put(bytes.length, ver < 10 ? 8 : 16);
    for (const b of bytes) put(b, 8);
    const cap = dataCodewords(ver) * 8;
    put(0, Math.min(4, cap - bits.length));      // terminator
    put(0, (8 - (bits.length % 8)) % 8);
    for (let pad = 0xec; bits.length < cap; pad ^= 0xec ^ 0x11) put(pad, 8);
    const data = [];
    for (let i = 0; i < bits.length; i += 8) data.push(bits.slice(i, i + 8).reduce((a, b) => (a << 1) | b, 0));

    // Split into blocks, add the error correction, interleave.
    const blocks = NUM_BLOCKS[ver], ecc = ECC_PER_BLOCK[ver], raw = Math.floor(rawModules(ver) / 8);
    const short = blocks - (raw % blocks), shortLen = Math.floor(raw / blocks), div = rsDivisor(ecc), parts = [];
    for (let i = 0, k = 0; i < blocks; i++) {
      const dat = data.slice(k, k + shortLen - ecc + (i < short ? 0 : 1));
      k += dat.length;
      const rem = rsRemainder(dat, div);
      if (i < short) dat.push(0);
      parts.push(dat.concat(rem));
    }
    const out = [];
    for (let i = 0; i < parts[0].length; i++) {
      parts.forEach((p, j) => { if (i !== shortLen - ecc || j >= short) out.push(p[i]); });
    }
    return out;
  }

  // matrix returns { size, dark(x, y) } for a string, or throws when it does not fit.
  function matrix(text) {
    const bytes = Array.from(new TextEncoder().encode(String(text)));
    let ver = 1;
    while (ver <= MAX_VERSION && bytes.length > dataCodewords(ver) - (ver < 10 ? 2 : 3)) ver++;
    if (ver > MAX_VERSION) throw new Error("too long for a QR code");
    const size = ver * 4 + 17;
    const mod = Array.from({ length: size }, () => new Array(size).fill(false));
    const fn = Array.from({ length: size }, () => new Array(size).fill(false));
    const set = (x, y, dark) => { mod[y][x] = dark; fn[y][x] = true; };

    // Function patterns: timing, finders, alignment, version.
    for (let i = 0; i < size; i++) { set(6, i, i % 2 === 0); set(i, 6, i % 2 === 0); }
    const finder = (cx, cy) => {
      for (let dy = -4; dy <= 4; dy++) for (let dx = -4; dx <= 4; dx++) {
        const d = Math.max(Math.abs(dx), Math.abs(dy)), x = cx + dx, y = cy + dy;
        if (x >= 0 && x < size && y >= 0 && y < size) set(x, y, d !== 2 && d !== 4);
      }
    };
    finder(3, 3); finder(size - 4, 3); finder(3, size - 4);
    const align = [];
    if (ver > 1) {
      const n = Math.floor(ver / 7) + 2, step = Math.ceil((ver * 4 + 4) / (n * 2 - 2)) * 2;
      for (let pos = size - 7; align.length < n - 1; pos -= step) align.unshift(pos);
      align.unshift(6);
    }
    align.forEach((cx, i) => align.forEach((cy, j) => {
      const last = align.length - 1;
      if ((i === 0 && j === 0) || (i === 0 && j === last) || (i === last && j === 0)) return;
      for (let dy = -2; dy <= 2; dy++) for (let dx = -2; dx <= 2; dx++) set(cx + dx, cy + dy, Math.max(Math.abs(dx), Math.abs(dy)) !== 1);
    }));
    const format = (mask) => {
      const data = mask;                         // level M is 00 in the two high bits
      let rem = data;
      for (let i = 0; i < 10; i++) rem = (rem << 1) ^ ((rem >>> 9) * 0x537);
      const bits = ((data << 10) | rem) ^ 0x5412, bit = (i) => ((bits >>> i) & 1) !== 0;
      for (let i = 0; i <= 5; i++) set(8, i, bit(i));
      set(8, 7, bit(6)); set(8, 8, bit(7)); set(7, 8, bit(8));
      for (let i = 9; i < 15; i++) set(14 - i, 8, bit(i));
      for (let i = 0; i < 8; i++) set(size - 1 - i, 8, bit(i));
      for (let i = 8; i < 15; i++) set(8, size - 15 + i, bit(i));
      set(8, size - 8, true);
    };
    format(0);                                   // reserves the format area; the real bits come with the mask
    if (ver >= 7) {
      let rem = ver;
      for (let i = 0; i < 12; i++) rem = (rem << 1) ^ ((rem >>> 11) * 0x1f25);
      const bits = (ver << 12) | rem;
      for (let i = 0; i < 18; i++) {
        const dark = ((bits >>> i) & 1) !== 0, a = size - 11 + (i % 3), b = Math.floor(i / 3);
        set(a, b, dark); set(b, a, dark);
      }
    }

    // The data, in the zigzag from the bottom right.
    const cw = codewords(bytes, ver);
    let i = 0;
    for (let right = size - 1; right >= 1; right -= 2) {
      if (right === 6) right = 5;
      for (let vert = 0; vert < size; vert++) for (let j = 0; j < 2; j++) {
        const x = right - j, up = ((right + 1) & 2) === 0, y = up ? size - 1 - vert : vert;
        if (!fn[y][x] && i < cw.length * 8) { mod[y][x] = ((cw[i >>> 3] >>> (7 - (i & 7))) & 1) !== 0; i++; }
      }
    }

    // Try the eight masks and keep the one a scanner reads most easily.
    const MASK = [
      (x, y) => (x + y) % 2 === 0, (x, y) => y % 2 === 0, (x, y) => x % 3 === 0, (x, y) => (x + y) % 3 === 0,
      (x, y) => (Math.floor(x / 3) + Math.floor(y / 2)) % 2 === 0, (x, y) => ((x * y) % 2) + ((x * y) % 3) === 0,
      (x, y) => (((x * y) % 2) + ((x * y) % 3)) % 2 === 0, (x, y) => (((x + y) % 2) + ((x * y) % 3)) % 2 === 0,
    ];
    const apply = (m) => { for (let y = 0; y < size; y++) for (let x = 0; x < size; x++) if (!fn[y][x] && MASK[m](x, y)) mod[y][x] = !mod[y][x]; };
    const penalty = () => {
      let p = 0, dark = 0;
      const line = (get) => {
        for (let a = 0; a < size; a++) {
          let run = 1;
          for (let b = 1; b < size; b++) {
            if (get(a, b) === get(a, b - 1)) { run++; if (run === 5) p += 3; else if (run > 5) p++; } else run = 1;
          }
          for (let b = 0; b + 10 < size; b++) {       // 1:1:3:1:1 with four light on one side
            const s = [];
            for (let k = 0; k < 11; k++) s.push(get(a, b + k) ? 1 : 0);
            const t = s.join("");
            if (t === "10111010000" || t === "00001011101") p += 40;
          }
        }
      };
      line((a, b) => mod[a][b]);
      line((a, b) => mod[b][a]);
      for (let y = 0; y < size; y++) for (let x = 0; x < size; x++) {
        if (mod[y][x]) dark++;
        if (x + 1 < size && y + 1 < size && mod[y][x] === mod[y][x + 1] && mod[y][x] === mod[y + 1][x] && mod[y][x] === mod[y + 1][x + 1]) p += 3;
      }
      return p + Math.floor(Math.abs(dark * 20 - size * size * 10) / (size * size)) * 10;
    };
    let best = 0, bestP = Infinity;
    for (let m = 0; m < 8; m++) {
      apply(m); format(m);
      const p = penalty();
      if (p < bestP) { bestP = p; best = m; }
      apply(m);                                  // masking twice undoes it
    }
    apply(best); format(best);
    return { size, version: ver, mask: best, dark: (x, y) => mod[y][x] };
  }

  // svg draws the code with the four-module quiet zone a scanner needs, always dark on white —
  // an inverted code is not one every reader accepts.
  function svg(text) {
    const m = matrix(text), q = 4, n = m.size + q * 2, NS = "http://www.w3.org/2000/svg";
    let d = "";
    for (let y = 0; y < m.size; y++) for (let x = 0; x < m.size; x++) if (m.dark(x, y)) d += `M${x + q} ${y + q}h1v1h-1z`;
    const el = document.createElementNS(NS, "svg");
    el.setAttribute("viewBox", `0 0 ${n} ${n}`);
    el.setAttribute("shape-rendering", "crispEdges");
    const bg = document.createElementNS(NS, "rect"), path = document.createElementNS(NS, "path");
    bg.setAttribute("width", String(n)); bg.setAttribute("height", String(n)); bg.setAttribute("fill", "#fff");
    path.setAttribute("d", d); path.setAttribute("fill", "#000");
    el.append(bg, path);
    return el;
  }

  return { matrix, svg };
})();

if (typeof module !== "undefined") module.exports = QR;
