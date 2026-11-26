# Remove the Capacitor mobile app: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan.

**Goal:** the Capacitor iOS/Android app (`packages/mobile`) and everything that exists only for it is gone. Server
releases publish without the APK step. Native mobile apps come later as a separate project.

**Spec:** Jonas, 2026-11-26:
- "Kan we kill the mobile apps? We will need to rebuild them later as native apps".
- Scope: "Everything, one PR". The app, both mobile workflows, the CI entries, the mobile code in client-core/web, the
  user docs and the README all go. Old plans, specs and `docs/deploy/releases/*` stay as history.
- `http.mobile_origins`: "Remove completely". He re-confirmed this after being told that strict config means every
  `dilla.toml` written since mobile1 refuses to start (exit 78) until the line is deleted.

## Global Constraints
- **Keep:**
  - the web client's phone layout (Shell phone, layout.ts): the browser client still has it;
  - device kinds like "phone" in the desktop client (`dilla-client/src/features/devices.rs`, `strings.rs`);
  - `internal/desktopauth/browser.js` and its source `packages/web/src/desktop-auth-browser.ts`.
  Only code that exists for the Capacitor shell goes: native-app checks, the `mobile-build` flag, the native HTTP
  paths, and the Switch-instance UI that only the app shows. When unsure whether a branch serves the browser too,
  keep it and list it in the report.
- **`http.mobile_origins` is removed from the config struct, the defaults, validation, CORS and the gateway's
  TrustedOrigins.** Strict decoding stays as it is. A test pins that a `dilla.toml` with `mobile_origins` under
  `[http]` now fails to load with an unknown-key error.
- **No CORS without the setting.** `mobileCORS` and its preflight/rate-limit tests go. If `internal/api/cors.go`
  serves only mobile origins, it goes too. Otherwise only the mobile part goes, and the report says which.
- **release.yml publishes the release itself:**
  - It drops `--draft` and the header text about mobile-release.yml and the APK.
  - The release is created only after every asset and SHA256SUMS are ready. Releases are immutable, so nothing can be
    added after publish.
  - It still reads no secret and has no environment.
- **CI:**
  - `mobile` disappears from `scripts/ci-areas.mjs` (AREAS, patterns, graph, workflows, jobs).
  - `mobile-ok` disappears from `ci-rerun-starved.mjs`, `check-ci-workflow.mjs`, `ci-local-plan.mjs`, the watchdog
    and every test that lists it.
  - Delete `scripts/ci-android.sh`.
  - Remove the patch-package postinstall and the dependency only if `patches/` is empty after deleting the Capacitor
    patch.
- **Delete these files:** `.github/workflows/mobile.yml`, `.github/workflows/mobile-release.yml`, `packages/mobile/`,
  `patches/@capacitor+android+8.6.0.patch`, `docs/user/mobile.md`.
- **`package-lock.json` regenerated with `npm install`.** No unrelated version bumps: compare the diff.
- **Done when:** `git grep -nIi -E 'capacitor|packages/mobile|mobile-ok|mobile_origins|MobileOrigins|mobile-release|testflight|\.apk\b'`
  hits only `docs/superpowers/`, `docs/deploy/releases/`, `docs/spikes/`, and the new release-note text and test from
  this plan.
- **Checks before each commit:**
  - Go tasks: `go build ./... && go vet ./...`, plus the touched packages' tests.
  - Script tasks: `npm run test:ci-check`.
  - TS tasks: `npm run typecheck` and the touched workspaces' tests.
  - Every task: `npm run check:docs` if docs changed.
  - Builds use `CARGO_TARGET_DIR=$PWD/target CC=clang CXX=clang++ CARGO_BUILD_JOBS=4`. Delete
    `target/debug/incremental` afterwards and never run `cargo clean`.

### Task 1: CI and release flow (review: opus, release gate)
- [ ] Delete both mobile workflows and `scripts/ci-android.sh`. Remove mobile from ci-areas, ci-local,
  ci-local-plan, check-ci-workflow, ci-rerun-starved, the watchdog, retry-on and ci-same-tree, together with their
  tests.
- [ ] release.yml publishes directly, as specified above. Check `scripts/server-release-gate.mjs` and its test for
  mobile assumptions.
- [ ] `npm run test:ci-check` is green. Commit: `ci: drop the mobile lane; server releases publish without the APK step`.

### Task 2: client code (review: sonnet)
- [ ] Delete `packages/mobile` and the Capacitor patch. Update the root package.json (workspaces, postinstall if
  `patches/` is now empty) and regenerate the lockfile.
- [ ] Remove the app-only code:
  - client-core `worker/mobile-build.ts` and its uses in instance-url/storage-key, with their tests;
  - web `mobile.ts`/`mobile.test.ts` and their uses (main.tsx, Boot, You, Shell, SignIn, Onboarding, notify,
    webauthn, call/session as found);
  - app-only strings in `strings/en.ts`;
  - vite.config and check-entry-chunk mobile entries.
- [ ] `npm run typecheck` and the web/client-core tests are green. Commit: `web: drop the Capacitor app's code paths`.

### Task 3: server (review: opus, authz/CORS)
- [ ] Remove `MobileOrigins` (config.go, defaults.go, validate.go), `mobileCORS` and the TrustedOrigins concat in
  server.go, plus their tests (`dilladtest/mobile_origin_test.go`, `mobilecors_ratelimit_test.go`, and the mobile
  cases in the config and cors tests).
- [ ] Add a test: a config with `[http] mobile_origins = ["capacitor://localhost"]` fails Load with an unknown-key
  error.
- [ ] `protocol/02-delivery-service.md`: drop the mobile-origin text. Commit: `server: remove http.mobile_origins`.

### Task 4: docs (review: sonnet)
- [ ] Delete `docs/user/mobile.md` and every link to it.
- [ ] README: remove the mobile row and lines, and say that native phone apps are planned.
- [ ] Update CONTRIBUTING, `docs/deploy/README.md` (`http.mobile_origins` section), `docs/dev/testing.md` and
  `docs/dev/ci-local.md`.
- [ ] Write the next server release's note in `docs/deploy/releases/unreleased.md` (merged there after the final review). It must contain,
  in bold: **before upgrading, delete the `mobile_origins` line under `[http]` in dilla.toml, or dillad refuses to
  start (exit 78)**. It also says the release no longer carries an APK.
- [ ] `npm run check:docs` is green. Commit: `docs: the Capacitor mobile app is gone; native apps come later`.

## Outside the repo (Jonas, at merge time)
- The ruleset 25709839 requires `mobile-ok`. That check must be removed from the required checks before the PR can
  merge, because nothing reports it any more.
- Delete the `mobile-release` environment and its secrets.
- The Homelab session deletes `mobile_origins` from CT 144's dilla.toml before the next server upgrade.
