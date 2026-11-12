//! `Lightbox` (packages/ui/src/Lightbox): one image over the whole window on an opaque `bg`, contained above a `bg-2`
//! bar of its label (12px `fg-2`) and its buttons: previous and next (ghost) when there are any, save, close (ghost).
//! Escape, ArrowLeft and ArrowRight are bound by the app (`screens::attachments::keys`).

use std::sync::Arc;

use gpui::{
    AnyElement, FocusHandle, Image, InteractiveElement, IntoElement, ObjectFit, ParentElement,
    SharedString, Styled, StyledImage, div, img, px,
};

use super::{LEADING, OnClick};
use crate::components::button::{Button, ButtonVariant};
use crate::text::{line, line_height};
use crate::tokens::{color, radius, text};

/// `.d-lightbox__stage { padding: 1rem }`.
const STAGE_PAD: f32 = 16.0;
/// `.d-lightbox__box`: at most 40rem wide, 4:3.
const BOX_W: f32 = 640.0;

/// What the lightbox shows.
pub struct LightboxProps {
    pub label: SharedString,
    /// `None` while the image is opened: a neutral box stands in.
    pub image: Option<Arc<Image>>,
    pub save_label: SharedString,
    pub on_save: OnClick,
    pub close_label: SharedString,
    pub on_close: OnClick,
    /// The close button's handle: it takes focus when the lightbox opens.
    pub close_focus: FocusHandle,
    pub previous: Option<(SharedString, OnClick)>,
    pub next: Option<(SharedString, OnClick)>,
}

fn button(id: &'static str, label: SharedString, variant: ButtonVariant, f: OnClick) -> Button {
    Button::new(id, label).variant(variant).on_click(move |e, w, cx| f(e, w, cx))
}

/// The lightbox, filling its (window-sized) parent.
pub fn lightbox(p: LightboxProps) -> AnyElement {
    let stage: AnyElement = match p.image {
        Some(image) => img(image)
            .max_w_full()
            .max_h_full()
            .object_fit(ObjectFit::Contain)
            .into_any_element(),
        None => div()
            .w(px(BOX_W))
            .h(px(BOX_W * 3.0 / 4.0))
            .max_w_full()
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(color::HAIRLINE_2)
            .rounded(radius::MD)
            .into_any_element(),
    };
    let mut nav = div().flex().gap(px(8.0));
    if let Some((label, f)) = p.previous {
        nav = nav.child(button("lightbox-previous", label, ButtonVariant::Ghost, f));
    }
    if let Some((label, f)) = p.next {
        nav = nav.child(button("lightbox-next", label, ButtonVariant::Ghost, f));
    }
    let on_close = p.on_close.clone();
    div()
        .id("lightbox")
        .absolute()
        .inset_0()
        .flex()
        .flex_col()
        .bg(color::BG)
        .text_color(color::FG)
        .occlude()
        .child(
            div()
                .flex_1()
                .min_h_0()
                .flex()
                .items_center()
                .justify_center()
                .p(px(STAGE_PAD))
                .child(stage),
        )
        .child(
            div()
                .flex()
                .flex_wrap()
                .items_center()
                .gap_x(px(16.0))
                .gap_y(px(8.0))
                .px(px(16.0))
                .py(px(10.0))
                .bg(color::BG_2)
                .border_t_1()
                .border_color(color::HAIRLINE)
                .child(
                    div().flex_1().min_w_0().child(
                        line(p.label, text::UI, line_height(text::UI, LEADING), color::FG_2).ellipsis(),
                    ),
                )
                .child(nav)
                .child(
                    div()
                        .flex()
                        .gap(px(8.0))
                        .child(button("lightbox-save", p.save_label, ButtonVariant::Default, p.on_save))
                        .child(
                            Button::new("lightbox-close", p.close_label)
                                .variant(ButtonVariant::Ghost)
                                .focus_handle(&p.close_focus)
                                .on_click(move |e, w, cx| on_close(e, w, cx)),
                        ),
                ),
        )
        .into_any_element()
}
