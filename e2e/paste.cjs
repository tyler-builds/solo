"use strict";
// Large pastes into the composer, in a real browser: Markdown of a few hundred kilobytes and of a
// few megabytes is pasted into Solo's composer in the web build, typed after and sent. The page must
// not stop, must answer each key in time, and must not run out of memory: a paste of 500 KB once
// ran it out of the memory a web page has, and one of 200 KB took seconds at each key.
//
// scripts/e2e.sh paste runs it. The page runs over generated data (?sample=1), served from this
// machine. Its composer is the box at the bottom of a 1280 by 800 page; the test reads where the
// keyboard's focus is to know it hit it.
const http = require("http");
const fs = require("fs");
const path = require("path");
const { chromium } = require(process.env.PW || "playwright");
const out = process.argv[2] || ".";
const web = process.env.WEB;

const fail = (msg) => { throw new Error(msg); };
const types = { ".html": "text/html", ".js": "text/javascript", ".wasm": "application/wasm", ".css": "text/css", ".svg": "image/svg+xml" };

// markdown returns about kb kilobytes of Markdown: headings, paragraphs with emphasis, code and
// links, lists, code blocks, tables and text in other scripts.
function markdown(kb) {
  const para = "Lorem ipsum dolor sit amet, **consectetur** adipiscing elit, sed do _eiusmod_ tempor " +
    "`incididunt` ut labore et [dolore](https://example.com/x) magna aliqua. ";
  const parts = [
    (i) => "## Heading " + i + "\n\n",
    () => para + para + "\n\n",
    () => "- item one\n- item **two**\n  - nested `x`\n\n",
    (i) => "```js\nconst x = " + i + ";\nconsole.log(x);\n```\n\n",
    () => "| a | b |\n|---|---|\n| 1 | 2 |\n\n",
    () => "Ünïcödé — 日本語 ✓ Ελληνικά\n\n",
  ];
  let s = "";
  for (let i = 0; s.length < kb * 1024; i++) s += parts[i % parts.length](i);
  return s;
}

function serve() {
  const server = http.createServer((req, res) => {
    const name = path.join(web, decodeURIComponent(new URL(req.url, "http://x").pathname));
    const file = name.endsWith(path.sep) ? path.join(name, "index.html") : name;
    fs.readFile(file, (err, data) => {
      if (err) { res.writeHead(404).end(); return; }
      res.writeHead(200, { "Content-Type": types[path.extname(file)] || "application/octet-stream" }).end(data);
    });
  });
  return new Promise((r) => server.listen(0, "127.0.0.1", () => r(server)));
}

// Each case pastes kb kilobytes, types keys after it, and sends. keyMs is the most a key may take,
// two frames drawn after it, in a headless browser without a GPU, which is slow.
const cases = [
  { kb: 200, keys: 20, keyMs: 1500, send: true },
  { kb: 2048, keys: 5, keyMs: 8000, send: true },
];

(async () => {
  const server = await serve();
  const url = "http://127.0.0.1:" + server.address().port + "/?sample=1";
  const browser = await chromium.launch({ args: ["--enable-gpu", "--use-angle=gl"] });
  try {
    for (const c of cases) {
      const page = await browser.newPage({ viewport: { width: 1280, height: 800 }, deviceScaleFactor: 1 });
      const errors = [];
      page.on("pageerror", (e) => errors.push(String(e)));
      page.on("console", (m) => { if (m.type() === "error" && !/GL Driver/.test(m.text())) errors.push(m.text()); });
      page.on("crash", () => errors.push("the page crashed"));
      const frames = () => page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
      const memory = () => page.evaluate(() => globalThis.soloInstance ? soloInstance.exports.memory.buffer.byteLength : 0);
      const check = (what) => {
        if (errors.length) fail(c.kb + " KB, " + what + ": " + JSON.stringify(errors).slice(0, 2000));
      };

      await page.goto(url);
      await page.waitForSelector("canvas", { timeout: 60000 });
      await page.waitForFunction(() => globalThis.soloInstance, null, { timeout: 60000 });
      await page.waitForTimeout(3000);
      for (let y = 770; y >= 730 && await page.evaluate(() => document.activeElement.tagName) !== "TEXTAREA"; y -= 10) {
        await page.mouse.click(760, y);
        await page.waitForTimeout(300);
      }
      if (await page.evaluate(() => document.activeElement.tagName) !== "TEXTAREA") fail("the composer never took the focus");

      const text = markdown(c.kb);
      const t0 = Date.now();
      await page.evaluate((t) => {
        const data = new DataTransfer();
        data.setData("text/plain", t);
        document.activeElement.dispatchEvent(new ClipboardEvent("paste", { clipboardData: data, bubbles: true, cancelable: true }));
      }, text);
      await frames();
      const pasted = Date.now() - t0;
      check("pasting");
      await page.screenshot({ path: path.join(out, "paste-" + c.kb + "kb.png") });

      const t1 = Date.now();
      for (let i = 0; i < c.keys; i++) {
        await page.keyboard.type("k");
        await frames();
      }
      const perKey = Math.round((Date.now() - t1) / c.keys);
      check("typing");
      const mb = Math.round(await memory() / 1048576);
      console.log(`${c.kb} KB: pasted in ${pasted} ms, ${perKey} ms a key, ${mb} MB of memory`);
      if (perKey > c.keyMs) fail(`${c.kb} KB: a key took ${perKey} ms, more than ${c.keyMs}`);

      if (c.send) {
        await page.keyboard.press("Enter");
        await frames();
        await page.waitForTimeout(2000);
        await frames();
        check("sending");
        await page.screenshot({ path: path.join(out, "sent-" + c.kb + "kb.png") });
        // The composer is empty again: the page sent the message and still answers.
        const left = await page.evaluate(() => document.activeElement.value || "");
        if (left.length > 0) fail(`${c.kb} KB: the composer still holds ${left.length} characters after sending`);
      }
      const stopped = await page.evaluate(() => document.body.innerText.includes("Solo stopped"));
      if (stopped) fail(c.kb + " KB: the page says Solo stopped");
      await page.close();
    }
    console.log("ok");
  } finally {
    await browser.close();
    server.close();
  }
})().catch((e) => { console.error(String(e && e.message || e)); process.exit(1); });
