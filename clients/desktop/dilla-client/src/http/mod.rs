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

    fn header(&self) -> Option<reqwest::header::HeaderValue> {
        let guard = self
            .0
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let token = guard.as_ref()?;
        let mut value =
            reqwest::header::HeaderValue::from_str(&format!("Bearer {}", token.as_str())).ok()?;
        value.set_sensitive(true);
        Some(value)
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

/// `template` with each `{name}` replaced by the next of `args`, percent-encoded as one path segment or query value. A
/// trailing `?k={v}` group whose arguments were not given is left out; any other missing or surplus argument is an error.
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
            Some(arg) => encode_into(&mut out, arg),
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
            && let Some(bearer) = self.auth.header()
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
            return Err(HttpError::Status(status));
        }
        let body = response.bytes().await.map_err(transport)?.to_vec();
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
