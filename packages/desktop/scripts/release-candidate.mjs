import { createHash } from 'node:crypto';
import { appendFileSync, createReadStream } from 'node:fs';
import { lstat, readdir, readFile, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { assembleAcceptance, expectedArtifactFiles } from './release-assemble.mjs';
import { createGithubApi } from './ci-run.mjs';
import { DESKTOP_TAG, generateNotes } from './release-notes.mjs';

/**
 * The signing candidate crosses from the keyless `collect` job (which handles
 * the raw, untrusted CI artifacts) to `sign` and `publish` as one artifact.
 * `collect` digests it (SHA256SUMS over the exact file set) and reports the
 * digest as a validated job output; every consumer re-checks that digest, the
 * gate's record hashes and, in `sign`, re-derives the release notes and
 * acceptance.json itself rather than trusting collect's.
 */
const SUMS = 'SHA256SUMS';
const MAX_FILE = 4 * 1024 ** 3;
const VERSION = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/;
const COMMIT = /^[a-f0-9]{40}$/, HEX = /^[a-f0-9]{64}$/, ID = /^[1-9]\d*$/;
const sha256 = bytes => createHash('sha256').update(bytes).digest('hex');

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

function context(env) {
 const values = { version: env.VERSION, sha: env.SHA, tree: env.TREE, run: env.RUN, runRecord: env.RUN_RECORD, jobsRecord: env.JOBS_RECORD, digest: env.CANDIDATE_DIGEST, previous: env.PREVIOUS ?? '' };
 if (!VERSION.test(values.version ?? '')) throw new Error('Invalid VERSION');
 if (!COMMIT.test(values.sha ?? '') || !COMMIT.test(values.tree ?? '')) throw new Error('Invalid SHA or TREE');
 if (!ID.test(values.run ?? '') || !Number.isSafeInteger(Number(values.run))) throw new Error('Invalid RUN');
 if (!HEX.test(values.runRecord ?? '') || !HEX.test(values.jobsRecord ?? '')) throw new Error('Invalid gate record digests');
 if (!HEX.test(values.digest ?? '')) throw new Error('Invalid CANDIDATE_DIGEST');
 if (values.previous !== '' && !DESKTOP_TAG.test(values.previous)) throw new Error('Invalid PREVIOUS');
 return values;
}

/**
 * Checks a downloaded candidate against collect's digest and the gate's records.
 * With `api` (the sign job), the notes and acceptance.json are re-derived from
 * the gate's values and must equal the candidate's byte for byte.
 */
export async function verifyCandidate({ directory, env, api }) {
 const c = context(env);
 const digests = await exactFiles(directory, [...candidateFiles(c.version), SUMS].sort());
 const recorded = await readFile(join(directory, SUMS));
 if (sha256(recorded) !== c.digest) throw new Error('The candidate’s SHA256SUMS does not match the digest the collect job reported');
 digests.delete(SUMS);
 if (!sums(digests).equals(recorded)) throw new Error('Candidate digest mismatch');
 if (digests.get('ci-run.json') !== c.runRecord || digests.get('ci-jobs.json') !== c.jobsRecord) throw new Error('The candidate’s ci run records are not the gate’s');
 const run = JSON.parse(await readFile(join(directory, 'ci-run.json'), 'utf8')), jobs = JSON.parse(await readFile(join(directory, 'ci-jobs.json'), 'utf8'));
 if (run?.id !== Number(c.run) || run.head_sha !== c.sha) throw new Error('The candidate’s ci run is not the gate’s');
 if (!api) return { run, jobs };
 const notes = await generateNotes({ api, version: c.version, sha: c.sha, previous: c.previous || null });
 if (!Buffer.from(notes).equals(await readFile(join(directory, 'release-notes.md')))) throw new Error('The candidate’s release notes differ from the ones the sign job derives');
 const { acceptance } = await assembleAcceptance({ candidateDirectory: directory, run, jobs, version: c.version, commitTree: c.tree });
 if (acceptance.commit !== c.sha || acceptance.head !== c.sha || acceptance.tree !== c.tree || acceptance.ciRun !== Number(c.run)) throw new Error('The derived acceptance does not name the gate’s commit, tree and run');
 if (!Buffer.from(JSON.stringify(acceptance, null, 2) + '\n').equals(await readFile(join(directory, 'acceptance.json')))) throw new Error('The candidate’s acceptance.json differs from the one the sign job derives');
 return { run, jobs };
}

export async function cli(argv, env, { fetchImpl = fetch } = {}) {
 const [command, ...args] = argv;
 if (command === 'digest' && args.length === 2) {
  const hex = await writeCandidateDigest(resolve(args[0]), args[1]);
  if (!HEX.test(hex) || !env.GITHUB_OUTPUT) throw new Error('Cannot write the candidate digest output');
  appendFileSync(env.GITHUB_OUTPUT, `candidate_digest=${hex}\n`);
  return 'Digested the signing candidate.';
 }
 if (command === 'record-id' && args.length === 0) {
  // The upload action's artifact id becomes a job output only after validation.
  if (!ID.test(env.CANDIDATE_ID ?? '') || !Number.isSafeInteger(Number(env.CANDIDATE_ID)) || !env.GITHUB_OUTPUT) throw new Error('Invalid candidate artifact id');
  appendFileSync(env.GITHUB_OUTPUT, `candidate_id=${env.CANDIDATE_ID}\n`);
  return 'Recorded the candidate artifact id.';
 }
 if (command === 'verify' && args.length === 1) {
  await verifyCandidate({ directory: resolve(args[0]), env });
  return 'The candidate matches the collect job’s digest and the gate’s records.';
 }
 if (command === 'verify-and-rederive' && args.length === 1) {
  await verifyCandidate({ directory: resolve(args[0]), env, api: createGithubApi({ token: env.GH_TOKEN, fetchImpl }) });
  return 'The candidate matches the collect job’s digest and the gate’s records; notes and acceptance re-derived.';
 }
 throw new Error('Usage: release-candidate.mjs digest DIR VERSION | record-id | verify DIR | verify-and-rederive DIR');
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) console.log(await cli(process.argv.slice(2), process.env));
