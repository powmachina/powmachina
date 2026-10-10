// Drives the page like a person would and writes down what it saw. Not shipped.
(async () => {
  const flags = new Set(location.hash.slice(1).split(",").filter(Boolean));
  const $ = (id) => document.getElementById(id), M = window.__mock, log = [];
  const wait = (ms) => new Promise((r) => setTimeout(r, ms));
  const until = async (fn, what) => { for (let i = 0; i < 100; i++) { if (fn()) return; await wait(30); } log.push("TIMEOUT " + what); };
  const ok = (cond, what) => log.push((cond ? "ok   " : "FAIL ") + what);
  const type = (id, v) => { $(id).value = v; $(id).dispatchEvent(new Event("input", { bubbles: true })); };
  const submit = (id) => $(id).dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  const visible = (id) => !$(id).classList.contains("hidden") && !$(id).closest(".screen").classList.contains("hidden");
  const unlock = async () => { await until(() => visible("g-unlock"), "unlock screen"); type("unlock-pass", "hunter22"); submit("g-unlock"); await until(() => visible("tabs"), "main"); await wait(300); };
  const tab = (t) => document.querySelector(`#tabs [data-tab="${t}"]`).click();
  const fmtOf = (text) => { const [w, f = ""] = text.split("."); return w.replace(/\B(?=(\d{3})+(?!\d))/g, ",") + "." + (f.replace(/0+$/, "").padEnd(2, "0")); };
  const done = () => { const pre = document.createElement("pre"); pre.id = "test-log"; pre.textContent = log.join("\n"); pre.style.display = "none"; document.body.append(pre); document.title = log.some((l) => !l.startsWith("ok")) ? "TESTS FAILED" : "TESTS OK " + log.length; };

  if (flags.has("t-all")) {
    // --- unlock: a wrong password stays out, the right one gets in
    await until(() => visible("g-unlock"), "unlock screen");
    type("unlock-pass", "nope"); submit("g-unlock"); await wait(150);
    ok(visible("g-unlock") && /isn't right/.test($("unlock-msg").textContent), "wrong password is refused with a sentence");
    ok($("unlock-pass").value === "", "the password field is emptied after use");
    await unlock();
    ok(visible("tab-home"), "right password opens the wallet");
    // --- numbers
    await until(() => $("avail").textContent !== "—", "balance");
    ok(/^\d{1,3}(,\d{3})*\.\d{2,8}$/.test($("avail").textContent), "available is a formatted number: " + $("avail").textContent);
    ok(visible("onway") && /unlocks in about/.test($("onway-sub").textContent), "on the way says when: " + $("onway-sub").textContent);
    await until(() => document.querySelector("#latest .tag"), "lane tags");
    ok(!!document.querySelector("#latest .tag"), "mining rewards carry a GPU/CPU tag");
    ok(visible("home-mining") && !!document.querySelector("#stream svg polygon"), "the mining stream is drawn");
    // --- nothing from outside is parsed as HTML
    tab("activity"); await wait(100);
    ok(!document.querySelector("#act-list img") && /<img src=x/.test($("act-list").textContent), "a label with HTML in it is shown as text");
    const rows = document.querySelectorAll("#act-list .row").length;
    ok(rows === 68, "all 68 payments are listed: " + rows);
    document.querySelector('#flt [data-f="sent"]').click(); await wait(50);
    ok(document.querySelectorAll("#act-list .row").length === 1, "the Sent filter leaves one row");
    document.querySelector("#act-list .row").click(); await wait(50);
    ok(/cannot be reversed/.test(document.querySelector("#act-list .det").textContent), "a row opens into an explanation");
    // --- sending: every mistake gets its own sentence and the button stays off
    tab("send"); await wait(100);
    const hint = () => $("m-to").textContent, off = () => $("send-check").disabled;
    ok(off(), "the button is off while the form is empty");
    type("send-to", "powm1qqqqqqqq"); ok(/mainnet address/.test(hint()) && off(), "mainnet address: " + hint());
    type("send-to", "bc1qxyz"); ok(/starts with tpowm1/.test(hint()) && off(), "foreign address");
    type("send-to", "tpowm1qvlcvh7ekwxyyaxyffgrm0zuvn4cf5xeekzfvergtl6kuevw74r4s9qxtff"); ok(/your own address/.test(hint()) && off(), "own address");
    type("send-to", "tpowm1q9x4f8t2mzk7ha2c0m3d9kq5wj8r6n4ptl0vyx3ceu7hgs2dac"); ok(/too short/.test(hint()) && off(), "cut-off address: " + hint());
    type("send-to", "tpowm1q9x4f8t2mzk7ha2c0m3d9kq5wj8r6n4ptl0vyx3ceu7hgs2dacfk4e22acb"); ok(/can't contain/.test(hint()) && off(), "bad character");
    const GOOD = "tpowm1q9x4f8t2mzk7ha2c0m3d9kq5wj8r6n4ptl0vyx3ceu7hgs2dacfk4e22acd";
    type("send-to", GOOD); ok(/Looks right/.test(hint()) && off(), "good address, still off without an amount");
    for (const bad of ["abc", "1e5", "0", "-3", "1.123456789", "99999999"]) { type("send-amt", bad); ok(off() && $("m-amt").classList.contains("bad"), "amount refused: " + bad); }
    type("send-amt", "25,5"); ok(!off(), "25,5 is accepted (comma as decimal)");
    // --- a typo the wallet catches on the next step
    type("send-to", GOOD.slice(0, 20) + "zzzz" + GOOD.slice(24)); submit("send-1"); await wait(150);
    ok(visible("send-1") && /typo/.test(hint()), "the wallet's own check stops a bad checksum");
    type("send-to", GOOD); submit("send-1"); await until(() => visible("send-2"), "step 2");
    ok($("chk-amt").textContent === "25.50 POWM" && $("chk-fee").textContent === "0.0002 POWM" && $("chk-total").textContent === "25.5002 POWM", "the check screen adds up: " + $("chk-total").textContent);
    ok($("chk-to").querySelectorAll("b").length === 2 && $("chk-to").textContent.replace(/\s/g, "") === GOOD, "the address is shown whole, start and end marked");
    ok(M.sent.length === 0, "nothing was sent by checking");
    // --- the password
    submit("send-2"); await wait(100); ok(/Type your wallet password/.test($("m-pass").textContent) && M.sent.length === 0, "no password, nothing sent");
    type("send-pass", "wrong"); submit("send-2"); await wait(150);
    ok(/Nothing was sent/.test($("m-pass").textContent) && M.sent.length === 0 && visible("send-2"), "wrong password, nothing sent");
    type("send-pass", "hunter22"); submit("send-2"); submit("send-2"); submit("send-2");
    await until(() => visible("send-3"), "step 3");
    ok(M.sent.length === 1, "three quick clicks send exactly once: " + M.sent.length);
    ok(M.sent[0][0] === GOOD && M.sent[0][1] === "25.50000000", "what was sent is what was shown: " + M.sent[0][1]);
    ok(M.unlockedUntil === 0, "the wallet is locked again after sending");
    ok($("send-pass").value === "", "the password is gone from the page");
    // --- the explorer link carries the transaction and nothing else
    tab("activity"); await wait(100); document.querySelector('#flt [data-f="sent"]').click(); await wait(50);
    document.querySelector("#act-list .row").click(); await wait(50);
    const openBtn = [...document.querySelectorAll("#act-list .det button")].find((b) => /explorer/.test(b.textContent));
    ok(!!openBtn, "a confirmed payment offers the block explorer");
    if (openBtn) { openBtn.click(); await wait(50); ok(/^https:\/\/powmachina\.com\/explorer\/tx\/[0-9a-f]{64}$/.test(M.opened.at(-1) || ""), "it opens the explorer at that transaction: " + M.opened.at(-1)); }
    // --- send everything: the wallet says what that is before the password
    tab("send"); await wait(100);
    type("send-to", GOOD); $("send-all").click(); await wait(30);
    ok($("send-amt").disabled && !off(), "Send everything needs no amount typed");
    submit("send-1"); await until(() => visible("send-2"), "step 2 (everything)");
    ok($("chk-amt-k").textContent === "Arrives" && /POWM$/.test($("chk-amt").textContent), "the check screen says what arrives: " + $("chk-amt").textContent);
    ok(M.swept.length === 0, "the estimate moved nothing");
    type("send-pass", "wrong"); submit("send-2"); await wait(150);
    ok(M.swept.length === 0 && /Nothing was sent/.test($("m-pass").textContent), "wrong password, nothing swept");
    type("send-pass", "hunter22"); submit("send-2"); submit("send-2"); await until(() => visible("send-3"), "step 3 (everything)");
    ok(M.swept.length === 1 && M.swept[0][0] === GOOD, "everything is swept exactly once, to the address shown");
    ok($("chk-amt").textContent === fmtOf(M.swept[0][1]) + " POWM", "and the amount is the one that was shown");
    ok(M.unlockedUntil === 0, "the wallet is locked again after the sweep");
    // --- receiving: a QR code, the addresses, their names
    tab("receive"); await until(() => document.querySelector("#recv-qr svg path"), "qr");
    ok(!!document.querySelector("#recv-qr svg path"), "the address has a QR code");
    ok($("recv-addr").textContent.replace(/\s/g, "") === M.addrs[0].address, "the address is shown whole");
    await until(() => document.querySelectorAll("#addr-list .arow").length === 1, "address list");
    type("addr-label", "mining rig"); submit("addr-new"); await until(() => document.querySelectorAll("#addr-list .arow").length === 2, "new address");
    ok(/mining rig/.test($("recv-title").textContent) && $("recv-addr").textContent.replace(/\s/g, "") === M.addrs[1].address, "a new address is named and shown");
    const firstRow = document.querySelector("#addr-list .arow");
    [...firstRow.querySelectorAll("button")].find((b) => /Name it/.test(b.textContent)).click(); await wait(30);
    const nameIn = document.querySelector("#addr-list input"); nameIn.value = "<b>savings</b>"; nameIn.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    await until(() => M.addrs[0].label === "<b>savings</b>", "label saved");
    await wait(100);
    ok(!document.querySelector("#addr-list b") && /<b>savings<\/b>/.test($("addr-list").textContent), "a name with HTML in it is saved and shown as text");
    [...document.querySelector("#addr-list .arow").querySelectorAll("button")].find((b) => b.textContent === "Show").click(); await wait(50);
    ok($("side-addr").textContent.replace(/\s/g, "") === M.addrs[0].address, "Show makes that address the one on the left");
    // --- a single key stays behind the password too
    tab("settings"); await wait(100); $("set-keys").click(); await until(() => visible("keys-panel"), "keys panel");
    const kp = $("keys-panel"), forms = kp.querySelectorAll("form"), sub = (f) => f.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    forms[0].querySelector("input").value = "nope"; sub(forms[0]); await wait(150);
    ok(!kp.querySelector(".secret") && /isn't right/.test(kp.textContent), "wrong password exports no key");
    forms[0].querySelector("input").value = "hunter22"; sub(forms[0]); await wait(150);
    ok(/^tpowmsec1/.test(kp.querySelector(".secret")?.textContent || ""), "right password shows the key");
    ok(M.unlockedUntil === 0, "the wallet is locked again after the export");
    const [ik, , ip] = forms[1].querySelectorAll("input");
    ik.value = "garbage"; ip.value = "hunter22"; sub(forms[1]); await wait(150);
    ok(/Invalid private key/.test(forms[1].textContent) && M.addrs.length === 2, "a bad key is refused with the reason");
    ik.value = "tpowmsec1qqgood"; ip.value = "hunter22"; sub(forms[1]); await wait(200);
    ok(M.addrs.length === 3 && /Imported/.test(forms[1].textContent), "a good key is imported");
    tab("home"); await wait(50);
    ok(!document.querySelector("#keys-panel .secret"), "leaving Settings removes the key from the page");
    // --- recovery words stay behind the password and leave when you do
    tab("settings"); await wait(100); $("set-words").click(); await wait(50);
    const wp = $("words-panel");
    ok(!wp.querySelector(".words"), "words are not shown before the password");
    wp.querySelector("input").value = "bad"; wp.querySelector("form").dispatchEvent(new Event("submit", { bubbles: true, cancelable: true })); await wait(100);
    ok(!wp.querySelector(".words") && /isn't right/.test(wp.textContent), "wrong password shows no words");
    wp.querySelector("input").value = "hunter22"; wp.querySelector("form").dispatchEvent(new Event("submit", { bubbles: true, cancelable: true })); await wait(100);
    ok(wp.querySelectorAll(".words li").length === 24, "right password shows 24 words");
    tab("home"); await wait(50);
    ok(!document.querySelector("#words-panel li"), "leaving Settings removes the words from the page");
    // --- lock
    $("lock").click(); await wait(100);
    ok(visible("g-unlock") && !visible("tabs"), "Lock covers the wallet");
    type("unlock-pass", "nope"); submit("g-unlock"); await wait(150);
    ok(visible("g-unlock"), "a locked wallet does not open with a wrong password");
    await unlock(); ok(visible("tabs"), "and opens with the right one");
    done();
  } else if (flags.has("t-outage")) {
    // The old bug: when the wallet stopped answering, History said "Nothing yet".
    await unlock();
    await until(() => $("avail").textContent !== "—", "balance");
    const before = $("avail").textContent;
    M.fail = true; M.off = true; await wait(5600);
    tab("activity"); await wait(100);
    ok($("avail").textContent === before, "the last known balance stays on screen");
    ok(/no connection/.test($("asof").textContent), "and it is marked as old: " + $("asof").textContent);
    ok(document.querySelectorAll("#act-list .row").length === 68, "the payments stay listed during an outage");
    ok(/can't reach the network/.test($("banner").textContent), "a banner says what is wrong");
    tab("send"); await wait(100);
    ok(visible("send-paused") && !visible("send-flow"), "sending is paused, with a reason");
    // the network comes back and the wallet still fails: that is a different sentence, with the reason in it
    M.off = false; tab("home"); await wait(5600);
    ok(/stopped updating/.test($("banner").textContent) && /node unavailable/.test($("banner").textContent), "a wallet that fails with the network up says so, and why");
    ok($("status").textContent === "Not updating" && /not updating/.test($("asof").textContent) && $("avail").textContent === before, "the numbers stay and are marked as old: " + $("status").textContent);
    // and once it answers, Try again is enough — no restart
    M.fail = false; [...document.querySelectorAll("#banner button")].find((b) => b.textContent === "Try again").click();
    await until(() => $("status").textContent === "Up to date", "recovery");
    ok($("banner").textContent === "" && $("asof").textContent === "", "it recovers on its own once the wallet answers");
    done();
  } else if (flags.has("t-create")) {
    await until(() => visible("g-welcome"), "welcome");
    document.querySelector('[data-go="g-create"]').click();
    type("create-pass", "short"); type("create-pass2", "short"); submit("g-create"); await wait(100);
    ok(/at least 8/.test($("create-msg").textContent), "a short password is refused");
    type("create-pass", "longenough"); type("create-pass2", "different1"); submit("g-create"); await wait(100);
    ok(/not the same/.test($("create-msg").textContent), "mismatched passwords are refused");
    type("create-pass2", "longenough"); submit("g-create"); await until(() => visible("g-seed"), "seed");
    ok(document.querySelectorAll("#seed-words li").length === 24, "24 words are shown");
    $("seed-next").click(); await wait(50);
    ok(!document.querySelector("#seed-words li"), "the words leave the page for the check");
    const inputs = [...document.querySelectorAll("#confirm-fields input")];
    ok(inputs.length === 3, "three words are asked for");
    inputs.forEach((i) => (i.value = "wrong")); submit("g-confirm"); await wait(50);
    ok(visible("g-confirm") && /does not match/.test($("confirm-msg").textContent), "wrong words do not pass");
    const W = "cabin velvet orbit ladder museum quiet harbor signal copper lantern fabric meadow rocket pencil window forest anchor marble silver tunnel garden bridge planet candle".split(" ");
    inputs.forEach((i) => (i.value = " " + W[+i.dataset.i].toUpperCase() + " ")); submit("g-confirm"); await until(() => visible("tabs"), "main");
    ok(visible("tab-home"), "the right words open the wallet");
    done();
  } else {
    // screenshots
    if (flags.has("fresh")) { if (flags.has("seed")) { await until(() => visible("g-welcome"), "w"); document.querySelector('[data-go="g-create"]').click(); type("create-pass", "longenough"); type("create-pass2", "longenough"); submit("g-create"); } return; }
    if (flags.has("locked")) return;
    await unlock();
    for (const t of ["activity", "send", "receive", "settings"]) if (flags.has(t)) tab(t);
    if (flags.has("keys")) { tab("settings"); $("set-keys").click(); }
    if (flags.has("all")) { tab("send"); type("send-to", "tpowm1q9x4f8t2mzk7ha2c0m3d9kq5wj8r6n4ptl0vyx3ceu7hgs2dacfk4e22acd"); $("send-all").click(); submit("send-1"); }
    if (flags.has("send2")) { tab("send"); type("send-to", "tpowm1q9x4f8t2mzk7ha2c0m3d9kq5wj8r6n4ptl0vyx3ceu7hgs2dacfk4e22acd"); type("send-amt", "25"); submit("send-1"); }
    if (flags.has("off")) { M.off = true; }
  }
})();
