//! `SensitivityMeter` (packages/ui/src/SensitivityMeter, README §5.9 Input sensitivity): 50 segments 28px high, 2px
//! apart, 2px corners, from -100 to 0 dB (unlit `--meter-off`, under the line `--fg-4`, over it `--ok`, over -10 dB
//! `--warn`); a 2px `--fg` line over the 6px padded box with a 3px `--bg` halo and its 9.5px label 16px above it; the scale
//! 6px under it in 9.5px `--fg-faint`. The line is the slider: the whole meter is its track, the arrows move it 1 dB, Page
//! Up and Page Down 10 dB. With automatic sensitivity it is fixed where dilla puts it and cannot be dragged.

use std::cell::Cell;
use std::rc::Rc;

use gpui::{
    App, Bounds, ElementId, FocusHandle, Hsla, InteractiveElement, IntoElement, MouseButton,
    MouseDownEvent, ParentElement, Pixels, RenderOnce, Styled, Window, div, px,
};

use super::slider::{SetFn, pointer_layer, value_at, value_for_key};
use crate::metrics::focus_ring;
use crate::text::{line, line_height};
use crate::tokens::{color, text};

pub const SEGMENTS: usize = 50;
const MIN: i32 = -100;
const MAX: i32 = 0;
/// The line's width: the thumb of the web's transparent range.
const LINE: f32 = 2.0;

/// A threshold or level as the prototype writes it: `-48 dB`.
pub fn format_db(db: f32) -> String {
    format!("{} dB", db.round().clamp(-100.0, 0.0) as i32)
}

/// One segment's state.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Segment {
    Off,
    /// Lit, under the threshold.
    Below,
    On,
    /// Lit, over -10 dB.
    Hot,
}

/// Each segment's state, the prototype's rule: a segment at d dB (-100, -98, ..., -2) is lit below the level; lit, it is
/// `Below` under the threshold, `Hot` over -10 dB, else `On`. Nothing is lit unless the test runs.
pub fn segment_states(level_db: f32, threshold_db: f32, testing: bool) -> [Segment; SEGMENTS] {
    let level = if testing { level_db } else { MIN as f32 };
    std::array::from_fn(|i| {
        let db = (MIN + i as i32 * 2) as f32;
        if db >= level {
            Segment::Off
        } else if db < threshold_db {
            Segment::Below
        } else if db > -10.0 {
            Segment::Hot
        } else {
            Segment::On
        }
    })
}

fn segment_color(s: Segment) -> Hsla {
    match s {
        Segment::Off => color::METER_OFF,
        Segment::Below => color::FG_4,
        Segment::On => color::OK,
        Segment::Hot => color::WARN,
    }
}

type ChangeHandler = Rc<dyn Fn(i32, bool, &mut Window, &mut App) + 'static>;

struct Memory {
    focus: FocusHandle,
    bounds: Rc<Cell<Bounds<Pixels>>>,
    dragging: Rc<Cell<bool>>,
    draft: Rc<Cell<Option<i32>>>,
}

#[derive(IntoElement)]
pub struct SensitivityMeter {
    id: ElementId,
    level_db: f32,
    threshold_db: f32,
    auto: bool,
    testing: bool,
    focus: Option<FocusHandle>,
    on_threshold: Option<ChangeHandler>,
}

impl SensitivityMeter {
    pub fn new(
        id: impl Into<ElementId>,
        level_db: f32,
        threshold_db: f32,
        auto: bool,
        testing: bool,
    ) -> Self {
        Self {
            id: id.into(),
            level_db,
            threshold_db,
            auto,
            testing,
            focus: None,
            on_threshold: None,
        }
    }

    /// Tracks `handle` as the line's focus.
    pub fn focus_handle(mut self, handle: &FocusHandle) -> Self {
        self.focus = Some(handle.clone());
        self
    }

    /// The threshold as it moves (`commit` false) and when it is let go or stepped by a key (`commit` true).
    pub fn on_threshold(
        mut self,
        handler: impl Fn(i32, bool, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.on_threshold = Some(Rc::new(handler));
        self
    }
}

impl RenderOnce for SensitivityMeter {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let memory = window.use_keyed_state((self.id.clone(), "meter"), cx, |_, cx| Memory {
            focus: cx.focus_handle().tab_stop(true),
            bounds: Rc::new(Cell::new(Bounds::default())),
            dragging: Rc::new(Cell::new(false)),
            draft: Rc::new(Cell::new(None)),
        });
        let (own_focus, bounds, dragging, draft) = memory.read_with(cx, |m, _| {
            (
                m.focus.clone(),
                m.bounds.clone(),
                m.dragging.clone(),
                m.draft.clone(),
            )
        });
        let focus = self.focus.clone().unwrap_or(own_focus);
        let stored = self.threshold_db.round().clamp(MIN as f32, MAX as f32) as i32;
        if draft.get() == Some(stored) && !dragging.get() {
            draft.set(None);
        }
        // The line follows the draft while it moves, but only a manual threshold has one.
        let threshold = if self.auto {
            stored
        } else {
            draft.get().unwrap_or(stored)
        };
        let auto = self.auto;
        let on_threshold = self.on_threshold;
        let set: SetFn = {
            let (draft, memory) = (draft.clone(), memory.clone());
            Rc::new(move |value, commit, window, cx| {
                draft.set(Some(value));
                memory.update(cx, |_, cx| cx.notify());
                if let Some(handler) = &on_threshold {
                    handler(value, commit, window, cx);
                }
            })
        };

        let left = gpui::relative((threshold - MIN) as f32 / (MAX - MIN) as f32);
        let mut segments = div().flex().gap(px(2.0)).h(px(28.0));
        for state in segment_states(self.level_db, threshold as f32, self.testing) {
            segments = segments.child(div().flex_1().rounded(px(2.0)).bg(segment_color(state)));
        }
        let line_el = div()
            .absolute()
            .top_0()
            .bottom_0()
            .left(left)
            .ml(px(-LINE / 2.0))
            .w(px(LINE))
            .rounded(px(2.0))
            .bg(color::FG)
            .shadow(vec![gpui::BoxShadow {
                color: color::BG,
                offset: gpui::point(px(0.0), px(0.0)),
                blur_radius: px(0.0),
                spread_radius: px(3.0),
                inset: false,
            }]);
        // The label is centred on the line: a zero-width box at the line's position that lets its text overflow evenly.
        let value = div()
            .absolute()
            .top(px(-16.0))
            .left(left)
            .w(px(0.0))
            .flex()
            .justify_center()
            .child(div().flex_none().child(line(
                format_db(threshold as f32),
                text::CHIP,
                line_height(text::CHIP, 1.5),
                color::FG,
            )));

        let mut meter = div()
            .id((self.id.clone(), "meter"))
            .relative()
            .py(px(6.0))
            .focus_visible(|s| s.shadow(focus_ring()))
            .child(segments)
            .child(line_el)
            .child(value)
            .child(pointer_layer(
                bounds.clone(),
                dragging.clone(),
                set.clone(),
                (MIN, MAX, 1, LINE),
            ));
        meter = if auto {
            // Automatic: the line is fixed and not a tab stop (the web's range is disabled).
            meter.cursor_not_allowed()
        } else {
            let (down_bounds, down_dragging, down_set, down_focus, key_set) = (
                bounds.clone(),
                dragging.clone(),
                set.clone(),
                focus.clone(),
                set.clone(),
            );
            meter
                .track_focus(&focus)
                .cursor_ew_resize()
                .on_mouse_down(
                    MouseButton::Left,
                    move |event: &MouseDownEvent, window, cx| {
                        down_focus.focus(window, cx);
                        down_dragging.set(true);
                        let b = down_bounds.get();
                        let x = f32::from(event.position.x - b.origin.x);
                        down_set(
                            value_at(x, f32::from(b.size.width), LINE, MIN, MAX, 1),
                            false,
                            window,
                            cx,
                        );
                    },
                )
                .on_key_down(move |event, window, cx| {
                    if let Some(next) =
                        value_for_key(event.keystroke.key.as_str(), threshold, MIN, MAX, 1, 10)
                    {
                        cx.stop_propagation();
                        if next != threshold {
                            key_set(next, true, window, cx);
                        }
                    }
                })
        };

        let tick = |label: &'static str| {
            div().child(line(
                label,
                text::CHIP,
                line_height(text::CHIP, 1.5),
                color::FG_FAINT,
            ))
        };
        div()
            .flex()
            .flex_col()
            .gap(px(6.0))
            .min_w_0()
            .child(meter)
            .child(
                div()
                    .flex()
                    .justify_between()
                    .child(tick("-100 dB"))
                    .child(tick("-50"))
                    .child(tick("0 dB")),
            )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nothing_is_lit_unless_the_test_runs() {
        assert!(
            segment_states(-9.0, -50.0, false)
                .iter()
                .all(|s| *s == Segment::Off)
        );
    }

    #[test]
    fn segments_light_below_the_level_in_three_colours() {
        // A level of -9 dB with the line at -50: the segments at -100..-52 dB are below the line, -50..-10 dB are on (-10 is
        // not over -10), and -8 dB is not under the level.
        let s = segment_states(-9.0, -50.0, true);
        assert_eq!(s[0], Segment::Below); // -100 dB
        assert_eq!(s[24], Segment::Below); // -52 dB
        assert_eq!(s[25], Segment::On); // -50 dB
        assert_eq!(s[45], Segment::On); // -10 dB
        assert_eq!(s[46], Segment::Off); // -8 dB: not under -9
        let loud = segment_states(-1.0, -50.0, true);
        assert_eq!(loud[46], Segment::Hot); // -8 dB
        assert_eq!(loud[49], Segment::Hot); // -2 dB is under -1 dB
    }

    #[test]
    fn the_scale_has_fifty_segments() {
        assert_eq!(SEGMENTS, 50);
        assert_eq!(segment_states(0.0, 0.0, true).len(), 50);
    }

    #[test]
    fn a_db_value_reads_as_the_prototype_writes_it() {
        assert_eq!(format_db(-48.0), "-48 dB");
        assert_eq!(format_db(-100.0), "-100 dB");
        assert_eq!(format_db(0.0), "0 dB");
        assert_eq!(format_db(3.0), "0 dB");
        assert_eq!(format_db(-100.4), "-100 dB");
    }
}
