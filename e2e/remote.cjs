"use strict";
// End to end, in a real browser: a phone opens a pairing link served by the local relay, pairs with a
// demo machine, uses it, sends it a file of several megabytes, loses and regains it, comes back after a reload, and is removed; then a second
// browser pairs from the bare address, with no code.
//
// scripts/e2e.sh runs it. The page is one canvas, so the test reads what is on screen from
// soloHost.shown and taps by position: the positions are a 390 by 844 phone's, at the app's touch size.
const { spawn } = require("child_process");
const crypto = require("crypto");
const fs = require("fs");
const path = require("path");
const { chromium } = require(process.env.PW || "playwright");
const out = process.argv[2] || ".";
const port = process.env.PORT || "8791";

const fail = (msg) => { throw new Error(msg); };

(async () => {
  const relay = spawn(process.env.RELAY, ["-demo", "-stream", "-addr", "127.0.0.1:" + port, "-web", process.env.WEB],
    { stdio: ["pipe", "pipe", "inherit"] });
  let buf = "";
  const lines = [];
  let waiters = [];
  relay.stdout.on("data", (d) => {
    buf += d;
    let i;
    while ((i = buf.indexOf("\n")) >= 0) {
      lines.push(buf.slice(0, i));
      buf = buf.slice(i + 1);
      for (const w of waiters.splice(0)) w();
    }
  });
  let seen = 0;
  const line = async (re) => {
    const deadline = Date.now() + 20000;
    for (;;) {
      for (; seen < lines.length; seen++) if (re.test(lines[seen])) return lines[seen++];
      if (Date.now() > deadline) fail("the relay never printed " + re);
      await new Promise((r) => { waiters.push(r); setTimeout(r, 500); });
    }
  };
  const say = (s) => relay.stdin.write(s + "\n");
  let browser;
  try {
    const link = (await line(/^PAIR /)).slice(5);
    browser = await chromium.launch({ args: ["--enable-gpu", "--use-angle=gl"] });
    const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, hasTouch: true, isMobile: true });
    const page = await ctx.newPage();
    const errors = [];
    page.on("pageerror", (e) => errors.push(String(e)));
    page.on("console", (m) => { if (m.type() === "error" && !/GL Driver/.test(m.text())) errors.push(m.text()); });
    const shot = (n) => page.screenshot({ path: path.join(out, n + ".png") });
    const tap = async (x, y) => { await page.touchscreen.tap(x, y); await page.waitForTimeout(500); };
    const shown = (want, what) => page.waitForFunction(
      (w) => { const s = globalThis.soloHost && soloHost.shown; return s && Object.keys(w).every((k) => s[k] === w[k]); },
      want, { timeout: 30000 }).catch(() => fail("never showed " + what + ": " + JSON.stringify(want)));

    const t0 = Date.now();
    await page.goto(link);
    await shown({ screen: "confirm" }, "the question");
    console.log("load to first screen:", Date.now() - t0, "ms");
    if (page.url().includes("#")) fail("the pairing code stayed in the address: " + page.url());
    await shot("1-confirm");
    // Pair. The button sits under text whose height depends on the fonts, so it is looked for down
    // the column it is in; nothing else there answers a tap.
    for (let y = 560; y <= 660; y += 10) {
      await tap(61, y);
      if (await page.evaluate(() => soloHost.shown.screen !== "confirm")) break;
    }
    await page.waitForFunction(() => soloHost.shown.digits.length === 6, null, { timeout: 30000 })
      .catch(() => fail("the digits never showed"));
    const digits = await page.evaluate(() => soloHost.shown.digits);
    await shot("2-digits");
    say(digits);
    await line(/^PAIRED /);
    await shown({ screen: "main", link: "ready" }, "the machine's UI");
    await page.waitForTimeout(1500);
    await shot("3-main");

    // Send a message to Solo.
    await tap(150, 804);
    await page.keyboard.type("hello from the phone");
    await page.waitForTimeout(300);
    await shot("4-typed");
    await tap(342, 804); // send
    await page.waitForTimeout(1200);
    await shot("5-sent");

    // A file picked from the phone's files crosses to the machine in parts and arrives whole; the
    // message that names it goes once it has. The paperclip is left of Send.
    const file = crypto.randomBytes(6 * 1048576 + 12345);
    let chooser = null;
    for (const x of [305, 300, 310, 296]) {
      const waiting = page.waitForEvent("filechooser", { timeout: 3000 }).catch(() => null);
      await page.touchscreen.tap(x, 804);
      if ((chooser = await waiting)) break;
    }
    if (!chooser) fail("the paperclip never opened the file picker");
    const t2 = Date.now();
    await chooser.setFiles({ name: "field-log.bin", mimeType: "application/octet-stream", buffer: file });
    await page.waitForTimeout(1500);
    await shot("5b-uploading");
    const stored = (await line(/^UPLOADED /)).slice("UPLOADED ".length);
    console.log("6 MB upload:", Date.now() - t2, "ms");
    if (!fs.readFileSync(stored).equals(file)) fail("the file arrived changed: " + stored);
    if (path.basename(stored).slice(37) !== "field-log.bin") fail("the file was stored as " + stored);
    await page.waitForTimeout(800);
    await shot("5c-attached");
    await tap(150, 804);
    await page.keyboard.type("the field log");
    await tap(342, 804); // send
    await page.waitForTimeout(1500);
    await shot("5d-sent-file");

    // The drawer, and a thread.
    await tap(24, 29);
    await shot("6-drawer");
    await tap(150, 440);
    await page.waitForTimeout(1500);
    await shot("7-thread");

    // The machine goes away and comes back.
    say("off");
    await shown({ link: "offline" }, "the machine being offline");
    await shot("8-offline");
    say("on");
    await shown({ link: "ready" }, "the machine being back");

    // The page is opened again later: no pairing, straight to the machine.
    const t1 = Date.now();
    await page.goto("http://127.0.0.1:" + port + "/");
    await shown({ screen: "main", link: "ready" }, "the machine after a reload");
    console.log("reload to the machine's UI:", Date.now() - t1, "ms");
    await page.waitForTimeout(1000);
    await shot("9-reloaded");

    // Removed at the machine.
    say("revoke");
    await shown({ link: "ended" }, "being removed");
    await page.waitForTimeout(500);
    await shot("10-removed");

    // Another computer's browser, opened at the bare address: no code to scan, no link to paste. It
    // pairs from the page, with digits typed at the machine.
    say("open");
    await line(/^OPEN /);
    const other = await (await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, hasTouch: true, isMobile: true })).newPage();
    other.on("pageerror", (e) => errors.push(String(e)));
    const otherShows = (want, what) => other.waitForFunction(
      (w) => { const s = globalThis.soloHost && soloHost.shown; return s && Object.keys(w).every((k) => s[k] === w[k]); },
      want, { timeout: 30000 }).catch(() => fail("the second browser never showed " + what));
    // press taps down the column a screen's button is in until the screen changes.
    const press = async (from) => {
      for (let y = 520; y <= 760; y += 10) {
        await other.touchscreen.tap(80, y);
        await other.waitForTimeout(400);
        if (await other.evaluate((f) => soloHost.shown.screen !== f, from)) return;
      }
      fail("no button answered on the " + from + " screen");
    };
    await other.goto("http://127.0.0.1:" + port + "/");
    await otherShows({ screen: "welcome" }, "the welcome");
    await other.screenshot({ path: path.join(out, "11-bare-address.png") });
    await press("welcome"); // Pair this device
    await otherShows({ screen: "confirm" }, "the question");
    await press("confirm"); // Pair
    await other.waitForFunction(() => soloHost.shown.digits.length === 6, null, { timeout: 30000 })
      .catch(() => fail("the second browser never showed digits"));
    await other.screenshot({ path: path.join(out, "12-bare-digits.png") });
    say(await other.evaluate(() => soloHost.shown.digits));
    await line(/^PAIRED /);
    // With no code, the page has only the answerer's word that it was paired: it asks the person
    // whether the machine says so too.
    await otherShows({ screen: "confirm-paired" }, "the question whether the machine says paired");
    await other.screenshot({ path: path.join(out, "12b-bare-confirm.png") });
    await press("confirm-paired"); // The machine says paired
    await otherShows({ screen: "main", link: "ready" }, "the machine's UI");
    await other.waitForTimeout(1000);
    await other.screenshot({ path: path.join(out, "13-bare-paired.png") });

    const heap = await page.evaluate(() => globalThis.soloInstance ? soloInstance.exports.memory.buffer.byteLength : 0);
    console.log("wasm memory:", Math.round(heap / 1048576), "MB");
    if (errors.length) fail("page errors: " + JSON.stringify(errors).slice(0, 2000));
    console.log("ok");
  } finally {
    if (browser) await browser.close();
    relay.kill();
  }
})().catch((e) => { console.error(String(e && e.message || e)); process.exit(1); });
