//! Which gateway frames leave the sync engine, and for which area. The engine consumes the MLS and structure ops
//! ([`SYNC_CONSUMED`]: engine.ts `onFrame` and controller.ts `community.changed`); every other op the TS client
//! dispatches on (controller.ts `onGatewayEvent`) is handed to the one [`Area`] that owns it, through
//! [`crate::sync::SyncHooks::area_frame`] and [`dispatch`], to that area's [`AreaFrames`] implementation.
//!
//! The table is Task 1's: a Wave 1 task implements its area's [`AreaFrames::on_frame`] in its own feature module and does
//! not edit this file (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::Inner;
use crate::features::{self, Area};
use crate::gateway::frames::{Frame, Op};

/// The ops the sync engine consumes itself ([`crate::sync::Engine`]'s `on_frame`). `epoch.changed` is consumed by being
/// dropped, as engine.ts drops it.
pub(crate) const SYNC_CONSUMED: &[u8] = &[
    Op::HANDSHAKE,
    Op::COMMIT_NEEDED,
    Op::EPOCH_CHANGED,
    Op::MESSAGE_CT,
    Op::WELCOME,
    Op::MESSAGE_DELETED,
    Op::COMMUNITY_CHANGED,
];

/// The area an op the sync engine does not consume goes to, or `None` (a transport op, or one the TS client ignores).
/// `op` is a gateway op number ([`Op`] holds them as constants).
pub(crate) fn route(op: u8) -> Option<Area> {
    Some(match op {
        // controller.ts `incomingPresence`: the member list's presence (Task 14).
        Op::PRESENCE => Area::Members,
        // controller.ts `incomingTypingFrame`: the typing indicator (Task 9).
        Op::TYPING => Area::Messages,
        // controller.ts `ownVoiceState` and `calls.onFrame`: the voice roster and this device's call (Task 17).
        Op::VOICE_STATE => Area::Voice,
        // `calls.onFrame` for op 53, and `incomingAFKAway` (admin/afk.ts): rings and the AFK move (Task 18).
        Op::CALL_RING | Op::AFK_AWAY => Area::Ring,
        // `setConnection({ appBuild })`: the update-required surface (Task 24).
        Op::APP_BUILD => Area::Diagnostics,
        // The Bot's catalog, hints and replies (Task 20).
        Op::BOT_CATALOG | Op::BOT_STATE_CHANGED | Op::BOT_REPLY => Area::Bot,
        _ => return None,
    })
}

/// A feature that receives gateway frames. Runs on the gateway's event loop: it must return at once (spawn anything
/// slow on [`Inner::spawn`]) and publish what changed through [`Inner::publish`].
pub(crate) trait AreaFrames {
    fn on_frame(inner: &Arc<Inner>, f: &Frame);
}

/// Hands `f` to `area`'s feature.
pub(crate) fn dispatch(area: Area, inner: &Arc<Inner>, f: &Frame) {
    match area {
        Area::Messages => <features::messages::Messages as AreaFrames>::on_frame(inner, f),
        Area::Members => <features::members::Members as AreaFrames>::on_frame(inner, f),
        Area::Voice => <features::voice::Voice as AreaFrames>::on_frame(inner, f),
        Area::Ring => <features::ring::Ring as AreaFrames>::on_frame(inner, f),
        Area::Bot => <features::bot::Bot as AreaFrames>::on_frame(inner, f),
        Area::Diagnostics => <features::diagnostics::Diagnostics as AreaFrames>::on_frame(inner, f),
        // No op routes to these areas.
        Area::Shell
        | Area::Reactions
        | Area::Threads
        | Area::Attachments
        | Area::Dms
        | Area::ServerSettings
        | Area::UserSettings
        | Area::VoiceSettings
        | Area::BotAdmin
        | Area::Notify
        | Area::Search
        | Area::Onboarding
        | Area::Factors
        | Area::Instances => {}
    }
}

#[cfg(test)]
mod tests {
    use std::path::Path;
    use std::process::Command;

    use super::*;

    /// Every op the TS client dispatches on (controller.ts and sync/*) is consumed by the sync engine or routed to an
    /// area, never both; and no op at all is both consumed and routed.
    #[test]
    fn route_table_covers_ts_ops() {
        let script = Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/support/dump-ops.mjs");
        let out = Command::new("node")
            .arg(&script)
            .output()
            .expect("node runs the op dump");
        assert!(
            out.status.success(),
            "the op dump failed:\n{}",
            String::from_utf8_lossy(&out.stderr)
        );
        let ts: Vec<serde_json::Value> =
            serde_json::from_slice(&out.stdout).expect("the dump is a JSON array");
        assert!(ts.len() >= 16, "the dump found only {} ops", ts.len());
        let mut uncovered = Vec::new();
        for v in &ts {
            let name = v["name"].as_str().expect("name");
            let op = u8::try_from(v["op"].as_u64().expect("op")).expect("an op is a byte");
            if !SYNC_CONSUMED.contains(&op) && route(op).is_none() {
                uncovered.push(format!("{name} ({op})"));
            }
        }
        assert!(
            uncovered.is_empty(),
            "ops the TS client dispatches on that neither the sync engine consumes nor an area receives: {uncovered:?}"
        );
        for op in 0..=u8::MAX {
            assert!(
                !(SYNC_CONSUMED.contains(&op) && route(op).is_some()),
                "op {op} is both consumed by sync and routed to {:?}",
                route(op)
            );
        }
    }

    /// The transport ops (hello .. response) never reach an area: the gateway and the socket requests own them.
    #[test]
    fn transport_ops_are_not_routed() {
        for op in [
            Op::HELLO,
            Op::IDENTIFY,
            Op::READY,
            Op::INVALID_SESSION,
            Op::HEARTBEAT,
            Op::HEARTBEAT_ACK,
            Op::RECONNECT,
            Op::ERROR,
            Op::COMMIT_ACK,
            Op::REQUEST,
            Op::RESPONSE,
        ] {
            assert_eq!(route(op), None, "op {op}");
        }
    }
}
