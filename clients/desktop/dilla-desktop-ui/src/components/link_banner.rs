//! `LinkBanner` (packages/ui/src/LinkBanner): the phone's connection line while the link to the server is down or coming
//! up. Online it draws nothing and takes no room.

use gpui::{
    App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    Styled, Window, div, px,
};

use super::state_dot::{StateDot, StateDotTone};
use crate::metrics::Density;
use crate::text::{line, line_height};
use crate::tokens::{color, text};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum LinkStatus {
    Offline,
    Connecting,
    Online,
}

#[derive(IntoElement)]
pub struct LinkBanner {
    id: ElementId,
    status: LinkStatus,
    offline: SharedString,
    connecting: SharedString,
    density: Density,
}

impl LinkBanner {
    /// `offline` and `connecting` are the two lines, already filled with the server's name.
    pub fn new(
        id: impl Into<ElementId>,
        status: LinkStatus,
        offline: impl Into<SharedString>,
        connecting: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            status,
            offline: offline.into(),
            connecting: connecting.into(),
            density: Density::Regular,
        }
    }

    pub fn density(mut self, density: Density) -> Self {
        self.density = density;
        self
    }
}

impl RenderOnce for LinkBanner {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let root = div().id(self.id.clone());
        let (tone, label, rule, fg) = match self.status {
            LinkStatus::Online => return root.size_0(),
            LinkStatus::Offline => (
                StateDotTone::Danger,
                self.offline,
                color::DANGER,
                color::FG,
            ),
            LinkStatus::Connecting => (
                StateDotTone::Pending,
                self.connecting,
                color::HAIRLINE_2,
                color::FG_2,
            ),
        };
        root.flex()
            .items_center()
            .gap(px(8.0))
            .min_w_0()
            .w_full()
            .px(self.density.row_pad_x())
            .py(px(6.0))
            .bg(color::SURFACE_2)
            .border_b_1()
            .border_color(rule)
            .child(StateDot::new((self.id, "dot"), tone))
            .child(
                div()
                    .min_w_0()
                    .child(line(label, text::SM, line_height(text::SM, 1.5), fg).ellipsis()),
            )
    }
}
