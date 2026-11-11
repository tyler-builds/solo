import { describe, expect, test } from "bun:test";
import { toolDenial } from "../src/session";

describe("toolDenial", () => {
  test("the allow policy (the orchestrator's and workers') runs any Bash command", () => {
    const p = { toolPolicy: "allow" as const };
    for (const command of ["cd /tmp && touch x", "git status", "make build | tee out.log", "echo $HOME > f"]) {
      expect(toolDenial(p, "Bash", { command })).toBeUndefined();
    }
    expect(toolDenial(p, "Write", { file_path: "/tmp/x" })).toBeUndefined();
  });

  test("the readonly policy allows read tools and allowlisted Bash only", () => {
    const p = { toolPolicy: "readonly" as const, bashAllow: [`git (status|log)( .*)?`] };
    expect(toolDenial(p, "Read", {})).toBeUndefined();
    expect(toolDenial(p, "mcp__solo__list_threads", {}, "sdk")).toBeUndefined();
    expect(toolDenial(p, "Bash", { command: "git log -3" })).toBeUndefined();
    expect(toolDenial(p, "Bash", { command: "cd /tmp && git status" })).toContain("only reads");
    expect(toolDenial(p, "Bash", { command: "rm -rf x" })).toContain("only reads");
    expect(toolDenial(p, "Edit", {})).toContain("only reads");
  });
});
