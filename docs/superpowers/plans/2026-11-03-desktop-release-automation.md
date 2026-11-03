# Automated desktop releases Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Pushing `desktop-v<version>` on a main commit whose `ci` push run is green signs the desktop release with fresh keys and publishes the versioned release and the channel feed with no human step, no commit to main by any workflow, no per-release files, and no private key reachable by anything that does not run from `main`.

**Repository name:** everything below uses `dilla-chat/dilla` (the repository moved and went public). A separate rename PR, including the Go module and the hard-coded `jonasthim/dilla` URLs in `updater.ts`, `update.ts`, `linux-sandbox-cache.ts`, `electron-builder.cjs`, `release-feed.mjs`, `build-native-linux-packages.mjs`, `verify-release.mjs` and the test support files, merges **first**. This plan is written and reviewed against the post-rename tree; any remaining `jonasthim/dilla` found while implementing is a rename miss to fix in that PR, not here.

**Architecture:** Two workflows.
1. `desktop-release-request.yml` (name `desktop release request`) starts on a `desktop-v*` tag push. It is unprivileged: top-level `permissions: contents: read`, no environment, no secrets, no `uses:`, one job that echoes.
2. `desktop-release.yml` (the keyed workflow) is triggered by `workflow_run` on that workflow's completion (`types: [completed]`, no `branches:` filter). GitHub always runs the **default branch's** copy of a `workflow_run` workflow, so only main code holds keys. Its environment `desktop-release` admits deployments from branch `main` only, no tags. Six jobs as built (batch B, then fix round B): `gate` (keyless; `contents`, `actions` read) re-derives tag, commit, posture, green run and exact artifact set (ids and zip digests) and uploads its run records; `collect` (keyless; `contents`, `actions`, `pull-requests` read) is an early-failure check only, with no outputs and no uploads; `sign` (environment `desktop-release`; `contents`, `actions`, `pull-requests` read; **no download action**) fetches the ten CI artifact zips and the gate's record zip itself as opaque blobs through the REST API by the gate's ids, requires each sha256 to equal the gate's digest, reads the zips in-process (node: built-ins only), builds the candidate, derives notes and acceptance, signs, and uploads the signed output and the candidate; `publish` (`contents: write`, `actions: read`) takes only sign's two uploads, by id, each checked against sign's digest; `verify-published` (`contents: read`); `feed` (environment `github-pages`; `contents: read`, `pages: write`, `id-token: write`) deploys the channel feed to Pages. The original three-job description follows for the record:
   - `sign` (environment `desktop-release`; `contents: read`, `actions: read`, `pull-requests: read`; no `npm ci`) re-derives everything from the GitHub API and git objects (the tag name is an untrusted hint), verifies the repository posture, downloads the `ci` run's artifacts **by id into separate directories**, generates `release-notes.md`, derives `acceptance.json` with an automated attestation, loads the keys from secrets **into memory only**, signs, digests the output and uploads it.
   - `publish` (no environment, no secrets, `contents: write`, `actions: read`; no `npm ci`, no third-party code) verifies the digest, re-runs the publisher's verification, normalises or creates both releases and re-checks the result.
   - `verify-published` (keyless, `contents: read`) re-downloads what was published and checks it against the signed metadata.

Checkouts in the keyed workflow are of `main` (the `workflow_run` default) and never of the tag. The tagged commit is read only as git data (`git show <sha>:packages/desktop/package.json`, tree comparison). One-time key creation, staged rotation and promotion are `release-keys.mjs setup | rotate | rotate --promote | rotate --retire`.

**Tech stack:** Node 24 ESM (`.mjs`, repo scripts), `node:test`, `gh` CLI, GitHub Actions with SHA-pinned first-party actions only.

---

## Facts this plan rests on (verified in the repo at 29023b36 and by the security review's live reads on 2026-11-03)

- `signDesktopRelease` (`release-feed.mjs`) reads `acceptance.json`, `ci-run.json`, `ci-jobs.json`, installers, sandbox sidecars, `build-provenance-*.json`, `performance-*.json`, `installed-account-*.json`, `release-notes.md`, `security-review.md` from the artifacts dir and three private keys from a keys dir (`createPrivateKey` of a PEM; Sparkle: base64 32-byte seed turned into a JWK). It refuses keys not matching `release-trust.json` / `linux-sandbox-component.json` and verifies the candidate before and after signing. Its imports are `node:` built-ins and repo files only, so the sign job needs no `npm ci`.
- Nothing produces `acceptance.json`, `release-notes.md` or `security-review.md`; a human wrote them. `verify-release.mjs` line 93 requires `source.review.status === 'approved'` with a sha256; lines 95-96 read `security-review.md`; `PUBLIC_ACCEPTANCE_RECORDS` lists it. Nothing client-side validates `source.review`.
- `verifyBundle` also demands `run.name === 'ci'`, `run.conclusion === 'success'`, same-repository head and a fixed inventory of named jobs and steps. A docs-only commit skips the `desktop` job (`scripts/ci-areas.mjs`: `DOC_PATTERNS = docs/**, *.md, LICENSE*, DCO`), so its green run has no installers and signing fails closed.
- Installer artifacts: `desktop-installers-<target>`, `desktop-performance-<target>`, `desktop-installed-account-<target>` (default 90-day retention). `ci.yml` uses `cancel-in-progress: true` per ref.
- **Client verification today.** `src/update.ts` `verifyRelease` takes `publicKeys: readonly string[]` and accepts a signature valid under **any** listed key (`some`); `RELEASE_PUBLIC_KEYS` in `src/update-keys.ts` is that list. `src/linux-sandbox-release.ts` verifies the capsule signature against 1..8 raw keys (`component.publicKeys`, embedded in the launcher). **Sparkle is the exception:** `Info.plist` `SUPublicEDKey` is a single key (from `release-trust.json` `sparklePublicKey` via `electron-builder.cjs`), and `SparkleBridge.mm` checks it. So metadata and sandbox verification are already key-list capable; macOS Sparkle is not (Task 12 spikes what Sparkle accepts).
- The feed URLs are hard-coded in clients: `updater.ts` line 20 (`.../releases/download/desktop-<channel>/desktop-<channel>.json`) and `electron-builder.cjs` `SUFeedURL`. The versioned artifact URLs must be `.../releases/download/desktop-v<version>/<name>` (`update.ts` line 66).
- Public pins: `packages/desktop/release-trust.json`, `src/update-keys.ts`, `packages/desktop/linux-sandbox-component.json`. `test/linux-appimage-launcher.test.ts:60` hard-codes the current sandbox key in a negative assertion.
- `release-keys.mjs` exists with `create DIR` (writes private keys to disk), `normalize-sparkle`, `import-public`. `check-release-workflow.mjs` bans `secrets.` file-wide.
- **Repository posture read on 2026-11-03:** ruleset `main` requires a PR with 0 approvals, `ci-ok` bound to integration 15368, no force-push, no deletion, bypass actor RepositoryRole admin `always`; one collaborator (Jonas, admin); `default_workflow_permissions: read`, `can_approve_pull_request_reviews: false`; **immutable releases now enabled** on `dilla-chat/dilla` (Jonas, 2026-11-03).
- `GET /releases/tags/{tag}` omits drafts. GitHub masks only whole secret values; multi-line values mask poorly under debug logging.

## Global Constraints

- No private key is ever written to disk by `release-keys.mjs` or by the sign job, printed, logged, put in argv, or placed in an environment variable of anything but the one Sign step. Secrets are single-line base64 values. Buffers that held key material are zeroed. `delete process.env.X` does not scrub `/proc/<pid>/environ`; the plan relies on the ephemeral hosted runner, not on that.
- No workflow commits, tags or pushes. `sign` has no write scope; `publish` writes releases only.
- `desktop-release.yml` triggers only on `workflow_run` of `desktop release request`, `types: [completed]`, no `branches:`. The request workflow triggers only on `push` of tags `desktop-v*`. Neither has `workflow_dispatch`, `pull_request*`, `schedule`, `repository_dispatch`, `workflow_call`.
- `environment: desktop-release` exists only in the `sign` job of `desktop-release.yml`. `secrets.DESKTOP_*` appear exactly six times (three key kinds, slots A and B) in the `env:` of one step of that job.
- **`workflow_run` rules.** The keyed jobs never use `actions/checkout` with `ref:`, `repository:` or `path:`; never download artifacts of the triggering run; never execute anything read from the tagged commit; pass `github.event.workflow_run.*` only through the gate step's `env:` as untrusted hints.
- Every value written to `$GITHUB_OUTPUT` is validated by a strict regex first (tag, version, channel, 40-hex sha and tree, numeric run id, numeric artifact-id list, `previous`).
- All actions are first-party (`actions/*`), pinned by full SHA: six of them (`checkout`, `setup-node`, `download-artifact`, `upload-artifact`, `upload-pages-artifact`, `deploy-pages`), the last two only in the keyless `feed` job. The request workflow uses none. `setup-node` runs with `package-manager-cache: false` in the keyed workflow.
- Top-level `permissions` is `{}` in the keyed workflow and `contents: read` in the request workflow. `gate`: `contents: read`, `actions: read`. `collect`: `contents: read`, `actions: read`, `pull-requests: read`. `sign`: `contents: read`, `actions: read` (only to fetch the gate-digested artifact zips), `pull-requests: read`. `publish`: `contents: write`, `actions: read`. `verify-published`: `contents: read`. `feed`: `contents: read`, `pages: write`, `id-token: write`.
- Fresh keys only; algorithms as the scripts expect (metadata Ed25519, Sparkle Ed25519 32-byte seed, sandbox Ed25519); the public keys of different kinds must differ (`assertDedicatedKeys`).
- Acceptance, build-provenance, installed-account, performance, sandbox and Sparkle checks in `verifyBundle` stay as strict as today. Its only change is the attestation (Task 2).
- Release notes contain only PR numbers and PR titles, rendered as inert code spans.
- Every task: strict TDD, narrow per-task gate, `npm run ci:local` before the final push. Commits end with the Co-Authored-By and Claude-Session trailers.

## Review Focus

Opus review, boundary-security level:
1. The trampoline: nothing from the triggering run or the tagged commit is trusted or executed; every value is re-derived; Task 10 fails on each pitfall.
2. The sign job: keys exist only in memory; nothing but the named step receives them; exact output set; I1 (artifact substitution) closed by exact-set, by-id, separate-directory download.
3. The gate: fail-closed order; the main-equivalence rule; posture checks (immutable releases, rules, environment policy).
4. The attestation change and the inventory call.
5. The publisher: draft normalisation, exact asset-set check after publish, tag re-resolution after publish, no third-party code under the write token.
6. `release-keys.mjs`: stdin-only transport, concrete posture preflight, staged rotation, no repository-level `DESKTOP_*` secrets.
7. Key rotation tests against the real verifiers.
8. Honest residual-risk wording.

## Threat model

Assets: the private keys; the right to publish a feed that installed clients and Sparkle accept; the integrity of what is signed (the CI-built installers of a green main commit).

Controls common to all rows: secrets are environment-scoped to `desktop-release`, whose deployment policy admits only the ref `main`; the keyed workflow is `workflow_run`-triggered so GitHub loads it from main whatever a tag points at; the checker (Task 10) fails CI if any other workflow names the environment or the secrets; the gate re-verifies the posture (immutable releases, `main` rules, environment policy) before signing; the signed content is fixed by the gate to the current main commit with a green `ci` push run, from an exact, id-pinned artifact set.

| Actor | What they can do | How the design limits it |
|---|---|---|
| **PR author** (fork or same-repo) | Run code in PR CI, edit workflows in a PR, try `pull_request_target`/`workflow_run` tricks. | PR refs are not `main`; the environment refuses them. A `pull_request_target` run executes on the base ref but only for workflow files already on main, and none may name the environment (checker). A fork's push does not trigger the base repo's `workflow_run`. A PR cannot create tags. Signed installers come only from `ci` push runs on main. Edits to the keyed workflow take effect only after merge to main. |
| **Compromised action** | Run code in the job that uses it with that job's token and env. | No third-party actions in either release workflow (six `actions/*` SHAs, of which `sign` uses only `checkout`, `setup-node` and `upload-artifact`, and no `download-artifact`; none in the request workflow). Keys exist only inside one `node` process, in memory, during the Sign step; `secrets.*` is referenced only by that step, so earlier actions never receive it. Actions do run after the Sign step (`checkout`/`setup-node` post steps, `upload-artifact`): by then the process has exited and the memory is gone, on an ephemeral runner; a SIGKILLed Sign step is the only way keys could outlive it, and a runner is discarded anyway. `setup-node` does no npm cache restore or save in the keyed workflow. The publish job holds a write token but no keys and runs no npm-installed code. Residual: compromise of `actions/*` itself running earlier on the same runner (a watcher process). |
| **Another job in the green `ci` run (or a stolen `ACTIONS_RUNTIME_TOKEN`)** | Upload an extra or duplicate artifact into the accepted run, hoping the sign job merges it over a real installer. | The gate lists the run's artifacts and requires the exact expected name set (no extras, no duplicates, none expired); the sign job fetches each artifact by id as a zip blob that must match the gate's digest, reads only the expected entry names of that artifact, and writes each with exclusive create, so a collision or any file outside the expected installer and record names fails; `ci-run.json`/`ci-jobs.json` come from the gate's own record artifact under the gate's digest and record hashes, so a CI artifact cannot replace them. Residual: a job that can overwrite a real artifact **within that run before it completes** is a compromise of `ci` itself on main (reviewed code). |
| **A malicious CI artifact that escapes extraction** (writes `.git/hooks` or git config, a `node` on `PATH`, `$GITHUB_ENV`/`$GITHUB_PATH`, `$HOME` dotfiles) | Plant code that a later step of the same job runs; or, from a compromised `collect` job, hand `sign` a forged installer with consistently rewritten provenance. | (Fix round B, Jonas's decision of 2026-11-03.) No job that holds keys or a write token runs an action that extracts a zip another job could have produced. `sign` has no `download-artifact` step. Its first step fetches each of the ten CI artifact zips, and the gate's record zip, as an opaque blob from `GET /repos/dilla-chat/dilla/actions/artifacts/{id}/zip`. It uses the ids the gate output, follows one redirect to the Actions blob host only (without the token), and streams each blob to a file it names itself. It keeps a blob only if its sha256 equals the digest the gate recorded for that id before any untrusted step ran. It then reads each zip in-process with node: built-ins (`artifact-zip.mjs`; no npm package, no extraction tool). It accepts only the exact expected entry names and refuses absolute paths, `..`, separators, duplicates, symlinks, directories and special entries, encryption, zip64, unknown methods and oversized entries, with a bounded total. It writes each entry with an exclusive create under the name from its fixed list, and checks the size and CRC-32. `sign` then builds the candidate itself, checks the gate's record hashes, derives the notes and `acceptance.json`, and `signDesktopRelease` runs the full acceptance verification before and after signing. The installer bytes signed are therefore exactly those of the CI zips whose digests the gate fixed; a forged installer or rewritten provenance in any artifact fails the digest check (regression test `a forged installer with rewritten provenance (or a forged record) in any of the ten artifacts is refused`). `collect` is an early-failure check only: it has no outputs and uploads nothing, the checker refuses any `needs.collect` reference, and an escape in `collect` reaches neither `sign` nor `publish`. `publish` takes only `sign`'s two uploads, by the artifact ids `sign` reports (an id names immutable bytes), and checks them against `sign`'s digests. Residual: `download-artifact` still extracts in `publish` (no keys; a `contents: write` token) and `feed`, but only zips that `sign` produced; and `collect`'s raw-artifact extraction can still poison a main-scoped cache, as a CI job that plants artifacts already could. The release scripts run git with hooks, fsmonitor and system/global config disabled. |
| **Stolen `GITHUB_TOKEN` from another workflow** | Whatever that run's scopes allow, for the run's life: write contents or releases, re-run or cancel runs, delete artifacts. | It cannot read environment secrets or edit environments (admin). Tags it creates do not trigger workflows. **Immutable releases are enabled**: once a versioned release is published, its assets and tag cannot change, so a `contents: write` token cannot alter published installers or signed metadata (the gate refuses to sign if the setting is off, and `publish` asserts each published release reports itself immutable). Before publication it could tamper with a draft; the publisher adopts a draft only after normalising it (below) and re-checks the exact asset set after publishing. A `contents: write` token can still tamper with any release that is not immutable; the feed question is Ruling 14. `actions: write` can re-run an old keyed run: the gate refuses (main-equivalence rule), so an old run cannot re-use old keyed code. |
| **Push access to non-main branches** | Commit code and workflows to branches; open PRs; create tags. | Branch code never runs with the keys. A tag on a branch commit starts only the unprivileged request workflow; the keyed workflow refuses it (not an ancestor of `origin/main`, no green `ci` push run on main). |
| **Someone who can push tags** | Start a release run for a commit they name. | Tagged code is never executed with privileges, so no key theft. The gate releases only a commit that has **no non-doc difference from main's current HEAD** (see "Main-equivalence rule"), a green `ci` push run, a `package.json` version equal to the tag, and a version greater than every published release of the channel and not yet published. This blocks releasing an older commit ahead of its fix. What a tag-pusher can still do: release main's *current* code early (premature, not stale), or deny a release by tagging a wrong version first (the version then cannot be reused; recoverable by bumping). There is no `desktop-v*` tag ruleset (Jonas declined it). |
| **Any credential with admin rights, or that can merge to main** (Jonas's own `gh` token held by agent sessions, a GitHub App with contents and PR write) | Change `desktop-release.yml` or the environment and read the keys. | Not prevented. The `main` ruleset has 0 required approvals and an admin bypass of mode `always` (Jonas kept both). Plainly: **any credential that can merge to `main` can obtain the signing keys** through a one-commit workflow change. `setup` verifies and prints the concrete posture, the gate re-reads it before signing, and the runbook says so. Keep agent sessions off admin tokens where practical. |

**What remains:** (1) the admin/merge row above; (2) compromise of GitHub-owned actions or GitHub; (3) a tag-pusher can trigger or pre-empt a release of current main; (4) the automated attestation proves CI passed, not that a human reviewed; (5) `/proc/<pid>/environ` of the Sign step holds the secrets for its lifetime on an ephemeral runner.

### Main-equivalence rule (the gate's answer to "old commit ahead of its fix" and "re-run of old keyed code")

Let `doc(path)` be the repository's `DOC_PATTERNS` from `scripts/ci-areas.mjs` (`docs/**`, `*.md`, `LICENSE*`, `DCO`). The gate requires, after a full fetch:
- `git diff --quiet <taggedSha> origin/main -- . ':(exclude)<each doc pattern>'` (the tagged commit has no code difference from main's current HEAD), **and**
- `git diff --quiet HEAD origin/main -- . ':(exclude)<each doc pattern>'` (the checked-out keyed code is current main; a re-run of an old keyed run fails because its `HEAD` is old).
Rationale for choosing this over "tag must be HEAD": a docs-only merge after the code commit does not force a re-tag (that commit has no installers, so strict HEAD would make releases impossible after any docs merge), while any code, workflow or script change after the tagged commit blocks it. Cost: a code merge between the green run and the tag forces a wait for the next green run and a new tag.

## End-to-end path for 0.1.0-beta.1

Precondition: `packages/desktop/package.json` is `0.1.0-beta.1`; the rename PR and this automation are both on the branch to be merged.

1. **Jonas, once:** `! node packages/desktop/scripts/release-keys.mjs setup --reviewed-sha <sha>` from the automation branch checkout at the commit that passed the security review (`<sha>` is recorded in the PR; the script refuses if `HEAD` differs or the tree is dirty). Needs `gh auth` as repo admin. It prints the concrete posture and fingerprints only. Effects: environment `desktop-release` with a single `main` branch policy; the three `*_A` secrets set from stdin; the repository has no repository-level `DESKTOP_*` secret; `release-trust.json`, `src/update-keys.ts`, `linux-sandbox-component.json` rewritten in the working tree, which he commits.
2. **The PR (automation plus public pins) is merged.** It changes workflow and `.json` files, so the merge commit's `ci` run builds all desktop installers with the new public keys.
3. **Main's `ci` run on that merge commit is green.**
4. **Claude pushes the tag:** `git tag desktop-v0.1.0-beta.1 <merge commit> && git push origin desktop-v0.1.0-beta.1`. `desktop release request` completes; `desktop release` starts from main; `sign`, `publish`, `verify-published` run. A desktop release needs no CT 144 upgrade.
5. Failure before `publish` leaves nothing public. Recovery: fix on main and tag the new green commit, or start a **new request run** (re-run `desktop release request`), which starts a fresh `workflow_run` on current main. Re-running the keyed run itself fails the gate by design. After `publish` started, a new request run is idempotent (Task 8).

## Tasks

Tests live in `packages/desktop/test/` and use the package's existing `node --test` runner (confirm in Task 1). Script tests needing `.ts` imports follow `release-publish.test.ts`.

### Task 1: `ci-run.mjs`, API client, green-run rule, tag resolution, run artifacts

**Files:** Create `packages/desktop/scripts/ci-run.mjs`, `packages/desktop/test/ci-run.test.mjs`. Modify `publish-release.mjs` (use shared helpers).

**Interfaces:**
```js
export const REPOSITORY = 'dilla-chat/dilla';
export function createGithubApi({ token, fetchImpl = fetch }): api   // api.github.com only, path must start /repos/dilla-chat/dilla/, redirect:'error', 30 s timeout, 404 -> null only if optional404
export async function findGreenPushRun(api, sha): Promise<run>        // throws /no green ci push run/
export async function fetchCiJobs(api, runId): Promise<job[]>          // filter=latest, 100 per page
export async function resolveTagCommit(api, tag): Promise<string>      // git/ref/tags/<tag>; commit -> sha; tag -> git/tags/<sha> once, must be a commit
export async function listRunArtifacts(api, runId): Promise<artifact[]> // paginated; {id,name,digest,expired}
export function selectReleaseArtifacts(artifacts): { ids:number[], byName:Record<string,artifact> }
  // names matching /^desktop-(installers|performance|installed-account)-/ must equal EXACTLY EXPECTED_ARTIFACTS (derived from TARGETS/BUILD_TARGETS in verify-release.mjs; confirm the real names against desktop.yml in this task), no duplicates, none expired, each with a digest
```
`findGreenPushRun` queries `actions/workflows/ci.yml/runs?head_sha=<sha>&event=push&branch=main&status=completed&per_page=100`; accepts only `name==='ci'`, `path==='.github/workflows/ci.yml'`, `event==='push'`, `head_branch==='main'`, `head_sha===sha`, `status==='completed'`, `conclusion==='success'`, `repository.full_name === head_repository.full_name === REPOSITORY`; returns the highest `run_number`.

**Tests:** `findGreenPushRun returns the green push run on exactly that sha`; `findGreenPushRun refuses pull_request, cancelled, failure, in_progress and other-branch runs`; `findGreenPushRun refuses a run whose head_sha differs`; `findGreenPushRun refuses a fork head repository`; `findGreenPushRun refuses a workflow named ci at another path`; `fetchCiJobs pages until a short page`; `resolveTagCommit follows an annotated tag`; `resolveTagCommit refuses a tag pointing at a tree or blob`; `createGithubApi rejects a path outside the repository`; `createGithubApi never follows redirects`; `selectReleaseArtifacts accepts the exact set`; `selectReleaseArtifacts refuses an extra desktop-installers-zz artifact, a duplicate name, a missing artifact and an expired artifact`; `selectReleaseArtifacts ignores unrelated artifacts`.

- **Gate:** the two test files; `release-publish.test.ts` green. **Commit:** `refactor(desktop): shared CI-run, tag and artifact-set lookup for the release flow`

### Task 2: automated review attestation replaces the human review file

**Files:** Modify `packages/desktop/scripts/verify-release.mjs`, `packages/desktop/test/release-publish.test.ts`. (`release-feed.mjs` needs no change here.)

**Changes (exact, minimal):**
- Remove `'security-review.md'` from `PUBLIC_ACCEPTANCE_RECORDS`.
- New export `requiredJobInventory(jobs, version)`: array sorted by job `name`, each `{ name, conclusion, steps:[{name,conclusion}] }`, containing exactly the jobs `verifyBundle` requires and only their required steps by name; throws if any is missing or not `success`. The name lists are hoisted to module constants and shared with `verifyBundle`.
- Replace line 93 and lines 95-96: `source.review` has exactly the keys `status`, `ciRun`, `tree`, `inventory`; `status==='automated'`; `ciRun===source.ciRun`; `tree===source.tree`; `inventory.sha256 === sha256(JSON.stringify(requiredJobInventory(jobs, version)))`. Both the assembler and `verifyBundle` call it with `(jobs, version)` identically. The notes check (line 94) stays.
- `verifyBundle`'s repository check uses `dilla-chat/dilla`.

**Tests:** update the fixture (`source.review = {status:'automated', ciRun, tree, inventory:{sha256}}`, no `security-review.md`); keep all negatives. New: `publication refuses an attestation whose inventory digest does not match the run's jobs`; `publication refuses an attestation for another ci run id`; `publication refuses an attestation for another tree`; `publication refuses the old human review shape`; `requiredJobInventory is sorted, complete and covers every job verifyBundle requires` (delete each required job in turn; both throw); `requiredJobInventory ignores extra unrelated jobs`; `requiredJobInventory with and without the version argument matches between assemble and verify` (guards M9).

- **Gate:** `node --test packages/desktop/test/release-publish.test.ts`. **Commit:** `feat(desktop): automated CI attestation replaces the per-release security review file`

### Task 3: `release-notes.mjs`, generated notes as inert text

**Files:** Create `packages/desktop/scripts/release-notes.mjs`, `packages/desktop/test/release-notes.test.mjs`.

**Interfaces:**
```js
export function inertTitle(text: string, max = 200): string
  // strips control characters and newlines (to one space), collapses whitespace, truncates by code point to max (adding U+2026) with no entity ever split,
  // then wraps the result in a code span whose fence is one backtick longer than the longest backtick run in the text (with a single space of padding when the text starts or ends with a backtick).
  // Inside a code span GitHub renders no links, autolinks, mentions, issue references or html.
export function previousDesktopVersion(tagNames: string[], version: string): string | null
export async function generateNotes({ api, version, sha, previous }): Promise<string>
  // previous = tag name or null. compare/<previous>...<sha> (100/page), commits/<c>/pulls per commit, keep merged PRs with base.ref==='main'; read ONLY number and title.
  // "# Dilla desktop <version>\n\n" + lines "- #<n> <inertTitle(title)>\n" (n is a number we format; the leading #n is the only link); >250 PRs -> first 250 + "- and N more merged pull requests\n".
  // No previous tag: "# Dilla desktop <version>\n\nFirst desktop release.\n".
// CLI: release-notes.mjs generate VERSION SHA OUTPUT_FILE   (GH_TOKEN; tags via git/matching-refs/tags/desktop-v; output flag 'wx')
```
Titles are editable after merge by their authors; the inert rendering is what makes that harmless.

**Tests:** `inertTitle neutralises links, images, html, fences, headings, mentions, cross-repo references and e-mail autolinks` (inputs `[x](http://e)`, `![i](u)`, `<img src=x onerror=1>`, triple backticks, `# h`, `@octocat`, `owner/repo#1`, `a@b.co`, `&lt;`; each output is a single code span and parsing it with a CommonMark reference implementation yields one `code` node and no link/html nodes); `inertTitle handles a title containing backtick runs of every length`; `inertTitle flattens newlines and control characters`; `inertTitle truncation never splits an entity or a surrogate pair`; `generateNotes lists numbers and titles only and never reads a PR body` (Proxy-poisoned mock); `generateNotes skips unmerged PRs and PRs not targeting main`; `generateNotes dedupes a PR across commits`; `generateNotes caps at 250`; `previousDesktopVersion picks the numerically previous release (beta.10 after beta.9)`; `generateNotes for a first release names no PRs`; `the CLI refuses to overwrite its output`; and one environment-gated test `rendered notes contain no anchor except #n` that posts a hostile fixture to `POST /markdown` (mode `gfm`, context `dilla-chat/dilla`) and asserts every `<a` is a leading `#n` link (skipped without `GH_TOKEN`; run once by hand in Task 13).

- **Gate:** `node --test packages/desktop/test/release-notes.test.mjs`. **Commit:** `feat(desktop): generate release notes from merged PR titles as inert code spans`

### Task 4: `release-assemble.mjs collect | assemble`

**Files:** Create `packages/desktop/scripts/release-assemble.mjs`, `packages/desktop/test/release-assemble.test.mjs`, `packages/desktop/test/support/release-fixture.mjs` (extracted fixture builder shared with `release-publish.test.ts`).

**Interfaces:**
```js
export async function collectArtifacts({ downloadRoot, candidateDirectory, version }): Promise<void>
  // downloadRoot holds one subdirectory per artifact (no merge-multiple). Allowed files per artifact: exactly buildArtifactNames(target, version) + build-provenance-<target>.json for installers,
  // performance-<target>.json for performance, installed-account-<target>.json for installed-account. Copies into candidateDirectory with flag 'wx' (a collision between artifacts, or with an existing file, throws);
  // every file must be a regular, non-symlink file; any unexpected file name throws.
export async function assembleAcceptance({ candidateDirectory, run, jobs, version, commitTree }): Promise<{ acceptance }>
  // acceptance = { format:1, ciRun: run.id, commit: run.head_sha, head: run.head_sha, tree: commitTree,
  //   review:{ status:'automated', ciRun: run.id, tree: commitTree, inventory:{ sha256: sha256(JSON.stringify(requiredJobInventory(jobs, version))) } },
  //   notes:{ sha256 of release-notes.md }, builds / performance / installedAccounts digest maps }
// CLI: release-assemble.mjs collect VERSION DOWNLOAD_ROOT CANDIDATE_DIR GATE_DIR
//      release-assemble.mjs assemble VERSION CANDIDATE_DIR COMMIT_TREE
//      collect also copies GATE_DIR/ci-run.json and ci-jobs.json into CANDIDATE_DIR with flag 'wx' AFTER the artifacts, so an artifact carrying either name makes collect fail.
```
**Tests:** `collect refuses a file name collision across artifacts`; `collect refuses an artifact that carries ci-run.json or ci-jobs.json`; `collect refuses unexpected files and symlinks`; `collect never merges: the same file in two artifacts fails`; `assembleAcceptance derives every digest from the collected files`; `assembleAcceptance output is accepted by verifyDesktopSigningCandidate` (shared fixture); `assembleAcceptance refuses jobs lacking a required job or step`; `assemble CLI never overwrites acceptance.json`.

- **Gate:** `node --test packages/desktop/test/release-assemble.test.mjs packages/desktop/test/release-publish.test.ts`. **Commit:** `feat(desktop): collect CI artifacts without merging and derive acceptance.json`

### Task 5: `release-gate.mjs`

**Files:** Create `packages/desktop/scripts/release-gate.mjs`, `packages/desktop/test/release-gate.test.mjs`.

**Interface:**
```js
export const TAG = /^desktop-v((0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?)$/;
export async function runGate({ env, git, api, outputDirectory, now }): Promise<{ tag, version, channel, sha, tree, ciRun, artifactIds, previous }>
// CLI: node release-gate.mjs GATE_DIR. Env: GITHUB_*, GH_TOKEN, HINT_TAG (= workflow_run.head_branch), HINT_RUN (= workflow_run.id).
// Writes GATE_DIR/ci-run.json and ci-jobs.json (flag 'wx'). Appends to $GITHUB_OUTPUT ONLY after validating each value:
//   tag (TAG), version, channel (^(beta|stable)$), sha and tree (^[a-f0-9]{40}$), run (^[1-9]\d*$), artifact_ids (^[1-9]\d*(,[1-9]\d*)*$), previous (empty or TAG).
```
Checks, in order, each fail-closed with its own message (all `HINT_*` untrusted):
1. Running as the keyed workflow from main: `GITHUB_ACTIONS==='true'`, `GITHUB_EVENT_NAME==='workflow_run'`, `GITHUB_REPOSITORY==='dilla-chat/dilla'`, `GITHUB_REF==='refs/heads/main'`, `GITHUB_WORKFLOW_REF` ends with `.github/workflows/desktop-release.yml@refs/heads/main`, `git rev-parse HEAD === GITHUB_SHA`.
2. `HINT_TAG` matches `TAG`; `sha = resolveTagCommit(api, HINT_TAG)`.
3. The triggering run `GET actions/runs/<HINT_RUN>`: `path === '.github/workflows/desktop-release-request.yml'`, `event==='push'`, `head_sha === sha`, same-repository head, `conclusion==='success'`.
4. `git merge-base --is-ancestor <sha> origin/main`.
5. **Main-equivalence rule** (both diffs in the section above).
6. `git show <sha>:packages/desktop/package.json` parsed as data: `.version` equals the tag version. `tree = git rev-parse <sha>^{tree}`.
7. **Posture** (fail closed; reads only with the job's token): `GET repos/{o}/{r}/immutable-releases` has `enabled===true`; `GET repos/{o}/{r}/rules/branches/main` contains a `pull_request` rule, a `required_status_checks` rule with context `ci-ok` and `integration_id` 15368, a `non_fast_forward` rule and a `deletion` rule; `GET environments/desktop-release/deployment-branch-policies` is exactly one policy `{name:'main', type:'branch'}` and `deployment_branch_policy.custom_branch_policies===true` (if the token cannot read it, the first run reports the HTTP status and Task 9's note applies: this one check moves to `setup` only).
8. `findGreenPushRun(api, sha)`; `fetchCiJobs`; `listRunArtifacts` + `selectReleaseArtifacts` (exact set); write both JSON files.
9. No published release `desktop-v<version>` exists (a draft is allowed); version strictly greater than every published `desktop-v*` release of the channel. `previous` = `previousDesktopVersion(tags, version)`.
10. `linux-sandbox-component.json` (main's HEAD) `expiresAt` at least 30 days after `now`.
No code, script, workflow or document from the tagged commit is read except the `package.json` version string and the tree hash.

**Tests:** `gate passes for the current main with a green run, matching version, exact artifacts and good posture`; `gate refuses when not running as the main-branch workflow_run` (wrong event, ref, workflow ref, HEAD != GITHUB_SHA); `gate refuses a run whose workflow commit is no longer main's tip` (old HEAD with a code difference to origin/main; I7); `gate allows a docs-only main advance after the keyed run started`; `gate refuses a tagged commit with a code difference from main's HEAD (older commit ahead of its fix)`; `gate allows a tagged commit that differs from main's HEAD only in docs`; `gate resolves an annotated tag through the API ignoring the tag message`; `gate refuses a hint tag that is not a desktop version tag` (includes `desktop-v1.0.0; rm -rf`, `../x`); `gate ignores a lying HINT_RUN`; `gate refuses a triggering run from another workflow path, a fork, a non-push event or a failed run`; `gate refuses a commit that is not an ancestor of main`; `gate refuses a version that differs from the tagged package.json` (no checkout of the sha occurs); `gate refuses red, cancelled, PR and other-sha ci runs`; `gate refuses an extra or missing release artifact`; `gate refuses when immutable releases are disabled`; `gate refuses when the main ruleset lacks pull_request, ci-ok(15368), non_fast_forward or deletion`; `gate refuses when the environment policy is not exactly branch main`; `gate refuses an already published version and allows a draft rerun`; `gate refuses a non-greater version (beta.10 vs beta.9 numeric)`; `gate refuses a sandbox component expiring within 30 days`; `gate output values containing a newline or = are refused before any write` (M5).

- **Gate:** `node --test packages/desktop/test/release-gate.test.mjs`. **Commit:** `feat(desktop): release gate that re-derives tag, main equivalence, posture and artifacts`

### Task 6: `candidate-digest.mjs` (artifact handoff)

**Files:** Create `packages/desktop/scripts/candidate-digest.mjs`, `packages/desktop/test/candidate-digest.test.mjs`.

**Interface:**
```js
export const EXPECTED_FILES = (channel, version, { staged }) => string[]   // publicReleaseRecords(channel) + `dilla-${version}-linux-x64.sandbox.sig` (+ 'rotation-proof.json' when staged), sorted
export async function writeDigest(directory, { channel, version, staged }): Promise<string>   // exact set, regular non-symlink files, each <= 64 MiB; SHA256SUMS sorted, flag 'wx'; returns sha256 hex of SHA256SUMS
export async function verifyDigest(directory, expectedHex, { channel, version, staged }): Promise<void>
// CLI: write DIR CHANNEL VERSION STAGED -> appends digest=<64 hex> to $GITHUB_OUTPUT (validated); verify DIR CHANNEL VERSION STAGED DIGEST
```
**Tests:** `digest round trip verifies`; `verify refuses one flipped byte in any file`; `verify refuses an extra file, a missing file and a symlink`; `verify refuses a substituted SHA256SUMS with a consistent set`; `write refuses to overwrite SHA256SUMS`; `rotation-proof.json is expected only when staged`.

- **Gate:** `node --test packages/desktop/test/candidate-digest.test.mjs`. **Commit:** `feat(desktop): digest-checked handoff of the signed release between jobs`

### Task 7: keys stay in memory (`release-sign-job.mjs`, `signDesktopRelease` accepts key objects)

**Files:** Modify `packages/desktop/scripts/release-feed.mjs` (add an optional `keys` parameter; behaviour with `keysDirectory` unchanged), `packages/desktop/test/release-publish.test.ts`. Create `packages/desktop/scripts/release-sign-job.mjs`, `packages/desktop/test/release-sign-job.test.mjs`.

**`signDesktopRelease` change:** `signDesktopRelease({ version, artifactsDirectory, keysDirectory?, keys?, outputDirectory, now })`, where `keys = { metadata: KeyObject, sparkleSeed: Buffer, sandbox: KeyObject }`. Exactly one of `keysDirectory` / `keys` is required. Every check that compares a key to the pins is unchanged. With `keys`, nothing is read from or written to a keys directory.

**Interface:**
```js
// Secrets (single-line base64, never multi-line):
//   DESKTOP_METADATA_KEY_A / _B   base64 of the PKCS8 DER of the Ed25519 metadata key
//   DESKTOP_SPARKLE_KEY_A  / _B   base64 of the 32-byte Sparkle seed
//   DESKTOP_SANDBOX_KEY_A  / _B   base64 of the PKCS8 DER of the Ed25519 sandbox key
export function loadKeys(env, slot): { metadata: KeyObject, sparkleSeed: Buffer, sandbox: KeyObject, zero() }
export async function signInJob({ env, version, candidateDirectory, outputDirectory, trust, sign = signDesktopRelease }): Promise<{ staged: boolean }>
// CLI: node release-sign-job.mjs VERSION CANDIDATE_DIR OUTPUT_DIR
```
Behaviour: refuses unless `GITHUB_ACTIONS==='true'`, `GITHUB_REF==='refs/heads/main'`, `GITHUB_EVENT_NAME==='workflow_run'`. Reads `release-trust.json` `signing: { active: 'A'|'B', staged: null|'A'|'B' }` (active != staged). Loads the active slot's three secrets (all must be present, single-line base64, strict length/shape checks), builds `KeyObject`s with `createPrivateKey({key: der, format:'der', type:'pkcs8'})`, and `delete`s all six variable names from `process.env`. Calls `sign({ keys })`. If `staged` is set it also loads the staged slot and writes `rotation-proof.json` (metadata signature over the exact metadata bytes, sandbox signature over the sandbox manifest message, and an Ed25519 signature by the staged Sparkle seed over the mac archive), asserting each verifies under the **staged** public key from the trust files. Zeroes every Buffer in `finally`. Never logs a value, length or derived form.

**Tests:** `signInJob refuses to run outside the main-branch workflow_run`; `signInJob refuses a missing, multi-line or wrong-length key and signs nothing`; `signInJob signs with the active slot only when no slot is staged`; `signInJob with a staged slot signs with the active keys and writes a rotation proof that verifies under the staged public keys`; `signInJob refuses a staged slot whose public key is not in the pins`; `no key file is ever created` (fs spy over the temp roots); `process.env no longer holds the key variables while signing`; `all Buffers are zero after success and after a signer throw`; `an end-to-end sign with generated test keys passes verifyDesktopBundle and candidate-digest verify`; `no key material or derived form (PEM line, JWK d, seed) appears in captured stdout or stderr`; `signDesktopRelease with keys and with keysDirectory produce identical output`.

- **Gate:** `node --test packages/desktop/test/release-sign-job.test.mjs packages/desktop/test/release-publish.test.ts`. **Commit:** `feat(desktop): sign from in-memory environment keys with staged-rotation proof`

### Task 8: publisher: main-run, draft normalisation, exact asset sets, no third-party code

**Files:** Modify `packages/desktop/scripts/publish-release.mjs`, `packages/desktop/scripts/standard-release-upload.mjs` (replace the electron-builder `GitHubPublisher` use by direct uploads), `packages/desktop/test/release-publish.test.ts`. Create `packages/desktop/scripts/verify-published.mjs`, `packages/desktop/test/verify-published.test.mjs`.

**Changes (exact):**
- Run context: `GITHUB_EVENT_NAME==='workflow_run'`, `GITHUB_REF==='refs/heads/main'`; tag and commit from `DESKTOP_TAG` / `DESKTOP_SHA` (validated by the Task 5 regexes); the tag is re-resolved through `resolveTagCommit` and must equal `DESKTOP_SHA`; `merge-base --is-ancestor DESKTOP_SHA origin/main`; `release.source.commit === release.source.head === DESKTOP_SHA`.
- `candidate` = `DESKTOP_RELEASE_CANDIDATE` (absolute, under `$RUNNER_TEMP`).
- `findRelease(tag)`: 404 on `/releases/tags/<tag>` also scans `GET /releases?per_page=100` for drafts. **More than one draft for a tag: refuse.** A published (non-draft) release for the versioned tag: refuse.
- **Adopting a draft normalises it:** PATCH `name` (`Dilla desktop <version>`), `body` (the signed `release-notes.md`), `prerelease` (`channel==='beta'`), `target_commitish` (`DESKTOP_SHA`), `draft: true`; delete every asset not in the signed inventory plus public records; same for the feed release (delete assets outside the expected set; set body).
- Upload through the existing `api()` base and `fetch` to `uploads.github.com` (origin allow-listed, `redirect:'error'`, size and sha256 checks as before); **no electron-builder, no `npm ci` in `publish`**.
- **After `draft:false`:** re-read the release; assert the asset set equals the signed inventory plus records **exactly** (name, size, digest) and that the release reports itself immutable; re-resolve the tag and require `DESKTOP_SHA`; any mismatch fails the job loudly (M10).
- The channel feed release: see Ruling 14 and Task 8b.

**Tests:** `publisher refuses to run outside a main-branch workflow_run`; `publisher refuses a tag that resolves to another commit than DESKTOP_SHA`; `publisher refuses a signed source commit that is not DESKTOP_SHA`; `publisher refuses a DESKTOP_SHA that is not an ancestor of main`; `publisher takes the candidate from DESKTOP_RELEASE_CANDIDATE and refuses a relative or missing path`; `publisher normalises an adopted draft (body, name, prerelease, target, extra assets)`; `publisher refuses two drafts for one tag`; `publisher refuses a published release for the versioned tag`; `publisher fails when the published asset set differs from the signed set`; `publisher fails when the tag moved after publishing`; `publisher fails when the published release is not immutable`; `publisher publishes the versioned release before the feed` (ordered call log); `uploads go to uploads.github.com only and never follow redirects`; `publish job installs no npm dependencies` (the workflow text has no npm step in `publish`; also asserted by Task 10).
`verify-published.mjs` (`verify-published.test.mjs`): `verify-published passes for exactly the signed inventory`; `verify-published fails on an extra asset, a missing asset and a digest mismatch`; `verify-published checks the feed assets against the signed metadata`.

- **Gate:** `node --test packages/desktop/test/release-publish.test.ts packages/desktop/test/verify-published.test.mjs packages/desktop/test/ci-run.test.mjs`. **Commit:** `feat(desktop): publish from main with a normalised draft, exact asset sets and a post-publish check`

### Task 8b: channel feed under immutable releases (Jonas 2026-11-03: GitHub Pages)

With immutable releases on, a published `desktop-<channel>` release can no longer receive new assets, so today's model (one mutable feed release per channel, hard-coded in `updater.ts` and `SUFeedURL`) cannot work. See "Where Jonas's decisions cannot be met". Default plan if no other answer: serve the channel feed from GitHub Pages, deployed by the keyless `verify-published` successor job (`pages: write`, `id-token: write`, environment `github-pages`), with files `desktop-<channel>.json`, `.sig`, `appcast-<arch>.xml` copied from the verified signed output and the other channel's files re-fetched from the live site and re-verified against the pins before being re-served. Client changes: the feed URL in `updater.ts` and `SUFeedURL` in `electron-builder.cjs`; tests: updater feed URL, appcast URL, release-publish feed assertions. **Decided (Jonas, 2026-11-03): GitHub Pages, as described above. The repo is public, so Pages is available; the site URL is https://dilla-chat.github.io/dilla/. Enable Pages with source "GitHub Actions" in Task 9.** **Amended (fix round B, review I3):** the live site is never read before the deploy, only overwritten and then checked. The forward-only baseline is the channel's latest published immutable `desktop-v*` release: it must be this version, and its `desktop-<channel>.json`, `.sig` and `appcast-arm64.xml` assets must equal the signed output byte for byte. The other channel's files come from that channel's latest published immutable release, by asset digest, verified under the pins at the commit that release's tag names (`release-trust.json` there, on `main`). A key rotation (promote, retire) or a defaced live file therefore cannot block the next release's feed. The other channel's release must also have been made by the release workflow: its author is the Actions bot, and its signed `source.commit` is its tag's commit. Both are checkable with `contents: read`; matching the workflow run would need `actions: read`, which `feed` does not get. It is re-served only while every key that signed it (metadata, Sparkle, sandbox manifest) is still in main's current pins, active or staged. A release signed by a retired key is left off the site and never re-served; its channel's feed returns once that channel releases again.

### Task 9: the three-job workflows

**Files:** Create `.github/workflows/desktop-release-request.yml`. Rewrite `.github/workflows/desktop-release.yml`.

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
concurrency: { group: desktop-release, cancel-in-progress: false }
permissions: {}
jobs:
  sign:
    if: github.event.workflow_run.event == 'push' && github.event.workflow_run.conclusion == 'success'
    runs-on: ubuntu-24.04
    timeout-minutes: 45
    environment: desktop-release
    permissions: { contents: read, actions: read, pull-requests: read }
    outputs: { tag, sha, version, channel, run, artifact_ids, staged, digest }
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7
        with: { fetch-depth: 0, persist-credentials: false }      # no ref:, default = main tip
      - uses: actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1 # v7
        with: { node-version-file: .nvmrc, package-manager-cache: false }
      - name: Gate (re-derives tag, main equivalence, posture, green ci run and artifact set)
        id: gate
        run: node packages/desktop/scripts/release-gate.mjs "$RUNNER_TEMP/gate"
        env: { GH_TOKEN: '${{ github.token }}', HINT_TAG: '${{ github.event.workflow_run.head_branch }}', HINT_RUN: '${{ github.event.workflow_run.id }}' }
      - name: Fetch the accepted ci run's artifacts by id
        uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with: { github-token: '${{ github.token }}', run-id: '${{ steps.gate.outputs.run }}', repository: dilla-chat/dilla, artifact-ids: '${{ steps.gate.outputs.artifact_ids }}', path: '${{ runner.temp }}/downloaded' }
      - name: Collect artifacts without merging
        run: node packages/desktop/scripts/release-assemble.mjs collect "$VERSION" "$RUNNER_TEMP/downloaded" "$RUNNER_TEMP/candidate" "$RUNNER_TEMP/gate"
        env: { VERSION: '${{ steps.gate.outputs.version }}' }
      - name: Generate release notes from merged PR titles
        run: node packages/desktop/scripts/release-notes.mjs generate "$VERSION" "$SHA" "$RUNNER_TEMP/candidate/release-notes.md"
        env: { GH_TOKEN: '${{ github.token }}', VERSION: ..., SHA: ... }
      - name: Derive acceptance.json and the automated attestation
        run: node packages/desktop/scripts/release-assemble.mjs assemble "$VERSION" "$RUNNER_TEMP/candidate" "$TREE"
        env: { VERSION: ..., TREE: '${{ steps.gate.outputs.tree }}' }
      - name: Sign (the only step that receives secrets)
        id: sign
        run: node packages/desktop/scripts/release-sign-job.mjs "$VERSION" "$RUNNER_TEMP/candidate" "$RUNNER_TEMP/signed"
        env:
          VERSION: ${{ steps.gate.outputs.version }}
          DESKTOP_METADATA_KEY_A: ${{ secrets.DESKTOP_METADATA_KEY_A }}
          DESKTOP_SPARKLE_KEY_A: ${{ secrets.DESKTOP_SPARKLE_KEY_A }}
          DESKTOP_SANDBOX_KEY_A: ${{ secrets.DESKTOP_SANDBOX_KEY_A }}
          DESKTOP_METADATA_KEY_B: ${{ secrets.DESKTOP_METADATA_KEY_B }}
          DESKTOP_SPARKLE_KEY_B: ${{ secrets.DESKTOP_SPARKLE_KEY_B }}
          DESKTOP_SANDBOX_KEY_B: ${{ secrets.DESKTOP_SANDBOX_KEY_B }}
      - name: Digest the signed output
        id: digest
        run: node packages/desktop/scripts/candidate-digest.mjs write "$RUNNER_TEMP/signed" "$CHANNEL" "$VERSION" "$STAGED"
        env: { CHANNEL: ..., VERSION: ..., STAGED: '${{ steps.sign.outputs.staged }}' }
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with: { name: desktop-signed-release, path: '${{ runner.temp }}/signed', if-no-files-found: error, retention-days: 3 }
  publish:
    needs: sign
    runs-on: ubuntu-24.04
    timeout-minutes: 30
    permissions: { contents: write, actions: read }
    steps: checkout (fetch-depth 0, persist-credentials: false), setup-node (package-manager-cache: false);
      download-artifact name desktop-signed-release -> $RUNNER_TEMP/signed;
      run candidate-digest.mjs verify (env CHANNEL, VERSION, STAGED, DIGEST from needs.sign.outputs);
      download-artifact artifact-ids needs.sign.outputs.artifact_ids, run-id needs.sign.outputs.run -> $RUNNER_TEMP/downloaded;
      run release-assemble.mjs collect into $RUNNER_TEMP/installers (same exact-set rules);
      run publish-release.mjs plan, then publish (env GH_TOKEN, DESKTOP_TAG, DESKTOP_SHA, DESKTOP_RELEASE_CANDIDATE, DESKTOP_INSTALLERS)
  verify-published:
    needs: [sign, publish]
    runs-on: ubuntu-24.04
    permissions: { contents: read }
    steps: checkout (persist-credentials: false), setup-node; run verify-published.mjs (env GH_TOKEN, DESKTOP_TAG, DESKTOP_SHA)
```
All `${{ }}` values reach scripts through `env:`; no `${{` appears inside any `run:` line, block scalars included. The event-payload hints appear only in the gate step's `env:` and the job `if`.

Steps: (1) write both files; (2) run `actionlint` if `ci:local` carries it; (3) in a throwaway fork or by reading GitHub's docs, confirm the `workflow_run` default-branch semantics relied on; (4) check whether `GITHUB_TOKEN` can read the environment policy endpoint (gate check 7) and record the result in the PR.

**Verification limits:** the keyed path cannot be run locally; the first real tag is the rehearsal, and every failure before `publish` is harmless.

- **Gate:** `node scripts/check-ci-workflow.mjs` (after Task 10), `npm run ci:local`. **Commit:** `ci(desktop): unprivileged tag request, keyed sign, keyless publish and post-publish verify`

### Task 10: `check-ci-workflow.mjs` extensions (and retire `check-release-workflow.mjs`)

**Files:** Modify `scripts/check-ci-workflow.mjs`, `scripts/check-ci-workflow.test.mjs`. Delete `packages/desktop/scripts/check-release-workflow.mjs` and `packages/desktop/test/release-workflow.test.ts` (their file-wide `secrets.` ban contradicts the new shape; every rule is carried over).

**Interface:**
```js
export const DESKTOP_RELEASE_SECRETS = ['DESKTOP_METADATA_KEY_A','DESKTOP_SPARKLE_KEY_A','DESKTOP_SANDBOX_KEY_A','DESKTOP_METADATA_KEY_B','DESKTOP_SPARKLE_KEY_B','DESKTOP_SANDBOX_KEY_B'];
export const DESKTOP_RELEASE_ACTIONS = [ /* the four actions/* SHAs, exact */ ];
export function checkDesktopReleaseWorkflow(root): string[]   // wired into the CLI next to checkDesktopWorkflow
```
The check parses both files with a YAML parser (the repo's lockfile already carries `yaml`; if not, add it as a dev dependency of the root) instead of matching text, so mappings, flow style, expressions and block scalars cannot hide anything. Assertions:
1. **Request workflow.** `name: desktop release request`; `on` is exactly `{push:{tags:['desktop-v*']}}`; top-level `permissions` is exactly `{contents:'read'}`; one job `request`; no job-level `permissions`, `environment`, `env`, `secrets`, `defaults`, `container`, `services`, and no `uses` in any step; `runs-on: ubuntu-24.04`.
2. **Keyed trigger.** `on` is exactly `{workflow_run:{workflows:['desktop release request'], types:['completed']}}` (string-equal to the request workflow's `name`; **no `branches` key**).
3. **`sign.if`** equals the pinned string.
4. **Permissions.** Top-level `{}`; jobs exactly `sign`, `publish`, `verify-published`; permissions exactly as the Global Constraints; `publish` needs `sign` and has no `environment`; `verify-published` needs `[sign, publish]`.
5. **Environment confined (parsed).** The only `environment` value anywhere under `.github/workflows/` that names `desktop-release` (as string, mapping `name`, or any expression containing it) is `sign.environment` of `desktop-release.yml`; any `environment` that is an expression or mapping elsewhere is reported. `secrets[` indexing, `secrets: inherit`, `toJSON(secrets)` are forbidden in all workflows. The six `DESKTOP_RELEASE_SECRETS` appear in `${{ secrets.NAME }}` exactly once each, all in the `env` of the step named "Sign (the only step that receives secrets)", and that step's `env` keys are exactly `VERSION` plus those six.
6. **`sign` isolation.** Forbidden keys: `defaults`, `container`, `services`, `continue-on-error`, job- or workflow-level `env`; forbidden in any step: `shell`, `working-directory`; `runs-on` exactly `ubuntu-24.04`; no env key `NODE_OPTIONS` or `NODE_PATH` anywhere in the two files.
7. **workflow_run pitfalls.** No `ref`, `repository` or `path` key under `actions/checkout`; no `workflow_run.head_sha` or `.head_repository` anywhere; `workflow_run.head_branch`/`.id` only in the gate step's `env`; `workflow_run.event`/`.conclusion` only in `sign.if`; no `${{` inside any `run` value (block scalars included); `sign` checkout has `persist-credentials: false`, and so do the `publish` and `verify-published` checkouts.
8. **Artifacts.** In `sign` and `publish`, every `download-artifact` uses `artifact-ids` from `steps.gate.outputs.artifact_ids` / `needs.sign.outputs.artifact_ids` and `run-id` from `steps.gate.outputs.run` / `needs.sign.outputs.run`, and has neither `pattern` nor `merge-multiple`. The `upload-artifact` in `sign` has `path` exactly `${{ runner.temp }}/signed`.
9. **Actions.** Every `uses` is one of `DESKTOP_RELEASE_ACTIONS` (exact SHA); `sign` uses only checkout, setup-node, download-artifact, upload-artifact; every `setup-node` has `package-manager-cache: false`; no `always()`, `|| true`, `if: ${{ false }}`; no `npm` command in `publish` or `verify-published` or `sign`.
10. **Order.** In `sign`: gate, download, collect, notes, assemble, sign, digest, upload in that order, with only `run` steps between download and sign; `publish` runs `candidate-digest.mjs ... verify` before `publish-release.mjs ... plan`.
11. **Other workflows** reference neither the environment, `DESKTOP_` secrets, `release-sign-job.mjs`, nor `workflow_run` on `desktop release request`.
12. **`release-keys.mjs` and `release-sign-job.mjs` imports** are only `node:` modules and repo-relative files (parsed from the import statements); `release-keys.mjs` defines the environment policy as the single `{ name: 'main', type: 'branch' }` and contains no `type: 'tag'`.

**Tests (mutations of the real files; each asserts a problem; plus one clean run):** `desktop release workflows as committed have no problems`; `request workflow gaining secrets, an environment, contents: write, any uses or a checkout is reported`; `keyed workflow gaining push, pull_request, workflow_dispatch, a branches filter, a different upstream name or missing types is reported`; `changing the sign if is reported`; `removing environment from sign, an environment on publish or verify-published, environment as a mapping or expression elsewhere, or the environment named in another workflow is reported`; `any permission added to sign, contents: write missing from publish, contents: write on verify-published are reported`; `a seventh secrets reference, a secrets reference outside the Sign step, secrets[ indexing, secrets: inherit and toJSON(secrets) are reported`; `an extra or missing key in the Sign step env is reported`; `defaults, shell, working-directory, container, services, NODE_OPTIONS in sign are reported`; `checkout ref/repository/path or workflow_run.head_sha is reported`; `download-artifact with pattern, merge-multiple, the triggering run id or no artifact-ids is reported`; `workflow_run.head_branch outside the gate env is reported`; `a ${{ }} in a run line, including a block scalar, is reported`; `an unpinned or third-party action, an action between download and sign, setup-node without package-manager-cache false, or npm in publish is reported`; `upload path other than runner.temp/signed is reported`; `release-keys.mjs importing a non-node module or adding a tag policy is reported`.

- **Gate:** `node --test scripts/check-ci-workflow.test.mjs`; `node scripts/check-ci-workflow.mjs`. **Commit:** `ci: pin the desktop release trampoline, environment, secrets, permissions and artifact handling`

### Task 11: `release-keys.mjs` setup, staged rotation, promote, retire

**Files:** Modify `packages/desktop/scripts/release-keys.mjs`, `packages/desktop/test/release-keys.test.ts`, `packages/desktop/test/linux-appimage-launcher.test.ts` (line 60 reads the sandbox key from `linux-sandbox-component.json`).

**Removed:** `create DIR` and `normalize-sparkle`. `import-public` stays and shares `writePublicTrust`.

**Public state** (`release-trust.json`, additive; existing readers ignore extra keys): `metadataPublicKeys` (list: the active key, plus the staged key during a transition), `sparklePublicKey` (the **active** Sparkle key, used by the signer and `verifyBundle`), `sparkleNextPublicKey` (present only while staged; `electron-builder.cjs` writes `SUPublicEDKey = sparkleNextPublicKey ?? sparklePublicKey`; superseded by the Task 12 spike: `SUPublicEDKey` is always `sparklePublicKey`), `signing: { active: 'A'|'B', staged: null|'A'|'B' }`. `linux-sandbox-component.json` `publicKeys` lists the active key, plus the staged key during a transition (the existing 1..8 list).

**Secrets live in slots A and B** (names in Task 7). Secret values cannot be read back from GitHub, so "swap NEXT into current" cannot copy secrets; the committed `signing.active` pointer is flipped instead (Ruling 13).

**Commands:**
- `setup [--reviewed-sha SHA] [--accept-unprotected-main]`: first-time; writes slot A; `signing: {active:'A', staged:null}`.
- `rotate`: requires `signing.staged===null`; generates a new key set into the **inactive slot** (secrets only; the current slot is untouched); writes public files with the staged keys added to the lists (`sparkleNextPublicKey`, `signing.staged`); the sandbox `generation` is bumped. The resulting PR plus a release is the **transition release**: it is signed by the active keys, carries `rotation-proof.json` (the staged keys also signed the exact bytes), and the app built from this commit trusts both key sets, so installs move over by updating.
- `rotate --promote`: requires `signing.staged!==null` and a typed confirmation that the transition release has been out at least 60 days (the runbook's window); flips `signing.active` to the staged slot, clears `staged` and `sparkleNextPublicKey`, sets `sparklePublicKey` to the staged Sparkle key, and keeps the old metadata and sandbox keys in the lists for one more release (dropped by the next `rotate`).
- `rotate --retire`: after the promote PR is merged and released, deletes the **old** slot's three secrets.

**Interfaces:**
```js
export function generateKeyMaterial(): { metadata:{ derBase64: Buffer, publicPem: string }, sparkle:{ seedBase64: Buffer, publicBase64: string }, sandbox:{ derBase64: Buffer, publicBase64: string }, zero(): void }
export function encodeBase64Buffer(bytes: Buffer): Buffer          // standard base64 into a Buffer, single line, no string intermediate
export async function setupRelease(opts): Promise<{ fingerprints }>  // opts: repo='dilla-chat/dilla', env='desktop-release', reviewedSha, acceptUnprotectedMain, gh, now, root
export async function rotateRelease(opts): Promise<{ fingerprints }> // opts adds promote, retire, confirm
// CLI: release-keys.mjs setup|rotate [--promote|--retire] [--reviewed-sha SHA] [--repo owner/name] | import-public PUBLIC_JSON
```
`gh` = `{ api(method, path, jsonBody?), secretSet(name, buffer), secretDelete(name), secretNames(scope) }`; `secretSet` spawns `gh secret set NAME --env desktop-release --repo owner/name` with `stdio:['pipe','ignore','pipe']`, writes the Buffer to stdin; argv never holds key material.

`setup` order, aborting on the first error:
1. **Run-from-reviewed-code (M8):** `--reviewed-sha` must equal `git rev-parse HEAD`; tree clean; the script prints the SHA it runs from.
2. **Concrete posture preflight (I4), printed:** `GET user`; `GET repos/{repo}` `permissions.admin`; `GET repos/{repo}/rules/branches/main` has `pull_request`, `required_status_checks` with `ci-ok` and `integration_id` 15368, `non_fast_forward`, `deletion`; the ruleset is `enforcement: active`; list collaborators with push or higher and installed Apps for manual confirmation; `actions/permissions/workflow` `default_workflow_permissions==='read'` and `can_approve_pull_request_reviews===false`; **no repository-level or organisation-level secret whose name starts with `DESKTOP_`** (M7); immutable releases `enabled===true`; `main`'s bypass list printed as-is (Jonas keeps admin bypass `always`; this is a printed residual, not a refusal). A missing required rule aborts unless `--accept-unprotected-main`.
3. `PUT environments/{env}` with `{ deployment_branch_policy:{ protected_branches:false, custom_branch_policies:true }, can_admins_bypass:false, reviewers:[], wait_timer:0 }`; delete every policy other than `{name:'main', type:'branch'}`; create it if missing; re-GET and assert exactly it.
4. If any `DESKTOP_*` environment secret exists: abort before generating anything.
5. Generate; self-test (probe sign/verify with each key); `assertDedicatedKeys`; build the new `linux-sandbox-component.json` (same `goVersion`/`helper`; `publicKeys:[sandboxPublic]`; `generation` unchanged on first setup; `publishedAt` now to the second; `expiresAt` now + 90 days) and validate with `validateConfiguration`.
6. Set the three slot-A secrets from the Buffers; `zero()` in `finally`.
7. Only after all succeeded write `release-trust.json`, `src/update-keys.ts`, `linux-sandbox-component.json`.
8. Print fingerprints, the three files to commit and "no private key was written to disk".
If step 6 fails midway: print which names were set; repository files are untouched; recover by deleting those secrets and rerunning.

**Tests (injected fake `gh`, no network, no spawn):** `generateKeyMaterial produces keys in the formats the sign job reads (single-line base64)`; `encodeBase64Buffer equals Buffer.toString('base64') and never yields a string`; `zero() clears every private Buffer`; `setupRelease sends each key only on stdin` (spawn spy: argv has no key bytes); `setupRelease writes no private key to disk, stdout or stderr`; `setupRelease refuses a HEAD that differs from --reviewed-sha or a dirty tree`; `setupRelease aborts and prints the missing rule when pull_request, ci-ok(15368), non_fast_forward or deletion is absent`; `setupRelease aborts when a repository-level or organisation DESKTOP_ secret exists`; `setupRelease aborts when immutable releases are disabled`; `setupRelease prints collaborators, Apps and the bypass list`; `setupRelease leaves exactly one deployment policy, branch main, no tag policy`; `setupRelease fails if the policy read-back differs`; `setupRelease refuses when any DESKTOP_ environment secret already exists and generates nothing`; `setupRelease leaves repository files untouched when a secret write fails`; `setupRelease public output satisfies the verifier's pins`; `release pins agree` (runs in normal CI); `import-public still writes trust and update-keys`; **rotation:** `rotate writes only the inactive slot and leaves the active slot's secrets untouched`; `rotate adds the staged keys to metadataPublicKeys and the sandbox publicKeys, sets sparkleNextPublicKey and bumps the sandbox generation`; `rotate refuses while a rotation is already staged`; `rotate --promote flips signing.active, clears staged and sparkleNextPublicKey and requires the typed confirmation`; `rotate --promote refuses when nothing is staged`; `rotate --retire deletes exactly the old slot's three secrets and only after the promote state is committed`; `electron-builder.cjs uses sparkleNextPublicKey for SUPublicEDKey while staged and sparklePublicKey otherwise`.

- **Gate:** `node --test packages/desktop/test/release-keys.test.ts packages/desktop/test/linux-appimage-launcher.test.ts`. **Commit:** `feat(desktop): one-time key setup with posture checks and staged dual-key rotation`

### Task 12: rotation-capable client verification (beta.1 must ship with it)

**Files:** Modify `packages/desktop/electron-builder.cjs` (`SUPublicEDKey` rule above); extend `packages/desktop/test/update.test.ts` (or the existing `update` test file), `packages/desktop/test/linux-sandbox-release.test.*`, `packages/desktop/test/native/mac-upgrade.test.mjs`. No change to `verifyRelease` or `verifyLinuxSandboxRelease`: both already accept any listed key (`publicKeys.some`, `keys.some`), and `RELEASE_PUBLIC_KEYS` / `publicKeys` are lists.

**What each verifier accepts today and what rotation needs:**
- Metadata (`update.ts`): a list; a signature valid under any key is accepted. Transition: build ships `[active, staged]`; the transition release is signed by `active`, so pre-transition clients (list `[active]`) accept it; updated clients trust both. After promote, the release is signed by the staged key and updated clients accept it. A client that skipped the transition release (`[active]` only) rejects a staged-key-only release: that is why promote waits (60-day runbook window) and why the transition must be a normal release in the feed. Documented residual.
- Linux sandbox metadata: same list property (up to 8 keys, baked into the launcher from the component file).
- macOS Sparkle: **one** `SUPublicEDKey` per build. The transition build embeds the **staged** Sparkle public key while its archive and appcast are signed by the **active** key, so pre-transition apps verify it with the key they have, and after the update the app trusts the new key. Whether Sparkle 2 accepts an update whose bundle declares a different `SUPublicEDKey` than the installed one depends on its key-rotation rule (it requires continuity of the Apple code-signing identity). **First step of this task is a spike** against the vendored Sparkle version (read its `SUUpdateValidator` source and run the native mac upgrade test with a rotated key). If Sparkle refuses, the fallback is documented honestly: macOS installs need one manual reinstall at a Sparkle key rotation, and `rotate` prints that warning; metadata and sandbox rotation are unaffected.

**Tests:** `a transition release signed by the active key verifies for an old client (list [active]) and a new client (list [active, staged])`; `an updated client (list [active, staged]) accepts a release signed only by the staged key`; `a client that skipped the transition (list [active]) rejects a staged-key-only release`; `a signature by a key in neither list is rejected`; `sandbox capsule signed by the active key verifies under an old launcher list and under the transition list, and a staged-only capsule verifies only under the transition list`; `rotation-proof.json verifies under the staged public keys for the exact metadata, sandbox manifest and mac archive bytes`; mac native (`mac-upgrade.test.mjs`): `an app built with SUPublicEDKey=active updates to a transition app whose SUPublicEDKey=staged and archive is signed by active, and the transition app then installs an archive signed by staged` (or, if the spike says Sparkle refuses: `the same flow is refused and rotate prints the macOS reinstall warning`).

- **Gate:** the named tests. **Commit:** `feat(desktop): key-list client verification proven across a staged rotation`

### Task 13: Docs, runbook and final integration

**Files:** Create `packages/desktop/RELEASING.md`. Modify `docs/user/desktop.md` (replace the independent-review/offline-signing wording with two sentences: signed and published by a main-branch workflow after a green CI run; the signed record attests CI, not a human review) and the README "what works" line if it mentions manual signing (`rg -n "offline|security review" README.md docs/user/desktop.md`). Create `packages/desktop/test/release-pipeline.test.mjs`.

`RELEASING.md` (short):
- **Release:** land code with the version bump; wait for green `ci` on the newest code commit of main (a later code merge cancels the run and blocks an older tag via the main-equivalence rule; docs-only merges do not); `git tag desktop-v<version> <commit> && git push origin desktop-v<version>`; watch `desktop release request`, then `desktop release` (`sign`, `publish`, `verify-published`).
- **Recovery:** a red `sign` leaves nothing public; fix on main and tag again, or re-run the **request** workflow to start a fresh keyed run on current main. Re-running the keyed run itself fails the gate by design.
- **Who can release:** anyone who can push a tag can trigger a release of current main's code; they cannot release old code, and cannot touch the keys.
- **The honest sentence:** any credential that can merge to `main` can obtain the signing keys. The `main` ruleset requires 0 approvals and admin bypass is `always` (Jonas's decision); keep agent sessions off admin tokens where practical.
- **Posture the gate re-checks:** immutable releases on; `main` rules; environment policy; no repository-level `DESKTOP_*` secrets (setup checks; do not create any with `gh secret set` without `--env`).
- **Sandbox component freshness:** valid at most 93 days; the gate wants 30 left; refresh by bumping `generation` and dates, PR, then release.
- **Staged rotation:** `rotate` (stages slot B, PR, transition release signed by A and proving B); wait the 60-day window and confirm adoption; `rotate --promote` (PR, release signed by B); then `rotate --retire`. Caveats: clients that skipped the transition release need a reinstall; macOS depends on Task 12's Sparkle finding.
- **Compromise response:** delete the environment and its secrets (`gh api -X DELETE repos/dilla-chat/dilla/environments/desktop-release`), audit who can merge, `setup` fresh keys, ship a release with the new pins (installs signed only by a compromised key must reinstall), announce.
- **Audit trail:** repository Deployments (environment `desktop-release`), the keyed run, `SHA256SUMS`, `rotation-proof.json`.

`release-pipeline.test.mjs`: `gate (mock api, temp git repo with a main and a tagged commit) -> collect -> notes -> assemble -> signInJob (fresh keys) -> digest write/verify -> publish plan` with `fetch` mocked; asserts the published asset list equals the signed inventory, no file named `*private*` or containing key bytes exists under the temp dirs, and nothing was read from the tagged commit's working tree.

- [ ] `npm run ci:local` clean; run the environment-gated `POST /markdown` notes test once by hand.
- [ ] Opus boundary-security review (Review Focus), then push, PR, CI green, merge, delete the branch remotely and locally.
- **Gate:** `npm run check:docs`; `node --test packages/desktop/test/release-pipeline.test.mjs`. **Commit:** `docs(desktop): release, rotation and posture runbook; end-to-end pipeline fixture`

## Rulings the plan made (not in Jonas's decisions)

1. **Attestation shape** `{status:'automated', ciRun, tree, inventory:{sha256}}` over the required-job inventory. *Cost if wrong:* Task 2 redone if it should carry more; old `approved` bundles no longer verify (none exist).
2. **Notes as inert code spans** (`compare` + `commits/{sha}/pulls`, number and title only, 250 cap, fixed first-release line). *Cost if wrong:* titles render in monospace rather than as prose.
3. **`pull-requests: read`** on `sign`. *Cost if wrong:* one possibly unneeded read scope; dropping it also changes the checker.
4. **Main-equivalence rule** (no non-doc diff between the tagged commit and main's HEAD, and between the keyed checkout and main's HEAD), chosen over "tag must be HEAD". *Cost if wrong:* a docs-only commit that someone wants treated as code-relevant would slip through; a code merge between green run and tag forces a wait and a re-tag.
5. **The keys never touch disk** (in-memory `KeyObject`s via an optional `keys` parameter on `signDesktopRelease`), instead of Jonas's "0700 dir under `$RUNNER_TEMP`, delete after". Strictly stronger and removes the cleanup failure mode. *Cost if wrong:* if he wants the dir for auditability, Task 7 adds a thin writer.
6. **Gate re-reads posture** (immutable releases, `main` rules, environment policy) before signing. *Cost if wrong:* a token that cannot read the environment-policy endpoint makes that one check `setup`-only (Task 9 step 4).
7. **`setup` requires `--reviewed-sha`** equal to HEAD and prints the posture. *Cost if wrong:* an extra argument for Jonas.
8. **Secrets are single-line base64** (DER/seed), named `DESKTOP_{METADATA,SPARKLE,SANDBOX}_KEY_{A,B}`. *Cost if wrong:* renaming touches setup, workflow, signer and checker together.
9. **`check-release-workflow.mjs` and `release-workflow.test.ts` deleted**; the new checker parses YAML. *Cost if wrong:* a missed rule; Task 10 was built from the old file line by line; a YAML dev dependency may be added.
10. **Publisher uploads through plain `fetch`** to `uploads.github.com` and `publish` runs no `npm ci`. *Cost if wrong:* a re-implementation of what `electron-builder`'s publisher did (retry and 422 recovery) that must be tested by hand.
11. **Drafts remain a transient state in `publish`**, normalised when adopted, with an exact asset-set re-check and tag re-resolution after publishing. *Cost if wrong:* a crash mid-publish leaves an invisible draft until a new request run.
12. **A `verify-published` job** (keyless) re-downloads and checks what was published. *Cost if wrong:* a failure there cannot unpublish an immutable release; it alerts only.
13. **Staged rotation uses slots A and B with a committed `signing.active` pointer, not `_NEXT` secrets swapped into place**, because GitHub secrets are write-only (a "swap" cannot copy a value it cannot read). Same semantics: `rotate` stages, `--promote` flips, `--retire` cleans up. A 60-day window gates `--promote`. *Cost if wrong:* a client that skipped the transition release is stranded by a promote; the window is the mitigation.
14. **The feed is the unresolved immutable-releases conflict** (Task 8b); the default plan is GitHub Pages. *Cost if wrong:* if Jonas wants another host, Task 8b changes; client feed URLs change either way before beta.1.
15. **Release-time checks added:** version greater than all published of the channel, not already published, sandbox component at least 30 days from expiry, exact CI artifact set. *Cost if wrong:* a legitimate backport tag is refused.
16. **`setup` fails on an existing `DESKTOP_*` environment secret** and `rotate` is the only way to add a second slot. *Cost if wrong:* a half-failed setup needs manual secret deletion before a rerun.

## Where Jonas's decisions cannot be met as stated

1. **Immutable releases versus the mutable channel feed.** With immutability enabled, the published `desktop-beta`/`desktop-stable` feed release can no longer have its assets replaced, so the current feed model (`updater.ts` URL, Sparkle `SUFeedURL`, `publish-release.mjs` feed upload) cannot work. He must choose the feed host (default in Task 8b: GitHub Pages, deployed by a keyless job, with client URL changes before beta.1; alternatives: the dilla server serving the feed, or disabling immutability). Not decidable by the plan.
2. **"`rotate` swaps NEXT into current"**: GitHub secrets cannot be read back, so no swap is possible; slots with a committed active pointer give the same behaviour (Ruling 13).
3. **"Writes the keys into a 0700 dir under `$RUNNER_TEMP`"** (earlier decision): replaced by in-memory keys (Ruling 5).
4. **"Zeroes buffers"** holds for Buffers only; V8 strings and `gh`'s own memory cannot be zeroed, and `/proc/<pid>/environ` keeps the secrets for the Sign step's lifetime.
5. **Sparkle rotation (Task 12 spike, Sparkle 2.10.1 `SUUpdateValidator`):** an update may declare a new `SUPublicEDKey` when its archive verifies under the installed key, but every later archive must verify under the new key, so one key per build cannot span a multi-release staged window. Ruling: `SUPublicEDKey` stays the active key while staged (not `sparkleNextPublicKey ?? sparklePublicKey` as Task 11 first said) and changes at `--promote`; macOS installs older than the promote release reinstall once. `rotate` and `--promote` print this; RELEASING.md says so.
6. **`main` stays at 0 approvals with admin bypass `always`** (declined by Jonas), so "keys reachable only from reviewed main code" holds only up to "any credential that can merge to main". Stated plainly in the threat model and runbook.
7. **A tag-pusher cannot be stopped from triggering a release of current main** (no tag ruleset); the main-equivalence rule limits it to current code.
8. **"A `ci` push run green on exactly that commit"** can be lost to `cancel-in-progress` when another commit lands on main; the gate fails loudly. Not fixable without changing `ci.yml` concurrency.
9. **"Independent review"** is no longer a gate (his decision); the record attests CI only.
10. **First-use integration cannot be tested locally**: GitHub's environment, secret and `workflow_run` behaviour is exercised for real only by the first tag. Every failure before `publish` leaves nothing public.
