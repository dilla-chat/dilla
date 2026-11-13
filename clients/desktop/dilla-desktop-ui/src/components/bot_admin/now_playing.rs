//! The music player (`packages/ui/src/NowPlaying`; Signal README 6.6): the card, which holds a 52px art tile, the state
//! line, the title and meta, a 4px progress bar with its times, play/pause, skip, stop, the volume and the "Up next" toggle
//! that opens the queue; and the compact row inside the voice dock (the Bot's 22px square, the title over "state · pos /
//! dur", play/pause and skip, a 3px bar, under a 0.15 `--ok` hairline).
//!
//! The controls are Tab stops and activate with Enter or Space as any button does. When the card itself is focused, Space
//! plays and pauses. Without control a press says why in a toast ([`show_toast`]) and does nothing else. Nothing animates
//! (the references are shot with reduced motion); the playing state shows as its `--ok` colour and the equaliser bars at
//! rest height.

use std::rc::Rc;

use gpui::{
    AnyElement, App, ClickEvent, ElementId, InteractiveElement, IntoElement, ParentElement,
    RenderOnce, SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::components::avatar::{Avatar, AvatarShape, AvatarSize};
use crate::components::toast::show_toast;
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::focus_ring;
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

/// What the player shows of the track playing.
#[derive(Clone, Debug, PartialEq)]
pub struct PlayerTrack {
    pub title: SharedString,
    pub duration_s: f64,
}

/// One row of the queue list (the track playing is first, tinted).
#[derive(Clone, Debug, PartialEq)]
pub struct QueueRow {
    pub title: SharedString,
    pub by: SharedString,
    pub duration_s: Option<f64>,
    pub current: bool,
}

/// The visible words of the player, filled by the caller from its strings (`music.*`).
#[derive(Clone, Debug)]
pub struct NowPlayingLabels {
    /// The state line: "Now playing", "Paused", "Stopped".
    pub playing: SharedString,
    pub paused: SharedString,
    pub stopped: SharedString,
    /// The title while nothing plays: "Nothing playing".
    pub idle: SharedString,
    /// The controls' names (their tooltips and accessible names).
    pub play: SharedString,
    pub pause: SharedString,
    pub skip: SharedString,
    pub stop: SharedString,
    /// The card's queue toggle, already filled: "Up next · 3".
    pub queue: SharedString,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum NowPlayingVariant {
    /// The player in the channel where `/play` ran.
    Card,
    /// The row inside the voice dock.
    Compact,
}

type Action = Rc<dyn Fn(&mut Window, &mut App)>;
type Toggle = Rc<dyn Fn(bool, &mut Window, &mut App)>;

/// `m:ss`, as the prototype prints a position and a duration.
pub fn player_time(seconds: f64) -> String {
    let s = seconds.max(0.0).floor() as u64;
    format!("{}:{:02}", s / 60, s % 60)
}

/// The progress bar's fill, 0 to 1: the position clamped to the track, 0 without a duration.
pub fn progress(position_s: f64, duration_s: f64) -> f32 {
    if duration_s <= 0.0 {
        return 0.0;
    }
    (position_s.clamp(0.0, duration_s) / duration_s) as f32
}

/// The tracks queued after the one playing (the card's "Up next" count).
pub fn up_next(queue: &[QueueRow]) -> usize {
    queue.iter().filter(|q| !q.current).count()
}

#[derive(IntoElement)]
pub struct NowPlaying {
    id: ElementId,
    variant: NowPlayingVariant,
    track: Option<PlayerTrack>,
    position_s: f64,
    playing: bool,
    meta: Option<SharedString>,
    volume: Option<SharedString>,
    bot: Option<SharedString>,
    can_control: bool,
    blocked: SharedString,
    queue: Vec<QueueRow>,
    queue_open: bool,
    labels: NowPlayingLabels,
    on_toggle: Option<Action>,
    on_skip: Option<Action>,
    on_stop: Option<Action>,
    on_queue: Option<Toggle>,
}

impl NowPlaying {
    pub fn new(
        id: impl Into<ElementId>,
        variant: NowPlayingVariant,
        labels: NowPlayingLabels,
    ) -> Self {
        Self {
            id: id.into(),
            variant,
            track: None,
            position_s: 0.0,
            playing: false,
            meta: None,
            volume: None,
            bot: None,
            can_control: false,
            blocked: SharedString::default(),
            queue: Vec::new(),
            queue_open: false,
            labels,
            on_toggle: None,
            on_skip: None,
            on_stop: None,
            on_queue: None,
        }
    }

    /// Absent: nothing plays (the idle card; the compact row is not drawn without a track).
    pub fn track(mut self, track: Option<PlayerTrack>) -> Self {
        self.track = track;
        self
    }

    pub fn position(mut self, seconds: f64) -> Self {
        self.position_s = seconds;
        self
    }

    pub fn playing(mut self, playing: bool) -> Self {
        self.playing = playing;
        self
    }

    /// The card's second line, e.g. "YouTube · requested by Ada · harbour".
    pub fn meta(mut self, meta: impl Into<SharedString>) -> Self {
        self.meta = Some(meta.into());
        self
    }

    /// The card's volume, already filled ("75%").
    pub fn volume(mut self, volume: impl Into<SharedString>) -> Self {
        self.volume = Some(volume.into());
        self
    }

    /// The Bot that plays (the dock row's square avatar).
    pub fn bot(mut self, name: impl Into<SharedString>) -> Self {
        self.bot = Some(name.into());
        self
    }

    /// You may control playback; without it a press says `blocked` in a toast.
    pub fn can_control(mut self, can: bool, blocked: impl Into<SharedString>) -> Self {
        self.can_control = can;
        self.blocked = blocked.into();
        self
    }

    pub fn queue(mut self, queue: Vec<QueueRow>, open: bool) -> Self {
        self.queue = queue;
        self.queue_open = open;
        self
    }

    pub fn on_toggle(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_toggle = Some(Rc::new(f));
        self
    }

    pub fn on_skip(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_skip = Some(Rc::new(f));
        self
    }

    pub fn on_stop(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_stop = Some(Rc::new(f));
        self
    }

    pub fn on_queue_toggle(mut self, f: impl Fn(bool, &mut Window, &mut App) + 'static) -> Self {
        self.on_queue = Some(Rc::new(f));
        self
    }

    /// One control: `run` when you may control playback, else the reason as a toast.
    fn control(
        &self,
        id: ElementId,
        icon: IconName,
        glyph: f32,
        size: (f32, f32),
        run: Option<Action>,
        toggle: bool,
    ) -> AnyElement {
        let disabled = self.track.is_none();
        let (can, blocked) = (self.can_control, self.blocked.clone());
        let compact = self.variant == NowPlayingVariant::Compact;
        let (fg, bg) = match (toggle && !compact, disabled) {
            (true, _) => (color::ACCENT_INK, color::ACCENT),
            _ => (color::FG_STRONG, gpui::transparent_black()),
        };
        let fg = if toggle && compact { color::FG } else { fg };
        let mut button = div()
            .id(id)
            .flex()
            .flex_none()
            .items_center()
            .justify_center()
            .w(px(size.0))
            .h(px(size.1))
            .rounded(if compact { radius::ROW } else { radius::MD })
            .bg(bg)
            .text_color(fg)
            .child(Icon::new(icon).size(px(glyph)).color(fg))
            .tab_index(0)
            .focus_visible(|s| s.shadow(focus_ring()));
        if disabled || !can {
            button = button.opacity(if disabled { 0.4 } else { 1.0 });
        }
        if !toggle || compact {
            button = button.hover(|s| s.bg(color::SURFACE_HI).text_color(color::FG));
        }
        button
            .cursor_pointer()
            .on_click(move |_: &ClickEvent, window, cx| {
                if disabled {
                    return;
                }
                if can {
                    if let Some(run) = &run {
                        run(window, cx);
                    }
                } else if !blocked.is_empty() {
                    show_toast(blocked.clone(), cx);
                }
            })
            .into_any_element()
    }

    fn state_label(&self) -> SharedString {
        match (&self.track, self.playing) {
            (None, _) => self.labels.stopped.clone(),
            (Some(_), true) => self.labels.playing.clone(),
            (Some(_), false) => self.labels.paused.clone(),
        }
    }

    fn bar(&self, thickness: f32) -> AnyElement {
        let duration = self.track.as_ref().map_or(0.0, |t| t.duration_s);
        let fraction = if self.track.is_some() {
            progress(self.position_s, duration)
        } else {
            0.0
        };
        div()
            .w_full()
            .h(px(thickness))
            .rounded(px(thickness))
            .overflow_hidden()
            .bg(color::HOVER_HI)
            .child(div().h_full().w(gpui::relative(fraction)).bg(color::ACCENT))
            .into_any_element()
    }

    fn compact(self) -> AnyElement {
        let Some(track) = self.track.clone() else {
            return div().into_any_element();
        };
        let duration = track.duration_s;
        let position = self.position_s.clamp(0.0, duration.max(0.0));
        let sub = format!(
            "{} · {} / {}",
            self.state_label(),
            player_time(position),
            player_time(duration)
        );
        let toggle_icon = if self.playing {
            IconName::Pause
        } else {
            IconName::Play
        };
        let toggle_name = if self.playing {
            &self.labels.pause
        } else {
            &self.labels.play
        };
        let _ = toggle_name;
        let mut row = div().flex().items_center().gap(px(8.0)).min_w_0();
        if let Some(bot) = &self.bot {
            row = row.child(
                Avatar::new((self.id.clone(), "bot"), bot.clone())
                    .shape(AvatarShape::Bot)
                    .size(AvatarSize::Sug),
            );
        }
        let text_col = div()
            .flex()
            .flex_col()
            .flex_1()
            .min_w_0()
            .child(
                line(
                    track.title.clone(),
                    text::BTN,
                    line_height(text::BTN, 1.3),
                    color::FG,
                )
                .ellipsis(),
            )
            .child(line(
                sub,
                text::MICRO,
                line_height(text::MICRO, 1.3),
                color::FG_3,
            ));
        let toggle = self.control(
            (self.id.clone(), "toggle").into(),
            toggle_icon,
            13.0,
            (26.0, 26.0),
            self.on_toggle.clone(),
            true,
        );
        let skip = self.control(
            (self.id.clone(), "skip").into(),
            IconName::SkipForward,
            13.0,
            (26.0, 26.0),
            self.on_skip.clone(),
            false,
        );
        div()
            .id(self.id.clone())
            .flex()
            .flex_col()
            .gap(px(6.0))
            .pt(px(8.0))
            .border_t_1()
            .border_color(color::OK_DIVIDER)
            .child(row.child(text_col).child(toggle).child(skip))
            .child(self.bar(3.0))
            .into_any_element()
    }

    fn card(self, window: &mut Window, cx: &mut App) -> AnyElement {
        let focus = window.use_keyed_state((self.id.clone(), "focus"), cx, |_, cx| {
            cx.focus_handle().tab_stop(true)
        });
        let handle = focus.read(cx).clone();
        let duration = self.track.as_ref().map_or(0.0, |t| t.duration_s);
        let position = self.position_s.clamp(0.0, duration.max(0.0));
        let live = self.track.is_some() && self.playing;
        let state_colour = if live { color::OK } else { color::FG_3 };
        let title = self
            .track
            .as_ref()
            .map_or_else(|| self.labels.idle.clone(), |t| t.title.clone());
        let art = div()
            .flex()
            .flex_none()
            .items_center()
            .justify_center()
            .size(px(52.0))
            .rounded(radius::SEG)
            .bg(color::HOVER)
            .child(Icon::new(IconName::Music).size(px(18.0)).color(color::FG_4));
        let mut text_col = div()
            .flex()
            .flex_col()
            .flex_1()
            .min_w_0()
            .gap(px(2.0))
            .justify_center()
            .child(
                line(
                    self.state_label().to_uppercase(),
                    text::CHIP,
                    line_height(text::CHIP, 1.4),
                    state_colour,
                )
                .weight(font::WEIGHT_LABEL)
                .tracking(0.12),
            );
        text_col = text_col.child(
            line(title, text::INPUT, line_height(text::INPUT, 1.4), color::FG)
                .weight(font::WEIGHT_DISPLAY)
                .ellipsis(),
        );
        if let Some(meta) = &self.meta {
            text_col = text_col.child(
                line(
                    meta.clone(),
                    text::XS,
                    line_height(text::XS, 1.4),
                    color::FG_2,
                )
                .ellipsis(),
            );
        }
        let head = div()
            .flex()
            .gap(px(12.0))
            .px(px(12.0))
            .pt(px(12.0))
            .pb(px(10.0))
            .child(art)
            .child(text_col);
        let times = div()
            .flex()
            .justify_between()
            .mt(px(4.0))
            .child(line(
                player_time(position),
                text::MICRO,
                line_height(text::MICRO, 1.4),
                color::FG_3,
            ))
            .child(line(
                player_time(duration),
                text::MICRO,
                line_height(text::MICRO, 1.4),
                color::FG_3,
            ));
        let progress_row = div().px(px(12.0)).child(self.bar(4.0)).child(times);

        let toggle_icon = if self.playing {
            IconName::Pause
        } else {
            IconName::Play
        };
        let mut controls = div()
            .flex()
            .items_center()
            .gap(px(4.0))
            .px(px(8.0))
            .pt(px(6.0))
            .pb(px(8.0));
        controls = controls
            .child(self.control(
                (self.id.clone(), "toggle").into(),
                toggle_icon,
                15.0,
                (34.0, 30.0),
                self.on_toggle.clone(),
                true,
            ))
            .child(self.control(
                (self.id.clone(), "skip").into(),
                IconName::SkipForward,
                15.0,
                (32.0, 30.0),
                self.on_skip.clone(),
                false,
            ))
            .child(self.control(
                (self.id.clone(), "stop").into(),
                IconName::Square,
                14.0,
                (32.0, 30.0),
                self.on_stop.clone(),
                false,
            ));
        if let Some(volume) = &self.volume {
            controls = controls.child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(6.0))
                    .ml(px(6.0))
                    .child(
                        Icon::new(IconName::Volume2)
                            .size(px(13.0))
                            .color(color::FG_3),
                    )
                    .child(line(
                        volume.clone(),
                        text::META,
                        line_height(text::META, 1.4),
                        color::FG_3,
                    )),
            );
        }
        let open = self.queue_open;
        let on_queue = self.on_queue.clone();
        let toggle_fg = if open { color::ACCENT } else { color::FG_2 };
        let queue_toggle = div()
            .id((self.id.clone(), "queue-toggle"))
            .flex()
            .items_center()
            .gap(px(6.0))
            .h(px(28.0))
            .px(px(9.0))
            .rounded(radius::MD)
            .bg(if open {
                color::ACCENT_OPEN
            } else {
                gpui::transparent_black()
            })
            .tab_index(0)
            .cursor_pointer()
            .focus_visible(|s| s.shadow(focus_ring()))
            .child(
                Icon::new(IconName::ListMusic)
                    .size(px(13.0))
                    .color(toggle_fg),
            )
            .child(line(
                self.labels.queue.clone(),
                text::XS,
                line_height(text::XS, 1.4),
                toggle_fg,
            ))
            .on_click(move |_: &ClickEvent, window, cx| {
                if let Some(f) = &on_queue {
                    f(!open, window, cx);
                }
            });
        controls = controls.child(div().flex_1()).child(queue_toggle);

        let mut card = div()
            .id(self.id.clone())
            .track_focus(&handle)
            .flex()
            .flex_col()
            .max_w(px(442.0))
            .overflow_hidden()
            .bg(color::SURFACE)
            .border_1()
            .border_color(color::CARD_EDGE)
            .rounded(radius::XL)
            .focus_visible(|s| s.shadow(focus_ring()))
            .child(head)
            .child(progress_row)
            .child(controls);
        // Space on the card itself (not on a control inside it) plays and pauses.
        let (on_toggle, can, blocked) = (
            self.on_toggle.clone(),
            self.can_control,
            self.blocked.clone(),
        );
        let has_track = self.track.is_some();
        let own = handle.clone();
        card = card.on_key_down(move |event, window, cx| {
            if event.keystroke.key != "space" || !own.is_focused(window) || !has_track {
                return;
            }
            if can {
                if let Some(f) = &on_toggle {
                    f(window, cx);
                }
            } else if !blocked.is_empty() {
                show_toast(blocked.clone(), cx);
            }
            cx.stop_propagation();
        });
        if open && !self.queue.is_empty() {
            let rows = self.queue.iter().enumerate().map(|(i, q)| {
                let mut row = div()
                    .flex()
                    .items_center()
                    .gap(px(10.0))
                    .h(px(30.0))
                    .px(px(6.0))
                    .rounded(radius::ROW)
                    .child(div().w(px(16.0)).flex_none().child(line(
                        (i + 1).to_string(),
                        text::MICRO,
                        line_height(text::MICRO, 1.4),
                        color::FG_3,
                    )))
                    .child(
                        div().flex_1().min_w_0().child(
                            line(
                                q.title.clone(),
                                text::UI,
                                line_height(text::UI, 1.4),
                                color::FG,
                            )
                            .ellipsis(),
                        ),
                    )
                    .child(line(
                        q.by.clone(),
                        text::MICRO,
                        line_height(text::MICRO, 1.4),
                        color::FG_3,
                    ))
                    .child(
                        div()
                            .w(px(34.0))
                            .flex_none()
                            .flex()
                            .justify_end()
                            .child(line(
                                q.duration_s.map(player_time).unwrap_or_default(),
                                text::META,
                                line_height(text::META, 1.4),
                                color::FG_3,
                            )),
                    );
                if q.current {
                    row = row.bg(color::ACCENT_HOVER);
                }
                row
            });
            card = card.child(
                div()
                    .id((self.id.clone(), "queue"))
                    .flex()
                    .flex_col()
                    .px(px(6.0))
                    .pt(px(4.0))
                    .pb(px(6.0))
                    .border_t_1()
                    .border_color(color::HAIRLINE)
                    .children(rows),
            );
        }
        card.into_any_element()
    }
}

impl RenderOnce for NowPlaying {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        match self.variant {
            NowPlayingVariant::Compact => self.compact(),
            NowPlayingVariant::Card => self.card(window, cx),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_time_is_m_ss() {
        assert_eq!(player_time(0.0), "0:00");
        assert_eq!(player_time(37.9), "0:37");
        assert_eq!(player_time(214.0), "3:34");
        assert_eq!(player_time(-3.0), "0:00");
        assert_eq!(player_time(3600.0), "60:00");
    }

    #[test]
    fn the_bar_fills_the_position_of_the_track_and_never_past_it() {
        assert_eq!(progress(0.0, 200.0), 0.0);
        assert_eq!(progress(50.0, 200.0), 0.25);
        assert_eq!(progress(500.0, 200.0), 1.0);
        assert_eq!(progress(-5.0, 200.0), 0.0);
        assert_eq!(progress(10.0, 0.0), 0.0);
    }

    #[test]
    fn up_next_counts_the_queue_without_the_track_playing() {
        let row = |current| QueueRow {
            title: "t".into(),
            by: "b".into(),
            duration_s: None,
            current,
        };
        assert_eq!(up_next(&[row(true), row(false), row(false)]), 2);
        assert_eq!(up_next(&[row(true)]), 0);
        assert_eq!(up_next(&[]), 0);
    }
}
