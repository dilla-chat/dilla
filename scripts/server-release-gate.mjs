import { setTimeout as delay } from 'node:timers/promises';
import { realpathSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { ISSUE_TITLE, ISSUE_LABEL } from './ci-nightly-issue.mjs';

/**
 * The release gates (CI speed plan, Task 5). Everything is re-derived from the API, never from the tag's name. A run
 * counts for a tag when its head commit's tree (`GET /repos/{r}/git/commits/{sha}` → `.tree.sha`) equals the tag
 * commit's tree, and never when one of its jobs is named `same-tree: skipped` (a desktop-gpui main push that a PR run covered).
 *
 * release.yml (`server`): a green `server.yml` run on the tag's tree from a push to main, never a
 * pull_request run (Jonas, 2026-11-18: a PR run may skip areas; the main push runs every one). Runs of other events are
 * ignored: they neither count, nor are waited for, nor refuse. While there is none but such a run
 * is queued or in progress, it polls every 30 s for up to 150 min. It refuses at once when the tree has no run at all,
 * or one concluded anything but success; and when the bound expires.
 *
 * desktop-gpui-release.yml (`--workflow=desktop-gpui`):
 *   1. it refuses while an open issue labelled `ci-nightly` (the red nightly's, `nightly desktop-gpui red`) exists;
 *   2. it passes on the newest green `desktop-gpui.yml` run titled exactly `desktop-gpui [nightly]` or
 *      `desktop-gpui [release]` (a schedule or workflow_dispatch run) on the tag's tree; a `[pr]` run never counts;
 *   3. while such a run is queued or in progress it waits for it; otherwise it dispatches desktop-gpui.yml on the tag
 *      (RELEASE_TAG) with tier=release and follows that one run until it is green (pass) or anything else (refuse,
 *      `cancelled` included), all within 150 min. The issue is checked again before it passes.
 * Without RELEASE_TAG (desktop-gpui-sign.yml's gate, which may not dispatch) step 3 refuses instead of dispatching.
 * A green server run never satisfies the desktop-gpui gate, nor a desktop-gpui run the server gate.
 *
 * Usage: GH_TOKEN=... GITHUB_REPOSITORY=owner/name [RELEASE_TAG=...] node scripts/server-release-gate.mjs [--workflow=desktop-gpui] <tag commit sha>
 */
const API_ORIGIN = 'https://api.github.com';
const RETRY_DELAYS = [1000, 3000];
const COMMIT = /^[a-f0-9]{40}$/;
const TAG_NAME = /^[\w][\w.+-]*$/;
/** The workflows a release may gate on: the run's `name` and its file. */
export const GATED_WORKFLOWS = {
  server: { name: 'server', file: 'server.yml' },
  'desktop-gpui': { name: 'desktop-gpui', file: 'desktop-gpui.yml' },
};
/** The run titles (desktop-gpui.yml's run-name) a desktop-gpui release accepts, and the events that produce them. */
export const ACCEPTED_TITLES = { 'desktop-gpui [nightly]': ['schedule', 'workflow_dispatch'], 'desktop-gpui [release]': ['workflow_dispatch'] };
export const RELEASE_TITLE = 'desktop-gpui [release]';
/** The job a main push covered by a PR run carries (Task 6); such a run proves nothing. */
export const SKIPPED_JOB = 'same-tree: skipped';
const POLL_MS = 30 * 1000;
export const BOUND_MS = 150 * 60 * 1000;
/** How long a dispatched run may take to show up in the listing before the gate gives up on it. */
const APPEAR_MS = 10 * 60 * 1000;
/** GitHub's clock and the runner's may differ: a dispatched run created this much before the dispatch still counts. */
const CLOCK_SKEW_MS = 60 * 1000;
const MAX_JOB_PAGES = 20;

/** One GitHub API request. A GET is retried on a 5xx or a network error; a 4xx is not, nor a POST. Redirects are refused and the token is never logged. */
export async function request({ token, fetchImpl, sleep, log, path, method = 'GET', body }) {
  const attempts = method === 'GET' ? RETRY_DELAYS.length + 1 : 1;
  for (let attempt = 1; ; attempt++) {
    const last = attempt >= attempts;
    // The log names the path (no query) and the status or error class only.
    const retry = async (reason) => { log(`GitHub API ${method} ${path.split('?')[0]} ${reason}; retrying (attempt ${attempt + 1} of ${attempts})`); await sleep(RETRY_DELAYS[attempt - 1]); };
    let response;
    try {
      response = await fetchImpl(new URL(path, API_ORIGIN), {
        method,
        headers: { Authorization: `Bearer ${token}`, Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28', ...(body ? { 'Content-Type': 'application/json' } : {}) },
        body: body ? JSON.stringify(body) : undefined,
        redirect: 'error',
        signal: AbortSignal.timeout(30000),
      });
    } catch (error) {
      if (last) throw error;
      await retry(`network error (${error?.name ?? 'Error'})`);
      continue;
    }
    if (response.status >= 500 && response.status <= 599 && !last) { await response.body?.cancel(); await retry(`failed (${response.status})`); continue; }
    if (!response.ok) throw new Error(`GitHub API ${method} ${path.split('?')[0]} failed (${response.status})`);
    if (response.status === 204) return null;
    return response.json();
  }
}

/** A run of the gated workflow in this repository (not a fork's), whatever its event, branch, commit or status. */
export const isOwnRun = (r, repository, { name, file }) => r && r.name === name && r.path === `.github/workflows/${file}`
  && r.repository?.full_name === repository && r.head_repository?.full_name === repository
  && typeof r.head_sha === 'string' && COMMIT.test(r.head_sha)
  && Number.isSafeInteger(r.id) && r.id > 0 && Number.isSafeInteger(r.run_number);

/** A server run the server gate may take: a push to main (every area on); never a pull_request run. The workflow_dispatch branch is dormant: server.yml has no dispatch trigger. */
export const isFullServerRun = (r) => (r.event === 'push' && r.head_branch === 'main') || r.event === 'workflow_dispatch';

/** A desktop-gpui run of the nightly or release tier: its title is exactly the tier's and its event can produce it. */
const isTierRun = (r) => Object.hasOwn(ACCEPTED_TITLES, r.display_title ?? '') && ACCEPTED_TITLES[r.display_title].includes(r.event);

export const isGreen = (r) => r.status === 'completed' && r.conclusion === 'success';
const newestFirst = (a, b) => b.run_number - a.run_number || b.id - a.id;
const duration = (ms) => (ms % 60000 === 0 ? `${ms / 60000} min` : `${ms / 1000} s`);
const runsOf = (result) => (Array.isArray(result?.workflow_runs) ? result.workflow_runs : []);

/** Whether any job of run `r` (every attempt, every page) is named SKIPPED_JOB; `api(path)` is a bound `request`. Throws when the list is incomplete. */
export async function hasSkippedJob(api, repository, r) {
  const names = [];
  for (let page = 1; ; page++) {
    const result = await api(`/repos/${repository}/actions/runs/${r.id}/jobs?filter=all&per_page=100&page=${page}`);
    const jobs = Array.isArray(result?.jobs) ? result.jobs : null;
    if (!jobs || !Number.isSafeInteger(result.total_count)) throw new Error(`GitHub returned a malformed job list for run ${r.id}`);
    names.push(...jobs.map((j) => j?.name));
    if (names.length >= result.total_count) break;
    if (!jobs.length || page >= MAX_JOB_PAGES) throw new Error(`cannot list every job of run ${r.id}`);
  }
  return names.includes(SKIPPED_JOB);
}

export async function run(env, sha, { fetchImpl = fetch, sleep = (ms) => delay(ms), log = (line) => console.log(line), now = () => Date.now(), pollMs = POLL_MS, boundMs = BOUND_MS, workflow = 'server' } = {}) {
  if (!Object.hasOwn(GATED_WORKFLOWS, workflow)) throw new Error(`Unknown gated workflow: ${workflow}`);
  const gated = GATED_WORKFLOWS[workflow];
  const label = gated.name;
  const { GH_TOKEN: token, GITHUB_REPOSITORY: repository, RELEASE_TAG: tag } = env;
  if (!token) throw new Error('GH_TOKEN is required');
  if (!/^[\w.-]+\/[\w.-]+$/.test(repository ?? '')) throw new Error('GITHUB_REPOSITORY must be owner/name');
  if (typeof sha !== 'string' || !COMMIT.test(sha)) throw new Error('The tag commit sha must be 40 lowercase hex characters');
  if (tag !== undefined && tag !== '' && (typeof tag !== 'string' || !TAG_NAME.test(tag) || tag.includes('..'))) throw new Error('RELEASE_TAG must be a plain tag name');

  const api = (path, extra = {}) => request({ token, fetchImpl, sleep, log, path, ...extra });
  const trees = new Map();
  const treeOf = async (commit) => {
    if (!trees.has(commit)) {
      const tree = (await api(`/repos/${repository}/git/commits/${commit}`))?.tree?.sha;
      if (typeof tree !== 'string' || !COMMIT.test(tree)) throw new Error(`GitHub returned no tree for commit ${commit}`);
      trees.set(commit, tree);
    }
    return trees.get(commit);
  };
  /** Whether any job of the run (every attempt, every page) is the skipped marker; cached per run attempt. */
  const skipped = new Map();
  const isSkipped = async (r) => {
    const key = `${r.id}:${r.run_attempt}`;
    if (!skipped.has(key)) skipped.set(key, await listsSkippedJob(r));
    return skipped.get(key);
  };
  const listsSkippedJob = (r) => hasSkippedJob(api, repository, r);
  /** The newest green run among `runs` without the skipped job, or null. */
  const firstCounting = async (runs) => {
    for (const r of runs.filter(isGreen).sort(newestFirst)) {
      if (await isSkipped(r)) { log(`${label} run ${r.id} (#${r.run_number}) is green but has a "${SKIPPED_JOB}" job; it does not count`); continue; }
      return r;
    }
    return null;
  };
  const tagTree = await treeOf(sha);
  /** The own runs from `listings` (deduplicated) whose head commit has the tag's tree. */
  const onTree = async (listings, keep = () => true) => {
    const byId = new Map();
    for (const path of listings) for (const r of runsOf(await api(path))) if (isOwnRun(r, repository, gated) && keep(r)) byId.set(r.id, r);
    const out = [];
    for (const r of byId.values()) if (r.head_sha === sha || await treeOf(r.head_sha) === tagTree) out.push(r);
    return out;
  };
  const what = workflow === 'server' ? 'server run' : `${label} [nightly] or [release] run`;
  const refuse = (why) => new Error(`There is no green ${what} on the tree of ${sha} (${tagTree})${why}; the ${label} release needs one`);
  const started = now();
  const waitOr = async (pending) => {
    const waited = now() - started;
    if (waited + pollMs > boundMs) throw refuse(`: ${label} run ${pending.id} (#${pending.run_number}) is still ${String(pending.status)} after ${duration(boundMs)}`);
    log(`${label} run ${pending.id} (#${pending.run_number}) is ${String(pending.status)} on the tree of ${sha}; waiting ${duration(pollMs)} (${duration(waited)} of ${duration(boundMs)} so far)`);
    await sleep(pollMs);
  };

  if (workflow === 'server') {
    const listings = [`/repos/${repository}/actions/workflows/${gated.file}/runs?head_sha=${sha}&per_page=100`, `/repos/${repository}/actions/workflows/${gated.file}/runs?per_page=100`];
    for (;;) {
      const runs = await onTree(listings, isFullServerRun);
      const green = await firstCounting(runs);
      if (green) {
        log(`${label} run ${green.id} (#${green.run_number}, ${green.event} on ${green.head_sha}) is green on the tree of ${sha}`);
        return green;
      }
      const red = runs.find((r) => r.status === 'completed' && r.conclusion !== 'success');
      if (red) throw refuse(`: ${label} run ${red.id} (#${red.run_number}) concluded ${String(red.conclusion)}`);
      const pending = runs.find((r) => r.status !== 'completed');
      if (!pending) throw refuse('');
      await waitOr(pending);
    }
  }

  // desktop-gpui: the nightly issue, then a nightly/release run on the tree, else a dispatched release run.
  const issueCheck = async () => {
    const issues = await api(`/repos/${repository}/issues?labels=${ISSUE_LABEL}&state=open&per_page=100`);
    if (!Array.isArray(issues)) throw new Error('GitHub returned a malformed issue list');
    // The nightly's own issue first; any other open issue with the label blocks just the same.
    const real = issues.filter((i) => i && !i.pull_request);
    const open = real.find((i) => i.title === ISSUE_TITLE) ?? real[0];
    if (open) throw new Error(`The ${label} release is refused while issue #${open.number} "${String(open.title)}" (label ${ISSUE_LABEL}) is open; a green nightly closes it`);
  };
  await issueCheck();
  const pass = async (r) => {
    await issueCheck();
    log(`${label} run ${r.id} (#${r.run_number}, "${r.display_title}" on ${r.head_sha}) is green on the tree of ${sha}`);
    return r;
  };
  const tierListings = ['schedule', 'workflow_dispatch'].map((event) => `/repos/${repository}/actions/workflows/${gated.file}/runs?event=${event}&per_page=100`);
  for (;;) {
    const runs = await onTree(tierListings, isTierRun);
    const green = await firstCounting(runs);
    if (green) return pass(green);
    const pending = runs.filter((r) => r.status !== 'completed').sort(newestFirst)[0];
    if (!pending) break;
    await waitOr(pending);
  }

  if (!tag) throw refuse(' and no RELEASE_TAG to dispatch a release-tier run on');
  // The nightly may have gone red while the gate waited: do not spend a ~2 h Mac run on a tree that is refused anyway.
  await issueCheck();
  const dispatchedListing = `/repos/${repository}/actions/workflows/${gated.file}/runs?event=workflow_dispatch&head_sha=${sha}&per_page=100`;
  const before = new Set(runsOf(await api(dispatchedListing)).map((r) => r?.id));
  const dispatchedAt = now();
  log(`no green ${what} on the tree of ${sha}; dispatching ${gated.file} on ${tag} with tier=release`);
  await api(`/repos/${repository}/actions/workflows/${gated.file}/dispatches`, { method: 'POST', body: { ref: tag, inputs: { tier: 'release' } } });
  // The dispatch answers without a run id: the new run is the newest release-tier dispatch run of this commit and tag
  // that the listing did not hold before the dispatch, created no earlier than it.
  const isOurs = (r) => isOwnRun(r, repository, gated) && !before.has(r.id) && r.event === 'workflow_dispatch' && r.display_title === RELEASE_TITLE
    && r.head_sha === sha && r.head_branch === tag && Date.parse(r.created_at) >= dispatchedAt - CLOCK_SKEW_MS;
  let ours;
  for (;;) {
    ours = runsOf(await api(dispatchedListing)).filter(isOurs).sort(newestFirst)[0];
    if (ours) break;
    const waited = now() - dispatchedAt;
    if (waited + pollMs > Math.min(APPEAR_MS, boundMs - (dispatchedAt - started))) throw refuse(`: the dispatched ${RELEASE_TITLE} run on ${tag} did not appear within ${duration(waited)}`);
    log(`waiting ${duration(pollMs)} for the dispatched ${RELEASE_TITLE} run on ${tag} to appear`);
    await sleep(pollMs);
  }
  log(`following ${label} run ${ours.id} (#${ours.run_number}), dispatched on ${tag}`);
  for (;;) {
    const r = await api(`/repos/${repository}/actions/runs/${ours.id}`);
    if (!r || r.id !== ours.id || !isOwnRun(r, repository, gated) || r.head_sha !== sha || r.display_title !== RELEASE_TITLE || r.event !== 'workflow_dispatch') {
      throw refuse(`: run ${ours.id} no longer looks like the dispatched ${RELEASE_TITLE} run`);
    }
    if (r.status === 'completed') {
      if (r.conclusion !== 'success') throw refuse(`: the dispatched ${label} run ${r.id} (#${r.run_number}) concluded ${String(r.conclusion)}`);
      if (await isSkipped(r)) throw refuse(`: the dispatched ${label} run ${r.id} has a "${SKIPPED_JOB}" job`);
      return pass(r);
    }
    await waitOr(r);
  }
}

function isMain() {
  try { return realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url)); } catch { return false; }
}
if (isMain()) {
  const args = process.argv.slice(2);
  const flag = args.length === 2 ? /^--workflow=(.+)$/.exec(args[0]) : null;
  if (args.length !== 1 && !flag) throw new Error('Usage: server-release-gate.mjs [--workflow=desktop-gpui] TAG_SHA');
  await run(process.env, args.at(-1), { workflow: flag ? flag[1] : 'server' });
}
