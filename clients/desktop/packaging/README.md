# Packaging inputs of the native desktop app

`scripts/desktop-gpui-package-linux.sh` builds the Linux release assets from these files (see `../RELEASING.md`).

## linux/

- `dev.dilla.desktop.desktop`: the desktop entry. `StartupWMClass` and the file name equal `APP_ID`
  (`dilla-desktop/src/lib.rs`), which Wayland matches the window's icon by. No `MimeType`: `dilla://` links are not
  handled yet (release plan D12). Check with `desktop-file-validate`.
- `dev.dilla.desktop.metainfo.xml`: the AppStream metadata. Check with `appstreamcli validate --no-net` (appimagetool
  runs it too).
- `AppRun`: the AppImage's entry point, the binary alone (nothing is bundled, no `LD_LIBRARY_PATH`).
- `nfpm.yaml`: the deb and rpm, package `dilla` (beta.5's name, so they upgrade it in place), with no maintainer
  scripts. `usr-bin-dilla-desktop` is their `/usr/bin/dilla-desktop` and says why it is a script and not a link;
  `copyright` is the deb's `/usr/share/doc/dilla/copyright`.
- `icons/hicolor/`: the app icon. The PNGs were made once from `../assets/brand/icon.png` (512 px) with ImageMagick 7,
  and `scalable/` and `symbolic/` are copies of `../assets/brand/linux.svg` and `linux-symbolic.svg`:

  ```sh
  cd clients/desktop
  for s in 16 22 24 32 48 64 128 256 512; do
    magick assets/brand/icon.png -filter Lanczos -resize ${s}x${s} -strip \
      -define png:exclude-chunks=date,time PNG32:packaging/linux/icons/hicolor/${s}x${s}/apps/dev.dilla.desktop.png
  done
  ```

## Licences shipped beside the binary

`scripts/desktop-third-party.mjs` writes the `licenses/` folder of every package (`/opt/Dilla/licenses/` in the deb,
rpm and Arch package, `usr/share/licenses/dilla/` in the AppImage):

- `LICENSE`: Dilla's own (AGPL-3.0-or-later).
- `OFL-JetBrainsMono.txt`, `OFL-DMSerifDisplay.txt`: the embedded fonts (`dilla-desktop-ui/assets/fonts/`).
- `libwebrtc-LICENSE.md`: LiveKit's prebuilt libwebrtc, linked statically, from its pinned archive.
- `THIRD-PARTY.md`: every crate linked into `dilla-desktop` on x86_64 Linux, from `cargo metadata --locked
  --filter-platform x86_64-unknown-linux-gnu` (normal dependencies only), with its licence and repository. A crate
  that declares no licence fails the build.

## windows/

`scripts/desktop-gpui-package-win.ps1` builds `dilla-<v>-win-x64-setup.exe` from `windows/dilla.iss` (Inno Setup 6, run by
`iscc`, preinstalled on `windows-latest`):

- Per user, no elevation, to `%LOCALAPPDATA%\Programs\Dilla` (beta.5's folder). The Start-menu shortcut carries
  AppUserModelID `dev.dilla.desktop`, the app's `APP_ID`, which Windows needs for toasts. No `dilla:` protocol (D12).
- `/VERYSILENT /SUPPRESSMSGBOXES` installs; `unins000.exe` with the same flags removes everything it installed. User data
  (`%APPDATA%\Dilla` from beta.5, `Dilla Preview` for this app) is never touched.
- The beta.5 takeover: before copying files, `PrepareToInstall` reads beta.5's uninstall entry under
  `HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\f6db60e4-10df-5b69-9206-408d5f2cebe9` (electron-builder's UUID v5 of
  `dev.dilla.desktop`, derived in `scripts/desktop-gpui-package-win.test.mjs`) and runs its uninstaller the way electron-builder's
  own installer does on an update (`/S /KEEP_APP_DATA /currentuser --updated _?=<dir>`).
- Unsigned for the alpha (D10); users meet SmartScreen. `DILLA_SIGNTOOL_COMMAND` switches the disabled signtool hook on.
- The binary is built with `RUSTFLAGS="-C target-feature=+crt-static"`; the script fails if `dumpbin /dependents` lists a
  Visual C++ runtime. The `gpui-win-package` job in `desktop-gpui.yml` installs beta.5, runs the setup over it and uninstalls.
