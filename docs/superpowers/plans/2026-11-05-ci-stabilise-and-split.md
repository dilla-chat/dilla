# CI Stabilisation and Desktop/Server Split Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A red build means a real problem: one CI round shows every failing test, GitHub hiccups no longer cancel healthy runs, and a desktop problem can never block a server release (or the reverse).

**Architecture:** Make the watchdog conservative (fail open, ignore runner-less jobs, never cancel on e2e shard failures), let desktop e2e shards run to the end, add bounded retries for known outside failures, and re-run jobs GitHub never gave a runner. Then split `ci.yml` into two always-triggered workflows, `server` and `desktop`. Each has its own `changes`-gated jobs, its own watchdog and its own aggregate check (`server-ok`, `desktop-ok`). Each release gate reads its own workflow. A nightly schedule runs the full desktop lane on main.

**Tech Stack:** GitHub Actions YAML, Node (`scripts/*.mjs` with `node --test`), Python 3 (`scripts/ci-watchdog.py` with unittest), Go (`cmd/dillad`), Playwright.

**Spec:** No separate spec. Jonas's decisions of 2026-11-04/05 (session `session_012Nny5K2B969mk49QX3XSXG`, memory `dilla-ci-overhaul-2026-11-04`) are the authority:
- ship signal2 first (done), then stabilise;
- all failures per round;
- auto-retry infra;
- area-scoped jobs via the split;
- a nightly e2e run, desktop lane only (the browser client is deprecated);
- "split CI, one repo" into desktop / server (api) / landing page. The landing page pipeline comes when the landing page exists, so it is not in this plan;
- the watchdog keep/narrow/remove decision comes after the split. Now it only fails open on its own API errors.

Also from those decisions: the installer prunes `dilla.db.pre-migration-*`, keeping the newest 3.

## Global Constraints

- One repository. The release flow stays fully automated with no manual clicks (decision of 2026-11-02).
- Every workflow that owns a required check triggers on every `pull_request` and every `push` to `main`, with no `paths:` filter at the trigger. Area skipping happens inside, through a `changes` job.
- Retries never use `continue-on-error`, `|| true` or `if: always()`; `check-ci-workflow.mjs` forbids them. A retry repeats only a failure whose output matches a named pattern, at most 3 attempts.
- Every `upload-artifact` keeps `if-no-files-found: error`. Evidence uploads become conditional on their directory existing instead.
- Action and tool versions stay SHA-pinned, as now.
- `npm run ci:local` keeps mirroring CI. Update it in the same task that changes a workflow's shape.
- Commits end with the session's Co-Authored-By and Claude-Session trailers. Never push to `main`; one branch and one PR (`feat/ci-stabilise-split`).
- The repository ruleset (id 25709839) is changed only with Jonas's explicit go (Task 9).

## Review Focus

1. **A required check that never reports.** A PR touching only docs, or only server code, must still get both `server-ok` and `desktop-ok` reported as success, or neither may be required. Test this with a docs-only fixture in `ci-areas` and in the checker.
2. **A release gate that finds the wrong run.** After the split, the desktop gate must accept only a green `desktop` push run on the exact tag SHA, and the server tag gate only a green `server` push run. A green run of the other workflow must not satisfy it. Test this in `ci-run.test.mjs` and the server-gate test.
3. **Fail-open hiding a real failure.** When the watchdog stops watching, the aggregate `*-ok` job must still go red for any failed job. Test a failing job together with a crashed watchdog.
4. **An endless re-run loop.** The auto re-run must act only on attempt 1, and only on jobs that never got a runner (no `runner_name`, zero steps, `cancelled`). Test attempt 2, and a cancelled job that has steps.
5. **A retry masking a real failure.** A Go test failure, a notarization rejection, or a 404 for a missing module must not be retried. Test non-matching output with each retry wrapper.

---

### Task 1: Watchdog fails open, ignores runner-less jobs, leaves e2e shards alone

**Files:**
- Modify: `scripts/ci-watchdog.py` (`failure_state` ~27-33, `watch` ~158-176, `main` ~185-207)
- Test: `scripts/ci-watchdog.test.py`
- Modify: `scripts/check-ci-workflow.mjs` (watchdog pin ~918-936, if it pins behaviour text)

**Interfaces:**
- Produces: `NEVER_STARTED(job) -> bool` (`status=="completed"`, `conclusion=="cancelled"`, empty `runner_name`, no steps) and `E2E_SHARD = re.compile(r"^desktop / desktop-(client|voice)-")`.
- `main()` returns 0 and prints `::warning::CI watchdog stopped watching: <detail>` on its own errors. It never calls `cancel()` on that path.

- [ ] **Step 1: Write the failing tests** in `ci-watchdog.test.py`:
  - `test_a_job_that_never_got_a_runner_is_not_a_failure`: a job record with `conclusion="cancelled"`, `runner_name=""` and `steps=[]` beside an in-progress job. `watch` must not cancel.
  - `test_an_e2e_shard_failure_does_not_cancel_the_run`: `desktop / desktop-client-linux-x64-3` has `conclusion="failure"`. No `POST /cancel`, and the watcher keeps watching until completion.
  - `test_a_build_failure_still_cancels`: `go-api-1` has a failure → `cancelled`.
  - `test_own_api_failure_fails_open`: replace `test_repeated_read_outage_stays_red_and_attempts_existing_fallback_cancellation`. Exit code 0, a `::warning::` on stderr containing "stopped watching", no POST request, no secret in output.
- [ ] **Step 2:** Run `python3 scripts/ci-watchdog.test.py` and expect these four to FAIL.
- [ ] **Step 3:** Implement. `failure_state` skips jobs that are `NEVER_STARTED` or match `E2E_SHARD`. `main()` prints the warning and returns 0 instead of cancelling. Keep the token redaction.
- [ ] **Step 4:** Run `python3 scripts/ci-watchdog.test.py` and `npm run test:ci-check`; both PASS.
- [ ] **Step 5:** Commit: `fix(ci): the watchdog fails open, ignores jobs never given a runner, and leaves e2e shards running`.

### Task 2: Desktop e2e shards run to the end

**Files:**
- Modify: `.github/workflows/desktop.yml` (lines 449, 456, 569, 572: drop `--max-failures=1`)
- Modify: `scripts/check-ci-workflow.mjs` (1285, 1473, 1507: require that no `--max-failures` is present instead of `=1`)
- Test: `scripts/check-ci-workflow.test.mjs`

- [ ] **Step 1: Failing tests:**
  - `a desktop client or voice step with --max-failures is reported`: the fixture adds `--max-failures=1` and expects a problem naming `--max-failures`.
  - `the real desktop workflow passes without --max-failures`.
- [ ] **Step 2:** Run `node --test scripts/check-ci-workflow.test.mjs` and expect FAIL.
- [ ] **Step 3:** Edit `desktop.yml` and the checker rules.
- [ ] **Step 4:** Run `npm run test:ci-check` and `npm run check:ci`; both PASS.
- [ ] **Step 5:** Commit: `ci(desktop): e2e shards run every test, so one round shows every failure`.

### Task 3: Known flake and misleading evidence-upload errors

**Files:**
- Modify: `e2e/unit/voice-join-fixture.test.mjs:59-65` (`timeout: 100` → `timeout: 5_000`)
- Modify: `e2e/ux/voice-fixture.mjs:10` (the phase read gets `timeout: Math.max(1_000, deadline - Date.now())`)
- Modify: every `upload-artifact` step with `if: failure()` in `.github/workflows/ci.yml` and `desktop.yml`. Each condition gains `&& hashFiles('<its path>/**') != ''`, so the upload runs only when there is evidence.
- Modify: `scripts/check-ci-workflow.mjs` (exact upload pins 1396-1409 and `assertEveryUploadFailsOnNoFiles` ~515) to accept that condition and require it on `if: failure()` uploads.
- Test: `scripts/check-ci-workflow.test.mjs`

- [ ] **Step 1: Failing test:** `a failure-evidence upload without a hashFiles guard is reported`, and the real workflows pass.
- [ ] **Step 2:** Run it and expect FAIL.
- [ ] **Step 3:** Make the edits. For the fixture, run `npm run test:voice-fixture -w @dilla/e2e` 20 times under `taskset -c 0` with a CPU hog. Expected: 20/20 PASS.
- [ ] **Step 4:** Run `npm run test:ci-check` and `npm run check:ci`; both PASS.
- [ ] **Step 5:** Commit: `test(ci): the voice fixture no longer races a 1 ms budget; evidence uploads run only when evidence exists`.

### Task 4: Bounded retries for known outside failures

**Files:**
- Create: `scripts/retry-on.mjs`, CLI `node scripts/retry-on.mjs --pattern <regex> [--pattern …] [--attempts 3] [--wait 20] -- <cmd> <args…>`. It runs the command and streams its output. It repeats only when the exit code is non-zero **and** the combined output matches a pattern. It exits with the last exit code.
- Test: `scripts/retry-on.test.mjs` (add it to `test:ci-check` in `package.json`)
- Modify: `.github/workflows/ci.yml` (later the `server`/`desktop` workflows). Each Go job gets a step `node scripts/retry-on.mjs --pattern 'proxy.golang.org|stream error|INTERNAL_ERROR|TLS handshake timeout|connection reset' -- go mod download` before its first build or test.
- Modify: `.github/workflows/desktop.yml:336`. The mac `npm run dist` step goes through `retry-on.mjs` with `--pattern 'staple and validate action failed|NSURLErrorDomain|Error 68'`.
- Modify: `scripts/check-ci-workflow.mjs` `REQUIRED_STEPS` for these steps.

**Interfaces:**
- Produces: `retryOn({ patterns: RegExp[], attempts: number, waitMs: number, run: () => Promise<{code:number, output:string}>, sleep }) -> Promise<{code:number, attempts:number}>`, exported for tests. The CLI wraps it.

- [ ] **Step 1: Failing tests** in `retry-on.test.mjs`:
  - `a matching failure is retried and its success returned` (attempts 2, code 0);
  - `a non-matching failure is not retried` (attempts 1, code passed through);
  - `a matching failure stops after the attempt limit` (attempts 3, last code);
  - `success on the first try runs once`.
- [ ] **Step 2:** Run `node --test scripts/retry-on.test.mjs` and expect FAIL (module missing).
- [ ] **Step 3:** Implement and wire up the steps.
- [ ] **Step 4:** Run `npm run test:ci-check` and `npm run check:ci`; both PASS. Run `ci:local` for one Go job.
- [ ] **Step 5:** Commit: `ci: retry Go module downloads and macOS stapling only on their known network errors`.

### Task 5: Re-run jobs GitHub never gave a runner

**Files:**
- Create: `scripts/ci-rerun-starved.mjs`. Given a run ID, it reads the run's jobs for attempt 1. If at least one job is `NEVER_STARTED` (as in Task 1), it calls `POST /actions/runs/{id}/rerun-failed-jobs` once.
- Test: `scripts/ci-rerun-starved.test.mjs` (add to `test:ci-check`)
- Create: `.github/workflows/ci-rerun-starved.yml`. Trigger: `workflow_run` of the CI workflows, `types: [completed]`. Permissions: `actions: write` only. Job `if: github.event.workflow_run.run_attempt == 1 && github.event.workflow_run.conclusion != 'success'`. No secrets, and no checkout of the run's code; checkout is `main`'s script only.
- Modify: `scripts/check-ci-workflow.mjs` (a rule pinning this workflow's trigger, permissions and attempt guard)

**Interfaces:**
- Produces: `starvedJobs(jobs) -> Job[]` and `shouldRerun({ attempt, conclusion, jobs }) -> boolean`.

- [ ] **Step 1: Failing tests:**
  - `a cancelled job with no runner and no steps triggers one rerun`;
  - `attempt 2 never reruns`;
  - `a cancelled job that ran steps does not count`;
  - `a green run does nothing`.
- [ ] **Step 2:** Run it and expect FAIL.
- [ ] **Step 3:** Implement the script and the workflow.
- [ ] **Step 4:** Run `npm run test:ci-check` and `npm run check:ci`; both PASS.
- [ ] **Step 5:** Commit: `ci: re-run once the jobs GitHub never gave a runner`.

### Task 6: Split `ci.yml` into `server` and `desktop` workflows

**Files:**
- Create: `.github/workflows/server.yml`, workflow `name: server`. Jobs:
  - `changes`, `docs`, `ci-watchdog`;
  - `go`, `go-checks`, `go-native`, `go-ds`, `go-api`, `go-harness`, `go-postgres`, `go-fts5-arm64`, `go-lint`, `go-vuln`, `go-sqlc`, `go-release`;
  - `rust-native`, `rust-wasi`, `vectors`, `deny`;
  - `server-work-complete`, `server-ok`.
- Create: `.github/workflows/desktop-ci.yml`, workflow `name: desktop`. Jobs:
  - `changes`, `ci-watchdog`;
  - `node`, `ui`, `rust-wasm-node`, `rust-wasi` (built again here; artifacts don't cross workflows), `rust-native` (needed by `media-protocol-integration` and `core-integration`);
  - `web-build`, `encrypted-store-integration`, `media-protocol-integration`, `linux-sandbox-foundation`, `core-integration`;
  - `desktop` (`uses: ./.github/workflows/desktop.yml`);
  - `desktop-work-complete`, `desktop-ok`.
- Delete: `.github/workflows/ci.yml`. The concurrency group stays per workflow and ref.
- Modify: `scripts/ci-areas.mjs`. `JOB_GATES` are keyed by workflow, and both workflows get all areas `on` for a push to `main`. Areas: server = `go`, `core`, `text`; desktop = `web`, `e2e`, `core`.
- Modify: `scripts/check-ci-workflow.mjs`. `checkWorkflow` is split into `checkServerWorkflow` and `checkDesktopCiWorkflow`, each with its own `REQUIRED_JOBS`/`REQUIRED_STEPS`, and its reads cover both files. The `*-ok` rule must list every other job of its own workflow.
- Modify: `scripts/ci-local-plan.mjs` (it reads both workflow files; `JOB_MAP` is per workflow), `scripts/ci-local.test.mjs`, `docs/dev/ci-local.md`.
- Test: `scripts/check-ci-workflow.test.mjs`, `scripts/ci-areas.test.mjs`

**Interfaces:**
- Produces workflow names `server` and `desktop`, and check names `server-ok` and `desktop-ok`, for Tasks 7 and 9.

- [ ] **Step 1: Failing tests:**
  - in `ci-areas.test.mjs`: `a docs-only PR runs both workflows' changes job and reports their ok jobs` (areas all `false`, and the ok-job gates still run), and `a Go-only PR turns no desktop area on except through core`;
  - in `check-ci-workflow.test.mjs`: `server.yml must not contain desktop jobs`, `desktop-ci.yml must call desktop.yml`, `each ok job lists every other job of its workflow`, and `neither workflow filters paths at the trigger`.
- [ ] **Step 2:** Run them and expect FAIL.
- [ ] **Step 3:** Move each job unchanged (steps, needs, timeouts, secrets) into its workflow, then adjust its `needs` to jobs in the same file.
- [ ] **Step 4:** Run `npm run test:ci-check`, `npm run check:ci` and `npm run ci:local -- --list`. All PASS, and the list shows both workflows.
- [ ] **Step 5:** Commit: `ci: split CI into server and desktop workflows, each with its own ok check`.

### Task 7: Each release gates on its own workflow

**Files:**
- Modify: `packages/desktop/scripts/ci-run.mjs:71-80`. `findGreenPushRun` looks for workflow `desktop`, path `.github/workflows/desktop-ci.yml`.
- Modify: `packages/desktop/scripts/release-gate.mjs:128-133`. Ruleset expectation: required checks `server-ok` and `desktop-ok` instead of `ci-ok`, matching Task 9.
- Test: `packages/desktop/scripts/ci-run.test.mjs`, `release-gate.test.mjs`
- Modify: `.github/workflows/release.yml` (`tag` job, ~33-63). Add a step that runs `node scripts/server-release-gate.mjs "$TAG_SHA"`.
- Create: `scripts/server-release-gate.mjs`. It requires a `server` workflow `push` run on `main` with `head_sha == TAG_SHA` and conclusion `success`, using the existing GET-with-5xx-retry client pattern from the desktop gate.
- Test: `scripts/server-release-gate.test.mjs`

- [ ] **Step 1: Failing tests:**
  - `a green server run does not satisfy the desktop gate`;
  - `a green desktop run does not satisfy the server gate`;
  - `the server gate refuses a SHA without a green server push run`;
  - `the server gate accepts the exact SHA's green run`.
- [ ] **Step 2:** Run them and expect FAIL.
- [ ] **Step 3:** Implement and wire up.
- [ ] **Step 4:** Run `npm run test:ci-check`, `npm test -w @dilla/desktop` and `npm run check:ci`. All PASS.
- [ ] **Step 5:** Commit: `ci(release): the server release gates on a green server run, the desktop release on a green desktop run`.

### Task 8: Nightly desktop e2e on main

**Files:**
- Modify: `.github/workflows/desktop-ci.yml`. Add `schedule: - cron: '17 2 * * *'` (02:17 UTC). On `schedule`, `changes` turns every desktop area on, and the run uses `main`.
- Modify: `scripts/ci-areas.mjs` (the `schedule` event means all areas on) and `scripts/check-ci-workflow.mjs` (allow the schedule trigger on `desktop-ci.yml` only).
- Test: `scripts/ci-areas.test.mjs` (`a scheduled run turns every desktop area on`)

- [ ] **Step 1:** Write the failing test. **Step 2:** Run it and see it fail. **Step 3:** Implement. **Step 4:** Run `npm run test:ci-check` and `npm run check:ci`; both PASS. **Step 5:** Commit: `ci: run the full desktop lane nightly on main`.

### Task 9: Required checks, docs and handoff

**Files:**
- Modify: `CONTRIBUTING.md:84-86`, `packages/desktop/RELEASING.md:15`, `docs/dev/ci-local.md`. They name the two workflows, the two ok checks, the retries, the re-run of runner-less jobs, the nightly run, and the watchdog's fail-open rule.
- No code.

- [ ] **Step 1:** Update the docs, and run `npm run check:docs`; PASS.
- [ ] **Step 2:** **Jonas's go required:** change ruleset 25709839's required checks from `ci-ok` to `server-ok` + `desktop-ok`. Once this branch deletes `ci.yml`, its PR never reports `ci-ok`. So the ruleset switch and the merge happen together:
  - confirm the PR's `server-ok` and `desktop-ok` are green;
  - switch the ruleset, then merge;
  - the ruleset's existing admin bypass covers the moment in between.

  Prepare the exact `gh api` call, and run it only on his explicit yes.
- [ ] **Step 3:** Commit: `docs(ci): two workflows, two ok checks, retries and the nightly lane`.

### Task 10: Installer-side pruning of pre-migration copies

**Files:**
- Modify: `cmd/dillad/serve.go:195-199`. After a successful migration, delete `<db>.pre-migration-*` files beyond the newest 3, sorted by the Unix timestamp in the name. Log `pruned pre-migration copies kept=3 removed=<n>`, naming each removed file.
- Test: `cmd/dillad/serve_premigration_test.go` (new):
  - `TestPreMigrationCopiesKeepTheNewestThree`: five copies with timestamps 1-5 leave 3, 4, 5;
  - `TestPreMigrationPruneLeavesUnrelatedFiles`: `dilla.db-wal` and `dilla.db.bak` survive;
  - `TestPreMigrationPruneRunsOnlyAfterSuccess`: a failed migration prunes nothing.
- Modify: `docs/deploy/` upgrade notes and `deploy/install.sh:624,771` text. They say the newest 3 copies are kept.

- [ ] **Step 1:** Write the failing tests. **Step 2:** Run `go test ./cmd/dillad/ -run PreMigration` and see FAIL. **Step 3:** Implement `prunePreMigrationCopies(dbPath string, keep int, log *slog.Logger) error`. **Step 4:** Run `go test -race ./cmd/dillad/` and golangci-lint; both PASS. **Step 5:** Commit: `feat(dillad): keep the newest three pre-migration database copies`.

---

## Out of scope (queued after this plan, in order)

1. The Bot registration loop on CT 144: "exact participant KeyPackage unavailable" every 30 s since signal1.5.
2. The watchdog decision (keep, narrow or remove), now that the workflows are split.
3. Your own screen-share picker, with Discord-like sound (screenshots first).
4. Dependabot #56 (`minimatch`).
5. The download-route minors M1–M6.
6. A landing-page pipeline, when the landing page exists.
7. AUR publishing, when AUR registration reopens.
