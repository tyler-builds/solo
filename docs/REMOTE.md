# Remote access

Solo in a browser, driving a machine that runs Solo, through a relay in your own Cloudflare account. This
file is the design, as far as it has landed, and then what was measured before it was built on.

Remote control of Solo is remote code execution on the machine, since its agents run with full permissions.
So one rule shapes everything: **the relay is trusted with nothing**. It forwards bytes it cannot read or
change, and it cannot say who is at the other end. Every decision that matters is made between a device and
a machine.

## What is and is not protected

A relay that turns hostile, or anyone reading its traffic, cannot read messages, threads, diffs or images,
cannot forge or alter a request, cannot add a device, and cannot replay old traffic.

Secrets an agent asks for (see ARCHITECTURE.md, Secrets) are typed only at the machine: a device shows
that one is waiting and can decline it, but no request or reply that carries a value exists, so a value
never crosses the relay.

Not protected, by design:

- **Whoever controls the Cloudflare account can replace the web client's code**, and so use a paired
  device's key the next time that page opens. Solo keeps the Cloudflare token in the keyring, so that
  includes any program running as you on that machine.
- A stolen, unlocked, paired device has full access until it is revoked, and revoking needs the machine
  online. Deleting the Worker in the Cloudflare dashboard stops everything from anywhere.
- Anyone who finds the relay's address can flood it and use up the free plan's 100,000 requests a day;
  remote access is then down until midnight UTC. The desktop app is unaffected.
- The relay sees addresses, public keys, timing and sizes.

Limits worth knowing, found when the finished code was reviewed (see "Review" at the end):

- The check for a changed relay is after the fact: a changed web client can run for up to six hours
  before Solo pauses, and not at all if no token is kept. It is detection, not prevention.
- A machine's key stays on the relay's list until the relay is removed: a machine that was retired can
  still connect to the relay as itself, and see when devices come and go. It can't read anything, and
  can't take another machine's place; it can open pairing of its own, which a device with no code would
  reach only if no other machine had pairing open, and which the person would then have to confirm.
- A device that was removed keeps its pass to the relay: it can open connections there, which the
  machine closes unanswered.
- A paired device, even one that may only look, can ask for things that cost the machine work (a long
  diff, a search). What it can make the machine hold or run at once is bounded.
- A relay on this machine (`cmd/solo-localrelay`) is for development: another program that takes its
  port stands in for it, and for the web client it serves. Settings says so.

## Identities and keys (`internal/remote/secure`)

No primitive is written here. The handshake is the Noise protocol (`github.com/flynn/noise`) with X25519,
ChaCha20-Poly1305 and BLAKE2s; keys for separate uses come from HKDF with a label for each.

- **A machine** has a 32-byte seed, its one secret. From it come the X25519 key it speaks Noise with and the
  Ed25519 key it signs for the relay with. Its ID is a hash of the two public keys.
- **A device** has an X25519 key, made in the browser. Its ID is a hash of the public key.

## Pairing

A device is added only at the machine, and the person there approves it.

1. The machine makes a **pairing code** and shows it as a QR code for
   `https://<relay>/#p=<code>`: the machine's ID, its Noise public key, a secret used once, and an expiry.
   The fragment is not sent to any server, and the page removes it from the address at once.
2. The device runs the handshake as the one who starts it, pinned to the machine's key from the code, with a
   pre-shared key derived from the code's secret. Both ends hash the purpose ("pair"), the relay's origin,
   the machine's ID and the whole code into it, so a code that was altered on the way, or a message meant for
   another relay or another purpose, makes the handshake fail.
3. **The pre-shared key is mixed in at the first message** (Noise's IKpsk1). The machine therefore rejects,
   at its first look, anyone who does not hold the code; with the usual placement it would answer anyone who
   knows its public key. Neither handshake message carries data, since a first message can be replayed. The
   machine acts on nothing until a message opens on the new channel, which only the device that ran this
   handshake can make.
4. Both ends show **six digits** derived from the handshake's hash. The person types the device's digits
   into the machine, which approves the request that has them. Someone who photographed the QR code can also
   ask to pair, but their handshake has other digits. The machine says only that a device is asking: a
   device's name is its own choice of words, and is not shown until it is approved. A code answers eight
   handshakes at most, whatever the relay lets through, and five wrong sets of digits end it.
5. On approval the machine records the device's public key, gives it a random key of its own for later
   sessions and a voucher for the relay, and the code is spent.

### Pairing without a code

A browser on another computer has no camera to scan with, and nobody can hand it a link. For that, the
person chooses, at the machine, to pair a computer's browser instead of showing a code: the card then
shows the relay's address. They open it there and choose Pair this device, the page shows six digits,
and they type them at the machine (over remote desktop if that is how they reach it). The two kinds of
pairing are never open together: nothing but the digits tells a device with no code from one with a
code, and pairing with a code should owe nothing to chance.

Here the device knows nothing of the machine beforehand, so the handshake (Noise XX) authenticates
nobody, and the six digits do all the work. Six digits are few, and the relay sits in the middle of what
would be two handshakes, one with each end; left alone, it could shape its part of the device's handshake
until the device showed digits the relay had already got from the machine. So **the device commits
first**: it sends a hash of its public key and of a random number, and reveals the number only when the
handshake is done; the digits depend on it. Towards the device, the relay must finish before it learns
the number; towards the machine, the relay is the one committed, and the machine checks the key and the
number against the commitment. The two sets of digits are then independent, equal once in a million.

What bounds the tries: the machine answers eight connections for each time pairing is opened; a
connection has ten seconds to finish its handshake and say who is asking, in one small message, and then
waits two minutes at most; three may wait at once. So a few in a million, for each time the person
types digits, is the chance of whoever controls the relay being paired in place of the device. With a
code there is no such chance: the machine's key comes with the code. (Whoever controls the relay can
also replace the web client, which is the larger exposure, and is said above.)

The digits prove the device to the machine, not the machine to the device. The device checks what it is
sent (the voucher is for itself, and the machine's ID is the hash of the key the handshake was with and
of the key the voucher is signed with), but anything that answers its handshake can say "paired". So
the page keeps nothing until the person has said that their machine says so too. A pairing with a code
that fails is followed by "scan again", never by an offer to pair without one.

Anyone who reaches the address while this is open can ask, and shows other digits; the machine says only
how many devices are asking. They can also use up the eight connections, which ends that opening and no
more. At the relay, a machine opens pairing for itself only, for ten minutes and sixteen connections at
most whatever it asks; with two machines open the relay doesn't choose, and tells the device so.

## Sessions

A paired device runs the same handshake with the purpose "session" and its own key from pairing as the
pre-shared key. The machine looks the device up by the ID the relay passed along, and then demands the
device's registered public key in the handshake: the relay's word alone opens nothing. Every connection is a
new handshake with new keys.

On a finished handshake's **channel**, a message that was altered, repeated, dropped or reordered does not
open, and the channel is over; the device connects again.

A connection belongs to its device from the moment the relay opens it. **Removing a device, or limiting it
to looking, closes every connection it has**, whatever the connection is doing: a handshake under way and
a connection that hasn't yet spoken included. The machine also reads the device's record again once the
handshake is done, gives a connection ten seconds to say hello, and lets a device have three connections
at a time. A device counts as seen when it has said hello, not when a first handshake message arrives,
which the relay could send again.

## Messages (`internal/remote/wire`)

A message is cut into records of at most 60,000 bytes, each sealed and sent as one frame. One message
crosses at a time in each direction, and a message is at most 1 MiB: anything larger is asked for in pages by
the layer above. A machine compresses what it sends before sealing it (text compresses five times or more,
and the relay's quota counts frames); a page does not.

The other end is authenticated but not trusted to behave, since a paired device may be taken over. Every
size is checked before anything is kept: a record that is not the message's last must be full, a flag no
link uses is refused, and a compressed message may inflate to 8 MiB at most. Breaking a rule ends the link.

## Requests and state (`internal/remote/rpc`)

Inside the channel, a device and a machine exchange JSON envelopes: `hello` and `welcome`, requests and
their answers, and `sync`, the machine's state as it changes. `Host` serves devices over any
`api.Backend`; `Client` is an `api.Backend` that keeps a mirror of the machine, which the web UI reads
without waiting.

- **Large things cross in parts.** An envelope over 192 KiB (an image, a long diff, a transcript) is cut
  into parts that other messages cross between, at most four unacknowledged: a device that stops
  reading stops the machine, so nothing piles up at the relay. Bytes travel after the JSON, not in it.
- **State, then the news.** Each `sync` carries what changed, then the machine's own `core.Change`
  records as it made them: notifications depend on their details, and a listener told of a change finds
  it already in the mirror. At most one `sync` every 300 ms, which is what keeps streaming text within
  the relay's quota.
- **Watched threads.** A device names the threads on screen (`watch`, renewed every 25 s; it lapses after
  45 s). Only those get their transcript, as the items that differ from what was sent, and their
  streaming text, as the text added. A lapsed lease stops both: a hidden or lost page costs nothing.
- **Resume.** A machine's run is an epoch, and each change a revision. A device that reconnects says what
  it holds; in the same epoch it is sent the threads that changed since, and a transcript only if its
  revision moved. A new epoch starts it over.
- **The machine's time.** The machine's times (when a turn or background work began) are stamped by its
  clock, so the welcome and each `sync` carry that clock, and the UI measures against the machine's time
  as the device estimates it (`Client.Now`), not the device's own clock. That clock can be minutes off,
  and in a page it falls behind while the device sleeps: under TinyGo it is `Date.now()` at load plus
  `performance.now()`. Within a session the estimate only moves forward to a later reading; a new session
  starts it over.
- **Requests run once.** Each has a random ID. A device whose connection drops sends it again on the
  next, and the machine answers from what the first run returned. A request that changes something runs
  to its end even when the device goes.
- **What a device may ask** is checked at the machine (`methods.go`): sizes and counts are bounded,
  threads are named by ID, images by attachment, a folder put in git must be one the machine found, the
  Claude checked is the machine's own, and the settings that name programs are refused
  (`api.SettingsPatch.CheckRemote`). A read-only device's changes are all refused. Every change is
  handed to the audit log with the device's name.
- **What a device can cost** is bounded before it is read: nothing arrives in parts before hello or from
  a device that may only look, a message in parts must finish within two minutes, only an image upload
  carries bytes, a device has sixteen requests under way at most over all its connections, the answers
  kept for repeats are bounded in number and in bytes, images are decoded one at a time, and Claude is
  checked at most every thirty seconds however many devices ask.
- **Liveness.** A device pings every 5 s; either end gives a session up after 15 s of silence.

Tests run a device against a machine in memory, against the real engine over the real encryption, and
against peers that break the rules; the frame decoder is fuzzed, and what a cold start, a resume and a
transcript cost on the wire have ceilings (`TestSizeBudgets`, `TestResume`).

## The machine's side (`internal/remote/host`)

`host.Remote` is what the desktop app runs. It keeps the machine's seed in the OS keyring (in a file only
the user can read where there is no keyring, which Settings then says), and the relay's address and the
paired devices in `remote.json`, also the user's alone. The keyring is read when remote access is first
used, not at every start.

- **The relay connection** is one WebSocket the machine opens, signed for with its relay key. It is
  retried with a growing, jittered wait; a relay that refuses the machine is said so in Settings rather
  than retried quickly; a clock that is off is corrected once from the relay's `/v1/info`.
- **Each device's connection** arrives on that socket. What the relay says of it (which device) only
  picks the key to try: the handshake must then show the public key approved at this machine. A device
  that was removed still gets through the relay with its voucher, and gets no answer here.
- **Pairing** offers one code at a time, for five minutes by the machine's clock. A request is shown only
  after a message arrives on the channel its handshake made. `Approve` takes the six digits typed at
  the machine, pairs the one request that shows them, and turns the others down; five wrong sets of
  digits end the code.
- **Sessions** are keyed afresh at least every hour, and end at once when a device is removed, its
  access changes, or remote access is turned off.
- **The audit log** (`remote-audit.log`) has a line for every pairing, removal and request that changed
  something, with the device's name.

Settings › Remote drives it through `api.Remote`: the relay's address and state, the devices with their
access (full, or only looking), and pairing with the QR code and the digits.

`internal/remote/device` is a device's side of pairing and connecting, shared by the web client and the
tests. `internal/remote/relaytest` is the relay in Go, for tests and for running the whole path on one
machine; the host's tests run over it with real WebSockets, including a relay that alters or drops
frames.

## The web client (`internal/webclient`, `cmd/solo-web`)

The page a device opens is the app's own UI, built with TinyGo, inside a small shell that pairs and
connects. `webclient.Shell` is that shell, and builds on every platform, so its tests run it against a
real machine and relay; `cmd/solo-web` gives it the browser through `internal/webhost` and the page's host
script, `solo.js`.

- **What a device keeps** is in the browser's storage for the relay's origin: its key pair, and for each
  machine the machine's public key, the device's session key and its voucher. A pairing code is never
  kept, and is taken out of the address as the page opens.
- **Screens.** Not paired: how to pair, which is done at the machine. A pairing link: a question first,
  then the six digits to type at the machine. Paired: the machine's UI once it has been read in full,
  which then stays up over a lost connection, under a line that says so. More than one machine: a list,
  reached from the machine's name in the app.
- **Connection health.** A dropped connection is tried again at once, then with waits that double up to
  30 seconds, and immediately when the page is shown again or the network returns. The page tells apart
  a machine that is offline (the relay says so), a relay that can't be reached, a device the machine
  removed (it is told, and stops trying), and versions that differ.
- **In the browser** messages cross between the script and the app as lengths over a shared buffer,
  the way jamb's own input does, so that a stream of them makes the collector no work; and everything
  the script does that can throw is caught there, since a panic ends a TinyGo app.
- **Served with** a content security policy that allows the page's own scripts, its WebAssembly module
  and connections to its own origin, and nothing else (`internal/remote/webheaders`).

`go run ./cmd/solo-localrelay` serves the built web client and a relay on this machine, for using remote
access without Cloudflare; with `-demo` it also runs a machine over sample data. `scripts/e2e.sh` drives
that in a real browser at a phone's size: pairing, use, a lost machine, a reload, and removal.

## Setting it up in Cloudflare (`internal/cloudflare`, `internal/remote/bundle`)

Solo deploys the relay itself, through Cloudflare's REST API, with a token the user makes: no wrangler and
no Node on the user's machine. Settings › Remote asks for the token once and keeps it in the OS keyring;
`CLOUDFLARE_API_TOKEN` is used when none is kept. The token needs to read account settings and edit
Workers scripts, in one account.

- **What is deployed** is built into the app: `scripts/bundle.sh` builds the web client and the relay and
  puts them in `internal/remote/bundle/files`, with the time of the build as their version. A Solo built
  without them can use a relay and can't deploy one.
- **Deploying** finds the account's workers.dev address, uploads the web client as static assets with its
  headers, uploads the relay with its Durable Object (declared once) and its bindings, serves it at
  `https://solo.<account>.workers.dev` (a longer name only if the account has another Worker called
  solo) with preview addresses off and no logs, and waits
  for it to answer.
- **A second machine** finds the relay by its marker binding and adds its key to `HOST_KEYS`, reading the
  list back in case another machine wrote at the same moment. It replaces the code and the web client
  only when its own bundle is newer; a relay is never replaced by an older one.
- **The check for changes.** Whoever controls the Cloudflare account can replace the web client, and a
  replaced web client could use a paired device's keys. Solo records the uploads of the relay as they
  stand right after its own change, and which one is served. At start, after setting up, and a few times
  a day it looks again: an upload it doesn't know, a switch back to an earlier one, or a token Cloudflare
  no longer takes (which leaves the relay unchecked) closes the relay connection and says so in Settings,
  with what was found. Nothing resumes it but the user's answer: the change was theirs (another of their
  machines made it), or resume with every device removed, or remove the relay. Turning remote access on,
  entering the address again, setting up again or restarting don't. A relay that was deleted from
  elsewhere takes its devices with it here. This needs the token; without it nothing is checked.
- **Removing** deletes the Worker and removes every device here, which cuts off every device and machine.
  Deleting the Worker in the Cloudflare dashboard does the same from anywhere.

`TestDeployAndJoin` runs all of this against a stand-in for the API. `SOLO_CF_TOKEN_FILE=… go test
./internal/remote/host -run TestRealCloudflare` runs it in a real account: it deployed and answered in
16 seconds, served the web client under its headers, carried a pairing and a session through the edge,
let a second machine join without dropping the first one's session, and removed what it made.

## The relay

A Cloudflare Worker and one Durable Object, with the web client as static assets. It holds no secret. Its
configuration is `HOST_KEYS`, the public relay keys of the machines allowed to connect, which only someone
with the Cloudflare account can change.

**Admission** is by a token in the WebSocket's second subprotocol (the first is `solo.v1`), base64url:

| Path | Token | Checked |
|---|---|---|
| `/v1/machine` | the machine's relay key, its ID, the time and a nonce, signed by that key for this relay's origin | by the Worker, which keeps nothing: the key is in `HOST_KEYS`, the signature is good, the time is within two minutes |
| `/v1/device` | a voucher: the machine's signature, for this relay's origin, over its ID and the device's | by the Worker: the signing key is in `HOST_KEYS` |
| `/v1/pair` | the machine's ID and a ticket derived from the pairing code's secret | by the Durable Object against the ticket's hash, which the machine registered. The relay never has the secret |
| `/v1/pair` | one byte, for a device with no code | by the Durable Object: exactly one connected machine has pairing open to such devices, and its window has connections left |

A voucher is a pass to the relay, not to the machine. `/v1/device` and `/v1/pair` also require the page's
own origin, and `/v1/machine` refuses a request that has one: a page is no machine.

**A machine is its key and its ID together.** A token signed by another listed key that claims a machine's
ID is another machine to the relay: it takes nothing over, receives none of the first one's devices, and
a pairing connection goes to the machine that registered the ticket. A right ticket is always let in; wrong
ones are counted only to ration what is written down.

**Forwarding.** A device's socket carries only its own conversation, so its frames are the sealed records
themselves. A machine's one socket carries every device's, so its frames start with a type:

| Type | Direction | Body |
|---|---|---|
| 1 data | both | connection (8 bytes), then a sealed record |
| 2 open | to the machine | connection, kind (1 device, 2 pairing with a code, 3 pairing without), the device's ID |
| 3 close | both | connection |
| 4 ticket | from the machine | add or remove a ticket (its hash, expiry, uses), or open or close pairing without a code (expiry, uses) |
| 5 usage | to the machine | the day's counts, as JSON |

The relay stamps the connection from the socket a frame arrived on. Lying about it only makes a handshake
fail.

`relay/` is the Worker and its Durable Object, in TypeScript with no dependencies at run time, and
`scripts/relay.sh` builds it into the one module Solo uploads. Sockets hibernate: who a socket is lives in
its tags and attachment, so routing survives the object being evicted. It writes to storage for tickets
and once in a thousand frames for the day's count, never per frame. Its own tests run in the Workers
runtime (`cd relay && bun run test`), and check the tokens against vectors the Go code made.
`SOLO_TEST_WORKER=1 go test ./internal/remote/host -run TestAgainstTheWorker` runs the machine's and the
device's Go code against that Worker on this machine: pairing, sessions and removal.

A device's large uploads are paced under the relay's limit on a device's frames (50 a second after a
burst of 200), which exists so that one device cannot spend the day's quota.


## What was measured first (2026-11-12)

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

## Review

Before any lasting deployment the finished code was read by two reviewers who had not written it, one
for the Go side (keys, handshakes, the request layer, the machine's side) and one for the relay, the web
client and the Cloudflare deploy. Neither found a way in for anyone not paired. What they found was
fixed, each with a test:

- A removed device, or one just limited to looking, could keep a connection it had parked before the
  change and start a session on it for up to an hour. Removal now closes every connection of the device.
- A changed relay could be accepted without the user knowing: the pause could be got round, and
  recovering from one left the devices trusted. See "The check for changes".
- A listed machine could take another machine's place at the relay by claiming its ID.
- A device, even one that may only look, could make the machine hold gigabytes or start Claude without
  limit; anyone who knew a machine's ID could keep it from pairing; a message could make the desktop app
  exit when its text was searched for.
- The name a device gave itself was shown beside the box for its digits.
- The release workflow ran actions by tag in a job that could write to the repository.

Pairing without a code was added afterwards and reviewed on its own. The commitment held; what was found
around it was fixed before it shipped: a device took "paired" from whoever answered it, any listed
machine could draw every such pairing to itself, a connection could hold its digits unseen for as long as
pairing was open, and it shared its limits with pairing by code.

Left as they are, and listed under "What is and is not protected" where they matter: a relay's list of
machines never shrinks, vouchers don't expire, a long diff is built whole before it is cut, and the web
client's mirror trusts its own machine to send sensible amounts.
