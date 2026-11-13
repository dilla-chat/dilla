//! Signing in on a new device, as the web client's sign-in screen drives `packages/client-core/src/account/enrol.ts`:
//! a password login (or a passkey, or the instance's OIDC sign-in), the second factor it owes (the authenticator code, a
//! passkey, or a backup code), then the device's registration and its recovery from the account's sealed backup with the
//! recovery key. The device ends with a ready identity (phase 2) in its store, its device list published and its sealed
//! state uploaded, so [`crate::Client::start`] can take over.
//!
//! web3.8 (R6) order: the login and the factor never register this device; the held, settled assertion waits for the
//! key step, which registers, fetches, completes and publishes back to back. A backup code waits in memory too and goes
//! only with the key ([`SignIn::add_device_with_backup`]), so an assertion that ran out meanwhile spends none.
//!
//! Every secret (the password, the code, the backup code, the recovery key, the assertion, the passkey response and the
//! session token) is held in [`Zeroizing`] buffers or the core and never printed. Narrowed against enrol.ts: the
//! device-list fork repair after a 409 (adoptServedList) is not ported; a 409 on the list is [`SignInError::ListRace`]
//! and the person signs in again.

use std::sync::{Arc, Mutex};

use dilla_core::client::ClientCore;
use zeroize::Zeroizing;

use crate::auth::AuthToken;
use crate::gateway::frames::Value;
use crate::http::ReqwestHttp;
use crate::http::routes::{self as r, NoBody, Raw};
use crate::seams::{Http, HttpError, InstanceConfig, Keychain};
use crate::store::{StoreError, open_store};

/// Why a sign-in step failed, in the web screen's terms (`signin.error.*`).
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum SignInError {
    /// The username and password were refused.
    #[error("that username and password did not work")]
    LoginFailed,
    /// The code was refused; the assertion is spent, so the person signs in again.
    #[error("that code did not work")]
    CodeFailed,
    /// The step was taken out of order (no login, or a login that owes something else).
    #[error("sign in first")]
    NoAssertion,
    /// This login owes a passkey (or a backup code), and none was given.
    #[error("this account signs in with a passkey")]
    PasskeyOnly,
    /// The backup code was refused; the assertion is spent, so the person signs in again.
    #[error("that backup code did not work")]
    BackupFailed,
    /// The instance does not know this passkey (401 `E_PASSKEY_UNKNOWN`).
    #[error("the instance does not know this passkey")]
    PasskeyUnknown,
    /// A passkey of another account than the password login (403 `E_PASSKEY_OTHER_ACCOUNT`).
    #[error("that passkey belongs to another account")]
    PasskeyOtherAccount,
    /// The system browser's ceremony ended without a passkey or an OIDC assertion.
    #[error("the browser sign-in did not finish: {0}")]
    Browser(crate::features::browser_auth::BrowserAuthError),
    /// The account has as many devices as the instance allows (the registration's 403 `E_FORBIDDEN`).
    #[error("the account has as many devices as the instance allows")]
    DeviceCap,
    /// The held assertion ran out before the key step registered this device (enrol.ts E_SIGNIN_EXPIRED).
    #[error("the sign-in ran out")]
    Expired,
    /// The account has no sealed backup on the instance (enrol.ts E_NO_BACKUP).
    #[error("the account has no recovery data on this instance")]
    NoBackup,
    /// The recovery key does not open the account's backup.
    #[error("this is not the recovery key of this account")]
    WrongKey,
    /// A recovery key is 52 characters (ignoring spaces and dashes).
    #[error("a recovery key has 52 characters")]
    KeyLength,
    /// The account's devices changed meanwhile (enrol.ts E_LIST_RACE).
    #[error("the account's devices changed while signing in")]
    ListRace,
    /// The registration was replaced by another (enrol.ts E_SIGNIN_EVICTED).
    #[error("someone else is signing in to this account")]
    Evicted,
    /// Too many attempts (429), with the instance's wait in ms when it named one.
    #[error("too many sign-in attempts")]
    TooMany(Option<u64>),
    /// The instance could not be reached.
    #[error("the connection dropped: {0}")]
    Network(String),
    /// The local store failed.
    #[error("the local store failed: {0}")]
    Store(String),
    /// Anything else, with its code.
    #[error("signing in did not work ({0})")]
    Other(String),
}

impl From<StoreError> for SignInError {
    fn from(e: StoreError) -> Self {
        Self::Store(e.to_string())
    }
}

fn http_error(e: &HttpError) -> SignInError {
    match e {
        HttpError::Status {
            status: 429,
            retry_after_ms,
            ..
        } => SignInError::TooMany(*retry_after_ms),
        HttpError::Status {
            status: s, code, ..
        } => SignInError::Other(code.clone().unwrap_or_else(|| format!("HTTP {s}"))),
        HttpError::Transport(t) => SignInError::Network(t.clone()),
        HttpError::Decode(d) => SignInError::Other(format!("decode: {d}")),
        HttpError::Unanswered { code, detail } => SignInError::Network(format!("{code}: {detail}")),
    }
}

fn core_error(e: &dilla_core::client::ClientError) -> SignInError {
    SignInError::Other(format!("{e:?}"))
}

/// What a password login still owes (enrol.ts `needs`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Owed {
    /// Nothing: the device can be added.
    Nothing,
    /// An authenticator code.
    Code,
    /// A passkey.
    Passkey,
}

impl SignInError {
    /// The web's refusal code for this error (`signin.error.other`'s `{code}`), never the instance's detail text.
    pub fn code(&self) -> String {
        match self {
            Self::LoginFailed | Self::CodeFailed | Self::BackupFailed => "E_UNAUTHENTICATED".into(),
            Self::NoAssertion => "E_NO_ASSERTION".into(),
            Self::PasskeyOnly => "E_PASSKEY_REQUIRED".into(),
            Self::PasskeyUnknown => "E_PASSKEY_UNKNOWN".into(),
            Self::PasskeyOtherAccount => "E_PASSKEY_OTHER_ACCOUNT".into(),
            Self::Browser(_) => "E_PASSKEY".into(),
            Self::DeviceCap => "E_FORBIDDEN".into(),
            Self::Expired => "E_SIGNIN_EXPIRED".into(),
            Self::NoBackup => "E_NO_BACKUP".into(),
            Self::WrongKey => "E_RECOVERY_KEY".into(),
            Self::KeyLength => "E_KEY_LENGTH".into(),
            Self::ListRace => "E_LIST_RACE".into(),
            Self::Evicted => "E_SIGNIN_EVICTED".into(),
            Self::TooMany(_) => "E_RATE_LIMITED".into(),
            Self::Network(_) => "E_NETWORK".into(),
            Self::Store(_) => "E_STORE".into(),
            Self::Other(code) => code.chars().take(48).collect(),
        }
    }
}

/// The held server assertion and what it still owes.
struct Assertion {
    value: Zeroizing<String>,
    owes: Owed,
}

/// The status and code of a refused route.
fn refused(e: &HttpError) -> Option<(u16, &str)> {
    match e {
        HttpError::Status { status, code, .. } => {
            Some((*status, code.as_deref().unwrap_or("E_HTTP")))
        }
        _ => None,
    }
}

/// One element of a credential-carrying body.
enum Part<'a> {
    Text(&'a str),
    Bytes(&'a [u8]),
}

/// `[p0, p1, ...]`, encoded into one zeroizing buffer of its final size (no reallocation leaves a copy behind).
fn secret_body(parts: &[Part<'_>]) -> r::Secret {
    // Each head is at most 9 bytes, the array head 1.
    let size = 1 + parts
        .iter()
        .map(|p| {
            9 + match p {
                Part::Text(t) => t.len(),
                Part::Bytes(b) => b.len(),
            }
        })
        .sum::<usize>();
    let mut e = dilla_core::cbor::Encoder::with_capacity(size);
    e.array(parts.len());
    for p in parts {
        match p {
            Part::Text(t) => {
                e.text(t);
            }
            Part::Bytes(b) => {
                e.bytes(b);
            }
        }
    }
    r::Secret(Zeroizing::new(e.into_vec()))
}

/// `[assertion, ...]`: the new assertion of a recovery or passkey answer, read into a zeroizing buffer, and the
/// passkey answer's `needs` (0 or 1) when `with_needs`.
fn decode_assertion(raw: &[u8], with_needs: bool) -> Result<(Zeroizing<String>, u8), SignInError> {
    let raw = Zeroizing::new(raw.to_vec());
    dilla_core::cbor::decode_strict(&raw, |d| {
        let len = d.array_len()?;
        let want = if with_needs { 2 } else { 1 };
        if len < want {
            return Err(dilla_core::cbor::CborError::Truncated);
        }
        let assertion = Zeroizing::new(d.text()?.to_owned());
        let needs = if with_needs { d.uint()? } else { 0 };
        if needs > 1 {
            return Err(dilla_core::cbor::CborError::IntegerOverflow);
        }
        for _ in want..len {
            d.skip()?;
        }
        Ok((assertion, needs as u8))
    })
    .map_err(|_| SignInError::Other("E_DECODE".into()))
}

/// One sign-in on a new device of one instance. Each step is `async` (reqwest needs a tokio runtime); the store is
/// opened on the first call that needs it.
pub struct SignIn {
    cfg: InstanceConfig,
    kc: Arc<dyn Keychain>,
    auth: AuthToken,
    http: ReqwestHttp,
    core: Option<Arc<Mutex<ClientCore>>>,
    assertion: Option<Assertion>,
    username: String,
    /// The password login said the account holds a passkey (wave B task 3).
    holds_passkey: bool,
    /// The factor this ceremony signed in with so far: a passkey whose authenticator did not verify the person still
    /// owes a code (web `account.signIn.factor === 'passkey'`).
    used_passkey: bool,
    /// When the held assertion was issued.
    held_at: Option<std::time::Instant>,
    /// The held assertion is a first-factor login's (password or OIDC, still owing a factor): a passkey finish carries
    /// it (enrol.ts `boundFirstFactor`).
    bound_first_factor: bool,
}

/// What the instance's document says about signing in (`GET /v1/instance`).
pub use crate::features::factors::InstanceDoc;

impl std::fmt::Debug for SignIn {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("SignIn")
            .field("server", &self.cfg.server.as_str())
            .field("username", &self.username)
            .finish_non_exhaustive()
    }
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

fn now_s() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

fn cbor_list_of_bytes(object: &[u8]) -> Raw {
    let mut e = dilla_core::cbor::Encoder::new();
    e.array(1).bytes(object);
    Raw(e.into_vec())
}

/// trust.ts `chainRow`: a five- or eight-element device-list head is cut to its first four elements (the list body the
/// core decodes); anything else is passed as served.
pub fn chain_row(head: &[u8]) -> Vec<u8> {
    match Value::decode(head) {
        Ok(Value::Array(items)) if items.len() == 5 || items.len() == 8 => {
            Value::Array(items[..4].to_vec()).encode()
        }
        _ => head.to_vec(),
    }
}

/// A recovery key as typed: spaces and dashes dropped, upper-cased (client-core `normaliseRecoveryKey`).
pub fn normalise_recovery_key(typed: &str) -> Zeroizing<String> {
    Zeroizing::new(
        typed
            .chars()
            .filter(|c| !c.is_whitespace() && *c != '-')
            .map(|c| c.to_ascii_uppercase())
            .collect(),
    )
}

impl SignIn {
    /// A sign-in against `cfg`'s instance; nothing is sent yet.
    pub fn new(cfg: InstanceConfig, kc: Arc<dyn Keychain>) -> Result<Self, SignInError> {
        let auth = AuthToken::anonymous();
        let http = ReqwestHttp::new(cfg.server.clone(), auth.clone())
            .map_err(|e| SignInError::Network(e.to_string()))?;
        Ok(Self {
            cfg,
            kc,
            auth,
            http,
            core: None,
            assertion: None,
            username: String::new(),
            holds_passkey: false,
            used_passkey: false,
            held_at: None,
            bound_first_factor: false,
        })
    }

    /// The instance's name and its sign-in methods.
    pub async fn instance_doc(&self) -> Result<InstanceDoc, SignInError> {
        let Raw(doc) = self
            .http
            .call::<r::GetInstance>(&[], NoBody)
            .await
            .map_err(|e| http_error(&e))?;
        InstanceDoc::decode(&doc).map_err(|e| SignInError::Other(e.code))
    }

    /// The username of this ceremony's login (lower-cased as sent).
    pub fn username(&self) -> &str {
        &self.username
    }

    /// The login said the account holds a passkey, or the person just used one: the code step offers "Use a passkey".
    pub fn holds_passkey(&self) -> bool {
        self.holds_passkey || self.used_passkey
    }

    /// The passkey just used did not verify the person, so the code step says so (`factorSignin.passkey.needsCode`).
    pub fn used_passkey(&self) -> bool {
        self.used_passkey
    }

    /// What the held assertion still owes, if one is held.
    pub fn owes(&self) -> Option<Owed> {
        self.assertion.as_ref().map(|a| a.owes)
    }

    /// enrol.ts `signInBack`: the held assertion is dropped, so nothing half-signed-in survives; the login asks again.
    ///
    /// An enrolment this ceremony registered (a wrong key kept it, phase 3) is reset and its pending session dropped, as
    /// the web's signInBack and signInCancel do: the next login (possibly another account) starts clean.
    pub fn back(&mut self) {
        self.assertion = None;
        self.held_at = None;
        self.bound_first_factor = false;
        self.used_passkey = false;
        if self.core.is_some() {
            let _ = self.with_core(|c| {
                let id = c.identity()?;
                if crate::sync::api::Identity::decode(&id).is_some_and(|i| i.phase == 3) {
                    c.enrol_reset()?;
                    c.session_clear()?;
                }
                Ok(())
            });
        }
        self.auth.clear();
    }

    /// `POST /v1/auth/passkey/login/begin` `[]`: a discoverable ceremony for the system browser.
    pub async fn passkey_begin(
        &mut self,
    ) -> Result<crate::features::factors::Ceremony, SignInError> {
        let Raw(raw) = self
            .http
            .call::<r::PasskeyLoginBegin>(&[], Raw::empty_list())
            .await
            .map_err(|e| http_error(&e))?;
        crate::features::factors::decode_ceremony(&raw).map_err(|e| SignInError::Other(e.code))
    }

    /// `POST /v1/auth/passkey/login/finish` `[ceremony_id, response, assertion?]`: bound to the held assertion when a
    /// password login owes its factor (wave B task 3), unbound on its own. Answers what is still owed: nothing, or a
    /// code when the authenticator did not verify the person.
    pub async fn passkey_finish(
        &mut self,
        ceremony: [u8; 16],
        response: &str,
    ) -> Result<Owed, SignInError> {
        // enrol.ts `passkey`: only a first-factor login's assertion (a password or OIDC login still owing a factor) is
        // bound, so the instance refuses a passkey of another account; the held assertion is not spent, and a refused
        // finish (unknown passkey, another account's, a dropped connection) leaves it as it was.
        let bound = match &self.assertion {
            Some(Assertion { value, owes })
                if self.bound_first_factor && *owes != Owed::Nothing =>
            {
                Some(value.clone())
            }
            _ => None,
        };
        let mut parts = vec![Part::Bytes(&ceremony), Part::Text(response)];
        if let Some(a) = &bound {
            parts.push(Part::Text(a));
        }
        let body = secret_body(&parts);
        let raw = match self.http.call::<r::PasskeyLoginFinish>(&[], body).await {
            Ok(Raw(raw)) => raw,
            Err(e) => {
                return Err(match refused(&e) {
                    Some((_, "E_PASSKEY_UNKNOWN")) => SignInError::PasskeyUnknown,
                    Some((_, "E_PASSKEY_OTHER_ACCOUNT")) => SignInError::PasskeyOtherAccount,
                    _ => http_error(&e),
                });
            }
        };
        let (assertion, needs) = decode_assertion(&raw, true)?;
        let owes = if needs == 1 {
            Owed::Code
        } else {
            Owed::Nothing
        };
        self.used_passkey = true;
        // The passkey names the account: the typed username is not kept.
        self.username.clear();
        self.hold(assertion, owes);
        Ok(owes)
    }

    /// web `signInOIDC`: the identity provider's assertion from the system browser, and what it owes. The assertion
    /// names the account, so no username is kept (as for a passkey).
    pub fn oidc(&mut self, assertion: Zeroizing<String>, needs: u8) -> Owed {
        let owes = match needs {
            0 => Owed::Nothing,
            1 => Owed::Code,
            _ => Owed::Passkey,
        };
        self.hold(assertion, owes);
        self.bound_first_factor = owes != Owed::Nothing;
        self.username.clear();
        owes
    }

    fn hold(&mut self, value: Zeroizing<String>, owes: Owed) {
        self.assertion = Some(Assertion { value, owes });
        self.held_at = Some(std::time::Instant::now());
        self.bound_first_factor = false;
    }

    /// The held assertion ran out (enrol.ts `ASSERTION_EXPIRED_MS`, five minutes on the instance).
    fn held_too_long(&self) -> bool {
        self.held_at
            .is_some_and(|t| t.elapsed() >= std::time::Duration::from_secs(5 * 60))
    }

    /// web3.8 (R6) `signInBackupCode`: the backup code the code or passkey step took, sent now with the key. dillad checks
    /// the assertion before the code: a 401 once the held assertion is past its five minutes spent no code
    /// ([`SignInError::Expired`]); inside them it is the refused code ([`SignInError::BackupFailed`]), and the person
    /// logs in again.
    pub async fn add_device_with_backup(
        &mut self,
        backup_code: &str,
        recovery_key: &str,
    ) -> Result<(), SignInError> {
        let key = normalise_recovery_key(recovery_key);
        if key.chars().count() != 52 {
            return Err(SignInError::KeyLength);
        }
        // The key's form is checked before the backup code is spent (controller.ts `checkKey`).
        dilla_core::client::recovery_key_check(&key).map_err(|_| SignInError::WrongKey)?;
        let code = crate::features::factors::normalise_backup_code(backup_code);
        if !crate::features::factors::is_backup_code(&code) {
            return Err(SignInError::BackupFailed);
        }
        let expired = self.held_too_long();
        let Some(Assertion {
            value,
            owes: Owed::Code | Owed::Passkey,
        }) = self.assertion.take()
        else {
            return Err(SignInError::NoAssertion);
        };
        let body = secret_body(&[Part::Text(&value), Part::Text(&code)]);
        let raw = match self.http.call::<r::RecoveryVerify>(&[], body).await {
            Ok(Raw(raw)) => raw,
            Err(e) => {
                return Err(match refused(&e) {
                    Some((401, _)) if expired => SignInError::Expired,
                    Some((s, _)) if (400..500).contains(&s) && s != 429 => {
                        SignInError::BackupFailed
                    }
                    _ => http_error(&e),
                });
            }
        };
        let (assertion, _) = decode_assertion(&raw, false)?;
        self.hold(assertion, Owed::Nothing);
        self.add_device(recovery_key).await
    }

    /// The instance this sign-in is for.
    pub fn config(&self) -> &InstanceConfig {
        &self.cfg
    }

    fn core(&mut self) -> Result<Arc<Mutex<ClientCore>>, SignInError> {
        if let Some(core) = &self.core {
            return Ok(core.clone());
        }
        let core = Arc::new(Mutex::new(open_store(&self.cfg, self.kc.as_ref())?));
        self.core = Some(core.clone());
        Ok(core)
    }

    fn with_core<T>(
        &mut self,
        f: impl FnOnce(&mut ClientCore) -> Result<T, dilla_core::client::ClientError>,
    ) -> Result<T, SignInError> {
        let core = self.core()?;
        let mut guard = core
            .lock()
            .map_err(|_| SignInError::Store("the store's lock is poisoned".into()))?;
        f(&mut guard).map_err(|e| core_error(&e))
    }

    /// enrol.ts `login`: answers what the login still owes.
    pub async fn login(&mut self, username: &str, password: &str) -> Result<Owed, SignInError> {
        self.assertion = None;
        self.used_passkey = false;
        self.holds_passkey = false;
        let answer = match self
            .http
            .call::<r::PasswordLogin>(&[], r::password_login_body(username, password))
            .await
        {
            Ok(a) => a,
            Err(HttpError::Status { status: s, .. }) if (400..500).contains(&s) && s != 429 => {
                return Err(SignInError::LoginFailed);
            }
            Err(e) => return Err(http_error(&e)),
        };
        let owes = match answer.needs {
            0 => Owed::Nothing,
            1 => Owed::Code,
            _ => Owed::Passkey,
        };
        self.holds_passkey = answer.passkey;
        self.hold(answer.assertion, owes);
        self.bound_first_factor = owes != Owed::Nothing;
        username.clone_into(&mut self.username);
        Ok(owes)
    }

    /// enrol.ts `totp`: the authenticator code a login owes. A refused code spends the assertion.
    pub async fn code(&mut self, code: &str) -> Result<(), SignInError> {
        let Some(Assertion {
            value,
            owes: Owed::Code,
        }) = self.assertion.take()
        else {
            return Err(SignInError::NoAssertion);
        };
        let answer = match self
            .http
            .call::<r::TotpVerify>(&[], r::totp_verify_body(&value, code))
            .await
        {
            Ok(a) => a,
            Err(HttpError::Status { status: s, .. }) if (400..500).contains(&s) && s != 429 => {
                return Err(SignInError::CodeFailed);
            }
            Err(e) => return Err(http_error(&e)),
        };
        self.hold(answer.assertion, Owed::Nothing);
        Ok(())
    }

    /// enrol.ts `register`, `fetch` and `complete`: registers this device with the settled assertion, recovers the
    /// account from its sealed backup with `recovery_key`, publishes the device list that names this device, opens an
    /// enrolled session and uploads the resealed state. On success the store holds a ready identity.
    pub async fn add_device(&mut self, recovery_key: &str) -> Result<(), SignInError> {
        let key = normalise_recovery_key(recovery_key);
        if key.chars().count() != 52 {
            return Err(SignInError::KeyLength);
        }
        // REGISTRATION-DEVICES-02: the key's form is checked before anything is registered or spent.
        dilla_core::client::recovery_key_check(&key).map_err(|_| SignInError::WrongKey)?;
        // A wrong key keeps the registration (enrol.ts phase signin-key after it): the retry goes straight on to the
        // fetch with the same enrolment and session.
        let user_id = match self.registered()? {
            Some(user_id) if self.auth.is_set() => user_id,
            _ => self.register().await?,
        };
        let result = match self.fetch(&user_id).await {
            Ok(fetched) => self.complete(&key, &fetched).await,
            Err(e) => Err(e),
        };
        // controller.ts `enrolWithKey`: once the core wrote the enrolment (it left phase 3), a later step that failed is
        // not the sign-in's (an account an operator reset refuses the state backup with E_FACTOR_REQUIRED): entering
        // ready publishes and uploads what is missing, or takes the forced set-up. A list race or an eviction stays one.
        if let Err(e) = &result
            && !matches!(e, SignInError::ListRace | SignInError::Evicted)
            && self.enrolment_written()?
        {
            self.auth.clear();
            return Ok(());
        }
        if result
            .as_ref()
            .is_err_and(|e| !matches!(e, SignInError::WrongKey))
        {
            // enrol.ts `reset`: an enrolment cut short starts over on the next attempt.
            let _ = self.with_core(|c| {
                let id = c.identity()?;
                if crate::sync::api::Identity::decode(&id).is_some_and(|i| i.phase == 3) {
                    c.enrol_reset()?;
                }
                Ok(())
            });
            self.auth.clear();
        }
        result
    }

    /// The core holds the enrolled identity (`enrol_complete` ran: phase 2, the ready identity with its user id).
    fn enrolment_written(&mut self) -> Result<bool, SignInError> {
        let identity = self.with_core(|c| c.identity())?;
        Ok(crate::sync::api::Identity::decode(&identity)
            .is_some_and(|i| i.phase == 2 && i.user_id.is_some()))
    }

    /// The user id of an enrolment this ceremony already registered (identity phase 3 with its user id).
    fn registered(&mut self) -> Result<Option<[u8; 16]>, SignInError> {
        let identity = self.with_core(|c| c.identity())?;
        Ok(crate::sync::api::Identity::decode(&identity)
            .filter(|i| i.phase == 3)
            .and_then(|i| i.user_id))
    }

    async fn register(&mut self) -> Result<[u8; 16], SignInError> {
        let expired = self.held_too_long();
        let assertion = match self.assertion.take() {
            Some(Assertion {
                value,
                owes: Owed::Nothing,
            }) => value,
            Some(Assertion {
                owes: Owed::Passkey,
                ..
            }) => return Err(SignInError::PasskeyOnly),
            _ => return Err(SignInError::NoAssertion),
        };
        let instance = self.instance().await?;
        let device_id = self
            .with_core(|c| {
                match crate::sync::api::Identity::decode(&c.identity()?) {
                    // An unregistered enrolment left by an earlier attempt is reused.
                    Some(i) if i.phase == 3 && i.user_id.is_none() => Ok(i.device_id),
                    _ => c
                        .enrol_begin(&instance.0)
                        .map(|pair| decode_device_id(&pair)),
                }
            })?
            .ok_or_else(|| SignInError::Other("E_CORE_DECODE enrol_begin".into()))?;
        let device_hex = hex(&device_id);
        let challenge = self
            .http
            .call::<r::PostChallenge>(&[&device_hex], Raw::empty_list())
            .await
            .map_err(|e| http_error(&e))?;
        let login = Zeroizing::new(assertion.as_bytes().to_vec());
        let body = self.with_core(|c| c.enrol_session_sign(&challenge.nonce, &login))?;
        let session = match self
            .http
            .call::<r::PostSession>(&[&device_hex], Raw(body))
            .await
        {
            Ok(s) => s,
            // REGISTRATION-DEVICES-02: the registration's 403 is the device cap.
            Err(HttpError::Status { status: 403, .. }) => return Err(SignInError::DeviceCap),
            Err(HttpError::Status { status: 401, .. }) if expired => {
                return Err(SignInError::Expired);
            }
            Err(HttpError::Status { status: 401, .. }) => return Err(SignInError::LoginFailed),
            Err(e) => return Err(http_error(&e)),
        };
        if session.scope != 1 {
            return Err(SignInError::Other("E_SESSION_SCOPE".into()));
        }
        let token = session.token.clone();
        let (expires, idle) = (session.expires, session.idle_expires);
        let user_id = session.user_id;
        self.with_core(|c| {
            c.session_store(&token, expires, idle)?;
            c.enrol_registered(&user_id)
        })?;
        self.auth.set(token.as_str());
        Ok(user_id)
    }

    async fn fetch(&mut self, user_id: &[u8; 16]) -> Result<Fetched, SignInError> {
        let root = self.backup(0).await?.ok_or(SignInError::NoBackup)?;
        let state = self.backup(1).await?.unwrap_or_default();
        let list = self
            .http
            .call::<r::GetDeviceList>(&[&hex(user_id)], NoBody)
            .await
            .map_err(|e| http_error(&e))?;
        if list.status == 404 {
            return Err(SignInError::NoBackup);
        }
        Ok(Fetched {
            root,
            state,
            list_body: chain_row(&list.body),
        })
    }

    async fn backup(&self, kind: u8) -> Result<Option<Vec<u8>>, SignInError> {
        let reply = self
            .http
            .call::<r::GetBackup>(&[&kind.to_string()], NoBody)
            .await
            .map_err(|e| http_error(&e))?;
        if reply.status == 404 {
            return Ok(None);
        }
        match Value::decode(&reply.body) {
            Ok(Value::Array(a)) if a.len() >= 2 => match &a[0] {
                Value::Bytes(b) => Ok(Some(b.clone())),
                _ => Err(SignInError::Other("E_DECODE backup".into())),
            },
            _ => Err(SignInError::Other("E_DECODE backup".into())),
        }
    }

    async fn complete(&mut self, key: &str, fetched: &Fetched) -> Result<(), SignInError> {
        let username = self.username.clone();
        let core = self.core()?;
        let signed = {
            let mut c = core
                .lock()
                .map_err(|_| SignInError::Store("the store's lock is poisoned".into()))?;
            c.enrol_complete(
                key,
                &fetched.root,
                &fetched.state,
                &fetched.list_body,
                &username,
                now_s(),
            )
            .map_err(|e| {
                if e.code == "E_RECOVERY_KEY" {
                    SignInError::WrongKey
                } else {
                    core_error(&e)
                }
            })?
        };
        let (state_sealed, interrupted) = decode_signed_lists(&signed)
            .ok_or_else(|| SignInError::Other("E_CORE_DECODE signed lists".into()))?;
        let identity = self.with_core(|c| c.identity())?;
        let user_id = crate::sync::api::Identity::decode(&identity)
            .and_then(|i| i.user_id)
            .ok_or_else(|| SignInError::Other("E_CORE_NO_IDENTITY".into()))?;
        let user_hex = hex(&user_id);
        if let Some(body) = interrupted {
            // enrol.ts `publishInterrupted`: the list another device signed and did not publish goes first.
            match self
                .http
                .call::<r::PutDeviceList>(&[&user_hex], Raw(body))
                .await
            {
                Ok(()) | Err(HttpError::Status { status: 409, .. }) => {}
                Err(HttpError::Status { status: 401, .. }) => return Err(SignInError::Evicted),
                Err(_) => return Err(SignInError::ListRace),
            }
        }
        let list = self.with_core(|c| c.device_list_body())?;
        match self
            .http
            .call::<r::PutDeviceList>(&[&user_hex], Raw(list))
            .await
        {
            Ok(()) => {}
            Err(HttpError::Status { status: 409, .. }) => return Err(SignInError::ListRace),
            Err(HttpError::Status { status: 401, .. }) => return Err(SignInError::Evicted),
            Err(e) => return Err(http_error(&e)),
        }
        self.with_core(|c| c.device_list_published())?;
        // WORKER-WEB-01: the pending token is not kept past the list that names this device.
        self.with_core(|c| c.session_clear())?;
        self.auth.clear();
        self.establish().await?;
        let (instance_id, signer) = self.instance().await?;
        self.with_core(|c| c.instance_signer_key_pin(&instance_id, &signer))?;
        self.http
            .call::<r::PutBackup>(&["1"], cbor_list_of_bytes(&state_sealed))
            .await
            .map_err(|e| http_error(&e))?;
        self.with_core(|c| c.state_sealed_uploaded())?;
        Ok(())
    }

    /// session.ts `establish` for the device just listed: only an enrolled session (scope 0) is accepted.
    async fn establish(&mut self) -> Result<(), SignInError> {
        let identity = self.with_core(|c| c.identity())?;
        let device = crate::sync::api::Identity::decode(&identity)
            .and_then(|i| i.device_id)
            .ok_or_else(|| SignInError::Other("E_CORE_NO_IDENTITY".into()))?;
        let device_hex = hex(&device);
        let challenge = self
            .http
            .call::<r::PostChallenge>(&[&device_hex], Raw::empty_list())
            .await
            .map_err(|e| http_error(&e))?;
        let body = self.with_core(|c| c.session_sign(&challenge.nonce, 0))?;
        let session = match self
            .http
            .call::<r::PostSession>(&[&device_hex], Raw(body))
            .await
        {
            Ok(s) => s,
            Err(HttpError::Status { status: 401, .. }) => return Err(SignInError::Evicted),
            Err(e) => return Err(http_error(&e)),
        };
        if session.scope != 0 {
            return Err(SignInError::Other("E_SESSION_SCOPE".into()));
        }
        let token = session.token.clone();
        let (expires, idle) = (session.expires, session.idle_expires);
        self.with_core(|c| c.session_store(&token, expires, idle))?;
        self.auth.set(token.as_str());
        Ok(())
    }

    /// `GET /v1/instance`: its id (element 3) and external-sender key (element 10).
    async fn instance(&self) -> Result<([u8; 16], [u8; 32]), SignInError> {
        let Raw(doc) = self
            .http
            .call::<r::GetInstance>(&[], NoBody)
            .await
            .map_err(|e| http_error(&e))?;
        let bad = || SignInError::Other("E_DECODE instance".into());
        let Ok(Value::Array(a)) = Value::decode(&doc) else {
            return Err(bad());
        };
        let id = a
            .get(3)
            .and_then(Value::as_bytes)
            .and_then(|b| <[u8; 16]>::try_from(b).ok())
            .ok_or_else(bad)?;
        let signer = a
            .get(10)
            .and_then(Value::as_bytes)
            .and_then(|b| <[u8; 32]>::try_from(b).ok())
            .ok_or_else(bad)?;
        Ok((id, signer))
    }
}

struct Fetched {
    root: Vec<u8>,
    state: Vec<u8>,
    list_body: Vec<u8>,
}

/// `enrol_begin`'s `[device_id, dsk_pub]`.
fn decode_device_id(pair: &[u8]) -> Option<[u8; 16]> {
    match Value::decode(pair).ok()? {
        Value::Array(a) => a.first()?.as_bytes()?.try_into().ok(),
        _ => None,
    }
}

/// `enrol_complete`'s `[put, state, interrupted|null]`: the state object and the interrupted list.
fn decode_signed_lists(raw: &[u8]) -> Option<(Vec<u8>, Option<Vec<u8>>)> {
    let Value::Array(a) = Value::decode(raw).ok()? else {
        return None;
    };
    let state = a.get(1)?.as_bytes()?.to_vec();
    let interrupted = match a.get(2)? {
        Value::Null => None,
        v => Some(v.as_bytes()?.to_vec()),
    };
    Some((state, interrupted))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Task 28 item 22: a 429's delay reaches the sign-in's "too many attempts" (the screen says how long to wait).
    #[test]
    fn too_many_attempts_carries_the_delay() {
        let st = |retry_after_ms| HttpError::Status {
            status: 429,
            code: Some("E_RATE_LIMITED".into()),
            retry_after_ms,
        };
        assert!(matches!(
            http_error(&st(Some(30_000))),
            SignInError::TooMany(Some(30_000))
        ));
        assert!(matches!(http_error(&st(None)), SignInError::TooMany(None)));
    }

    #[test]
    fn chain_row_cuts_five_and_eight_element_heads() {
        let four = Value::Array(vec![
            Value::Uint(3),
            Value::Bytes(vec![1]),
            Value::Bytes(vec![2]),
            Value::Bytes(vec![3]),
        ]);
        let mut five = four.clone();
        if let Value::Array(a) = &mut five {
            a.push(Value::Bytes(vec![9]));
        }
        assert_eq!(chain_row(&five.encode()), four.encode());
        assert_eq!(chain_row(&four.encode()), four.encode());
        assert_eq!(chain_row(b"\xff"), b"\xff".to_vec());
    }

    #[test]
    fn recovery_keys_are_normalised() {
        assert_eq!(
            normalise_recovery_key(" abcd-efgh ijkl\n").as_str(),
            "ABCDEFGHIJKL"
        );
    }

    #[test]
    fn signed_lists_decode() {
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(3).bytes(b"put").bytes(b"state").null();
        assert_eq!(
            decode_signed_lists(&e.into_vec()),
            Some((b"state".to_vec(), None))
        );
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(2).bytes(b"id-16-bytes-long").bytes(&[0; 32]);
        assert_eq!(decode_device_id(&e.into_vec()), Some(*b"id-16-bytes-long"));
    }

    /// A scripted instance: each request gets the next (status, CBOR body); `None` closes the connection unanswered.
    async fn script(replies: Vec<Option<(u16, Vec<u8>)>>) -> url::Url {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        tokio::spawn(async move {
            for reply in replies {
                let Ok((mut s, _)) = listener.accept().await else {
                    return;
                };
                let mut buf = vec![0u8; 65536];
                let mut got = Vec::new();
                loop {
                    let n = s.read(&mut buf).await.unwrap_or(0);
                    if n == 0 {
                        break;
                    }
                    got.extend_from_slice(&buf[..n]);
                    let text = String::from_utf8_lossy(&got).to_string();
                    if let Some(end) = text.find("\r\n\r\n") {
                        let len = text
                            .lines()
                            .find_map(|l| {
                                l.to_ascii_lowercase()
                                    .strip_prefix("content-length:")
                                    .map(|v| v.trim().parse::<usize>().unwrap())
                            })
                            .unwrap_or(0);
                        if got.len() >= end + 4 + len {
                            break;
                        }
                    }
                }
                let Some((status, body)) = reply else {
                    drop(s);
                    continue;
                };
                let head = format!(
                    "HTTP/1.1 {status} X\r\ncontent-type: application/cbor\r\ncontent-length: {}\r\nconnection: close\r\n\r\n",
                    body.len()
                );
                s.write_all(head.as_bytes()).await.unwrap();
                s.write_all(&body).await.unwrap();
                s.shutdown().await.ok();
            }
        });
        url::Url::parse(&format!("http://127.0.0.1:{port}/")).unwrap()
    }

    fn cbor(f: impl FnOnce(&mut dilla_core::cbor::Encoder)) -> Vec<u8> {
        let mut e = dilla_core::cbor::Encoder::new();
        f(&mut e);
        e.into_vec()
    }

    /// enrol.ts `passkey`: a refused finish (an unknown passkey, a dropped connection) leaves the password login's
    /// assertion held with what it owes, so the code step still works.
    #[tokio::test]
    async fn a_refused_passkey_keeps_the_password_login() {
        let refusal = cbor(|e| {
            e.array(3).text("E_PASSKEY_UNKNOWN").text("").null();
        });
        let server = script(vec![
            Some((
                200,
                cbor(|e| {
                    e.array(2).text("login-assertion").uint(1);
                }),
            )),
            Some((401, refusal)),
            None,
            Some((
                200,
                cbor(|e| {
                    e.array(1).text("settled-assertion");
                }),
            )),
        ])
        .await;
        let cfg = InstanceConfig {
            id: crate::seams::InstanceId(0),
            server,
            data_dir: std::env::temp_dir().join(format!("t26-signin-{}", std::process::id())),
        };
        let mut flow = SignIn::new(cfg, Arc::new(crate::keychain::MemoryKeychain::new())).unwrap();
        assert_eq!(flow.login("ada", "pw").await.unwrap(), Owed::Code);
        assert_eq!(
            flow.passkey_finish([1; 16], "{}").await.unwrap_err(),
            SignInError::PasskeyUnknown
        );
        assert_eq!(flow.owes(), Some(Owed::Code));
        assert!(flow.passkey_finish([1; 16], "{}").await.is_err());
        assert_eq!(flow.owes(), Some(Owed::Code));
        flow.code("123456").await.unwrap();
        assert_eq!(flow.owes(), Some(Owed::Nothing));
    }
}
