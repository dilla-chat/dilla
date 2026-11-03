import { createHash } from 'node:crypto';
import { lstat, readdir, readFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { createGithubApi, listRunArtifacts, selectReleaseArtifacts } from './ci-run.mjs';

/**
 * Runs in the keyless `collect` job right after the downloads, before anything
 * reads them. download-artifact already fails on a zip whose digest differs
 * from the server's (digest-mismatch: error, pinned by the checker); this step
 * proves the server digests of those artifact ids are still the ones the gate
 * accepted (review m4), that the gate's records arrived byte-for-byte, and
 * that the candidate directory does not exist yet. The key boundary is the job
 * split: `sign` never handles raw CI artifacts, only collect's digest-checked
 * candidate, which it verifies itself.
 */
const IDS = /^[1-9]\d*(,[1-9]\d*)*$/, DIGESTS = /^sha256:[a-f0-9]{64}(,sha256:[a-f0-9]{64})*$/, HEX = /^[a-f0-9]{64}$/;
const GATE_FILES = Object.freeze(['ci-jobs.json', 'ci-run.json']);

export async function checkDownloads({ env, api, gateDirectory, candidateDirectory }) {
 if (!/^[1-9]\d*$/.test(env.RUN ?? '')) throw new Error('Invalid RUN');
 if (!IDS.test(env.ARTIFACT_IDS ?? '')) throw new Error('Invalid ARTIFACT_IDS');
 if (!DIGESTS.test(env.ARTIFACT_DIGESTS ?? '') || env.ARTIFACT_DIGESTS.split(',').length !== env.ARTIFACT_IDS.split(',').length) throw new Error('Invalid ARTIFACT_DIGESTS');
 if (!HEX.test(env.RUN_RECORD ?? '') || !HEX.test(env.JOBS_RECORD ?? '')) throw new Error('Invalid gate record digests');

 const present = (await readdir(gateDirectory)).sort();
 if (present.length !== GATE_FILES.length || present.some((name, i) => name !== GATE_FILES[i])) throw new Error('The gate record directory holds other files');
 for (const [name, expected] of [['ci-run.json', env.RUN_RECORD], ['ci-jobs.json', env.JOBS_RECORD]]) {
  const info = await lstat(join(gateDirectory, name));
  if (info.isSymbolicLink() || !info.isFile()) throw new Error('A gate record is not a regular file: ' + name);
  if (createHash('sha256').update(await readFile(join(gateDirectory, name))).digest('hex') !== expected) throw new Error('A gate record differs from the gate’s: ' + name);
 }
 try { await lstat(candidateDirectory); throw new Error('The candidate directory already exists before collection'); }
 catch (error) { if (error?.code !== 'ENOENT') throw error; }

 const { ids, byName } = selectReleaseArtifacts(await listRunArtifacts(api, env.RUN));
 const digests = Object.values(byName).map(artifact => artifact.digest);
 if (ids.join(',') !== env.ARTIFACT_IDS || digests.join(',') !== env.ARTIFACT_DIGESTS) throw new Error('The run’s artifact ids or digests differ from the ones the gate accepted');
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
 if (process.argv.length !== 4) throw new Error('Usage: release-download-check.mjs GATE_DIR CANDIDATE_DIR');
 const [gateDirectory, candidateDirectory] = process.argv.slice(2).map(path => resolve(path));
 await checkDownloads({ env: process.env, api: createGithubApi({ token: process.env.GH_TOKEN }), gateDirectory, candidateDirectory });
 console.log('The downloaded artifacts and gate records match the gate.');
}
