//! `MediaPlayer` inside its `AttachmentCard` (packages/ui/src/{MediaPlayer,AttachmentCard}): the name and size, the
//! inline player and save. Idle is one play button; loading is the state text; ready is the audio control bar (play or
//! pause, the time, the position and the volume); error is the error text and save. A video never plays inline in
//! this task (Jonas 2026-11-12): its card is the 16:9 stage tile whose play button opens the system player.

use std::time::Duration;

use gpui::{
    AnyElement, InteractiveElement, IntoElement, ParentElement, SharedString,
    StatefulInteractiveElement, Styled, div, px, relative,
};

use super::{LEADING, OnClick};
use crate::a11y::{Accessible, Role};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::focus_ring;
use crate::text::{line, line_height};
use crate::tokens::{color, radius, text};

/// `.d-attachment-card__media`: at most 31.5rem wide.
pub const MEDIA_W: f32 = 504.0;
/// `.d-attachment-card__media { padding: 0.5rem 0.75rem; gap: 0.375rem }`.
const PAD_Y: f32 = 8.0;
const PAD_X: f32 = 12.0;
const GAP: f32 = 6.0;
/// The player's and save's buttons: `min-height: 2.5rem; padding: 0 0.875rem` (flow 06).
const CONTROL_H: f32 = 40.0;
const CONTROL_PAD_X: f32 = 14.0;
/// Button.css `.d-btn__label { gap: 8px }` and `.d-media-player__glyph { margin-right: 0.5rem }`: the play glyph (14px)
/// is 8 + 8 px before its label.
const LABEL_GAP: f32 = 8.0;
const GLYPH: f32 = 14.0;
const GLYPH_MARGIN: f32 = 8.0;
/// `.d-attachment-card__meta { gap: 0.5rem }`; the error line's glyph (14px) and its 6px.
const META_GAP: f32 = 8.0;
const ERROR_GAP: f32 = 6.0;
/// The ready audio bar: Chromium's `<audio controls>` is 54px high on its 300px default width.
const BAR_H: f32 = 54.0;
const BAR_W: f32 = 186.0;
/// `.d-media-player` video frame: at most 30rem wide, 16:9.
const VIDEO_W: f32 = 480.0;

/// `MediaPlayerProps.state`.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum PlayerState {
    Idle,
    Loading,
    Ready {
        playing: bool,
        position: Duration,
        duration: Option<Duration>,
    },
    Error,
}

/// What a media card shows.
pub struct MediaCardProps {
    pub id: SharedString,
    pub video: bool,
    pub name: SharedString,
    pub size: SharedString,
    pub state: PlayerState,
    /// `media.play` with the name filled in.
    pub play_label: SharedString,
    pub loading_label: SharedString,
    pub error_label: SharedString,
    /// `media.save`, the visible text of save.
    pub save_text: SharedString,
    pub on_play: OnClick,
    pub on_toggle: OnClick,
    pub on_save: Option<OnClick>,
}

/// `m:ss`, as the audio control shows a time.
pub fn clock(d: Duration) -> String {
    let s = d.as_secs();
    format!("{}:{:02}", s / 60, s % 60)
}

fn meta_h() -> f32 {
    line_height(text::SM, LEADING)
}

fn player_h(video: bool, state: PlayerState) -> f32 {
    let error_h = line_height(text::UI, LEADING) + GAP;
    match (video, state) {
        (true, PlayerState::Idle | PlayerState::Loading) => VIDEO_W * 9.0 / 16.0,
        (_, PlayerState::Idle) => CONTROL_H,
        (_, PlayerState::Loading) => line_height(text::UI, LEADING),
        (_, PlayerState::Ready { .. }) => BAR_H,
        (_, PlayerState::Error) => error_h + CONTROL_H,
    }
}

/// The card's height for its state: its padding and line, the meta line, the player and save (absent in the error
/// state, where the player's own save stands in).
pub fn media_card_height(video: bool, state: PlayerState, save: bool) -> f32 {
    let save_h = if save && state != PlayerState::Error {
        GAP + CONTROL_H
    } else {
        0.0
    };
    2.0 + 2.0 * PAD_Y + meta_h() + GAP + player_h(video, state) + save_h
}

/// A mono line's width as the web lays it out: the font's 0.6 em advance per character, ceiled onto Chromium's layout
/// unit (as `text::Line` measures it, so a box this wide never cuts its text).
fn mono_w(s: &str, size: gpui::Pixels) -> f32 {
    let w = s.chars().count() as f32 * f32::from(size) * crate::text::MONO_ADVANCE;
    (w * 64.0 - 1e-3).ceil() / 64.0
}

/// A control button's width: its edges, its padding, the glyph and its gaps, the label.
fn control_w(label: &str, glyph: bool) -> f32 {
    let glyph = if glyph {
        GLYPH + GLYPH_MARGIN + LABEL_GAP
    } else {
        0.0
    };
    2.0 + 2.0 * CONTROL_PAD_X + glyph + mono_w(label, text::BTN)
}

/// The card's width: `.d-attachment-card__media` is `width: min(100%, 31.5rem)` in a row that sizes it to its content,
/// so it fits its widest line (the meta, the player, save) inside its padding and edges, up to 31.5rem.
pub fn media_card_width(p: &MediaCardProps) -> f32 {
    let meta = mono_w(&p.name, text::SM) + META_GAP + mono_w(&p.size, text::XS);
    let player = match (p.video, p.state) {
        (true, PlayerState::Idle | PlayerState::Loading) => VIDEO_W,
        (_, PlayerState::Idle) => control_w(&p.play_label, true),
        (_, PlayerState::Loading) => mono_w(&p.loading_label, text::UI),
        (_, PlayerState::Ready { .. }) => BAR_W,
        (_, PlayerState::Error) => {
            let line = GLYPH + ERROR_GAP + mono_w(&p.error_label, text::UI);
            let save = if p.on_save.is_some() {
                control_w(&p.save_text, false)
            } else {
                0.0
            };
            line.max(save)
        }
    };
    let save = if p.on_save.is_some() && p.state != PlayerState::Error {
        control_w(&p.save_text, false)
    } else {
        0.0
    };
    (meta.max(player).max(save) + 2.0 * PAD_X + 2.0).min(MEDIA_W)
}

fn control_button(id: SharedString, label: SharedString, glyph: bool, f: OnClick) -> AnyElement {
    let mut b = div()
        .id(gpui::ElementId::Name(id))
        .a11y(Role::Button, label.clone())
        .flex()
        .flex_none()
        .items_center()
        .h(px(CONTROL_H))
        .px(px(CONTROL_PAD_X))
        .max_w_full()
        .border_1()
        .border_color(color::CONTROL_EDGE)
        .rounded(radius::MD)
        .tab_index(0)
        .cursor_pointer()
        .hover(|s| s.bg(color::HOVER))
        .focus_visible(|s| s.shadow(focus_ring()))
        .on_click(move |e, w, cx| f(e, w, cx));
    if glyph {
        // The glyph's own margin, then the label's gap before the text.
        b = b.child(
            div().mr(px(GLYPH_MARGIN + LABEL_GAP)).child(
                Icon::new(IconName::Play)
                    .size(px(GLYPH))
                    .color(color::ACCENT),
            ),
        );
    }
    b.child(line(label, text::BTN, line_height(text::BTN, LEADING), color::FG).ellipsis())
        .into_any_element()
}

fn audio_bar(
    id: &SharedString,
    label: &SharedString,
    playing: bool,
    position: Duration,
    duration: Option<Duration>,
    toggle: OnClick,
) -> AnyElement {
    let total = duration.unwrap_or_default();
    let fill = if total.is_zero() {
        0.0
    } else {
        (position.as_secs_f32() / total.as_secs_f32()).min(1.0)
    };
    div()
        .flex()
        .items_center()
        .gap(px(10.0))
        .w(px(BAR_W))
        .h(px(BAR_H))
        .px(px(16.0))
        .rounded(radius::PILL)
        .bg(color::HOVER_HI)
        .child(
            div()
                .id(gpui::ElementId::Name(format!("{id}-toggle").into()))
                .a11y(Role::Button, label.clone())
                .toggled(playing)
                .flex_none()
                .tab_index(0)
                .cursor_pointer()
                .focus_visible(|s| s.shadow(focus_ring()))
                .on_click(move |e, w, cx| toggle(e, w, cx))
                .child(
                    Icon::new(if playing {
                        IconName::Pause
                    } else {
                        IconName::Play
                    })
                    .size(px(16.0))
                    .color(color::FG),
                ),
        )
        .child(line(
            format!("{} / {}", clock(position), clock(total)),
            text::BASE,
            line_height(text::BASE, LEADING),
            color::FG,
        ))
        .child(
            div()
                .flex_1()
                .h(px(4.0))
                .rounded(radius::PILL)
                .bg(color::FG_3)
                .child(
                    div()
                        .h_full()
                        .w(relative(fill))
                        .rounded(radius::PILL)
                        .bg(color::FG),
                ),
        )
        .into_any_element()
}

/// The media card and its height.
pub fn media_card(p: MediaCardProps) -> (AnyElement, f32) {
    let h = media_card_height(p.video, p.state, p.on_save.is_some());
    let w = media_card_width(&p);
    let ui_h = line_height(text::UI, LEADING);
    let player: AnyElement = match (p.video, p.state) {
        (true, PlayerState::Idle | PlayerState::Loading) => div()
            .w(px(VIDEO_W))
            .max_w_full()
            .h(px(VIDEO_W * 9.0 / 16.0))
            .flex()
            .items_center()
            .justify_center()
            .bg(color::STAGE_TILE)
            .border_1()
            .border_color(color::HAIRLINE_2)
            .rounded(radius::XL)
            .child(if p.state == PlayerState::Loading {
                line(p.loading_label.clone(), text::UI, ui_h, color::FG_2).into_any_element()
            } else {
                control_button(
                    format!("{}-play", p.id).into(),
                    p.play_label.clone(),
                    true,
                    p.on_play.clone(),
                )
            })
            .into_any_element(),
        (_, PlayerState::Idle) => control_button(
            format!("{}-play", p.id).into(),
            p.play_label.clone(),
            true,
            p.on_play.clone(),
        ),
        (_, PlayerState::Loading) => {
            line(p.loading_label.clone(), text::UI, ui_h, color::FG_2).into_any_element()
        }
        (
            _,
            PlayerState::Ready {
                playing,
                position,
                duration,
            },
        ) => audio_bar(
            &p.id,
            &p.play_label,
            playing,
            position,
            duration,
            p.on_toggle.clone(),
        ),
        (_, PlayerState::Error) => {
            let save = p.on_save.clone();
            let mut col = div().flex().flex_col().gap(px(GAP)).child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(6.0))
                    .child(Icon::new(IconName::X).size(px(14.0)).color(color::DANGER))
                    .child(line(p.error_label.clone(), text::UI, ui_h, color::DANGER)),
            );
            if let Some(f) = save {
                col = col.child(control_button(
                    format!("{}-error-save", p.id).into(),
                    p.save_text.clone(),
                    false,
                    f,
                ));
            }
            col.into_any_element()
        }
    };
    let mut card = div()
        .flex()
        .flex_col()
        .items_start()
        .gap(px(GAP))
        .w(px(w))
        .max_w_full()
        .h(px(h))
        .px(px(PAD_X))
        .py(px(PAD_Y))
        .bg(color::SURFACE_2)
        .border_1()
        .border_color(color::HAIRLINE_2)
        .rounded(radius::MD)
        .child(
            div()
                .flex()
                .gap(px(8.0))
                .max_w_full()
                .child(line(p.name.clone(), text::SM, meta_h(), color::FG).ellipsis())
                .child(line(p.size.clone(), text::XS, meta_h(), color::FG_3)),
        )
        .child(player);
    if let (Some(f), false) = (p.on_save, p.state == PlayerState::Error) {
        // The save matches the player's buttons, 40px high (flow 06), not Button's 32px md size.
        card = card.child(control_button(
            format!("{}-save", p.id).into(),
            p.save_text,
            false,
            f,
        ));
    }
    (card.into_any_element(), h)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_clock_is_minutes_and_two_digit_seconds() {
        assert_eq!(clock(Duration::from_secs(2)), "0:02");
        assert_eq!(clock(Duration::from_millis(125_900)), "2:05");
    }

    /// T28 parity residue: the card fits its widest line (the play button: edges 2, padding 28, glyph 14 + 8 + 8, then
    /// "play bridge-shanty.wav" at 11.5px mono) inside its 24px padding and edges, as the web's fit-content card.
    #[test]
    fn an_audio_card_fits_its_play_button() {
        let noop: OnClick = std::rc::Rc::new(|_, _, _| {});
        let props = MediaCardProps {
            id: "a".into(),
            video: false,
            name: "bridge-shanty.wav".into(),
            size: "188 KB".into(),
            state: PlayerState::Idle,
            play_label: "play bridge-shanty.wav".into(),
            loading_label: "loading".into(),
            error_label: "error".into(),
            save_text: "save".into(),
            on_play: noop.clone(),
            on_toggle: noop.clone(),
            on_save: Some(noop),
        };
        // 22 characters at 6.9px: 151.8, ceiled onto the 1/64px layout unit.
        let label = (22.0f32 * 11.5 * 0.6 * 64.0 - 1e-3).ceil() / 64.0;
        let button = 2.0 + 28.0 + 14.0 + 8.0 + 8.0 + label;
        assert!((media_card_width(&props) - (button + 24.0 + 2.0)).abs() < 1e-3);
        assert!(media_card_width(&props) < MEDIA_W);
    }

    #[test]
    fn the_card_grows_with_its_player() {
        let ready = PlayerState::Ready {
            playing: false,
            position: Duration::ZERO,
            duration: None,
        };
        // 2 + 16 padding, 18.75 meta, 6 gap, 54 bar, 6 gap, 40 save: the docs shot's 142px card.
        assert_eq!(media_card_height(false, ready, true), 142.75);
        assert!(
            media_card_height(false, PlayerState::Idle, true)
                < media_card_height(false, ready, true)
        );
        assert!(media_card_height(true, PlayerState::Idle, true) > 270.0);
    }
}
