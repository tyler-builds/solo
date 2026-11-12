# The relay

A Cloudflare Worker and one Durable Object that pass WebSocket frames between machines (the Solo
desktop app) and devices (Solo in a browser). The design is in `docs/REMOTE.md`, under "The relay".

The relay is trusted with nothing. What it forwards is encrypted between a device and a machine, and
it has no key to read or change it. It holds no secret: its configuration is `HOST_KEYS`, the public
relay keys of the machines that may connect. It has no runtime dependencies.

## What is here

| File | What it does |
|---|---|
| `src/index.ts` | The Worker: `/v1/info`, and the token check for `/v1/machine`, `/v1/device` and `/v1/pair` |
| `src/auth.ts` | The tokens, read and verified with WebCrypto. It mirrors `internal/remote/secure/machine.go` |
| `src/relay.ts` | The Durable Object `Relay`: the sockets, the frames, pairing tickets, limits and the frame count |
| `src/protocol.ts` | The protocol's numbers: frame types, close codes, limits |
| `src/bytes.ts` | base64url, hex and byte-string helpers |
| `wrangler.jsonc` | Configuration for the tests and for local runs. Solo deploys through Cloudflare's REST API, not wrangler |
| `test/vectors.json` | Tokens made by Go (`internal/remote/secure/vectors_test.go`). Do not edit it by hand |

## Bindings

| Name | Value |
|---|---|
| `HOST_KEYS` | The allowed machines' relay keys: 32 bytes each as unpadded base64url, separated by commas |
| `SOLO_RELAY` | A marker, any text that is not empty |
| `SOLO_BUNDLE` | The deployed bundle's version, an integer as text. `/v1/info` reports it |
| `RELAY` | The Durable Object namespace of the class `Relay`, with SQLite storage. The Worker uses one object, named `default` |

The compatibility date is `2026-10-01`.

## Test

```sh
cd relay
bun install
bun run test        # vitest, in workerd
bun run typecheck   # tsc
```

The tests run in workerd through `@cloudflare/vitest-plugin`, so the Durable Object, its storage and
its hibernating sockets are the real ones. One file runs at a time.

`test/vectors.test.ts` checks `src/auth.ts` against `test/vectors.json`. When the Go side changes a
token, `go test ./internal/remote/secure` fails until the file is written again
(`go test ./internal/remote/secure -run TestRelayVectors -update`), and these tests then say whether
the relay still agrees.

The other tests sign their own tokens. `vitest.config.ts` makes an Ed25519 key for each run, puts its
public half in `HOST_KEYS` and gives the tests its private half.

## Build

```sh
scripts/relay.sh    # from the repository's root
```

It checks the types and writes one ES module, `dist/relay/worker.js`, with `bun build`. The module
imports only `cloudflare:workers`, which the runtime provides.

## Run locally

```sh
cd relay
bunx wrangler dev --local-protocol https
```

A token is signed for `https://<host>`, and a device's or a pairing socket's `Origin` must be that
too, so over plain `http` only `/v1/info` and `/v1/machine` answer as they do when deployed. Put a
machine's relay key in `HOST_KEYS` in `wrangler.jsonc` to let it connect.
