//! Self-service role panels (Task 28 wave 2, lane B): a member with Manage Roles has the community's Bot post a panel
//! (sealed app operation 31, SelfRolesSection.tsx `publishPanel`), registers its mappings (`register_role_panel`), and
//! every member's confirmed reaction on a message the community's Bot authored is offered to the instance as a panel
//! action (controller.ts `onPosted` → `applyRolePanelReaction`); the instance grants or revokes only for a registered
//! panel. A reaction on any other message is never sent outside its MLS group (w2-bot-review I-B2, "Bot messages only").
//!
//! The action is durable, as the web's: written to the store's settings (`rolePanel.pending.<event msg id>`) before it
//! is sent, deleted once the instance answers it definitively, retried with a backoff while the answer may still
//! change, and sent again at every gateway ready (controller.ts `retryPendingRolePanelActions`). Only this account's
//! and this device's own actions are ever sent.

use std::collections::{HashMap, HashSet};
use std::sync::{Arc, LazyLock, Mutex, PoisonError, Weak};
use std::time::Duration;

use serde::{Deserialize, Serialize};

use crate::client::{Client, Inner};
use crate::features::bot_admin::{BotArg, BotRequest, OpError};
use crate::features::members::reload_members;
use crate::features::roles::{call_once, refusal};
use crate::gateway::frames::Value;
use crate::http::routes as r;
use crate::state::roles::RoleId;
use crate::state::{AppState, ChannelId, CommunityId, ConnectionStatus, TimelineItemState, hex};
use crate::sync::api::{ApiError, try_core};

/// controller.ts / settings.ts `PENDING_ROLE_PANEL_ACTION_PREFIX`.
pub const PENDING_PREFIX: &str = "rolePanel.pending.";
/// controller.ts `ROLE_PANEL_ACTION_RETRY_BASE_MS`, `_MAX_MS`, `_LIMIT`.
pub const RETRY_BASE: Duration = Duration::from_millis(500);
pub const RETRY_MAX: Duration = Duration::from_secs(8);
pub const RETRY_LIMIT: u32 = 5;
/// The outbox kinds of a reaction added (3) and removed (4).
const REACT_ON: u8 = 3;
const REACT_OFF: u8 = 4;
/// SelfRolesSection.tsx: an emoji is 1..=32 bytes, a role name at most 100, the whole mapping at most 2200.
const EMOJI_MAX: usize = 32;
const NAME_MAX: usize = 100;
const MAPPINGS_MAX: usize = 2200;
const SEQ_MAX: u64 = i64::MAX as u64;

/// One confirmed reaction waiting for the instance (controller.ts `PendingRolePanelAction`), stored as the web stores it.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PendingAction {
    pub community_id: String,
    pub channel_id: String,
    pub message_seq: String,
    pub group_id: String,
    pub event_seq: String,
    pub emoji: String,
    pub enabled: bool,
    pub user_id: String,
    pub device_id: String,
}

fn is_id(s: &str) -> bool {
    s.len() == 32 && s.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
}

fn unhex(s: &str) -> Option<[u8; 16]> {
    if !is_id(s) {
        return None;
    }
    let mut out = [0u8; 16];
    for (i, b) in out.iter_mut().enumerate() {
        *b = u8::from_str_radix(s.get(2 * i..2 * i + 2)?, 16).ok()?;
    }
    Some(out)
}

fn seq_of(s: &str) -> Option<u64> {
    (!s.is_empty() && s.bytes().all(|b| b.is_ascii_digit()))
        .then(|| s.parse().ok())
        .flatten()
        .filter(|n| (1..=SEQ_MAX).contains(n))
}

fn emoji_ok(e: &str) -> bool {
    (1..=EMOJI_MAX).contains(&e.len()) && !e.chars().any(|c| c.is_control())
}

impl PendingAction {
    /// controller.ts `retryPendingRolePanelActions`' check: well formed, and this account's and device's own.
    pub fn valid_for(&self, user: [u8; 16], device: [u8; 16]) -> bool {
        [
            &self.community_id,
            &self.channel_id,
            &self.group_id,
            &self.user_id,
            &self.device_id,
        ]
        .iter()
        .all(|s| is_id(s))
            && seq_of(&self.message_seq).is_some()
            && seq_of(&self.event_seq).is_some()
            && emoji_ok(&self.emoji)
            && self.user_id == hex(&user)
            && self.device_id == hex(&device)
    }
}

/// controller.ts `onPosted`: the action a confirmed own fold makes, or `None` for anything but a reaction (kind 3 or 4)
/// on a stored message (`target`, its seq known) of a community channel that the community's Bot authored. `group` is
/// the channel's text group, `event_seq` the seq the instance gave the reaction, `bot_user` the Bot user this device
/// pinned for the community.
///
/// w2-bot-review I-B2 (Jonas: "Bot messages only"): only the Bot posts role panels, so only a reaction on a message
/// whose sender credential is a Bot's and whose sender is the pinned Bot user is sent in plaintext (emoji and target
/// seq). Every other reaction stays inside the MLS group.
#[allow(clippy::too_many_arguments)]
pub fn action_of(
    state: &AppState,
    channel: ChannelId,
    group: [u8; 16],
    kind: u8,
    target: Option<[u8; 16]>,
    emoji: &str,
    event_seq: u64,
    bot_user: Option<[u8; 16]>,
) -> Option<PendingAction> {
    if kind != REACT_ON && kind != REACT_OFF {
        return None;
    }
    let target = target?;
    let community = state.channel(channel)?.community_id;
    let item = state
        .timelines
        .get(&channel)?
        .items
        .iter()
        .find(|i| i.msg_id.is_some_and(|m| m.0 == target))?;
    if item.state != TimelineItemState::Ok
        || !item.bot
        || bot_user.is_none()
        || item.sender_user.map(|u| u.0) != bot_user
    {
        return None;
    }
    let message_seq = item.seq?;
    let (user, device) = (state.account.user_id?, state.account.device_id?);
    let action = PendingAction {
        community_id: community.hex(),
        channel_id: channel.hex(),
        message_seq: message_seq.to_string(),
        group_id: hex(&group),
        event_seq: event_seq.to_string(),
        emoji: emoji.to_owned(),
        enabled: kind == REACT_ON,
        user_id: user.hex(),
        device_id: device.hex(),
    };
    action.valid_for(user.0, device.0).then_some(action)
}

/// The store's settings `[[k, v], …]` whose key is a pending action: `(key, action)`, the malformed ones as `None` (they
/// are deleted).
pub fn pending_of(settings: &[u8]) -> Vec<(String, Option<PendingAction>)> {
    let Ok(Value::Array(rows)) = Value::decode(settings) else {
        return Vec::new();
    };
    rows.iter()
        .filter_map(|row| match row.as_array()? {
            [k, v] => {
                let k = k.as_text()?;
                k.starts_with(PENDING_PREFIX).then(|| {
                    (
                        k.to_owned(),
                        v.as_text().and_then(|v| serde_json::from_str(v).ok()),
                    )
                })
            }
            _ => None,
        })
        .collect()
}

/// The wait before retry `attempt` (from 1): doubling from [`RETRY_BASE`], at most [`RETRY_MAX`]; `None` past
/// [`RETRY_LIMIT`].
pub fn retry_delay(attempt: u32) -> Option<Duration> {
    if attempt == 0 || attempt > RETRY_LIMIT {
        return None;
    }
    Some((RETRY_BASE * 2u32.pow(attempt - 1)).min(RETRY_MAX))
}

/// Only an outcome that may still change is retried; a definitive refusal is final, so it can never become a later
/// grant after the policy or the permissions changed.
pub fn retryable(e: &ApiError) -> bool {
    e.status == 0 || e.status == 408 || e.status == 425 || e.status == 429 || e.status >= 500
}

/// This device refused to build the request (`apply`'s `E_ROUTE_INPUT`): no answer from the instance, so the stored
/// action is neither retried nor deleted (w2-bot-review M-B4: controller.ts deletes only on a `DillaHttpError`).
const LOCAL_REFUSAL: &str = "E_ROUTE_INPUT";

/// What an answer does to a stored action: `Some(true)` retry, `Some(false)` delete, `None` keep as it is.
pub fn on_refusal(e: &ApiError) -> Option<bool> {
    if e.code == LOCAL_REFUSAL {
        None
    } else {
        Some(retryable(e))
    }
}

// ---- the running client ----

#[derive(Default)]
struct PerClient {
    /// Actions being sent now (one send per key at a time).
    jobs: Mutex<HashSet<String>>,
    /// Each key's armed retry.
    retries: Mutex<HashMap<String, tokio::task::AbortHandle>>,
}

type Clients = Vec<(Weak<Inner>, Arc<PerClient>)>;
static CLIENTS: LazyLock<Mutex<Clients>> = LazyLock::new(Mutex::default);

fn lock<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(PoisonError::into_inner)
}

fn per_client(inner: &Arc<Inner>) -> Arc<PerClient> {
    let mut clients = lock(&CLIENTS);
    clients.retain(|(w, _)| w.strong_count() > 0);
    let mine = Arc::downgrade(inner);
    if let Some((_, s)) = clients.iter().find(|(w, _)| Weak::ptr_eq(w, &mine)) {
        return s.clone();
    }
    let s = Arc::new(PerClient::default());
    clients.push((mine, s.clone()));
    s
}

/// The sync engine stored an own outbox row `msg` of `group` (kind, the message it folds onto, its body) at `event_seq`.
pub(crate) fn on_posted(
    inner: &Arc<Inner>,
    group: [u8; 16],
    msg: [u8; 16],
    kind: u8,
    target: Option<[u8; 16]>,
    body: String,
    event_seq: u64,
) {
    if (kind != REACT_ON && kind != REACT_OFF) || target.is_none() {
        return;
    }
    let weak = Arc::downgrade(inner);
    drop(inner.spawn(async move {
        let Some(inner) = weak.upgrade() else {
            return Ok(());
        };
        let rows = inner.engine().groups().await;
        let Some(row) = rows.iter().find(|r| r.group_id == group && r.kind == 0) else {
            return Ok(());
        };
        let channel = ChannelId(row.target_id);
        let state = inner.snapshot();
        let Some(community) = state.channel(channel).map(|c| c.community_id) else {
            return Ok(());
        };
        // The Bot user this device pinned for the community (the web's `core.botUserPinned`): only its messages are
        // role panels (w2-bot-review I-B2).
        let core = inner.engine().core().clone();
        let bot_user = try_core(&core, move |c| c.bot_user_pinned(&community.0))
            .await
            .ok()
            .and_then(Result::ok)
            .flatten();
        let Some(action) = action_of(
            &state, channel, group, kind, target, &body, event_seq, bot_user,
        ) else {
            return Ok(());
        };
        // Keyed by the reaction's own message id, as the web keys it: one action per reaction event.
        let key = format!("{PENDING_PREFIX}{}", hex(&msg));
        let json = serde_json::to_string(&action).unwrap_or_default();
        let core = inner.engine().core().clone();
        let stored = {
            let (k, v) = (key.clone(), json);
            try_core(&core, move |c| c.setting_put(&k, &v)).await
        };
        if matches!(stored, Ok(Ok(()))) {
            submit(&inner, key, action, 0).await;
        }
        Ok(())
    }));
}

/// Every gateway ready: the armed retries are dropped and every stored action of this device is sent again; one of
/// another account or device, or a malformed one, is deleted.
pub(crate) fn on_ready(inner: &Arc<Inner>) {
    let pc = per_client(inner);
    for (_, h) in lock(&pc.retries).drain() {
        h.abort();
    }
    let weak = Arc::downgrade(inner);
    drop(inner.spawn(async move {
        let Some(inner) = weak.upgrade() else {
            return Ok(());
        };
        let s = inner.snapshot();
        let (Some(user), Some(device)) = (s.account.user_id, s.account.device_id) else {
            return Ok(());
        };
        let core = inner.engine().core().clone();
        let Ok(Ok(settings)) = try_core(&core, |c| c.settings()).await else {
            return Ok(());
        };
        for (key, action) in pending_of(&settings) {
            match action.filter(|a| a.valid_for(user.0, device.0)) {
                Some(a) => submit(&inner, key, a, 0).await,
                None => {
                    let _ = try_core(&core, move |c| c.setting_delete(&key)).await;
                }
            }
        }
        Ok(())
    }));
}

/// controller.ts `submitPendingRolePanelAction`.
async fn submit(inner: &Arc<Inner>, key: String, action: PendingAction, attempt: u32) {
    let pc = per_client(inner);
    if !lock(&pc.jobs).insert(key.clone()) {
        return;
    }
    let answer = apply(inner, &action).await;
    let core = inner.engine().core().clone();
    match answer {
        Ok(()) => {
            let k = key.clone();
            let _ = try_core(&core, move |c| c.setting_delete(&k)).await;
            // The grant is an authenticated state change: read the member roles back now, so this device shows it
            // even when its community_changed frame is late or lost.
            if let Some(c) = unhex(&action.community_id) {
                let _ = reload_members(inner, CommunityId(c)).await;
            }
        }
        Err(e) => match on_refusal(&e) {
            Some(true) => schedule(inner, &pc, key.clone(), action, attempt + 1),
            Some(false) => {
                let k = key.clone();
                let _ = try_core(&core, move |c| c.setting_delete(&k)).await;
            }
            None => {}
        },
    }
    lock(&pc.jobs).remove(&key);
}

fn schedule(
    inner: &Arc<Inner>,
    pc: &Arc<PerClient>,
    key: String,
    action: PendingAction,
    attempt: u32,
) {
    let Some(delay) = retry_delay(attempt) else {
        return;
    };
    if inner.snapshot().connection.status != ConnectionStatus::Online
        || lock(&pc.retries).contains_key(&key)
    {
        return;
    }
    let weak = Arc::downgrade(inner);
    let k = key.clone();
    let task = tokio::spawn(async move {
        tokio::time::sleep(delay).await;
        let Some(inner) = weak.upgrade() else {
            return;
        };
        lock(&per_client(&inner).retries).remove(&k);
        if inner.snapshot().connection.status != ConnectionStatus::Online {
            return;
        }
        Box::pin(submit(&inner, k, action, attempt)).await;
    });
    lock(&pc.retries).insert(key, task.abort_handle());
}

/// routes.ts `applyRolePanelReaction`: `POST …/role-panels/{channel}/{message_seq}/reaction [group, event_seq, emoji,
/// enabled]`, no 429 wait.
async fn apply(inner: &Inner, a: &PendingAction) -> Result<(), ApiError> {
    let (Some(group), Some(event_seq)) = (unhex(&a.group_id), seq_of(&a.event_seq)) else {
        return Err(refusal("E_ROUTE_INPUT", "invalid role panel reaction"));
    };
    if seq_of(&a.message_seq).is_none() || !emoji_ok(&a.emoji) {
        return Err(refusal("E_ROUTE_INPUT", "invalid role panel reaction"));
    }
    let body = Value::Array(vec![
        Value::Bytes(group.to_vec()),
        Value::Uint(event_seq),
        Value::Text(a.emoji.clone()),
        Value::Uint(a.enabled.into()),
    ])
    .encode();
    call_once::<r::ApplyRolePanelReaction>(
        inner,
        &[&a.community_id, &a.channel_id, &a.message_seq],
        Some(body),
        None,
    )
    .await
    .map(|_| ())
}

// ---- publishing a panel ----

/// One role of a panel: the reaction that picks it, the role, and its name as the panel shows it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PanelOption {
    pub emoji: String,
    pub role: RoleId,
    pub name: String,
}

/// Why a panel was not published and registered.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum PanelError {
    /// No channel, no role, an empty, long, repeated or control-character emoji, or too large a panel
    /// (`admin.bot.rolePanel.invalid`).
    #[error("invalid role panel")]
    Invalid,
    /// No answer in time: the Bot may have posted. Publishing again with the same request id settles it
    /// (`admin.bot.rolePanel.uncertain`, Retry).
    #[error("the role panel's publication is not confirmed")]
    Uncertain { request_id: [u8; 16] },
    /// Refused, by code (`admin.bot.rolePanel.error`).
    #[error("the role panel was refused ({0})")]
    Refused(String),
}

/// The CBOR the Bot posts from: `[[emoji, role, name], …]`.
pub fn mappings(options: &[PanelOption]) -> Vec<u8> {
    Value::Array(
        options
            .iter()
            .map(|o| {
                Value::Array(vec![
                    Value::Text(o.emoji.trim().to_owned()),
                    Value::Bytes(o.role.to_vec()),
                    Value::Text(o.name.clone()),
                ])
            })
            .collect(),
    )
    .encode()
}

/// SelfRolesSection.tsx `publishPanel`'s check.
pub fn panel_valid(options: &[PanelOption]) -> bool {
    let mut seen = HashSet::new();
    !options.is_empty()
        && options.len() <= 100
        && options.iter().all(|o| {
            let e = o.emoji.trim();
            emoji_ok(e) && o.name.len() <= NAME_MAX && seen.insert(e.to_owned())
        })
        && mappings(options).len() <= MAPPINGS_MAX
}

/// The accepted panel message `[channel, seq]`: its seq, when it names `channel`.
pub fn accepted_seq(data: &[u8], channel: ChannelId) -> Option<u64> {
    let v = Value::decode(data).ok()?;
    match v.as_array()? {
        [c, s] if c.as_bytes() == Some(&channel.0[..]) => {
            s.as_uint().filter(|n| (1..=SEQ_MAX).contains(n))
        }
        _ => None,
    }
}

/// The role panels area's handle.
#[derive(Clone)]
pub struct RolePanels {
    inner: Arc<Inner>,
    admin: crate::features::bot_admin::BotAdmin,
}

impl Client {
    /// The [`RolePanels`] handle. Hold one per page: a Retry of an unconfirmed publication resends its request.
    pub fn role_panels(&self) -> RolePanels {
        RolePanels {
            inner: self.inner().clone(),
            admin: self.bot_admin(),
        }
    }
}

impl RolePanels {
    /// [`RolePanels::publish`] on the client's runtime, for a view on another executor.
    pub fn publish_job(
        &self,
        community: CommunityId,
        channel: ChannelId,
        options: Vec<PanelOption>,
        request_id: [u8; 16],
    ) -> impl std::future::Future<Output = Result<(), PanelError>> + use<> {
        let this = self.clone();
        let rx = self
            .inner
            .spawn(async move { Ok(this.publish(community, channel, &options, request_id).await) });
        async move {
            match rx.await {
                Ok(Ok(answer)) => answer,
                _ => Err(PanelError::Refused("E_STOPPED".into())),
            }
        }
    }

    /// SelfRolesSection.tsx `publishPanel`: the Bot posts the panel into `channel` (a Bot channel; sealed operation 31 at
    /// revision 0, answered with the accepted message), then the mappings are registered against that message. A Retry
    /// passes the same `request_id`: the Bot answers what it did and never posts a second panel.
    pub async fn publish(
        &self,
        community: CommunityId,
        channel: ChannelId,
        options: &[PanelOption],
        request_id: [u8; 16],
    ) -> Result<(), PanelError> {
        if !panel_valid(options) {
            return Err(PanelError::Invalid);
        }
        let ops = self.admin.operations(community);
        let answer = ops
            .request(BotRequest {
                operation: 31,
                channel: None,
                args: vec![
                    BotArg::Text(channel.hex()),
                    BotArg::Bytes(mappings(options)),
                ],
                revision: Some(0),
                request_id: Some(request_id),
            })
            .await;
        let result = match answer {
            Ok(r) => r,
            Err(OpError::Uncertain { request_id }) => {
                return Err(PanelError::Uncertain { request_id });
            }
            Err(OpError::Refused { code, .. }) => return Err(PanelError::Refused(code)),
            Err(OpError::Invalid) => return Err(PanelError::Invalid),
        };
        if result.status != 0 {
            return Err(PanelError::Refused(status_code(result.status).to_owned()));
        }
        let seq = accepted_seq(&result.data, channel)
            .ok_or_else(|| PanelError::Refused("E_CBOR_SHAPE".into()))?;
        self.register(community, request_id, channel, seq, options)
            .await
            .map_err(|e| PanelError::Refused(e.code))
    }

    /// routes.ts `registerRolePanel`: `PUT …/role-panels [request, channel, seq, [[emoji, role], …]]`, acknowledged
    /// with `[request, count]`.
    async fn register(
        &self,
        community: CommunityId,
        request_id: [u8; 16],
        channel: ChannelId,
        seq: u64,
        options: &[PanelOption],
    ) -> Result<(), ApiError> {
        let rows = options
            .iter()
            .map(|o| {
                Value::Array(vec![
                    Value::Text(o.emoji.trim().to_owned()),
                    Value::Bytes(o.role.to_vec()),
                ])
            })
            .collect();
        let body = Value::Array(vec![
            Value::Bytes(request_id.to_vec()),
            Value::Bytes(channel.0.to_vec()),
            Value::Uint(seq),
            Value::Array(rows),
        ])
        .encode();
        let c = community.hex();
        let raw = call_once::<r::RegisterRolePanel>(
            &self.inner,
            &[&c],
            Some(body),
            Some(request_id.to_vec()),
        )
        .await?;
        let ok = Value::decode(&raw).ok().is_some_and(|v| {
            v.as_array().is_some_and(|a| {
                a.len() == 2
                    && a[0].as_bytes() == Some(&request_id[..])
                    && a[1].as_uint() == Some(options.len() as u64)
            })
        });
        if ok {
            Ok(())
        } else {
            Err(refusal(
                "E_CBOR_SHAPE",
                "invalid role panel acknowledgement",
            ))
        }
    }
}

/// SelfRolesSection.tsx `STATUS_CODE`: a sealed answer's v1 status as the code it stands for.
pub fn status_code(status: u8) -> &'static str {
    match status {
        1 => "E_INVALID",
        2 => "E_FORBIDDEN",
        3 => "E_BOT_STALE",
        4 => "E_CONFLICT",
        5 => "E_UNAVAILABLE",
        6 => "E_BUSY",
        7 => "E_NOT_FOUND",
        8 => "E_UPGRADE_REQUIRED",
        9 => "E_INTERNAL",
        _ => "E_UNKNOWN",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::{
        Channel, ChannelGroupState, DeviceId, MessageId, Timeline, TimelineItem, UserId,
    };

    const COMMUNITY: CommunityId = CommunityId([0xa1; 16]);
    const GENERAL: ChannelId = ChannelId([0xc3; 16]);
    const GROUP: [u8; 16] = [0x61; 16];
    const PANEL: [u8; 16] = [0x70; 16];
    const ME: UserId = UserId([0x11; 16]);
    const DEVICE: DeviceId = DeviceId([0x12; 16]);
    /// The community's Bot user, the panel's author.
    const BOT: [u8; 16] = [0xb0; 16];

    fn world(panel_state: TimelineItemState, seq: Option<u64>) -> AppState {
        let mut s = AppState::default();
        s.account.user_id = Some(ME);
        s.account.device_id = Some(DEVICE);
        s.channels.push(Channel {
            id: GENERAL,
            community_id: COMMUNITY,
            kind: 0,
            mode: 0,
            name: "general".into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode: 0,
            text_group_id: None,
            group: ChannelGroupState::default(),
        });
        let mut t = Timeline {
            channel_id: GENERAL,
            group: ChannelGroupState::default(),
            items: Vec::new(),
            has_earlier: false,
        };
        t.items.push(TimelineItem {
            key: "s7".into(),
            state: panel_state,
            reason: String::new(),
            sender_user: Some(UserId([0xb0; 16])),
            sender_device: None,
            own: false,
            bot: true,
            ts: 1,
            body: "React with the matching emoji to choose a role:\n👍 Readers".into(),
            msg_id: Some(MessageId(PANEL)),
            seq,
            edited: false,
            reply: None,
            reactions: Vec::new(),
            pinned: false,
            attachments: Vec::new(),
            mention: false,
        });
        s.timelines.insert(GENERAL, t);
        s
    }

    #[test]
    fn a_confirmed_reaction_on_a_stored_message_is_an_action_and_nothing_else_is() {
        let s = world(TimelineItemState::Ok, Some(7));
        let on =
            action_of(&s, GENERAL, GROUP, 3, Some(PANEL), "👍", 9, Some(BOT)).expect("a reaction");
        assert_eq!(
            on,
            PendingAction {
                community_id: COMMUNITY.hex(),
                channel_id: GENERAL.hex(),
                message_seq: "7".into(),
                group_id: hex(&GROUP),
                event_seq: "9".into(),
                emoji: "👍".into(),
                enabled: true,
                user_id: ME.hex(),
                device_id: DEVICE.hex(),
            }
        );
        assert!(
            !action_of(&s, GENERAL, GROUP, 4, Some(PANEL), "👍", 10, Some(BOT))
                .unwrap()
                .enabled
        );
        // A post, a pin, an edit: not reactions.
        for kind in [0, 1, 2, 5, 6] {
            assert_eq!(
                action_of(&s, GENERAL, GROUP, kind, Some(PANEL), "👍", 9, Some(BOT)),
                None
            );
        }
        assert_eq!(
            action_of(&s, GENERAL, GROUP, 3, None, "👍", 9, Some(BOT)),
            None
        );
        assert_eq!(
            action_of(&s, GENERAL, GROUP, 3, Some([1; 16]), "👍", 9, Some(BOT)),
            None,
            "not loaded"
        );
        assert_eq!(
            action_of(
                &s,
                ChannelId([9; 16]),
                GROUP,
                3,
                Some(PANEL),
                "👍",
                9,
                Some(BOT)
            ),
            None,
            "a DM"
        );
        assert_eq!(
            action_of(&s, GENERAL, GROUP, 3, Some(PANEL), "", 9, Some(BOT)),
            None
        );
        let unread = world(TimelineItemState::CannotRead, Some(7));
        assert_eq!(
            action_of(&unread, GENERAL, GROUP, 3, Some(PANEL), "👍", 9, Some(BOT)),
            None
        );
        let pending = world(TimelineItemState::Ok, None);
        assert_eq!(
            action_of(&pending, GENERAL, GROUP, 3, Some(PANEL), "👍", 9, Some(BOT)),
            None
        );
    }

    /// w2-bot-review I-B2 (Jonas: "Bot messages only"): only a reaction on a message the community's Bot authored (a
    /// role panel) is offered to the instance in plaintext; one on anybody else's message stays E2EE-only.
    #[test]
    fn only_a_reaction_on_the_communitys_bot_message_is_an_action() {
        let s = world(TimelineItemState::Ok, Some(7));
        assert!(action_of(&s, GENERAL, GROUP, 3, Some(PANEL), "👍", 9, Some(BOT)).is_some());
        assert_eq!(
            action_of(&s, GENERAL, GROUP, 3, Some(PANEL), "👍", 9, None),
            None,
            "no Bot pinned for the community"
        );
        assert_eq!(
            action_of(
                &s,
                GENERAL,
                GROUP,
                3,
                Some(PANEL),
                "👍",
                9,
                Some([0x55; 16])
            ),
            None,
            "another Bot user than the community's"
        );
        let mut friend = s.clone();
        let item = &mut friend.timelines.get_mut(&GENERAL).unwrap().items[0];
        item.bot = false;
        item.sender_user = Some(UserId([0x22; 16]));
        assert_eq!(
            action_of(&friend, GENERAL, GROUP, 3, Some(PANEL), "🍆", 9, Some(BOT)),
            None,
            "a friend's message"
        );
        let mut claims = s.clone();
        claims.timelines.get_mut(&GENERAL).unwrap().items[0].bot = false;
        assert_eq!(
            action_of(&claims, GENERAL, GROUP, 3, Some(PANEL), "👍", 9, Some(BOT)),
            None,
            "the Bot user's message without a Bot credential"
        );
        let mut posing = s.clone();
        posing.timelines.get_mut(&GENERAL).unwrap().items[0].sender_user = Some(UserId([0x22; 16]));
        assert_eq!(
            action_of(&posing, GENERAL, GROUP, 3, Some(PANEL), "👍", 9, Some(BOT)),
            None,
            "a Bot credential of another user"
        );
    }

    #[test]
    fn stored_actions_read_back_and_only_this_devices_own_are_valid() {
        let s = world(TimelineItemState::Ok, Some(7));
        let a = action_of(&s, GENERAL, GROUP, 3, Some(PANEL), "👍", 9, Some(BOT)).unwrap();
        let json = serde_json::to_string(&a).unwrap();
        assert!(
            json.contains("\"communityId\"") && json.contains("\"eventSeq\":\"9\""),
            "{json}"
        );
        let settings = Value::Array(vec![
            Value::Array(vec![Value::Text("other".into()), Value::Text("x".into())]),
            Value::Array(vec![
                Value::Text(format!("{PENDING_PREFIX}a")),
                Value::Text(json),
            ]),
            Value::Array(vec![
                Value::Text(format!("{PENDING_PREFIX}b")),
                Value::Text("{not json".into()),
            ]),
        ])
        .encode();
        let read = pending_of(&settings);
        assert_eq!(read.len(), 2);
        assert_eq!(read[0].1.as_ref(), Some(&a));
        assert_eq!(read[1].1, None, "malformed: deleted");
        assert!(a.valid_for(ME.0, DEVICE.0));
        assert!(!a.valid_for([0x99; 16], DEVICE.0), "another account's");
        assert!(!a.valid_for(ME.0, [0x99; 16]), "another device's");
        let mut bad = a.clone();
        bad.message_seq = "0".into();
        assert!(!bad.valid_for(ME.0, DEVICE.0));
        bad.message_seq = "9223372036854775808".into();
        assert!(!bad.valid_for(ME.0, DEVICE.0));
    }

    #[test]
    fn retries_back_off_to_a_bound_and_only_an_outcome_that_may_change_is_retried() {
        assert_eq!(retry_delay(0), None);
        assert_eq!(retry_delay(1), Some(Duration::from_millis(500)));
        assert_eq!(retry_delay(2), Some(Duration::from_secs(1)));
        assert_eq!(retry_delay(5), Some(Duration::from_secs(8)));
        assert_eq!(retry_delay(6), None);
        let e = |status| ApiError::local(status, "E", "");
        for s in [0, 408, 425, 429, 500, 503] {
            assert!(retryable(&e(s)), "{s}");
        }
        for s in [400, 403, 404, 409] {
            assert!(!retryable(&e(s)), "{s}");
        }
        // w2-bot-review M-B4: a refusal of this device's own (no answer from the instance) is kept, never deleted or
        // retried; the instance's answers are retried or deleted.
        assert_eq!(on_refusal(&ApiError::local(0, "E_ROUTE_INPUT", "")), None);
        assert_eq!(on_refusal(&e(503)), Some(true));
        assert_eq!(on_refusal(&e(0)), Some(true));
        assert_eq!(on_refusal(&e(404)), Some(false));
    }

    #[test]
    fn a_panel_needs_unique_short_emojis_and_its_accepted_message_names_its_channel() {
        let o = |emoji: &str, role: u8| PanelOption {
            emoji: emoji.into(),
            role: [role; 16],
            name: "Readers".into(),
        };
        assert!(panel_valid(&[o("👍", 1), o(" 🎉 ", 2)]));
        assert!(!panel_valid(&[]));
        assert!(!panel_valid(&[o("", 1)]));
        assert!(!panel_valid(&[o("👍", 1), o("👍", 2)]), "repeated");
        assert!(!panel_valid(&[o("a\u{7}", 1)]), "control");
        assert!(!panel_valid(&[o(&"x".repeat(33), 1)]));
        let accepted =
            Value::Array(vec![Value::Bytes(GENERAL.0.to_vec()), Value::Uint(12)]).encode();
        assert_eq!(accepted_seq(&accepted, GENERAL), Some(12));
        assert_eq!(accepted_seq(&accepted, ChannelId([1; 16])), None);
        let zero = Value::Array(vec![Value::Bytes(GENERAL.0.to_vec()), Value::Uint(0)]).encode();
        assert_eq!(accepted_seq(&zero, GENERAL), None);
    }
}
