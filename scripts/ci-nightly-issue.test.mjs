import assert from 'node:assert/strict';
import test from 'node:test';
import { conclusionOf, report, ISSUE_TITLE, ISSUE_LABEL } from './ci-nightly-issue.mjs';

const RUN = 'https://github.com/o/r/actions/runs/42';
const json = (body, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });

/** A fake GitHub: `issues` is what the list call returns; every call is recorded. */
function fake({ issues = [], labelStatus = 201 } = {}) {
  const calls = [];
  const fetchImpl = async (url, init = {}) => {
    const u = new URL(url);
    const method = init.method ?? 'GET';
    const body = init.body ? JSON.parse(init.body) : undefined;
    calls.push({ method, path: u.pathname, query: u.searchParams, body, auth: init.headers?.Authorization });
    if (method === 'GET' && u.pathname === '/repos/o/r/issues') return json(issues);
    if (method === 'POST' && u.pathname === '/repos/o/r/labels') return json({}, labelStatus);
    return json({}, 201);
  };
  return { calls, fetchImpl };
}
const run = (conclusion, f) => report({ conclusion, runUrl: RUN, token: 't0k', repo: 'o/r', fetchImpl: f.fetchImpl });
const open = [{ number: 7, title: ISSUE_TITLE }];

test('aRedNightlyOpensTheIssue', async () => {
  const f = fake();
  assert.equal(await run('failure', f), 'opened');
  const list = f.calls[0];
  assert.equal(list.method, 'GET');
  assert.equal(list.query.get('labels'), ISSUE_LABEL);
  assert.equal(list.query.get('state'), 'open');
  assert.equal(list.auth, 'Bearer t0k');
  const label = f.calls.find((c) => c.path === '/repos/o/r/labels');
  assert.equal(label.method, 'POST');
  assert.equal(label.body.name, 'ci-nightly');
  const create = f.calls.find((c) => c.path === '/repos/o/r/issues' && c.method === 'POST');
  assert.equal(create.body.title, 'nightly desktop-gpui red');
  assert.deepEqual(create.body.labels, ['ci-nightly']);
  assert.ok(create.body.body.includes(RUN));
  assert.ok(f.calls.indexOf(label) < f.calls.indexOf(create), 'label first');
});

test('aRedNightlyToleratesAnExistingLabel', async () => {
  const f = fake({ labelStatus: 422 });
  assert.equal(await run('cancelled', f), 'opened');
});

test('aSecondRedNightlyComments', async () => {
  const f = fake({ issues: [{ number: 3, title: 'other' }, ...open] });
  assert.equal(await run('failure', f), 'commented');
  assert.equal(f.calls.length, 2);
  assert.equal(f.calls[1].method, 'POST');
  assert.equal(f.calls[1].path, '/repos/o/r/issues/7/comments');
  assert.ok(f.calls[1].body.body.includes(RUN));
});

test('aGreenNightlyClosesIt', async () => {
  const f = fake({ issues: open });
  assert.equal(await run('success', f), 'closed');
  assert.equal(f.calls[1].path, '/repos/o/r/issues/7/comments');
  assert.ok(f.calls[1].body.body.includes(RUN));
  assert.equal(f.calls[2].method, 'PATCH');
  assert.equal(f.calls[2].path, '/repos/o/r/issues/7');
  assert.deepEqual(f.calls[2].body, { state: 'closed', state_reason: 'completed' });
});

test('aGreenNightlyWithNoIssueDoesNothing', async () => {
  const f = fake();
  assert.equal(await run('success', f), 'none');
  assert.equal(f.calls.length, 1);
  assert.equal(f.calls[0].method, 'GET');
});

test('pull requests in the issue list are ignored', async () => {
  const f = fake({ issues: [{ number: 9, title: ISSUE_TITLE, pull_request: {} }] });
  assert.equal(await run('success', f), 'none');
});

test('an API failure is an error', async () => {
  await assert.rejects(report({ conclusion: 'failure', runUrl: RUN, token: 't', repo: 'o/r', fetchImpl: async () => json({}, 403) }), /403/);
});

test('conclusionOf: any failure or cancelled is failure, success and skipped are success', () => {
  assert.equal(conclusionOf(['success', 'skipped', 'success']), 'success');
  assert.equal(conclusionOf(['success', 'failure']), 'failure');
  assert.equal(conclusionOf(['cancelled', 'success']), 'failure');
  assert.equal(conclusionOf(['skipped']), 'success');
});
