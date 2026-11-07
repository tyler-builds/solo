// solo-agent: hosts Claude Agent SDK sessions for the Solo desktop app over NDJSON JSON-RPC on stdio.

import { query, type SDKUserMessage } from "@anthropic-ai/claude-agent-sdk";
import { PROTOCOL_VERSION, type OpenParams, type ProbeResult, type SendParams } from "./protocol";
import { Peer, RpcError, errorText, log, pump } from "./rpc";
import { claudeExecutable, Session, type SessionHost } from "./session";

const sessions = new Map<string, Session>();

const peer = new Peer((line) => {
  process.stdout.write(line);
});

const host: SessionHost = {
  peer,
  emit: (sessionId, body) => peer.notify("event", { sessionId, ...body }),
  ...(process.env.SOLO_AGENT_LOG_DIR ? { logDir: process.env.SOLO_AGENT_LOG_DIR } : {}),
};

function session(id: unknown): Session {
  const s = sessions.get(String(id));
  if (!s) throw new RpcError(`no session ${String(id)}`, -32001);
  return s;
}

peer.handle("hello", () => ({
  protocol: PROTOCOL_VERSION,
  runtime: typeof Bun === "undefined" ? `node ${process.version}` : `bun ${Bun.version}`,
}));

peer.handle("session.open", async (p: OpenParams) => {
  const existing = sessions.get(p.sessionId);
  if (existing) await existing.close();
  const s = new Session(p, host);
  sessions.set(p.sessionId, s);
  await s.open();
  return {};
});

peer.handle("session.send", async (p: SendParams) => {
  await session(p.sessionId).send(p);
  return {};
});

peer.handle("session.interrupt", async (p: { sessionId: string }) => session(p.sessionId).interrupt());

peer.handle("session.setModel", async (p: { sessionId: string; model?: string }) => {
  await session(p.sessionId).setModel(p.model);
  return {};
});

peer.handle("session.close", async (p: { sessionId: string }) => {
  const s = sessions.get(p.sessionId);
  if (!s) return {};
  sessions.delete(p.sessionId);
  await s.close();
  return {};
});

peer.handle("probe", async (p: { claudePath?: string; cwd?: string }): Promise<ProbeResult> => {
  // A prompt that never yields keeps the CLI alive just long enough to answer initialize.
  const never: AsyncIterable<SDKUserMessage> = { [Symbol.asyncIterator]: () => ({ next: () => new Promise(() => {}) }) };
  const q = query({
    prompt: never,
    options: {
      ...claudeExecutable(p.claudePath),
      ...(p.cwd ? { cwd: p.cwd } : {}),
      settingSources: ["user", "project", "local"],
    },
  });
  try {
    const init = await withTimeout(q.initializationResult(), 60_000, "Claude did not start within 60 seconds");
    let claudeVersion = "";
    try {
      // The version arrives on the first system message, which never comes without a prompt; the
      // init response is enough for models and account.
      claudeVersion = (init as any).claude_code_version ?? "";
    } catch {}
    return {
      claudeVersion,
      models: init.models.map((m) => ({
        value: m.value,
        displayName: m.displayName,
        description: m.description,
        ...(m.supportedEffortLevels ? { effortLevels: m.supportedEffortLevels } : {}),
      })),
      account: {
        ...(init.account.email ? { email: init.account.email } : {}),
        ...(init.account.organization ? { organization: init.account.organization } : {}),
        ...(init.account.subscriptionType ? { subscriptionType: init.account.subscriptionType } : {}),
        ...(init.account.apiKeySource ? { apiKeySource: init.account.apiKeySource } : {}),
      },
    };
  } finally {
    q.close();
  }
});

let shuttingDown = false;
async function shutdown(code = 0) {
  if (shuttingDown) return;
  shuttingDown = true;
  await Promise.allSettled([...sessions.values()].map((s) => s.close()));
  sessions.clear();
  process.exit(code);
}

peer.handle("shutdown", () => {
  setTimeout(() => void shutdown(0), 0);
  return {};
});

// Go closing our stdin means the app is gone: stop every session.
peer.onClose(() => void shutdown(0));
process.on("SIGTERM", () => void shutdown(0));
process.on("SIGINT", () => void shutdown(0));
process.on("unhandledRejection", (e) => log(`unhandled rejection: ${errorText(e)}`));

function withTimeout<T>(p: Promise<T>, ms: number, message: string): Promise<T> {
  return new Promise((resolve, reject) => {
    const t = setTimeout(() => reject(new Error(message)), ms);
    p.then(
      (v) => {
        clearTimeout(t);
        resolve(v);
      },
      (e) => {
        clearTimeout(t);
        reject(e);
      },
    );
  });
}

log(`ready (protocol ${PROTOCOL_VERSION})`);
void pump(Bun.stdin.stream(), peer);
