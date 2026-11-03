import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { lstat, open, readFile, writeFile } from 'node:fs/promises';
import { basename, join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

const PLATFORMS = Object.freeze({ 'linux-x64': ['linux', 'x64'], 'linux-packages-x64': ['linux', 'x64'], 'win-x64': ['win32', 'x64'], 'mac-arm64': ['darwin', 'arm64'] });
const COMMIT = /^[a-f0-9]{40}$/, HASH = /^[A-Za-z0-9+/]{86}==$/;
function exact(value, fields) { return value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === fields.length && fields.every(key => Object.hasOwn(value, key)); }
function validVersion(version) {
 const parts = typeof version === 'string' && /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/.exec(version);
 if (!parts || !parts.slice(1).filter(part => part !== undefined).every(part => Number.isSafeInteger(Number(part)))) throw new Error('Invalid build provenance version');
}
export function buildArtifactNames(target, version) {
 validVersion(version); if (!Object.hasOwn(PLATFORMS, target)) throw new Error('Invalid build provenance target');
 if (target === 'linux-packages-x64') return ['deb', 'rpm', 'aur.tar.gz'].map(ext => `dilla-${version}-linux-x64.${ext}`);
 const prefix = `dilla-${version}-${target}`;
 return target === 'linux-x64' ? [prefix + '.AppImage', prefix + '.sandbox', prefix + '.sandbox.json'] : target === 'win-x64' ? [prefix + '.exe'] : [prefix + '.dmg', prefix + '.zip'];
}
export function buildProvenanceName(target) {
 if (!Object.hasOwn(PLATFORMS, target)) throw new Error('Invalid build provenance target');
 return `build-provenance-${target}.json`;
}
/** The expected commit/tree come from signed release source. The raw record
 * must additionally be the CI-downloaded file, never a candidate replacement. */
export function verifyBuildProvenance(record, target, expected, signedAssets) {
 const names = buildArtifactNames(target, expected.version);
 if (!exact(record, ['format', 'kind', 'repository', 'target', 'version', 'sourceCommit', 'sourceTree', 'ciRun', 'sourceClean', 'artifacts', ...(target === 'linux-x64' ? ['linuxSandbox'] : [])])
  || record.format !== 1 || record.kind !== 'dilla-desktop-build' || record.repository !== 'jonasthim/dilla' || record.target !== target || record.version !== expected.version) throw new Error('Invalid build provenance fields');
 if (!COMMIT.test(record.sourceCommit) || !COMMIT.test(record.sourceTree) || record.sourceCommit !== expected.commit || record.sourceTree !== expected.tree
  || !Number.isSafeInteger(record.ciRun) || record.ciRun < 1 || record.ciRun !== expected.ciRun || record.sourceClean !== true) throw new Error('Build provenance source binding mismatch');
 if (!Array.isArray(record.artifacts) || record.artifacts.length !== names.length || !Array.isArray(signedAssets)) throw new Error('Invalid build provenance artifacts');
 if (target === 'linux-x64') {
  const proof = record.linuxSandbox, fields = ['capsuleSha512','manifestSha512','electronSha512','asarSha512','embeddedCapsuleSha512','embeddedManifestSha512'];
  if (!exact(proof, fields) || fields.some(field => typeof proof[field] !== 'string' || !HASH.test(proof[field]) || Buffer.from(proof[field], 'base64').toString('base64') !== proof[field]) || proof.capsuleSha512 !== record.artifacts[1]?.sha512 || proof.manifestSha512 !== record.artifacts[2]?.sha512 || proof.embeddedCapsuleSha512 !== proof.capsuleSha512 || proof.embeddedManifestSha512 !== proof.manifestSha512) throw new Error('Invalid Linux sandbox build provenance proof');
 }
 for (let index = 0; index < names.length; index++) {
  const artifact = record.artifacts[index];
  if (!exact(artifact, ['name', 'size', 'sha512']) || artifact.name !== names[index] || !Number.isSafeInteger(artifact.size) || artifact.size < 1 || artifact.size > 4 * 1024 ** 3
   || typeof artifact.sha512 !== 'string' || !HASH.test(artifact.sha512) || Buffer.from(artifact.sha512, 'base64').toString('base64') !== artifact.sha512) throw new Error('Invalid build provenance artifact');
  const matches = signedAssets.filter(asset => asset?.name === artifact.name);
  if (matches.length !== 1 || matches[0].size !== artifact.size || matches[0].sha512 !== artifact.sha512) throw new Error('Build provenance artifact mismatch: ' + artifact.name);
 }
}
async function artifactRecord(directory, name) {
 const file = join(directory, name), before = await lstat(file);
 if (!before.isFile() || before.isSymbolicLink() || before.nlink !== 1 || before.size < 1 || before.size > 4 * 1024 ** 3) throw new Error('Invalid native build artifact: ' + name);
 const handle = await open(file, 'r');
 try {
  const initial = await handle.stat();
  if (!initial.isFile() || initial.ino !== before.ino || initial.dev !== before.dev || initial.size !== before.size) throw new Error('Native build artifact changed: ' + name);
  const hash = createHash('sha512'); let size = 0;
  for await (const chunk of handle.createReadStream({ autoClose: false })) { size += chunk.byteLength; if (size > initial.size) throw new Error('Native build artifact grew: ' + name); hash.update(chunk); }
  const final = await handle.stat();
  if (size !== initial.size || final.size !== initial.size || final.mtimeMs !== initial.mtimeMs) throw new Error('Native build artifact changed: ' + name);
  return { name, size, sha512: hash.digest('base64') };
 } finally { await handle.close(); }
}

/** Called after the native dist/fuse steps. Source facts are read from Git and
 * the trusted Actions environment; the CLI has no commit/tree/approval flags. */
export async function emitBuildProvenance(directory, target, options = {}) {
 const repositoryRoot = options.repositoryRoot ?? resolve(import.meta.dirname, '../../..'), environment = options.environment ?? process.env;
 if (environment.GITHUB_ACTIONS !== 'true' || environment.GITHUB_REPOSITORY !== 'jonasthim/dilla') throw new Error('Native build provenance requires this repository’s GitHub Actions run');
 if (!/^[1-9]\d*$/.test(environment.GITHUB_RUN_ID ?? '') || !Number.isSafeInteger(Number(environment.GITHUB_RUN_ID))) throw new Error('Invalid build provenance CI run');
 const native = PLATFORMS[target]; if (!native || native[0] !== process.platform || native[1] !== process.arch) throw new Error('Build provenance requires its actual native platform');
 const git = args => execFileSync('git', args, { cwd: repositoryRoot, encoding: 'utf8' }).trim();
 const sourceCommit = git(['rev-parse', 'HEAD']), sourceTree = git(['rev-parse', 'HEAD^{tree}']);
 if (!COMMIT.test(sourceCommit) || !COMMIT.test(sourceTree) || sourceCommit !== environment.GITHUB_SHA) throw new Error('Native build Git HEAD differs from GITHUB_SHA');
 if (git(['status', '--porcelain', '--untracked-files=all']) !== '') throw new Error('Native build provenance requires a clean source checkout');
 const version = JSON.parse(await readFile(join(repositoryRoot, 'packages/desktop/package.json'), 'utf8')).version;
 const artifacts = []; for (const name of buildArtifactNames(target, version)) artifacts.push(await artifactRecord(directory, name));
 let linuxSandbox;
 if (target === 'linux-x64') {
  const { inspectAppImage } = await import('./inspect-linux-sandbox-package.mjs');
  linuxSandbox = await inspectAppImage({ image: join(directory, artifacts[0].name), capsuleFile: join(directory, artifacts[1].name), manifestFile: join(directory, artifacts[2].name) });
  for (let i=0;i<artifacts.length;i++) {
   const after=await artifactRecord(directory, artifacts[i].name);
   if (after.size!==artifacts[i].size || after.sha512!==artifacts[i].sha512) throw new Error('Linux artifact changed during final-image inspection');
  }
 }
 if (target === 'linux-packages-x64') {
  const { verifyAurRecipe } = await import('./build-native-linux-packages.mjs');
  await verifyAurRecipe(version, join(directory, artifacts[0].name), join(directory, artifacts[2].name));
  for (const artifact of artifacts) { const after = await artifactRecord(directory, artifact.name); if (after.size !== artifact.size || after.sha512 !== artifact.sha512) throw new Error('Native package changed during recipe inspection'); }
 }
 const record = { format: 1, kind: 'dilla-desktop-build', repository: 'jonasthim/dilla', target, version, sourceCommit, sourceTree, ciRun: Number(environment.GITHUB_RUN_ID), sourceClean: true, artifacts, ...(linuxSandbox ? {linuxSandbox} : {}) };
 verifyBuildProvenance(record, target, { commit: sourceCommit, tree: sourceTree, ciRun: record.ciRun, version }, artifacts);
 const file = join(directory, buildProvenanceName(target)); await writeFile(file, JSON.stringify(record) + '\n', { flag: 'wx' }); return { file, record };
}
export async function readBuildProvenanceFile(file) {
 const info = await lstat(file); if (!info.isFile() || info.isSymbolicLink() || info.size < 1 || info.size > 8192) throw new Error('Invalid build provenance file');
 const bytes = await readFile(file); if (bytes.length > 8192) throw new Error('Build provenance file is too large'); return bytes;
}
/** Candidate records are public, but cannot replace the independent record
 * delivered alongside the exact CI installers from the accepted run. */
export async function verifyCandidateBuildRecord(candidateFile, downloadedFile) {
 const candidate = await readBuildProvenanceFile(candidateFile), downloaded = await readBuildProvenanceFile(downloadedFile);
 if (!candidate.equals(downloaded)) throw new Error('CI-downloaded build provenance differs from the signing candidate');
}
if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
 if (process.argv.length !== 4) throw new Error('Usage: build-provenance.mjs ARTIFACT_DIRECTORY NATIVE_TARGET');
 const result = await emitBuildProvenance(resolve(process.argv[2]), process.argv[3]); console.log('Recorded native build provenance: ' + basename(result.file));
}
