// The QR generator against fixed vectors. Every code listed here was read back correctly by jsQR, a
// decoder that shares no code with the generator (914 codes in all: every length from 1 to 213 bytes,
// every version 1–10 and all eight masks). The hashes pin that behaviour: if a change to qr.js alters a
// single module of any of them, this fails, and the change has to be verified against a decoder again
// before the vectors are replaced.
//
//   node gui/test/qr.test.js
"use strict";
const QR = require("../src/qr.js"), crypto = require("crypto");
const VECTORS = [
  ["a",1,5,"2e7add7dfd3288d4ed23295399f43abf38be57f9a3df265ce870eb6fa468237e"],
  ["PowMachina",1,2,"0cc1499c860f472adf01ddd74b63bcf7e3dc928ba2a3b89a52e57fc40a524539"],
  ["tpowm1qvlcvh7ekwxyyaxyffgrm0zuvn4cf5xeekzfvergtl6kuevw74r4s9qxtff",5,2,"b72bd9486f9ad56eb32fd6663516acaaf3be3c01ec0ad6d6fd49b99b2f784039"],
  ["powm1qfek5kcv2clutw44watpmfqfr0u09z0n47pd0ze2e7evkgdcrfeyqq97s9t",5,2,"cd98fe88552da71e2415aa525fe84a1cf0f20b7a31ab894ac79cee3384642eb2"],
  ["xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",4,0,"6224a6f3a0bbc68bf1f11309238651e9ef17e8fe344d97b66bee65b9b59d04bb"],
  ["xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",5,2,"952bd67e6a9b1f11681ae16d531d53e6da4c07736774af4db06493f6e2124096"],
  ["yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy",6,0,"21abcf50bdbf15b3b5dc6b5871a30d4ecc07ab7da5e33ed3155f86efb6e3702a"],
  ["zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",8,0,"19d4ec42db06784f942fa4f4c12abdc3125e1502f59a2a4fe9e5430a49d3e9ee"],
  ["wwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwww",9,4,"8036e3d83272143c258c5cf6c88e25b6012d2e782af96888a6d0518761c27871"],
  ["vvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvv",10,2,"e11a749829a138e2eb66e411e89d56eb31e38bf32aa2272aec905c0273bed93e"],
  ["uuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuu",10,1,"46a19b925ebf97f4c5f4762e3ab5389305a66657ac736e2ad09e02fb44920c06"],
];
let bad = 0;
for (const [text, version, mask, hash] of VECTORS) {
  const m = QR.matrix(text);
  let bits = "";
  for (let y = 0; y < m.size; y++) for (let x = 0; x < m.size; x++) bits += m.dark(x, y) ? "1" : "0";
  const got = crypto.createHash("sha256").update(bits).digest("hex");
  if (m.version !== version || m.mask !== mask || got !== hash || m.size !== version * 4 + 17) {
    bad++;
    console.log(`FAIL ${JSON.stringify(text).slice(0, 40)}: version ${m.version}/${version}, mask ${m.mask}/${mask}, ${got === hash ? "same modules" : "different modules"}`);
  }
}
let refused = false;
try { QR.matrix("x".repeat(214)); } catch { refused = true; }
if (!refused) { bad++; console.log("FAIL 214 bytes should not fit"); }
console.log(bad ? `QR: ${bad} failed` : `QR: ${VECTORS.length} vectors match, oversize refused`);
process.exit(bad ? 1 : 0);
