import { test } from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { SparkleInstaller } from '../src/sparkle.ts';

class FakeApp extends EventEmitter { quits = 0; quit() { this.quits++; } }
const willQuit = (app: FakeApp) => { let prevented = false; app.emit('will-quit', { preventDefault() { prevented = true; } }); return prevented; };

test('Sparkle cannot commit until actual will-quit and cancellation removes permission', () => {
 const app = new FakeApp(); let commits = 0, cancels = 0;
 const installer = new SparkleInstaller({ prepare: async version => version, commit: () => { commits++; }, cancel: () => { cancels++; } }, app, 60_000);
 installer.install(); assert.equal(commits, 0);
 installer.cancelledQuit(); assert.equal(willQuit(app), false); assert.equal(commits, 0); assert.equal(cancels, 1);
 installer.install(); assert.equal(commits, 0); assert.equal(willQuit(app), true); assert.equal(commits, 1);
 // The committed host stays alive for Sparkle's installer to terminate it: a second will-quit (its termination) passes.
 assert.equal(willQuit(app), false); assert.equal(commits, 1); assert.equal(app.quits, 0);
});

test('a committed host that Sparkle never terminates quits on its own after the grace', async () => {
 const app = new FakeApp(); let commits = 0;
 const installer = new SparkleInstaller({ prepare: async version => version, commit: () => { commits++; }, cancel: () => {} }, app, 20);
 installer.install(); assert.equal(willQuit(app), true); assert.equal(commits, 1); assert.equal(app.quits, 0);
 await new Promise(done => setTimeout(done, 60));
 assert.equal(app.quits, 1);
});

// Sparkle 2 (SUUpdateValidator) verifies an archive against the installed app's single SUPublicEDKey, so one key per
// build cannot span a staged window of several releases: the Sparkle key changes at --promote. Each channel's first
// release after it then carries the new key in a Developer-ID-signed DMG, which Sparkle accepts as a key change
// (mac-rotation.test.mjs models the whole update; RELEASING.md, "Staged key rotation").
async function builtMacConfig(trust: Record<string, unknown>, env: Record<string, string> = {}): Promise<{ key: string; dmgSign: boolean }> {
 const { mkdtemp, writeFile, rm } = await import('node:fs/promises');
 const { tmpdir } = await import('node:os');
 const { join, resolve } = await import('node:path');
 const { spawnSync } = await import('node:child_process');
 const directory = await mkdtemp(join(tmpdir(), 'dilla-sparkle-trust-'));
 try {
  const file = join(directory, 'release-trust.json'); await writeFile(file, JSON.stringify(trust));
  const desktop = resolve(import.meta.dirname, '..');
  const script = `const Module = require('node:module'); const resolveFilename = Module._resolveFilename;
Module._resolveFilename = function (request, ...rest) { return request === './release-trust.json' ? ${JSON.stringify(file)} : resolveFilename.call(this, request, ...rest); };
const config = require('./electron-builder.cjs');
process.stdout.write(JSON.stringify({ key: config.mac.extendInfo.SUPublicEDKey, dmgSign: config.dmg.sign }));`;
  const child = spawnSync(process.execPath, ['-e', script], { cwd: desktop, encoding: 'utf8', env: { PATH: process.env.PATH, ...env } });
  assert.equal(child.status, 0, child.stderr);
  return JSON.parse(child.stdout);
 } finally { await rm(directory, { recursive: true, force: true }); }
}

test('a staged build keeps SUPublicEDKey on the active Sparkle key; the promote build switches to the staged key', async () => {
 const { generateKeyPairSync, sign, verify } = await import('node:crypto');
 const raw = (key: ReturnType<typeof generateKeyPairSync>['publicKey']) => key.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
 const active = generateKeyPairSync('ed25519'), staged = generateKeyPairSync('ed25519'), metadata = generateKeyPairSync('ed25519');
 const pem = metadata.publicKey.export({ type: 'spki', format: 'pem' }).toString();
 const stagedTrust = { metadataPublicKeys: [pem], sparklePublicKey: raw(active.publicKey), sparkleNextPublicKey: raw(staged.publicKey), signing: { active: 'A', staged: 'B' } };
 const promotedTrust = { metadataPublicKeys: [pem], sparklePublicKey: raw(staged.publicKey), signing: { active: 'B', staged: null, macRotation: { fromSlot: 'A', fromSparklePublicKey: raw(active.publicKey) } } };
 const stagedBuild = (await builtMacConfig(stagedTrust)).key, promotedBuild = (await builtMacConfig(promotedTrust)).key;
 assert.equal(stagedBuild, raw(active.publicKey), 'every build of the staged window trusts the key that signs it');
 assert.equal(promotedBuild, raw(staged.publicKey), 'the rotation release carries the new key');
 // Sparkle's archive check, modelled with the installed key: within the staged window every update verifies...
 const embedded = (key: string) => ({ key: { kty: 'OKP', crv: 'Ed25519', x: Buffer.from(key, 'base64').toString('base64url') }, format: 'jwk' as const });
 const archive = Buffer.from('dilla mac archive');
 assert.ok(verify(null, archive, embedded(stagedBuild), sign(null, archive, active.privateKey)));
 // ...and after promote the new-key archive does not verify under the old key: a zip would be refused, so the
 // rotation release reaches those Macs through the Developer ID code-signing fallback of a DMG.
 assert.ok(!verify(null, archive, embedded(stagedBuild), sign(null, archive, staged.privateKey)));
 assert.ok(verify(null, archive, embedded(promotedBuild), sign(null, archive, staged.privateKey)));
});

test('the DMG is Developer ID signed exactly while a macOS key change is pending and signing credentials exist', async () => {
 const { generateKeyPairSync } = await import('node:crypto');
 const raw = (key: ReturnType<typeof generateKeyPairSync>['publicKey']) => key.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
 const oldKey = generateKeyPairSync('ed25519'), newKey = generateKeyPairSync('ed25519');
 const pending = { metadataPublicKeys: [], sparklePublicKey: raw(newKey.publicKey), signing: { active: 'B', staged: null, macRotation: { fromSlot: 'A', fromSparklePublicKey: raw(oldKey.publicKey) } } };
 const settled = { metadataPublicKeys: [], sparklePublicKey: raw(newKey.publicKey), signing: { active: 'B', staged: null } };
 assert.deepEqual(await builtMacConfig(pending, { CSC_LINK: 'certificate' }), { key: raw(newKey.publicKey), dmgSign: true });
 assert.deepEqual(await builtMacConfig(pending), { key: raw(newKey.publicKey), dmgSign: false }, 'a fork build without credentials stays ad-hoc');
 assert.deepEqual(await builtMacConfig(settled, { CSC_LINK: 'certificate' }), { key: raw(newKey.publicKey), dmgSign: false }, 'every other release keeps the DMG as before');
});

test('while a macOS key change is pending, the build notarizes and staples the DMG; otherwise it leaves it alone', async () => {
 const { createRequire } = await import('node:module');
 const { createAfterAllArtifactBuild } = createRequire(import.meta.url)('../scripts/mac-rotation-dmg.cjs');
 const env = { CSC_LINK: 'certificate', APPLE_API_KEY: '/runner/AuthKey.p8', APPLE_API_KEY_ID: 'KEYID', APPLE_API_ISSUER: 'issuer' };
 const pending = { signing: { active: 'B', staged: null, macRotation: { fromSlot: 'A', fromSparklePublicKey: 'x' } } };
 const result = { artifactPaths: ['/out/dilla-1.2.3-mac-arm64.dmg', '/out/dilla-1.2.3-mac-arm64.zip', '/out/dilla-1.2.3-mac-arm64.dmg.blockmap'] };
 const runs = (trust: unknown, options: Record<string, unknown> = {}) => {
  const calls: string[][] = [];
  return { calls, hook: createAfterAllArtifactBuild({ trust, env, platform: 'darwin', run: (command: string, args: string[]) => { calls.push([command, ...args]); }, log: () => {}, ...options }) };
 };
 const rotation = runs(pending);
 await rotation.hook(result);
 assert.deepEqual(rotation.calls.map(call => call.slice(0, 3)), [['xcrun', 'notarytool', 'submit'], ['xcrun', 'stapler', 'staple'], ['xcrun', 'stapler', 'validate']]);
 assert.ok(rotation.calls.every(call => call.includes('/out/dilla-1.2.3-mac-arm64.dmg')), 'only the disk image is notarized here');
 for (const [trust, options] of [[{ signing: { active: 'B', staged: null } }, {}], [pending, { platform: 'linux' }], [pending, { env: {} }]] as const) {
  const other = runs(trust, options);
  await other.hook(result);
  assert.deepEqual(other.calls, []);
 }
});
