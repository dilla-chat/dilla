# Testing: the CI tiers and the release gate

Which checks run when, what each tier proves, and how a release finds a run it can trust. The design and its reasons are
in `docs/superpowers/specs/2026-11-18-ci-speed-design.md`; where that differs from this page, the workflows and
`scripts/server-release-gate.mjs` are right. `docs/dev/ci-local.md` covers running the CI jobs on your machine.

## The three tiers of desktop-gpui

`.github/workflows/desktop-gpui.yml` is one workflow with one `TIER` value. Every run is titled `desktop-gpui [<tier>]`
(`run-name`), and the release gate reads the tier from that title.

| Tier | When it runs | What it runs |
|---|---|---|
| `pr` | every pull request that touches the workflow's path filters (`clients/desktop/**`, `core/**`, the Cargo files and the other paths listed under `on:`); a push to `main` that touches them and that no green PR run already proved (see below) | all Linux legs except the Bot ignored tests; Windows build, unit tests, clippy and `gpui-win-package`; macOS build, `cargo test`, clippy and `gpui-mac-package` |
| `nightly` | `schedule` at 03:00 UTC (`0 3 * * *`) on `main`, or `workflow_dispatch` with `tier=nightly` | the `pr` tier plus the macOS call tests, the macOS parity screens, the Linux Bot ignored tests (the Bot shard) and the macOS VideoToolbox decode |
| `release` | `workflow_dispatch` with `tier=release`, started by the release gate on a tag | the same set as `nightly`, on the tagged commit |

The four extra checks are moved out of the `pr` tier, not weakened: their assertions are the same in every tier. In the
`pr` tier the Bot shard is absent from the job matrix, so nothing builds or starts for it. The workflow's tier
conditions are pinned by `scripts/check-ci-workflow.mjs`.

What each tier proves:

- `pr`: the change builds, passes the unit tests and lints on Linux, Windows and macOS. It says nothing about calls,
  parity screens, Bot behaviour or hardware video decode.
- `nightly` and `release`: the full set on one tree. Only these may release a desktop build. Because the nightly runs
  on `main`, it also keeps `main`'s sccache scope warm for the PR runs that read it.

Server (`server.yml`), `desktop-ci.yml` and `mobile.yml` have no tiers. `server.yml` runs every area on every push to
`main`, with no skip.

### A main push that repeats a PR run

A push to `main` runs the `pr` tier again unless a green PR run already proved the same tree. `check-tree`
(`scripts/ci-same-tree.mjs`) decides, and the job `same-tree` that follows it is named `same-tree: skipped` (or
`same-tree: ran`). The other jobs skip themselves only when it is the skipped one, which happens only when:

- a `pull_request` run of this workflow into `main`, from this repository, concluded success;
- that run's head contained `main`'s previous commit (so the PR was up to date); and
- its tree equals the pushed commit's tree.

Any error, missing field or unexpected answer means "not covered", and the push runs in full. A skipped run concludes
success but proves nothing: the release gate never counts a run with a `same-tree: skipped` job.

### Rust compile cache

Every Rust job in CI uses sccache (`SCCACHE_GHA_ENABLED`, `RUSTC_WRAPPER=sccache`) and prints its stats at the end. The
release and sign builds do not: they stay clean and `--locked`.

The Linux dependency step in desktop-gpui CI bounds its apt fetch (`DILLA_APT_FETCH_TIMEOUT=600`, with retries;
`scripts/ci-apt-install.sh`). The release and proof installs have no bound.

## A red nightly

A red nightly is loud and blocks releases until it is green again. The `nightly-report` job
(`scripts/ci-nightly-issue.mjs`) runs after every other job of a nightly run on `main` (not a branch dispatch, and not a cancelled run):

- if the run was red, it opens the issue `nightly desktop-gpui red` (label `ci-nightly`) or, when that issue is
  already open, comments on it. Both link the run;
- if the run was green, it closes the issue.

Whoever is on duty fixes a red nightly the same day (CI is fail-fast). The next green nightly closes the issue; the
gate stays shut while it is open.

## The release gate

`scripts/server-release-gate.mjs` re-derives everything from the GitHub API and never trusts a tag's name. A run
counts for a tag when its head commit's tree equals the tagged commit's tree.

### Desktop (`desktop-gpui-v*`)

`desktop-gpui-release.yml` runs the gate with `--workflow=desktop-gpui` and `RELEASE_TAG` set:

1. It refuses while an open issue labelled `ci-nightly` exists.
2. It passes on the newest green `desktop-gpui.yml` run on the tag's tree titled `desktop-gpui [nightly]` or
   `desktop-gpui [release]`. A `[pr]` run never counts, and neither does any run with a `same-tree: skipped` job.
3. If such a run is queued or in progress, it waits for it. Otherwise it dispatches `desktop-gpui.yml` on the tag with
   `tier=release` and follows that run: green passes; anything else, `cancelled` included, refuses.
4. It waits at most 150 minutes in total, and checks the issue again before it passes.

`desktop-gpui-sign.yml` re-checks the same rule through `scripts/desktop-gpui-gate.mjs` but has no `RELEASE_TAG`, so it
never dispatches. It waits for a qualifying run that is queued or in progress only until its own job's 75-minute
timeout (`timeout-minutes` on the `gate` job); the script's 150-minute bound applies to the release workflow's gate,
whose `tag` job has no shorter timeout. The bound is the same 150 minutes in both gates; the sign gate's 75-minute job timeout cuts it short. With no qualifying run it refuses. In practice the sign run starts only after
the release run succeeded, so the release-tier run it needs is normally already green.

A release therefore costs one full Mac run on the tagged tree. When the tag's tree was already proved by a nightly (a
tag cut right after a green nightly, with no change in between), it costs nothing extra.

### Server (`v*`)

`release.yml` needs a green `server.yml` run on the tag's tree that is a push to `main`. A
`pull_request` run never counts, even on the same tree: a PR run may skip areas, the `main` push runs every one. While
only such a run is queued or in progress, the gate polls every 30 seconds, up to 150 minutes. It refuses at once when the
tree has no run at all or its run concluded anything but success.

A green run of one workflow never satisfies the other workflow's gate.

### When the gate refuses

| Refusal | What to do |
|---|---|
| an open `ci-nightly` issue | fix the nightly, or run `desktop-gpui` with `tier=nightly` by hand on `main` and let it go green; the issue closes |
| the dispatched `[release]` run failed or was cancelled, so the release workflow's `tag` job refused | a re-run only helps for a flake: run `desktop-gpui-release.yml` again by hand (`workflow_dispatch`, "Use workflow from" the tag itself, and the `tag` input set to the same tag; the input is required), which dispatches a new `[release]` run. A real fix is a new commit, so it needs a new tag |
| the 150-minute bound expired | re-run `desktop-gpui-release.yml` on the tag; it finds the in-progress or green `[release]` run |
| the sign workflow's gate found no qualifying run (a release run made a draft, but the tree has no green nightly or release run, or the nightly turned red meanwhile) | the sign gate refuses a re-run (attempt 1 only) and the release workflow refuses a tag that already has a release, so neither can be repeated as is. Green the tree first (fix the nightly, or run `desktop-gpui` with `tier=release` on the tag), then delete the draft (`gh release delete <tag> --yes`, keeping the tag) and run `desktop-gpui-release.yml` by hand as in the row above; its success starts a fresh sign run (`clients/desktop/RELEASING.md`, "When the sign run fails") |
| a server tag on a superseded `main` commit whose `server` run was cancelled | re-run that commit's `server` run; the tree then has a green `main` push run |
| no run on the tree for a server tag | the tag is not on a `main` commit that ran `server.yml`; tag a `main` commit whose push run is green, or re-run that commit's `server` run (`server.yml` has no dispatch trigger) |
