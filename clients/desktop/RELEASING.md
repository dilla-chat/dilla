# Releasing the native desktop app

The native app (`dilla-desktop`) is released from tags `desktop-gpui-v<X.Y.Z>-alpha.<N>` by
`.github/workflows/desktop-gpui-release.yml` (plan: `docs/superpowers/plans/2026-11-15-gpui-release.md`). The prefix
keeps these tags away from the Electron app's `desktop-v*` tags and its update feed.

## Milestone 1: the Linux alpha (keyless)

Assets: `dilla-<v>-linux-x64.{AppImage,tar.gz,deb,rpm,aur.tar.gz}` and `SHA256SUMS`, each asset with a GitHub
build-provenance attestation. The AUR recipe is a release asset only; nobody publishes it to the AUR.

### Cut a release

1. Set `version` in `clients/desktop/dilla-desktop/Cargo.toml` to the release's version (`0.2.0-alpha.N`), with its
   `Cargo.lock` line, in the pull request that ships it.
2. Merge the pull request. You do not wait for a second CI run on the merge commit. The tag job needs a green
   `desktop-gpui [nightly]` or `[release]` run on the tagged commit's tree and no open `ci-nightly` issue; a `[pr]` run
   never counts. When there is no such run it dispatches a `[release]` run on the tag and waits up to 150 minutes for
   it (`scripts/server-release-gate.mjs --workflow=desktop-gpui`). The tiers, and what to do when the gate refuses,
   are in `docs/dev/testing.md`.
3. Push the tag, once:

   ```sh
   git tag desktop-gpui-v0.2.0-alpha.N <merge sha>
   git push origin desktop-gpui-v0.2.0-alpha.N
   ```

4. Watch the run. `tag` checks the tag, `build-linux` builds and packages, `smoke` installs every asset in clean
   Ubuntu, Debian, Fedora and Arch containers (and over beta.5's deb and rpm) and launches the AppImage, and
   `publish` creates a **draft** prerelease with every file, re-downloads it and checks it against `SHA256SUMS`. It
   leaves the draft: since Milestone 2, `desktop-gpui-sign.yml` adds the macOS and Windows files and publishes it
   (below).
5. Check the published release:

   ```sh
   gh release download desktop-gpui-v0.2.0-alpha.N --dir check && cd check
   sha256sum --check SHA256SUMS
   gh attestation verify dilla-0.2.0-alpha.N-linux-x64.AppImage --repo dilla-chat/dilla
   ```

There is no server change in a desktop release, so no Homelab upgrade follows it.

### When a run fails

- **Never edit a published release.** Releases in this repository are immutable once published; fix forward with the
  next alpha.
- A run that failed before `publish` left no release: fix the cause on `main`, then re-run the failed jobs, or run
  the workflow by hand (Actions, "desktop gpui release", Run workflow, **Use workflow from: the tag itself**, and type
  the same tag). The tag job refuses a mismatch between the two.
- A run that failed inside `publish` may have left a **draft**. Delete the draft (`gh release delete <tag>`, keeping
  the tag), then run the workflow by hand from the tag as above. A release that already exists, draft or published,
  is refused.
- A tag on the wrong commit: delete the tag (and its draft) before anything is published, then tag again.
- A throwaway tag `desktop-gpui-v0.0.0-alpha.0` may dry-run the workflow (D9). It needs `dilla-desktop` at that
  version on the tagged commit, so it suits a branch merged for the purpose; delete its draft and the tag afterwards,
  and never let it publish.

### Pins

Everything the release downloads is pinned in the repository:

- `scripts/fetch-libwebrtc.sh`: LiveKit's prebuilt libwebrtc per target (SHA-256), the archive `webrtc-sys-build`
  names. A Cargo.lock bump of `webrtc-sys-build` fails `scripts/fetch-libwebrtc.test.mjs` until the tag and digests
  are re-reviewed.
- `scripts/install-ci-tool.sh`: `appimagetool` 1.9.1, the AppImage `type2-runtime` 20251108 (dated releases, never
  `continuous`) and `nfpm` 2.48.0.
- `scripts/desktop-gpui-smoke-linux.sh`: beta.5's deb and rpm for the upgrade checks.
- Actions by commit; `scripts/check-ci-workflow.mjs` (`checkDesktopGpuiReleaseWorkflow`) pins the workflow's shape.

### Linting notes

`lintian` on the deb reports `dir-or-file-in-opt` (the app lives in `/opt/Dilla`, beta.5's layout, plan D3),
`malformed-contact` and `no-changelog` (the project has no maintainer address to put in either). `namcap` on the Arch
package reports `ELF files outside of a valid path ('opt/')` for the same layout, and warns about dependencies the app
loads at run time (Vulkan, Wayland, X11) rather than links. Those are accepted; anything else is a bug (the smoke
test fails on any other namcap error).

## Milestone 2: signed feed, macOS and Windows (keyed)

A release is two workflows. The tag run above (`desktop-gpui-release.yml`, keyless) now leaves the release a
**draft** with the Linux files. When it goes green, `.github/workflows/desktop-gpui-sign.yml` starts by itself. It is
a `workflow_run` workflow, so GitHub always runs **main's** copy of it, never the tag's, and it is the only workflow
that touches the release keys.

Assets of a published release: the five Linux files, `dilla-<v>-mac-arm64.dmg`, `dilla-<v>-mac-arm64.zip` (what the
in-app updater downloads), `dilla-<v>-win-x64-setup.exe` and `SHA256SUMS` over all eight, each with a build-provenance
attestation. The signed feed is served at
`https://dilla-chat.github.io/dilla/gpui-alpha/desktop-gpui-alpha.json` (+ `.sig`).

### Secrets

| Secret | Where it lives | Used by (the only step that names it) |
|---|---|---|
| `DESKTOP_METADATA_KEY_A` | `desktop-release` environment | `sign`: "Sign the feed" (beta.5's metadata key slot A, D8; the `channel` field keeps the GPUI feed apart from beta.5's) |
| `CSC_LINK`, `CSC_KEY_PASSWORD` | repository secrets today | `build-mac`: "Package, sign and notarize" (the base64 Developer ID Application `.p12` and its password, as beta.5) |
| `APPLE_API_KEY`, `APPLE_API_KEY_ID`, `APPLE_API_ISSUER` | repository secrets today | `build-mac`: the same step (the App Store Connect API key for `notarytool`) |

Both keyed jobs enter `environment: desktop-release`, whose deployment policy admits `main` only. The Apple and
certificate secrets work unchanged if they are moved into that environment, which is the tighter place for them (a
repository secret is readable by any workflow on main). The macOS step fails, naming the missing secrets, rather than
sign ad hoc. Windows is unsigned for the alpha (D10); `scripts/desktop-gpui-package-win.ps1` has a disabled `signtool`
hook for later. `scripts/check-ci-workflow.mjs` (`checkDesktopGpuiSignWorkflow`) fails if a secret appears anywhere
else, and `checkDesktopGpuiReleaseWorkflow` fails if the tag workflow names any secret, enters any environment or
publishes.

### Order of operations

1. Cut the tag as in Milestone 1. `desktop-gpui-release.yml` builds, smoke-tests and attests the Linux files and leaves
   a draft prerelease made by `github-actions[bot]` with them and `SHA256SUMS`.
2. `desktop-gpui-sign.yml` starts on that run's success:
   - `gate` (`scripts/desktop-gpui-gate.mjs`): first attempt only and no other sign run active; re-derives the tag's
     commit from the API, checks the triggering run is `desktop-gpui-release.yml` for that tag and commit and green,
     the commit is on `main`, the Cargo version is the tag's, the tree has a green `desktop-gpui` `[nightly]` or `[release]` run and no `ci-nightly` issue is open (this gate never
     dispatches a run; `docs/dev/testing.md`), and the
     draft holds exactly the Linux files, uploaded by `github-actions[bot]`. It downloads them, checks them against
     the API digests and `SHA256SUMS`, and runs `gh attestation verify` on each (signed by `desktop-gpui-release.yml`
     at that tag and commit, in that run, on a hosted runner, with exactly the five assets as subjects).
   - `build-mac` (macos-15, `desktop-release`) and `build-win` (windows-latest, keyless) build the gated commit with
     the pinned libwebrtc and package it (`desktop-gpui-package-mac.sh`: signed, notarized, stapled, checked by
     `spctl`; `desktop-gpui-package-win.ps1`).
   - `sign` (`desktop-release`) checks all eight files against the gate and the builds and signs the feed.
   - `publish` attaches the macOS and Windows files to the draft, rewrites `SHA256SUMS` over all eight, re-downloads
     everything and checks it against `SHA256SUMS` and the signed feed, and only then publishes the release.
   - `feed` (`github-pages`) rebuilds the Pages site, keeping beta.5's live `beta/` files byte for byte after checking
     them under the pinned key, adds `gpui-alpha/`, deploys and checks the live site.
3. Check the published release:

   ```sh
   gh release download desktop-gpui-v0.2.0-alpha.N --dir check && cd check
   sha256sum --check SHA256SUMS
   curl -fsSLO https://dilla-chat.github.io/dilla/gpui-alpha/desktop-gpui-alpha.json
   curl -fsSLO https://dilla-chat.github.io/dilla/gpui-alpha/desktop-gpui-alpha.json.sig
   node scripts/desktop-gpui-feed.mjs verify desktop-gpui-alpha.json desktop-gpui-alpha.json.sig
   ```

4. Once the feed is live, the Homelab session can set `--desktop-downloads-channel=gpui-alpha` on CT 144 (D7).

### Dry run (D9)

The throwaway tag `desktop-gpui-v0.0.0-alpha.0` runs both workflows end to end except publishing: the tag run leaves
its draft, the sign run notarizes, signs a feed (kept only as a 3-day workflow artifact) and completes the draft, but
`publish` skips its last step and `feed` does not run. As in Milestone 1, it needs `dilla-desktop` at version
`0.0.0-alpha.0` on the tagged commit of `main`. Afterwards delete the draft and the tag:

```sh
gh release delete desktop-gpui-v0.0.0-alpha.0 --yes
git push origin :refs/tags/desktop-gpui-v0.0.0-alpha.0
```

### When the sign run fails

- Every job but `feed` refuses a re-run (`GITHUB_RUN_ATTEMPT` must be 1): a partial re-run would reuse the gate's
  outputs without re-checking them. `feed` alone may be re-run (it deploys the same signed files).
- Anything that failed before `publish`'s last step left the release a **draft**. Fix the cause on `main`, delete the
  draft (`gh release delete <tag> --yes`, keeping the tag), and run `desktop-gpui-release.yml` by hand from the tag
  (Milestone 1, "When a run fails"); its success starts a fresh sign run, which runs main's current file.
- A sign run that never started: the release run failed, or was not for a `desktop-gpui-v*` tag. The gate's log says
  which check refused.
- Never edit a published release; fix forward with the next alpha.
