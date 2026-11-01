import { test } from 'node:test';
import assert from 'node:assert/strict';
import { generateKeyPairSync, createHash, sign } from 'node:crypto';
import { verifyWindowsRelease } from '../src/windows-release.ts';
const keys = generateKeyPairSync('ed25519'), publicKey = keys.publicKey.export({ type: 'spki', format: 'pem' }).toString(), now = Date.now();
const digest = createHash('sha512').update('fixture').digest('base64');
const context = { publicKeys: [publicKey], channel: 'stable' as const, platform: 'win32', arch: 'x64', currentVersion: '1.0.0', now };
const descriptor = () => ({ format: 1, version: '1.0.1', channel: 'stable', publishedAt: new Date(now - 1000).toISOString(), expiresAt: new Date(now + 3600000).toISOString(), artifacts: [{ platform: 'win32', arch: 'x64', url: 'https://github.com/jonasthim/dilla/releases/download/desktop-v1.0.1/dilla-1.0.1-win-x64.exe', size: 7, sha512: digest, programManifest: { url: 'https://github.com/jonasthim/dilla/releases/download/desktop-v1.0.1/dilla-1.0.1-win-x64.manifest.json', size: 100, sha512: digest } }] });
const signed = (doc: unknown) => { const raw = Buffer.from(typeof doc === 'string' ? doc : JSON.stringify(doc)); return { raw, signature: sign(null, raw, keys.privateKey).toString('base64') }; };
test('Windows signed descriptor binds its exact installed-program manifest without changing installer hash authority', () => {
 const { raw, signature } = signed(descriptor()); const release = verifyWindowsRelease(raw, signature, context);
 assert.equal(release.artifacts[0].programManifest?.sha512, digest);
 assert.throws(() => verifyWindowsRelease(Buffer.from('{not JSON'), signature, context), /signature/);
});
test('Windows launcher and updater reject the same ambiguous or unauthenticated manifest descriptors', () => {
 const changes: Array<(doc: ReturnType<typeof descriptor> & Record<string, unknown>) => void> = [doc => { delete (doc.artifacts[0] as { programManifest?: unknown }).programManifest; }, doc => { doc.artifacts[0].programManifest.url = 'https://attacker.invalid/program.manifest.json'; }, doc => { doc.artifacts[0].programManifest.url = doc.artifacts[0].programManifest.url.replace('.manifest.json', '.json'); }, doc => { (doc.artifacts[0].programManifest as Record<string, unknown>).extra = 'path'; }, doc => { doc.artifacts[0].programManifest.size = 1048577; }, doc => { doc.publishedAt = '2026-02-30T00:00:00Z'; }, doc => { doc.Format = 1; }];
 for (const change of changes) {
  const doc = descriptor(); change(doc); const { raw, signature } = signed(doc);
  assert.throws(() => verifyWindowsRelease(raw, signature, context));
 }
 const { raw, signature } = signed(JSON.stringify(descriptor()).replace('"format":1', '"format":1,"format":1'));
 assert.throws(() => verifyWindowsRelease(raw, signature, context), /duplicate/i);
});
