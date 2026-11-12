// Wire types between the Go app and this sidecar. Mirrored in internal/agent/protocol.go; keep both in sync.
//
// Transport: newline-delimited JSON over stdio. A request has an `id` and `method`; its response has the
// same `id` and either `result` or `error`. A notification has a `method` and no `id`. Both sides send
// requests: Go opens and drives sessions, and the sidecar asks Go to run orchestrator tools and answer
// AskUserQuestion.

export const PROTOCOL_VERSION = 1;

export type PermissionMode = "default" | "acceptEdits" | "bypassPermissions" | "plan" | "auto" | "dontAsk";
export type Effort = "low" | "medium" | "high" | "xhigh" | "max";

/** A tool the Go app implements and exposes to a session through an in-process MCP server. */
export interface ToolSpec {
  name: string;
  description: string;
  /** JSON Schema for the arguments; must be `type: "object"`. */
  inputSchema: Record<string, unknown>;
  readOnly?: boolean;
}

export interface OpenParams {
  sessionId: string;
  /** true: continue the saved transcript of sessionId. false: start a new session with that id. */
  resume: boolean;
  cwd: string;
  model?: string;
  effort?: Effort;
  title?: string;
  systemAppend?: string;
  additionalDirectories?: string[];
  permissionMode: PermissionMode;
  /** Restrict the built-in tool set (names from the claude_code preset). Omit for all. */
  tools?: string[];
  disallowedTools?: string[];
  /** Skills the session may use (names, or "plugin:skill"); omit for Claude Code's defaults. */
  skills?: string[];
  /** true: keep no transcript (a one-off job, never resumed). */
  ephemeral?: boolean;
  /** Tools served by Go under the MCP server name `mcpServerName` (default "solo"). */
  mcpTools?: ToolSpec[];
  mcpServerName?: string;
  /**
   * How canUseTool answers requests the CLI forwards (it forwards some even under bypass):
   * "allow" approves everything, "readonly" allows only read tools and the Bash commands matched
   * by `bashAllow`, and denies the rest.
   */
  toolPolicy: "allow" | "readonly";
  /** Regular expressions (JS syntax) a Bash command must fully match under the "readonly" policy. */
  bashAllow?: string[];
  claudePath?: string;
  env?: Record<string, string>;
}

export interface SendParams {
  sessionId: string;
  /** Client uuid for this user message; echoed back on the turn that answers it. */
  uuid: string;
  text: string;
  /** Images sent with the message, base64. */
  images?: { mediaType: "image/png" | "image/jpeg" | "image/gif" | "image/webp"; data: string }[];
  /** Text the app composed (status updates to the orchestrator), not typed by the user. */
  synthetic?: boolean;
  /**
   * Steer the running turn: Claude stops what it is doing (its running subagents move to the
   * background first) and reads this now. Without a running turn it is an ordinary send.
   */
  steer?: boolean;
}

export interface QuestionOption {
  label: string;
  description: string;
  preview?: string;
}

export interface Question {
  question: string;
  header: string;
  multiSelect: boolean;
  options: QuestionOption[];
}

/** Sidecar → Go request `question.ask`. */
export interface AskParams {
  sessionId: string;
  toolUseId: string;
  questions: Question[];
}

/** Answer keyed by question text. A cancelled ask has no answers. */
export interface AskResult {
  answers?: Record<string, string>;
  cancelled?: boolean;
  /** What the agent hears when cancelled; absent means the user dismissed it. */
  message?: string;
}

/** Sidecar → Go request `tool.call`. */
export interface ToolCallParams {
  sessionId: string;
  toolUseId?: string;
  name: string;
  args: Record<string, unknown>;
}

export interface ToolCallResult {
  text: string;
  isError?: boolean;
}

export type TurnStatus = "completed" | "interrupted" | "failed" | "cancelled";

export interface Usage {
  inputTokens: number;
  outputTokens: number;
  cacheReadTokens: number;
  cacheCreationTokens: number;
}

export type Block =
  | { type: "text"; text: string }
  | { type: "thinking"; text: string }
  | { type: "tool_use"; id: string; name: string; input: unknown };

/** Payload of the `event` notification. `seq` increases by one per session. */
export type AgentEvent = { sessionId: string; seq: number } & (
  | {
      kind: "init";
      model: string;
      cwd: string;
      tools: string[];
      claudeVersion: string;
      permissionMode: string;
      apiKeySource: string;
      mcpServers: { name: string; status: string }[];
    }
  /** A turn began. `uuids` are the client sends it answers; empty for a turn Claude started itself. */
  | { kind: "turn_start"; uuids: string[] }
  | { kind: "text_delta"; messageId: string; index: number; text: string; parentToolUseId: string | null }
  | { kind: "thinking_delta"; messageId: string; index: number; text: string; parentToolUseId: string | null }
  /** A complete assistant message. Supersedes the deltas with the same messageId and index. */
  | { kind: "assistant"; messageId: string; blocks: Block[]; parentToolUseId: string | null; error?: string }
  | { kind: "tool_result"; toolUseId: string; isError: boolean; text: string; parentToolUseId: string | null }
  | {
      kind: "task";
      phase: "started" | "progress" | "updated" | "ended";
      taskId: string;
      toolUseId?: string;
      taskType?: string;
      description?: string;
      status?: string;
      summary?: string;
      lastToolName?: string;
      totalTokens?: number;
      toolUses?: number;
      background?: boolean;
      ambient?: boolean;
    }
  /**
   * The work the session runs in the background (commands, subagents) after a change: every such task
   * still running, replacing the previous list. Housekeeping tasks (ambient) are left out. When a task
   * finishes by itself between turns, Claude starts a turn to read its result; one stopped (stopTask, or
   * by Claude) does not.
   */
  | { kind: "background"; tasks: BackgroundTask[] }
  | { kind: "compact"; trigger: "manual" | "auto"; preTokens: number; postTokens?: number }
  | { kind: "state"; state: "idle" | "running" | "requires_action" }
  | { kind: "api_retry"; attempt: number; maxRetries: number; delayMs: number; error: string }
  | {
      kind: "rate_limit";
      /** status, resetsAt, rateLimitType and utilization describe only the limit that binds right now. */
      status: string;
      resetsAt?: number;
      rateLimitType?: string;
      utilization?: number;
      /** Every subscription window Claude Code tracks (5-hour, weekly, …), whichever binds. */
      windows?: RateLimitWindow[];
    }
  | {
      kind: "turn_end";
      uuids: string[];
      status: TurnStatus;
      /** terminal_reason or error subtype, when the turn did not simply complete. */
      reason?: string;
      error?: string;
      result?: string;
      numTurns: number;
      durationMs: number;
      /** Running total for this query() (the SDK reports it cumulatively). */
      totalCostUsd: number;
      usage: Usage;
      /** Input-side tokens of the last API call: an estimate of the context in use. */
      contextTokens: number;
      queuedTurns: number;
      /** The turn only paused to take a steer: Claude carries on with it in the next turn_start. */
      steered?: boolean;
    }
  | { kind: "notice"; level: "info" | "warning" | "error"; text: string }
  /** The session's claude process is gone. A later send reopens it with resume. */
  | { kind: "exited"; error?: string }
);

/** A task running in the background, from background_tasks_changed. */
export interface BackgroundTask {
  taskId: string;
  /** local_bash, local_agent, … */
  taskType: string;
  description: string;
  /** The subagent task that started it, when not the main agent. */
  parentTaskId?: string;
}

/** One subscription rate-limit window, from rate_limit_info.unifiedWindows. */
export interface RateLimitWindow {
  rateLimitType: string;
  /** Fraction of the window used; can pass 1 when usage runs past the cap. */
  utilization: number;
  resetsAt?: number;
  status?: string;
}

export type AgentEventBody = AgentEvent extends infer E ? (E extends AgentEvent ? Omit<E, "sessionId" | "seq"> : never) : never;

export interface ProbeResult {
  claudeVersion: string;
  models: { value: string; displayName: string; description: string; effortLevels?: string[] }[];
  account: { email?: string; organization?: string; subscriptionType?: string; apiKeySource?: string };
}
