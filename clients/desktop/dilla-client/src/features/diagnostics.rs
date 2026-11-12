//! Area **diagnostics** (Task 24): the stable error codes, the slowest socket routes, the connection and wire facts, the
//! app build the instance announces and the text Copy and Export write.
//!
//! Ports: packages/web/src/screens/settings/Diagnostics.tsx, packages/client-core/src/http/route-rtt.ts, the diagnostics
//! slice of packages/client-core/src/worker/controller.ts, packages/web/src/{update-view,update-checkpoint}.ts.
//!
//! Receives op 56 `app_build` from the gateway (`crate::sync::route`), through [`AreaFrames`].
//!
//! Nothing a person could be hurt by leaving the device goes through here (L-TS-43): a code is stable (`E_...`), a route is
//! a pattern without ids, a count is a count. A recorded "code" that is not that shape is replaced by `E_UNKNOWN`, so a
//! detail text, a key or a token handed in by mistake never reaches the screen, Copy or Export.
//!
//! Who calls what: the engine and the other areas call [`Diagnostics::record_error`] where the web controller calls
//! `recordError`; `sync/requests.rs` (Task 3) calls [`Diagnostics::record_route`] when a socket response settles; Task 2's
//! key package maintenance calls [`Diagnostics::set_key_packages`]; Task 16's device list calls
//! [`Diagnostics::set_devices`]; the app calls [`Diagnostics::set_instance_name`] once the instance is known.

use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use crate::client::{Client, Inner};
use crate::gateway::frames::{Frame, Value, valid_app_entry};
use crate::state::AppState;
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

/// A route pattern as the socket route table writes it (`GET /v1/channels/{id}`): printable ASCII without a query, a
/// space-separated method and path. Anything else is not recorded, so a path with real ids never lands on the screen.
fn is_pattern(pattern: &str) -> bool {
    pattern.len() <= 200
        && pattern.bytes().all(|b| b.is_ascii_graphic() || b == b' ')
        && !pattern.contains('?')
        && pattern.split(' ').count() == 2
}

impl Diagnostics {
    /// Records one error code (controller.ts `recordError`): the last [`ERROR_LIMIT`] are kept.
    pub fn record_error(&self, code: &str) {
        let entry = ErrorEntry {
            code: stable_code(code).to_owned(),
            at_ms: now_ms(),
        };
        self.inner.publish(move |s| {
            let errors = &mut s.diagnostics.errors;
            errors.push(entry);
            if errors.len() > ERROR_LIMIT {
                errors.remove(0);
            }
        });
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
    pub fn set_key_packages(&self, status: KeyPackageStatus) {
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

/// What Export writes: the facts the screen shows, as `key=value` lines, then the slowest routes and the codes. It reads
/// only [`AppState::connection`], [`AppState::diagnostics`] and the account's phase: no key, token, recovery key, message
/// or member name can be in it by construction (the test `export_has_no_secrets` pins that with canaries).
pub fn export_text(state: &AppState) -> String {
    use std::fmt::Write;
    let d = &state.diagnostics;
    let mut out = String::from("dilla diagnostics v1\n");
    let _ = writeln!(out, "connection={:?}", state.connection.status);
    let _ = writeln!(
        out,
        "generation={}",
        state
            .connection
            .generation
            .map_or_else(|| "-".to_owned(), |g| g.to_string())
    );
    let _ = writeln!(
        out,
        "wire={}",
        state
            .connection
            .wire_version
            .map_or_else(|| "-".to_owned(), |w| w.to_string())
    );
    let _ = writeln!(out, "readies={}", state.connection.readies);
    let _ = writeln!(out, "account={:?}", state.account.phase);
    let _ = writeln!(
        out,
        "devices={}",
        d.devices.map_or("-".into(), |n| n.to_string())
    );
    let _ = writeln!(
        out,
        "keyPackages={} lastAttemptMs={} lastError={}",
        d.key_packages.count.map_or("-".into(), |n| n.to_string()),
        d.key_packages
            .last_attempt_ms
            .map_or("-".into(), |n| n.to_string()),
        d.key_packages
            .last_error
            .as_deref()
            .map_or("-", stable_code),
    );
    for r in &d.slow_routes {
        let _ = writeln!(
            out,
            "route {} p50={} p95={} count={}",
            r.pattern, r.p50, r.p95, r.count
        );
    }
    for e in d.errors.iter().rev().take(ERROR_LIMIT).rev() {
        let _ = writeln!(out, "error {} atMs={}", e.code, e.at_ms);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::AccountState;
    use crate::state::diagnostics::RouteTiming;

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

    #[test]
    fn export_has_no_secrets() {
        let mut s = state();
        // A recorded value is only ever a stable code, a pattern, a count: even a hostile one cannot carry text out.
        s.diagnostics.errors.push(ErrorEntry {
            code: stable_code("recovery key: aaaa bbbb").to_owned(),
            at_ms: 3,
        });
        s.diagnostics.key_packages.last_error = Some("token=secret-token".into());
        let out = export_text(&s);
        for canary in [
            "canary-username",
            "secret-token",
            "recovery",
            "aaaa",
            "token",
        ] {
            assert!(!out.contains(canary), "{canary} in {out}");
        }
        assert!(out.contains("error E_UNKNOWN atMs=3"));
        assert!(out.contains("route GET /v1/x/{id} p50=3 p95=9 count=4"));
    }
}
