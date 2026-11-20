import { describe, expect, test } from "bun:test";
import { secretGuard } from "../src/guard";
import { guardHook } from "../src/session";

describe("secretGuard", () => {
  test("refuses reading secret stores and process environments", () => {
    for (const command of [
      "secret-tool lookup service solo-secrets username X",
      "cat /proc/4242/environ | tr '\\0' '\\n'",
      "for p in /proc/*/environ; do strings $p; done",
      "security find-generic-password -s solo-secrets -w",
      "python3 -c 'import keyring; print(keyring.get_password(\"solo-secrets\", \"X\"))'",
      "gdbus call --session --dest org.freedesktop.secrets --object-path /org/freedesktop/secrets",
    ]) {
      expect(secretGuard("Bash", { command })).toContain("run_with_secrets");
    }
    expect(secretGuard("Read", { file_path: "/proc/123/environ" })).toBeDefined();
    expect(secretGuard("PowerShell", { command: "cmdkey /list" })).toBeDefined();
  });

  test("lets ordinary work through", () => {
    for (const command of ["git status", "bun test", "env | wc -l", "cat README.md", "ls /proc/cpuinfo"]) {
      expect(secretGuard("Bash", { command })).toBeUndefined();
    }
    expect(secretGuard("Read", { file_path: "/home/u/code/app/main.go" })).toBeUndefined();
    expect(secretGuard("Edit", { file_path: "/proc/1/environ" })).toBeUndefined(); // not a guarded tool
  });

  test("the hook denies with the reason", async () => {
    const out: any = await guardHook(
      { hook_event_name: "PreToolUse", tool_name: "Bash", tool_input: { command: "secret-tool search x y" } } as any,
      "t1",
      { signal: new AbortController().signal },
    );
    expect(out.hookSpecificOutput.permissionDecision).toBe("deny");
    const ok: any = await guardHook(
      { hook_event_name: "PreToolUse", tool_name: "Bash", tool_input: { command: "ls" } } as any,
      "t2",
      { signal: new AbortController().signal },
    );
    expect(ok).toEqual({});
  });
});
