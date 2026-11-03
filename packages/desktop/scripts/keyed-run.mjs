import { classifyFile } from '../../../scripts/ci-areas.mjs';
import { REPOSITORY } from './ci-run.mjs';

/**
 * Checks every job of desktop-release.yml that holds the keys or writes a
 * release repeats: gate, sign (release-candidate build, before any secret is in
 * scope) and publish. A partial re-run ("Re-run failed jobs", `gh run rerun
 * --failed` or `--job`) runs those jobs again at the run's original GITHUB_SHA
 * with the old gate's outputs, without re-running the gate (final review, I-1).
 * So they refuse any attempt but the first, and each repeats the gate's
 * main-equivalence and one-run-at-a-time checks itself. Only the keyless `feed`
 * job may be re-run; every other recovery is a new request run or a new tag.
 */
const COMMIT = /^[a-f0-9]{40}$/;
const RUN_ID = /^[1-9]\d*$/;
export const ACTIVE_STATUSES = Object.freeze(['queued', 'in_progress', 'waiting', 'requested', 'pending']);

/** Only the first attempt of a keyed run may run this job. */
export function refuseRerunAttempt(env, job) {
 if (env.GITHUB_RUN_ATTEMPT !== '1') {
  throw new Error(`The ${job} job refuses a re-run attempt (GITHUB_RUN_ATTEMPT=${JSON.stringify(env.GITHUB_RUN_ATTEMPT ?? null)}): only feed may be re-run; re-run the release request run (gh run rerun <request run id>) or tag again`);
 }
}

/** True when every path that differs between the two commits is documentation by the CI classifier itself
 * (classifyFile: a directory named LICENSE or DCO is code). Any git error counts as a difference. */
export function codeEqual(git, a, b) {
 let names;
 try { names = git(['diff', '--name-only', '-z', '--no-renames', '--no-ext-diff', '--no-textconv', a, b, '--']); } catch { return false; }
 if (typeof names !== 'string') return false;
 return names.split('\0').filter(Boolean).every(path => classifyFile(path).docs === true);
}

/** The job runs main's current code: the checkout is GITHUB_SHA, with no code difference from origin/main. */
export function assertCheckoutIsMain(env, git) {
 if (!COMMIT.test(env.GITHUB_SHA ?? '')) throw new Error('Invalid GITHUB_SHA');
 if (git(['rev-parse', 'HEAD']) !== env.GITHUB_SHA) throw new Error('The checked-out HEAD differs from GITHUB_SHA');
 if (!codeEqual(git, 'HEAD', 'origin/main')) throw new Error('The keyed workflow code is no longer main’s tip; start a new release request');
}

/** One release at a time: no other desktop release run may be unfinished. */
export async function assertNoOtherReleaseRun(env, api) {
 if (!RUN_ID.test(env.GITHUB_RUN_ID ?? '') || !Number.isSafeInteger(Number(env.GITHUB_RUN_ID))) throw new Error('Invalid GITHUB_RUN_ID');
 for (const status of ACTIVE_STATUSES) {
  const result = await api(`/repos/${REPOSITORY}/actions/workflows/desktop-release.yml/runs?status=${status}&per_page=100`);
  if (!Array.isArray(result?.workflow_runs)) throw new Error('Malformed workflow run list');
  const other = result.workflow_runs.find(run => String(run?.id) !== env.GITHUB_RUN_ID);
  if (other) throw new Error(`Another desktop release run (${other.id}) is ${status}; re-run this release's request run after it finishes`);
 }
}
