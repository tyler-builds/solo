import { describe, expect, test } from "bun:test";
import { mkdtempSync, mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { claudeEnv, ENTRYPOINT, HOST_SESSION_VARIABLES } from "../src/launch";

// A home with no Claude settings, so the user's own don't reach the tests.
const emptyHome = () => mkdtempSync(join(tmpdir(), "solo-launch-"));

describe("claudeEnv", () => {
  test("sets Solo's entrypoint, so Claude Code doesn't take the session for an Agent SDK one", () => {
    const env = claudeEnv(undefined, { PATH: "/bin", HOME: "/h" }, "linux", emptyHome());
    expect(env.CLAUDE_CODE_ENTRYPOINT).toBe(ENTRYPOINT);
    expect(env.PATH).toBe("/bin");
  });

  test("drops a parent Claude Code session's variables and replaces its entrypoint", () => {
    const base: Record<string, string> = { PATH: "/bin", CLAUDE_CODE_ENTRYPOINT: "cli", CLAUDE_CONFIG_DIR: "/nowhere" };
    for (const name of HOST_SESSION_VARIABLES) base[name] = "parent";
    const env = claudeEnv(undefined, base, "linux", emptyHome());
    for (const name of HOST_SESSION_VARIABLES) expect(env[name]).toBeUndefined();
    expect(env.CLAUDE_CODE_ENTRYPOINT).toBe(ENTRYPOINT);
    expect(env.CLAUDE_CONFIG_DIR).toBe("/nowhere");
  });

  test("adds the session's own variables last", () => {
    const env = claudeEnv({ FOO: "bar", CLAUDE_EFFORT: "low" }, { CLAUDE_EFFORT: "high" }, "linux", emptyHome());
    expect(env.FOO).toBe("bar");
    expect(env.CLAUDE_EFFORT).toBe("low");
  });

  test("an entrypoint in the user's Claude settings wins, as Claude Code doesn't read it from there", () => {
    const home = emptyHome();
    mkdirSync(join(home, ".claude"));
    writeFileSync(
      join(home, ".claude", "settings.json"),
      `{\n  // set by the user\n  "env": { "CLAUDE_CODE_ENTRYPOINT": "mine", },\n}\n`,
    );
    expect(claudeEnv(undefined, {}, "linux", home).CLAUDE_CODE_ENTRYPOINT).toBe("mine");
  });

  test("reads the settings in CLAUDE_CONFIG_DIR when it is set", () => {
    const dir = emptyHome();
    writeFileSync(join(dir, "settings.json"), `{"env": {"CLAUDE_CODE_ENTRYPOINT": "custom-home"}}`);
    expect(claudeEnv(undefined, { CLAUDE_CONFIG_DIR: dir }, "linux", emptyHome()).CLAUDE_CODE_ENTRYPOINT).toBe("custom-home");
    expect(claudeEnv({ CLAUDE_CONFIG_DIR: dir }, {}, "linux", emptyHome()).CLAUDE_CODE_ENTRYPOINT).toBe("custom-home");
  });

  test("ignores settings it can't read, and an empty entrypoint", () => {
    const home = emptyHome();
    mkdirSync(join(home, ".claude"));
    writeFileSync(join(home, ".claude", "settings.json"), `{"env": {"CLAUDE_CODE_ENTRYPOINT": ""}}`);
    expect(claudeEnv(undefined, {}, "linux", home).CLAUDE_CODE_ENTRYPOINT).toBe(ENTRYPOINT);
    writeFileSync(join(home, ".claude", "settings.json"), `not json`);
    expect(claudeEnv(undefined, {}, "linux", home).CLAUDE_CODE_ENTRYPOINT).toBe(ENTRYPOINT);
  });
});
