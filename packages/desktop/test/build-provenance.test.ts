import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { execFileSync, spawnSync } from 'node:child_process';
import { mkdtemp, mkdir, writeFile, readFile, copyFile, realpath, rm } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { tmpdir } from 'node:os';
// @ts-expect-error native packaging tooling is JavaScript
import { writeAurRecipe } from '../scripts/build-native-linux-packages.mjs';
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
test('every target binds exactly its native installable bytes, with no recovery sidecar', () => {
 assert.deepEqual(targets.map(value => buildArtifactNames(value, '1.2.3-beta.4')), [
  ['dilla-1.2.3-beta.4-linux-x64.AppImage', 'dilla-1.2.3-beta.4-linux-x64.sandbox', 'dilla-1.2.3-beta.4-linux-x64.sandbox.json'], ['dilla-1.2.3-beta.4-win-x64.exe'],
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
  if (target === 'linux-x64') { await assert.rejects(emitBuildProvenance(out, target, { repositoryRoot: root, environment }), /AppImage|EACCES|ENOENT|extract/i); return; }
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

test('Linux provenance requires extracted sandbox proof and binds external and embedded hashes', () => {
 const version='1.2.3', target='linux-x64', names=buildArtifactNames(target,version);
 const artifacts=names.map((name:string)=>({name,size:1,sha512:sha(Buffer.from(name))}));
 const record={format:1,kind:'dilla-desktop-build',repository:'jonasthim/dilla',target,version,sourceCommit:'1'.repeat(40),sourceTree:'2'.repeat(40),ciRun:1,sourceClean:true,artifacts};
 const expected={version,commit:record.sourceCommit,tree:record.sourceTree,ciRun:1};
 assert.throws(()=>verifyBuildProvenance(record,target,expected,artifacts),/sandbox|provenance/i);
 const proof={capsuleSha512:artifacts[1].sha512,manifestSha512:artifacts[2].sha512,embeddedCapsuleSha512:artifacts[1].sha512,embeddedManifestSha512:artifacts[2].sha512,electronSha512:sha(Buffer.from('fixture-elf')),asarSha512:sha(Buffer.from('fixture-asar'))};
 verifyBuildProvenance({...record,linuxSandbox:proof},target,expected,artifacts);
 for (const bad of [{...proof,embeddedCapsuleSha512:proof.manifestSha512},{...proof,capsuleSha512:proof.manifestSha512},{...proof,asarSha512:'bad'}, {...proof,embeddedManifestSha512:undefined}]) assert.throws(()=>verifyBuildProvenance({...record,linuxSandbox:bad},target,expected,artifacts),/provenance/i);
});


test('Linux emitter derives provenance from actual final image and exact sidecars', { skip: process.platform !== 'linux' || !process.env.DILLA_INSPECT_FIXTURE_IMAGE }, async () => {
 const root=await realpath(await mkdtemp(join(tmpdir(),'dilla-real-image-provenance-')));
 try {
  const out=join(root,'out');await mkdir(join(root,'packages/desktop'),{recursive:true});await mkdir(out);
  const version='0.1.0-beta.1', names=buildArtifactNames('linux-x64',version);
  await writeFile(join(root,'packages/desktop/package.json'),JSON.stringify({version}));await writeFile(join(root,'.gitignore'),'out/\n');
  const git=(args:string[])=>execFileSync('git',args,{cwd:root,encoding:'utf8'}).trim();
  git(['init','-q']);git(['config','core.hooksPath',join(root,'no-hooks')]);git(['add','.']);git(['-c','user.name=Build fixture','-c','user.email=build@invalid.example','-c','commit.gpgsign=false','commit','-q','-m','fixture']);
  const files=[process.env.DILLA_INSPECT_FIXTURE_IMAGE!,process.env.DILLA_INSPECT_FIXTURE_CAPSULE!,process.env.DILLA_INSPECT_FIXTURE_MANIFEST!];
  for (let i=0;i<names.length;i++) await copyFile(files[i],join(out,names[i]));
  const commit=git(['rev-parse','HEAD']), tree=git(['rev-parse','HEAD^{tree}']);
  const {record}=await emitBuildProvenance(out,'linux-x64',{repositoryRoot:root,environment:{GITHUB_ACTIONS:'true',GITHUB_REPOSITORY:'jonasthim/dilla',GITHUB_RUN_ID:'42',GITHUB_SHA:commit}});
  verifyBuildProvenance(record,'linux-x64',{version,commit,tree,ciRun:42},record.artifacts);
  assert.equal(record.linuxSandbox.capsuleSha512,record.artifacts[1].sha512);
  assert.equal(record.linuxSandbox.manifestSha512,record.artifacts[2].sha512);
 } finally {await rm(root,{recursive:true,force:true});}
});


test('native system packages have their own exact provenance inventory and source binding', () => {
 const version = '1.2.3-beta.4', target = 'linux-packages-x64';
 const names = ['deb', 'rpm', 'aur.tar.gz'].map(ext => `dilla-${version}-linux-x64.${ext}`);
 assert.deepEqual(buildArtifactNames(target, version), names);
 assert.equal(buildProvenanceName(target), 'build-provenance-linux-packages-x64.json');
 const artifacts = names.map(name => ({ name, size: 10, sha512: sha(Buffer.from(name)) }));
 const record = { format: 1, kind: 'dilla-desktop-build', repository: 'jonasthim/dilla', target, version, sourceCommit: '1'.repeat(40), sourceTree: '2'.repeat(40), ciRun: 42, sourceClean: true, artifacts };
 const expected = { version, commit: record.sourceCommit, tree: record.sourceTree, ciRun: 42 };
 verifyBuildProvenance(record, target, expected, artifacts);
 for (const fields of [{ sourceCommit: '3'.repeat(40) }, { sourceTree: '3'.repeat(40) }, { ciRun: 43 }, { sourceClean: false }, { target: 'linux-x64' }, { artifacts: artifacts.slice(1) }]) assert.throws(() => verifyBuildProvenance({ ...record, ...fields }, target, expected, artifacts), /provenance/i);
 assert.throws(() => verifyBuildProvenance(record, target, expected, artifacts.map(asset => ({ ...asset, sha512: sha(Buffer.from('other build')) }))), /artifact mismatch/i);
 assert.throws(() => verifyBuildProvenance(record, 'linux-x64', expected, artifacts), /provenance/i);
});


test('the native-package emitter reads a real clean checkout and independently observes its AUR source pin', { skip: process.platform !== 'linux' || process.arch !== 'x64' }, async () => {
 const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-native-build-source-')));
 try {
  const out = join(root, 'out'), version = '1.2.3-beta.4', target = 'linux-packages-x64';
  await mkdir(join(root, 'packages/desktop'), { recursive: true }); await mkdir(out);
  await writeFile(join(root, 'packages/desktop/package.json'), JSON.stringify({ version })); await writeFile(join(root, '.gitignore'), 'out/\n');
  const git = (args: string[]) => execFileSync('git', args, { cwd: root, encoding: 'utf8' }).trim();
  git(['init', '-q']); git(['config', 'core.hooksPath', join(root, 'no-hooks')]); git(['add', '.']); git(['-c', 'user.name=Build fixture', '-c', 'user.email=build@invalid.example', '-c', 'commit.gpgsign=false', 'commit', '-q', '-m', 'Fixture source']);
  const commit = git(['rev-parse', 'HEAD']), tree = git(['rev-parse', 'HEAD^{tree}']);
  const names = buildArtifactNames(target, version);
  await writeFile(join(out, names[0]), 'source-bound fixture DEB'); await writeFile(join(out, names[1]), 'source-bound fixture RPM');
  const archive = await writeAurRecipe(version, join(out, names[0]), join(out, 'recipe')); await copyFile(archive, join(out, names[2]));
  const environment = { GITHUB_ACTIONS: 'true', GITHUB_REPOSITORY: 'jonasthim/dilla', GITHUB_RUN_ID: '42', GITHUB_SHA: commit };
  const result = await emitBuildProvenance(out, target, { repositoryRoot: root, environment });
  verifyBuildProvenance(result.record, target, { version, commit, tree, ciRun: 42 }, result.record.artifacts);
  assert.equal(result.record.artifacts.length, 3); assert.equal(Object.hasOwn(result.record, 'linuxSandbox'), false);
  await rm(result.file);
  await assert.rejects(emitBuildProvenance(out, target, { repositoryRoot: root, environment: { ...environment, GITHUB_SHA: 'f'.repeat(40) } }), /GITHUB_SHA/);
  await writeFile(join(root, 'untracked-change'), 'dirty source');
  await assert.rejects(emitBuildProvenance(out, target, { repositoryRoot: root, environment }), /clean/);
  await rm(join(root, 'untracked-change')); await writeFile(join(out, names[0]), 'different DEB after recipe creation');
  await assert.rejects(emitBuildProvenance(out, target, { repositoryRoot: root, environment }), /recipe/i);
 } finally { await rm(root, { recursive: true, force: true }); }
});
