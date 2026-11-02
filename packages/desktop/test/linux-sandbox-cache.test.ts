import { test } from 'node:test';
import assert from 'node:assert/strict';
import { sandboxReleaseFixture } from './support/linux-sandbox-release-fixture.ts';
import { authenticateSandboxSidecars, saveSandboxSidecars, readSandboxSidecars, saveManualSignature, fetchSandboxAsset } from '../src/linux-sandbox-cache.ts';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

test('authenticated sidecars are bound to all three signed asset records and generation', () => {
 const f = sandboxReleaseFixture(), name = 'dilla-1.2.3-linux-x64.sandbox';
 const records = [f.capsule, f.manifest, f.signature].map((bytes, i) => ({ name: name + ['', '.json', '.sig'][i], size: bytes.length, sha512: f.hash(bytes) }));
 const input = { version: '1.2.3', descriptor: { compatibilityEpoch: 1 as const, minimumGeneration: 1, bundle: { url: `https://github.com/jonasthim/dilla/releases/download/desktop-v1.2.3/${name}`, size: records[0].size, sha512: records[0].sha512 } }, assets: records, capsule: f.capsule, manifest: f.manifest, signature: f.signature, keys: [f.config.publicKeys[0]], now: Date.parse('2026-11-02T00:00:00Z') };
 assert.equal(authenticateSandboxSidecars(input).generation, 1);
 assert.throws(() => authenticateSandboxSidecars({ ...input, signature: Buffer.from('x'.repeat(89)) }));
 assert.throws(() => authenticateSandboxSidecars({ ...input, descriptor: { ...input.descriptor, minimumGeneration: 2 } }));
 assert.throws(() => authenticateSandboxSidecars({ ...input, keys: [Buffer.alloc(32).toString('base64')] }));
});

test('sandbox downloads reject unapproved redirects and oversized bodies', async () => {
 const original = globalThis.fetch;
 try {
  globalThis.fetch = async (_input, init) => { assert.equal(init?.credentials, 'omit'); assert.equal(init?.redirect, 'manual'); return new Response(null, { status: 302, headers: { location: 'https://evil.example/asset' } }); };
  await assert.rejects(fetchSandboxAsset('https://github.com/jonasthim/dilla/releases/download/v/a', 4, AbortSignal.timeout(1000)), /destination/);
  globalThis.fetch = async () => new Response('12345');
  await assert.rejects(fetchSandboxAsset('https://github.com/jonasthim/dilla/releases/download/v/a', 4, AbortSignal.timeout(1000)), /too large/);
 } finally { globalThis.fetch = original; }
});

test('private sidecar cache is durable and reauthenticates disk bytes on every read', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-cache-test-'));
 try {
  const f = sandboxReleaseFixture(), name = 'dilla-1.2.3-linux-x64.sandbox';
  const assets = [f.capsule, f.manifest, f.signature].map((bytes, i) => ({ name: name + ['', '.json', '.sig'][i], size: bytes.length, sha512: f.hash(bytes) }));
  const input = { version: '1.2.3', descriptor: { compatibilityEpoch: 1 as const, minimumGeneration: 1, bundle: { url: `https://github.com/jonasthim/dilla/releases/download/desktop-v1.2.3/${name}`, size: assets[0].size, sha512: assets[0].sha512 } }, assets, capsule: f.capsule, manifest: f.manifest, signature: f.signature, keys: f.config.publicKeys, now: Date.parse('2026-11-02T00:00:00Z') };
  const directory = await saveSandboxSidecars(root, input);
  assert.equal((await readSandboxSidecars(directory, input)).generation, 1);
  const manual = await saveManualSignature(root, input);
  assert.deepEqual(await readFile(manual), Buffer.from(f.signature.toString('utf8').trim(), 'base64'));
  await writeFile(join(directory, 'manifest'), 'tampered');
  await assert.rejects(readSandboxSidecars(directory, input));
 } finally { await rm(root, { recursive: true, force: true }); }
});
