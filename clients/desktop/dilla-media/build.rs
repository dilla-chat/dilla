//! LiveKit's prebuilt libwebrtc defines Objective-C categories (e.g. `+[NSString stringForStdString:]`),
//! which the macOS linker keeps in the final binary only with `-ObjC`. webrtc-sys's build script emits it,
//! but cargo applies a dependency's `rustc-link-arg` only to that package's own targets, so every binary and
//! test here that creates a PeerConnection needs it too. (.cargo/config.toml may not set rustflags.)
//!
//! On Linux the desktop capturer of the same libwebrtc (the screen and window sources, `sources::desktop`) talks to the
//! XDG desktop portal through GIO: webrtc-sys compiles against GLib's headers only, so the libraries are linked here.
//! (`--as-needed`: a binary that never captures the screen gets no GLib dependency.)
fn main() {
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("linux") {
        for lib in ["gio-2.0", "gobject-2.0", "glib-2.0"] {
            println!("cargo:rustc-link-lib=dylib={lib}");
        }
    }
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("macos") {
        println!("cargo:rustc-link-arg=-ObjC");
    }
}
