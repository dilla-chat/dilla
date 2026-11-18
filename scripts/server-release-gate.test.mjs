import { test } from 'node:test';
import assert from 'node:assert/strict';
import { run, BOUND_MS, SKIPPED_JOB } from './server-release-gate.mjs';
import { ISSUE_TITLE, ISSUE_LABEL } from './ci-nightly-issue.mjs';

const SHA = 'a'.repeat(40), OTHER = 'b'.repeat(40), TWIN = 'c'.repeat(40);
const TREE = 'e'.repeat(40), OTHER_TREE = 'f'.repeat(40);
const TAG = 'desktop-gpui-v0.2.0-alpha.7';
const envFor = (over) => ({ GH_TOKEN: 't', GITHUB_REPOSITORY: 'o/r', ...over });
const desktopEnv = (over) => envFor({ RELEASE_TAG: TAG, ...over });
const serverRun = (over = {}) => ({
  id: 77, run_number: 5, name: 'server', path: '.github/workflows/server.yml', event: 'push', head_branch: 'main', head_sha: SHA,
  display_title: 'server', created_at: '2026-11-18T00:00:00Z',
  status: 'completed', conclusion: 'success', repository: { full_name: 'o/r' }, head_repository: { full_name: 'o/r' }, ...over,
});
const gpuiRun = (over = {}) => serverRun({ name: 'desktop-gpui', path: '.github/workflows/desktop-gpui.yml', display_title: 'desktop-gpui [nightly]', event: 'schedule', ...over });
const inProgress = (r) => ({ ...r, status: 'in_progress', conclusion: null });
const queued = serverRun({ status: 'queued', conclusion: null });

/**
 * A fake GitHub. `state.runs` are all runs of every workflow; a workflow listing answers like the API (by file, and the
 * query's event and head_sha), `state.trees` maps a commit to its tree, `state.jobs` a run id to its job names,
 * `state.issues` are the open issues. A dispatch is recorded and handed to `state.onDispatch`. `state.status` overrides
 * a path's answer with an HTTP status.
 */
function fakeGithub(over = {}) {
  const state = { runs: [], trees: { [SHA]: TREE, [OTHER]: OTHER_TREE, [TWIN]: TREE }, jobs: {}, issues: [], dispatches: [], seen: [], status: {}, onDispatch: () => {}, ...over };
  state.fetchImpl = async (url, init = {}) => {
    const key = `${url.pathname}${url.search}`;
    const method = init.method ?? 'GET';
    state.seen.push(`${method} ${key}`);
    if (Object.hasOwn(state.status, url.pathname)) return new Response('', { status: state.status[url.pathname] });
    const p = url.pathname, q = url.searchParams;
    let m;
    if (method === 'POST' && (m = /^\/repos\/o\/r\/actions\/workflows\/([\w.-]+)\/dispatches$/.exec(p))) {
      const body = JSON.parse(init.body);
      state.dispatches.push({ file: m[1], body });
      state.onDispatch(state, body);
      return new Response(null, { status: 204 });
    }
    if (method !== 'GET') return new Response('', { status: 405 });
    if ((m = /^\/repos\/o\/r\/actions\/workflows\/([\w.-]+)\/runs$/.exec(p))) {
      const runs = state.runs.filter((r) => r.path === `.github/workflows/${m[1]}`
        && (!q.get('event') || r.event === q.get('event')) && (!q.get('head_sha') || r.head_sha === q.get('head_sha')));
      return Response.json({ total_count: runs.length, workflow_runs: runs.map((r) => ({ ...r })) });
    }
    if ((m = /^\/repos\/o\/r\/actions\/runs\/(\d+)\/jobs$/.exec(p))) {
      const names = state.jobs[m[1]] ?? ['same-tree: ran', 'build'];
      return Response.json({ total_count: names.length, jobs: names.map((name, i) => ({ id: i + 1, name })) });
    }
    if ((m = /^\/repos\/o\/r\/actions\/runs\/(\d+)$/.exec(p))) {
      const found = state.runs.find((r) => String(r.id) === m[1]);
      return found ? Response.json({ ...found }) : new Response('', { status: 404 });
    }
    if ((m = /^\/repos\/o\/r\/git\/commits\/([a-f0-9]{40})$/.exec(p))) {
      return Object.hasOwn(state.trees, m[1]) ? Response.json({ sha: m[1], tree: { sha: state.trees[m[1]] } }) : new Response('', { status: 404 });
    }
    if (p === '/repos/o/r/issues') return Response.json(state.issues.filter((i) => i.labels.some((l) => l.name === q.get('labels'))));
    return new Response('', { status: 404 });
  };
  return state;
}

// A fake clock: sleeping advances it at once (and runs `onSleep`), so polls and the bound cost no real time.
const clocked = (gh, { onSleep = () => {}, lines = [] } = {}) => {
  const clock = { now: Date.parse('2026-11-18T12:00:00Z'), sleeps: [] };
  const options = {
    fetchImpl: gh.fetchImpl, now: () => clock.now, log: (l) => lines.push(l),
    sleep: async (ms) => { clock.sleeps.push(ms); clock.now += ms; onSleep(clock); },
  };
  return { clock, options, lines };
};
const desktop = (options) => ({ ...options, workflow: 'desktop-gpui' });
const iso = (ms) => new Date(ms).toISOString();

// ---------------------------------------------------------------- the server gate

test('theServerGateAcceptsOnlyFullMainOrDispatchRuns', async () => {
  // A push run on main (this commit, or another with the same tree) and a dispatch run are accepted.
  for (const run0 of [serverRun(), serverRun({ head_sha: TWIN }), serverRun({ event: 'workflow_dispatch', head_sha: TWIN })]) {
    const gh = fakeGithub({ runs: [run0] });
    const { options } = clocked(gh);
    const found = await run(envFor({}), SHA, options);
    assert.equal(found.id, 77, JSON.stringify(run0));
    assert.ok(gh.seen.every((s) => !s.startsWith('POST')), 'the server gate never dispatches');
  }
  // A green pull_request run on the tree, a push on another branch and a schedule run are not.
  for (const run0 of [serverRun({ head_sha: TWIN, event: 'pull_request', head_branch: 'feature' }), serverRun({ event: 'pull_request', head_branch: 'feature' }), serverRun({ head_branch: 'feature' }), serverRun({ event: 'schedule' })]) {
    const { options } = clocked(fakeGithub({ runs: [run0] }));
    await assert.rejects(() => run(envFor({}), SHA, options), /There is no green server run/, JSON.stringify(run0));
  }
  // Beside a green PR run, the main push is the one taken, even when the PR run is newer.
  const gh = fakeGithub({ runs: [serverRun({ id: 90, run_number: 9, event: 'pull_request', head_branch: 'feature' }), serverRun()] });
  assert.equal((await run(envFor({}), SHA, clocked(gh).options)).id, 77);
});

test('aGreenPrRunAloneIsNeverAcceptedTheGateWaitsForTheMainPush', async () => {
  // Only a green PR run on the tree: refused at once, never accepted.
  const lone = clocked(fakeGithub({ runs: [serverRun({ id: 90, event: 'pull_request', head_branch: 'feature' })] }));
  await assert.rejects(() => run(envFor({}), SHA, lone.options), /There is no green server run/);
  assert.deepEqual(lone.clock.sleeps, []);
  // A green PR run beside the main push still in progress: it waits for the push, and takes it once green.
  const pending = serverRun({ status: 'in_progress', conclusion: null });
  const gh = fakeGithub({ runs: [serverRun({ id: 90, run_number: 9, event: 'pull_request', head_branch: 'feature' }), pending] });
  const { options, clock } = clocked(gh, { onSleep: () => Object.assign(pending, { status: 'completed', conclusion: 'success' }) });
  assert.equal((await run(envFor({}), SHA, options)).id, 77);
  assert.deepEqual(clock.sleeps, [30000]);
  // A red PR run never refuses either: the main push decides.
  const red = clocked(fakeGithub({ runs: [serverRun({ id: 91, run_number: 9, event: 'pull_request', head_branch: 'feature', conclusion: 'failure' }), serverRun()] }));
  assert.equal((await run(envFor({}), SHA, red.options)).id, 77);
});

test('the server gate names the newest green run on the tree by run_number', async () => {
  const gh = fakeGithub({ runs: [serverRun({ id: 70, run_number: 4 }), serverRun(), serverRun({ id: 60, run_number: 3, head_sha: TWIN })] });
  const { options } = clocked(gh);
  assert.equal((await run(envFor({}), SHA, options)).id, 77);
});

test('the tree lookup is cached per commit within one gate run', async () => {
  const gh = fakeGithub({ runs: [serverRun({ id: 70, run_number: 4, head_sha: TWIN }), serverRun({ id: 71, run_number: 6, head_sha: TWIN, conclusion: null, status: 'queued' })] });
  const { options } = clocked(gh);
  await run(envFor({}), SHA, options);
  const lookups = gh.seen.filter((s) => s.includes('/git/commits/'));
  assert.deepEqual(lookups.toSorted(), [`GET /repos/o/r/git/commits/${SHA}`, `GET /repos/o/r/git/commits/${TWIN}`]);
});

test('aDifferentTreeNeverCounts', async () => {
  const gh = fakeGithub({ runs: [serverRun({ head_sha: OTHER })] });
  await assert.rejects(run(envFor({}), SHA, clocked(gh).options), /no green server run on the tree/);
  // Nor for desktop-gpui: a green nightly on another tree is not taken; the gate dispatches instead.
  const dgh = fakeGithub({ runs: [gpuiRun({ head_sha: OTHER })] });
  const { options } = clocked(dgh);
  await assert.rejects(run(desktopEnv({}), SHA, desktop(options)));
  assert.equal(dgh.dispatches.length, 1);
});

test('aSkippedRunNeverCounts', async () => {
  // A main push covered by a PR run concludes success with a job named `same-tree: skipped`; it proves nothing.
  const skipped = serverRun({ id: 90, run_number: 9 });
  const gh = fakeGithub({ runs: [skipped], jobs: { 90: [SKIPPED_JOB] } });
  await assert.rejects(run(envFor({}), SHA, clocked(gh).options), /no green server run on the tree/);
  assert.ok(gh.seen.includes('GET /repos/o/r/actions/runs/90/jobs?filter=all&per_page=100&page=1'), gh.seen.join('\n'));
  // Beside a real green run, the skipped one is passed over.
  const both = fakeGithub({ runs: [skipped, serverRun({ id: 80, run_number: 8 })], jobs: { 90: ['same-tree: skipped'] } });
  assert.equal((await run(envFor({}), SHA, clocked(both).options)).id, 80);
  // desktop-gpui: even a [nightly]-titled green run with the job never counts.
  const dgh = fakeGithub({ runs: [gpuiRun({ id: 91 })], jobs: { 91: ['build', SKIPPED_JOB] } });
  const { options } = clocked(dgh);
  await assert.rejects(run(desktopEnv({}), SHA, desktop(options)));
  assert.equal(dgh.dispatches.length, 1);
});

test('a skipped job on a later page of the jobs still counts', async () => {
  const names = [...Array.from({ length: 150 }, (_, i) => `job ${i}`), SKIPPED_JOB];
  const gh = fakeGithub({ runs: [serverRun()] });
  const base = gh.fetchImpl;
  gh.fetchImpl = async (url, init) => {
    if (url.pathname === '/repos/o/r/actions/runs/77/jobs') {
      const page = Number(url.searchParams.get('page'));
      gh.seen.push(`GET ${url.pathname}${url.search}`);
      return Response.json({ total_count: names.length, jobs: names.slice((page - 1) * 100, page * 100).map((name) => ({ name })) });
    }
    return base(url, init);
  };
  await assert.rejects(run(envFor({}), SHA, clocked(gh).options), /no green server run on the tree/);
});

test('the server gate refuses a tree without a green server run', async () => {
  await assert.rejects(run(envFor({}), SHA, clocked(fakeGithub()).options), /no green server run on the tree/);
  for (const bad of [{ conclusion: 'failure' }, { conclusion: 'cancelled' }]) {
    await assert.rejects(run(envFor({}), SHA, clocked(fakeGithub({ runs: [serverRun(bad)] })).options), /no green server run on the tree/, JSON.stringify(bad));
  }
});

test('a green desktop run does not satisfy the server gate', async () => {
  const desktopRun = serverRun({ id: 88, run_number: 9, name: 'desktop', path: '.github/workflows/server.yml' });
  // Listed under the server workflow's own path (whatever the API returned) it is still not a server run.
  await assert.rejects(run(envFor({}), SHA, clocked(fakeGithub({ runs: [desktopRun] })).options), /no green server run/);
  const gh = fakeGithub({ runs: [serverRun({ path: '.github/workflows/desktop-ci.yml', name: 'desktop' })] });
  await assert.rejects(run(envFor({}), SHA, clocked(gh).options));
  assert.ok(gh.seen.every((key) => !key.includes('desktop-ci.yml')), gh.seen.join('\n'));
});

test('a run of another workflow path or a fork head repository is refused', async () => {
  for (const bad of [{ path: '.github/workflows/other.yml' }, { name: 'server2' }, { head_repository: { full_name: 'fork/r' } }, { repository: { full_name: 'fork/r' } }, { head_repository: null }]) {
    const gh = fakeGithub({ runs: [serverRun(bad)] });
    // The fake lists by path; a run under another path is returned under server.yml's listing to prove the gate re-checks.
    const base = gh.fetchImpl;
    gh.fetchImpl = async (url, init) => (url.pathname.endsWith('/server.yml/runs') ? Response.json({ workflow_runs: gh.runs }) : base(url, init));
    await assert.rejects(run(envFor({}), SHA, clocked(gh).options), /no green server run/, JSON.stringify(bad));
  }
});

test('the SHA and the environment are validated before any request', async () => {
  const fetchImpl = async () => assert.fail('must not call the API');
  const opts = { fetchImpl, sleep: async () => {}, log: () => {} };
  await assert.rejects(run(envFor({}), 'not-a-sha', opts), /sha/i);
  await assert.rejects(run(envFor({}), SHA.toUpperCase(), opts), /sha/i);
  await assert.rejects(run(envFor({ GH_TOKEN: '' }), SHA, opts), /GH_TOKEN/);
  await assert.rejects(run(envFor({ GITHUB_REPOSITORY: 'no-slash' }), SHA, opts), /GITHUB_REPOSITORY/);
  await assert.rejects(run(desktopEnv({ RELEASE_TAG: 'a tag; rm -rf' }), SHA, { ...opts, workflow: 'desktop-gpui' }), /RELEASE_TAG/);
});

test('a 5xx on a GET is retried, a 4xx is not, and redirects are refused', async () => {
  const gh = fakeGithub({ runs: [serverRun()] });
  let calls = 0;
  const redirects = [];
  const flaky = async (url, init) => {
    calls++; redirects.push(init.redirect);
    return calls === 1 ? new Response('', { status: 502 }) : gh.fetchImpl(url, init);
  };
  assert.equal((await run(envFor({}), SHA, { ...clocked(gh).options, fetchImpl: flaky })).id, 77);
  assert.ok(redirects.every((r) => r === 'error'));
  calls = 0;
  await assert.rejects(run(envFor({}), SHA, { ...clocked(gh).options, fetchImpl: async () => { calls++; return new Response('', { status: 403 }); } }), /403/);
  assert.equal(calls, 1);
});

test('the token never reaches the log or an error', async () => {
  const lines = [];
  await assert.rejects(
    run(envFor({ GH_TOKEN: 'secret-token' }), SHA, { fetchImpl: async () => new Response('', { status: 403 }), sleep: async () => {}, log: (l) => lines.push(l) }),
    (e) => !e.message.includes('secret-token'),
  );
  assert.ok(lines.every((l) => !l.includes('secret-token')));
});

test('an in-flight server run that later succeeds is waited for, polling every 30 s', async () => {
  const gh = fakeGithub({ runs: [queued] });
  let polls = 0;
  const { clock, options, lines } = clocked(gh, { onSleep: () => {
    polls++;
    gh.runs = [polls < 3 ? inProgress(serverRun()) : serverRun()];
  } });
  const found = await run(envFor({}), SHA, options);
  assert.equal(found.id, 77);
  assert.deepEqual(clock.sleeps, [30000, 30000, 30000]);
  assert.ok(lines.some((l) => /in_progress/.test(l) && /waiting/.test(l)), lines.join('\n'));
});

test('an in-flight server run that later fails is refused at once when it completes', async () => {
  const gh = fakeGithub({ runs: [inProgress(serverRun())] });
  const { clock, options } = clocked(gh, { onSleep: () => { gh.runs = [serverRun({ conclusion: 'failure' })]; } });
  await assert.rejects(run(envFor({}), SHA, options), /concluded failure/);
  assert.deepEqual(clock.sleeps, [30000]);
});

test('a completed non-green server run on the tree is refused at once, even beside an in-flight one', async () => {
  for (const conclusion of ['failure', 'cancelled', 'timed_out']) {
    const gh = fakeGithub({ runs: [inProgress(serverRun()), serverRun({ id: 70, run_number: 4, conclusion, head_sha: TWIN })] });
    const { clock, options } = clocked(gh);
    await assert.rejects(run(envFor({}), SHA, options), new RegExp(`concluded ${conclusion}`), conclusion);
    assert.deepEqual(clock.sleeps, [], conclusion);
  }
});

test('theWaitIsBoundedAt150Minutes', async () => {
  assert.equal(BOUND_MS, 150 * 60 * 1000);
  const gh = fakeGithub({ runs: [inProgress(serverRun())] });
  const { clock, options } = clocked(gh);
  const started = clock.now;
  await assert.rejects(run(envFor({}), SHA, options), /still in_progress after 150 min/);
  assert.ok(clock.now - started <= BOUND_MS, `waited ${clock.now - started} ms`);
  assert.equal(clock.sleeps.length, 300);
  assert.ok(clock.sleeps.every((ms) => ms === 30000));
  // The desktop gate's dispatched run is held to the same bound.
  const dgh = fakeGithub();
  const d = clocked(dgh);
  dgh.onDispatch = (s) => s.runs.push(inProgress(releaseRun(d.clock)));
  const dstart = d.clock.now;
  await assert.rejects(run(desktopEnv({}), SHA, desktop(d.options)), /still in_progress after 150 min/);
  assert.ok(d.clock.now - dstart <= BOUND_MS);
});

test('the poll interval and the bound are injectable', async () => {
  const gh = fakeGithub({ runs: [inProgress(serverRun())] });
  const { clock, options } = clocked(gh);
  await assert.rejects(run(envFor({}), SHA, { ...options, pollMs: 1000, boundMs: 5000 }), /still in_progress after/);
  assert.deepEqual(clock.sleeps, [1000, 1000, 1000, 1000, 1000]);
});

test('no server run at all on the tree is refused at once, without waiting', async () => {
  const { clock, options } = clocked(fakeGithub());
  await assert.rejects(run(envFor({}), SHA, options), /no green server run on the tree/);
  assert.deepEqual(clock.sleeps, []);
});

test('an in-flight run that is not a server run on the tree is not waited for', async () => {
  for (const bad of [{ head_sha: OTHER }, { name: 'desktop' }, { head_repository: { full_name: 'fork/r' } }]) {
    const gh = fakeGithub({ runs: [inProgress(serverRun(bad))] });
    const { clock, options } = clocked(gh);
    await assert.rejects(run(envFor({}), SHA, options), /no green server run on the tree/, JSON.stringify(bad));
    assert.deepEqual(clock.sleeps, [], JSON.stringify(bad));
  }
});

test('an unknown gated workflow is refused before any request', async () => {
  const gh = fakeGithub();
  await assert.rejects(run(envFor({}), SHA, { ...clocked(gh).options, workflow: 'mobile' }), /Unknown gated workflow/);
  assert.deepEqual(gh.seen, []);
});

// ---------------------------------------------------------------- the desktop-gpui gate

// A dispatched release run, as GitHub creates it: workflow_dispatch on the tag, titled [release], created now.
const releaseRun = (clock, over = {}) => gpuiRun({ id: 500, run_number: 50, event: 'workflow_dispatch', head_branch: TAG, display_title: 'desktop-gpui [release]', created_at: iso(clock.now), ...over });

test('theGateAcceptsANightlyRunOnTheSameTree', async () => {
  // Last night's schedule run on main, at another commit with the same tree.
  const gh = fakeGithub({ runs: [gpuiRun({ head_sha: TWIN })] });
  const { options } = clocked(gh);
  const found = await run(desktopEnv({}), SHA, desktop(options));
  assert.equal(found.id, 77);
  assert.deepEqual(gh.dispatches, []);
  // A manual nightly or an earlier release run on the tree counts too.
  for (const over of [{ event: 'workflow_dispatch' }, { event: 'workflow_dispatch', display_title: 'desktop-gpui [release]', head_branch: TAG }]) {
    const g = fakeGithub({ runs: [gpuiRun(over)] });
    assert.equal((await run(desktopEnv({}), SHA, desktop(clocked(g).options))).id, 77, JSON.stringify(over));
    assert.deepEqual(g.dispatches, []);
  }
});

test('theGateNeverAcceptsAPrTierRun', async () => {
  // A green [pr] run on exactly the tagged commit (a main push) is not enough: the gate dispatches a release-tier run.
  const gh = fakeGithub({ runs: [gpuiRun({ display_title: 'desktop-gpui [pr]', event: 'push' }), gpuiRun({ id: 78, run_number: 6, display_title: 'desktop-gpui [pr]', event: 'pull_request', head_sha: TWIN })] });
  const { clock, options } = clocked(gh);
  gh.onDispatch = (s) => s.runs.push(releaseRun(clock));
  const found = await run(desktopEnv({}), SHA, desktop(options));
  assert.equal(found.id, 500);
  assert.equal(gh.dispatches.length, 1);
  // A [pr] or untitled run that a forged title cannot pass for nightly: the title must be exactly the tier's.
  for (const display_title of ['desktop-gpui [pr]', 'desktop-gpui [skipped]', 'my [nightly]', 'desktop-gpui [nightly] ', undefined]) {
    const g = fakeGithub({ runs: [gpuiRun({ display_title })] });
    const c = clocked(g);
    g.onDispatch = (s) => s.runs.push(releaseRun(c.clock));
    assert.equal((await run(desktopEnv({}), SHA, desktop(c.options))).id, 500, String(display_title));
    assert.equal(g.dispatches.length, 1, String(display_title));
  }
  // A [nightly] title on a push or pull_request event is not a nightly.
  const forged = fakeGithub({ runs: [gpuiRun({ event: 'push' }), gpuiRun({ id: 79, run_number: 7, event: 'pull_request' })] });
  const f = clocked(forged);
  forged.onDispatch = (s) => s.runs.push(releaseRun(f.clock));
  assert.equal((await run(desktopEnv({}), SHA, desktop(f.options))).id, 500);
});

test('anOpenNightlyIssueBlocksTheGate', async () => {
  const issue = { number: 3, title: ISSUE_TITLE, labels: [{ name: ISSUE_LABEL }] };
  const gh = fakeGithub({ runs: [gpuiRun()], issues: [issue] });
  await assert.rejects(run(desktopEnv({}), SHA, desktop(clocked(gh).options)), /issue #3 "nightly desktop-gpui red" \(label ci-nightly\) is open/);
  assert.deepEqual(gh.dispatches, []);
  assert.ok(gh.seen.includes(`GET /repos/o/r/issues?labels=${ISSUE_LABEL}&state=open&per_page=100`), gh.seen.join('\n'));
  // A pull request carrying the label is not the issue.
  const pr = fakeGithub({ runs: [gpuiRun()], issues: [{ ...issue, pull_request: {} }] });
  assert.equal((await run(desktopEnv({}), SHA, desktop(clocked(pr).options))).id, 77);
  // An issue that opens while the gate waits for its release run still blocks it.
  const late = fakeGithub();
  const c = clocked(late, { onSleep: () => {
    const r = late.runs.find((x) => x.id === 500);
    if (r) { Object.assign(r, { status: 'completed', conclusion: 'success' }); late.issues = [issue]; }
  } });
  late.onDispatch = (s) => s.runs.push(inProgress(releaseRun(c.clock)));
  await assert.rejects(run(desktopEnv({}), SHA, desktop(c.options)), /nightly desktop-gpui red/);
  // The issues listing failing is a refusal, never a pass.
  const broken = fakeGithub({ runs: [gpuiRun()], status: { '/repos/o/r/issues': 403 } });
  await assert.rejects(run(desktopEnv({}), SHA, desktop(clocked(broken).options)), /403/);
});

test('theGateDispatchesAReleaseRunAndWaitsForIt', async () => {
  // An older release dispatch run on the same commit (red, from an hour ago) first shows up in the listing only after
  // the dispatch, while the new run has not appeared yet: its creation time rules it out.
  const gh = fakeGithub();
  let polls = 0;
  const { clock, options } = clocked(gh, { onSleep: () => {
    polls++;
    const r = gh.runs.find((x) => x.id === 500);
    if (polls === 2) gh.runs.push(inProgress(releaseRun(clock)));
    if (r && polls === 5) Object.assign(r, { status: 'completed', conclusion: 'success' });
  } });
  gh.onDispatch = (s) => s.runs.push(releaseRun(clock, { id: 400, run_number: 40, conclusion: 'failure', created_at: iso(clock.now - 3600000) }));
  // GitHub creates the run a little after the 204; the gate polls the listing until it appears.
  const found = await run(desktopEnv({}), SHA, desktop(options));
  assert.equal(found.id, 500);
  assert.deepEqual(gh.dispatches, [{ file: 'desktop-gpui.yml', body: { ref: TAG, inputs: { tier: 'release' } } }]);
  assert.ok(gh.seen.includes('GET /repos/o/r/actions/runs/500'), 'polls the run it dispatched');
  assert.ok(!gh.seen.includes('GET /repos/o/r/actions/runs/400'), 'never polls the older run');
});

test('an older dispatch run that is green is not mistaken for the new one', async () => {
  // A green [release] run on the commit that cannot pass (it carries the skipped job) must not be picked up as the
  // dispatched run either: the gate follows only a run that was not listed before its dispatch. It was created a second
  // before the dispatch (within the clock-skew allowance), and the new run appears only after the first poll.
  const gh = fakeGithub({ jobs: { 400: [SKIPPED_JOB] } });
  const { clock, options } = clocked(gh, { onSleep: () => { if (!gh.runs.some((r) => r.id === 500)) gh.runs.push(inProgress(releaseRun(clock))); } });
  gh.runs.push(releaseRun(clock, { id: 400, run_number: 40, created_at: iso(clock.now - 1000) }));
  await assert.rejects(run(desktopEnv({}), SHA, desktop({ ...options, boundMs: 120000 })), /run 500 .*still in_progress/);
  assert.ok(!gh.seen.includes('GET /repos/o/r/actions/runs/400'));
  assert.ok(gh.seen.includes('GET /repos/o/r/actions/runs/500'));
});

test('aRedDispatchedRunRefuses', async () => {
  for (const conclusion of ['failure', 'cancelled', 'timed_out']) {
    const gh = fakeGithub();
    const { clock, options } = clocked(gh);
    gh.onDispatch = (s) => s.runs.push(releaseRun(clock, { conclusion }));
    await assert.rejects(run(desktopEnv({}), SHA, desktop(options)), new RegExp(`run 500 .*concluded ${conclusion}`), conclusion);
    assert.equal(gh.dispatches.length, 1, conclusion);
  }
  // A dispatched run that goes green but carries the skipped job refuses too.
  const gh = fakeGithub({ jobs: { 500: [SKIPPED_JOB] } });
  const { clock, options } = clocked(gh);
  gh.onDispatch = (s) => s.runs.push(releaseRun(clock));
  await assert.rejects(run(desktopEnv({}), SHA, desktop(options)), /same-tree: skipped/);
});

test('a dispatched run that never appears is refused within the bound', async () => {
  const gh = fakeGithub();
  const { clock, options } = clocked(gh);
  const started = clock.now;
  await assert.rejects(run(desktopEnv({}), SHA, desktop(options)), /did not appear/);
  assert.ok(clock.now - started <= BOUND_MS);
  assert.equal(gh.dispatches.length, 1);
});

test('a failed dispatch refuses and is not retried', async () => {
  const gh = fakeGithub({ status: { '/repos/o/r/actions/workflows/desktop-gpui.yml/dispatches': 422 } });
  await assert.rejects(run(desktopEnv({}), SHA, desktop(clocked(gh).options)), /422/);
  assert.equal(gh.seen.filter((s) => s.startsWith('POST')).length, 1);
});

test('without a release tag the desktop gate refuses instead of dispatching', async () => {
  const gh = fakeGithub();
  await assert.rejects(run(envFor({}), SHA, desktop(clocked(gh).options)), /no green desktop-gpui \[nightly\] or \[release\] run on the tree/);
  assert.ok(gh.seen.every((s) => !s.startsWith('POST')));
});

test('an in-progress nightly or release run on the tree is waited for, not dispatched over', async () => {
  const gh = fakeGithub({ runs: [inProgress(gpuiRun({ head_sha: TWIN }))] });
  let polls = 0;
  const { options } = clocked(gh, { onSleep: () => { if (++polls === 3) Object.assign(gh.runs[0], { status: 'completed', conclusion: 'success' }); } });
  assert.equal((await run(desktopEnv({}), SHA, desktop(options))).id, 77);
  assert.deepEqual(gh.dispatches, []);
  // If it goes red, the gate dispatches its own release run.
  const red = fakeGithub({ runs: [inProgress(gpuiRun())] });
  const c = clocked(red, { onSleep: () => { const r = red.runs.find((x) => x.id === 77); if (r.status !== 'completed') Object.assign(r, { status: 'completed', conclusion: 'failure' }); } });
  red.onDispatch = (s) => s.runs.push(releaseRun(c.clock));
  assert.equal((await run(desktopEnv({}), SHA, desktop(c.options))).id, 500);
  assert.equal(red.dispatches.length, 1);
});

test('a green server run does not satisfy the desktop-gpui gate, nor a desktop-gpui run the server gate', async () => {
  const gh = fakeGithub({ runs: [serverRun({ display_title: 'desktop-gpui [nightly]', event: 'schedule' })] });
  const { clock, options } = clocked(gh);
  gh.onDispatch = (s) => s.runs.push(releaseRun(clock));
  assert.equal((await run(desktopEnv({}), SHA, desktop(options))).id, 500);
  assert.equal(gh.dispatches.length, 1);
  await assert.rejects(run(envFor({}), SHA, clocked(fakeGithub({ runs: [gpuiRun()] })).options), /no green server run/);
});
