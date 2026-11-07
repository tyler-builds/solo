// One Claude session: a single long-lived query() fed by a streaming prompt queue.
//
// Rules ported from t3code's ClaudeAdapterV2 (see docs/ARCHITECTURE.md, "Claude SDK correctness rules"):
// - one query per session; follow-up turns are offered into the same prompt stream
// - a new session passes `sessionId`, an existing one `resume`
// - turns are bound to client sends by the uuid the CLI echoes back (Normalizer)
// - "success" results can still be failures (Normalizer.turnStatus)
// - the CLI runs in its own process group, killed when it exits or when this sidecar dies
// - only stderr written before the first message is surfaced (startup failures)
// - canUseTool is always installed: the CLI forwards some requests even under bypassPermissions

import * as childProcess from "node:child_process";
import { appendFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import {
  createSdkMcpServer,
  query,
  tool,
  type CanUseTool,
  type Options,
  type PermissionResult,
  type Query,
  type SDKMessage,
  type SDKUserMessage,
  type SpawnOptions,
  type SpawnedProcess,
} from "@anthropic-ai/claude-agent-sdk";
import { fromJSONSchema, type ZodObject } from "zod";
import { Normalizer } from "./normalize";
import type { AgentEventBody, AskResult, OpenParams, Question, SendParams, ToolCallResult } from "./protocol";
import { errorText, log, type Peer } from "./rpc";

const STDERR_TAIL = 2048;
const EXIT_WAIT_MS = 10_000;

/** Built-in tools a read-only session may use without asking. */
const READ_TOOLS = new Set(["Read", "Grep", "Glob", "LS", "WebFetch", "WebSearch", "TodoWrite", "ToolSearch"]);

// A watcher shares the CLI's process group and holds fd 3, a pipe only this sidecar writes. When the
// sidecar exits (even by crashing) the pipe reaches EOF and the watcher stops the whole group, so MCP
// servers and shells the CLI started don't outlive us. From t3code's spawnClaudeProcessGroup.
const GROUP_WATCH_SCRIPT = [
  "{ trap '' TERM; read -r _ <&3; kill -TERM 0; sleep 5; kill -KILL 0; } </dev/null >/dev/null 2>&1 &",
  'exec "$@" 3<&-',
].join("\n");

export interface SessionHost {
  peer: Peer;
  emit(sessionId: string, body: AgentEventBody): void;
  logDir?: string;
}

/** An unbounded async queue that query() reads its prompt from. */
class PromptQueue implements AsyncIterable<SDKUserMessage> {
  private items: SDKUserMessage[] = [];
  private waiter: ((r: IteratorResult<SDKUserMessage>) => void) | null = null;
  private done = false;

  push(msg: SDKUserMessage) {
    if (this.done) throw new Error("session is closed");
    if (this.waiter) {
      const w = this.waiter;
      this.waiter = null;
      w({ value: msg, done: false });
    } else this.items.push(msg);
  }

  close() {
    this.done = true;
    if (this.waiter) {
      const w = this.waiter;
      this.waiter = null;
      w({ value: undefined, done: true });
    }
  }

  [Symbol.asyncIterator](): AsyncIterator<SDKUserMessage> {
    return {
      next: () => {
        const item = this.items.shift();
        if (item) return Promise.resolve({ value: item, done: false });
        if (this.done) return Promise.resolve({ value: undefined, done: true });
        return new Promise((resolve) => (this.waiter = resolve));
      },
      return: () => {
        this.close();
        return Promise.resolve({ value: undefined, done: true });
      },
    };
  }
}

interface Live {
  query: Query;
  prompt: PromptQueue;
  abort: AbortController;
  child?: SpawnedProcess & { pid?: number };
  exited: Promise<void>;
  /** Set once the CLI produced its first message; stderr after that stays private. */
  answered: boolean;
  stderrTail: string;
  closing: boolean;
  pump: Promise<void>;
}

export class Session {
  private live: Live | null = null;
  private normalizer = new Normalizer();
  private seq = 0;
  /** True once a transcript exists, so a reopen must resume rather than start fresh. */
  private hasTranscript: boolean;
  private opening: Promise<void> | null = null;

  constructor(
    private params: OpenParams,
    private host: SessionHost,
  ) {
    this.hasTranscript = params.resume;
  }

  get id() {
    return this.params.sessionId;
  }

  /** Starts the claude process. Resolves once query() is created; startup errors arrive as events. */
  async open() {
    if (this.live) return;
    if (this.opening) return this.opening;
    this.opening = this.start().finally(() => (this.opening = null));
    return this.opening;
  }

  async send(p: SendParams) {
    if (!this.live) await this.open();
    const msg: SDKUserMessage = {
      type: "user",
      message: { role: "user", content: p.text },
      parent_tool_use_id: null,
      uuid: p.uuid as SDKUserMessage["uuid"],
      ...(p.synthetic ? { client_composed: true as const } : {}),
    };
    this.live!.prompt.push(msg);
  }

  async interrupt() {
    if (!this.live) return { stillQueued: [] as string[] };
    const res = await this.live.query.interrupt();
    return { stillQueued: res?.still_queued ?? [] };
  }

  async setModel(model: string | undefined) {
    this.params = { ...this.params, ...(model ? { model } : {}) };
    if (this.live) await this.live.query.setModel(model);
  }

  /** Ends the session's process. The session can be reopened later with resume. */
  async close() {
    const live = this.live;
    if (!live) return;
    live.closing = true;
    live.prompt.close();
    try {
      live.query.close();
    } catch (e) {
      log(`session ${this.id}: close failed: ${errorText(e)}`);
    }
    await this.reap(live);
    await live.pump.catch(() => {});
  }

  private async start() {
    const p = this.params;
    const prompt = new PromptQueue();
    const abort = new AbortController();
    const live: Live = {
      query: undefined as unknown as Query,
      prompt,
      abort,
      exited: Promise.resolve(),
      answered: false,
      stderrTail: "",
      closing: false,
      pump: Promise.resolve(),
    };

    const options: Options = {
      cwd: p.cwd,
      ...(this.hasTranscript ? { resume: p.sessionId } : { sessionId: p.sessionId }),
      ...(p.model ? { model: p.model } : {}),
      ...(p.effort ? { effort: p.effort } : {}),
      ...(p.title ? { title: p.title } : {}),
      ...(p.additionalDirectories?.length ? { additionalDirectories: p.additionalDirectories } : {}),
      ...(p.tools ? { tools: p.tools } : {}),
      ...(p.disallowedTools?.length ? { disallowedTools: p.disallowedTools } : {}),
      ...(p.claudePath ? { pathToClaudeCodeExecutable: p.claudePath } : {}),
      env: { ...process.env, ...p.env },
      abortController: abort,
      permissionMode: p.permissionMode,
      ...(p.permissionMode === "bypassPermissions" ? { allowDangerouslySkipPermissions: true } : {}),
      canUseTool: this.canUseTool,
      includePartialMessages: true,
      thinking: { type: "adaptive", display: "summarized" } as Options["thinking"],
      settingSources: ["user", "project", "local"],
      systemPrompt: { type: "preset", preset: "claude_code", ...(p.systemAppend ? { append: p.systemAppend } : {}) },
      ...(p.mcpTools?.length ? { mcpServers: { [p.mcpServerName ?? "solo"]: this.mcpServer(p) } } : {}),
      spawnClaudeCodeProcess: (o) => this.spawn(o, live),
    };

    live.query = query({ prompt, options });
    this.live = live;
    live.pump = this.consume(live);
  }

  private async consume(live: Live) {
    let failure: string | undefined;
    try {
      for await (const msg of live.query) {
        if (!live.answered) {
          live.answered = true;
          this.hasTranscript = true;
        }
        this.record(msg);
        for (const body of this.normalizer.push(msg)) this.emit(body);
      }
    } catch (e) {
      if (!live.closing) {
        // Before the first message, the CLI explains a failed start only on stderr.
        const tail = live.answered ? "" : live.stderrTail.trim();
        failure = tail ? `${errorText(e)}\n${tail}` : errorText(e);
      }
    } finally {
      live.prompt.close();
      await this.reap(live);
      if (this.live === live) this.live = null;
      this.record({ type: "$exit", ...(failure === undefined ? {} : { error: failure }) });
      const closed = this.normalizer.abortTurn(failure ?? "Claude exited before the turn finished.");
      if (closed) this.emit(closed);
      this.emit(failure === undefined ? { kind: "exited" } : { kind: "exited", error: failure });
    }
  }

  /** Waits for the CLI to exit, killing its group if it lingers. */
  private async reap(live: Live) {
    const child = live.child;
    if (!child) return;
    const timeout = new Promise<"timeout">((r) => setTimeout(() => r("timeout"), EXIT_WAIT_MS).unref?.());
    if ((await Promise.race([live.exited.then(() => "exited" as const), timeout])) === "timeout") {
      killGroup(child, "SIGKILL");
      await live.exited;
    }
  }

  private spawn(o: SpawnOptions, live: Live): SpawnedProcess {
    const onStderr = (chunk: string) => {
      if (!live.answered) live.stderrTail = (live.stderrTail + chunk).slice(-STDERR_TAIL);
    };
    let child: childProcess.ChildProcess;
    if (process.platform === "win32") {
      child = childProcess.spawn(o.command, o.args, {
        cwd: o.cwd,
        env: o.env,
        signal: o.signal,
        windowsHide: true,
        stdio: ["pipe", "pipe", "pipe"],
      });
    } else {
      child = childProcess.spawn("/bin/sh", ["-c", GROUP_WATCH_SCRIPT, "solo-claude", o.command, ...o.args], {
        cwd: o.cwd,
        env: o.env,
        signal: o.signal,
        stdio: ["pipe", "pipe", "pipe", "pipe"],
        detached: true,
      });
    }
    // Always drained, so a chatty CLI never blocks on a full stderr pipe.
    child.stderr?.setEncoding("utf8").on("data", onStderr);
    live.exited = new Promise<void>((resolve) => {
      if (child.exitCode !== null || child.signalCode !== null) return resolve();
      child.once("exit", () => {
        killGroup(child, "SIGKILL");
        resolve();
      });
      child.once("error", () => resolve());
    });
    live.child = child as unknown as Live["child"];
    return child as unknown as SpawnedProcess;
  }

  private canUseTool: CanUseTool = async (toolName, input, opts) => {
    const toolUseID = opts.toolUseID;
    if (toolName === "AskUserQuestion") return this.ask(input, toolUseID, opts.signal);
    if (this.params.toolPolicy === "allow") return { behavior: "allow", updatedInput: input, toolUseID };

    // Read-only policy.
    if (READ_TOOLS.has(toolName)) return { behavior: "allow", updatedInput: input, toolUseID };
    if (toolName.startsWith("mcp__") && opts.mcpServer?.source === "sdk")
      return { behavior: "allow", updatedInput: input, toolUseID };
    if (toolName === "Bash" && typeof input.command === "string" && this.bashAllowed(input.command))
      return { behavior: "allow", updatedInput: input, toolUseID };
    return {
      behavior: "deny",
      message: `${toolName} is not available to this session: it only reads. Start a worker thread to make changes.`,
      toolUseID,
    } satisfies PermissionResult;
  };

  private bashAllowed(command: string): boolean {
    // Chaining or redirection could smuggle a write past an allowlisted prefix.
    if (/[;&|`$<>]|\n/.test(command)) return false;
    return (this.params.bashAllow ?? []).some((re) => new RegExp(`^(?:${re})$`).test(command.trim()));
  }

  private async ask(input: Record<string, unknown>, toolUseID: string | undefined, signal: AbortSignal) {
    const questions = normalizeQuestions(input.questions);
    try {
      const res = await this.host.peer.request<AskResult>(
        "question.ask",
        { sessionId: this.id, toolUseId: toolUseID ?? "", questions },
        signal,
      );
      if (res.cancelled || !res.answers) {
        return { behavior: "deny", message: "The user dismissed the question.", toolUseID } satisfies PermissionResult;
      }
      return {
        behavior: "allow",
        updatedInput: { questions: input.questions, answers: res.answers },
        toolUseID,
      } satisfies PermissionResult;
    } catch (e) {
      return { behavior: "deny", message: `The question could not be shown: ${errorText(e)}`, toolUseID } satisfies PermissionResult;
    }
  }

  private mcpServer(p: OpenParams) {
    const tools = (p.mcpTools ?? []).map((spec) => {
      const schema = fromJSONSchema(spec.inputSchema as any) as unknown as ZodObject<any>;
      return tool(
        spec.name,
        spec.description,
        schema.shape ?? {},
        async (args: Record<string, unknown>) => {
          try {
            const res = await this.host.peer.request<ToolCallResult>("tool.call", {
              sessionId: this.id,
              name: spec.name,
              args,
            });
            return { content: [{ type: "text" as const, text: res.text }], ...(res.isError ? { isError: true } : {}) };
          } catch (e) {
            return { content: [{ type: "text" as const, text: `Tool failed: ${errorText(e)}` }], isError: true };
          }
        },
        // Always loaded: these are the session's main tools, not worth a ToolSearch round trip.
        { alwaysLoad: true, ...(spec.readOnly ? { annotations: { readOnlyHint: true } } : {}) },
      );
    });
    return createSdkMcpServer({ name: p.mcpServerName ?? "solo", version: "1.0.0", tools });
  }

  private emit(body: AgentEventBody) {
    this.seq++;
    this.host.emit(this.id, { ...body, seq: this.seq } as AgentEventBody & { seq: number });
  }

  /** Raw messages, one per line, for debugging and replay fixtures. */
  private record(msg: SDKMessage | { type: "$exit"; error?: string }) {
    if (!this.host.logDir) return;
    try {
      mkdirSync(this.host.logDir, { recursive: true });
      appendFileSync(join(this.host.logDir, `${this.id}.jsonl`), JSON.stringify(msg) + "\n");
    } catch {}
  }
}

function killGroup(child: { pid?: number; kill(s: NodeJS.Signals): boolean }, signal: NodeJS.Signals) {
  try {
    if (process.platform !== "win32" && child.pid) process.kill(-child.pid, signal);
    else child.kill(signal);
  } catch {}
}

function normalizeQuestions(raw: unknown): Question[] {
  if (!Array.isArray(raw)) return [];
  return raw.flatMap((q: any) =>
    typeof q?.question === "string"
      ? [
          {
            question: q.question,
            header: String(q.header ?? ""),
            multiSelect: q.multiSelect === true,
            options: Array.isArray(q.options)
              ? q.options.map((o: any) => ({
                  label: String(o?.label ?? ""),
                  description: String(o?.description ?? ""),
                  ...(typeof o?.preview === "string" ? { preview: o.preview } : {}),
                }))
              : [],
          },
        ]
      : [],
  );
}
