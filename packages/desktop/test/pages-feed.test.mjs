import { test } from 'node:test';
import assert from 'node:assert/strict';
import { generateKeyPairSync, sign } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { FEED_SITE, authenticateChannel, channelFiles, feedUrl, buildSite, trustAtTag, verifyLiveFeed } from '../scripts/pages-feed.mjs';
import { appcast, signedAppcast } from '../scripts/feed-format.mjs';
import { createGithubApi } from '../scripts/ci-run.mjs';
import { buildReleaseFixture } from './support/release-fixture.mjs';
import { fakeGithub } from './support/fake-github.mjs';

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

/** Signed channel files (metadata, signature, appcast) for any version, under the fixture's keys or rotated ones. */
function signedChannel(f, version, { publishedAt = '2026-11-01T00:00:00Z', expiresAt = '2026-12-01T00:00:00Z', keys = { metadata: f.key.privateKey, sparkle: f.sparkle.privateKey } } = {}) {
 const text = JSON.stringify(f.release).replaceAll(`dilla-${f.version}-`, `dilla-${version}-`).replaceAll(`desktop-v${f.version}/`, `desktop-v${version}/`);
 const release = { ...JSON.parse(text), version, channel: version.includes('-beta.') ? 'beta' : 'stable', publishedAt, expiresAt };
 const metadata = Buffer.from(JSON.stringify(release));
 const darwin = release.artifacts.find(artifact => artifact.platform === 'darwin');
 const [json, sig, feed] = channelFiles(release.channel);
 return new Map([[json, metadata], [sig, Buffer.from(sign(null, metadata, keys.metadata).toString('base64') + '\n')],
  [feed, signedAppcast(appcast(release, darwin, darwin.sparkleSignature), keys.sparkle)]]);
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

/** A published, immutable versioned release carrying the channel files as assets (plus an installer). */
let sandbox; // the current setup's sandbox fixture: its manifest and detached signature are release assets
const release = (version, files, fields = {}) => ({ tag_name: `desktop-v${version}`, draft: false, prerelease: version.includes('-beta.'), immutable: true,
 files: { ...Object.fromEntries(files), [`dilla-${version}-win-x64.exe`]: Buffer.from('installer'),
  [`dilla-${version}-linux-x64.sandbox.json`]: sandbox.manifest, [`dilla-${version}-linux-x64.sandbox.sig`]: sandbox.signature, ...fields.extraFiles }, ...fields });

async function setup({ releases = s => [release('1.2.3', s.signedFiles)] } = {}) {
 const root = await mkdtemp(join(tmpdir(), 'dilla-pages-feed-'));
 const f = await buildReleaseFixture({ directory: await mkdir(join(root, 'fixture')).then(() => join(root, 'fixture')) });
 sandbox = f.sandbox;
 const signed = join(root, 'signed'); await mkdir(signed);
 const signedFiles = signedChannel(f, '1.2.3');
 for (const [name, bytes] of signedFiles) await writeFile(join(signed, name), bytes);
 const s = { root, f, signed, signedFiles };
 const github = fakeGithub({ releases: releases(s) });
 const trustCalls = [];
 const trustAt = async tag => { trustCalls.push(tag); return { trust: f.trust, commit: f.source.commit }; };
 const build = (options = {}) => buildSite({ signedDirectory: signed, channel: 'stable', version: '1.2.3', siteDirectory: join(root, `site-${Math.random()}`),
  api: createGithubApi({ token: 'workflow-token', fetchImpl: options.fetchImpl ?? github.fetch }), fetchImpl: github.fetch, trust: f.trust, sandboxKeys: f.sandbox.config.publicKeys, trustAt, now: f.context.now, ...options });
 return { ...s, github, trustAt, trustCalls, build, cleanup: () => rm(root, { recursive: true, force: true }) };
}
const tree = async directory => (await readdir(directory, { recursive: true })).sort();

test('feed build serves the signed channel files and the other channel from its latest published release', async () => {
 const s = await setup();
 try {
  const first = await s.build();
  assert.deepEqual(await tree(first.siteDirectory), ['stable', 'stable/appcast-arm64.xml', 'stable/desktop-stable.json', 'stable/desktop-stable.json.sig']);
  for (const name of channelFiles('stable')) assert.deepEqual(await readFile(join(first.siteDirectory, 'stable', name)), await readFile(join(s.signed, name)));
  const beta = signedChannel(s.f, '1.3.0-beta.2');
  s.github.addRelease(release('1.3.0-beta.1', signedChannel(s.f, '1.3.0-beta.1')));
  s.github.addRelease(release('1.3.0-beta.2', beta));
  const second = await s.build();
  assert.deepEqual(second.channels, { stable: '1.2.3', beta: '1.3.0-beta.2' });
  for (const [name, bytes] of beta) assert.deepEqual(await readFile(join(second.siteDirectory, 'beta', name)), bytes, 'the other channel is re-served byte for byte');
  assert.deepEqual(s.trustCalls, ['desktop-v1.3.0-beta.2'], 'the other channel is verified under the pins of the release that carries it');
 } finally { await s.cleanup(); }
});

test('feed build refuses signed channel files that do not verify under the pins', async () => {
 const s = await setup();
 try {
  await writeFile(join(s.signed, 'desktop-stable.json.sig'), Buffer.alloc(64, 1).toString('base64') + '\n');
  await assert.rejects(s.build(), /signature/i);
  for (const [name, bytes] of s.signedFiles) await writeFile(join(s.signed, name), bytes);
  await assert.rejects(s.build({ version: '1.2.4' }), /version/i);
  await writeFile(join(s.signed, 'appcast-arm64.xml'), 'unsigned');
  await assert.rejects(s.build(), /Sparkle/);
 } finally { await s.cleanup(); }
});

test('feed build refuses a tampered or incomplete other-channel release and drops an expired one', async () => {
 const beta = f => signedChannel(f, '1.3.0-beta.2');
 const cases = [
  [/Sparkle/, s => release('1.3.0-beta.2', new Map([...beta(s.f), ['appcast-arm64.xml', Buffer.from('tampered')]]))],
  [/signature/i, s => release('1.3.0-beta.2', new Map([...beta(s.f), ['desktop-beta.json', Buffer.from(beta(s.f).get('desktop-beta.json').toString().replace('1.3.0-beta.2', '1.3.0-beta.9'))]]))],
  [/lacks/i, s => release('1.3.0-beta.2', new Map([...beta(s.f)].filter(([name]) => name !== 'appcast-arm64.xml')))],
  [/immutable/i, s => release('1.3.0-beta.2', beta(s.f), { immutable: false })],
 ];
 for (const [error, other] of cases) {
  const s = await setup({ releases: s => [release('1.2.3', s.signedFiles), other(s)] });
  try { await assert.rejects(s.build(), error); } finally { await s.cleanup(); }
 }
 const s = await setup({ releases: s => [release('1.2.3', s.signedFiles), release('1.3.0-beta.2', signedChannel(s.f, '1.3.0-beta.2', { publishedAt: '2026-08-01T00:00:00Z', expiresAt: '2026-10-01T00:00:00Z' }))] });
 try {
  const result = await s.build();
  assert.deepEqual(result.channels, { stable: '1.2.3' });
  assert.deepEqual(result.dropped, ['beta']);
 } finally { await s.cleanup(); }
});

test('the forward-only baseline is the channel’s latest published immutable release', async () => {
 const refused = [
  [/newer/i, s => [release('1.2.3', s.signedFiles), release('1.2.4', signedChannel(s.f, '1.2.4'))]],
  [/not published/i, s => [release('1.2.2', signedChannel(s.f, '1.2.2'))]],
  [/not published/i, s => [release('1.2.3', s.signedFiles, { draft: true })]],
  [/differ/i, s => [release('1.2.3', signedChannel(s.f, '1.2.3', { publishedAt: '2026-11-01T00:00:01Z' }))]],
  [/immutable/i, s => [release('1.2.3', s.signedFiles, { immutable: false })]],
 ];
 for (const [error, releases] of refused) {
  const s = await setup({ releases });
  try { await assert.rejects(s.build(), error); } finally { await s.cleanup(); }
 }
 // A re-run over its own release, with older releases of both channels around, is accepted.
 const s = await setup({ releases: s => [release('1.2.2', signedChannel(s.f, '1.2.2')), release('1.2.3', s.signedFiles), release('1.2.10-beta.1', signedChannel(s.f, '1.2.10-beta.1'))] });
 try {
  await s.build();
  assert.deepEqual((await s.build()).channels, { stable: '1.2.3', beta: '1.2.10-beta.1' });
 } finally { await s.cleanup(); }
});

test('promote, then release: releases signed under the previous keys do not block the feed', async () => {
 // Main's pins after `rotate --promote` and `--retire`: new metadata and Sparkle keys only.
 const s = await setup({ releases: () => [] });
 try {
  const metadata = generateKeyPairSync('ed25519'), sparkle = generateKeyPairSync('ed25519');
  const promoted = { ...s.f.trust, metadataPublicKeys: [metadata.publicKey.export({ type: 'spki', format: 'pem' }).toString()],
   sparklePublicKey: sparkle.publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64') };
  const current = signedChannel(s.f, '1.2.3', { keys: { metadata: metadata.privateKey, sparkle: sparkle.privateKey } });
  for (const [name, bytes] of current) await writeFile(join(s.signed, name), bytes);
  const oldBeta = signedChannel(s.f, '1.3.0-beta.2'), oldStable = signedChannel(s.f, '1.2.2');
  s.github.addRelease(release('1.2.2', oldStable)); s.github.addRelease(release('1.3.0-beta.2', oldBeta)); s.github.addRelease(release('1.2.3', current));
  assert.throws(() => authenticateChannel(oldBeta, 'beta', promoted, s.f.context.now), 'the old beta feed does not verify under main’s promoted pins');
  // The live site still serves the transition release, signed by the old keys: it is never read.
  const live = pages(); live.publish('stable', oldStable); live.publish('beta', oldBeta);
  const pins = { 'desktop-v1.3.0-beta.2': { trust: s.f.trust, commit: s.f.source.commit } };
  const fetchImpl = (input, init) => String(input).startsWith(FEED_SITE) ? live.fetchImpl(input, init) : s.github.fetch(input, init);
  const result = await s.build({ trust: promoted, trustAt: async tag => pins[tag], fetchImpl });
  // The release goes out; the beta feed, signed by keys main no longer pins, is not re-served (a retired key never authenticates again).
  assert.deepEqual(result.channels, { stable: '1.2.3' });
  assert.deepEqual(result.retired, ['beta']);
  assert.deepEqual(await tree(result.siteDirectory), ['stable', 'stable/appcast-arm64.xml', 'stable/desktop-stable.json', 'stable/desktop-stable.json.sig']);
  assert.equal(live.log.length, 0);
 } finally { await s.cleanup(); }
});

test('during a staged rotation, the other channel signed by the still-pinned keys is re-served', async () => {
 const s = await setup({ releases: s => [release('1.2.3', s.signedFiles), release('1.3.0-beta.2', signedChannel(s.f, '1.3.0-beta.2'))] });
 try {
  const next = generateKeyPairSync('ed25519'), nextSparkle = generateKeyPairSync('ed25519');
  // Main's pins at the transition: both metadata keys, the Sparkle key unchanged plus the staged next one.
  const staged = { ...s.f.trust, metadataPublicKeys: [...s.f.trust.metadataPublicKeys, next.publicKey.export({ type: 'spki', format: 'pem' }).toString()],
   sparkleNextPublicKey: nextSparkle.publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64') };
  const result = await s.build({ trust: staged });
  assert.deepEqual(result.channels, { stable: '1.2.3', beta: '1.3.0-beta.2' });
  assert.deepEqual(result.retired, []);
 } finally { await s.cleanup(); }
});

test('an other-channel release signed under a retired metadata, Sparkle or sandbox key is not re-served', async () => {
 const other = generateKeyPairSync('ed25519');
 const raw = key => key.publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
 const cases = {
  // The release's pins held the key; main's current pins no longer do.
  // Main pins only `other` for that role; this release is signed accordingly, the beta release by the old keys.
  metadata: s => ({ trust: { ...s.f.trust, metadataPublicKeys: [other.publicKey.export({ type: 'spki', format: 'pem' }).toString()] }, keys: { metadata: other.privateKey, sparkle: s.f.sparkle.privateKey } }),
  sparkle: s => ({ trust: { ...s.f.trust, sparklePublicKey: raw(other) }, keys: { metadata: s.f.key.privateKey, sparkle: other.privateKey } }),
  sandbox: () => ({ sandboxKeys: [raw(other)] }),
 };
 for (const [label, change] of Object.entries(cases)) {
  const s = await setup({ releases: s => [release('1.2.3', s.signedFiles), release('1.3.0-beta.2', signedChannel(s.f, '1.3.0-beta.2'))] });
  try {
   const { trust, keys, sandboxKeys } = change(s);
   if (keys) {
    const current = signedChannel(s.f, '1.2.3', { keys });
    for (const [name, bytes] of current) await writeFile(join(s.signed, name), bytes);
    for (const asset of s.github.state.releases.find(item => item.tag_name === 'desktop-v1.2.3').assets) {
     if (current.has(asset.name)) Object.assign(asset, { bytes: current.get(asset.name), size: current.get(asset.name).length, digest: s.github.sha256(current.get(asset.name)) });
    }
   }
   const result = await s.build({ ...(trust ? { trust } : {}), ...(sandboxKeys ? { sandboxKeys } : {}) });
   assert.deepEqual(result.channels, { stable: '1.2.3' }, label);
   assert.deepEqual(result.retired, ['beta'], label);
  } finally { await s.cleanup(); }
 }
});

test('only releases the release workflow made are followed: a hand-made release or a foreign source commit is refused', async () => {
 const person = { login: 'someone', type: 'User' };
 for (const [error, releases] of [
  [/release workflow/i, s => [release('1.2.3', s.signedFiles), release('1.3.0-beta.2', signedChannel(s.f, '1.3.0-beta.2'), { author: person })]],
  [/release workflow/i, s => [release('1.2.3', s.signedFiles, { author: person })]],
  [/release workflow/i, s => [release('1.2.3', s.signedFiles), release('1.3.0-beta.2', signedChannel(s.f, '1.3.0-beta.2'), { author: { login: 'github-actions[bot]', type: 'User' } })]],
 ]) {
  const s = await setup({ releases });
  try { await assert.rejects(s.build(), error); } finally { await s.cleanup(); }
 }
 const s = await setup({ releases: s => [release('1.2.3', s.signedFiles), release('1.3.0-beta.2', signedChannel(s.f, '1.3.0-beta.2'))] });
 try {
  await assert.rejects(s.build({ trustAt: async () => ({ trust: s.f.trust, commit: 'f'.repeat(40) }) }), /source commit/i);
 } finally { await s.cleanup(); }
});

test('a defaced live site is ignored: the build never reads Pages', async () => {
 const s = await setup({ releases: s => [release('1.2.3', s.signedFiles), release('1.3.0-beta.2', signedChannel(s.f, '1.3.0-beta.2'))] });
 try {
  const seen = [];
  const fetchImpl = async (input, init) => {
   seen.push(String(input));
   if (String(input).startsWith(FEED_SITE)) return new Response('<h1>defaced</h1>', { status: 200 });
   return s.github.fetch(input, init);
  };
  const result = await s.build({ fetchImpl });
  assert.deepEqual(result.channels, { stable: '1.2.3', beta: '1.3.0-beta.2' });
  assert.ok(seen.length > 0 && seen.every(url => !url.startsWith(FEED_SITE)), 'no request to the live site');
 } finally { await s.cleanup(); }
});

test('feed fetches stay on the API and the release download hosts, and never follow API redirects', async () => {
 const s = await setup({ releases: s => [release('1.2.3', s.signedFiles), release('1.3.0-beta.2', signedChannel(s.f, '1.3.0-beta.2'))] });
 try {
  await s.build();
  const api = s.github.state.log.filter(entry => entry.url.startsWith('https://api.github.com/'));
  assert.ok(api.length >= 3 && api.every(entry => entry.redirect === 'error' && entry.method === 'GET'));
  const downloads = s.github.state.log.filter(entry => !entry.url.startsWith('https://api.github.com/'));
  assert.equal(downloads.length, 8, 'three channel files each, plus the other release\'s sandbox manifest and signature');
  for (const entry of downloads) assert.match(entry.url, /^https:\/\/github\.com\/dilla-chat\/dilla\/releases\/download\/desktop-v[^/]+\/[^/]+$/);
  const elsewhere = async (input, init) => {
   const response = await s.github.fetch(input, init);
   if (!String(input).startsWith('https://github.com/')) return response;
   const moved = new Response(await response.arrayBuffer(), { status: response.status });
   Object.defineProperty(moved, 'url', { value: 'https://evil.example/x' });
   return moved;
  };
  await assert.rejects(s.build({ fetchImpl: elsewhere }), /unexpected host/i);
 } finally { await s.cleanup(); }
});

test('the pins of a published release are release-trust.json at its tag’s commit, which must be on main', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-pages-trust-'));
 try {
  const git = args => execFileSync('git', ['-c', 'core.hooksPath=/dev/null', ...args], { cwd: root, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();
  git(['init', '-q', '-b', 'main']);
  for (const [key, value] of [['user.email', 'test@example.invalid'], ['user.name', 'test'], ['commit.gpgsign', 'false']]) git(['config', key, value]);
  await mkdir(join(root, 'packages/desktop'), { recursive: true });
  await writeFile(join(root, 'packages/desktop/release-trust.json'), JSON.stringify({ sparklePublicKey: 'old' }));
  git(['add', '-A']); git(['commit', '-q', '-m', 'old pins']); const old = git(['rev-parse', 'HEAD']);
  await writeFile(join(root, 'packages/desktop/release-trust.json'), JSON.stringify({ sparklePublicKey: 'new' }));
  git(['commit', '-q', '-am', 'promote']); git(['update-ref', 'refs/remotes/origin/main', 'HEAD']);
  git(['checkout', '-q', '-b', 'side', old]); git(['commit', '-q', '--allow-empty', '-m', 'off main']); const side = git(['rev-parse', 'HEAD']);
  const github = fakeGithub({ tags: { 'desktop-v1.3.0-beta.2': old, 'desktop-v1.3.0-beta.3': side } });
  const trustAt = trustAtTag({ api: createGithubApi({ token: 't', fetchImpl: github.fetch }), git });
  assert.deepEqual(await trustAt('desktop-v1.3.0-beta.2'), { trust: { sparklePublicKey: 'old' }, commit: old });
  await assert.rejects(trustAt('desktop-v1.3.0-beta.3'), /not on main/);
  await assert.rejects(trustAt('v1.0.0'), /Invalid/);
 } finally { await rm(root, { recursive: true, force: true }); }
});

test('live feed verification retries until the deployed bytes equal the signed files', async () => {
 const s = await setup();
 try {
  const live = pages();
  let calls = 0;
  const sleep = async () => { calls++; if (calls === 2) live.publish('stable', signedChannel(s.f, '1.2.3')); };
  await verifyLiveFeed({ signedDirectory: s.signed, channel: 'stable', fetchImpl: live.fetchImpl, attempts: 5, sleep });
  assert.equal(calls, 2);
  for (const entry of live.log) { assert.equal(entry.redirect, 'error'); assert.ok(entry.url.startsWith(FEED_SITE), entry.url); }
  live.site.set(feedUrl('stable', 'appcast-arm64.xml'), Buffer.from('stale'));
  await assert.rejects(verifyLiveFeed({ signedDirectory: s.signed, channel: 'stable', fetchImpl: live.fetchImpl, attempts: 3, sleep: async () => {} }), /does not serve/i);
 } finally { await s.cleanup(); }
});
