import { test } from 'node:test';
import assert from 'node:assert/strict';
import { run } from './server-release-gate.mjs';

const SHA = 'a'.repeat(40), OTHER = 'b'.repeat(40);
const envFor = (over) => ({ GH_TOKEN: 't', GITHUB_REPOSITORY: 'o/r', ...over });
const runsUrl = (workflow, sha = SHA) => `/repos/o/r/actions/workflows/${workflow}/runs?head_sha=${sha}&event=push&branch=main&status=completed&per_page=100`;
const serverRun = (over = {}) => ({
  id: 77, run_number: 5, name: 'server', path: '.github/workflows/server.yml', event: 'push', head_branch: 'main', head_sha: SHA,
  status: 'completed', conclusion: 'success', repository: { full_name: 'o/r' }, head_repository: { full_name: 'o/r' }, ...over,
});
// A GitHub that answers each listing path from the table and nothing else.
const github = (table, seen = []) => async (url) => {
  const key = `${url.pathname}${url.search}`;
  seen.push(key);
  return Object.hasOwn(table, key) ? Response.json({ workflow_runs: table[key] }) : new Response('', { status: 404 });
};
const opts = (fetchImpl, lines = []) => ({ fetchImpl, sleep: async () => {}, log: (l) => lines.push(l) });

test('the server gate accepts the exact SHA\'s green run and names the newest by run_number', async () => {
  const seen = [];
  const fetchImpl = github({ [runsUrl('server.yml')]: [serverRun({ id: 70, run_number: 4 }), serverRun(), serverRun({ id: 60, run_number: 3 })] }, seen);
  const found = await run(envFor({}), SHA, opts(fetchImpl));
  assert.equal(found.id, 77);
  assert.deepEqual(seen, [runsUrl('server.yml')]);
});

test('the server gate refuses a SHA without a green server push run', async () => {
  await assert.rejects(run(envFor({}), SHA, opts(github({ [runsUrl('server.yml')]: [] }))), /no green server push run/);
  for (const bad of [{ conclusion: 'failure' }, { conclusion: 'cancelled' }, { status: 'in_progress', conclusion: null }, { head_branch: 'feature' }]) {
    await assert.rejects(run(envFor({}), SHA, opts(github({ [runsUrl('server.yml')]: [serverRun(bad)] }))), /no green server push run/, JSON.stringify(bad));
  }
});

test('a green server run on a different SHA is refused', async () => {
  const fetchImpl = github({ [runsUrl('server.yml')]: [serverRun({ head_sha: OTHER })] });
  await assert.rejects(run(envFor({}), SHA, opts(fetchImpl)), /no green server push run/);
});

test('a green server run from a pull_request event is refused', async () => {
  const fetchImpl = github({ [runsUrl('server.yml')]: [serverRun({ event: 'pull_request' })] });
  await assert.rejects(run(envFor({}), SHA, opts(fetchImpl)), /no green server push run/);
});

test('a green desktop run does not satisfy the server gate', async () => {
  const desktop = serverRun({ id: 88, run_number: 9, name: 'desktop', path: '.github/workflows/desktop-ci.yml' });
  // Listed under the server workflow's own path (whatever the API returned) it is still not a server run.
  await assert.rejects(run(envFor({}), SHA, opts(github({ [runsUrl('server.yml')]: [desktop] }))), /no green server push run/);
  // And the gate never asks the desktop workflow.
  const seen = [];
  await assert.rejects(run(envFor({}), SHA, opts(github({ [runsUrl('desktop-ci.yml')]: [desktop] }, seen))));
  assert.ok(seen.every((key) => !key.includes('desktop-ci.yml')), seen.join('\n'));
});

test('a run of another workflow path or a fork head repository is refused', async () => {
  for (const bad of [{ path: '.github/workflows/other.yml' }, { name: 'server2' }, { head_repository: { full_name: 'fork/r' } }, { repository: { full_name: 'fork/r' } }, { head_repository: null }]) {
    await assert.rejects(run(envFor({}), SHA, opts(github({ [runsUrl('server.yml')]: [serverRun(bad)] }))), /no green server push run/, JSON.stringify(bad));
  }
});

test('the SHA and the environment are validated before any request', async () => {
  const fetchImpl = async () => assert.fail('must not call the API');
  await assert.rejects(run(envFor({}), 'not-a-sha', opts(fetchImpl)), /sha/i);
  await assert.rejects(run(envFor({}), SHA.toUpperCase(), opts(fetchImpl)), /sha/i);
  await assert.rejects(run(envFor({ GH_TOKEN: '' }), SHA, opts(fetchImpl)), /GH_TOKEN/);
  await assert.rejects(run(envFor({ GITHUB_REPOSITORY: 'no-slash' }), SHA, opts(fetchImpl)), /GITHUB_REPOSITORY/);
});

test('a 5xx on the listing is retried, a 4xx is not, and redirects are refused', async () => {
  let calls = 0, redirect;
  const flaky = async (url, init) => {
    calls++; redirect = init.redirect;
    return calls === 1 ? new Response('', { status: 502 }) : Response.json({ workflow_runs: [serverRun()] });
  };
  assert.equal((await run(envFor({}), SHA, opts(flaky))).id, 77);
  assert.equal(calls, 2);
  assert.equal(redirect, 'error');
  calls = 0;
  await assert.rejects(run(envFor({}), SHA, opts(async () => { calls++; return new Response('', { status: 403 }); })), /403/);
  assert.equal(calls, 1);
});

test('the token never reaches the log or an error', async () => {
  const lines = [];
  await assert.rejects(
    run(envFor({ GH_TOKEN: 'secret-token' }), SHA, opts(async () => new Response('', { status: 403 }), lines)),
    (e) => !e.message.includes('secret-token'),
  );
  assert.ok(lines.every((l) => !l.includes('secret-token')));
});
