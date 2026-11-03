import { CoreError } from '../core-port';
interface LocalRuntime { probe_persistence(): Promise<string> }
/** Content-free local capability check. It does not open an account store,
 * acquire an identity, load a KEK, or make a network request. */
export function localRuntimeReadiness(load: () => Promise<LocalRuntime>): () => Promise<null> {
 let readiness: Promise<null> | null = null;
 return () => {
  readiness ??= (async () => {
   const runtime = await load(), probe: unknown = JSON.parse(await runtime.probe_persistence());
   if (!probe || typeof probe !== 'object' || !('mode' in probe) || probe.mode !== 'opfs') throw new CoreError('E_UNSUPPORTED');
   return null;
  })();
  return readiness;
 };
}
