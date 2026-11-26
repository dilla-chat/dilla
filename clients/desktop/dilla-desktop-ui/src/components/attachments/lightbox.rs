//! `Lightbox` (packages/ui/src/Lightbox): one image over the whole window on an opaque `bg`, contained above a `bg-2`
//! bar of its label (12px `fg-2`) and its buttons: previous and next (ghost) when there are any, save, close (ghost).
//! Escape, ArrowLeft and ArrowRight are bound by the app (`screens::attachments::keys`).

use std::sync::Arc;

use gpui::{
    AnyElement, App, FocusHandle, Image, InteractiveElement, IntoElement, ObjectFit, ParentElement,
    SharedString, StatefulInteractiveElement, Styled, StyledImage, div, img, px,
};

use super::OnClick;
use crate::components::button::{Button, ButtonVariant};
use crate::text::line;
use crate::tokens::{color, motion, radius, text};

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
    Button::new(id, label)
        .variant(variant)
        .on_click(move |e, w, cx| f(e, w, cx))
}

/// The lightbox, filling its (window-sized) parent.
pub fn lightbox(p: LightboxProps, cx: &App) -> AnyElement {
    let stage: AnyElement = match p.image {
        // The id keeps an opened GIF's frame clock: GPUI's `Img` animates only with one.
        Some(image) => img(image)
            .id("lightbox-img")
            .debug_selector(|| "lightbox-img".to_owned())
            .max_w_full()
            .max_h_full()
            .object_fit(ObjectFit::Contain)
            // A click on the picture is not one outside it (below).
            .on_click(|_, _, cx| cx.stop_propagation())
            .into_any_element(),
        None => div()
            .id("lightbox-box")
            .on_click(|_, _, cx| cx.stop_propagation())
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
    let backdrop_close = p.on_close.clone();
    // Lightbox.css:5: the dialog fades in with `d-fade` over `--duration-scrim` (the CSS's default curve).
    let lightbox = div()
        .id("lightbox")
        .absolute()
        .inset_0()
        .flex()
        .flex_col()
        .bg(color::BG)
        .text_color(color::FG)
        .occlude()
        .child(
            // The backdrop around the picture: a click on it closes the lightbox (Jonas, 2026-11-26), as Escape and
            // the close button do; the picture stops its own clicks, and the bar is beside it, not in it.
            div()
                .id("lightbox-stage")
                .flex_1()
                .min_h_0()
                .flex()
                .items_center()
                .justify_center()
                .p(px(STAGE_PAD))
                .on_click(move |e, w, cx| backdrop_close(e, w, cx))
                .child(stage),
        )
        .child(
            div()
                .debug_selector(|| "lightbox-bar".to_owned())
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
                        line(
                            p.label,
                            text::UI,
                            crate::text::root_line_height(text::UI),
                            color::FG_2,
                        )
                        .ellipsis(),
                    ),
                )
                .child(nav)
                .child(
                    div()
                        .flex()
                        .gap(px(8.0))
                        .child(button(
                            "lightbox-save",
                            p.save_label,
                            ButtonVariant::Default,
                            p.on_save,
                        ))
                        .child(
                            Button::new("lightbox-close", p.close_label)
                                .variant(ButtonVariant::Ghost)
                                .focus_handle(&p.close_focus)
                                .on_click(move |e, w, cx| on_close(e, w, cx)),
                        ),
                ),
        );
    crate::motion::fade_in(
        lightbox,
        "lightbox-fade",
        motion::SCRIM,
        crate::motion::EASE,
        cx,
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn props(image: Option<Arc<Image>>, cx: &mut App) -> LightboxProps {
        let none: OnClick = std::rc::Rc::new(|_, _, _| {});
        LightboxProps {
            label: "".into(),
            image,
            save_label: "".into(),
            on_save: none.clone(),
            close_label: "".into(),
            on_close: none,
            close_focus: cx.focus_handle(),
            previous: None,
            next: None,
        }
    }

    /// An opened GIF plays (its picture has an id): a render a frame's delay later paints its next frame; under reduced
    /// motion it holds its first.
    #[gpui::test]
    fn an_opened_gif_paints_its_next_frame(cx: &mut gpui::TestAppContext) {
        use crate::components::attachments::testing::{frames_painted, gif};
        let image = gif();
        let draw =
            move |_: &mut gpui::Window, cx: &mut App| lightbox(props(Some(image.clone()), cx), cx);
        assert_eq!(frames_painted(cx, false, draw.clone()), 2, "it plays");
        assert_eq!(frames_painted(cx, true, draw), 1, "reduced motion");
    }

    /// A click on the backdrop, outside the picture and its bar, closes the lightbox (Jonas, 2026-11-26; the old
    /// `<dialog>` did not); a click on the picture or on the bar does not.
    #[gpui::test]
    fn a_click_outside_the_picture_closes_it(cx: &mut gpui::TestAppContext) {
        use std::cell::Cell;
        use std::rc::Rc;

        use gpui::{Context, Modifiers, Render, Window};

        struct Host {
            closes: Rc<Cell<usize>>,
            image: Arc<Image>,
        }
        impl Render for Host {
            fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
                let closes = self.closes.clone();
                let mut p = props(Some(self.image.clone()), cx);
                p.on_close = Rc::new(move |_, _, _| closes.set(closes.get() + 1));
                div().relative().size_full().child(lightbox(p, cx))
            }
        }

        cx.update(|cx| cx.set_reduce_motion(true));
        let closes = Rc::new(Cell::new(0));
        let (_, vcx) = cx.add_window_view(|_, _| Host {
            closes: closes.clone(),
            image: crate::components::attachments::testing::gif_sized(200, 100),
        });
        vcx.run_until_parked();
        let click = |vcx: &mut gpui::VisualTestContext, at: gpui::Point<gpui::Pixels>| {
            vcx.update(|window, cx| window.draw(cx).clear(cx));
            vcx.simulate_click(at, Modifiers::default());
            vcx.run_until_parked();
        };
        let picture = vcx.debug_bounds("lightbox-img").expect("the picture");
        let bar = vcx.debug_bounds("lightbox-bar").expect("the bar");
        assert!(
            picture.size.width > px(100.0),
            "the picture is drawn at its size"
        );
        click(vcx, picture.center());
        assert_eq!(closes.get(), 0, "a click on the picture");
        click(vcx, gpui::point(bar.left() + px(4.0), bar.center().y));
        assert_eq!(closes.get(), 0, "a click on the bar");
        click(vcx, gpui::point(px(4.0), px(4.0)));
        assert_eq!(closes.get(), 1, "a click on the backdrop");
    }
}
