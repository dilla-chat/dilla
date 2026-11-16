// Tests for scripts/desktop-gpui-pages.mjs (GPUI release plan, Task 10). The live site is a fake fetch; every key is a
// throwaway generated here.
import assert from 'node:assert/strict';
import { generateKeyPairSync, sign } from 'node:crypto';
import { mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { after, test } from 'node:test';

import { assetNames, build as buildFeed, parseTrust } from './desktop-gpui-feed.mjs';
import { BETA_FILES, BETA5_SPARKLE_KEY, SITE, build, checkBeta, gpuiFiles, verify } from './desktop-gpui-pages.mjs';

const NOW = Date.parse('2026-11-16T00:00:00.000Z');
const ZIP = 'https://github.com/dilla-chat/dilla/releases/download/desktop-v0.1.0-beta.5/dilla-0.1.0-beta.5-mac-arm64.zip';

const temps = [];
after(async () => { for (const dir of temps) await rm(dir, { recursive: true, force: true }); });
async function tempDir() {
  const dir = await mkdtemp(join(tmpdir(), 'gpui-pages-'));
  temps.push(dir);
  return dir;
}

const metadata = generateKeyPairSync('ed25519');
const sparkle = generateKeyPairSync('ed25519');
const sparkleKey = Buffer.from(sparkle.publicKey.export({ format: 'jwk' }).x, 'base64url').toString('base64');
const trust = parseTrust(JSON.stringify({ metadataKeys: [metadata.publicKey.export({ type: 'spki', format: 'pem' })], channel: 'alpha' }));

/** beta.5-shaped live files: a pretty-printed signed feed and a Sparkle-signed appcast. */
function betaFiles({ publishedAt = '2026-11-12T03:10:00.359Z', expiresAt = '2027-02-10T03:10:00.359Z', channel = 'beta', key = metadata.privateKey, appcastUrl = ZIP } = {}) {
  const sparkleSignature = Buffer.alloc(64, 7).toString('base64');
  const doc = {
    format: 1, version: '0.1.0-beta.5', channel, publishedAt, expiresAt,
    assets: [{ name: 'dilla-0.1.0-beta.5-mac-arm64.zip', size: 1234, sha512: Buffer.alloc(64, 1).toString('base64') }],
    artifacts: [{ platform: 'darwin', arch: 'arm64', url: ZIP, size: 1234, sha512: Buffer.alloc(64, 1).toString('base64'), sparkleSignature }],
  };
  const json = Buffer.from(JSON.stringify(doc, null, 2) + '\n');
  const sig = Buffer.from(sign(null, json, key).toString('base64') + '\n');
  const body = Buffer.from('<?xml version="1.0" encoding="utf-8"?>\n<rss version="2.0" xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle"><channel><title>Dilla desktop beta</title><item><title>Dilla 0.1.0-beta.5</title><sparkle:version>0.1.0b5</sparkle:version><sparkle:shortVersionString>0.1.0-beta.5</sparkle:shortVersionString>'
    + `<enclosure url="${appcastUrl}" sparkle:edSignature="${sparkleSignature}" length="1234" type="application/octet-stream" /></item></channel></rss>\n`);
  const appcast = Buffer.concat([body, Buffer.from(`<!-- sparkle-signatures:\nedSignature: ${sign(null, body, sparkle.privateKey).toString('base64')}\nlength: ${body.length}\n-->\n`)]);
  return new Map([[BETA_FILES[0], json], [BETA_FILES[1], sig], [BETA_FILES[2], appcast]]);
}

/** A fake live site serving `files` (path -> bytes, or a status number); records the requests. */
function fakeSite(files) {
  const requests = [];
  const fetchImpl = async (href, options) => {
    requests.push({ href, options });
    const url = new URL(href);
    assert.equal(url.origin + '/dilla/', SITE);
    assert.ok(url.searchParams.has('t'), 'every request busts the cache');
    const entry = files.get(url.pathname.slice('/dilla/'.length));
    if (typeof entry === 'number') return new Response('error', { status: entry });
    if (!entry) return new Response('not found', { status: 404 });
    return new Response(entry, { status: 200 });
  };
  return { fetchImpl, requests };
}

async function signedDir() {
  const assets = await tempDir();
  for (const name of assetNames('0.2.0-alpha.1')) await writeFile(join(assets, name), `asset ${name}\n`);
  const { json, sig } = await buildFeed(assets, '0.2.0-alpha.1', 'alpha', { privateKey: metadata.privateKey, trust, now: new Date(NOW) });
  const dir = await tempDir();
  await writeFile(join(dir, 'desktop-gpui-alpha.json'), json);
  await writeFile(join(dir, 'desktop-gpui-alpha.json.sig'), sig);
  return { dir, json, sig };
}

const options = (fetchImpl) => ({ trust, fetchImpl, now: NOW, sparkleKey });

test('the beta file list is beta.5 pages-feed.mjs channelFiles("beta") and the Sparkle pin is beta.5\'s', () => {
  assert.deepEqual(BETA_FILES, ['beta/desktop-beta.json', 'beta/desktop-beta.json.sig', 'beta/appcast-arm64.xml']);
  assert.equal(Buffer.from(BETA5_SPARKLE_KEY, 'base64').length, 32);
  assert.deepEqual(gpuiFiles('alpha'), ['gpui-alpha/desktop-gpui-alpha.json', 'gpui-alpha/desktop-gpui-alpha.json.sig']);
});

test('build copies the live beta files byte for byte and adds the signed GPUI feed', async () => {
  const beta = betaFiles();
  const { fetchImpl, requests } = fakeSite(beta);
  const signed = await signedDir();
  const site = join(await tempDir(), 'site');
  const result = await build(signed.dir, site, options(fetchImpl));
  assert.deepEqual(result.beta, { version: '0.1.0-beta.5', expiresAt: '2027-02-10T03:10:00.359Z', expired: false });
  assert.deepEqual(result.gpui, { channel: 'alpha', version: '0.2.0-alpha.1' });
  assert.deepEqual(result.files.sort(), [...BETA_FILES, ...gpuiFiles('alpha')].sort());
  for (const [path, bytes] of beta) assert.ok((await readFile(join(site, path))).equals(bytes), path);
  assert.ok((await readFile(join(site, 'gpui-alpha/desktop-gpui-alpha.json'))).equals(signed.json));
  assert.ok((await readFile(join(site, 'gpui-alpha/desktop-gpui-alpha.json.sig'))).equals(signed.sig));
  assert.deepEqual((await readdir(site)).sort(), ['beta', 'gpui-alpha']);
  assert.equal(requests.length, 3);
  for (const { options: init } of requests) {
    assert.equal(init.redirect, 'error');
    assert.equal(init.cache, 'no-store');
  }
});

async function refuses(files, pattern, { signed } = {}) {
  const { fetchImpl } = fakeSite(files);
  const site = join(await tempDir(), 'site');
  await assert.rejects(build((signed ?? (await signedDir())).dir, site, options(fetchImpl)), pattern);
  await assert.rejects(readdir(site), { code: 'ENOENT' }, 'nothing is written on refusal');
}

test('build refuses a tampered beta feed, signature or appcast', async () => {
  const flip = (path, edit) => { const files = betaFiles(); files.set(path, edit(Buffer.from(files.get(path)))); return files; };
  await refuses(flip(BETA_FILES[0], (b) => Buffer.from(b.toString().replace('0.1.0-beta.5', '0.1.0-beta.6'))), /signature does not verify/);
  await refuses(flip(BETA_FILES[0], (b) => Buffer.concat([b, Buffer.from(' ')])), /signature does not verify/);
  await refuses(flip(BETA_FILES[1], (b) => { b[3] = b[3] === 65 ? 66 : 65; return b; }), /signature does not verify|malformed/);
  await refuses(flip(BETA_FILES[1], () => Buffer.from('not a signature\n')), /malformed beta feed signature/);
  await refuses(flip(BETA_FILES[2], (b) => Buffer.from(b.toString().replace('length="1234"', 'length="1235"'))), /Sparkle signature/);
  await refuses(flip(BETA_FILES[2], (b) => Buffer.from(b.toString().replace(/<!-- sparkle-signatures:[\s\S]*$/, ''))), /not signed/);
});

test('build refuses a beta feed signed by another key or for another channel', async () => {
  await refuses(betaFiles({ key: generateKeyPairSync('ed25519').privateKey }), /signature does not verify/);
  await refuses(betaFiles({ channel: 'alpha' }), /not a beta release/);
  await refuses(betaFiles({ appcastUrl: 'https://example.invalid/evil.zip' }), /does not point at the signed feed's macOS update/);
  await refuses(betaFiles({ publishedAt: '2026-12-01T00:00:00.000Z', expiresAt: '2027-01-01T00:00:00.000Z' }), /dates are invalid/);
});

test('build reports an expired but authentic beta feed and keeps its bytes', async () => {
  const beta = betaFiles({ publishedAt: '2026-08-01T00:00:00.000Z', expiresAt: '2026-10-30T00:00:00.000Z' });
  const { fetchImpl } = fakeSite(beta);
  const site = join(await tempDir(), 'site');
  const result = await build((await signedDir()).dir, site, options(fetchImpl));
  assert.deepEqual(result.beta, { version: '0.1.0-beta.5', expiresAt: '2026-10-30T00:00:00.000Z', expired: true });
  for (const [path, bytes] of beta) assert.ok((await readFile(join(site, path))).equals(bytes), path);
});

test('build refuses a missing or unreadable live file', async () => {
  for (const path of BETA_FILES) {
    const files = betaFiles();
    files.delete(path);
    await refuses(files, new RegExp(`does not serve ${path}`));
  }
  const failing = betaFiles();
  failing.set(BETA_FILES[2], 503);
  await refuses(failing, /HTTP 503/);
});

test('build refuses when the fetch fails (a redirect is an error)', async () => {
  const site = join(await tempDir(), 'site');
  const fetchImpl = async () => { throw new TypeError('fetch failed: redirect mode is set to error'); };
  await assert.rejects(build((await signedDir()).dir, site, options(fetchImpl)), /cannot read beta\/desktop-beta.json from the live site: fetch failed/);
});

test('build refuses a missing or tampered signed GPUI feed and a non-empty site directory', async () => {
  const empty = await tempDir();
  await refuses(betaFiles(), /lacks desktop-gpui-alpha.json/, { signed: { dir: empty } });
  const signed = await signedDir();
  const json = await readFile(join(signed.dir, 'desktop-gpui-alpha.json'));
  await writeFile(join(signed.dir, 'desktop-gpui-alpha.json'), Buffer.from(json.toString().replace('0.2.0-alpha.1', '0.2.0-alpha.2')));
  await refuses(betaFiles(), /signature does not verify/, { signed });

  const site = await tempDir();
  await writeFile(join(site, 'stale'), 'x');
  const { fetchImpl } = fakeSite(betaFiles());
  await assert.rejects(build((await signedDir()).dir, site, options(fetchImpl)), /not empty/);
});

test('checkBeta needs every file and a pinned key', () => {
  const files = betaFiles();
  assert.throws(() => checkBeta(files, { keys: [], sparkleKey, now: NOW }), /no pinned metadata key/);
  files.delete(BETA_FILES[2]);
  assert.throws(() => checkBeta(files, { keys: trust.keys, sparkleKey, now: NOW }), /missing beta\/appcast-arm64.xml/);
});

test('verify passes once the live site serves the built site, and fails while it does not', async () => {
  const beta = betaFiles();
  const signed = await signedDir();
  const site = join(await tempDir(), 'site');
  await build(signed.dir, site, options(fakeSite(beta).fetchImpl));

  const live = new Map(beta);
  live.set('gpui-alpha/desktop-gpui-alpha.json', signed.json);
  live.set('gpui-alpha/desktop-gpui-alpha.json.sig', signed.sig);
  let sleeps = 0;
  const report = await verify(signed.dir, { siteDir: site, trust, sparkleKey, fetchImpl: fakeSite(live).fetchImpl, sleep: async () => { sleeps++; } });
  assert.equal(report.version, '0.1.0-beta.5');
  assert.equal(sleeps, 0);

  // Not deployed yet (the GPUI feed is missing), then deployed: verify waits.
  let calls = 0;
  const pending = fakeSite(live).fetchImpl;
  const delayed = async (href, init) => (++calls <= 5 && href.includes('gpui-alpha') ? new Response('', { status: 404 }) : pending(href, init));
  await verify(signed.dir, { siteDir: site, trust, sparkleKey, fetchImpl: delayed, sleep: async () => { sleeps++; } });
  assert.equal(sleeps, 1);

  const stale = new Map(live);
  stale.set('gpui-alpha/desktop-gpui-alpha.json.sig', Buffer.from('stale\n'));
  await assert.rejects(verify(signed.dir, { siteDir: site, trust, sparkleKey, fetchImpl: fakeSite(stale).fetchImpl, attempts: 3, sleep: async () => {} }), /does not serve the built gpui-alpha\/desktop-gpui-alpha.json.sig/);

  const defaced = new Map(live);
  defaced.set(BETA_FILES[0], Buffer.concat([beta.get(BETA_FILES[0]), Buffer.from('\n')]));
  await assert.rejects(verify(signed.dir, { siteDir: site, trust, sparkleKey, fetchImpl: fakeSite(defaced).fetchImpl, attempts: 2, sleep: async () => {} }), /signature does not verify/);
});
