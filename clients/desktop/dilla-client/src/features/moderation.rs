//! Area **members** (Task 14): kick, ban, unban, timeout and warn with evidence.
//!
//! Ports: packages/client-core/src/admin/controller.ts (`kickMember`, `banMember`, `unbanMember`, `loadBans`), the
//! worker's timeout and warning commands (worker/controller.ts `setMemberTimeout`, `clearMemberTimeout`,
//! `getMemberTimeout`, `warnMember`), and the input checks of packages/web/src/screens/admin/Moderation.tsx.
//!
//! The view offers an action only where `features::members::menu_for` says the caller may take it, but that gate is
//! advisory: the instance decides each request again. A 403 means the access the view held is stale, so the access
//! and the member list are read again before the refusal is handed on with its own code.

use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use crate::client::{Client, Inner};
use crate::features::members::reload_members;
use crate::features::roles::{call, load_access, refusal, request_id};
use crate::gateway::frames::Value;
use crate::http::routes as r;
use crate::state::members::BanSummary;
use crate::state::{CommunityId, UserId};
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
    /// The handle of the client `inner` (for another area's own use).
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
