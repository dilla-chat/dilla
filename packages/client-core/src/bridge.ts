// The page side of the core worker (L-TS-10): calls go out with an id and come back as one ret; slices
// come in with a revision, and the page keeps only the newest of each.
import type { CallKeys, SliceName, SliceTypes, WorkerError } from './state/types';
import type { Command } from './worker/protocol';

export class CoreCallError extends Error {
  readonly code: string;
  readonly detail: string;
  readonly status: number;
  readonly retryAfterMs: number | null;
  /** A stable key for a refusal the worker made itself; the page maps it to copy. */
  readonly reason: string | undefined;

  constructor(error: WorkerError) {
    super(error.detail ? `${error.code}: ${error.detail}` : error.code);
    this.name = 'CoreCallError';
    this.code = error.code;
    this.detail = error.detail;
    this.status = error.status;
    this.retryAfterMs = error.retryAfterMs;
    this.reason = error.reason;
  }
}

export interface CoreClient {
  call<C extends Command>(command: C): Promise<unknown>;
  get<N extends SliceName>(name: N): SliceTypes[N] | undefined;
  subscribe(name: SliceName, listener: () => void): () => void;
  /** L-TS-52: each epoch's call key, handed over synchronously. The buffer is zero-filled as soon as every listener has
   *  returned, so a listener copies what it needs before it returns (decision 3). */
  onCallKeys(listener: (callId: string, keys: CallKeys) => void): () => void;
  dispose(): void;
}

const HEX_ID = /^[0-9a-f]{32}$/;
const EPOCH = /^(0|[1-9][0-9]{0,19})$/;
const KEY_BYTES = 16;
const U64_MAX = 18446744073709551615n;
const ROSTER_MAX = 1024;

const leafIndex = (v: unknown): boolean => typeof v === 'number' && Number.isSafeInteger(v) && v >= 0;

/** A callKeys push as L-TS-52 shapes it: 32-hex ids, a decimal epoch and a 16-byte key. */
function callKeysOf(m: Record<string, unknown>): { callId: string; keys: CallKeys } | null {
  const { callId, keys } = m;
  if (typeof callId !== 'string' || !HEX_ID.test(callId) || typeof keys !== 'object' || keys === null) return null;
  const { groupId, epoch, baseKey, selfLeaf, roster } = keys as Record<string, unknown>;
  if (typeof groupId !== 'string' || !HEX_ID.test(groupId) || typeof epoch !== 'string' || !EPOCH.test(epoch) || BigInt(epoch) > U64_MAX) return null;
  if (!(baseKey instanceof ArrayBuffer) || baseKey.byteLength !== KEY_BYTES || !leafIndex(selfLeaf)) return null;
  if (!Array.isArray(roster) || roster.length > ROSTER_MAX) return null;
  for (const row of roster as unknown[]) {
    if (typeof row !== 'object' || row === null) return null;
    const { leaf, deviceId, userId } = row as Record<string, unknown>;
    if (!leafIndex(leaf) || typeof deviceId !== 'string' || !HEX_ID.test(deviceId) || typeof userId !== 'string' || !HEX_ID.test(userId)) return null;
  }
  return { callId, keys: keys as CallKeys };
}

const DISPOSED: WorkerError = { code: 'E_DISPOSED', detail: '', status: 0, retryAfterMs: null };

function workerError(value: unknown): WorkerError {
  if (typeof value === 'object' && value !== null) {
    const { code, detail, status, retryAfterMs, reason } = value as Record<string, unknown>;
    if (typeof code === 'string' && typeof detail === 'string' && typeof status === 'number'
      && (typeof retryAfterMs === 'number' || retryAfterMs === null)) {
      return { code, detail, status, retryAfterMs, ...(typeof reason === 'string' ? { reason } : {}) };
    }
  }
  return { code: 'E_INTERNAL', detail: 'malformed worker error', status: 0, retryAfterMs: null };
}

export function connectCore(worker: Worker): CoreClient {
  let next = 1;
  let disposed = false;
  const pending = new Map<number, { resolve: (value: unknown) => void; reject: (error: CoreCallError) => void }>();
  const slices = new Map<SliceName, { rev: number; value: unknown }>();
  // One entry object per subscription, so subscribing the same function twice needs two removals.
  const listeners = new Map<SliceName, Set<{ listener: () => void }>>();
  const keyListeners = new Set<{ listener: (callId: string, keys: CallKeys) => void }>();

  worker.addEventListener('message', (event: MessageEvent<unknown>) => {
    const data = event.data;
    if (typeof data !== 'object' || data === null) return;
    const m = data as Record<string, unknown>;
    if (m.t === 'callKeys') {
      const keys = typeof m.keys === 'object' && m.keys !== null ? (m.keys as Record<string, unknown>) : null;
      const buffer = keys?.baseKey instanceof ArrayBuffer ? keys.baseKey : null;
      try {
        const valid = callKeysOf(m);
        if (valid === null) return;
        for (const entry of [...keyListeners]) {
          try { entry.listener(valid.callId, valid.keys); }
          catch (err) { queueMicrotask(() => { throw err; }); }
        }
      } finally {
        // Decision 3: the transferred key is zeroed once every listener has had it, whether or not it was valid or heard.
        if (buffer !== null) new Uint8Array(buffer).fill(0);
      }
      return;
    }
    if (m.t === 'ret' && typeof m.id === 'number') {
      const call = pending.get(m.id);
      if (call === undefined) return;
      pending.delete(m.id);
      if (m.ok === true) call.resolve(m.value);
      else call.reject(new CoreCallError(workerError(m.error)));
      return;
    }
    if (m.t === 'slice' && typeof m.name === 'string' && typeof m.rev === 'number') {
      const name = m.name as SliceName;
      if (m.rev <= (slices.get(name)?.rev ?? 0)) return;
      slices.set(name, { rev: m.rev, value: m.value });
      for (const entry of [...(listeners.get(name) ?? [])]) {
        try { entry.listener(); }
        catch (err) { queueMicrotask(() => { throw err; }); }
      }
    }
  });

  return {
    call(command) {
      if (disposed) return Promise.reject(new CoreCallError(DISPOSED));
      const id = next++;
      return new Promise<unknown>((resolve, reject) => {
        pending.set(id, { resolve, reject });
        worker.postMessage({ t: 'call', id, command });
      });
    },
    get<N extends SliceName>(name: N): SliceTypes[N] | undefined {
      return slices.get(name)?.value as SliceTypes[N] | undefined;
    },
    subscribe(name, listener) {
      const entry = { listener };
      const set = listeners.get(name) ?? new Set<{ listener: () => void }>();
      set.add(entry);
      listeners.set(name, set);
      return () => { set.delete(entry); };
    },
    onCallKeys(listener) {
      const entry = { listener };
      keyListeners.add(entry);
      return () => { keyListeners.delete(entry); };
    },
    dispose() {
      if (disposed) return;
      disposed = true;
      worker.terminate();
      for (const call of pending.values()) call.reject(new CoreCallError(DISPOSED));
      pending.clear();
    },
  };
}

export function createCoreWorker(options?: { instanceURL: string }): Worker {
  const worker = new Worker(new URL('./worker/entry.ts', import.meta.url), { type: 'module', name: 'dilla-core' });
  if (options) worker.postMessage({ t: 'configure-instance', instanceURL: options.instanceURL });
  return worker;
}
