import { execFileSync } from 'node:child_process';
import { setTimeout as delay } from 'node:timers/promises';
import { BUILD_TARGETS, TARGETS } from './verify-release.mjs';

/** git for the release jobs: no hooks, no fsmonitor, no system or global config, whatever the checkout holds. */
export const GIT_HARDENING = Object.freeze(['-c', 'core.hooksPath=/dev/null', '-c', 'core.fsmonitor=']);
export function hardenedGit(cwd) {
 const env = { ...process.env, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null' };
 return args => execFileSync('git', [...GIT_HARDENING, ...args], { cwd, env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();
}

/** Shared GitHub API lookups for the desktop release flow. Every answer is
 * re-derived from the API; nothing here trusts a value from a workflow event. */
export const REPOSITORY = 'dilla-chat/dilla';
const API_ORIGIN = 'https://api.github.com';
const COMMIT = /^[a-f0-9]{40}$/;
const RUN_ID = /^[1-9]\d*$/;

/** The exact desktop.yml artifact set one release consumes (desktop.yml upload names). */
export const EXPECTED_ARTIFACTS = Object.freeze([
 ...BUILD_TARGETS.map(target => `desktop-installers-${target}`),
 ...TARGETS.map(target => `desktop-performance-${target}`),
 ...TARGETS.map(target => `desktop-installed-account-${target}`),
]);
const RELEASE_ARTIFACT = /^desktop-(installers|performance|installed-account)-/;

/** Reads (GET only) retry a 5xx or a network error: three attempts, 1 s then 3 s apart. A 4xx is never retried,
 * and neither is any write (POST, PATCH, PUT, DELETE): publish must not double-create a release or double-delete an asset. */
export const API_RETRY_DELAYS = Object.freeze([1000, 3000]);

export function createGithubApi({ token, fetchImpl = fetch, sleep = ms => delay(ms), log = line => console.warn(line) }) {
 if (typeof token !== 'string' || !token) throw new Error('The release flow requires the workflow’s scoped GitHub token');
 return async function api(path, options = {}, optional404 = false) {
  const url = new URL(path, API_ORIGIN);
  if (url.origin !== API_ORIGIN || !url.pathname.startsWith(`/repos/${REPOSITORY}/`)) throw new Error('Invalid GitHub API destination');
  const method = options.method ?? 'GET';
  const body = options.body === undefined ? undefined : JSON.stringify(options.body);
  const attempts = method === 'GET' ? API_RETRY_DELAYS.length + 1 : 1;
  for (let attempt = 1; ; attempt++) {
   const last = attempt >= attempts;
   // The log names the method, path (no query) and status or error class only: never the token or the full URL.
   const retry = async reason => { log(`GitHub API ${method} ${url.pathname} ${reason}; retrying (attempt ${attempt + 1} of ${attempts})`); await sleep(API_RETRY_DELAYS[attempt - 1]); };
   let response;
   try {
    response = await fetchImpl(url, {
     method,
     headers: { Authorization: `Bearer ${token}`, Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28', ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) },
     ...(body === undefined ? {} : { body }),
     redirect: 'error', signal: AbortSignal.timeout(30000),
    });
   } catch (error) {
    if (last) throw error;
    await retry(`network error (${error?.name ?? 'Error'})`);
    continue;
   }
   if (response.status >= 500 && response.status <= 599 && !last) { await response.body?.cancel(); await retry(`failed (${response.status})`); continue; }
   if (optional404 && response.status === 404) return null;
   if (!response.ok) throw new Error(`GitHub API request failed (${response.status})`);
   return response.status === 204 ? null : response.json();
  }
 };
}

function runId(value) {
 const text = String(value);
 if (!RUN_ID.test(text) || !Number.isSafeInteger(Number(text))) throw new Error('Invalid CI run id');
 return text;
}

/** The newest successful `ci` push run on main for exactly this commit. */
export async function findGreenPushRun(api, sha) {
 if (typeof sha !== 'string' || !COMMIT.test(sha)) throw new Error('Invalid commit sha');
 const result = await api(`/repos/${REPOSITORY}/actions/workflows/ci.yml/runs?head_sha=${sha}&event=push&branch=main&status=completed&per_page=100`);
 const runs = (Array.isArray(result?.workflow_runs) ? result.workflow_runs : []).filter(run => run && run.name === 'ci' && run.path === '.github/workflows/ci.yml'
  && run.event === 'push' && run.head_branch === 'main' && run.head_sha === sha && run.status === 'completed' && run.conclusion === 'success'
  && run.repository?.full_name === REPOSITORY && run.head_repository?.full_name === REPOSITORY
  && Number.isSafeInteger(run.id) && run.id > 0 && Number.isSafeInteger(run.run_number));
 if (!runs.length) throw new Error(`There is no green ci push run on main for ${sha}`);
 return runs.reduce((best, run) => run.run_number > best.run_number ? run : best);
}

async function paged(api, path, key) {
 const items = [];
 for (let page = 1; ; page++) {
  const result = await api(`${path}${path.includes('?') ? '&' : '?'}per_page=100&page=${page}`);
  const list = result?.[key];
  if (!Array.isArray(list)) throw new Error('Malformed GitHub API page');
  items.push(...list);
  if (list.length < 100) return items;
  if (page >= 100) throw new Error('Too many GitHub API pages');
 }
}

export async function fetchCiJobs(api, id) {
 return paged(api, `/repos/${REPOSITORY}/actions/runs/${runId(id)}/jobs?filter=latest`, 'jobs');
}

/** Exact-match tag lookup; an annotated tag is followed once and must name a commit. */
export async function resolveTagCommit(api, tag) {
 if (typeof tag !== 'string' || !/^[A-Za-z0-9._-]+$/.test(tag) || tag.startsWith('.')) throw new Error('Invalid tag name');
 const ref = await api(`/repos/${REPOSITORY}/git/ref/tags/${encodeURIComponent(tag)}`, {}, true);
 if (!ref || ref.ref !== `refs/tags/${tag}` || !ref.object || !COMMIT.test(ref.object.sha ?? '')) throw new Error('The tag does not exist: ' + tag);
 let object = ref.object;
 if (object.type === 'tag') {
  const annotated = await api(`/repos/${REPOSITORY}/git/tags/${object.sha}`);
  object = annotated?.object;
 }
 if (!object || object.type !== 'commit' || typeof object.sha !== 'string' || !COMMIT.test(object.sha)) throw new Error('The tag must point at a commit: ' + tag);
 return object.sha;
}

export async function listRunArtifacts(api, id) {
 const artifacts = await paged(api, `/repos/${REPOSITORY}/actions/runs/${runId(id)}/artifacts`, 'artifacts');
 return artifacts.map(({ id: artifactId, name, digest, expired }) => ({ id: artifactId, name, digest, expired }));
}

/** The run must carry exactly the expected release artifacts: no extras under
 * the release prefixes, no duplicates, nothing expired, each with an API digest. */
export function selectReleaseArtifacts(artifacts) {
 if (!Array.isArray(artifacts)) throw new Error('Missing CI artifact list');
 const relevant = artifacts.filter(artifact => typeof artifact?.name === 'string' && RELEASE_ARTIFACT.test(artifact.name));
 const byName = {};
 for (const artifact of relevant) {
  if (Object.hasOwn(byName, artifact.name)) throw new Error('Duplicate CI release artifact: ' + artifact.name);
  byName[artifact.name] = artifact;
 }
 const names = Object.keys(byName);
 if (names.length !== EXPECTED_ARTIFACTS.length || EXPECTED_ARTIFACTS.some(name => !Object.hasOwn(byName, name))) throw new Error('The CI run must carry exactly the expected desktop release artifacts');
 for (const name of EXPECTED_ARTIFACTS) {
  const artifact = byName[name];
  if (artifact.expired !== false) throw new Error('CI release artifact expired: ' + name);
  if (typeof artifact.digest !== 'string' || !/^sha256:[a-f0-9]{64}$/.test(artifact.digest)) throw new Error('CI release artifact has no digest: ' + name);
  if (!Number.isSafeInteger(artifact.id) || artifact.id < 1) throw new Error('Invalid CI release artifact id: ' + name);
 }
 return { ids: EXPECTED_ARTIFACTS.map(name => byName[name].id), byName: Object.fromEntries(EXPECTED_ARTIFACTS.map(name => [name, { ...byName[name] }])) };
}
