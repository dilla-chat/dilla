//! `diagnostics`: Task 24's slice of [`super::AppState`]: the stable error codes, the slowest socket routes, the key
//! package status and the app build the instance announced.
//!
//! What is here is what `packages/web`'s Diagnostics screen may show and its Copy may write: stable codes and counts,
//! never a detail text, a key, a token or message plaintext (L-TS-43). The connection facts (status, generation, wire
//! version) stay in [`super::ConnectionState`]; the screen reads them from there.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

/// How many error codes are kept (controller.ts `recordError`).
pub const ERROR_LIMIT: usize = 20;
/// How many slow routes are shown (controller.ts `publishDiagnostics`).
pub const SLOW_ROUTE_LIMIT: usize = 5;
/// The samples kept per route pattern (route-rtt.ts `RTT_SAMPLES`).
pub const RTT_SAMPLES: usize = 128;
/// The patterns kept (route-rtt.ts `RTT_PATTERNS`).
pub const RTT_PATTERNS: usize = 512;

/// One recorded error: its stable code and when it happened (ms since the epoch).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ErrorEntry {
    pub code: String,
    pub at_ms: u64,
}

/// One socket route's timing (route-rtt.ts `RouteTiming`): its pattern (never a path with ids), the median and 95th
/// percentile of its recent round trips in ms, and how many were recorded in all.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RouteTiming {
    pub pattern: String,
    pub p50: u64,
    pub p95: u64,
    pub count: u64,
}

/// The key package status (`DiagnosticsState.keyPackages`): the count the server holds, when a publish was last tried and
/// the code it failed with. Nothing is known until the key package maintenance reports (Task 2's
/// `account::refill_key_packages`).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct KeyPackageStatus {
    pub count: Option<u32>,
    pub last_attempt_ms: Option<u64>,
    pub last_error: Option<String>,
}

/// The nearest-rank percentile of sorted, non-empty samples.
fn percentile(sorted: &[u64], p: f64) -> u64 {
    let n = sorted.len();
    let rank = (p * n as f64).ceil() as usize;
    sorted[rank.saturating_sub(1).min(n - 1)]
}

#[derive(Debug)]
struct Samples {
    recent: Vec<u64>,
    next: usize,
    count: u64,
}

/// The round trip of each socket request, kept per route pattern (route-rtt.ts `RouteRtt`).
#[derive(Debug, Default)]
pub struct RouteRtt {
    samples: HashMap<String, Samples>,
    /// The top rows last handed out by [`RouteRtt::record_changing`].
    published: Vec<RouteTiming>,
}

impl RouteRtt {
    /// Records one round trip. A non-finite or negative time is ignored, and a pattern past [`RTT_PATTERNS`] is not kept.
    pub fn record(&mut self, pattern: &str, ms: f64) {
        if !ms.is_finite() || ms < 0.0 {
            return;
        }
        if !self.samples.contains_key(pattern) && self.samples.len() >= RTT_PATTERNS {
            return;
        }
        let entry = self.samples.entry(pattern.to_owned()).or_insert(Samples {
            recent: Vec::new(),
            next: 0,
            count: 0,
        });
        let rounded = ms.round() as u64;
        if entry.recent.len() < RTT_SAMPLES {
            entry.recent.push(rounded);
        } else {
            entry.recent[entry.next] = rounded;
        }
        entry.next = (entry.next + 1) % RTT_SAMPLES;
        entry.count += 1;
    }

    /// [`RouteRtt::record`], then the slowest `n` rows when they differ from the ones this last answered (`None` when the
    /// shown list did not change, so the caller publishes nothing).
    pub fn record_changing(
        &mut self,
        pattern: &str,
        ms: f64,
        n: usize,
    ) -> Option<Vec<RouteTiming>> {
        self.record(pattern, ms);
        let top = self.slowest(n);
        if top == self.published {
            return None;
        }
        self.published.clone_from(&top);
        Some(top)
    }

    /// The `n` patterns with the highest p95 over their recent samples, slowest first, ties by pattern. `count` is every
    /// request recorded for the pattern.
    pub fn slowest(&self, n: usize) -> Vec<RouteTiming> {
        let mut rows: Vec<RouteTiming> = self
            .samples
            .iter()
            .map(|(pattern, entry)| {
                let mut sorted = entry.recent.clone();
                sorted.sort_unstable();
                RouteTiming {
                    pattern: pattern.clone(),
                    p50: percentile(&sorted, 0.5),
                    p95: percentile(&sorted, 0.95),
                    count: entry.count,
                }
            })
            .collect();
        rows.sort_by(|a, b| b.p95.cmp(&a.p95).then_with(|| a.pattern.cmp(&b.pattern)));
        rows.truncate(n);
        rows
    }
}

/// The diagnostics slice. [`Default`] is the empty slice: no errors, nothing known.
#[derive(Clone, Debug, Default)]
pub struct DiagnosticsState {
    /// The last [`ERROR_LIMIT`] stable codes, oldest first.
    pub errors: Vec<ErrorEntry>,
    /// The slowest [`SLOW_ROUTE_LIMIT`] socket routes, slowest first.
    pub slow_routes: Vec<RouteTiming>,
    pub key_packages: KeyPackageStatus,
    /// The verified entry hint a wire-5+ socket announced (op 56, `/assets/<name>.js`).
    pub app_build: Option<String>,
    /// The instance's name, for the instance fact and the connecting line; `None` until the host app says.
    pub instance_name: Option<String>,
    /// The live (not revoked) devices of this account, once known.
    pub devices: Option<u32>,
    /// The samples behind `slow_routes`; shared by every snapshot, not part of what the UI compares.
    pub(crate) rtt: Arc<Mutex<RouteRtt>>,
}

impl PartialEq for DiagnosticsState {
    fn eq(&self, other: &Self) -> bool {
        self.errors == other.errors
            && self.slow_routes == other.slow_routes
            && self.key_packages == other.key_packages
            && self.app_build == other.app_build
            && self.instance_name == other.instance_name
            && self.devices == other.devices
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn slowest_is_by_p95_then_pattern_and_counts_every_request() {
        let mut rtt = RouteRtt::default();
        for ms in [10.0, 20.0, 30.0, 40.0] {
            rtt.record("GET /v1/a", ms);
        }
        rtt.record("GET /v1/b", 500.0);
        rtt.record("GET /v1/c", 500.0);
        rtt.record("GET /v1/a", f64::NAN);
        rtt.record("GET /v1/a", -1.0);
        let top = rtt.slowest(2);
        assert_eq!(top[0].pattern, "GET /v1/b");
        assert_eq!(top[1].pattern, "GET /v1/c");
        let all = rtt.slowest(5);
        let a = all.iter().find(|r| r.pattern == "GET /v1/a").unwrap();
        // nearest rank: p50 of four samples is the second, p95 the fourth.
        assert_eq!((a.p50, a.p95, a.count), (20, 40, 4));
    }

    #[test]
    fn a_record_that_leaves_the_shown_rows_alone_answers_none() {
        let mut rtt = RouteRtt::default();
        let first = rtt.record_changing("GET /v1/a", 100.0, 1).unwrap();
        assert_eq!(first[0].pattern, "GET /v1/a");
        // The same p95 and a second count: the rows differ by `count`, so it is a change.
        assert!(rtt.record_changing("GET /v1/a", 100.0, 1).is_some());
        // A faster route past the top one: the shown row is unchanged.
        assert_eq!(rtt.record_changing("GET /v1/b", 1.0, 1), None);
    }

    #[test]
    fn old_samples_drop_out_but_the_count_keeps_growing() {
        let mut rtt = RouteRtt::default();
        rtt.record("GET /v1/a", 9000.0);
        for _ in 0..RTT_SAMPLES {
            rtt.record("GET /v1/a", 5.0);
        }
        let row = &rtt.slowest(1)[0];
        assert_eq!((row.p95, row.count), (5, RTT_SAMPLES as u64 + 1));
    }

    #[test]
    fn patterns_past_the_cap_are_not_recorded() {
        let mut rtt = RouteRtt::default();
        for i in 0..RTT_PATTERNS + 5 {
            rtt.record(&format!("GET /v1/{i}"), 1.0);
        }
        assert_eq!(rtt.slowest(usize::MAX).len(), RTT_PATTERNS);
    }
}
