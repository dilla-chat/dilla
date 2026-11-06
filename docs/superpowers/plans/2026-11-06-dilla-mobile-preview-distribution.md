# Mobile preview distribution (TestFlight + release-signed APK) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Friends install the dilla preview app: iPhone through a TestFlight public link, Android through a release-signed APK attached to the GitHub release, both cut from every `v*` server release tag.

**Architecture:** A new workflow `mobile-release.yml` runs after `release.yml` completes successfully for a tag (`workflow_run`), or by hand for an existing tag (`workflow_dispatch`, which re-runs an existing tag (the backfill of `v0.1.0-mobile1` was dropped: it predates this branch's files)). One job signs the Android APK with the release keystore from the `mobile-release` GitHub environment, checks the signing certificate against a pin in `packages/mobile/release-trust.json`, and uploads `dilla-<tag>.apk` and its `.sha256` to the existing release. Another job, on the pinned macOS/Xcode runner, archives the iOS app with automatic signing through an App Store Connect API key and uploads it to TestFlight. The server release is never blocked by a mobile failure.

**Tech Stack:** GitHub Actions, Gradle signingConfig, `apksigner`, `xcodebuild archive/-exportArchive` with `-allowProvisioningUpdates` and the App Store Connect API key, Node check scripts.

**Spec:** `docs/superpowers/specs/2026-11-05-dilla-mobile-design.md` (Wave M4, Decisions: audience, lockstep releases). This plan pulls M4's distribution forward on Jonas's 2026-11-06 choice ("TestFlight link", "Release-signed APK"). The in-app APK updater (spec line 23) stays in M4.

## Global Constraints

- App id `chat.dilla.app`; iOS 17+, Xcode pinned as in `mobile.yml` (`macos-26`, Xcode 26.6); Android minSdk 29.
- Releases are lockstep: every `v*` server tag also cuts the mobile builds from the same commit. No minimum-version gate.
- Secrets live only in the GitHub environment `mobile-release` (required reviewer: none; branch/tag rule: tags `v*`). Never in the repo, logs, or artifacts. The release keystore and its password also go to Jonas's password manager (he holds the only offline copy).
- Release builds never trust user CAs and allow no cleartext (`npm run android:check-release` must pass on the signed APK).
- Version: marketing version = the tag without the `v` prefix's suffix (`0.1.0`); iOS build number and Android `versionCode` = `github.run_number` of `mobile-release.yml` plus 1000 (monotonic across reruns and backfills).
- Workflow pinning follows the repo: actions by full SHA with a version comment; `scripts/check-ci.mjs` (`npm run check:ci`) must pass.

## Review Focus

1. A rerun or backfill for a tag whose release already has the APK: upload must replace (`--clobber`) the APK and checksum together, never leave a mismatched pair.
2. A missing or wrong secret (empty keystore, wrong password, expired API key): the job fails with a message naming the secret, and prints nothing secret.
3. The pin check must be able to fail: a debug-signed APK, or one signed with another key, is refused before upload (test with the debug APK).
4. `workflow_run` from a failed or non-tag `release.yml` run must not build anything.
5. TestFlight rejects a reused build number: two runs for the same tag still get distinct build numbers.

## Jonas's one-time setup (blocks the first real run, not the implementation)

Written into `packages/mobile/README.md` by Task 3; listed here so the controller can send it on day one.

1. App Store Connect → Apps → **+ New App**: iOS, name (Dilla, or Dilla Chat if taken), bundle id `chat.dilla.app`, SKU `dilla-ios`.
2. Users and Access → Integrations → **App Store Connect API** → Team key, role **App Manager**. Put Key ID, Issuer ID and the `.p8` into environment `mobile-release` as `ASC_KEY_ID`, `ASC_ISSUER_ID`, `ASC_KEY_P8` (`gh secret set --env mobile-release ...`).
3. TestFlight: beta app description, feedback email, an external group with a **public link**; answer export compliance on the first build (see Task 2's `ITSAppUsesNonExemptEncryption`), submit it for beta review.
4. Android keystore: the controller generates it (Task 1) and sets `ANDROID_KEYSTORE_B64`, `ANDROID_KEYSTORE_PASSWORD`, `ANDROID_KEY_ALIAS`, `ANDROID_KEY_PASSWORD` in `mobile-release`; Jonas stores the keystore file and password in his password manager, then the controller deletes its local copy.

---

### Task 1: Android release signing and the certificate pin

**Files:**
- Modify: `packages/mobile/android/app/build.gradle` (release `signingConfig` from env)
- Create: `packages/mobile/release-trust.json`
- Create: `packages/mobile/scripts/check-apk-signer.mjs` + `packages/mobile/scripts/check-apk-signer.test.mjs`
- Modify: `packages/mobile/package.json` (scripts), `scripts/check-apk-network-config.mjs` call site if the signed APK path differs

**Interfaces:**
- Produces: env contract `DILLA_ANDROID_KEYSTORE` (path to a `.jks`), `DILLA_ANDROID_KEYSTORE_PASSWORD`, `DILLA_ANDROID_KEY_ALIAS`, `DILLA_ANDROID_KEY_PASSWORD`. Without them `assembleRelease` still builds `app-release-unsigned.apk` exactly as today (CI's `mobile-android` job is unchanged).
- Produces: `release-trust.json` `{ "android": { "certSha256": ["<64 lowercase hex>"] } }` (a list, so a key rotation can stage a second entry).
- Produces: `node packages/mobile/scripts/check-apk-signer.mjs <apk>`: exit 0 only when `apksigner verify --print-certs` reports a v2+ signature whose certificate SHA-256 is in the pin list; otherwise exit 1 naming the found digest.

- [ ] Test: `check-apk-signer.test.mjs` parses recorded `apksigner --print-certs` output (fixtures) — pinned digest → ok; other digest → refused with that digest in the message; no v2/v3 signature → refused; two signers → refused.
- [ ] Implement the parser and CLI; `apksigner` path from `$ANDROID_HOME/build-tools/<latest>/apksigner`.
- [ ] Gradle: a `release` signingConfig created only when all four env vars are set.
- [ ] Controller step (not the implementer): generate the keystore (`keytool -genkeypair -keystore dilla-release.jks -alias dilla -keyalg EC -groupname secp256r1 -validity 10000 -dname "CN=Dilla"`) in the scratchpad, set the four secrets, write its certificate SHA-256 into `release-trust.json`.
- [ ] Verify locally: signed build passes `check-apk-signer`; the debug APK is refused; `android:check-release` passes on the signed APK.
- [ ] Commit.

### Task 2: `mobile-release.yml`

**Files:**
- Create: `.github/workflows/mobile-release.yml`
- Modify: `packages/mobile/ios/App/App/Info.plist` (`ITSAppUsesNonExemptEncryption` = `YES`: Jonas 2026-11-06 "Standard algorithms" — MLS/E2EE from standard published algorithms; App Store Connect's first-build questionnaire is answered "standard encryption algorithms", not available in France is not claimed; if ASC still asks per build, Jonas answers there once and the controller records the resulting `ITSEncryptionExportComplianceCode` if one is issued), `packages/mobile/ios/App/ExportOptions.plist` (create: method `app-store-connect`, `signingStyle` automatic, `destination` upload, team id)
- Modify: `scripts/ci-areas.mjs` / `scripts/check-ci.mjs` only if the checker requires registering a new workflow

**Interfaces:**
- Consumes: Task 1's env contract and checker; `release.yml`'s published release for the tag.
- Triggers: `workflow_run` (workflows: [release], types: [completed]) guarded by `conclusion == 'success'` and `head_branch` matching `v*` — with `workflow_run`, `head_branch` is the tag name; `workflow_dispatch` with input `tag` (must exist and have a release).
- Jobs: `resolve` (tag, version, build number), `android-release` (ubuntu, environment `mobile-release`: build web bundle as `mobile.yml` does, `cap sync android`, `assembleRelease` signed, `check-apk-signer`, `android:check-release`, `sha256sum`, `gh release upload <tag> dilla-<tag>.apk dilla-<tag>.apk.sha256 --clobber`), `ios-testflight` (macos-26, Xcode 26.6, environment `mobile-release`: write `.p8` to `$RUNNER_TEMP` with mode 600, `xcodebuild archive` with `-allowProvisioningUpdates -authenticationKeyPath/-authenticationKeyID/-authenticationKeyIssuerID`, `CURRENT_PROJECT_VERSION`=build number, `MARKETING_VERSION`=version, then `-exportArchive` with `ExportOptions.plist` (destination upload)). The two mobile jobs are independent.

- [ ] Write the workflow; `npm run check:ci` passes.
- [ ] Dry run: `workflow_dispatch` for a tag cut after this branch merged once Task 1's secrets exist → APK + sha256 on the release; iOS job either uploads or (before Jonas's ASC setup) fails naming the missing secret.
- [ ] Commit.

### Task 3: Docs

**Files:**
- Modify: `docs/user/mobile.md` (install: TestFlight public link; APK download, check the `.sha256`, allow the install source; updates: TestFlight auto-updates, APK installs over the old one; preview warnings unchanged), `README.md` mobile row, `packages/mobile/README.md` (release section: environment, secrets, Jonas's one-time setup, key rotation via a staged pin entry, backfill by dispatch).

- [ ] Write; links and anchors resolve (`npm run check:docs` if present).
- [ ] Commit.
