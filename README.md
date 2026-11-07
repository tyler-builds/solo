# Solo

A desktop app for coding with Claude agents. You talk to one long-running **orchestrator**; it plans the
work, starts Claude **worker threads** (each in its own git worktree and branch), follows them, relays
their questions, and proposes merges. You review and merge.

![Solo](docs/screenshots/dark-main.png)

Built with Go and [jamb](https://github.com/tyler-builds/jamb). Claude runs through the official Claude
Agent SDK in a small Bun sidecar. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Run

Needs Go 1.27, [Bun](https://bun.sh), git, and a Claude login (Claude Code's, or `ANTHROPIC_API_KEY`).

```
cd sidecar && bun install && cd ..
go run ./cmd/solo
```

Open Settings and add the folders that hold your repositories. Data lives in `~/.local/share/solo`
(`%LOCALAPPDATA%\Solo` on Windows); `SOLO_DEBUG=1` also records raw SDK messages under `logs/` there.

## Package (Linux)

```
./scripts/appimage.sh        # dist/Solo-x86_64.AppImage, with the sidecar bundled
uas add dist/Solo-x86_64.AppImage   # optional: install it with uas
```

Needs Go, Bun, `mksquashfs` and ImageMagick. The AppImage runtime is downloaded once into
`~/.cache/solo-build`. The app needs only Claude Code installed and signed in: the sidecar is a
compiled binary with Bun inside, and Solo finds `claude` on PATH or where its installer puts it
(Settings shows what it found and takes a path to use instead).

## Develop

```
go test ./...                                   # unit tests, with a fake sidecar
cd sidecar && bun test                          # sidecar tests, with recorded SDK sessions
go test -tags live -run Live ./internal/...     # real Claude, small cost
SOLO_SHOTS=/tmp/shots go test ./internal/gui -run TestShots   # screenshots for review
go run ./cmd/solo-harness -cwd <repo> "<task>"  # one session in the terminal
```
