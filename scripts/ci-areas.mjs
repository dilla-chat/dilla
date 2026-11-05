import { execFileSync } from 'node:child_process';
import { appendFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

/**
 * Which parts of the tree a change touches, and so which jobs of the two CI workflows (WORKFLOWS: server.yml and
 * desktop-ci.yml) it runs. Each workflow's `changes` job runs this file and writes one boolean output per area; every
 * other job is gated on the outputs named for it in JOB_GATES[workflow] (scripts/check-ci-workflow.mjs holds each file
 * to its table). A skipped job counts as passed for a required check, so branch protection keeps working: both
 * workflows run on every pull request, and their aggregate checks (server-ok, desktop-ok) always report.
 *
 * Areas: go, core, web, e2e, and text (the profile-text tables: the generator, the tables and the rule, which the
 * `node` job's step that regenerates them against unicode.org runs for). `code` is true when anything other than docs changed. Docs-only changes (every file
 * under docs/ or a *.md at any depth, README.md and protocol/*.md included) set no area, so only each workflow's
 * infrastructure (WORKFLOWS[workflow].always: changes, server's docs, the watchdog, the barrier and the ok check) runs.
 *
 * Fail safe: a push, any event that is not a pull request, a diff that cannot be computed, an empty diff and a file
 * no rule names all turn every area on.
 */
export const AREAS = ['go', 'core', 'web', 'e2e', 'text'];

/**
 * The path tables. A pattern with a slash matches the whole path (`**` crosses directories, `*` does not); a pattern
 * without one matches the file name at any depth.
 *
 * A file that matches DOC_PATTERNS is documentation: it sets no area, except through DOC_RULES, the Markdown that a
 * test or a build reads (found by scanning the sources for reads, see the guard test in ci-areas.test.mjs). Every
 * other file takes the union of the areas of all PATH_RULES it matches, and a file that matches none is unknown.
 * `ci` is a pseudo-area that forces every area.
 */
export const DOC_PATTERNS = ['docs/**', '*.md', 'LICENSE', 'LICENSE-APACHE', 'DCO'];
export const DOC_RULES = [
  // packages/web copy.test.ts and strings*.test.ts byte-compare each flow's `## Copy` table with en.ts.
  { areas: ['web'], patterns: ['docs/design/flows/**'] },
  // internal/dillad/factor_owed_test.go parses the route tables of 02 and 09; check:docs (the docs job) reads the rest.
  { areas: ['go'], patterns: ['protocol/*.md'] },
  // core/vendor/sqlite-wasm-vfs/src/lib.rs: #![doc = include_str!("../README.md")].
  { areas: ['core'], patterns: ['core/**'] },
];
export const PATH_RULES = [
  { areas: ['ci'], patterns: ['.github/**', 'scripts/**'] },
  { areas: ['go'], patterns: ['internal/**', 'cmd/**', 'go.mod', 'go.sum', 'sqlc*', 'deploy/**', '.golangci.yml'] },
  // Rust MLS conformance tests read these shared delivery-service fixtures.
  { areas: ['core'], patterns: ['internal/ds/dstest/testdata/kind3-interaction/**'] },
  { areas: ['core'], patterns: ['core/**', 'testkit/**', 'Cargo.*', 'rust-toolchain.toml', 'deny.toml', '.cargo/**', 'protocol/vectors/**'] },
  { areas: ['web'], patterns: ['packages/**', 'package*.json', '.nvmrc', 'eslint.config.js'] },
  { areas: ['e2e'], patterns: ['e2e/**'] },
  // The generator, the tables it writes and the Go and TS rules that read them: `node`'s "profile-text tables are current" step.
  { areas: ['text'], patterns: [
    'scripts/gen-profile-text-tables.mjs', 'internal/auth/profile_tables.go', 'internal/auth/profile_text.go', 'internal/auth/testdata/rgi-zwj-sequences.txt',
    'packages/client-core/src/profile-tables.ts', 'packages/client-core/src/profile-text.ts',
  ] },
  // Portable maintenance output privacy audit reads this exact native test source.
  { areas: ['web'], patterns: ['internal/desktopsandbox/native_root_linux_test.go'] },
  // internal/desktopsandbox/transaction_linux_test.go runs the frontend bootstrap for interop coverage.
  { areas: ['go'], patterns: ['packages/desktop/native/linux-sandbox/frontend/bootstrap.py'] },
  // internal/web/downloads_test.go verifies the embedded brand against this desktop asset.
  { areas: ['go'], patterns: ['packages/desktop/assets/tray.png'] },
  // internal/desktopdownload/download_test.go holds the server's pinned feed keys to the desktop app's.
  { areas: ['go'], patterns: ['packages/desktop/release-trust.json', 'packages/desktop/src/update-keys.ts'] },
  // internal/api/invites_test.go (TestLandingTokensMatchMesh) reads dist/tokens.css.
  { areas: ['go'], patterns: ['packages/design-tokens/**'] },
  { areas: ['go'], patterns: ['packages/web/dist', 'packages/web/dist/**'] },
  // packages/client-core/src/profile-text.test.ts reads the RGI sequences the generator writes for the Go tests too.
  { areas: ['web'], patterns: ['internal/auth/testdata/rgi-zwj-sequences.txt'] },
  // packages/media/scripts/check-protocol-schema.mjs compares @livekit/protocol with this Go golden.
  { areas: ['web'], patterns: ['internal/sfu/testdata/**'] },
];

/**
 * What an area drags in with it: the WASI core is used by the Go tests, and the desktop suites run the web client. A
 * server-only (go) change does not drag in the desktop lane (e2e): desktop acceptance of a server change is the nightly
 * desktop run's, so a server change is never blocked on desktop e2e (plan 2026-11-05, Task 6).
 */
export const FORCES = {
  ci: ['go', 'core', 'web', 'e2e', 'text'],
  core: ['go', 'web', 'e2e'],
  go: [],
  web: ['e2e'],
  e2e: [],
  text: [],
};

/**
 * The two CI workflows. Each is always triggered and reports one aggregate check (`ok`); `always` are its
 * infrastructure jobs, which carry no area gate and run for every change. The release gates read the workflow `name`.
 */
export const WORKFLOWS = {
  server: { file: 'server.yml', name: 'server', ok: 'server-ok', always: ['ci-watchdog', 'changes', 'docs', 'server-work-complete', 'server-ok'] },
  desktop: { file: 'desktop-ci.yml', name: 'desktop', ok: 'desktop-ok', always: ['ci-watchdog', 'changes', 'desktop-work-complete', 'desktop-ok'] },
};

/**
 * The outputs each job of each workflow is gated on (OR). A job missing here is one of the workflow's `always` jobs.
 * Because the areas imply each other (FORCES), `web` is "web or core" and `e2e` is "web, core or e2e".
 * - rust-native and rust-wasi run for any code: go-harness, go-native and the shared integration and desktop suites
 *   download the testkit binary, the native helper and the WASI core they build. rust-native's lint and test steps are
 *   gated on core at step level. Artifacts never cross workflows, so each workflow builds its own.
 * - go-release builds the embedded client; server's web-build exists to feed it, so it shares go-release's gate.
 */
const RUST_ARTIFACTS = { 'rust-native': ['code'], 'rust-wasi': ['code'] };
export const JOB_GATES = {
  server: {
    ...RUST_ARTIFACTS,
    vectors: ['core'],
    'web-build': ['go', 'web'],
    deny: ['core'],
    go: ['go'],
    'go-checks': ['go'],
    'go-native': ['go'],
    'go-ds': ['go'],
    'go-api': ['go'],
    'go-harness': ['go'],
    'go-fts5-arm64': ['go'],
    'go-lint': ['go'],
    'go-vuln': ['go'],
    'go-sqlc': ['go'],
    'go-postgres': ['go'],
    'go-release': ['go', 'web'],
  },
  desktop: {
    node: ['code'],
    ui: ['web'],
    // Besides the wasm core's tests it runs packages/media's typecheck, wasm tests and the LiveKit schema check.
    'rust-wasm-node': ['web'],
    ...RUST_ARTIFACTS,
    'browser-spike': ['e2e'],
    'browser-media': ['e2e'],
    'web-build': ['e2e'],
    'linux-sandbox-foundation': ['e2e'],
    desktop: ['e2e'],
    'core-integration': ['e2e'],
  },
};

/**
 * The `if:` expression a workflow must carry for a job. It tests `!= 'false'`, not `== 'true'`, so an output that is
 * missing or empty (a renamed step id, say) runs the job instead of silently skipping it.
 */
export function gateExpression(workflow, job) {
  return JOB_GATES[workflow][job].map((output) => `needs.changes.outputs.${output} != 'false'`).join(' || ');
}

/** Whether a job runs for a set of outputs (as areasFor returns them): like the expression, anything but false runs it. */
export function jobRuns(workflow, job, outputs) {
  return JOB_GATES[workflow][job].some((output) => outputs[output] !== false);
}

const compiled = new Map();
function matches(pattern, file) {
  let re = compiled.get(pattern);
  if (re === undefined) {
    const source = pattern
      .split('**')
      .map((part) => part.split('*').map((s) => s.replace(/[.+?^${}()|[\]\\]/g, '\\$&')).join('[^/]*'))
      .join('[\\s\\S]*');
    re = new RegExp(`^${source}$`);
    compiled.set(pattern, re);
  }
  return re.test(pattern.includes('/') ? file : file.slice(file.lastIndexOf('/') + 1));
}

const areasOf = (rules, file) => rules.filter((rule) => rule.patterns.some((pattern) => matches(pattern, file))).flatMap((rule) => rule.areas);

/** The areas of a file (docs set none, but for DOC_RULES), or `unknown` for a file no rule names. */
export function classifyFile(file) {
  if (DOC_PATTERNS.some((pattern) => matches(pattern, file))) return { docs: true, areas: areasOf(DOC_RULES, file) };
  const areas = areasOf(PATH_RULES, file);
  return areas.length === 0 ? { unknown: true, areas: [] } : { areas };
}

const everything = () => ({ go: true, core: true, web: true, e2e: true, text: true, code: true });

/**
 * The outputs for a list of changed files. `null` (the diff could not be computed), an empty list and an unnamed
 * file all fail safe to everything.
 */
export function areasFor(files) {
  if (files === null || files.length === 0) return everything();
  const on = new Set();
  for (const file of files) {
    const result = classifyFile(file);
    if (result.unknown) return everything();
    for (const area of result.areas) {
      on.add(area);
      for (const forced of FORCES[area]) on.add(forced);
    }
  }
  return { ...Object.fromEntries(AREAS.map((area) => [area, on.has(area)])), code: on.size > 0 };
}

/** The files of the checkout, NUL-separated by git so that no name is split. */
export function trackedFiles() {
  return execFileSync('git', ['ls-files', '-z'], { cwd: fileURLToPath(new URL('..', import.meta.url)), encoding: 'utf8', maxBuffer: 64 << 20 }).split('\0').filter(Boolean);
}

const OUTPUT_NAMES = [...AREAS, 'code'];
const NONE = Object.fromEntries(OUTPUT_NAMES.map((name) => [name, false]));

/**
 * Every set of outputs a diff of tracked files can produce: the outputs of each file alone, every union of those,
 * and the empty and the everything cases. Derived from the rules and the files, so a new rule is covered without
 * touching a sample list. Falls back to one file per rule when git is unavailable.
 */
let reachableCache = null;
export function reachableOutputs(files = null) {
  if (files === null && reachableCache !== null) return reachableCache;
  let list = files;
  if (list === null) {
    try {
      list = trackedFiles();
    } catch {
      list = ['docs/a.md', 'scripts/x.mjs', 'internal/x.go', 'core/x.rs', 'packages/x.ts', 'e2e/x.ts', 'no-rule-names-this'];
    }
  }
  const singles = new Map();
  for (const file of list) {
    const outputs = areasFor([file]);
    singles.set(JSON.stringify(outputs), outputs);
  }
  singles.set(JSON.stringify(NONE), NONE);
  let sets = new Map([[JSON.stringify(NONE), NONE]]);
  for (const single of singles.values()) {
    const next = new Map(sets);
    for (const set of sets.values()) {
      const union = Object.fromEntries(OUTPUT_NAMES.map((name) => [name, set[name] || single[name]]));
      next.set(JSON.stringify(union), union);
    }
    sets = next;
  }
  const all = areasFor(null);
  sets.set(JSON.stringify(all), all);
  const result = [...sets.values()];
  if (files === null) reachableCache = result;
  return result;
}

/**
 * The outputs for an event, the same for both workflows. Only a pull request is narrowed; a push (main) and the nightly schedule run everything. `diff` returns the
 * changed paths as an array and may throw.
 */
export function computeAreas({ eventName, baseSha, diff }) {
  if (eventName !== 'pull_request' || !baseSha) return everything();
  try {
    return areasFor(diff(baseSha));
  } catch {
    return everything();
  }
}

// --no-renames lists both paths of a rename, so moving code into docs/ is still a code change.
const gitDiff = (base, head) => execFileSync('git', ['diff', '--name-only', '--no-renames', '-z', base, head], { encoding: 'utf8', maxBuffer: 64 << 20 }).split('\0').filter(Boolean);

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const { EVENT_NAME, BASE_SHA, HEAD_SHA = 'HEAD', GITHUB_OUTPUT } = process.env;
  const outputs = computeAreas({ eventName: EVENT_NAME, baseSha: BASE_SHA, diff: (base) => gitDiff(base, HEAD_SHA) });
  const lines = Object.entries(outputs).map(([name, value]) => `${name}=${value}`);
  console.error(`changes: ${lines.join(' ')}`);
  if (GITHUB_OUTPUT) appendFileSync(GITHUB_OUTPUT, `${lines.join('\n')}\n`);
  else console.log(lines.join('\n'));
}
