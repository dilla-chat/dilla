//! `Avatar` (packages/ui/src/Avatar): a coloured disc (or a square for a bot) with initials and an optional presence dot.

use gpui::{
    App, ElementId, Hsla, InteractiveElement, IntoElement, ParentElement, Pixels, RenderOnce,
    SharedString, Styled, Window, div, hsla, px,
};

use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

/// The Signal prototype's avatar colours (`AVATAR_PALETTE` in Avatar.tsx): hue in degrees, saturation and lightness in percent.
pub const PALETTE: [(u16, u8, u8); 7] = [
    (211, 45, 42),
    (298, 45, 42),
    (12, 55, 42),
    (48, 45, 34),
    (28, 50, 40),
    (234, 45, 42),
    (160, 40, 34),
];

/// The palette entry of `name`: FNV-1a over `avatar58:<name>`, modulo the palette (`avatarColour` in Avatar.tsx).
pub fn colour_of(name: &str) -> (u16, u8, u8) {
    let mut h: u32 = 0x811c_9dc5;
    for c in format!("avatar58:{name}").chars() {
        h ^= u32::from(c);
        h = h.wrapping_mul(0x0100_0193);
    }
    PALETTE[h as usize % PALETTE.len()]
}

/// The two letters drawn when none are given: the first letters of the first two words, else the name's first two characters.
pub fn initials_of(name: &str) -> String {
    let mut words = name.split_whitespace();
    match (words.next(), words.next()) {
        (Some(a), Some(b)) => a
            .chars()
            .take(1)
            .chain(b.chars().take(1))
            .collect::<String>()
            .to_uppercase(),
        _ => name
            .trim()
            .chars()
            .take(2)
            .collect::<String>()
            .to_uppercase(),
    }
}

fn hsl(h: u16, s: u8, l: u8) -> Hsla {
    hsla(
        f32::from(h) / 360.0,
        f32::from(s) / 100.0,
        f32::from(l) / 100.0,
        1.0,
    )
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum AvatarShape {
    #[default]
    Circle,
    Bot,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum AvatarSize {
    Mini,
    Voice,
    Sug,
    Xs,
    Sm,
    Member,
    Msg,
    Row,
    #[default]
    Md,
    Lg,
    Panel,
    Confirm,
    Card,
}

impl AvatarSize {
    /// The box and the initials' size (`Avatar.css`; `Md` is `--avatar-size`, 32px at the regular density).
    pub fn metrics(self) -> (Pixels, Pixels) {
        match self {
            Self::Mini => (px(14.0), text::PICO),
            Self::Voice => (px(18.0), text::MINI),
            Self::Sug => (px(22.0), text::TAG),
            Self::Sm => (px(20.0), text::BADGE),
            Self::Xs => (px(24.0), text::BADGE),
            Self::Member => (px(28.0), text::MICRO),
            Self::Panel => (px(30.0), text::MICRO),
            Self::Msg => (px(30.0), text::META),
            Self::Row | Self::Md => (px(32.0), text::XS),
            Self::Confirm => (px(40.0), text::UI),
            Self::Card => (px(56.0), text::LG),
            Self::Lg => (px(64.0), text::LG),
        }
    }

    /// A bot's square corner: `--r-bot`, except the sizes `Avatar.css` overrides.
    pub fn bot_radius(self) -> Pixels {
        match self {
            Self::Voice => radius::ROW,
            Self::Mini => radius::TAG,
            Self::Card => radius::XXL,
            _ => radius::BOT,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Presence {
    Online,
    Idle,
    Dnd,
    Offline,
}

impl Presence {
    fn tone(self) -> Hsla {
        match self {
            Self::Online => color::OK,
            Self::Idle => color::WARN,
            Self::Dnd => color::DANGER,
            Self::Offline => color::FG_3,
        }
    }
}

#[derive(IntoElement)]
pub struct Avatar {
    id: ElementId,
    name: SharedString,
    initials: Option<SharedString>,
    hue: Option<u16>,
    presence: Option<Presence>,
    size: AvatarSize,
    shape: AvatarShape,
}

impl Avatar {
    pub fn new(id: impl Into<ElementId>, name: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            name: name.into(),
            initials: None,
            hue: None,
            presence: None,
            size: AvatarSize::Md,
            shape: AvatarShape::Circle,
        }
    }

    pub fn initials(mut self, initials: impl Into<SharedString>) -> Self {
        self.initials = Some(initials.into());
        self
    }

    /// Pins the hue (degrees); saturation and lightness take the CSS fallbacks, 45% and 42%.
    pub fn hue(mut self, hue: u16) -> Self {
        self.hue = Some(hue);
        self
    }

    pub fn presence(mut self, presence: Presence) -> Self {
        self.presence = Some(presence);
        self
    }

    pub fn size(mut self, size: AvatarSize) -> Self {
        self.size = size;
        self
    }

    pub fn shape(mut self, shape: AvatarShape) -> Self {
        self.shape = shape;
        self
    }
}

impl RenderOnce for Avatar {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let (side, initials_size) = self.size.metrics();
        let initials = self
            .initials
            .unwrap_or_else(|| initials_of(&self.name).into());
        let disc = match self.hue {
            Some(h) => hsl(h, 45, 42),
            None => {
                let (h, s, l) = colour_of(&self.name);
                hsl(h, s, l)
            }
        };
        let (bg, corner) = match self.shape {
            AvatarShape::Circle => (disc, radius::AVATAR),
            AvatarShape::Bot => (color::BRAND, self.size.bot_radius()),
        };
        let mut root = div()
            .id(self.id)
            .relative()
            .flex()
            .flex_none()
            .items_center()
            .justify_center()
            .size(side)
            .rounded(corner)
            .bg(bg)
            .child(
                line(
                    initials,
                    initials_size,
                    line_height(initials_size, 1.5),
                    color::BRAND_INK,
                )
                .weight(font::WEIGHT_DISPLAY),
            );
        if let Some(presence) = self.presence {
            // The 9px dot sits 1px outside the corner inside a 2px ring in --bg-2.
            root = root.child(
                div()
                    .absolute()
                    .right(px(-3.0))
                    .bottom(px(-3.0))
                    .size(px(13.0))
                    .flex()
                    .items_center()
                    .justify_center()
                    .rounded(radius::PILL)
                    .bg(color::BG_2)
                    .child(
                        div()
                            .size(px(9.0))
                            .rounded(radius::PILL)
                            .bg(presence.tone()),
                    ),
            );
        }
        root
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The expectation of Avatar.test.tsx ("the prototype sample people get the prototype colours").
    #[test]
    fn prototype_people_get_the_prototype_colours() {
        let want = [
            ("Ada", (298, 45, 42)),
            ("Björn", (211, 45, 42)),
            ("Mira", (234, 45, 42)),
            ("Sven", (28, 50, 40)),
            ("Liv", (160, 40, 34)),
            ("Kari", (12, 55, 42)),
            ("Ole", (48, 45, 34)),
        ];
        for (name, hsl) in want {
            assert_eq!(colour_of(name), hsl, "{name}");
        }
    }

    #[test]
    fn initials_follow_the_web_rule() {
        assert_eq!(initials_of("jonas"), "JO");
        assert_eq!(initials_of("Ada Lovelace"), "AL");
        assert_eq!(initials_of("  x "), "X");
    }

    #[test]
    fn sizes_match_avatar_css() {
        assert_eq!(AvatarSize::Member.metrics(), (px(28.0), text::MICRO));
        assert_eq!(AvatarSize::Lg.metrics().0, px(64.0));
        assert_eq!(AvatarSize::Voice.bot_radius(), radius::ROW);
        assert_eq!(AvatarSize::Md.bot_radius(), radius::BOT);
    }
}
