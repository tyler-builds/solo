import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { SDKMessage, SDKResultMessage } from "@anthropic-ai/claude-agent-sdk";
import { Normalizer, turnStatus } from "../src/normalize";
import type { AgentEventBody } from "../src/protocol";

// A recorded worker session (claude 2.1.x, haiku): a plain turn, an interrupted turn, a turn killed by
// SIGKILL (no result: the stream just ends), a resumed process answering from memory, an explicit
// resume, and an AskUserQuestion round trip. Recorded by scratch drive scripts; paths sanitized.
function fixture(name: string): SDKMessage[] {
  return readFileSync(join(import.meta.dir, "fixtures", name), "utf8")
    .split("\n")
    .filter(Boolean)
    .map((l) => JSON.parse(l));
}

/** Replays like Session.consume: a `$exit` marker is where the process died and its turn was closed. */
function run(messages: SDKMessage[]) {
  const n = new Normalizer();
  return messages.flatMap((m): AgentEventBody[] => {
    if ((m as any).type === "$exit") {
      const closed = n.abortTurn((m as any).error ?? "exited");
      return closed ? [closed] : [];
    }
    return n.push(m);
  });
}

const kinds = (events: AgentEventBody[]) => events.map((e) => e.kind);

describe("recorded worker session", () => {
  const events = run(fixture("worker-session.jsonl"));
  const turns = events.filter((e) => e.kind === "turn_start" || e.kind === "turn_end");

  test("turn_start and turn_end alternate", () => {
    for (let i = 0; i < turns.length; i++) expect(turns[i].kind).toBe(i % 2 === 0 ? "turn_start" : "turn_end");
  });

  test("every turn is bound to the uuid of the send it answers", () => {
    for (let i = 0; i + 1 < turns.length; i += 2) {
      const start = turns[i] as Extract<AgentEventBody, { kind: "turn_start" }>;
      const end = turns[i + 1] as Extract<AgentEventBody, { kind: "turn_end" }>;
      expect(start.uuids.length).toBeGreaterThan(0);
      // A turn closed by a dead process has no result to echo its uuids.
      if (end.reason === "process_exited") continue;
      expect(end.uuids).toEqual(start.uuids);
    }
  });

  test("statuses match what happened", () => {
    const statuses = turns.filter((e) => e.kind === "turn_end").map((e) => (e as any).status);
    expect(statuses).toContain("interrupted");
    expect(statuses).toContain("failed"); // the SIGKILLed turn
    expect(statuses.filter((s) => s === "completed").length).toBeGreaterThanOrEqual(4);
  });

  test("init is forwarded once per process, not once per turn", () => {
    const inits = events.filter((e) => e.kind === "init");
    expect(inits.length).toBeGreaterThanOrEqual(1);
    expect(inits.length).toBeLessThan(turns.length / 2);
  });

  test("deltas carry the message id of their stream", () => {
    const deltas = events.filter((e) => e.kind === "text_delta") as Extract<AgentEventBody, { kind: "text_delta" }>[];
    expect(deltas.length).toBeGreaterThan(0);
    const ids = new Set(events.filter((e) => e.kind === "assistant").map((e) => (e as any).messageId));
    for (const d of deltas) expect(ids.has(d.messageId)).toBe(true);
  });

  test("the AskUserQuestion answer comes back as a tool result", () => {
    const ask = events.find(
      (e) => e.kind === "assistant" && e.blocks.some((b) => b.type === "tool_use" && b.name === "AskUserQuestion"),
    ) as Extract<AgentEventBody, { kind: "assistant" }>;
    const id = (ask.blocks.find((b) => b.type === "tool_use") as any).id;
    const result = events.find((e) => e.kind === "tool_result" && e.toolUseId === id) as any;
    expect(result.isError).toBe(false);
  });
});

describe("abortTurn", () => {
  test("closes an open turn when the process dies", () => {
    const n = new Normalizer();
    n.push(assistant("hi"));
    expect(n.inTurn).toBe(true);
    const ev = n.abortTurn("killed");
    expect(ev?.kind).toBe("turn_end");
    expect((ev as any).status).toBe("failed");
    expect(n.inTurn).toBe(false);
    expect(n.abortTurn("again")).toBeNull();
  });
});

describe("turns Claude starts itself", () => {
  test("a turn with no stamped uuid starts with empty uuids", () => {
    const events = run([assistant("background task finished")]);
    expect(events[0]).toEqual({ kind: "turn_start", uuids: [] });
  });

  test("housekeeping frames do not open a turn", () => {
    const events = run([{ type: "system", subtype: "session_state_changed", state: "idle", uuid: "u", session_id: "s" } as any]);
    expect(kinds(events)).toEqual(["state"]);
  });
});

describe("turnStatus", () => {
  const base = {
    type: "result",
    duration_ms: 1,
    duration_api_ms: 1,
    num_turns: 1,
    total_cost_usd: 0,
    usage: {},
    modelUsage: {},
    permission_denials: [],
    uuid: "u",
    session_id: "s",
  };
  const success = (extra: object) =>
    ({ ...base, subtype: "success", is_error: false, result: "", stop_reason: null, ...extra }) as unknown as SDKResultMessage;
  const error = (errors: string[], extra: object = {}) =>
    ({ ...base, subtype: "error_during_execution", is_error: true, errors, ...extra }) as unknown as SDKResultMessage;

  test("plain success completes", () => expect(turnStatus(success({}))).toBe("completed"));
  test("success with is_error fails", () => expect(turnStatus(success({ is_error: true }))).toBe("failed"));
  test("overloaded success fails", () => expect(turnStatus(success({ api_error_status: 529 }))).toBe("failed"));
  test("rate-limited success fails", () => expect(turnStatus(success({ api_error_status: 429 }))).toBe("failed"));
  test("context exhaustion fails", () => expect(turnStatus(success({ terminal_reason: "prompt_too_long" }))).toBe("failed"));
  test("abort reason beats a success envelope", () =>
    expect(turnStatus(success({ terminal_reason: "aborted_tools" }))).toBe("interrupted"));
  test("interrupt error text", () => expect(turnStatus(error(["Request interrupted by user"]))).toBe("interrupted"));
  test("cancel error text", () => expect(turnStatus(error(["cancelled"]))).toBe("cancelled"));
  test("other errors fail", () => expect(turnStatus(error(["boom"]))).toBe("failed"));
});

function assistant(text: string): SDKMessage {
  return {
    type: "assistant",
    message: { id: "m1", content: [{ type: "text", text }], usage: { input_tokens: 1 } } as any,
    parent_tool_use_id: null,
    uuid: "a" as any,
    session_id: "s",
  } as SDKMessage;
}
