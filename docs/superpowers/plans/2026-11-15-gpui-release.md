# GPUI desktop — downloadable release (phase 3 packaging) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

## Decisions (final, Jonas 2026-11-15)

| # | Decision |
|---|---|
| D1 | Tags are `desktop-gpui-v0.2.0-alpha.N`, and the `dilla-desktop` Cargo version equals the tag version. The separate prefix keeps the old `desktop-v` gate and feed away from them. |
| D2 | Channel `alpha` (GitHub prerelease + feed `channel`) until the switch (phase 4). |
| D3 | **The product is plain "Dilla" from day one, laid out like beta.5.** Binary `dilla-desktop`, `/opt/Dilla`, window/bundle/AUMID id `dev.dilla.desktop`. The deb and rpm package is named `dilla` (same as beta.5), so it upgrades over an installed beta.5. The AUR recipe is `dilla-bin` with `provides`/`conflicts` `dilla`. **User data paths stay as they are:** the GPUI data root `<OS data dir>/Dilla Preview` (`dilla-client/src/instances.rs` `default_root`) and keychain service `dev.dilla.desktop.preview` (`keychain.rs` `SERVICE`) are not touched, so GPUI data never collides with Electron's. Only `dilla-desktop/src/lib.rs` `APP_ID` (the window id) changes. This overrides spec §8's "separately named test build". |
| D4 | The AUR recipe is a **release asset only**. Nobody publishes to AUR, by hand or automatically, in M1 or M2. |
| D5 | M1 formats: AppImage, tar.gz, deb, rpm, AUR recipe. If the rpm smoke test is flaky, drop rpm rather than delay. |
| D6 | glibc floor 2.39 (Ubuntu 24.04+, Debian 13, Fedora 40+, Arch). |
| D7 | `dillad --desktop-downloads` switches to GPUI in M2 through a new `--desktop-downloads-channel=beta\|gpui-alpha`. The default stays `beta` (beta.5). |
| D8 | The feed is signed with beta.5 metadata key slot A (`DESKTOP_METADATA_KEY_A`), and the `channel` field keeps the feeds apart. `internal/desktopdownload/keys.go` needs no new pin. |
| D9 | Updates come from our own minimal Rust updater on every OS: signed feed, sha512, and on macOS a Developer ID team check before an atomic bundle swap. There is no Sparkle. A throwaway tag `desktop-gpui-v0.0.0-alpha.0` may be used to dry-run the workflow and is deleted before anything is published. |
| D10 | Windows: Inno Setup, per-user, unsigned for alpha. The SmartScreen step is documented. Signing is decided before beta. |
| D11 | Platforms: linux-x64, mac-arm64, win-x64 only. |
| D12 | No `dilla://` link handler until the GPUI app handles invite links (a separate phase-3 task). Replacing beta.5 drops its handler in the meantime. |
| D13 | M1 provenance = GitHub build-provenance attestations + SHA256SUMS on every asset. |
| D14 | Release builds pin libwebrtc's SHA-256 (`LK_CUSTOM_WEBRTC`). CI keeps downloading. |
| D15 | How beta.5 installs move over at the switch (the spec's Electron "bridge release", whose sources #173 removed) is out of scope here. It is decided in the phase-4 plan. |

**Goal:** People can download and run the native GPUI desktop client (`clients/desktop`, crate `dilla-desktop`) as "Dilla".
- **M1, Linux x86_64 alpha:** AppImage, tar.gz, deb, rpm and an AUR recipe, all built from `main` by a release workflow on a tag such as `desktop-gpui-v0.2.0-alpha.1`. The deb, rpm and AUR packages replace an installed beta.5. There is no updater yet, and the release notes say plainly that this is an early build.
- **M2:** a signed update feed under the beta.5 Ed25519 metadata key, so that `dillad serve --desktop-downloads-channel=gpui-alpha` can serve GPUI. M2 also brings a signed and notarized macOS DMG, a Windows installer and an in-app updater.

**Architecture:**
- **M1 is keyless.** A tag-triggered workflow (`desktop-gpui-release.yml`) works like `release.yml`.
  - A tag job proves the commit is on `main` with a green `desktop-gpui` push run, and that the Cargo version equals the tag.
  - A clean build runs on `ubuntu-24.04`: no cache, clang 21, pinned libwebrtc.
  - Packaging: AppImage through a pinned `appimagetool`, deb and rpm through a pinned `nfpm`, a tar.gz and a generated AUR recipe.
  - Smoke installs run in containers, including an upgrade over the beta.5 deb and rpm.
  - SHA256SUMS and build-provenance attestations cover every asset.
  - The release is created as a draft, every asset is uploaded, and only then is it published, because releases are immutable.
- **M2 adds `desktop-gpui-sign.yml`.** It always runs `main`'s copy through `workflow_run`, like `mobile-release.yml` and beta.5's `desktop release`, and it is the only place that touches secrets (main-only `desktop-release` environment).
  - It builds and notarizes the Mac app and builds the Windows installer.
  - It signs the feed and deploys Pages, keeping beta.5's `beta/` feed as it is.

**Tech Stack:**
- Rust 1.98.1, `gpui-pre =0.3.12` (wgpu 30: Vulkan with a GLES fallback on Linux).
- `livekit =0.10.1` with LiveKit's prebuilt libwebrtc (vendored `webrtc-sys`).
- `deep-filter-rt =0.2.0`, whose model is embedded in the binary.
- `keyring 4.2` (zbus Secret Service on Linux).
- Packaging tools: `appimagetool` (static type2 runtime, no libfuse2), `nfpm`, Node 22 scripts with `node --test`, `xcrun notarytool`, `hdiutil`, Inno Setup 6 (M2).
- `ed25519-dalek` for the updater (already in Cargo.lock).

**Spec:** `docs/superpowers/specs/2026-11-12-gpui-desktop-design.md`: §6 phase 3 ("packaging per OS", "updater"), §8 Rollout (the naming is overridden by D3), §12 (the switch makes a new device).

**Prior art (git history, tag `desktop-v0.1.0-beta.5`):**
- The release workflow, `.github/workflows/desktop-release.yml` (gate → collect → sign → publish → verify-published → feed), and `packages/desktop/RELEASING.md`.
- Packaging config: `packages/desktop/electron-builder.cjs` and `electron-builder.native-linux.cjs`. The second has deb/rpm `packageName: 'dilla'`, deb version `X.Y.Z~beta.N-1` and rpm release `0.beta.N.1`.
- `packages/desktop/release-trust.json`.
- Scripts under `packages/desktop/scripts/`: `build-native-linux-packages.mjs` (`aurRecipe`), `feed-format.mjs`, `pages-feed.mjs`, `linux-package-version.mjs`.
- Server side, still present: `internal/desktopdownload/{download.go,keys.go}` and `cmd/dillad/serve.go:135`.

---

## Facts this plan rests on (checked 2026-11-15)

- **Identity and data paths in the code today:**
  - `dilla-desktop` is `version = "0.1.0"`.
  - `APP_ID = "dev.dilla.desktop.preview"` (lib.rs:35, used only as the window app id at lib.rs:208 and windows.rs:374). It becomes `dev.dilla.desktop` (D3).
  - The data root is `<OS data dir>/Dilla Preview` (instances.rs `default_root`) and the keychain service is `dev.dilla.desktop.preview`. Both stay, so GPUI data never mixes with Electron's.
- **Release builds:** `automation` and `test-hooks` are opt-in features. The release build must check that they are absent, not just assume it.
- **Nothing ships beside the binary** except the OFL texts, the third-party licence bundle and the brand icons (`clients/desktop/assets/brand/{icon.png,linux.svg,linux-symbolic.svg,Dilla.icns,dilla.ico}`).
  - The fonts (JetBrains Mono, DM Serif Display, OFL) and the icons are `include_bytes!`.
  - The DeepFilterNet3 weights are embedded by `deep-filter-rt`.
- **Linux runtime needs:**
  - `NEEDED` (from `objdump -p`): `libxcb.so.1 libxkbcommon.so.0 libxkbcommon-x11.so.0 libasound.so.2 libm libgcc_s libc ld-linux`.
  - Loaded at runtime: `libvulkan.so.1`, or EGL/GLES through wgpu `gles`; `libwayland-client`; `libX11.so.6`; `libpulse.so.0`; `libdrm`; `libgbm`.
  - A running Secret Service provider is **required**. The app reaches it over D-Bus with pure-Rust zbus, and there is no file fallback.
  - Printing and file dialogs go through the XDG portal (ashpd). Emoji use the system Noto Color Emoji. Links open through `xdg-open`.
- **glibc:** the local CachyOS build needs GLIBC_2.44, so release binaries are built on `ubuntu-24.04` and checked with `scripts/check-glibc-max.sh 2.39`.
- **libwebrtc:** `webrtc-sys` build.rs calls `webrtc_sys_build::download_webrtc()` with no digest check.
- **`desktopdownload` today** takes only:
  - channel `beta`, version `X.Y.Z-beta.N`, tag prefix `desktop-v`;
  - feed `https://dilla-chat.github.io/dilla/beta/desktop-beta.json` (+ `.sig`, Ed25519 over the exact bytes, SPKI pin in keys.go);
  - assets `dilla-<v>-{linux-x64.AppImage,linux-x64.deb,linux-x64.rpm,linux-x64.aur.tar.gz,mac-arm64.dmg,win-x64.exe}`.
- **Pages:** a deployment replaces the whole site. The live `beta/` files still serve installed Electron apps and today's `--desktop-downloads`.
- **beta.5 signing:** macOS used `CSC_LINK`, `CSC_KEY_PASSWORD` and `APPLE_API_KEY` / `APPLE_API_KEY_ID` / `APPLE_API_ISSUER`. Windows was unsigned.
- **beta.5 Windows install:** one-click per-user NSIS under `%LOCALAPPDATA%\Programs\Dilla`, with `deleteAppDataOnUninstall: false`.

## Global Constraints

- **Server changes:** none in M1. In M2 only `internal/desktopdownload` and its flags change. They ship in a normal `v*` server release, followed by the Homelab CT 144 upgrade.
- **Client code changes:**
  - `main.rs` gains `--version`.
  - `APP_ID` becomes `dev.dilla.desktop`.
  - M2 adds the updater module.
  - Nothing else: no screen work, and no change to data roots or the keychain service.
- **Same layout as beta.5 (D3):** product `Dilla`, binary `dilla-desktop`, `/opt/Dilla/`, `/usr/bin/dilla-desktop`, deb/rpm package `dilla`, AUR `dilla-bin`, Mac `Dilla.app` with bundle id `dev.dilla.desktop`, Windows `%LOCALAPPDATA%\Programs\Dilla`. Upgrading from beta.5 never deletes Electron's user data (it just stays on disk, unused).
- **Version ordering must be above beta.5:**
  - deb `0.2.0~alpha.N-1` > `0.1.0~beta.5-1`;
  - rpm `0.2.0-0.alpha.N.1` > `0.1.0-0.beta.5.1`;
  - AUR `0.2.0alphaN` > `0.1.0beta5` (`vercmp`).
- **The release build is clean:** no `actions/cache` restore, `--locked`, `-p dilla-desktop` with default features only, and a check that fails the build if automation or test-hook symbols are present.
- **Pins:** every downloaded tool and archive has a SHA-256 pin in the repo (`scripts/install-ci-tool.sh` style). Actions are pinned by commit, and `scripts/check-ci-workflow.mjs` pins each new workflow.
- **Secrets:** a tag-run workflow file never reaches them. The M1 workflow reads no secret and enters no environment. All keyed and Apple work (M2) runs in main's `workflow_run` workflow in `desktop-release`. It runs on the first attempt only and re-derives tag → commit → green runs → asset digests from the API.
- **Asset names:** `dilla-<version>-<os>-<arch>.<ext>`, the same pattern as beta.5. Tags differ (`desktop-gpui-v…` against `desktop-v…`), so the names never clash.
- **Process:** `npm run ci:local` before every push. One PR per milestone (fewer-PRs rule). Each closing task updates README "what works", `docs/user/` and the release notes.

## Review Focus

1. **Supply chain:**
   - `appimagetool`, `nfpm` and the libwebrtc archive are pinned by digest;
   - no cache in the release build;
   - the attestation subjects are exactly the published assets;
   - SHA256SUMS covers every asset.
2. **Upgrade over beta.5:**
   - the deb and rpm upgrade in place and remove `/opt/Dilla`'s Electron files;
   - no maintainer script deletes user data;
   - the version ordering holds;
   - the AUR `provides`/`conflicts` are right;
   - beta.5's rpm `after-remove` script must not break the upgrade, so test it.
3. **Secret isolation (M2):**
   - no `secrets.*` or `environment:` in `desktop-gpui-release.yml`;
   - the sign workflow loads from main only and re-derives everything;
   - the Apple and metadata keys exist only in the signing steps.
4. **Feed domain separation:**
   - the GPUI feed's `channel` is never `beta`;
   - a beta.5 Electron updater never accepts a GPUI document;
   - `desktopdownload` never crosses channels in either direction.
5. **Pages:** a GPUI feed deploy keeps the live `beta/` files byte for byte, after checking them under the pinned key.
6. **Release binary hygiene:**
   - `automation` is compiled out;
   - `--allow-loopback` is inert in release (`cfg!(debug_assertions)`);
   - the glibc floor holds;
   - the binary is stripped and the licences are shipped.
7. **Packaging dependencies:** the packages declare what the runtime facts above need. The Secret Service requirement is documented and its error is clear.
8. **Updater (M2):**
   - the signature is checked before parsing;
   - size and sha512 are checked before applying;
   - no downgrade;
   - the swap is atomic;
   - on Mac, the Developer ID team is checked before the bundle is replaced.

---

## Milestone 1 — Linux alpha

Parallel lanes: **Tasks 1, 2, 3 and 4 run in parallel** (disjoint files). Task 5 needs 1–4. Tasks 6 and 7 run in parallel after 5. Task 8 is last.

### Task 1 — Release identity: version, app id, `--version`, release-build guard  *(parallel lane A)*

**Files:**
- `clients/desktop/dilla-desktop/Cargo.toml`: version.
- `clients/desktop/dilla-desktop/src/lib.rs`: `APP_ID = "dev.dilla.desktop"`.
- `clients/desktop/dilla-desktop/src/main.rs`: `--version` / `-V`, handled before any GPUI init.
- `scripts/check-desktop-release-binary.sh` (new) and `scripts/check-desktop-release-binary.test.mjs`.

**Interfaces:**
- `dilla-desktop --version` prints `Dilla <CARGO_PKG_VERSION> (<short sha from DILLA_BUILD_SHA at build, else "dev">)` and exits 0 without opening a display. The packaging smoke tests rely on it.
- `check-desktop-release-binary.sh <binary>`:
  - exits 1 if it finds `automation::run`, `test_support`, `forget_from_forced` or `--automation`;
  - exits 2 on a tool error, the same exit contract as `check-glibc-max.sh`.

**Steps:**
- [ ] Set `version = "0.2.0-alpha.1"`.
- [ ] Add `parse_args` tests: `--version` is recognised, and unknown flags are still ignored.
- [ ] Change `APP_ID`. Grep tests and parity code for `dev.dilla.desktop.preview` used as the *window* id, and leave `keychain::SERVICE` and `PRODUCT_DIR` alone.
- [ ] Write the guard script and its node test (a fixture binary with and without the markers).
- [ ] Verify:
  - `cargo build --release -p dilla-desktop --locked && target/release/dilla-desktop --version && bash scripts/check-desktop-release-binary.sh target/release/dilla-desktop` passes;
  - a `--features automation` build fails the guard;
  - `cargo test -p dilla-desktop` is green.

### Task 2 — Linux packaging inputs  *(parallel lane B)*

**Files (new):**
- `clients/desktop/packaging/linux/dev.dilla.desktop.desktop`
- `clients/desktop/packaging/linux/dev.dilla.desktop.metainfo.xml`
- `clients/desktop/packaging/linux/icons/hicolor/{16..512}/apps/dev.dilla.desktop.png`, plus `scalable/` from `linux.svg` and `symbolic/` from `linux-symbolic.svg`
- `clients/desktop/packaging/linux/AppRun`
- `clients/desktop/packaging/linux/nfpm.yaml`
- `clients/desktop/packaging/THIRD-PARTY.md` generation notes

**Interfaces:**
- **Desktop entry:**
  - `Name=Dilla`, `Exec=dilla-desktop %U`, `Icon=dev.dilla.desktop`;
  - `StartupWMClass=dev.dilla.desktop`, which must equal `APP_ID` (Wayland matches icons by it);
  - `Categories=Network;InstantMessaging;Chat;`;
  - no `MimeType` (D12).
  - Check beta.5's desktop file name (`dpkg-deb -c` on the published `dilla-0.1.0-beta.5-linux-x64.deb`). If it differs, the upgrade removes it anyway, because the package is the same.
- **`nfpm.yaml`:**
  - `name: dilla`, `version_schema: none`;
  - deb version `0.2.0~alpha.N`, release `1`; rpm version `0.2.0`, release `0.alpha.N.1` (port `linux-package-version.mjs`; set per format from the script);
  - `provides: [dilla]`;
  - installs `/opt/Dilla/dilla-desktop`, the licences at `/opt/Dilla/licenses/`, a symlink `/usr/bin/dilla-desktop`, and the desktop file, metainfo and icons under `/usr/share`;
  - no maintainer scripts.
- **Deb dependencies (Ubuntu names):**
  - `depends`: `libc6 (>= 2.39), libgcc-s1, libxcb1, libxkbcommon0, libxkbcommon-x11-0, libasound2t64 | libasound2, libvulkan1 | libegl1, libwayland-client0, libx11-6, libdrm2, libgbm1, xdg-utils`.
  - `recommends`: `gnome-keyring | kwalletmanager | keepassxc, libpulse0 | pipewire-pulse, xdg-desktop-portal, fonts-noto-color-emoji, mesa-vulkan-drivers`.
- **RPM dependencies:** the same set with Fedora names (`alsa-lib`, `vulkan-loader`, `libxkbcommon-x11`, …).

**Steps:**
- [ ] Generate the PNG sizes once from `assets/brand/icon.png`, record the command in a README beside them, and check them in.
- [ ] Desktop file and metainfo. Verify with `desktop-file-validate` and `appstreamcli validate --no-net`.
- [ ] `AppRun` is `exec "$APPDIR/usr/bin/dilla-desktop" "$@"`. Nothing is bundled, and it sets no `LD_LIBRARY_PATH`.
- [ ] Licence bundle: `scripts/desktop-third-party.mjs` over `cargo metadata` (licence plus repository per crate), plus the OFL texts.
- [ ] Verify:
  - `nfpm package -p deb` and `-p rpm` on a hand-built binary;
  - `dpkg-deb -I/-c` and `rpm -qpil` show the layout above;
  - `lintian` shows no errors.

### Task 3 — Pinned tools and libwebrtc  *(parallel lane C)*

**Files:**
- `scripts/install-ci-tool.sh`: new `appimagetool` and `nfpm` cases.
- `scripts/install-ci-tool.test.mjs`.
- `scripts/fetch-libwebrtc.sh` (new) and its test.

**Interfaces:**
- `install-ci-tool.sh appimagetool|nfpm`:
  - each pins a reviewed release asset by its archive SHA-256;
  - `appimagetool` comes from a *dated* release of AppImage/appimagetool (static type2 runtime), never `continuous`.
- `fetch-libwebrtc.sh <target-triple> <out-dir>`:
  - downloads the exact archive `webrtc_sys_build` fetches for `webrtc-sys 0.3.53`;
  - checks a checked-in SHA-256 per triple (`x86_64-unknown-linux-gnu`, `aarch64-apple-darwin`, `x86_64-pc-windows-msvc`);
  - unpacks it and prints `LK_CUSTOM_WEBRTC=<dir>`.

**Steps:**
- [ ] Add both tools, each with a digest-mismatch test.
- [ ] Find the libwebrtc URL in the vendored `build.rs` and `webrtc-sys-build`, and pin the three digests. Record the source tag in a comment.
- [ ] Verify: with `LK_CUSTOM_WEBRTC` set, a clean `cargo build --release -p dilla-desktop` fetches no libwebrtc. Run it with the network off after `cargo fetch`.

### Task 4 — AUR recipe generator (release asset only)  *(parallel lane D)*

**Files:** `scripts/desktop-gpui-aur.mjs` and `scripts/desktop-gpui-aur.test.mjs` (new). Port `aurRecipe`, `verifyAurRecipe` and `writeAurRecipe` from beta.5's `build-native-linux-packages.mjs`.

**Interfaces:**
- `aurRecipe(version, sha256) -> { PKGBUILD, '.SRCINFO' }`, mirroring beta.5's recipe:
  - `pkgname=dilla-bin`, `pkgdesc='Dilla desktop client'`, `provides=('dilla')`, `conflicts=('dilla')`, `options=('!strip' '!debug')`;
  - `pkgver` maps `0.2.0-alpha.1` → `0.2.0alpha1`;
  - `source` = the release's `dilla-<v>-linux-x64.tar.gz`;
  - `depends=(glibc gcc-libs libxcb libxkbcommon libxkbcommon-x11 alsa-lib vulkan-icd-loader wayland libx11 libdrm mesa xdg-utils)`;
  - `optdepends`: `gnome-keyring` (a Secret Service is required unless another provider such as `kwallet` or `keepassxc` is running), `libpulse`, `xdg-desktop-portal`, `noto-fonts-emoji`;
  - `package()` installs `/opt/Dilla/dilla-desktop`, `/usr/bin/dilla-desktop` and the `/usr/share` entries. There is no `chrome-sandbox` line (that was Electron's).
- `writeAurRecipe(version, tarball, outDir)` writes `dilla-<v>-linux-x64.aur.tar.gz` reproducibly (`--sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner`).
- **Nothing pushes to aur.archlinux.org (D4).**

**Steps:**
- [ ] Port the code and its tests (bounded tar reader, exact-content check).
- [ ] Verify:
  - `node --test scripts/desktop-gpui-aur.test.mjs`;
  - in `archlinux:latest`, run `makepkg -si` with `source` pointed at a local file, then `namcap`, then `dilla-desktop --version` (Task 6's smoke job repeats this);
  - `vercmp 0.2.0alpha1-1 0.1.0beta5-1` prints `1`.

### Task 5 — Linux package script  *(after 1–4)*

**Files:** `scripts/desktop-gpui-package-linux.sh` (new) and `scripts/desktop-gpui-package-linux.test.mjs` (asset set and naming over a fake binary).

**Interface:** `desktop-gpui-package-linux.sh <version> <binary> <out-dir>` writes exactly these files and fails on anything extra or missing:
- `dilla-<v>-linux-x64.AppImage`
- `dilla-<v>-linux-x64.tar.gz`, laid out as `dilla-<v>/{dilla-desktop,dev.dilla.desktop.desktop,icons/,licenses/}`
- `dilla-<v>-linux-x64.deb`
- `dilla-<v>-linux-x64.rpm`
- `dilla-<v>-linux-x64.aur.tar.gz`

**Steps:**
- [ ] Order of work:
  1. `strip`
  2. `check-glibc-max.sh 2.39`
  3. `check-desktop-release-binary.sh`
  4. AppDir: `usr/bin`, `usr/share/{applications,icons,metainfo}`, top-level `.desktop` and icon, `AppRun`
  5. `appimagetool` (AppStream check on)
  6. tar.gz
  7. `nfpm` deb and rpm
  8. the AUR recipe from the tar.gz
- [ ] Verify locally in `ubuntu:24.04`: `./dilla-*.AppImage --appimage-extract-and-run --version` and `apt install ./…deb && dilla-desktop --version`.

### Task 6 — Release workflow `desktop-gpui-release.yml`  *(after 5; parallel with 7)*

**Files:**
- `.github/workflows/desktop-gpui-release.yml` (new)
- `scripts/check-ci-workflow.mjs`: new `checkDesktopGpuiReleaseWorkflow`
- `scripts/check-ci-workflow.test.mjs`
- `scripts/desktop-gpui-release-notes.mjs` and its test

**Jobs:**
1. **`tag`** (ubuntu-24.04, `contents: read`, `actions: read`):
   - checks the tag matches `^desktop-gpui-v(\d+\.\d+\.\d+-alpha\.\d+)$`;
   - the tag commit is an ancestor of `origin/main` (as in `release.yml`'s tag job);
   - a successful `desktop-gpui` **push** run exists on that SHA;
   - the `cargo metadata` version equals the tag;
   - no release exists for the tag yet;
   - outputs `version` and `sha`;
   - `workflow_dispatch` takes a `tag` input for rebuilds.
2. **`build-linux`** (ubuntu-24.04, `contents: read`, timeout 90):
   - checkout at `sha`, with no cache;
   - the LLVM 21 apt repo and the apt dependency list from `desktop-gpui.yml`;
   - `fetch-libwebrtc.sh`;
   - `DILLA_BUILD_SHA=$sha cargo build --release --locked -p dilla-desktop`;
   - `install-ci-tool.sh` for both tools;
   - `desktop-gpui-package-linux.sh`;
   - `SHA256SUMS`;
   - `actions/attest-build-provenance` over the five assets (`id-token: write`, `attestations: write`);
   - upload as an artifact.
3. **`smoke`** (matrix, `needs: build-linux`): `dilla-desktop --version` after installing in:
   - `ubuntu:24.04` (deb, and AppImage extract-and-run);
   - `debian:trixie` (deb);
   - `fedora:41` (rpm);
   - `archlinux:latest` (makepkg from the recipe with a local source, plus namcap).
   
   It also covers:
   - **Upgrade over beta.5:** in `ubuntu:24.04`, install the published `dilla-0.1.0-beta.5-linux-x64.deb`, then the new deb. `dpkg -l dilla` shows the new version, `/opt/Dilla` holds only the GPUI files, and `dilla-desktop --version` works. The same with the beta.5 rpm in `fedora:41`. Fetch the beta.5 assets with `gh release download desktop-v0.1.0-beta.5` and check them against that release's SHA256SUMS.
   - **One real launch:** `xvfb-run` of the AppImage with lavapipe and `gnome-keyring-daemon --unlock` in a dbus session. It runs 10 s, must still be alive, then exits on SIGTERM.
4. **`publish`** (`contents: write`, needs tag, build-linux and smoke):
   - release notes via `desktop-gpui-release-notes.mjs`: the Task 7 banner, an install table, and PR titles since the previous `desktop-gpui-v*` tag;
   - `gh release create --draft --prerelease --verify-tag`;
   - upload the 5 assets and SHA256SUMS;
   - re-download them and check them against SHA256SUMS;
   - `gh release edit --draft=false`.

**Steps:**
- [ ] Workflow and check-ci pins:
  - no `secrets` and no `environment`;
  - no cache action;
  - actions pinned by SHA;
  - the tag checks present;
  - `--locked`;
  - the glibc and release-binary guards present;
  - `publish` runs only after `smoke`.
- [ ] Release-notes tests: the banner comes first, and every asset has its install command.
- [ ] Verify:
  - `node --test scripts/check-ci-workflow.test.mjs scripts/desktop-gpui-release-notes.test.mjs` and `npm run ci:local`;
  - a dry run on `desktop-gpui-v0.0.0-alpha.0` through `workflow_dispatch` (D9), stopped before `publish` or with the draft deleted, and the tag deleted afterwards.

### Task 7 — Docs and alpha wording  *(parallel with 6)*

**Files:**
- `clients/desktop/RELEASING.md` (new): one tag push; recovery means deleting the draft and re-running through dispatch; never edit a published release; M2 sections stubbed.
- `docs/user/desktop.md`: install per format, runtime requirements, upgrading from beta.5, known gaps.
- `README.md`: "what works".
- `clients/desktop/README.md`: link to RELEASING.

**Release banner (owner approves the wording):** "**Alpha of the new native Dilla desktop app.** Installing the deb, rpm or AUR package replaces Dilla beta. Your old app data stays on disk but is not used. Dilla signs in as a new device, so keep another signed-in device at hand to link it. It does not update itself yet; download new alphas from this page."

**Runtime requirements section:**
- A Secret Service (GNOME Keyring, KWallet or KeePassXC). Without one, Dilla cannot store its database key and cannot sign in.
- Vulkan or OpenGL ES 3.
- PipeWire or PulseAudio for calls.
- glibc 2.39+.

- [ ] Verify: run `node scripts/check-ui-copy.mjs` if it covers docs, and check the links by hand.

### Task 8 — Cut `desktop-gpui-v0.2.0-alpha.1`  *(last; controller)*

- [ ] Merge the green PR (Tasks 1–7), then wait for a green `desktop-gpui` push run on the merge commit.
- [ ] `git tag desktop-gpui-v0.2.0-alpha.1 <merge sha> && git push origin desktop-gpui-v0.2.0-alpha.1`. Act on the first red job (fail-fast rule).
- [ ] Verify the published release:
  - `gh release download` and `sha256sum -c SHA256SUMS`;
  - `gh attestation verify <AppImage> --repo dilla-chat/dilla`;
  - the owner installs on CachyOS (AppImage, and `makepkg` from the AUR asset over an installed beta.5), signs in and links the device, posts a message and joins a call.
- [ ] Nothing goes to AUR (D4). There is no CT 144 upgrade, since there is no server change. Offer `/claude-obsidian:save`.

---

## Milestone 2 — signed feed, Mac, Windows, updater

Parallel lanes: **Tasks 9, 11, 12 and 13 run in parallel**. Task 10 needs 9. Task 14 needs 9 and 10. Task 15 is last.

### Task 9 — GPUI feed format and signer  *(parallel lane A)*

**Files:**
- `scripts/desktop-gpui-feed.mjs` and its test.
- `clients/desktop/release-trust.json` (new). It holds public pins only: `metadataKeys` = beta.5 key A's SPKI PEM from `keys.go`, and the `channel`.

**Interfaces:**
- **Document** (beta.5 format 1, so `desktopdownload.Verify` parses it):
  - fields: `{ format: 1, channel: "alpha", version, publishedAt, expiresAt (+90 d), assets: [{ name, size, sha512 }] }`;
  - canonical bytes: `JSON.stringify(doc) + "\n"`.
- **Signature:** Ed25519 over the exact bytes with `DESKTOP_METADATA_KEY_A` (D8), base64 in `<feed>.sig`.
- **Location:** `https://dilla-chat.github.io/dilla/gpui-alpha/desktop-gpui-alpha.json`.
- **API:** `build(assetsDir, version, channel) -> {json, sig}` and `verify(json, sig, pins)`.

**Steps:**
- [ ] Port from beta.5's `release-feed.mjs` and `feed-format.mjs`, without the Sparkle and sandbox parts.
- [ ] Go cross-test: a node-signed fixture verifies under `desktopdownload` once Task 11 lands (`internal/desktopdownload/testdata/`).
- [ ] Verify: `node --test scripts/desktop-gpui-feed.test.mjs`.

### Task 10 — Pages deploy that keeps beta.5's feed  *(after 9)*

**Files:** `scripts/desktop-gpui-pages.mjs` and its test.

**Interfaces:**
- `build(signedDir, outSite)`:
  - fetches the live `beta/` files (the list comes from beta.5's `pages-feed.mjs`);
  - verifies `beta/desktop-beta.json` under the pinned key and refuses on failure, unless the feed has expired, which it reports and keeps as is;
  - copies every file byte for byte;
  - adds `gpui-alpha/…`.
- `verify(signedDir)` re-fetches after the deploy.

- [ ] Verify: a unit test with a fake fetch, and a dry-run `diff` of the live `beta/` against the built site.

### Task 11 — `dillad --desktop-downloads-channel`  *(parallel lane B; server; D7)*

**Files:**
- `internal/desktopdownload/download.go` and `download_test.go`
- `cmd/dillad/serve.go` and `cmd/dillad/serve_downloads_test.go`
- `internal/dillad/downloads_routes_test.go`
- the operator doc for the flag

**Interfaces:**
- `type Channel struct{ Name, FeedURL, TagPrefix string; Version *regexp.Regexp; Platforms map[string]string }` with `var Beta, GPUIAlpha Channel`.
- `Options.Channel`: the zero value is `Beta`, so behaviour today is unchanged.
- The `--desktop-downloads-channel=beta|gpui-alpha` flag defaults to `beta`.
- `GPUIAlpha`:
  - tag prefix `desktop-gpui-v`, version `^\d+\.\d+\.\d+-alpha\.\d+$`;
  - the same `dilla-<v>-` asset names;
  - platforms as today, plus `linux-tar`, with `win-x64` → `win-x64-setup.exe`.
- `Verify` requires `doc.Channel == ch.Name`.

**Steps:**
- [ ] TDD:
  - a cross-channel replay is refused in both directions;
  - redirects stay under `releases/download/desktop-gpui-v<ver>/`;
  - an unknown channel flag fails at startup.
- [ ] Verify: `go test ./internal/desktopdownload/... ./cmd/dillad/... ./internal/dillad/...`. It ships in the next `v*` server release, then the Homelab session upgrades CT 144 and sets `--desktop-downloads-channel=gpui-alpha` once Task 15's feed is live.

### Task 12 — macOS app bundle, DMG, notarization  *(parallel lane C)*

**Files:**
- `clients/desktop/packaging/macos/Info.plist.in`:
  - `CFBundleIdentifier=dev.dilla.desktop`, `CFBundleName=Dilla`, `CFBundleExecutable=dilla-desktop`;
  - `LSMinimumSystemVersion=13.0`;
  - `NSMicrophoneUsageDescription`, `NSCameraUsageDescription`;
  - the version keys.
- `clients/desktop/packaging/macos/entitlements.plist`: audio-input and camera, with the hardened runtime.
- `scripts/desktop-gpui-package-mac.sh`

**Interface:** `desktop-gpui-package-mac.sh <version> <binary> <out-dir>` writes `dilla-<v>-mac-arm64.dmg`, plus `-mac-arm64.zip` for the updater, containing `Dilla.app`.

With `CSC_LINK`:
1. Import the certificate into a temporary keychain.
2. Sign the binary, then the .app: `codesign --options runtime --timestamp --entitlements … --sign "Developer ID Application"`.
3. Create the DMG (`hdiutil create -format ULFO`), then sign it.
4. `xcrun notarytool submit --key AuthKey.p8 --key-id $APPLE_API_KEY_ID --issuer $APPLE_API_ISSUER --wait`.
5. `stapler staple` the DMG and the .app.
6. `spctl -a -t open --context context:primary-signature` must pass.

Without `CSC_LINK`, the script signs ad hoc.

**Steps:**
- [ ] Build the bundle from `Dilla.icns`. Same bundle id and team as beta.5, so drag-replacing `/Applications/Dilla.app` works, and macOS permissions for that bundle id stay.
- [ ] Verify on `macos-15`, unsigned in a PR: `hdiutil attach`, `Dilla.app/Contents/MacOS/dilla-desktop --version`, `codesign -dv`.

### Task 13 — Windows installer  *(parallel lane D)*

**Files:** `clients/desktop/packaging/windows/dilla.iss` (Inno Setup, D10) and `scripts/desktop-gpui-package-win.ps1`.

**Interfaces:**
- Install: per-user to `%LOCALAPPDATA%\Programs\Dilla`, `PrivilegesRequired=lowest`.
- Start-menu shortcut with `AppUserModelID=dev.dilla.desktop`.
- Uninstaller, with `/VERYSILENT /SUPPRESSMSGBOXES` support for the updater.
- No `dilla:` protocol (D12).
- **beta.5 takeover:** before installing, read beta.5's NSIS uninstall entry under `HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\` (electron-builder's GUID for `dev.dilla.desktop`) and run its uninstaller with `/S`. That keeps app data (`deleteAppDataOnUninstall: false`).
- Output: `dilla-<v>-win-x64-setup.exe`.
- Build with `RUSTFLAGS=-C target-feature=+crt-static`.

**Steps:**
- [ ] Use `iscc` (preinstalled on `windows-latest`; the script checks its version). Leave a disabled `signtool` hook (D10).
- [ ] Verify on `windows-latest`:
  - the beta.5 installer (from its release) is installed silently, then the new setup runs silently;
  - beta.5 is gone and `%APPDATA%\Dilla` is kept;
  - `dilla-desktop.exe --version` works;
  - `dumpbin /dependents` shows no `VCRUNTIME140.dll`;
  - a silent uninstall leaves no program files.

### Task 14 — Keyed workflow `desktop-gpui-sign.yml` + in-app updater  *(after 9 and 10; the updater runs parallel with 12 and 13)*

**14a Workflow files:**
- `.github/workflows/desktop-gpui-sign.yml` (new)
- `desktop-gpui-release.yml`: now leaves the release as a **draft** once the Linux assets are attached
- `scripts/check-ci-workflow.mjs` and its test
- `scripts/desktop-gpui-gate.mjs` and its test

**14a Jobs:**
1. Trigger: `on: workflow_run [desktop gpui release] completed`.
2. **`gate`:**
   - first attempt only;
   - re-derives the tag, commit and green run from the API;
   - the draft was made by `github-actions[bot]`;
   - the draft's Linux asset digests equal the attestation subjects (`gh attestation verify`).
3. **`build-mac`** (macos-15, environment `desktop-release`): runs the Task 12 script. The `CSC_*` and `APPLE_*` secrets reach that step only.
4. **`build-win`** (windows-latest, keyless).
5. **`sign`** (`desktop-release`): runs Task 9 over every asset with `DESKTOP_METADATA_KEY_A` only.
6. **`publish`:**
   - uploads the Mac and Windows assets;
   - completes SHA256SUMS and re-verifies;
   - takes the release out of draft.
7. **`feed`** (`github-pages`, concurrency group `desktop-release-feed`): runs Task 10.

There is no AUR job (D4).

**14b Updater files:**
- `clients/desktop/dilla-desktop/src/updater/{mod.rs,feed.rs,apply_linux.rs,apply_mac.rs,apply_win.rs}`
- Cargo dependencies: `ed25519-dalek`, `sha2`, `semver` (all in the lock).
- Strings in `packages/web/src/strings/`, following beta.5's update UI if the parity set has one; otherwise ask the owner.

**14b Interfaces:**
- **`check(now) -> Result<Option<Update>>`:**
  - fetches the feed (≤1 MiB) and the `.sig` (≤128 B);
  - verifies against the `release-trust.json` pins (`include_str!`);
  - checks `channel` and `expiresAt`;
  - accepts only a version above the current one by semver, never a downgrade.
- **`download(&Update) -> Result<PathBuf>`:** the file's size and sha512 must equal the feed's.
- **`apply(path)`:**
  - **Linux:** only for an AppImage (`$APPIMAGE` set and writable): atomic rename beside it, then re-exec. deb, rpm, AUR and tar.gz installs show "a new version is available" instead.
  - **macOS:** unzip, then `SecStaticCodeCheckValidity` with the requirement `anchor apple generic and certificate leaf[subject.OU] = "<TEAM_ID>"`, an atomic bundle swap, and a relaunch.
  - **Windows:** run the setup with `/VERYSILENT` and exit.
- **When:** at start and every 6 h.

**14 Verify:**
- `cargo test -p dilla-desktop updater::`: bad signature, tampering, downgrade, expiry, wrong channel, size, hash.
- The check-ci tests.
- End to end, alpha.N-1 → alpha.N on all three OSes (Task 15).

### Task 15 — Cut the first signed GPUI release  *(last; controller)*

- [ ] PR, merge, green run, tag `desktop-gpui-v0.2.0-alpha.N`. Watch both workflows.
- [ ] Verify:
  - the feed verifies with `node scripts/desktop-gpui-feed.mjs verify`;
  - the live `beta/` bytes are unchanged;
  - `spctl -a -vv` passes on the DMG, and it opens with no Gatekeeper prompt on a clean Mac (dilla-MacBook session);
  - the Windows install works, with the SmartScreen step documented;
  - the updater moves alpha.N-1 → alpha.N on all three OSes.
- [ ] Ship the server release with Task 11. The Homelab CT 144 upgrade follows, with `--desktop-downloads-channel=gpui-alpha` (D7).
- [ ] Docs: the M2 sections of RELEASING, Mac and Windows in `docs/user/desktop.md`, and the README.
