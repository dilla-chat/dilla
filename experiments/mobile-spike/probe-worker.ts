/// <reference lib="webworker" />
// The OPFS half of the probe, in a module Worker like the real core worker (packages/client-core/src/worker/runtime.ts).
// opfsSyncHandle: the real readiness gate, localRuntimeReadiness over @dilla/core-wasm's probe_persistence (mode "opfs"
// means createSyncAccessHandle worked). opfsPersisted: a marker file written on the first launch, found on the next.
import { localRuntimeReadiness } from '../../packages/client-core/src/worker/runtime-readiness';

interface CoreWasm { default(): Promise<unknown>; probe_persistence(): Promise<string> }

let core: Promise<CoreWasm> | null = null;
function loadCoreWasm(): Promise<CoreWasm> {
  core ??= (async () => {
    const mod = (await import('@dilla/core-wasm')) as unknown as CoreWasm;
    await mod.default();
    return mod;
  })();
  return core;
}

const MARKER = 'dilla-probe-marker';

async function marker(): Promise<{ persisted: boolean; detail: string }> {
  const root = await navigator.storage.getDirectory();
  let persisted = false, detail = '';
  try {
    const text = await (await (await root.getFileHandle(MARKER)).getFile()).text();
    persisted = text.startsWith('dilla-probe ');
    detail = `found: ${text}`;
  } catch (e) {
    detail = `absent: ${(e as DOMException).name}`;
  }
  const handle = await root.getFileHandle(MARKER, { create: true });
  const body = new TextEncoder().encode(`dilla-probe ${new Date().toISOString()}`);
  const sync = await handle.createSyncAccessHandle();
  try { sync.truncate(0); sync.write(body, { at: 0 }); sync.flush(); } finally { sync.close(); }
  return { persisted, detail };
}

// The page posts its trigger first. Only a launch run reads the marker a previous process left: every run rewrites it,
// so a tap in the same process would otherwise find the previous run's marker and report persistence without a relaunch.
self.onmessage = (e: MessageEvent<{ trigger: string }>) => { void probe(e.data.trigger); };

async function probe(trigger: string): Promise<void> {
  const out: Record<string, unknown> = {};
  try {
    await localRuntimeReadiness(loadCoreWasm)();
    out.opfsSyncHandle = true;
  } catch (e) {
    out.opfsSyncHandle = false;
    out.opfsError = String(e);
  }
  try { out.probePersistence = JSON.parse(await (await loadCoreWasm()).probe_persistence()); } catch (e) { out.probePersistence = String(e); }
  if (trigger !== 'launch') {
    out.opfsPersisted = false;
    out.markerDetail = `skipped: ${trigger}`;
  } else try {
    const m = await marker();
    out.opfsPersisted = m.persisted;
    out.markerDetail = m.detail;
  } catch (e) {
    out.opfsPersisted = false;
    out.markerDetail = `error: ${String(e)}`;
  }
  postMessage(out);
}
