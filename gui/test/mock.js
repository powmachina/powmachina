// A stand-in for the Tauri bridge and wallet-rpc, for testing the page in a plain browser.
// It answers the same methods with the same shapes the Go backend uses. Not shipped.
(() => {
  const flags = new Set(location.hash.slice(1).split(",").filter(Boolean));
  const PASS = "hunter22", ME = "tpowm1qvlcvh7ekwxyyaxyffgrm0zuvn4cf5xeekzfvergtl6kuevw74r4s9qxtff";
  const OTHER = "tpowm1q9x4f8t2mzk7ha2c0m3d9kq5wj8r6n4ptl0vyx3ceu7hgs2dacfk4e22acd";
  const WORDS = "cabin velvet orbit ladder museum quiet harbor signal copper lantern fabric meadow rocket pencil window forest anchor marble silver tunnel garden bridge planet candle";
  const M = { loaded: flags.has("reloaded") ? ["wallet"] : [], unlockedUntil: 0, height: 1200, calls: [], sent: [], swept: [], opened: [], txs: [], addrs: [{ address: ME, label: "" }], fail: flags.has("txfail"), off: false, synced: !flags.has("sync") };
  const now = () => Math.floor(Date.now() / 1000);
  const hex = (n) => n.toString(16).padStart(64, "0");
  const amt = (c) => (c < 0 ? "-" : "") + (Math.abs(c) / 1e8).toFixed(8);
  if (!flags.has("empty")) {
    for (let i = 0; i < 46; i++) { const h = M.height - i * 2 - 1; M.txs.push({ txid: hex(h * 7), vout: 0, category: M.height - h + 1 > 100 ? "generate" : "immature", amount: amt((h + 1) * 800000), address: ME, confirmations: M.height - h + 1, time: now() - (M.height - h) * 120, blockheight: h, blockhash: hex(h) }); }
    for (let i = 0; i < 20; i++) { const h = M.height - 104 - i * 3; M.txs.push({ txid: hex(h * 7), vout: 0, category: "generate", amount: amt((h + 1) * 800000), address: ME, confirmations: M.height - h + 1, time: now() - (M.height - h) * 120, blockheight: h, blockhash: hex(h) }); }
    M.txs.push({ txid: hex(999001), vout: 1, category: "receive", amount: "40.00000000", address: ME, label: "<img src=x onerror=alert(1)>", confirmations: 48, time: now() - 47 * 120, blockheight: M.height - 47, blockhash: hex(M.height - 47) });
    M.txs.push({ txid: hex(999002), vout: 0, category: "send", amount: "-25.00000000", fee: "-0.00020000", address: OTHER, confirmations: 13, time: now() - 12 * 120, blockheight: M.height - 12, blockhash: hex(M.height - 12) });
    M.txs.sort((a, b) => a.time - b.time);
  }
  const bal = () => { let t = 380e8, im = 0; for (const x of M.txs) { const c = Math.round(parseFloat(x.amount) * 1e8); if (x.category === "immature") im += c; else t += c + (x.fee ? Math.round(parseFloat(x.fee) * 1e8) : 0); } return { trusted: flags.has("empty") ? "0.00000000" : amt(t), immature: amt(im), locked: "0.00000000", untrusted_pending: "0.00000000" }; };
  const E = (m) => { throw new Error(m); };
  const needW = (w) => M.loaded.includes(w || M.loaded[0]) || E("Requested wallet does not exist or is not loaded");
  const rpc = {
    listwallets: () => M.loaded.slice(),
    createwallet: ([path, pass]) => { if (path.endsWith("/taken")) E("the file already exists: " + path); M.loaded = ["wallet"]; return { name: "wallet", address: ME, mnemonic: WORDS, birth_height: M.height }; },
    restorewallet: ([path, pass, words]) => { if (words !== WORDS) E("mnemonic: bad checksum"); M.loaded = ["restored"]; return { name: "restored" }; },
    loadwallet: ([path, pass]) => { if (M.loaded.length) return { name: M.loaded[0], already_loaded: true }; if (pass !== PASS) E("wrong password"); M.loaded = ["wallet"]; return { name: "wallet" }; },
    unloadwallet: () => { M.loaded = []; return {}; },
    getseed: ([w, pass]) => pass === PASS ? { mnemonic: WORDS, address: ME, network: "testnet" } : E("Error: The wallet passphrase entered was incorrect."),
    getinfo: () => M.off ? E("connection refused") : { network: "testnet", height: M.height, applied_height: M.synced ? M.height : 700, best_known: M.height, peers: 5, synced: M.synced, tip_time: now() - 40 },
    getblock: (p) => ({ lane: parseInt(p.id.slice(-6), 16) % 2 ? "G" : "C" }),
    getbalances: () => (M.fail && M.calls.filter((c) => c === "getbalances").length > 1 ? E("node unavailable") : { mine: bal() }),
    listtransactions: () => M.txs.slice(),
    getaddressesbylabel: () => ({ [ME]: { purpose: "receive" } }),
    getnewaddress: ([label]) => { const a = ME.slice(0, -6) + "new" + String(M.addrs.length).padStart(3, "0"); M.addrs.push({ address: a, label: label || "" }); return a; },
    validateaddress: ([a]) => ({ isvalid: a.length === 65 && !a.includes("zzzz"), ismine: a === ME }),
    createrawtransaction: ([ins, outs]) => { const v = Object.values(outs[0])[0]; if (typeof v !== "string" || !/^\d+\.\d{8}$/.test(v)) E("Invalid amount " + v); return "rawhex"; },
    fundrawtransaction: () => ({ hex: "funded", fee: "0.00020000", changepos: 1 }),
    walletpassphrase: ([pass, t]) => { if (pass !== PASS) E("Error: The wallet passphrase entered was incorrect."); M.unlockedUntil = Date.now() + t * 1000; return null; },
    walletlock: () => { M.unlockedUntil = 0; return null; },
    walletpassphrasechange: ([o, n]) => (o === PASS ? null : E("Error: The wallet passphrase entered was incorrect.")),
    sendtoaddress: ([to, a]) => { if (Date.now() > M.unlockedUntil) E("Error: Please enter the wallet passphrase with walletpassphrase first."); M.sent.push([to, a]); M.txs.push({ txid: hex(555000 + M.sent.length), vout: 0, category: "send", amount: "-" + a, fee: "-0.00020000", address: to, confirmations: 0, time: now() }); return hex(555000 + M.sent.length); },
    rescanblockchain: () => ({ start_height: 0, stop_height: M.height }),
    listreceivedbyaddress: () => M.addrs.map((a) => ({ address: a.address, label: a.label, imported: !!a.imported, confirmations: 3, txids: [],
      amount: amt(M.txs.filter((t) => t.address === a.address && t.category !== "send").reduce((s, t) => s + Math.round(parseFloat(t.amount) * 1e8), 0)) })),
    setlabel: ([a, l]) => { const x = M.addrs.find((y) => y.address === a) || E("Address is not in wallet"); x.label = l; return null; },
    sweepall: ([to, minconf, maxin, estimate]) => {
      const total = Math.round(parseFloat(bal().trusted) * 1e8), parts = flags.has("manycoins") ? 3 : 1, fee = 54000 * parts;
      if (estimate) return { estimate: true, amount: amt(total - fee), fee: amt(fee), transactions: flags.has("empty") ? 0 : parts, inputs: 66 };
      if (Date.now() > M.unlockedUntil) E("Error: Please enter the wallet passphrase with walletpassphrase first.");
      M.swept.push([to, amt(total - fee)]);
      return { txids: Array.from({ length: parts }, (_, i) => hex(777000 + i)), amount: amt(total - fee) };
    },
    dumpprivkey: ([a]) => (Date.now() > M.unlockedUntil ? E("Error: Please enter the wallet passphrase with walletpassphrase first.") : "tpowmsec1qqsecretkeyfor" + a.slice(-6)),
    importprivkey: ([key, label]) => { if (Date.now() > M.unlockedUntil) E("Error: Please enter the wallet passphrase with walletpassphrase first."); if (!key.startsWith("tpowmsec1")) E("Invalid private key encoding: bad prefix"); M.addrs.push({ address: ME.slice(0, -6) + "import", label, imported: true }); return null; },
  };
  window.__mock = M;
  window.__TAURI__ = {
    core: { invoke: async (cmd, a) => {
      if (cmd === "rpc_info") return { ready: true, network: "testnet" };
      if (cmd === "default_wallet_dir") return "C:/Users/test/AppData/Roaming/com.powmachina.wallet/wallets";
      if (cmd === "rpc_call") { M.calls.push(a.method); if (!rpc[a.method]) E("Method not found: " + a.method); if (a.wallet) needW(a.wallet); return rpc[a.method](a.params); }
      E("unknown command " + cmd); } },
    event: { listen: async () => () => {} },
    app: { getVersion: async () => "0.1.0" },
    dialog: { open: async () => null },
    opener: { openUrl: async (u) => { M.opened.push(u); }, revealItemInDir: async (p) => { M.opened.push(p); } },
  };
  if (!flags.has("fresh") && !flags.has("reloaded")) localStorage.setItem("powm.lastWallet", "C:/Users/test/AppData/Roaming/com.powmachina.wallet/wallets/wallet");
  if (flags.has("fresh")) localStorage.removeItem("powm.lastWallet");
  if (flags.has("light")) localStorage.setItem("powm.theme", "light"); else localStorage.removeItem("powm.theme");
})();
