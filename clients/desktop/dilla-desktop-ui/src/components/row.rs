//! `Row` (packages/ui/src/ChannelRow): one 28px list row, the glyph, the name, then the mention or unread badge.

use gpui::{
    App, ClickEvent, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use super::badge::{Badge, BadgeKind, capped};
use super::button::ClickHandler;
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::text::{line, line_height};
use crate::tokens::{color, font, layout, radius, text};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RowKind {
    Text,
    Voice,
    Announcement,
    Afk,
    Dm,
}

#[derive(IntoElement)]
pub struct Row {
    id: ElementId,
    name: SharedString,
    kind: RowKind,
    active: bool,
    unread: u32,
    mentions: u32,
    muted: bool,
    private: bool,
    on_click: Option<ClickHandler>,
}

impl Row {
    pub fn new(id: impl Into<ElementId>, name: impl Into<SharedString>, kind: RowKind) -> Self {
        Self {
            id: id.into(),
            name: name.into(),
            kind,
            active: false,
            unread: 0,
            mentions: 0,
            muted: false,
            private: false,
            on_click: None,
        }
    }

    pub fn active(mut self, active: bool) -> Self {
        self.active = active;
        self
    }

    pub fn unread(mut self, unread: u32) -> Self {
        self.unread = unread;
        self
    }

    pub fn mentions(mut self, mentions: u32) -> Self {
        self.mentions = mentions;
        self
    }

    pub fn muted(mut self, muted: bool) -> Self {
        self.muted = muted;
        self
    }

    pub fn private(mut self, private: bool) -> Self {
        self.private = private;
        self
    }

    pub fn on_click(
        mut self,
        handler: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.on_click = Some(Box::new(handler));
        self
    }
}

/// The glyph's icon (a DM draws a dot instead).
pub fn glyph_of(kind: RowKind) -> Option<IconName> {
    match kind {
        RowKind::Text => Some(IconName::Hash),
        RowKind::Voice => Some(IconName::Volume2),
        RowKind::Announcement => Some(IconName::Megaphone),
        RowKind::Afk => Some(IconName::Moon),
        RowKind::Dm => None,
    }
}

impl RenderOnce for Row {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        // A muted channel hides its unread count; its mention count still shows.
        let unread = if self.muted { 0 } else { self.unread };
        let bright = self.active || unread > 0 || self.mentions > 0;
        let name_colour = if self.muted && !self.active {
            color::FG_3
        } else if bright {
            color::FG
        } else {
            color::FG_2
        };
        let glyph_colour = if self.active {
            color::ACCENT
        } else {
            color::FG_4
        };
        let glyph = match glyph_of(self.kind) {
            Some(icon) => div().child(Icon::new(icon).size(px(14.0)).color(glyph_colour)),
            None => div().size(px(8.0)).rounded(radius::PILL).bg(glyph_colour),
        };
        let id = self.id.clone();
        let mut row = div()
            .id(self.id)
            .relative()
            .flex()
            .items_center()
            .gap(px(8.0))
            .w_full()
            .h(layout::ROW)
            .px(px(8.0))
            .rounded(radius::ROW)
            .when_active(self.active)
            .cursor_pointer()
            .hover(|s| s.bg(color::HOVER))
            .child(
                div()
                    .flex()
                    .flex_none()
                    .items_center()
                    .justify_center()
                    .w(px(14.0))
                    .child(glyph),
            )
            .child(
                line(
                    self.name,
                    text::INPUT,
                    line_height(text::INPUT, 1.5),
                    name_colour,
                )
                .weight(if bright {
                    font::WEIGHT_LABEL
                } else {
                    font::WEIGHT_BODY
                })
                .ellipsis(),
            );
        if self.active {
            // The 2px accent bar at the left edge, 6px in from the top and bottom.
            row = row.child(
                div()
                    .absolute()
                    .left_0()
                    .top(px(6.0))
                    .bottom(px(6.0))
                    .w(px(2.0))
                    .rounded(px(2.0))
                    .bg(color::ACCENT),
            );
        }
        if self.private {
            row = row.child(
                div()
                    .flex_none()
                    .child(Icon::new(IconName::Lock).size(px(12.0)).color(color::FG_3)),
            );
        }
        row = row.child(div().flex_1());
        if self.mentions > 0 {
            row = row.child(
                Badge::new((id.clone(), "badge"), BadgeKind::Mention, self.mentions)
                    .text(format!("@{}", capped(self.mentions)))
                    .in_row(),
            );
        } else if unread > 0 {
            row = row.child(Badge::new((id, "badge"), BadgeKind::Unread, unread).in_row());
        }
        match self.on_click {
            Some(handler) => row.on_click(handler),
            None => row,
        }
    }
}

/// Small style helpers so the render reads like the CSS rules above it.
trait RowStyle: Styled + Sized {
    /// The selected fill, and the 2px accent bar's room: `--surface-hi`.
    fn when_active(self, active: bool) -> Self {
        if active {
            self.bg(color::SURFACE_HI)
        } else {
            self
        }
    }
}
impl<T: Styled + Sized> RowStyle for T {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn glyphs_match_channel_row() {
        assert_eq!(glyph_of(RowKind::Text), Some(IconName::Hash));
        assert_eq!(glyph_of(RowKind::Voice), Some(IconName::Volume2));
        assert_eq!(glyph_of(RowKind::Announcement), Some(IconName::Megaphone));
        assert_eq!(glyph_of(RowKind::Afk), Some(IconName::Moon));
        assert_eq!(glyph_of(RowKind::Dm), None);
    }
}
