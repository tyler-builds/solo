// How Solo starts every claude process: which binary, and with what environment. The environment
// follows t3code's mergeClaudeProcessEnvironment (apps/server/src/provider/Drivers/ClaudeHome.ts).

import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join, resolve } from "node:path";

/**
 * What Solo's claude processes report as their entrypoint. Left unset, the SDK reports "sdk-ts", and
 * Claude Code then keeps off tools it doesn't offer Agent SDK sessions by default, among them the
 * Artifact tools. Undocumented: the sidecar warns when a session starts without Artifact.
 */
export const ENTRYPOINT = "solo";

// A Claude Code session that started Solo (from its terminal, say) exports these. A claude spawned
// with them acts as part of that session: its session id, messaging socket and token, and effort.
export const HOST_SESSION_VARIABLES = [
  "CLAUDECODE",
  "CLAUDE_CODE_SESSION_ID",
  "CLAUDE_CODE_HOST_SESSION_ID",
  "CLAUDE_CODE_CHILD_SESSION",
  "CLAUDE_CODE_SESSION_ATTENDED",
  "CLAUDE_CODE_MESSAGING_SOCKET",
  "CLAUDE_CODE_MESSAGING_TOKEN",
  "CLAUDE_CODE_EXECPATH",
  "CLAUDE_CODE_DESKTOP_APP_VERSION",
  "CLAUDE_EFFORT",
  "CLAUDE_PID",
  "CLAUDE_AGENT_SDK_VERSION",
];

export type Env = Record<string, string | undefined>;

/**
 * claudeProcess is the binary and environment options for every query() Solo makes, so that no
 * claude process misses the environment. `env` is the session's own additions (OpenParams.env).
 */
export function claudeProcess(path?: string, env?: Record<string, string>): { pathToClaudeCodeExecutable?: string; env: Env } {
  return { ...claudeExecutable(path), env: claudeEnv(env) };
}

// claudeExecutable is the SDK option naming the claude binary. A compiled sidecar can't fall back to
// the SDK's bundled CLI, so it needs a path from Go.
function claudeExecutable(path?: string): { pathToClaudeCodeExecutable?: string } {
  if (path) return { pathToClaudeCodeExecutable: path };
  if (/\$bunfs|~BUN/.test(import.meta.path)) {
    throw new Error("Claude Code not found. Install it, or set its path in Settings.");
  }
  return {};
}

/**
 * claudeEnv is the environment of a claude process: the sidecar's own, with Solo's entrypoint and
 * without a parent Claude Code session's variables, then `extra`. Claude Code reads its entrypoint
 * from the process environment and not from its settings' `env`, so an entrypoint set in the user or
 * managed settings is applied here, as t3code does.
 */
export function claudeEnv(extra?: Record<string, string>, base: Env = process.env, platform = process.platform, home = homedir()): Env {
  const env: Env = { ...base, CLAUDE_CODE_ENTRYPOINT: ENTRYPOINT };
  for (const name of HOST_SESSION_VARIABLES) delete env[name];
  Object.assign(env, extra);
  const fromSettings = settingsEntrypoint(env, platform, home);
  return fromSettings ? { ...env, CLAUDE_CODE_ENTRYPOINT: fromSettings } : env;
}

/** The CLAUDE_CODE_ENTRYPOINT that Claude's user and managed settings set in `env`; later files win. */
export function settingsEntrypoint(env: Env, platform: NodeJS.Platform, home: string): string | undefined {
  let entrypoint: string | undefined;
  for (const file of settingsFiles(env, platform, home)) {
    let text: string;
    try {
      text = readFileSync(file, "utf8");
    } catch {
      continue;
    }
    const value = lenientJson(text)?.env?.CLAUDE_CODE_ENTRYPOINT;
    if (typeof value === "string") entrypoint = value;
  }
  return entrypoint;
}

// Claude Code's user settings, then its managed settings (t3code's claudeSettingsPaths without a cwd).
function settingsFiles(env: Env, platform: NodeJS.Platform, home: string): string[] {
  const configDir = envValue(env, "CLAUDE_CONFIG_DIR", platform)?.trim();
  const files = [join(configDir ? resolve(configDir) : join(home, ".claude"), "settings.json")];
  if (platform === "win32") {
    for (const root of [envValue(env, "ProgramData", platform), envValue(env, "ProgramFiles", platform)]) {
      if (root) files.push(join(root, "ClaudeCode", "managed-settings.json"));
    }
  } else if (platform === "darwin") {
    files.push("/Library/Application Support/ClaudeCode/managed-settings.json");
  } else {
    files.push("/etc/claude-code/managed-settings.json");
  }
  return files;
}

// Windows reads environment names without regard to case.
function envValue(env: Env, name: string, platform: NodeJS.Platform): string | undefined {
  if (platform !== "win32") return env[name];
  const key = Object.keys(env).find((k) => k.toUpperCase() === name.toUpperCase());
  return key === undefined ? undefined : env[key];
}

// Settings files may hold comments and trailing commas.
function lenientJson(text: string): any {
  const stripped = text
    .replace(/("(?:[^"\\]|\\.)*")|\/\/[^\r\n]*|\/\*[\s\S]*?\*\//g, (m, str: string | undefined) => (str ? m : " "))
    .replace(/("(?:[^"\\]|\\.)*")|,(\s*[}\]])/g, (m, str: string | undefined, close: string | undefined) => (str ? m : (close ?? "")));
  try {
    return JSON.parse(stripped);
  } catch {
    return undefined;
  }
}
