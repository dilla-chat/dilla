import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, existsSync, lstatSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseYaml } from './ci-local-yaml.mjs';
import {
  BLOCK_SIZE, E2E_PORT_SPAN, EXTRA_JOBS, JOB_MAP, MAX_BLOCK, UI_PORT_SPAN, blockPorts, classifyStep, dependencyClosure, e2eBlockEnv, evalIf,
  expandJobs, localMirrorProblems, mapService, moveUiPorts, selectJobs, skipReason, substitute,
} from './ci-local-plan.mjs';
import { changedOutputs, parseArgs, parseEnvFile, sarifBlocking } from './ci-local.mjs';
import { checkLocalMirror } from './check-ci-workflow.mjs';
import {
  SCRUBBED_ENV, buildCleanTree, busyPorts, lockDir, lockDirPath, ownerAlive, processStart, recoverSnapshots, releaseLock, restoreSnapshot, scrubEnv, takeSnapshot, tryLock,
} from './ci-local-fs.mjs';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const REAL_CI = readFileSync(join(ROOT, '.github', 'workflows', 'ci.yml'), 'utf8');
const JOBS = expandJobs(parseYaml(REAL_CI));
const EVERYTHING = { go: true, core: true, web: true, e2e: true, text: true, code: true };
const job = (id) => JOBS.find((j) => j.id === id);

// ---------------------------------------------------------------- parsing

test('the YAML subset: mappings, `- key:` items, block scalars, flow sequences, quotes and comments', () => {
  const doc = parseYaml([
    'a: 1 # trailing',
    'b:',
    '  - x',
    "  - 'it''s'",
    '  - k: v',
    '    run: |',
    '      echo one',
    '      # kept: a comment inside a block scalar is content',
    '',
    '      echo two',
    '    if: always()',
    'c: [one, "two", three]',
    'd: >-',
    '  --health-cmd "pg_isready"',
    '  --health-retries 10',
    'e: "a # not a comment"',
  ].join('\n'));
  assert.deepEqual(doc, {
    a: '1',
    b: ['x', "it's", { k: 'v', run: 'echo one\n# kept: a comment inside a block scalar is content\n\necho two\n', if: 'always()' }],
    c: ['one', 'two', 'three'],
    d: '--health-cmd "pg_isready" --health-retries 10',
    e: 'a # not a comment',
  });
});

test('the YAML subset refuses anchors, flow mappings and duplicate keys instead of guessing', () => {
  assert.throws(() => parseYaml('a: &x 1\nb: *x\n'), /unsupported YAML construct/);
  assert.throws(() => parseYaml('a: {b: 1}\n'), /unsupported YAML construct/);
  assert.throws(() => parseYaml('a: 1\na: 2\n'), /duplicate key/);
});

test('ci.yml parses exactly as js-yaml reads it (when js-yaml is installed)', async (t) => {
  let yaml;
  try {
    yaml = (await import('js-yaml')).default;
  } catch {
    t.skip('js-yaml is not installed');
    return;
  }
  const strings = (v) => (v === null ? null : Array.isArray(v) ? v.map(strings) : typeof v === 'object' ? Object.fromEntries(Object.entries(v).map(([k, x]) => [k, strings(x)])) : String(v));
  for (const file of ['ci.yml', 'release.yml']) {
    const text = readFileSync(join(ROOT, '.github', 'workflows', file), 'utf8');
    assert.deepEqual(parseYaml(text), strings(yaml.load(text)), file);
  }
});

// ---------------------------------------------------------------- the job graph

test('every ci.yml job is mapped in JOB_MAP and every JOB_MAP entry is a ci.yml job', () => {
  assert.deepEqual(localMirrorProblems(REAL_CI), []);
  assert.deepEqual(checkLocalMirror(ROOT), []);
  assert.deepEqual(new Set(JOBS.map((j) => j.name)), new Set(Object.keys(JOB_MAP)));
});

test('a job ci.yml gains without a JOB_MAP entry fails the sync check, and so does a stale entry', () => {
  const added = REAL_CI.replace('\n  ci-ok:\n', '\n  brand-new:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n  ci-ok:\n');
  assert.ok(localMirrorProblems(added).some((p) => p.includes('"brand-new" is neither mapped nor skipped')));
  const removed = REAL_CI.replace(/\n {2}deny:\n[\s\S]*?(?=\n {2}go:\n)/, '');
  assert.ok(localMirrorProblems(removed).some((p) => p.includes('JOB_MAP maps "deny"')));
});

test('an unmapped action, sudo step, if: form, expression or service fails the sync check', () => {
  const cases = [
    ['      - run: npm run test:docs-check\n', '      - uses: actions/setup-python@0000000000000000000000000000000000000000\n      - run: npm run test:docs-check\n', 'actions/setup-python is unmapped'],
    ['      - run: npm run test:docs-check\n', '      - run: sudo apt-get install -y foo\n      - run: npm run test:docs-check\n', 'needs sudo'],
    ['      - run: npm run test:docs-check\n', "      - if: github.event_name == 'push'\n        run: npm run test:docs-check\n", 'unmapped if:'],
    ['      - run: npm run test:docs-check\n', '      - run: echo ${{ secrets.TOKEN }}\n', 'unmapped expression'],
    ['      - run: npm run test:docs-check\n', '      - run: npm run test:docs-check\n        shell: pwsh\n', 'step key "shell"'],
    ['    services:\n      postgres:\n', '    services:\n      redis:\n        image: redis\n      postgres:\n', 'service "redis" is unmapped'],
  ];
  for (const [from, to, want] of cases) {
    assert.ok(REAL_CI.includes(from), from);
    const problems = localMirrorProblems(REAL_CI.replace(from, to));
    assert.ok(problems.some((p) => p.includes(want)), `${want}:\n${problems.join('\n')}`);
  }
});

test('a matrix job becomes one job per leg, and an arm64 leg is skipped with its reason on x64', () => {
  assert.deepEqual(JOBS.filter((j) => j.name === 'spotify-helper').map((j) => [j.id, j.runsOn]), [['spotify-helper-amd64', 'ubuntu-latest'], ['spotify-helper-arm64', 'ubuntu-24.04-arm']]);
  assert.match(skipReason(job('spotify-helper-arm64'), 'x64'), /native linux\/arm64 runner/);
  assert.match(skipReason(job('go-fts5-arm64'), 'x64'), /native linux\/arm64 runner/);
  assert.equal(skipReason(job('go-fts5-arm64'), 'arm64'), null);
  assert.equal(skipReason(job('spotify-helper-amd64'), 'x64'), null);
});

test('the needs graph: a job brings what it needs, in dependency order, and a cycle or unknown need throws', () => {
  const order = dependencyClosure(JOBS, ['chromium-web-1']);
  assert.deepEqual(order.slice(-1), ['chromium-web-1']);
  assert.deepEqual(new Set(order), new Set(['changes', 'rust-native', 'rust-wasi', 'web-build', 'chromium-web-1']));
  for (const id of order) for (const need of job(id).needs) assert.ok(order.indexOf(need) < order.indexOf(id), `${need} before ${id}`);
  const fake = [{ id: 'a', name: 'a', needs: ['b'] }, { id: 'b', name: 'b', needs: ['a'] }];
  assert.throws(() => dependencyClosure(fake, ['a']), /cycle/);
  assert.throws(() => dependencyClosure([{ id: 'a', name: 'a', needs: ['zz'] }], ['a']), /unknown job "zz"/);
  // ci-ok needs both spotify-helper legs.
  assert.ok(dependencyClosure(JOBS, ['ci-ok']).includes('spotify-helper-arm64'));
});

test('--only picks those jobs and their needs; --no-deps leaves the needs out; an unknown name lists the jobs', () => {
  assert.deepEqual(selectJobs(JOBS, { only: ['go'], outputs: EVERYTHING }), ['changes', 'rust-wasi', 'go']);
  assert.deepEqual(selectJobs(JOBS, { only: ['go'], outputs: EVERYTHING, deps: false }), ['go']);
  assert.deepEqual(selectJobs(JOBS, { only: ['spotify-helper'], outputs: EVERYTHING, deps: false }), ['spotify-helper-amd64', 'spotify-helper-arm64']);
  assert.deepEqual(selectJobs(JOBS, { only: ['codeql'], outputs: EVERYTHING }), ['codeql']);
  // ci-ok aggregates what ran; naming it does not pull in the whole workflow.
  assert.deepEqual(selectJobs(JOBS, { only: ['docs', 'ci-ok'], outputs: EVERYTHING }), ['docs', 'ci-ok']);
  assert.throws(() => selectJobs(JOBS, { only: ['nope'], outputs: EVERYTHING }), /unknown job "nope"; jobs: changes, docs/);
});

test('a full run covers every ci.yml job and the extra jobs', () => {
  const all = selectJobs(JOBS, { only: null, outputs: EVERYTHING });
  assert.deepEqual(new Set(all), new Set([...JOBS.map((j) => j.id), ...Object.keys(EXTRA_JOBS)]));
});

// ---------------------------------------------------------------- --changed

test('--changed runs what the ci-areas gates would: a docs-only diff runs only changes, docs and ci-ok', () => {
  const outputs = changedOutputs('base', ['docs/user/x.md', 'README.md']);
  assert.equal(outputs.code, false);
  assert.deepEqual(selectJobs(JOBS, { only: null, outputs }), ['changes', 'docs', 'ci-ok']);
});

test('--changed: a Go file runs the Go jobs and the browser suites, not the Rust lint or ui', () => {
  const outputs = changedOutputs('base', ['internal/api/x.go']);
  const chosen = selectJobs(JOBS, { only: null, outputs });
  for (const id of ['go', 'go-api', 'go-postgres', 'chromium-web-1', 'webkit-web', 'rust-wasi', 'rust-native', 'codeql']) assert.ok(chosen.includes(id), id);
  for (const id of ['ui', 'vectors', 'deny', 'rust-wasm-node']) assert.ok(!chosen.includes(id), id);
  // rust-native runs for its artifact; its lint and test steps are core-gated.
  const fmt = job('rust-native').steps.find((s) => s.run === 'cargo fmt --all --check');
  assert.equal(evalIf(fmt.if, { outputs, steps: {} }), false);
});

test('--changed fails safe: an unknown path or a ci change turns everything on', () => {
  assert.deepEqual(changedOutputs('base', ['no-rule-names-this']), EVERYTHING);
  assert.deepEqual(changedOutputs('base', ['scripts/ci-local.mjs']), EVERYTHING);
  assert.deepEqual(changedOutputs(undefined, ['internal/x.go']), EVERYTHING);
});

// ---------------------------------------------------------------- steps

test('if: expressions of the forms ci.yml uses', () => {
  const ctx = { outputs: { ...EVERYTHING, web: false }, steps: { a: { 'cache-hit': 'true' } } };
  assert.equal(evalIf("needs.changes.outputs.web != 'false'", ctx), false);
  assert.equal(evalIf("needs.changes.outputs.go != 'false' || needs.changes.outputs.web != 'false'", ctx), true);
  assert.equal(evalIf("steps.a.outputs.cache-hit != 'true'", ctx), false);
  assert.equal(evalIf("steps.b.outputs.cache-hit != 'true'", ctx), true);
  assert.equal(evalIf(null, ctx), true);
  assert.equal(evalIf(null, { ...ctx, failed: true }), false);
  assert.equal(evalIf('failure()', { ...ctx, failed: true }), true);
  assert.equal(evalIf('failure()', ctx), false);
  assert.equal(evalIf("needs.changes.outputs.go != 'false'", { ...ctx, failed: true }), false);
  assert.throws(() => evalIf("github.ref == 'refs/heads/main'", ctx), /unmapped if:/);
});

test('step mapping: GitHub-only steps become local equivalents, the rest runs as written', () => {
  const steps = (id) => job(id).steps.map((s) => classifyStep(s, job(id)));
  const web1 = steps('chromium-web-1');
  assert.deepEqual(web1[0], { kind: 'noop', reason: 'the working tree is the checkout' });
  assert.deepEqual(web1.filter((m) => m.kind === 'prereq').map((m) => m.tool), ['go', 'node']);
  assert.deepEqual(web1.filter((m) => m.kind === 'download').map((m) => [m.name, m.path]), [
    ['dilla-core-wasi', 'internal/mlswasi/testdata'], ['dilla-testkit', 'artifacts'], ['dilla-web-dist', 'packages/web/dist'], ['dilla-core-harness', 'packages/client-core/harness-dist'],
  ]);
  const pw = web1.find((m) => m.memo === 'playwright install chromium');
  assert.equal(pw.script, 'npx playwright install chromium');
  assert.ok(web1.some((m) => m.kind === 'run' && m.script === 'npm run test:e2e:web:chromium -w @dilla/e2e -- --shard=1/3' && m.memo === null));
  assert.match(web1.at(-1).reason, /failure upload/);
  assert.equal(web1.filter((m) => m.kind === 'noop' && /apt cache/.test(m.reason)).length, 3);
  // ui installs through the playwright CLI file; it shares the same once-per-run install.
  assert.ok(steps('ui').some((m) => m.memo === 'playwright install chromium'));
  // The clang step checks the local LLVM line instead of apt-installing it.
  const clang = steps('web-build').find((m) => m.tool === 'llvm');
  assert.equal(clang.version, '23.1');
  // install-ci-tool runs as written (pinned, hash-checked downloads), once per run.
  assert.ok(steps('go-lint').some((m) => m.memo === 'bash scripts/install-ci-tool.sh golangci-lint'));
  // sudo prlimit becomes an unprivileged prlimit; the cargo test still runs.
  const prl = steps('rust-native').find((m) => m.kind === 'run' && /cargo test --workspace/.test(m.script));
  assert.doesNotMatch(prl.script, /sudo/);
  assert.match(prl.script, /^prlimit --pid "\$\$" --memlock=268435456:268435456 /);
  // The audio server is a prerequisite, the rest of firefox-voice runs.
  assert.ok(steps('firefox-voice').some((m) => m.tool === 'audio'));
  // Uploads keep their paths and the if-no-files-found rule.
  assert.deepEqual(steps('rust-wasi').at(-1), { kind: 'upload', name: 'dilla-core-wasi', paths: ['target/wasm32-wasip1/release/dilla_core_wasi.wasm'], required: true });
});

test('the node job runs test:ci-check on a clean tree, and a cleanTree entry that is no step fails the sync check', () => {
  assert.deepEqual(JOB_MAP.node.cleanTree, ['npm run test:ci-check']);
  const gone = REAL_CI.replace('      - run: npm run test:ci-check\n', '');
  assert.ok(localMirrorProblems(gone).some((p) => p.includes('cleanTree names "npm run test:ci-check"')));
});

test('expressions in steps that run: workspace, runner.temp and matrix values, nothing else', () => {
  const ctx = { root: '/r', runnerTemp: '/t', jobName: 'j', matrix: { arch: 'amd64' } };
  assert.equal(substitute('${{ github.workspace }}/artifacts/dilla-testkit', ctx), '/r/artifacts/dilla-testkit');
  assert.equal(substitute('"$X" ${{ runner.temp }}/ucd ${{ matrix.arch }}', ctx), '"$X" /t/ucd amd64');
  assert.throws(() => substitute('${{ secrets.X }}', ctx), /unmapped expression/);
});

test('the postgres service: same image, env and initdb args, a readiness command, published on a port of its own', () => {
  const svc = mapService('postgres', job('go-postgres').services.postgres);
  assert.equal(svc.image, 'postgres:18.6-alpine3.24');
  assert.deepEqual(svc.env, { POSTGRES_PASSWORD: 'dilla', POSTGRES_USER: 'dilla', POSTGRES_DB: 'dilla', POSTGRES_INITDB_ARGS: '--locale-provider=builtin --builtin-locale=C.UTF-8' });
  assert.deepEqual(svc.ports, [{ host: 5432, container: 5432 }]);
  assert.equal(svc.health, 'pg_isready -U dilla');
  assert.deepEqual(svc.options, []);
  const pgEnv = job('go-postgres').steps.find((s) => s.env?.DILLA_TEST_PG).env.DILLA_TEST_PG;
  assert.match(pgEnv, /127\.0\.0\.1:5432/);
});

test('GITHUB_ENV and GITHUB_OUTPUT files, heredocs included', () => {
  assert.deepEqual(parseEnvFile('A=1\nB<<EOF\nx\ny\nEOF\nC=a=b\n'), { A: '1', B: 'x\ny', C: 'a=b' });
});

test('CodeQL: error-level results and security severity 7 or more would fail the check', () => {
  const sarif = { runs: [{ tool: { driver: { rules: [{ id: 'hi', properties: { 'security-severity': '7.5' } }, { id: 'lo', properties: { 'security-severity': '3.1' } }] } }, results: [
    { ruleId: 'hi', level: 'warning', message: { text: 'm' }, locations: [{ physicalLocation: { artifactLocation: { uri: 'a.go' }, region: { startLine: 3 } } }] },
    { ruleId: 'lo', level: 'note' },
    { ruleId: 'lo', level: 'error' },
  ] }] };
  const r = sarifBlocking(sarif);
  assert.equal(r.total, 3);
  assert.equal(r.blocking.length, 2);
  assert.match(r.blocking[0], /^hi \(warning, severity 7\.5\) a\.go:3: m$/);
});

// ---------------------------------------------------------------- ports

test('port blocks: block k moves the suites by k*1000 and the ui job by k*50, within 0..MAX_BLOCK', () => {
  assert.deepEqual(blockPorts(0), { block: 0, offset: 0, uiOffset: 0 });
  assert.deepEqual(blockPorts(3), { block: 3, offset: 3000, uiOffset: 150 });
  assert.throws(() => blockPorts(MAX_BLOCK + 1), /outside/);
  const env = e2eBlockEnv(2, 'chromium-web-1', '/repo', '/tmp/x');
  assert.deepEqual(env, { DILLA_E2E_PORT_OFFSET: '2000', DILLA_E2E_HOST_DATA_ROOT: '/tmp/x/dw-2', DILLA_E2E_OUTPUT_DIR: '/repo/target/ci-local/e2e/chromium-web-1' });
});

test('the ui job: every Storybook port moves with the block, and blocks never reach the browser suites', () => {
  const ui = job('ui').steps.filter((s) => s.run).map((s) => s.run).join('\n');
  const ports = (text) => [...text.matchAll(/(?:--port |127\.0\.0\.1:|shoot-stories\.mjs )(\d{4,5})\b/g)].map((m) => Number(m[1]));
  const base = ports(ui);
  assert.ok(base.length >= 6);
  const moved = ports(moveUiPorts(ui, blockPorts(MAX_BLOCK).uiOffset));
  assert.deepEqual(moved, base.map((p) => p + MAX_BLOCK * 50));
  assert.ok(Math.max(...moved) < E2E_PORT_SPAN[0], 'the highest ui block stays under the lowest browser-suite port');
  for (const p of base) assert.ok(p >= UI_PORT_SPAN[0] && p <= UI_PORT_SPAN[1], `ui port ${p} is outside UI_PORT_SPAN, so the block probe misses it`);
});

/** The suites' sources the port blocks move: the core, web and voice configs and specs. */
function suiteSources() {
  const e2e = join(ROOT, 'e2e');
  const files = ['playwright.core.config.ts', 'playwright.web.config.ts', 'playwright.voice.config.ts'].map((f) => join(e2e, f));
  const walk = (dir) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      if (entry.isDirectory()) walk(join(dir, entry.name));
      else if (entry.name.endsWith('.ts')) files.push(join(dir, entry.name));
    }
  };
  walk(join(e2e, 'web'));
  walk(join(e2e, 'voice'));
  return files.map((f) => [f, readFileSync(f, 'utf8')]);
}

test('every host, control and SFU port of the core, web and voice suites goes through e2ePort, within one block', () => {
  const used = [];
  for (const [file, text] of suiteSources()) {
    for (const [n, line] of text.split('\n').entries()) {
      if (/^\s*(\/\/|\*)/.test(line)) continue;
      assert.doesNotMatch(line, /\b(?:port|control|udpPort): \d{4,5}\b/, `${file}:${n + 1} binds a fixed port; use e2ePort(...)`);
      assert.doesNotMatch(line, /\?\? \d{4,5}\)/, `${file}:${n + 1} defaults to a fixed port; use e2ePort(...)`);
      assert.doesNotMatch(line, /['"](?:https?|wss?):\/\/(?:127\.0\.0\.1|localhost):\d{4,5}['"]/, `${file}:${n + 1} names a fixed origin; build it from e2ePort(...)`);
      for (const m of line.matchAll(/e2ePort\((\d{4,5})\)/g)) used.push(Number(m[1]));
    }
  }
  const low = Math.min(...used);
  const high = Math.max(...used) + 2; // an SFU's UDP port is its signalling port + 2
  assert.ok(high - low < BLOCK_SIZE, `the suites span ${low}..${high}, more than one block of ${BLOCK_SIZE}`);
  assert.ok(low >= E2E_PORT_SPAN[0] && high <= E2E_PORT_SPAN[1], `the suites span ${low}..${high}, outside E2E_PORT_SPAN, so the block probe misses ports`);
  assert.ok(high + MAX_BLOCK * BLOCK_SIZE < 32768, 'the highest block stays under the ephemeral port range');
});

// ---------------------------------------------------------------- the command line

test('the command line', () => {
  const d = parseArgs([]);
  assert.equal(d.only, null);
  assert.equal(d.keepGoing, false);
  assert.equal(d.base, 1);
  const a = parseArgs(['--only', 'node,docs', '--keep-going', '--base=4', '--stress', '--e2e-jobs', '2', '--no-deps']);
  assert.deepEqual([a.only, a.keepGoing, a.base, a.stress, a.e2eJobs, a.deps], [['node', 'docs'], true, 4, true, 2, false]);
  assert.equal(parseArgs(['--changed']).changed, true);
  assert.throws(() => parseArgs(['--base', '25']), /--base takes an integer from 0 to 24/);
  assert.throws(() => parseArgs(['--only', 'go', '--changed']), /do not combine/);
  assert.throws(() => parseArgs(['--no-deps']), /needs --only/);
  assert.throws(() => parseArgs(['--frobnicate']), /unknown option/);
});

// ---------------------------------------------------------------- review round 1 (F1-F9)

test('F5: a workflow-level key ci-local does not read (defaults:, …) fails the sync check', () => {
  const problems = localMirrorProblems(REAL_CI.replace('\njobs:\n', '\ndefaults:\n  run:\n    shell: pwsh\njobs:\n'));
  assert.ok(problems.some((p) => p.includes('workflow-level key "defaults" is unmapped')), problems.join('\n'));
});

test('F1: a caller\'s database, test-switch and tool-override variables never reach a job', () => {
  const env = scrubEnv({
    PATH: '/bin', HOME: '/h', XDG_RUNTIME_DIR: '/run/user/1', PULSE_SERVER: 'unix:/p', GOPATH: '/g', GOFLAGS: '-mod=mod',
    DILLA_TEST_PG: 'postgres://prod', PGHOST: 'db', PGPASSWORD: 'x', PGDATABASE: 'd', DATABASE_URL: 'postgres://x', DILLA_METRICS_TOKEN: 't',
    DILLA_TESTKIT: '/x', DILLA_E2E_PORT: '1', DILLA_MUSIC_PORT: '2', GO: '/x/go', NPM: '/x/npm', CARGO: '/x/cargo', WASM_PACK: '/x/wp', CARGO_TARGET_DIR: '/elsewhere',
  });
  assert.deepEqual(Object.keys(env).sort(), ['GOFLAGS', 'GOPATH', 'HOME', 'PATH', 'PULSE_SERVER', 'XDG_RUNTIME_DIR']);
});

test('F1: every database or credential variable the Go code and the e2e helpers read is scrubbed', () => {
  const names = new Set();
  const walk = (dir, exts) => {
    for (const e of readdirSync(dir, { withFileTypes: true })) {
      if (['node_modules', 'target', 'dist', 'testdata'].includes(e.name)) continue;
      const p = join(dir, e.name);
      if (e.isDirectory()) walk(p, exts);
      else if (exts.some((x) => e.name.endsWith(x))) {
        const text = readFileSync(p, 'utf8');
        for (const m of text.matchAll(/os\.(?:Getenv|LookupEnv)\("([A-Z0-9_]+)"\)|process\.env\.([A-Z0-9_]+)/g)) names.add(m[1] ?? m[2]);
      }
    }
  };
  walk(join(ROOT, 'internal'), ['.go']);
  walk(join(ROOT, 'cmd'), ['.go']);
  walk(join(ROOT, 'e2e'), ['.ts', '.mjs']);
  const sensitive = [...names].filter((n) => /PG|DATABASE|TOKEN|SECRET|PASSWORD|DSN/.test(n));
  assert.ok(sensitive.includes('DILLA_TEST_PG'));
  for (const n of sensitive) assert.match(n, SCRUBBED_ENV, `${n} is read by the tests but would pass through to a job`);
  // go-postgres still gets its DSN, from ci.yml's step env (pointed at the throwaway container).
  assert.ok(job('go-postgres').steps.some((st) => /127\.0\.0\.1:5432/.test(st.env?.DILLA_TEST_PG ?? '')));
});

const scratch = () => mkdtempSync(join(tmpdir(), 'ci-local-test-'));
const tree = (dir) => (existsSync(dir) ? readdirSync(dir).sort().map((f) => `${f}=${readFileSync(join(dir, f), 'utf8')}`) : null);

test('F2/F7: a snapshot is restored whole, and swapped in by rename', () => {
  const root = scratch();
  const snaps = join(root, 'snaps');
  mkdirSync(join(root, 'internal/web/dist'), { recursive: true });
  writeFileSync(join(root, 'internal/web/dist/index.html'), 'placeholder');
  writeFileSync(join(root, 'internal/web/dist/dilla-manifest.json'), '{}');
  takeSnapshot(root, snaps, 'internal/web/dist');
  rmSync(join(root, 'internal/web/dist'), { recursive: true });
  mkdirSync(join(root, 'internal/web/dist'));
  writeFileSync(join(root, 'internal/web/dist/index.html'), 'the real client');
  writeFileSync(join(root, 'internal/web/dist/app.js'), 'js');
  assert.equal(restoreSnapshot(root, snaps, 'internal/web/dist'), true);
  assert.deepEqual(tree(join(root, 'internal/web/dist')), ['dilla-manifest.json={}', 'index.html=placeholder']);
  assert.deepEqual(readdirSync(join(root, 'internal/web')), ['dist'], 'no restore siblings are left');
  assert.deepEqual(readdirSync(snaps), [], 'the marker and the copy are gone');
  assert.equal(restoreSnapshot(root, snaps, 'internal/web/dist'), false, 'nothing pending: nothing restored');
  rmSync(root, { recursive: true, force: true });
});

test('F2: a snapshot a killed run left pending is restored at the next start, before a new snapshot is taken', () => {
  const root = scratch();
  const snaps = join(root, 'snaps');
  mkdirSync(join(root, 'd'));
  writeFileSync(join(root, 'd/index.html'), 'placeholder');
  takeSnapshot(root, snaps, 'd');
  // The run dies mid go-release: the real client is in the tree, plus a half-done restore copy beside it.
  writeFileSync(join(root, 'd/index.html'), 'the real client');
  mkdirSync(join(root, '.d.ci-local-restore-123-abcd'));
  // A naive next run would snapshot "the real client" as the original; takeSnapshot recovers first.
  takeSnapshot(root, snaps, 'd');
  assert.deepEqual(tree(join(root, 'd')), ['index.html=placeholder']);
  assert.deepEqual(readdirSync(root).sort(), ['d', 'snaps']);
  writeFileSync(join(root, 'd/index.html'), 'the real client again');
  assert.deepEqual(recoverSnapshots(root, snaps), ['d']);
  assert.deepEqual(tree(join(root, 'd')), ['index.html=placeholder']);
  assert.deepEqual(recoverSnapshots(root, snaps), []);
  rmSync(root, { recursive: true, force: true });
});

test('F2: a snapshot of a path that did not exist restores to no path; a run that died between the renames heals', () => {
  const root = scratch();
  const snaps = join(root, 'snaps');
  takeSnapshot(root, snaps, 'gone');
  mkdirSync(join(root, 'gone'));
  assert.deepEqual(recoverSnapshots(root, snaps), ['gone']);
  assert.equal(existsSync(join(root, 'gone')), false);
  mkdirSync(join(root, 'd'));
  writeFileSync(join(root, 'd/x'), 'orig');
  takeSnapshot(root, snaps, 'd');
  // Died after moving the target aside and before moving the restored copy in: the target is missing.
  mkdirSync(join(root, '.d.ci-local-old-1-ff'));
  rmSync(join(root, 'd'), { recursive: true });
  assert.deepEqual(recoverSnapshots(root, snaps), ['d']);
  assert.deepEqual(tree(join(root, 'd')), ['x=orig']);
  assert.deepEqual(readdirSync(root).sort(), ['d', 'snaps']);
  rmSync(root, { recursive: true, force: true });
});

/** A pid that existed and is gone now. */
function deadPid() {
  const r = spawnSync(process.execPath, ['-e', 'console.log(process.pid)'], { encoding: 'utf8' });
  return Number(r.stdout.trim());
}

test('F3: a lock is exclusive while its holder lives, and only its holder releases it', () => {
  const dir = scratch();
  const a = tryLock(dir, 'block-1', { job: 'a' });
  assert.ok(a !== null);
  assert.equal(tryLock(dir, 'block-1', { job: 'b' }), null, 'held by a live process');
  releaseLock({ path: a.path, token: 'not-ours' });
  assert.ok(existsSync(a.path), 'a stranger\'s release leaves the lock');
  releaseLock(a);
  const b = tryLock(dir, 'block-1', { job: 'b' });
  assert.ok(b !== null, 'free again after the holder released it');
  assert.deepEqual(readdirSync(dir), ['block-1.lock'], 'no temporary or stale files are left');
  rmSync(dir, { recursive: true, force: true });
});

test('F3: a dead holder\'s lock is taken over, and so is a lock whose pid now belongs to a later process', () => {
  const dir = scratch();
  const pid = deadPid();
  assert.equal(ownerAlive({ pid, start: '1' }), false);
  assert.ok(tryLock(dir, 'x', {}, { pid, start: '1' }) !== null);
  assert.ok(tryLock(dir, 'x') !== null, 'the dead holder\'s lock is taken over');
  // Pid reuse: the lock names this process's pid with another start time, so it is someone who died long ago.
  const dir2 = scratch();
  assert.ok(tryLock(dir2, 'y', {}, { pid: process.pid, start: '1' }) !== null);
  assert.equal(ownerAlive({ pid: process.pid, start: '1' }), existsSync('/proc/self/stat') ? false : true);
  assert.equal(ownerAlive({ pid: process.pid, start: processStart(process.pid) }), true);
  if (existsSync('/proc/self/stat')) assert.ok(tryLock(dir2, 'y') !== null, 'a reused pid does not keep a dead lock alive');
  for (const d of [dir, dir2]) {
    assert.ok(readdirSync(d).every((f) => f.endsWith('.lock')), readdirSync(d).join(', '));
    rmSync(d, { recursive: true, force: true });
  }
});

test('F3: two runs racing for one stale lock: exactly one wins', async () => {
  const dir = scratch();
  const pid = deadPid();
  assert.ok(tryLock(dir, 'z', {}, { pid, start: '1' }) !== null);
  const script = `import { tryLock } from ${JSON.stringify(join(ROOT, 'scripts', 'ci-local-fs.mjs'))};
    const l = tryLock(${JSON.stringify(dir)}, 'z');
    console.log(l === null ? 'lost' : 'won');
    setTimeout(() => {}, 1500);`;
  const { spawn } = await import('node:child_process');
  const run = () => new Promise((done) => {
    const c = spawn(process.execPath, ['--input-type=module', '-e', script], { stdio: ['ignore', 'pipe', 'inherit'] });
    let out = '';
    c.stdout.on('data', (d) => { out += d; });
    c.on('exit', () => done(out.trim()));
  });
  const results = await Promise.all(Array.from({ length: 6 }, run));
  assert.equal(results.filter((r) => r === 'won').length, 1, results.join(' '));
  rmSync(dir, { recursive: true, force: true });
});

test('F4: the port probe reports a port something already binds, and a free one as free', async () => {
  const srv = createServer();
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  const { port } = srv.address();
  assert.deepEqual(await busyPorts(port, port), [`${port}/tcp`]);
  await new Promise((r) => srv.close(r));
  assert.deepEqual(await busyPorts(port, port), []);
});

test('F8: the clean tree holds the listed files, not local build output, with a git index and node_modules linked', () => {
  const root = scratch();
  mkdirSync(join(root, 'scripts'));
  writeFileSync(join(root, 'scripts/a.mjs'), 'a');
  mkdirSync(join(root, 'packages/web/dist'), { recursive: true });
  writeFileSync(join(root, 'packages/web/dist/index.html'), 'built');
  mkdirSync(join(root, 'node_modules'));
  const dir = join(root, 'clean');
  buildCleanTree(root, dir, ['scripts/a.mjs', 'deleted-but-tracked.txt']);
  assert.equal(readFileSync(join(dir, 'scripts/a.mjs'), 'utf8'), 'a');
  assert.equal(existsSync(join(dir, 'packages')), false, 'build output stays out');
  assert.ok(lstatSync(join(dir, 'node_modules')).isSymbolicLink());
  const ls = spawnSync('git', ['ls-files'], { cwd: dir, encoding: 'utf8' });
  assert.deepEqual(ls.stdout.trim().split('\n'), ['scripts/a.mjs']);
  // Rebuilding unlinks node_modules, never follows it.
  writeFileSync(join(root, 'node_modules/keep'), 'k');
  buildCleanTree(root, dir, ['scripts/a.mjs']);
  assert.equal(readFileSync(join(root, 'node_modules/keep'), 'utf8'), 'k');
  rmSync(root, { recursive: true, force: true });
});

test('lock directory: per user, in the session runtime dir when there is one', () => {
  assert.equal(lockDirPath({ env: { XDG_RUNTIME_DIR: '/run/user/1000' }, tmp: '/tmp', uid: 1000 }), '/run/user/1000/dilla-ci-local-locks');
  assert.equal(lockDirPath({ env: {}, tmp: '/tmp', uid: 1000 }), '/tmp/dilla-ci-local-locks-1000');
});

test('lock directory: created 0700 and used as is; a later call accepts it unchanged', () => {
  const root = scratch();
  const dir = join(root, 'locks');
  assert.equal(lockDir(dir), dir);
  assert.equal(statSync(dir).mode & 0o777, 0o700);
  assert.equal(lockDir(dir), dir);
  assert.equal(statSync(dir).mode & 0o777, 0o700);
  rmSync(root, { recursive: true, force: true });
});

test('lock directory: a symlink, a group- or world-writable directory, a foreign owner and a file are refused, never chmodded', () => {
  const root = scratch();
  const target = join(root, 'elsewhere');
  mkdirSync(target, { mode: 0o700 });
  const link = join(root, 'link');
  symlinkSync(target, link);
  assert.throws(() => lockDir(link), /is a symlink/);
  const open = join(root, 'open');
  mkdirSync(open);
  chmodSync(open, 0o777);
  assert.throws(() => lockDir(open), /writable by group or others \(mode 777\)/);
  assert.equal(statSync(open).mode & 0o777, 0o777, 'refused, not chmodded');
  const foreign = join(root, 'foreign');
  mkdirSync(foreign, { mode: 0o700 });
  const asOther = (p) => ({ ...lstatSync(p), isSymbolicLink: () => false, isDirectory: () => true, uid: process.getuid() + 1, mode: lstatSync(p).mode });
  assert.throws(() => lockDir(foreign, { stat: asOther }), /is owned by uid \d+, not \d+/);
  const file = join(root, 'file');
  writeFileSync(file, '');
  assert.throws(() => lockDir(file), /is not a directory/);
  rmSync(root, { recursive: true, force: true });
});
