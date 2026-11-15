# Manual checks (what CI cannot reach)

CI builds and tests the desktop client on Linux, macOS and Windows, but only Linux runs a real call. These checks
need a person at a real machine. Record the date, the OS version and the result in the parity PR.

## Screen share and camera (parity plan Package 1)

`cargo run -p dilla-media --example capture-probe -- <what>` lists the sources and captures the first one for three
seconds, printing the frame size; `ended` means the source closed.

| Check | macOS 14+ (ScreenCaptureKit) | Windows 10/11 (Windows Graphics Capture) | Linux X11 | Linux Wayland (portal) |
|---|---|---|---|---|
| `capture-probe screens` lists every display, each with a 320 x 180 thumbnail, and frames arrive | the first run asks for Screen Recording permission (System Settings → Privacy & Security); grant it and run again | no prompt | no prompt | n/a (`portal`) |
| `capture-probe windows` lists the open windows by title, frames arrive; closing the window prints `ended` | as above | the capture border appears around the window | — | n/a |
| `capture-probe portal` opens the system's "share your screen" dialog; Share gives frames, Cancel prints `did not open: the selection was cancelled` | n/a | n/a | n/a | KDE and GNOME |
| `capture-probe camera` lists the cameras and frames arrive (about 640 x 480) | no camera source yet (follow-up) | the camera light comes on, and goes off at the end | V4L2 camera | V4L2 camera |
| In a call: Share, then pick a screen; the other side sees it; Stop ends it, and closing the window ends it | | | | |
| In a call: the camera button starts the camera; the other side sees it | n/a | | | |
