//! LiveKit's prebuilt libwebrtc defines Objective-C categories (e.g. `+[NSString stringForStdString:]`),
//! which the macOS linker keeps in the final binary only with `-ObjC`. webrtc-sys's build script emits it,
//! but cargo applies a dependency's `rustc-link-arg` only to that package's own targets, so every binary and
//! test here that creates a PeerConnection needs it too. (.cargo/config.toml may not set rustflags.)
fn main() {
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("macos") {
        println!("cargo:rustc-link-arg=-ObjC");
    }
}
