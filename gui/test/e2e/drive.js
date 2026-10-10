// Drives the real page against the real node and wallet (see run.js). Served into the page; it clicks
// and types like a person and checks what the wallet itself then reports over RPC.
(async () => {
  const $ = (id) => document.getElementById(id), log = [];
  const wait = (ms) => new Promise((r) => setTimeout(r, ms));
  const until = async (fn, what, ms = 40000) => { for (let t = 0; t < ms; t += 100) { try { if (await fn()) return true; } catch { /* not yet */ } await wait(100); } log.push("TIMEOUT " + what + " — on screen: " + state()); return false; };
  const state = () => [...document.querySelectorAll(".screen:not(.hidden), .pane:not(.hidden)")].map((e) => e.id).join("/") + " | " + [...document.querySelectorAll(".msg.bad")].map((e) => e.textContent).filter(Boolean).join(" · ") + " | " + document.getElementById("boot-text").textContent;
  const ok = (cond, what) => log.push((cond ? "ok   " : "FAIL ") + what);
  const type = (id, v) => { $(id).value = v; $(id).dispatchEvent(new Event("input", { bubbles: true })); };
  const submit = (elOrId) => (typeof elOrId === "string" ? $(elOrId) : elOrId).dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  const visible = (id) => !$(id).classList.contains("hidden") && !$(id).closest(".screen").classList.contains("hidden");
  const tab = (t) => document.querySelector(`#tabs [data-tab="${t}"]`).click();
  const rpc = (method, params = [], wallet = null) => window.__TAURI__.core.invoke("rpc_call", { method, params, wallet });
  const text = (id) => $(id).textContent.replace(/\s/g, "");
  const num = (s) => s.replace(/[^\d.]/g, "");                       // "1,234.50 POWM" → "1234.50"
  const same = (shown, server) => Number(num(shown)).toFixed(8) === Number(server).toFixed(8);   // comparing what is displayed, not computing money
  const PASS = "correct horse battery";
  // mine brings the chain n blocks further. In small batches, and counted by the height rather than by
  // the replies: regtest mines a block in a second or several, and the node drops a request that runs long
  // — the blocks of a dropped request are still mined.
  const mine = async (n, address) => {
    const height = async () => Number((await window.__node("getinfo", {})).height);
    const target = (await height()) + n;
    for (let tries = 0; tries < 200; tries++) {
      const left = target - (await height());
      if (left <= 0) return;
      try { await window.__node("generatetoaddress", { nblocks: Math.min(4, left), address }); } catch { await wait(1500); }
    }
    throw new Error("the chain did not reach height " + target);
  };
  const report = () => fetch("/report", { method: "POST", body: JSON.stringify({ log }) });
  try {
    // ---- create a wallet the way a person does
    await until(() => visible("g-welcome"), "welcome screen");
    document.querySelector('[data-go="g-create"]').click();
    type("create-name", "e2e"); type("create-pass", PASS); type("create-pass2", PASS); submit("g-create");
    await until(() => visible("g-seed"), "the 24 words");
    const words = [...document.querySelectorAll("#seed-words li")].map((li) => li.textContent);
    ok(words.length === 24 && words.every((w) => /^[a-z]+$/.test(w)), "the real wallet hands out 24 words");
    $("seed-next").click(); await wait(100);
    for (const i of document.querySelectorAll("#confirm-fields input")) i.value = words[+i.dataset.i];
    submit("g-confirm");
    await until(() => visible("tabs") && /^rpowm1/.test(text("side-addr")), "the wallet opens with an address");
    const addr = text("side-addr");
    ok(/^rpowm1[023456789ac-hj-np-z]+$/.test(addr), "the address is a regtest one: " + addr);
    ok($("avail").textContent === "0.00", "a new wallet shows 0.00, not a dash: " + $("avail").textContent);
    ok(visible("home-empty"), "and says that an empty wallet is normal");

    // ---- the QR code, read back by a decoder that shares no code with the generator
    tab("receive"); await until(() => document.querySelector("#recv-qr svg path"), "QR code");
    if (window.jsQR) {
      const m = QR.matrix(addr), q = 4, sc = 4, n = (m.size + q * 2) * sc, px = new Uint8ClampedArray(n * n * 4).fill(255);
      for (let y = 0; y < m.size; y++) for (let x = 0; x < m.size; x++) if (m.dark(x, y)) for (let dy = 0; dy < sc; dy++) for (let dx = 0; dx < sc; dx++) { const i = (((y + q) * sc + dy) * n + (x + q) * sc + dx) * 4; px[i] = px[i + 1] = px[i + 2] = 0; }
      ok(window.jsQR(px, n, n)?.data === addr, "the QR code decodes to exactly the address");
      const d = document.querySelector("#recv-qr svg path").getAttribute("d"), cells = (d.match(/M/g) || []).length; let dark = 0;
      for (let y = 0; y < m.size; y++) for (let x = 0; x < m.size; x++) if (m.dark(x, y)) dark++;
      ok(cells === dark, "what is drawn is that same code, module for module");
    }

    // ---- mine to it: 104 blocks, so the first four rewards have matured and a hundred have not
    await mine(104, addr);
    tab("home");
    // Wait for the wallet to have read every block, then for the page to show the wallet's numbers — the
    // page polls, so it is a few seconds behind by design.
    const tip = Number((await window.__node("getinfo", {})).height);
    await until(async () => Number((await rpc("getwalletinfo", [], "e2e")).height) >= tip, "the wallet reads all the blocks", 90000);
    const bal = (await rpc("getbalances", [], "e2e")).mine;
    await until(() => same($("avail").textContent, bal.trusted) && visible("onway") && same($("onway-amt").textContent, bal.immature), "the balance arrives by itself", 30000);
    ok(same($("avail").textContent, bal.trusted), `Available is the wallet's own number: ${$("avail").textContent} = ${bal.trusted}`);
    ok(Number(bal.immature) > 0 && same($("onway-amt").textContent, bal.immature), `On the way is the immature balance: ${$("onway-amt").textContent}`);
    ok(/unlocks in about 2 min/.test($("onway-sub").textContent), "the next reward is one block away: " + $("onway-sub").textContent);
    tab("activity");
    await until(() => document.querySelectorAll("#act-list .row").length === 104, "the wallet catches up with all 104 blocks", 60000);
    ok(document.querySelectorAll("#act-list .row").length === 104, "all 104 rewards are listed: " + document.querySelectorAll("#act-list .row").length);
    await until(() => document.querySelector("#act-list .tag"), "lane tags from the node", 30000);
    ok(/GPU|CPU/.test(document.querySelector("#act-list .tag")?.textContent || ""), "rewards carry the lane the node reports");

    // ---- somebody to pay: a second wallet, made directly
    const dir = await window.__TAURI__.core.invoke("default_wallet_dir");
    const dest = await rpc("createwallet", [dir + "/dest", "another password"]);
    const destBal = async () => Number(await rpc("getbalance", [], "dest"));

    // ---- send 0.5: what the check screen shows is what the chain then records
    tab("send"); await wait(200);
    type("send-to", addr); ok(/your own address/.test($("m-to").textContent), "paying yourself is caught");
    type("send-to", dest.address.slice(0, -1) + (dest.address.endsWith("q") ? "p" : "q")); type("send-amt", "0.5"); submit("send-1"); await wait(1500);
    ok(visible("send-1") && /typo/.test($("m-to").textContent), "one wrong character is caught by the wallet's checksum");
    type("send-to", dest.address); submit("send-1");
    await until(() => visible("send-2"), "the check screen");
    const fee = num($("chk-fee").textContent);
    ok(Number(fee) > 0 && same($("chk-amt").textContent, "0.5"), `the real fee is shown before the password: ${fee}`);
    type("send-pass", "not the password"); submit("send-2");
    await until(() => /isn't right/.test($("m-pass").textContent), "the wrong send password is answered");
    ok(visible("send-2") && /isn't right/.test($("m-pass").textContent), "a wrong password sends nothing");
    type("send-pass", PASS); submit("send-2"); submit("send-2");
    await until(() => visible("send-3"), "sent", 30000);
    const txid = $("sent-txid").textContent;
    const tx = await rpc("gettransaction", [txid], "e2e");
    ok(Number(tx.amount) === -0.5 && same(fee, String(-Number(tx.fee))), `the wallet recorded −0.5 and the fee that was shown (${tx.fee})`);
    ok((await rpc("getwalletinfo", [], "e2e")).unlocked_until === 0, "the wallet is locked again after sending");
    await mine(1, dest.address);
    await until(async () => (await destBal()) === 0.5, "the payment arrives", 30000);
    ok((await destBal()) === 0.5, "the other wallet received exactly 0.5");
    ok((await rpc("listtransactions", ["*", 400], "e2e")).filter((t) => t.category === "send").length === 1, "two clicks on Send made one payment");

    // ---- send everything: the plan is what happens
    tab("send"); await wait(300); $("send-again")?.click(); await wait(100);
    type("send-to", dest.address); $("send-all").click(); await wait(100); submit("send-1");
    await until(() => visible("send-2"), "the check screen (everything)");
    const arrives = num($("chk-amt").textContent);
    ok(Number(arrives) > 0, "the wallet says what will arrive: " + arrives);
    type("send-pass", PASS); submit("send-2");
    await until(() => visible("send-3"), "everything sent", 30000);
    await mine(1, dest.address);
    await until(async () => (await destBal()).toFixed(8) === (0.5 + Number(arrives)).toFixed(8), "everything arrives", 30000);
    ok((await destBal()).toFixed(8) === (0.5 + Number(arrives)).toFixed(8), `the other wallet received exactly what was shown: ${await destBal()}`);

    // ---- addresses and their names
    tab("receive"); await wait(500);
    type("addr-label", "mining rig"); submit("addr-new");
    await until(() => document.querySelectorAll("#addr-list .arow").length === 2, "a second address");
    const listed = await rpc("listreceivedbyaddress", [1, true], "e2e");
    ok(listed.length === 2 && listed[1].label === "mining rig" && text("recv-addr") === listed[1].address, "the new address is named in the wallet and shown");
    ok(same(document.querySelector("#addr-list .arow .r").textContent, listed[0].amount), "the first address shows what it received: " + listed[0].amount);

    // ---- the words and a key, each behind the password
    tab("settings"); await wait(200); $("set-words").click(); await wait(100);
    const wp = $("words-panel"); wp.querySelector("input").value = PASS; submit(wp.querySelector("form"));
    await until(() => wp.querySelectorAll(".words li").length === 24, "recovery words");
    ok([...wp.querySelectorAll(".words li")].map((l) => l.textContent).join(" ") === words.join(" "), "Settings shows the same 24 words the wallet was created with");
    $("set-keys").click(); await until(() => visible("keys-panel"), "keys panel");
    ok(!document.querySelector("#words-panel li"), "opening the key panel puts the words away");
    const kf = $("keys-panel").querySelectorAll("form")[0]; kf.querySelector("input").value = PASS; submit(kf);
    await until(() => $("keys-panel").querySelector(".secret"), "exported key");
    ok(/^rpowmsec1/.test($("keys-panel").querySelector(".secret")?.textContent || ""), "the exported key is a regtest secret key");
    ok((await rpc("getwalletinfo", [], "e2e")).unlocked_until === 0, "and the wallet is locked again");

    // ---- lock, close, open again
    $("lock").click(); await wait(200);
    ok(visible("g-unlock") && !document.querySelector("#keys-panel .secret"), "Lock covers the wallet and clears the key");
    type("unlock-pass", "wrong one"); submit("g-unlock");
    await until(() => /isn't right/.test($("unlock-msg").textContent), "the wrong unlock password is answered");
    ok(visible("g-unlock"), "a wrong password does not unlock");
    type("unlock-pass", PASS); submit("g-unlock"); await until(() => visible("tabs"), "unlocked");
    tab("settings"); await wait(200); $("set-close").click(); await until(() => visible("g-welcome"), "closed");
    ok(!(await rpc("listwallets")).includes("e2e"), "Close wallet unloads it from the backend");
    document.querySelector('[data-go="g-open"]').click(); await wait(100);
    type("open-path", dir + "/e2e"); type("open-pass", "wrong one"); submit("g-open");
    await until(() => /isn't right/.test($("open-msg").textContent), "the wrong password is answered");
    ok(visible("g-open") && /isn't right/.test($("open-msg").textContent), "the file does not open with a wrong password: " + $("open-msg").textContent);
    type("open-pass", PASS); submit("g-open");
    await until(() => visible("tabs") && /^rpowm1/.test(text("side-addr")), "reopened");
    ok(text("side-addr") === addr, "the reopened wallet is the same wallet");
  } catch (e) {
    log.push("FAIL the run threw: " + (e?.stack || e));
  }
  report();
})();
