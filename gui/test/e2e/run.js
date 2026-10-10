// The wallet page against the REAL backend: a regtest node and wallet-rpc, started here, with the
// page driven in a headless browser (drive.js next to this file). Nothing is mocked except the thin
// Tauri bridge, which this script replaces with the same thing in Node: forward a JSON-RPC call to
// wallet-rpc with the token, return the result or the error message.
//
//   node gui/test/e2e/run.js <dir with powmachinad and powmachina-wallet-rpc> [path to a browser]
//
// The binaries are whatever was just built; a browser is Chromium, Chrome or Edge.
// JSQR=<path to jsQR.js> also reads the address's QR code back with an independent decoder.
"use strict";
const { spawn } = require("child_process");
const http = require("http"), fs = require("fs"), os = require("os"), path = require("path"), crypto = require("crypto");

const bin = process.argv[2], browser = process.argv[3] || process.env.BROWSER;
if (!bin || !browser) { console.error("usage: node run.js <binaries dir> <browser>"); process.exit(2); }
const exe = (n) => path.join(bin, n + (process.platform === "win32" ? ".exe" : ""));
const SRC = path.join(__dirname, "..", "..", "src");
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "powm-e2e-"));
const nodeToken = crypto.randomBytes(24).toString("hex"), walletToken = crypto.randomBytes(24).toString("hex");
const kids = [];
let finished = false;
function finish(code) {
  if (finished) return;
  finished = true;
  for (const k of kids) { try { k.kill(); } catch { /* already gone */ } }
  setTimeout(() => { try { fs.rmSync(tmp, { recursive: true, force: true }); } catch { /* a lock may linger a moment */ } process.exit(code); }, 800);
}
process.on("SIGINT", () => finish(130));
setTimeout(() => { console.error("TIMEOUT: the run did not finish in 6 minutes"); finish(3); }, 360000);

// start a sidecar and resolve with the address it says it listens on
function start(name, args, env, marker) {
  return new Promise((resolve, reject) => {
    const p = spawn(exe(name), args, { env: { ...process.env, ...env } });
    kids.push(p);
    let buf = "";
    const on = (d) => {
      buf += d;
      const i = buf.indexOf(marker);
      if (i >= 0) { const m = buf.slice(i + marker.length).match(/^(\S+?)[,\s]/); if (m) resolve(m[1]); }
    };
    p.stdout.on("data", on); p.stderr.on("data", on);
    p.on("exit", (c) => { if (!finished) { console.error(`${name} exited (${c})\n${buf.slice(-600)}`); reject(new Error(name + " exited")); finish(4); } });
  });
}
function rpc(addr, token, urlPath, body) {
  return new Promise((resolve, reject) => {
    const [host, port] = addr.split(":");
    const data = JSON.stringify(body);
    const req = http.request({ host, port, path: urlPath, method: "POST", headers: { Authorization: "Bearer " + token, "Content-Type": "application/json", "Content-Length": Buffer.byteLength(data) } }, (res) => {
      let out = ""; res.on("data", (d) => (out += d)); res.on("end", () => { try { resolve(JSON.parse(out)); } catch { reject(new Error("bad reply: " + out.slice(0, 200))); } });
    });
    req.on("error", reject); req.end(data);
  });
}

(async () => {
  const nodeAddr = await start("powmachinad", ["--regtest", "--datadir", path.join(tmp, "node"), "--rpc", "127.0.0.1:0", "--dag=auto"], { POWM_RPC_TOKEN: nodeToken }, "RPC listening on ");
  const walletAddr = await start("powmachina-wallet-rpc", ["--regtest", "--allow-manage", "--rpc-bind", "127.0.0.1:0", "--daemon-address", nodeAddr],
    { POWM_WALLET_RPC_TOKEN: walletToken, POWM_RPC_TOKEN: nodeToken }, "listening on ");
  const wallets = path.join(tmp, "wallets").replace(/\\/g, "/");
  fs.mkdirSync(wallets, { recursive: true });

  // the bridge the page gets instead of Tauri
  const shim = `window.__TAURI__ = (() => {
    const post = async (u, b) => { const r = await fetch(u, { method: "POST", body: JSON.stringify(b) }); const j = await r.json(); if (j.error) throw new Error(j.error); return j.result; };
    return { core: { invoke: async (cmd, a) => cmd === "rpc_info" ? { ready: true, network: "regtest" } : cmd === "default_wallet_dir" ? ${JSON.stringify(wallets)} : post("/rpc", a) },
      event: { listen: async () => () => {} }, app: { getVersion: async () => "e2e" }, dialog: { open: async () => null },
      opener: { openUrl: async () => {}, revealItemInDir: async () => {} } };
  })();
  window.__node = (method, params) => fetch("/node", { method: "POST", body: JSON.stringify({ method, params }) }).then((r) => r.json()).then((j) => { if (j.error) throw new Error(j.error); return j.result; });`;
  const read = (req) => new Promise((r) => { let b = ""; req.on("data", (d) => (b += d)); req.on("end", () => r(b ? JSON.parse(b) : {})); });
  const TYPES = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".woff2": "font/woff2" };
  const server = http.createServer(async (req, res) => {
    try {
      const send = (obj) => { res.writeHead(200, { "Content-Type": "application/json" }); res.end(JSON.stringify(obj)); };
      if (req.method === "POST" && req.url === "/rpc") {
        const a = await read(req);
        const r = await rpc(walletAddr, walletToken, a.wallet ? "/wallet/" + a.wallet : "/", { jsonrpc: "1.0", id: "ui", method: a.method, params: a.params });
        return send(r.error ? { error: r.error.message || "unknown error" } : { result: r.result ?? null });
      }
      if (req.method === "POST" && req.url === "/node") {
        const a = await read(req);
        const r = await rpc(nodeAddr, nodeToken, "/", { jsonrpc: "2.0", id: 1, method: a.method, params: a.params });
        return send(r.error ? { error: r.error.message || JSON.stringify(r.error) } : { result: r.result ?? null });
      }
      if (req.method === "POST" && req.url === "/report") {
        const a = await read(req);
        console.log(a.log.join("\n"));
        const bad = a.log.filter((l) => !l.startsWith("ok"));
        console.log(bad.length ? `\nE2E FAILED: ${bad.length} of ${a.log.length}` : `\nE2E OK: ${a.log.length} checks against the real node and wallet`);
        send({});
        return finish(bad.length ? 1 : 0);
      }
      if (req.url === "/__bridge.js") { res.writeHead(200, { "Content-Type": "text/javascript" }); return res.end(shim); }
      if (req.url === "/__drive.js") { res.writeHead(200, { "Content-Type": "text/javascript" }); return res.end(fs.readFileSync(path.join(__dirname, "drive.js"))); }
      if (req.url === "/__jsqr.js") { res.writeHead(200, { "Content-Type": "text/javascript" }); return res.end(process.env.JSQR ? fs.readFileSync(process.env.JSQR) : ""); }
      const name = req.url.split("?")[0].replace(/^\/+/, "") || "index.html";
      const file = path.join(SRC, name);
      if (!file.startsWith(SRC) || !fs.existsSync(file)) { res.writeHead(404); return res.end(); }
      let body = fs.readFileSync(file);
      if (name === "index.html") body = String(body).replace('<script src="app.js"></script>', '<script src="/__bridge.js"></script><script src="/__jsqr.js"></script><script src="app.js"></script><script src="/__drive.js"></script>');
      res.writeHead(200, { "Content-Type": TYPES[path.extname(name)] || "application/octet-stream" });
      res.end(body);
    } catch (e) { res.writeHead(500); res.end(String(e)); }
  });
  server.listen(0, "127.0.0.1", () => {
    const url = `http://127.0.0.1:${server.address().port}/index.html`;
    console.log(`node ${nodeAddr}, wallet-rpc ${walletAddr}, page ${url}`);
    const b = spawn(browser, ["--headless=new", "--disable-gpu", "--no-first-run", "--user-data-dir=" + path.join(tmp, "profile"), url], { stdio: "ignore" });
    kids.push(b);
  });
})().catch((e) => { console.error(e); finish(5); });
