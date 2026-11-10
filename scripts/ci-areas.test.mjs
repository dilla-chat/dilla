import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
// Fixtures live under one per-process root that is removed on exit.
const TEST_ROOT = mkdtempSync(join(tmpdir(), 'dilla-areas-run-'));
process.on('exit', () => rmSync(TEST_ROOT, { recursive: true, force: true }));
import { tmpdir } from 'node:os';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { areasFor, classifyFile, computeAreas, gateExpression, JOB_GATES, PATH_RULES, jobRuns, reachableOutputs, trackedFiles, WORKFLOWS } from './ci-areas.mjs';
import { parseYaml } from './ci-local-yaml.mjs';

const SCRIPT = fileURLToPath(new URL('./ci-areas.mjs', import.meta.url));
const REPO_ROOT = join(dirname(SCRIPT), '..');

const NONE = { go: false, core: false, web: false, e2e: false, text: false, mobile: false, code: false };
const ALL = { go: true, core: true, web: true, e2e: true, text: true, mobile: true, code: true };
const BROWSERS = ['core-integration'];
const WFS = Object.keys(WORKFLOWS);
// The gated jobs of one workflow that run for a set of outputs.
const ranJobs = (workflow, outputs) => Object.keys(JOB_GATES[workflow]).filter((job) => jobRuns(workflow, job, outputs));
const readWorkflow = (workflow) => parseYaml(readFileSync(join(REPO_ROOT, '.github', 'workflows', WORKFLOWS[workflow].file), 'utf8'));

test('docs-only changes set no area', () => {
  for (const files of [
    ['README.md'],
    ['docs/deploy/README.md', 'docs/screenshots/channel.png'],
    ['docs/superpowers/plans/2026-10-14-plan.md', 'docs/spikes/2026-09-wazero.md'],
    ['internal/api/README.md', 'e2e/README.md', 'scripts/README.md'],
    ['CONTRIBUTING.md', 'docs/a.md', 'LICENSE'],
  ]) {
    assert.deepEqual(areasFor(files), NONE, files.join(' '));
    for (const wf of WFS) assert.deepEqual(ranJobs(wf, areasFor(files)), [], `${wf}: ${files.join(' ')}`);
  }
});

test('Markdown that a test or a build reads is code of the area that reads it', () => {
  // docs/design/flows/*.md: byte-compared with en.ts by packages/web/src/copy.test.ts and strings*.test.ts.
  assert.deepEqual(areasFor(['docs/design/flows/06-voice.md']), { go: false, core: false, web: true, e2e: true, text: false, mobile: false, code: true });
  // protocol/*.md: the route tables are parsed by internal/dillad/factor_owed_test.go.
  for (const doc of ['protocol/09-http-api.md', 'protocol/02-delivery-service.md', 'protocol/00-overview.md']) {
    assert.deepEqual(areasFor([doc]), { go: true, core: false, web: false, e2e: false, text: false, mobile: false, code: true }, doc);
  }
  // A doc under core/ can be compiled into a crate (include_str!), so it runs the core area like the code.
  assert.deepEqual(areasFor(['core/vendor/README.md']), { ...ALL, text: false });
  // A flow doc next to docs-only files is still web.
  assert.equal(areasFor(['README.md', 'docs/design/flows/07-create.md']).web, true);
});

test('a UI-only change runs the web jobs and the browsers, no Go jobs', () => {
  const outputs = areasFor(['packages/ui/src/Button.tsx', 'packages/web/package.json']);
  assert.deepEqual(outputs, { go: false, core: false, web: true, e2e: true, text: false, mobile: true, code: true });
  assert.deepEqual(ranJobs('desktop', outputs), Object.keys(JOB_GATES.desktop));
  assert.deepEqual(ranJobs('mobile', outputs), Object.keys(JOB_GATES.mobile));
  // The release binaries embed the web client, so server rebuilds and embeds it; no Go tests run.
  assert.deepEqual(ranJobs('server', outputs).sort(), ['go-release', 'rust-native', 'rust-wasi', 'server-scripts', 'web-build']);
});

test('a change to anything release.yml ships or the bot-ops guard reads runs server-scripts', () => {
  for (const file of ['deploy/install.sh', 'deploy/install-bot.sh', 'deploy/install_bot_test.sh', 'scripts/check-bot-operations.mjs', 'internal/bot/protocol/codec.go', 'packages/client-core/src/bot/utilities.ts', 'package.json']) {
    assert.ok(jobRuns('server', 'server-scripts', areasFor([file])), file);
  }
  assert.ok(!jobRuns('server', 'server-scripts', areasFor(['docs/deploy/README.md'])));
});

test('the design tokens are read by a Go test, so they run the Go jobs too', () => {
  const outputs = areasFor(['packages/design-tokens/dist/tokens.css']);
  assert.deepEqual(outputs, { go: true, core: false, web: true, e2e: true, text: false, mobile: true, code: true });
  assert.ok(jobRuns('server', 'go-api', outputs));
});

test('sandbox bootstrap changes run Go interop and retain web and desktop coverage', () => {
  const outputs = areasFor(['packages/desktop/native/linux-sandbox/frontend/bootstrap.py']);
  assert.deepEqual(outputs, { go: true, core: false, web: true, e2e: true, text: false, mobile: false, code: true });
  for (const job of ['go', 'go-native']) assert.ok(jobRuns('server', job, outputs), job);
  for (const job of ['ui', 'desktop', 'linux-sandbox-foundation']) assert.ok(jobRuns('desktop', job, outputs), job);
  const neighbor = areasFor(['packages/desktop/native/linux-sandbox/frontend/other.py']);
  assert.deepEqual(neighbor, { go: false, core: false, web: true, e2e: true, text: false, mobile: false, code: true });
});

test('the LiveKit schema golden is read by the media checks, which run with web', () => {
  const outputs = areasFor(['internal/sfu/testdata/livekit-signal-schema.json']);
  assert.deepEqual(outputs, { go: true, core: false, web: true, e2e: true, text: false, mobile: false, code: true });
  assert.ok(jobRuns('desktop', 'rust-wasm-node', outputs));
  assert.ok(jobRuns('desktop', 'rust-wasm-node', areasFor(['packages/media/src/worker/x.ts'])));
});

test('a Go-only change runs the Go jobs and the release build, no rust lint or ui unit jobs', () => {
  const outputs = areasFor(['internal/api/messages.go', 'go.sum']);
  assert.deepEqual(outputs, { go: true, core: false, web: false, e2e: false, text: false, mobile: false, code: true });
  const ran = ranJobs('server', outputs);
  for (const job of ['go', 'go-checks', 'go-native', 'go-ds', 'go-api', 'go-harness', 'go-lint', 'go-postgres', 'go-release', 'web-build', 'rust-native', 'rust-wasi']) assert.ok(ran.includes(job), job);
  for (const job of ['vectors', 'deny']) assert.ok(!ran.includes(job), job);
});

test('a Go-only PR turns no desktop area on except through core', () => {
  const desktopAreas = ['web', 'e2e', 'core'];
  for (const files of [['internal/api/messages.go'], ['go.sum', 'cmd/dillad/main.go'], ['deploy/install.sh'], ['protocol/09-http-api.md'], ['internal/web/downloads.go', 'README.md']]) {
    const outputs = areasFor(files);
    assert.equal(outputs.go, true, files.join(' '));
    assert.deepEqual(desktopAreas.filter((area) => outputs[area]), [], files.join(' '));
    // Only the code-gated jobs run (the script gates and the two Rust artifact builds): no ui, wasm, browser or desktop job.
    assert.deepEqual(ranJobs('desktop', outputs), ['node', 'rust-native', 'rust-wasi'], files.join(' '));
  }
  // core drags the desktop lane in: the wasm core is the desktop app's.
  const core = areasFor(['internal/api/messages.go', 'core/dilla-core/src/lib.rs']);
  assert.deepEqual(desktopAreas.filter((area) => core[area]), desktopAreas);
  assert.deepEqual(ranJobs('desktop', core), Object.keys(JOB_GATES.desktop));
});

test('the mobile lane runs for the apps and the client they bundle, never for a server-only or desktop-only change', () => {
  for (const file of ['packages/mobile/src/bridge.ts', 'packages/mobile/android/app/build.gradle', 'packages/mobile/ios/App/App/Info.plist', 'packages/web/src/mobile.ts',
    'packages/client-core/src/worker/storage-key.ts', 'packages/ui/src/Button.tsx', 'packages/media/src/x.ts', 'packages/design-tokens/src/x.ts',
    'packages/desktop/src/browser-auth.ts', 'package-lock.json', 'e2e/web/support/native-https-proxy.mjs', 'core/dilla-core/src/lib.rs', 'Cargo.lock',
    '.github/workflows/mobile.yml', 'scripts/ci-areas.mjs', 'scripts/ci-android.sh',
    // What the emulator smoke test runs beside the app: the test host, the native peer and the WASI core.
    'cmd/dilla-testhost/main.go', 'testkit/src/main.rs', 'core/dilla-core-wasi/src/lib.rs', 'packages/mobile/e2e/android-smoke.mjs']) {
    assert.deepEqual(ranJobs('mobile', areasFor([file])), Object.keys(JOB_GATES.mobile), file);
  }
  for (const files of [['internal/api/messages.go'], ['cmd/dillad/main.go', 'go.sum'], ['packages/desktop/src/main.ts'], ['e2e/web/x.spec.ts'], ['docs/user/mobile.md']]) {
    assert.deepEqual(ranJobs('mobile', areasFor(files)), [], files.join(' '));
  }
  // A mobile-only change runs no Go job and no desktop lane beyond the code-gated script and artifact jobs.
  const outputs = areasFor(['packages/mobile/src/picker.ts']);
  assert.deepEqual(outputs, { go: false, core: false, web: true, e2e: true, text: false, mobile: true, code: true });
  // The test host's main package is Go and mobile: its own server tests still run, and so does the smoke test.
  assert.deepEqual(areasFor(['cmd/dilla-testhost/main.go']), { go: true, core: false, web: false, e2e: false, text: false, mobile: true, code: true });
});

test('a core change runs everything', () => {
  for (const file of ['core/dilla-core/src/lib.rs', 'Cargo.lock', 'testkit/src/main.rs', 'protocol/vectors/groups.json', 'deny.toml']) {
    assert.deepEqual(areasFor([file]), { ...ALL, text: false }, file);
    for (const wf of WFS) assert.deepEqual(ranJobs(wf, areasFor([file])), Object.keys(JOB_GATES[wf]), `${wf}: ${file}`);
  }
});

test('a workflow or script change runs everything', () => {
  for (const file of ['.github/workflows/server.yml', '.github/workflows/desktop-ci.yml', 'scripts/ci-areas.mjs', 'scripts/install-ci-tool.sh']) {
    assert.deepEqual(areasFor([file]), ALL, file);
    for (const wf of WFS) assert.deepEqual(ranJobs(wf, areasFor([file])), Object.keys(JOB_GATES[wf]), `${wf}: ${file}`);
  }
});

test('an e2e-only change runs the browsers and the persistence matrix, not the unit jobs', () => {
  const outputs = areasFor(['e2e/tests/voice.spec.ts']);
  assert.deepEqual(outputs, { go: false, core: false, web: false, e2e: true, text: false, mobile: false, code: true });
  assert.ok(BROWSERS.every((job) => jobRuns('desktop', job, outputs)));
  assert.ok(jobRuns('desktop', 'browser-spike', outputs));
  assert.ok(!jobRuns('desktop', 'ui', outputs) && !jobRuns('server', 'go', outputs));
});

test('a mixed change is the union of its files, and docs files add nothing', () => {
  assert.deepEqual(areasFor(['docs/a.md', 'packages/web/src/a.ts']), { go: false, core: false, web: true, e2e: true, text: false, mobile: true, code: true });
  assert.deepEqual(areasFor(['README.md', 'cmd/dillad/main.go', 'packages/web/src/a.ts']), { go: true, core: false, web: true, e2e: true, text: false, mobile: true, code: true });
  assert.deepEqual(areasFor(['deploy/install.sh']), { go: true, core: false, web: false, e2e: false, text: false, mobile: false, code: true });
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

test('both workflows get every area on for a push to main', () => {
  const outputs = computeAreas({ eventName: 'push', baseSha: '', diff: () => ['docs/a.md'] });
  assert.deepEqual(outputs, ALL);
  for (const wf of WFS) assert.deepEqual(ranJobs(wf, outputs), Object.keys(JOB_GATES[wf]), wf);
});

test('a scheduled run turns every desktop and mobile area on', () => {
  // The nightly run has no base to diff against; whatever the diff would say, nothing is narrowed.
  const outputs = computeAreas({ eventName: 'schedule', baseSha: '', diff: () => ['docs/a.md'] });
  assert.deepEqual(outputs, ALL);
  assert.deepEqual(computeAreas({ eventName: 'schedule', baseSha: 'abc', diff: () => ['docs/a.md'] }), ALL);
  assert.deepEqual(ranJobs('desktop', outputs), Object.keys(JOB_GATES.desktop));
  assert.deepEqual(ranJobs('mobile', outputs), Object.keys(JOB_GATES.mobile));
});

test('a path with a newline in it stays one path', () => {
  // docs/x\nrun.go is one file under docs/; split on newlines it would be the code file run.go.
  assert.deepEqual(computeAreas({ eventName: 'pull_request', baseSha: 'abc', diff: () => ['docs/x\nrun.go'] }), NONE);
});

test('every browser job runs together, whenever any does', () => {
  for (const outputs of reachableOutputs()) {
    assert.equal(new Set(BROWSERS.map((job) => jobRuns('desktop', job, outputs))).size, 1, JSON.stringify(outputs));
  }
});

test('a gate fails safe: a job whose output is missing or empty runs', () => {
  for (const wf of WFS) {
    for (const job of Object.keys(JOB_GATES[wf])) {
      assert.equal(jobRuns(wf, job, {}), true, job);
      assert.equal(jobRuns(wf, job, NONE), false, job);
      assert.match(gateExpression(wf, job), /^needs\.changes\.outputs\.(go|core|web|e2e|text|mobile|code) != 'false'( \|\| needs\.changes\.outputs\.(go|core|web|e2e|text|mobile|code) != 'false')*$/, job);
    }
  }
});

test('the reachable output sets come from the tracked files, so a new rule is covered', () => {
  const files = trackedFiles();
  assert.ok(files.length > 500, 'git ls-files returned the tree');
  const sets = reachableOutputs().map((o) => JSON.stringify(o));
  for (const file of files) assert.ok(sets.includes(JSON.stringify(areasFor([file]))), file);
  assert.ok(sets.includes(JSON.stringify(NONE)) && sets.includes(JSON.stringify(ALL)));
});

test('JOB_GATES names exactly the jobs of each workflow except its infrastructure', () => {
  for (const wf of WFS) {
    const jobs = Object.keys(readWorkflow(wf).jobs).filter((job) => !WORKFLOWS[wf].always.includes(job));
    assert.deepEqual(jobs.sort(), Object.keys(JOB_GATES[wf]).sort(), wf);
  }
});

test("a docs-only PR runs every workflow's changes job and reports its ok job", () => {
  const outputs = computeAreas({ eventName: 'pull_request', baseSha: 'abc', diff: () => ['README.md', 'docs/user/x.md'] });
  assert.deepEqual(outputs, NONE);
  for (const wf of WFS) {
    const { name, on, jobs } = readWorkflow(wf);
    assert.equal(name, WORKFLOWS[wf].name);
    // Triggered for every pull request and push to main, so its checks exist whatever the change touched.
    // The desktop and mobile workflows also run nightly (the schedule test above).
    assert.deepEqual(on, { push: { branches: ['main'] }, pull_request: null, ...(WORKFLOWS[wf].nightly ? { schedule: [{ cron: '17 2 * * *' }] } : {}) }, wf);
    assert.deepEqual(ranJobs(wf, outputs), [], wf);
    for (const job of WORKFLOWS[wf].always) {
      assert.ok(job in jobs, `${wf}: ${job}`);
      assert.doesNotMatch(String(jobs[job].if ?? ''), /needs\.changes/, `${wf}: ${job} carries an area gate`);
    }
    assert.equal(jobs.changes.needs, undefined, wf);
    // The ok job runs after skipped jobs too, and its verdict covers every other job of its workflow.
    const ok = jobs[WORKFLOWS[wf].ok];
    assert.equal(ok.if, 'always()', wf);
    assert.deepEqual([...ok.needs].sort(), Object.keys(jobs).filter((job) => job !== WORKFLOWS[wf].ok).sort(), wf);
  }
});

test('Linux sandbox foundation shares desktop fail-safe code eligibility', () => {
  assert.deepEqual(JOB_GATES.desktop['linux-sandbox-foundation'], ['e2e']);
  for (const outputs of [...reachableOutputs(), {}, { e2e: '' }]) {
    assert.equal(jobRuns('desktop', 'linux-sandbox-foundation', outputs), jobRuns('desktop', 'desktop', outputs));
  }
});

// The guard for the whole table: every file that a test or a build reads from another place must trigger the
// reader's jobs when it changes. It scans the sources for relative paths in reads and resolves them.
// Readers that no CI job runs (pre-existing): e2e/scripts/shoot-docs.test.mjs reads docs/user/*.md, and nothing runs it.
const UNRUN_READERS = ['e2e/scripts/shoot-docs.test.mjs'];
const READER_AREA = { internal: 'go', cmd: 'go', packages: 'web', core: 'core', testkit: 'core', e2e: 'e2e' };
// The apps' sources are read by the mobile lane (mobile-unit and the builds), so what they read must run it.
const READER_PREFIX_AREA = [['packages/mobile/', 'mobile']];
// This build-tagged Go test runs only in web-build/go-release, all gated by the web area (server's also by go).
// A local generated dist must not make the scanner mistake it for an ordinary Go-job reader.
const READER_PATH_AREA = {
  'internal/web/webdist_test.go': 'web',
  // Desktop units run in the reusable desktop workflow, whose real gate is e2e.
  'packages/desktop/test/linux-appimage-launcher.test.ts': 'e2e',
};

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

test('native maintenance diagnostic audit follows its exact Go source dependency',()=>{
 const path='internal/desktopsandbox/native_root_linux_test.go';assert.equal(areasFor([path]).web,true);
 assert.equal(areasFor(['internal/desktopsandbox/another_native_test.go']).web,false);
 const index=PATH_RULES.findIndex(rule=>rule.patterns.length===1&&rule.patterns[0]===path&&rule.areas.join(',')==='web');assert.ok(index>=0);
 const [rule]=PATH_RULES.splice(index,1);
 try{assert.equal(areasFor([path]).web,false,'omitting the exact dependency loses the privacy audit consumer');}finally{PATH_RULES.splice(index,0,rule);}
});

test('every path the code reads from another place is covered by a rule', () => {
  const found = [];
  // Only tracked targets: a runner's checkout holds nothing else, so a local build output (packages/web/dist after a
  // web build) is not a path a change can touch.
  const tracked = trackedFiles();
  const trackedSet = new Set(tracked);
  const trackedDirs = new Set();
  for (const f of tracked) {
    const parts = f.split('/');
    for (let i = 1; i < parts.length; i++) trackedDirs.add(parts.slice(0, i).join('/'));
  }
  for (const file of tracked) {
    const area = READER_PATH_AREA[file] ?? READER_PREFIX_AREA.find(([prefix]) => file.startsWith(prefix))?.[1] ?? READER_AREA[file.split('/')[0]];
    if (area === undefined || UNRUN_READERS.includes(file) || !/\.(go|rs|ts|tsx|mjs|js)$/.test(file) || !existsSync(join(REPO_ROOT, file))) continue;
    for (const read of relativeReads(file, readFileSync(join(REPO_ROOT, file), 'utf8'))) {
      const target = relative(REPO_ROOT, resolve(REPO_ROOT, dirname(file), read));
      if (target.startsWith('..') || !(trackedSet.has(target) || trackedDirs.has(target))) continue;
      const probe = trackedDirs.has(target) ? `${target}/x` : target;
      found.push({ file, area, target, probe });
    }
  }
  assert.ok(found.length > 30, `the scan found ${found.length} reads; it must have rotted`);
  assert.ok(found.some((f) => f.target.endsWith('design-tokens/dist/tokens.css')), 'the scan sees the Go test that reads the tokens');
  assert.ok(found.some((f) => f.target === 'docs/design/flows/06-voice.md'), 'the scan sees the strings tests that read the flows');
  assert.ok(found.some((f) => f.target === 'protocol/vectors/envelope.json'), 'the scan sees include_str! of a vector file');
  assert.ok(found.some((f) => f.target.endsWith('livekit-signal-schema.json')), 'the scan sees the schema check that reads the Go golden');
  const uncovered = found.filter((f) => !areasFor([f.probe])[f.area]);
  assert.deepEqual(uncovered.map((f) => `${f.file} reads ${f.target}: a change there does not run ${f.area}`), []);
});

// The CLI against a real repository: the path each workflow's changes job takes.
function repo() {
  const dir = mkdtempSync(join(TEST_ROOT, 'dilla-areas-'));
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
  assert.deepEqual(runCli(dir, { EVENT_NAME: 'pull_request', BASE_SHA: base }), { go: true, core: false, web: false, e2e: false, text: false, mobile: false, code: true });
});

test('the CLI treats moving code into docs/ as a code change', () => {
  const { dir, git, base } = repo();
  mkdirSync(join(dir, 'docs'));
  git('mv', 'internal/a.go', 'docs/a.go');
  git('commit', '-q', '-m', 'move');
  assert.deepEqual(runCli(dir, { EVENT_NAME: 'pull_request', BASE_SHA: base }), { go: true, core: false, web: false, e2e: false, text: false, mobile: false, code: true });
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
    assert.equal(jobRuns('desktop', 'node', areasFor([file])), true, file);
  }
  for (const file of ['internal/api/accounts.go', 'packages/web/src/screens/Settings.tsx', 'packages/client-core/src/profile-text.test.ts', 'core/dilla-core/src/lib.rs', 'e2e/tests/voice.spec.ts', 'internal/auth/handle.go']) {
    assert.equal(areasFor([file]).text, false, file);
  }
  assert.equal(areasFor(['README.md', 'docs/user/getting-started.md']).text, false);
  // A change to CI itself, a push and a diff that cannot be computed run the check.
  assert.equal(areasFor(['.github/workflows/server.yml']).text, true);
  assert.equal(areasFor(null).text, true);
});
