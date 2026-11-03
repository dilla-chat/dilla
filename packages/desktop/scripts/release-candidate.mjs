import { createHash } from 'node:crypto';
import { appendFileSync, createReadStream } from 'node:fs';
import { lstat, mkdir, readdir, readFile, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { assembleAcceptance, expectedArtifactFiles } from './release-assemble.mjs';
import { EXPECTED_ARTIFACTS, createGithubApi } from './ci-run.mjs';
import { DESKTOP_TAG, generateNotes } from './release-notes.mjs';
import { downloadArtifactZip, extractArtifactZip } from './artifact-zip.mjs';

/**
 * The signing candidate is built by the keyed `sign` job itself (review batch
 * B, I1/I2). It fetches the accepted ci run's ten artifact zips and the gate's
 * record zip as opaque blobs, by the ids the gate output. Each blob's sha256
 * must be the digest the gate recorded, and each zip is read in-process
 * (artifact-zip.mjs). Then `sign` writes the release notes and acceptance.json
 * and checks the gate's records. Nothing the keyless `collect` job produces
 * reaches `sign` or `publish`. After signing, `sign` digests the candidate
 * (SHA256SUMS over the exact file set) and uploads it. `publish` takes it by
 * the artifact id `sign` reports and re-checks it against the digest `sign`
 * reports.
 */
const SUMS = 'SHA256SUMS';
const MAX_FILE = 4 * 1024 ** 3;
const VERSION = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/;
const COMMIT = /^[a-f0-9]{40}$/, HEX = /^[a-f0-9]{64}$/, ID = /^[1-9]\d*$/, DIGEST = /^sha256:[a-f0-9]{64}$/;
const RECORDS = Object.freeze(['ci-jobs.json', 'ci-run.json']);
const sha256 = bytes => createHash('sha256').update(bytes).digest('hex');
const safeId = value => ID.test(value ?? '') && Number.isSafeInteger(Number(value));

export function candidateFiles(version) {
 if (typeof version !== 'string' || !VERSION.test(version)) throw new Error('Invalid desktop version');
 return [...[...expectedArtifactFiles(version).values()].flat(), 'ci-run.json', 'ci-jobs.json', 'release-notes.md', 'acceptance.json'].sort();
}

async function fileDigest(path) {
 const hash = createHash('sha256');
 for await (const chunk of createReadStream(path)) hash.update(chunk);
 return hash.digest('hex');
}
/** Exactly `expected` (sorted) as regular files; returns name -> sha256. */
async function exactFiles(directory, expected) {
 const present = (await readdir(directory)).sort();
 const missing = expected.filter(name => !present.includes(name)), unexpected = present.filter(name => !expected.includes(name));
 if (missing.length || unexpected.length) throw new Error(`The candidate must hold the exact file set (missing: ${missing.join(', ') || 'none'}; unexpected: ${unexpected.join(', ') || 'none'})`);
 const digests = new Map();
 for (const name of expected) {
  const info = await lstat(join(directory, name));
  if (info.isSymbolicLink() || !info.isFile() || info.nlink !== 1) throw new Error('Candidate files must be regular, unlinked files: ' + name);
  if (info.size > MAX_FILE) throw new Error('Candidate file is too large: ' + name);
  digests.set(name, await fileDigest(join(directory, name)));
 }
 return digests;
}
const sums = digests => Buffer.from([...digests.keys()].sort().map(name => `${digests.get(name)}  ${name}\n`).join(''));

export async function writeCandidateDigest(directory, version) {
 const content = sums(await exactFiles(directory, candidateFiles(version)));
 await writeFile(join(directory, SUMS), content, { flag: 'wx' });
 return sha256(content);
}

function context(env, { digest }) {
 const values = { version: env.VERSION, sha: env.SHA, tree: env.TREE, run: env.RUN, runRecord: env.RUN_RECORD, jobsRecord: env.JOBS_RECORD, previous: env.PREVIOUS ?? '' };
 if (!VERSION.test(values.version ?? '')) throw new Error('Invalid VERSION');
 if (!COMMIT.test(values.sha ?? '') || !COMMIT.test(values.tree ?? '')) throw new Error('Invalid SHA or TREE');
 if (!safeId(values.run)) throw new Error('Invalid RUN');
 if (!HEX.test(values.runRecord ?? '') || !HEX.test(values.jobsRecord ?? '')) throw new Error('Invalid gate record digests');
 if (values.previous !== '' && !DESKTOP_TAG.test(values.previous)) throw new Error('Invalid PREVIOUS');
 if (digest) {
  if (!HEX.test(env.CANDIDATE_DIGEST ?? '')) throw new Error('Invalid CANDIDATE_DIGEST');
  values.digest = env.CANDIDATE_DIGEST;
 }
 return values;
}

/** The gate's records must be the gate's bytes and name the gate's run and commit. */
async function checkRecords(directory, digests, c) {
 if (digests.get('ci-run.json') !== c.runRecord || digests.get('ci-jobs.json') !== c.jobsRecord) throw new Error('The candidate’s ci run records are not the gate’s');
 const run = JSON.parse(await readFile(join(directory, 'ci-run.json'), 'utf8')), jobs = JSON.parse(await readFile(join(directory, 'ci-jobs.json'), 'utf8'));
 if (run?.id !== Number(c.run) || run.head_sha !== c.sha) throw new Error('The candidate’s ci run is not the gate’s');
 return { run, jobs };
}

/** The gate's ten artifact ids (in EXPECTED_ARTIFACTS order) and their recorded digests, plus its record artifact. */
function artifactContext(env) {
 const ids = (env.ARTIFACT_IDS ?? '').split(','), digests = (env.ARTIFACT_DIGESTS ?? '').split(',');
 if (ids.length !== EXPECTED_ARTIFACTS.length || !ids.every(safeId) || new Set(ids).size !== ids.length) throw new Error('Invalid ARTIFACT_IDS: the gate records exactly ten distinct ids');
 if (digests.length !== ids.length || !digests.every(value => DIGEST.test(value))) throw new Error('Invalid ARTIFACT_DIGESTS: one sha256 digest per artifact id');
 if (!safeId(env.RECORDS_ID) || ids.includes(env.RECORDS_ID)) throw new Error('Invalid RECORDS_ID');
 if (!DIGEST.test(env.RECORDS_DIGEST ?? '')) throw new Error('Invalid RECORDS_DIGEST');
 return { ids, digests, recordsId: env.RECORDS_ID, recordsDigest: env.RECORDS_DIGEST };
}

/**
 * The sign job's candidate, built from bytes the gate fixed: each zip is
 * fetched by id, kept only under the gate's digest and read in-process. Then
 * the notes and acceptance.json are derived here. Both directories must be new.
 */
export async function buildCandidate({ env, api, fetchImpl = fetch, zipDirectory, candidateDirectory }) {
 const c = context(env, { digest: false });
 const a = artifactContext(env);
 await mkdir(zipDirectory, { mode: 0o700 });
 await mkdir(candidateDirectory, { mode: 0o700 });
 const expected = expectedArtifactFiles(c.version);
 const fetchZip = async (id, digest, label) => {
  const file = join(zipDirectory, `${label}.zip`);
  await downloadArtifactZip({ token: env.GH_TOKEN, id, digest, file, fetchImpl });
  return file;
 };
 for (const [i, name] of EXPECTED_ARTIFACTS.entries()) {
  await extractArtifactZip({ file: await fetchZip(a.ids[i], a.digests[i], name), names: expected.get(name), directory: candidateDirectory });
 }
 await extractArtifactZip({ file: await fetchZip(a.recordsId, a.recordsDigest, 'desktop-release-gate'), names: [...RECORDS], directory: candidateDirectory });
 const files = [...[...expected.values()].flat(), ...RECORDS].sort();
 const { run, jobs } = await checkRecords(candidateDirectory, await exactFiles(candidateDirectory, files), c);

 const notes = await generateNotes({ api, version: c.version, sha: c.sha, previous: c.previous || null });
 await writeFile(join(candidateDirectory, 'release-notes.md'), notes, { flag: 'wx' });
 const { acceptance } = await assembleAcceptance({ candidateDirectory, run, jobs, version: c.version, commitTree: c.tree });
 if (acceptance.commit !== c.sha || acceptance.head !== c.sha || acceptance.tree !== c.tree || acceptance.ciRun !== Number(c.run)) throw new Error('The derived acceptance does not name the gate’s commit, tree and run');
 await writeFile(join(candidateDirectory, 'acceptance.json'), JSON.stringify(acceptance, null, 2) + '\n', { flag: 'wx' });
 await exactFiles(candidateDirectory, candidateFiles(c.version));
 return { run, jobs };
}

/** The publish job: the candidate sign uploaded must match sign's digest and the gate's records. */
export async function verifyCandidate({ directory, env }) {
 const c = context(env, { digest: true });
 const digests = await exactFiles(directory, [...candidateFiles(c.version), SUMS].sort());
 const recorded = await readFile(join(directory, SUMS));
 if (sha256(recorded) !== c.digest) throw new Error('The candidate’s SHA256SUMS does not match the digest the sign job reported');
 digests.delete(SUMS);
 if (!sums(digests).equals(recorded)) throw new Error('Candidate digest mismatch');
 return checkRecords(directory, digests, c);
}

export async function cli(argv, env, { fetchImpl = fetch } = {}) {
 const [command, ...args] = argv;
 if (command === 'build' && args.length === 2) {
  const [zipDirectory, candidateDirectory] = args.map(path => resolve(path));
  await buildCandidate({ env, api: createGithubApi({ token: env.GH_TOKEN, fetchImpl }), fetchImpl, zipDirectory, candidateDirectory });
  return 'Built the signing candidate from the gate-digested CI artifact zips.';
 }
 if (command === 'digest' && args.length === 2) {
  const hex = await writeCandidateDigest(resolve(args[0]), args[1]);
  if (!HEX.test(hex) || !env.GITHUB_OUTPUT) throw new Error('Cannot write the candidate digest output');
  appendFileSync(env.GITHUB_OUTPUT, `candidate_digest=${hex}\n`);
  return 'Digested the signing candidate.';
 }
 if (command === 'record-id' && args.length === 0) {
  // The upload actions' artifact ids become job outputs only after validation.
  if (!safeId(env.SIGNED_ID) || !safeId(env.CANDIDATE_ID) || !env.GITHUB_OUTPUT) throw new Error('Invalid signed or candidate artifact id');
  appendFileSync(env.GITHUB_OUTPUT, `signed_id=${env.SIGNED_ID}\ncandidate_id=${env.CANDIDATE_ID}\n`);
  return 'Recorded the signed and candidate artifact ids.';
 }
 if (command === 'verify' && args.length === 1) {
  await verifyCandidate({ directory: resolve(args[0]), env });
  return 'The candidate matches the sign job’s digest and the gate’s records.';
 }
 throw new Error('Usage: release-candidate.mjs build ZIP_DIR CANDIDATE_DIR | digest DIR VERSION | record-id | verify DIR');
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) console.log(await cli(process.argv.slice(2), process.env));
