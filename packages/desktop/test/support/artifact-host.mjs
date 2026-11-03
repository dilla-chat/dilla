import { createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { EXPECTED_ARTIFACTS } from '../../scripts/ci-run.mjs';
import { expectedArtifactFiles } from '../../scripts/release-assemble.mjs';
import { zip } from './zip-writer.mjs';

const digestOf = bytes => 'sha256:' + createHash('sha256').update(bytes).digest('hex');
export const RECORDS_ID = 299;

/**
 * The accepted ci run's ten artifacts and the gate's record artifact, as the
 * zips GitHub stores, behind the REST redirect and the blob host. `digests`
 * are what the gate records (computed over the stored zips); `replace` swaps
 * the stored bytes of one id afterwards, the way an attacker would have to.
 */
export async function artifactHost({ fixture, runBytes, jobsBytes, entries = {} }) {
 const blobs = new Map(), ids = [], digests = [];
 const expected = expectedArtifactFiles(fixture.version);
 for (const [i, name] of EXPECTED_ARTIFACTS.entries()) {
  const files = [];
  for (const file of expected.get(name)) files.push({ name: file, data: await readFile(join(fixture.directory, file)) });
  const bytes = zip(entries[name]?.(files) ?? files);
  ids.push(300 + i); digests.push(digestOf(bytes)); blobs.set(300 + i, bytes);
 }
 const records = zip([{ name: 'ci-run.json', data: runBytes }, { name: 'ci-jobs.json', data: jobsBytes }]);
 blobs.set(RECORDS_ID, records);
 const log = [];
 async function fetchImpl(input, init = {}) {
  const url = new URL(String(input)); log.push({ url: url.href, redirect: init.redirect, authorization: init.headers?.Authorization });
  const api = /^\/repos\/dilla-chat\/dilla\/actions\/artifacts\/(\d+)\/zip$/.exec(url.pathname);
  if (url.origin === 'https://api.github.com' && api && blobs.has(Number(api[1]))) {
   return new Response(null, { status: 302, headers: { Location: `https://productionresultssa3.blob.core.windows.net/actions-results/${api[1]}.zip?sig=fixture` } });
  }
  const blob = /^\/actions-results\/(\d+)\.zip$/.exec(url.pathname);
  if (url.origin === 'https://productionresultssa3.blob.core.windows.net' && blob && blobs.has(Number(blob[1]))) return new Response(blobs.get(Number(blob[1])), { status: 200 });
  return new Response('Not Found', { status: 404 });
 }
 const env = {
  ARTIFACT_IDS: ids.join(','), ARTIFACT_DIGESTS: digests.join(','), RECORDS_ID: String(RECORDS_ID), RECORDS_DIGEST: digestOf(records),
  RUN_RECORD: createHash('sha256').update(runBytes).digest('hex'), JOBS_RECORD: createHash('sha256').update(jobsBytes).digest('hex'),
 };
 const idOf = name => 300 + EXPECTED_ARTIFACTS.indexOf(name);
 return { blobs, ids, digests, env, fetchImpl, log, idOf, replace: (id, bytes) => blobs.set(id, bytes), zip };
}
