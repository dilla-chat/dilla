import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { areasFor, classifyFile, computeAreas, gateExpression, JOB_GATES, jobRuns, reachableOutputs, trackedFiles } from './ci-areas.mjs';

const SCRIPT = fileURLToPath(new URL('./ci-areas.mjs', import.meta.url));
const REPO_ROOT = join(dirname(SCRIPT), '..');

const NONE = { go: false, core: false, web: false, e2e: false, text: false, code: false };
const ALL = { go: true, core: true, web: true, e2e: true, text: true, code: true };
const BROWSERS = ['chromium-core', 'chromium-voice', 'chromium-web-1', 'chromium-web-2', 'chromium-web-3', 'firefox-core', 'firefox-voice', 'firefox-web-1', 'firefox-web-2', 'firefox-web-3', 'webkit-web'];
const ranJobs = (outputs) => Object.keys(JOB_GATES).filter((job) => jobRuns(job, outputs));

test('docs-only changes set no area', () => {
  for (const files of [
    ['README.md'],
    ['docs/deploy/README.md', 'docs/screenshots/channel.png'],
    ['docs/superpowers/plans/2026-10-14-plan.md', 'docs/spikes/2026-09-wazero.md'],
    ['internal/api/README.md', 'e2e/README.md', 'scripts/README.md'],
    ['CONTRIBUTING.md', 'docs/a.md', 'LICENSE'],
  ]) {
    assert.deepEqual(areasFor(files), NONE, files.join(' '));
    assert.deepEqual(ranJobs(areasFor(files)), [], files.join(' '));
  }
});

test('Markdown that a test or a build reads is code of the area that reads it', () => {
  // docs/design/flows/*.md: byte-compared with en.ts by packages/web/src/copy.test.ts and strings*.test.ts.
  assert.deepEqual(areasFor(['docs/design/flows/06-voice.md']), { go: false, core: false, web: true, e2e: true, text: false, code: true });
  // protocol/*.md: the route tables are parsed by internal/dillad/factor_owed_test.go.
  for (const doc of ['protocol/09-http-api.md', 'protocol/02-delivery-service.md', 'protocol/00-overview.md']) {
    assert.deepEqual(areasFor([doc]), { go: true, core: false, web: false, e2e: true, text: false, code: true }, doc);
  }
  // core/vendor/sqlite-wasm-vfs/README.md: #![doc = include_str!("../README.md")] compiles it into the wasm core.
  assert.deepEqual(areasFor(['core/vendor/sqlite-wasm-vfs/README.md']), { ...ALL, text: false });
  // A flow doc next to docs-only files is still web.
  assert.equal(areasFor(['README.md', 'docs/design/flows/07-create.md']).web, true);
});

test('a UI-only change runs the web jobs and the browsers, no Go jobs', () => {
  const outputs = areasFor(['packages/ui/src/Button.tsx', 'packages/web/package.json']);
  assert.deepEqual(outputs, { go: false, core: false, web: true, e2e: true, text: false, code: true });
  const ran = ranJobs(outputs);
  for (const job of ['node', 'ui', 'rust-wasm-node', 'browser-spike', 'browser-media', 'web-build', 'rust-native', 'rust-wasi', 'go-release', ...BROWSERS]) assert.ok(ran.includes(job), job);
  for (const job of ['vectors', 'deny', 'go', 'go-native', 'go-ds', 'go-api', 'go-harness', 'go-fts5-arm64', 'go-lint', 'go-vuln', 'go-sqlc', 'go-postgres']) assert.ok(!ran.includes(job), job);
});

test('the design tokens are read by a Go test, so they run the Go jobs too', () => {
  const outputs = areasFor(['packages/design-tokens/dist/tokens.css']);
  assert.deepEqual(outputs, { go: true, core: false, web: true, e2e: true, text: false, code: true });
  assert.ok(jobRuns('go-api', outputs));
});

test('the LiveKit schema golden is read by the media checks, which run with web', () => {
  const outputs = areasFor(['internal/sfu/testdata/livekit-signal-schema.json']);
  assert.deepEqual(outputs, { go: true, core: false, web: true, e2e: true, text: false, code: true });
  assert.ok(jobRuns('rust-wasm-node', outputs));
  assert.ok(jobRuns('rust-wasm-node', areasFor(['packages/media/src/worker/x.ts'])));
});

test('a Go-only change runs the Go jobs and the browsers, no rust or ui unit jobs', () => {
  const outputs = areasFor(['internal/api/messages.go', 'go.sum']);
  assert.deepEqual(outputs, { go: true, core: false, web: false, e2e: true, text: false, code: true });
  const ran = ranJobs(outputs);
  for (const job of ['go', 'go-native', 'go-ds', 'go-api', 'go-harness', 'go-lint', 'go-postgres', 'go-release', 'web-build', 'browser-spike', ...BROWSERS]) assert.ok(ran.includes(job), job);
  for (const job of ['ui', 'rust-wasm-node', 'vectors', 'deny']) assert.ok(!ran.includes(job), job);
});

test('a core change runs everything', () => {
  for (const file of ['core/dilla-core/src/lib.rs', 'Cargo.lock', 'testkit/src/main.rs', 'protocol/vectors/groups.json', 'deny.toml']) {
    assert.deepEqual(areasFor([file]), { ...ALL, text: false }, file);
    assert.deepEqual(ranJobs(areasFor([file])), Object.keys(JOB_GATES), file);
  }
});

test('a workflow or script change runs everything', () => {
  for (const file of ['.github/workflows/ci.yml', 'scripts/ci-areas.mjs', 'scripts/install-ci-tool.sh']) {
    assert.deepEqual(areasFor([file]), ALL, file);
    assert.deepEqual(ranJobs(areasFor([file])), Object.keys(JOB_GATES), file);
  }
});

test('an e2e-only change runs the browsers and the persistence matrix, not the unit jobs', () => {
  const outputs = areasFor(['e2e/tests/voice.spec.ts']);
  assert.deepEqual(outputs, { go: false, core: false, web: false, e2e: true, text: false, code: true });
  assert.ok(BROWSERS.every((job) => jobRuns(job, outputs)));
  assert.ok(jobRuns('browser-spike', outputs));
  assert.ok(!jobRuns('ui', outputs) && !jobRuns('go', outputs));
});

test('a mixed change is the union of its files, and docs files add nothing', () => {
  assert.deepEqual(areasFor(['docs/a.md', 'packages/web/src/a.ts']), { go: false, core: false, web: true, e2e: true, text: false, code: true });
  assert.deepEqual(areasFor(['README.md', 'cmd/dillad/main.go', 'packages/web/src/a.ts']), { go: true, core: false, web: true, e2e: true, text: false, code: true });
  assert.deepEqual(areasFor(['deploy/install.sh']), { go: true, core: false, web: false, e2e: true, text: false, code: true });
});

test('a file no rule names is code in every area', () => {
  assert.deepEqual(classifyFile('experiments/x.rs'), { unknown: true, areas: [] });
  assert.deepEqual(areasFor(['docs/a.md', 'experiments/x.rs']), ALL);
  assert.deepEqual(areasFor(['some/new/dir/file.txt']), ALL);
});

test('an unknown or empty diff fails safe to everything', () => {
  assert.deepEqual(areasFor(null), ALL);
  assert.deepEqual(areasFor([]), ALL);
});

test('only a pull request is narrowed, and a failing diff fails safe', () => {
  const docsOnly = () => ['README.md', 'docs/a.md'];
  assert.deepEqual(computeAreas({ eventName: 'pull_request', baseSha: 'abc', diff: docsOnly }), NONE);
  assert.deepEqual(computeAreas({ eventName: 'push', baseSha: 'abc', diff: docsOnly }), ALL);
  assert.deepEqual(computeAreas({ eventName: 'workflow_dispatch', baseSha: '', diff: docsOnly }), ALL);
  assert.deepEqual(computeAreas({ eventName: 'pull_request', baseSha: '', diff: docsOnly }), ALL);
  assert.deepEqual(computeAreas({ eventName: 'pull_request', baseSha: 'abc', diff: () => { throw new Error('bad object'); } }), ALL);
  assert.deepEqual(computeAreas({ eventName: 'pull_request', baseSha: 'abc', diff: () => [] }), ALL);
});

test('a path with a newline in it stays one path', () => {
  // docs/x\nrun.go is one file under docs/; split on newlines it would be the code file run.go.
  assert.deepEqual(computeAreas({ eventName: 'pull_request', baseSha: 'abc', diff: () => ['docs/x\nrun.go'] }), NONE);
});

test('every browser job runs together, whenever any does', () => {
  for (const outputs of reachableOutputs()) {
    assert.equal(new Set(BROWSERS.map((job) => jobRuns(job, outputs))).size, 1, JSON.stringify(outputs));
  }
});

test('a gate fails safe: a job whose output is missing or empty runs', () => {
  for (const job of Object.keys(JOB_GATES)) {
    assert.equal(jobRuns(job, {}), true, job);
    assert.equal(jobRuns(job, NONE), false, job);
    assert.match(gateExpression(job), /^needs\.changes\.outputs\.(go|core|web|e2e|text|code) != 'false'( \|\| needs\.changes\.outputs\.(go|core|web|e2e|text|code) != 'false')*$/, job);
  }
});

test('the reachable output sets come from the tracked files, so a new rule is covered', () => {
  const files = trackedFiles();
  assert.ok(files.length > 500, 'git ls-files returned the tree');
  const sets = reachableOutputs().map((o) => JSON.stringify(o));
  for (const file of files) assert.ok(sets.includes(JSON.stringify(areasFor([file]))), file);
  assert.ok(sets.includes(JSON.stringify(NONE)) && sets.includes(JSON.stringify(ALL)));
});

test('JOB_GATES names exactly the jobs of ci.yml but changes, docs and ci-ok', () => {
  const ci = readFileSync(join(REPO_ROOT, '.github', 'workflows', 'ci.yml'), 'utf8');
  const jobs = [...ci.slice(ci.indexOf('\njobs:')).matchAll(/^ {2}([A-Za-z0-9_-]+):\s*$/gm)].map((m) => m[1]).filter((job) => !['changes', 'docs', 'ci-ok'].includes(job));
  assert.deepEqual(jobs.sort(), Object.keys(JOB_GATES).sort());
});

// The guard for the whole table: every file that a test or a build reads from another place must trigger the
// reader's jobs when it changes. It scans the sources for relative paths in reads and resolves them.
// Readers that no CI job runs (pre-existing): e2e/scripts/shoot-docs.test.mjs reads docs/user/*.md, and nothing runs it.
const UNRUN_READERS = ['e2e/scripts/shoot-docs.test.mjs'];
const READER_AREA = { internal: 'go', cmd: 'go', packages: 'web', core: 'core', testkit: 'core', e2e: 'e2e' };
// This build-tagged Go test runs only in web-build/go-release, both gated by the web area.
// A local generated dist must not make the scanner mistake it for an ordinary Go-job reader.
const READER_PATH_AREA = { 'internal/web/webdist_test.go': 'web' };

function relativeReads(file, text) {
  const reads = [];
  if (file.endsWith('.go')) {
    for (const m of text.matchAll(/"((?:\.\.\/)+[^"]*)"/g)) reads.push(m[1]);
    for (const m of text.matchAll(/filepath\.Join\(\s*"\.\."((?:\s*,\s*"[^"]*")+)\s*\)/g)) {
      reads.push(['..', ...[...m[1].matchAll(/"([^"]*)"/g)].map((s) => s[1])].join('/'));
    }
  } else if (file.endsWith('.rs')) {
    for (const m of text.matchAll(/include_(?:str|bytes)!\(\s*"([^"]+)"/g)) reads.push(m[1]);
  } else {
    for (const m of text.matchAll(/new URL\(\s*[`'"]((?:\.\.\/)+[^`'"]*)[`'"]/g)) reads.push(m[1]);
  }
  return reads.filter((read) => read.startsWith('..'));
}

test('every path the code reads from another place is covered by a rule', () => {
  const found = [];
  for (const file of trackedFiles()) {
    const area = READER_PATH_AREA[file] ?? READER_AREA[file.split('/')[0]];
    if (area === undefined || UNRUN_READERS.includes(file) || !/\.(go|rs|ts|tsx|mjs|js)$/.test(file) || !existsSync(join(REPO_ROOT, file))) continue;
    for (const read of relativeReads(file, readFileSync(join(REPO_ROOT, file), 'utf8'))) {
      const target = relative(REPO_ROOT, resolve(REPO_ROOT, dirname(file), read));
      if (target.startsWith('..') || !existsSync(join(REPO_ROOT, target))) continue;
      const probe = statSync(join(REPO_ROOT, target)).isDirectory() ? `${target}/x` : target;
      found.push({ file, area, target, probe });
    }
  }
  assert.ok(found.length > 30, `the scan found ${found.length} reads; it must have rotted`);
  assert.ok(found.some((f) => f.target.endsWith('design-tokens/dist/tokens.css')), 'the scan sees the Go test that reads the tokens');
  assert.ok(found.some((f) => f.target === 'docs/design/flows/06-voice.md'), 'the scan sees the strings tests that read the flows');
  assert.ok(found.some((f) => f.target === 'core/vendor/sqlite-wasm-vfs/README.md'), 'the scan sees include_str! of a README');
  assert.ok(found.some((f) => f.target.endsWith('livekit-signal-schema.json')), 'the scan sees the schema check that reads the Go golden');
  const uncovered = found.filter((f) => !areasFor([f.probe])[f.area]);
  assert.deepEqual(uncovered.map((f) => `${f.file} reads ${f.target}: a change there does not run ${f.area}`), []);
});

// The CLI against a real repository: the path ci.yml takes.
function repo() {
  const dir = mkdtempSync(join(tmpdir(), 'dilla-areas-'));
  const git = (...args) => execFileSync('git', ['-c', 'user.email=a@b.c', '-c', 'user.name=t', '-c', 'commit.gpgsign=false', ...args], { cwd: dir, encoding: 'utf8' });
  git('init', '-q');
  mkdirSync(join(dir, 'internal'));
  writeFileSync(join(dir, 'internal', 'a.go'), 'package a\n');
  writeFileSync(join(dir, 'README.md'), 'x\n');
  git('add', '.');
  git('commit', '-q', '-m', 'base');
  return { dir, git, base: git('rev-parse', 'HEAD').trim() };
}

function runCli(dir, env) {
  const out = join(dir, 'github-output');
  writeFileSync(out, '');
  const result = spawnSync('node', [SCRIPT], { cwd: dir, env: { ...process.env, GITHUB_OUTPUT: out, ...env }, encoding: 'utf8' });
  assert.equal(result.status, 0, result.stderr);
  return Object.fromEntries(readFileSync(out, 'utf8').trim().split('\n').map((line) => line.split('=')).map(([k, v]) => [k, v === 'true']));
}

test('the CLI reads the diff against the base: docs-only, then code', () => {
  const { dir, git, base } = repo();
  writeFileSync(join(dir, 'README.md'), 'changed\n');
  mkdirSync(join(dir, 'docs'));
  writeFileSync(join(dir, 'docs', 'a.md'), 'new\n');
  git('add', '.');
  git('commit', '-q', '-m', 'docs');
  assert.deepEqual(runCli(dir, { EVENT_NAME: 'pull_request', BASE_SHA: base }), NONE);
  writeFileSync(join(dir, 'internal', 'a.go'), 'package b\n');
  git('commit', '-q', '-a', '-m', 'code');
  assert.deepEqual(runCli(dir, { EVENT_NAME: 'pull_request', BASE_SHA: base }), { go: true, core: false, web: false, e2e: true, text: false, code: true });
});

test('the CLI treats moving code into docs/ as a code change', () => {
  const { dir, git, base } = repo();
  mkdirSync(join(dir, 'docs'));
  git('mv', 'internal/a.go', 'docs/a.go');
  git('commit', '-q', '-m', 'move');
  assert.deepEqual(runCli(dir, { EVENT_NAME: 'pull_request', BASE_SHA: base }), { go: true, core: false, web: false, e2e: true, text: false, code: true });
});

test('the CLI keeps a path with a newline in one piece', () => {
  const { dir, git, base } = repo();
  mkdirSync(join(dir, 'docs'));
  writeFileSync(join(dir, 'docs', 'x\nrun.go'), 'x\n');
  git('add', '.');
  git('commit', '-q', '-m', 'odd name');
  // docs/x\nrun.go is under docs/, so it is docs; split on newlines it would have been the code file run.go.
  assert.deepEqual(runCli(dir, { EVENT_NAME: 'pull_request', BASE_SHA: base }), NONE);
});

test('the CLI fails safe: a base commit that is not in the clone, a push, no base', () => {
  const { dir } = repo();
  assert.deepEqual(runCli(dir, { EVENT_NAME: 'pull_request', BASE_SHA: '0'.repeat(40) }), ALL);
  assert.deepEqual(runCli(dir, { EVENT_NAME: 'pull_request', BASE_SHA: '' }), ALL);
  assert.deepEqual(runCli(dir, { EVENT_NAME: 'push', BASE_SHA: '' }), ALL);
});

test('the profile-text tables are regenerated only when the generator, the tables or the rule change', () => {
  const touching = [
    'scripts/gen-profile-text-tables.mjs', 'internal/auth/profile_tables.go', 'internal/auth/profile_text.go', 'internal/auth/testdata/rgi-zwj-sequences.txt',
    'packages/client-core/src/profile-tables.ts', 'packages/client-core/src/profile-text.ts',
  ];
  for (const file of touching) {
    assert.equal(areasFor([file]).text, true, file);
    assert.equal(jobRuns('node', areasFor([file])), true, file);
  }
  for (const file of ['internal/api/accounts.go', 'packages/web/src/screens/Settings.tsx', 'packages/client-core/src/profile-text.test.ts', 'core/dilla-core/src/lib.rs', 'e2e/tests/voice.spec.ts', 'internal/auth/handle.go']) {
    assert.equal(areasFor([file]).text, false, file);
  }
  assert.equal(areasFor(['README.md', 'docs/user/getting-started.md']).text, false);
  // A change to CI itself, a push and a diff that cannot be computed run the check.
  assert.equal(areasFor(['.github/workflows/ci.yml']).text, true);
  assert.equal(areasFor(null).text, true);
});
