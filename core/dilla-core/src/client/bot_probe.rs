//! The Bot's health probe (Bot v2 task 17, fix round 2): an `OpProbe` (90) sealed to the Bot's own
//! current command key, as the Bot's own device, so the probe travels the path a member's command
//! does (dillad's relay, the helper's open and verify, the intake, the reply route) and fails where
//! a member's command would.

use super::error::E_CORE_INPUT;
use super::{ClientCore, ClientError};
use crate::bot::command::{CommandKeyPublication, CommandRequest, ReplySecret, seal_command};
use crate::ids::{CommunityId, DeviceId, UserId};

/// The probe's operation, `OpProbe` in the Bot's catalog (`internal/bot/protocol`).
const BOT_OP_PROBE: u64 = 90;

impl ClientCore {
    /// Signs an `OpProbe` with no arguments and no channel as this device, `sent_at = now`, and
    /// seals it to `publication`: the Bot's own command key for `community_id`, which this device
    /// signed (the helper keeps it; nothing here trusts another device's key). Returns the sealed
    /// bytes and the reply secret its answers open with.
    pub fn bot_probe_seal(
        &self,
        publication: &[u8],
        community_id: &[u8; 16],
        request_id: [u8; 16],
        now: u64,
    ) -> Result<(Vec<u8>, ReplySecret), ClientError> {
        let p = CommandKeyPublication::decode(publication)
            .map_err(|e| ClientError::new(E_CORE_INPUT, format!("probe: publication: {e}")))?;
        let own = self.own()?;
        if p.community_id.as_bytes() != community_id
            || p.bot_device_id.as_bytes() != &own.device_id
            || request_id == [0; 16]
        {
            return Err(ClientError::new(
                E_CORE_INPUT,
                "probe: not this device's own command key",
            ));
        }
        let req = CommandRequest {
            request_id,
            community_id: CommunityId::from_bytes(*community_id),
            channel_id: None,
            operation: BOT_OP_PROBE,
            args: Vec::new(),
            sent_at: now,
            reply_key: [1; 32], // replaced by the reply secret's public half
        };
        seal_command(
            &p,
            &req,
            UserId::from_bytes(own.user_id),
            DeviceId::from_bytes(own.device_id),
            self.signer()?,
        )
        .map_err(|e| ClientError::new(E_CORE_INPUT, format!("probe: seal: {e}")))
    }
}
