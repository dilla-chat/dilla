import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, readFile, readdir, rm, symlink, mkdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { EXPECTED_FILES, writeDigest, verifyDigest } from '../scripts/candidate-digest.mjs';
import { publicReleaseRecords } from '../scripts/verify-release.mjs';

const script = resolve(import.meta.dirname, '../scripts/candidate-digest.mjs');
const VERSION = '0.1.0-beta.1', CHANNEL = 'beta';

async function signedOutput({ staged = false } = {}) {
 const root = await mkdtemp(join(tmpdir(), 'dilla-digest-'));
 const directory = join(root, 'signed'); await mkdir(directory);
 for (const name of EXPECTED_FILES(CHANNEL, VERSION, { staged })) await writeFile(join(directory, name), `content of ${name}\n`);
 return { root, directory, options: { channel: CHANNEL, version: VERSION, staged }, cleanup: () => rm(root, { recursive: true, force: true }) };
}

test('digest round trip verifies', async () => {
 const s = await signedOutput();
 try {
  const hex = await writeDigest(s.directory, s.options);
  assert.match(hex, /^[a-f0-9]{64}$/);
  const sums = await readFile(join(s.directory, 'SHA256SUMS'));
  assert.equal(hex, createHash('sha256').update(sums).digest('hex'));
  const lines = sums.toString().trimEnd().split('\n');
  assert.deepEqual(lines.map(line => line.split('  ')[1]), [...EXPECTED_FILES(CHANNEL, VERSION, { staged: false })].sort());
  await verifyDigest(s.directory, hex, s.options);
  // The CLI forms: write appends a validated digest output, verify accepts it.
  const t = await signedOutput();
  try {
   const output = join(t.root, 'github-output'); await writeFile(output, '');
   const written = spawnSync(process.execPath, [script, 'write', t.directory, CHANNEL, VERSION, 'false'], { encoding: 'utf8', env: { PATH: process.env.PATH, GITHUB_OUTPUT: output } });
   assert.equal(written.status, 0, written.stderr);
   const digest = /^digest=([a-f0-9]{64})\n$/.exec(await readFile(output, 'utf8'))[1];
   const verified = spawnSync(process.execPath, [script, 'verify', t.directory, CHANNEL, VERSION, 'false', digest], { encoding: 'utf8', env: { PATH: process.env.PATH } });
   assert.equal(verified.status, 0, verified.stderr);
   const bad = spawnSync(process.execPath, [script, 'verify', t.directory, CHANNEL, VERSION, 'false', 'f'.repeat(64)], { encoding: 'utf8', env: { PATH: process.env.PATH } });
   assert.notEqual(bad.status, 0);
   for (const staged of ['yes', '1', '']) assert.notEqual(spawnSync(process.execPath, [script, 'verify', t.directory, CHANNEL, VERSION, staged, digest], { encoding: 'utf8', env: { PATH: process.env.PATH } }).status, 0);
  } finally { await t.cleanup(); }
 } finally { await s.cleanup(); }
});

test('verify refuses one flipped byte in any file', async () => {
 const s = await signedOutput();
 try {
  const hex = await writeDigest(s.directory, s.options);
  for (const name of await readdir(s.directory)) {
   const file = join(s.directory, name), clean = await readFile(file), flipped = Buffer.from(clean);
   flipped[0] ^= 1;
   await writeFile(file, flipped);
   await assert.rejects(verifyDigest(s.directory, hex, s.options), /digest|mismatch/i, name);
   await writeFile(file, clean);
  }
  await verifyDigest(s.directory, hex, s.options);
 } finally { await s.cleanup(); }
});

test('verify refuses an extra file, a missing file and a symlink', async () => {
 const s = await signedOutput();
 try {
  const hex = await writeDigest(s.directory, s.options);
  await writeFile(join(s.directory, 'extra.txt'), 'x');
  await assert.rejects(verifyDigest(s.directory, hex, s.options), /exact|unexpected/i);
  await rm(join(s.directory, 'extra.txt'));
  const victim = join(s.directory, 'release-notes.md'), clean = await readFile(victim);
  await rm(victim);
  await assert.rejects(verifyDigest(s.directory, hex, s.options), /exact|missing/i);
  await writeFile(join(s.root, 'outside.md'), clean); await symlink(join(s.root, 'outside.md'), victim);
  await assert.rejects(verifyDigest(s.directory, hex, s.options), /symlink|regular/i);
 } finally { await s.cleanup(); }
 const w = await signedOutput();
 try {
  await writeFile(join(w.directory, 'extra.txt'), 'x');
  await assert.rejects(writeDigest(w.directory, w.options), /exact|unexpected/i);
 } finally { await w.cleanup(); }
});

test('verify refuses a substituted SHA256SUMS with a consistent set', async () => {
 const s = await signedOutput();
 try {
  const hex = await writeDigest(s.directory, s.options);
  // Replace a file and rewrite SHA256SUMS so it describes the new set exactly.
  await writeFile(join(s.directory, 'desktop-beta.json'), 'substituted metadata');
  await rm(join(s.directory, 'SHA256SUMS'));
  const substituted = await writeDigest(s.directory, s.options);
  assert.notEqual(substituted, hex);
  await assert.rejects(verifyDigest(s.directory, hex, s.options), /SHA256SUMS/);
 } finally { await s.cleanup(); }
});

test('write refuses to overwrite SHA256SUMS', async () => {
 const s = await signedOutput();
 try {
  await writeFile(join(s.directory, 'SHA256SUMS'), 'existing');
  await assert.rejects(writeDigest(s.directory, s.options), /SHA256SUMS|EEXIST/);
  assert.equal(await readFile(join(s.directory, 'SHA256SUMS'), 'utf8'), 'existing');
 } finally { await s.cleanup(); }
});

test('rotation-proof.json is expected only when staged', async () => {
 assert.deepEqual(EXPECTED_FILES(CHANNEL, VERSION, { staged: false }), [...publicReleaseRecords(CHANNEL), `dilla-${VERSION}-linux-x64.sandbox.sig`].sort());
 assert.deepEqual(EXPECTED_FILES(CHANNEL, VERSION, { staged: true }), [...publicReleaseRecords(CHANNEL), `dilla-${VERSION}-linux-x64.sandbox.sig`, 'rotation-proof.json'].sort());
 assert.throws(() => EXPECTED_FILES(CHANNEL, VERSION, { staged: 'true' }), /staged/i);
 assert.throws(() => EXPECTED_FILES('nightly', VERSION, { staged: false }), /channel/i);
 assert.throws(() => EXPECTED_FILES(CHANNEL, '1.0', { staged: false }), /version/i);
 const unstaged = await signedOutput();
 try {
  await writeFile(join(unstaged.directory, 'rotation-proof.json'), '{}');
  await assert.rejects(writeDigest(unstaged.directory, unstaged.options), /exact|unexpected/i);
 } finally { await unstaged.cleanup(); }
 const staged = await signedOutput({ staged: true });
 try {
  const hex = await writeDigest(staged.directory, staged.options);
  await verifyDigest(staged.directory, hex, staged.options);
  await assert.rejects(verifyDigest(staged.directory, hex, { ...staged.options, staged: false }), /exact|unexpected/i);
  await rm(join(staged.directory, 'rotation-proof.json'));
  await assert.rejects(verifyDigest(staged.directory, hex, staged.options), /exact|missing/i);
 } finally { await staged.cleanup(); }
});
