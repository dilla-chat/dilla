import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, writeFile, readFile, readdir, rm, symlink, copyFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { collectArtifacts, assembleAcceptance } from '../scripts/release-assemble.mjs';
import { buildReleaseFixture, isolatedReleaseTools } from './support/release-fixture.mjs';

const script = resolve(import.meta.dirname, '../scripts/release-assemble.mjs');
const SHA = '2'.repeat(40);

/** A downloaded artifact tree (one directory per artifact), a gate directory and an empty candidate path. */
async function setup() {
 const root = await mkdtemp(join(tmpdir(), 'dilla-assemble-'));
 const fixture = await buildReleaseFixture({ directory: await mkdir(join(root, 'fixture'), { recursive: true }).then(() => join(root, 'fixture')), commit: SHA, head: SHA });
 const downloadRoot = join(root, 'downloaded'), gate = join(root, 'gate'), candidate = join(root, 'candidate');
 await fixture.writeArtifactLayout(downloadRoot);
 await mkdir(gate);
 await writeFile(join(gate, 'ci-run.json'), JSON.stringify(fixture.run));
 await writeFile(join(gate, 'ci-jobs.json'), JSON.stringify(fixture.jobs));
 return { root, fixture, downloadRoot, gate, candidate, version: fixture.version, cleanup: () => rm(root, { recursive: true, force: true }) };
}
const collected = s => collectArtifacts({ downloadRoot: s.downloadRoot, candidateDirectory: s.candidate, version: s.version });
const cli = (...args) => spawnSync(process.execPath, [script, ...args], { encoding: 'utf8', env: { PATH: process.env.PATH }, timeout: 60000 });

test('collect copies exactly the expected files, one artifact directory at a time', async () => {
 const s = await setup();
 try {
  await collected(s);
  const expected = Object.values(s.fixture.files).flatMap(byTarget => Object.values(byTarget).flat()).sort();
  assert.deepEqual((await readdir(s.candidate)).sort(), expected);
  for (const name of expected) assert.deepEqual(await readFile(join(s.candidate, name)), await readFile(join(s.fixture.directory, name)));
 } finally { await s.cleanup(); }
});

test('collect refuses a file name collision across artifacts', async () => {
 const s = await setup();
 try {
  // A real file name placed in a second artifact, and a candidate that already holds a file.
  await copyFile(join(s.downloadRoot, 'desktop-installers-win-x64', `dilla-${s.version}-win-x64.exe`), join(s.downloadRoot, 'desktop-installers-mac-arm64', `dilla-${s.version}-win-x64.exe`));
  await assert.rejects(collected(s), /unexpected|collision|EEXIST/i);
  await rm(join(s.downloadRoot, 'desktop-installers-mac-arm64', `dilla-${s.version}-win-x64.exe`));
  await rm(s.candidate, { recursive: true, force: true });
  await mkdir(s.candidate); await writeFile(join(s.candidate, `dilla-${s.version}-win-x64.exe`), 'pre-existing');
  await assert.rejects(collected(s), /EEXIST/);
  assert.equal(await readFile(join(s.candidate, `dilla-${s.version}-win-x64.exe`), 'utf8'), 'pre-existing');
 } finally { await s.cleanup(); }
});

test('collect refuses an artifact that carries ci-run.json or ci-jobs.json', async () => {
 for (const name of ['ci-run.json', 'ci-jobs.json']) {
  const s = await setup();
  try {
   await writeFile(join(s.downloadRoot, 'desktop-performance-linux-x64', name), '{"forged":true}');
   await assert.rejects(collected(s), /unexpected/i);
   const result = cli('collect', s.version, s.downloadRoot, s.candidate, s.gate);
   assert.notEqual(result.status, 0);
  } finally { await s.cleanup(); }
 }
 // Even if an artifact's file reached the candidate, the gate's copy is never replaced.
 const s = await setup();
 try {
  assert.equal(cli('collect', s.version, s.downloadRoot, s.candidate, s.gate).status, 0);
  assert.deepEqual(await readFile(join(s.candidate, 'ci-run.json')), await readFile(join(s.gate, 'ci-run.json')));
  assert.deepEqual(await readFile(join(s.candidate, 'ci-jobs.json')), await readFile(join(s.gate, 'ci-jobs.json')));
  const again = join(s.root, 'again'); await mkdir(again); await writeFile(join(again, 'ci-run.json'), '{"forged":true}');
  // collect into a candidate that already holds ci-run.json (as an artifact copy would) fails on the gate copy.
  await rm(s.candidate, { recursive: true });
  await mkdir(s.candidate); await writeFile(join(s.candidate, 'ci-run.json'), '{"forged":true}');
  const result = cli('collect', s.version, s.downloadRoot, s.candidate, s.gate);
  assert.notEqual(result.status, 0);
  assert.equal(await readFile(join(s.candidate, 'ci-run.json'), 'utf8'), '{"forged":true}', 'nothing overwritten');
 } finally { await s.cleanup(); }
});

test('collect refuses unexpected files and symlinks', async () => {
 const cases = [
  async s => writeFile(join(s.downloadRoot, 'desktop-installers-linux-x64', 'extra.txt'), 'x'),
  async s => mkdir(join(s.downloadRoot, 'desktop-installers-linux-x64', 'nested')),
  async s => mkdir(join(s.downloadRoot, 'desktop-installers-zz')),
  async s => writeFile(join(s.downloadRoot, 'stray.json'), '{}'),
  async s => rm(join(s.downloadRoot, 'desktop-installed-account-win-x64'), { recursive: true }),
  async s => rm(join(s.downloadRoot, 'desktop-installers-win-x64', `dilla-${s.version}-win-x64.exe`)),
  async s => { const file = join(s.downloadRoot, 'desktop-installers-win-x64', `dilla-${s.version}-win-x64.exe`); await rm(file); await symlink(join(s.fixture.directory, `dilla-${s.version}-win-x64.exe`), file); },
  async s => { const dir = join(s.downloadRoot, 'desktop-performance-win-x64'); await rm(dir, { recursive: true }); await symlink(join(s.downloadRoot, 'desktop-performance-linux-x64'), dir); },
 ];
 for (const [index, mutate] of cases.entries()) {
  const s = await setup();
  try {
   await mutate(s);
   await assert.rejects(collected(s), /unexpected|missing|symlink|regular|ENOENT/i, String(index));
   assert.deepEqual(await readdir(s.candidate).catch(() => []), [], 'nothing is copied before every artifact is validated');
  } finally { await s.cleanup(); }
 }
});

test('collect never merges: the same file in two artifacts fails', async () => {
 const s = await setup();
 try {
  const name = 'performance-linux-x64.json';
  await writeFile(join(s.downloadRoot, 'desktop-installed-account-linux-x64', name), 'substitute');
  await assert.rejects(collected(s), /unexpected/i);
  assert.deepEqual(await readdir(s.candidate).catch(() => []), []);
 } finally { await s.cleanup(); }
});

test('assembleAcceptance derives every digest from the collected files', async () => {
 const s = await setup();
 try {
  await collected(s);
  await writeFile(join(s.candidate, 'release-notes.md'), s.fixture.notes);
  const { acceptance } = await assembleAcceptance({ candidateDirectory: s.candidate, run: s.fixture.run, jobs: s.fixture.jobs, version: s.version, commitTree: s.fixture.source.tree });
  assert.deepEqual(acceptance, s.fixture.source);
  assert.deepEqual(Object.keys(acceptance), ['format', 'ciRun', 'commit', 'head', 'tree', 'review', 'notes', 'builds', 'performance', 'installedAccounts']);
  // A changed collected file changes its digest.
  await writeFile(join(s.candidate, 'performance-win-x64.json'), 'changed');
  const changed = await assembleAcceptance({ candidateDirectory: s.candidate, run: s.fixture.run, jobs: s.fixture.jobs, version: s.version, commitTree: s.fixture.source.tree });
  assert.equal(changed.acceptance.performance['win-x64'].sha256, createHash('sha256').update('changed').digest('hex'));
  await assert.rejects(assembleAcceptance({ candidateDirectory: s.candidate, run: s.fixture.run, jobs: s.fixture.jobs, version: s.version, commitTree: 'HEAD' }), /tree/i);
  await assert.rejects(assembleAcceptance({ candidateDirectory: s.candidate, run: { ...s.fixture.run, head_sha: 'x' }, jobs: s.fixture.jobs, version: s.version, commitTree: s.fixture.source.tree }), /run/i);
 } finally { await s.cleanup(); }
});

test('assembleAcceptance output is accepted by verifyDesktopSigningCandidate', async () => {
 const s = await setup();
 try {
  assert.equal(cli('collect', s.version, s.downloadRoot, s.candidate, s.gate).status, 0);
  await writeFile(join(s.candidate, 'release-notes.md'), s.fixture.notes);
  const result = cli('assemble', s.version, s.candidate, s.fixture.source.tree);
  assert.equal(result.status, 0, result.stderr);
  const acceptance = JSON.parse(await readFile(join(s.candidate, 'acceptance.json'), 'utf8'));
  const { verifier } = await isolatedReleaseTools(join(s.root, 'tools'), { trust: s.fixture.trust, sandboxConfig: s.fixture.sandbox.config });
  const release = { ...s.fixture.release, source: acceptance };
  const context = { ...s.fixture.resign(release), directory: s.candidate, feeds: { arm64: await readFile(join(s.fixture.directory, 'appcast-arm64.xml')) } };
  assert.equal((await verifier.verifyDesktopSigningCandidate(context)).version, s.version);
 } finally { await s.cleanup(); }
});

test('assembleAcceptance refuses jobs lacking a required job or step', async () => {
 const s = await setup();
 try {
  await collected(s);
  await writeFile(join(s.candidate, 'release-notes.md'), s.fixture.notes);
  const base = { candidateDirectory: s.candidate, run: s.fixture.run, version: s.version, commitTree: s.fixture.source.tree };
  await assert.rejects(assembleAcceptance({ ...base, jobs: s.fixture.jobs.filter(job => !job.name.endsWith('desktop-voice-win-x64')) }), /exactly once/);
  const noStep = structuredClone(s.fixture.jobs); noStep.find(job => job.name.endsWith('desktop-mac-arm64')).steps.pop();
  await assert.rejects(assembleAcceptance({ ...base, jobs: noStep }), /step/i);
 } finally { await s.cleanup(); }
});

test('assemble CLI never overwrites acceptance.json', async () => {
 const s = await setup();
 try {
  assert.equal(cli('collect', s.version, s.downloadRoot, s.candidate, s.gate).status, 0);
  await writeFile(join(s.candidate, 'release-notes.md'), s.fixture.notes);
  await writeFile(join(s.candidate, 'acceptance.json'), 'existing');
  const result = cli('assemble', s.version, s.candidate, s.fixture.source.tree);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /EEXIST/);
  assert.equal(await readFile(join(s.candidate, 'acceptance.json'), 'utf8'), 'existing');
  assert.notEqual(cli('assemble', s.version, s.candidate).status, 0, 'usage');
 } finally { await s.cleanup(); }
});
