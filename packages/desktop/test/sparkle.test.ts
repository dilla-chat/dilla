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

// Sparkle 2 (SUUpdateValidator) verifies an archive against the installed app's single SUPublicEDKey. A build may
// change that key only if its archive verifies under the old one, and the next archive must then verify under the
// new one, so a multi-release staged window cannot carry a Sparkle key change: the Sparkle key changes at --promote,
// and macOS installs from before the promote release reinstall once (RELEASING.md, release-keys.mjs warning).
async function builtSUPublicEDKey(trust: Record<string, unknown>): Promise<string> {
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
process.stdout.write(require('./electron-builder.cjs').mac.extendInfo.SUPublicEDKey);`;
  const child = spawnSync(process.execPath, ['-e', script], { cwd: desktop, encoding: 'utf8', env: { PATH: process.env.PATH } });
  assert.equal(child.status, 0, child.stderr);
  return child.stdout;
 } finally { await rm(directory, { recursive: true, force: true }); }
}

test('a staged build keeps SUPublicEDKey on the active Sparkle key; the promote build switches to the staged key', async () => {
 const { generateKeyPairSync, sign, verify } = await import('node:crypto');
 const raw = (key: ReturnType<typeof generateKeyPairSync>['publicKey']) => key.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
 const active = generateKeyPairSync('ed25519'), staged = generateKeyPairSync('ed25519'), metadata = generateKeyPairSync('ed25519');
 const pem = metadata.publicKey.export({ type: 'spki', format: 'pem' }).toString();
 const stagedTrust = { metadataPublicKeys: [pem], sparklePublicKey: raw(active.publicKey), sparkleNextPublicKey: raw(staged.publicKey), signing: { active: 'A', staged: 'B' } };
 const promotedTrust = { metadataPublicKeys: [pem], sparklePublicKey: raw(staged.publicKey), signing: { active: 'B', staged: null } };
 const stagedBuild = await builtSUPublicEDKey(stagedTrust), promotedBuild = await builtSUPublicEDKey(promotedTrust);
 assert.equal(stagedBuild, raw(active.publicKey), 'every build of the staged window trusts the key that signs it');
 assert.equal(promotedBuild, raw(staged.publicKey));
 // Sparkle's archive check, modelled with the installed key: within the staged window every update verifies...
 const embedded = (key: string) => ({ key: { kty: 'OKP', crv: 'Ed25519', x: Buffer.from(key, 'base64').toString('base64url') }, format: 'jwk' as const });
 const archive = Buffer.from('dilla mac archive');
 assert.ok(verify(null, archive, embedded(stagedBuild), sign(null, archive, active.privateKey)));
 // ...and after promote an install older than the promote release cannot verify the staged-key archive: it reinstalls.
 assert.ok(!verify(null, archive, embedded(stagedBuild), sign(null, archive, staged.privateKey)));
 assert.ok(verify(null, archive, embedded(promotedBuild), sign(null, archive, staged.privateKey)));
});
