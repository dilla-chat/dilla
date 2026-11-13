//! Profile text (a display name, a status message, a bio): the client's mirror of internal/auth `NormalizeUserText`, a port of
//! `packages/client-core/src/profile-text.ts`. Pure. Every character property comes from the generated tables of one
//! pinned Unicode version ([`super::tables`]), the server's too, so the two agree.
//!
//! A Rust `str` is valid UTF-8, so the TS rule's lone-surrogate check has no counterpart here.

use super::tables::{
    DEFAULT_IGNORABLE, EMOJI, EMOJI_PRESENTATION, EXT_PICT, GRAPHEME_EXTEND, JOINING, MARKS,
    MONGOLIAN_LETTERS, Range, TAG_SEQUENCES, UNUSABLE, VIRAMA, WHITE_SPACE,
};

pub const DISPLAY_MAX_CHARS: usize = 64;
pub const STATUS_MAX_CHARS: usize = 190;
pub const BIO_MAX_CHARS: usize = 500;
/// The most combining marks one base may carry; more is overlay ("zalgo") text.
const MAX_MARK_RUN: usize = 8;

/// Why profile text is refused: more characters than the field allows, characters that cannot be shown, or nothing visible.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ProfileTextProblem {
    Long,
    Chars,
    Blank,
}

impl ProfileTextProblem {
    /// The `strings.rs` key of the line shown next to the field (`profile.error.*`).
    pub fn string_key(self) -> &'static str {
        match self {
            Self::Chars => "profile.error.chars",
            Self::Long => "profile.error.long",
            Self::Blank => "profile.error.blank",
        }
    }
}

fn in_ranges(cp: u32, ranges: &[Range]) -> bool {
    ranges
        .binary_search_by(|&(from, to)| {
            if cp < from {
                std::cmp::Ordering::Greater
            } else if cp > to {
                std::cmp::Ordering::Less
            } else {
                std::cmp::Ordering::Equal
            }
        })
        .is_ok()
}

/// Joining_Type: L, R, D, C or T from the generated table, U for every other code point.
fn join_type(cp: u32) -> u8 {
    JOINING
        .binary_search_by(|&(from, to, _)| {
            if cp < from {
                std::cmp::Ordering::Greater
            } else if cp > to {
                std::cmp::Ordering::Less
            } else {
                std::cmp::Ordering::Equal
            }
        })
        .map_or(b'U', |i| JOINING[i].2)
}

fn is_virama(cp: u32) -> bool {
    in_ranges(cp, VIRAMA)
}

fn is_white_space(cp: u32) -> bool {
    in_ranges(cp, WHITE_SPACE)
}

fn code_points(s: &str) -> Vec<u32> {
    s.chars().map(u32::from).collect()
}

/// RFC 5892 Appendix A.1: (L|D) T* ZWNJ T* (R|D).
fn zwnj_joins(cps: &[u32], i: usize) -> bool {
    let mut j = i;
    while j > 0 && join_type(cps[j - 1]) == b'T' {
        j -= 1;
    }
    if j == 0 || !matches!(join_type(cps[j - 1]), b'L' | b'D') {
        return false;
    }
    let mut k = i + 1;
    while k < cps.len() && join_type(cps[k]) == b'T' {
        k += 1;
    }
    k < cps.len() && matches!(join_type(cps[k]), b'R' | b'D')
}

/// A ZWJ between two pictographs (see `joinsEmoji` in profile_text.go).
fn joins_emoji(cps: &[u32], i: usize) -> bool {
    if i == 0 || i + 1 >= cps.len() {
        return false;
    }
    let (b, a) = (cps[i - 1], cps[i + 1]);
    let before = (0x1f3fb..=0x1f3ff).contains(&b)
        || in_ranges(b, EMOJI_PRESENTATION)
        || (b == 0xfe0f && i >= 2 && in_ranges(cps[i - 2], EXT_PICT));
    let after = in_ranges(a, EMOJI_PRESENTATION)
        || (in_ranges(a, EXT_PICT) && i + 2 < cps.len() && cps[i + 2] == 0xfe0f);
    before && after
}

/// The Default_Ignorable code points that may stand where they are, by context.
fn ignorable_allowed(cps: &[u32], i: usize) -> bool {
    let cp = cps[i];
    let after_virama = i > 0 && is_virama(cps[i - 1]);
    match cp {
        0x200c => after_virama || zwnj_joins(cps, i),
        0x200d => after_virama || joins_emoji(cps, i),
        0xfe0e | 0xfe0f => i > 0 && in_ranges(cps[i - 1], EMOJI),
        0x180b | 0x180c | 0x180d | 0x180f => i > 0 && in_ranges(cps[i - 1], MONGOLIAN_LETTERS),
        _ => false,
    }
}

/// The length of the RGI tag sequence that starts at `cps[i]`, 0 when none does.
fn tag_sequence_at(cps: &[u32], i: usize) -> usize {
    TAG_SEQUENCES
        .iter()
        .find(|seq| {
            cps.len() >= i + seq.len() && seq.iter().enumerate().all(|(k, &cp)| cps[i + k] == cp)
        })
        .map_or(0, |seq| seq.len())
}

/// More than [`MAX_MARK_RUN`] Grapheme_Extend code points after one base character; a Default_Ignorable (a joiner, a
/// selector) between them is transparent and never starts a new count, only a new base does.
fn mark_run_too_long(cps: &[u32]) -> bool {
    let mut run = 0;
    for &cp in cps {
        if in_ranges(cp, GRAPHEME_EXTEND) {
            run += 1;
            if run > MAX_MARK_RUN {
                return true;
            }
        } else if !in_ranges(cp, DEFAULT_IGNORABLE) {
            run = 0;
        }
    }
    false
}

fn chars_ok(cps: &[u32], multiline: bool) -> bool {
    let mut i = 0;
    while i < cps.len() {
        let cp = cps[i];
        if cp == 0x0a {
            if !multiline {
                return false;
            }
        } else if cp < 0x20
            || (0x7f..=0x9f).contains(&cp)
            || in_ranges(cp, UNUSABLE)
            || matches!(cp, 0x2028 | 0x2029 | 0x2800 | 0xfff9..=0xfffb)
        {
            return false;
        } else if cp == 0x1f3f4 {
            // A flag tag sequence from the RGI list (England, Scotland, Wales) is kept whole. Any other tag character is
            // Default_Ignorable and refused below, so a run of tags cannot carry hidden text.
            let n = tag_sequence_at(cps, i);
            if n > 0 {
                i += n - 1;
            }
        } else if in_ranges(cp, DEFAULT_IGNORABLE) && !ignorable_allowed(cps, i) {
            return false;
        }
        i += 1;
    }
    true
}

fn trim_white_space(cps: &[u32]) -> &[u32] {
    let mut from = 0;
    let mut to = cps.len();
    while from < to && is_white_space(cps[from]) {
        from += 1;
    }
    while to > from && is_white_space(cps[to - 1]) {
        to -= 1;
    }
    &cps[from..to]
}

/// NFC from `icu_normalizer`'s compiled data. Its Unicode version is the crate's, not pinned to the tables' 17.0.0: the two
/// differ only for a character composed in a newer version.
fn nfc(s: &str) -> String {
    icu_normalizer::ComposingNormalizer::new_nfc()
        .normalize(s)
        .into_owned()
}

fn lf(s: &str, multiline: bool) -> String {
    if multiline {
        s.replace("\r\n", "\n")
    } else {
        s.to_owned()
    }
}

/// The text as it is sent and stored: NFC, CRLF as LF, trimmed (White_Space).
pub fn profile_text_value(v: &str, multiline: bool) -> String {
    let text = nfc(&lf(v, multiline));
    let cps = code_points(&text);
    trim_white_space(&cps)
        .iter()
        .filter_map(|&cp| char::from_u32(cp))
        .collect()
}

/// Profile text as the instance checks it (the mirror of `NormalizeUserText`): at most `max_chars` characters (code points,
/// after NFC), none of the refused characters, not nothing-visible; a bio (`multiline`) keeps its line breaks. `None` when
/// it passes; "" passes and clears the field.
pub fn profile_text_problem(
    v: &str,
    max_chars: usize,
    multiline: bool,
) -> Option<ProfileTextProblem> {
    let typed = lf(v, multiline);
    // The marks are counted as typed as well: NFC composes the first one into the base.
    if mark_run_too_long(&code_points(&typed)) {
        return Some(ProfileTextProblem::Chars);
    }
    let cps = code_points(&nfc(&typed));
    if cps.len() > max_chars {
        return Some(ProfileTextProblem::Long);
    }
    if mark_run_too_long(&cps) || !chars_ok(&cps, multiline) {
        return Some(ProfileTextProblem::Chars);
    }
    if !cps.is_empty()
        && !trim_white_space(&cps).iter().any(|&cp| {
            !is_white_space(cp) && !in_ranges(cp, MARKS) && !in_ranges(cp, DEFAULT_IGNORABLE)
        })
    {
        return Some(ProfileTextProblem::Blank);
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    // The client half of packages/client-core/src/profile-text.test.ts (and -ignorable.test.ts): the same cases, the same
    // answers. The characters are code points, never literals.
    fn s(cps: &[u32]) -> String {
        cps.iter().filter_map(|&c| char::from_u32(c)).collect()
    }
    fn problem(cps: &[u32]) -> Option<ProfileTextProblem> {
        profile_text_problem(&s(cps), 190, false)
    }
    const ZWNJ: u32 = 0x200c;
    const ZWJ: u32 = 0x200d;

    #[test]
    fn joiners_keep_persian_kurdish_and_sinhala_text_working() {
        let accepted: [&[u32]; 8] = [
            &[0x645, 0x6cc, ZWNJ, 0x62e, 0x648, 0x627, 0x647, 0x645],
            &[
                0x645, 0x62d, 0x645, 0x62f, 0x6cc, ZWNJ, 0x646, 0x698, 0x627, 0x62f,
            ],
            &[0x6a9, 0x62a, 0x627, 0x628, ZWNJ, 0x647, 0x627],
            &[0xdc1, 0xdca, ZWJ, 0xdbb, 0xdd3],
            &[0x915, 0x94d, ZWNJ, 0x937],
            &[0x915, 0x94d, ZWJ, 0x937],
            &[0xd23, 0xd4d, ZWJ],
            &[0x628, 0x64e, ZWNJ, 0x647],
        ];
        for t in accepted {
            assert_eq!(problem(t), None, "{t:x?}");
        }
        let refused: [&[u32]; 10] = [
            &[ZWNJ, 0x645],
            &[0x645, ZWNJ],
            &[97, ZWNJ, 98],
            &[0x627, ZWNJ, 0x628],
            &[0x628, ZWNJ, 0x621],
            &[0x628, ZWNJ, ZWNJ, 0x647],
            &[ZWJ, 0x645],
            &[0x645, ZWJ, 0x6cc],
            &[97, ZWJ, 98],
            &[97, 0x94d, ZWJ, ZWJ, 98],
        ];
        for t in refused {
            assert_eq!(problem(t), Some(ProfileTextProblem::Chars), "{t:x?}");
        }
    }

    #[test]
    fn emoji_joiners_need_pictographs() {
        for t in [
            &[0xa9, ZWJ, 0xa9][..],
            &[94, ZWJ, 0xa9],
            &[0x2800, ZWJ, 0x2801],
        ] {
            assert_eq!(problem(t), Some(ProfileTextProblem::Chars));
        }
        assert_eq!(problem(&[0x1f642, ZWJ, 0x2194, 0xfe0f]), None);
        assert_eq!(problem(&[0x1f468, ZWJ, 0x1f469, ZWJ, 0x1f467]), None);
        assert_eq!(problem(&[0x1f642, ZWJ]), Some(ProfileTextProblem::Chars));
        assert_eq!(
            problem(&[0x1f642, ZWJ, ZWJ, 0x1f642]),
            Some(ProfileTextProblem::Chars)
        );
    }

    #[test]
    fn every_rgi_emoji_zwj_sequence_passes() {
        let file = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../../internal/auth/testdata/rgi-zwj-sequences.txt");
        let text = std::fs::read_to_string(file).expect("the RGI list");
        let lines: Vec<&str> = text
            .lines()
            .filter(|l| !l.trim().is_empty() && !l.starts_with('#'))
            .collect();
        assert!(lines.len() > 1000);
        let bad: Vec<&&str> = lines
            .iter()
            .filter(|l| {
                let cps: Vec<u32> = l
                    .split_whitespace()
                    .map(|h| u32::from_str_radix(h, 16).unwrap())
                    .collect();
                problem(&cps).is_some()
            })
            .collect();
        assert!(bad.is_empty(), "{bad:?}");
    }

    #[test]
    fn invisible_characters_and_tags() {
        let england = [
            0x1f3f4, 0xe0067, 0xe0062, 0xe0065, 0xe006e, 0xe0067, 0xe007f,
        ];
        assert_eq!(problem(&england), None);
        for t in [
            &[97, 0xe0041, 98][..],
            &[0xe007f],
            &[0x1f3f4, 0xe0067, 0xe0062],
            &[0x1f642, 0xe0068, 0xe0069, 0xe007f],
            &[0x1f3f4, 0xe0067, 0xe007f],
        ] {
            assert_eq!(problem(t), Some(ProfileTextProblem::Chars), "{t:x?}");
        }
        for cp in [
            0xad, 0x34f, 0x115f, 0x1160, 0x3164, 0xffa0, 0x2800, 0x2061, 0x2064, 0x180e, 0x206a,
            0xfff9, 0x200b, 0x2060, 0xfeff, 0x61c, 0x2028, 0x2029,
        ] {
            assert_eq!(
                problem(&[97, cp, 98]),
                Some(ProfileTextProblem::Chars),
                "{cp:x}"
            );
        }
    }

    #[test]
    fn blank_long_and_variation_selectors() {
        assert_eq!(problem(&[0x301]), Some(ProfileTextProblem::Blank));
        assert_eq!(
            problem(&[32, 0x301, 32, 0x302]),
            Some(ProfileTextProblem::Blank)
        );
        assert_eq!(problem(&[101, 0x301]), None);
        assert_eq!(problem(&[0x1f642, 0xfe0f]), None);
        assert_eq!(problem(&[0x61, 0xfe0f]), Some(ProfileTextProblem::Chars));
        assert_eq!(problem(&[0xfe0f]), Some(ProfileTextProblem::Chars));
        assert_eq!(
            problem(&[0x1f642, 0xfe0f, 0xfe0f]),
            Some(ProfileTextProblem::Chars)
        );
        assert_eq!(problem(&[0x301, 0xfe0f]), Some(ProfileTextProblem::Chars));
        assert_eq!(
            profile_text_problem(&"a".repeat(65), DISPLAY_MAX_CHARS, false),
            Some(ProfileTextProblem::Long)
        );
        assert_eq!(
            profile_text_problem(&"a".repeat(64), DISPLAY_MAX_CHARS, false),
            None
        );
        assert_eq!(profile_text_problem("", DISPLAY_MAX_CHARS, false), None);
    }

    #[test]
    fn a_bio_keeps_its_line_breaks_and_a_status_does_not() {
        assert_eq!(profile_text_problem("a\nb", BIO_MAX_CHARS, true), None);
        assert_eq!(profile_text_problem("a\r\nb", BIO_MAX_CHARS, true), None);
        assert_eq!(
            profile_text_problem("a\nb", STATUS_MAX_CHARS, false),
            Some(ProfileTextProblem::Chars)
        );
    }

    #[test]
    fn the_stored_value_is_nfc_and_trimmed() {
        assert_eq!(profile_text_value("  e\u{301}  ", false), "\u{e9}");
        assert_eq!(profile_text_value("a\r\nb \n", true), "a\nb");
        assert_eq!(profile_text_value("   ", false), "");
    }
}
