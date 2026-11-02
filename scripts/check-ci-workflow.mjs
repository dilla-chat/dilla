import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { gateExpression, JOB_GATES, jobRuns, reachableOutputs } from './ci-areas.mjs';
import { checkNativeStockFeasibility } from './check-native-stock-feasibility.mjs';

/**
 * Every job the workflow must carry (interfaces §5). Ruling L added the two Go jobs: they landed with
 * Plan B task 8, and leaving them merely "checked if present" meant deleting them was the one way to
 * make the Go half of the tree ungated without this gate saying a word. The `go` job additionally has
 * to declare its `needs: rust-wasi` edge (see below).
 */
export const REQUIRED_JOBS = [
  // The change filter and always-on docs gates; infrastructure remains active for docs-only changes too.
  'changes',
  'docs',
  'ci-watchdog',
  'ci-work-complete',
  // The aggregator: one check that is red when any job failed or was cancelled and green when the rest passed or were skipped by design.
  'ci-ok',
  'node',
  'ui',
  'rust-native',
  'rust-wasm-node',
  'rust-wasi',
  'vectors',
  'browser-spike',
  'browser-media',
  'deny',
  'go',
  'go-native',
  // internal/ds under -race in a job of its own (fix wave, CI run 36697379567): it is the slowest
  // package by far, and in the go job it pushed a 4-vCPU runner past the job's 25 minutes.
  'go-ds',
  // internal/api under -race in a job of its own (CI run 39486567482): ~620 s locally, it outran the go job's 20m package timeout.
  'go-api',
  // The scenario harness in its own job with its own timeout (final review): it needs both the
  // wasi core and the testkit binary, and in the go job it pushed a 4-vCPU runner past its budget.
  'go-harness',
  // go-fts5-arm64 is created by plan-1a task 2 (interfaces §9.2): it cross-builds the SQLite FTS5
  // path for arm64. If it is ever missing, the fix is to add it there, not to drop it from this list.
  'go-fts5-arm64',
  'go-lint',
  'go-vuln',
  'go-sqlc',
  'go-postgres',
  'go-release',
  // Build the bundled client once for Electron and retain shared encrypted-core
  // integration (BROWSER_WEB_JOBS below) without browser compatibility lanes.
  'web-build',
  'windows-kernel',
  'windows-installer',
  'linux-sandbox-foundation',
  'desktop',
];

/**
 * Infrastructure jobs are exempt from area gates. Every real test/build job needs `changes` and carries the `if:` that JOB_GATES
 * (scripts/ci-areas.mjs) names for it, so a docs-only pull request skips them. A skipped job counts as passed for a
 * required check. `docs` is exempt so the cheap docs gates still run; `changes` is the gate itself.
 */
export const CHANGE_GATE_EXEMPT = ['changes', 'docs', 'ci-watchdog', 'ci-work-complete', 'ci-ok'];

/**
 * R22: no escape hatch anywhere in the workflow. Both `always()` spellings are equally valid GitHub
 * Actions syntax and equally able to make a step run (and so a job pass) regardless of an earlier
 * failure, so the gate matches `always()` anywhere on an `if:` line rather than only the bare
 * `if: always()` form — `if: ${{ always() }}` is the more common of the two in practice.
 */
const FORBIDDEN = [
  { re: /run:.*playwright\s+install[^\n]*\b(?:firefox|webkit)\b/, what: 'unsupported browser engine in desktop-only CI' },
  { re: /\|\|\s*true/, what: '|| true' },
  { re: /continue-on-error/, what: 'continue-on-error' },
  { re: /if:.*always\(\)/, what: 'if: always()' },
  { re: /uses:\s*dtolnay\/rust-toolchain@/, what: 'third-party Rust toolchain action' },
  { re: /uses:\s*Swatinem\/rust-cache@/, what: 'third-party Rust cache action' },
];

/**
 * Ruling I8: a Go test that publishes video through the in-process LiveKit can trip the pinned
 * upstream race in updateRidsFromSDP, so it skips itself under -race (`if raceEnabled {`) and runs in
 * this step of the `go` job instead. assertRaceGatedTestsRunWithoutRace keeps the step and the gated
 * tests in step with each other: the pinned text alone would let a newly gated test, or a renamed
 * one, run nowhere.
 */
export const NON_RACE_STEP =
  "go test -timeout 5m ./cmd/dilla-loadrig ./cmd/dilla-mediabot ./internal/media ./internal/sfu -run 'TestALoopbackCellDecryptsEverything|TestTwoBotsDecryptEachOtherThroughTheSFU|TestGoPublisherToGoSubscriberDecryptsThroughTheSFU|TestTheSFUOfferCarriesOnlyTheDillaCodecs|TestPromotionAddsTheVideoSourcesAndDemotionRemovesThem'";

/** Shared core-worker/OPFS integration uses the Chromium engine embedded in
 * Electron. Product chat and voice acceptance run only in desktop.yml. */
export const WEB_SHARDS = 3;
export const BROWSER_WEB_JOBS = {
  'core-integration': { engine: 'chromium', suite: 'core', shard: null, timeout: 20 },
};
export const MIN_BROWSER_WEB_TIMEOUT = 20;

/** The testkit binary's env line, which every suite step that drives the native peer carries. */
const TESTKIT_ENV = 'DILLA_TESTKIT: ${{ github.workspace }}/artifacts/dilla-testkit';

/** The one suite command a browser job runs, a shard of the web suite carrying its `--shard=i/3` flag. */
export function browserSuiteCommand({ engine, suite, shard }) {
  return `npm run test:e2e:${suite}:${engine} -w @dilla/e2e${shard === null ? '' : ` -- --shard=${shard}/${WEB_SHARDS}`}`;
}

/** The steps every browser job must carry: the four downloads, its one engine, its one suite, its own artifact. */
function browserWebSteps(job, spec) {
  return [
    `timeout-minutes: ${spec.timeout}`,
    'name: dilla-core-wasi',
    'path: internal/mlswasi/testdata',
    'name: dilla-testkit',
    'name: dilla-web-dist',
    'path: packages/web/dist',
    'name: dilla-core-harness',
    'path: packages/client-core/harness-dist',
    `npx playwright install --with-deps ${spec.engine}`,
    browserSuiteCommand(spec),
    // The suite step's own env and (for Firefox) the audio server are checked as steps, not as substrings: checkSuiteSteps.
    TESTKIT_ENV,
    'if: failure()',
    // upload-artifact v4+ refuses a duplicate name within a run: one artifact name per job.
    `name: browser-web-results-${job}`,
    'path: e2e/test-results',
  ];
}

/** L-CI-22: the seven web-2c story families, shot at 1280×800 and at 360×740, each command a whole step of its own. */
const UI_PREFIXES_2C = 'trust-safetycompare-- trust-trustalert-- search-searchpalette-- status-typingindicator-- settings-diagnosticspanel-- shell-tabhand-over-- screens-trustandpolish--';
export const UI_SHOTS_2C = [
  `node packages/ui/scripts/shoot-stories.mjs 6027 target/ui-screens/web-2c/desktop ${UI_PREFIXES_2C}`,
  `node packages/ui/scripts/shoot-stories.mjs 6027 target/ui-screens/web-2c/phone ${UI_PREFIXES_2C} --viewport=360x740`,
];

/** web-3 task 11: the voice, ring and media-player story families at both viewports, uploaded from target/ui-screens/web-3. */
const UI_PREFIXES_3 = 'voice- media-mediaplayer-- screens-voice--';
export const UI_SHOTS_3 = [
  `node packages/ui/scripts/shoot-stories.mjs 6038 target/ui-screens/web-3/desktop ${UI_PREFIXES_3}`,
  `node packages/ui/scripts/shoot-stories.mjs 6038 target/ui-screens/web-3/phone ${UI_PREFIXES_3} --viewport=360x740`,
];

/** One load-bearing command per job, so a silently gutted job is caught. */
const REQUIRED_STEPS = {
  desktop: ['uses: ./.github/workflows/desktop.yml'],
  // The areas are computed by scripts/ci-areas.mjs (tested in ci-areas.test.mjs), which fails safe to everything.
  // fetch-depth: 0 gives it the base commit to diff against.
  changes: [
    'fetch-depth: 0',
    'node scripts/ci-areas.mjs',
    'EVENT_NAME: ${{ github.event_name }}',
    'BASE_SHA: ${{ github.event.pull_request.base.sha }}',
    ...['go', 'core', 'web', 'e2e', 'text', 'code'].map((area) => `${area}: \${{ steps.areas.outputs.${area} }}`),
  ],
  // The cheap docs gates, for every change.
  // Their unit tests and the real checks against the real files; check:ci reads the Go tree for the race and Postgres rules.
  docs: ['npm run test:docs-check', 'npm run test:brief-check', 'npm run test:copy-check', 'npm run check:docs', 'npm run check:brief', 'npm run check:copy', 'npm run check:ci'],
  // The final gate runs regardless of dependency results, alongside its no-op completion barrier.
  'ci-ok': ["contains(needs.*.result, 'failure')", "contains(needs.*.result, 'cancelled')"],
  // The testkit artifact hand-off: rust-native builds and uploads it, go downloads it.
  'rust-native': [
    'cargo fmt --all --check',
    'cargo clippy --workspace --all-targets --all-features --locked -- -D warnings',
    'cargo test --workspace --all-features --locked',
    'cargo build -p dilla-testkit --release --locked',
    'name: dilla-testkit',
    'if-no-files-found: error',
    // The lint and test steps need the core area; the build and upload run for any code (go-harness and the browser suites download it).
    "if: needs.changes.outputs.core != 'false'\n        run: cargo fmt --all --check",
    "if: needs.changes.outputs.core != 'false'\n        run: cargo clippy --workspace",
    "if: needs.changes.outputs.core != 'false'\n        run: |\n          sudo prlimit --pid \"$$\" --memlock=268435456:268435456\n          cargo test --workspace",
  ],
  'rust-wasm-node': [
    'bash scripts/install-ci-tool.sh wasm-bindgen',
    'bash scripts/install-ci-tool.sh wasm-pack',
    'cargo test -p dilla-core-wasm --target wasm32-unknown-unknown --locked --features vectors',
    // Ruling 40/task 26: lint wasm-only modules and typecheck the browser wrapper.
    'cargo clippy -p dilla-core-wasm --target wasm32-unknown-unknown --lib --locked -- -D warnings',
    '- run: rustup show active-toolchain',
    // Task 17 (dilla-media): the media worker's wasm-backed tests, the BaseE2EEManager type contract and the
    // JS half of the signalling-schema contract (DEV-66) run where the web wasm is built.
    'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/media/wasm',
    // web-1 task 2, C16: the browser core is built with the size profile and gated by L-CI-02.
    'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg',
    'node scripts/check-wasm-size.mjs packages/core-wasm/pkg/dilla_core_wasm_bg.wasm',
    'npm run typecheck -w @dilla/media',
    'npm run typecheck -w @dilla/core-wasm',
    'npm run test:wasm -w @dilla/media',
    'npm run check:schema -w @dilla/media',
  ],
  // Each entry must be a string the job carries and no other step of that job already contains.
  // `'dilla-core-wasi'` would be useless here: the cargo build line above already contains it, so the
  // artifact's `name:` would go unchecked. The three `with:` lines are named in full instead, which is
  // what makes step 9's argument — that `if-no-files-found: error` is what stops Plan B being handed
  // an empty directory — an enforced rule rather than prose.
  'rust-wasi': [
    'cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked',
    'actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7',
    'name: dilla-core-wasi',
    'path: target/wasm32-wasip1/release/dilla_core_wasi.wasm',
    'if-no-files-found: error',
  ],
  // The repository's pattern is that every scripts/check-*.mjs gate runs its own unit tests in the
  // `node` job (test:docs-check, test:brief-check, test:copy-check). This gate enforces that for
  // itself, so it cannot silently rot.
  // Task 26: exercise the manifest writer beside the other script gates.
  // install-release: deploy/install.sh's own tests (and shellcheck, which the runner image carries).
  node: ["- if: needs.changes.outputs.web != 'false'\n        run: npm test", "name: vectors are up to date\n        if: needs.changes.outputs.web != 'false'", 'npm run test:css-check', 'npm run check:css', 'npm run test:ci-check', 'npm run test:wasm-size-check', 'npm run test:manifest-writer', 'npm run test:install', "uses: actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0\n        if: needs.changes.outputs.text != 'false'", "name: profile-text tables are current\n        if: needs.changes.outputs.text != 'false'\n        run: node scripts/gen-profile-text-tables.mjs --download --check \"$RUNNER_TEMP/ucd\""],
  // Task 26/F16 (web-1), task 23 (web-2a), task 11 (web-2b): capture the seven story groups and three web-2b-only prefixes in all three themes.
  // Task 11 (web-2c, L-CI-22): the web-2c families at both viewports go up as a second artifact, both directories named.
  ui: [
    'node packages/ui/scripts/shoot-stories.mjs 6016 target/ui-screens/web-2b form- ceremony- shell- conversation- settings- screens- primitives-channelrow-- conversation-messagetoolbar-- conversation-attachmentcard-- screens-conversation--', 'name: ui-screens', 'path: target/ui-screens/web-2b',
    'name: ui-screens-web-2c', 'target/ui-screens/web-2c/desktop/**', 'target/ui-screens/web-2c/phone/**',
    // web-3 task 11: the voice families go up as a third artifact, from the web-3 directory.
    'name: ui-screens-web-3', 'path: target/ui-screens/web-3/**',
  ],
  vectors: [
    'npm run vectors',
    'git diff --exit-code -- protocol/vectors',
    'cargo run -p dilla-testkit --bin dilla-testkit --locked -- vectors',
  ],
  // Only the matrix run: it already includes the chromium project, so also running `test:e2e` would
  // execute the 10-hand-over test twice per CI run for no added signal.
  'browser-spike': [
    'bash scripts/install-ci-tool.sh wasm-bindgen',
    'bash scripts/install-ci-tool.sh wasm-pack',
    'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir spike/pkg',
    'npm run test:e2e:matrix -w @dilla/e2e',
    'timeout-minutes: 20',
  ],
  'browser-media': [
    'bash scripts/install-ci-tool.sh wasm-bindgen',
    'bash scripts/install-ci-tool.sh wasm-pack',
    'actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7',
    'if: failure()',
    'name: browser-media-results',
    'path: e2e/test-results',
    'if-no-files-found: error',
    'timeout-minutes: 25',
    'name: dilla-core-wasi',
    'path: internal/mlswasi/testdata',
    'name: dilla-testkit',
    'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/media/wasm',
    'go build -o target/dilla-mediabot ./cmd/dilla-mediabot',
    'node packages/media/scripts/extract-rnnoise-wasm.mjs',
    'npx playwright install --with-deps chromium',
    'sh scripts/ci-audio-server.sh',
    'npm run test:e2e:media -w @dilla/e2e',
    'unsupported-sfu-codec.spec.ts',
    "DILLA_MEDIA_SFU_AV1: '1'",
    'DILLA_TESTKIT: ${{ github.workspace }}/artifacts/dilla-testkit',
  ],
  // Task 26/C9: the release and both browser suites consume these same build artifacts.
  'web-build': [
    'bash scripts/install-ci-tool.sh wasm-bindgen',
    'bash scripts/install-ci-tool.sh wasm-pack',
    'timeout-minutes: 15',
    'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg',
    'node scripts/check-wasm-size.mjs packages/core-wasm/pkg/dilla_core_wasm_bg.wasm',
    'npm run typecheck -w @dilla/client-core',
    'npm run typecheck -w @dilla/web',
    'npm run typecheck:e2e',
    'npm run build -w @dilla/web',
    'npm run -w @dilla/web test:dist',
    'npm run build:harness -w @dilla/client-core',
    'go test -tags webdist ./internal/web/...',
    'name: dilla-web-dist',
    'path: packages/web/dist/',
    'name: dilla-core-harness',
    'path: packages/client-core/harness-dist/',
  ],
  // Task 26/C18: each engine and the native peer against the downloaded web and core builds, one job per engine.
  ...Object.fromEntries(Object.entries(BROWSER_WEB_JOBS).map(([job, spec]) => [job, browserWebSteps(job, spec)])),
  deny: ['cargo deny --all-features check advisories bans licenses sources'],
  // Ruling M. The first two lines are the hand-off from `rust-wasi`: without the download, or with it
  // landing anywhere but `internal/mlswasi/testdata`, the wazero tests skip themselves and the job is
  // green having proved nothing. The third is the only compile of the pinned LiveKit/wazero/sqlite
  // graph — `internal/deps` sits behind the `dillapins` tag precisely so no ordinary build pulls the
  // SFU in, which also means no other step in this workflow would notice that graph breaking. The
  // fourth is the race-detector run itself; it needs cgo, so it must never be reduced to a plain
  // `go test`.
  go: [
    'timeout-minutes: 30',
    'actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8',
    'path: internal/mlswasi/testdata',
    'CGO_ENABLED=0 go build -tags dillapins ./internal/deps',
    'go test -race -shuffle=on -timeout 20m $(go list ./... | grep -vx -e github.com/jonasthim/dilla/internal/ds -e github.com/jonasthim/dilla/internal/api)',
    'name: dilla-core-wasi',
    NON_RACE_STEP,
  ],
  // The one package the go job leaves out, with the wasi core it needs and a budget of its own.
  'go-ds': [
    'timeout-minutes: 55',
    'actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8',
    'name: dilla-core-wasi',
    'path: internal/mlswasi/testdata',
    'go test -race -shuffle=on -timeout 45m ./internal/ds/...',
  ],
  // The other package the go job leaves out, with the same wasi hand-off and a budget that leaves margin.
  'go-api': [
    'timeout-minutes: 55',
    'actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8',
    'name: dilla-core-wasi',
    'path: internal/mlswasi/testdata',
    'go test -race -shuffle=on -timeout 50m ./internal/api/...',
  ],
  // The harness job: both hand-offs, the binary, and a run that FAILS without it.
  'go-harness': [
    'timeout-minutes: 45',
    'actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8',
    'path: internal/mlswasi/testdata',
    'name: dilla-core-wasi',
    'name: dilla-testkit',
    'artifacts/dilla-testkit',
    'go test -race -shuffle=on -timeout 38m ./internal/testkit/...',
    'DILLA_TESTKIT',
    // `DILLA_TESTKIT` above is a prefix of this one, so both are named: without the second, a
    // missing binary would skip every scenario test and the job would stay green.
    'DILLA_TESTKIT_REQUIRED',
  ],
  // modernc.org/sqlite carries one generated translation unit per GOOS/GOARCH, so the FTS5
  // assertion is only actually *executed* on arm64 by a native arm64 runner (deviation B9): moved to
  // ubuntu-latest this job would silently re-run what the `go` job already ran.
  'go-fts5-arm64': ['runs-on: ubuntu-24.04-arm', 'go test ./internal/store/sqlite/...'],
  // The five W2-W5 gates. Each entry is a string the job carries and no other step of that job
  // already contains, so a silently gutted job is caught.
  'go-lint': ['bash scripts/install-ci-tool.sh golangci-lint', 'golangci-lint run ./...'],
  deny: ['bash scripts/install-ci-tool.sh cargo-deny', 'cargo deny --all-features check advisories bans licenses sources'],
  'go-vuln': [
    'golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...',
    '-scan package -tags dillapins ./internal/deps',
  ],
  'go-sqlc': ['bash scripts/install-ci-tool.sh sqlc', 'sqlc diff', 'git diff --exit-code'],
  'go-postgres': [
    'postgres:18.6-alpine3.24',
    '--locale-provider=builtin --builtin-locale=C.UTF-8',
    'DILLA_TEST_PG',
    // I7 (fix wave): the packages with a Postgres leg, internal/ops's backup, restore and heal tests
    // among them, with an explicit -timeout that fits the job's budget.
    'go test -race -shuffle=on -timeout 20m ./internal/store/... ./internal/ops/...',
    // web3.9: the AFK backfill and the channel-parent rules have Postgres legs in internal/api.
    "go test -race -shuffle=on -timeout 20m -run 'AFK|Afk|HiddenCategory|HiddenParent|ParentAnywhere|Marker|ConcurrentEnsures|ReadsThePointer' ./internal/api/...",
  ],
  // Task 26/C9: release verifies the downloaded client before embedding it.
  'go-release': ['CGO_ENABLED=0', 'if-no-files-found: error', 'name: dilla-web-dist', 'path: internal/web/dist', 'content="placeholder"', 'go test -tags webdist ./internal/web/...', 'DILLA_WEB_DIST: ${{ github.workspace }}/internal/web/dist'],
};

/**
 * The `go` job's test step is a module-wide wildcard on purpose: every new internal/… package is
 * picked up with zero workflow edits. Narrowing it to a package list is how a whole subsystem
 * silently stops being tested, so the exact text is pinned. It leaves out exactly two packages,
 * internal/ds and internal/api, by their full import paths, and only because the go-ds and go-api jobs (whose
 * steps are pinned in REQUIRED_STEPS) run them: the union of the two is still the whole module.
 */
function assertTestStepIsNotNarrowed(workflow, fail) {
  const line = 'go test -race -shuffle=on -timeout 20m $(go list ./... | grep -vx -e github.com/jonasthim/dilla/internal/ds -e github.com/jonasthim/dilla/internal/api)';
  if (!workflow.includes(line)) {
    fail(`the go job's test step must be exactly "${line}"`);
  }
}

/** Directories that hold no Go package of the module: dependencies, build output, dot-directories. */
const SKIP_DIRS = new Set(['node_modules', 'target', 'dist', 'testdata']);

/**
 * Every Go test the race detector skips: `{ pkg: './cmd/x', name: 'TestY' }` for each `if raceEnabled {`
 * in a *_test.go file under root, named by the Test function it sits in (null when it sits in none).
 */
export function raceGatedTests(root) {
  const found = [];
  const walk = (rel) => {
    for (const e of readdirSync(join(root, rel), { withFileTypes: true })) {
      if (e.isDirectory()) {
        if (!e.name.startsWith('.') && !SKIP_DIRS.has(e.name)) walk(rel === '' ? e.name : `${rel}/${e.name}`);
        continue;
      }
      if (!e.isFile() || !e.name.endsWith('_test.go')) continue;
      const text = readFileSync(join(root, rel, e.name), 'utf8');
      const gate = /^\s*if raceEnabled \{/gm;
      for (let m = gate.exec(text); m !== null; m = gate.exec(text)) {
        const funcs = [...text.slice(0, m.index).matchAll(/^func (\w+)\(/gm)];
        const name = funcs.at(-1)?.[1] ?? null;
        found.push({ pkg: `./${rel}`, name: name !== null && /^Test/.test(name) ? name : null, file: `${rel}/${e.name}` });
      }
    }
  };
  walk('');
  return found;
}

/**
 * web3.9: go-postgres runs internal/api through a -run regex, and `go test` passes when a regex matches nothing,
 * so a test it misses is silently not run on Postgres. Every Test* in internal/api's afk*_test.go and
 * channels_parent_test.go must match that regex.
 */
function assertPostgresAPITestsMatchRun(root, pgJob, problems) {
  const regex = /-run '([^']*)'[^\n]*\.\/internal\/api\/\.\.\./.exec(pgJob)?.[1];
  if (regex === undefined) return; // the missing line is reported by REQUIRED_STEPS
  let re;
  try {
    re = new RegExp(regex);
  } catch {
    problems.push('ci.yml: job "go-postgres"\'s internal/api -run regex is not a valid expression');
    return;
  }
  const dir = join(root, 'internal', 'api');
  let files;
  try {
    files = readdirSync(dir).filter((f) => /^(afk.*|channels_parent)_test\.go$/.test(f));
  } catch {
    return;
  }
  for (const f of files) {
    for (const m of readFileSync(join(dir, f), 'utf8').matchAll(/^func (Test\w+)\(/gm)) {
      if (!re.test(m[1])) problems.push(`ci.yml: job "go-postgres"'s internal/api -run does not match ${m[1]} (internal/api/${f})`);
    }
  }
}

/**
 * The `go` job's non-race step (the `go test` without -race that names its tests with -run) runs the
 * package of every race-gated test and names each one exactly, and names no test that is not gated:
 * a renamed or deleted test leaves a -run alternative that matches nothing, which go test passes.
 */
function assertRaceGatedTestsRunWithoutRace(root, goJob, problems) {
  const gated = raceGatedTests(root);
  const line = goJob.split('\n').find((l) => /\bgo test\b/.test(l) && !/-race\b/.test(l) && / -run '/.test(l));
  if (line === undefined) {
    if (gated.length > 0) {
      problems.push(`ci.yml: job "go" has no non-race "go test ... -run '...'" step for the ${gated.length} race-gated test(s)`);
    }
    return;
  }
  const pkgs = new Set(line.split(/\s+/).filter((w) => w.startsWith('./')));
  const names = new Set((/ -run '([^']*)'/.exec(line)?.[1] ?? '').split('|'));
  for (const g of gated) {
    if (g.name === null) {
      problems.push(`${g.file}: an "if raceEnabled {" gate outside a Test function; the non-race step cannot name it`);
      continue;
    }
    if (!pkgs.has(g.pkg)) problems.push(`ci.yml: job "go"'s non-race step does not run ${g.pkg}, the package of race-gated ${g.name}`);
    if (!names.has(g.name)) problems.push(`ci.yml: job "go"'s non-race step does not name race-gated ${g.name} (${g.pkg}) in -run`);
  }
  const known = new Set(gated.map((g) => g.name));
  for (const n of names) {
    if (!known.has(n)) problems.push(`ci.yml: job "go"'s non-race step names ${n || '(empty)'}, which is no race-gated test`);
  }
}

/**
 * Every `actions/upload-artifact` step sets `if-no-files-found: error` (Global Constraints: CI is
 * fail-closed). The per-job REQUIRED_STEPS pin it for the jobs that exist today; this rule is the
 * one that covers a job added tomorrow. A step is its `- ` list item and every line indented deeper.
 */
function assertEveryUploadFailsOnNoFiles(text, problems, file = 'ci.yml') {
  const lines = text.split('\n');
  for (let i = 0; i < lines.length; i++) {
    if (!/uses:\s*actions\/upload-artifact@/.test(lines[i])) continue;
    // The step's own indent: the column of its `- ` marker, which may be on this line or above it.
    let start = i;
    while (start > 0 && !/^\s*- /.test(lines[start])) start--;
    const indent = /^(\s*)- /.exec(lines[start])?.[1].length ?? 0;
    let end = i + 1;
    while (end < lines.length && lines[end].trim() !== '' && !new RegExp(`^\\s{0,${indent}}\\S`).test(lines[end])) end++;
    const step = lines.slice(start, end).join('\n');
    if (!/if-no-files-found:\s*error/.test(step)) {
      problems.push(`${file}:${i + 1}: actions/upload-artifact without "if-no-files-found: error" — CI is fail-closed`);
    }
  }
}

/** C16 / L-CI-05: also cover any wasm-pack build line added tomorrow. */
function assertWasmPackUsesTheSizeProfile(text, problems, file = 'ci.yml') {
  text.split('\n').forEach((line, i) => {
    if (!line.includes('wasm-pack build')) return;
    if (!line.includes('--profile wasm-release') || / --release(?:\s|$)/.test(line)) {
      problems.push(`${file}:${i + 1}: wasm-pack build must use --profile wasm-release, not --release (C16)`);
    }
  });
}

/**
 * Ruling of 2026-10-13 (Jonas; web-2c D5 in CI): the wasm C build (sqlite-wasm-rs compiles SQLite3MC through
 * cc) uses clang 23, the dev box's major. The runner's own clang made the same tree about 25 KB larger, so the
 * 3,450,000-byte budget measured different bytes in CI than on the box it was set on.
 *
 * The compiler comes from a third-party apt archive and its output ships (web-build's dist is embedded by
 * go-release), so the step is pinned: the archive key is checked by fingerprint on the very keyring
 * apt is pointed at, every llvm-toolchain-23 package is installed at one exact version and read back with
 * dpkg-query, and cc gets absolute paths, which a PATH entry added by a later action cannot shadow. A newer
 * upload to apt.llvm.org removes the pinned build from the archive and fails the step closed; the fix is to
 * re-pin here and in ci.yml, after checking the new build's size.
 *
 * The checker holds the canonical text and compares whole blocks, not substrings: a substring rule passes a
 * workflow that names the strings in a comment or another step while the job runs something else (the
 * validator differential of the d3429ce scanner lead). Outside the canonical lines, no workflow line may name
 * a variable cc reads for the compiler, archiver or flags, no wasm job may write GITHUB_ENV or GITHUB_PATH,
 * and the workflow may use no YAML anchor, alias or merge key, whose expansion a line-based checker cannot see.
 */
export const WASM_LLVM_VERSION = '23.1';
export const WASM_LLVM_KEY_FPR = '6084F3CF814B57C1CF12EFD515CF4D18AF4F7421';
const WASM_LLVM_PACKAGES = [
  'clang-23', 'llvm-23', 'libllvm23', 'libclang-cpp23', 'libclang1-23', 'libclang-common-23-dev',
  'llvm-23-linker-tools', 'llvm-23-runtime',
];
export const WASM_CLANG_ENV_BLOCK = [
  '    env:',
  '      CC_wasm32_unknown_unknown: /usr/bin/clang-23',
  '      AR_wasm32_unknown_unknown: /usr/bin/llvm-ar-23',
].join('\n');
export const WASM_CLANG_STEP = [
  '      - name: clang 23 for the wasm C build',
  '        run: |',
  '          test "$(lsb_release -cs)" = noble',
  '          curl -fsSL https://apt.llvm.org/llvm-snapshot.gpg.key -o "$RUNNER_TEMP/llvm-snapshot.asc"',
  '          gpg --dearmor -o "$RUNNER_TEMP/apt-llvm.gpg" "$RUNNER_TEMP/llvm-snapshot.asc"',
  '          primaries=$(gpg --show-keys --with-colons "$RUNNER_TEMP/apt-llvm.gpg" | awk -F: \'$1 == "pub" { want = 1; next } $1 == "fpr" && want { print $10; want = 0 }\')',
  `          test "$primaries" = ${WASM_LLVM_KEY_FPR}`,
  '          sudo install -m 0644 "$RUNNER_TEMP/apt-llvm.gpg" /usr/share/keyrings/apt-llvm.gpg',
  '          cmp "$RUNNER_TEMP/apt-llvm.gpg" /usr/share/keyrings/apt-llvm.gpg',
  '          echo "deb [signed-by=/usr/share/keyrings/apt-llvm.gpg] https://apt.llvm.org/noble/ llvm-toolchain-noble-23 main" | sudo tee /etc/apt/sources.list.d/apt-llvm-23.list',
  '          sudo apt-get update',
  '          apt-cache policy clang-23 llvm-23',
  '          v=$(apt-cache policy clang-23 | awk \'/Candidate:/ { print $2 }\')',
  `          case "$v" in 1:${WASM_LLVM_VERSION}.*~++*) ;; *) echo "expected clang ${WASM_LLVM_VERSION}.x candidate, got $v"; exit 1 ;; esac`,
  `          sudo apt-get install -y --no-install-recommends ${WASM_LLVM_PACKAGES.map((p) => `"${p}=$v"`).join(' ')}`,
  `          for p in ${WASM_LLVM_PACKAGES.join(' ')}; do test "$(dpkg-query -W -f='\${Version}' "$p")" = "$v"; done`,
  `          /usr/bin/clang-23 --version | grep -F "clang version ${WASM_LLVM_VERSION}."`,
  '          /usr/bin/clang-23 --version',
  '          /usr/bin/llvm-ar-23 --version',
].join('\n');

/** A job builds for the browser target when it names it or runs wasm-pack; the cache key alone also counts. */
const WASM_JOB = /wasm32[-_]unknown[-_]unknown|wasm-pack/;
/** Every variable cc consults for the compiler, the archiver or their flags, in any target spelling. */
const CC_VARIABLE =
  /\b(?:CC|CXX|AR|RANLIB|CFLAGS|CXXFLAGS|CPPFLAGS|ARFLAGS|TARGET_CC|TARGET_CXX|TARGET_AR|TARGET_CFLAGS|TARGET_CXXFLAGS|TARGET_ARFLAGS|HOST_CC|HOST_AR|HOST_CFLAGS|CRATE_CC_NO_DEFAULTS|CC_SHELL_ESCAPED_FLAGS|CC_FORCE_DISABLE)(?:[-_]wasm32[-_]unknown[-_]unknown)?\b/;
/** A YAML anchor (&name), alias (*name) or merge key (<<:) outside a quoted glob. */
const YAML_INDIRECTION = /(?:^|[\s:[,{-])[&*][A-Za-z0-9_-]+(?=[\s,\]}]|$)|<<\s*:/;

/** The `- ` items of a job's `steps:` list, each as its exact lines, trailing blank lines dropped. */
function stepItems(lines) {
  const items = [];
  const at = lines.indexOf('    steps:');
  if (at === -1) return items;
  for (const line of lines.slice(at + 1)) {
    if (/^ {6}- /.test(line)) items.push([line]);
    else if (items.length > 0 && (line === '' || /^ {7,}\S/.test(line))) items.at(-1).push(line);
    else if (/^ {0,5}\S/.test(line)) break;
  }
  return items.map((item) => {
    while (item.at(-1) === '') item.pop();
    return item.join('\n');
  });
}

function assertWasmJobsUseClang23(text, jobs, problems, file = 'ci.yml') {
  const canonicalEnv = new Set(WASM_CLANG_ENV_BLOCK.split('\n').slice(1));
  const wasmJobs = Object.entries(jobs).filter(([, body]) => WASM_JOB.test(body));
  const allowedEnvLines = new Set();
  for (const [name, body] of wasmJobs) {
    const lines = body.split('\n');
    const envAt = lines.indexOf('    env:');
    const envLines = envAt === -1 ? [] : lines.slice(envAt, lines.findIndex((l, i) => i > envAt && !/^ {6,}\S/.test(l)));
    if (lines.filter((l) => /^ {4}env:/.test(l)).length !== 1 || envLines.join('\n') !== WASM_CLANG_ENV_BLOCK) {
      problems.push(`${file}: job "${name}" builds for wasm32-unknown-unknown; its job-level env must be exactly:\n${WASM_CLANG_ENV_BLOCK}`);
    } else {
      for (let i = envAt + 1; i < envAt + envLines.length; i++) allowedEnvLines.add(`${name}:${i}`);
    }
    const steps = stepItems(lines);
    if (lines.filter((l) => /^ {4}steps:/.test(l)).length !== 1 || steps[0] !== '      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7' || steps[1] !== WASM_CLANG_STEP) {
      problems.push(`${file}: job "${name}" builds for wasm32-unknown-unknown; its second step, right after actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7, must be exactly the pinned clang 23 step (WASM_CLANG_STEP)`);
    }
    if (steps.filter((s) => s === WASM_CLANG_STEP).length > 1) {
      problems.push(`${file}: job "${name}" carries the clang 23 step more than once`);
    }
    if (/GITHUB_ENV|GITHUB_PATH/.test(body)) {
      problems.push(`${file}: job "${name}" builds for wasm32-unknown-unknown and writes GITHUB_ENV or GITHUB_PATH, which could replace the pinned compiler`);
    }
  }
  // Line by line over the whole file: a compiler variable anywhere else (workflow env, a step env, a flow
  // mapping, a quoted key, the hyphenated target spelling cc reads first) could override the job's.
  const wasmLineOwner = [];
  let job = null;
  let jobLine = 0;
  text.split('\n').forEach((line, i) => {
    const key = /^ {2}([A-Za-z0-9_-]+):\s*$/.exec(line);
    if (key) {
      job = key[1];
      jobLine = -1;
    }
    wasmLineOwner[i] = job === null ? null : `${job}:${jobLine}`;
    jobLine++;
    if (YAML_INDIRECTION.test(line)) problems.push(`${file}:${i + 1}: YAML anchors, aliases and merge keys are not allowed: the checker could not see what they expand to`);
    if (!CC_VARIABLE.test(line)) return;
    if (canonicalEnv.has(line) && allowedEnvLines.has(wasmLineOwner[i])) return;
    problems.push(`${file}:${i + 1}: names a C toolchain variable outside the canonical clang 23 job env`);
  });
}

/** A job body's steps: each `      - ` item's lines, trimmed, its first line without the dash (`run: …`, `uses: …`).
 *  Comment lines are dropped, so no rule matched against steps is satisfied by a comment. */
export function jobSteps(body) {
  const steps = [];
  for (const line of body.split('\n')) {
    const t = line.trim();
    if (t === '' || t.startsWith('#')) continue;
    if (/^ {6}- \S/.test(line)) steps.push([t.slice(2)]);
    else if (steps.length > 0 && /^ {8,}\S/.test(line)) steps[steps.length - 1].push(t);
  }
  return steps;
}

// Loading the complete module on Linux includes the cgo GTK setup frontend.
// Keep it in vet/lint/vulnerability/race coverage, including future full-tree
// jobs: provide headers before loading rather than hiding the package with cgo.
function checkWholeModuleGTK(jobs, problems) {
  const loadsAll = line => /(?:\bgo\s+(?:vet|test|list|build)\b|\bgolangci-lint\s+run\b|\bgovulncheck(?:@\S+)?\b)[^\n]*\s\.\/\.\.\.(?=\s|$|[);|])/.test(line);
  for (const [name, body] of Object.entries(jobs)) {
    if (!/^ {4}runs-on: ubuntu-\S+$/m.test(body)) continue;
    const steps = jobSteps(body), consumers = steps.map((step, index) => step.some(loadsAll) ? index : -1).filter(index => index >= 0);
    if (!consumers.length) continue;
    const gtk = steps.findIndex(step => step.length === 1 && step[0] === 'run: sudo apt-get update && sudo apt-get install -y libgtk-3-dev');
    if (gtk < 0 || consumers.some(index => index <= gtk)) problems.push(`ci.yml: job "${name}" must install GTK development headers before loading all Go packages`);
    const header = body.split(/^ {4}steps:/m)[0];
    if (/\bCGO_ENABLED:\s*['"]?0\b/.test(header) || consumers.some(index => steps[index].some(line => !/^(?:name|id):/.test(line) && /\bCGO_ENABLED(?:=|:\s*['"]?)0\b|^if:/.test(line)))) {
      problems.push(`ci.yml: job "${name}" must retain cgo GTK frontend coverage in whole-module checks`);
    }
  }
}

// This cheap prerequisite must stay native and run both complete Go packages.
// An exact bounded job shape also rejects filters, conditional steps, GOOS
// overrides, and asset provisioning that would hide or delay the kernel gate.
function checkWindowsKernelJob(jobs, problems) {
  if (!jobs['windows-kernel']) return;
  const expected = `needs: [changes]
if: needs.changes.outputs.e2e != 'false'
runs-on: windows-2025
timeout-minutes: 10
steps:
- uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7
with:
persist-credentials: false
- uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7
with:
go-version-file: go.mod
- run: go test -count=1 -failfast -v -timeout=2m ./internal/desktoplaunch ./packages/desktop/native/windows-launcher`;
  const actual = jobs['windows-kernel'].split('\n').map(line => line.trim()).filter(line => line && !line.startsWith('#')).join('\n');
  if (actual !== expected) problems.push('ci.yml: early Windows kernel gate must run only pinned checkout/setup-go and both unfiltered native Go packages on Windows within its fixed budget');
  const desktopNeeds = /^ {4}needs: \[([^\]\n]*)\]$/m.exec(jobs.desktop ?? '')?.[1].split(',').map(name => name.trim()) ?? [];
  if (!desktopNeeds.includes('windows-kernel')) {
    problems.push('ci.yml: desktop must wait for the early Windows kernel gate via needs: windows-kernel');
  }
}

// Narrow cancellation finalizers for the reviewed disposable fixture only.
// Full job shape below also rejects reordered, duplicate or extra steps.
const SANDBOX_MAINTENANCE_CONTROLS = [
  "      - name: Portable sandbox maintenance controls",
  "        if: matrix.lane == 'managed'",
  "        run: node --test packages/desktop/test/linux-sandbox-maintenance.test.mjs",
  "      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7",
  "        if: matrix.lane == 'managed'",
  "        with:",
  "          go-version-file: go.mod",
  "      - name: Root sandbox maintenance kernel proof",
  "        id: sandbox_maintenance",
  "        if: matrix.lane == 'managed'",
  "        timeout-minutes: 8",
  "        run: node --test packages/desktop/test/native/linux-sandbox-maintenance.test.mjs",
  "        env:",
  "          DILLA_LINUX_SANDBOX_FOUNDATION_LANE: managed",
].join('\n');
const SANDBOX_MAINTENANCE_CLEANUP = [
  "      - name: Cleanup root sandbox maintenance fixture",
  "        if: always() && matrix.lane == 'managed' && contains(fromJSON('[\"success\",\"failure\",\"cancelled\"]'), steps.sandbox_maintenance.outcome)",
  "        timeout-minutes: 1",
  "        run: node packages/desktop/test/native/linux-sandbox-maintenance.test.mjs --cleanup-only",
  "        env:",
  "          DILLA_LINUX_SANDBOX_FOUNDATION_LANE: managed",
].join('\n');
const SANDBOX_MAINTENANCE_ARTIFACT = [
  "      - name: Preserve root sandbox maintenance proof",
  "        if: always() && matrix.lane == 'managed' && contains(fromJSON('[\"success\",\"failure\",\"cancelled\"]'), steps.sandbox_maintenance.outcome)",
  "        timeout-minutes: 1",
  "        uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7",
  "        with:",
  "          name: linux-sandbox-maintenance-managed",
  "          path: packages/desktop/test-results/linux-sandbox-maintenance-managed.json",
  "          if-no-files-found: error",
  "          retention-days: 3",
].join('\n');

// Keep this feasibility proof independent of asset builds and long Windows tests.
// Exact native lanes/commands prohibit extracted execution and host-policy overrides.
function checkLinuxSandboxFoundationJob(jobs, problems) {
  if (!jobs['linux-sandbox-foundation']) return;
  const expected = [
    "needs: [changes]",
    "if: needs.changes.outputs.e2e != 'false'",
    "name: linux-sandbox-foundation-${{ matrix.lane }}",
    "runs-on: ${{ matrix.os }}",
    "timeout-minutes: 15",
    "strategy:",
    "fail-fast: true",
    "matrix:",
    "include:",
    "- { os: ubuntu-22.04, lane: namespace }",
    "- { os: ubuntu-24.04, lane: managed }",
    "steps:",
    "- uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7",
    "with:",
    "persist-credentials: false",
    "- uses: actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1 # v7",
    "with:",
    "node-version-file: .nvmrc",
    "cache: npm",
    "- run: |",
    "sudo apt-get update",
    "case \"$(lsb_release -rs)\" in",
    "22.04) sudo apt-get install -y xvfb dbus-x11 libnss3 libgbm1 libgtk-3-0 libasound2 libfuse2 ;;",
    "24.04) sudo apt-get install -y xvfb dbus-x11 libnss3 libgbm1 libgtk-3-0t64 libasound2t64 libfuse2t64 ;;",
    "*) exit 1 ;;",
    "esac",
    "- run: npm ci",
    "- run: node node_modules/electron/install.js",
    ...SANDBOX_MAINTENANCE_CONTROLS.split('\n').map(line => line.trim()),
    ...SANDBOX_MAINTENANCE_CLEANUP.split('\n').map(line => line.trim()),
    ...SANDBOX_MAINTENANCE_ARTIFACT.split('\n').map(line => line.trim()),
    "- id: sandbox_foundation",
    "run: xvfb-run -a dbus-run-session -- node --test packages/desktop/test/native/linux-sandbox-foundation.test.mjs",
    "env:",
    "DILLA_LINUX_SANDBOX_FOUNDATION_LANE: ${{ matrix.lane }}",
    "- if: always() && contains(fromJSON('[\"success\",\"failure\",\"cancelled\"]'), steps.sandbox_foundation.outcome)",
    "uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7",
    "with:",
    "name: linux-sandbox-foundation-${{ matrix.lane }}",
    "path: packages/desktop/test-results/linux-sandbox-foundation-${{ matrix.lane }}.json",
    "if-no-files-found: error",
  ].join('\n');
  const actual = jobs['linux-sandbox-foundation'].split('\n').map(line => line.trim()).filter(line => line && !line.startsWith('#')).join('\n');
  if (actual !== expected) problems.push('ci.yml: Linux sandbox foundation must run both fixed native Ubuntu lanes with unchanged host policy, pinned provisioning and the unfiltered FUSE feasibility fixture');
  const needs = /^ {4}needs: \[([^\]\n]*)\]$/m.exec(jobs.desktop ?? '')?.[1].split(',').map(name => name.trim()) ?? [];
  if (!needs.includes('linux-sandbox-foundation')) problems.push('ci.yml: desktop must wait for Linux sandbox foundation');
}

// This native fixture has no bundled-web/WASI prerequisite. Keep both complete
// lifecycle files and the Go/NSIS interop control independent of the desktop matrix.
// Short controls must finish in their own step before the long maintenance test,
// so its runtime cannot hide an earlier file's failure from the step API.
// Exact shape rejects filtered modes, ignored failures and extra authority/env.
function checkWindowsInstallerJob(jobs, problems) {
  if (!jobs['windows-installer']) return;
  const expected = `needs: [changes, windows-kernel]
if: needs.changes.outputs.e2e != 'false'
runs-on: windows-2025
timeout-minutes: 25
steps:
- uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7
with:
persist-credentials: false
- uses: actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1 # v7
with:
node-version-file: .nvmrc
cache: npm
- uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7
with:
go-version-file: go.mod
- run: npm ci
- run: node node_modules/electron/install.js
- run: node packages/desktop/scripts/prepare-nsis.mjs
- run: node --test packages/desktop/test/native/windows-installer-syntax.test.mjs packages/desktop/test/windows-operation-gate.test.ts
- run: npm test -w @dilla/desktop
- id: launcher_smoke
run: node packages/desktop/scripts/windows-launcher-smoke.mjs --capture-coff
- if: failure() && steps.launcher_smoke.outcome == 'failure'
uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
with:
name: windows-launcher-smoke-coff
path: packages/desktop/test-results/windows-launcher-smoke-failure.syso
if-no-files-found: error
- run: node --test packages/desktop/test/native/windows-installer-maintenance.test.mjs`;
  const actual = jobs['windows-installer'].split('\n').map(line => line.trim()).filter(line => line && !line.startsWith('#')).join('\n');
  if (actual !== expected) problems.push('ci.yml: independent Windows installer gate must run pinned provisioning, short syntax/interop controls, unconditional desktop units, launcher smoke and full unfiltered maintenance in order within its fixed budget');
  const desktopNeeds = /^ {4}needs: \[([^\]\n]*)\]$/m.exec(jobs.desktop ?? '')?.[1].split(',').map(name => name.trim()) ?? [];
  if (desktopNeeds.includes('windows-installer')) {
    problems.push('ci.yml: desktop must not wait for the independent Windows installer maintenance gate');
  }
}

/**
 * Each browser job is a `run:` step of its own for its one suite, carrying the testkit binary in its own env, and it runs
 * no other `test:e2e` suite (a job folded back into several suites would hide a split job's budget). In Firefox an
 * audio-server `run:` step comes before the web and voice suites (headless Firefox's AudioContext stays suspended
 * without one, as in browser-media; web3.9 CI2: the web suite places calls too, and a suspended context reads no level, so
 * the fake microphone's tone counts as speech). Matched on steps, never on substrings, so a commented-out step or a
 * comment naming the command satisfies nothing.
 */
function checkSuiteSteps(jobs, problems) {
  for (const [job, spec] of Object.entries(BROWSER_WEB_JOBS)) {
    if (!(job in jobs)) continue;
    const steps = jobSteps(jobs[job]);
    const command = browserSuiteCommand(spec);
    const suiteAt = steps.findIndex((s) => s[0] === `run: ${command}`);
    if (suiteAt < 0) problems.push(`ci.yml: job "${job}" is missing: ${command} (a run step of its own)`);
    else if (!steps[suiteAt].includes(TESTKIT_ENV)) {
      problems.push(`ci.yml: job "${job}" runs ${command} without its own env ${TESTKIT_ENV}`);
    }
    const suites = steps.filter((s) => /^run: .*test:e2e/.test(s[0]));
    if (suites.length !== 1 || (suiteAt >= 0 && suites[0] !== steps[suiteAt])) {
      problems.push(`ci.yml: job "${job}" must run exactly one Playwright suite, ${command}, and no other test:e2e step`);
    }
    if (spec.engine !== 'firefox' || spec.suite === 'core') continue;
    const audioAt = steps.findIndex((s) => s[0] === 'run: sh scripts/ci-audio-server.sh');
    if (audioAt < 0) problems.push(`ci.yml: job "${job}" is missing: sh scripts/ci-audio-server.sh (a run step of its own)`);
    else if (suiteAt >= 0 && audioAt > suiteAt) problems.push(`ci.yml: ${job} audio server must start before the ${spec.suite} suite`);
  }
}

/** Splits the `jobs:` mapping into `{ name: body }` by two-space job keys. */
function splitJobs(text) {
  const lines = text.split('\n');
  const start = lines.findIndex((l) => l === 'jobs:');
  const jobs = {};
  if (start < 0) return jobs;
  let current = null;
  for (const line of lines.slice(start + 1)) {
    const key = /^ {2}([A-Za-z0-9_-]+):\s*$/.exec(line);
    if (key !== null) {
      current = key[1];
      jobs[current] = [];
      continue;
    }
    if (current !== null) jobs[current].push(line);
  }
  return Object.fromEntries(Object.entries(jobs).map(([k, v]) => [k, v.join('\n')]));
}

function checkActionReference(line, file, number, problems) {
  // The line-oriented checker supports block-style steps only. GitHub also
  // accepts flow-style YAML, so refuse it explicitly instead of silently
  // skipping a `uses` value hidden inside `{ ... }`.
  if (/^\s*-\s*\{[^#\n]*\buses\s*:/.test(line)) {
    problems.push(`${file}:${number}: flow-style action steps are unsupported; use a block-style step`);
    return;
  }
  const match = /^\s*(?:-\s*)?uses:\s*(\S+)/.exec(line);
  if (!match) return;
  const action = match[1];
  if (action === './.github/workflows/desktop.yml' && file === 'ci.yml') return;
  if (!action.startsWith('actions/')) {
    problems.push(`${file}:${number}: ${action} is not a GitHub-owned action; use a reviewed local command`);
  } else if (!/^actions\/[\w-]+(?:\/[\w-]+)?@[0-9a-f]{40}$/.test(action)) {
    problems.push(`${file}:${number}: ${action} must use a 40-hex commit SHA`);
  }
}

export function checkWorkflow(root) {
  const path = join(root, '.github', 'workflows', 'ci.yml');
  const text = readFileSync(path, 'utf8');
  const problems = [];

  let currentJob = null;
  const workflowLines = text.split('\n');
  workflowLines.forEach((line, i) => {
    checkActionReference(line, 'ci.yml', i + 1, problems);
    const jobKey = /^ {2}([A-Za-z0-9_-]+):\s*$/.exec(line);
    if (jobKey !== null) currentJob = jobKey[1];
    for (const { re, what } of FORBIDDEN) {
      // Only the final gate and its no-op DAG barrier may run regardless of dependency results.
      if (what === 'if: always()' && ['ci-ok', 'ci-work-complete'].includes(currentJob) && line === '    if: always()') continue;
      // Preserve only the bounded sandbox observation file after a failed
      // native test. This action cannot execute or bypass an acceptance step.
      if (what === 'if: always()' && currentJob === 'linux-sandbox-foundation' && workflowLines.slice(i, i + 6).join('\n') === [
        "      - if: always() && contains(fromJSON('[\"success\",\"failure\",\"cancelled\"]'), steps.sandbox_foundation.outcome)",
        '        uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7',
        '        with:',
        '          name: linux-sandbox-foundation-${{ matrix.lane }}',
        '          path: packages/desktop/test-results/linux-sandbox-foundation-${{ matrix.lane }}.json',
        '          if-no-files-found: error',
      ].join('\n')) continue;
      if (what === 'if: always()' && currentJob === 'linux-sandbox-foundation' &&
          [SANDBOX_MAINTENANCE_CLEANUP, SANDBOX_MAINTENANCE_ARTIFACT].some(block =>
            workflowLines.slice(i - 1, i - 1 + block.split('\n').length).join('\n') === block)) continue;
      if (re.test(line)) problems.push(`ci.yml:${i + 1}: forbidden "${what}" — CI is fail-closed (R22)`);
    }
    // v0.1.0-web3.1: only release.yml stamps a version; a ci build is 0.0.0-dev, so it is never mistaken for a release.
    // A quoted symbol (-X 'main.Version=…', -X "main.Version=…") and the -X= spelling stamp it all the same.
    if (/-X[\s=]*['"]?main\.Version/.test(line)) problems.push(`ci.yml:${i + 1}: stamps main.Version; ci builds keep 0.0.0-dev (release.yml stamps the tag)`);
  });

  if (!/^concurrency:$/m.test(text) || !/cancel-in-progress: true/.test(text)) {
    problems.push('ci.yml: missing the workflow-level concurrency block with cancel-in-progress: true');
  }
  if (!/^concurrency:\n {2}group: \$\{\{ github\.workflow \}\}-\$\{\{ github\.ref \}\}\n {2}cancel-in-progress: true$/m.test(text)) {
    problems.push('ci.yml: concurrency group must be workflow/ref so newer commits cancel obsolete runs for the same pull request');
  }
  if (!/^permissions:\n {2}contents: read$/m.test(text)) {
    problems.push('ci.yml: the workflow-level token must be read-only (permissions: contents: read)');
  }
  // gap-29 §4 ii: a workflow-level RUSTFLAGS overrides every target.*.rustflags outright.
  if (/^\s{2}RUSTFLAGS:/m.test(text)) {
    problems.push('ci.yml: workflow-level RUSTFLAGS overrides .cargo/config.toml — use cargo clippy -- -D warnings');
  }
  if (!/^\s{2}CARGO_TERM_COLOR: always$/m.test(text)) {
    problems.push('ci.yml: missing env CARGO_TERM_COLOR: always');
  }

  const jobs = splitJobs(text);
  if ('ci-watchdog' in jobs) {
    const body = jobs['ci-watchdog'];
    const required = [
      "    if: (github.event_name == 'push' && github.ref == 'refs/heads/main') || (github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name == github.repository)",
      '    timeout-minutes: 90',
      '    permissions:\n      contents: read\n      actions: write',
      '          persist-credentials: false',
      '      - run: python3 scripts/ci-watchdog.py\n        env:\n          GH_TOKEN: ${{ github.token }}',
    ];
    const permissions = /^    permissions:\n((?:      [^\n]+\n)+)/m.exec(body)?.[1];
    const steps = jobSteps(body);
    const expectedSteps = [
      ['uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7', 'with:', 'persist-credentials: false'],
      ['run: python3 scripts/ci-watchdog.py', 'env:', 'GH_TOKEN: ${{ github.token }}'],
    ];
    if (required.some(value => !body.includes(value)) || /^    (?:needs|name):/m.test(body) || /secrets\./.test(body) ||
        permissions !== '      contents: read\n      actions: write\n' || JSON.stringify(steps) !== JSON.stringify(expectedSteps)) {
      problems.push('ci.yml: unsafe watchdog: require an independent, bounded, same-repository watcher with only its job token and credential-free checkout');
    }
  }
  for (const name of [...REQUIRED_JOBS, ...Object.keys(BROWSER_WEB_JOBS)]) {
    if (!(name in jobs)) {
      problems.push(`ci.yml: missing job "${name}"`);
      continue;
    }
    for (const step of REQUIRED_STEPS[name] ?? []) {
      if (!jobs[name].includes(step)) problems.push(`ci.yml: job "${name}" is missing: ${step}`);
    }
  }
  checkWindowsKernelJob(jobs, problems);
  checkWholeModuleGTK(jobs, problems);
  checkWindowsInstallerJob(jobs, problems);
  checkLinuxSandboxFoundationJob(jobs, problems);

  // Every test/build job needs `changes` and carries its gate, or it would run on a docs-only pull
  // request (or, with a wrong condition, be skipped on code).
  for (const [name, body] of Object.entries(jobs)) {
    const needs = /^ {4}needs:\s*\[([^\]]*)\]\s*$/m.exec(body);
    const needList = needs === null ? [] : needs[1].split(',').map((s) => s.trim());
    if (name === 'ci-work-complete') {
      const real = Object.keys(jobs).filter(other => !['ci-ok', 'ci-watchdog', 'ci-work-complete'].includes(other));
      if (needList.length !== real.length || real.some(other => !needList.includes(other)) ||
          !body.split('\n').includes('    if: always()') ||
          !body.includes('    timeout-minutes: 2') ||
          jobSteps(body).length !== 1 || !body.includes('      - run: echo "All CI work reached a terminal result"')) {
        problems.push('ci.yml: completion barrier must wait for every real job, exclude the watcher/final gate, and perform only its bounded no-op');
      }
      continue;
    }
    if (name === 'ci-ok') {
      const others = Object.keys(jobs).filter(other => other !== 'ci-ok');
      if (needList.length !== others.length || new Set(needList).size !== others.length || needList.some(other => !others.includes(other))) {
        problems.push('ci.yml: job "ci-ok" must list every other job exactly once in its needs:');
      }
      if (!body.split('\n').includes('    if: always()')) problems.push('ci.yml: job "ci-ok" must carry the job-level if: always(), or it is skipped when a job fails');
      if (!body.includes("      - if: needs.ci-watchdog.result == 'skipped' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)\n        run: exit 1")) {
        problems.push('ci.yml: job "ci-ok" may accept a skipped watcher only for fork pull requests');
      }
      for (const other of Object.keys(jobs)) {
        if (other !== 'ci-ok' && !needList.includes(other)) problems.push(`ci.yml: job "ci-ok" must list "${other}" in its needs:`);
      }
      continue;
    }
    if (CHANGE_GATE_EXEMPT.includes(name)) {
      if (name === 'docs' && (needList.includes('changes') || /^ {4}if:/m.test(body))) problems.push(`ci.yml: job "${name}" must run for every change: no needs: changes, no job-level if:`);
      continue;
    }
    if (!(name in JOB_GATES)) {
      problems.push(`ci.yml: job "${name}" has no gate in JOB_GATES (scripts/ci-areas.mjs)`);
      continue;
    }
    if (!needList.includes('changes')) problems.push(`ci.yml: job "${name}" must list "changes" in its needs:`);
    if (!body.split('\n').includes(`    if: ${gateExpression(name)}`)) problems.push(`ci.yml: job "${name}" must carry the job-level if: ${gateExpression(name)}`);
    // A job whose needs are skipped is skipped too, so its gate may not be true where a needed job's is false.
    for (const need of needList) {
      if (!(need in JOB_GATES)) continue;
      if (reachableOutputs().some((outputs) => jobRuns(name, outputs) && !jobRuns(need, outputs))) {
        problems.push(`ci.yml: job "${name}" can run while the job it needs, "${need}", is skipped`);
      }
    }
  }

  // The desktop command is a prefix of the phone one, so each must be a whole `- run:` line of the ui job.
  if ('ui' in jobs) {
    const runs = jobs.ui.split('\n').map((line) => line.trim());
    for (const command of UI_SHOTS_2C) {
      if (!runs.includes(`- run: ${command}`)) problems.push(`ci.yml: job "ui" is missing the web-2c screenshot step: ${command}`);
    }
    for (const command of UI_SHOTS_3) {
      if (!runs.includes(`- run: ${command}`)) problems.push(`ci.yml: job "ui" is missing the web-3 screenshot step: ${command}`);
    }
  }

  assertTestStepIsNotNarrowed(text, (msg) => problems.push(`ci.yml: ${msg}`));
  assertEveryUploadFailsOnNoFiles(text, problems);
  assertWasmPackUsesTheSizeProfile(text, problems);
  assertWasmJobsUseClang23(text, jobs, problems);

  // Jonas, 2026-10-21: no container image is built or published; deploy/install.sh is the one install.
  if (/docker\/|ghcr\.io|packages: write|build-ci-image|\bimage:\s*\$\{\{/.test(text)) problems.push('ci.yml: builds or pushes a container image; the container image was dropped, deploy/install.sh is the one install');

  // Plan B task 8 owns the `go` job; this plan owns `rust-wasi`. `actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8`
  // fetches from the same workflow run, so GitHub schedules `go` after `rust-wasi` only if a `needs:`
  // says so, and whichever plan lands second has to add the edge. Nobody owned that rule, so it lives
  // here. The job is required above; the guard keeps this block from piling a second, confusing
  // complaint on top of the plain "missing job" one.
  if ('go' in jobs) {
    if (!/^\s*needs:.*rust-wasi/m.test(jobs.go)) {
      problems.push('ci.yml: job "go" downloads the rust-wasi artifact but has no "needs: rust-wasi"');
    }
    // The go job has no testkit binary, so requiring one there fails every run.
    if (jobs.go.includes('DILLA_TESTKIT_REQUIRED')) {
      problems.push('ci.yml: job "go" requires the testkit but does not download it; the harness is go-harness\'s');
    }
    // gap-31 §4 item 2 / NV-14 (resolved): the current majors deliberately pair
    // `actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7` (this file's `rust-wasi` job) with `actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8`.
    // Nobody else owns this half of the hand-off either — a `go` job written from facts/plan-B.md's
    // now-stale deviation B15 text (`download-artifact@v4`) would pair v7 with v4, the untested
    // combination B15 was created to avoid, and neither this checker nor Plan B's own Go test would
    // catch it unless this rule lives here.
    if (!jobs.go.includes('actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8')) {
      problems.push(
        'ci.yml: job "go" must use actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8 to pair with rust-wasi\'s actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7 (gap-31 §4 item 2)',
      );
    }
    assertRaceGatedTestsRunWithoutRace(root, jobs.go, problems);
  }

  if ('go-postgres' in jobs) assertPostgresAPITestsMatchRun(root, jobs['go-postgres'], problems);

  if ('go-ds' in jobs && !/^\s*needs:.*rust-wasi/m.test(jobs['go-ds'])) {
    problems.push('ci.yml: job "go-ds" downloads the rust-wasi artifact but has no "needs: rust-wasi"');
  }

  if ('go-api' in jobs && !/^\s*needs:.*rust-wasi/m.test(jobs['go-api'])) {
    problems.push('ci.yml: job "go-api" downloads the rust-wasi artifact but has no "needs: rust-wasi"');
  }

  if ('go-harness' in jobs) {
    const harness = jobs['go-harness'];
    if (!/^\s*needs:.*rust-wasi/m.test(harness)) {
      problems.push('ci.yml: job "go-harness" downloads the rust-wasi artifact but has no "needs: rust-wasi"');
    }
    // The testkit binary is built by `rust-native` and downloaded by `go-harness`; without the
    // edge the download finds nothing in the run and the job is red on every push.
    if (!/^\s*needs:.*rust-native/m.test(harness)) {
      problems.push('ci.yml: job "go-harness" downloads the rust-native testkit artifact but has no "needs: rust-native"');
    }
  }

  if ('browser-media' in jobs) {
    const extractAt = jobs['browser-media'].indexOf('node packages/media/scripts/extract-rnnoise-wasm.mjs');
    const suiteAt = jobs['browser-media'].indexOf('npm run test:e2e:media -w @dilla/e2e');
    if (extractAt !== -1 && suiteAt !== -1 && extractAt > suiteAt) {
      problems.push('ci.yml: browser-media RNNoise extraction must run before the media suite');
    }
    // Firefox's AudioContext stays suspended on a runner with no audio server (first run of PR #7).
    const audioAt = jobs['browser-media'].indexOf('sh scripts/ci-audio-server.sh');
    if (audioAt !== -1 && suiteAt !== -1 && audioAt > suiteAt) {
      problems.push('ci.yml: browser-media audio server must start before the media suite');
    }
    for (const need of ['rust-wasi', 'rust-native']) {
      if (!new RegExp(`^\\s*needs:.*(?<![\\w-])${need}(?![\\w-])`, 'm').test(jobs['browser-media'])) {
        problems.push(`ci.yml: job "browser-media" downloads the ${need} artifact but has no "needs: ${need}"`);
      }
    }
  }

  checkSuiteSteps(jobs, problems);

  // Task 26: every artifact consumer waits for the producer in the same workflow run.
  for (const [job, needs] of [
    ...Object.keys(BROWSER_WEB_JOBS).map((job) => [job, ['rust-native', 'rust-wasi', 'web-build']]),
    ['go-release', ['web-build']],
    ['desktop', ['web-build']],
  ]) {
    if (!(job in jobs)) continue;
    for (const need of needs) {
      if (!new RegExp(`^\\s*needs:.*(?<![\\w-])${need}(?![\\w-])`, 'm').test(jobs[job])) {
        problems.push(`ci.yml: job "${job}" downloads the ${need} artifact but has no "needs: ${need}"`);
      }
    }
  }

  // Task 26: downloading and refusing the placeholder must precede each build.
  for (const [job, build] of [['go-release', 'go build']]) {
    if (!(job in jobs)) continue;
    const body = jobs[job];
    const fetchAt = body.indexOf('name: dilla-web-dist');
    const guardAt = body.indexOf('content="placeholder"');
    const buildAt = body.indexOf(build);
    if (fetchAt !== -1 && guardAt !== -1 && buildAt !== -1 && !(fetchAt < guardAt && guardAt < buildAt)) {
      problems.push(`ci.yml: job "${job}" must fetch dilla-web-dist into internal/web/dist and refuse the placeholder before it builds`);
    }
  }

  return problems;
}

/**
 * The release workflow (install-release, 2026-10-14): a v* tag builds the assets deploy/install.sh downloads and
 * checks. It builds what it ships, so it is held to ci.yml's rules for the same jobs (fail-closed lines, the clang
 * 23 pin, the size profile, the placeholder guard), and it uses only actions ci.yml uses at the same versions. It
 * builds and pushes no container image: the release is the native install's (Jonas, 2026-10-14).
 */
export const RELEASE_JOBS = ['tag', 'web-build', 'rust-wasi', 'bot-release', 'go-release', 'release'];
export const RELEASE_ASSETS = ['dillad-linux-amd64', 'dillad-linux-arm64', 'dillabot-linux-amd64', 'dillabot-linux-arm64', 'dilla-bot-core-linux-amd64', 'dilla-bot-core-linux-arm64', 'dilla_core_wasi.wasm', 'install.sh', 'install-bot.sh'];
const RELEASE_STEPS = {
  tag: [
    'test "$GITHUB_REF" = "refs/tags/$tag"',
    'INPUT_TAG: ${{ inputs.tag }}',
    '*-*) prerelease=true ;;',
    // Only a commit on main, which ci.yml has gated, is released (review M6).
    'git merge-base --is-ancestor "$GITHUB_SHA" origin/main',
    // v0.1.0-web3.1: the version go-release stamps into dillad is the validated tag without its leading v.
    'echo "version=${tag#v}" >> "$GITHUB_OUTPUT"',
  ],
  'web-build': [
    'bash scripts/install-ci-tool.sh wasm-bindgen',
    'bash scripts/install-ci-tool.sh wasm-pack',
    'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg',
    'node scripts/check-wasm-size.mjs packages/core-wasm/pkg/dilla_core_wasm_bg.wasm',
    'npm run build -w @dilla/web',
    'name: dilla-web-dist',
  ],
  'rust-wasi': REQUIRED_STEPS['rust-wasi'],
  'bot-release': [
    'name: dilla-core-wasi',
    'path: internal/mlswasi/testdata',
    'cargo test -p dilla-bot-core --locked',
    'cargo build -p dilla-bot-core --release --locked',
    'go test ./internal/bot/... ./cmd/dillabot',
    'GOOS=linux GOARCH=${{ matrix.arch }} go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o dist/dillabot-linux-${{ matrix.arch }} ./cmd/dillabot',
    'test "$(dist/dillabot-linux-${{ matrix.arch }} version)" = "dillabot $VERSION"',
    'install -m 0755 target/release/dilla-bot-core dist/dilla-bot-core-linux-${{ matrix.arch }}',
    'name: dilla-bot-${{ matrix.arch }}',
    'if-no-files-found: error',
  ],
  'go-release': [
    // v0.1.0-web3.1: the tag job's version is stamped into main.Version, and the stamp is checked on both binaries.
    'CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o dist/dillad-linux-amd64 ./cmd/dillad',
    'CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" -o dist/dillad-linux-arm64 ./cmd/dillad',
    `test "$(dist/dillad-linux-amd64 version | cut -d' ' -f1-2)" = "dillad $VERSION"`,
    'grep -qaF -- "$VERSION" dist/dillad-linux-arm64',
    'DILLA_WEB_DIST: ${{ github.workspace }}/internal/web/dist',
    'name: dillad-binaries',
  ],
  release: [
    'cp deploy/install.sh release/install.sh',
    'cp deploy/install-bot.sh release/install-bot.sh',
    'pattern: dilla-bot-*',
    'merge-multiple: true',
    // install.sh downloads from the tag stamped into it (review I1): no "latest", which a prerelease never is.
    `sed -i "s/^DILLA_RELEASE_TAG=\\"\\"$/DILLA_RELEASE_TAG=\\"$TAG\\"/" release/install.sh`,
    'test "$(grep -c "^DILLA_RELEASE_TAG=\\"$TAG\\"$" release/install.sh)" -eq 1',
    `sed -i "s/^DILLA_BOT_RELEASE_TAG=\\"\\"$/DILLA_BOT_RELEASE_TAG=\\"$TAG\\"/" release/install-bot.sh`,
    'test "$(grep -c "^DILLA_BOT_RELEASE_TAG=\\"$TAG\\"$" release/install-bot.sh)" -eq 1',
    'gh release view "$TAG"',
    `sha256sum ${RELEASE_ASSETS.join(' ')} > SHA256SUMS`,
    'sha256sum -c --strict SHA256SUMS',
    'gh release create "$TAG" --verify-tag',
    'flags="--prerelease"',
    `${RELEASE_ASSETS.map((a) => `release/${a}`).join(' ')} release/SHA256SUMS`,
  ],
};
const RELEASE_VERSION_ENV = 'VERSION: ${{ needs.tag.outputs.version }}';
const RELEASE_NEEDS = { 'web-build': ['tag'], 'rust-wasi': ['tag'], 'bot-release': ['tag', 'rust-wasi'], 'go-release': ['tag', 'web-build'], release: ['tag', 'go-release', 'rust-wasi', 'bot-release'] };

export function checkReleaseWorkflow(root) {
  const file = 'release.yml';
  const text = readFileSync(join(root, '.github', 'workflows', file), 'utf8');
  const ci = readFileSync(join(root, '.github', 'workflows', 'ci.yml'), 'utf8');
  const problems = [];
  const lines = text.split('\n');

  lines.forEach((line, i) => {
    checkActionReference(line, file, i + 1, problems);
    for (const { re, what } of FORBIDDEN) {
      if (re.test(line)) problems.push(`${file}:${i + 1}: forbidden "${what}" — a release is fail-closed (R22)`);
    }
    // Workflow-injection: a dispatch input reaches a script only through an env: key.
    if (/\$\{\{\s*(?:github\.event\.)?inputs\./.test(line) && !/^\s+[A-Z_]+: \$\{\{\s*(?:github\.event\.)?inputs\.[a-z_]+\s*\}\}\s*$/.test(line)) {
      problems.push(`${file}:${i + 1}: a workflow_dispatch input is expanded outside an env: key; pass it through env to the script`);
    }
    const uses = /^\s*(?:- )?uses:\s*(\S+)/.exec(line);
    if (uses && !new RegExp(`uses:\\s*${uses[1].replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}(?:\\s|$)`, 'm').test(ci)) {
      problems.push(`${file}:${i + 1}: ${uses[1]} is not an action ci.yml uses at that version; pin actions as ci.yml does`);
    }
  });
  if (!/^ {2}push:\n {4}tags: \['v\*'\]$/m.test(text)) problems.push(`${file}: must run on a push of tags 'v*'`);
  if (!/^ {2}workflow_dispatch:\n {4}inputs:\n {6}tag:$/m.test(text)) problems.push(`${file}: must offer workflow_dispatch with a tag input`);
  if (!/^permissions:\n {2}contents: read$/m.test(text)) problems.push(`${file}: the workflow-level token must be read-only (permissions: contents: read)`);
  if (/docker\/|ghcr\.io|packages: write/.test(text)) problems.push(`${file}: builds or pushes a container image; the release carries only the native assets`);
  // A tag run can restore a cache a branch run wrote, and the release embeds what it builds (review M7).
  lines.forEach((line, i) => {
    if (/^\s*cache:\s*(?!false\s*$)\S/.test(line)
      || /uses:\s*Swatinem\/rust-cache@/.test(line)
      || /uses:\s*actions\/cache(?:\/[^@\s]+)?@/.test(line)) {
      problems.push(`${file}:${i + 1}: a build cache in the release workflow; release builds start clean`);
    }
  });
  for (const m of text.matchAll(/uses: actions\/setup-go@[^\n]+\n((?: {8,}\S.*\n)*)/g)) {
    if (!/^ {10}cache: false$/m.test(m[1])) problems.push(`${file}: actions/setup-go caches by default; set cache: false in the release workflow`);
  }

  const jobs = splitJobs(text);
  for (const name of RELEASE_JOBS) {
    if (!(name in jobs)) {
      problems.push(`${file}: missing job "${name}"`);
      continue;
    }
    for (const step of RELEASE_STEPS[name] ?? []) {
      if (!jobSteps(jobs[name]).some((s) => s.some((l) => l.includes(step)))) problems.push(`${file}: job "${name}" is missing: ${step}`);
    }
    for (const need of RELEASE_NEEDS[name] ?? []) {
      if (!new RegExp(`^\\s*needs:.*(?<![\\w-])${need}(?![\\w-])`, 'm').test(jobs[name])) {
        problems.push(`${file}: job "${name}" must list "${need}" in its needs:`);
      }
    }
  }
  if ('release' in jobs && !/^ {4}permissions:\n {6}contents: write$/m.test(jobs.release)) {
    problems.push(`${file}: job "release" needs permissions: contents: write for gh release create`);
  }
  for (const [name, body] of Object.entries(jobs)) {
    if (name !== 'release' && /contents: write/.test(body)) problems.push(`${file}: job "${name}" asks for contents: write; only the release job writes`);
  }
  if ('tag' in jobs && !/^ {6}version: \$\{\{ steps\.tag\.outputs\.version \}\}$/m.test(jobs.tag)) {
    problems.push(`${file}: job "tag" must output version: \${{ steps.tag.outputs.version }}`);
  }
  if ('go-release' in jobs) {
    const body = jobs['go-release'];
    // v0.1.0-web3.1: every release go build stamps main.Version, and every step that uses the version takes it from
    // the tag job through its own env (never a ref name of its own, never an expression inside the script).
    for (const step of jobSteps(body)) {
      if (step.some((l) => /\bgo build\b/.test(l) && !l.includes('-X main.Version=${VERSION}'))) {
        problems.push(`${file}: job "go-release" has a go build without -X main.Version=\${VERSION}; a release binary says its version`);
      }
      if (step.some((l) => /main\.Version|\$\{?VERSION\b/.test(l)) && !step.includes(RELEASE_VERSION_ENV)) {
        problems.push(`${file}: job "go-release" uses the version without its own env ${RELEASE_VERSION_ENV}`);
      }
    }
    const fetchAt = body.indexOf('name: dilla-web-dist');
    const guardAt = body.indexOf('content="placeholder"');
    const buildAt = body.indexOf('go build');
    if (!(fetchAt !== -1 && guardAt !== -1 && buildAt !== -1 && fetchAt < guardAt && guardAt < buildAt)) {
      problems.push(`${file}: job "go-release" must fetch dilla-web-dist into internal/web/dist and refuse the placeholder before it builds`);
    }
  }
  if ('bot-release' in jobs) {
    for (const step of jobSteps(jobs['bot-release'])) {
      if (step.some((l) => /\bgo build\b/.test(l) && l.includes('dillabot-linux-') && !l.includes('-X main.Version=${VERSION}'))) {
        problems.push(`${file}: job "bot-release" has a dillabot build without -X main.Version=\${VERSION}; a release Bot binary says its version`);
      }
      if (step.some((l) => /main\.Version|\$\{?VERSION\b/.test(l)) && !step.includes(RELEASE_VERSION_ENV)) {
        problems.push(`${file}: job "bot-release" uses the version without its own env ${RELEASE_VERSION_ENV}`);
      }
    }
  }
  assertEveryUploadFailsOnNoFiles(text, problems, file);
  assertWasmPackUsesTheSizeProfile(text, problems, file);
  assertWasmJobsUseClang23(text, jobs, problems, file);
  return problems;
}

export function checkDesktopWorkflow(root) {
  const file = 'desktop.yml';
  let text;
  try { text = readFileSync(join(root, '.github', 'workflows', file), 'utf8'); }
  catch (error) { if (error.code === 'ENOENT') return ['desktop.yml: missing desktop acceptance workflow']; throw error; }
  const problems = [];
  problems.push(...checkNativeStockFeasibility(text, { workflow: true }).map(problem => `${file}: native stock feasibility: ${problem}`));
  if (/fail-fast:\s*false|--max-failures=(?!1\b)\d+/.test(text)) {
    problems.push(`${file}: desktop validation must stop on the first failure`);
  }
  text.split('\n').forEach((line, i) => {
    checkActionReference(line, file, i + 1, problems);
    for (const { re, what } of FORBIDDEN) if (re.test(line)) problems.push(`${file}:${i + 1}: forbidden ${what}`);
  });
  for (const required of [
    'workflow_call:', 'contents: read', 'fail-fast: true',
    'os: ubuntu-24.04, platform: linux, arch: x64', 'os: windows-latest, platform: win, arch: x64',
    'os: macos-15-intel, platform: mac, arch: x64', 'os: macos-15, platform: mac, arch: arm64',
    'name: dilla-web-dist', 'name: dilla-desktop-core', 'node node_modules/electron/install.js',
    'npm test -w @dilla/desktop', 'npm run typecheck -w @dilla/desktop',
    'npm run build -w @dilla/desktop -- --existing-client', 'npm run build:validation -w @dilla/desktop',
    'xvfb-run -a dbus-run-session -- bash packages/desktop/scripts/with-window-manager.sh npm run test:electron -w @dilla/desktop',
    'run: npm run test:electron -w @dilla/desktop', 'npm run pack -w @dilla/desktop',
    'node packages/desktop/scripts/check-fuses.mjs', 'node --test packages/desktop/test/native/mac-upgrade.test.mjs',
    'node --test packages/desktop/test/native/installed-upgrade.test.mjs', 'libfuse2t64', 'name: desktop-installed-upgrade-',
    'prepare-windows-audio.ps1', '--grep-invert "application-audio.spec.ts"', 'name: Application audio encryption and exclusion', 'playwright test --config playwright.config.ts application-audio.spec.ts',
    'npm run dist -w @dilla/desktop', 'name: desktop-installers-${{ matrix.platform }}-${{ matrix.arch }}',
  ]) if (!text.includes(required)) problems.push(`${file}: missing ${required}`);
  if (/--no-sandbox|contents: write|--publish always/.test(text)) problems.push(`${file}: desktop validation must keep sandboxing and cannot publish`);
  const body = job => text.split(`\n  ${job}:\n`)[1]?.split(/\n  [a-z][a-z0-9-]*:\n/)[0] ?? '';
  const nativeSteps = jobSteps(body('native'));
  // Installed AppImages compile the GTK launcher and provision their exact
  // compiled initial capsule inside the isolated test, not a copied bare helper.
  for (const job of ['native','voice','client']) {
    const steps=jobSteps(body(job));
    const headers=steps.findIndex(step=>step.length===2 && step[0]==="if: runner.os == 'Linux'"
      && /^run: sudo apt-get update && sudo apt-get install -y [a-z0-9.+ -]+$/.test(step[1])
      && step[1].split(' ').includes('libgtk-3-dev') && step[1].split(' ').includes('pkg-config'));
    const build=steps.findIndex(step=>step.includes('run: npm run build -w @dilla/desktop -- --existing-client'));
    if(headers<0 || build<0 || headers>=build)problems.push(`${file}: installed sandbox GTK compiler prerequisites must precede ${job} builds`);
  }
  const nativeGo=nativeSteps.findIndex(step=>step.join('\n')===`uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7
with:
go-version-file: go.mod
cache-dependency-path: go.sum`);
  const controls=nativeSteps.findIndex(step=>step.join('\n')===`name: Linux installed sandbox prerequisite controls
if: runner.os == 'Linux'
run: |
CGO_ENABLED=0 go test ./packages/desktop/test/fixtures/linux-installed-sandbox-bootstrap
node --test packages/desktop/test/linux-installed-sandbox-bootstrap.test.mjs packages/desktop/test/linux-sandbox-package-runner.test.mjs`);
  const installedLinux=nativeSteps.findIndex(step=>step.includes('name: Installed upgrade acceptance') && step.includes("if: runner.os == 'Linux'"));
  const nativeUnit=nativeSteps.findIndex(step=>step.some(line=>line.includes('run: npm test -w @dilla/desktop')));
  if(nativeGo<0 || nativeUnit<=nativeGo || controls<=nativeGo || controls>=installedLinux)problems.push(`${file}: installed sandbox controls require pinned native Go and unfiltered controls before installed acceptance`);
  for(const [job,name,guard,artifact]of [
    ['native','Initial installed sandbox prerequisite proof',"runner.os == 'Linux'",'desktop-initial-sandbox-native-linux-x64'],
    ['client','Initial installed account sandbox prerequisite proof',"runner.os == 'Linux' && steps.installed-account.outputs.present == 'true'",'desktop-initial-sandbox-account-linux-x64'],
  ]){
    const expected=`name: ${name}\nif: ${guard}\nuses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7\nwith:\nname: ${artifact}\npath: \${{ runner.temp }}/dilla-installed-sandbox-initial.json\nif-no-files-found: error`;
    if(jobSteps(body(job)).filter(step=>step.join('\n')===expected).length!==1)problems.push(`${file}: installed sandbox requires exact job-selected initial prerequisite proof upload for ${job}`);
  }
  const desktopBuild = nativeSteps.findIndex(step=>step.includes('id: desktop-build') && step.includes('run: npm run build -w @dilla/desktop -- --existing-client'));
  const failureUpload = `if: failure() && steps.desktop-build.outcome == 'success'
uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
with:
name: desktop-failure-\${{ matrix.platform }}-\${{ matrix.arch }}
path: |
packages/desktop/test-results
\${{ runner.temp }}/dilla-windows-audio/*.log
\${{ runner.temp }}/dilla-windows-audio/*.out
\${{ runner.temp }}/dilla-windows-audio/*.err
if-no-files-found: error`;
  const failureUploads = nativeSteps.map((step,index)=>step.join('\n')===failureUpload ? index : -1).filter(index=>index>=0);
  if(desktopBuild<0 || failureUploads.length!==1 || failureUploads[0]<=desktopBuild)problems.push(`${file}: native failure diagnostics require the exact post-build upload`);
  const unitSteps = nativeSteps.filter(step => step.some(line => line.includes('run: npm test -w @dilla/desktop')));
  if (unitSteps.length !== 1 || unitSteps[0].join('\n') !== "if: runner.os != 'Windows'\nrun: npm test -w @dilla/desktop") {
    problems.push(`${file}: non-Windows desktop unit tests must run on every other native platform`);
  }
  const proof = nativeSteps.findIndex(step => step.some(line => line.includes('name: desktop-installed-upgrade-')));
  const buildProvenance = nativeSteps.findIndex(step => step.some(line => line.includes('name: Record native desktop build provenance')));
  const finalDistribution = nativeSteps.findLastIndex(step => step.some(line => line.includes('run: npm run dist -w @dilla/desktop')));
  const finalFuses = nativeSteps.findLastIndex(step => step.some(line => line.includes('run: node packages/desktop/scripts/check-fuses.mjs')));
  const installerUpload = nativeSteps.findIndex(step => step.some(line => line.includes('name: desktop-installers-')));
  if (buildProvenance < 0 || finalDistribution < 0 || finalFuses <= finalDistribution || buildProvenance <= finalFuses || installerUpload <= buildProvenance
    || nativeSteps[buildProvenance]?.some(line => /\bif:/.test(line))
    || !nativeSteps[buildProvenance]?.some(line => line.includes('run: node packages/desktop/scripts/build-provenance.mjs packages/desktop/out ${{ matrix.platform }}-${{ matrix.arch }}'))
    || !nativeSteps[installerUpload]?.some(line => line.trim() === 'packages/desktop/out/build-provenance-*.json')) {
    problems.push(`${file}: build provenance must authenticate final fused installers and accompany their accepted bytes`);
  }
  const mutation=nativeSteps.findIndex(step=>step.join('\n')===`name: Linux AppImage final package inspection mutations
if: runner.os == 'Linux'
run: node packages/desktop/scripts/test-linux-sandbox-package.mjs packages/desktop/out`);
  if(mutation<=finalDistribution || mutation>=buildProvenance)problems.push(`${file}: installed sandbox final-image mutation command must run unfiltered after distribution and before provenance`);
  for(const glob of ['packages/desktop/out/*.sandbox','packages/desktop/out/*.sandbox.json'])if(!nativeSteps[installerUpload]?.some(line=>line.trim()===glob))problems.push(`${file}: installed sandbox accepted component sidecars must accompany final AppImage`);
  const lastPlaywright = nativeSteps.findLastIndex(step => step.some(line => /npm run test:electron|playwright test/.test(line)));
  const installed = nativeSteps.map((step, index) => step.some(line => /node --test packages\/desktop\/test\/native\/(?:mac-upgrade|installed-upgrade)\.test\.mjs/.test(line)) ? index : -1).filter(index => index >= 0);
  if (proof < 0 || installed.some(index => index <= lastPlaywright || index >= proof)) problems.push(`${file}: installed acceptance proof must follow Playwright cleanup and precede upload`);
  for (const os of ['Linux', 'Windows']) {
    if (!nativeSteps.some(step => step.some(line => line.includes('name: Installed upgrade acceptance'))
      && step.some(line => line.includes(`if: runner.os == '${os}'`))
      && step.some(line => line.includes('node --test packages/desktop/test/native/installed-upgrade.test.mjs')))) {
      problems.push(`${file}: ${os} requires actual installed upgrade acceptance`);
    }
  }
  // These full Windows gates run once in ci.yml before this reusable workflow.
  if (/windows-installer-maintenance\.test\.mjs/.test(text)) problems.push(`${file}: duplicate Windows maintenance acceptance belongs only in the independent CI gate`);
  if (/go test[^\n]*packages\/desktop\/native\/windows-launcher/.test(text)) problems.push(`${file}: duplicate Windows kernel acceptance belongs only in the independent CI gate`);
  if (!nativeSteps.some(step => step.some(line => line.includes('name: desktop-installers-'))
    && step.some(line => line.trim() === 'packages/desktop/out/*.manifest.json'))) problems.push(`${file}: Windows signed program manifest must accompany its accepted installer`);
  if (/APPIMAGE_EXTRACT_AND_RUN|--appimage-extract/.test(body('native'))) problems.push(`${file}: Linux installed acceptance must exercise normal FUSE launch`);
  const tooling = body('test-tools');
  for (const required of [
    'os: ubuntu-24.04, platform: linux, arch: x64', 'os: windows-latest, platform: win, arch: x64',
    'os: macos-15-intel, platform: mac, arch: x64', 'os: macos-15, platform: mac, arch: arm64',
    'node scripts/e2e-test-tools.mjs build artifacts/e2e-test-tools',
    'name: dilla-e2e-tools-${{ matrix.platform }}-${{ matrix.arch }}-${{ github.sha }}', 'path: artifacts/e2e-test-tools', 'overwrite: true',
  ]) if (!tooling.includes(required)) problems.push(`${file}: native tooling producer missing ${required}`);
  if (/^    needs:|shard:/m.test(tooling)) problems.push(`${file}: tooling must build once per native target without waiting for test jobs`);
  const buildStep = jobSteps(tooling).find(step => step.some(line => line.includes('node scripts/e2e-test-tools.mjs build ')));
  if (buildStep?.some(line => /\bif:/.test(line))) problems.push(`${file}: tooling builds cannot be bypassed by a cache hit`);
  if (/^    needs:/m.test(body('native'))) problems.push(`${file}: native acceptance must stay independent of tooling builds`);
  for (const job of ['client', 'voice']) {
    const consumer = body(job);
    for (const required of [
      'needs: test-tools', 'DILLA_E2E_TOOLING_DIR: ${{ github.workspace }}/artifacts/e2e-test-tools',
      'name: dilla-e2e-tools-${{ matrix.platform }}-${{ matrix.arch }}-${{ github.sha }}',
      'path: artifacts/e2e-test-tools', 'node scripts/e2e-test-tools.mjs verify artifacts/e2e-test-tools',
      '--max-failures=1',
    ]) if (!consumer.includes(required)) problems.push(`${file}: ${job} tooling consumer missing ${required}`);
    if (!consumer.includes("go test ./internal/ops -run 'Lock|BackupAndServe|ServeAdmits'")) {
      problems.push(`${file}: ${job} must retain native test-host directory locks coverage`);
    }
    if (/cargo build|^\s+(?:run-id|repository|github-token):/m.test(consumer)) {
      problems.push(`${file}: ${job} tooling must reuse the verified artifact from this run without rebuilding`);
    }
    const steps = jobSteps(consumer);
    const verify = steps.findIndex(step => step.some(line => line.includes('node scripts/e2e-test-tools.mjs verify ')));
    const acceptance = steps.findIndex(step => step.some(line => /playwright test/.test(line)));
    if (verify < 0 || acceptance < 0 || verify >= acceptance || steps[verify]?.some(line => /\bif:/.test(line))) {
      problems.push(`${file}: ${job} tooling verification must run unconditionally before acceptance`);
    }
  }
  const client = body('client');
  for (const required of ['e2e/web/support/installed-keyring.sh', 'libasound2t64 gnome-keyring libfuse2t64',
    'CHROME_DEVEL_SANDBOX: ${{ github.workspace }}/node_modules/electron/dist/chrome-sandbox',
    'node packages/desktop/scripts/installed-account-proof.mjs e2e/desktop-client-results']) {
    if (!client.includes(required)) problems.push(`${file}: installed account acceptance missing ${required}`);
  }
  if (/APPIMAGE_EXTRACT_AND_RUN|--appimage-extract|--password-store=basic/.test(client)) problems.push(`${file}: installed account acceptance must keep normal FUSE and OS credential protection`);
  const accountUpload = jobSteps(client).find(step => step.some(line => line.includes('name: Installed account upgrade proof')));
  if (!accountUpload || !accountUpload.some(line => line.includes("if: steps.installed-account.outputs.present == 'true'"))
    || !accountUpload.some(line => line.includes('name: desktop-installed-account-${{ matrix.platform }}-${{ matrix.arch }}'))
    || !accountUpload.some(line => line.includes('path: packages/desktop/test-report/installed-account-${{ matrix.platform }}-${{ matrix.arch }}.json'))) {
    problems.push(`${file}: installed account proof must upload only from its owning shard`);
  }
  for (const required of [
    'shard: [1, 2, 3]', 'os: [ubuntu-24.04, windows-latest, macos-15-intel, macos-15]',
    'os: ubuntu-24.04, platform: linux, arch: x64', 'os: windows-latest, platform: win, arch: x64',
    'os: macos-15-intel, platform: mac, arch: x64', 'os: macos-15, platform: mac, arch: arm64',
    'name: dilla-core-wasi', 'node packages/desktop/scripts/prepare-browser-sandbox.mjs',
    'playwright test --config playwright.desktop.config.ts --project=electron-client --shard=${{ matrix.shard }}/3',
  ]) if (!client.includes(required)) problems.push(`${file}: shared Electron client coverage missing ${required}`);
  const voice = body('voice');
  for (const required of ['desktop-voice-${{ matrix.platform }}-${{ matrix.arch }}',
    'os: [ubuntu-24.04, windows-latest, macos-15-intel, macos-15]',
    'playwright.desktop-voice.config.ts --project=electron-voice --workers=1', 'DILLA_E2E_NOTIFICATIONS', 'dunst',
    'name: dilla-core-wasi', 'node packages/desktop/scripts/prepare-browser-sandbox.mjs',
    'name: Desktop performance comparison', 'node packages/desktop/scripts/run-performance.mjs', 'name: desktop-performance-']) {
    if (!voice.includes(required)) problems.push(`${file}: Electron voice coverage missing ${required}`);
  }
  if (/--shard|shard:/.test(voice)) problems.push(`${file}: the desktop voice suite must preserve its single frozen-clock worker`);
  assertEveryUploadFailsOnNoFiles(text, problems, file);
  return problems;
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const root = process.argv[2] ?? process.cwd();
  const problems = [...checkWorkflow(root), ...checkReleaseWorkflow(root), ...checkDesktopWorkflow(root)];
  for (const p of problems) console.error(p);
  process.exit(problems.length === 0 ? 0 : 1);
}
