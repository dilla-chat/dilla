import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash, generateKeyPairSync, sign } from 'node:crypto';
import { verifyRelease, verifyArtifact, UpdateCoordinator, type Release } from '../src/update.ts';

const pair = generateKeyPairSync('ed25519');
const publicKey = pair.publicKey.export({ type: 'spki', format: 'pem' }).toString();
const bytes = Buffer.from('an update artifact');
const release: Release = {
  format: 1, version: '0.1.1', channel: 'stable', publishedAt: '2026-10-30T00:00:00.000Z', expiresAt: '2026-11-30T00:00:00.000Z',
  artifacts: [{ platform: 'linux', arch: 'x64', url: 'https://github.com/jonasthim/dilla/releases/download/desktop-v0.1.1/dilla-0.1.1-linux-x64.AppImage', size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64') }],
};
const now = Date.parse('2026-10-31T00:00:00Z');
function signed(value: unknown) {
  const body = Buffer.from(JSON.stringify(value));
  return { body, signature: sign(null, body, pair.privateKey).toString('base64') };
}
const context = { publicKeys: [publicKey], channel: 'stable' as const, platform: 'linux', arch: 'x64', currentVersion: '0.1.0', now };

test('release signatures bind artifact hashes, channel, expiration and the fixed GitHub destination', () => {
  const good = signed(release);
  const result = verifyRelease(good.body, good.signature, context);
  assert.equal(result.version, '0.1.1');
  assert.equal(verifyArtifact(bytes, result.artifacts[0]), true);
  assert.equal(verifyArtifact(Buffer.from('tampered'), result.artifacts[0]), false);
  assert.throws(() => verifyRelease(Buffer.from(good.body.toString().replace('0.1.1', '9.9.9')), good.signature, context), /signature/);
  assert.throws(() => verifyRelease(good.body, good.signature, { ...context, publicKeys: [] }), /key/);
  for (const bad of [
    { ...release, channel: 'beta' }, { ...release, version: '0.1.0' }, { ...release, version: '0.1.2-beta.1' },
    { ...release, expiresAt: '2026-10-30T00:00:00.000Z' }, { ...release, publishedAt: '2026-12-30T00:00:00.000Z' },
    { ...release, artifacts: [{ ...release.artifacts[0], url: 'https://evil.test/update.AppImage' }] },
    { ...release, artifacts: [{ ...release.artifacts[0], url: 'https://github.com/jonasthim/dilla/releases/download/desktop-v0.1.1/../evil.exe' }] },
    { ...release, artifacts: [{ ...release.artifacts[0], sha512: 'bad' }] },
    { ...release, artifacts: [...release.artifacts, ...release.artifacts] },
  ]) {
    const doc = signed(bad);
    assert.throws(() => verifyRelease(doc.body, doc.signature, context), JSON.stringify(bad));
  }
});

test('updates wait for calls and drafts, survive tray closes, retry downloads and only install on an actual quit', async () => {
  let fail = true;
  let installs = 0;
  let downloads = 0;
  const updater = new UpdateCoordinator({
    check: async () => release,
    download: async () => { downloads++; if (fail) throw new Error('disk full'); },
    install: () => { installs++; },
  });
  await updater.check();
  assert.equal(updater.state.phase, 'error');
  assert.equal(installs, 0);
  fail = false;
  await updater.check();
  assert.equal(updater.state.phase, 'ready');
  assert.equal(downloads, 2);
  updater.setGuard('a', { activeCall: true, unsavedDraft: false });
  assert.equal(await updater.quit(true), false);
  assert.equal(installs, 0);
  updater.setGuard('a', { activeCall: false, unsavedDraft: true });
  assert.equal(await updater.quit(true), false);
  updater.setGuard('a', { activeCall: false, unsavedDraft: false });
  assert.equal(await updater.quit(false), false);
  assert.equal(installs, 0);
  assert.equal(await updater.quit(true), true);
  assert.equal(installs, 1);
});

test('checking or downloading is single flight and a missing guard is unsafe', async () => {
  let complete!: (value: Release) => void;
  let checks = 0;
  const updater = new UpdateCoordinator({ check: () => { checks++; return new Promise(resolve => { complete = resolve; }); }, download: async () => {}, install: () => {} });
  updater.expectGuard('instance');
  const check = updater.check();
  await updater.check();
  assert.equal(checks, 1);
  complete(release);
  await check;
  assert.equal(await updater.quit(true), false);
  updater.setGuard('instance', { activeCall: false, unsavedDraft: false });
  assert.equal(await updater.quit(true), true);
  updater.cancelledQuit();
  assert.equal(updater.state.phase, 'error');
  const retry = updater.check();
  complete(release);
  await retry;
  assert.equal(updater.state.phase, 'ready');
});

test('an installer failure becomes retryable and never consumes the working version', async () => {
  let fail = true;
  const updater = new UpdateCoordinator({ check: async () => release, download: async () => {}, install: () => { if (fail) throw new Error('replacement failed'); } });
  await updater.check();
  assert.equal(await updater.quit(true), false);
  assert.deepEqual(updater.state, { phase: 'error', message: 'replacement failed' });
  fail = false;
  await updater.check();
  assert.equal(await updater.quit(true), true);
});
