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
| Landing | Solo proposes; **you** click Open PR (GitHub via `gh`, GitLab via `glab`), then Merge once checks pass, or the forge auto-merges. Repos without a remote merge locally ("Merge locally"): `git merge-tree` builds the merge without touching a checkout, so it works with uncommitted changes or another branch checked out; a checkout on the base branch then fast-forwards, keeping your edits. A conflict goes back to the thread to resolve ("Resolving conflicts"), and the branch merges when it finishes (at most twice). Work that lands outside these buttons counts too (`internal/core/landed.go`): a PR the thread opens itself (`gh pr create`/`glab mr create` in its Bash output, also found in the saved transcript after a restart) or one opened from a branch it pushed (`gh pr list --head`, only PRs opened since the thread started) is linked and followed like Solo's own; and a clean worktree whose changes are already in `main` or a freshly fetched `origin/main`, by ancestry or by content (`git merge-tree` of the branch into the base changes nothing, so squash merges, rebases and cherry-picks count), is marked merged, ready for Done. A thread started from an open PR's branch (`spawn_thread` with `base`) to add to it is the exception: once its work is in origin's copy of that branch, that PR becomes the thread's (found with `gh pr list --head <base>`; never for the default branch), its later commits are pushed there (no force), and it is merged when the PR merges. These checks run when a turn ends, on load, and every 5 minutes for waiting threads. |
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
   the sidecar marks that result `steered`, so the app keeps the turn going. Only a message to a
   working **worker** steers. The user's message to Solo while it works never reaches Claude mid-turn:
   Go queues it (see Orchestrator › Queued messages), because a message offered without `priority` can
   still be folded into the running turn, which ran two separate requests together.
8. **MCP registration** can time out. Retry once, and only for resumed opens.
9. **Probe** capabilities (models, account) with a query whose prompt never yields, then read
   `initializationResult()`, instead of hard-coding the model list.
10. **Process environment** (t3code's `mergeClaudeProcessEnvironment`, in `sidecar/src/launch.ts`).
   Every `claude` gets `CLAUDE_CODE_ENTRYPOINT=solo`, unless the user's or managed Claude settings
   set one in `env`, and none of the variables a parent Claude Code session exports (`CLAUDECODE`,
   `CLAUDE_CODE_SESSION_ID`, its messaging socket and token, …). Left unset, the SDK reports `sdk-ts`,
   and Claude Code then leaves out tools it keeps off in Agent SDK sessions, the Artifact tools among
   them. That is undocumented, so a session meant to have Artifact that starts without it logs a
   warning.

### Verified in phase 1 (live, claude 2.1.319, SDK 0.3.319)

- Turns bind by uuid: `result.user_message_uuids` and the first stamped frame carry the send's uuid.
- Interrupt ends the turn with `terminal_reason: aborted_streaming` → `interrupted`.
- A SIGKILLed CLI mid-turn: the stream just ends with no result. The sidecar closes the turn as
  `failed/process_exited`, emits `exited`, and the next send reopens with `resume`; context survives.
  The raw log gets a `{"type":"$exit"}` marker so replays can reproduce this.
- Under `bypassPermissions` the SDK warns that `canUseTool` is shadowed, but **AskUserQuestion is
  still routed to it**, so worker questions reach the app.
- In-process MCP tools are marked `alwaysLoad`; otherwise Claude spends a ToolSearch round trip first.
- The CLI also emits `command_lifecycle` (queued/started/completed per send uuid). Solo's queued
  messages don't use it: Go holds them itself until Solo's turn ends (Orchestrator › Queued messages).
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
- **MCP tools:** `list_repos`, `list_threads(include_closed?, limit?, days?, repo?)` (open threads,
  never capped; with `include_closed`, closed ones (merged, discarded, marked done) newest first,
  capped at the 20 newest from the last 30 days unless `limit` (max 100) or `days` asks for more, with a
  line counting those left out), `get_thread(id)` (status, last summary, files changed,
  pending question; for a done thread, what reopening it brings back), `read_thread(id, tail)`,
  `spawn_thread(repo, title, prompt, model?, base?, scratch?)`, `message_thread(id, text)`,
  `interrupt_thread(id)`, `get_diff(id)`, `propose_merge(id, summary)` (renders a Merge card in place of the
  thread's earlier one; only you can press it; a thread whose PR is open gets one even while the PR
  can't merge yet, and the card's button says why), `answer_question(id, answer)`, `reopen_thread(id)` (Restore from History when
  the user asks: its kept branch, or a new one from the base branch's head when its work landed, it
  had none, or it was discarded), `set_scratch(id, on)` (turns a thread's scratch mode on or
  off when the user asks; see Worker threads › Scratch threads).
- **Proactive updates:** when a worker finishes, fails, or asks a question, Go queues a synthetic
  `[thread update]` message. Updates are batched while the orchestrator is busy and delivered when
  it's idle, so it reports without being asked. Solo's chat shows neither these nor Solo's steps as
  rows: each exchange opens with a sentence or two that says what the threads reported and what Solo
  did ("A and B have questions. Solo messaged C."), with the word for what happened in its status's
  color and each thread's name a link to it, and a rule sets the exchanges apart (`gui/summary.go`).
  The sentences are built from fixed wording, not by a model.
- **The open thread:** each UI tells core which thread it has open beside Solo's chat
  (`api.Opener`; `core.SetViewing`): the desktop window whether or not it is in front, a paired
  device with its watch, until the watch lapses or the session ends. A turn that ends in an open thread
  (finished, waiting on background work, failed, asking, interrupted) starts no Solo turn, since the
  user read it there: it becomes a note for Solo's next message, one per thread. The exception is
  finished work that still needs a merge card (files changed, in git, not scratch, no card or open PR
  yet): Solo gets the update, marked as read by the user, and proposes the merge without recapping it.
  Pull request events, merges, discards and the merge after conflicts are resolved go to Solo as usual.
- **Queued messages:** the user's message to Solo while it works (a turn running, or a message just sent
  whose turn hasn't started) doesn't steer. `core.send` keeps it on the thread (`thread.queue`, shown as
  `ThreadView.Queued`, saved under the `state.queue.orchestrator` setting) instead of sending it, and so
  does a message sent while others wait, to keep their order. When Solo's turn ends, however it ends
  (Stop included: stopping moves on to what waits), `sendQueued` hands Solo the oldest as an ordinary
  message, which then joins the transcript; the end of that turn hands over the next, so each queued
  message is a turn of its own, in the order sent. A user message to Solo takes its hidden context (a
  recap after a fresh session, notes) when it is handed over (`SetSoloContext`), not when it queued. A
  restart keeps the queue; `ResumeInterrupted` sends it after Solo's own carrying on (or at once, if Solo
  wasn't carried on, unless it still waits on a question). A message that can't be delivered says so in
  the transcript, as any message does, and the next one is tried. While Solo waits on its own question
  (AskUserQuestion), a message isn't queued by Go: it goes to Claude, as before, and the question is
  answered in its card. Thread updates wait for queued messages, which go first. Sessions rotate only
  between turns, so a queued message goes to whichever session is current when it is handed over.
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
- Commit messages: threads write their own commits. Settings' commit rules, when set, go in every
  thread's instructions (its system prompt, so resumed sessions get the current ones) in place of the
  Conventional Commits default. Solo's own commits don't follow them: the fallback commit of what a
  thread left uncommitted (`chore: uncommitted work from <title>`, which the PR text skips), the
  local merge commit (`Merge <branch>: <title>`) and the first commit of a folder put in git. A squash
  merge on the forge takes its message from the PR title, which has its own rules.
- Settings (laid out as t3code's: a page list on the left, rows in cards, changes saved as they're made)
  has a page each for General, Repositories, Models (Solo and threads), Branches and commits (naming
  rules and their model, commit message rules), Pull requests (title and description rules, their
  model, merging) and History. The worktree is removed after merge
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
  ends `background`, not `idle`: shown at work (rail's Working group, an hourglass, the pinned line
  says what and for how long), landing disabled in the header and the
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
- Threads stay in the rail until the user marks them **Done**, offered for any thread that isn't working,
  landed or not. Done sets `done_at`, removes its worktree, and moves the thread to Settings › History;
  a thread whose work landed (or had none) loses its branch too. One with changes that haven't landed
  (`Unlanded`) asks first, and keeps its branch with its uncommitted edits committed, so nothing is
  lost; **Discard** is what deletes work. Discarding marks it done too. **Restore** recreates the
  worktree at the same path (on the branch if it survived, else a new one from the base branch's head),
  so the Claude session resumes.
- **Scratch threads** (`threads.scratch`, `core.Core.SetScratch`): a thread the user started only to
  try or test something (a benchmark, a spike) and doesn't mean to land. Solo starts one with
  `spawn_thread(..., scratch: true)` or turns the mode on or off with `set_scratch(id, on)`, only
  when the user says so; the user does it from the thread's More menu ("Mark as scratch", "Not
  scratch"), and a "scratch" label shows in its header, beside where "not in git" would. Nothing
  about landing asks for attention: `propose_merge` refuses it, turning the mode on deletes its merge
  card, it doesn't count toward "needs you" (rail, the bar's count, Solo's chat pill, next-thread),
  failed checks on a PR it already has raise no notification, and an idle one reads as Finished, as
  an in-place thread does. A question or a failure still needs the user. Thread updates, `get_thread`
  and `list_threads` tell Solo it is a scratch thread with nothing to land, and the user turning the
  mode on or off is noted for Solo's next message. It is not another way of working: it keeps its
  worktree and branch, its worker may commit, Done and Discard do what they do for any thread (Done
  keeps an unlanded branch, but doesn't ask first), and the header's Open PR / Merge stays, as a
  secondary button, so the user can still land it.
- Pull requests (`internal/forge`): the forge comes from origin's host, or `git config solo.forge
  github|gitlab` for self-hosted ones. Open PR commits leftovers, pushes the branch and opens the PR
  against the base branch. The PR text model (Settings › Models; Haiku by default) writes the title and
  description from the commits, changed files and summary, following Settings › Pull requests' title
  and description rules (one call); the description can be turned off. Settings › Pull requests also
  picks Claude Code skills (`prSkills`). With one chosen, the text comes instead from a one-off Claude
  Code session (`Core.runJob`: the sidecar's `session.open` with `ephemeral`, no transcript) in the
  branch's worktree, with the PR text model, the chosen skills only (`skills`), and the tools Skill,
  Bash, Read, Glob and Grep, so skills load natively and run their bundled scripts. It answers in the
  same TITLE:/DESCRIPTION: format within 5 minutes. Empty title/description rules add no defaults, so
  the skill alone sets the format. It is told not to change the worktree; if HEAD, the branch or
  `git status` differ after, Solo resets and cleans it back and posts a notice. A skill that is gone
  is skipped with a notice. If the model or agent fails, the title is the worker's one commit (else
  the thread title) and the body its summary. Open PR is a split button whose menu opens a draft; a draft gets
  Ready for review, and auto-merge turns on only once it's ready.
  With Settings › Git › Auto-merge, Solo also runs `gh pr merge --auto` (`glab mr merge --auto-merge`).
  A watcher polls open PRs every 30 s: failed checks (sent to Solo to have the thread fix them, or just
  shown, per the setting), passed checks, closed, merged (the thread closes and its worktree goes).
  New commits a thread makes are pushed to its PR at the end of each turn. A thread with a merge card
  then gets it posted anew, with the same summary, at the end of Solo's chat (the old one goes), so it
  isn't left far up the conversation. The card waits until Solo is between turns with no thread
  update on its way, which puts it after Solo's answer to the turn that made the commits. The card is
  also posted anew when checks that failed pass again. Nothing is re-posted when nothing was pushed,
  the thread has no card, none of its PRs is still open, or it is a scratch thread.
- **Merge pill** (`gui/mergepill.go`): a pill at the top right of Solo's chat (not a thread's) counts the
  merge cards still waiting on the user ("3 to land") with a dot per card in its checks' color, so they
  stay one glance away however far the chat has moved past them. A card waits while its thread hasn't
  merged, been discarded or been marked done (`cardLive`), newest first; with none, there is no pill.
  It covers nothing until clicked; then it lists the cards over the chat, a row each, with every action
  the card has: the landing button (the header's and the card's, from one description, `gui/land.go`'s
  `landSpec`, so it acts and shows busy the same way), the PR link(s) with their checks, Review changes,
  and "…" for Request changes, Show card in chat (scrolls to the card and its summary), Done and Discard.
  A narrow list gives each row's title a line of its own. Escape, a press outside and its close button
  close it. Since the pill keeps merge cards in view, the chat's "needs you" jump control counts only
  unanswered questions.
- Multi-repository threads: one task across repositories (client, API, database) is one thread with
  a part per repository (`core.Part`: worktree, branch, base, PR; stored as JSON in `threads.parts`,
  the first mirrored into the single-repository columns). Every repository gets a worktree on the same
  branch in `<data>/worktrees/threads/<title>-<id>/<repo>/`, and the session runs in that folder.
  Changes and the diff prefix paths with the repository. Open PRs opens one per changed repository,
  each description listing the others; the split button's menu opens or merges one repository on its
  own. Auto-merge is each forge's own. The thread is merged once every changed repository has landed.
- Discarding a thread deletes its message images (the user's and its agent's), except ones another
  thread's history still shows, such as a screenshot Solo showed the user again.
- **Screenshots of UI changes:** both prompts ask for them. A worker whose work changes what a user
  sees (in any repository) puts screenshots of each changed view in its summary, before and after (the
  base commit rendered from before its changes or a temporary checkout) or after alone for a new view,
  saved outside the repository; when it can't take them it says why. Solo asks for them when it gives
  a thread such a task, shows them when it reports, and sends the thread back when they're missing
  without a reason. Both are told how to embed an image (below).

## UI (jamb)

- **Center:** orchestrator chat. Markdown via `jamb/markdown`; tool calls as collapsed chips;
  status chips; Merge and Question cards inline.
- **Right rail:** thread cards (title, repo, status dot, elapsed time, last activity line).
- **Slide-over:** a thread's transcript, a diff (`codeedit`, read-only), an input to message it
  directly, and Interrupt / Merge / Discard buttons.
- **Composer button** (as in t3code): send; while the thread works, a red stop button, which turns into
  Steer once there is a message. A message sent then steers the turn and is tagged "Steered". Solo's
  composer has no Steer: while Solo works the button is Stop with an empty box and plain Send (Enter)
  once there is a message, whose tooltip says Solo reads it when it finishes. The message shows at the
  foot of Solo's chat, after its streaming text and working line, tagged "Queued", until Solo is handed
  it (Orchestrator › Queued messages). The web client draws the same views from the same `ThreadView`.
- **"/" and "@" in a message box** (`gui/mention.go`, after t3code's composer): "/" at the start of a
  word lists the skills and commands the conversation can run, as `core.Skills` finds them where Claude
  Code does (`~/.claude/skills` and `commands`, the organization's synced skills as
  `anthropic-skills:<name>`, plugins as `<plugin>:<name>` from `plugins/synced` and
  `installed_plugins.json`, and the project's `.claude/skills` and `commands`). "@" lists Solo's
  repositories and folders in Solo's conversation, and a thread's files and folders in a thread's
  (`core.SearchPaths`: `git ls-files`, so ignored files are left out; fuzzy, 50 at most). Picking types
  in `/name ` or `@path ` (quoted when it has a space); Claude Code runs the skill, and reads the file or
  lists the folder, itself. The list opens over the caret. jamb doesn't say where a text area's caret
  is, so the composer works it out from each edit, closes the list when a key or click moves the caret,
  and places the caret after what it types in with the field's Move to end action. Up and Down in an
  empty box bring back the messages sent there. A dropped file or folder that isn't an image is
  mentioned as `@path`.
- **Images:** paste, drop or attach images in any message box. They're copied into
  `<data>/attachments` (scaled to at most 2000 px and 3.5 MB) and sent as image blocks before the text.
  Messages to Solo also list the saved paths, so Solo can pass them on with `spawn_thread` or
  `message_thread`'s `images`.
- **Images in agents' messages:** an agent (a worker or Solo) shows an image by writing
  `![caption](path)`, with an absolute path or one relative to its working directory. Before the
  message is applied (`core.embedImages`), each local image it names is imported as the user's are
  (`ImportImage`: PNG, JPEG, GIF or WebP by content, at most 16 MB and 50 MP, scaled to the limits
  above, 20 a message), and the text is rewritten to point at the copy; the turn's result, which is the
  thread's summary, the same way. Web addresses, network paths, folders, pipes and files that aren't
  images stay as written. `TextData.Images` lists the attachments a message shows (`core.ImagesIn`),
  and the UI draws only those: a line of nothing but images becomes a row of previews with their
  captions (`agentText`), an image among text is drawn by the markdown view from a small preview, and
  a click opens either in the lightbox. Any other image a message names shows a placeholder, and
  nothing else is read. A screenshot outlives the worktree and its original file, and Solo reads a
  thread's (the paths are in `get_thread`, `read_thread` and the "finished its turn" update, listed
  after the text when clipping cuts them off) and shows it again with the same path.
- **Charts in agents' messages:** a fenced code block with the info string `chart` holds a small JSON
  spec (`internal/chartspec`: line, area, bar, scatter, pie or donut; a title, axis labels and series
  of `[x, y]` points), and `agentText` draws it where it stands with `jamb/charts`: hover shows
  values, the arrow keys walk the points, the legend toggles a series. Both prompts teach it
  (`core.ChartRules`). Nothing is stored beyond the message text, so charts replay as the text does,
  and the web client draws them with the same code. A block still open (streaming), nested in another,
  or whose spec is invalid, of an unknown kind or over the limits (64 KB, 12 series, 5,000 points,
  24 slices, 20 charts a message) shows as code. Excerpts (the rail, notifications) leave charts out.
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
- **First run** (`gui/onboarding.go`, `gui/onboarding_steps.go`, `gui/hints.go`): a new user gets a six-step
  setup in place of the main view. Welcome says what Solo is, with a picture of the loop drawn with the app's
  own pieces. Claude Code is `CheckClaude`, with what to run when it is missing or signed out, and the path
  setting. Models picks the model and effort for Solo and for threads, from the list Settings › Models
  offers (Claude Code's models, with its words for each; its family names on cards, every version in a
  menu). Repositories has the folder picker, `api.Setup.SuggestFolders`' usual project folders that hold
  repositories (offered, never added by themselves), and what the search found in each. Pull requests is
  `api.Setup.CheckForges` (`gh`/`glab` installed and signed in, per forge the repositories push to) and,
  where a forge is in play, the merge method, auto-merge and what happens when checks fail. Preferences has
  notifications, costs, and the editor and terminal a thread's folder opens in. The rest (branch, commit
  and PR text rules, PR skills, the helper models, scan depth, session size) stays in Settings. The setup
  has a larger scale than the app's views (`design.go`): its content grows with the window to 1,080 px,
  sits in the middle of the space between the steps and the buttons, and puts a step's two kinds of
  content side by side from 880 px. Every choice is saved as Settings saves it (`UpdateSettings`, in
  order; typed text once the typing stops), so Settings shows it, skipping keeps the defaults, and running
  the setup again shows the current values. A problem is checked again when the window comes back to the
  front. Where it stands is `Settings.Setup` (a step to resume at, or `done`); anyone with folders, threads
  or a conversation with Solo counts as set up, so updating shows nothing. Skip setup is always there, and
  Settings › General runs it again. After it, Solo's empty chat offers three tasks written about the
  user's repositories (a click fills the message box), and one-time hints, one at a time above the message
  box, explain the first thread, the first pinned question and the first merge card, and suggest pairing a
  phone once work has landed (`Settings.Hints`). A web page's backend isn't an `api.Setup`: it shows the
  starter tasks, not the setup or the hints.
- **Compact windows:** under 700 px wide, as on a phone, there is room for one conversation
  (`isCompact`, `compactCenter`): Solo's, or the open thread's. One bar tops the window (`compactBar`):
  the button that opens the drawer, with a count of the threads that need you, then Solo or the
  thread's title, and for a thread the button that closes it. The drawer (`threadsDrawer`) slides in
  from the left with Solo, search and Settings over the rail of todos and threads, and closes when
  you pick one. A thread puts its model under its message box, Settings lists its pages across the
  top, and diffs are unified. The desktop window can be made that narrow too.
- **Touch:** an entry point sets `App.Touch` where the UI is used with fingers (a web page whose
  pointer is coarse). The UI is then drawn 1.2 times larger, so text is about 16 px, Enter in a
  message makes a new line and the button sends, and the hints of keyboard shortcuts are left out.

## Web build

The UI also builds for a web page (`GOOS=js GOARCH=wasm`, jamb's web target), so that Solo can be used
from a browser, on a phone too. `internal/gui` reaches Solo only through `api.Backend` (`internal/api`,
which `gui.Backend` names); the desktop's backend, `gui.Live`, is in `backend_live.go`, which a page
leaves out.

A page shares the desktop's types, not its code. In `core`, `store`, `git`, `forge`, `agent` and
`launch`, the files that run git, Claude, the database or the user's apps are marked
`//go:build !js`, and the types a client sees are in files that build everywhere:

- `core/view.go` (threads, parts, todos, settings, changes) and `core/items.go` (the transcript)
- `forge/pr.go`, `git/change.go`, `store/thread.go`, `agent/protocol.go`
- `launch/launch_js.go`: a page opens no editor or terminal, so its calls answer with an error

Keep code that touches the machine out of those files. `TestBuildsForWeb` (`internal/gui`) builds the
UI for the web and fails when it reaches something a page does not have.

A page cannot open an editor or a terminal (`launch.Available` is false there), so the UI leaves those
buttons out.

`cmd/solo-web` is the web client's entry point, and `scripts/web.sh` builds it with TinyGo into
`dist/web` (see [REMOTE.md](REMOTE.md)). It runs `internal/webclient`, which pairs with a machine and
shows its UI. With `?sample=1` it runs over `internal/sample` instead: generated threads and transcripts
as large as a busy real database, one of them streaming, which is what the UI is measured against.

Attachments work without a file system: a file picked in a page is stored by its contents
(`SaveImageData`), and the UI reads every image through the backend (`ReadImage`), which reads only the
attachments folder. With `App.RemoteImages` the UI fetches them off the UI thread and asks for previews
scaled down.

What a remote device may change is checked at the machine. `api.SettingsPatch` is a change to the
settings field by field; its `CheckRemote` refuses the settings that name programs Solo runs (Claude's
path, the editor and terminal commands), which are set at the machine only. A thread runs one landing
action at a time (`core.ErrBusy`), so two devices can't both merge it.

`internal/remote/rpc` has the page's backend, `rpc.Client`, and the machine's side, `rpc.Host`
(see [REMOTE.md](REMOTE.md)). The UI tells such a backend which conversations are on screen
(`api.Watcher`), and loads fewer transcript items when `App.ItemLimit` says so.

## Repo layout

```
cmd/solo/            main
cmd/solo-web/        the web client's main, and its page (TinyGo; scripts/web.sh)
internal/api/        the interface between the UI and the engine; the settings patch
internal/remote/     remote access: secure (keys, handshakes), wire (records), rpc (requests and
                     state), host (the machine's side), device (a device's side), relaytest
internal/cloudflare/ deploys the relay and the web client to the user's Cloudflare account
internal/webclient/  the web client's shell: pairing and connecting, around the app's UI
internal/webhost/    the browser, as the web client reaches it through its host script
cmd/solo-localrelay/ the relay and the web client on this machine, for development
relay/               the relay: a Cloudflare Worker and a Durable Object (TypeScript)
e2e/                 remote access end to end in a real browser (scripts/e2e.sh)
internal/core/       orchestrator, threads, notifications
internal/store/      SQLite schema + queries
internal/git/        repo scan, worktrees, diff, merge
internal/agent/      sidecar supervisor + JSON-RPC client + event types
internal/gui/        jamb views; builds for the desktop and for a web page
sidecar/             TypeScript (Bun): rpc, session, normalize, tools; fixture tests
docs/
```

## Build phases

1. **SDK layer:** sidecar + protocol + a headless Go harness running one worker end to end.
   Replay-fixture tests for turn gating, result mapping, interrupt, crash/resume.
2. **Core:** store, repo index, worktrees, ThreadManager.
3. **Orchestrator:** session, MCP tools, proactive updates.
4. **UI:** jamb views.
5. **Packaging:** `scripts/appimage.sh` (Linux) and `scripts/windows.sh` (Windows, built from Linux in
   CI). The compiled sidecar ships beside the app; Claude Code is the user's own. On Windows the
   sidecar, `solo-agent.exe`, carries Solo's icon and the version info "Solo Agent" (product Solo):
   Bun's `--windows-*` options work only when compiling on Windows, so the script stamps Bun's Windows
   runtime with go-winres first and then compiles the sidecar into it with `--compile-executable-path`,
   which leaves the appended code untouched.

## Open defaults (change if you disagree)

- Orchestrator model: Opus; workers: Sonnet. Both configurable per thread.
- Auth: the user's existing Claude Code login or `ANTHROPIC_API_KEY`.
- App name: **Solo**.
