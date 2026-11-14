//! `Row` (packages/ui/src/ChannelRow): one 28px list row, the glyph, the name, then the mention or unread badge.

use gpui::{
    App, ClickEvent, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use super::avatar::{Avatar, AvatarSize, Presence};
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

/// A DM drawn as a person (`ChannelRow` with `person`, Jonas Q2, the prototype's DM row): 42px, the 28px avatar with its
/// online dot, the name over its sub-line, and a 7px accent dot for unread in place of the "n new" tag.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct Person {
    /// Their status, or a group's members.
    pub sub: Option<SharedString>,
    pub presence: Option<Presence>,
    /// A group DM: the avatar is the neutral `--line-strong`, with no presence dot.
    pub group: bool,
    pub initials: Option<SharedString>,
}

/// The person row's height: 42px (`2.625rem`).
pub const PERSON_HEIGHT: f32 = 42.0;

/// The name's weight in a person row: 500, and 700 while unread (or active and unread).
pub fn person_name_weight(unread: u32) -> f32 {
    if unread > 0 { 700.0 } else { 500.0 }
}

#[derive(IntoElement)]
pub struct Row {
    id: ElementId,
    name: SharedString,
    kind: RowKind,
    active: bool,
    unread: u32,
    /// The unread pill's text (ChannelRow.tsx `layout.row.new`, "{n} new"); the ui crate has no strings, so the caller
    /// passes it in. Without it the pill shows the bare count.
    unread_label: Option<SharedString>,
    mentions: u32,
    muted: bool,
    private: bool,
    person: Option<Person>,
    /// A voice row (VoiceChannelRow.css): the people in its call (the count at the right) and whether this device is in it.
    voice_count: usize,
    joined: bool,
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
            unread_label: None,
            mentions: 0,
            muted: false,
            private: false,
            person: None,
            voice_count: 0,
            joined: false,
            on_click: None,
        }
    }

    /// A voice row's call: `count` people in it (shown at the right when any), and whether this device is in it (the
    /// `--fg` name at 600 and the `--ok` glyph, VoiceChannelRow.css `data-joined`).
    pub fn voice(mut self, count: usize, joined: bool) -> Self {
        self.voice_count = count;
        self.joined = joined;
        self
    }

    /// Draws the row as a person (a DM).
    pub fn person(mut self, person: Person) -> Self {
        self.person = Some(person);
        self
    }

    pub fn active(mut self, active: bool) -> Self {
        self.active = active;
        self
    }

    pub fn unread(mut self, unread: u32) -> Self {
        self.unread = unread;
        self
    }

    /// The unread pill's text ("{n} new"), drawn when the row shows its unread count.
    pub fn unread_label(mut self, label: impl Into<SharedString>) -> Self {
        self.unread_label = Some(label.into());
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
        if let Some(person) = self.person.clone() {
            return self.person_row(person, unread).into_any_element();
        }
        let bright = self.active || unread > 0 || self.mentions > 0 || self.joined;
        let name_colour = if self.muted && !self.active {
            color::FG_3
        } else if bright {
            color::FG
        } else {
            color::FG_2
        };
        let glyph_colour = if self.active {
            color::ACCENT
        } else if self.joined {
            color::OK
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
        // A voice row's occupant count: 10.5px, `--fg-3` (`--fg-2` on the selected fill); it is never cut.
        if self.voice_count > 0 {
            row = row.child(div().flex_none().child(line(
                self.voice_count.to_string(),
                text::META,
                line_height(text::META, 1.5),
                if self.active {
                    color::FG_2
                } else {
                    color::FG_3
                },
            )));
        }
        // The voice row this device is in has no trailing slot (VoiceChannelRow.tsx: the slot holds the join, which a
        // joined row does not offer), so its count ends at the row's padding edge.
        if self.joined {
            return match self.on_click {
                Some(handler) => row.on_click(handler).into_any_element(),
                None => row.into_any_element(),
            };
        }
        // The trailing slot is always there (ChannelRow.tsx `<TrailingSlot>`): empty it still holds `--slot-w`, so a long
        // name is cut short of it, and a badge sits flush right in it.
        let slot = super::trailing_slot::TrailingSlot::new((id.clone(), "slot")).in_row();
        row = row.child(if self.mentions > 0 {
            slot.child(
                Badge::new((id.clone(), "badge"), BadgeKind::Mention, self.mentions)
                    .text(format!("@{}", capped(self.mentions)))
                    .in_row(),
            )
        } else if unread > 0 {
            let badge = Badge::new((id, "badge"), BadgeKind::Unread, unread).in_row();
            slot.child(match self.unread_label {
                Some(label) => badge.text(label),
                None => badge,
            })
        } else {
            slot
        });
        match self.on_click {
            Some(handler) => row.on_click(handler).into_any_element(),
            None => row.into_any_element(),
        }
    }
}

impl Row {
    fn person_row(self, person: Person, unread: u32) -> gpui::Stateful<gpui::Div> {
        let bright = self.active || unread > 0 || self.mentions > 0;
        let id = self.id.clone();
        let mut avatar =
            Avatar::new((id.clone(), "avatar"), self.name.clone()).size(AvatarSize::Member);
        if let Some(initials) = person.initials.clone() {
            avatar = avatar.initials(initials);
        }
        if person.group {
            avatar = avatar.fill(color::LINE_STRONG);
        } else if person.presence == Some(Presence::Online) {
            avatar = avatar.presence(Presence::Online);
        }
        let mut who = div().flex().flex_col().flex_1().min_w_0().child(
            line(
                self.name.clone(),
                text::SM,
                line_height(text::SM, 1.3),
                if bright { color::FG } else { color::FG_2 },
            )
            .weight(gpui::FontWeight(person_name_weight(unread)))
            .ellipsis(),
        );
        if let Some(sub) = person.sub.clone().filter(|s| !s.is_empty()) {
            who = who
                .child(line(sub, text::META, line_height(text::META, 1.3), color::FG_3).ellipsis());
        }
        let mut row = div()
            .id(self.id)
            .relative()
            .flex()
            .items_center()
            .gap(px(10.0))
            .w_full()
            .h(px(PERSON_HEIGHT))
            .px(px(8.0))
            .my(px(1.0))
            .rounded(px(6.0))
            .when_active(self.active)
            .cursor_pointer()
            .hover(|s| s.bg(color::HOVER))
            .child(div().flex_none().child(avatar))
            .child(who);
        if self.active {
            row = row.child(
                div()
                    .absolute()
                    .left_0()
                    .top(px(8.0))
                    .bottom(px(8.0))
                    .w(px(2.0))
                    .rounded(px(2.0))
                    .bg(color::ACCENT),
            );
        }
        if self.mentions > 0 {
            row = row.child(
                Badge::new((id, "badge"), BadgeKind::Mention, self.mentions)
                    .text(format!("@{}", capped(self.mentions)))
                    .in_row(),
            );
        } else if unread > 0 {
            row = row.child(
                div()
                    .flex_none()
                    .size(px(7.0))
                    .rounded(radius::PILL)
                    .bg(color::ACCENT),
            );
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
    fn a_person_row_is_42px_and_bolds_the_name_when_unread() {
        assert_eq!(PERSON_HEIGHT, 42.0);
        assert_eq!(person_name_weight(0), 500.0);
        assert_eq!(person_name_weight(3), 700.0);
    }

    #[test]
    fn glyphs_match_channel_row() {
        assert_eq!(glyph_of(RowKind::Text), Some(IconName::Hash));
        assert_eq!(glyph_of(RowKind::Voice), Some(IconName::Volume2));
        assert_eq!(glyph_of(RowKind::Announcement), Some(IconName::Megaphone));
        assert_eq!(glyph_of(RowKind::Afk), Some(IconName::Moon));
        assert_eq!(glyph_of(RowKind::Dm), None);
    }
}
