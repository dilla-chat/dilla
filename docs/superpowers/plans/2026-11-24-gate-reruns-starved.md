# Release gate re-runs its own starved dispatched run: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan.

**Goal:** a desktop-gpui release no longer fails because GitHub never gave one macOS job a runner.

**Cause:** run 45383253187 (the gate's dispatched `desktop-gpui [release]` run for alpha.2, 2026-11-24) went red only
because `gpui-mac-package` was "not acquired by Runner of type hosted even after multiple attempts" (cancelled, no
runner, no steps; macOS arm64 capacity). The other 11 jobs were green, and a re-run of that one job was green. Two gaps:

1. `ci-rerun-starved.yml` never ran for it. The run's actor is `github-actions[bot]`: the gate dispatches with
   `GITHUB_TOKEN`, and GitHub raises no `workflow_run` event for runs a `GITHUB_TOKEN` started.
2. `scripts/server-release-gate.mjs` (the dispatched-run follow loop, around line 233) refuses as soon as the run
   concludes anything but success.

**Spec:** Jonas, 2026-11-24: "Fix it!" (the recurring Mac cancellations that sink the release). The rules for what counts
as starved are `scripts/ci-rerun-starved.mjs` (`neverStarted`, `shouldRerun`). A real failure never gets a second try.

## Global Constraints

- Reuse `shouldRerun` and `starvedJobs` by importing them from `./ci-rerun-starved.mjs`. Don't copy the predicate.
- Only the gate's own dispatched run, and only on its attempt 1, is re-run, and at most once per gate invocation. The
  re-run is `POST /repos/{r}/actions/runs/{id}/rerun-failed-jobs`. The jobs it judges are those of attempt 1
  (`/actions/runs/{id}/attempts/1/jobs?per_page=100`, paged like ci-rerun-starved does).
- A dispatched run that is red for any other reason refuses as today, without a POST. So does an attempt 2 that
  concludes anything but success, a re-run that the API refuses, or a starved attempt 2.
- After the re-run, the gate keeps following the same run id. That run is "the dispatched run" only if it still passes
  every identity check in the loop. The overall bound grows by `RERUN_BOUND_MS = 120 min`, once: gpui-mac-package's own
  `timeout-minutes` is 120.
- Log lines name job ids and run ids only, never job names (the same reason ci-rerun-starved gives).
- The server path (`workflow === 'server'`), the nightly or tier-run search, and the dispatch-appear logic are
  unchanged.
- Every sparse checkout that fetches `scripts/server-release-gate.mjs` must also fetch `scripts/ci-rerun-starved.mjs`,
  or the import fails at runtime. Check every workflow that runs the gate or `ci-same-tree.mjs`: `desktop-gpui.yml`
  same-tree, `desktop-gpui-release.yml`, `desktop-gpui-sign.yml`, `release.yml`, plus any other `grep -rn
  server-release-gate .github` finds.
- Update the gate's doc comment (step 3) and the gate paragraph in `docs/dev/testing.md`, if it describes this.

## Review Focus

1. A run with a real failure plus a starved job is never re-run (rerunning re-runs every failed job).
2. There is no loop: a second starved conclusion refuses, and there is no second POST.
3. The bound arithmetic: the re-run gets its 120 min, and the gate still ends.
4. An import missing from a sparse checkout would only show up in a release, so every checkout is covered.
5. A re-run POST that fails (non-2xx) refuses with a clear message and doesn't retry. The write is never retried, as in
   `call` in ci-rerun-starved.

### Task 1: the gate re-runs a starved dispatched run once

**Files:**
- modify `scripts/server-release-gate.mjs`, `scripts/server-release-gate.test.mjs`;
- modify the workflows named in the constraints (sparse-checkout lists only);
- modify `docs/dev/testing.md` if it describes the gate's dispatch step.

- [ ] **Step 1, failing tests** in `server-release-gate.test.mjs`, using the file's existing fake API and fixture
  style:
  - `a_dispatched_run_red_only_from_a_starved_job_is_rerun_once_and_its_green_attempt_passes`: attempt 1 completes
    `failure`, and its attempt-1 jobs are one green job plus one `{status:'completed', conclusion:'cancelled',
    runner_name:null/'' , steps:[]}`. Expect exactly one POST to `.../runs/<id>/rerun-failed-jobs`. Then the run reports
    `run_attempt: 2`, in progress and then `success`, and the gate passes.
  - `a_dispatched_run_with_a_real_failure_is_refused_without_a_rerun`: a starved job plus a job that concluded
    `failure` gives a refusal and no POST.
  - `a_rerun_attempt_that_is_red_again_is_refused_without_a_second_rerun`: attempt 2 concludes `failure` with starved
    jobs. Expect a refusal and exactly one POST in total.
  - `a_refused_rerun_post_refuses_the_release`: the POST answers 403. Expect a refusal and no retry of the POST.
  - `the_rerun_extends_the_bound_by_RERUN_BOUND_MS`: with a fake clock, an attempt 2 still in progress past
    `BOUND_MS` but before `BOUND_MS + RERUN_BOUND_MS` keeps waiting, and refuses after it.
  - Run `node --test scripts/server-release-gate.test.mjs` and see these fail.
- [ ] **Step 2, implement** in the follow loop. Export `RERUN_BOUND_MS`. The `waitOr` bound reads a variable that the
  re-run raises. Run identity checks are unchanged.
- [ ] **Step 3, sparse checkouts and docs**, as in the constraints.
- [ ] **Step 4, verify:** `node --test scripts/*.test.mjs` all green. Also, for each edited workflow, `npx --yes
  @action-validator/cli` or the repo's own workflow lint if `scripts/ci-local-plan.mjs` has one (run that job via
  `npm run ci:local -- --only <job>`).
- [ ] **Step 5, commit:** `ci: the desktop-gpui release gate re-runs its dispatched run once when a job never got a runner`.
