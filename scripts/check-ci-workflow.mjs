import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { AREAS, gateExpression, JOB_GATES, jobRuns, reachableOutputs, WORKFLOWS } from './ci-areas.mjs';
import { localMirrorProblems } from './ci-local-plan.mjs';
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
    // Build the bundled client once for the shared encrypted-core integration (BROWSER_WEB_JOBS below).
    'web-build',
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

/** Shared core-worker/OPFS integration runs in Chromium. */
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
    'cargo clippy --workspace --exclude dilla-desktop --exclude dilla-media --exclude dilla-desktop-ui --exclude dilla-parity --exclude dilla-client --all-targets --all-features --locked -- -D warnings',
    'cargo test --workspace --exclude dilla-desktop --exclude dilla-media --exclude dilla-desktop-ui --exclude dilla-parity --exclude dilla-client --all-features --locked',
    'cargo build -p dilla-testkit --release --locked',
    // The native release binaries must run on the consumers' ubuntu-24.04 (glibc 2.39).
    'bash scripts/check-glibc-max.sh 2.39 target/release/dilla-bot-core target/release/dilla-testkit',
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

/** The retried module download every Go job runs right after its Go (and, where present, Node) setup. */
const GO_MOD_DOWNLOAD_NAME = 'name: Download Go modules (retried only on known network errors)';
const GO_MOD_DOWNLOAD_RUN = "node scripts/retry-on.mjs --pattern 'proxy\\.golang\\.org[^\\n]*:\\s(50[0-4]|429)\\b|stream error|INTERNAL_ERROR|TLS handshake timeout|connection reset|i/o timeout' -- go mod download";

/** Playwright's `install --with-deps` knows the packages of the Ubuntu releases it supports only; ubuntu-latest moved to
 *  26.04, where it fails (CI run 20). Every job that runs it is pinned to ubuntu-24.04. */
export function checkPlaywrightImage(jobs, problems, file) {
  for (const [name, body] of Object.entries(jobs)) {
    if (!jobSteps(body).some((step) => step.some((line) => /\binstall --with-deps\b/.test(line) && /playwright/.test(line)))) continue;
    if (!/^ {4}runs-on: ubuntu-24\.04$/m.test(body)) problems.push(`${file}: job "${name}" runs playwright install --with-deps and must run on ubuntu-24.04 (no package list for 26.04)`);
  }
}

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
  checkGoModuleDownloads(jobs, problems, file);
  checkPlaywrightImage(jobs, problems, file);

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
/** Structural equality of two parsed YAML values. */
const drSame = (a, b) => JSON.stringify(a) === JSON.stringify(b);

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

/**
 * desktop-gpui.yml (the GPUI desktop client, on three OSes) is pinned where a silent edit would hide a gap: the token
 * freshness test, the step that builds the Go test host and the WASI core and exports them to every later step, and the
 * `--ignored` runs on Linux (the parity render and dilla-client's real-dillad tests, which a plain `cargo test` skips).
 * Without them the real-server tests would be ignored everywhere and nothing would say so.
 */
export const GPUI_PINNED_STEPS = {
  tokens: '- run: node --test scripts/gen-gpui-tokens.test.mjs',
  // The generated strings.rs matches packages/web/src/strings/*.ts (scripts/gen-gpui-strings.test.mjs, Task 6).
  strings: '- run: node --test scripts/gen-gpui-strings.test.mjs',
  // Every e2e test title has a row in clients/desktop/BEHAVIOUR-GATES.md (scripts/behaviour-gates.test.mjs, Task 28).
  behaviourGates: '- run: node --test scripts/behaviour-gates.test.mjs',
  // dilla-client's route-table test runs tests/support/dump-routes.mjs, which imports the typescript package.
  npm: ['- uses: actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1 # v7', '  with:', '    node-version-file: .nvmrc', '    cache: npm', "# dilla-client's route-table test runs scripts that import the typescript package.", '- run: npm ci'].join('\n'),
  build: [
    '- name: Build the test host and the WASI core',
    '  shell: bash',
    '  run: |',
    '    exe=""',
    '    if [ "$RUNNER_OS" = "Windows" ]; then exe=".exe"; fi',
    '    cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked',
    '    go build -o "target/dilla-testhost$exe" ./cmd/dilla-testhost',
    '    {',
    '      echo "DILLA_TESTHOST=$GITHUB_WORKSPACE/target/dilla-testhost$exe"',
    '      echo "DILLA_CORE_WASI=$GITHUB_WORKSPACE/target/wasm32-wasip1/release/dilla_core_wasi.wasm"',
    '    } >> "$GITHUB_ENV"',
  ].join('\n'),
  // dilla's real Bot for the Bot tests (Task 28: dilla-testhost -bot, clients/desktop/dilla-client/src/testing/host/bot.rs).
  bot: [
    '- name: Build the Bot for the Bot tests (Linux)',
    "  if: runner.os == 'Linux' && !cancelled()",
    '  shell: bash',
    '  run: |',
    '    go build -tags dilla_testhost -o target/dillabot ./cmd/dillabot',
    '    cargo build -p dilla-bot-core --release --locked',
  ].join('\n'),
  // The real-server tests run in the gpui-linux job's four shards (client, Bot, desktop, media), one step each.
  ignored: [
    '- name: Run the ignored tests (Linux, client)',
    "  if: matrix.shard == 'client' && !cancelled()",
    '  shell: bash',
    '  run: |',
    // The null sound card (callPrereqs) for these steps only: Chromium's AudioContext in the media step never renders on it.
    '    export ALSA_CONFIG_PATH="$RUNNER_TEMP/asound-null.conf"',
    '    cargo test -p dilla-client --test gateway --locked -- --ignored',
    '    cargo test -p dilla-client --test sync --locked -- --ignored',
    '    cargo test -p dilla-client --test http_sign_in --locked -- --ignored',
    '    cargo test -p dilla-client --test calls --locked -- --ignored',
    '    cargo test -p dilla-client --test removals --test keypackages --test backups --test lifecycle --test readmit --test send_after_new_device --locked -- --ignored',
    '    cargo test -p dilla-client --test notify --locked -- --ignored',
    '    cargo build -p dilla-testkit --bin dilla-testkit --locked',
    '    cargo test -p dilla-client --test dms --locked -- --ignored',
    '    cargo test -p dilla-client --test voice --locked -- --ignored',
    '    cargo test -p dilla-client --test ring --locked -- --ignored',
  ].join('\n'),
  ignoredBot: [
    '- name: Run the ignored tests (Linux, Bot)',
    "  if: matrix.shard == 'bot' && !cancelled()",
    '  shell: bash',
    '  run: |',
    '    export ALSA_CONFIG_PATH="$RUNNER_TEMP/asound-null.conf"',
    '    cargo test -p dilla-client --test attachments --test bot --test bot_admin --test diagnostics --test factors --test members --test messages --test onboarding --test reactions --test search --test server_settings --test threads --test user_settings --test voice_settings --locked -- --ignored --skip enter_to_first_answer_is_within_rtt_plus_100ms_at_p95',
    "    # G2 is a latency budget: measured alone, not beside the other Bot tests' instances on the same four cores.",
    '    cargo test -p dilla-client --test bot --locked -- --ignored --exact enter_to_first_answer_is_within_rtt_plus_100ms_at_p95',
    '    cargo clippy -p dilla-desktop --all-targets --features automation --locked -- -D warnings',
    '    cargo test -p dilla-desktop --features automation --locked',
    '    cargo test -p dilla-desktop --features automation --test a11y --test bot --test bot_admin --test members --test search --test server_settings --test settings_host --test shell --test sign_in --test user_settings --test voice_settings --locked -- --ignored --skip parity_',
  ].join('\n'),
  ignoredDesktop: [
    '- name: Run the ignored tests (Linux, desktop)',
    "  if: matrix.shard == 'desktop' && !cancelled()",
    '  shell: bash',
    '  run: |',
    '    export ALSA_CONFIG_PATH="$RUNNER_TEMP/asound-null.conf"',
    '    cargo test -p dilla-parity --locked -- --ignored',
    '    cargo test -p dilla-desktop --test attachments --test diagnostics --test factors --test instances --test members --test messages --test notify --test onboarding --test reactions --test search --test server_settings --test shell --test threads --test user_settings --test voice --test voice_settings --locked -- --ignored --skip parity_',
    '    cargo test -p dilla-desktop --test integration --locked -- --ignored',
    '    cargo test -p dilla-desktop --test slice --locked -- --ignored',
  ].join('\n'),
  // The media tests need the e2e media suite's prerequisites, which the steps before this one build on Linux.
  mediaWasm: [
    '- name: Build packages/media\'s wasm core (Linux)',
    "  if: runner.os == 'Linux' && !cancelled()",
    '  env:',
    '    CC_wasm32_unknown_unknown: /usr/bin/clang-23',
    '    AR_wasm32_unknown_unknown: /usr/bin/llvm-ar-23',
    '  run: |',
    '    bash scripts/install-ci-tool.sh wasm-bindgen',
    '    bash scripts/install-ci-tool.sh wasm-pack',
    '    bash scripts/install-ci-tool.sh binaryen',
    // install-ci-tool.sh adds its directory to GITHUB_PATH, which only the later steps see.
    '    export PATH="$RUNNER_TEMP/dilla-ci-tools:$PATH"',
    '    wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/media/wasm',
  ].join('\n'),
  // The runner has no sound card (a call's media opens the default speaker): a null one for the ignored step. The
  // media tests' web peer reads the git-ignored WASI core from internal/mlswasi/testdata.
  callPrereqs: [
    '- name: A null sound card and the WASI core for the call tests (Linux)',
    "  if: runner.os == 'Linux' && !cancelled()",
    '  shell: bash',
    '  run: |',
    "    printf 'pcm.!default {\\n  type null\\n}\\n' > \"$RUNNER_TEMP/asound-null.conf\"",
    '    mkdir -p internal/mlswasi/testdata',
    '    cp target/wasm32-wasip1/release/dilla_core_wasi.wasm internal/mlswasi/testdata/',
  ].join('\n'),
  chromium: [
    '- name: Playwright Chromium (Linux)',
    "  if: runner.os == 'Linux' && !cancelled()",
    '  timeout-minutes: 8',
    '  run: npx playwright install --with-deps chromium',
  ].join('\n'),
  media: [
    '- name: Run the media call tests (Linux)',
    "  if: matrix.shard == 'media' && !cancelled()",
    '  shell: bash',
    '  run: |',
    '    cargo test -p dilla-media --test live --test call --test e2e_call --locked -- --ignored --skip bot_play_from_enter_to_first_audio_frame_meets_g3 --skip bot_next_queued_track_gap_is_under_150_ms --skip bot_repeated_track_starts_within_500_ms_at_p95',
    '    # The Bot music timing tests (G3) are latency budgets: measured one at a time, not beside a Bot restart or a browser call.',
    '    cargo test -p dilla-media --test call --locked -- --ignored --exact --test-threads=1 bot_play_from_enter_to_first_audio_frame_meets_g3 bot_next_queued_track_gap_is_under_150_ms bot_repeated_track_starts_within_500_ms_at_p95',
  ].join('\n'),
};

/** The job(s) each GPUI_PINNED_STEPS entry must be in; an entry not named here belongs to the gpui-linux job. */
const GPUI_PIN_JOBS = { tokens: ['gpui'], strings: ['gpui'], behaviourGates: ['gpui'], npm: ['gpui'], build: ['gpui', 'gpui-linux'] };

/** The GPUI crates' integration-test directories (relative to the repository root) and the `cargo test -p` crate each one's `--ignored` run names. */
const GPUI_IGNORED_TEST_DIRS = [['clients/desktop/dilla-client/tests', 'dilla-client'], ['clients/desktop/dilla-desktop/tests', 'dilla-desktop'], ['clients/desktop/dilla-media/tests', 'dilla-media'], ['clients/desktop/dilla-desktop-ui/tests', 'dilla-desktop-ui'], ['clients/desktop/parity/tests', 'dilla-parity']];
/** Test files whose #[ignore] needs something CI cannot give (the real OS keychain, a developer's desktop session). */
const GPUI_IGNORED_BY_HAND = new Set(['clients/desktop/dilla-client/tests/store.rs']);
/**
 * Parity tests parked by controller ruling R22 until the shared-component pass (`#[ignore = "parity after T6c: ..."]`, each carrying
 * its measured miss). They are known to fail, so no `--ignored` run may pick them up; any other ignore in the same file still counts.
 * Temporary: the parity sweep removes every such attribute, and this exemption with them.
 */
const GPUI_PARKED_PARITY = /#\[\s*ignore\s*=\s*"parity after T6c[^"]*"\s*\]/g;
/** An ignore that applies only off Linux: the plain Linux run already runs those tests, so no `--ignored` line is owed for it. */
const GPUI_OFF_LINUX_IGNORE = /#\[\s*cfg_attr\(\s*not\(\s*target_os\s*=\s*"linux"\s*\)\s*,\s*ignore\b[^\]]*\]/g;

/** A test file's source with the ignores that need no Linux `--ignored` run removed (see the two patterns above). */
function liveIgnoreSource(source) {
  let s = source.replace(GPUI_PARKED_PARITY, '').replace(GPUI_OFF_LINUX_IGNORE, '');
  // A macro that forwards its reason (`#[ignore = $why]`, or `#[cfg_attr(target_os = "linux", ignore = $why)]` beside an
  // off-Linux ignore) is parked only when every reason literal passed to it is the R22 one.
  const forwarded = /#\[\s*(?:cfg_attr\(\s*target_os\s*=\s*"linux"\s*,\s*ignore\s*=\s*\$\w+\s*\)|ignore\s*=\s*\$\w+)\s*\]/g;
  if (forwarded.test(s)) {
    const reasons = [...s.matchAll(/;\s*"([^"]*)"/g)].map((m) => m[1]);
    if (reasons.length > 0 && reasons.every((r) => r.startsWith('parity after T6c'))) s = s.replace(forwarded, '');
  }
  return s;
}

/**
 * The `cargo test ... --ignored` command lines of the gpui job's Linux steps and of the gpui-linux job's (Linux only, one
 * step per shard) steps: run blocks, comment lines dropped, wrapped lines joined.
 */
function gpuiIgnoredRunLines(text) {
  let doc;
  try { doc = yaml.load(text); } catch { return []; }
  const stepsOf = (job) => (Array.isArray(doc?.jobs?.[job]?.steps) ? doc.jobs[job].steps : []);
  const steps = [...stepsOf('gpui').map((step) => [step, true]), ...stepsOf('gpui-linux').map((step) => [step, false])];
  const lines = [];
  for (const [step, needsLinuxGuard] of steps) {
    const guard = String(step?.if ?? '');
    if (typeof step?.run !== 'string' || (needsLinuxGuard && !guard.includes("runner.os == 'Linux'")) || /\bfalse\b/.test(guard)) continue;
    const body = step.run.split('\n').filter((l) => !/^\s*#/.test(l)).join('\n').replace(/\\\n/g, ' ');
    for (const l of body.split('\n')) if (l.includes('cargo test') && l.includes('--ignored')) lines.push(l);
  }
  return lines;
}

export function checkDesktopGpuiWorkflow(root) {
  const file = 'desktop-gpui.yml';
  let text;
  try { text = readFileSync(join(root, '.github', 'workflows', file), 'utf8'); } catch { return [`${file}: missing`]; }
  const problems = [];
  // Compare each pinned step with the steps of the job it belongs to (GPUI_PIN_JOBS), indentation removed: the gpui job
  // (three OSes) runs the freshness tests, the gpui-linux job's shards the real-server tests, and both build the test host.
  const split = text.indexOf('\n  gpui-linux:\n');
  const jobText = { gpui: split < 0 ? text : text.slice(0, split), 'gpui-linux': split < 0 ? '' : text.slice(split) };
  const unindent = (t) => t.split('\n').map((l) => l.replace(/^ {6}/, '')).join('\n');
  for (const [name, step] of Object.entries(GPUI_PINNED_STEPS)) {
    for (const job of GPUI_PIN_JOBS[name] ?? ['gpui-linux']) {
      if (!unindent(jobText[job]).includes(step)) problems.push(`${file}: the ${name} step must be exactly (job ${job}):\n${step}`);
    }
  }
  // Every integration-test file with an ignored test is run by a `cargo test -p <crate> [--test <file>] ... --ignored` line in the Linux
  // ignored or media step's run block (comments stripped, so a commented-out line or a disabled step does not count), so a suite a
  // Wave 1 task adds cannot be ignored everywhere without this check saying so. The controller appends the line as each test file merges.
  const runLines = gpuiIgnoredRunLines(text);
  for (const [dir, crate] of GPUI_IGNORED_TEST_DIRS) {
    const entries = [];
    try {
      for (const e of readdirSync(join(root, dir), { withFileTypes: true })) {
        if (e.isFile() && e.name.endsWith('.rs')) entries.push([e.name.slice(0, -3), `${dir}/${e.name}`]);
        else if (e.isDirectory()) entries.push([e.name, `${dir}/${e.name}/main.rs`]); // tests/<name>/main.rs is the test target <name>
      }
    } catch { continue; }
    for (const [name, rel] of entries) {
      let source;
      try { source = liveIgnoreSource(readFileSync(join(root, rel), 'utf8')); } catch { continue; }
      if (GPUI_IGNORED_BY_HAND.has(rel) || !/#\[\s*(?:ignore\b|cfg_attr\([^\]]*\bignore\b)/.test(source)) continue;
      const forCrate = runLines.filter((l) => l.includes(`cargo test -p ${crate} `));
      // A run with no --test filter runs every ignored test of the crate.
      const ran = forCrate.some((l) => !/--test\s/.test(l)) || forCrate.some((l) => new RegExp(`--test ${name}(\\s|$)`).test(l));
      if (!ran) problems.push(`${file}: ${rel} has ignored tests but no \`cargo test -p ${crate} --test ${name} ... -- --ignored\` line in a Linux ignored step runs them`);
    }
  }
  // The Go and WASI build runs before the cargo test run that reads DILLA_TESTHOST, so the real-server tests find it.
  if (text.indexOf('Build the test host and the WASI core') > text.indexOf('cargo test $GPUI_CRATES')) problems.push(`${file}: the test host must be built before the cargo test run`);
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
  const problems = [...checkServerWorkflow(root), ...checkDesktopCiWorkflow(root), ...checkMobileCiWorkflow(root), ...checkReleaseWorkflow(root), ...checkMobileReleaseWorkflow(root), ...checkRerunStarvedWorkflow(root), ...checkDesktopGpuiWorkflow(root), ...checkLocalMirror(root)];
  for (const p of problems) console.error(p);
  process.exit(problems.length === 0 ? 0 : 1);
}
