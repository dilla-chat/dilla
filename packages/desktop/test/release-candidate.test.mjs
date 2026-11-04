import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { buildCandidate, candidateFiles, cli, verifyCandidate, writeCandidateDigest } from '../scripts/release-candidate.mjs';
import { EXPECTED_ARTIFACTS } from '../scripts/ci-run.mjs';
import { BUILD_TARGETS } from '../scripts/verify-release.mjs';
import { buildProvenanceName } from '../scripts/build-provenance.mjs';
import { buildReleaseFixture, isolatedReleaseTools } from './support/release-fixture.mjs';
import { artifactHost } from './support/artifact-host.mjs';

const SHA = '2'.repeat(40), TREE = '3'.repeat(40), MAIN = '4'.repeat(40);
const R = '/repos/dilla-chat/dilla';
const STATUSES = ['queued', 'in_progress', 'waiting', 'requested', 'pending'];

/** The ten CI artifact zips and the gate's record zip behind the artifact API, plus the gate's env for the sign job. */
async function setup({ entries } = {}) {
 const root = await mkdtemp(join(tmpdir(), 'dilla-candidate-'));
 const fixtureDirectory = join(root, 'fixture'); await mkdir(fixtureDirectory);
 const fixture = await buildReleaseFixture({ directory: fixtureDirectory, commit: SHA, head: SHA, tree: TREE });
 const runBytes = Buffer.from(JSON.stringify(fixture.run) + '\n'), jobsBytes = Buffer.from(JSON.stringify(fixture.jobs) + '\n');
 const host = await artifactHost({ fixture, runBytes, jobsBytes, entries });
 const env = { GITHUB_SHA: MAIN, GITHUB_RUN_ID: '5000', GITHUB_RUN_ATTEMPT: '1', GH_TOKEN: 'workflow-token', VERSION: fixture.version, SHA, TREE, RUN: String(fixture.run.id), PREVIOUS: '', ...host.env };
 // The sign job's API: the unfinished desktop release runs (only this one, 5000); notes for a first release need nothing else.
 const active = { in_progress: [{ id: 5000 }] };
 const api = async path => {
  const status = STATUSES.find(name => path === `${R}/actions/workflows/desktop-release.yml/runs?status=${name}&per_page=100`);
  if (status) return { workflow_runs: structuredClone(active[status] ?? []) };
  throw new Error('unexpected API call ' + path);
 };
 // The sign job's full-history checkout of main: its HEAD, and the paths that differ from origin/main.
 const checkout = { head: MAIN, diff: '' };
 const git = args => {
  if (args.length === 2 && args[0] === 'rev-parse' && args[1] === 'HEAD') return checkout.head;
  if (args[0] === 'diff' && args.at(-3) === 'HEAD' && args.at(-2) === 'origin/main') return checkout.diff;
  throw new Error('Unexpected git call: ' + args.join(' '));
 };
 let n = 0;
 const build = (changes = {}) => {
  const k = ++n;
  return buildCandidate({ env: { ...env, ...changes }, api, git, fetchImpl: host.fetchImpl, zipDirectory: join(root, `zips-${k}`), candidateDirectory: join(root, `candidate-${k}`) })
   .then(result => ({ ...result, directory: join(root, `candidate-${k}`) }));
 };
 return { root, fixture, host, env, api, git, active, checkout, build, cleanup: () => rm(root, { recursive: true, force: true }) };
}

/** Final review, I-1: "Re-run failed jobs" on an old keyed run reuses that run's gate outputs and runs its old sign code. */
test('sign refuses a re-run attempt, a checkout that is no longer main’s tip and another active release run, before fetching anything', async () => {
 const s = await setup();
 try {
  const nothingFetched = async () => {
   assert.deepEqual(s.host.log, [], 'no artifact is fetched');
   assert.deepEqual((await readdir(s.root)).filter(name => name !== 'fixture'), [], 'no directory is created');
  };
  for (const GITHUB_RUN_ATTEMPT of ['2', '0', '01', '', undefined]) await assert.rejects(s.build({ GITHUB_RUN_ATTEMPT }), /re-run attempt/i);
  await nothingFetched();
  // sign refuses when its checkout is no longer main's tip (partial re-run).
  s.checkout.diff = 'packages/desktop/scripts/release-sign-job.mjs\0';
  await assert.rejects(s.build(), /no longer main’s tip/);
  s.checkout.diff = '';
  s.checkout.head = 'f'.repeat(40);
  await assert.rejects(s.build(), /HEAD differs from GITHUB_SHA/);
  s.checkout.head = MAIN;
  for (const GITHUB_SHA of ['', 'main']) await assert.rejects(s.build({ GITHUB_SHA }), /GITHUB_SHA/);
  for (const GITHUB_RUN_ID of ['', '12a']) await assert.rejects(s.build({ GITHUB_RUN_ID }), /GITHUB_RUN_ID/);
  s.active.queued = [{ id: 4999 }];
  await assert.rejects(s.build(), /another desktop release run \(4999\)/i);
  await nothingFetched();
  delete s.active.queued;
  // A docs-only main advance after the run started is still main's code.
  s.checkout.diff = 'docs/guide.md\0README.md\0';
  await s.build();
 } finally { await s.cleanup(); }
});

test('sign builds the candidate itself from the gate-digested CI zips and the gate’s records', async () => {
 const s = await setup();
 try {
  const { directory } = await s.build();
  assert.deepEqual((await readdir(directory)).sort(), candidateFiles(s.fixture.version));
  assert.equal(await readFile(join(directory, 'release-notes.md'), 'utf8'), `# Dilla desktop ${s.fixture.version}\n\nFirst desktop release.\n`);
  const acceptance = JSON.parse(await readFile(join(directory, 'acceptance.json'), 'utf8'));
  assert.deepEqual([acceptance.commit, acceptance.tree, acceptance.ciRun], [SHA, TREE, s.fixture.run.id]);
  const api = s.host.log.filter(entry => entry.url.startsWith('https://api.github.com/'));
  assert.deepEqual(api.map(entry => entry.url), [...s.host.ids, 299].map(id => `https://api.github.com/repos/dilla-chat/dilla/actions/artifacts/${id}/zip`));
  assert.ok(api.every(entry => entry.redirect === 'manual'));
  // The candidate signs: the installers are the CI's.
  const { signer } = await isolatedReleaseTools(join(s.root, 'tools'), { trust: s.fixture.trust, sandboxConfig: s.fixture.sandbox.config });
  const keys = { metadata: s.fixture.key.privateKey, sandbox: s.fixture.sandbox.pair.privateKey, sparkleSeed: s.fixture.sparkle.privateKey.export({ type: 'pkcs8', format: 'der' }).subarray(-32) };
  await signer.signDesktopRelease({ version: s.fixture.version, artifactsDirectory: directory, keys, outputDirectory: join(s.root, 'signed'), now: new Date(s.fixture.context.now) });
 } finally { await s.cleanup(); }
});

/** Review batch B, I1 (forged-candidate PoC), as a permanent regression: forged bytes plus consistently rewritten provenance or records. */
function forge(files, name) {
 const out = files.map(file => ({ ...file }));
 const provenance = out.find(file => file.name.startsWith('build-provenance-'));
 if (provenance) {
  const record = JSON.parse(provenance.data);
  const target = out.find(file => file.name === record.artifacts[0].name);
  target.data = Buffer.from('MZ evil installer planted after the gate fixed the digests: ' + name);
  Object.assign(record.artifacts[0], { size: target.data.length, sha512: createHash('sha512').update(target.data).digest('base64') });
  provenance.data = Buffer.from(JSON.stringify(record));
 } else {
  const report = JSON.parse(out[0].data); report.status = 'forged'; out[0].data = Buffer.from(JSON.stringify(report));
 }
 return out;
}

test('a forged installer with rewritten provenance (or a forged record) in any of the ten artifacts is refused', async () => {
 const s = await setup();
 try {
  const forged = await setup({ entries: Object.fromEntries(EXPECTED_ARTIFACTS.map(name => [name, files => forge(files, name)])) });
  try {
   for (const name of EXPECTED_ARTIFACTS) {
    const id = s.host.idOf(name), genuine = s.host.blobs.get(id);
    s.host.replace(id, forged.host.blobs.get(forged.host.idOf(name)));
    await assert.rejects(s.build(), /does not match the digest the gate recorded/, name);
    s.host.replace(id, genuine);
   }
   // The gate's records are bound the same way.
   s.host.replace(299, forged.host.blobs.get(s.host.idOf(EXPECTED_ARTIFACTS[0])));
   await assert.rejects(s.build(), /digest the gate recorded/);
  } finally { await forged.cleanup(); }
 } finally { await s.cleanup(); }
});

/** Release desktop-v0.1.0-beta.1 (run 41787307813): upload-artifact's `artifact-digest` output is the bare 64-hex SHA-256, not the API's `sha256:` form. */
test('RECORDS_DIGEST takes upload-artifact’s bare hex or the sha256: form, and nothing looser', async () => {
 const s = await setup();
 try {
  const prefixed = s.env.RECORDS_DIGEST, hex = prefixed.slice('sha256:'.length);
  assert.match(prefixed, /^sha256:[a-f0-9]{64}$/);
  const zipUrl = 'https://api.github.com/repos/dilla-chat/dilla/actions/artifacts/299/zip';
  // The bare hex is accepted and normalised: the records zip is still checked against its digest.
  await s.build({ RECORDS_DIGEST: hex });
  await s.build({ RECORDS_DIGEST: prefixed });
  const other = 'f'.repeat(64);
  await assert.rejects(s.build({ RECORDS_DIGEST: other }), /digest the gate recorded/, 'a bare hex is still compared, not trusted');
  for (const RECORDS_DIGEST of [hex.toUpperCase(), 'sha256:' + hex.toUpperCase(), 'SHA256:' + hex, hex.slice(1), hex + '0', 'sha256:' + hex.slice(1), 'sha512:' + hex, 'sha512:' + hex + hex, 'sha256:', '', ' ' + hex, hex + '\n', 'sha256:sha256:' + hex, undefined]) {
   const before = s.host.log.filter(entry => entry.url === zipUrl).length;
   await assert.rejects(s.build({ RECORDS_DIGEST }), /Invalid RECORDS_DIGEST/, JSON.stringify(RECORDS_DIGEST));
   assert.equal(s.host.log.filter(entry => entry.url === zipUrl).length, before, 'a refused digest fetches nothing');
  }
 } finally { await s.cleanup(); }
});

test('a CI artifact zip that the gate digested but that slips, duplicates or adds entries is refused', async () => {
 const cases = {
  'zip slip': files => [...files.slice(1), { ...files[0], name: '../' + files[0].name }],
  duplicate: files => [...files, { ...files[0], data: 'second copy' }],
  extra: files => [...files, { name: 'release-notes.md', data: '[Download the fixed build](https://evil.example)' }],
  symlink: files => [{ ...files[0], mode: 0o120777, data: '/etc/passwd' }, ...files.slice(1)],
 };
 for (const [label, change] of Object.entries(cases)) {
  const s = await setup({ entries: { [EXPECTED_ARTIFACTS[1]]: change } });
  try {
   await assert.rejects(s.build(), /artifact zip/i, label);
   const written = (await readdir(s.root)).filter(name => !/^(fixture|zips-\d+|candidate-\d+)$/.test(name));
   assert.deepEqual(written, [], label + ': nothing outside the job’s own directories');
  } finally { await s.cleanup(); }
 }
});

test('the gate’s ids, digests and record hashes are validated, and the directories must be new', async () => {
 const s = await setup();
 try {
  await assert.rejects(s.build({ ARTIFACT_IDS: s.host.ids.slice(1).join(',') }), /ARTIFACT_IDS/);
  await assert.rejects(s.build({ ARTIFACT_DIGESTS: s.host.digests.slice(0, 9).join(',') }), /ARTIFACT_DIGESTS/);
  await assert.rejects(s.build({ RECORDS_ID: '0' }), /RECORDS_ID/);
  await assert.rejects(s.build({ RUN_RECORD: 'a'.repeat(64) }), /gate/i);
  await assert.rejects(s.build({ RUN: '11' }), /gate/i);
  await assert.rejects(s.build({ SHA: 'c'.repeat(40) }), /gate/i);
  const zips = join(s.root, 'existing'); await mkdir(zips);
  await assert.rejects(buildCandidate({ env: s.env, api: s.api, git: s.git, fetchImpl: s.host.fetchImpl, zipDirectory: zips, candidateDirectory: join(s.root, 'fresh') }), /EEXIST/);
 } finally { await s.cleanup(); }
});

test('publish verifies the candidate against the sign job’s digest', async () => {
 const s = await setup();
 try {
  const { directory } = await s.build();
  const digest = await writeCandidateDigest(directory, s.fixture.version);
  const env = { ...s.env, CANDIDATE_DIGEST: digest };
  await verifyCandidate({ directory, env });
  await assert.rejects(verifyCandidate({ directory, env: { ...env, CANDIDATE_DIGEST: 'f'.repeat(64) } }), /sign job/);
  const exe = join(directory, `dilla-${s.fixture.version}-win-x64.exe`), clean = await readFile(exe);
  await writeFile(exe, 'substituted installer');
  await assert.rejects(verifyCandidate({ directory, env }), /digest mismatch/i);
  await writeFile(exe, clean);
  await writeFile(join(directory, 'extra.sh'), 'planted');
  await assert.rejects(verifyCandidate({ directory, env }), /exact file set/i);
  await rm(join(directory, 'extra.sh'));
  await assert.rejects(verifyCandidate({ directory, env: { ...env, JOBS_RECORD: 'b'.repeat(64) } }), /gate/i);
  for (const target of BUILD_TARGETS) assert.ok(candidateFiles(s.fixture.version).includes(buildProvenanceName(target)));
 } finally { await s.cleanup(); }
});

test('record-id writes only validated artifact ids from the sign job’s uploads', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-candidate-ids-'));
 try {
  const output = join(root, 'out'); await writeFile(output, '');
  await cli(['record-id'], { GITHUB_OUTPUT: output, SIGNED_ID: '12', CANDIDATE_ID: '13' });
  assert.equal(await readFile(output, 'utf8'), 'signed_id=12\ncandidate_id=13\n');
  for (const env of [{ SIGNED_ID: '12\nx=1', CANDIDATE_ID: '13' }, { SIGNED_ID: '12', CANDIDATE_ID: '' }, { SIGNED_ID: '0', CANDIDATE_ID: '13' }]) {
   await assert.rejects(cli(['record-id'], { GITHUB_OUTPUT: output, ...env }), /artifact id/i);
  }
  assert.equal(await readFile(output, 'utf8'), 'signed_id=12\ncandidate_id=13\n');
  await assert.rejects(cli(['verify-and-rederive', root], {}), /Usage/, 'the collect hand-over is gone');
 } finally { await rm(root, { recursive: true, force: true }); }
});
