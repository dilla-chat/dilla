// Tests for scripts/desktop-gpui-feed.mjs (GPUI release plan, Task 9).
//
// The Go cross-test fixture in internal/desktopdownload/testdata/gpui-feed is signed by a THROWAWAY test key
// (throwaway-test-key.pem, never a release key). Ed25519 is deterministic, so the fixture is rebuilt here from fixed
// inputs and must match the committed bytes. Regenerate it with:
//   DILLA_WRITE_GPUI_FEED_FIXTURE=1 node --test scripts/desktop-gpui-feed.test.mjs
import assert from 'node:assert/strict';
import { createPrivateKey, createPublicKey, generateKeyPairSync, sign } from 'node:crypto';
import { mkdtemp, readFile, rm, writeFile, symlink } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { after, test } from 'node:test';

import {
  assetNames, build, buildCommand, canonical, checkChannel, feedName, feedUrl, loadTrust, parseTrust, privateKeyFrom, verify,
} from './desktop-gpui-feed.mjs';

const ROOT = new URL('..', import.meta.url).pathname;
const SCRIPT = join(ROOT, 'scripts/desktop-gpui-feed.mjs');
const FIXTURE = join(ROOT, 'internal/desktopdownload/testdata/gpui-feed');
const FIXTURE_VERSION = '0.2.0-alpha.1';
const FIXTURE_NOW = new Date('2026-11-16T00:00:00.000Z');
const KEYS_GO = join(ROOT, 'internal/desktopdownload/keys.go');

const temps = [];
after(async () => { for (const dir of temps) await rm(dir, { recursive: true, force: true }); });
async function tempDir() {
  const dir = await mkdtemp(join(tmpdir(), 'gpui-feed-'));
  temps.push(dir);
  return dir;
}

function testKey() {
  const { privateKey, publicKey } = generateKeyPairSync('ed25519');
  const pem = publicKey.export({ type: 'spki', format: 'pem' });
  return { privateKey, trust: parseTrust(JSON.stringify({ metadataKeys: [pem], channel: 'alpha' })) };
}

async function assetsDir(version = FIXTURE_VERSION, extra = {}) {
  const dir = await tempDir();
  for (const name of assetNames(version)) await writeFile(join(dir, name), `fixture asset ${name}\n`);
  for (const [name, body] of Object.entries(extra)) await writeFile(join(dir, name), body);
  return dir;
}

// Re-signs an edited document with the same key, to test the checks after the signature.
function resign(doc, privateKey, text = JSON.stringify(doc) + '\n') {
  const json = Buffer.from(text);
  return { json, sig: Buffer.from(sign(null, json, privateKey).toString('base64') + '\n') };
}

test('release-trust.json pins exactly key A from keys.go and the alpha channel', async () => {
  const trust = await loadTrust();
  assert.equal(trust.channel, 'alpha');
  const goLiterals = [...(await readFile(KEYS_GO, 'utf8')).matchAll(/"(-----BEGIN PUBLIC KEY-----\\n[^"]+)"/g)]
    .map((m) => m[1].replaceAll('\\n', '\n'));
  assert.equal(goLiterals.length, 1);
  assert.deepEqual(trust.metadataKeys, goLiterals);
});

test('build writes the canonical format-1 document and a signature that verifies', async () => {
  const { privateKey, trust } = testKey();
  const dir = await assetsDir(FIXTURE_VERSION, { SHA256SUMS: 'not listed\n' });
  const { json, sig } = await build(dir, FIXTURE_VERSION, 'alpha', { privateKey, trust, now: FIXTURE_NOW });
  const doc = JSON.parse(json);
  assert.deepEqual(Object.keys(doc), ['format', 'channel', 'version', 'publishedAt', 'expiresAt', 'assets']);
  assert.equal(doc.format, 1);
  assert.equal(doc.channel, 'alpha');
  assert.equal(doc.publishedAt, '2026-11-16T00:00:00.000Z');
  assert.equal(doc.expiresAt, '2027-02-14T00:00:00.000Z');
  assert.deepEqual(doc.assets.map((a) => a.name), assetNames(FIXTURE_VERSION));
  assert.ok(!doc.assets.some((a) => a.name === 'SHA256SUMS'));
  assert.equal(json.toString(), JSON.stringify(doc) + '\n');
  assert.match(sig.toString(), /^[A-Za-z0-9+/]{86}==\n$/);
  assert.equal(verify(json, sig, trust, { now: FIXTURE_NOW }).version, FIXTURE_VERSION);
});

test('build refuses the beta channel, a channel the trust does not pin, and a non-channel version', async () => {
  const { privateKey, trust } = testKey();
  const dir = await assetsDir();
  await assert.rejects(build(dir, '0.2.0-beta.1', 'beta', { privateKey, trust }), /never the beta channel/);
  const betaTrust = { ...trust, channel: 'beta' };
  await assert.rejects(build(dir, '0.2.0-beta.1', 'beta', { privateKey, trust: betaTrust }), /never the beta channel/);
  await assert.rejects(build(dir, '0.2.0-stable.1', 'stable', { privateKey, trust }), /not the pinned channel/);
  await assert.rejects(build(dir, '0.2.0-beta.1', 'alpha', { privateKey, trust }), /not a alpha release/);
  await assert.rejects(build(dir, '0.2.0', 'alpha', { privateKey, trust }), /not a alpha release/);
  assert.throws(() => parseTrust(JSON.stringify({ metadataKeys: trust.metadataKeys, channel: 'beta' })), /never the beta channel/);
});

test('build refuses an unpinned signer, a missing, empty or symlinked asset', async () => {
  const { privateKey, trust } = testKey();
  const other = testKey();
  const dir = await assetsDir();
  await assert.rejects(build(dir, FIXTURE_VERSION, 'alpha', { privateKey: other.privateKey, trust }), /not a pinned metadata key/);
  const names = assetNames(FIXTURE_VERSION);
  const missing = await assetsDir();
  await rm(join(missing, names[5]));
  await assert.rejects(build(missing, FIXTURE_VERSION, 'alpha', { privateKey, trust }), /missing or not a regular file/);
  const empty = await assetsDir();
  await writeFile(join(empty, names[0]), '');
  await assert.rejects(build(empty, FIXTURE_VERSION, 'alpha', { privateKey, trust }), /empty/);
  const linked = await assetsDir();
  await rm(join(linked, names[7]));
  await symlink(join(linked, names[6]), join(linked, names[7]));
  await assert.rejects(build(linked, FIXTURE_VERSION, 'alpha', { privateKey, trust }), /not a regular file/);
});

test('verify checks the signature before parsing and refuses tampering and other keys', async () => {
  const { privateKey, trust } = testKey();
  const { json, sig } = await build(await assetsDir(), FIXTURE_VERSION, 'alpha', { privateKey, trust, now: FIXTURE_NOW });
  const opts = { now: FIXTURE_NOW };
  const tampered = Buffer.from(json.toString().replace('"size":', '"size": '));
  assert.throws(() => verify(tampered, sig, trust, opts), /does not verify/);
  assert.throws(() => verify(Buffer.from('not json\n'), sig, trust, opts), /does not verify/);
  assert.throws(() => verify(json, sig, testKey().trust, opts), /does not verify/);
  assert.throws(() => verify(json, Buffer.from('AAAA\n'), trust, opts), /malformed/);
  assert.throws(() => verify(json, Buffer.from(sig.toString() + '\n'), trust, opts), /malformed/);
  assert.equal(verify(json, Buffer.from(sig.toString().trimEnd()), trust, opts).version, FIXTURE_VERSION);
});

test('verify refuses a channel mismatch in both directions and never accepts beta', async () => {
  const { privateKey, trust } = testKey();
  const { json, sig } = await build(await assetsDir(), FIXTURE_VERSION, 'alpha', { privateKey, trust, now: FIXTURE_NOW });
  // An alpha feed is refused where another channel is expected, and beta is never an accepted expectation.
  assert.throws(() => verify(json, sig, trust, { channel: 'stable', now: FIXTURE_NOW }), /channel "alpha" is not stable/);
  assert.throws(() => verify(json, sig, trust, { channel: 'beta', now: FIXTURE_NOW }), /never the beta channel/);
  // A beta.5-shaped document signed by the same key is refused by the GPUI verifier.
  const doc = JSON.parse(json);
  const beta = resign({ ...doc, channel: 'beta', version: '0.1.0-beta.6' }, privateKey);
  assert.throws(() => verify(beta.json, beta.sig, trust, { now: FIXTURE_NOW }), /channel "beta" is not alpha/);
});

test('verify refuses a malformed document even when it is signed', async () => {
  const { privateKey, trust } = testKey();
  const { json } = await build(await assetsDir(), FIXTURE_VERSION, 'alpha', { privateKey, trust, now: FIXTURE_NOW });
  const doc = JSON.parse(json);
  const now = { now: FIXTURE_NOW };
  const cases = [
    [{ ...doc, format: 2 }, /format is not 1/],
    [{ ...doc, version: '0.2.0-beta.1' }, /not a alpha release/],
    [{ ...doc, extra: true }, /format-1 fields/],
    [{ format: 1, version: doc.version, channel: doc.channel, publishedAt: doc.publishedAt, expiresAt: doc.expiresAt, assets: doc.assets }, /format-1 fields/],
    [{ ...doc, expiresAt: '2026-11-15T00:00:00.000Z' }, /expired or its dates/],
    [{ ...doc, expiresAt: '2027-03-01T00:00:00.000Z' }, /expired or its dates/],
    [{ ...doc, publishedAt: '2026-11-17T00:00:00.000Z' }, /expired or its dates/],
    [{ ...doc, publishedAt: '2026-11-16T00:00:00+00:00' }, /UTC timestamp/],
    [{ ...doc, assets: doc.assets.slice(1) }, /exactly once/],
    [{ ...doc, assets: [doc.assets[1], doc.assets[0], ...doc.assets.slice(2)] }, /asset 0 is/],
    [{ ...doc, assets: doc.assets.map((a, i) => (i ? a : { ...a, size: 0 })) }, /size is invalid/],
    [{ ...doc, assets: doc.assets.map((a, i) => (i ? a : { ...a, sha512: 'x' })) }, /sha512 is invalid/],
    [{ ...doc, assets: doc.assets.map((a, i) => (i ? a : { ...a, url: 'https://evil' })) }, /name, size and sha512/],
  ];
  for (const [edited, error] of cases) {
    const signed = resign(edited, privateKey);
    assert.throws(() => verify(signed.json, signed.sig, trust, now), error, JSON.stringify(edited).slice(0, 80));
  }
  const pretty = resign(doc, privateKey, JSON.stringify(doc, null, 2) + '\n');
  assert.throws(() => verify(pretty.json, pretty.sig, trust, now), /not canonical/);
  // An expired feed is refused at a later time.
  const { json: ok, sig: okSig } = await build(await assetsDir(), FIXTURE_VERSION, 'alpha', { privateKey, trust, now: FIXTURE_NOW });
  assert.throws(() => verify(ok, okSig, trust, { now: new Date('2027-02-14T00:00:00.000Z') }), /expired/);
});

test('privateKeyFrom reads beta.5\'s base64 PKCS8 DER and a PKCS8 PEM, and never echoes the value', () => {
  const { privateKey } = generateKeyPairSync('ed25519');
  const der = privateKey.export({ type: 'pkcs8', format: 'der' }).toString('base64');
  const pem = privateKey.export({ type: 'pkcs8', format: 'pem' });
  const raw = (k) => createPublicKey(k).export({ type: 'spki', format: 'der' }).toString('hex');
  assert.equal(raw(privateKeyFrom(der)), raw(privateKey));
  assert.equal(raw(privateKeyFrom(pem)), raw(privateKey));
  assert.throws(() => privateKeyFrom(undefined), /not set/);
  const secretish = 'A'.repeat(63) + '!';
  assert.throws(() => privateKeyFrom(secretish), (e) => !e.message.includes(secretish));
  const rsa = generateKeyPairSync('rsa', { modulusLength: 1024 }).privateKey.export({ type: 'pkcs8', format: 'pem' });
  assert.throws(() => privateKeyFrom(rsa), /not an Ed25519 key/);
});

test('the CLI builds from DESKTOP_METADATA_KEY_A and verifies against release-trust.json pins', async () => {
  const { privateKey, trust } = testKey();
  const out = await tempDir();
  const env = { DESKTOP_METADATA_KEY_A: privateKey.export({ type: 'pkcs8', format: 'der' }).toString('base64') };
  const file = await buildCommand(await assetsDir(), FIXTURE_VERSION, out, { env, trust, now: FIXTURE_NOW });
  assert.equal(file, join(out, 'desktop-gpui-alpha.json'));
  verify(await readFile(file), await readFile(file + '.sig'), trust, { now: FIXTURE_NOW });
  // Never overwrites an existing feed.
  await assert.rejects(buildCommand(await assetsDir(), FIXTURE_VERSION, out, { env, trust, now: FIXTURE_NOW }), /EEXIST/);
  // The real CLI with a key the real trust does not pin refuses to sign.
  const cli = spawnSync(process.execPath, [SCRIPT, 'build', await assetsDir(), FIXTURE_VERSION, await tempDir()], { env: { ...process.env, ...env }, encoding: 'utf8' });
  assert.equal(cli.status, 1);
  assert.match(cli.stderr, /not a pinned metadata key/);
  assert.ok(!cli.stderr.includes(env.DESKTOP_METADATA_KEY_A));
  // The real CLI's verify uses the real pins, which the throwaway fixture key is not.
  const verifyCli = spawnSync(process.execPath, [SCRIPT, 'verify', file, file + '.sig'], { encoding: 'utf8' });
  assert.equal(verifyCli.status, 1);
  assert.match(verifyCli.stderr, /does not verify under the pinned keys/);
  const usage = spawnSync(process.execPath, [SCRIPT, 'sign'], { encoding: 'utf8' });
  assert.equal(usage.status, 2);
});

test('the feed lives at gpui-alpha/desktop-gpui-alpha.json', () => {
  assert.equal(feedName('alpha'), 'desktop-gpui-alpha.json');
  assert.equal(feedUrl('alpha'), 'https://dilla-chat.github.io/dilla/gpui-alpha/desktop-gpui-alpha.json');
  assert.throws(() => feedUrl('beta'), /never the beta channel/);
  assert.throws(() => checkChannel('Alpha'), /invalid feed channel/);
});

test('the Go cross-test fixture is this script\'s output under the throwaway test key', async () => {
  const privateKey = createPrivateKey(await readFile(join(FIXTURE, 'throwaway-test-key.pem')));
  const pub = await readFile(join(FIXTURE, 'throwaway-test-key.pub.pem'), 'utf8');
  assert.equal(createPublicKey(privateKey).export({ type: 'spki', format: 'pem' }), pub);
  const trust = parseTrust(JSON.stringify({ metadataKeys: [pub], channel: 'alpha' }));
  // Never the release key.
  assert.ok(!(await loadTrust()).metadataKeys.includes(pub));
  const { json, sig } = await build(await assetsDir(), FIXTURE_VERSION, 'alpha', { privateKey, trust, now: FIXTURE_NOW });
  const jsonPath = join(FIXTURE, feedName('alpha'));
  if (process.env.DILLA_WRITE_GPUI_FEED_FIXTURE === '1') {
    await writeFile(jsonPath, json);
    await writeFile(jsonPath + '.sig', sig);
  }
  assert.deepEqual(await readFile(jsonPath), json);
  assert.deepEqual(await readFile(jsonPath + '.sig'), sig);
  assert.deepEqual(canonical(JSON.parse(json)), json);
});
