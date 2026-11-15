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
    core_call(core, f).await.map_err(|_| CorePoisoned)
}

/// Why a core call did not answer.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum CoreFailure {
    /// The lock is poisoned, or the call itself panicked.
    Poisoned,
    /// The runtime is shutting down (the client stops: `Client::stop`, Forget's close): the call never ran.
    Stopped,
}

async fn core_call<T: Send + 'static>(
    core: &CoreHandle,
    f: impl FnOnce(&mut ClientCore) -> T + Send + 'static,
) -> Result<T, CoreFailure> {
    let core = core.clone();
    core_outcome(
        tokio::task::spawn_blocking(move || {
            let mut guard = core.lock().map_err(|_| CorePoisoned)?;
            Ok(f(&mut guard))
        })
        .await,
    )
}

/// A blocking core call's outcome: a call cancelled by the runtime's shutdown is the client stopping, not a poisoned core
/// (review M1: Forget's close logged a false E_CORE_POISONED panic); a panic or a poisoned lock is.
pub(crate) fn core_outcome<T>(
    joined: Result<Result<T, CorePoisoned>, tokio::task::JoinError>,
) -> Result<T, CoreFailure> {
    match joined {
        Ok(Ok(v)) => Ok(v),
        Ok(Err(CorePoisoned)) => Err(CoreFailure::Poisoned),
        Err(e) if e.is_cancelled() => Err(CoreFailure::Stopped),
        Err(_) => Err(CoreFailure::Poisoned),
    }
}

/// [`try_core`] for the engine's jobs: a poisoned core ends the calling job (it unwinds; the engine reports itself
/// stopped from then on, see `Engine::stopped`) instead of running on a core a panic may have left inconsistent. A call
/// the runtime's shutdown cancelled ends the job quietly: it waits for the runtime to drop it.
pub async fn with_core<T: Send + 'static>(
    core: &CoreHandle,
    f: impl FnOnce(&mut ClientCore) -> T + Send + 'static,
) -> T {
    match core_call(core, f).await {
        Ok(v) => v,
        Err(CoreFailure::Stopped) => std::future::pending().await,
        Err(CoreFailure::Poisoned) => panic!("{CorePoisoned}"),
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
            HttpError::Status {
                status,
                code,
                retry_after_ms,
            } => {
                let mut e = Self::local(*status, code.as_deref().unwrap_or("E_HTTP"), "");
                e.retry_after_ms = *retry_after_ms;
                e
            }
            HttpError::Transport(d) => Self::local(0, "E_NETWORK", d),
            HttpError::Decode(d) => Self::local(0, "E_DECODE", d),
            HttpError::Unanswered { code, detail } => Self::local(0, code, detail),
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

/// Called when a session established again over HTTP meets an instance that fails the signer pin (final review I1): the
/// client stops its gateway and fails the account closed, as controller.ts `failInstanceSignerPin` does.
pub type PinRefusedHook = Arc<dyn Fn() + Send + Sync>;

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
    /// Set once by the client ([`Api::on_pin_refused`]).
    pin_refused: std::sync::OnceLock<PinRefusedHook>,
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
            pin_refused: std::sync::OnceLock::new(),
            establishing: tokio::sync::Mutex::new(()),
        }
    }

    /// What runs when a session established again over HTTP fails the instance signer pin (set once; a later call is
    /// ignored).
    pub fn on_pin_refused(&self, hook: PinRefusedHook) {
        let _ = self.pin_refused.set(hook);
    }

    /// controller.ts `failInstanceSignerPin`, the HTTP half: the new session goes (every later call asks for one again
    /// and meets the same check), the refusal is recorded, and the client is told, so it stops the gateway and fails the
    /// account closed. Answers the refusal to return.
    pub(crate) fn refuse_pin(&self, code: &str) -> ApiError {
        self.auth.clear();
        let refused = ApiError::local(
            0,
            code,
            "authenticated instance discovery does not match the pinned identity",
        );
        (self.on_refusal)(&refused.code);
        if let Some(hook) = self.pin_refused.get() {
            hook();
        }
        refused
    }

    pub fn socket(&self) -> &Arc<SocketRequests> {
        &self.socket
    }

    /// Unix seconds by the instance's clock, as its HTTP answers' `Date` headers said (Task 20: `sent_at`).
    pub fn server_now_s(&self) -> u64 {
        self.http.server_now_s()
    }

    // ---- the transports ----

    /// One socket route: `R`'s table row names its path, method, accepted statuses and body bound.
    pub(crate) async fn socket_call<R: Route>(
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
            match idem.or_else(super::requests::random_idem) {
                Some(idem) => Some(idem),
                None => {
                    return Err(ApiError::local(
                        0,
                        "E_RANDOM",
                        "no OS randomness for an idempotency key",
                    ));
                }
            }
        };
        // One zeroizing path end to end (a body may carry a step-up token): the caller's buffer moves in, and every retry's
        // copy is a zeroizing one too.
        let body = Zeroizing::new(body.unwrap_or_default());
        for attempt in 1..=RETRY_MAX_ATTEMPTS {
            let answer = self
                .socket
                .request_route(
                    method,
                    // The pattern without its query group: the diagnostics record only a path of literals and `{name}`s.
                    Some(format!(
                        "{method} {}",
                        entry.path.split('?').next().unwrap_or("")
                    )),
                    route.clone(),
                    body.clone(),
                    idem.clone(),
                )
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
    pub(crate) async fn http_call<R: Route>(
        &self,
        args: &[&str],
        req: R::Request,
        idempotent: bool,
    ) -> Result<R::Response, ApiError>
    where
        R::Request: Clone,
    {
        self.http_call_bounded::<R>(args, req, idempotent, None)
            .await
    }

    /// [`Self::http_call`] with an optional per-call success body bound (client.ts `maxBodyBytes`).
    async fn http_call_bounded<R: Route>(
        &self,
        args: &[&str],
        req: R::Request,
        idempotent: bool,
        cap: Option<u64>,
    ) -> Result<R::Response, ApiError>
    where
        R::Request: Clone,
    {
        let auth = r::lookup(&R::METHOD, R::PATH).is_some_and(|e| e.auth);
        let mut reauthenticated = false;
        for attempt in 1..=RETRY_MAX_ATTEMPTS {
            let answer = match cap {
                Some(cap) => self.http.call_capped::<R>(args, req.clone(), cap).await,
                None => self.http.call::<R>(args, req.clone()).await,
            };
            let error = match answer {
                Ok(v) => return Ok(v),
                Err(e) => ApiError::of_http(&e),
            };
            if error.status == 401 && auth && !reauthenticated {
                reauthenticated = true;
                if self.establish().await.is_ok() {
                    // Review HL m2: held to the pin before anything more goes out over it, as the gateway's connect is.
                    if let Err(refused) = self.pin_checked().await {
                        if refused.status != 0 {
                            (self.on_refusal)(&refused.code);
                        }
                        return Err(refused);
                    }
                    if attempt < RETRY_MAX_ATTEMPTS {
                        continue;
                    }
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

    // ---- the Wave 1 areas' routes (Task 12: blobs and GIFs) ----

    /// One HTTP row of the route table for an area feature, with [`Self::http_call`]'s retries and re-authentication.
    pub(crate) async fn http_route<R: Route>(
        &self,
        args: &[&str],
        req: R::Request,
        idempotent: bool,
    ) -> Result<R::Response, ApiError>
    where
        R::Request: Clone,
    {
        self.http_call::<R>(args, req, idempotent).await
    }

    /// [`Self::http_route`] with a success body bound of the caller's (routes.ts `getBlob(.., size + 16)`, `gifMedia`):
    /// a longer body is `E_BODY_TOO_LARGE`, refused before the rest is read.
    pub(crate) async fn http_route_capped<R: Route>(
        &self,
        args: &[&str],
        req: R::Request,
        idempotent: bool,
        cap: u64,
    ) -> Result<R::Response, ApiError>
    where
        R::Request: Clone,
    {
        self.http_call_bounded::<R>(args, req, idempotent, Some(cap))
            .await
    }

    /// One gateway row of the route table for an area feature: the accepted answer's status and body.
    pub(crate) async fn socket_route<R: Route>(
        &self,
        args: &[&str],
        body: Option<Vec<u8>>,
    ) -> Result<(u16, Vec<u8>), ApiError> {
        let answer = self.socket_call::<R>(args, body, None, true).await?;
        Ok((answer.status, answer.body))
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
            Err(HttpError::Status { status: 401, .. }) => Ok(None),
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
        decode_instance(&raw)
    }

    /// controller.ts `reauthenticate` → `checkInstanceSignerPin` (review HL m2): after a session was established again
    /// over HTTP, the instance's signer key is read over it (once, straight: no retry and no second re-establish) and held
    /// to the pin ([`pin_after_establish`]). A refusal closes ([`Api::refuse_pin`]): the new session is dropped, and the
    /// client stops its gateway and fails the account.
    async fn pin_checked(&self) -> Result<(), ApiError> {
        let read = match self.http.call::<r::GetInstance>(&[], NoBody).await {
            Ok(Raw(raw)) => decode_instance(&raw),
            Err(e) => Err(ApiError::of_http(&e)),
        };
        let pinned = match &read {
            Ok(instance) => {
                let (id, key) = (instance.instance_id, instance.external_sender_pub);
                Some(
                    with_core(&self.core, move |c| c.instance_signer_key_pin(&id, &key))
                        .await
                        .map_err(|e| e.code.to_string()),
                )
            }
            Err(_) => None,
        };
        match pin_after_establish(read.as_ref().map(|_| ()), pinned) {
            PinAfter::Go => Ok(()),
            // The read's own refusal as it came (its status kept), else the core's code.
            PinAfter::Fail(code) => Err(read
                .err()
                .unwrap_or_else(|| ApiError::local(0, &code, "the instance signer pin"))),
            PinAfter::Refuse(code) => Err(self.refuse_pin(&code)),
        }
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

    // ---- account and devices (Task 16: Settings -> Profile and Devices) ----

    /// routes.ts `getAccountMe`: the raw `[user_id, username, display, kind, flags, presence, status, bio]`.
    pub async fn get_account_me(&self) -> Result<Vec<u8>, ApiError> {
        let Raw(raw) = self.http_call::<r::GetAccountMe>(&[], NoBody, true).await?;
        Ok(raw)
    }

    /// routes.ts `patchMe`: `[display|null, status_msg|null, bio|null]` -> 204.
    pub async fn patch_me(&self, body: Vec<u8>) -> Result<(), ApiError> {
        self.socket_call::<r::PatchMe>(&[], Some(body), None, true)
            .await?;
        Ok(())
    }

    /// routes.ts `listDevices`: the account's devices, raw.
    pub async fn list_devices(&self) -> Result<Vec<u8>, ApiError> {
        Ok(self
            .socket_call::<r::ListDevices>(&[], None, None, true)
            .await?
            .body)
    }

    /// routes.ts `patchDevice`: `[label|null]` -> 204.
    pub async fn patch_device(&self, device: &[u8; 16], body: Vec<u8>) -> Result<(), ApiError> {
        self.socket_call::<r::PatchDevice>(&[&hex(device)], Some(body), None, true)
            .await?;
        Ok(())
    }

    /// routes.ts `deleteDevice`: removes a device no signed list names (no recovery key).
    pub async fn delete_device(&self, device: &[u8; 16]) -> Result<(), ApiError> {
        self.socket_call::<r::DeleteDevice>(&[&hex(device)], None, None, true)
            .await?;
        Ok(())
    }

    /// routes.ts `getBackup`: `[object, created]`'s object, `None` on 404.
    pub async fn get_backup(&self, kind: u8) -> Result<Option<Vec<u8>>, ApiError> {
        let reply = self
            .http_call::<r::GetBackup>(&[&kind.to_string()], NoBody, true)
            .await?;
        if reply.status == 404 {
            return Ok(None);
        }
        let v = value::decode(&reply.body).map_err(ApiError::decode)?;
        let a = at_least(&v, 2).map_err(ApiError::decode)?;
        Ok(Some(value::bytes(&a[0]).map_err(ApiError::decode)?))
    }

    /// routes.ts `putBackup`: `[object]`.
    pub async fn put_backup(&self, kind: u8, object: &[u8]) -> Result<(), ApiError> {
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(1).bytes(object);
        self.http_call::<r::PutBackup>(&[&kind.to_string()], Raw(e.into_vec()), true)
            .await?;
        Ok(())
    }

    /// routes.ts `putDeviceList`: a 409 reaches the caller as [`ApiError::status`] 409 (a list race).
    pub async fn put_device_list(&self, user: &[u8; 16], body: Vec<u8>) -> Result<(), ApiError> {
        self.http_call::<r::PutDeviceList>(&[&hex(user)], Raw(body), false)
            .await
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

/// What a session established again comes to once held to the instance signer pin ([`pin_after_establish`]).
#[derive(Debug, PartialEq, Eq)]
pub(crate) enum PinAfter {
    /// The pin holds: the call goes on.
    Go,
    /// The check could not run (the read failed, a core error other than a pin refusal): this call fails with the code.
    Fail(String),
    /// The pin is refused (controller.ts `failInstanceSignerPin`, `E_CORE_STATE`): closed.
    Refuse(String),
}

/// controller.ts `checkInstanceSignerPin`'s verdict from the instance read (`Err`: its refusal) and, once read, the
/// core's pin check (`Err`: its code): a document that does not decode, a changed key or another instance (the core's
/// `E_CORE_STATE` / `E_CORE_INPUT`) refuse; anything else that failed rethrows.
pub(crate) fn pin_after_establish(
    read: Result<(), &ApiError>,
    pinned: Option<Result<(), String>>,
) -> PinAfter {
    match (read, pinned) {
        (Err(e), _) if e.code == "E_DECODE" => PinAfter::Refuse("E_CORE_STATE".into()),
        (Err(e), _) => PinAfter::Fail(e.code.clone()),
        (Ok(()), Some(Ok(()))) => PinAfter::Go,
        (Ok(()), Some(Err(code))) if code == "E_CORE_STATE" || code == "E_CORE_INPUT" => {
            PinAfter::Refuse("E_CORE_STATE".into())
        }
        (Ok(()), Some(Err(code))) => PinAfter::Fail(code),
        (Ok(()), None) => PinAfter::Refuse("E_CORE_STATE".into()),
    }
}

/// routes.ts `getInstance`'s document: the instance id, the policy version and the external sender's key.
fn decode_instance(raw: &[u8]) -> Result<InstanceInfo, ApiError> {
    let v = value::decode(raw).map_err(ApiError::decode)?;
    let a = at_least(&v, 11).map_err(ApiError::decode)?;
    Ok(InstanceInfo {
        instance_id: bin16(&a[3]).map_err(ApiError::decode)?,
        policy_version: uint(&a[8]).map_err(ApiError::decode)?,
        external_sender_pub: bin32(&a[10]).map_err(ApiError::decode)?,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Review HL m2 (controller.ts `reauthenticate` → `checkInstanceSignerPin`): a session established again over HTTP
    /// is held to the instance signer pin before the call goes on. A pin the core refuses (a changed key, another
    /// instance) or a document that does not decode refuses closed; a read that failed, or another core error, fails the
    /// call (the web rethrows) without closing; a pin that holds lets it go on.
    #[test]
    fn a_reestablished_session_is_held_to_the_pin() {
        let network = ApiError::local(0, "E_NETWORK", "down");
        let garbled = ApiError::local(0, "E_DECODE", "a list");
        let held = |code: &str| Some(Err(code.to_owned()));
        assert_eq!(pin_after_establish(Ok(()), Some(Ok(()))), PinAfter::Go);
        assert_eq!(
            pin_after_establish(Ok(()), held("E_CORE_STATE")),
            PinAfter::Refuse("E_CORE_STATE".into())
        );
        assert_eq!(
            pin_after_establish(Ok(()), held("E_CORE_INPUT")),
            PinAfter::Refuse("E_CORE_STATE".into())
        );
        assert_eq!(
            pin_after_establish(Err(&garbled), None),
            PinAfter::Refuse("E_CORE_STATE".into())
        );
        assert_eq!(
            pin_after_establish(Err(&network), None),
            PinAfter::Fail("E_NETWORK".into())
        );
        assert_eq!(
            pin_after_establish(Ok(()), held("E_STORE")),
            PinAfter::Fail("E_STORE".into())
        );
    }

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
