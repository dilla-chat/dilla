//! Area **attachments** (Task 12): GIF search through the instance's GIF routes, and DM GIF consent.
//!
//! Ports: packages/web/src/screens/conversation/{use-gif-community,dm-gif-consent}.ts and the E8 routes of
//! packages/client-core/src/http/routes.ts (`gifStatus`, `gifSearch`, `gifMedia`). The instance searches and fetches
//! for a member of the server: no Giphy URL is ever built or read here, and this device never contacts Giphy.

use std::sync::Arc;

use crate::client::{Client, ClientError, Inner};
use crate::http::routes::{self as r, NoBody};
use crate::state::{CommunityId, DeviceId, UserId};
use crate::sync::api::ApiError;
use crate::sync::value::{self, DecodeError};

/// routes.ts `GIF_QUERY_MAX_BYTES`.
pub const GIF_QUERY_MAX_BYTES: usize = 100;
/// routes.ts `GIF_OFFSET_MAX`.
pub const GIF_OFFSET_MAX: u32 = 4999;
/// routes.ts `GIF_PREVIEW_MAX_BYTES`.
pub const GIF_PREVIEW_MAX_BYTES: usize = 5 * 1024 * 1024;
/// routes.ts `GIF_FULL_MAX_BYTES`.
pub const GIF_FULL_MAX_BYTES: usize = 25 * 1024 * 1024;
/// routes.ts `GIF_DIM_MAX`.
pub const GIF_DIM_MAX: u32 = 4000;
/// routes.ts `GIF_RATINGS`.
pub const GIF_RATINGS: [&str; 4] = ["g", "pg", "pg-13", "r"];

/// routes.ts `GifStatus`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct GifStatus {
    pub on: bool,
    pub rating: String,
}

/// routes.ts `GifItem`: `width` and `height` are the preview's, both 0 when unknown.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct GifItem {
    pub id: String,
    pub title: String,
    pub width: u32,
    pub height: u32,
}

/// routes.ts `GifPage`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct GifPage {
    pub items: Vec<GifItem>,
    pub next: Option<u32>,
}

/// `preview` or `full` (`?r=`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum GifVariant {
    Preview,
    Full,
}

/// routes.ts `GIF_ID`: 1 to 64 letters and digits.
pub fn is_gif_id(id: &str) -> bool {
    (1..=64).contains(&id.len()) && id.bytes().all(|b| b.is_ascii_alphanumeric())
}

/// routes.ts `gifDims`: a preview's size as small positive integers, or both 0: the instance is not trusted to have
/// checked.
pub fn gif_dims(w: u64, h: u64) -> (u32, u32) {
    let ok = |n: u64| (1..=u64::from(GIF_DIM_MAX)).contains(&n);
    if ok(w) && ok(h) {
        (w as u32, h as u32)
    } else {
        (0, 0)
    }
}

/// routes.ts `gifMedia`'s `?r=` and its body bound (`maxBodyBytes`).
pub fn media_request(variant: GifVariant) -> (&'static str, u64) {
    match variant {
        GifVariant::Preview => ("preview", GIF_PREVIEW_MAX_BYTES as u64),
        GifVariant::Full => ("full", GIF_FULL_MAX_BYTES as u64),
    }
}

/// routes.ts `gifMedia`'s check: `GIF87a` or `GIF89a`.
pub fn is_gif_bytes(b: &[u8]) -> bool {
    b.len() >= 6 && &b[0..4] == b"GIF8" && (b[4] == b'7' || b[4] == b'9') && b[5] == b'a'
}

/// routes.ts `gifStatus`'s decode: `[on 0|1, rating, ...]`.
pub fn decode_status(raw: &[u8]) -> Result<GifStatus, DecodeError> {
    let v = value::decode(raw)?;
    let a = value::at_least(&v, 2)?;
    let on = match value::u53(&a[0])? {
        0 => false,
        1 => true,
        _ => return Err(DecodeError("gif status on")),
    };
    let rating = value::text(&a[1])?;
    if !GIF_RATINGS.contains(&rating.as_str()) {
        return Err(DecodeError("unknown gif rating"));
    }
    Ok(GifStatus { on, rating })
}

/// routes.ts `gifSearch`'s decode: `[[[id, title, w, h, ...]...], next|null, ...]`; an id the instance should never
/// send is dropped, not trusted into a path.
pub fn decode_page(raw: &[u8]) -> Result<GifPage, DecodeError> {
    let v = value::decode(raw)?;
    let a = value::at_least(&v, 2)?;
    let rows = a[0].as_array().ok_or(DecodeError("gif results"))?;
    let mut items = Vec::new();
    for row in rows {
        let row = value::at_least(row, 4)?;
        let id = value::text(&row[0])?;
        if !is_gif_id(&id) {
            continue;
        }
        let (width, height) = gif_dims(value::u53(&row[2])?, value::u53(&row[3])?);
        items.push(GifItem {
            id,
            title: value::text(&row[1])?,
            width,
            height,
        });
    }
    let next = value::opt(&a[1], value::u53)?.map(|n| u32::try_from(n).unwrap_or(u32::MAX));
    Ok(GifPage { items, next })
}

/// routes.ts `gifSearch`'s input check: at most 100 bytes, no control character; an offset of 0 to 4999.
pub fn check_query(q: &str, offset: u32) -> Result<(), ClientError> {
    if q.len() > GIF_QUERY_MAX_BYTES || q.chars().any(char::is_control) || offset > GIF_OFFSET_MAX {
        return Err(ClientError::Sync("E_ROUTE_INPUT".to_owned()));
    }
    Ok(())
}

/// dm-gif-consent.ts `dmGifConsentKey`, in this instance's prefs (`gifs.*` is Task 12's prefix): scoped to the
/// account and the device, so an old value is harmless across an identity change.
pub fn dm_consent_key(user: UserId, device: DeviceId) -> String {
    format!("gifs.dmConsent.{}.{}", user.hex(), device.hex())
}

fn api_error(e: ApiError) -> ClientError {
    if e.status == 0 && e.code == "E_NETWORK" {
        ClientError::Network(e.detail)
    } else {
        ClientError::Sync(e.code)
    }
}

/// The attachments area's handle for gifs: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Gifs {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Gifs`] handle.
    pub fn gifs(&self) -> Gifs {
        Gifs {
            inner: self.inner().clone(),
        }
    }
}

type Answer<T> = tokio::sync::oneshot::Receiver<Result<T, ClientError>>;

impl Gifs {
    /// `GET /v1/communities/{id}/gifs`: whether the server has a key, and its rating.
    pub fn status(&self, community: CommunityId) -> Answer<GifStatus> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let (_, body) = inner
                .api()
                .socket_route::<r::GifStatus>(&[&community.hex()], None)
                .await
                .map_err(api_error)?;
            decode_status(&body).map_err(|_| ClientError::Sync("E_CBOR_SHAPE".to_owned()))
        })
    }

    /// `GET …/gifs/search?q=&offset=`: 24 results a page; an empty `q` is trending.
    pub fn search(&self, community: CommunityId, q: String, offset: u32) -> Answer<GifPage> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            check_query(&q, offset)?;
            let (_, body) = inner
                .api()
                .socket_route::<r::GifSearch>(&[&community.hex(), &q, &offset.to_string()], None)
                .await
                .map_err(api_error)?;
            decode_page(&body).map_err(|_| ClientError::Sync("E_CBOR_SHAPE".to_owned()))
        })
    }

    /// `GET …/gifs/media/{gif_id}?r=preview|full`: the GIF's bytes through the instance, refused unless they are a GIF
    /// within the variant's bound.
    pub fn media(
        &self,
        community: CommunityId,
        id: String,
        variant: GifVariant,
    ) -> Answer<Vec<u8>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            if !is_gif_id(&id) {
                return Err(ClientError::Sync("E_ROUTE_INPUT".to_owned()));
            }
            let (r, cap) = media_request(variant);
            // A body past the bound is E_BODY_TOO_LARGE, refused before the rest is read.
            let bytes = inner
                .api()
                .http_route_capped::<r::GifMedia>(&[&community.hex(), &id, r], NoBody, true, cap)
                .await
                .map_err(api_error)?
                .0;
            if !is_gif_bytes(&bytes) {
                return Err(ClientError::Sync("E_UNAVAILABLE".to_owned()));
            }
            Ok(bytes)
        })
    }

    /// Whether this account on this device agreed to DM GIFs (`DmGifPrivacySetting`); `false` before sign-in.
    /// The Privacy toggle: whether any DM GIF consent is stored (from the toggle, or for a named server).
    pub fn dm_consent(&self) -> bool {
        self.consent_value().is_some()
    }

    fn consent_value(&self) -> Option<String> {
        let s = self.inner.snapshot();
        let (u, d) = (s.account.user_id?, s.account.device_id?);
        self.inner.prefs().get::<String>(&dm_consent_key(u, d))
    }

    /// Whether this account on this device agreed to DM GIFs through `community` (the notice named it).
    pub fn dm_consent_for(&self, community: CommunityId) -> bool {
        consent_covers(self.consent_value().as_deref(), community)
    }

    /// Records the DM GIF consent (`Some("on")` from Privacy, `Some(<server hex>)` from the notice that named the
    /// server) or removes it (`None`). Before sign-in there is nothing to scope it to: refused.
    pub fn set_dm_consent(&self, value: Option<&str>) -> Result<(), ClientError> {
        let s = self.inner.snapshot();
        let (Some(u), Some(d)) = (s.account.user_id, s.account.device_id) else {
            return Err(ClientError::NoIdentity);
        };
        let key = dm_consent_key(u, d);
        let prefs = self.inner.prefs();
        let done = match value {
            Some(v) => prefs.set(&key, &v),
            None => prefs.remove(&key),
        };
        done.map_err(|e| ClientError::Store(e.to_string()))
    }

    /// use-gif-community.ts `findCommunity` for a 1:1 DM with `peer`: the servers both people are in, asked in order
    /// with `gifStatus` only (no search, no content of the DM), until one has GIFs on. Called only once the GIF button
    /// is pressed, never on opening the conversation.
    pub fn find_dm_community(&self, peer: UserId) -> Answer<Option<CommunityId>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let s = inner.snapshot();
            let Some(me) = s.account.user_id else {
                return Ok(None);
            };
            for community in dm_gif_candidates(&s, me, peer) {
                let Ok((_, body)) = inner
                    .api()
                    .socket_route::<r::GifStatus>(&[&community.hex()], None)
                    .await
                else {
                    // This server's key is absent or it is unavailable: try the next shared one.
                    continue;
                };
                if decode_status(&body).is_ok_and(|st| st.on) {
                    return Ok(Some(community));
                }
            }
            Ok(None)
        })
    }

    /// Test support: sets `community`'s Giphy key (a manager's `PUT …/gifs/key`; the app's own setting is Task 15's).
    #[cfg(feature = "testing")]
    pub fn testing_set_key(&self, community: CommunityId, key: &str) -> Answer<()> {
        let inner = self.inner.clone();
        let body = crate::gateway::frames::Value::Array(vec![crate::gateway::frames::Value::Text(
            key.to_owned(),
        )])
        .encode();
        self.inner.spawn(async move {
            inner
                .api()
                .socket_route::<r::GifSetKey>(&[&community.hex()], Some(body))
                .await
                .map(|_| ())
                .map_err(api_error)
        })
    }
}

/// Where a composer's GIFs come from (use-gif-community.ts `scope`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum GifScope {
    /// A server channel: its own server.
    Server(CommunityId),
    /// A 1:1 DM with this person: a server both share, found when the GIF button is pressed.
    Direct(UserId),
    /// A thread composer, a group DM, or no conversation: no GIFs.
    None,
}

/// use-gif-community.ts's scope: a thread never offers GIFs; a server channel uses its server; a 1:1 DM its peer.
pub fn gif_scope(
    community: Option<CommunityId>,
    dm_peer: Option<UserId>,
    in_thread: bool,
) -> GifScope {
    match (in_thread, community, dm_peer) {
        (true, _, _) => GifScope::None,
        (false, Some(c), _) => GifScope::Server(c),
        (false, None, Some(p)) => GifScope::Direct(p),
        _ => GifScope::None,
    }
}

/// The servers, in the list's order, whose member lists hold both `me` and `peer` (use-gif-community.ts `shared`).
pub fn dm_gif_candidates(s: &crate::state::AppState, me: UserId, peer: UserId) -> Vec<CommunityId> {
    s.communities
        .iter()
        .map(|c| c.id)
        .filter(|id| {
            s.members.get(id).is_some_and(|m| {
                m.iter().any(|x| x.user_id == me) && m.iter().any(|x| x.user_id == peer)
            })
        })
        .collect()
}

/// Whether a stored consent value covers `community`: only the notice's consent, which names the server, does.
pub fn consent_covers(value: Option<&str>, community: CommunityId) -> bool {
    value.is_some_and(|v| v == community.hex())
}

/// The logic of routes.ts's E8 helpers, same inputs and same outputs.
#[cfg(test)]
mod tests {
    use super::*;
    use crate::gateway::frames::Value;

    #[test]
    fn ids_dims_and_signatures_are_checked_as_routes_ts_checks_them() {
        assert!(is_gif_id("abcXYZ09"));
        assert!(!is_gif_id(""));
        assert!(!is_gif_id(&"a".repeat(65)));
        assert!(!is_gif_id("../x"));
        assert_eq!(gif_dims(200, 150), (200, 150));
        assert_eq!(gif_dims(0, 150), (0, 0));
        assert_eq!(gif_dims(4001, 10), (0, 0));
        assert!(is_gif_bytes(b"GIF89a...."));
        assert!(is_gif_bytes(b"GIF87a"));
        assert!(!is_gif_bytes(b"GIF88a"));
        assert!(!is_gif_bytes(b"\x89PNG\r\n"));
    }

    /// routes.ts `gifMedia`: the request is bounded by the variant's size (the transport stops reading past it).
    #[test]
    fn a_gif_is_fetched_with_its_variant_s_bound() {
        assert_eq!(
            media_request(GifVariant::Preview),
            ("preview", 5 * 1024 * 1024)
        );
        assert_eq!(media_request(GifVariant::Full), ("full", 25 * 1024 * 1024));
    }

    #[test]
    fn a_query_is_bounded_and_has_no_control_character() {
        assert!(check_query("cats", 0).is_ok());
        assert!(check_query(&"a".repeat(100), 4999).is_ok());
        assert!(check_query(&"a".repeat(101), 0).is_err());
        assert!(check_query("a\nb", 0).is_err());
        assert!(check_query("a", 5000).is_err());
    }

    #[test]
    fn status_and_page_decode_and_drop_an_id_never_trusted_into_a_path() {
        let status = Value::Array(vec![Value::Uint(1), Value::Text("pg-13".into())]).encode();
        assert_eq!(
            decode_status(&status).unwrap(),
            GifStatus {
                on: true,
                rating: "pg-13".into()
            }
        );
        let bad = Value::Array(vec![Value::Uint(1), Value::Text("x".into())]).encode();
        assert!(decode_status(&bad).is_err());
        let row = |id: &str, w: u64| {
            Value::Array(vec![
                Value::Text(id.into()),
                Value::Text("a cat".into()),
                Value::Uint(w),
                Value::Uint(100),
            ])
        };
        let page = Value::Array(vec![
            Value::Array(vec![row("ok1", 200), row("../evil", 200), row("ok2", 0)]),
            Value::Uint(24),
        ])
        .encode();
        let p = decode_page(&page).unwrap();
        assert_eq!(p.items.len(), 2);
        assert_eq!((p.items[0].width, p.items[0].height), (200, 100));
        assert_eq!((p.items[1].width, p.items[1].height), (0, 0));
        assert_eq!(p.next, Some(24));
    }

    /// use-gif-community.ts: a server channel's own server; a 1:1 DM only through a shared server; never a thread.
    #[test]
    fn the_gif_scope_is_the_server_a_dm_s_shared_servers_or_nothing_in_a_thread() {
        let c = CommunityId([1; 16]);
        let peer = UserId([2; 16]);
        assert_eq!(gif_scope(Some(c), None, false), GifScope::Server(c));
        assert_eq!(gif_scope(None, Some(peer), false), GifScope::Direct(peer));
        assert_eq!(
            gif_scope(Some(c), None, true),
            GifScope::None,
            "a thread composer offers no GIFs"
        );
        assert_eq!(gif_scope(None, Some(peer), true), GifScope::None);
        assert_eq!(
            gif_scope(None, None, false),
            GifScope::None,
            "a group DM offers none"
        );
    }

    #[test]
    fn dm_candidates_are_the_servers_both_people_are_in() {
        use crate::state::{AppState, Member};
        let (me, peer, other) = (UserId([1; 16]), UserId([2; 16]), UserId([3; 16]));
        let member = |u: UserId| Member {
            user_id: u,
            username: String::new(),
            display: String::new(),
            kind: 0,
            role_ids: Vec::new(),
            status_msg: String::new(),
            bio: String::new(),
        };
        let mut s = AppState::default();
        for (c, people) in [
            (5u8, vec![me, peer]),
            (6, vec![me, other]),
            (7, vec![peer, me, other]),
        ] {
            let id = CommunityId([c; 16]);
            s.communities.push(crate::state::Community {
                id,
                name: String::new(),
                owner: me,
                policy_version: 0,
            });
            s.members
                .insert(id, people.into_iter().map(member).collect());
        }
        assert_eq!(
            dm_gif_candidates(&s, me, peer),
            [CommunityId([5; 16]), CommunityId([7; 16])]
        );
    }

    /// DmGifPrivacy.test.tsx: consent names the server it was given for; "on" from Privacy alone is not consent for any
    /// server (the named-server notice still comes first).
    #[test]
    fn consent_is_for_the_named_server_only() {
        let a = CommunityId([0xa; 16]);
        assert!(consent_covers(Some(&a.hex()), a));
        assert!(!consent_covers(Some("on"), a));
        assert!(!consent_covers(Some(&CommunityId([0xb; 16]).hex()), a));
        assert!(!consent_covers(None, a));
    }

    #[test]
    fn the_consent_key_is_under_the_gifs_prefix_and_names_no_secret() {
        let k = dm_consent_key(UserId([0xaa; 16]), DeviceId([0xbb; 16]));
        assert!(k.starts_with("gifs.dmConsent."));
        assert!(!crate::prefs::names_a_secret(&k));
    }
}
