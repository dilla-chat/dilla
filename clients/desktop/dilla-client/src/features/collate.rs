//! Locale-aware ordering of names: what the web's `a.localeCompare(b, undefined, ...)` does in the user's browser, with
//! ICU4X's collator and the OS locale. Swedish puts Å, Ä and Ö after Z; English folds them into their base letters.
//!
//! The locale is process-wide ([`set_locale`], the desktop sets it once at start from the OS locale); the `*_in`
//! functions take it explicitly, for tests and fixtures, so a test never depends on the machine's locale.

use std::cell::RefCell;
use std::cmp::Ordering;
use std::collections::HashMap;
use std::rc::Rc;
use std::sync::{PoisonError, RwLock};

use icu_collator::options::{CollatorOptions, Strength};
use icu_collator::{Collator, CollatorPreferences};
use icu_locale_core::Locale;

/// The locale used when none was set, and when one does not parse: the web's fallback for names, English.
const FALLBACK: &str = "en-US";

static LOCALE: RwLock<String> = RwLock::new(String::new());

/// Sets the locale names are ordered by (a BCP 47 tag such as `sv-SE`).
pub fn set_locale(tag: &str) {
    *LOCALE.write().unwrap_or_else(PoisonError::into_inner) = tag.to_owned();
}

/// The process locale: the one set, else English.
pub fn locale() -> String {
    with_locale(str::to_owned)
}

/// Runs `f` with the process locale, without copying it.
fn with_locale<R>(f: impl FnOnce(&str) -> R) -> R {
    let set = LOCALE.read().unwrap_or_else(PoisonError::into_inner);
    f(if set.is_empty() { FALLBACK } else { &set })
}

type Pair = [Option<Rc<Collator>>; 2];

thread_local! {
    /// The collators this thread built (an ICU collator is not `Send`), per locale: [default strength, base strength].
    static CACHE: RefCell<HashMap<String, Pair>> = RefCell::new(HashMap::new());
}

fn build(tag: &str, base: bool) -> Rc<Collator> {
    let locale = tag
        .parse::<Locale>()
        .or_else(|_| FALLBACK.parse::<Locale>())
        .unwrap_or(Locale::UNKNOWN);
    let mut options = CollatorOptions::default();
    if base {
        options.strength = Some(Strength::Primary);
    }
    let borrowed = Collator::try_new(CollatorPreferences::from(&locale), options)
        .or_else(|_| Collator::try_new(CollatorPreferences::default(), options))
        .expect("the compiled collation data holds the root collator");
    Rc::new(borrowed.static_to_owned())
}

/// The collator of `tag`: base letters only when `base` (localeCompare `sensitivity: 'base'`), else ICU's default
/// strength (accents and case break ties). Built once per locale and strength; a hit allocates nothing.
fn collator(tag: &str, base: bool) -> Rc<Collator> {
    CACHE.with(|cache| {
        let mut cache = cache.borrow_mut();
        let slot = usize::from(base);
        if let Some(found) = cache.get(tag).and_then(|pair| pair[slot].clone()) {
            return found;
        }
        let built = build(tag, base);
        cache.entry(tag.to_owned()).or_default()[slot] = Some(built.clone());
        built
    })
}

/// `a.localeCompare(b, tag, { sensitivity: 'base' })`: letters only; case and accents do not tell names apart.
pub fn base_in(tag: &str, a: &str, b: &str) -> Ordering {
    collator(tag, true).as_borrowed().compare(a, b)
}

/// `a.localeCompare(b, tag)`: ICU's default strength, so case and accents break ties.
pub fn default_in(tag: &str, a: &str, b: &str) -> Ordering {
    collator(tag, false).as_borrowed().compare(a, b)
}

/// [`base_in`] in the process locale.
pub fn base(a: &str, b: &str) -> Ordering {
    with_locale(|tag| base_in(tag, a, b))
}

/// [`default_in`] in the process locale.
pub fn by_default(a: &str, b: &str) -> Ordering {
    with_locale(|tag| default_in(tag, a, b))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sorted(tag: &str, names: &[&str]) -> Vec<String> {
        let mut v: Vec<String> = names.iter().map(|s| (*s).to_owned()).collect();
        v.sort_by(|a, b| base_in(tag, a, b));
        v
    }

    #[test]
    fn swedish_puts_a_ring_a_umlaut_and_o_umlaut_after_z_and_english_does_not() {
        let names = ["Åsa", "Zed", "Örjan", "Ada", "Ärlig"];
        assert_eq!(
            sorted("sv-SE", &names),
            ["Ada", "Zed", "Åsa", "Ärlig", "Örjan"]
        );
        assert_eq!(
            sorted("en-US", &names),
            ["Ada", "Ärlig", "Åsa", "Örjan", "Zed"]
        );
    }

    #[test]
    fn a_name_with_a_caron_sorts_by_its_base_letter() {
        assert_eq!(sorted("en-US", &["Tom", "Šimon"]), ["Šimon", "Tom"]);
        assert_eq!(sorted("sv-SE", &["Tom", "Šimon"]), ["Šimon", "Tom"]);
    }

    #[test]
    fn decomposed_input_orders_as_composed_and_base_strength_ignores_case() {
        assert_eq!(base_in("en-US", "A\u{301}da", "\u{c1}da"), Ordering::Equal);
        assert_eq!(base_in("en-US", "BJÖRN", "björn"), Ordering::Equal);
        assert_eq!(base_in("en-US", "Bjorn", "Björn"), Ordering::Equal);
    }

    #[test]
    fn the_default_strength_lets_case_and_accents_break_ties() {
        assert_eq!(default_in("en-US", "bjorn", "Bjorn"), Ordering::Less);
        assert_eq!(default_in("en-US", "bjorn", "björn"), Ordering::Less);
        assert_eq!(default_in("en-US", "björn", "bjorn2"), Ordering::Less);
    }

    #[test]
    fn a_second_call_reuses_the_collator_of_its_locale_and_strength() {
        let a = collator("sv-SE", true);
        let b = collator("sv-SE", true);
        assert!(Rc::ptr_eq(&a, &b), "one collator per locale and strength");
        assert!(!Rc::ptr_eq(&a, &collator("sv-SE", false)));
        assert!(!Rc::ptr_eq(&a, &collator("en-US", true)));
    }

    #[test]
    fn a_tag_that_does_not_parse_falls_back_to_english() {
        assert_eq!(
            base_in("not a tag", "Åsa", "Zed"),
            base_in("en-US", "Åsa", "Zed")
        );
    }
}
