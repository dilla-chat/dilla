import { createHash, createPublicKey, verify } from 'node:crypto';
import { readFile, writeFile, lstat } from 'node:fs/promises';
import { join, basename } from 'node:path';
import { appcast, macArtifactKind, macRotationOf, macUpdateKind, sparkleFeedKey } from './feed-format.mjs';
import { verifyRelease, verifyArtifact } from '../src/update.ts';
import { inspectLinuxSandboxCapsule, verifyLinuxSandboxRelease } from '../src/linux-sandbox-release.ts';
import { assertDedicatedKeys, parsePublicConfiguration } from './linux-sandbox-package.mjs';
import { verifyInstalledAccountReport } from './installed-account-proof.mjs';
import { verifyAurRecipe } from './build-native-linux-packages.mjs';
import { buildProvenanceName, readBuildProvenanceFile, verifyBuildProvenance, verifyCandidateBuildRecord } from './build-provenance.mjs';

export const TARGETS = ['linux-x64', 'win-x64', 'mac-arm64'];
/** The Linux client suite's shards in desktop.yml (WEB_SHARDS in scripts/check-ci-workflow.mjs pins the same number). */
export const LINUX_CLIENT_SHARDS = 6;
export const BUILD_TARGETS = Object.freeze([...TARGETS, 'linux-packages-x64']);
export const BUILD_PROVENANCE_RECORDS = Object.freeze(BUILD_TARGETS.map(buildProvenanceName));
export const PUBLIC_ACCEPTANCE_RECORDS = Object.freeze(['release-notes.md',
 ...TARGETS.flatMap(target => [`performance-${target}.json`, `installed-account-${target}.json`]), ...BUILD_PROVENANCE_RECORDS]);

/** The CI jobs and steps a release requires, shared by verifyBundle and the
 * automated attestation. Jobs are matched by name suffix ("desktop / <suffix>"). */
const PACKAGE_PRODUCER = Object.freeze({ suffix: 'desktop-linux-packages-x64', steps: Object.freeze(['Build native Linux release packages', 'Record native Linux package build provenance']) });
const PACKAGE_FORMATS = Object.freeze(['deb', 'rpm', 'aur']);
const packageAcceptanceStep = format => `Installed ${format.toUpperCase()} package acceptance`;
const nativeGates = target => ['Record native desktop build provenance', 'Application audio encryption and exclusion',
 target.startsWith('mac-') ? 'Installed unsigned Sparkle upgrade and signed-feed rejection' : 'Installed upgrade acceptance',
 // The DMG's Developer ID team, notarization and Sparkle key (check-mac-dmg.mjs; final review, m9).
 ...(target.startsWith('mac-') ? ['macOS update disk image check'] : []),
 ...(target === 'linux-x64' ? ['Linux AppImage sandbox native acceptance'] : [])];
// Linux runs the shared client suite in three shards; the other targets run the desktop-specific specs in one job.
// Linux runs the shared client suite in LINUX_CLIENT_SHARDS shards; the other targets run the desktop-specific specs in one job.
const clientShards = target => target === 'linux-x64' ? Array.from({ length: LINUX_CLIENT_SHARDS }, (_, i) => i + 1) : [1];
const ACCOUNT_PROOF_STEP = 'Installed account upgrade proof';
const PERFORMANCE_STEP = 'Desktop performance comparison';
const VERSION = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/;

/** Each entry: a job name suffix, the steps it must report, and whether every one of them must succeed. */
function requiredJobs() {
 return [
  { ...PACKAGE_PRODUCER, allSteps: true },
  ...PACKAGE_FORMATS.map(format => ({ suffix: `desktop-linux-package-acceptance-${format}-x64`, steps: [packageAcceptanceStep(format)], allSteps: true })),
  ...TARGETS.flatMap(target => [
   { suffix: `desktop-${target}`, steps: nativeGates(target), allSteps: true },
   ...clientShards(target).map(shard => ({ suffix: `desktop-client-${target}-${shard}`, steps: [ACCOUNT_PROOF_STEP], allSteps: false, proof: target })),
   { suffix: `desktop-voice-${target}`, steps: [PERFORMANCE_STEP], allSteps: true },
  ]),
 ];
}

/**
 * The exact required-job inventory the automated attestation digests: sorted by
 * job name, each with its conclusion and only its required steps (in run order).
 * Throws if a required job is missing, duplicated or not successful. The
 * inventory does not depend on the version; it is validated when given so the
 * assembler and verifier call this identically as (jobs, version).
 */
export function requiredJobInventory(jobs, version) {
 if (version !== undefined && (typeof version !== 'string' || !VERSION.test(version))) throw new Error('Invalid desktop release version');
 if (!Array.isArray(jobs)) throw new Error('Missing complete CI job inventory');
 const inventory = [], proofs = {};
 for (const required of requiredJobs()) {
  const matches = jobs.filter(job => typeof job?.name === 'string' && job.name.endsWith(required.suffix));
  if (matches.length !== 1) throw new Error(`Required CI job must appear exactly once (missing or duplicated): ${required.suffix}`);
  const [job] = matches;
  if (job.conclusion !== 'success') throw new Error('Required CI job did not succeed: ' + required.suffix);
  const steps = (Array.isArray(job.steps) ? job.steps : []).filter(step => required.steps.includes(step?.name)).map(step => ({ name: step.name, conclusion: step.conclusion }));
  if (required.allSteps && required.steps.some(name => !steps.some(step => step.name === name && step.conclusion === 'success'))) throw new Error('Required CI step missing or did not succeed: ' + required.suffix);
  if (required.proof) proofs[required.proof] = (proofs[required.proof] ?? 0) + steps.filter(step => step.conclusion === 'success').length;
  inventory.push({ name: job.name, conclusion: job.conclusion, steps });
 }
 for (const target of TARGETS) if (proofs[target] !== 1) throw new Error(`${target}: installed-account upgrade proof must succeed in exactly one client shard`);
 return inventory.sort((a, b) => a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
}
/** sha256 of the canonical inventory JSON; the only form the assembler and verifier compare. */
export function inventoryDigest(jobs, version) {
 return createHash('sha256').update(JSON.stringify(requiredJobInventory(jobs, version))).digest('hex');
}
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
  const bytes = await readFile(join(candidate, name));
  try { await writeFile(join(directory, name), bytes, { flag: 'wx' }); }
  catch (error) {
   // A record the CI artifacts already delivered (performance, installed-account) must be byte-identical.
   if (error?.code !== 'EEXIST') throw error;
   const info = await lstat(join(directory, name));
   if (info.isSymbolicLink() || !info.isFile() || !(await readFile(join(directory, name))).equals(bytes)) throw new Error('A CI-collected record differs from the signed candidate: ' + name);
  }
 }
}
const digest = bytes => createHash('sha256').update(bytes).digest('hex');

/** Publication checks the complete accepted run, not just a green build or a
 * mutable release draft. Signing keys are not part of this process. */
const preSigning = Symbol('pre-signing acceptance only');
export async function verifyDesktopSigningCandidate(options) { return verifyBundle(options, preSigning); }
export async function verifyDesktopBundle(options) { return verifyBundle(options); }
/** `macUpdate` is the macOS update kind the gate decided for this release ('zip', the default, or 'dmg' for a channel's
 * rotation release): the signed darwin artifact must be exactly that archive, and a DMG needs a pending macOS key change. */
async function verifyBundle({ metadata, signature, trust, directory, run, jobs, version, feeds, now = Date.now(), sandboxSignature, macUpdate = 'zip' }, mode) {
 const channel = version.includes('-beta.') ? 'beta' : 'stable';
 macUpdateKind(macUpdate);
 if (macUpdate === 'dmg' && !macRotationOf(trust)) throw new Error('A macOS disk image update is only allowed for the rotation release of a pending macOS key change');
 const release = verifyRelease(metadata, signature.trim(), { publicKeys: trust.metadataPublicKeys, channel, platform: 'linux', arch: 'x64', currentVersion: '0.0.0', now });
 if (release.version !== version) throw new Error('Release tag and authenticated version differ');
 verifyRelease(metadata, signature.trim(), { publicKeys: trust.metadataPublicKeys, channel, platform: 'win32', arch: 'x64', currentVersion: '0.0.0', now });
 const expectedTargets = ['linux-x64', 'win32-x64', 'darwin-arm64'];
 if (release.artifacts.length !== expectedTargets.length || expectedTargets.some(target => !release.artifacts.some(a => `${a.platform}-${a.arch}` === target))) throw new Error('All desktop platforms must release together');
 const source = release.source;
 if (!source || source.format !== 1 || !Number.isSafeInteger(source.ciRun) || source.ciRun < 1
   || ![source.commit, source.head, source.tree].every(value => typeof value === 'string' && /^[a-f0-9]{40}$/.test(value))) throw new Error('Missing authenticated acceptance source');
 if (run.id !== source.ciRun || run.head_sha !== source.head || run.name !== 'ci' || run.status !== 'completed' || run.conclusion !== 'success'
   || run.repository?.full_name !== 'dilla-chat/dilla' || run.head_repository?.full_name !== 'dilla-chat/dilla') throw new Error('The accepted same-repository CI run must be complete and successful');
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
 const packageProducer = jobs.filter(job => job.name.endsWith(PACKAGE_PRODUCER.suffix));
 if (packageProducer.length !== 1 || packageProducer[0].conclusion !== 'success' || PACKAGE_PRODUCER.steps.some(name => !packageProducer[0].steps?.some(step => step.name === name && step.conclusion === 'success'))) throw new Error('Missing accepted native Linux package producer');
 // Build/recipe observation is not installed-package qualification. These
 // same-run gates deliberately remain required until each format is exercised.
 for (const format of PACKAGE_FORMATS) {
  const accepted = jobs.filter(job => job.name.endsWith(`desktop-linux-package-acceptance-${format}-x64`));
  if (accepted.length !== 1 || accepted[0].conclusion !== 'success' || !accepted[0].steps?.some(step => step.name === packageAcceptanceStep(format) && step.conclusion === 'success')) throw new Error('Missing installed native Linux package acceptance: ' + format);
 }
 for (const target of TARGETS) {
  const native = jobs.find(job => job.name.endsWith(`desktop-${target}`));
  if (!native || native.conclusion !== 'success') throw new Error('Missing accepted native target: ' + target);
  // Each target requires its installed-upgrade acceptance. macOS additionally
  // proves unsigned Sparkle installation rather than running the NSIS/AppImage fixture.
  for (const gate of nativeGates(target)) if (!native.steps?.some(step => step.name === gate && step.conclusion === 'success')) throw new Error(`${target}: missing accepted ${gate}`);
  for (const shard of clientShards(target)) {
   if (!jobs.some(job => job.name.endsWith(`desktop-client-${target}-${shard}`) && job.conclusion === 'success')) throw new Error(`Missing Electron client coverage: ${target}-${shard}`);
  }
  const accountProofs = jobs.filter(job => job.conclusion === 'success' && clientShards(target).some(shard => job.name.endsWith(`desktop-client-${target}-${shard}`)))
   .flatMap(job => job.steps ?? []).filter(step => step.name === ACCOUNT_PROOF_STEP && step.conclusion === 'success');
  if (accountProofs.length !== 1) throw new Error(`${target}: installed-account upgrade proof must succeed in exactly one client shard`);
  const voice = jobs.find(job => job.name.endsWith(`desktop-voice-${target}`) && job.conclusion === 'success');
  if (!voice) throw new Error(`Missing Electron voice coverage: ${target}`);
  if (!voice.steps?.some(step => step.name === PERFORMANCE_STEP && step.conclusion === 'success')) throw new Error(`${target}: missing accepted performance comparison`);
 }
 // The automated attestation binds the accepted run, tree and exact required-job inventory (no human review file).
 const review = source.review;
 if (!review || typeof review !== 'object' || Array.isArray(review) || Object.keys(review).length !== 4 || !['status', 'ciRun', 'tree', 'inventory'].every(key => Object.hasOwn(review, key))
   || review.status !== 'automated' || review.ciRun !== source.ciRun || review.tree !== source.tree
   || !review.inventory || typeof review.inventory !== 'object' || Array.isArray(review.inventory) || Object.keys(review.inventory).length !== 1
   || typeof review.inventory.sha256 !== 'string' || review.inventory.sha256 !== inventoryDigest(jobs, version)) throw new Error('The automated CI attestation does not match the accepted run');
 if (!source.notes || !/^[a-f0-9]{64}$/.test(source.notes.sha256) || digest(await readFile(join(directory, 'release-notes.md'))) !== source.notes.sha256) throw new Error('Release notes digest mismatch');
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
   if (artifact.url !== `https://github.com/dilla-chat/dilla/releases/download/desktop-v${version}/dilla-${version}-mac-${artifact.arch}.${macUpdate}`) {
    throw new Error(macUpdate === 'dmg' ? 'The rotation release must update macOS through its disk image, not the zip' : 'Only the rotation release of a pending macOS key change may update macOS through a disk image');
   }
   const archive = await readFile(join(directory, asset.name));
   if (!verify(null, archive, sparkleKey, Buffer.from(artifact.sparkleSignature, 'base64'))) throw new Error('Sparkle archive signature mismatch');
   verifySparkleFeed(release, artifact, feeds?.[artifact.arch] ?? await readFile(join(directory, `appcast-${artifact.arch}.xml`)), trust);
  }
 }
 return release;
}

const ROTATION_SANDBOX_CONTEXT = 'Dilla Linux sandbox capsule v1\0';
const rawPublicKey = raw => createPublicKey({ key: { kty: 'OKP', crv: 'Ed25519', x: Buffer.from(raw, 'base64').toString('base64url') }, format: 'jwk' });
const pemSigner = (keys, message, signature) => keys.find(key => { try { return verify(null, message, createPublicKey(key), signature); } catch { return false; } });
const rawSigner = (keys, message, signature) => keys.find(key => { try { return verify(null, message, rawPublicKey(key), signature); } catch { return false; } });

/**
 * A staged-rotation proof must bind exactly this release (version, channel, the
 * signed metadata, the sandbox manifest and the macOS archive) and carry a
 * signature from each staged key, where every staged key is pinned, is not the
 * key that signed the release, and the staged slot is the one release-trust.json names.
 * `files` holds the signed metadata, the sandbox manifest and the arm64 archive.
 */
export async function verifyRotationProof(proofBytes, { trust, version, channel, files, metadataSignature, sandboxSignature }) {
 if (!VERSION.test(version ?? '') || !['beta', 'stable'].includes(channel)) throw new Error('Invalid rotation proof context');
 const fail = what => { throw new Error('Invalid rotation proof: ' + what); };
 let proof;
 try { proof = JSON.parse(Buffer.from(proofBytes).toString('utf8')); } catch { fail('not JSON'); }
 const exact = (value, keys) => value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === keys.length && keys.every(key => Object.hasOwn(value, key));
 if (!exact(proof, ['format', 'kind', 'version', 'channel', 'stagedSlot', 'metadata', 'sandbox', 'sparkle']) || proof.format !== 1 || proof.kind !== 'dilla-desktop-rotation-proof') fail('shape');
 if (proof.version !== version || proof.channel !== channel) fail('another version or channel');
 const staged = trust?.signing?.staged;
 if (!['A', 'B'].includes(staged) || proof.stagedSlot !== staged || trust.signing.active === staged) fail('the staged slot is not the one release-trust.json names');
 const metadataFile = `desktop-${channel}.json`, manifestFile = `dilla-${version}-linux-x64.sandbox.json`, archiveFile = `dilla-${version}-mac-arm64.zip`;
 const component = parsePublicConfiguration(await readFile(new URL('../linux-sandbox-component.json', import.meta.url), 'utf8'));
 const entries = [
  ['metadata', metadataFile, files.get(metadataFile)],
  ['sandbox', manifestFile, files.get(manifestFile)],
  ['sparkle', archiveFile, files.get(archiveFile)],
 ];
 for (const [kind, file, bytes] of entries) {
  const entry = proof[kind];
  if (!exact(entry, kind === 'sandbox' ? ['file', 'sha256', 'publicKey', 'signature', 'context'] : ['file', 'sha256', 'publicKey', 'signature'])) fail(kind + ' entry shape');
  if (!Buffer.isBuffer(bytes) || entry.file !== file || entry.sha256 !== digest(bytes)) fail(kind + ' does not bind this release');
  if (typeof entry.signature !== 'string' || !/^[A-Za-z0-9+/]{86}==$/.test(entry.signature)) fail(kind + ' signature');
 }
 if (proof.sandbox.context !== 'Dilla Linux sandbox capsule v1\\0') fail('sandbox context');
 const metadata = files.get(metadataFile), manifest = Buffer.concat([Buffer.from(ROTATION_SANDBOX_CONTEXT), files.get(manifestFile)]), archive = files.get(archiveFile);
 // Metadata: a pinned key, not the one that signed the release metadata.
 const activeMetadata = pemSigner(trust.metadataPublicKeys, metadata, Buffer.from(metadataSignature.trim(), 'base64'));
 if (!trust.metadataPublicKeys.includes(proof.metadata.publicKey) || proof.metadata.publicKey === activeMetadata) fail('the staged metadata key is not pinned');
 if (!verify(null, metadata, createPublicKey(proof.metadata.publicKey), Buffer.from(proof.metadata.signature, 'base64'))) fail('metadata signature');
 // Sandbox: a pinned component key, not the one that signed the capsule.
 const activeSandbox = rawSigner(component.publicKeys, manifest, Buffer.from(sandboxSignature.toString('utf8').trim(), 'base64'));
 if (!component.publicKeys.includes(proof.sandbox.publicKey) || proof.sandbox.publicKey === activeSandbox) fail('the staged sandbox key is not pinned');
 if (!verify(null, manifest, rawPublicKey(proof.sandbox.publicKey), Buffer.from(proof.sandbox.signature, 'base64'))) fail('sandbox signature');
 // Sparkle: exactly the pinned next key.
 if (typeof trust.sparkleNextPublicKey !== 'string' || proof.sparkle.publicKey !== trust.sparkleNextPublicKey || proof.sparkle.publicKey === trust.sparklePublicKey) fail('the staged Sparkle key is not pinned');
 if (!verify(null, archive, rawPublicKey(proof.sparkle.publicKey), Buffer.from(proof.sparkle.signature, 'base64'))) fail('sparkle signature');
 return proof;
}

/** A signed appcast must carry a valid Sparkle signature under the installed apps' pinned key and be exactly the appcast
 * of this authenticated release. That key is the active one, except for a DMG rotation release (feed-format.mjs). */
export function verifySparkleFeed(release, artifact, feed, trust) {
 if (typeof trust.sparklePublicKey !== 'string' || Buffer.from(trust.sparklePublicKey, 'base64').length !== 32) throw new Error('Missing installed Sparkle trust');
 const feedKey = sparkleFeedKey(trust, macArtifactKind(artifact));
 const sparkleKey = createPublicKey({ key: { kty: 'OKP', crv: 'Ed25519', x: Buffer.from(feedKey, 'base64').toString('base64url') }, format: 'jwk' });
 const marker = Buffer.from('<!-- sparkle-signatures:\n'), boundary = feed.lastIndexOf(marker);
 if (boundary < 0) throw new Error('Unsigned Sparkle feed');
 const block = /^<!-- sparkle-signatures:\nedSignature: ([A-Za-z0-9+/]{86}==)\nlength: (\d+)\n-->\n$/.exec(feed.subarray(boundary).toString());
 if (!block || Number(block[2]) !== boundary || !verify(null, feed.subarray(0, boundary), sparkleKey, Buffer.from(block[1], 'base64'))
   || feed.subarray(0, boundary).toString() !== appcast(release, artifact, artifact.sparkleSignature)) throw new Error('Sparkle feed signature or authenticated target mismatch');
}
