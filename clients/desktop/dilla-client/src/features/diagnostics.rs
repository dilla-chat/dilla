//! Area **diagnostics** (Task 24): the stable error codes, the slowest socket routes, the connection and wire facts, the
//! app build the instance announces and the text Copy writes.
//!
//! Ports: packages/web/src/screens/settings/Diagnostics.tsx, packages/client-core/src/http/route-rtt.ts, the diagnostics
//! slice of packages/client-core/src/worker/controller.ts, packages/web/src/{update-view,update-checkpoint}.ts.
//!
//! Receives op 56 `app_build` from the gateway (`crate::sync::route`), through [`AreaFrames`].
//!
//! Nothing a person could be hurt by leaving the device goes through here (L-TS-43): a code is stable (`E_...`), a route is
//! a pattern without ids, a count is a count. A recorded "code" that is not that shape is replaced by `E_UNKNOWN`, so a
//! detail text, a key or a token handed in by mistake never reaches the screen or Copy.
//!
//! Who calls what: the engine and the other areas call [`Diagnostics::record_error`] where the web controller calls
//! `recordError`; `sync/requests.rs` (Task 3) calls [`Diagnostics::record_route`] when a socket response settles; Task 2's
//! key package maintenance calls [`Diagnostics::set_key_packages`]; Task 16's device list calls
//! [`Diagnostics::set_devices`]; the app calls [`Diagnostics::set_instance_name`] once the instance is known.

use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use crate::client::{Client, Inner};
use crate::gateway::frames::{Frame, Value, valid_app_entry};
use crate::state::diagnostics::{
    DiagnosticsState, ERROR_LIMIT, ErrorEntry, KeyPackageStatus, SLOW_ROUTE_LIMIT,
};
use crate::sync::route::AreaFrames;

/// What an unsafe or malformed code is recorded as.
pub const UNKNOWN_CODE: &str = "E_UNKNOWN";

/// The diagnostics area's handle: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Diagnostics {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Diagnostics`] handle.
    pub fn diagnostics(&self) -> Diagnostics {
        Diagnostics {
            inner: self.inner().clone(),
        }
    }
}

impl Diagnostics {
    /// The handle for the client's own hooks (the Api's refusal hook, the socket's route timings).
    pub(crate) fn of(inner: Arc<Inner>) -> Self {
        Self { inner }
    }
}

/// The stable code a failed command is recorded under: the client's own code where it has one, else a fixed one per kind.
/// Never the error's text.
pub(crate) fn client_error_code(error: &crate::ClientError) -> &str {
    use crate::ClientError as E;
    match error {
        E::Store(_) => "E_STORE",
        E::NoIdentity => "E_NO_IDENTITY",
        E::Revoked => "E_REVOKED",
        E::FactorRequired => "E_FACTOR_REQUIRED",
        E::Network(_) => "E_NETWORK",
        E::Sync(code) => stable_code(code),
        E::UnknownChannel => "E_UNKNOWN_CHANNEL",
        E::Stopped => "E_STOPPED",
        E::Runtime(_) => "E_RUNTIME",
    }
}

/// The state change [`Diagnostics::record_error`] publishes: one more error, the last [`ERROR_LIMIT`] kept.
pub(crate) fn error_update(
    code: &str,
) -> impl FnOnce(&mut crate::state::AppState) + Send + 'static {
    let entry = ErrorEntry {
        code: stable_code(code).to_owned(),
        at_ms: now_ms(),
    };
    move |s| {
        let errors = &mut s.diagnostics.errors;
        errors.push(entry);
        if errors.len() > ERROR_LIMIT {
            errors.remove(0);
        }
    }
}

fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| u64::try_from(d.as_millis()).unwrap_or(u64::MAX))
}

/// `code` when it has the shape of a stable code (an upper-case letter, then upper-case letters, digits and `_`, at most
/// 64 characters), else [`UNKNOWN_CODE`].
pub fn stable_code(code: &str) -> &str {
    let mut bytes = code.bytes();
    let shaped = code.len() <= 64
        && bytes.next().is_some_and(|b| b.is_ascii_uppercase())
        && bytes.all(|b| b.is_ascii_uppercase() || b.is_ascii_digit() || b == b'_');
    if shaped { code } else { UNKNOWN_CODE }
}

/// One literal segment of a route pattern: a lower-case word (`[a-z][a-z0-9_-]*`, at most 24 characters) that is not made
/// only of hex digits and has digits only as a version (`v1`): anything else is what an id or a token looks like.
fn is_literal(seg: &str) -> bool {
    let mut bytes = seg.bytes();
    seg.len() <= 24
        && bytes.next().is_some_and(|b| b.is_ascii_lowercase())
        && bytes.all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_' || b == b'-')
        && !seg.bytes().all(|b| b.is_ascii_hexdigit())
        // A digit belongs only to a version segment (`v1`): a word with digits in it is an opaque token.
        && (!seg.bytes().any(|b| b.is_ascii_digit())
            || seg.strip_prefix('v').is_some_and(|n| !n.is_empty() && n.bytes().all(|b| b.is_ascii_digit())))
}

/// A route pattern as the socket route table writes it (`GET /v1/channels/{id}`): a method, a space and a path of
/// segments that are each a literal ([`is_literal`]) or a placeholder `{name}` (`name` in `[a-z_]+`). A path with a real
/// id or token in it (a hex id, a digit string, an invite code) is not a pattern and is never recorded, so it cannot land
/// on the screen. The method is upper-case letters.
fn is_pattern(pattern: &str) -> bool {
    let Some((method, path)) = pattern.split_once(' ') else {
        return false;
    };
    let placeholder = |seg: &str| {
        seg.strip_prefix('{')
            .and_then(|r| r.strip_suffix('}'))
            .is_some_and(|name| {
                !name.is_empty() && name.bytes().all(|b| b.is_ascii_lowercase() || b == b'_')
            })
    };
    pattern.len() <= 200
        && !method.is_empty()
        && method.bytes().all(|b| b.is_ascii_uppercase())
        && path.strip_prefix('/').is_some_and(|rest| {
            rest.split('/')
                .all(|seg| is_literal(seg) || placeholder(seg))
        })
}

impl Diagnostics {
    /// Records one error code (controller.ts `recordError`): the last [`ERROR_LIMIT`] are kept.
    pub fn record_error(&self, code: &str) {
        self.inner.publish(error_update(code));
    }

    /// Records the round trip of one socket request, by route pattern (`GET /v1/x/{id}`). It publishes only when the
    /// slowest [`SLOW_ROUTE_LIMIT`] change, so a busy socket does not wake the UI on every response.
    pub fn record_route(&self, pattern: &str, ms: f64) {
        if !is_pattern(pattern) {
            return;
        }
        let top = {
            let current = self.inner.snapshot();
            let mut rtt = current
                .diagnostics
                .rtt
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            rtt.record_changing(pattern, ms, SLOW_ROUTE_LIMIT)
        };
        if let Some(top) = top {
            self.inner.publish(move |s| s.diagnostics.slow_routes = top);
        }
    }

    /// The key package status, as the maintenance last knew it (Task 2).
    ///
    /// `last_error` goes through [`stable_code`] before it is stored. `stable_code` assumes callers pass real codes: it
    /// checks the shape, not the meaning, so a secret formatted like a code would pass; call sites pass `E_*` strings only.
    pub fn set_key_packages(&self, mut status: KeyPackageStatus) {
        status.last_error = status.last_error.map(|c| stable_code(&c).to_owned());
        self.inner
            .publish(move |s| s.diagnostics.key_packages = status);
    }

    /// The live device count of this account (Task 16's device list).
    pub fn set_devices(&self, devices: u32) {
        self.inner
            .publish(move |s| s.diagnostics.devices = Some(devices));
    }

    /// The instance's name, as the instance list holds it.
    pub fn set_instance_name(&self, name: &str) {
        let name = name.to_owned();
        self.inner
            .publish(move |s| s.diagnostics.instance_name = Some(name));
    }
}

/// The entry hint an op 56 `app_build` frame carries, when it is an `/assets/<name>.js` entry.
pub fn app_build_of(f: &Frame) -> Option<String> {
    match f.payload.first() {
        Some(Value::Text(entry)) if valid_app_entry(entry) => Some(entry.clone()),
        _ => None,
    }
}

impl AreaFrames for Diagnostics {
    /// Op 56 `app_build`: the verified entry hint (`setConnection({ appBuild })`). A hint that is not an
    /// `/assets/<name>.js` entry is dropped (the frame validator already refuses it; this is the second check).
    fn on_frame(inner: &Arc<Inner>, f: &Frame) {
        if let Some(entry) = app_build_of(f) {
            inner.publish(move |s| s.diagnostics.app_build = Some(entry));
        }
    }
}

/// What Copy writes: the stable codes, one per line, and nothing else (Diagnostics.tsx). Empty when there are none.
pub fn copy_text(state: &DiagnosticsState) -> String {
    state
        .errors
        .iter()
        .rev()
        .take(ERROR_LIMIT)
        .rev()
        .map(|e| e.code.as_str())
        .collect::<Vec<_>>()
        .join("\n")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::diagnostics::RouteTiming;
    use crate::state::{AccountState, AppState};

    #[test]
    fn a_stable_code_passes_and_anything_else_is_unknown() {
        assert_eq!(stable_code("E_STORE_NEWER"), "E_STORE_NEWER");
        assert_eq!(stable_code("E_X1"), "E_X1");
        for bad in [
            "",
            "e_lower",
            "E_HAS SPACE",
            "token=abc123",
            "E_\u{e9}",
            &"E".repeat(65),
            "1_E",
        ] {
            assert_eq!(stable_code(bad), UNKNOWN_CODE, "{bad}");
        }
    }

    #[test]
    fn patterns_carry_no_ids_or_queries() {
        assert!(is_pattern("GET /v1/channels/{id}"));
        assert!(!is_pattern("GET /v1/channels/{id}?x=1"));
        assert!(!is_pattern("/v1/channels"));
        assert!(!is_pattern("GET /v1/a b"));
        // A real id or token in the path is not a pattern.
        assert!(!is_pattern("GET /v1/invites/Zk3x9QmT2vPLw8"));
        assert!(!is_pattern("GET /v1/invites/zk3x9qmt2vplw8"));
        assert!(!is_pattern(
            "GET /v1/users/0123456789abcdef0123456789abcdef"
        ));
        assert!(!is_pattern("GET /v1/users/deadbeef"));
        assert!(!is_pattern("GET /v1/channels/12345"));
        assert!(is_pattern("GET /v1/users/{user_id}/device-list"));
        assert!(!is_pattern("GET /v1/users/{UserId}"));
        assert!(!is_pattern(&format!("GET /v1/{}", "a".repeat(25))));
    }

    fn state() -> AppState {
        let mut s = AppState {
            account: AccountState {
                username: "canary-username".into(),
                ..AccountState::default()
            },
            ..AppState::default()
        };
        s.diagnostics.errors = vec![
            ErrorEntry {
                code: "E_A".into(),
                at_ms: 1,
            },
            ErrorEntry {
                code: "E_B".into(),
                at_ms: 2,
            },
        ];
        s.diagnostics.slow_routes = vec![RouteTiming {
            pattern: "GET /v1/x/{id}".into(),
            p50: 3,
            p95: 9,
            count: 4,
        }];
        s.diagnostics.instance_name = Some("home".into());
        s
    }

    fn frame(payload: Vec<Value>) -> Frame {
        Frame {
            op: 56,
            n: 0,
            group_id: None,
            payload,
        }
    }

    #[test]
    fn only_an_asset_entry_is_an_app_build() {
        let ok = frame(vec![Value::Text("/assets/entry-abc123.js".into())]);
        assert_eq!(
            app_build_of(&ok).as_deref(),
            Some("/assets/entry-abc123.js")
        );
        for bad in ["/assets/../bad.js", "/other/x.js", "/assets/x.css", ""] {
            assert_eq!(
                app_build_of(&frame(vec![Value::Text(bad.into())])),
                None,
                "{bad}"
            );
        }
        assert_eq!(app_build_of(&frame(vec![Value::Uint(1)])), None);
        assert_eq!(app_build_of(&frame(vec![])), None);
    }

    #[test]
    fn copy_writes_the_codes_one_per_line_and_nothing_else() {
        assert_eq!(copy_text(&state().diagnostics), "E_A\nE_B");
        assert_eq!(copy_text(&DiagnosticsState::default()), "");
    }
}
