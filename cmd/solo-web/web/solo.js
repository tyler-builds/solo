// solo.js is Solo's host script: what the app needs of the browser beyond jamb's own (jamb.js). The
// app (internal/webhost) gives it two buffers of its own memory. A message that arrives on the
// WebSocket is copied into the first and the app is told its length; a message the app sends is read
// from the second. Only numbers cross for each message, so a stream of them makes the app's collector
// no work. Everything here that can throw is caught: an exception would end the app.
//
// boot.js gives it the app's instance before the app runs: soloHost.attach(instance).
(() => {
  "use strict";
  let app = null; // the app's exports, when it has the functions this script calls (a TinyGo build)
  let ws = null;
  let inAddr = 0, outAddr = 0, size = 0;
  let outgoing = []; // the pieces so far of the message the app is sending

  // view returns the app's buffer at addr. The memory's own buffer is another object after the memory
  // grew, so the view is made for each use.
  const view = (addr) => new Uint8Array(app.memory.buffer, addr, size);
  const run = () => { if (app) app.go_scheduler(); };
  const state = (code) => {
    if (app) { app.solo_ws_state(code); run(); } else if (globalThis.soloGoState) { globalThis.soloGoState(code); }
  };
  const message = (e) => {
    if (typeof e.data === "string") return; // the relay sends a device no text
    const data = new Uint8Array(e.data);
    let off = 0;
    do {
      const n = Math.min(size, data.length - off);
      const piece = data.subarray(off, off + n);
      off += n;
      const final = off >= data.length ? 1 : 0;
      if (app) { view(inAddr).set(piece); app.solo_ws_data(n, final); } else { globalThis.soloGoData(piece, final); }
    } while (off < data.length);
    run(); // the app takes the message before this callback ends
  };
  // The page was shown again, or the network came back: a connection that is waiting is tried now.
  const resumed = () => {
    if (app) { app.solo_page_event(); run(); } else if (globalThis.soloGoPage) { globalThis.soloGoPage(); }
  };
  document.addEventListener("visibilitychange", () => { if (document.visibilityState === "visible") resumed(); });
  addEventListener("online", resumed);
  addEventListener("pageshow", (e) => { if (e.persisted) resumed(); });

  globalThis.soloHost = {
    attach(instance) {
      const e = instance.exports;
      app = e.solo_ws_data && e.solo_ws_state && e.solo_page_event && e.solo_probe_result && e.go_scheduler && e.memory ? e : null;
    },
    // The app calls the rest.
    buffers(inAddress, outAddress, bufferSize) { inAddr = inAddress; outAddr = outAddress; size = bufferSize; },
    open(url, protocol, token) {
      outgoing = [];
      const old = ws;
      ws = null;
      try { if (old) old.close(); } catch {}
      let mine;
      try {
        mine = new WebSocket(url, [protocol, token]);
      } catch {
        setTimeout(() => state(1006), 0);
        return;
      }
      ws = mine;
      mine.binaryType = "arraybuffer";
      mine.onopen = () => { if (mine === ws) state(0); };
      mine.onclose = (e) => { if (mine === ws) { ws = null; state(e.code || 1006); } };
      mine.onerror = () => {};
      mine.onmessage = (e) => { if (mine === ws) message(e); };
    },
    send(n, last) {
      let piece;
      if (app) { piece = view(outAddr).slice(0, n); } else { piece = new Uint8Array(n); globalThis.soloGoOut(piece, n); }
      outgoing.push(piece);
      if (!last) return;
      let whole = outgoing[0];
      if (outgoing.length > 1) {
        whole = new Uint8Array(outgoing.reduce((a, p) => a + p.length, 0));
        let off = 0;
        for (const p of outgoing) { whole.set(p, off); off += p.length; }
      }
      outgoing = [];
      // send throws while the socket is connecting.
      try { if (ws && ws.readyState === WebSocket.OPEN) ws.send(whole); } catch {}
    },
    close() {
      const old = ws;
      ws = null;
      try { if (old) old.close(); } catch {}
    },
    // probe asks the relay whether it is there, for when a connection failed and the browser won't
    // say why.
    probe() {
      const done = (ok) => {
        if (app) { app.solo_probe_result(ok ? 1 : 0); run(); } else if (globalThis.soloGoProbe) { globalThis.soloGoProbe(ok); }
      };
      fetch("/v1/info", { cache: "no-store" }).then((r) => done(r.ok), () => done(false));
    },
    // random fills the app's outgoing buffer with n random bytes, and says whether it could.
    random(n) {
      try {
        const bytes = crypto.getRandomValues(new Uint8Array(n));
        if (app) { view(outAddr).set(bytes); } else { globalThis.soloGoIn(bytes); }
        return true;
      } catch {
        return false;
      }
    },
    load(name) {
      try { return localStorage.getItem(name) || ""; } catch { return ""; }
    },
    store(name, value) {
      try {
        if (value === "") localStorage.removeItem(name); else localStorage.setItem(name, value);
        if (navigator.storage && navigator.storage.persist) navigator.storage.persist().catch(() => {});
        return true;
      } catch {
        return false;
      }
    },
    // takeFragment returns what followed "#" in the address and removes it, so a pairing code is in
    // neither the address bar nor the history.
    takeFragment() {
      try {
        const frag = location.hash.replace(/^#/, "");
        if (frag) history.replaceState(null, "", location.pathname + location.search);
        return frag;
      } catch {
        return "";
      }
    },
    // What the app shows: its screen, the pairing digits while they are on it, and the state of its
    // connection. The app is one canvas, so this is how a test can tell.
    shown: { screen: "", digits: "", link: "" },
    show(screen, digits, link) { this.shown = { screen, digits, link }; },
    touch() {
      try { return matchMedia("(pointer: coarse)").matches; } catch { return false; }
    },
    deviceName() {
      try {
        const ua = navigator.userAgent || "";
        const touch = navigator.maxTouchPoints > 1;
        let kind = "Computer";
        if (/iPhone/.test(ua)) kind = "iPhone";
        else if (/iPad/.test(ua) || (/Macintosh/.test(ua) && touch)) kind = "iPad";
        else if (/Android/.test(ua)) kind = /Mobile/.test(ua) ? "Android phone" : "Android tablet";
        else if (/CrOS/.test(ua)) kind = "Chromebook";
        else if (/Macintosh|Mac OS X/.test(ua)) kind = "Mac";
        else if (/Windows/.test(ua)) kind = "Windows PC";
        else if (/Linux/.test(ua)) kind = "Linux computer";
        let browser = "";
        if (/Edg\//.test(ua)) browser = "Edge";
        else if (/Firefox\//.test(ua)) browser = "Firefox";
        else if (/Chrome\//.test(ua)) browser = "Chrome";
        else if (/Safari\//.test(ua)) browser = "Safari";
        return browser ? kind + " (" + browser + ")" : kind;
      } catch {
        return "";
      }
    },
  };
})();
