//! One [`Route`] type per HTTP route of `packages/client-core/src/http/routes.ts` (and `negotiate-session.ts`), the
//! route table they are listed in, and the wire types they carry.
//!
//! The table is declared once, by the [`routes!`] macro below, which writes each route's type, its `Route` impl and its
//! [`TABLE`] row together, so a route cannot be in one and not the other. `tests/http.rs` (`route_table_matches_ts`) holds
//! the table to the TypeScript source. The type names are the TypeScript method names in `UpperCamelCase`
//! (`getInstance` is [`GetInstance`]); [`Entry::name`] is the snake_case spelling.
//!
//! Bodies are carried as the TypeScript client carries them. A request body is [`Raw`] (the bytes of a body the caller
//! built with `dilla_core::cbor`, or an octet stream) or [`NoBody`]; an answer is [`Raw`] (the body, for the caller's
//! strict decoder), [`Reply`] (the status too, for a route whose status means something), `()` (not read), or, for the
//! routes sign-in and the gateway ticket need, a typed answer decoded strictly with `dilla_core::cbor`.

use dilla_core::cbor::{CborError, Decoder, Encoder, decode_strict};
use serde::de::{self, Deserializer, Visitor};
use serde::{Deserialize, Serialize, Serializer};

use crate::seams::Route;

/// How a route's request body is encoded.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum BodyEncoding {
    /// No body.
    None,
    /// `application/cbor`: deterministic CBOR (`dilla_core::cbor`).
    Cbor,
    /// `application/octet-stream`.
    Octet,
}

/// What the caller reads of an answer.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Answer {
    /// Nothing: the answer is not read.
    None,
    /// The body.
    Body,
    /// The status and the body.
    Status,
}

/// Where the instance serves a route.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Transport {
    /// Over HTTP.
    Http,
    /// Only as a gateway request frame: the instance does not serve it over HTTP, so [`super::ReqwestHttp`] refuses it.
    Socket,
}

/// One row of [`TABLE`].
#[derive(Clone, Debug)]
pub struct Entry {
    /// The TypeScript `Routes` method, snake_case.
    pub name: &'static str,
    pub method: http::Method,
    /// The path template: every `{name}` is one `path_args` element, in order; a trailing `?k={v}` group is left out when
    /// its arguments are.
    pub path: &'static str,
    pub request: BodyEncoding,
    pub answer: Answer,
    /// Whether the session's bearer is sent.
    pub auth: bool,
    /// The statuses the route accepts; empty is the default 200, 201 and 204.
    pub ok: &'static [u16],
    pub transport: Transport,
}

impl Entry {
    /// Whether `status` is a success for this route.
    pub fn accepts(&self, status: u16) -> bool {
        if self.ok.is_empty() {
            matches!(status, 200 | 201 | 204)
        } else {
            self.ok.contains(&status)
        }
    }
}

/// The route of `method` and `path` (a [`Route::PATH`] template), if it is in the table.
pub fn lookup(method: &http::Method, path: &str) -> Option<&'static Entry> {
    TABLE.iter().find(|e| e.method == *method && e.path == path)
}

// ---- wire types ----

/// Bytes on the wire: a request body already built (CBOR from `dilla_core::cbor`, or an octet stream) or an answer body
/// handed on unread.
#[derive(Clone, Debug, PartialEq, Eq, Default)]
pub struct Raw(pub Vec<u8>);

impl Raw {
    /// `[]`: the body of the routes that take an empty list.
    pub fn empty_list() -> Self {
        let mut e = Encoder::new();
        e.array(0);
        Self(e.into_vec())
    }
}

impl Serialize for Raw {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_bytes(&self.0)
    }
}

struct BytesVisitor;

impl<'de> Visitor<'de> for BytesVisitor {
    type Value = Vec<u8>;
    fn expecting(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("a byte string")
    }
    fn visit_bytes<E: de::Error>(self, v: &[u8]) -> Result<Vec<u8>, E> {
        Ok(v.to_vec())
    }
    fn visit_byte_buf<E: de::Error>(self, v: Vec<u8>) -> Result<Vec<u8>, E> {
        Ok(v)
    }
}

impl<'de> Deserialize<'de> for Raw {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        d.deserialize_byte_buf(BytesVisitor).map(Raw)
    }
}

/// A request without a body.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub struct NoBody;

impl Serialize for NoBody {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_unit()
    }
}

/// An answer whose status is part of the result (201 against 200, 404 against 200), with its body.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Reply {
    pub status: u16,
    pub body: Vec<u8>,
}

impl<'de> Deserialize<'de> for Reply {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct V;
        impl<'de> Visitor<'de> for V {
            type Value = Reply;
            fn expecting(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                f.write_str("a status and a body")
            }
            fn visit_seq<A: de::SeqAccess<'de>>(self, mut seq: A) -> Result<Reply, A::Error> {
                let status = seq
                    .next_element::<u16>()?
                    .ok_or_else(|| de::Error::invalid_length(0, &self))?;
                let Raw(body) = seq
                    .next_element::<Raw>()?
                    .ok_or_else(|| de::Error::invalid_length(1, &self))?;
                Ok(Reply { status, body })
            }
        }
        d.deserialize_seq(V)
    }
}

/// An answer decoded strictly from its body.
trait FromBody: Sized {
    fn from_body(body: &[u8]) -> Result<Self, CborError>;
}

fn deserialize_body<'de, T: FromBody, D: Deserializer<'de>>(d: D) -> Result<T, D::Error> {
    let Raw(body) = Raw::deserialize(d)?;
    T::from_body(&body).map_err(de::Error::custom)
}

macro_rules! typed_answer {
    ($($ty:ty),*) => {$(
        impl<'de> Deserialize<'de> for $ty {
            fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
                deserialize_body(d)
            }
        }
    )*};
}

/// `[ ... ]` of at least `min` elements, as the TypeScript `atLeast` reads one: `read` takes the elements it wants and
/// says how many it took; the rest are skipped.
fn at_least<T>(
    body: &[u8],
    min: usize,
    read: impl FnOnce(&mut Decoder<'_>, usize) -> Result<(T, usize), CborError>,
) -> Result<T, CborError> {
    decode_strict(body, |d| {
        let len = d.array_len()?;
        if len < min {
            return Err(CborError::WrongArrayLen {
                expected: min,
                actual: len,
            });
        }
        let (value, taken) = read(d, len)?;
        for _ in taken..len {
            d.skip()?;
        }
        Ok(value)
    })
}

fn one_of(v: u64, allowed: &[u64]) -> Result<u8, CborError> {
    if allowed.contains(&v) {
        Ok(v as u8)
    } else {
        Err(CborError::IntegerOverflow)
    }
}

/// What `POST /v1/auth/password/login` answers (protocol/09 § Second factors): `[assertion, needs, passkey?]`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PasswordLoginAnswer {
    pub assertion: String,
    /// 0 nothing owed, 1 a TOTP or backup code, 2 a passkey login or a backup code.
    pub needs: u8,
    /// The account holds a passkey for this instance (only sent when a factor is owed).
    pub passkey: bool,
}

impl FromBody for PasswordLoginAnswer {
    fn from_body(body: &[u8]) -> Result<Self, CborError> {
        at_least(body, 2, |d, len| {
            let assertion = d.text()?.to_owned();
            let needs = one_of(d.uint()?, &[0, 1, 2])?;
            let mut passkey = false;
            if len > 2 {
                let flag = d.uint()?;
                if flag > 1 {
                    return Err(CborError::IntegerOverflow);
                }
                passkey = needs != 0 && flag == 1;
            }
            Ok((
                PasswordLoginAnswer {
                    assertion,
                    needs,
                    passkey,
                },
                if len > 2 { 3 } else { 2 },
            ))
        })
    }
}

/// What `POST /v1/auth/totp/verify` answers: `[assertion]`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TotpVerifyAnswer {
    pub assertion: String,
}

impl FromBody for TotpVerifyAnswer {
    fn from_body(body: &[u8]) -> Result<Self, CborError> {
        at_least(body, 1, |d, _| {
            let assertion = d.text()?.to_owned();
            Ok((TotpVerifyAnswer { assertion }, 1))
        })
    }
}

/// What `POST /v1/devices/{id}/sessions/challenge` answers: `[nonce, expires]`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ChallengeAnswer {
    pub nonce: [u8; 32],
    pub expires: u64,
}

impl FromBody for ChallengeAnswer {
    fn from_body(body: &[u8]) -> Result<Self, CborError> {
        at_least(body, 2, |d, _| {
            let nonce = d.bytes_exact::<32>()?;
            let expires = d.uint()?;
            Ok((ChallengeAnswer { nonce, expires }, 2))
        })
    }
}

/// What `POST /v1/devices/{id}/sessions` answers: `[token, scope, user_id, device_id, expires, idle_expires, generation]`.
#[derive(Clone, PartialEq, Eq)]
pub struct SessionAnswer {
    pub token: String,
    /// 0, 1 or 2 (protocol/09): 1 reaches only the enrolment routes.
    pub scope: u8,
    pub user_id: [u8; 16],
    pub device_id: [u8; 16],
    pub expires: u64,
    pub idle_expires: u64,
    pub generation: u64,
}

impl std::fmt::Debug for SessionAnswer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("SessionAnswer")
            .field("token", &"<redacted>")
            .field("scope", &self.scope)
            .finish_non_exhaustive()
    }
}

impl FromBody for SessionAnswer {
    fn from_body(body: &[u8]) -> Result<Self, CborError> {
        at_least(body, 7, |d, _| {
            let token = d.text()?.to_owned();
            let scope = one_of(d.uint()?, &[0, 1, 2])?;
            let user_id = d.bytes_exact::<16>()?;
            let device_id = d.bytes_exact::<16>()?;
            let (expires, idle_expires, generation) = (d.uint()?, d.uint()?, d.uint()?);
            Ok((
                SessionAnswer {
                    token,
                    scope,
                    user_id,
                    device_id,
                    expires,
                    idle_expires,
                    generation,
                },
                7,
            ))
        })
    }
}

/// What `POST /v1/gateway/ticket` answers: `[ticket, expires]`.
#[derive(Clone, PartialEq, Eq)]
pub struct TicketAnswer {
    pub ticket: String,
    pub expires: u64,
}

impl std::fmt::Debug for TicketAnswer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TicketAnswer")
            .field("ticket", &"<redacted>")
            .field("expires", &self.expires)
            .finish()
    }
}

impl FromBody for TicketAnswer {
    fn from_body(body: &[u8]) -> Result<Self, CborError> {
        at_least(body, 2, |d, _| {
            let ticket = d.text()?.to_owned();
            let expires = d.uint()?;
            Ok((TicketAnswer { ticket, expires }, 2))
        })
    }
}

/// The versions this client offers in `POST /v1/sessions/negotiate`: `[[wire], [e2ee], [media]]`
/// (`WIRE_VERSION` in `negotiate-session.ts`, and 1 and 1).
pub const NEGOTIATE_OFFER: (u64, u64, u64) = (6, 1, 1);

/// The body of `POST /v1/sessions/negotiate`.
pub fn negotiate_body() -> Raw {
    let (wire, e2ee, media) = NEGOTIATE_OFFER;
    let mut e = Encoder::new();
    e.array(3)
        .array(1)
        .uint(wire)
        .array(1)
        .uint(e2ee)
        .array(1)
        .uint(media);
    Raw(e.into_vec())
}

/// What `POST /v1/sessions/negotiate` answers: exactly the three versions offered.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct NegotiatedAnswer {
    pub wire: u64,
    pub e2ee: u64,
    pub media: u64,
}

impl FromBody for NegotiatedAnswer {
    fn from_body(body: &[u8]) -> Result<Self, CborError> {
        let (wire, e2ee, media) = decode_strict(body, |d| {
            d.array(3)?;
            Ok((d.uint()?, d.uint()?, d.uint()?))
        })?;
        // A version the client did not offer is refused, as the TypeScript client does.
        if (wire, e2ee, media) != NEGOTIATE_OFFER {
            return Err(CborError::IntegerOverflow);
        }
        Ok(NegotiatedAnswer { wire, e2ee, media })
    }
}

typed_answer!(
    PasswordLoginAnswer,
    TotpVerifyAnswer,
    ChallengeAnswer,
    SessionAnswer,
    TicketAnswer,
    NegotiatedAnswer
);

/// The body of `POST /v1/auth/password/login`: `[username, password]`.
pub fn password_login_body(username: &str, password: &str) -> Raw {
    let mut e = Encoder::new();
    e.array(2).text(username).text(password);
    Raw(e.into_vec())
}

/// The body of `POST /v1/auth/totp/verify`: `[assertion, code]`.
pub fn totp_verify_body(assertion: &str, code: &str) -> Raw {
    let mut e = Encoder::new();
    e.array(2).text(assertion).text(code);
    Raw(e.into_vec())
}

// ---- the routes ----

macro_rules! routes {
    ($(
        $name:ident { $ts:literal, $method:ident, $path:literal, $enc:ident, $answer:ident, $auth:literal,
            [$($ok:literal),*], $transport:ident, $req:ty => $res:ty }
    )*) => {
        $(
            #[doc = concat!("`", stringify!($method), " ", $path, "` (`", $ts, "`).")]
            #[derive(Clone, Copy, Debug)]
            pub struct $name;

            impl Route for $name {
                const METHOD: http::Method = http::Method::$method;
                const PATH: &'static str = $path;
                type Request = $req;
                type Response = $res;
            }
        )*

        /// Every route of the TypeScript client, in the order of its `name`.
        pub static TABLE: &[Entry] = &[$(
            Entry {
                name: $ts,
                method: <$name as Route>::METHOD,
                path: <$name as Route>::PATH,
                request: BodyEncoding::$enc,
                answer: Answer::$answer,
                auth: $auth,
                ok: &[$($ok),*],
                transport: Transport::$transport,
            },
        )*];
    };
}

routes! {
    AcceptedManualWarning { "accepted_manual_warning", GET, "/v1/bot-runtime/installations/{installation_id}/moderation/warnings/{request_id}", None, Body, true, [], Socket, NoBody => Raw }
    ApplyRolePanelReaction { "apply_role_panel_reaction", POST, "/v1/communities/{community_id}/role-panels/{channel_id}/{message_seq}/reaction", Cbor, None, true, [204], Socket, Raw => () }
    ChangePassword { "change_password", POST, "/v1/auth/password", Cbor, None, true, [204], Socket, Raw => () }
    ConfigureBotInstallation { "configure_bot_installation", PUT, "/v1/communities/{community_id}/bot?v=2", Cbor, Body, true, [], Socket, Raw => Raw }
    ConfigureBotJoinRole { "configure_bot_join_role", PUT, "/v1/communities/{community_id}/bot-join-role", Cbor, Body, true, [], Socket, Raw => Raw }
    ConfigureSelfRoles { "configure_self_roles", PUT, "/v1/communities/{community_id}/self-roles/policy", Cbor, Body, true, [], Socket, Raw => Raw }
    ConfirmBlob { "confirm_blob", POST, "/v1/channels/{channel_id}/blobs/{blob_id}/confirm", None, None, true, [204], Http, NoBody => () }
    CreateBotWebhook { "create_bot_webhook", POST, "/v1/communities/{community_id}/bot/webhooks", Cbor, Body, true, [201], Socket, Raw => Raw }
    CreateChannel { "create_channel", POST, "/v1/communities/{community_id}/channels", Cbor, Body, true, [], Socket, Raw => Raw }
    CreateCommunity { "create_community", POST, "/v1/communities", Cbor, Body, true, [], Socket, Raw => Raw }
    CreateInvite { "create_invite", POST, "/v1/communities/{community_id}/invites", Cbor, Body, true, [], Socket, Raw => Raw }
    CreateRole { "create_role", POST, "/v1/communities/{community_id}/roles", Cbor, Body, true, [], Socket, Raw => Raw }
    DeclineCall { "decline_call", POST, "/v1/calls/{call_id}/decline", Cbor, None, true, [204], Socket, Raw => () }
    DeleteBan { "delete_ban", DELETE, "/v1/communities/{community_id}/bans/{user_id}", None, None, true, [], Socket, NoBody => () }
    DeleteBlob { "delete_blob", DELETE, "/v1/channels/{channel_id}/blobs/{blob_id}", None, None, true, [204, 404], Http, NoBody => () }
    DeleteBotWebhook { "delete_bot_webhook", DELETE, "/v1/communities/{community_id}/bot/webhooks/{webhook_id}", None, None, true, [204], Socket, NoBody => () }
    DeleteChannel { "delete_channel", DELETE, "/v1/channels/{channel_id}", None, None, true, [], Socket, NoBody => () }
    DeleteCommunity { "delete_community", DELETE, "/v1/communities/{community_id}", None, None, true, [], Socket, NoBody => () }
    DeleteDevice { "delete_device", DELETE, "/v1/devices/{device_id}", None, None, true, [204], Socket, NoBody => () }
    DeleteGroupMessage { "delete_group_message", DELETE, "/v1/groups/{group_id}/messages/{seq}", None, Status, true, [204, 404], Socket, NoBody => Reply }
    DeleteMemberTimeout { "delete_member_timeout", DELETE, "/v1/communities/{community_id}/members/{user_id}/timeout", Cbor, None, true, [204], Socket, Raw => () }
    DeleteOverwrite { "delete_overwrite", DELETE, "/v1/channels/{channel_id}/overwrites/{kind}/{target_id}", None, None, true, [], Socket, NoBody => () }
    DeleteRole { "delete_role", DELETE, "/v1/roles/{role_id}", None, None, true, [], Socket, NoBody => () }
    DeleteSessions { "delete_sessions", DELETE, "/v1/devices/{device_id}/sessions", None, None, true, [204], Http, NoBody => () }
    DeleteWelcome { "delete_welcome", DELETE, "/v1/welcomes/{welcome_id}", None, None, true, [204, 404], Socket, NoBody => () }
    DisableBotInstallation { "disable_bot_installation", POST, "/v1/communities/{community_id}/bot/disable", Cbor, Body, true, [], Socket, Raw => Raw }
    EndCallIfAlone { "end_call_if_alone", DELETE, "/v1/calls/{call_id}?if_alone=1", None, None, true, [204], Socket, NoBody => () }
    GetAccountMe { "get_account_me", GET, "/v1/accounts/me", None, Body, true, [], Http, NoBody => Raw }
    GetAfk { "get_afk", GET, "/v1/communities/{community_id}/afk", None, Body, true, [], Socket, NoBody => Raw }
    GetAfkSettings { "get_afk_settings", GET, "/v1/communities/{community_id}/afk/settings", None, Body, true, [], Socket, NoBody => Raw }
    GetBackup { "get_backup", GET, "/v1/backups/{kind}/0", None, Status, true, [200, 404], Http, NoBody => Reply }
    GetBlob { "get_blob", GET, "/v1/channels/{channel_id}/blobs/{blob_id}", None, Status, true, [200, 404], Http, NoBody => Reply }
    GetBotCommandStatus { "get_bot_command_status", GET, "/v1/communities/{community_id}/bot/command-status", None, Body, true, [], Socket, NoBody => Raw }
    GetBotInstallation { "get_bot_installation", GET, "/v1/communities/{community_id}/bot", None, Body, true, [], Socket, NoBody => Raw }
    GetBotJoinRole { "get_bot_join_role", GET, "/v1/communities/{community_id}/bot-join-role", None, Body, true, [], Socket, NoBody => Raw }
    GetChannel { "get_channel", GET, "/v1/channels/{channel_id}", None, Body, true, [], Socket, NoBody => Raw }
    GetChannelCall { "get_channel_call", GET, "/v1/channels/{channel_id}/call", None, Body, true, [], Socket, NoBody => Raw }
    GetCommunityCalls { "get_community_calls", GET, "/v1/communities/{community_id}/calls", None, Body, true, [], Socket, NoBody => Raw }
    GetDeviceList { "get_device_list", GET, "/v1/users/{user_id}/device-list", None, Status, true, [200, 404], Http, NoBody => Reply }
    GetDeviceListHistory { "get_device_list_history", GET, "/v1/users/{user_id}/device-list?after={after}", None, Body, true, [], Http, NoBody => Raw }
    GetFactors { "get_factors", GET, "/v1/auth/factors", None, Body, true, [], Http, NoBody => Raw }
    GetGroupHealState { "get_group_heal_state", GET, "/v1/groups/{group_id}/heal-state", None, Body, true, [], Socket, NoBody => Raw }
    GetGroupInfo { "get_group_info", GET, "/v1/groups/{group_id}/info", None, Body, true, [], Socket, NoBody => Raw }
    GetGroupTree { "get_group_tree", GET, "/v1/groups/{group_id}/tree", None, Body, true, [], Socket, NoBody => Raw }
    GetHandshakes { "get_handshakes", GET, "/v1/groups/{group_id}/handshakes?from={from}&limit={limit}", None, Body, true, [], Socket, NoBody => Raw }
    GetInstance { "get_instance", GET, "/v1/instance", None, Body, true, [], Http, NoBody => Raw }
    GetInvite { "get_invite", GET, "/i/{code}", None, Body, false, [], Http, NoBody => Raw }
    GetKeyPackageCount { "get_key_package_count", GET, "/v1/keypackages", None, Status, true, [200, 404, 405], Http, NoBody => Reply }
    GetLimits { "get_limits", GET, "/v1/instance/limits", None, Body, false, [], Http, NoBody => Raw }
    GetMemberTimeout { "get_member_timeout", GET, "/v1/communities/{community_id}/members/{user_id}/timeout", None, Body, true, [], Socket, NoBody => Raw }
    GetMessages { "get_messages", GET, "/v1/groups/{group_id}/messages?from={from}&limit={limit}", None, Body, true, [], Socket, NoBody => Raw }
    GetModerationRemovals { "get_moderation_removals", GET, "/v1/groups/{group_id}/moderation/removals?after={after}&limit={limit}", None, Body, true, [], Socket, NoBody => Raw }
    GetPermissions { "get_permissions", GET, "/v1/communities/{community_id}/permissions", None, Body, true, [], Socket, NoBody => Raw }
    GetPresenceSnapshot { "get_presence_snapshot", GET, "/v1/communities/{community_id}/presence", None, Body, true, [], Socket, NoBody => Raw }
    GetProposals { "get_proposals", GET, "/v1/groups/{group_id}/proposals", None, Body, true, [], Socket, NoBody => Raw }
    GetWebhookAvatar { "get_webhook_avatar", GET, "/v1/communities/{community_id}/bot/webhook-avatars/{avatar_id}", None, Status, true, [200, 404], Http, NoBody => Reply }
    GetWelcomes { "get_welcomes", GET, "/v1/welcomes?after={after}&limit=64", None, Body, true, [], Socket, NoBody => Raw }
    GifClearKey { "gif_clear_key", DELETE, "/v1/communities/{community_id}/gifs/key", None, None, true, [204], Socket, NoBody => () }
    GifMedia { "gif_media", GET, "/v1/communities/{community_id}/gifs/media/{gif_id}?r={variant}", None, Body, true, [], Http, NoBody => Raw }
    GifSearch { "gif_search", GET, "/v1/communities/{community_id}/gifs/search?q={q}&offset={offset}", None, Body, true, [], Socket, NoBody => Raw }
    GifSetKey { "gif_set_key", PUT, "/v1/communities/{community_id}/gifs/key", Cbor, None, true, [204], Socket, Raw => () }
    GifSetRating { "gif_set_rating", PUT, "/v1/communities/{community_id}/gifs/rating", Cbor, None, true, [204], Socket, Raw => () }
    GifStatus { "gif_status", GET, "/v1/communities/{community_id}/gifs", None, Body, true, [], Socket, NoBody => Raw }
    GrantRole { "grant_role", PUT, "/v1/communities/{community_id}/members/{user_id}/roles/{role_id}", Cbor, None, true, [], Socket, Raw => () }
    JoinCommunity { "join_community", POST, "/v1/communities/{community_id}/join", Cbor, None, true, [], Socket, Raw => () }
    KickMember { "kick_member", DELETE, "/v1/communities/{community_id}/members/{user_id}", None, None, true, [], Socket, NoBody => () }
    LeaveCommunity { "leave_community", POST, "/v1/communities/{community_id}/leave", Cbor, None, true, [], Socket, Raw => () }
    ListBackups { "list_backups", GET, "/v1/backups", None, Body, true, [], Http, NoBody => Raw }
    ListBans { "list_bans", GET, "/v1/communities/{community_id}/bans", None, Body, true, [], Socket, NoBody => Raw }
    ListBotWebhooks { "list_bot_webhooks", GET, "/v1/communities/{community_id}/bot/webhooks", None, Body, true, [], Socket, NoBody => Raw }
    ListChannels { "list_channels", GET, "/v1/communities/{community_id}/channels", None, Body, true, [], Socket, NoBody => Raw }
    ListCommunities { "list_communities", GET, "/v1/communities", None, Body, true, [], Socket, NoBody => Raw }
    ListDevices { "list_devices", GET, "/v1/devices", None, Body, true, [], Socket, NoBody => Raw }
    ListDms { "list_dms", GET, "/v1/dms", None, Body, true, [], Socket, NoBody => Raw }
    ListInvites { "list_invites", GET, "/v1/communities/{community_id}/invites", None, Body, true, [], Socket, NoBody => Raw }
    ListMembers { "list_members", GET, "/v1/communities/{community_id}/members?after={after}", None, Body, true, [], Socket, NoBody => Raw }
    ListOverwrites { "list_overwrites", GET, "/v1/communities/{community_id}/overwrites", None, Body, true, [], Socket, NoBody => Raw }
    ListRoles { "list_roles", GET, "/v1/communities/{community_id}/roles", None, Body, true, [], Socket, NoBody => Raw }
    ListSelfRoles { "list_self_roles", GET, "/v1/communities/{community_id}/self-roles", None, Body, true, [], Socket, NoBody => Raw }
    MarkBotChannel { "mark_bot_channel", PUT, "/v1/communities/{community_id}/channels/{channel_id}/bot-channel", None, None, true, [204], Socket, NoBody => () }
    NegotiateSession { "negotiate_session", POST, "/v1/sessions/negotiate", Cbor, Body, true, [200], Http, Raw => NegotiatedAnswer }
    PasskeyLoginBegin { "passkey_login_begin", POST, "/v1/auth/passkey/login/begin", Cbor, Body, false, [], Http, Raw => Raw }
    PasskeyLoginFinish { "passkey_login_finish", POST, "/v1/auth/passkey/login/finish", Cbor, Body, false, [], Http, Raw => Raw }
    PasskeyRegisterBegin { "passkey_register_begin", POST, "/v1/auth/passkey/register/begin", Cbor, Body, true, [], Http, Raw => Raw }
    PasskeyRegisterFinish { "passkey_register_finish", POST, "/v1/auth/passkey/register/finish", Cbor, Body, true, [], Http, Raw => Raw }
    PasswordLogin { "password_login", POST, "/v1/auth/password/login", Cbor, Body, false, [], Http, Raw => PasswordLoginAnswer }
    PatchBotProfile { "patch_bot_profile", PATCH, "/v1/communities/{community_id}/bot/profile", Cbor, Body, true, [], Socket, Raw => Raw }
    PatchBotWebhook { "patch_bot_webhook", PATCH, "/v1/communities/{community_id}/bot/webhooks/{webhook_id}", Cbor, None, true, [204], Socket, Raw => () }
    PatchChannel { "patch_channel", PATCH, "/v1/channels/{channel_id}", Cbor, None, true, [], Socket, Raw => () }
    PatchCommunity { "patch_community", PATCH, "/v1/communities/{community_id}", Cbor, Body, true, [], Socket, Raw => Raw }
    PatchDevice { "patch_device", PATCH, "/v1/devices/{device_id}", Cbor, None, true, [204], Socket, Raw => () }
    PatchMe { "patch_me", PATCH, "/v1/accounts/me", Cbor, None, true, [204], Socket, Raw => () }
    PatchRole { "patch_role", PATCH, "/v1/roles/{role_id}", Cbor, None, true, [], Socket, Raw => () }
    PostAccount { "post_account", POST, "/v1/accounts", Cbor, Body, false, [], Http, Raw => Raw }
    PostCallRtt { "post_call_rtt", POST, "/v1/calls/{call_id}/rtt", Cbor, None, true, [204], Socket, Raw => () }
    PostCallStats { "post_call_stats", POST, "/v1/calls/{call_id}/stats", Cbor, None, true, [204], Socket, Raw => () }
    PostChallenge { "post_challenge", POST, "/v1/devices/{device_id}/sessions/challenge", Cbor, Body, false, [], Http, Raw => ChallengeAnswer }
    PostCommit { "post_commit", POST, "/v1/groups/{group_id}/commit", Cbor, Body, true, [], Socket, Raw => Raw }
    PostCursor { "post_cursor", POST, "/v1/groups/{group_id}/cursor", Cbor, None, true, [], Socket, Raw => () }
    PostDm { "post_dm", POST, "/v1/dms", Cbor, Status, true, [], Socket, Raw => Reply }
    PostGroup { "post_group", POST, "/v1/groups", Cbor, Body, true, [], Socket, Raw => Raw }
    PostGroupHeal { "post_group_heal", POST, "/v1/groups/{group_id}/heal", Cbor, Body, true, [], Socket, Raw => Raw }
    PostKeyPackages { "post_key_packages", POST, "/v1/keypackages", Cbor, Body, true, [], Http, Raw => Raw }
    PostMessage { "post_message", POST, "/v1/groups/{group_id}/message", Cbor, Body, true, [], Socket, Raw => Raw }
    PostModerationRemoval { "post_moderation_removal", POST, "/v1/groups/{group_id}/moderation/removals", Cbor, None, true, [], Socket, Raw => () }
    PostProposal { "post_proposal", POST, "/v1/groups/{group_id}/proposal", Cbor, Body, true, [], Socket, Raw => Raw }
    PostResync { "post_resync", POST, "/v1/groups/{group_id}/resync", Cbor, Body, true, [], Socket, Raw => Raw }
    PostSession { "post_session", POST, "/v1/devices/{device_id}/sessions", Cbor, Body, false, [], Http, Raw => SessionAnswer }
    PostTicket { "post_ticket", POST, "/v1/gateway/ticket", Cbor, Body, true, [], Http, Raw => TicketAnswer }
    PutAfkAway { "put_afk_away", PUT, "/v1/communities/{community_id}/afk/away", Cbor, None, true, [204], Socket, Raw => () }
    PutBackup { "put_backup", PUT, "/v1/backups/{kind}/0", Cbor, Status, true, [], Http, Raw => Reply }
    PutBan { "put_ban", PUT, "/v1/communities/{community_id}/bans/{user_id}", Cbor, None, true, [], Socket, Raw => () }
    PutBlob { "put_blob", PUT, "/v1/channels/{channel_id}/blobs/{blob_id}", Octet, Status, true, [200, 201], Http, Raw => Reply }
    PutDeviceList { "put_device_list", PUT, "/v1/users/{user_id}/device-list", Cbor, None, true, [], Http, Raw => () }
    PutMemberTimeout { "put_member_timeout", PUT, "/v1/communities/{community_id}/members/{user_id}/timeout", Cbor, Body, true, [], Socket, Raw => Raw }
    PutOverwrite { "put_overwrite", PUT, "/v1/channels/{channel_id}/overwrites/{kind}/{target_id}", Cbor, None, true, [], Socket, Raw => () }
    ReAddBot { "re_add_bot", POST, "/v1/communities/{community_id}/channels/{channel_id}/bot-channel/re-add", None, Body, true, [], Socket, NoBody => Raw }
    RecoveryVerify { "recovery_verify", POST, "/v1/auth/recovery/verify", Cbor, Body, false, [], Http, Raw => Raw }
    RegenerateBackupCodes { "regenerate_backup_codes", POST, "/v1/auth/recovery/codes", Cbor, Body, true, [], Socket, Raw => Raw }
    RegisterRolePanel { "register_role_panel", PUT, "/v1/communities/{community_id}/role-panels", Cbor, Body, true, [], Socket, Raw => Raw }
    RemoveFactor { "remove_factor", POST, "/v1/auth/factors/remove", Cbor, None, true, [204], Socket, Raw => () }
    RestartBot { "restart_bot", POST, "/v1/communities/{community_id}/bot/restart", Cbor, Body, true, [], Socket, Raw => Raw }
    RevokeInvite { "revoke_invite", DELETE, "/v1/invites/{invite_id}", None, None, true, [], Socket, NoBody => () }
    RevokeRole { "revoke_role", DELETE, "/v1/communities/{community_id}/members/{user_id}/roles/{role_id}", None, None, true, [], Socket, NoBody => () }
    RotateBotWebhook { "rotate_bot_webhook", POST, "/v1/communities/{community_id}/bot/webhooks/{webhook_id}/rotate", None, Body, true, [], Socket, NoBody => Raw }
    SelectSelfRole { "select_self_role", PUT, "/v1/communities/{community_id}/self-roles/{role_id}", Cbor, None, true, [204], Socket, Raw => () }
    SendAfkActivity { "send_afk_activity", POST, "/v1/communities/{community_id}/afk/activity", Cbor, None, true, [204], Socket, Raw => () }
    SendPresenceActivity { "send_presence_activity", POST, "/v1/accounts/me/presence/activity", None, None, true, [204], Socket, NoBody => () }
    SetAfkSettings { "set_afk_settings", PUT, "/v1/communities/{community_id}/afk/settings", Cbor, None, true, [204], Socket, Raw => () }
    SetBotMusicSettings { "set_bot_music_settings", PUT, "/v1/communities/{community_id}/bot/music-settings", Cbor, Body, true, [], Socket, Raw => Raw }
    SetPresenceChoice { "set_presence_choice", PATCH, "/v1/accounts/me/presence", Cbor, None, true, [204], Socket, Raw => () }
    ShareCall { "share_call", POST, "/v1/calls/{call_id}/share", Cbor, None, true, [204], Socket, Raw => () }
    StartCall { "start_call", POST, "/v1/channels/{channel_id}/calls", Cbor, Status, true, [200, 201], Socket, Raw => Reply }
    StepUp { "step_up", POST, "/v1/auth/stepup", Cbor, Body, true, [], Socket, Raw => Raw }
    StepUpPasskeyBegin { "step_up_passkey_begin", POST, "/v1/auth/stepup/passkey/begin", Cbor, Body, true, [], Socket, Raw => Raw }
    StepUpPasskeyFinish { "step_up_passkey_finish", POST, "/v1/auth/stepup/passkey/finish", Cbor, Body, true, [], Socket, Raw => Raw }
    TotpConfirm { "totp_confirm", POST, "/v1/auth/totp/confirm", Cbor, Body, true, [], Http, Raw => Raw }
    TotpEnroll { "totp_enroll", POST, "/v1/auth/totp/enroll", Cbor, Body, true, [], Http, Raw => Raw }
    TotpVerify { "totp_verify", POST, "/v1/auth/totp/verify", Cbor, Body, false, [], Http, Raw => TotpVerifyAnswer }
    TransferOwnership { "transfer_ownership", POST, "/v1/communities/{community_id}/owner", Cbor, None, true, [], Socket, Raw => () }
    UnmarkBotChannel { "unmark_bot_channel", DELETE, "/v1/communities/{community_id}/channels/{channel_id}/bot-channel", None, None, true, [204], Socket, NoBody => () }
    UnshareCall { "unshare_call", DELETE, "/v1/calls/{call_id}/share", None, None, true, [204], Socket, NoBody => () }
    WarnMember { "warn_member", POST, "/v1/communities/{community_id}/members/{user_id}/warning", Cbor, Body, true, [], Socket, Raw => Raw }
}
