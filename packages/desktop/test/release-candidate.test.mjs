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

const SHA = '2'.repeat(40), TREE = '3'.repeat(40);

/** The ten CI artifact zips and the gate's record zip behind the artifact API, plus the gate's env for the sign job. */
async function setup({ entries } = {}) {
 const root = await mkdtemp(join(tmpdir(), 'dilla-candidate-'));
 const fixtureDirectory = join(root, 'fixture'); await mkdir(fixtureDirectory);
 const fixture = await buildReleaseFixture({ directory: fixtureDirectory, commit: SHA, head: SHA, tree: TREE });
 const runBytes = Buffer.from(JSON.stringify(fixture.run) + '\n'), jobsBytes = Buffer.from(JSON.stringify(fixture.jobs) + '\n');
 const host = await artifactHost({ fixture, runBytes, jobsBytes, entries });
 const env = { GH_TOKEN: 'workflow-token', VERSION: fixture.version, SHA, TREE, RUN: String(fixture.run.id), PREVIOUS: '', ...host.env };
 // The sign job's API is used only for notes; a first release needs none.
 const api = async path => { throw new Error('unexpected API call ' + path); };
 let n = 0;
 const build = (changes = {}) => {
  const k = ++n;
  return buildCandidate({ env: { ...env, ...changes }, api, fetchImpl: host.fetchImpl, zipDirectory: join(root, `zips-${k}`), candidateDirectory: join(root, `candidate-${k}`) })
   .then(result => ({ ...result, directory: join(root, `candidate-${k}`) }));
 };
 return { root, fixture, host, env, api, build, cleanup: () => rm(root, { recursive: true, force: true }) };
}

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
  await assert.rejects(buildCandidate({ env: s.env, api: s.api, fetchImpl: s.host.fetchImpl, zipDirectory: zips, candidateDirectory: join(s.root, 'fresh') }), /EEXIST/);
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
