// The secrets guard: an agent's own tool calls may not read Solo's secrets, from the keyring or from the
// environment of a command run_with_secrets is running. The hook asks Go (tool.guard), which knows
// which commands are running and decides (internal/secrets/guard.go); anything else goes through,
// including searches that only mention keyrings or /proc. It closes the obvious ways, not every way.

import type { HookCallback } from "@anthropic-ai/claude-agent-sdk";
import { errorText, log } from "./rpc";

/** The tools the guard looks at: the shells, and the tools that read files. */
export const GUARDED_TOOLS = "Bash|PowerShell|Read|Grep|Glob";

/** GuardCheck asks the app about a tool call: deny, when set, is why it may not run. */
export type GuardCheck = (toolName: string, input: Record<string, unknown>) => Promise<{ deny?: string }>;

/**
 * guardHook is a PreToolUse hook that denies the calls check refuses. A hook, not canUseTool: hooks run
 * under bypassPermissions too, and for subagents. If the app can't be asked, the call goes through.
 */
export function guardHook(check: GuardCheck): HookCallback {
  return async (input) => {
    if (input.hook_event_name !== "PreToolUse") return {};
    let deny: string | undefined;
    try {
      deny = (await check(input.tool_name, (input.tool_input ?? {}) as Record<string, unknown>)).deny;
    } catch (e) {
      log(`guard: ${errorText(e)}`);
      return {};
    }
    if (!deny) return {};
    return { hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: deny } };
  };
}
