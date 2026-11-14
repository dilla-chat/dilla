//! Op 63's hints (packages/client-core/src/bot/hints.ts `BotHints`): a hint carries no content, it says a scope has a
//! newer revision and the client fetches once. This only decides when: a revision not newer than the last one seen is
//! dropped, and a scope is fetched at most once per [`HINT_COALESCE`]; a hint inside that window leaves one trailing
//! fetch at its end, which reads whatever is newest by then (Task 28 item 15).

use std::collections::{HashMap, HashSet};
use std::time::{Duration, Instant};

/// hints.ts `HINT_COALESCE_MS`: at most one fetch per scope in this window.
pub const HINT_COALESCE: Duration = Duration::from_millis(250);

/// An op 63 scope: its kind (0 the user's own, 1 a channel, 2 a call) and id.
pub type Scope = (u8, [u8; 16]);

/// What to do with a hint.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Hint {
    /// Nothing: an old revision, or a trailing fetch is already due.
    Drop,
    /// Fetch now.
    Now,
    /// Fetch once this has passed (the trailing fetch); then [`HintGate::due`].
    After(Duration),
}

/// One client's op 63 bookkeeping.
#[derive(Debug, Default)]
pub struct HintGate {
    seen: HashMap<Scope, u64>,
    last_fetch: HashMap<Scope, Instant>,
    pending: HashSet<Scope>,
}

impl HintGate {
    /// hints.ts `onHint` and `request`: a hint of `scope` at `revision`, arriving at `now`.
    pub fn on_hint(&mut self, scope: Scope, revision: u64, now: Instant) -> Hint {
        if self.seen.get(&scope).is_some_and(|held| revision <= *held) {
            return Hint::Drop;
        }
        self.seen.insert(scope, revision);
        if self.pending.contains(&scope) {
            // The trailing fetch reads whatever is newest.
            return Hint::Drop;
        }
        let wait = self.last_fetch.get(&scope).map_or(Duration::ZERO, |last| {
            (*last + HINT_COALESCE).saturating_duration_since(now)
        });
        if wait.is_zero() {
            self.last_fetch.insert(scope, now);
            Hint::Now
        } else {
            self.pending.insert(scope);
            Hint::After(wait)
        }
    }

    /// The trailing fetch of `scope` runs at `now`.
    pub fn due(&mut self, scope: Scope, now: Instant) {
        self.pending.remove(&scope);
        self.last_fetch.insert(scope, now);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const S: Scope = (0, [1; 16]);
    const T: Scope = (2, [2; 16]);

    /// hints.test.ts: one fetch per scope per 250 ms, a trailing one for a burst, old revisions dropped.
    #[test]
    fn a_burst_of_hints_fetches_once_then_once_more() {
        let mut g = HintGate::default();
        let t0 = Instant::now();
        assert_eq!(g.on_hint(S, 1, t0), Hint::Now);
        assert_eq!(g.on_hint(S, 1, t0), Hint::Drop, "the same revision");
        let at = t0 + Duration::from_millis(100);
        assert_eq!(
            g.on_hint(S, 2, at),
            Hint::After(Duration::from_millis(150))
        );
        assert_eq!(
            g.on_hint(S, 3, at + Duration::from_millis(10)),
            Hint::Drop,
            "the trailing fetch reads the newest"
        );
        assert_eq!(g.on_hint(T, 1, at), Hint::Now, "another scope is its own");
        g.due(S, t0 + HINT_COALESCE);
        assert_eq!(
            g.on_hint(S, 4, t0 + HINT_COALESCE + Duration::from_millis(1)),
            Hint::After(HINT_COALESCE - Duration::from_millis(1))
        );
        assert_eq!(g.on_hint(S, 2, t0 + Duration::from_secs(9)), Hint::Drop);
        g.due(S, t0 + Duration::from_secs(1));
        assert_eq!(g.on_hint(S, 5, t0 + Duration::from_secs(2)), Hint::Now);
    }
}
