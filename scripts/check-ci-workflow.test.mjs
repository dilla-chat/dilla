import { after, test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { spawnSync } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
// Every fixture lives under one per-process root that is removed on exit, so test runs never pile up in the temp dir.
const TEST_ROOT = mkdtempSync(join(tmpdir(), 'dilla-ci-run-'));
process.on('exit', () => rmSync(TEST_ROOT, { recursive: true, force: true }));
import { gateExpression, JOB_GATES, WORKFLOWS } from './ci-areas.mjs';
import { checkDesktopGpuiSignWorkflow, GPUI_SIGN_STEPS, checkDesktopGpuiReleaseWorkflow, GPUI_RELEASE_STEPS, aptPackages, GPUI_MAC_PACKAGE_RUNS, checkDesktopGpuiWorkflow, GPUI_PINNED_STEPS, checkRerunStarvedWorkflow, assertGoShards,checkGoModuleDownloads, checkPlaywrightImage, BROWSER_WEB_JOBS, browserSuiteCommand, checkDesktopCiWorkflow, checkMobileCiWorkflow, checkReleaseWorkflow, checkMobileReleaseWorkflow, checkServerWorkflow, REQUIRED_JOBS, goShardCommand, goShardMatrix, jobSteps, NIGHTLY_CRON, MIN_BROWSER_WEB_TIMEOUT, NON_RACE_STEP, raceGatedTests, RELEASE_ASSETS, WASM_CLANG_BLOCK, WASM_CLANG_ENV_BLOCK, WASM_CLANG_STEPS, WASM_LLVM_KEY_FPR, WASM_LLVM_VERSION } from './check-ci-workflow.mjs';
import { POSTGRES_API_RUN, SETS as GO_SHARD_SETS } from './go-shard.mjs';

const NPM_TEST = "      - if: needs.changes.outputs.web != 'false'\n        run: npm test\n";
const SCRIPT_PATH = fileURLToPath(new URL('./check-ci-workflow.mjs', import.meta.url));
const REPO_ROOT = join(dirname(SCRIPT_PATH), '..');
const REAL_SERVER = readFileSync(join(REPO_ROOT, '.github', 'workflows', 'server.yml'), 'utf8');
const REAL_DESKTOP_CI = readFileSync(join(REPO_ROOT, '.github', 'workflows', 'desktop-ci.yml'), 'utf8');
// mobile.yml is not part of the server/desktop fixture text: fixture() writes the real file beside them (ci-local's
// mirror check reads every CI workflow), and the mobile tests below edit it on their own.
const REAL_MOBILE = readFileSync(join(REPO_ROOT, '.github', 'workflows', 'mobile.yml'), 'utf8');
// A fixture holds both CI workflows in one text, server.yml first, split at this line (fixture() writes them apart): a
// test edits either file with one replace. A job both carry is edited in server.yml, its first occurrence.
const SPLIT = '\n# ---- desktop-ci.yml ----\n';
const REAL_CI = REAL_SERVER + SPLIT + REAL_DESKTOP_CI;
const checkWorkflow = (root) => [...checkServerWorkflow(root), ...checkDesktopCiWorkflow(root)];
// The file that reports on a job a fixture edits (its first occurrence).
const fileOfJob = (job) => (REQUIRED_JOBS.server.includes(job) ? 'server.yml' : 'desktop-ci.yml');

const REAL_RELEASE = readFileSync(join(REPO_ROOT, '.github', 'workflows', 'release.yml'), 'utf8');

function addE2ePackage(root) {
  mkdirSync(join(root, 'e2e'), { recursive: true });
  writeFileSync(join(root, 'e2e/package.json'), readFileSync(join(REPO_ROOT, 'e2e/package.json')));
}

test('desktop-only CI keeps shared core security and removes unsupported browser client lanes', () => {
  assert.doesNotMatch(REAL_CI, /^  (?:firefox-[^:]+|webkit-[^:]+|chromium-(?:web-[123]|voice|core)):/m);
  assert.match(REAL_CI, /^  core-integration:/m);
  const scripts = JSON.parse(readFileSync(join(REPO_ROOT, 'e2e/package.json'), 'utf8')).scripts;
  for (const name of ['test:e2e:matrix', 'test:e2e:media', 'test:e2e:spike', 'test:e2e:core']) assert.doesNotMatch(scripts[name], /firefox|webkit/);
});

test('CI refuses reinstalling an unsupported compatibility browser in shared harness jobs', () => {
  const changed = GOOD.replace('npx playwright install --with-deps chromium', 'npx playwright install --with-deps chromium firefox');
  assert.notEqual(changed, GOOD);
  assert.ok(checkWorkflow(fixture(changed)).some(problem => problem.includes('unsupported browser engine')));
});

test('CI rejects removing the whole-run watcher or its completion barrier', () => {
  for (const name of ['ci-watchdog', 'server-work-complete', 'desktop-work-complete']) {
    const changed = REAL_CI.replace(new RegExp(`^  ${name}:\\n[\\s\\S]*?(?=^  [a-z][\\w-]*:|$(?![\\s\\S]))`, 'm'), '');
    assert.notEqual(changed, REAL_CI);
    const problems = checkWorkflow(fixture(changed));
    assert.ok(problems.some(problem => problem.includes(`missing job "${name}"`)), problems.join('\n'));
  }
});

test('CI rejects an unsafe, delayed or unbounded whole-run watcher', () => {
  for (const [before, after] of [
    ["github.event.pull_request.head.repo.full_name == github.repository)", 'true)'],
    ['timeout-minutes: 90', 'timeout-minutes: 360'],
    ['persist-credentials: false', 'persist-credentials: true'],
    ['actions: write', 'actions: read'],
    ['actions: write', 'actions: write\n      contents: write'],
    ['  ci-watchdog:\n', '  ci-watchdog:\n    name: renamed-watcher\n'],
    ['persist-credentials: false', 'persist-credentials: false\n          repository: untrusted/fork'],
    ['GH_TOKEN: ${{ github.token }}', 'GH_TOKEN: ${{ secrets.PUBLISH_TOKEN }}'],
    ['  ci-watchdog:\n', '  ci-watchdog:\n    needs: [changes]\n'],
    ['run: python3 scripts/ci-watchdog.py', 'run: echo done'],
    // Each workflow's own barrier: server's without docs would finish before its docs job, desktop's has no docs.
    ['CI_COMPLETION_JOBS: changes docs server-work-complete', 'CI_COMPLETION_JOBS: changes server-work-complete'],
    ['CI_COMPLETION_JOBS: changes desktop-work-complete', 'CI_COMPLETION_JOBS: changes docs desktop-work-complete'],
    ['CI_COMPLETION_JOBS: changes desktop-work-complete', 'CI_COMPLETION_JOBS: changes server-work-complete'],
    ['          CI_COMPLETION_JOBS: changes docs server-work-complete\n', ''],
  ]) {
    const changed = REAL_CI.replace(before, after);
    assert.notEqual(changed, REAL_CI, before);
    assert.ok(checkWorkflow(fixture(changed)).some(problem => problem.includes('unsafe watchdog')), before);
  }
});

test('CI rejects a completion barrier that misses jobs or waits for the watcher', () => {
  for (const changed of [
    REAL_CI.replace(/(  server-work-complete:\n    needs: \[)changes, /, '$1'),
    REAL_CI.replace(/(  server-work-complete:\n    needs: \[)/, '$1ci-watchdog, '),
    REAL_CI.replace(/(  server-work-complete:\n    needs: \[[^\]]*\]\n)    if: always\(\)\n/, '$1'),
    REAL_CI.replace(/(  desktop-work-complete:\n    needs: \[)changes, /, '$1'),
    // A job of the other workflow is no job of this run: waiting for it never ends.
    REAL_CI.replace(/(  desktop-work-complete:\n    needs: \[)/, '$1go-lint, '),
  ]) {
    assert.notEqual(changed, REAL_CI);
    assert.ok(checkWorkflow(fixture(changed)).some(problem => problem.includes('completion barrier')));
  }
});

test('CI accepts a skipped watcher only for fork pull requests', () => {
  const step = "      - if: needs.ci-watchdog.result == 'skipped' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)\n        run: exit 1\n";
  for (const [file, check] of [['server.yml', checkServerWorkflow], ['desktop-ci.yml', checkDesktopCiWorkflow]]) {
    const [server, desktop] = REAL_CI.split(SPLIT);
    const changed = file === 'server.yml' ? server.replace(step, '') + SPLIT + desktop : server + SPLIT + desktop.replace(step, '');
    assert.notEqual(changed, REAL_CI);
    assert.ok(check(fixture(changed)).some(problem => problem.includes('skipped watcher') && problem.startsWith(file)), file);
  }
});


test('compiled helper recovery tests receive the same-commit helper and cannot silently skip', () => {
  const native = REAL_CI.split('\n  rust-native:\n')[1].split('\n  rust-wasi:\n')[0];
  const unit = REAL_CI.split('\n  go:\n')[1].split('\n  go-native:\n')[0];
  const go = REAL_CI.split('\n  go-native:\n')[1]?.split('\n  go-ds:\n')[0] ?? '';
  assert.ok(go, 'native integrations need a separate bounded job instead of consuming the unit job budget');
  assert.match(go, /timeout-minutes: 15/);
  assert.match(unit, /timeout-minutes: 30/);
  assert.doesNotMatch(unit, /DILLA_BOT_HELPER_INTEGRATION/);
  assert.match(native, /cargo build -p dilla-bot-core --release --locked/);
  assert.match(native, /name: dilla-bot-core\n\s+path: target\/release\/dilla-bot-core/);
  assert.match(go, /needs:.*rust-native/);
  assert.match(go, /name: dilla-bot-core\n\s+path: target\/release/);
  const run = jobSteps(go).find(step => step.some(line => line.includes('go test -race ./internal/bot/native ./cmd/dillabot')));
  assert.ok(run, 'the native adapter and production dependency tests must run with the compiled helper');
  assert.ok(run.some(line => line.includes("DILLA_BOT_HELPER_INTEGRATION: '1'")));
  assert.ok(run.some(line => line.includes("DILLA_CONTROL_E2E: '1'")));
  assert.ok(run.some(line => line.includes('DILLA_BOT_HELPER_BIN: ${{ github.workspace }}/target/release/dilla-bot-core')));
  assert.ok(run.some(line => line.includes('./internal/bot/music/persistence')), 'encrypted queue upgrades must run against the compiled helper');
  const steps = jobSteps(go);
  const fixtureBuild = steps.findIndex(step => step.some(line => line.includes('cargo test -p dilla-core --test native_bot_group_peer_fixture --no-run --locked')));
  const integration = steps.findIndex(step => step.some(line => line.includes('go test -race ./internal/bot/native ./cmd/dillabot')));
  const npmInstall = steps.findIndex(step => step.some(line => line.includes('run: npm ci')));
  assert.ok(npmInstall >= 0 && npmInstall < integration, 'install esbuild before opting into the real client control tests');
  const botRelease = REAL_RELEASE.split('\n  bot-release:\n')[1].split(/^  \S/m)[0];
  assert.doesNotMatch(botRelease, /DILLA_CONTROL_E2E|run: npm ci/);
  assert.ok(fixtureBuild >= 0 && fixtureBuild < integration, 'compile the Rust peer before the integration test starts its bounded readiness wait');
});

test('workflow steps execute only commit-pinned GitHub actions', () => {
  for (const workflow of [REAL_CI, REAL_RELEASE]) {
    for (const match of workflow.matchAll(/^\s*-?\s*uses:\s*(\S+)/gm)) {
      assert.match(match[1], /^actions\/[\w-]+(?:\/[\w-]+)?@[0-9a-f]{40}$/);
    }
  }
});

test('clang 23 build jobs stay on the Ubuntu 24.04 image they install packages for', () => {
  for (const job of ['rust-wasm-node', 'browser-spike', 'browser-media', 'web-build']) {
    assert.match(REAL_CI, new RegExp(`^  ${job}:\\n(?:    (?:name|needs|if):.*\\n)*    runs-on: ubuntu-24\\.04$`, 'm'));
  }
  assert.match(REAL_RELEASE, /^  web-build:\n    runs-on: ubuntu-24\.04$/m);
});

test('the CI checker rejects a third-party action even when pinned', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(
    'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7',
    'taiki-e/install-action@1168f6810a3cb30a3514bf5756a3b7f0422c6df8 # v2',
  )));
  assert.ok(problems.some((problem) => problem.includes('GitHub-owned action')), problems.join('\n'));
});

test('the CI checker rejects a movable GitHub action tag', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(
    'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7',
    'actions/checkout@v7',
  )));
  assert.ok(problems.some((problem) => problem.includes('40-hex commit')), problems.join('\n'));
});

test('the CI checker rejects a flow-style action step', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(
    NPM_TEST,
    '      - { name: unsafe, uses: taiki-e/install-action@1168f6810a3cb30a3514bf5756a3b7f0422c6df8 }\n' + NPM_TEST,
  )));
  assert.ok(problems.some((problem) => problem.includes('flow-style action')), problems.join('\n'));
});

test('Rust CI and release setup use the pinned rustup toolchain without third-party helper actions', () => {
  const toolchain = readFileSync(join(REPO_ROOT, 'rust-toolchain.toml'), 'utf8');
  assert.match(toolchain, /channel = "1\.98\.1"/);
  for (const workflow of [REAL_CI, REAL_RELEASE]) {
    assert.doesNotMatch(workflow, /uses:\s*(?:dtolnay\/rust-toolchain|Swatinem\/rust-cache)@/);
    assert.match(workflow, /- run: rustup show active-toolchain/);
  }
});

// The go job's test step: a shard of every package but internal/ds and internal/api, which go-ds and go-api run.
const GO_TEST = goShardCommand('go');

/** The browser jobs of GOOD, generated from BROWSER_WEB_JOBS: one job per engine, suite and web shard. */
function browserJobsFixture() {
  return Object.entries(BROWSER_WEB_JOBS).map(([job, spec]) => `  ${job}:
    runs-on: ubuntu-24.04
    timeout-minutes: ${spec.timeout}
    needs: [rust-native, rust-wasi, web-build]
    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-testkit
          path: artifacts
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-web-dist
          path: packages/web/dist
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-harness
          path: packages/client-core/harness-dist
      - run: npx playwright install --with-deps ${spec.engine}
${spec.engine === 'firefox' && spec.suite !== 'core' ? '      - run: sh scripts/ci-audio-server.sh\n' : ''}      - run: ${browserSuiteCommand(spec)}
        env:
          DILLA_TESTKIT: \${{ github.workspace }}/artifacts/dilla-testkit
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        if: failure() && hashFiles('e2e/test-results/**') != ''
        with:
          name: browser-web-results-${job}
          path: e2e/test-results
          if-no-files-found: error
`).join('\n');
}

const GOOD_BASE = `name: ci-fixture
on:
  push:
    branches: [main]
  pull_request:
permissions:
  contents: read
concurrency:
  group: \${{ github.workflow }}-\${{ github.event_name }}-\${{ github.ref }}
  cancel-in-progress: true
env:
  CARGO_TERM_COLOR: always
jobs:
  node:
    runs-on: ubuntu-latest
    steps:
      - if: needs.changes.outputs.web != 'false'
        run: npm test
        env:
          DILLA_UCD_DIR: \${{ runner.temp }}/ucd
      - name: vectors are up to date
        if: needs.changes.outputs.web != 'false'
        run: npm run vectors
      - run: npm run test:css-check
      - run: npm run check:css
      - run: npm run check:desktop-auth
      - run: npm run test:ci-check
      - run: npm run test:wasm-size-check
      - run: npm run test:manifest-writer
      - uses: actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        if: needs.changes.outputs.web != 'false' || needs.changes.outputs.text != 'false'
        with:
          path: \${{ runner.temp }}/ucd
          key: ucd-\${{ hashFiles('scripts/gen-profile-text-tables.mjs') }}
      - name: profile-text tables are current
        if: needs.changes.outputs.text != 'false'
        run: node scripts/gen-profile-text-tables.mjs --download --check "$RUNNER_TEMP/ucd"
  server-scripts:
    runs-on: ubuntu-latest
    steps:
      - run: npm ci
      - run: npm run test:bot-ops-check
      - run: npm run check:bot-ops
      - run: npm run test:install
      - run: bash deploy/install_bot_test.sh
  ui:
    runs-on: ubuntu-latest
    steps:
      - run: npm test -w packages/ui
      - run: node packages/ui/scripts/shoot-stories.mjs 6016 target/ui-screens/web-2b form- ceremony- shell- conversation- settings- screens- primitives-channelrow-- conversation-messagetoolbar-- conversation-attachmentcard-- screens-conversation--
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: ui-screens
          path: target/ui-screens/web-2b
          if-no-files-found: error
      - run: node packages/ui/scripts/shoot-stories.mjs 6027 target/ui-screens/web-2c/desktop trust-safetycompare-- trust-trustalert-- search-searchpalette-- status-typingindicator-- settings-diagnosticspanel-- shell-tabhand-over-- screens-trustandpolish--
      - run: node packages/ui/scripts/shoot-stories.mjs 6027 target/ui-screens/web-2c/phone trust-safetycompare-- trust-trustalert-- search-searchpalette-- status-typingindicator-- settings-diagnosticspanel-- shell-tabhand-over-- screens-trustandpolish-- --viewport=360x740
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: ui-screens-web-2c
          path: |
            target/ui-screens/web-2c/desktop/**
            target/ui-screens/web-2c/phone/**
          if-no-files-found: error
      - run: node packages/ui/scripts/shoot-stories.mjs 6038 target/ui-screens/web-3/desktop voice- media-mediaplayer-- screens-voice--
      - run: node packages/ui/scripts/shoot-stories.mjs 6038 target/ui-screens/web-3/phone voice- media-mediaplayer-- screens-voice-- --viewport=360x740
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: ui-screens-web-3
          path: target/ui-screens/web-3/**
          if-no-files-found: error
  rust-native:
    runs-on: ubuntu-latest
    steps:
      - if: needs.changes.outputs.core != 'false'
        run: cargo fmt --all --check
      - if: needs.changes.outputs.core != 'false'
        run: cargo clippy --workspace --exclude dilla-desktop --exclude dilla-media --exclude dilla-desktop-ui --exclude dilla-parity --exclude dilla-client --all-targets --all-features --locked -- -D warnings
      - if: needs.changes.outputs.core != 'false'
        run: |
          sudo prlimit --pid "$$" --memlock=268435456:268435456
          cargo test --workspace --exclude dilla-desktop --exclude dilla-media --exclude dilla-desktop-ui --exclude dilla-parity --exclude dilla-client --all-features --locked
      - run: cargo build -p dilla-testkit --release --locked
      - run: bash scripts/check-glibc-max.sh 2.39 target/release/dilla-bot-core target/release/dilla-testkit
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: dilla-testkit
          path: target/release/dilla-testkit
          if-no-files-found: error
  rust-wasm-node:
    runs-on: ubuntu-latest
    steps:
      - run: rustup show active-toolchain
      - run: bash scripts/install-ci-tool.sh wasm-bindgen
      - run: bash scripts/install-ci-tool.sh wasm-pack
      - run: cargo test -p dilla-core-wasm --target wasm32-unknown-unknown --locked --features vectors
      - run: cargo clippy -p dilla-core-wasm --target wasm32-unknown-unknown --lib --locked -- -D warnings
      - run: wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/media/wasm
      - run: wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg
      - run: node scripts/check-wasm-size.mjs packages/core-wasm/pkg/dilla_core_wasm_bg.wasm
      - run: npm ci
      - run: npm run typecheck -w @dilla/media
      - run: npm run typecheck -w @dilla/core-wasm
      - run: npm run test:wasm -w @dilla/media
      - run: npm run check:schema -w @dilla/media
  rust-wasi:
    runs-on: ubuntu-latest
    steps:
      - run: cargo build -p dilla-core-wasi --target wasm32-wasip1 --release --locked
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: dilla-core-wasi
          path: target/wasm32-wasip1/release/dilla_core_wasi.wasm
          if-no-files-found: error
  vectors:
    runs-on: ubuntu-latest
    steps:
      - run: npm run vectors
      - run: git diff --exit-code -- protocol/vectors
      - run: cargo run -p dilla-testkit --bin dilla-testkit --locked -- vectors
  browser-spike:
    runs-on: ubuntu-latest
    timeout-minutes: 20
    steps:
      - run: bash scripts/install-ci-tool.sh wasm-bindgen
      - run: bash scripts/install-ci-tool.sh wasm-pack
      - run: wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir spike/pkg
      - run: npm run test:e2e:matrix -w @dilla/e2e
  deny:
    runs-on: ubuntu-latest
    steps:
      - run: bash scripts/install-ci-tool.sh cargo-deny
      - run: cargo deny --all-features check advisories bans licenses sources
  go:
    name: go-\${{ matrix.shard }}
    runs-on: ubuntu-latest
    timeout-minutes: 30
    needs: [rust-wasi]
${goShardMatrix('go')}    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - run: ${GO_TEST}
        env:
          GORACE: halt_on_error=1

  go-checks:
    runs-on: ubuntu-latest
    timeout-minutes: 20
    needs: [rust-wasi]
    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - run: go mod verify
      - run: go vet ./...
      - run: CGO_ENABLED=0 go build -tags dillapins ./internal/deps
      - run: go test -race github.com/pion/turn/v5/internal/allocation -run '^TestAddPermissionPublishesInitializedTimer$' -count=20
      - run: ${NON_RACE_STEP}
      - run: CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' ./cmd/dillad

  browser-media:
    runs-on: ubuntu-24.04
    timeout-minutes: 25
    needs: [rust-native, rust-wasi]
    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-testkit
          path: artifacts
      - run: bash scripts/install-ci-tool.sh wasm-bindgen
      - run: bash scripts/install-ci-tool.sh wasm-pack
      - run: wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/media/wasm
      - run: go build -o target/dilla-mediabot ./cmd/dilla-mediabot
      - run: node packages/media/scripts/extract-rnnoise-wasm.mjs
      - run: npx playwright install --with-deps chromium
      - run: sh scripts/ci-audio-server.sh
      - run: npm run test:e2e:media -w @dilla/e2e
        env:
          DILLA_TESTKIT: \${{ github.workspace }}/artifacts/dilla-testkit
      - run: npx playwright test --config e2e/playwright.media.config.ts --project=chromium-media unsupported-sfu-codec.spec.ts
        env:
          DILLA_MEDIA_SFU_AV1: '1'
          DILLA_TESTKIT: \${{ github.workspace }}/artifacts/dilla-testkit
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        if: failure() && hashFiles('e2e/test-results/**') != ''
        with:
          name: browser-media-results
          path: e2e/test-results
          if-no-files-found: error

  web-build:
    runs-on: ubuntu-latest
    timeout-minutes: 15
    steps:
      - run: bash scripts/install-ci-tool.sh wasm-bindgen
      - run: bash scripts/install-ci-tool.sh wasm-pack
      - run: wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg
      - run: node scripts/check-wasm-size.mjs packages/core-wasm/pkg/dilla_core_wasm_bg.wasm
      - run: npm run typecheck -w @dilla/client-core
      - run: npm run typecheck -w @dilla/web
      - run: npm run typecheck:e2e
      - run: npm run build -w @dilla/web
      - run: npm run -w @dilla/web test:dist
      - run: npm run build:harness -w @dilla/client-core
      - run: go test -tags webdist ./internal/web/...
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: dilla-web-dist
          path: packages/web/dist/
          if-no-files-found: error
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: dilla-core-harness
          path: packages/client-core/harness-dist/
          if-no-files-found: error

${browserJobsFixture()}
  go-native:
    runs-on: ubuntu-latest
    timeout-minutes: 15
    needs: [rust-wasi, rust-native]
    steps:
      - run: cargo test -p dilla-core --test native_bot_group_peer_fixture --no-run --locked
      - run: go test -race ./internal/bot/native ./cmd/dillabot
        env:
          DILLA_BOT_HELPER_INTEGRATION: '1'
          DILLA_BOT_HELPER_BIN: \${{ github.workspace }}/target/release/dilla-bot-core

  go-ds:
    name: go-ds-\${{ matrix.shard }}
    runs-on: ubuntu-latest
    timeout-minutes: 50
    needs: [rust-wasi]
${goShardMatrix('go-ds')}    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - run: ${goShardCommand('go-ds')}

  go-api:
    name: go-api-\${{ matrix.shard }}
    runs-on: ubuntu-latest
    timeout-minutes: 50
    needs: [rust-wasi]
${goShardMatrix('go-api')}    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - run: ${goShardCommand('go-api')}

  go-harness:
    name: go-harness-\${{ matrix.shard }}
    runs-on: ubuntu-latest
    timeout-minutes: 35
    needs: [rust-wasi, rust-native]
${goShardMatrix('go-harness')}    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-testkit
          path: artifacts
      - run: ${goShardCommand('go-harness')}
        env:
          DILLA_TESTKIT: \${{ github.workspace }}/artifacts/dilla-testkit
          DILLA_TESTKIT_REQUIRED: '1'

  go-fts5-arm64:
    runs-on: ubuntu-24.04-arm
    steps:
      - run: go test ./internal/store/sqlite/...

  go-lint:
    runs-on: ubuntu-latest
    steps:
      - run: bash scripts/install-ci-tool.sh golangci-lint
      - run: golangci-lint run ./...

  go-vuln:
    runs-on: ubuntu-latest
    steps:
      - run: go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
      - run: go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 -scan package -tags dillapins ./internal/deps

  go-sqlc:
    runs-on: ubuntu-latest
    steps:
      - run: bash scripts/install-ci-tool.sh sqlc
      - run: sqlc diff
      - run: git diff --exit-code

  go-postgres:
    name: go-postgres-\${{ matrix.shard }}
    runs-on: ubuntu-latest
    needs: [rust-wasi]
${goShardMatrix('go-postgres')}    services:
      postgres:
        image: postgres:18.6-alpine3.24
        env:
          POSTGRES_INITDB_ARGS: --locale-provider=builtin --builtin-locale=C.UTF-8
    steps:
      - run: ${goShardCommand('go-postgres')}
        env:
          DILLA_TEST_PG: postgres://dilla:dilla@127.0.0.1:5432/dilla?sslmode=disable

  go-release:
    runs-on: ubuntu-latest
    needs: [web-build]
    steps:
      - run: rm -rf internal/web/dist
      - name: Fetch the web client built by the web-build job
        uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-web-dist
          path: internal/web/dist
      - run: |
          test -f internal/web/dist/index.html
          if grep -q 'content="placeholder"' internal/web/dist/index.html; then echo "internal/web/dist still holds the placeholder client"; exit 1; fi
      - name: the downloaded client passes the manifest check
        run: go test -tags webdist ./internal/web/...
        env:
          DILLA_WEB_DIST: \${{ github.workspace }}/internal/web/dist
      - run: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/dillad-linux-amd64 ./cmd/dillad
      - name: No dillad is a dilla_unsafe_test build
        run: |
          test "$(go version -m dist/dillad-linux-amd64 dist/dillad-linux-arm64 | grep -c 'build[[:space:]]CGO_ENABLED=0')" -eq 2
          if go version -m dist/dillad-linux-amd64 dist/dillad-linux-arm64 | grep -q dilla_unsafe_test; then echo "a dillad was built with the dilla_unsafe_test tag"; exit 1; fi
          if go version -m dist/dillad-linux-amd64 dist/dillad-linux-arm64 | grep -q dilla_testhost; then echo "a dillad was built with the dilla_testhost tag"; exit 1; fi
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: dillad-binaries
          path: dist/
          if-no-files-found: error

`;

// The four jobs that build for wasm32-unknown-unknown, as the CI workflows write them: the canonical clang 23 job env and,
// right after checkout, the canonical pinned install steps (ruling of 2026-10-13; hardened after the d3429ce lead).
const WASM_JOBS = ['rust-wasm-node', 'browser-spike', 'browser-media', 'web-build'];

function withWasmClang(text) {
  const out = [];
  let job = null;
  for (const line of text.split('\n')) {
    const key = /^ {2}([A-Za-z0-9_-]+):\s*$/.exec(line);
    if (key) job = key[1];
    if (WASM_JOBS.includes(job) && line === '    steps:') {
      out.push(WASM_CLANG_ENV_BLOCK, line, '      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7', WASM_CLANG_BLOCK);
      continue;
    }
    out.push(line);
  }
  return out.join('\n');
}

// The shape each CI workflow gives the change filter: a `changes` job, server's `docs` job that always runs, and every
// other job gated on `changes` as JOB_GATES[workflow] says (its own needs merged in).
const CHANGES_JOB = `  changes:
    runs-on: ubuntu-latest
    outputs:
      go: \${{ steps.areas.outputs.go }}
      core: \${{ steps.areas.outputs.core }}
      web: \${{ steps.areas.outputs.web }}
      e2e: \${{ steps.areas.outputs.e2e }}
      text: \${{ steps.areas.outputs.text }}
      mobile: \${{ steps.areas.outputs.mobile }}
      code: \${{ steps.areas.outputs.code }}
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7
        with:
          fetch-depth: 0
      - id: areas
        run: node scripts/ci-areas.mjs
        env:
          EVENT_NAME: \${{ github.event_name }}
          BASE_SHA: \${{ github.event.pull_request.base.sha }}
`;
const DOCS_JOB = `  docs:
    runs-on: ubuntu-latest
    steps:
      - run: npm run test:docs-check
      - run: npm run test:brief-check
      - run: npm run test:copy-check
      - run: npm run check:docs
      - run: npm run check:brief
      - run: npm run check:copy
      - run: npm run check:ci
`;

const OK_JOB = (workflow, jobs) => `  ${WORKFLOWS[workflow].ok}:
    needs: [${jobs.join(', ')}]
    if: always()
    runs-on: ubuntu-latest
    steps:
      - if: contains(needs.*.result, 'failure') || contains(needs.*.result, 'cancelled')
        run: exit 1
      - if: needs.ci-watchdog.result == 'skipped' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)
        run: exit 1
      - run: echo ok
`;

/**
 * GOOD_BASE's jobs dealt into the two workflows as REQUIRED_JOBS says (a job both carry into both), each workflow with
 * its real watcher, the changes job (server also docs), its gates, its barrier and its ok job, joined at SPLIT.
 */
function withChanges(text) {
  const header = text.slice(0, text.indexOf('jobs:\n') + 'jobs:\n'.length);
  const blocks = new Map();
  let current = null;
  for (const line of text.slice(header.length).split('\n')) {
    const key = /^ {2}([A-Za-z0-9_-]+):\s*$/.exec(line);
    if (key) { current = key[1]; blocks.set(current, []); }
    if (current !== null) blocks.get(current).push(line);
  }
  const build = (workflow) => {
    const own = [...REQUIRED_JOBS[workflow], ...(workflow === 'desktop' ? Object.keys(BROWSER_WEB_JOBS) : [])];
    const watcher = /^  ci-watchdog:\n[\s\S]*?(?=^  changes:)/m.exec(workflow === 'server' ? REAL_SERVER : REAL_DESKTOP_CI)[0];
    const out = [];
    for (const [name, lines] of blocks) {
      if (!own.includes(name)) continue;
      const body = [...lines];
      const needsAt = body.findIndex((line) => /^ {4}needs: \[.*\]$/.test(line));
      const needs = needsAt < 0 ? [] : /^ {4}needs: \[(.*)\]$/.exec(body[needsAt])[1].split(',').map((s) => s.trim());
      if (needsAt >= 0) body.splice(needsAt, 1);
      if (name in JOB_GATES[workflow]) body.splice(1, 0, `    needs: [${['changes', ...needs].join(', ')}]`, `    if: ${gateExpression(workflow, name)}`);
      else if (needs.length) body.splice(1, 0, `    needs: [${needs.join(', ')}]`);
      out.push(...body);
    }
    const jobs = ['ci-watchdog', 'changes', ...(workflow === 'server' ? ['docs'] : []), ...[...blocks.keys()].filter((name) => own.includes(name))];
    const barrier = `  ${workflow}-work-complete:
    needs: [${jobs.filter(name => name !== 'ci-watchdog').join(', ')}]
    if: always()
    runs-on: ubuntu-latest
    timeout-minutes: 2
    steps:
      - run: echo "All CI work reached a terminal result"
`;
    const named = header.replace('name: ci-fixture', `name: ${WORKFLOWS[workflow].name}`);
    const triggered = workflow === 'desktop' ? named.replace('  pull_request:\n', `  pull_request:\n  schedule:\n    - cron: '${NIGHTLY_CRON}'\n`) : named;
    return `${triggered}${watcher}${CHANGES_JOB}${workflow === 'server' ? DOCS_JOB : ''}${out.join('\n')}\n${barrier}${OK_JOB(workflow, [...jobs, `${workflow}-work-complete`])}`;
  };
  return build('server') + SPLIT.slice(1) + build('desktop');
}

const GOOD = withChanges(withWasmClang(GOOD_BASE));

const BUDGET = MIN_BROWSER_WEB_TIMEOUT;

// The race-gated Go tests of the repository (ruling I8), as GOOD's non-race step names them.
const GATED = [
  ['cmd/dilla-loadrig', 'TestALoopbackCellDecryptsEverything'],
  ['cmd/dilla-mediabot', 'TestTwoBotsDecryptEachOtherThroughTheSFU'],
  ['internal/media', 'TestGoPublisherToGoSubscriberDecryptsThroughTheSFU'],
  ['internal/sfu', 'TestTheSFUOfferCarriesOnlyTheDillaCodecs'],
  ['internal/sfu', 'TestPromotionAddsTheVideoSourcesAndDemotionRemovesThem'],
];

function gatedTest(name) {
  return `package x\n\nimport "testing"\n\nfunc ${name}(t *testing.T) {\n\tif raceEnabled {\n\t\tt.Skip("upstream livekit-server data race in updateRidsFromSDP; runs in the non-race step")\n\t}\n}\n`;
}

// A repository root holding server.yml and desktop-ci.yml (body's two halves, split at SPLIT; desktop-ci.yml is left out
// when an edit removed the split) and, as Go test files, the race-gated tests in `gated`.
function fixture(body, gated = GATED) {
  const dir = mkdtempSync(join(TEST_ROOT, 'dilla-ci-'));
  mkdirSync(join(dir, '.github', 'workflows'), { recursive: true });
  addE2ePackage(dir);
  const [server, desktop, ...rest] = body.split(SPLIT);
  assert.equal(rest.length, 0, 'fixture sanity: one split');
  writeFileSync(join(dir, '.github', 'workflows', 'server.yml'), desktop === undefined ? server : `${server}\n`);
  if (desktop !== undefined) writeFileSync(join(dir, '.github', 'workflows', 'desktop-ci.yml'), desktop);
  writeFileSync(join(dir, '.github', 'workflows', 'mobile.yml'), REAL_MOBILE);
  // The CLI also checks release.yml.
  writeFileSync(join(dir, '.github', 'workflows', 'release.yml'), REAL_RELEASE);
  // ... and mobile-release.yml, which signs the APK and publishes the draft release.
  writeFileSync(join(dir, '.github', 'workflows', 'mobile-release.yml'), readFileSync(join(REPO_ROOT, '.github', 'workflows', 'mobile-release.yml'), 'utf8'));
  // ... and the other workflows the CLI checks.
  releaseRoots.push(dir);
  for (const file of RELEASE_FILES.filter(path => !path.startsWith('.github/') || /ci-rerun-starved\.yml$|desktop-gpui(?:-release|-sign)?\.yml$/.test(path))) {
    mkdirSync(dirname(join(dir, file)), { recursive: true });
    writeFileSync(join(dir, file), readFileSync(join(REPO_ROOT, file), 'utf8'));
  }
  for (const [pkg, name] of gated) {
    mkdirSync(join(dir, pkg), { recursive: true });
    writeFileSync(join(dir, pkg, `${name}_test.go`), gatedTest(name));
  }
  return dir;
}

test('the real workflow passes every rule', () => {
  const root = join(dirname(fileURLToPath(import.meta.url)), '..');
  assert.deepEqual(checkWorkflow(root), []);
});

test('a well-formed workflow passes', () => {
  assert.deepEqual(checkWorkflow(fixture(GOOD)), []);
});

test('a missing job is reported by name', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/  deny:[\s\S]*$/, '')));
  assert.ok(problems.some((p) => p.includes('deny')), problems.join('\n'));
});

test('|| true is reported with its line number', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('run: npm test\n', 'run: npm test || true\n')));
  assert.ok(problems.some((p) => /\|\| true/.test(p)), problems.join('\n'));
});

test('continue-on-error is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('  deny:\n', '  deny:\n    continue-on-error: true\n')));
  assert.ok(problems.some((p) => p.includes('continue-on-error')), problems.join('\n'));
});

test('if: always() is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(NPM_TEST, '      - if: always()\n        run: npm test\n')));
  assert.ok(problems.some((p) => p.includes('always()')), problems.join('\n'));
});

// Fix round 1, finding 1 (important): the explicit-expression spelling `if: ${{ always() }}` is just
// as valid as `if: always()` and just as capable of making a job non-blocking, so the R22 gate must
// catch it too.
test('if: ${{ always() }} is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace(NPM_TEST, '      - if: ${{ always() }}\n        run: npm test\n')),
  );
  assert.ok(problems.some((p) => p.includes('always()')), problems.join('\n'));
});

test('a job that lost a required step is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace("      - if: needs.changes.outputs.core != 'false'\n        run: cargo fmt --all --check\n", '')));
  assert.ok(problems.some((p) => p.includes('cargo fmt')), problems.join('\n'));
});

test('a missing concurrency block is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/concurrency:[\s\S]*?cancel-in-progress: true\n/, '')));
  assert.ok(problems.some((p) => p.includes('concurrency')), problems.join('\n'));
});

test('commit-specific concurrency groups cannot leave obsolete pull-request runs alive', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('group: ${{ github.workflow }}-${{ github.event_name }}-${{ github.ref }}', 'group: ${{ github.workflow }}-${{ github.event_name }}-${{ github.ref }}-${{ github.sha }}')));
  assert.ok(problems.some((p) => p.includes('concurrency group')), problems.join('\n'));
});

test('a concurrency group without the event name lets a scheduled run and a push to main cancel each other', () => {
  for (const [file, check] of [['server.yml', checkServerWorkflow], ['desktop-ci.yml', checkDesktopCiWorkflow]]) {
    const [server, desktop] = GOOD.split(SPLIT);
    const group = 'group: ${{ github.workflow }}-${{ github.event_name }}-${{ github.ref }}';
    const bare = 'group: ${{ github.workflow }}-${{ github.ref }}';
    const changed = file === 'server.yml' ? server.replace(group, bare) + SPLIT + desktop : server + SPLIT + desktop.replace(group, bare);
    assert.notEqual(changed, GOOD);
    assert.ok(check(fixture(changed)).some((p) => p.startsWith(file) && p.includes('concurrency group')), file);
  }
});

test('only desktop-ci.yml runs nightly, on exactly the one cron', () => {
  const [server, desktop] = GOOD.split(SPLIT);
  const schedule = `  schedule:\n    - cron: '${NIGHTLY_CRON}'\n`;
  assert.ok(desktop.includes(schedule));
  const cases = [
    ['desktop without the schedule', server + SPLIT + desktop.replace(schedule, ''), checkDesktopCiWorkflow, 'desktop-ci.yml'],
    ['desktop on another cron', server + SPLIT + desktop.replace(NIGHTLY_CRON, '0 * * * *'), checkDesktopCiWorkflow, 'desktop-ci.yml'],
    ['desktop on a second cron', server + SPLIT + desktop.replace(schedule, schedule + "    - cron: '5 5 * * *'\n"), checkDesktopCiWorkflow, 'desktop-ci.yml'],
    ['server on the schedule', server.replace('  pull_request:\n', '  pull_request:\n' + schedule) + SPLIT + desktop, checkServerWorkflow, 'server.yml'],
  ];
  for (const [what, changed, check, file] of cases) {
    assert.notEqual(changed, GOOD, what);
    assert.ok(check(fixture(changed)).some((p) => p.startsWith(file) && p.includes('must trigger')), what);
  }
});

test('the nightly run keeps its watcher: a scheduled desktop run without one is refused, server.yml takes no schedule clause', () => {
  const clause = "github.event_name == 'schedule' || ";
  const [server, desktop] = GOOD.split(SPLIT);
  assert.ok(desktop.includes(clause));
  assert.ok(checkDesktopCiWorkflow(fixture(server + SPLIT + desktop.replace(clause, ''))).some((p) => p.includes('unsafe watchdog')));
  assert.ok(checkServerWorkflow(fixture(server.replace("|| (github.event_name == 'pull_request' && github.event.pull_request.head.repo", "|| " + clause + "(github.event_name == 'pull_request' && github.event.pull_request.head.repo") + SPLIT + desktop)).some((p) => p.includes('unsafe watchdog')));
});

test('a workflow token without read-only permissions is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('permissions:\n  contents: read\n', '')));
  assert.ok(problems.some((p) => p.includes('permissions: contents: read')), problems.join('\n'));
});

test('a workflow-level RUSTFLAGS is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('  CARGO_TERM_COLOR: always\n', '  CARGO_TERM_COLOR: always\n  RUSTFLAGS: -D warnings\n')));
  assert.ok(problems.some((p) => p.includes('RUSTFLAGS')), problems.join('\n'));
});

test('a rust-wasi job that lost if-no-files-found is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('          if-no-files-found: error\n', '')));
  assert.ok(problems.some((p) => p.includes('if-no-files-found')), problems.join('\n'));
});

test('a rust-wasi job that lost the artifact path is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace('          path: target/wasm32-wasip1/release/dilla_core_wasi.wasm\n', '')),
  );
  assert.ok(problems.some((p) => p.includes('dilla_core_wasi.wasm')), problems.join('\n'));
});

test('a go job without needs: rust-wasi is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('    needs: [changes, rust-wasi]\n', '    needs: [changes, deny]\n')));
  assert.ok(problems.some((p) => p.includes('"go"') && p.includes('needs: rust-wasi')), problems.join('\n'));
});

test('a go-harness job without needs: rust-wasi is reported', () => {
  const problems = checkWorkflow(fixture(editJob('go-harness', b => b.replace('    needs: [changes, rust-wasi, rust-native]\n', '    needs: [changes, rust-native]\n'))));
  assert.ok(problems.some((p) => p.includes('"go-harness"') && p.includes('needs: rust-wasi')), problems.join('\n'));
});

// The edges are order-independent to GitHub, so the ordering rule must accept either order.
test('a go-harness job whose needs: lists the two edges in the other order passes', () => {
  assert.deepEqual(
    checkWorkflow(fixture(editJob('go-harness', b => b.replace('needs: [changes, rust-wasi, rust-native]', 'needs: [changes, rust-native, rust-wasi]')))),
    [],
  );
});

// The go job has no testkit binary: requiring one there would fail every run.
test('a go job that requires the testkit it does not download is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace(`      - run: ${GO_TEST}\n`,
      `      - run: ${GO_TEST}\n        env:\n          DILLA_TESTKIT_REQUIRED: '1'\n`)),
  );
  assert.ok(problems.some((p) => p.includes('go-harness\'s')), problems.join('\n'));
});

test('a workflow without the go-harness job is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/  go-harness:[\s\S]*?\n\n/, '')));
  assert.ok(problems.some((p) => p.includes('missing job "go-harness"')), problems.join('\n'));
});

test('a go-harness job that lost its own timeout is reported', () => {
  const changed = GOOD.replace(/(  go-harness:\n(?:    (?:name|needs|if):.*\n)*    runs-on: ubuntu-latest\n)    timeout-minutes: 35\n/, '$1');
  assert.notEqual(changed, GOOD, 'fixture sanity');
  const problems = checkWorkflow(fixture(changed));
  assert.ok(problems.some((p) => p.includes('"go-harness"') && p.includes('timeout-minutes: 35')), problems.join('\n'));
});

test('a node job that stopped running the checker\'s own tests is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('      - run: npm run test:ci-check\n', '')));
  assert.ok(problems.some((p) => p.includes('test:ci-check')), problems.join('\n'));
});

// Fix round 1, finding 3 (important): gap-31 §4 item 2 / NV-14 resolved the pairing as
// `upload-artifact@v7` with `download-artifact@v8` — pairing v7 with v4 (facts/plan-B.md's now-stale
// deviation B15 text) is the untested combination B15 existed to prevent. The checker owns the
// cross-plan ordering rule already, so it must own this half of the hand-off too.
test('a go-harness job without needs: rust-native is reported', () => {
  const problems = checkWorkflow(fixture(editJob('go-harness', b => b.replace('needs: [changes, rust-wasi, rust-native]', 'needs: [changes, rust-wasi]'))));
  assert.ok(problems.some((p) => p.includes('needs: rust-native')), problems.join('\n'));
});

test('a rust-native job that stopped building the testkit is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace('      - run: cargo build -p dilla-testkit --release --locked\n', '')),
  );
  assert.ok(problems.some((p) => p.includes('cargo build -p dilla-testkit')), problems.join('\n'));
});

test('a rust-native job that lost the testkit artifact name is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('          name: dilla-testkit\n          path: target', '          path: target')));
  assert.ok(problems.some((p) => p.includes('name: dilla-testkit')), problems.join('\n'));
});

test('a rust-native test step without the SQLCipher memory-lock allowance is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('          sudo prlimit --pid "$$" --memlock=268435456:268435456\n', '')));
  assert.ok(problems.some((p) => p.includes('sudo prlimit')), problems.join('\n'));
});

test('a go-harness job that stopped receiving DILLA_TESTKIT is reported', () => {
  const problems = checkWorkflow(
    fixture(
      GOOD.replace(
        "        env:\n          DILLA_TESTKIT: \${{ github.workspace }}/artifacts/dilla-testkit\n          DILLA_TESTKIT_REQUIRED: '1'\n",
        '',
      ),
    ),
  );
  assert.ok(problems.some((p) => p.endsWith('is missing: DILLA_TESTKIT')), problems.join('\n'));
});

// `DILLA_TESTKIT` is a prefix of `DILLA_TESTKIT_REQUIRED`, so the first check alone would pass a
// go-harness job that dropped the second: a missing binary would then skip every scenario test, job
// still green.
test('a go-harness job that stopped requiring the testkit is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace("          DILLA_TESTKIT_REQUIRED: '1'\n", '')));
  assert.ok(problems.some((p) => p.endsWith('is missing: DILLA_TESTKIT_REQUIRED')), problems.join('\n'));
});

// The wildcard is what picks up every new internal/... package without a workflow edit.
test('a go job whose test step is narrowed to a package list is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace(GO_TEST, 'go test -race -shuffle=on -timeout 15m ./internal/store/...')),
  );
  assert.ok(problems.some((p) => p.includes('"go"') && p.includes('exactly one step')), problems.join('\n'));
  assert.ok(problems.some((p) => p.includes('"go"') && p.includes('through scripts/go-shard.mjs only')), problems.join('\n'));
});

/** The go-shard set problems of the real workflow's jobs with one set replaced. */
function setProblems(set, value) {
  const jobs = {};
  const problems = [];
  assertGoShards(jobs, problems, { ...GO_SHARD_SETS, [set]: value });
  return problems;
}

// CI budget (fix wave): the go set leaves out internal/ds and internal/api and nothing else; go-ds and go-api run them.
test('a go set that leaves out more than internal/ds and internal/api is reported', () => {
  assert.deepEqual(setProblems('go', GO_SHARD_SETS.go), []);
  const problems = setProblems('go', { shards: 4, packages: [{ pattern: './...', exclude: ['./internal/ds', './internal/api', './internal/store'] }] });
  assert.ok(problems.some((p) => p.includes('the go set must be exactly')), problems.join('\n'));
});

test('a sharded set with no shard count, or a set too many or too few, is reported', () => {
  assert.ok(setProblems('go-ds', { packages: GO_SHARD_SETS['go-ds'].packages }).some((p) => p.includes('positive shard count')));
  assert.ok(setProblems('go-extra', { shards: 2, packages: [{ pattern: './cmd/...' }] }).some((p) => p.includes('must be exactly go, go-api')));
  const problems = [];
  const { 'go-harness': _dropped, ...rest } = GO_SHARD_SETS;
  assertGoShards({}, problems, rest);
  assert.ok(problems.some((p) => p.includes('the go-harness set must be exactly')), problems.join('\n'));
});

test('a sharded job whose matrix does not match its set\'s shard count is reported', () => {
  for (const set of Object.keys(GO_SHARD_SETS)) {
    const matrix = goShardMatrix(set);
    const shorter = matrix.replace(/, \d+\]/, ']');
    assert.notEqual(shorter, matrix, set);
    const problems = checkWorkflow(fixture(editJob(set, (b) => b.replace(matrix, shorter))));
    assert.ok(problems.some((p) => p.includes(`"${set}"`) && p.includes('fail-fast matrix')), problems.join('\n'));
    const loose = checkWorkflow(fixture(editJob(set, (b) => b.replace('fail-fast: true', 'fail-fast: false'))));
    assert.ok(loose.some((p) => p.includes(`"${set}"`) && p.includes('fail-fast matrix')), loose.join('\n'));
  }
});

test('a sharded job that runs another set, a fixed shard or a second go test is reported', () => {
  const problems = checkWorkflow(fixture(editJob('go-api', (b) => b.replace('run go-api ', 'run go-ds '))));
  assert.ok(problems.some((p) => p.includes('"go-api"') && p.includes('exactly one step')), problems.join('\n'));
  const fixed = checkWorkflow(fixture(editJob('go-ds', (b) => b.replace('run go-ds ${{ matrix.shard }}', 'run go-ds 1'))));
  assert.ok(fixed.some((p) => p.includes('"go-ds"') && p.includes('exactly one step')), fixed.join('\n'));
  const twice = checkWorkflow(fixture(editJob('go-harness', (b) => b.replace('    steps:\n', '    steps:\n      - run: go test -race ./internal/testkit/...\n'))));
  assert.ok(twice.some((p) => p.includes('"go-harness"') && p.includes('through scripts/go-shard.mjs only')), twice.join('\n'));
  const unnamed = checkWorkflow(fixture(editJob('go-postgres', (b) => b.replace('    name: go-postgres-${{ matrix.shard }}\n', ''))));
  assert.ok(unnamed.some((p) => p.includes('"go-postgres"') && p.includes('must be named')), unnamed.join('\n'));
  const skipped = checkWorkflow(fixture(editJob('go', (b) => b.replace(`      - run: ${GO_TEST}`, `      - if: matrix.shard != 4\n        run: ${GO_TEST}`))));
  assert.ok(skipped.some((p) => p.includes('"go"') && p.includes('must not be conditional')), skipped.join('\n'));
});

// web3.9: the css checker's own test and the check itself, and the AFK / channel-parent tests on Postgres.
test('a node job without the css checker steps is reported', () => {
  for (const step of ['npm run test:css-check', 'npm run check:css']) {
    const problems = checkWorkflow(fixture(GOOD.replace(`      - run: ${step}\n`, '')));
    assert.ok(problems.some((p) => p.includes('"node"') && p.includes(step)), problems.join('\n'));
  }
});

// Desktop merge security review M2: the go:embed'ed desktop auth script must match its generator's output.
test('a node job that stopped checking the embedded desktop auth script is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('      - run: npm run check:desktop-auth\n', '')));
  assert.ok(problems.some((p) => p.includes('"node"') && p.includes('npm run check:desktop-auth')), problems.join('\n'));
});

test('a go-postgres set that stopped running the api AFK tests is reported', () => {
  const packages = GO_SHARD_SETS['go-postgres'].packages;
  const problems = setProblems('go-postgres', { shards: 2, packages: packages.filter((p) => p.pattern !== './internal/api/...') });
  assert.ok(problems.some((p) => p.includes('the go-postgres set must be exactly') && p.includes('./internal/api/...')), problems.join('\n'));
  assert.ok(packages.some((p) => p.pattern === './internal/api/...' && p.run === POSTGRES_API_RUN));
});

function withAPITests(body) {
  const dir = fixture(GOOD);
  mkdirSync(join(dir, 'internal', 'api'), { recursive: true });
  writeFileSync(join(dir, 'internal', 'api', 'afk_x_test.go'), body);
  return dir;
}

test('an AFK test the go-postgres -run regex misses is reported', () => {
  const problems = checkWorkflow(withAPITests('package api\n\nfunc TestAFKFine(t *testing.T) {}\nfunc TestSomethingUnmatched(t *testing.T) {}\n'));
  assert.ok(problems.some((p) => p.includes('TestSomethingUnmatched') && p.includes('afk_x_test.go')), problems.join('\n'));
  assert.ok(!problems.some((p) => p.includes('TestAFKFine')), problems.join('\n'));
});

test('AFK tests the go-postgres -run regex matches pass', () => {
  const dir = withAPITests('package api\n\nfunc TestAFKFine(t *testing.T) {}\nfunc TestTheBuiltInCarriesTheMarker(t *testing.T) {}\n');
  assert.deepEqual(checkWorkflow(dir), []);
});

test('a workflow without the go-ds job is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/  go-ds:[\s\S]*?\n\n/, '')));
  assert.ok(problems.some((p) => p.includes('missing job "go-ds"')), problems.join('\n'));
});

test('a go-ds job that stopped running internal/ds is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace(`      - run: ${goShardCommand('go-ds')}\n`, '')),
  );
  assert.ok(problems.some((p) => p.includes('"go-ds"') && p.includes('run go-ds')), problems.join('\n'));
});

test('a go-ds job with a shorter test budget is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace(goShardCommand('go-ds'), goShardCommand('go-ds').replace('-timeout 40m', '-timeout 10m'))),
  );
  assert.ok(problems.some((p) => p.includes('"go-ds"') && p.includes('-timeout 40m')), problems.join('\n'));
});

test('a go-ds job with too little wall-clock margin is reported', () => {
  const start = GOOD.indexOf('  go-ds:\n');
  const body = GOOD.slice(start, GOOD.indexOf('\n\n', start));
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace('timeout-minutes: 50', 'timeout-minutes: 25'))));
  assert.ok(problems.some((p) => p.includes('"go-ds"') && p.includes('timeout-minutes: 50')), problems.join('\n'));
});

test('a workflow without the go-api job is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/  go-api:[\s\S]*?\n\n/, '')));
  assert.ok(problems.some((p) => p.includes('missing job "go-api"')), problems.join('\n'));
});

test('a go-api job that stopped running internal/api is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace(`      - run: ${goShardCommand('go-api')}\n`, '')),
  );
  assert.ok(problems.some((p) => p.includes('"go-api"') && p.includes('run go-api')), problems.join('\n'));
});

test('a go-api job with a shorter budget is reported', () => {
  const start = GOOD.indexOf('  go-api:\n');
  const body = GOOD.slice(start, GOOD.indexOf('\n\n', start));
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace('timeout-minutes: 50', 'timeout-minutes: 20'))));
  assert.ok(problems.some((p) => p.includes('"go-api"') && p.includes('timeout-minutes: 50')), problems.join('\n'));
});

test('a go set that stopped leaving out internal/api is reported', () => {
  const problems = setProblems('go', { shards: 4, packages: [{ pattern: './...', exclude: ['./internal/ds'] }] });
  assert.ok(problems.some((p) => p.includes('the go set must be exactly')), problems.join('\n'));
});

test('a go-api job without needs: rust-wasi is reported', () => {
  const start = GOOD.indexOf('  go-api:\n');
  const body = GOOD.slice(start, GOOD.indexOf('\n\n', start));
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace('    needs: [changes, rust-wasi]\n', ''))));
  assert.ok(problems.some((p) => p.includes('"go-api"') && p.includes('rust-wasi')), problems.join('\n'));
});

test('a go-ds job without needs: rust-wasi is reported', () => {
  const start = GOOD.indexOf('  go-ds:\n');
  const body = GOOD.slice(start, GOOD.indexOf('\n\n', start));
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace('    needs: [changes, rust-wasi]\n', ''))));
  assert.ok(problems.some((p) => p.includes('"go-ds"') && p.includes('rust-wasi')), problems.join('\n'));
});

for (const [job, needle] of [
  ['rust-wasm-node', 'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/media/wasm'],
  ['rust-wasm-node', 'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg'],
  ['rust-wasm-node', 'node scripts/check-wasm-size.mjs packages/core-wasm/pkg/dilla_core_wasm_bg.wasm'],
  ['rust-wasm-node', 'npm run typecheck -w @dilla/media'],
  ['rust-wasm-node', 'npm run test:wasm -w @dilla/media'],
  ['rust-wasm-node', 'npm run check:schema -w @dilla/media'],
  ['go-harness', goShardCommand('go-harness')],
  ['go-harness', 'name: dilla-testkit'],
  ['go-lint', 'golangci-lint run ./...'],
  ['go-vuln', '-scan package -tags dillapins ./internal/deps'],
  ['go-sqlc', 'sqlc diff'],
  ['go-postgres', 'postgres:18.6-alpine3.24'],
  ['go-postgres', '--locale-provider=builtin --builtin-locale=C.UTF-8'],
  ['go-postgres', 'DILLA_TEST_PG'],
  // I7 (fix wave): internal/ops holds the only Postgres backup, restore and heal tests; a step that
  // drops it skips them in every job. The -timeout is explicit so a slow package fails by name.
  ['go-postgres', goShardCommand('go-postgres')],
  ['go-release', 'if-no-files-found: error'],
]) {
  test(`a ${job} job that lost "${needle}" is reported`, () => {
    const start = GOOD.indexOf(`  ${job}:\n`);
    const end = GOOD.indexOf('\n\n', start);
    const body = GOOD.slice(start, end === -1 ? undefined : end);
    assert.ok(body.includes(needle), 'fixture sanity: ' + needle);
    const gutted = body.split('\n').filter((l) => !l.includes(needle)).join('\n');
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => gutted)));
    assert.ok(problems.some((p) => p.includes(`"${job}"`) && p.includes(needle)), problems.join('\n'));
  });
}

test('a go job with download-artifact@v4 instead of v8 is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replaceAll('actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8', 'actions/download-artifact@v4')),
  );
  assert.ok(problems.some((p) => p.includes('download-artifact@v4')), problems.join('\n'));
});

// Ruling L: the Go half of the tree is gated by this checker too, so a workflow that simply never
// declares the `go` job — the state this repository was in before Plan B task 8 — must be reported
// rather than silently accepted. Before this rule the checker passed such a workflow.
test('a workflow with no go job at all is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/  go:[\s\S]*$/, '')));
  assert.ok(problems.some((p) => p.includes('missing job "go"')), problems.join('\n'));
  assert.ok(problems.some((p) => p.includes('missing job "go-fts5-arm64"')), problems.join('\n'));
  for (const job of ['go-lint', 'go-vuln', 'go-sqlc', 'go-postgres', 'go-release']) {
    assert.ok(problems.some((p) => p.includes(`missing job "${job}"`)), problems.join('\n'));
  }
});

// Ruling M: `internal/deps` is the only thing that compiles the pinned LiveKit/wazero/sqlite graph
// with the three pion `replace` directives, and it is behind the `dillapins` tag, so no other step
// in the workflow would notice the graph breaking.
test('a go job that lost the pinned-module-graph build is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace('      - run: CGO_ENABLED=0 go build -tags dillapins ./internal/deps\n', '')),
  );
  assert.ok(problems.some((p) => p.includes('dillapins')), problems.join('\n'));
});

test('a go job that stopped running the race-detector tests is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace(`      - run: ${GO_TEST}\n`, '')),
  );
  assert.ok(problems.some((p) => p.includes('-race')), problems.join('\n'));
});

test('a go-fts5-arm64 job that stopped running on arm is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('    runs-on: ubuntu-24.04-arm\n', '    runs-on: ubuntu-latest\n')));
  assert.ok(problems.some((p) => p.includes('ubuntu-24.04-arm')), problems.join('\n'));
});

// The CLI entry point at the bottom of check-ci-workflow.mjs, not `checkWorkflow` itself: this must
// be run as a subprocess from a cwd that is NOT the fixture root and has no .github/workflows of its
// own, so a script that silently fell back to process.cwd() (ignoring argv[2]) cannot pass by
// accident — it would fail to find server.yml at all rather than happening to validate the right file.
function runCli(root, cwd) {
  return spawnSync(process.execPath, [SCRIPT_PATH, root], { cwd, encoding: 'utf8' });
}

test('the CLI honours a root argument passed from a different cwd', () => {
  // The real workflows: the CLI also checks release.yml, whose actions must be theirs at the same versions.
  const root = fixture(REAL_CI);
  const cwd = mkdtempSync(join(TEST_ROOT, 'dilla-ci-cwd-'));
  const result = runCli(root, cwd);
  assert.equal(result.status, 0, `stdout: ${result.stdout}\nstderr: ${result.stderr}`);
});

test('the CLI reports problems in a workflow at the given root, run from elsewhere', () => {
  const root = fixture(GOOD.replace(/  deny:[\s\S]*$/, ''));
  const cwd = mkdtempSync(join(TEST_ROOT, 'dilla-ci-cwd-'));
  const result = runCli(root, cwd);
  assert.equal(result.status, 1, `stdout: ${result.stdout}\nstderr: ${result.stderr}`);
  assert.match(result.stderr, /deny/);
});

// Jonas, 2026-10-21: the container image is dropped.
test('a workflow that builds or pushes a container image is reported', () => {
  for (const addition of [
    '      - run: docker login ghcr.io -u me\n',
    '      - uses: docker/setup-buildx-action@v4\n',
    '      - run: bash scripts/build-ci-image.sh\n',
  ]) {
    const problems = checkWorkflow(fixture(GOOD.replace('  server-ok:', () => `  image:\n    runs-on: ubuntu-latest\n    steps:\n${addition}  server-ok:`)));
    assert.ok(problems.some((p) => p.includes('container image was dropped')), `${addition}: ${problems.join('\n')}`);
  }
});

test('the real workflow has no image job', () => {
  assert.doesNotMatch(REAL_CI, /^ {2}image:/m);
});

// The rule that covers a job added tomorrow: no job is named in REQUIRED_STEPS, yet its upload-artifact
// step is still held to the fail-closed setting.
test('an upload-artifact step in any job without if-no-files-found: error is reported', () => {
  const extra = '\n  extra:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7\n        with:\n          name: x\n          path: y\n      - run: echo after\n';
  const problems = checkWorkflow(fixture(GOOD + extra));
  assert.deepEqual(
    problems.map((p) => p.replace(/^desktop-ci\.yml:\d+: /, '')).filter((p) => !p.includes('JOB_GATES') && !p.includes('"desktop-ok"') && !p.includes('completion barrier') && !p.includes('does not belong')),
    ['actions/upload-artifact without "if-no-files-found: error" — CI is fail-closed'],
  );
  // The same step with the setting is clean.
  assert.deepEqual(checkWorkflow(fixture(GOOD + extra.replace('          path: y\n', '          path: y\n          if-no-files-found: error\n'))).filter((p) => !p.includes('JOB_GATES') && !p.includes('"desktop-ok"') && !p.includes('completion barrier') && !p.includes('does not belong')), []);
});

// Task 21: the browser E2EE suite.
test('a workflow without the browser-media job is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/  browser-media:[\s\S]*?\n\n/, '')));
  assert.ok(problems.some((p) => p.includes('missing job "browser-media"')), problems.join('\n'));
});

test('browser-media must explicitly extract RNNoise wasm before Playwright', () => {
  const step = '      - run: node packages/media/scripts/extract-rnnoise-wasm.mjs\n';
  assert.deepEqual(checkWorkflow(fixture(GOOD)), []);
  const problems = checkWorkflow(fixture(GOOD.replace(step, '')));
  assert.ok(problems.some((p) => p.includes('extract-rnnoise-wasm.mjs')), problems.join('\n'));
});

test('browser-media extraction must precede the media suite', () => {
  const step = '      - run: node packages/media/scripts/extract-rnnoise-wasm.mjs\n';
  const moved = GOOD.replace(step, '').replace('      - run: npm run test:e2e:media -w @dilla/e2e\n', '      - run: npm run test:e2e:media -w @dilla/e2e\n' + step);
  const problems = checkWorkflow(fixture(moved));
  assert.ok(problems.some((p) => p.includes('RNNoise extraction') && p.includes('before')), problems.join('\n'));
});

test('browser-media must start an audio server before the media suite', () => {
  const step = '      - run: sh scripts/ci-audio-server.sh\n';
  const missing = checkWorkflow(fixture(GOOD.replace(step, '')));
  assert.ok(missing.some((p) => p.includes('ci-audio-server.sh')), missing.join('\n'));
  const moved = GOOD.replace(step, '').replace('      - run: npm run test:e2e:media -w @dilla/e2e\n', '      - run: npm run test:e2e:media -w @dilla/e2e\n' + step);
  const late = checkWorkflow(fixture(moved));
  assert.ok(late.some((p) => p.includes('audio server') && p.includes('before')), late.join('\n'));
});

test('the browser-media job requires the real-SFU AV1 leg', () => {
  const stripped = GOOD.replace(/      - run: npx playwright test --config e2e\/playwright\.media\.config\.ts --project=chromium-media unsupported-sfu-codec\.spec\.ts\n        env:\n          DILLA_MEDIA_SFU_AV1: '1'\n          DILLA_TESTKIT: \$\{\{ github\.workspace \}\}\/artifacts\/dilla-testkit\n/, '');
  const problems = checkWorkflow(fixture(stripped));
  assert.ok(problems.some((p) => p.includes('unsupported-sfu-codec.spec.ts')), problems.join('\n'));
});

for (const [need, kept] of [['rust-wasi', 'rust-native'], ['rust-native', 'rust-wasi']]) {
  test(`a browser-media job without needs: ${need} is reported`, () => {
    const problems = checkWorkflow(fixture(GOOD.replace('    needs: [changes, rust-native, rust-wasi]\n', `    needs: [changes, ${kept}]\n`)));
    assert.ok(problems.some((p) => p.includes('"browser-media"') && p.includes(`needs: ${need}`)), problems.join('\n'));
  });
}

for (const needle of [
  'timeout-minutes: 25',
  'name: dilla-core-wasi',
  'path: internal/mlswasi/testdata',
  'name: dilla-testkit',
  'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/media/wasm',
  'go build -o target/dilla-mediabot ./cmd/dilla-mediabot',
  'npx playwright install --with-deps chromium',
  'npm run test:e2e:media -w @dilla/e2e',
  'DILLA_TESTKIT: ${{ github.workspace }}/artifacts/dilla-testkit',
]) {
  test(`a browser-media job that lost "${needle}" is reported`, () => {
    const start = GOOD.indexOf('  browser-media:\n');
    const body = GOOD.slice(start, GOOD.indexOf('\n\n', start));
    assert.ok(body.includes(needle), 'fixture sanity: ' + needle);
    const gutted = body.split('\n').filter((l) => !l.includes(needle)).join('\n');
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => gutted)));
    assert.ok(problems.some((p) => p.includes('"browser-media"') && p.includes(needle)), problems.join('\n'));
  });
}

test('a browser-spike job without its timeout is reported', () => {
  const problems = checkWorkflow(fixture(editJob('browser-spike', (body) => body.replace('    timeout-minutes: 20\n', ''))));
  assert.ok(problems.some((p) => p.includes('"browser-spike"') && p.includes('timeout-minutes: 20')), problems.join('\n'));
});

test('the video publishing tests retain a non-race CI step', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(NON_RACE_STEP, 'go test ./cmd/dilla-mediabot')));
  assert.ok(problems.some((p) => p.includes('job "go-checks"') && p.includes(NON_RACE_STEP)), problems.join('\n'));
  assert.ok(problems.some((p) => p.includes('no non-race') && p.includes('5 race-gated')), problems.join('\n'));
});

// Ruling I8, the CI run on main after PR #7: TestALoopbackCellDecryptsEverything published VP8 under
// -race. The repository's gated tests are exactly the ones the non-race step names.
test('the repository gates exactly the tests the non-race step names', () => {
  const root = join(dirname(fileURLToPath(import.meta.url)), '..');
  const found = raceGatedTests(root).map((g) => [g.pkg.slice(2), g.name]);
  assert.deepEqual(found.toSorted(), GATED.toSorted());
});

test('a race-gated test the non-race step does not name is reported', () => {
  const extra = [...GATED, ['internal/api', 'TestANewVideoPublisher']];
  const problems = checkWorkflow(fixture(GOOD, extra));
  assert.ok(problems.some((p) => p.includes('does not name race-gated TestANewVideoPublisher')), problems.join('\n'));
  assert.ok(problems.some((p) => p.includes('does not run ./internal/api')), problems.join('\n'));
});

test('a non-race step that lost a gated package is reported', () => {
  const step = NON_RACE_STEP.replace(' ./cmd/dilla-loadrig', '');
  const problems = checkWorkflow(fixture(GOOD.replace(NON_RACE_STEP, step)));
  assert.ok(
    problems.some((p) => p.includes('does not run ./cmd/dilla-loadrig') && p.includes('TestALoopbackCellDecryptsEverything')),
    problems.join('\n'),
  );
});

test('a non-race step that lost a gated test name is reported', () => {
  const step = NON_RACE_STEP.replace('TestALoopbackCellDecryptsEverything|', '');
  const problems = checkWorkflow(fixture(GOOD.replace(NON_RACE_STEP, step)));
  assert.ok(problems.some((p) => p.includes('does not name race-gated TestALoopbackCellDecryptsEverything')), problems.join('\n'));
});

// A renamed or deleted test leaves an alternative that matches nothing, and go test passes that.
test('a non-race step naming a test that is not gated is reported', () => {
  const problems = checkWorkflow(fixture(GOOD, GATED.slice(1)));
  assert.ok(problems.some((p) => p.includes('names TestALoopbackCellDecryptsEverything, which is no race-gated test')), problems.join('\n'));
});

test('a non-race step that gained -race is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(NON_RACE_STEP, NON_RACE_STEP.replace('go test ', 'go test -race '))));
  assert.ok(problems.some((p) => p.includes('no non-race')), problems.join('\n'));
});

test('a raceEnabled gate outside a Test function is reported', () => {
  const root = fixture(GOOD);
  writeFileSync(join(root, 'internal', 'sfu', 'helper_test.go'), 'package x\n\nfunc helper() {\n\tif raceEnabled {\n\t\treturn\n\t}\n}\n');
  const problems = checkWorkflow(root);
  assert.ok(problems.some((p) => p.includes('internal/sfu/helper_test.go') && p.includes('outside a Test function')), problems.join('\n'));
});

test('race gates under node_modules and dot-directories are not the module\'s', () => {
  const root = fixture(GOOD);
  for (const dir of ['node_modules/x', '.claude/worktrees/other']) {
    mkdirSync(join(root, dir), { recursive: true });
    writeFileSync(join(root, dir, 'gate_test.go'), gatedTest('TestElsewhere'));
  }
  assert.deepEqual(checkWorkflow(root), []);
});

test('browser-media must upload failure traces and host logs', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('name: browser-media-results', 'name: removed-results')));
  assert.ok(problems.some((p) => p.includes('browser-media') && p.includes('browser-media-results')), problems.join('\n'));
});

test('a node job that stopped running the wasm size checker\'s own tests is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('      - run: npm run test:wasm-size-check\n', '')));
  assert.ok(problems.some((p) => p.includes('"node"') && p.includes('npm run test:wasm-size-check')), problems.join('\n'));
});

test('a browser-spike job that lost its size-profile wasm-pack line is reported', () => {
  const line = 'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir spike/pkg';
  const problems = checkWorkflow(fixture(GOOD.replace(`      - run: ${line}\n`, '')));
  assert.ok(problems.some((p) => p.includes('"browser-spike"') && p.includes(line)), problems.join('\n'));
});

test('a wasm-pack build with --release instead of the size profile is reported with its line', () => {
  const body = GOOD.replace(
    'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir spike/pkg',
    'wasm-pack build core/dilla-core-wasm --target web --release --mode no-install --out-dir spike/pkg',
  );
  assert.notEqual(body, GOOD, 'fixture sanity');
  const lineNo = body.split(SPLIT)[1].split('\n').findIndex((l) => l.includes('--release --mode no-install --out-dir spike/pkg')) + 1;
  const problems = checkWorkflow(fixture(body));
  assert.ok(
    problems.includes(`desktop-ci.yml:${lineNo}: wasm-pack build must use --profile wasm-release, not --release (C16)`),
    problems.join('\n'),
  );
});

test('a wasm-pack build with no profile at all is reported', () => {
  const body = GOOD.replace(
    'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg',
    'wasm-pack build core/dilla-core-wasm --target web --mode no-install --out-dir ../../packages/core-wasm/pkg',
  );
  assert.notEqual(body, GOOD, 'fixture sanity');
  const problems = checkWorkflow(fixture(body));
  assert.ok(problems.some((p) => p.endsWith('wasm-pack build must use --profile wasm-release, not --release (C16)')), problems.join('\n'));
});

// Task 26 (web-1): the web client's build, its browser suites, and its hand-off to release.

/** One job of GOOD, from its key line to the line before the next job key. */
function jobBody(job) {
  const lines = GOOD.split('\n');
  const from = lines.indexOf(`  ${job}:`);
  assert.ok(from !== -1, `fixture sanity: job ${job}`);
  let to = from + 1;
  while (to < lines.length && !/^ {2}[A-Za-z0-9_-]+:\s*$/.test(lines[to])) to++;
  return lines.slice(from, to).join('\n');
}

for (const job of ['web-build', ...Object.keys(BROWSER_WEB_JOBS)]) {
  test(`a workflow without the ${job} job is reported`, () => {
    const problems = checkWorkflow(fixture(GOOD.replace(jobBody(job), '')));
    assert.ok(problems.some((p) => p.includes(`missing job "${job}"`)), problems.join('\n'));
  });
}

for (const [job, needle] of [
  ['web-build', 'timeout-minutes: 15'],
  ['web-build', 'wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg'],
  ['web-build', 'node scripts/check-wasm-size.mjs packages/core-wasm/pkg/dilla_core_wasm_bg.wasm'],
  ['web-build', 'npm run typecheck -w @dilla/client-core'],
  ['web-build', 'npm run typecheck -w @dilla/web'],
  ['web-build', 'npm run typecheck:e2e'],
  ['web-build', 'npm run build -w @dilla/web'],
  ['web-build', 'npm run build:harness -w @dilla/client-core'],
  ['web-build', 'go test -tags webdist ./internal/web/...'],
  ['web-build', 'name: dilla-web-dist'],
  ['web-build', 'path: packages/web/dist/'],
  ['web-build', 'name: dilla-core-harness'],
  ['web-build', 'path: packages/client-core/harness-dist/'],
  ...Object.entries(BROWSER_WEB_JOBS).flatMap(([job, spec]) => [
    [job, `timeout-minutes: ${spec.timeout}`],
    ...(spec.engine === 'firefox' && spec.suite !== 'core' ? [[job, 'sh scripts/ci-audio-server.sh']] : []),
    [job, 'name: dilla-core-wasi'],
    [job, 'path: internal/mlswasi/testdata'],
    [job, 'name: dilla-testkit'],
    [job, 'name: dilla-web-dist'],
    [job, 'path: packages/web/dist'],
    [job, 'name: dilla-core-harness'],
    [job, 'path: packages/client-core/harness-dist'],
    [job, `npx playwright install --with-deps ${spec.engine}`],
    [job, browserSuiteCommand(spec)],
    [job, 'DILLA_TESTKIT: ${{ github.workspace }}/artifacts/dilla-testkit'],
    [job, "if: failure() && hashFiles('e2e/test-results/**') != ''"],
    [job, `name: browser-web-results-${job}`],
    [job, 'path: e2e/test-results'],
  ]),
  ['go-release', 'name: dilla-web-dist'],
  ['go-release', 'path: internal/web/dist'],
  ['go-release', 'content="placeholder"'],
  ['go-release', 'go test -tags webdist ./internal/web/...'],
  ['node', 'npm run test:manifest-writer'],
  ['server-scripts', 'npm run test:bot-ops-check'],
  ['server-scripts', 'npm run check:bot-ops'],
  ['server-scripts', 'npm run test:install'],
  ['server-scripts', 'bash deploy/install_bot_test.sh'],
  ['rust-wasm-node', 'npm run typecheck -w @dilla/core-wasm'],
  ['rust-wasm-node', 'cargo clippy -p dilla-core-wasm --target wasm32-unknown-unknown --lib --locked -- -D warnings'],
  ['rust-wasm-node', '- run: rustup show active-toolchain'],
]) {
  test(`a ${job} job that lost "${needle}" is reported (web-1)`, () => {
    const body = jobBody(job);
    assert.ok(body.includes(needle), 'fixture sanity: ' + needle);
    const gutted = body.split('\n').filter((l) => !l.includes(needle)).join('\n');
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => gutted)));
    assert.ok(problems.some((p) => p.includes(`"${job}"`) && p.includes(needle)), problems.join('\n'));
  });
}

// release.yml publishes deploy/install.sh and deploy/install-bot.sh, so the server workflow (server-ok gates the release)
// runs their tests and the Bot operation drift guard; desktop's node job no longer carries the installer tests.
const SERVER_SCRIPT_STEPS = ['npm run test:bot-ops-check', 'npm run check:bot-ops', 'npm run test:install', 'bash deploy/install_bot_test.sh'];
test('the server workflow runs the scripts release.yml ships, gated on any code change', () => {
  const body = /^  server-scripts:\n[\s\S]*?(?=^  [a-z][\w-]*:)/m.exec(REAL_SERVER)?.[0] ?? '';
  assert.match(body, /^    needs: \[changes\]\n    if: needs\.changes\.outputs\.code != 'false'$/m);
  for (const step of SERVER_SCRIPT_STEPS) assert.ok(body.includes(`      - run: ${step}\n`), step);
  assert.match(REAL_SERVER, /^  server-ok:\n    needs: \[[^\]\n]*\bserver-scripts\b/m);
  assert.doesNotMatch(REAL_DESKTOP_CI, /npm run test:install|install_bot_test\.sh/);
  for (const step of SERVER_SCRIPT_STEPS) {
    const problems = checkServerWorkflow(fixture(REAL_CI.replace(`      - run: ${step}\n`, '')));
    assert.ok(problems.some((p) => p.includes('job "server-scripts" is missing') && p.includes(step)), `${step}\n${problems.join('\n')}`);
  }
});

for (const job of Object.keys(BROWSER_WEB_JOBS)) {
  for (const need of ['rust-native', 'rust-wasi', 'web-build']) {
    test(`a ${job} job without needs: ${need} is reported`, () => {
      const body = jobBody(job);
      const kept = ['rust-native', 'rust-wasi', 'web-build'].filter((n) => n !== need).join(', ');
      const cut = body.replace('    needs: [changes, rust-native, rust-wasi, web-build]\n', `    needs: [changes, ${kept}]\n`);
      assert.notEqual(cut, body, 'fixture sanity: needs');
      const problems = checkWorkflow(fixture(GOOD.replace(body, () => cut)));
      assert.ok(problems.some((p) => p.includes(`"${job}"`) && p.includes(`needs: ${need}`)), problems.join('\n'));
    });
  }
}

test('shared core integration uses only the Chromium engine', () => {
  assert.deepEqual(BROWSER_WEB_JOBS, { 'core-integration': { engine: 'chromium', suite: 'core', shard: null, timeout: 20 } });
  assert.equal(browserSuiteCommand(BROWSER_WEB_JOBS['core-integration']), 'npm run test:e2e:core:chromium -w @dilla/e2e');
});

test('the core security job cannot be deleted, folded into another suite or given too little time', () => {
  const body = jobBody('core-integration');
  assert.ok(checkWorkflow(fixture(GOOD.replace(body, ''))).some(p => p.includes('missing job "core-integration"')));
  const both = body.replace('    steps:\n', '    steps:\n      - run: npm run test:e2e:voice:chromium -w @dilla/e2e\n');
  assert.ok(checkWorkflow(fixture(GOOD.replace(body, () => both))).some(p => p.includes('exactly one Playwright suite')));
  assert.ok(BROWSER_WEB_JOBS['core-integration'].timeout >= MIN_BROWSER_WEB_TIMEOUT);
});

test('unsupported browser client jobs are rejected when reintroduced', () => {
  const old = '  firefox-web-1:\n    needs: [changes]\n    if: needs.changes.outputs.e2e != \'false\'\n    steps:\n      - run: npm run test:e2e:web:firefox -w @dilla/e2e\n';
  const problems = checkWorkflow(fixture(GOOD.replace('  deny:', () => old + '  deny:')));
  assert.ok(problems.some(p => p.includes('firefox-web-1') && p.includes('JOB_GATES')));
});

test('every browser job has its own artifact name in the real desktop-ci.yml', () => {
  const names = Object.keys(BROWSER_WEB_JOBS).map((job) => `name: browser-web-results-${job}`);
  assert.equal(new Set(names).size, names.length);
  for (const name of names) assert.equal(REAL_CI.split('\n').filter((l) => l.trim() === name).length, 1, name);
});

test('the real desktop-ok job needs every browser job', () => {
  const body = REAL_DESKTOP_CI.slice(REAL_DESKTOP_CI.indexOf('\n  desktop-ok:'));
  for (const job of Object.keys(BROWSER_WEB_JOBS)) assert.ok(new RegExp(`needs:[^\\n]*[ \\[]${job}[,\\]]`).test(body), job);
});

test('a desktop-ok job that forgot core security integration is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/(  desktop-ok:\n    needs: \[[^\]]*), core-integration/, '$1')));
  assert.ok(problems.some((p) => p.includes('"desktop-ok"') && p.includes('"core-integration"')), problems.join('\n'));
});

test('a browser job without the e2e gate is reported', () => {
  for (const job of Object.keys(BROWSER_WEB_JOBS)) {
    const body = jobBody(job);
    const ungated = body.replace("    if: needs.changes.outputs.e2e != 'false'\n", '');
    assert.notEqual(ungated, body, `fixture sanity: ${job} gate`);
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => ungated)));
    assert.ok(problems.some((p) => p.includes(`"${job}"`) && p.includes("needs.changes.outputs.e2e != 'false'")), `${job}: ${problems.join('\n')}`);
  }
});

test('go-release must point the webdist test at the downloaded client', () => {
  const line = '          DILLA_WEB_DIST: ${{ github.workspace }}/internal/web/dist\n';
  const body = jobBody('go-release');
  assert.ok(body.includes(line), 'fixture sanity: the DILLA_WEB_DIST line');
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace(line, ''))));
  assert.ok(
    problems.some((p) => p.includes('"go-release"') && p.includes('DILLA_WEB_DIST: ${{ github.workspace }}/internal/web/dist')),
    problems.join('\n'),
  );
});

test('a go-release job without needs: web-build is reported', () => {
  const body = jobBody('go-release');
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace('    needs: [changes, web-build]\n', ''))));
  assert.ok(problems.some((p) => p.includes('"go-release"') && p.includes('needs: web-build')), problems.join('\n'));
});

test('a go-release job that builds before it fetches the web client is reported', () => {
  const build = '      - run: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/dillad-linux-amd64 ./cmd/dillad\n';
  const body = jobBody('go-release');
  assert.ok(body.includes(build), 'fixture sanity: the amd64 build');
  const moved = body.replace(build, '').replace('    steps:\n', `    steps:\n${build}`);
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => moved)));
  assert.ok(problems.some((p) => p.includes('"go-release"') && p.includes('before it builds')), problems.join('\n'));
});

// web-2b task 11 (L-CI-20, L-CI-21): the screenshots of the web-2b stories and the re-derived browser budget.
const UI_SHOTS_2B = 'node packages/ui/scripts/shoot-stories.mjs 6016 target/ui-screens/web-2b form- ceremony- shell- conversation- settings- screens- primitives-channelrow-- conversation-messagetoolbar-- conversation-attachmentcard-- screens-conversation--';

for (const needle of [UI_SHOTS_2B, 'name: ui-screens', 'path: target/ui-screens/web-2b']) {
  test(`a ui job that lost "${needle}" is reported (web-2b)`, () => {
    const body = jobBody('ui');
    assert.ok(body.includes(needle), 'fixture sanity: ' + needle);
    const gutted = body.split('\n').filter((l) => !l.includes(needle)).join('\n');
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => gutted)));
    assert.ok(problems.some((p) => p.includes('"ui"') && p.includes(needle)), problems.join('\n'));
  });
}

for (const prefix of ['form-', 'ceremony-', 'shell-', 'conversation-', 'settings-', 'screens-', 'primitives-channelrow--', 'conversation-messagetoolbar--', 'conversation-attachmentcard--', 'screens-conversation--']) {
  test(`a ui screenshot step that dropped the ${prefix} prefix is reported (web-2b)`, () => {
    const body = jobBody('ui');
    const narrowed = body.replace(` ${prefix}`, '');
    assert.notEqual(narrowed, body, `fixture sanity: the ${prefix} prefix`);
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => narrowed)));
    assert.ok(problems.some((p) => p.includes('"ui"') && p.includes(UI_SHOTS_2B)), problems.join('\n'));
  });
}

test('a ui job still capturing into an earlier directory is reported (web-2b)', () => {
  for (const earlier of ['web-1', 'web-2a']) {
    const body = jobBody('ui');
    const old = body.replaceAll('target/ui-screens/web-2b', `target/ui-screens/${earlier}`);
    assert.notEqual(old, body, 'fixture sanity: the web-2b directory');
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => old)));
    assert.ok(
      problems.some((p) => p.includes('"ui"') && p.includes('path: target/ui-screens/web-2b')),
      `${earlier}: ${problems.join('\n')}`,
    );
  }
});

test('neither the checker nor the fixture names an earlier screenshot directory (web-2b)', () => {
  const checker = readFileSync(SCRIPT_PATH, 'utf8');
  for (const earlier of ['target/ui-screens/web-1', 'target/ui-screens/web-2a']) {
    assert.ok(!checker.includes(earlier), `check-ci-workflow.mjs still names ${earlier}`);
    assert.ok(!GOOD.includes(earlier), `GOOD still names ${earlier}`);
  }
});

for (const [job, { timeout }] of Object.entries(BROWSER_WEB_JOBS)) {
  test(`a ${job} job off its derived budget is reported (web-2c)`, () => {
    const body = jobBody(job);
    const off = body.replace(`    timeout-minutes: ${timeout}\n`, `    timeout-minutes: ${timeout - 5}\n`);
    assert.notEqual(off, body, 'fixture sanity: the derived budget');
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => off)));
    assert.ok(problems.some((p) => p.includes(`"${job}"`) && p.includes(`timeout-minutes: ${timeout}`)), problems.join('\n'));
  });
}

// web-2c task 11 (L-CI-22): the seven new story families at both viewports, both upload paths and the budget.
test('web-2c captures, both paths and timeout are required independently', () => {
  const desktop = 'node packages/ui/scripts/shoot-stories.mjs 6027 target/ui-screens/web-2c/desktop trust-safetycompare-- trust-trustalert-- search-searchpalette-- status-typingindicator-- settings-diagnosticspanel-- shell-tabhand-over-- screens-trustandpolish--';
  const phone = 'node packages/ui/scripts/shoot-stories.mjs 6027 target/ui-screens/web-2c/phone trust-safetycompare-- trust-trustalert-- search-searchpalette-- status-typingindicator-- settings-diagnosticspanel-- shell-tabhand-over-- screens-trustandpolish-- --viewport=360x740';
  const required = [
    ['desktop step', GOOD.replace(`      - run: ${desktop}\n`, '')],
    ['phone step', GOOD.replace(`      - run: ${phone}\n`, '')],
    ['desktop path', GOOD.replace('target/ui-screens/web-2c/desktop/**', 'target/ui-screens/web-2b/desktop/**')],
    ['phone path', GOOD.replace('target/ui-screens/web-2c/phone/**', 'target/ui-screens/web-2b/phone/**')],
    ['timeout', GOOD.replace(`timeout-minutes: ${BUDGET}`, `timeout-minutes: ${BUDGET - 1}`)],
  ];
  for (const [name, workflow] of required) {
    const problems = checkWorkflow(fixture(workflow));
    assert.ok(problems.length > 0, `${name}: checker accepted a missing gate`);
    assert.match(problems.join('\n'), /web-2c|screenshot|browser-web|timeout/i, name);
  }
});

// web-3 task 11: the voice, ring and media-player families at both viewports, uploaded from target/ui-screens/web-3.
test('web-3 captures and the web-3 upload path are required independently', () => {
  const desktop = 'node packages/ui/scripts/shoot-stories.mjs 6038 target/ui-screens/web-3/desktop voice- media-mediaplayer-- screens-voice--';
  const phone = 'node packages/ui/scripts/shoot-stories.mjs 6038 target/ui-screens/web-3/phone voice- media-mediaplayer-- screens-voice-- --viewport=360x740';
  assert.deepEqual(checkWorkflow(fixture(GOOD)), [], 'fixture sanity: GOOD passes');
  const required = [
    ['desktop step', GOOD.replace(`      - run: ${desktop}\n`, ''), desktop],
    ['phone step', GOOD.replace(`      - run: ${phone}\n`, ''), phone],
    ['narrowed prefixes', GOOD.replace(` voice- media-mediaplayer-- screens-voice--\n`, ' voice-\n'), desktop],
    ['upload path', GOOD.replace('path: target/ui-screens/web-3/**', 'path: target/ui-screens/web-2c/**'), 'path: target/ui-screens/web-3/**'],
    ['artifact name', GOOD.replace('name: ui-screens-web-3\n', 'name: ui-screens-web-2d\n'), 'name: ui-screens-web-3'],
  ];
  for (const [name, workflow, needle] of required) {
    assert.notEqual(workflow, GOOD, `fixture sanity: ${name}`);
    const problems = checkWorkflow(fixture(workflow));
    assert.ok(problems.some((p) => p.includes('"ui"') && p.includes(needle)), `${name}: ${problems.join('\n')}`);
  }
});

// Ruling of 2026-10-13 (web-2c D5 in CI), hardened after the d3429ce scanner lead: every wasm32-unknown-unknown
// build compiles SQLite with one exact clang 23 build, and the checker compares whole blocks, not substrings.
/** GOOD with one job's body passed through `edit`. */
function editJob(job, edit) {
  const body = jobBody(job);
  const edited = edit(body);
  assert.notEqual(edited, body, `fixture sanity: the edit of ${job} changed nothing`);
  return GOOD.replace(body, edited);
}

const STEP_RULE = (job) =>
  `${fileOfJob(job)}: job "${job}" builds for wasm32-unknown-unknown; its second to fourth steps, right after actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7, must be exactly the pinned clang 23 steps (WASM_CLANG_STEPS)`;
const ENV_RULE = (job) =>
  `${fileOfJob(job)}: job "${job}" builds for wasm32-unknown-unknown; its job-level env must be exactly:\n${WASM_CLANG_ENV_BLOCK}`;

const LLVM_PACKAGES = ['clang-23', 'llvm-23', 'libllvm23', 'libclang-cpp23', 'libclang1-23', 'libclang-common-23-dev', 'llvm-23-linker-tools', 'llvm-23-runtime'];

/** The install script the canonical steps run: the pin, the fingerprint check and the read-back live there now. */
const CLANG_SCRIPT = readFileSync(new URL('./ci-llvm.sh', import.meta.url), 'utf8');

test('the canonical steps restore the cached build, run the install script and save a build the cache lacked', () => {
  assert.equal(WASM_CLANG_STEPS.length, 3);
  assert.equal(WASM_CLANG_STEPS[1], '      - id: clang23\n        name: clang 23 for the wasm C build\n        run: bash scripts/ci-llvm.sh install');
  assert.match(WASM_CLANG_STEPS[0], /^      - id: clang23-cache\n        uses: actions\/cache\/restore@[0-9a-f]{40} # v[\d.]+\n/);
  assert.ok(WASM_CLANG_STEPS[0].includes('          path: ~/.cache/llvm-23-debs\n'));
  assert.ok(WASM_CLANG_STEPS[0].includes('          restore-keys: llvm-23-debs-${{ runner.arch }}-ubuntu-24.04-'));
  assert.match(WASM_CLANG_STEPS[2], /^      - if: steps\.clang23\.outputs\.changed == 'true'\n        uses: actions\/cache\/save@[0-9a-f]{40} # v[\d.]+\n/);
  assert.ok(WASM_CLANG_STEPS[2].includes('          key: llvm-23-debs-${{ runner.arch }}-ubuntu-24.04-${{ steps.clang23.outputs.build }}'));
  assert.equal(WASM_CLANG_BLOCK, WASM_CLANG_STEPS.join('\n'));
});

test('the install script pins every llvm-toolchain-23 package at one version and reads each back', () => {
  assert.ok(CLANG_SCRIPT.includes('v=$(apt-cache policy clang-23 | awk \'/Candidate:/ { print $2 }\')'));
  assert.match(WASM_LLVM_VERSION, /^23\.\d+$/);
  assert.ok(CLANG_SCRIPT.includes(`case "$v" in 1:${WASM_LLVM_VERSION}.*~++*) ;; *) echo "expected clang ${WASM_LLVM_VERSION}.x candidate, got $v" >&2; exit 1 ;; esac`), 'the signed apt source candidate must stay on LLVM 23.1');
  const install = CLANG_SCRIPT.split('\n').find((l) => l.includes('apt-get install') && l.includes('clang-23='));
  for (const p of LLVM_PACKAGES) {
    assert.ok(install.includes(` "${p}=$v"`), `${p} is not installed at the pinned version`);
  }
  assert.ok(!/\s(clang-23|llvm-23)(\s|$)/.test(install), 'an unpinned package name in the install line');
  assert.ok(CLANG_SCRIPT.includes(`for p in ${LLVM_PACKAGES.join(' ')}; do test "$(dpkg-query -W -f='\${Version}' "$p")" = "$v"; done`));
  assert.ok(CLANG_SCRIPT.includes(`case "$v" in 1:${WASM_LLVM_VERSION}.*~++*) ;; *) echo "expected an installed clang ${WASM_LLVM_VERSION}.x build, got $v" >&2; exit 1 ;; esac`), 'the cached build is read back against the same pin');
  assert.ok(CLANG_SCRIPT.includes(`"$bin/clang-23" --version | grep -F "clang version ${WASM_LLVM_VERSION}."`));
  assert.ok(CLANG_SCRIPT.includes('bin="${LLVM_BIN:-/usr/bin}"'), 'the compiler is read back by its absolute path');
  assert.ok(CLANG_SCRIPT.includes('apt-cache policy clang-23 llvm-23'));
  assert.ok(CLANG_SCRIPT.includes('packages=(' + LLVM_PACKAGES.join(' ') + ')'), 'the cached build is the same eight packages');
});

test('the install script checks the fingerprint on the keyring apt is pointed at', () => {
  const lines = CLANG_SCRIPT.split('\n').map((l) => l.trim());
  assert.ok(lines.includes(`fingerprint=${WASM_LLVM_KEY_FPR}`));
  assert.ok(lines.includes('keyring="${LLVM_KEYRING:-/usr/share/keyrings/apt-llvm.gpg}"'));
  const dearmor = lines.indexOf('gpg --dearmor -o "$temp/apt-llvm.gpg" "$temp/llvm-snapshot.asc"');
  const check = lines.indexOf('test "$primaries" = "$fingerprint"');
  const install = lines.indexOf('sudo install -m 0644 "$temp/apt-llvm.gpg" "$keyring"');
  const same = lines.indexOf('cmp "$temp/apt-llvm.gpg" "$keyring"');
  const source = lines.findIndex((l) => l.includes('[signed-by=$keyring]'));
  assert.ok(dearmor !== -1 && dearmor < check && check < install && install < same && same < source, lines.join('\n'));
  assert.ok(lines[check - 1].startsWith('primaries=$(gpg --show-keys --with-colons "$temp/apt-llvm.gpg"'));
  assert.ok(WASM_CLANG_ENV_BLOCK.includes('CC_wasm32_unknown_unknown: /usr/bin/clang-23'));
  assert.ok(WASM_CLANG_ENV_BLOCK.includes('AR_wasm32_unknown_unknown: /usr/bin/llvm-ar-23'));
});

for (const job of WASM_JOBS) {
  test(`a ${job} job without the clang 23 job env is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace(`${WASM_CLANG_ENV_BLOCK}\n`, ''))));
    assert.ok(problems.includes(ENV_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job whose CC is a bare clang-23 that PATH resolves is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace(': /usr/bin/clang-23', ': clang-23'))));
    assert.ok(problems.includes(ENV_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job env with an extra compiler flag is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace(WASM_CLANG_ENV_BLOCK, `${WASM_CLANG_ENV_BLOCK}\n      CFLAGS_wasm32_unknown_unknown: -O0`))));
    assert.ok(problems.includes(ENV_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job running the install script with another argument is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace('bash scripts/ci-llvm.sh install', 'bash scripts/ci-llvm.sh install --any'))));
    assert.ok(problems.includes(STEP_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job restoring another cache is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace('~/.cache/llvm-23-debs', '~/.cache/llvm-24-debs'))));
    assert.ok(problems.includes(STEP_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job saving the cache unconditionally is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace("      - if: steps.clang23.outputs.changed == 'true'\n        uses: actions/cache/save", '      - uses: actions/cache/save'))));
    assert.ok(problems.includes(STEP_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job that skips the clang 23 step with an if: is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace('        name: clang 23 for the wasm C build\n', '        name: clang 23 for the wasm C build\n        if: false\n'))));
    assert.ok(problems.includes(STEP_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job that installs clang 23 after other steps is reported`, () => {
    const moved = editJob(job, (b) => {
      const without = b.replace(`${WASM_CLANG_BLOCK}\n`, '');
      return `${without.replace(/\n*$/, '')}\n${WASM_CLANG_BLOCK}\n`;
    });
    const problems = checkWorkflow(fixture(moved));
    assert.ok(problems.includes(STEP_RULE(job)), problems.join('\n'));
  });
}

// The validator differential of the d3429ce lead: the earlier rule matched substrings, so this job passed it while
// running no install at all, with the required strings in a comment and in an echo.
test('an evading job that only names the pinned strings in a comment and an echo is reported', () => {
  const evading = editJob('web-build', (b) =>
    b.replace(
      `${WASM_CLANG_BLOCK}\n`,
      `      # ${WASM_CLANG_BLOCK.split('\n').map((l) => l.trim()).join(' ; ')}\n` +
        `      - run: echo '${WASM_CLANG_BLOCK.split('\n').filter((l) => l.includes('ci-llvm.sh')).join('').trim()}'\n`,
    ),
  );
  const problems = checkWorkflow(fixture(evading));
  assert.ok(problems.includes(STEP_RULE('web-build')), problems.join('\n'));
});

for (const [what, edit, expected] of [
  [
    'the hyphenated target variable on a step, which cc reads before the underscored one',
    (b) => b.replace('      - run: wasm-pack build', '      - env:\n          CC_wasm32-unknown-unknown: /usr/bin/gcc\n        run: wasm-pack build'),
    /names a C toolchain variable outside the canonical clang 23 job env/,
  ],
  [
    'a TARGET_CC in a flow-style step env',
    (b) => b.replace('      - run: wasm-pack build', '      - env: { TARGET_CC: gcc }\n        run: wasm-pack build'),
    /names a C toolchain variable outside the canonical clang 23 job env/,
  ],
  [
    'a quoted CC key on a step',
    (b) => b.replace('      - run: wasm-pack build', '      - env:\n          "CC": gcc\n        run: wasm-pack build'),
    /names a C toolchain variable outside the canonical clang 23 job env/,
  ],
  [
    'a step that exports the compiler through GITHUB_ENV',
    (b) => b.replace('      - run: wasm-pack build', '      - run: echo "CC_wasm32_unknown_unknown=/usr/bin/gcc" >> "$GITHUB_ENV"\n      - run: wasm-pack build'),
    /writes GITHUB_ENV or GITHUB_PATH/,
  ],
  [
    'a step that puts another clang first on PATH',
    (b) => b.replace('      - run: wasm-pack build', '      - run: echo "$RUNNER_TEMP/fake" >> "$GITHUB_PATH"\n      - run: wasm-pack build'),
    /writes GITHUB_ENV or GITHUB_PATH/,
  ],
]) {
  test(`web-build with ${what} is reported`, () => {
    const problems = checkWorkflow(fixture(editJob('web-build', edit)));
    assert.ok(problems.some((p) => expected.test(p)), problems.join('\n'));
  });
}

test('a compiler variable in the workflow-level env is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('env:\n  CARGO_TERM_COLOR: always\n', 'env:\n  CARGO_TERM_COLOR: always\n  CC: gcc\n')));
  assert.ok(problems.some((p) => /names a C toolchain variable outside the canonical clang 23 job env/.test(p)), problems.join('\n'));
});

test('a YAML anchor and alias that would reuse an env block is reported', () => {
  const anchored = GOOD.replace('env:\n  CARGO_TERM_COLOR: always\n', 'env: &shared\n  CARGO_TERM_COLOR: always\n').replace(
    '  deny:\n    runs-on: ubuntu-latest\n',
    '  deny:\n    runs-on: ubuntu-latest\n    env: *shared\n',
  );
  const problems = checkWorkflow(fixture(anchored));
  assert.ok(problems.some((p) => p.includes('YAML anchors, aliases and merge keys are not allowed')), problems.join('\n'));
});

test('a wasm job added tomorrow without the pinned clang 23 is reported', () => {
  const extra =
    '  wasm-extra:\n' +
    '    runs-on: ubuntu-latest\n' +
    '    steps:\n' +
    '      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7\n' +
    '      - run: cargo build -p dilla-core --target wasm32-unknown-unknown --locked\n';
  const problems = checkWorkflow(fixture(GOOD.replace('  browser-spike:\n', `${extra}  browser-spike:\n`)));
  assert.ok(problems.includes(ENV_RULE('wasm-extra')), problems.join('\n'));
  assert.ok(problems.includes(STEP_RULE('wasm-extra')), problems.join('\n'));
});

test('jobs that build no wasm need no clang 23', () => {
  assert.deepEqual(checkWorkflow(fixture(GOOD)), []);
});

// install-release (2026-10-14): the release workflow, checked against the real CI workflows it must match.
function releaseFixture(release) {
  const dir = mkdtempSync(join(TEST_ROOT, 'dilla-release-'));
  mkdirSync(join(dir, '.github', 'workflows'), { recursive: true });
  addE2ePackage(dir);
  writeFileSync(join(dir, '.github', 'workflows', 'server.yml'), REAL_SERVER);
  writeFileSync(join(dir, '.github', 'workflows', 'desktop-ci.yml'), REAL_DESKTOP_CI);
  writeFileSync(join(dir, '.github', 'workflows', 'release.yml'), release);
  return dir;
}

test('go-release must refuse a dilla_unsafe_test dillad in both workflows', () => {
  const step = 'if go version -m dist/dillad-linux-amd64 dist/dillad-linux-arm64 | grep -q dilla_unsafe_test; then echo "a dillad was built with the dilla_unsafe_test tag"; exit 1; fi';
  assert.deepEqual(checkWorkflow(fixture(GOOD)), [], 'fixture sanity: GOOD passes');
  const problems = checkWorkflow(fixture(GOOD.replace(step, 'true')));
  assert.ok(problems.some((p) => p.includes('go-release') && p.includes('dilla_unsafe_test')), problems.join('\n'));
  assertReported(releaseProblems((t) => t.replace(step, 'true')), 'dilla_unsafe_test');
});

test('go-release must refuse a dilla_testhost dillad in both workflows', () => {
  const step = 'if go version -m dist/dillad-linux-amd64 dist/dillad-linux-arm64 | grep -q dilla_testhost; then echo "a dillad was built with the dilla_testhost tag"; exit 1; fi';
  assert.deepEqual(checkWorkflow(fixture(GOOD)), [], 'fixture sanity: GOOD passes');
  const problems = checkWorkflow(fixture(GOOD.replace(step, 'true')));
  assert.ok(problems.some((p) => p.includes('go-release') && p.includes('dilla_testhost')), problems.join('\n'));
  assertReported(releaseProblems((t) => t.replace(step, 'true')), 'dilla_testhost');
});

test('bot-release must refuse a dilla_testhost dillabot', () => {
  const step = 'if go version -m dist/dillabot-linux-${{ matrix.arch }} | grep -q dilla_testhost; then echo "a dillabot was built with the dilla_testhost tag"; exit 1; fi';
  assertReported(releaseProblems((t) => t.replace(step, 'true')), 'dilla_testhost');
  // The build info must also be present, or an absent tag proves nothing.
  const info = "go version -m dist/dillabot-linux-${{ matrix.arch }} | grep -q 'build[[:space:]]CGO_ENABLED=0'";
  assertReported(releaseProblems((t) => t.replace(info, 'true')), 'CGO_ENABLED=0');
});

function releaseProblems(edit) {
  const edited = edit(REAL_RELEASE);
  assert.notEqual(edited, REAL_RELEASE, 'the edit changed nothing; the fixture drifted from release.yml');
  return checkReleaseWorkflow(releaseFixture(edited));
}

function assertReported(problems, needle) {
  assert.ok(problems.some((p) => p.includes(needle)), `want a problem naming ${needle}:\n${problems.join('\n')}`);
}

test('the real release workflow passes every rule', () => {
  assert.deepEqual(checkReleaseWorkflow(REPO_ROOT), []);
});

// 2026-11-06 "Draft, sign on main": release.yml, the tag's own file, leaves the release a draft and holds no
// environment or secret; mobile-release.yml (main's file) signs, attaches and publishes (review H1, M1).
test('release.yml creates a draft', () => {
  assertReported(releaseProblems((t) => t.replace(' --verify-tag --draft ', ' --verify-tag ')), '--draft');
});

test('an environment, a secret, a status if: or a checkout ref in release.yml is reported', () => {
  assertReported(releaseProblems((t) => t.replace('  rust-wasi:\n    runs-on: ubuntu-latest\n', '  rust-wasi:\n    runs-on: ubuntu-latest\n    environment: mobile-release\n')), 'enters an environment');
  assertReported(releaseProblems((t) => t.replace('          GH_TOKEN: ${{ github.token }}\n          TAG: ${{ needs.tag.outputs.tag }}\n', '          GH_TOKEN: ${{ github.token }}\n          TAG: ${{ needs.tag.outputs.tag }}\n          K: ${{ secrets.ANDROID_KEY_PASSWORD }}\n')), 'reads no secret');
  assertReported(releaseProblems((t) => t.replace('  release:\n    runs-on: ubuntu-latest\n', "  release:\n    if: ${{ !cancelled() }}\n    runs-on: ubuntu-latest\n")), 'status check');
  assertReported(releaseProblems((t) => t.replace('  release:\n    runs-on: ubuntu-latest\n', '  release:\n    if: success() || failure()\n    runs-on: ubuntu-latest\n')), 'status check');
  assertReported(releaseProblems((t) => t.replace('  rust-wasi:\n    runs-on: ubuntu-latest\n    needs: [tag]\n    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7\n', '  rust-wasi:\n    runs-on: ubuntu-latest\n    needs: [tag]\n    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7\n        with:\n          ref: main\n')), 'checks out ref main');
});

const REAL_MOBILE_RELEASE = readFileSync(join(REPO_ROOT, '.github', 'workflows', 'mobile-release.yml'), 'utf8');
function mobileReleaseProblems(edit) {
  const edited = edit(REAL_MOBILE_RELEASE);
  assert.notEqual(edited, REAL_MOBILE_RELEASE, 'the edit changed nothing; the fixture drifted from mobile-release.yml');
  const dir = mkdtempSync(join(TEST_ROOT, 'dilla-mobile-release-'));
  mkdirSync(join(dir, '.github', 'workflows'), { recursive: true });
  writeFileSync(join(dir, '.github', 'workflows', 'mobile-release.yml'), edited);
  return checkMobileReleaseWorkflow(dir);
}
const DRAFT_IF = "    if: needs.resolve.outputs.draft == 'true'\n";
const ANDROID_HEAD = '  android-release:\n    needs: [resolve, mobile-bundle]\n';
const PUBLISH_HEAD = '  publish:\n    needs: [resolve, android-release]\n';
const IOS_HEAD = '  ios-testflight:\n    needs: [resolve, mobile-bundle]\n';

test('the real mobile-release workflow passes every rule', () => {
  assert.deepEqual(checkMobileReleaseWorkflow(REPO_ROOT), []);
});

test('the Android jobs carry exactly the draft if:, which keeps their needs; the real file passes with it', () => {
  assert.ok(REAL_MOBILE_RELEASE.includes(ANDROID_HEAD + DRAFT_IF) && REAL_MOBILE_RELEASE.includes(PUBLISH_HEAD + DRAFT_IF), 'fixture sanity: both jobs carry the draft condition');
  assert.deepEqual(checkMobileReleaseWorkflow(REPO_ROOT), []);
  assertReported(mobileReleaseProblems((t) => t.replace(ANDROID_HEAD + DRAFT_IF, ANDROID_HEAD)), 'job "android-release" must carry exactly if:');
});

test('a job-level if: that runs a secret or publishing job past its needs is reported', () => {
  assertReported(mobileReleaseProblems((t) => t.replace(ANDROID_HEAD + DRAFT_IF, `${ANDROID_HEAD}    if: always()\n`)), 'job "android-release" must carry exactly if:');
  assertReported(mobileReleaseProblems((t) => t.replace(ANDROID_HEAD + DRAFT_IF, `${ANDROID_HEAD}    if: \${{ !cancelled() && needs.resolve.outputs.draft == 'true' }}\n`)), 'job "android-release" must carry exactly if:');
  assertReported(mobileReleaseProblems((t) => t.replace(PUBLISH_HEAD + DRAFT_IF, `${PUBLISH_HEAD}    if: success() || failure()\n`)), 'job "publish" must carry exactly if:');
  assertReported(mobileReleaseProblems((t) => t.replace('  mobile-bundle:\n    needs: [resolve]\n', '  mobile-bundle:\n    needs: [resolve]\n    if: always()\n')), 'job "mobile-bundle" has a job-level if:');
  assertReported(mobileReleaseProblems((t) => t.replace(IOS_HEAD, `${IOS_HEAD}    if: always()\n`)), 'job "ios-testflight" has a job-level if:');
});

test('a secret outside the signing and export steps is reported', () => {
  assertReported(mobileReleaseProblems((t) => t.replace('          DILLA_ANDROID_VERSION_CODE: ${{ steps.build.outputs.number }}\n', '          DILLA_ANDROID_VERSION_CODE: ${{ steps.build.outputs.number }}\n          KS: ${{ secrets.ANDROID_KEYSTORE_PASSWORD }}\n')), 'names a secret it may not');
  assertReported(mobileReleaseProblems((t) => t.replace('\n      TAG: ${{ needs.resolve.outputs.tag }}\n      GH_TOKEN: ${{ github.token }}\n', '\n      TAG: ${{ needs.resolve.outputs.tag }}\n      GH_TOKEN: ${{ github.token }}\n      K: ${{ secrets.ANDROID_KEY_PASSWORD }}\n')), 'names a secret at job level');
  // The export step may hold the App Store Connect API key, not the Android keystore.
  assertReported(mobileReleaseProblems((t) => t.replace('          APPLE_API_KEY: ${{ secrets.APPLE_API_KEY }}\n          APPLE_API_KEY_ID: ${{ secrets.APPLE_API_KEY_ID }}\n', '          APPLE_API_KEY: ${{ secrets.APPLE_API_KEY }}\n          APPLE_API_KEY_ID: ${{ secrets.APPLE_API_KEY_ID }}\n          KS: ${{ secrets.ANDROID_KEYSTORE_B64 }}\n')), 'names a secret it may not');
});

test('a commented-out gate line does not count (review L2)', () => {
  // A whole comment line, and a gate pushed into a trailing comment after a no-op.
  assertReported(mobileReleaseProblems((t) => t.replace('          git merge-base --is-ancestor "$sha" origin/main ||', '          # git merge-base --is-ancestor "$sha" origin/main ||')), 'git merge-base --is-ancestor "$sha" origin/main');
  assertReported(mobileReleaseProblems((t) => t.replace('          [ "$RUN_REF" = refs/heads/main ] ||', '          true # [ "$RUN_REF" = refs/heads/main ] ||')), '[ "$RUN_REF" = refs/heads/main ]');
  assertReported(mobileReleaseProblems((t) => t.replace('          [ "$RUN_REF" = refs/heads/main ] ||', '          # [ "$RUN_REF" = refs/heads/main ] ||')), '[ "$RUN_REF" = refs/heads/main ]');
});

test('a workflow_run gate without the push event or the tag-to-head_sha match is reported (review L1)', () => {
  assertReported(mobileReleaseProblems((t) => t.replace('[ "$RUN_EVENT" = push ]', 'true')), '[ "$RUN_EVENT" = push ]');
  assertReported(mobileReleaseProblems((t) => t.replace('[ "$sha" = "$RUN_SHA" ]', 'true')), '[ "$sha" = "$RUN_SHA" ]');
});

test('a publish that trusts the artifact alone is reported (review L3)', () => {
  assertReported(mobileReleaseProblems((t) => t.replace('          digest-mismatch: error\n', '')), 'digest-mismatch');
  assertReported(mobileReleaseProblems((t) => t.replace(`          printf '%s  %s\\n' "$APK_SHA256" "$apk" | sha256sum -c --strict\n`, '')), '"$APK_SHA256" "$apk" | sha256sum -c --strict');
  assertReported(mobileReleaseProblems((t) => t.replace('          APK_SHA256: ${{ needs.android-release.outputs.apk_sha256 }}\n', '          APK_SHA256: ${{ needs.resolve.outputs.sha }}\n')), 'needs.android-release.outputs.apk_sha256');
});

test('toJSON(secrets) anywhere is reported', () => {
  assertReported(mobileReleaseProblems((t) => t.replace('\n      TAG: ${{ needs.resolve.outputs.tag }}\n      GH_TOKEN: ${{ github.token }}\n', '\n      TAG: ${{ needs.resolve.outputs.tag }}\n      GH_TOKEN: ${{ github.token }}\n      ALL: ${{ toJSON(secrets) }}\n')), 'toJSON(secrets)');
});

test('a checkout of anything but the gated commit in a secret job, or any checkout in publish, is reported', () => {
  const at = REAL_MOBILE_RELEASE.indexOf(ANDROID_HEAD);
  const edited = (t) => t.slice(0, at) + t.slice(at).replace('          ref: ${{ needs.resolve.outputs.sha }}\n', '          ref: ${{ github.event.workflow_run.head_branch }}\n');
  assertReported(mobileReleaseProblems(edited), 'job "android-release" must check out ref');
  assertReported(mobileReleaseProblems((t) => t.replace(`${PUBLISH_HEAD}`, PUBLISH_HEAD).replace('    steps:\n      - name: The release is a draft\n', '    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7\n      - name: The release is a draft\n')), 'job "publish" checks out the repository');
});

test('a key file whose trap leaves the step that writes it is reported', () => {
  assertReported(mobileReleaseProblems((t) => t.replace(`          trap 'rm -f "$ks"' EXIT\n`, '').replace('        run: npm run android:check-signer -w @dilla/mobile\n', `        run: |\n          trap 'rm -f "$ks"' EXIT\n          npm run android:check-signer -w @dilla/mobile\n`)), 'the key file is written in one step only');
  assertReported(mobileReleaseProblems((t) => t.replace(`          trap 'rm -f "$RUNNER_TEMP/asc-key.p8"' EXIT\n`, '')), 'the key file is written in one step only');
});

test('an environment or write access outside their jobs, or a publish that skips the APK, is reported', () => {
  assertReported(mobileReleaseProblems((t) => t.replace(PUBLISH_HEAD + DRAFT_IF, `${PUBLISH_HEAD + DRAFT_IF}    environment: mobile-release\n`)), 'job "publish" enters an environment');
  assertReported(mobileReleaseProblems((t) => t.replace(ANDROID_HEAD + DRAFT_IF, `${ANDROID_HEAD + DRAFT_IF}    permissions:\n      contents: write\n`)), 'only publish writes');
  assertReported(mobileReleaseProblems((t) => t.replace(PUBLISH_HEAD, '  publish:\n    needs: [resolve]\n')), 'job "publish" must list "android-release"');
  assertReported(mobileReleaseProblems((t) => t.replace('            echo "draft=$draft"\n', '')), 'echo "draft=$draft"');
  assertReported(mobileReleaseProblems((t) => t.replace('[ "$RUN_REF" = refs/heads/main ]', 'true')), 'refs/heads/main');
  assertReported(mobileReleaseProblems((t) => t.replace('[ "$draft" = true ]', 'true')), '[ "$draft" = true ]');
});


test('a release workflow without the release job is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/\n {2}release:\n[\s\S]*$/, '\n')), 'missing job "release"');
});

test('a release without the native Bot build is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/\n {2}bot-release:\n[\s\S]*?\n {2}go-release:/, '\n  go-release:')), 'bot-release');
});

test('a Bot release without a stamped and checked version is reported', () => {
  const unstamped = REAL_RELEASE.replace(' -X main.Version=${VERSION}', '');
  assertReported(releaseProblems(() => unstamped), 'Bot binary says its version');
});

test('a release checksum that omits Bot binaries is reported', () => {
  assertReported(
    releaseProblems((t) => t.replace(`sha256sum ${RELEASE_ASSETS.join(' ')} > SHA256SUMS`,
      'sha256sum dillad-linux-amd64 dillad-linux-arm64 dilla_core_wasi.wasm install.sh > SHA256SUMS')),
    '> SHA256SUMS',
  );
});

test('a release that no longer attaches install.sh is reported', () => {
  assertReported(releaseProblems((t) => t.replace('release/install.sh release/install-bot.sh release/SHA256SUMS', 'release/install-bot.sh release/SHA256SUMS')), 'release/install.sh');
});

test('a release that no longer attaches install-bot.sh is reported', () => {
  assertReported(releaseProblems((t) => t.replace('release/install-bot.sh release/SHA256SUMS', 'release/SHA256SUMS')), 'release/install-bot.sh');
});

test('SHA256SUMS that leaves an asset out is reported', () => {
  assertReported(
    releaseProblems((t) => t.replace(`sha256sum ${RELEASE_ASSETS.join(' ')} > SHA256SUMS`, 'sha256sum dillad-linux-amd64 dillad-linux-arm64 > SHA256SUMS')),
    '> SHA256SUMS',
  );
});

test('a release job without contents: write is reported', () => {
  assertReported(releaseProblems((t) => t.replace('    permissions:\n      contents: write\n', '')), 'contents: write');
});

test('contents: write on a build job is reported', () => {
  assertReported(releaseProblems((t) => t.replace('  rust-wasi:\n    runs-on: ubuntu-latest\n', '  rust-wasi:\n    runs-on: ubuntu-latest\n    permissions:\n      contents: write\n')), 'only the release job writes');
});

test('a container image in the release workflow is reported', () => {
  assertReported(releaseProblems((t) => `${t}\n  image:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: docker/setup-buildx-action@f87e5991a6d7451dcb8d9637bfbc97413f497069 # v4.4.1\n`), 'container image');
});

test('a dispatch input expanded inside a script is reported', () => {
  assertReported(releaseProblems((t) => t.replace('tag="${INPUT_TAG:-$GITHUB_REF_NAME}"', 'tag="${{ inputs.tag }}"')), 'outside an env: key');
});

test('an action at a version the CI workflows do not use is reported', () => {
  assertReported(releaseProblems((t) => t.replace('  rust-wasi:\n    runs-on: ubuntu-latest\n    needs: [tag]\n    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7', '  rust-wasi:\n    runs-on: ubuntu-latest\n    needs: [tag]\n    steps:\n      - uses: actions/checkout@main')), 'actions/checkout@main');
});

test('a tag job that no longer checks the ref is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {10}test "\$GITHUB_REF" = .*\n/, '')), 'refs/tags/$tag');
});

test('a release web-build without the clang 23 pin is reported', () => {
  assertReported(releaseProblems((t) => t.replace('        name: clang 23 for the wasm C build\n', '        name: clang for the wasm C build\n')), 'clang 23');
});

test('a release go-release that builds before the placeholder guard is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {6}- name: The binaries embed the real client[^\n]*\n {8}run: \|\n(?: {10}[^\n]*\n)+/, '')), 'refuse the placeholder');
});

test('a release go-release that does not wait for web-build is reported', () => {
  assertReported(releaseProblems((t) => t.replace('needs: [tag, web-build]', 'needs: [tag]')), '"web-build" in its needs');
});

test('a release Bot build that does not wait for the WASI core is reported', () => {
  assertReported(releaseProblems((t) => t.replace('needs: [tag, rust-wasi]', 'needs: [tag]')), '"rust-wasi" in its needs');
});

test('a release Bot build that omits the WASI core download is reported', () => {
  assertReported(releaseProblems((t) => t.replace('          path: internal/mlswasi/testdata\n', '')), 'path: internal/mlswasi/testdata');
});

test('|| true in the release workflow is reported', () => {
  assertReported(releaseProblems((t) => t.replace('sha256sum -c --strict SHA256SUMS', 'sha256sum -c --strict SHA256SUMS || true')), '|| true');
});

test('a release workflow that no longer runs on v* tags is reported', () => {
  assertReported(releaseProblems((t) => t.replace("tags: ['v*']", "tags: ['*']")), "tags 'v*'");
});

for (const action of ['actions/cache', 'actions/cache/restore']) {
  test(`a release build that restores ${action} is rejected as a build cache`, () => {
    const problems = releaseProblems((t) => t.replace(
      '      - run: rustup show active-toolchain',
      `      - uses: ${action}@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0\n      - run: rustup show active-toolchain`,
    ));
    assertReported(problems, 'a build cache in the release workflow');
  });
}

test('a third-party action pinned by SHA is still rejected in release', () => {
  const problems = releaseProblems((t) => t.replace(
    '      - run: rustup show active-toolchain',
    '      - uses: taiki-e/install-action@1168f6810a3cb30a3514bf5756a3b7f0422c6df8 # v2\n      - run: rustup show active-toolchain',
  ));
  assertReported(problems, 'not a GitHub-owned action');
});

test('a movable GitHub action tag is rejected in release', () => {
  const problems = releaseProblems((t) => t.replace(
    'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7',
    'actions/checkout@v7',
  ));
  assertReported(problems, '40-hex commit SHA');
});

test('a flow-style action step is rejected in release', () => {
  const problems = releaseProblems((t) => t.replace(
    '      - run: rustup show active-toolchain',
    '      - { uses: taiki-e/install-action@1168f6810a3cb30a3514bf5756a3b7f0422c6df8 }\n      - run: rustup show active-toolchain',
  ));
  assertReported(problems, 'flow-style action');
});

test('every release action is owned by GitHub and pinned to a commit', () => {
  const actions = REAL_RELEASE.split('\n').filter((line) => /uses:/.test(line));
  assert.ok(actions.length > 0);
  for (const line of actions) assert.match(line, /uses:\s*actions\/[\w-]+@[0-9a-f]{40} # \S+$/, line);
});

test('a release that attaches an unstamped install.sh is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {10}sed -i "s\/\^DILLA_RELEASE_TAG=.*\n/, '')), 'DILLA_RELEASE_TAG');
});

test('a release that does not check the stamp landed is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {10}test "\$\(grep -c "\^DILLA_RELEASE_TAG=.*\n/, '')), 'grep -c');
});

test('a release that does not refuse an existing release by name is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {10}if gh release view "\$TAG".*\n/, '')), 'gh release view');
});

test('a tag job that releases a commit off main is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {10}git merge-base --is-ancestor .*\n/, '')), 'origin/main');
});

test('a tag job without the server release gate step is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {6}- name: The tagged commit has a green server run on main\n(?: {8,}.*\n)+/, '')), 'green server run');
  assertReported(releaseProblems((t) => t.replace('node scripts/server-release-gate.mjs "$TAG_SHA"', 'node scripts/server-release-gate.mjs "${{ github.sha }}"')), 'server-release-gate');
});

test('a tag job without actions: read, or another job with actions access, is reported', () => {
  assertReported(releaseProblems((t) => t.replace('      contents: read\n      actions: read\n', '      contents: read\n')), 'actions: read');
  assertReported(releaseProblems((t) => t.replace('  web-build:\n    runs-on: ubuntu-24.04\n', '  web-build:\n    runs-on: ubuntu-24.04\n    permissions:\n      contents: read\n      actions: read\n')), 'only the tag job reads workflow runs');
});

test('a build cache in the release workflow is reported', () => {
  assertReported(releaseProblems((t) => t.replace('          node-version-file: .nvmrc\n', '          node-version-file: .nvmrc\n          cache: npm\n')), 'build cache');
});

test('a release setup-go without cache: false is reported', () => {
  assertReported(releaseProblems((t) => t.replace('          cache: false\n', '')), 'setup-go caches by default');
});

// v0.1.0-web3.1: the release stamps its tag (without the leading v) into dillad at link time; the CI workflows' builds never do.
test('a release build without the version stamp is reported', () => {
  assertReported(releaseProblems((t) => t.replace(' -X main.Version=${VERSION}" -o dist/dillad-linux-arm64', '" -o dist/dillad-linux-arm64')),
    'go build without -X main.Version=${VERSION}');
});

test('an extra release go build without the version stamp is reported', () => {
  const extra = "      - name: linux/riscv64\n        run: CGO_ENABLED=0 GOOS=linux GOARCH=riscv64 go build -trimpath -ldflags='-s -w' -o dist/dillad-linux-riscv64 ./cmd/dillad\n";
  assertReported(releaseProblems((t) => t.replace('      - name: linux/arm64\n', `${extra}      - name: linux/arm64\n`)),
    'go build without -X main.Version=${VERSION}');
});

test('a release that does not check the stamped version is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {10}test "\$\(dist\/dillad-linux-amd64 version .*\n/, '')), 'dillad-linux-amd64 version');
});

test('a release build whose version does not come from the tag job is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/( {6}- name: linux\/arm64\n {8}env:\n {10}VERSION: )\$\{\{ needs\.tag\.outputs\.version \}\}/, '$1${{ github.ref_name }}')),
    'uses the version without its own env VERSION: ${{ needs.tag.outputs.version }}');
});

test('a release whose tag job does not output the version is reported', () => {
  assertReported(releaseProblems((t) => t.replace('      version: ${{ steps.tag.outputs.version }}\n', '')), 'must output version');
  assertReported(releaseProblems((t) => t.replace('          echo "version=${tag#v}" >> "$GITHUB_OUTPUT"\n', '')), 'is missing: echo "version=${tag#v}"');
});

test('a CI build that stamps a version is reported', () => {
  const stamped = GOOD.replace("go build -trimpath -ldflags='-s -w' ./cmd/dillad", "go build -trimpath -ldflags='-s -w -X main.Version=1.2.3' ./cmd/dillad");
  assert.notEqual(stamped, GOOD);
  const problems = checkWorkflow(fixture(stamped));
  assert.ok(problems.some((p) => p.includes('main.Version')), problems.join('\n'));
});

// Task 3 m2: a quoted symbol after -X, and the -X= spelling, stamp a version all the same.
test('a CI build that stamps a quoted version is reported', () => {
  for (const flag of ["-X 'main.Version=1.2.3'", '-X "main.Version=1.2.3"', '-X=main.Version=1.2.3', "-X='main.Version=1.2.3'"]) {
    const stamped = GOOD.replace("go build -trimpath -ldflags='-s -w' ./cmd/dillad", `go build -trimpath -ldflags="-s -w ${flag}" ./cmd/dillad`);
    assert.notEqual(stamped, GOOD);
    const problems = checkWorkflow(fixture(stamped));
    assert.ok(problems.some((p) => p.includes('stamps main.Version')), `${flag}\n${problems.join('\n')}`);
  }
});

test('the CLI checks release.yml too', () => {
  const root = releaseFixture(REAL_RELEASE.replace('release/install.sh release/install-bot.sh release/SHA256SUMS', 'release/install-bot.sh release/SHA256SUMS'));
  const cwd = mkdtempSync(join(TEST_ROOT, 'dilla-ci-cwd-'));
  const result = runCli(root, cwd);
  assert.equal(result.status, 1, `stdout: ${result.stdout}\nstderr: ${result.stderr}`);
  assert.match(result.stderr, /release\.yml/);
});

test('a job that lost its change gate is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace("    if: needs.changes.outputs.go != 'false'\n    runs-on: ubuntu-latest\n    timeout-minutes: 20\n", '    runs-on: ubuntu-latest\n    timeout-minutes: 20\n')));
  assert.ok(problems.some((p) => p.includes('must carry the job-level if:')), problems.join('\n'));
});

test('a job that does not need changes is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/(  deny:\n)    needs: \[changes\]\n/, '$1')));
  assert.ok(problems.some((p) => p.includes('"deny" must list "changes"')), problems.join('\n'));
});

test('the docs job may not be gated: it is the one that runs for a docs-only change', () => {
  const problems = checkWorkflow(fixture(GOOD.replace('  docs:\n', "  docs:\n    needs: [changes]\n    if: needs.changes.outputs.code != 'false'\n")));
  assert.ok(problems.some((p) => p.includes('"docs" must run for every change')), problems.join('\n'));
});

test('a job added without a gate is reported', () => {
  const problems = checkWorkflow(fixture(`${GOOD}\n  extra:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n`));
  assert.ok(problems.some((p) => p.includes('"extra" has no gate')), problems.join('\n'));
});

test('a gate that can run while a needed job is skipped is reported', () => {
  // go-release (go or web) needs web-build (e2e): fine. Narrow web-build to core and a UI change would skip go-release's dependency.
  const problems = checkWorkflow(fixture(GOOD.replace(/(  web-build:\n(?:    needs:.*\n)?)    if: .*\n/, "$1    if: needs.changes.outputs.core != 'false'\n")));
  assert.ok(problems.some((p) => p.includes('"web-build" must carry')), problems.join('\n'));
});

test('each real ok job needs every other job of its workflow', () => {
  for (const real of [REAL_SERVER, REAL_DESKTOP_CI]) {
    const ok = /^name: (\S+)$/m.exec(real)[1] + '-ok';
    const body = real.slice(real.indexOf(`\n  ${ok}:`));
    const needs = /needs: \[([^\]]*)\]/.exec(body)[1].split(',').map((s) => s.trim());
    const jobs = [...real.slice(real.indexOf('\njobs:')).matchAll(/^ {2}([A-Za-z0-9_-]+):\s*$/gm)].map((m) => m[1]).filter((job) => job !== ok);
    assert.deepEqual(needs.sort(), jobs.sort(), ok);
  }
});

test('each ok job lists every other job of its workflow', () => {
  for (const workflow of ['server', 'desktop']) {
    const { ok } = WORKFLOWS[workflow];
    const [server, desktop] = GOOD.split(SPLIT);
    const edit = (change) => (workflow === 'server' ? change(server) + SPLIT + desktop : server + SPLIT + change(desktop));
    const forgot = edit((part) => part.replace(new RegExp(`(  ${ok}:\\n    needs: \\[[^\\]]*)changes, `), '$1'));
    assert.notEqual(forgot, GOOD);
    let problems = checkWorkflow(fixture(forgot));
    assert.ok(problems.some((p) => p.includes(`"${ok}"`) && p.includes('"changes"')), problems.join('\n'));
    // A job of the other workflow is not one of this run's: GitHub would refuse the workflow.
    const foreign = workflow === 'server' ? 'ui' : 'go-lint';
    problems = checkWorkflow(fixture(edit((part) => part.replace(new RegExp(`(  ${ok}:\\n    needs: \\[)`), `$1${foreign}, `))));
    assert.ok(problems.some((p) => p.includes(`"${ok}" must list every other job of its workflow exactly once`)), problems.join('\n'));
  }
});

test('an ok job without the failure check, or without always(), is reported', () => {
  let problems = checkWorkflow(fixture(GOOD.replace("contains(needs.*.result, 'cancelled')", 'false')));
  assert.ok(problems.some((p) => p.includes('"server-ok"') && p.includes('cancelled')), problems.join('\n'));
  problems = checkWorkflow(fixture(GOOD.replace('    if: always()\n    runs-on: ubuntu-latest\n    steps:\n      - if: contains', '    runs-on: ubuntu-latest\n    steps:\n      - if: contains')));
  assert.ok(problems.some((p) => p.includes('"server-ok"') && p.includes('always()')), problems.join('\n'));
});

test('server.yml must not contain desktop jobs', () => {
  assert.deepEqual(checkServerWorkflow(REPO_ROOT), []);
  const desktopOnly = [...REQUIRED_JOBS.desktop, ...Object.keys(BROWSER_WEB_JOBS)].filter((job) => !REQUIRED_JOBS.server.includes(job));
  assert.deepEqual(desktopOnly.filter((job) => new RegExp(`^  ${job}:$`, 'm').test(REAL_SERVER)), []);
  for (const job of ['ui', 'core-integration']) {
    const body = jobBody(job);
    const [server, desktop] = GOOD.split(SPLIT);
    const moved = server.replace('  deny:\n', () => `${body}\n  deny:\n`) + SPLIT + desktop;
    const problems = checkServerWorkflow(fixture(moved));
    assert.ok(problems.some((p) => p === `server.yml: job "${job}" does not belong to the server workflow (REQUIRED_JOBS.server)`), `${job}:\n${problems.join('\n')}`);
  }
  // And the reverse: a server job in desktop-ci.yml.
  const problems = checkDesktopCiWorkflow(fixture(GOOD + jobBody('go-lint') + '\n'));
  assert.ok(problems.some((p) => p.startsWith('desktop-ci.yml: job "go-lint" does not belong')), problems.join('\n'));
});

test('neither workflow filters paths at the trigger', () => {
  for (const [file, check] of [['server.yml', checkServerWorkflow], ['desktop-ci.yml', checkDesktopCiWorkflow]]) {
    for (const [before, after] of [
      ['  push:\n    branches: [main]\n', "  push:\n    branches: [main]\n    paths: ['internal/**']\n"],
      ['  pull_request:\n', "  pull_request:\n    paths-ignore: ['docs/**']\n"],
      ['  pull_request:\n', '  pull_request:\n    branches: [main]\n'],
      ['  push:\n    branches: [main]\n', ''],
      ['name: server\n', 'name: renamed\n'],
      ['name: desktop\n', 'name: renamed\n'],
    ]) {
      const [server, desktop] = GOOD.split(SPLIT);
      const part = file === 'server.yml' ? server : desktop;
      if (!part.includes(before)) continue;
      const changed = file === 'server.yml' ? server.replace(before, after) + SPLIT + desktop : server + SPLIT + desktop.replace(before, after);
      const problems = check(fixture(changed));
      assert.ok(problems.some((p) => p.startsWith(`${file}: `) && /no paths or other filter|must be named/.test(p)), `${file} ${before}:\n${problems.join('\n')}`);
    }
  }
});

test('if: always() stays forbidden in every job but the ok jobs and barriers, and in their steps', () => {
  let problems = checkWorkflow(fixture(GOOD.replace("  deny:\n    needs: [changes]\n    if: needs.changes.outputs.core != 'false'\n", '  deny:\n    needs: [changes]\n    if: always()\n')));
  assert.ok(problems.some((p) => p.includes('if: always()') && p.includes('R22')), problems.join('\n'));
  problems = checkWorkflow(fixture(GOOD.replace('      - run: echo ok\n', '      - if: always()\n        run: echo ok\n')));
  assert.ok(problems.some((p) => p.includes('if: always()') && p.includes('R22')), problems.join('\n'));
});

test('a gate that tests == true instead of != false is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace("needs.changes.outputs.go != 'false'", "needs.changes.outputs.go == 'true'")));
  assert.ok(problems.some((p) => p.includes('must carry the job-level if:')), problems.join('\n'));
});

test('the docs job must run the real checks, not only their unit tests', () => {
  for (const step of ['check:brief', 'check:copy', 'check:docs', 'check:ci']) {
    const problems = checkWorkflow(fixture(GOOD.replace(`      - run: npm run ${step}\n`, '')));
    assert.ok(problems.some((p) => p.includes('"docs"') && p.includes(step)), `${step}: ${problems.join('\n')}`);
  }
});


// ---- Temporary repository roots holding the real workflow files ----
// Each test mutates the real committed files in a temporary copy.

const yaml = createRequire(import.meta.url)('js-yaml');
const RELEASE_FILES = [
  ...readdirSync(join(REPO_ROOT, '.github', 'workflows')).map(name => `.github/workflows/${name}`),
];
/** A temporary root holding the real files with `changes` (path -> text => text) applied. */
const releaseRoots = [];
after(() => { for (const root of releaseRoots.splice(0)) rmSync(root, { recursive: true, force: true }); });
function releaseRoot(changes = {}) {
  const root = mkdtempSync(join(tmpdir(), 'dilla-workflows-ci-'));
  releaseRoots.push(root);
  for (const file of RELEASE_FILES) {
    const text = readFileSync(join(REPO_ROOT, file), 'utf8');
    mkdirSync(dirname(join(root, file)), { recursive: true });
    writeFileSync(join(root, file), changes[file] ? changes[file](text) : text);
  }
  for (const [file, change] of Object.entries(changes)) if (!RELEASE_FILES.includes(file)) {
    mkdirSync(dirname(join(root, file)), { recursive: true }); writeFileSync(join(root, file), change(''));
  }
  return root;
}
/** A structural change: parse, mutate, dump. */
const edit = mutate => text => { const doc = yaml.load(text); mutate(doc); return yaml.dump(doc, { lineWidth: -1 }); };
const step = (doc, job, match) => doc.jobs[job].steps.find(item => (item.name ?? item.uses ?? item.run ?? '').includes(match));

// A failure-evidence upload of a step that wrote no files adds a second, misleading error ("No files were
// found with the provided path"); the guard makes the upload run only when its evidence exists.
test('a failure-evidence upload without a hashFiles guard is reported', () => {
  assert.deepEqual(checkWorkflow(REPO_ROOT), []);
  for (const [file, text, check] of [['server.yml', REAL_SERVER, checkServerWorkflow], ['desktop-ci.yml', REAL_DESKTOP_CI, checkDesktopCiWorkflow]]) {
    const guards = [...text.matchAll(/if: failure\(\)[^\n]*hashFiles\([^\n]*/g)].map(m => m[0]);
    if (file === 'server.yml') { assert.equal(guards.length, 0, 'server.yml uploads no failure evidence'); continue; }
    assert.ok(guards.length > 0, file);
    for (const guard of guards) {
      const root = fixture(GOOD);
      writeFileSync(join(root, '.github/workflows', file), text.replace(guard, guard.replace(/ && hashFiles\(.*$/, '')));
      assert.ok(check(root).some(p => p.includes('hashFiles') && p.includes('failure-evidence')), `${file}: ${guard}`);
    }
  }
});

test('every Go job downloads its modules through the retry wrapper, after its setup steps', () => {
  assert.deepEqual(checkWorkflow(REPO_ROOT), []);
  const step = '      - name: Download Go modules (retried only on known network errors)\n';
  const body = new RegExp('^  go-lint:\\n[\\s\\S]*?(?=^  [a-z][\\w-]*:|$(?![\\s\\S]))', 'm').exec(REAL_CI)[0], start = body.indexOf(step), end = body.indexOf('\n      - ', start + 1) + 1;
  assert.ok(start >= 0);
  const collect = (job) => { const found = []; checkGoModuleDownloads({ 'go-lint': job }, found, 'server.yml'); return found; };
  assert.deepEqual(collect(body), []);
  for (const changed of [
    body.slice(0, start) + body.slice(end),
    body.replace('go mod download', 'go mod download || true'),
    body.replace(step, step + '        continue-on-error: true\n'),
    body.slice(0, start) + body.slice(end) + body.slice(start, end),
    body.slice(0, end) + body.slice(start, end) + body.slice(end),
  ]) {
    assert.ok(collect(changed).some((p) => p.includes('go mod download')));
  }
});

test('every job that runs playwright install --with-deps is pinned to ubuntu-24.04', () => {
  assert.deepEqual(checkWorkflow(REPO_ROOT), []);
  for (const job of ['ui', 'core-integration', 'browser-spike', 'browser-media']) {
    const body = new RegExp(`^  ${job}:\\n[\\s\\S]*?(?=^  [a-z][\\w-]*:\\n|$(?![\\s\\S]))`, 'm').exec(REAL_DESKTOP_CI)[0];
    const found = [];
    checkPlaywrightImage({ [job]: body }, found, 'desktop-ci.yml');
    assert.deepEqual(found, [], job);
    checkPlaywrightImage({ [job]: body.replace('    runs-on: ubuntu-24.04\n', '    runs-on: ubuntu-latest\n') }, found, 'desktop-ci.yml');
    assert.ok(found.some((p) => p.includes(`"${job}"`) && p.includes('ubuntu-24.04')), job);
  }
});

test('every release Go job downloads its modules through the retry wrapper', () => {
  const step = /      - name: Download Go modules \(retried only on known network errors\)\n        run: [^\n]*\n/;
  assert.match(REAL_RELEASE, step);
  assertReported(releaseProblems((r) => r.replace(step, '')), 'must run exactly one retried "go mod download" step');
});

const RERUN = '.github/workflows/ci-rerun-starved.yml';
const rerunProblems = (change) => {
  const root = releaseRoot({ [RERUN]: change });
  const problems = checkRerunStarvedWorkflow(root);
  rmSync(root, { recursive: true, force: true });
  return problems;
};

test('the re-run-starved workflow as committed has no problems', () => {
  assert.deepEqual(checkRerunStarvedWorkflow(REPO_ROOT), []);
  assert.deepEqual(rerunProblems(edit(() => {})), [], 'a parse and re-dump is not a change');
  const cli = spawnSync(process.execPath, [SCRIPT_PATH, releaseRoot({ [RERUN]: (text) => text.replace('types: [completed]', 'types: [requested]') })], { encoding: 'utf8' });
  assert.notEqual(cli.status, 0, 'wired into the CLI');
  assert.match(cli.stderr, /ci-rerun-starved\.yml/);
});

test('renaming a CI workflow cannot silently unhook the re-run-starved workflow', () => {
  for (const [file, from] of [['.github/workflows/server.yml', 'name: server\n'], ['.github/workflows/desktop-ci.yml', 'name: desktop\n']]) {
    const root = releaseRoot({ [file]: (text) => text.replace(from, 'name: renamed\n') });
    assert.ok(checkRerunStarvedWorkflow(root).some((problem) => problem.includes('"renamed"')), file);
    rmSync(root, { recursive: true, force: true });
  }
});

test('the re-run-starved workflow is reported when it is missing', () => {
  const root = releaseRoot();
  rmSync(join(root, RERUN));
  assert.deepEqual(checkRerunStarvedWorkflow(root), ['ci-rerun-starved.yml: missing']);
  rmSync(root, { recursive: true, force: true });
});

test('each pinned property of the re-run-starved workflow is enforced', () => {
  const mutations = {
    'another trigger': [edit((doc) => { doc.on.pull_request = {}; }), 'must trigger only on workflow_run'],
    'another upstream workflow': [edit((doc) => { doc.on.workflow_run.workflows = ['desktop-release']; }), 'must trigger only on workflow_run'],
    'only one CI workflow': [edit((doc) => { doc.on.workflow_run.workflows = ['desktop']; }), 'must trigger only on workflow_run of "server" and "desktop"'],
    'the deleted ci workflow': [edit((doc) => { doc.on.workflow_run.workflows = ['ci']; }), 'must trigger only on workflow_run of "server" and "desktop"'],
    'wrong event type': [edit((doc) => { doc.on.workflow_run.types = ['requested']; }), 'must trigger only on workflow_run'],
    'extra permission': [edit((doc) => { doc.permissions.contents = 'write'; }), 'exactly actions: write'],
    'no workflow permissions': [edit((doc) => { delete doc.permissions; }), 'exactly actions: write'],
    'job write-all': [edit((doc) => { doc.jobs.rerun.permissions = 'write-all'; }), 'only actions: write and contents: read'],
    'attempt guard removed': [edit((doc) => { doc.jobs.rerun.if = "github.event.workflow_run.conclusion != 'success'"; }), 'run_attempt == 1'],
    'conclusion guard loosened': [edit((doc) => { doc.jobs.rerun.if = "github.event.workflow_run.run_attempt == 1 && github.event.workflow_run.conclusion != 'success'"; }), 'must be exactly'],
    'if with an or': [edit((doc) => { doc.jobs.rerun.if += ' || always()'; }), 'must not contain ||'],
    'RUN_ID from elsewhere': [edit((doc) => { step(doc, 'rerun', 'node scripts').env.RUN_ID = '${{ github.run_id }}'; }), 'RUN_ID, RUN_ATTEMPT and RUN_CONCLUSION'],
    'RUN_ATTEMPT removed': [edit((doc) => { delete step(doc, 'rerun', 'node scripts').env.RUN_ATTEMPT; }), 'RUN_ID, RUN_ATTEMPT and RUN_CONCLUSION'],
    'RUN_CONCLUSION removed': [edit((doc) => { delete step(doc, 'rerun', 'node scripts').env.RUN_CONCLUSION; }), 'RUN_ID, RUN_ATTEMPT and RUN_CONCLUSION'],
    'checkout of the run head': [edit((doc) => { step(doc, 'rerun', 'actions/checkout').with.ref = '${{ github.event.workflow_run.head_sha }}'; }), 'default_branch'],
    'checkout without a ref': [edit((doc) => { delete step(doc, 'rerun', 'actions/checkout').with.ref; }), 'default_branch'],
    'run id interpolated inline': [edit((doc) => { step(doc, 'rerun', 'node scripts').run = 'node scripts/ci-rerun-starved.mjs ${{ github.event.workflow_run.id }}'; }), 'interpolated inline'],
    'a secret': [edit((doc) => { step(doc, 'rerun', 'node scripts').env.TOKEN = '${{ secrets.PAT }}'; }), 'no secrets'],
  };
  for (const [label, [change, needle]] of Object.entries(mutations)) {
    assert.ok(rerunProblems(change).some((problem) => problem.includes(needle)), `not reported: ${label}`);
  }
});

// Plan Review Focus 3: ci-watchdog fails open (an API error ends it with exit 0), so a green watcher says nothing about
// the work. Each ok job's own steps must still turn red on a failed or cancelled need. This runs the real ok job's
// steps against crafted `needs` results with a small evaluator for the expression forms they use.
function evalExpression(source, ctx) {
  const tokens = source.match(/'[^']*'|&&|\|\||==|!=|[(),]|[A-Za-z_][\w.*-]*/g);
  assert.equal(tokens.join(''), source.replace(/\s+/g, ''), `unparsed expression: ${source}`);
  let at = 0;
  const peek = () => tokens[at];
  const take = (want) => { const token = tokens[at++]; if (want !== undefined) assert.equal(token, want, source); return token; };
  const lookup = (path) => {
    const [head, ...rest] = path.split('.');
    if (head === 'needs' && rest[0] === '*') return Object.values(ctx.needs).map((need) => need[rest[1]]);
    return rest.reduce((value, key) => value?.[key], ctx[head]);
  };
  const primary = () => {
    const token = take();
    if (token === '(') { const value = or(); take(')'); return value; }
    if (token.startsWith("'")) return token.slice(1, -1);
    if (token === 'contains') { take('('); const list = or(); take(','); const item = or(); take(')'); return list.includes(item); }
    return lookup(token);
  };
  const comparison = () => {
    const left = primary();
    if (peek() === '==' || peek() === '!=') { const op = take(); const right = primary(); return op === '==' ? left === right : left !== right; }
    return left;
  };
  const and = () => { let value = comparison(); while (peek() === '&&') { take(); const right = comparison(); value = value && right; } return value; };
  const or = () => { let value = and(); while (peek() === '||') { take(); const right = and(); value = value || right; } return value; };
  const value = or();
  assert.equal(at, tokens.length, `trailing tokens in: ${source}`);
  return Boolean(value);
}

/** Whether the ok job ends green: its steps run in order and the first one that runs `exit 1` fails it. */
function okJobPasses(job, needs, github) {
  for (const step of job.steps) {
    if (step.if !== undefined && !evalExpression(String(step.if), { needs, github })) continue;
    if (/^\s*exit 1\s*$/.test(step.run)) return false;
  }
  return true;
}

const SAME_REPO_PR = { event_name: 'pull_request', repository: 'o/r', event: { pull_request: { head: { repo: { full_name: 'o/r' } } } } };
const FORK_PR = { ...SAME_REPO_PR, event: { pull_request: { head: { repo: { full_name: 'fork/r' } } } } };
const MAIN_PUSH = { event_name: 'push', repository: 'o/r', event: {} };

for (const [workflow, text] of [['server', REAL_SERVER], ['desktop', REAL_DESKTOP_CI]]) {
  const ok = WORKFLOWS[workflow].ok;
  const job = yaml.load(text).jobs[ok];
  const results = (over = {}) => Object.fromEntries(job.needs.map((name) => [name, { result: over[name] ?? 'success' }]));

  test(`${ok} fails on a failed or cancelled need even though ci-watchdog succeeded after failing open`, () => {
    assert.equal(job.if, 'always()');
    assert.ok(job.needs.includes('ci-watchdog'));
    for (const github of [SAME_REPO_PR, MAIN_PUSH]) {
      assert.equal(okJobPasses(job, results(), github), true, 'every need green');
      for (const need of job.needs.filter((name) => name !== 'ci-watchdog')) {
        for (const result of ['failure', 'cancelled']) {
          assert.equal(okJobPasses(job, results({ [need]: result }), github), false, `${need} ${result} on ${github.event_name}`);
        }
        assert.equal(okJobPasses(job, results({ [need]: 'skipped' }), github), true, `${need} skipped by its area gate`);
      }
    }
  });

  test(`${ok} accepts a skipped ci-watchdog only on a fork pull request`, () => {
    assert.equal(okJobPasses(job, results({ 'ci-watchdog': 'skipped' }), FORK_PR), true);
    assert.equal(okJobPasses(job, results({ 'ci-watchdog': 'skipped' }), SAME_REPO_PR), false);
    assert.equal(okJobPasses(job, results({ 'ci-watchdog': 'skipped' }), MAIN_PUSH), false);
    assert.equal(okJobPasses(job, results({ 'ci-watchdog': 'failure' }), FORK_PR), false);
  });

  test(`${ok} without its failure step would pass a failed need (the test can fail)`, () => {
    const gutted = { ...job, steps: job.steps.filter((step) => !String(step.if ?? '').includes("contains(needs.*.result, 'failure')")) };
    const need = job.needs.find((name) => name !== 'ci-watchdog' && name !== 'changes');
    assert.equal(okJobPasses(gutted, results({ [need]: 'failure' }), MAIN_PUSH), true);
  });
}

// ---------------------------------------------------------------- mobile.yml (plan 2026-11-05 mobile M1, Task 7)

/** A fixture whose mobile.yml is `text` (the real server and desktop workflows beside it), checked as the mobile workflow. */
function checkMobile(text) {
  const dir = fixture(GOOD);
  writeFileSync(join(dir, '.github', 'workflows', 'mobile.yml'), text);
  return checkMobileCiWorkflow(dir);
}
const mobileEdit = (from, to) => {
  assert.ok(REAL_MOBILE.includes(from), `fixture sanity: mobile.yml has ${from}`);
  const changed = REAL_MOBILE.replace(from, to);
  assert.notEqual(changed, REAL_MOBILE);
  return changed;
};

test('the committed mobile.yml has no problems, and neither does the fixture that carries it', () => {
  assert.deepEqual(checkMobileCiWorkflow(REPO_ROOT), []);
  assert.deepEqual(checkMobile(REAL_MOBILE), []);
});

test('mobile.yml keeps every load-bearing command: unit tests, the client build, the APKs and their release check, the smoke test, the iOS build', () => {
  for (const [from, what] of [
    ['      - run: npm test -w @dilla/mobile\n', 'npm test -w @dilla/mobile'],
    ['      - run: npm run typecheck -w @dilla/mobile\n', 'npm run typecheck -w @dilla/mobile'],
    ['      - run: npm run build -w @dilla/mobile\n', 'npm run build -w @dilla/mobile'],
    ['      - run: npm run android:check-release -w @dilla/mobile\n', 'android:check-release'],
    [' --no-daemon assembleDebug assembleRelease', 'assembleDebug assembleRelease'],
    ['        run: npm run e2e:android -w @dilla/mobile -- --skip-build\n', 'e2e:android'],
    ['      - run: bash scripts/ci-android.sh avd\n', 'ci-android.sh avd'],
    ["-sdk iphonesimulator -destination 'generic/platform=iOS Simulator'", 'iphonesimulator'],
    ['      - run: go build -o target/dilla-testhost ./cmd/dilla-testhost\n', 'dilla-testhost'],
  ]) {
    const problems = checkMobile(mobileEdit(from, from.startsWith('      - run:') || from.startsWith('        run:') ? from.replace(/run: .*/, 'run: echo skipped') : ' build'));
    assert.ok(problems.some((p) => p.startsWith('mobile.yml: job "') && p.includes('is missing:')), `${what}: ${problems.join('\n')}`);
  }
});

test('mobile.yml is filtered by the changes job, never at the trigger, and keeps its nightly run and watcher', () => {
  const cases = [
    ['a paths filter', mobileEdit('  pull_request:\n', "  pull_request:\n    paths: ['packages/mobile/**']\n"), 'must trigger'],
    ['no schedule', mobileEdit(`  schedule:\n    - cron: '${NIGHTLY_CRON}'\n`, ''), 'must trigger'],
    ['a watcher that skips the nightly run', mobileEdit("|| github.event_name == 'schedule' ", ''), 'unsafe watchdog'],
    ['a job without its gate', mobileEdit("  mobile-ios:\n    needs: [changes, mobile-bundle]\n    if: needs.changes.outputs.mobile != 'false'\n", '  mobile-ios:\n    needs: [changes, mobile-bundle]\n'), 'job-level if:'],
    ['a job gated on another area', mobileEdit("  mobile-unit:\n    needs: [changes]\n    if: needs.changes.outputs.mobile != 'false'\n", "  mobile-unit:\n    needs: [changes]\n    if: needs.changes.outputs.web != 'false'\n"), 'job-level if:'],
    ['an ok job that misses a job', mobileEdit('    needs: [ci-watchdog, mobile-work-complete, changes, mobile-unit, ', '    needs: [ci-watchdog, mobile-work-complete, changes, '), 'mobile-ok'],
  ];
  for (const [what, changed, message] of cases) {
    const problems = checkMobile(changed);
    assert.ok(problems.some((p) => p.startsWith('mobile.yml') && p.includes(message)), `${what}: ${problems.join('\n')}`);
  }
});

test('every mobile artifact consumer waits for its producer', () => {
  for (const [from, to, need] of [
    ['  mobile-bundle:\n    needs: [changes, mobile-core-wasm]\n', '  mobile-bundle:\n    needs: [changes]\n', 'mobile-core-wasm'],
    ['  mobile-android:\n    needs: [changes, mobile-bundle, mobile-e2e-tools]\n', '  mobile-android:\n    needs: [changes, mobile-bundle]\n', 'mobile-e2e-tools'],
    ['  mobile-android:\n    needs: [changes, mobile-bundle, mobile-e2e-tools]\n', '  mobile-android:\n    needs: [changes, mobile-e2e-tools]\n', 'mobile-bundle'],
    ['  mobile-ios:\n    needs: [changes, mobile-bundle]\n', '  mobile-ios:\n    needs: [changes]\n', 'mobile-bundle'],
  ]) {
    const problems = checkMobile(mobileEdit(from, to));
    assert.ok(problems.some((p) => p.includes(`has no "needs: ${need}"`)), `${need}: ${problems.join('\n')}`);
  }
});

test('mobile.yml uses GitHub-owned pinned actions only, and its uploads fail closed', () => {
  const third = checkMobile(mobileEdit('      - run: bash scripts/ci-android.sh avd\n', '      - uses: reactivecircus/android-emulator-runner@62dbb605bba737720e10b196cb4220d374026a6d\n      - run: bash scripts/ci-android.sh avd\n'));
  assert.ok(third.some((p) => p.includes('reactivecircus/android-emulator-runner') && p.includes('not a GitHub-owned action')), third.join('\n'));
  const floating = checkMobile(mobileEdit('uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0', 'uses: actions/cache/restore@v6'));
  assert.ok(floating.some((p) => p.includes('must use a 40-hex commit SHA')), floating.join('\n'));
  const silent = checkMobile(mobileEdit('          path: target/mobile-android-e2e\n          if-no-files-found: error\n', '          path: target/mobile-android-e2e\n'));
  assert.ok(silent.some((p) => p.includes('if-no-files-found: error')), silent.join('\n'));
  const unguarded = checkMobile(mobileEdit("        if: failure() && hashFiles('target/mobile-android-e2e/**') != ''\n", '        if: failure()\n'));
  assert.ok(unguarded.some((p) => p.includes('hashFiles(...)')), unguarded.join('\n'));
  const escape = checkMobile(mobileEdit('        run: npm run e2e:android -w @dilla/mobile -- --skip-build\n', '        run: npm run e2e:android -w @dilla/mobile -- --skip-build\n        continue-on-error: true\n'));
  assert.ok(escape.some((p) => p.includes('continue-on-error')), escape.join('\n'));
});

test('mobile.yml builds the wasm core alone, uploads the emulator evidence from a non-hidden path and pins Xcode', () => {
  assert.ok(!/^  web-build:$/m.test(REAL_MOBILE), 'no copy of desktop-ci.yml\'s web-build');
  for (const [from, to, what] of [
    ['      - run: node scripts/check-wasm-size.mjs packages/core-wasm/pkg/dilla_core_wasm_bg.wasm\n', '', 'check-wasm-size'],
    ["        if: failure() && hashFiles('target/mobile-android-e2e/**') != ''", "        if: failure() && hashFiles('packages/mobile/e2e/.out/**') != ''", 'hidden evidence path'],
    ['          DILLA_ANDROID_E2E_OUT: ${{ github.workspace }}/target/mobile-android-e2e\n', '', 'evidence directory'],
    ['          sudo xcode-select -s /Applications/Xcode_26.6.app\n', '', 'xcode-select'],
  ]) {
    const problems = checkMobile(mobileEdit(from, to));
    assert.ok(problems.some((p) => p.startsWith('mobile.yml: job "') && p.includes('is missing:')), `${what}: ${problems.join('\n')}`);
  }
  // The slim job is still a wasm job: the pinned clang 23 block applies to it.
  const clang = checkMobile(mobileEdit('        run: bash scripts/ci-llvm.sh install\n', '        run: echo no clang\n'));
  assert.ok(clang.some((p) => p.includes('job "mobile-core-wasm" builds for wasm32-unknown-unknown')), clang.join('\n'));
});

const GPUI = '.github/workflows/desktop-gpui.yml';
const gpuiProblems = (change) => {
  const root = releaseRoot({ [GPUI]: change });
  const problems = checkDesktopGpuiWorkflow(root);
  rmSync(root, { recursive: true, force: true });
  return problems;
};
const indent = (block) => block.split('\n').map((l) => `      ${l}`).join('\n');

test('the desktop-gpui workflow as committed has no problems', () => {
  assert.deepEqual(checkDesktopGpuiWorkflow(REPO_ROOT), []);
  const cli = spawnSync(process.execPath, [SCRIPT_PATH, releaseRoot({ [GPUI]: (text) => text.replace('go build -o "target/dilla-testhost$exe"', 'true') })], { encoding: 'utf8' });
  assert.notEqual(cli.status, 0, 'wired into the CLI');
  assert.match(cli.stderr, /desktop-gpui\.yml/);
});

test('the desktop-gpui token test, test-host build and --ignored runs are each pinned', () => {
  for (const [name, step] of Object.entries(GPUI_PINNED_STEPS)) {
    const problems = gpuiProblems((text) => { assert.ok(text.includes(indent(step)), name); return text.replace(indent(step), ''); });
    assert.ok(problems.some((p) => p.includes(`the ${name} step`)), `${name}: ${problems.join('|')}`);
  }
  for (const dropped of ['cargo test -p dilla-client --test sync --locked -- --ignored', 'cargo test -p dilla-client --test gateway --locked -- --ignored']) {
    assert.ok(gpuiProblems((text) => text.replace(`          ${dropped}\n`, '')).some((p) => p.includes('the ignored step')), dropped);
  }
});

test('the desktop-gpui macOS call tests are pinned: the app voice suite, the macOS guard, the BlackHole hash', () => {
  for (const [what, from, to] of [
    ['the dilla-desktop voice line', '          cargo test -p dilla-desktop --test voice --test ring --locked -- --ignored --skip parity_\n', ''],
    ['the two-client voice line', '          cargo test -p dilla-client --test voice --test ring --locked -- --ignored\n', ''],
    ['the macOS guard', "      - name: Run the call tests (macOS)\n        if: runner.os == 'macOS' && !cancelled()", '      - name: Run the call tests (macOS)\n        if: ${{ !cancelled() }}'],
    ['the BlackHole hash', 'BLACKHOLE_SHA256: 57b540f27a3e29c37e310e01bee0fdfab76733087e47f997ef9dccf851400dcf', 'BLACKHOLE_SHA256: 0000'],
  ]) {
    const problems = gpuiProblems((text) => { assert.ok(text.includes(from), what); return text.replace(from, to); });
    assert.ok(problems.some((p) => /the mac(Calls|Audio) step/.test(p)), `${what}: ${problems.join('|')}`);
  }
});

test('the macOS parity and VideoToolbox steps cannot lose a line, their guard or their nightly gate (macOS CI plan)', () => {
  const edits = [
    ['    cargo test -p dilla-desktop --features automation --locked --test video_player_shot\n', '', 'macParity'],
    ["  if: runner.os == 'macOS' && !cancelled()\n  shell: bash\n  run: |\n    cargo test -p dilla-desktop --features automation", "  if: !cancelled()\n  shell: bash\n  run: |\n    cargo test -p dilla-desktop --features automation", 'macParity'],
    ["  if: runner.os == 'macOS' && github.event_name == 'schedule' && !cancelled()", "  if: runner.os == 'macOS' && !cancelled()", 'macVideoToolbox'],
  ];
  for (const [from, to, name] of edits) {
    const problems = gpuiProblems((text) => { const f = indent(from); assert.ok(text.includes(f), from); return text.replace(f, indent(to)); });
    assert.ok(problems.some((p) => p.includes(`the ${name} step`)), `${from}: ${problems.join('|')}`);
  }
});

test('desktop-gpui.yml never sets DILLA_PARITY_MEASURE (it turns every parity check into a pass)', () => {
  const step = 'cargo test -p dilla-desktop --features automation --locked --no-fail-fast parity_';
  for (const edit of [`DILLA_PARITY_MEASURE=1 ${step}`, `export DILLA_PARITY_MEASURE=1; ${step}`]) {
    const problems = gpuiProblems((text) => { assert.ok(text.includes(step), step); return text.replace(step, edit); });
    assert.ok(problems.some((p) => p.includes('DILLA_PARITY_MEASURE')), `${edit}: ${problems.join('|')}`);
  }
  assert.ok(!checkDesktopGpuiWorkflow(REPO_ROOT).some((p) => p.includes('DILLA_PARITY_MEASURE')));
});

test('the desktop-gpui macOS package job is pinned: macos-15, no secrets, and every packaging and verification command', () => {
  for (const fragment of GPUI_MAC_PACKAGE_RUNS) {
    const problems = gpuiProblems((text) => { assert.ok(text.includes(fragment), fragment); return text.replace(fragment, 'true'); });
    assert.ok(problems.some((p) => p.includes('gpui-mac-package must run')), `${fragment}: ${problems.join('|')}`);
  }
  assert.ok(gpuiProblems((text) => text.replace('runs-on: macos-15\n    timeout-minutes: 120\n    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7\n        with:\n          persist-credentials: false\n      - name: Free disk space (macOS)\n        run: |\n          df -h /\n          keep=', 'runs-on: macos-14\n    timeout-minutes: 120\n    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7\n        with:\n          persist-credentials: false\n      - name: Free disk space (macOS)\n        run: |\n          df -h /\n          keep=')).some((p) => p.includes('must run on macos-15')));
  assert.ok(gpuiProblems((text) => text.replace('    name: gpui-mac-package\n    if: ${{ !cancelled() }}', '    name: gpui-mac-package')).some((p) => p.includes('!cancelled()')));
  assert.ok(gpuiProblems((text) => text.replace('        run: cargo build -p dilla-desktop --release --locked', '        env:\n          CSC_LINK: ${{ secrets.CSC_LINK }}\n        run: cargo build -p dilla-desktop --release --locked')).some((p) => p.includes('no secrets')));
  assert.ok(gpuiProblems((text) => text.replace(/\n  gpui-mac-package:[\s\S]*$/, '\n')).some((p) => p.includes('job is missing')));
});

test('the desktop-gpui macOS UI check (Layer 3) fails the job, and its cleanup and shots run after a failure', () => {
  const ui = '        run: bash scripts/desktop-gpui-mac-ui.sh "$RUNNER_TEMP/zip-check/Dilla.app" "$RUNNER_TEMP/mac-ui"';
  assert.ok(gpuiProblems((text) => text.replace(ui, `        continue-on-error: true\n${ui}`)).some((p) => p.includes('macOS UI step must have no')));
  assert.ok(gpuiProblems((text) => text.replace(ui, `        if: false\n${ui}`)).some((p) => p.includes('desktop-gpui-mac-ui.sh')));
  assert.ok(gpuiProblems((text) => text.replace(ui, `        # ${ui.trim()}\n        run: true`)).some((p) => p.includes('gpui-mac-package must run `bash scripts/desktop-gpui-mac-ui.sh')));
  const limit = '        timeout-minutes: 30  # a hung helper must not hold a macOS slot until the job timeout\n';
  assert.ok(gpuiProblems((text) => { assert.ok(text.includes(limit)); return text.replace(limit, ''); }).some((p) => p.includes('timeout-minutes of 30 or less')));
  assert.ok(gpuiProblems((text) => text.replace(limit, '        timeout-minutes: 120\n')).some((p) => p.includes('timeout-minutes of 30 or less')));
  const stop = '      - name: Stop the packaged app\n        if: ${{ !cancelled() }}';
  assert.ok(gpuiProblems((text) => { assert.ok(text.includes(stop)); return text.replace(stop, '      - name: Stop the packaged app'); }).some((p) => p.includes('pkill -9') && p.includes('!cancelled()')));
  assert.ok(gpuiProblems((text) => text.replace(stop, '      - name: Stop the packaged app\n        if: always()')).some((p) => p.includes('pkill -9')));
  const upload = '      - name: Upload the macOS UI shots\n        if: ${{ !cancelled() }}';
  assert.ok(gpuiProblems((text) => { assert.ok(text.includes(upload)); return text.replace(upload, '      - name: Upload the macOS UI shots'); }).some((p) => p.includes('gpui-mac-ui')));
});

test('the desktop-gpui workflow is reported when it is missing', () => {
  const root = releaseRoot();
  rmSync(join(root, GPUI));
  assert.deepEqual(checkDesktopGpuiWorkflow(root), ['desktop-gpui.yml: missing']);
  rmSync(root, { recursive: true, force: true });
});

test('every GPUI integration-test file with #[ignore]d tests has a --ignored run in desktop-gpui.yml', () => {
  const root = releaseRoot();
  const dir = join(root, 'clients/desktop/dilla-client/tests');
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, 'newarea.rs'), '#[test]\n#[ignore = "needs dillad"]\nfn t() {}\n');
  writeFileSync(join(dir, 'plain.rs'), '#[test]\nfn t() {}\n');
  const problems = checkDesktopGpuiWorkflow(root);
  assert.ok(problems.some((p) => p.includes('newarea.rs') && p.includes('--test newarea')), problems.join('\n'));
  assert.ok(!problems.some((p) => p.includes('plain.rs')), 'a file without ignored tests needs no run');
  const wf = join(root, GPUI);
  const listed = 'cargo test -p dilla-desktop --test slice --locked -- --ignored\n';
  writeFileSync(wf, readFileSync(wf, 'utf8').replace(listed, `${listed}          cargo test -p dilla-client --test newarea --locked -- --ignored\n`));
  assert.ok(!checkDesktopGpuiWorkflow(root).some((p) => p.includes('newarea.rs')), 'listing the file satisfies the guard');
  rmSync(root, { recursive: true, force: true });
});

// A directory holding a fake `objdump` that prints `output` and exits with `code`; `null` for no objdump at all.
function glibcToolDir(output, code) {
  const dir = mkdtempSync(join(tmpdir(), 'dilla-glibc-tool-'));
  if (output !== null) writeFileSync(join(dir, 'objdump'), `#!/bin/sh\nprintf '%s' '${output}'\nexit ${code}\n`, { mode: 0o755 });
  return dir;
}
const glibcScript = join(REPO_ROOT, 'scripts', 'check-glibc-max.sh');
const runGlibc = (args, dir) => spawnSync('/bin/bash', [glibcScript, ...args], { encoding: 'utf8', env: { ...process.env, PATH: dir === undefined ? process.env.PATH : `${dir}:${process.env.PATH}` } });

test('check-glibc-max.sh refuses a binary that needs a newer glibc than allowed', { skip: process.platform !== 'linux' }, () => {
  const dir = glibcToolDir('0000 g DF .text 0 GLIBC_2.39 memcpy\n0000 g DF .text 0 GLIBC_2.17 free\n', 0);
  const ok = runGlibc(['2.39', process.execPath], dir);
  assert.equal(ok.status, 0, ok.stderr);
  assert.match(ok.stdout, /needs GLIBC_2\.39/);
  const bad = runGlibc(['2.38', process.execPath], dir);
  assert.equal(bad.status, 1);
  assert.match(bad.stderr, /needs GLIBC_2\.39, newer than the allowed GLIBC_2\.38/);
  assert.equal(runGlibc(['2.39', '/nonexistent/binary'], dir).status, 2);
  rmSync(dir, { recursive: true, force: true });
});

test('check-glibc-max.sh cannot pass without measuring: no objdump, a failing objdump or no GLIBC versions all fail', { skip: process.platform !== 'linux' }, () => {
  const none = glibcToolDir(null, 0);
  // PATH holds only the empty directory: no objdump, no other tool.
  const missing = spawnSync('/bin/bash', [glibcScript, '2.39', process.execPath], { encoding: 'utf8', env: { PATH: none } });
  assert.equal(missing.status, 2, 'a missing objdump is a tooling error, not a pass');
  assert.match(missing.stderr, /objdump is not installed/);
  const failing = glibcToolDir('', 1);
  assert.equal(runGlibc(['2.39', process.execPath], failing).status, 2, 'an objdump failure is not swallowed');
  const static_ = glibcToolDir('0000 g DF .text 0 Base memcpy\n', 0);
  const noVersions = runGlibc(['2.39', process.execPath], static_);
  assert.equal(noVersions.status, 1, 'a binary with no GLIBC versions (static, musl) fails');
  assert.match(noVersions.stderr, /nothing was measured/);
  for (const d of [none, failing, static_]) rmSync(d, { recursive: true, force: true });
});

test('the GPUI ignored-coverage guard counts only live Linux run lines and sees every ignore spelling', () => {
  const root = releaseRoot();
  const tests = (dir, file, source) => { mkdirSync(join(root, 'clients/desktop', dir, 'tests', ...file.split('/').slice(0, -1)), { recursive: true }); writeFileSync(join(root, 'clients/desktop', dir, 'tests', file), source); };
  tests('dilla-client', 'cfg.rs', '#[test]\n#[cfg_attr(not(windows), ignore = "needs dillad")]\nfn t() {}\n');
  tests('dilla-client', 'inline.rs', '#[test] #[ignore = "x"] fn t() {}\n');
  tests('dilla-desktop', 'dirsuite/main.rs', '#[test]\n#[ignore]\nfn t() {}\n');
  tests('dilla-desktop-ui', 'ui.rs', '#[test]\n#[ignore]\nfn t() {}\n');
  const wf = join(root, GPUI);
  const original = readFileSync(wf, 'utf8');
  const missing = checkDesktopGpuiWorkflow(root).join('\n');
  for (const name of ['cfg.rs', 'inline.rs', 'dirsuite/main.rs', 'ui.rs']) assert.ok(missing.includes(name), `${name}: ${missing}`);
  const listed = 'cargo test -p dilla-desktop --test slice --locked -- --ignored\n';
  // A YAML comment line naming the suite does not count.
  writeFileSync(wf, original.replace(listed, `${listed}          # cargo test -p dilla-client --test cfg --locked -- --ignored\n`));
  assert.ok(checkDesktopGpuiWorkflow(root).some((p) => p.includes('cfg.rs')), 'a commented-out line must not count');
  // A live line does, and so does a run with no --test filter.
  writeFileSync(wf, original.replace(listed, `${listed}          cargo test -p dilla-client --test cfg --test inline --locked -- --ignored\n          cargo test -p dilla-desktop --test dirsuite --locked -- --ignored\n          cargo test -p dilla-desktop-ui --locked -- --ignored\n`));
  assert.deepEqual(checkDesktopGpuiWorkflow(root), []);
  // A disabled step does not count.
  writeFileSync(wf, readFileSync(wf, 'utf8').replace("name: Run the ignored tests (Linux, desktop)\n        if: matrix.shard == 'desktop' && !cancelled()", "name: Run the ignored tests (Linux, desktop)\n        if: matrix.shard == 'desktop' && false"));
  assert.ok(checkDesktopGpuiWorkflow(root).length > 0, 'a step guarded by false must not count');
  rmSync(root, { recursive: true, force: true });
});

test('parity tests parked until the shared-component pass need no --ignored run, but any other ignore in the file still does', () => {
  const root = releaseRoot();
  const dir = join(root, 'clients/desktop/dilla-desktop/tests');
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, 'parked.rs'), '#[test]\n#[ignore = "parity after T6c: chrome 0.25 % (voice row)"]\nfn parity_a() {}\n');
  assert.ok(!checkDesktopGpuiWorkflow(root).some((p) => p.includes('parked.rs')), 'a parked parity test alone needs no run line');
  writeFileSync(join(dir, 'parked.rs'), '#[test]\n#[ignore = "parity after T6c: chrome 0.25 %"]\nfn parity_a() {}\n#[test]\n#[ignore = "needs dillad"]\nfn live() {}\n');
  assert.ok(checkDesktopGpuiWorkflow(root).some((p) => p.includes('parked.rs')), 'another ignore in the same file still needs its run line');
  writeFileSync(join(dir, 'parked.rs'), '#[test]\n#[ignore = "parity later"]\nfn parity_a() {}\n');
  assert.ok(checkDesktopGpuiWorkflow(root).some((p) => p.includes('parked.rs')), 'only the exact R22 reason is exempt');
  // An off-Linux ignore owes no Linux --ignored run; a macro forwarding only R22 reasons is parked; any other forwarded reason is not.
  const macro = (why) => `macro_rules! p { ($($t:ident $(; $why:literal)?),*) => {$(#[test] $(#[ignore = $why])? #[cfg_attr(not(target_os = "linux"), ignore = "refs are Linux only")] fn $t() {})*}; }\np!(a; "${why}", b);\n`;
  writeFileSync(join(dir, 'parked.rs'), macro('parity after T6c: chrome 0.3 %'));
  assert.ok(!checkDesktopGpuiWorkflow(root).some((p) => p.includes('parked.rs')), 'off-Linux ignore and R22-only macro reasons need no run line');
  writeFileSync(join(dir, 'parked.rs'), macro('needs dillad'));
  assert.ok(checkDesktopGpuiWorkflow(root).some((p) => p.includes('parked.rs')), 'a macro forwarding another reason still needs its run line');
  // The Linux-only forwarding spelling (one ignore per OS, so clippy's duplicate-attribute lint holds off Linux) reads the same.
  const linuxOnly = (why) => `macro_rules! p { ($($t:ident $(; $why:literal)?),*) => {$(#[test] #[cfg_attr(not(target_os = "linux"), ignore = "refs are Linux only")] $(#[cfg_attr(target_os = "linux", ignore = $why)])? fn $t() {})*}; }\np!(a; "${why}", b);\n`;
  writeFileSync(join(dir, 'parked.rs'), linuxOnly('parity after T6c: chrome 0.3 %'));
  assert.ok(!checkDesktopGpuiWorkflow(root).some((p) => p.includes('parked.rs')), 'a Linux-only forwarded R22 reason needs no run line');
  writeFileSync(join(dir, 'parked.rs'), linuxOnly('needs dillad'));
  assert.ok(checkDesktopGpuiWorkflow(root).some((p) => p.includes('parked.rs')), 'a Linux-only forwarded other reason still needs its run line');
  // The Linux-and-macOS forwarding spelling (macOS CI plan, Task 3) reads the same.
  const linuxMac = (why) => `macro_rules! p { ($($t:ident $(; $why:literal)?),*) => {$(#[test] #[cfg_attr(not(any(target_os = "linux", target_os = "macos")), ignore = "refs")] $(#[cfg_attr(any(target_os = "linux", target_os = "macos"), ignore = $why)])? fn $t() {})*}; }\np!(a; "${why}", b);\n`;
  writeFileSync(join(dir, 'parked.rs'), linuxMac('parity after T6c: chrome 0.3 %'));
  assert.ok(!checkDesktopGpuiWorkflow(root).some((p) => p.includes('parked.rs')), 'a Linux-and-macOS forwarded R22 reason needs no run line');
  writeFileSync(join(dir, 'parked.rs'), linuxMac('needs dillad'));
  assert.ok(checkDesktopGpuiWorkflow(root).some((p) => p.includes('parked.rs')), 'a Linux-and-macOS forwarded other reason still needs its run line');
  // The Linux-and-macOS spelling (macOS CI plan, Task 3) is off Linux too; an ignore off macOS alone is not.
  writeFileSync(join(dir, 'parked.rs'), '#[test]\n#[cfg_attr(not(any(target_os = "linux", target_os = "macos")), ignore = "references are Linux and macOS only")]\nfn parity_a() {}\n');
  assert.ok(!checkDesktopGpuiWorkflow(root).some((p) => p.includes('parked.rs')), 'an ignore off Linux and macOS needs no Linux run line');
  writeFileSync(join(dir, 'parked.rs'), '#[test]\n#[cfg_attr(not(target_os = "macos"), ignore = "macOS only")]\nfn mac_only() {}\n');
  assert.ok(checkDesktopGpuiWorkflow(root).some((p) => p.includes('parked.rs')), 'an ignore off macOS alone is ignored on Linux and still needs its run line');
  rmSync(root, { recursive: true, force: true });
});

// desktop-gpui-release.yml (GPUI release plan, Milestone 1): the Linux alpha release, a tag run reading no secret.
const GPUI_RELEASE = '.github/workflows/desktop-gpui-release.yml';
const REAL_GPUI_RELEASE = readFileSync(join(REPO_ROOT, GPUI_RELEASE), 'utf8');
function gpuiReleaseProblems(edit) {
  const edited = edit(REAL_GPUI_RELEASE);
  assert.notEqual(edited, REAL_GPUI_RELEASE, 'the edit changed nothing; the fixture drifted from desktop-gpui-release.yml');
  const dir = mkdtempSync(join(TEST_ROOT, 'dilla-gpui-release-'));
  mkdirSync(join(dir, '.github', 'workflows'), { recursive: true });
  writeFileSync(join(dir, GPUI_RELEASE), edited);
  return checkDesktopGpuiReleaseWorkflow(dir);
}

test('the real GPUI release workflow passes every rule, and the CLI runs the check', () => {
  assert.deepEqual(checkDesktopGpuiReleaseWorkflow(REPO_ROOT), []);
  const dir = mkdtempSync(join(TEST_ROOT, 'dilla-gpui-release-missing-'));
  assert.deepEqual(checkDesktopGpuiReleaseWorkflow(dir), ['desktop-gpui-release.yml: missing']);
  assert.match(readFileSync(SCRIPT_PATH, 'utf8'), /\.\.\.checkDesktopGpuiReleaseWorkflow\(root\)/);
});

test('the GPUI release workflow reads no secret and enters no environment', () => {
  assertReported(gpuiReleaseProblems((t) => t.replace('GH_TOKEN: ${{ github.token }}\n          TAG: ${{ steps.tag.outputs.tag }}', 'GH_TOKEN: ${{ secrets.RELEASE_TOKEN }}\n          TAG: ${{ steps.tag.outputs.tag }}')), 'reads no secret');
  assertReported(gpuiReleaseProblems((t) => t.replace('  publish:\n    runs-on: ubuntu-24.04\n', '  publish:\n    runs-on: ubuntu-24.04\n    environment: desktop-release\n')), 'enters an environment');
});

test('the GPUI release build is clean: no cache, --locked, default features only', () => {
  assertReported(gpuiReleaseProblems((t) => t.replace('      - run: rustup show active-toolchain\n', '      - uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0\n        with:\n          path: target\n          key: k\n      - run: rustup show active-toolchain\n')), 'restores or saves a cache');
  assertReported(gpuiReleaseProblems((t) => t.replace('          node-version-file: .nvmrc\n      # LiveKit', '          node-version-file: .nvmrc\n          cache: npm\n      # LiveKit')), "turns on actions/setup-node's cache");
  assertReported(gpuiReleaseProblems((t) => t.replace('run: cargo build --release --locked -p dilla-desktop', 'run: cargo build --release -p dilla-desktop')), 'cargo build --release --locked -p dilla-desktop');
  assertReported(gpuiReleaseProblems((t) => t.replace('run: cargo build --release --locked -p dilla-desktop', 'run: cargo build --release --locked -p dilla-desktop --features automation')), 'default features only');
});

test("the GPUI release build installs every build package of desktop-gpui.yml's, less the test-only ones", () => {
  const ci = readFileSync(join(REPO_ROOT, '.github/workflows/desktop-gpui.yml'), 'utf8');
  const withCi = (release) => {
    const dir = mkdtempSync(join(TEST_ROOT, 'dilla-gpui-release-apt-'));
    mkdirSync(join(dir, '.github', 'workflows'), { recursive: true });
    writeFileSync(join(dir, GPUI_RELEASE), release);
    writeFileSync(join(dir, '.github/workflows/desktop-gpui.yml'), ci);
    return checkDesktopGpuiReleaseWorkflow(dir);
  };
  assert.deepEqual(withCi(REAL_GPUI_RELEASE), []);
  const dropped = REAL_GPUI_RELEASE.replace(' libpipewire-0.3-dev\n', '\n');
  assert.notEqual(dropped, REAL_GPUI_RELEASE);
  assertReported(withCi(dropped), 'does not install libpipewire-0.3-dev');
  assert.ok(aptPackages(ci).includes('ffmpeg') && !aptPackages(REAL_GPUI_RELEASE).includes('ffmpeg'), 'test-only packages stay out of the release');
});

test('every pinned step of the GPUI release workflow is enforced', () => {
  for (const [job, pins] of Object.entries(GPUI_RELEASE_STEPS)) {
    for (const pin of pins) {
      const needle = pin.replace(/^run: /, '');
      if (!REAL_GPUI_RELEASE.includes(needle)) assert.fail(`the pin is not in the workflow: ${pin}`);
      const problems = gpuiReleaseProblems((t) => t.split(needle).join('true'));
      assert.ok(problems.some((p) => p.includes(`job "${job}" lost its pinned step`) || p.includes(job)), `${job}: ${pin}\n${problems.join('\n')}`);
    }
  }
});

test('the GPUI release attests exactly the five assets', () => {
  assertReported(gpuiReleaseProblems((t) => t.replace('            dist/dilla-${{ needs.tag.outputs.version }}-linux-x64.rpm\n', '')), 'attests exactly the five assets');
  assertReported(gpuiReleaseProblems((t) => t.replace('            dist/dilla-${{ needs.tag.outputs.version }}-linux-x64.rpm\n', '            dist/dilla-${{ needs.tag.outputs.version }}-linux-x64.rpm\n            dist/*\n')), 'attests exactly the five assets');
});

test('the GPUI release publishes only after the smoke tests, from a checked draft', () => {
  assertReported(gpuiReleaseProblems((t) => t.replace('    needs: [tag, build-linux, smoke]\n', '    needs: [tag, build-linux]\n')), 'job "publish" must need exactly');
  assertReported(gpuiReleaseProblems((t) => t.replace("          - { check: arch-aur, image: 'archlinux:latest' }\n", '')), 'smoke runs exactly the checks');
  assertReported(gpuiReleaseProblems((t) => t.replace('--verify-tag --draft --prerelease', '--verify-tag --prerelease')), 'lost its pinned step');
  assertReported(gpuiReleaseProblems((t) => t.replace('"$base.deb" "$base.rpm" "$base.aur.tar.gz" dist/SHA256SUMS', '"$base.rpm" "$base.aur.tar.gz" dist/SHA256SUMS')), 'does not attach the .deb asset');
  assertReported(gpuiReleaseProblems((t) => t.replace("        if: matrix.check == 'ubuntu-upgrade'", "        if: always() && matrix.check == 'ubuntu-upgrade'")), 'forbidden "if: always()"');
  assertReported(gpuiReleaseProblems((t) => t.replace("  publish:\n    runs-on: ubuntu-24.04\n", "  publish:\n    if: success() || failure()\n    runs-on: ubuntu-24.04\n")), 'status check');
});

test('the GPUI release tag gate, permissions and triggers are enforced', () => {
  assertReported(gpuiReleaseProblems((t) => t.replace("tags: ['desktop-gpui-v*']", "tags: ['desktop-v*']")), "tags 'desktop-gpui-v*' only");
  assertReported(gpuiReleaseProblems((t) => t.replace('on:\n  push:\n', 'on:\n  pull_request:\n  push:\n')), 'runs only on a tag push');
  assertReported(gpuiReleaseProblems((t) => t.replace('    permissions:\n      contents: write\n', '    permissions:\n      contents: write\n      actions: write\n')), 'job "publish" permissions');
  assertReported(gpuiReleaseProblems((t) => t.replace('      id-token: write\n', '')), 'job "build-linux" permissions');
  assertReported(gpuiReleaseProblems((t) => t.replace('          INPUT_TAG: ${{ inputs.tag }}\n', '').replace('tag="${INPUT_TAG:-$GITHUB_REF_NAME}"', 'tag="${{ inputs.tag }}"')), 'workflow_dispatch input is expanded outside an env');
  assertReported(gpuiReleaseProblems((t) => t.replace('uses: actions/attest-build-provenance@4d101475d8b20a2381f78447822ac1eab6504dd8', 'uses: actions/attest-build-provenance@v4')), 'must use a 40-hex commit SHA');
  assertReported(gpuiReleaseProblems((t) => t.replace('          persist-credentials: false\n      - name: The checkout is the tagged commit', '          persist-credentials: false\n          ref: main\n      - name: The checkout is the tagged commit')), 'checks out ref main');
});

test('the GPUI release leaves its release a draft for desktop-gpui-sign.yml (Task 14a)', () => {
  const publishStep = '      - name: Publish the release\n        env:\n          GH_TOKEN: ${{ github.token }}\n        run: gh release edit "$TAG" --draft=false\n';
  assertReported(gpuiReleaseProblems((t) => `${t}${publishStep}`), 'takes the release out of draft');
  assertReported(gpuiReleaseProblems((t) => t.replace('--verify-tag --draft --prerelease', '--verify-tag --draft=false --prerelease')), 'takes the release out of draft');
  assertReported(gpuiReleaseProblems((t) => `${t}      - run: gh api -X PATCH "repos/$GITHUB_REPOSITORY/releases/1" -F draft=false\n`), 'takes the release out of draft');
});

// desktop-gpui-sign.yml (GPUI release plan, Task 14a): main's workflow_run workflow, the only one holding release keys.
const GPUI_SIGN = '.github/workflows/desktop-gpui-sign.yml';
const REAL_GPUI_SIGN = readFileSync(join(REPO_ROOT, GPUI_SIGN), 'utf8');
function gpuiSignProblems(edit) {
  const edited = edit(REAL_GPUI_SIGN);
  assert.notEqual(edited, REAL_GPUI_SIGN, 'the edit changed nothing; the fixture drifted from desktop-gpui-sign.yml');
  const dir = mkdtempSync(join(TEST_ROOT, 'dilla-gpui-sign-'));
  mkdirSync(join(dir, '.github', 'workflows'), { recursive: true });
  writeFileSync(join(dir, GPUI_SIGN), edited);
  writeFileSync(join(dir, GPUI_RELEASE), REAL_GPUI_RELEASE);
  return checkDesktopGpuiSignWorkflow(dir);
}
/** Replaces `from` (which must exist) within the named job only. */
const inJob = (job, from, to) => (t) => {
  const start = t.indexOf(`\n  ${job}:\n`);
  const next = t.slice(start + 1).search(/\n {2}[a-z-]+:\n/);
  const end = next < 0 ? t.length : start + 1 + next;
  const body = t.slice(start, end);
  assert.ok(start >= 0 && body.includes(from), `${job}: ${from}`);
  return t.slice(0, start) + body.replace(from, to) + t.slice(end);
};

test('the real GPUI sign workflow passes every rule, and the CLI runs the check', () => {
  assert.deepEqual(checkDesktopGpuiSignWorkflow(REPO_ROOT), []);
  const dir = mkdtempSync(join(TEST_ROOT, 'dilla-gpui-sign-missing-'));
  assert.deepEqual(checkDesktopGpuiSignWorkflow(dir), ['desktop-gpui-sign.yml: missing']);
  assert.match(readFileSync(SCRIPT_PATH, 'utf8'), /\.\.\.checkDesktopGpuiSignWorkflow\(root\)/);
});

test('the GPUI sign workflow runs main\'s file on the release workflow\'s completion only, with no workflow token', () => {
  assertReported(gpuiSignProblems((t) => t.replace("workflows: ['desktop gpui release']", "workflows: ['desktop gpui']")), 'must run only on workflow_run');
  assertReported(gpuiSignProblems((t) => t.replace('    types: [completed]\n', '    types: [completed]\n  workflow_dispatch:\n')), 'must run only on workflow_run');
  assertReported(gpuiSignProblems((t) => t.replace('    types: [completed]\n', '    types: [completed]\n    branches: [main]\n')), 'must run only on workflow_run');
  assertReported(gpuiSignProblems((t) => t.replace('permissions: {}\n', 'permissions:\n  contents: read\n')), 'workflow permissions must be exactly {}');
  assertReported(gpuiSignProblems((t) => t.replace('permissions: {}\n', 'permissions: {}\nconcurrency: sign\n')), 'workflow concurrency group');
  assertReported(gpuiSignProblems((t) => t.replace("    if: github.event.workflow_run.conclusion == 'success'\n", '')), 'job "gate" must carry exactly if:');
  assertReported(gpuiSignProblems((t) => t.replace('HINT_TAG: ${{ github.event.workflow_run.head_branch }}', 'HINT_TAG: ${{ github.event.workflow_run.head_sha }}')), "trusts the triggering run's");
});

test('the GPUI sign workflow names each secret in its one signing step only, in desktop-release', () => {
  assertReported(gpuiSignProblems(inJob('build-win', '      VERSION: ${{ needs.gate.outputs.version }}\n', '      VERSION: ${{ needs.gate.outputs.version }}\n      CSC_LINK: ${{ secrets.CSC_LINK }}\n')), 'names a secret at job level');
  assertReported(gpuiSignProblems(inJob('build-mac', '      - name: The DMG and the app pass Gatekeeper and run\n', '      - name: The DMG and the app pass Gatekeeper and run\n        env:\n          CSC_LINK: ${{ secrets.CSC_LINK }}\n')), 'names a secret it may not');
  assertReported(gpuiSignProblems(inJob('sign', '          DESKTOP_METADATA_KEY_A: ${{ secrets.DESKTOP_METADATA_KEY_A }}', '          DESKTOP_METADATA_KEY_A: ${{ secrets.DESKTOP_METADATA_KEY_B }}')), 'names a secret it may not');
  assertReported(gpuiSignProblems(inJob('sign', '          DESKTOP_METADATA_KEY_A: ${{ secrets.DESKTOP_METADATA_KEY_A }}', '          DESKTOP_METADATA_KEY_A: ${{ secrets.DESKTOP_METADATA_KEY_A }}\n          CSC_LINK: ${{ secrets.CSC_LINK }}')), 'names a secret it may not');
  assertReported(gpuiSignProblems(inJob('publish', '          GH_TOKEN: ${{ github.token }}\n          RELEASE_ID', '          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}\n          RELEASE_ID')), 'names a secret it may not');
  assertReported(gpuiSignProblems(inJob('sign', '        run: node scripts/desktop-gpui-feed.mjs build', '        run: echo "${{ toJSON(secrets) }}" && node scripts/desktop-gpui-feed.mjs build')), 'toJSON(secrets)');
  assertReported(gpuiSignProblems(inJob('sign', '    environment: desktop-release\n', '')), 'job "sign" must run in environment: desktop-release');
  assertReported(gpuiSignProblems(inJob('build-mac', '    environment: desktop-release\n', '')), 'job "build-mac" must run in environment: desktop-release');
  assertReported(gpuiSignProblems(inJob('publish', '    runs-on: ubuntu-24.04\n', '    runs-on: ubuntu-24.04\n    environment: desktop-release\n')), 'job "publish" must enter no environment');
  assertReported(gpuiSignProblems(inJob('build-mac', '      - name: Package, sign and notarize Dilla.app and the DMG (the only step with the Apple secrets)\n', '      - name: Package and sign\n')), 'lost its signing step');
  assertReported(gpuiSignProblems(inJob('build-mac', '[ -z "$missing" ] || { echo', '[ -n "$missing" ] && { echo')), 'lost its pinned step');
});

test('the GPUI sign workflow runs once, builds the gated commit cleanly and checks main\'s code out elsewhere', () => {
  assertReported(gpuiSignProblems(inJob('sign', '      - name: First attempt only\n        run: test "$GITHUB_RUN_ATTEMPT" = 1\n', '')), 'job "sign" must start with the step');
  assertReported(gpuiSignProblems(inJob('publish', '      - name: First attempt only\n        run: test "$GITHUB_RUN_ATTEMPT" = 1\n', '')), 'job "publish" must start with the step');
  assertReported(gpuiSignProblems(inJob('build-mac', '          ref: ${{ needs.gate.outputs.sha }}\n', '          ref: ${{ github.event.workflow_run.head_branch }}\n')), 'job "build-mac" must check out ref');
  assertReported(gpuiSignProblems(inJob('sign', '      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7\n        with:\n', '      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7\n        with:\n          ref: ${{ needs.gate.outputs.sha }}\n')), "it runs main's code only");
  assertReported(gpuiSignProblems(inJob('build-win', 'run: cargo build --release --locked -p dilla-desktop', 'run: cargo build --release -p dilla-desktop')), 'build-win runs exactly one');
  assertReported(gpuiSignProblems(inJob('build-mac', '          DILLA_BUILD_SHA: ${{ needs.gate.outputs.sha }}\n', '')), 'build-mac runs exactly one');
  assertReported(gpuiSignProblems(inJob('build-mac', '      - run: rustup show active-toolchain\n', '      - uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0\n        with:\n          path: target\n          key: k\n      - run: rustup show active-toolchain\n')), 'restores or saves a cache');
  assertReported(gpuiSignProblems(inJob('build-win', '          package-manager-cache: false\n', '')), 'package-manager-cache: false');
  assertReported(gpuiSignProblems(inJob('sign', '          name: desktop-gpui-win-x64\n          path: assets\n          digest-mismatch: error\n', '          name: desktop-gpui-win-x64\n          path: assets\n')), 'without digest-mismatch: error');
  assertReported(gpuiSignProblems(inJob('gate', '    permissions:\n      contents: write\n', '    permissions:\n      contents: write\n      packages: write\n')), 'job "gate" permissions');
  assertReported(gpuiSignProblems(inJob('sign', '    permissions:\n      contents: read\n', '    permissions:\n      contents: write\n')), 'job "sign" permissions');
  assertReported(gpuiSignProblems(inJob('build-mac', '      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7', '      - uses: actions/checkout@v7')), 'must use a 40-hex commit SHA');
});

test('every pinned step of the GPUI sign workflow is enforced', () => {
  for (const [job, pins] of Object.entries(GPUI_SIGN_STEPS)) {
    for (const pin of pins) {
      const needle = pin.replace(/^run: /, '');
      if (!REAL_GPUI_SIGN.includes(needle)) assert.fail(`the pin is not in the workflow: ${pin}`);
      const problems = gpuiSignProblems((t) => t.split(needle).join('true'));
      assert.ok(problems.some((p) => p.includes(`job "${job}"`) || p.includes(job)), `${job}: ${pin}\n${problems.join('\n')}`);
    }
  }
});

test('the GPUI sign workflow publishes last, never on the dry-run tag, and deploys the feed after it', () => {
  const PUBLISH_IF = "        if: needs.gate.outputs.dry_run == 'false'\n        env:\n          GH_TOKEN: ${{ github.token }}\n        run: gh release edit";
  assertReported(gpuiSignProblems((t) => t.replace(PUBLISH_IF, PUBLISH_IF.replace("        if: needs.gate.outputs.dry_run == 'false'\n", ''))), 'the D9 dry-run tag is never published');
  assertReported(gpuiSignProblems((t) => t.replace("    needs: [gate, sign, publish]\n    if: needs.gate.outputs.dry_run == 'false'\n", '    needs: [gate, sign, publish]\n')), 'job "feed" must carry exactly if:');
  assertReported(gpuiSignProblems((t) => t.replace("    needs: [gate, sign, publish]\n    if: needs.gate.outputs.dry_run == 'false'\n", "    needs: [gate, sign, publish]\n    if: ${{ !cancelled() && needs.gate.outputs.dry_run == 'false' }}\n")), 'job "feed" must carry exactly if:');
  assertReported(gpuiSignProblems((t) => t.replace('    needs: [gate, sign, publish]\n', '    needs: [gate, sign]\n')), 'job "feed" must need exactly');
  assertReported(gpuiSignProblems((t) => t.replace('    needs: [gate, build-mac, build-win, sign]\n', '    needs: [gate, build-mac, build-win]\n')), 'job "publish" must need exactly');
  assertReported(gpuiSignProblems(inJob('sign', '    runs-on: ubuntu-24.04\n', '    if: always()\n    runs-on: ubuntu-24.04\n')), 'forbidden "if: always()"');
  assertReported(gpuiSignProblems(inJob('feed', '      group: desktop-release-feed\n', '      group: feed\n')), 'concurrency group desktop-release-feed');
  // The undraft moved before the re-download check, or into a second step, is refused.
  assertReported(gpuiSignProblems(inJob('publish', '          gh release upload "$TAG" "$RUNNER_TEMP/sums/SHA256SUMS" --clobber\n', '          gh release upload "$TAG" "$RUNNER_TEMP/sums/SHA256SUMS" --clobber\n          gh release edit "$TAG" --draft=false\n')), 'exactly one step');
  assertReported(gpuiSignProblems((t) => t.replace('  feed:\n', '  aur:\n    needs: [gate]\n    runs-on: ubuntu-24.04\n    steps:\n      - run: git push ssh://aur@aur.archlinux.org/dilla-bin.git\n  feed:\n')), 'release asset only (D4)');
});

test('the gpui-win-package job pins beta.5\'s installer, the uninstall GUID, the static CRT, the order and no secrets', () => {
  for (const [what, from, to, expect] of [
    ['sha', '0cf2c3ad5c94b9cd4aa21956e6d37209dafe06fa525fcd7866240379fadcf1bb', '0'.repeat(64), 'SHA-256'],
    ['guid', 'BETA5_UNINSTALL_GUID: f6db60e4-10df-5b69-9206-408d5f2cebe9', 'BETA5_UNINSTALL_GUID: 00000000-0000-5000-8000-000000000000', 'uninstall GUID'],
    ['crt', 'echo "RUSTFLAGS=-C target-feature=+crt-static" >> "$GITHUB_ENV"\n      # The release\'s', 'echo nothing\n      # The release\'s', 'pinned step'],
    ['build', 'cargo build -p dilla-desktop --release --locked', 'cargo build -p dilla-desktop --release', 'pinned step'],
    ['runner', 'runs-on: windows-latest\n    timeout-minutes: 120  # a cold release', 'runs-on: ubuntu-24.04\n    timeout-minutes: 120  # a cold release', 'windows-latest'],
    ['secret', "    env:\n      BETA5_WIN_SHA256", "    env:\n      X: ${{ secrets.X }}\n      BETA5_WIN_SHA256", 'no secret'],
    ['order', "'/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', \"/LOG=", "'/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', \"/LOGX=", 'in that order'],
  ]) {
    // Mutate inside the gpui-win-package job only: other jobs (gpui-mac-package) share some of these lines.
    const problems = gpuiProblems((text) => {
      const at = text.indexOf('  gpui-win-package:\n');
      assert.ok(at >= 0 && text.indexOf(from, at) >= 0, what);
      return text.slice(0, at) + text.slice(at).replace(from, to);
    });
    assert.ok(problems.some((p) => p.includes('gpui-win-package') && p.includes(expect)), `${what}: ${problems.join('|')}`);
  }
  assert.ok(gpuiProblems((text) => text.replace('  gpui-win-package:\n', '  gpui-win-package-x:\n')).some((p) => p.includes('job is missing')));
});
