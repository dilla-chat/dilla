import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, writeFile, readFile, realpath, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { verifyTestTools } from '../../../scripts/e2e-test-tools.mjs';
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

test('verified generated tooling leaves the actual Git source clean while other changes still block the real proof CLI', async () => {
 const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-account-clean-checkout-'))), checkout = join(root, 'checkout');
 try {
  await mkdir(checkout); await mkdir(join(root, 'empty-hooks'));
  const git = (args: string[]) => execFileSync('git', ['-c', 'user.name=Owned fixture', '-c', 'user.email=fixture@invalid', '-c', 'commit.gpgsign=false', '-c', 'core.hooksPath=' + join(root, 'empty-hooks'), ...args], { cwd: checkout, encoding: 'utf8' }).trim();
  await writeFile(join(checkout, '.gitignore'), await readFile(new URL('../../../.gitignore', import.meta.url)));
  const scripts = join(checkout, 'packages/desktop/scripts'); await mkdir(scripts, { recursive: true });
  const cli = join(scripts, 'installed-account-proof.mjs'); await writeFile(cli, await readFile(new URL('../scripts/installed-account-proof.mjs', import.meta.url)));
  await writeFile(join(checkout, 'source.txt'), 'owned source\n');
  git(['init', '--quiet']); git(['add', '.']); git(['commit', '--quiet', '--no-gpg-sign', '-m', 'owned clean source']);
  const commit = git(['rev-parse', 'HEAD']), tools = join(checkout, 'artifacts/e2e-test-tools'); await mkdir(tools, { recursive: true });
  const files: Record<string, { size: number; sha256: string }> = {};
  for (const name of ['dilla-testhost', 'dilla-testkit']) {
   const bytes = Buffer.from('owned tool fixture ' + name); await writeFile(join(tools, name), bytes);
   files[name] = { size: bytes.length, sha256: createHash('sha256').update(bytes).digest('hex') };
  }
  await writeFile(join(tools, 'manifest.json'), JSON.stringify({ v: 1, sourceSha: commit, runId: '123', producerAttempt: '1', platform: 'darwin', arch: 'arm64', files }));
  const options = { repoRoot: checkout, env: { GITHUB_SHA: commit, GITHUB_RUN_ID: '123' }, platform: 'darwin', arch: 'arm64' };
  verifyTestTools(tools, options);
  assert.equal(git(['status', '--porcelain']), '', 'verified tool downloads are generated outputs, not source modifications');
  const input = join(root, 'results'), output = join(root, 'reports'); await mkdir(input);
  await writeFile(join(input, 'installed-account-proof.json'), JSON.stringify({ format: 1, platform: 'darwin', arch: 'arm64', from: '1.0.0', to: '1.0.1', ...Object.fromEntries(ACCOUNT_CHECKS.map((key: string) => [key, true])) }));
  const run = () => execFileSync(process.execPath, [cli, input, 'mac-arm64', output], { cwd: checkout, encoding: 'utf8', env: { ...process.env, GITHUB_OUTPUT: join(root, 'actions-output') }, stdio: ['ignore', 'pipe', 'pipe'] });
  assert.match(run(), /Verified installed-account-mac-arm64\.json/);
  const report = JSON.parse(await readFile(join(output, 'installed-account-mac-arm64.json'), 'utf8'));
  assert.equal(report.sourceDirty, false); assert.equal(report.sourceCommit, commit);
  await writeFile(join(tools, 'dilla-testhost'), 'tampered generated tool'); assert.throws(() => verifyTestTools(tools, options), /size|hash/);
  await writeFile(join(tools, 'dilla-testhost'), 'owned tool fixture dilla-testhost'); verifyTestTools(tools, options);
  await writeFile(join(checkout, 'source.txt'), 'changed source\n'); assert.notEqual(git(['status', '--porcelain']), ''); assert.throws(run, /clean source checkout/);
  await writeFile(join(checkout, 'source.txt'), 'owned source\n');
  await writeFile(join(checkout, 'artifacts/unknown-output.txt'), 'unexpected output'); assert.notEqual(git(['status', '--porcelain']), ''); assert.throws(run, /clean source checkout/);
 } finally { await rm(root, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 }); }
});
