// Splits a Go test job into N shards by package and, for the slow packages, by top-level test name.
//
// Every shard computes the same partition from the same inputs (`go list` and the test files it names), so the
// shards of one matrix agree without talking to each other. The partition is balanced on the measured times in
// scripts/go-shard-timings.json: a package at or under `splitAboveSeconds` is one unit and runs whole (no -run, so
// nothing in it can be missed), a slower one is split into its top-level tests, each run by name with
// `-run '^(TestA|TestB)$'` (subtests stay with their parent: the pattern has no `/`). Units are assigned longest
// first to the least-loaded shard.
//
// Before a shard runs anything it checks the whole partition: every package of the set is either whole in exactly
// one shard or split with every one of its tests in exactly one shard, and no shard names anything else. It then
// asks the compiled test binaries of its split packages for their tests (`go test -list`) and fails if one is not
// assigned, so a test the source scan missed cannot silently run nowhere.
//
//   node scripts/go-shard.mjs plan [SET...]                        the shards of each set with their expected seconds
//   node scripts/go-shard.mjs run SET SHARD [--dry-run] -- FLAGS   run shard SHARD (1-based) of SET with go test FLAGS
import { readFileSync } from 'node:fs';
import { spawn, spawnSync } from 'node:child_process';
import { availableParallelism } from 'node:os';
import { dirname, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

export const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
export const TIMINGS_FILE = join(ROOT, 'scripts', 'go-shard-timings.json');

/** web3.9: the AFK backfill and channel-parent rules are the internal/api tests with a Postgres leg. */
export const POSTGRES_API_RUN = 'AFK|Afk|HiddenCategory|HiddenParent|ParentAnywhere|Marker|ConcurrentEnsures|ReadsThePointer';

/**
 * The sharded Go jobs of server.yml, by job name: how many shards each matrix has and which packages it tests. `go` is
 * the module minus exactly internal/ds and internal/api (go-ds and go-api run them; the union is the whole module).
 * An entry's `run` keeps only the tests whose name it matches, as `go test -run` would.
 */
export const SETS = {
  go: { shards: 4, packages: [{ pattern: './...', exclude: ['./internal/ds', './internal/api'] }] },
  'go-api': { shards: 3, packages: [{ pattern: './internal/api/...' }] },
  'go-ds': { shards: 3, packages: [{ pattern: './internal/ds/...' }] },
  'go-harness': { shards: 3, packages: [{ pattern: './internal/testkit/...' }] },
  'go-postgres': {
    shards: 2,
    packages: [{ pattern: './internal/store/...' }, { pattern: './internal/ops/...' }, { pattern: './internal/api/...', run: POSTGRES_API_RUN }],
  },
};

const PREFIXES = ['Test', 'Fuzz', 'Example'];

/** Whether `go test` treats a top-level function name as a test, fuzz test or example (`TestMain` is none). */
export function isTestName(name) {
  if (name === 'TestMain') return false;
  return PREFIXES.some((prefix) => name.startsWith(prefix) && (name.length === prefix.length || !/[a-z]/.test(name[prefix.length])));
}

/** The top-level test, fuzz and example functions a Go test file declares, in source order. */
export function scanTests(source) {
  return [...source.matchAll(/^func\s+([A-Za-z_]\w*)\s*\(/gm)].map((m) => m[1]).filter(isTestName);
}

/** `go test -json -list` output → { importPath: [names] } (benchmarks left out: -run never runs them). */
export function parseListJSON(text) {
  const found = {};
  for (const line of text.split('\n')) {
    if (!line.startsWith('{')) continue;
    const event = JSON.parse(line);
    if (event.Action !== 'output' || !event.Package) continue;
    const name = String(event.Output ?? '').trim();
    if (/^(?:Test|Fuzz|Example)\w*$/.test(name) && isTestName(name)) (found[event.Package] ??= []).push(name);
  }
  return found;
}

export function loadTimings(file = TIMINGS_FILE) {
  return JSON.parse(readFileSync(file, 'utf8'));
}

function goList(pattern, race) {
  const format = '{{.ImportPath}}\t{{.Dir}}\t{{join .TestGoFiles " "}}\t{{join .XTestGoFiles " "}}';
  const result = spawnSync('go', ['list', ...(race ? ['-race'] : []), '-f', format, pattern], { cwd: ROOT, encoding: 'utf8', maxBuffer: 64 << 20 });
  if (result.status !== 0) throw new Error(`go list ${pattern} failed:\n${result.stderr}`);
  return result.stdout.split('\n').filter(Boolean).map((line) => {
    const [importPath, dir, tests, xtests] = line.split('\t');
    return { importPath, dir, files: [...tests.split(' '), ...xtests.split(' ')].filter(Boolean) };
  });
}

/**
 * The packages of a set with their tests: [{ pkg: './x/y', importPath, run, tests: [names] }], sorted by pkg. Only
 * packages with test files are kept, and for an entry with `run` only those with a matching test.
 */
export function discover(setName, { race = true, list = goList, read = (file) => readFileSync(file, 'utf8') } = {}) {
  const set = SETS[setName];
  if (set === undefined) throw new Error(`unknown set "${setName}"; known: ${Object.keys(SETS).join(', ')}`);
  const found = new Map();
  for (const entry of set.packages) {
    const filter = entry.run === undefined ? null : new RegExp(entry.run);
    for (const row of list(entry.pattern, race)) {
      const rel = relative(ROOT, row.dir).split(sep).join('/');
      const pkg = rel === '' ? '.' : `./${rel}`;
      if ((entry.exclude ?? []).includes(pkg) || row.files.length === 0) continue;
      const all = [...new Set(row.files.flatMap((file) => scanTests(read(join(row.dir, file)))))].sort();
      const tests = filter === null ? all : all.filter((name) => filter.test(name));
      if (filter !== null && tests.length === 0) continue;
      if (found.has(pkg)) throw new Error(`${setName}: ${pkg} is matched by two package entries`);
      found.set(pkg, { pkg, importPath: row.importPath, run: entry.run ?? null, tests });
    }
  }
  return [...found.values()].sort((a, b) => a.pkg.localeCompare(b.pkg));
}

/**
 * Assigns the packages of a set to `count` shards. Returns [{ shard, seconds, whole: [{ pkg, run, seconds }],
 * split: [{ pkg, tests: [names], seconds }] }], deterministic for the same inputs.
 */
export function partition(setName, packages, count, timings) {
  if (!Number.isInteger(count) || count < 1) throw new Error(`shard count must be a positive integer, not ${count}`);
  const measured = timings.packages?.[setName] ?? {};
  const perTest = timings.tests?.[setName] ?? {};
  const { packageSeconds, testSeconds } = timings.defaults;
  const units = [];
  for (const p of packages) {
    const seconds = measured[p.pkg] ?? packageSeconds;
    if (seconds > timings.splitAboveSeconds && p.tests.length > 1) {
      const known = p.tests.filter((name) => perTest[`${p.pkg} ${name}`] !== undefined);
      const rest = Math.max(seconds - known.reduce((sum, name) => sum + perTest[`${p.pkg} ${name}`], 0), 0);
      const share = p.tests.length === known.length ? 0 : Math.max(rest / (p.tests.length - known.length), testSeconds);
      for (const name of p.tests) units.push({ key: `${p.pkg} ${name}`, pkg: p.pkg, name, seconds: perTest[`${p.pkg} ${name}`] ?? share });
    } else {
      units.push({ key: p.pkg, pkg: p.pkg, name: null, run: p.run, seconds });
    }
  }
  units.sort((a, b) => b.seconds - a.seconds || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
  const shards = Array.from({ length: count }, (_, i) => ({ shard: i + 1, seconds: 0, whole: [], split: new Map() }));
  for (const unit of units) {
    const target = shards.reduce((best, s) => (s.seconds < best.seconds ? s : best));
    target.seconds += unit.seconds;
    if (unit.name === null) target.whole.push({ pkg: unit.pkg, run: unit.run, seconds: unit.seconds });
    else {
      const piece = target.split.get(unit.pkg) ?? { pkg: unit.pkg, tests: [], seconds: 0 };
      piece.tests.push(unit.name);
      piece.seconds += unit.seconds;
      target.split.set(unit.pkg, piece);
    }
  }
  return shards.map((s) => ({
    shard: s.shard,
    seconds: s.seconds,
    whole: s.whole.sort((a, b) => a.pkg.localeCompare(b.pkg)),
    split: [...s.split.values()].map((piece) => ({ ...piece, tests: piece.tests.sort() })).sort((a, b) => a.pkg.localeCompare(b.pkg)),
  }));
}

/**
 * The union check: every package is whole in exactly one shard, or split with each of its tests in exactly one
 * shard and whole in none; no shard names a package or test outside the set. Returns the problems (empty when sound).
 */
export function partitionProblems(packages, shards) {
  const problems = [];
  const byPkg = new Map(packages.map((p) => [p.pkg, p]));
  const whole = new Map();
  const tests = new Map();
  for (const s of shards) {
    for (const w of s.whole) whole.set(w.pkg, [...(whole.get(w.pkg) ?? []), s.shard]);
    for (const piece of s.split) {
      for (const name of piece.tests) tests.set(`${piece.pkg} ${name}`, [...(tests.get(`${piece.pkg} ${name}`) ?? []), s.shard]);
    }
  }
  for (const pkg of whole.keys()) if (!byPkg.has(pkg)) problems.push(`shards run ${pkg}, which is not in the set`);
  for (const key of tests.keys()) {
    const [pkg, name] = key.split(' ');
    if (!byPkg.get(pkg)?.tests.includes(name)) problems.push(`shards run ${name} of ${pkg}, which is not in the set`);
  }
  for (const p of packages) {
    const at = whole.get(p.pkg) ?? [];
    const pieces = p.tests.map((name) => [name, tests.get(`${p.pkg} ${name}`) ?? []]);
    const split = pieces.some(([, where]) => where.length > 0);
    if (at.length > 1) problems.push(`${p.pkg} runs whole in shards ${at.join(', ')}`);
    if (at.length === 1 && split) problems.push(`${p.pkg} runs whole in shard ${at[0]} and split by test name too`);
    if (at.length === 0 && !split) problems.push(`${p.pkg} is in no shard`);
    if (at.length === 0 && split) {
      for (const [name, where] of pieces) {
        if (where.length === 0) problems.push(`${name} (${p.pkg}) is in no shard`);
        if (where.length > 1) problems.push(`${name} (${p.pkg}) is in shards ${where.join(', ')}`);
      }
    }
  }
  return problems;
}

/** The `go test` argument lists of one shard, longest expected first. */
export function invocations(shard, flags, packageParallelism = null) {
  // -p bounds how many test binaries one invocation runs at once; with runAll's limit it caps the shard's total.
  if (packageParallelism !== null && !flags.some((f) => f === '-p' || f.startsWith('-p='))) flags = [...flags, `-p=${packageParallelism}`];
  const out = [];
  const groups = new Map();
  for (const w of shard.whole) {
    const group = groups.get(w.run) ?? { run: w.run, pkgs: [], seconds: 0 };
    group.pkgs.push(w.pkg);
    group.seconds += w.seconds;
    groups.set(w.run, group);
  }
  for (const g of groups.values()) out.push({ seconds: g.seconds, args: ['test', ...flags, ...(g.run === null ? [] : ['-run', g.run]), ...g.pkgs] });
  for (const piece of shard.split) out.push({ seconds: piece.seconds, args: ['test', ...flags, '-run', `^(${piece.tests.join('|')})$`, piece.pkg] });
  return out.sort((a, b) => b.seconds - a.seconds);
}

/**
 * Compares the scanned tests of the split packages with what their compiled test binaries list. Returns the
 * problems: a listed test that is in no shard fails the shard; so does a scanned one the binary lacks (an example
 * without an output comment is not a test and is let through).
 */
export function listProblems(packages, listed) {
  const problems = [];
  for (const p of packages) {
    const filter = p.run === null ? null : new RegExp(p.run);
    const have = new Set((listed[p.importPath] ?? []).filter((name) => filter === null || filter.test(name)));
    const want = new Set(p.tests);
    for (const name of have) if (!want.has(name)) problems.push(`${name} (${p.pkg}) is listed by go test but assigned to no shard`);
    for (const name of want) if (!have.has(name) && !name.startsWith('Example')) problems.push(`${name} (${p.pkg}) is assigned but go test does not list it`);
  }
  return problems;
}

export function listTests(packages, flags) {
  if (packages.length === 0) return {};
  const args = ['test', ...flags.filter((f) => f === '-race' || /^-tags[= ]/.test(f)), '-json', '-list', '.', ...packages.map((p) => p.pkg)];
  const result = spawnSync('go', args, { cwd: ROOT, encoding: 'utf8', maxBuffer: 256 << 20, stdio: ['ignore', 'pipe', 'pipe'] });
  if (result.status !== 0) throw new Error(`go ${args.join(' ')} failed:\n${result.stdout}\n${result.stderr}`);
  return parseListJSON(result.stdout);
}

function quote(arg) {
  return /^[\w./=:,@%+-]+$/.test(arg) ? arg : `'${arg.replaceAll("'", "'\\''")}'`;
}

function minutes(seconds) {
  return `${(seconds / 60).toFixed(1)} min`;
}

/** Runs the commands, at most `limit` at a time in the given order (invocations() puts the longest first); each one's output is printed whole when it ends. Returns the failures. */
export async function runAll(commands, limit, program = 'go') {
  let failed = 0;
  let next = 0;
  const worker = async () => {
    while (next < commands.length) {
      const command = commands[next++];
      const line = `${program} ${command.args.map(quote).join(' ')}`;
      process.stdout.write(`go-shard: start (${minutes(command.seconds)} expected): ${line.length > 300 ? `${line.slice(0, 300)}…` : line}\n`);
      const started = Date.now();
      const code = await new Promise((done) => {
        const child = spawn(program, command.args, { cwd: ROOT, stdio: ['ignore', 'pipe', 'pipe'] });
        const chunks = [];
        child.stdout.on('data', (c) => chunks.push(c));
        child.stderr.on('data', (c) => chunks.push(c));
        child.on('error', (error) => { chunks.push(Buffer.from(`${error}\n`)); done(1); });
        child.on('close', (status) => { process.stdout.write(Buffer.concat(chunks)); done(status ?? 1); });
      });
      process.stdout.write(`go-shard: ${code === 0 ? 'ok' : `FAILED (exit ${code})`} after ${minutes((Date.now() - started) / 1000)}: ${line.slice(0, 120)}…\n`);
      if (code !== 0) failed++;
    }
  };
  await Promise.all(Array.from({ length: Math.min(limit, commands.length) }, worker));
  return failed;
}

/** A shard's expected wall seconds: its package time over the set's measured parallelism, never under its longest test binary. */
export function expectedSeconds(setName, shard, timings) {
  const longest = Math.max(0, ...shard.whole.map((w) => w.seconds), ...shard.split.map((piece) => piece.seconds));
  return Math.max(shard.seconds / (timings.parallelism?.[setName] ?? 1), longest);
}

function describe(setName, shards, timings) {
  const lines = [`${setName}: ${shards.length} shards`];
  for (const s of shards) {
    const pieces = s.split.map((piece) => `${piece.pkg} ${piece.tests.length} tests`).join(', ');
    lines.push(`  shard ${s.shard}: ~${minutes(expectedSeconds(setName, s, timings))} expected (${Math.round(s.seconds)} s of measured package time); ${s.whole.length} whole packages${pieces ? `; split: ${pieces}` : ''}`);
  }
  return lines.join('\n');
}

/** The checked partition of a set: throws when the union check finds a problem. */
export function plan(setName, options = {}) {
  const timings = options.timings ?? loadTimings();
  const packages = discover(setName, options);
  const shards = partition(setName, packages, options.shards ?? SETS[setName].shards, timings);
  const problems = partitionProblems(packages, shards);
  if (problems.length > 0) throw new Error(`${setName}: the shards do not cover the set exactly once:\n  ${problems.join('\n  ')}`);
  return { packages, shards };
}

async function main(argv) {
  const [command, ...rest] = argv;
  if (command === 'plan') {
    const timings = loadTimings();
    for (const setName of rest.length > 0 ? rest : Object.keys(SETS)) process.stdout.write(`${describe(setName, plan(setName, { timings }).shards, timings)}\n`);
    return 0;
  }
  if (command === 'run') {
    const dash = rest.indexOf('--');
    const head = dash < 0 ? rest : rest.slice(0, dash);
    const flags = dash < 0 ? [] : rest.slice(dash + 1);
    const dryRun = head.includes('--dry-run');
    const [setName, shardText] = head.filter((a) => a !== '--dry-run');
    const set = SETS[setName];
    const index = Number(shardText);
    if (set === undefined || !Number.isInteger(index) || index < 1 || index > set.shards) {
      throw new Error(`usage: go-shard.mjs run SET SHARD [--dry-run] -- FLAGS (SET one of ${Object.keys(SETS).join(', ')}; SHARD 1..its count)`);
    }
    if (flags.some((f) => f === '-run' || f.startsWith('-run=') || f.startsWith('-list'))) throw new Error('go-shard.mjs chooses -run itself');
    const timings = loadTimings();
    const { packages, shards } = plan(setName, { race: flags.includes('-race'), timings });
    process.stdout.write(`${describe(setName, shards, timings)}\ngo-shard: the ${shards.length} shards cover all ${packages.length} packages of ${setName} exactly once\n`);
    const shard = shards[index - 1];
    // A CPU budget like the old single job's (go test -p = CPUs): half the CPUs as side-by-side invocations, each with
    // -p so their product is the CPU count. CPUs x CPUs race binaries starved a timing-sensitive test (PR #144's first
    // run: the sandbox probe reaping test in go-3).
    const cpus = Math.max(1, availableParallelism());
    const sideBySide = Math.max(1, Math.floor(cpus / 2));
    const commands = invocations(shard, flags, Math.max(1, Math.floor(cpus / sideBySide)));
    if (dryRun) {
      for (const c of commands) process.stdout.write(`go ${c.args.map(quote).join(' ')}\n`);
      return 0;
    }
    const splitPkgs = packages.filter((p) => shard.split.some((piece) => piece.pkg === p.pkg));
    const problems = listProblems(splitPkgs, listTests(splitPkgs, flags));
    if (problems.length > 0) throw new Error(`${setName}: the scan and go test -list disagree:\n  ${problems.join('\n  ')}`);
    process.stdout.write(`go-shard: go test -list agrees with the scan for the ${splitPkgs.length} split packages of shard ${index}\n`);
    const failed = await runAll(commands, sideBySide);
    return failed === 0 ? 0 : 1;
  }
  throw new Error('usage: go-shard.mjs plan [SET...] | go-shard.mjs run SET SHARD [--dry-run] -- GO_TEST_FLAGS');
}

if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
  main(process.argv.slice(2)).then((code) => { process.exitCode = code; }, (error) => {
    process.stderr.write(`go-shard: ${error.message}\n`);
    process.exitCode = 1;
  });
}
