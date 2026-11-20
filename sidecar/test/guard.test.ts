import { describe, expect, test } from "bun:test";
import { guardHook } from "../src/guard";

const opts = { signal: new AbortController().signal };
const pre = (tool_name: string, tool_input: unknown) => ({ hook_event_name: "PreToolUse", tool_name, tool_input }) as any;

describe("guardHook", () => {
  test("asks the app, and denies with its reason", async () => {
    const asked: unknown[] = [];
    const hook = guardHook(async (tool, input) => {
      asked.push([tool, input]);
      return { deny: "it reads Solo's secrets" };
    });
    const out: any = await hook(pre("Bash", { command: "secret-tool lookup service solo-secrets username X" }), "t1", opts);
    expect(out.hookSpecificOutput).toEqual({
      hookEventName: "PreToolUse",
      permissionDecision: "deny",
      permissionDecisionReason: "it reads Solo's secrets",
    });
    expect(asked).toEqual([["Bash", { command: "secret-tool lookup service solo-secrets username X" }]]);
  });

  test("lets the call through when the app allows it or can't be asked", async () => {
    expect(await guardHook(async () => ({}))(pre("Grep", { pattern: "solo-secrets" }), "t2", opts)).toEqual({});
    const failing = guardHook(async () => {
      throw new Error("no app");
    });
    expect(await failing(pre("Bash", { command: "ls" }), "t3", opts)).toEqual({});
  });
});
