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

fn shadow(color: Hsla, y: f32, blur: f32) -> Vec<BoxShadow> {
    vec![BoxShadow {
        color,
        offset: point(px(0.0), px(y)),
        blur_radius: px(blur),
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

    const TOKENS_TS: &str = include_str!("../../../../packages/design-tokens/src/tokens.ts");

    #[test]
    fn constants_match_tokens_ts() {
        for needle in [
            "labelTracking: '0.08em'",
            "trackingSoft: '0.04em'",
            "trackingTag: '0.06em'",
            "labelTrackingWide: '0.12em'",
            "opacity: { muted: '0.8' }",
            "s1: '0 8px 20px rgba(0,0,0,0.4)'",
            "s3: '0 30px 80px rgba(0,0,0,0.65)'",
            "knob: '0 1px 3px rgba(0,0,0,0.4)'",
            "menu: '0 18px 48px rgba(0,0,0,0.6)'",
            "toast: '0 12px 30px rgba(0,0,0,0.5)'",
            "focusRing: '2px solid var(--accent)'",
            "compact: { rowPadY: '0.25rem' /* 4px */, rowPadX: '1rem' /* 16px */, rowGap: '0rem' /* 0px */, groupGap: '0.5rem' /* 8px */, avatar: '1.75rem' /* 28px */, lineHeight: '1.4' }",
            "regular: { rowPadY: '0.375rem' /* 6px */, rowPadX: '1.125rem' /* 18px */, rowGap: '0.125rem' /* 2px */, groupGap: '0.875rem' /* 14px */, avatar: '2rem' /* 32px */, lineHeight: '1.5' }",
            "cozy: { rowPadY: '0.625rem' /* 10px */, rowPadX: '1.25rem' /* 20px */, rowGap: '0.25rem' /* 4px */, groupGap: '1.375rem' /* 22px */, avatar: '2.25rem' /* 36px */, lineHeight: '1.55' }",
        ] {
            assert!(
                TOKENS_TS.contains(needle),
                "tokens.ts no longer has `{needle}`"
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
