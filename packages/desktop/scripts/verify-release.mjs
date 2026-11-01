import { createHash, createPublicKey, verify } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { join, basename } from 'node:path';
import { appcast } from './feed-format.mjs';
import { verifyRelease, verifyArtifact } from '../src/update.ts';

export const TARGETS = ['linux-x64', 'win-x64', 'mac-x64', 'mac-arm64'];
const REQUIRED = ['Installed upgrade acceptance', 'Application audio encryption and exclusion', 'Desktop performance comparison'];
const digest = bytes => createHash('sha256').update(bytes).digest('hex');

/** Publication checks the complete accepted run, not just a green build or a
 * mutable release draft. Signing keys are not part of this process. */
export async function verifyDesktopBundle({ metadata, signature, trust, directory, run, jobs, version, feeds, now = Date.now() }) {
 const channel = version.includes('-beta.') ? 'beta' : 'stable';
 const release = verifyRelease(metadata, signature.trim(), { publicKeys: trust.metadataPublicKeys, channel, platform: 'linux', arch: 'x64', currentVersion: '0.0.0', now });
 if (release.version !== version) throw new Error('Release tag and authenticated version differ');
 const expectedTargets = ['linux-x64', 'win32-x64', 'darwin-x64', 'darwin-arm64'];
 if (release.artifacts.length !== 4 || expectedTargets.some(target => !release.artifacts.some(a => `${a.platform}-${a.arch}` === target))) throw new Error('All desktop platforms must release together');
 const source = release.source;
 if (!source || source.format !== 1 || !Number.isSafeInteger(source.ciRun) || source.ciRun < 1
   || ![source.commit, source.head, source.tree].every(value => typeof value === 'string' && /^[a-f0-9]{40}$/.test(value))) throw new Error('Missing authenticated acceptance source');
 if (run.id !== source.ciRun || run.head_sha !== source.head || run.name !== 'ci' || run.status !== 'completed' || run.conclusion !== 'success'
   || run.repository?.full_name !== 'jonasthim/dilla' || run.head_repository?.full_name !== 'jonasthim/dilla') throw new Error('The accepted same-repository CI run must be complete and successful');
 if (!Array.isArray(jobs)) throw new Error('Missing complete CI job inventory');
 for (const target of TARGETS) {
  const native = jobs.find(job => job.name.endsWith(`desktop-${target}`));
  if (!native || native.conclusion !== 'success') throw new Error('Missing accepted native target: ' + target);
  const gates = target.startsWith('mac-') ? [...REQUIRED, 'Installed unsigned Sparkle upgrade and signed-feed rejection'] : REQUIRED;
  for (const gate of gates) if (!native.steps?.some(step => step.name === gate && step.conclusion === 'success')) throw new Error(`${target}: missing accepted ${gate}`);
  for (let shard = 1; shard <= 3; shard++) {
   if (!jobs.some(job => job.name.endsWith(`desktop-client-${target}-${shard}`) && job.conclusion === 'success')) throw new Error(`Missing Electron client coverage: ${target}-${shard}`);
  }
  if (!jobs.some(job => job.name.endsWith(`desktop-voice-${target}`) && job.conclusion === 'success')) throw new Error(`Missing Electron voice coverage: ${target}`);
 }
 if (!source.review || source.review.status !== 'approved' || source.review.tree !== source.tree || !/^[a-f0-9]{64}$/.test(source.review.sha256)) throw new Error('An independent security review must approve this source tree');
 if (!source.notes || !/^[a-f0-9]{64}$/.test(source.notes.sha256) || digest(await readFile(join(directory, 'release-notes.md'))) !== source.notes.sha256) throw new Error('Release notes digest mismatch');
 const review = await readFile(join(directory, 'security-review.md'));
 if (digest(review) !== source.review.sha256) throw new Error('Security review digest mismatch');
 for (const target of TARGETS) {
  const record = source.performance?.[target];
  if (!record || !/^[a-f0-9]{64}$/.test(record.sha256)) throw new Error('Missing measured performance comparison: ' + target);
  const bytes = await readFile(join(directory, `performance-${target}.json`));
  if (digest(bytes) !== record.sha256) throw new Error('Performance report digest mismatch: ' + target);
  const report = JSON.parse(bytes);
  if (report.format !== 1 || report.target !== target || report.sourceCommit !== source.commit || report.status !== 'measured'
    || !['startup', 'memory', 'idleCPU', 'interaction', 'callAudio'].every(metric => Array.isArray(report[metric]) && report[metric].length > 0
      && report[metric].every(sample => Number.isFinite(sample.browser) && sample.browser >= 0 && Number.isFinite(sample.desktop) && sample.desktop >= 0))) throw new Error('Incomplete measured performance comparison: ' + target);
 }
 const assets = release.assets;
 const requiredNames = [
  `dilla-${version}-linux-x64.AppImage`, `dilla-${version}-win-x64.exe`,
  ...['x64', 'arm64'].flatMap(arch => [`dilla-${version}-mac-${arch}.zip`, `dilla-${version}-mac-${arch}.dmg`]),
 ];
 if (!Array.isArray(assets) || assets.length !== requiredNames.length || requiredNames.some(name => assets.filter(asset => asset.name === name).length !== 1)) throw new Error('All six installable/update artifacts must be authenticated');
 for (const asset of assets) {
  if (typeof asset.name !== 'string' || basename(asset.name) !== asset.name || !requiredNames.includes(asset.name)
    || !Number.isSafeInteger(asset.size) || asset.size < 1 || typeof asset.sha512 !== 'string' || !/^[A-Za-z0-9+/]{86}==$/.test(asset.sha512)) throw new Error('Invalid release asset');
  const bytes = await readFile(join(directory, asset.name));
  if (!verifyArtifact(bytes, asset)) throw new Error('Release artifact digest mismatch: ' + asset.name);
 }
 if (typeof trust.sparklePublicKey !== 'string' || Buffer.from(trust.sparklePublicKey, 'base64').length !== 32) throw new Error('Missing installed Sparkle trust');
 const sparkleKey = createPublicKey({ key: { kty: 'OKP', crv: 'Ed25519', x: Buffer.from(trust.sparklePublicKey, 'base64').toString('base64url') }, format: 'jwk' });
 for (const artifact of release.artifacts) {
  const asset = assets.find(asset => asset.name === new URL(artifact.url).pathname.split('/').at(-1));
  if (!asset || asset.sha512 !== artifact.sha512 || asset.size !== artifact.size) throw new Error('Update and installer descriptor mismatch');
  if (artifact.platform === 'darwin') {
   const archive = await readFile(join(directory, asset.name));
   if (!verify(null, archive, sparkleKey, Buffer.from(artifact.sparkleSignature, 'base64'))) throw new Error('Sparkle archive signature mismatch');
   const feed = feeds?.[artifact.arch] ?? await readFile(join(directory, `appcast-${artifact.arch}.xml`));
   const marker = Buffer.from('<!-- sparkle-signatures:\n'), boundary = feed.lastIndexOf(marker);
   if (boundary < 0) throw new Error('Unsigned Sparkle feed');
   const block = /^<!-- sparkle-signatures:\nedSignature: ([A-Za-z0-9+/]{86}==)\nlength: (\d+)\n-->\n$/.exec(feed.subarray(boundary).toString());
   if (!block || Number(block[2]) !== boundary || !verify(null, feed.subarray(0, boundary), sparkleKey, Buffer.from(block[1], 'base64'))
     || feed.subarray(0, boundary).toString() !== appcast(release, artifact, artifact.sparkleSignature)) throw new Error('Sparkle feed signature or authenticated target mismatch');
  }
 }
 return release;
}
