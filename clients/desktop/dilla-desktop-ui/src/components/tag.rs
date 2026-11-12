//! `Tag` (packages/ui/src/Tag): a small uppercase label chip: bot, web, admin, live, playback only, and the readable eye.

use gpui::{
    App, ElementId, Hsla, InteractiveElement, IntoElement, ParentElement, Pixels, RenderOnce,
    Styled, Window, div, px,
};

use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{LABEL_TRACKING, LABEL_TRACKING_WIDE, TRACKING_SOFT};
use crate::text::line;
use crate::tokens::{color, radius, text};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TagKind {
    Bot,
    Web,
    Admin,
    Readable,
    PlaybackOnly,
    Live,
}

impl TagKind {
    /// The visible text (`Tag.tsx`); the readable tag is a glyph.
    pub fn text(self) -> Option<&'static str> {
        match self {
            Self::Bot => Some("bot"),
            Self::Web => Some("web"),
            Self::Admin => Some("admin"),
            Self::PlaybackOnly => Some("playback only"),
            Self::Live => Some("live"),
            Self::Readable => None,
        }
    }

    /// The accessible name of the readable glyph.
    pub fn readable_label() -> &'static str {
        "Readable by this server"
    }
}

/// What `Tag.css` gives a kind: text colour, fill, edge (1px, or none), size, weight, tracking in em, line height in px,
/// whether the text is uppercased.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Look {
    pub fg: Hsla,
    pub bg: Option<Hsla>,
    pub edge: Option<Hsla>,
    pub size: Pixels,
    pub weight: f32,
    pub tracking: f32,
    pub line_height: f32,
    pub upper: bool,
}

pub fn look(kind: TagKind) -> Look {
    let base = Look {
        fg: color::FG_3,
        bg: None,
        edge: Some(color::HAIRLINE_2),
        size: px(9.0),
        weight: 600.0,
        tracking: LABEL_TRACKING_WIDE,
        line_height: 14.0,
        upper: true,
    };
    match kind {
        TagKind::Web | TagKind::Readable => base,
        TagKind::Admin => Look {
            fg: color::ACCENT,
            bg: Some(color::ACCENT_SOFT),
            edge: Some(color::ACCENT),
            ..base
        },
        TagKind::Bot => Look {
            fg: color::ACCENT_2,
            bg: Some(color::ACCENT_CHIP),
            edge: None,
            weight: 700.0,
            tracking: LABEL_TRACKING,
            line_height: 15.0,
            ..base
        },
        TagKind::Live => Look {
            fg: color::DANGER_INK,
            bg: Some(color::DANGER),
            edge: None,
            size: text::TAG,
            weight: 700.0,
            tracking: LABEL_TRACKING,
            line_height: 13.0,
            ..base
        },
        TagKind::PlaybackOnly => Look {
            fg: color::FG_2,
            edge: Some(color::LINE_STRONG),
            size: text::TAG,
            weight: 400.0,
            tracking: TRACKING_SOFT,
            line_height: 13.0,
            upper: false,
            ..base
        },
    }
}

#[derive(IntoElement)]
pub struct Tag {
    id: ElementId,
    kind: TagKind,
}

impl Tag {
    pub fn new(id: impl Into<ElementId>, kind: TagKind) -> Self {
        Self {
            id: id.into(),
            kind,
        }
    }
}

impl RenderOnce for Tag {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let root = div().id(self.id).flex_none();
        let Some(word) = self.kind.text() else {
            return root.child(Icon::new(IconName::Eye).size(px(14.0)).color(color::FG_3));
        };
        let look = look(self.kind);
        let word = if look.upper {
            word.to_uppercase()
        } else {
            word.to_owned()
        };
        let mut chip = root.px(px(4.0)).rounded(radius::TAG);
        if let Some(bg) = look.bg {
            chip = chip.bg(bg);
        }
        if let Some(edge) = look.edge {
            chip = chip.border_1().border_color(edge);
        }
        chip.child(
            line(word, look.size, look.line_height, look.fg)
                .weight(gpui::FontWeight(look.weight))
                .tracking(look.tracking),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn kinds_draw_the_css() {
        assert_eq!(look(TagKind::Admin).edge, Some(color::ACCENT));
        assert_eq!(look(TagKind::Bot).bg, Some(color::ACCENT_CHIP));
        assert_eq!(look(TagKind::Bot).edge, None);
        assert_eq!(look(TagKind::Live).bg, Some(color::DANGER));
        assert!(!look(TagKind::PlaybackOnly).upper);
        assert_eq!(TagKind::PlaybackOnly.text(), Some("playback only"));
        assert_eq!(TagKind::Readable.text(), None);
    }
}
