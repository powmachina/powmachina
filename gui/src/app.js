// Wallet UI. It talks to wallet-rpc (Go) over JSON-RPC and nothing else — there is not a
// single cryptographic operation in this file and there never will be. Keys, signatures and
// the seed words live in Go; the page only ever sees what any stranger could see over RPC.
//
// Three rules this file keeps, because each one is a way to lose somebody's money:
//
//  1. Amounts are **never** floating point. The server sends text with eight decimals; we
//     convert through whole cogs (BigInt). A JS float lies the moment it adds 0.1 and 0.2.
//  2. Nothing that came from outside is ever parsed as HTML. Every address, label, txid and
//     error message goes in through `textContent`. There is no `innerHTML` with data here.
//  3. A failure is never shown as "nothing". If the wallet cannot be asked, the screen says
//     so and keeps the last known numbers, marked with their age.
"use strict";

// `window.__TAURI__` only exists with `withGlobalTauri` in tauri.conf.json. Without it the
// app would hang on the first screen with no explanation, so say it plainly instead.
const tauri = window.__TAURI__;
if (!tauri) {
  document.getElementById("boot-text").textContent =
    "The app bridge is missing (withGlobalTauri) — this is a build problem, not your machine.";
  throw new Error("window.__TAURI__ is not available");
}
const { invoke } = tauri.core;
const { listen } = tauri.event;

const COG = 100_000_000n;
const MATURITY = 100;            // chain.CoinbaseMaturity
const BLOCK_SECONDS = 120;       // chain.BlockTime
const POLL_MS = 5000;
const IDLE_LOCK_MS = 10 * 60 * 1000;
const HRP = { mainnet: "powm", testnet: "tpowm", regtest: "rpowm" };
// Where a transaction can be looked up. Only networks that have a public explorer are listed;
// on the others the link is simply not offered.
const EXPLORER = { testnet: "https://powmachina.com/explorer/tx/" };

let rpc = null;                  // { ready, network } — the token and address stay in Rust
let wallet = null;               // name of the loaded wallet
let walletPath = "";             // where its file is
let dataDir = "";

const st = {
  tab: "home", filter: "all", open: null,
  node: null,                    // the node's getinfo, or null when it did not answer
  walletOk: false,               // the wallet answered the last poll
  walletErr: "",                 // and when it did not, what it said instead
  everOk: false, lastOk: 0,
  bal: { avail: 0n, immature: 0n, held: 0n },
  txs: [],                       // newest first
  known: null,                   // keys seen so far — what is not in here is new
  fresh: new Set(),              // keys to animate once
  lanes: new Map(),              // block hash → "G" | "C"
  addr: "",                      // the address shown as "yours" — the first one unless another is picked
  addrs: [],                     // every address of the wallet, with its name and what it received
  renaming: null,
  send: { step: 1, to: "", cogs: 0n, fee: 0n, busy: false, all: false, parts: 1 },
  locked: false, timer: null, lastActive: Date.now(),
};

const $ = (id) => document.getElementById(id);
const show = (id) => {
  for (const s of document.querySelectorAll(".screen")) s.classList.toggle("hidden", s.id !== id);
};

// el builds an element. Text goes in as text nodes — this is the only way data reaches the page.
function el(tag, props, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === "class") e.className = v;
    else if (k === "on") for (const [ev, fn] of Object.entries(v)) e.addEventListener(ev, fn);
    else if (k === "data") for (const [dk, dv] of Object.entries(v)) e.dataset[dk] = dv;
    else e.setAttribute(k, v);
  }
  for (const kid of kids.flat()) if (kid != null && kid !== false) e.append(kid);
  return e;
}
const clear = (node) => { while (node.firstChild) node.firstChild.remove(); return node; };
const setMsg = (id, text, bad) => { const m = $(id); m.textContent = text; m.classList.toggle("bad", !!bad); };

// ---------- RPC ----------

// Calls go through Rust (`rpc_call`) rather than fetch: the page runs on the `tauri://`
// origin and a cross-origin call to 127.0.0.1 would be dropped without CORS headers. The
// side effect matters more than the cause — the wallet token never reaches the page.
async function call(method, params = [], walletName = wallet) {
  return invoke("rpc_call", { method, params, wallet: walletName || null });
}
const errText = (e) => String(e?.message ?? e ?? "unknown error");
const wrongPass = (e) => /passphrase|password/i.test(errText(e));

// ---------- amounts and time ----------

// Server text ("12.50000000") or what a person typed ("12,5") to cogs. No parseFloat anywhere
// near money.
function toCogs(text) {
  const s = String(text).trim().replace(",", ".");
  if (!/^-?\d{1,12}(\.\d{1,8})?$/.test(s)) throw new Error(`"${s}" is not a valid amount`);
  const neg = s.startsWith("-");
  const [whole, frac = ""] = s.replace("-", "").split(".");
  const cogs = BigInt(whole) * COG + BigInt((frac + "00000000").slice(0, 8));
  return neg ? -cogs : cogs;
}
const cogsOr0 = (v) => { try { return toCogs(v ?? "0"); } catch { return 0n; } };

// fmt writes cogs for reading: thousands separators, at least two decimals, no trailing zeros
// beyond that. `exact` is what goes back to the server: plain digits, eight decimals.
function fmt(cogs) {
  const neg = cogs < 0n, v = neg ? -cogs : cogs;
  const whole = (v / COG).toString().replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  let frac = (v % COG).toString().padStart(8, "0").replace(/0+$/, "");
  if (frac.length < 2) frac = frac.padEnd(2, "0");
  return (neg ? "−" : "") + whole + "." + frac;
}
const exact = (cogs) => (cogs / COG).toString() + "." + (cogs % COG).toString().padStart(8, "0");

function dur(seconds) {
  const m = Math.max(1, Math.round(seconds / 60));
  if (m < 60) return m + " min";
  const h = Math.floor(m / 60), r = m % 60;
  return h + " h" + (r ? " " + r + " min" : "");
}
function ago(unix) {
  if (!unix) return "";
  const s = Math.floor(Date.now() / 1000) - unix;
  if (s < 90) return "just now";
  if (s < 86400) return dur(s) + " ago";
  return new Date(unix * 1000).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
}
const n0 = (n) => Number(n ?? 0).toLocaleString("en-US");

// group writes an address in sixes. With `mark` the first and the last six are set apart —
// those are the two places a person actually compares.
function group(addr, mark) {
  const a = String(addr);
  if (!mark || a.length <= 18) return [a.match(/.{1,6}/g)?.join(" ") ?? ""];
  return [el("b", null, a.slice(0, 6)), " " + a.slice(6, -6).match(/.{1,6}/g).join(" ") + " ", el("b", null, a.slice(-6))];
}
const short = (a) => (a.length > 20 ? a.slice(0, 10) + "…" + a.slice(-6) : a);

// ---------- startup ----------

function led(node, share, cols = 40) {
  clear(node);
  const t = Math.floor(Date.now() / 400);
  for (let i = 0; i < cols; i++) {
    const done = i / cols < share, h = done ? 2 + ((i * 7 + t * 3) % 4) : 0, col = el("div");
    for (let k = 0; k < 5; k++) col.append(el("b", { class: k < h ? (k === h - 1 ? "on top" : "on") : "" }));
    node.append(col);
  }
}

async function boot() {
  // The node starts first and the wallet waits for its address, so this can take tens of
  // seconds (building the DAG). Hence the long wait and a message that says what we wait for.
  for (let i = 0; i < 480; i++) {
    led($("boot-led"), 1);
    const info = await invoke("rpc_info");
    if (info && info.ready) { rpc = info; break; }
    if (i === 20) $("boot-text").textContent = "Starting the node — the first run takes a moment…";
    await new Promise((r) => setTimeout(r, 250));
  }
  if (!rpc) return fatal("The wallet did not start. Close the app and open it again.");

  $("gate-net").textContent = rpc.network;
  dataDir = await invoke("default_wallet_dir");
  $("welcome-dir").textContent = "Wallets are kept in " + dataDir;
  try { $("set-version").textContent = await tauri.app.getVersion(); } catch { /* not vital */ }
  applyTheme();

  const loaded = await call("listwallets", [], null);
  if (loaded.length > 0) {
    // The backend still holds a wallet (the page was reloaded). Knowing its name is not
    // knowing its password, so ask before showing anything.
    wallet = loaded[0];
    walletPath = localStorage.getItem("powm.lastWallet") || "";
    return lock();
  }
  const last = localStorage.getItem("powm.lastWallet");
  if (last) { walletPath = last; return gate("g-unlock"); }
  gate("g-welcome");
}

function fatal(text) {
  stopPolling();
  clear($("boot-led"));
  $("boot-text").textContent = text;
  show("boot");
}
listen("wallet-rpc-exited", () => fatal("The wallet backend stopped. Close the app and open it again — your coins are safe."));
listen("node-exited", () => fatal("The node stopped. Close the app and open it again — your coins are safe."));

// ---------- before a wallet is open ----------

const fileName = (p) => String(p).split(/[\\/]/).pop() || "";

function gate(pane) {
  show("gate");
  for (const p of document.querySelectorAll("#gate .pane")) p.classList.toggle("hidden", p.id !== pane);
  if (pane === "g-unlock") {
    $("unlock-name").textContent = fileName(walletPath) || wallet || "your wallet";
    setMsg("unlock-msg", "Forgot it? You can restore the wallet from its 24 recovery words.");
    $("unlock-pass").value = "";
    $("unlock-pass").focus();
  }
  if (pane === "g-open") {
    if (!$("open-path").value) $("open-path").value = walletPath || dataDir + "/wallet";
    setMsg("open-msg", "");
  }
}
for (const b of document.querySelectorAll("[data-go]")) b.addEventListener("click", () => gate(b.dataset.go));

async function newPath(name) {
  return dataDir + "/" + (name.trim().replace(/[^\w.-]/g, "_") || "wallet");
}
function passProblem(p1, p2) {
  if (p1.length < 8) return "The password needs at least 8 characters.";
  if (p1 !== p2) return "The two passwords are not the same.";
  return "";
}
function opened(name, path) {
  wallet = name;
  walletPath = path;
  localStorage.setItem("powm.lastWallet", path);
}

// Create. The words are held only until the person has shown they wrote them down.
let seed = null;
$("g-create").addEventListener("submit", async (e) => {
  e.preventDefault();
  const p1 = $("create-pass").value, bad = passProblem(p1, $("create-pass2").value);
  if (bad) return setMsg("create-msg", bad, true);
  const btn = $("create-go");
  btn.disabled = true;
  setMsg("create-msg", "Creating…");
  try {
    const path = await newPath($("create-name").value);
    const r = await call("createwallet", [path, p1], null);
    opened(r.name, path);
    seed = String(r.mnemonic).trim().split(/\s+/);
    $("create-pass").value = $("create-pass2").value = "";
    setMsg("create-msg", "");
    showSeed();
  } catch (err) {
    setMsg("create-msg", /already exists/.test(errText(err))
      ? "A wallet with this name already exists. Pick another name, or open that one instead."
      : "The wallet could not be created: " + errText(err), true);
  } finally {
    btn.disabled = false;
  }
});
function showSeed() {
  const ol = clear($("seed-words"));
  for (const w of seed) ol.append(el("li", null, w));
  gate("g-seed");
}
let asked = [];
$("seed-next").addEventListener("click", () => {
  // Three positions, different every time, so the check cannot be passed from memory of the
  // first few words.
  const pool = seed.map((_, i) => i);
  asked = [];
  while (asked.length < 3) asked.push(pool.splice(Math.floor(Math.random() * pool.length), 1)[0]);
  asked.sort((a, b) => a - b);
  clear($("seed-words"));
  const box = clear($("confirm-fields"));
  for (const i of asked) {
    box.append(el("label", { class: "fld" }, el("span", null, "Word " + (i + 1)),
      el("div", { class: "in" }, el("input", { spellcheck: "false", autocapitalize: "off", "data-i": String(i) }))));
  }
  setMsg("confirm-msg", "");
  gate("g-confirm");
  box.querySelector("input").focus();
});
$("confirm-back").addEventListener("click", showSeed);
$("g-confirm").addEventListener("submit", (e) => {
  e.preventDefault();
  for (const inp of $("confirm-fields").querySelectorAll("input")) {
    if (inp.value.trim().toLowerCase() !== seed[Number(inp.dataset.i)]) {
      return setMsg("confirm-msg", `Word ${Number(inp.dataset.i) + 1} does not match. Look at your paper again, or go back to see the words.`, true);
    }
  }
  seed = null;
  clear($("confirm-fields"));
  enterMain();
});

// Restore.
const wordsOf = (text) => text.trim().toLowerCase().split(/\s+/).filter(Boolean);
$("restore-words").addEventListener("input", () => {
  const n = wordsOf($("restore-words").value).length;
  setMsg("restore-count", n === 24 ? "24 of 24 words." : `${n} of 24 words.`, n > 24);
});
$("g-restore").addEventListener("submit", async (e) => {
  e.preventDefault();
  const words = wordsOf($("restore-words").value);
  if (words.length !== 24) return setMsg("restore-msg", `24 words are needed — you entered ${words.length}.`, true);
  const p1 = $("restore-pass").value, bad = passProblem(p1, $("restore-pass2").value);
  if (bad) return setMsg("restore-msg", bad, true);
  const btn = $("restore-go");
  btn.disabled = true;
  setMsg("restore-msg", "Restoring — reading the chain from the start takes a while…");
  try {
    const path = await newPath($("restore-name").value || "restored");
    const r = await call("restorewallet", [path, p1, words.join(" "), 0], null);
    opened(r.name, path);
    $("restore-words").value = $("restore-pass").value = $("restore-pass2").value = "";
    setMsg("restore-msg", "");
    await enterMain();
  } catch (err) {
    setMsg("restore-msg", /already exists/.test(errText(err))
      ? "A wallet with this name already exists. Pick another name."
      : "These words did not restore a wallet: " + errText(err), true);
  } finally {
    btn.disabled = false;
  }
});

// Open a file, or come back to the last one.
$("open-browse").addEventListener("click", async () => {
  try {
    const picked = await tauri.dialog.open({ multiple: false, directory: false, defaultPath: dataDir });
    if (typeof picked === "string") $("open-path").value = picked;
  } catch { setMsg("open-msg", "Type or paste the path of the wallet file."); }
});
let loading = false;
async function load(path, pass, msgId, btn) {
  if (loading) return;                            // one attempt at a time
  if (!pass) return setMsg(msgId, "Type the wallet's password.", true);
  loading = true;
  btn.disabled = true;
  setMsg(msgId, "Opening…");
  try {
    const r = await call("loadwallet", [path, pass], null);
    if (r.already_loaded) {
      // The backend had this wallet open already and so did not look at the password at all.
      // Check it ourselves — a name is not a key.
      await call("walletpassphrase", [pass, 1], r.name);
      await call("walletlock", [], r.name).catch(() => {});
    }
    opened(r.name, path);
    setMsg(msgId, "");
    await enterMain();
  } catch (err) {
    setMsg(msgId, wrongPass(err) ? "That password isn't right. Try again."
      : "This file could not be opened: " + errText(err), true);
  } finally {
    loading = false;
    btn.disabled = false;
  }
}
$("g-open").addEventListener("submit", (e) => {
  e.preventDefault();
  const pass = $("open-pass").value;
  $("open-pass").value = "";
  load($("open-path").value.trim(), pass, "open-msg", $("open-go"));
});
$("g-unlock").addEventListener("submit", async (e) => {
  e.preventDefault();
  const pass = $("unlock-pass").value;
  $("unlock-pass").value = "";
  if (!st.locked) return load(walletPath, pass, "unlock-msg", $("unlock-go"));
  // The wallet is still loaded in the backend; the screen was only covered. Checking the
  // password costs nothing but must not leave signing unlocked, so lock straight after.
  if (!pass) return setMsg("unlock-msg", "Type the wallet's password.", true);
  $("unlock-go").disabled = true;
  try {
    await call("walletpassphrase", [pass, 1]);
    await call("walletlock", []).catch(() => {});
    st.locked = false;
    await enterMain();
  } catch (err) {
    setMsg("unlock-msg", wrongPass(err) ? "That password isn't right. Try again."
      : "The wallet did not answer: " + errText(err), true);
  } finally {
    $("unlock-go").disabled = false;
  }
});
$("unlock-other").addEventListener("click", async () => {
  if (st.locked && wallet) await call("unloadwallet", [wallet], null).catch(() => {});
  st.locked = false;
  wallet = null;
  gate("g-welcome");
});

// ---------- lock ----------

function lock() {
  st.locked = true;
  stopPolling();
  hideSecrets();
  resetSend();
  gate("g-unlock");
}
$("lock").addEventListener("click", lock);
for (const ev of ["pointerdown", "keydown", "wheel"]) {
  window.addEventListener(ev, () => { st.lastActive = Date.now(); }, { passive: true });
}
setInterval(() => {
  if (wallet && !st.locked && !$("main").classList.contains("hidden") && Date.now() - st.lastActive > IDLE_LOCK_MS) lock();
}, 15000);

// ---------- the wallet: polling ----------

async function enterMain() {
  st.lastActive = Date.now();
  st.known = null;
  st.everOk = false;
  $("net-note").textContent = rpc.network === "mainnet" ? "" : `${rpc.network[0].toUpperCase()}${rpc.network.slice(1)} — these coins have no value`;
  $("set-net").textContent = rpc.network;
  $("set-file").textContent = walletPath || wallet;
  $("send-to").placeholder = (HRP[rpc.network] || "powm") + "1…";
  show("main");
  setTab("home");
  await refresh();
  stopPolling();
  st.timer = setInterval(refresh, POLL_MS);
}
function stopPolling() { if (st.timer) clearInterval(st.timer); st.timer = null; }

const keyOf = (t) => `${t.txid}:${t.vout ?? ""}:${t.category}`;
function parseTx(t) {
  const cat = String(t.category);
  return {
    key: keyOf(t), txid: String(t.txid ?? ""), cat,
    kind: cat === "generate" || cat === "immature" ? "mined" : cat === "send" ? "sent" : "received",
    cogs: cogsOr0(t.amount), fee: t.fee != null ? cogsOr0(t.fee) : null,
    address: String(t.address ?? ""), label: String(t.label ?? ""),
    conf: Number(t.confirmations ?? 0), time: Number(t.time ?? 0),
    height: t.blockheight != null ? Number(t.blockheight) : null, hash: t.blockhash ? String(t.blockhash) : "",
  };
}
// A mined reward is spendable once the chain is CoinbaseMaturity blocks past its own block.
const blocksLeft = (t) => Math.max(1, MATURITY + 1 - t.conf);
const waiting = (t) => t.conf === 0 || t.cat === "immature";

// One poll at a time — but a poll that never comes back must not stop all the later ones, so the
// guard expires. And a poll that started for one wallet must not write its answers into another:
// the wallet is named in every call and checked again before anything is kept.
let refreshingSince = 0;
async function refresh() {
  if (st.locked || !wallet || (refreshingSince && Date.now() - refreshingSince < 30000)) return;
  const asked = wallet, started = (refreshingSince = Date.now());
  const stale = () => wallet !== asked || st.locked || refreshingSince !== started;
  try {
    const node = await call("getinfo", [], null).catch(() => null);
    if (stale()) return;
    st.node = node;
    try {
      const [bal, txs] = await Promise.all([call("getbalances", [], asked), call("listtransactions", ["*", 400], asked)]);
      if (stale()) return;
      const mine = bal.mine || bal;
      st.bal = { avail: cogsOr0(mine.trusted), immature: cogsOr0(mine.immature), held: cogsOr0(mine.locked) };
      const list = txs.map(parseTx).reverse();
      if (st.known) {
        const fresh = list.filter((t) => !st.known.has(t.key));
        for (const t of fresh) st.fresh.add(t.key);
        announce(fresh);
      }
      st.known = new Set(list.map((t) => t.key));
      st.txs = list;
      st.walletOk = true;
      st.everOk = true;
      st.lastOk = Date.now();
    } catch (err) {
      st.walletOk = false;         // keep the last numbers; the screen says they are old
      st.walletErr = errText(err);
    }
    if (!st.addr || st.tab === "receive") await loadAddresses();
    if (stale()) return;
    render();
    loadLanes();
  } finally {
    if (refreshingSince === started) refreshingSince = 0;
  }
}

async function loadAddresses() {
  const mine = wallet;
  try {
    let rows = await call("listreceivedbyaddress", [1, true], mine);
    if (!rows.length) {                          // a restored wallet has issued none yet
      await call("getnewaddress", [""], mine);
      rows = await call("listreceivedbyaddress", [1, true], mine);
    }
    if (wallet !== mine) return;
    st.addrs = rows.map((r) => ({ address: String(r.address), label: String(r.label ?? ""), cogs: cogsOr0(r.amount), imported: !!r.imported }));
    if (!st.addrs.some((a) => a.address === st.addr)) st.addr = st.addrs[0]?.address || "";
  } catch { /* shown as "—" until the wallet answers */ }
}

// Which lane mined a block is the chain's knowledge, not the wallet's; ask the node once per
// block and remember. A few per poll, newest first, so a long history never blocks the screen.
let laneBusy = false;
async function loadLanes() {
  if (laneBusy || !st.node) return;
  laneBusy = true;
  try {
    const need = st.txs.filter((t) => t.kind === "mined" && t.hash && !st.lanes.has(t.hash)).slice(0, 25);
    for (const t of need) {
      if (st.lanes.has(t.hash)) continue;
      const b = await call("getblock", { id: t.hash }, null).catch(() => null);
      st.lanes.set(t.hash, b && (b.lane === "G" || b.lane === "C") ? b.lane : "");
    }
    if (need.length && !st.locked) render();
  } finally {
    laneBusy = false;
  }
}

function announce(fresh) {
  const t = fresh.find((x) => x.kind !== "sent");
  if (!t) return;
  toast(t.kind === "mined" ? `+${fmt(t.cogs)} POWM mined` : t.conf === 0 ? `+${fmt(t.cogs)} POWM is arriving` : `+${fmt(t.cogs)} POWM received`);
}
let toastTimer = null;
function toast(text) {
  $("toast-text").textContent = text;
  $("toast").classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => $("toast").classList.remove("show"), 4500);
}

// ---------- the wallet: drawing ----------

// mode says whether the numbers on screen can be trusted right now, in three words a person
// can act on: fine, still catching up, or cut off.
function mode() {
  if (!st.node || !st.walletOk) return "off";
  return st.node.synced ? "ok" : "sync";
}
function syncShare() {
  const have = Number(st.node?.applied_height ?? 0);
  const target = Math.max(Number(st.node?.best_known ?? 0), Number(st.node?.height ?? 0));
  // 100 % belongs to a node that is actually done; while the last block is still being
  // applied, rounding must not show a full bar and make people stop waiting.
  return target > 0 ? Math.min(0.99, have / target) : 0;
}

function render() {
  if (st.locked) return;
  const y = $("body").scrollTop;                  // redrawing a list must not move what is being read
  renderSide();
  renderBanner();
  renderHome();
  renderActivity();
  renderSendState();
  renderReceive();
  renderSettings();
  $("body").scrollTop = y;
  st.fresh.clear();
}

function onTheWay() {
  const mined = st.txs.filter((t) => t.cat === "immature");
  const arriving = st.txs.filter((t) => t.kind === "received" && t.conf === 0);
  const total = st.bal.immature + arriving.reduce((s, t) => s + t.cogs, 0n);
  // The one that unlocks first is the one with the most blocks behind it.
  const next = mined.slice().sort((a, b) => b.conf - a.conf)[0] || null;
  return { mined, arriving, total, next };
}

function renderSide() {
  const m = mode(), ow = onTheWay();
  const avail = fmt(st.bal.avail);
  $("avail").textContent = st.everOk ? avail : "—";
  $("avail").parentElement.classList.toggle("long", avail.length > 11);
  $("asof").textContent = !st.everOk ? "waiting for the wallet…"
    : m === "off" ? `as of ${Date.now() - st.lastOk < 90000 ? "a moment" : dur((Date.now() - st.lastOk) / 1000)} ago — ${st.node ? "not updating" : "no connection"}`
    : m === "sync" ? "may be out of date — still catching up"
    : st.bal.held > 0n ? `${fmt(st.bal.held)} more comes back when your last payment confirms` : "";
  $("onway").classList.toggle("hidden", ow.total <= 0n);
  if (ow.total > 0n) {
    $("onway-amt").textContent = "+" + fmt(ow.total);
    $("onway-sub").textContent = ow.arriving.length
      ? `${fmt(ow.arriving.reduce((s, t) => s + t.cogs, 0n))} arriving with the next block`
      : ow.next ? `next ${fmt(ow.next.cogs)} unlocks in about ${dur(blocksLeft(ow.next) * BLOCK_SECONDS)}` : "";
  }
  clear($("side-addr")).append(...(st.addr ? group(st.addr, true) : ["—"]));
  $("status").textContent = m === "off" ? (st.node ? "Not updating" : "No connection") : m === "sync" ? `Catching up · ${Math.round(syncShare() * 100)} %` : "Up to date";
  $("status-block").textContent = st.node ? "block " + n0(st.node.height) : "";
}

function renderBanner() {
  const box = clear($("banner")), m = mode();
  if (st.tab === "send") return;                  // the send tab says the same thing in its own words
  if (!st.everOk) {
    box.append(el("div", { class: "banner" }, el("b", null, "Waiting for the wallet to answer…")));
    return;
  }
  if (m === "off" && st.node) {
    // The network is there and the wallet still did not answer. Calling that "no connection"
    // sends people to check their router; say what it is and show what the wallet said.
    box.append(el("div", { class: "banner" },
      el("b", null, "The wallet stopped updating."),
      el("p", null, "Your coins are safe. The numbers shown are the last ones the wallet gave; new payments won't show up and you can't send until it answers again. Closing the app and opening it again usually helps."),
      el("p", { class: "said" }, "What it said: " + (st.walletErr || "nothing").slice(0, 200)),
      el("button", { class: "btn", on: { click: refresh } }, "Try again")));
  } else if (m === "off") {
    box.append(el("div", { class: "banner" },
      el("b", null, "The wallet can't reach the network."),
      el("p", null, "Your coins are safe. New payments just won't show up until the connection is back, and you can't send for now."),
      el("button", { class: "btn", on: { click: refresh } }, "Try again")));
  } else if (m === "sync") {
    const share = syncShare(), bar = el("div", { class: "led" });
    led(bar, share, 44);
    const peers = Number(st.node.peers ?? 0);
    box.append(el("div", { class: "banner" },
      el("b", null, share > 0 ? `Catching up with the network — ${Math.round(share * 100)} %` : "Catching up with the network"),
      el("p", null, peers === 0 ? "Looking for other nodes to talk to. The numbers may be out of date until then."
        : `Block ${n0(st.node.applied_height)} of ${n0(Math.max(Number(st.node.best_known ?? 0), Number(st.node.height ?? 0)))}. The numbers may be out of date until it is done.`),
      bar));
  }
}

function laneTag(t) {
  const lane = st.lanes.get(t.hash);
  if (!lane) return null;
  return el("span", { class: "tag" }, el("i", { class: lane === "G" ? "sq g" : "sq" }), lane === "G" ? "GPU" : "CPU");
}
function segs(t) {
  const on = Math.min(10, Math.floor((t.conf / (MATURITY + 1)) * 10)), s = el("span", { class: "seg" });
  for (let i = 0; i < 10; i++) s.append(el("b", { class: i < on ? "on" : "" }));
  return s;
}
// One payment, as a sentence: what it was, how much, and whether you can use it yet.
function stateOf(t) {
  if (t.conf === 0) return [t.kind === "sent" ? "Sending — waiting for the next block" : "Arriving — waiting for the next block"];
  if (t.cat === "immature") return [`Unlocks in about ${dur(blocksLeft(t) * BLOCK_SECONDS)}`, segs(t)];
  return [t.kind === "sent" ? "Sent" : "Available"];
}
function explain(t) {
  if (t.conf === 0) return "The network has it. It becomes final when the next block includes it — usually within a few minutes.";
  if (t.cat === "immature") return `You found block ${n0(t.height)}. Freshly mined coins can be spent after ${MATURITY} more blocks — about ${dur(blocksLeft(t) * BLOCK_SECONDS)} to go.`;
  if (t.kind === "mined") return `You found block ${n0(t.height)}. This reward is yours to spend.`;
  return t.kind === "sent" ? "This payment is confirmed and cannot be reversed." : "This payment is confirmed and yours to spend.";
}
function row(t) {
  const title = t.kind === "mined" ? ["Mining reward", laneTag(t)] : t.kind === "sent" ? ["Sent to " + short(t.address)] : [t.label ? "Received · " + t.label : "Received"];
  const btn = el("button", { class: "row" + (waiting(t) ? " wait" : "") + (st.fresh.has(t.key) ? " new" : ""), "aria-expanded": String(st.open === t.key),
    on: { click: () => { st.open = st.open === t.key ? null : t.key; render(); } } },
    el("span", { class: "t" }, title),
    el("span", { class: "a" }, (t.cogs > 0n ? "+" : "") + fmt(t.cogs)),
    el("span", { class: "s" }, t.conf === 0 ? "just now" : ago(t.time)),
    el("span", { class: "st" }, stateOf(t)));
  if (st.open !== t.key) return [btn];
  const kv = el("div", { class: "kv" },
    el("div", null, el("span", null, "When"), el("span", null, t.time ? new Date(t.time * 1000).toLocaleString() : "just now")),
    el("div", null, el("span", null, "Block"), el("span", null, t.height ? n0(t.height) : "not in a block yet")),
    t.address ? el("div", null, el("span", null, t.kind === "sent" ? "To" : "Address"), el("span", null, group(t.address))) : null,
    t.fee != null ? el("div", null, el("span", null, "Network fee"), el("span", null, fmt(t.fee < 0n ? -t.fee : t.fee) + " POWM")) : null,
    el("div", null, el("span", null, "Transaction"), el("span", null, t.txid)));
  const copy = el("button", { class: "link", on: { click: () => copyText(t.txid, copy, "Copy transaction ID") } }, "Copy transaction ID");
  const base = EXPLORER[rpc.network];
  const open = base && /^[0-9a-f]{64}$/.test(t.txid)
    ? el("button", { class: "link", on: { click: () => openExternal(base + t.txid) } }, "Open in the block explorer") : null;
  return [btn, el("div", { class: "det" }, el("p", null, explain(t)), kv, el("div", { class: "btns" }, copy, open))];
}

function renderHome() {
  const ow = onTheWay(), has = st.txs.length > 0;
  $("home-empty").classList.toggle("hidden", has || !st.everOk || st.bal.avail > 0n);
  $("home-onway").classList.toggle("hidden", ow.mined.length === 0);
  if (ow.mined.length) {
    $("ow-amt").textContent = fmt(st.bal.immature);
    const n = ow.mined.length;
    clear($("ow-text")).append(`${n} mining reward${n === 1 ? " is" : "s are"} unlocking. The next ${fmt(ow.next.cogs)} POWM is yours to spend in about `,
      el("b", null, dur(blocksLeft(ow.next) * BLOCK_SECONDS)), ".");
    $("ow-bar").style.width = Math.min(100, (ow.next.conf / (MATURITY + 1)) * 100) + "%";
  }
  renderMining();
  $("home-latest").classList.toggle("hidden", !has);
  const box = clear($("latest"));
  for (const t of st.txs.slice(0, 5)) box.append(...row(t));
}

// The stream from the explorer, drawn for one wallet: GPU rewards above the line, solid; CPU
// below it, hatched. Width of a band = what you earned in that part of the last two hours.
function renderMining() {
  const now = Math.floor(Date.now() / 1000), span = 7200, N = 24, step = span / N;
  const recent = st.txs.filter((t) => t.kind === "mined" && t.time > now - span && st.lanes.get(t.hash));
  $("home-mining").classList.toggle("hidden", recent.length === 0);
  if (!recent.length) return;
  const g = new Array(N).fill(0n), c = new Array(N).fill(0n);
  let hourG = 0n, hourC = 0n;
  for (const t of recent) {
    const i = Math.min(N - 1, Math.max(0, Math.floor((t.time - (now - span)) / step)));
    const isG = st.lanes.get(t.hash) === "G";
    (isG ? g : c)[i] += t.cogs;
    if (t.time > now - 3600) isG ? (hourG += t.cogs) : (hourC += t.cogs);
  }
  $("rate-g").textContent = fmt(hourG);
  $("rate-c").textContent = fmt(hourC);
  // Smooth over three buckets, so one lucky block is a bump and not a spike.
  const smooth = (a) => a.map((_, i) => Number((a[Math.max(0, i - 1)] + a[i] + a[Math.min(N - 1, i + 1)]) / 3n));
  const sg = smooth(g), sc = smooth(c), max = Math.max(1, ...sg, ...sc);
  const W = 600, H = 64, mid = H / 2, k = (H / 2 - 4) / max, x = (i) => ((i / (N - 1)) * W).toFixed(1);
  const NS = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(NS, "svg");
  svg.setAttribute("class", "stream"); svg.setAttribute("viewBox", `0 0 ${W} ${H}`); svg.setAttribute("preserveAspectRatio", "none");
  svg.setAttribute("role", "img"); svg.setAttribute("aria-label", "Your mining rewards over the last two hours: GPU above, CPU below");
  const defs = document.createElementNS(NS, "defs"), pat = document.createElementNS(NS, "pattern"), ln = document.createElementNS(NS, "line");
  pat.setAttribute("id", "cpu-hatch"); pat.setAttribute("width", "6"); pat.setAttribute("height", "6");
  pat.setAttribute("patternUnits", "userSpaceOnUse"); pat.setAttribute("patternTransform", "rotate(45)");
  ln.setAttribute("class", "hatch"); ln.setAttribute("x1", "0"); ln.setAttribute("y1", "0"); ln.setAttribute("x2", "0"); ln.setAttribute("y2", "6");
  pat.append(ln); defs.append(pat); svg.append(defs);
  const band = (cls, vals, sign, fill) => {
    const p = document.createElementNS(NS, "polygon"), base = mid + sign * 1.5;
    p.setAttribute("class", cls);
    if (fill) p.setAttribute("fill", fill);
    p.setAttribute("points", `0,${base} ` + vals.map((v, i) => `${x(i)},${(base + sign * Math.max(1, v * k)).toFixed(1)}`).join(" ") + ` ${W},${base}`);
    svg.append(p);
  };
  band("g", sg, -1);
  band("c", sc, 1, "url(#cpu-hatch)");
  clear($("stream")).append(svg);
}

function renderActivity() {
  const F = { all: () => true, mined: (t) => t.kind === "mined", received: (t) => t.kind === "received", sent: (t) => t.kind === "sent" };
  const list = st.txs.filter(F[st.filter]), box = clear($("act-list"));
  for (const t of list.slice(0, 200)) box.append(...row(t));
  const note = $("act-note");
  // An empty list has three different causes and each gets its own sentence. "Nothing yet"
  // is only ever said when the wallet answered and really has nothing.
  note.textContent = !st.everOk ? "The wallet has not answered yet, so there is nothing to show. This is not an empty wallet."
    : list.length > 200 ? `Showing the latest 200 of ${list.length}.`
    : list.length ? "" : st.txs.length ? "Nothing of this kind yet."
    : "No payments yet. Everything you receive, mine or send will be listed here, newest first.";
  note.classList.toggle("hidden", !note.textContent);
}

// ---------- tabs ----------

function setTab(tab) {
  if (st.tab === "settings" && tab !== "settings") hideSecrets();
  st.renaming = null;
  st.tab = tab;
  st.open = null;
  for (const b of document.querySelectorAll("#tabs button")) b.classList.toggle("on", b.dataset.tab === tab);
  for (const t of document.querySelectorAll(".tab")) t.classList.toggle("hidden", t.id !== "tab-" + tab);
  $("body").scrollTop = 0;
  if (tab === "send" && st.send.step === 3) resetSend();
  render();
  if (tab === "receive") loadAddresses().then(renderReceive);
  if (tab === "send" && st.send.step === 1) $("send-to").focus();
}
for (const b of document.querySelectorAll("[data-tab]")) b.addEventListener("click", () => setTab(b.dataset.tab));
for (const b of document.querySelectorAll("#flt button")) {
  b.addEventListener("click", () => {
    st.filter = b.dataset.f;
    st.open = null;
    for (const x of document.querySelectorAll("#flt button")) x.classList.toggle("on", x === b);
    renderActivity();
  });
}

// ---------- copy ----------

async function copyText(text, btn, label) {
  try {
    await navigator.clipboard.writeText(text);
    btn.textContent = "Copied — paste it anywhere";
  } catch {
    btn.textContent = "Could not copy — select the text and press Ctrl+C";
  }
  setTimeout(() => { btn.textContent = label; }, 2200);
}
$("side-copy").addEventListener("click", () => st.addr && copyText(st.addr, $("side-copy"), "Copy address"));
$("recv-copy").addEventListener("click", () => st.addr && copyText(st.addr, $("recv-copy"), "Copy address"));
$("empty-copy").addEventListener("click", () => st.addr && copyText(st.addr, $("empty-copy"), "Copy my address"));
async function openExternal(url) {
  try { await tauri.opener.openUrl(url); } catch { toast("The browser could not be opened. Copy the transaction ID instead."); }
}

// ---------- receiving ----------

let qrFor = null;
function renderReceive() {
  if (st.tab !== "receive") return;
  const cur = st.addrs.find((a) => a.address === st.addr);
  $("recv-title").textContent = cur && cur.label ? "Your address · " + cur.label : "Your address";
  clear($("recv-addr")).append(...(st.addr ? group(st.addr, true) : ["—"]));
  if (qrFor !== st.addr) {
    clear($("recv-qr"));
    try { if (st.addr) $("recv-qr").append(QR.svg(st.addr)); } catch { /* no code is better than a wrong one */ }
    qrFor = st.addr;
  }
  if (st.renaming) return;                        // somebody is typing a name; do not pull the field away
  const box = clear($("addr-list"));
  for (const a of st.addrs) box.append(addrRow(a));
}
function addrRow(a) {
  const on = a.address === st.addr;
  const pick = on ? el("span", { class: "mut" }, "shown above")
    : el("button", { class: "link", on: { click: () => { st.addr = a.address; renderSide(); renderReceive(); $("body").scrollTop = 0; } } }, "Show");
  return el("div", { class: "arow" + (on ? " on" : ""), data: { addr: a.address } },
    el("span", { class: "n" + (a.label ? "" : " none") }, a.label || "no name"),
    el("span", { class: "r" }, fmt(a.cogs) + " received"),
    el("span", { class: "a" }, short(a.address) + (a.imported ? " · imported key" : "")),
    el("span", { class: "x" }, pick, el("button", { class: "link", on: { click: () => rename(a) } }, a.label ? "Rename" : "Name it")));
}
function rename(a) {
  const rowEl = [...$("addr-list").children].find((r) => r.dataset.addr === a.address);
  if (!rowEl || st.renaming) return;
  st.renaming = a.address;
  const input = el("input", { maxlength: "60", "aria-label": "A name for this address" });
  input.value = a.label;
  const done = async (save) => {
    if (st.renaming !== a.address) return;
    st.renaming = null;
    const name = input.value.trim();
    if (save && name !== a.label) {
      try { await call("setlabel", [a.address, name]); } catch (err) { toast("The name could not be saved: " + errText(err)); }
      await loadAddresses();
    }
    renderReceive();
  };
  input.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); done(true); } else if (e.key === "Escape") done(false); });
  input.addEventListener("blur", () => done(true));
  rowEl.firstChild.replaceWith(input);
  input.focus();
  input.select();
}
$("addr-new").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    st.addr = String(await call("getnewaddress", [$("addr-label").value.trim()]));
    $("addr-label").value = "";
    await loadAddresses();
    renderSide();
    renderReceive();
    $("body").scrollTop = 0;
  } catch (err) { setMsg("addr-msg", "A new address could not be made: " + errText(err), true); }
});

// ---------- sending ----------

function resetSend() {
  st.send = { step: 1, to: "", cogs: 0n, fee: 0n, busy: false, all: false, parts: 1 };
  $("send-to").value = $("send-amt").value = $("send-pass").value = "";
  showAll();
  showStep(1);
}
// "Send everything" is its own mode, not a number typed for you: the wallet works out what can
// go and what the fee is, so there is nothing to get wrong by a few cogs.
function showAll() {
  const all = st.send.all, amt = $("send-amt");
  amt.disabled = all;
  if (all) amt.value = "";
  amt.placeholder = all ? "Everything you can send" : "0.00";
  $("send-all").textContent = all ? "Type an amount instead" : "Send everything";
}
$("send-all").addEventListener("click", () => {
  st.send.all = !st.send.all;
  showAll();
  hints();
  if (!st.send.all) $("send-amt").focus();
});
function showStep(n) {
  st.send.step = n;
  for (const i of [1, 2, 3]) {
    $("send-" + i).classList.toggle("hidden", i !== n);
    $("s" + i).className = i === n ? "on" : i < n ? "done" : "";
  }
  if (n === 1) hints();
}

// The address is checked twice: here, instantly, for the mistakes people actually make —
// and by the wallet itself (bech32m checksum) before the payment can move to the next step.
function checkTo(v) {
  const hrp = HRP[rpc.network] || "powm";
  if (!v) return [false, "Paste or type the address you were given.", false];
  if (v === st.addr) return [false, "That is your own address. Paste the address of the person you are paying.", true];
  const low = v.toLowerCase();
  if (v !== low && v !== v.toUpperCase()) return [false, "An address is all lower case. Copy and paste is safest.", true];
  for (const [net, h] of Object.entries(HRP)) {
    if (net !== rpc.network && low.startsWith(h + "1")) return [false, `That is a ${net} address. This wallet is on ${rpc.network}, where addresses start with ${hrp}1.`, true];
  }
  if (!low.startsWith(hrp + "1")) return [false, `A PowMachina ${rpc.network} address starts with ${hrp}1. This looks like something else.`, true];
  if (!/^[a-z]+1[023456789ac-hj-np-z]+$/.test(low)) return [false, "There is a character in there that an address can't contain. Copy and paste is safest.", true];
  const want = st.addr ? st.addr.length : 0;
  if (want && low.length !== want) {
    return [false, low.length < want ? `The address is too short — ${want - low.length} characters are missing. Was it cut off when copying?`
      : "The address is too long. Check that nothing extra got pasted with it.", true];
  }
  return [true, "Looks right. The wallet checks it once more on the next step.", false];
}
function checkAmt(v) {
  const ow = onTheWay().total;
  if (st.send.all) {
    return st.bal.avail > 0n ? [true, `All ${fmt(st.bal.avail)} POWM you have available, minus the network fee. The exact numbers are on the next step.`, false]
      : [false, `There is nothing available to send yet${ow > 0n ? ` — ${fmt(ow)} is still on the way` : ""}.`, true];
  }
  if (!v) return [false, `You have ${fmt(st.bal.avail)} POWM available.`, false];
  let cogs;
  try { cogs = toCogs(v); } catch { return [false, "Type a number, for example 25 or 0.5 — at most 8 decimals.", true]; }
  if (cogs <= 0n) return [false, "The amount has to be more than zero.", true];
  if (cogs > st.bal.avail) return [false, `That is more than you can send. You have ${fmt(st.bal.avail)} available${ow > 0n ? ` — another ${fmt(ow)} is still on the way` : ""}.`, true];
  return [true, `You will have about ${fmt(st.bal.avail - cogs)} left, minus a small network fee.`, false];
}
function hints() {
  const a = checkTo($("send-to").value.trim()), b = checkAmt($("send-amt").value.trim());
  for (const [id, r] of [["to", a], ["amt", b]]) {
    setMsg("m-" + id, r[1], r[2]);
    $("f-" + id).classList.toggle("bad", r[2]);
  }
  $("send-check").disabled = !(a[0] && b[0]) || st.send.busy;
}
$("send-to").addEventListener("input", hints);
$("send-amt").addEventListener("input", hints);
$("send-paste").addEventListener("click", async () => {
  try {
    $("send-to").value = (await navigator.clipboard.readText()).trim();
    hints();
    $("send-amt").focus();
  } catch {
    setMsg("m-to", "Click into the field and press Ctrl+V.", false);
    $("send-to").focus();
  }
});

function renderSendState() {
  const m = mode(), paused = m !== "ok" && st.send.step !== 3;
  $("send-paused").classList.toggle("hidden", !paused);
  $("send-flow").classList.toggle("hidden", paused);
  if (paused) {
    $("send-paused-text").textContent = m === "off"
      ? "The wallet can't reach the network right now. Nothing is lost — try again when the connection is back."
      : "The wallet is still catching up with the network. You can send as soon as it is done.";
  } else if (st.send.step === 1 && st.tab === "send") hints();
}

// Step 1 → 2: the wallet validates the address and builds the payment without signing or
// sending it, which tells us the real fee. Nothing has left the wallet at this point.
$("send-1").addEventListener("submit", async (e) => {
  e.preventDefault();
  if (st.send.busy) return;
  const to = $("send-to").value.trim().toLowerCase();
  const all = st.send.all;
  let cogs = 0n;
  if (!all) { try { cogs = toCogs($("send-amt").value.trim()); } catch { return hints(); } }
  st.send.busy = true;
  $("send-check").disabled = true;
  setMsg("m-amt", "Checking…");
  try {
    const v = await call("validateaddress", [to]);
    if (!v.isvalid) {
      setMsg("m-to", "The wallet does not accept this address — there is a typo in it. Copy it again from the person you are paying.", true);
      $("f-to").classList.add("bad");
      return setMsg("m-amt", "");
    }
    if (v.ismine) {
      setMsg("m-to", "That address belongs to this wallet. Paste the address of the person you are paying.", true);
      $("f-to").classList.add("bad");
      return setMsg("m-amt", "");
    }
    let fee, parts = 1, note = "";
    if (all) {
      // The wallet says what "everything" comes to — without signing or sending anything.
      const plan = await call("sweepall", [to, 1, 500, true]);
      parts = Number(plan.transactions ?? 0);
      if (parts === 0) {
        $("f-amt").classList.add("bad");
        return setMsg("m-amt", "Nothing can be sent right now: your coins are still waiting for their first block. Try again in a few minutes.", true);
      }
      cogs = cogsOr0(plan.amount);
      fee = cogsOr0(plan.fee);
      if (parts > 1) note = `This goes out as ${parts} payments, because the wallet holds many small coins (mining rewards). The total is the same.`;
      const stays = st.bal.avail - cogs - fee;
      if (stays > 0n) note += (note ? " " : "") + `${fmt(stays)} POWM stays behind until your last payment confirms.`;
    } else {
      const raw = await call("createrawtransaction", [[], [{ [to]: exact(cogs) }]]);
      fee = cogsOr0((await call("fundrawtransaction", [raw])).fee);
    }
    st.send.to = to; st.send.cogs = cogs; st.send.fee = fee; st.send.parts = parts;
    clear($("chk-to")).append(...group(to, true));
    $("chk-amt-k").textContent = all ? "Arrives" : "Amount";
    $("chk-amt").textContent = fmt(cogs) + " POWM";
    $("chk-fee").textContent = fmt(fee) + " POWM";
    $("chk-total").textContent = fmt(cogs + fee) + " POWM";
    $("chk-note").textContent = note;
    $("chk-note").classList.toggle("hidden", !note);
    $("send-go").textContent = all ? `Send everything — ${fmt(cogs + fee)} POWM` : `Send ${fmt(cogs + fee)} POWM`;
    $("send-pass").value = "";
    setMsg("m-pass", "A payment can't be taken back once it's sent.");
    $("f-pass").classList.remove("bad");
    showStep(2);
    $("send-pass").focus();
  } catch (err) {
    const t = errText(err);
    setMsg("m-amt", /insufficient|not enough|funds/i.test(t)
      ? `With the network fee this is a little more than you have available (${fmt(st.bal.avail)}). Lower the amount slightly.`
      : "The payment could not be prepared: " + t, true);
    $("f-amt").classList.add("bad");
  } finally {
    st.send.busy = false;
    if (st.send.step === 1) $("send-check").disabled = false;
  }
});
$("send-back").addEventListener("click", () => { if (!st.send.busy) { $("send-pass").value = ""; showStep(1); } });

// Step 2 → 3: unlock for this one payment, send, lock again whatever happened.
$("send-2").addEventListener("submit", async (e) => {
  e.preventDefault();
  if (st.send.busy) return;                       // a double click must not pay twice
  const pass = $("send-pass").value;
  $("send-pass").value = "";
  if (!pass) { $("f-pass").classList.add("bad"); return setMsg("m-pass", "Type your wallet password to send.", true); }
  st.send.busy = true;
  const btn = $("send-go"), label = btn.textContent;
  btn.disabled = $("send-back").disabled = true;
  btn.textContent = "Sending…";
  try {
    try {
      await call("walletpassphrase", [pass, 60]);
    } catch (err) {
      $("f-pass").classList.add("bad");
      setMsg("m-pass", wrongPass(err) ? "That password isn't right. Nothing was sent — try again." : "Nothing was sent: " + errText(err), true);
      $("send-pass").focus();
      return;
    }
    let txids, sent = st.send.cogs, partial = "";
    try {
      if (st.send.all) {
        const r = await call("sweepall", [st.send.to]);
        txids = (r.txids || []).map(String);
        sent = cogsOr0(r.amount);
        partial = r.error ? String(r.error) : "";   // some of it went out, the rest did not
      } else {
        txids = [String(await call("sendtoaddress", [st.send.to, exact(st.send.cogs)]))];
      }
    } catch (err) {
      $("f-pass").classList.add("bad");
      setMsg("m-pass", "Nothing was sent: " + errText(err), true);
      return;
    }
    $("sent-text").textContent = `${fmt(sent)} POWM is on its way to ${short(st.send.to)}. It becomes final with the next block, usually within a few minutes — you can close the wallet, it will still go through.`
      + (partial ? ` Not everything went out: ${partial}. What is left is still in your wallet.` : "");
    $("sent-txid-k").textContent = txids.length === 1 ? "Transaction" : `${txids.length} transactions`;
    $("sent-txid").textContent = txids.join(" ");
    showStep(3);
    refresh();
  } finally {
    await call("walletlock", []).catch(() => {});
    st.send.busy = false;
    btn.disabled = $("send-back").disabled = false;
    btn.textContent = label;
  }
});
$("send-again").addEventListener("click", () => { resetSend(); $("send-to").focus(); });

// ---------- settings ----------

function renderSettings() {
  const n = st.node;
  $("set-peers").textContent = !n ? "offline" : Number(n.peers ?? 0) === 0 ? "looking for peers…" : `${n.peers} ${Number(n.peers) === 1 ? "peer" : "peers"}`;
  $("set-height").textContent = n ? n0(n.height) : "—";
  $("set-age").textContent = n && n.tip_time ? ago(Number(n.tip_time)) : "";
}

const THEMES = ["auto", "light", "dark"], THEME_NAME = { auto: "System", light: "Light", dark: "Dark" };
function applyTheme() {
  const t = THEMES.includes(localStorage.getItem("powm.theme")) ? localStorage.getItem("powm.theme") : "auto";
  document.documentElement.dataset.scheme = t;
  $("set-theme").textContent = THEME_NAME[t];
}
$("set-theme").addEventListener("click", () => {
  const cur = document.documentElement.dataset.scheme;
  localStorage.setItem("powm.theme", THEMES[(THEMES.indexOf(cur) + 1) % THEMES.length]);
  applyTheme();
});

// Recovery words. They are the whole wallet forever, so: a warning first, the password every
// time, and they leave the screen by themselves.
let wordsTimer = null, keyTimer = null;
function hideWords() {
  clearTimeout(wordsTimer);
  clear($("words-panel")).classList.add("hidden");
  clear($("paper"));
}
function hideKeys() {
  clearTimeout(keyTimer);
  clear($("keys-panel")).classList.add("hidden");
}
function hideSecrets() { hideWords(); hideKeys(); }
$("set-words").addEventListener("click", () => {
  hideKeys();
  const pass = el("input", { type: "password", autocomplete: "current-password" });
  const msg = el("div", { class: "msg" });
  const form = el("form", { autocomplete: "off", on: { submit: async (e) => {
    e.preventDefault();
    const p = pass.value;
    pass.value = "";
    if (!p) { msg.textContent = "Type the wallet's password."; msg.classList.add("bad"); return; }
    try {
      const r = await call("getseed", [wallet, p], null);
      revealWords(String(r.mnemonic).trim().split(/\s+/), String(r.address ?? ""), String(r.network ?? rpc.network));
    } catch (err) {
      msg.textContent = wrongPass(err) ? "That password isn't right." : "The words could not be read: " + errText(err);
      msg.classList.add("bad");
    }
  } } },
    el("b", null, "Is anyone looking at your screen?"),
    el("p", null, "Whoever sees these 24 words can take all your coins. Never type them into a website and never send them to anyone — nobody from PowMachina will ever ask for them."),
    el("label", { class: "fld" }, el("span", null, "Your wallet password"), el("div", { class: "in" }, pass), msg),
    el("div", { class: "btns" }, el("button", { type: "submit", class: "btn p" }, "Show the words"),
      el("button", { type: "button", class: "btn", on: { click: hideWords } }, "Cancel")));
  clear($("words-panel")).append(form);
  $("words-panel").classList.remove("hidden");
  $("body").scrollTop = 0;
  pass.focus();
});
function revealWords(words, address, network) {
  const ol = el("ol", { class: "words" });
  for (const w of words) ol.append(el("li", null, w));
  clear($("words-panel")).append(
    el("b", null, "Your 24 recovery words"),
    el("p", null, "Write them on paper, in this order. Not a photo, not a file. They disappear from the screen in two minutes."),
    ol,
    el("div", { class: "btns" }, el("button", { class: "btn p", on: { click: hideWords } }, "Hide them"),
      el("button", { class: "btn", on: { click: () => printPaper(words, address, network) } }, "Print a paper backup")));
  clearTimeout(wordsTimer);
  wordsTimer = setTimeout(hideWords, 120000);
}
function printPaper(words, address, network) {
  const ol = el("ol");
  for (const w of words) ol.append(el("li", null, w));
  clear($("paper")).append(
    el("h1", null, `PowMachina — paper backup (${network})`),
    el("div", { class: "box" }, el("b", null, "Address: "), address),
    el("div", { class: "box" }, el("b", null, "24 words:"), ol),
    el("p", null, "Whoever holds these 24 words holds the coins. Keep this sheet where you would keep cash — never photograph it and never type it into a website."),
    el("p", null, "To restore: PowMachina Wallet → Restore from recovery words."));
  window.print();
  clear($("paper"));
}

// A single key, out or in. Rarely needed — the recovery words already cover every address the
// wallet made — and the two directions carry opposite risks, so each says its own.
$("set-keys").addEventListener("click", async () => {
  hideWords();
  if (!st.addrs.length) await loadAddresses();
  const passField = () => el("input", { type: "password", autocomplete: "current-password" });
  // Unlock for one operation, run it, lock again whatever happened.
  const unlocked = async (pass, msg, fn) => {
    if (!pass) { msg.textContent = "Type the wallet's password."; msg.classList.add("bad"); return; }
    try {
      await call("walletpassphrase", [pass, 60]);
      await fn();
    } catch (err) {
      msg.textContent = wrongPass(err) ? "That password isn't right. Nothing was done." : errText(err);
      msg.classList.add("bad");
    } finally {
      await call("walletlock", []).catch(() => {});
    }
  };

  const pick = el("select", { "aria-label": "Address" });
  for (const a of st.addrs) pick.append(el("option", { value: a.address }, (a.label ? a.label + " · " : "") + short(a.address)));
  const outPass = passField(), outMsg = el("div", { class: "msg" }), outBox = el("div");
  const exportForm = el("form", { autocomplete: "off", on: { submit: async (e) => {
    e.preventDefault();
    const p = outPass.value;
    outPass.value = "";
    outMsg.textContent = ""; outMsg.classList.remove("bad");
    await unlocked(p, outMsg, async () => {
      const key = String(await call("dumpprivkey", [pick.value]));
      const copy = el("button", { type: "button", class: "link", on: { click: () => copyText(key, copy, "Copy the key") } }, "Copy the key");
      clear(outBox).append(el("div", { class: "secret" }, key), copy, el("p", null, "It disappears from the screen in one minute."));
      clearTimeout(keyTimer);
      keyTimer = setTimeout(() => clear(outBox), 60000);
    });
  } } },
    el("h4", null, "Export a key"),
    el("p", null, "Whoever has this key can spend everything that address holds, now and later. Your 24 recovery words already back up every address — export a key only to move one address into another wallet."),
    el("label", { class: "fld" }, el("span", null, "Address"), el("div", { class: "in" }, pick)),
    el("label", { class: "fld" }, el("span", null, "Your wallet password"), el("div", { class: "in" }, outPass), outMsg),
    el("div", { class: "btns" }, el("button", { type: "submit", class: "btn" }, "Show the key")), outBox);

  const inKey = el("input", { spellcheck: "false", autocapitalize: "off", "aria-label": "Private key" });
  const inName = el("input", { maxlength: "60" });
  const inPass = passField(), inMsg = el("div", { class: "msg" });
  const importForm = el("form", { autocomplete: "off", on: { submit: async (e) => {
    e.preventDefault();
    const p = inPass.value, key = inKey.value.trim();
    inPass.value = "";
    inMsg.classList.remove("bad");
    if (!key) { inMsg.textContent = "Paste the key first."; inMsg.classList.add("bad"); return; }
    inMsg.textContent = "Importing and reading the chain for its payments — this can take a few minutes…";
    let done = false;
    await unlocked(p, inMsg, async () => {
      await call("importprivkey", [key, inName.value.trim(), true]);
      done = true;
    });
    if (done) {
      inKey.value = inName.value = "";
      inMsg.textContent = "Imported. Its address and payments are now part of this wallet.";
      await loadAddresses();
      refresh();
    }
  } } },
    el("h4", null, "Import a key"),
    el("p", null, "An imported key is NOT covered by your 24 recovery words. Keep your own copy of it, or the coins on it are lost with this computer."),
    el("label", { class: "fld" }, el("span", null, "Private key"), el("div", { class: "in" }, inKey)),
    el("label", { class: "fld" }, el("span", null, "A name for it (optional)"), el("div", { class: "in" }, inName)),
    el("label", { class: "fld" }, el("span", null, "Your wallet password"), el("div", { class: "in" }, inPass), inMsg),
    el("div", { class: "btns" }, el("button", { type: "submit", class: "btn" }, "Import the key")));

  clear($("keys-panel")).append(el("b", null, "Move a single key"), exportForm, importForm,
    el("div", { class: "btns" }, el("button", { class: "btn p", on: { click: hideKeys } }, "Close")));
  $("keys-panel").classList.remove("hidden");
  $("body").scrollTop = 0;
});
$("set-reveal").addEventListener("click", async () => {
  try { await tauri.opener.revealItemInDir(walletPath); } catch { toast("The folder could not be opened. The path is shown above."); }
});

// Password change.
$("set-pass").addEventListener("click", () => { $("pass-form").classList.toggle("hidden"); setMsg("pass-msg", ""); $("pass-old").focus(); });
$("pass-cancel").addEventListener("click", () => { $("pass-form").classList.add("hidden"); $("pass-form").reset(); });
$("pass-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const old = $("pass-old").value, p1 = $("pass-new").value, bad = passProblem(p1, $("pass-new2").value);
  if (bad) return setMsg("pass-msg", bad, true);
  try {
    await call("walletpassphrasechange", [old, p1]);
    $("pass-form").reset();
    $("pass-form").classList.add("hidden");
    toast("The password was changed.");
  } catch (err) {
    setMsg("pass-msg", wrongPass(err) ? "The current password isn't right. Nothing was changed." : "The password could not be changed: " + errText(err), true);
  }
});

$("set-close").addEventListener("click", async () => {
  stopPolling();
  hideSecrets();
  await call("unloadwallet", [wallet], null).catch(() => {});
  wallet = null;
  refreshingSince = 0;
  st.addr = "";
  st.addrs = [];
  st.known = null;
  st.bal = { avail: 0n, immature: 0n, held: 0n };
  qrFor = null;
  st.txs = [];
  st.lanes.clear();
  localStorage.removeItem("powm.lastWallet");
  gate("g-welcome");
});

let rescanning = false;
$("set-rescan").addEventListener("click", async () => {
  if (rescanning) return;
  rescanning = true;
  setMsg("set-msg", "Reading the chain again — this can take a few minutes. You can keep using the wallet.");
  try {
    await call("rescanblockchain", []);
    setMsg("set-msg", "Done. The list of payments is up to date.");
    refresh();
  } catch (err) {
    setMsg("set-msg", "The chain could not be read again: " + errText(err), true);
  } finally {
    rescanning = false;
  }
});

// Every startup failure has to end in a sentence on screen. An empty window with a running
// equalizer is the worst thing to show — it looks like the app is working.
boot().catch((e) => fatal("The wallet did not start: " + errText(e)));
