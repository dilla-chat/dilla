# Release gate recognises tiered desktop-gpui runs: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan.

**Goal:** the desktop-gpui release gate accepts the nightly and release runs it is meant to accept. Today it refuses
every one of them, so alpha.2 cannot release.

**Cause:** `scripts/server-release-gate.mjs:80` `isOwnRun` requires `r.name === gated.name` (`'desktop-gpui'`). But
#188 (d0232a592, 2026-11-18) gave desktop-gpui.yml a `run-name:`, and from then on GitHub's API returns the run name
in `name`. Observed (`gh api /repos/dilla-chat/dilla/actions/runs/<id>`):

| run         | name                     | display_title            | path                                |
|-------------|--------------------------|--------------------------|-------------------------------------|
| 45370379135 | `desktop-gpui [release]` | `desktop-gpui [release]` | `.github/workflows/desktop-gpui.yml` |
| 45343271392 | `desktop-gpui [nightly]` | `desktop-gpui [nightly]` | (same)                              |
| 45370276222 | `desktop-gpui [pr]`      | `desktop-gpui [pr]`      | (same)                              |
| 44049307281 (2026-11-17, before #188) | `desktop-gpui` | (the commit title) | (same) |

The test fixture `gpuiRun` (`scripts/server-release-gate.test.mjs:16`) pairs `name: 'desktop-gpui'` with a tiered
`display_title`, a shape GitHub never sends, so the tests passed.

The same `isOwnRun` filter in `scripts/ci-same-tree.mjs:77` therefore never finds a desktop-gpui PR run: the same-tree
skip never fires for desktop-gpui. Fixing `isOwnRun` fixes that too.

**Spec:** the gate's own doc comment (`server-release-gate.mjs:1-30`) and `ACCEPTED_TITLES` (line 39). Jonas,
2026-11-24: "Merge and release" for alpha.2.

## Global Constraints

- The workflow is identified by `r.path === .github/workflows/<file>`, plus the existing repository, head_repository,
  sha, id and run_number checks. `r.name` no longer decides identity. A run with any `name` but the right path counts
  as the workflow's run, as GitHub's own `workflow_id` listing does.
- The tier is still decided only by `display_title` plus `event` (`isTierRun`, `ACCEPTED_TITLES`, `RELEASE_TITLE`).
  That check doesn't loosen.
- Drop `name` from `GATED_WORKFLOWS` if nothing else reads it. Otherwise keep it, used only for labels.
- `scripts/desktop-gpui-gate.mjs:147` (`trigger.name !== RELEASE_WORKFLOW_NAME`) checks desktop-gpui-release.yml,
  which has no `run-name` (run 45162691235: name `desktop gpui release`). It stays unchanged; say so in the report.
- Test fixtures use the real shapes: `gpuiRun` gets `name` equal to its `display_title`, as in the table above.

## Review Focus

1. A fork's or another workflow file's run with a forged title is still refused (path and repository checks).
2. A `[pr]` run is still never accepted for a release.
3. The dispatched-run match (`isOurs`, lines 217-218 and 231) accepts the real-shaped run.
4. Same-tree finds real-shaped desktop-gpui PR runs. The server workflow (no run-name, `name === 'server'`) behaves
   the same as before.

### Task 1: identify runs by path; real-shaped fixtures

**Files:**
- modify `scripts/server-release-gate.mjs`, `scripts/server-release-gate.test.mjs`;
- `scripts/ci-same-tree.mjs` only if needed, and its test file if one exists (`scripts/ci-same-tree.test.mjs`).

- [ ] **Step 1, failing tests.**
  - Change `gpuiRun` so that `name` defaults to the run's `display_title`: a tiered name, as GitHub sends it.
  - Add `a_real_shaped_tier_run_is_accepted` style tests. The gate accepts a green nightly whose `name` is
    `desktop-gpui [nightly]`, and follows a dispatched run whose `name` is `desktop-gpui [release]`.
  - Add a same-tree test with a PR run named `desktop-gpui [pr]`.
  - Run `node --test scripts/server-release-gate.test.mjs` (and the same-tree test) and see the new tests fail.
- [ ] **Step 2, implement:** remove the `r.name === name` condition from `isOwnRun`, and tidy `GATED_WORKFLOWS` as the
  constraint says. Add one line in its doc comment: GitHub reports `run-name` in `name`, so path identifies the
  workflow.
- [ ] **Step 3, verify:**
  - `node --test scripts/*.test.mjs` all green;
  - `npm run ci:local -- --only <the job that runs scripts tests>` if it exists (find it in `scripts/ci-local-plan.mjs`);
  - otherwise the node tests are enough.
- [ ] **Step 4, commit:** `ci: the release gate knows a desktop-gpui run by its path, not its run name`.
