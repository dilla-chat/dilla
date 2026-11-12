//! Task 14's components (`components/members/`): the member list (packages/ui MemberList, Signal §5.7), the profile card
//! (ProfileCard) and the role chip. They draw plain data handed in by the members screen; every gate (which actions a
//! card offers) is decided by the client's `features::members::menu_for` before it reaches here.

use std::rc::Rc;

use gpui::{
    AnyElement, App, BoxShadow, ClickEvent, ElementId, Hsla, InteractiveElement, IntoElement,
    MouseButton, MouseDownEvent, ParentElement, Pixels, Point, RenderOnce, Rgba, SharedString,
    StatefulInteractiveElement, Styled, Window, div, point, px, rgb,
};

use crate::components::avatar::{Avatar, AvatarShape, AvatarSize, Presence};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{LABEL_TRACKING, LABEL_TRACKING_WIDE, OPACITY_MUTED};
use crate::text::{line, line_height};
use crate::tokens::{color, font, layout, radius, text};

/// What a group of the list is (MemberList.tsx `kind`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum GroupKind {
    Role,
    Online,
    Offline,
    Bots,
}

/// One row (MemberList.tsx `MemberListMember`).
#[derive(Clone, Debug)]
pub struct ListMember {
    /// The member's id, handed to the callbacks (the row's element id is `member-<name>`, as a channel row's is its name).
    pub id: SharedString,
    pub name: SharedString,
    pub is_owner: bool,
    pub is_bot: bool,
    /// The line under the name: a custom status, where they are in voice, or the presence word.
    pub status: SharedString,
    /// The top coloured role's 0xRRGGBB, when it has one.
    pub color: Option<u32>,
    pub presence: Presence,
}

/// One section (MemberList.tsx `MemberListGroup`).
#[derive(Clone, Debug)]
pub struct ListGroup {
    pub id: SharedString,
    pub kind: GroupKind,
    /// The section's raw name; the heading reads `{title} — {n}`.
    pub title: SharedString,
    pub color: Option<u32>,
    pub members: Vec<ListMember>,
}

/// The sRGB relative luminance of 0xRRGGBB (WCAG).
fn luminance(c: Rgba) -> f32 {
    let lin = |v: f32| {
        if v <= 0.040_45 {
            v / 12.92
        } else {
            ((v + 0.055) / 1.055).powf(2.4)
        }
    };
    0.2126 * lin(c.r) + 0.7152 * lin(c.g) + 0.0722 * lin(c.b)
}

fn contrast(a: Rgba, b: Rgba) -> f32 {
    let (x, y) = (luminance(a), luminance(b));
    (x.max(y) + 0.05) / (x.min(y) + 0.05)
}

/// `roleNameColor`: the role colour as drawn on a name or a heading, when it keeps 4.5:1 against both surfaces a row
/// sits on (`--surface` and `--surface-2`) after `opacity` (an offline row's dimming); else `None` (the text token).
pub fn role_tint(colour: Option<u32>, opacity: f32) -> Option<Hsla> {
    let colour = colour.filter(|c| *c != 0)?;
    let c = rgb(colour);
    let over = |bg: Hsla| {
        let bg = Rgba::from(bg);
        let painted = Rgba {
            r: c.r * opacity + bg.r * (1.0 - opacity),
            g: c.g * opacity + bg.g * (1.0 - opacity),
            b: c.b * opacity + bg.b * (1.0 - opacity),
            a: 1.0,
        };
        contrast(painted, bg)
    };
    (over(color::SURFACE) >= 4.5 && over(color::SURFACE_2) >= 4.5).then(|| c.into())
}

type OnId = Rc<dyn Fn(&str, &mut Window, &mut App)>;
type OnMenu = Rc<dyn Fn(&str, Point<Pixels>, &mut Window, &mut App)>;
type OnClose = Rc<dyn Fn(&mut Window, &mut App)>;
/// Draws an extra child into the element named by its id (the app's automation bounds probe).
pub type Probe = Rc<dyn Fn(&str) -> AnyElement>;

/// The members of a server (Signal §5.7): an optional 44px head ("Members" and a close ×), then one section per
/// non-empty group, each a heading in the role colour with a 3×10 bar and "— n", then 44px rows. A click opens the
/// member's card (`on_open`), a right-click the person menu (`on_menu`).
#[derive(IntoElement)]
pub struct MemberList {
    id: ElementId,
    label: Option<SharedString>,
    close_label: Option<SharedString>,
    groups: Vec<ListGroup>,
    loading: bool,
    loading_label: SharedString,
    empty_label: SharedString,
    bot_label: SharedString,
    open_id: Option<SharedString>,
    on_open: Option<OnId>,
    on_menu: Option<OnMenu>,
    on_close: Option<OnClose>,
    probe: Option<Probe>,
}

impl MemberList {
    pub fn new(id: impl Into<ElementId>, groups: Vec<ListGroup>) -> Self {
        Self {
            id: id.into(),
            label: None,
            close_label: None,
            groups,
            loading: false,
            loading_label: SharedString::default(),
            empty_label: SharedString::default(),
            bot_label: "bot".into(),
            open_id: None,
            on_open: None,
            on_menu: None,
            on_close: None,
            probe: None,
        }
    }

    /// The 44px head, with its close × when `close_label` is given.
    pub fn head(
        mut self,
        label: impl Into<SharedString>,
        close_label: Option<SharedString>,
    ) -> Self {
        self.label = Some(label.into());
        self.close_label = close_label;
        self
    }

    pub fn loading(mut self, loading: bool, label: impl Into<SharedString>) -> Self {
        self.loading = loading;
        self.loading_label = label.into();
        self
    }

    pub fn empty_label(mut self, label: impl Into<SharedString>) -> Self {
        self.empty_label = label.into();
        self
    }

    pub fn bot_label(mut self, label: impl Into<SharedString>) -> Self {
        self.bot_label = label.into();
        self
    }

    /// The member whose card is open: its row keeps the hover fill.
    pub fn open_id(mut self, id: Option<SharedString>) -> Self {
        self.open_id = id;
        self
    }

    pub fn on_open(mut self, f: impl Fn(&str, &mut Window, &mut App) + 'static) -> Self {
        self.on_open = Some(Rc::new(f));
        self
    }

    pub fn on_menu(
        mut self,
        f: impl Fn(&str, Point<Pixels>, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.on_menu = Some(Rc::new(f));
        self
    }

    pub fn on_close(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_close = Some(Rc::new(f));
        self
    }

    /// Draws `probe(element id)` into each row and the close ×, so the app's automation can find them.
    pub fn probe(mut self, probe: Probe) -> Self {
        self.probe = Some(probe);
        self
    }
}

fn row(
    m: &ListMember,
    kind: GroupKind,
    open: bool,
    bot_label: &SharedString,
    on_open: Option<OnId>,
    on_menu: Option<OnMenu>,
    probe: Option<&Probe>,
) -> AnyElement {
    let dim = kind == GroupKind::Offline;
    let tint = role_tint(m.color, if dim { OPACITY_MUTED } else { 1.0 });
    let mut line1 = div().flex().items_center().gap(px(5.)).min_w_0().child(
        div().flex_initial().min_w_0().flex().child(
            line(
                m.name.clone(),
                text::SM,
                line_height(text::SM, 1.3),
                tint.unwrap_or(color::FG),
            )
            .ellipsis(),
        ),
    );
    if m.is_owner {
        line1 = line1.child(Icon::new(IconName::Crown).size(px(11.)).color(color::WARN));
    }
    if m.is_bot {
        line1 = line1.child(
            div()
                .flex_none()
                .px(px(4.))
                .h(px(15.))
                .flex()
                .items_center()
                .rounded(radius::TAG)
                .bg(color::ACCENT_CHIP)
                .child(
                    line(bot_label.to_uppercase(), text::BADGE, 15.0, color::ACCENT_2)
                        .weight(font::WEIGHT_DISPLAY)
                        .tracking(LABEL_TRACKING),
                ),
        );
    }
    if dim {
        line1 = line1.opacity(OPACITY_MUTED);
    }
    let mut words = div().flex().flex_col().flex_1().min_w_0().child(line1);
    if !m.status.is_empty() {
        words = words.child(
            line(
                m.status.clone(),
                text::META,
                line_height(text::META, 1.3),
                color::FG_3,
            )
            .ellipsis(),
        );
    }
    let id: SharedString = format!("member-{}", m.name).into();
    let (open_id, menu_id) = (m.id.clone(), m.id.clone());
    div()
        .id(id.clone())
        .h(layout::ROW_H_2)
        .px(px(8.))
        .flex()
        .items_center()
        .gap(px(10.))
        .rounded(radius::MD)
        .when_open(open)
        .hover(|s| s.bg(color::HOVER))
        .cursor_pointer()
        .child(
            Avatar::new(SharedString::from(format!("{id}-avatar")), m.name.clone())
                .size(AvatarSize::Member)
                .shape(if m.is_bot {
                    AvatarShape::Bot
                } else {
                    AvatarShape::Circle
                })
                .presence(m.presence),
        )
        .child(words)
        .relative()
        .children(probe.map(|p| p(&id)))
        .on_click(move |_: &ClickEvent, window, cx| {
            if let Some(f) = &on_open {
                f(&open_id, window, cx);
            }
        })
        .on_mouse_down(MouseButton::Right, move |e: &MouseDownEvent, window, cx| {
            if let Some(f) = &on_menu {
                f(&menu_id, e.position, window, cx);
            }
        })
        .into_any_element()
}

trait OpenFill {
    fn when_open(self, open: bool) -> Self;
}

impl OpenFill for gpui::Stateful<gpui::Div> {
    fn when_open(self, open: bool) -> Self {
        if open { self.bg(color::HOVER) } else { self }
    }
}

impl RenderOnce for MemberList {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let mut root = div().id(self.id).flex().flex_col().min_w_0();
        if let Some(label) = self.label {
            let mut bar = div()
                .flex_none()
                .h(layout::BAR)
                .pl(px(16.))
                .pr(px(8.))
                .flex()
                .items_center()
                .border_b_1()
                .border_color(color::HAIRLINE)
                .child(
                    div().flex_1().min_w_0().flex().child(
                        line(label, text::UI, line_height(text::UI, 1.5), color::FG)
                            .weight(font::WEIGHT_DISPLAY)
                            .ellipsis(),
                    ),
                );
            if let (Some(_), Some(on_close)) = (self.close_label, self.on_close.clone()) {
                bar = bar.child(
                    div()
                        .id("members-close")
                        .size(px(32.))
                        .flex_none()
                        .flex()
                        .items_center()
                        .justify_center()
                        .rounded(radius::MD)
                        .hover(|s| s.bg(color::HOVER))
                        .cursor_pointer()
                        .child(Icon::new(IconName::X).size(px(15.)).color(color::FG_2))
                        .on_click(move |_: &ClickEvent, window, cx| on_close(window, cx))
                        .relative()
                        .children(self.probe.as_ref().map(|p| p("members-close"))),
                );
            }
            root = root.child(bar);
        }
        let note = |s: SharedString| {
            div().px(px(8.)).py(px(10.)).child(line(
                s,
                text::UI,
                line_height(text::UI, 1.5),
                color::FG_3,
            ))
        };
        let shown: Vec<&ListGroup> = self
            .groups
            .iter()
            .filter(|g| !g.members.is_empty())
            .collect();
        let mut body = div().id("members-body").p(px(8.)).flex().flex_col();
        if self.loading {
            body = body.child(note(self.loading_label));
        } else if shown.is_empty() {
            body = body.child(note(self.empty_label));
        } else {
            for g in shown {
                let tint = if g.kind == GroupKind::Role {
                    role_tint(g.color, 1.0)
                } else {
                    None
                };
                let mut heading = div()
                    .mt(px(10.))
                    .mb(px(4.))
                    .px(px(8.))
                    .flex()
                    .items_center()
                    .gap(px(6.));
                if let Some(t) = tint {
                    heading =
                        heading.child(div().flex_none().w(px(3.)).h(px(10.)).rounded(px(2.)).bg(t));
                }
                heading = heading.child(
                    line(
                        format!("{} — {}", g.title, g.members.len()).to_uppercase(),
                        text::MICRO,
                        line_height(text::MICRO, 1.5),
                        tint.unwrap_or(color::FG_3),
                    )
                    .weight(font::WEIGHT_LABEL)
                    .tracking(LABEL_TRACKING_WIDE),
                );
                let rows = g.members.iter().map(|m| {
                    row(
                        m,
                        g.kind,
                        self.open_id.as_ref() == Some(&m.id),
                        &self.bot_label,
                        self.on_open.clone(),
                        self.on_menu.clone(),
                        self.probe.as_ref(),
                    )
                });
                body = body.child(
                    div()
                        .id(SharedString::from(format!("members-group-{}", g.id)))
                        .flex()
                        .flex_col()
                        .child(heading)
                        .children(rows),
                );
            }
        }
        root.child(body)
    }
}

/// A role chip of the profile card: 2px 8px, 4px corners in the `--control-edge-field` line, 10.5px `--fg-chip`, a 6px
/// dot in the role's colour (`--fg-2` without one).
pub fn role_chip(name: impl Into<SharedString>, colour: u32) -> AnyElement {
    let dot = if colour == 0 {
        color::FG_2
    } else {
        rgb(colour).into()
    };
    div()
        .flex()
        .items_center()
        .gap(px(5.))
        .px(px(8.))
        .py(px(2.))
        .border_1()
        .border_color(color::CONTROL_EDGE_FIELD)
        .rounded(radius::SM)
        .child(div().size(px(6.)).flex_none().rounded(radius::PILL).bg(dot))
        .child(line(
            name,
            text::META,
            line_height(text::META, 1.5),
            color::FG_CHIP,
        ))
        .into_any_element()
}

/// One glyph action of the card (34px, `--r-seg`, the `--control-edge-field` line).
#[derive(Clone, Debug)]
pub struct CardAction {
    /// Stable: `profile-action-<id>`.
    pub id: SharedString,
    pub label: SharedString,
    pub icon: IconName,
    pub danger: bool,
}

/// `--shadow-profile`: `0 24px 60px rgba(0,0,0,0.6)`.
fn shadow_profile() -> Vec<BoxShadow> {
    vec![BoxShadow {
        color: gpui::hsla(0., 0., 0., 0.6),
        offset: point(px(0.), px(24.)),
        blur_radius: px(60.),
        spread_radius: px(0.),
        inset: false,
    }]
}

/// The profile card (Signal §5.7): 300px, `--surface-2` in the `--control-edge-field` line, 12px corners; the 56px band
/// in the person's hue, the 56px avatar over it, the name (15px 700) with the crown, the status line, the bio, the role
/// chips, then Message and the glyph actions.
#[derive(IntoElement)]
pub struct ProfileCard {
    id: ElementId,
    name: SharedString,
    sub: SharedString,
    bio: SharedString,
    is_owner: bool,
    is_bot: bool,
    hue: u16,
    roles: Vec<(SharedString, u32)>,
    message: Option<SharedString>,
    actions: Vec<CardAction>,
    on_action: Option<OnId>,
}

impl ProfileCard {
    /// `sub` is the line under the name (`@username · status`).
    pub fn new(
        id: impl Into<ElementId>,
        name: impl Into<SharedString>,
        sub: impl Into<SharedString>,
        hue: u16,
    ) -> Self {
        Self {
            id: id.into(),
            name: name.into(),
            sub: sub.into(),
            bio: SharedString::default(),
            is_owner: false,
            is_bot: false,
            hue,
            roles: Vec::new(),
            message: None,
            actions: Vec::new(),
            on_action: None,
        }
    }

    pub fn bio(mut self, bio: impl Into<SharedString>) -> Self {
        self.bio = bio.into();
        self
    }

    pub fn owner(mut self, owner: bool) -> Self {
        self.is_owner = owner;
        self
    }

    pub fn bot(mut self, bot: bool) -> Self {
        self.is_bot = bot;
        self
    }

    pub fn roles(mut self, roles: Vec<(SharedString, u32)>) -> Self {
        self.roles = roles;
        self
    }

    /// The filled Message button, when the card offers it.
    pub fn message(mut self, label: impl Into<SharedString>) -> Self {
        self.message = Some(label.into());
        self
    }

    pub fn actions(mut self, actions: Vec<CardAction>) -> Self {
        self.actions = actions;
        self
    }

    /// Called with `message` or an action's id.
    pub fn on_action(mut self, f: impl Fn(&str, &mut Window, &mut App) + 'static) -> Self {
        self.on_action = Some(Rc::new(f));
        self
    }
}

impl RenderOnce for ProfileCard {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let hue = f32::from(self.hue) / 360.;
        let band = div().h(px(56.)).bg(gpui::linear_gradient(
            135.,
            gpui::linear_color_stop(gpui::hsla(hue, 0.45, 0.30, 1.), 0.),
            gpui::linear_color_stop(gpui::hsla(hue, 0.45, 0.16, 1.), 1.),
        ));
        let mut title = div()
            .mt(px(10.))
            .flex()
            .items_center()
            .gap(px(6.))
            .min_w_0()
            .child(
                line(
                    self.name.clone(),
                    text::CARD,
                    line_height(text::CARD, 1.5),
                    color::FG,
                )
                .weight(font::WEIGHT_DISPLAY)
                .ellipsis(),
            );
        if self.is_owner {
            title = title.child(Icon::new(IconName::Crown).size(px(13.)).color(color::WARN));
        }
        let mut body = div()
            .mt(px(-28.))
            .px(px(16.))
            .pb(px(16.))
            .flex()
            .flex_col()
            .child(
                div().flex().child(
                    div()
                        .rounded(radius::PILL)
                        .border_4()
                        .border_color(color::SURFACE_2)
                        .child(
                            Avatar::new("profile-avatar", self.name.clone())
                                .size(AvatarSize::Card)
                                .shape(if self.is_bot {
                                    AvatarShape::Bot
                                } else {
                                    AvatarShape::Circle
                                }),
                        ),
                ),
            )
            .child(title)
            .child(line(
                self.sub,
                text::XS,
                line_height(text::XS, 1.5),
                color::FG_2,
            ));
        if !self.bio.is_empty() {
            body = body.child(div().mt(px(10.)).children(self.bio.split('\n').map(|l| {
                line(
                    l.to_owned(),
                    text::UI,
                    line_height(text::UI, 1.5),
                    color::FG_STRONG,
                )
            })));
        }
        if !self.roles.is_empty() {
            body = body.child(
                div()
                    .mt(px(12.))
                    .flex()
                    .flex_wrap()
                    .gap(px(6.))
                    .children(self.roles.into_iter().map(|(n, c)| role_chip(n, c))),
            );
        }
        let mut row = div().mt(px(14.)).flex().justify_end().gap(px(6.));
        if let Some(label) = self.message {
            let on = self.on_action.clone();
            row = row.child(
                div()
                    .id("profile-action-message")
                    .flex_1()
                    .h(px(34.))
                    .px(px(12.))
                    .flex()
                    .items_center()
                    .justify_center()
                    .gap(px(6.))
                    .rounded(radius::SEG)
                    .bg(color::ACCENT)
                    .cursor_pointer()
                    .child(
                        Icon::new(IconName::MessageSquare)
                            .size(px(14.))
                            .color(color::ACCENT_INK),
                    )
                    .child(
                        line(
                            label,
                            text::UI,
                            line_height(text::UI, 1.5),
                            color::ACCENT_INK,
                        )
                        .weight(font::WEIGHT_DISPLAY),
                    )
                    .on_click(move |_: &ClickEvent, window, cx| {
                        if let Some(f) = &on {
                            f("message", window, cx);
                        }
                    }),
            );
        }
        for a in self.actions {
            let on = self.on_action.clone();
            let id = a.id.clone();
            row = row.child(
                div()
                    .id(SharedString::from(format!("profile-action-{}", a.id)))
                    .size(px(34.))
                    .flex_none()
                    .flex()
                    .items_center()
                    .justify_center()
                    .rounded(radius::SEG)
                    .border_1()
                    .border_color(color::CONTROL_EDGE_FIELD)
                    .hover(|s| s.border_color(color::ACCENT))
                    .cursor_pointer()
                    .child(Icon::new(a.icon).size(px(14.)).color(if a.danger {
                        color::DANGER
                    } else {
                        color::FG_CHIP
                    }))
                    .on_click(move |_: &ClickEvent, window, cx| {
                        if let Some(f) = &on {
                            f(&id, window, cx);
                        }
                    }),
            );
        }
        div()
            .id(self.id)
            .w(px(300.))
            .overflow_hidden()
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(color::CONTROL_EDGE_FIELD)
            .rounded(radius::XXL)
            .shadow(shadow_profile())
            .occlude()
            .child(band)
            .child(body.child(row))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_role_colour_is_drawn_only_while_it_keeps_4_5_to_1() {
        assert!(role_tint(Some(0x5A_A8E6), 1.0).is_some());
        assert!(role_tint(Some(0x10_1010), 1.0).is_none());
        assert!(role_tint(None, 1.0).is_none());
        assert!(role_tint(Some(0), 1.0).is_none());
    }
}

/// The members screens' spacing and sizes, from the web's CSS (MemberList.css, ProfileCard.css, PersonContextMenu.css,
/// the roles and settings sections): the screen code composes these, never literals (R21).
pub mod space {
    use gpui::{Pixels, px};

    pub const S2: Pixels = px(2.);
    pub const S4: Pixels = px(4.);
    pub const S6: Pixels = px(6.);
    pub const S8: Pixels = px(8.);
    pub const S10: Pixels = px(10.);
    pub const S12: Pixels = px(12.);
    pub const S14: Pixels = px(14.);
    pub const S16: Pixels = px(16.);
    /// A role's colour dot in the roles list.
    pub const ROLE_DOT: Pixels = px(10.);
    /// A colour swatch of the role editor.
    pub const SWATCH: Pixels = px(22.);
    /// The person menu (PersonContextMenu.css `.d-person-menu`).
    pub const PERSON_MENU_W: Pixels = px(236.);
    /// The profile card's width and its tallest body (the role chips wrap below it).
    pub const CARD_W: Pixels = px(300.);
    pub const CARD_MAX_H: Pixels = px(360.);
    /// The card's distance from the window's edges and from the aside (`calc(100vw - 16px)`).
    pub const CARD_EDGE: Pixels = px(8.);
}
