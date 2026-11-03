import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import {
  discover, expectedSeconds, invocations, isTestName, listProblems, loadTimings, parseListJSON, partition, partitionProblems,
  POSTGRES_API_RUN, ROOT, runAll, scanTests, SETS,
} from './go-shard.mjs';

const SCRIPT = fileURLToPath(new URL('./go-shard.mjs', import.meta.url));
const TIMINGS = { defaults: { packageSeconds: 2, testSeconds: 1 }, splitAboveSeconds: 100, packages: {}, tests: {} };

/** n packages of a few tests each, plus one slow package of `big` tests. */
function fakeSet(big = 40) {
  const packages = [];
  for (let i = 0; i < 25; i++) packages.push({ pkg: `./p${i}`, importPath: `m/p${i}`, run: null, tests: Array.from({ length: 3 }, (_, j) => `TestP${i}N${j}`) });
  packages.push({ pkg: './slow', importPath: 'm/slow', run: null, tests: Array.from({ length: big }, (_, j) => `TestSlow${j}`) });
  return packages;
}

const timed = (extra = {}) => ({ ...TIMINGS, packages: { s: { './slow': 900, './p0': 60, './p1': 40, ...extra } } });

test('go test names: Test, Fuzz and Example functions, never TestMain, a lower-case suffix or a method', () => {
  for (const name of ['Test', 'TestX', 'Test_x', 'Test1', 'FuzzParse', 'Example', 'ExampleFoo_bar']) assert.ok(isTestName(name), name);
  for (const name of ['TestMain', 'Testing', 'Fuzzy', 'Examples', 'BenchmarkX', 'helper']) assert.ok(!isTestName(name), name);
  const source = [
    'package x',
    'func TestA(t *testing.T) {',
    '\tt.Run("sub", func(t *testing.T) {})',
    '}',
    'func (s *suite) TestMethod(t *testing.T) {}',
    'func TestMain(m *testing.M) {}',
    'func testHelper(t *testing.T) {}',
    'func BenchmarkB(b *testing.B) {}',
    'func FuzzC(f *testing.F) {}',
    'func ExampleD() {}',
    '// func TestCommented(t *testing.T) {}',
    'func TestE (t *testing.T) {}',
  ].join('\n');
  assert.deepEqual(scanTests(source), ['TestA', 'FuzzC', 'ExampleD', 'TestE']);
});

test('every test and package lands in exactly one shard, for any shard count', () => {
  const packages = fakeSet();
  for (const count of [1, 2, 3, 4, 6, 7]) {
    const shards = partition('s', packages, count, timed());
    assert.equal(shards.length, count);
    assert.deepEqual(partitionProblems(packages, shards), [], `count ${count}`);
    const seen = [];
    for (const s of shards) {
      for (const w of s.whole) seen.push(...packages.find((p) => p.pkg === w.pkg).tests.map((t) => `${w.pkg} ${t}`));
      for (const piece of s.split) seen.push(...piece.tests.map((t) => `${piece.pkg} ${t}`));
    }
    const all = packages.flatMap((p) => p.tests.map((t) => `${p.pkg} ${t}`));
    assert.equal(seen.length, all.length, 'no test runs twice');
    assert.deepEqual(new Set(seen), new Set(all), 'every test runs');
  }
});

test('only a package over the split threshold is split by test name; the rest run whole', () => {
  const shards = partition('s', fakeSet(), 3, timed());
  assert.ok(shards.every((s) => s.split.length === 1 && s.split[0].pkg === './slow'), 'the slow package has a piece in every shard');
  assert.ok(shards.every((s) => s.whole.every((w) => w.pkg !== './slow')));
  assert.deepEqual(partition('s', fakeSet(), 3, timed({ './slow': 90 })).flatMap((s) => s.split), [], 'under the threshold nothing is split');
});

test('the partition balances on the timing table and is deterministic', () => {
  const a = partition('s', fakeSet(), 3, timed());
  const b = partition('s', [...fakeSet()].reverse(), 3, timed());
  assert.deepEqual(a, b, 'input order does not change the shards');
  const loads = a.map((s) => s.seconds);
  assert.ok(Math.max(...loads) - Math.min(...loads) <= 60, `balanced: ${loads}`);
  // A test with its own timing weighs what the table says; the package's other tests share the rest.
  const heavy = partition('s', fakeSet(), 3, { ...timed(), tests: { s: { './slow TestSlow7': 600 } } });
  const holder = heavy.find((s) => s.split.some((piece) => piece.tests.includes('TestSlow7')));
  assert.ok(holder.split.find((piece) => piece.pkg === './slow').tests.length < 5, 'the heavy test\'s shard takes few other slow tests');
  assert.deepEqual(partitionProblems(fakeSet(), heavy), []);
});

test('a package or test without a timing gets the default', () => {
  const packages = [{ pkg: './a', importPath: 'm/a', run: null, tests: ['TestA'] }, { pkg: './b', importPath: 'm/b', run: null, tests: ['TestB'] }];
  const shards = partition('none', packages, 2, TIMINGS);
  assert.deepEqual(shards.map((s) => s.seconds), [2, 2]);
});

test('the union check names a missing, duplicated, doubly-run or foreign test', () => {
  const packages = fakeSet();
  const good = partition('s', packages, 3, timed());
  const clone = () => structuredClone(good);
  let shards = clone();
  shards[0].split[0].tests.pop();
  assert.ok(partitionProblems(packages, shards).some((p) => p.includes('is in no shard')));
  shards = clone();
  shards[1].split[0].tests.push(shards[0].split[0].tests[0]);
  assert.ok(partitionProblems(packages, shards).some((p) => p.includes('is in shards 1, 2')));
  shards = clone();
  shards[2].whole.push({ pkg: './slow', run: null, seconds: 1 });
  assert.ok(partitionProblems(packages, shards).some((p) => p.includes('whole in shard 3 and split')));
  shards = clone();
  shards[0].whole = shards[0].whole.filter((w) => w.pkg !== shards[0].whole[0].pkg);
  assert.ok(partitionProblems(packages, shards).some((p) => p.endsWith('is in no shard')));
  shards = clone();
  shards[0].split[0].tests.push('TestNotThere');
  shards[1].whole.push({ pkg: './elsewhere', run: null, seconds: 1 });
  const problems = partitionProblems(packages, shards);
  assert.ok(problems.some((p) => p.includes('TestNotThere')) && problems.some((p) => p.includes('./elsewhere')), problems.join('\n'));
});

test('a shard runs whole packages together and each split piece by an anchored name list', () => {
  const shard = {
    shard: 1, seconds: 0,
    whole: [{ pkg: './a', run: null, seconds: 5 }, { pkg: './b', run: null, seconds: 5 }, { pkg: './api', run: 'AFK|Marker', seconds: 3 }],
    split: [{ pkg: './slow', tests: ['TestA', 'TestB'], seconds: 50 }],
  };
  assert.deepEqual(invocations(shard, ['-race', '-timeout', '20m']), [
    { seconds: 50, args: ['test', '-race', '-timeout', '20m', '-run', '^(TestA|TestB)$', './slow'] },
    { seconds: 10, args: ['test', '-race', '-timeout', '20m', './a', './b'] },
    { seconds: 3, args: ['test', '-race', '-timeout', '20m', '-run', 'AFK|Marker', './api'] },
  ]);
  // The pattern has no `/`, so go test runs every subtest of a selected test with its parent.
  const re = new RegExp('^(TestA|TestB)$');
  assert.ok(re.test('TestA') && !re.test('TestAB') && !re.test('XTestA'));
});

test('go test -list output is checked against the assignment in both directions', () => {
  const lines = [
    { Action: 'start', Package: 'm/slow' },
    { Action: 'output', Package: 'm/slow', Output: 'TestA\n' },
    { Action: 'output', Package: 'm/slow', Output: 'TestNew\n' },
    { Action: 'output', Package: 'm/slow', Output: 'BenchmarkX\n' },
    { Action: 'output', Package: 'm/slow', Output: 'ok  \tm/slow\t0.01s\n' },
    { Action: 'output', Package: 'm/api', Output: 'TestAFKOne\n' },
    { Action: 'output', Package: 'm/api', Output: 'TestOther\n' },
  ].map((e) => JSON.stringify(e)).join('\n');
  const listed = parseListJSON(`${lines}\nnot json\n`);
  assert.deepEqual(listed, { 'm/slow': ['TestA', 'TestNew'], 'm/api': ['TestAFKOne', 'TestOther'] });
  const packages = [
    { pkg: './slow', importPath: 'm/slow', run: null, tests: ['TestA', 'TestGone', 'ExampleNoOutput'] },
    { pkg: './api', importPath: 'm/api', run: 'AFK', tests: ['TestAFKOne'] },
  ];
  assert.deepEqual(listProblems(packages, listed), [
    'TestNew (./slow) is listed by go test but assigned to no shard',
    'TestGone (./slow) is assigned but go test does not list it',
  ]);
});

test('discovery keeps test packages, applies the exclusions and the -run filter, and refuses an overlap', () => {
  const rows = {
    './...': [
      { importPath: 'm', dir: ROOT, files: [] },
      { importPath: 'm/internal/ds', dir: `${ROOT}/internal/ds`, files: ['a_test.go'] },
      { importPath: 'm/internal/x', dir: `${ROOT}/internal/x`, files: ['a_test.go'] },
      { importPath: 'm/internal/x/sub', dir: `${ROOT}/internal/x/sub`, files: [] },
    ],
    './internal/api/...': [{ importPath: 'm/internal/api', dir: `${ROOT}/internal/api`, files: ['a_test.go', 'b_test.go'] }],
    './internal/store/...': [{ importPath: 'm/internal/store', dir: `${ROOT}/internal/store`, files: ['a_test.go'] }],
    './internal/ops/...': [{ importPath: 'm/internal/ops', dir: `${ROOT}/internal/ops`, files: ['a_test.go'] }],
  };
  const read = (file) => (file.endsWith('b_test.go') ? 'func TestAFKBackfill(t *testing.T) {}\nfunc TestMarkerX(t *testing.T) {}\n' : 'func TestA(t *testing.T) {}\nfunc TestUnrelated(t *testing.T) {}\n');
  const list = (pattern) => rows[pattern];
  assert.deepEqual(discover('go', { list, read }).map((p) => p.pkg), ['./internal/x']);
  const pg = discover('go-postgres', { list, read });
  assert.deepEqual(pg.map((p) => [p.pkg, p.run, p.tests]), [
    ['./internal/api', POSTGRES_API_RUN, ['TestAFKBackfill', 'TestMarkerX']],
    ['./internal/ops', null, ['TestA', 'TestUnrelated']],
    ['./internal/store', null, ['TestA', 'TestUnrelated']],
  ]);
  assert.throws(() => discover('go-postgres', { list: () => rows['./internal/store/...'], read }), /matched by two package entries/);
  assert.throws(() => discover('nope', { list, read }), /unknown set/);
});

test('the sets keep the module covered: go is ./... minus exactly internal/ds and internal/api', () => {
  assert.deepEqual(SETS.go.packages, [{ pattern: './...', exclude: ['./internal/ds', './internal/api'] }]);
  assert.deepEqual(SETS['go-api'].packages, [{ pattern: './internal/api/...' }]);
  assert.deepEqual(SETS['go-ds'].packages, [{ pattern: './internal/ds/...' }]);
  assert.deepEqual(SETS['go-harness'].packages, [{ pattern: './internal/testkit/...' }]);
  assert.deepEqual(SETS['go-postgres'].packages.map((p) => p.pattern), ['./internal/store/...', './internal/ops/...', './internal/api/...']);
});

test('the committed timing table is well formed and names only sets that exist', () => {
  const timings = loadTimings();
  assert.ok(timings.defaults.packageSeconds > 0 && timings.defaults.testSeconds > 0 && timings.splitAboveSeconds > 0);
  for (const set of [...Object.keys(timings.packages), ...Object.keys(timings.tests), ...Object.keys(timings.parallelism)]) assert.ok(set in SETS, set);
  for (const table of Object.values(timings.packages)) {
    for (const [pkg, seconds] of Object.entries(table)) assert.ok(/^\.\/[\w./-]+$/.test(pkg) && seconds > 0, pkg);
  }
  const shard = partition('go', fakeSet(), 2, { ...timings, splitAboveSeconds: 100 })[0];
  assert.ok(expectedSeconds('go', shard, timings) < shard.seconds, 'the go set runs packages side by side');
});

test('the CLI refuses an unknown set, a shard out of range and a caller-chosen -run', () => {
  for (const args of [['run', 'nope', '1'], ['run', 'go', '0'], ['run', 'go', String(SETS.go.shards + 1)], ['run', 'go', '1', '--', '-run', 'X'], ['bogus']]) {
    const result = spawnSync(process.execPath, [SCRIPT, ...args], { encoding: 'utf8' });
    assert.equal(result.status, 1, args.join(' '));
    assert.match(result.stderr, /go-shard: /);
  }
});

test('a shard runs its commands side by side and fails when any one fails', async () => {
  const sleep = (ms, code) => ({ seconds: ms / 1000, args: ['-e', `setTimeout(() => process.exit(${code}), ${ms})`] });
  const started = Date.now();
  assert.equal(await runAll([sleep(400, 0), sleep(400, 0), sleep(400, 0)], 3, process.execPath), 0);
  assert.ok(Date.now() - started < 1100, 'three 400 ms commands at once take well under 1.2 s');
  assert.equal(await runAll([sleep(10, 0), sleep(10, 3), sleep(10, 0)], 2, process.execPath), 1);
  assert.equal(await runAll([], 4, process.execPath), 0);
});
