import { createHash, createPublicKey, verify } from 'node:crypto';
import { readFile, writeFile, lstat } from 'node:fs/promises';
import { join, basename } from 'node:path';
import { appcast } from './feed-format.mjs';
import { verifyRelease, verifyArtifact } from '../src/update.ts';
import { inspectLinuxSandboxCapsule, verifyLinuxSandboxRelease } from '../src/linux-sandbox-release.ts';
import { assertDedicatedKeys, parsePublicConfiguration } from './linux-sandbox-package.mjs';
import { verifyInstalledAccountReport } from './installed-account-proof.mjs';
import { verifyAurRecipe } from './build-native-linux-packages.mjs';
import { buildProvenanceName, readBuildProvenanceFile, verifyBuildProvenance, verifyCandidateBuildRecord } from './build-provenance.mjs';

export const TARGETS = ['linux-x64', 'win-x64', 'mac-arm64'];
export const BUILD_TARGETS = Object.freeze([...TARGETS, 'linux-packages-x64']);
export const BUILD_PROVENANCE_RECORDS = Object.freeze(BUILD_TARGETS.map(buildProvenanceName));
export const PUBLIC_ACCEPTANCE_RECORDS = Object.freeze(['release-notes.md', 'security-review.md',
 ...TARGETS.flatMap(target => [`performance-${target}.json`, `installed-account-${target}.json`]), ...BUILD_PROVENANCE_RECORDS]);
/** Shared by the offline signer and the online publisher. Only public,
 * authenticated acceptance records enter a candidate or versioned release. */
export function publicReleaseRecords(channel) {
 if (!['stable', 'beta'].includes(channel)) throw new Error('Invalid desktop publication channel');
 return [`desktop-${channel}.json`, `desktop-${channel}.json.sig`, ...PUBLIC_ACCEPTANCE_RECORDS, 'appcast-arm64.xml'];
}
/** The publisher downloads these independent records inside the accepted installer
 * artifacts. Verify their independent copies before staging candidate records;
 * the signed candidate may never create or overwrite native build provenance. */
export async function stagePublicReleaseRecords({ candidate, directory, channel, version }) {
 const records = publicReleaseRecords(channel);
 if (typeof version !== 'string' || !/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/.test(version)) throw new Error('Invalid sandbox signature version');
 const sandboxSignatureName = `dilla-${version}-linux-x64.sandbox.sig`;
 for (const name of BUILD_PROVENANCE_RECORDS) await verifyCandidateBuildRecord(join(candidate, name), join(directory, name));
 for (const name of [...records, sandboxSignatureName]) if (!BUILD_PROVENANCE_RECORDS.includes(name)) {
  await writeFile(join(directory, name), await readFile(join(candidate, name)), { flag: 'wx' });
 }
}
const digest = bytes => createHash('sha256').update(bytes).digest('hex');

/** Publication checks the complete accepted run, not just a green build or a
 * mutable release draft. Signing keys are not part of this process. */
const preSigning = Symbol('pre-signing acceptance only');
export async function verifyDesktopSigningCandidate(options) { return verifyBundle(options, preSigning); }
export async function verifyDesktopBundle(options) { return verifyBundle(options); }
async function verifyBundle({ metadata, signature, trust, directory, run, jobs, version, feeds, now = Date.now(), sandboxSignature }, mode) {
 const channel = version.includes('-beta.') ? 'beta' : 'stable';
 const release = verifyRelease(metadata, signature.trim(), { publicKeys: trust.metadataPublicKeys, channel, platform: 'linux', arch: 'x64', currentVersion: '0.0.0', now });
 if (release.version !== version) throw new Error('Release tag and authenticated version differ');
 verifyRelease(metadata, signature.trim(), { publicKeys: trust.metadataPublicKeys, channel, platform: 'win32', arch: 'x64', currentVersion: '0.0.0', now });
 const expectedTargets = ['linux-x64', 'win32-x64', 'darwin-arm64'];
 if (release.artifacts.length !== expectedTargets.length || expectedTargets.some(target => !release.artifacts.some(a => `${a.platform}-${a.arch}` === target))) throw new Error('All desktop platforms must release together');
 const source = release.source;
 if (!source || source.format !== 1 || !Number.isSafeInteger(source.ciRun) || source.ciRun < 1
   || ![source.commit, source.head, source.tree].every(value => typeof value === 'string' && /^[a-f0-9]{40}$/.test(value))) throw new Error('Missing authenticated acceptance source');
 if (run.id !== source.ciRun || run.head_sha !== source.head || run.name !== 'ci' || run.status !== 'completed' || run.conclusion !== 'success'
   || run.repository?.full_name !== 'jonasthim/dilla' || run.head_repository?.full_name !== 'jonasthim/dilla') throw new Error('The accepted same-repository CI run must be complete and successful');
 if (!Array.isArray(jobs)) throw new Error('Missing complete CI job inventory');
 const builds = source.builds;
 if (!builds || typeof builds !== 'object' || Array.isArray(builds) || Object.keys(builds).length !== BUILD_TARGETS.length
   || BUILD_TARGETS.some(target => !Object.hasOwn(builds, target))) throw new Error('Missing build provenance digests');
 for (const target of BUILD_TARGETS) {
  const record = builds[target];
  if (!record || typeof record !== 'object' || Array.isArray(record) || Object.keys(record).length !== 1 || !/^[a-f0-9]{64}$/.test(record.sha256)) throw new Error('Missing build provenance digest: ' + target);
  const bytes = await readBuildProvenanceFile(join(directory, buildProvenanceName(target)));
  if (digest(bytes) !== record.sha256) throw new Error('Build provenance digest mismatch: ' + target);
  verifyBuildProvenance(JSON.parse(bytes), target, { commit: source.commit, tree: source.tree, ciRun: source.ciRun, version }, release.assets);
 }
 const packageProducer = jobs.filter(job => job.name.endsWith('desktop-linux-packages-x64'));
 if (packageProducer.length !== 1 || packageProducer[0].conclusion !== 'success' || ['Build native Linux release packages', 'Record native Linux package build provenance'].some(name => !packageProducer[0].steps?.some(step => step.name === name && step.conclusion === 'success'))) throw new Error('Missing accepted native Linux package producer');
 // Build/recipe observation is not installed-package qualification. These
 // same-run gates deliberately remain required until each format is exercised.
 for (const format of ['deb', 'rpm', 'aur']) {
  const accepted = jobs.filter(job => job.name.endsWith(`desktop-linux-package-acceptance-${format}-x64`));
  if (accepted.length !== 1 || accepted[0].conclusion !== 'success' || !accepted[0].steps?.some(step => step.name === `Installed ${format.toUpperCase()} package acceptance` && step.conclusion === 'success')) throw new Error('Missing installed native Linux package acceptance: ' + format);
 }
 for (const target of TARGETS) {
  const native = jobs.find(job => job.name.endsWith(`desktop-${target}`));
  if (!native || native.conclusion !== 'success') throw new Error('Missing accepted native target: ' + target);
  // Each target requires its installed-upgrade acceptance. macOS additionally
  // proves unsigned Sparkle installation rather than running the NSIS/AppImage fixture.
  const gates = ['Record native desktop build provenance', 'Application audio encryption and exclusion', target.startsWith('mac-')
   ? 'Installed unsigned Sparkle upgrade and signed-feed rejection' : 'Installed upgrade acceptance'];
  if(target==='linux-x64')gates.push('Linux AppImage sandbox native acceptance');
  for (const gate of gates) if (!native.steps?.some(step => step.name === gate && step.conclusion === 'success')) throw new Error(`${target}: missing accepted ${gate}`);
  for (let shard = 1; shard <= 3; shard++) {
   if (!jobs.some(job => job.name.endsWith(`desktop-client-${target}-${shard}`) && job.conclusion === 'success')) throw new Error(`Missing Electron client coverage: ${target}-${shard}`);
  }
  const accountProofs = jobs.filter(job => job.conclusion === 'success' && [1, 2, 3].some(shard => job.name.endsWith(`desktop-client-${target}-${shard}`)))
   .flatMap(job => job.steps ?? []).filter(step => step.name === 'Installed account upgrade proof' && step.conclusion === 'success');
  if (accountProofs.length !== 1) throw new Error(`${target}: installed-account upgrade proof must succeed in exactly one client shard`);
  const voice = jobs.find(job => job.name.endsWith(`desktop-voice-${target}`) && job.conclusion === 'success');
  if (!voice) throw new Error(`Missing Electron voice coverage: ${target}`);
  if (!voice.steps?.some(step => step.name === 'Desktop performance comparison' && step.conclusion === 'success')) throw new Error(`${target}: missing accepted performance comparison`);
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
  if (report.format !== 1 || report.target !== target || report.sourceCommit !== source.commit || report.sourceDirty !== false || report.status !== 'measured'
    || !['startup', 'memory', 'idleCPU', 'interaction', 'callAudio'].every(metric => Array.isArray(report[metric]) && report[metric].length > 0
      && report[metric].every(sample => Number.isFinite(sample.browser) && sample.browser >= 0 && Number.isFinite(sample.desktop) && sample.desktop >= 0))) throw new Error('Incomplete measured performance comparison: ' + target);
 }
 const installedAccounts = source.installedAccounts;
 if (!installedAccounts || typeof installedAccounts !== 'object' || Array.isArray(installedAccounts)
  || Object.keys(installedAccounts).length !== TARGETS.length || TARGETS.some(target => !Object.hasOwn(installedAccounts, target))) throw new Error('Missing installed-account acceptance digests');
 for (const target of TARGETS) {
  const record = installedAccounts[target];
  if (!record || typeof record !== 'object' || Array.isArray(record) || Object.keys(record).length !== 1 || !/^[a-f0-9]{64}$/.test(record.sha256)) throw new Error('Missing installed-account acceptance digest: ' + target);
  const file = join(directory, `installed-account-${target}.json`), info = await lstat(file);
  if (!info.isFile() || info.isSymbolicLink() || info.size < 1 || info.size > 8192) throw new Error('Invalid installed-account report file: ' + target);
  const bytes = await readFile(file);
  if (bytes.length > 8192 || digest(bytes) !== record.sha256) throw new Error('Installed-account report digest mismatch: ' + target);
  verifyInstalledAccountReport(JSON.parse(bytes), target, source.commit);
 }
 const assets = release.assets;
 const requiredNames = [
  ...['deb', 'rpm', 'aur.tar.gz'].map(ext => `dilla-${version}-linux-x64.${ext}`),
  `dilla-${version}-linux-x64.AppImage`, ...['sandbox','sandbox.json','sandbox.sig'].map(ext => `dilla-${version}-linux-x64.${ext}`), `dilla-${version}-win-x64.exe`,
  `dilla-${version}-mac-arm64.zip`, `dilla-${version}-mac-arm64.dmg`,
 ];
 if (!Array.isArray(assets) || assets.length !== requiredNames.length || requiredNames.some(name => assets.filter(asset => asset.name === name).length !== 1)) throw new Error('All installable/update artifacts must be authenticated');
 for (const asset of assets) {
  if (typeof asset.name !== 'string' || basename(asset.name) !== asset.name || !requiredNames.includes(asset.name)
    || !Number.isSafeInteger(asset.size) || asset.size < 1 || typeof asset.sha512 !== 'string' || !/^[A-Za-z0-9+/]{86}==$/.test(asset.sha512)) throw new Error('Invalid release asset');
  if (mode === preSigning && asset.name.endsWith('.sandbox.sig')) continue;
  if (asset.name.includes('.sandbox') && !(asset.name.endsWith('.sandbox.sig') && sandboxSignature !== undefined)) { const info = await lstat(join(directory, asset.name)); if (!info.isFile() || info.isSymbolicLink() || info.nlink !== 1 || info.size !== asset.size) throw new Error('Invalid sandbox sidecar file: ' + asset.name); }
  const bytes = asset.name.endsWith('.sandbox.sig') && sandboxSignature !== undefined ? Buffer.from(sandboxSignature) : await readFile(join(directory, asset.name));
  if (!verifyArtifact(bytes, asset)) throw new Error('Release artifact digest mismatch: ' + asset.name);
 }
 await verifyAurRecipe(version, join(directory, `dilla-${version}-linux-x64.deb`), join(directory, `dilla-${version}-linux-x64.aur.tar.gz`));
 const capsuleName = `dilla-${version}-linux-x64.sandbox`, manifestName = capsuleName + '.json', signatureName = capsuleName + '.sig';
 const capsuleAsset = assets.find(asset => asset.name === capsuleName), manifestAsset = assets.find(asset => asset.name === manifestName), signatureAsset = assets.find(asset => asset.name === signatureName);
 const linux = release.artifacts.find(a => a.platform === 'linux' && a.arch === 'x64'), descriptor = linux?.linuxSandbox;
 if (!descriptor || capsuleAsset.size !== descriptor.bundle.size || capsuleAsset.sha512 !== descriptor.bundle.sha512 || manifestAsset.size > 65536 || signatureAsset.size > 89) throw new Error('Linux sandbox descriptor and assets differ');
 const capsule = await readFile(join(directory, capsuleName)), manifest = await readFile(join(directory, manifestName));
 const component = parsePublicConfiguration(await readFile(new URL('../linux-sandbox-component.json', import.meta.url), 'utf8')); assertDedicatedKeys(component, trust);
 const input = { capsule, manifest, now, minimumGeneration: descriptor.minimumGeneration };
 const verified = mode === preSigning ? inspectLinuxSandboxCapsule(input) : verifyLinuxSandboxRelease({ ...input, signature: (sandboxSignature !== undefined ? Buffer.from(sandboxSignature) : await readFile(join(directory, signatureName))).toString('utf8'), keys: component.publicKeys });
 const manifestDocument = JSON.parse(manifest.toString('utf8'));
 if (manifestDocument.publishedAt !== component.publishedAt || manifestDocument.expiresAt !== component.expiresAt || manifestDocument.helper.size !== component.helper.size || manifestDocument.helper.sha512 !== component.helper.sha512 || verified.generation !== component.generation || verified.compatibilityEpoch !== descriptor.compatibilityEpoch) throw new Error('Linux sandbox configuration does not match accepted component');
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
