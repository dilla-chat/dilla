import { test } from 'node:test';
import assert from 'node:assert/strict';
import { generateKeyPairSync, createHash, sign } from 'node:crypto';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
// @ts-expect-error publishing tooling is JavaScript
import { appcast, signedAppcast } from '../scripts/feed-format.mjs';
// @ts-expect-error publishing tooling is JavaScript
import { verifyDesktopBundle, TARGETS } from '../scripts/verify-release.mjs';

test('publication refuses missing native acceptance even when the generic CI status is green', async () => {
 const directory = await mkdtemp(join(tmpdir(), 'dilla-publish-'));
 try {
  const key = generateKeyPairSync('ed25519'), sparkle = generateKeyPairSync('ed25519'), version = '1.2.3';
  const source = { format: 1, ciRun: 10, commit: '1'.repeat(40), head: '2'.repeat(40), tree: '3'.repeat(40), review: { status: 'approved', tree: '3'.repeat(40), sha256: '' }, performance: {} as Record<string, { sha256: string }>, notes: { sha256: '' } };
  const notes = Buffer.from('Dilla desktop 1.2.3'); source.notes.sha256 = createHash('sha256').update(notes).digest('hex'); await writeFile(join(directory, 'release-notes.md'), notes);
  const review = Buffer.from('Independent completed-app security review'); source.review.sha256 = createHash('sha256').update(review).digest('hex'); await writeFile(join(directory, 'security-review.md'), review);
  const jobs: { name: string; conclusion: string; steps?: { name: string; conclusion: string }[] }[] = [];
  for (const target of TARGETS) {
   jobs.push({ name: `desktop / desktop-${target}`, conclusion: 'success', steps: ['Installed upgrade acceptance', 'Application audio encryption and exclusion', 'Desktop performance comparison', 'Installed unsigned Sparkle upgrade and signed-feed rejection'].map(name => ({ name, conclusion: 'success' })) });
   for (let n = 1; n <= 3; n++) jobs.push({ name: `desktop / desktop-client-${target}-${n}`, conclusion: 'success' });
   jobs.push({ name: `desktop / desktop-voice-${target}`, conclusion: 'success' });
   const report = Buffer.from(JSON.stringify({ format: 1, target, sourceCommit: source.commit, status: 'measured', ...Object.fromEntries(['startup', 'memory', 'idleCPU', 'interaction', 'callAudio'].map(metric => [metric, [{ browser: 1, desktop: 2 }]])) }));
   await writeFile(join(directory, `performance-${target}.json`), report); source.performance[target] = { sha256: createHash('sha256').update(report).digest('hex') };
  }
  const assets = [];
  for (const name of [`dilla-${version}-linux-x64.AppImage`, `dilla-${version}-win-x64.exe`, ...['x64', 'arm64'].flatMap(arch => [`dilla-${version}-mac-${arch}.zip`, `dilla-${version}-mac-${arch}.dmg`])]) {
   const bytes = Buffer.from(name); await writeFile(join(directory, name), bytes); assets.push({ name, size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64') });
  }
  const release = { format: 1, version, channel: 'stable', publishedAt: '2026-11-01T00:00:00Z', expiresAt: '2026-12-01T00:00:00Z', source, assets, artifacts: assets.filter(asset => !asset.name.endsWith('.dmg')).map(asset => ({ ...asset, platform: asset.name.includes('-linux-') ? 'linux' : asset.name.includes('-win-') ? 'win32' : 'darwin', arch: asset.name.includes('arm64') ? 'arm64' : 'x64', url: `https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/${asset.name}`, ...(asset.name.endsWith('.zip') ? { sparkleSignature: sign(null, Buffer.from(asset.name), sparkle.privateKey).toString('base64') } : {}) })) };
  for (const artifact of release.artifacts.filter(a => a.platform === 'darwin')) await writeFile(join(directory, `appcast-${artifact.arch}.xml`), signedAppcast(appcast(release, artifact, artifact.sparkleSignature), sparkle.privateKey));
  const metadata = Buffer.from(JSON.stringify(release)), signature = sign(null, metadata, key.privateKey).toString('base64');
  const context = { metadata, signature, trust: { metadataPublicKeys: [key.publicKey.export({ type: 'spki', format: 'pem' }).toString()], sparklePublicKey: sparkle.publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64') }, directory, run: { id: 10, head_sha: source.head, name: 'ci', status: 'completed', conclusion: 'success', repository: { full_name: 'jonasthim/dilla' }, head_repository: { full_name: 'jonasthim/dilla' } }, jobs, version, now: Date.parse('2026-11-02') };
  assert.equal((await verifyDesktopBundle(context)).version, version);
  await assert.rejects(verifyDesktopBundle({ ...context, jobs: jobs.filter(job => !job.name.endsWith('desktop-voice-win-x64')) }), /Missing Electron voice/);
  await assert.rejects(verifyDesktopBundle({ ...context, jobs: jobs.filter(job => !job.name.endsWith('desktop-client-mac-arm64-3')) }), /Missing Electron/);
  const noAudio = structuredClone(jobs); noAudio[0].steps = noAudio[0].steps!.filter(step => !step.name.includes('audio encryption'));
  await assert.rejects(verifyDesktopBundle({ ...context, jobs: noAudio }), /missing accepted Application audio/);
  const originalFeed = await import('node:fs/promises').then(fs => fs.readFile(join(directory, 'appcast-x64.xml')));
  await writeFile(join(directory, 'appcast-x64.xml'), 'tampered feed');
  await assert.rejects(verifyDesktopBundle(context), /Unsigned Sparkle/);
  await writeFile(join(directory, 'appcast-x64.xml'), originalFeed);
  await writeFile(join(directory, 'release-notes.md'), 'tampered release notes');
  await assert.rejects(verifyDesktopBundle(context), /Release notes/);
  await writeFile(join(directory, 'release-notes.md'), notes);
  await writeFile(join(directory, assets[0].name), 'tampered');
  await assert.rejects(verifyDesktopBundle(context), /artifact digest/);
 } finally { await rm(directory, { recursive: true, force: true }); }
});
