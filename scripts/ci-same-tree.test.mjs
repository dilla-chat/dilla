import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { coveredByPr, main } from './ci-same-tree.mjs';

// PUSH is the main push being decided; PARENT is main before it; HEAD is the PR head that main squashed; OTHER is an unrelated commit.
const PUSH = 'a'.repeat(40), PARENT = 'b'.repeat(40), HEAD = 'c'.repeat(40), OTHER = 'd'.repeat(40);
const TREE = 'e'.repeat(40), OTHER_TREE = 'f'.repeat(40);
const OWN = { full_name: 'o/r' };
const pr = (over = {}) => ({
  number: 7, head: { ref: 'feature', sha: HEAD, repo: { id: 1, url: 'https://api.github.com/repos/o/r', name: 'r' } },
  base: { ref: 'main', sha: PARENT, repo: { id: 1, url: 'https://api.github.com/repos/o/r', name: 'r' } }, ...over,
});
const prRun = (over = {}) => ({
  id: 10, run_number: 3, run_attempt: 1, name: 'desktop-gpui', path: '.github/workflows/desktop-gpui.yml', event: 'pull_request',
  head_branch: 'feature', head_sha: HEAD, status: 'completed', conclusion: 'success', repository: OWN, head_repository: OWN,
  pull_requests: [pr()], ...over,
});

/**
 * A fake GitHub. The workflow listing deliberately ignores the `event` and `status` filters, so the script's own checks
 * are what the tests exercise. `state.status` overrides a path's answer with an HTTP status; `state.throws` makes fetch throw.
 */
function fakeGithub(over = {}) {
  const state = {
    runs: [prRun()],
    commits: { [PUSH]: { tree: TREE, parents: [PARENT] }, [HEAD]: { tree: TREE, parents: [OTHER] }, [OTHER]: { tree: OTHER_TREE, parents: [] }, [PARENT]: { tree: OTHER_TREE, parents: [] } },
    compare: { [`${PARENT}...${HEAD}`]: { status: 'ahead', behind_by: 0, ahead_by: 2 } },
    pulls: {}, jobs: {}, status: {}, throws: false, seen: [], ...over,
  };
  state.fetchImpl = async (url, init = {}) => {
    state.seen.push(`${init.method ?? 'GET'} ${url.pathname}${url.search}`);
    if (state.throws) throw new TypeError('fetch failed');
    if ((init.method ?? 'GET') !== 'GET') return new Response('', { status: 405 });
    const p = url.pathname;
    if (Object.hasOwn(state.status, p)) return new Response('', { status: state.status[p] });
    let m;
    if ((m = /^\/repos\/o\/r\/actions\/workflows\/([\w.-]+)\/runs$/.exec(p))) {
      const runs = state.runs.filter((r) => r.path === `.github/workflows/${m[1]}` || r.path?.startsWith('.github/workflows/other'));
      return Response.json({ total_count: runs.length, workflow_runs: runs.map((r) => structuredClone(r)) });
    }
    if ((m = /^\/repos\/o\/r\/actions\/runs\/(\d+)\/jobs$/.exec(p))) {
      const names = state.jobs[m[1]] ?? ['same-tree: ran', 'gpui-linux-x64'];
      return Response.json({ total_count: names.length, jobs: names.map((name, i) => ({ id: i + 1, name })) });
    }
    if ((m = /^\/repos\/o\/r\/git\/commits\/([a-f0-9]{40})$/.exec(p))) {
      const c = state.commits[m[1]];
      return c ? Response.json({ sha: m[1], tree: { sha: c.tree }, parents: c.parents.map((sha) => ({ sha })) }) : new Response('', { status: 404 });
    }
    if ((m = /^\/repos\/o\/r\/compare\/([a-f0-9]{40}\.\.\.[a-f0-9]{40})$/.exec(p))) {
      return state.compare[m[1]] ? Response.json(state.compare[m[1]]) : new Response('', { status: 404 });
    }
    if ((m = /^\/repos\/o\/r\/commits\/([a-f0-9]{40})\/pulls$/.exec(p))) return Response.json(state.pulls[m[1]] ?? []);
    return new Response('', { status: 404 });
  };
  return state;
}

const decide = (gh, over = {}) => coveredByPr({ sha: PUSH, workflowFile: 'desktop-gpui.yml', token: 't', repo: 'o/r', fetchImpl: gh.fetchImpl, sleep: async () => {}, log: () => {}, ...over });

test('aTreeAGreenPrRunCoveredIsSkipped', async () => {
  assert.equal(await decide(fakeGithub()), true);
  // The server workflow too, by its own file.
  const server = fakeGithub({ runs: [prRun({ name: 'server', path: '.github/workflows/server.yml' })] });
  assert.equal(await decide(server, { workflowFile: 'server.yml' }), true);
});

test('aTreeNoPrRunCoveredRunsInFull', async () => {
  // Main moved under the PR: its head's tree is not main's new tree.
  assert.equal(await decide(fakeGithub({ runs: [prRun({ head_sha: OTHER })] })), false);
  assert.equal(await decide(fakeGithub({ runs: [] })), false);
});

test('aRedPrRunNeverCovers', async () => {
  for (const over of [{ conclusion: 'failure' }, { conclusion: 'cancelled' }, { conclusion: 'skipped' }, { status: 'in_progress', conclusion: null }, { status: 'queued', conclusion: null }]) {
    assert.equal(await decide(fakeGithub({ runs: [prRun(over)] })), false, JSON.stringify(over));
  }
});

test('aPushRunNeverCoversItself', async () => {
  for (const event of ['push', 'schedule', 'workflow_dispatch', 'pull_request_target']) {
    // The push run of this very commit, and a run on the PR head that would otherwise count in every respect.
    const runs = [prRun({ event, head_sha: PUSH, head_branch: 'main', pull_requests: [pr({ head: { ...pr().head, sha: PUSH } })] }), prRun({ id: 11, event })];
    const compare = { [`${PARENT}...${HEAD}`]: { status: 'ahead', behind_by: 0 }, [`${PARENT}...${PUSH}`]: { status: 'ahead', behind_by: 0 } };
    assert.equal(await decide(fakeGithub({ runs, compare })), false, event);
  }
});

test('aPrAgainstAnotherBaseNeverCovers', async () => {
  const other = pr({ base: { ...pr().base, ref: 'release' } });
  assert.equal(await decide(fakeGithub({ runs: [prRun({ pull_requests: [other] })] })), false);
  // Two PRs share the head and one targets another base: the run's tested merge is ambiguous.
  assert.equal(await decide(fakeGithub({ runs: [prRun({ pull_requests: [pr(), other] })] })), false);
  // A base in another repository.
  const foreign = pr({ base: { ...pr().base, repo: { id: 2, url: 'https://api.github.com/repos/x/r', name: 'r' } } });
  assert.equal(await decide(fakeGithub({ runs: [prRun({ pull_requests: [foreign] })] })), false);
  // With no pull_requests on the run, the commit's own PRs decide: another base never covers, main does.
  const pullOf = (base) => [{ number: 7, head: { sha: HEAD }, base: { ref: base, repo: OWN } }];
  assert.equal(await decide(fakeGithub({ runs: [prRun({ pull_requests: [] })], pulls: { [HEAD]: pullOf('release') } })), false);
  assert.equal(await decide(fakeGithub({ runs: [prRun({ pull_requests: [] })], pulls: {} })), false);
  assert.equal(await decide(fakeGithub({ runs: [prRun({ pull_requests: [] })], pulls: { [HEAD]: [...pullOf('main'), ...pullOf('release')] } })), false);
  assert.equal(await decide(fakeGithub({ runs: [prRun({ pull_requests: [] })], pulls: { [HEAD]: pullOf('main') } })), true);
});

test('aForkPrRunNeverCovers', async () => {
  const fork = { full_name: 'x/r' };
  assert.equal(await decide(fakeGithub({ runs: [prRun({ head_repository: fork })] })), false);
  assert.equal(await decide(fakeGithub({ runs: [prRun({ repository: fork })] })), false);
});

test('aRunOfAnotherWorkflowNeverCovers', async () => {
  assert.equal(await decide(fakeGithub({ runs: [prRun({ path: '.github/workflows/other.yml', name: 'desktop-gpui' })] })), false);
  assert.equal(await decide(fakeGithub({ runs: [prRun({ name: 'server' })] })), false);
  // A workflow the release gates do not know is never decided from the API.
  const gh = fakeGithub();
  assert.equal(await decide(gh, { workflowFile: 'mobile.yml' }), false);
  assert.equal(gh.seen.length, 0);
});

test('aRunWithTheSkippedJobNeverCovers', async () => {
  assert.equal(await decide(fakeGithub({ jobs: { 10: ['same-tree: skipped'] } })), false);
});

test('aPrHeadMissingMainsParentNeverCovers', async () => {
  // The PR run tested its head merged into an older main; the head lacks main's previous commit, so the tested tree is unknown.
  assert.equal(await decide(fakeGithub({ compare: { [`${PARENT}...${HEAD}`]: { status: 'diverged', behind_by: 1, ahead_by: 2 } } })), false);
  assert.equal(await decide(fakeGithub({ compare: {} })), false);
});

test('anApiErrorRunsInFull', async () => {
  const lines = [];
  for (const gh of [
    fakeGithub({ throws: true }),
    fakeGithub({ status: { '/repos/o/r/actions/workflows/desktop-gpui.yml/runs': 500 } }),
    fakeGithub({ status: { [`/repos/o/r/git/commits/${PUSH}`]: 404 } }),
    fakeGithub({ status: { '/repos/o/r/actions/runs/10/jobs': 403 } }),
    fakeGithub({ commits: { [PUSH]: { tree: 'not-a-tree', parents: [PARENT] } } }),
  ]) {
    assert.equal(await decide(gh, { log: (l) => lines.push(l) }), false);
  }
  assert.ok(lines.some((l) => /runs in full/.test(l)));
  assert.ok(lines.every((l) => !l.includes('Bearer')));
});

test('onlyAPushToMainAsksTheApi', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'same-tree-'));
  const base = { GH_TOKEN: 't', GITHUB_REPOSITORY: 'o/r', GITHUB_SHA: PUSH, WORKFLOW_FILE: 'desktop-gpui.yml' };
  for (const [event, ref] of [['pull_request', 'refs/pull/7/merge'], ['schedule', 'refs/heads/main'], ['workflow_dispatch', 'refs/heads/main'], ['push', 'refs/heads/feature'], ['push', 'refs/tags/v1']]) {
    const gh = fakeGithub();
    const out = join(dir, `${event}-${ref.replaceAll('/', '_')}`);
    assert.equal(await main({ ...base, GITHUB_EVENT_NAME: event, GITHUB_REF: ref, GITHUB_OUTPUT: out }, { fetchImpl: gh.fetchImpl, log: () => {} }), false);
    assert.equal(gh.seen.length, 0, `${event} ${ref} called the API`);
    assert.equal(readFileSync(out, 'utf8'), 'skip=false\n');
  }
  const gh = fakeGithub();
  const out = join(dir, 'push-main');
  assert.equal(await main({ ...base, GITHUB_EVENT_NAME: 'push', GITHUB_REF: 'refs/heads/main', GITHUB_OUTPUT: out }, { fetchImpl: gh.fetchImpl, sleep: async () => {}, log: () => {} }), true);
  assert.equal(readFileSync(out, 'utf8'), 'skip=true\n');
  // A missing token or file decides "run in full", never a skip.
  const out2 = join(dir, 'no-token');
  assert.equal(await main({ ...base, GH_TOKEN: '', GITHUB_EVENT_NAME: 'push', GITHUB_REF: 'refs/heads/main', GITHUB_OUTPUT: out2 }, { fetchImpl: gh.fetchImpl, log: () => {} }), false);
  assert.equal(readFileSync(out2, 'utf8'), 'skip=false\n');
});
