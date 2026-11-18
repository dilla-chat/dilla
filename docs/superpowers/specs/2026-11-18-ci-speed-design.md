# Faster CI: test tiers, a Rust build cache, one run per change

Status: draft for Jonas, 2026-11-18.

## Why

Jonas: "But the builds take forever?" These are the main runs before 2026-11-18:

| Workflow | Wall clock |
|---|---|
| desktop-gpui | 78–134 min |
| server | 27–43 min |
| desktop-ci | 26–31 min |
| mobile | 17–32 min |

desktop-gpui gates every desktop PR and every desktop release, and its slowest job sets the pace.

Inside run 44253915661:

- **gpui-mac-arm64 (83 min)**, steps run one after another:
  - parity screens: 19 min
  - Mac call tests: 17 min
  - `cargo build`: 13 min
  - `cargo test`: 12 min
  - test host and WASI core: 4 min
  - disk cleanup: 4 min
  - cache save: 3 min
  - clippy: 3 min
- **gpui-linux-x64-bot (54 min):** the ignored Bot tests take 42 min.
- **gpui-linux-x64-client (49 min):** "Install the GPUI build dependencies" took **27 min**, which is an anomaly (apt).
- **gpui-win-x64 (47 min):** build 15 min, cache save **10 min**, test 8 min.

Queueing for scarce macOS runners comes on top of all of this.

Every merge also runs the same CI again on main, and the release gate (`scripts/server-release-gate.mjs:57-92`) waits for that push run on the exact commit. A desktop release therefore waits another ~75–130 min after its merge (alpha.1 did this three times on 2026-11-17/18).

## Goals

- A desktop PR's checks finish in about 35–45 min instead of 80–130.
- A release starts minutes after its merge, not after a second full CI run.
- Every check that runs today still runs before a release. Nothing is deleted, only moved between tiers.
- The tiers are written down, so "how do the tests work" has one answer.

## Non-goals

- Splitting the repository. Folded into the native-mobile project later, with a pinned, released `dilla-testhost` for client tests.
- Self-hosted runners (not chosen).
- Making the tests themselves faster (the Bot timing budgets, the parity renderer), beyond the two anomalies below.

## Anti-goals

- A Mac-only regression reaching a published release. The release tier always runs the full Mac set on the tagged tree.
- Nightly failures that nobody sees. A red nightly must be loud and must block releases until it is green again.
- Speed bought by weakening a check. Moving a check to another tier is fine; loosening its assertion is not.

## Approaches considered

1. **Tiered tests + sccache + same-tree gate (chosen by Jonas, 2026-11-18):** move the heavy macOS legs out of the PR tier, warm-cache Rust compiles on every OS, and let a release reuse the PR's green run when the tree is identical.
2. **Repo split (Signal-style):** server and each client in their own repo, with clients testing against a pinned server. It saves CI only on server-only changes; a desktop PR stays about 80 min. Deferred to the native-mobile project.
3. **Self-hosted runners (homelab Linux):** a warm `target/` and no queue, but a trusted-branches-only security model and a box to maintain. Not chosen.

## Design

### 1. Three test tiers

| Tier | When | desktop-gpui runs |
|---|---|---|
| **PR** | every pull request, and main pushes whose tree no green PR run covers | all Linux legs; Windows build + unit + clippy; macOS build + `cargo test` + clippy + `gpui-mac-package` |
| **Nightly** | 03:00 UTC on main, plus `workflow_dispatch` | the PR tier + the macOS parity screens + the macOS call tests + the Linux Bot ignored tests + the macOS VideoToolbox decode |
| **Release** | before any `desktop-gpui-v*` tag is released | the nightly tier on the exact tagged tree |

- One workflow file with a `tier` input/env. Jobs and steps carry `if:` conditions on the tier, so a check's commands are written once.
- **Release-tier gate:** the release workflow's gate requires a green nightly-tier run of desktop-gpui whose tree equals the tag's tree. If there is none, it starts one with `workflow_dispatch` (tier=release) on the tag and waits for it. That costs one Mac-heavy run, the same as today, but only per release instead of per PR plus per merge.
- **A red nightly is loud:**
  - The workflow opens one GitHub issue, `nightly desktop-gpui red`, or comments on it if it is already open. It is labelled `ci-nightly` and links the run.
  - The next green nightly closes it.
  - The release gate refuses while that issue is open.
  - Jonas's fail-fast rule applies: whoever is on duty fixes it the same day.
- `docs/dev/testing.md` (new) holds the tier table and what each tier proves. `clients/desktop/RELEASING.md` points to it.

### 2. Rust compile cache (sccache)

- Install `sccache` with `mozilla-actions/sccache-action` (pinned by SHA, like every action here), with `SCCACHE_GHA_ENABLED=true` and `RUSTC_WRAPPER=sccache`, on every Rust job in desktop-gpui, server, desktop-ci and mobile.
- Keep the existing `actions/cache` for `~/.cargo/registry` and `~/.cargo/git`. Drop the `target/` caches that sccache replaces. The Windows `target/` cache save takes 10 min.
- Release builds (`desktop-gpui-release.yml`, `desktop-gpui-sign.yml`) stay **uncached and `--locked`** by design ("A clean build: no cache restored", sign.yml:127). sccache is not used there.
- Success measure: `sccache --show-stats` is printed at the end of each job. The warm Mac `cargo build` should take ≤ 5 min.
- **Cache budget (my call):** measure first. The repo's 10 GB GitHub cache is shared. After a week, report each workflow's cache sizes and hit rates before adding any eviction rule.
- **Warm-up:** the nightly runs on main, so it keeps main's cache scope warm even when main pushes skip themselves. PR branches read main's scope.

### 3. One run per change

- The PR tier already runs on `refs/pull/N/merge`. A squash merge produces a commit whose tree equals that merge ref's tree when the PR was up to date with main at merge time.
- **Two different rules use the tree** (Tree SHAs come from `GET /repos/{r}/git/commits/{sha}`; run head SHAs map to trees the same way):
  - **Release gate (desktop):** it needs a green run of tier `nightly` or `release` on the tagged commit's tree. A PR-tier run never satisfies it, because it lacks the Mac parity and call tests (anti-goal 1).
  - **Server release gate:** the server workflow has no tiers, so any green server run on the tree satisfies it.
  - **Main-push skip:** a green PR-tier run on the same tree lets a main push skip its own run. It never counts for a release.
- **Tier marker (my call):** every desktop-gpui run sets `run-name: desktop-gpui [${{ tier }}]`, so the gate reads the tier from `display_title`.
  - A `skipped-same-tree` push run is titled `[skipped]`. The gate never counts it, even though it concludes success.
  - The gate takes the newest green matching run.
- **Release dispatch (my call):**
  - When no qualifying run exists for the tag's tree (for example a merge whose paths skipped desktop-gpui), the release workflow dispatches desktop-gpui with `tier=release` on the tag, using `GITHUB_TOKEN` with `actions: write`, and polls for it.
  - The gate accepts `workflow_dispatch` runs whose head SHA is the tag's commit, whatever the head branch.
  - `BOUND_MS` rises to 150 min for this wait.
- **Main pushes:** a lightweight first job looks for a green PR-tier run on main's new tree. If it finds one, the push run titles itself `[skipped]` and does nothing else. Otherwise it runs the PR tier as today. This covers main having moved under the PR.
- **Branch protection:** the `main` ruleset requires only `server-ok`, `desktop-ok` and `mobile-ok`, and does not require branches to be up to date (`strict_required_status_checks_policy: false`). The design stays correct either way. Without "up to date", more main pushes fall through to a full PR-tier run, which is slower but correct. Turning it on is Jonas's choice.
- **`check-ci-workflow.mjs`:** its pins move with the steps. Rewriting an assertion so it names a step's new tier counts as moving the check, not loosening it.

### 4. The two anomalies

- **Linux "Install the GPUI build dependencies": 27 min on one leg, about 1 min on the others.** Root-cause it before fixing: an apt mirror, a `apt-get update` retry loop, or a clang 21 download. Then pin and cache. Expected: ≤ 2 min on every leg.
- **Windows `actions/cache/save`: 10 min.** This is the `target/` cache. It goes away with §2.

## Expected result

- **Desktop PR:**
  - Mac job ≈ 4 (cleanup) + 4 (warm build) + 12 (test) + 3 (clippy) + 4 (test host) ≈ 27–30 min.
  - The Linux legs set the critical path: about 25 min once the client anomaly is fixed, with the Linux Bot ignored tests moved to nightly (Jonas).
  - Target: 35–45 min.
- **Release:** it starts right after the merge. Its gate waits for a nightly-tier run of that tree, so a release costs about 80 min of Mac CI once, instead of PR + main + release.

## Constraints

- The CI checks in `scripts/check-ci-workflow.mjs` (and its test) and `scripts/ci-areas.mjs` change with every workflow edit. `npm run test:ci-check` must pass.
- Actions are pinned by SHA. No secrets are used by the PR or nightly tiers.
- Jonas's rules apply:
  - CI is fail-fast;
  - fixes are batched into one PR;
  - `npm run ci:local` passes before pushing;
  - merges need his go-ahead.

## Left to the builder

- The exact `if:` layout, the tier env name, and how the nightly opens and closes its issue (`gh issue` in a final job with `issues: write`).
- Which steps in server, desktop-ci and mobile get sccache. All Rust compiles do.

## Decided by Jonas (2026-11-18)

1. **Linux Bot ignored tests (42 min): nightly and release tiers**, like the Mac call tests. A PR runs about 35–45 min.
2. **"Decode with VideoToolbox (macOS)" gets fixed first.**
   - Today the step is gated on `github.event_name == 'schedule'` and has never run. Its comment says it crashed with SIGSEGV on macOS CI (`core/vendor/README.md`).
   - The VideoToolbox SIGSEGV is root-caused and fixed as part of this work, together with the MacBook's macOS 27 H.264 findings (the `h264_here()` probe disagrees with the real decoder). The nightly must then gate fully from its first night.
   - The tier switch-over lands only once that step passes.
