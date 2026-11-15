//! A bounded least-recently-used memo, as markdown.ts keeps for `parseTree` (`CACHE_LIMITS`): at most 200 entries and
//! two million characters of keys, and a key longer than 4000 characters is never kept. The log is drawn again on every
//! state change and every tick, so what hostile input costs to parse, highlight or lay out is paid once, not per frame.

use std::collections::HashMap;
use std::sync::{Arc, Mutex, PoisonError};

/// markdown.ts `CACHE_LIMITS`.
pub const ENTRIES: usize = 200;
pub const CHARS: usize = 2_000_000;
pub const BODY: usize = 4000;

/// The memo: values are shared, so a hit costs a pointer.
pub struct Lru<V> {
    entries: usize,
    chars: usize,
    body: usize,
    used: usize,
    tick: u64,
    map: HashMap<String, (u64, Arc<V>)>,
}

impl<V> Lru<V> {
    pub fn new(entries: usize, chars: usize, body: usize) -> Self {
        Self {
            entries,
            chars,
            body,
            used: 0,
            tick: 0,
            map: HashMap::new(),
        }
    }

    /// The web's limits.
    pub fn web() -> Self {
        Self::new(ENTRIES, CHARS, BODY)
    }

    pub fn len(&self) -> usize {
        self.map.len()
    }

    pub fn is_empty(&self) -> bool {
        self.map.is_empty()
    }

    /// The characters of the keys held.
    pub fn used_chars(&self) -> usize {
        self.used
    }

    /// Forgets everything held (a release: the keys and values are decrypted message text).
    pub fn clear(&mut self) {
        self.map.clear();
        self.used = 0;
    }

    pub fn get(&mut self, key: &str) -> Option<Arc<V>> {
        self.tick += 1;
        let tick = self.tick;
        self.map.get_mut(key).map(|(at, v)| {
            *at = tick;
            v.clone()
        })
    }

    /// Keeps `value` under `key` (unless the key is over the body limit) and drops the least recently used until the
    /// limits hold again.
    pub fn put(&mut self, key: String, value: Arc<V>) {
        let n = key.chars().count();
        if n > self.body {
            return;
        }
        self.tick += 1;
        if let Some((_, old)) = self.map.insert(key.clone(), (self.tick, value)) {
            drop(old);
        } else {
            self.used += n;
        }
        while self.map.len() > self.entries || self.used > self.chars {
            let Some(oldest) = self
                .map
                .iter()
                .min_by_key(|(_, (at, _))| *at)
                .map(|(k, _)| k.clone())
            else {
                break;
            };
            self.used -= oldest.chars().count();
            self.map.remove(&oldest);
        }
    }

    /// The value under `key`, made by `make` and kept when it is not there.
    pub fn memo(&mut self, key: &str, make: impl FnOnce() -> V) -> Arc<V> {
        if let Some(hit) = self.get(key) {
            return hit;
        }
        let made = Arc::new(make());
        self.put(key.to_owned(), made.clone());
        made
    }
}

/// A process-wide memo behind a lock (the log draws on one thread, tests on several).
pub struct Shared<V>(Mutex<Lru<V>>);

impl<V> Shared<V> {
    pub fn new(lru: Lru<V>) -> Self {
        Self(Mutex::new(lru))
    }

    /// `make` runs outside the lock, so a slow parse never blocks another thread's hit.
    pub fn memo(&self, key: &str, make: impl FnOnce() -> V) -> Arc<V> {
        if let Some(hit) = self
            .0
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .get(key)
        {
            return hit;
        }
        let made = Arc::new(make());
        self.0
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .put(key.to_owned(), made.clone());
        made
    }

    pub fn len(&self) -> usize {
        self.0.lock().unwrap_or_else(PoisonError::into_inner).len()
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    pub fn used_chars(&self) -> usize {
        self.0
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .used_chars()
    }

    /// Whether `key` is held (without touching its age).
    pub fn contains(&self, key: &str) -> bool {
        self.0
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .map
            .contains_key(key)
    }

    /// Forgets everything held.
    pub fn clear(&self) {
        self.0
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .clear();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// markdown.test.ts N1: keeps at most 200 bodies, dropping the least recently used first.
    #[test]
    fn keeps_at_most_200_entries_least_recently_used_first() {
        let mut c: Lru<usize> = Lru::web();
        let first = c.memo("first body", || 1);
        for k in 0..ENTRIES - 1 {
            c.memo(&format!("filler {k}"), || k);
            c.get("first body");
        }
        for k in 0..150 {
            c.memo(&format!("more {k}"), || k);
        }
        assert!(c.len() <= ENTRIES);
        // Still there, and the same value.
        assert!(Arc::ptr_eq(&c.get("first body").expect("kept"), &first));
    }

    /// N1: at most two million characters of bodies.
    #[test]
    fn keeps_at_most_two_million_characters() {
        let mut c: Lru<()> = Lru::web();
        for k in 0..700 {
            let key = format!("{k} ")
                .repeat(1)
                .chars()
                .chain(std::iter::repeat('x'))
                .take(3900)
                .collect::<String>();
            c.memo(&key, || ());
            assert!(c.used_chars() <= CHARS);
        }
        assert!(c.len() <= ENTRIES);
    }

    /// N1: a body over 4000 characters is never kept.
    #[test]
    fn does_not_keep_a_body_over_4000_characters() {
        let mut c: Lru<u8> = Lru::web();
        let long = "a".repeat(BODY + 1);
        let a = c.memo(&long, || 1);
        assert!(c.is_empty());
        assert_eq!(c.used_chars(), 0);
        let b = c.memo(&long, || 1);
        assert!(!Arc::ptr_eq(&a, &b), "made again, not remembered");
    }

    /// "remembers the parse of a body, so a re-render does not parse again".
    #[test]
    fn a_hit_runs_nothing() {
        let mut c: Lru<u8> = Lru::web();
        let mut made = 0;
        c.memo("k", || {
            made += 1;
            0
        });
        c.memo("k", || {
            made += 1;
            0
        });
        assert_eq!(made, 1);
    }
}
