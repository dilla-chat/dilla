# Manual checks (what CI cannot reach)

CI builds and tests the desktop client on Linux, macOS and Windows, but only Linux runs a real call. These checks
need a person at a real machine. Record the date, the OS version and the result in the parity PR.

## Screen share and camera (parity plan Package 1)

`cargo run -p dilla-media --example capture-probe -- <what>` lists the sources and captures the first one for three
seconds, printing the frame size; `ended` means the source closed.

| Check | macOS 13+ (ScreenCaptureKit) | Windows 10/11 (Windows Graphics Capture) | Linux X11 | Linux Wayland (KDE Plasma: Dilla's picker through KWin; elsewhere the portal) |
|---|---|---|---|---|
| `capture-probe screens` lists every display, each with a 320 x 180 thumbnail, and frames arrive | the first run asks for Screen Recording; thumbnails on 14+, one frame on 13 | no prompt | no prompt | KDE: no prompt; elsewhere n/a (`portal`) |
| `capture-probe windows` lists the open windows by title, frames arrive; closing the window prints `ended` | the first run asks for Screen Recording; thumbnails on 14+, one frame on 13 | the capture border appears around the window | — | KDE: no panels, no desktop, no minimised window, no Dilla window; elsewhere n/a |
| `capture-probe portal` opens the system's "share your screen" dialog; Share gives frames, Cancel prints `did not open: the selection was cancelled` | n/a | n/a | n/a | GNOME, a Flatpak or Snap, and KWin 6.0-6.7 without the installed package (KDE otherwise: n/a) |
| `capture-probe camera` lists the cameras and frames arrive (about 640 x 480) | no camera source yet (follow-up) | the camera light comes on, and goes off at the end | V4L2 camera | V4L2 camera |
| In a call: Share, then pick a screen; the other side sees it, and your own tile shows your share; Stop ends it (your own tile's picture goes with it), and closing the window ends it | | | | |
| In a call: Share opens Dilla's picker; without Screen Recording it shows the permission line, and after allowing and relaunching it lists the tiles | | n/a | n/a | n/a |
| KDE Plasma (KWin 6.8+, or 6.0-6.7 with the deb, rpm or AUR installed): Go live opens Dilla's picker (screens and windows with thumbnails), never the system's dialog; sharing a window brings its application's sound with no question before (the live panel's sound row says which); a still window keeps its picture for the others, also after a quality change in the live panel; closing the shared window ends the share; `cargo test -p dilla-media --test capture -- --ignored kde_picker_lists_captures_and_lets_go` passes. On KWin 6.0-6.7 (unverified: whether KWin keeps to the frame rate Dilla asks for): share a busy 4K screen at 30 fps and check in `top` that Dilla's CPU stays near what it is on 6.8, not several times higher | n/a | n/a | n/a | |
| KDE Plasma: with the picker open for 10 s (it takes a thumbnail of every source every 2 s), and again while sharing, note whether Plasma's own "screen is being recorded" indicator (the system tray's camera or screencast icon) flashes or stays; it must be gone a few seconds after the picker closes or the share stops | n/a | n/a | n/a | |
| The system's dialog (GNOME, a Flatpak or Snap, KWin 6.0-6.7 with the AppImage or tar.gz): the panel before a share has no sound row; Go live opens the dialog, which asks every time (no "remember"); after the pick the share panel opens by itself with the sound row | n/a | n/a | n/a | |
| The share panel (Share, or the chevron while sharing) opens above the Share button, its right edge on the button's, and stays inside a narrow window | | | | |
| In a call: the camera button starts the camera; the other side sees it, and your own tile shows it mirrored (the other side sees it the right way round) | n/a | | | |

## Application audio (parity plan Package 3)

| Check | macOS 14.2+ | Windows 10/11 | Linux (PipeWire) |
|---|---|---|---|
| `capture-probe apps` lists the applications playing sound, never Dilla itself | each playing application once (before 14.2: `not available on this system`) | each playing application once (before build 19041: `not available on this system`) | each playing application once |
| In a call, while sharing, the share menu shows the sound row ("Also share an application’s sound"); on, it lists the playing applications (the panel before a share has none). A choice there holds for that share only: the next window share brings its own application's sound, the next screen share none | shown on 14.2+, absent before | shown on build 19041+, absent before | shown |
| `cargo test -p dilla-media --test app_audio -- --ignored` (two `pw-play` into a null sink; the listener hears only the chosen one) | n/a | n/a | passes with a PipeWire session |

## Calls with the Bot

| Check | Any OS |
|---|---|
| Two clients plus the Bot: both join a voice channel, start music (the Bot joins), one shares its screen; then the sharer's network drops (Wi-Fi off) and comes back, and it joins voice again. With the other person still in the call it is back in within seconds; with only the Bot left it is back in a fresh call (the music stopped) once the server ended the old one: usually about a minute, up to about three minutes when the old connection keeps trying to come back. Meanwhile **join voice** waits about 30 seconds, then says "Could not join voice. Try again."; press **join voice** again | |
