# Automated desktop releases Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Pushing `desktop-v<version>` on a main commit whose `ci` push run is green signs the desktop release with fresh keys and publishes the versioned release and the channel feed with no human step, no commit to main by any workflow, no per-release files, and no private key reachable by anything that does not run from `main`.

**Architecture:** Two workflows.
1. `desktop-release-request.yml` (name `desktop release request`) starts on a `desktop-v*` tag push. It is unprivileged: top-level `permissions: contents: read`, no environment, no secrets, no `uses:`, one job that echoes. Whatever the tagged commit's copy of it does, it can reach nothing.
2. `desktop-release.yml` (the keyed workflow) is triggered by `workflow_run` on the completion of that request workflow. GitHub always runs the **default branch's** copy of a `workflow_run` workflow, so only reviewed main code ever holds keys. Its environment `desktop-release` admits deployments from branch `main` only, no tags. It has two jobs:
   - `sign` (environment `desktop-release`; `contents: read`, `actions: read`, `pull-requests: read`; no `npm ci`) re-derives everything from the GitHub API and git objects: the tag name is only an untrusted hint, resolved through the API to a commit; ancestry of `origin/main`; the version; the green `ci` push run on exactly that commit. It then downloads that `ci` run's artifacts (never any artifact of the triggering run), generates `release-notes.md` from merged PR titles, derives `acceptance.json` including an automated review attestation, writes the keys from secrets into a 0700 dir inside one script, runs the unchanged `signDesktopRelease`, deletes the keys, digests the output, and uploads it.
   - `publish` (no environment, no secrets, `contents: write`, `actions: read`) verifies the digest, then runs the existing `publish-release.mjs` plan/publish verification and creates both releases.

Checkouts in the keyed workflow are of `main` (the `workflow_run` default) and never of the tag. The tagged commit is read only as git data (`git show <sha>:packages/desktop/package.json`, tree hash) and compared byte-for-byte against main for the three public-pin files. One-time key creation is `release-keys.mjs setup`.

**Tech stack:** Node 24 ESM (`.mjs`, repo scripts), `node:test`, `gh` CLI, GitHub Actions with SHA-pinned first-party actions only.

---

## Facts this plan rests on (verified in the repo at 29023b36)

- `signDesktopRelease` (`release-feed.mjs`) reads `acceptance.json`, `ci-run.json`, `ci-jobs.json`, installers, sandbox sidecars, `build-provenance-*.json`, `performance-*.json`, `installed-account-*.json`, `release-notes.md`, `security-review.md` from the artifacts dir and `metadata-private.pem` (Ed25519 PKCS8 PEM), `sparkle-private.key` (base64 32-byte seed), `linux-sandbox-private.pem` (Ed25519 PEM) from the keys dir. It refuses keys that do not match `release-trust.json` / `linux-sandbox-component.json`, and verifies the whole candidate before and after signing. Its imports are `node:` built-ins and repo files only, so the sign job needs no `npm ci`.
- Nothing in the repo produces `acceptance.json`, `release-notes.md` or `security-review.md`; a human wrote them. `verify-release.mjs` line 93 requires `source.review.status === 'approved'`, `review.tree === source.tree`, a sha256; lines 95-96 read `security-review.md` and compare its digest; `PUBLIC_ACCEPTANCE_RECORDS` lists `security-review.md`. Nothing client-side (`src/*.ts`) validates `source.review`. Other occurrences of the strings are in `release-publish.test.ts`, old plans and `docs/deploy/releases/v0.1.0-signal1.2.md` (history; left alone).
- `verifyBundle` also demands `run.name === 'ci'`, `run.conclusion === 'success'`, same-repository head, and a fixed inventory of named native/client/voice/package-acceptance jobs and steps. A docs-only commit skips the `desktop` job (`scripts/ci-areas.mjs`), so its green run has no installers and the sign job fails closed; tag the newest commit that touched code.
- Installer artifact names: `desktop-installers-*`, `desktop-performance-*`, `desktop-installed-account-*` (default 90-day retention). `ci.yml` uses `cancel-in-progress: true` per ref, so a later push to main cancels an earlier main run, and a cancelled run is not green.
- Public pins of the key halves: `packages/desktop/release-trust.json` (`metadataPublicKeys`, `sparklePublicKey`, also read by `electron-builder.cjs` into `SUPublicEDKey`), `packages/desktop/src/update-keys.ts`, `packages/desktop/linux-sandbox-component.json` (`publicKeys`, `publishedAt`, `expiresAt` at most 93 days apart; embedded into the launcher at build). `test/linux-appimage-launcher.test.ts:60` hard-codes the current sandbox key in a negative assertion.
- `packages/desktop/scripts/release-keys.mjs` exists with `create DIR` (writes private keys to disk), `normalize-sparkle`, `import-public`. `check-release-workflow.mjs` (+ `test/release-workflow.test.ts`) forbids `secrets.` file-wide in `desktop-release.yml`.
- Events from `GITHUB_TOKEN` (tags it creates) do not start workflow runs. `release.yml` (tags `v*`) has a `contents: write` job. Feed tags `desktop-beta`/`desktop-stable` do not match `desktop-v*`.
- The publisher creates each release as a draft, uploads, then PATCHes `draft: false`. `GET /releases/tags/{tag}` omits drafts, so a rerun would create a second draft (fixed in Task 8).
- For a tag-push run, `workflow_run.head_branch` is the tag name. Whether the `branches:` filter of `workflow_run` matches tag names must be confirmed (Task 9); the gate does not depend on the filter.

## Global Constraints

- No private key is ever written to disk by `release-keys.mjs`, printed, logged, put in argv, or placed in an environment variable of anything but the one sign step. Buffers that held key material are zeroed.
- No workflow commits, tags or pushes. By permissions: the keyed `sign` job has no write scope; `publish` writes releases only.
- `desktop-release.yml` triggers only on `workflow_run` of `desktop release request`, `types: [completed]`, `branches: ['desktop-v*']`. The request workflow triggers only on `push` of tags `desktop-v*`. Neither has `workflow_dispatch`, `pull_request*`, `schedule`, `repository_dispatch`, `workflow_call`.
- `environment: desktop-release` exists only in the `sign` job of `desktop-release.yml`. `secrets.DESKTOP_*` appear exactly three times, in the `env:` of one step of that job.
- **`workflow_run` rules.** The keyed job never uses `actions/checkout` with `ref:`, `repository:` or `path:` pointing at `github.event.workflow_run.*`, never downloads artifacts of the triggering run (`run-id` comes only from the gate's output for the `ci` run), never executes anything read from the tagged commit, and passes `github.event.workflow_run.*` values only through `env:` of the gate step, where they are untrusted hints validated by regex and re-derived from the API.
- All actions are first-party (`actions/*`) pinned by full SHA. The request workflow uses none.
- Top-level `permissions` is `{}` in the keyed workflow and `contents: read` in the request workflow; each keyed job declares its own. `sign`: `contents: read`, `actions: read`, `pull-requests: read`. `publish`: `contents: write`, `actions: read`.
- Fresh keys only; algorithms exactly as the scripts expect (metadata Ed25519 PKCS8 PEM; Sparkle Ed25519 32-byte seed base64; sandbox Ed25519 PKCS8 PEM with raw 32-byte public key base64). The three public keys must be pairwise different (`assertDedicatedKeys`).
- Acceptance, build-provenance, installed-account, performance, sandbox and Sparkle checks in `verifyBundle` stay exactly as strict. The only change to `verify-release.mjs` is replacing the human review file with the automated attestation (Task 2).
- Release notes contain only PR numbers and PR titles, rendered as escaped plain text; never PR bodies, commit messages or author names.
- Every task: strict TDD, narrow per-task gate, `npm run ci:local` before the final push. Commits end with the Co-Authored-By and Claude-Session trailers.

## Review Focus

Opus review, boundary-security level, in this order:
1. The `workflow_run` trampoline: nothing from the triggering run or the tagged commit is trusted or executed; every value the keyed workflow uses is re-derived; the Task 10 checks fail on each pitfall.
2. `release-sign-job.mjs` and the `sign` job: can any key byte reach disk outside the 0700 dir, an artifact, a log, argv, or a step other than the named one? Cleanup in `finally`; exact output file set.
3. `release-gate.mjs`: fail-closed order; tag resolved through the API (lightweight and annotated); main ancestry; version from git data; byte equality of the three pin files with main; green push run for exactly that sha.
4. The attestation change in `verify-release.mjs`: strictness unchanged elsewhere; inventory computed identically at sign and publish time.
5. `release-notes.mjs`: escaping, no bodies, no mentions or links from titles.
6. `release-keys.mjs setup`: stdin-only transport, in-memory discipline, partial-failure behaviour, environment policy exactly branch `main`, refusal when `main` is unprotected.
7. The sign-to-publish handoff digest.
8. Honest residual-risk wording.

## Threat model

Assets: the three private keys; the right to publish a feed that installed clients and Sparkle accept; the integrity of what is signed (the CI-built installers of a green main commit).

Controls common to all rows: secrets are environment-scoped to `desktop-release`, whose deployment policy admits only the ref `main`; jobs that can run on ref `main` come only from workflow files merged to main, and the checker (Task 10) fails CI if any other workflow names the environment or the secrets; the keyed workflow is `workflow_run`-triggered, so GitHub loads it from main whatever the tag points at; the signed content is fixed by the gate to a main commit with a green `ci` push run.

| Actor | What they can do | How the design limits it |
|---|---|---|
| **PR author** (fork or same-repo) | Run code in PR CI, edit workflow files in their PR, try `pull_request_target`/`workflow_run` tricks. | PR refs are not `main`, so GitHub refuses the environment. A `pull_request_target` run does execute on the base ref, but only for workflow files already on main, and no such file may name the environment (checker). A fork's push does not trigger the base repository's `workflow_run`. A PR cannot create tags. Signed installers are only those `ci` built on main pushes (`event=push`, `branch=main`). Edits to the keyed workflow take effect only after merge to protected main. |
| **Compromised third-party action** | Run code in the job that uses it with that job's token and env. | No third-party actions in either release workflow (allow-list of four `actions/*` SHAs; the request workflow uses none). Keys exist only inside one `node` process in one step, in a 0700 dir deleted in `finally`; `secrets.*` is referenced only by that step, so earlier actions never receive it. The only action after signing is `upload-artifact` over the output dir. The publish job uses `download-artifact` and an npm-installed publisher but holds no keys. Residual: a compromise of `actions/*` itself (checkout/setup-node/download run earlier on the same ephemeral runner and could leave a watcher on `$RUNNER_TEMP`); accepted. |
| **Stolen `GITHUB_TOKEN` from another workflow** | Whatever that run's scopes allow, for the run's life: write contents or releases, re-run or cancel runs, delete artifacts. | It cannot read environment secrets, cannot edit environments or branch policies (admin), and tags it creates do not trigger workflows. A `contents: write` token could tamper with an unpublished draft or the mutable feed release, but the publisher trusts only bytes it re-verifies: installers are re-downloaded from the green CI run and checked against hashes inside metadata signed by keys the token never saw; unsigned lookalikes in the feed are rejected by clients (at worst a denial of updates until the next release). It cannot substitute CI artifacts of another run; the build-provenance digests are signed. Published versioned releases are immutable. If it can push to main it is the next row. |
| **Push access to non-main branches** | Commit anything, including workflow files, to branches; open PRs; create tags if not restricted. | Branch code never runs with the keys: the environment admits only `main`, and a tag on a branch commit only starts the unprivileged request workflow; the keyed workflow from main then refuses the tag (commit not an ancestor of `origin/main`, no green `ci` push run on main for it). Modified workflow files on the tagged commit are never executed with privileges. |
| **Someone who can push tags** | Start a release run for any commit they can name. | The tagged commit's workflow files and scripts are never executed with keys, so key theft through a tag is closed (it was the residual of a tag-triggered design). They can cause a release only of a commit that is on main, has a green `ci` push run, whose `package.json` version equals the tag, whose version is greater than every published `desktop-v*` of the channel and not yet published, and whose public-pin files equal main's. That is a premature or unwanted release of already-reviewed code, not key exposure. Optional defence in depth: `setup --tag-ruleset` restricts `desktop-v*` creation to admins. |

**What remains:** (1) an admin, or anyone able to merge to `main` without review (branch-protection settings), can change the keyed workflow or the environment policy and read the keys; `setup` refuses to run unless `main` is protected, but admin bypass is outside the design; (2) compromise of GitHub-owned actions or GitHub itself; (3) who may start a release is limited only as described above; (4) the automated attestation proves CI passed on the tree, not that a human reviewed it.

## End-to-end path for 0.1.0-beta.1

Precondition: `packages/desktop/package.json` is already `0.1.0-beta.1`.

1. **Jonas, once:** `! node packages/desktop/scripts/release-keys.mjs setup`, from the automation branch's checkout (needs `gh auth` as repo admin; `main` must be protected). It prints fingerprints and progress only. Effects: environment `desktop-release` with one deployment policy, branch `main`; three secrets set from stdin; `release-trust.json`, `src/update-keys.ts`, `linux-sandbox-component.json` rewritten in the working tree, which he commits on the branch.
2. **The PR (this automation plus the public pins) is merged.** It changes workflow and `.json` files, which `ci-areas` treats as every-area, so the merge commit's `ci` run builds all desktop installers with the new public keys.
3. **Main's `ci` run on that merge commit is green** (merge nothing else first, or the run is cancelled).
4. **Claude pushes the tag:** `git tag desktop-v0.1.0-beta.1 <merge commit> && git push origin desktop-v0.1.0-beta.1`. `desktop release request` completes in seconds; `desktop release` starts from main; `sign` runs gate, notes, assemble, sign, digest, upload; `publish` verifies and publishes `desktop-v0.1.0-beta.1` and `desktop-beta`. Desktop releases do not change `--desktop-downloads` and need no CT 144 upgrade.
5. A failure before `publish` leaves nothing public. Fix on main and re-tag the new green commit (delete and re-create the tag if the same version is reused), or use "Re-run jobs" on the keyed run (it re-derives from the tag as it then is). After `publish` started, rerun is idempotent (Task 8).

## Tasks

Test files live in `packages/desktop/test/` and run with the package's existing `node --test` runner (confirm the exact command in Task 1 by reading `packages/desktop/package.json`). Script tests needing `.ts` imports follow `release-publish.test.ts`.

### Task 1: `ci-run.mjs`, shared API client and green-run rule

**Files:** Create `packages/desktop/scripts/ci-run.mjs`, `packages/desktop/test/ci-run.test.mjs`. Modify `publish-release.mjs` (use the shared helpers; no behaviour change).

**Interfaces:**
```js
export const REPOSITORY = 'jonasthim/dilla';
export function createGithubApi({ token, fetchImpl = fetch }): api   // api.github.com only, path must start /repos/jonasthim/dilla/, redirect:'error', 30 s timeout, 404 -> null only if optional404
export async function findGreenPushRun(api, sha): Promise<run>        // throws /no green ci push run/
export async function fetchCiJobs(api, runId): Promise<job[]>          // filter=latest, 100 per page
export async function resolveTagCommit(api, tag): Promise<string>      // GET git/ref/tags/<tag>; object.type 'commit' -> sha; 'tag' -> GET git/tags/<sha> once, must be a commit; anything else throws
```
`findGreenPushRun` queries `actions/workflows/ci.yml/runs?head_sha=<sha>&event=push&branch=main&status=completed&per_page=100` and accepts only runs with `name==='ci'`, `path==='.github/workflows/ci.yml'`, `event==='push'`, `head_branch==='main'`, `head_sha===sha`, `status==='completed'`, `conclusion==='success'`, and `repository.full_name === head_repository.full_name === REPOSITORY`; returns the highest `run_number`.

**Tests:** `findGreenPushRun returns the green push run on exactly that sha`; `findGreenPushRun refuses pull_request, cancelled, failure, in_progress and other-branch runs`; `findGreenPushRun refuses a run whose head_sha differs from the requested sha`; `findGreenPushRun refuses a fork head repository`; `findGreenPushRun refuses a workflow named ci at another path`; `fetchCiJobs pages until a short page` (100 + 3 gives 103, requests page 1 and 2); `resolveTagCommit follows an annotated tag to its commit`; `resolveTagCommit refuses a tag pointing at a tree or blob`; `createGithubApi rejects a path outside the repository`; `createGithubApi never follows redirects`.

- [ ] Failing tests, implement, green; `release-publish.test.ts` still green.
- **Gate:** the two test files. **Commit:** `refactor(desktop): shared CI-run lookup and tag resolution for the release flow`

### Task 2: automated review attestation replaces the human review file

**Files:** Modify `packages/desktop/scripts/verify-release.mjs`, `packages/desktop/test/release-publish.test.ts`. (`release-feed.mjs` needs no code change: it forwards `source` and `PUBLIC_ACCEPTANCE_RECORDS`.)

**Changes (exact, minimal):**
- Remove `'security-review.md'` from `PUBLIC_ACCEPTANCE_RECORDS`.
- New export `requiredJobInventory(jobs, version)`: returns an array sorted by job `name`, each `{ name, conclusion, steps: [{ name, conclusion }] }`, containing exactly the jobs `verifyBundle` requires (native targets, client shards, voice jobs, the package producer, the three package-acceptance jobs) and, for each, only the steps `verifyBundle` requires by name. Throws if a required job or step is missing or not `success`. Built from the same name lists `verifyBundle` uses (hoist those lists to module constants; do not duplicate them).
- Replace line 93 and lines 95-96 with: `source.review` must have exactly the keys `status`, `ciRun`, `tree`, `inventory`; `status === 'automated'`; `ciRun === source.ciRun`; `tree === source.tree`; `inventory.sha256 === sha256(JSON.stringify(requiredJobInventory(jobs)))`. The notes check (line 94, `source.notes.sha256` vs `release-notes.md`) stays.
- `publish-release.mjs` log text drops "review".

**Tests:** in `release-publish.test.ts`, update the fixture so `source.review = {status:'automated', ciRun, tree, inventory:{sha256}}` and drop `security-review.md`; keep every existing negative case. New: `publication refuses an attestation whose inventory digest does not match the run's jobs` (flip one step conclusion after signing); `publication refuses an attestation for another ci run id`; `publication refuses an attestation for another tree`; `publication refuses the old human review shape` (`status:'approved'`); `requiredJobInventory is sorted, complete and covers every job verifyBundle requires` (mutation: delete each required job in turn; both the function and `verifyBundle` throw); `requiredJobInventory ignores extra unrelated jobs` (digest unchanged when an unrelated job is added).

- **Gate:** `node --test packages/desktop/test/release-publish.test.ts`. **Commit:** `feat(desktop): automated CI attestation replaces the per-release security review file`

### Task 3: `release-notes.mjs`, generated notes from merged PR titles

**Files:** Create `packages/desktop/scripts/release-notes.mjs`, `packages/desktop/test/release-notes.test.mjs`.

**Interfaces:**
```js
export function escapeMarkdown(text: string, max = 200): string
  // strips control characters and newlines (to one space), collapses whitespace, truncates to max with an ellipsis,
  // backslash-escapes ASCII punctuation  \ ` * _ { } [ ] < > ( ) # + - . ! | ~ : " ' $ % ^ = / ? ,  then writes '&' as '&amp;' and '@' as '&#64;'
export function previousDesktopVersion(tagNames: string[], version: string): string | null
  // among tags matching the desktop tag regex, the highest semver precedence strictly below `version`; numeric prerelease compare
export async function generateNotes({ api, version, sha, previous }): Promise<string>
  // previous = tag name or null. With a previous tag: GET compare/<previous>...<sha> (paginated, 100/page), then for each commit
  // GET commits/<c>/pulls, keep PRs with merged_at != null and base.ref === 'main'; read ONLY `number` and `title`. Dedupe by number, sort ascending.
  // Output: "# Dilla desktop <version>\n\n" + lines "- #<n> <escaped title>\n"; more than 250 PRs -> first 250 and "- and N more merged pull requests\n".
  // No previous tag: "# Dilla desktop <version>\n\nFirst desktop release.\n".
// CLI: release-notes.mjs generate VERSION SHA OUTPUT_FILE   (token from GH_TOKEN; lists tags via GET git/matching-refs/tags/desktop-v; writes with flag 'wx')
```

**Tests:** `escapeMarkdown neutralises links, images, html, code fences, headings and mentions` (inputs `[x](http://e)`, `![i](u)`, `<img src=x onerror=1>`, triple backticks, `# h`, `@octocat`, `&lt;`; the output has no unescaped `[`, `<`, `` ` ``, `@`); `escapeMarkdown flattens newlines and control characters`; `escapeMarkdown truncates at the limit without cutting an escape pair`; `generateNotes lists numbers and titles only and never reads a PR body` (mock PRs carry poisoned `body`, `user`, `html_url`; a Proxy records no access; output has none of them); `generateNotes skips unmerged PRs and PRs not targeting main`; `generateNotes dedupes a PR that spans several commits`; `generateNotes caps the list at 250`; `previousDesktopVersion picks the numerically previous release (0.1.0-beta.10 after 0.1.0-beta.9) and ignores non-desktop and higher tags`; `generateNotes for a first release names no PRs`; `the CLI refuses to overwrite its output`.

- **Gate:** `node --test packages/desktop/test/release-notes.test.mjs`. **Commit:** `feat(desktop): generate release notes from merged PR titles, escaped`

### Task 4: `release-assemble.mjs`, derive `acceptance.json`

**Files:** Create `packages/desktop/scripts/release-assemble.mjs`, `packages/desktop/test/release-assemble.test.mjs`, `packages/desktop/test/support/release-fixture.mjs` (extract the fixture builder from `release-publish.test.ts`; both tests use it).

**Interface:**
```js
export async function assembleAcceptance({ artifactsDirectory, run, jobs, version, commitTree }): Promise<{ acceptance }>
  // acceptance = { format:1, ciRun: run.id, commit: run.head_sha, head: run.head_sha, tree: commitTree,
  //   review: { status:'automated', ciRun: run.id, tree: commitTree, inventory:{ sha256: sha256(JSON.stringify(requiredJobInventory(jobs, version))) } },
  //   notes: { sha256 of release-notes.md in artifactsDirectory },
  //   builds:{[t]:{sha256}} (BUILD_TARGETS, from build-provenance-<t>.json), performance:{[t]:{sha256}} and installedAccounts:{[t]:{sha256}} (TARGETS) }
  // Every referenced file must be a regular, non-symlink file; release-notes.md 1..262144 bytes.
// CLI: release-assemble.mjs assemble VERSION ARTIFACTS_DIR COMMIT_TREE   (reads ci-run.json, ci-jobs.json, release-notes.md from ARTIFACTS_DIR; writes acceptance.json with flag 'wx')
```
Nothing named `release-inputs`, `inputs.json`, `reviewedTree` or a per-release `security-review.md` exists anywhere.

**Tests:** `assembleAcceptance derives every digest from the downloaded artifacts`; `assembleAcceptance output is accepted by verifyDesktopSigningCandidate` (shared fixture; signing succeeds with the derived acceptance); `assembleAcceptance refuses a missing, symlinked or oversized record`; `assembleAcceptance refuses jobs that lack a required job or step`; `assemble CLI never overwrites an existing acceptance.json`.

- **Gate:** `node --test packages/desktop/test/release-assemble.test.mjs packages/desktop/test/release-publish.test.ts`. **Commit:** `feat(desktop): derive acceptance.json and the automated attestation from the accepted CI run`

### Task 5: `release-gate.mjs` (re-derive everything from main)

**Files:** Create `packages/desktop/scripts/release-gate.mjs`, `packages/desktop/test/release-gate.test.mjs`.

**Interface:**
```js
export const TAG = /^desktop-v((0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?)$/;
export async function runGate({ env, git, api, outputDirectory }): Promise<{ tag, version, channel, sha, tree, ciRun, previous }>
// CLI: node release-gate.mjs OUTPUT_DIR. Env: GITHUB_*, GH_TOKEN, HINT_TAG (= workflow_run.head_branch), HINT_RUN (= workflow_run.id).
//      Writes ci-run.json and ci-jobs.json (flag 'wx'); appends tag=, version=, channel=, sha=, tree=, run=, previous= to $GITHUB_OUTPUT.
```
Checks, in order, each fail-closed with its own message. `HINT_*` are untrusted and compared only to what the API says:
1. Running as the keyed workflow from main: `GITHUB_ACTIONS==='true'`, `GITHUB_EVENT_NAME==='workflow_run'`, `GITHUB_REPOSITORY==='jonasthim/dilla'`, `GITHUB_REF==='refs/heads/main'`, `GITHUB_WORKFLOW_REF` ends with `.github/workflows/desktop-release.yml@refs/heads/main`, `git rev-parse HEAD === GITHUB_SHA`.
2. `HINT_TAG` matches `TAG`. `sha = resolveTagCommit(api, HINT_TAG)`: the API, not the event payload, says where the tag points.
3. The triggering run, read from `GET actions/runs/<HINT_RUN>`: `path === '.github/workflows/desktop-release-request.yml'`, `event==='push'`, `head_sha === sha`, same-repository head, `conclusion==='success'` (consistency only; failure means a moved tag or a spoofed hint).
4. `git merge-base --is-ancestor <sha> origin/main` (full-depth checkout of main).
5. `git show <sha>:packages/desktop/package.json` parsed as data: `.version` equals the tag version. `tree = git rev-parse <sha>^{tree}`.
6. `git diff --quiet <sha> HEAD -- packages/desktop/release-trust.json packages/desktop/src/update-keys.ts packages/desktop/linux-sandbox-component.json`, else "public pins changed after this commit was built".
7. `findGreenPushRun(api, sha)`; `fetchCiJobs`; write both JSON files.
8. No published release `desktop-v<version>` exists (a draft is allowed); version strictly greater, by semver precedence, than every published `desktop-v*` release of the same channel (`GET releases`, paginated). `previous` is `previousDesktopVersion(tags, version)` as a tag name or empty.
9. `linux-sandbox-component.json` from main's HEAD (equal to the tag's by 6): `expiresAt` at least 30 days after now.
No code, script, workflow or document from the tagged commit is read except that `package.json` version string and the tree hash.

**Tests:** `gate passes for a main-ancestor tag with green run and matching version` (outputs and both JSON files); `gate refuses when not running as the main-branch workflow_run` (four env variants: wrong event, wrong ref, wrong workflow ref, HEAD != GITHUB_SHA); `gate resolves an annotated tag through the API, ignoring the tag message`; `gate refuses a hint tag that is not a desktop version tag` (includes `desktop-v1.0.0; rm -rf`, `../x`, leading `v`); `gate ignores a lying HINT_RUN` (run's head_sha differs from the API-resolved sha: refused); `gate refuses a triggering run from another workflow path, a fork, a non-push event or a failed run`; `gate refuses a tag commit that is not an ancestor of main`; `gate refuses a version that differs from the tagged package.json` (reads via mocked `git show`; asserts no checkout of the sha occurred); `gate refuses when a public-pin file differs between the tag commit and main`; `gate refuses red, cancelled, PR and other-sha ci runs`; `gate refuses an already published version and allows a draft rerun`; `gate refuses a version not greater than the newest published release of the channel` (includes beta.10 vs beta.9); `gate refuses a sandbox component expiring within 30 days`.

- **Gate:** `node --test packages/desktop/test/release-gate.test.mjs`. **Commit:** `feat(desktop): release gate that re-derives tag, ancestry, version and green CI from main`

### Task 6: `candidate-digest.mjs` (artifact handoff)

**Files:** Create `packages/desktop/scripts/candidate-digest.mjs`, `packages/desktop/test/candidate-digest.test.mjs`.

**Interface:**
```js
export const EXPECTED_FILES = (channel, version) => string[]   // publicReleaseRecords(channel) + `dilla-${version}-linux-x64.sandbox.sig`, sorted (no security-review.md after Task 2)
export async function writeDigest(directory, { channel, version }): Promise<string>   // exactly EXPECTED_FILES, regular non-symlink files, each <= 64 MiB; writes SHA256SUMS ("<sha256>  <name>\n", sorted, flag 'wx'); returns sha256 hex of SHA256SUMS
export async function verifyDigest(directory, expectedHex, { channel, version }): Promise<void>
// CLI: candidate-digest.mjs write DIR CHANNEL VERSION -> appends digest=<hex> to $GITHUB_OUTPUT and prints it; verify DIR CHANNEL VERSION DIGEST
```
**Tests:** `digest round trip verifies`; `verify refuses one flipped byte in any file`; `verify refuses an extra file, a missing file and a symlink`; `verify refuses a substituted SHA256SUMS with a consistent set` (the outer digest from the sign job catches it); `write refuses to overwrite SHA256SUMS`.

- **Gate:** `node --test packages/desktop/test/candidate-digest.test.mjs`. **Commit:** `feat(desktop): digest-checked handoff of the signed release between jobs`

### Task 7: `release-sign-job.mjs` (keys from secrets, sign, delete)

**Files:** Create `packages/desktop/scripts/release-sign-job.mjs`, `packages/desktop/test/release-sign-job.test.mjs`.

**Interface:**
```js
export const KEY_ENV = Object.freeze({
  DESKTOP_METADATA_PRIVATE_PEM: 'metadata-private.pem',
  DESKTOP_SPARKLE_PRIVATE_KEY: 'sparkle-private.key',
  DESKTOP_LINUX_SANDBOX_PRIVATE_PEM: 'linux-sandbox-private.pem' });
export async function signInJob({ env, version, artifactsDirectory, outputDirectory, tempRoot, sign = signDesktopRelease }): Promise<void>
// CLI: node release-sign-job.mjs VERSION ARTIFACTS_DIR OUTPUT_DIR   (tempRoot = $RUNNER_TEMP)
```
Refuses unless `GITHUB_ACTIONS==='true'`, `GITHUB_REF==='refs/heads/main'`, `GITHUB_EVENT_NAME==='workflow_run'`, and all three variables are non-empty. Creates `mkdtemp(tempRoot/dilla-keys-)` with mode 0700; writes each value `{flag:'wx', mode:0o600}`; deletes the three names from `process.env`; calls `sign`; in `finally` overwrites each key file with zeros, fsyncs, unlinks, removes the dir; a cleanup failure fails the job. Never logs a value or length.

**Tests:** `signInJob refuses to run outside the main-branch workflow_run`; `signInJob refuses a missing key variable and writes nothing`; `key files are 0600 in a 0700 directory under tempRoot and absent after success`; `key files are gone after the signer throws` (original error rethrown); `process.env no longer holds the key variables while signing`; `an end-to-end sign with freshly generated test keys produces a bundle that passes verifyDesktopBundle and candidate-digest verify` (real `signDesktopRelease`, the shared fixture, trust pins swapped via the fixture's temp `release-trust.json` as `release-publish.test.ts` does); `no key material appears in captured stdout or stderr`.

- **Gate:** `node --test packages/desktop/test/release-sign-job.test.mjs`. **Commit:** `feat(desktop): sign inside the keyed job from environment secrets, deleting keys after`

### Task 8: `publish-release.mjs` runs from main on the sign job's output and tolerates reruns

**Files:** Modify `packages/desktop/scripts/publish-release.mjs`, `packages/desktop/test/release-publish.test.ts`.

**Changes (exact):**
- Run context is the keyed workflow: require `GITHUB_EVENT_NAME==='workflow_run'`, `GITHUB_REF==='refs/heads/main'`. The tag and commit come from `DESKTOP_TAG` and `DESKTOP_SHA` (job outputs of `sign`), validated by the Task 5 `TAG` regex and 40-hex. The publisher re-resolves the tag through `resolveTagCommit` and requires it equals `DESKTOP_SHA`, requires `merge-base --is-ancestor DESKTOP_SHA origin/main`, and requires `release.source.commit === release.source.head === DESKTOP_SHA`. It no longer reads `GITHUB_SHA`/`GITHUB_REF_NAME` for the tag.
- `candidate` = `DESKTOP_RELEASE_CANDIDATE` (absolute, under `$RUNNER_TEMP`). `packages/desktop/releases/` is gone; the `git diff` exclusion is removed.
- Release `target_commitish` is `DESKTOP_SHA`.
- `findRelease(tag)`: on a 404 from `/releases/tags/<tag>` also scan `GET /releases?per_page=100` pages for `draft===true && tag_name===tag`.
- Release bodies come from the candidate's `release-notes.md` (generated, escaped text). Drafts remain the transient state for atomic attachment; the end state is published.

**Tests:** `publisher refuses to run outside a main-branch workflow_run`; `publisher refuses a tag that resolves to a different commit than DESKTOP_SHA`; `publisher refuses a signed source commit that is not DESKTOP_SHA`; `publisher refuses a DESKTOP_SHA that is not an ancestor of main`; `publisher takes the candidate from DESKTOP_RELEASE_CANDIDATE and refuses a relative or missing path`; `publisher finds an existing draft by tag and does not create a second release`; `publisher publishes the versioned release before touching the feed` (ordered call log); existing tests updated for the env inputs.

- **Gate:** `node --test packages/desktop/test/release-publish.test.ts packages/desktop/test/ci-run.test.mjs`. **Commit:** `feat(desktop): publish from main on the signed candidate, resume drafts`

### Task 9: the two workflows

**Files:** Create `.github/workflows/desktop-release-request.yml`. Rewrite `.github/workflows/desktop-release.yml`. (Action SHAs copied from the current file; `upload-artifact` from `desktop.yml`.)

`desktop-release-request.yml`, exact:
```yaml
name: desktop release request
on:
  push:
    tags: ['desktop-v*']
permissions:
  contents: read
jobs:
  request:
    runs-on: ubuntu-24.04
    timeout-minutes: 2
    steps:
      - run: echo "Desktop release requested; the privileged workflow re-derives everything from main."
```
`desktop-release.yml`, exact structure:
```yaml
name: desktop release
on:
  workflow_run:
    workflows: ['desktop release request']
    types: [completed]
    branches: ['desktop-v*']
concurrency: { group: desktop-release, cancel-in-progress: false }
permissions: {}
jobs:
  sign:
    if: github.event.workflow_run.event == 'push' && github.event.workflow_run.conclusion == 'success'
    runs-on: ubuntu-24.04
    timeout-minutes: 45
    environment: desktop-release
    permissions: { contents: read, actions: read, pull-requests: read }
    outputs: { tag, sha, version, channel, run, digest }   # steps.gate.outputs.* and steps.digest.outputs.digest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7
        with: { fetch-depth: 0, persist-credentials: false }      # no ref:, default = main tip
      - uses: actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1 # v7
        with: { node-version-file: .nvmrc }
      - name: Gate (re-derives tag, ancestry, version and green ci run)
        id: gate
        run: node packages/desktop/scripts/release-gate.mjs "$RUNNER_TEMP/candidate"
        env: { GH_TOKEN: '${{ github.token }}', HINT_TAG: '${{ github.event.workflow_run.head_branch }}', HINT_RUN: '${{ github.event.workflow_run.id }}' }
      - name: Fetch the accepted ci run's artifacts
        uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with: { github-token: '${{ github.token }}', run-id: '${{ steps.gate.outputs.run }}', repository: jonasthim/dilla, pattern: 'desktop-{installers,performance,installed-account}-*', merge-multiple: true, path: '${{ runner.temp }}/candidate' }
      - name: Generate release notes from merged PR titles
        run: node packages/desktop/scripts/release-notes.mjs generate "$VERSION" "$SHA" "$RUNNER_TEMP/candidate/release-notes.md"
        env: { GH_TOKEN: '${{ github.token }}', VERSION: '${{ steps.gate.outputs.version }}', SHA: '${{ steps.gate.outputs.sha }}' }
      - name: Derive acceptance.json and the automated attestation
        run: node packages/desktop/scripts/release-assemble.mjs assemble "$VERSION" "$RUNNER_TEMP/candidate" "$TREE"
        env: { VERSION: '${{ steps.gate.outputs.version }}', TREE: '${{ steps.gate.outputs.tree }}' }
      - name: Sign (the only step that receives secrets)
        run: node packages/desktop/scripts/release-sign-job.mjs "$VERSION" "$RUNNER_TEMP/candidate" "$RUNNER_TEMP/signed"
        env: { VERSION: ..., DESKTOP_METADATA_PRIVATE_PEM: '${{ secrets.DESKTOP_METADATA_PRIVATE_PEM }}', DESKTOP_SPARKLE_PRIVATE_KEY: ..., DESKTOP_LINUX_SANDBOX_PRIVATE_PEM: ... }
      - name: Digest the signed output
        id: digest
        run: node packages/desktop/scripts/candidate-digest.mjs write "$RUNNER_TEMP/signed" "$CHANNEL" "$VERSION"
        env: { CHANNEL: ..., VERSION: ... }
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with: { name: desktop-signed-release, path: '${{ runner.temp }}/signed', if-no-files-found: error, retention-days: 3 }
  publish:
    needs: sign
    runs-on: ubuntu-24.04
    timeout-minutes: 30
    permissions: { contents: write, actions: read }
    steps: checkout (fetch-depth 0, default main); setup-node;
      download-artifact name desktop-signed-release -> $RUNNER_TEMP/signed;
      run candidate-digest.mjs verify (env CHANNEL, VERSION, DIGEST from needs.sign.outputs);
      npm ci --ignore-scripts;
      download-artifact pattern desktop-installers-*, run-id needs.sign.outputs.run, repository jonasthim/dilla -> desktop-release-artifacts;
      run publish-release.mjs plan, then publish (env GH_TOKEN, DESKTOP_TAG, DESKTOP_SHA, DESKTOP_RELEASE_CANDIDATE)
```
All `${{ }}` values reach scripts through `env:`; no `${{` in any `run:` line. The event-payload hints appear only in the gate step's `env:` and in the job `if`.

Steps: (1) write both files; (2) read GitHub's `workflow_run` documentation and confirm that `branches: ['desktop-v*']` matches a tag-push run's `head_branch`; if it does not, remove the `branches` line and its checker pin (the gate re-derives the tag, so nothing weakens; the trigger then also fires for branch-push runs, which the gate refuses); (3) run `actionlint` if `ci:local` carries it.

**Verification limits:** the keyed path cannot be executed locally; the first real tag is the rehearsal, and every failure before `publish` is harmless.

- **Gate:** `node scripts/check-ci-workflow.mjs` (after Task 10), `npm run ci:local`. **Commit:** `ci(desktop): unprivileged tag request workflow and a keyed workflow_run release from main`

### Task 10: `check-ci-workflow.mjs` extensions (and retire `check-release-workflow.mjs`)

**Files:** Modify `scripts/check-ci-workflow.mjs`, `scripts/check-ci-workflow.test.mjs`. Delete `packages/desktop/scripts/check-release-workflow.mjs` and `packages/desktop/test/release-workflow.test.ts` (their `secrets.`-anywhere ban contradicts the new shape; every rule is carried over below).

**Interface:**
```js
export const DESKTOP_RELEASE_SECRETS = ['DESKTOP_METADATA_PRIVATE_PEM', 'DESKTOP_SPARKLE_PRIVATE_KEY', 'DESKTOP_LINUX_SANDBOX_PRIVATE_PEM'];
export const DESKTOP_RELEASE_ACTIONS = [ /* the four actions/* SHAs, exact */ ];
export function checkDesktopReleaseWorkflow(root): string[]   // wired into the CLI next to checkDesktopWorkflow
```
Assertions (each problem prefixed with the file name):
1. **Request workflow shape.** `desktop-release-request.yml`: `name: desktop release request`; `on:` exactly `push:\n    tags: ['desktop-v*']`; top-level `permissions:` exactly `contents: read`; exactly one job `request`; no job-level `permissions`, `environment`, `env`, `secrets` or any `uses:` (no actions, no checkout); no `${{ secrets`.
2. **Keyed trigger.** `desktop-release.yml` `on:` is exactly `workflow_run` with `workflows: ['desktop release request']` (string-equal to the request workflow's `name`), `types: [completed]`, `branches: ['desktop-v*']`; no other trigger (`push`, `pull_request*`, `workflow_dispatch`, `schedule`, `repository_dispatch`, `workflow_call`, `merge_group`).
3. **Job `if`** of `sign` is exactly the pinned string (event `push` and conclusion `success`).
4. **Permissions.** Top-level `permissions: {}`; jobs exactly `sign` and `publish`; `sign` permissions exactly `contents: read`, `actions: read`, `pull-requests: read` (no `contents: write`, `id-token`, `packages`, anything else); `publish` exactly `contents: write` and `actions: read`, `needs: sign`, no `environment`.
5. **Environment confined.** `environment: desktop-release` occurs exactly once in the whole `.github/workflows/` tree: in the `sign` job of `desktop-release.yml`. The three `DESKTOP_RELEASE_SECRETS` occur in `${{ secrets.` exactly three times in total across all workflow files, all inside the step named "Sign (the only step that receives secrets)"; no `secrets: inherit` or `toJSON(secrets)` in the release workflows; no `env:` at job or workflow level in `sign`.
6. **workflow_run pitfalls.** In `desktop-release.yml`: no `ref:`, `repository:` or `path:` key under any `actions/checkout`; no occurrence of `workflow_run.head_sha` or `workflow_run.head_repository`; `workflow_run.head_branch` and `workflow_run.id` only in the `env:` of the step named "Gate (re-derives ...", and `workflow_run.event`/`.conclusion` only in the job `if`; every `download-artifact` has `run-id: ${{ steps.gate.outputs.run }}` or `${{ needs.sign.outputs.run }}` (never `workflow_run.id`, never omitted); no `${{` interpolation inside any `run:` line; no `run:`/`uses:` references a path of a tag checkout.
7. **Actions.** Every `uses:` in the two files is one of `DESKTOP_RELEASE_ACTIONS` (exact SHA); `sign` uses only checkout, setup-node, download-artifact, upload-artifact; `sign`'s checkout has `persist-credentials: false`; no `continue-on-error`, `always()`, `|| true`, `if: ${{ false }}`.
8. **Order.** In `sign`: gate, download, notes, assemble, sign, digest, upload in that order, with only `run:` steps between the download and the sign step; `publish` runs `candidate-digest.mjs ... verify` before `publish-release.mjs ... plan`.
9. **No other workflow** references `desktop-release` as an environment, `DESKTOP_` secrets, `release-sign-job.mjs`, or triggers with `workflow_run` on `desktop release request`.
10. **`release-keys.mjs` source (text):** contains no write call whose path argument mentions `private`; defines the environment policy with exactly one `{ name: 'main', type: 'branch' }` and no `type: 'tag'` policy.

**Tests (mutations of the real files; each asserts a problem; plus one clean run):** `desktop release workflows as committed have no problems`; `request workflow gaining secrets, an environment, contents: write, a uses: line or a checkout is reported` (five mutations); `keyed workflow gaining push, pull_request, workflow_dispatch, a different upstream name, a missing types or branches filter, or an extra trigger is reported`; `removing or changing the sign job if is reported`; `removing environment: desktop-release from sign, adding an environment to publish, or naming the environment in another workflow is reported`; `adding contents: write, id-token: write or any scope to sign is reported`; `removing contents: write from publish is reported`; `a fourth secrets reference, a secrets reference outside the sign step, secrets: inherit and toJSON(secrets) are reported`; `checkout of workflow_run.head_sha, or any ref:, repository: or path: on checkout, is reported`; `download-artifact with the triggering run id, or without run-id, is reported`; `workflow_run.head_branch outside the gate env is reported`; `a ${{ }} interpolation in a run line is reported`; `an unpinned or third-party action, or an action between download and sign, is reported`; `release-keys.mjs writing a private key path or adding a tag policy is reported`.

- **Gate:** `node --test scripts/check-ci-workflow.test.mjs`; `node scripts/check-ci-workflow.mjs`. **Commit:** `ci: pin the desktop release trampoline, environment, secrets and permissions`

### Task 11: `release-keys.mjs setup`

**Files:** Modify `packages/desktop/scripts/release-keys.mjs` (exists; extended), `packages/desktop/test/release-keys.test.ts`, `packages/desktop/test/linux-appimage-launcher.test.ts` (line 60 reads the sandbox key from `linux-sandbox-component.json` instead of the hard-coded `qQe0Z4...`).

**Removed:** `create DIR` (writes private keys to disk) and `normalize-sparkle`. `import-public` stays and shares `writePublicTrust`.

**Interfaces:**
```js
export function generateKeyMaterial(): {
  metadata: { privatePem: Buffer, publicPem: string },
  sparkle:  { seedBase64: Buffer, publicBase64: string },
  sandbox:  { privatePem: Buffer, publicBase64: string },
  zero(): void }
export function encodeBase64Buffer(bytes: Buffer, { wrap }?: { wrap?: number }): Buffer   // standard base64 into a Buffer, no string intermediate
export async function setupRelease({ repo = 'jonasthim/dilla', env = 'desktop-release', rotate = false, tagRuleset = false, acceptUnprotectedMain = false, gh = defaultGh, now = new Date(), root }): Promise<{ fingerprints }>
// CLI: release-keys.mjs setup [--rotate] [--tag-ruleset] [--accept-unprotected-main] [--repo owner/name] | import-public PUBLIC_JSON
```
`gh` = `{ api(method, path, jsonBody?), secretSet(name, buffer), secretNames(env) }`; `secretSet` spawns `gh secret set NAME --env desktop-release --repo owner/name` with `stdio:['pipe','ignore','pipe']` and writes the Buffer to stdin; argv never holds key material.

Order, aborting on the first error:
1. Preflight: `GET user`; `GET repos/{repo}` has `permissions.admin`; the three public files have no uncommitted changes; `main` is protected (`GET repos/{repo}/branches/main` `protected===true` or an active branch ruleset covers it), else abort unless `acceptUnprotectedMain`, printing that the whole design depends on it.
2. `PUT environments/{env}` with `{ deployment_branch_policy: { protected_branches: false, custom_branch_policies: true }, can_admins_bypass: false, reviewers: [], wait_timer: 0 }`; list `deployment-branch-policies`; delete every policy other than `{ name: 'main', type: 'branch' }`; create it if missing; re-GET and assert exactly that one policy.
3. Only with `tagRuleset` (optional defence in depth): repository ruleset `desktop-release tags` (target `tag`, include `refs/tags/desktop-v*`, rules `creation`, `update`, `deletion`, `non_fast_forward`, bypass actor repository Admin role).
4. If any of the three secrets already exists and not `rotate`: abort before generating anything.
5. Generate; self-test (sign/verify probe with each key); `assertDedicatedKeys`; build the new `linux-sandbox-component.json` (same `goVersion` and `helper`; `publicKeys:[sandboxPublic]`; `generation` unchanged on first setup, +1 on rotate; `publishedAt = now` to the second; `expiresAt = now + 90 days`) and validate with `validateConfiguration`.
6. Set the three secrets from the Buffers; `zero()` in `finally`.
7. Only after all three succeeded, write `release-trust.json`, `src/update-keys.ts`, `linux-sandbox-component.json`.
8. Print fingerprints (sha256 of each public DER), the three files to commit, and "no private key was written to disk".
If step 6 fails midway, print which secret names were set and that repository files are untouched; recover with `--rotate`.

**Tests (injected fake `gh`, no network, no spawn):** `generateKeyMaterial produces keys in the formats signDesktopRelease reads`; `encodeBase64Buffer equals Buffer.toString('base64') and never yields a string` (lengths 0..300, wrapped and not); `zero() clears every private Buffer`; `setupRelease sends each key only on stdin` (fake records bytes; a `spawn` spy proves argv has none of the key bytes); `setupRelease writes no private key to disk, stdout or stderr`; `setupRelease leaves exactly one deployment policy, branch main, and no tag policy` (starts with a stale tag policy and no environment); `setupRelease fails if the policy read-back differs`; `setupRelease refuses an unprotected main without --accept-unprotected-main`; `setupRelease creates the tag ruleset only with --tag-ruleset`; `setupRelease refuses to replace existing secrets without --rotate and generates nothing`; `setupRelease leaves repository files untouched when a secret write fails` (reports which names were set); `setupRelease public output satisfies the verifier's pins` (`assertDedicatedKeys` passes; `update-keys.ts` equals `release-trust.json`; `electron-builder.cjs` loads the new `sparklePublicKey`); `release pins agree` (runs in normal CI on the committed files); `import-public still writes trust and update-keys and reads no private material`.

- **Gate:** `node --test packages/desktop/test/release-keys.test.ts packages/desktop/test/linux-appimage-launcher.test.ts`. **Commit:** `feat(desktop): one-time release key setup straight into the desktop-release environment`

### Task 12: Docs and runbook

**Files:** Create `packages/desktop/RELEASING.md` (packages/desktop has no docs dir). Modify `docs/user/desktop.md` (replace the "independent security review"/offline-signing wording with two sentences: the release is signed and published by a main-branch workflow after a green CI run; the signed record attests the CI run, not a human review) and the README "what works" line if it mentions manual signing (`rg -n "offline|security review" README.md docs/user/desktop.md`). No screenshots change.

`RELEASING.md` (short):
- **Release:** land code with the version bump in `packages/desktop/package.json`; wait for green `ci` on the newest code-touching commit of main (merge nothing else first: a later push cancels the earlier run, and a docs-only tip has no installers); `git tag desktop-v<version> <that commit> && git push origin desktop-v<version>`; watch `desktop release request`, then `desktop release` (`sign`, `publish`); check `desktop-v<version>` and the `desktop-beta`/`desktop-stable` assets.
- **What the workflow trusts:** the tag name is a hint; everything is re-derived from main; nothing from the tagged commit executes.
- **Failure handling:** a red `sign` leaves nothing public; fix on main, tag the new green commit; "Re-run failed jobs" on a red `publish` is idempotent.
- **Sandbox component freshness:** valid at most 93 days; the gate wants 30 left; refresh by bumping `generation` and dates (same keys), PR, then release.
- **Key rotation:** `release-keys.mjs setup --rotate`, commit the public pins, release. Metadata keys: `metadataPublicKeys` is a list; keep the old public key one release so installed clients can update, then drop it. Sparkle: clients pin one `SUPublicEDKey`; rotation needs a transition release signed with the old key that ships the new one (manual, announced). Sandbox: `publicKeys` accepts up to 8; add the new key, bump `generation`, release, drop the old key a release later.
- **Compromise response:** delete the environment and its secrets (`gh api -X DELETE repos/jonasthim/dilla/environments/desktop-release`), review who can merge to main, rotate as above, ship a release dropping the old keys, announce.
- **Residual risk:** copied from the threat model's "What remains".
- **Audit trail:** repository Deployments (environment `desktop-release`), the keyed Actions run, `SHA256SUMS` in its artifact.

- **Gate:** `npm run check:docs`; manual read-through. **Commit:** `docs(desktop): release and key-rotation runbook for the automated flow`

### Task 13: Final integration

- [ ] `npm run ci:local` clean (includes `check:ci`, `test:ci-check`, docs).
- [ ] One fixture test, `release-pipeline.test.mjs`: `gate (mock api, temp git repo with a main and a tagged commit) -> notes -> assemble -> signInJob (fresh keys) -> candidate-digest write -> verify -> publish plan`, `fetch` mocked; asserts the published asset list equals the signed inventory, that no file named `*private*` exists under the temp dirs afterwards, and that nothing was read from the tagged commit's working tree.
- [ ] Opus boundary-security review per "Review Focus".
- [ ] Push branch, PR, CI green, merge; delete the feature branch remotely and locally.
- **Commit:** `test(desktop): end-to-end fixture of gate, notes, assemble, sign, digest and publish plan`

## Rulings the plan made (not in Jonas's decisions)

1. **The attestation shape**: `source.review = { status:'automated', ciRun, tree, inventory:{sha256} }`, the digest covering the required-job inventory (names, conclusions, required steps). A new signed record format; `verifyBundle` accepts only it. *Cost if wrong:* Task 2 is redone if the record should carry more (for example the full job list); old `approved` bundles no longer verify (none exist).
2. **Notes use `compare` + `commits/{sha}/pulls`, PR number and title only**, capped at 250, with a fixed "First desktop release." for the first tag. *Cost if wrong:* a long gap between tags shows "and N more"; direct pushes to main (none, with protection) are not listed.
3. **`pull-requests: read`** on `sign` for the commit-to-PR API. *Cost if wrong:* if `contents: read` suffices it is one unnecessary read scope; dropping it also means editing the checker's pinned set.
4. **`setup` refuses an unprotected `main`** unless `--accept-unprotected-main`, since the design's whole key boundary is "only reviewed main code runs with the keys". *Cost if wrong:* an extra flag or protection step for Jonas before first use.
5. **The tag ruleset is optional** (`--tag-ruleset`, off by default); tags no longer reach the keys. *Cost if wrong:* anyone with write access can start a release of a green, higher-version main commit.
6. **The gate requires byte equality of the three public-pin files between the tagged commit and main's tip.** *Cost if wrong:* after a key rotation merges, an older green commit cannot be released; re-tag a newer commit.
7. **The gate adds rules beyond "tag on a green main commit":** version greater than all published of the channel and not already published; sandbox component with 30 days left; the triggering request run must be a successful push run of the request workflow for that same sha. *Cost if wrong:* a backport tag for an older line is refused.
8. **Drafts remain a transient state inside `publish`** (the only atomic way to attach assets to an immutable release); end state published; draft lookup fixed for reruns. *Cost if wrong:* a crash mid-publish leaves an invisible draft until rerun.
9. **`release-keys.mjs` already exists**; extended, with `create` and `normalize-sparkle` removed. *Cost if wrong:* none (no keys exist).
10. **Secret names** `DESKTOP_METADATA_PRIVATE_PEM`, `DESKTOP_SPARKLE_PRIVATE_KEY`, `DESKTOP_LINUX_SANDBOX_PRIVATE_PEM`. *Cost if wrong:* renaming touches setup, workflow and checker together.
11. **`check-release-workflow.mjs` and `release-workflow.test.ts` are deleted**; their rules move into `check-ci-workflow.mjs`. *Cost if wrong:* a missed rule; Task 10 was built from the old file line by line.
12. **The sign job still does no `npm ci`**, so no npm dependency code runs in the keyed process. *Cost if wrong:* a future signer dependency needs a new checker rule.
13. **`workflow_run` `branches: ['desktop-v*']` is assumed to match tag-push runs;** Task 9 confirms it and removes the filter and its pin if not. *Cost if wrong:* if unnoticed, the first tag starts no keyed run (fail-safe, visible, one edit to two files).
14. **The automation and the public pins ship in one PR** (fewer PRs). *Cost if wrong:* a larger review; splitting only requires the pins to merge before the tagged commit.
15. **Publisher strictness:** `source.commit === head === DESKTOP_SHA`; the tag is re-resolved through the API; `packages/desktop/releases/` no longer exists. *Cost if wrong:* none for new releases.

## Where Jonas's decisions cannot be met as stated

1. **"Signing job on tag push only"** is replaced, per his later decision, by a tag-push request workflow plus a `workflow_run`-triggered keyed workflow whose environment admits `main`, not `desktop-v*` tags. This is the intended change, not a conflict.
2. **A `workflow_run` workflow starts only if the request run completes**, and only from main's copy of the keyed file; the request workflow must therefore already be on main when the first tag is pushed (it is, in the same PR).
3. **"Zeroes buffers"** holds for Buffers only. V8 strings cannot be zeroed; the script builds PEM and base64 into Buffers to avoid private strings, but `gh`'s process memory and the OS pipe are outside its control.
4. **"A `ci` push run green on exactly that commit"** can be silently lost to `cancel-in-progress` when another commit lands on main; the gate fails loudly and the runbook says to tag while that run is the newest. Not fixable without changing `ci.yml` concurrency, which this plan does not touch.
5. **"Independent review"** no longer exists as a gate; the signed record attests CI, not a human (Jonas's decision). Development-time security reviews remain a process matter outside the release path.
6. **"Publish the releases, not drafts":** drafts remain transient inside `publish` (ruling 8).
7. **First-use integration cannot be tested locally.** GitHub's environment, secret and `workflow_run` behaviour is exercised for real only by the first tag; fixtures prove formats and flow. Every failure before `publish` leaves nothing public.
