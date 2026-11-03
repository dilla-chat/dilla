# Releasing the desktop app

A desktop release is one tag push. Pushing `desktop-v<version>` on a `main` commit whose `ci` push run is green
starts `desktop release request` (unprivileged, it only echoes). Its completion starts `desktop release`, which
GitHub always loads from `main`: `gate → collect → sign → publish → verify-published → feed`. Nothing commits to
`main`, no file is written per release, and the signing keys exist only in the `desktop-release` environment,
which admits the branch `main` only.

## One-time setup

1. **Pages** (done 2026-11-03: `build_type: workflow`, and `github-pages` admits the branch `main` only). To check:
   `gh api repos/dilla-chat/dilla/pages --jq '{build_type, html_url}'` gives `workflow` and
   `https://dilla-chat.github.io/dilla/`. To enable it on a fresh repository:
   `gh api -X POST repos/dilla-chat/dilla/pages -f build_type=workflow` (or `-X PUT` to switch an existing site).
2. **Immutable releases** on, the `main` ruleset active (pull request, `ci-ok` from integration 15368, no force
   push, no deletion), and the default workflow token read-only. `setup` checks all of it.
3. **Keys**, once, by Jonas (repository admin, `gh auth status` signed in), from a clean checkout of the exact
   commit that passed the boundary-security review:

   ```
   ! node packages/desktop/scripts/release-keys.mjs setup --reviewed-sha <reviewed sha>
   ```

   It refuses unless `HEAD` is that commit and the tree is clean, then prints the live posture (ruleset and bypass
   list, collaborators, installed Apps, workflow token, repository- and organisation-level `DESKTOP_*` secrets,
   immutable releases, Pages) and aborts on any gap. It narrows `desktop-release` to the single branch policy
   `main`, refuses if any `DESKTOP_*` secret already exists there, generates the three keys in memory, sends each
   to `gh secret set --env desktop-release` on stdin (slot A), and only then rewrites the public pins:
   `release-trust.json` (with `signing: {active: 'A', staged: null}`), `src/update-keys.ts` and
   `linux-sandbox-component.json`. It prints fingerprints only; no private key is printed or written to disk.
   If a secret write fails midway it names the secrets it set and leaves the files untouched: delete those
   secrets and run it again.

Never create a `DESKTOP_*` secret with `gh secret set` without `--env desktop-release`: a repository-level secret
is readable by every branch workflow. `setup`, `rotate` and the gate all refuse while one exists.

## The 0.1.0-beta.1 path

1. Jonas runs `setup` as above, at the reviewed commit of the automation PR.
2. The automation PR merges. The three public files `setup` rewrote go to `main` in a follow-up PR (a release
   before it fails closed at `sign`: the pins do not match the keys).
3. `main`'s `ci` run on that follow-up merge commit is green (it builds every installer with the new pins).
4. Claude pushes the tag: `git tag desktop-v0.1.0-beta.1 <merge commit>` and
   `git push origin desktop-v0.1.0-beta.1`. Watch `desktop release request`, then `desktop release`.

A desktop release needs no CT 144 upgrade.

## Every release

- Land the code with the version bump in `packages/desktop/package.json`; wait for a green `ci` push run on the
  newest code commit of `main`.
- Tag that commit: `git tag desktop-v<version> <commit>` and `git push origin desktop-v<version>`.
- The gate releases only a commit with no code difference from `main`'s current tip (docs-only merges after it are
  fine; a later code merge cancels the older `ci` run and blocks the tag, so tag the new tip once it is green), a
  `package.json` version equal to the tag, and a version above every published release of its channel.

## Recovery

- **A red `gate`, `collect` or `sign`** leaves nothing public. Fix on `main` and tag the new green commit, or
  re-run the **request** run (`gh run rerun <request run id>`): that starts a fresh keyed run on current `main`.
  Re-running a keyed run itself fails the gate by design (its code is no longer `main`'s tip).
- **A red `publish`** after a draft was created: re-run the request run; the publisher adopts and normalises the
  draft (exact asset set, signed notes) and is idempotent.
- **A red `feed` after `publish`** (the versioned release is out, the Pages feed is not): re-run **only the failed
  `feed` job** of that keyed run (`gh run rerun <keyed run id> --failed`). It is keyless, uses the signed artifact
  (kept 3 days) and accepts a byte-identical re-deploy. A new request run cannot redo it: the gate refuses a
  version that is already published. After 3 days, release the next version instead. The feed is built from the
  published immutable releases only (this channel's newest must be this version; the other channel's newest is
  checked under the pins it was signed for), so a key rotation or a defaced Pages file never blocks it: the live
  site is simply overwritten.
- **A red `verify-published`** cannot unpublish an immutable release; treat it as an incident and investigate
  before the next tag.

## Who can release

Anyone who can push a tag can trigger a release of current `main`'s code, early, or block a version by tagging it
wrong first (bump the version). They cannot release older code and cannot reach the keys.

## Staged key rotation

1. `node packages/desktop/scripts/release-keys.mjs rotate --reviewed-sha <sha>` (same preflight as `setup`): new
   keys go into the inactive slot B only; slot A is untouched. The public files list both metadata and sandbox
   keys, set `sparkleNextPublicKey` and `signing.staged`, and bump the sandbox `generation`. Merge them, then
   release: this **transition release** is signed by A and carries `rotation-proof.json` (B signed the same
   metadata, sandbox manifest and macOS archive), and every app built from it trusts both key sets.
2. Wait at least 60 days and check that installs have moved to a transition build.
3. `rotate --promote` (typed confirmation `promote B`): B becomes active, A's keys stay pinned for one more
   release. Merge, release (signed by B).
4. After that release is out: `rotate --retire` (typed confirmation `retire A`) deletes slot A's three secrets,
   only once `origin/main` has B active with nothing staged.

Caveats: a Windows or Linux client that skipped the transition release rejects releases signed by B and must
reinstall. **macOS:** Sparkle 2 trusts one EdDSA key per build; an update may carry a new key only if its archive
verifies under the installed one, and every later archive must verify under the new one. A staged window of
several releases cannot satisfy that, so `SUPublicEDKey` stays on the active key while staged and changes at
`--promote`: macOS installs older than the promote release must be reinstalled once. `rotate` prints this.

## Sandbox component freshness

`linux-sandbox-component.json` is valid for at most 93 days and the gate wants 30 days left. Refresh it before:
bump `generation`, set `publishedAt` to now and `expiresAt` to at most 93 days later, open a PR, then release.

## Compromise response

1. Delete the environment and its secrets: `gh api -X DELETE repos/dilla-chat/dilla/environments/desktop-release`.
2. Audit who and what can merge to `main` (collaborators, Apps, tokens held by agent sessions).
3. Run `setup` again for fresh keys, merge the new pins, and release. Installs that trust only the compromised
   keys must reinstall; announce it.

## Audit trail

The repository's Deployments for `desktop-release`, the keyed run's logs, each release's `SHA256SUMS` and, for a
transition release, `rotation-proof.json`.

## Residual risks

- **Any credential that can merge to `main` can obtain the signing keys** with a one-commit workflow change. The
  `main` ruleset requires 0 approvals and the admin bypass is `always` (Jonas's decision). Keep agent sessions off
  admin tokens where practical.
- A compromise of GitHub or of the pinned `actions/*` actions (six, by SHA) running on the same runner.
- A tag-pusher can trigger or pre-empt a release of current `main`.
- The signed record attests that CI passed, not that a human reviewed the release.
- `/proc/<pid>/environ` of the Sign step holds the secrets for its lifetime, on an ephemeral runner; V8 strings and
  `gh`'s own memory cannot be zeroed.
- Key rotation strands clients that skipped the transition release, and every macOS install at `--promote`.
- First-use behaviour of the environment, secrets and `workflow_run` is only exercised by the first real tag; every
  failure before `publish` leaves nothing public.
