//! The boot fixtures (Package 10d): `phase: revoked` has the viewer's own headless device remove every other device of the
//! account (the browser that signed in) after the history, so the browser's next start is refused and it shows the
//! revoked splash. `phase: store_lost` needs nothing here: the shooter deletes the browser's key wrapper itself.

use std::time::Duration;

use dilla_client::Client;
use dilla_client::testing::host::{Person, wait_for};
use dilla_client::testing::spec::{Phase, ScreenFixture};

/// Removes the viewer's other devices (the browser) with the viewer's recovery key, for a `revoked` fixture.
pub async fn seed(f: &ScreenFixture, people: &[(String, Person)], clients: &[(&str, Client)]) {
    if f.phase != Phase::Revoked {
        return;
    }
    let viewer = &people
        .iter()
        .find(|(id, _)| *id == f.viewer)
        .expect("validated")
        .1;
    let client = &clients
        .iter()
        .find(|(id, _)| *id == f.viewer)
        .expect("the viewer's client")
        .1;
    let own = client.state().borrow().account.device_id;
    client.devices().refresh().await.unwrap().unwrap();
    let s = wait_for(client, Duration::from_secs(30), "the device list", |s| {
        s.profile.devices_loaded && s.profile.devices.len() >= 2
    })
    .await;
    let others: Vec<_> = s
        .profile
        .devices
        .iter()
        .filter(|d| Some(d.id) != own && d.revoked_at.is_none())
        .map(|d| d.id)
        .collect();
    for device in others {
        client
            .devices()
            .revoke(
                device,
                Some(zeroize::Zeroizing::new(viewer.recovery_key.clone())),
            )
            .await
            .unwrap()
            .expect("the viewer removes the browser's device");
    }
}
