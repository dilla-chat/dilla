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
use dilla_desktop_ui::components::text_field::TextField;
use dilla_desktop_ui::text::{line, line_height, paragraph};
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
    pub(super) fn profile_view(&mut self, _cx: &mut Context<Self>) -> AnyElement {
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

        let mut display =
            TextField::new("profile-display", t("profile.display.label"), &self.display);
        if let Some(text) = problem(e.problem_display) {
            display = display.error(text);
        } else if display_error {
            display = display.error(t("profile.error.display"));
        }
        let mut status = TextField::new("profile-status", t("profile.status.label"), &self.status);
        if let Some(text) = problem(e.problem_status) {
            status = status.error(text);
        }
        let mut bio = TextField::multiline("profile-bio", t("profile.bio.label"), &self.bio);
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
            .child(line(
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
            .child(
                div()
                    .flex()
                    .flex_none()
                    .rounded_full()
                    .bg(color::BG_3)
                    .p(px(AVATAR_RING))
                    .w(px(AVATAR_SIZE + 2.0 * AVATAR_RING))
                    .child(
                        Avatar::new("profile-preview-avatar", name.clone()).size(AvatarSize::Card),
                    ),
            )
            .child(
                div().mt(px(NAME_GAP)).child(
                    line(
                        name.clone(),
                        text::BASE,
                        line_height(text::BASE, 1.5),
                        color::FG,
                    )
                    .weight(font::WEIGHT_DISPLAY),
                ),
            )
            .child(line(
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
                PREVIEW_WIDTH - 2.0 * BODY_PAD,
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
