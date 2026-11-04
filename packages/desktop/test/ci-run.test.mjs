import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
 REPOSITORY, EXPECTED_ARTIFACTS, createGithubApi, findGreenPushRun, fetchCiJobs, resolveTagCommit, listRunArtifacts, selectReleaseArtifacts, hardenedGit,
} from '../scripts/ci-run.mjs';
import { execFileSync } from 'node:child_process';
import { chmodSync, existsSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

test('hardenedGit ignores hooks and fsmonitor planted in the checkout', t => {
 const root = mkdtempSync(join(tmpdir(), 'dilla-hardened-git-'));
 try {
  const marker = join(root, 'ran'), script = join(root, 'planted.sh');
  writeFileSync(script, `#!/bin/sh\ntouch '${marker}'\nexit 0\n`); chmodSync(script, 0o755);
  const plain = args => execFileSync('git', args, { cwd: root, encoding: 'utf8', env: { ...process.env, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null' } });
  plain(['init', '-q']); writeFileSync(join(root, 'a'), 'a'); plain(['add', 'a']); plain(['-c', 'user.name=t', '-c', 'user.email=t@t', 'commit', '-qm', 'a']);
  plain(['config', 'core.fsmonitor', script]); plain(['config', 'core.hooksPath', root]);
  writeFileSync(join(root, 'post-checkout'), `#!/bin/sh\ntouch '${marker}'\n`); chmodSync(join(root, 'post-checkout'), 0o755);
  const git = hardenedGit(root);
  git(['status', '--porcelain']); git(['checkout', '-q', '-b', 'other']);
  assert.ok(!existsSync(marker), 'neither the planted fsmonitor nor the planted hook ran');
  // The control needs the VCS to exec a '#!/bin/sh' script named by core.fsmonitor; on Windows that chmod-ed script is not run from a drive path, so the control proves nothing there. The hardening assertion above runs everywhere.
  if (process.platform === 'win32') t.diagnostic('control skipped on win32: plain git does not run the planted fsmonitor script there');
  else {
   plain(['status', '--porcelain']);
   assert.ok(existsSync(marker), 'control: plain git runs the planted fsmonitor');
  }
 } finally { rmSync(root, { recursive: true, force: true }); }
});

const SHA = 'a'.repeat(40), OTHER = 'b'.repeat(40);
const prefix = `/repos/${REPOSITORY}`;

/** An api double with the same call shape as createGithubApi's result. */
function routes(table) {
 const calls = [];
 const api = async (path, options = {}, optional404 = false) => {
  calls.push(path);
  if (!Object.hasOwn(table, path)) { if (optional404) return null; throw new Error('unexpected path ' + path); }
  const value = table[path];
  return typeof value === 'function' ? value(options) : structuredClone(value);
 };
 api.calls = calls;
 return api;
}

function greenRun(overrides = {}) {
 return { id: 77, run_number: 5, name: 'ci', path: '.github/workflows/ci.yml', event: 'push', head_branch: 'main', head_sha: SHA, status: 'completed', conclusion: 'success',
  repository: { full_name: REPOSITORY }, head_repository: { full_name: REPOSITORY }, ...overrides };
}
const runsPath = `${prefix}/actions/workflows/ci.yml/runs?head_sha=${SHA}&event=push&branch=main&status=completed&per_page=100`;

test('findGreenPushRun returns the green push run on exactly that sha', async () => {
 const api = routes({ [runsPath]: { workflow_runs: [greenRun({ id: 70, run_number: 4 }), greenRun(), greenRun({ id: 60, run_number: 3 })] } });
 const run = await findGreenPushRun(api, SHA);
 assert.equal(run.id, 77);
 assert.deepEqual(api.calls, [runsPath]);
});

test('findGreenPushRun refuses pull_request, cancelled, failure, in_progress and other-branch runs', async () => {
 for (const bad of [{ event: 'pull_request' }, { conclusion: 'cancelled' }, { conclusion: 'failure' }, { status: 'in_progress', conclusion: null }, { head_branch: 'feature' }]) {
  const api = routes({ [runsPath]: { workflow_runs: [greenRun(bad)] } });
  await assert.rejects(findGreenPushRun(api, SHA), /no green ci push run/, JSON.stringify(bad));
 }
});

test('findGreenPushRun refuses a run whose head_sha differs', async () => {
 const api = routes({ [runsPath]: { workflow_runs: [greenRun({ head_sha: OTHER })] } });
 await assert.rejects(findGreenPushRun(api, SHA), /no green ci push run/);
});

test('findGreenPushRun refuses a fork head repository', async () => {
 for (const bad of [{ head_repository: { full_name: 'fork/dilla' } }, { repository: { full_name: 'fork/dilla' } }, { head_repository: null }]) {
  const api = routes({ [runsPath]: { workflow_runs: [greenRun(bad)] } });
  await assert.rejects(findGreenPushRun(api, SHA), /no green ci push run/);
 }
});

test('findGreenPushRun refuses a workflow named ci at another path', async () => {
 for (const bad of [{ path: '.github/workflows/other.yml' }, { name: 'ci2' }]) {
  const api = routes({ [runsPath]: { workflow_runs: [greenRun(bad)] } });
  await assert.rejects(findGreenPushRun(api, SHA), /no green ci push run/);
 }
 await assert.rejects(findGreenPushRun(routes({}), 'not-a-sha'), /sha/i);
});

test('fetchCiJobs pages until a short page', async () => {
 const job = n => ({ id: n, name: `job ${n}`, conclusion: 'success', steps: [] });
 const page = (n, count, start) => [`${prefix}/actions/runs/77/jobs?filter=latest&per_page=100&page=${n}`, { total_count: 250, jobs: Array.from({ length: count }, (_, i) => job(start + i)) }];
 const api = routes(Object.fromEntries([page(1, 100, 0), page(2, 100, 100), page(3, 50, 200)]));
 const jobs = await fetchCiJobs(api, 77);
 assert.equal(jobs.length, 250);
 assert.equal(api.calls.length, 3);
 await assert.rejects(fetchCiJobs(routes({}), '77; rm'), /run id/i);
});

test('resolveTagCommit follows an annotated tag', async () => {
 const tagObject = 'c'.repeat(40);
 const api = routes({
  [`${prefix}/git/ref/tags/desktop-v1.0.0`]: { ref: 'refs/tags/desktop-v1.0.0', object: { type: 'tag', sha: tagObject } },
  [`${prefix}/git/tags/${tagObject}`]: { sha: tagObject, message: 'desktop-v9.9.9 ignore me', object: { type: 'commit', sha: SHA } },
 });
 assert.equal(await resolveTagCommit(api, 'desktop-v1.0.0'), SHA);
 const light = routes({ [`${prefix}/git/ref/tags/desktop-v1.0.0`]: { ref: 'refs/tags/desktop-v1.0.0', object: { type: 'commit', sha: SHA } } });
 assert.equal(await resolveTagCommit(light, 'desktop-v1.0.0'), SHA);
 const missing = routes({});
 await assert.rejects(resolveTagCommit(missing, 'desktop-v1.0.0'), /tag/i);
});

test('resolveTagCommit refuses a tag pointing at a tree or blob', async () => {
 for (const type of ['tree', 'blob']) {
  const direct = routes({ [`${prefix}/git/ref/tags/desktop-v1.0.0`]: { ref: 'refs/tags/desktop-v1.0.0', object: { type, sha: SHA } } });
  await assert.rejects(resolveTagCommit(direct, 'desktop-v1.0.0'), /commit/);
  const tagObject = 'c'.repeat(40);
  const annotated = routes({
   [`${prefix}/git/ref/tags/desktop-v1.0.0`]: { ref: 'refs/tags/desktop-v1.0.0', object: { type: 'tag', sha: tagObject } },
   [`${prefix}/git/tags/${tagObject}`]: { sha: tagObject, object: { type, sha: SHA } },
  });
  await assert.rejects(resolveTagCommit(annotated, 'desktop-v1.0.0'), /commit/);
 }
 // A tag of a tag is followed once only.
 const tagObject = 'c'.repeat(40);
 const nested = routes({
  [`${prefix}/git/ref/tags/desktop-v1.0.0`]: { ref: 'refs/tags/desktop-v1.0.0', object: { type: 'tag', sha: tagObject } },
  [`${prefix}/git/tags/${tagObject}`]: { sha: tagObject, object: { type: 'tag', sha: 'd'.repeat(40) } },
 });
 await assert.rejects(resolveTagCommit(nested, 'desktop-v1.0.0'), /commit/);
 const otherRef = routes({ [`${prefix}/git/ref/tags/desktop-v1.0.0`]: { ref: 'refs/tags/desktop-v1.0.00', object: { type: 'commit', sha: SHA } } });
 await assert.rejects(resolveTagCommit(otherRef, 'desktop-v1.0.0'), /tag/i);
});

function fakeFetch(handler) {
 const calls = [];
 const impl = async (url, init) => { calls.push({ url: String(url), init }); return handler(url, init); };
 impl.calls = calls;
 return impl;
}
const json = (body, status = 200) => new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });

test('createGithubApi rejects a path outside the repository', async () => {
 const fetchImpl = fakeFetch(() => json({ ok: true }));
 const api = createGithubApi({ token: 't0ken', fetchImpl });
 for (const path of ['/repos/other/dilla/releases', '/user', 'https://evil.example/repos/dilla-chat/dilla/x', '//evil.example/repos/dilla-chat/dilla/x',
  `/repos/${REPOSITORY}/../../other/repo/x`, `/repos/${REPOSITORY}x/releases`, `http://api.github.com/repos/${REPOSITORY}/releases`]) {
  await assert.rejects(api(path), /destination/i, path);
 }
 assert.equal(fetchImpl.calls.length, 0);
 assert.deepEqual(await api(`/repos/${REPOSITORY}/releases`), { ok: true });
 const [{ url, init }] = fetchImpl.calls;
 assert.equal(url, `https://api.github.com/repos/${REPOSITORY}/releases`);
 assert.equal(init.headers.Authorization, 'Bearer t0ken');
 assert.ok(init.signal instanceof AbortSignal);
 assert.throws(() => createGithubApi({ token: '' }), /token/i);
});

/** Release desktop-v0.1.0-beta.1: the gate failed on one GitHub API 502. Reads retry a transient 5xx or network error; writes never do. */
function retrying(handler) {
 const fetchImpl = fakeFetch(handler), slept = [], logged = [];
 const api = createGithubApi({ token: 's3cret-token', fetchImpl, sleep: async ms => { slept.push(ms); }, log: line => logged.push(line) });
 return { api, fetchImpl, slept, logged };
}
const queryPath = `/repos/${REPOSITORY}/actions/runs/7/jobs?filter=latest&per_page=100&page=1`;

test('createGithubApi retries a GET once GitHub answers 502, then succeeds', async () => {
 const statuses = [502];
 const r = retrying(() => statuses.length ? json({ message: 'Bad Gateway' }, statuses.shift()) : json({ jobs: [] }));
 assert.deepEqual(await r.api(queryPath), { jobs: [] });
 assert.equal(r.fetchImpl.calls.length, 2);
 assert.deepEqual(r.slept, [1000]);
 assert.equal(r.logged.length, 1);
 assert.match(r.logged[0], /502/);
 assert.match(r.logged[0], new RegExp(`/repos/${REPOSITORY}/actions/runs/7/jobs`));
 assert.ok(r.logged.every(line => !line.includes('s3cret-token') && !line.includes('?') && !line.includes('filter=latest') && !line.includes('https://')), 'the log names the path only: ' + r.logged);
 assert.ok(r.fetchImpl.calls.every(call => call.init.signal instanceof AbortSignal));
 assert.notEqual(r.fetchImpl.calls[0].init.signal, r.fetchImpl.calls[1].init.signal, 'each attempt has its own timeout');
});

test('createGithubApi gives up after three 5xx answers to a GET', async () => {
 const statuses = [500, 503, 502];
 const r = retrying(() => json({}, statuses.shift() ?? 200));
 await assert.rejects(r.api(queryPath), /GitHub API request failed \(502\)/);
 assert.equal(r.fetchImpl.calls.length, 3);
 assert.deepEqual(r.slept, [1000, 3000]);
 assert.equal(r.logged.length, 2);
});

test('createGithubApi retries a GET network error, at most three attempts', async () => {
 const once = retrying(handler => { if (once.fetchImpl.calls.length === 1) throw new TypeError('fetch failed'); return json({ ok: true }); });
 assert.deepEqual(await once.api(`/repos/${REPOSITORY}/releases`), { ok: true });
 assert.equal(once.fetchImpl.calls.length, 2);
 assert.match(once.logged[0], /network error/i);
 const always = retrying(() => { throw new TypeError('fetch failed'); });
 await assert.rejects(always.api(`/repos/${REPOSITORY}/releases`), /fetch failed/);
 assert.equal(always.fetchImpl.calls.length, 3);
 assert.deepEqual(always.slept, [1000, 3000]);
});

test('createGithubApi never retries a 4xx', async () => {
 for (const status of [400, 401, 403, 404, 422, 429]) {
  const r = retrying(() => json({}, status));
  await assert.rejects(r.api(`/repos/${REPOSITORY}/releases/tags/x`), new RegExp(`\\(${status}\\)`));
  assert.equal(r.fetchImpl.calls.length, 1, String(status));
  assert.deepEqual([r.slept, r.logged], [[], []]);
 }
 const optional = retrying(() => json({}, 404));
 assert.equal(await optional.api(`/repos/${REPOSITORY}/releases/tags/x`, {}, true), null);
 assert.equal(optional.fetchImpl.calls.length, 1);
});

test('createGithubApi never retries a write: a POST, PATCH, PUT or DELETE that hits a 5xx or a network error fails at once', async () => {
 for (const method of ['POST', 'PATCH', 'PUT', 'DELETE']) {
  const r = retrying(() => json({}, 502));
  await assert.rejects(r.api(`/repos/${REPOSITORY}/releases`, { method, body: method === 'DELETE' ? undefined : { draft: true } }), /502/, method);
  assert.equal(r.fetchImpl.calls.length, 1, method);
  assert.deepEqual([r.slept, r.logged], [[], []], method);
  const n = retrying(() => { throw new TypeError('fetch failed'); });
  await assert.rejects(n.api(`/repos/${REPOSITORY}/releases`, { method }), /fetch failed/, method);
  assert.equal(n.fetchImpl.calls.length, 1, method);
 }
});

test('createGithubApi refuses a bad destination before any attempt, without retrying', async () => {
 const r = retrying(() => json({}, 502));
 await assert.rejects(r.api('/user'), /destination/i);
 assert.deepEqual([r.fetchImpl.calls.length, r.slept.length], [0, 0]);
});

test('createGithubApi never follows redirects', async () => {
 const fetchImpl = fakeFetch(() => new Response(null, { status: 302, headers: { location: 'https://evil.example/' } }));
 const api = createGithubApi({ token: 't', fetchImpl });
 await assert.rejects(api(`/repos/${REPOSITORY}/releases`), /302/);
 assert.equal(fetchImpl.calls[0].init.redirect, 'error');
 const notFound = createGithubApi({ token: 't', fetchImpl: fakeFetch(() => json({}, 404)) });
 assert.equal(await notFound(`/repos/${REPOSITORY}/releases/tags/x`, {}, true), null);
 await assert.rejects(notFound(`/repos/${REPOSITORY}/releases/tags/x`), /404/);
 const posted = fakeFetch(() => json({}, 204));
 assert.equal(await createGithubApi({ token: 't', fetchImpl: posted })(`/repos/${REPOSITORY}/x`, { method: 'PATCH', body: { a: 1 } }), null);
 assert.equal(posted.calls[0].init.method, 'PATCH');
 assert.equal(posted.calls[0].init.body, '{"a":1}');
});

const digest = n => 'sha256:' + String(n % 10).repeat(64);
function artifactSet() { return EXPECTED_ARTIFACTS.map((name, i) => ({ id: 100 + i, name, digest: digest(i), expired: false })); }

test('selectReleaseArtifacts accepts the exact set', () => {
 assert.deepEqual([...EXPECTED_ARTIFACTS].sort(), [
  'desktop-installed-account-linux-x64', 'desktop-installed-account-mac-arm64', 'desktop-installed-account-win-x64',
  'desktop-installers-linux-packages-x64', 'desktop-installers-linux-x64', 'desktop-installers-mac-arm64', 'desktop-installers-win-x64',
  'desktop-performance-linux-x64', 'desktop-performance-mac-arm64', 'desktop-performance-win-x64',
 ]);
 const artifacts = artifactSet();
 const selected = selectReleaseArtifacts([...artifacts].reverse());
 assert.deepEqual(selected.ids, artifacts.map(a => a.id));
 assert.deepEqual(Object.keys(selected.byName).sort(), [...EXPECTED_ARTIFACTS].sort());
 assert.equal(selected.byName['desktop-installers-linux-x64'].digest, artifacts.find(a => a.name === 'desktop-installers-linux-x64').digest);
});

test('selectReleaseArtifacts refuses an extra desktop-installers-zz artifact, a duplicate name, a missing artifact and an expired artifact', () => {
 const artifacts = artifactSet();
 assert.throws(() => selectReleaseArtifacts([...artifacts, { id: 999, name: 'desktop-installers-zz', digest: digest(1), expired: false }]), /exact/i);
 assert.throws(() => selectReleaseArtifacts([...artifacts, { ...artifacts[0], id: 998 }]), /duplicate/i);
 assert.throws(() => selectReleaseArtifacts(artifacts.slice(1)), /exact/i);
 assert.throws(() => selectReleaseArtifacts(artifacts.map((a, i) => i === 2 ? { ...a, expired: true } : a)), /expired/i);
 assert.throws(() => selectReleaseArtifacts(artifacts.map((a, i) => i === 3 ? { ...a, digest: null } : a)), /digest/i);
 assert.throws(() => selectReleaseArtifacts(artifacts.map((a, i) => i === 3 ? { ...a, id: 0 } : a)), /id/i);
});

test('selectReleaseArtifacts ignores unrelated artifacts', () => {
 const artifacts = artifactSet();
 const unrelated = ['desktop-installed-upgrade-linux-x64', 'desktop-initial-sandbox-account-linux-x64', 'desktop-validation-win-x64', 'dilla-e2e-tools-linux-x64-x', 'desktop-failure-mac-arm64']
  .map((name, i) => ({ id: 500 + i, name, digest: null, expired: true }));
 assert.deepEqual(selectReleaseArtifacts([...unrelated, ...artifacts]).ids, artifacts.map(a => a.id));
});

test('listRunArtifacts pages and keeps id, name, digest and expired only', async () => {
 const artifact = n => ({ id: n + 1, name: `a${n}`, digest: digest(n), expired: false, archive_download_url: 'x', size_in_bytes: 5 });
 const page = (n, count, start) => [`${prefix}/actions/runs/77/artifacts?per_page=100&page=${n}`, { total_count: 150, artifacts: Array.from({ length: count }, (_, i) => artifact(start + i)) }];
 const api = routes(Object.fromEntries([page(1, 100, 0), page(2, 50, 100)]));
 const list = await listRunArtifacts(api, 77);
 assert.equal(list.length, 150);
 assert.deepEqual(list[0], { id: 1, name: 'a0', digest: digest(0), expired: false });
});
