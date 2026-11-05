import { setTimeout as delay } from 'node:timers/promises';

/**
 * Re-runs, once, the jobs of a CI run that GitHub never gave a runner. Such a job waits for a runner until
 * GitHub gives up and reports it cancelled, with no runner and no steps: a capacity problem, not a defect in
 * the change. The same predicate is NEVER_STARTED in scripts/ci-watchdog.py (Python cannot be imported here).
 */
export function neverStarted(job) {
  return job.status === 'completed' && job.conclusion === 'cancelled' && !job.runner_name && !(job.steps?.length > 0);
}

export function starvedJobs(jobs) {
  return jobs.filter(neverStarted);
}

/** Aggregate jobs fail because a starved job did, so they say nothing about whether the change is broken. */
export const AGGREGATE_JOBS = ['ci-ok', 'server-ok', 'desktop-ok', 'ci-watchdog'];

/** A job that really ran and failed: failure or timeout, or cancelled after it started (a runner or steps). */
function realFailure(job) {
  if (AGGREGATE_JOBS.includes(job.name)) return false;
  if (job.conclusion === 'failure' || job.conclusion === 'timed_out') return true;
  return job.conclusion === 'cancelled' && !neverStarted(job);
}

/**
 * rerun-failed-jobs re-runs every failed job too, so a real failure must never get a free second try: only a
 * failed run whose sole damage is jobs that never got a runner is re-run. A cancelled run (watchdog after a real
 * failure, fail-fast siblings, supersede, user cancel) also leaves never-started jobs behind and is never re-run.
 */
export function shouldRerun({ attempt, conclusion, jobs }) {
  return attempt === 1 && conclusion === 'failure' && starvedJobs(jobs).length > 0 && !jobs.some(realFailure);
}

const API_ORIGIN = 'https://api.github.com';
const RETRY_DELAYS = [1000, 3000];
const PAGE_SIZE = 100;

/** One GitHub API call. GETs retry a 5xx or a network error; a write is never retried. The token is never logged. */
async function call({ token, fetchImpl, sleep, path, method = 'GET' }) {
  const attempts = method === 'GET' ? RETRY_DELAYS.length + 1 : 1;
  for (let attempt = 1; ; attempt++) {
    let response;
    try {
      response = await fetchImpl(new URL(path, API_ORIGIN), {
        method,
        headers: { Authorization: `Bearer ${token}`, Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28' },
        redirect: 'error',
        signal: AbortSignal.timeout(30000),
      });
    } catch (error) {
      if (attempt >= attempts) throw error;
      await sleep(RETRY_DELAYS[attempt - 1]);
      continue;
    }
    if (response.status >= 500 && attempt < attempts) {
      await response.body?.cancel();
      await sleep(RETRY_DELAYS[attempt - 1]);
      continue;
    }
    if (!response.ok) throw new Error(`GitHub API ${method} ${path.split('?')[0]} failed (${response.status})`);
    return method === 'GET' ? response.json() : null;
  }
}

export async function run(env, { fetchImpl = fetch, sleep = (ms) => delay(ms), log = (line) => console.log(line) } = {}) {
  const { GH_TOKEN: token, GITHUB_REPOSITORY: repository, RUN_ID: runId, RUN_ATTEMPT: attempt, RUN_CONCLUSION: conclusion } = env;
  if (!token) throw new Error('GH_TOKEN is required');
  if (!/^[\w.-]+\/[\w.-]+$/.test(repository ?? '')) throw new Error('GITHUB_REPOSITORY must be owner/name');
  if (!/^[1-9]\d*$/.test(runId ?? '')) throw new Error('RUN_ID must be a run number');
  if (Number(attempt) !== 1) { log(`run ${runId} attempt ${attempt}: only attempt 1 is re-run`); return false; }
  if (!/^[a-z_]+$/.test(conclusion ?? '')) throw new Error('RUN_CONCLUSION must be a run conclusion');
  if (conclusion !== 'failure') { log(`run ${runId} concluded ${conclusion}; only a failed run is re-run`); return false; }

  const api = { token, fetchImpl, sleep };
  const base = `/repos/${repository}/actions/runs/${runId}`;
  const jobs = [];
  for (let page = 1; ; page++) {
    const body = await call({ ...api, path: `${base}/attempts/1/jobs?per_page=${PAGE_SIZE}&page=${page}` });
    jobs.push(...body.jobs);
    if (body.jobs.length < PAGE_SIZE) break;
  }
  const starved = starvedJobs(jobs);
  if (!shouldRerun({ attempt: 1, conclusion, jobs })) { log(`run ${runId}: no starved job, or a job really failed; nothing to do`); return false; }
  // Job ids, never names: a fork controls names, and a line starting with :: is a workflow command.
  log(`run ${runId}: ${starved.length} job(s) never got a runner (ids ${starved.map((job) => Number(job.id)).join(', ')}); re-running failed jobs once`);
  await call({ ...api, path: `${base}/rerun-failed-jobs`, method: 'POST' });
  return true;
}

if (import.meta.url === `file://${process.argv[1]}`) {
  await run(process.env);
}
