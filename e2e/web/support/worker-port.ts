// A test-only handle on the app's own core worker (web3.7 ruling R27), on the RecordedWorker pattern of
// e2e/voice/support/peer-browser.ts. The init script wraps `window.Worker`, keeps the worker the bridge starts ("dilla-core")
// and the newest value of every slice it publishes, and lets the test post a `call` message of its own: the message
// `client.call(command)` would post, around the UI (a disabled composer, a hidden control). The reply comes back to the test,
// not to the app (the bridge drops a `ret` with an id it did not issue).
//
// `holdUploads` is the other half of the lying-sender attack: it rewrites the class of an upload on its way to the host. The
// upload is a gateway request frame sent by the worker, so the worker's WebSocket.send does it.
import type { BrowserContext, Page, Worker as PlaywrightWorker } from '@playwright/test';
import { watchSocketRequests, type SocketRequestSeen } from './socket-requests';

export interface WorkerReply { ok: boolean; value?: unknown; error?: { code: string; detail: string; status: number; reason?: string } }
interface PortGlobal {
  call(command: Record<string, unknown>): Promise<WorkerReply>;
  slice(name: string): unknown;
}

/** Installs the handle in every page of `context` from its next navigation on. Call it before the first `goto`. */
export async function installWorkerPort(context: BrowserContext): Promise<void> {
  await context.addInitScript(() => {
    const cores: Worker[] = [];
    const slices = new Map<string, { rev: number; value: unknown }>();
    let next = 2_000_000_000;
    const Native = window.Worker;
    class TappedWorker extends Native {
      constructor(url: string | URL, options?: WorkerOptions) {
        super(url, options);
        if (options?.name === 'dilla-core') cores.push(this);
        this.addEventListener('message', (e) => {
          const m = (e as MessageEvent).data as { t?: unknown; name?: unknown; rev?: unknown; value?: unknown } | null;
          if (m === null || typeof m !== 'object' || m.t !== 'slice' || typeof m.name !== 'string' || typeof m.rev !== 'number') return;
          if (m.rev > (slices.get(m.name)?.rev ?? 0)) slices.set(m.name, { rev: m.rev, value: m.value });
        });
      }
    }
    Object.defineProperty(window, 'Worker', { value: TappedWorker, configurable: true, writable: true });
    const port: PortGlobal = {
      call(command) {
        const core = cores[cores.length - 1];
        if (core === undefined) return Promise.reject(new Error('the core worker has not started'));
        const id = next++;
        return new Promise<WorkerReply>((resolve) => {
          const onMessage = (e: Event): void => {
            const m = (e as MessageEvent).data as { t?: unknown; id?: unknown; ok?: unknown; value?: unknown; error?: unknown } | null;
            if (m === null || typeof m !== 'object' || m.t !== 'ret' || m.id !== id) return;
            core.removeEventListener('message', onMessage);
            resolve({ ok: m.ok === true, value: m.value, error: m.error as WorkerReply['error'] });
          };
          core.addEventListener('message', onMessage);
          core.postMessage({ t: 'call', id, command });
        });
      },
      slice: (name) => slices.get(name)?.value,
    };
    Object.defineProperty(window, '__dillaPort', { value: port, configurable: true });
  });
}

/** Posts `command` to the page's core worker as the app's own client would, and returns the worker's reply. */
export function portCall(page: Page, command: Record<string, unknown>): Promise<WorkerReply> {
  return page.evaluate((c) => (window as unknown as { __dillaPort: PortGlobal }).__dillaPort.call(c), command);
}

/** The newest value of a slice the worker published to this page (undefined before the first). */
export function portSlice<T>(page: Page, name: string): Promise<T | undefined> {
  return page.evaluate((n) => (window as unknown as { __dillaPort: PortGlobal }).__dillaPort.slice(n), name) as Promise<T | undefined>;
}

/** One upload the context saw: the host's status, and whether the class byte was rewritten on its way. */
export interface UploadSeen { status: number; relabelled: boolean }
export interface UploadHold {
  /** Rewrites the class of every upload the workers send from now on (await it before the send it must catch). */
  setRelabel(on: boolean): Promise<void>;
  /** Every answered upload, in the order it was sent. */
  readonly seen: UploadSeen[];
}

const UPLOAD = /^\/v1\/groups\/[0-9a-f]{32}\/message$/;

/** Runs in a worker: while `on`, a group message upload request frame leaves with its class byte turned from 0 to 1. The
 *  frame is `[59, 0, null, [id, method, route, body, idem]]`, the body `[epoch, private_message, class]`, so the class is the
 *  body's last byte. The patch is on the WebSocket prototype, so it also applies to a socket the worker opened before. */
function relabelUploads(on: boolean): void {
  const g = globalThis as unknown as { dillaRelabel?: boolean; dillaRelabelPatched?: boolean };
  g.dillaRelabel = on;
  if (g.dillaRelabelPatched === true) return;
  g.dillaRelabelPatched = true;
  const send = WebSocket.prototype.send;
  WebSocket.prototype.send = function (this: WebSocket, data: string | ArrayBufferLike | Blob | ArrayBufferView): void {
    if (g.dillaRelabel === true && typeof data !== 'string' && !(data instanceof Blob)) {
      const view = ArrayBuffer.isView(data) ? new Uint8Array(data.buffer, data.byteOffset, data.byteLength) : new Uint8Array(data);
      const bytes = view.slice();
      const head = (i: number): [number, number, number] | null => {
        const b = bytes[i];
        if (b === undefined) return null;
        const info = b & 31;
        if (info < 24) return [b >> 5, info, i + 1];
        const n = info === 24 ? 1 : info === 25 ? 2 : info === 26 ? 4 : 0;
        if (n === 0) return null;
        let v = 0;
        for (let k = 1; k <= n; k++) v = v * 256 + (bytes[i + k] ?? 0);
        return [b >> 5, v, i + 1 + n];
      };
      if (bytes[0] === 0x84 && bytes[1] === 0x18 && bytes[2] === 0x3b && bytes[3] === 0x00 && bytes[4] === 0xf6 && bytes[5] === 0x85) {
        const id = head(6);
        const method = id === null ? null : head(id[2]);
        const methodText = method === null ? '' : String.fromCharCode(...bytes.subarray(method[2], method[2] + method[1]));
        const route = method === null ? null : head(method[2] + method[1]);
        const routeText = route === null ? '' : String.fromCharCode(...bytes.subarray(route[2], route[2] + route[1]));
        const body = route === null ? null : head(route[2] + route[1]);
        if (body !== null && methodText === 'POST' && /^\/v1\/groups\/[0-9a-f]{32}\/message$/.test(routeText)) {
          const last = body[2] + body[1] - 1;
          if (bytes[last] === 0x00) bytes[last] = 0x01;
        }
      }
      send.call(this, bytes);
      return;
    }
    send.call(this, data);
  };
}

/**
 * Watches the group message uploads of every page in `context` and can rewrite their class. The uploads are gateway request
 * frames: they are read from the page's socket frames (their status from the response frame), and the class byte is
 * turned in the worker's WebSocket.send while `setRelabel(true)` is on (`relabelUploads`). Call it before the first `goto`.
 */
export async function holdUploads(context: BrowserContext): Promise<UploadHold> {
  const requests: SocketRequestSeen[] = [];
  const workers = new Set<PlaywrightWorker>();
  let relabel = false;
  const patch = (worker: PlaywrightWorker): void => { workers.add(worker); void worker.evaluate(relabelUploads, relabel).catch(() => undefined); };
  const attach = (page: Page): void => {
    watchSocketRequests(page, (r) => { if (r.method === 'POST' && UPLOAD.test(r.path)) requests.push(r); });
    page.on('worker', patch);
    for (const worker of page.workers()) patch(worker);
  };
  for (const page of context.pages()) attach(page);
  context.on('page', attach);
  return {
    async setRelabel(on: boolean): Promise<void> {
      relabel = on;
      await Promise.all([...workers].map((w) => w.evaluate(relabelUploads, on).catch(() => undefined)));
    },
    get seen(): UploadSeen[] {
      return requests.filter((r) => r.status !== null).map((r) => ({ status: r.status!, relabelled: r.body.at(-1) === 0x01 }));
    },
  };
}
