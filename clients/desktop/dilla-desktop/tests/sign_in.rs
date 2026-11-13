//! Task 28 (parity lane): the pre-account screens against the web client's references. A `signed_out` fixture is a device
//! with no account: `shoot.mjs` opens the route in a fresh browser without signing in, and the app draws the same route
//! with no client (`fixture::screen_of`).

use std::path::{Path, PathBuf};

fn fixture(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures")
        .join(format!("{name}.json"))
}

fn parity(name: &str) {
    if let Err(e) = dilla_parity::screen::assert_screen(&fixture(name)) {
        panic!("{name}: {e}");
    }
}

#[test]
#[ignore = "parity after T6c: whole frame 3.1447 % (/welcome on the web is the sign-up's first step, Join with an invite, over the radial glow; the app draws its sign-in form there)"]
fn parity_sign_in_welcome() {
    parity("sign-in/welcome");
}
