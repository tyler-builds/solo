import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { SDKMessage, SDKResultMessage } from "@anthropic-ai/claude-agent-sdk";
import { backgroundTasks, Normalizer, rateLimitWindows, turnStatus } from "../src/normalize";
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

// A recorded orchestrator turn (claude 2.1.321, opus, summarized thinking), content replaced with
// synthetic text: a Read call, then a thought, then the reply. As Claude sends it, the thought's summary
// arrives in one burst just before the finished thinking block. The app's core replays the events this
// produces (orchestrator-thinking.events.jsonl, in internal/core's tests), so the two must stay in step.
describe("recorded orchestrator turn with thinking", () => {
  const events = run(fixture("orchestrator-thinking.jsonl"));

  test("matches the events the core replays", () => {
    const want = readFileSync(join(import.meta.dir, "fixtures", "orchestrator-thinking.events.jsonl"), "utf8")
      .split("\n")
      .filter(Boolean)
      .map((l) => {
        const { sessionId: _, ...e } = JSON.parse(l);
        return e;
      });
    expect(JSON.parse(JSON.stringify(events))).toEqual(want);
  });

  test("streams the thought's text before its finished block", () => {
    const deltas = events.filter((e) => e.kind === "thinking_delta") as Extract<AgentEventBody, { kind: "thinking_delta" }>[];
    const done = events.find((e) => e.kind === "assistant" && e.blocks.some((b) => b.type === "thinking")) as Extract<
      AgentEventBody,
      { kind: "assistant" }
    >;
    expect(deltas.length).toBeGreaterThan(1);
    expect(deltas.map((d) => d.text).join("")).toBe((done.blocks[0] as { text: string }).text);
    expect(new Set(deltas.map((d) => d.messageId))).toEqual(new Set([done.messageId]));
    expect(events.indexOf(deltas[deltas.length - 1])).toBeLessThan(events.indexOf(done));
  });
});

describe("rate_limit_event", () => {
  // As claude 2.1.321 sends it: the top level names only the binding (weekly) limit; the 5-hour window
  // is only in unifiedWindows.
  const msg = {
    type: "rate_limit_event",
    rate_limit_info: {
      status: "allowed_warning",
      resetsAt: 1794448800,
      rateLimitType: "seven_day",
      utilization: 0.76,
      overageStatus: "rejected",
      surpassedThreshold: 0.75,
      unifiedWindows: {
        five_hour: { utilization: 0.67, resetsAt: 1794387600, status: "allowed" },
        seven_day: { utilization: 0.76, resetsAt: 1794448800, status: "allowed_warning", surpassedThreshold: 0.75 },
      },
    },
    uuid: "u",
    session_id: "s",
  } as unknown as SDKMessage;

  test("carries every window, not just the binding one", () => {
    expect(new Normalizer().push(msg)).toEqual([
      {
        kind: "rate_limit",
        status: "allowed_warning",
        resetsAt: 1794448800,
        rateLimitType: "seven_day",
        utilization: 0.76,
        windows: [
          { rateLimitType: "five_hour", utilization: 0.67, resetsAt: 1794387600, status: "allowed" },
          { rateLimitType: "seven_day", utilization: 0.76, resetsAt: 1794448800, status: "allowed_warning" },
        ],
      },
    ]);
  });

  test("skips windows it can't read, and is absent without any", () => {
    expect(rateLimitWindows({ status: "allowed" })).toEqual({});
    expect(rateLimitWindows({ unifiedWindows: { five_hour: { resetsAt: 1 }, x: null } })).toEqual({});
    expect(rateLimitWindows({ unifiedWindows: { seven_day: { utilization: 0.1 } } })).toEqual({
      windows: [{ rateLimitType: "seven_day", utilization: 0.1 }],
    });
  });
});

// Recorded with claude 2.1.321 (haiku): a Bash command run with run_in_background. In the first, the
// turn ends while it runs, the command finishes by itself, and Claude starts a turn to read its result.
// In the second, Claude stops it (TaskStop) in a later turn, so no turn follows.
describe("recorded background work", () => {
  const backgrounds = (events: AgentEventBody[]) =>
    events.flatMap((e) => (e.kind === "background" ? [e.tasks.map((t) => t.description)] : []));

  test("a command that finishes wakes Claude for another turn", () => {
    const events = run(fixture("background-complete.jsonl"));
    expect(backgrounds(events)).toEqual([["Sleep then echo"], []]);
    const seq = kinds(events).filter((k) => ["turn_start", "turn_end", "background", "task"].includes(k));
    // started → turn ends while it runs → it ends → the list empties → Claude's own turn reads it.
    expect(seq).toEqual(["turn_start", "background", "task", "turn_end", "task", "task", "background", "turn_start", "turn_end"]);
    const started = events.find((e) => e.kind === "task" && e.phase === "started");
    expect(started).toMatchObject({ taskType: "local_bash", background: true, description: "Sleep then echo" });
    const ended = events.find((e) => e.kind === "task" && e.phase === "ended");
    expect(ended).toMatchObject({ status: "completed" });
    const wake = events.filter((e) => e.kind === "turn_start")[1];
    expect(wake).toMatchObject({ uuids: [] });
  });

  test("a command Claude stops ends inside its turn, with status stopped", () => {
    const events = run(fixture("background-kill.jsonl"));
    expect(backgrounds(events)).toEqual([["Long sleep"], []]);
    const ends = events.flatMap((e, i) => (e.kind === "turn_end" ? [i] : []));
    const emptied = events.findIndex((e) => e.kind === "background" && e.tasks.length === 0);
    expect(ends.length).toBe(2);
    expect(emptied).toBeGreaterThan(ends[0]);
    expect(emptied).toBeLessThan(ends[1]);
    expect(events.find((e) => e.kind === "task" && e.phase === "ended")).toMatchObject({ status: "stopped" });
  });

  test("backgroundTasks leaves out housekeeping and malformed entries", () => {
    expect(
      backgroundTasks([
        { task_id: "b1", task_type: "local_bash", description: "Run tests" },
        { task_id: "w1", task_type: "monitor", description: "watch", ambient: true },
        { task_id: "a1", task_type: "local_agent", description: "Review", parent_task_id: "a0" },
        { description: "no id" },
        null,
      ]),
    ).toEqual([
      { taskId: "b1", taskType: "local_bash", description: "Run tests" },
      { taskId: "a1", taskType: "local_agent", description: "Review", parentTaskId: "a0" },
    ]);
    expect(backgroundTasks(undefined)).toEqual([]);
  });
});
