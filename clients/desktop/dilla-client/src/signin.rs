//! Signing in on a new device, as the web client's sign-in screen drives `packages/client-core/src/account/enrol.ts`:
//! a password login, the authenticator code it owes, then the device's registration and its recovery from the
//! account's sealed backup with the recovery key. The device ends with a ready identity (phase 2) in its store, its
//! device list published and its sealed state uploaded, so [`crate::Client::start`] can take over.
//!
//! Every secret (the password, the code, the recovery key, the assertion and the session token) is held in
//! [`Zeroizing`] buffers or the core and never printed. Narrowed against enrol.ts: a passkey or backup code instead of
//! the app code, and the device-list fork repair after a 409 (adoptServedList), are not ported; a 409 on the list is
//! [`SignInError::ListRace`] and the person signs in again.

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
    /// This login owes a passkey, which this client cannot offer yet.
    #[error("this account signs in with a passkey")]
    PasskeyOnly,
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
    /// Too many attempts (429).
    #[error("too many sign-in attempts")]
    TooMany,
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
        HttpError::Status(429) => SignInError::TooMany,
        HttpError::Status(s) => SignInError::Other(format!("HTTP {s}")),
        HttpError::Transport(t) => SignInError::Network(t.clone()),
        HttpError::Decode(d) => SignInError::Other(format!("decode: {d}")),
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

/// The held server assertion and what it still owes.
struct Assertion {
    value: Zeroizing<String>,
    owes: Owed,
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
}

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
        })
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
        let answer = match self
            .http
            .call::<r::PasswordLogin>(&[], r::password_login_body(username, password))
            .await
        {
            Ok(a) => a,
            Err(HttpError::Status(s)) if (400..500).contains(&s) && s != 429 => {
                return Err(SignInError::LoginFailed);
            }
            Err(e) => return Err(http_error(&e)),
        };
        let owes = match answer.needs {
            0 => Owed::Nothing,
            1 => Owed::Code,
            _ => Owed::Passkey,
        };
        self.assertion = Some(Assertion {
            value: answer.assertion,
            owes,
        });
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
            Err(HttpError::Status(s)) if (400..500).contains(&s) && s != 429 => {
                return Err(SignInError::CodeFailed);
            }
            Err(e) => return Err(http_error(&e)),
        };
        self.assertion = Some(Assertion {
            value: answer.assertion,
            owes: Owed::Nothing,
        });
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
        let user_id = self.register().await?;
        let fetched = self.fetch(&user_id).await?;
        let result = self.complete(&key, &fetched).await;
        if result.is_err() {
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

    async fn register(&mut self) -> Result<[u8; 16], SignInError> {
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
            Err(HttpError::Status(s)) if s == 401 || s == 403 => {
                return Err(SignInError::LoginFailed);
            }
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
                Ok(()) | Err(HttpError::Status(409)) => {}
                Err(HttpError::Status(401)) => return Err(SignInError::Evicted),
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
            Err(HttpError::Status(409)) => return Err(SignInError::ListRace),
            Err(HttpError::Status(401)) => return Err(SignInError::Evicted),
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
            Err(HttpError::Status(401)) => return Err(SignInError::Evicted),
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
}
