import { execFileSync } from 'node:child_process';
import { appendFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

/**
 * Which parts of the tree a change touches, and so which jobs of .github/workflows/ci.yml it runs. The `changes` job
 * runs this file and writes one boolean output per area; every other job is gated on the outputs named for it in
 * JOB_GATES (scripts/check-ci-workflow.mjs holds ci.yml to that table). A skipped job counts as passed for a required
 * check, so branch protection keeps working.
 *
 * Areas: go, core, web, e2e, and text (the profile-text tables: the generator, the tables and the rule, which the
 * `node` job's step that regenerates them against unicode.org runs for). `code` is true when anything other than docs changed. Docs-only changes (every file
 * under docs/ or a *.md at any depth, README.md and protocol/*.md included) set no area, so only the `docs` job runs.
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
  // internal/api/invites_test.go (TestLandingTokensMatchMesh) reads dist/tokens.css.
  { areas: ['go'], patterns: ['packages/design-tokens/**'] },
  // packages/client-core/src/profile-text.test.ts reads the RGI sequences the generator writes for the Go tests too.
  { areas: ['web'], patterns: ['internal/auth/testdata/rgi-zwj-sequences.txt'] },
  // packages/media/scripts/check-protocol-schema.mjs compares @livekit/protocol with this Go golden.
  { areas: ['web'], patterns: ['internal/sfu/testdata/**'] },
];

/** What an area drags in with it: the WASI core is used by the Go tests, and the browser suites run the server and the client together. */
export const FORCES = {
  ci: ['go', 'core', 'web', 'e2e', 'text'],
  core: ['go', 'web', 'e2e'],
  go: ['e2e'],
  web: ['e2e'],
  e2e: [],
  text: [],
};

/**
 * The outputs each job of ci.yml is gated on (OR). A job missing here is `changes` or `docs`, which always run.
 * Because the areas imply each other (FORCES), `e2e` is in practice "any code" and `web` is "web or core".
 * - rust-native and rust-wasi run for any code: go-harness and the browser suites download the testkit binary and
 *   the WASI core they build. rust-native's lint and test steps are gated on core at step level.
 * - go-release builds the embedded client.
 */
export const JOB_GATES = {
  node: ['code'],
  ui: ['web'],
  'rust-native': ['code'],
  // Besides the wasm core's tests it runs packages/media's typecheck, wasm tests and the LiveKit schema check.
  'rust-wasm-node': ['web'],
  'rust-wasi': ['code'],
  'spotify-helper': ['code'],
  vectors: ['core'],
  'browser-spike': ['e2e'],
  'browser-media': ['e2e'],
  'web-build': ['e2e'],
  'chromium-core': ['e2e'],
  'chromium-voice': ['e2e'],
  'chromium-web-1': ['e2e'],
  'chromium-web-2': ['e2e'],
  'chromium-web-3': ['e2e'],
  'firefox-core': ['e2e'],
  'firefox-voice': ['e2e'],
  'firefox-web-1': ['e2e'],
  'firefox-web-2': ['e2e'],
  'firefox-web-3': ['e2e'],
  'webkit-web': ['e2e'],
  deny: ['core'],
  go: ['go'],
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
};

/**
 * The `if:` expression ci.yml must carry for a job. It tests `!= 'false'`, not `== 'true'`, so an output that is
 * missing or empty (a renamed step id, say) runs the job instead of silently skipping it.
 */
export function gateExpression(job) {
  return JOB_GATES[job].map((output) => `needs.changes.outputs.${output} != 'false'`).join(' || ');
}

/** Whether a job runs for a set of outputs (as areasFor returns them): like the expression, anything but false runs it. */
export function jobRuns(job, outputs) {
  return JOB_GATES[job].some((output) => outputs[output] !== false);
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
 * The outputs for an event. Only a pull request is narrowed; a push (main) runs everything. `diff` returns the
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
