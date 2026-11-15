//! The Voice section's range (packages/ui VoiceSettings `d-voice-settings__range`): a 20px box, a 4px track filled with
//! `--accent` up to the value, a 14px `--knob` thumb with a 3px `--accent-halo` ring. A press or a drag sets the value under
//! the pointer; the arrows step it, Page Up and Page Down move it by a page, Home and End go to the ends.
//!
//! `on_change(value, commit)`: every move previews (`commit` false) and the pointer let go, or a key step, commits (`commit`
//! true), as `useRangeCommit` does on the web. While it moves the range shows its draft; once the stored value caught up
//! with the draft it shows the stored one again (a write that lands late never snaps the thumb back).

use std::cell::Cell;
use std::rc::Rc;

use gpui::{
    App, Bounds, ElementId, FocusHandle, InteractiveElement, IntoElement, MouseButton,
    MouseDownEvent, MouseMoveEvent, MouseUpEvent, ParentElement, Pixels, RenderOnce, Styled,
    Window, canvas, div, point, px,
};

use crate::metrics::focus_ring;
use crate::tokens::{color, radius};

type ChangeHandler = Rc<dyn Fn(i32, bool, &mut Window, &mut App) + 'static>;

/// `.d-voice-settings__range`: 20px tall, 2px of margin either side.
const HEIGHT: f32 = 20.0;
const MARGIN_X: f32 = 2.0;
const TRACK: f32 = 4.0;
const THUMB: f32 = 14.0;
const HALO: f32 = 3.0;

/// The value at pixel `x` of a range that is `width` wide with a thumb `thumb` wide: a native range's thumb centre
/// travels from half its width to `width` less half of it, so the ends are reachable.
pub fn value_at(x: f32, width: f32, thumb: f32, min: i32, max: i32, step: i32) -> i32 {
    let travel = (width - thumb).max(1.0);
    let frac = ((x - thumb / 2.0) / travel).clamp(0.0, 1.0);
    let raw = min as f32 + frac * (max - min) as f32;
    let step = step.max(1);
    let snapped = min + ((raw - min as f32) / step as f32).round() as i32 * step;
    snapped.clamp(min, max)
}

/// The value a key sets, or `None` for a key the range does not take.
pub fn value_for_key(
    key: &str,
    value: i32,
    min: i32,
    max: i32,
    step: i32,
    page: i32,
) -> Option<i32> {
    let next = match key {
        "left" | "down" => value - step,
        "right" | "up" => value + step,
        "pagedown" => value - page,
        "pageup" => value + page,
        "home" => min,
        "end" => max,
        _ => return None,
    };
    Some(next.clamp(min, max))
}

/// The change callback as the layers call it.
pub(super) type SetFn = Rc<dyn Fn(i32, bool, &mut Window, &mut App) + 'static>;

/// An invisible layer over a range that records its bounds each frame and follows the pointer: a move while pressed
/// drags the value (`commit` false), the release commits it. The press itself is the caller's (`value_at`).
pub(super) fn pointer_layer(
    bounds: Rc<Cell<Bounds<Pixels>>>,
    dragging: Rc<Cell<bool>>,
    set: SetFn,
    (min, max, step, thumb): (i32, i32, i32, f32),
) -> impl IntoElement {
    canvas(
        {
            let bounds = bounds.clone();
            move |b, _, _| bounds.set(b)
        },
        move |_, (), window, _cx| {
            let (bounds_move, dragging_move, set_move) =
                (bounds.clone(), dragging.clone(), set.clone());
            window.on_mouse_event(move |event: &MouseMoveEvent, _, window, cx| {
                if dragging_move.get() && event.dragging() {
                    let b = bounds_move.get();
                    let x = f32::from(event.position.x - b.origin.x);
                    set_move(
                        value_at(x, f32::from(b.size.width), thumb, min, max, step),
                        false,
                        window,
                        cx,
                    );
                }
            });
            let (bounds_up, dragging_up, set_up) = (bounds.clone(), dragging.clone(), set.clone());
            window.on_mouse_event(move |event: &MouseUpEvent, _, window, cx| {
                if dragging_up.replace(false) {
                    let b = bounds_up.get();
                    let x = f32::from(event.position.x - b.origin.x);
                    set_up(
                        value_at(x, f32::from(b.size.width), thumb, min, max, step),
                        true,
                        window,
                        cx,
                    );
                }
            });
        },
    )
    .absolute()
    .top_0()
    .left_0()
    .size_full()
}

struct Memory {
    focus: FocusHandle,
    bounds: Rc<Cell<Bounds<Pixels>>>,
    dragging: Rc<Cell<bool>>,
    draft: Rc<Cell<Option<i32>>>,
}

#[derive(IntoElement)]
pub struct Slider {
    id: ElementId,
    value: i32,
    min: i32,
    max: i32,
    step: i32,
    page: i32,
    disabled: bool,
    focus: Option<FocusHandle>,
    on_change: Option<ChangeHandler>,
}

impl Slider {
    pub fn new(id: impl Into<ElementId>, value: i32, min: i32, max: i32) -> Self {
        Self {
            id: id.into(),
            value,
            min,
            max,
            step: 1,
            page: ((max - min) / 10).max(1),
            disabled: false,
            focus: None,
            on_change: None,
        }
    }

    /// Tracks `handle` as this range's focus, for a host that knows where the focus is (the automation `focused` query).
    pub fn focus_handle(mut self, handle: &FocusHandle) -> Self {
        self.focus = Some(handle.clone());
        self
    }

    pub fn step(mut self, step: i32) -> Self {
        self.step = step.max(1);
        self
    }

    pub fn page(mut self, page: i32) -> Self {
        self.page = page.max(1);
        self
    }

    pub fn disabled(mut self, disabled: bool) -> Self {
        self.disabled = disabled;
        self
    }

    pub fn on_change(
        mut self,
        handler: impl Fn(i32, bool, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.on_change = Some(Rc::new(handler));
        self
    }
}

impl RenderOnce for Slider {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let memory = window.use_keyed_state((self.id.clone(), "slider"), cx, |_, cx| Memory {
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
        // Hand back to the stored value once it caught up with the draft.
        if draft.get() == Some(self.value) && !dragging.get() {
            draft.set(None);
        }
        let shown = draft.get().unwrap_or(self.value).clamp(self.min, self.max);
        let frac = (shown - self.min) as f32 / (self.max - self.min).max(1) as f32;
        let (min, max, step, page) = (self.min, self.max, self.step, self.page);
        let disabled = self.disabled;
        let on_change = self.on_change;

        let set: SetFn = {
            let (draft, on_change, memory) = (draft.clone(), on_change.clone(), memory.clone());
            Rc::new(
                move |value: i32, commit: bool, window: &mut Window, cx: &mut App| {
                    draft.set(Some(value));
                    memory.update(cx, |_, cx| cx.notify());
                    if let Some(handler) = &on_change {
                        handler(value, commit, window, cx);
                    }
                },
            )
        };

        let track = div()
            .absolute()
            .left_0()
            .right_0()
            .top(px((HEIGHT - TRACK) / 2.0))
            .h(px(TRACK))
            .rounded(px(TRACK))
            .bg(color::HOVER_HI)
            .child(
                div()
                    .absolute()
                    .left_0()
                    .top_0()
                    .h_full()
                    .w(gpui::relative(frac))
                    .rounded(px(TRACK))
                    .bg(color::ACCENT),
            );
        // `left: calc(frac * (100% - 14px))`: the thumb sits in a box 14px narrower than the range, so a fraction of it
        // is the thumb's travel.
        let thumb_layer = div()
            .absolute()
            .top(px((HEIGHT - THUMB) / 2.0))
            .left_0()
            .right(px(THUMB))
            .h(px(THUMB))
            .child(
                div()
                    .absolute()
                    .top_0()
                    .left(gpui::relative(frac))
                    .size(px(THUMB))
                    .rounded(radius::PILL)
                    .bg(color::KNOB)
                    .shadow(vec![gpui::BoxShadow {
                        color: color::ACCENT_HALO,
                        offset: point(px(0.0), px(0.0)),
                        blur_radius: px(0.0),
                        spread_radius: px(HALO),
                        inset: false,
                    }]),
            );

        let pointer = pointer_layer(
            bounds.clone(),
            dragging.clone(),
            set.clone(),
            (min, max, step, THUMB),
        );

        let key_set = set.clone();
        let (down_bounds, down_dragging, down_set, down_focus) =
            (bounds.clone(), dragging.clone(), set.clone(), focus.clone());
        let mut root = div()
            .id(self.id)
            .track_focus(&focus)
            .tab_stop(!disabled)
            .relative()
            .flex_1()
            .min_w_0()
            .h(px(HEIGHT))
            .mx(px(MARGIN_X))
            .focus_visible(|s| s.shadow(focus_ring()))
            .child(track)
            .child(thumb_layer)
            .child(pointer);
        root = if disabled {
            root.cursor_not_allowed()
        } else {
            root.cursor_pointer()
                .on_mouse_down(
                    MouseButton::Left,
                    move |event: &MouseDownEvent, window, cx| {
                        down_focus.focus(window, cx);
                        down_dragging.set(true);
                        let b = down_bounds.get();
                        let x = f32::from(event.position.x - b.origin.x);
                        down_set(
                            value_at(x, f32::from(b.size.width), THUMB, min, max, step),
                            false,
                            window,
                            cx,
                        );
                    },
                )
                .on_key_down(move |event, window, cx| {
                    let current = shown;
                    if let Some(next) =
                        value_for_key(event.keystroke.key.as_str(), current, min, max, step, page)
                    {
                        cx.stop_propagation();
                        if next != current {
                            key_set(next, true, window, cx);
                        }
                    }
                })
        };
        root
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_ends_are_reachable_and_the_middle_is_the_middle() {
        assert_eq!(value_at(0.0, 200.0, THUMB, 0, 200, 1), 0);
        assert_eq!(value_at(200.0, 200.0, THUMB, 0, 200, 1), 200);
        assert_eq!(value_at(100.0, 200.0, THUMB, 0, 200, 1), 100);
        assert_eq!(value_at(-50.0, 200.0, THUMB, 0, 200, 1), 0);
        assert_eq!(value_at(900.0, 200.0, THUMB, 0, 200, 1), 200);
    }

    #[test]
    fn a_value_snaps_to_its_step() {
        // The release delay: 0..1000 in steps of 20.
        assert_eq!(value_at(100.0, 200.0, THUMB, 0, 1000, 20), 500);
        assert_eq!(value_at(107.0, 200.0, THUMB, 0, 1000, 20) % 20, 0);
    }

    #[test]
    fn keys_step_page_and_jump() {
        // sensitivity.spec.ts: End (200 %), then fifty steps down: 150.
        let mut v = value_for_key("end", 100, 0, 200, 1, 20).unwrap();
        assert_eq!(v, 200);
        for _ in 0..50 {
            v = value_for_key("left", v, 0, 200, 1, 20).unwrap();
        }
        assert_eq!(v, 150);
        assert_eq!(value_for_key("home", 150, 0, 200, 1, 20), Some(0));
        assert_eq!(value_for_key("pageup", 190, 0, 200, 1, 20), Some(200));
        assert_eq!(value_for_key("pagedown", 10, 0, 200, 1, 20), Some(0));
        assert_eq!(value_for_key("a", 10, 0, 200, 1, 20), None);
    }
}
