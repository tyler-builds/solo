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

The first run walks you through a short setup: it checks that Claude Code is installed and signed in, lets you
pick the models and effort for Solo and its threads, adds the folders that hold your repositories (suggesting ones
such as `~/code` when they hold any), checks `gh` or `glab` for pull requests and how they merge, and sets a few
preferences. Every step can be skipped and keeps its defaults; all of it is in Settings later, and Settings ›
General can run the setup again. A project that isn't in git works too: its threads edit the folder directly, without their own copy or
a diff, until you click "Put in git". Data lives in `~/.local/share/solo` (`%LOCALAPPDATA%\Solo` on Windows;
`SOLO_DATA_DIR` picks another folder, such as an empty one to see the setup again); `SOLO_DEBUG=1` also records raw
SDK messages under `logs/` there.

## Package

```
./scripts/appimage.sh                 # dist/Solo-x86_64.AppImage (Linux)
VERSION=0.2.0 ./scripts/windows.sh    # dist/Solo-0.2.0-windows-amd64-setup.exe and .zip (builds from Linux too)
uas add dist/Solo-x86_64.AppImage     # optional: install the AppImage with uas
```

Settings › General shows the build's version: `VERSION` when it is set, as a release sets it, or else
git's description of the commit, such as `0.6.0-3-gcb015ff` for three commits after 0.6.0
(`scripts/version.sh`). The web client shows its own and the machine's.

Both need Go and Bun; the AppImage also needs `mksquashfs`, and the Windows installer NSIS
(`makensis`; without it only the zip is built). The Windows build also fetches Bun's Windows runtime
from npm and gives it Solo's icon and version info (with go-winres) before compiling the sidecar into
it, since Bun can set those only when compiling on Windows: Task Manager names the sidecar "Solo
Agent", not "Bun". The app needs
only Claude Code installed and signed in: the sidecar is a compiled binary with Bun inside, and Solo
finds `claude` on PATH or where its installer puts it (Settings shows what it found and takes a path
to use instead).

Releases: push a tag such as `v0.2.0` and `.github/workflows/release.yml` tests, builds both and
publishes them as a GitHub release (a tag with a suffix, such as `v0.2.0-beta.1`, is a pre-release).
Running the workflow by hand builds them as an artifact without releasing.

## Develop

```
go test ./...                                   # unit tests, with a fake sidecar
cd sidecar && bun test                          # sidecar tests, with recorded SDK sessions
go test -tags live -run Live ./internal/...     # real Claude, small cost
SOLO_SHOTS=/tmp/shots go test ./internal/gui -run 'TestShots|TestSetupShots'   # screenshots for review
go run ./cmd/solo-harness -cwd <repo> "<task>"  # one session in the terminal
```

## License

Solo is licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
Its release builds also contain third-party software under its own licenses, listed in NOTICE,
including the Claude Agent SDK, which is not open-source software: Anthropic provides it under its
own terms, not Solo's license.
