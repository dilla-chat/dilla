import { test } from 'node:test';
import assert from 'node:assert/strict';
import { runPublisher } from '../scripts/publish-release.mjs';
import { verifyPublished } from '../scripts/verify-published.mjs';
import { publishFixture } from './support/publish-fixture.mjs';

/** A versioned release published by the real publisher into the in-memory GitHub. */
async function published(options) {
 const p = await publishFixture(options);
 await runPublisher({ mode: 'publish', env: p.env, git: p.git, fetchImpl: p.github.fetch, trust: p.fixture.trust, verifier: p.tools.verifier, now: p.now });
 const release = p.github.state.releases.find(item => item.tag_name === p.tag);
 const verify = (env = {}) => verifyPublished({ env: { ...p.env, ...env }, fetchImpl: p.github.fetch, trust: p.fixture.trust, verifier: p.tools.verifier, now: p.now });
 return { ...p, release, verify };
}
const asset = (p, name) => p.release.assets.find(item => item.name === name);

test('verify-published passes for exactly the signed inventory', async () => {
 const p = await published();
 try {
  const before = p.github.state.log.length;
  const result = await p.verify();
  assert.equal(result.version, p.version);
  assert.equal(result.assets, p.release.assets.length);
  const log = p.github.state.log.slice(before);
  assert.ok(log.every(entry => entry.method === 'GET'), 'verification never writes');
  assert.equal(log.filter(entry => entry.url.startsWith('https://github.com/')).length, p.release.assets.length, 'every asset is re-downloaded');
  await assert.rejects(p.verify({ GITHUB_EVENT_NAME: 'push' }), /main-branch workflow_run/);
  await assert.rejects(p.verify({ DESKTOP_SHA: 'c'.repeat(40) }), /tag.*DESKTOP_SHA/i);
  p.release.immutable = false;
  await assert.rejects(p.verify(), /immutable/i);
 } finally { await p.cleanup(); }
});

test('verify-published fails on an extra asset, a missing asset and a digest mismatch', async () => {
 const p = await published();
 try {
  const extra = p.github.addAsset(p.release, `dilla-${p.version}-win-x64-setup.exe`, Buffer.from('extra'));
  await assert.rejects(p.verify(), /asset set/i);
  p.release.assets = p.release.assets.filter(item => item !== extra);
  const removed = p.release.assets.pop();
  await assert.rejects(p.verify(), /asset set/i);
  p.release.assets.push(removed);
  await p.verify();
  // Bytes that differ from what the API reports, and API-consistent bytes that differ from the signed metadata.
  const exe = asset(p, `dilla-${p.version}-win-x64.exe`), clean = exe.bytes;
  exe.bytes = Buffer.from('tampered installer');
  await assert.rejects(p.verify(), /digest/i);
  Object.assign(exe, { size: exe.bytes.length, digest: p.github.sha256(exe.bytes) });
  await assert.rejects(p.verify(), /signed metadata/i);
  Object.assign(exe, { bytes: clean, size: clean.length, digest: p.github.sha256(clean) });
  const notes = asset(p, 'release-notes.md');
  notes.bytes = Buffer.from('phishing notes'); Object.assign(notes, { size: notes.bytes.length, digest: p.github.sha256(notes.bytes) });
  await assert.rejects(p.verify(), /release-notes\.md/);
 } finally { await p.cleanup(); }
});

test('verify-published checks the feed assets against the signed metadata', async () => {
 const p = await published();
 try {
  const metadata = asset(p, `desktop-${p.channel}.json`), signature = asset(p, `desktop-${p.channel}.json.sig`), feed = asset(p, 'appcast-arm64.xml');
  const original = { metadata: metadata.bytes, signature: signature.bytes, feed: feed.bytes };
  const set = (item, bytes) => Object.assign(item, { bytes, size: bytes.length, digest: p.github.sha256(bytes) });
  set(signature, Buffer.from(Buffer.alloc(64, 1).toString('base64') + '\n'));
  await assert.rejects(p.verify(), /signature/i);
  set(signature, original.signature);
  set(feed, Buffer.from(original.feed.toString().replace('<title>Dilla desktop', '<title>Evil desktop')));
  await assert.rejects(p.verify(), /Sparkle feed/i);
  set(feed, original.feed);
  // Metadata signed by a key that is not pinned.
  const { generateKeyPairSync, sign } = await import('node:crypto');
  const other = generateKeyPairSync('ed25519');
  set(signature, Buffer.from(sign(null, original.metadata, other.privateKey).toString('base64') + '\n'));
  await assert.rejects(p.verify(), /signature/i);
  set(signature, original.signature);
  await p.verify();
 } finally { await p.cleanup(); }
});

test('verify-published checks a published rotation proof under the pinned staged keys', async () => {
 const p = await published({ staged: true });
 try {
  assert.equal((await p.verify()).assets, p.release.assets.length);
  const proof = asset(p, 'rotation-proof.json'), clean = proof.bytes;
  const changed = JSON.parse(clean.toString()); changed.sandbox.signature = changed.metadata.signature;
  const bytes = Buffer.from(JSON.stringify(changed));
  Object.assign(proof, { bytes, size: bytes.length, digest: p.github.sha256(bytes) });
  await assert.rejects(p.verify(), /rotation proof/i);
  Object.assign(proof, { bytes: clean, size: clean.length, digest: p.github.sha256(clean) });
  await assert.rejects(p.verify({ DESKTOP_STAGED: 'false' }), /asset set/i, 'a proof present when nothing is staged');
 } finally { await p.cleanup(); }
});
