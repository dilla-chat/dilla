//! `BotResult` (packages/ui BotResult): a private result row in the log, drawn like a message row (MessageRow.css: the
//! 44px gutter, the 30px avatar, 10px gaps) with the status chip, "Only you can see this" and Dismiss under the body.

use std::rc::Rc;

use gpui::{
    App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::components::avatar::{Avatar, AvatarShape, AvatarSize};
use crate::components::tag::{Tag, TagKind};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::text::{line, line_height, paragraph};
use crate::tokens::{color, font, radius, text};

/// The row's status (`BotStatus`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Status {
    Pending,
    Success,
    Failed,
    Cancelled,
}

impl Status {
    /// BotResult.tsx's icon per status.
    pub fn icon(self) -> IconName {
        match self {
            Self::Pending => IconName::Loader,
            Self::Success => IconName::Check,
            Self::Failed => IconName::X,
            Self::Cancelled => IconName::TriangleAlert,
        }
    }

    /// BotResult.css: the chip's fill and ink.
    pub fn chip(self) -> (gpui::Hsla, gpui::Hsla) {
        match self {
            Self::Pending => (color::ACCENT_SOFT, color::ACCENT_2),
            Self::Success => (color::OK_RESULT, color::OK),
            Self::Failed => (color::DANGER_SOFT, color::DANGER),
            Self::Cancelled => (color::WARN_RESULT, color::WARN_RESULT_TEXT),
        }
    }
}

/// Who the row names: the Bot (its avatar) or this app (the brand `D` tile).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Author {
    Bot { name: SharedString },
    Builtin { name: SharedString },
}

/// The row's copy.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Labels {
    pub status: SharedString,
    pub only_you: SharedString,
    pub dismiss: SharedString,
}

/// MessageRow.css's columns, and the row's padding (7px above, 3px below).
pub const GUTTER: f32 = 44.0;
pub const AVATAR: f32 = 30.0;
pub const GAP: f32 = 10.0;
const ROW_TOP: f32 = 7.0;
const ROW_BOTTOM: f32 = 3.0;
const ROW_RIGHT: f32 = 16.0;

type OnDismiss = Rc<dyn Fn(&mut Window, &mut App)>;

#[derive(IntoElement)]
pub struct BotResult {
    id: ElementId,
    author: Author,
    time: SharedString,
    status: Status,
    body: SharedString,
    labels: Labels,
    width: f32,
    on_dismiss: Option<OnDismiss>,
    /// A held row (no answer yet): its Retry label and handler.
    retry: Option<(SharedString, OnDismiss)>,
}

impl BotResult {
    /// `width`: the log's width, which the body wraps in.
    pub fn new(
        id: impl Into<ElementId>,
        author: Author,
        time: impl Into<SharedString>,
        status: Status,
        body: impl Into<SharedString>,
        labels: Labels,
        width: f32,
    ) -> Self {
        Self {
            id: id.into(),
            author,
            time: time.into(),
            status,
            body: body.into(),
            labels,
            width,
            on_dismiss: None,
            retry: None,
        }
    }

    /// Offers Retry (the same request id) on a row that got no answer.
    pub fn on_retry(
        mut self,
        label: impl Into<SharedString>,
        f: impl Fn(&mut Window, &mut App) + 'static,
    ) -> Self {
        self.retry = Some((label.into(), Rc::new(f)));
        self
    }

    pub fn on_dismiss(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_dismiss = Some(Rc::new(f));
        self
    }

    fn body_width(width: f32) -> f32 {
        (width - GUTTER - GAP - AVATAR - GAP - ROW_RIGHT).max(0.0)
    }

    /// The row's height for `body` in a log `width` wide: the head line, the wrapped body, the 3px gap and the 18px
    /// meta line, inside the row's padding.
    pub fn height(body: &str, width: f32) -> f32 {
        let lines = paragraph(
            body,
            text::INPUT,
            line_height(text::INPUT, 1.55),
            color::FG_BODY,
            Self::body_width(width),
        )
        .len()
        .max(1);
        ROW_TOP
            + line_height(text::SM, 1.5)
            + 1.0
            + lines as f32 * line_height(text::INPUT, 1.55)
            + 3.0
            + 18.0
            + ROW_BOTTOM
    }
}

impl RenderOnce for BotResult {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let (chip_bg, chip_fg) = self.status.chip();
        let meta_lh = line_height(text::META, 1.5);
        let avatar = match &self.author {
            Author::Bot { name } => Avatar::new("bot-result-avatar", name.clone())
                .size(AvatarSize::Msg)
                .shape(AvatarShape::Bot)
                .into_any_element(),
            Author::Builtin { .. } => div()
                .size(px(AVATAR))
                .rounded(radius::SM)
                .bg(color::BRAND)
                .flex()
                .items_center()
                .justify_center()
                .child(
                    line("D", text::SM, line_height(text::SM, 1.5), color::BRAND_INK)
                        .weight(font::WEIGHT_DISPLAY),
                )
                .into_any_element(),
        };
        let name = match &self.author {
            Author::Bot { name } | Author::Builtin { name } => name.clone(),
        };
        let body_fg = if self.status == Status::Pending {
            color::FG_2
        } else {
            color::FG_BODY
        };
        let body_lines = paragraph(
            &self.body,
            text::INPUT,
            line_height(text::INPUT, 1.55),
            body_fg,
            Self::body_width(self.width),
        );
        let dismiss = self.on_dismiss.clone();
        div()
            .id(self.id)
            .flex()
            .pt(px(ROW_TOP))
            .pb(px(ROW_BOTTOM))
            .pr(px(ROW_RIGHT))
            .child(
                div()
                    .w(px(GUTTER))
                    .flex_none()
                    .pt(px(18.0))
                    .flex()
                    .justify_end()
                    .child(line(self.time, text::META, meta_lh, color::FG_QUIET)),
            )
            .child(div().w(px(GAP)).flex_none())
            .child(div().w(px(AVATAR)).flex_none().child(avatar))
            .child(div().w(px(GAP)).flex_none())
            .child(
                div()
                    .flex_1()
                    .min_w_0()
                    .flex()
                    .flex_col()
                    .gap(px(1.0))
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap(px(8.0))
                            .child(
                                line(name, text::SM, line_height(text::SM, 1.5), color::ACCENT_2)
                                    .weight(font::WEIGHT_DISPLAY),
                            )
                            .child(Tag::new("bot-result-tag", TagKind::Bot)),
                    )
                    .child(div().flex().flex_col().children(body_lines))
                    .child(
                        div()
                            .mt(px(3.0))
                            .flex()
                            .items_center()
                            .gap(px(8.0))
                            .child(
                                div()
                                    .flex()
                                    .items_center()
                                    .gap(px(5.0))
                                    .h(px(18.0))
                                    .px(px(6.0))
                                    .rounded(radius::SM)
                                    .bg(chip_bg)
                                    .child(
                                        Icon::new(self.status.icon()).size(px(11.0)).color(chip_fg),
                                    )
                                    .child(line(self.labels.status, text::META, meta_lh, chip_fg)),
                            )
                            .child(
                                div()
                                    .flex()
                                    .items_center()
                                    .gap(px(4.0))
                                    .child(
                                        Icon::new(IconName::Eye).size(px(11.0)).color(color::FG_3),
                                    )
                                    .child(line(
                                        self.labels.only_you,
                                        text::META,
                                        meta_lh,
                                        color::FG_3,
                                    )),
                            )
                            .children(self.retry.map(|(label, f)| {
                                div()
                                    .id("bot-result-retry")
                                    .cursor_pointer()
                                    .border_b_1()
                                    .border_color(color::ACCENT)
                                    .on_click(move |_, window, cx| f(window, cx))
                                    .child(line(label, text::META, meta_lh, color::ACCENT))
                            }))
                            .child(
                                div()
                                    .id("bot-result-dismiss")
                                    .cursor_pointer()
                                    .border_b_1()
                                    .border_color(color::FG_2)
                                    .on_click(move |_, window, cx| {
                                        if let Some(f) = &dismiss {
                                            f(window, cx);
                                        }
                                    })
                                    .child(line(
                                        self.labels.dismiss,
                                        text::META,
                                        meta_lh,
                                        color::FG_2,
                                    )),
                            ),
                    ),
            )
    }
}
