//! Area **factors** (Task 26): the system-browser ceremony (get, create, oidc; 2 min timeout; SHA-256 verifier;
//! `desktop-auth/request` and `desktop-auth/<id>/consume`), the browser opened with `open`.
//!
//! Ports: packages/desktop/src/browser-auth.ts (the shell's half) and, for what the page does, web
//! `desktop-auth-browser.ts`. The desktop app has no WebAuthn of its own: a passkey ceremony (and an OIDC sign-in) runs
//! in the system browser on the instance's own page, which hands the result back through the instance.
//!
//! Only a fixed navigation reaches the browser: `<instance>/desktop-auth/<id>`, where `id` is the instance's 43-character
//! opaque request id. The verifier (32 random bytes, base64url) stays in this process, expires with the request after two
//! minutes and never occurs in a URL; the instance holds only its SHA-256. The answer (a WebAuthn response or an OIDC
//! assertion) is answered in a [`Zeroizing`] buffer and never printed.
//!
//! It runs before a [`crate::Client`] exists (like [`crate::signin`]), so it has no `Client` accessor.

use std::sync::Arc;
use std::time::{Duration, Instant};

use url::Url;
use zeroize::Zeroizing;

/// The longest a ceremony may take (browser-auth.ts `AbortSignal.timeout(120000)`).
pub const CEREMONY_TIMEOUT: Duration = Duration::from_secs(120);
/// How often the result is asked for (browser-auth.ts `delay(1000)`).
pub const POLL_INTERVAL: Duration = Duration::from_secs(1);
/// The largest reply read (browser-auth.ts `size > 70000`).
const REPLY_MAX_BYTES: usize = 70_000;
/// The largest options document sent (browser-auth.ts `options.length > 65536`).
const OPTIONS_MAX_BYTES: usize = 65_536;

/// What the browser is asked to do.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CeremonyKind {
    /// `navigator.credentials.get` on the instance's options (a passkey sign-in or step-up).
    Get,
    /// `navigator.credentials.create` (a new passkey).
    Create,
    /// The instance's OIDC sign-in.
    Oidc,
}

impl CeremonyKind {
    fn wire(self) -> &'static str {
        match self {
            Self::Get => "get",
            Self::Create => "create",
            Self::Oidc => "oidc",
        }
    }
}

/// What the ceremony answered: the WebAuthn response (get and create) or the OIDC assertion, and what that login still
/// owes (0, 1 or 2). Never printed.
pub struct CeremonyResult {
    pub response: Option<Zeroizing<String>>,
    pub assertion: Option<Zeroizing<String>>,
    pub needs: u8,
}

impl std::fmt::Debug for CeremonyResult {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CeremonyResult")
            .field("needs", &self.needs)
            .finish_non_exhaustive()
    }
}

/// Why a ceremony ended without a result (web `PasskeyError` kinds through `desktopAuthKind`).
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum BrowserAuthError {
    /// The request or a reply was malformed (never sent to the browser).
    #[error("invalid browser ceremony: {0}")]
    Invalid(&'static str),
    /// The instance is older than the desktop sign-in (404 or 405 on the first request): `desktop-auth-unsupported`.
    #[error("desktop-auth-unsupported")]
    Unsupported,
    /// Too many attempts (429): `desktop-auth-rate-limited`.
    #[error("desktop-auth-rate-limited")]
    RateLimited,
    /// The request expired or was refused on the way: start again in the app.
    #[error("Browser sign-in unavailable. Start again in Dilla.")]
    Unavailable,
    /// The page reported a failure (its own text, at most 128 characters): cancelled, or the passkey failed.
    #[error("{0}")]
    Failed(String),
    /// Two minutes passed without a result.
    #[error("the browser sign-in timed out")]
    TimedOut,
    /// The instance could not be reached.
    #[error("the connection dropped: {0}")]
    Network(String),
    /// The browser could not be opened.
    #[error("the browser could not be opened: {0}")]
    Opener(String),
    /// The person cancelled (Cancel, Back, Escape, or the screen went away): nothing more is sent.
    #[error("the browser sign-in was cancelled")]
    Cancelled,
}

/// Ends a running ceremony ([`BrowserAuth::run_cancellable`]): every clone shares it; once cancelled, no further request
/// is sent and the ceremony answers [`BrowserAuthError::Cancelled`].
#[derive(Clone, Debug, Default)]
pub struct Cancel(Arc<(std::sync::atomic::AtomicBool, tokio::sync::Notify)>);

impl Cancel {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn cancel(&self) {
        self.0.0.store(true, std::sync::atomic::Ordering::Release);
        self.0.1.notify_waiters();
    }

    pub fn is_cancelled(&self) -> bool {
        self.0.0.load(std::sync::atomic::Ordering::Acquire)
    }

    async fn cancelled(&self) {
        loop {
            let notified = self.0.1.notified();
            if self.is_cancelled() {
                return;
            }
            notified.await;
        }
    }

    /// Runs `f` unless (or until) cancelled.
    async fn guard<T>(
        &self,
        f: impl std::future::Future<Output = Result<T, BrowserAuthError>>,
    ) -> Result<T, BrowserAuthError> {
        if self.is_cancelled() {
            return Err(BrowserAuthError::Cancelled);
        }
        tokio::select! {
            () = self.cancelled() => Err(BrowserAuthError::Cancelled),
            r = f => r,
        }
    }
}

/// Opens the system browser on a URL (`open::that_detached` in the app, a test browser in tests).
pub trait Opener: Send + Sync {
    fn open(&self, url: &str) -> Result<(), String>;
}

/// The OS's default browser.
#[derive(Clone, Copy, Debug, Default)]
pub struct SystemOpener;

impl Opener for SystemOpener {
    fn open(&self, url: &str) -> Result<(), String> {
        open::that_detached(url).map_err(|e| e.to_string())
    }
}

/// The browser auth entry point: one ceremony at a time per call of [`BrowserAuth::run`].
#[derive(Clone)]
pub struct BrowserAuth {
    opener: Arc<dyn Opener>,
    /// An `http://` instance on a loopback address is accepted (development and tests only).
    allow_loopback: bool,
    timeout: Duration,
    poll: Duration,
}

impl std::fmt::Debug for BrowserAuth {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("BrowserAuth")
            .field("allow_loopback", &self.allow_loopback)
            .finish_non_exhaustive()
    }
}

impl Default for BrowserAuth {
    /// The system browser; loopback instances only in a debug build.
    fn default() -> Self {
        Self::new(Arc::new(SystemOpener), cfg!(debug_assertions))
    }
}

/// browser-auth.ts `canonicalInstance` for a stored instance: Task 27's [`crate::instances::address::canonical_instance`]
/// (one implementation for the desktop: no credentials, `?` or `#`; no encoded slash, backslash, NUL or dot, which could
/// steer the fixed `desktop-auth/` navigation elsewhere; HTTPS, or HTTP on a loopback host when `allow_loopback`; a host
/// not ending in a dot; the path ends in `/` and is not under `/assets/`).
pub fn canonical_instance(server: &Url, allow_loopback: bool) -> Result<Url, BrowserAuthError> {
    crate::instances::address::canonical_instance(server.as_str(), allow_loopback)
        .map_err(|e| BrowserAuthError::Invalid(e.0))
}

/// base64url without padding (RFC 4648 §5).
fn base64url(bytes: &[u8]) -> String {
    const ALPHABET: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
    let mut out = String::with_capacity(bytes.len().div_ceil(3) * 4);
    for chunk in bytes.chunks(3) {
        let n = (u32::from(chunk[0]) << 16)
            | (u32::from(chunk.get(1).copied().unwrap_or(0)) << 8)
            | u32::from(chunk.get(2).copied().unwrap_or(0));
        let chars = chunk.len() + 1;
        for i in 0..chars {
            out.push(char::from(ALPHABET[((n >> (18 - 6 * i)) & 63) as usize]));
        }
    }
    out
}

/// The verifier (32 random bytes, base64url) and its challenge, `base64url(SHA-256(verifier))`.
fn verifier() -> Result<(Zeroizing<String>, String), BrowserAuthError> {
    let mut raw = Zeroizing::new([0u8; 32]);
    getrandom::fill(raw.as_mut()).map_err(|_| BrowserAuthError::Invalid("no OS randomness"))?;
    let verifier = Zeroizing::new(base64url(raw.as_ref()));
    let challenge = base64url(&dilla_core::identity::sha256(verifier.as_bytes()));
    Ok((verifier, challenge))
}

/// The instance's opaque ids and OIDC assertions: 43 characters of base64url.
fn opaque(s: &str) -> bool {
    s.len() == 43
        && s.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
}

enum Posted {
    Accepted,
    Json(Zeroizing<String>),
}

impl BrowserAuth {
    /// A ceremony that opens `opener`.
    pub fn new(opener: Arc<dyn Opener>, allow_loopback: bool) -> Self {
        Self {
            opener,
            allow_loopback,
            timeout: CEREMONY_TIMEOUT,
            poll: POLL_INTERVAL,
        }
    }

    /// The same, with its own timeout and poll interval (tests).
    pub fn with_timing(mut self, timeout: Duration, poll: Duration) -> Self {
        self.timeout = timeout;
        self.poll = poll;
        self
    }

    /// browser-auth.ts `browserAuth`: asks the instance for a request carrying the verifier's challenge, opens the
    /// browser on its page, then asks for the result every second until the page has finished, at most two minutes.
    pub async fn run(
        &self,
        instance: &Url,
        kind: CeremonyKind,
        options: &str,
    ) -> Result<CeremonyResult, BrowserAuthError> {
        self.run_cancellable(instance, kind, options, &Cancel::new())
            .await
    }

    /// [`BrowserAuth::run`] that `cancel` ends: each request and each wait between polls races it, so after a cancel
    /// nothing more is sent.
    pub async fn run_cancellable(
        &self,
        instance: &Url,
        kind: CeremonyKind,
        options: &str,
        cancel: &Cancel,
    ) -> Result<CeremonyResult, BrowserAuthError> {
        if options.len() > OPTIONS_MAX_BYTES || (kind == CeremonyKind::Oidc && !options.is_empty())
        {
            return Err(BrowserAuthError::Invalid("options"));
        }
        let base = canonical_instance(instance, self.allow_loopback)?
            .join("desktop-auth/")
            .map_err(|_| BrowserAuthError::Invalid("instance URL"))?;
        let deadline = Instant::now() + self.timeout;
        let client = reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(Duration::from_secs(15))
            .build()
            .map_err(|e| BrowserAuthError::Network(e.to_string()))?;
        let (verifier, challenge) = verifier()?;
        let request = serde_json::json!({
            "kind": kind.wire(),
            "options": options,
            "challenge": challenge,
        })
        .to_string();
        let created = match cancel
            .guard(self.post(&client, &base, "request", request, true, deadline))
            .await?
        {
            Posted::Json(v) => v,
            Posted::Accepted => return Err(BrowserAuthError::Invalid("browser reply")),
        };
        let id = serde_json::from_str::<serde_json::Value>(&created)
            .ok()
            .and_then(|v| v.get("id").and_then(|i| i.as_str()).map(str::to_owned))
            .filter(|id| opaque(id))
            .ok_or(BrowserAuthError::Invalid("browser request ID"))?;
        let navigation = base
            .join(&id)
            .map_err(|_| BrowserAuthError::Invalid("browser request ID"))?;
        if cancel.is_cancelled() {
            return Err(BrowserAuthError::Cancelled);
        }
        self.opener
            .open(navigation.as_str())
            .map_err(BrowserAuthError::Opener)?;
        let consume = format!("{id}/consume");
        loop {
            if Instant::now() >= deadline {
                return Err(BrowserAuthError::TimedOut);
            }
            let body =
                Zeroizing::new(serde_json::json!({ "verifier": verifier.as_str() }).to_string());
            match cancel
                .guard(self.post(&client, &base, &consume, body.to_string(), false, deadline))
                .await?
            {
                Posted::Accepted => {}
                Posted::Json(value) => return completion(kind, &value),
            }
            let wait = self
                .poll
                .min(deadline.saturating_duration_since(Instant::now()));
            cancel
                .guard(async {
                    tokio::time::sleep(wait).await;
                    Ok(())
                })
                .await?;
        }
    }

    async fn post(
        &self,
        client: &reqwest::Client,
        base: &Url,
        path: &str,
        body: String,
        first: bool,
        deadline: Instant,
    ) -> Result<Posted, BrowserAuthError> {
        let url = base
            .join(path)
            .map_err(|_| BrowserAuthError::Invalid("instance URL"))?;
        let body = Zeroizing::new(body);
        let left = deadline.saturating_duration_since(Instant::now());
        if left.is_zero() {
            return Err(BrowserAuthError::TimedOut);
        }
        let send = client
            .post(url)
            .header("content-type", "application/json")
            .header("cache-control", "no-store")
            .body(body.as_bytes().to_vec())
            .send();
        let mut response = match tokio::time::timeout(left, send).await {
            Err(_) => return Err(BrowserAuthError::TimedOut),
            Ok(Err(e)) => return Err(BrowserAuthError::Network(e.to_string())),
            Ok(Ok(r)) => r,
        };
        let status = response.status().as_u16();
        if status == 202 {
            return Ok(Posted::Accepted);
        }
        // An instance older than the desktop sign-in has no such route: said so, so the page does not blame the passkey.
        if first && (status == 404 || status == 405) {
            return Err(BrowserAuthError::Unsupported);
        }
        if status == 429 {
            return Err(BrowserAuthError::RateLimited);
        }
        if !(200..300).contains(&status) {
            return Err(BrowserAuthError::Unavailable);
        }
        let mut reply = Zeroizing::new(Vec::new());
        loop {
            let left = deadline.saturating_duration_since(Instant::now());
            let chunk = match tokio::time::timeout(left, response.chunk()).await {
                Err(_) => return Err(BrowserAuthError::TimedOut),
                Ok(Err(e)) => return Err(BrowserAuthError::Network(e.to_string())),
                Ok(Ok(c)) => c,
            };
            let Some(chunk) = chunk else { break };
            if reply.len() + chunk.len() > REPLY_MAX_BYTES {
                return Err(BrowserAuthError::Invalid("browser reply"));
            }
            reply.extend_from_slice(&chunk);
        }
        let text =
            std::str::from_utf8(&reply).map_err(|_| BrowserAuthError::Invalid("browser reply"))?;
        Ok(Posted::Json(Zeroizing::new(text.to_owned())))
    }
}

/// browser-auth.ts's checks of the finished reply: an `error` of the page's (at most 128 characters), else `needs` in
/// 0..=2 with an OIDC assertion of 43 characters or a WebAuthn response of at most 65 536.
fn completion(kind: CeremonyKind, raw: &str) -> Result<CeremonyResult, BrowserAuthError> {
    let invalid = BrowserAuthError::Invalid("browser completion");
    let value: serde_json::Value =
        serde_json::from_str(raw).map_err(|_| BrowserAuthError::Invalid("browser reply"))?;
    let object = value
        .as_object()
        .ok_or(BrowserAuthError::Invalid("browser reply"))?;
    if let Some(error) = object.get("error") {
        return Err(BrowserAuthError::Failed(match error.as_str() {
            Some(e) if e.chars().count() <= 128 => e.to_owned(),
            _ => "Browser sign-in failed".to_owned(),
        }));
    }
    let needs = object
        .get("needs")
        .and_then(serde_json::Value::as_u64)
        .filter(|n| *n <= 2)
        .ok_or(invalid.clone())? as u8;
    if kind == CeremonyKind::Oidc {
        let assertion = object
            .get("assertion")
            .and_then(|a| a.as_str())
            .filter(|a| opaque(a))
            .ok_or(invalid)?;
        return Ok(CeremonyResult {
            response: None,
            assertion: Some(Zeroizing::new(assertion.to_owned())),
            needs,
        });
    }
    let response = object
        .get("response")
        .and_then(|r| r.as_str())
        .filter(|r| r.len() <= OPTIONS_MAX_BYTES)
        .ok_or(invalid)?;
    Ok(CeremonyResult {
        response: Some(Zeroizing::new(response.to_owned())),
        assertion: None,
        needs,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    #[test]
    fn base64url_matches_rfc4648() {
        assert_eq!(base64url(b""), "");
        assert_eq!(base64url(b"f"), "Zg");
        assert_eq!(base64url(b"fo"), "Zm8");
        assert_eq!(base64url(b"foo"), "Zm9v");
        assert_eq!(base64url(&[0xfb, 0xff]), "-_8");
        let (v, c) = verifier().unwrap();
        assert_eq!(v.len(), 43);
        assert_eq!(c, base64url(&dilla_core::identity::sha256(v.as_bytes())));
    }

    #[test]
    fn instances_are_canonical() {
        let u = |s: &str| Url::parse(s).unwrap();
        assert_eq!(
            canonical_instance(&u("https://chat.test/team"), false)
                .unwrap()
                .as_str(),
            "https://chat.test/team/"
        );
        assert!(canonical_instance(&u("http://127.0.0.1:8463/"), false).is_err());
        assert!(canonical_instance(&u("http://127.0.0.1:8463/"), true).is_ok());
        assert!(canonical_instance(&u("http://remote.test/"), true).is_err());
        assert!(canonical_instance(&u("https://a:b@chat.test/"), false).is_err());
        assert!(canonical_instance(&u("https://chat.test/?x=1"), false).is_err());
        assert!(canonical_instance(&u("https://chat.test/assets/"), false).is_err());
        for bad in [
            "https://chat.test/a%2Fb/",
            "https://chat.test/%5c/",
            "https://chat.test/%00/",
            "https://chat.test/a%2Eb/",
        ] {
            assert!(canonical_instance(&u(bad), false).is_err(), "{bad}");
        }
        // An encoded dot segment (`%2E%2e`) is resolved by the URL parser itself before this check: it never survives.
        assert_eq!(u("https://chat.test/%2E%2e/").path(), "/");
    }

    #[test]
    fn completions_are_checked() {
        let ok = completion(
            CeremonyKind::Get,
            r#"{"response":"{\"id\":\"c\"}","needs":0}"#,
        )
        .unwrap();
        assert_eq!(
            ok.response.as_deref().map(|s| s.as_str()),
            Some("{\"id\":\"c\"}")
        );
        assert_eq!(format!("{ok:?}"), "CeremonyResult { needs: 0, .. }");
        let a = "A".repeat(43);
        let oidc = completion(
            CeremonyKind::Oidc,
            &format!(r#"{{"assertion":"{a}","needs":2}}"#),
        )
        .unwrap();
        assert_eq!(oidc.needs, 2);
        assert!(completion(CeremonyKind::Oidc, r#"{"assertion":"short","needs":0}"#).is_err());
        assert!(completion(CeremonyKind::Get, r#"{"response":"x","needs":3}"#).is_err());
        assert_eq!(
            completion(CeremonyKind::Get, r#"{"error":"cancelled"}"#).unwrap_err(),
            BrowserAuthError::Failed("cancelled".into())
        );
        assert_eq!(
            completion(
                CeremonyKind::Get,
                &format!(r#"{{"error":"{}"}}"#, "x".repeat(129))
            )
            .unwrap_err(),
            BrowserAuthError::Failed("Browser sign-in failed".into())
        );
    }

    /// A tiny HTTP server answering each request with the next scripted (status, body); it records each request's path
    /// and body.
    async fn server(script: Vec<(u16, String)>) -> (Url, Arc<Mutex<Vec<(String, String)>>>) {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let seen = Arc::new(Mutex::new(Vec::new()));
        let log = seen.clone();
        tokio::spawn(async move {
            for (status, body) in script {
                let Ok((mut s, _)) = listener.accept().await else {
                    return;
                };
                let mut buf = Vec::new();
                let mut chunk = [0u8; 4096];
                // Read the head and the body by Content-Length.
                loop {
                    let n = s.read(&mut chunk).await.unwrap();
                    if n == 0 {
                        break;
                    }
                    buf.extend_from_slice(&chunk[..n]);
                    let text = String::from_utf8_lossy(&buf).to_string();
                    if let Some(end) = text.find("\r\n\r\n") {
                        let len = text
                            .lines()
                            .find_map(|l| {
                                l.to_ascii_lowercase()
                                    .strip_prefix("content-length:")
                                    .map(|v| v.trim().parse::<usize>().unwrap())
                            })
                            .unwrap_or(0);
                        if buf.len() >= end + 4 + len {
                            let path = text.split_whitespace().nth(1).unwrap().to_owned();
                            log.lock().unwrap().push((path, text[end + 4..].to_owned()));
                            break;
                        }
                    }
                }
                let reply = format!(
                    "HTTP/1.1 {status} X\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
                    body.len()
                );
                s.write_all(reply.as_bytes()).await.unwrap();
                s.shutdown().await.ok();
            }
        });
        (
            Url::parse(&format!("http://127.0.0.1:{}/team", addr.port())).unwrap(),
            seen,
        )
    }

    struct Recorder(Mutex<Vec<String>>);

    impl Opener for Recorder {
        fn open(&self, url: &str) -> Result<(), String> {
            self.0.lock().unwrap().push(url.to_owned());
            Ok(())
        }
    }

    /// browser-auth.test.ts "browser ceremony uses fixed instance, private PKCE and single-use POST completion".
    #[tokio::test]
    async fn ceremony_uses_fixed_instance_private_pkce_and_post_completion() {
        let id = "A".repeat(43);
        let (base, seen) = server(vec![
            (201, format!(r#"{{"id":"{id}"}}"#)),
            (202, String::new()),
            (
                200,
                r#"{"response":"{\"id\":\"credential\"}","needs":0}"#.into(),
            ),
        ])
        .await;
        let opened = Arc::new(Recorder(Mutex::new(Vec::new())));
        let auth = BrowserAuth::new(opened.clone(), true)
            .with_timing(Duration::from_secs(10), Duration::from_millis(10));
        let result = auth
            .run(
                &base,
                CeremonyKind::Get,
                r#"{"publicKey":{"challenge":"AAAAAAAAAAAAAAAAAAAAAA"}}"#,
            )
            .await
            .unwrap();
        assert_eq!(result.response.unwrap().as_str(), r#"{"id":"credential"}"#);
        let opened = opened.0.lock().unwrap().clone();
        assert_eq!(opened, vec![format!("{base}/desktop-auth/{id}")]);
        let seen = seen.lock().unwrap().clone();
        assert_eq!(seen[0].0, "/team/desktop-auth/request");
        assert_eq!(seen[1].0, format!("/team/desktop-auth/{id}/consume"));
        let request: serde_json::Value = serde_json::from_str(&seen[0].1).unwrap();
        let verifier: serde_json::Value = serde_json::from_str(&seen[1].1).unwrap();
        let verifier = verifier["verifier"].as_str().unwrap();
        assert_eq!(request["kind"], "get");
        assert_eq!(
            request["challenge"].as_str().unwrap(),
            base64url(&dilla_core::identity::sha256(verifier.as_bytes()))
        );
        assert!(!opened[0].contains(verifier));
    }

    /// browser-auth.test.ts "browser handoff rejects malformed replies and arbitrary capability arguments".
    #[tokio::test]
    async fn malformed_replies_never_open_the_browser() {
        let (base, _) = server(vec![(201, r#"{"id":"https://evil.test"}"#.into())]).await;
        let opened = Arc::new(Recorder(Mutex::new(Vec::new())));
        let auth = BrowserAuth::new(opened.clone(), true);
        assert_eq!(
            auth.run(&base, CeremonyKind::Oidc, "").await.unwrap_err(),
            BrowserAuthError::Invalid("browser request ID")
        );
        assert_eq!(
            auth.run(&base, CeremonyKind::Oidc, "{}").await.unwrap_err(),
            BrowserAuthError::Invalid("options")
        );
        assert!(opened.0.lock().unwrap().is_empty());
    }

    /// browser-auth.test.ts: 404/405 on the first request is unsupported; 429 rate-limited; a later 404 unavailable.
    #[tokio::test]
    async fn unsupported_rate_limited_and_unavailable() {
        for status in [404, 405] {
            let (base, _) = server(vec![(status, String::new())]).await;
            let auth = BrowserAuth::new(Arc::new(Recorder(Mutex::new(Vec::new()))), true);
            assert_eq!(
                auth.run(&base, CeremonyKind::Get, "{}").await.unwrap_err(),
                BrowserAuthError::Unsupported
            );
        }
        let (base, _) = server(vec![(429, String::new())]).await;
        let auth = BrowserAuth::new(Arc::new(Recorder(Mutex::new(Vec::new()))), true);
        assert_eq!(
            auth.run(&base, CeremonyKind::Get, "{}").await.unwrap_err(),
            BrowserAuthError::RateLimited
        );
        let id = "A".repeat(43);
        let (base, _) = server(vec![
            (201, format!(r#"{{"id":"{id}"}}"#)),
            (404, String::new()),
        ])
        .await;
        assert_eq!(
            auth.run(&base, CeremonyKind::Get, "{}").await.unwrap_err(),
            BrowserAuthError::Unavailable
        );
    }

    /// browser-auth.test.ts loopback rule: an `http://` loopback instance only when allowed.
    #[tokio::test]
    async fn loopback_needs_the_development_allowance() {
        let (base, _) = server(vec![]).await;
        let auth = BrowserAuth::new(Arc::new(Recorder(Mutex::new(Vec::new()))), false);
        assert!(matches!(
            auth.run(&base, CeremonyKind::Get, "{}").await.unwrap_err(),
            BrowserAuthError::Invalid(_)
        ));
    }

    /// A cancelled ceremony sends nothing more and answers Cancelled (no response reaches a finish).
    #[tokio::test]
    async fn a_cancelled_ceremony_sends_nothing_more() {
        let id = "A".repeat(43);
        let mut script = vec![(201, format!(r#"{{"id":"{id}"}}"#))];
        script.extend((0..200).map(|_| (202, String::new())));
        let (base, seen) = server(script).await;
        let auth = BrowserAuth::new(Arc::new(Recorder(Mutex::new(Vec::new()))), true)
            .with_timing(Duration::from_secs(30), Duration::from_millis(50));
        let cancel = Cancel::new();
        let stop = cancel.clone();
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(300)).await;
            stop.cancel();
        });
        let started = std::time::Instant::now();
        assert_eq!(
            auth.run_cancellable(&base, CeremonyKind::Get, "{}", &cancel)
                .await
                .unwrap_err(),
            BrowserAuthError::Cancelled
        );
        assert!(started.elapsed() < Duration::from_secs(2));
        let after = seen.lock().unwrap().len();
        tokio::time::sleep(Duration::from_millis(400)).await;
        assert_eq!(
            seen.lock().unwrap().len(),
            after,
            "no request after the cancel"
        );
        // Cancelled before it starts: nothing is sent at all.
        let (base, seen) = server(vec![(201, format!(r#"{{"id":"{id}"}}"#))]).await;
        assert_eq!(
            auth.run_cancellable(&base, CeremonyKind::Get, "{}", &cancel)
                .await
                .unwrap_err(),
            BrowserAuthError::Cancelled
        );
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert!(seen.lock().unwrap().is_empty());
    }

    #[tokio::test]
    async fn a_page_that_never_finishes_times_out() {
        let id = "A".repeat(43);
        let mut script = vec![(201, format!(r#"{{"id":"{id}"}}"#))];
        script.extend((0..50).map(|_| (202, String::new())));
        let (base, _) = server(script).await;
        let auth = BrowserAuth::new(Arc::new(Recorder(Mutex::new(Vec::new()))), true)
            .with_timing(Duration::from_millis(300), Duration::from_millis(20));
        assert_eq!(
            auth.run(&base, CeremonyKind::Get, "{}").await.unwrap_err(),
            BrowserAuthError::TimedOut
        );
    }
}
