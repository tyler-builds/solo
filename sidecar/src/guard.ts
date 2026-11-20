// The secrets guard: agents' own tool calls may not read Solo's secrets where they are kept. Solo puts a
// secret's value in the environment of one command it runs itself (run_with_secrets) and redacts it
// from what comes back; an agent's shell runs as the user, so it could otherwise read the value
// straight from the keyring, or from that command's environment while it runs. This closes the obvious
// ways, not every way: a program the agent writes can still do it. Mirrors internal/secrets/guard.go.

const READERS: RegExp[] = [
  /\bsecret-tool\b/i,
  /org\.freedesktop\.secrets/i,
  /\bgnome-keyring\b/i,
  /\bkwallet/i,
  /\bsolo-secrets\b/i,
  /\bkeyring\s+get\b/i,
  /\bkeyring\.get_password\b/i,
  /\bfind-(generic|internet)-password\b/i,
  /\bcmdkey\b/i,
  /\bCredRead/i,
  /\bGet-StoredCredential\b/i,
  /\bPasswordVault\b/i,
  /\/proc\/[^\s/]+\/environ\b/i,
  /\benviron\b.*\/proc|\/proc\b.*\benviron\b/i,
];

/** The tools the guard looks at: the shells, and the tools that read files. */
export const GUARDED_TOOLS = "Bash|PowerShell|Read|Grep|Glob";

/**
 * secretGuard says why a tool call may not run because it reads a secret store or another process's
 * environment, or undefined when it may.
 */
export function secretGuard(toolName: string, input: Record<string, unknown>): string | undefined {
  if (!GUARDED_TOOLS.split("|").includes(toolName)) return undefined;
  const text =
    toolName === "Bash" || toolName === "PowerShell"
      ? input.command
      : [input.file_path, input.path, input.pattern].filter((v) => typeof v === "string").join(" ");
  if (typeof text !== "string" || !text) return undefined;
  for (const re of READERS) {
    const m = text.match(re);
    if (m) {
      return (
        `Solo doesn't let agents read secret stores or other processes' environments (matched ${m[0]}). ` +
        "Secrets are used only through run_with_secrets, which sets them in one command for you; you never see their values."
      );
    }
  }
  return undefined;
}
