//! The desktop client's HTTP transport: [`ReqwestHttp`] implements [`Http`] over reqwest (rustls only), sending every
//! route of [`routes::TABLE`] as the TypeScript client does (`packages/client-core/src/http/client.ts`): the bearer in
//! `Authorization`, `application/cbor` or `application/octet-stream` bodies as the route's table row says, no cookies,
//! no redirects, a per-attempt timeout.
//!
//! Not carried over from the TypeScript transport: its pacing buckets, its retries (429, 503, an idempotent 500 or
//! network failure), its one re-authentication on a 401, and its `X-Dilla-Generation` hook. The caller that owns the
//! session adds them; a refusal here is the status of the first answer.

use std::sync::{Arc, RwLock};
use std::time::Duration;

use ciborium::value::Value;
use url::Url;
use zeroize::Zeroizing;

use crate::seams::{Http, HttpError, Route};

pub mod routes;

use routes::{Answer, BodyEncoding, Entry, Transport};

/// WB I1: one attempt that has not answered in this long is a transport failure.
const ATTEMPT_TIMEOUT: Duration = Duration::from_secs(30);
/// The bound on a success body for a route client.ts gives no `maxBodyBytes` (a blob or a GIF, at most 25 MiB, or a
/// caller-chosen bound): the TypeScript client reads those unbounded, this one never reads more than this.
const BODY_CEILING: u64 = 32 * 1024 * 1024;
/// A backup is capped but sits on the sign-up, sign-in and revoke paths: a longer bound than the default.
const BACKUP_TIMEOUT: Duration = Duration::from_secs(120);

/// The session's bearer token, shared by every clone so a renewed session reaches the transport that holds it. It is
/// zeroized when the last clone drops and never printed.
#[derive(Clone, Default)]
pub struct AuthToken(Arc<RwLock<Option<Zeroizing<String>>>>);

impl AuthToken {
    /// A token as the instance issued it.
    pub fn new(token: impl Into<String>) -> Self {
        let this = Self::default();
        this.set(token);
        this
    }

    /// No session yet: only the routes that take no bearer are answered.
    pub fn anonymous() -> Self {
        Self::default()
    }

    /// Replaces the token (a new session).
    pub fn set(&self, token: impl Into<String>) {
        *self
            .0
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner) =
            Some(Zeroizing::new(token.into()));
    }

    /// Forgets the token (sign-out).
    pub fn clear(&self) {
        *self
            .0
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = None;
    }

    /// The `Authorization` header, `None` when there is no session. A token that cannot be a header value is an error,
    /// never a request sent without its bearer.
    fn header(&self) -> Result<Option<reqwest::header::HeaderValue>, HttpError> {
        let guard = self
            .0
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let Some(token) = guard.as_ref() else {
            return Ok(None);
        };
        // The header value is the one copy reqwest owns; the staging buffer is zeroized and the value marked sensitive.
        let mut bytes = Zeroizing::new(Vec::with_capacity(7 + token.len()));
        bytes.extend_from_slice(b"Bearer ");
        bytes.extend_from_slice(token.as_bytes());
        let mut value = reqwest::header::HeaderValue::from_bytes(&bytes).map_err(|_| {
            HttpError::Transport("the session token is not a valid header value".into())
        })?;
        value.set_sensitive(true);
        Ok(Some(value))
    }
}

impl std::fmt::Debug for AuthToken {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("AuthToken(<redacted>)")
    }
}

/// [`Http`] over reqwest, for one instance.
#[derive(Clone, Debug)]
pub struct ReqwestHttp {
    base: String,
    auth: AuthToken,
    client: reqwest::Client,
}

fn transport(e: reqwest::Error) -> HttpError {
    // The URL can carry an invite code; the cause is what a caller needs.
    HttpError::Transport(e.without_url().to_string())
}

impl ReqwestHttp {
    /// A transport for the instance at `base`. It fails only when the TLS backend cannot start.
    pub fn new(base: Url, auth: AuthToken) -> Result<Self, HttpError> {
        let client = reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(Duration::from_secs(15))
            .build()
            .map_err(transport)?;
        Ok(Self {
            base: base.as_str().trim_end_matches('/').to_owned(),
            auth,
            client,
        })
    }
}

/// What a path slot holds, by its name in the route table (the checks `routes.ts` makes before it builds a path).
enum Slot {
    /// An id (`id()` in routes.ts): 16 bytes, 32 lowercase hex digits.
    Id,
    /// A blob digest or a webhook avatar id: 32 bytes, 64 lowercase hex digits.
    Digest,
    /// A count (`count()`): decimal digits.
    Count,
    /// `kindOf()`: 0 or 1.
    Kind,
    /// A GIF id: 1 to 64 letters and digits.
    Gif,
    /// Anything else (an invite code): one non-empty segment.
    Free,
}

fn slot_of(name: &str) -> Slot {
    match name {
        "blob_id" | "avatar_id" => Slot::Digest,
        "seq" | "message_seq" | "welcome_id" => Slot::Count,
        "kind" => Slot::Kind,
        "gif_id" => Slot::Gif,
        "code" => Slot::Free,
        n if n.ends_with("_id") => Slot::Id,
        _ => Slot::Free,
    }
}

fn is_lower_hex(s: &str, len: usize) -> bool {
    s.len() == len && s.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
}

/// A path-segment argument the server could read as another route, or that routes.ts would have refused.
fn check_segment(name: &str, arg: &str) -> Result<(), HttpError> {
    // The url crate (WHATWG) removes dot segments, `%2e` included, so these can never be sent as data.
    let ok = !matches!(arg, "" | "." | "..")
        && match slot_of(name) {
            Slot::Id => is_lower_hex(arg, 32),
            Slot::Digest => is_lower_hex(arg, 64),
            Slot::Count => (1..=20).contains(&arg.len()) && arg.bytes().all(|b| b.is_ascii_digit()),
            Slot::Kind => matches!(arg, "0" | "1"),
            Slot::Gif => {
                (1..=64).contains(&arg.len()) && arg.bytes().all(|b| b.is_ascii_alphanumeric())
            }
            Slot::Free => true,
        };
    if ok {
        Ok(())
    } else {
        Err(HttpError::Transport(format!(
            "invalid path argument for {name}"
        )))
    }
}

/// `template` with each `{name}` replaced by the next of `args`, percent-encoded as one path segment or query value. A
/// path-segment argument (before any `?`) must be what its slot holds (an id is 32 lowercase hex digits, ...) and is
/// never empty, `.` or `..`. A trailing `?k={v}` group whose arguments were not given is left out; any other missing or
/// surplus argument is an error.
pub fn fill_path(template: &str, args: &[&str]) -> Result<String, HttpError> {
    let mut out = String::with_capacity(template.len() + 32);
    let mut args = args.iter();
    let mut rest = template;
    let mut query_from = None;
    while let Some(open) = rest.find('{') {
        let (literal, tail) = rest.split_at(open);
        let close = tail
            .find('}')
            .ok_or_else(|| HttpError::Transport(format!("bad route template {template}")))?;
        if query_from.is_none() {
            query_from = literal.find('?').map(|i| out.len() + i);
        }
        out.push_str(literal);
        match args.next() {
            Some(arg) => {
                if query_from.is_none() {
                    check_segment(&tail[1..close], arg)?;
                }
                encode_into(&mut out, arg);
            }
            None => {
                return match query_from {
                    Some(at) => {
                        out.truncate(at);
                        Ok(out)
                    }
                    None => Err(HttpError::Transport(format!(
                        "missing path argument for {template}"
                    ))),
                };
            }
        }
        rest = &tail[close + 1..];
    }
    out.push_str(rest);
    if args.next().is_some() {
        return Err(HttpError::Transport(format!(
            "too many path arguments for {template}"
        )));
    }
    Ok(out)
}

fn encode_into(out: &mut String, arg: &str) {
    for b in arg.bytes() {
        if b.is_ascii_alphanumeric() || matches!(b, b'-' | b'.' | b'_' | b'~') {
            out.push(char::from(b));
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
}

/// The success body of `response`, at most `cap` bytes (`maxBodyBytes` in client.ts): a longer `Content-Length` is refused
/// before a byte is read, and a body that outgrows the cap while streaming stops being read.
async fn read_capped(mut response: reqwest::Response, cap: u64) -> Result<Vec<u8>, HttpError> {
    let too_large = || HttpError::Decode(format!("the response body is larger than {cap} bytes"));
    if response.content_length().is_some_and(|n| n > cap) {
        return Err(too_large());
    }
    let mut body = Vec::new();
    while let Some(chunk) = response.chunk().await.map_err(transport)? {
        if (body.len() as u64).saturating_add(chunk.len() as u64) > cap {
            return Err(too_large());
        }
        body.extend_from_slice(&chunk);
    }
    Ok(body)
}

/// The body a request serializes to: `None` for [`routes::NoBody`], the bytes of a [`routes::Raw`].
fn body_of<R: Route>(req: &R::Request, entry: &Entry) -> Result<Option<Vec<u8>>, HttpError> {
    let value = Value::serialized(req).map_err(|e| HttpError::Decode(format!("request: {e}")))?;
    match (value, entry.request) {
        (Value::Null, BodyEncoding::None) => Ok(None),
        (Value::Bytes(body), BodyEncoding::Cbor | BodyEncoding::Octet) => Ok(Some(body)),
        _ => Err(HttpError::Decode(format!(
            "the {} request does not carry the body its route takes",
            entry.name
        ))),
    }
}

#[async_trait::async_trait]
impl Http for ReqwestHttp {
    async fn call<R: Route>(
        &self,
        path_args: &[&str],
        req: R::Request,
    ) -> Result<R::Response, HttpError> {
        let entry = routes::lookup(&R::METHOD, R::PATH).ok_or_else(|| {
            HttpError::Transport(format!(
                "{} {} is not in the route table",
                R::METHOD,
                R::PATH
            ))
        })?;
        if entry.transport == Transport::Socket {
            // No HTTP fallback: the instance serves this route only as a gateway request frame.
            return Err(HttpError::Transport(format!(
                "{} is a gateway request, not an HTTP route",
                entry.name
            )));
        }
        let body = body_of::<R>(&req, entry)?;
        let url = format!("{}{}", self.base, fill_path(entry.path, path_args)?);

        let mut request = self.client.request(entry.method.clone(), url);
        if entry.auth
            && let Some(bearer) = self.auth.header()?
        {
            request = request.header(reqwest::header::AUTHORIZATION, bearer);
        }
        if let Some(body) = body {
            let content_type = match entry.request {
                BodyEncoding::Octet => "application/octet-stream",
                _ => "application/cbor",
            };
            request = request
                .header(reqwest::header::CONTENT_TYPE, content_type)
                .body(body);
        }
        if entry.path.starts_with("/i/") {
            request = request.header(reqwest::header::ACCEPT, "application/cbor");
        }
        // A blob transfer may outlast any bound on a slow link.
        if !entry.path.contains("/blobs/") {
            let timeout = if entry.path.starts_with("/v1/backups") {
                BACKUP_TIMEOUT
            } else {
                ATTEMPT_TIMEOUT
            };
            request = request.timeout(timeout);
        }

        let response = request.send().await.map_err(transport)?;
        let status = response.status().as_u16();
        if !entry.accepts(status) {
            // The refusal's body is never read: the status is the whole error.
            return Err(HttpError::Status(status));
        }
        let body = read_capped(response, entry.max_body.unwrap_or(BODY_CEILING)).await?;
        let value = match entry.answer {
            Answer::None => Value::Null,
            Answer::Body => Value::Bytes(body),
            Answer::Status => Value::Array(vec![Value::Integer(status.into()), Value::Bytes(body)]),
        };
        value
            .deserialized()
            .map_err(|e| HttpError::Decode(e.to_string()))
    }
}
