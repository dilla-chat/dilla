//! `AttachmentTray` (packages/ui/src/AttachmentTray): the files waiting to be sent, above the composer's box: a row of
//! 84px cards (112px previews, 200px file cards) with the name, the size or the phase, a 3px progress line and remove.

use std::sync::Arc;

use gpui::{
    AnyElement, App, Image, InteractiveElement, IntoElement, ObjectFit, ParentElement,
    SharedString, StatefulInteractiveElement, Styled, div, px, relative,
};

use super::OnClick;
use crate::a11y::{Accessible, Role};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{TRACKING_TAG, focus_ring};
use crate::picture::rounded_picture;
use crate::text::{line, line_height};
use crate::tokens::{color, font, motion, radius, text};

/// `.d-attachment-tray__item`: 84px high inside its 1px line (content-box).
pub const ITEM_H: f32 = 84.0;
/// A file card's width (12.5rem), content-box.
pub const FILE_W: f32 = 200.0;
/// A preview card's width (7rem), content-box.
pub const PREVIEW_W: f32 = 112.0;
/// `.d-attachment-tray { gap: 0.5rem; padding-bottom: 0.5rem }`.
pub const GAP: f32 = 8.0;
/// `.d-attachment-tray__bar`.
const BAR: f32 = 3.0;
/// `.d-attachment-tray__remove`: 1.25rem.
const REMOVE: f32 = 20.0;
/// `.d-attachment-tray__glyph`: 1.75rem.
const GLYPH: f32 = 28.0;

/// tray.ts `TrayPhase`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Step {
    Reading,
    Preparing,
    Uploading,
    Ready,
    Failed,
}

impl Step {
    /// AttachmentTray.tsx `STEP_FILL`.
    pub fn fill(self) -> f32 {
        match self {
            Self::Reading => 0.25,
            Self::Preparing => 0.5,
            Self::Uploading => 0.75,
            Self::Ready | Self::Failed => 1.0,
        }
    }
}

/// The card's picture (AttachmentTray.tsx `kindOf`).
#[derive(Clone)]
pub enum Preview {
    /// A still image decoded in memory.
    Image(Arc<Image>),
    /// A GIF: its name on the striped tile with the GIF mark.
    Gif,
    /// The file card.
    None,
}

/// One entry.
pub struct TrayEntry {
    pub id: SharedString,
    pub name: SharedString,
    pub size: SharedString,
    /// The phase's text, shown while not ready.
    pub phase: SharedString,
    pub step: Step,
    pub preview: Preview,
    pub remove_label: SharedString,
    pub on_remove: OnClick,
}

/// The tray's height when it holds anything: a card with its line, and the padding below.
pub fn height() -> f32 {
    ITEM_H + 2.0 + GAP
}

fn card(e: TrayEntry, cx: &App) -> AnyElement {
    let failed = e.step == Step::Failed;
    let w = match e.preview {
        Preview::None => FILE_W,
        _ => PREVIEW_W,
    };
    let body: AnyElement = match &e.preview {
        // The picture carries the card's rounded top corners itself (GPUI clips no child to them); the progress line
        // is under it, so its bottom corners stay square.
        Preview::Image(image) => {
            rounded_picture(image.clone(), radius::LG, px(1.0), ObjectFit::Cover)
                .h_auto()
                .flex_1()
                .min_h_0()
                .rounded_b(px(0.0))
                .into_any_element()
        }
        Preview::Gif => div()
            .relative()
            .flex_1()
            .min_h_0()
            .flex()
            .items_center()
            .justify_center()
            .bg(color::HOVER)
            .child(
                div().px(px(6.0)).max_w_full().child(
                    line(
                        e.name.to_uppercase(),
                        text::BADGE,
                        crate::text::root_line_height(text::BADGE),
                        color::FG_CHIP,
                    )
                    .tracking(TRACKING_TAG)
                    .ellipsis(),
                ),
            )
            .child(
                div()
                    .absolute()
                    .left(px(4.0))
                    .top(px(4.0))
                    .px(px(4.0))
                    .rounded(radius::TAG)
                    .bg(color::SCRIM_TAG)
                    .child(line("GIF", text::TAG, 14.0, color::FG).weight(font::WEIGHT_DISPLAY)),
            )
            .into_any_element(),
        Preview::None => {
            let meta = if e.step == Step::Ready {
                e.size.clone()
            } else {
                e.phase.clone()
            };
            div()
                .flex_1()
                .min_h_0()
                .flex()
                .items_center()
                .gap(px(8.0))
                .px(px(10.0))
                .min_w_0()
                .child(
                    div()
                        .flex_none()
                        .size(px(GLYPH))
                        .flex()
                        .items_center()
                        .justify_center()
                        .rounded(radius::MD)
                        .bg(color::HOVER)
                        .child(
                            Icon::new(IconName::File)
                                .size(px(14.0))
                                .color(color::ACCENT_2),
                        ),
                )
                .child(
                    div()
                        .flex()
                        .flex_col()
                        .min_w_0()
                        .child(
                            line(
                                e.name.clone(),
                                text::BTN,
                                line_height(text::BTN, 1.3),
                                color::FG,
                            )
                            .ellipsis(),
                        )
                        .child(
                            line(
                                meta,
                                text::MICRO,
                                line_height(text::MICRO, 1.3),
                                if failed { color::DANGER } else { color::FG_3 },
                            )
                            .ellipsis(),
                        ),
                )
                .into_any_element()
        }
    };
    let fill = match e.step {
        Step::Ready => color::OK,
        Step::Failed => color::DANGER,
        _ => color::ACCENT,
    };
    let on_remove = e.on_remove.clone();
    let enter_id = gpui::ElementId::Name(format!("tray-enter-{}", e.id).into());
    let item = div()
        .id(gpui::ElementId::Name(format!("tray-{}", e.id).into()))
        .relative()
        .flex_none()
        .flex()
        .flex_col()
        .overflow_hidden()
        .w(px(w + 2.0))
        .h(px(ITEM_H + 2.0))
        .bg(color::BG_3)
        .border_1()
        .border_color(if failed {
            color::DANGER_LINE
        } else {
            color::HAIRLINE_2
        })
        .rounded(radius::LG)
        .child(body)
        .child(
            div()
                .flex_none()
                .h(px(BAR))
                .bg(color::HOVER_HI)
                .child(div().h_full().w(relative(e.step.fill())).bg(fill)),
        )
        .child(
            div()
                .id(gpui::ElementId::Name(
                    format!("tray-remove-{}", e.id).into(),
                ))
                .a11y(Role::Button, e.remove_label.clone())
                .absolute()
                .top(px(4.0))
                .right(px(4.0))
                .size(px(REMOVE))
                .flex()
                .items_center()
                .justify_center()
                .rounded(radius::PILL)
                .bg(color::SCRIM_TAG)
                .tab_index(0)
                .cursor_pointer()
                .hover(|s| s.bg(color::DANGER))
                .focus_visible(|s| s.shadow(focus_ring()))
                .on_click(move |ev, w, cx| on_remove(ev, w, cx))
                .child(Icon::new(IconName::X).size(px(11.0)).color(color::FG_CHIP)),
        );
    // AttachmentTray.css:5: each item rises in (`d-up` over `--duration-toggle`, `ease`) when it joins the tray.
    crate::motion::up_in(
        item,
        enter_id,
        std::time::Duration::ZERO,
        motion::TOGGLE,
        crate::motion::EASE,
        cx,
    )
}

/// The tray, or `None` while it is empty (nothing renders).
pub fn tray(entries: Vec<TrayEntry>, cx: &App) -> Option<AnyElement> {
    if entries.is_empty() {
        return None;
    }
    Some(
        div()
            .flex()
            .gap(px(GAP))
            .pb(px(GAP))
            .overflow_hidden()
            .children(entries.into_iter().map(|e| card(e, cx)))
            .into_any_element(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_progress_line_fills_in_steps() {
        let fills: Vec<f32> = [
            Step::Reading,
            Step::Preparing,
            Step::Uploading,
            Step::Ready,
            Step::Failed,
        ]
        .iter()
        .map(|s| s.fill())
        .collect();
        assert_eq!(fills, [0.25, 0.5, 0.75, 1.0, 1.0]);
        assert_eq!(height(), 94.0);
    }

    /// A picture's preview follows the card's rounded top corners (radius `lg` inside its 1px border); the progress
    /// line is under it, so its bottom corners stay square.
    #[gpui::test]
    fn a_preview_follows_the_card_s_rounded_corners(cx: &mut gpui::TestAppContext) {
        use crate::picture::{
            inner_radius,
            testing::{picture_corners, png},
        };
        let image = png();
        let corners = picture_corners(cx, move |_, cx| {
            card(
                TrayEntry {
                    id: "1".into(),
                    name: "a.png".into(),
                    size: "1 KB".into(),
                    phase: "".into(),
                    step: Step::Ready,
                    preview: Preview::Image(image.clone()),
                    remove_label: "".into(),
                    on_remove: std::rc::Rc::new(|_, _, _| {}),
                },
                cx,
            )
        });
        let c = corners.last().expect("the preview");
        let inner = inner_radius(radius::LG, px(1.0));
        assert!(c.top_left >= inner && c.top_right >= inner, "{c:?}");
        assert_eq!((c.bottom_left, c.bottom_right), (px(0.0), px(0.0)));
    }
}
