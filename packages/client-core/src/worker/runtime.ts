/// <reference lib="webworker" />
// The real browser wiring of the core worker (C5, C7): the worker scope's fetch, WebSocket, Web Locks,
// IndexedDB and OPFS, and the one dynamic import of the wasm core.
import { boot, lazyStoreExists, wasmCoreOpener } from '../account/boot';
import { indexedDbKekStore } from '../account/kek';
import type { CoreWasmModule } from '../core-port';
import { toHex } from '../hex';
import { Controller, type ControllerParts } from './controller';
import { workerInstanceURL } from './instance-url';
import { localRuntimeReadiness } from './runtime-readiness';

type WasmModule = CoreWasmModule & { default(): Promise<unknown> };
let wasm: Promise<WasmModule> | null = null;

/** Loads @dilla/core-wasm once. With no argument wasm-bindgen's init fetches the .wasm next to its glue
 *  (new URL('dilla_core_wasm_bg.wasm', import.meta.url)), which Vite emits as one hashed asset. The
 *  generated types are not imported statically (L-TS-03 keeps them out of the lint). */
function loadCoreWasm(): Promise<WasmModule> {
  wasm ??= (async () => {
    const mod = (await import('@dilla/core-wasm')) as unknown as WasmModule;
    await mod.default();
    return mod;
  })();
  return wasm;
}

function isNotFound(e: unknown): boolean {
  return e instanceof DOMException && e.name === 'NotFoundError';
}

export function startCoreWorker(scope: DedicatedWorkerGlobalScope, options: { testHooks: boolean; parts?: Partial<ControllerParts>; instanceURL?: unknown }): Controller {
  const sleep = (ms: number, signal?: AbortSignal): Promise<void> => new Promise<void>((resolve, reject) => {
    const aborted = (): Error => (signal?.reason instanceof Error ? signal.reason : new DOMException('The operation was aborted.', 'AbortError'));
    if (signal?.aborted === true) { reject(aborted()); return; }
    const onAbort = (): void => { scope.clearTimeout(timer); reject(aborted()); };
    const timer = scope.setTimeout(() => { signal?.removeEventListener('abort', onAbort); resolve(); }, ms);
    signal?.addEventListener('abort', onAbort, { once: true });
  });
  const kek = indexedDbKekStore(scope.indexedDB, scope.crypto.subtle, (a) => scope.crypto.getRandomValues(a));
  const controller = new Controller({
    origin: workerInstanceURL(options.instanceURL, scope.location.origin),
    fetch: scope.fetch.bind(scope),
    // The worker's global WebSocket; lib.webworker declares it as a global, not as a member of the scope type.
    WebSocket: (scope as DedicatedWorkerGlobalScope & typeof globalThis).WebSocket,
    locks: { request: async (name, opts, callback) => { await scope.navigator.locks.request(name, opts, callback); } },
    now: () => Date.now(),
    monotonic: () => scope.performance.now(),
    random: () => Math.random(),
    setTimeout: (fn, ms) => scope.setTimeout(fn, ms),
    clearTimeout: (id) => { scope.clearTimeout(id); },
    sleep,
    // A callKeys push transfers its key buffer (L-TS-52): the post detaches it here, so no copy stays in the worker.
    post: (message, transfer) => { scope.postMessage(message, transfer ?? []); },
    testHooks: options.testHooks,
    runtimeReady: localRuntimeReadiness(loadCoreWasm),
    boot: async (instance) => {
      const mod = await loadCoreWasm();
      // The root is asked for lazily: a private WebKit window rejects getDirectory(), and boot()'s probe must report that first.
      return boot({
        instance,
        probePersistence: () => mod.probe_persistence(),
        storeExists: lazyStoreExists(() => scope.navigator.storage.getDirectory()),
        kek,
        openCore: wasmCoreOpener(mod, (ms) => sleep(ms)),
      });
    },
    resetDevice: async (instance) => {
      const hex = toHex(instance.instanceId);
      await kek.remove(hex);
      const root = await scope.navigator.storage.getDirectory();
      try {
        const dilla = await root.getDirectoryHandle('dilla');
        await dilla.removeEntry(hex, { recursive: true });
      } catch (e) {
        if (!isNotFound(e)) throw e;
      }
    },
    ...(options.parts === undefined ? {} : { parts: options.parts }),
  });
  scope.addEventListener('message', (e: MessageEvent<unknown>) => { controller.handle(e.data); });
  return controller;
}
