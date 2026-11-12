//! The app links dilla-media, whose LiveKit libwebrtc needs `-ObjC` on macOS to keep its Objective-C
//! categories; a dependency's `rustc-link-arg` does not reach this package's binary, so it is set here.
fn main() {
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("macos") {
        println!("cargo:rustc-link-arg=-ObjC");
    }
}
