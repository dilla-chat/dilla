import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, writeFile, readFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
// @ts-expect-error build tooling is JavaScript
import { collectInstalledAccountProof, verifyInstalledAccountReport, ACCOUNT_CHECKS } from '../scripts/installed-account-proof.mjs';

test('only the owning shard publishes a complete, clean, source-bound installed-account proof', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-account-proof-'));
 const source = { commit: 'a'.repeat(40), dirty: false };
 const proof = { format: 1, platform: 'linux', arch: 'x64', from: '1.0.0', to: '1.0.1', ...Object.fromEntries(ACCOUNT_CHECKS.map((key: string) => [key, true])) };
 try {
  const input = join(root, 'results'), output = join(root, 'reports'); await mkdir(input);
  assert.equal(await collectInstalledAccountProof(input, 'linux-x64', output, source), null);
  await mkdir(join(input, 'case')); const file = join(input, 'case/installed-account-proof.json');
  await writeFile(file, JSON.stringify(proof));
  const result = await collectInstalledAccountProof(input, 'linux-x64', output, source);
  const report = JSON.parse(await readFile(result.file, 'utf8'));
  assert.equal(report.sourceCommit, source.commit); assert.equal(report.sourceDirty, false);
  verifyInstalledAccountReport(report, 'linux-x64', source.commit);
  await assert.rejects(collectInstalledAccountProof(input, 'linux-x64', output, { ...source, dirty: true }), /clean/);
  await assert.rejects(collectInstalledAccountProof(input, 'win-x64', output, source), /platform/);
  for (const key of ACCOUNT_CHECKS) {
   await writeFile(file, JSON.stringify({ ...proof, [key]: false }));
   await assert.rejects(collectInstalledAccountProof(input, 'linux-x64', output, source), /incomplete/);
  }
  await writeFile(file, JSON.stringify({ ...proof, unexpectedPrivateData: 'must-not-upload' }));
  await assert.rejects(collectInstalledAccountProof(input, 'linux-x64', output, source), /unexpected/);
  await writeFile(file, JSON.stringify(proof)); await mkdir(join(input, 'second'));
  await writeFile(join(input, 'second/installed-account-proof.json'), JSON.stringify(proof));
  await assert.rejects(collectInstalledAccountProof(input, 'linux-x64', output, source), /ambiguous/);
  assert.throws(() => verifyInstalledAccountReport({ ...report, sourceCommit: 'b'.repeat(40) }, 'linux-x64', source.commit), /source/);
 } finally { await rm(root, { recursive: true, force: true }); }
});
