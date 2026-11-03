import { test } from 'node:test';
import assert from 'node:assert/strict';
import { sign } from 'node:crypto';
import { mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { FEED_SITE, channelFiles, feedUrl, buildSite, verifyLiveFeed } from '../scripts/pages-feed.mjs';
import { appcast, signedAppcast } from '../scripts/feed-format.mjs';
import { buildReleaseFixture } from './support/release-fixture.mjs';

const desktop = resolve(import.meta.dirname, '..');

test('the updater fetches the channel feed from GitHub Pages', async () => {
 const source = await readFile(join(desktop, 'src/updater.ts'), 'utf8');
 const lines = source.match(/const feed = `([^`]+)`;/g);
 assert.equal(lines?.length, 1, 'one feed URL boundary (the installed-account fixture replaces it)');
 const template = /`([^`]+)`/.exec(lines[0])[1];
 for (const channel of ['beta', 'stable']) assert.equal(template.replaceAll('${channel}', channel), feedUrl(channel, `desktop-${channel}.json`));
 assert.equal(FEED_SITE, 'https://dilla-chat.github.io/dilla/');
 assert.ok(!source.includes('releases/download/desktop-${channel}'), 'no feed release URL remains');
});

test('Sparkle reads the appcast from GitHub Pages', () => {
 const config = createRequire(import.meta.url)('../electron-builder.cjs');
 const channel = createRequire(import.meta.url)('../package.json').version.includes('-beta.') ? 'beta' : 'stable';
 assert.equal(config.mac.extendInfo.SUFeedURL, feedUrl(channel, `appcast-${process.arch}.xml`));
 assert.deepEqual(channelFiles(channel), [`desktop-${channel}.json`, `desktop-${channel}.json.sig`, 'appcast-arm64.xml']);
});

/** Signed channel files (metadata, signature, appcast) for any version, under one fixture's keys. */
function signedChannel(f, version, { publishedAt = '2026-11-01T00:00:00Z', expiresAt = '2026-12-01T00:00:00Z' } = {}) {
 const text = JSON.stringify(f.release).replaceAll(`dilla-${f.version}-`, `dilla-${version}-`).replaceAll(`desktop-v${f.version}/`, `desktop-v${version}/`);
 const release = { ...JSON.parse(text), version, channel: version.includes('-beta.') ? 'beta' : 'stable', publishedAt, expiresAt };
 const metadata = Buffer.from(JSON.stringify(release));
 const darwin = release.artifacts.find(artifact => artifact.platform === 'darwin');
 const [json, sig, feed] = channelFiles(release.channel);
 return new Map([[json, metadata], [sig, Buffer.from(sign(null, metadata, f.key.privateKey).toString('base64') + '\n')],
  [feed, signedAppcast(appcast(release, darwin, darwin.sparkleSignature), f.sparkle.privateKey)]]);
}

/** The live Pages site, as a fetch that serves a URL -> bytes map and logs every request. */
function pages(site = new Map()) {
 const log = [];
 const fetchImpl = async (input, init = {}) => {
  const url = new URL(String(input)); log.push({ url: url.href, redirect: init.redirect });
  const bytes = site.get(url.origin + url.pathname);
  return bytes ? new Response(bytes, { status: 200 }) : new Response('Not Found', { status: 404 });
 };
 const publish = (channel, files) => { for (const [name, bytes] of files) site.set(feedUrl(channel, name), bytes); };
 return { site, log, fetchImpl, publish };
}

async function setup() {
 const root = await mkdtemp(join(tmpdir(), 'dilla-pages-feed-'));
 const f = await buildReleaseFixture({ directory: await mkdir(join(root, 'fixture')).then(() => join(root, 'fixture')) });
 const signed = join(root, 'signed'); await mkdir(signed);
 for (const [name, bytes] of signedChannel(f, '1.2.3')) await writeFile(join(signed, name), bytes);
 const live = pages();
 const build = (options = {}) => buildSite({ signedDirectory: signed, channel: 'stable', version: '1.2.3', siteDirectory: join(root, `site-${Math.random()}`), fetchImpl: live.fetchImpl, trust: f.trust, now: f.now, ...options });
 return { root, f, signed, live, build, cleanup: () => rm(root, { recursive: true, force: true }) };
}
const tree = async directory => (await readdir(directory, { recursive: true })).sort();

test('feed build serves the signed channel files and re-serves the verified other channel', async () => {
 const s = await setup();
 try {
  const first = await s.build();
  assert.deepEqual(await tree(first.siteDirectory), ['stable', 'stable/appcast-arm64.xml', 'stable/desktop-stable.json', 'stable/desktop-stable.json.sig']);
  for (const name of channelFiles('stable')) assert.deepEqual(await readFile(join(first.siteDirectory, 'stable', name)), await readFile(join(s.signed, name)));
  const beta = signedChannel(s.f, '1.3.0-beta.2');
  s.live.publish('beta', beta);
  const second = await s.build();
  assert.deepEqual(second.channels, { stable: '1.2.3', beta: '1.3.0-beta.2' });
  for (const [name, bytes] of beta) assert.deepEqual(await readFile(join(second.siteDirectory, 'beta', name)), bytes, 'the other channel is re-served byte for byte');
 } finally { await s.cleanup(); }
});

test('feed build refuses signed channel files that do not verify under the pins', async () => {
 const s = await setup();
 try {
  await writeFile(join(s.signed, 'desktop-stable.json.sig'), Buffer.alloc(64, 1).toString('base64') + '\n');
  await assert.rejects(s.build(), /signature/i);
  for (const [name, bytes] of signedChannel(s.f, '1.2.3')) await writeFile(join(s.signed, name), bytes);
  await assert.rejects(s.build({ version: '1.2.4' }), /version/i);
  await writeFile(join(s.signed, 'appcast-arm64.xml'), 'unsigned');
  await assert.rejects(s.build(), /Sparkle/);
 } finally { await s.cleanup(); }
});

test('feed build refuses a tampered live other channel and drops an expired one', async () => {
 const s = await setup();
 try {
  const beta = signedChannel(s.f, '1.3.0-beta.2');
  s.live.publish('beta', new Map([...beta, ['appcast-arm64.xml', Buffer.from('tampered')]]));
  await assert.rejects(s.build(), /Sparkle/);
  s.live.publish('beta', new Map([...beta, ['desktop-beta.json', Buffer.from(beta.get('desktop-beta.json').toString().replace('1.3.0-beta.2', '1.3.0-beta.9'))]]));
  await assert.rejects(s.build(), /signature/i);
  s.live.site.delete(feedUrl('beta', 'appcast-arm64.xml')); s.live.publish('beta', new Map([['desktop-beta.json', beta.get('desktop-beta.json')], ['desktop-beta.json.sig', beta.get('desktop-beta.json.sig')]]));
  await assert.rejects(s.build(), /incomplete/i);
  s.live.publish('beta', signedChannel(s.f, '1.3.0-beta.2', { publishedAt: '2026-08-01T00:00:00Z', expiresAt: '2026-10-01T00:00:00Z' }));
  const result = await s.build();
  assert.deepEqual(result.channels, { stable: '1.2.3' });
  assert.deepEqual(result.dropped, ['beta']);
 } finally { await s.cleanup(); }
});

test('feed build refuses a live same-channel feed that is newer than the release', async () => {
 const s = await setup();
 try {
  s.live.publish('stable', signedChannel(s.f, '1.2.4'));
  await assert.rejects(s.build(), /newer/i);
  s.live.publish('stable', signedChannel(s.f, '1.2.3'));
  await s.build(); // a re-run over its own deployment
  s.live.publish('stable', signedChannel(s.f, '1.2.2'));
  await s.build();
 } finally { await s.cleanup(); }
});

test('feed fetches stay on the Pages origin and never follow redirects', async () => {
 const s = await setup();
 try {
  s.live.publish('beta', signedChannel(s.f, '1.3.0-beta.2'));
  await s.build();
  assert.ok(s.live.log.length >= 6);
  for (const entry of s.live.log) {
   assert.equal(entry.redirect, 'error');
   assert.ok(entry.url.startsWith(FEED_SITE), entry.url);
  }
  const failing = async () => new Response('', { status: 500 });
  await assert.rejects(s.build({ fetchImpl: failing }), /500/);
 } finally { await s.cleanup(); }
});

test('live feed verification retries until the deployed bytes equal the signed files', async () => {
 const s = await setup();
 try {
  let calls = 0;
  const sleep = async () => { calls++; if (calls === 2) s.live.publish('stable', signedChannel(s.f, '1.2.3')); };
  await verifyLiveFeed({ signedDirectory: s.signed, channel: 'stable', fetchImpl: s.live.fetchImpl, attempts: 5, sleep });
  assert.equal(calls, 2);
  s.live.site.set(feedUrl('stable', 'appcast-arm64.xml'), Buffer.from('stale'));
  await assert.rejects(verifyLiveFeed({ signedDirectory: s.signed, channel: 'stable', fetchImpl: s.live.fetchImpl, attempts: 3, sleep: async () => {} }), /does not serve/i);
 } finally { await s.cleanup(); }
});
