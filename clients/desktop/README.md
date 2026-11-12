# dilla desktop (GPUI preview)

Native Rust/GPUI desktop client — see `docs/superpowers/specs/2026-11-12-gpui-desktop-design.md`.

Crates: `dilla-client` (store, keychain, HTTP, gateway, sync, calls), `dilla-media` (LiveKit Rust SDK with
`dilla-sframe/1`), `dilla-desktop-ui` (Signal tokens, fonts, icons, components), `dilla-desktop` (the app),
`parity` (pixel-parity harness).

## Building

- **Linux:** clang 21 or newer (LiveKit's prebuilt libwebrtc ships a hermetic libc++ that needs it):
  `CC=clang-21 CXX=clang++-21 cargo run -p dilla-desktop`. X11 and Wayland dev packages: see
  `.github/workflows/desktop-gpui.yml`.
- **macOS:** nothing extra; `dilla-media` and `dilla-desktop` pass `-ObjC` to the linker in their build scripts.
- **Windows:** libwebrtc uses the static MSVC runtime, so build with
  `$env:RUSTFLAGS="-C target-feature=+crt-static"; cargo run -p dilla-desktop`
  (`.cargo/config.toml` deliberately carries no rustflags).
