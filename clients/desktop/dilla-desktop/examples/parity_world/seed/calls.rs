//! Seeds the calls of a fixture into the real dillad. Owned by Tasks 17 and 18; lane G (Task 28 wave 2) seeds them.
//!
//! Who else is in a call, and a ring, come from dilla-testhost's parity-fixture routes (`/debug/parity/voice` and
//! `/debug/parity/ring`, cmd/dilla-testhost/parity_calls.go): the op 50 voice_state and op 53 call_ring frames dillad
//! sends, delivered to the browser's live connection, with no media peer and no SFU room behind them. They are what the
//! web client draws from (the voice row's people, a channel's preview, the ring banner); the device ids are the GPUI
//! mapping's (`fixture/calls.rs`: the account id with its last byte flipped) and a call's id is its channel's.
//!
//! The viewer's own place in a call is the browser's: the fixture's steps join it (Join voice, or the DM's call button),
//! so the host then runs its LiveKit ([`needs_sfu`]). A DM call without the viewer rings the viewer from its first
//! participant.

use std::collections::BTreeMap;

use dilla_client::testing::host::{self as world, Host};
use dilla_client::testing::spec::{ParticipantSpec, ScreenFixture};

/// voice_state's flag bits (02 § voice_state, `dilla_client::state::voice::flags`).
const IN_CALL: u8 = 1;
const SELF_MUTE: u8 = 2;
const SELF_DEAF: u8 = 4;
const SCREEN: u8 = 16;

/// Every call of a fixture can be seeded: a DM's needs the DM (the dms facet refuses what it cannot make).
pub(super) fn unseedable(_f: &ScreenFixture, _out: &mut Vec<String>) {}

/// Whether the viewer is in one of the fixture's calls: the browser joins it, through the host's LiveKit.
pub(super) fn needs_sfu(f: &ScreenFixture) -> bool {
    f.world
        .calls
        .iter()
        .any(|c| c.participants.iter().any(|p| p.person == f.viewer))
}

/// The device a fixture person is in a call on: their account id with its last byte flipped (`fixture/calls.rs`).
fn device_of(user_hex: &str) -> String {
    let mut bytes = dilla_client::testing::spec::unhex16(user_hex).expect("hex");
    bytes[15] ^= 0xd7;
    world::hex(&bytes)
}

fn flags_of(p: &ParticipantSpec) -> u8 {
    let mut f = IN_CALL;
    if p.muted {
        f |= SELF_MUTE;
    }
    if p.deafened {
        f |= SELF_DEAF | SELF_MUTE;
    }
    if p.sharing {
        f |= SCREEN;
    }
    f
}

async fn post(host: &Host, path: &str, body: serde_json::Value) {
    let status = reqwest::Client::new()
        .post(format!("{}{path}", host.control))
        .header("content-type", "application/json")
        .body(body.to_string())
        .send()
        .await
        .unwrap_or_else(|e| panic!("{path}: {e}"))
        .status();
    assert!(status.is_success(), "{path}: {status}");
}

/// Seeds the calls once the history is posted, while the browser is online.
pub(super) async fn seed(f: &ScreenFixture, host: &Host, ids: &BTreeMap<String, String>) {
    let viewer = ids[&format!("person:{}", f.viewer)].clone();
    for call in &f.world.calls {
        let key = match call.channel.strip_prefix("dm/") {
            Some(dm) => format!("dm:{dm}"),
            None => format!("channel:{}", call.channel),
        };
        let channel = ids[&key].clone();
        let others: Vec<&ParticipantSpec> = call
            .participants
            .iter()
            .filter(|p| p.person != f.viewer)
            .collect();
        let rows: Vec<serde_json::Value> = others
            .iter()
            .map(|p| {
                let user = ids[&format!("person:{}", p.person)].clone();
                serde_json::json!({"user": user, "device": device_of(&user), "flags": flags_of(p)})
            })
            .collect();
        if !rows.is_empty() {
            post(
                host,
                "/debug/parity/voice",
                serde_json::json!({"to": [viewer], "channel": channel, "call": channel, "participants": rows}),
            )
            .await;
        }
        let viewer_in = call.participants.iter().any(|p| p.person == f.viewer);
        if call.channel.starts_with("dm/")
            && !viewer_in
            && let Some(caller) = others.first()
        {
            let now = tokio::task::block_in_place(|| world::clock_now(host));
            post(
                host,
                "/debug/parity/ring",
                serde_json::json!({
                    "to": viewer,
                    "call": channel,
                    "channel": channel,
                    "caller": ids[&format!("person:{}", caller.person)],
                    "expires": now + 30,
                }),
            )
            .await;
        }
    }
}
