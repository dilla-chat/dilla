//! The app's motion (packages/ui/src/styles/base.css:170-177): the two keyframes the now-playing player uses, `d-bar` (the
//! equaliser bounce) and `d-up` (an entrance that rises 8px and fades in), driven by GPUI's animation API.
//!
//! The durations are the generated `--duration-*` tokens ([`crate::tokens::motion`]). Reduced motion is GPUI's own
//! `App::reduce_motion`, which the Appearance setting and the parity fixtures set: the web zeroes the duration tokens
//! under it, so here the entrance is not drawn at all and the equaliser stands at its full height, which is what a
//! zero-duration CSS animation leaves behind. What is drawn is decided by the pure functions below ([`bar_scale`],
//! [`rise_at`]); the element helpers only hand them GPUI's clock.
//!
//! An animation asks for a frame only while it is drawn and unfinished. An entrance ends after one run (its element state
//! lives as long as the element is drawn at the same place, so a re-render does not replay it, a remount does), and the
//! equaliser exists only while the caller draws it, which is while the music plays: nothing keeps the window rendering
//! once the player is paused, gone or the window hidden (GPUI stops frames for a window the platform hides).

use std::time::Duration;

use gpui::{
    Animation, AnimationExt, AnyElement, App, BoxShadow, ElementId, Hsla, IntoElement,
    ParentElement, Pixels, Styled, div, linear_color_stop, linear_gradient, point, px, relative,
};

use crate::icon::Icon;
use crate::tokens::{color, motion};

/// CSS's default timing function, `ease`: `cubic-bezier(0.25, 0.1, 0.25, 1)`, what an animation without a
/// `animation-timing-function` uses between its keyframes.
pub const EASE: [f32; 4] = [0.25, 0.1, 0.25, 1.0];

/// `d-bar`'s rest scale (`scaleY(0.2)`, at 0% and 100%); it peaks at 1 at 50%.
const BAR_LOW: f32 = 0.2;

/// `d-up`'s start offset (`translateY(8px)`).
const RISE_PX: f32 = 8.0;

/// The value of the CSS curve `cubic-bezier(x1, y1, x2, y2)` at time `x` (0 to 1): the curve's parameter is found for `x`
/// by bisection, and its `y` there is the answer.
pub fn cubic_bezier([x1, y1, x2, y2]: [f32; 4], x: f32) -> f32 {
    let x = x.clamp(0.0, 1.0);
    let at = |a: f32, b: f32, s: f32| {
        3.0 * a * s * (1.0 - s) * (1.0 - s) + 3.0 * b * s * s * (1.0 - s) + s * s * s
    };
    let (mut lo, mut hi) = (0.0_f32, 1.0_f32);
    for _ in 0..32 {
        let mid = (lo + hi) / 2.0;
        if at(x1, x2, mid) < x {
            lo = mid;
        } else {
            hi = mid;
        }
    }
    at(y1, y2, (lo + hi) / 2.0)
}

/// The scale of equaliser bar `bar` (0, 1 or 2) `elapsed` after the bars appeared, for a beat of `beat`
/// (NowPlaying.css:12-14): `d-bar` rises from 0.2 to 1 over the first half of the beat and falls back over the second,
/// each half on the `ease` curve; the second bar starts beat/4 late and the third beat/2. A bar's wait before its first
/// beat is drawn as the phase it would have, not at rest height as CSS draws it (a quarter beat at most, once).
pub fn bar_scale(elapsed: Duration, beat: Duration, bar: usize) -> f32 {
    if beat.is_zero() {
        return 1.0;
    }
    let delay = bar as f32 / 4.0;
    bar_phase_scale((elapsed.as_secs_f32() / beat.as_secs_f32() - delay).rem_euclid(1.0))
}

/// The scale of a bar at `phase` (0 to 1) of its beat.
fn bar_phase_scale(phase: f32) -> f32 {
    let (from, to, t) = if phase < 0.5 {
        (BAR_LOW, 1.0, phase * 2.0)
    } else {
        (1.0, BAR_LOW, (phase - 0.5) * 2.0)
    };
    from + (to - from) * cubic_bezier(EASE, t)
}

/// `d-up` `elapsed` into its run of `run` on `curve`: the opacity and the downward offset in px (8px at the start, none at
/// the end). A run of zero is over at once.
pub fn rise_at(elapsed: Duration, run: Duration, curve: [f32; 4]) -> (f32, f32) {
    if run.is_zero() {
        return (1.0, 0.0);
    }
    let p = cubic_bezier(curve, elapsed.as_secs_f32() / run.as_secs_f32());
    (p, RISE_PX * (1.0 - p))
}

/// `d-sweep` (`from { translateX(-100%) } to { translateX(100%) }`, linear, infinite): the strip's offset as a fraction of
/// its own width `elapsed` into the run of `run`, from -1 to 1. A run of zero stands still.
pub fn sweep_at(elapsed: Duration, run: Duration) -> f32 {
    if run.is_zero() {
        return 0.0;
    }
    -1.0 + 2.0 * (elapsed.as_secs_f32() / run.as_secs_f32()).rem_euclid(1.0)
}

/// `d-caret` (`0%, 49% { opacity: 1 } 50%, 100% { opacity: 0 }`, on `ease` between its keyframes, infinite): the opacity
/// `elapsed` into a period of `period`. It is on until 49% of the period, falls over the next 1% and is off for the rest.
/// A period of zero is drawn on.
pub fn caret_at(elapsed: Duration, period: Duration) -> f32 {
    if period.is_zero() {
        return 1.0;
    }
    let phase = (elapsed.as_secs_f32() / period.as_secs_f32()).rem_euclid(1.0);
    if phase < 0.49 {
        1.0
    } else if phase < 0.5 {
        1.0 - cubic_bezier(EASE, (phase - 0.49) / 0.01)
    } else {
        0.0
    }
}

/// `d-ring` (`from { opacity: 0.6; scale(1) } to { opacity: 0; scale(1.5) }`, infinite): the wave's opacity and scale
/// `elapsed` into a run of `run` on `curve` (RingBanner.css names `--ease-out`). Each run starts again from the first
/// keyframe. A run of zero is no animation: the full ring, unscaled.
pub fn ring_at(elapsed: Duration, run: Duration, curve: [f32; 4]) -> (f32, f32) {
    if run.is_zero() {
        return (1.0, 1.0);
    }
    let p = cubic_bezier(
        curve,
        (elapsed.as_secs_f32() / run.as_secs_f32()).rem_euclid(1.0),
    );
    (0.6 * (1.0 - p), 1.0 + 0.5 * p)
}

/// `d-spin` (`to { transform: rotate(360deg) }`, linear, infinite): the turn in radians `elapsed` into a run of `run`.
/// A run of zero stands upright.
pub fn spin_at(elapsed: Duration, run: Duration) -> f32 {
    if run.is_zero() {
        return 0.0;
    }
    std::f32::consts::TAU * (elapsed.as_secs_f32() / run.as_secs_f32()).rem_euclid(1.0)
}

/// The CSS keyword `ease-out`: `cubic-bezier(0, 0, 0.58, 1)` (not the `--ease-out` token).
pub const CSS_EASE_OUT: [f32; 4] = [0.0, 0.0, 0.58, 1.0];

/// `d-row-flash` (`0%, 30% { background: var(--accent-flash) } 100% { background: transparent }`, on the keyword
/// `ease-out`, once): how much of the flash tint is left `elapsed` into a run of `run`, 1 to 0. It holds for 30% of the
/// run, then fades. A run of zero has no flash.
pub fn flash_at(elapsed: Duration, run: Duration) -> f32 {
    if run.is_zero() {
        return 0.0;
    }
    let t = elapsed.as_secs_f32() / run.as_secs_f32();
    if t <= 0.3 {
        1.0
    } else {
        1.0 - cubic_bezier(CSS_EASE_OUT, (t - 0.3) / 0.7)
    }
}

/// How often the endless indicators and the flash repaint while they are drawn: a sweep, a spinner or a wave reads as
/// smooth at 30 frames a second, like the equaliser.
pub const INDICATOR_FPS: f32 = 30.0;

/// How often the brand caret asks for a frame. It changes twice a period, so a coarse cap is exact enough: the blink
/// lands within a tenth of a second of the CSS one, at a third of the equaliser's repaints.
pub const CARET_FPS: f32 = 10.0;

/// How often the equaliser repaints while it bounces: the 520ms beat reads as smooth at 30 frames a second, half the
/// cost of a repaint on every display frame.
pub const EQUALISER_FPS: f32 = 30.0;

/// The three `--ok` equaliser bars (`.d-np__bars`), `height` px tall and 2px wide, 1.5px apart, bouncing from their bottom
/// on `--duration-beat`. One animation drives the three, so one timer repaints them together, at [`EQUALISER_FPS`]. Draw
/// them only while the music plays: a drawn equaliser keeps asking for frames, an undrawn one costs nothing. With reduced
/// motion they stand at full height.
pub fn equaliser(id: impl Into<ElementId>, height: f32, cx: &App) -> AnyElement {
    let bars = move |scale: [f32; 3]| {
        div()
            .flex()
            .flex_none()
            .items_end()
            .gap(px(1.5))
            .h(px(height))
            .children(scale.map(|s| div().w(px(2.0)).h(px(height * s)).bg(color::OK)))
    };
    if cx.reduce_motion() {
        return bars([1.0; 3]).into_any_element();
    }
    let beat = motion::BEAT;
    bars([1.0; 3])
        .with_animation(
            id,
            Animation::new(beat).repeat().with_max_fps(EQUALISER_FPS),
            move |_, delta| bars([0, 1, 2].map(|i| bar_scale(beat.mul_f32(delta), beat, i))),
        )
        .into_any_element()
}

/// `el` entering with `d-up` over `run` on `curve` (`EASE` unless the CSS names `--ease-out`), once for as long as it stays
/// drawn at `id`'s place: the element fades in while it rises into position (its layout does not move). With reduced motion
/// it is drawn as it is.
pub fn rise_in<E>(
    el: E,
    id: impl Into<ElementId>,
    run: Duration,
    curve: [f32; 4],
    cx: &App,
) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    if cx.reduce_motion() {
        return el.into_any_element();
    }
    el.with_animation(id, Animation::new(run), move |el, delta| {
        let (opacity, offset) = rise_at(run.mul_f32(delta), run, curve);
        // The finished entrance is the element itself, with nothing laid over it.
        if delta >= 1.0 {
            el
        } else {
            el.relative().top(px(offset)).opacity(opacity)
        }
    })
    .into_any_element()
}

/// The composer's typing sweep (`.d-composer__sweep`, Composer.css:13-15): a 2px strip on the box's top edge, over which a
/// gradient from clear through `--accent` to clear slides from one side to the other on `--duration-flash`, linear. Draw
/// it only while someone types: a drawn sweep keeps asking for frames, at [`INDICATOR_FPS`]; with reduced motion the
/// gradient stands still, as a zero-duration CSS animation leaves it.
pub fn sweep(id: impl Into<ElementId>, cx: &App) -> AnyElement {
    let clear: Hsla = color::ACCENT.opacity(0.0);
    let band = move |offset: f32| {
        div()
            .absolute()
            .top(px(0.0))
            .left(relative(offset))
            .w_full()
            .h_full()
            .flex()
            .child(div().w(relative(0.5)).h_full().bg(linear_gradient(
                90.0,
                linear_color_stop(clear, 0.0),
                linear_color_stop(color::ACCENT, 1.0),
            )))
            .child(div().w(relative(0.5)).h_full().bg(linear_gradient(
                90.0,
                linear_color_stop(color::ACCENT, 0.0),
                linear_color_stop(clear, 1.0),
            )))
    };
    let strip = move |offset: f32| {
        div()
            .relative()
            .w_full()
            .h(px(2.0))
            .overflow_hidden()
            .child(band(offset))
    };
    if cx.reduce_motion() {
        return strip(0.0).into_any_element();
    }
    let run = motion::FLASH;
    strip(-1.0)
        .with_animation(
            id,
            Animation::new(run).repeat().with_max_fps(INDICATOR_FPS),
            move |_, delta| strip(sweep_at(run.mul_f32(delta), run)),
        )
        .into_any_element()
}

/// `el` blinking as the brand caret does (`d-caret` on `--duration-caret`, infinite). Always drawn where the caller draws
/// it, so it asks for frames, at [`CARET_FPS`], for as long as its window is visible. With reduced motion it stays on.
pub fn blink<E>(el: E, id: impl Into<ElementId>, cx: &App) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    if cx.reduce_motion() {
        return el.into_any_element();
    }
    let period = motion::CARET;
    el.with_animation(
        id,
        Animation::new(period).repeat().with_max_fps(CARET_FPS),
        move |el, delta| el.opacity(caret_at(period.mul_f32(delta), period)),
    )
    .into_any_element()
}

/// An `icon` turning as `d-spin` does (`--duration-result-spin`, linear, infinite). Draw it only while the work is pending:
/// a drawn spinner asks for frames at [`INDICATOR_FPS`]. With reduced motion it is upright.
pub fn spinning(icon: Icon, id: impl Into<ElementId>, cx: &App) -> AnyElement {
    if cx.reduce_motion() {
        return icon.into_any_element();
    }
    let run = motion::RESULT_SPIN;
    let turned = icon.clone();
    icon.with_animation(
        id,
        Animation::new(run).repeat().with_max_fps(INDICATOR_FPS),
        move |_, delta| turned.clone().rotate(spin_at(run.mul_f32(delta), run)),
    )
    .into_any_element()
}

/// The incoming-ring wave (`.d-ring-banner__glyph::after`, RingBanner.css:8-10): a `--ok` ring 2px wide round a box of
/// `size` px with corners of `radius`, drawn over its positioned parent's whole box, that grows to 1.5 times its size while
/// it fades from 0.6 to nothing, on `--duration-pulse` and `--ease-out`, over and over. Draw it only while the call is
/// incoming. With reduced motion there is no wave: the ring stands as a plain 2px `--ok` edge.
pub fn ring_wave(id: impl Into<ElementId>, size: f32, radius: Pixels, cx: &App) -> AnyElement {
    const SPREAD: f32 = 2.0;
    let ring = move |opacity: f32, scale: f32| {
        let grown = size * scale;
        let inset = -(grown - size) / 2.0;
        div()
            .absolute()
            .top(px(inset))
            .left(px(inset))
            .size(px(grown))
            .rounded(radius * scale)
            .shadow(vec![BoxShadow {
                color: color::OK.opacity(opacity),
                offset: point(px(0.0), px(0.0)),
                blur_radius: px(0.0),
                spread_radius: px(SPREAD * scale),
                inset: false,
            }])
    };
    if cx.reduce_motion() {
        return ring(1.0, 1.0).into_any_element();
    }
    let run = motion::PULSE;
    ring(0.6, 1.0)
        .with_animation(
            id,
            Animation::new(run).repeat().with_max_fps(INDICATOR_FPS),
            move |_, delta| {
                let (opacity, scale) = ring_at(run.mul_f32(delta), run, motion::EASE_OUT);
                ring(opacity, scale)
            },
        )
        .into_any_element()
}

/// `el` flashing the accent tint as a jump-to row does (`d-row-flash` on `--duration-flash`, once): the row's background is
/// `--accent-flash` for 30% of the run, then fades to clear, and the row is as it was after. It runs once for as long as
/// the element stays drawn at `id`'s place, so give a new jump a new id. With reduced motion (MessageRow.css's own
/// `prefers-reduced-motion` form) the row keeps the tint and gains an inset 2px `--accent` edge on its left, added to the
/// shadows it has; the caller stops calling this when the flash is over, which is when [`motion::FLASH`] has passed.
pub fn flash<E>(mut el: E, id: impl Into<ElementId>, cx: &App) -> AnyElement
where
    E: Styled + IntoElement + 'static,
{
    if cx.reduce_motion() {
        let mut shadows = el.style().box_shadow.clone().unwrap_or_default();
        shadows.push(BoxShadow {
            color: color::ACCENT,
            offset: point(px(2.0), px(0.0)),
            blur_radius: px(0.0),
            spread_radius: px(0.0),
            inset: true,
        });
        return el
            .bg(color::ACCENT_FLASH)
            .shadow(shadows)
            .into_any_element();
    }
    let run = motion::FLASH;
    el.with_animation(
        id,
        Animation::new(run).with_max_fps(INDICATOR_FPS),
        move |el, delta| {
            if delta >= 1.0 {
                return el;
            }
            el.bg(color::ACCENT_FLASH.opacity(flash_at(run.mul_f32(delta), run)))
        },
    )
    .into_any_element()
}

#[cfg(test)]
mod tests {
    use super::*;

    const BEAT: Duration = motion::BEAT;

    fn near(a: f32, b: f32) -> bool {
        (a - b).abs() < 0.01
    }

    /// The curves are the CSS ones: `ease` at its midpoint is 0.8024, `--ease-out` runs ahead of linear, both end where
    /// they start and finish.
    #[test]
    fn the_curves_match_css() {
        assert!(near(cubic_bezier(EASE, 0.0), 0.0) && near(cubic_bezier(EASE, 1.0), 1.0));
        assert!(near(cubic_bezier(EASE, 0.5), 0.8024));
        assert!(cubic_bezier(motion::EASE_OUT, 0.25) > 0.5);
        assert!(near(cubic_bezier(motion::EASE_OUT, 1.0), 1.0));
    }

    /// Each bar at 0, beat/4, beat/2 and 3beat/4: the first from its low point up to full and back, the others the same
    /// shifted by their delays (beat/4 and beat/2).
    #[test]
    fn the_bars_bounce_between_a_fifth_and_full_height_a_quarter_beat_apart() {
        let at = |bar, quarters: u32| bar_scale(BEAT * quarters / 4, BEAT, bar);
        // Bar 0: low, rising (each half is eased, so a quarter beat is 0.8024 of the way up), peak, then falling the same way.
        assert!(near(at(0, 0), 0.2));
        assert!(near(at(0, 1), 0.2 + 0.8 * cubic_bezier(EASE, 0.5)));
        assert!(near(at(0, 2), 1.0));
        assert!(near(at(0, 3), 1.0 - 0.8 * cubic_bezier(EASE, 0.5)));
        // Bar 1 is bar 0 a quarter beat late.
        for q in 0..4 {
            assert!(near(at(1, q + 1), at(0, q)));
            assert!(near(at(2, q + 2), at(0, q)));
        }
        assert!(near(at(1, 3), 1.0) && near(at(2, 0), 1.0));
    }

    /// `d-up` at its start, half way (on `ease`) and its end.
    #[test]
    fn the_rise_goes_from_eight_px_down_and_clear_to_in_place_and_opaque() {
        let run = motion::PLAYER;
        let (o, y) = rise_at(Duration::ZERO, run, EASE);
        assert!(near(o, 0.0) && near(y, 8.0));
        let (o, y) = rise_at(run / 2, run, EASE);
        assert!(near(o, 0.8024) && near(y, 8.0 * (1.0 - 0.8024)));
        let (o, y) = rise_at(run, run, EASE);
        assert!(near(o, 1.0) && near(y, 0.0));
    }

    /// Reduced motion zeroes the durations: the bars are full and nothing rises.
    #[test]
    fn reduced_motion_has_full_bars_and_no_entrance() {
        for bar in 0..3 {
            assert_eq!(
                bar_scale(Duration::from_millis(100), Duration::ZERO, bar),
                1.0
            );
        }
        assert_eq!(rise_at(Duration::ZERO, Duration::ZERO, EASE), (1.0, 0.0));
    }

    /// A view that counts its renders and draws `build`'s element.
    struct Host {
        renders: usize,
        build: Box<dyn Fn(&App) -> AnyElement>,
    }
    impl gpui::Render for Host {
        fn render(
            &mut self,
            _: &mut gpui::Window,
            cx: &mut gpui::Context<Self>,
        ) -> impl IntoElement {
            self.renders += 1;
            (self.build)(cx)
        }
    }

    /// Renders of a host drawing `build` over `ms` of test clock, in 10ms steps (animation frames are delivered by hand,
    /// as a test has no platform frame loop).
    fn renders_over(
        cx: &mut gpui::TestAppContext,
        reduced: bool,
        ms: u64,
        build: impl Fn(&App) -> AnyElement + 'static,
    ) -> usize {
        cx.update(|cx| cx.set_reduce_motion(reduced));
        let (host, vcx) = cx.add_window_view(|_, _| Host {
            renders: 0,
            build: Box::new(build),
        });
        vcx.run_until_parked();
        let before = host.read_with(vcx, |h, _| h.renders);
        for _ in 0..ms / 10 {
            vcx.executor().advance_clock(Duration::from_millis(10));
            vcx.run_until_parked();
            vcx.update(|window, cx| window.simulate_next_frame(cx));
            vcx.run_until_parked();
        }
        host.read_with(vcx, |h, _| h.renders) - before
    }

    /// A drawn equaliser repaints, at the cap and not at every frame (a 10ms step is a 100 Hz display: a second of it asks
    /// for about 30 repaints, not 100); reduced motion, or no equaliser drawn (a paused player draws none), repaints nothing.
    #[gpui::test]
    fn the_equaliser_repaints_at_its_cap_only_while_drawn(cx: &mut gpui::TestAppContext) {
        let drawn = |cx: &App| equaliser("eq", 11.0, cx);
        let playing = renders_over(cx, false, 1000, drawn);
        assert!(
            (20..=40).contains(&playing),
            "{playing} repaints in a second"
        );
        assert_eq!(renders_over(cx, true, 1000, drawn), 0, "reduced motion");
        assert_eq!(
            renders_over(cx, false, 1000, |_| div().into_any_element()),
            0,
            "nothing drawn"
        );
    }

    /// An entrance runs once: it repaints while it rises, and not at all after it has finished, however long the element
    /// stays and however often its parent renders; reduced motion draws no entrance.
    #[gpui::test]
    fn an_entrance_runs_once_per_mount(cx: &mut gpui::TestAppContext) {
        let rising = |cx: &App| rise_in(div(), "up", motion::PLAYER, EASE, cx);
        let during = renders_over(cx, false, 300, rising);
        assert!(during > 0, "the entrance repaints while it runs");
        // The element has been up for longer than its run; frames after that are none.
        let (host, vcx) = cx.add_window_view(|_, _| Host {
            renders: 0,
            build: Box::new(rising),
        });
        vcx.run_until_parked();
        for _ in 0..60 {
            vcx.executor().advance_clock(Duration::from_millis(10));
            vcx.update(|window, cx| window.simulate_next_frame(cx));
            vcx.run_until_parked();
        }
        let settled = host.read_with(vcx, |h, _| h.renders);
        host.update(vcx, |_, cx| cx.notify());
        vcx.run_until_parked();
        for _ in 0..10 {
            vcx.executor().advance_clock(Duration::from_millis(10));
            vcx.update(|window, cx| window.simulate_next_frame(cx));
            vcx.run_until_parked();
        }
        assert_eq!(
            host.read_with(vcx, |h, _| h.renders),
            settled + 1,
            "a re-render after the entrance asks for no more frames"
        );
        assert_eq!(renders_over(cx, true, 300, rising), 0, "reduced motion");
    }

    /// `d-sweep` at its start, middle and end, and over again: -100% to 100%, linear.
    #[test]
    fn the_sweep_runs_from_one_edge_to_the_other_and_starts_again() {
        let run = motion::FLASH;
        assert!(near(sweep_at(Duration::ZERO, run), -1.0));
        assert!(near(sweep_at(run / 4, run), -0.5));
        assert!(near(sweep_at(run / 2, run), 0.0));
        assert!(near(sweep_at(run * 3 / 4, run), 0.5));
        assert!(near(sweep_at(run + run / 4, run), -0.5), "the next run");
        assert_eq!(
            sweep_at(run / 3, Duration::ZERO),
            0.0,
            "reduced: stands still"
        );
    }

    /// `d-caret`: on until 49%, off from 50% (falling between), then again.
    #[test]
    fn the_caret_is_on_for_half_a_period_and_off_for_the_other() {
        let p = motion::CARET;
        assert!(near(caret_at(Duration::ZERO, p), 1.0));
        assert!(near(caret_at(p * 3 / 10, p), 1.0));
        assert!(near(caret_at(p * 49 / 100, p), 1.0));
        let falling = caret_at(p * 495 / 1000, p);
        assert!(
            falling > 0.0 && falling < 1.0,
            "{falling} between 49% and 50%"
        );
        assert!(near(caret_at(p / 2, p), 0.0));
        assert!(near(caret_at(p * 9 / 10, p), 0.0));
        assert!(near(caret_at(p + p / 10, p), 1.0), "the next period");
        assert_eq!(caret_at(p / 2, Duration::ZERO), 1.0, "reduced: on");
    }

    /// `d-ring`: 0.6 and unscaled at the start, on `--ease-out` to nothing at 1.5 times, and again.
    #[test]
    fn the_ring_wave_grows_to_one_and_a_half_while_it_fades_from_six_tenths() {
        let run = motion::PULSE;
        let (o, s) = ring_at(Duration::ZERO, run, motion::EASE_OUT);
        assert!(near(o, 0.6) && near(s, 1.0));
        let p = cubic_bezier(motion::EASE_OUT, 0.5);
        let (o, s) = ring_at(run / 2, run, motion::EASE_OUT);
        assert!(near(o, 0.6 * (1.0 - p)) && near(s, 1.0 + 0.5 * p));
        assert!(p > 0.5, "--ease-out runs ahead of linear");
        let (o, s) = ring_at(run - Duration::from_millis(1), run, motion::EASE_OUT);
        assert!(o < 0.01 && near(s, 1.5));
        let (o, s) = ring_at(run, run, motion::EASE_OUT);
        assert!(near(o, 0.6) && near(s, 1.0), "the next run starts over");
        assert_eq!(
            ring_at(run / 3, Duration::ZERO, motion::EASE_OUT),
            (1.0, 1.0)
        );
    }

    /// `d-spin`: linear, a full turn per run.
    #[test]
    fn the_spinner_turns_once_a_run() {
        let run = motion::RESULT_SPIN;
        let pi = std::f32::consts::PI;
        assert!(near(spin_at(Duration::ZERO, run), 0.0));
        assert!(near(spin_at(run / 2, run), pi));
        assert!(near(spin_at(run * 3 / 4, run), 1.5 * pi));
        assert!(near(spin_at(run + run / 2, run), pi), "the next run");
        assert_eq!(spin_at(run / 3, Duration::ZERO), 0.0, "reduced: upright");
    }

    /// `d-row-flash`: the full tint to 30% of the run, then `ease-out` to nothing.
    #[test]
    fn the_row_flash_holds_for_thirty_percent_then_fades_out() {
        let run = motion::FLASH;
        assert!(near(flash_at(Duration::ZERO, run), 1.0));
        assert!(near(flash_at(run * 3 / 10, run), 1.0));
        let mid = flash_at(run * 65 / 100, run);
        assert!(near(mid, 1.0 - cubic_bezier(CSS_EASE_OUT, 0.5)));
        assert!(mid > 0.0 && mid < 1.0);
        assert!(near(flash_at(run, run), 0.0));
        assert_eq!(flash_at(run / 2, Duration::ZERO), 0.0, "reduced: no flash");
    }

    /// The sweep repaints at its cap while drawn (a 10ms step is a 100 Hz display: a second asks for about 30, not 100);
    /// an idle composer draws none, and reduced motion draws it still.
    #[gpui::test]
    fn the_sweep_repaints_at_its_cap_only_while_drawn(cx: &mut gpui::TestAppContext) {
        let drawn = |cx: &App| sweep("sweep", cx);
        let typing = renders_over(cx, false, 1000, drawn);
        assert!((20..=40).contains(&typing), "{typing} repaints in a second");
        assert_eq!(renders_over(cx, true, 1000, drawn), 0, "reduced motion");
        assert_eq!(
            renders_over(cx, false, 1000, |_| div().h(px(2.0)).into_any_element()),
            0,
            "an idle composer"
        );
    }

    /// The caret asks for frames at its coarse cap, and none under reduced motion.
    #[gpui::test]
    fn the_caret_repaints_at_its_cap(cx: &mut gpui::TestAppContext) {
        let drawn = |cx: &App| blink(div().w(px(6.0)).h(px(12.0)), "caret", cx);
        let blinking = renders_over(cx, false, 1000, drawn);
        assert!(
            (5..=15).contains(&blinking),
            "{blinking} repaints in a second"
        );
        assert_eq!(renders_over(cx, true, 1000, drawn), 0, "reduced motion");
    }

    /// A pending result's spinner repaints at its cap; a settled result draws a plain icon and schedules nothing.
    #[gpui::test]
    fn the_spinner_repaints_only_while_pending(cx: &mut gpui::TestAppContext) {
        use crate::icon_names::IconName;
        let pending = |cx: &App| spinning(Icon::new(IconName::Loader), "spin", cx);
        let turning = renders_over(cx, false, 1000, pending);
        assert!(
            (20..=40).contains(&turning),
            "{turning} repaints in a second"
        );
        assert_eq!(renders_over(cx, true, 1000, pending), 0, "reduced motion");
        assert_eq!(
            renders_over(cx, false, 1000, |_| Icon::new(IconName::Check)
                .into_any_element()),
            0,
            "a settled result"
        );
    }

    /// The wave repaints at its cap while an incoming ring is drawn; a busy ring draws none, and reduced motion a still ring.
    #[gpui::test]
    fn the_ring_wave_repaints_only_while_drawn(cx: &mut gpui::TestAppContext) {
        let drawn = |cx: &App| {
            div()
                .relative()
                .size(px(32.0))
                .child(ring_wave("wave", 32.0, px(999.0), cx))
                .into_any_element()
        };
        let waving = renders_over(cx, false, 1000, drawn);
        assert!((20..=40).contains(&waving), "{waving} repaints in a second");
        assert_eq!(renders_over(cx, true, 1000, drawn), 0, "reduced motion");
        assert_eq!(
            renders_over(cx, false, 1000, |_| div()
                .relative()
                .size(px(32.0))
                .into_any_element()),
            0,
            "a busy ring"
        );
    }

    /// A flash runs once: it repaints while it fades, never again after its run however the parent renders; reduced motion
    /// repaints nothing.
    #[gpui::test]
    fn a_flash_runs_once_per_jump(cx: &mut gpui::TestAppContext) {
        let flashing = |cx: &App| flash(div(), "flash", cx);
        let during = renders_over(cx, false, 1600, flashing);
        assert!(during > 0, "the flash repaints while it fades");
        let (host, vcx) = cx.add_window_view(|_, _| Host {
            renders: 0,
            build: Box::new(flashing),
        });
        vcx.run_until_parked();
        for _ in 0..160 {
            vcx.executor().advance_clock(Duration::from_millis(10));
            vcx.update(|window, cx| window.simulate_next_frame(cx));
            vcx.run_until_parked();
        }
        let settled = host.read_with(vcx, |h, _| h.renders);
        host.update(vcx, |_, cx| cx.notify());
        vcx.run_until_parked();
        for _ in 0..10 {
            vcx.executor().advance_clock(Duration::from_millis(10));
            vcx.update(|window, cx| window.simulate_next_frame(cx));
            vcx.run_until_parked();
        }
        assert_eq!(
            host.read_with(vcx, |h, _| h.renders),
            settled + 1,
            "a re-render after the flash asks for no more frames"
        );
        assert_eq!(renders_over(cx, true, 1600, flashing), 0, "reduced motion");
    }
}
