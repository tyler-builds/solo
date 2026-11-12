// boot.js loads app.wasm, Solo's web build, and runs it. It is a file of its own, not a script in the
// page, so the page's content security policy can forbid inline script.
//
// When the app stops because of an error, the page says so, with the error and the top of its stack,
// which a phone has no other way to show. A TinyGo build leaves the names of its functions out of
// app.wasm; app.names lists them, and is fetched only then.
(() => {
  "use strict";
  Error.stackTraceLimit = 100000;
  const status = document.getElementById("status");
  const named = async (stack) => {
    if (!/wasm-function\[\d+\]/.test(stack)) return stack;
    try {
      const res = await fetch("app.names");
      if (!res.ok) return stack;
      const names = new Map();
      for (const line of (await res.text()).split("\n")) {
        const i = line.indexOf(":");
        if (i > 0) names.set(line.slice(0, i), line.slice(i + 1));
      }
      return stack.replace(/wasm-function\[(\d+)\]/g, (frame, n) => names.get(n) || frame);
    } catch {
      return stack;
    }
  };
  let stopped = false;
  const stop = (err) => {
    if (stopped) return;
    stopped = true;
    console.error(err);
    for (const c of document.querySelectorAll("canvas")) c.remove();
    status.textContent = "Solo stopped because of an error. Reload the page to start it again.";
    const detail = document.createElement("pre");
    const show = (stack) => {
      const frames = stack.split("\n");
      detail.textContent = String(err) + "\n" + frames.slice(0, 6).join("\n");
    };
    const stack = String((err && err.stack) || "");
    show(stack);
    status.appendChild(detail);
    document.body.appendChild(status);
    named(stack).then((withNames) => {
      if (withNames === stack) return;
      show(withNames);
      console.error(withNames);
    });
  };
  addEventListener("error", (e) => {
    // An error with no file of this origin is a browser's or an extension's own script.
    if (!e.error && !(e.filename || "").startsWith(location.origin)) return;
    stop(e.error || e.message);
  });
  addEventListener("unhandledrejection", (e) => stop(e.reason));
  (async () => {
    const go = new Go();
    let result;
    try {
      result = await WebAssembly.instantiateStreaming(fetch("app.wasm"), go.importObject);
    } catch {
      const bytes = await (await fetch("app.wasm")).arrayBuffer();
      result = await WebAssembly.instantiate(bytes, go.importObject);
    }
    status.remove();
    globalThis.soloInstance = result.instance; // for measuring
    jambHost.attach(result.instance);
    await go.run(result.instance);
  })().catch(stop);
})();
