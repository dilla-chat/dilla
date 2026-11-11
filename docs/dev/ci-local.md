# ci-local: the CI workflows on your machine

`npm run ci:local` runs the three CI workflows, `.github/workflows/server.yml` (check `server-ok`),
`.github/workflows/desktop-ci.yml` (check `desktop-ok`) and `.github/workflows/mobile.yml` (check `mobile-ok`), locally, so a failure shows up before the push instead of on
the runner. Run it before every push.

```sh
npm run ci:local                          # every job, fail fast
npm run ci:local:changed                  # only the jobs the ci-areas gates run for this branch's diff
npm run ci:local -- --only go,go-api      # some jobs, plus the jobs they need
npm run ci:local -- --only core-integration --no-deps   # reuse the artifacts of an earlier run
npm run ci:local -- --stress --only go-api,browser-media   # under CPU contention, like a loaded runner
npm run ci:local -- --list                # each workflow's jobs and what ci-local does with them
npm run ci:local -- --dry-run             # print the plan: jobs, needs, how each step maps
npm run ci:local -- --doctor              # check the prerequisites and exit
```

## How it works

ci-local doesn't keep its own list of commands. `scripts/ci-local-plan.mjs` reads the workflows and builds the jobs
from them. A job several carry (`changes`, `rust-native`, `rust-wasi`, `web-build`) runs once: it runs when any
workflow's gate holds, and its steps must be the same in every file. Matrix jobs are expanded into one job per leg, `needs:` and the job and step `if:` area gates are respected, and each
job's `run:` steps run as written in the order they appear, with `bash -eo pipefail`, `CI=true` and the
`timeout-minutes` from the workflow. Jobs that don't depend on each other run in parallel within a CPU budget
(`--jobs`, which defaults to the number of CPUs). Each job has a weight, about the number of cores it keeps busy (a
GitHub runner has 4).

Steps that only make sense on GitHub are mapped to local equivalents:

| workflow | locally |
| --- | --- |
| `actions/checkout`, `actions/cache*` | nothing: the working tree is the checkout, and `target/` and `~/.cargo` persist |
| `actions/setup-node`, `actions/setup-go` | prerequisite checks against `.nvmrc` and `go.mod` |
| `actions/upload-artifact` / `download-artifact` | a copy through `target/ci-local/artifacts/<name>/` into the path the later job reads (`internal/mlswasi/testdata`, `artifacts/`, `packages/web/dist`, …) |
| the clang 23 apt step | a check that `/usr/bin/clang-23`, or a `clang`/`llvm-ar` on PATH from the same LLVM line, exists; the local one stands in |
| `ci-apt-cache.sh`, `playwright install --with-deps` | the browsers are installed with `npx playwright install` (once per run); the system libraries are a prerequisite |
| `ci-audio-server.sh` | a check that `pactl info` reaches a running PipeWire-Pulse or PulseAudio server |
| `sudo prlimit` (rust-native) | the same `prlimit` without sudo, within the hard limit |
| the `postgres` service (go-postgres) | a throwaway `podman` (or `docker`) container with the same image, env and initdb args, published on a free local port. `DILLA_TEST_PG` points at that port. The container is removed after the job, on failure, and on Ctrl-C |
| `changes` | computed in process by `scripts/ci-areas.mjs`: every area for a full run, the diff's areas for `--changed` |
| `server-ok`, `desktop-ok` and their `*-work-complete` barriers | the summary's verdict |

The node job's `npm run test:ci-check` runs in a fresh copy of the tracked and untracked files
(`target/ci-local/clean-tree/node`), as on a runner's checkout. ci-areas.test.mjs's reader scan skips paths that
aren't tracked, so a local `packages/web/dist` doesn't trip it either way.

Steps that several jobs share run once per run: `npm ci` (before any job, so it never removes `node_modules` from
under a running job), the `install-ci-tool.sh` installs, the `wasm-pack build`s and the Playwright installs. The pinned
CI tools that `install-ci-tool.sh` downloads (golangci-lint with its gosec rules, wasm-pack, wasm-bindgen, binaryen,
cargo-deny and sqlc) land in `target/ci-local/runner-temp/dilla-ci-tools`. They go ahead of your own copies on PATH,
so the lint is the one CI runs.

`npm run check:ci` (server's docs job) runs `localMirrorProblems`. If any workflow gains a job, step, action, service,
`if:` form or `${{ }}` expression that ci-local can't map or doesn't explicitly skip (`JOB_MAP.server`,
`JOB_MAP.desktop` and `JOB_MAP.mobile`), or a shared job's copies differ, the check fails, so the mirror can't drift.

## Jobs it skips, and why

| job | why |
| --- | --- |
| `go-fts5-arm64` | it needs a native linux/arm64 runner (`ubuntu-24.04-arm`). The job tests FTS5 on arm64 itself, so a cross-build or qemu wouldn't be the same check. It runs on an arm64 machine. |
| `linux-sandbox-foundation` | requires disposable Ubuntu 22.04/24.04 runners and privileged kernel fixtures. |
| `desktop` | requires native Linux, Windows and macOS acceptance runners in `desktop.yml`. |
| `mobile-android`, `mobile-e2e-tools` | the emulator smoke test needs KVM, JDK 21, the Android SDK and an AVD, which `scripts/ci-android.sh` provisions on the runner only. It is opt-in locally: `npm run e2e:android -w @dilla/mobile` (`packages/mobile/README.md` lists its prerequisites). `mobile-unit`, `mobile-core-wasm` (its wasm-pack build is web-build's, memoised, so it runs once) and `mobile-bundle` (the mobile client build) do run. |
| `mobile-ios` | needs a macOS runner with Xcode; on a Mac, `packages/mobile/README.md` has the build. |
| `ci-watchdog` | GitHub run cancellation is replaced by the local fail-fast scheduler. |
| `codeql` (extra) | it runs only when the `codeql` CLI is on PATH, and is otherwise reported as SKIPPED with the install hint. When it runs, it creates databases for actions, go, javascript-typescript and rust and analyses them with the code-scanning suites. Results at error level or with a security severity of 7 or more fail it. |

A skipped job is always listed in the summary with its reason.

## Browser suites in parallel

The core, web and voice suites take their ports from `e2e/web/support/ports.ts`: `e2ePort(n)` is the CI port plus
`DILLA_E2E_PORT_OFFSET`. ci-local gives each browser job a port block of its own. Block `k` moves every host, control
and SFU port by `k*1000`; the suites span 7884–8615. Each job also gets its own host data root and Playwright output
directory (`target/ci-local/e2e/<job>/`, which holds the traces and the testhost and dillabot logs). The ui job's
Storybook ports move by `k*50`.

The blocks are locked across your lanes (lock files under `$XDG_RUNTIME_DIR/dilla-ci-local-locks`, or
`$TMPDIR/dilla-ci-local-locks-<uid>` without a session runtime directory), so two of your lanes never get the same
block. The directory is created 0700; ci-local refuses to use it (and says why) if it is a symlink, isn't owned by you,
or is writable by group or others. Other users' services on a block's ports are caught by the port probe below. A lock names its holder's pid and process start time; a lock whose holder is gone (or whose
pid now belongs to a later process) is taken over atomically. Before a block is handed out, its ports (TCP and UDP
7884–8616 plus the offset, or the ui job's Storybook ports) are probed: a block with anything already listening, such
as your own services, is skipped. `--base <n>` sets the first block to try (default 1; 0 is the CI ports, the most is 24), and `--e2e-jobs`
sets how many browser jobs run at once (default 3). browser-spike and browser-media still use hard-coded ports, so
they share one lock: your lanes queue for those two jobs instead of clashing.

## The job environment

Every job starts from your environment without the variables that change what a test touches or where a build goes:
every `DILLA_*` (among them `DILLA_TEST_PG`, which points the store and API tests at a server they create and drop
databases on, and the e2e port pins), `PG*`, `DATABASE_URL`, and the tool overrides `GO`, `NPM`, `CARGO`, `WASM_PACK`
and `CARGO_TARGET_DIR`. A job then gets only what its workflow sets, so go-postgres's `DILLA_TEST_PG` points only at its own
throwaway container. `CI=true` and the `GITHUB_*`/`RUNNER_*` variables are set as on a runner.

## Modes

- **fail fast** (the default): the first failing job stops the run. The running jobs are cancelled, its log path and
  last lines are printed, and the summary lists what didn't run.
- `--keep-going`: every job runs. A job whose needed job failed is skipped, and the summary says which job it needed.
- `--changed`: the jobs the `changes` job would turn on for this branch against `origin/main` (or `main`). Committed,
  staged, unstaged and untracked files all count. As in CI, the diff fails safe to everything.
- `--list`: each workflow's jobs, their matrix legs, which jobs both carry, and which ci-local skips and why.
- `--only <job,…>`: those jobs and the jobs they need. A job name from either workflow selects every matrix leg; a leg is
  `linux-sandbox-foundation-ubuntu-22.04` or a Go shard such as `go-api-2`. `--no-deps` leaves the needed jobs out and reuses their artifacts from an earlier run.
- `--stress`: the Go and browser jobs run pinned (`taskset`) to a quarter of the CPUs (at least 2), beside one
  busy-loop process per pinned CPU. This brings out the timing flakes a loaded runner does: TOTP window edges,
  animation waits, fixed-millisecond checks and package timeouts.

## Output

- `target/ci-local/<job>.log`: one log per job, with each step's command, its mapping and its output.
- `target/ci-local/summary.txt` and `summary.json`: pass, FAIL or skipped with the reason, plus durations. The same
  table is printed at the end.
- The exit status is non-zero when any job failed or was cancelled.

The jobs run on the working tree as it is. go-sqlc and the vectors checks require a clean tree, as CI sees the pushed
commit, so commit before the run that gates your push. go-release runs alone because it replaces the placeholder in
`internal/web/dist` with the real client. ci-local copies that directory aside first and puts it back afterwards
(copied beside it, then swapped in by rename), once every process the job started is gone, Ctrl-C included. If a run
is killed outright during go-release, the next run puts the directory back from the pending snapshot before it does
anything else.

## Prerequisites

`npm run ci:local -- --doctor` checks the selected jobs' prerequisites and names what to install:

- bash, git, curl, tar, coreutils (`sha256sum`), jq, util-linux (`prlimit`; `taskset` for `--stress`)
- Node.js of `.nvmrc` (24) with npm
- Go of `go.mod`'s `go` line (1.27.4)
- rustup; `rust-toolchain.toml` pins the toolchain, with clippy, rustfmt and the wasm32 targets
- clang and llvm-ar from the LLVM 23.1 line (`/usr/bin/clang-23`, or a `clang`/`llvm-ar` on PATH that reports 23.1)
- the system libraries Playwright's Chromium needs (`npx playwright install-deps chromium` on Debian/Ubuntu, or your
  distribution's packages)
- a running PipeWire-Pulse or PulseAudio server and `pactl`, for the browser-media audio suites
- the GTK3 headers and pkg-config (the Go jobs) and bsdtar from libarchive (the node job)
- podman or docker, for go-postgres
- network access to the release hosts that `install-ci-tool.sh` downloads from, to proxy.golang.org (govulncheck), and
  to unicode.org for the profile-text tables
- optional: the CodeQL CLI
