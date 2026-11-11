# Solo — architecture (draft v0.1)

A desktop app where you talk to one long-running **orchestrator** that starts, watches, and reports on
Claude **worker threads**, each working in its own git worktree.

## Decisions

| Area | Decision |
|---|---|
| UI | Go + [jamb](https://github.com/tyler-builds/jamb). Windows is the primary target; Linux (Wayland) is experimental and has no IME; macOS waits on jamb. |
| Claude | Official `@anthropic-ai/claude-agent-sdk` in one **Bun sidecar** (`bun build --compile`). The SDK isn't open source and wraps a closed `claude` binary over an undocumented protocol, so it isn't reimplemented in Go. |
| Orchestrator | One **global** session, resumed across restarts. Knowledge of live state comes from tools, not its transcript. |
| Repos | Settings hold a list of folders; every git repo found under them is known to the orchestrator. A folder there that isn't in git (and holds no repo) is listed too and takes threads the way t3code does: the thread works directly in the folder (`Part.InPlace`), with no worktree, branch, diff or merge, alone (not with other repos), and Done or Discard never touch the folder. Settings or Solo's `init_repo` tool can put the folder in git instead: `git init` on main, a default `.gitignore` if it has none, and a first commit. |
| Workers | One git worktree + branch per thread, `bypassPermissions`, no sandbox. |
| Landing | Solo proposes; **you** click Open PR (GitHub via `gh`, GitLab via `glab`), then Merge once checks pass, or the forge auto-merges. Repos without a remote merge locally ("Merge locally"): `git merge-tree` builds the merge without touching a checkout, so it works with uncommitted changes or another branch checked out; a checkout on the base branch then fast-forwards, keeping your edits. A conflict goes back to the thread to resolve ("Resolving conflicts"), and the branch merges when it finishes (at most twice). A thread that commits its work straight into the base branch itself (clean worktree, its commit already in `main` or `origin/main`) is marked merged, ready for Done. |
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
additionalDirectories, permissionMode, tools}`, `session.send {sessionId, uuid, text, images}`,
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
   **Steer** (t3code's `steerTurn`): move the turn's foreground subagents to the background
   (`backgroundTasks(toolUseId)`, 5 s cap), then offer the message at `priority: "now"`. Claude ends the
   running segment with an `aborted_*` result and carries on in a new segment that answers the steer;
   the sidecar marks that result `steered`, so the app keeps the turn going.
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
  Read, Grep, Glob, Bash, WebFetch, WebSearch, AskUserQuestion and the `solo` MCP tools, under the
  `allow` tool policy: Bash runs any command (compound commands, `cd`, builds, scripts, git writes).
  Edit and Write aren't offered. By process, not permission, code changes to repositories go to worker
  threads so they get a branch, diff and review.
- **MCP tools:** `list_repos`, `list_threads`, `get_thread(id)` (status, last summary, files changed,
  pending question), `read_thread(id, tail)`, `spawn_thread(repo, title, prompt, model?, base?)`,
  `message_thread(id, text)`, `interrupt_thread(id)`, `get_diff(id)`,
  `propose_merge(id, summary)` (renders a Merge card; only you can press it), `answer_question(id,
  answer)`.
- **Proactive updates:** when a worker finishes, fails, or asks a question, Go queues a synthetic
  `[thread update]` message. Updates are batched while the orchestrator is busy and delivered when
  it's idle, so it reports without being asked. These render as small status chips, not user bubbles.
- **To-do list:** `add_todo`, `update_todo`, `remove_todo`, `list_todos` (core/todos.go). Solo judges
  what's a todo; most have no thread. A todo linked to threads is ticked off when their work lands
  (a pull request opened or merged, a merge, or the thread marked done). Thread updates list the open
  todos so Solo can tick off the rest.
- **Sessions rotate; the chat doesn't:** once a turn ends with its context at 120k tokens (Settings:
  60k, 120k, 250k or never), or the user returns after 3 hours with 15k or more, Go starts Solo on a fresh Claude session (between turns
  only) rather than letting the SDK compact. The next message carries a recap built in Go: open
  threads, open todos, the last dozen messages. It goes as hidden context, not in the transcript, and
  is saved so a restart doesn't lose it. Cost runs on across sessions (`cost_base`).
- **Memory:** `remember(text, keywords, repo?, replaces?)`, `recall(queries[])`, `forget(id)` over a
  SQLite FTS5 table (BM25, Porter stemming; store/memory.go). Each memory is one fact of at most 400
  characters; replacing one keeps the old row, marked superseded. The newest memories, up to about
  1.5k tokens, load into the system prompt at session open, so most need no search. `recall` merges
  several phrasings by reciprocal rank across memories, Solo's own earlier messages and worker
  threads' title, task and summary, returning a few one-line results. It is capped at two calls a
  turn. Deferred: per-message auto-injection, model-distilled memories, embeddings, a memory UI.

## Worker threads

- Worktrees start from the latest default branch: Solo fetches `origin/<default>` (origin's HEAD, else
  main or master) and runs `git worktree add <data>/worktrees/<repo>/<title>-<id> -b <branch>
  origin/<default>`. Without a remote, the local main or master.
- Branch names: `solo/<title>-<id>`, unless Settings has naming rules (global, or per repository).
  Then a small model (`complete` RPC: one turn, no tools, no session) names it; an unusable answer
  falls back to the default name, and a taken name gets a numeric suffix.
- Settings (laid out as t3code's: a page list on the left, rows in cards, changes saved as they're made)
  has a page each for General, Repositories, Models (Solo and threads), Branches (naming rules and their
  model), Pull requests (text rules, its model, merging) and History. The worktree is removed after merge
  or discard; the branch is kept until merged.
- Session: `cwd` = worktree, `permissionMode: "bypassPermissions"`,
  `allowDangerouslySkipPermissions: true`, default model configurable.
- AskUserQuestion goes to Go → shows as a card in the rail and orchestrator chat. You or the
  orchestrator (`answer_question`) can reply.
- Status machine: `queued → starting → running ⇄ waiting → idle | failed | interrupted → merged |
  discarded | archived`, plus `running ⇄ background`. No concurrency cap. After each turn Solo counts
  the files the branch changes: an idle thread with changes is "Ready to merge"; one without shows
  "No changes".
- **Background work** (verified live, claude 2.1.321, SDK 0.3.319): a turn can end while a command run
  with `run_in_background` or a background subagent still runs. The CLI sends
  `background_tasks_changed` with every live background task (replace semantics; ambient tasks are
  left out), which the sidecar forwards as a `background` event. A turn that completes with any left
  ends `background`, not `idle`: shown at work (rail's Working group, an hourglass, "N in background"
  in Solo's bar, the pinned line says what and for how long), landing disabled in the header and the
  merge card alike, `propose_merge` refused, and the orchestrator told it isn't finished. When a task
  finishes by itself (`task_notification` completed or failed) the list empties and the CLI starts a
  turn of its own to read it, so the thread stays `background` until that turn ends (30 s fallback if
  none comes). A task stopped (`stopTask`, or Claude's TaskStop) wakes no turn: the thread settles at
  once. Stop stops background work too (`session.stopTask`); an effort change waits for it; the
  process exiting kills it (the thread is stopped), and a restart resumes the thread with a note
  naming what was stopped.
- **Restart:** like t3code, Solo carries on what a restart cut short. On load, a thread that was starting
  or running is marked interrupted and its unfinished steps end (tool calls without a result, running
  tasks); the commands and subagents among them are noted. `ResumeInterrupted`, called once the
  orchestrator is set up, resumes each session with a note that Solo restarted, what that stopped, and to
  check a step took effect before repeating it; a worker that never reached Claude starts over. A thread
  carried on 3 times within 5 minutes stays stopped (loop guard, logged in the `resume-log` setting), and
  Settings › General can turn this off. A thread waiting on a question stays waiting: the question is
  pending again, and its answer reaches the thread as a message. Threads the user stopped stay stopped.
- Threads stay in the rail until the user marks them **Done**, which is offered only when nothing is
  left to merge (merged, or no changes). Done sets `done_at`, removes a leftover worktree and branch,
  and moves the thread to Settings › History; discarding marks it done too. **Restore** recreates the
  worktree at the same path (on the branch if it survived, else a new one from the base branch's head),
  so the Claude session resumes.
- Pull requests (`internal/forge`): the forge comes from origin's host, or `git config solo.forge
  github|gitlab` for self-hosted ones. Open PR commits leftovers, pushes the branch and opens the PR
  against the base branch. The PR text model (Settings › Models; Haiku by default) writes the title and
  description from the commits, changed files and summary, following Settings › Git's rules; the
  description can be turned off. If it fails, the title is the worker's one commit (else the thread
  title) and the body its summary. Open PR is a split button whose menu opens a draft; a draft gets
  Ready for review, and auto-merge turns on only once it's ready.
  With Settings › Git › Auto-merge, Solo also runs `gh pr merge --auto` (`glab mr merge --auto-merge`).
  A watcher polls open PRs every 30 s: failed checks (sent to Solo to have the thread fix them, or just
  shown, per the setting), passed checks, closed, merged (the thread closes and its worktree goes).
  New commits a thread makes are pushed to its PR at the end of each turn.
- Multi-repository threads: one task across repositories (client, API, database) is one thread with
  a part per repository (`core.Part`: worktree, branch, base, PR; stored as JSON in `threads.parts`,
  the first mirrored into the single-repository columns). Every repository gets a worktree on the same
  branch in `<data>/worktrees/threads/<title>-<id>/<repo>/`, and the session runs in that folder.
  Changes and the diff prefix paths with the repository. Open PRs opens one per changed repository,
  each description listing the others; the split button's menu opens or merges one repository on its
  own. Auto-merge is each forge's own. The thread is merged once every changed repository has landed.
- Discarding a thread deletes its message images, except ones another thread's history still shows.

## UI (jamb)

- **Center:** orchestrator chat. Markdown via `jamb/markdown`; tool calls as collapsed chips;
  status chips; Merge and Question cards inline.
- **Right rail:** thread cards (title, repo, status dot, elapsed time, last activity line).
- **Slide-over:** a thread's transcript, a diff (`codeedit`, read-only), an input to message it
  directly, and Interrupt / Merge / Discard buttons.
- **Composer button** (as in t3code): send; while the thread works, a red stop button, which turns into
  Steer once there is a message. A message sent then steers the turn and is tagged "Steered".
- **Images:** paste, drop or attach images in any message box. They're copied into
  `<data>/attachments` (scaled to at most 2000 px and 3.5 MB) and sent as image blocks before the text.
  Messages to Solo also list the saved paths, so Solo can pass them on with `spawn_thread` or
  `message_thread`'s `images`.
- **Search:** the rail's filter matches thread titles and repositories. Ctrl+K (the title bar's
  Search) lists threads, including merged ones and History, and, from three letters, messages (yours,
  Solo's and the threads'), found with `core.SearchMessages`.
- **Model and effort per thread:** the thread's message box shows them; a new model applies at once,
  a new effort restarts the session (with resume) between turns.
- **Folders:** each thread's header opens its folder (the worktree, or the multi-repository folder) in
  an editor or a terminal (`internal/launch`: detected, or Settings' commands); a right click on a
  thread copies its paths and branch.
- **Notifications:** desktop notifications (jamb's `app.Notify`) while Solo isn't in front, when a
  thread asks, fails or finishes, a PR's checks fail or it merges, and when Solo replies. A click
  opens the thread. Solo is a single instance; on Windows its AppID is the installer's identity.
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
