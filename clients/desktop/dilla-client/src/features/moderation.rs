//! Area **members** (Task 14): kick, ban, unban, timeout and warn with evidence; and (Task 28 wave 2, lane A) the
//! moderated removal of another person's message with its reason, its signed proof and its sealed evidence.
//!
//! Ports: packages/client-core/src/admin/controller.ts (`kickMember`, `banMember`, `unbanMember`, `loadBans`), the
//! worker's timeout and warning commands (worker/controller.ts `setMemberTimeout`, `clearMemberTimeout`,
//! `getMemberTimeout`, `warnMember`), the input checks of packages/web/src/screens/admin/Moderation.tsx, and the worker's
//! moderated removal (`moderateRemoveMessage`, `resolveManualRemoval`, `resumeManualRemovalEvidence`,
//! `uploadManualRemovalEvidence`, `sealedEvidence`) with the page's retry (web `conversation/manual-removal.ts`).
//!
//! A removal's proof is signed and its evidence sealed by the same core and Bot transport the web client uses
//! (`ClientCore::prepare_manual_removal_evidence`, the core's durable outbox, and the sealed app operation 161): no crypto
//! of the desktop's own. The reason and the excerpt are plaintext: this module holds them in [`Zeroizing`] buffers and
//! wipes the copies it reads back from the core.
//!
//! The view offers an action only where `features::members::menu_for` says the caller may take it, but that gate is
//! advisory: the instance decides each request again. A 403 means the access the view held is stale, so the access
//! and the member list are read again before the refusal is handed on with its own code.

use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use dilla_core::client::{ClientCore, ClientError as CoreError, ManualRemovalEvidence};
use zeroize::{Zeroize, Zeroizing};

use crate::client::{Client, Inner};
use crate::features::bot_admin::{BotArg, BotOperations, BotRequest, OpError, default_transport};
use crate::features::members::reload_members;
use crate::features::roles::{call, load_access, perm, refusal, request_id, viewer_channel_bits};
use crate::gateway::frames::Value;
use crate::http::routes as r;
use crate::state::members::BanSummary;
use crate::state::messages::decode_timeline;
use crate::state::{ChannelId, CommunityId, MessageId, UserId, hex};
use crate::sync::GroupRow;
use crate::sync::api::{ApiError, try_core};
use crate::sync::value::{DecodeError, Rows, at_least, bin16, text, u53, uint};

/// A ban lasts at least an hour and at most a hundred years (controller.ts).
pub const BAN_SECONDS_MIN: u64 = 3600;
pub const BAN_SECONDS_MAX: u64 = 3_153_600_000;
/// routes.ts `BAN_REASON_MAX_BYTES`, Moderation.tsx `REASON_MAX_BYTES`.
pub const REASON_MAX_BYTES: usize = 512;
/// routes.ts `BANS_MAX`.
pub const BANS_MAX: usize = 10_000;

/// Moderation.tsx `BAN_LENGTHS`: the select's lengths in seconds; `Forever` sends none.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum BanLength {
    Hour,
    Day,
    Week,
    Month,
    Forever,
}

impl BanLength {
    pub const ALL: [BanLength; 5] = [
        Self::Hour,
        Self::Day,
        Self::Week,
        Self::Month,
        Self::Forever,
    ];

    pub fn seconds(self) -> Option<u64> {
        match self {
            Self::Hour => Some(3600),
            Self::Day => Some(86_400),
            Self::Week => Some(604_800),
            Self::Month => Some(2_592_000),
            Self::Forever => None,
        }
    }

    /// The strings key (`mod.ban.duration.<key>`).
    pub fn key(self) -> &'static str {
        match self {
            Self::Hour => "hour",
            Self::Day => "day",
            Self::Week => "week",
            Self::Month => "month",
            Self::Forever => "forever",
        }
    }
}

/// Moderation.tsx `TIMEOUT_LENGTHS`: `Clear` lifts a timeout.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TimeoutLength {
    TenMinutes,
    Hour,
    Day,
    Clear,
}

impl TimeoutLength {
    pub const ALL: [TimeoutLength; 4] = [Self::TenMinutes, Self::Hour, Self::Day, Self::Clear];

    pub fn seconds(self) -> Option<u64> {
        match self {
            Self::TenMinutes => Some(600),
            Self::Hour => Some(3600),
            Self::Day => Some(86_400),
            Self::Clear => None,
        }
    }

    /// The strings key (`mod.timeout.duration.<key>`).
    pub fn key(self) -> &'static str {
        match self {
            Self::TenMinutes => "tenMinutes",
            Self::Hour => "hour",
            Self::Day => "day",
            Self::Clear => "clear",
        }
    }
}

/// Why the instance would refuse a reason (Moderation.tsx `reasonProblem`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ReasonProblem {
    Long,
    Control,
}

/// Moderation.tsx `reasonProblem`: over 512 bytes, or a control character other than a line feed or a tab.
pub fn reason_problem(reason: &str) -> Option<ReasonProblem> {
    if reason.len() > REASON_MAX_BYTES {
        return Some(ReasonProblem::Long);
    }
    let bad = |c: char| {
        let n = u32::from(c);
        (n <= 0x08)
            || n == 0x0b
            || n == 0x0c
            || n == 0x0d
            || (0x0e..=0x1f).contains(&n)
            || (0x7f..=0x9f).contains(&n)
    };
    reason.chars().any(bad).then_some(ReasonProblem::Control)
}

/// What became of a warning's sealed evidence (worker/controller.ts `warnMember`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Evidence {
    /// The Bot stored it.
    Accepted,
    /// Staged on this device and owed to the Bot: a later resume delivers it.
    Pending,
    Rejected,
    Unknown,
}

/// The instance's receipt of a warning.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct WarnReceipt {
    pub request_id: [u8; 16],
    pub action_id: [u8; 16],
    pub revision: u64,
    pub accepted_at: u64,
    pub evidence: Evidence,
}

fn now_s() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

fn bad(e: DecodeError) -> ApiError {
    ApiError::local(0, "E_DECODE", e.0)
}

fn decode(raw: &[u8]) -> Result<Value, ApiError> {
    Value::decode(raw).map_err(|_| refusal("E_DECODE", "not CBOR"))
}

/// routes.ts `banRow`: `[user_id, reason, by_user, created, expires|null, username?, display?]`.
fn decode_bans(raw: &[u8]) -> Result<Vec<BanSummary>, DecodeError> {
    let rows = Rows::of(raw)?;
    if rows.iter().len() > BANS_MAX {
        return Err(DecodeError("too many bans"));
    }
    rows.iter()
        .map(|row| {
            let a = at_least(row, 5)?;
            let username = a.get(5).map(text).transpose()?.unwrap_or_default();
            let display = a.get(6).map(text).transpose()?.unwrap_or_default();
            Ok(BanSummary {
                user_id: UserId(bin16(&a[0])?),
                name: if display.is_empty() {
                    username
                } else {
                    display
                },
                reason: text(&a[1])?,
                by_user: UserId(bin16(&a[2])?),
                created: uint(&a[3])?,
                expires: match &a[4] {
                    Value::Null => None,
                    v => Some(uint(v)?),
                },
            })
        })
        .collect()
}

/// The members area's handle for moderation: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Moderation {
    inner: Arc<Inner>,
}

impl Moderation {
    /// The handle of the client `inner` (for another area's own use; the `/kick` and `/ban` commands run through it).
    pub(crate) fn of(inner: &Arc<Inner>) -> Self {
        Self {
            inner: inner.clone(),
        }
    }
}

impl Client {
    /// The [`Moderation`] handle.
    pub fn moderation(&self) -> Moderation {
        Moderation {
            inner: self.inner().clone(),
        }
    }
}

impl Moderation {
    /// Runs `job` (built from this handle) on the client's runtime and answers its result, so a view on another executor
    /// can await it: `client.moderation().run(|m| async move { m.kick(c, u).await })`.
    pub fn run<T: Send + 'static, F, J: FnOnce(Moderation) -> F>(
        &self,
        job: J,
    ) -> impl std::future::Future<Output = Result<T, ApiError>> + use<T, F, J>
    where
        F: std::future::Future<Output = Result<T, ApiError>> + Send + 'static,
    {
        crate::features::roles::on_runtime(&self.inner, job(self.clone()))
    }

    fn listed(&self, community: CommunityId) -> Result<(), ApiError> {
        if self
            .inner
            .snapshot()
            .communities
            .iter()
            .any(|c| c.id == community)
        {
            Ok(())
        } else {
            Err(refusal("E_BAD_INPUT", "unknown community"))
        }
    }

    /// A refusal of a member command: a 403 means the view's access and member list are stale; both are read again.
    async fn stale(&self, community: CommunityId, e: &ApiError) {
        // E_ code: E_FORBIDDEN (matched on the status, as controller.ts `guarded` does).
        if e.status == 403 {
            let _ = load_access(&self.inner, community).await;
            let _ = reload_members(&self.inner, community).await;
        }
    }

    /// After a kick, a ban or an unban: the member list and the caller's access, read again. A failed read does not undo
    /// the change.
    async fn after(&self, community: CommunityId) {
        let _ = reload_members(&self.inner, community).await;
        let _ = load_access(&self.inner, community).await;
    }

    /// controller.ts `kickMember`: `DELETE .../members/{user}`. A 404 surfaces (the view says the member is already gone);
    /// the member list is read again either way.
    pub async fn kick(&self, community: CommunityId, user: UserId) -> Result<(), ApiError> {
        self.listed(community)?;
        let (c, u) = (community.hex(), user.hex());
        if let Err(e) = call::<r::KickMember>(&self.inner, &[&c, &u], None, None).await {
            // E_ code: E_NOT_FOUND (the member already left or was removed).
            if e.status == 404 {
                let _ = reload_members(&self.inner, community).await;
            } else {
                self.stale(community, &e).await;
            }
            return Err(e);
        }
        self.after(community).await;
        Ok(())
    }

    /// controller.ts `banMember`: `PUT .../bans/{user}` with `[reason, expires|null]`; `seconds` is the length (`None`
    /// for ever), checked here as the worker checks it.
    pub async fn ban(
        &self,
        community: CommunityId,
        user: UserId,
        reason: &str,
        seconds: Option<u64>,
    ) -> Result<(), ApiError> {
        if seconds.is_some_and(|s| !(BAN_SECONDS_MIN..=BAN_SECONDS_MAX).contains(&s)) {
            return Err(refusal("E_BAD_INPUT", "seconds is out of range"));
        }
        if reason.len() > REASON_MAX_BYTES {
            return Err(refusal("E_BAD_INPUT", "reason is too long"));
        }
        self.listed(community)?;
        let expires = seconds.map(|s| now_s() + s);
        let body = Value::Array(vec![
            Value::Text(reason.to_owned()),
            expires.map_or(Value::Null, Value::Uint),
        ])
        .encode();
        let (c, u) = (community.hex(), user.hex());
        if let Err(e) = call::<r::PutBan>(&self.inner, &[&c, &u], Some(body), None).await {
            self.stale(community, &e).await;
            return Err(e);
        }
        self.after(community).await;
        if self.inner.snapshot().people.bans.contains_key(&community) {
            let _ = self.load_bans(community).await;
        }
        Ok(())
    }

    /// controller.ts `unbanMember`: a 404 is success (the request was retried after the first one went through).
    pub async fn unban(&self, community: CommunityId, user: UserId) -> Result<(), ApiError> {
        self.listed(community)?;
        let (c, u) = (community.hex(), user.hex());
        match call::<r::DeleteBan>(&self.inner, &[&c, &u], None, None).await {
            // E_ code: E_NOT_FOUND (no such ban: already lifted).
            Err(e) if e.status != 404 => {
                self.stale(community, &e).await;
                return Err(e);
            }
            _ => {}
        }
        self.after(community).await;
        let _ = self.load_bans(community).await;
        Ok(())
    }

    /// controller.ts `loadBans`: published into `PeopleState::bans`.
    pub async fn load_bans(&self, community: CommunityId) -> Result<Vec<BanSummary>, ApiError> {
        self.listed(community)?;
        let c = community.hex();
        let raw = match call::<r::ListBans>(&self.inner, &[&c], None, None).await {
            Ok(raw) => raw,
            Err(e) => {
                self.stale(community, &e).await;
                return Err(e);
            }
        };
        let rows = decode_bans(&raw).map_err(bad)?;
        let published = rows.clone();
        self.inner.publish(move |s| {
            if s.communities.iter().any(|c| c.id == community) {
                s.people.bans.insert(community, published);
            }
        });
        Ok(rows)
    }

    /// The member's timeout: its expiry in Unix seconds, or `None`.
    pub async fn timeout_of(
        &self,
        community: CommunityId,
        user: UserId,
    ) -> Result<Option<u64>, ApiError> {
        self.listed(community)?;
        let (c, u) = (community.hex(), user.hex());
        let raw = call::<r::GetMemberTimeout>(&self.inner, &[&c, &u], None, None).await?;
        let v = decode(&raw)?;
        let a = at_least(&v, 1).map_err(bad)?;
        let expires = match &a[0] {
            Value::Null => None,
            v => Some(uint(v).map_err(bad)?),
        };
        self.publish_timeout(community, user, expires);
        Ok(expires)
    }

    fn publish_timeout(&self, community: CommunityId, user: UserId, expires: Option<u64>) {
        self.inner.publish(move |s| {
            let map = s.people.timeouts.entry(community).or_default();
            match expires {
                Some(at) => {
                    map.insert(user, at);
                }
                None => {
                    map.remove(&user);
                }
            }
        });
    }

    /// worker `setMemberTimeout` / `clearMemberTimeout`: `length` from now, or lifted with [`TimeoutLength::Clear`]. The
    /// request id is stable across the retries of this one request.
    pub async fn timeout(
        &self,
        community: CommunityId,
        user: UserId,
        length: TimeoutLength,
    ) -> Result<(), ApiError> {
        self.listed(community)?;
        let request = request_id();
        let (c, u) = (community.hex(), user.hex());
        let result = match length.seconds() {
            Some(seconds) => {
                let expires = now_s() + seconds;
                let body = Value::Array(vec![Value::Bytes(request.to_vec()), Value::Uint(expires)])
                    .encode();
                let raw = call::<r::PutMemberTimeout>(
                    &self.inner,
                    &[&c, &u],
                    Some(body),
                    Some(request.to_vec()),
                )
                .await;
                raw.and_then(|raw| {
                    let v = decode(&raw)?;
                    let a = at_least(&v, 2).map_err(bad)?;
                    if bin16(&a[0]).map_err(bad)? != request {
                        return Err(refusal(
                            "E_CBOR_SHAPE",
                            "timeout response request id mismatch",
                        ));
                    }
                    if uint(&a[1]).map_err(bad)? != expires {
                        return Err(refusal("E_CBOR_SHAPE", "timeout response expiry mismatch"));
                    }
                    Ok(Some(expires))
                })
            }
            None => {
                let body = Value::Array(vec![Value::Bytes(request.to_vec())]).encode();
                call::<r::DeleteMemberTimeout>(
                    &self.inner,
                    &[&c, &u],
                    Some(body),
                    Some(request.to_vec()),
                )
                .await
                .map(|_| None)
            }
        };
        match result {
            Ok(expires) => {
                self.publish_timeout(community, user, expires);
                Ok(())
            }
            Err(e) => {
                self.stale(community, &e).await;
                Err(e)
            }
        }
    }

    /// worker `warnMember`: the reason is staged as durable evidence in the core first (bound to its request id), the
    /// warning is posted (the request carries no reason), and the instance's receipt marks the evidence accepted. The
    /// sealed evidence for the Bot (operation 61) is Task 20's Bot operations: until it lands here the evidence stays
    /// staged on this device and the answer says [`Evidence::Pending`], as the web says when the Bot did not answer.
    ///
    /// `request` is the caller's id for this warning: the same id again (a retry after a lost answer) is the same
    /// warning; the same id with another reason or member is refused (E_CONFLICT).
    pub async fn warn(
        &self,
        community: CommunityId,
        user: UserId,
        request: [u8; 16],
        reason: &str,
    ) -> Result<WarnReceipt, ApiError> {
        if reason.trim().is_empty() || reason_problem(reason).is_some() {
            return Err(refusal("E_BAD_INPUT", "manual warning reason is invalid"));
        }
        self.listed(community)?;
        let core = self.inner.engine().core().clone();
        let now = now_s();
        let staged = {
            let reason = reason.to_owned();
            let evidence = request_id();
            try_core(&core, move |c| -> Result<(), ApiError> {
                let rows = c
                    .manual_warning_evidence()
                    .map_err(|e| ApiError::local(0, e.code, &e.detail))?;
                match rows.iter().find(|r| r.request_id == request) {
                    Some(r)
                        if r.community_id != community.0
                            || r.target_user_id != user.0
                            || r.reason != reason =>
                    {
                        Err(refusal(
                            "E_CONFLICT",
                            "warning request id is already bound to another reason or member",
                        ))
                    }
                    Some(_) => Ok(()),
                    None => c
                        .prepare_manual_warning_evidence(
                            &request,
                            &evidence,
                            &community.0,
                            &user.0,
                            &reason,
                            now,
                            now,
                        )
                        .map_err(|e| ApiError::local(0, e.code, &e.detail)),
                }
            })
            .await
            .map_err(|_| refusal("E_CORE_STATE", "the client core stopped"))?
        };
        staged?;
        let body = Value::Array(vec![Value::Bytes(request.to_vec())]).encode();
        let (c, u) = (community.hex(), user.hex());
        let raw =
            match call::<r::WarnMember>(&self.inner, &[&c, &u], Some(body), Some(request.to_vec()))
                .await
            {
                Ok(raw) => raw,
                Err(e) => {
                    self.stale(community, &e).await;
                    return Err(e);
                }
            };
        let v = decode(&raw)?;
        let a = at_least(&v, 4).map_err(bad)?;
        if bin16(&a[0]).map_err(bad)? != request {
            return Err(refusal(
                "E_CBOR_SHAPE",
                "warning response request id mismatch",
            ));
        }
        let action_id = bin16(&a[1]).map_err(bad)?;
        let revision = u53(&a[2]).map_err(bad)?;
        let accepted_at = u53(&a[3]).map_err(bad)?;
        let marked = try_core(&core, move |c| {
            c.mark_manual_warning_accepted(&request, &action_id, revision, accepted_at)
        })
        .await;
        let evidence = match marked {
            Ok(Ok(())) => Evidence::Pending,
            _ => Evidence::Unknown,
        };
        Ok(WarnReceipt {
            request_id: request,
            action_id,
            revision,
            accepted_at,
            evidence,
        })
    }
}

// ---- moderated message removal (worker/controller.ts `moderateRemoveMessage`) ----

/// The removal reason's budget in UTF-8 bytes (controller.ts `moderateRemoveMessage`, the dialog's Remove gate).
pub const REMOVAL_REASON_MAX_BYTES: usize = 1024;
/// The excerpt's budget in UTF-8 bytes (controller.ts).
pub const REMOVAL_EXCERPT_MAX_BYTES: usize = 512;
/// The excerpt the dialog takes from the message: its first 120 characters (Conversation.tsx `excerpt(.., 120)`).
pub const REMOVAL_EXCERPT_CHARS: usize = 120;
/// The share of the instance's 30-second acceptance window a proof uses: five seconds are kept for clock skew.
const PROOF_LIFETIME_S: u64 = 25;
/// The v1 operation of moderation evidence (sealed as 161).
const EVIDENCE_OPERATION: u64 = 61;

/// MessageItem.tsx: "remove for everyone" is offered on another person's stored message to a reader whose channel bits
/// (the one resolver, `roles::viewer_channel_bits`) are known and hold `manage_messages`. Unknown bits offer nothing.
pub fn may_remove_for_everyone(own: bool, stored: bool, bits: Option<u64>) -> bool {
    !own && stored && bits.is_some_and(|b| b & u64::from(perm::MANAGE_MESSAGES) != 0)
}

/// Conversation.tsx: the dialog's Remove acts only on a reason that is not blank and fits 1024 bytes.
pub fn removal_reason_ok(reason: &str) -> bool {
    !reason.trim().is_empty() && reason.len() <= REMOVAL_REASON_MAX_BYTES
}

/// What the instance made of a removal.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Removal {
    Accepted,
    /// The request may have landed (a timeout, a 5xx, a dropped link): the same proof is checked or sent again, never a
    /// new removal.
    Unknown,
    /// The instance refused it (a 4xx other than 408): nothing was removed.
    Rejected,
}

/// The answer of [`Moderation::remove_message`] and [`Moderation::resolve_removal`] (the worker's result object).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct RemovalOutcome {
    pub removal: Removal,
    pub proof_request_id: [u8; 16],
    pub evidence_request_id: Option<[u8; 16]>,
    pub evidence: Evidence,
}

impl RemovalOutcome {
    fn new(
        removal: Removal,
        proof: [u8; 16],
        evidence_request_id: Option<[u8; 16]>,
        evidence: Evidence,
    ) -> Self {
        Self {
            removal,
            proof_request_id: proof,
            evidence_request_id,
            evidence,
        }
    }
}

fn core_err(e: CoreError) -> ApiError {
    ApiError::local(0, e.code, &e.detail)
}

/// Runs `f` on the core; a stopped or poisoned core is E_CORE_STATE.
async fn on_core<T: Send + 'static>(
    inner: &Inner,
    f: impl FnOnce(&mut ClientCore) -> Result<T, ApiError> + Send + 'static,
) -> Result<T, ApiError> {
    try_core(inner.engine().core(), f)
        .await
        .map_err(|_| refusal("E_CORE_STATE", "the client core stopped"))?
}

fn wipe(row: &mut ManualRemovalEvidence) {
    row.reason.zeroize();
    row.excerpt.zeroize();
    row.signed_body.zeroize();
}

/// The core's unresolved removal that `pick` chooses; the plaintext of every other row read is wiped.
async fn removal_row(
    inner: &Inner,
    pick: impl Fn(&ManualRemovalEvidence) -> bool + Send + 'static,
) -> Result<Option<ManualRemovalEvidence>, ApiError> {
    on_core(inner, move |c| {
        let rows = c.manual_removal_evidence().map_err(core_err)?;
        let mut found = None;
        for mut row in rows {
            if found.is_none() && pick(&row) {
                found = Some(row);
            } else {
                wipe(&mut row);
            }
        }
        Ok(found)
    })
    .await
}

/// The core's unresolved removals as `(proof, group, community, state)`, with no plaintext kept.
async fn removal_ids(inner: &Inner) -> Result<Vec<([u8; 16], [u8; 16], [u8; 16], u8)>, ApiError> {
    on_core(inner, |c| {
        let rows = c.manual_removal_evidence().map_err(core_err)?;
        Ok(rows
            .into_iter()
            .map(|mut r| {
                wipe(&mut r);
                (r.proof_request_id, r.group_id, r.community_id, r.state)
            })
            .collect())
    })
    .await
}

/// controller.ts `definiteManualRemovalRefusal`: a 4xx other than 408 is the instance's verdict; a 408, a 5xx and every
/// transport failure may have raced its write, so they stay unknown.
fn definite_refusal(e: &ApiError) -> bool {
    (400..500).contains(&e.status) && e.status != 408
}

/// controller.ts `sealedEvidence`: the evidence as the sealed app operation 161 (v1 61, revision 0) to the community's
/// Bot. Accepted only on the Bot's own success; a refusal of the Bot's (1 invalid, 2 forbidden) is rejected; a Bot that
/// did not take it (offline, no catalog yet) leaves it pending; no answer leaves it unknown. The operations handle is
/// made for this one request and dropped, so it keeps no copy of the plaintext for a resend.
async fn sealed_evidence(
    inner: &Arc<Inner>,
    community: CommunityId,
    request_id: [u8; 16],
    proof: [u8; 16],
    reason: &str,
    excerpt: &str,
) -> Evidence {
    let ops = BotOperations::new(community, default_transport(inner));
    let answer = ops
        .request(BotRequest {
            operation: EVIDENCE_OPERATION,
            channel: None,
            args: vec![
                BotArg::Text(hex(&proof)),
                BotArg::Text(reason.to_owned()),
                BotArg::Text(excerpt.to_owned()),
            ],
            revision: Some(0),
            request_id: Some(request_id),
        })
        .await;
    drop(ops);
    match answer {
        Ok(r) if r.status == 0 => Evidence::Accepted,
        Ok(r) if r.status == 1 || r.status == 2 => Evidence::Rejected,
        Ok(_) | Err(OpError::Refused { .. }) => Evidence::Pending,
        Err(_) => Evidence::Unknown,
    }
}

/// controller.ts `uploadManualRemovalEvidence`: reserves the evidence request id in the core (reused while it is fresh,
/// so a resend is the same request), sends the sealed evidence, and closes the core's row once the Bot stored it.
async fn upload_evidence(
    inner: &Arc<Inner>,
    community: CommunityId,
    proof: [u8; 16],
    reason: &str,
    excerpt: &str,
) -> Evidence {
    let reserved = on_core(inner, move |c| {
        c.ensure_manual_removal_evidence_request(&proof, &request_id(), now_s())
            .map_err(core_err)
    })
    .await;
    let Ok((request, _expires)) = reserved else {
        return Evidence::Unknown;
    };
    let evidence = sealed_evidence(inner, community, request, proof, reason, excerpt).await;
    if evidence == Evidence::Accepted {
        let _ = on_core(inner, move |c| {
            c.complete_manual_removal_evidence(&proof, now_s())
                .map_err(core_err)
        })
        .await;
    }
    evidence
}

/// What a definite refusal of a proof's post does to the core's row.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum OnRefusal {
    /// The removal's own post (and the page's retry): the refusal is the instance's verdict, and the row is retired.
    Retire,
    /// The resume after a ready (controller.ts `resumeManualRemovalEvidence`, `catch { continue; }`): the row stays as
    /// it is, an explicit unknown, retried at the next ready (review A I1).
    Keep,
}

/// Whether the refusal `e` of a proof's post retires its row.
fn retires(e: &ApiError, on: OnRefusal) -> bool {
    on == OnRefusal::Retire && definite_refusal(e)
}

/// POSTs a signed proof the core holds: marked submitted first (a restart then knows it may have landed), accepted on
/// the instance's answer, retired on its refusal when `on` says so. `Err` carries the outcome to answer.
async fn post_proof(
    inner: &Arc<Inner>,
    group: [u8; 16],
    proof: [u8; 16],
    signed: Zeroizing<Vec<u8>>,
    on: OnRefusal,
) -> Result<(), Removal> {
    on_core(inner, move |c| {
        c.mark_manual_removal_submitted(&proof).map_err(core_err)
    })
    .await
    .map_err(|_| Removal::Unknown)?;
    let posted =
        call::<r::PostModerationRemoval>(inner, &[&hex(&group)], Some(signed.to_vec()), None).await;
    match posted {
        Ok(_) => {
            on_core(inner, move |c| {
                c.mark_manual_removal_accepted(&proof).map_err(core_err)
            })
            .await
            .map_err(|_| Removal::Unknown)?;
            // The tombstone: the group's feed is read now (the worker's `refreshTimeline`), not at the next ready.
            inner.engine().request_removal_read(group);
            Ok(())
        }
        Err(e) if retires(&e, on) => {
            let _ = on_core(inner, move |c| {
                c.reject_manual_removal(&proof, now_s()).map_err(core_err)
            })
            .await;
            Err(Removal::Rejected)
        }
        Err(_) => Err(Removal::Unknown),
    }
}

impl Moderation {
    /// controller.ts `moderateRemoveMessage`: removes another person's message `msg` (stored at `seq`) of the community
    /// channel `channel` for everyone, with `reason` and the message's `excerpt` as its evidence.
    ///
    /// Checked first, as the worker checks: the bounds of the reason and excerpt, the channel's active community text
    /// group, the stored row (still readable, still `msg`), and the reader's `manage_messages` in this channel through the
    /// one resolver (rights not read yet are E_NOT_READY). A removal of this message already waiting in the core is
    /// resolved instead of signing another. Then the core signs the proof and keeps it with the evidence before anything
    /// is sent; the proof is posted; and on its acceptance the evidence goes to the Bot. The instance checks the proof and
    /// the right again.
    pub async fn remove_message(
        &self,
        channel: ChannelId,
        msg: MessageId,
        seq: u64,
        reason: &str,
        excerpt: &str,
    ) -> Result<RemovalOutcome, ApiError> {
        let reason = Zeroizing::new(reason.to_owned());
        let excerpt = Zeroizing::new(excerpt.to_owned());
        if seq == 0 {
            return Err(refusal("E_BAD_INPUT", "invalid target sequence"));
        }
        if reason.is_empty()
            || reason.len() > REMOVAL_REASON_MAX_BYTES
            || reason.trim().is_empty()
            || excerpt.len() > REMOVAL_EXCERPT_MAX_BYTES
        {
            return Err(refusal(
                "E_BAD_INPUT",
                "moderation reason or excerpt is outside its allowed bounds",
            ));
        }
        let snapshot = self.inner.snapshot();
        let Some(known) = snapshot.channel(channel).cloned() else {
            return Err(refusal(
                "E_BAD_INPUT",
                "moderation removal needs a community channel",
            ));
        };
        let community = known.community_id;
        let groups = on_core(&self.inner, |c| {
            let raw = c.groups().map_err(core_err)?;
            GroupRow::decode_all(&raw).map_err(bad)
        })
        .await?;
        let group = groups
            .iter()
            .filter(|g| g.kind == 0 && g.target_id == channel.0)
            .find(|g| known.text_group_id.is_none_or(|t| t.0 == g.group_id))
            .filter(|g| g.state == 2 && g.community_id == Some(community.0))
            .map(|g| g.group_id)
            .ok_or_else(|| {
                refusal(
                    "E_NOT_READY",
                    "the active community text group is unavailable",
                )
            })?;
        let stored = on_core(&self.inner, move |c| {
            let raw = Zeroizing::new(c.timeline(&group, seq + 1, 1).map_err(core_err)?);
            let mut rows = decode_timeline(&raw).map_err(bad)?;
            let ok = rows
                .iter()
                .any(|r| r.seq == seq && r.msg_id == Some(msg.0) && r.status == 0);
            for r in &mut rows {
                r.body.zeroize();
            }
            Ok(ok)
        })
        .await?;
        if !stored {
            return Err(refusal(
                "E_BAD_INPUT",
                "target message is no longer available",
            ));
        }
        let Some(bits) = viewer_channel_bits(&snapshot, channel) else {
            return Err(refusal(
                "E_NOT_READY",
                "current channel permissions are unavailable",
            ));
        };
        if bits & u64::from(perm::MANAGE_MESSAGES) == 0 {
            return Err(refusal(
                "E_FORBIDDEN",
                "manage messages permission required",
            ));
        }
        let waiting = removal_row(&self.inner, move |r| {
            r.group_id == group && r.message_id == msg.0 && r.target_sequence == seq
        })
        .await?;
        if let Some(mut row) = waiting {
            let proof = row.proof_request_id;
            wipe(&mut row);
            return self.resolve_removal(proof).await;
        }
        let (proof, evidence_request) = (request_id(), request_id());
        let now = now_s();
        let signed = {
            let (reason, excerpt) = (reason.clone(), excerpt.clone());
            on_core(&self.inner, move |c| {
                c.prepare_manual_removal_evidence(
                    &proof,
                    &evidence_request,
                    &community.0,
                    &group,
                    &channel.0,
                    &msg.0,
                    seq,
                    &reason,
                    &excerpt,
                    now,
                    now + PROOF_LIFETIME_S,
                    now,
                )
                .map(Zeroizing::new)
                .map_err(core_err)
            })
            .await?
        };
        if let Err(removal) = post_proof(&self.inner, group, proof, signed, OnRefusal::Retire).await
        {
            let (evidence_request_id, evidence) = match removal {
                Removal::Rejected => (None, Evidence::Rejected),
                _ => (Some(evidence_request), Evidence::Unknown),
            };
            return Ok(RemovalOutcome::new(
                removal,
                proof,
                evidence_request_id,
                evidence,
            ));
        }
        let evidence = upload_evidence(&self.inner, community, proof, &reason, &excerpt).await;
        Ok(RemovalOutcome::new(
            Removal::Accepted,
            proof,
            Some(evidence_request),
            evidence,
        ))
    }

    /// controller.ts `resolveManualRemoval`: settles a removal the core still holds (the dialog's "Check or retry
    /// removal"). One whose post may have landed is first looked for in the group's verified removal feed; one not yet
    /// accepted is posted again with the same proof (unless it expired); an accepted one has its evidence sent again.
    /// E_NOT_FOUND when the core no longer holds it.
    pub async fn resolve_removal(&self, proof: [u8; 16]) -> Result<RemovalOutcome, ApiError> {
        let gone = || refusal("E_NOT_FOUND", "manual removal request is no longer pending");
        let mut row = removal_row(&self.inner, move |r| r.proof_request_id == proof)
            .await?
            .ok_or_else(gone)?;
        if row.state == 1 {
            // Only an authenticated feed entry of this actor, device, group and sequence promotes it; a failed read
            // proves nothing.
            let _ = Arc::clone(self.inner.engine())
                .recover_removals_of(vec![row.group_id])
                .await;
            wipe(&mut row);
            row = removal_row(&self.inner, move |r| r.proof_request_id == proof)
                .await?
                .ok_or_else(gone)?;
        }
        if row.state < 2 {
            let now = now_s();
            let (group, evidence_request) = (row.group_id, row.evidence_request_id);
            if row.expires <= now {
                let removal = if row.state == 0 {
                    let _ = on_core(&self.inner, move |c| {
                        c.reject_unsubmitted_manual_removal(&proof, now)
                            .map_err(core_err)
                    })
                    .await;
                    Removal::Rejected
                } else {
                    Removal::Unknown
                };
                wipe(&mut row);
                return Ok(RemovalOutcome::new(
                    removal,
                    proof,
                    Some(evidence_request),
                    Evidence::Unknown,
                ));
            }
            let signed = Zeroizing::new(std::mem::take(&mut row.signed_body));
            wipe(&mut row);
            if let Err(removal) =
                post_proof(&self.inner, group, proof, signed, OnRefusal::Retire).await
            {
                let (evidence_request_id, evidence) = match removal {
                    Removal::Rejected => (None, Evidence::Rejected),
                    _ => (Some(evidence_request), Evidence::Unknown),
                };
                return Ok(RemovalOutcome::new(
                    removal,
                    proof,
                    evidence_request_id,
                    evidence,
                ));
            }
            match removal_row(&self.inner, move |r| r.proof_request_id == proof).await? {
                Some(r) if r.state == 2 => row = r,
                other => {
                    let evidence_request = other.map_or([0; 16], |mut r| {
                        wipe(&mut r);
                        r.evidence_request_id
                    });
                    return Ok(RemovalOutcome::new(
                        Removal::Unknown,
                        proof,
                        Some(evidence_request),
                        Evidence::Unknown,
                    ));
                }
            }
        }
        let community = CommunityId(row.community_id);
        let evidence_request = row.evidence_request_id;
        let (reason, excerpt) = (
            Zeroizing::new(std::mem::take(&mut row.reason)),
            Zeroizing::new(std::mem::take(&mut row.excerpt)),
        );
        wipe(&mut row);
        let evidence = upload_evidence(&self.inner, community, proof, &reason, &excerpt).await;
        Ok(RemovalOutcome::new(
            Removal::Accepted,
            proof,
            Some(evidence_request),
            evidence,
        ))
    }

    /// manual-removal.ts `sendManualRemovalEvidence`: the dialog's "Retry evidence" for an accepted removal. A fresh
    /// request id (an earlier reservation's expiry is fixed); the Bot keys the evidence by the proof's id and takes the
    /// same reason and excerpt again, so a late duplicate is safe. Answers the request id and the outcome.
    pub async fn send_removal_evidence(
        &self,
        community: CommunityId,
        proof: [u8; 16],
        reason: &str,
        excerpt: &str,
    ) -> ([u8; 16], Evidence) {
        let request = crate::features::bot_admin::new_request_id();
        let evidence =
            sealed_evidence(&self.inner, community, request, proof, reason, excerpt).await;
        (request, evidence)
    }
}

/// controller.ts `resumeManualRemovalEvidence`, run after each ready: every removal the core still holds for an active
/// group of its community is reconciled (a maybe-landed one against the verified feed), posted again while its proof is
/// fresh, and its evidence sent; whatever fails waits for the next ready. One whose post may have landed stays unknown
/// until the feed says.
pub(crate) async fn resume_removal_evidence(inner: Arc<Inner>) {
    let Ok(saved) = removal_ids(&inner).await else {
        return;
    };
    if saved.is_empty() {
        return;
    }
    let groups = inner.engine().groups().await;
    let mut catalogs = std::collections::HashSet::new();
    for (proof, group, community, state) in saved {
        if inner.engine().stopped() {
            return;
        }
        let active = groups.iter().any(|g| {
            g.group_id == group && g.kind == 0 && g.state == 2 && g.community_id == Some(community)
        });
        if !active {
            continue;
        }
        if state == 1 {
            let _ = Arc::clone(inner.engine())
                .recover_removals_of(vec![group])
                .await;
        }
        let Ok(Some(mut row)) = removal_row(&inner, move |r| r.proof_request_id == proof).await
        else {
            continue;
        };
        if row.state < 2 {
            let now = now_s();
            if row.expires <= now {
                if row.state == 0 {
                    let _ = on_core(&inner, move |c| {
                        c.reject_unsubmitted_manual_removal(&proof, now)
                            .map_err(core_err)
                    })
                    .await;
                }
                wipe(&mut row);
                continue;
            }
            let signed = Zeroizing::new(std::mem::take(&mut row.signed_body));
            wipe(&mut row);
            if post_proof(&inner, group, proof, signed, OnRefusal::Keep)
                .await
                .is_err()
            {
                continue;
            }
            match removal_row(&inner, move |r| r.proof_request_id == proof).await {
                Ok(Some(r)) => row = r,
                _ => continue,
            }
        }
        if row.state != 2 {
            wipe(&mut row);
            continue;
        }
        let (reason, excerpt) = (
            Zeroizing::new(std::mem::take(&mut row.reason)),
            Zeroizing::new(std::mem::take(&mut row.excerpt)),
        );
        wipe(&mut row);
        // At a start the Bot's verified key is not read yet (the catalog sync runs beside this): it is read once per
        // community here, so the owed evidence can be sealed now instead of waiting for another ready.
        if catalogs.insert(community) {
            let _ = crate::features::bot::Bot::of(&inner)
                .sync_catalog(CommunityId(community))
                .await;
        }
        let _ = upload_evidence(&inner, CommunityId(community), proof, &reason, &excerpt).await;
    }
}

#[cfg(test)]
mod removal_tests {
    use super::*;

    const MANAGE: u64 = perm::MANAGE_MESSAGES as u64;

    /// MessageItem.tsx: another person's stored message, to a reader whose known channel bits hold manage_messages.
    #[test]
    fn remove_for_everyone_needs_known_manage_messages_on_another_persons_stored_message() {
        assert!(may_remove_for_everyone(false, true, Some(MANAGE)));
        assert!(may_remove_for_everyone(
            false,
            true,
            Some(u64::from(perm::PERM_ALL))
        ));
        // Unknown bits offer nothing (unlike pin, which unknown bits offer).
        assert!(!may_remove_for_everyone(false, true, None));
        let read_send_pin = 0b11 | u64::from(perm::PIN_MESSAGES);
        assert!(!may_remove_for_everyone(false, true, Some(read_send_pin)));
        // The own message is deleted, not removed; a message not stored yet has no seq to remove.
        assert!(!may_remove_for_everyone(true, true, Some(MANAGE)));
        assert!(!may_remove_for_everyone(false, false, Some(MANAGE)));
    }

    /// Conversation.tsx: Remove acts on a non-blank reason of at most 1024 bytes.
    #[test]
    fn the_reason_is_not_blank_and_fits_1024_bytes() {
        assert!(removal_reason_ok("spam"));
        assert!(!removal_reason_ok(""));
        assert!(!removal_reason_ok(" \n\t "));
        assert!(removal_reason_ok(&"x".repeat(1024)));
        assert!(!removal_reason_ok(&"x".repeat(1025)));
        // Bytes, not characters: 512 two-byte characters fit, 513 do not.
        assert!(removal_reason_ok(&"ä".repeat(512)));
        assert!(!removal_reason_ok(&"ä".repeat(513)));
    }

    /// Review A I1 (controller.ts `resumeManualRemovalEvidence`: `catch { continue; }`): a removal's own post retires it
    /// on a definite refusal, but the resume after a ready never does. The instance answers some 4xx before its replay
    /// check (a closed group's 404, a 429), so a removal that may have landed stays an explicit unknown, retried at the
    /// next ready, and its evidence still reaches the Bot once the feed says it landed.
    #[test]
    fn the_resume_never_retires_a_removal_on_a_refusal() {
        let e = |status| ApiError::local(status, "E_X", "");
        assert!(retires(&e(403), OnRefusal::Retire));
        assert!(!retires(&e(503), OnRefusal::Retire));
        for status in [404, 429, 403, 409, 503] {
            assert!(!retires(&e(status), OnRefusal::Keep), "{status}");
        }
    }

    /// controller.ts `definiteManualRemovalRefusal`.
    #[test]
    fn only_a_4xx_other_than_408_is_a_definite_refusal() {
        let e = |status| ApiError::local(status, "E_X", "");
        for status in [400, 403, 409, 499] {
            assert!(definite_refusal(&e(status)), "{status}");
        }
        // A 408, a 5xx and a local failure (no answer) prove nothing.
        for status in [408, 500, 503, 0] {
            assert!(!definite_refusal(&e(status)), "{status}");
        }
    }
}
