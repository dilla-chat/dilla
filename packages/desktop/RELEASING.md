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
- **One tag at a time.** The keyed workflow has no concurrency group, because GitHub would silently cancel a
  pending run when a third arrived. Instead every run starts, and its gate **fails loudly** ("Another desktop
  release run (id) is …") while another `desktop release` run is queued or in progress. Wait for that run, then
  re-run the refused release's request run (`gh run rerun <request run id>`). `sign` and `publish` repeat the
  check, so a tag pushed while another release is in `sign` or `publish` can fail both runs; re-run their request
  runs one at a time. Only the `feed` job is serialised (group `desktop-release-feed`), so two Pages deploys never
  overlap.

## Recovery

- **Never re-run `gate`, `sign` or `publish` in a keyed run** (no "Re-run all jobs", no "Re-run failed jobs", no
  `gh run rerun <keyed run id>` with or without `--failed` or `--job`, unless only `feed` failed). A partial re-run
  would reuse the old gate's outputs with that run's old code, so all three refuse any attempt but the first
  (`GITHUB_RUN_ATTEMPT` must be `1`), and `sign` and `publish` also repeat the gate's main-tip and one-run checks.
  The recovery is always a fresh keyed run: re-run the **request** run, or tag again.
- **A red `gate`, `collect` or `sign`** leaves nothing public. Fix on `main` and tag the new green commit, or
  re-run the **request** run (`gh run rerun <request run id>`): that starts a fresh keyed run on current `main`.
- **A red `gate` on the first tag** (a posture read, a first-use assumption about `workflow_run`, the environment or
  the secrets): nothing is public and the version is not published, but the tagged commit is no longer `main`'s
  tip once the fix lands. Fix on `main`, wait for its green `ci` push run, delete the unpublished tag
  (`git push origin :refs/tags/desktop-v<version>` and `git tag -d desktop-v<version>`), then tag the new green
  commit with the same version. A posture read the job token cannot do reports "Cannot read … with the job token":
  that is a permission problem of the read, not a setting that is off; check the setting by hand
  (`gh api repos/dilla-chat/dilla/…` as admin) before changing anything.
- **A red `publish`** after a draft was created: re-run the request run; the publisher adopts and normalises the
  draft (exact asset set, signed notes) and is idempotent.
- **A red `feed` after `publish`** (the versioned release is out, the Pages feed is not): re-run **only the failed
  `feed` job** of that keyed run (`gh run rerun <keyed run id> --failed`, only when `feed` is the one failed job).
  It is keyless, uses the signed artifact (kept 3 days) and accepts a byte-identical re-deploy. A new request run
  cannot redo it: the gate refuses a version that is already published. After 3 days, release the next version
  instead. The feed is built from the published immutable releases only (this channel's newest must be this version; the other channel's newest is
  checked under the pins it was signed for), so a key rotation or a defaced Pages file never blocks it: the live
  site is simply overwritten.
- **A red `verify-published`** cannot unpublish an immutable release; treat it as an incident and investigate
  before the next tag.
- **A red `publish` after `draft: false`, or a red `verify-published`, with an asset mismatch:** a
  `contents: write` token swapped a same-named asset between the last pre-publish check and publication, and the
  release is now immutable. The updater is safe: it checks every installer against the sha512 in the signed
  metadata and rejects the swapped file. People who download from the release page are not. Respond at once:
  1. Edit the release notes (still editable on an immutable release) to warn at the top: name the asset that does
     not match `SHA256SUMS`/the signed metadata and say not to install it.
     `gh release edit desktop-v<version> --notes-file <warning + original notes>`.
  2. Treat the token as stolen: rotate it, and audit Apps, collaborators and agent sessions with write access.
  3. Publish a superseding release: bump the version on `main`, wait for green `ci`, and tag it. The feed moves to
     it; the bad release stays, marked by its notes.
  4. Do not delete the bad release (deleting frees the tag for reuse); leave it, warned.

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
   release, and `signing.macRotation` records the pending macOS key change (`fromSlot: A` and A's Sparkle key).
   Merge, then release **each channel** once: that channel's first release after the promote is its **rotation
   release** (signed by B; macOS updates through the DMG, below).
4. After every channel's rotation release is out: `rotate --retire` (typed confirmation `retire A`) deletes slot
   A's three secrets, only once `origin/main` has B active with nothing staged, and clears `signing.macRotation`.
   Merge that `release-trust.json`; from then on every release updates macOS with the zip again.

### macOS: the DMG rotation release

Sparkle 2 trusts one EdDSA key per build (`SUPublicEDKey`), so the Sparkle key cannot span a staged window: it stays
on the active key while staged and changes at `--promote`. Sparkle's documented rule
(<https://sparkle-project.org/documentation/>, "Rotating signing keys") is what moves the installed Macs:

> Sparkle allows rotating keys by issuing a new update that changes either your Apple code signing certificate or
> your EdDSA keys (but not both). For applications that opt into enabling SUVerifyUpdateBeforeExtraction, changing
> your EdDSA keys can only be done if the update archive is a Developer ID code signed disk image (dmg).

We set `SUVerifyUpdateBeforeExtraction` and `SURequireSignedFeed`, and keep the Developer ID certificate, so the
EdDSA key changes through a DMG. Sparkle 2 (`SUUpdateValidator`, `AppInstaller`, `SUAppcastDriver`) applies the rule
this way, and the rotation release follows it exactly:

- **The update archive is the DMG**, Developer ID signed by the same team as the installed app, notarized and
  stapled. `electron-builder.cjs` sets `dmg.sign` and `scripts/mac-rotation-dmg.cjs` notarizes and staples it while
  `signing.macRotation` is set; a zip cannot carry a code signature, so a zip with a new key is refused.
- **Its app carries the new key** in `SUPublicEDKey` (main's `sparklePublicKey`, B since the promote).
- **The archive's EdDSA signature is by the new key (B).** The installed app first checks it under its own key (A);
  that fails, so Sparkle falls back to the DMG's Developer ID signature (team match), and after extraction requires
  the archive signature to verify under the new `SUPublicEDKey`. An archive not signed by the key the update declares
  is rejected.
- **The appcast is signed by the old key (A)**, the key the installed apps trust: `SURequireSignedFeed` checks the
  feed under the installed app's key. The sign job loads slot A for exactly this (`MAC_UPDATE=dmg`).

Macs then move to the new key by updating normally, with no reinstall. Every other release keeps the zip.

How the flow knows: the gate outputs `mac_update`. It is `dmg` when `signing.macRotation` is set on main **and** the
channel's newest published release (immutable, made by the workflow) was built at a commit whose `release-trust.json`
pinned the old Sparkle key. Once a channel's rotation release is out its newest release carries the new key, so its
next release is a zip again, with or without `--retire`. A channel with no published release has no Macs to move.
`sign`, `publish`, `verify-published` and `feed` all receive the gate's decision and refuse a DMG for any other
release and a zip for the rotation release. Installed apps (`update.ts`, `SparkleBridge.mm`) accept a `.dmg` update URL
inside the version's release; the workflow decides which archive the signed metadata names.

While the change is pending the Pages feed keeps serving the other channel's last release (signed by the old keys,
still pinned) until that channel's own rotation release. After `--retire`, a channel whose newest release is still
its DMG rotation release (appcast signed by A) leaves the site until its next release.

**Timing.** A Mac reaches the new key only through its channel's rotation release, while that is the channel's
newest release: the rotation build verifies the next (B-signed) feed, but a Mac still on A cannot. So leave each
rotation release live long enough for Macs to update (as with the 60-day transition wait) before that channel's next
release. A Mac that skips it reinstalls once.

**The first real rotation, checked by hand** (CI cannot: its unsigned acceptance build has no Developer ID team, and
Sparkle's key-change path needs one). Before tagging a channel's rotation release:

1. The macOS `desktop-mac-arm64` job of the tagged commit's `ci` run passed `macOS update disk image check`
   (`scripts/check-mac-dmg.mjs`): the DMG's app carries B's `SUPublicEDKey`; the DMG is Developer ID signed by the
   app's team, stapled, and `spctl --assess --type open --context context:primary-signature` reports
   `source=Notarized Developer ID`.
2. After `verify-published`: download `appcast-arm64.xml` from the release and check its enclosure is
   `dilla-<version>-mac-arm64.dmg`; its `sparkle:edSignature` verifies under B (`sparklePublicKey`) and the feed's
   signature under A (`signing.macRotation.fromSparklePublicKey`), e.g. with Sparkle's `sign_update --verify`.
3. On a Mac running the channel's previous release (built for A): Check for Updates installs the rotation release
   and relaunches; `defaults read /Applications/Dilla.app/Contents/Info SUPublicEDKey` then shows B. Keep Console
   open filtered on `Sparkle`: a failure says which check refused it.
4. After the channel's next release (zip, all B): the updated Mac installs it as usual.

Caveat: a Windows or Linux client that skipped the transition release rejects releases signed by B and must
reinstall. `rotate` and `--promote` print the macOS summary.

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
- Key rotation strands Windows and Linux clients that skipped the transition release, and Macs that skipped their
  channel's DMG rotation release.
- The DMG rotation path is exercised end to end only by the first real rotation (see the hand checks above); CI
  checks the rotation DMG's signature, notarization and key, and the release flow's key use, but not Sparkle itself.
- First-use behaviour of the environment, secrets and `workflow_run` is only exercised by the first real tag; every
  failure before `publish` leaves nothing public (recovery: Recovery, "A red `gate` on the first tag").
