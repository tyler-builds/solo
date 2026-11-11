// Turns raw SDKMessages into the compact AgentEvents the Go app consumes. Pure apart from its own turn
// state, so it is tested against recorded and synthetic message streams (normalize.test.ts).

import type { SDKMessage, SDKResultMessage } from "@anthropic-ai/claude-agent-sdk";
import type { AgentEventBody, Block, RateLimitWindow, TurnStatus, Usage } from "./protocol";

const TOOL_RESULT_LIMIT = 16_000;

export class Normalizer {
  /** A turn is between its first root frame and its result. */
  private turnActive = false;
  /** The streaming message id per agent (root = ""), from message_start. */
  private streamMessage = new Map<string, string>();
  private initModel: string | null = null;
  private contextTokens = 0;

  get inTurn() {
    return this.turnActive;
  }

  /**
   * Call when the session's process is gone mid-turn: the turn will never get its result, so close it.
   * Returns the closing event, or null when no turn was open.
   */
  abortTurn(error: string): AgentEventBody | null {
    if (!this.turnActive) return null;
    this.turnActive = false;
    this.streamMessage.clear();
    return {
      kind: "turn_end",
      uuids: [],
      status: "failed",
      reason: "process_exited",
      error,
      numTurns: 0,
      durationMs: 0,
      totalCostUsd: 0,
      usage: zeroUsage(),
      contextTokens: this.contextTokens,
      queuedTurns: 0,
    };
  }

  push(msg: SDKMessage): AgentEventBody[] {
    const out: AgentEventBody[] = [];
    if (isTurnFrame(msg) && !this.turnActive) {
      this.turnActive = true;
      out.push({ kind: "turn_start", uuids: stampedUuids(msg) });
    }

    switch (msg.type) {
      case "system":
        this.system(msg, out);
        break;
      case "stream_event":
        this.stream(msg, out);
        break;
      case "assistant": {
        const parent = msg.parent_tool_use_id;
        const m = msg.message;
        if (parent === null && m.usage) this.contextTokens = inputSide(m.usage);
        const blocks: Block[] = [];
        for (const b of m.content as any[]) {
          if (b.type === "text") blocks.push({ type: "text", text: b.text });
          else if (b.type === "thinking") blocks.push({ type: "thinking", text: b.thinking ?? "" });
          else if (b.type === "tool_use" || b.type === "server_tool_use" || b.type === "mcp_tool_use")
            blocks.push({ type: "tool_use", id: b.id, name: b.name, input: b.input });
        }
        out.push({
          kind: "assistant",
          messageId: m.id,
          blocks,
          parentToolUseId: parent,
          ...(msg.error ? { error: msg.error } : {}),
        });
        break;
      }
      case "user": {
        if ((msg as any).isReplay) break;
        const content = msg.message.content;
        if (!Array.isArray(content)) break;
        for (const b of content as any[]) {
          if (b.type !== "tool_result") continue;
          out.push({
            kind: "tool_result",
            toolUseId: b.tool_use_id,
            isError: b.is_error === true,
            text: truncate(toolResultText(b.content)),
            parentToolUseId: msg.parent_tool_use_id,
          });
        }
        break;
      }
      case "result":
        out.push(this.result(msg));
        break;
      case "rate_limit_event": {
        const i = msg.rate_limit_info;
        out.push({
          kind: "rate_limit",
          status: i.status,
          ...(i.resetsAt === undefined ? {} : { resetsAt: i.resetsAt }),
          ...(i.rateLimitType === undefined ? {} : { rateLimitType: i.rateLimitType }),
          ...(i.utilization === undefined ? {} : { utilization: i.utilization }),
          ...rateLimitWindows(i),
        });
        break;
      }
    }
    return out;
  }

  private system(msg: Extract<SDKMessage, { type: "system" }>, out: AgentEventBody[]) {
    const m = msg as any;
    switch (m.subtype) {
      case "init":
        // The CLI repeats init at the start of every turn; forward the first and any model change.
        if (this.initModel === m.model) return;
        this.initModel = m.model;
        out.push({
          kind: "init",
          model: m.model,
          cwd: m.cwd,
          tools: m.tools ?? [],
          claudeVersion: m.claude_code_version ?? "",
          permissionMode: m.permissionMode ?? "",
          apiKeySource: m.apiKeySource ?? "",
          mcpServers: (m.mcp_servers ?? []).map((s: any) => ({ name: s.name, status: s.status })),
        });
        return;
      case "compact_boundary":
        out.push({
          kind: "compact",
          trigger: m.compact_metadata.trigger,
          preTokens: m.compact_metadata.pre_tokens,
          ...(m.compact_metadata.post_tokens === undefined ? {} : { postTokens: m.compact_metadata.post_tokens }),
        });
        return;
      case "session_state_changed":
        out.push({ kind: "state", state: m.state });
        return;
      case "api_retry":
        out.push({
          kind: "api_retry",
          attempt: m.attempt,
          maxRetries: m.max_retries,
          delayMs: m.retry_delay_ms,
          error: String(m.error ?? m.error_status ?? "unknown"),
        });
        return;
      case "task_started":
        out.push({
          kind: "task",
          phase: "started",
          taskId: m.task_id,
          ...opt("toolUseId", m.tool_use_id),
          ...opt("taskType", m.task_type ?? m.subagent_type),
          ...opt("description", m.description),
          ...opt("background", m.is_backgrounded),
          ...opt("ambient", m.ambient ?? m.skip_transcript),
        });
        return;
      case "task_progress":
        out.push({
          kind: "task",
          phase: "progress",
          taskId: m.task_id,
          ...opt("toolUseId", m.tool_use_id),
          ...opt("description", m.description),
          ...opt("summary", m.summary),
          ...opt("lastToolName", m.last_tool_name),
          ...opt("totalTokens", m.usage?.total_tokens),
          ...opt("toolUses", m.usage?.tool_uses),
        });
        return;
      case "task_updated":
        out.push({
          kind: "task",
          phase: "updated",
          taskId: m.task_id,
          ...opt("status", m.patch?.status),
          ...opt("description", m.patch?.description),
          ...opt("background", m.patch?.is_backgrounded),
        });
        return;
      case "task_notification":
        out.push({
          kind: "task",
          phase: "ended",
          taskId: m.task_id,
          ...opt("toolUseId", m.tool_use_id),
          status: m.status,
          ...opt("summary", m.summary),
          ...opt("totalTokens", m.usage?.total_tokens),
          ...opt("toolUses", m.usage?.tool_uses),
          ...opt("ambient", m.ambient ?? m.skip_transcript),
        });
        return;
    }
  }

  private stream(msg: Extract<SDKMessage, { type: "stream_event" }>, out: AgentEventBody[]) {
    const parent = msg.parent_tool_use_id;
    // Subagent text is not streamed to the app; its complete messages and task progress are enough.
    if (parent !== null) return;
    const ev = msg.event as any;
    const key = "";
    if (ev.type === "message_start") {
      this.streamMessage.set(key, ev.message.id);
      if (ev.message.usage) this.contextTokens = inputSide(ev.message.usage);
      return;
    }
    if (ev.type !== "content_block_delta") return;
    const messageId = this.streamMessage.get(key) ?? "";
    if (ev.delta.type === "text_delta") {
      out.push({ kind: "text_delta", messageId, index: ev.index, text: ev.delta.text, parentToolUseId: null });
    } else if (ev.delta.type === "thinking_delta") {
      out.push({ kind: "thinking_delta", messageId, index: ev.index, text: ev.delta.thinking, parentToolUseId: null });
    }
  }

  private result(msg: SDKResultMessage): AgentEventBody {
    this.turnActive = false;
    this.streamMessage.clear();
    const m = msg as any;
    const uuids: string[] = m.user_message_uuids ?? (m.user_message_uuid ? [m.user_message_uuid] : []);
    const status = turnStatus(msg);
    const reason = status === "completed" ? undefined : (msg.terminal_reason ?? msg.subtype);
    const error =
      status === "completed"
        ? undefined
        : msg.subtype === "success"
          ? msg.result || resultHint(msg)
          : msg.errors.join("\n") || resultHint(msg);
    const u = (msg.usage ?? {}) as any;
    return {
      kind: "turn_end",
      uuids,
      status,
      ...(reason === undefined ? {} : { reason }),
      ...(error ? { error } : {}),
      ...(msg.subtype === "success" && status === "completed" ? { result: msg.result } : {}),
      numTurns: msg.num_turns ?? 0,
      durationMs: msg.duration_ms ?? 0,
      totalCostUsd: msg.total_cost_usd ?? 0,
      usage: {
        inputTokens: u.input_tokens ?? 0,
        outputTokens: u.output_tokens ?? 0,
        cacheReadTokens: u.cache_read_input_tokens ?? 0,
        cacheCreationTokens: u.cache_creation_input_tokens ?? 0,
      },
      contextTokens: this.contextTokens,
      queuedTurns: m.queued_turn_count ?? 0,
    };
  }
}

/**
 * The status of a finished turn. Ported from t3code's terminalStatusFromResult: an abort reason wins over
 * a success envelope, and "success" can still be a failure (API errors, rate limits, context limits).
 */
export function turnStatus(msg: SDKResultMessage): TurnStatus {
  if (msg.terminal_reason === "aborted_tools" || msg.terminal_reason === "aborted_streaming") return "interrupted";
  if (msg.subtype === "success") {
    const failed =
      msg.api_error_status === 529 ||
      msg.api_error_status === 429 ||
      isFailureReason(msg.terminal_reason) ||
      msg.is_error;
    return failed ? "failed" : "completed";
  }
  const text = msg.errors.join("\n").toLowerCase();
  if (text.includes("interrupt")) return "interrupted";
  if (text.includes("cancel")) return "cancelled";
  return "failed";
}

const FAILURE_REASONS = new Set([
  "blocking_limit",
  "rapid_refill_breaker",
  "prompt_too_long",
  "image_error",
  "model_error",
  "api_error",
  "malformed_tool_use_exhausted",
  "budget_exhausted",
  "max_turns",
  "turn_setup_failed",
  "structured_output_retry_exhausted",
  "tool_deferred_unavailable",
]);

function isFailureReason(reason: string | undefined) {
  return reason !== undefined && FAILURE_REASONS.has(reason);
}

function resultHint(msg: SDKResultMessage): string | undefined {
  if (msg.subtype === "success" && msg.api_error_status === 429) return "Claude is rate limited. Try again later.";
  if (msg.subtype === "success" && msg.api_error_status === 529) return "Claude is overloaded. Try again shortly.";
  switch (msg.terminal_reason) {
    case "blocking_limit":
    case "prompt_too_long":
      return "The conversation is too long for the model's context window.";
    case "rapid_refill_breaker":
      return "Claude stopped: the context refilled too quickly after compaction.";
    case "image_error":
      return "An image in the conversation could not be processed.";
    case "model_error":
      return "The model returned an error.";
    case "max_turns":
      return "Claude reached the turn limit.";
    case "budget_exhausted":
      return "Claude reached the spending limit.";
  }
  return undefined;
}

/** Frames that belong to a turn, as opposed to session housekeeping (init, state, tasks). */
function isTurnFrame(msg: SDKMessage): boolean {
  if (msg.type === "assistant") return true;
  if (msg.type === "stream_event") return (msg.event as any).type !== "ping";
  if (msg.type === "user") return !(msg as any).isReplay && msg.parent_tool_use_id === null;
  if (msg.type === "result") return true;
  return false;
}

function stampedUuids(msg: SDKMessage): string[] {
  const m = msg as any;
  if (Array.isArray(m.user_message_uuids)) return m.user_message_uuids;
  if (typeof m.user_message_uuid === "string") return [m.user_message_uuid];
  // The CLI echoes the user message itself; its uuid is the client's.
  if (msg.type === "user" && typeof m.uuid === "string" && !Array.isArray(msg.message.content)) return [m.uuid];
  return [];
}

function inputSide(u: any): number {
  return (u.input_tokens ?? 0) + (u.cache_read_input_tokens ?? 0) + (u.cache_creation_input_tokens ?? 0);
}

function toolResultText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((b: any) => (b.type === "text" ? b.text : b.type === "image" ? "[image]" : ""))
    .filter(Boolean)
    .join("\n");
}

function truncate(s: string): string {
  return s.length <= TOOL_RESULT_LIMIT ? s : s.slice(0, TOOL_RESULT_LIMIT) + `\n… (${s.length - TOOL_RESULT_LIMIT} more characters)`;
}

function opt<K extends string, V>(key: K, value: V | undefined | null): { [P in K]?: V } {
  return value === undefined || value === null ? {} : ({ [key]: value } as { [P in K]?: V });
}

function zeroUsage(): Usage {
  return { inputTokens: 0, outputTokens: 0, cacheReadTokens: 0, cacheCreationTokens: 0 };
}

/**
 * The per-window usage of a rate_limit_info. Its top-level fields describe only the window that binds
 * now (the weekly one, say, while it is the closer to its cap), so the 5-hour window would never show;
 * Claude Code also reports every window it tracks in `unifiedWindows` (an internal field, absent for
 * API-key sessions). Windows that don't parse are skipped.
 */
export function rateLimitWindows(info: unknown): { windows?: RateLimitWindow[] } {
  const w = (info as { unifiedWindows?: unknown } | undefined)?.unifiedWindows;
  if (!w || typeof w !== "object") return {};
  const windows: RateLimitWindow[] = [];
  for (const [rateLimitType, v] of Object.entries(w as Record<string, any>)) {
    if (!v || typeof v !== "object" || typeof v.utilization !== "number") continue;
    windows.push({
      rateLimitType,
      utilization: v.utilization,
      ...(typeof v.resetsAt === "number" ? { resetsAt: v.resetsAt } : {}),
      ...(typeof v.status === "string" ? { status: v.status } : {}),
    });
  }
  return windows.length ? { windows } : {};
}
