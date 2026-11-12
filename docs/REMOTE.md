# Remote access

Solo in a browser, driving a machine that runs Solo, through a relay in your own Cloudflare account. This
file records what was measured before the design was built on. The design itself will be written here as it
lands.

## Spike results (2026-11-12)

Measured in Chromium with an NVIDIA GPU, a 390×844 page at 2× (a phone's size), and the CPU slowed 4×, which
stands in for a mid-range phone. The web client is `cmd/solo-web`, built with TinyGo 0.44.0
(`scripts/web.sh`), over `internal/sample`: 60 threads, most of them done, and transcripts of 400 items
(206 KB of JSON), with one thread streaming a few words every 33 ms.

Headless Chromium draws WebGL on the CPU by default. That alone held the page to 10 frames a second and made
every frame a 100 ms task, so measure with a real GPU (`--enable-gpu --use-angle=gl`) or the numbers are the
browser's, not Solo's.

### 1. The real UI under TinyGo: it runs, with one fix and one prerequisite

| | Result |
|---|---|
| Build | 6.7 MB, 1.92 MB in Brotli; 1.5 minutes, under 3 GB |
| Load (4× slowed) | canvas at 1.1 s, first frames at 1.2 s |
| Memory after load | 45 MB (23 MB with 100-item transcripts) |
| Idle | no frames, no timers: nothing runs |
| A thread streaming, Solo's conversation showing | 40 frames a second; a frame is 9.7 ms at the median, 14.5 ms at the 95th percentile |
| The same while scrolling the transcript | 56 frames a second; median 1.9 ms, 95th percentile 14 ms |
| 300 threads | no measurable difference |

**Fixed: a goroutine per change.** `onChange` started a timer for each burst of changes, and under TinyGo
every timer is a goroutine with a 1 MB stack. With a thread streaming, memory grew from 90 MB to 180 MB in
15 seconds and kept going. One long-lived refresher goroutine (`App.refresher` in `internal/gui/app.go`)
holds it at 45 MB. The refresher also waits for the UI to take an update before reading the next, so a
hidden tab, which draws no frames, no longer queues one copy of every list per burst.

**Fixed: everything rendered again for any thread's change.** Asking for a thread by ID tracked the whole
list, every refresh set every list, and a transcript's rows were laid out (decoding each item) on every
render. Each thread now has its own signal, lists are set when they differ, and rows are kept until the
items change. With another thread streaming, the median frame went from 9.7 ms to 1.0 ms and the frames
over 50 ms from 12 in 15 seconds to none.

**Remaining: the collector's pause grows with the heap.** Streaming while scrolling a 400-item transcript
still grows the heap to 180 MB (TinyGo doubles it when it fills), and a collection then stops the page for
about 150 ms, four times in 30 seconds. The remote protocol's smaller first window of items is what keeps
the heap, and so the pause, small.

### 2. Encryption and encoding under TinyGo: no obstacles

`cmd/spike-wire`, the same build settings, both ends of the handshake in one page.

| | Normal speed | 4× slowed |
|---|---|---|
| Noise IK handshake with a pre-shared key, both ends (`github.com/flynn/noise` v1.1.0, X25519, ChaCha20-Poly1305, BLAKE2s) | 5.4 ms | 25 ms |
| One 16 KiB record, sealed and opened | 0.17 ms (91 MiB/s) | 0.76 ms (21 MiB/s) |
| Key pair | 1.2 ms | 5.5 ms |
| 60 threads as JSON (175 KB): encode, decode | 2.8 ms, 3.9 ms | 16 ms decode |
| 300 threads as JSON (901 KB): decode | 9.7 ms | 42 ms |
| 400 items as JSON (206 KB): decode | 2.7 ms | 9.2 ms |

- **Size:** the crypto adds 94 KB to the build, 27 KB in Brotli (1.4% of the client).
- **Memory:** none added. Go's 32 MB crypto buffer on the desktop does not appear under TinyGo: the page is
  24 MB with or without the crypto.
- **Randomness:** TinyGo's `crypto/rand` reaches `crypto.getRandomValues` through `wasm_exec.js`, which
  throws when the browser has none. A reader that calls it directly also works, and is what the client will
  use, so nothing can fall back.
- **Pre-shared key placement decides pairing.** With placement 1 the machine rejects a wrong key at the first
  message. With placement 2 it accepts the first message from anyone who has its public key, and only the
  device learns of the mismatch. Pairing therefore uses placement 1, so the machine shows nothing for a
  request that does not hold the one-time secret.
- **JSON stays as the wire encoding.** `encoding/json` under TinyGo round-trips every type the UI uses
  (`core.ThreadView` with its embedded `store.Thread`, `core.Item`, `core.Settings` with its map, times,
  pointers, `omitzero`). Decoding is far under the 50 ms that would have justified MessagePack.

### 4. A plain end-to-end slice: the path works, and the bridge costs nothing

`cmd/spike-host` is a machine and a relay in one process; the page is `cmd/solo-web` with `?relay=1`. No
encryption and no deltas (`internal/remote/naive`): the machine tells the page of every change, and the page
fetches the whole state again as JSON, 176 KB each time. The relay forwards messages and reads nothing.

| 4× slowed, one thread streaming | Result |
|---|---|
| First frames | 1.3 s, including the first state from the machine |
| Frames | 35 a second; median 2.1 ms, 95th percentile 14.6 ms, 7 over 50 ms in 15 s |
| Memory | 47 MB, flat (188 MB while scrolling, as in spike 1) |
| Traffic | 541 whole states in 18 s: 5.3 MB and 65 messages a second |

- **The WebSocket bridge works under TinyGo** (`internal/remote/webws`, `cmd/solo-web/web/solo.js`): messages
  arrive through exported functions and a shared buffer, as jamb's frames do, so 65 messages a second cause
  no collector pauses of their own. A build with Go uses `syscall/js` callbacks instead.
- **The naive protocol is survivable but unaffordable.** The page keeps up even while decoding 5 MB of JSON a
  second, which says the client has room. The relay does not: 65 messages a second is 5.6 million a day
  against a free quota of 2 million, and that is one device. Coalescing, deltas and the presence lease are
  what make the relay free to run, not what make the page fast.
- **The mirror's ordering rule held in the test** (`internal/remote/naive/naive_test.go`): the page hears of
  a change only after its copy shows it.

### 3. Cloudflare through the REST API alone: everything the deploy needs works

`cmd/spike-cf`, run twice against a real account on the free plan with an account-owned token that has
Workers Scripts: Edit and Account Settings: Read. Each run made one Worker, tested it and deleted it.

| Step | Result |
|---|---|
| Token check | An account-owned token fails `/user/tokens/verify` (401) and passes `/accounts/{id}/tokens/verify`: try both |
| List accounts, read the workers.dev subdomain, list scripts | Work with that token |
| Static assets without wrangler | `assets-upload-session` with a manifest, then one multipart upload per bucket (field name is the hash, value is base64), then the completion token in the script's metadata. Three files, one of 1.2 MB, in three buckets |
| Asset hash | **Any 32 hex characters are accepted**: SHA-256 of the base64 text plus the extension worked, so BLAKE3 (wrangler's choice) is not needed |
| Worker, SQLite Durable Object, plain-text bindings and assets | One upload; answering 2 seconds after workers.dev is enabled |
| Security headers on assets | The `_headers` text goes in the metadata (`assets.config._headers`), not as a file; the content security policy, `nosniff` and `no-referrer` came back on `/` |
| Asset responses | `application/wasm` kept; `Cache-Control: public, max-age=0, must-revalidate` by default; a path that is neither an asset nor a Worker route is the Worker's 404 |
| WebSocket to the Durable Object with hibernation | Opens and echoes |
| Reading settings back | Plain-text binding values are returned, so a second machine can read `HOST_KEYS` and append its key |
| Versions and deployments | Listed, with `created_on`, `author_id`, `source: "api"` and `workers/triggered_by: "upload"`: enough to tell an upload Solo made from one it did not |
| Re-upload with a changed binding, no migration, `keep_assets: true` | Live in 1 to 3 seconds; assets kept; versions go from 1 to 2 |
| Repeating the migration | Refused: HTTP 412, "Actor migration tag precondition failed". The migration is sent once, and later uploads leave it out |
| Delete | Works; the URL answers 404 within seconds (once it still answered 200 three seconds later) |

- **A re-upload does not close open WebSockets.** A socket opened before the re-upload still echoed 94
  seconds after it. So adding a machine's key does not disturb connected devices, and an update cannot count
  on the platform to disconnect old clients: the relay tells them itself. The Durable Object that holds those
  sockets may also still have the old bindings, so the machine-key check belongs in the Worker's stateless
  handler, which had the new value at once.
- **Not measured:** how awake time is billed under steady traffic (it needs hours and the analytics API), and
  whether a re-upload with changed code, rather than a changed binding, closes sockets.

## What the spikes change

- The order of work stands. The UI prerequisites came first and are in: the refresher, a signal per
  thread, and rows laid out once.
- Pairing uses pre-shared key placement 1.
- JSON is the wire encoding.
- The deploy needs no wrangler and no BLAKE3: Go's `net/http` and SHA-256 are enough. Token verification tries
  the account-owned endpoint as well as the user one.
- Machine keys are checked in the Worker, not in the Durable Object, and an update announces itself to
  connected clients.
- `scripts/web.sh`, `cmd/solo-web` and `internal/sample` carry over as they are, and the WebSocket bridge
  comes with the web client. The spike programs (`cmd/spike-wire`, `cmd/spike-host`, `cmd/spike-cf`, the
  naive protocol) stay on the `spike/remote` branch and do not ship; `cmd/spike-cf`'s upload code is what
  `internal/cloudflare` starts from.
