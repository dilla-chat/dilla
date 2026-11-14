//! Area **onboarding** (Task 25): sign-up (invite, name, recovery key, then the account), and the running client's
//! create and join flows (start a server, add a channel, make an invite, join by invite).
//!
//! Ports: packages/client-core/src/account/signup.ts, packages/client-core/src/recovery-key.ts, the worker's
//! `signupBegin`, `signupSubmit`, `signupReset`, `factorSetupDone`'s pending join, `joinCommunity`, `createCommunity`,
//! `createChannel` and `createInvite` (packages/client-core/src/worker/controller.ts), packages/web/src/invite.ts,
//! packages/web/src/screens/recovery-file.ts and the name rules of packages/web/src/screens/{Onboarding,Create}.tsx.
//!
//! [`Signup`] runs before a [`crate::Client`] exists (like [`crate::signin`]), so it has no `Client` accessor. Its steps:
//! [`Signup::instance`] reads what the instance takes, [`Signup::begin`] makes the keys and answers the recovery key
//! (shown once, then dropped: a step back or a refused creation discards it with [`Signup::reset`]), and
//! [`Signup::submit`] creates the account, signs the device list and publishes it. The account then owes its second
//! factor (web3.6): the client starts in `FactorSetup`, and the invite's server is joined by
//! [`Onboarding::finish_signup`] once the factor is in place, as the worker's `factorSetupDone` joins it.
//!
//! **Secrets.** The recovery key, the password and the session token live only in [`Zeroizing`] buffers (or the core),
//! never in the state, a log, an error or a `Debug`. The recovery key is never written anywhere by this module: the
//! person writes it down (the screen's "Save as file" is their own act, into a file they choose).

use std::sync::{Arc, Mutex};

use dilla_core::client::ClientCore;
use zeroize::Zeroizing;

use crate::auth::AuthToken;
use crate::client::{Client, ClientError, Inner};
use crate::gateway::frames::Value;
use crate::http::ReqwestHttp;
use crate::http::routes::{self as r, NoBody, Raw, Secret};
use crate::seams::{Http, HttpError, InstanceConfig, Keychain, Route};
use crate::state::{ChannelId, CommunityId};
use crate::store::{StoreError, open_store};
use crate::sync::api::{ApiError, Identity};

// ---- the rules the screens check before anything is sent ----

/// A new password has at least this many characters (code points); the instance refuses fewer (protocol/09).
pub const PASSWORD_MIN: usize = 12;
/// internal/api `maxCommunityNameBytes`.
pub const SERVER_NAME_MAX_BYTES: usize = 255;
/// internal/api `maxChannelNameBytes`.
pub const CHANNEL_NAME_MAX_BYTES: usize = 100;
/// An invite link made by the create flow works this many times (Create.tsx `INVITE_MAX_USES`).
pub const INVITE_MAX_USES: u64 = 10;
/// ... and for this many days (Create.tsx `INVITE_TTL_DAYS`).
pub const INVITE_TTL_DAYS: u64 = 7;
/// The built-in AFK channel's position (admin/afk.ts `AFK_BUILTIN_POSITION`): never counted for the next position.
pub const AFK_BUILTIN_POSITION: u64 = 1 << 30;
/// The store setting a sign-up keeps its join in until the factor is paid (state/settings.ts `PENDING_JOIN_KEY`).
pub const PENDING_JOIN_KEY: &str = "signup.pendingJoin";

/// `send_messages` (admin/perm.ts `PERM.sendMessages`).
const PERM_SEND_MESSAGES: u64 = 1 << 1;
const PERM_MANAGE_CHANNELS: u64 = 1 << 14;
const PERM_MANAGE_ROLES: u64 = 1 << 15;
const PERM_ADMINISTRATOR: u64 = 1 << 20;

/// internal/auth/handle.go: 3 to 32 of a-z 0-9 . _ - after case mapping, no leading or trailing dot (Onboarding.tsx
/// `USERNAME`, applied to the trimmed, lower-cased name).
pub fn username_ok(raw: &str) -> bool {
    let name = raw.trim().to_lowercase();
    let n = name.chars().count();
    (3..=32).contains(&n)
        && name
            .chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || matches!(c, '.' | '_' | '-'))
        && !name.starts_with('.')
        && !name.ends_with('.')
}

/// The name the account is created with: trimmed and lower-cased (Onboarding.tsx `keysNext`).
pub fn username_of(raw: &str) -> String {
    raw.trim().to_lowercase()
}

/// At least [`PASSWORD_MIN`] code points.
pub fn password_ok(password: &str) -> bool {
    password.chars().count() >= PASSWORD_MIN
}

/// invite.ts `extractInviteCode`: the path segment after `/i/`, else the `invite` query value, else the trimmed input.
pub fn extract_invite_code(input: &str) -> String {
    let trimmed = input.trim();
    let stops = |c: char| matches!(c, '/' | '?' | '#') || c.is_whitespace();
    if let Some(at) = trimmed.find("/i/") {
        let rest = &trimmed[at + 3..];
        let end = rest.find(stops).unwrap_or(rest.len());
        if end > 0 {
            return percent_decoded(&rest[..end]);
        }
    }
    for (at, _) in trimmed.match_indices("invite=") {
        let before = trimmed[..at].chars().last();
        if matches!(before, Some('?' | '&')) {
            let rest = &trimmed[at + 7..];
            let end = rest
                .find(|c: char| c == '&' || c == '#' || c.is_whitespace())
                .unwrap_or(rest.len());
            return percent_decoded(&rest[..end].replace('+', " "));
        }
    }
    trimmed.to_owned()
}

/// An invite link as the desktop takes it: the instance it names (canonical, `scheme://host[:port]/`) and its code.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct InviteLink {
    pub instance: url::Url,
    pub code: String,
}

/// Why a pasted invite link is refused before anything is stored or sent.
#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
pub enum InviteLinkError {
    /// Not a link of the form `https://host/i/<code>` (a bare code, another page, a bad code or encoding).
    #[error("not an invite link")]
    Invalid,
    /// Not https (http only to the loopback, and only in a debug build).
    #[error("an HTTPS instance is required")]
    Insecure,
    /// It carries a user name, a password, a query or a fragment.
    #[error("instance links cannot contain credentials, queries or fragments")]
    Credentials,
}

/// The desktop's `policy.ts canonicalInstance` and invite route, for a pasted invite link: one implementation, Task 27's
/// [`crate::instances::address::invite_link`] (the strictest checks of the two ports: at most 2048 characters with no
/// white space, backslash or control character, no `%2f`, `%5c`, `%00` or `%2e`; no user info and no `?` or `#`; https,
/// or http to `127.0.0.1`, `[::1]` or `localhost` when `allow_loopback` (debug builds); a host not ending in a dot; the
/// path exactly `/i/<code>` with the code `[A-Za-z0-9_-]{1,128}`), its refusals in the sign-up's terms.
pub fn parse_invite_link(input: &str, allow_loopback: bool) -> Result<InviteLink, InviteLinkError> {
    use crate::instances::address;
    let parsed = address::invite_link(input, allow_loopback).map_err(|e| match e.0 {
        address::CREDENTIALS => InviteLinkError::Credentials,
        address::INSECURE => InviteLinkError::Insecure,
        _ => InviteLinkError::Invalid,
    })?;
    Ok(InviteLink {
        instance: parsed.instance,
        code: parsed.invite.ok_or(InviteLinkError::Invalid)?,
    })
}

/// Writes the recovery key file the person chose: a new temporary file in the same directory (created exclusively,
/// mode 0600, never through a symlink), synced, then renamed over `path`. A file already at `path` is replaced (its
/// old mode does not carry over), and a symlink there is replaced, never followed. The temporary file is removed on
/// any failure.
pub fn write_key_file(path: &std::path::Path, bytes: &[u8]) -> std::io::Result<()> {
    use std::io::Write;
    let dir = path
        .parent()
        .filter(|d| !d.as_os_str().is_empty())
        .unwrap_or_else(|| std::path::Path::new("."));
    let mut salt = [0u8; 8];
    getrandom::fill(&mut salt).map_err(|e| std::io::Error::other(e.to_string()))?;
    let temp = dir.join(format!(".dilla-key-{}.tmp", hex(&salt)));
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600).custom_flags(libc::O_NOFOLLOW);
    }
    let written = (|| {
        let mut file = options.open(&temp)?;
        file.write_all(bytes)?;
        file.sync_all()?;
        drop(file);
        std::fs::rename(&temp, path)
    })();
    if written.is_err() {
        let _ = std::fs::remove_file(&temp);
    }
    written
}

/// `decodeURIComponent`, or the raw text when it is not valid percent-encoding of UTF-8.
fn percent_decoded(raw: &str) -> String {
    let bytes = raw.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' {
            let hex = bytes
                .get(i + 1..i + 3)
                .and_then(|h| std::str::from_utf8(h).ok());
            match hex.and_then(|h| u8::from_str_radix(h, 16).ok()) {
                Some(b) => {
                    out.push(b);
                    i += 3;
                    continue;
                }
                None => return raw.to_owned(),
            }
        }
        out.push(bytes[i]);
        i += 1;
    }
    String::from_utf8(out).unwrap_or_else(|_| raw.to_owned())
}

/// Why a server or channel name would be refused (Create.tsx `nameProblem`), on the trimmed name.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum NameProblem {
    Empty,
    Long,
    Control,
}

/// Create.tsx `nameProblem`: empty, longer than `max_bytes` UTF-8 bytes, or holding a control character.
pub fn name_problem(raw: &str, max_bytes: usize) -> Option<NameProblem> {
    let name = raw.trim();
    if name.is_empty() {
        Some(NameProblem::Empty)
    } else if name.len() > max_bytes {
        Some(NameProblem::Long)
    } else if name.chars().any(char::is_control) {
        Some(NameProblem::Control)
    } else {
        None
    }
}

/// The kinds a new channel takes (Create.tsx `NewChannelKind`, plus the channel list's category).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum NewChannelKind {
    Text,
    Voice,
    /// A text channel whose @everyone may not send (an announcement channel, read-only from birth).
    Announcement,
    /// A category (kind 2), which holds channels.
    Category,
}

impl NewChannelKind {
    /// The channel row's kind on the wire: 0 text, 1 voice, 2 category.
    pub fn wire(self) -> u64 {
        match self {
            Self::Text | Self::Announcement => 0,
            Self::Voice => 1,
            Self::Category => 2,
        }
    }

    /// Whether its name is a lower-case slug (`ship-talk`) rather than kept as typed.
    pub fn slugged(self) -> bool {
        !matches!(self, Self::Voice | Self::Category)
    }
}

/// Create.tsx `channelName`: how a typed name is stored. Text and announcement channels as a lower-case slug, the
/// others as typed, trimmed, with runs of white space as one space.
pub fn channel_name(kind: NewChannelKind, raw: &str) -> String {
    let sep = if kind.slugged() { "-" } else { " " };
    let spaced = raw.split_whitespace().collect::<Vec<_>>().join(sep);
    if kind.slugged() {
        spaced.to_lowercase()
    } else {
        spaced
    }
}

/// admin/controller.ts `nextPosition`: one past the highest position below the AFK reservation.
pub fn next_position(positions: impl IntoIterator<Item = u64>) -> u64 {
    positions
        .into_iter()
        .filter(|p| *p < AFK_BUILTIN_POSITION)
        .map(|p| p + 1)
        .max()
        .unwrap_or(0)
}

/// recovery-file.ts `instanceSlug`: lower case, every run of characters outside a-z 0-9 . - as one hyphen, no leading
/// or trailing dot or hyphen, at most 64 characters; "dilla" when nothing is left. (NFKC is not applied: a name that
/// needs it slugs its compatibility characters to hyphens instead.)
pub fn instance_slug(name: &str) -> String {
    const SLUG_MAX: usize = 64;
    let mut slug = String::new();
    for c in name.to_lowercase().chars() {
        if c.is_ascii_lowercase() || c.is_ascii_digit() || c == '.' || c == '-' {
            slug.push(c);
        } else if !slug.ends_with('-') {
            slug.push('-');
        }
    }
    let mut collapsed = String::with_capacity(slug.len());
    for c in slug.chars() {
        if !(c == '-' && collapsed.ends_with('-')) {
            collapsed.push(c);
        }
    }
    let trim = |s: &str| s.trim_matches(|c| c == '.' || c == '-').to_owned();
    let capped = trim(&trim(&collapsed).chars().take(SLUG_MAX).collect::<String>());
    if capped.is_empty() {
        "dilla".into()
    } else {
        capped
    }
}

// ---- the recovery key ----

/// A recovery key as the core made it: 52 Crockford base32 characters, shown as 13 groups of 4. Held in a zeroizing
/// buffer; its `Debug` names only its length.
#[derive(Clone)]
pub struct RecoveryKey(Zeroizing<String>);

impl std::fmt::Debug for RecoveryKey {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "RecoveryKey({} characters)", self.0.chars().count())
    }
}

impl RecoveryKey {
    fn new(text: Zeroizing<String>) -> Result<Self, SignupError> {
        if text.len() != 52 || !text.is_ascii() {
            return Err(SignupError::Other("E_CORE_DECODE".into()));
        }
        Ok(Self(text))
    }

    /// A key of 52 ASCII characters for a screen fixture (the automation build draws the key step offline; the web's own
    /// key is a fresh one, and both are masked as text in the comparison). Never in a shipping build.
    #[cfg(feature = "testing")]
    pub fn for_fixture(text: &str) -> Option<Self> {
        Self::new(Zeroizing::new(text.to_owned())).ok()
    }

    /// signup.ts `begin`: the key's 13 groups of 4.
    pub fn groups(&self) -> impl Iterator<Item = &str> {
        (0..13).map(move |i| &self.0[i * 4..i * 4 + 4])
    }

    /// The groups joined by hyphens, as the file and the clipboard carry it, in a zeroizing buffer.
    pub fn hyphenated(&self) -> Zeroizing<String> {
        let mut out = Zeroizing::new(String::with_capacity(64));
        for (i, g) in self.groups().enumerate() {
            if i > 0 {
                out.push('-');
            }
            out.push_str(g);
        }
        out
    }
}

/// recovery-key.ts `normaliseRecoveryKey` (the core's `recovery_key_normalise` is authoritative): white space and the
/// hyphen, en and em dashes dropped, a-z upper-cased, I and L read as 1 and O as 0. Zeroizing.
pub fn normalise_recovery_key(text: &str) -> Zeroizing<String> {
    let mut out = Zeroizing::new(String::with_capacity(text.len()));
    for c in text.chars() {
        if matches!(c, ' ' | '\t' | '\n' | '\r' | '-' | '\u{2013}' | '\u{2014}') {
            continue;
        }
        let up = c.to_ascii_uppercase();
        out.push(match up {
            'I' | 'L' => '1',
            'O' => '0',
            other => other,
        });
    }
    out
}

/// The file "Save as file" offers (recovery-file.ts `recoveryKeyFile`): its suggested name and its body. The caller
/// writes it only where the person chose, and drops the body at once.
pub struct RecoveryKeyFile {
    pub name: String,
    pub text: Zeroizing<String>,
}

/// recovery-file.ts `recoveryKeyFile`. `file_name` is `onboarding.keys.fileName` for the instance's slug and `intro`
/// `onboarding.keys.fileBody` for its name (the caller's strings).
pub fn recovery_key_file(file_name: String, intro: &str, key: &RecoveryKey) -> RecoveryKeyFile {
    let groups = key.hyphenated();
    let mut text = Zeroizing::new(String::with_capacity(intro.len() + groups.len() + 4));
    text.push_str(intro);
    text.push_str("\n\n");
    text.push_str(&groups);
    text.push('\n');
    RecoveryKeyFile {
        name: file_name,
        text,
    }
}

// ---- the instance ----

/// `GET /v1/instance`'s `registrationMode`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RegistrationMode {
    /// An invite is required.
    InviteOnly,
    /// Open to new accounts; an invite is optional.
    Open,
    /// Not taking new accounts.
    Closed,
}

/// What a sign-up needs to know of the instance (controller.ts `instanceStateOf`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct InstanceInfo {
    pub id: [u8; 16],
    pub name: String,
    pub registration_mode: RegistrationMode,
    /// `auth_methods` holds 0: a new account has a password.
    pub password_signup: bool,
    pub totp: bool,
    pub passkeys: bool,
    pub external_sender: [u8; 32],
}

impl InstanceInfo {
    /// routes.ts `getInstance`: `[.., .., .., id, generation, name, mode, methods, policy_version, .., signer]`.
    pub fn decode(raw: &[u8]) -> Option<Self> {
        let Value::Array(a) = Value::decode(raw).ok()? else {
            return None;
        };
        if a.len() < 11 {
            return None;
        }
        let id = <[u8; 16]>::try_from(a[3].as_bytes()?).ok()?;
        let name = a[5].as_text()?.to_owned();
        let registration_mode = match a[6].as_uint()? {
            0 => RegistrationMode::InviteOnly,
            1 => RegistrationMode::Open,
            2 => RegistrationMode::Closed,
            _ => return None,
        };
        let methods: Vec<u64> = a[7]
            .as_array()?
            .iter()
            .map(Value::as_uint)
            .collect::<Option<_>>()?;
        let external_sender = <[u8; 32]>::try_from(a[10].as_bytes()?).ok()?;
        Some(Self {
            id,
            name,
            registration_mode,
            password_signup: methods.contains(&0),
            totp: methods.contains(&1),
            passkeys: methods.contains(&2),
            external_sender,
        })
    }
}

// ---- refusals ----

/// Why a sign-up step failed: Onboarding.tsx `refusalView`'s rows, in its order. Read from the code, the status and
/// the wait only, never from the instance's detail text (protocol/02).
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum SignupError {
    /// E_INVITE_INVALID: back to the invite step, on the invite field.
    #[error("the invite is expired, used up or unknown")]
    InviteInvalid,
    /// E_INVALID_REQUEST with 409: on the username field.
    #[error("the username is taken")]
    UsernameTaken,
    /// Any other E_INVALID_REQUEST: the account step's banner.
    #[error("the instance did not accept these details")]
    DetailsRefused,
    /// E_FORBIDDEN: the instance takes no new accounts (the closed frame).
    #[error("the instance takes no new accounts")]
    Closed,
    /// E_RATE_LIMITED, with the instance's wait when it gave one.
    #[error("too many attempts")]
    RateLimited { retry_after_ms: Option<u64> },
    /// E_NETWORK or a 5xx while creating the account: it may exist already, so the step never sends again; only
    /// [`Signup::resume`] goes on.
    #[error("the connection dropped while creating the account")]
    Stalled,
    /// The instance could not be reached before anything was created.
    #[error("the instance could not be reached")]
    Network,
    /// The local store failed.
    #[error("the local store failed: {0}")]
    Store(String),
    /// Anything else, by its code.
    #[error("the account could not be created ({0})")]
    Other(String),
}

impl SignupError {
    /// The code the `onboarding.error.other` text names.
    pub fn code(&self) -> &str {
        match self {
            Self::InviteInvalid => "E_INVITE_INVALID",
            Self::UsernameTaken | Self::DetailsRefused => "E_INVALID_REQUEST",
            Self::Closed => "E_FORBIDDEN",
            Self::RateLimited { .. } => "E_RATE_LIMITED",
            Self::Stalled | Self::Network => "E_NETWORK",
            Self::Store(_) => "E_STORE",
            Self::Other(code) => code,
        }
    }
}

impl From<StoreError> for SignupError {
    fn from(e: StoreError) -> Self {
        Self::Store(e.to_string())
    }
}

/// refusalView over an instance's answer. `creating`: the account POST was sent, so a lost answer stalls.
fn refusal(e: &HttpError, creating: bool) -> SignupError {
    match e {
        HttpError::Status {
            status,
            code,
            retry_after_ms,
        } => match (code.as_deref(), *status) {
            (Some("E_INVITE_INVALID"), _) => SignupError::InviteInvalid,
            (Some("E_INVALID_REQUEST"), 409) => SignupError::UsernameTaken,
            (Some("E_INVALID_REQUEST"), _) => SignupError::DetailsRefused,
            (Some("E_FORBIDDEN"), _) => SignupError::Closed,
            (Some("E_RATE_LIMITED"), _) | (_, 429) => SignupError::RateLimited {
                retry_after_ms: *retry_after_ms,
            },
            (_, s) if s >= 500 && creating => SignupError::Stalled,
            (_, s) if s >= 500 => SignupError::Network,
            (code, s) => {
                SignupError::Other(code.map_or_else(|| format!("HTTP {s}"), str::to_owned))
            }
        },
        HttpError::Transport(_) | HttpError::Unanswered { .. } if creating => SignupError::Stalled,
        HttpError::Transport(_) | HttpError::Unanswered { .. } => SignupError::Network,
        HttpError::Decode(_) => SignupError::Other("E_DECODE".into()),
    }
}

fn core_error(e: &dilla_core::client::ClientError) -> SignupError {
    SignupError::Other(e.code.to_string())
}

// ---- the sign-up ----

/// `POST /v1/accounts` with the body as a [`Secret`]: it carries the password, so the one copy the transport takes is
/// the only one besides this zeroizing buffer (the table's `PostAccount` row; the request type is the credential one).
struct PostAccountSecret;

impl Route for PostAccountSecret {
    const METHOD: http::Method = http::Method::POST;
    const PATH: &'static str = "/v1/accounts";
    type Request = Secret;
    type Response = Raw;
}

/// What a finished [`Signup::submit`] or [`Signup::resume`] answers.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Submitted {
    /// The account's username, as created.
    pub username: String,
    /// The server the invite names, kept in the store for [`Onboarding::finish_signup`].
    pub pending_join: Option<CommunityId>,
}

/// What [`Signup::resume`] found.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Resumed {
    /// No account on this device: a sign-up starts afresh (keys left from an attempt that never registered are gone).
    Fresh,
    /// The account exists and its device list is published: the client can start.
    Done(Submitted),
    /// The instance no longer accepts the stored session: the account must be signed into instead.
    Revoked,
}

/// One sign-up on a new device of one instance. Each network step is `async` (reqwest needs a tokio runtime); the
/// store opens on the first step that needs it.
pub struct Signup {
    cfg: InstanceConfig,
    kc: Arc<dyn Keychain>,
    auth: AuthToken,
    http: ReqwestHttp,
    core: Option<Arc<Mutex<ClientCore>>>,
    instance: Option<InstanceInfo>,
    /// An account POST was answered (2xx) on this sign-up: never POST again, never reset.
    answered: bool,
    #[cfg(feature = "testing")]
    fault_account_decode: bool,
}

impl std::fmt::Debug for Signup {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Signup")
            .field("server", &self.cfg.server.as_str())
            .finish_non_exhaustive()
    }
}

fn now_s() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

fn hex(bytes: &[u8]) -> String {
    crate::state::hex(bytes)
}

fn unhex16(s: &str) -> Option<[u8; 16]> {
    if s.len() != 32 {
        return None;
    }
    let mut out = [0u8; 16];
    for (i, b) in out.iter_mut().enumerate() {
        *b = u8::from_str_radix(s.get(i * 2..i * 2 + 2)?, 16).ok()?;
    }
    Some(out)
}

/// routes.ts `getInvite`: `[.., community_id|null, expires, community_name|null]`; the community id.
fn invite_community(raw: &[u8]) -> Result<Option<[u8; 16]>, ()> {
    let Ok(Value::Array(a)) = Value::decode(raw) else {
        return Err(());
    };
    match a.get(1) {
        Some(Value::Null) => Ok(None),
        Some(v) => v
            .as_bytes()
            .and_then(|b| <[u8; 16]>::try_from(b).ok())
            .map(Some)
            .ok_or(()),
        None => Err(()),
    }
}

impl Signup {
    /// A sign-up against `cfg`'s instance; nothing is sent yet.
    pub fn new(cfg: InstanceConfig, kc: Arc<dyn Keychain>) -> Result<Self, SignupError> {
        let auth = AuthToken::anonymous();
        let http =
            ReqwestHttp::new(cfg.server.clone(), auth.clone()).map_err(|_| SignupError::Network)?;
        Ok(Self {
            cfg,
            kc,
            auth,
            http,
            core: None,
            instance: None,
            answered: false,
            #[cfg(feature = "testing")]
            fault_account_decode: false,
        })
    }

    /// The instance this sign-up is for.
    pub fn config(&self) -> &InstanceConfig {
        &self.cfg
    }

    fn core(&mut self) -> Result<Arc<Mutex<ClientCore>>, SignupError> {
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
    ) -> Result<T, SignupError> {
        let core = self.core()?;
        let mut guard = core
            .lock()
            .map_err(|_| SignupError::Store("the store's lock is poisoned".into()))?;
        f(&mut guard).map_err(|e| core_error(&e))
    }

    fn identity(&mut self) -> Result<Identity, SignupError> {
        let raw = self.with_core(|c| c.identity())?;
        Identity::decode(&raw).ok_or_else(|| SignupError::Other("E_CORE_DECODE identity".into()))
    }

    /// `GET /v1/instance`: its name, whether it takes new accounts and how. Read once, then kept.
    pub async fn instance(&mut self) -> Result<InstanceInfo, SignupError> {
        if let Some(info) = &self.instance {
            return Ok(info.clone());
        }
        let Raw(doc) = self
            .http
            .call::<r::GetInstance>(&[], NoBody)
            .await
            .map_err(|e| refusal(&e, false))?;
        let info = InstanceInfo::decode(&doc)
            .ok_or_else(|| SignupError::Other("E_DECODE instance".into()))?;
        self.instance = Some(info.clone());
        Ok(info)
    }

    /// signup.ts `begin` (the worker's `signupBegin`): the device's keys, and the recovery key to show. Keys an earlier
    /// attempt left pending (never registered) are reset first, so a key that was shown is never shown again.
    pub async fn begin(&mut self) -> Result<RecoveryKey, SignupError> {
        let instance = self.instance().await?;
        if self.identity()?.phase == 1 {
            self.reset()?;
        }
        let key = self.with_core(|c| c.signup_begin(&instance.id).map(Zeroizing::new))?;
        RecoveryKey::new(key)
    }

    /// The worker's `signupReset`: the pending keys (and with them the shown recovery key) are forgotten. Refused once
    /// the account is registered (the core keeps what the instance knows).
    /// Refused ([`SignupError::Stalled`]) once an account POST was answered on this sign-up: the account may exist,
    /// and its keys are the only way into it. The kept join goes with the keys.
    pub fn reset(&mut self) -> Result<(), SignupError> {
        if self.answered {
            return Err(SignupError::Stalled);
        }
        if self.identity()?.phase != 1 {
            return Ok(());
        }
        self.with_core(|c| {
            c.signup_reset()?;
            c.setting_delete(PENDING_JOIN_KEY)
        })
    }

    /// The worker's `signupSubmit` and signup.ts `submit`: the invite is read first (a refused one is
    /// [`SignupError::InviteInvalid`] before anything is created), then the account is created with the keys of
    /// [`Signup::begin`], the device list signed and published, and the invite's server kept for the join after the
    /// factor. `invite` is a code (already extracted from a link); `password` is `None` for an instance with no
    /// password sign-up. There is no display name at sign-up (Jonas, task 3b).
    pub async fn submit(
        &mut self,
        invite: &str,
        username: &str,
        password: Option<&str>,
    ) -> Result<Submitted, SignupError> {
        let invite = invite.trim();
        let username = username_of(username);
        let mut community = None;
        if !invite.is_empty() {
            match self.http.call::<r::GetInvite>(&[invite], NoBody).await {
                Ok(Raw(raw)) => community = invite_community(&raw).ok().flatten(),
                Err(HttpError::Status { code: Some(c), .. }) if c == "E_INVITE_INVALID" => {
                    return Err(SignupError::InviteInvalid);
                }
                // Any other failure of this read: the instance stays the authority at registration.
                Err(_) => {}
            }
        }
        if self.answered {
            // A POST was answered on this sign-up: the account may exist, so only `resume` goes on.
            return Err(SignupError::Stalled);
        }
        let body = self.with_core(|c| {
            c.signup_request(invite, &username, "", password)
                .map(Zeroizing::new)
        })?;
        // Kept before the POST, so an account whose later steps stall still joins after its factor; a refused POST
        // forgets it with the keys (`reset`).
        self.keep_pending_join(community, invite)?;
        let answer = self
            .http
            .call::<PostAccountSecret>(&[], Secret(body))
            .await
            .map_err(|e| refusal(&e, true))?;
        // Answered: from here on every failure is Stalled (resume only), and `reset` is refused.
        self.answered = true;
        #[cfg(feature = "testing")]
        let answer = if std::mem::take(&mut self.fault_account_decode) {
            Raw(b"\xff".to_vec())
        } else {
            answer
        };
        let finished = async {
            let (user_id, token, expires) =
                decode_account(&answer.0).ok_or(SignupError::Stalled)?;
            self.with_core(|c| c.session_store(&token, expires, expires))?;
            self.auth.set(token.as_str());
            drop(token);
            let signer = self.authenticated_signer().await?;
            let name = username.clone();
            self.with_core(|c| c.signup_complete(&user_id, &name, now_s(), &signer))?;
            self.publish_device_list().await
        }
        .await;
        finished.map_err(|_| SignupError::Stalled)?;
        Ok(Submitted {
            username,
            pending_join: community.map(CommunityId),
        })
    }

    /// Test fault (feature `testing`): the next account POST's answer reads as undecodable, as a garbled answer would.
    #[cfg(feature = "testing")]
    pub fn fail_next_account_decode(&mut self) {
        self.fault_account_decode = true;
    }

    /// signup.ts `resume`: an account registered but not finished (the answer was lost, the app closed) is finished
    /// with the stored session; keys that never registered are dropped; a published account is [`Resumed::Done`].
    pub async fn resume(&mut self) -> Result<Resumed, SignupError> {
        let id = self.identity()?;
        match id.phase {
            0 => return Ok(Resumed::Fresh),
            2 if id.list_published => {
                return Ok(Resumed::Done(Submitted {
                    username: id.username,
                    pending_join: self.pending_join()?.map(|(c, _)| c),
                }));
            }
            2 => {}
            1 => {
                let Some(token) = self.stored_session()? else {
                    if self.answered {
                        // Answered but the answer unreadable: the keys stay; nothing here can finish it.
                        return Err(SignupError::Stalled);
                    }
                    self.reset()?;
                    return Ok(Resumed::Fresh);
                };
                self.auth.set(token.as_str());
                let me = match self.http.call::<r::GetAccountMe>(&[], NoBody).await {
                    Ok(Raw(me)) => me,
                    Err(HttpError::Status { status: 401, .. }) => return Ok(Resumed::Revoked),
                    Err(e) => return Err(refusal(&e, true)),
                };
                let (user_id, username) =
                    decode_me(&me).ok_or_else(|| SignupError::Other("E_DECODE account".into()))?;
                let signer = self.authenticated_signer().await?;
                self.with_core(|c| c.signup_complete(&user_id, &username, now_s(), &signer))?;
            }
            _ => return Ok(Resumed::Revoked),
        }
        if !self.auth.is_set() {
            let Some(token) = self.stored_session()? else {
                return Ok(Resumed::Revoked);
            };
            self.auth.set(token.as_str());
        }
        self.publish_device_list().await?;
        let id = self.identity()?;
        Ok(Resumed::Done(Submitted {
            username: id.username,
            pending_join: self.pending_join()?.map(|(c, _)| c),
        }))
    }

    fn stored_session(&mut self) -> Result<Option<Zeroizing<String>>, SignupError> {
        let raw = Zeroizing::new(self.with_core(|c| c.session())?);
        let Ok(Value::Array(a)) = Value::decode(&raw) else {
            return Ok(None);
        };
        Ok(a.first()
            .and_then(Value::as_text)
            .map(|t| Zeroizing::new(t.to_owned())))
    }

    /// signup.ts `authenticatedInstanceSigner`: the instance read again on the new session must be the one the keys
    /// were made for; its external-sender key is pinned by `signup_complete`.
    async fn authenticated_signer(&mut self) -> Result<[u8; 32], SignupError> {
        let Raw(doc) = self
            .http
            .call::<r::GetInstance>(&[], NoBody)
            .await
            .map_err(|e| refusal(&e, true))?;
        let read = InstanceInfo::decode(&doc)
            .ok_or_else(|| SignupError::Other("E_DECODE instance".into()))?;
        let identity = self.identity()?;
        let made_for = match identity.instance_id {
            Some(id) => Some(id),
            None => self.instance.as_ref().map(|i| i.id),
        };
        if made_for != Some(read.id) {
            return Err(SignupError::Other("E_CORE_STATE".into()));
        }
        Ok(read.external_sender)
    }

    /// signup.ts `publishDeviceList`: a 409 whose served list already names this device is adopted as published.
    async fn publish_device_list(&mut self) -> Result<(), SignupError> {
        let id = self.identity()?;
        if id.list_published {
            return Ok(());
        }
        let user = id
            .user_id
            .ok_or_else(|| SignupError::Other("E_CORE_NO_IDENTITY".into()))?;
        let body = self.with_core(|c| c.device_list_body())?;
        let candidate = list_version(&body);
        match self
            .http
            .call::<r::PutDeviceList>(&[&hex(&user)], Raw(body))
            .await
        {
            Ok(()) => {}
            Err(HttpError::Status { status: 409, .. }) => {
                // signup.ts: a 409 whose served list is already at this list's version is the PUT applied with its
                // answer lost (a new account has no other device that could have written it).
                let served = self
                    .http
                    .call::<r::GetDeviceList>(&[&hex(&user)], NoBody)
                    .await
                    .map_err(|e| refusal(&e, true))?;
                if served.status == 404
                    || candidate.is_none()
                    || list_version(&served.body) < candidate
                {
                    return Err(SignupError::Other("E_LIST_RACE".into()));
                }
            }
            Err(e) => return Err(refusal(&e, true)),
        }
        self.with_core(|c| c.device_list_published())
    }

    fn keep_pending_join(
        &mut self,
        community: Option<[u8; 16]>,
        invite: &str,
    ) -> Result<(), SignupError> {
        match community {
            Some(c) if !invite.is_empty() => {
                let row =
                    serde_json::json!({ "communityId": hex(&c), "invite": invite }).to_string();
                self.with_core(|core| core.setting_put(PENDING_JOIN_KEY, &row))
            }
            _ => self.with_core(|core| core.setting_delete(PENDING_JOIN_KEY)),
        }
    }

    fn pending_join(&mut self) -> Result<Option<(CommunityId, String)>, SignupError> {
        let raw = self.with_core(|c| c.settings())?;
        Ok(stored_pending_join(&raw))
    }
}

/// The version of a device-list PUT body or a served head: element 0 of the array.
fn list_version(raw: &[u8]) -> Option<u64> {
    match Value::decode(raw).ok()? {
        Value::Array(a) => a.first()?.as_uint(),
        _ => None,
    }
}

/// routes.ts `postAccount`: `[user_id, device_id, token, expires, ...]`.
fn decode_account(raw: &[u8]) -> Option<([u8; 16], Zeroizing<String>, u64)> {
    let Value::Array(a) = Value::decode(raw).ok()? else {
        return None;
    };
    if a.len() < 4 {
        return None;
    }
    let user = <[u8; 16]>::try_from(a[0].as_bytes()?).ok()?;
    let token = Zeroizing::new(a[2].as_text()?.to_owned());
    let expires = a[3].as_uint()?;
    Some((user, token, expires))
}

/// routes.ts `getAccountMe`: `[user_id, username, ...]`.
fn decode_me(raw: &[u8]) -> Option<([u8; 16], String)> {
    let Value::Array(a) = Value::decode(raw).ok()? else {
        return None;
    };
    let user = <[u8; 16]>::try_from(a.first()?.as_bytes()?).ok()?;
    let username = a.get(1)?.as_text()?.to_owned();
    Some((user, username))
}

/// The worker's `storedPendingJoin`: the stored join, or `None` when there is none or the row is not one this module
/// wrote. `settings` is the core's `[[k, v], ...]`.
fn stored_pending_join(settings: &[u8]) -> Option<(CommunityId, String)> {
    let Value::Array(rows) = Value::decode(settings).ok()? else {
        return None;
    };
    let v = rows.iter().find_map(|row| match row.as_array()? {
        [k, v] if k.as_text() == Some(PENDING_JOIN_KEY) => v.as_text(),
        _ => None,
    })?;
    let json: serde_json::Value = serde_json::from_str(v).ok()?;
    let community = unhex16(json.get("communityId")?.as_str()?)?;
    let invite = json.get("invite")?.as_str()?.to_owned();
    (!invite.is_empty()).then_some((CommunityId(community), invite))
}

// ---- the running client's flows ----

/// Why a create or join failed, by the instance's (or this client's) code: the screens word it by code only.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
#[error("{code} ({status})")]
pub struct FlowError {
    pub code: String,
    pub status: u16,
}

impl FlowError {
    fn local(code: &str) -> Self {
        Self {
            code: code.into(),
            status: 0,
        }
    }
}

impl From<ApiError> for FlowError {
    fn from(e: ApiError) -> Self {
        Self {
            code: e.code,
            status: e.status,
        }
    }
}

impl From<HttpError> for FlowError {
    fn from(e: HttpError) -> Self {
        match e {
            HttpError::Status { status, code, .. } => Self {
                code: code.unwrap_or_else(|| "E_HTTP".into()),
                status,
            },
            HttpError::Transport(_) => Self::local("E_NETWORK"),
            HttpError::Decode(_) => Self::local("E_DECODE"),
            HttpError::Unanswered { code, .. } => Self::local(&code),
        }
    }
}

impl From<ClientError> for FlowError {
    fn from(e: ClientError) -> Self {
        match e {
            ClientError::Network(_) => Self::local("E_NETWORK"),
            ClientError::Stopped => Self::local("E_STOPPED"),
            ClientError::Sync(code) => Self::local(&code),
            _ => Self::local("E_CLIENT"),
        }
    }
}

/// An invite made by [`Onboarding::create_invite`] (routes.ts `createInvite`): the instance's link, its code and its
/// expiry (unix seconds, 0 for never).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MadeInvite {
    pub url: String,
    pub code: String,
    pub expires: u64,
}

/// What a sign-up's join after the factor came to (Onboarding.tsx `SignupResult`): the server joined, or why the join
/// was refused (a refused join is a result, never a failed sign-up: ruling 34).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct SignupResult {
    pub community: Option<CommunityId>,
    pub join_error: Option<FlowError>,
    /// The invite code the join was tried with: a refused join is offered again in the join dialog (App.tsx).
    pub invite: Option<String>,
}

/// The onboarding area's handle on a running client: create and join. Cheap to clone, no I/O on construction. A
/// created or joined server reaches the state with the client's next structure read ([`Client::refresh`], which the
/// screens call after each of these).
#[derive(Clone)]
pub struct Onboarding {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Onboarding`] handle.
    pub fn onboarding(&self) -> Onboarding {
        Onboarding {
            inner: self.inner().clone(),
        }
    }
}

impl Onboarding {
    fn run<T: Send + 'static>(
        &self,
        f: impl std::future::Future<Output = Result<T, FlowError>> + Send + 'static,
    ) -> tokio::sync::oneshot::Receiver<Result<Result<T, FlowError>, ClientError>> {
        self.inner.spawn(async move { Ok(f.await) })
    }

    /// The worker's `joinCommunity`: the invite (a code or a link) is read, then its server joined.
    /// `E_INVITE_NOT_COMMUNITY` for an instance invite.
    pub async fn join(&self, invite: &str) -> Result<CommunityId, FlowError> {
        let code = extract_invite_code(invite);
        if code.is_empty() {
            return Err(FlowError::local("E_BAD_INPUT"));
        }
        let inner = self.inner.clone();
        let rx = self.run(async move {
            let api = inner.api().clone();
            let Raw(raw) = api
                .http_call::<r::GetInvite>(&[&code], NoBody, true)
                .await?;
            let community = invite_community(&raw)
                .map_err(|()| FlowError::local("E_DECODE"))?
                .ok_or_else(|| FlowError::local("E_INVITE_NOT_COMMUNITY"))?;
            join_path(&api, community, &code).await?;
            Ok(CommunityId(community))
        });
        flatten(rx.await)
    }

    /// The worker's `factorSetupDone` join: the server a sign-up kept (in the store, so a restart at the factor step
    /// still joins it), else none. Spent once tried: a refused join is the result, never tried again. Call it once the
    /// account is ready (its factor in place).
    pub async fn finish_signup(&self) -> SignupResult {
        let inner = self.inner.clone();
        let rx = self.run(async move {
            let api = inner.api().clone();
            let core = inner.engine().core().clone();
            let settings = crate::sync::api::try_core(&core, |c| c.settings()).await;
            let pending = settings
                .ok()
                .and_then(Result::ok)
                .as_deref()
                .and_then(stored_pending_join);
            let _ = crate::sync::api::try_core(&core, |c| c.setting_delete(PENDING_JOIN_KEY)).await;
            let Some((community, invite)) = pending else {
                return Ok(SignupResult::default());
            };
            Ok(match join_path(&api, community.0, &invite).await {
                Ok(()) => SignupResult {
                    community: Some(community),
                    join_error: None,
                    invite: Some(invite),
                },
                Err(e) => SignupResult {
                    community: None,
                    join_error: Some(e),
                    invite: Some(invite),
                },
            })
        });
        match flatten(rx.await) {
            Ok(result) => result,
            Err(e) => SignupResult {
                community: None,
                join_error: Some(e),
                invite: None,
            },
        }
    }

    /// Whether a sign-up's join is kept in the store, waiting for the account's factor ([`Onboarding::finish_signup`]).
    /// The app asks at every start of a client that is not ready yet, so a restart at the factor step still joins.
    ///
    /// The store is read on the client's own runtime, as every other call here: the app awaits this on its UI executor,
    /// which has no Tokio reactor for the core's blocking call.
    pub async fn has_pending_join(&self) -> bool {
        let inner = self.inner.clone();
        let rx = self.run(async move {
            let core = inner.engine().core().clone();
            Ok(crate::sync::api::try_core(&core, |c| c.settings())
                .await
                .ok()
                .and_then(Result::ok)
                .as_deref()
                .and_then(stored_pending_join)
                .is_some())
        });
        flatten(rx.await).unwrap_or(false)
    }

    /// The worker's `createCommunity`: `POST /v1/communities [name, policy "{}"... ]`. The name is checked first
    /// ([`name_problem`]); the instance checks it again.
    pub async fn create_server(&self, name: &str) -> Result<CommunityId, FlowError> {
        let name = name.trim().to_owned();
        if name_problem(&name, SERVER_NAME_MAX_BYTES).is_some() {
            return Err(FlowError::local("E_BAD_INPUT"));
        }
        let inner = self.inner.clone();
        let rx = self.run(async move {
            let mut e = dilla_core::cbor::Encoder::new();
            e.array(4).text(&name).bytes(&[]).uint(0).uint(0);
            let answer = inner
                .api()
                .socket_call::<r::CreateCommunity>(&[], Some(e.into_vec()), None, false)
                .await?;
            let id = first_id(&answer.body).ok_or_else(|| FlowError::local("E_DECODE"))?;
            Ok(CommunityId(id))
        });
        flatten(rx.await)
    }

    /// The worker's `createChannel` (and `createCategory`): a channel of `kind` named `name` (already in its stored form,
    /// [`channel_name`]) at the position after the server's last one, top level or inside the category `parent`. An
    /// announcement channel carries its overwrites from birth: @everyone may not send, the roles in `announce_roles` may
    /// (and so may the creator, unless the owner or an administrator).
    pub async fn create_channel(
        &self,
        community: CommunityId,
        kind: NewChannelKind,
        name: &str,
        parent: Option<ChannelId>,
        announce_roles: &[[u8; 16]],
    ) -> Result<ChannelId, FlowError> {
        if name_problem(name, CHANNEL_NAME_MAX_BYTES).is_some()
            || (kind == NewChannelKind::Category && parent.is_some())
        {
            return Err(FlowError::local("E_BAD_INPUT"));
        }
        let name = name.to_owned();
        let roles = announce_roles.to_vec();
        let inner = self.inner.clone();
        let rx = self.run(async move {
            let api = inner.api().clone();
            let position = {
                let state = inner.snapshot();
                next_position(
                    state
                        .channels
                        .iter()
                        .filter(|c| c.community_id == community)
                        .map(|c| u64::from(c.position)),
                )
            };
            let overwrites = if kind == NewChannelKind::Announcement {
                Some(announcement_overwrites(&inner, &api, community, &roles).await?)
            } else {
                None
            };
            let mut e = dilla_core::cbor::Encoder::new();
            e.array(if overwrites.is_some() { 9 } else { 8 })
                .uint(kind.wire())
                .uint(0)
                .uint(0);
            match parent {
                Some(p) => {
                    e.bytes(&p.0);
                }
                None => {
                    e.null();
                }
            }
            e.text(&name).text("").uint(position).uint(0);
            if let Some(list) = &overwrites {
                e.array(list.len());
                for (target_kind, target, allow, deny) in list {
                    e.array(4)
                        .uint(*target_kind)
                        .bytes(target)
                        .uint(*allow)
                        .uint(*deny);
                }
            }
            let answer = api
                .socket_call::<r::CreateChannel>(
                    &[&community.hex()],
                    Some(e.into_vec()),
                    None,
                    false,
                )
                .await?;
            let id = first_id(&answer.body).ok_or_else(|| FlowError::local("E_DECODE"))?;
            Ok(ChannelId(id))
        });
        flatten(rx.await)
    }

    /// Whether this account may create an announcement channel in `community` (`canCreateAnnouncement`: the owner, an
    /// administrator, or someone who may both manage channels and manage roles). Advisory: the instance decides again.
    pub async fn can_announce(&self, community: CommunityId) -> bool {
        let inner = self.inner.clone();
        let rx = self.run(async move {
            if is_owner(&inner, community) {
                return Ok(true);
            }
            let bits = permission_bits(inner.api(), community).await?;
            Ok(bits & PERM_ADMINISTRATOR != 0
                || (bits & PERM_MANAGE_CHANNELS != 0 && bits & PERM_MANAGE_ROLES != 0))
        });
        flatten(rx.await).unwrap_or(false)
    }

    /// The worker's `createInvite`: [`INVITE_MAX_USES`] uses over [`INVITE_TTL_DAYS`] days.
    pub async fn create_invite(&self, community: CommunityId) -> Result<MadeInvite, FlowError> {
        let inner = self.inner.clone();
        let rx = self.run(async move {
            let mut e = dilla_core::cbor::Encoder::new();
            e.array(3)
                .uint(INVITE_MAX_USES)
                .uint(INVITE_TTL_DAYS * 24 * 3600)
                .uint(0);
            let answer = inner
                .api()
                .socket_call::<r::CreateInvite>(
                    &[&community.hex()],
                    Some(e.into_vec()),
                    None,
                    false,
                )
                .await?;
            decode_invite(&answer.body).ok_or_else(|| FlowError::local("E_DECODE"))
        });
        let made = flatten(rx.await)?;
        let (url, code, expires) = (made.url.clone(), made.code.clone(), made.expires);
        self.inner.publish(move |s| {
            s.onboarding.invites.insert(
                community,
                crate::state::onboarding::HeldInvite {
                    url,
                    code,
                    expires,
                    made_at: now_s(),
                },
            );
        });
        Ok(made)
    }
}

fn flatten<T>(
    r: Result<Result<Result<T, FlowError>, ClientError>, tokio::sync::oneshot::error::RecvError>,
) -> Result<T, FlowError> {
    match r {
        Ok(Ok(v)) => v,
        Ok(Err(e)) => Err(e.into()),
        Err(_) => Err(FlowError::local("E_STOPPED")),
    }
}

/// The worker's `joinPath`: `POST /v1/communities/{id}/join [code]`.
async fn join_path(
    api: &crate::sync::api::Api,
    community: [u8; 16],
    invite: &str,
) -> Result<(), FlowError> {
    let mut e = dilla_core::cbor::Encoder::new();
    e.array(1).text(invite);
    api.socket_call::<r::JoinCommunity>(&[&hex(&community)], Some(e.into_vec()), None, false)
        .await?;
    Ok(())
}

fn is_owner(inner: &Inner, community: CommunityId) -> bool {
    let state = inner.snapshot();
    let me = state.account.user_id;
    state
        .communities
        .iter()
        .any(|c| c.id == community && Some(c.owner) == me)
}

async fn permission_bits(
    api: &crate::sync::api::Api,
    community: CommunityId,
) -> Result<u64, FlowError> {
    let answer = api
        .socket_call::<r::GetPermissions>(&[&community.hex()], None, None, true)
        .await?;
    match Value::decode(&answer.body) {
        Ok(Value::Array(a)) => a
            .first()
            .and_then(Value::as_uint)
            .ok_or_else(|| FlowError::local("E_DECODE")),
        _ => Err(FlowError::local("E_DECODE")),
    }
}

/// roles.ts `announcementOverwrites`: `(kind, target, allow, deny)` rows; kind 0 a role, 1 a user.
async fn announcement_overwrites(
    inner: &Inner,
    api: &crate::sync::api::Api,
    community: CommunityId,
    roles: &[[u8; 16]],
) -> Result<Vec<(u64, [u8; 16], u64, u64)>, FlowError> {
    let answer = api
        .socket_call::<r::ListRoles>(&[&community.hex()], None, None, true)
        .await?;
    let everyone = everyone_role(&answer.body).ok_or_else(|| FlowError::local("E_DECODE"))?;
    let mut out = vec![(0, everyone, 0, PERM_SEND_MESSAGES)];
    for role in roles {
        if *role != everyone && !out.iter().any(|(_, t, _, _)| t == role) {
            out.push((0, *role, PERM_SEND_MESSAGES, 0));
        }
    }
    if !is_owner(inner, community) {
        let bits = permission_bits(api, community).await?;
        if bits & PERM_ADMINISTRATOR == 0
            && let Some(me) = inner.snapshot().account.user_id
        {
            out.push((1, me.0, PERM_SEND_MESSAGES, 0));
        }
    }
    check_overwrite_count(out.len())?;
    Ok(out)
}

/// routes.ts `OVERWRITES_PER_CREATE_MAX`: a create carries at most this many overwrites.
pub const OVERWRITES_PER_CREATE_MAX: usize = 100;

fn check_overwrite_count(n: usize) -> Result<(), FlowError> {
    if n > OVERWRITES_PER_CREATE_MAX {
        return Err(FlowError::local("E_BAD_INPUT"));
    }
    Ok(())
}

/// `GET /v1/communities/{id}/roles`: the @everyone role is the one at position 0.
fn everyone_role(raw: &[u8]) -> Option<[u8; 16]> {
    let Value::Array(rows) = Value::decode(raw).ok()? else {
        return None;
    };
    rows.iter().find_map(|row| {
        let a = row.as_array()?;
        (a.get(3)?.as_uint()? == 0)
            .then(|| <[u8; 16]>::try_from(a.first()?.as_bytes()?).ok())
            .flatten()
    })
}

/// `encodeURIComponent` for one path segment, as the transport encodes its arguments.
fn path_segment(arg: &str) -> String {
    let mut out = String::with_capacity(arg.len());
    for b in arg.bytes() {
        if b.is_ascii_alphanumeric() || matches!(b, b'-' | b'.' | b'_' | b'~') {
            out.push(char::from(b));
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

/// A create's answer: its first element, a 16-byte id.
fn first_id(raw: &[u8]) -> Option<[u8; 16]> {
    match Value::decode(raw).ok()? {
        Value::Array(a) => <[u8; 16]>::try_from(a.first()?.as_bytes()?).ok(),
        _ => None,
    }
}

/// routes.ts `createInvite`: `[invite_id, code, url, expires]`; the url must be the landing of the answered code.
fn decode_invite(raw: &[u8]) -> Option<MadeInvite> {
    let Value::Array(a) = Value::decode(raw).ok()? else {
        return None;
    };
    if a.len() < 4 {
        return None;
    }
    let code = a[1].as_text()?.to_owned();
    let url = a[2].as_text()?.to_owned();
    let parsed = url::Url::parse(&url).ok()?;
    let landing = format!("/i/{}", path_segment(&code));
    if !matches!(parsed.scheme(), "http" | "https")
        || parsed.path() != landing
        || parsed.query().is_some()
        || parsed.fragment().is_some()
    {
        return None;
    }
    Some(MadeInvite {
        url,
        code,
        expires: a[3].as_uint()?,
    })
}

/// [`AreaFrames`](crate::sync::route::AreaFrames)-free: the onboarding area handles no gateway op.
#[cfg(test)]
mod tests {
    use super::*;

    /// Integration 3, step 0 item 4: an instance served under a base path mints `<base URL>/i/<code>` (dillad
    /// invites.go `NewInvites`): the link names that instance, base path included, when the base is the instance's own
    /// canonical base path; nothing else is loosened.
    #[test]
    fn a_base_path_invite_names_its_instance() {
        let ok = parse_invite_link("https://host.example/dilla/i/AB_12-x", false).unwrap();
        assert_eq!(ok.instance.as_str(), "https://host.example/dilla/");
        assert_eq!(ok.code, "AB_12-x");
        let deep = parse_invite_link("https://host.example/a/b/i/AB/", false).unwrap();
        assert_eq!(deep.instance.as_str(), "https://host.example/a/b/");
        use InviteLinkError::*;
        for (link, why) in [
            // A base the instance check refuses or rewrites is no instance's own base.
            ("https://host.example/assets/i/AB", Invalid),
            ("https://host.example/welcome/i/AB", Invalid),
            (
                "https://host.example/c/0123456789abcdef0123456789abcdef/i/AB",
                Invalid,
            ),
            ("https://host.example/i/AB/i/CD", Invalid),
            ("https://host.example/dilla/i/AB?", Credentials),
            ("https://host.example/dilla/i/AB#", Credentials),
            ("https://host.example/dil%2fla/i/AB", Invalid),
            ("https://host.example/dilla/i/A%2fB", Invalid),
            ("http://host.example/dilla/i/AB", Insecure),
            ("https://host.example/dilla/x/AB", Invalid),
        ] {
            assert_eq!(parse_invite_link(link, false), Err(why), "{link}");
        }
    }

    #[test]
    fn invite_links_follow_canonical_instance() {
        let ok = parse_invite_link(" https://dilla.example/i/AB_12-x ", false).unwrap();
        assert_eq!(ok.instance.as_str(), "https://dilla.example/");
        assert_eq!(ok.code, "AB_12-x");
        assert_eq!(
            parse_invite_link("https://dilla.example:8443/i/AB/", false)
                .unwrap()
                .instance
                .as_str(),
            "https://dilla.example:8443/"
        );
        use InviteLinkError::*;
        for (link, why) in [
            ("http://dilla.example/i/AB", Insecure),
            ("http://127.0.0.1:8453/i/AB", Insecure),
            ("https://evil@dilla.example/i/AB", Credentials),
            ("https://user:pw@dilla.example/i/AB", Credentials),
            ("https://dilla.example/i/AB?x=1", Credentials),
            ("https://dilla.example/i/AB#x", Credentials),
            ("https://dilla.example/i/A%2fB", Invalid),
            ("https://dilla.example/%2e%2e/i/AB", Invalid),
            ("https://dilla.example/i/A B", Invalid),
            ("https://dilla.example./i/AB", Invalid),
            ("https://dilla.example/welcome", Invalid),
            ("https://dilla.example/i/", Invalid),
            ("https://dilla.example/i/a.b", Invalid),
            ("AB12", Invalid),
            ("javascript:alert(1)", Invalid),
        ] {
            assert_eq!(parse_invite_link(link, false), Err(why), "{link}");
        }
        let long = format!("https://dilla.example/i/{}", "a".repeat(129));
        assert_eq!(parse_invite_link(&long, false), Err(Invalid));
        // Loopback http only when allowed (debug builds).
        for host in ["127.0.0.1:8453", "[::1]:8453", "localhost"] {
            assert!(
                parse_invite_link(&format!("http://{host}/i/AB"), true).is_ok(),
                "{host}"
            );
        }
        assert_eq!(
            parse_invite_link("http://10.0.0.1/i/AB", true),
            Err(Insecure)
        );
    }

    fn scratch(tag: &str) -> std::path::PathBuf {
        let dir = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join(format!(
            "../../../target/t25-unit-{tag}-{}",
            std::process::id()
        ));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[cfg(unix)]
    #[test]
    fn the_key_file_is_private_and_never_follows_a_link() {
        use std::os::unix::fs::{MetadataExt, PermissionsExt};
        let dir = scratch("keyfile");
        // An existing world-readable file ends up 0600 with only the key.
        let path = dir.join("key.txt");
        std::fs::write(&path, b"old").unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
        write_key_file(&path, b"KEY").unwrap();
        assert_eq!(std::fs::read(&path).unwrap(), b"KEY");
        assert_eq!(std::fs::metadata(&path).unwrap().mode() & 0o777, 0o600);
        // A symlink at the path is replaced, and its target is untouched.
        let target = dir.join("target.txt");
        std::fs::write(&target, b"target").unwrap();
        let link = dir.join("link.txt");
        std::os::unix::fs::symlink(&target, &link).unwrap();
        write_key_file(&link, b"KEY").unwrap();
        assert_eq!(std::fs::read(&target).unwrap(), b"target");
        assert!(
            !std::fs::symlink_metadata(&link)
                .unwrap()
                .file_type()
                .is_symlink()
        );
        assert_eq!(std::fs::metadata(&link).unwrap().mode() & 0o777, 0o600);
        // No temporary file is left behind.
        let left: Vec<_> = std::fs::read_dir(&dir)
            .unwrap()
            .filter_map(Result::ok)
            .filter(|e| e.file_name().to_string_lossy().ends_with(".tmp"))
            .collect();
        assert!(left.is_empty());
        // A failure (no such directory) is an error, not silence.
        assert!(write_key_file(&dir.join("missing/key.txt"), b"KEY").is_err());
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_create_carries_at_most_100_overwrites() {
        assert!(check_overwrite_count(100).is_ok());
        assert_eq!(check_overwrite_count(101).unwrap_err().code, "E_BAD_INPUT");
    }

    #[test]
    fn usernames_follow_handle_go() {
        for ok in ["ada", "Ada", " a.b_c-d ", "abc", &"a".repeat(32)] {
            assert!(username_ok(ok), "{ok}");
        }
        for bad in ["ab", ".ada", "ada.", "a b", "ådå", &"a".repeat(33), ""] {
            assert!(!username_ok(bad), "{bad}");
        }
        assert_eq!(username_of("  Ada "), "ada");
    }

    #[test]
    fn passwords_count_code_points() {
        assert!(!password_ok("elevenchars"));
        assert!(password_ok("twelve chars"));
        assert!(password_ok("ååååååååååå å"));
        assert!(!password_ok("åååååå"));
    }

    // invite.test.ts
    #[test]
    fn invite_codes_come_out_of_links() {
        assert_eq!(extract_invite_code("  abc123 "), "abc123");
        assert_eq!(extract_invite_code("https://dilla.test/i/Z4RS?x=1"), "Z4RS");
        assert_eq!(extract_invite_code("https://dilla.test/i/a%20b"), "a b");
        assert_eq!(
            extract_invite_code("https://dilla.test/welcome?invite=xyz&a=1"),
            "xyz"
        );
        assert_eq!(
            extract_invite_code("https://dilla.test/welcome?a=1&invite=x+y"),
            "x y"
        );
        assert_eq!(
            extract_invite_code("https://dilla.test/i/%E0%A4%A"),
            "%E0%A4%A"
        );
        assert_eq!(extract_invite_code(""), "");
    }

    // Create.test.ts nameProblem, channelName
    #[test]
    fn names_are_checked_and_slugged() {
        assert_eq!(name_problem("  ", 255), Some(NameProblem::Empty));
        assert_eq!(name_problem(&"x".repeat(256), 255), Some(NameProblem::Long));
        assert_eq!(name_problem(&"å".repeat(50), 100), None);
        assert_eq!(name_problem(&"å".repeat(51), 100), Some(NameProblem::Long));
        assert_eq!(name_problem("a\tb", 255), Some(NameProblem::Control));
        assert_eq!(name_problem(" Midgard ", 255), None);
        assert_eq!(
            channel_name(NewChannelKind::Text, "  Ship  Talk "),
            "ship-talk"
        );
        assert_eq!(
            channel_name(NewChannelKind::Announcement, "News Desk"),
            "news-desk"
        );
        assert_eq!(
            channel_name(NewChannelKind::Voice, "  Late   Night "),
            "Late Night"
        );
        assert_eq!(
            channel_name(NewChannelKind::Category, " Text Channels "),
            "Text Channels"
        );
    }

    #[test]
    fn next_position_skips_the_afk_reservation() {
        assert_eq!(next_position([]), 0);
        assert_eq!(next_position([0, 3, 1]), 4);
        assert_eq!(next_position([2, AFK_BUILTIN_POSITION]), 3);
    }

    // recovery-file.test.ts instanceSlug
    #[test]
    fn instance_slugs() {
        assert_eq!(instance_slug("Midgard Crew"), "midgard-crew");
        assert_eq!(instance_slug("dilla.test"), "dilla.test");
        assert_eq!(instance_slug("..--a//b--.."), "a-b");
        assert_eq!(instance_slug("///"), "dilla");
        assert_eq!(instance_slug(&"a".repeat(80)).len(), 64);
        assert_eq!(instance_slug("\u{202e}evil"), "evil");
    }

    #[test]
    fn recovery_keys_group_and_never_print() {
        let key = RecoveryKey::new(Zeroizing::new("ABCD".repeat(13))).unwrap();
        assert_eq!(key.groups().count(), 13);
        assert!(key.groups().all(|g| g == "ABCD"));
        assert_eq!(format!("{key:?}"), "RecoveryKey(52 characters)");
        assert_eq!(key.hyphenated().matches('-').count(), 12);
        let file = recovery_key_file("k.txt".into(), "Intro", &key);
        assert_eq!(
            file.text.as_str(),
            format!("Intro\n\n{}\n", key.hyphenated().as_str())
        );
        assert!(RecoveryKey::new(Zeroizing::new("ABC".into())).is_err());
    }

    // recovery-key.test.ts
    #[test]
    fn typed_keys_normalise_as_the_core() {
        assert_eq!(
            normalise_recovery_key(" ab-cd\u{2013}ef\u{2014}gh\n").as_str(),
            "ABCDEFGH"
        );
        assert_eq!(normalise_recovery_key("iIlLoO").as_str(), "111100");
        assert_eq!(
            normalise_recovery_key("abcd-efgh").as_str(),
            dilla_core::identity::recovery_key_normalise("abcd-efgh")
        );
    }

    #[test]
    fn refusals_follow_refusal_view() {
        let st = |status, code: &str| HttpError::Status {
            status,
            code: Some(code.into()),
            retry_after_ms: None,
        };
        assert_eq!(
            refusal(&st(404, "E_INVITE_INVALID"), true),
            SignupError::InviteInvalid
        );
        assert_eq!(
            refusal(&st(409, "E_INVALID_REQUEST"), true),
            SignupError::UsernameTaken
        );
        assert_eq!(
            refusal(&st(400, "E_INVALID_REQUEST"), true),
            SignupError::DetailsRefused
        );
        assert_eq!(refusal(&st(403, "E_FORBIDDEN"), true), SignupError::Closed);
        assert!(matches!(
            refusal(&st(429, "E_RATE_LIMITED"), true),
            SignupError::RateLimited { .. }
        ));
        // Task 28 item 22: the refusal's delay comes through.
        assert_eq!(
            refusal(
                &HttpError::Status {
                    status: 429,
                    code: Some("E_RATE_LIMITED".into()),
                    retry_after_ms: Some(2500),
                },
                true
            ),
            SignupError::RateLimited {
                retry_after_ms: Some(2500)
            }
        );
        assert_eq!(refusal(&st(503, "E_HTTP"), true), SignupError::Stalled);
        assert_eq!(
            refusal(&HttpError::Transport("x".into()), true),
            SignupError::Stalled
        );
        assert_eq!(
            refusal(&HttpError::Transport("x".into()), false),
            SignupError::Network
        );
        assert_eq!(
            refusal(&st(418, "E_TEAPOT"), true),
            SignupError::Other("E_TEAPOT".into())
        );
    }

    #[test]
    fn pending_joins_round_trip_through_settings() {
        let row = serde_json::json!({ "communityId": "00".repeat(15) + "0a", "invite": "abc" })
            .to_string();
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(2);
        e.array(2).text("other").text("x");
        e.array(2).text(PENDING_JOIN_KEY).text(&row);
        let mut id = [0u8; 16];
        id[15] = 10;
        assert_eq!(
            stored_pending_join(&e.into_vec()),
            Some((CommunityId(id), "abc".to_owned()))
        );
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(1)
            .array(2)
            .text(PENDING_JOIN_KEY)
            .text("{\"communityId\":\"zz\"}");
        assert_eq!(stored_pending_join(&e.into_vec()), None);
    }

    #[test]
    fn instance_documents_decode() {
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(11)
            .uint(1)
            .uint(1)
            .uint(1)
            .bytes(&[7; 16])
            .uint(3)
            .text("dilla.test")
            .uint(0);
        e.array(2).uint(0).uint(1);
        e.uint(1).null().bytes(&[9; 32]);
        let info = InstanceInfo::decode(&e.into_vec()).unwrap();
        assert_eq!(info.name, "dilla.test");
        assert_eq!(info.registration_mode, RegistrationMode::InviteOnly);
        assert!(info.password_signup && info.totp && !info.passkeys);
        assert_eq!(info.external_sender, [9; 32]);
    }

    #[test]
    fn invite_answers_must_land_on_their_code() {
        let enc = |url: &str| {
            let mut e = dilla_core::cbor::Encoder::new();
            e.array(4).bytes(&[1; 16]).text("AB12").text(url).uint(99);
            e.into_vec()
        };
        assert_eq!(
            decode_invite(&enc("https://dilla.test/i/AB12")),
            Some(MadeInvite {
                url: "https://dilla.test/i/AB12".into(),
                code: "AB12".into(),
                expires: 99
            })
        );
        assert_eq!(decode_invite(&enc("https://dilla.test/i/OTHER")), None);
        assert_eq!(decode_invite(&enc("javascript:alert(1)")), None);
        assert_eq!(decode_invite(&enc("https://dilla.test/i/AB12#x")), None);
    }
}
