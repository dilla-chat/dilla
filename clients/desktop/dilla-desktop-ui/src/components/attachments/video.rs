//! The ready video of a `MediaPlayer` (parity plan Package 12). The web drew Chromium's own `<video controls>` in the
//! 480 x 270 frame (`.d-media-player__video`: the stage tile behind a letterboxed picture, `--r-xl` corners); this draws
//! the same picture and Chromium's control overlay as Chromium 153 lays it out at that size (measured from its 2x
//! shot): a bottom shade rising 120px, a 48px button row (play or pause and the time on the left; volume and full screen
//! on the right, 48px apart) and the 4px timeline 16px in from either side, 22px above the bottom. The controls show
//! while paused and for a moment after the pointer moves over the picture, as Chromium's do.
//!
//! The same element fills the window in full screen ([`video_fullscreen`]).

use std::cell::Cell;
use std::rc::Rc;
use std::sync::Arc;
use std::time::Duration;

use gpui::{
    AnyElement, App, Bounds, ElementId, Hsla, InteractiveElement, IntoElement, MouseButton,
    MouseDownEvent, MouseMoveEvent, ObjectFit, ParentElement, Pixels, RenderImage, SharedString,
    StatefulInteractiveElement, Styled, StyledImage, Window, canvas, div, hsla, img,
    linear_color_stop, linear_gradient, px, relative,
};

use super::OnClick;
use super::player::clock;
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::focus_ring;
use crate::text::{line, line_height};
use crate::tokens::{color, radius, text};

/// `.d-media-player__video`: at most 30rem wide, 16:9 (`max-height: 16.875rem`).
pub const VIDEO_W: f32 = 480.0;
pub const VIDEO_H: f32 = 270.0;

/// Chromium's control overlay at 480 x 270 (its media controls' layout, measured).
mod c {
    /// The button row's height and its buttons' pitch.
    pub const ROW_H: f32 = 48.0;
    /// The row's centre line above the bottom edge.
    pub const ROW_Y: f32 = 48.0;
    /// The play button's centre from the left edge; the time starts at 48px.
    pub const PLAY_X: f32 = 25.0;
    pub const TIME_X: f32 = 48.0;
    /// The right-hand buttons' centres from the right edge: volume, full screen. Chromium's overflow menu (⋮, at 24px:
    /// download, playback speed, picture in picture) is not drawn; its place stays empty (a deviation for Jonas).
    pub const VOLUME_FROM_RIGHT: f32 = 120.0;
    pub const FULL_FROM_RIGHT: f32 = 72.0;
    pub const BUTTON: f32 = 36.0;
    pub const GLYPH: f32 = 20.0;
    /// The timeline: 4px, 16px in, its centre 22px above the bottom; a 16px tall hit area.
    pub const TRACK_H: f32 = 4.0;
    pub const TRACK_X: f32 = 16.0;
    pub const TRACK_Y: f32 = 22.0;
    pub const TRACK_HIT: f32 = 16.0;
    pub const THUMB: f32 = 12.0;
    /// The volume slider shown left of its button on hover.
    pub const VOLUME_W: f32 = 52.0;
    /// The shade: three linear pieces from clear to black over the bottom 120px (Chromium's eased gradient).
    pub const SHADE: [(f32, f32, f32, f32); 3] = [
        (120.0, 80.0, 0.0, 0.17),
        (80.0, 40.0, 0.17, 0.75),
        (40.0, 0.0, 0.75, 1.0),
    ];
    /// The timeline's unplayed part: white at 68%.
    pub const REST_ALPHA: f32 = 0.68;
}

const WHITE: Hsla = hsla(0.0, 0.0, 1.0, 1.0);

fn white(a: f32) -> Hsla {
    hsla(0.0, 0.0, 1.0, a)
}

fn black(a: f32) -> Hsla {
    hsla(0.0, 0.0, 0.0, a)
}

/// A pointer position along a bar as a fraction (0 to 1) of its width.
pub type OnFraction = Rc<dyn Fn(f32, &mut Window, &mut App) + 'static>;
/// The pointer moved over the picture (the controls show for a moment).
pub type OnMotion = Rc<dyn Fn(&mut Window, &mut App) + 'static>;

/// What a ready video shows and does.
#[derive(Clone)]
pub struct VideoView {
    pub id: SharedString,
    pub image: Arc<RenderImage>,
    /// The picture's own size (it is fitted into the frame, letterboxed).
    pub picture: (u32, u32),
    pub playing: bool,
    pub position: Duration,
    pub duration: Option<Duration>,
    pub volume: f32,
    pub muted: bool,
    /// Whether the file has sound: without it the volume button is not drawn (as Chromium hides it).
    pub sound: bool,
    pub controls: bool,
    pub fullscreen: bool,
    /// The file's name: the frame's accessible name, as the web's `aria-label`.
    pub label: SharedString,
    pub play_label: SharedString,
    pub pause_label: SharedString,
    pub mute_label: SharedString,
    pub unmute_label: SharedString,
    pub fullscreen_label: SharedString,
    pub exit_fullscreen_label: SharedString,
    pub on_toggle: OnClick,
    pub on_mute: OnClick,
    pub on_fullscreen: OnClick,
    pub on_seek: OnFraction,
    pub on_volume: OnFraction,
    pub on_motion: OnMotion,
}

fn fraction(position: Duration, duration: Option<Duration>) -> f32 {
    match duration {
        Some(d) if !d.is_zero() => (position.as_secs_f32() / d.as_secs_f32()).clamp(0.0, 1.0),
        _ => 0.0,
    }
}

fn button(
    id: ElementId,
    glyph: IconName,
    label: SharedString,
    f: OnClick,
) -> gpui::Stateful<gpui::Div> {
    div()
        .id(id)
        .absolute()
        .size(px(c::BUTTON))
        .flex()
        .items_center()
        .justify_center()
        .rounded(radius::PILL)
        .tab_index(0)
        .cursor_pointer()
        .focus_visible(|s| s.shadow(focus_ring()))
        .on_click(move |e, w, cx| f(e, w, cx))
        // Chromium's buttons are named, never tooltipped.
        .aria_label(label)
        .child(Icon::new(glyph).size(px(c::GLYPH)).color(WHITE))
}

/// A bar the pointer sets by pressing or dragging along it: its bounds are kept as it paints.
/// `thumb_group`: the hover group that reveals the thumb (Chromium shows the timeline's only while the pointer is over
/// the video); `None` shows it always.
fn scrub_bar(
    id: ElementId,
    fill: f32,
    width: Pixels,
    thumb_group: Option<SharedString>,
    f: OnFraction,
) -> AnyElement {
    let bounds: Rc<Cell<Option<Bounds<Pixels>>>> = Rc::new(Cell::new(None));
    let at = |b: &Rc<Cell<Option<Bounds<Pixels>>>>, x: Pixels| {
        b.get().map(|b| {
            let w = f32::from(b.size.width).max(1.0);
            (f32::from(x - b.origin.x) / w).clamp(0.0, 1.0)
        })
    };
    let (down_b, move_b, keep) = (bounds.clone(), bounds.clone(), bounds);
    let (down_f, move_f) = (f.clone(), f);
    div()
        .id(id)
        .relative()
        .w(width)
        .h(px(c::TRACK_HIT))
        .flex()
        .items_center()
        .cursor_pointer()
        .on_mouse_down(MouseButton::Left, move |e: &MouseDownEvent, w, cx| {
            if let Some(v) = at(&down_b, e.position.x) {
                cx.stop_propagation();
                down_f(v, w, cx);
            }
        })
        .on_mouse_move(move |e: &MouseMoveEvent, w, cx| {
            if e.pressed_button == Some(MouseButton::Left)
                && let Some(v) = at(&move_b, e.position.x)
            {
                move_f(v, w, cx);
            }
        })
        .child(
            div()
                .relative()
                .w_full()
                .h(px(c::TRACK_H))
                .rounded(radius::PILL)
                .bg(white(c::REST_ALPHA))
                .child(
                    div()
                        .absolute()
                        .left_0()
                        .top_0()
                        .h_full()
                        .w(relative(fill))
                        .rounded(radius::PILL)
                        .bg(WHITE),
                )
                .child(
                    canvas(move |b, _, _| keep.set(Some(b)), |_, _, _, _| {})
                        .absolute()
                        .size_full(),
                ),
        )
        .with_thumb(thumb_group, fill)
        .into_any_element()
}

trait Thumb {
    fn with_thumb(self, group: Option<SharedString>, fill: f32) -> Self;
}

impl Thumb for gpui::Stateful<gpui::Div> {
    fn with_thumb(self, group: Option<SharedString>, fill: f32) -> Self {
        let thumb = div()
            .absolute()
            .top(px((c::TRACK_HIT - c::THUMB) / 2.0))
            .left(relative(fill))
            .ml(px(-c::THUMB / 2.0))
            .size(px(c::THUMB))
            .rounded(radius::PILL)
            .bg(WHITE);
        self.child(match group {
            Some(g) => thumb.opacity(0.0).group_hover(g, |s| s.opacity(1.0)),
            None => thumb,
        })
    }
}

/// The picture and, while shown, Chromium's controls over it, in a `w` x `h` frame.
pub fn video_frame(v: VideoView, w: f32, h: f32) -> AnyElement {
    let id = v.id.clone();
    let name = |s: &str| ElementId::Name(format!("{id}-{s}").into());
    let fill = fraction(v.position, v.duration);
    let motion = v.on_motion.clone();
    let frame_group: SharedString = format!("{id}-frame").into();
    let mut frame = div()
        .id(name("video"))
        .group(frame_group.clone())
        .relative()
        .w(px(w))
        .max_w_full()
        .h(px(h))
        .overflow_hidden()
        .bg(color::STAGE_TILE)
        .when_rounded(!v.fullscreen)
        .aria_label(v.label.clone())
        .on_mouse_move(move |_, w, cx| motion(w, cx))
        .child(
            img(v.image.clone())
                .absolute()
                .top_0()
                .left_0()
                .size_full()
                .object_fit(ObjectFit::Contain)
                // GPUI clips no child to its parent's corners: the picture rounds its own.
                .when_rounded(!v.fullscreen),
        );
    if !v.controls {
        return frame.into_any_element();
    }
    for (from, to, a0, a1) in c::SHADE {
        let mut piece = div()
            .absolute()
            .left_0()
            .w_full()
            .bottom(px(to))
            .h(px(from - to));
        if to == 0.0 && !v.fullscreen {
            piece = piece.rounded_b(radius::XL);
        }
        frame = frame.child(piece.bg(linear_gradient(
            180.0,
            linear_color_stop(black(a0), 0.0),
            linear_color_stop(black(a1), 1.0),
        )));
    }
    let row_top = h - c::ROW_Y - c::BUTTON / 2.0;
    let (glyph, label) = if v.playing {
        (IconName::Pause, v.pause_label.clone())
    } else {
        (IconName::Play, v.play_label.clone())
    };
    frame = frame
        .child(
            button(name("toggle"), glyph, label, v.on_toggle.clone())
                .left(px(c::PLAY_X - c::BUTTON / 2.0))
                .top(px(row_top)),
        )
        .child(
            div()
                .absolute()
                .left(px(c::TIME_X))
                .top(px(h - c::ROW_Y - c::ROW_H / 2.0))
                .h(px(c::ROW_H))
                .flex()
                .items_center()
                .child(line(
                    format!(
                        "{} / {}",
                        clock(v.position),
                        clock(v.duration.unwrap_or_default())
                    ),
                    text::UI,
                    line_height(text::UI, super::LEADING),
                    WHITE,
                )),
        );
    if v.sound {
        let (glyph, label) = if v.muted || v.volume == 0.0 {
            (IconName::VolumeX, v.unmute_label.clone())
        } else {
            (IconName::Volume2, v.mute_label.clone())
        };
        let shown = if v.muted { 0.0 } else { v.volume };
        let group: SharedString = format!("{id}-volume-group").into();
        frame = frame.child(
            div()
                .id(name("volume-group"))
                .group(group.clone())
                .absolute()
                .top(px(row_top))
                .right(px(c::VOLUME_FROM_RIGHT - c::BUTTON / 2.0))
                .h(px(c::BUTTON))
                .flex()
                .flex_row_reverse()
                .items_center()
                .child(
                    button(name("volume"), glyph, label, v.on_mute.clone())
                        .relative()
                        .flex_none(),
                )
                .child(
                    div()
                        .opacity(0.0)
                        .group_hover(group, |s| s.opacity(1.0))
                        .child(scrub_bar(
                            name("volume-bar"),
                            shown,
                            px(c::VOLUME_W),
                            None,
                            v.on_volume.clone(),
                        )),
                ),
        );
    }
    let (glyph, label) = if v.fullscreen {
        (IconName::Minimize, v.exit_fullscreen_label.clone())
    } else {
        (IconName::Maximize, v.fullscreen_label.clone())
    };
    frame
        .child(
            button(name("fullscreen"), glyph, label, v.on_fullscreen.clone())
                .right(px(c::FULL_FROM_RIGHT - c::BUTTON / 2.0))
                .top(px(row_top)),
        )
        .child(
            div()
                .absolute()
                .left(px(c::TRACK_X))
                .right(px(c::TRACK_X))
                .top(px(h - c::TRACK_Y - c::TRACK_HIT / 2.0))
                .child(scrub_bar(
                    name("timeline"),
                    fill,
                    px(w - 2.0 * c::TRACK_X),
                    Some(frame_group),
                    v.on_seek.clone(),
                )),
        )
        .into_any_element()
}

trait Rounded {
    fn when_rounded(self, on: bool) -> Self;
}

impl<T: Styled> Rounded for T {
    fn when_rounded(self, on: bool) -> Self {
        if on { self.rounded(radius::XL) } else { self }
    }
}

/// The video filling a `w` x `h` window: black around the picture, as Chromium's full screen video.
pub fn video_fullscreen(v: VideoView, w: f32, h: f32) -> AnyElement {
    div()
        .size_full()
        .bg(black(1.0))
        .occlude()
        .child(video_frame(v, w, h))
        .into_any_element()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_timeline_fills_by_the_played_fraction() {
        let s = Duration::from_secs;
        assert_eq!(fraction(s(3), Some(s(12))), 0.25);
        assert_eq!(fraction(s(30), Some(s(12))), 1.0);
        assert_eq!(fraction(s(3), None), 0.0);
        assert_eq!(fraction(s(3), Some(Duration::ZERO)), 0.0);
    }

    /// Chromium's shade darkens the bottom 120px from clear to black without a step.
    #[test]
    fn the_shade_pieces_meet() {
        for pair in c::SHADE.windows(2) {
            assert_eq!(pair[0].1, pair[1].0);
            assert_eq!(pair[0].3, pair[1].2);
        }
        assert_eq!(c::SHADE[0].2, 0.0);
        assert_eq!(c::SHADE[2].3, 1.0);
        assert_eq!(c::SHADE[2].1, 0.0);
    }
}
