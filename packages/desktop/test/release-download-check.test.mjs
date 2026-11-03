import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execFileSync } from 'node:child_process';
import { checkDownloads } from '../scripts/release-download-check.mjs';
import { EXPECTED_ARTIFACTS } from '../scripts/ci-run.mjs';

const sha256 = text => createHash('sha256').update(text).digest('hex');
const digest = i => 'sha256:' + String(i % 10).repeat(64);

function scenario() {
 const root = mkdtempSync(join(tmpdir(), 'dilla-download-check-'));
 const repo = join(root, 'repo'), temp = join(root, 'temp'), gate = join(temp, 'gate');
 mkdirSync(repo); mkdirSync(gate, { recursive: true });
 const git = args => execFileSync('git', args, { cwd: repo, encoding: 'utf8', env: { ...process.env, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_SYSTEM: '/dev/null' } }).trim();
 git(['init', '-q']); writeFileSync(join(repo, 'README.md'), 'x\n'); writeFileSync(join(repo, '.gitignore'), 'ignored/\n');
 git(['add', '.']); git(['-c', 'user.name=t', '-c', 'user.email=t@t', 'commit', '-qm', 'init']);
 writeFileSync(join(gate, 'ci-run.json'), '{"id":77}\n'); writeFileSync(join(gate, 'ci-jobs.json'), '[]\n');
 let artifacts = EXPECTED_ARTIFACTS.map((name, i) => ({ id: 300 + i, name, digest: digest(i), expired: false }));
 const api = async path => {
  assert.equal(path, '/repos/dilla-chat/dilla/actions/runs/77/artifacts?per_page=100&page=1');
  return { artifacts };
 };
 const env = { RUN: '77', ARTIFACT_IDS: artifacts.map(a => a.id).join(','), ARTIFACT_DIGESTS: artifacts.map(a => a.digest).join(','), RUN_RECORD: sha256('{"id":77}\n'), JOBS_RECORD: sha256('[]\n') };
 const check = (overrides = {}) => checkDownloads({ env: { ...env, ...overrides }, git, api, gateDirectory: gate, candidateDirectory: join(temp, 'candidate'), signedDirectory: join(temp, 'signed') });
 return { root, repo, temp, gate, env, check, setArtifacts: list => { artifacts = list; }, artifacts: () => artifacts, cleanup: () => rmSync(root, { recursive: true, force: true }) };
}

test('download check passes for a clean workspace, untouched gate records and the gate digests', async () => {
 const s = scenario();
 try { await s.check(); } finally { s.cleanup(); }
});

test('download check refuses artifact digests that differ from the gate outputs', async () => {
 const s = scenario();
 try {
  const changed = s.artifacts().map((a, i) => i === 3 ? { ...a, digest: 'sha256:' + 'f'.repeat(64) } : a);
  s.setArtifacts(changed);
  await assert.rejects(s.check(), /digest/i);
  s.setArtifacts(s.artifacts().map((a, i) => i === 3 ? { ...a, digest: digest(3), id: 999 } : a));
  await assert.rejects(s.check(), /digest|id/i);
  s.setArtifacts(s.artifacts().map((a, i) => i === 3 ? { ...a, id: 303 } : a));
  await s.check();
  await assert.rejects(s.check({ ARTIFACT_DIGESTS: s.env.ARTIFACT_DIGESTS.split(',').slice(1).join(',') }), /ARTIFACT_DIGESTS/);
  await assert.rejects(s.check({ ARTIFACT_IDS: '1\n2' }), /ARTIFACT_IDS/);
 } finally { s.cleanup(); }
});

test('download check refuses files written into the workspace, including ignored paths', async () => {
 for (const path of ['packages/desktop/scripts/release-sign-job.mjs', 'ignored/node_modules/x.js', 'README.md']) {
  const s = scenario();
  try {
   mkdirSync(join(s.repo, path, '..'), { recursive: true }); writeFileSync(join(s.repo, path), 'planted');
   await assert.rejects(s.check(), /workspace/i, path);
  } finally { s.cleanup(); }
 }
});

test('download check refuses changed or extra gate records and pre-existing candidate or signed directories', async () => {
 const cases = [
  s => writeFileSync(join(s.gate, 'ci-run.json'), '{"id":78}\n'),
  s => writeFileSync(join(s.gate, 'ci-jobs.json'), '[{"name":"forged"}]\n'),
  s => writeFileSync(join(s.gate, 'acceptance.json'), '{}'),
  s => mkdirSync(join(s.temp, 'candidate')),
  s => mkdirSync(join(s.temp, 'signed')),
 ];
 for (const change of cases) {
  const s = scenario();
  try { change(s); await assert.rejects(s.check(), /gate record|already exists/i); } finally { s.cleanup(); }
 }
});
