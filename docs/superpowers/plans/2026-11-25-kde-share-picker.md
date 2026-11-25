# Dilla's own share picker on KDE Wayland, and the share panel anchored to its button: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan.

**Goal:** on KDE Plasma (Wayland), Share opens Dilla's own picker. It's the same grid and thumbnails that X11, Windows
and macOS show, so sharing a window brings that application's sound by itself (D21) and no sound question comes before
the pick. Elsewhere on Wayland, the system dialog stays. Also: the share panel opens from the Share button, not centred
on the stage.

**Spec and decisions:**
- **Jonas, 2026-11-25:**
  - "why not use the same as macos for all platforms?";
  - after the spike: "Build it, portal fallback";
  - KDE's dialog, where it remains, is not remembered ("No, always ask");
  - alpha.3 waits for this work.
- **D21, 2026-11-19 (docs/superpowers/plans/2026-11-19-gpui-gui-gaps.md:92-100):**
  - a window brings its app's sound;
  - a screen brings none (manual choice);
  - the person's own choice is never replaced;
  - Dilla's own process tree is never captured.
- **D22:** Dilla's own picker, as on macOS.

**Spike evidence** (read it in full):
/tmp/claude-1000/-home-thim-Repositories-dilla/da223a73-3d7b-43a1-bad9-8a8d719639d6/scratchpad/kde-spike-report.md and
.../scratchpad/kde-spike/picker/src/main.rs. On KWin 6.8.2:
- **Globals offered:** `org_kde_plasma_window_management` (v21) and `zkde_screencast_unstable_v1` (v6) are offered to
  any client that isn't sandboxed (Flatpak, Snap, or `wp_security_context_v1`).
- **Window list:** 9 windows, each with app id and pid.
- **Screen capture:** about 58 fps at 3840x2160 (AR24).
- **Window capture:** sends a frame only when the window changes, so the last frame has to be repeated.
- **Linking:** WirePlumber didn't auto-link the consumer. Dilla must link the ports itself, as app_audio/linux.rs does.
- **KWin 6.0-6.7:** only trusts a client whose installed `.desktop` file names its binary in `Exec=` and lists the
  interfaces in `X-KDE-Wayland-Interfaces=`.

## Global Constraints

- **Where the own picker applies:** Linux on Wayland when the compositor offers both
  `org_kde_plasma_window_management` and `zkde_screencast_unstable_v1`, detected at runtime by binding the registry,
  never by the desktop name alone. Otherwise `desktop::portal()` stays true and today's portal path is unchanged.
  `portal()` becomes this runtime check (cached once per process).
- **Picker:** the same `share_picker` UI as X11 and Windows, with screens and windows, titles and thumbnails.
  - Exclude Dilla's own windows (by pid tree, as `sources()` does today).
  - Exclude panels, the desktop, skip-taskbar and minimised-only windows. The spike report lists the window state
    flags to use.
  - Titles are shown only in the picker. They're never logged, never in Debug output, never in test output.
- **Thumbnails:** use one short screencast stream per source, or a single frame, capped in size like `fit_rgba`. Close
  every stream the picker opened when it closes. No stream may outlive the picker except the chosen one.
- **Capture:**
  - The chosen output or window becomes a PipeWire stream, read through the existing `pipewire` 0.9.2 dependency.
  - The consumer is linked by Dilla itself.
  - Frames are converted into the same `Frame` type that `desktop::open` produces, so dilla-media's share pipeline
    (scaling, the self-view tap, the fps cap) is unchanged.
  - For a window, repeat the last frame at the share's frame rate when nothing changes.
  - The stream ends and the share stops with `ended` when the window closes.
- **Sound (D21):**
  - A window share looks up the window's pid through the window list, and the app's sound follows through the existing
    `apps::window_sound` / `share_source` path. A screen share brings none.
  - The person's own choice is never replaced.
  - With the own picker there is no sound question before the pick. The sound row stays in the live panel.
- **Portal path (fallback) order fix:** where the system dialog is still used (GNOME, older or sandboxed KDE), the
  pre-share panel has no sound row. After the dialog's pick, the live share panel opens by itself with the sound row.
  The dialog is not remembered (no restore token).
- **Older KWin trust (6.0-6.7):**
  - Add `X-KDE-Wayland-Interfaces=org_kde_plasma_window_management,zkde_screencast_unstable_v1` to
    `chat.dilla.Dilla.desktop`, shipped by the deb, rpm and AUR packages.
  - Check that its `Exec=` matches the installed binary path (`/opt/Dilla/dilla-desktop` behind a `/usr/bin` symlink).
    KWin compares the resolved executable, so state which path it needs.
  - The AppImage and tar.gz fall back to the portal on 6.0-6.7.
- **Consent:**
  - The own picker is the only consent step, so it must say plainly what will be shared, as it does on Windows and
    macOS.
  - The live indicators (LIVE badge, Stop sharing) are unchanged.
  - Capture never starts before the person picks a source in Go live.
- **Crates:** use the wayland-client / wayland-protocols-plasma families, or generate from the protocol XML if the
  published crates lack v21/v6. Pin exact versions in Cargo.lock. Linux-only (`cfg(target_os = "linux")`). Builds on
  macOS and Windows must not pull them in.
- **Share panel anchoring:** the share panel (`share_menu`, dilla-desktop/src/screens/voice/mod.rs ~5007) opens
  attached to the Share button: above it, right-aligned to the button's right edge, clamped inside the window. Copy how
  other anchored menus in the app do this. If none does, follow the old client's SharePicker/CallBar CSS
  (packages/web). The same goes for the phone layout, if it has a share panel.
- **Tests and docs:**
  - Unit-test the source filtering, the runtime protocol detection (fake registry), the frame conversion and repeat,
    and the anchoring geometry.
  - Add a live-capture test that is `#[ignore]` by default and needs a KWin session.
  - Update MANUAL.md (a KDE row for the own picker, plus the portal-fallback row) and docs/user/desktop.md (sharing on
    KDE, and the GNOME fallback).

## Review Focus
1. Capture can never start without the person's pick, and no stream stays open after the picker or the share ends.
2. A compositor that offers only one of the two globals, or an old version of either, falls back to the portal cleanly.
3. Thumbnails of many windows don't spike memory or leave PipeWire nodes behind.
4. Window titles and pixels never reach logs or test output.
5. The macOS and Windows builds don't change (cfg gating), and X11 is unchanged.

### Task 1: KDE source list and capture in dilla-media
Files: a new `clients/desktop/dilla-media/src/sources/desktop_kde.rs`, plus wiring in `sources/desktop.rs` (`portal()`,
`list`, `snapshot`, `thumbnail`, `open`, `window_owner_pid`) and Cargo.toml / Cargo.lock. Tests go inline or in
dilla-media/tests. Commit: `media: Dilla lists and captures screens and windows itself on KDE Wayland`.

### Task 2: the picker on KDE, the sound order, and the anchored panel
Files: dilla-desktop's share_picker and voice/mod.rs (`share_menu`, `toggle_share`, the portal branch), plus tests.
Commit: `desktop: Dilla's own share picker on KDE, the sound after the pick on the portal, the share panel at its button`.

### Task 3: packaging and docs
Files: the desktop entry source (find it, chat.dilla.Dilla.desktop, and the scripts that build deb/rpm/AUR), MANUAL.md,
docs/user/desktop.md. Commit: `desktop: KDE trusts the installed package with the share protocols; docs`.
