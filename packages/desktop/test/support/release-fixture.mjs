import { generateKeyPairSync, createHash, sign } from 'node:crypto';
import { mkdtemp, mkdir, writeFile, readFile, copyFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, dirname, resolve } from 'node:path';
import { execFileSync } from 'node:child_process';
import { pathToFileURL } from 'node:url';
import { sandboxReleaseFixture } from './linux-sandbox-release-fixture.ts';
import { appcast, signedAppcast } from '../../scripts/feed-format.mjs';
import { TARGETS, BUILD_TARGETS, LINUX_CLIENT_SHARDS, inventoryDigest } from '../../scripts/verify-release.mjs';
import { ACCOUNT_CHECKS } from '../../scripts/installed-account-proof.mjs';
import { buildArtifactNames, buildProvenanceName } from '../../scripts/build-provenance.mjs';
import { aurRecipe } from '../../scripts/build-native-linux-packages.mjs';

const desktopRoot = resolve(import.meta.dirname, '../..');
const sha256 = bytes => createHash('sha256').update(bytes).digest('hex');
const sha512 = bytes => createHash('sha512').update(bytes).digest('base64');

/** Public tooling copied next to fixture trust, so tests never read or change production pins. */
export const RELEASE_TOOL_FILES = Object.freeze(['scripts/release-feed.mjs', 'scripts/verify-release.mjs', 'scripts/feed-format.mjs', 'scripts/installed-account-proof.mjs',
 'scripts/build-provenance.mjs', 'scripts/is-main.mjs', 'scripts/build-native-linux-packages.mjs', 'scripts/linux-package-version.mjs', 'scripts/linux-sandbox-package.mjs',
 'src/update.ts', 'src/update-fs.ts', 'src/linux-sandbox-release.ts']);

export async function isolatedReleaseTools(directory, { trust, sandboxConfig, files = [] }) {
 for (const relative of [...RELEASE_TOOL_FILES, ...files]) {
  const file = join(directory, relative); await mkdir(dirname(file), { recursive: true }); await copyFile(join(desktopRoot, relative), file);
 }
 await writeFile(join(directory, 'package.json'), JSON.stringify({ type: 'module' }));
 await writeFile(join(directory, 'release-trust.json'), JSON.stringify(trust));
 await writeFile(join(directory, 'linux-sandbox-component.json'), JSON.stringify(sandboxConfig));
 return {
  verifier: await import(pathToFileURL(join(directory, 'scripts/verify-release.mjs')).href),
  signer: await import(pathToFileURL(join(directory, 'scripts/release-feed.mjs')).href),
 };
}

/** The CI job inventory a green desktop run reports. */
export function acceptedJobs() {
 const jobs = [];
 for (const target of TARGETS) {
  jobs.push({ name: `desktop / desktop-${target}`, conclusion: 'success', steps: ['Record native desktop build provenance', 'Application audio encryption and exclusion', target.startsWith('mac-') ? 'Installed unsigned Sparkle upgrade and signed-feed rejection' : 'Installed upgrade acceptance', ...(target.startsWith('mac-') ? ['macOS update disk image check'] : []), ...(target === 'linux-x64' ? ['Linux AppImage sandbox native acceptance'] : [])].map(name => ({ name, conclusion: 'success' })) });
  const shards = target === 'linux-x64' ? Array.from({ length: LINUX_CLIENT_SHARDS }, (_, i) => i + 1) : [1], proofShard = target === 'linux-x64' ? 5 : 1;
  for (const n of shards) jobs.push({ name: `desktop / desktop-client-${target}-${n}`, conclusion: 'success', steps: [{ name: 'Installed account upgrade proof', conclusion: n === proofShard ? 'success' : 'skipped' }] });
  jobs.push({ name: `desktop / desktop-voice-${target}`, conclusion: 'success', steps: [{ name: 'Desktop performance comparison', conclusion: 'success' }] });
 }
 for (const format of ['deb', 'rpm', 'aur']) jobs.push({ name: `desktop / desktop-linux-package-acceptance-${format}-x64`, conclusion: 'success', steps: [{ name: `Installed ${format.toUpperCase()} package acceptance`, conclusion: 'success' }] });
 jobs.push({ name: 'desktop / desktop-linux-packages-x64', conclusion: 'success', steps: ['Build native Linux release packages', 'Record native Linux package build provenance'].map(name => ({ name, conclusion: 'success' })) });
 return jobs;
}

/**
 * A complete accepted desktop release: every installer, sidecar and record
 * written to `directory`, fixture keys, a green run and a signed metadata document.
 * `layout: 'artifacts'` additionally writes each file into the CI artifact
 * subdirectory it is downloaded from (downloadRoot/<artifact name>/<file>).
 */
/** Sandbox component dates around the real clock, for tests that sign with the current time. */
export function currentSandboxDates(now = Date.now()) {
 const day = 86400000, start = new Date(Math.floor(now / day) * day - day);
 const iso = date => date.toISOString().replace('.000Z', 'Z');
 return { publishedAt: iso(start), expiresAt: iso(new Date(start.getTime() + 61 * day)) };
}

export async function buildReleaseFixture({ version = '1.2.3', directory, now = Date.parse('2026-11-02'), commit = '1'.repeat(40), head = '2'.repeat(40), tree = '3'.repeat(40), sandboxDates } = {}) {
 directory ??= await mkdtemp(join(tmpdir(), 'dilla-release-fixture-'));
 const sandbox = sandboxReleaseFixture(sandboxDates);
 const key = generateKeyPairSync('ed25519'), sparkle = generateKeyPairSync('ed25519');
 const source = { format: 1, ciRun: 10, commit, head, tree, review: null, builds: {}, performance: {}, installedAccounts: {}, notes: { sha256: '' } };
 const notes = Buffer.from(`Dilla desktop ${version}`); source.notes.sha256 = sha256(notes); await writeFile(join(directory, 'release-notes.md'), notes);
 const jobs = acceptedJobs();
 const files = { installers: {}, performance: {}, 'installed-account': {} };
 for (const target of TARGETS) {
  const report = Buffer.from(JSON.stringify({ format: 1, target, sourceCommit: source.commit, sourceDirty: false, status: 'measured', ...Object.fromEntries(['startup', 'memory', 'idleCPU', 'interaction', 'callAudio'].map(metric => [metric, [{ browser: 1, desktop: 2 }]])) }));
  await writeFile(join(directory, `performance-${target}.json`), report); source.performance[target] = { sha256: sha256(report) };
  files.performance[target] = [`performance-${target}.json`];
  const [platform, arch] = { 'linux-x64': ['linux', 'x64'], 'win-x64': ['win32', 'x64'], 'mac-arm64': ['darwin', 'arm64'] }[target];
  const account = Buffer.from(JSON.stringify({ format: 1, platform, arch, from: '1.0.0', to: '1.0.1', ...Object.fromEntries(ACCOUNT_CHECKS.map(check => [check, true])), target, sourceCommit: source.commit, sourceDirty: false, status: 'passed' }));
  await writeFile(join(directory, `installed-account-${target}.json`), account); source.installedAccounts[target] = { sha256: sha256(account) };
  files['installed-account'][target] = [`installed-account-${target}.json`];
 }
 const assets = [];
 for (const name of [...['deb', 'rpm', 'aur.tar.gz'].map(ext => `dilla-${version}-linux-x64.${ext}`), `dilla-${version}-linux-x64.AppImage`, ...['sandbox', 'sandbox.json', 'sandbox.sig'].map(ext => `dilla-${version}-linux-x64.${ext}`), `dilla-${version}-win-x64.exe`, `dilla-${version}-mac-arm64.zip`, `dilla-${version}-mac-arm64.dmg`]) {
  const bytes = name.endsWith('.sandbox.sig') ? sandbox.signature : name.endsWith('.sandbox.json') ? sandbox.manifest : name.endsWith('.sandbox') ? sandbox.capsule : Buffer.from(name);
  await writeFile(join(directory, name), bytes); assets.push({ name, size: bytes.length, sha512: sha512(bytes) });
 }
 const recipeDirectory = join(directory, 'recipe'); await mkdir(recipeDirectory);
 for (const [name, value] of Object.entries(aurRecipe(version, sha256(`dilla-${version}-linux-x64.deb`)))) await writeFile(join(recipeDirectory, name), value);
 const recipeName = `dilla-${version}-linux-x64.aur.tar.gz`;
 execFileSync('tar', ['--format=ustar', '-czf', join(directory, recipeName), '-C', recipeDirectory, '.SRCINFO', 'PKGBUILD']);
 const recipeBytes = await readFile(join(directory, recipeName)); Object.assign(assets.find(asset => asset.name === recipeName), { size: recipeBytes.length, sha512: sha512(recipeBytes) });
 for (const target of BUILD_TARGETS) {
  const record = Buffer.from(JSON.stringify({ format: 1, kind: 'dilla-desktop-build', repository: 'dilla-chat/dilla', target, version,
   sourceCommit: source.commit, sourceTree: source.tree, ciRun: source.ciRun, sourceClean: true,
   artifacts: buildArtifactNames(target, version).map(name => assets.find(asset => asset.name === name)), ...(target === 'linux-x64' ? { linuxSandbox: { capsuleSha512: sandbox.hash(sandbox.capsule), manifestSha512: sandbox.hash(sandbox.manifest), embeddedCapsuleSha512: sandbox.hash(sandbox.capsule), embeddedManifestSha512: sandbox.hash(sandbox.manifest), electronSha512: sandbox.hash(Buffer.from('fixture-elf')), asarSha512: sandbox.hash(Buffer.from('fixture-asar')) } } : {}) }));
  await writeFile(join(directory, buildProvenanceName(target)), record); source.builds[target] = { sha256: sha256(record) };
  files.installers[target] = [...buildArtifactNames(target, version), buildProvenanceName(target)];
 }
 source.review = { status: 'automated', ciRun: source.ciRun, tree: source.tree, inventory: { sha256: inventoryDigest(jobs, version) } };
 const channel = version.includes('-beta.') ? 'beta' : 'stable';
 const release = { format: 1, version, channel, publishedAt: '2026-11-01T00:00:00Z', expiresAt: '2026-12-01T00:00:00Z', source, assets, artifacts: assets.filter(asset => /\.(AppImage|exe|zip)$/.test(asset.name)).map(({ name, ...asset }) => ({ ...asset, platform: name.includes('-linux-') ? 'linux' : name.includes('-win-') ? 'win32' : 'darwin', arch: name.includes('arm64') ? 'arm64' : 'x64', url: `https://github.com/dilla-chat/dilla/releases/download/desktop-v${version}/${name}`, ...(name.endsWith('-linux-x64.AppImage') ? { linuxSandbox: { compatibilityEpoch: 1, minimumGeneration: 1, bundle: { url: `https://github.com/dilla-chat/dilla/releases/download/desktop-v${version}/dilla-${version}-linux-x64.sandbox`, size: sandbox.capsule.length, sha512: sandbox.hash(sandbox.capsule) } } } : {}), ...(name.endsWith('.zip') ? { sparkleSignature: sign(null, Buffer.from(name), sparkle.privateKey).toString('base64') } : {}) })) };
 for (const artifact of release.artifacts.filter(a => a.platform === 'darwin')) await writeFile(join(directory, `appcast-${artifact.arch}.xml`), signedAppcast(appcast(release, artifact, artifact.sparkleSignature), sparkle.privateKey));
 const metadata = Buffer.from(JSON.stringify(release)), signature = sign(null, metadata, key.privateKey).toString('base64');
 const trust = { metadataPublicKeys: [key.publicKey.export({ type: 'spki', format: 'pem' }).toString()], sparklePublicKey: sparkle.publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64') };
 const run = { id: source.ciRun, head_sha: source.head, name: 'ci', status: 'completed', conclusion: 'success', repository: { full_name: 'dilla-chat/dilla' }, head_repository: { full_name: 'dilla-chat/dilla' } };
 const context = { metadata, signature, trust, directory, run, jobs, version, now };
 const resign = changed => { const bytes = Buffer.from(JSON.stringify(changed)); return { ...context, metadata: bytes, signature: sign(null, bytes, key.privateKey).toString('base64') }; };
 /** Lays the CI-produced files out as one subdirectory per downloaded artifact. */
 async function writeArtifactLayout(downloadRoot) {
  for (const [kind, byTarget] of Object.entries(files)) for (const [target, names] of Object.entries(byTarget)) {
   const artifact = join(downloadRoot, `desktop-${kind}-${target}`); await mkdir(artifact, { recursive: true });
   for (const name of names) await copyFile(join(directory, name), join(artifact, name));
  }
 }
 return { directory, version, channel, sandbox, key, sparkle, source, notes, jobs, assets, recipeDirectory, recipeName, release, metadata, signature, trust, run, context, resign, files, writeArtifactLayout };
}
