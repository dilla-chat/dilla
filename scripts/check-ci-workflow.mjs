import { readdirSync, readFileSync } from 'node:fs';
import { join, posix } from 'node:path';
import { AREAS, gateExpression, JOB_GATES, jobRuns, reachableOutputs, WORKFLOWS } from './ci-areas.mjs';
import { localMirrorProblems } from './ci-local-plan.mjs';
import { checkNativeStockFeasibility } from './check-native-stock-feasibility.mjs';
import { POSTGRES_API_RUN, SETS as GO_SHARD_SETS } from './go-shard.mjs';
import { createRequire } from 'node:module';
const yaml = createRequire(import.meta.url)('js-yaml');

/**
 * Every job each CI workflow must carry, and no other (interfaces §5; plan 2026-11-05 Task 6 split ci.yml into these
 * two). Ruling L added the two Go jobs: they landed with Plan B task 8, and leaving them merely "checked if present"
 * meant deleting them was the one way to make the Go half of the tree ungated without this gate saying a word. The
 * `go` job additionally has to declare its `needs: rust-wasi` edge (see below). Artifacts never cross workflows, so
 * a job whose output several need (rust-native, rust-wasi, web-build) is in each of them, unchanged. mobile.yml
 * (plan 2026-11-05 mobile M1, Task 7) is the third: the iOS and Android apps.
 */
export const REQUIRED_JOBS = {
  server: [
    // The change filter and always-on docs gates; infrastructure remains active for docs-only changes too.
    'changes',
    'docs',
    'ci-watchdog',
    'server-work-complete',
    // The aggregator: one check that is red when any job failed or was cancelled and green when the rest passed or were skipped by design.
    'server-ok',
    // The installers release.yml publishes and the Bot operation drift guard, with their tests.
    'server-scripts',
    'rust-native',
    'rust-wasi',
    'vectors',
    'deny',
    // The race-detector unit tests, in GO_SHARD_SETS.go.shards shards (scripts/go-shard.mjs), and beside them the
    // once-only steps (vet, the pinned graph, the non-race and TURN reruns, the static build) in go-checks.
    'go',
    'go-checks',
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
    // go-release embeds the web client, so the server workflow builds it too.
    'web-build',
  ],
  desktop: [
    'changes',
    'ci-watchdog',
    'desktop-work-complete',
    'desktop-ok',
    'node',
    'ui',
    'rust-native',
    'rust-wasm-node',
    'rust-wasi',
    'browser-spike',
    'browser-media',
    // Build the bundled client once for Electron and retain shared encrypted-core
    // integration (BROWSER_WEB_JOBS below) without browser compatibility lanes.
    'web-build',
    'linux-sandbox-foundation',
    'desktop',
    // core-integration: BROWSER_WEB_JOBS.
  ],
  mobile: [
    'changes',
    'ci-watchdog',
    'mobile-work-complete',
    'mobile-ok',
    // npm test and typecheck of @dilla/mobile.
    'mobile-unit',
    // The wasm core only (desktop's web-build runs the typechecks and dist guards): mobile-bundle's input.
    'mobile-core-wasm',
    // The mobile client (packages/mobile/dist), once for both apps.
    'mobile-bundle',
    // The smoke test's test host, WASI core and native peer.
    'mobile-e2e-tools',
    // The APKs, the release network-config check and the emulator smoke test.
    'mobile-android',
    // The iOS simulator build.
    'mobile-ios',
  ],
};

/** The nightly run (02:17 UTC) on main of the workflows WORKFLOWS marks `nightly` (desktop, mobile). */
export const NIGHTLY_CRON = '17 2 * * *';

// The watcher runs for every trusted event; a scheduled run is one (GitHub starts it on main), so desktop-ok and
// mobile-ok keep failing a skipped watcher there.
const watchdogIf = (workflow) => `    if: (github.event_name == 'push' && github.ref == 'refs/heads/main') || ${WORKFLOWS[workflow].nightly ? "github.event_name == 'schedule' || " : ''}(github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name == github.repository)`;

/** A workflow's file name, for messages. */

const fileOf = (workflow) => WORKFLOWS[workflow].file;

/**
 * The watchdog's completion barrier (CI_COMPLETION_JOBS): the workflow's always-on roots and its DAG barrier.
 */
export const completionJobs = (workflow) => WORKFLOWS[workflow].always.filter((job) => job !== 'ci-watchdog' && job !== WORKFLOWS[workflow].ok).join(' ');

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
export const WEB_SHARDS = 6;
export const BROWSER_WEB_JOBS = {
  'core-integration': { engine: 'chromium', suite: 'core', shard: null, timeout: 20 },
};
export const MIN_BROWSER_WEB_TIMEOUT = 20;

/**
 * The sharded Go jobs (GO_SHARD_SETS in scripts/go-shard.mjs): each is a matrix of its set's shard count whose one test
 * step hands its `go test` flags to the partitioner. The partitioner checks the union of the shards before it runs a
 * test, so these flags and the matrix are all the workflow has to get right.
 */
export const GO_SHARD_FLAGS = {
  go: '-race -shuffle=on -timeout 20m',
  'go-api': '-race -shuffle=on -timeout 40m',
  'go-ds': '-race -shuffle=on -timeout 40m',
  'go-harness': '-race -shuffle=on -timeout 25m',
  'go-postgres': '-race -shuffle=on -timeout 20m',
};
export function goShardCommand(set) {
  return `node scripts/go-shard.mjs run ${set} \${{ matrix.shard }} -- ${GO_SHARD_FLAGS[set]}`;
}
export function goShardMatrix(set) {
  const shards = Array.from({ length: GO_SHARD_SETS[set].shards }, (_, i) => i + 1).join(', ');
  return `    strategy:\n      fail-fast: true\n      matrix:\n        shard: [${shards}]\n`;
}

/** The testkit binary's env line, which every suite step that drives the native peer carries. */
const TESTKIT_ENV = 'DILLA_TESTKIT: ${{ github.workspace }}/artifacts/dilla-testkit';

/** The one suite command a browser job runs, a shard of the web suite carrying its `--shard=i/WEB_SHARDS` flag. */
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
    "if: failure() && hashFiles('e2e/test-results/**') != ''",
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

/** One load-bearing command per job, so a silently gutted job is caught; a job carries them in either workflow. */
const REQUIRED_STEPS = {
  desktop: ['uses: ./.github/workflows/desktop.yml'],
  // The areas are computed by scripts/ci-areas.mjs (tested in ci-areas.test.mjs), which fails safe to everything.
  // fetch-depth: 0 gives it the base commit to diff against.
  changes: [
    'fetch-depth: 0',
    'node scripts/ci-areas.mjs',
    'EVENT_NAME: ${{ github.event_name }}',
    'BASE_SHA: ${{ github.event.pull_request.base.sha }}',
    ...[...AREAS, 'code'].map((area) => `${area}: \${{ steps.areas.outputs.${area} }}`),
  ],
  // The cheap docs gates, for every change.
  // Their unit tests and the real checks against the real files; check:ci reads the Go tree for the race and Postgres rules.
  docs: ['npm run test:docs-check', 'npm run test:brief-check', 'npm run test:copy-check', 'npm run check:docs', 'npm run check:brief', 'npm run check:copy', 'npm run check:ci'],
  // release.yml publishes deploy/install.sh and deploy/install-bot.sh, so the server workflow runs their tests (and
  // shellcheck, which the runner image carries) and the Bot operation drift guard.
  'server-scripts': ['npm run test:bot-ops-check', 'npm run check:bot-ops', 'npm run test:install', 'bash deploy/install_bot_test.sh'],
  // The final gate of each workflow runs regardless of dependency results, alongside its no-op completion barrier.
  'server-ok': ["contains(needs.*.result, 'failure')", "contains(needs.*.result, 'cancelled')"],
  'desktop-ok': ["contains(needs.*.result, 'failure')", "contains(needs.*.result, 'cancelled')"],
  'mobile-ok': ["contains(needs.*.result, 'failure')", "contains(needs.*.result, 'cancelled')"],
  // The mobile lane (plan 2026-11-05 mobile M1, Task 7): each job's load-bearing commands.
  'mobile-unit': ['- run: npm test -w @dilla/mobile', '- run: npm run typecheck -w @dilla/mobile'],
  'mobile-core-wasm': ['- run: wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg', '- run: node scripts/check-wasm-size.mjs packages/core-wasm/pkg/dilla_core_wasm_bg.wasm', 'name: mobile-core-wasm\n          path: packages/core-wasm/pkg/\n          if-no-files-found: error'],
  'mobile-bundle': ['name: mobile-core-wasm\n          path: packages/core-wasm/pkg', '- run: npm run build -w @dilla/mobile', 'name: dilla-mobile-dist\n          path: packages/mobile/dist/\n          if-no-files-found: error'],
  'mobile-e2e-tools': ['- run: go build -o target/dilla-testhost ./cmd/dilla-testhost', '- run: cargo build -p dilla-testkit --release --locked', '- run: cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked', 'name: mobile-e2e-tools'],
  'mobile-android': [
    'name: dilla-mobile-dist\n          path: packages/mobile/dist',
    'name: mobile-e2e-tools',
    'run: echo "JAVA_HOME=$JAVA_HOME_21_X64" >> "$GITHUB_ENV"',
    '- run: cd packages/mobile && npx cap sync android',
    ' -- packages/mobile/android/gradlew -p packages/mobile/android --no-daemon assembleDebug assembleRelease',
    // Review finding M2: the release APK trusts system CAs only and refuses cleartext.
    '- run: npm run android:check-release -w @dilla/mobile',
    'run: bash scripts/ci-android.sh kvm',
    ' -- bash scripts/ci-android.sh sdk',
    '- run: bash scripts/ci-android.sh avd',
    'run: npm run e2e:android -w @dilla/mobile -- --skip-build\n        env:\n          DILLA_ANDROID_E2E_OUT: ${{ github.workspace }}/target/mobile-android-e2e',
    "if: failure() && hashFiles('target/mobile-android-e2e/**') != ''\n        with:\n          name: mobile-android-results\n          path: target/mobile-android-e2e\n          if-no-files-found: error",
  ],
  'mobile-ios': [
    '- run: cd packages/mobile && npx cap sync ios',
    'sudo xcode-select -s /Applications/Xcode_26.6.app',
    ' -- xcodebuild -resolvePackageDependencies -project packages/mobile/ios/App/App.xcodeproj -scheme App -clonedSourcePackagesDirPath "$RUNNER_TEMP/spm"',
    "run: xcodebuild -project packages/mobile/ios/App/App.xcodeproj -scheme App -sdk iphonesimulator -destination 'generic/platform=iOS Simulator' -clonedSourcePackagesDirPath \"$RUNNER_TEMP/spm\" -disableAutomaticPackageResolution -derivedDataPath \"$RUNNER_TEMP/DerivedData\" CODE_SIGNING_ALLOWED=NO build",
  ],
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
  // Desktop merge security review M2: internal/desktopauth/browser.js (go:embed) is generated from the web client's
  // webauthn.ts, and a stale copy would ship passkey code the web client no longer has.
  node: ["- if: needs.changes.outputs.web != 'false'\n        run: npm test\n        env:\n          DILLA_UCD_DIR: ${{ runner.temp }}/ucd", "name: vectors are up to date\n        if: needs.changes.outputs.web != 'false'", 'npm run test:css-check', 'npm run check:css', 'npm run check:desktop-auth', 'npm run test:ci-check', 'npm run test:wasm-size-check', 'npm run test:manifest-writer', "uses: actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0\n        if: needs.changes.outputs.web != 'false' || needs.changes.outputs.text != 'false'", "name: profile-text tables are current\n        if: needs.changes.outputs.text != 'false'\n        run: node scripts/gen-profile-text-tables.mjs --download --check \"$RUNNER_TEMP/ucd\""],
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
    "if: failure() && hashFiles('e2e/test-results/**') != ''",
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
  // Ruling M. The download lines are the hand-off from `rust-wasi`: without the download, or with it landing anywhere
  // but `internal/mlswasi/testdata`, the wazero tests skip themselves and the job is green having proved nothing. The
  // race-detector run needs cgo, so it must never be reduced to a plain `go test`; its shards are pinned by
  // assertGoShards.
  go: [
    'timeout-minutes: 30',
    'actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8',
    'path: internal/mlswasi/testdata',
    'name: dilla-core-wasi',
    goShardCommand('go'),
    'GORACE: halt_on_error=1',
  ],
  // The go job's once-only steps. `internal/deps` sits behind the `dillapins` tag precisely so no ordinary build pulls
  // the SFU in, which also means no other step in this workflow would notice the pinned LiveKit/wazero/sqlite graph
  // breaking.
  'go-checks': [
    'timeout-minutes: 20',
    'actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8',
    'path: internal/mlswasi/testdata',
    'name: dilla-core-wasi',
    'go mod verify',
    'go vet ./...',
    'CGO_ENABLED=0 go build -tags dillapins ./internal/deps',
    "go test -race github.com/pion/turn/v5/internal/allocation -run '^TestAddPermissionPublishesInitializedTimer$' -count=20",
    NON_RACE_STEP,
    "CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' ./cmd/dillad",
  ],
  // The one package the go job leaves out, with the wasi core it needs and a budget of its own.
  'go-ds': [
    'timeout-minutes: 50',
    'actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8',
    'name: dilla-core-wasi',
    'path: internal/mlswasi/testdata',
    goShardCommand('go-ds'),
  ],
  // The other package the go job leaves out, with the same wasi hand-off and a budget that leaves margin.
  'go-api': [
    'timeout-minutes: 50',
    'actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8',
    'name: dilla-core-wasi',
    'path: internal/mlswasi/testdata',
    goShardCommand('go-api'),
  ],
  // The harness job: both hand-offs, the binary, and a run that FAILS without it.
  'go-harness': [
    'timeout-minutes: 35',
    'actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8',
    'path: internal/mlswasi/testdata',
    'name: dilla-core-wasi',
    'name: dilla-testkit',
    'artifacts/dilla-testkit',
    goShardCommand('go-harness'),
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
    // I7 (fix wave): the packages with a Postgres leg, internal/ops's backup, restore and heal tests among them, and
    // (web3.9) internal/api's AFK backfill and channel-parent tests: the go-postgres set of scripts/go-shard.mjs.
    goShardCommand('go-postgres'),
  ],
  // Task 26/C9: release verifies the downloaded client before embedding it.
  'go-release': ['CGO_ENABLED=0', 'if-no-files-found: error', 'name: dilla-web-dist', 'path: internal/web/dist', 'content="placeholder"', 'go test -tags webdist ./internal/web/...', 'DILLA_WEB_DIST: ${{ github.workspace }}/internal/web/dist',
    // Bot v2 task 11: no shipped dillad is a dilla_unsafe_test build (the tag survives -trimpath in the build info).
    `test "$(go version -m dist/dillad-linux-amd64 dist/dillad-linux-arm64 | grep -c 'build[[:space:]]CGO_ENABLED=0')" -eq 2`,
    'if go version -m dist/dillad-linux-amd64 dist/dillad-linux-arm64 | grep -q dilla_unsafe_test; then echo "a dillad was built with the dilla_unsafe_test tag"; exit 1; fi',
    'if go version -m dist/dillad-linux-amd64 dist/dillad-linux-arm64 | grep -q dilla_testhost; then echo "a dillad was built with the dilla_testhost tag"; exit 1; fi',
  ],
};

/**
 * The race-detector sets are module-wide on purpose: `go` is `./...` minus exactly internal/ds and internal/api, which
 * go-ds and go-api run, so every new internal/… package is picked up with zero workflow edits and the union of the
 * three is still the whole module. Narrowing a set is how a whole subsystem silently stops being tested, so the sets
 * are pinned here, and each sharded job must be a matrix of its set's shard count that hands the partitioner its set
 * and shard (its job name carries the shard). The partitioner fails a shard whose set is not covered exactly once.
 */
const EXPECTED_GO_SETS = {
  go: [{ pattern: './...', exclude: ['./internal/ds', './internal/api'] }],
  'go-api': [{ pattern: './internal/api/...' }],
  'go-ds': [{ pattern: './internal/ds/...' }],
  'go-harness': [{ pattern: './internal/testkit/...' }],
  'go-postgres': [{ pattern: './internal/store/...' }, { pattern: './internal/ops/...' }, { pattern: './internal/api/...', run: POSTGRES_API_RUN }],
};

export function assertGoShards(jobs, problems, sets = GO_SHARD_SETS, file = WORKFLOWS.server.file) {
  const names = Object.keys(sets).sort();
  if (JSON.stringify(names) !== JSON.stringify(Object.keys(EXPECTED_GO_SETS).sort())) {
    problems.push(`scripts/go-shard.mjs: the sharded sets must be exactly ${Object.keys(EXPECTED_GO_SETS).join(', ')}`);
  }
  for (const [set, packages] of Object.entries(EXPECTED_GO_SETS)) {
    if (JSON.stringify(sets[set]?.packages) !== JSON.stringify(packages)) {
      problems.push(`scripts/go-shard.mjs: the ${set} set must be exactly ${JSON.stringify(packages)}`);
    }
    if (!Number.isInteger(sets[set]?.shards) || sets[set].shards < 1) problems.push(`scripts/go-shard.mjs: the ${set} set needs a positive shard count`);
    const body = jobs[set];
    if (body === undefined || !Number.isInteger(sets[set]?.shards)) continue;
    if (!body.includes(goShardMatrix(set))) {
      problems.push(`${file}: job "${set}" must be a fail-fast matrix of exactly its ${sets[set].shards} shards (scripts/go-shard.mjs SETS): ${goShardMatrix(set).trim().replaceAll('\n', ' ')}`);
    }
    if (!body.split('\n').includes(`    name: ${set}-\${{ matrix.shard }}`)) problems.push(`${file}: job "${set}" must be named ${set}-\${{ matrix.shard }}`);
    const steps = jobSteps(body);
    const shardSteps = steps.filter((step) => step.some((line) => line.includes('scripts/go-shard.mjs')));
    if (shardSteps.length !== 1 || !shardSteps[0].some((line) => line === `run: ${goShardCommand(set)}`)) {
      problems.push(`${file}: job "${set}" must run its shard in exactly one step: ${goShardCommand(set)}`);
    } else if (shardSteps[0].some((line) => /^if:/.test(line))) {
      problems.push(`${file}: job "${set}"'s shard step must not be conditional`);
    }
    if (steps.some((step) => step.some((line) => /\bgo test\b/.test(line)))) {
      problems.push(`${file}: job "${set}" must run its tests through scripts/go-shard.mjs only (another go test step would run them twice or outside the union check)`);
    }
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
 * web3.9: go-postgres runs internal/api through POSTGRES_API_RUN (scripts/go-shard.mjs), and a test it misses is
 * silently not run on Postgres. Every Test* in internal/api's afk*_test.go and channels_parent_test.go must match it.
 */
function assertPostgresAPITestsMatchRun(root, problems, regex = POSTGRES_API_RUN) {
  let re;
  try {
    re = new RegExp(regex);
  } catch {
    problems.push('scripts/go-shard.mjs: POSTGRES_API_RUN is not a valid expression');
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
      if (!re.test(m[1])) problems.push(`scripts/go-shard.mjs: go-postgres's internal/api filter (POSTGRES_API_RUN) does not match ${m[1]} (internal/api/${f})`);
    }
  }
}

/**
 * The `go-checks` job's non-race step (the `go test` without -race that names its tests with -run) runs the
 * package of every race-gated test and names each one exactly, and names no test that is not gated:
 * a renamed or deleted test leaves a -run alternative that matches nothing, which go test passes.
 */
function assertRaceGatedTestsRunWithoutRace(root, goJob, problems, file) {
  const gated = raceGatedTests(root);
  const line = goJob.split('\n').find((l) => /\bgo test\b/.test(l) && !/-race\b/.test(l) && / -run '/.test(l));
  if (line === undefined) {
    if (gated.length > 0) {
      problems.push(`${file}: job "go-checks" has no non-race "go test ... -run '...'" step for the ${gated.length} race-gated test(s)`);
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
    if (!pkgs.has(g.pkg)) problems.push(`${file}: job "go-checks"'s non-race step does not run ${g.pkg}, the package of race-gated ${g.name}`);
    if (!names.has(g.name)) problems.push(`${file}: job "go-checks"'s non-race step does not name race-gated ${g.name} (${g.pkg}) in -run`);
  }
  const known = new Set(gated.map((g) => g.name));
  for (const n of names) {
    if (!known.has(n)) problems.push(`${file}: job "go-checks"'s non-race step names ${n || '(empty)'}, which is no race-gated test`);
  }
}

/**
 * Every `actions/upload-artifact` step sets `if-no-files-found: error` (Global Constraints: CI is
 * fail-closed). The per-job REQUIRED_STEPS pin it for the jobs that exist today; this rule is the
 * one that covers a job added tomorrow. A step is its `- ` list item and every line indented deeper.
 */
function assertEveryUploadFailsOnNoFiles(text, problems, file) {
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
    // Failure evidence is uploaded only when it exists: a step that fails before writing any file would
    // otherwise add a second, misleading "No files were found" error to the real one.
    const condition = /^\s*(?:- )?if:\s*(failure\(\).*)$/m.exec(step)?.[1];
    if (condition !== undefined && !/&&\s*hashFiles\('[^']+'(?:,\s*'[^']+')*\)\s*!=\s*''\s*$/.test(condition)) {
      problems.push(`${file}:${i + 1}: failure-evidence actions/upload-artifact without a hashFiles(...) != '' guard — it would add a misleading "No files were found" error`);
    }
  }
}

/** C16 / L-CI-05: also cover any wasm-pack build line added tomorrow. */
function assertWasmPackUsesTheSizeProfile(text, problems, file) {
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
 * re-pin in scripts/ci-llvm.sh and in the checker's tests, after checking the new build's size.
 *
 * Since run 639 (2026-11-03: apt.llvm.org answered nothing for over twenty minutes and every Linux job failed at
 * this step) the install is scripts/ci-llvm.sh between an actions/cache restore and save of the build's eight
 * .deb files: an archive that cannot be reached at all installs the cached build, which apt verified against the
 * archive's signed index on the run that fetched it. A reachable archive is still pinned or fails closed.
 *
 * The checker holds the canonical text and compares whole blocks, not substrings: a substring rule passes a
 * workflow that names the strings in a comment or another step while the job runs something else (the
 * validator differential of the d3429ce scanner lead). Outside the canonical lines, no workflow line may name
 * a variable cc reads for the compiler, archiver or flags, no wasm job may write GITHUB_ENV or GITHUB_PATH,
 * and the workflow may use no YAML anchor, alias or merge key, whose expansion a line-based checker cannot see.
 */
export const WASM_LLVM_VERSION = '23.1';
export const WASM_LLVM_KEY_FPR = '6084F3CF814B57C1CF12EFD515CF4D18AF4F7421';
export const WASM_LLVM_PACKAGES = [
  'clang-23', 'llvm-23', 'libllvm23', 'libclang-cpp23', 'libclang1-23', 'libclang-common-23-dev',
  'llvm-23-linker-tools', 'llvm-23-runtime',
];
export const WASM_CLANG_ENV_BLOCK = [
  '    env:',
  '      CC_wasm32_unknown_unknown: /usr/bin/clang-23',
  '      AR_wasm32_unknown_unknown: /usr/bin/llvm-ar-23',
].join('\n');
/** The three steps every wasm job runs right after checkout, as one block: the cached build of the pinned
 *  clang 23 line is restored, scripts/ci-llvm.sh installs the line (from apt.llvm.org, or from that cache when the
 *  archive cannot be reached at all; its pin, fingerprint check and read-back are what the checker's tests hold it
 *  to), and a build the cache did not hold is saved under its own key. */
export const WASM_CLANG_STEPS = [
  [
    '      - id: clang23-cache',
    '        uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0',
    '        with:',
    '          path: ~/.cache/llvm-23-debs',
    '          key: llvm-23-debs-${{ runner.arch }}-ubuntu-24.04-latest',
    '          restore-keys: llvm-23-debs-${{ runner.arch }}-ubuntu-24.04-',
  ].join('\n'),
  [
    '      - id: clang23',
    '        name: clang 23 for the wasm C build',
    '        run: bash scripts/ci-llvm.sh install',
  ].join('\n'),
  [
    '      - if: steps.clang23.outputs.changed == \'true\'',
    '        uses: actions/cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0',
    '        with:',
    '          path: ~/.cache/llvm-23-debs',
    '          key: llvm-23-debs-${{ runner.arch }}-ubuntu-24.04-${{ steps.clang23.outputs.build }}',
  ].join('\n'),
];
export const WASM_CLANG_BLOCK = WASM_CLANG_STEPS.join('\n');

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

function assertWasmJobsUseClang23(text, jobs, problems, file) {
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
    // A release build starts clean (no build cache in release.yml), so it runs the install step alone: an archive
    // that cannot be reached fails that build closed.
    const expected = file === 'release.yml' ? [WASM_CLANG_STEPS[1]] : WASM_CLANG_STEPS;
    if (lines.filter((l) => /^ {4}steps:/.test(l)).length !== 1 || steps[0] !== '      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7' || steps.slice(1, 1 + expected.length).join('\n') !== expected.join('\n')) {
      problems.push(`${file}: job "${name}" builds for wasm32-unknown-unknown; its ${expected.length === 1 ? 'second step' : 'second to fourth steps'}, right after actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7, must be exactly the pinned clang 23 ${expected.length === 1 ? 'install step (WASM_CLANG_STEPS[1]; a release build starts clean, without the cache steps)' : 'steps (WASM_CLANG_STEPS)'}`);
    }
    if (steps.filter((s) => s === WASM_CLANG_STEPS[1]).length > 1) {
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
function checkWholeModuleGTK(jobs, problems, file) {
  // The go set's shards load ./... through the partitioner (scripts/go-shard.mjs).
  const loadsAll = line => /(?:\bgo\s+(?:vet|test|list|build)\b|\bgolangci-lint\s+run\b|\bgovulncheck(?:@\S+)?\b)[^\n]*\s\.\/\.\.\.(?=\s|$|[);|])/.test(line) ||
    /\bnode scripts\/go-shard\.mjs run go\s/.test(line);
  for (const [name, body] of Object.entries(jobs)) {
    if (!/^ {4}runs-on: ubuntu-\S+$/m.test(body)) continue;
    const steps = jobSteps(body), consumers = steps.map((step, index) => step.some(loadsAll) ? index : -1).filter(index => index >= 0);
    if (!consumers.length) continue;
    const gtk = steps.findIndex(step => step.length === 1 && step[0] === 'run: sudo apt-get update && sudo apt-get install -y libgtk-3-dev');
    if (gtk < 0 || consumers.some(index => index <= gtk)) problems.push(`${file}: job "${name}" must install GTK development headers before loading all Go packages`);
    const header = body.split(/^ {4}steps:/m)[0];
    if (/\bCGO_ENABLED:\s*['"]?0\b/.test(header) || consumers.some(index => steps[index].some(line => !/^(?:name|id):/.test(line) && /\bCGO_ENABLED(?:=|:\s*['"]?)0\b|^if:/.test(line)))) {
      problems.push(`${file}: job "${name}" must retain cgo GTK frontend coverage in whole-module checks`);
    }
  }
}

/** The retried module download every Go job runs right after its Go (and, where present, Node) setup. */
const GO_MOD_DOWNLOAD_NAME = 'name: Download Go modules (retried only on known network errors)';
const GO_MOD_DOWNLOAD_RUN = "node scripts/retry-on.mjs --pattern 'proxy\\.golang\\.org[^\\n]*:\\s(50[0-4]|429)\\b|stream error|INTERNAL_ERROR|TLS handshake timeout|connection reset|i/o timeout' -- go mod download";
/** The Electron binary comes from GitHub releases: only a gateway error, a rate limit or a transport error repeats it, never a 404 or a checksum mismatch. */
const ELECTRON_INSTALL_NAME = 'name: Download the Electron binary (retried only on known network errors)';
const ELECTRON_INSTALL_RUN = "node scripts/retry-on.mjs --pattern 'Response code (50[0-4]|429)\\b|ETIMEDOUT|ECONNRESET|EAI_AGAIN|socket hang up' -- node node_modules/electron/install.js";
/** The macOS stapling retry: only Apple-service errors repeat the (re-notarizing) build. */
const STAPLE_RETRY = "node scripts/retry-on.mjs --pattern 'staple and validate action failed|NSURLErrorDomain|\\bError 68\\b' -- npm run dist -w @dilla/desktop";

/** Every job with actions/setup-go downloads its modules through retry-on.mjs, once, after the Go and Node setup. */
export function checkGoModuleDownloads(jobs, problems, file) {
  for (const [name, body] of Object.entries(jobs)) {
    const steps = jobSteps(body);
    const go = steps.findIndex((step) => step[0].startsWith('uses: actions/setup-go@'));
    if (go < 0) continue;
    const node = steps.findIndex((step) => step[0].startsWith('uses: actions/setup-node@'));
    const downloads = steps.map((step, index) => (step[0] === GO_MOD_DOWNLOAD_NAME ? index : -1)).filter((index) => index >= 0);
    const firstGoUse = steps.findIndex((s) => s.some((line) => !line.startsWith('name:') && /\bgo\s+(?:build|test|vet|run|list)\b|\bgolangci-lint\b|go-shard\.mjs/.test(line)));
    const setupIf = steps[go].find((line) => line.startsWith('if:'));
    const step = steps[downloads[0]];
    const expected = [GO_MOD_DOWNLOAD_NAME, ...(setupIf === undefined ? [] : [setupIf]), `run: ${GO_MOD_DOWNLOAD_RUN}`];
    if (downloads.length !== 1 || step.join('\n') !== expected.join('\n') || downloads[0] <= go || downloads[0] <= node || (firstGoUse >= 0 && downloads[0] > firstGoUse)) {
      problems.push(`${file}: job "${name}" must run exactly one retried "go mod download" step (${GO_MOD_DOWNLOAD_RUN}) after its setup-go and setup-node steps and before the first Go build or test, with setup-go's condition`);
    }
  }
}

/**
 * The Electron binary is cached per OS and version in electron_config_cache (the variable install.js reads) (step env: the runner context is not available in a
 * job's env), so the retried download only runs on a miss. The three steps stay together, after the job's npm ci.
 */
const ELECTRON_VERSION_STEP = [
  'name: Read the Electron version',
  'id: electron-version',
  'shell: bash',
  'run: echo "version=$(node -p "require(\'electron/package.json\').version")" >> "$GITHUB_OUTPUT"',
];
const ELECTRON_CACHE_STEP = [
  'name: Cache the Electron binary',
  'uses: actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0',
  'with:',
  'path: ${{ runner.temp }}/electron-cache',
  'key: electron-${{ runner.os }}-${{ runner.arch }}-${{ steps.electron-version.outputs.version }}',
];
const ELECTRON_INSTALL_STEP = [
  ELECTRON_INSTALL_NAME,
  `run: ${ELECTRON_INSTALL_RUN}`,
  'env:',
  'electron_config_cache: ${{ runner.temp }}/electron-cache',
];
const ELECTRON_STEPS = [ELECTRON_VERSION_STEP, ELECTRON_CACHE_STEP, ELECTRON_INSTALL_STEP].map((step) => step.join('\n')).join('\n');

const ELECTRON_INSTALLS_PER_FILE = { 'desktop.yml': 5, 'desktop-ci.yml': 1, 'server.yml': 0, 'mobile.yml': 0 };

/** Every step that runs electron/install.js is the one retried step, byte for byte, behind the version and cache steps. */
export function checkElectronInstalls(jobs, problems, file) {
  let installs = 0;
  for (const [name, body] of Object.entries(jobs)) {
    const steps = jobSteps(body);
    steps.forEach((step, index) => {
      if (!step.some((line) => line.includes('electron/install.js'))) return;
      installs += 1;
      const block = steps.slice(Math.max(0, index - 2), index + 1).map((lines) => lines.join('\n')).join('\n');
      const ci = steps.findIndex((lines) => lines.join('\n') === 'run: npm ci');
      if (block !== ELECTRON_STEPS) {
        problems.push(`${file}: job "${name}" must read the Electron version, cache electron_config_cache by OS, arch and that version, then download the binary through the retried step (${ELECTRON_INSTALL_RUN}), as three consecutive steps with no other condition or flag`);
      } else if (ci < 0 || ci >= index - 2) {
        problems.push(`${file}: job "${name}" must run npm ci before it reads the Electron version`);
      }
    });
  }
  // A deleted block would otherwise go unnoticed: each workflow keeps one per job that runs Electron.
  const want = ELECTRON_INSTALLS_PER_FILE[file];
  if (want !== undefined && installs !== want) problems.push(`${file}: must download the Electron binary in exactly ${want} steps, found ${installs}`);
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
  "      - name: Download Go modules (retried only on known network errors)",
  "        if: matrix.lane == 'managed'",
  `        run: ${GO_MOD_DOWNLOAD_RUN}`,
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
function checkLinuxSandboxFoundationJob(jobs, problems, file) {
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
    `- ${ELECTRON_VERSION_STEP[0]}`,
    ...ELECTRON_VERSION_STEP.slice(1),
    `- ${ELECTRON_CACHE_STEP[0]}`,
    ...ELECTRON_CACHE_STEP.slice(1),
    `- ${ELECTRON_INSTALL_STEP[0]}`,
    ...ELECTRON_INSTALL_STEP.slice(1),
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
  if (actual !== expected) problems.push(`${file}: Linux sandbox foundation must run both fixed native Ubuntu lanes with unchanged host policy, pinned provisioning and the unfiltered FUSE feasibility fixture`);
  const needs = /^ {4}needs: \[([^\]\n]*)\]$/m.exec(jobs.desktop ?? '')?.[1].split(',').map(name => name.trim()) ?? [];
  if (!needs.includes('linux-sandbox-foundation')) problems.push(`${file}: desktop must wait for Linux sandbox foundation`);
}

/**
 * Each browser job is a `run:` step of its own for its one suite, carrying the testkit binary in its own env, and it runs
 * no other `test:e2e` suite (a job folded back into several suites would hide a split job's budget). In Firefox an
 * audio-server `run:` step comes before the web and voice suites (headless Firefox's AudioContext stays suspended
 * without one, as in browser-media; web3.9 CI2: the web suite places calls too, and a suspended context reads no level, so
 * the fake microphone's tone counts as speech). Matched on steps, never on substrings, so a commented-out step or a
 * comment naming the command satisfies nothing.
 */
function checkSuiteSteps(jobs, problems, file) {
  for (const [job, spec] of Object.entries(BROWSER_WEB_JOBS)) {
    if (!(job in jobs)) continue;
    const steps = jobSteps(jobs[job]);
    const command = browserSuiteCommand(spec);
    const suiteAt = steps.findIndex((s) => s[0] === `run: ${command}`);
    if (suiteAt < 0) problems.push(`${file}: job "${job}" is missing: ${command} (a run step of its own)`);
    else if (!steps[suiteAt].includes(TESTKIT_ENV)) {
      problems.push(`${file}: job "${job}" runs ${command} without its own env ${TESTKIT_ENV}`);
    }
    const suites = steps.filter((s) => /^run: .*test:e2e/.test(s[0]));
    if (suites.length !== 1 || (suiteAt >= 0 && suites[0] !== steps[suiteAt])) {
      problems.push(`${file}: job "${job}" must run exactly one Playwright suite, ${command}, and no other test:e2e step`);
    }
    if (spec.engine !== 'firefox' || spec.suite === 'core') continue;
    const audioAt = steps.findIndex((s) => s[0] === 'run: sh scripts/ci-audio-server.sh');
    if (audioAt < 0) problems.push(`${file}: job "${job}" is missing: sh scripts/ci-audio-server.sh (a run step of its own)`);
    else if (suiteAt >= 0 && audioAt > suiteAt) problems.push(`${file}: ${job} audio server must start before the ${spec.suite} suite`);
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
  if (action === './.github/workflows/desktop.yml' && file === WORKFLOWS.desktop.file) return;
  if (!action.startsWith('actions/')) {
    problems.push(`${file}:${number}: ${action} is not a GitHub-owned action; use a reviewed local command`);
  } else if (!/^actions\/[\w-]+(?:\/[\w-]+)?@[0-9a-f]{40}$/.test(action)) {
    problems.push(`${file}:${number}: ${action} must use a 40-hex commit SHA`);
  }
}

/**
 * One CI workflow (server.yml, desktop-ci.yml or mobile.yml) against its own job list, gates, watchdog barrier and ok
 * check. Each runs on every pull request and push to main, never filtered at the trigger, so a required check always
 * reports.
 */
function checkCiWorkflow(root, workflow) {
  const file = fileOf(workflow);
  const { name: workflowName, ok, always } = WORKFLOWS[workflow];
  const barrier = `${workflow}-work-complete`;
  let text;
  try { text = readFileSync(join(root, '.github', 'workflows', file), 'utf8'); } catch { return [`${file}: missing`]; }
  const problems = [];
  const gates = JOB_GATES[workflow];
  const requiredJobs = [...REQUIRED_JOBS[workflow], ...(workflow === 'desktop' ? Object.keys(BROWSER_WEB_JOBS) : [])];

  // The release gates and ci-rerun-starved.yml find this workflow by name; a paths filter would leave its required
  // check unreported on a pull request it skips.
  let doc = null;
  try { doc = yaml.load(text); } catch (error) { problems.push(`${file}: invalid YAML (${error.message.split('\n')[0]})`); }
  if (doc !== null && doc?.name !== workflowName) problems.push(`${file}: the workflow must be named "${workflowName}" (the release gates and ci-rerun-starved.yml read it)`);
  // Only the nightly workflows (desktop, mobile) also run on a schedule (every area on, see scripts/ci-areas.mjs);
  // server.yml has none.
  const { nightly } = WORKFLOWS[workflow];
  const triggers = { push: { branches: ['main'] }, pull_request: null, ...(nightly ? { schedule: [{ cron: NIGHTLY_CRON }] } : {}) };
  if (doc !== null && !drSame(doc?.on, triggers)) {
    problems.push(`${file}: must trigger on every pull_request and every push to main${nightly ? `, and nightly on schedule cron '${NIGHTLY_CRON}'` : ''}, with no paths or other filter at the trigger (area skipping is the changes job's)`);
  }

  let currentJob = null;
  const workflowLines = text.split('\n');
  workflowLines.forEach((line, i) => {
    checkActionReference(line, file, i + 1, problems);
    const jobKey = /^ {2}([A-Za-z0-9_-]+):\s*$/.exec(line);
    if (jobKey !== null) currentJob = jobKey[1];
    for (const { re, what } of FORBIDDEN) {
      // Only the final gate and its no-op DAG barrier may run regardless of dependency results.
      if (what === 'if: always()' && [ok, barrier].includes(currentJob) && line === '    if: always()') continue;
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
      if (re.test(line)) problems.push(`${file}:${i + 1}: forbidden "${what}" — CI is fail-closed (R22)`);
    }
    // v0.1.0-web3.1: only release.yml stamps a version; a ci build is 0.0.0-dev, so it is never mistaken for a release.
    // A quoted symbol (-X 'main.Version=…', -X "main.Version=…") and the -X= spelling stamp it all the same.
    if (/-X[\s=]*['"]?main\.Version/.test(line)) problems.push(`${file}:${i + 1}: stamps main.Version; ci builds keep 0.0.0-dev (release.yml stamps the tag)`);
  });

  if (!/^concurrency:$/m.test(text) || !/cancel-in-progress: true/.test(text)) {
    problems.push(`${file}: missing the workflow-level concurrency block with cancel-in-progress: true`);
  }
  if (!/^concurrency:\n {2}group: \$\{\{ github\.workflow \}\}-\$\{\{ github\.event_name \}\}-\$\{\{ github\.ref \}\}\n {2}cancel-in-progress: true$/m.test(text)) {
    problems.push(`${file}: concurrency group must be workflow/event/ref so newer commits cancel obsolete runs for the same pull request, and a scheduled run and a push to main never cancel each other`);
  }
  if (!/^permissions:\n {2}contents: read$/m.test(text)) {
    problems.push(`${file}: the workflow-level token must be read-only (permissions: contents: read)`);
  }
  // gap-29 §4 ii: a workflow-level RUSTFLAGS overrides every target.*.rustflags outright.
  if (/^\s{2}RUSTFLAGS:/m.test(text)) {
    problems.push(`${file}: workflow-level RUSTFLAGS overrides .cargo/config.toml — use cargo clippy -- -D warnings`);
  }
  if (!/^\s{2}CARGO_TERM_COLOR: always$/m.test(text)) {
    problems.push(`${file}: missing env CARGO_TERM_COLOR: always`);
  }

  const jobs = splitJobs(text);
  if ('ci-watchdog' in jobs) {
    const body = jobs['ci-watchdog'];
    const required = [
      watchdogIf(workflow),
      '    timeout-minutes: 90',
      '    permissions:\n      contents: read\n      actions: write',
      '          persist-credentials: false',
      '      - run: python3 scripts/ci-watchdog.py\n        env:\n          GH_TOKEN: ${{ github.token }}',
      `          CI_COMPLETION_JOBS: ${completionJobs(workflow)}`,
    ];
    const permissions = /^    permissions:\n((?:      [^\n]+\n)+)/m.exec(body)?.[1];
    const steps = jobSteps(body);
    const expectedSteps = [
      ['uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7', 'with:', 'persist-credentials: false'],
      ['run: python3 scripts/ci-watchdog.py', 'env:', 'GH_TOKEN: ${{ github.token }}', `CI_COMPLETION_JOBS: ${completionJobs(workflow)}`],
    ];
    if (required.some(value => !body.includes(value)) || /^    (?:needs|name):/m.test(body) || /secrets\./.test(body) ||
        permissions !== '      contents: read\n      actions: write\n' || JSON.stringify(steps) !== JSON.stringify(expectedSteps)) {
      problems.push(`${file}: unsafe watchdog: require an independent, bounded, same-repository watcher with only its job token and credential-free checkout`);
    }
  }
  for (const name of requiredJobs) {
    if (!(name in jobs)) {
      problems.push(`${file}: missing job "${name}"`);
      continue;
    }
    for (const step of REQUIRED_STEPS[name] ?? []) {
      if (!jobs[name].includes(step)) problems.push(`${file}: job "${name}" is missing: ${step}`);
    }
  }
  // Each workflow holds its own jobs only: a job of the other one here would run twice, or gate the wrong release.
  for (const name of Object.keys(jobs)) {
    if (!requiredJobs.includes(name)) problems.push(`${file}: job "${name}" does not belong to the ${workflowName} workflow (REQUIRED_JOBS.${workflow})`);
  }
  checkWholeModuleGTK(jobs, problems, file);
  checkGoModuleDownloads(jobs, problems, file);
  checkElectronInstalls(jobs, problems, file);
  checkLinuxSandboxFoundationJob(jobs, problems, file);

  // Every test/build job needs `changes` and carries its gate, or it would run on a docs-only pull
  // request (or, with a wrong condition, be skipped on code).
  for (const [name, body] of Object.entries(jobs)) {
    const needs = /^ {4}needs:\s*\[([^\]]*)\]\s*$/m.exec(body);
    const needList = needs === null ? [] : needs[1].split(',').map((s) => s.trim());
    if (name === barrier) {
      const real = Object.keys(jobs).filter(other => ![ok, 'ci-watchdog', barrier].includes(other));
      if (needList.length !== real.length || real.some(other => !needList.includes(other)) ||
          !body.split('\n').includes('    if: always()') ||
          !body.includes('    timeout-minutes: 2') ||
          jobSteps(body).length !== 1 || !body.includes('      - run: echo "All CI work reached a terminal result"')) {
        problems.push(`${file}: completion barrier must wait for every real job, exclude the watcher/final gate, and perform only its bounded no-op`);
      }
      continue;
    }
    if (name === ok) {
      const others = Object.keys(jobs).filter(other => other !== ok);
      if (needList.length !== others.length || new Set(needList).size !== others.length || needList.some(other => !others.includes(other))) {
        problems.push(`${file}: job "${ok}" must list every other job of its workflow exactly once in its needs:`);
      }
      if (!body.split('\n').includes('    if: always()')) problems.push(`${file}: job "${ok}" must carry the job-level if: always(), or it is skipped when a job fails`);
      if (!body.includes("      - if: needs.ci-watchdog.result == 'skipped' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)\n        run: exit 1")) {
        problems.push(`${file}: job "${ok}" may accept a skipped watcher only for fork pull requests`);
      }
      for (const other of Object.keys(jobs)) {
        if (other !== ok && !needList.includes(other)) problems.push(`${file}: job "${ok}" must list "${other}" in its needs:`);
      }
      continue;
    }
    if (always.includes(name)) {
      if (name === 'docs' && (needList.includes('changes') || /^ {4}if:/m.test(body))) problems.push(`${file}: job "${name}" must run for every change: no needs: changes, no job-level if:`);
      continue;
    }
    if (!(name in gates)) {
      problems.push(`${file}: job "${name}" has no gate in JOB_GATES.${workflow} (scripts/ci-areas.mjs)`);
      continue;
    }
    if (!needList.includes('changes')) problems.push(`${file}: job "${name}" must list "changes" in its needs:`);
    if (!body.split('\n').includes(`    if: ${gateExpression(workflow, name)}`)) problems.push(`${file}: job "${name}" must carry the job-level if: ${gateExpression(workflow, name)}`);
    // A job whose needs are skipped is skipped too, so its gate may not be true where a needed job's is false.
    for (const need of needList) {
      if (!(need in gates)) continue;
      if (reachableOutputs().some((outputs) => jobRuns(workflow, name, outputs) && !jobRuns(workflow, need, outputs))) {
        problems.push(`${file}: job "${name}" can run while the job it needs, "${need}", is skipped`);
      }
    }
  }

  // The desktop command is a prefix of the phone one, so each must be a whole `- run:` line of the ui job.
  if ('ui' in jobs) {
    const runs = jobs.ui.split('\n').map((line) => line.trim());
    for (const command of UI_SHOTS_2C) {
      if (!runs.includes(`- run: ${command}`)) problems.push(`${file}: job "ui" is missing the web-2c screenshot step: ${command}`);
    }
    for (const command of UI_SHOTS_3) {
      if (!runs.includes(`- run: ${command}`)) problems.push(`${file}: job "ui" is missing the web-3 screenshot step: ${command}`);
    }
  }

  // The Go shard sets and the Postgres filter are the server workflow's, so only it reports on them.
  if (workflow === 'server') {
    assertGoShards(jobs, problems, GO_SHARD_SETS, file);
    assertPostgresAPITestsMatchRun(root, problems);
  }
  assertEveryUploadFailsOnNoFiles(text, problems, file);
  assertWasmPackUsesTheSizeProfile(text, problems, file);
  assertWasmJobsUseClang23(text, jobs, problems, file);

  // Jonas, 2026-10-21: no container image is built or published; deploy/install.sh is the one install.
  if (/docker\/|ghcr\.io|packages: write|build-ci-image|\bimage:\s*\$\{\{/.test(text)) problems.push(`${file}: builds or pushes a container image; the container image was dropped, deploy/install.sh is the one install`);

  // Plan B task 8 owns the `go` job; this plan owns `rust-wasi`. `actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8`
  // fetches from the same workflow run, so GitHub schedules `go` after `rust-wasi` only if a `needs:`
  // says so, and whichever plan lands second has to add the edge. Nobody owned that rule, so it lives
  // here. The job is required above; the guard keeps this block from piling a second, confusing
  // complaint on top of the plain "missing job" one.
  for (const job of ['go', 'go-checks']) {
    if (!(job in jobs)) continue;
    if (!/^\s*needs:.*rust-wasi/m.test(jobs[job])) {
      problems.push(`${file}: job "${job}" downloads the rust-wasi artifact but has no "needs: rust-wasi"`);
    }
    // gap-31 §4 item 2 / NV-14 (resolved): the current majors deliberately pair
    // `actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7` (this file's `rust-wasi` job) with `actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8`.
    // A `go` job written from facts/plan-B.md's now-stale deviation B15 text (`download-artifact@v4`) would pair v7
    // with v4, the untested combination B15 was created to avoid.
    if (!jobs[job].includes('actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8')) {
      problems.push(
        `${file}: job "${job}" must use actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8 to pair with rust-wasi's actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7 (gap-31 §4 item 2)`,
      );
    }
  }
  // The go job has no testkit binary, so requiring one there fails every run.
  if ('go' in jobs && jobs.go.includes('DILLA_TESTKIT_REQUIRED')) {
    problems.push(`${file}: job "go" requires the testkit but does not download it; the harness is go-harness's`);
  }
  if ('go-checks' in jobs) assertRaceGatedTestsRunWithoutRace(root, jobs['go-checks'], problems, file);

  if ('go-ds' in jobs && !/^\s*needs:.*rust-wasi/m.test(jobs['go-ds'])) {
    problems.push(`${file}: job "go-ds" downloads the rust-wasi artifact but has no "needs: rust-wasi"`);
  }

  if ('go-api' in jobs && !/^\s*needs:.*rust-wasi/m.test(jobs['go-api'])) {
    problems.push(`${file}: job "go-api" downloads the rust-wasi artifact but has no "needs: rust-wasi"`);
  }

  if ('go-harness' in jobs) {
    const harness = jobs['go-harness'];
    if (!/^\s*needs:.*rust-wasi/m.test(harness)) {
      problems.push(`${file}: job "go-harness" downloads the rust-wasi artifact but has no "needs: rust-wasi"`);
    }
    // The testkit binary is built by `rust-native` and downloaded by `go-harness`; without the
    // edge the download finds nothing in the run and the job is red on every push.
    if (!/^\s*needs:.*rust-native/m.test(harness)) {
      problems.push(`${file}: job "go-harness" downloads the rust-native testkit artifact but has no "needs: rust-native"`);
    }
  }

  if ('browser-media' in jobs) {
    const extractAt = jobs['browser-media'].indexOf('node packages/media/scripts/extract-rnnoise-wasm.mjs');
    const suiteAt = jobs['browser-media'].indexOf('npm run test:e2e:media -w @dilla/e2e');
    if (extractAt !== -1 && suiteAt !== -1 && extractAt > suiteAt) {
      problems.push(`${file}: browser-media RNNoise extraction must run before the media suite`);
    }
    // Firefox's AudioContext stays suspended on a runner with no audio server (first run of PR #7).
    const audioAt = jobs['browser-media'].indexOf('sh scripts/ci-audio-server.sh');
    if (audioAt !== -1 && suiteAt !== -1 && audioAt > suiteAt) {
      problems.push(`${file}: browser-media audio server must start before the media suite`);
    }
    for (const need of ['rust-wasi', 'rust-native']) {
      if (!new RegExp(`^\\s*needs:.*(?<![\\w-])${need}(?![\\w-])`, 'm').test(jobs['browser-media'])) {
        problems.push(`${file}: job "browser-media" downloads the ${need} artifact but has no "needs: ${need}"`);
      }
    }
  }

  checkSuiteSteps(jobs, problems, file);

  // Task 26: every artifact consumer waits for the producer in the same workflow run.
  for (const [job, needs] of [
    ...Object.keys(BROWSER_WEB_JOBS).map((job) => [job, ['rust-native', 'rust-wasi', 'web-build']]),
    ['go-release', ['web-build']],
    ['desktop', ['web-build']],
    ['mobile-bundle', ['mobile-core-wasm']],
    ['mobile-android', ['mobile-bundle', 'mobile-e2e-tools']],
    ['mobile-ios', ['mobile-bundle']],
  ]) {
    if (!(job in jobs)) continue;
    for (const need of needs) {
      if (!new RegExp(`^\\s*needs:.*(?<![\\w-])${need}(?![\\w-])`, 'm').test(jobs[job])) {
        problems.push(`${file}: job "${job}" downloads the ${need} artifact but has no "needs: ${need}"`);
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
      problems.push(`${file}: job "${job}" must fetch dilla-web-dist into internal/web/dist and refuse the placeholder before it builds`);
    }
  }

  return problems;
}

export const checkServerWorkflow = (root) => checkCiWorkflow(root, 'server');
export const checkDesktopCiWorkflow = (root) => checkCiWorkflow(root, 'desktop');
export const checkMobileCiWorkflow = (root) => checkCiWorkflow(root, 'mobile');

/**
 * The release workflow (install-release, 2026-10-14): a v* tag builds the assets deploy/install.sh downloads and
 * checks. It builds what it ships, so it is held to the CI workflows' rules for the same jobs (fail-closed lines, the
 * clang 23 pin, the size profile, the placeholder guard), and it uses only actions server.yml or desktop-ci.yml uses at
 * the same versions. It
 * builds and pushes no container image: the release is the native install's (Jonas, 2026-10-14).
 */
export const RELEASE_JOBS = ['tag', 'web-build', 'rust-wasi', 'bot-release', 'go-release', 'release'];
export const RELEASE_ASSETS = ['dillad-linux-amd64', 'dillad-linux-arm64', 'dillabot-linux-amd64', 'dillabot-linux-arm64', 'dilla-bot-core-linux-amd64', 'dilla-bot-core-linux-arm64', 'dilla_core_wasi.wasm', 'install.sh', 'install-bot.sh'];
const RELEASE_STEPS = {
  tag: [
    'test "$GITHUB_REF" = "refs/tags/$tag"',
    'INPUT_TAG: ${{ inputs.tag }}',
    '*-*) prerelease=true ;;',
    // Only a commit on main, which the CI workflows have gated, is released (review M6).
    'git merge-base --is-ancestor "$GITHUB_SHA" origin/main',
    // The server release gates on its own workflow: a green server push run on main for exactly the tagged commit.
    'name: The tagged commit has a green server run on main',
    'TAG_SHA: ${{ github.sha }}',
    'run: node scripts/server-release-gate.mjs "$TAG_SHA"',
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
    // Bot v2 task 18: the shipped Bot binary is read back and must not be a dilla_testhost (fixture music) build.
    "go version -m dist/dillabot-linux-${{ matrix.arch }} | grep -q 'build[[:space:]]CGO_ENABLED=0'",
    'if go version -m dist/dillabot-linux-${{ matrix.arch }} | grep -q dilla_testhost; then echo "a dillabot was built with the dilla_testhost tag"; exit 1; fi',
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
    // Bot v2 task 11: no released dillad is a dilla_unsafe_test build (the tag survives -trimpath in the build info).
    `test "$(go version -m dist/dillad-linux-amd64 dist/dillad-linux-arm64 | grep -c 'build[[:space:]]CGO_ENABLED=0')" -eq 2`,
    'if go version -m dist/dillad-linux-amd64 dist/dillad-linux-arm64 | grep -q dilla_unsafe_test; then echo "a dillad was built with the dilla_unsafe_test tag"; exit 1; fi',
    'if go version -m dist/dillad-linux-amd64 dist/dillad-linux-arm64 | grep -q dilla_testhost; then echo "a dillad was built with the dilla_testhost tag"; exit 1; fi',
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
    // A draft: mobile-release.yml signs the APK on main, attaches it and publishes (Jonas, 2026-11-06).
    'gh release create "$TAG" --verify-tag --draft',
    'flags="--prerelease"',
    `${RELEASE_ASSETS.map((a) => `release/${a}`).join(' ')} release/SHA256SUMS`,
  ],
};
const RELEASE_VERSION_ENV = 'VERSION: ${{ needs.tag.outputs.version }}';
const RELEASE_NEEDS = { 'web-build': ['tag'], 'rust-wasi': ['tag'], 'bot-release': ['tag', 'rust-wasi'], 'go-release': ['tag', 'web-build'], release: ['tag', 'go-release', 'rust-wasi', 'bot-release'] };

export function checkReleaseWorkflow(root) {
  const file = 'release.yml';
  const text = readFileSync(join(root, '.github', 'workflows', file), 'utf8');
  const ci = Object.values(WORKFLOWS).map(({ file: ciFile }) => { try { return readFileSync(join(root, '.github', 'workflows', ciFile), 'utf8'); } catch { return ''; } }).join('\n');
  const problems = [];
  const lines = text.split('\n');
  checkGoModuleDownloads(splitJobs(text), problems, file);

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
      problems.push(`${file}:${i + 1}: ${uses[1]} is not an action server.yml or desktop-ci.yml uses at that version; pin actions as they do`);
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
  // The gate lists the server workflow's runs: only the tag job reads actions, and a job-level block replaces the workflow's.
  if ('tag' in jobs && !/^ {4}permissions:\n {6}contents: read\n {6}actions: read$/m.test(jobs.tag)) {
    problems.push(`${file}: job "tag" needs exactly permissions: contents: read, actions: read for the server release gate`);
  }
  for (const [name, body] of Object.entries(jobs)) {
    if (name !== 'tag' && /actions: (?:read|write)/.test(body)) problems.push(`${file}: job "${name}" asks for actions access; only the tag job reads workflow runs`);
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
  // A v* tag run uses the workflow file at the tagged commit, which any tag pusher controls: no job here may enter an
  // environment or name a secret, no job may run past a failed gate, and every checkout is the tag's own commit.
  let doc;
  try { doc = yaml.load(text); } catch (error) { problems.push(`${file}: invalid YAML (${error.message.split('\n')[0]})`); return problems; }
  for (const [name, job] of Object.entries(doc?.jobs ?? {})) {
    if (Object.hasOwn(job ?? {}, 'environment')) problems.push(`${file}: job "${name}" enters an environment; a tag run's workflow file is the tag's, so release.yml holds no environment (signing is mobile-release.yml's, on main)`);
    if (typeof job?.if === 'string' && STATUS_CHECK.test(job.if)) problems.push(`${file}: job "${name}" has a job-level if: with a status check (${job.if}), which runs it past a failed need`);
    for (const step of job?.steps ?? []) {
      if (isCheckout(step) && Object.hasOwn(step.with ?? {}, 'ref')) problems.push(`${file}: job "${name}" checks out ref ${step.with.ref}; a release builds the tagged commit only`);
    }
  }
  for (const expr of expressions(text)) {
    if (/\bsecrets\b/.test(expr)) problems.push(`${file}: names secrets (\${{${expr}}}); release.yml reads no secret`);
  }
  return problems;
}

/** A job-level `if:` with a status check runs the job even when a need failed or was skipped. */
const STATUS_CHECK = /\b(?:always|cancelled|failure|success)\s*\(/;
const isCheckout = (step) => typeof step?.uses === 'string' && step.uses.startsWith('actions/checkout@');
/** Every `${{ … }}` expression body in a text. */
const expressions = (text) => [...text.matchAll(/\$\{\{([\s\S]*?)\}\}/g)].map((m) => m[1]);

/**
 * mobile-release.yml (2026-11-06, "Draft, sign on main"): release.yml leaves each v* release a draft; this workflow,
 * always main's file in the main-only mobile-release environment, signs the APK from the tag's commit, attaches it to
 * the draft, adds it to SHA256SUMS and publishes. The rules keep the secrets in the steps that use them (review M1).
 */
export const MOBILE_RELEASE_NEEDS = { 'mobile-bundle': ['resolve'], 'android-release': ['resolve', 'mobile-bundle'], publish: ['resolve', 'android-release'], 'ios-testflight': ['resolve', 'mobile-bundle'] };
const ANDROID_SECRETS = ['ANDROID_KEYSTORE_B64', 'ANDROID_KEYSTORE_PASSWORD', 'ANDROID_KEY_PASSWORD'];
const ASC_SECRETS = ['ASC_KEY_ID', 'ASC_ISSUER_ID', 'ASC_KEY_P8'];
/** job -> step name -> the secrets that step's env may name. No other step, and no job-level key, names a secret. */
export const MOBILE_SECRET_STEPS = {
  'android-release': { 'The signing secrets are set': ANDROID_SECRETS, 'Sign the APK with the release key (the keystore exists only during this step)': ANDROID_SECRETS },
  'ios-testflight': { 'The App Store Connect secrets and team id are set': ASC_SECRETS, 'Export and upload to TestFlight': ASC_SECRETS },
};
/** The one step of a job that writes a key file, the trap that removes it, and the write, which the trap precedes. */
const MOBILE_KEY_FILES = {
  'android-release': { step: 'Sign the APK with the release key (the keystore exists only during this step)', trap: `trap 'rm -f "$ks"' EXIT`, write: 'base64 -d > "$ks"' },
  'ios-testflight': { step: 'Export and upload to TestFlight', trap: `trap 'rm -f "$RUNNER_TEMP/asc-key.p8"' EXIT`, write: '> "$RUNNER_TEMP/asc-key.p8"' },
};
const RESOLVE_SHA = '${{ needs.resolve.outputs.sha }}';
/** The jobs that change the release run only while it is a draft; this exact condition keeps their needs in force. */
export const MOBILE_DRAFT_IF = "needs.resolve.outputs.draft == 'true'";
const MOBILE_DRAFT_ONLY = { 'android-release': true, publish: true };
const MOBILE_RELEASE_STEPS = {
  resolve: [
    '[ "$RUN_REF" = refs/heads/main ]',
    'git merge-base --is-ancestor "$sha" origin/main',
    '[ "$RUN_EVENT" = push ]',
    '[ "$sha" = "$RUN_SHA" ]',
    'actions/workflows/release.yml/runs?branch=$tag&status=success',
    'published releases are immutable',
    'echo "draft=$draft"',
  ],
  'android-release': ['npm run android:check-signer -w @dilla/mobile', 'npm run android:check-release-signed -w @dilla/mobile', 'if [ "$RUN_ATTEMPT" -gt 9 ]', 'dilla-android-apk'],
  publish: ['[ "$draft" = true ]', 'dilla-android-apk', '"digest-mismatch":"error"', '"APK_SHA256":"${{ needs.android-release.outputs.apk_sha256 }}"', 'printf \'%s  %s\\n\' "$APK_SHA256" "$apk" | sha256sum -c --strict', '--clobber', 'sha256sum -c --strict SHA256SUMS', 'gh release edit "$TAG" --draft=false'],
};

export function checkMobileReleaseWorkflow(root) {
  const file = 'mobile-release.yml';
  let text;
  try { text = readFileSync(join(root, '.github', 'workflows', file), 'utf8'); }
  catch (error) { if (error.code === 'ENOENT') return [`${file}: missing`]; throw error; }
  const problems = [];
  let doc;
  try { doc = yaml.load(text); } catch (error) { return [`${file}: invalid YAML (${error.message.split('\n')[0]})`]; }
  text.split('\n').forEach((line, i) => checkActionReference(line, file, i + 1, problems));
  const on = doc?.on ?? doc?.true ?? {};
  if (JSON.stringify(on.workflow_run?.workflows) !== '["release"]' || JSON.stringify(on.workflow_run?.types) !== '["completed"]') problems.push(`${file}: must run on workflow_run of [release], types [completed]`);
  if (!on.workflow_dispatch?.inputs?.tag) problems.push(`${file}: must offer workflow_dispatch with a tag input`);
  if (Object.keys(on).some((event) => !['workflow_run', 'workflow_dispatch'].includes(event))) problems.push(`${file}: runs only on workflow_run and workflow_dispatch (main's file)`);
  if (JSON.stringify(doc?.permissions) !== '{"contents":"read"}') problems.push(`${file}: the workflow-level token must be read-only (permissions: contents: read)`);
  for (const expr of expressions(text)) {
    if (/toJSON\s*\(\s*secrets\b/i.test(expr)) problems.push(`${file}: toJSON(secrets) exposes every secret`);
  }
  const jobs = doc?.jobs ?? {};
  for (const name of ['resolve', ...Object.keys(MOBILE_RELEASE_NEEDS)]) if (!(name in jobs)) problems.push(`${file}: missing job "${name}"`);
  // A step as text for the required lines, its run without comment lines and trailing ` # …` comments, so a
  // commented-out gate does not count (review L2).
  const uncomment = (run) => run.split('\n').filter((line) => !/^\s*#/.test(line)).map((line) => line.replace(/\s+#.*$/, '')).join('\n');
  const stepLines = (step) => JSON.stringify(typeof step?.run === 'string' ? { ...step, run: uncomment(step.run) } : step);
  for (const [name, job] of Object.entries(jobs)) {
    const steps = job?.steps ?? [];
    for (const want of MOBILE_RELEASE_STEPS[name] ?? []) {
      // A wanted line is script text (JSON-escaped to match) or, written as JSON, a with:/env: entry.
      if (!steps.some((step) => stepLines(step).includes(JSON.stringify(want).slice(1, -1)) || stepLines(step).includes(want))) problems.push(`${file}: job "${name}" is missing: ${want}`);
    }
    const needs = typeof job?.needs === 'string' ? [job.needs] : job?.needs ?? [];
    for (const need of MOBILE_RELEASE_NEEDS[name] ?? []) if (!needs.includes(need)) problems.push(`${file}: job "${name}" must list "${need}" in its needs:`);
    // Only resolve, which has no needs, may carry a free job-level if:. The Android jobs carry exactly the draft
    // condition, which has no status check and so still runs them only when their needs succeeded; every other job
    // carries none (always(), !cancelled() or success() || failure() would run a job past a failed gate).
    if (name in MOBILE_DRAFT_ONLY) {
      if (job?.if !== MOBILE_DRAFT_IF) problems.push(`${file}: job "${name}" must carry exactly if: ${MOBILE_DRAFT_IF} (a published release is immutable), got ${job?.if === undefined ? 'none' : `if: ${job.if}`}`);
    } else if (name !== 'resolve' && Object.hasOwn(job ?? {}, 'if')) problems.push(`${file}: job "${name}" has a job-level if: (${job.if}); it must run only when its needs succeeded`);
    const environment = job?.environment;
    if (name in MOBILE_SECRET_STEPS) {
      if (environment !== 'mobile-release') problems.push(`${file}: job "${name}" must run in environment: mobile-release, which holds its secrets`);
    } else if (environment !== undefined) problems.push(`${file}: job "${name}" enters an environment; only ${Object.keys(MOBILE_SECRET_STEPS).join(' and ')} do`);
    if (name === 'publish' ? JSON.stringify(job?.permissions) !== '{"contents":"write"}' : job?.permissions !== undefined && JSON.stringify(job.permissions).includes('write')) {
      problems.push(name === 'publish' ? `${file}: job "publish" needs exactly permissions: contents: write` : `${file}: job "${name}" asks for write access; only publish writes`);
    }
    // Secrets only in the env of the steps that use them; nowhere at job level, in a with:, or in a script.
    const { steps: _steps, ...jobKeys } = job ?? {};
    for (const expr of expressions(JSON.stringify(jobKeys))) if (/\bsecrets\b/.test(expr)) problems.push(`${file}: job "${name}" names a secret at job level (\${{${expr}}})`);
    steps.forEach((step, index) => {
      const label = step?.name ?? step?.uses ?? step?.run?.split('\n')[0] ?? `step ${index + 1}`;
      const allowed = MOBILE_SECRET_STEPS[name]?.[step?.name];
      const { env = {}, ...rest } = step ?? {};
      for (const expr of expressions(JSON.stringify(rest))) if (/\bsecrets\b/.test(expr)) problems.push(`${file}: job "${name}" step "${label}" names a secret outside its env (\${{${expr}}})`);
      for (const [key, value] of Object.entries(env ?? {})) {
        for (const expr of expressions(String(value))) {
          if (!/\bsecrets\b/.test(expr)) continue;
          const secret = /^\s*secrets\.([A-Z0-9_]+)\s*$/.exec(expr)?.[1];
          if (allowed === undefined || secret === undefined || !allowed.includes(secret)) problems.push(`${file}: job "${name}" step "${label}" names a secret it may not (${key}: \${{${expr}}}); secrets live only in ${Object.entries(MOBILE_SECRET_STEPS).map(([j, s]) => `${j}: ${Object.keys(s).join(', ')}`).join('; ')}`);
        }
      }
      if (isCheckout(step) && name !== 'resolve') {
        if (name === 'publish') problems.push(`${file}: job "publish" checks out the repository; the job that writes runs no repository code`);
        else if (step.with?.ref !== RESOLVE_SHA || step.with?.['persist-credentials'] !== false) problems.push(`${file}: job "${name}" must check out ref: ${RESOLVE_SHA} (the gated commit) with persist-credentials: false`);
      }
    });
    const keyFile = MOBILE_KEY_FILES[name];
    if (keyFile) {
      const writers = steps.filter((step) => typeof step?.run === 'string' && step.run.includes(keyFile.write));
      const run = writers[0]?.run ?? '';
      if (writers.length !== 1 || writers[0].name !== keyFile.step || !run.includes(keyFile.trap) || run.indexOf(keyFile.trap) > run.indexOf(keyFile.write)) {
        problems.push(`${file}: job "${name}": the key file is written in one step only, "${keyFile.step}", whose ${keyFile.trap} comes before the write`);
      }
    }
  }
  return problems;
}

export function checkDesktopWorkflow(root) {
  const file = 'desktop.yml';
  let text;
  try { text = readFileSync(join(root, '.github', 'workflows', file), 'utf8'); }
  catch (error) { if (error.code === 'ENOENT') return ['desktop.yml: missing desktop acceptance workflow']; throw error; }
  const problems = [];
  problems.push(...checkNativeStockFeasibility(text, { workflow: true }).map(problem => `${file}: native stock feasibility: ${problem}`));
  // The e2e shards run every test so one round shows every failing test; the watchdog leaves their failures running too.
  if (/--max-failures/.test(text)) {
    problems.push(`${file}: desktop e2e shards must run every test, so --max-failures is not allowed`);
  }
  text.split('\n').forEach((line, i) => {
    checkActionReference(line, file, i + 1, problems);
    for (const { re, what } of FORBIDDEN) if (re.test(line)) problems.push(`${file}:${i + 1}: forbidden ${what}`);
  });
  for (const required of [
    'workflow_call:', 'contents: read', 'fail-fast: true',
    'os: ubuntu-24.04, platform: linux, arch: x64', 'os: windows-latest, platform: win, arch: x64',
    'os: macos-15, platform: mac, arch: arm64',
    'name: dilla-web-dist', 'name: dilla-desktop-core',
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
  // Build matrices stop at the first failure; the e2e shards keep running so one round shows every failing test.
  for (const job of ['test-tools', 'native-linux-package-acceptance', 'native', 'client', 'voice']) {
    const want = job === 'client' || job === 'voice' ? 'false' : 'true';
    const got = /^ {6}fail-fast:\s*(\w+)/m.exec(body(job))?.[1];
    if (got !== want) {
      problems.push(`${file}: ${job} matrix must set fail-fast: ${want} (builds stop the matrix on the first failure, e2e shards run every test)`);
    }
  }
  for (const job of ['native', 'client', 'voice']) {
    const steps = jobSteps(body(job));
    const install = steps.findIndex(step => step.join('\n') === ELECTRON_INSTALL_STEP.join('\n'));
    const seal = steps.map((step, index) => step.some(line => line.includes('prepare-mac-electron-runtime.ts')) ? index : -1).filter(index => index >= 0);
    const expected = "name: Seal macOS npm Electron runtime for native notifications\nif: runner.os == 'macOS'\nrun: node e2e/web/support/prepare-mac-electron-runtime.ts";
    if (install < 0 || seal.length !== 1 || seal[0] !== install + 1 || steps[seal[0]].join('\n') !== expected)
      problems.push(`${file}: ${job} must seal the macOS npm Electron runtime immediately after installation`);
  }
  checkGoModuleDownloads(splitJobs(text), problems, file);
  checkElectronInstalls(splitJobs(text), problems, file);
  // Three notarizing macOS builds (STAPLE_RETRY's attempts) do not fit in 30 minutes.
  if (!/^ {4}timeout-minutes: 60$/m.test(body('native'))) problems.push(`${file}: the native job must carry timeout-minutes: 60, room for the macOS staple retry's three builds`);
  const nativeSteps = jobSteps(body('native'));
  const distSteps = nativeSteps.filter((step) => step.some((line) => line.includes('npm run dist -w @dilla/desktop')));
  const distArgs = '-- --config.electronDist=../../node_modules/electron/dist --${{ matrix.platform }} --${{ matrix.arch }}';
  const [macDist, plainDist] = distSteps;
  // The retry wrapper is macOS-only (it cannot start npm.cmd on Windows and only stapling needs it); the other platforms run plain.
  if (distSteps.length !== 2 || macDist[0] !== 'name: Build installable desktop artifacts (macOS, staple retry)' || macDist[1] !== "if: runner.os == 'macOS'"
    || !macDist.includes(`run: ${STAPLE_RETRY} ${distArgs}`)
    || plainDist[0] !== 'name: Build installable desktop artifacts' || plainDist[1] !== "if: runner.os != 'macOS'"
    || !plainDist.includes(`run: npm run dist -w @dilla/desktop ${distArgs}`)
    || JSON.stringify(macDist.slice(3)) !== JSON.stringify(plainDist.slice(3))) {
    problems.push(`${file}: the native installer build must be a macOS-only step through retry-on.mjs retrying only the macOS stapling errors, plus the same build plain on other platforms`);
  }
  const formatAcceptance = body('native-linux-package-acceptance');
  for (const required of [
    'name: desktop-linux-package-acceptance-${{ matrix.format }}-x64',
    'needs: [test-tools, native-linux-packages]', 'runs-on: ubuntu-24.04',
    '- { format: deb, label: DEB }', '- { format: rpm, label: RPM }', '- { format: aur, label: AUR }',
    'persist-credentials: false', 'node-version-file: .nvmrc',
    'name: desktop-installers-linux-packages-x64', 'path: ${{ runner.temp }}/native-linux-package-inputs',
    'name: dilla-e2e-tools-linux-x64-${{ github.sha }}', 'name: dilla-web-dist', 'name: dilla-core-wasi',
    'run: npm ci --ignore-scripts',
    'run: node --test e2e/unit/native-linux-package-acceptance.test.mjs e2e/unit/native-stock-sandbox.test.mjs',
    'name: Installed ${{ matrix.label }} package acceptance\n        run: bash e2e/web/support/native-linux-package-run.sh "${{ matrix.format }}"',
    'name: desktop-linux-package-observation-${{ matrix.format }}-x64', 'if-no-files-found: error',
  ]) if (!formatAcceptance.includes(required)) problems.push(`${file}: native package acceptance missing ${required}`);
  if (/^\s*if:|continue-on-error|run-id:|repository:|github-token:|persist-credentials: true|build-native-linux-packages|--no-sandbox/m.test(formatAcceptance)) problems.push(`${file}: native package acceptance must consume same-run artifacts without skips, rebuilds or credentials`);
  try {
    const job = yaml.load(text).jobs['native-linux-package-acceptance'], steps = job?.steps ?? [];
    const download = name => steps.findIndex(step => step.uses?.startsWith('actions/download-artifact@') && step.with?.name === name);
    const accept = steps.findIndex(step => step.name === 'Installed ${{ matrix.label }} package acceptance' && step.run === 'bash e2e/web/support/native-linux-package-run.sh "${{ matrix.format }}"');
    if (JSON.stringify(job?.strategy?.matrix?.include) !== JSON.stringify([{format:'deb',label:'DEB'},{format:'rpm',label:'RPM'},{format:'aur',label:'AUR'}])
      || steps.filter(step => step.uses?.startsWith('actions/checkout@') && step.with?.['persist-credentials'] === false).length !== 1
      || accept < 0 || ['desktop-installers-linux-packages-x64','dilla-e2e-tools-linux-x64-${{ github.sha }}','dilla-web-dist','dilla-core-wasi'].some(name => download(name) < 0 || download(name) >= accept)
      || [...steps, job].some(item => !item || Object.hasOwn(item,'if') || Object.hasOwn(item,'continue-on-error'))) problems.push(`${file}: native package acceptance must execute all three formats after their same-run artifact downloads`);
  } catch { problems.push(`${file}: invalid native package acceptance YAML`); }
  const packages = body('native-linux-packages'), packageSteps = jobSteps(packages);
  const packageBuild = `name: Build native Linux release packages
run: |
version="$(node -p "require('./packages/desktop/package.json').version")"
node --input-type=module -e "import {verifyShippingCoreWasmInDist} from './e2e/web/support/shipping-wasm.mjs'; verifyShippingCoreWasmInDist('packages/web/dist')"
node packages/desktop/scripts/build-native-linux-packages.mjs build "$RUNNER_TEMP/native-linux-packages" "$version"
node --input-type=module -e "import {checkDesktopFuses} from './packages/desktop/scripts/check-fuses.mjs'; await checkDesktopFuses(process.argv[1])" "$RUNNER_TEMP/native-linux-packages"
node packages/desktop/scripts/build-native-linux-packages.mjs recipe "$version" "$RUNNER_TEMP/native-linux-packages/dilla-$version-linux-x64.deb" "$RUNNER_TEMP/native-linux-recipe"
cp "$RUNNER_TEMP/native-linux-recipe/dilla-$version-linux-x64.aur.tar.gz" "$RUNNER_TEMP/native-linux-packages/"`;
  const packageProof = 'name: Record native Linux package build provenance\nrun: node packages/desktop/scripts/build-provenance.mjs "$RUNNER_TEMP/native-linux-packages" linux-packages-x64';
  const packageUpload = `uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
with:
name: desktop-installers-linux-packages-x64
path: |
\${{ runner.temp }}/native-linux-packages/*.deb
\${{ runner.temp }}/native-linux-packages/*.rpm
\${{ runner.temp }}/native-linux-packages/*.aur.tar.gz
\${{ runner.temp }}/native-linux-packages/build-provenance-linux-packages-x64.json
if-no-files-found: error`;
  const packageBuildIndex = packageSteps.findIndex(step => step.join('\n') === packageBuild);
  const packageProofIndex = packageSteps.findIndex(step => step.join('\n') === packageProof);
  const packageUploadIndex = packageSteps.findIndex(step => step.join('\n') === packageUpload);
  const packageCheckout = `uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7
with:
persist-credentials: false`;
  if (!packages.includes('name: desktop-linux-packages-x64') || !packages.includes('runs-on: ubuntu-24.04')
   || packageSteps[0]?.join('\n') !== packageCheckout || packageBuildIndex < 0 || packageProofIndex <= packageBuildIndex || packageUploadIndex <= packageProofIndex
   || ['dilla-web-dist', 'dilla-desktop-core'].some(name => !packageSteps.slice(0, packageBuildIndex).some(step => step.includes('name: ' + name) && step.includes('uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8')))
   || /^\s*if:|run-id:|repository:|github-token:|packages\/desktop\/out|--no-sandbox/m.test(packages)) problems.push(`${file}: native Linux package producer must build the source version with existing client/core, verify fuses and independent clean provenance, and upload only its exact three outputs and record`);

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
node --test packages/desktop/test/linux-installed-sandbox-bootstrap.test.mjs packages/desktop/test/linux-sandbox-package-runner.test.mjs packages/desktop/test/linux-installed-runtime-proof.test.mjs`);
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
  const failureUpload = `if: failure() && steps.desktop-build.outcome == 'success' && hashFiles('packages/desktop/test-results/**') != ''
uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
with:
name: desktop-failure-\${{ matrix.platform }}-\${{ matrix.arch }}
path: packages/desktop/test-results
if-no-files-found: error`;
  // hashFiles and upload-artifact see only the workspace: the Windows audio logs are copied into it by the step
  // directly before the upload, which must fail on nothing (no continue-on-error, no || true, no always()).
  const collectLogs = `name: Collect the Windows audio logs
if: failure() && steps.desktop-build.outcome == 'success'
shell: bash
run: for f in "$RUNNER_TEMP"/dilla-windows-audio/*.log "$RUNNER_TEMP"/dilla-windows-audio/*.out "$RUNNER_TEMP"/dilla-windows-audio/*.err; do if [ -f "$f" ]; then mkdir -p packages/desktop/test-results/windows-audio && cp "$f" packages/desktop/test-results/windows-audio/; fi; done`;
  const failureUploads = nativeSteps.map((step,index)=>step.join('\n')===failureUpload ? index : -1).filter(index=>index>=0);
  if(desktopBuild<0 || failureUploads.length!==1 || failureUploads[0]<=desktopBuild || nativeSteps[failureUploads[0]-1]?.join('\n')!==collectLogs)problems.push(`${file}: native failure diagnostics require the exact post-build upload`);
  const unitSteps = nativeSteps.filter(step => step.some(line => line.includes('run: npm test -w @dilla/desktop')));
  if (unitSteps.length !== 1 || unitSteps[0].join('\n') !== 'run: npm test -w @dilla/desktop') {
    problems.push(`${file}: desktop unit tests must run on every native platform`);
  }
  const proof = nativeSteps.findIndex(step => step.some(line => line.includes('name: desktop-installed-upgrade-')));
  const sandboxGateName = 'name: Linux AppImage sandbox native acceptance';
  const sandboxGates = nativeSteps.map((step,index)=>step.includes(sandboxGateName)?index:-1).filter(index=>index>=0);
  const sandboxGate = `name: Linux AppImage sandbox native acceptance
if: runner.os == 'Linux'
run: node packages/desktop/test/support/linux-installed-runtime-proof.mjs packages/desktop/test-results/linux-installed-upgrade.json`;
  if(sandboxGates.length!==1 || nativeSteps[sandboxGates[0]]?.join('\n')!==sandboxGate
    || sandboxGates[0]<=installedLinux || sandboxGates[0]>=proof)
    problems.push(`${file}: installed AppImage sandbox requires exact non-bypassable kernel proof verification after Linux installed upgrade and before upload`);
  const buildProvenance = nativeSteps.findIndex(step => step.some(line => line.includes('name: Record native desktop build provenance')));
  const finalDistribution = nativeSteps.findLastIndex(step => step.some(line => line.includes('npm run dist -w @dilla/desktop')));
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
  const macDiskImage = nativeSteps.map((step, index) => step.join('\n') === `name: macOS update disk image check
if: runner.os == 'macOS'
run: node packages/desktop/scripts/check-mac-dmg.mjs packages/desktop/out` ? index : -1).filter(index => index >= 0);
  if (macDiskImage.length !== 1 || nativeSteps.filter(step => step.some(line => line.includes('check-mac-dmg.mjs'))).length !== 1 || macDiskImage[0] <= finalFuses || macDiskImage[0] >= buildProvenance)
    problems.push(`${file}: the macOS update disk image (the key-rotation update) must be checked exactly once after the final fused distribution and before provenance`);
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
  if (/APPIMAGE_EXTRACT_AND_RUN|--appimage-extract/.test(body('native'))) problems.push(`${file}: Linux installed acceptance must exercise normal FUSE launch`);
  const tooling = body('test-tools');
  for (const required of [
    'os: ubuntu-24.04, platform: linux, arch: x64', 'os: windows-latest, platform: win, arch: x64',
    'os: macos-15, platform: mac, arch: arm64',
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
    ...Array.from({ length: WEB_SHARDS }, (_, i) => `os: ubuntu-24.04, platform: linux, arch: x64, shard: ${i + 1} }`),
    "os: windows-latest, platform: win, arch: x64, shard: 1, specs: 'web/installed-upgrade-account.spec.ts",
    "os: macos-15, platform: mac, arch: arm64, shard: 1, specs: 'web/installed-upgrade-account.spec.ts",
    'name: dilla-core-wasi', 'node packages/desktop/scripts/prepare-browser-sandbox.mjs',
    `playwright test --config playwright.desktop.config.ts --project=electron-client --shard=\${{ matrix.shard }}/${WEB_SHARDS}`,
    'playwright test --config playwright.desktop.config.ts --project=electron-client ${{ matrix.specs }}',
  ]) if (!client.includes(required)) problems.push(`${file}: shared Electron client coverage missing ${required}`);
  // Every Linux shard and no more: an extra one would run nothing, a missing one would drop part of the suite.
  const linuxShards = client.match(/^ {10}- \{ os: ubuntu-24\.04, platform: linux, arch: x64, shard: \d+ \}$/gm) ?? [];
  if (linuxShards.length !== WEB_SHARDS) problems.push(`${file}: the Linux client suite must run in exactly ${WEB_SHARDS} shards, not ${linuxShards.length}`);
  const voice = body('voice');
  for (const required of ['desktop-voice-${{ matrix.platform }}-${{ matrix.arch }}',
    'os: [ubuntu-24.04, windows-latest, macos-15]',
    'playwright.desktop-voice.config.ts --project=electron-voice --workers=1', 'DILLA_E2E_NOTIFICATIONS', 'dunst',
    'name: dilla-core-wasi', 'node packages/desktop/scripts/prepare-browser-sandbox.mjs',
    'name: Desktop performance comparison', 'node packages/desktop/scripts/run-performance.mjs', 'name: desktop-performance-']) {
    if (!voice.includes(required)) problems.push(`${file}: Electron voice coverage missing ${required}`);
  }
  if (/--shard|shard:/.test(voice)) problems.push(`${file}: the desktop voice suite must preserve its single frozen-clock worker`);
  const voiceSteps = jobSteps(voice);
  const lastVoice = voiceSteps.findLastIndex(step => step.includes('name: Desktop voice end-to-end coverage in Electron'));
  const baselineInstall = voiceSteps.findIndex(step => step.includes('run: npm exec -w @dilla/e2e -- playwright install --with-deps chromium'));
  const baselineSandbox = voiceSteps.findIndex(step => step.includes('run: node packages/desktop/scripts/prepare-browser-sandbox.mjs'));
  const firstPerformance = voiceSteps.findIndex(step => step.includes('name: Desktop performance comparison'));
  if (lastVoice < 0 || baselineInstall <= lastVoice || baselineSandbox <= baselineInstall || firstPerformance <= baselineSandbox)
    problems.push(`${file}: browser baseline setup must run after Electron voice and before performance measurement`);
  assertEveryUploadFailsOnNoFiles(text, problems, file);
  return problems;
}

/*
 * The desktop release trampoline (plan 2026-11-03, Task 10; job split from implementation batch B).
 * Both workflows are parsed, never text-matched alone, so flow style, mappings, expressions and
 * block scalars cannot hide anything. desktop-release-request.yml is an unprivileged echo on a
 * desktop-v* tag; desktop-release.yml is the main-only workflow_run that holds the keys:
 *   gate -> collect (keyless early-failure check; no outputs, no uploads) -> sign (environment
 *   desktop-release: fetches the gate-digested CI zips as blobs and reads them in-process, no
 *   download action; the six secrets in one step) -> publish (contents: write, no keys; takes only
 *   sign's uploads by id) -> verify-published -> feed (GitHub Pages).
 */
export const DESKTOP_RELEASE_SECRETS = Object.freeze(['DESKTOP_METADATA_KEY_A', 'DESKTOP_SPARKLE_KEY_A', 'DESKTOP_SANDBOX_KEY_A', 'DESKTOP_METADATA_KEY_B', 'DESKTOP_SPARKLE_KEY_B', 'DESKTOP_SANDBOX_KEY_B']);
export const DESKTOP_RELEASE_ACTIONS = Object.freeze([
  'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1', // v7
  'actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1', // v7
  'actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333', // v8
  'actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9', // v7
  'actions/upload-pages-artifact@fc324d3547104276b827a68afc52ff2a11cc49c9', // v5.0.0
  'actions/deploy-pages@368f82528645a54fb793d4d04e342629a3f51346', // v5.0.1
]);
export const DESKTOP_RELEASE_IF = "github.event.workflow_run.event == 'push' && github.event.workflow_run.conclusion == 'success'";
const DR_REQUEST_NAME = 'desktop release request';
const DR_SIGN_STEP = 'Sign (the only step that receives secrets)';
const DR_JOBS = ['gate', 'collect', 'sign', 'publish', 'verify-published', 'feed'];
const DR_PERMISSIONS = {
  gate: { contents: 'read', actions: 'read' },
  collect: { contents: 'read', actions: 'read', 'pull-requests': 'read' },
  // actions: read only to fetch the gate-digested artifact zips through the REST API.
  sign: { contents: 'read', actions: 'read', 'pull-requests': 'read' },
  publish: { contents: 'write', actions: 'read' },
  'verify-published': { contents: 'read' },
  feed: { contents: 'read', pages: 'write', 'id-token': 'write' },
};
const DR_NEEDS = { collect: ['gate'], sign: ['gate', 'collect'], publish: ['gate', 'sign'], 'verify-published': ['gate', 'sign', 'publish'], feed: ['gate', 'sign', 'publish', 'verify-published'] };
const GATE_OUT = name => `\${{ needs.gate.outputs.${name} }}`;
/** Exact job outputs: collect has none, so nothing it produces can reach sign or publish. */
const DR_OUTPUTS = {
  gate: { ...Object.fromEntries(['tag', 'version', 'channel', 'sha', 'tree', 'run', 'artifact_ids', 'artifact_digests', 'run_record', 'jobs_record', 'previous', 'mac_update'].map(name => [name, `\${{ steps.gate.outputs.${name} }}`])),
    records_id: '${{ steps.records.outputs.artifact-id }}', records_digest: '${{ steps.records.outputs.artifact-digest }}' },
  collect: undefined,
  sign: { staged: '${{ steps.sign.outputs.staged }}', digest: '${{ steps.digest.outputs.digest }}', candidate_digest: '${{ steps.candidate_digest.outputs.candidate_digest }}',
    signed_id: '${{ steps.ids.outputs.signed_id }}', candidate_id: '${{ steps.ids.outputs.candidate_id }}' },
  publish: undefined, 'verify-published': undefined, feed: undefined,
};
const DR_ENVIRONMENTS = { sign: 'desktop-release', feed: 'github-pages' };
const DR_JOB_KEYS = new Set(['if', 'needs', 'runs-on', 'timeout-minutes', 'environment', 'concurrency', 'permissions', 'outputs', 'steps']);
/** Only the Pages deploy is serialised (a superseded pending feed is harmless: every feed deploys the newest published releases). */
const DR_CONCURRENCY = { feed: { group: 'desktop-release-feed', 'cancel-in-progress': false } };
/** Never in a release step's env, pinned or not: loader, shell, runtime, git and proxy overrides (review batch B, m3). */
const DR_BANNED_ENV = /^(LD_|DYLD_|BASH_ENV$|ENV$|BASH_FUNC_|SHELLOPTS$|BASHOPTS$|PS4$|IFS$|NODE_|NPM_CONFIG_|GIT_|SSL_CERT_|CURL_|HTTPS?_PROXY$|ALL_PROXY$|NO_PROXY$|PATH$|HOME$|TMPDIR$|PYTHON|PERL5|RUBY|GCONV_PATH$|GITHUB_|RUNNER_|ACTIONS_)/i;
const DR_STEP_KEYS = new Set(['name', 'id', 'uses', 'with', 'run', 'env']);
const S = 'node packages/desktop/scripts/';
/** Every job's exact step sequence, by step kind (see drKind). */
const DR_ORDER = {
  gate: ['checkout', 'setup-node', 'release-gate', 'upload:desktop-release-gate'],
  collect: ['checkout', 'setup-node', 'download:desktop-release-gate', 'download:ci', 'release-download-check', 'release-assemble collect', 'release-notes generate', 'release-assemble assemble'],
  // No download action in sign: `release-candidate build` fetches the gate-digested zips as blobs and reads them in-process.
  sign: ['checkout', 'setup-node', 'release-candidate build', 'release-sign-job', 'candidate-digest write', 'release-candidate digest',
    'upload:desktop-signed-release', 'upload:desktop-signed-candidate', 'release-candidate record-id'],
  publish: ['checkout', 'setup-node', 'download:signed', 'candidate-digest verify', 'download:candidate', 'release-candidate verify', 'publish-release plan', 'publish-release publish'],
  'verify-published': ['checkout', 'setup-node', 'verify-published'],
  feed: ['checkout', 'setup-node', 'download:signed', 'candidate-digest verify', 'pages-feed build', 'upload-pages', 'deploy-pages', 'pages-feed verify'],
};
/** Exact run commands. */
const DR_RUNS = {
  'release-gate': `${S}release-gate.mjs "$RUNNER_TEMP/gate"`,
  'release-download-check': `${S}release-download-check.mjs "$RUNNER_TEMP/gate" "$RUNNER_TEMP/candidate"`,
  'release-assemble collect': `${S}release-assemble.mjs collect "$VERSION" "$RUNNER_TEMP/downloaded" "$RUNNER_TEMP/candidate" "$RUNNER_TEMP/gate"`,
  'release-notes generate': `${S}release-notes.mjs generate "$VERSION" "$SHA" "$RUNNER_TEMP/candidate/release-notes.md"`,
  'release-assemble assemble': `${S}release-assemble.mjs assemble "$VERSION" "$RUNNER_TEMP/candidate" "$TREE"`,
  'release-candidate digest': `${S}release-candidate.mjs digest "$RUNNER_TEMP/candidate" "$VERSION"`,
  'release-candidate record-id': `${S}release-candidate.mjs record-id`,
  'release-candidate build': `${S}release-candidate.mjs build "$RUNNER_TEMP/zips" "$RUNNER_TEMP/candidate"`,
  'release-sign-job': `${S}release-sign-job.mjs "$VERSION" "$RUNNER_TEMP/candidate" "$RUNNER_TEMP/signed"`,
  'candidate-digest write': `${S}candidate-digest.mjs write "$RUNNER_TEMP/signed" "$CHANNEL" "$VERSION" "$STAGED"`,
  'candidate-digest verify': `${S}candidate-digest.mjs verify "$RUNNER_TEMP/signed" "$CHANNEL" "$VERSION" "$STAGED" "$DIGEST"`,
  'release-candidate verify': `${S}release-candidate.mjs verify "$RUNNER_TEMP/candidate"`,
  'publish-release plan': `${S}publish-release.mjs plan`,
  'publish-release publish': `${S}publish-release.mjs publish`,
  'verify-published': `${S}verify-published.mjs`,
  'pages-feed build': `${S}pages-feed.mjs build "$RUNNER_TEMP/signed" "$CHANNEL" "$VERSION" "$RUNNER_TEMP/site"`,
  'pages-feed verify': `${S}pages-feed.mjs verify "$RUNNER_TEMP/signed" "$CHANNEL"`,
};
const DR_UPLOAD = (name, dir) => ({ name, path: `\${{ runner.temp }}/${dir}`, 'if-no-files-found': 'error', 'retention-days': 3 });
/** Exact `with` of every action step, by job and kind. */
const DR_WITH = {
  checkout: job => ['gate', 'sign', 'publish', 'feed'].includes(job) ? { 'fetch-depth': 0, 'persist-credentials': false } : { 'persist-credentials': false },
  'setup-node': () => ({ 'node-version-file': '.nvmrc', 'package-manager-cache': false }),
  'download:desktop-release-gate': () => ({ name: 'desktop-release-gate', path: '${{ runner.temp }}/gate', 'digest-mismatch': 'error' }),
  // sign's two uploads, each by the artifact id sign reports (an id names immutable bytes; a name could be re-uploaded).
  'download:signed': () => ({ 'artifact-ids': '${{ needs.sign.outputs.signed_id }}', path: '${{ runner.temp }}/signed', 'digest-mismatch': 'error' }),
  'download:candidate': () => ({ 'artifact-ids': '${{ needs.sign.outputs.candidate_id }}', path: '${{ runner.temp }}/candidate', 'digest-mismatch': 'error' }),
  'download:ci': () => ({ 'github-token': '${{ github.token }}', 'run-id': '${{ needs.gate.outputs.run }}', repository: 'dilla-chat/dilla',
    'artifact-ids': '${{ needs.gate.outputs.artifact_ids }}', path: '${{ runner.temp }}/downloaded', 'digest-mismatch': 'error' }),
  'upload:desktop-release-gate': () => DR_UPLOAD('desktop-release-gate', 'gate'),
  'upload:desktop-signed-release': () => DR_UPLOAD('desktop-signed-release', 'signed'),
  'upload:desktop-signed-candidate': () => DR_UPLOAD('desktop-signed-candidate', 'candidate'),
  'upload-pages': () => ({ path: '${{ runner.temp }}/site' }),
  'deploy-pages': () => undefined,
};
const DR_ACTION_KINDS = { checkout: 'checkout', 'setup-node': 'setup-node', 'upload-pages-artifact': 'upload-pages', 'deploy-pages': 'deploy-pages' };
const PUBLISH_ENV = { GH_TOKEN: '${{ github.token }}', DESKTOP_TAG: GATE_OUT('tag'), DESKTOP_SHA: GATE_OUT('sha'), DESKTOP_STAGED: '${{ needs.sign.outputs.staged }}',
  DESKTOP_MAC_UPDATE: GATE_OUT('mac_update'), DESKTOP_RELEASE_CANDIDATE: '${{ runner.temp }}/signed', DESKTOP_INSTALLERS: '${{ runner.temp }}/candidate' };
/** Exact env of every run step (review batch B, m3): a value from collect, or a loader variable, cannot slip in. */
const DR_ENV = {
  'gate release-gate': { GH_TOKEN: '${{ github.token }}', HINT_TAG: '${{ github.event.workflow_run.head_branch }}', HINT_RUN: '${{ github.event.workflow_run.id }}' },
  // The notes range starts at the gate's validated previous published release; the notes never read tags.
  'collect release-notes generate': { GH_TOKEN: '${{ github.token }}', VERSION: '${{ needs.gate.outputs.version }}', SHA: '${{ needs.gate.outputs.sha }}', PREVIOUS: '${{ needs.gate.outputs.previous }}' },
  'collect release-download-check': { GH_TOKEN: '${{ github.token }}', ...Object.fromEntries(['run', 'artifact_ids', 'artifact_digests', 'run_record', 'jobs_record'].map(name => [name.toUpperCase(), GATE_OUT(name)])) },
  'collect release-assemble collect': { VERSION: GATE_OUT('version') },
  'collect release-assemble assemble': { VERSION: GATE_OUT('version'), TREE: GATE_OUT('tree') },
  'sign candidate-digest write': { CHANNEL: GATE_OUT('channel'), VERSION: GATE_OUT('version'), STAGED: '${{ steps.sign.outputs.staged }}' },
  'sign release-candidate digest': { VERSION: GATE_OUT('version') },
  'publish candidate-digest verify': { CHANNEL: GATE_OUT('channel'), VERSION: GATE_OUT('version'), STAGED: '${{ needs.sign.outputs.staged }}', DIGEST: '${{ needs.sign.outputs.digest }}' },
  'publish publish-release plan': PUBLISH_ENV,
  'publish publish-release publish': PUBLISH_ENV,
  'verify-published verify-published': { GH_TOKEN: '${{ github.token }}', DESKTOP_TAG: GATE_OUT('tag'), DESKTOP_SHA: GATE_OUT('sha'), DESKTOP_STAGED: '${{ needs.sign.outputs.staged }}', DESKTOP_MAC_UPDATE: GATE_OUT('mac_update') },
  'feed candidate-digest verify': { CHANNEL: GATE_OUT('channel'), VERSION: GATE_OUT('version'), STAGED: '${{ needs.sign.outputs.staged }}', DIGEST: '${{ needs.sign.outputs.digest }}' },
  'feed pages-feed verify': { CHANNEL: GATE_OUT('channel') },
  // Every id and digest the zip fetch trusts comes from the gate's outputs, never from collect.
  'sign release-candidate build': { GH_TOKEN: '${{ github.token }}', ...Object.fromEntries(['version', 'sha', 'tree', 'run', 'artifact_ids', 'artifact_digests', 'records_id', 'records_digest', 'run_record', 'jobs_record', 'previous']
    .map(name => [name.toUpperCase(), GATE_OUT(name)])) },
  // MAC_UPDATE: the gate's decision; 'dmg' only for a channel's rotation release of a pending macOS key change.
  'sign release-sign-job': { VERSION: GATE_OUT('version'), MAC_UPDATE: GATE_OUT('mac_update'), ...Object.fromEntries(DESKTOP_RELEASE_SECRETS.map(name => [name, `\${{ secrets.${name} }}`])) },
  'sign release-candidate record-id': { SIGNED_ID: '${{ steps.signed_upload.outputs.artifact-id }}', CANDIDATE_ID: '${{ steps.candidate_upload.outputs.artifact-id }}' },
  // The feed reads published releases through the API (contents: read), never the live site.
  'feed pages-feed build': { GH_TOKEN: '${{ github.token }}', CHANNEL: GATE_OUT('channel'), VERSION: GATE_OUT('version'), MAC_UPDATE: GATE_OUT('mac_update') },
  'publish release-candidate verify': { ...Object.fromEntries(['version', 'sha', 'tree', 'run', 'run_record', 'jobs_record'].map(name => [name.toUpperCase(), GATE_OUT(name)])),
    CANDIDATE_DIGEST: '${{ needs.sign.outputs.candidate_digest }}' },
};

function drKind(step) {
  if (typeof step?.uses === 'string') {
    const action = /^actions\/([a-z-]+)@/.exec(step.uses)?.[1];
    if (action === 'download-artifact') {
      const w = step.with ?? {};
      if (w['run-id'] !== undefined || w['github-token'] !== undefined || w.repository !== undefined) return 'download:ci';
      if (w.name !== undefined) return `download:${w.name}`;
      const ids = String(w['artifact-ids'] ?? '');
      return ids.includes('signed_id') ? 'download:signed' : ids.includes('candidate_id') ? 'download:candidate' : 'download:other';
    }
    if (action === 'upload-artifact') return `upload:${step.with?.name}`;
    return DR_ACTION_KINDS[action] ?? `action:${step.uses}`;
  }
  const match = typeof step?.run === 'string' ? /^node packages\/desktop\/scripts\/([a-z-]+)\.mjs(?: ([a-z-]+))?(?:\s|$)/.exec(step.run) : null;
  return match ? match[1] + (match[2] ? ' ' + match[2] : '') : `run:${step?.run}`;
}
/** Every string in a parsed document, with its path. */
function drStrings(value, path = [], out = []) {
  if (typeof value === 'string') out.push([path, value]);
  else if (Array.isArray(value)) value.forEach((item, i) => drStrings(item, [...path, i], out));
  else if (value && typeof value === 'object') for (const [key, item] of Object.entries(value)) { out.push([[...path, `key:${key}`], key]); drStrings(item, [...path, key], out); }
  return out;
}
const drSame = (a, b) => JSON.stringify(a) === JSON.stringify(b);
const drNeeds = value => typeof value === 'string' ? [value] : value;

/** Checks `file` and, with `seen`, every repository file it imports: only node: modules and repository files. */
function checkScriptImports(root, file, problems, seen = null) {
  if (seen?.has(file)) return null;
  seen?.add(file);
  let text;
  try { text = readFileSync(join(root, file), 'utf8'); } catch { problems.push(`${file}: missing`); return null; }
  for (const match of text.matchAll(/(?:^|[\s;])(?:import|export)\s+(?:[^'";]*?\sfrom\s+)?(['"])([^'"]+)\1|import\(\s*([^)]*)\)/g)) {
    const specifier = match[2] ?? match[3];
    const literal = match[2] !== undefined ? specifier : /^(['"])([^'"]+)\1$/.exec(specifier.trim())?.[2];
    if (literal === undefined) { problems.push(`${file}: dynamic import of a computed specifier`); continue; }
    if (!literal.startsWith('node:') && !literal.startsWith('./') && !literal.startsWith('../')) problems.push(`${file}: imports ${literal}; only node: modules and repository files are allowed`);
    // Static imports load with the module and are followed. A literal dynamic import of a repository
    // file loads only when its code path runs (build-provenance's CI-only inspectors) and is not walked.
    else if (seen && match[2] !== undefined && !literal.startsWith('node:')) checkScriptImports(root, posix.join(posix.dirname(file), literal), problems, seen);
  }
  return text;
}
/** The scripts the keyed sign job runs; it runs no `npm ci`, so their whole import graph is node: built-ins and repository files. */
const DR_KEYED_SCRIPTS = Object.freeze(['release-candidate.mjs', 'artifact-zip.mjs', 'release-sign-job.mjs', 'candidate-digest.mjs'].map(name => `packages/desktop/scripts/${name}`));

export function checkDesktopReleaseWorkflow(root) {
  const problems = [];
  const directory = join(root, '.github', 'workflows');
  const files = {};
  for (const name of readdirSync(directory).filter(name => /\.ya?ml$/.test(name)).sort()) {
    const text = readFileSync(join(directory, name), 'utf8');
    let doc;
    try { doc = yaml.load(text); } catch (error) { problems.push(`${name}: invalid YAML (${error.message.split('\n')[0]})`); continue; }
    files[name] = { text, doc };
  }
  const request = files['desktop-release-request.yml'], keyed = files['desktop-release.yml'];
  if (!request) problems.push('desktop-release-request.yml: missing');
  if (!keyed) problems.push('desktop-release.yml: missing');

  // Rules that hold in every workflow: the release environment and secrets are confined.
  for (const [name, { text, doc }] of Object.entries(files)) {
    if (/secrets\s*\[/.test(text)) problems.push(`${name}: secrets[...] indexing is forbidden`);
    // Parsed first (a quoted "inherit" is the same YAML value), with the text match as a backstop.
    const inherits = Object.values(doc?.jobs ?? {}).some(body => body && typeof body === 'object' && body.secrets === 'inherit');
    if (inherits || /secrets\s*:\s*['"]?inherit\b/.test(text)) problems.push(`${name}: secrets: inherit is forbidden; pass named secrets`);
    // GitHub Pages serves the desktop update feed: only desktop-release.yml's feed job may deploy it (review batch B, m2).
    if (name !== 'desktop-release.yml') {
      const pagesWrite = permissions => permissions === 'write-all' || (permissions && typeof permissions === 'object' && permissions.pages === 'write');
      if (pagesWrite(doc?.permissions)) problems.push(`${name}: pages: write is reserved for desktop-release.yml's feed job`);
      for (const [job, body] of Object.entries(doc?.jobs ?? {})) {
        if (!body || typeof body !== 'object') continue;
        const environment = typeof body.environment === 'string' ? body.environment : body.environment?.name;
        // GitHub resolves environment and secret names case-insensitively, so every name check here ignores case (final review, m1).
        if (typeof environment === 'string' && environment.toLowerCase() === 'github-pages') problems.push(`${name}: ${job}: the github-pages environment is reserved for desktop-release.yml's feed job`);
        if (pagesWrite(body.permissions)) problems.push(`${name}: ${job}: pages: write is reserved for desktop-release.yml's feed job`);
        for (const step of Array.isArray(body.steps) ? body.steps : []) {
          if (typeof step?.uses === 'string' && /^actions\/(deploy-pages|upload-pages-artifact)@/.test(step.uses)) problems.push(`${name}: ${job}: ${step.uses.split('@')[0]} is reserved for desktop-release.yml's feed job`);
        }
      }
    }
    if (/toJSON\(\s*secrets\s*\)/i.test(text)) problems.push(`${name}: toJSON(secrets) is forbidden`);
    for (const [job, body] of Object.entries(doc?.jobs ?? {})) {
      if (!body || typeof body !== 'object' || !Object.hasOwn(body, 'environment')) continue;
      const value = body.environment;
      if (typeof value !== 'string') { problems.push(`${name}: ${job}: environment must be a plain name, not a mapping`); continue; }
      if (value.includes('${{')) problems.push(`${name}: ${job}: environment must not be an expression`);
      const allowed = name === 'desktop-release.yml' ? DR_ENVIRONMENTS[job] : undefined;
      if (allowed !== undefined ? value !== allowed : (name === 'desktop-release.yml' || /desktop-release/i.test(value))) problems.push(`${name}: ${job}: environment ${value} is not allowed here; desktop-release belongs to the sign job of desktop-release.yml only`);
    }
    if (name === 'desktop-release.yml' || name === 'desktop-release-request.yml') continue;
    if (/DESKTOP_(METADATA|SPARKLE|SANDBOX)_KEY/i.test(text)) problems.push(`${name}: names a desktop signing secret`);
    if (/release-sign-job\.mjs/.test(text)) problems.push(`${name}: runs the desktop sign job script`);
    if (/desktop-release\b(?![-.])/i.test(text)) problems.push(`${name}: names the desktop-release environment`);
    const upstream = doc?.on?.workflow_run?.workflows;
    if ([].concat(upstream ?? []).some(item => item === DR_REQUEST_NAME || item === 'desktop release')) problems.push(`${name}: chains on the desktop release workflows`);
  }

  if (request) {
    const { doc, text } = request, r = 'desktop-release-request.yml';
    if (!drSame(Object.keys(doc ?? {}), ['name', 'on', 'permissions', 'jobs'])) problems.push(`${r}: top level must be exactly name, on, permissions and jobs`);
    if (doc?.name !== DR_REQUEST_NAME) problems.push(`${r}: name must be "${DR_REQUEST_NAME}"`);
    if (!drSame(doc?.on, { push: { tags: ['desktop-v*'] } })) problems.push(`${r}: must trigger only on push of desktop-v* tags`);
    if (!drSame(doc?.permissions, { contents: 'read' })) problems.push(`${r}: permissions must be exactly contents: read`);
    const jobs = doc?.jobs ?? {}, job = jobs.request;
    if (!drSame(Object.keys(jobs), ['request'])) problems.push(`${r}: must have exactly one job, request`);
    if (!job || Object.keys(job).some(key => !['runs-on', 'timeout-minutes', 'steps'].includes(key))) problems.push(`${r}: request may carry only runs-on, timeout-minutes and steps`);
    if (job?.['runs-on'] !== 'ubuntu-24.04') problems.push(`${r}: request must run on ubuntu-24.04`);
    if (!drSame(job?.steps, [{ run: 'echo "Desktop release requested; the privileged workflow re-derives everything from main."' }])) problems.push(`${r}: request must only echo (no uses, no env, no expressions)`);
    if (/secrets|\$\{\{/.test(text)) problems.push(`${r}: must not reference secrets or expressions`);
    if (keyed && keyed.doc?.on?.workflow_run?.workflows?.[0] !== doc?.name) problems.push('desktop-release.yml: workflow_run must name the request workflow exactly');
  }

  if (keyed) {
    const { doc, text } = keyed, k = 'desktop-release.yml';
    // No workflow concurrency group: GitHub keeps one pending run per group and silently cancels it when a third
    // arrives (review batch B, m5). The gate fails loudly instead while another run is active.
    if (!drSame(Object.keys(doc ?? {}), ['name', 'on', 'permissions', 'jobs'])) problems.push(`${k}: top level must be exactly name, on, permissions and jobs (no concurrency, env or defaults)`);
    if (doc?.name !== 'desktop release') problems.push(`${k}: name must be "desktop release"`);
    if (!drSame(doc?.on, { workflow_run: { workflows: [DR_REQUEST_NAME], types: ['completed'] } })) problems.push(`${k}: must trigger only on workflow_run of "${DR_REQUEST_NAME}", types [completed], no branches`);
    if (!drSame(doc?.permissions, {})) problems.push(`${k}: top-level permissions must be {}`);
    if (/always\(\)|\|\|\s*true|continue-on-error|cancelled\(\)|failure\(\)/.test(text)) problems.push(`${k}: no always(), || true, continue-on-error, cancelled() or failure()`);
    if (/NODE_OPTIONS|NODE_PATH/.test(text) || /NODE_OPTIONS|NODE_PATH/.test(request?.text ?? '')) problems.push(`${k}: NODE_OPTIONS and NODE_PATH are forbidden in the release workflows`);
    const jobs = doc?.jobs ?? {};
    if (!drSame(Object.keys(jobs), DR_JOBS)) problems.push(`${k}: jobs must be exactly ${DR_JOBS.join(', ')}`);
    let signSteps = 0;
    for (const name of DR_JOBS) {
      const job = jobs[name];
      if (!job || typeof job !== 'object') continue;
      const extra = Object.keys(job).filter(key => !DR_JOB_KEYS.has(key));
      if (extra.length) problems.push(`${k}: ${name}: forbidden job keys ${extra.join(', ')} (defaults, container, services, env, continue-on-error...)`);
      if (name === 'gate' ? job.if !== DESKTOP_RELEASE_IF : Object.hasOwn(job, 'if')) problems.push(`${k}: ${name}: ${name === 'gate' ? `if must be exactly ${DESKTOP_RELEASE_IF}` : 'no if besides the gate\'s'}`);
      if (!drSame(drNeeds(job.needs), DR_NEEDS[name])) problems.push(`${k}: ${name}: needs must be ${JSON.stringify(DR_NEEDS[name] ?? null)}`);
      if (job['runs-on'] !== 'ubuntu-24.04') problems.push(`${k}: ${name}: must run on ubuntu-24.04`);
      if (!Number.isInteger(job['timeout-minutes'])) problems.push(`${k}: ${name}: needs a timeout-minutes`);
      if (!drSame(job.permissions, DR_PERMISSIONS[name])) problems.push(`${k}: ${name}: permissions must be exactly ${JSON.stringify(DR_PERMISSIONS[name])}`);
      if (DR_ENVIRONMENTS[name] !== undefined && job.environment !== DR_ENVIRONMENTS[name]) problems.push(`${k}: ${name}: environment must be ${DR_ENVIRONMENTS[name]}`);
      if (DR_ENVIRONMENTS[name] === undefined && Object.hasOwn(job, 'environment')) problems.push(`${k}: ${name}: must not have an environment`);
      if (!drSame(job.outputs, DR_OUTPUTS[name])) problems.push(`${k}: ${name}: outputs must be exactly ${JSON.stringify(DR_OUTPUTS[name] ?? null)}`);
      if (!drSame(job.concurrency, DR_CONCURRENCY[name])) problems.push(`${k}: ${name}: concurrency must be exactly ${JSON.stringify(DR_CONCURRENCY[name] ?? null)}`);
      const steps = Array.isArray(job.steps) ? job.steps : [];
      const kinds = steps.map(drKind);
      // Review batch B, I1/I2: no action in sign ever extracts a zip another job could have produced.
      if (name === 'sign' && kinds.some(kind => kind.startsWith('download:'))) problems.push(`${k}: sign: no download-artifact step; sign fetches the gate-digested zips itself (release-candidate build)`);
      if (!drSame(kinds, DR_ORDER[name])) problems.push(`${k}: ${name}: steps must be exactly, in order: ${DR_ORDER[name].join(' -> ')} (found ${kinds.join(' -> ')})`);
      steps.forEach((step, i) => {
        const where = `${k}: ${name}: step ${i + 1}`;
        const extraStep = Object.keys(step ?? {}).filter(key => !DR_STEP_KEYS.has(key));
        if (extraStep.length) problems.push(`${where}: forbidden step keys ${extraStep.join(', ')} (if, shell, working-directory...)`);
        const kind = kinds[i];
        if (typeof step?.uses === 'string') {
          if (!DESKTOP_RELEASE_ACTIONS.includes(step.uses)) problems.push(`${where}: ${step.uses} is not one of the pinned first-party actions`);
          const expected = DR_WITH[kind]?.(name);
          if (!drSame(step.with, expected)) problems.push(`${where}: ${kind} must use exactly ${JSON.stringify(expected)}`);
          if (step.run !== undefined || step.env !== undefined) problems.push(`${where}: an action step carries no run or env`);
        } else {
          if (typeof step?.run !== 'string') { problems.push(`${where}: must be a run or uses step`); return; }
          if (step.run.includes('${{')) problems.push(`${where}: no \${{ }} inside run; pass values through env`);
          if (DR_RUNS[kind] === undefined || step.run !== DR_RUNS[kind]) problems.push(`${where}: run must be exactly a pinned release command`);
          if (/\b(npm|npx|git)\b/.test(step.run)) problems.push(`${where}: no npm, npx or git in the release workflow`);
          for (const key of Object.keys(step.env ?? {})) if (DR_BANNED_ENV.test(key)) problems.push(`${where}: ${key} is forbidden in a release step's env`);
          // Every run step's env is pinned exactly, as its run line is.
          if (!Object.hasOwn(DR_ENV, `${name} ${kind}`)) problems.push(`${where}: no pinned env for ${kind} in ${name}`);
        }
        const pinnedEnv = DR_ENV[`${name} ${kind}`];
        if (pinnedEnv !== undefined && !drSame(step.env, pinnedEnv)) problems.push(`${where}: env must be exactly ${Object.keys(pinnedEnv).join(', ')}`);
        if (step?.name === DR_SIGN_STEP) { signSteps++; if (name !== 'sign' || kind !== 'release-sign-job') problems.push(`${where}: only the sign job's signing step carries the Sign name`); }
        if (kind === 'release-sign-job' && step?.name !== DR_SIGN_STEP) problems.push(`${where}: the signing step must be named "${DR_SIGN_STEP}"`);
      });
    }
    if (signSteps !== 1) problems.push(`${k}: exactly one "${DR_SIGN_STEP}" step`);
    // Secrets: the six names, once each, only in the Sign step's env; the event payload only in the gate.
    const secretRefs = [...text.matchAll(/secrets\.([A-Za-z0-9_]+)/g)].map(match => match[1]).sort();
    if (!drSame(secretRefs, [...DESKTOP_RELEASE_SECRETS].sort())) problems.push(`${k}: the six DESKTOP_* secrets must each be referenced exactly once`);
    const signIndex = (jobs.sign?.steps ?? []).findIndex(step => step?.name === DR_SIGN_STEP);
    const gateIndex = (jobs.gate?.steps ?? []).findIndex(step => drKind(step) === 'release-gate');
    for (const [path, value] of drStrings(doc)) {
      const at = path.join('.');
      if (/needs\s*\.\s*collect\b|needs\s*\[\s*['"]collect/.test(value)) problems.push(`${k}: nothing collect produces may reach another job (${at})`);
      if (/secrets\b/.test(value) && /secrets\s*[.[(]|\(\s*secrets/.test(value)) {
        const key = path[path.length - 1];
        if (!(at === `jobs.sign.steps.${signIndex}.env.${key}` && DESKTOP_RELEASE_SECRETS.includes(key) && value === `\${{ secrets.${key} }}`)) problems.push(`${k}: secrets referenced outside the Sign step's env (${at})`);
      }
      if (/github\.event/.test(value)) {
        const ok = (at === 'jobs.gate.if' && value === DESKTOP_RELEASE_IF)
          || (at === `jobs.gate.steps.${gateIndex}.env.HINT_TAG` && value === '${{ github.event.workflow_run.head_branch }}')
          || (at === `jobs.gate.steps.${gateIndex}.env.HINT_RUN` && value === '${{ github.event.workflow_run.id }}');
        if (!ok) problems.push(`${k}: the workflow_run payload may reach only the gate's if and its HINT_TAG/HINT_RUN env (${at})`);
      }
    }
    if (/workflow_run\.(head_sha|head_repository)/.test(text)) problems.push(`${k}: workflow_run.head_sha and head_repository are never used`);
  }

  // The scripts that hold or create the keys, and the whole import graph of the sign job's steps
  // (the in-process zip reader included), import only node: modules and repository files.
  const keys = checkScriptImports(root, 'packages/desktop/scripts/release-keys.mjs', problems);
  const seen = new Set();
  for (const file of DR_KEYED_SCRIPTS) checkScriptImports(root, file, problems, seen);
  // Final review, I-1: a partial re-run of an old keyed run reuses its gate's outputs with old code. gate, sign
  // (release-candidate build, before any secret) and publish refuse every attempt but the first and repeat the main
  // and one-run checks; only the keyless feed job stays re-runnable. Each call must be a statement on its own line.
  const scriptText = file => { try { return readFileSync(join(root, 'packages/desktop/scripts', file), 'utf8'); } catch { problems.push(`packages/desktop/scripts/${file}: missing`); return ''; } };
  const statement = (text, call) => new RegExp(`^[ \\t]*${call.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`, 'm').test(text);
  if (!statement(scriptText('keyed-run.mjs'), "if (env.GITHUB_RUN_ATTEMPT !== '1') {")) problems.push("keyed-run.mjs: refuseRerunAttempt must refuse every GITHUB_RUN_ATTEMPT but '1'");
  for (const [file, job, calls] of [['release-gate.mjs', 'gate', []], ['release-candidate.mjs', 'sign', ['assertCheckoutIsMain(env, git);']], ['publish-release.mjs', 'publish', ['assertCheckoutIsMain(env, git);']]]) {
    const text = scriptText(file);
    for (const call of [`refuseRerunAttempt(env, '${job}');`, 'await assertNoOtherReleaseRun(env, api);', ...calls]) {
      if (!statement(text, call)) problems.push(`${file}: the ${job} job must run ${call} (otherwise a re-run attempt or old keyed code reaches it)`);
    }
  }
  if (/refuseRerunAttempt/.test(scriptText('pages-feed.mjs'))) problems.push('pages-feed.mjs: feed is the one keyed-run job that may be re-run');
  if (keys !== null) {
    if (/type\s*:\s*['"]tag['"]/.test(keys)) problems.push('release-keys.mjs: the desktop-release environment admits no tag policy');
    // The environment setup names exactly one deployment policy: the branch main.
    if (!/deployment-branch-policies/.test(keys) || ((keys.match(/type\s*:\s*['"]branch['"]/g) ?? []).length !== 1 || !/\{\s*name\s*:\s*['"]main['"]\s*,\s*type\s*:\s*['"]branch['"]\s*\}/.test(keys))) {
      problems.push("release-keys.mjs: the environment policy must be the single { name: 'main', type: 'branch' }");
    }
  }
  return problems;
}

/**
 * ci-rerun-starved.yml holds `actions: write` on a workflow_run event, so it is pinned as a privileged workflow:
 * it fires only on a completed server or desktop run, its sole permission is actions: write (contents: read at most), it
 * re-runs attempt 1 only, checks out the default branch and never the triggering run's code, and takes the
 * run's id and attempt through env rather than inline in a command.
 */
const RERUN_STARVED_IF = "github.event.workflow_run.run_attempt == 1 && github.event.workflow_run.conclusion == 'failure'";
const RERUN_STARVED_ENV = { RUN_ID: '${{ github.event.workflow_run.id }}', RUN_ATTEMPT: '${{ github.event.workflow_run.run_attempt }}', RUN_CONCLUSION: '${{ github.event.workflow_run.conclusion }}' };

export function checkRerunStarvedWorkflow(root) {
  const file = 'ci-rerun-starved.yml';
  const problems = [];
  const fail = (message) => problems.push(`${file}: ${message}`);
  let text, doc;
  try {
    text = readFileSync(join(root, '.github', 'workflows', file), 'utf8');
    doc = yaml.load(text);
  } catch (error) { return [`${file}: ${error.code === 'ENOENT' ? 'missing' : `invalid YAML (${error.message.split('\n')[0]})`}`]; }
  // The CI workflows' own names, so renaming one cannot silently unhook this one.
  const ciNames = Object.values(WORKFLOWS).map(({ file: ciFile, name }) => {
    try { return yaml.load(readFileSync(join(root, '.github', 'workflows', ciFile), 'utf8'))?.name ?? name; } catch { return name; /* checkCiWorkflow reports it missing */ }
  });
  if (!drSame(doc?.on, { workflow_run: { workflows: ciNames, types: ['completed'] } })) fail(`must trigger only on workflow_run of ${ciNames.map((n) => `"${n}"`).join(' and ')}, types [completed], no branches`);
  if (!drSame(doc?.permissions, { actions: 'write' })) fail('the workflow permissions must be exactly actions: write');
  const jobs = Object.values(doc?.jobs ?? {});
  if (jobs.length !== 1) fail('must have exactly one job');
  for (const job of jobs) {
    if (job?.permissions !== undefined && !drSame(job.permissions, { actions: 'write' }) && !drSame(job.permissions, { actions: 'write', contents: 'read' })) fail('a job may hold only actions: write and contents: read');
    const steps = Array.isArray(job?.steps) ? job.steps : [];
    if (job?.if !== RERUN_STARVED_IF) fail(`the job's if must be exactly: ${RERUN_STARVED_IF}`);
    if (/\|\|/.test(String(job?.if ?? ''))) fail("the job's if must not contain ||");
    const runStep = steps.find((step) => typeof step?.run === 'string' && step.run.includes('ci-rerun-starved.mjs'));
    if (!drSame(runStep?.env?.RUN_ID, RERUN_STARVED_ENV.RUN_ID) || !drSame(runStep?.env?.RUN_ATTEMPT, RERUN_STARVED_ENV.RUN_ATTEMPT) || !drSame(runStep?.env?.RUN_CONCLUSION, RERUN_STARVED_ENV.RUN_CONCLUSION)) {
      fail('the script step must pass RUN_ID, RUN_ATTEMPT and RUN_CONCLUSION from github.event.workflow_run through env');
    }
    const checkouts = steps.filter((step) => typeof step?.uses === 'string' && step.uses.startsWith('actions/checkout@'));
    if (checkouts.length !== 1 || checkouts[0].with?.ref !== '${{ github.event.repository.default_branch }}') fail('must check out exactly once, with ref: ${{ github.event.repository.default_branch }}');
    for (const step of steps) {
      if (typeof step?.run === 'string' && step.run.includes('${{')) fail('an expression is interpolated inline in a run command; pass it through env');
    }
  }
  if (/workflow_run\.(head_sha|head_branch|head_repository|pull_requests)/.test(text)) fail("the triggering run's head is never used");
  if (/\bsecrets\b/.test(text.replace(/^\s*#.*$/gm, ''))) fail('uses no secrets beyond GITHUB_TOKEN');
  return problems;
}

/**
 * `npm run ci:local` derives its jobs from server.yml and desktop-ci.yml (scripts/ci-local-plan.mjs): a job, step,
 * action, service, `if:` or expression it cannot map or explicitly skip fails here, so the local mirror cannot fall
 * behind either workflow.
 */
export function checkLocalMirror(root) {
  return localMirrorProblems(Object.fromEntries(Object.entries(WORKFLOWS).map(([workflow, { file }]) => {
    try { return [workflow, readFileSync(join(root, '.github', 'workflows', file), 'utf8')]; } catch { return [workflow, null]; }
  })));
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const root = process.argv[2] ?? process.cwd();
  const problems = [...checkServerWorkflow(root), ...checkDesktopCiWorkflow(root), ...checkMobileCiWorkflow(root), ...checkReleaseWorkflow(root), ...checkMobileReleaseWorkflow(root), ...checkDesktopWorkflow(root), ...checkDesktopReleaseWorkflow(root), ...checkRerunStarvedWorkflow(root), ...checkLocalMirror(root)];
  for (const p of problems) console.error(p);
  process.exit(problems.length === 0 ? 0 : 1);
}
