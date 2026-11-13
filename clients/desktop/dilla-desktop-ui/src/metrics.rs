//! The structural tokens `tokens.rs` does not generate (the letter spacings, the muted opacity, the shadows, the density
//! steps, the focus ring), as constants named after `packages/design-tokens/src/tokens.ts`.
//! `tests::constants_match_tokens_ts` reads that file and fails when one of them drifts.

use gpui::{BoxShadow, Hsla, Pixels, point, px};

use crate::tokens::color;

/// `--label-tracking`, in em.
pub const LABEL_TRACKING: f32 = 0.08;
/// `--label-tracking-wide`, in em.
pub const LABEL_TRACKING_WIDE: f32 = 0.12;
/// `--tracking-soft`, in em.
pub const TRACKING_SOFT: f32 = 0.04;
/// `--tracking-tag`, in em.
pub const TRACKING_TAG: f32 = 0.06;
/// `--opacity-muted`.
pub const OPACITY_MUTED: f32 = 0.8;
/// The opacity of a disabled control (Button.css, IconButton.css).
pub const OPACITY_DISABLED: f32 = 0.4;

/// `--density`: the setting Appearance changes (`data-density`), which steps the row paddings and the avatar.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Density {
    Compact,
    #[default]
    Regular,
    Cozy,
}

impl Density {
    /// `--row-pad-x`.
    pub fn row_pad_x(self) -> Pixels {
        match self {
            Self::Compact => px(16.0),
            Self::Regular => px(18.0),
            Self::Cozy => px(20.0),
        }
    }

    /// `--row-pad-y`.
    pub fn row_pad_y(self) -> Pixels {
        match self {
            Self::Compact => px(4.0),
            Self::Regular => px(6.0),
            Self::Cozy => px(10.0),
        }
    }

    /// `--row-gap`.
    pub fn row_gap(self) -> Pixels {
        match self {
            Self::Compact => px(0.0),
            Self::Regular => px(2.0),
            Self::Cozy => px(4.0),
        }
    }

    /// `--group-gap`.
    pub fn group_gap(self) -> Pixels {
        match self {
            Self::Compact => px(8.0),
            Self::Regular => px(14.0),
            Self::Cozy => px(22.0),
        }
    }

    /// `--avatar-size`.
    pub fn avatar(self) -> Pixels {
        match self {
            Self::Compact => px(28.0),
            Self::Regular => px(32.0),
            Self::Cozy => px(36.0),
        }
    }

    /// `--line-height`.
    pub fn line_height(self) -> f32 {
        match self {
            Self::Compact => 1.4,
            Self::Regular => 1.5,
            Self::Cozy => 1.55,
        }
    }
}

/// GPUI's `blur_radius` for a CSS shadow blur `blur`: CSS blurs with a Gaussian of standard deviation half the blur
/// radius (CSS Backgrounds 3, `box-shadow`), and GPUI's shadow shader takes `blur_radius` as that deviation.
pub fn css_blur(blur: f32) -> Pixels {
    px(blur / 2.0)
}

/// A ring of `spread` px in `color` (CSS `box-shadow: 0 0 0 Npx color`: no offset, no blur): a field's soft focus ring.
pub fn spread_ring(color: Hsla, spread: f32) -> Vec<BoxShadow> {
    vec![BoxShadow {
        color,
        offset: point(px(0.0), px(0.0)),
        blur_radius: px(0.0),
        spread_radius: px(spread),
        inset: false,
    }]
}

/// A CSS shadow token `0 <y>px <blur>px <color>` as GPUI draws it: the blur through [`css_blur`].
pub fn css_shadow(color: Hsla, y: f32, blur: f32) -> Vec<BoxShadow> {
    shadow(color, y, blur)
}

fn shadow(color: Hsla, y: f32, blur: f32) -> Vec<BoxShadow> {
    vec![BoxShadow {
        color,
        offset: point(px(0.0), px(y)),
        blur_radius: css_blur(blur),
        spread_radius: px(0.0),
        inset: false,
    }]
}

fn black(alpha: f32) -> Hsla {
    gpui::hsla(0.0, 0.0, 0.0, alpha)
}

/// `--shadow-1`: `0 8px 20px rgba(0,0,0,0.4)` (a tooltip).
pub fn shadow_1() -> Vec<BoxShadow> {
    shadow(black(0.4), 8.0, 20.0)
}

/// `--shadow-2`: `0 20px 50px rgba(0,0,0,0.55)` (the mention list).
pub fn shadow_2() -> Vec<BoxShadow> {
    shadow(black(0.55), 20.0, 50.0)
}

/// `--shadow-3`: `0 30px 80px rgba(0,0,0,0.65)` (a dialog).
pub fn shadow_3() -> Vec<BoxShadow> {
    shadow(black(0.65), 30.0, 80.0)
}

/// `--shadow-menu`: `0 18px 48px rgba(0,0,0,0.6)`.
pub fn shadow_menu() -> Vec<BoxShadow> {
    shadow(black(0.6), 18.0, 48.0)
}

/// `--shadow-toast`: `0 12px 30px rgba(0,0,0,0.5)`.
pub fn shadow_toast() -> Vec<BoxShadow> {
    shadow(black(0.5), 12.0, 30.0)
}

/// `--shadow-knob`: `0 1px 3px rgba(0,0,0,0.4)`.
pub fn shadow_knob() -> Vec<BoxShadow> {
    shadow(black(0.4), 1.0, 3.0)
}

/// `--focus-ring` (`2px solid var(--accent)`), as a spread shadow around the element. GPUI has no `outline`, so the ring
/// hugs the box; the web's 2px `outline-offset` has no equivalent (a gap would need the surface colour behind it).
pub fn focus_ring() -> Vec<BoxShadow> {
    vec![BoxShadow {
        color: color::ACCENT,
        offset: point(px(0.0), px(0.0)),
        blur_radius: px(0.0),
        spread_radius: px(2.0),
        inset: false,
    }]
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The vendored GPUI's box-edge snap (core/vendor/README.md, "gpui-pre 0.3.12"): at a whole scale factor an edge goes
    /// to a whole logical px, half up, as Chromium paints the web client's boxes; at a fractional scale it keeps
    /// upstream's device-px rounding (midpoints toward zero). Device px in, device px out.
    #[test]
    fn box_edges_snap_as_chromium_paints_them() {
        use gpui::snap_layout_edge as snap;
        // Scale 2: 51.5px paints from 52px; 51.25px from 51px; 51.75px from 52px.
        assert_eq!(snap(103.0, 2.0), 104.0);
        assert_eq!(snap(102.5, 2.0), 102.0);
        assert_eq!(snap(103.5, 2.0), 104.0);
        assert_eq!(snap(104.0, 2.0), 104.0);
        // Scale 1: half up.
        assert_eq!(snap(50.5, 1.0), 51.0);
        assert_eq!(snap(50.49, 1.0), 50.0);
        // A fractional scale keeps device-px rounding, midpoints toward zero.
        assert_eq!(snap(77.5, 1.5), 77.0);
        assert_eq!(snap(77.6, 1.5), 78.0);
        // Review m5: the other fractional scales too (Windows 125 % and 175 %).
        for scale in [1.25, 1.75] {
            assert_eq!(snap(77.5, scale), 77.0);
            assert_eq!(snap(77.6, scale), 78.0);
            assert_eq!(snap(78.0, scale), 78.0);
        }
    }

    /// Before the edges snap, the vendored GPUI lays lengths out on Chromium's layout unit (1/64 css px) at a whole scale
    /// factor: an authored length floored onto it, a measured one (text) ceiled, as LayoutUnit does. A fractional scale
    /// keeps upstream's whole device px. Logical px in, device px out.
    #[test]
    fn lengths_lay_out_on_chromiums_layout_unit() {
        // 6.6px of mono "3": authored 6.59375, measured 6.609375 (the web's count span); upstream made both 6.5.
        assert_eq!(gpui::layout_length(6.6, 2.0), 6.59375 * 2.0);
        assert_eq!(gpui::measured_length(6.6, 2.0), 6.609375 * 2.0);
        assert_eq!(gpui::layout_length(28.0, 2.0), 56.0);
        assert_eq!(gpui::measured_length(28.0, 2.0), 56.0);
        // Scale 1.5: whole device px, as upstream (authored rounded half toward zero, measured ceiled).
        assert_eq!(gpui::layout_length(6.6, 1.5), 10.0);
        assert_eq!(gpui::measured_length(6.6, 1.5), 10.0);
        // Review m5: 1.25 and 1.75 as upstream (8.25 device px: authored 8, measured 9; 11.55: 12 and 12).
        assert_eq!(gpui::layout_length(6.6, 1.25), 8.0);
        assert_eq!(gpui::measured_length(6.6, 1.25), 9.0);
        assert_eq!(gpui::layout_length(6.6, 1.75), 12.0);
        assert_eq!(gpui::measured_length(6.6, 1.75), 12.0);
    }

    /// Task 28 (T6c review m4): a whole length computed in f32 just off the integer (`30.0 * 2.1` is 62.999996,
    /// `25.0 * 0.6` is 15.000001) lays out whole, as Chromium's double arithmetic gets it; and a negative length
    /// truncates toward zero, as `LayoutUnit(float)` does, where flooring went a unit further out. The text rule
    /// (`text::layout_unit`) agrees.
    #[test]
    fn layout_units_ignore_f32_noise_and_truncate_negatives() {
        let below = std::hint::black_box(30.0_f32) * 2.1;
        let above = std::hint::black_box(25.0_f32) * 0.6;
        assert!(
            below < 63.0 && above > 15.0,
            "the f32 noise the rules must absorb"
        );
        assert_eq!(gpui::layout_length(below, 2.0), 126.0);
        assert_eq!(crate::text::layout_unit(below), 63.0);
        assert_eq!(gpui::measured_length(above, 2.0), 30.0);
        // Negatives: toward zero (-6.6 is -6.59375, not -6.609375).
        assert_eq!(gpui::layout_length(-6.6, 2.0), -6.59375 * 2.0);
        assert_eq!(crate::text::layout_unit(-6.6), -6.59375);
        assert_eq!(gpui::layout_length(-10.0, 2.0), -20.0);
        // A real fraction is still floored onto the unit, as before.
        assert_eq!(gpui::layout_length(6.6, 2.0), 6.59375 * 2.0);
        assert_eq!(crate::text::layout_unit(20.15), 20.140625);
    }

    const TOKENS_TS: &str = include_str!("../../../../packages/design-tokens/src/tokens.ts");

    /// The number that follows `key` in tokens.ts (`labelTracking: '0.08em'` gives 0.08).
    fn num_after(key: &str) -> f32 {
        let at = TOKENS_TS
            .find(key)
            .unwrap_or_else(|| panic!("tokens.ts has no `{key}`"))
            + key.len();
        let digits: String = TOKENS_TS[at..]
            .chars()
            .take_while(|c| c.is_ascii_digit() || *c == '.')
            .collect();
        digits
            .parse()
            .unwrap_or_else(|_| panic!("no number after `{key}`"))
    }

    /// The shadow `name: '0 Ypx Bpx rgba(0,0,0,A)'` of tokens.ts as (y, blur, alpha).
    fn shadow_of(name: &str) -> (f32, f32, f32) {
        let key = format!("{name}: '0 ");
        let at = TOKENS_TS
            .find(&key)
            .unwrap_or_else(|| panic!("no shadow {name}"))
            + key.len();
        let end = at + TOKENS_TS[at..].find('\'').unwrap();
        let nums: Vec<f32> = TOKENS_TS[at..end]
            .split(|c: char| !(c.is_ascii_digit() || c == '.'))
            .filter(|t| !t.is_empty())
            .map(|t| t.parse().unwrap())
            .collect();
        // y, blur, then rgba(0,0,0,alpha): the alpha is the last number.
        (nums[0], nums[1], *nums.last().unwrap())
    }

    #[test]
    fn constants_match_tokens_ts() {
        assert_eq!(LABEL_TRACKING, num_after("labelTracking: '"));
        assert_eq!(TRACKING_SOFT, num_after("trackingSoft: '"));
        assert_eq!(TRACKING_TAG, num_after("trackingTag: '"));
        assert_eq!(LABEL_TRACKING_WIDE, num_after("labelTrackingWide: '"));
        assert_eq!(OPACITY_MUTED, num_after("opacity: { muted: '"));
        // The disabled opacity is Button.css's, not a token.
        assert!(
            include_str!("../../../../packages/ui/src/Button/Button.css").contains("opacity: 0.4")
        );
        assert_eq!(OPACITY_DISABLED, 0.4);
        for (name, got) in [
            ("s1", shadow_1()),
            ("s3", shadow_3()),
            ("menu", shadow_menu()),
            ("toast", shadow_toast()),
            ("knob", shadow_knob()),
        ] {
            let (y, blur, alpha) = shadow_of(name);
            let s = &got[0];
            assert_eq!(
                (
                    f32::from(s.offset.y),
                    2.0 * f32::from(s.blur_radius),
                    s.color.a
                ),
                (y, blur, alpha),
                "shadow {name}"
            );
        }
        assert_eq!(
            f32::from(focus_ring()[0].spread_radius),
            num_after("focusRing: '")
        );
    }

    #[test]
    fn densities_match_tokens_ts() {
        for (name, d) in [
            ("compact", Density::Compact),
            ("regular", Density::Regular),
            ("cozy", Density::Cozy),
        ] {
            let row = &TOKENS_TS[TOKENS_TS
                .find(&format!("  {name}: {{ rowPadY"))
                .expect("density row")..];
            let row = &row[..row.find('}').unwrap()];
            let px_of = |key: &str| {
                let at = row.find(key).unwrap() + key.len();
                let at = at + row[at..].find("/* ").unwrap() + 3;
                row[at..]
                    .split("px")
                    .next()
                    .unwrap()
                    .parse::<f32>()
                    .unwrap()
            };
            assert_eq!(
                f32::from(d.row_pad_y()),
                px_of("rowPadY:"),
                "{name} rowPadY"
            );
            assert_eq!(
                f32::from(d.row_pad_x()),
                px_of("rowPadX:"),
                "{name} rowPadX"
            );
            assert_eq!(f32::from(d.row_gap()), px_of("rowGap:"), "{name} rowGap");
            assert_eq!(
                f32::from(d.group_gap()),
                px_of("groupGap:"),
                "{name} groupGap"
            );
            assert_eq!(f32::from(d.avatar()), px_of("avatar:"), "{name} avatar");
            let lh = row[row.find("lineHeight: '").unwrap() + 13..]
                .split('\'')
                .next()
                .unwrap();
            assert_eq!(
                d.line_height(),
                lh.parse::<f32>().unwrap(),
                "{name} lineHeight"
            );
        }
    }

    #[test]
    fn density_steps_match_the_web() {
        assert_eq!(Density::default(), Density::Regular);
        assert_eq!(Density::Compact.row_pad_x(), px(16.0));
        assert_eq!(Density::Cozy.avatar(), px(36.0));
        assert_eq!(Density::Regular.group_gap(), px(14.0));
    }
}
