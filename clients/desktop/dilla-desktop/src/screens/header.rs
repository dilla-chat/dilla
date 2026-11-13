//! The channel header (packages/ui ChannelHeader, Signal §5.5): a 44px bar with the channel's glyph and name, and at the
//! right the Threads and Members buttons. Members has no label but its count, so it carries a tooltip. Either button is
//! pressed while its aside is open.

use std::rc::Rc;

use dilla_client::state::Channel;
use gpui::{
    App, ClickEvent, IntoElement, ParentElement, Pixels, SharedString, Styled, Window, div,
};

use dilla_desktop_ui::components::shell::BarButton;
use dilla_desktop_ui::components::tooltip::WithTooltip;
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, font, layout, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::strings::{t, tf};

/// ChannelHeader.css: padding 0 10px 0 20px, gap 8px (6px inside the title, 2px between the actions), a 14px glyph.
mod css {
    pub const LEFT: f32 = 20.0;
    pub const RIGHT: f32 = 10.0;
    pub const GAP: f32 = 8.0;
    pub const TITLE_GAP: f32 = 6.0;
    pub const ACTIONS_GAP: f32 = 2.0;
    pub const GLYPH: f32 = 14.0;
}

/// Which aside a header button opens.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AsideKind {
    Threads,
    Members,
}

pub struct HeaderModel<'a> {
    pub channel: Option<&'a Channel>,
    pub members: usize,
    /// The aside that is open (`None`: none).
    pub aside: Option<AsideKind>,
    /// A DM's call button (VoiceView.tsx `dmCallStart`, an icon in the DM header): its label (`call.start` or
    /// `call.join`) and what it does; `None` where there is nothing to start.
    pub call: Option<(&'static str, OnCall)>,
}

/// Tooltip.tsx's 400 ms. The automation build shows the tooltip at once: its frames settle within a few frames and do not
/// wait out the delay, and the web shooter frames the screen after it has passed.
#[cfg(not(feature = "automation"))]
const TIP_DELAY: std::time::Duration = dilla_desktop_ui::components::tooltip::SHOW_DELAY;
#[cfg(feature = "automation")]
const TIP_DELAY: std::time::Duration = std::time::Duration::ZERO;

pub type OnToggle = Rc<dyn Fn(AsideKind, &mut Window, &mut App)>;

/// The DM call button's press.
pub type OnCall = Rc<dyn Fn(&mut Window, &mut App)>;

pub fn render(model: &HeaderModel<'_>, on_toggle: &OnToggle) -> impl IntoElement + use<> {
    let name = model.channel.map_or_else(String::new, |c| c.name.clone());
    let threads = on_toggle.clone();
    let members = on_toggle.clone();
    let count = model.members.to_string();
    let members_label = tf("layout.header.membersCount", &[("n", count.as_str())]);
    let members_tip = SharedString::from(t("layout.header.members"));
    let call = model.call.clone().map(|(label, on_call)| {
        crate::tracked_box(
            "header-dm-call",
            BarButton::new("header-dm-call-b", IconName::Phone, label)
                .on_click(move |_: &ClickEvent, window, cx| on_call(window, cx)),
        )
    });
    // Tooltip.tsx places the tooltip against the button (above, centred, kept inside the window), not at the pointer.
    let members_button = WithTooltip::new(
        "header-members-tip",
        members_tip,
        &[],
        BarButton::new(SharedString::from(members_label), IconName::Users, count)
            .pressed(model.aside == Some(AsideKind::Members))
            .on_click(move |_: &ClickEvent, window, cx| members(AsideKind::Members, window, cx)),
    )
    .delay(TIP_DELAY);
    #[cfg(feature = "automation")]
    let members_button = div()
        .relative()
        .child(members_button)
        .child(crate::automation::bounds_probe("header-members"));
    let bar = div()
        .flex_none()
        .relative()
        .h(layout::CHANNEL_HEADER_H)
        .bg(color::BG)
        .border_b_1()
        .border_color(color::HAIRLINE)
        .flex()
        .items_center()
        .pl(Pixels::from(css::LEFT))
        .pr(Pixels::from(css::RIGHT))
        .gap(Pixels::from(css::GAP))
        .child(
            div()
                .flex_1()
                .min_w_0()
                .flex()
                .items_center()
                .gap(Pixels::from(css::TITLE_GAP))
                .child(
                    Icon::new(IconName::Hash)
                        .size(Pixels::from(css::GLYPH))
                        .color(color::FG_3),
                )
                .child(
                    line(name, text::BASE, line_height(text::BASE, 1.5), color::FG)
                        .weight(font::WEIGHT_DISPLAY)
                        .ellipsis(),
                ),
        )
        .child(
            div()
                .flex_none()
                .flex()
                .items_center()
                .gap(Pixels::from(css::ACTIONS_GAP))
                .children(call)
                .child(
                    BarButton::new(
                        "header-threads",
                        IconName::MessagesSquare,
                        t("layout.header.threads"),
                    )
                    .pressed(model.aside == Some(AsideKind::Threads))
                    .on_click(move |_: &ClickEvent, window, cx| {
                        threads(AsideKind::Threads, window, cx)
                    }),
                )
                .child(members_button),
        );
    #[cfg(feature = "automation")]
    let bar = bar.child(crate::automation::bounds_probe("shell-header"));
    bar
}
