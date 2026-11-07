// A bidirectional NDJSON JSON-RPC peer over a readable and a writable stream.
//
// stdout belongs to the protocol: nothing else may write to it. Logs go to stderr (see log()).

type Handler = (params: any) => Promise<unknown> | unknown;

interface Pending {
  resolve: (value: any) => void;
  reject: (error: Error) => void;
}

export class RpcError extends Error {
  constructor(
    message: string,
    readonly code = -32000,
  ) {
    super(message);
  }
}

export class Peer {
  private handlers = new Map<string, Handler>();
  private pending = new Map<number, Pending>();
  private nextId = 1;
  private closed = false;
  private closeListeners: (() => void)[] = [];

  constructor(private readonly write: (line: string) => void) {}

  handle(method: string, handler: Handler) {
    this.handlers.set(method, handler);
  }

  onClose(listener: () => void) {
    this.closeListeners.push(listener);
  }

  notify(method: string, params: unknown) {
    this.send({ method, params });
  }

  request<T>(method: string, params: unknown, signal?: AbortSignal): Promise<T> {
    if (this.closed) return Promise.reject(new RpcError("connection closed"));
    const id = this.nextId++;
    return new Promise<T>((resolve, reject) => {
      const abort = () => {
        this.pending.delete(id);
        this.notify("$/cancel", { id });
        reject(new RpcError("aborted"));
      };
      if (signal?.aborted) return abort();
      signal?.addEventListener("abort", abort, { once: true });
      this.pending.set(id, {
        resolve: (v) => {
          signal?.removeEventListener("abort", abort);
          resolve(v);
        },
        reject: (e) => {
          signal?.removeEventListener("abort", abort);
          reject(e);
        },
      });
      this.send({ id, method, params });
    });
  }

  /** Feed one line read from the input stream. */
  async receive(line: string) {
    if (line.trim() === "") return;
    let msg: any;
    try {
      msg = JSON.parse(line);
    } catch {
      log("rpc: dropped unparseable line");
      return;
    }
    if (typeof msg.method === "string") {
      const handler = this.handlers.get(msg.method);
      if (msg.id === undefined) {
        if (handler) {
          try {
            await handler(msg.params);
          } catch (e) {
            log(`rpc: notification ${msg.method} failed: ${errorText(e)}`);
          }
        }
        return;
      }
      if (!handler) {
        this.send({ id: msg.id, error: { code: -32601, message: `unknown method ${msg.method}` } });
        return;
      }
      try {
        const result = await handler(msg.params);
        this.send({ id: msg.id, result: result ?? null });
      } catch (e) {
        this.send({
          id: msg.id,
          error: { code: e instanceof RpcError ? e.code : -32000, message: errorText(e) },
        });
      }
      return;
    }
    if (typeof msg.id === "number") {
      const p = this.pending.get(msg.id);
      if (!p) return;
      this.pending.delete(msg.id);
      if (msg.error) p.reject(new RpcError(String(msg.error.message ?? "error"), msg.error.code));
      else p.resolve(msg.result);
    }
  }

  close() {
    if (this.closed) return;
    this.closed = true;
    for (const p of this.pending.values()) p.reject(new RpcError("connection closed"));
    this.pending.clear();
    for (const l of this.closeListeners) l();
  }

  private send(msg: unknown) {
    if (this.closed) return;
    this.write(JSON.stringify(msg) + "\n");
  }
}

/** Splits a byte stream into lines and feeds them to the peer, in order. */
export async function pump(stream: ReadableStream<Uint8Array>, peer: Peer) {
  const decoder = new TextDecoder();
  let buf = "";
  for await (const chunk of stream) {
    buf += decoder.decode(chunk, { stream: true });
    let nl: number;
    while ((nl = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, nl);
      buf = buf.slice(nl + 1);
      // Requests run concurrently: a long tool.call must not block an interrupt behind it.
      void peer.receive(line);
    }
  }
  if (buf.length > 0) void peer.receive(buf);
  peer.close();
}

export function errorText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

export function log(text: string) {
  process.stderr.write(`[solo-agent] ${text}\n`);
}
