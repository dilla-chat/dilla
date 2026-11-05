import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { TAG, runGate, writeOutputs } from '../scripts/release-gate.mjs';
import { EXPECTED_ARTIFACTS } from '../scripts/ci-run.mjs';
import { acceptedJobs } from './support/release-fixture.mjs';

const R = '/repos/dilla-chat/dilla';
const NOW = Date.parse('2026-11-03T00:00:00Z');
const DAY = 86400000;

/** A throwaway git repository standing in for the keyed workflow's full-history checkout. */
function makeRepo() {
 const dir = mkdtempSync(join(tmpdir(), 'dilla-gate-'));
 const run = args => execFileSync('git', args, { cwd: dir, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();
 run(['init', '-q', '-b', 'main']);
 for (const [key, value] of [['user.email', 'test@example.invalid'], ['user.name', 'test'], ['commit.gpgsign', 'false'], ['tag.gpgsign', 'false']]) run(['config', key, value]);
 const commit = (files, message = 'change') => {
  for (const [path, content] of Object.entries(files)) { mkdirSync(dirname(join(dir, path)), { recursive: true }); writeFileSync(join(dir, path), content); }
  run(['add', '-A']); run(['commit', '-q', '--allow-empty', '-m', message]);
  return run(['rev-parse', 'HEAD']);
 };
 return { dir, run, commit, originMain: sha => run(['update-ref', 'refs/remotes/origin/main', sha]), checkout: sha => run(['checkout', '-q', '--detach', sha]), cleanup: () => rmSync(dir, { recursive: true, force: true }) };
}
/** Raw Ed25519 public keys standing in for Sparkle keys: OLD before a promote, NEW after it. */
const OLD_SPARKLE = Buffer.alloc(32, 1).toString('base64'), NEW_SPARKLE = Buffer.alloc(32, 2).toString('base64');
const TRUST = { metadataPublicKeys: [], sparklePublicKey: OLD_SPARKLE, signing: { active: 'A', staged: null } };
/** main after `rotate --promote`: B active, a macOS key change from slot A's Sparkle key pending. */
const PROMOTED = { metadataPublicKeys: [], sparklePublicKey: NEW_SPARKLE, signing: { active: 'B', staged: null, macRotation: { fromSlot: 'A', fromSparklePublicKey: OLD_SPARKLE } } };
const baseFiles = (version, expiresAt = '2027-01-31T00:00:00Z', trust = TRUST) => ({
 'packages/desktop/package.json': JSON.stringify({ name: '@dilla/desktop', version }) + '\n',
 'packages/desktop/release-trust.json': JSON.stringify(trust) + '\n',
 'packages/desktop/linux-sandbox-component.json': JSON.stringify({ generation: 1, expiresAt }) + '\n',
 'src/app.js': 'export const v = 1;\n', 'README.md': '# dilla\n', 'docs/guide.md': 'guide\n',
});

function ciRun(sha, overrides = {}) {
 return { id: 77, run_number: 5, name: 'desktop', path: '.github/workflows/desktop-ci.yml', event: 'push', head_branch: 'main', head_sha: sha, status: 'completed', conclusion: 'success', repository: { full_name: 'dilla-chat/dilla' }, head_repository: { full_name: 'dilla-chat/dilla' }, ...overrides };
}
/** The keyed workflow's unfinished runs, by status: only this run (5000) by default. */
function activeRuns(byStatus = { in_progress: [{ id: 5000 }] }) {
 return Object.fromEntries(['queued', 'in_progress', 'waiting', 'requested', 'pending'].map(status =>
  [`${R}/actions/workflows/desktop-release.yml/runs?status=${status}&per_page=100`, { workflow_runs: byStatus[status] ?? [] }]));
}
function requestRun(sha, tag, overrides = {}) {
 return { id: 900, name: 'desktop release request', path: '.github/workflows/desktop-release-request.yml', event: 'push', head_branch: tag, head_sha: sha, status: 'completed', conclusion: 'success', repository: { full_name: 'dilla-chat/dilla' }, head_repository: { full_name: 'dilla-chat/dilla' }, ...overrides };
}

/**
 * Builds a passing scenario; `adjust` may change the repository layout, routes and env before running.
 * layout: (repo) => { tagged, head, main } shas.
 */
function scenario({ version = '1.2.3', expiresAt, layout, trust } = {}) {
 const repo = makeRepo();
 const c1 = repo.commit(baseFiles(version, expiresAt, trust), 'release commit');
 const shas = layout ? layout(repo, c1) : { tagged: c1, head: c1, main: c1 };
 repo.originMain(shas.main); repo.checkout(shas.head);
 const tag = `desktop-v${version}`;
 const routes = {
  [`${R}/git/ref/tags/${tag}`]: { ref: `refs/tags/${tag}`, object: { type: 'commit', sha: shas.tagged } },
  [`${R}/actions/runs/900`]: requestRun(shas.tagged, tag),
  [`${R}/immutable-releases`]: { enabled: true, enforced_by_owner: false },
  [`${R}/rules/branches/main`]: [{ type: 'deletion' }, { type: 'non_fast_forward' }, { type: 'pull_request', parameters: { required_approving_review_count: 0 } }, { type: 'required_status_checks', parameters: { required_status_checks: [{ context: 'server-ok', integration_id: 15368 }, { context: 'desktop-ok', integration_id: 15368 }] } }],
  [`${R}/environments/desktop-release`]: { name: 'desktop-release', deployment_branch_policy: { protected_branches: false, custom_branch_policies: true } },
  [`${R}/environments/desktop-release/deployment-branch-policies?per_page=100`]: { total_count: 1, branch_policies: [{ id: 1, name: 'main', type: 'branch' }] },
  [`${R}/actions/workflows/desktop-ci.yml/runs?head_sha=${shas.tagged}&event=push&branch=main&status=completed&per_page=100`]: { workflow_runs: [ciRun(shas.tagged)] },
  [`${R}/actions/runs/77/jobs?filter=latest&per_page=100&page=1`]: { jobs: acceptedJobs() },
  [`${R}/actions/runs/77/artifacts?per_page=100&page=1`]: { artifacts: [...EXPECTED_ARTIFACTS.map((name, i) => ({ id: 300 + i, name, digest: 'sha256:' + 'e'.repeat(64), expired: false })), { id: 1, name: 'desktop-validation-linux-x64', digest: null, expired: false }] },
  [`${R}/releases?per_page=100&page=1`]: [{ tag_name: 'desktop-v1.2.2', draft: false }, { tag_name: 'desktop-v1.3.0-beta.1', draft: false }, { tag_name: 'desktop-beta', draft: false }],
  [`${R}/git/matching-refs/tags/desktop-v`]: [{ ref: 'refs/tags/desktop-v1.2.2' }, { ref: `refs/tags/${tag}` }, { ref: 'refs/tags/desktop-v1.3.0-beta.1' }],
  ...activeRuns(),
 };
 const calls = [];
 const api = async (path, options = {}, optional404 = false) => {
  calls.push(path);
  if (options.method && options.method !== 'GET') throw new Error('the gate must not write through the API');
  if (!Object.hasOwn(routes, path)) { if (optional404) return null; throw new Error(`GitHub API request failed (404) ${path}`); }
  const value = routes[path];
  return typeof value === 'function' ? value() : structuredClone(value);
 };
 const gitCalls = [];
 const git = args => { gitCalls.push(args); return repo.run(args); };
 const work = mkdtempSync(join(tmpdir(), 'dilla-gate-out-'));
 const outputFile = join(work, 'github-output');
 writeFileSync(outputFile, '');
 const env = { GITHUB_ACTIONS: 'true', GITHUB_EVENT_NAME: 'workflow_run', GITHUB_REPOSITORY: 'dilla-chat/dilla', GITHUB_REF: 'refs/heads/main', GITHUB_WORKFLOW_REF: 'dilla-chat/dilla/.github/workflows/desktop-release.yml@refs/heads/main', GITHUB_SHA: shas.head, HINT_TAG: tag, HINT_RUN: '900', GITHUB_RUN_ID: '5000', GITHUB_RUN_ATTEMPT: '1', GITHUB_OUTPUT: outputFile };
 const outputDirectory = join(work, 'gate');
 const go = (overrides = {}) => runGate({ env: { ...env, ...overrides }, git, api, outputDirectory, now: NOW });
 return { repo, routes, calls, gitCalls, env, go, shas, tag, version, outputFile, outputDirectory, cleanup: () => { repo.cleanup(); rmSync(work, { recursive: true, force: true }); } };
}
async function refused(s, pattern, overrides) {
 await assert.rejects(s.go(overrides), pattern);
 assert.equal(readFileSync(s.outputFile, 'utf8'), '', 'no output is written on refusal');
 assert.ok(!existsSync(join(s.outputDirectory, 'ci-run.json')), 'no gate file is written on refusal');
}

test('gate passes for the current main with a green run, matching version, exact artifacts and good posture', async () => {
 const s = scenario();
 try {
  const result = await s.go();
  const digests = EXPECTED_ARTIFACTS.map(() => 'sha256:' + 'e'.repeat(64));
  const record = name => createHash('sha256').update(readFileSync(join(s.outputDirectory, name))).digest('hex');
  assert.deepEqual(result, { tag: 'desktop-v1.2.3', version: '1.2.3', channel: 'stable', sha: s.shas.tagged, tree: s.repo.run(['rev-parse', `${s.shas.tagged}^{tree}`]), ciRun: 77, artifactIds: EXPECTED_ARTIFACTS.map((_, i) => 300 + i), artifactDigests: digests, runRecord: record('ci-run.json'), jobsRecord: record('ci-jobs.json'), previous: 'desktop-v1.2.2', macUpdate: 'zip' });
  assert.equal(readFileSync(s.outputFile, 'utf8'), `tag=desktop-v1.2.3\nversion=1.2.3\nchannel=stable\nsha=${result.sha}\ntree=${result.tree}\nrun=77\nartifact_ids=${result.artifactIds.join(',')}\nartifact_digests=${digests.join(',')}\nrun_record=${result.runRecord}\njobs_record=${result.jobsRecord}\nprevious=desktop-v1.2.2\nmac_update=zip\n`);
  assert.equal(JSON.parse(readFileSync(join(s.outputDirectory, 'ci-run.json'), 'utf8')).id, 77);
  assert.equal(JSON.parse(readFileSync(join(s.outputDirectory, 'ci-jobs.json'), 'utf8')).length, acceptedJobs().length);
  await assert.rejects(s.go(), /EEXIST/, 'gate files are created exclusively');
 } finally { s.cleanup(); }
});

test('gate refuses when not running as the main-branch workflow_run', async () => {
 const s = scenario();
 try {
  for (const overrides of [{ GITHUB_ACTIONS: 'false' }, { GITHUB_EVENT_NAME: 'push' }, { GITHUB_EVENT_NAME: 'workflow_dispatch' }, { GITHUB_REF: 'refs/tags/desktop-v1.2.3' }, { GITHUB_REF: 'refs/heads/feature' },
   { GITHUB_REPOSITORY: 'fork/dilla' }, { GITHUB_WORKFLOW_REF: 'dilla-chat/dilla/.github/workflows/desktop-release.yml@refs/heads/feature' }, { GITHUB_WORKFLOW_REF: 'dilla-chat/dilla/.github/workflows/other.yml@refs/heads/main' },
   { GITHUB_SHA: 'f'.repeat(40) }]) {
   await refused(s, /main-branch workflow_run|HEAD/, overrides);
  }
  assert.equal(s.calls.length, 0, 'no API call before the run context is proven');
 } finally { s.cleanup(); }
});

test('gate refuses a re-run attempt of a keyed run (only feed may be re-run; re-run the request run instead)', async () => {
 const s = scenario();
 try {
  for (const GITHUB_RUN_ATTEMPT of ['2', '3', '0', '01', '1 ', '', undefined]) await refused(s, /re-run|attempt/i, { GITHUB_RUN_ATTEMPT });
  assert.equal(s.calls.length, 0, 'no API call on a re-run attempt');
  assert.equal((await s.go()).sha, s.shas.tagged, 'the first attempt passes');
 } finally { s.cleanup(); }
});

test('gate fails loudly while another desktop release run is queued or in progress (no silent drop)', async () => {
 for (const status of ['queued', 'in_progress', 'waiting', 'requested', 'pending']) {
  const s = scenario();
  try {
   Object.assign(s.routes, activeRuns({ in_progress: [{ id: 5000 }], [status]: [...(status === 'in_progress' ? [{ id: 5000 }] : []), { id: 4999 }] }));
   await refused(s, /another desktop release run \(4999\)/i);
  } finally { s.cleanup(); }
 }
 const s = scenario();
 try {
  for (const overrides of [{ GITHUB_RUN_ID: '' }, { GITHUB_RUN_ID: '12a' }]) await refused(s, /GITHUB_RUN_ID/, overrides);
  await s.go(); // only this run is active
 } finally { s.cleanup(); }
});

test('gate refuses a run whose workflow commit is no longer main\'s tip', async () => {
 // The keyed run checked out an old main (HEAD); main has since gained a code change, and the tag is on the new tip.
 const s = scenario({ layout: (repo, c1) => { const c2 = repo.commit({ 'src/app.js': 'export const v = 2;\n' }, 'fix'); return { tagged: c2, head: c1, main: c2 }; } });
 try { await refused(s, /main.*(tip|HEAD)|no longer/i); } finally { s.cleanup(); }
});

test('gate allows a docs-only main advance after the keyed run started', async () => {
 const s = scenario({ layout: (repo, c1) => { const c2 = repo.commit({ 'docs/guide.md': 'new guide\n', 'packages/desktop/NOTES.md': 'x\n' }, 'docs'); return { tagged: c1, head: c1, main: c2 }; } });
 try { assert.equal((await s.go()).sha, s.shas.tagged); } finally { s.cleanup(); }
});

test('gate refuses a tagged commit with a code difference from main\'s HEAD (older commit ahead of its fix)', async () => {
 const s = scenario({ layout: (repo, c1) => { const c2 = repo.commit({ 'src/app.js': 'export const v = 2;\n' }, 'fix'); return { tagged: c1, head: c2, main: c2 }; } });
 try { await refused(s, /code difference/i); } finally { s.cleanup(); }
 // A rename of code into docs/ is still a code difference.
 const r = scenario({ layout: (repo, c1) => { repo.run(['mv', 'src/app.js', 'docs/app.js']); const c2 = repo.commit({}, 'move'); return { tagged: c1, head: c2, main: c2 }; } });
 try { await refused(r, /code difference/i); } finally { r.cleanup(); }
});

test('gate allows a tagged commit that differs from main\'s HEAD only in docs', async () => {
 const s = scenario({ layout: (repo, c1) => { const c2 = repo.commit({ 'docs/guide.md': 'v2\n', 'README.md': '# dilla 2\n', 'LICENSE': 'x\n' }, 'docs'); return { tagged: c1, head: c2, main: c2 }; } });
 try { assert.equal((await s.go()).sha, s.shas.tagged); } finally { s.cleanup(); }
});

test('gate treats a root directory named LICENSE or DCO as code, as classifyFile does', async () => {
 for (const name of ['LICENSE', 'LICENSE-APACHE', 'DCO']) {
  const s = scenario({ layout: (repo, c1) => { const c2 = repo.commit({ [`${name}/evil.js`]: 'export const x = 1;\n' }, 'code under a doc name'); return { tagged: c1, head: c2, main: c2 }; } });
  try { await refused(s, /code difference/i); } finally { s.cleanup(); }
 }
});

test('gate takes previous from published releases only, never from a bare or draft tag', async () => {
 const s = scenario();
 try {
  s.routes[`${R}/releases?per_page=100&page=1`] = [{ tag_name: 'desktop-v1.2.0', draft: false }, { tag_name: 'desktop-v1.2.2', draft: true }, { tag_name: 'desktop-v1.3.0-beta.1', draft: false }];
  s.routes[`${R}/git/matching-refs/tags/desktop-v`] = [{ ref: 'refs/tags/desktop-v1.2.0' }, { ref: 'refs/tags/desktop-v1.2.2' }, { ref: 'refs/tags/desktop-v1.2.2-beta.9' }, { ref: 'refs/tags/desktop-v1.2.3' }];
  assert.equal((await s.go()).previous, 'desktop-v1.2.0');
  assert.ok(!s.calls.some(path => path.includes('matching-refs')), 'tags are not consulted');
 } finally { s.cleanup(); }
});

test('gate validates its outputs before writing any record file', async () => {
 const s = scenario();
 try { await refused(s, /output/i, { GITHUB_OUTPUT: '' }); } finally { s.cleanup(); }
});

test('gate resolves an annotated tag through the API ignoring the tag message', async () => {
 const s = scenario();
 try {
  const tagObject = 'c'.repeat(40);
  s.routes[`${R}/git/ref/tags/${s.tag}`] = { ref: `refs/tags/${s.tag}`, object: { type: 'tag', sha: tagObject } };
  s.routes[`${R}/git/tags/${tagObject}`] = { sha: tagObject, tag: 'desktop-v9.9.9', message: `release ${'d'.repeat(40)}\nversion=9.9.9`, object: { type: 'commit', sha: s.shas.tagged } };
  // A local tag of the same name pointing elsewhere is never consulted.
  const other = s.repo.commit({ 'src/other.js': '1\n' }, 'other'); s.repo.run(['tag', s.tag, other]); s.repo.checkout(s.shas.head);
  const result = await s.go();
  assert.equal(result.sha, s.shas.tagged);
  assert.equal(result.version, '1.2.3');
 } finally { s.cleanup(); }
});

test('gate refuses a hint tag that is not a desktop version tag', async () => {
 const s = scenario();
 try {
  for (const HINT_TAG of ['desktop-v1.0.0; rm -rf', '../x', 'v1.2.3', 'desktop-v1.2', 'desktop-v01.2.3', 'desktop-v1.2.3\n', 'desktop-v1.2.3-rc.1', 'desktop-beta', '', 'main']) {
   await refused(s, /tag/i, { HINT_TAG });
  }
  assert.equal(s.calls.length, 0);
  assert.ok(!TAG.test('desktop-v1.2.3\n'));
 } finally { s.cleanup(); }
});

test('gate ignores a lying HINT_RUN', async () => {
 const s = scenario();
 try {
  // A non-numeric hint, the accepted ci run's own id, and a green request run for another tag and commit.
  await refused(s, /run/i, { HINT_RUN: '900; echo' });
  s.routes[`${R}/actions/runs/77`] = ciRun(s.shas.tagged);
  await refused(s, /triggering/i, { HINT_RUN: '77' });
  s.routes[`${R}/actions/runs/901`] = requestRun('9'.repeat(40), 'desktop-v1.2.4', { id: 901 });
  await refused(s, /triggering/i, { HINT_RUN: '901' });
  // The released ci run always comes from the API lookup, never from the hint.
  assert.equal((await s.go()).ciRun, 77);
 } finally { s.cleanup(); }
});

test('gate refuses a triggering run from another workflow path, a fork, a non-push event or a failed run', async () => {
 for (const bad of [{ path: '.github/workflows/desktop-release.yml' }, { path: '.github/workflows/other.yml' }, { head_repository: { full_name: 'fork/dilla' } }, { repository: { full_name: 'fork/dilla' } },
  { event: 'pull_request' }, { event: 'workflow_dispatch' }, { conclusion: 'failure' }, { conclusion: 'cancelled' }, { head_sha: 'f'.repeat(40) }, { head_branch: 'desktop-v1.2.4' }]) {
  const s = scenario();
  try {
   s.routes[`${R}/actions/runs/900`] = requestRun(s.shas.tagged, s.tag, bad);
   await refused(s, /triggering/i);
  } finally { s.cleanup(); }
 }
});

test('gate refuses a commit that is not an ancestor of main', async () => {
 const s = scenario({ layout: (repo, c1) => { repo.run(['checkout', '-q', '-b', 'side']); const side = repo.commit({ 'docs/side.md': 'side\n' }, 'side'); repo.run(['checkout', '-q', 'main']); return { tagged: side, head: c1, main: c1 }; } });
 try { await refused(s, /ancestor/i); } finally { s.cleanup(); }
});

test('gate refuses a version that differs from the tagged package.json', async () => {
 const s = scenario({ layout: (repo, c1) => ({ tagged: c1, head: c1, main: c1 }) });
 try {
  const before = s.repo.run(['rev-parse', 'HEAD']);
  // A desktop-v1.2.4 tag on the commit whose package.json says 1.2.3.
  s.routes[`${R}/git/ref/tags/desktop-v1.2.4`] = { ref: 'refs/tags/desktop-v1.2.4', object: { type: 'commit', sha: s.shas.tagged } };
  s.routes[`${R}/actions/runs/900`] = requestRun(s.shas.tagged, 'desktop-v1.2.4');
  await refused(s, /package\.json version/i, { HINT_TAG: 'desktop-v1.2.4' });
  // The tagged commit's package.json is read as git data only: no checkout of the sha occurs.
  for (const args of s.gitCalls) assert.ok(!['checkout', 'switch', 'reset', 'restore', 'worktree', 'stash', 'apply', 'am'].includes(args[0]), args.join(' '));
  assert.equal(s.repo.run(['rev-parse', 'HEAD']), before);
  assert.ok(s.gitCalls.some(args => args[0] === 'show' && args[1] === `${s.shas.tagged}:packages/desktop/package.json`));
 } finally { s.cleanup(); }
});

test('gate refuses red, cancelled, PR and other-sha ci runs', async () => {
 for (const bad of [{ conclusion: 'failure' }, { conclusion: 'cancelled' }, { event: 'pull_request' }, { head_sha: 'f'.repeat(40) }]) {
  const s = scenario();
  try {
   s.routes[`${R}/actions/workflows/desktop-ci.yml/runs?head_sha=${s.shas.tagged}&event=push&branch=main&status=completed&per_page=100`] = { workflow_runs: [ciRun(s.shas.tagged, bad)] };
   await refused(s, /no green desktop push run/);
  } finally { s.cleanup(); }
 }
});

test('gate refuses an extra or missing release artifact', async () => {
 for (const change of [list => [...list, { id: 999, name: 'desktop-installers-zz', digest: 'sha256:' + 'e'.repeat(64), expired: false }], list => list.slice(1), list => [...list, { ...list[0], id: 998 }]]) {
  const s = scenario();
  try {
   const path = `${R}/actions/runs/77/artifacts?per_page=100&page=1`;
   s.routes[path] = { artifacts: change(s.routes[path].artifacts) };
   await refused(s, /exact|duplicate/i);
  } finally { s.cleanup(); }
 }
});

test('gate does not read the immutable releases setting: the job token cannot (setup, publish and latestPublished cover it)', async () => {
 const s = scenario();
 try {
  delete s.routes[`${R}/immutable-releases`];
  s.routes[`${R}/immutable-releases`] = () => { throw new Error('GitHub API request failed (403)'); };
  assert.equal((await s.go()).sha, s.shas.tagged);
  assert.ok(!s.calls.some(path => path.includes('immutable-releases')), 'the gate never asks for the admin-only setting');
 } finally { s.cleanup(); }
});

test('a posture read the job token cannot do is reported as unreadable, never as off', async () => {
 for (const path of [`${R}/rules/branches/main`, `${R}/environments/desktop-release`, `${R}/environments/desktop-release/deployment-branch-policies?per_page=100`]) {
  for (const status of [403, 404]) {
   const s = scenario();
   try {
    s.routes[path] = () => { throw new Error(`GitHub API request failed (${status})`); };
    const error = await s.go().then(() => null, caught => caught);
    assert.ok(error, `${path} ${status} is refused`);
    assert.match(error.message, /^Cannot read the .+ with the job token/, `${path} ${status}`);
    assert.match(error.message, new RegExp(`\\(${status}\\)`));
    assert.doesNotMatch(error.message, /must|lacks|disabled|is off/i, `${path} ${status} is not reported as a missing setting`);
    assert.equal(readFileSync(s.outputFile, 'utf8'), '');
   } finally { s.cleanup(); }
  }
 }
});

test('gate refuses when the main ruleset lacks pull_request, server-ok and desktop-ok(15368), non_fast_forward or deletion', async () => {
 const variants = [
  rules => rules.filter(rule => rule.type !== 'pull_request'),
  rules => rules.filter(rule => rule.type !== 'required_status_checks'),
  rules => rules.map(rule => rule.type === 'required_status_checks' ? { ...rule, parameters: { required_status_checks: [{ context: 'server-ok', integration_id: 999 }, { context: 'desktop-ok', integration_id: 15368 }] } } : rule),
  rules => rules.map(rule => rule.type === 'required_status_checks' ? { ...rule, parameters: { required_status_checks: [{ context: 'server-ok', integration_id: 15368 }, { context: 'desktop-ok' }] } } : rule),
  rules => rules.map(rule => rule.type === 'required_status_checks' ? { ...rule, parameters: { required_status_checks: [{ context: 'server-ok', integration_id: 15368 }] } } : rule),
  rules => rules.map(rule => rule.type === 'required_status_checks' ? { ...rule, parameters: { required_status_checks: [{ context: 'desktop-ok', integration_id: 15368 }] } } : rule),
  rules => rules.map(rule => rule.type === 'required_status_checks' ? { ...rule, parameters: { required_status_checks: [{ context: 'ci-ok', integration_id: 15368 }] } } : rule),
  rules => rules.map(rule => rule.type === 'required_status_checks' ? { ...rule, parameters: { required_status_checks: [{ context: 'ci', integration_id: 15368 }, { context: 'desktop', integration_id: 15368 }] } } : rule),
  rules => rules.filter(rule => rule.type !== 'non_fast_forward'),
  rules => rules.filter(rule => rule.type !== 'deletion'),
  () => [],
 ];
 for (const change of variants) {
  const s = scenario();
  try {
   s.routes[`${R}/rules/branches/main`] = change(s.routes[`${R}/rules/branches/main`]);
   await refused(s, /main ruleset/i);
  } finally { s.cleanup(); }
 }
});

test('gate refuses when the environment policy is not exactly branch main', async () => {
 const policies = `${R}/environments/desktop-release/deployment-branch-policies?per_page=100`;
 const variants = [
  routes => { routes[policies] = { total_count: 2, branch_policies: [{ id: 1, name: 'main', type: 'branch' }, { id: 2, name: 'release/*', type: 'branch' }] }; },
  routes => { routes[policies] = { total_count: 2, branch_policies: [{ id: 1, name: 'main', type: 'branch' }, { id: 2, name: 'desktop-v*', type: 'tag' }] }; },
  routes => { routes[policies] = { total_count: 1, branch_policies: [{ id: 1, name: 'main', type: 'tag' }] }; },
  routes => { routes[policies] = { total_count: 1, branch_policies: [{ id: 1, name: 'main*', type: 'branch' }] }; },
  routes => { routes[policies] = { total_count: 0, branch_policies: [] }; },
  routes => { routes[`${R}/environments/desktop-release`] = { name: 'desktop-release', deployment_branch_policy: { protected_branches: true, custom_branch_policies: false } }; },
  routes => { routes[`${R}/environments/desktop-release`] = { name: 'desktop-release', deployment_branch_policy: null }; },
  routes => { delete routes[policies]; },
 ];
 for (const change of variants) {
  const s = scenario();
  try { change(s.routes); await refused(s, /environment/i); } finally { s.cleanup(); }
 }
});

test('gate refuses an already published version and allows a draft rerun', async () => {
 const s = scenario();
 try {
  s.routes[`${R}/releases?per_page=100&page=1`] = [{ tag_name: 'desktop-v1.2.3', draft: true }, { tag_name: 'desktop-v1.2.2', draft: false }];
  assert.equal((await s.go()).version, '1.2.3');
 } finally { s.cleanup(); }
 const p = scenario();
 try {
  p.routes[`${R}/releases?per_page=100&page=1`] = [{ tag_name: 'desktop-v1.2.3', draft: false }];
  await refused(p, /already published/i);
 } finally { p.cleanup(); }
});

test('gate refuses a non-greater version (beta.10 vs beta.9 numeric)', async () => {
 const s = scenario({ version: '0.1.0-beta.9' });
 try {
  s.routes[`${R}/releases?per_page=100&page=1`] = [{ tag_name: 'desktop-v0.1.0-beta.10', draft: false }, { tag_name: 'desktop-v0.0.9', draft: false }];
  await refused(s, /greater/i);
 } finally { s.cleanup(); }
 const ok = scenario({ version: '0.1.0-beta.10' });
 try {
  ok.routes[`${R}/releases?per_page=100&page=1`] = [{ tag_name: 'desktop-v0.1.0-beta.9', draft: false }, { tag_name: 'desktop-v0.2.0', draft: false }];
  ok.routes[`${R}/git/matching-refs/tags/desktop-v`] = [{ ref: 'refs/tags/desktop-v0.1.0-beta.9' }, { ref: 'refs/tags/desktop-v0.1.0-beta.10' }];
  const result = await ok.go();
  assert.equal(result.channel, 'beta');
  assert.equal(result.previous, 'desktop-v0.1.0-beta.9');
 } finally { ok.cleanup(); }
 // A paged release list is read to the end.
 const paged = scenario({ version: '0.1.0-beta.9' });
 try {
  paged.routes[`${R}/releases?per_page=100&page=1`] = Array.from({ length: 100 }, (_, i) => ({ tag_name: `desktop-v0.0.${i}`, draft: false }));
  paged.routes[`${R}/releases?per_page=100&page=2`] = [{ tag_name: 'desktop-v0.1.0-beta.9', draft: false }];
  await refused(paged, /already published/i);
 } finally { paged.cleanup(); }
});

test('gate refuses a sandbox component expiring within 30 days', async () => {
 const s = scenario({ expiresAt: new Date(NOW + 29 * DAY).toISOString() });
 try { await refused(s, /sandbox component/i); } finally { s.cleanup(); }
 const ok = scenario({ expiresAt: new Date(NOW + 31 * DAY).toISOString() });
 try { assert.equal((await ok.go()).version, '1.2.3'); } finally { ok.cleanup(); }
});

test('gate output values containing a newline or = are refused before any write', () => {
 const work = mkdtempSync(join(tmpdir(), 'dilla-gate-output-'));
 try {
  const file = join(work, 'out'); writeFileSync(file, '');
  const d = 'sha256:' + 'c'.repeat(64);
  const good = { tag: 'desktop-v1.2.3', version: '1.2.3', channel: 'stable', sha: 'a'.repeat(40), tree: 'b'.repeat(40), run: '77', artifact_ids: '1,2,3', artifact_digests: [d, d, d].join(','), run_record: 'd'.repeat(64), jobs_record: 'e'.repeat(64), previous: '', mac_update: 'zip' };
  for (const [key, value] of [['tag', 'desktop-v1.2.3\nstaged=B'], ['version', '1.2.3=x'], ['channel', 'beta\n'], ['sha', 'a'.repeat(39) + '\n'], ['tree', 'x'], ['run', '0'], ['run', '7\n8'], ['artifact_ids', '1,,2'], ['artifact_ids', '1=2'],
   ['artifact_digests', [d, d].join(',')], ['artifact_digests', [d, d, d + '\nx=y'].join(',')], ['artifact_digests', [d, d, 'sha256:' + 'C'.repeat(64)].join(',')], ['run_record', 'd'.repeat(63)], ['jobs_record', 'e'.repeat(64) + '\n'],
   ['previous', 'desktop-v1.0.0\nx=y'], ['previous', 'a=b'], ['mac_update', 'dmg\nx=y'], ['mac_update', 'pkg'], ['mac_update', '']]) {
   assert.throws(() => writeOutputs(file, { ...good, [key]: value }), /output/i, `${key}=${JSON.stringify(value)}`);
   assert.equal(readFileSync(file, 'utf8'), '');
  }
  assert.throws(() => writeOutputs(file, { ...good, extra: 'x' }), /output/i);
  assert.throws(() => writeOutputs(undefined, good), /output/i);
  writeOutputs(file, good);
  assert.equal(readFileSync(file, 'utf8'), `tag=desktop-v1.2.3\nversion=1.2.3\nchannel=stable\nsha=${'a'.repeat(40)}\ntree=${'b'.repeat(40)}\nrun=77\nartifact_ids=1,2,3\nartifact_digests=${[d, d, d].join(',')}\nrun_record=${'d'.repeat(64)}\njobs_record=${'e'.repeat(64)}\nprevious=\nmac_update=zip\n`);
 } finally { rmSync(work, { recursive: true, force: true }); }
});

/**
 * A rotation scenario on main's history c1 -> c0 -> c2: c1 is the pre-promote commit (slot A, OLD Sparkle key), c0 a
 * commit pinning an unrelated Sparkle key, c2 the tagged promote merge (`mainTrust`, PROMOTED by default).
 * `published` lists [version, commit ('c1' | 'c0' | 'c2'), extra release fields] of the published releases.
 */
function rotationScenario({ version = '1.2.3', published, mainTrust = PROMOTED }) {
 const commits = {};
 const s = scenario({ version, layout: (repo, c1) => {
  commits.c1 = c1;
  commits.c0 = repo.commit({ 'packages/desktop/release-trust.json': JSON.stringify({ ...TRUST, sparklePublicKey: Buffer.alloc(32, 9).toString('base64') }) + '\n' }, 'unrelated key');
  commits.c2 = repo.commit({ 'packages/desktop/release-trust.json': JSON.stringify(mainTrust) + '\n' }, 'rotate --promote');
  return { tagged: commits.c2, head: commits.c2, main: commits.c2 };
 } });
 s.routes[`${R}/releases?per_page=100&page=1`] = published.map(([v, , fields = {}]) => ({ tag_name: `desktop-v${v}`, draft: false, prerelease: v.includes('-beta.'), immutable: true, author: { login: 'github-actions[bot]', type: 'Bot' }, ...fields }));
 for (const [v, commit] of published) s.routes[`${R}/git/ref/tags/desktop-v${v}`] = { ref: `refs/tags/desktop-v${v}`, object: { type: 'commit', sha: commits[commit] } };
 return { ...s, commits };
}

test('after a promote, a channel\'s first release (its Macs still on the old Sparkle key) updates macOS with a DMG', async () => {
 for (const [version, published] of [
  ['1.2.3', [['1.2.2', 'c1'], ['1.3.0-beta.1', 'c1']]],
  ['1.3.0-beta.2', [['1.2.2', 'c1'], ['1.3.0-beta.1', 'c1']]],
  // Per channel: the beta rotation release does not move the stable Macs.
  ['1.2.3', [['1.2.2', 'c1'], ['1.3.0-beta.2', 'c2']]],
 ]) {
  const s = rotationScenario({ version, published });
  try {
   const result = await s.go();
   assert.equal(result.macUpdate, 'dmg', `${version} after ${JSON.stringify(published)}`);
   assert.match(readFileSync(s.outputFile, 'utf8'), /\nmac_update=dmg\n$/);
  } finally { s.cleanup(); }
 }
});

test('after a channel\'s rotation release, its next release reverts to the zip; a channel with no release has no Macs to move', async () => {
 for (const [version, published] of [
  ['1.2.4', [['1.2.3', 'c2'], ['1.2.2', 'c1']]],
  ['1.2.3', [['1.3.0-beta.1', 'c1']]],
 ]) {
  const s = rotationScenario({ version, published });
  try { assert.equal((await s.go()).macUpdate, 'zip', `${version} after ${JSON.stringify(published)}`); } finally { s.cleanup(); }
 }
 // Without a pending macOS key change every release is a zip, whatever was published before.
 const plain = rotationScenario({ published: [['1.2.2', 'c1']], mainTrust: { ...PROMOTED, signing: { active: 'B', staged: null } } });
 try { assert.equal((await plain.go()).macUpdate, 'zip'); } finally { plain.cleanup(); }
});

test('the gate refuses to pick a macOS update for Macs on a key the pending change does not start from, or from a release it did not make', async () => {
 const unrelated = rotationScenario({ published: [['1.2.2', 'c0']] });
 try { await refused(unrelated, /neither main's nor/); } finally { unrelated.cleanup(); }
 const handMade = rotationScenario({ published: [['1.2.2', 'c1', { author: { login: 'someone', type: 'User' } }]] });
 try { await refused(handMade, /not made by the release workflow/); } finally { handMade.cleanup(); }
 const mutable = rotationScenario({ published: [['1.2.2', 'c1', { immutable: false }]] });
 try { await refused(mutable, /immutable/); } finally { mutable.cleanup(); }
});
