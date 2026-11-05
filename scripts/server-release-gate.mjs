import { setTimeout as delay } from 'node:timers/promises';
import { realpathSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

/**
 * release.yml's gate: a v* tag is released only if the `server` workflow went green on a push to main for exactly
 * the tagged commit. The desktop workflow gates the desktop release the same way (packages/desktop/scripts/ci-run.mjs);
 * neither run satisfies the other's gate. Everything is re-derived from the API, never from the tag's name.
 *
 * A tag pushed right after its merge finds the commit's server run still queued or in progress: the gate then polls
 * every 30 s for up to 60 min (release.yml's tag job has the default 360 min timeout). It refuses at once when the
 * commit has no such run at all or one concluded anything but success, and when the bound expires.
 *
 * Usage: GH_TOKEN=... GITHUB_REPOSITORY=owner/name node scripts/server-release-gate.mjs <tag commit sha>
 */
const API_ORIGIN = 'https://api.github.com';
const RETRY_DELAYS = [1000, 3000];
const COMMIT = /^[a-f0-9]{40}$/;
const WORKFLOW_FILE = 'server.yml';
const WORKFLOW_PATH = `.github/workflows/${WORKFLOW_FILE}`;
const WORKFLOW_NAME = 'server';
const POLL_MS = 30 * 1000;
const BOUND_MS = 60 * 60 * 1000;

/** One GitHub API GET. A 5xx or a network error is retried; a 4xx is not. Redirects are refused and the token is never logged. */
async function get({ token, fetchImpl, sleep, log, path }) {
  const attempts = RETRY_DELAYS.length + 1;
  for (let attempt = 1; ; attempt++) {
    const last = attempt >= attempts;
    // The log names the path (no query) and the status or error class only.
    const retry = async (reason) => { log(`GitHub API GET ${path.split('?')[0]} ${reason}; retrying (attempt ${attempt + 1} of ${attempts})`); await sleep(RETRY_DELAYS[attempt - 1]); };
    let response;
    try {
      response = await fetchImpl(new URL(path, API_ORIGIN), {
        method: 'GET',
        headers: { Authorization: `Bearer ${token}`, Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28' },
        redirect: 'error',
        signal: AbortSignal.timeout(30000),
      });
    } catch (error) {
      if (last) throw error;
      await retry(`network error (${error?.name ?? 'Error'})`);
      continue;
    }
    if (response.status >= 500 && response.status <= 599 && !last) { await response.body?.cancel(); await retry(`failed (${response.status})`); continue; }
    if (!response.ok) throw new Error(`GitHub API request failed (${response.status})`);
    return response.json();
  }
}

/** A run of the `server` workflow, pushed to main in this repository, for exactly this commit (whatever its status). */
const isServerPushRun = (r, sha, repository) => r && r.name === WORKFLOW_NAME && r.path === WORKFLOW_PATH
  && r.event === 'push' && r.head_branch === 'main' && r.head_sha === sha
  && r.repository?.full_name === repository && r.head_repository?.full_name === repository
  && Number.isSafeInteger(r.id) && r.id > 0 && Number.isSafeInteger(r.run_number);

const duration = (ms) => (ms % 60000 === 0 ? `${ms / 60000} min` : `${ms / 1000} s`);

/**
 * The newest successful `server` push run on main for exactly this commit. While there is none but such a run is still
 * queued or in progress, it polls every `pollMs` for at most `boundMs`; it throws when there is no run, when one
 * completed without success, and when the bound expires.
 */
export async function run(env, sha, { fetchImpl = fetch, sleep = (ms) => delay(ms), log = (line) => console.log(line), now = () => Date.now(), pollMs = POLL_MS, boundMs = BOUND_MS } = {}) {
  const { GH_TOKEN: token, GITHUB_REPOSITORY: repository } = env;
  if (!token) throw new Error('GH_TOKEN is required');
  if (!/^[\w.-]+\/[\w.-]+$/.test(repository ?? '')) throw new Error('GITHUB_REPOSITORY must be owner/name');
  if (typeof sha !== 'string' || !COMMIT.test(sha)) throw new Error('The tag commit sha must be 40 lowercase hex characters');

  const refuse = (why) => new Error(`There is no green server push run on main for ${sha}${why}; the server release needs one`);
  const started = now();
  for (;;) {
    // No status filter in the query: the in-flight runs are listed too, and every filter is re-applied below.
    const result = await get({ token, fetchImpl, sleep, log, path: `/repos/${repository}/actions/workflows/${WORKFLOW_FILE}/runs?head_sha=${sha}&event=push&branch=main&per_page=100` });
    const runs = (Array.isArray(result?.workflow_runs) ? result.workflow_runs : []).filter((r) => isServerPushRun(r, sha, repository));
    const green = runs.filter((r) => r.status === 'completed' && r.conclusion === 'success');
    if (green.length) {
      const newest = green.reduce((best, r) => (r.run_number > best.run_number ? r : best));
      log(`server run ${newest.id} (#${newest.run_number}) is green on main for ${sha}`);
      return newest;
    }
    const red = runs.find((r) => r.status === 'completed');
    if (red) throw refuse(`: server run ${red.id} (#${red.run_number}) concluded ${String(red.conclusion)}`);
    const pending = runs.find((r) => r.status !== 'completed');
    if (!pending) throw refuse('');
    const waited = now() - started;
    if (waited + pollMs > boundMs) throw refuse(`: server run ${pending.id} (#${pending.run_number}) is still ${String(pending.status)} after ${duration(boundMs)}`);
    log(`server run ${pending.id} (#${pending.run_number}) is ${String(pending.status)} for ${sha}; waiting ${duration(pollMs)} (${duration(waited)} of ${duration(boundMs)} so far)`);
    await sleep(pollMs);
  }
}

function isMain() {
  try { return realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url)); } catch { return false; }
}
if (isMain()) {
  if (process.argv.length !== 3) throw new Error('Usage: server-release-gate.mjs TAG_SHA');
  await run(process.env, process.argv[2]);
}
