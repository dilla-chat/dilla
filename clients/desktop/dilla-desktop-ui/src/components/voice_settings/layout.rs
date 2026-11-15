//! The Voice section's page furniture (packages/ui VoiceSettings.css `.d-voice-settings__*`): bordered groups of 16px
//! padding and a 10px radius under 10px uppercase headings, 14px apart inside and 16px apart; labels, notes and
//! descriptions; the volume row under a device; the status pill and the readouts.
//!
//! Text wraps at the width of the frame's content column ([`crate::components::settings::frame::section_width`], 720px) less
//! the group's border and padding, as the web lays it out.

use gpui::{
    AnyElement, Div, ElementId, Hsla, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, Styled, div, px,
};

use super::slider::Slider;
use crate::components::settings::frame::section_width;
use crate::metrics::LABEL_TRACKING_WIDE;
use crate::text::{line, line_height, paragraph};
use crate::tokens::{color, font, radius, text};

/// A group's inner width: the content column less 2px of border and 2 x 16px of padding.
pub fn group_inner() -> f32 {
    section_width(false) - 2.0 - 32.0
}

/// One of two columns of a group's grid (18px apart).
pub fn column_width() -> f32 {
    (group_inner() - 18.0) / 2.0
}

/// The section: its head (whose sub-line keeps the 22px over the groups, `h2 + p + *`), the groups and the status lines
/// under them. The frame's section rule (`.d-settings-frame__body > section`, gap 0) beats `.dw-settings-section`'s gap:
/// the parts are spaced by their own margins.
pub fn page() -> Div {
    div().flex().flex_col().min_w_0()
}

/// A line under the groups: 14px below the block above (`section > * + *`).
pub fn status() -> Div {
    div().pt(px(14.0)).min_w_0()
}

/// The head of a section's text (title and sub-line) and its banners, stacked.
pub fn head() -> Div {
    div().flex().flex_col().min_w_0()
}

/// The mic test's head: the heading, then Let's check on the right, wrapping, 10px apart.
pub fn test_head() -> Div {
    div().flex().flex_wrap().items_center().gap(px(10.0))
}

/// The four groups, 16px apart.
pub fn panel() -> Div {
    div().flex().flex_col().gap(px(16.0)).min_w_0()
}

/// A bordered group: hairline, 10px radius, 16px padding, 14px between its parts.
pub fn group() -> Div {
    div()
        .flex()
        .flex_col()
        .gap(px(14.0))
        .min_w_0()
        .p(px(16.0))
        .border_1()
        .border_color(color::HAIRLINE)
        .rounded(radius::XL)
}

/// A group's micro heading (10px 600, 0.12em, uppercase, `--fg-3`).
pub fn heading(text_: &str) -> Div {
    div().child(crate::a11y::heading(
        3,
        text_.to_owned(),
        line(
            text_.to_uppercase(),
            text::MICRO,
            line_height(text::MICRO, 1.5),
            color::FG_3,
        )
        .weight(font::WEIGHT_LABEL)
        .tracking(LABEL_TRACKING_WIDE),
    ))
}

/// The group's two-column grid (18px apart).
pub fn grid() -> Div {
    div().flex().gap(px(18.0)).min_w_0()
}

/// One cell of the grid: its parts 8px apart.
pub fn field() -> Div {
    div()
        .flex()
        .flex_col()
        .flex_1()
        .gap(px(8.0))
        .min_w_0()
        .items_stretch()
}

/// A field's label (11px `--fg-2`, its 16.5px line).
pub fn label(text_: impl Into<SharedString>) -> Div {
    div().child(line(
        text_,
        text::XS,
        line_height(text::XS, 1.5),
        color::FG_2,
    ))
}

fn lines(text_: &str, size: gpui::Pixels, height_ratio: f32, colour: Hsla, width: f32) -> Div {
    div().flex().flex_col().children(
        paragraph(text_, size, line_height(size, height_ratio), colour, width)
            .into_iter()
            .map(IntoElement::into_any_element),
    )
}

/// A note under a control (10.5px `--fg-3`), wrapped to `width`.
pub fn note(text_: &str, width: f32) -> Div {
    lines(text_, text::META, 1.5, color::FG_3, width)
}

/// A group's description (11.5px `--fg-2`), wrapped to `width`.
pub fn description(text_: &str, width: f32) -> Div {
    lines(text_, text::BTN, 1.5, color::FG_2, width)
}

/// The mic test's description (`.d-voice-settings__desc`): 4px into the 14px above it (`margin: -4px 0 0`).
pub fn test_description(text_: &str, width: f32) -> Div {
    description(text_, width).mt(px(-4.0))
}

/// A row's own description (11px `--fg-2`), wrapped to `width`.
pub fn row_hint(text_: &str, width: f32) -> Div {
    lines(text_, text::XS, 1.4, color::FG_2, width)
}

/// A row's label (12.5px 600).
pub fn row_label(text_: &str, tag: Option<&str>) -> Div {
    let mut row = div().flex().items_center().gap(px(8.0)).child(
        line(
            text_.to_owned(),
            text::SM,
            line_height(text::SM, 1.4),
            color::FG,
        )
        .weight(font::WEIGHT_LABEL),
    );
    if let Some(tag) = tag {
        row = row.child(
            div()
                .px(px(5.0))
                .rounded(radius::TAG)
                .bg(color::ACCENT_TAG)
                .child(
                    line(
                        tag.to_owned(),
                        text::CHIP,
                        line_height(text::CHIP, 1.4),
                        color::ACCENT_2,
                    )
                    .weight(font::WEIGHT_LABEL)
                    .tracking(0.06),
                ),
        );
    }
    row
}

/// The volume row under a device: "Volume" (10.5px `--fg-3`, 54px), the 0-200 % range, the value (11px, 40px, right).
pub fn volume_row(label_: &str, slider: Slider, value: &str, value_width: f32) -> Div {
    div()
        .flex()
        .items_center()
        .gap(px(10.0))
        .min_w_0()
        .child(div().flex_none().w(px(54.0)).child(line(
            label_.to_owned(),
            text::META,
            line_height(text::META, 1.5),
            color::FG_3,
        )))
        .child(slider)
        .child(
            div()
                .flex_none()
                .w(px(value_width))
                .flex()
                .justify_end()
                .child(line(
                    value.to_owned(),
                    text::XS,
                    line_height(text::XS, 1.5),
                    color::FG,
                )),
        )
}

/// The release delay's row (`.d-voice-settings__ptt .d-voice-settings__volume`): the range and its value, at least 24px high.
pub fn volume_row_bare(slider: Slider, value: &str, value_width: f32) -> Div {
    div()
        .flex()
        .items_center()
        .gap(px(10.0))
        .min_h(px(24.0))
        .min_w_0()
        .child(slider)
        .child(
            div()
                .flex_none()
                .w(px(value_width))
                .flex()
                .justify_end()
                .child(line(
                    value.to_owned(),
                    text::XS,
                    line_height(text::XS, 1.5),
                    color::FG,
                )),
        )
}

/// The width of a [`Segmented`](crate::components::segmented::Segmented) with these labels: 2px of padding and 1px of border
/// around options of 10px padding and the label's 11.5px mono advance.
pub fn segmented_width(labels: &[&str]) -> f32 {
    let advance = f32::from(text::BTN) * crate::text::MONO_ADVANCE;
    2.0 * 3.0
        + labels
            .iter()
            .map(|l| 20.0 + l.chars().count() as f32 * advance)
            .sum::<f32>()
}

/// The width of a volume value (`40px`), and of the release delay's (`56px`).
pub const VOLUME_VALUE_W: f32 = 40.0;
pub const RELEASE_VALUE_W: f32 = 56.0;

/// Where the mic test pill stands.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Heard {
    Idle,
    Quiet,
    Heard,
}

/// The status pill: 26px (plus its 1px border), 11px; idle `--fg-3` in a `--control-edge-field` outline, heard `--ok` on
/// its tint, too quiet `--warn` on its tint.
#[derive(IntoElement)]
pub struct StatusPill {
    id: ElementId,
    state: Heard,
    text: SharedString,
}

impl StatusPill {
    pub fn new(id: impl Into<ElementId>, state: Heard, text: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            state,
            text: text.into(),
        }
    }
}

impl RenderOnce for StatusPill {
    fn render(self, _window: &mut gpui::Window, _cx: &mut gpui::App) -> impl IntoElement {
        let (fg, bg, edge) = match self.state {
            Heard::Idle => (
                color::FG_3,
                gpui::transparent_black(),
                color::CONTROL_EDGE_FIELD,
            ),
            Heard::Heard => (color::OK, color::OK_TINT, color::OK_LINE),
            Heard::Quiet => (color::WARN, color::WARN_FAINT, color::WARN_LINE),
        };
        div()
            .id(self.id)
            .flex()
            .flex_none()
            .items_center()
            .gap(px(7.0))
            .h(px(28.0))
            .px(px(10.0))
            .border_1()
            .border_color(edge)
            .rounded(radius::PILL)
            .bg(bg)
            .child(div().size(px(6.0)).rounded(radius::PILL).bg(fg))
            .child(line(self.text, text::XS, line_height(text::XS, 1.5), fg))
    }
}

/// A readout beside the pill (11px `--fg-2`).
pub fn stat(text_: impl Into<SharedString>) -> Div {
    div().child(line(
        text_,
        text::XS,
        line_height(text::XS, 1.5),
        color::FG_2,
    ))
}

/// The readout row: pill and readouts 10px apart, wrapping.
pub fn readout() -> Div {
    div()
        .flex()
        .flex_wrap()
        .items_center()
        .gap(px(10.0))
        .min_w_0()
}

/// A switch row: a hairline above, 12px under it, 14px between the text and the switch.
pub fn switch_row(label_: &str, hint: &str, switch: impl IntoElement) -> Div {
    div()
        .flex()
        .items_center()
        .gap(px(14.0))
        .pt(px(12.0))
        .border_t_1()
        .border_color(color::HAIRLINE)
        .child(
            div()
                .flex()
                .flex_col()
                .flex_1()
                .min_w_0()
                .child(row_label(label_, None))
                .child(row_hint(
                    hint,
                    group_inner() - 14.0 - crate::components::toggle::TRACK_W,
                )),
        )
        .child(div().flex_none().child(switch))
}

/// A processing row: the text (label, tag, hint) beside its control, wrapping.
pub fn control_row(text_col: impl IntoElement, control: impl IntoElement) -> Div {
    div()
        .flex()
        .flex_wrap()
        .items_center()
        .gap_x(px(14.0))
        .gap_y(px(10.0))
        .child(text_col)
        .child(control)
}

/// The capture prompt: a status region that is always present; empty it takes no room, shown it is a 3px accent bar, 6px 12px
/// of padding, 11px.
pub fn prompt(text_: Option<&str>, width: f32) -> Option<AnyElement> {
    // Empty, the web's prompt is taken out of the group's flow (`:empty { position: absolute }`): no gap above it.
    text_.map(|t| {
        div()
            .px(px(12.0))
            .py(px(6.0))
            .border_l(px(3.0))
            .border_color(color::ACCENT)
            .child(lines(t, text::XS, 1.5, color::FG, width - 15.0))
            .into_any_element()
    })
}
