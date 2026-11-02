import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdtemp, mkdir, writeFile, readFile, copyFile, realpath, rm } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { tmpdir } from 'node:os';
import { createRequire } from 'node:module';
// @ts-expect-error build tooling is JavaScript
import { buildArtifactNames, buildProvenanceName, verifyBuildProvenance, emitBuildProvenance, verifyCandidateBuildRecord } from '../scripts/build-provenance.mjs';

const targets = ['linux-x64', 'win-x64', 'mac-x64', 'mac-arm64'];
const platform = ({ linux: 'linux', win32: 'win', darwin: 'mac' } as Record<string, string>)[process.platform];
const target = platform + '-' + process.arch;
const sha = (bytes: Uint8Array) => createHash('sha512').update(bytes).digest('base64');
const require = createRequire(import.meta.url);
function appImageTarget(version: string) {
 const config = require('../electron-builder.cjs');
 // Initialize the public entry point before its mutually dependent internals.
 require('app-builder-lib');
 const { PlatformPackager } = require('app-builder-lib/out/platformPackager');
 const AppImageTarget = require('app-builder-lib/out/targets/appimage/AppImageTarget').default;
 const packager = Object.create(PlatformPackager.prototype);
 Object.assign(packager, { info: { config }, platform: { buildConfigurationKey: 'linux' }, platformSpecificBuildOptions: config.linux,
  appInfo: { version, buildVersion: version, productName: 'Dilla', productFilename: 'Dilla', name: 'dilla-desktop' } });
 return new AppImageTarget('AppImage', packager, { computeDesktopEntry: (_options: unknown, exec: string) => exec }, 'unused-artifact-directory');
}
test('the pinned AppImage packager emits the exact authenticated Linux artifact name', () => {
 const target = appImageTarget('1.2.3-beta.4'), { Arch } = require('builder-util');
 assert.equal(target.packager.expandArtifactNamePattern(target.options, 'AppImage', Arch.x64), buildArtifactNames('linux-x64', '1.2.3-beta.4')[0]);
});
test('the actual AppImage desktop entry preserves Chromium sandboxing', async () => {
 const target = appImageTarget('1.2.3-beta.4');
 assert.equal(await target.desktopEntry.value, 'AppRun %U');
});
test('every target binds exactly its native installable bytes, including the Windows manifest', () => {
 assert.deepEqual(targets.map(value => buildArtifactNames(value, '1.2.3-beta.4')), [
  ['dilla-1.2.3-beta.4-linux-x64.AppImage'], ['dilla-1.2.3-beta.4-win-x64.exe', 'dilla-1.2.3-beta.4-win-x64.manifest.json'],
  ['dilla-1.2.3-beta.4-mac-x64.dmg', 'dilla-1.2.3-beta.4-mac-x64.zip'], ['dilla-1.2.3-beta.4-mac-arm64.dmg', 'dilla-1.2.3-beta.4-mac-arm64.zip'],
 ]);
 for (const value of ['../outside', 'linux-arm64', 'win32-x64']) assert.throws(() => buildArtifactNames(value, '1.2.3'));
 for (const version of ['../outside', '01.2.3', '1.2.3-unknown']) assert.throws(() => buildArtifactNames('linux-x64', version));
});

test('the CI emitter reads real clean Git HEAD/tree and GITHUB_SHA, never caller-supplied approval facts', async () => {
 // macOS aliases /var to /private/var; Node canonicalizes import.meta.url.
 // Give the copied CLI the same canonical path as its loaded module.
 const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-build-source-')));
 try {
  const scripts = join(root, 'packages/desktop/scripts'), out = join(root, 'out'); await mkdir(scripts, { recursive: true }); await mkdir(out);
  await copyFile(resolve(import.meta.dirname, '../scripts/build-provenance.mjs'), join(scripts, 'build-provenance.mjs'));
  await writeFile(join(root, 'packages/desktop/package.json'), JSON.stringify({ version: '1.2.3' })); await writeFile(join(root, '.gitignore'), 'out/\n');
  const git = (args: string[]) => execFileSync('git', args, { cwd: root, encoding: 'utf8' }).trim();
  git(['init', '-q']); git(['config', 'core.hooksPath', join(root, 'no-hooks')]); git(['add', '.']); git(['-c', 'user.name=Build fixture', '-c', 'user.email=build@invalid.example', '-c', 'commit.gpgsign=false', 'commit', '-q', '-m', 'Fixture source']);
  const commit = git(['rev-parse', 'HEAD']), tree = git(['rev-parse', 'HEAD^{tree}']);
  const artifacts: Array<{ name: string; size: number; sha512: string }> = [];
  for (const name of buildArtifactNames(target, '1.2.3')) { const bytes = Buffer.from('actual native artifact ' + name); await writeFile(join(out, name), bytes); artifacts.push({ name, size: bytes.length, sha512: sha(bytes) }); }
  const environment = { GITHUB_ACTIONS: 'true', GITHUB_REPOSITORY: 'jonasthim/dilla', GITHUB_RUN_ID: '42', GITHUB_SHA: commit };
  const result = await emitBuildProvenance(out, target, { repositoryRoot: root, environment });
  const report = JSON.parse(await readFile(result.file, 'utf8'));
  verifyBuildProvenance(report, target, { commit, tree, ciRun: 42, version: '1.2.3' }, artifacts);
  assert.equal(report.sourceCommit, commit); assert.equal(report.sourceTree, tree); assert.equal(report.sourceClean, true); assert.equal(report.ciRun, 42);
  assert.equal(result.file, join(out, buildProvenanceName(target)));
  for (const changed of [{ sourceCommit: 'f'.repeat(40) }, { sourceTree: 'e'.repeat(40) }, { ciRun: 43 }, { sourceClean: false }, { privateKey: 'must-not-upload' }]) assert.throws(() => verifyBuildProvenance({ ...report, ...changed }, target, { commit, tree, ciRun: 42, version: '1.2.3' }, artifacts), /build provenance/i);
  assert.throws(() => verifyBuildProvenance(report, target, { commit, tree, ciRun: 42, version: '1.2.3' }, artifacts.map(asset => ({ ...asset, sha512: sha(Buffer.from('tampered')) }))), /artifact/i);
  await rm(result.file);
  const cli = spawnSync(process.execPath, [join(scripts, 'build-provenance.mjs'), out, target], { cwd: root, encoding: 'utf8', env: { ...process.env, ...environment } });
  assert.equal(cli.status, 0, cli.stderr);
  assert.equal(cli.stdout.trim(), 'Recorded native build provenance: ' + buildProvenanceName(target));
  assert.deepEqual(JSON.parse(await readFile(result.file, 'utf8')), report, 'the actual CLI must produce the verified native report');
  await rm(result.file);
  await assert.rejects(emitBuildProvenance(out, target, { repositoryRoot: root, environment: { ...environment, GITHUB_SHA: 'f'.repeat(40) } }), /GITHUB_SHA/);
  await assert.rejects(emitBuildProvenance(out, target, { repositoryRoot: root, environment: { ...environment, GITHUB_REPOSITORY: 'untrusted/repo' } }), /repository/);
  await writeFile(join(root, 'untracked-change'), 'changed after build');
  await assert.rejects(emitBuildProvenance(out, target, { repositoryRoot: root, environment }), /clean/);
 } finally { await rm(root, { recursive: true, force: true }); }
});

test('a signing candidate cannot replace CI-downloaded build provenance with a forged source record', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-build-binding-'));
 try {
  const candidate = join(root, 'candidate.json'), downloaded = join(root, 'downloaded.json'), trusted = Buffer.from('{"sourceCommit":"actual-ci-commit"}');
  await writeFile(candidate, trusted); await writeFile(downloaded, trusted); await verifyCandidateBuildRecord(candidate, downloaded);
  await writeFile(candidate, '{"sourceCommit":"unrelated-new-code"}'); await assert.rejects(verifyCandidateBuildRecord(candidate, downloaded), /CI.*build provenance/i);
  assert.deepEqual(await readFile(downloaded), trusted, 'publisher must leave the trusted downloaded record untouched');
  await rm(downloaded); await assert.rejects(verifyCandidateBuildRecord(candidate, downloaded), /ENOENT|CI.*build provenance/i);
 } finally { await rm(root, { recursive: true, force: true }); }
});
