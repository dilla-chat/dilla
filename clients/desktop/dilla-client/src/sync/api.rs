//! The routes the sync engine calls, typed as routes.ts types them, with the pacing-free part of
//! `packages/client-core/src/http/client.ts` around them:
//!
//! - A socket route (the route table's `Socket` rows) goes only through [`SocketRequests`]; a route meter's 429 is
//!   waited out (Retry-After from the body, else 1 s) up to five attempts unless the call opts out (`postMessage`).
//!   Every refusal reaches the caller decoded (`[code, detail, delay|null, ...extra]`, internal/server/errors.go). A 401
//!   there is the revocation of the connection's session and is never re-authenticated.
//! - An HTTP route (the rest) is retried as client.ts retries it: one re-authentication on a 401 (a new session from the
//!   device key: challenge, `session_sign`, `POST /sessions`), a 429 or 503 waited out (1 s: reqwest's answer carries no
//!   body here), and an idempotent call's network failure or 500 backed off `min(8 s, 500 ms * 2^(n-1)) + jitter`, at most
//!   five attempts. Only the status reaches the caller.
//! - Every refusal code is handed to the refusal hook: E_FACTOR_REQUIRED moves the client to its factor-setup state.

use std::sync::{Arc, Mutex};
use std::time::Duration;

use dilla_core::client::ClientCore;
use zeroize::Zeroizing;

use crate::auth::AuthToken;
use crate::gateway::frames::Value;
use crate::http::ReqwestHttp;
use crate::http::fill_path;
use crate::http::routes::{self as r, NoBody, Raw};
use crate::seams::{Http, HttpError, Route};

use super::requests::{Answer, SocketRequests};
use super::value::{self, DecodeError, Page, arr, at_least, bin16, bin32, page, text, uint};

/// The store's client core, shared by the engine's lanes; every call runs on the blocking pool.
pub type CoreHandle = Arc<Mutex<ClientCore>>;

/// The core's lock is poisoned: a call panicked while it held the core, which may be left inconsistent. Nothing uses
/// it again; the client stops ([`crate::state::AccountPhase::Failed`]).
#[derive(Debug, Clone, Copy, PartialEq, Eq, thiserror::Error)]
#[error("E_CORE_POISONED: the local store's core panicked and is no longer used")]
pub struct CorePoisoned;

/// Runs `f` on the core off the async workers (MLS and SQLCipher work can take a while). A poisoned core is never
/// used: `f` is not run and the answer is [`CorePoisoned`], also when `f` itself panics (which poisons it).
pub async fn try_core<T: Send + 'static>(
    core: &CoreHandle,
    f: impl FnOnce(&mut ClientCore) -> T + Send + 'static,
) -> Result<T, CorePoisoned> {
    let core = core.clone();
    tokio::task::spawn_blocking(move || {
        let mut guard = core.lock().map_err(|_| CorePoisoned)?;
        Ok(f(&mut guard))
    })
    .await
    .unwrap_or(Err(CorePoisoned))
}

/// [`try_core`] for the engine's jobs: a poisoned core ends the calling job (it unwinds; the engine reports itself
/// stopped from then on, see `Engine::stopped`) instead of running on a core a panic may have left inconsistent.
pub async fn with_core<T: Send + 'static>(
    core: &CoreHandle,
    f: impl FnOnce(&mut ClientCore) -> T + Send + 'static,
) -> T {
    match try_core(core, f).await {
        Ok(v) => v,
        Err(e) => panic!("{e}"),
    }
}

/// http/errors.ts `DillaHttpError`: status 0 is a failure before any answer (E_NETWORK, E_OUTCOME_UNKNOWN).
#[derive(Clone, Debug, thiserror::Error)]
#[error("{code} ({status}){}", if detail.is_empty() { String::new() } else { format!(": {detail}") })]
pub struct ApiError {
    pub status: u16,
    pub code: String,
    pub detail: String,
    pub retry_after_ms: Option<u64>,
    pub extra: Vec<Value>,
}

impl ApiError {
    pub fn local(status: u16, code: &str, detail: &str) -> Self {
        Self {
            status,
            code: code.to_owned(),
            detail: detail.to_owned(),
            retry_after_ms: None,
            extra: Vec::new(),
        }
    }

    /// errors.ts `decodeErrorBody`: `[code, detail, delay|null, ...extra]`, else E_HTTP.
    pub fn from_body(status: u16, body: &[u8]) -> Self {
        let mut e = Self::local(status, "E_HTTP", "");
        let Ok(Value::Array(a)) = Value::decode(body) else {
            return e;
        };
        if a.len() < 3 {
            return e;
        }
        let code_ok = |c: &str| {
            c.len() > 2
                && c.len() <= 66
                && c.starts_with("E_")
                && c[2..]
                    .bytes()
                    .all(|b| b.is_ascii_uppercase() || b.is_ascii_digit() || b == b'_')
        };
        let (Some(code), Some(detail)) = (a[0].as_text(), a[1].as_text()) else {
            return e;
        };
        let delay = match &a[2] {
            Value::Null => None,
            Value::Uint(n) => Some(*n),
            _ => return e,
        };
        if !code_ok(code) {
            return e;
        }
        e.code = code.to_owned();
        e.detail = detail.to_owned();
        e.retry_after_ms = delay;
        e.extra = a[3..].to_vec();
        e
    }

    fn of_http(e: &HttpError) -> Self {
        match e {
            HttpError::Status(s) => Self::local(*s, "E_HTTP", ""),
            HttpError::Transport(d) => Self::local(0, "E_NETWORK", d),
            HttpError::Decode(d) => Self::local(0, "E_DECODE", d),
        }
    }

    fn decode(e: DecodeError) -> Self {
        Self::local(0, "E_DECODE", e.0)
    }

    pub fn is(&self, status: u16, code: &str) -> bool {
        self.status == status && self.code == code
    }
}

/// client.ts `RETRY`.
pub const RETRY_MAX_ATTEMPTS: u32 = 5;
const RETRY_BASE_MS: u64 = 500;
const RETRY_CAP_MS: u64 = 8_000;
const RETRY_JITTER_MS: u64 = 250;
const RETRY_MAX_WAIT_MS: u64 = 60_000;
/// session.ts `RENEW_MARGIN_S`.
pub const RENEW_MARGIN_S: u64 = 3600;

/// routes.ts `RowsPage` / `WelcomesPage`.
pub type RowsPage = Page;

/// `GET /v1/users/{id}/device-list`'s head.
pub struct DeviceListHead {
    pub version: u64,
    pub blob: Vec<u8>,
    pub raw: Vec<u8>,
}

/// routes.ts `GroupHealState`.
pub struct HealState {
    pub epoch: u64,
    pub next_seq: u64,
    pub generation: u64,
    pub needs_heal: bool,
}

/// What the client needs to know about a refusal right away (http client.ts `onRefusal`).
pub type RefusalHook = Arc<dyn Fn(&str) + Send + Sync>;

/// Why a session could not be established.
#[derive(Debug, Clone)]
pub enum SessionError {
    /// The store holds no ready identity (phase 2).
    NoIdentity,
    /// The instance no longer answers this device's challenge (revoked); the stored session was cleared.
    Revoked,
    /// The answer's scope is not one this phase accepts (session.ts E_SESSION_SCOPE).
    Scope,
    Api(ApiError),
    Core(String),
}

/// One instance's routes, over the device session in `auth`.
pub struct Api {
    http: ReqwestHttp,
    socket: Arc<SocketRequests>,
    auth: AuthToken,
    core: CoreHandle,
    on_refusal: RefusalHook,
    /// One session establishment at a time (session.ts `inflight`).
    establishing: tokio::sync::Mutex<()>,
}

fn jitter(max: u64) -> u64 {
    let mut b = [0u8; 8];
    let _ = getrandom::fill(&mut b);
    u64::from_le_bytes(b) % max.max(1)
}

fn hex(b: &[u8]) -> String {
    crate::state::hex(b)
}

fn random_idem() -> Vec<u8> {
    let mut b = vec![0u8; 16];
    let _ = getrandom::fill(&mut b);
    b
}

/// routes.ts `messageIdem`: SHA-256("dilla/message-idem/v1\0" || msg_id || body)[..16], so a send written again after a
/// lost socket or a restart is answered from the instance's replay record.
pub fn message_idem(msg_id: &[u8; 16], body: &[u8]) -> Vec<u8> {
    let mut pre = Vec::with_capacity(22 + 16 + body.len());
    pre.extend_from_slice(b"dilla/message-idem/v1\0");
    pre.extend_from_slice(msg_id);
    pre.extend_from_slice(body);
    dilla_core::identity::sha256(&pre)[..16].to_vec()
}

impl Api {
    pub fn new(
        http: ReqwestHttp,
        socket: Arc<SocketRequests>,
        auth: AuthToken,
        core: CoreHandle,
        on_refusal: RefusalHook,
    ) -> Self {
        Self {
            http,
            socket,
            auth,
            core,
            on_refusal,
            establishing: tokio::sync::Mutex::new(()),
        }
    }

    pub fn socket(&self) -> &Arc<SocketRequests> {
        &self.socket
    }

    // ---- the transports ----

    /// One socket route: `R`'s table row names its path, method, accepted statuses and body bound.
    async fn socket_call<R: Route>(
        &self,
        args: &[&str],
        body: Option<Vec<u8>>,
        idem: Option<Vec<u8>>,
        retry429: bool,
    ) -> Result<Answer, ApiError> {
        let entry = r::lookup(&R::METHOD, R::PATH)
            .ok_or_else(|| ApiError::local(0, "E_ROUTE", "the route is not in the table"))?;
        let route = fill_path(entry.path, args).map_err(|e| ApiError::of_http(&e))?;
        let method: &'static str = match R::METHOD {
            http::Method::GET => "GET",
            http::Method::POST => "POST",
            http::Method::PUT => "PUT",
            http::Method::PATCH => "PATCH",
            http::Method::DELETE => "DELETE",
            _ => return Err(ApiError::local(0, "E_ROUTE", "unsupported method")),
        };
        let idem = if method == "GET" {
            None
        } else {
            Some(idem.unwrap_or_else(random_idem))
        };
        let body = body.unwrap_or_default();
        for attempt in 1..=RETRY_MAX_ATTEMPTS {
            let answer = self
                .socket
                .request(method, route.clone(), body.clone(), idem.clone())
                .await?;
            if entry.accepts(answer.status) {
                if let Some(cap) = entry.max_body
                    && answer.body.len() as u64 > cap
                {
                    return Err(ApiError::local(
                        answer.status,
                        "E_BODY_TOO_LARGE",
                        "the response body is too large",
                    ));
                }
                return Ok(answer);
            }
            let error = ApiError::from_body(answer.status, &answer.body);
            if error.status == 429 && retry429 && attempt < RETRY_MAX_ATTEMPTS {
                let wait = error.retry_after_ms.unwrap_or(1000);
                if wait <= RETRY_MAX_WAIT_MS {
                    tokio::time::sleep(Duration::from_millis(wait)).await;
                    continue;
                }
            }
            (self.on_refusal)(&error.code);
            return Err(error);
        }
        unreachable!("the loop returns on its last attempt")
    }

    /// One HTTP route with client.ts's retries and its one re-authentication.
    async fn http_call<R: Route>(
        &self,
        args: &[&str],
        req: R::Request,
        idempotent: bool,
    ) -> Result<R::Response, ApiError>
    where
        R::Request: Clone,
    {
        let auth = r::lookup(&R::METHOD, R::PATH).is_some_and(|e| e.auth);
        let mut reauthenticated = false;
        for attempt in 1..=RETRY_MAX_ATTEMPTS {
            let error = match self.http.call::<R>(args, req.clone()).await {
                Ok(v) => return Ok(v),
                Err(e) => ApiError::of_http(&e),
            };
            if error.status == 401 && auth && !reauthenticated {
                reauthenticated = true;
                if self.establish().await.is_ok() && attempt < RETRY_MAX_ATTEMPTS {
                    continue;
                }
            }
            if (error.status == 429 || error.status == 503) && attempt < RETRY_MAX_ATTEMPTS {
                tokio::time::sleep(Duration::from_millis(1000)).await;
                continue;
            }
            if idempotent
                && (error.status == 0 || error.status == 500)
                && attempt < RETRY_MAX_ATTEMPTS
            {
                let wait =
                    RETRY_CAP_MS.min(RETRY_BASE_MS << (attempt - 1)) + jitter(RETRY_JITTER_MS);
                tokio::time::sleep(Duration::from_millis(wait)).await;
                continue;
            }
            if error.status != 0 {
                (self.on_refusal)(&error.code);
            }
            return Err(error);
        }
        unreachable!("the loop returns on its last attempt")
    }

    // ---- the device session (session.ts) ----

    /// session.ts `ensure`: a stored session that is not within an hour of either expiry is used as it is; otherwise a
    /// new one is established.
    pub async fn ensure_session(&self, now_s: u64) -> Result<(), SessionError> {
        let stored = with_core(&self.core, |c| c.session())
            .await
            .map_err(|e| SessionError::Core(e.to_string()))?;
        if let Some((token, expires, idle)) = decode_session(&stored)
            && expires.saturating_sub(now_s) > RENEW_MARGIN_S
            && idle.saturating_sub(now_s) > RENEW_MARGIN_S
        {
            self.auth.set(token.as_str());
            return Ok(());
        }
        self.establish().await
    }

    /// session.ts `establish`: challenge, sign with the device key (purpose 0), `POST /sessions`; a 401 is tried once more,
    /// and a second one clears the stored session (the device was revoked).
    pub async fn establish(&self) -> Result<(), SessionError> {
        let _one = self.establishing.lock().await;
        let identity = with_core(&self.core, |c| c.identity())
            .await
            .map_err(|e| SessionError::Core(e.to_string()))?;
        let id = Identity::decode(&identity).ok_or(SessionError::NoIdentity)?;
        let (Some(device), 2) = (id.device_id, id.phase) else {
            return Err(SessionError::NoIdentity);
        };
        let mut result = self.attempt_session(&device).await?;
        if result.is_none() {
            result = self.attempt_session(&device).await?;
        }
        let Some(answer) = result else {
            let _ = with_core(&self.core, |c| c.session_clear()).await;
            self.auth.clear();
            return Err(SessionError::Revoked);
        };
        // session.ts: past the publication of the own list only scope 0 is accepted; before it, a pending session too.
        let accepted: &[u8] = if id.list_published { &[0] } else { &[0, 1] };
        if !accepted.contains(&answer.scope) {
            return Err(SessionError::Scope);
        }
        let token = answer.token.clone();
        let (expires, idle) = (answer.expires, answer.idle_expires);
        let stored = token.clone();
        with_core(&self.core, move |c| c.session_store(&stored, expires, idle))
            .await
            .map_err(|e| SessionError::Core(e.to_string()))?;
        self.auth.set(token.as_str());
        Ok(())
    }

    async fn attempt_session(
        &self,
        device: &[u8; 16],
    ) -> Result<Option<r::SessionAnswer>, SessionError> {
        let device_hex = hex(device);
        let nonce = self
            .challenge(&device_hex)
            .await
            .map_err(SessionError::Api)?
            .nonce;
        let body = with_core(&self.core, move |c| c.session_sign(&nonce, 0))
            .await
            .map_err(|e| SessionError::Core(e.to_string()))?;
        match self
            .http
            .call::<r::PostSession>(&[&device_hex], Raw(body))
            .await
        {
            Ok(answer) => Ok(Some(answer)),
            Err(HttpError::Status(401)) => Ok(None),
            Err(e) => Err(SessionError::Api(ApiError::of_http(&e))),
        }
    }

    /// `POST /sessions/challenge`, retried as an idempotent call; it takes no bearer, so it never re-authenticates (and
    /// so never re-enters [`Api::establish`]).
    async fn challenge(&self, device_hex: &str) -> Result<r::ChallengeAnswer, ApiError> {
        for attempt in 1..=RETRY_MAX_ATTEMPTS {
            let error = match self
                .http
                .call::<r::PostChallenge>(&[device_hex], Raw::empty_list())
                .await
            {
                Ok(v) => return Ok(v),
                Err(e) => ApiError::of_http(&e),
            };
            if attempt < RETRY_MAX_ATTEMPTS {
                if error.status == 429 || error.status == 503 {
                    tokio::time::sleep(Duration::from_millis(1000)).await;
                    continue;
                }
                if error.status == 0 || error.status == 500 {
                    let wait =
                        RETRY_CAP_MS.min(RETRY_BASE_MS << (attempt - 1)) + jitter(RETRY_JITTER_MS);
                    tokio::time::sleep(Duration::from_millis(wait)).await;
                    continue;
                }
            }
            return Err(error);
        }
        unreachable!("the loop returns on its last attempt")
    }

    // ---- HTTP routes ----

    /// routes.ts `getInstance`: `[.., .., .., instance_id, generation, name, mode, methods, policy_version, .., external_sender_pub]`.
    pub async fn get_instance(&self) -> Result<InstanceInfo, ApiError> {
        let Raw(raw) = self.http_call::<r::GetInstance>(&[], NoBody, true).await?;
        let v = value::decode(&raw).map_err(ApiError::decode)?;
        let a = at_least(&v, 11).map_err(ApiError::decode)?;
        Ok(InstanceInfo {
            instance_id: bin16(&a[3]).map_err(ApiError::decode)?,
            policy_version: uint(&a[8]).map_err(ApiError::decode)?,
            external_sender_pub: bin32(&a[10]).map_err(ApiError::decode)?,
        })
    }

    /// routes.ts `getDeviceList`: `None` on 404.
    pub async fn get_device_list(
        &self,
        user: &[u8; 16],
    ) -> Result<Option<DeviceListHead>, ApiError> {
        let reply = self
            .http_call::<r::GetDeviceList>(&[&hex(user)], NoBody, true)
            .await?;
        if reply.status == 404 {
            return Ok(None);
        }
        let v = value::decode(&reply.body).map_err(ApiError::decode)?;
        let a = at_least(&v, 2).map_err(ApiError::decode)?;
        Ok(Some(DeviceListHead {
            version: uint(&a[0]).map_err(ApiError::decode)?,
            blob: value::bytes(&a[1]).map_err(ApiError::decode)?,
            raw: reply.body,
        }))
    }

    /// routes.ts `getDeviceListHistory`: at most 64 rows after `after`.
    pub async fn get_device_list_history(
        &self,
        user: &[u8; 16],
        after: u64,
    ) -> Result<(Vec<u8>, Vec<Value>), ApiError> {
        let Raw(raw) = self
            .http_call::<r::GetDeviceListHistory>(&[&hex(user), &after.to_string()], NoBody, true)
            .await?;
        let rows = match value::decode(&raw).map_err(ApiError::decode)? {
            Value::Array(rows) if rows.len() <= 64 => rows,
            _ => return Err(ApiError::decode(DecodeError("device-list history page"))),
        };
        Ok((raw, rows))
    }

    // ---- socket routes ----

    pub async fn list_communities(&self) -> Result<Vec<u8>, ApiError> {
        Ok(self
            .socket_call::<r::ListCommunities>(&[], None, None, true)
            .await?
            .body)
    }

    pub async fn list_channels(&self, community: &[u8; 16]) -> Result<Vec<u8>, ApiError> {
        Ok(self
            .socket_call::<r::ListChannels>(&[&hex(community)], None, None, true)
            .await?
            .body)
    }

    /// routes.ts `listMembers`: every page, as one list of raw pages.
    pub async fn list_members(&self, community: &[u8; 16]) -> Result<Vec<Vec<u8>>, ApiError> {
        let c = hex(community);
        let mut pages = Vec::new();
        let mut after: Option<[u8; 16]> = None;
        loop {
            let after_hex = after.map(|a| hex(&a));
            let args: Vec<&str> = match &after_hex {
                Some(a) => vec![c.as_str(), a.as_str()],
                None => vec![c.as_str()],
            };
            let body = self
                .socket_call::<r::ListMembers>(&args, None, None, true)
                .await?
                .body;
            let v = value::decode(&body).map_err(ApiError::decode)?;
            let rows = v
                .as_array()
                .ok_or_else(|| ApiError::decode(DecodeError("members")))?;
            let n = rows.len();
            let last = match rows.last() {
                Some(row) => Some(
                    bin16(&at_least(row, 1).map_err(ApiError::decode)?[0])
                        .map_err(ApiError::decode)?,
                ),
                None => None,
            };
            pages.push(body);
            if n < crate::state::members::MEMBERS_PAGE {
                return Ok(pages);
            }
            if let (Some(last), Some(prev)) = (last, after)
                && last <= prev
            {
                return Err(ApiError::decode(DecodeError(
                    "members page did not advance",
                )));
            }
            after = last;
        }
    }

    /// routes.ts `getChannel`'s text group: element 11 of the channel row.
    pub async fn get_channel_text_group(
        &self,
        channel: &[u8; 16],
    ) -> Result<Option<[u8; 16]>, ApiError> {
        let body = self
            .socket_call::<r::GetChannel>(&[&hex(channel)], None, None, true)
            .await?
            .body;
        let v = value::decode(&body).map_err(ApiError::decode)?;
        let a = at_least(&v, 12).map_err(ApiError::decode)?;
        value::opt(&a[11], bin16).map_err(ApiError::decode)
    }

    /// routes.ts `postGroup`: `[.., next_seq]`.
    pub async fn post_group(&self, body: Vec<u8>) -> Result<u64, ApiError> {
        let a = self
            .socket_call::<r::PostGroup>(&[], Some(body), None, true)
            .await?;
        let v = value::decode(&a.body).map_err(ApiError::decode)?;
        uint(&at_least(&v, 2).map_err(ApiError::decode)?[1]).map_err(ApiError::decode)
    }

    pub async fn get_group_info(&self, g: &[u8; 16]) -> Result<Vec<u8>, ApiError> {
        Ok(self
            .socket_call::<r::GetGroupInfo>(&[&hex(g)], None, None, true)
            .await?
            .body)
    }

    pub async fn get_group_tree(&self, g: &[u8; 16]) -> Result<Vec<u8>, ApiError> {
        Ok(self
            .socket_call::<r::GetGroupTree>(&[&hex(g)], None, None, true)
            .await?
            .body)
    }

    /// routes.ts `getGroupHealState`: `[epoch, next_seq, generation, need_from_seq, needs_heal]`.
    pub async fn get_group_heal_state(&self, g: &[u8; 16]) -> Result<HealState, ApiError> {
        let body = self
            .socket_call::<r::GetGroupHealState>(&[&hex(g)], None, None, true)
            .await?
            .body;
        let v = value::decode(&body).map_err(ApiError::decode)?;
        let a = arr(&v, 5).map_err(ApiError::decode)?;
        let needs = uint(&a[4]).map_err(ApiError::decode)?;
        if needs > 1 {
            return Err(ApiError::decode(DecodeError("needs_heal is 0 or 1")));
        }
        Ok(HealState {
            epoch: uint(&a[0]).map_err(ApiError::decode)?,
            next_seq: uint(&a[1]).map_err(ApiError::decode)?,
            generation: uint(&a[2]).map_err(ApiError::decode)?,
            needs_heal: needs == 1,
        })
    }

    /// routes.ts `postGroupHeal`: `[epoch, next_seq]`.
    pub async fn post_group_heal(
        &self,
        g: &[u8; 16],
        snapshot: Vec<u8>,
    ) -> Result<(u64, u64), ApiError> {
        let body = self
            .socket_call::<r::PostGroupHeal>(&[&hex(g)], Some(snapshot), None, true)
            .await?
            .body;
        seq_epoch(&body, 2)
    }

    pub async fn get_handshakes(
        &self,
        g: &[u8; 16],
        from: u64,
        limit: usize,
    ) -> Result<RowsPage, ApiError> {
        let body = self
            .socket_call::<r::GetHandshakes>(
                &[&hex(g), &from.to_string(), &limit.to_string()],
                None,
                None,
                true,
            )
            .await?
            .body;
        page(body).map_err(ApiError::decode)
    }

    pub async fn get_messages(
        &self,
        g: &[u8; 16],
        from: u64,
        limit: usize,
    ) -> Result<RowsPage, ApiError> {
        let body = self
            .socket_call::<r::GetMessages>(
                &[&hex(g), &from.to_string(), &limit.to_string()],
                None,
                None,
                true,
            )
            .await?
            .body;
        page(body).map_err(ApiError::decode)
    }

    pub async fn get_proposals(&self, g: &[u8; 16]) -> Result<Vec<u8>, ApiError> {
        Ok(self
            .socket_call::<r::GetProposals>(&[&hex(g)], None, None, true)
            .await?
            .body)
    }

    /// routes.ts `postCommit`: `[seq, epoch]`.
    pub async fn post_commit(&self, g: &[u8; 16], body: Vec<u8>) -> Result<(u64, u64), ApiError> {
        let a = self
            .socket_call::<r::PostCommit>(&[&hex(g)], Some(body), None, true)
            .await?;
        seq_epoch(&a.body, 2)
    }

    /// routes.ts `postMessage`: the raw answer (the core's `send_confirm` reads it), idempotent per message and body, and
    /// a 429 handed back to the send lane (slow mode is the lane's to wait out).
    pub async fn post_message(
        &self,
        g: &[u8; 16],
        body: Vec<u8>,
        msg_id: &[u8; 16],
    ) -> Result<Vec<u8>, ApiError> {
        let idem = message_idem(msg_id, &body);
        let a = self
            .socket_call::<r::PostMessage>(&[&hex(g)], Some(body), Some(idem), false)
            .await?;
        let v = value::decode(&a.body).map_err(ApiError::decode)?;
        uint(&at_least(&v, 1).map_err(ApiError::decode)?[0]).map_err(ApiError::decode)?;
        Ok(a.body)
    }

    pub async fn post_cursor(&self, g: &[u8; 16], body: Vec<u8>) -> Result<(), ApiError> {
        self.socket_call::<r::PostCursor>(&[&hex(g)], Some(body), None, true)
            .await?;
        Ok(())
    }

    /// routes.ts `postResync`: `[seq, epoch]`.
    pub async fn post_resync(&self, g: &[u8; 16], body: Vec<u8>) -> Result<(u64, u64), ApiError> {
        let a = self
            .socket_call::<r::PostResync>(&[&hex(g)], Some(body), None, true)
            .await?;
        seq_epoch(&a.body, 2)
    }

    pub async fn get_welcomes(&self, after: u64) -> Result<RowsPage, ApiError> {
        let body = self
            .socket_call::<r::GetWelcomes>(&[&after.to_string()], None, None, true)
            .await?
            .body;
        page(body).map_err(ApiError::decode)
    }

    pub async fn delete_welcome(&self, id: u64) -> Result<(), ApiError> {
        self.socket_call::<r::DeleteWelcome>(&[&id.to_string()], None, None, true)
            .await?;
        Ok(())
    }
}

fn seq_epoch(body: &[u8], n: usize) -> Result<(u64, u64), ApiError> {
    let v = value::decode(body).map_err(ApiError::decode)?;
    let a = at_least(&v, n).map_err(ApiError::decode)?;
    Ok((
        uint(&a[0]).map_err(ApiError::decode)?,
        uint(&a[1]).map_err(ApiError::decode)?,
    ))
}

/// routes.ts `Instance`, the part sync needs.
#[derive(Clone, Copy, Debug)]
pub struct InstanceInfo {
    pub instance_id: [u8; 16],
    pub policy_version: u64,
    pub external_sender_pub: [u8; 32],
}

/// core-port.ts `IdentityInfo`: `[phase, instance_id|null, user_id|null, device_id|null, username, list_published]`.
#[derive(Clone, Debug)]
pub struct Identity {
    pub phase: u8,
    pub instance_id: Option<[u8; 16]>,
    pub user_id: Option<[u8; 16]>,
    pub device_id: Option<[u8; 16]>,
    pub username: String,
    pub list_published: bool,
}

impl Identity {
    pub fn decode(raw: &[u8]) -> Option<Self> {
        let v = value::decode(raw).ok()?;
        let a = arr(&v, 6).ok()?;
        let phase = uint(&a[0]).ok()?;
        if phase > 3 {
            return None;
        }
        Some(Self {
            phase: phase as u8,
            instance_id: value::opt(&a[1], bin16).ok()?,
            user_id: value::opt(&a[2], bin16).ok()?,
            device_id: value::opt(&a[3], bin16).ok()?,
            username: text(&a[4]).ok()?,
            list_published: uint(&a[5]).ok()? == 1,
        })
    }
}

/// core-port.ts `SessionRecord`: `[token, expires, idle_expires]` or null.
fn decode_session(raw: &[u8]) -> Option<(Zeroizing<String>, u64, u64)> {
    let v = value::decode(raw).ok()?;
    let a = arr(&v, 3).ok()?;
    Some((
        Zeroizing::new(text(&a[0]).ok()?),
        uint(&a[1]).ok()?,
        uint(&a[2]).ok()?,
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn error_bodies_decode_as_errors_ts() {
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(4)
            .text("E_COMMIT_INVALID")
            .text("stale")
            .uint(1500)
            .text("epoch");
        let err = ApiError::from_body(422, &e.into_vec());
        assert_eq!(
            (err.status, err.code.as_str(), err.detail.as_str()),
            (422, "E_COMMIT_INVALID", "stale")
        );
        assert_eq!(err.retry_after_ms, Some(1500));
        assert_eq!(err.extra, vec![Value::Text("epoch".into())]);
        // Anything else is E_HTTP with the status.
        assert_eq!(ApiError::from_body(500, b"\xff").code, "E_HTTP");
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(3).text("lower").text("").null();
        assert_eq!(ApiError::from_body(400, &e.into_vec()).code, "E_HTTP");
    }

    #[test]
    fn message_idem_is_the_ts_derivation() {
        // routes.ts messageIdem over msg id 0x01*16 and body "x": the first 16 bytes of SHA-256(domain || id || body).
        let mut pre = b"dilla/message-idem/v1\0".to_vec();
        pre.extend_from_slice(&[1; 16]);
        pre.push(b'x');
        assert_eq!(
            message_idem(&[1; 16], b"x"),
            dilla_core::identity::sha256(&pre)[..16].to_vec()
        );
        assert_eq!(message_idem(&[1; 16], b"x").len(), 16);
    }
}
