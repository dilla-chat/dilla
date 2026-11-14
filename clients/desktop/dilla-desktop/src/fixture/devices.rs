//! The devices facet of a fixture: the viewer's device list (Settings -> Devices, Task 16).
//!
//! The web shooter's viewer signed up on one device and signs in on a fresh one (see `diagnostics.rs`), so the account
//! lists two live devices: this one (the shooter's, first in the list) and the sign-up device. Their ids are derived
//! from the viewer's, as the fixture's other ids are. This one is the shooter's browser, "Chrome on <os>" with no
//! platform tag, as the web names it from the user agent.

use dilla_client::state::AppState;
use dilla_client::state::DeviceId;
use dilla_client::state::profile::DeviceEntry;
use dilla_client::testing::spec::{WorldSpec, derive_id};

/// Maps the facet into `state`.
pub(super) fn apply(_world: &WorldSpec, viewer: &str, state: &mut AppState) {
    let this = DeviceId(derive_id("device", &format!("{viewer}/this")));
    let signup = DeviceId(derive_id("device", &format!("{viewer}/signup")));
    let entry = |id: DeviceId, own: bool| DeviceEntry {
        id,
        tier: 1,
        signer_tier: 1,
        last_seen: 0,
        revoked_at: None,
        label: None,
        listed: true,
        own,
    };
    state.profile.devices = vec![entry(this, true), entry(signup, false)];
    state.profile.devices_loaded = true;
    // The reference's own device is the shooter's Chromium (shoot.mjs), which names itself from its user agent.
    state.profile.fixture_browser = Some("Chrome".to_owned());
}
