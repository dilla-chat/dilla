# Automated desktop releases Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Pushing `desktop-v<version>` on a main commit whose `ci` push run is green signs the desktop release with fresh keys and publishes the versioned release and the channel feed with no human step, no commit to main by the workflow, and no private key ever reachable outside one tag-only, environment-scoped job.

**Architecture:** `desktop-release.yml` becomes two jobs. `sign` (environment `desktop-release`, `contents: read` + `actions: read`, holds the keys, installs nothing from npm) runs a gate (`release-gate.mjs`: tag, main ancestry, version, green `ci` push run on exactly that commit, inputs present), downloads that run's artifacts, derives `acceptance.json` (`release-assemble.mjs`), materialises the three keys from secrets into a 0700 dir inside one script (`release-sign-job.mjs`), calls the unchanged `signDesktopRelease`, deletes the keys, writes a `SHA256SUMS` digest, and uploads the signed output as an artifact. `publish` (no environment, no secrets, `contents: write` + `actions: read`) downloads that artifact, verifies the digest the sign job published as a job output, then runs the existing `publish-release.mjs plan/publish` verification (which re-verifies every signature against the public pins and re-downloads the installers from the CI run) and creates both releases. The three things a human used to assemble become: `acceptance.json` (derived from CI artifacts), `ci-run.json`/`ci-jobs.json` (fetched by the gate), and `release-notes.md` + `security-review.md` (committed per version under `packages/desktop/release-inputs/<version>/` in the tagged commit, bound by a tree digest). One-time key creation is `release-keys.mjs setup`.

**Tech stack:** Node 24 ESM (`.mjs`, repo scripts), `node:test`, `gh` CLI, GitHub Actions with SHA-pinned first-party actions only.

---

## Facts this plan rests on (verified in the repo at 29023b36)

- `signDesktopRelease` (`release-feed.mjs`) reads `acceptance.json`, `ci-run.json`, `ci-jobs.json`, installers, sandbox sidecars, `build-provenance-*.json`, `performance-*.json`, `installed-account-*.json`, `release-notes.md`, `security-review.md` from the artifacts dir and `metadata-private.pem` (Ed25519 PKCS8 PEM), `sparkle-private.key` (base64 of the 32-byte seed), `linux-sandbox-private.pem` (Ed25519 PEM) from the keys dir. It refuses keys that do not match `release-trust.json` / `linux-sandbox-component.json`, and verifies the whole candidate before and after signing. Its imports are `node:` built-ins and repo files only, so the sign job needs no `npm ci`.
- Nothing in the repo produces `acceptance.json`, `release-notes.md` or `security-review.md` today; a human wrote them. `acceptance.json` is fully derivable (`format`, `ciRun`, `commit`, `head`, `tree`, `builds`/`performance`/`installedAccounts` sha256 maps, `notes.sha256`, `review{status,tree,sha256}`); only notes and review are authored content.
- `verifyBundle` demands `source.review.tree === source.tree`, `run.name === 'ci'`, `run.conclusion === 'success'`, same-repository head, and the full list of named native/client/voice/package-acceptance jobs and steps. A docs-only commit skips the `desktop` job (`scripts/ci-areas.mjs`: `*.md` and `docs/**` set no area), so such a commit has a green run but no installers and the sign job fails closed. A file matching no rule (for example a `.json`) is "unknown" and forces every area, which the release-inputs manifest relies on.
- Installer artifact names: `desktop-installers-*`, `desktop-performance-*`, `desktop-installed-account-*` (retention: default 90 days). `ci.yml` uses `cancel-in-progress: true` per ref, so a later push to main cancels an earlier main run; a cancelled run is not green.
- Public pins of the three key halves: `packages/desktop/release-trust.json` (`metadataPublicKeys`, `sparklePublicKey`; also read by `electron-builder.cjs` into Sparkle `SUPublicEDKey`), `packages/desktop/src/update-keys.ts` (`RELEASE_PUBLIC_KEYS`, written by `release-keys.mjs import-public`), `packages/desktop/linux-sandbox-component.json` (`publicKeys`, `publishedAt`, `expiresAt` at most 93 days apart, embedded into the launcher at build). `test/linux-appimage-launcher.test.ts:60` hard-codes the current sandbox key in a negative assertion.
- `packages/desktop/scripts/release-keys.mjs` already exists with `create DIR` (writes private keys to disk), `normalize-sparkle` and `import-public`. `check-release-workflow.mjs` (+ `test/release-workflow.test.ts`) currently forbids `secrets.` anywhere in `desktop-release.yml`.
- Tag-created-by-`GITHUB_TOKEN` events do not start workflow runs; `release.yml` (tags `v*`) has a `contents: write` job; `desktop-beta`/`desktop-stable` feed tags do not match `desktop-v*`.
- The publisher creates each release as a draft, uploads, then PATCHes `draft: false`. `GET /releases/tags/{tag}` does not return drafts, so a rerun after a half-finished publish would create a second draft. This plan fixes that (Task 6).

## Global Constraints

- No private key (metadata, Sparkle, sandbox) is ever written to disk by `release-keys.mjs`, printed, logged, put in argv, or put in an environment variable of anything but the one sign step. Buffers that held key material are zeroed after use.
- The workflow never commits to, tags, or pushes any branch. It cannot, by permissions: neither job has `contents: write` while holding keys, and the publish job writes only releases.
- `desktop-release.yml` triggers on `push` of tags `desktop-v*` and nothing else. No `workflow_dispatch`, `pull_request*`, `workflow_run`, `workflow_call`, `schedule`, `repository_dispatch`.
- `secrets.DESKTOP_*` appear exactly three times, in the `env:` of one step of the `sign` job. No other workflow or job references them or `environment: desktop-release`.
- All actions are first-party (`actions/*`), pinned by full SHA with a version comment. The sign job uses `actions/checkout`, `actions/setup-node`, `actions/download-artifact`, `actions/upload-artifact` and nothing else; the keys exist only during the single script step, after every other action in the job except the final upload has run.
- Top-level `permissions: {}`; each job declares its own. `sign`: `contents: read`, `actions: read`. `publish`: `contents: write`, `actions: read`.
- `GITHUB_TOKEN` is passed explicitly only to steps that call the API; checkout uses `persist-credentials: false` in `sign`.
- Fresh keys only. Algorithms exactly as the scripts expect: metadata Ed25519 PKCS8 PEM, Sparkle Ed25519 32-byte seed base64, sandbox Ed25519 PKCS8 PEM with the raw 32-byte public key base64 in `publicKeys`. The three public keys must be pairwise different (`assertDedicatedKeys`).
- Every task: strict TDD, narrow per-task gate, then `npm run ci:local` before the final push. Per the repo convention, commits end with the Co-Authored-By and Claude-Session trailers.
- Do not alter `signDesktopRelease`, `verifyBundle` or `verifyDesktopSigningCandidate` semantics. The new code feeds them; it does not weaken them.

## Review Focus

Opus review, boundary-security level, in this order:
1. `release-sign-job.mjs` and the `sign` job: can any key byte reach disk outside the 0700 dir, an artifact, a log, argv, or a step other than the one named? Is deletion in a `finally` that also runs on signer failure? Does the uploaded artifact contain only the expected exact file set?
2. `release-gate.mjs`: every check fails closed; the gate reads only the checked-out tag commit and the GitHub API; no check can be satisfied by attacker-chosen bytes (tag message, branch names, PR titles).
3. The sign-to-publish handoff: digest computed over exactly the artifact bytes, compared in the other job, exact file-set check, no symlinks.
4. `release-keys.mjs setup`: secret transport (stdin only), in-memory discipline, partial-failure behaviour, environment policy actually being `desktop-v*` tags only, public pins written only after all three secrets are set.
5. `check-ci-workflow.mjs` additions: each assertion has a mutation test that proves it fires.
6. Honest wording of residual risk in `RELEASING.md` and in this plan's threat model.

## Threat model

Assets: the three private keys; the right to publish a signed feed that installed clients and Sparkle will accept; the integrity of what is signed (the exact CI-built installers).

Controls common to all rows: environment `desktop-release` is restricted to tags `desktop-v*` (so no branch, PR, `workflow_dispatch` or other-workflow job can read its secrets); secrets are environment-scoped, not repository-scoped; a repository tag ruleset on `desktop-v*` (creation, update, deletion, non-fast-forward restricted to repository admins); the signed content is fixed by the gate to a main commit with a green `ci` push run.

| Actor | What they can do | How the design limits it |
|---|---|---|
| **PR author** (fork or same-repo) | Run arbitrary code in PR CI; edit workflow files in their PR; try `pull_request_target`/`workflow_run` tricks. | Secrets live in an environment whose deployment policy admits only `desktop-v*` tag refs; a PR ref (`refs/pull/*`) is refused by GitHub before the job starts. `desktop-release.yml` has no PR trigger and the checker (Task 8) fails CI if one appears. A PR cannot create tags. Changing the workflow in a PR does nothing until merged to main, and main requires review. Installers signed are only those built by `ci` on main commits, never PR builds (gate requires `event=push`, `branch=main`). |
| **Compromised third-party action** | Run code in whatever job uses it with that job's token and env. | No third-party actions are allowed anywhere in `desktop-release.yml` (checker allow-list: four `actions/*` SHAs). Keys exist only inside one `node` process in one step, in a 0700 dir deleted in `finally`; `secrets.*` are referenced only by that step, so earlier actions never receive them in env. The step after signing is `actions/upload-artifact` over the output dir only. The publish job, which does use `actions/download-artifact` and an npm-installed publisher, holds no keys. Residual: a compromise of `actions/*` itself (checkout/setup-node/download run before the key step on the same runner and could leave a background process that watches `$RUNNER_TEMP`). Accepted; reduce by SHA pins, ephemeral hosted runners, and by keeping the sign job to four actions. |
| **Stolen `GITHUB_TOKEN` from another workflow** | Whatever that run's scopes allow, for the life of that run: read/write contents or releases, re-run or cancel runs, delete artifacts. | The token cannot read environment secrets (no API exposes them) and cannot create or edit an environment (needs admin, not a `GITHUB_TOKEN` scope). Tags created with `GITHUB_TOKEN` do not trigger `desktop-v*` runs. A `contents: write` token could create a draft or tamper with an unpublished draft release, but the publisher only trusts bytes it re-verifies: installers are re-downloaded from the green CI run and checked against hashes inside the metadata signed by keys the token never saw; draft assets are replaced, never trusted. A token with `actions: write` could delete CI artifacts (publish then fails closed) but cannot substitute them: artifact uploads belong to their own run and the build-provenance digests are signed. Immutable published versioned releases cannot have assets replaced (`standard-release-upload.mjs`). The feed release `desktop-beta` is mutable by a `contents: write` token: the assets it can place are unsigned lookalikes that clients reject (signature check against pinned keys), at worst a denial of updates until the next release re-uploads them. |
| **Push access to non-main branches** | Commit any code and workflow files to a branch; open PRs; create a branch ruleset-permitted tag if not restricted. | Branch pushes never reach the environment (tag policy). The one path to the keys is creating a `desktop-v*` tag: a tag on a branch commit runs *that commit's* workflow file, so it could exfiltrate. Controlled by (a) the tag ruleset limiting `desktop-v*` creation to repository admins, and (b) the gate refusing any tag whose commit is not an ancestor of `origin/main` or has no green `ci` push run (this protects against honest mistakes and against actors without ruleset bypass, but is itself code from the tagged commit, so alone it does not stop a malicious tagger). Residual and stated in the runbook: whoever can create `desktop-v*` tags can exfiltrate the keys by tagging a modified workflow. See "Where Jonas's decisions cannot be met" for the stronger alternative. |
| **Someone who can push tags** (admin bypass, or a stolen PAT/SSH key of one) | Trigger a release of any commit they can name; re-tag. | Tag on non-main or non-green commit: gate refuses, nothing is signed. Tag on an old but legitimate main commit: gate requires the version to be greater than every published `desktop-v*` of the channel and not already released, and requires committed release inputs for exactly that version, so an old commit can neither roll the feed back nor re-sign a published version. Moving or deleting a published tag is blocked by the ruleset. Malicious workflow edit in the tagged commit: exfiltration possible (the residual above); the damage is bounded by key rotation (runbook) because metadata keys are pinned in shipped clients as a list and Sparkle/sandbox keys rotate through generation bumps. Detection: every run of the environment shows in the Deployments tab and the Actions log. |

Residual risk summary (also in `RELEASING.md`): compromise of GitHub-owned actions; an admin-equivalent actor creating a tag with a modified workflow; GitHub itself. Independence of the human security review is not machine-checkable.

## End-to-end path for 0.1.0-beta.1 (after this plan's PR merges)

Precondition: this plan's implementation is merged to main. `packages/desktop/package.json` is already `0.1.0-beta.1`.

1. **Jonas, once:** `! node packages/desktop/scripts/release-keys.mjs setup` (needs `gh auth` with repo admin). It prints only fingerprints and progress. Effects: environment `desktop-release` exists with a tag-only policy `desktop-v*`; tag ruleset on `desktop-v*` exists; three secrets set from stdin; `release-trust.json`, `src/update-keys.ts`, `linux-sandbox-component.json` rewritten in the working tree. He commits those on a branch and opens the **public-key PR** (`chore(desktop): pin fresh release public keys`).
2. **Public-key PR merged** (squash). Its `ci` run builds installers that embed the new public keys; wait for green on that main commit (call it M1). This build is not released.
3. **Release-inputs PR** (a step Jonas's list did not include; see rulings). Claude: runs the independent completed-app security review on M1's tree, writes `release-notes.md` and `security-review.md` under `packages/desktop/release-inputs/0.1.0-beta.1/`, runs `node packages/desktop/scripts/release-assemble.mjs freeze 0.1.0-beta.1` which writes `inputs.json` (version, `reviewedTree` = tree of M1 with the `release-inputs/0.1.0-beta.1` directory removed, sha256 of both documents). Jonas reviews and merges the PR (call the result R). Because `inputs.json` matches no `ci-areas` rule, the `desktop` job runs for R. If anything else lands on main between M1 and R, `reviewedTree` no longer matches and the gate refuses; re-run `freeze` on the new tip and re-review the delta.
4. **Main's `ci` run on R is green** (`ci-ok`, with the desktop native/client/voice/package-acceptance jobs).
5. **Claude pushes the tag:** `git tag desktop-v0.1.0-beta.1 R && git push origin desktop-v0.1.0-beta.1`. Nothing else happens by hand: `sign` (gate, download, assemble, sign, digest, upload) then `publish` (digest check, plan, publish) run; the versioned release `desktop-v0.1.0-beta.1` and the `desktop-beta` feed appear published. A desktop release is separate from the server: it does not change `--desktop-downloads` and needs no CT 144 upgrade.
6. If a step fails before `publish`, nothing is public; fix and use "Re-run jobs" on the same run (same tag, same commit) or delete and recreate the tag only if the cause was in the tagged commit. After `publish` started, rerun is idempotent (Task 6).

## Tasks

Test files live in `packages/desktop/test/` and run with the package's existing `node --test` runner (confirm the exact command in Step 1 of Task 1 by reading `packages/desktop/package.json` and reuse it). Script tests that need `.ts` imports follow `release-publish.test.ts`.

### Task 1: `ci-run.mjs`, shared CI-run lookup and the green-run rule

**Files:**
- Create: `packages/desktop/scripts/ci-run.mjs`
- Create: `packages/desktop/test/ci-run.test.mjs`
- Modify: `packages/desktop/scripts/publish-release.mjs` (use the shared helpers instead of its inline `api`/jobs loop; no behaviour change)

**Interfaces (exact):**
```js
// All functions take an injected `api(path, options?) => Promise<json|null>` so tests need no network.
export const REPOSITORY = 'jonasthim/dilla';
export function createGithubApi({ token, fetchImpl = fetch }): api   // origin api.github.com, path must start /repos/jonasthim/dilla/, redirect:'error', 30 s timeout, 404 -> null only when options.optional404
export async function findGreenPushRun(api, sha): Promise<run>        // throws if none
export async function fetchCiJobs(api, runId): Promise<job[]>          // filter=latest, paginated 100
```
`findGreenPushRun` calls `GET /repos/jonasthim/dilla/actions/workflows/ci.yml/runs?head_sha=<sha>&event=push&branch=main&status=completed&per_page=100` and accepts a run only if all hold: `name === 'ci'`, `path === '.github/workflows/ci.yml'`, `event === 'push'`, `head_branch === 'main'`, `head_sha === sha`, `status === 'completed'`, `conclusion === 'success'`, `repository.full_name === head_repository.full_name === REPOSITORY`. Among accepted runs it returns the highest `run_number`. A newer run for the same sha that failed or was cancelled does not invalidate an older green one only if it is a different run id; a run's `run_attempt` is the latest attempt by API definition.

**Tests (names and assertions):**
- `findGreenPushRun returns the green push run on exactly that sha` - fixture list with one success; returns it.
- `findGreenPushRun refuses pull_request, cancelled, failure, in_progress and other-branch runs` - five fixtures, each alone throws `/no green ci push run/`.
- `findGreenPushRun refuses a run whose head_sha differs from the requested sha` - API returns a run for another sha (hostile response); throws.
- `findGreenPushRun refuses a fork head repository` - `head_repository.full_name = 'x/dilla'`; throws.
- `findGreenPushRun refuses a workflow named ci at another path` - `path: '.github/workflows/evil.yml'`; throws.
- `fetchCiJobs pages until a short page` - two pages (100, 3) yields 103 jobs and requests `page=1`, `page=2`.
- `createGithubApi rejects a path outside the repository` - `api('/user')` throws before fetch is called.
- `createGithubApi never follows redirects` - fetch mock records `redirect: 'error'`.

- [ ] Step 1: write the failing tests. Step 2: run `node --test packages/desktop/test/ci-run.test.mjs`, expect failures. Step 3: implement, move the inline helpers out of `publish-release.mjs`. Step 4: green, plus `node --test packages/desktop/test/release-publish.test.ts` still green.
- **Gate:** the two test files above.
- **Commit:** `refactor(desktop): shared CI-run lookup for release gate and publisher`

### Task 2: `release-inputs.mjs` and `release-assemble.mjs` (derive `acceptance.json`)

**Files:**
- Create: `packages/desktop/scripts/release-assemble.mjs`
- Create: `packages/desktop/test/release-assemble.test.mjs`
- Create (by the tool, not by hand, in Task 11): `packages/desktop/release-inputs/<version>/{inputs.json,release-notes.md,security-review.md}`

**The committed inputs.** `inputs.json`, exact shape, no extra keys:
```json
{ "format": 1, "version": "0.1.0-beta.1", "reviewedTree": "<40 hex>",
  "review": { "status": "approved", "sha256": "<64 hex>" }, "notes": { "sha256": "<64 hex>" } }
```
`reviewedTree` is the git tree of the release commit with the directory `packages/desktop/release-inputs/<version>/` removed. It does not depend on the contents of the inputs, so it can be computed before the inputs are written, and it survives squash merges (it names content, not a commit).

**Interfaces (exact):**
```js
export function reviewedTreeOf(commit, version, { git }): string
  // git = (args:string[], env?) => string. Implementation: temporary GIT_INDEX_FILE under os.tmpdir(),
  // `read-tree <commit>`, `rm -r --cached --ignore-unmatch -q -- packages/desktop/release-inputs/<version>`, `write-tree`; always removes the temp index.
export async function readInputs(directory, version): Promise<{ inputs, notes: Buffer, review: Buffer }>
  // strict key set; sha256 of notes/review files must equal inputs; version must equal; both documents 1..262144 bytes, regular files, not symlinks
export async function assembleAcceptance({ artifactsDirectory, run, inputsDirectory, version, commitTree, reviewedTree }): Promise<{ acceptance, files }>
  // acceptance = { format:1, ciRun: run.id, commit: run.head_sha, head: run.head_sha, tree: commitTree,
  //   review:{status:'approved', tree: commitTree, sha256}, notes:{sha256},
  //   builds:{[target]:{sha256}} for BUILD_TARGETS from build-provenance-<target>.json,
  //   performance:{[target]:{sha256}} and installedAccounts:{[target]:{sha256}} for TARGETS }
  // Throws if reviewedTree (computed) differs from inputs.reviewedTree, or any referenced file is missing / not a regular file.
// CLI: release-assemble.mjs assemble VERSION ARTIFACTS_DIR   (reads ci-run.json from ARTIFACTS_DIR; copies release-notes.md and security-review.md in; writes acceptance.json with flag 'wx')
//      release-assemble.mjs freeze VERSION                    (writes inputs.json for the working tree; refuses if notes/review missing or a dirty tree)
```
The assembler does not judge CI quality: `verifyDesktopSigningCandidate` inside the signer does, from `ci-jobs.json`.

**Tests:**
- `assembleAcceptance derives every digest from the downloaded artifacts` - fixture dir with all records; each map entry equals the sha256 of the file bytes; `ciRun`, `commit`, `head` from `run`.
- `assembleAcceptance output is accepted by verifyDesktopSigningCandidate` - reuse the `release-publish.test.ts` fixture builder (extract it to `test/support/release-fixture.mjs` in this task so both tests share it); the derived acceptance replaces the hand-built `source` and signing succeeds.
- `readInputs refuses a notes file that differs from inputs.json` - flip one byte; throws `/notes digest/`.
- `readInputs refuses extra keys, wrong format, wrong version and a symlinked document`.
- `assembleAcceptance refuses a stale reviewedTree` - computed tree differs; throws `/changed since the security review/`.
- `reviewedTreeOf ignores the inputs directory but nothing else` - temp git repo: tree identical before and after writing inputs, different after editing any other file.
- `assemble CLI never overwrites an existing acceptance.json` - second run fails with `EEXIST`.
- `freeze writes inputs.json whose digests match the files and refuses a dirty working tree`.

- [ ] Steps: failing tests; run `node --test packages/desktop/test/release-assemble.test.mjs`; implement; green; extend `release-publish.test.ts` import to the shared fixture and keep it green.
- **Gate:** `release-assemble.test.mjs`, `release-publish.test.ts`.
- **Commit:** `feat(desktop): derive acceptance.json from CI artifacts and committed release inputs`

### Task 3: `release-gate.mjs`

**Files:**
- Create: `packages/desktop/scripts/release-gate.mjs`
- Create: `packages/desktop/test/release-gate.test.mjs`

**Interface (exact):**
```js
export const TAG = /^desktop-v((0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?)$/;
export async function runGate({ env, git, api, readFile, outputDirectory }): Promise<{ run, version, channel, commit }>
// CLI: node release-gate.mjs OUTPUT_DIR   (env: GITHUB_*, GH_TOKEN). Writes OUTPUT_DIR/ci-run.json and OUTPUT_DIR/ci-jobs.json (flag 'wx'),
// appends run=, version=, channel=, commit= to $GITHUB_OUTPUT. Exit non-zero with one-line reason on any failure.
```
Checks, in this order, each fail-closed with a distinct message:
1. `GITHUB_ACTIONS==='true'`, `GITHUB_EVENT_NAME==='push'`, `GITHUB_REF_TYPE==='tag'`, `GITHUB_REPOSITORY==='jonasthim/dilla'`, `GITHUB_REF===refs/tags/${GITHUB_REF_NAME}`, name matches `TAG`.
2. `git rev-parse HEAD === GITHUB_SHA` (the checkout is the tagged commit) and is a 40-hex.
3. `git merge-base --is-ancestor GITHUB_SHA origin/main`.
4. `packages/desktop/package.json` `version` equals the tag version.
5. `findGreenPushRun(api, GITHUB_SHA)`; `fetchCiJobs`; write the two JSON files.
6. `packages/desktop/release-inputs/<version>/` holds exactly `inputs.json`, `release-notes.md`, `security-review.md`.
7. No published release `desktop-v<version>` exists (a draft is allowed: it is a rerun); the version is greater, by semver precedence, than every published `desktop-v*` release of the same channel (list `GET /releases?per_page=100`, paginated).
8. `linux-sandbox-component.json` `expiresAt` is at least 30 days after now (a fresh release must not ship a capsule about to expire; message names the file and the refresh command in `RELEASING.md`).

**Tests:**
- `gate passes for a main-ancestor tag with green run, inputs and matching version` - happy path returns `{run, version, channel, commit}` and writes both JSON files.
- `gate refuses a tag whose commit is not an ancestor of main` - `git` mock throws on `merge-base`.
- `gate refuses a tag ref that is a branch, a PR ref, or a non-desktop tag` - three env variants.
- `gate refuses a version that differs from package.json`.
- `gate refuses when the ci run is red, cancelled, a PR run, or for another sha` - delegates to Task 1; asserts the message surfaces.
- `gate refuses missing, extra or renamed release input files`.
- `gate refuses an already published version and allows a draft rerun`.
- `gate refuses a version not greater than the newest published release of the channel` - table: `0.1.0-beta.1` after `0.1.0-beta.2`, equal, and `0.1.0-beta.10` after `0.1.0-beta.9` passes (numeric, not lexical).
- `gate refuses a sandbox component expiring within 30 days`.
- `gate never reads the tag message or annotation` - env has an adversarial `GITHUB_EVENT_PATH` payload; the gate's result is unaffected (asserted by not mocking any read of it).

- [ ] Steps: failing tests, implement, green.
- **Gate:** `node --test packages/desktop/test/release-gate.test.mjs`.
- **Commit:** `feat(desktop): release gate for tag, main ancestry, version and green CI run`

### Task 4: `candidate-digest.mjs` (artifact handoff)

**Files:**
- Create: `packages/desktop/scripts/candidate-digest.mjs`
- Create: `packages/desktop/test/candidate-digest.test.mjs`

**Interface:**
```js
export const EXPECTED_FILES = (channel, version) => string[]   // publicReleaseRecords(channel) + `dilla-${version}-linux-x64.sandbox.sig`, sorted
export async function writeDigest(directory, { channel, version }): Promise<string>   // lists directory, requires exactly EXPECTED_FILES, regular non-symlink files, each <= 64 MiB;
                                                                                      // writes SHA256SUMS ("<sha256>  <name>\n", sorted) with flag 'wx'; returns sha256 hex of SHA256SUMS bytes
export async function verifyDigest(directory, expectedHex, { channel, version }): Promise<void>
// CLI: candidate-digest.mjs write DIR CHANNEL VERSION   -> prints digest=<hex> on stdout (public data) and appends to $GITHUB_OUTPUT if set
//      candidate-digest.mjs verify DIR CHANNEL VERSION DIGEST
```
`SHA256SUMS` is itself part of the artifact. `verifyDigest` fails if the file set is not exactly `EXPECTED_FILES` plus `SHA256SUMS`, if any entry mismatches, or if the hash of `SHA256SUMS` differs from `expectedHex`. Because `PUBLIC_ACCEPTANCE_RECORDS` includes `release-notes.md`, the notes ride along.

**Tests:** `digest round trip verifies`; `verify refuses one flipped byte in any file`; `verify refuses an extra file, a missing file and a symlink`; `verify refuses a substituted SHA256SUMS with a consistent set` (attacker rewrites both the sums and a file; the outer digest from the sign job catches it); `write refuses to overwrite SHA256SUMS`.

- **Gate:** `node --test packages/desktop/test/candidate-digest.test.mjs`.
- **Commit:** `feat(desktop): digest-checked handoff of the signed release between jobs`

### Task 5: `release-sign-job.mjs` (keys from secrets, sign, delete)

**Files:**
- Create: `packages/desktop/scripts/release-sign-job.mjs`
- Create: `packages/desktop/test/release-sign-job.test.mjs`

**Interface:**
```js
export const KEY_ENV = Object.freeze({
  DESKTOP_METADATA_PRIVATE_PEM: 'metadata-private.pem',
  DESKTOP_SPARKLE_PRIVATE_KEY: 'sparkle-private.key',
  DESKTOP_LINUX_SANDBOX_PRIVATE_PEM: 'linux-sandbox-private.pem' });
export async function signInJob({ env, version, artifactsDirectory, outputDirectory, tempRoot, sign = signDesktopRelease }): Promise<void>
// CLI: node release-sign-job.mjs VERSION ARTIFACTS_DIR OUTPUT_DIR   (tempRoot = $RUNNER_TEMP)
```
Behaviour: refuses unless `GITHUB_ACTIONS==='true'`, `GITHUB_REF==='refs/tags/desktop-v'+version`; refuses if any of the three variables is missing or empty; creates `mkdtemp(tempRoot + '/dilla-keys-')` and `chmod 0700`; writes each value with `{flag:'wx', mode:0o600}`; immediately `delete`s the three names from `process.env`; calls `sign({version, artifactsDirectory, keysDirectory, outputDirectory})`; in `finally`, for every key file overwrites with zeros of the same length, `fsync`, unlinks, then `rm` the directory; any cleanup failure throws and fails the job. Never logs a variable's value or length.

**Tests:**
- `signInJob refuses to run outside a matching tag ref`.
- `signInJob refuses a missing key variable and writes nothing`.
- `key files are 0600 in a 0700 directory under tempRoot and absent after success` - the injected `sign` captures the directory, asserts modes and contents equal the env values, then returns; afterwards the directory does not exist.
- `key files are gone after the signer throws` - injected `sign` rejects; the directory is gone and the original error is rethrown.
- `process.env no longer holds the key variables while signing` - asserted inside the injected `sign`.
- `an end-to-end sign with freshly generated test keys produces a verifiable bundle` - real `signDesktopRelease` with the shared fixture from Task 2 and trust pins swapped via the fixture's temp copy of `release-trust.json` (follow how `release-publish.test.ts` injects keys); output passes `candidate-digest verify` and `verifyDesktopBundle`.
- `no key material appears in captured stdout/stderr` - spy on `console.*` and `process.stdout.write`; no substring of any key value.

- **Gate:** `node --test packages/desktop/test/release-sign-job.test.mjs`.
- **Commit:** `feat(desktop): sign inside the release job from environment secrets, deleting keys after`

### Task 6: `publish-release.mjs` takes the candidate from the sign job and tolerates reruns

**Files:**
- Modify: `packages/desktop/scripts/publish-release.mjs`
- Modify: `packages/desktop/test/release-publish.test.ts`

**Changes (exact):**
- `candidate` = `resolve(process.env.DESKTOP_RELEASE_CANDIDATE)` (required, must be absolute under `$RUNNER_TEMP`) instead of `packages/desktop/releases/<version>`. Remove `packages/desktop/releases/` from the design; no release output is ever committed.
- Replace the `git diff --exit-code source.commit GITHUB_SHA -- . ':(exclude)releases/<v>'` by `release.source.commit === process.env.GITHUB_SHA` and `release.source.head === GITHUB_SHA` (the tagged commit is the accepted commit; there is no longer a commit after the accepted one).
- Keep `merge-base --is-ancestor GITHUB_SHA origin/main` (defence in depth; the sign job already did it).
- `findRelease(tag)`: if `GET /releases/tags/<tag>` is 404, also scan `GET /releases?per_page=100` pages for `draft === true && tag_name === tag` and return it (GitHub does not return drafts by tag).
- Releases stay draft-then-publish internally: a draft is the only way to attach assets to an immutable version atomically; the end state is published, never left draft. Keep `prerelease: channel==='beta'`.
- The `desktop-<channel>` feed is created the same way; unchanged ordering (`.sig`, metadata, appcast last after versioned assets).

**Tests (add to `release-publish.test.ts`):**
- `publisher refuses a tag commit that is not the accepted source commit`.
- `publisher takes the candidate from DESKTOP_RELEASE_CANDIDATE and refuses a relative or missing path`.
- `publisher finds an existing draft by tag and does not create a second release` - API mock returns 404 for `/releases/tags/..` and a draft in the list.
- `publisher publishes the versioned release before touching the feed` - ordered call log.
- Existing tests that relied on `packages/desktop/releases/<version>` are updated to the env variable.

- **Gate:** `node --test packages/desktop/test/release-publish.test.ts packages/desktop/test/ci-run.test.mjs`.
- **Commit:** `feat(desktop): publish the sign job's candidate, resume drafts, require tag == accepted commit`

### Task 7: `desktop-release.yml`

**Files:**
- Modify: `.github/workflows/desktop-release.yml` (full rewrite; action SHAs copied from the current file, `upload-artifact` SHA copied from `desktop.yml`)

**Exact structure:**
```yaml
name: desktop release
on:
  push:
    tags: ['desktop-v*']
concurrency:
  group: desktop-release
  cancel-in-progress: false
permissions: {}
jobs:
  sign:
    runs-on: ubuntu-24.04
    timeout-minutes: 45
    environment: desktop-release
    permissions:
      contents: read
      actions: read
    outputs:
      run: ${{ steps.gate.outputs.run }}
      version: ${{ steps.gate.outputs.version }}
      channel: ${{ steps.gate.outputs.channel }}
      digest: ${{ steps.digest.outputs.digest }}
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7
        with: { fetch-depth: 0, persist-credentials: false }
      - uses: actions/setup-node@949feb2413d6458794dcd2491c4babbbce0c15c1 # v7
        with: { node-version-file: .nvmrc }
      - name: Gate the tag, main ancestry, version and green ci run
        id: gate
        run: node packages/desktop/scripts/release-gate.mjs "$RUNNER_TEMP/candidate"
        env: { GH_TOKEN: '${{ github.token }}' }
      - name: Fetch the accepted run's artifacts
        uses: actions/download-artifact@9000827ccba6bdab643e8b6fd33ac0654aef8333 # v8
        with:
          github-token: ${{ github.token }}
          run-id: ${{ steps.gate.outputs.run }}
          repository: jonasthim/dilla
          pattern: desktop-{installers,performance,installed-account}-*
          merge-multiple: true
          path: ${{ runner.temp }}/candidate
      - name: Derive acceptance.json from the accepted run and committed inputs
        run: node packages/desktop/scripts/release-assemble.mjs assemble "${{ steps.gate.outputs.version }}" "$RUNNER_TEMP/candidate"
      - name: Sign (the only step that receives secrets)
        run: node packages/desktop/scripts/release-sign-job.mjs "${{ steps.gate.outputs.version }}" "$RUNNER_TEMP/candidate" "$RUNNER_TEMP/signed"
        env:
          DESKTOP_METADATA_PRIVATE_PEM: ${{ secrets.DESKTOP_METADATA_PRIVATE_PEM }}
          DESKTOP_SPARKLE_PRIVATE_KEY: ${{ secrets.DESKTOP_SPARKLE_PRIVATE_KEY }}
          DESKTOP_LINUX_SANDBOX_PRIVATE_PEM: ${{ secrets.DESKTOP_LINUX_SANDBOX_PRIVATE_PEM }}
      - name: Digest the signed output
        id: digest
        run: node packages/desktop/scripts/candidate-digest.mjs write "$RUNNER_TEMP/signed" "${{ steps.gate.outputs.channel }}" "${{ steps.gate.outputs.version }}"
      - uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7
        with: { name: desktop-signed-release, path: '${{ runner.temp }}/signed', if-no-files-found: error, retention-days: 3 }
  publish:
    needs: sign
    runs-on: ubuntu-24.04
    timeout-minutes: 30
    permissions:
      contents: write
      actions: read
    steps:
      - checkout (fetch-depth: 0), setup-node (as above)
      - uses: actions/download-artifact (name: desktop-signed-release, path: ${{ runner.temp }}/signed)
      - run: node packages/desktop/scripts/candidate-digest.mjs verify "$RUNNER_TEMP/signed" "${{ needs.sign.outputs.channel }}" "${{ needs.sign.outputs.version }}" "${{ needs.sign.outputs.digest }}"
      - run: node packages/desktop/scripts/publish-release.mjs plan   # env DESKTOP_RELEASE_CANDIDATE: ${{ runner.temp }}/signed
      - run: npm ci --ignore-scripts
      - uses: actions/download-artifact (pattern: desktop-installers-*, run-id: ${{ needs.sign.outputs.run }}, path: desktop-release-artifacts, merge-multiple: true)
      - run: node packages/desktop/scripts/publish-release.mjs publish   # env GH_TOKEN, DESKTOP_RELEASE_CANDIDATE
```
Expression values taken from `steps.gate.outputs.*` are validated by the gate before being written (regexes), but are still passed through `env:` in a final implementation rather than interpolated into `run:` (use `env: VERSION: ${{ ... }}` and `"$VERSION"`); the checker enforces no `${{` inside any `run:` line except `runner.temp`/`github.token` style paths. The `tags` filter, `environment` and `permissions` lines above are exactly what Task 8 pins.

**Verification without secrets:** nothing in this task can be executed end to end locally. The first real run is the rehearsal; its failure modes before `publish` leave nothing public. Run `actionlint` if present in `ci:local`, and the Task 8 checks.

- **Gate:** `node scripts/check-ci-workflow.mjs` (after Task 8), `npm run ci:local`.
- **Commit:** `ci(desktop): keyed sign job and keyless publish job on desktop-v tags`

### Task 8: `check-ci-workflow.mjs` extensions (and retire `check-release-workflow.mjs`)

**Files:**
- Modify: `scripts/check-ci-workflow.mjs`
- Modify: `scripts/check-ci-workflow.test.mjs`
- Delete: `packages/desktop/scripts/check-release-workflow.mjs`, `packages/desktop/test/release-workflow.test.ts` (its rules forbid `secrets.` and `contents: write` file-wide and would contradict the new design; every rule it enforced is carried over below)

**Interface:**
```js
export const DESKTOP_RELEASE_SECRETS = ['DESKTOP_METADATA_PRIVATE_PEM', 'DESKTOP_SPARKLE_PRIVATE_KEY', 'DESKTOP_LINUX_SANDBOX_PRIVATE_PEM'];
export const DESKTOP_RELEASE_ACTIONS = [ /* the four actions/* SHAs, exact */ ];
export function checkDesktopReleaseWorkflow(root): string[]   // problems; wired into the CLI at the bottom of the file next to checkDesktopWorkflow
```
Assertions (each yields a problem line prefixed `desktop-release.yml:`):
1. The whole `on:` block equals `push:\n    tags: ['desktop-v*']` and nothing else (no `branches`, `workflow_dispatch`, `pull_request`, `pull_request_target`, `workflow_run`, `workflow_call`, `schedule`, `repository_dispatch`, `merge_group`).
2. Top-level `permissions: {}`; jobs are exactly `sign` and `publish`.
3. `sign` has `environment: desktop-release`, permissions exactly `contents: read` and `actions: read` (so no `contents: write`, `id-token`, `packages`, `pull-requests`, anything else); `publish` has no `environment:`, permissions exactly `contents: write` and `actions: read`, and `needs: sign`.
4. `${{ secrets.` appears exactly three times in the file, with exactly the three `DESKTOP_RELEASE_SECRETS` names, all inside the single step named "Sign (the only step that receives secrets)" of `sign`; no `secrets: inherit`, no `toJSON(secrets)`, no `env:` at job or workflow level in `sign`.
5. Every `uses:` is in `DESKTOP_RELEASE_ACTIONS` (exact SHA); `sign` uses only checkout, setup-node, download-artifact, upload-artifact; checkout in `sign` has `persist-credentials: false`; no `continue-on-error`, `always()`, `|| true`, `if: ${{ false }}`.
6. `sign` contains the step order `gate` -> download -> assemble -> sign -> digest -> upload, and no step between the download and the sign step uses an action (only `run:`); `publish` runs `candidate-digest.mjs verify` before `publish-release.mjs plan`.
7. Neither job's `run:` lines contain `${{ steps.`/`needs.` interpolation (values come through `env:`).
8. No other file under `.github/workflows/` mentions `DESKTOP_`-prefixed secrets, `environment: desktop-release`, or triggers on a tag pattern that can match `desktop-v*` (`tags: ['*']`, `desktop-*`, `desktop-v*`) besides `desktop-release.yml`.
9. Existing jobs of `ci.yml` do not reference `desktop-release` or `release-sign-job.mjs`.
10. `release-keys.mjs` source (read as text) contains no `writeFile`/`createWriteStream` call whose first argument mentions `private` and sets the environment policy name `desktop-v*` exactly once (guards the one-time setup against regressing to disk writes).

**Tests (names, each mutates the real file text and asserts a problem, plus one that the real file is clean):**
- `desktop release workflow as committed has no problems`.
- `adding workflow_dispatch, pull_request, workflow_run, a branches filter or a second tag pattern is reported` (five mutations).
- `removing environment: desktop-release from sign is reported`; `adding an environment to publish is reported`.
- `adding contents: write to sign is reported`; `adding id-token: write to sign is reported`; `removing contents: write from publish is reported`.
- `a fourth secrets reference, a secrets reference outside the sign step, secrets: inherit and toJSON(secrets) are reported`.
- `an unpinned or unknown action, or a third-party action in sign, is reported`.
- `a step with an action between download and sign is reported`.
- `a ${{ steps.* }} interpolation in a run line is reported`.
- `another workflow referencing environment desktop-release is reported` (mutation of a copy of `ci.yml` text through the function's text-level helper).
- `release-keys.mjs writing a private key path is reported`.

- **Gate:** `node --test scripts/check-ci-workflow.test.mjs`; `node scripts/check-ci-workflow.mjs`.
- **Commit:** `ci: pin the desktop release workflow's trigger, environment, secrets and permissions`

### Task 9: `release-keys.mjs setup`

**Files:**
- Modify: `packages/desktop/scripts/release-keys.mjs` (the file exists; extended, see rulings)
- Modify: `packages/desktop/test/release-keys.test.ts`
- Modify: `packages/desktop/test/linux-appimage-launcher.test.ts` (line 60: read the sandbox key from `linux-sandbox-component.json` instead of the hard-coded `qQe0Z4...`)

**Removed:** the `create DIR` command (writes private keys to disk) and `normalize-sparkle` (legacy encoding; fresh keys are generated in the right format). `import-public` stays and now shares the writer below.

**Interfaces (exact):**
```js
export function generateKeyMaterial(): {
  metadata: { privatePem: Buffer, publicPem: string },          // PEM built into a Buffer from DER via encodeBase64Buffer, never a string
  sparkle:  { seedBase64: Buffer, publicBase64: string },        // seed + '\n' as Buffer
  sandbox:  { privatePem: Buffer, publicBase64: string },
  zero(): void }                                                 // fills every private Buffer with 0
export function encodeBase64Buffer(bytes: Buffer, { wrap }?: { wrap?: number }): Buffer   // standard base64, no string intermediates
export async function setupRelease({ repo = 'jonasthim/dilla', env = 'desktop-release', rotate = false, gh = defaultGh, now = new Date(), root }): Promise<{ fingerprints }>
// CLI: release-keys.mjs setup [--rotate] [--repo owner/name]
//      release-keys.mjs import-public PUBLIC_JSON      (unchanged behaviour)
```
`gh` is `{ api(args, jsonBody?) , secretSet(name, buffer) }`. `secretSet` spawns `gh secret set NAME --env desktop-release --repo owner/name` with `stdio: ['pipe','ignore','pipe']`, writes the buffer to stdin, ends it, collects stderr (scrubbed of nothing: it never contains the value), rejects on non-zero. Argv never contains key material; a test spies on `spawn`.

`setupRelease` order, aborting on the first error:
1. Preflight: `gh auth status` equivalent via `api('user')`; repo admin (`api('repos/{repo}')` `permissions.admin === true`); `git status --porcelain` for the three public files clean.
2. `PUT repos/{repo}/environments/{env}` with `{ "deployment_branch_policy": { "protected_branches": false, "custom_branch_policies": true }, "can_admins_bypass": false, "reviewers": [], "wait_timer": 0 }`; list `deployment-branch-policies`; delete every policy that is not `{name:'desktop-v*', type:'tag'}`; create that one if missing; re-`GET` and assert exactly one policy, type `tag`, name `desktop-v*`.
3. Repository ruleset `desktop-release tags` (target `tag`, include `refs/tags/desktop-v*`, rules `creation`, `update`, `deletion`, `non_fast_forward`, bypass actor repository role Admin `always`), created if no ruleset of that name exists, updated otherwise.
4. If any of the three secrets already exists in the environment (`gh secret list --env`) and not `rotate`, abort before generating anything: "refusing to replace release keys; pass --rotate".
5. `generateKeyMaterial()`; self-test: sign and verify a probe with each key against its derived public key; `assertDedicatedKeys` on the new public set; construct the new `linux-sandbox-component.json` (same `goVersion`, `helper`; `publicKeys: [sandboxPublic]`; `generation` unchanged on first setup and `+1` when `rotate`; `publishedAt = now` truncated to seconds, `expiresAt = now + 90 days`) and validate with `validateConfiguration`.
6. Set the three secrets (`DESKTOP_METADATA_PRIVATE_PEM`, `DESKTOP_SPARKLE_PRIVATE_KEY`, `DESKTOP_LINUX_SANDBOX_PRIVATE_PEM`) one by one from the Buffers, then `zero()` in a `finally` (also on failure).
7. Only after all three succeeded, write `release-trust.json`, `src/update-keys.ts` (via the shared `writePublicTrust`), and `linux-sandbox-component.json`.
8. Print fingerprints (sha256 of each public key DER), the three file names to commit, and "no private key was written to disk".
If step 6 fails midway, print which secret names were set and that the repo files were not touched; rerunning with `--rotate` is the recovery.

**Tests (all with an injected fake `gh`, no network, no spawn):**
- `generateKeyMaterial produces keys in the formats signDesktopRelease reads` - parse each: PEM loads with `createPrivateKey`, seed decodes to 32 bytes and round-trips through the JWK path used in `release-feed.mjs`, sandbox raw public is 32 bytes; all three public keys distinct.
- `encodeBase64Buffer equals Buffer.toString('base64') and never yields a string` - random lengths 0..300, with and without 64-column wrapping.
- `zero() clears every private Buffer` - all bytes 0 afterwards.
- `setupRelease sends each key only on stdin` - the fake `secretSet` records names and byte equality with the generated material; a `spawn` spy proves argv contains none of the secret bytes (checked against base64/PEM of each key).
- `setupRelease writes no private key to disk, stdout or stderr` - run in a temp root with `fs` watchers: only the three public files appear; console output contains none of the private key bytes.
- `setupRelease creates the environment with exactly one tag policy desktop-v*` - fake `gh` starts with a stale `main` branch policy and no environment; final state has exactly the tag policy, `can_admins_bypass:false`.
- `setupRelease fails if the policy read-back differs` (fake returns a branch policy).
- `setupRelease creates or updates the tag ruleset`.
- `setupRelease refuses to replace existing secrets without --rotate and generates nothing`.
- `setupRelease leaves the repository files untouched when a secret write fails` and reports which names were set.
- `setupRelease public output satisfies the verifier's pins` - after a run, `assertDedicatedKeys(component, trust)` passes, `update-keys.ts` contains exactly the PEM in `release-trust.json`, `electron-builder.cjs` loads the new `sparklePublicKey`.
- `release pins agree` (new, runs in normal CI against the committed files): `RELEASE_PUBLIC_KEYS` equals `release-trust.json.metadataPublicKeys`; sandbox key differs from both.
- `import-public still writes trust and update-keys and reads no private material` (existing behaviour, kept).

- **Gate:** `node --test packages/desktop/test/release-keys.test.ts packages/desktop/test/linux-appimage-launcher.test.ts`.
- **Commit:** `feat(desktop): one-time release key setup straight into the desktop-release environment`

### Task 10: Docs and runbook

**Files:**
- Create: `packages/desktop/RELEASING.md` (the one doc a maintainer needs; `packages/desktop/` has no docs dir)
- Modify: `docs/user/desktop.md` (replace the "offline signer" wording about publication with the automated flow in two sentences; keep it user-facing)
- Modify: `README.md` "what works" line for desktop updates, if it mentions manual signing (check with `rg -n "offline|sign" README.md`)
- Re-shoot nothing (no UI change).

`RELEASING.md` content (short):
- **Release:** (1) land the code and a version bump in `packages/desktop/package.json`; (2) independent security review of the main tip tree; (3) `release-assemble.mjs freeze <version>` after writing `release-notes.md` and `security-review.md`; PR, merge; (4) wait for green `ci` on that exact commit (do not merge anything else first; a later push cancels the earlier run); (5) `git tag desktop-v<version> <commit> && git push origin desktop-v<version>`; (6) watch `sign` then `publish`; (7) verify `https://github.com/jonasthim/dilla/releases/tag/desktop-v<version>` and `desktop-beta`/`desktop-stable` feed assets; tell the Homelab session.
- **Failure handling:** red `sign` leaves nothing public; fix the cause on main and move to a new commit, then delete and recreate the tag (admin only) or bump the version; rerun a red `publish` with "Re-run failed jobs".
- **Sandbox component freshness:** `linux-sandbox-component.json` is valid for at most 93 days; the gate wants 30 days left. Refresh: new `generation`, new dates, same keys, PR, then release.
- **Key rotation:** routine (yearly or on suspicion): `node packages/desktop/scripts/release-keys.mjs setup --rotate`, public-key PR, release. Metadata keys: `metadataPublicKeys` accepts a list; to avoid stranding installed clients keep the old public key in the list for one release and sign with the new one only after clients have updated (note: `signDesktopRelease` signs with one key and requires it in the list). Sparkle key: Sparkle clients pin one `SUPublicEDKey`; rotation requires a Developer-ID-signed transition release signed with the old key that ships the new key (document as a manual, announced procedure). Sandbox key: `publicKeys` accepts up to 8; add the new key, bump `generation`, release, then drop the old key a release later.
- **Compromise response:** delete the three environment secrets and the environment (`gh api -X DELETE`), disable the tag ruleset bypass, rotate as above, publish a release that removes the old public keys, announce.
- **Residual risk** paragraph copied from the threat model.
- **Reading the audit trail:** repository Deployments (environment `desktop-release`), Actions run, the `SHA256SUMS` in the run's artifact.

- **Gate:** `npm run check:docs` (the docs job's check) and a manual read-through.
- **Commit:** `docs(desktop): release and key-rotation runbook for the automated flow`

### Task 11: Final integration and release-readiness checks

**Files:** none new.

- [ ] `npm run ci:local` clean (includes `check:ci`, `test:ci-check`, docs).
- [ ] `node packages/desktop/scripts/release-gate.mjs` against a local fixture repo is covered by Task 3; additionally run the whole chain in one fixture test `release-pipeline.test.mjs`: `gate (mock api, temp git repo) -> assemble -> signInJob (fresh keys) -> candidate-digest write -> verify -> publish plan` with `fetch` mocked; asserts the published asset list equals the signed inventory (12 assets) and that no file named `*private*` exists anywhere under the temp dirs after the run.
- [ ] Opus boundary-security review per "Review Focus".
- [ ] Push branch, PR, CI green, merge. After merge, delete the feature branch remotely and locally per the standing branch-cleanup rule.
- **Commit:** `test(desktop): end-to-end fixture of gate, assemble, sign, digest and publish plan`

## Rulings the plan made (not in Jonas's decisions)

1. **Release inputs are committed per version** under `packages/desktop/release-inputs/<version>/` (`release-notes.md`, `security-review.md`, `inputs.json`) in the tagged commit, bound by `reviewedTree`. Alternatives (store in a release draft, or fetch from a PR comment) put authored, signed content outside reviewed history. *Cost if wrong:* one extra PR per release and a brittle exact-tree match (anything merged between review and tag forces a re-freeze and a delta review).
2. **`acceptance.json` is derived, not authored.** Quality evidence is the CI run's job/step conclusions, which `verifyBundle` already demands. *Cost if wrong:* no human attestation of measured performance beyond "CI passed", which is what the old flow also verified mechanically.
3. **The security review's independence is not machine-checkable.** The gate only requires a merged, digest-bound file. *Cost if wrong:* a rubber-stamp file ships; mitigation is review of the inputs PR.
4. **`review.tree` is set to the release commit's tree** (required by `verifyBundle`), while the true review claim is `reviewedTree` (that tree minus the inputs directory). *Cost if wrong:* the signed record slightly overstates what was reviewed (the inputs directory itself).
5. **A tag ruleset on `desktop-v*`** (creation/update/deletion/non-fast-forward limited to admins) is created by `setup`. *Cost if wrong:* if Claude's push credentials are not admin, tag pushes are rejected until Jonas pushes or adds a bypass actor.
6. **The gate adds rules beyond "tag on green main commit":** version must exceed every published release of the channel and must not already be published; sandbox component must have at least 30 days left. *Cost if wrong:* a legitimate backport tag for an older line is refused (remedy: temporarily relax by code change).
7. **Releases are created as drafts and published within the same run** (the only atomic way to attach assets to an immutable versioned release); the end state is published. `findRelease` is fixed to see drafts so reruns do not duplicate. *Cost if wrong:* a crash mid-publish leaves a visible draft (invisible to the public) until rerun.
8. **`release-keys.mjs` already exists**; it is extended, and its disk-writing `create` and `normalize-sparkle` commands are removed. *Cost if wrong:* anyone with a key directory made by `create` loses the helper (none exist; nothing has been released).
9. **Secret names** `DESKTOP_METADATA_PRIVATE_PEM`, `DESKTOP_SPARKLE_PRIVATE_KEY`, `DESKTOP_LINUX_SANDBOX_PRIVATE_PEM`; secrets are PEM/base64 text, assembled in Buffers. *Cost if wrong:* renaming needs a coordinated change in setup, workflow and checker.
10. **`check-release-workflow.mjs` and its test are deleted**; its rules are carried into `check-ci-workflow.mjs`. *Cost if wrong:* a missed rule; Task 8's list was built from the old file line by line.
11. **`setup` refuses to replace secrets without `--rotate`, bumps the sandbox `generation` only on rotate, and resets `publishedAt/expiresAt`.** *Cost if wrong:* generation 1 with new dates for a fresh start is harmless as nothing shipped.
12. **The sign job does no `npm ci`**, so no npm dependency code can run in the process that holds keys. *Cost if wrong:* if a future signer import needs a package, the checker (assertion 6 forbids actions, not npm) and review must catch an `npm ci` appearing before the sign step; add that to Task 8 as a failing mutation if it ever lands.
13. **First-release path gains a release-inputs PR** between the public-key PR and the tag, and tags the release-inputs merge commit, not the public-key merge commit. *Cost if wrong:* none functionally; one more CI cycle (about one desktop build) before beta.1.
14. **Publisher strictness:** `source.commit === head === GITHUB_SHA`; `packages/desktop/releases/` no longer exists. *Cost if wrong:* none for new releases; any old candidate directory flow is gone.

## Where Jonas's decisions cannot be met as stated

1. **"Keys never reachable from branch workflow_dispatch or other workflows" is met, but "tag-only environment policy" is not a real boundary against someone who can create `desktop-v*` tags.** A tag push runs the workflow file *from the tagged commit*, so anyone allowed to create the tag can tag a branch commit whose workflow prints the secrets; the environment policy checks only the ref name, and the gate lives in that same commit. The plan compensates with the admin-only tag ruleset (ruling 5) and states the residual honestly. The no-click design that closes it is a trusted-code trampoline: a tiny unprivileged `desktop-release-request.yml` on tag push, and the keyed workflow triggered by `workflow_run` (which always loads the workflow file from the default branch), with the environment restricted to branch `main` and the gate checking the tag commit as data. That departs from "signing job on tag push only" and "policy allows only `desktop-v*` tags", so it is not in this plan; cost of switching later is a rewrite of Task 7/8 and the setup policy (about a day).
2. **"Write a new `release-keys.mjs`":** the file exists with a disk-writing `create`. Extended in place (ruling 8).
3. **"Zeroes buffers" is achievable only for Buffers.** V8 strings cannot be zeroed; the script avoids creating strings of private material (custom Buffer base64 encoder), but `gh`'s own process memory and the OS pipe are outside its control.
4. **"The workflow avoids committing to main unless provably unavoidable":** it is avoidable; nothing commits. Inputs are committed by ordinary PRs before the tag, which is a process change from "commit OUTPUT to `releases/<version>/`".
5. **"A `ci` push run green on exactly that commit"** can be silently lost to `cancel-in-progress` when another commit lands on main. The tag must be pushed while the run's commit is still green; the runbook says so; the gate fails loudly otherwise. Not fixable without changing `ci.yml` concurrency, which this plan does not touch.
6. **Jonas's four-step path for 0.1.0-beta.1 omits the release-inputs PR** (notes, security review, `inputs.json`), without which the gate refuses. See "End-to-end path".
7. **"Publish the releases, not drafts":** drafts remain as a transient state inside the publish step (ruling 7).
8. **Fresh-key verification of the Sparkle and sandbox pins can only be exercised for real at the first release;** the fixture pipeline test proves the formats, not GitHub's secret and environment behaviour. The first tag is the integration test, and every failure before `publish` is harmless.
