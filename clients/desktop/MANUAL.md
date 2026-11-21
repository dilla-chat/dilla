# Manual checks (what CI cannot reach)

CI builds and tests the desktop client on Linux, macOS and Windows, but only Linux runs a real call. These checks
need a person at a real machine. Record the date, the OS version and the result in the parity PR.

## Screen share and camera (parity plan Package 1)

`cargo run -p dilla-media --example capture-probe -- <what>` lists the sources and captures the first one for three
seconds, printing the frame size; `ended` means the source closed.

| Check | macOS 14+ (ScreenCaptureKit) | Windows 10/11 (Windows Graphics Capture) | Linux X11 | Linux Wayland (portal) |
|---|---|---|---|---|
| `capture-probe screens` lists every display, each with a 320 x 180 thumbnail, and frames arrive | n/a (the system picker: `portal`) | no prompt | no prompt | n/a (`portal`) |
| `capture-probe windows` lists the open windows by title, frames arrive; closing the window prints `ended` | n/a | the capture border appears around the window | — | n/a |
| `capture-probe portal` opens the system's "share your screen" dialog; Share gives frames, Cancel prints `did not open: the selection was cancelled` | ScreenCaptureKit's picker (macOS 14+); the first run asks for Screen Recording permission (System Settings → Privacy & Security) | n/a | n/a | KDE and GNOME |
| `capture-probe camera` lists the cameras and frames arrive (about 640 x 480) | no camera source yet (follow-up) | the camera light comes on, and goes off at the end | V4L2 camera | V4L2 camera |
| In a call: Share, then pick a screen; the other side sees it; Stop ends it, and closing the window ends it | | | | |
| In a call: the camera button starts the camera; the other side sees it | n/a | | | |

## Application audio (parity plan Package 3)

| Check | macOS 14.2+ | Windows 10/11 | Linux (PipeWire) |
|---|---|---|---|
| `capture-probe apps` lists the applications playing sound, never Dilla itself | each playing application once (before 14.2: `not available on this system`) | each playing application once (before build 19041: `not available on this system`) | each playing application once |
| In a call, the share menu shows the sound row ("Also share an application’s sound"); on, it lists the playing applications | shown on 14.2+, absent before | shown on build 19041+, absent before | shown |
| `cargo test -p dilla-media --test app_audio -- --ignored` (two `pw-play` into a null sink; the listener hears only the chosen one) | n/a | n/a | passes with a PipeWire session |
