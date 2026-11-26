import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { copyFileSync, mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { run, shouldRerun, starvedJobs } from './ci-rerun-starved.mjs';

// Same shape as job() in ci-watchdog.test.py.
function job(name, { status = 'completed', conclusion = 'success', steps = [], runner_name = 'runner-1' } = {}) {
  return { id: 1, name, status, conclusion, runner_name, steps };
}
const starved = (name = 'desktop / macos') => job(name, { conclusion: 'cancelled', runner_name: null });
const envFor = (over) => ({ GH_TOKEN: 't', GITHUB_REPOSITORY: 'o/r', RUN_ID: '42', RUN_ATTEMPT: '1', RUN_CONCLUSION: 'failure', ...over });

test('a cancelled job with no runner and no steps triggers one rerun', async () => {
  const jobs = [job('node'), starved()];
  assert.deepEqual(starvedJobs(jobs).map((j) => j.name), ['desktop / macos']);
  assert.equal(shouldRerun({ attempt: 1, conclusion: 'failure', jobs }), true);

  const calls = [];
  const fetchImpl = async (url, { method }) => {
    calls.push(`${method} ${url.pathname}${url.search}`);
    return method === 'POST' ? new Response(null, { status: 201 }) : Response.json({ total_count: 2, jobs });
  };
  assert.equal(await run(envFor({}), { fetchImpl, log: () => {} }), true);
  assert.deepEqual(calls, ['GET /repos/o/r/actions/runs/42/attempts/1/jobs?per_page=100&page=1', 'POST /repos/o/r/actions/runs/42/rerun-failed-jobs']);
});

test('attempt 2 never reruns', async () => {
  assert.equal(shouldRerun({ attempt: 2, conclusion: 'failure', jobs: [starved()] }), false);
  const fetchImpl = async () => assert.fail('attempt 2 must not call the API');
  assert.equal(await run(envFor({ RUN_ATTEMPT: '2', RUN_CONCLUSION: 'failure' }), { fetchImpl, log: () => {} }), false);
});

test('a cancelled job that ran steps does not count', () => {
  const ran = job('node', { conclusion: 'cancelled', steps: [{ name: 'Set up job', status: 'completed', conclusion: 'success' }] });
  const onRunner = job('go', { conclusion: 'cancelled' });
  assert.deepEqual(starvedJobs([ran, onRunner]), []);
  assert.equal(shouldRerun({ attempt: 1, conclusion: 'failure', jobs: [ran, onRunner] }), false);
});

test('a green run does nothing', async () => {
  assert.equal(shouldRerun({ attempt: 1, conclusion: 'success', jobs: [starved()] }), false);
  const fetchImpl = async () => assert.fail('a green run must not call the API');
  assert.equal(await run(envFor({ RUN_CONCLUSION: 'success' }), { fetchImpl, log: () => {} }), false);
});

test('a failed job beside a starved one does not rerun: a real failure gets no free second try', () => {
  const jobs = [job('go', { conclusion: 'failure' }), starved()];
  assert.equal(shouldRerun({ attempt: 1, conclusion: 'failure', jobs }), false);
});

test('a cancelled run with a starved job does not rerun', async () => {
  assert.equal(shouldRerun({ attempt: 1, conclusion: 'cancelled', jobs: [starved()] }), false);
  const fetchImpl = async () => assert.fail('a cancelled run must not call the API');
  assert.equal(await run(envFor({ RUN_CONCLUSION: 'cancelled' }), { fetchImpl, log: () => {} }), false);
});

test('a starved job plus a failed aggregate reruns', () => {
  for (const name of ['server-ok', 'desktop-ok', 'ci-watchdog']) {
    assert.equal(shouldRerun({ attempt: 1, conclusion: 'failure', jobs: [starved(), job(name, { conclusion: 'failure' })] }), true, name);
  }
});

test('a starved job plus a cancelled job that started does not rerun', () => {
  const onRunner = job('go', { conclusion: 'cancelled' });
  const withSteps = job('node', { conclusion: 'cancelled', runner_name: null, steps: [{ name: 'Set up job' }] });
  assert.equal(shouldRerun({ attempt: 1, conclusion: 'failure', jobs: [starved(), onRunner] }), false);
  assert.equal(shouldRerun({ attempt: 1, conclusion: 'failure', jobs: [starved(), withSteps] }), false);
});

test('a timed_out job blocks the rerun', () => {
  assert.equal(shouldRerun({ attempt: 1, conclusion: 'failure', jobs: [starved(), job('go', { conclusion: 'timed_out' })] }), false);
});

test('predicate parity with NEVER_STARTED in ci-watchdog.py', () => {
  // A failed job with no runner and no steps is not never-started.
  assert.deepEqual(starvedJobs([job('x', { conclusion: 'failure', runner_name: null })]), []);
  // An empty runner name with steps undefined is never-started.
  const bare = { id: 1, name: 'x', status: 'completed', conclusion: 'cancelled', runner_name: '' };
  assert.deepEqual(starvedJobs([bare]), [bare]);
  assert.deepEqual(starvedJobs([{ ...bare, status: 'in_progress' }]), []);
});

test('a raw job name never reaches the log', async () => {
  const lines = [];
  const jobs = [{ ...starved('::add-mask::x'), id: 9 }];
  const fetchImpl = async (url, { method }) => (method === 'POST' ? new Response(null, { status: 201 }) : Response.json({ jobs }));
  assert.equal(await run(envFor({}), { fetchImpl, log: (l) => lines.push(l) }), true);
  assert.ok(lines.some((l) => l.includes('9')));
  assert.ok(lines.every((l) => !l.includes('::add-mask::') && !l.includes('add-mask')));
});

test('an invalid conclusion is refused', async () => {
  await assert.rejects(run(envFor({ RUN_CONCLUSION: '::x' }), { fetchImpl: async () => assert.fail('no call'), log: () => {} }), /RUN_CONCLUSION/);
});

test('a failed job alone does not trigger a rerun', () => {
  assert.equal(shouldRerun({ attempt: 1, conclusion: 'failure', jobs: [job('go', { conclusion: 'failure' })] }), false);
});

test('jobs are paged and a 5xx on the listing is retried', async () => {
  const page1 = Array.from({ length: 100 }, (_, i) => job(`j${i}`));
  const seen = [];
  let failedOnce = false;
  const fetchImpl = async (url, { method }) => {
    seen.push(`${method} ${url.search}`);
    if (method === 'POST') return new Response(null, { status: 201 });
    if (url.searchParams.get('page') === '1' && !failedOnce) { failedOnce = true; return new Response('', { status: 502 }); }
    return Response.json({ jobs: url.searchParams.get('page') === '1' ? page1 : [starved()] });
  };
  assert.equal(await run(envFor({ RUN_ID: '7' }), { fetchImpl, sleep: async () => {}, log: () => {} }), true);
  assert.equal(seen.filter((s) => s.startsWith('POST')).length, 1);
  assert.equal(seen.filter((s) => s.includes('page=2')).length, 1);
});

test('the token never reaches the log or an error', async () => {
  const lines = [];
  const fetchImpl = async () => new Response('', { status: 403 });
  await assert.rejects(
    run(envFor({ GH_TOKEN: 'secret-token', RUN_CONCLUSION: 'failure' }), { fetchImpl, log: (l) => lines.push(l) }),
    (e) => !e.message.includes('secret-token'),
  );
  assert.ok(lines.every((l) => !l.includes('secret-token')));
});

test('the script runs from a checkout whose path a file:// URL has to percent-encode', () => {
  const dir = join(mkdtempSync(join(tmpdir(), 'rerun-starved-')), 'a dir with spaces');
  mkdirSync(dir);
  copyFileSync(fileURLToPath(new URL('./ci-rerun-starved.mjs', import.meta.url)), join(dir, 'ci-rerun-starved.mjs'));
  try {
    const r = spawnSync(process.execPath, ['ci-rerun-starved.mjs'], { cwd: dir, env: { ...process.env, GH_TOKEN: '' }, encoding: 'utf8' });
    assert.notEqual(r.status, 0, 'the script did not run');
    assert.match(r.stderr, /GH_TOKEN is required/);
  } finally {
    rmSync(dirname(dir), { recursive: true, force: true });
  }
});
