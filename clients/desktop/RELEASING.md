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
2. Merge the pull request and wait for the `desktop-gpui` workflow's **push** run on the merge commit to go green. The
   tag job refuses a commit without one (`scripts/server-release-gate.mjs --workflow=desktop-gpui`). That workflow is
   path-filtered: a commit that touched nothing it watches has no run, so tag a commit that has one.
3. Push the tag, once:

   ```sh
   git tag desktop-gpui-v0.2.0-alpha.N <merge sha>
   git push origin desktop-gpui-v0.2.0-alpha.N
   ```

4. Watch the run. `tag` checks the tag, `build-linux` builds and packages, `smoke` installs every asset in clean
   Ubuntu, Debian, Fedora and Arch containers (and over beta.5's deb and rpm) and launches the AppImage, and
   `publish` creates a **draft** prerelease with every file, re-downloads it, checks it against `SHA256SUMS` and only
   then publishes it.
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

## Milestone 2 (not yet)

The signed update feed, the macOS DMG, the Windows installer and the in-app updater come with
`desktop-gpui-sign.yml`, main's own `workflow_run` workflow in the `desktop-release` environment. This section is
written when they land.
