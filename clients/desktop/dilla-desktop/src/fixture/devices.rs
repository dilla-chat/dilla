//! The devices facet of a fixture: the viewer's device list (Settings -> Devices, Task 16).
//!
//! The web shooter's viewer signed up on one device and signs in on a fresh one (see `diagnostics.rs`), so the account
//! lists two live devices: this one (the shooter's, first in the list) and the sign-up device. Their ids are derived
//! from the viewer's, as the fixture's other ids are. This one is the shooter's browser, "Chrome on <os>" with no
//! platform tag, as the web names it from the user agent.
//!
//! The docs' native fixtures ([`DESKTOP_ONLY`]) draw what the native app shows instead: no shooter's browser, so this
//! device names itself ("Dilla on <os>") and a profile card carries no `WEB` tag, and the sign-up device has a name.

use dilla_client::state::AppState;
use dilla_client::state::DeviceId;
use dilla_client::state::profile::DeviceEntry;
use dilla_client::testing::spec::{WorldSpec, derive_id};

/// The docs' native fixtures, drawn as the native app is with no web reference behind them.
pub const DESKTOP_ONLY: [&str; 3] = [
    "user-settings/devices-named",
    "user-settings/devices-sign-out-named",
    "members/profile-card-native",
];

/// Maps the facet into `state`.
pub(super) fn apply(name: &str, _world: &WorldSpec, viewer: &str, state: &mut AppState) {
    let this = DeviceId(derive_id("device", &format!("{viewer}/this")));
    let signup = DeviceId(derive_id("device", &format!("{viewer}/signup")));
    let native = DESKTOP_ONLY.contains(&name);
    let entry = |id: DeviceId, own: bool, label: Option<&str>| DeviceEntry {
        id,
        tier: 1,
        signer_tier: 1,
        last_seen: 0,
        revoked_at: None,
        label: label.map(str::to_owned),
        listed: true,
        own,
    };
    let other = native.then_some("Ada's laptop");
    state.profile.devices = vec![entry(this, true, None), entry(signup, false, other)];
    state.profile.devices_loaded = true;
    if native {
        return;
    }
    // The reference's own device is the shooter's Chromium (shoot.mjs), which names itself from its user agent.
    state.profile.fixture_browser = Some("Chrome".to_owned());
}
