# Solo — architecture (draft v0.1)

A desktop app where you talk to one long-running **orchestrator** that starts, watches, and reports on
Claude **worker threads**, each working in its own git worktree.

## Decisions

| Area | Decision |
|---|---|
| UI | Go + [jamb](https://github.com/tyler-builds/jamb). Windows is the primary target; Linux (Wayland) is experimental and has no IME; macOS waits on jamb. |
| Claude | Official `@anthropic-ai/claude-agent-sdk` in one **Bun sidecar** (`bun build --compile`). The SDK isn't open source and wraps a closed `claude` binary over an undocumented protocol, so it isn't reimplemented in Go. |
| Orchestrator | One **global** session, resumed across restarts. Knowledge of live state comes from tools, not its transcript. |
| Repos | Settings hold a list of folders; every git repo found under them is known to the orchestrator. |
| Workers | One git worktree + branch per thread, `bypassPermissions`, no sandbox. |
| Landing | Orchestrator reports diff and test results; **you** click Merge. |
| Layout | Title bar (go-to-thread, settings) · thread rail on the left · orchestrator chat · a thread panel that slides in from the right (50/50, resizable). The rail filters by repository when threads span several. |
| Persistence | SQLite (`modernc.org/sqlite`, no cgo), owned by Go. Claude's own transcripts are used only for `resume`. |

## Process model

```
┌──────────────────────── solo (Go) ─────────────────────────┐
│ jamb UI ── signals ── core                                  │
│                       ├─ OrchestratorManager                │
│                       ├─ ThreadManager (worker lifecycle)   │
│                       ├─ RepoIndex (scan folders, git info) │
│                       ├─ Worktrees (git worktree add/rm)    │
│                       ├─ Store (SQLite)                     │
│                       └─ SidecarClient (supervisor + RPC)   │
└──────────────┬──────────────────────────────────────────────┘
               │ NDJSON JSON-RPC over stdio (bidirectional)
┌──────────────▼───────── solo-agent (Bun) ───────────────────┐
│ SessionHost: Map<sessionId, Session>                        │
│ Session = one long-lived query() with a streaming prompt    │
│ Normalizer: SDKMessage → compact events                     │
│ In-process MCP server (orchestrator tools → RPC to Go)      │
└──────────────┬──────────────────────────────────────────────┘
               │ one `claude` child process per session (SDK-managed)
```

- A single sidecar hosts every session. If it crashes, Go restarts it with backoff and sessions
  reopen lazily with `resume`.
- Go is the source of truth for threads and state. The sidecar holds no durable state.

## Sidecar protocol (sketch)

Go → sidecar (requests): `session.open {sessionId, resume?, role, cwd, model, effort, systemAppend,
additionalDirectories, permissionMode, tools}`, `session.send {sessionId, uuid, text, attachments}`,
`session.interrupt`, `session.close`, `session.setModel`, `probe` (models, account, version).

Sidecar → Go (notifications): `event {sessionId, seq, kind, ...}` where kind is one of `init`,
`text_delta`, `thinking_delta`, `message_done`, `tool_start`, `tool_input`, `tool_result`,
`subagent_start|progress|end`, `compact`, `usage`, `rate_limit`, `turn_end {status, reason, usage,
costUsd}`, `question`, `error`, `exited`.

Sidecar → Go (requests): `tool.call {sessionId, name, args}` for orchestrator MCP tools, and
`question.ask` for AskUserQuestion. Go answers both.

Raw `SDKMessage`s are also appended to a per-session JSONL file for debugging and replay tests.

## Claude SDK correctness rules (ported from t3code `ClaudeAdapterV2`)

1. **Streaming input, one query per session.** `prompt` is an AsyncIterable fed from an unbounded
   queue. Follow-up turns are `offer`ed into the same live query, not a new `query()`.
2. **Identity.** New session: pre-allocate a uuid and pass `sessionId`. Existing session: `resume`.
   Never reuse a fresh id with `sessionId` (a metadata-only transcript may already exist).
3. **Turn gating by prompt uuid.** Each `SDKUserMessage` carries a uuid. Frames are attributed to a
   turn only after the uuid echoes back; earlier root frames are held, which prevents background
   wakes from bleeding into the wrong turn. `result` closes the turn.
4. **Result interpretation.** `subtype: "success"` can still be a failure (`is_error`,
   `api_error_status` 429/529, `terminal_reason` such as `prompt_too_long` or `blocking_limit`).
   `terminal_reason: aborted_tools|aborted_streaming` means interrupted. Map these to explicit
   statuses.
5. **Options.** `systemPrompt: {type:"preset", preset:"claude_code", append}`,
   `includePartialMessages: true`, `thinking: {type:"adaptive", display:"summarized"}`,
   `settingSources: ["user","project","local"]`, `cwd` + `additionalDirectories`, and
   `pathToClaudeCodeExecutable` when the user overrides the binary.
6. **Process hygiene.** Spawn `claude` in its own process group on POSIX
   (`spawnClaudeCodeProcess`) so the whole tree can be killed. On a failed open: shut the prompt
   queue, SIGKILL, await exit (10 s cap), then `asyncDispose`. Capture a stderr tail **only before the
   first message** (startup errors) and keep later stderr out of the UI.
7. **Interrupt.** `interrupt()`, then wait for the turn's `result`. To restart a wedged session:
   `close()`, and the next turn reopens with `resume`.
8. **MCP registration** can time out. Retry once, and only for resumed opens.
9. **Probe** capabilities (models, account) with a query whose prompt never yields, then read
   `initializationResult()`, instead of hard-coding the model list.

### Verified in phase 1 (live, claude 2.1.319, SDK 0.3.319)

- Turns bind by uuid: `result.user_message_uuids` and the first stamped frame carry the send's uuid.
- Interrupt ends the turn with `terminal_reason: aborted_streaming` → `interrupted`.
- A SIGKILLed CLI mid-turn: the stream just ends with no result. The sidecar closes the turn as
  `failed/process_exited`, emits `exited`, and the next send reopens with `resume`; context survives.
  The raw log gets a `{"type":"$exit"}` marker so replays can reproduce this.
- Under `bypassPermissions` the SDK warns that `canUseTool` is shadowed, but **AskUserQuestion is
  still routed to it**, so worker questions reach the app.
- In-process MCP tools are marked `alwaysLoad`; otherwise Claude spends a ToolSearch round trip first.
- The CLI also emits `command_lifecycle` (queued/started/completed per send uuid), which is useful
  for a "queued" state later.
- Claude binary: Solo runs the user's own Claude Code, as t3code does, so it shares their install,
  sign-in and updates. `agent.FindClaude` tries the Settings path, then `claude` on PATH, then the
  installers' default locations (a desktop launcher's PATH often lacks `~/.local/bin`). A compiled
  sidecar can't fall back to the SDK's bundled CLI, so it refuses to open without a path. Settings
  shows the version and account found (`core.CheckClaude`: `--version`, then a probe).

## Orchestrator

- **Session:** `cwd` = app data dir, `additionalDirectories` = configured repo folders. Tools are
  read-only built-ins (Read, Grep, Glob, plus read-only git through Bash with a `canUseTool`
  allowlist) and the `solo` MCP tools. It does not edit code itself.
- **MCP tools:** `list_repos`, `list_threads`, `get_thread(id)` (status, last summary, files changed,
  pending question), `read_thread(id, tail)`, `spawn_thread(repo, title, prompt, model?, base?)`,
  `message_thread(id, text)`, `interrupt_thread(id)`, `get_diff(id)`,
  `propose_merge(id, summary)` (renders a Merge card; only you can press it), `answer_question(id,
  answer)`.
- **Proactive updates:** when a worker finishes, fails, or asks a question, Go queues a synthetic
  `[thread update]` message. Updates are batched while the orchestrator is busy and delivered when
  it's idle, so it reports without being asked. These render as small status chips, not user bubbles.
- **Memory:** SDK auto-compaction handles length. Nothing critical lives only in its context
  because state is always re-queryable. If `resume` fails, Go starts a new session seeded with a
  ledger (repos, open threads, recent decisions); the UI history stays continuous.

## Worker threads

- `git worktree add <data>/worktrees/<repo>/<slug> -b solo/<slug> <base>`. The worktree is removed
  after merge or discard; the branch is kept until merged.
- Session: `cwd` = worktree, `permissionMode: "bypassPermissions"`,
  `allowDangerouslySkipPermissions: true`, default model configurable.
- AskUserQuestion goes to Go → shows as a card in the rail and orchestrator chat. You or the
  orchestrator (`answer_question`) can reply.
- Status machine: `queued → starting → running ⇄ waiting_input → idle(done) | failed | interrupted
  → merged | discarded`. A concurrency cap (default 4) queues extra threads.

## UI (jamb)

- **Center:** orchestrator chat. Markdown via `jamb/markdown`; tool calls as collapsed chips;
  status chips; Merge and Question cards inline.
- **Right rail:** thread cards (title, repo, status dot, elapsed time, last activity line).
- **Slide-over:** a thread's transcript, a diff (`codeedit`, read-only), an input to message it
  directly, and Interrupt / Merge / Discard buttons.
- Text deltas are coalesced (~30 ms) before signals update, to keep re-renders cheap.

## Repo layout

```
cmd/solo/            main
internal/core/       orchestrator, threads, notifications
internal/store/      SQLite schema + queries
internal/git/        repo scan, worktrees, diff, merge
internal/agent/      sidecar supervisor + JSON-RPC client + event types
internal/ui/         jamb views
sidecar/             TypeScript (Bun): rpc, session, normalize, tools; fixture tests
docs/
```

## Build phases

1. **SDK layer:** sidecar + protocol + a headless Go harness running one worker end to end.
   Replay-fixture tests for turn gating, result mapping, interrupt, crash/resume.
2. **Core:** store, repo index, worktrees, ThreadManager.
3. **Orchestrator:** session, MCP tools, proactive updates.
4. **UI:** jamb views.
5. **Packaging:** `scripts/appimage.sh` (Linux). The compiled sidecar ships beside the app; Claude
   Code is the user's own.

## Open defaults (change if you disagree)

- Orchestrator model: Opus; workers: Sonnet. Both configurable per thread.
- Auth: the user's existing Claude Code login or `ANTHROPIC_API_KEY`.
- App name: **Solo**.
