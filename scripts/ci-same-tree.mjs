import { appendFileSync, realpathSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';
import { GATED_WORKFLOWS, hasSkippedJob, isGreen, isOwnRun, request } from './server-release-gate.mjs';

/**
 * A main push that a green pull-request run already proved (CI speed plan, Task 6). desktop-gpui.yml's `check-tree` job
 * runs this on a push to main only; every other event decides "run in full" without asking the API. server.yml has no
 * such skip: its main push runs every area (Jonas, 2026-11-18).
 *
 * coveredByPr is true only when a pull_request run of exactly `workflowFile`, in this repository (not a fork's):
 *   - completed with conclusion success, and has no job named `same-tree: skipped`;
 *   - belongs to a pull request whose base is main in this repository (the run's pull_requests, or, when GitHub left
 *     that empty, the pull requests of its head commit), and to no pull request with another base;
 *   - has a head commit (`run.head_sha`, the PR branch head, not refs/pull/N/merge) whose tree equals `sha`'s tree;
 *   - and that head contains main's previous commit (`sha`'s first parent). The run tested the head merged into the
 *     main of its day, an ancestor of that parent, so the tested tree is the head's own tree: exactly `sha`'s tree.
 * Any error, missing field or unexpected answer is "not covered": the push runs in full. It never skips on uncertainty.
 *
 * Usage (in the job): GH_TOKEN=... WORKFLOW_FILE=desktop-gpui.yml node scripts/ci-same-tree.mjs
 *   reads GITHUB_REPOSITORY, GITHUB_SHA, GITHUB_EVENT_NAME, GITHUB_REF and appends `skip=true|false` to GITHUB_OUTPUT.
 */
const COMMIT = /^[a-f0-9]{40}$/;
const BASE = 'main';
/** The newest pull_request runs listed, and the most distinct head commits compared, before deciding "run in full". */
const MAX_HEADS = 50;

export async function coveredByPr({ sha, workflowFile, token, repo, fetchImpl = fetch, sleep = (ms) => delay(ms), log = (line) => console.log(line) }) {
  try {
    return await decide({ sha, workflowFile, token, repo, fetchImpl, sleep, log });
  } catch (error) {
    log(`same-tree: ${error?.message ?? error}; the push runs in full`);
    return false;
  }
}

async function decide({ sha, workflowFile, token, repo, fetchImpl, sleep, log }) {
  const gated = Object.values(GATED_WORKFLOWS).find((w) => w.file === workflowFile);
  if (!gated) throw new Error(`unknown workflow file ${String(workflowFile)}`);
  if (!token) throw new Error('GH_TOKEN is required');
  if (!/^[\w.-]+\/[\w.-]+$/.test(repo ?? '')) throw new Error('the repository must be owner/name');
  if (typeof sha !== 'string' || !COMMIT.test(sha)) throw new Error('the pushed sha must be 40 lowercase hex characters');

  const api = (path) => request({ token, fetchImpl, sleep, log, path });
  const commits = new Map();
  const commitOf = async (commit) => {
    if (!commits.has(commit)) {
      const c = await api(`/repos/${repo}/git/commits/${commit}`);
      const tree = c?.tree?.sha;
      if (typeof tree !== 'string' || !COMMIT.test(tree)) throw new Error(`GitHub returned no tree for commit ${commit}`);
      commits.set(commit, { tree, parents: Array.isArray(c.parents) ? c.parents.map((p) => p?.sha) : [] });
    }
    return commits.get(commit);
  };
  const pushed = await commitOf(sha);
  const parent = pushed.parents[0];
  if (typeof parent !== 'string' || !COMMIT.test(parent)) { log(`same-tree: ${sha} has no parent to compare with; the push runs in full`); return false; }

  const repoUrl = `https://api.github.com/repos/${repo}`;
  /** Every pull request the run belongs to targets main in this repository (and there is at least one). */
  const targetsMain = async (r) => {
    const listed = Array.isArray(r.pull_requests) ? r.pull_requests : [];
    if (listed.length) return listed.every((p) => p?.base?.ref === BASE && p?.base?.repo?.url === repoUrl);
    const pulls = await api(`/repos/${repo}/commits/${r.head_sha}/pulls`);
    if (!Array.isArray(pulls)) throw new Error(`GitHub returned a malformed pull request list for ${r.head_sha}`);
    const own = pulls.filter((p) => p?.head?.sha === r.head_sha);
    return own.length > 0 && own.every((p) => p?.base?.ref === BASE && p?.base?.repo?.full_name === repo);
  };
  /** The head contains main's previous commit. */
  const containsParent = async (head) => {
    const c = await api(`/repos/${repo}/compare/${parent}...${head}`);
    return (c?.status === 'ahead' || c?.status === 'identical') && c?.behind_by === 0;
  };

  const listing = await api(`/repos/${repo}/actions/workflows/${gated.file}/runs?event=pull_request&status=completed&per_page=100`);
  const runs = (Array.isArray(listing?.workflow_runs) ? listing.workflow_runs : [])
    .filter((r) => isOwnRun(r, repo, gated) && r.event === 'pull_request' && isGreen(r))
    .sort((a, b) => b.run_number - a.run_number || b.id - a.id);
  const heads = new Set();
  for (const r of runs) {
    if (!heads.has(r.head_sha) && heads.size >= MAX_HEADS) continue;
    heads.add(r.head_sha);
    if ((await commitOf(r.head_sha)).tree !== pushed.tree) continue;
    if (!(await targetsMain(r))) { log(`same-tree: run ${r.id} (#${r.run_number}) has ${sha}'s tree but is not a pull request into ${BASE}; it does not count`); continue; }
    if (!(await containsParent(r.head_sha))) { log(`same-tree: run ${r.id} (#${r.run_number}) has ${sha}'s tree but its head ${r.head_sha} lacks ${parent}; it does not count`); continue; }
    if (await hasSkippedJob(api, repo, r)) continue;
    log(`same-tree: pull_request run ${r.id} (#${r.run_number}, head ${r.head_sha}) is green on ${sha}'s tree (${pushed.tree}); this push skips`);
    return true;
  }
  log(`same-tree: no green pull_request run of ${gated.file} on ${sha}'s tree (${pushed.tree}); the push runs in full`);
  return false;
}

/** The job's entry point: decides, appends `skip=true|false` to GITHUB_OUTPUT and returns the decision. */
export async function main(env, { fetchImpl = fetch, sleep, log = (line) => console.log(line) } = {}) {
  let skip = false;
  if (env.GITHUB_EVENT_NAME === 'push' && env.GITHUB_REF === `refs/heads/${BASE}`) {
    skip = await coveredByPr({ sha: env.GITHUB_SHA, workflowFile: env.WORKFLOW_FILE, token: env.GH_TOKEN, repo: env.GITHUB_REPOSITORY, fetchImpl, ...(sleep ? { sleep } : {}), log });
  } else {
    log(`same-tree: ${String(env.GITHUB_EVENT_NAME)} on ${String(env.GITHUB_REF)} is not a push to ${BASE}; the run goes ahead in full`);
  }
  if (!env.GITHUB_OUTPUT) throw new Error('GITHUB_OUTPUT is required');
  appendFileSync(env.GITHUB_OUTPUT, `skip=${skip === true}\n`);
  return skip === true;
}

function isMain() {
  try { return realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url)); } catch { return false; }
}
if (isMain()) await main(process.env);
