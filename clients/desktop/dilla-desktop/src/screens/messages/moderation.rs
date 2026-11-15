//! The moderated removal's dialog and its status line (Task 28 wave 2, lane A): the `Dialog` of Conversation.tsx titled
//! `shell.moderation.removeTitle` and the `.d-moderation-evidence-status` line under the log. The flow itself is
//! `dilla_client::features::moderation` (`remove_message`, `resolve_removal`, `send_removal_evidence`); the state is
//! [`super::ui::Moderating`].

use gpui::{AnyElement, App, Entity, IntoElement, ParentElement, Styled, Window, div};
use gpui_component::input::Textarea;

use dilla_desktop_ui::components::button::{Button, ButtonSize, ButtonVariant};
use dilla_desktop_ui::components::dialog::{Dialog, DialogKind};
use dilla_desktop_ui::text::{
    Face, chromium_baseline, columns, line, line_height, paragraph, wrap_mono,
};
use dilla_desktop_ui::tokens::{color, text};

use super::css::len;
use super::ui::{MessageUi, ModPhase, Moderating};
use crate::strings::t;

/// The reason's `<textarea>` as Chromium lays out one no stylesheet touches (`rows=2`, `cols=20`, the UA's 13.333px
/// monospace, its scrollbar gutter, 2px padding in a 1px edge): a 201 x 42 box, filled `#3b3b3b` in a `#858585` edge (the
/// UA's dark form colours), its text white. These are the engine's own box, not the app's CSS (no rule styles the
/// web's `<textarea>`), so they are the UA's numbers, each derived below (review A M3).
mod textarea {
    /// The UA's `<textarea>` attributes the web leaves at their defaults.
    pub const ROWS: f32 = 2.0;
    /// `padding: 2px` on each side, in a `1px` edge.
    pub const PADDING: f32 = 2.0;
    pub const BORDER: f32 = 1.0;
    /// The content width Chromium gives `cols=20` (`LayoutTextControlMultiLine`: 20 × the UA monospace face's average
    /// advance at 13.333px, plus the scrollbar gutter it reserves), read from the reference's box: the face's metrics are
    /// the engine's, not a length any stylesheet names.
    pub const CONTENT_WIDTH: f32 = 195.0;
    pub const WIDTH: f32 = CONTENT_WIDTH + 2.0 * (PADDING + BORDER);
    /// rows × the line, 2 × 2px padding, 2 × 1px edge: 2 × 18 + 4 + 2.
    pub const HEIGHT: f32 = ROWS * LINE + 2.0 * (PADDING + BORDER);
    pub const SIZE: f32 = 13.333;
    /// The UA's `line-height: normal` for its monospace face.
    pub const LINE: f32 = 18.0;
    pub const FILL: u32 = 0x3b3b3b;
    pub const EDGE: u32 = 0x858585;
}

/// The dialog while a removal is asked for, being sent, or shows its result; `None` otherwise (or once closed on an
/// unsettled result: [`status_line`] shows it then).
pub fn dialog(ui: &Entity<MessageUi>, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let s = ui.read(cx);
    let m = s.moderating.as_ref().filter(|m| m.open)?;
    let width = DialogKind::Form.body_width(f32::from(window.viewport_size().width));
    let phase = m.phase;
    let body = body(m, width);
    let footer = footer(ui, m, cx);
    let close = ui.clone();
    let close_label = if phase == ModPhase::Sending {
        t("shell.moderation.submitting")
    } else {
        t("shell.delete.cancel")
    };
    Some(
        Dialog::new(
            "message-moderate",
            t("shell.moderation.removeTitle"),
            close_label,
        )
        .body(body)
        .footer(footer)
        .can_close(phase != ModPhase::Sending)
        .on_close(move |_, cx| close.update(cx, |ui, cx| ui.close_moderate(cx)))
        .into_any_element(),
    )
}

/// `--text-ui` `--fg-2` paragraphs at the body's width.
fn para(text: &str, width: f32) -> gpui::Div {
    div().flex().flex_col().children(paragraph(
        text,
        text::UI,
        line_height(text::UI, 1.5),
        color::FG_2,
        width,
    ))
}

fn body(m: &Moderating, width: f32) -> AnyElement {
    let lh = line_height(text::UI, 1.5);
    match m.phase {
        ModPhase::Confirm => {
            // `<p>` margins are 1em of the body's 12px; the first and last child lose their outer ones.
            let gap = f32::from(text::UI);
            div()
                .flex()
                .flex_col()
                .gap(len(gap))
                .child(para(t("shell.moderation.removeBody"), width))
                .child(reason_label(m))
                .child(excerpt_line(&m.excerpt, width, lh))
                .into_any_element()
        }
        ModPhase::Sending => para(t("shell.moderation.submitting"), width).into_any_element(),
        ModPhase::Result => para(t(m.result_key()), width).into_any_element(),
    }
}

/// `<label>Reason (required)<textarea/></label>`: the label's text, then the textarea on the same line. The textarea is
/// an inline-block whose bottom edge sits on the line's baseline, so the line box is the textarea's height plus the text's
/// depth below its baseline (`line_height - chromium_baseline`).
fn reason_label(m: &Moderating) -> AnyElement {
    use textarea::*;
    let lh = line_height(text::UI, 1.5);
    let depth = lh - chromium_baseline(Face::Mono, f32::from(text::UI), lh);
    div()
        .flex()
        .items_end()
        .child(line(
            t("shell.moderation.reason"),
            text::UI,
            lh,
            color::FG_2,
        ))
        .child(
            div().pb(len(depth)).child(crate::tracked_box(
                "moderation-reason",
                div()
                    .w(len(WIDTH))
                    .h(len(HEIGHT))
                    .border_1()
                    .border_color(gpui::rgb(EDGE))
                    .bg(gpui::rgb(FILL))
                    .text_color(gpui::white())
                    .overflow_hidden()
                    .child(super::textarea_origin(
                        Textarea::new(&m.reason)
                            .aria_label(t("shell.moderation.reason"))
                            .appearance(false)
                            .text_size(len(SIZE))
                            .line_height(len(LINE))
                            .w(len(WIDTH - 2.0 + super::TEXTAREA_WRAP_EXTRA))
                            .h(len(HEIGHT - 2.0)),
                    )),
            )),
        )
        .into_any_element()
}

/// `<p><strong>Message excerpt:</strong> {excerpt}</p>`, wrapped at the body's width; the label is bold.
fn excerpt_line(excerpt: &str, width: f32, lh: f32) -> AnyElement {
    let label = t("shell.moderation.excerptLabel");
    let shown = if excerpt.is_empty() {
        t("shell.moderation.emptyExcerpt")
    } else {
        excerpt
    };
    let whole = format!("{label} {shown}");
    let mut taken = 0usize;
    let label_chars = label.chars().count();
    let mut col = div().flex().flex_col();
    for l in wrap_mono(&whole, columns(text::UI, width)) {
        let n = l.chars().count();
        let bold = label_chars.saturating_sub(taken).min(n);
        taken += n;
        let (head, tail): (String, String) = (
            l.chars().take(bold).collect(),
            l.chars().skip(bold).collect(),
        );
        let mut row = div().flex();
        if !head.is_empty() {
            row = row.child(line(head, text::UI, lh, color::FG_2).weight(gpui::FontWeight::BOLD));
        }
        if !tail.is_empty() {
            row = row.child(line(tail, text::UI, lh, color::FG_2));
        }
        col = col.child(row);
    }
    col.into_any_element()
}

/// The footer's action: Remove (danger; blocked, `aria-disabled`, on a blank or too long reason, where it does nothing), the
/// blocked "Submitting…", Done once the evidence is stored, else the retry that fits.
fn footer(ui: &Entity<MessageUi>, m: &Moderating, cx: &App) -> AnyElement {
    let button = match m.phase {
        ModPhase::Confirm => {
            let ui = ui.clone();
            let blocked = !m.can_remove(cx);
            Button::new(
                "message-moderate-confirm",
                t("shell.moderation.removeConfirm"),
            )
            .variant(ButtonVariant::Danger)
            .size(ButtonSize::Dialog)
            .blocked(blocked)
            .on_click(move |_, window, cx| {
                ui.update(cx, |ui, cx| ui.remove_for_everyone(window, cx));
            })
        }
        ModPhase::Sending => {
            Button::new("message-moderate-confirm", t("shell.moderation.submitting"))
                .size(ButtonSize::Dialog)
                .blocked(true)
        }
        ModPhase::Result if m.settled() => {
            let ui = ui.clone();
            Button::new("message-moderate-confirm", t("shell.moderation.done"))
                .size(ButtonSize::Dialog)
                .on_click(move |_, _, cx| ui.update(cx, |ui, cx| ui.close_moderate(cx)))
        }
        ModPhase::Result => {
            let ui = ui.clone();
            Button::new("message-moderate-confirm", t(m.retry_key()))
                .variant(ButtonVariant::Danger)
                .size(ButtonSize::Dialog)
                .on_click(move |_, _, cx| ui.update(cx, |ui, cx| ui.retry_moderation(cx)))
        }
    };
    crate::tracked_box("message-moderate-confirm", button).into_any_element()
}

/// Conversation.tsx `.d-moderation-evidence-status`: a result the dialog was closed on before it settled, with its retry
/// (or Done), under the log. `None` when there is none.
pub fn status_line(ui: &Entity<MessageUi>, cx: &App) -> Option<AnyElement> {
    let m = ui.read(cx).moderating.as_ref()?;
    if m.open || m.phase != ModPhase::Result {
        return None;
    }
    let action = if m.settled() {
        let ui = ui.clone();
        Button::new("moderation-status-action", t("shell.moderation.done"))
            .on_click(move |_, _, cx| ui.update(cx, |ui, cx| ui.close_moderate(cx)))
    } else {
        let ui = ui.clone();
        Button::new("moderation-status-action", t(m.retry_key()))
            .on_click(move |_, _, cx| ui.update(cx, |ui, cx| ui.retry_moderation(cx)))
    };
    // Review A M2: `.d-moderation-evidence-status` has no rule, so it is the UA's block `div` (no padding) holding a `<p>`
    // with its 1em block margins (`text::UI`), then the Button on its own line.
    Some(
        div()
            .flex()
            .flex_col()
            .child(div().my(text::UI).child(line(
                t(m.result_key()),
                text::UI,
                line_height(text::UI, 1.5),
                color::FG_2,
            )))
            .child(div().flex().child(action))
            .into_any_element(),
    )
}
