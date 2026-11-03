import { sandboxAssetRecords } from './support/linux-sandbox-release-fixture.ts';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash, generateKeyPairSync, sign } from 'node:crypto';
import { verifyRelease, verifyArtifact, UpdateCoordinator, desktopReleasePage, type Release } from '../src/update.ts';

const pair = generateKeyPairSync('ed25519');
const publicKey = pair.publicKey.export({ type: 'spki', format: 'pem' }).toString();
const bytes = Buffer.from('an update artifact');
const release: Release = {
  assets:sandboxAssetRecords('0.1.1'), format: 1, version: '0.1.1', channel: 'stable', publishedAt: '2026-10-30T00:00:00.000Z', expiresAt: '2026-11-30T00:00:00.000Z',
  artifacts: [{ platform: 'linux', arch: 'x64', url: 'https://github.com/jonasthim/dilla/releases/download/desktop-v0.1.1/dilla-0.1.1-linux-x64.AppImage', size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64'), linuxSandbox:{compatibilityEpoch:1,minimumGeneration:1,bundle:{url:'https://github.com/jonasthim/dilla/releases/download/desktop-v0.1.1/dilla-0.1.1-linux-x64.sandbox',size:100,sha512:Buffer.alloc(64).toString('base64')}} }],
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
    { ...release, publishedAt: '2026-02-30T00:00:00Z' }, { ...release, expiresAt: '2026-10-30T00:00:00.000Z' }, { ...release, publishedAt: '2026-12-30T00:00:00.000Z' },
    { ...release, artifacts: [{ ...release.artifacts[0], url: 'https://evil.test/update.AppImage' }] },
    { ...release, artifacts: [{ ...release.artifacts[0], url: 'https://github.com/jonasthim/dilla/releases/download/desktop-v0.1.1/../evil.exe' }] },
    { ...release, artifacts: [{ ...release.artifacts[0], sha512: 'bad' }] },
    { ...release, artifacts: [{ ...release.artifacts[0], arbitrary: true }] },
    { ...release, artifacts: [{ ...release.artifacts[0], url:'https://github.com/jonasthim/dilla/releases/download/desktop-v0.1.1/other.AppImage' }] },
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

test('prerequisite authorization runs only for an unguarded actual update and fresh guards can veto installation', async () => {
 let preparations = 0, installs = 0;
 const updater = new UpdateCoordinator({ check: async () => release, download: async () => {}, preparePrerequisites: async () => { preparations++; }, install: () => { installs++; } });
 await updater.check();
 updater.expectGuard('instance');
 assert.equal(await updater.preparePrerequisites(), false);
 assert.equal(preparations, 0);
 updater.setGuard('instance', { activeCall: false, unsavedDraft: false });
 assert.equal(await updater.preparePrerequisites(), true);
 updater.expectGuard('instance');
 assert.equal(await updater.quit(true), false);
 assert.equal(installs, 0);
 updater.setGuard('instance', { activeCall: true, unsavedDraft: false });
 assert.equal(await updater.quit(true), false);
 assert.equal(installs, 0);
});

test('cancelled prerequisite authorization retains the working app and is retryable', async () => {
 const updater = new UpdateCoordinator({ check: async () => release, download: async () => {}, preparePrerequisites: async () => { throw Error('Authorization cancelled'); }, install: () => { throw Error('must not install'); } });
 await updater.check();
 assert.equal(await updater.preparePrerequisites(), false);
 assert.deepEqual(updater.state, { phase: 'error', message: 'Authorization cancelled' });
});

test('macOS metadata authenticates the exact Sparkle archive signature', () => {
 const artifact = { ...release.artifacts[0], linuxSandbox: undefined, platform: 'darwin', url: 'https://github.com/jonasthim/dilla/releases/download/desktop-v0.1.1/dilla-0.1.1-mac-arm64.zip' };
 const missing = signed({ ...release, artifacts: [artifact] });
 assert.throws(() => verifyRelease(missing.body, missing.signature, { ...context, platform: 'darwin' }));
 const valid = signed({ ...release, artifacts: [{ ...artifact, sparkleSignature: Buffer.alloc(64).toString('base64') }] });
 assert.equal(verifyRelease(valid.body, valid.signature, { ...context, platform: 'darwin' }).artifacts[0].sparkleSignature, Buffer.alloc(64).toString('base64'));
});

test('package manager updates expose authenticated availability without downloading or installing', async () => {
 let downloads = 0, installs = 0;
 const phases: string[] = [];
 const updater = new UpdateCoordinator({
  ownership: 'package-manager', check: async () => release,
  download: async () => { downloads++; }, install: () => { installs++; },
 }, state => phases.push(state.phase));
 await updater.check();
 assert.deepEqual(updater.state, { phase: 'available', version: '0.1.1', message: 'Dilla 0.1.1 is available. Update using your package manager.' });
 assert.deepEqual(phases, ['checking', 'available']);
 assert.equal(downloads, 0);
 updater.setGuard('a', { activeCall: false, unsavedDraft: false });
 assert.equal(await updater.quit(true), false);
 assert.equal(installs, 0);
 await updater.check();
 assert.equal(updater.state.phase, 'available');
 assert.equal(downloads, 0);
});

test('package manager availability rejects malformed release versions and supports idle rechecks', async () => {
 let candidate: Release | null = { ...release, version: '0.1.1\n' };
 const updater = new UpdateCoordinator({ ownership: 'package-manager', check: async () => candidate, download: async () => { throw Error('download forbidden'); }, install: () => { throw Error('install forbidden'); } });
 await updater.check(); assert.equal(updater.state.phase, 'error');
 candidate = null; await updater.check(); assert.deepEqual(updater.state, { phase: 'idle' });
});

test('release pages use only validated full upstream versions and the fixed repository', () => {
 assert.equal(desktopReleasePage('0.1.0-beta.10'), 'https://github.com/jonasthim/dilla/releases/tag/desktop-v0.1.0-beta.10');
 for (const value of ['0.1.0\n', 'https://evil.test', '../latest', '0.1.0+build', '9007199254740992.0.0']) assert.throws(() => desktopReleasePage(value));
});

test('Linux descriptor pins sandbox epoch, generation and exact versioned capsule URL', () => {
 const asset = release.artifacts[0];
 const sandbox = {compatibilityEpoch:1, minimumGeneration:3, bundle:{url:'https://github.com/jonasthim/dilla/releases/download/desktop-v0.1.1/dilla-0.1.1-linux-x64.sandbox',size:100,sha512:Buffer.alloc(64).toString('base64')}};
 const check = (v: unknown) => { const doc=signed({...release,artifacts:[{...asset,linuxSandbox:v}]}); return verifyRelease(doc.body,doc.signature,context); };
 assert.equal(check(sandbox).artifacts.length,1);
 for (const bad of [undefined,{...sandbox,compatibilityEpoch:2},{...sandbox,minimumGeneration:0},{...sandbox,bundle:{...sandbox.bundle,url:sandbox.bundle.url+'.json'}},{...sandbox,bundle:{...sandbox.bundle,size:48*1024**2+1}},{...sandbox,extra:true}]) assert.throws(()=>check(bad));
});

test('Linux release requires signed fixed sidecar asset records matching its capsule descriptor', () => {
 const asset=release.artifacts[0], capsule=asset.linuxSandbox!.bundle;
 const assets=[{name:'dilla-0.1.1-linux-x64.sandbox',size:capsule.size,sha512:capsule.sha512},{name:'dilla-0.1.1-linux-x64.sandbox.json',size:20,sha512:Buffer.alloc(64).toString('base64')},{name:'dilla-0.1.1-linux-x64.sandbox.sig',size:89,sha512:Buffer.alloc(64).toString('base64')}];
 const check=(records:unknown)=>{const doc=signed({...release,assets:records}); return verifyRelease(doc.body,doc.signature,context);};
 assert.equal(check(assets).version,'0.1.1');
 for (const bad of [[],assets.slice(0,2),[...assets,assets[0]],assets.map((a,i)=>i===0?{...a,sha512:Buffer.alloc(64,1).toString('base64')}:a),assets.map((a,i)=>i===1?{...a,size:65537}:a)]) assert.throws(()=>check(bad));
});

test('signed release rejects duplicate and noncanonical JSON fields', () => {
 const raw=JSON.stringify(release), duplicate=Buffer.from(raw.replace('"compatibilityEpoch":1','"compatibilityEpoch":1,"compatibilityEpoch":1'));
 const number=Buffer.from(raw.replace('"minimumGeneration":1','"minimumGeneration":1e0'));
 for (const bytes of [duplicate,number]) assert.throws(()=>verifyRelease(bytes,sign(null,bytes,pair.privateKey).toString('base64'),context));
});

test('signed release dates require real UTC calendar timestamps', () => {
 const doc=signed({...release,publishedAt:'2026-02-30T00:00:00Z',expiresAt:'2026-03-10T00:00:00Z'});
 assert.throws(()=>verifyRelease(doc.body,doc.signature,{...context,now:Date.parse('2026-03-03T00:00:00Z')}));
});
