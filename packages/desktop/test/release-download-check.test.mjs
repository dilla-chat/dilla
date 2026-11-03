import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { checkDownloads } from '../scripts/release-download-check.mjs';
import { EXPECTED_ARTIFACTS } from '../scripts/ci-run.mjs';

const sha256 = text => createHash('sha256').update(text).digest('hex');
const digest = i => 'sha256:' + String(i % 10).repeat(64);

function scenario() {
 const root = mkdtempSync(join(tmpdir(), 'dilla-download-check-'));
 const gate = join(root, 'gate'); mkdirSync(gate);
 writeFileSync(join(gate, 'ci-run.json'), '{"id":77}\n'); writeFileSync(join(gate, 'ci-jobs.json'), '[]\n');
 let artifacts = EXPECTED_ARTIFACTS.map((name, i) => ({ id: 300 + i, name, digest: digest(i), expired: false }));
 const api = async path => {
  assert.equal(path, '/repos/dilla-chat/dilla/actions/runs/77/artifacts?per_page=100&page=1');
  return { artifacts };
 };
 const env = { RUN: '77', ARTIFACT_IDS: artifacts.map(a => a.id).join(','), ARTIFACT_DIGESTS: artifacts.map(a => a.digest).join(','), RUN_RECORD: sha256('{"id":77}\n'), JOBS_RECORD: sha256('[]\n') };
 const check = (overrides = {}) => checkDownloads({ env: { ...env, ...overrides }, api, gateDirectory: gate, candidateDirectory: join(root, 'candidate') });
 return { root, gate, env, check, setArtifacts: list => { artifacts = list; }, artifacts: () => artifacts, cleanup: () => rmSync(root, { recursive: true, force: true }) };
}

test('download check passes for untouched gate records and the gate digests', async () => {
 const s = scenario();
 try { await s.check(); } finally { s.cleanup(); }
});

test('download check refuses artifact digests that differ from the gate outputs', async () => {
 const s = scenario();
 try {
  s.setArtifacts(s.artifacts().map((a, i) => i === 3 ? { ...a, digest: 'sha256:' + 'f'.repeat(64) } : a));
  await assert.rejects(s.check(), /digest/i);
  s.setArtifacts(s.artifacts().map((a, i) => i === 3 ? { ...a, digest: digest(3), id: 999 } : a));
  await assert.rejects(s.check(), /digest|id/i);
  s.setArtifacts(s.artifacts().map((a, i) => i === 3 ? { ...a, id: 303 } : a));
  await s.check();
  await assert.rejects(s.check({ ARTIFACT_DIGESTS: s.env.ARTIFACT_DIGESTS.split(',').slice(1).join(',') }), /ARTIFACT_DIGESTS/);
  await assert.rejects(s.check({ ARTIFACT_IDS: '1\n2' }), /ARTIFACT_IDS/);
 } finally { s.cleanup(); }
});

test('download check refuses changed or extra gate records and a pre-existing candidate directory', async () => {
 const cases = [
  s => writeFileSync(join(s.gate, 'ci-run.json'), '{"id":78}\n'),
  s => writeFileSync(join(s.gate, 'ci-jobs.json'), '[{"name":"forged"}]\n'),
  s => writeFileSync(join(s.gate, 'acceptance.json'), '{}'),
  s => mkdirSync(join(s.root, 'candidate')),
 ];
 for (const change of cases) {
  const s = scenario();
  try { change(s); await assert.rejects(s.check(), /gate record|already exists/i); } finally { s.cleanup(); }
 }
});
