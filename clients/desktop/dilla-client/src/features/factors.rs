//! Area **factors** (Task 26): TOTP with QR, backup codes, passkeys, password change and step-up.
//!
//! Ports: packages/client-core/src/account/{factors,session}.ts and the factor routes of `http/routes.ts`.
//!
//! Every client gate is advisory: the instance decides each request and a refusal reaches the caller with its status and
//! code ([`FactorError`]). A command that changes what the account holds publishes the [`FactorState`] slice before it
//! answers (factors.ts contract 5); a refused step-up, a missing row or a conflict reads the factors again and answers
//! the refusal unchanged, so the screen shows what the instance holds. The TOTP secret, the otpauth URI, the backup codes
//! and the step-up tokens are answered to the caller in [`Zeroizing`] buffers and never put in a slice, a log or a store.
//! Passwords and codes go out in bodies built in one zeroizing buffer; a socket route's frame takes its own copy of the
//! body (`sync/requests.rs`), which this file cannot wipe.

use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};

use dilla_core::cbor::{CborError, Decoder, Encoder, decode_strict};
use tokio::sync::oneshot;
use zeroize::Zeroizing;

use crate::client::{Client, ClientError, Inner};
use crate::http::routes::{self as r, NoBody, Raw};
use crate::state::AccountPhase;
use crate::state::factors::{FactorState, Factors as FactorsView, PasskeyRow};
use crate::sync::api::{Api, ApiError};

/// The instance's bounds (internal/api auth.go, routes.ts): backup codes in one answer, passkeys on one account, a
/// passkey name in bytes and in characters.
pub const BACKUP_CODES_MAX: usize = 16;
pub const PASSKEYS_MAX: usize = 1024;
pub const PASSKEY_NAME_MAX_BYTES: usize = 64;
pub const PASSKEY_NAME_MAX_CHARS: usize = 64;
/// A WebAuthn response the instance takes, in bytes.
pub const RESPONSE_MAX_BYTES: usize = 65_536;

/// A refusal, by status and code only (never by the instance's detail text, protocol/02). Status 0 is the client's own:
/// `E_NETWORK` (no answer), `E_BAD_INPUT` (a synchronous check, before anything is sent), `E_DECODE`, `E_STOPPED`.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
#[error("{code} ({status})")]
pub struct FactorError {
    pub status: u16,
    pub code: String,
    /// The instance's wait, when it named one (a 429 over the socket).
    pub retry_after_ms: Option<u64>,
}

impl FactorError {
    pub fn new(status: u16, code: &str) -> Self {
        Self {
            status,
            code: code.to_owned(),
            retry_after_ms: None,
        }
    }

    /// factors.ts `FACTOR_VALIDATORS`: an input the instance would refuse, refused before it is sent.
    pub fn bad_input() -> Self {
        Self::new(0, "E_BAD_INPUT")
    }

    pub fn is(&self, status: u16, code: &str) -> bool {
        self.status == status && self.code == code
    }

    /// A step-up token was refused (spent, expired, from another session): 403 `E_STEP_UP`.
    pub fn is_step_up(&self) -> bool {
        self.is(403, "E_STEP_UP")
    }

    fn of_api(e: &ApiError) -> Self {
        Self {
            status: e.status,
            code: e.code.clone(),
            retry_after_ms: e.retry_after_ms,
        }
    }

    fn decode() -> Self {
        Self::new(0, "E_DECODE")
    }
}

impl From<ClientError> for FactorError {
    fn from(e: ClientError) -> Self {
        match e {
            ClientError::Network(_) => Self::new(0, "E_NETWORK"),
            ClientError::Stopped => Self::new(0, "E_STOPPED"),
            ClientError::FactorRequired => Self::new(403, "E_FACTOR_REQUIRED"),
            ClientError::Sync(code) => Self::new(0, &code),
            other => Self::new(
                0,
                &format!("{other:?}").chars().take(32).collect::<String>(),
            ),
        }
    }
}

impl From<CborError> for FactorError {
    fn from(_: CborError) -> Self {
        Self::decode()
    }
}

/// A new authenticator app's key: the base32 secret and the `otpauth://` URI the QR code shows. Never printed.
pub struct TotpKey {
    pub secret: Zeroizing<String>,
    pub uri: Zeroizing<String>,
}

impl std::fmt::Debug for TotpKey {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("TotpKey(<redacted>)")
    }
}

/// Backup codes as the instance issued them (at most [`BACKUP_CODES_MAX`]). Empty when the account already held unused
/// codes. Never printed.
#[derive(Default)]
pub struct BackupCodes(pub Zeroizing<Vec<String>>);

impl std::fmt::Debug for BackupCodes {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "BackupCodes({} codes)", self.0.len())
    }
}

/// A WebAuthn ceremony the instance began: its id and its options document (`{publicKey:{…}}`, not secret).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Ceremony {
    pub id: [u8; 16],
    pub options: String,
}

/// A step-up token: proof of a fresh second factor, spent by the next sensitive request. Never printed.
#[derive(Clone)]
pub struct StepUpToken(pub Zeroizing<String>);

impl std::fmt::Debug for StepUpToken {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("StepUpToken(<redacted>)")
    }
}

/// How a step-up is proved (`POST /v1/auth/stepup` method 0, 1, 2).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum StepUpMethod {
    Password,
    Totp,
    Backup,
}

/// Which factor to remove.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum FactorKind {
    Totp,
    /// A passkey, by its credential id in lower-case hex.
    Passkey(String),
}

/// What the instance's document says about sign-in (`GET /v1/instance`, element 5 the name, element 7 the auth
/// methods: 0 password sign-up, 1 TOTP, 2 passkeys, 3 OIDC).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct InstanceDoc {
    pub name: String,
    pub passkeys: bool,
    pub oidc: bool,
}

impl InstanceDoc {
    /// Decodes the instance document.
    pub fn decode(doc: &[u8]) -> Result<Self, FactorError> {
        Ok(decode_strict(doc, |d| {
            let len = d.array_len()?;
            if len < 11 {
                return Err(CborError::Truncated);
            }
            for _ in 0..5 {
                d.skip()?;
            }
            let name = d.text()?.to_owned();
            d.skip()?;
            let n = d.array_len()?;
            let mut methods = Vec::with_capacity(n.min(16));
            for _ in 0..n {
                methods.push(d.uint()?);
            }
            for _ in 8..len {
                d.skip()?;
            }
            Ok(InstanceDoc {
                name,
                passkeys: methods.contains(&2),
                oidc: methods.contains(&3),
            })
        })?)
    }
}

// ---- the synchronous checks (factors.ts FACTOR_VALIDATORS) ----

/// A backup code as typed: dashes and white space dropped, upper-cased (factors.ts `normaliseBackupCode`).
pub fn normalise_backup_code(typed: &str) -> Zeroizing<String> {
    Zeroizing::new(
        typed
            .chars()
            .filter(|c| !c.is_whitespace() && *c != '-')
            .map(|c| c.to_ascii_uppercase())
            .collect(),
    )
}

/// factors.ts `BACKUP_CODE`: 26 characters of `A-Z2-7`.
pub fn is_backup_code(normalised: &str) -> bool {
    normalised.len() == 26
        && normalised
            .bytes()
            .all(|b| b.is_ascii_uppercase() || (b'2'..=b'7').contains(&b))
}

/// Six ASCII digits.
pub fn is_six_digits(code: &str) -> bool {
    code.len() == 6 && code.bytes().all(|b| b.is_ascii_digit())
}

/// factors.ts `nameProblem`: 1 to 64 characters, at most 64 UTF-8 bytes, no control character.
pub fn passkey_name_fits(name: &str) -> bool {
    let chars = name.chars().count();
    (1..=PASSKEY_NAME_MAX_CHARS).contains(&chars)
        && name.len() <= PASSKEY_NAME_MAX_BYTES
        && !name.chars().any(char::is_control)
}

/// factors.ts `responseProblem`: 1 to 65 536 bytes.
fn response_fits(response: &str) -> bool {
    !response.is_empty() && response.len() <= RESPONSE_MAX_BYTES
}

/// factors.ts `CRED_ID`: 1 to 1023 bytes in lower-case hex.
fn cred_id(hex: &str) -> Option<Vec<u8>> {
    if hex.is_empty() || !hex.len().is_multiple_of(2) || hex.len() > 2046 {
        return None;
    }
    let digit = |c: u8| match c {
        b'0'..=b'9' => Some(c - b'0'),
        b'a'..=b'f' => Some(c - b'a' + 10),
        _ => None,
    };
    hex.as_bytes()
        .chunks(2)
        .map(|p| Some(digit(p[0])? << 4 | digit(p[1])?))
        .collect()
}

// ---- the answers ----

/// `GET /v1/auth/factors`: `[owed, totp|null, [[cred_id, name, created, last_used|null]], backup_codes_left, password,
/// password_changed|null?]` (factors.ts `factorsState`).
pub fn decode_factors(raw: &[u8]) -> Result<FactorsView, FactorError> {
    Ok(decode_strict(raw, |d| {
        let len = d.array_len()?;
        if len < 5 {
            return Err(CborError::Truncated);
        }
        let owed = flag(d.uint()?)?;
        let totp = d.opt_uint()?;
        let n = d.array_len()?;
        if n > PASSKEYS_MAX {
            return Err(CborError::IntegerOverflow);
        }
        let mut passkeys = Vec::with_capacity(n);
        for _ in 0..n {
            let fields = d.array_len()?;
            if fields < 4 {
                return Err(CborError::Truncated);
            }
            let id = crate::state::hex(d.bytes()?);
            let name = d.text()?.to_owned();
            let created = d.uint()?;
            let last_used = d.opt_uint()?;
            for _ in 4..fields {
                d.skip()?;
            }
            passkeys.push(PasskeyRow {
                id,
                name,
                created,
                last_used,
            });
        }
        let backup_codes_left = u32::try_from(d.uint()?).map_err(|_| CborError::IntegerOverflow)?;
        let has_password = flag(d.uint()?)?;
        let password_changed = if len > 5 { d.opt_uint()? } else { None };
        for _ in 6..len {
            d.skip()?;
        }
        Ok(FactorsView {
            owed,
            totp,
            passkeys,
            backup_codes_left,
            has_password,
            password_changed,
        })
    })?)
}

fn flag(v: u64) -> Result<bool, CborError> {
    match v {
        0 => Ok(false),
        1 => Ok(true),
        _ => Err(CborError::IntegerOverflow),
    }
}

/// Reads a list of backup codes into a zeroizing vector (routes.ts `backupCodes`).
fn codes(d: &mut Decoder<'_>) -> Result<BackupCodes, CborError> {
    let n = d.array_len()?;
    if n > BACKUP_CODES_MAX {
        return Err(CborError::IntegerOverflow);
    }
    let mut out = Zeroizing::new(Vec::with_capacity(n));
    for _ in 0..n {
        out.push(d.text()?.to_owned());
    }
    Ok(BackupCodes(out))
}

/// `[stepup, expires]`.
fn decode_step_up(raw: &[u8]) -> Result<StepUpToken, FactorError> {
    Ok(decode_strict(raw, |d| {
        let len = d.array_len()?;
        if len < 2 {
            return Err(CborError::Truncated);
        }
        let token = Zeroizing::new(d.text()?.to_owned());
        if token.is_empty() {
            return Err(CborError::Truncated);
        }
        d.uint()?;
        for _ in 2..len {
            d.skip()?;
        }
        Ok(StepUpToken(token))
    })?)
}

/// `[ceremony_id, options]`.
pub fn decode_ceremony(raw: &[u8]) -> Result<Ceremony, FactorError> {
    Ok(decode_strict(raw, |d| {
        let len = d.array_len()?;
        if len < 2 {
            return Err(CborError::Truncated);
        }
        let id = d.bytes_exact::<16>()?;
        let options = d.text()?.to_owned();
        for _ in 2..len {
            d.skip()?;
        }
        Ok(Ceremony { id, options })
    })?)
}

/// `[codes]` of TOTP confirm and backup-code regeneration.
fn decode_codes_only(raw: &[u8]) -> Result<BackupCodes, FactorError> {
    Ok(decode_strict(raw, |d| {
        let len = d.array_len()?;
        if len < 1 {
            return Err(CborError::Truncated);
        }
        let c = codes(d)?;
        for _ in 1..len {
            d.skip()?;
        }
        Ok(c)
    })?)
}

/// A body built in one buffer that is wiped when dropped.
fn body(f: impl FnOnce(&mut Encoder)) -> Zeroizing<Vec<u8>> {
    let mut e = Encoder::with_capacity(256);
    f(&mut e);
    Zeroizing::new(e.into_vec())
}

fn opt_text(e: &mut Encoder, v: Option<&str>) {
    match v {
        Some(t) => {
            e.text(t);
        }
        None => {
            e.null();
        }
    }
}

// ---- the handle ----

/// What a [`Factors`] command answers: a future of its result (the client's runtime runs the request).
pub struct FactorReply<T>(oneshot::Receiver<Result<Result<T, FactorError>, ClientError>>);

impl<T> Future for FactorReply<T> {
    type Output = Result<T, FactorError>;

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        Pin::new(&mut self.0).poll(cx).map(|r| match r {
            Ok(Ok(answer)) => answer,
            Ok(Err(e)) => Err(e.into()),
            Err(_) => Err(FactorError::new(0, "E_STOPPED")),
        })
    }
}

/// The factors area's handle: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Factors {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Factors`] handle.
    pub fn factors(&self) -> Factors {
        Factors {
            inner: self.inner().clone(),
        }
    }
}

/// Whether the account is in a phase that holds the factors slice (factors.ts `holdsFactors`).
fn holds_factors(inner: &Inner) -> bool {
    matches!(
        inner.snapshot().account.phase,
        AccountPhase::Ready | AccountPhase::FactorSetup
    )
}

/// factors.ts `refresh`: reads the factors and publishes them.
async fn refresh(inner: &Inner) -> Result<FactorsView, FactorError> {
    let Raw(raw) = inner
        .api()
        .http_call::<r::GetFactors>(&[], NoBody, true)
        .await
        .map_err(|e| FactorError::of_api(&e))?;
    let factors = decode_factors(&raw)?;
    // A late answer after the account left is not this account's to publish.
    if holds_factors(inner) {
        let slice = factors.clone();
        inner.publish(move |s| {
            s.factors = FactorState {
                factors: Some(slice),
            };
        });
    }
    Ok(factors)
}

/// The re-read after a commit: the server has already acted, so a failed read must not hide the result.
async fn reread(inner: &Inner) {
    let _ = refresh(inner).await;
}

/// factors.ts `guarded`: a refused step-up (403 `E_STEP_UP`), a missing row (404) or a conflict (409) means the
/// screen's picture of the account is stale: the factors are read again, best effort, and the refusal answered as is.
async fn guarded<T>(
    inner: &Inner,
    call: impl Future<Output = Result<T, FactorError>>,
) -> Result<T, FactorError> {
    match call.await {
        Ok(v) => Ok(v),
        Err(e) => {
            if e.status == 404 || e.status == 409 || e.is_step_up() {
                reread(inner).await;
            }
            Err(e)
        }
    }
}

/// One socket route: its answer's body, or the refusal.
async fn socket<R: crate::seams::Route>(
    api: &Api,
    request: Zeroizing<Vec<u8>>,
    retry429: bool,
) -> Result<Vec<u8>, FactorError> {
    // The frame takes its own copy of the body (sync/requests.rs); the buffer built here is wiped as it drops.
    let answer = api
        .socket_call::<R>(&[], Some(request.to_vec()), None, retry429)
        .await
        .map_err(|e| FactorError::of_api(&e))?;
    Ok(answer.body)
}

async fn http<R>(api: &Api, request: R::Request) -> Result<R::Response, FactorError>
where
    R: crate::seams::Route,
    R::Request: Clone,
{
    api.http_call::<R>(&[], request, false)
        .await
        .map_err(|e| FactorError::of_api(&e))
}

impl Factors {
    fn run<T: Send + 'static>(
        &self,
        f: impl FnOnce(Arc<Inner>) -> Pin<Box<dyn Future<Output = Result<T, FactorError>> + Send>>,
    ) -> FactorReply<T> {
        let inner = self.inner.clone();
        let job = f(inner);
        FactorReply(self.inner.spawn(async move { Ok(job.await) }))
    }

    fn refused<T: Send + 'static>(&self, e: FactorError) -> FactorReply<T> {
        let (tx, rx) = oneshot::channel();
        let _ = tx.send(Ok(Err(e)));
        FactorReply(rx)
    }

    /// Reads the factors and publishes them (factors.ts `refresh`). Answers nothing when the account is in no phase that
    /// holds them.
    pub fn refresh(&self) -> FactorReply<Option<FactorsView>> {
        self.run(|inner| {
            Box::pin(async move {
                if !holds_factors(&inner) {
                    return Ok(None);
                }
                refresh(&inner).await.map(Some)
            })
        })
    }

    /// The instance's sign-in document (its name and whether it offers passkeys and OIDC).
    pub fn instance(&self) -> FactorReply<InstanceDoc> {
        self.run(|inner| {
            Box::pin(async move {
                let Raw(doc) = inner
                    .api()
                    .http_call::<r::GetInstance>(&[], NoBody, true)
                    .await
                    .map_err(|e| FactorError::of_api(&e))?;
                InstanceDoc::decode(&doc)
            })
        })
    }

    /// controller.ts `factorSetupDone`: the factors are read again. `Ok` when the account owes nothing (the caller then
    /// calls [`Client::factor_completed`]); 403 `E_FACTOR_REQUIRED` when it still owes a factor.
    pub fn setup_done(&self) -> FactorReply<()> {
        self.run(|inner| {
            Box::pin(async move {
                let factors = refresh(&inner).await?;
                if factors.owed {
                    return Err(FactorError::new(403, "E_FACTOR_REQUIRED"));
                }
                Ok(())
            })
        })
    }

    /// `POST /v1/auth/totp/enroll` `[stepup|null]` → `[secret, otpauth_url]`.
    pub fn totp_begin(&self, stepup: Option<StepUpToken>) -> FactorReply<TotpKey> {
        if stepup.as_ref().is_some_and(|t| t.0.is_empty()) {
            return self.refused(FactorError::bad_input());
        }
        self.run(move |inner| {
            Box::pin(async move {
                let req = body(|e| {
                    e.array(1);
                    opt_text(e, stepup.as_ref().map(|t| t.0.as_str()));
                });
                let call = async {
                    let Raw(raw) = http::<r::TotpEnroll>(inner.api(), r::Secret(req)).await?;
                    let raw = Zeroizing::new(raw);
                    Ok(decode_strict(&raw, |d| {
                        let len = d.array_len()?;
                        if len < 2 {
                            return Err(CborError::Truncated);
                        }
                        let secret = Zeroizing::new(d.text()?.to_owned());
                        let uri = Zeroizing::new(d.text()?.to_owned());
                        for _ in 2..len {
                            d.skip()?;
                        }
                        Ok(TotpKey { secret, uri })
                    })?)
                };
                guarded(&inner, call).await
            })
        })
    }

    /// `POST /v1/auth/totp/confirm` `[code]` → `[backup_codes]`; the list is empty when the account already held unused
    /// codes. The factors are read again.
    pub fn totp_confirm(&self, code: &str) -> FactorReply<BackupCodes> {
        if !is_six_digits(code) {
            return self.refused(FactorError::bad_input());
        }
        let req = body(|e| {
            e.array(1).text(code);
        });
        self.run(move |inner| {
            Box::pin(async move {
                let call = async {
                    let Raw(raw) = http::<r::TotpConfirm>(inner.api(), r::Secret(req)).await?;
                    decode_codes_only(&Zeroizing::new(raw))
                };
                let codes = guarded(&inner, call).await?;
                reread(&inner).await;
                Ok(codes)
            })
        })
    }

    /// `POST /v1/auth/passkey/register/begin` `[stepup|null]` → the ceremony.
    pub fn passkey_begin(&self, stepup: Option<StepUpToken>) -> FactorReply<Ceremony> {
        if stepup.as_ref().is_some_and(|t| t.0.is_empty()) {
            return self.refused(FactorError::bad_input());
        }
        self.run(move |inner| {
            Box::pin(async move {
                let req = body(|e| {
                    e.array(1);
                    opt_text(e, stepup.as_ref().map(|t| t.0.as_str()));
                });
                let call = async {
                    let Raw(raw) =
                        http::<r::PasskeyRegisterBegin>(inner.api(), r::Secret(req)).await?;
                    decode_ceremony(&raw)
                };
                guarded(&inner, call).await
            })
        })
    }

    /// `POST /v1/auth/passkey/register/finish` `[ceremony_id, response, name]` → `[cred_id, backup_codes]`.
    pub fn passkey_finish(
        &self,
        ceremony: [u8; 16],
        response: Zeroizing<String>,
        name: &str,
    ) -> FactorReply<BackupCodes> {
        let name = name.trim().to_owned();
        if !response_fits(&response) || !passkey_name_fits(&name) {
            return self.refused(FactorError::bad_input());
        }
        self.run(move |inner| {
            Box::pin(async move {
                let req = body(|e| {
                    e.array(3).bytes(&ceremony).text(&response).text(&name);
                });
                let call = async {
                    let Raw(raw) =
                        http::<r::PasskeyRegisterFinish>(inner.api(), Raw(req.to_vec())).await?;
                    let raw = Zeroizing::new(raw);
                    Ok(decode_strict(&raw, |d| {
                        let len = d.array_len()?;
                        if len < 2 {
                            return Err(CborError::Truncated);
                        }
                        d.bytes()?;
                        let c = codes(d)?;
                        for _ in 2..len {
                            d.skip()?;
                        }
                        Ok(c)
                    })?)
                };
                let codes = guarded(&inner, call).await?;
                reread(&inner).await;
                Ok(codes)
            })
        })
    }

    /// `POST /v1/auth/stepup` `[method, secret]` → the token. A wrong secret (403 `E_STEP_UP`) is the person's to correct:
    /// no re-read. A 429 is not waited out.
    pub fn step_up(
        &self,
        method: StepUpMethod,
        secret: Zeroizing<String>,
    ) -> FactorReply<StepUpToken> {
        let secret = match method {
            StepUpMethod::Backup => normalise_backup_code(&secret),
            _ => secret,
        };
        let ok = !secret.is_empty()
            && match method {
                StepUpMethod::Password => true,
                StepUpMethod::Totp => is_six_digits(&secret),
                StepUpMethod::Backup => is_backup_code(&secret),
            };
        if !ok {
            return self.refused(FactorError::bad_input());
        }
        let kind = match method {
            StepUpMethod::Password => 0,
            StepUpMethod::Totp => 1,
            StepUpMethod::Backup => 2,
        };
        let req = body(|e| {
            e.array(2).uint(kind).text(&secret);
        });
        drop(secret);
        self.run(move |inner| {
            Box::pin(async move {
                let raw = Zeroizing::new(socket::<r::StepUp>(inner.api(), req, false).await?);
                decode_step_up(&raw)
            })
        })
    }

    /// `POST /v1/auth/stepup/passkey/begin` `[]` → a ceremony over this account's passkeys.
    pub fn step_up_passkey_begin(&self) -> FactorReply<Ceremony> {
        self.run(|inner| {
            Box::pin(async move {
                let req = body(|e| {
                    e.array(0);
                });
                let raw = socket::<r::StepUpPasskeyBegin>(inner.api(), req, true).await?;
                decode_ceremony(&raw)
            })
        })
    }

    /// `POST /v1/auth/stepup/passkey/finish` `[ceremony_id, response]` → the token.
    pub fn step_up_passkey_finish(
        &self,
        ceremony: [u8; 16],
        response: Zeroizing<String>,
    ) -> FactorReply<StepUpToken> {
        if !response_fits(&response) {
            return self.refused(FactorError::bad_input());
        }
        self.run(move |inner| {
            Box::pin(async move {
                let req = body(|e| {
                    e.array(2).bytes(&ceremony).text(&response);
                });
                let raw = Zeroizing::new(
                    socket::<r::StepUpPasskeyFinish>(inner.api(), req, false).await?,
                );
                decode_step_up(&raw)
            })
        })
    }

    /// `POST /v1/auth/factors/remove` `[stepup, kind, cred_id|null]` → 204. The last factor is 409. The factors are read
    /// again.
    pub fn remove(&self, stepup: StepUpToken, kind: FactorKind) -> FactorReply<()> {
        let cred = match &kind {
            FactorKind::Totp => None,
            FactorKind::Passkey(hex) => match cred_id(hex) {
                Some(id) => Some(id),
                None => return self.refused(FactorError::bad_input()),
            },
        };
        if stepup.0.is_empty() {
            return self.refused(FactorError::bad_input());
        }
        self.run(move |inner| {
            Box::pin(async move {
                let req = body(|e| {
                    e.array(3).text(&stepup.0);
                    match &cred {
                        None => {
                            e.uint(0).null();
                        }
                        Some(id) => {
                            e.uint(1).bytes(id);
                        }
                    }
                });
                guarded(&inner, async {
                    socket::<r::RemoveFactor>(inner.api(), req, true).await
                })
                .await?;
                reread(&inner).await;
                Ok(())
            })
        })
    }

    /// `POST /v1/auth/recovery/codes` `[stepup]` → new backup codes; every earlier code stops working.
    pub fn regenerate_backup_codes(&self, stepup: StepUpToken) -> FactorReply<BackupCodes> {
        if stepup.0.is_empty() {
            return self.refused(FactorError::bad_input());
        }
        self.run(move |inner| {
            Box::pin(async move {
                let req = body(|e| {
                    e.array(1).text(&stepup.0);
                });
                let raw = guarded(&inner, async {
                    socket::<r::RegenerateBackupCodes>(inner.api(), req, true).await
                })
                .await?;
                let codes = decode_codes_only(&Zeroizing::new(raw))?;
                reread(&inner).await;
                Ok(codes)
            })
        })
    }

    /// `POST /v1/auth/password` `[old|null, new, stepup|null]` → 204. With a password on the account the old one is the
    /// step-up (a wrong one is 403 `E_FORBIDDEN`); a first password on an account holding a factor takes a step-up
    /// token. A 429 is not waited out (it would send the password again).
    pub fn change_password(
        &self,
        old: Option<Zeroizing<String>>,
        next: Zeroizing<String>,
        stepup: Option<StepUpToken>,
    ) -> FactorReply<()> {
        if next.is_empty()
            || old.as_ref().is_some_and(|o| o.is_empty())
            || stepup.as_ref().is_some_and(|t| t.0.is_empty())
        {
            return self.refused(FactorError::bad_input());
        }
        let req = body(|e| {
            e.array(3);
            opt_text(e, old.as_ref().map(|o| o.as_str()));
            e.text(&next);
            opt_text(e, stepup.as_ref().map(|t| t.0.as_str()));
        });
        drop((old, next, stepup));
        self.run(move |inner| {
            Box::pin(async move {
                guarded(&inner, async {
                    socket::<r::ChangePassword>(inner.api(), req, false).await
                })
                .await?;
                reread(&inner).await;
                Ok(())
            })
        })
    }
}

impl Factors {
    /// controller.ts `forgetBrowser`, the server half: a candidate list this device signed and did not publish is
    /// published (best effort: a failure leaves it to the next recovering device), then every session of this device is
    /// ended (`DELETE /v1/devices/{id}/sessions`; a 401 means they are gone already). The caller takes the gateway down
    /// first ([`Client::disconnect`]) and, once this answers, halts the client and forgets the store
    /// ([`forget_device`]). Any other refusal is answered and nothing is forgotten.
    ///
    /// L-TS-56 step 9 leaves a running call first: the forced-factor screen that offers this has no gateway (the
    /// instance refuses it while a factor is owed), so no call can be running there; another caller (Task 17's voice)
    /// leaves its call before calling this.
    pub fn sign_out_device(&self) -> FactorReply<()> {
        self.run(|inner| {
            Box::pin(async move {
                let core = inner.engine().core().clone();
                let identity = crate::sync::api::try_core(&core, |c| c.identity())
                    .await
                    .map_err(|_| FactorError::new(0, "E_CORE_POISONED"))?
                    .ok()
                    .and_then(|raw| crate::sync::api::Identity::decode(&raw))
                    .ok_or_else(|| FactorError::new(0, "E_CORE_NO_IDENTITY"))?;
                if identity.phase == 2
                    && !identity.list_published
                    && let Some(user) = identity.user_id
                {
                    let body = crate::sync::api::try_core(&core, |c| c.device_list_body())
                        .await
                        .ok()
                        .and_then(Result::ok);
                    if let Some(body) = body {
                        let user = crate::state::hex(&user);
                        let put = inner
                            .api()
                            .http_call::<r::PutDeviceList>(&[&user], Raw(body), false)
                            .await;
                        if put.is_ok() {
                            let _ =
                                crate::sync::api::try_core(&core, |c| c.device_list_published())
                                    .await;
                        }
                    }
                }
                let Some(device) = identity.device_id else {
                    return Ok(());
                };
                let device = crate::state::hex(&device);
                match inner
                    .api()
                    .http_call::<r::DeleteSessions>(&[&device], NoBody, true)
                    .await
                {
                    Ok(()) => Ok(()),
                    Err(e) if e.status == 401 => Ok(()),
                    Err(e) => Err(FactorError::of_api(&e)),
                }
            })
        })
    }
}

/// Why [`close_and_forget`] did not wipe.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum ForgetError {
    /// The store stayed open (a handle of the client outlived it) past the wait: nothing was wiped.
    #[error("the store is still open")]
    StoreOpen,
    /// The wipe itself failed.
    #[error("the store could not be removed: {0}")]
    Wipe(String),
}

/// Forget this device, the local half, after [`Factors::sign_out_device`]: the client halts and stops (its runtime
/// shuts down), then the wipe waits until the store is closed ([`crate::client::StoreProbe`]: every job and handle let go
/// of the core and its SQLite connection), at most `wait`, and only then removes the data dir and its keychain entry.
/// Blocking: call it off the UI thread and outside any runtime. The caller drops every other handle of the client
/// (views, area handles) first, or the store stays open and nothing is wiped ([`ForgetError::StoreOpen`]).
pub fn close_and_forget(
    client: Client,
    root: &std::path::Path,
    cfg: &crate::seams::InstanceConfig,
    kc: &dyn crate::seams::Keychain,
    wait: std::time::Duration,
) -> Result<(), ForgetError> {
    let probe = client.store_probe();
    client.halt();
    client.stop();
    let deadline = std::time::Instant::now() + wait;
    while !probe.is_closed() {
        if std::time::Instant::now() >= deadline {
            return Err(ForgetError::StoreOpen);
        }
        std::thread::sleep(std::time::Duration::from_millis(20));
    }
    forget_device(root, cfg, kc).map_err(|e| ForgetError::Wipe(e.to_string()))
}

/// web ForgetBrowser: this device forgets the instance. Its data dir goes (the store and its keys), then its keychain
/// entry; the account's device list on the instance is not touched, so the device stays listed until it is removed from
/// another device. The caller stops the instance's client first.
pub fn forget_device(
    root: &std::path::Path,
    cfg: &crate::seams::InstanceConfig,
    kc: &dyn crate::seams::Keychain,
) -> Result<(), crate::store::StoreError> {
    let account = crate::store::store_keychain_account(cfg).ok();
    let mut instances = crate::instances::Instances::load(root)?;
    instances.remove(cfg.id)?;
    if let Some(account) = account {
        let _ = kc.delete(&account);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn enc(f: impl FnOnce(&mut Encoder)) -> Vec<u8> {
        let mut e = Encoder::new();
        f(&mut e);
        e.into_vec()
    }

    /// factors.test.ts `factorsState`: ids in hex, the optional sixth element.
    #[test]
    fn factors_decode_like_factors_state() {
        let raw = enc(|e| {
            e.array(6).uint(0).uint(1_700_000_000);
            e.array(1)
                .array(4)
                .bytes(&[0xab, 0x01])
                .text("Laptop")
                .uint(5)
                .null();
            e.uint(9).uint(1).uint(42);
        });
        let f = decode_factors(&raw).unwrap();
        assert!(!f.owed);
        assert_eq!(f.totp, Some(1_700_000_000));
        assert_eq!(f.passkeys[0].id, "ab01");
        assert_eq!(f.passkeys[0].name, "Laptop");
        assert_eq!(f.passkeys[0].last_used, None);
        assert_eq!(f.backup_codes_left, 9);
        assert!(f.has_password);
        assert_eq!(f.password_changed, Some(42));
        assert_eq!(f.count(), 2);
        // An older server sends five elements.
        let raw = enc(|e| {
            e.array(5).uint(1).null().array(0).uint(0).uint(0);
        });
        let f = decode_factors(&raw).unwrap();
        assert!(f.owed && !f.holds_factor() && !f.has_password);
        assert_eq!(f.password_changed, None);
        // A flag that is not 0 or 1 is refused.
        let raw = enc(|e| {
            e.array(5).uint(2).null().array(0).uint(0).uint(0);
        });
        assert!(decode_factors(&raw).is_err());
    }

    /// factors.test.ts FACTOR_VALIDATORS rows.
    #[test]
    fn validators_match_factors_ts() {
        assert!(is_six_digits("123456"));
        assert!(!is_six_digits("12345"));
        assert!(!is_six_digits("12345a"));
        assert!(!is_six_digits("١٢٣٤٥٦"));
        assert_eq!(
            normalise_backup_code("abcd-efgh ijkl-mnop qrst-uvwx yz").as_str(),
            "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
        );
        assert!(is_backup_code("ABCDEFGHIJKLMNOPQRSTUVWX27"));
        assert!(!is_backup_code("ABCDEFGHIJKLMNOPQRSTUVWX18"));
        assert!(!is_backup_code("ABCDEFGHIJKLMNOPQRSTUVWX2"));
        assert!(passkey_name_fits("Laptop"));
        assert!(!passkey_name_fits(""));
        assert!(!passkey_name_fits("a\u{7}b"));
        assert!(passkey_name_fits(&"x".repeat(64)));
        assert!(!passkey_name_fits(&"x".repeat(65)));
        // 22 characters of three bytes each: 66 bytes.
        assert!(!passkey_name_fits(&"€".repeat(22)));
        assert_eq!(cred_id("ab01"), Some(vec![0xab, 0x01]));
        assert_eq!(cred_id("AB01"), None);
        assert_eq!(cred_id("abc"), None);
        assert_eq!(cred_id(""), None);
        assert!(cred_id(&"a".repeat(2048)).is_none());
    }

    #[test]
    fn answers_decode_and_never_print() {
        let raw = enc(|e| {
            e.array(2).text("tok").uint(9);
        });
        let t = decode_step_up(&raw).unwrap();
        assert_eq!(t.0.as_str(), "tok");
        assert_eq!(format!("{t:?}"), "StepUpToken(<redacted>)");
        let raw = enc(|e| {
            e.array(2).bytes(&[7; 16]).text("{}");
        });
        assert_eq!(
            decode_ceremony(&raw).unwrap(),
            Ceremony {
                id: [7; 16],
                options: "{}".into()
            }
        );
        let raw = enc(|e| {
            e.array(1).array(2).text("A").text("B");
        });
        let c = decode_codes_only(&raw).unwrap();
        assert_eq!(c.0.as_slice(), ["A", "B"]);
        assert_eq!(format!("{c:?}"), "BackupCodes(2 codes)");
        let too_many = enc(|e| {
            e.array(1).array(17);
            for _ in 0..17 {
                e.text("A");
            }
        });
        assert!(decode_codes_only(&too_many).is_err());
        let key = TotpKey {
            secret: Zeroizing::new("S".into()),
            uri: Zeroizing::new("otpauth://".into()),
        };
        assert_eq!(format!("{key:?}"), "TotpKey(<redacted>)");
    }

    #[test]
    fn instance_doc_reads_name_and_methods() {
        let raw = enc(|e| {
            e.array(11)
                .uint(1)
                .uint(1)
                .uint(1)
                .bytes(&[0; 16])
                .uint(1)
                .text("Team");
            e.uint(0)
                .array(3)
                .uint(0)
                .uint(1)
                .uint(2)
                .uint(1)
                .null()
                .bytes(&[0; 32]);
        });
        let doc = InstanceDoc::decode(&raw).unwrap();
        assert_eq!(doc.name, "Team");
        assert!(doc.passkeys && !doc.oidc);
    }
}
