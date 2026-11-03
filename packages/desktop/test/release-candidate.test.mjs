import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { candidateFiles, verifyCandidate, writeCandidateDigest } from '../scripts/release-candidate.mjs';
import { assembleAcceptance, collectArtifacts } from '../scripts/release-assemble.mjs';
import { buildReleaseFixture, isolatedReleaseTools } from './support/release-fixture.mjs';

const SHA = '2'.repeat(40), TREE = '3'.repeat(40);
const sha256 = bytes => createHash('sha256').update(bytes).digest('hex');

/** The candidate exactly as the collect job uploads it, plus the env the sign job passes. */
async function setup() {
 const root = await mkdtemp(join(tmpdir(), 'dilla-candidate-'));
 const fixtureDirectory = join(root, 'fixture'); await mkdir(fixtureDirectory);
 const fixture = await buildReleaseFixture({ directory: fixtureDirectory, commit: SHA, head: SHA, tree: TREE });
 const download = join(root, 'downloaded'), candidate = join(root, 'candidate');
 await fixture.writeArtifactLayout(download);
 await collectArtifacts({ downloadRoot: download, candidateDirectory: candidate, version: fixture.version });
 const run = Buffer.from(JSON.stringify(fixture.run) + '\n'), jobs = Buffer.from(JSON.stringify(fixture.jobs) + '\n');
 await writeFile(join(candidate, 'ci-run.json'), run); await writeFile(join(candidate, 'ci-jobs.json'), jobs);
 await writeFile(join(candidate, 'release-notes.md'), `# Dilla desktop ${fixture.version}\n\nFirst desktop release.\n`);
 const { acceptance } = await assembleAcceptance({ candidateDirectory: candidate, run: fixture.run, jobs: fixture.jobs, version: fixture.version, commitTree: TREE });
 await writeFile(join(candidate, 'acceptance.json'), JSON.stringify(acceptance, null, 2) + '\n');
 const digest = await writeCandidateDigest(candidate, fixture.version);
 const env = { VERSION: fixture.version, SHA, TREE, RUN: String(fixture.run.id), RUN_RECORD: sha256(run), JOBS_RECORD: sha256(jobs), CANDIDATE_DIGEST: digest, PREVIOUS: '' };
 // The sign job's API is used only for notes; a first release needs none.
 const api = async path => { throw new Error('unexpected API call ' + path); };
 const redigest = async () => { await rm(join(candidate, 'SHA256SUMS')); env.CANDIDATE_DIGEST = await writeCandidateDigest(candidate, fixture.version); };
 return { root, fixture, candidate, env, api, redigest, verify: (changes = {}, withApi = true) => verifyCandidate({ directory: candidate, env: { ...env, ...changes }, ...(withApi ? { api } : {}) }), cleanup: () => rm(root, { recursive: true, force: true }) };
}

test('a candidate that matches collect’s digest and the gate’s records verifies, with notes and acceptance re-derived', async () => {
 const s = await setup();
 try {
  await s.verify();
  await s.verify({}, false);
  assert.ok(candidateFiles(s.fixture.version).includes('acceptance.json'));
 } finally { await s.cleanup(); }
});

test('a candidate whose digest does not match is refused before signing', async () => {
 const s = await setup();
 try {
  await assert.rejects(s.verify({ CANDIDATE_DIGEST: 'f'.repeat(64) }), /digest/i);
  const exe = join(s.candidate, `dilla-${s.fixture.version}-win-x64.exe`), clean = await readFile(exe);
  await writeFile(exe, 'substituted installer');
  await assert.rejects(s.verify(), /digest mismatch/i, 'a file changed after collect digested it');
  await writeFile(exe, clean);
  await writeFile(join(s.candidate, 'extra.sh'), 'planted');
  await assert.rejects(s.verify(), /exact file set/i);
  await rm(join(s.candidate, 'extra.sh'));
  await s.verify();
 } finally { await s.cleanup(); }
});

test('a candidate whose run records are not the gate’s is refused', async () => {
 const s = await setup();
 try {
  await assert.rejects(s.verify({ RUN_RECORD: 'a'.repeat(64) }), /gate/i);
  await assert.rejects(s.verify({ RUN: '11' }), /gate/i);
  await assert.rejects(s.verify({ SHA: 'c'.repeat(40) }), /gate/i);
  // A forged record, consistently re-digested by a compromised collect job.
  await writeFile(join(s.candidate, 'ci-jobs.json'), '[]\n'); await s.redigest();
  await assert.rejects(s.verify(), /gate/i);
 } finally { await s.cleanup(); }
});

test('the sign job re-derives notes and acceptance and refuses collect’s if they differ', async () => {
 const s = await setup();
 try {
  await writeFile(join(s.candidate, 'release-notes.md'), '# Dilla desktop\n\n[Download the fixed build](https://evil.example)\n'); await s.redigest();
  await assert.rejects(s.verify(), /release notes/i);
  await s.verify({}, false); // the publish job checks digests only
  await writeFile(join(s.candidate, 'release-notes.md'), `# Dilla desktop ${s.fixture.version}\n\nFirst desktop release.\n`);
  const acceptance = JSON.parse(await readFile(join(s.candidate, 'acceptance.json'), 'utf8')); acceptance.tree = 'f'.repeat(40);
  await writeFile(join(s.candidate, 'acceptance.json'), JSON.stringify(acceptance, null, 2) + '\n'); await s.redigest();
  await assert.rejects(s.verify(), /acceptance/i);
 } finally { await s.cleanup(); }
});

test('the sign job’s own verification runs on the candidate', async () => {
 const s = await setup();
 try {
  const { signer } = await isolatedReleaseTools(join(s.root, 'tools'), { trust: s.fixture.trust, sandboxConfig: s.fixture.sandbox.config });
  const keys = () => ({ metadata: s.fixture.key.privateKey, sandbox: s.fixture.sandbox.pair.privateKey, sparkleSeed: s.fixture.sparkle.privateKey.export({ type: 'pkcs8', format: 'der' }).subarray(-32) });
  const sign = output => signer.signDesktopRelease({ version: s.fixture.version, artifactsDirectory: s.candidate, keys: keys(), outputDirectory: join(s.root, output), now: new Date(s.fixture.context.now) });
  await sign('signed-clean');
  // collect could hand over a consistently re-digested candidate with a substituted installer: verifyCandidate
  // does not look at installer bytes, the signer's acceptance checks do.
  const exe = join(s.candidate, `dilla-${s.fixture.version}-win-x64.exe`);
  await writeFile(exe, 'substituted installer'); await s.redigest();
  await s.verify();
  await assert.rejects(sign('signed-tampered'), /provenance|artifact/i);
 } finally { await s.cleanup(); }
});
