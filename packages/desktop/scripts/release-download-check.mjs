import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { lstat, readdir, readFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { createGithubApi, listRunArtifacts, selectReleaseArtifacts } from './ci-run.mjs';

/**
 * Runs in `sign` right after the CI artifacts are downloaded and before
 * anything reads them. download-artifact already fails on a zip whose digest
 * differs from the server's (digest-mismatch: error, pinned by the checker);
 * this step proves the server digests of those artifact ids are still the ones
 * the gate accepted, and that the extraction wrote nothing outside its root:
 * the checked-out workspace is untouched (ignored paths included), the gate's
 * records are byte-for-byte what the gate wrote, and the candidate and signed
 * directories do not exist yet.
 */
const IDS = /^[1-9]\d*(,[1-9]\d*)*$/, DIGESTS = /^sha256:[a-f0-9]{64}(,sha256:[a-f0-9]{64})*$/, HEX = /^[a-f0-9]{64}$/;
const GATE_FILES = Object.freeze(['ci-jobs.json', 'ci-run.json']);

async function absent(path) {
 try { await lstat(path); } catch (error) { if (error?.code === 'ENOENT') return; throw error; }
 throw new Error('The output directory already exists before collection: ' + path);
}

export async function checkDownloads({ env, git, api, gateDirectory, candidateDirectory, signedDirectory }) {
 if (!/^[1-9]\d*$/.test(env.RUN ?? '')) throw new Error('Invalid RUN');
 if (!IDS.test(env.ARTIFACT_IDS ?? '')) throw new Error('Invalid ARTIFACT_IDS');
 if (!DIGESTS.test(env.ARTIFACT_DIGESTS ?? '') || env.ARTIFACT_DIGESTS.split(',').length !== env.ARTIFACT_IDS.split(',').length) throw new Error('Invalid ARTIFACT_DIGESTS');
 if (!HEX.test(env.RUN_RECORD ?? '') || !HEX.test(env.JOBS_RECORD ?? '')) throw new Error('Invalid gate record digests');

 if (git(['status', '--porcelain', '--ignored', '--untracked-files=all']) !== '') throw new Error('The workspace changed during the artifact download');

 const present = (await readdir(gateDirectory)).sort();
 if (present.length !== GATE_FILES.length || present.some((name, i) => name !== GATE_FILES[i])) throw new Error('The gate record directory holds other files');
 for (const [name, expected] of [['ci-run.json', env.RUN_RECORD], ['ci-jobs.json', env.JOBS_RECORD]]) {
  const info = await lstat(join(gateDirectory, name));
  if (info.isSymbolicLink() || !info.isFile()) throw new Error('A gate record is not a regular file: ' + name);
  if (createHash('sha256').update(await readFile(join(gateDirectory, name))).digest('hex') !== expected) throw new Error('A gate record changed after the gate: ' + name);
 }
 await absent(candidateDirectory);
 await absent(signedDirectory);

 const { ids, byName } = selectReleaseArtifacts(await listRunArtifacts(api, env.RUN));
 const digests = Object.values(byName).map(artifact => artifact.digest);
 if (ids.join(',') !== env.ARTIFACT_IDS || digests.join(',') !== env.ARTIFACT_DIGESTS) throw new Error('The run’s artifact ids or digests differ from the ones the gate accepted');
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
 if (process.argv.length !== 5) throw new Error('Usage: release-download-check.mjs GATE_DIR CANDIDATE_DIR SIGNED_DIR');
 const root = resolve(import.meta.dirname, '../../..');
 const git = args => execFileSync('git', args, { cwd: root, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();
 const [gateDirectory, candidateDirectory, signedDirectory] = process.argv.slice(2).map(path => resolve(path));
 await checkDownloads({ env: process.env, git, api: createGithubApi({ token: process.env.GH_TOKEN }), gateDirectory, candidateDirectory, signedDirectory });
 console.log('The downloaded artifacts match the gate; the workspace and gate records are untouched.');
}
