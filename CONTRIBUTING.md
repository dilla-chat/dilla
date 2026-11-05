# Contributing to dilla

## Sign-off

Every commit must carry a `Signed-off-by:` line (`git commit -s`), which certifies the Developer
Certificate of Origin in `DCO`. There is no contributor licence agreement and there will not be one.

## Cryptography rule

dilla uses only published standards, implemented in public code: MLS (RFC 9420), the RFC 9605
SFrame shape for media frames, HKDF-SHA256, HMAC-SHA256, SHA-256, AES-128-GCM, AES-256-GCM (the
protocol/06 header objects and protocol/04 attachments), Ed25519, X25519.
A pull request that introduces a construction not defined in one of those documents, or a
"simplified" variant of one, is closed without review. Ask first in an issue if you think an
exception is needed.

## Rust toolchain

The repository pins its Rust toolchain in `rust-toolchain.toml`. That file is a **rustup**
feature: a distribution-packaged `cargo` ignores it without printing anything at all, so a
contributor on such a machine builds with whatever their package manager last installed while
believing the pin applies. Run `scripts/doctor-rust.sh` to find out; it exits non-zero and lists
what is missing.

Install a user-scoped rustup once, without touching the system compiler:

```sh
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
  | sh -s -- -y --no-modify-path --default-toolchain none --profile minimal
~/.cargo/bin/rustup toolchain install 1.98.1 --profile minimal \
  --component clippy --component rustfmt \
  --target wasm32-unknown-unknown --target wasm32-wasip1
```

Then put `~/.cargo/bin` at the front of your `PATH`. That makes `cargo` and `rustc` rustup proxies
for every project on the machine, not only this one; it is the intended effect, and
`rustup self uninstall` reverses all of it. Nothing under `/usr` is modified, so a
distribution-packaged Rust keeps working if you remove the `PATH` entry.

## Protocol changes

`protocol/` is normative. A change to any document there needs, in the same pull request:
1. a bump of the affected version integer (`e2ee_version`, `media_version` or the wire version),
2. regenerated vectors (`npm run vectors`) with the diff explained,
3. a note in `protocol/07-versioning.md` describing the compatibility window.

## Documentation

The README's "What works today" and "What is not there yet", the user guide in `docs/user/` and its
screenshots describe what a person can do with the code at `main`, with a date. Every plan's closing task
brings them up to date in the same pull request:

1. update the README's "What works today" table and "What is not there yet" list, and their date;
2. update `docs/user/` for every screen, string or limit the plan changed (quote the strings from
   `packages/web/src/strings/en.ts`);
3. re-shoot the screenshots: `npm run build -w @dilla/web`, then
   `GO=$(command -v go) npm run docs:shots -w @dilla/e2e` (`e2e/scripts/shoot-docs.mjs`; it starts its own
   `dilla-testhost` on 127.0.0.1:8471 and 8472, and `--port`/`--control` pick others). Add a shot to the
   script when the plan adds a screen worth showing. The script is not part of CI.

The operator guide, `docs/deploy/README.md`, is updated by the task that changes what an operator runs or
configures.

## Commits

Conventional commits: `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`, `ci:`.

## Continuous integration

CI is two workflows, both run on every pull request and every push to main: `server` (`.github/workflows/server.yml`:
the docs gates, the Go jobs, the Rust core checks and the release build) and `desktop`
(`.github/workflows/desktop-ci.yml`: the script gates, the UI, the wasm core, the integration suites and the
`desktop.yml` acceptance). Each reports one check, `server-ok` and `desktop-ok`, so a desktop failure never blocks a
server change, or the reverse. These two checks become the required checks for `main` when this change merges and the
ruleset switches from `ci-ok` to them; until the switch, `ci-ok` is the required check.

The `desktop` workflow also runs nightly on main at 02:17 UTC with every area on, so test drift surfaces within a day.

A pull request runs only the parts of CI it touches. Each workflow's `changes` job
(`scripts/ci-areas.mjs`, which holds the path table and has its own tests) reads the changed paths and sets one output
per area:

| Area | Paths | Also forces |
| --- | --- | --- |
| docs | `docs/**`, any `*.md`, `LICENSE*`, `DCO` | nothing: only server's `docs` job runs (the docs, brief and copy checks and their tests, and `check:ci`) |
| go | `internal/**`, `cmd/**`, `go.mod`, `go.sum`, `sqlc*`, `deploy/**`, `packages/design-tokens/**` (a Go test reads its CSS), `protocol/*.md` (a Go test parses the route tables) | nothing: a server change does not run the desktop lane |
| core | `core/**` (Markdown included: a vendored crate compiles its README), `testkit/**`, `Cargo.*`, `protocol/vectors/**` | go, web, e2e |
| web | `packages/**`, `package*.json`, `.nvmrc`, `docs/design/flows/**` (tests compare the copy tables with `en.ts`), `internal/sfu/testdata/**` (the LiveKit schema check) | e2e |
| e2e | `e2e/**` | |
| ci | `.github/**`, `scripts/**` | everything |

The browser jobs in CI (`browser-spike`, `browser-media` and `core-integration`) run only Chromium projects; the desktop
lane runs the Electron client. The Firefox and WebKit projects stay in the Playwright configs for local runs. A push to
main runs everything, and so does a pull request whose diff cannot be computed or touches a path no rule names. A gate
tests `!= 'false'`, so a missing output runs the job.
Skipped jobs count as passed for required checks. Each `*-ok` job needs every other job of its workflow and is red
when any failed or was cancelled. No container image is built or published. A new
job belongs to one workflow (`REQUIRED_JOBS` in `scripts/check-ci-workflow.mjs`) and needs `needs: changes`, an
`if:`, an entry in that workflow's `JOB_GATES`, and a place in its `*-work-complete` and `*-ok` needs;
`npm run check:ci` enforces them. A new test or build that reads a file from another area needs a path rule: a guard test in
`scripts/ci-areas.test.mjs` scans the sources for such reads.

Each workflow has a `ci-watchdog` job (`scripts/ci-watchdog.py`). It cancels the run at the first failed job, so a broken
build does not keep the other jobs busy, except the desktop e2e shards (`desktop / desktop-client-*` and
`desktop / desktop-voice-*`): they run to the end, so one round shows every failing test, and `desktop-ok` still fails
on any failed shard. The client and voice matrices have `fail-fast: false` with no `--max-failures`; the build matrices
keep `fail-fast: true`. The watchdog ignores a job that GitHub never gave a runner. It fails open: when its own API
calls fail, it stops watching with a warning and never cancels a run.

`scripts/retry-on.mjs` retries a command, at most three attempts, only when its output matches a known network error.
It wraps `go mod download` in every Go job (`release.yml` included) and the macOS notarization staple in the native
desktop build (a retry notarizes again, so only the Apple service errors qualify). Evidence uploads run only when the
evidence exists.

`ci-rerun-starved.yml` re-runs the failed jobs of a `server` or `desktop` run once. It acts only on attempt 1 of a run
that concluded `failure`, when at least one job never got a runner and no job really failed. A run with a real failure
is never re-run.

A release needs a green push run of its own workflow on main for the tagged commit: `desktop` for the desktop release
(`packages/desktop/RELEASING.md`), `server` for `release.yml` (`scripts/server-release-gate.mjs`). A `v*` tag on a main
commit from before this change runs that commit's own `release.yml`, which has no server gate.
