//! The ready video of a `MediaPlayer` (parity plan Package 12). The web drew Chromium's own `<video controls>` in the
//! 480 x 270 frame (`.d-media-player__video`: the stage tile behind a letterboxed picture, `--r-xl` corners); this draws
//! the same picture and Chromium's control overlay as Chromium 153 lays it out at that size (measured from its 2x
//! shot): a bottom shade rising 120px, a 48px button row (play or pause and the time on the left; volume and full screen
//! on the right, 48px apart) and the 4px timeline 16px in from either side, 22px above the bottom. The controls show
//! while paused and for a moment after the pointer moves over the picture, as Chromium's do.
//!
//! The controls are drawn in the Signal style (Lucide glyphs, the mono time; Jonas 2026-11-15) at Chromium's places. The
//! ⋮ menu has Chromium's three items (Download, Playback speed, Picture in picture), drawn as the app's
//! [`ActionMenu`]. The same element fills the window in full screen ([`video_fullscreen`]) and the picture-in-picture
//! window ([`PipMode::Window`]); while the video is in that window its card says so ([`PipMode::Elsewhere`]).

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
use crate::components::action_menu::{ActionMenu, MenuItem};
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
    /// The right-hand buttons' centres from the right edge: volume, full screen, the ⋮ menu.
    pub const VOLUME_FROM_RIGHT: f32 = 120.0;
    pub const FULL_FROM_RIGHT: f32 = 72.0;
    pub const MORE_FROM_RIGHT: f32 = 24.0;
    /// The ⋮ menu's width (Chromium's overflow menu).
    pub const MENU_W: f32 = 220.0;
    /// The menu's bottom right corner from the ⋮'s centre, both ways.
    pub const MENU_CORNER: f32 = 20.0;
    /// The picture-in-picture window's corner buttons: 8px in.
    pub const PIP_INSET: f32 = 8.0;
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

/// Opens (`true`, at the pointer) or closes the ⋮ menu.
pub type OnMenuOpen = Rc<dyn Fn(bool, gpui::Point<Pixels>, &mut Window, &mut App) + 'static>;
/// An item of the ⋮ menu was chosen (its id, [`menu_items`]).
pub type OnMenuPick = Rc<dyn Fn(&str, &mut Window, &mut App) + 'static>;

/// The ⋮ menu: Chromium's overflow menu, its playback-speed page, the labels and the handlers.
#[derive(Clone)]
pub struct MenuView {
    pub open: bool,
    /// Where it opened (the pointer, window coordinates).
    pub at: gpui::Point<Pixels>,
    /// The playback-speed page instead of the main one.
    pub speed_page: bool,
    pub speed: f32,
    pub more_label: SharedString,
    pub download_label: SharedString,
    pub speed_label: SharedString,
    pub normal_label: SharedString,
    pub pip_label: SharedString,
    pub on_open: OnMenuOpen,
    pub on_pick: OnMenuPick,
}

/// The speeds of Chromium's playback-speed page.
pub const SPEEDS: [f32; 8] = [0.25, 0.5, 0.75, 1.0, 1.25, 1.5, 1.75, 2.0];

/// A speed as the page names it: `normal` at 1, else the number without trailing zeros ("0.25", "1.5", "2").
pub fn speed_name(rate: f32, normal: &str) -> String {
    if rate == 1.0 {
        return normal.to_owned();
    }
    let s = format!("{rate:.2}");
    s.trim_end_matches('0').trim_end_matches('.').to_owned()
}

/// The ⋮ menu's items: download, playback speed (its current speed as the hint) and picture in picture; or the speed
/// page, its first row going back, then each speed with the current one ticked. Ids: `download`, `speed`, `pip`,
/// `speed-back`, `speed-<rate>`.
pub fn menu_items(m: &MenuView) -> Vec<MenuItem> {
    if m.speed_page {
        let mut items =
            vec![MenuItem::new("speed-back", m.speed_label.clone()).icon(IconName::ChevronLeft)];
        items.extend(SPEEDS.iter().map(|r| {
            MenuItem::new(format!("speed-{r}"), speed_name(*r, &m.normal_label))
                .checked(*r == m.speed)
        }));
        return items;
    }
    vec![
        MenuItem::new("download", m.download_label.clone()).icon(IconName::Download),
        MenuItem::new("speed", m.speed_label.clone()).hint(speed_name(m.speed, &m.normal_label)),
        MenuItem::new("pip", m.pip_label.clone()),
    ]
}

/// The rate an item id names (`speed-1.5`), if it is one of [`SPEEDS`].
pub fn picked_speed(id: &str) -> Option<f32> {
    let r: f32 = id.strip_prefix("speed-")?.parse().ok()?;
    SPEEDS.contains(&r).then_some(r)
}

/// Where the video is drawn with regard to picture in picture.
#[derive(Clone)]
pub enum PipMode {
    /// In its card (or full screen): the usual controls and the ⋮ menu.
    Inline,
    /// Its card while the video plays in the picture-in-picture window: the stage tile and Chromium's notice.
    Elsewhere { notice: SharedString },
    /// The picture-in-picture window itself: play or pause in the middle, back to the card top left, close top right.
    Window {
        back_label: SharedString,
        close_label: SharedString,
        on_back: OnClick,
        on_close: OnClick,
    },
}

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
    pub menu: MenuView,
    pub pip: PipMode,
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
    if let PipMode::Elsewhere { notice } = &v.pip {
        return div()
            .id(name("video"))
            .w(px(w))
            .max_w_full()
            .h(px(h))
            .flex()
            .items_center()
            .justify_center()
            .px(px(c::TRACK_X))
            .bg(color::STAGE_TILE)
            .rounded(radius::XL)
            .child(line(
                notice.clone(),
                text::UI,
                line_height(text::UI, super::LEADING),
                color::FG_2,
            ))
            .into_any_element();
    }
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
    if let PipMode::Window {
        back_label,
        close_label,
        on_back,
        on_close,
    } = &v.pip
    {
        let (glyph, label) = if v.playing {
            (IconName::Pause, v.pause_label.clone())
        } else {
            (IconName::Play, v.play_label.clone())
        };
        return frame
            .child(div().absolute().size_full().bg(black(0.35)))
            .child(
                button(name("toggle"), glyph, label, v.on_toggle.clone())
                    .left(px((w - c::BUTTON) / 2.0))
                    .top(px((h - c::BUTTON) / 2.0)),
            )
            .child(
                button(
                    name("back"),
                    IconName::ChevronLeft,
                    back_label.clone(),
                    on_back.clone(),
                )
                .left(px(c::PIP_INSET))
                .top(px(c::PIP_INSET)),
            )
            .child(
                button(
                    name("close"),
                    IconName::X,
                    close_label.clone(),
                    on_close.clone(),
                )
                .right(px(c::PIP_INSET))
                .top(px(c::PIP_INSET)),
            )
            .into_any_element();
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
    let m = v.menu.clone();
    let open_menu = m.on_open.clone();
    let was_open = m.open;
    let more: OnClick = Rc::new(move |e, w, cx| open_menu(!was_open, e.position(), w, cx));
    let (on_open, on_pick) = (m.on_open.clone(), m.on_pick.clone());
    let at = m.at;
    let items = menu_items(&m);
    // Chromium's menu ends 20px right of and 20px below the ⋮'s centre (4px in from the video's corner), over the
    // controls (the window clamps it).
    let menu_h: f32 = items
        .iter()
        .map(crate::components::action_menu::item_height)
        .sum::<f32>()
        + 8.0;
    let place = gpui::point(
        at.x + px(c::MENU_CORNER - c::MENU_W),
        at.y + px(c::MENU_CORNER - menu_h),
    );
    let menu = ActionMenu::new(name("more-menu"), m.more_label.clone(), items)
        .no_trigger(None)
        .at(place)
        .width(px(c::MENU_W))
        .open(m.open)
        .on_open_change(move |open, w, cx| on_open(open, at, w, cx))
        .on_select(move |item, w, cx| on_pick(item, w, cx));
    frame
        .child(
            button(name("fullscreen"), glyph, label, v.on_fullscreen.clone())
                .right(px(c::FULL_FROM_RIGHT - c::BUTTON / 2.0))
                .top(px(row_top)),
        )
        .child(
            button(
                name("more"),
                IconName::EllipsisVertical,
                m.more_label.clone(),
                more,
            )
            .right(px(c::MORE_FROM_RIGHT - c::BUTTON / 2.0))
            .top(px(row_top)),
        )
        .child(menu)
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

    fn menu(speed_page: bool, speed: f32) -> MenuView {
        MenuView {
            open: true,
            at: gpui::Point::default(),
            speed_page,
            speed,
            more_label: "more".into(),
            download_label: "Download".into(),
            speed_label: "Playback speed".into(),
            normal_label: "Normal".into(),
            pip_label: "Picture in picture".into(),
            on_open: Rc::new(|_, _, _, _| {}),
            on_pick: Rc::new(|_, _, _| {}),
        }
    }

    /// Chromium's three items, the speed named on the main page; its eight speeds, the current one ticked.
    #[test]
    fn the_more_menu_has_chromium_s_items_and_speeds() {
        let main = menu_items(&menu(false, 1.5));
        let ids: Vec<_> = main.iter().map(|i| i.id.to_string()).collect();
        assert_eq!(ids, ["download", "speed", "pip"]);
        assert_eq!(main[1].hint.as_deref(), Some("1.5"));
        let page = menu_items(&menu(true, 1.0));
        let labels: Vec<_> = page.iter().map(|i| i.label.to_string()).collect();
        assert_eq!(
            labels,
            [
                "Playback speed",
                "0.25",
                "0.5",
                "0.75",
                "Normal",
                "1.25",
                "1.5",
                "1.75",
                "2"
            ]
        );
        assert_eq!(page.iter().filter(|i| i.checked == Some(true)).count(), 1);
        assert_eq!(page[4].checked, Some(true));
        for item in &page[1..] {
            assert!(picked_speed(&item.id).is_some(), "{}", item.id);
        }
        assert_eq!(picked_speed("speed-3"), None);
        assert_eq!(picked_speed("download"), None);
    }

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
