import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { spawnSync } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { gateExpression, JOB_GATES } from './ci-areas.mjs';
import { BROWSER_WEB_JOBS, browserSuiteCommand, checkReleaseWorkflow, checkWorkflow, jobSteps, MIN_BROWSER_WEB_TIMEOUT, NON_RACE_STEP, raceGatedTests, RELEASE_ASSETS, WASM_CLANG_ENV_BLOCK, WASM_CLANG_STEP, WASM_LLVM_KEY_FPR, WASM_LLVM_VERSION, WEB_SHARDS } from './check-ci-workflow.mjs';

const NPM_TEST = "      - if: needs.changes.outputs.web != 'false'\n        run: npm test\n";
const SCRIPT_PATH = fileURLToPath(new URL('./check-ci-workflow.mjs', import.meta.url));
const REPO_ROOT = join(dirname(SCRIPT_PATH), '..');
const REAL_CI = readFileSync(join(REPO_ROOT, '.github', 'workflows', 'ci.yml'), 'utf8');
const REAL_RELEASE = readFileSync(join(REPO_ROOT, '.github', 'workflows', 'release.yml'), 'utf8');

test('compiled helper recovery tests receive the same-commit helper and cannot silently skip', () => {
  const native = REAL_CI.split('\n  rust-native:\n')[1].split('\n  spotify-helper:\n')[0];
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
  assert.ok(run.some(line => line.includes('DILLA_BOT_HELPER_BIN: ${{ github.workspace }}/target/release/dilla-bot-core')));
  const steps = jobSteps(go);
  const fixtureBuild = steps.findIndex(step => step.some(line => line.includes('cargo test -p dilla-core --test native_bot_group_peer_fixture --no-run --locked')));
  const integration = steps.findIndex(step => step.some(line => line.includes('go test -race ./internal/bot/native ./cmd/dillabot')));
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
    assert.match(REAL_CI, new RegExp(`^  ${job}:\\n(?:    (?:needs|if):.*\\n)*    runs-on: ubuntu-24\\.04$`, 'm'));
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

// The go job's test step: every package but internal/ds and internal/api, which go-ds and go-api run (CI budget, fix wave).
const GO_TEST = 'go test -race -shuffle=on -timeout 20m $(go list ./... | grep -vx -e github.com/jonasthim/dilla/internal/ds -e github.com/jonasthim/dilla/internal/api)';

/** The browser jobs of GOOD, generated from BROWSER_WEB_JOBS: one job per engine, suite and web shard. */
function browserJobsFixture() {
  return Object.entries(BROWSER_WEB_JOBS).map(([job, spec]) => `  ${job}:
    runs-on: ubuntu-latest
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
        if: failure()
        with:
          name: browser-web-results-${job}
          path: e2e/test-results
          if-no-files-found: error
`).join('\n');
}

const GOOD_BASE = `name: ci
on:
  push:
    branches: [main]
  pull_request:
permissions:
  contents: read
concurrency:
  group: \${{ github.workflow }}-\${{ github.ref }}
  cancel-in-progress: true
env:
  CARGO_TERM_COLOR: always
jobs:
  node:
    runs-on: ubuntu-latest
    steps:
      - if: needs.changes.outputs.web != 'false'
        run: npm test
      - name: vectors are up to date
        if: needs.changes.outputs.web != 'false'
        run: npm run vectors
      - run: npm run test:css-check
      - run: npm run check:css
      - run: npm run test:ci-check
      - run: npm run test:wasm-size-check
      - run: npm run test:manifest-writer
      - run: npm run test:install
      - uses: actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        if: needs.changes.outputs.text != 'false'
        with:
          path: \${{ runner.temp }}/ucd
          key: ucd-\${{ hashFiles('scripts/gen-profile-text-tables.mjs') }}
      - name: profile-text tables are current
        if: needs.changes.outputs.text != 'false'
        run: node scripts/gen-profile-text-tables.mjs --download --check "$RUNNER_TEMP/ucd"
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
        run: cargo clippy --workspace --all-targets --all-features --locked -- -D warnings
      - if: needs.changes.outputs.core != 'false'
        run: |
          sudo prlimit --pid "$$" --memlock=268435456:268435456
          cargo test --workspace --all-features --locked
      - run: cargo build -p dilla-testkit --release --locked
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
  spotify-helper:
    runs-on: ubuntu-latest
    steps:
      - run: cargo fmt --manifest-path crates/dilla-spotify-helper/Cargo.toml -- --check
      - run: cargo clippy --manifest-path crates/dilla-spotify-helper/Cargo.toml --all-targets --locked -- -D warnings
      - run: cargo test --manifest-path crates/dilla-spotify-helper/Cargo.toml --locked
      - run: cargo build --manifest-path crates/dilla-spotify-helper/Cargo.toml --release --locked
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
    runs-on: ubuntu-latest
    timeout-minutes: 30
    needs: [rust-wasi]
    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - run: go vet ./...
      - run: CGO_ENABLED=0 go build -tags dillapins ./internal/deps
      - run: go test -race -shuffle=on -timeout 20m $(go list ./... | grep -vx -e github.com/jonasthim/dilla/internal/ds -e github.com/jonasthim/dilla/internal/api)
      - run: ${NON_RACE_STEP}
      - run: CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' ./cmd/dillad

  browser-media:
    runs-on: ubuntu-latest
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
      - run: npx playwright install --with-deps chromium firefox
      - run: sh scripts/ci-audio-server.sh
      - run: npm run test:e2e:media -w @dilla/e2e
        env:
          DILLA_TESTKIT: \${{ github.workspace }}/artifacts/dilla-testkit
      - run: npx playwright test --config e2e/playwright.media.config.ts --project=chromium-media unsupported-sfu-codec.spec.ts
        env:
          DILLA_MEDIA_SFU_AV1: '1'
          DILLA_TESTKIT: \${{ github.workspace }}/artifacts/dilla-testkit
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        if: failure()
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
    runs-on: ubuntu-latest
    timeout-minutes: 55
    needs: [rust-wasi]
    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - run: go test -race -shuffle=on -timeout 45m ./internal/ds/...

  go-api:
    runs-on: ubuntu-latest
    timeout-minutes: 55
    needs: [rust-wasi]
    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - run: go test -race -shuffle=on -timeout 50m ./internal/api/...

  go-harness:
    runs-on: ubuntu-latest
    timeout-minutes: 45
    needs: [rust-wasi, rust-native]
    steps:
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-core-wasi
          path: internal/mlswasi/testdata
      - uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          name: dilla-testkit
          path: artifacts
      - run: go test -race -shuffle=on -timeout 38m ./internal/testkit/...
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
    runs-on: ubuntu-latest
    needs: [rust-wasi]
    services:
      postgres:
        image: postgres:18.6-alpine3.24
        env:
          POSTGRES_INITDB_ARGS: --locale-provider=builtin --builtin-locale=C.UTF-8
    steps:
      - run: go test -race -shuffle=on -timeout 20m ./internal/store/... ./internal/ops/...
        env:
          DILLA_TEST_PG: postgres://dilla:dilla@127.0.0.1:5432/dilla?sslmode=disable
      - run: go test -race -shuffle=on -timeout 20m -run 'AFK|Afk|HiddenCategory|HiddenParent|ParentAnywhere|Marker|ConcurrentEnsures|ReadsThePointer' ./internal/api/...
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
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with:
          name: dillad-binaries
          path: dist/
          if-no-files-found: error

`;

// The four jobs that build for wasm32-unknown-unknown, as ci.yml writes them: the canonical clang 23 job env and,
// right after checkout, the canonical pinned install step (ruling of 2026-10-13; hardened after the d3429ce lead).
const WASM_JOBS = ['rust-wasm-node', 'browser-spike', 'browser-media', 'web-build'];

function withWasmClang(text) {
  const out = [];
  let job = null;
  for (const line of text.split('\n')) {
    const key = /^ {2}([A-Za-z0-9_-]+):\s*$/.exec(line);
    if (key) job = key[1];
    if (WASM_JOBS.includes(job) && line === '    steps:') {
      out.push(WASM_CLANG_ENV_BLOCK, line, '      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7', WASM_CLANG_STEP);
      continue;
    }
    out.push(line);
  }
  return out.join('\n');
}

// The shape ci.yml gives the change filter: a `changes` job, a `docs` job that always runs, and every other job gated
// on `changes` as JOB_GATES says (its own needs merged in).
const CHANGES_JOB = `  changes:
    runs-on: ubuntu-latest
    outputs:
      go: \${{ steps.areas.outputs.go }}
      core: \${{ steps.areas.outputs.core }}
      web: \${{ steps.areas.outputs.web }}
      e2e: \${{ steps.areas.outputs.e2e }}
      text: \${{ steps.areas.outputs.text }}
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
  docs:
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

const CI_OK_JOB = (jobs) => `  ci-ok:
    needs: [${jobs.join(', ')}]
    if: always()
    runs-on: ubuntu-latest
    steps:
      - if: contains(needs.*.result, 'failure') || contains(needs.*.result, 'cancelled')
        run: exit 1
      - run: echo ok
`;

function withChanges(text) {
  const lines = text.replace('jobs:\n', `jobs:\n${CHANGES_JOB}`).split('\n');
  const out = [];
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const key = /^ {2}([A-Za-z0-9_-]+):\s*$/.exec(line);
    out.push(line);
    if (key === null || !(key[1] in JOB_GATES) || !lines.slice(0, i).includes('jobs:')) continue;
    let needs = [];
    for (let j = i + 1; j < lines.length && !/^ {2}\S/.test(lines[j]); j++) {
      const m = /^ {4}needs: \[(.*)\]$/.exec(lines[j]);
      if (m) { needs = m[1].split(',').map((s) => s.trim()); lines.splice(j, 1); break; }
    }
    out.push(`    needs: [${['changes', ...needs].join(', ')}]`, `    if: ${gateExpression(key[1])}`);
  }
  const jobs = out.filter((line) => /^ {2}[A-Za-z0-9_-]+:\s*$/.test(line)).map((line) => line.trim().slice(0, -1)).filter((name, i, all) => all.indexOf(name) === i && out.indexOf(`  ${name}:`) > out.indexOf('jobs:'));
  return `${out.join('\n').replace(/\n$/, '')}\n${CI_OK_JOB(jobs)}`;
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

// A repository root holding ci.yml and, as Go test files, the race-gated tests in `gated`.
function fixture(body, gated = GATED) {
  const dir = mkdtempSync(join(tmpdir(), 'dilla-ci-'));
  mkdirSync(join(dir, '.github', 'workflows'), { recursive: true });
  writeFileSync(join(dir, '.github', 'workflows', 'ci.yml'), body);
  // The CLI also checks release.yml.
  writeFileSync(join(dir, '.github', 'workflows', 'release.yml'), REAL_RELEASE);
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
  const problems = checkWorkflow(fixture(GOOD.replace(/(  go-harness:\n(?:    (?:needs|if):.*\n)*    runs-on: ubuntu-latest\n)    timeout-minutes: 45\n/, '$1')));
  assert.ok(problems.some((p) => p.includes('timeout-minutes: 45')), problems.join('\n'));
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
  assert.ok(problems.some((p) => p.includes('must be exactly')), problems.join('\n'));
});

// CI budget (fix wave): the go job leaves out internal/ds and nothing else, and go-ds runs it.
test('a go job that leaves out more than internal/ds and internal/api is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace('-e github.com/jonasthim/dilla/internal/api)', '-e github.com/jonasthim/dilla/internal/api -e github.com/jonasthim/dilla/internal/store)')),
  );
  assert.ok(problems.some((p) => p.includes('must be exactly')), problems.join('\n'));
});

// web3.9: the css checker's own test and the check itself, and the AFK / channel-parent tests on Postgres.
test('a node job without the css checker steps is reported', () => {
  for (const step of ['npm run test:css-check', 'npm run check:css']) {
    const problems = checkWorkflow(fixture(GOOD.replace(`      - run: ${step}\n`, '')));
    assert.ok(problems.some((p) => p.includes('"node"') && p.includes(step)), problems.join('\n'));
  }
});

test('a go-postgres job that stopped running the api AFK tests is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/      - run: go test -race -shuffle=on -timeout 20m -run 'AFK[^\n]*\n        env:\n[^\n]*\n/, '')));
  assert.ok(problems.some((p) => p.includes('"go-postgres"') && p.includes('./internal/api/...')), problems.join('\n'));
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
    fixture(GOOD.replace('      - run: go test -race -shuffle=on -timeout 45m ./internal/ds/...\n', '')),
  );
  assert.ok(problems.some((p) => p.includes('"go-ds"') && p.includes('./internal/ds/...')), problems.join('\n'));
});

test('a go-ds job with a shorter test budget is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace('      - run: go test -race -shuffle=on -timeout 45m ./internal/ds/...', '      - run: go test -race -shuffle=on -timeout 20m ./internal/ds/...')),
  );
  assert.ok(problems.some((p) => p.includes('"go-ds"') && p.includes('-timeout 45m')), problems.join('\n'));
});

test('a go-ds job with too little wall-clock margin is reported', () => {
  const start = GOOD.indexOf('  go-ds:\n');
  const body = GOOD.slice(start, GOOD.indexOf('\n\n', start));
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace('timeout-minutes: 55', 'timeout-minutes: 25'))));
  assert.ok(problems.some((p) => p.includes('"go-ds"') && p.includes('timeout-minutes: 55')), problems.join('\n'));
});

test('a workflow without the go-api job is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/  go-api:[\s\S]*?\n\n/, '')));
  assert.ok(problems.some((p) => p.includes('missing job "go-api"')), problems.join('\n'));
});

test('a go-api job that stopped running internal/api is reported', () => {
  const problems = checkWorkflow(
    fixture(GOOD.replace('      - run: go test -race -shuffle=on -timeout 50m ./internal/api/...\n', '')),
  );
  assert.ok(problems.some((p) => p.includes('"go-api"') && p.includes('./internal/api/...')), problems.join('\n'));
});

test('a go-api job with a shorter budget is reported', () => {
  const start = GOOD.indexOf('  go-api:\n');
  const body = GOOD.slice(start, GOOD.indexOf('\n\n', start));
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace('timeout-minutes: 55', 'timeout-minutes: 30'))));
  assert.ok(problems.some((p) => p.includes('"go-api"') && p.includes('timeout-minutes: 55')), problems.join('\n'));
});

test('a go job that stopped leaving out internal/api is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(' -e github.com/jonasthim/dilla/internal/api', '')));
  assert.ok(problems.some((p) => p.includes('must be exactly')), problems.join('\n'));
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
  ['go-harness', 'go test -race -shuffle=on -timeout 38m ./internal/testkit/...'],
  ['go-harness', 'name: dilla-testkit'],
  ['go-lint', 'golangci-lint run ./...'],
  ['go-vuln', '-scan package -tags dillapins ./internal/deps'],
  ['go-sqlc', 'sqlc diff'],
  ['go-postgres', 'postgres:18.6-alpine3.24'],
  ['go-postgres', '--locale-provider=builtin --builtin-locale=C.UTF-8'],
  ['go-postgres', 'DILLA_TEST_PG'],
  // I7 (fix wave): internal/ops holds the only Postgres backup, restore and heal tests; a step that
  // drops it skips them in every job. The -timeout is explicit so a slow package fails by name.
  ['go-postgres', 'go test -race -shuffle=on -timeout 20m ./internal/store/... ./internal/ops/...'],
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
// accident — it would fail to find ci.yml at all rather than happening to validate the right file.
function runCli(root, cwd) {
  return spawnSync(process.execPath, [SCRIPT_PATH, root], { cwd, encoding: 'utf8' });
}

test('the CLI honours a root argument passed from a different cwd', () => {
  // The real ci.yml: the CLI also checks release.yml, whose actions must be ci.yml's at the same versions.
  const root = fixture(REAL_CI);
  const cwd = mkdtempSync(join(tmpdir(), 'dilla-ci-cwd-'));
  const result = runCli(root, cwd);
  assert.equal(result.status, 0, `stdout: ${result.stdout}\nstderr: ${result.stderr}`);
});

test('the CLI reports problems in a workflow at the given root, run from elsewhere', () => {
  const root = fixture(GOOD.replace(/  deny:[\s\S]*$/, ''));
  const cwd = mkdtempSync(join(tmpdir(), 'dilla-ci-cwd-'));
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
    const problems = checkWorkflow(fixture(GOOD.replace('  ci-ok:', () => `  image:\n    runs-on: ubuntu-latest\n    steps:\n${addition}  ci-ok:`)));
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
    problems.map((p) => p.replace(/^ci\.yml:\d+: /, '')).filter((p) => !p.includes('JOB_GATES') && !p.includes('"ci-ok"')),
    ['actions/upload-artifact without "if-no-files-found: error" — CI is fail-closed'],
  );
  // The same step with the setting is clean.
  assert.deepEqual(checkWorkflow(fixture(GOOD + extra.replace('          path: y\n', '          path: y\n          if-no-files-found: error\n'))).filter((p) => !p.includes('JOB_GATES') && !p.includes('"ci-ok"')), []);
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
  'npx playwright install --with-deps chromium firefox',
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
  const problems = checkWorkflow(fixture(GOOD.replace('    timeout-minutes: 20\n', '')));
  assert.ok(problems.some((p) => p.includes('"browser-spike"') && p.includes('timeout-minutes: 20')), problems.join('\n'));
});

test('the video publishing tests retain a non-race CI step', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(NON_RACE_STEP, 'go test ./cmd/dilla-mediabot')));
  assert.ok(problems.some((p) => p.includes('job "go"') && p.includes(NON_RACE_STEP)), problems.join('\n'));
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
  const lineNo = body.split('\n').findIndex((l) => l.includes('--release --mode no-install --out-dir spike/pkg')) + 1;
  const problems = checkWorkflow(fixture(body));
  assert.ok(
    problems.includes(`ci.yml:${lineNo}: wasm-pack build must use --profile wasm-release, not --release (C16)`),
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
    [job, 'if: failure()'],
    [job, `name: browser-web-results-${job}`],
    [job, 'path: e2e/test-results'],
  ]),
  ['go-release', 'name: dilla-web-dist'],
  ['go-release', 'path: internal/web/dist'],
  ['go-release', 'content="placeholder"'],
  ['go-release', 'go test -tags webdist ./internal/web/...'],
  ['node', 'npm run test:manifest-writer'],
  ['node', 'npm run test:install'],
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

// Jonas, 2026-10-30: each engine splits into a core job, a voice job and the web suite as three Playwright shards, one
// job each. Dropping a job, folding suites back into one job, an unpinned shard or two jobs uploading one artifact name
// (upload-artifact v4+ refuses a duplicate) is reported.
test('the browser job table covers core, voice and three web shards for Chromium and Firefox, and WebKit web', () => {
  const rows = Object.entries(BROWSER_WEB_JOBS).map(([job, { engine, suite, shard, timeout }]) => `${job} ${engine} ${suite} ${shard} ${timeout}`);
  assert.deepEqual(rows, [
    'chromium-core chromium core null 20',
    'chromium-voice chromium voice null 30',
    'chromium-web-1 chromium web 1 35',
    'chromium-web-2 chromium web 2 35',
    'chromium-web-3 chromium web 3 35',
    'firefox-core firefox core null 20',
    'firefox-voice firefox voice null 30',
    'firefox-web-1 firefox web 1 40',
    'firefox-web-2 firefox web 2 40',
    'firefox-web-3 firefox web 3 40',
    'webkit-web webkit web null 30',
  ]);
  assert.equal(WEB_SHARDS, 3);
});

test('the shard commands are the web suite with --shard=i/3', () => {
  assert.equal(browserSuiteCommand(BROWSER_WEB_JOBS['chromium-web-2']), 'npm run test:e2e:web:chromium -w @dilla/e2e -- --shard=2/3');
  assert.equal(browserSuiteCommand(BROWSER_WEB_JOBS['firefox-voice']), 'npm run test:e2e:voice:firefox -w @dilla/e2e');
  assert.equal(browserSuiteCommand(BROWSER_WEB_JOBS['webkit-web']), 'npm run test:e2e:web:webkit -w @dilla/e2e');
});

test('the old per-engine jobs are no longer accepted', () => {
  assert.ok(!('chromium-web' in BROWSER_WEB_JOBS) && !('firefox-web' in BROWSER_WEB_JOBS));
  const old = '  chromium-web:\n    needs: [changes, rust-native, rust-wasi, web-build]\n    if: needs.changes.outputs.e2e != \'false\'\n    steps:\n      - run: npm run test:e2e:core:chromium -w @dilla/e2e\n';
  const problems = checkWorkflow(fixture(GOOD.replace('  deny:', () => `${old}\n  deny:`)));
  assert.ok(problems.some((p) => p.includes('"chromium-web"') && p.includes('JOB_GATES')), problems.join('\n'));
});

test('a workflow missing one split job is reported, also when another job runs that suite', () => {
  const shard = jobBody('chromium-web-2');
  const problems = checkWorkflow(fixture(GOOD.replace(shard, '')));
  assert.ok(problems.some((p) => p.includes('missing job "chromium-web-2"')), problems.join('\n'));
  const merged = jobBody('chromium-web-1').replace(
    '      - run: npm run test:e2e:web:chromium -w @dilla/e2e -- --shard=1/3\n',
    '      - run: npm run test:e2e:web:chromium -w @dilla/e2e -- --shard=1/3\n        env:\n          DILLA_TESTKIT: ${{ github.workspace }}/artifacts/dilla-testkit\n      - run: npm run test:e2e:web:chromium -w @dilla/e2e -- --shard=2/3\n',
  );
  const folded = checkWorkflow(fixture(GOOD.replace(shard, '').replace(jobBody('chromium-web-1'), () => merged)));
  assert.ok(folded.some((p) => p.includes('missing job "chromium-web-2"')), folded.join('\n'));
  assert.ok(folded.some((p) => p.includes('"chromium-web-1"') && p.includes('exactly one Playwright suite')), folded.join('\n'));
});

test('a job that also runs another suite is reported', () => {
  const body = jobBody('chromium-core');
  const both = body.replace('    steps:\n', '    steps:\n      - run: npm run test:e2e:voice:chromium -w @dilla/e2e\n');
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => both)));
  assert.ok(problems.some((p) => p.includes('"chromium-core"') && p.includes('exactly one Playwright suite')), problems.join('\n'));
});

for (const [job, spec] of Object.entries(BROWSER_WEB_JOBS).filter(([, { shard }]) => shard !== null)) {
  test(`a ${job} job running the wrong shard, or the whole suite, is reported`, () => {
    const body = jobBody(job);
    const command = browserSuiteCommand(spec);
    assert.ok(body.includes(command), 'fixture sanity: the shard command');
    const other = spec.shard === 1 ? 2 : 1;
    for (const changed of [
      command.replace(`--shard=${spec.shard}/3`, `--shard=${other}/3`),
      command.replace(`--shard=${spec.shard}/3`, `--shard=${spec.shard}/4`),
      command.replace(' -- --shard=' + spec.shard + '/3', ''),
    ]) {
      const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace(command, changed))));
      assert.ok(problems.some((p) => p.includes(`"${job}"`) && p.includes(command)), `${changed}: ${problems.join('\n')}`);
    }
  });
}

// Web-3 task 10: Firefox's voice suite runs after the audio server, as browser-media's does.
test('firefox-voice must start an audio server before the voice suite', () => {
  const body = jobBody('firefox-voice');
  const audio = '      - run: sh scripts/ci-audio-server.sh\n';
  const voice = '      - run: npm run test:e2e:voice:firefox -w @dilla/e2e\n';
  assert.ok(body.includes(audio) && body.includes(voice), 'fixture sanity: the audio server and the voice suite');
  const late = body.replace(audio, '').replace(voice + '        env:\n          DILLA_TESTKIT: ${{ github.workspace }}/artifacts/dilla-testkit\n', (m) => m + audio);
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => late)));
  assert.ok(problems.some((p) => p.includes('firefox-voice') && p.includes('audio server must start before the voice suite')), problems.join('\n'));
});

// Web3.9 CI2: the web suite has calls too (afk.spec's idle move, the layout's AFK preview). Without the audio server
// Firefox's AudioContext stays suspended, so a fake microphone's tone reaches no local meter and counts as speech.
for (const shard of [1, 2, 3]) {
  const job = `firefox-web-${shard}`;
  test(`${job} must start an audio server before the web suite`, () => {
    const body = jobBody(job);
    const audio = '      - run: sh scripts/ci-audio-server.sh\n';
    const env = '        env:\n          DILLA_TESTKIT: ${{ github.workspace }}/artifacts/dilla-testkit\n';
    const web = `      - run: npm run test:e2e:web:firefox -w @dilla/e2e -- --shard=${shard}/3\n`;
    assert.ok(body.includes(audio) && body.includes(web), 'fixture sanity: the audio server and the web suite');
    const late = body.replace(audio, '').replace(web + env, (m) => m + audio);
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => late)));
    assert.ok(problems.some((p) => p.includes(job) && p.includes('audio server must start before the web suite')), problems.join('\n'));
    const missing = checkWorkflow(fixture(GOOD.replace(body, () => body.replace(audio, ''))));
    assert.ok(missing.some((p) => p.includes(`"${job}"`) && p.includes('ci-audio-server.sh')), missing.join('\n'));
  });
}

test('firefox-core and the Chromium and WebKit jobs need no audio server', () => {
  for (const job of ['firefox-core', 'chromium-core', 'chromium-voice', 'chromium-web-1', 'webkit-web']) {
    assert.ok(!jobBody(job).includes('ci-audio-server.sh'), job);
  }
  assert.deepEqual(checkWorkflow(fixture(GOOD)), []);
});

// Web-3 task 10 (review M10): the suite rules match steps, so a comment never satisfies them, and the suite step's own
// env is required.
for (const [job, spec] of Object.entries(BROWSER_WEB_JOBS)) {
  const command = browserSuiteCommand(spec);
  const step = `      - run: ${command}\n`;
  const env = '        env:\n          DILLA_TESTKIT: ${{ github.workspace }}/artifacts/dilla-testkit\n';
  test(`a ${job} suite step that is only a comment is reported`, () => {
    const body = jobBody(job);
    assert.ok(body.includes(step), 'fixture sanity: the suite step');
    const commented = body.replace(step, `      # - run: ${command}\n`);
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => commented)));
    assert.ok(problems.some((p) => p.includes(`"${job}" is missing: ${command}`)), problems.join('\n'));
  });
  test(`a ${job} suite step without its own DILLA_TESTKIT env is reported`, () => {
    const body = jobBody(job);
    assert.ok(body.includes(step + env), 'fixture sanity: the suite step and its env');
    const problems = checkWorkflow(fixture(GOOD.replace(body, () => body.replace(step + env, step))));
    assert.ok(problems.some((p) => p.includes(`"${job}"`) && p.includes('without its own env')), problems.join('\n'));
  });
}

test('a comment naming the audio server above the suite step does not stand in for the real step after it', () => {
  const body = jobBody('firefox-voice');
  const audio = '      - run: sh scripts/ci-audio-server.sh\n';
  const voice = '      - run: npm run test:e2e:voice:firefox -w @dilla/e2e\n        env:\n          DILLA_TESTKIT: ${{ github.workspace }}/artifacts/dilla-testkit\n';
  assert.ok(body.includes(audio) && body.includes(voice), 'fixture sanity: the audio server, then the voice suite');
  const sneaky = body.replace(audio, '      # sh scripts/ci-audio-server.sh\n').replace(voice, `${voice}${audio}`);
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => sneaky)));
  assert.ok(problems.some((p) => p.includes('firefox-voice') && p.includes('audio server must start before')), problems.join('\n'));
  const commentOnly = body.replace(audio, '      # - run: sh scripts/ci-audio-server.sh\n');
  const missing = checkWorkflow(fixture(GOOD.replace(body, () => commentOnly)));
  assert.ok(missing.some((p) => p.includes('"firefox-voice"') && p.includes('ci-audio-server.sh')), missing.join('\n'));
});

test('jobSteps drops comment lines and keeps each step\'s own lines', () => {
  const steps = jobSteps('    steps:\n      # - run: a\n      - run: b\n        env:\n          X: 1\n      # c\n      - uses: d\n');
  assert.deepEqual(steps, [['run: b', 'env:', 'X: 1'], ['uses: d']]);
});

test('the browser budgets keep margin over the measured durations', () => {
  // Chromium's whole web suite took 45.6 minutes on one worker, Firefox's about 47: a third of it plus about 3 minutes of
  // setup is about 19 minutes, so a shard's cap must not sink under 30.
  for (const [job, { suite, engine, timeout }] of Object.entries(BROWSER_WEB_JOBS)) {
    if (suite === 'web' && engine !== 'webkit') assert.ok(timeout >= 35, `${job}: ${timeout}`);
    if (suite === 'voice') assert.ok(timeout >= 30, `${job}: ${timeout}`);
    if (suite === 'core') assert.ok(timeout >= 20, `${job}: ${timeout}`);
  }
  assert.equal(BROWSER_WEB_JOBS['webkit-web'].timeout, 30);
});

test('two browser jobs uploading one artifact name are reported', () => {
  const body = jobBody('firefox-web-2');
  const shared = body.replace('name: browser-web-results-firefox-web-2', 'name: browser-web-results-firefox-web-1');
  const problems = checkWorkflow(fixture(GOOD.replace(body, () => shared)));
  assert.ok(problems.some((p) => p.includes('"firefox-web-2"') && p.includes('browser-web-results-firefox-web-2')), problems.join('\n'));
});

test('every browser job has its own artifact name in the real ci.yml', () => {
  const names = Object.keys(BROWSER_WEB_JOBS).map((job) => `name: browser-web-results-${job}`);
  assert.equal(new Set(names).size, names.length);
  for (const name of names) assert.equal(REAL_CI.split('\n').filter((l) => l.trim() === name).length, 1, name);
});

test('the real ci-ok job needs every browser job', () => {
  const body = REAL_CI.slice(REAL_CI.indexOf('\n  ci-ok:'));
  for (const job of Object.keys(BROWSER_WEB_JOBS)) assert.ok(new RegExp(`needs:[^\\n]*[ \\[]${job}[,\\]]`).test(body), job);
});

test('a ci-ok job that forgot a browser shard is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/(  ci-ok:\n    needs: \[[^\]]*), firefox-web-3/, '$1')));
  assert.ok(problems.some((p) => p.includes('"ci-ok"') && p.includes('"firefox-web-3"')), problems.join('\n'));
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
  `ci.yml: job "${job}" builds for wasm32-unknown-unknown; its second step, right after actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7, must be exactly the pinned clang 23 step (WASM_CLANG_STEP)`;
const ENV_RULE = (job) =>
  `ci.yml: job "${job}" builds for wasm32-unknown-unknown; its job-level env must be exactly:\n${WASM_CLANG_ENV_BLOCK}`;

const LLVM_PACKAGES = ['clang-23', 'llvm-23', 'libllvm23', 'libclang-cpp23', 'libclang1-23', 'libclang-common-23-dev', 'llvm-23-linker-tools', 'llvm-23-runtime'];

test('the canonical step pins every llvm-toolchain-23 package at one version and reads each back', () => {
  assert.ok(WASM_CLANG_STEP.includes('          v=$(apt-cache policy clang-23 | awk \'/Candidate:/ { print $2 }\')'));
  assert.match(WASM_LLVM_VERSION, /^23\.\d+$/);
  assert.ok(WASM_CLANG_STEP.includes(`case "$v" in 1:${WASM_LLVM_VERSION}.*~++*)`), 'the signed apt source candidate must stay on LLVM 23.1');
  const install = WASM_CLANG_STEP.split('\n').find((l) => l.includes('apt-get install'));
  for (const p of LLVM_PACKAGES) {
    assert.ok(install.includes(` "${p}=$v"`), `${p} is not installed at the pinned version`);
  }
  assert.ok(!/\s(clang-23|llvm-23)(\s|$)/.test(install), 'an unpinned package name in the install line');
  assert.ok(WASM_CLANG_STEP.includes(`          for p in ${LLVM_PACKAGES.join(' ')}; do test "$(dpkg-query -W -f='\${Version}' "$p")" = "$v"; done`));
  assert.ok(WASM_CLANG_STEP.includes(`          /usr/bin/clang-23 --version | grep -F "clang version ${WASM_LLVM_VERSION}."`));
  assert.ok(WASM_CLANG_STEP.includes('          apt-cache policy clang-23 llvm-23'));
});

test('the canonical step checks the fingerprint on the keyring apt is pointed at', () => {
  const lines = WASM_CLANG_STEP.split('\n').map((l) => l.trim());
  const dearmor = lines.indexOf('gpg --dearmor -o "$RUNNER_TEMP/apt-llvm.gpg" "$RUNNER_TEMP/llvm-snapshot.asc"');
  const check = lines.indexOf(`test "$primaries" = ${WASM_LLVM_KEY_FPR}`);
  const install = lines.indexOf('sudo install -m 0644 "$RUNNER_TEMP/apt-llvm.gpg" /usr/share/keyrings/apt-llvm.gpg');
  const same = lines.indexOf('cmp "$RUNNER_TEMP/apt-llvm.gpg" /usr/share/keyrings/apt-llvm.gpg');
  const source = lines.findIndex((l) => l.includes('[signed-by=/usr/share/keyrings/apt-llvm.gpg]'));
  assert.ok(dearmor !== -1 && dearmor < check && check < install && install < same && same < source, lines.join('\n'));
  assert.ok(lines[check - 1].startsWith('primaries=$(gpg --show-keys --with-colons "$RUNNER_TEMP/apt-llvm.gpg"'));
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
  test(`a ${job} job installing an unpinned clang is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace(' "clang-23=$v"', ' clang-23'))));
    assert.ok(problems.includes(STEP_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job pinned to another build is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace(WASM_LLVM_VERSION, WASM_LLVM_VERSION.replace('23.1', '23.2')))));
    assert.ok(problems.includes(STEP_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job checking another key fingerprint is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace(WASM_LLVM_KEY_FPR, '0'.repeat(40)))));
    assert.ok(problems.includes(STEP_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job that skips the clang 23 step with an if: is reported`, () => {
    const problems = checkWorkflow(fixture(editJob(job, (b) => b.replace('      - name: clang 23 for the wasm C build\n', '      - name: clang 23 for the wasm C build\n        if: false\n'))));
    assert.ok(problems.includes(STEP_RULE(job)), problems.join('\n'));
  });
  test(`a ${job} job that installs clang 23 after other steps is reported`, () => {
    const moved = editJob(job, (b) => {
      const without = b.replace(`${WASM_CLANG_STEP}\n`, '');
      return `${without.replace(/\n*$/, '')}\n${WASM_CLANG_STEP}\n`;
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
      `${WASM_CLANG_STEP}\n`,
      `      # ${WASM_CLANG_STEP.split('\n').map((l) => l.trim()).join(' ; ')}\n` +
        `      - run: echo '${WASM_CLANG_STEP.split('\n').filter((l) => l.includes('apt-get install')).join('').trim()}'\n`,
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
  const problems = checkWorkflow(fixture(GOOD.replace('  deny:\n', `${extra}  deny:\n`)));
  assert.ok(problems.includes(ENV_RULE('wasm-extra')), problems.join('\n'));
  assert.ok(problems.includes(STEP_RULE('wasm-extra')), problems.join('\n'));
});

test('jobs that build no wasm need no clang 23', () => {
  assert.deepEqual(checkWorkflow(fixture(GOOD)), []);
});

// install-release (2026-10-14): the release workflow, checked against the real ci.yml it must match.
function releaseFixture(release) {
  const dir = mkdtempSync(join(tmpdir(), 'dilla-release-'));
  mkdirSync(join(dir, '.github', 'workflows'), { recursive: true });
  writeFileSync(join(dir, '.github', 'workflows', 'ci.yml'), REAL_CI);
  writeFileSync(join(dir, '.github', 'workflows', 'release.yml'), release);
  return dir;
}

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

test('the release Spotify helper must be built for arm64', () => {
  assertReported(
    releaseProblems((t) => t.replace('runner: ubuntu-24.04-arm', 'runner: ubuntu-latest')),
    'build native amd64 and arm64 assets',
  );
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

test('an action at a version ci.yml does not use is reported', () => {
  assertReported(releaseProblems((t) => t.replace('  rust-wasi:\n    runs-on: ubuntu-latest\n    needs: [tag]\n    steps:\n      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7', '  rust-wasi:\n    runs-on: ubuntu-latest\n    needs: [tag]\n    steps:\n      - uses: actions/checkout@main')), 'actions/checkout@main');
});

test('a tag job that no longer checks the ref is reported', () => {
  assertReported(releaseProblems((t) => t.replace(/ {10}test "\$GITHUB_REF" = .*\n/, '')), 'refs/tags/$tag');
});

test('a release web-build without the clang 23 pin is reported', () => {
  assertReported(releaseProblems((t) => t.replace('      - name: clang 23 for the wasm C build\n', '      - name: clang for the wasm C build\n')), 'clang 23');
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

test('a build cache in the release workflow is reported', () => {
  assertReported(releaseProblems((t) => t.replace('          node-version-file: .nvmrc\n', '          node-version-file: .nvmrc\n          cache: npm\n')), 'build cache');
});

test('a release setup-go without cache: false is reported', () => {
  assertReported(releaseProblems((t) => t.replace('          cache: false\n', '')), 'setup-go caches by default');
});

// v0.1.0-web3.1: the release stamps its tag (without the leading v) into dillad at link time; ci.yml's builds never do.
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

test('a ci.yml build that stamps a version is reported', () => {
  const stamped = GOOD.replace("go build -trimpath -ldflags='-s -w' ./cmd/dillad", "go build -trimpath -ldflags='-s -w -X main.Version=1.2.3' ./cmd/dillad");
  assert.notEqual(stamped, GOOD);
  const problems = checkWorkflow(fixture(stamped));
  assert.ok(problems.some((p) => p.includes('main.Version')), problems.join('\n'));
});

// Task 3 m2: a quoted symbol after -X, and the -X= spelling, stamp a version all the same.
test('a ci.yml build that stamps a quoted version is reported', () => {
  for (const flag of ["-X 'main.Version=1.2.3'", '-X "main.Version=1.2.3"', '-X=main.Version=1.2.3', "-X='main.Version=1.2.3'"]) {
    const stamped = GOOD.replace("go build -trimpath -ldflags='-s -w' ./cmd/dillad", `go build -trimpath -ldflags="-s -w ${flag}" ./cmd/dillad`);
    assert.notEqual(stamped, GOOD);
    const problems = checkWorkflow(fixture(stamped));
    assert.ok(problems.some((p) => p.includes('stamps main.Version')), `${flag}\n${problems.join('\n')}`);
  }
});

test('the CLI checks release.yml too', () => {
  const root = releaseFixture(REAL_RELEASE.replace('release/install.sh release/install-bot.sh release/SHA256SUMS', 'release/install-bot.sh release/SHA256SUMS'));
  const cwd = mkdtempSync(join(tmpdir(), 'dilla-ci-cwd-'));
  const result = runCli(root, cwd);
  assert.equal(result.status, 1, `stdout: ${result.stdout}\nstderr: ${result.stderr}`);
  assert.match(result.stderr, /release\.yml/);
});

test('a job that lost its change gate is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace("    if: needs.changes.outputs.go != 'false'\n    runs-on: ubuntu-latest\n    timeout-minutes: 55\n", '    runs-on: ubuntu-latest\n    timeout-minutes: 55\n')));
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

test('the real ci-ok job needs every other job', () => {
  const body = REAL_CI.slice(REAL_CI.indexOf('\n  ci-ok:'));
  const needs = /needs: \[([^\]]*)\]/.exec(body)[1].split(',').map((s) => s.trim());
  const jobs = [...REAL_CI.slice(REAL_CI.indexOf('\njobs:')).matchAll(/^ {2}([A-Za-z0-9_-]+):\s*$/gm)].map((m) => m[1]).filter((job) => job !== 'ci-ok');
  assert.deepEqual(needs.sort(), jobs.sort());
});

test('a ci-ok job that forgot a job is reported', () => {
  const problems = checkWorkflow(fixture(GOOD.replace(/(  ci-ok:\n    needs: \[)changes, /, '$1')));
  assert.ok(problems.some((p) => p.includes('"ci-ok"') && p.includes('"changes"')), problems.join('\n'));
});

test('a ci-ok job without the failure check, or without always(), is reported', () => {
  let problems = checkWorkflow(fixture(GOOD.replace("contains(needs.*.result, 'cancelled')", 'false')));
  assert.ok(problems.some((p) => p.includes('"ci-ok"') && p.includes('cancelled')), problems.join('\n'));
  problems = checkWorkflow(fixture(GOOD.replace('    if: always()\n    runs-on: ubuntu-latest\n    steps:\n      - if: contains', '    runs-on: ubuntu-latest\n    steps:\n      - if: contains')));
  assert.ok(problems.some((p) => p.includes('"ci-ok"') && p.includes('always()')), problems.join('\n'));
});

test('if: always() stays forbidden in every job but ci-ok, and in ci-ok steps', () => {
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
