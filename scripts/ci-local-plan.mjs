import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { parseYaml } from './ci-local-yaml.mjs';

/**
 * The pure half of `npm run ci:local` (scripts/ci-local.mjs runs it): .github/workflows/ci.yml read into jobs, each
 * step mapped onto what this machine does instead of the GitHub runner, the needs graph, the job selection of --only and
 * --changed, and the port blocks the browser jobs bind. Nothing here touches the tree, spawns a process or reads the
 * environment, so scripts/check-ci-workflow.mjs runs `localMirrorProblems` in the docs job: a job, a step, an action, an
 * `if:` or an expression that ci-local cannot map fails `npm run check:ci`, and the two cannot drift.
 */

/**
 * Every job of ci.yml, by name. A job missing here (or listed here but gone from ci.yml) is a problem. Fields:
 * - weight: the cores it keeps busy (a GitHub runner has 4); the scheduler runs jobs while their sum fits the CPU budget.
 * - kind: 'areas' (the `changes` job: ci-local computes the areas itself), 'aggregate' (ci-ok: the summary is its
 *   verdict) or 'steps' (default: the steps run).
 * - group: 'go' and 'e2e' jobs are the ones --stress squeezes.
 * - ports: 'block' (the core, web and voice suites: each run gets its own DILLA_E2E_PORT_OFFSET block), 'fixed' (suites
 *   whose ports are hard-coded: one per-user lock, so lanes queue instead of clashing) or 'ui' (Storybook's servers,
 *   moved by the block's ui offset).
 * - locks: named locks held for the whole job (jobs that regenerate or check tracked files serialise on 'tracked-tree').
 * - exclusive: nothing else runs beside it; snapshot: paths copied aside before it and put back after it, as it was.
 * - cleanTree: run steps (exact commands) that run in a copy of the tracked and untracked-unignored files, as on a fresh
 *   checkout: a test that looks at the disk must not see build output (packages/web/dist, …) the runner never has.
 */
export const JOB_MAP = {
  changes: { kind: 'areas', weight: 0 },
  'ci-watchdog': { kind: 'aggregate', weight: 0, skip: 'GitHub run cancellation is replaced by the local fail-fast scheduler' },
  'ci-work-complete': { kind: 'aggregate', weight: 0 },
  'linux-sandbox-foundation': { weight: 2, skip: 'requires disposable Ubuntu 22.04/24.04 native runners with privileged kernel fixtures' },
  desktop: { weight: 4, skip: 'requires the Linux, Windows and macOS native platform acceptance runners in desktop.yml' },
  'core-integration': { weight: 2, group: 'e2e', ports: 'block' },
  docs: { weight: 1 },
  // ci-areas.test.mjs's reader scan skips targets that do not exist: on a runner packages/web/dist never does when it runs.
  node: { weight: 2, locks: ['tracked-tree'], cleanTree: ['npm run test:ci-check'] },
  ui: { weight: 3, ports: 'ui' },
  'rust-wasm-node': { weight: 4 },
  'rust-native': { weight: 4 },
  'rust-wasi': { weight: 2 },
  vectors: { weight: 2, locks: ['tracked-tree'] },
  'browser-spike': { weight: 3, group: 'e2e', ports: 'fixed' },
  'browser-media': { weight: 3, group: 'e2e', ports: 'fixed' },
  'web-build': { weight: 4 },
  deny: { weight: 1 },
  go: { weight: 4, group: 'go' },
  'go-checks': { weight: 3, group: 'go' },
  'go-native': { weight: 4, group: 'go' },
  'go-ds': { weight: 4, group: 'go' },
  'go-api': { weight: 4, group: 'go' },
  'go-harness': { weight: 4, group: 'go' },
  'go-fts5-arm64': { weight: 2, group: 'go' },
  'go-lint': { weight: 3, group: 'go' },
  'go-vuln': { weight: 2, group: 'go' },
  'go-sqlc': { weight: 1, group: 'go', locks: ['tracked-tree'] },
  'go-postgres': { weight: 3, group: 'go' },
  // It deletes the tracked placeholder in internal/web/dist and builds with the real client there: alone, and put back.
  'go-release': { weight: 4, group: 'go', exclusive: true, snapshot: ['internal/web/dist'] },
  'ci-ok': { kind: 'aggregate', weight: 0 },
};

/** Checks GitHub runs that ci.yml does not hold: ci-local runs them after its own jobs (`--only codeql` alone). */
export const EXTRA_JOBS = {
  codeql: {
    weight: 4,
    languages: ['actions', 'go', 'javascript-typescript', 'rust'],
    install: 'CodeQL CLI not installed: download codeql-bundle-linux64.tar.gz from https://github.com/github/codeql-action/releases, unpack it and put its codeql directory on PATH (or `gh extension install github/gh-codeql && gh codeql set-version latest`).',
  },
};

/** What each `uses:` action becomes locally. */
export const ACTION_MAP = {
  'actions/checkout': { kind: 'noop', reason: 'the working tree is the checkout' },
  'actions/setup-node': { kind: 'prereq', tool: 'node' },
  'actions/setup-go': { kind: 'prereq', tool: 'go' },
  'actions/cache': { kind: 'noop', reason: 'cache: local build directories persist between runs' },
  'actions/cache/restore': { kind: 'noop', reason: 'cache: local build directories persist between runs' },
  'actions/cache/save': { kind: 'noop', reason: 'cache: local build directories persist between runs' },
  'actions/upload-artifact': { kind: 'upload' },
  'actions/download-artifact': { kind: 'download' },
};

/** The ports ci-local moves (for the sync test): the web, core and voice suites read e2e/web/support/ports.ts. */
export const BLOCK_SIZE = 1000;
export const UI_BLOCK_SIZE = 50;
/** Ports a block may reach stay under Linux's ephemeral range (32768): the suites span 7884..8615. */
export const MAX_BLOCK = 24;
export const DEFAULT_BASE = 1;

/** The port offsets of block `k`: e2e ports (DILLA_E2E_PORT_OFFSET) and the ui job's Storybook ports. */
export function blockPorts(k) {
  if (!Number.isInteger(k) || k < 0 || k > MAX_BLOCK) throw new Error(`port block ${k} is outside 0..${MAX_BLOCK}`);
  return { block: k, offset: k * BLOCK_SIZE, uiOffset: k * UI_BLOCK_SIZE };
}

/** The env a block-ported browser job runs with: its own port offset, host data root and Playwright output dir. */
export function e2eBlockEnv(k, job, root, tmp) {
  const { offset } = blockPorts(k);
  return {
    DILLA_E2E_PORT_OFFSET: String(offset),
    DILLA_E2E_HOST_DATA_ROOT: join(tmp, `dw-${k}`),
    DILLA_E2E_OUTPUT_DIR: join(root, 'target', 'ci-local', 'e2e', job),
  };
}

/**
 * The ports a block covers before its offset, probed free before the block is handed out: the browser suites' host,
 * control and SFU ports (TCP and UDP) and the ui job's Storybook ports (TCP). The sync test holds the sources to them.
 */
export const E2E_PORT_SPAN = [7884, 8616];
export const UI_PORT_SPAN = [6006, 6038];

/** The workflow-level keys ci-local reads or knows to be inert locally; any other (`defaults:`, …) fails the sync check. */
export const TOP_LEVEL_KEYS = ['name', 'on', 'permissions', 'concurrency', 'env', 'jobs'];

/** The ui job's fixed Storybook ports (`--port N`, `127.0.0.1:N`, `shoot-stories.mjs N`) moved by the block's ui offset. */
export function moveUiPorts(script, uiOffset) {
  return script.replace(/(--port |127\.0\.0\.1:|shoot-stories\.mjs )(\d{4,5})\b/g, (_, lead, port) => `${lead}${Number(port) + uiOffset}`);
}

const MEMO_RUNS = [
  /^npm ci$/,
  /^bash scripts\/install-ci-tool\.sh \S+$/,
  /^wasm-pack build .+$/,
];

const APT_RUNS = [
  /^echo "suffix=\$\(bash scripts\/ci-apt-cache\.sh key\)" >> "\$GITHUB_OUTPUT"$/,
  /^bash scripts\/ci-apt-cache\.sh (?:prepare|stash)$/,
];

const PLAYWRIGHT_INSTALL = /^(?:npx playwright|node node_modules\/playwright\/cli\.js) install --with-deps ((?:chromium|firefox|webkit)(?: (?:chromium|firefox|webkit))*)$/;
const CLANG_STEP_NAME = /^clang (\d+) for the wasm C build$/;
const PRLIMIT_LINE = /^sudo prlimit --pid "\$\$" --memlock=(\d+):(\d+)$/;

/** Expressions a step that runs may carry, and what they are locally. */
const EXPRESSIONS = {
  'github.workspace': (ctx) => ctx.root,
  'runner.temp': (ctx) => ctx.runnerTemp,
  'github.job': (ctx) => ctx.jobName,
  'runner.os': () => 'Linux',
};

/** `${{ … }}` replaced, or an error naming the expression ci-local does not know. */
export function substitute(text, ctx) {
  return String(text).replace(/\$\{\{\s*([^}]*?)\s*\}\}/g, (whole, expr) => {
    if (expr in EXPRESSIONS) return EXPRESSIONS[expr](ctx);
    const matrix = /^matrix\.([A-Za-z0-9_-]+)$/.exec(expr);
    if (matrix && ctx.matrix && matrix[1] in ctx.matrix) return ctx.matrix[matrix[1]];
    throw new Error(`unmapped expression ${whole}`);
  });
}

function expressionsIn(text) {
  return [...String(text).matchAll(/\$\{\{\s*([^}]*?)\s*\}\}/g)].map((m) => m[1]);
}

const IF_ATOMS = [
  [/^needs\.changes\.outputs\.([a-z0-9]+) != 'false'$/, (m, ctx) => ctx.outputs[m[1]] !== false],
  [/^needs\.changes\.outputs\.([a-z0-9]+) == 'true'$/, (m, ctx) => ctx.outputs[m[1]] === true],
  [/^failure\(\)$/, (m, ctx) => ctx.failed === true],
  [/^success\(\)$/, (m, ctx) => ctx.failed !== true],
  [/^always\(\)$/, () => true],
  // An upload's evidence guard. There is no runner workspace to hash here; the upload step itself reports a missing path.
  [/^hashFiles\('[^']+'(?:, '[^']+')*\) != ''$/, () => true],
  [/^steps\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+) != 'true'$/, (m, ctx) => ctx.steps?.[m[1]]?.[m[2]] !== 'true'],
  [/^steps\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+) == 'true'$/, (m, ctx) => ctx.steps?.[m[1]]?.[m[2]] === 'true'],
  [/^contains\(needs\.\*\.result, '(failure|cancelled)'\)$/, (m, ctx) => (ctx.needResults ?? []).includes(m[1])],
];

/**
 * An `if:` expression of the forms ci.yml uses: atoms joined by `||` or `&&` (no parentheses, `&&` binding tighter).
 * An empty condition is `success()`. Anything else throws, so a new form fails the sync check instead of being guessed.
 */
export function evalIf(expr, ctx) {
  if (expr === null || expr === undefined || String(expr).trim() === '') return ctx.failed !== true;
  const text = String(expr).trim().replace(/^\$\{\{\s*([\s\S]*?)\s*\}\}$/, '$1');
  const atom = (a) => {
    for (const [re, fn] of IF_ATOMS) {
      const m = re.exec(a.trim());
      if (m) return fn(m, ctx);
    }
    throw new Error(`unmapped if: expression "${a.trim()}"`);
  };
  const value = text.split('||').map((or) => or.split('&&').map(atom).every(Boolean)).some(Boolean);
  // A step without failure()/always() is skipped once a step failed, whatever the rest says.
  if (ctx.failed === true && !/\b(failure|always)\(\)/.test(text)) return false;
  return value;
}

/** The action of a `uses:` without its pinned ref. */
const actionName = (uses) => String(uses).split('@')[0];

/**
 * What one step becomes locally: { kind, … } where kind is
 * 'noop' (reason), 'prereq' (tool, and its details), 'upload' (name, paths, required), 'download' (name, path),
 * 'run' (script, memo, note) or 'playwright' (engines). Throws for anything unmapped.
 */
export function classifyStep(step, job = {}) {
  const allowed = new Set(['name', 'id', 'uses', 'with', 'run', 'env', 'if', 'timeout-minutes']);
  for (const key of Object.keys(step)) if (!allowed.has(key)) throw new Error(`step key "${key}" is unmapped`);
  for (const key of ['run', 'if']) {
    for (const expr of expressionsIn(step[key] ?? '')) {
      if (!(expr in EXPRESSIONS) && !/^matrix\./.test(expr) && key === 'run') throw new Error(`unmapped expression \${{ ${expr} }} in a run step`);
    }
  }
  for (const v of Object.values(step.env ?? {})) {
    for (const expr of expressionsIn(v)) if (!(expr in EXPRESSIONS) && !/^matrix\./.test(expr)) throw new Error(`unmapped expression \${{ ${expr} }} in a step env`);
  }
  if (step.uses !== undefined) {
    const name = actionName(step.uses);
    const mapped = ACTION_MAP[name];
    if (mapped === undefined) throw new Error(`action ${name} is unmapped`);
    const w = step.with ?? {};
    if (mapped.kind === 'prereq') return { kind: 'prereq', tool: mapped.tool, versionFile: w['node-version-file'] ?? w['go-version-file'] ?? null };
    if (mapped.kind === 'upload') {
      if (/\bfailure\(\)/.test(step.if ?? '')) return { kind: 'noop', reason: `failure upload: the results stay in the tree (${String(w.path).trim()})` };
      return { kind: 'upload', name: w.name, paths: String(w.path).split('\n').map((p) => p.trim()).filter(Boolean), required: w['if-no-files-found'] === 'error' };
    }
    if (mapped.kind === 'download') {
      if (!w.name || !w.path) throw new Error('download-artifact without a name and a path is unmapped');
      return { kind: 'download', name: w.name, path: w.path };
    }
    return { ...mapped };
  }
  if (step.run === undefined) throw new Error('a step with neither uses: nor run: is unmapped');
  const script = String(step.run).replace(/\n$/, '');
  const clang = CLANG_STEP_NAME.exec(step.name ?? '');
  if (clang) {
    // scripts/ci-llvm.sh pins the apt.llvm.org build of the line the step names; an inline step checks its version.
    const version = script === 'bash scripts/ci-llvm.sh install' ? clang[1] : /clang version (\d+\.\d+)\./.exec(script)?.[1];
    if (!version) throw new Error(`step "${step.name}" no longer checks a clang version`);
    return { kind: 'prereq', tool: 'llvm', version, reason: 'apt.llvm.org packages: the local LLVM of the same line stands in' };
  }
  if (/^sudo apt-get update && sudo apt-get install -y libgtk-3-dev(?: pkg-config)?$/.test(script)) return { kind: 'prereq', tool: 'gtk', reason: 'GTK3 headers and pkg-config must be installed locally' };
  if (script === 'sudo apt-get update && sudo apt-get install -y libarchive-tools') return { kind: 'prereq', tool: 'bsdtar', reason: 'libarchive-tools: bsdtar must be installed locally' };
  if (APT_RUNS.some((re) => re.test(script))) return { kind: 'noop', reason: 'apt cache: no apt here' };
  const pw = PLAYWRIGHT_INSTALL.exec(script);
  if (pw) {
    const engines = pw[1].split(' ').sort();
    return { kind: 'run', script: `npx playwright install ${engines.join(' ')}`, memo: `playwright install ${engines.join(' ')}`, note: 'browsers only: --with-deps needs sudo apt, so the system libraries are a prerequisite' };
  }
  if (script === 'sh scripts/ci-audio-server.sh') return { kind: 'prereq', tool: 'audio', reason: 'apt pulseaudio: a running local audio server (PipeWire or PulseAudio) stands in' };
  const lines = script.split('\n');
  const prlimit = lines.findIndex((l) => PRLIMIT_LINE.test(l.trim()));
  if (prlimit >= 0) {
    const [, soft, hard] = PRLIMIT_LINE.exec(lines[prlimit].trim());
    lines[prlimit] = `prlimit --pid "$$" --memlock=${soft}:${hard} 2>/dev/null || echo "ci-local: RLIMIT_MEMLOCK stays at $(ulimit -l) KiB (CI raises it to ${Number(soft) / 1024} KiB as root); raise the hard limit if SQLCipher's mlock fails"`;
  }
  const mapped = lines.join('\n');
  if (/(^|[\s;&|(])sudo\s/.test(mapped)) throw new Error(`run step needs sudo and is unmapped: ${script.split('\n')[0]}`);
  const memo = MEMO_RUNS.some((re) => re.test(mapped)) ? mapped : null;
  return { kind: 'run', script: mapped, memo, note: prlimit >= 0 ? 'sudo prlimit: raised without sudo where the hard limit allows' : null };
}

/**
 * A job's matrix legs: the product of its axes (`shard: [1, 2, 3]`, the sharded Go jobs) or its `include` list (one leg
 * per native target), or [null] without a strategy. `fail-fast` is inert locally (the scheduler stops at the first
 * failure anyway); anything else (exclude, include beside axes, max-parallel) throws, never guessed at.
 */
function matrixLegs(name, strategy) {
  if (strategy === undefined || strategy === null) return [null];
  for (const key of Object.keys(strategy)) if (key !== 'matrix' && key !== 'fail-fast') throw new Error(`job "${name}": strategy key "${key}" is unmapped`);
  const matrix = strategy.matrix ?? {};
  const axes = Object.entries(matrix).filter(([key]) => key !== 'include');
  if (matrix.include !== undefined) {
    if (axes.length > 0 || !Array.isArray(matrix.include)) throw new Error(`job "${name}": strategy.matrix.include beside axes is unmapped`);
    return matrix.include;
  }
  if (axes.length === 0) throw new Error(`job "${name}": strategy.matrix has no axes`);
  let legs = [{}];
  for (const [key, values] of axes) {
    if (!Array.isArray(values) || values.length === 0) throw new Error(`job "${name}": matrix axis "${key}" is not a list`);
    legs = legs.flatMap((leg) => values.map((v) => ({ ...leg, [key]: v })));
  }
  return legs;
}

/** The jobs of a parsed workflow, a matrix job expanded into one job per leg (one job per native target or shard). */
export function expandJobs(workflow) {
  const out = [];
  for (const [name, body] of Object.entries(workflow.jobs ?? {})) {
    // secrets: a reusable workflow's named secrets (the skipped desktop job); nothing local reads them.
    const allowed = new Set(['needs', 'if', 'runs-on', 'steps', 'env', 'timeout-minutes', 'strategy', 'services', 'outputs', 'name', 'permissions', 'uses', 'secrets']);
    for (const key of Object.keys(body)) if (!allowed.has(key)) throw new Error(`job "${name}": key "${key}" is unmapped`);
    const needs = body.needs === undefined || body.needs === null ? [] : Array.isArray(body.needs) ? body.needs : [body.needs];
    const legs = matrixLegs(name, body.strategy);
    for (const matrix of legs) {
      const id = matrix === null ? name : `${name}-${Object.values(matrix)[0]}`;
      const runsOn = substitute(body['runs-on'], { matrix: matrix ?? {} });
      out.push({
        id, name, matrix: matrix ?? {}, runsOn, needs, if: name === 'ci-watchdog' ? null : body.if ?? null,
        env: body.env ?? {}, steps: body.steps ?? [], timeout: body['timeout-minutes'] === undefined ? 360 : Number(body['timeout-minutes']),
        services: body.services ?? {},
      });
    }
  }
  return out;
}

/** Why a job cannot run on this machine, or null. */
export function skipReason(job, arch = 'x64') {
  if (JOB_MAP[job.name]?.skip) return JOB_MAP[job.name].skip;
  if (/-arm\b|arm64/.test(job.runsOn) && arch !== 'arm64') return `needs a native linux/arm64 runner (${job.runsOn}); this machine is ${arch}`;
  return null;
}

/** ci.yml's services, as a container to start: image, env, the published ports and the readiness command. */
export function mapService(name, svc) {
  if (name !== 'postgres') throw new Error(`service "${name}" is unmapped (only postgres is)`);
  const allowed = new Set(['image', 'env', 'ports', 'options']);
  for (const key of Object.keys(svc)) if (!allowed.has(key)) throw new Error(`service "${name}": key "${key}" is unmapped`);
  const options = splitShellWords(svc.options ?? '');
  let health = null;
  const passOptions = [];
  for (let n = 0; n < options.length; n++) {
    if (options[n] === '--health-cmd') health = options[++n];
    else if (/^--health-(interval|timeout|retries)$/.test(options[n])) n++;
    else passOptions.push(options[n]);
  }
  const ports = (svc.ports ?? []).map((p) => {
    const m = /^(\d+):(\d+)$/.exec(String(p));
    if (!m) throw new Error(`service "${name}": port mapping ${p} is unmapped`);
    return { host: Number(m[1]), container: Number(m[2]) };
  });
  return { name, image: svc.image, env: svc.env ?? {}, ports, health, options: passOptions };
}

export function splitShellWords(text) {
  const words = [];
  const re = /"([^"]*)"|'([^']*)'|(\S+)/g;
  for (const m of String(text).matchAll(re)) words.push(m[1] ?? m[2] ?? m[3]);
  return words;
}

/** Every job id each job depends on, transitively, in a stable order; throws on a cycle or an unknown need. */
export function dependencyClosure(jobs, ids) {
  const byName = new Map();
  for (const job of jobs) byName.set(job.name, [...(byName.get(job.name) ?? []), job.id]);
  const byId = new Map(jobs.map((j) => [j.id, j]));
  const seen = new Set();
  const visiting = new Set();
  const order = [];
  const visit = (id) => {
    if (seen.has(id)) return;
    if (visiting.has(id)) throw new Error(`needs cycle through "${id}"`);
    visiting.add(id);
    const job = byId.get(id);
    if (!job) throw new Error(`unknown job "${id}"`);
    for (const need of job.needs) {
      if (!byName.has(need)) throw new Error(`job "${job.id}" needs unknown job "${need}"`);
      for (const dep of byName.get(need)) visit(dep);
    }
    visiting.delete(id);
    seen.add(id);
    order.push(id);
  };
  for (const id of ids) visit(id);
  return order;
}

/** The job ids a list of names selects (a matrix job's name selects every leg); throws for an unknown name. */
export function resolveNames(jobs, names) {
  const ids = [];
  for (const name of names) {
    const hits = jobs.filter((j) => j.id === name || j.name === name).map((j) => j.id);
    if (name in EXTRA_JOBS) ids.push(name);
    else if (hits.length === 0) throw new Error(`unknown job "${name}"; jobs: ${[...new Set(jobs.map((j) => j.id)), ...Object.keys(EXTRA_JOBS)].join(', ')}`);
    else ids.push(...hits);
  }
  return [...new Set(ids)];
}

/**
 * The jobs a run covers, in dependency order:
 * - `only`: those jobs, plus what they need unless `deps` is false (then their artifacts must exist from an earlier run);
 * - otherwise every job whose `if:` holds for `outputs` (the areas: everything for a full run, the diff's for --changed)
 *   and the extra jobs.
 * ci-ok and changes are always in, as in CI.
 */
export function selectJobs(jobs, { only = null, outputs, deps = true }) {
  let chosen;
  if (only !== null) chosen = resolveNames(jobs, only);
  else {
    chosen = jobs.filter((j) => evalIf(j.if, { outputs, needResults: [] })).map((j) => j.id);
    // The extra checks (CodeQL) run on any code change, as GitHub's code scanning does.
    if (outputs.code !== false) chosen.push(...Object.keys(EXTRA_JOBS));
  }
  const extras = chosen.filter((id) => id in EXTRA_JOBS);
  // ci-ok's verdict covers the jobs that ran: its needs never pull more jobs in.
  const isAggregate = (id) => JOB_MAP[jobs.find((j) => j.id === id)?.name]?.kind === 'aggregate';
  const aggregates = chosen.filter((id) => !(id in EXTRA_JOBS) && isAggregate(id));
  const ciIds = chosen.filter((id) => !(id in EXTRA_JOBS) && !isAggregate(id));
  const closure = dependencyClosure(jobs, ciIds).filter((id) => !isAggregate(id));
  const ordered = deps ? closure : closure.filter((id) => ciIds.includes(id));
  return [...ordered, ...aggregates, ...extras];
}

/** The workflow at `root`, parsed and expanded. */
export function loadWorkflow(root) {
  const text = readFileSync(join(root, '.github', 'workflows', 'ci.yml'), 'utf8');
  return { text, workflow: parseYaml(text), jobs: expandJobs(parseYaml(text)) };
}

/**
 * Everything in ci.yml that ci-local would not know how to run: an unmapped job (or a JOB_MAP entry for a job ci.yml
 * no longer has), step, action, service, `if:` or expression. scripts/check-ci-workflow.mjs fails on any.
 */
export function localMirrorProblems(text) {
  const problems = [];
  let workflow;
  let jobs;
  try {
    workflow = parseYaml(text);
    jobs = expandJobs(workflow);
  } catch (e) {
    return [`ci-local: ${e.message}`];
  }
  for (const key of Object.keys(workflow ?? {})) {
    if (!TOP_LEVEL_KEYS.includes(key)) problems.push(`ci-local: workflow-level key "${key}" is unmapped (TOP_LEVEL_KEYS in scripts/ci-local-plan.mjs)`);
  }
  const names = new Set(jobs.map((j) => j.name));
  for (const name of names) if (!(name in JOB_MAP)) problems.push(`ci-local: job "${name}" is neither mapped nor skipped in JOB_MAP (scripts/ci-local-plan.mjs)`);
  for (const name of Object.keys(JOB_MAP)) if (!names.has(name)) problems.push(`ci-local: JOB_MAP maps "${name}", which ci.yml no longer has`);
  const everything = { go: true, core: true, web: true, e2e: true, text: true, code: true };
  for (const job of jobs) {
    const where = `ci-local: job "${job.id}"`;
    try {
      evalIf(job.if, { outputs: everything, needResults: [] });
      dependencyClosure(jobs, [job.id]);
      for (const [svc, body] of Object.entries(job.services)) mapService(svc, body);
      for (const v of Object.values(job.env)) substitute(v, { root: '/', runnerTemp: '/', jobName: job.name, matrix: job.matrix });
    } catch (e) {
      problems.push(`${where}: ${e.message}`);
    }
    if (JOB_MAP[job.name]?.skip) continue;
    for (const command of JOB_MAP[job.name]?.cleanTree ?? []) {
      if (!job.steps.some((s) => typeof s.run === 'string' && s.run.trim() === command)) problems.push(`${where}: cleanTree names "${command}", which is not a run step of the job`);
    }
    const kind = JOB_MAP[job.name]?.kind ?? 'steps';
    if (kind !== 'steps') continue;
    job.steps.forEach((step, n) => {
      try {
        evalIf(step.if, { outputs: everything, steps: {}, failed: false });
        classifyStep(step, job);
      } catch (e) {
        problems.push(`${where} step ${n + 1}${step.name ? ` (${step.name})` : ''}: ${e.message}`);
      }
    });
  }
  return problems;
}
