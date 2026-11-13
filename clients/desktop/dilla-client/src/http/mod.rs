//! The desktop client's HTTP transport: [`ReqwestHttp`] implements [`Http`] over reqwest (rustls only), sending every
//! route of [`routes::TABLE`] as the TypeScript client does (`packages/client-core/src/http/client.ts`): the bearer in
//! `Authorization`, `application/cbor` or `application/octet-stream` bodies as the route's table row says, no cookies,
//! no redirects, a per-attempt timeout.
//!
//! Not carried over from the TypeScript transport: its pacing buckets, its retries (429, 503, an idempotent 500 or
//! network failure), its one re-authentication on a 401, and its `X-Dilla-Generation` hook. The caller that owns the
//! session adds them; a refusal here is the status of the first answer.

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

pub use crate::auth::AuthToken;

/// The `Authorization` header for `auth`, `None` when there is no session. A token that cannot be a header value is an
/// error, never a request sent without its bearer.
fn bearer_header(auth: &AuthToken) -> Result<Option<reqwest::header::HeaderValue>, HttpError> {
    auth.with(|token| {
        let Some(token) = token else {
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
    })
}

/// [`Http`] over reqwest, for one instance.
#[derive(Clone, Debug)]
pub struct ReqwestHttp {
    base: String,
    auth: AuthToken,
    client: reqwest::Client,
    /// The instance's clock as its last `Date` header said (Task 20: a sealed Bot command's `sent_at`).
    clock: std::sync::Arc<ServerClock>,
}

/// The most the instance's clock is believed to be off this device's.
const MAX_OFFSET_S: i64 = 86_400;

/// The offset of the instance's clock from this device's, from the `Date` header of each HTTP answer (whole seconds).
#[derive(Debug, Default)]
pub struct ServerClock {
    /// Instance seconds minus local seconds; `i64::MIN` while no answer carried a date.
    offset: std::sync::atomic::AtomicI64,
}

fn local_now_s() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| i64::try_from(d.as_secs()).unwrap_or(i64::MAX))
}

impl ServerClock {
    fn new() -> Self {
        Self {
            offset: std::sync::atomic::AtomicI64::new(i64::MIN),
        }
    }

    /// Records an answer's `Date` header.
    pub fn observe(&self, date: &str) {
        // A date more than a day off this device's clock is no clock reading (a proxy's cache, a broken header).
        if let Some(at) = parse_http_date(date)
            && (at - local_now_s()).abs() <= MAX_OFFSET_S
        {
            self.offset
                .store(at - local_now_s(), std::sync::atomic::Ordering::Relaxed);
        }
    }

    /// Unix seconds by the instance's clock, else this device's (no answer seen yet).
    pub fn now_s(&self) -> u64 {
        let offset = self.offset.load(std::sync::atomic::Ordering::Relaxed);
        let offset = if offset == i64::MIN { 0 } else { offset };
        u64::try_from(local_now_s().saturating_add(offset)).unwrap_or(0)
    }
}

/// An RFC 9110 IMF-fixdate (`Sun, 06 Nov 1994 08:49:37 GMT`) as unix seconds.
pub fn parse_http_date(text: &str) -> Option<i64> {
    let p: Vec<&str> = text.split_whitespace().collect();
    if p.len() != 6 || p[5] != "GMT" {
        return None;
    }
    let day: i64 = p[1].parse().ok()?;
    let month = [
        "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
    ]
    .iter()
    .position(|m| *m == p[2])? as i64
        + 1;
    let year: i64 = p[3].parse().ok()?;
    let hms: Vec<i64> = p[4]
        .split(':')
        .map(|x| x.parse().ok())
        .collect::<Option<_>>()?;
    if hms.len() != 3 || !(1..=31).contains(&day) || hms[0] > 23 || hms[1] > 59 || hms[2] > 60 {
        return None;
    }
    let y = if month <= 2 { year - 1 } else { year };
    let era = y.div_euclid(400);
    let yoe = y - era * 400;
    let doy = (153 * ((month + 9) % 12) + 2) / 5 + day - 1;
    let days = era * 146_097 + yoe * 365 + yoe / 4 - yoe / 100 + doy - 719_468;
    Some(days * 86_400 + hms[0] * 3600 + hms[1] * 60 + hms[2])
}

fn transport(e: reqwest::Error) -> HttpError {
    // The URL can carry an invite code; the cause is what a caller needs.
    HttpError::Transport(e.without_url().to_string())
}

impl ReqwestHttp {
    /// Unix seconds by the instance's clock (its last `Date` header), else this device's.
    pub fn server_now_s(&self) -> u64 {
        self.clock.now_s()
    }

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
            clock: std::sync::Arc::new(ServerClock::new()),
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
            && let Some(bearer) = bearer_header(&self.auth)?
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
        if let Some(date) = response
            .headers()
            .get(reqwest::header::DATE)
            .and_then(|d| d.to_str().ok())
        {
            self.clock.observe(date);
        }
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

#[cfg(test)]
mod clock_tests {
    use super::*;

    #[test]
    fn an_imf_fixdate_is_read_as_unix_seconds() {
        assert_eq!(
            parse_http_date("Sun, 06 Nov 1994 08:49:37 GMT"),
            Some(784_111_777)
        );
        assert_eq!(
            parse_http_date("Fri, 13 Nov 2026 01:19:41 GMT"),
            Some(1_794_532_781)
        );
        assert_eq!(parse_http_date("Sunday, 06-Nov-94 08:49:37 GMT"), None);
        let clock = ServerClock::new();
        let local = clock.now_s();
        clock.observe("Sun, 06 Nov 1994 08:49:37 GMT");
        assert!(clock.now_s() >= local, "a date decades off is ignored");
        let an_hour_ahead = local_now_s() + 3600;
        let date = chrono_free_date(an_hour_ahead);
        clock.observe(&date);
        let ahead = clock.now_s();
        assert!(
            (3599..=3601).contains(&(ahead as i64 - local_now_s())),
            "an hour ahead is taken: {date}"
        );
    }

    fn chrono_free_date(t: i64) -> String {
        let days = t.div_euclid(86_400);
        let rem = t.rem_euclid(86_400);
        let z = days + 719_468;
        let era = z.div_euclid(146_097);
        let doe = z - era * 146_097;
        let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
        let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
        let mp = (5 * doy + 2) / 153;
        let d = doy - (153 * mp + 2) / 5 + 1;
        let m = if mp < 10 { mp + 3 } else { mp - 9 };
        let y = yoe + era * 400 + i64::from(m <= 2);
        let months = [
            "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
        ];
        format!(
            "Mon, {d:02} {} {y} {:02}:{:02}:{:02} GMT",
            months[(m - 1) as usize],
            rem / 3600,
            rem % 3600 / 60,
            rem % 60
        )
    }
}
