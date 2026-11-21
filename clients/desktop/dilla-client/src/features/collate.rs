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
    let set = LOCALE.read().unwrap_or_else(PoisonError::into_inner);
    if set.is_empty() {
        FALLBACK.to_owned()
    } else {
        set.clone()
    }
}

thread_local! {
    /// The collators this thread built (an ICU collator is not `Send`), one per locale and strength.
    static CACHE: RefCell<HashMap<(String, bool), Rc<Collator>>> = RefCell::new(HashMap::new());
}

/// The collator of `tag`: base letters only when `base` (localeCompare `sensitivity: 'base'`), else ICU's default
/// strength (accents and case break ties). Built once per locale.
fn collator(tag: &str, base: bool) -> Rc<Collator> {
    CACHE.with(|cache| {
        cache
            .borrow_mut()
            .entry((tag.to_owned(), base))
            .or_insert_with(|| {
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
            })
            .clone()
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
    base_in(&locale(), a, b)
}

/// [`default_in`] in the process locale.
pub fn by_default(a: &str, b: &str) -> Ordering {
    default_in(&locale(), a, b)
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
    fn a_tag_that_does_not_parse_falls_back_to_english() {
        assert_eq!(
            base_in("not a tag", "Åsa", "Zed"),
            base_in("en-US", "Åsa", "Zed")
        );
    }
}
