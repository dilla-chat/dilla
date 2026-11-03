import { describe, it, expect } from 'vitest';
import { localRuntimeReadiness } from './runtime-readiness';
import { harness } from './world';
describe('local runtime readiness', () => {
 it('loads and probes once without network, account boot or keystore operations', async () => {
  const work: string[] = [];
  const ready = localRuntimeReadiness(() => { work.push('wasm'); return Promise.resolve({ probe_persistence: () => { work.push('probe'); return Promise.resolve('{"mode":"opfs"}'); } }); });
  expect(await Promise.all([ready(), ready(), ready()])).toEqual([null, null, null]);
  expect(work).toEqual(['wasm', 'probe']);
 });
 it('refuses a missing/broken WASM module, persistence refusal or malformed probe result', async () => {
  const failed = localRuntimeReadiness(() => Promise.reject(new Error('WASM bytes missing')));
  await expect(failed()).rejects.toThrow('WASM bytes missing');
  for (const result of ['{"mode":"memory","reason":"no-access"}', '{}', 'invalid']) {
   const ready = localRuntimeReadiness(() => Promise.resolve({ probe_persistence: () => Promise.resolve(result) }));
   await expect(ready()).rejects.toThrow();
  }
 });
 it('answers through the normal worker barrier before network start and sanitizes failures', async () => {
  const paths: string[] = [];
  const w = harness({ runtimeReady: () => Promise.resolve(null), fetch: () => { paths.push('network'); return Promise.reject(new Error('offline')); } });
  w.call(1, { m: 'runtimeReady' });
  await expect.poll(() => w.ret(1)).toEqual({ t: 'ret', id: 1, ok: true, value: null });
  expect(paths).toEqual([]); expect(w.account()).toBeUndefined();
  const broken = harness({ runtimeReady: () => Promise.reject(new Error('private local path must stay in worker')) });
  broken.call(2, { m: 'runtimeReady' });
  await expect.poll(() => broken.ret(2)).toMatchObject({ t: 'ret', id: 2, ok: false, error: { code: 'E_INTERNAL', detail: '' } });
  let malformedId = 3;
  for (const command of [{ m: 'runtimeReady', path: '/tmp/other-store' }, { m: 'runtimeReady', key: 'secret' }, { m: 'runtimeReadyUnknown' }]) {
   broken.call(malformedId, command); expect(broken.ret(malformedId)).toMatchObject({ t: 'ret', ok: false, error: { code: 'E_BAD_INPUT' } }); malformedId++;
  }
 });
});
