//! Settings -> Profile (packages/web/src/screens/settings/Profile.tsx): the display name, a one-line status message and a bio.
//! The instance stores them readable, and the section says so. The form is the draft of [`super::draft`]; this file draws it.
//!
//! Layout (`app.css` `.dw-profile`): the form at the left and the prototype's preview card at the right (260px, 24px apart),
//! the card showing how others see you as you type.

use gpui::{
    AnyElement, Context, Hsla, IntoElement, ParentElement, Styled, div, hsla, linear_color_stop,
    linear_gradient, px,
};

use dilla_desktop_ui::components::avatar::{Avatar, AvatarSize, colour_of};
use dilla_desktop_ui::components::banner::{Banner, BannerTone};
use dilla_desktop_ui::components::settings::frame::section_header;
use dilla_desktop_ui::components::text_field::{Frame, TextField};
use dilla_desktop_ui::text::{flow_paragraph, line, line_height, paragraph};
use dilla_desktop_ui::tokens::{color, font, radius, text};

use super::draft::Failure;
use super::section::UserSettings;
use crate::strings::{t, tf};

/// `.dw-profile`: the preview card's column and the gap between it and the form.
const PREVIEW_WIDTH: f32 = 260.0;
const COLUMN_GAP: f32 = 24.0;
/// `.dw-profile-form`: the gap between fields.
const FORM_GAP: f32 = 14.0;
/// `.dw-profile-preview__band`: the band's height; the body's inset; the avatar overlaps the band by 24px.
const BAND_HEIGHT: f32 = 52.0;
const BODY_PAD: f32 = 14.0;
const AVATAR_LIFT: f32 = 24.0;
/// `.dw-profile-preview__body .d-avatar`: the avatar's 4px ring in the card's colour.
const AVATAR_RING: f32 = 4.0;
/// `.dw-profile-preview__body .d-avatar`: 48px.
const AVATAR_SIZE: f32 = 48.0;
const NAME_GAP: f32 = 8.0;
/// The band's two stops: `hsl(hue s 32%)` to `hsl(hue s 18%)` at 135 degrees.
const BAND_LIGHT: f32 = 0.32;
const BAND_DARK: f32 = 0.18;
const BAND_ANGLE: f32 = 135.0;
/// `.dw-profile-form__saved`: the status line sits 4px under the form.
const NOTE_PAD: f32 = 4.0;
/// `.dw-profile-form .d-text-field__input { height: 2.375rem }`.
const INPUT_HEIGHT: f32 = 38.0;
/// `.dw-profile-form textarea.d-text-field__input`: `padding: 10px 12px; line-height: 1.5`, three rows, the 1px edge.
const BIO_PAD_Y: f32 = 10.0;
const BIO_ROWS: f32 = 3.0;
const EDGE: f32 = 1.0;

/// The bio's frame: three rows of 13px at 1.5, its padding and edges.
fn bio_frame() -> Frame {
    Frame {
        height: BIO_ROWS * line_height(text::INPUT, 1.5) + 2.0 * (BIO_PAD_Y + EDGE),
        pad_y: BIO_PAD_Y,
        line: line_height(text::INPUT, 1.5),
    }
}

/// The band's colour at a lightness, from the avatar palette entry of the shown name.
fn band_stop(name: &str, lightness: f32) -> Hsla {
    let (hue, saturation, _) = colour_of(name);
    hsla(
        f32::from(hue) / 360.0,
        f32::from(saturation) / 100.0,
        lightness,
        1.0,
    )
}

impl UserSettings {
    pub(super) fn profile_view(&mut self, cx: &mut Context<Self>) -> AnyElement {
        let username = self.ctx.state.account.username.clone();
        let (e, preview, failed, pending, done, display_error, dirty) = {
            let d = self.draft.borrow();
            (
                d.evaluate(),
                d.preview(&username),
                d.failed,
                d.pending,
                d.done,
                d.display_error,
                d.evaluate().dirty,
            )
        };
        let problem = |p: Option<dilla_client::features::profile::ProfileTextProblem>| {
            p.map(|p| t(p.string_key()))
        };

        let input = Frame {
            height: INPUT_HEIGHT,
            pad_y: 0.0,
            line: line_height(text::INPUT, 1.5),
        };
        // The typed name's and status's line boxes for the R13 mask (automation build only), as the bio's.
        let typed_line = |value: &str| {
            crate::screens::composer::typed_line(div().size_full(), value, input.line)
        };
        let mut display =
            TextField::new("profile-display", t("profile.display.label"), &self.display)
                .frame(input)
                .text_overlay(typed_line(&self.display.read(cx).value()));
        if let Some(text) = problem(e.problem_display) {
            display = display.error(text);
        } else if display_error {
            display = display.error(t("profile.error.display"));
        }
        let mut status =
            TextField::new("profile-status", t("profile.status.label"), &self.status)
                .frame(input)
                .text_overlay(typed_line(&self.status.read(cx).value()));
        if let Some(text) = problem(e.problem_status) {
            status = status.error(text);
        }
        // The typed bio's line boxes for the R13 mask (automation build only), over the field's text box.
        let typed = crate::screens::composer::typed_text(
            div().absolute().top_0().left_0().size_full(),
            &self.bio.read(cx).value(),
            bio_frame().line,
        );
        let mut bio = TextField::multiline("profile-bio", t("profile.bio.label"), &self.bio)
            .frame(bio_frame())
            .text_overlay(typed);
        if let Some(text) = problem(e.problem_bio) {
            bio = bio.error(text);
        }

        let saved_line = if pending {
            t("profile.saving")
        } else if done && !dirty {
            t("profile.saved")
        } else {
            ""
        };
        let form = div()
            .flex()
            .flex_col()
            .flex_1()
            .min_w_0()
            .gap(px(FORM_GAP))
            .child(display)
            .child(status)
            .child(bio)
            // `.dw-profile-form__note` wraps at the form's width.
            .child(flow_paragraph(
                t("profile.footnote"),
                text::XS,
                line_height(text::XS, 1.5),
                color::FG_3,
            ))
            .child(div().pt(px(NOTE_PAD)).child(line(
                saved_line,
                text::XS,
                line_height(text::XS, 1.5),
                color::FG_3,
            )));

        // The prototype's preview: how the card shows you, as you type.
        let name = preview.display.clone();
        let handle_line = if preview.status_msg.is_empty() {
            format!("@{username}")
        } else {
            tf(
                "you.line",
                &[("handle", &username), ("status", &preview.status_msg)],
            )
        };
        let mut body = div()
            .px(px(BODY_PAD))
            .pb(px(BODY_PAD))
            .mt(px(-AVATAR_LIFT))
            // The ring is a box-shadow: it takes no room, so the avatar's 48px box is what lays out (the ring's wrapper
            // reaches 4px past it on every side).
            .child(
                div()
                    .flex()
                    .flex_none()
                    .rounded_full()
                    .bg(color::BG_3)
                    .p(px(AVATAR_RING))
                    .m(px(-AVATAR_RING))
                    .w(px(AVATAR_SIZE + 2.0 * AVATAR_RING))
                    .child(
                        Avatar::new("profile-preview-avatar", name.clone())
                            .size(AvatarSize::Preview),
                    ),
            )
            // `.dw-profile-preview__name`: 14px (`--text-md`), bold.
            .child(
                div().mt(px(NAME_GAP)).child(
                    flow_paragraph(
                        name.clone(),
                        text::MD,
                        line_height(text::MD, 1.5),
                        color::FG,
                    )
                    .weight(font::WEIGHT_DISPLAY),
                ),
            )
            .child(flow_paragraph(
                handle_line,
                text::XS,
                line_height(text::XS, 1.5),
                color::FG_2,
            ));
        if !preview.bio.is_empty() {
            body = body.child(div().mt(px(NAME_GAP)).children(paragraph(
                &preview.bio,
                text::BTN,
                line_height(text::BTN, 1.5),
                color::FG_STRONG,
                PREVIEW_WIDTH - 2.0 * (EDGE + BODY_PAD),
            )));
        }
        let card = div()
            .flex()
            .flex_col()
            .flex_none()
            .w(px(PREVIEW_WIDTH))
            .overflow_hidden()
            .bg(color::BG_3)
            .border_1()
            .border_color(color::CONTROL_EDGE_FIELD)
            .rounded(radius::XL)
            .child(div().h(px(BAND_HEIGHT)).bg(linear_gradient(
                BAND_ANGLE,
                linear_color_stop(band_stop(&name, BAND_LIGHT), 0.0),
                linear_color_stop(band_stop(&name, BAND_DARK), 1.0),
            )))
            .child(body);

        let mut col = div()
            .flex()
            .flex_col()
            .w_full()
            .child(section_header(t("profile.title"), Some(t("profile.body"))));
        if let Some(f) = failed {
            let key = match f {
                Failure::Text => "profile.error.chars",
                Failure::Wait => "profile.error.wait",
                Failure::Save => "profile.error.save",
            };
            col = col.child(div().pb(self.density().group_gap()).child(Banner::new(
                "profile-failed",
                BannerTone::Danger,
                t(key),
            )));
        }
        col.child(
            div()
                .flex()
                .items_start()
                .gap(px(COLUMN_GAP))
                .child(form)
                .child(card),
        )
        .into_any_element()
    }
}
